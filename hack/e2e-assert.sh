#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

KUBECONFIG_FILE=${E2E_KUBECONFIG:-}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:-}
FOREIGN_NAMESPACE=${E2E_FOREIGN_NAMESPACE:-}
HELM_RELEASE=${E2E_HELM_RELEASE:-}
EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
RUNNER_IMAGE=${E2E_RUNNER_IMAGE:-}
PTAH_VERSION=${E2E_PTAH_VERSION:-}
CONTROLLER_IMAGE=${E2E_CONTROLLER_IMAGE:-}
CONTROLLER_REVISION=${E2E_CONTROLLER_REVISION:-}
CONTROLLER_STATE_VERSION=${E2E_CONTROLLER_STATE_VERSION:-}

fail() {
	printf 'e2e assertions: %s\n' "$*" >&2
	exit 1
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return
	fi
	shasum -a 256 "$1" | awk '{print $1}'
}

sha256_stdin() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
		return
	fi
	shasum -a 256 | awk '{print $1}'
}

for value_name in KUBECONFIG_FILE OPERATOR_NAMESPACE TEST_NAMESPACE FOREIGN_NAMESPACE HELM_RELEASE EXECUTOR_IMAGE RUNNER_IMAGE PTAH_VERSION CONTROLLER_IMAGE CONTROLLER_REVISION CONTROLLER_STATE_VERSION; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ "$(printf '%s' "$CONTROLLER_REVISION" | wc -c | tr -d '[:space:]')" -le 128 ] ||
	fail "E2E_CONTROLLER_REVISION must be at most 128 bytes"
[ "$CONTROLLER_REVISION" = "$(printf '%s' "$CONTROLLER_REVISION" | tr -d '[:cntrl:]')" ] ||
	fail "E2E_CONTROLLER_REVISION must not contain control characters"
printf '%s' "$CONTROLLER_REVISION" | grep -Eq '^[^[:space:]](.*[^[:space:]])?$' ||
	fail "E2E_CONTROLLER_REVISION must not be empty or have edge whitespace"
printf '%s\n' "$CONTROLLER_IMAGE" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
	fail "E2E_CONTROLLER_IMAGE must be pinned by a lowercase SHA-256 digest"
printf '%s\n' "$CONTROLLER_STATE_VERSION" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_CONTROLLER_STATE_VERSION must be a positive integer"
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
[ "$TEST_NAMESPACE" != "$FOREIGN_NAMESPACE" ] ||
	fail "E2E_TEST_NAMESPACE and E2E_FOREIGN_NAMESPACE must differ"

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

h() {
	helm --kubeconfig "$KUBECONFIG_FILE" "$@"
}

expect_denied() {
	description=$1
	pattern=$2
	input_file=$3
	error_file=$4
	if k create -f "$input_file" >"$error_file.stdout" 2>"$error_file"; then
		fail "$description unexpectedly succeeded"
	fi
	if ! grep -Eiq "$pattern" "$error_file"; then
		printf 'e2e assertions: unexpected refusal for %s:\n' "$description" >&2
		cat "$error_file" >&2
		fail "$description did not fail for the expected reason"
	fi
}

expect_invalid_schema_duration() {
	description=$1
	filter=$2
	pattern=$3
	jq "$filter" "$schema_file" >"$invalid_schema_file"
	expect_denied "$description" "$pattern" "$invalid_schema_file" "$error_file"
}

expect_invalid_schema_policy() {
	description=$1
	filter=$2
	jq "$filter" "$schema_file" >"$invalid_schema_file"
	expect_denied "$description" 'spec.policy.exclude|policy.exclude' \
		"$invalid_schema_file" "$error_file"
}

expect_invalid_schema_reference() {
	description=$1
	filter=$2
	pattern=$3
	jq "$filter" "$schema_file" >"$invalid_schema_file"
	expect_denied "$description" "$pattern" "$invalid_schema_file" "$error_file"
}

CONTROLLER_NAME="${HELM_RELEASE}-ptah-operator"
SERVICE_ACCOUNT="system:serviceaccount:${OPERATOR_NAMESPACE}:${CONTROLLER_NAME}"

printf '%s\n' 'e2e assertions: checking manager readiness and chart state'
h -n "$OPERATOR_NAMESPACE" status "$HELM_RELEASE" >/dev/null
k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$CONTROLLER_NAME" --timeout=180s
k -n "$OPERATOR_NAMESPACE" get endpointslice \
	-l "kubernetes.io/service-name=${CONTROLLER_NAME}-webhook" -o json |
	jq -e '.items | any(.[]?.endpoints[]?; .conditions.ready == true)' >/dev/null ||
	fail "webhook Service has no ready endpoint"

printf '%s\n' 'e2e assertions: checking CRD discovery'
for crd in \
	ptahschemas.operator.ptah.dev \
	ptahschemaplans.operator.ptah.dev \
	ptahschemaapprovals.operator.ptah.dev; do
	k wait --for=condition=Established crd/"$crd" --timeout=60s
done
api_resources=$(k api-resources --api-group=operator.ptah.dev -o name)
for resource in \
	ptahschemas.operator.ptah.dev \
	ptahschemaplans.operator.ptah.dev \
	ptahschemaapprovals.operator.ptah.dev; do
	printf '%s\n' "$api_resources" | grep -Fx "$resource" >/dev/null ||
		fail "API discovery is missing $resource"
done

printf '%s\n' 'e2e assertions: checking owner-reference finalizer authorization'
for owner_resource in ptahschemas.operator.ptah.dev ptahschemaplans.operator.ptah.dev; do
	answer=$(k auth can-i update "$owner_resource" \
		--subresource=finalizers --as="$SERVICE_ACCOUNT" || true)
	[ "$answer" = yes ] ||
		fail "controller service account cannot update the $owner_resource finalizers subresource"
done

printf '%s\n' 'e2e assertions: checking webhook failure policy and scope'
k get mutatingwebhookconfiguration/ptah-operator-admission -o json |
	jq -e --arg namespace "$OPERATOR_NAMESPACE" --arg service "${CONTROLLER_NAME}-webhook" '
      .webhooks | length == 1 and all(.[];
        .failurePolicy == "Fail" and .sideEffects == "None" and
	        .matchPolicy == "Equivalent" and
	        ((.namespaceSelector // {}) == {}) and
	        ((.objectSelector // {}) == {}) and
	        ((.matchConditions // []) == []) and
        (.admissionReviewVersions | index("v1")) != null and
        .clientConfig.caBundle != "" and
        .clientConfig.service.namespace == $namespace and
        .clientConfig.service.name == $service and
        .clientConfig.service.path == "/mutate-operator-ptah-dev-v1alpha1-ptahschemaapproval" and
        .rules == [{
          apiGroups: ["operator.ptah.dev"], apiVersions: ["v1alpha1"],
          operations: ["CREATE"], resources: ["ptahschemaapprovals"], scope: "Namespaced"
        }])
    ' >/dev/null || fail "approval mutating webhook is not exact and fail-closed"
k get validatingwebhookconfiguration/ptah-operator-admission -o json |
	jq -e --arg namespace "$OPERATOR_NAMESPACE" --arg service "${CONTROLLER_NAME}-webhook" '
      .webhooks | length == 2 and
      (map(select(.name == "vapproval.operator.ptah.dev")) | length == 1 and all(.[];
        .failurePolicy == "Fail" and .sideEffects == "None" and .matchPolicy == "Equivalent" and
        ((.namespaceSelector // {}) == {}) and ((.objectSelector // {}) == {}) and
        ((.matchConditions // []) == []) and
        (.admissionReviewVersions | index("v1")) != null and .clientConfig.caBundle != "" and
        .clientConfig.service.namespace == $namespace and .clientConfig.service.name == $service and
        .clientConfig.service.path == "/validate-operator-ptah-dev-v1alpha1-ptahschemaapproval" and
        .rules == [{apiGroups: ["operator.ptah.dev"], apiVersions: ["v1alpha1"],
          operations: ["CREATE", "UPDATE"], resources: ["ptahschemaapprovals"], scope: "Namespaced"}])) and
      (map(select(.name == "vpodintent.operator.ptah.dev")) | length == 1 and all(.[];
        .failurePolicy == "Fail" and .sideEffects == "None" and .matchPolicy == "Equivalent" and
        ((.namespaceSelector // {}) == {}) and
        .objectSelector == {matchLabels: {
          "app.kubernetes.io/managed-by": "ptah-operator",
          "app.kubernetes.io/component": "schema-operation"
        }} and
        (.matchConditions | length == 1 and .[0].name == "job-owned-pod" and
          (.[0].expression | contains("batch/v1") and contains("oldObject"))) and
        (.admissionReviewVersions | index("v1")) != null and .clientConfig.caBundle != "" and
        .clientConfig.service.namespace == $namespace and .clientConfig.service.name == $service and
        .clientConfig.service.path == "/validate-v1-pod-ptah-operation-intent" and
        .rules == [{apiGroups: [""], apiVersions: ["v1"], operations: ["CREATE", "UPDATE"],
          resources: ["pods", "pods/ephemeralcontainers", "pods/resize"], scope: "Namespaced"}]))
    ' >/dev/null || fail "validating webhooks are not exact and fail-closed"

printf '%s\n' 'e2e assertions: checking controller Secret isolation'
k create namespace "$TEST_NAMESPACE" >/dev/null
k create namespace "$FOREIGN_NAMESPACE" >/dev/null
k -n "$TEST_NAMESPACE" create secret generic local-database \
	--from-literal=url='postgres://e2e:unused@database.invalid/e2e' >/dev/null
k -n "$FOREIGN_NAMESPACE" create secret generic foreign-database \
	--from-literal=url='postgres://e2e:unused@database.invalid/e2e' >/dev/null
for namespace in "$OPERATOR_NAMESPACE" "$TEST_NAMESPACE" "$FOREIGN_NAMESPACE"; do
	answer=$(k auth can-i get serviceaccount/default -n "$namespace" --as="$SERVICE_ACCOUNT" || true)
	[ "$answer" = yes ] ||
		fail "controller service account cannot resolve a ServiceAccount in namespace $namespace"
	answer=$(k auth can-i list limitranges -n "$namespace" --as="$SERVICE_ACCOUNT" || true)
	[ "$answer" = yes ] ||
		fail "controller service account cannot resolve LimitRanges in namespace $namespace"
	for denied_tuple in \
		'list serviceaccounts' \
		'watch serviceaccounts' \
		'get limitranges' \
		'watch limitranges' \
		'create serviceaccounts' \
		'update serviceaccounts' \
		'patch serviceaccounts' \
		'delete serviceaccounts' \
		'create limitranges' \
		'update limitranges' \
		'patch limitranges' \
		'delete limitranges'; do
		denied_verb=${denied_tuple%% *}
		denied_resource=${denied_tuple#* }
		answer=$(k auth can-i "$denied_verb" "$denied_resource" -n "$namespace" \
			--as="$SERVICE_ACCOUNT" || true)
		[ "$answer" = no ] ||
			fail "controller service account can $denied_verb $denied_resource in namespace $namespace"
	done
done
for namespace in "$OPERATOR_NAMESPACE" "$TEST_NAMESPACE" "$FOREIGN_NAMESPACE"; do
	for verb in get list watch; do
		answer=$(k auth can-i "$verb" secrets -n "$namespace" --as="$SERVICE_ACCOUNT" || true)
		[ "$answer" = no ] ||
			fail "controller service account can $verb Secrets in namespace $namespace"
	done
done
for namespaced_secret in \
	"${OPERATOR_NAMESPACE}/${CONTROLLER_NAME}-webhook-cert" \
	"${TEST_NAMESPACE}/local-database" \
	"${FOREIGN_NAMESPACE}/foreign-database"; do
	secret_namespace=${namespaced_secret%%/*}
	secret_name=${namespaced_secret#*/}
	k -n "$secret_namespace" get secret "$secret_name" >/dev/null
	answer=$(k auth can-i get "secret/${secret_name}" -n "$secret_namespace" --as="$SERVICE_ACCOUNT" || true)
	[ "$answer" = no ] ||
		fail "controller service account can read Secret $namespaced_secret"
done

printf '%s\n' 'e2e assertions: checking references are namespace-local'
k get crd ptahschemas.operator.ptah.dev -o json |
	jq -e '
      .spec.versions[] | select(.name == "v1alpha1") |
      .schema.openAPIV3Schema.properties.spec.properties as $spec |
      (($spec.target.properties.urlFrom.properties | has("namespace")) | not) and
      (($spec.desired.properties.verificationPolicyFrom.properties | has("namespace")) | not) and
      (($spec.desired.properties.registryAuthFrom.properties | has("namespace")) | not)
    ' >/dev/null || fail "PtahSchema exposes a cross-namespace reference"
k get crd ptahschemaapprovals.operator.ptah.dev -o json |
	jq -e '
      .spec.versions[] | select(.name == "v1alpha1") |
      .schema.openAPIV3Schema.properties.spec.properties as $spec |
      (($spec.schemaRef.properties | has("namespace")) | not) and
      (($spec.planRef.properties | has("namespace")) | not)
    ' >/dev/null || fail "PtahSchemaApproval exposes a cross-namespace reference"

POLICY_NAME=e2e-verification-policy
POLICY_KEY=policy.yaml
SCHEMA_NAME=e2e-suspended-schema
UNSUPPORTED_ENGINE_SCHEMA=e2e-unsupported-engine
PLAN_NAME=e2e-plan
PLAN_CHUNK_NAME=e2e-plan-chunk-0
APPROVAL_NAME=e2e-approval

k -n "$TEST_NAMESPACE" create configmap "$POLICY_NAME" \
	--from-file="${POLICY_KEY}=${ROOT_DIR}/testdata/e2e/verification-policy.yaml" \
	--dry-run=client -o json | jq '.immutable = true' | k create -f - >/dev/null
k -n "$TEST_NAMESPACE" get configmap "$POLICY_NAME" -o json |
	jq -e '.immutable == true' >/dev/null || fail "verification policy ConfigMap is mutable"
policy_uid=$(k -n "$TEST_NAMESPACE" get configmap "$POLICY_NAME" -o jsonpath='{.metadata.uid}')
policy_digest="sha256:$(sha256_file "${ROOT_DIR}/testdata/e2e/verification-policy.yaml")"

schema_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-schema.XXXXXX")
invalid_schema_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-invalid-schema.XXXXXX")
plan_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-plan.XXXXXX")
approval_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-approval.XXXXXX")
invalid_approval_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-invalid-approval.XXXXXX")
missing_fingerprint_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-missing-fingerprint.XXXXXX")
foreign_plan_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-foreign-plan.XXXXXX")
foreign_approval_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-foreign-approval.XXXXXX")
error_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-error.XXXXXX")
webhook_scope_job_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-webhook-job.XXXXXX")
webhook_scope_event_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-webhook-events.XXXXXX")
webhook_deployment_file=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-webhook-deployment.XXXXXX")
chmod 600 "$webhook_scope_job_file" "$webhook_scope_event_file" "$webhook_deployment_file"
WEBHOOK_UNRELATED_JOB=e2e-webhook-unrelated
WEBHOOK_OUTAGE_JOB=e2e-webhook-managed-outage
WEBHOOK_SPOOF_JOB=e2e-webhook-foreign-spoof
WEBHOOK_DEPLOYMENT_STOPPED=0
WEBHOOK_ORIGINAL_REPLICAS=

delete_webhook_scope_fixtures() {
	k -n "$TEST_NAMESPACE" delete jobs \
		"$WEBHOOK_UNRELATED_JOB" "$WEBHOOK_OUTAGE_JOB" "$WEBHOOK_SPOOF_JOB" \
		--cascade=foreground --wait=true --ignore-not-found >/dev/null 2>&1
}

webhook_service_ready() {
	k -n "$OPERATOR_NAMESPACE" get endpointslice \
		-l "kubernetes.io/service-name=${CONTROLLER_NAME}-webhook" -o json |
		jq -e '.items | any(.[]?.endpoints[]?; .conditions.ready == true)' >/dev/null
}

restore_webhook_deployment() {
	[ "$WEBHOOK_DEPLOYMENT_STOPPED" -eq 1 ] || return 0
	[ -n "$WEBHOOK_ORIGINAL_REPLICAS" ] || return 1
	if ! k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" >/dev/null 2>&1; then
		k create -f "$webhook_deployment_file" >/dev/null || return 1
	fi
	k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$CONTROLLER_NAME" \
		--timeout=180s >/dev/null || return 1
	restore_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$restore_deadline" ]; do
		if webhook_service_ready; then
			WEBHOOK_DEPLOYMENT_STOPPED=0
			return 0
		fi
		sleep 1
	done
	return 1
}

cleanup_files() {
	status=$?
	trap - EXIT
	set +e
	delete_webhook_scope_fixtures
	k -n "$TEST_NAMESPACE" delete ptahschema "$UNSUPPORTED_ENGINE_SCHEMA" \
		--wait=true --ignore-not-found >/dev/null 2>&1
	if ! restore_webhook_deployment; then
		printf '%s\n' 'e2e assertions: could not restore the webhook Deployment during cleanup' >&2
		status=1
	fi
	rm -f "$schema_file" "$invalid_schema_file" "$plan_file" "$approval_file" \
		"$invalid_approval_file" \
		"$missing_fingerprint_file" "$foreign_plan_file" "$foreign_approval_file" \
		"$error_file" "$error_file.stdout" "$webhook_scope_job_file" \
		"$webhook_scope_event_file" "$webhook_deployment_file"
	exit "$status"
}
trap cleanup_files EXIT
trap 'exit 130' HUP INT TERM

write_webhook_scope_job() {
	scope_job_name=$1
	scope_managed=$2
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$scope_job_name" \
		--arg image "$RUNNER_IMAGE" \
		--argjson managed "$scope_managed" '
      (if $managed then {
        "app.kubernetes.io/managed-by": "ptah-operator",
        "app.kubernetes.io/component": "schema-operation"
      } else {"operator.ptah.dev/e2e-webhook-scope": "unrelated"} end) as $labels |
      {
        apiVersion: "batch/v1",
        kind: "Job",
        metadata: {namespace: $namespace, name: $name, labels: $labels},
        spec: {
          backoffLimit: 0,
          template: {
            metadata: {labels: $labels},
            spec: {
              automountServiceAccountToken: false,
              restartPolicy: "Never",
              containers: [{
                name: "probe",
                image: $image,
                imagePullPolicy: "IfNotPresent",
                command: ["/ptah-runner"],
                args: ["--version"],
                env: (if $managed then [{
                  name: "PTAH_DB_URL",
                  valueFrom: {secretKeyRef: {name: "local-database", key: "url"}}
                }] else [] end),
                securityContext: {
                  allowPrivilegeEscalation: false,
                  capabilities: {drop: ["ALL"]}
                }
              }]
            }
          }
        }
      }
    ' >"$webhook_scope_job_file"
}

assert_webhook_scope_events_safe() {
	if grep -F 'postgres://e2e:unused@database.invalid/e2e' \
		"$webhook_scope_event_file" >/dev/null; then
		fail "Pod admission failure Event exposed the referenced database credential"
	fi
}

wait_for_job_pod_admission() {
	scope_job_name=$1
	scope_job_uid=$2
	scope_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$scope_deadline" ]; do
		if k -n "$TEST_NAMESPACE" get pods -l "job-name=${scope_job_name}" -o json |
			jq -e --arg uid "$scope_job_uid" '
              [.items[] | select(any(.metadata.ownerReferences[]?;
                .apiVersion == "batch/v1" and .kind == "Job" and
                .uid == $uid and .controller == true))] | length == 1
            ' >/dev/null; then
			return 0
		fi
		sleep 1
	done
	return 1
}

wait_for_job_failed_create() {
	scope_job_name=$1
	scope_job_uid=$2
	scope_failure_pattern=$3
	scope_deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$scope_deadline" ]; do
		k -n "$TEST_NAMESPACE" get events \
			--field-selector "involvedObject.kind=Job,involvedObject.name=${scope_job_name}" \
			-o json >"$webhook_scope_event_file"
		assert_webhook_scope_events_safe
		if jq -e \
			--arg uid "$scope_job_uid" \
			--arg pattern "$scope_failure_pattern" '
              any(.items[];
                .involvedObject.uid == $uid and .reason == "FailedCreate" and
                ((.message // "") | contains("vpodintent.operator.ptah.dev")) and
                ((.message // "") | test($pattern; "i")))
            ' "$webhook_scope_event_file" >/dev/null; then
			if k -n "$TEST_NAMESPACE" get pods -l "job-name=${scope_job_name}" -o json |
				jq -e --arg uid "$scope_job_uid" '
                  all(.items[]; all(.metadata.ownerReferences[]?; .uid != $uid))
                ' >/dev/null; then
				: >"$webhook_scope_event_file"
				return 0
			fi
		fi
		sleep 1
	done
	: >"$webhook_scope_event_file"
	return 1
}

printf '%s\n' 'e2e assertions: checking Pod webhook outage scope and foreign-label refusal'
WEBHOOK_ORIGINAL_REPLICAS=$(k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" \
	-o json | jq -er '.spec.replicas // 1')
printf '%s\n' "$WEBHOOK_ORIGINAL_REPLICAS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "webhook Deployment does not have a positive replica count"
k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" -o json |
	jq 'del(
      .metadata.creationTimestamp,
      .metadata.generation,
      .metadata.managedFields,
      .metadata.resourceVersion,
      .metadata.uid,
      .metadata.annotations."deployment.kubernetes.io/revision",
      .status
    )' >"$webhook_deployment_file"
WEBHOOK_DEPLOYMENT_STOPPED=1
k -n "$OPERATOR_NAMESPACE" delete deployment "$CONTROLLER_NAME" \
	--cascade=foreground --wait=true >/dev/null
outage_deadline=$(($(date +%s) + 180))
while [ "$(date +%s)" -lt "$outage_deadline" ]; do
	if ! k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" \
		>/dev/null 2>&1 && ! webhook_service_ready; then
		break
	fi
	sleep 1
done
if webhook_service_ready; then
	fail "webhook Service retained a ready endpoint after the Deployment was removed"
fi

write_webhook_scope_job "$WEBHOOK_UNRELATED_JOB" false
k create -f "$webhook_scope_job_file" >/dev/null
unrelated_job_uid=$(k -n "$TEST_NAMESPACE" get job "$WEBHOOK_UNRELATED_JOB" \
	-o jsonpath='{.metadata.uid}')
wait_for_job_pod_admission "$WEBHOOK_UNRELATED_JOB" "$unrelated_job_uid" ||
	fail "webhook outage blocked an unrelated Job Pod outside the object selector"

write_webhook_scope_job "$WEBHOOK_OUTAGE_JOB" true
k create -f "$webhook_scope_job_file" >/dev/null
outage_job_uid=$(k -n "$TEST_NAMESPACE" get job "$WEBHOOK_OUTAGE_JOB" \
	-o jsonpath='{.metadata.uid}')
wait_for_job_failed_create "$WEBHOOK_OUTAGE_JOB" "$outage_job_uid" \
	'failed calling webhook|no endpoints available|connection refused|service unavailable' ||
	fail "webhook outage did not fail closed for a managed-operation Pod"

delete_webhook_scope_fixtures
restore_webhook_deployment || fail "could not restore the webhook Deployment after the outage proof"

write_webhook_scope_job "$WEBHOOK_SPOOF_JOB" true
k create -f "$webhook_scope_job_file" >/dev/null
spoof_job_uid=$(k -n "$TEST_NAMESPACE" get job "$WEBHOOK_SPOOF_JOB" \
	-o jsonpath='{.metadata.uid}')
wait_for_job_failed_create "$WEBHOOK_SPOOF_JOB" "$spoof_job_uid" \
	'managed Pod Job has no exact PtahSchema controller identity' ||
	fail "a foreign Job spoofing managed-operation labels was not denied by the Pod intent webhook"
delete_webhook_scope_fixtures

jq -n \
	--arg namespace "$TEST_NAMESPACE" \
	--arg name "$SCHEMA_NAME" \
	--arg policy "$POLICY_NAME" \
	--arg policyKey "$POLICY_KEY" '
  {
    apiVersion: "operator.ptah.dev/v1alpha1",
    kind: "PtahSchema",
    metadata: {namespace: $namespace, name: $name},
    spec: {
      target: {
        engine: "PostgreSQL",
        coordinationKey: "e2e/admission/postgresql",
        urlFrom: {name: "database-url", key: "url"}
      },
      desired: {
        ociRef: "oci://registry.invalid/e2e/schema:unused",
        verificationPolicyFrom: {name: $policy, key: $policyKey}
      },
      suspend: true
    }
  }' >"$schema_file"

printf '%s\n' 'e2e assertions: checking API-server duration bounds'
expect_invalid_schema_duration 'nanosecond interval' \
	'.spec.interval = "1ns"' 'interval must be between 10s and 24h'
expect_invalid_schema_duration 'negative failure retry interval' \
	'.spec.execution.failureRetryInterval = "-1s"' \
	'failureRetryInterval must be between 5s and 1h'
expect_invalid_schema_duration 'nanosecond connect timeout' \
	'.spec.execution.connectTimeout = "1ns"' \
	'connectTimeout must be between 1s and 10m'
expect_invalid_schema_duration 'negative lock timeout' \
	'.spec.policy.lockTimeout = "-1s"' \
	'lockTimeout must be between 1s and 10m'
expect_invalid_schema_duration 'connect timeout over active deadline' \
	'.spec.execution = {activeDeadlineSeconds: 30, connectTimeout: "31s"}' \
	'connectTimeout must not exceed execution.activeDeadlineSeconds'
expect_invalid_schema_duration 'lock timeout over active deadline' \
	'.spec.execution.activeDeadlineSeconds = 30 | .spec.policy.lockTimeout = "31s"' \
	'lockTimeout must not exceed execution.activeDeadlineSeconds'

printf '%s\n' 'e2e assertions: checking required reference keys'
reference_jobs_before=$(k -n "$TEST_NAMESPACE" get jobs -o json |
	jq -c '[.items[].metadata.uid] | sort')
expect_invalid_schema_reference 'empty target Secret key' \
	'.spec.target.urlFrom.key = ""' \
	'target.urlFrom must name a required Secret key'
expect_invalid_schema_reference 'empty development target Secret key' \
	'.spec.dev = {urlFrom: {name: "development-url", key: ""}}' \
	'dev.urlFrom must name a required Secret key'
expect_invalid_schema_reference 'empty verification policy ConfigMap key' \
	'.spec.desired.verificationPolicyFrom.key = ""' \
	'desired.verificationPolicyFrom must name a required ConfigMap key'
expect_invalid_schema_reference 'empty OCI CA ConfigMap key' \
	'.spec.desired.transport.caFrom = {name: "registry-ca", key: ""}' \
	'desired.transport.caFrom must name a required ConfigMap key'
reference_jobs_after=$(k -n "$TEST_NAMESPACE" get jobs -o json |
	jq -c '[.items[].metadata.uid] | sort')
[ "$reference_jobs_after" = "$reference_jobs_before" ] ||
	fail "a rejected empty reference key created an operation Job"

printf '%s\n' 'e2e assertions: checking managed-scope selector bounds'
exclude_jobs_before=$(k -n "$TEST_NAMESPACE" get jobs -o json |
	jq -c '[.items[].metadata.uid] | sort')
expect_invalid_schema_policy 'whitespace-only managed-scope selector' \
	'.spec.policy.exclude = ["   "]'
expect_invalid_schema_policy 'leading whitespace in managed-scope selector' \
	'.spec.policy.exclude = [" public.*"]'
expect_invalid_schema_policy 'trailing whitespace in managed-scope selector' \
	'.spec.policy.exclude = ["public.* "]'
expect_invalid_schema_policy 'control character in managed-scope selector' \
	'.spec.policy.exclude = ["public.\nusers"]'
expect_invalid_schema_policy 'overlong managed-scope selector' \
	'.spec.policy.exclude = [("x" * 257)]'
expect_invalid_schema_policy 'duplicate managed-scope selector' \
	'.spec.policy.exclude = ["public.*", "public.*"]'
exclude_jobs_after=$(k -n "$TEST_NAMESPACE" get jobs -o json |
	jq -c '[.items[].metadata.uid] | sort')
[ "$exclude_jobs_after" = "$exclude_jobs_before" ] ||
	fail "a rejected managed-scope selector created an operation Job"

printf '%s\n' 'e2e assertions: checking explicit unsupported-engine status'
jq \
	--arg name "$UNSUPPORTED_ENGINE_SCHEMA" '
      .metadata.name = $name |
      .spec.target.engine = "SQLite" |
      .spec.target.coordinationKey = "e2e/admission/unsupported" |
      .spec.suspend = false
    ' "$schema_file" >"$invalid_schema_file"
k create -f "$invalid_schema_file" >/dev/null
unsupported_deadline=$(($(date +%s) + 90))
unsupported_schema_object=
while [ "$(date +%s)" -lt "$unsupported_deadline" ]; do
	unsupported_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema \
		"$UNSUPPORTED_ENGINE_SCHEMA" -o json)
	if printf '%s\n' "$unsupported_schema_object" | jq -e '
      .metadata.generation as $generation |
      .status.observedGeneration == $generation and
      .status.phase == "Blocked" and
      (.status.plan == null) and
      (.status.activeOperation == null) and
      (.status.pendingObservation == null) and
      any(.status.conditions[]?;
        .type == "EngineSupported" and .status == "False" and
        .reason == "UnsupportedEngine" and .observedGeneration == $generation) and
      any(.status.conditions[]?;
        .type == "Ready" and .status == "False" and
        .reason == "UnsupportedEngine" and .observedGeneration == $generation)
    ' >/dev/null; then
		break
	fi
	sleep 1
done
[ -n "$unsupported_schema_object" ] ||
	fail "unsupported engine did not produce a readable PtahSchema"
printf '%s\n' "$unsupported_schema_object" | jq -e '
  .metadata.generation as $generation |
  .status.observedGeneration == $generation and
  .status.phase == "Blocked" and
  (.status.plan == null) and
  (.status.activeOperation == null) and
  (.status.pendingObservation == null) and
  any(.status.conditions[]?;
    .type == "EngineSupported" and .status == "False" and
    .reason == "UnsupportedEngine" and .observedGeneration == $generation) and
  any(.status.conditions[]?;
    .type == "Ready" and .status == "False" and
    .reason == "UnsupportedEngine" and .observedGeneration == $generation)
' >/dev/null || fail "unsupported engine did not reach its explicit current-generation status"
unsupported_schema_uid=$(printf '%s\n' "$unsupported_schema_object" | jq -er '.metadata.uid')
k -n "$TEST_NAMESPACE" get jobs -o json |
	jq -e --arg uid "$unsupported_schema_uid" '
      all(.items[]; all(.metadata.ownerReferences[]?; .uid != $uid))
    ' >/dev/null || fail "unsupported engine created an owned operation Job"
k -n "$TEST_NAMESPACE" get ptahschemaplans -o json |
	jq -e --arg uid "$unsupported_schema_uid" '
      [.items[] | select(.spec.schemaRef.uid == $uid)] | length == 0
    ' >/dev/null || fail "unsupported engine created a plan"
k -n "$TEST_NAMESPACE" get ptahschemaapprovals -o json |
	jq -e --arg uid "$unsupported_schema_uid" '
      [.items[] | select(.spec.schemaRef.uid == $uid)] | length == 0
    ' >/dev/null || fail "unsupported engine created an approval"
k -n "$TEST_NAMESPACE" delete ptahschema "$UNSUPPORTED_ENGINE_SCHEMA" \
	--wait=true >/dev/null

k create -f "$schema_file" >/dev/null
k -n "$TEST_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Suspended \
	ptahschema/"$SCHEMA_NAME" --timeout=90s
schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$SCHEMA_NAME" -o json)
printf '%s\n' "$schema_object" | jq -e '
  .spec.interval == "10m" and
  .spec.policy.apply == "OnApproval" and
  .spec.policy.allowDestructive == false and
  .spec.policy.driftSeverity == "all" and
  .spec.policy.lockTimeout == "30s" and
  .spec.policy.transactionMode == "file" and
  .spec.execution.activeDeadlineSeconds == 900 and
  .spec.execution.failureRetryInterval == "30s" and
  .spec.execution.connectTimeout == "10s"
' >/dev/null || fail "PtahSchema API defaults were not persisted for omitted safe policy and execution fields"
schema_uid=$(printf '%s\n' "$schema_object" | jq -er '.metadata.uid')
schema_generation=$(printf '%s\n' "$schema_object" | jq -er '.metadata.generation')
if ! execution_binding=$(printf '%s\n' "$schema_object" | jq -ce \
	--arg controllerImage "$CONTROLLER_IMAGE" \
	--arg controllerRevision "$CONTROLLER_REVISION" \
	--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" \
	--arg ptahVersion "$PTAH_VERSION" \
	--arg executorImage "$EXECUTOR_IMAGE" \
	--arg runnerImage "$RUNNER_IMAGE" '
    .status.executionBinding as $binding |
    select(
      ($binding.epoch | test("^v1-[0-9a-f]{32}$")) and
      $binding.controllerImage == $controllerImage and
      $binding.controllerRevision == $controllerRevision and
      $binding.controllerStateVersion == $controllerStateVersion and
      $binding.ptahVersion == $ptahVersion and
      $binding.executorImage == $executorImage and
      $binding.runnerImage == $runnerImage and
      ($binding.runnerProtocolVersion | type == "number" and . == 5)
    ) | $binding
  '); then
	fail "suspended schema lacks the exact seven-field controller/runtime execution binding"
fi
execution_binding_id=$(printf '%s\n' "$execution_binding" | jq -er '.epoch')
controller_image=$(printf '%s\n' "$execution_binding" | jq -er '.controllerImage')
controller_revision=$(printf '%s\n' "$execution_binding" | jq -er '.controllerRevision')
controller_state_version=$(printf '%s\n' "$execution_binding" | jq -er '.controllerStateVersion')
deployed_controller_image=$(k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" -o json |
	jq -er '
      [.spec.template.spec.containers[] | select(.name == "manager").args[]? |
        select(startswith("--controller-image=")) | ltrimstr("--controller-image=")] as $images |
      if ($images | length) == 1 then $images[0]
      else error("manager must have exactly one --controller-image argument") end
    ')
[ "$controller_image" = "$deployed_controller_image" ] ||
	fail "schema execution binding does not match the manager's exact controller image argument"
[ "$deployed_controller_image" = "$CONTROLLER_IMAGE" ] ||
	fail "manager controller image argument does not match the externally expected image identity"

artifact_digest=sha256:2222222222222222222222222222222222222222222222222222222222222222
content_digest=sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881
coordination_digest=sha256:a039cc1dcc29e539b94d48451d5b4b5fa2b2eebc7927f5c4e106679b98479725
target_digest=sha256:4444444444444444444444444444444444444444444444444444444444444444
actual_fingerprint=sha256:5555555555555555555555555555555555555555555555555555555555555555
drift_report_digest=sha256:9999999999999999999999999999999999999999999999999999999999999999
desired_fingerprint=sha256:6666666666666666666666666666666666666666666666666666666666666666
policy_fingerprint=sha256:7777777777777777777777777777777777777777777777777777777777777777
created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

plan_binding_json=$(jq -cn \
	--arg schemaUID "$schema_uid" \
	--arg contentDigest "$content_digest" \
	--arg artifactDigest "$artifact_digest" \
	--arg coordinationDigest "$coordination_digest" \
	--arg targetDigest "$target_digest" \
	--arg actualFingerprint "$actual_fingerprint" \
	--arg desiredFingerprint "$desired_fingerprint" \
	--arg policyFingerprint "$policy_fingerprint" \
	--arg verificationPolicyUID "$policy_uid" \
	--arg verificationPolicyDigest "$policy_digest" \
	--arg executionBindingID "$execution_binding_id" \
	--arg controllerImage "$controller_image" \
	--arg controllerRevision "$controller_revision" \
	--argjson controllerStateVersion "$controller_state_version" \
	--arg ptahVersion "$PTAH_VERSION" \
	--arg executorImage "$EXECUTOR_IMAGE" \
	--arg runnerImage "$RUNNER_IMAGE" '
  {
    contract_version: 3,
    schema_uid: $schemaUID,
    plan_content_digest: $contentDigest,
    artifact_digest: $artifactDigest,
    coordination_digest: $coordinationDigest,
    target_identity_digest: $targetDigest,
    actual_state_fingerprint: $actualFingerprint,
    desired_state_fingerprint: $desiredFingerprint,
    policy_fingerprint: $policyFingerprint,
    verification_policy_uid: $verificationPolicyUID,
    verification_policy_digest: $verificationPolicyDigest,
    execution_binding_id: $executionBindingID,
    controller_image: $controllerImage,
    controller_revision: $controllerRevision,
    controller_state_version: $controllerStateVersion,
    ptah_version: $ptahVersion,
    executor_image: $executorImage,
    runner_image: $runnerImage,
    runner_protocol_version: 5
  }
')
plan_fingerprint="sha256:$(printf '%s' "$plan_binding_json" | sha256_stdin)"

jq -n \
	--arg namespace "$TEST_NAMESPACE" \
	--arg name "$PLAN_NAME" \
	--arg chunkName "$PLAN_CHUNK_NAME" \
	--arg schemaName "$SCHEMA_NAME" \
	--arg schemaUID "$schema_uid" \
	--arg fingerprint "$plan_fingerprint" \
	--arg contentDigest "$content_digest" \
	--arg artifactDigest "$artifact_digest" \
	--arg coordinationDigest "$coordination_digest" \
	--arg targetDigest "$target_digest" \
	--arg actualFingerprint "$actual_fingerprint" \
	--arg desiredFingerprint "$desired_fingerprint" \
	--arg policyFingerprint "$policy_fingerprint" \
	--arg verificationPolicyUID "$policy_uid" \
	--arg verificationPolicyDigest "$policy_digest" \
	--arg executionBindingID "$execution_binding_id" \
	--arg controllerImage "$controller_image" \
	--arg controllerRevision "$controller_revision" \
	--argjson controllerStateVersion "$controller_state_version" \
	--arg ptahVersion "$PTAH_VERSION" \
	--arg executorImage "$EXECUTOR_IMAGE" \
	--arg runnerImage "$RUNNER_IMAGE" '
  {
    apiVersion: "operator.ptah.dev/v1alpha1",
    kind: "PtahSchemaPlan",
    metadata: {
      namespace: $namespace,
      name: $name,
      labels: {"operator.ptah.dev/schema": $schemaName},
      ownerReferences: [{
        apiVersion: "operator.ptah.dev/v1alpha1",
        kind: "PtahSchema",
        name: $schemaName,
        uid: $schemaUID,
        controller: true,
        blockOwnerDeletion: true
      }]
    },
    spec: {
      contractVersion: 3,
      schemaRef: {name: $schemaName, uid: $schemaUID},
      fingerprint: $fingerprint,
      contentDigest: $contentDigest,
      size: 1,
      artifactDigest: $artifactDigest,
      coordinationDigest: $coordinationDigest,
      targetIdentityDigest: $targetDigest,
      actualStateFingerprint: $actualFingerprint,
      desiredStateFingerprint: $desiredFingerprint,
      policyFingerprint: $policyFingerprint,
      verificationPolicyUID: $verificationPolicyUID,
      verificationPolicyDigest: $verificationPolicyDigest,
      executionBindingID: $executionBindingID,
      controllerImage: $controllerImage,
      controllerRevision: $controllerRevision,
      controllerStateVersion: $controllerStateVersion,
      ptahVersion: $ptahVersion,
      executorImage: $executorImage,
      runnerImage: $runnerImage,
      runnerProtocolVersion: 5,
      dialect: "postgres",
      destructive: false,
      statementCount: 0,
      chunks: [{
        name: $chunkName,
        key: "chunk",
        index: 0,
        digest: $contentDigest,
        size: 1
      }]
    }
  }' >"$plan_file"
k create -f "$plan_file" >/dev/null
plan_uid=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$PLAN_NAME" -o jsonpath='{.metadata.uid}')
plan_generation=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$PLAN_NAME" -o jsonpath='{.metadata.generation}')

jq -n \
	--arg namespace "$TEST_NAMESPACE" \
	--arg name "$PLAN_CHUNK_NAME" \
	--arg planName "$PLAN_NAME" \
	--arg schemaName "$SCHEMA_NAME" \
	--arg planUID "$plan_uid" '
  {
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: {
      namespace: $namespace,
      name: $name,
      labels: {
        "operator.ptah.dev/plan": $planName,
        "operator.ptah.dev/schema": $schemaName
      },
      ownerReferences: [{
        apiVersion: "operator.ptah.dev/v1alpha1",
        kind: "PtahSchemaPlan",
        name: $planName,
        uid: $planUID,
        controller: true,
        blockOwnerDeletion: true
      }]
    },
    immutable: true,
    binaryData: {chunk: "eA=="}
  }' | k create -f - >/dev/null
plan_chunk_uid=$(k -n "$TEST_NAMESPACE" get configmap "$PLAN_CHUNK_NAME" \
	-o jsonpath='{.metadata.uid}')

plan_status=$(jq -n \
	--argjson observedGeneration "$plan_generation" \
	--arg chunkName "$PLAN_CHUNK_NAME" \
	--arg chunkUID "$plan_chunk_uid" \
	--arg now "$created_at" '
  {status: {
    observedGeneration: $observedGeneration,
    publishedChunks: [{name: $chunkName, uid: $chunkUID, index: 0}],
    conditions: [{
      type: "Ready",
      status: "True",
      reason: "E2EFixture",
      message: "Plan fixture is committed for admission testing",
      lastTransitionTime: $now
    }]
  }}')
k -n "$TEST_NAMESPACE" patch ptahschemaplan "$PLAN_NAME" \
	--subresource=status --type=merge -p "$plan_status" >/dev/null

schema_status=$(jq -n \
	--argjson observedGeneration "$schema_generation" \
	--arg planName "$PLAN_NAME" \
	--arg planUID "$plan_uid" \
	--arg fingerprint "$plan_fingerprint" \
	--arg contentDigest "$content_digest" \
	--arg artifactDigest "$artifact_digest" \
	--arg coordinationDigest "$coordination_digest" \
	--arg targetDigest "$target_digest" \
	--arg actualFingerprint "$actual_fingerprint" \
	--arg driftReportDigest "$drift_report_digest" \
	--arg desiredFingerprint "$desired_fingerprint" \
	--arg policyFingerprint "$policy_fingerprint" \
	--arg verificationPolicyUID "$policy_uid" \
	--arg verificationPolicyDigest "$policy_digest" \
	--arg executionBindingID "$execution_binding_id" \
	--arg controllerImage "$controller_image" \
	--arg controllerRevision "$controller_revision" \
	--argjson controllerStateVersion "$controller_state_version" \
	--arg ptahVersion "$PTAH_VERSION" \
	--arg executorImage "$EXECUTOR_IMAGE" \
	--arg runnerImage "$RUNNER_IMAGE" \
	--arg now "$created_at" '
  {status: {
    observedGeneration: $observedGeneration,
    phase: "AwaitingApproval",
    conditions: [
      {
        type: "Ready",
        status: "False",
        reason: "ApprovalRequired",
        message: "Plan fixture is awaiting approval",
        lastTransitionTime: $now
      },
      {
        type: "ApprovalRequired",
        status: "True",
        reason: "Policy",
        message: "Plan fixture requires explicit approval",
        lastTransitionTime: $now
      }
    ],
    source: {
      digest: $artifactDigest,
      verificationPolicyUID: $verificationPolicyUID,
      verificationPolicyDigest: $verificationPolicyDigest
    },
    target: {
      engine: "PostgreSQL",
      coordinationDigest: $coordinationDigest,
      identityDigest: $targetDigest,
      driftReportDigest: $driftReportDigest
    },
    plan: {
      name: $planName,
      uid: $planUID,
      fingerprint: $fingerprint,
      contentDigest: $contentDigest,
      artifactDigest: $artifactDigest,
      coordinationDigest: $coordinationDigest,
      targetIdentityDigest: $targetDigest,
      actualStateFingerprint: $actualFingerprint,
      desiredStateFingerprint: $desiredFingerprint,
      policyFingerprint: $policyFingerprint,
      verificationPolicyUID: $verificationPolicyUID,
      verificationPolicyDigest: $verificationPolicyDigest,
      executionBindingID: $executionBindingID,
      controllerImage: $controllerImage,
      controllerRevision: $controllerRevision,
      controllerStateVersion: $controllerStateVersion,
      ptahVersion: $ptahVersion,
      executorImage: $executorImage,
      runnerImage: $runnerImage,
      runnerProtocolVersion: 5,
      destructive: false,
      statementCount: 0,
      createdAt: $now
    }
  }}')
k -n "$TEST_NAMESPACE" patch ptahschema "$SCHEMA_NAME" \
	--subresource=status --type=merge -p "$schema_status" >/dev/null

approval_json() {
	name=$1
	fingerprint=$2
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$name" \
		--arg schemaName "$SCHEMA_NAME" \
		--arg schemaUID "$schema_uid" \
		--arg planName "$PLAN_NAME" \
		--arg planUID "$plan_uid" \
		--arg fingerprint "$fingerprint" '
    {
      apiVersion: "operator.ptah.dev/v1alpha1",
      kind: "PtahSchemaApproval",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        schemaRef: {name: $schemaName, uid: $schemaUID},
        planRef: {name: $planName, uid: $planUID},
        planFingerprint: $fingerprint
      }
    }'
}

printf '%s\n' 'e2e assertions: checking approval stamping and exact binding'
approval_json "$APPROVAL_NAME" "$plan_fingerprint" >"$approval_file"
k create -f "$approval_file" >/dev/null
k -n "$TEST_NAMESPACE" get ptahschemaapproval "$APPROVAL_NAME" -o json |
	jq -e \
		--arg artifactDigest "$artifact_digest" \
		--arg coordinationDigest "$coordination_digest" \
		--arg targetDigest "$target_digest" \
		--arg actualFingerprint "$actual_fingerprint" \
		--arg desiredFingerprint "$desired_fingerprint" \
		--arg policyFingerprint "$policy_fingerprint" \
		--arg verificationPolicyUID "$policy_uid" \
		--arg verificationPolicyDigest "$policy_digest" \
		--arg executionBindingID "$execution_binding_id" \
		--arg controllerImage "$controller_image" \
		--arg controllerRevision "$controller_revision" \
		--argjson controllerStateVersion "$controller_state_version" \
		--arg ptahVersion "$PTAH_VERSION" \
		--arg executorImage "$EXECUTOR_IMAGE" \
		--arg runnerImage "$RUNNER_IMAGE" '
      .spec.approver.username != "" and
      .spec.approvedAt != null and
      .spec.mutationRequestUID != "" and
      .spec.artifactDigest == $artifactDigest and
      .spec.coordinationDigest == $coordinationDigest and
      .spec.targetIdentityDigest == $targetDigest and
      .spec.actualStateFingerprint == $actualFingerprint and
      .spec.desiredStateFingerprint == $desiredFingerprint and
      .spec.policyFingerprint == $policyFingerprint and
      .spec.verificationPolicyUID == $verificationPolicyUID and
      .spec.verificationPolicyDigest == $verificationPolicyDigest and
      .spec.executionBindingID == $executionBindingID and
      .spec.controllerImage == $controllerImage and
      .spec.controllerRevision == $controllerRevision and
      .spec.controllerStateVersion == $controllerStateVersion and
      .spec.ptahVersion == $ptahVersion and
      .spec.executorImage == $executorImage and
      .spec.runnerImage == $runnerImage and
      .spec.runnerProtocolVersion == 5
    ' >/dev/null || fail "mutating webhook did not stamp identity and hydrate the plan binding"

for missing_binding in schema-name schema-uid plan-name plan-uid plan-fingerprint; do
	case "$missing_binding" in
	schema-name)
		missing_filter='del(.spec.schemaRef.name)'
		missing_pattern='explicitly identify the schema name and UID'
		;;
	schema-uid)
		missing_filter='del(.spec.schemaRef.uid)'
		missing_pattern='explicitly identify the schema name and UID'
		;;
	plan-name)
		missing_filter='del(.spec.planRef.name)'
		missing_pattern='explicitly identify the plan name and UID'
		;;
	plan-uid)
		missing_filter='del(.spec.planRef.uid)'
		missing_pattern='explicitly identify the plan name and UID'
		;;
	plan-fingerprint)
		missing_filter='del(.spec.planFingerprint)'
		missing_pattern='explicitly identify the plan fingerprint'
		;;
	esac
	jq --arg name "e2e-missing-${missing_binding}" \
		".metadata.name = \$name | ${missing_filter}" \
		"$approval_file" >"$missing_fingerprint_file"
	expect_denied "approval without explicit ${missing_binding}" "$missing_pattern" \
		"$missing_fingerprint_file" "$error_file"
done

jq --arg name e2e-conflicting-artifact \
	--arg digest sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa '
    .metadata.name = $name | .spec.artifactDigest = $digest
  ' "$approval_file" >"$invalid_approval_file"
expect_denied "approval with a conflicting derived artifact binding" \
	'artifact digest conflicts with the immutable plan' \
	"$invalid_approval_file" "$error_file"
jq --arg name e2e-conflicting-controller-image \
	--arg image 'e2e.invalid/manager@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' '
    .metadata.name = $name | .spec.controllerImage = $image
  ' "$approval_file" >"$invalid_approval_file"
expect_denied "approval with a conflicting controller image binding" \
	'controller image conflicts with the immutable plan' \
	"$invalid_approval_file" "$error_file"
# Version 4 is the immediately previous runner contract and must not be
# accepted against a version-5 immutable plan.
jq --arg name e2e-conflicting-protocol '
    .metadata.name = $name | .spec.runnerProtocolVersion = 4
  ' "$approval_file" >"$invalid_approval_file"
expect_denied "approval with a conflicting derived protocol binding" \
	'runner protocol version conflicts with the immutable plan' \
	"$invalid_approval_file" "$error_file"

bad_fingerprint=sha256:8888888888888888888888888888888888888888888888888888888888888888
approval_json e2e-invalid-approval "$bad_fingerprint" >"$invalid_approval_file"
expect_denied "approval with a stale plan binding" 'plan fingerprint does not match the immutable plan' \
	"$invalid_approval_file" "$error_file"

if k -n "$TEST_NAMESPACE" patch ptahschemaapproval "$APPROVAL_NAME" --type=merge \
	-p "{\"spec\":{\"planFingerprint\":\"$bad_fingerprint\"}}" \
	>"$error_file.stdout" 2>"$error_file"; then
	fail "immutable approval spec update unexpectedly succeeded"
fi
grep -Eiq 'immutable' "$error_file" ||
	fail "approval update was refused for an unexpected reason"

printf '%s\n' 'e2e assertions: checking cross-namespace approval refusal'
jq --arg namespace "$FOREIGN_NAMESPACE" \
	'.metadata.namespace = $namespace |
	 .metadata.name = "foreign-plan" |
	 del(.metadata.ownerReferences, .metadata.finalizers)' \
	"$plan_file" >"$foreign_plan_file"
foreign_plan_uid=$(k create -f "$foreign_plan_file" -o jsonpath='{.metadata.uid}')
[ -n "$foreign_plan_uid" ] || fail "foreign plan was created without a UID"
assert_foreign_plan_stable() {
	k -n "$FOREIGN_NAMESPACE" get ptahschemaplan foreign-plan -o json |
		jq -e --arg uid "$foreign_plan_uid" '
          .metadata.uid == $uid and
          .metadata.deletionTimestamp == null and
          ((.metadata.ownerReferences // []) | length == 0) and
          ((.metadata.finalizers // []) | length == 0)
        ' >/dev/null || fail "foreign plan disappeared, changed, or entered deletion"
}
assert_foreign_plan_stable
if k -n "$TEST_NAMESPACE" get ptahschemaplan foreign-plan >/dev/null 2>&1; then
	fail "cross-namespace approval fixture unexpectedly has a local plan"
fi
jq \
	--arg namespace "$TEST_NAMESPACE" \
	--arg planName foreign-plan \
	--arg planUID "$foreign_plan_uid" \
	'.metadata.namespace = $namespace |
     .metadata.name = "e2e-cross-namespace-approval" |
     .spec.planRef.name = $planName |
     .spec.planRef.uid = $planUID' \
	"$approval_file" >"$foreign_approval_file"
expect_denied "cross-namespace approval" 'foreign-plan.*not found|not found.*foreign-plan' \
	"$foreign_approval_file" "$error_file"
assert_foreign_plan_stable
if k -n "$TEST_NAMESPACE" get ptahschemaapproval e2e-cross-namespace-approval \
	>/dev/null 2>&1; then
	fail "cross-namespace approval refusal created an approval"
fi

printf '%s\n' 'e2e assertions: PASS control-plane contract'
