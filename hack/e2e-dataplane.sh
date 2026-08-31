#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

KUBECONFIG_FILE=${E2E_KUBECONFIG:-}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:-}
HELM_RELEASE=${E2E_HELM_RELEASE:-}
EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
RUNNER_IMAGE=${E2E_RUNNER_IMAGE:-}
FIXTURE_IMAGE=${E2E_FIXTURE_IMAGE:-}
POSTGRES_IMAGE=${E2E_POSTGRES_IMAGE:-}
MYSQL_IMAGE=${E2E_MYSQL_IMAGE:-}
REGISTRY_IP=${E2E_REGISTRY_IP:-}
REGISTRY_SERVICE=${E2E_REGISTRY_SERVICE:-registry}
REGISTRY_CREDENTIALS_FILE=${E2E_REGISTRY_CREDENTIALS_FILE:-}
RECONCILE_INTERVAL=${E2E_RECONCILE_INTERVAL:-1m}
TAG_MOVE_INTERVAL=${E2E_TAG_MOVE_INTERVAL:-2m}
APPROVAL_INTERVAL=${E2E_APPROVAL_INTERVAL:-5m}
STALE_APPROVAL_INTERVAL=${E2E_STALE_APPROVAL_INTERVAL:-4m}
QUIESCENT_INTERVAL=${E2E_QUIESCENT_INTERVAL:-30m}
BLOCKED_REFRESH_SECONDS=${E2E_BLOCKED_REFRESH_SECONDS:-30}
BLOCKED_REFRESH_INTERVAL=${BLOCKED_REFRESH_SECONDS}s
TIMEOUT_SECONDS=${E2E_TIMEOUT_SECONDS:-600}
ADMISSION_RUNTIME_CLASS=ptah-e2e-runtime
ADMISSION_RUNTIME_TAINT=operator.ptah.dev/e2e-runtime

# Imported variables retain their export attribute across reassignment in
# POSIX shells. Clear every secret-bearing name before loading task values.
unset REGISTRY_PASSWORD PG_PASSWORD PG_URL MYSQL_PASSWORD MYSQL_ROOT_PASSWORD MYSQL_URL

fail() {
	printf 'e2e data plane: %s\n' "$*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "required command is not installed: $1"
}

is_pinned_image() {
	printf '%s\n' "$1" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
		return
	fi
	shasum -a 256 | awk '{print $1}'
}

coordination_digest() {
	coordination_engine=$1
	coordination_key=$2
	coordination_canonical=$(jq -cn \
		--arg engine "$coordination_engine" \
		--arg key "$coordination_key" '
      {contract_version: 1, engine: $engine, coordination_key: $key}
    ')
	printf 'sha256:%s\n' "$(printf '%s' "$coordination_canonical" | sha256)"
}

for command_name in kubectl jq awk sed grep tr cksum mktemp date sleep tail stat go env; do
	require_command "$command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "sha256sum or shasum is required"
fi
for value_name in \
	KUBECONFIG_FILE OPERATOR_NAMESPACE TEST_NAMESPACE HELM_RELEASE EXECUTOR_IMAGE \
	RUNNER_IMAGE FIXTURE_IMAGE POSTGRES_IMAGE MYSQL_IMAGE REGISTRY_IP REGISTRY_CREDENTIALS_FILE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
if [ ! -f "$REGISTRY_CREDENTIALS_FILE" ] || [ -L "$REGISTRY_CREDENTIALS_FILE" ]; then
	fail "E2E_REGISTRY_CREDENTIALS_FILE must name a regular non-symlink file"
fi
if registry_credentials_mode=$(stat -c '%a' "$REGISTRY_CREDENTIALS_FILE" 2>/dev/null); then
	:
else
	registry_credentials_mode=$(stat -f '%Lp' "$REGISTRY_CREDENTIALS_FILE" 2>/dev/null) ||
		fail "could not inspect registry credential file permissions"
fi
[ "$registry_credentials_mode" = 600 ] ||
	fail "E2E_REGISTRY_CREDENTIALS_FILE must have mode 0600"
jq -e '
  type == "object" and
  (keys == ["password", "username"]) and
  (.username | type == "string" and length > 0 and test("^[A-Za-z0-9_.-]+$")) and
  (.password | type == "string" and length > 0 and contains("\\n") | not)
' "$REGISTRY_CREDENTIALS_FILE" >/dev/null ||
	fail "E2E_REGISTRY_CREDENTIALS_FILE has an invalid shape"
REGISTRY_USERNAME=$(jq -er '.username' "$REGISTRY_CREDENTIALS_FILE")
REGISTRY_PASSWORD=$(jq -er '.password' "$REGISTRY_CREDENTIALS_FILE")
for image in "$EXECUTOR_IMAGE" "$RUNNER_IMAGE" "$FIXTURE_IMAGE" "$POSTGRES_IMAGE" "$MYSQL_IMAGE"; do
	is_pinned_image "$image" || fail "data-plane images must be pinned by a lowercase SHA-256 digest: $image"
done
printf '%s\n' "$REGISTRY_IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' ||
	fail "E2E_REGISTRY_IP must be an IPv4 address on the kind Docker network"
printf '%s\n' "$TIMEOUT_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_TIMEOUT_SECONDS must be a positive integer"
printf '%s\n' "$BLOCKED_REFRESH_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_BLOCKED_REFRESH_SECONDS must be a positive integer"
[ "$BLOCKED_REFRESH_SECONDS" -ge 10 ] ||
	fail "E2E_BLOCKED_REFRESH_SECONDS must be at least 10"
[ "$RECONCILE_INTERVAL" != "$APPROVAL_INTERVAL" ] ||
	fail "scheduled no-op interval must differ from approval interval"
[ "$TAG_MOVE_INTERVAL" != "$RECONCILE_INTERVAL" ] ||
	fail "tag-move interval must differ from scheduled no-op interval"
[ "$STALE_APPROVAL_INTERVAL" != "$TAG_MOVE_INTERVAL" ] ||
	fail "stale-approval interval must differ from tag-move interval"
[ "$QUIESCENT_INTERVAL" != "$BLOCKED_REFRESH_INTERVAL" ] ||
	fail "quiescent interval must differ from blocked refresh interval"
minimum_gate_timeout=$((BLOCKED_REFRESH_SECONDS * 3 + 120))
[ "$TIMEOUT_SECONDS" -ge "$minimum_gate_timeout" ] ||
	fail "E2E_TIMEOUT_SECONDS must cover three blocked refresh intervals plus 120 seconds"

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

CONTROLLER_NAME="${HELM_RELEASE}-ptah-operator"
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-data-e2e.XXXXXX")
chmod 700 "$WORK_DIR"
umask 077
RESOURCE_FILE=$WORK_DIR/resource.json
SECRET_FILE=$WORK_DIR/database-secrets.json
LOG_FILE=$WORK_DIR/logs.txt
AUDITED_JOBS_FILE=$WORK_DIR/audited-jobs.txt
OBSERVED_JOBS_FILE=$WORK_DIR/observed-jobs.jsonl
CREDENTIAL_PATTERNS_FILE=$WORK_DIR/credential-patterns.txt
PG_PASSWORD_FILE=$WORK_DIR/postgresql.password
PG_URL_FILE=$WORK_DIR/postgresql.url
MYSQL_PASSWORD_FILE=$WORK_DIR/mysql.password
MYSQL_ROOT_PASSWORD_FILE=$WORK_DIR/mysql-root.password
MYSQL_URL_FILE=$WORK_DIR/mysql.url
REGISTRY_PASSWORD_FILE=$WORK_DIR/registry.password
RESULT_ASSERT_BINARY=$WORK_DIR/e2e-resultassert
RESULT_LOG_FILE=$WORK_DIR/runner-result.log
ADMISSION_ERROR_FILE=$WORK_DIR/admission-error.txt
MYSQL_DESTRUCTIVE_SCHEMA=
MYSQL_DESTRUCTIVE_PLAN=
MYSQL_DESTRUCTIVE_PLAN_UID=
MYSQL_DESTRUCTIVE_APPLY_CHECKPOINT=
MYSQL_DESTRUCTIVE_DIGEST=
PERIODIC_NOOP_CHECKPOINT=
: >"$AUDITED_JOBS_FILE"
: >"$OBSERVED_JOBS_FILE"
RBAC_PAUSED=0
RBAC_RULE_INDEX=
RBAC_ORIGINAL_VERBS=
RBAC_STATUS_API_GROUPS=
RBAC_STATUS_RESOURCES=
EPHEMERAL_SUBRESOURCE_TESTED=0

mkdir -p "$WORK_DIR/go-cache"
env GOCACHE="$WORK_DIR/go-cache" go build -trimpath \
	-o "$RESULT_ASSERT_BINARY" ./test/e2e/resultassert

collect_diagnostics() {
	printf '%s\n' 'e2e data plane: collecting failure diagnostics' >&2
	k -n "$TEST_NAMESPACE" get ptahschemas,ptahschemaplans,ptahschemaapprovals -o wide >&2 || true
	k -n "$TEST_NAMESPACE" get jobs,pods -o wide >&2 || true
	printf '%s\n' 'e2e data plane: raw events and logs are suppressed to protect credential-isolation failures' >&2
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	set +e
	if [ "$status" -ne 0 ]; then
		collect_diagnostics
	fi
	if [ "$RBAC_PAUSED" -eq 1 ]; then
		if ! resume_controller_status_writes; then
			printf '%s\n' 'e2e data plane: could not restore controller status-write RBAC' >&2
			status=1
		fi
	fi
	case "$WORK_DIR" in
		"${TMPDIR:-/tmp}"/ptah-operator-data-e2e.*) rm -rf -- "$WORK_DIR" ;;
		*)
			printf 'e2e data plane: refusing to remove unexpected work directory %s\n' \
				"$WORK_DIR" >&2
			status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

deadline_from_now() {
	printf '%s\n' "$(($(date +%s) + TIMEOUT_SECONDS))"
}

scan_file_for_credentials() {
	scan_file=$1
	scan_context=$2
	[ -s "$CREDENTIAL_PATTERNS_FILE" ] ||
		fail "credential scanner has no non-empty protected patterns"
	if grep -F -f "$CREDENTIAL_PATTERNS_FILE" "$scan_file" >/dev/null; then
		fail "a task credential escaped into $scan_context"
	else
		scan_status=$?
		[ "$scan_status" -eq 1 ] ||
			fail "credential scanner failed closed while checking $scan_context"
	fi
}

record_observed_jobs() {
	k -n "$TEST_NAMESPACE" get jobs -o json | jq -c '
    .items[] | {
      uid: .metadata.uid,
      name: .metadata.name,
      created: .metadata.creationTimestamp,
      schema: (.metadata.labels["operator.ptah.dev/schema"] // ""),
      operation: (.metadata.labels["operator.ptah.dev/operation"] // "")
    }
  ' | while IFS= read -r observed_job; do
		observed_uid=$(printf '%s\n' "$observed_job" | jq -r '.uid')
		if ! grep -F "\"uid\":\"${observed_uid}\"" "$OBSERVED_JOBS_FILE" >/dev/null 2>&1; then
			printf '%s\n' "$observed_job" >>"$OBSERVED_JOBS_FILE"
		fi
	done
}

assert_active_pod_ephemeral_container_rejected() {
	active_pod_object=$(k -n "$TEST_NAMESPACE" get pods \
		-l 'app.kubernetes.io/managed-by=ptah-operator,app.kubernetes.io/component=schema-operation' \
		-o json | jq -c '
      [.items[] | select(
        .metadata.deletionTimestamp == null and
        (.status.phase == "Pending" or .status.phase == "Running") and
        any(.metadata.ownerReferences[]?;
          .apiVersion == "batch/v1" and .kind == "Job" and .controller == true))] |
      first // empty
    ')
	[ -n "$active_pod_object" ] || return 1
	active_pod_name=$(printf '%s\n' "$active_pod_object" | jq -er '.metadata.name')
	active_pod_uid=$(printf '%s\n' "$active_pod_object" | jq -er '.metadata.uid')
	active_job_name=$(printf '%s\n' "$active_pod_object" | jq -er '
      .metadata.ownerReferences[] |
      select(.apiVersion == "batch/v1" and .kind == "Job" and .controller == true) |
      .name
    ')
	active_job_uid=$(printf '%s\n' "$active_pod_object" | jq -er '
      .metadata.ownerReferences[] |
      select(.apiVersion == "batch/v1" and .kind == "Job" and .controller == true) |
      .uid
    ')
	active_schema=$(printf '%s\n' "$active_pod_object" |
		jq -er '.metadata.labels["operator.ptah.dev/schema"]')
	active_job_object=$(k -n "$TEST_NAMESPACE" get job "$active_job_name" -o json 2>/dev/null || true)
	[ -n "$active_job_object" ] || return 1
	printf '%s\n' "$active_job_object" | jq -e \
		--arg jobUID "$active_job_uid" \
		--arg podUID "$active_pod_uid" '
      .metadata.uid == $jobUID and
      .status.active >= 1 and
      .spec.template.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] != null and
      $podUID != ""
    ' >/dev/null || return 1
	active_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$active_schema" -o json 2>/dev/null || true)
	[ -n "$active_schema_object" ] || return 1
	printf '%s\n' "$active_schema_object" | jq -e \
		--arg jobName "$active_job_name" \
		--arg jobUID "$active_job_uid" '
      .status.activeOperation.jobName == $jobName and
      .status.activeOperation.jobUID == $jobUID and
      (.status.activeOperation.admissionSnapshot.digest |
        test("^sha256:[0-9a-f]{64}$"))
    ' >/dev/null || return 1

	jq -n --arg uid "$active_pod_uid" --arg image "$RUNNER_IMAGE" '
    {
      metadata: {uid: $uid},
      spec: {
        ephemeralContainers: [{
          name: "forbidden-e2e",
          image: $image,
          imagePullPolicy: "IfNotPresent",
          command: ["/ptah-runner"],
          args: ["--help"],
          securityContext: {
            allowPrivilegeEscalation: false,
            capabilities: {drop: ["ALL"]}
          }
        }]
      }
    }
  ' >"$RESOURCE_FILE"
	# kubectl binds this request to the captured Pod UID and sends it to the
	# /ephemeralcontainers subresource, which must remain inside the snapshot.
	if k -n "$TEST_NAMESPACE" patch pod "$active_pod_name" \
		--subresource=ephemeralcontainers --type=merge \
		--patch-file="$RESOURCE_FILE" >"$ADMISSION_ERROR_FILE" 2>&1; then
		fail "Pod intent admission allowed an ephemeral container on exact active Pod $active_pod_name UID $active_pod_uid"
	fi
	grep -F 'vpodintent.operator.ptah.dev' "$ADMISSION_ERROR_FILE" >/dev/null ||
		fail "ephemeral-container rejection did not come from the Pod intent webhook"
	grep -F 'persisted admission envelope' "$ADMISSION_ERROR_FILE" >/dev/null ||
		fail "Pod intent webhook rejected the active Pod for an unexpected reason"
	k -n "$TEST_NAMESPACE" get pod "$active_pod_name" -o json | jq -e \
		--arg podUID "$active_pod_uid" \
		--arg jobUID "$active_job_uid" '
      .metadata.uid == $podUID and
      any(.metadata.ownerReferences[]?;
        .apiVersion == "batch/v1" and .kind == "Job" and
        .uid == $jobUID and .controller == true) and
      ((.spec.ephemeralContainers // []) | length) == 0
    ' >/dev/null || fail "active Pod identity changed during the negative subresource test"
	: >"$ADMISSION_ERROR_FILE"
	return 0
}

audit_completed_jobs() {
	if [ "$EPHEMERAL_SUBRESOURCE_TESTED" -eq 0 ] &&
		assert_active_pod_ephemeral_container_rejected; then
		EPHEMERAL_SUBRESOURCE_TESTED=1
	fi
	record_observed_jobs
	audit_jobs=$(k -n "$TEST_NAMESPACE" get jobs -o json)
	printf '%s\n' "$audit_jobs" | jq -r '
    .items[] |
	    select(.status.conditions // [] |
	      any((.type == "Complete" or .type == "Failed") and .status == "True")) |
    [.metadata.uid, .metadata.name] | @tsv
  ' | while IFS="$(printf '\t')" read -r audit_uid audit_name; do
		[ -n "$audit_uid" ] || continue
		if grep -Fx "$audit_uid" "$AUDITED_JOBS_FILE" >/dev/null 2>&1; then
			continue
		fi
		audit_job_object=$(k -n "$TEST_NAMESPACE" get job "$audit_name" -o json 2>/dev/null || true)
		[ -n "$audit_job_object" ] ||
			fail "terminal Job $audit_name UID $audit_uid disappeared before its audit"
		printf '%s\n' "$audit_job_object" | jq -e \
			--arg uid "$audit_uid" '
          .metadata.uid == $uid and
          (.status.conditions // [] |
            any((.type == "Complete" or .type == "Failed") and .status == "True"))
        ' >/dev/null ||
			fail "terminal Job $audit_name was replaced before UID $audit_uid could be audited"
		audit_pods=$(k -n "$TEST_NAMESPACE" get pods -o json)
		audit_owned_pods=$(printf '%s\n' "$audit_pods" | jq \
			--arg uid "$audit_uid" '
          {apiVersion: "v1", kind: "List", items: [
            .items[] | select(.metadata.ownerReferences // [] | any(
              .apiVersion == "batch/v1" and .kind == "Job" and
              .uid == $uid and .controller == true))
          ]}
        ')
		[ "$(printf '%s\n' "$audit_owned_pods" | jq '.items | length')" -gt 0 ] ||
			fail "terminal Job $audit_name UID $audit_uid has no exact owned Pod to audit"
		if printf '%s\n' "$audit_job_object" | jq -e \
			--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" '
          .metadata.labels["app.kubernetes.io/component"] == "schema-operation" and
          .spec.template.spec.runtimeClassName == $runtimeClass
        ' >/dev/null; then
			printf '%s\n' "$audit_job_object" | jq -e \
				--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" '
              (.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] // "") as $digest |
              ($digest | test("^sha256:[0-9a-f]{64}$")) and
              .spec.template.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] == $digest and
              .spec.template.spec.runtimeClassName == $runtimeClass
            ' >/dev/null ||
				fail "managed Job $audit_name lacks its persisted admission binding"
		fi
		: >"$RESOURCE_FILE"
		printf '%s\n%s\n' "$audit_job_object" "$audit_owned_pods" >"$RESOURCE_FILE"
		scan_file_for_credentials "$RESOURCE_FILE" \
			"Job $audit_name UID $audit_uid and its exact owned Pods"
		printf '%s\n' "$audit_owned_pods" | jq -r '
          .items[] | [.metadata.uid, .metadata.name] | @tsv
        ' | while IFS="$(printf '\t')" read -r audit_pod_uid audit_pod_name; do
			[ -n "$audit_pod_uid" ] || continue
			audit_pod_object=$(k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" -o json 2>/dev/null || true)
			[ -n "$audit_pod_object" ] ||
				fail "Pod $audit_pod_name UID $audit_pod_uid disappeared before terminal Job audit"
			printf '%s\n' "$audit_pod_object" | jq -e \
				--arg podUID "$audit_pod_uid" \
				--arg jobUID "$audit_uid" '
              ([.status.initContainerStatuses // [], .status.containerStatuses // [],
                .status.ephemeralContainerStatuses // []] | add) as $statuses |
              .metadata.uid == $podUID and
              (.metadata.ownerReferences // [] | any(
                .apiVersion == "batch/v1" and .kind == "Job" and
                .uid == $jobUID and .controller == true)) and
              (.status.phase == "Succeeded" or .status.phase == "Failed") and
              all($statuses[]; (.restartCount // 0) == 0) and
              ([.spec.initContainers[]?.name, .spec.containers[]?.name,
                .spec.ephemeralContainers[]?.name] | sort) ==
              ([$statuses[] | select(.state.terminated != null) | .name] | sort)
            ' >/dev/null ||
				fail "exact Pod $audit_pod_name UID $audit_pod_uid lacks complete terminal evidence"
			if printf '%s\n' "$audit_job_object" | jq -e \
				--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" '
              .metadata.labels["app.kubernetes.io/component"] == "schema-operation" and
              .spec.template.spec.runtimeClassName == $runtimeClass
            ' >/dev/null; then
				printf '%s\n' "$audit_pod_object" | jq -e \
					--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" \
					--arg runtimeTaint "$ADMISSION_RUNTIME_TAINT" \
					--arg pullSecret "$REGISTRY_PULL_SECRET" '
                  (.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] // "") as $digest |
                  ($digest | test("^sha256:[0-9a-f]{64}$")) and
                  .spec.runtimeClassName == $runtimeClass and
                  .spec.nodeSelector["kubernetes.io/os"] == "linux" and
                  .spec.overhead.memory == "8Mi" and
                  .spec.imagePullSecrets == [{name: $pullSecret}] and
                  all((.spec.containers + (.spec.initContainers // []))[];
                    .resources.requests.cpu == "10m" and
                    .resources.requests.memory == "16Mi" and
                    .resources.limits.cpu == "100m" and
                    .resources.limits.memory == "64Mi") and
                  any(.spec.tolerations[];
                    .key == "node.kubernetes.io/not-ready" and
                    .operator == "Exists" and .effect == "NoExecute" and .tolerationSeconds == 300) and
                  any(.spec.tolerations[];
                    .key == "node.kubernetes.io/unreachable" and
                    .operator == "Exists" and .effect == "NoExecute" and .tolerationSeconds == 300) and
                  any(.spec.tolerations[];
                    .key == $runtimeTaint and .operator == "Exists" and .effect == "NoSchedule")
                ' >/dev/null ||
					fail "managed Pod $audit_pod_name lacks exact LimitRange, ServiceAccount, RuntimeClass, or default-toleration admission"
			fi
			audit_containers=$(printf '%s\n' "$audit_pod_object" | jq -r '
              [(.status.initContainerStatuses // [])[],
               (.status.containerStatuses // [])[],
               (.status.ephemeralContainerStatuses // [])[]] |
              .[] | select(.state.terminated != null) | .name
            ')
			[ -n "$audit_containers" ] ||
				fail "exact Pod $audit_pod_name UID $audit_pod_uid has no terminated container logs"
			for audit_container in $audit_containers; do
				if ! k -n "$TEST_NAMESPACE" logs pod/"$audit_pod_name" \
					-c "$audit_container" >"$LOG_FILE" 2>&1; then
					fail "could not audit $audit_container logs for exact Pod $audit_pod_name UID $audit_pod_uid"
				fi
				scan_file_for_credentials "$LOG_FILE" \
					"$audit_container logs for exact Pod $audit_pod_name UID $audit_pod_uid"
			done
			k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" -o json | jq -e \
				--arg podUID "$audit_pod_uid" \
				--arg jobUID "$audit_uid" '
              .metadata.uid == $podUID and
              (.metadata.ownerReferences // [] | any(
                .apiVersion == "batch/v1" and .kind == "Job" and
                .uid == $jobUID and .controller == true))
            ' >/dev/null ||
				fail "exact Pod $audit_pod_name changed identity during its log audit"
		done
		k -n "$TEST_NAMESPACE" get job "$audit_name" -o json | jq -e \
			--arg uid "$audit_uid" '
          .metadata.uid == $uid and
          (.status.conditions // [] |
            any((.type == "Complete" or .type == "Failed") and .status == "True"))
        ' >/dev/null ||
			fail "terminal Job $audit_name changed identity during its exact Pod audit"
		printf '%s\n' "$audit_uid" >>"$AUDITED_JOBS_FILE"
	done
}

audit_runtime_credentials() {
	audit_completed_jobs
	: >"$RESOURCE_FILE"
	k -n "$TEST_NAMESPACE" get \
		ptahschemas,ptahschemaplans,ptahschemaapprovals,jobs,pods,configmaps,events,deployments,replicasets,services,serviceaccounts \
		-o json >>"$RESOURCE_FILE"
	k -n "$OPERATOR_NAMESPACE" get \
		pods,configmaps,events,deployments,replicasets,services,serviceaccounts,leases \
		-o json >>"$RESOURCE_FILE"
	scan_file_for_credentials "$RESOURCE_FILE" "a non-Secret Kubernetes resource"
	manager_audit_pods=$(k -n "$OPERATOR_NAMESPACE" get pods \
		-l "app.kubernetes.io/name=ptah-operator,app.kubernetes.io/instance=${HELM_RELEASE},app.kubernetes.io/component=controller" \
		-o json)
	[ "$(printf '%s\n' "$manager_audit_pods" | jq '[.items[] | select(.metadata.deletionTimestamp == null)] | length')" -gt 0 ] ||
		fail "no exact manager Pod was available for credential audit"
	printf '%s\n' "$manager_audit_pods" | jq -e '
      all(.items[] | select(.metadata.deletionTimestamp == null);
        ([.status.initContainerStatuses // [], .status.containerStatuses // [],
          .status.ephemeralContainerStatuses // []] | add) as $statuses |
        all($statuses[]; (.restartCount // 0) == 0))
    ' >/dev/null || fail "a manager container restarted before its complete log history was audited"
	printf '%s\n' "$manager_audit_pods" | jq -r '
      .items[] | select(.metadata.deletionTimestamp == null) | .metadata.name
    ' | while IFS= read -r manager_audit_pod; do
		[ -n "$manager_audit_pod" ] || continue
		manager_audit_uid=$(k -n "$OPERATOR_NAMESPACE" get pod "$manager_audit_pod" -o jsonpath='{.metadata.uid}')
		[ -n "$manager_audit_uid" ] || fail "manager Pod $manager_audit_pod has no exact UID"
		if ! k -n "$OPERATOR_NAMESPACE" logs pod/"$manager_audit_pod" \
			--all-containers >"$LOG_FILE" 2>&1; then
			fail "could not audit logs for exact manager Pod $manager_audit_pod UID $manager_audit_uid"
		fi
		scan_file_for_credentials "$LOG_FILE" "logs for exact manager Pod $manager_audit_pod"
	done
	for audit_pod in $(k -n "$TEST_NAMESPACE" get pods -o name); do
		audit_pod_object=$(k -n "$TEST_NAMESPACE" get "$audit_pod" -o json)
		audit_pod_phase=$(printf '%s\n' "$audit_pod_object" | jq -r '.status.phase // ""')
		printf '%s\n' "$audit_pod_object" | jq -e '
        ([.status.initContainerStatuses // [], .status.containerStatuses // [],
          .status.ephemeralContainerStatuses // []] | add) as $statuses |
        all($statuses[]; (.restartCount // 0) == 0)
      ' >/dev/null || fail "$audit_pod restarted a container before complete log audit"
		audit_started_containers=$(printf '%s\n' "$audit_pod_object" | jq -r '
        [(.status.initContainerStatuses // [])[],
         (.status.containerStatuses // [])[],
         (.status.ephemeralContainerStatuses // [])[]] |
        .[] | select(.state.running != null or .state.terminated != null) | .name
      ')
		for audit_container in $audit_started_containers; do
			if ! k -n "$TEST_NAMESPACE" logs "$audit_pod" \
				-c "$audit_container" >"$LOG_FILE" 2>&1; then
				fail "could not audit logs for started container $audit_container in $audit_pod"
			fi
			scan_file_for_credentials "$LOG_FILE" \
				"logs for container $audit_container in $audit_pod"
		done
		case "$audit_pod_phase" in
		Succeeded | Failed)
			printf '%s\n' "$audit_pod_object" | jq -e '
              ([.spec.initContainers // [], .spec.containers // [],
                .spec.ephemeralContainers // []] | add | map(.name) | sort) as $declared |
              ([.status.initContainerStatuses // [], .status.containerStatuses // [],
                .status.ephemeralContainerStatuses // []] | add |
                map(select(.state.terminated != null) | .name) | sort) as $terminated |
              $declared == $terminated
            ' >/dev/null || fail "terminal $audit_pod has unaudited nonterminal containers"
			;;
		esac
	done
}

assert_observed_jobs_audited() {
	jq -er '.uid' "$OBSERVED_JOBS_FILE" | while IFS= read -r observed_uid; do
		[ -n "$observed_uid" ] || continue
		grep -Fx "$observed_uid" "$AUDITED_JOBS_FILE" >/dev/null ||
			fail "observed Job UID $observed_uid disappeared without a complete credential audit"
	done
}

wait_for_schema() {
	wait_schema=$1
	wait_expression=$2
	wait_description=$3
	wait_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		audit_completed_jobs
		if wait_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$wait_schema" -o json 2>/dev/null); then
			if printf '%s\n' "$wait_object" | jq -e "$wait_expression" >/dev/null; then
				return 0
			fi
			wait_phase=$(printf '%s\n' "$wait_object" | jq -r '.status.phase // ""')
			if [ "$wait_phase" = Failed ]; then
				fail "$wait_schema entered Failed while waiting for $wait_description"
			fi
		fi
		sleep 2
	done
	fail "timed out waiting for $wait_schema: $wait_description"
}

wait_for_approval() {
	wait_approval=$1
	wait_expression=$2
	wait_description=$3
	wait_expected_plan_uid=${4:-}
	wait_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		audit_completed_jobs
		if wait_object=$(k -n "$TEST_NAMESPACE" get ptahschemaapproval "$wait_approval" -o json 2>/dev/null); then
			if printf '%s\n' "$wait_object" |
				jq -e --arg expectedPlanUID "$wait_expected_plan_uid" "$wait_expression" >/dev/null; then
				return 0
			fi
		fi
		sleep 2
	done
	fail "timed out waiting for $wait_approval: $wait_description"
}

assert_approval_consumed() {
	consumed_approval=$1
	consumed_plan_uid=$2
	# shellcheck disable=SC2016 # jq variable is supplied by wait_for_approval.
	wait_for_approval "$consumed_approval" '
      .spec.planRef.uid == $expectedPlanUID and
      .status.observedGeneration == .metadata.generation and
      (.status.conditions | any(
        .type == "Accepted" and .status == "False" and
        .reason == "PlanNoLongerCurrent")) and
      (.status.conditions | any(
        .type == "Consumed" and .status == "True" and .reason == "DispatchCommitted")) and
      (.status.conditions | any(
        .type == "Stale" and .status == "True" and
        .reason == "PlanNoLongerCurrent"))
    ' "the exact consumed approval to retain its later-observation history" \
		"$consumed_plan_uid"
}

wait_for_job() {
	wait_job=$1
	wait_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		audit_completed_jobs
		if wait_object=$(k -n "$TEST_NAMESPACE" get job "$wait_job" -o json 2>/dev/null); then
			if printf '%s\n' "$wait_object" |
				jq -e '.status.conditions // [] | any(.type == "Complete" and .status == "True")' >/dev/null; then
					audit_completed_jobs
					return 0
			fi
			if printf '%s\n' "$wait_object" |
				jq -e '.status.conditions // [] | any(.type == "Failed" and .status == "True")' >/dev/null; then
				fail "Job $wait_job failed"
			fi
		fi
		sleep 2
	done
	fail "timed out waiting for Job $wait_job"
}

checkpoint_jobs() {
	checkpoint_schema=$1
	checkpoint_operation=$2
	checkpoint_file=$3
	record_observed_jobs
	jq -s \
		--arg schema "$checkpoint_schema" \
		--arg operation "$checkpoint_operation" '
      [.[] | select(.schema == $schema and .operation == $operation) | .uid] | sort
    ' "$OBSERVED_JOBS_FILE" >"$checkpoint_file"
}

checkpoint_schema_jobs() {
	checkpoint_schema=$1
	checkpoint_file=$2
	record_observed_jobs
	jq -s \
		--arg schema "$checkpoint_schema" '
      [.[] | select(.schema == $schema) | .uid] | unique | sort
    ' "$OBSERVED_JOBS_FILE" >"$checkpoint_file"
}

job_count_between_checkpoints() {
	bounded_schema=$1
	bounded_operation=$2
	bounded_before=$3
	bounded_after=$4
	jq -s \
		--slurpfile before "$bounded_before" \
		--slurpfile after "$bounded_after" \
		--arg schema "$bounded_schema" \
		--arg operation "$bounded_operation" '
      [.[] |
        select(.schema == $schema and .operation == $operation) |
        .uid as $uid |
        select(($after[0] | index($uid)) != null and
          ($before[0] | index($uid)) == null)] |
      unique_by(.uid) | length
    ' "$OBSERVED_JOBS_FILE"
}

assert_one_job_between_checkpoints() {
	bounded_schema=$1
	bounded_operation=$2
	bounded_before=$3
	bounded_after=$4
	bounded_count=$(job_count_between_checkpoints "$bounded_schema" "$bounded_operation" \
		"$bounded_before" "$bounded_after")
	[ "$bounded_count" -eq 1 ] ||
		fail "$bounded_schema created $bounded_count bounded $bounded_operation Jobs, expected exactly one"
}

assert_no_job_between_checkpoints() {
	bounded_schema=$1
	bounded_operation=$2
	bounded_before=$3
	bounded_after=$4
	bounded_count=$(job_count_between_checkpoints "$bounded_schema" "$bounded_operation" \
		"$bounded_before" "$bounded_after")
	[ "$bounded_count" -eq 0 ] ||
		fail "$bounded_schema created $bounded_count unexpected bounded $bounded_operation Jobs"
}

new_job_count_since() {
	new_schema=$1
	new_operation=$2
	new_checkpoint=$3
	record_observed_jobs
	jq -s \
		--slurpfile before "$new_checkpoint" \
		--arg schema "$new_schema" \
		--arg operation "$new_operation" '
      [.[] |
        select(.schema == $schema and .operation == $operation) |
        .uid as $uid | select(($before[0] | index($uid)) == null)] | length
    ' "$OBSERVED_JOBS_FILE"
}

wait_for_one_new_job() {
	new_schema=$1
	new_operation=$2
	new_checkpoint=$3
	new_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$new_deadline" ]; do
		audit_completed_jobs
		new_count=$(new_job_count_since "$new_schema" "$new_operation" "$new_checkpoint")
		if [ "$new_count" -gt 1 ]; then
			fail "$new_schema created more than one new $new_operation Job"
		fi
		if [ "$new_count" -eq 1 ]; then
			return 0
		fi
		sleep 2
	done
	fail "timed out waiting for a new $new_operation Job for $new_schema"
}

assert_one_new_job() {
	new_schema=$1
	new_operation=$2
	new_checkpoint=$3
	new_count=$(new_job_count_since "$new_schema" "$new_operation" "$new_checkpoint")
	[ "$new_count" -eq 1 ] ||
		fail "$new_schema created $new_count new $new_operation Jobs, expected exactly one"
}

assert_no_new_jobs() {
	new_schema=$1
	new_operation=$2
	new_checkpoint=$3
	new_count=$(new_job_count_since "$new_schema" "$new_operation" "$new_checkpoint")
	[ "$new_count" -eq 0 ] ||
		fail "$new_schema created $new_count unexpected $new_operation Jobs"
}

all_new_jobs_complete() {
	complete_schema=$1
	complete_operation=$2
	complete_checkpoint=$3
	complete_minimum=$4
	record_observed_jobs
	complete_records=$(jq -c -s \
		--slurpfile before "$complete_checkpoint" \
		--arg schema "$complete_schema" \
		--arg operation "$complete_operation" '
      [.[] |
        select(.schema == $schema and .operation == $operation) |
        .uid as $uid | select(($before[0] | index($uid)) == null)] |
      unique_by(.uid)
    ' "$OBSERVED_JOBS_FILE")
	[ "$(printf '%s\n' "$complete_records" | jq 'length')" -ge "$complete_minimum" ] || return 1
	printf '%s\n' "$complete_records" | jq -c '.[]' |
		while IFS= read -r complete_record; do
			complete_name=$(printf '%s\n' "$complete_record" | jq -er '.name')
			complete_uid=$(printf '%s\n' "$complete_record" | jq -er '.uid')
			complete_object=$(k -n "$TEST_NAMESPACE" get job "$complete_name" -o json 2>/dev/null) || exit 1
			printf '%s\n' "$complete_object" | jq -e \
				--arg uid "$complete_uid" \
				--arg schema "$complete_schema" \
				--arg operation "$complete_operation" '
              .metadata.uid == $uid and
              .metadata.labels["operator.ptah.dev/schema"] == $schema and
              .metadata.labels["operator.ptah.dev/operation"] == $operation and
              (.status.conditions // [] |
                any(.type == "Complete" and .status == "True")) and
              (.status.conditions // [] |
                all(.type != "Failed" or .status != "True"))
            ' >/dev/null || exit 1
		done
}

capture_one_new_job_result() {
	result_schema=$1
	result_operation=$2
	result_checkpoint=$3
	result_output=$4
	result_after_checkpoint=${5:-}
	record_observed_jobs
	if [ -n "$result_after_checkpoint" ]; then
		result_records=$(jq -c -s \
			--slurpfile before "$result_checkpoint" \
			--slurpfile after "$result_after_checkpoint" \
			--arg schema "$result_schema" \
			--arg operation "$result_operation" '
          [.[] |
            select(.schema == $schema and .operation == $operation) |
            .uid as $uid |
            select(($after[0] | index($uid)) != null and
              ($before[0] | index($uid)) == null)] |
          unique_by(.uid)
        ' "$OBSERVED_JOBS_FILE")
	else
		result_records=$(jq -c -s \
			--slurpfile before "$result_checkpoint" \
			--arg schema "$result_schema" \
			--arg operation "$result_operation" '
      [.[] |
        select(.schema == $schema and .operation == $operation) |
        .uid as $uid | select(($before[0] | index($uid)) == null)] |
      unique_by(.uid)
	    ' "$OBSERVED_JOBS_FILE")
	fi
	result_count=$(printf '%s\n' "$result_records" | jq 'length')
	[ "$result_count" -eq 1 ] ||
		fail "$result_schema has $result_count new $result_operation Jobs, expected exactly one result"
	CAPTURED_JOB_NAME=$(printf '%s\n' "$result_records" | jq -er '.[0].name')
	CAPTURED_JOB_UID=$(printf '%s\n' "$result_records" | jq -er '.[0].uid')

	result_deadline=$(deadline_from_now)
	result_job=
	while [ "$(date +%s)" -lt "$result_deadline" ]; do
		if result_job=$(k -n "$TEST_NAMESPACE" get job "$CAPTURED_JOB_NAME" -o json 2>/dev/null); then
			printf '%s\n' "$result_job" | jq -e \
				--arg uid "$CAPTURED_JOB_UID" \
				--arg schema "$result_schema" \
				--arg operation "$result_operation" '
              .metadata.uid == $uid and
              .metadata.labels["operator.ptah.dev/schema"] == $schema and
              .metadata.labels["operator.ptah.dev/operation"] == $operation and
              (.metadata.annotations["operator.ptah.dev/operation-id"] | length > 0) and
              .spec.podReplacementPolicy == "Failed" and .spec.backoffLimit == 0
            ' >/dev/null || fail "$CAPTURED_JOB_NAME changed its immutable operation identity"
			if printf '%s\n' "$result_job" | jq -e '
              .status.conditions // [] | any(.type == "Failed" and .status == "True")
            ' >/dev/null; then
				fail "$CAPTURED_JOB_NAME failed before producing a transport-success result"
			fi
			if printf '%s\n' "$result_job" | jq -e '
              .status.conditions // [] | any(.type == "Complete" and .status == "True")
            ' >/dev/null; then
				break
			fi
		fi
		sleep 1
	done
	printf '%s\n' "$result_job" | jq -e '
      .status.conditions // [] | any(.type == "Complete" and .status == "True")
    ' >/dev/null || fail "timed out waiting for exact result Job $CAPTURED_JOB_NAME"
	CAPTURED_OPERATION_ID=$(printf '%s\n' "$result_job" |
		jq -er '.metadata.annotations["operator.ptah.dev/operation-id"]')

	result_pods=$(k -n "$TEST_NAMESPACE" get pods -l "job-name=${CAPTURED_JOB_NAME}" -o json)
	CAPTURED_POD_NAME=$(printf '%s\n' "$result_pods" | jq -er \
		--arg uid "$CAPTURED_JOB_UID" \
		--arg operationID "$CAPTURED_OPERATION_ID" '
      [.items[] |
        select(.metadata.ownerReferences // [] | any(
          .kind == "Job" and .uid == $uid and .controller == true)) |
        select(.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID)] |
      if length == 1 then .[0].metadata.name
      else error("exact result Job does not own exactly one bound Pod") end
    ')
	printf '%s\n' "$result_pods" | jq -e \
		--arg name "$CAPTURED_POD_NAME" '
      .items[] | select(.metadata.name == $name) |
      ([.status.initContainerStatuses // [], .status.containerStatuses // []] | add) as $statuses |
      all($statuses[]; (.restartCount // 0) == 0) and
      ([.status.containerStatuses[] |
        select(.name == "ptah" and .state.terminated.exitCode == 0)] | length) == 1
    ' >/dev/null || fail "$CAPTURED_POD_NAME did not preserve one zero-restart result transport"

	if ! k -n "$TEST_NAMESPACE" logs pod/"$CAPTURED_POD_NAME" -c ptah >"$RESULT_LOG_FILE" 2>&1; then
		fail "could not read the exact result transport from $CAPTURED_POD_NAME"
	fi
	scan_file_for_credentials "$RESULT_LOG_FILE" "the exact $result_operation result transport"
	"$RESULT_ASSERT_BINARY" \
		--logs "$RESULT_LOG_FILE" \
		--operation "$result_operation" \
		--operation-id "$CAPTURED_OPERATION_ID" >"$result_output"
	chmod 600 "$result_output"
	scan_file_for_credentials "$result_output" "the validated $result_operation result"
	jq -e \
		--arg operation "$result_operation" \
		--arg operationID "$CAPTURED_OPERATION_ID" '
      .protocolVersion == 3 and .operation == $operation and
      .operationId == $operationID and .truncation == null
    ' "$result_output" >/dev/null ||
		fail "validated result lost its protocol-v3 binding or complete-output guarantee"
	grep -Fx "$CAPTURED_JOB_UID" "$AUDITED_JOBS_FILE" >/dev/null 2>&1 ||
		printf '%s\n' "$CAPTURED_JOB_UID" >>"$AUDITED_JOBS_FILE"
}

wait_for_controller_status_authorization() {
	expected_answer=$1
	authorization_deadline=$(($(date +%s) + 30))
	while [ "$(date +%s)" -lt "$authorization_deadline" ]; do
		rbac_answer=$(k auth can-i patch ptahschemas.operator.ptah.dev \
			--subresource=status \
			--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${CONTROLLER_NAME}" 2>/dev/null || true)
		[ "$rbac_answer" = "$expected_answer" ] && return 0
		sleep 1
	done
	return 1
}

pause_controller_status_writes() {
	[ "$RBAC_PAUSED" -eq 0 ] || fail "controller status-write RBAC is already paused"
	rbac_role=$(k get clusterrole "$CONTROLLER_NAME" -o json)
	RBAC_RULE_INDEX=$(printf '%s\n' "$rbac_role" | jq -er '
	    [.rules | to_entries[] |
	      select(.value.apiGroups == ["operator.ptah.dev"] and
	        (.value.resources | index("ptahschemas/status")) != null)] |
	    if length == 1 then .[0].key
	    else error("expected exactly one PtahSchema status rule") end
	  ')
	printf '%s\n' "$RBAC_RULE_INDEX" | grep -Eq '^[0-9]+$' ||
		fail "could not identify the controller status-write ClusterRole rule"
	RBAC_ORIGINAL_VERBS=$(printf '%s\n' "$rbac_role" |
		jq -c --argjson index "$RBAC_RULE_INDEX" '.rules[$index].verbs')
	RBAC_STATUS_API_GROUPS=$(printf '%s\n' "$rbac_role" |
		jq -c --argjson index "$RBAC_RULE_INDEX" '.rules[$index].apiGroups')
	RBAC_STATUS_RESOURCES=$(printf '%s\n' "$rbac_role" |
		jq -c --argjson index "$RBAC_RULE_INDEX" '.rules[$index].resources')
	rbac_patch=$(jq -nc \
		--arg index "$RBAC_RULE_INDEX" \
		--argjson apiGroups "$RBAC_STATUS_API_GROUPS" \
		--argjson resources "$RBAC_STATUS_RESOURCES" \
		--argjson verbs "$RBAC_ORIGINAL_VERBS" '
	    [
	      {op: "test", path: ("/rules/" + $index + "/apiGroups"), value: $apiGroups},
	      {op: "test", path: ("/rules/" + $index + "/resources"), value: $resources},
	      {op: "test", path: ("/rules/" + $index + "/verbs"), value: $verbs},
	      {op: "replace", path: ("/rules/" + $index + "/verbs"), value: ["get"]}
	    ]
	  ')
	k patch clusterrole "$CONTROLLER_NAME" --type=json -p "$rbac_patch" >/dev/null
	RBAC_PAUSED=1
	rbac_live_verbs=$(k get clusterrole "$CONTROLLER_NAME" -o json |
		jq -c --argjson index "$RBAC_RULE_INDEX" '.rules[$index].verbs')
	[ "$rbac_live_verbs" = '["get"]' ] ||
		fail "controller status-write RBAC did not enter the stale-approval barrier"
	wait_for_controller_status_authorization no ||
		fail "controller status writes remained authorized during the stale-approval barrier"
}

resume_controller_status_writes() {
	[ "$RBAC_PAUSED" -eq 1 ] || return 0
	rbac_role=$(k get clusterrole "$CONTROLLER_NAME" -o json) || return 1
	resume_rule_index=$(printf '%s\n' "$rbac_role" | jq -er \
		--argjson apiGroups "$RBAC_STATUS_API_GROUPS" \
		--argjson resources "$RBAC_STATUS_RESOURCES" '
	    [.rules | to_entries[] |
	      select(.value.apiGroups == $apiGroups and .value.resources == $resources)] |
	    if length == 1 then .[0].key
	    else error("status rule identity changed while writes were paused") end
	  ') || return 1
	paused_verbs=$(printf '%s\n' "$rbac_role" |
		jq -c --argjson index "$resume_rule_index" '.rules[$index].verbs') || return 1
	[ "$paused_verbs" = '["get"]' ] || return 1
	rbac_patch=$(jq -nc \
		--arg index "$resume_rule_index" \
		--argjson apiGroups "$RBAC_STATUS_API_GROUPS" \
		--argjson resources "$RBAC_STATUS_RESOURCES" \
		--argjson verbs "$RBAC_ORIGINAL_VERBS" '
	    [
	      {op: "test", path: ("/rules/" + $index + "/apiGroups"), value: $apiGroups},
	      {op: "test", path: ("/rules/" + $index + "/resources"), value: $resources},
	      {op: "test", path: ("/rules/" + $index + "/verbs"), value: ["get"]},
	      {op: "replace", path: ("/rules/" + $index + "/verbs"), value: $verbs}
	    ]
	  ')
	if ! k patch clusterrole "$CONTROLLER_NAME" --type=json -p "$rbac_patch" >/dev/null; then
		return 1
	fi
	RBAC_RULE_INDEX=$resume_rule_index
	rbac_live_verbs=$(k get clusterrole "$CONTROLLER_NAME" -o json |
		jq -c --argjson index "$RBAC_RULE_INDEX" '.rules[$index].verbs') || return 1
	[ "$rbac_live_verbs" = "$RBAC_ORIGINAL_VERBS" ] || return 1
	wait_for_controller_status_authorization yes || return 1
	RBAC_PAUSED=0
}

create_registry_service() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$REGISTRY_SERVICE" \
		--arg address "$REGISTRY_IP" '
    {
      apiVersion: "v1",
      kind: "List",
      items: [
        {
          apiVersion: "v1",
          kind: "Service",
          metadata: {namespace: $namespace, name: $name},
          spec: {ports: [{name: "http", port: 5000, protocol: "TCP", targetPort: 5000}]}
        },
        {
          apiVersion: "discovery.k8s.io/v1",
          kind: "EndpointSlice",
          metadata: {
            namespace: $namespace,
            name: ($name + "-docker"),
            labels: {"kubernetes.io/service-name": $name}
          },
          addressType: "IPv4",
          endpoints: [{addresses: [$address]}],
          ports: [{name: "http", port: 5000, protocol: "TCP"}]
        }
      ]
    }' >"$RESOURCE_FILE"
	k apply -f "$RESOURCE_FILE" >/dev/null
}

create_admission_fixtures() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" \
		--arg runtimeTaint "$ADMISSION_RUNTIME_TAINT" '
    {
      apiVersion: "v1", kind: "List", items: [
        {
          apiVersion: "v1", kind: "LimitRange",
          metadata: {namespace: $namespace, name: "ptah-operation-defaults"},
          spec: {limits: [{
            type: "Container",
            defaultRequest: {cpu: "10m", memory: "16Mi"},
            default: {cpu: "100m", memory: "64Mi"}
          }]}
        },
        {
          apiVersion: "node.k8s.io/v1", kind: "RuntimeClass",
          metadata: {name: $runtimeClass},
          handler: "runc",
          overhead: {podFixed: {memory: "8Mi"}},
          scheduling: {
            nodeSelector: {"kubernetes.io/os": "linux"},
            tolerations: [{key: $runtimeTaint, operator: "Exists", effect: "NoSchedule"}]
          }
        }
      ]
    }' >"$RESOURCE_FILE"
	k apply -f "$RESOURCE_FILE" >/dev/null
	service_account_patch=$(jq -cn --arg secret "$REGISTRY_PULL_SECRET" \
		'{imagePullSecrets: [{name: $secret}]}')
	k -n "$TEST_NAMESPACE" patch serviceaccount default --type=merge \
		--patch "$service_account_patch" >/dev/null
}

credential_suffix=$(printf '%s' "$TEST_NAMESPACE" | cksum | awk '{print $1}')
PG_USER=ptah_e2e
PG_DATABASE=ptah_e2e
PG_PASSWORD="e2ePg${credential_suffix}Q7"
PG_SECRET=e2e-postgresql-db
PG_SERVICE=e2e-postgresql
PG_URL="postgres://${PG_USER}:${PG_PASSWORD}@${PG_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5432/${PG_DATABASE}?sslmode=disable"
MYSQL_USER=ptah_e2e
MYSQL_DATABASE=ptah_e2e
MYSQL_PASSWORD="e2eMy${credential_suffix}Q7"
MYSQL_ROOT_PASSWORD="e2eMyRoot${credential_suffix}Q7"
MYSQL_SECRET=e2e-mysql-db
MYSQL_SERVICE=e2e-mysql
MYSQL_URL="mysql://${MYSQL_USER}:${MYSQL_PASSWORD}@tcp(${MYSQL_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:3306)/${MYSQL_DATABASE}"
REGISTRY_AUTH_SECRET=e2e-registry-auth
REGISTRY_PULL_SECRET=e2e-registry-pull
REGISTRY_HOST="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000"

printf '%s' "$PG_PASSWORD" >"$PG_PASSWORD_FILE"
printf '%s' "$PG_URL" >"$PG_URL_FILE"
printf '%s' "$MYSQL_PASSWORD" >"$MYSQL_PASSWORD_FILE"
printf '%s' "$MYSQL_ROOT_PASSWORD" >"$MYSQL_ROOT_PASSWORD_FILE"
printf '%s' "$MYSQL_URL" >"$MYSQL_URL_FILE"
printf '%s' "$REGISTRY_PASSWORD" >"$REGISTRY_PASSWORD_FILE"
printf '%s\n' \
	"$REGISTRY_PASSWORD" \
	"$PG_PASSWORD" "$PG_URL" \
	"$MYSQL_PASSWORD" "$MYSQL_ROOT_PASSWORD" "$MYSQL_URL" \
	>"$CREDENTIAL_PATTERNS_FILE"
chmod 600 \
	"$PG_PASSWORD_FILE" "$PG_URL_FILE" \
	"$MYSQL_PASSWORD_FILE" "$MYSQL_ROOT_PASSWORD_FILE" "$MYSQL_URL_FILE" \
	"$REGISTRY_PASSWORD_FILE" "$CREDENTIAL_PATTERNS_FILE"

create_databases() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg postgresImage "$POSTGRES_IMAGE" \
		--arg pgUser "$PG_USER" \
		--rawfile pgPassword "$PG_PASSWORD_FILE" \
		--arg pgDatabase "$PG_DATABASE" \
		--rawfile pgURL "$PG_URL_FILE" \
		--arg pgSecret "$PG_SECRET" \
		--arg pgService "$PG_SERVICE" \
		--arg mysqlImage "$MYSQL_IMAGE" \
		--arg mysqlUser "$MYSQL_USER" \
		--rawfile mysqlPassword "$MYSQL_PASSWORD_FILE" \
		--rawfile mysqlRootPassword "$MYSQL_ROOT_PASSWORD_FILE" \
		--arg mysqlDatabase "$MYSQL_DATABASE" \
		--rawfile mysqlURL "$MYSQL_URL_FILE" \
		--arg mysqlSecret "$MYSQL_SECRET" \
		--arg mysqlService "$MYSQL_SERVICE" \
		--arg registryUsername "$REGISTRY_USERNAME" \
		--rawfile registryPassword "$REGISTRY_PASSWORD_FILE" \
		--arg registryHost "$REGISTRY_HOST" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
		--arg registryPullSecret "$REGISTRY_PULL_SECRET" '
    def secretEnv($name; $secret; $key):
      {name: $name, valueFrom: {secretKeyRef: {name: $secret, key: $key}}};
    def labels($name): {"app.kubernetes.io/name": $name, "app.kubernetes.io/component": "e2e-database"};
    {
      apiVersion: "v1",
      kind: "List",
      items: [
        {
          apiVersion: "v1", kind: "Secret",
          metadata: {namespace: $namespace, name: $registryAuthSecret},
          type: "Opaque",
          stringData: {
            username: $registryUsername, password: $registryPassword, registry: $registryHost
          }
        },
        {
          apiVersion: "v1", kind: "Secret",
          metadata: {namespace: $namespace, name: $registryPullSecret},
          type: "kubernetes.io/dockerconfigjson",
          stringData: {
            ".dockerconfigjson": ({auths: {
              ($registryHost): {
                username: $registryUsername,
                password: $registryPassword,
                auth: (($registryUsername + ":" + $registryPassword) | @base64)
              }
            }} | tojson)
          }
        },
        {
          apiVersion: "v1", kind: "Secret",
          metadata: {namespace: $namespace, name: $pgSecret},
          type: "Opaque",
          stringData: {username: $pgUser, password: $pgPassword, database: $pgDatabase, url: $pgURL}
        },
        {
          apiVersion: "apps/v1", kind: "Deployment",
          metadata: {namespace: $namespace, name: $pgService, labels: labels($pgService)},
          spec: {
            replicas: 1,
            selector: {matchLabels: labels($pgService)},
            template: {
              metadata: {labels: labels($pgService)},
              spec: {
                automountServiceAccountToken: false,
                imagePullSecrets: [{name: $registryPullSecret}],
                containers: [{
                  name: "postgresql", image: $postgresImage, imagePullPolicy: "IfNotPresent",
                  env: [
                    secretEnv("POSTGRES_USER"; $pgSecret; "username"),
                    secretEnv("POSTGRES_PASSWORD"; $pgSecret; "password"),
                    secretEnv("POSTGRES_DB"; $pgSecret; "database"),
                    {name: "PGDATA", value: "/var/lib/postgresql/data/pgdata"}
                  ],
                  ports: [{name: "postgresql", containerPort: 5432}],
                  readinessProbe: {tcpSocket: {port: "postgresql"}, initialDelaySeconds: 2, periodSeconds: 2},
                  volumeMounts: [{name: "data", mountPath: "/var/lib/postgresql/data"}]
                }],
                volumes: [{name: "data", emptyDir: {}}]
              }
            }
          }
        },
        {
          apiVersion: "v1", kind: "Service",
          metadata: {namespace: $namespace, name: $pgService},
          spec: {selector: labels($pgService), ports: [{name: "postgresql", port: 5432, targetPort: "postgresql"}]}
        },
        {
          apiVersion: "v1", kind: "Secret",
          metadata: {namespace: $namespace, name: $mysqlSecret},
          type: "Opaque",
          stringData: {
            username: $mysqlUser, password: $mysqlPassword, rootPassword: $mysqlRootPassword,
            database: $mysqlDatabase, url: $mysqlURL
          }
        },
        {
          apiVersion: "apps/v1", kind: "Deployment",
          metadata: {namespace: $namespace, name: $mysqlService, labels: labels($mysqlService)},
          spec: {
            replicas: 1,
            selector: {matchLabels: labels($mysqlService)},
            template: {
              metadata: {labels: labels($mysqlService)},
              spec: {
                automountServiceAccountToken: false,
                imagePullSecrets: [{name: $registryPullSecret}],
                containers: [{
                  name: "mysql", image: $mysqlImage, imagePullPolicy: "IfNotPresent",
                  env: [
                    secretEnv("MYSQL_USER"; $mysqlSecret; "username"),
                    secretEnv("MYSQL_PASSWORD"; $mysqlSecret; "password"),
                    secretEnv("MYSQL_ROOT_PASSWORD"; $mysqlSecret; "rootPassword"),
                    secretEnv("MYSQL_DATABASE"; $mysqlSecret; "database")
                  ],
                  ports: [{name: "mysql", containerPort: 3306}],
                  readinessProbe: {tcpSocket: {port: "mysql"}, initialDelaySeconds: 5, periodSeconds: 2},
                  volumeMounts: [{name: "data", mountPath: "/var/lib/mysql"}]
                }],
                volumes: [{name: "data", emptyDir: {}}]
              }
            }
          }
        },
        {
          apiVersion: "v1", kind: "Service",
          metadata: {namespace: $namespace, name: $mysqlService},
          spec: {selector: labels($mysqlService), ports: [{name: "mysql", port: 3306, targetPort: "mysql"}]}
        }
      ]
    }' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k apply -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"
}

wait_for_database() {
	database_engine=$1
	database_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$database_deadline" ]; do
		case "$database_engine" in
		postgresql)
			# shellcheck disable=SC2016 # Variables expand inside the database container.
			database_result=$(k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
				sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "SELECT 1"' \
				2>/dev/null || true)
			;;
		mysql)
			# shellcheck disable=SC2016 # Variables expand inside the database container.
			database_result=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
				sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "SELECT 1"' \
				2>/dev/null || true)
			;;
		*) fail "unsupported database engine $database_engine" ;;
		esac
		database_result=$(printf '%s' "$database_result" | tr -d '[:space:]')
		[ "$database_result" = 1 ] && return 0
		sleep 2
	done
	fail "$database_engine did not become queryable"
}

report_database_versions() {
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	postgres_server_version=$(k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "SHOW server_version"')
	postgres_server_version=$(printf '%s' "$postgres_server_version" | tr -d '\r\n')
	printf '%s\n' "$postgres_server_version" | grep -Eq '^[0-9]+(\.[0-9]+){1,2}$' ||
		fail "PostgreSQL returned an unexpected server_version"
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	mysql_server_version=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "SELECT VERSION()"')
	mysql_server_version=$(printf '%s' "$mysql_server_version" | tr -d '\r\n')
	printf '%s\n' "$mysql_server_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([._+-][0-9A-Za-z.-]+)*$' ||
		fail "MySQL returned an unexpected server version"
	printf 'e2e data plane: PostgreSQL server version %s\n' "$postgres_server_version"
	printf 'e2e data plane: MySQL server version %s\n' "$mysql_server_version"
}

database_column_count() {
	column_engine=$1
	column_name=$2
	case "$column_engine" in
	postgresql)
		column_query="SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='${column_name}'"
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		column_result=$(k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "$1"' \
			sh "$column_query")
		;;
	mysql)
		column_query="SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND column_name='${column_name}'"
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		column_result=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "$1"' \
			sh "$column_query")
		;;
	*) fail "unsupported database engine $column_engine" ;;
	esac
	printf '%s' "$column_result" | tr -d '[:space:]'
}

assert_database_column() {
	column_engine=$1
	column_name=$2
	column_expected=$3
	column_actual=$(database_column_count "$column_engine" "$column_name")
	[ "$column_actual" = "$column_expected" ] ||
		fail "$column_engine column $column_name count is $column_actual, expected $column_expected"
}

assert_mysql_unique_index() {
	index_expected=$1
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	index_actual=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "$1"' \
		sh "SELECT count(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND index_name='e2e_widgets_name_unique' AND non_unique=0")
	index_actual=$(printf '%s' "$index_actual" | tr -d '[:space:]')
	[ "$index_actual" = "$index_expected" ] ||
		fail "MySQL unique index count is $index_actual, expected $index_expected"
}

assert_mysql_plain_index() {
	index_expected=$1
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	index_actual=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "$1"' \
		sh "SELECT count(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND index_name='e2e_widgets_name_idx' AND non_unique=1")
	index_actual=$(printf '%s' "$index_actual" | tr -d '[:space:]')
	[ "$index_actual" = "$index_expected" ] ||
		fail "MySQL plain index count is $index_actual, expected $index_expected"
}

run_mysql_dsn_refusal() {
	unsafe_secret=e2e-mysql-unsafe-dsn
	unsafe_schema_label=e2e-mysql-dsn-negative
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$unsafe_secret" \
		--rawfile url "$MYSQL_URL_FILE" '
      {
        apiVersion: "v1", kind: "Secret",
        metadata: {namespace: $namespace, name: $name},
        type: "Opaque",
        stringData: {
          url: ($url +
            "?multiStatements=true&sql_mode=%27%27%3BDROP%20TABLE%20e2e_widgets")
        }
      }
    ' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k create -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"

	assert_database_column mysql note 1
	assert_database_column mysql enabled 1
	assert_mysql_unique_index 1
	assert_mysql_plain_index 1

	for refusal_operation in observe plan; do
		refusal_name=e2e-mysql-dsn-${refusal_operation}
		refusal_operation_id=e2e-mysql-dsn-${refusal_operation}-operation
		refusal_source=$(k -n "$TEST_NAMESPACE" get jobs \
			-l "operator.ptah.dev/schema=e2e-mysql,operator.ptah.dev/operation=${refusal_operation}" \
			-o json | jq -er '
          [.items[] | select(.status.conditions // [] |
            any(.type == "Complete" and .status == "True"))] |
          sort_by(.metadata.creationTimestamp) |
          if length > 0 then .[-1] else error("no completed source Job") end
        ')
		printf '%s\n' "$refusal_source" | jq \
			--arg namespace "$TEST_NAMESPACE" \
			--arg name "$refusal_name" \
			--arg schema "$unsafe_schema_label" \
			--arg operation "$refusal_operation" \
			--arg operationID "$refusal_operation_id" \
			--arg secret "$unsafe_secret" '
          def rewrite_env:
            .env |= map(
              if .name == "PTAH_DB_URL" then
	                del(.value) |
                .valueFrom = {secretKeyRef: {name: $secret, key: "url"}}
              elif .name == "PTAH_OPERATION_ID" then
                .value = $operationID | del(.valueFrom)
              else . end);
          {
            apiVersion: "batch/v1", kind: "Job",
            metadata: {
              namespace: $namespace,
              name: $name,
              labels: {
                "app.kubernetes.io/component": "e2e-invalid-dsn",
                "operator.ptah.dev/schema": $schema,
                "operator.ptah.dev/operation": $operation
              },
              annotations: {"operator.ptah.dev/operation-id": $operationID}
            },
            spec: (.spec |
              del(.selector, .manualSelector) |
              .template.metadata = {
                labels: {
                  "app.kubernetes.io/component": "e2e-invalid-dsn",
                  "operator.ptah.dev/schema": $schema,
                  "operator.ptah.dev/operation": $operation
                },
                annotations: {"operator.ptah.dev/operation-id": $operationID}
              } |
              .template.spec.containers |= map(rewrite_env) |
              .template.spec.initContainers |= map(rewrite_env))
          }
        ' >"$RESOURCE_FILE"
		k create -f "$RESOURCE_FILE" >/dev/null
		k -n "$TEST_NAMESPACE" wait --for=condition=Complete \
			--timeout="${TIMEOUT_SECONDS}s" job/"$refusal_name" >/dev/null
		refusal_job=$(k -n "$TEST_NAMESPACE" get job "$refusal_name" -o json)
		refusal_job_uid=$(printf '%s\n' "$refusal_job" | jq -er '.metadata.uid')
		refusal_pods=$(k -n "$TEST_NAMESPACE" get pods -l "job-name=${refusal_name}" -o json)
		refusal_pod=$(printf '%s\n' "$refusal_pods" | jq -er \
			--arg uid "$refusal_job_uid" '
              [.items[] | select(.metadata.ownerReferences // [] | any(
                .kind == "Job" and .uid == $uid and .controller == true))] |
              if length == 1 then .[0].metadata.name
              else error("invalid-DSN Job does not own one Pod") end
            ')
		if ! k -n "$TEST_NAMESPACE" logs pod/"$refusal_pod" -c ptah >"$RESULT_LOG_FILE" 2>&1; then
			fail "could not read $refusal_operation invalid-DSN runner result"
		fi
		scan_file_for_credentials "$RESULT_LOG_FILE" \
			"the $refusal_operation invalid-DSN runner transport"
		if grep -Ei 'DROP[[:space:]]+TABLE|%3B|side_effecting_function' \
			"$RESULT_LOG_FILE" >/dev/null; then
			fail "$refusal_operation invalid-DSN result disclosed the encoded server-session payload"
		fi
		refusal_result=$WORK_DIR/${refusal_name}-result.json
		"$RESULT_ASSERT_BINARY" --logs "$RESULT_LOG_FILE" \
			--operation "$refusal_operation" --operation-id "$refusal_operation_id" \
			>"$refusal_result"
		chmod 600 "$refusal_result"
		jq -e \
			--arg operation "$refusal_operation" \
			--arg operationID "$refusal_operation_id" '
              .protocolVersion == 3 and .operation == $operation and
              .operationId == $operationID and .error.code == "invalid_target" and
              .stdout == "" and (.planContentDigest // "") == "" and
              (.planOutcome // "") == "" and
              (.mutationStarted // false) == false and
              (.uncertain // false) == false and .truncation == null
            ' "$refusal_result" >/dev/null ||
			fail "$refusal_operation invalid-DSN Job dispatched executor work"
	done

	assert_database_column mysql note 1
	assert_database_column mysql enabled 1
	assert_mysql_unique_index 1
	assert_mysql_plain_index 1
	audit_runtime_credentials
}

publish_schema() {
	publish_engine=$1
	publish_revision=$2
	publish_dialect=$3
	publish_reference=$4
	publish_file="$ROOT_DIR/testdata/e2e/${publish_engine}-${publish_revision}.sql"
	publish_configmap="e2e-${publish_engine}-${publish_revision}"
	publish_job="e2e-push-${publish_engine}-${publish_revision}"
	[ -f "$publish_file" ] || fail "schema fixture is missing: $publish_file"

	printf 'e2e data plane: publishing %s %s\n' "$publish_engine" "$publish_revision" >&2
	k -n "$TEST_NAMESPACE" create configmap "$publish_configmap" \
		--from-file="schema.sql=${publish_file}" >/dev/null
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$publish_job" \
		--arg image "$EXECUTOR_IMAGE" \
		--arg configMap "$publish_configmap" \
		--arg reference "$publish_reference" \
		--arg dialect "$publish_dialect" \
		--arg version "$publish_revision" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
		--arg registryPullSecret "$REGISTRY_PULL_SECRET" '
	    def registrySecretEnv($name; $key):
	      {name: $name, valueFrom: {secretKeyRef: {name: $registryAuthSecret, key: $key}}};
    {
      apiVersion: "batch/v1", kind: "Job",
      metadata: {
        namespace: $namespace, name: $name,
        labels: {"app.kubernetes.io/component": "e2e-schema-publisher"}
      },
      spec: {
        backoffLimit: 0, activeDeadlineSeconds: 300,
        template: {
          metadata: {labels: {"app.kubernetes.io/component": "e2e-schema-publisher"}},
          spec: {
            restartPolicy: "Never", automountServiceAccountToken: false,
            imagePullSecrets: [{name: $registryPullSecret}],
            securityContext: {
              runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
              seccompProfile: {type: "RuntimeDefault"}
            },
            containers: [{
              name: "publisher", image: $image, imagePullPolicy: "IfNotPresent",
              command: ["/usr/local/bin/ptah"],
              args: [
                "schema", "push", $reference, "--schema-file", "/schema/schema.sql",
                "--dialect", $dialect, "--version", $version, "--plain-http"
              ],
              env: [
                {name: "HOME", value: "/work"},
                {name: "TMPDIR", value: "/work"},
                registrySecretEnv("PTAH_OCI_USERNAME"; "username"),
                registrySecretEnv("PTAH_OCI_PASSWORD"; "password"),
                registrySecretEnv("PTAH_OCI_REGISTRY"; "registry")
              ],
              securityContext: {
                allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                capabilities: {drop: ["ALL"]}
              },
              volumeMounts: [
                {name: "schema", mountPath: "/schema", readOnly: true},
                {name: "work", mountPath: "/work"}
              ]
            }],
            volumes: [
              {name: "schema", configMap: {name: $configMap}},
              {name: "work", emptyDir: {sizeLimit: "64Mi"}}
            ]
          }
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	wait_for_job "$publish_job"
	k -n "$TEST_NAMESPACE" get job "$publish_job" -o json |
		jq -e \
			--arg image "$EXECUTOR_IMAGE" \
			--arg registrySecret "$REGISTRY_AUTH_SECRET" \
			-f "$ROOT_DIR/testdata/e2e/publisher-job-isolation.jq" >/dev/null ||
		fail "publisher Job did not preserve the no-database-credential boundary"
	k -n "$TEST_NAMESPACE" logs job/"$publish_job" >"$LOG_FILE"
	publish_digest=$(sed -n 's/^Digest: \(sha256:[0-9a-f]\{64\}\)$/\1/p' "$LOG_FILE" | tail -n 1)
	printf '%s\n' "$publish_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || {
		fail "could not read the published schema digest from Job $publish_job"
	}
	printf '%s\n' "$publish_digest"
}

create_schema_resource() {
	resource_schema=$1
	resource_engine=$2
	resource_secret=$3
	resource_reference=$4
	resource_coordination_key=$5
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$resource_schema" \
		--arg engine "$resource_engine" \
		--arg secret "$resource_secret" \
		--arg reference "$resource_reference" \
		--arg coordinationKey "$resource_coordination_key" \
		--arg policy e2e-verification-policy \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
		--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" \
		--arg interval "$APPROVAL_INTERVAL" '
    {
      apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        target: {
          engine: $engine,
          coordinationKey: $coordinationKey,
          urlFrom: {name: $secret, key: "url"}
        },
        desired: {
          ociRef: $reference,
          registryAuthFrom: {
            name: $registryAuthSecret,
            mode: "Environment",
            usernameKey: "username",
            passwordKey: "password",
            registryKey: "registry"
          },
          verificationPolicyFrom: {name: $policy, key: "policy.yaml"},
          transport: {plainHTTP: true}
        },
        policy: {
          apply: "OnApproval", allowDestructive: false, driftSeverity: "all",
          lockTimeout: "30s", transactionMode: "file"
        },
        interval: $interval,
        execution: {
	          activeDeadlineSeconds: 300, failureRetryInterval: "5s", connectTimeout: "30s",
	          runtimeClassName: $runtimeClass
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

CURRENT_PLAN=
CURRENT_PLAN_UID=
CURRENT_PLAN_FINGERPRINT=
capture_current_plan() {
	plan_schema=$1
	CURRENT_PLAN=$(k -n "$TEST_NAMESPACE" get ptahschema "$plan_schema" -o jsonpath='{.status.plan.name}')
	[ -n "$CURRENT_PLAN" ] || fail "$plan_schema has no current plan"
	CURRENT_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$CURRENT_PLAN" -o jsonpath='{.metadata.uid}')
	CURRENT_PLAN_FINGERPRINT=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$CURRENT_PLAN" -o jsonpath='{.spec.fingerprint}')
	printf '%s\n' "$CURRENT_PLAN_FINGERPRINT" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "$CURRENT_PLAN does not have a SHA-256 plan fingerprint"
}

assert_plan() {
	plan_schema=$1
	plan_reference=$2
	plan_digest=$3
	plan_dialect=$4
	plan_destructive=$5
	plan_observe_checkpoint=$6
	plan_job_checkpoint=$7
	plan_after_checkpoint=${8:-}
	wait_for_schema "$plan_schema" \
		".status.source.digest == \"$plan_digest\" and .status.plan.name != null and .status.nextReconciliationTime != null and (.status.phase == \"AwaitingApproval\" or .status.phase == \"Blocked\")" \
		"verified digest $plan_digest and a published plan"
	if [ -n "$plan_after_checkpoint" ]; then
		pause_controller_status_writes
		checkpoint_schema_jobs "$plan_schema" "$plan_after_checkpoint"
	fi
	k -n "$TEST_NAMESPACE" get ptahschema "$plan_schema" -o json |
		jq -e \
			--arg reference "$plan_reference" \
			--arg digest "$plan_digest" \
			--arg type application/vnd.stokaro.ptah.schema.v1 '
      .status.source.requestedReference == $reference and
      .status.source.digest == $digest and
      .status.source.resolvedReference == ($reference | sub(":[^/:]+$"; "@" + $digest)) and
      .status.source.verified == true and
      .status.source.artifactType == $type and
      .status.source.verificationPolicyDigest != "" and
		.status.target.identityDigest != "" and
		.status.target.driftReportDigest != ""
    ' >/dev/null || fail "$plan_schema did not retain resolve, verification, and observation evidence"
	drift_report_digest=$(k -n "$TEST_NAMESPACE" get ptahschema "$plan_schema" \
		-o jsonpath='{.status.target.driftReportDigest}')
	printf '%s\n' "$drift_report_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "$plan_schema did not retain a SHA-256 drift-report digest"
	capture_current_plan "$plan_schema"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$CURRENT_PLAN" -o json |
		jq -e \
			--arg schema "$plan_schema" \
			--arg digest "$plan_digest" \
			--arg dialect "$plan_dialect" \
			--argjson destructive "$plan_destructive" '
      .spec.schemaRef.name == $schema and
      .spec.artifactDigest == $digest and
      .spec.dialect == $dialect and
	  (.spec.actualStateFingerprint | test("^sha256:[0-9a-f]{64}$")) and
      .spec.destructive == $destructive and
      .spec.statementCount > 0 and
      (.spec.chunks | length) > 0 and
      (.status.conditions | any(.type == "Ready" and .status == "True"))
    ' >/dev/null ||
		fail "$CURRENT_PLAN is not a committed content-addressed native plan"

	observe_result_file="$WORK_DIR/${plan_schema}-changed-observe-result.json"
	plan_result_file="$WORK_DIR/${plan_schema}-changed-plan-result.json"
	plan_document_file="$WORK_DIR/${plan_schema}-changed-plan.json"
	capture_one_new_job_result "$plan_schema" observe "$plan_observe_checkpoint" \
		"$observe_result_file" "$plan_after_checkpoint"
	capture_one_new_job_result "$plan_schema" plan "$plan_job_checkpoint" \
		"$plan_result_file" "$plan_after_checkpoint"

	plan_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$plan_schema" -o json)
	printf '%s\n' "$plan_schema_object" | jq -e \
		--slurpfile result "$observe_result_file" \
		--arg dialect "$plan_dialect" '
      $result[0] as $result |
      $result.error == null and $result.stdout == "" and
      ($result.childExitCode == 0 or $result.childExitCode == 1) and
      $result.coordinationDigest == .status.target.coordinationDigest and
      $result.targetIdentityDigest == .status.target.identityDigest and
      $result.driftReportDigest == .status.target.driftReportDigest and
      $result.observedDrift == true and $result.driftFindingCount > 0 and
      ($result.highestDriftSeverity |
        IN("safe", "info", "warning", "error", "destructive")) and
      (($dialect == "postgres" and ($result.observedDialect | IN("postgres", "postgresql"))) or
       ($dialect == "mysql" and ($result.observedDialect | IN("mysql", "mariadb"))))
    ' >/dev/null || fail "$plan_schema Observe result is not bound to its persisted drift evidence"
	printf '%s\n' "$plan_schema_object" | jq -e \
		--slurpfile result "$plan_result_file" '
      $result[0] as $result |
      $result.error == null and $result.childExitCode == 0 and
      $result.planOutcome == "Changes" and ($result.stdout | length) > 0 and
      $result.planContentDigest == .status.plan.contentDigest and
      $result.coordinationDigest == .status.plan.coordinationDigest and
      $result.targetIdentityDigest == .status.plan.targetIdentityDigest
    ' >/dev/null || fail "$plan_schema Plan result is not bound to its published immutable plan"
	jq -jr '.stdout' "$plan_result_file" >"$plan_document_file"
	chmod 600 "$plan_document_file"
	scan_file_for_credentials "$plan_document_file" "$plan_schema native plan document"
	plan_document_digest="sha256:$(sha256 <"$plan_document_file")"
	[ "$plan_document_digest" = "$(jq -er '.planContentDigest' "$plan_result_file")" ] ||
		fail "$plan_schema Plan result content digest does not cover the exact stdout bytes"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$CURRENT_PLAN" -o json | jq -e \
		--slurpfile document "$plan_document_file" \
		--arg contentDigest "$plan_document_digest" '
      $document[0] as $document |
      .spec.contentDigest == $contentDigest and
      .spec.actualStateFingerprint == $document.from_fingerprint and
      .spec.desiredStateFingerprint == $document.to_fingerprint and
      .spec.dialect == $document.dialect and
	  ($document.destructive != true or .spec.destructive == true) and
      .spec.statementCount == ($document.statements | length)
    ' >/dev/null || fail "$CURRENT_PLAN is not bound to the exact native Plan result"
}

assert_convergence_result_pair() {
	converged_schema=$1
	converged_observe_checkpoint=$2
	converged_plan_checkpoint=$3
	converged_after_checkpoint=${4:-}
	converged_observe_result="$WORK_DIR/${converged_schema}-converged-observe-result.json"
	converged_plan_result="$WORK_DIR/${converged_schema}-converged-plan-result.json"
	capture_one_new_job_result "$converged_schema" observe "$converged_observe_checkpoint" \
		"$converged_observe_result" "$converged_after_checkpoint"
	capture_one_new_job_result "$converged_schema" plan "$converged_plan_checkpoint" \
		"$converged_plan_result" "$converged_after_checkpoint"
	converged_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$converged_schema" -o json)
	printf '%s\n' "$converged_schema_object" | jq -e \
		--slurpfile observe "$converged_observe_result" \
		--slurpfile plan "$converged_plan_result" '
      $observe[0] as $observe | $plan[0] as $plan |
      $observe.error == null and $observe.childExitCode == 0 and
      $observe.stdout == "" and ($observe.observedDrift // false) == false and
      ($observe.highestDriftSeverity // "") == "" and
      ($observe.driftFindingCount // 0) == 0 and
      $observe.coordinationDigest == .status.target.coordinationDigest and
      $observe.targetIdentityDigest == .status.target.identityDigest and
      $observe.driftReportDigest == .status.target.driftReportDigest and
      $plan.error == null and $plan.childExitCode == 0 and
      $plan.planOutcome == "NoChanges" and $plan.stdout == "" and
      ($plan.planContentDigest // "") == "" and
      $plan.coordinationDigest == .status.target.coordinationDigest and
      $plan.targetIdentityDigest == .status.target.identityDigest and
	  .status.plan == null and .status.pendingObservation == null and
	  .status.activeOperation == null and .status.pendingLockRelease == null and
      (.status.conditions | any(
        .type == "InSync" and .status == "True" and .reason == "ScopedConverged"))
    ' >/dev/null || fail "$converged_schema did not prove convergence with exact Observe and NoChanges Plan results"
}

create_exact_approval() {
	approval_schema=$1
	approval_plan=$2
	approval_name=$3
	approval_coordination_key=$4
	approval_coordination_digest=$5
	approval_schema_uid=$(k -n "$TEST_NAMESPACE" get ptahschema "$approval_schema" -o jsonpath='{.metadata.uid}')
	approval_plan_uid=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$approval_plan" -o jsonpath='{.metadata.uid}')
	approval_fingerprint=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$approval_plan" -o jsonpath='{.spec.fingerprint}')
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$approval_name" \
		--arg schema "$approval_schema" \
		--arg schemaUID "$approval_schema_uid" \
		--arg plan "$approval_plan" \
		--arg planUID "$approval_plan_uid" \
		--arg fingerprint "$approval_fingerprint" '
    {
      apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchemaApproval",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        schemaRef: {name: $schema, uid: $schemaUID},
        planRef: {name: $plan, uid: $planUID},
        planFingerprint: $fingerprint
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	k -n "$TEST_NAMESPACE" get ptahschemaapproval "$approval_name" -o json |
		jq -e \
			--arg schema "$approval_schema" \
			--arg plan "$approval_plan" \
			--arg fingerprint "$approval_fingerprint" \
			--arg coordinationKey "$approval_coordination_key" \
			--arg coordinationDigest "$approval_coordination_digest" '
      .spec.schemaRef.name == $schema and .spec.planRef.name == $plan and
      .spec.planFingerprint == $fingerprint and .spec.artifactDigest != "" and
      .spec.coordinationDigest == $coordinationDigest and
      .spec.targetIdentityDigest != "" and
      .spec.actualStateFingerprint != "" and
      .spec.desiredStateFingerprint != "" and .spec.policyFingerprint != "" and
      .spec.verificationPolicyDigest != "" and .spec.ptahVersion != "" and
      (.spec.executorImage | test("@sha256:[0-9a-f]{64}$")) and
      (.spec.runnerImage | test("@sha256:[0-9a-f]{64}$")) and
      .spec.runnerProtocolVersion == 3 and .spec.approver.username != "" and
      .spec.approvedAt != null and .spec.mutationRequestUID != "" and
      ([.spec | .. | scalars | select(. == $coordinationKey)] | length == 0)
    ' >/dev/null || fail "$approval_name was not hydrated and bound to the exact plan"
}

assert_job_isolation() {
	isolation_schema=$1
	isolation_secret=$2
	isolation_require_apply=$3
	k -n "$TEST_NAMESPACE" get jobs -l "operator.ptah.dev/schema=${isolation_schema}" -o json |
		jq -e \
			--arg databaseSecret "$isolation_secret" \
			--arg registrySecret "$REGISTRY_AUTH_SECRET" \
			--argjson requireApply "$isolation_require_apply" \
			-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" >/dev/null ||
		fail "$isolation_schema Jobs did not isolate authenticated registry access from database credentials"
}

assert_coordination_boundary() {
	coordination_schema=$1
	coordination_key=$2
	coordination_expected_digest=$3
	k -n "$TEST_NAMESPACE" get ptahschema "$coordination_schema" -o json |
		jq -e \
			--arg coordinationKey "$coordination_key" \
			--arg coordinationDigest "$coordination_expected_digest" '
      .spec.target.coordinationKey == $coordinationKey and
      .status.target.coordinationDigest == $coordinationDigest and
      .status.plan.coordinationDigest == $coordinationDigest and
      ([.status | .. | scalars | select(. == $coordinationKey)] | length == 0)
    ' >/dev/null || fail "$coordination_schema did not preserve the key-free coordination boundary"
	coordination_plan=$(k -n "$TEST_NAMESPACE" get ptahschema "$coordination_schema" \
		-o jsonpath='{.status.plan.name}')
	[ -n "$coordination_plan" ] || fail "$coordination_schema has no plan for coordination checks"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$coordination_plan" -o json |
		jq -e \
			--arg coordinationKey "$coordination_key" \
			--arg coordinationDigest "$coordination_expected_digest" '
      .spec.coordinationDigest == $coordinationDigest and
      ([. | .. | scalars | select(. == $coordinationKey)] | length == 0)
    ' >/dev/null || fail "$coordination_plan did not preserve the exact key-free coordination digest"
}

checkpoint_coordination_leases() {
	coordination_checkpoint=$1
	k -n "$OPERATOR_NAMESPACE" get leases \
		-l 'app.kubernetes.io/managed-by=ptah-operator,operator.ptah.dev/coordination=database-target' \
		-o json | jq '[.items[].metadata.uid] | sort' >"$coordination_checkpoint"
}

assert_coordination_lease_boundary() {
	coordination_key=$1
	coordination_checkpoint=$2
	k -n "$OPERATOR_NAMESPACE" get leases \
		-l 'app.kubernetes.io/managed-by=ptah-operator,operator.ptah.dev/coordination=database-target' \
		-o json |
		jq -e \
			--arg coordinationKey "$coordination_key" \
			--slurpfile before "$coordination_checkpoint" '
      [.items[] | .metadata.uid as $uid |
        select(($before[0] | index($uid)) == null)] as $created |
      ($created | length) == 1 and
      ([$created[] | .. | scalars | select(. == $coordinationKey)] | length == 0) and
      ([.items[] | .. | scalars | select(. == $coordinationKey)] | length == 0)
    ' >/dev/null || fail "target Lease did not preserve one exact key-free coordination realm"
}

wait_for_in_sync() {
	sync_schema=$1
	sync_digest=$2
	sync_observe_checkpoint=$3
	sync_plan_checkpoint=$4
	wait_for_schema "$sync_schema" \
		".status.phase == \"InSync\" and .status.source.digest == \"$sync_digest\" and .status.applied.artifactDigest == \"$sync_digest\" and .status.pendingObservation == null and .status.activeOperation == null and .status.pendingLockRelease == null and (.status.conditions | any(.type == \"InSync\" and .status == \"True\" and .reason == \"ScopedConverged\"))" \
		"post-apply convergence for $sync_digest"
	assert_convergence_result_pair "$sync_schema" "$sync_observe_checkpoint" \
		"$sync_plan_checkpoint"
}

assert_periodic_noop() {
	noop_schema=$1
	noop_checkpoint=$2
	[ "$RBAC_PAUSED" -eq 0 ] || fail "periodic no-op proof requires active controller status writes"
	[ -s "$noop_checkpoint" ] || fail "$noop_schema periodic no-op proof lacks a quiescent checkpoint"
	wait_for_one_new_job "$noop_schema" resolve "$noop_checkpoint"
	wait_for_one_new_job "$noop_schema" verify "$noop_checkpoint"
	wait_for_one_new_job "$noop_schema" observe "$noop_checkpoint"
	wait_for_one_new_job "$noop_schema" plan "$noop_checkpoint"
	wait_for_schema "$noop_schema" '.status.phase == "InSync" and .status.activeOperation == null' \
		"a completed periodic no-op observation"
	pause_controller_status_writes
	noop_after_checkpoint="$WORK_DIR/${noop_schema}-noop-after.json"
	checkpoint_schema_jobs "$noop_schema" "$noop_after_checkpoint"
	for noop_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$noop_schema" "$noop_operation" \
			"$noop_checkpoint" "$noop_after_checkpoint"
	done
	assert_no_job_between_checkpoints "$noop_schema" apply \
		"$noop_checkpoint" "$noop_after_checkpoint"
	assert_convergence_result_pair "$noop_schema" "$noop_checkpoint" \
		"$noop_checkpoint" "$noop_after_checkpoint"
	audit_runtime_credentials
}

set_reconcile_interval_and_assert_noop() {
	interval_schema=$1
	interval_digest=$2
	interval_value=$3
	interval_keep_paused=${4:-false}
	case "$interval_keep_paused" in
	true | false) ;;
	*) fail "interval keep-paused flag must be true or false" ;;
	esac
	previous_interval=$(k -n "$TEST_NAMESPACE" get ptahschema "$interval_schema" \
		-o jsonpath='{.spec.interval}')
	[ "$previous_interval" != "$interval_value" ] ||
		fail "$interval_schema interval transition requires a distinct value"
	if [ "$RBAC_PAUSED" -eq 0 ]; then
		pause_controller_status_writes
	fi
	interval_checkpoint="$WORK_DIR/${interval_schema}-interval-before.json"
	checkpoint_schema_jobs "$interval_schema" "$interval_checkpoint"
	interval_patch=$(jq -nc --arg interval "$interval_value" \
		'{spec: {interval: $interval}}')
	k -n "$TEST_NAMESPACE" patch ptahschema "$interval_schema" --type=merge \
		-p "$interval_patch" >/dev/null
	resume_controller_status_writes || fail "could not restore controller status-write RBAC"
	wait_for_one_new_job "$interval_schema" resolve "$interval_checkpoint"
	wait_for_one_new_job "$interval_schema" verify "$interval_checkpoint"
	wait_for_one_new_job "$interval_schema" observe "$interval_checkpoint"
	wait_for_one_new_job "$interval_schema" plan "$interval_checkpoint"
	wait_for_schema "$interval_schema" \
		".status.observedGeneration == .metadata.generation and .status.phase == \"InSync\" and .status.source.digest == \"$interval_digest\" and .status.activeOperation == null" \
		"one generation-triggered no-op cycle after selecting interval $interval_value"
	actual_interval=$(k -n "$TEST_NAMESPACE" get ptahschema "$interval_schema" \
		-o jsonpath='{.spec.interval}')
	[ "$actual_interval" = "$interval_value" ] ||
		fail "$interval_schema did not retain reconciliation interval $interval_value"
	pause_controller_status_writes
	interval_after_checkpoint="$WORK_DIR/${interval_schema}-interval-after.json"
	checkpoint_schema_jobs "$interval_schema" "$interval_after_checkpoint"
	for interval_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$interval_schema" "$interval_operation" \
			"$interval_checkpoint" "$interval_after_checkpoint"
	done
	assert_no_job_between_checkpoints "$interval_schema" apply \
		"$interval_checkpoint" "$interval_after_checkpoint"
	assert_convergence_result_pair "$interval_schema" "$interval_checkpoint" \
		"$interval_checkpoint" "$interval_after_checkpoint"
	audit_runtime_credentials
	if [ "$interval_keep_paused" = false ]; then
		PERIODIC_NOOP_CHECKPOINT="$WORK_DIR/${interval_schema}-periodic-noop-before.json"
		checkpoint_schema_jobs "$interval_schema" "$PERIODIC_NOOP_CHECKPOINT"
		resume_controller_status_writes || fail "could not restore controller status-write RBAC"
	fi
}

suspend_schema_for_tag_move() {
	move_schema=$1
	move_interval=$2
	move_writes_paused=$RBAC_PAUSED
	move_patch=$(jq -nc --arg interval "$move_interval" \
		'{spec: {suspend: true, interval: $interval}}')
	k -n "$TEST_NAMESPACE" patch ptahschema "$move_schema" --type=merge \
		-p "$move_patch" >/dev/null
	if [ "$move_writes_paused" -eq 1 ]; then
		resume_controller_status_writes || fail "could not restore controller status-write RBAC"
	fi
	wait_for_schema "$move_schema" '
      .status.observedGeneration == .metadata.generation and
      .status.phase == "Suspended" and .status.activeOperation == null and
      .status.pendingObservation == null and .status.pendingLockRelease == null
    ' "a quiescent suspension before moving the mutable tag"
	actual_move_interval=$(k -n "$TEST_NAMESPACE" get ptahschema "$move_schema" \
		-o jsonpath='{.spec.interval}')
	[ "$actual_move_interval" = "$move_interval" ] ||
		fail "$move_schema did not retain tag-move interval $move_interval"
}

resume_schema_after_tag_move() {
	move_schema=$1
	k -n "$TEST_NAMESPACE" patch ptahschema "$move_schema" --type=merge \
		-p '{"spec":{"suspend":false}}' >/dev/null
}

prepare_blocked_refresh_cadence() {
	blocked_schema=$1
	suspend_schema_for_tag_move "$blocked_schema" "$BLOCKED_REFRESH_INTERVAL"
	blocked_generation_checkpoint="$WORK_DIR/${blocked_schema}-blocked-generation-before.json"
	checkpoint_schema_jobs "$blocked_schema" "$blocked_generation_checkpoint"
	resume_schema_after_tag_move "$blocked_schema"
	for blocked_operation in resolve verify observe plan; do
		wait_for_one_new_job "$blocked_schema" "$blocked_operation" \
			"$blocked_generation_checkpoint"
	done
	wait_for_schema "$blocked_schema" '
      .status.observedGeneration == .metadata.generation and
      .status.phase == "Blocked" and .status.activeOperation == null and
      .status.pendingObservation == null and .status.pendingLockRelease == null and
      .status.nextReconciliationTime != null
    ' "the generation-triggered blocked refresh before scheduled cadence proof"
	BLOCKED_GATE_CHECKPOINT="$WORK_DIR/${blocked_schema}-blocked-generation-after.json"
	checkpoint_schema_jobs "$blocked_schema" "$BLOCKED_GATE_CHECKPOINT"
	for blocked_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$blocked_schema" "$blocked_operation" \
			"$blocked_generation_checkpoint" "$BLOCKED_GATE_CHECKPOINT"
	done
	assert_no_job_between_checkpoints "$blocked_schema" apply \
		"$blocked_generation_checkpoint" "$BLOCKED_GATE_CHECKPOINT"
	audit_runtime_credentials
}

assert_destructive_gate() {
	gate_schema=$1
	gate_apply_checkpoint=$2
	gate_plan_name=$3
	gate_plan_uid=$4
	gate_plan_fingerprint=$5
	gate_source_digest=$6
	gate_refresh_checkpoint=$7
	gate_interval=$(k -n "$TEST_NAMESPACE" get ptahschema "$gate_schema" \
		-o jsonpath='{.spec.interval}')
	[ "$gate_interval" = "$BLOCKED_REFRESH_INTERVAL" ] ||
		fail "$gate_schema destructive gate requires interval $BLOCKED_REFRESH_INTERVAL"
	if [ -z "$gate_plan_name" ] || [ -z "$gate_plan_uid" ] ||
		[ -z "$gate_plan_fingerprint" ] || [ -z "$gate_source_digest" ]; then
		fail "$gate_schema destructive gate lacks its immutable starting evidence"
	fi
	[ -s "$gate_refresh_checkpoint" ] ||
		fail "$gate_schema destructive gate lacks its atomic refresh checkpoint"
	gate_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$gate_deadline" ]; do
		audit_completed_jobs
		assert_no_new_jobs "$gate_schema" apply "$gate_apply_checkpoint"
		gate_resolve_count=$(new_job_count_since "$gate_schema" resolve "$gate_refresh_checkpoint")
		gate_verify_count=$(new_job_count_since "$gate_schema" verify "$gate_refresh_checkpoint")
		gate_observe_count=$(new_job_count_since "$gate_schema" observe "$gate_refresh_checkpoint")
		gate_plan_count=$(new_job_count_since "$gate_schema" plan "$gate_refresh_checkpoint")
		if [ "$gate_resolve_count" -ge 3 ] && [ "$gate_verify_count" -ge 3 ] && \
			[ "$gate_observe_count" -ge 3 ] && [ "$gate_plan_count" -ge 3 ] && \
			all_new_jobs_complete "$gate_schema" resolve "$gate_refresh_checkpoint" 3 && \
			all_new_jobs_complete "$gate_schema" verify "$gate_refresh_checkpoint" 3 && \
			all_new_jobs_complete "$gate_schema" observe "$gate_refresh_checkpoint" 3 && \
			all_new_jobs_complete "$gate_schema" plan "$gate_refresh_checkpoint" 3; then
			record_observed_jobs
			jq -e -s \
				--slurpfile before "$gate_refresh_checkpoint" \
				--arg schema "$gate_schema" \
				--argjson intervalSeconds "$BLOCKED_REFRESH_SECONDS" '
              def new_since($before):
                .uid as $uid | ($before[0] | index($uid)) == null;
              def operation_rank:
                if .operation == "resolve" then 0
                elif .operation == "verify" then 1
                elif .operation == "observe" then 2
                else 3 end;
              [.[] |
                select(.schema == $schema) |
                select(.operation == "resolve" or .operation == "verify" or
                  .operation == "observe" or .operation == "plan") |
                select(new_since($before))
              ] | unique_by(.uid) | sort_by([.created, operation_rank]) as $new |
              [$new[] | select(.operation == "resolve")] as $resolves |
              ($resolves | length) >= 3 and
              (($resolves[1].created | fromdateiso8601) -
                ($resolves[0].created | fromdateiso8601) >= $intervalSeconds) and
              (($resolves[2].created | fromdateiso8601) -
                ($resolves[1].created | fromdateiso8601) >= $intervalSeconds) and
              (($resolves[0].created) as $firstResolve |
                ([$new[] | select(.created >= $firstResolve)][0:12] |
                  map(.operation)) ==
                ["resolve", "verify", "observe", "plan",
                 "resolve", "verify", "observe", "plan",
                 "resolve", "verify", "observe", "plan"])
            ' "$OBSERVED_JOBS_FILE" >/dev/null ||
				fail "$gate_schema did not preserve ordered interval-spaced blocked refresh cycles"
			wait_for_schema "$gate_schema" '
          .status.phase == "Blocked" and .status.activeOperation == null and
          .status.pendingObservation == null and .status.pendingLockRelease == null and
          .status.nextReconciliationTime != null and
          (.status.conditions | any(
            .type == "ApprovalRequired" and .status == "False" and
            .reason == "DestructiveChangesDisabled"))
        ' "three complete scheduled blocked refresh cycles"
			k -n "$TEST_NAMESPACE" get ptahschema "$gate_schema" -o json |
				jq -e \
					--arg plan "$gate_plan_name" \
					--arg planUID "$gate_plan_uid" \
					--arg fingerprint "$gate_plan_fingerprint" \
					--arg digest "$gate_source_digest" '
              .status.phase == "Blocked" and .status.source.digest == $digest and
              .status.plan.name == $plan and .status.plan.uid == $planUID and
              .status.plan.fingerprint == $fingerprint and
              .status.plan.destructive == true
            ' >/dev/null || fail "$gate_schema blocked refresh changed its current plan evidence"
			k -n "$TEST_NAMESPACE" get ptahschemaplan "$gate_plan_name" -o json |
				jq -e \
					--arg uid "$gate_plan_uid" \
					--arg fingerprint "$gate_plan_fingerprint" \
					--arg digest "$gate_source_digest" '
              .metadata.uid == $uid and .spec.fingerprint == $fingerprint and
              .spec.artifactDigest == $digest and .spec.destructive == true
            ' >/dev/null || fail "$gate_schema immutable destructive plan changed during refresh"
			assert_no_new_jobs "$gate_schema" apply "$gate_apply_checkpoint"
			return 0
		fi
		sleep 2
	done
	fail "$gate_schema did not complete three scheduled blocked refresh cycles"
}

restore_blocked_refresh_cadence() {
	restore_schema=$1
	restore_plan_name=$2
	restore_plan_uid=$3
	restore_plan_fingerprint=$4
	restore_source_digest=$5
	suspend_schema_for_tag_move "$restore_schema" "$QUIESCENT_INTERVAL"
	restore_before_checkpoint="$WORK_DIR/${restore_schema}-blocked-restore-before.json"
	checkpoint_schema_jobs "$restore_schema" "$restore_before_checkpoint"
	resume_schema_after_tag_move "$restore_schema"
	for restore_operation in resolve verify observe plan; do
		wait_for_one_new_job "$restore_schema" "$restore_operation" \
			"$restore_before_checkpoint"
	done
	wait_for_schema "$restore_schema" '
      .status.observedGeneration == .metadata.generation and
      .status.phase == "Blocked" and .status.activeOperation == null and
      .status.pendingObservation == null and .status.pendingLockRelease == null and
      .status.nextReconciliationTime != null
    ' "the quiescent blocked cadence restore"
	pause_controller_status_writes
	restore_after_checkpoint="$WORK_DIR/${restore_schema}-blocked-restore-after.json"
	checkpoint_schema_jobs "$restore_schema" "$restore_after_checkpoint"
	for restore_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$restore_schema" "$restore_operation" \
			"$restore_before_checkpoint" "$restore_after_checkpoint"
	done
	assert_no_job_between_checkpoints "$restore_schema" apply \
		"$restore_before_checkpoint" "$restore_after_checkpoint"
	k -n "$TEST_NAMESPACE" get ptahschema "$restore_schema" -o json |
		jq -e \
			--arg plan "$restore_plan_name" \
			--arg planUID "$restore_plan_uid" \
			--arg fingerprint "$restore_plan_fingerprint" \
			--arg digest "$restore_source_digest" \
			--arg interval "$QUIESCENT_INTERVAL" '
          .spec.interval == $interval and .status.phase == "Blocked" and
          .status.source.digest == $digest and .status.plan.name == $plan and
          .status.plan.uid == $planUID and .status.plan.fingerprint == $fingerprint and
          .status.plan.destructive == true
        ' >/dev/null || fail "$restore_schema changed blocked evidence while restoring a quiet cadence"
	audit_runtime_credentials
	resume_controller_status_writes || fail "could not restore controller status-write RBAC"
}

assert_mysql_destructive_refusal_durable() {
	if [ -z "$MYSQL_DESTRUCTIVE_SCHEMA" ] || [ -z "$MYSQL_DESTRUCTIVE_PLAN" ] ||
		[ -z "$MYSQL_DESTRUCTIVE_PLAN_UID" ] ||
		[ -z "$MYSQL_DESTRUCTIVE_APPLY_CHECKPOINT" ] ||
		[ -z "$MYSQL_DESTRUCTIVE_DIGEST" ]; then
		fail "MySQL destructive-refusal evidence was not retained"
	fi
	wait_for_schema "$MYSQL_DESTRUCTIVE_SCHEMA" \
		'.status.phase == "Blocked" and .status.activeOperation == null and .status.pendingObservation == null and .status.pendingLockRelease == null' \
		"the long-window MySQL DROP INDEX refusal"
	k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_DESTRUCTIVE_SCHEMA" -o json |
		jq -e \
			--arg plan "$MYSQL_DESTRUCTIVE_PLAN" \
			--arg planUID "$MYSQL_DESTRUCTIVE_PLAN_UID" \
			--arg digest "$MYSQL_DESTRUCTIVE_DIGEST" '
        .status.phase == "Blocked" and .status.source.digest == $digest and
        .status.plan.name == $plan and .status.plan.uid == $planUID and
        .status.plan.destructive == true and .status.plan.approval == null and
        .status.activeOperation == null and .status.pendingObservation == null and
		.status.pendingLockRelease == null and
        (.status.conditions | any(
          .type == "ApprovalRequired" and .status == "False" and
          .reason == "DestructiveChangesDisabled")) and
        (.status.conditions | any(.type == "Ready" and .status == "False"))
      ' >/dev/null || fail "MySQL DROP INDEX did not remain durably blocked"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$MYSQL_DESTRUCTIVE_PLAN" -o json |
		jq -e --arg uid "$MYSQL_DESTRUCTIVE_PLAN_UID" '
        .metadata.uid == $uid and .spec.destructive == true and
        .spec.statementCount == 1
      ' >/dev/null || fail "MySQL DROP INDEX destructive plan identity changed"
	assert_no_new_jobs "$MYSQL_DESTRUCTIVE_SCHEMA" apply \
		"$MYSQL_DESTRUCTIVE_APPLY_CHECKPOINT"
	assert_database_column mysql note 1
	assert_database_column mysql enabled 1
	assert_mysql_unique_index 1
	assert_mysql_plain_index 1
}

run_engine_lifecycle() {
	lifecycle_slug=$1
	lifecycle_engine=$2
	lifecycle_dialect=$3
	lifecycle_secret=$4
	lifecycle_schema="e2e-${lifecycle_slug}"
	lifecycle_coordination_key="e2e/${lifecycle_slug}/app"
	case "$lifecycle_engine" in
	PostgreSQL) lifecycle_coordination_engine=postgresql ;;
	MySQL) lifecycle_coordination_engine=mysql ;;
	*) fail "unsupported coordination engine $lifecycle_engine" ;;
	esac
	lifecycle_coordination_digest=$(coordination_digest \
		"$lifecycle_coordination_engine" "$lifecycle_coordination_key")
	lifecycle_reference="oci://${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000/schemas/${lifecycle_slug}:stable"

	printf 'e2e data plane: starting %s lifecycle\n' "$lifecycle_engine"
	v1_apply_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-apply.json"
	v1_observe_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-observe.json"
	v1_plan_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-plan.json"
	coordination_lease_checkpoint="$WORK_DIR/${lifecycle_schema}-leases.json"
	checkpoint_jobs "$lifecycle_schema" apply "$v1_apply_checkpoint"
	checkpoint_jobs "$lifecycle_schema" observe "$v1_observe_checkpoint"
	checkpoint_jobs "$lifecycle_schema" plan "$v1_plan_checkpoint"
	checkpoint_coordination_leases "$coordination_lease_checkpoint"
	digest_v1=$(publish_schema "$lifecycle_slug" v1 "$lifecycle_dialect" "$lifecycle_reference")
	create_schema_resource "$lifecycle_schema" "$lifecycle_engine" "$lifecycle_secret" \
		"$lifecycle_reference" "$lifecycle_coordination_key"
	assert_plan "$lifecycle_schema" "$lifecycle_reference" "$digest_v1" "$lifecycle_dialect" false \
		"$v1_observe_checkpoint" "$v1_plan_checkpoint"
	assert_coordination_boundary "$lifecycle_schema" "$lifecycle_coordination_key" \
		"$lifecycle_coordination_digest"
	plan_v1=$CURRENT_PLAN
	plan_v1_uid=$CURRENT_PLAN_UID
	plan_v1_fingerprint=$CURRENT_PLAN_FINGERPRINT
	assert_job_isolation "$lifecycle_schema" "$lifecycle_secret" false
	assert_no_new_jobs "$lifecycle_schema" apply "$v1_apply_checkpoint"
	v1_post_observe_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-post-observe.json"
	v1_post_plan_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-post-plan.json"
	checkpoint_jobs "$lifecycle_schema" observe "$v1_post_observe_checkpoint"
	checkpoint_jobs "$lifecycle_schema" plan "$v1_post_plan_checkpoint"
	create_exact_approval "$lifecycle_schema" "$plan_v1" "${lifecycle_schema}-v1" \
		"$lifecycle_coordination_key" "$lifecycle_coordination_digest"
	wait_for_one_new_job "$lifecycle_schema" apply "$v1_apply_checkpoint"
	wait_for_in_sync "$lifecycle_schema" "$digest_v1" \
		"$v1_post_observe_checkpoint" "$v1_post_plan_checkpoint"
	assert_approval_consumed "${lifecycle_schema}-v1" "$plan_v1_uid"
	assert_one_new_job "$lifecycle_schema" apply "$v1_apply_checkpoint"
	assert_coordination_lease_boundary "$lifecycle_coordination_key" \
		"$coordination_lease_checkpoint"
	assert_job_isolation "$lifecycle_schema" "$lifecycle_secret" true
	assert_database_column "$lifecycle_slug" name 1
	assert_database_column "$lifecycle_slug" note 0
	set_reconcile_interval_and_assert_noop "$lifecycle_schema" "$digest_v1" \
		"$RECONCILE_INTERVAL"
	assert_periodic_noop "$lifecycle_schema" "$PERIODIC_NOOP_CHECKPOINT"
	set_reconcile_interval_and_assert_noop "$lifecycle_schema" "$digest_v1" \
		"$TAG_MOVE_INTERVAL" true

	# Block only controller status writes before publishing. The PtahSchema
	# generation stays unchanged, so the persisted timer is the sole trigger
	# that can discover the moved tag after authorization is restored.
	[ "$RBAC_PAUSED" -eq 1 ] || fail "scheduled tag proof lacks a status-write barrier"
	scheduled_tag_generation=$(k -n "$TEST_NAMESPACE" get ptahschema "$lifecycle_schema" \
		-o jsonpath='{.metadata.generation}')
	digest_v2=$(publish_schema "$lifecycle_slug" v2 "$lifecycle_dialect" "$lifecycle_reference")
	[ "$digest_v2" != "$digest_v1" ] || fail "$lifecycle_schema v2 did not move the mutable tag"
	wait_for_schema "$lifecycle_schema" \
		".metadata.generation == $scheduled_tag_generation and .status.observedGeneration == $scheduled_tag_generation and .status.phase == \"InSync\" and .status.source.digest == \"$digest_v1\" and .status.activeOperation == null and (.status.nextReconciliationTime | fromdateiso8601) <= now" \
		"the persisted mutable-tag polling deadline to elapse without a spec change"
	v2_checkpoint="$WORK_DIR/${lifecycle_schema}-v2-scheduled-before.json"
	checkpoint_schema_jobs "$lifecycle_schema" "$v2_checkpoint"
	resume_controller_status_writes || fail "could not restore controller status-write RBAC"
	v2_after_checkpoint="$WORK_DIR/${lifecycle_schema}-v2-scheduled-after.json"
	assert_plan "$lifecycle_schema" "$lifecycle_reference" "$digest_v2" "$lifecycle_dialect" false \
		"$v2_checkpoint" "$v2_checkpoint" "$v2_after_checkpoint"
	for scheduled_tag_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$lifecycle_schema" "$scheduled_tag_operation" \
			"$v2_checkpoint" "$v2_after_checkpoint"
	done
	assert_no_job_between_checkpoints "$lifecycle_schema" apply \
		"$v2_checkpoint" "$v2_after_checkpoint"
	v2_generation=$(k -n "$TEST_NAMESPACE" get ptahschema "$lifecycle_schema" \
		-o jsonpath='{.metadata.generation}')
	v2_observed_generation=$(k -n "$TEST_NAMESPACE" get ptahschema "$lifecycle_schema" \
		-o jsonpath='{.status.observedGeneration}')
	if [ "$v2_generation" != "$scheduled_tag_generation" ] ||
		[ "$v2_observed_generation" != "$scheduled_tag_generation" ]; then
		fail "$lifecycle_schema scheduled tag refresh depended on a spec generation change"
	fi
	plan_v2=$CURRENT_PLAN
	plan_v2_uid=$CURRENT_PLAN_UID
	plan_v2_fingerprint=$CURRENT_PLAN_FINGERPRINT
	[ "$plan_v2" != "$plan_v1" ] || fail "$lifecycle_schema reused the v1 plan name after a tag move"
	[ "$plan_v2_uid" != "$plan_v1_uid" ] || fail "$lifecycle_schema reused the v1 plan UID after a tag move"
	[ "$plan_v2_fingerprint" != "$plan_v1_fingerprint" ] ||
		fail "$lifecycle_schema reused the v1 fingerprint after a tag move"

	# Admission and reconciliation normally race after an approval is created.
	# Remove only the controller's status-write verb while leaving webhook reads
	# available, then move the tag again. Restoring the exact original verbs
	# makes the controller observe v3 before it can consume the v2 decision.
	obsolete_approval="${lifecycle_schema}-obsolete"
	stale_checkpoint=$v2_after_checkpoint
	create_exact_approval "$lifecycle_schema" "$plan_v2" "$obsolete_approval" \
		"$lifecycle_coordination_key" "$lifecycle_coordination_digest"
	digest_v3=$(publish_schema "$lifecycle_slug" v3 "$lifecycle_dialect" "$lifecycle_reference")
	[ "$digest_v3" != "$digest_v2" ] || fail "$lifecycle_schema v3 did not move the mutable tag"
	k -n "$TEST_NAMESPACE" patch ptahschema "$lifecycle_schema" --type=merge \
		-p "$(jq -nc --arg interval "$STALE_APPROVAL_INTERVAL" \
			'{spec: {interval: $interval}}')" >/dev/null
	resume_controller_status_writes || fail "could not restore controller status-write RBAC"
	v3_after_checkpoint="$WORK_DIR/${lifecycle_schema}-v3-after.json"
	assert_plan "$lifecycle_schema" "$lifecycle_reference" "$digest_v3" "$lifecycle_dialect" false \
		"$stale_checkpoint" "$stale_checkpoint" "$v3_after_checkpoint"
	plan_v3=$CURRENT_PLAN
	plan_v3_uid=$CURRENT_PLAN_UID
	plan_v3_fingerprint=$CURRENT_PLAN_FINGERPRINT
	[ "$plan_v3" != "$plan_v2" ] || fail "$lifecycle_schema reused the v2 plan name after a tag move"
	[ "$plan_v3_uid" != "$plan_v2_uid" ] || fail "$lifecycle_schema reused the v2 plan UID after a tag move"
	[ "$plan_v3_fingerprint" != "$plan_v2_fingerprint" ] ||
		fail "$lifecycle_schema reused the v2 fingerprint after a tag move"
	assert_no_job_between_checkpoints "$lifecycle_schema" apply \
		"$stale_checkpoint" "$v3_after_checkpoint"
	resume_controller_status_writes || fail "could not restore controller status-write RBAC"
	wait_for_approval "$obsolete_approval" \
		'.status.conditions | any(.type == "Stale" and .status == "True" and .reason == "PlanNoLongerCurrent")' \
		"the old exact approval to become stale"
	assert_no_new_jobs "$lifecycle_schema" apply "$stale_checkpoint"
	v3_apply_checkpoint="$WORK_DIR/${lifecycle_schema}-v3-apply.json"
	checkpoint_schema_jobs "$lifecycle_schema" "$v3_apply_checkpoint"
	create_exact_approval "$lifecycle_schema" "$plan_v3" "${lifecycle_schema}-v3" \
		"$lifecycle_coordination_key" "$lifecycle_coordination_digest"
	wait_for_one_new_job "$lifecycle_schema" apply "$v3_apply_checkpoint"
	wait_for_in_sync "$lifecycle_schema" "$digest_v3" \
		"$v3_apply_checkpoint" "$v3_apply_checkpoint"
	assert_approval_consumed "${lifecycle_schema}-v3" "$plan_v3_uid"
	assert_one_new_job "$lifecycle_schema" apply "$v3_apply_checkpoint"
	assert_coordination_lease_boundary "$lifecycle_coordination_key" \
		"$coordination_lease_checkpoint"
	assert_database_column "$lifecycle_slug" note 1
	assert_database_column "$lifecycle_slug" enabled 1
	if [ "$lifecycle_slug" = mysql ]; then
		assert_mysql_unique_index 1
		assert_mysql_plain_index 1
	fi

	suspend_schema_for_tag_move "$lifecycle_schema" "$APPROVAL_INTERVAL"
	digest_v4=$(publish_schema "$lifecycle_slug" v4 "$lifecycle_dialect" "$lifecycle_reference")
	[ "$digest_v4" != "$digest_v3" ] || fail "$lifecycle_schema v4 did not move the mutable tag"
	destructive_apply_checkpoint="$WORK_DIR/${lifecycle_schema}-v4-before.json"
	checkpoint_schema_jobs "$lifecycle_schema" "$destructive_apply_checkpoint"
	resume_schema_after_tag_move "$lifecycle_schema"
	v4_after_checkpoint="$WORK_DIR/${lifecycle_schema}-v4-after.json"
	assert_plan "$lifecycle_schema" "$lifecycle_reference" "$digest_v4" "$lifecycle_dialect" true \
		"$destructive_apply_checkpoint" "$destructive_apply_checkpoint" "$v4_after_checkpoint"
	if [ "$lifecycle_slug" = mysql ]; then
		jq -e '
		  .destructive == false and
		  (.statements | length) == 1 and
		  (.statements[0].sql | test("\\bDROP[[:space:]]+INDEX\\b"; "i")) and
		  (.statements[0].sql | contains("e2e_widgets_name_idx")) and
		  (.statements[0].severity | ascii_downcase) != "destructive"
		' "$plan_document_file" >/dev/null ||
			fail "MySQL fixture did not exercise an executor-underclassified DROP INDEX"
		k -n "$TEST_NAMESPACE" get ptahschemaplan "$CURRENT_PLAN" -o json |
			jq -e '.spec.destructive == true' >/dev/null ||
			fail "MySQL DROP INDEX was not conservatively elevated to destructive"
	fi
	wait_for_schema "$lifecycle_schema" '
      .status.phase == "Blocked" and
      (.status.conditions | any(.type == "ApprovalRequired" and .status == "False" and .reason == "DestructiveChangesDisabled")) and
      (.status.conditions | any(.type == "Ready" and .status == "False"))
	    ' "the destructive policy gate"
	assert_no_job_between_checkpoints "$lifecycle_schema" apply \
		"$destructive_apply_checkpoint" "$v4_after_checkpoint"
	prepare_blocked_refresh_cadence "$lifecycle_schema"
	assert_destructive_gate "$lifecycle_schema" "$destructive_apply_checkpoint" \
		"$CURRENT_PLAN" "$CURRENT_PLAN_UID" "$CURRENT_PLAN_FINGERPRINT" "$digest_v4" \
		"$BLOCKED_GATE_CHECKPOINT"
	restore_blocked_refresh_cadence "$lifecycle_schema" \
		"$CURRENT_PLAN" "$CURRENT_PLAN_UID" "$CURRENT_PLAN_FINGERPRINT" "$digest_v4"
	assert_database_column "$lifecycle_slug" note 1
	assert_database_column "$lifecycle_slug" enabled 1
	if [ "$lifecycle_slug" = mysql ]; then
		assert_mysql_unique_index 1
		assert_mysql_plain_index 1
		MYSQL_DESTRUCTIVE_SCHEMA=$lifecycle_schema
		MYSQL_DESTRUCTIVE_PLAN=$CURRENT_PLAN
		MYSQL_DESTRUCTIVE_PLAN_UID=$CURRENT_PLAN_UID
		MYSQL_DESTRUCTIVE_APPLY_CHECKPOINT=$destructive_apply_checkpoint
		MYSQL_DESTRUCTIVE_DIGEST=$digest_v4
	fi
	assert_coordination_boundary "$lifecycle_schema" "$lifecycle_coordination_key" \
		"$lifecycle_coordination_digest"
	assert_coordination_lease_boundary "$lifecycle_coordination_key" \
		"$coordination_lease_checkpoint"
	audit_runtime_credentials
	printf 'e2e data plane: PASS %s lifecycle\n' "$lifecycle_engine"
}

printf '%s\n' 'e2e data plane: creating registry endpoint and isolated databases'
create_registry_service
create_databases
k -n "$TEST_NAMESPACE" rollout status deployment/"$PG_SERVICE" --timeout="${TIMEOUT_SECONDS}s"
k -n "$TEST_NAMESPACE" rollout status deployment/"$MYSQL_SERVICE" --timeout="${TIMEOUT_SECONDS}s"
wait_for_database postgresql
wait_for_database mysql
report_database_versions
create_admission_fixtures

run_engine_lifecycle postgresql PostgreSQL postgres "$PG_SECRET"
run_engine_lifecycle mysql MySQL mysql "$MYSQL_SECRET"
run_mysql_dsn_refusal
[ "$EPHEMERAL_SUBRESOURCE_TESTED" -eq 1 ] ||
	fail "no UID-bound active operation Pod was available for the ephemeralcontainers rejection test"
audit_runtime_credentials

printf '%s\n' 'e2e data plane: starting restart and fault-injection acceptance'
E2E_AUDITED_JOBS_FILE=$AUDITED_JOBS_FILE \
E2E_OBSERVED_JOBS_FILE=$OBSERVED_JOBS_FILE \
E2E_FIXTURE_IMAGE=$FIXTURE_IMAGE \
E2E_RESULT_ASSERT_BINARY=$RESULT_ASSERT_BINARY \
	"$ROOT_DIR/hack/e2e-faults.sh"
assert_mysql_destructive_refusal_durable
audit_runtime_credentials
assert_observed_jobs_audited

printf '%s\n' 'e2e data plane: PASS full PostgreSQL, MySQL, OCI, restart, and fault lifecycle'
