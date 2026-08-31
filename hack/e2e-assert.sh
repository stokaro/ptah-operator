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
PTAH_VERSION=${E2E_PTAH_VERSION:-v0.3.0}

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

for value_name in KUBECONFIG_FILE OPERATOR_NAMESPACE TEST_NAMESPACE FOREIGN_NAMESPACE HELM_RELEASE EXECUTOR_IMAGE RUNNER_IMAGE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
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
        ((.namespaceSelector // {}) == {}) and ((.objectSelector // {}) == {}) and
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
cleanup_files() {
	rm -f "$schema_file" "$invalid_schema_file" "$plan_file" "$approval_file" \
		"$invalid_approval_file" \
		"$missing_fingerprint_file" "$foreign_plan_file" "$foreign_approval_file" \
		"$error_file" "$error_file.stdout"
}
trap cleanup_files EXIT
trap 'exit 130' HUP INT TERM

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

k create -f "$schema_file" >/dev/null
k -n "$TEST_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Suspended \
	ptahschema/"$SCHEMA_NAME" --timeout=90s
schema_uid=$(k -n "$TEST_NAMESPACE" get ptahschema "$SCHEMA_NAME" -o jsonpath='{.metadata.uid}')
schema_generation=$(k -n "$TEST_NAMESPACE" get ptahschema "$SCHEMA_NAME" -o jsonpath='{.metadata.generation}')

plan_fingerprint=sha256:1111111111111111111111111111111111111111111111111111111111111111
artifact_digest=sha256:2222222222222222222222222222222222222222222222222222222222222222
content_digest=sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881
coordination_digest=sha256:a039cc1dcc29e539b94d48451d5b4b5fa2b2eebc7927f5c4e106679b98479725
target_digest=sha256:4444444444444444444444444444444444444444444444444444444444444444
actual_fingerprint=sha256:5555555555555555555555555555555555555555555555555555555555555555
drift_report_digest=sha256:9999999999999999999999999999999999999999999999999999999999999999
desired_fingerprint=sha256:6666666666666666666666666666666666666666666666666666666666666666
policy_fingerprint=sha256:7777777777777777777777777777777777777777777777777777777777777777
created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

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
      contractVersion: 1,
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
      ptahVersion: $ptahVersion,
      executorImage: $executorImage,
      runnerImage: $runnerImage,
      runnerProtocolVersion: 3,
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
      ptahVersion: $ptahVersion,
      executorImage: $executorImage,
      runnerImage: $runnerImage,
      runnerProtocolVersion: 3,
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
      .spec.ptahVersion == $ptahVersion and
      .spec.executorImage == $executorImage and
      .spec.runnerImage == $runnerImage and
      .spec.runnerProtocolVersion == 3
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
jq --arg name e2e-conflicting-protocol '
    .metadata.name = $name | .spec.runnerProtocolVersion = 2
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
