#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

KUBECONFIG_FILE=${E2E_KUBECONFIG:-}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:-}
HELM_RELEASE=${E2E_HELM_RELEASE:-}
EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
FIXTURE_IMAGE=${E2E_FIXTURE_IMAGE:-}
RESULT_ASSERT_BINARY=${E2E_RESULT_ASSERT_BINARY:-}
REGISTRY_SERVICE=${E2E_REGISTRY_SERVICE:-registry}
TIMEOUT_SECONDS=${E2E_TIMEOUT_SECONDS:-600}
FAULT_ACTIVE_DEADLINE_SECONDS=${E2E_FAULT_ACTIVE_DEADLINE_SECONDS:-7200}
FAULT_BARRIER_SECONDS=${E2E_FAULT_BARRIER_SECONDS:-7800}
FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS=${E2E_FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS:-45}
SHARED_AUDITED_JOBS_FILE=${E2E_AUDITED_JOBS_FILE:-}
SHARED_FULLY_AUDITED_JOBS_FILE=${E2E_FULLY_AUDITED_JOBS_FILE:-}
SHARED_OBSERVED_JOBS_FILE=${E2E_OBSERVED_JOBS_FILE:-}

# These locals transiently hold Secret values. Clearing imported definitions
# prevents a caller's export attribute from leaking reassigned values to host
# subprocesses.
unset protected_value base_url new_url url_value aliased_url PRINCIPAL_PASSWORD

CONTROLLER_NAME="${HELM_RELEASE}-ptah-operator"
LEADER_LEASE=ptah-operator.operator.ptah.dev
PG_SERVICE=e2e-postgresql
PG_BASE_SECRET=e2e-postgresql-db
MYSQL_SERVICE=e2e-mysql
MYSQL_BASE_SECRET=e2e-mysql-db
REGISTRY_AUTH_SECRET=e2e-registry-auth
REGISTRY_PULL_SECRET=e2e-registry-pull
PRINCIPAL_SCHEMA_SECRET=e2e-credential-principal-schema
MYSQL_APP_USER=ptah_e2e
PTAH_PG_APPLY_LOCK_KEY=1237737229

WATCH_PIDS=
WATCH_FRAME_STATUS_FILES=
WATCH_STOP_FILES=
WATCH_STEMS=
WATCH_FILES=
WATCH_ERROR_FILES=
WATCH_HEARTBEAT_PID=
WATCH_HEARTBEAT_STOP_FILE=
WATCH_HEARTBEAT_STATUS_FILE=
WATCH_HEARTBEAT_ERROR_FILE=
WATCH_HEARTBEAT_STOPPED=0
WATCH_BARRIER_SEQUENCE=0
PG_BARRIER_TOKENS=
MYSQL_BARRIER_PID=
MYSQL_BARRIER_READY_LOCK=
STATUS_RBAC_PAUSED=0
STATUS_RBAC_RULE_INDEX=
STATUS_RBAC_ORIGINAL_VERBS=
READ_WORKLOAD_BARRIER_ACTIVE=0
READ_WORKLOAD_BARRIER_KEY=operator.ptah.dev/e2e-read-workload-barrier
FOLLOW_LOG_PID=
FOLLOW_LOG_FILE=
FOLLOW_LOG_STATUS_FILE=
FOLLOW_LOG_POD_UID=
FOLLOW_LOG_RECORD_POD=0
DELETED_SCHEMA_NAME=
DELETED_DATABASE_NAME=
DELETED_DATABASE_FINGERPRINT=
LAST_FAULT_AUDIT_AT=0
PRINCIPAL_REFUSAL_SCHEMA=
PRINCIPAL_PLAN_UID=
PRINCIPAL_PLAN_POD_UID=

fail() {
	printf 'e2e faults: %s\n' "$*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "required command is not installed: $1"
}

is_pinned_image() {
	printf '%s\n' "$1" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
}

for command_name in kubectl jq awk sed grep tr mktemp mkfifo date sleep base64 wc tail mkdir mv; do
	require_command "$command_name"
done
for value_name in \
	KUBECONFIG_FILE OPERATOR_NAMESPACE TEST_NAMESPACE HELM_RELEASE EXECUTOR_IMAGE FIXTURE_IMAGE \
	RESULT_ASSERT_BINARY \
	SHARED_AUDITED_JOBS_FILE SHARED_FULLY_AUDITED_JOBS_FILE SHARED_OBSERVED_JOBS_FILE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
if [ ! -f "$SHARED_AUDITED_JOBS_FILE" ] || [ ! -w "$SHARED_AUDITED_JOBS_FILE" ]; then
	fail "E2E_AUDITED_JOBS_FILE must name the writable parent audit ledger"
fi
if [ ! -f "$SHARED_FULLY_AUDITED_JOBS_FILE" ] || [ ! -w "$SHARED_FULLY_AUDITED_JOBS_FILE" ]; then
	fail "E2E_FULLY_AUDITED_JOBS_FILE must name the writable parent full-audit ledger"
fi
if [ ! -f "$SHARED_OBSERVED_JOBS_FILE" ] || [ ! -w "$SHARED_OBSERVED_JOBS_FILE" ]; then
	fail "E2E_OBSERVED_JOBS_FILE must name the writable parent Job ledger"
fi
is_pinned_image "$EXECUTOR_IMAGE" ||
	fail "E2E_EXECUTOR_IMAGE must be pinned by a lowercase SHA-256 digest"
is_pinned_image "$FIXTURE_IMAGE" ||
	fail "E2E_FIXTURE_IMAGE must be pinned by a lowercase SHA-256 digest"
if [ ! -f "$RESULT_ASSERT_BINARY" ] || [ ! -x "$RESULT_ASSERT_BINARY" ]; then
	fail "E2E_RESULT_ASSERT_BINARY must name the executable parent result parser"
fi
printf '%s\n' "$TIMEOUT_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_TIMEOUT_SECONDS must be a positive integer"
printf '%s\n' "$FAULT_ACTIVE_DEADLINE_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_FAULT_ACTIVE_DEADLINE_SECONDS must be a positive integer"
printf '%s\n' "$FAULT_BARRIER_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_FAULT_BARRIER_SECONDS must be a positive integer"
printf '%s\n' "$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS must be a positive integer"
if [ "$FAULT_ACTIVE_DEADLINE_SECONDS" -lt 7200 ] || [ "$FAULT_ACTIVE_DEADLINE_SECONDS" -gt 86400 ]; then
	fail "the fault Apply active deadline must be between 7200 and 86400 seconds"
fi
[ "$FAULT_BARRIER_SECONDS" -gt "$FAULT_ACTIVE_DEADLINE_SECONDS" ] ||
	fail "the fault database barrier must outlive the Apply active deadline"
if [ "$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS" -lt 30 ] ||
	[ "$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS" -gt 120 ]; then
	fail "the timeout acceptance Apply deadline must be between 30 and 120 seconds"
fi
minimum_timeout_seconds=$((FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS + 60))
[ "$TIMEOUT_SECONDS" -ge "$minimum_timeout_seconds" ] ||
	fail "E2E_TIMEOUT_SECONDS must leave at least 60 seconds after the timeout acceptance Apply deadline"

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-fault-e2e.XXXXXX")
chmod 700 "$WORK_DIR"
RESOURCE_FILE=$WORK_DIR/resource.json
LOG_FILE=$WORK_DIR/private.log
AUDITED_FAULT_JOBS_FILE=$WORK_DIR/audited-job-uids.txt
AUDITED_FAULT_PODS_FILE=$WORK_DIR/audited-pod-uids.txt
# Targeted proofs enter the broad ledgers above. Only these full ledgers can
# authorize a TTL/GC skip; a proven never-started Pod has no logs to collect.
FULLY_AUDITED_FAULT_PODS_FILE=$WORK_DIR/fully-audited-pod-uids.txt
FAULT_CREDENTIAL_PATTERNS_FILE=$WORK_DIR/credential-patterns.txt
URL_VALUE_FILE=$WORK_DIR/database.url
: >"$AUDITED_FAULT_JOBS_FILE"
: >"$AUDITED_FAULT_PODS_FILE"
: >"$FULLY_AUDITED_FAULT_PODS_FILE"

stop_pid() {
	stop_target=$1
	[ -n "$stop_target" ] || return 0
	kill "$stop_target" >/dev/null 2>&1 || true
	wait "$stop_target" >/dev/null 2>&1 || true
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	set +e
	stop_pid "$FOLLOW_LOG_PID"
	if [ -n "$WATCH_HEARTBEAT_STOP_FILE" ]; then
		: >"$WATCH_HEARTBEAT_STOP_FILE"
	fi
	if [ -n "$WATCH_HEARTBEAT_PID" ]; then
		cleanup_heartbeat_deadline=$(($(date +%s) + 15))
		while [ "$(date +%s)" -lt "$cleanup_heartbeat_deadline" ] &&
			[ ! -f "$WATCH_HEARTBEAT_STATUS_FILE" ]; do
			sleep 1
		done
		stop_pid "$WATCH_HEARTBEAT_PID"
	fi
	for cleanup_watch_stop in $WATCH_STOP_FILES; do
		: >"$cleanup_watch_stop"
	done
	cleanup_watch_deadline=$(($(date +%s) + 35))
	for cleanup_watch_status in $WATCH_FRAME_STATUS_FILES; do
		while [ "$(date +%s)" -lt "$cleanup_watch_deadline" ] && [ ! -f "$cleanup_watch_status" ]; do
			sleep 1
		done
	done
	for cleanup_pid in $WATCH_PIDS; do
		stop_pid "$cleanup_pid"
	done
	for cleanup_pg_token in $PG_BARRIER_TOKENS; do
		pg_query postgres "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='${cleanup_pg_token}'" >/dev/null 2>&1 || true
		cleanup_pg_pid_file=$WORK_DIR/pg-barrier-${cleanup_pg_token}.pid
		if [ -f "$cleanup_pg_pid_file" ]; then
			cleanup_pg_pid=$(tr -d '[:space:]' <"$cleanup_pg_pid_file")
			stop_pid "$cleanup_pg_pid"
		fi
	done
	if [ -n "$MYSQL_BARRIER_READY_LOCK" ]; then
		cleanup_mysql_id=$(mysql_root_query mysql "SELECT IS_USED_LOCK('${MYSQL_BARRIER_READY_LOCK}')" 2>/dev/null | tr -d '[:space:]')
		case "$cleanup_mysql_id" in
		'' | NULL) ;;
		*[!0-9]*) ;;
		*) mysql_root_query mysql "KILL ${cleanup_mysql_id}" >/dev/null 2>&1 || true ;;
		esac
	fi
	stop_pid "$MYSQL_BARRIER_PID"
	if [ "$STATUS_RBAC_PAUSED" -eq 1 ]; then
		if ! resume_controller_status_writes; then
			printf '%s\n' 'e2e faults: could not restore controller status-write RBAC' >&2
			status=1
		fi
	fi
	if [ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 1 ]; then
		if ! k taint nodes --all "${READ_WORKLOAD_BARRIER_KEY}-" >/dev/null 2>&1; then
			printf '%s\n' 'e2e faults: could not remove the test scheduling barrier' >&2
			status=1
		fi
		READ_WORKLOAD_BARRIER_ACTIVE=0
	fi
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-operator-fault-e2e.*) rm -rf -- "$WORK_DIR" ;;
	*)
		printf 'e2e faults: refusing to remove unexpected work directory %s\n' "$WORK_DIR" >&2
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

assert_safe_name() {
	printf '%s\n' "$1" | grep -Eq '^[a-z][a-z0-9_]{0,47}$' ||
		fail "unsafe generated database or barrier name: $1"
}

pg_query() {
	pg_database=$1
	pg_sql=$2
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"' \
		sh "$pg_database" "$pg_sql"
}

mysql_root_query() {
	mysql_database=$1
	mysql_sql=$2
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"' \
		sh "$mysql_database" "$mysql_sql"
}

wait_query_equals() {
	query_engine=$1
	query_database=$2
	query_sql=$3
	query_expected=$4
	query_description=$5
	query_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$query_deadline" ]; do
		maybe_audit_fault_runtime
		case "$query_engine" in
		postgresql) query_result=$(pg_query "$query_database" "$query_sql" 2>/dev/null || true) ;;
		mysql) query_result=$(mysql_root_query "$query_database" "$query_sql" 2>/dev/null || true) ;;
		*) fail "unsupported query engine $query_engine" ;;
		esac
		query_result=$(printf '%s' "$query_result" | tr -d '[:space:]')
		[ "$query_result" = "$query_expected" ] && return 0
		sleep 1
	done
	fail "timed out waiting for $query_description"
}

wait_for_controller_status_authorization() {
	expected_answer=$1
	authorization_deadline=$(($(date +%s) + 30))
	while [ "$(date +%s)" -lt "$authorization_deadline" ]; do
		maybe_audit_fault_runtime
		rbac_answer=$(k auth can-i patch ptahschemas.operator.ptah.dev \
			--subresource=status \
			--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${CONTROLLER_NAME}" 2>/dev/null || true)
		[ "$rbac_answer" = "$expected_answer" ] && return 0
		sleep 1
	done
	return 1
}

pause_controller_status_writes() {
	[ "$STATUS_RBAC_PAUSED" -eq 0 ] || fail "controller status-write RBAC is already paused"
	rbac_role=$(k get clusterrole "$CONTROLLER_NAME" -o json)
	STATUS_RBAC_RULE_INDEX=$(printf '%s\n' "$rbac_role" | jq -r '
    [.rules | to_entries[] |
      select(.value.apiGroups == ["operator.ptah.dev"] and
        (.value.resources | index("ptahschemas/status")) != null) | .key][0] // empty
  ')
	printf '%s\n' "$STATUS_RBAC_RULE_INDEX" | grep -Eq '^[0-9]+$' ||
		fail "could not identify the controller status-write ClusterRole rule"
	STATUS_RBAC_ORIGINAL_VERBS=$(printf '%s\n' "$rbac_role" |
		jq -c --argjson index "$STATUS_RBAC_RULE_INDEX" '.rules[$index].verbs')
	rbac_patch=$(jq -nc --arg index "$STATUS_RBAC_RULE_INDEX" '
    [{op: "replace", path: ("/rules/" + $index + "/verbs"), value: ["get"]}]
  ')
	STATUS_RBAC_PAUSED=1
	k patch clusterrole "$CONTROLLER_NAME" --type=json -p "$rbac_patch" >/dev/null
	live_verbs=$(k get clusterrole "$CONTROLLER_NAME" -o json |
		jq -c --argjson index "$STATUS_RBAC_RULE_INDEX" '.rules[$index].verbs')
	[ "$live_verbs" = '["get"]' ] || fail "controller status-write RBAC barrier did not become exact"
	wait_for_controller_status_authorization no ||
		fail "controller status writes remained authorized during the manual-drift barrier"
}

resume_controller_status_writes() {
	[ "$STATUS_RBAC_PAUSED" -eq 1 ] || return 0
	rbac_patch=$(jq -nc \
		--arg index "$STATUS_RBAC_RULE_INDEX" \
		--argjson verbs "$STATUS_RBAC_ORIGINAL_VERBS" '
    [{op: "replace", path: ("/rules/" + $index + "/verbs"), value: $verbs}]
  ')
	if ! k patch clusterrole "$CONTROLLER_NAME" --type=json -p "$rbac_patch" >/dev/null; then
		return 1
	fi
	live_verbs=$(k get clusterrole "$CONTROLLER_NAME" -o json |
		jq -c --argjson index "$STATUS_RBAC_RULE_INDEX" '.rules[$index].verbs') || return 1
	[ "$live_verbs" = "$STATUS_RBAC_ORIGINAL_VERBS" ] || return 1
	wait_for_controller_status_authorization yes || return 1
	STATUS_RBAC_PAUSED=0
}

start_read_workload_barrier() {
	[ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 0 ] ||
		fail "the read-workload scheduling barrier is already active"
	node_snapshot=$(k get nodes -o json)
	printf '%s\n' "$node_snapshot" | jq -e \
		--arg key "$READ_WORKLOAD_BARRIER_KEY" '
      (.items | length) > 0 and
      all(.items[]; all((.spec.taints // [])[]; .key != $key))
    ' >/dev/null || fail "the test scheduling taint already exists on a cluster node"
	# Record ownership before the mutating call so trap cleanup also covers a
	# partially successful --all update.
	READ_WORKLOAD_BARRIER_ACTIVE=1
	k taint nodes --all "${READ_WORKLOAD_BARRIER_KEY}=held:NoSchedule" --overwrite >/dev/null
	k get nodes -o json | jq -e \
		--arg key "$READ_WORKLOAD_BARRIER_KEY" '
      (.items | length) > 0 and
      all(.items[];
        ([.spec.taints[]? |
          select(.key == $key and .value == "held" and .effect == "NoSchedule")] |
          length) == 1)
    ' >/dev/null || fail "the test scheduling barrier was not installed on every node"
}

stop_read_workload_barrier() {
	[ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 1 ] ||
		fail "the read-workload scheduling barrier is not active"
	k taint nodes --all "${READ_WORKLOAD_BARRIER_KEY}-" >/dev/null
	k get nodes -o json | jq -e \
		--arg key "$READ_WORKLOAD_BARRIER_KEY" '
      all(.items[]; all((.spec.taints // [])[]; .key != $key))
    ' >/dev/null || fail "the test scheduling barrier remained on a cluster node"
	READ_WORKLOAD_BARRIER_ACTIVE=0
}

assert_read_workload_blocked() {
	blocked_job_uid=$1
	blocked_description=$2
	[ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 1 ] ||
		fail "$blocked_description was checked without the scheduling barrier"
	blocked_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$blocked_deadline" ]; do
		maybe_audit_fault_runtime
		blocked_job=$(k -n "$TEST_NAMESPACE" get jobs -o json | jq -c \
			--arg uid "$blocked_job_uid" '
          [.items[] | select(.metadata.uid == $uid)] |
          if length == 1 then .[0] else empty end
        ')
		if [ -n "$blocked_job" ]; then
			printf '%s\n' "$blocked_job" | jq -e \
				--arg key "$READ_WORKLOAD_BARRIER_KEY" '
              def matches_test_taint:
                ((.effect // "") == "" or .effect == "NoSchedule") and
                if (.operator // "Equal") == "Exists" then
                  ((.key // "") == "" or .key == $key)
                else
                  .key == $key and (.value // "") == "held"
                end;
              (.status.conditions // [] | all(
                (.type != "Complete" and .type != "Failed") or .status != "True")) and
              all((.spec.template.spec.tolerations // [])[];
                (matches_test_taint | not))
            ' >/dev/null || fail "$blocked_description bypasses or completed through the scheduling barrier"
			blocked_pods=$(k -n "$TEST_NAMESPACE" get pods \
				-l "batch.kubernetes.io/controller-uid=${blocked_job_uid}" -o json)
			printf '%s\n' "$blocked_pods" | jq -e '
              all(.items[];
                (.spec.nodeName // "") == "" and
                (.status.containerStatuses // [] | all(
                  .state.running == null and .state.terminated == null)))
            ' >/dev/null || fail "$blocked_description scheduled or started before status-write revocation"
			return 0
		fi
		sleep 1
	done
	fail "timed out proving the scheduling barrier for $blocked_description"
}

secret_url() {
	secret_name=$1
	k -n "$TEST_NAMESPACE" get secret "$secret_name" -o json |
		jq -er '.data.url | @base64d'
}

secret_value() {
	value_secret=$1
	value_key=$2
	k -n "$TEST_NAMESPACE" get secret "$value_secret" -o json |
		jq -er --arg key "$value_key" '.data[$key] | @base64d'
}

create_credential_principal_secret() {
	principal_suffix=$(printf '%s-principal' "$TEST_NAMESPACE" | cksum | awk '{print $1}')
	PRINCIPAL_ROLE_NAME="e2e_credential_principal_${principal_suffix}"
	PRINCIPAL_PASSWORD="e2ePrincipal${principal_suffix}Q7"
	principal_schema_file=$WORK_DIR/credential-principal.hcl
	principal_password_file=$WORK_DIR/credential-principal.password
	printf 'schema "public" {}\n\nrole "%s" {\n  login = true\n  password = "%s"\n}\n' \
		"$PRINCIPAL_ROLE_NAME" "$PRINCIPAL_PASSWORD" >"$principal_schema_file"
	printf '%s' "$PRINCIPAL_PASSWORD" >"$principal_password_file"
	chmod 600 "$principal_schema_file" "$principal_password_file"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$PRINCIPAL_SCHEMA_SECRET" \
		--rawfile schema "$principal_schema_file" \
		--rawfile password "$principal_password_file" '
      {
        apiVersion: "v1", kind: "Secret",
        metadata: {namespace: $namespace, name: $name},
        type: "Opaque", stringData: {"schema.hcl": $schema, password: $password}
      }
    ' >"$RESOURCE_FILE"
	chmod 600 "$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	: >"$RESOURCE_FILE"
	: >"$principal_schema_file"
	: >"$principal_password_file"
}

materialize_fault_credential_patterns() {
	: >"$FAULT_CREDENTIAL_PATTERNS_FILE"
	for protected_value in \
		"$(secret_value "$REGISTRY_AUTH_SECRET" password)" \
		"$(secret_value "$PG_BASE_SECRET" password)" \
		"$(secret_value "$PG_BASE_SECRET" url)" \
		"$(secret_value "$MYSQL_BASE_SECRET" password)" \
		"$(secret_value "$MYSQL_BASE_SECRET" rootPassword)" \
		"$(secret_value "$MYSQL_BASE_SECRET" url)" \
		"$(secret_value "$PRINCIPAL_SCHEMA_SECRET" password)"; do
		[ -n "$protected_value" ] || fail "a protected credential is empty"
		printf '%s\n' "$protected_value" >>"$FAULT_CREDENTIAL_PATTERNS_FILE"
	done
	chmod 600 "$FAULT_CREDENTIAL_PATTERNS_FILE"
}

scan_fault_file() {
	scan_file=$1
	scan_context=$2
	[ -s "$FAULT_CREDENTIAL_PATTERNS_FILE" ] ||
		fail "fault credential scanner has no non-empty protected patterns"
	if grep -F -f "$FAULT_CREDENTIAL_PATTERNS_FILE" "$scan_file" >/dev/null; then
		fail "a task credential escaped into $scan_context"
	else
		scan_status=$?
		[ "$scan_status" -eq 1 ] ||
			fail "fault credential scanner failed closed while checking $scan_context"
	fi
}

record_audited_uid() {
	uid_file=$1
	uid_value=$2
	grep -Fx "$uid_value" "$uid_file" >/dev/null 2>&1 || printf '%s\n' "$uid_value" >>"$uid_file"
}

audit_fault_runtime() {
	: >"$RESOURCE_FILE"
	k -n "$TEST_NAMESPACE" get \
		ptahschemas,ptahschemaplans,ptahschemaapprovals,jobs,pods,configmaps,events,deployments,replicasets,services,serviceaccounts \
		-o json >>"$RESOURCE_FILE"
	k -n "$OPERATOR_NAMESPACE" get \
		pods,configmaps,events,deployments,replicasets,services,serviceaccounts,leases \
		-o json >>"$RESOURCE_FILE"
	scan_fault_file "$RESOURCE_FILE" "a fault-test non-Secret Kubernetes resource"
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
	printf '%s\n' "$manager_audit_pods" | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name' |
		while IFS= read -r manager_audit_pod; do
			[ -n "$manager_audit_pod" ] || continue
			manager_audit_uid=$(k -n "$OPERATOR_NAMESPACE" get pod "$manager_audit_pod" -o jsonpath='{.metadata.uid}')
			[ -n "$manager_audit_uid" ] || fail "manager Pod $manager_audit_pod has no exact UID"
			if ! k -n "$OPERATOR_NAMESPACE" logs pod/"$manager_audit_pod" \
				--all-containers >"$LOG_FILE" 2>&1; then
				fail "could not audit logs for exact manager Pod $manager_audit_pod UID $manager_audit_uid"
			fi
			scan_fault_file "$LOG_FILE" "logs for exact manager Pod $manager_audit_pod"
		done

	fault_audit_pods=$(k -n "$TEST_NAMESPACE" get pods -o json)
	if ! fault_audit_pod_records=$(printf '%s\n' "$fault_audit_pods" | jq -r '
      .items[] |
	    (.metadata.name |
	      if type == "string" and length > 0 then .
	      else error("Pod has no exact name") end) as $podName |
	    (.metadata.uid |
	      if type == "string" and length > 0 then .
	      else error("Pod has no exact UID") end) as $podUID |
	    ([.metadata.ownerReferences[]? |
	      select(.apiVersion == "batch/v1" and .kind == "Job" and .controller == true) |
	      (.uid |
	        if type == "string" and length > 0 then .
	        else error("controlling Job owner has no exact UID") end)]) as $controllerJobUIDs |
	    ($controllerJobUIDs |
	      if length == 0 then "-"
	      elif length == 1 then .[0]
	      else error("Pod has multiple controlling batch/v1 Job owners") end) as $controllerJobUID |
	    [$podName, $podUID, (.status.phase // "Unknown"), $controllerJobUID] | @tsv
	  '); then
		fail "could not capture exact fault-test Pod identities for credential audit"
	fi
	printf '%s\n' "$fault_audit_pod_records" | while IFS="$(printf '\t')" read -r \
		audit_pod_name audit_pod_uid \
		audit_snapshot_phase audit_pod_job_uid; do
		[ -n "$audit_pod_name" ] || continue
		if [ "$audit_snapshot_phase" = Succeeded ] || [ "$audit_snapshot_phase" = Failed ]; then
			if [ "$audit_pod_job_uid" != "-" ]; then
				if grep -Fx "$audit_pod_job_uid" "$SHARED_FULLY_AUDITED_JOBS_FILE" >/dev/null 2>&1; then
					record_audited_uid "$AUDITED_FAULT_PODS_FILE" "$audit_pod_uid"
					record_audited_uid "$FULLY_AUDITED_FAULT_PODS_FILE" "$audit_pod_uid"
					record_audited_uid "$AUDITED_FAULT_JOBS_FILE" "$audit_pod_job_uid"
					continue
				fi
			fi
		fi
		if ! audit_pod_object=$(k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" \
			-o json 2>/dev/null); then
			fail "unaudited fault-test Pod $audit_pod_name UID $audit_pod_uid disappeared before its log audit"
		fi
		printf '%s\n' "$audit_pod_object" | jq -e \
			--arg uid "$audit_pod_uid" '.metadata.uid == $uid' >/dev/null ||
			fail "unaudited fault-test Pod $audit_pod_name was replaced before UID $audit_pod_uid was audited"
		printf '%s\n' "$audit_pod_object" >"$RESOURCE_FILE"
		scan_fault_file "$RESOURCE_FILE" \
			"the exact live fault-test Pod $audit_pod_name UID $audit_pod_uid"
		: >"$RESOURCE_FILE"
		audit_pod_phase=$(printf '%s\n' "$audit_pod_object" | jq -r '.status.phase // ""')
		printf '%s\n' "$audit_pod_object" | jq -e '
        ([.status.initContainerStatuses // [], .status.containerStatuses // [],
          .status.ephemeralContainerStatuses // []] | add) as $statuses |
        all($statuses[]; (.restartCount // 0) == 0)
      ' >/dev/null || fail "fault-test Pod $audit_pod_name restarted a container before complete log audit"
		audit_started_containers=$(printf '%s\n' "$audit_pod_object" | jq -r '
        [(.status.initContainerStatuses // [])[],
         (.status.containerStatuses // [])[],
         (.status.ephemeralContainerStatuses // [])[]] |
        .[] | select(.state.running != null or .state.terminated != null) | .name
      ')
		for audit_container in $audit_started_containers; do
			if ! k -n "$TEST_NAMESPACE" logs pod/"$audit_pod_name" -c "$audit_container" >"$LOG_FILE" 2>&1; then
				fail "could not audit logs for started container $audit_container in fault-test Pod $audit_pod_name"
			fi
			scan_fault_file "$LOG_FILE" "logs for container $audit_container in fault-test Pod $audit_pod_name"
		done
		if ! audit_pod_after=$(k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" \
			-o json 2>/dev/null); then
			fail "unaudited fault-test Pod $audit_pod_name UID $audit_pod_uid disappeared during its log audit"
		fi
		printf '%s\n' "$audit_pod_after" | jq -e \
			--arg uid "$audit_pod_uid" '.metadata.uid == $uid' >/dev/null ||
			fail "unaudited fault-test Pod $audit_pod_name changed identity during its log audit"
		case "$audit_pod_phase" in
		Succeeded | Failed)
			printf '%s\n' "$audit_pod_object" | jq -e '
            ([.spec.initContainers // [], .spec.containers // [], .spec.ephemeralContainers // []] |
              add | map(.name) | sort) as $declared |
            ([.status.initContainerStatuses // [], .status.containerStatuses // [],
              .status.ephemeralContainerStatuses // []] |
              add | map(select(.state.terminated != null) | .name) | sort) as $terminated |
            $declared == $terminated
          ' >/dev/null || fail "terminal fault-test Pod $audit_pod_name has unaudited nonterminal containers"
			record_audited_uid "$AUDITED_FAULT_PODS_FILE" "$audit_pod_uid"
			record_audited_uid "$FULLY_AUDITED_FAULT_PODS_FILE" "$audit_pod_uid"
			;;
		esac
	done

	terminal_jobs=$(k -n "$TEST_NAMESPACE" get jobs -o json)
	printf '%s\n' "$terminal_jobs" | jq -r '
      .items[] |
      select(.status.conditions // [] |
        any((.type == "Complete" or .type == "Failed") and .status == "True")) |
		[.metadata.uid, .metadata.name] | @tsv
    ' | while IFS="$(printf '\t')" read -r audit_job_uid audit_job_name; do
		[ -n "$audit_job_uid" ] || continue
		if grep -Fx "$audit_job_uid" "$SHARED_FULLY_AUDITED_JOBS_FILE" >/dev/null 2>&1; then
			record_audited_uid "$AUDITED_FAULT_JOBS_FILE" "$audit_job_uid"
			continue
		fi
		audit_job_object=$(k -n "$TEST_NAMESPACE" get job "$audit_job_name" -o json 2>/dev/null || true)
		[ -n "$audit_job_object" ] ||
			fail "terminal fault-test Job $audit_job_name UID $audit_job_uid disappeared before audit"
		printf '%s\n' "$audit_job_object" | jq -e \
			--arg uid "$audit_job_uid" '
          .metadata.uid == $uid and
          (.status.conditions // [] |
            any((.type == "Complete" or .type == "Failed") and .status == "True"))
        ' >/dev/null ||
			fail "terminal fault-test Job $audit_job_name was replaced before UID $audit_job_uid was audited"
		audit_job_pods=$(k -n "$TEST_NAMESPACE" get pods -o json | jq \
			--arg uid "$audit_job_uid" '
          {apiVersion: "v1", kind: "List", items: [
            .items[] | select(.metadata.ownerReferences // [] | any(
              .apiVersion == "batch/v1" and .kind == "Job" and
              .uid == $uid and .controller == true))
          ]}
        ')
		[ "$(printf '%s\n' "$audit_job_pods" | jq '.items | length')" -gt 0 ] || continue
		if ! printf '%s\n' "$audit_job_pods" | jq -r '.items[].metadata.uid' |
			while IFS= read -r audit_job_pod_uid; do
				grep -Fx "$audit_job_pod_uid" "$FULLY_AUDITED_FAULT_PODS_FILE" >/dev/null || exit 1
			done; then
			continue
		fi
		printf '%s\n' "$audit_job_object" >"$RESOURCE_FILE"
		printf '%s\n' "$audit_job_pods" >>"$RESOURCE_FILE"
		scan_fault_file "$RESOURCE_FILE" \
			"the exact terminal fault-test Job $audit_job_name and its exact owned Pods"
		: >"$RESOURCE_FILE"
		k -n "$TEST_NAMESPACE" get job "$audit_job_name" -o json | jq -e \
			--arg uid "$audit_job_uid" '.metadata.uid == $uid' >/dev/null ||
			fail "terminal fault-test Job $audit_job_name changed UID during its Pod audit"
		record_audited_uid "$AUDITED_FAULT_JOBS_FILE" "$audit_job_uid"
		record_audited_uid "$SHARED_AUDITED_JOBS_FILE" "$audit_job_uid"
		record_audited_uid "$SHARED_FULLY_AUDITED_JOBS_FILE" "$audit_job_uid"
	done
	: >"$LOG_FILE"
	LAST_FAULT_AUDIT_AT=$(date +%s)
}

maybe_audit_fault_runtime() {
	audit_now=$(date +%s)
	if [ $((audit_now - LAST_FAULT_AUDIT_AT)) -ge 30 ]; then
		audit_fault_runtime
	fi
}

start_follow_logs() {
	follow_namespace=$1
	follow_pod=$2
	follow_stem=$3
	follow_pod_uid=${4:-}
	[ -z "$FOLLOW_LOG_PID" ] || fail "a protected log follower is already running"
	live_follow_uid=$(k -n "$follow_namespace" get pod "$follow_pod" -o jsonpath='{.metadata.uid}')
	[ -n "$live_follow_uid" ] || fail "protected log Pod $follow_pod has no UID"
	if [ -n "$follow_pod_uid" ] && [ "$live_follow_uid" != "$follow_pod_uid" ]; then
		fail "protected log Pod $follow_pod changed identity before the destructive window"
	fi
	k -n "$follow_namespace" get pod "$follow_pod" -o json | jq -e '
      ([.status.initContainerStatuses // [], .status.containerStatuses // [],
        .status.ephemeralContainerStatuses // []] | add) as $statuses |
      all($statuses[]; (.restartCount // 0) == 0)
    ' >/dev/null || fail "protected log Pod $follow_pod restarted before its continuous audit window"
	FOLLOW_LOG_FILE=$WORK_DIR/follow-${follow_stem}.log
	FOLLOW_LOG_STATUS_FILE=$WORK_DIR/follow-${follow_stem}.status
	FOLLOW_LOG_POD_UID=$follow_pod_uid
	FOLLOW_LOG_RECORD_POD=0
	[ "$follow_namespace" != "$TEST_NAMESPACE" ] || FOLLOW_LOG_RECORD_POD=1
	(
		set +e
		k -n "$follow_namespace" logs -f pod/"$follow_pod" --all-containers \
			>"$FOLLOW_LOG_FILE" 2>&1
		follow_status=$?
		printf '%s\n' "$follow_status" >"$FOLLOW_LOG_STATUS_FILE"
		exit "$follow_status"
	) &
	FOLLOW_LOG_PID=$!
	sleep 2
	if [ -f "$FOLLOW_LOG_STATUS_FILE" ] || ! kill -0 "$FOLLOW_LOG_PID" >/dev/null 2>&1; then
		fail "protected log follower for Pod $follow_pod exited before the destructive window"
	fi
}

finish_follow_logs() {
	follow_context=$1
	[ -n "$FOLLOW_LOG_PID" ] || fail "protected log follower is not running"
	follow_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$follow_deadline" ] && [ ! -f "$FOLLOW_LOG_STATUS_FILE" ]; do
		sleep 1
	done
	[ -f "$FOLLOW_LOG_STATUS_FILE" ] ||
		fail "protected log follower did not reach natural EOF after $follow_context"
	follow_status=$(tr -d '[:space:]' <"$FOLLOW_LOG_STATUS_FILE")
	printf '%s\n' "$follow_status" | grep -Eq '^[0-9]+$' ||
		fail "protected log follower wrote an invalid exit status after $follow_context"
	if ! wait "$FOLLOW_LOG_PID"; then
		[ "$follow_status" -eq 0 ] ||
			fail "protected log follower failed after $follow_context"
	fi
	[ "$follow_status" -eq 0 ] || fail "protected log follower failed after $follow_context"
	FOLLOW_LOG_PID=
	scan_fault_file "$FOLLOW_LOG_FILE" "$follow_context"
	if [ "$FOLLOW_LOG_RECORD_POD" -eq 1 ] && [ -n "$FOLLOW_LOG_POD_UID" ]; then
		record_audited_uid "$AUDITED_FAULT_PODS_FILE" "$FOLLOW_LOG_POD_UID"
	fi
	FOLLOW_LOG_FILE=
	FOLLOW_LOG_STATUS_FILE=
	FOLLOW_LOG_POD_UID=
	FOLLOW_LOG_RECORD_POD=0
}

assert_fault_audit_complete() {
	for audit_kind in jobs pods; do
		case "$audit_kind" in
		jobs) audit_watch=$WORK_DIR/watch-jobs.jsonl; audit_ledger=$AUDITED_FAULT_JOBS_FILE ;;
		pods) audit_watch=$WORK_DIR/watch-pods.jsonl; audit_ledger=$AUDITED_FAULT_PODS_FILE ;;
		esac
		jq -r -s '[.[] | select(.type == "ADDED") | .object.metadata.uid] | unique[]' \
			"$audit_watch" | while IFS= read -r watched_uid; do
			[ -n "$watched_uid" ] || continue
			grep -Fx "$watched_uid" "$audit_ledger" >/dev/null ||
				fail "fault-test $audit_kind UID $watched_uid was never credential-audited"
		done
	done
}

audit_protected_terminal_job() {
	protected_job_name=$1
	protected_job_uid=$2
	protected_pod_uid=$3
	protected_job_object=$(k -n "$TEST_NAMESPACE" get job "$protected_job_name" -o json)
	printf '%s\n' "$protected_job_object" | jq -e \
		--arg uid "$protected_job_uid" '
      .metadata.uid == $uid and
      (.status.conditions // [] |
        any((.type == "Complete" or .type == "Failed") and .status == "True"))
    ' >/dev/null || fail "protected Job $protected_job_name did not become terminal with its original UID"
	printf '%s\n' "$protected_job_object" >"$RESOURCE_FILE"
	scan_fault_file "$RESOURCE_FILE" "protected terminal Job $protected_job_name"
	grep -Fx "$protected_pod_uid" "$AUDITED_FAULT_PODS_FILE" >/dev/null ||
		fail "protected Job $protected_job_name was terminal before its exact Pod log stream was audited"
	record_audited_uid "$AUDITED_FAULT_JOBS_FILE" "$protected_job_uid"
	record_audited_uid "$SHARED_AUDITED_JOBS_FILE" "$protected_job_uid"
}

record_fault_jobs_for_parent() {
	jq -c -s '
      [.[] | select(.type == "ADDED") | .object] |
      unique_by(.metadata.uid)[] | {
        uid: .metadata.uid,
        name: .metadata.name,
        schema: (.metadata.labels["operator.ptah.dev/schema"] // ""),
        operation: (.metadata.labels["operator.ptah.dev/operation"] // "")
      }
    ' "$WORK_DIR/watch-jobs.jsonl" | while IFS= read -r observed_job; do
		observed_uid=$(printf '%s\n' "$observed_job" | jq -er '.uid')
		if ! grep -F "\"uid\":\"${observed_uid}\"" "$SHARED_OBSERVED_JOBS_FILE" >/dev/null 2>&1; then
			printf '%s\n' "$observed_job" >>"$SHARED_OBSERVED_JOBS_FILE"
		fi
	done
}

record_initial_job_list_for_parent() {
	initial_job_list=$1
	jq -c '
      .items[] | {
        uid: .metadata.uid,
        name: .metadata.name,
        schema: (.metadata.labels["operator.ptah.dev/schema"] // ""),
        operation: (.metadata.labels["operator.ptah.dev/operation"] // "")
      }
    ' "$initial_job_list" | while IFS= read -r observed_job; do
		observed_uid=$(printf '%s\n' "$observed_job" | jq -er '.uid')
		if ! grep -F "\"uid\":\"${observed_uid}\"" "$SHARED_OBSERVED_JOBS_FILE" >/dev/null 2>&1; then
			printf '%s\n' "$observed_job" >>"$SHARED_OBSERVED_JOBS_FILE"
		fi
	done
}

load_ready_manager_leader() {
	previous_uid=${1:-}
	leader_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$leader_deadline" ]; do
		leader_holder=$(k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" \
			-o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
		leader_pod_name=$(printf '%s\n' "$leader_holder" | sed 's/_[^_]*$//')
		if [ -n "$leader_pod_name" ] &&
			leader_pod=$(k -n "$OPERATOR_NAMESPACE" get pod "$leader_pod_name" -o json 2>/dev/null) &&
			printf '%s\n' "$leader_pod" | jq -e \
				--arg release "$HELM_RELEASE" \
				--arg previousUID "$previous_uid" '
          .metadata.deletionTimestamp == null and
          .metadata.uid != $previousUID and
          .metadata.labels["app.kubernetes.io/instance"] == $release and
          .metadata.labels["app.kubernetes.io/component"] == "controller" and
          any(.status.conditions[]?; .type == "Ready" and .status == "True")
        ' >/dev/null; then
			MANAGER_POD_NAME=$leader_pod_name
			MANAGER_POD_UID=$(printf '%s\n' "$leader_pod" | jq -er '.metadata.uid')
			return 0
		fi
		sleep 1
	done
	fail "timed out waiting for the manager leader Lease to identify a ready replacement Pod"
}

ready_manager_pod_uids() {
	expected_replicas=$1
	jq -ce --argjson replicas "$expected_replicas" '
      [.items[] | select(.metadata.deletionTimestamp == null)] as $live |
      select(
        ($live | length) == $replicas and
        all($live[]; any(.status.conditions[]?; .type == "Ready" and .status == "True")) and
        ([$live[].metadata.uid] | unique | length) == $replicas
      ) |
      [$live[].metadata.uid] | sort
    '
}

load_ready_manager_pod_uids() {
	manager_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$manager_deadline" ]; do
		manager_replicas=$(k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" \
			-o jsonpath='{.spec.replicas}' 2>/dev/null || true)
		manager_pods=$(k -n "$OPERATOR_NAMESPACE" get pods \
			-l "app.kubernetes.io/name=ptah-operator,app.kubernetes.io/instance=${HELM_RELEASE},app.kubernetes.io/component=controller" \
			-o json 2>/dev/null || true)
		if printf '%s\n' "$manager_replicas" | grep -Eq '^[1-9][0-9]*$' &&
			MANAGER_POD_UIDS=$(printf '%s\n' "$manager_pods" |
				ready_manager_pod_uids "$manager_replicas"); then
			return 0
		fi
		sleep 1
	done
	fail "timed out waiting for every manager replica to have one ready non-terminating Pod"
}

manager_pods_replaced() {
	old_uids=$1
	new_uids=$2
	jq -en --argjson old "$old_uids" --argjson new "$new_uids" '
      ($old | length) == ($new | length) and
      ($old | length) > 0 and
      all($old[]; . as $uid | ($new | index($uid)) == null)
    '
}

assert_manager_pods_replaced() {
	manager_pods_replaced "$1" "$2" >/dev/null ||
		fail "manager rollout retained or lost a controller Pod UID"
}

database_url_for() {
	url_engine=$1
	url_database=$2
	case "$url_engine" in
	postgresql) base_secret=$PG_BASE_SECRET ;;
	mysql) base_secret=$MYSQL_BASE_SECRET ;;
	*) fail "unsupported URL engine $url_engine" ;;
	esac
	base_url=$(secret_url "$base_secret")
	new_url=$(replace_database_url_path "$base_url" "$url_database")
	if [ -z "$new_url" ] || [ "$new_url" = "$base_url" ]; then
		fail "could not derive an isolated $url_engine database URL"
	fi
	printf '%s\n' "$new_url"
}

replace_database_url_path() {
	original_url=$1
	replacement_database=$2
	case "$original_url" in
	*\?*)
		url_path=${original_url%%\?*}
		url_suffix="?${original_url#*\?}"
		;;
	*\#*)
		url_path=${original_url%%\#*}
		url_suffix="#${original_url#*\#}"
		;;
	*)
		url_path=$original_url
		url_suffix=
		;;
	esac
	case "$url_path" in
	*://*/?*) url_prefix=${url_path%/*} ;;
	*) fail "database URL does not contain a non-empty database path" ;;
	esac
	printf '%s/%s%s\n' "$url_prefix" "$replacement_database" "$url_suffix"
}

create_url_secret() {
	url_engine=$1
	url_database=$2
	url_secret=$3
	url_alias=${4:-fqdn}
	url_value=$(database_url_for "$url_engine" "$url_database")
	case "$url_alias" in
	fqdn) ;;
	short)
		case "$url_engine" in
		postgresql) url_service=$PG_SERVICE ;;
		mysql) url_service=$MYSQL_SERVICE ;;
		esac
		aliased_url=$(printf '%s' "$url_value" |
			sed "s/${url_service}\\.${TEST_NAMESPACE}\\.svc\\.cluster\\.local/${url_service}/")
		[ "$aliased_url" != "$url_value" ] || fail "database URL alias did not change the route spelling"
		url_value=$aliased_url
		;;
	*) fail "unsupported database URL alias mode $url_alias" ;;
	esac
	printf '%s' "$url_value" >"$URL_VALUE_FILE"
	chmod 600 "$URL_VALUE_FILE"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$url_secret" \
		--rawfile url "$URL_VALUE_FILE" '
      {
        apiVersion: "v1", kind: "Secret",
        metadata: {namespace: $namespace, name: $name},
        type: "Opaque", stringData: {url: $url}
      }
    ' >"$RESOURCE_FILE"
	chmod 600 "$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	: >"$RESOURCE_FILE"
	: >"$URL_VALUE_FILE"
}

create_database() {
	database_engine=$1
	database_name=$2
	database_secret=$3
	assert_safe_name "$database_name"
	case "$database_engine" in
	postgresql)
		database_exists=$(pg_query postgres "SELECT count(*) FROM pg_database WHERE datname='${database_name}'" | tr -d '[:space:]')
		[ "$database_exists" = 0 ] || fail "PostgreSQL database $database_name already exists"
		pg_query postgres "CREATE DATABASE ${database_name}" >/dev/null
		create_url_secret postgresql "$database_name" "$database_secret"
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec -i deployment/"$PG_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1"' \
			sh "$database_name" <"$ROOT_DIR/testdata/e2e/postgresql-v3.sql" >"$LOG_FILE" 2>&1 ||
			fail "could not seed PostgreSQL database $database_name"
		;;
	mysql)
		database_exists=$(mysql_root_query mysql "SELECT count(*) FROM information_schema.schemata WHERE schema_name='${database_name}'" | tr -d '[:space:]')
		[ "$database_exists" = 0 ] || fail "MySQL database $database_name already exists"
		mysql_root_query mysql "CREATE DATABASE ${database_name}; GRANT ALL PRIVILEGES ON ${database_name}.* TO '${MYSQL_APP_USER}'@'%'; FLUSH PRIVILEGES" >/dev/null
		create_url_secret mysql "$database_name" "$database_secret"
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec -i deployment/"$MYSQL_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1"' \
			sh "$database_name" <"$ROOT_DIR/testdata/e2e/mysql-v3.sql" >"$LOG_FILE" 2>&1 ||
			fail "could not seed MySQL database $database_name"
		;;
	*) fail "unsupported database engine $database_engine" ;;
	esac
	: >"$LOG_FILE"
}

database_column_count() {
	column_engine=$1
	column_database=$2
	column_name=$3
	case "$column_engine" in
	postgresql)
		column_result=$(pg_query "$column_database" "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='${column_name}'")
		;;
	mysql)
		column_result=$(mysql_root_query "$column_database" "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND column_name='${column_name}'")
		;;
	*) fail "unsupported column engine $column_engine" ;;
	esac
	printf '%s' "$column_result" | tr -d '[:space:]'
}

assert_database_column() {
	column_engine=$1
	column_database=$2
	column_name=$3
	column_expected=$4
	column_actual=$(database_column_count "$column_engine" "$column_database" "$column_name")
	[ "$column_actual" = "$column_expected" ] ||
		fail "$column_engine database $column_database column $column_name count is $column_actual, expected $column_expected"
}

postgres_schema_fingerprint() {
	fingerprint_database=$1
	pg_query "$fingerprint_database" "
    SELECT md5(COALESCE(string_agg(
      table_schema || '.' || table_name || '.' || column_name || ':' || data_type || ':' || is_nullable,
      ',' ORDER BY table_schema, table_name, ordinal_position), ''))
    FROM information_schema.columns
    WHERE table_schema = 'public'"
}

mysql_schema_fingerprint() {
	fingerprint_database=$1
	mysql_root_query "$fingerprint_database" "
    SELECT MD5(COALESCE(GROUP_CONCAT(entry ORDER BY entry SEPARATOR '\n'), ''))
    FROM (
      SELECT CONCAT(
        'column:', table_name, ':', LPAD(ordinal_position, 6, '0'), ':', column_name, ':',
        column_type, ':', is_nullable, ':', COALESCE(column_default, '<NULL>'), ':',
        extra, ':', COALESCE(collation_name, '<NULL>'), ':', COALESCE(generation_expression, '<NULL>')) AS entry
      FROM information_schema.columns
      WHERE table_schema = DATABASE()
      UNION ALL
      SELECT CONCAT(
        'index:', table_name, ':', index_name, ':', LPAD(seq_in_index, 6, '0'), ':',
        non_unique, ':', COALESCE(column_name, '<NULL>'), ':', COALESCE(sub_part, 0), ':',
        index_type, ':', COALESCE(collation, '<NULL>'), ':', COALESCE(expression, '<NULL>')) AS entry
      FROM information_schema.statistics
      WHERE table_schema = DATABASE()
    ) AS managed_schema"
}

create_watch_heartbeat_lease() {
	jq -n \
		--arg namespace "$OPERATOR_NAMESPACE" \
		--arg name e2e-fault-watch-heartbeat '
      {
        apiVersion: "coordination.k8s.io/v1", kind: "Lease",
        metadata: {
          namespace: $namespace, name: $name,
          labels: {"operator.ptah.dev/e2e-purpose": "watch-heartbeat"}
        },
        spec: {}
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

watch_heartbeat_loop() {
	heartbeat_pod=$1
	heartbeat_stop_file=$2
	heartbeat_status_file=$3
	heartbeat_error_file=$4
	heartbeat_sequence=0
	heartbeat_status=0
	while [ ! -f "$heartbeat_stop_file" ]; do
		heartbeat_marker="fault-watch-$$-${heartbeat_sequence}-$(date +%s)"
		k --request-timeout=8s -n "$TEST_NAMESPACE" annotate job e2e-fault-push-postgresql \
			operator.ptah.dev/e2e-watch-heartbeat="$heartbeat_marker" --overwrite >/dev/null 2>>"$heartbeat_error_file" &
		heartbeat_job_pid=$!
		k --request-timeout=8s -n "$TEST_NAMESPACE" annotate pod "$heartbeat_pod" \
			operator.ptah.dev/e2e-watch-heartbeat="$heartbeat_marker" --overwrite >/dev/null 2>>"$heartbeat_error_file" &
		heartbeat_pod_pid=$!
		k --request-timeout=8s -n "$TEST_NAMESPACE" annotate ptahschema e2e-suspended-schema \
			operator.ptah.dev/e2e-watch-heartbeat="$heartbeat_marker" --overwrite >/dev/null 2>>"$heartbeat_error_file" &
		heartbeat_schema_pid=$!
		k --request-timeout=8s -n "$TEST_NAMESPACE" annotate ptahschemaapproval e2e-approval \
			operator.ptah.dev/e2e-watch-heartbeat="$heartbeat_marker" --overwrite >/dev/null 2>>"$heartbeat_error_file" &
		heartbeat_approval_pid=$!
		k --request-timeout=8s -n "$OPERATOR_NAMESPACE" annotate lease e2e-fault-watch-heartbeat \
			operator.ptah.dev/e2e-watch-heartbeat="$heartbeat_marker" --overwrite >/dev/null 2>>"$heartbeat_error_file" &
		heartbeat_lease_pid=$!
		heartbeat_update_status=0
		for heartbeat_update_pid in \
			"$heartbeat_job_pid" "$heartbeat_pod_pid" "$heartbeat_schema_pid" \
			"$heartbeat_approval_pid" "$heartbeat_lease_pid"; do
			wait "$heartbeat_update_pid" || heartbeat_update_status=1
		done
		if [ "$heartbeat_update_status" -ne 0 ]; then
			heartbeat_status=1
			break
		fi
		heartbeat_sequence=$((heartbeat_sequence + 1))
		heartbeat_sleep=0
		while [ "$heartbeat_sleep" -lt 5 ] && [ ! -f "$heartbeat_stop_file" ]; do
			sleep 1
			heartbeat_sleep=$((heartbeat_sleep + 1))
		done
	done
	printf '%s\n' "$heartbeat_status" >"$heartbeat_status_file"
	return "$heartbeat_status"
}

start_watch_heartbeat() {
	[ "$WATCH_HEARTBEAT_STOPPED" -eq 0 ] || fail "watch heartbeat cannot be restarted"
	heartbeat_pod=$(k -n "$TEST_NAMESPACE" get pods \
		-l "app.kubernetes.io/name=${PG_SERVICE},app.kubernetes.io/component=e2e-database" \
		-o json | jq -er '
      [.items[] | select(.metadata.deletionTimestamp == null)] |
      if length == 1 then .[0].metadata.name else error("expected one PostgreSQL database Pod") end
    ')
	k -n "$TEST_NAMESPACE" get job e2e-fault-push-postgresql >/dev/null
	k -n "$TEST_NAMESPACE" get ptahschema e2e-suspended-schema >/dev/null
	k -n "$TEST_NAMESPACE" get ptahschemaapproval e2e-approval >/dev/null
	k -n "$OPERATOR_NAMESPACE" get lease e2e-fault-watch-heartbeat >/dev/null
	WATCH_HEARTBEAT_STOP_FILE=$WORK_DIR/watch-heartbeat.stop
	WATCH_HEARTBEAT_STATUS_FILE=$WORK_DIR/watch-heartbeat.status
	WATCH_HEARTBEAT_ERROR_FILE=$WORK_DIR/watch-heartbeat.err
	: >"$WATCH_HEARTBEAT_ERROR_FILE"
	watch_heartbeat_loop "$heartbeat_pod" "$WATCH_HEARTBEAT_STOP_FILE" \
		"$WATCH_HEARTBEAT_STATUS_FILE" "$WATCH_HEARTBEAT_ERROR_FILE" &
	WATCH_HEARTBEAT_PID=$!
}

stop_watch_heartbeat() {
	[ -n "$WATCH_HEARTBEAT_PID" ] || return 0
	: >"$WATCH_HEARTBEAT_STOP_FILE"
	heartbeat_deadline=$(($(date +%s) + 15))
	while [ "$(date +%s)" -lt "$heartbeat_deadline" ] && [ ! -f "$WATCH_HEARTBEAT_STATUS_FILE" ]; do
		sleep 1
	done
	[ -f "$WATCH_HEARTBEAT_STATUS_FILE" ] || fail "Kubernetes watch heartbeat did not stop cleanly"
	wait "$WATCH_HEARTBEAT_PID" || fail "Kubernetes watch heartbeat failed"
	[ "$(tr -d '[:space:]' <"$WATCH_HEARTBEAT_STATUS_FILE")" = 0 ] ||
		fail "Kubernetes watch heartbeat reported an update failure"
	[ ! -s "$WATCH_HEARTBEAT_ERROR_FILE" ] ||
		fail "Kubernetes watch heartbeat wrote an API error"
	WATCH_HEARTBEAT_PID=
	WATCH_HEARTBEAT_STOPPED=1
}

supervise_watch() {
	watch_path=$1
	watch_initial_rv=$2
	watch_stem=$3
	watch_stop_file=$4
	watch_status_file=$5
	watch_error_file=$6
	watch_frame_directory=$WORK_DIR/watch-${watch_stem}.frames
	watch_current_rv=$watch_initial_rv
	watch_frame_sequence=0
	watch_segment_sequence=0
	watch_supervisor_status=0
	while [ ! -f "$watch_stop_file" ]; do
		watch_segment_number=$(printf '%012d' "$watch_segment_sequence")
		watch_segment_pipe=$WORK_DIR/watch-${watch_stem}-segment-${watch_segment_number}.pipe
		watch_segment_error=$WORK_DIR/watch-${watch_stem}-segment-${watch_segment_number}.err
		mkfifo "$watch_segment_pipe"
		k --request-timeout=0s get --raw \
			"${watch_path}?watch=true&allowWatchBookmarks=true&resourceVersion=${watch_current_rv}&timeoutSeconds=30" \
			>"$watch_segment_pipe" 2>"$watch_segment_error" &
		watch_request_pid=$!
		watch_segment_event_count=0
		while IFS= read -r watch_event; do
			if ! printf '%s\n' "$watch_event" | jq -e '
              type == "object" and (.type | type == "string") and
              (.object | type == "object") and .type != "ERROR" and
              (.object.metadata.resourceVersion | type == "string" and length > 0)
            ' >/dev/null 2>&1; then
				printf '%s\n' 'watch segment emitted an invalid or error event' >"$watch_error_file"
				watch_supervisor_status=1
				continue
			fi
			watch_frame_number=$(printf '%012d' "$watch_frame_sequence")
			watch_frame_temporary=$watch_frame_directory/.frame-${watch_frame_number}.tmp
			watch_frame_final=$watch_frame_directory/frame-${watch_frame_number}.json
			printf '%s\n' "$watch_event" >"$watch_frame_temporary"
			mv -- "$watch_frame_temporary" "$watch_frame_final"
			watch_event_rv=$(printf '%s\n' "$watch_event" | jq -er '.object.metadata.resourceVersion')
			watch_current_rv=$watch_event_rv
			watch_frame_sequence=$((watch_frame_sequence + 1))
			watch_segment_event_count=$((watch_segment_event_count + 1))
		done <"$watch_segment_pipe"
		watch_request_failed=0
		if ! wait "$watch_request_pid"; then
			watch_request_failed=1
		fi
		rm -f -- "$watch_segment_pipe"
		if [ "$watch_request_failed" -ne 0 ]; then
			sed -n 'p' "$watch_segment_error" >"$watch_error_file"
			watch_supervisor_status=1
			break
		fi
		if [ -s "$watch_segment_error" ]; then
			sed -n 'p' "$watch_segment_error" >"$watch_error_file"
			watch_supervisor_status=1
			break
		fi
		if [ "$watch_supervisor_status" -ne 0 ]; then
			break
		fi
		if [ "$watch_segment_event_count" -eq 0 ] && [ ! -f "$watch_stop_file" ]; then
			printf '%s\n' 'watch segment made no progress while its heartbeat was required' >"$watch_error_file"
			watch_supervisor_status=1
			break
		fi
		rm -f -- "$watch_segment_error"
		watch_segment_sequence=$((watch_segment_sequence + 1))
	done
	printf '%s\n' "$watch_supervisor_status" >"$watch_status_file"
	return "$watch_supervisor_status"
}

materialize_watch() {
	materialize_stem=$1
	materialize_output=$2
	materialize_directory=$WORK_DIR/watch-${materialize_stem}.frames
	materialize_build=$materialize_output.build
	: >"$materialize_build"
	for materialize_frame in "$materialize_directory"/frame-*.json; do
		[ -f "$materialize_frame" ] || continue
		sed -n 'p' "$materialize_frame" >>"$materialize_build"
	done
	mv -- "$materialize_build" "$materialize_output"
}

snapshot_watch() {
	snapshot_stem=$1
	WATCH_SNAPSHOT_FILE=$WORK_DIR/watch-${snapshot_stem}-snapshot.jsonl
	materialize_watch "$snapshot_stem" "$WATCH_SNAPSHOT_FILE"
}

establish_watch_barrier() {
	barrier_stem=$1
	barrier_namespace=$2
	barrier_resource=$3
	barrier_name=$4
	barrier_marker="fault-${barrier_stem}-$$-${WATCH_BARRIER_SEQUENCE}-$(date +%s)"
	WATCH_BARRIER_SEQUENCE=$((WATCH_BARRIER_SEQUENCE + 1))
	barrier_object=$(k -n "$barrier_namespace" annotate "$barrier_resource" "$barrier_name" \
		operator.ptah.dev/e2e-watch-barrier="$barrier_marker" --overwrite -o json)
	barrier_uid=$(printf '%s\n' "$barrier_object" | jq -er '.metadata.uid')
	barrier_rv=$(printf '%s\n' "$barrier_object" | jq -er '.metadata.resourceVersion')
	barrier_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$barrier_deadline" ]; do
		snapshot_watch "$barrier_stem"
		if jq -s -e \
			--arg uid "$barrier_uid" \
			--arg rv "$barrier_rv" \
			--arg marker "$barrier_marker" '
          any(.[].object;
            .metadata.uid == $uid and .metadata.resourceVersion == $rv and
            .metadata.annotations["operator.ptah.dev/e2e-watch-barrier"] == $marker)
        ' "$WATCH_SNAPSHOT_FILE" >/dev/null; then
			return 0
		fi
		maybe_audit_fault_runtime
		sleep 1
	done
	fail "$barrier_stem watch did not cross its exact API resourceVersion barrier"
}

start_watch() {
	watch_resource=$1
	watch_list_namespace=$2
	watch_path=$3
	watch_stem=$4
	watch_file="$WORK_DIR/watch-${watch_stem}.jsonl"
	watch_error_file="$WORK_DIR/watch-${watch_stem}.err"
	watch_frame_directory=$WORK_DIR/watch-${watch_stem}.frames
	watch_frame_status=$WORK_DIR/watch-${watch_stem}.frame-status
	watch_stop_file=$WORK_DIR/watch-${watch_stem}.stop
	watch_initial_list=$WORK_DIR/watch-${watch_stem}-initial-list.json
	mkdir -m 700 "$watch_frame_directory"
	: >"$watch_file"
	: >"$watch_error_file"
	if [ -n "$watch_list_namespace" ]; then
		k -n "$watch_list_namespace" get "$watch_resource" -o json >"$watch_initial_list"
	else
		k get "$watch_resource" -o json >"$watch_initial_list"
	fi
	watch_rv=$(jq -er '.metadata.resourceVersion' "$watch_initial_list")
	if [ "$watch_stem" = jobs ]; then
		# The list resourceVersion and the following watch form one gap-free
		# boundary. Persist the list side so a short-lived Job created between
		# the parent audit and this fault watch cannot disappear from the final
		# no-replay and no-Apply UID accounting.
		record_initial_job_list_for_parent "$watch_initial_list"
	fi
	supervise_watch "$watch_path" "$watch_rv" "$watch_stem" \
		"$watch_stop_file" "$watch_frame_status" "$watch_error_file" &
	watch_supervisor_pid=$!
	WATCH_PIDS="${WATCH_PIDS} ${watch_supervisor_pid}"
	WATCH_FRAME_STATUS_FILES="${WATCH_FRAME_STATUS_FILES} ${watch_frame_status}"
	WATCH_STOP_FILES="${WATCH_STOP_FILES} ${watch_stop_file}"
	WATCH_STEMS="${WATCH_STEMS} ${watch_stem}"
	WATCH_FILES="${WATCH_FILES} ${watch_file}"
	WATCH_ERROR_FILES="${WATCH_ERROR_FILES} ${watch_error_file}"
}

start_watches() {
	start_watch jobs.batch "$TEST_NAMESPACE" "/apis/batch/v1/namespaces/${TEST_NAMESPACE}/jobs" jobs
	start_watch pods "$TEST_NAMESPACE" "/api/v1/namespaces/${TEST_NAMESPACE}/pods" pods
	start_watch ptahschemas.operator.ptah.dev "$TEST_NAMESPACE" "/apis/operator.ptah.dev/v1alpha1/namespaces/${TEST_NAMESPACE}/ptahschemas" schemas
	start_watch ptahschemaapprovals.operator.ptah.dev "$TEST_NAMESPACE" "/apis/operator.ptah.dev/v1alpha1/namespaces/${TEST_NAMESPACE}/ptahschemaapprovals" approvals
	start_watch leases.coordination.k8s.io "$OPERATOR_NAMESPACE" "/apis/coordination.k8s.io/v1/namespaces/${OPERATOR_NAMESPACE}/leases" leases
}

assert_watches_alive() {
	if [ -n "$WATCH_HEARTBEAT_PID" ]; then
		kill -0 "$WATCH_HEARTBEAT_PID" >/dev/null 2>&1 ||
			fail "the Kubernetes watch heartbeat exited before the assertions completed"
		[ ! -f "$WATCH_HEARTBEAT_STATUS_FILE" ] ||
			fail "the Kubernetes watch heartbeat reported an early exit"
		[ ! -s "$WATCH_HEARTBEAT_ERROR_FILE" ] ||
			fail "the Kubernetes watch heartbeat reported an API error"
	fi
	for watch_pid in $WATCH_PIDS; do
		kill -0 "$watch_pid" >/dev/null 2>&1 || fail "a Kubernetes resourceVersion watch exited before the assertions completed"
	done
	for watch_frame_status in $WATCH_FRAME_STATUS_FILES; do
		[ ! -f "$watch_frame_status" ] || fail "a Kubernetes rotating watch supervisor exited before the assertions completed"
	done
	for watch_error_file in $WATCH_ERROR_FILES; do
		[ ! -s "$watch_error_file" ] || fail "a Kubernetes resourceVersion watch reported an error"
	done
}

stop_watches() {
	assert_watches_alive
	for watch_stop_file in $WATCH_STOP_FILES; do
		: >"$watch_stop_file"
	done
	frame_deadline=$(($(date +%s) + 45))
	for watch_frame_status in $WATCH_FRAME_STATUS_FILES; do
		while [ "$(date +%s)" -lt "$frame_deadline" ] && [ ! -f "$watch_frame_status" ]; do
			sleep 1
		done
		[ -f "$watch_frame_status" ] || fail "a Kubernetes rotating watch did not reach natural segment EOF"
		[ "$(tr -d '[:space:]' <"$watch_frame_status")" = 0 ] ||
			fail "a Kubernetes rotating watch reported a framing failure"
	done
	for watch_pid in $WATCH_PIDS; do
		wait "$watch_pid" || fail "a Kubernetes rotating watch supervisor failed"
	done
	for watch_stem in $WATCH_STEMS; do
		materialize_watch "$watch_stem" "$WORK_DIR/watch-${watch_stem}.jsonl"
	done
	WATCH_PIDS=
}

validate_watch_file() {
	watch_file=$1
	[ -s "$watch_file" ] || fail "Kubernetes watch produced no JSONL events: $watch_file"
	jq -s -e '
      length > 0 and
      all(.[]; type == "object" and (.type | type == "string") and (.object | type == "object")) and
      all(.[]; .type != "ERROR")
    ' "$watch_file" >/dev/null || fail "Kubernetes watch stream is not valid error-free JSONL: $watch_file"
}

watch_added_uid_count() {
	watch_schema=$1
	watch_operation=$2
	snapshot_watch jobs
	jq -s \
		--arg schema "$watch_schema" \
		--arg operation "$watch_operation" '
      [.[] |
        select(.type == "ADDED") |
        select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
        select(.object.metadata.labels["operator.ptah.dev/operation"] == $operation) |
        .object.metadata.uid] | unique | length
	' "$WATCH_SNAPSHOT_FILE"
}

watch_added_pod_uid_count() {
	watch_schema=$1
	watch_operation=$2
	snapshot_watch pods
	jq -s \
		--arg schema "$watch_schema" \
		--arg operation "$watch_operation" '
      [.[] |
        select(.type == "ADDED") |
        select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
        select(.object.metadata.labels["operator.ptah.dev/operation"] == $operation) |
        .object.metadata.uid] | unique | length
	' "$WATCH_SNAPSHOT_FILE"
}

checkpoint_operation_watch() {
	checkpoint_schema=$1
	checkpoint_operation=$2
	checkpoint_expected_count=$3
	checkpoint_file=$4
	establish_watch_barrier jobs "$TEST_NAMESPACE" job e2e-fault-push-postgresql
	snapshot_watch jobs
	watched_checkpoint_uids=$(jq -c -s \
		--arg schema "$checkpoint_schema" \
		--arg operation "$checkpoint_operation" '
      [.[] |
        select(.type == "ADDED") |
        select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
        select(.object.metadata.labels["operator.ptah.dev/operation"] == $operation) |
        .object.metadata.uid] | unique | sort
    ' "$WATCH_SNAPSHOT_FILE")
	[ "$(printf '%s\n' "$watched_checkpoint_uids" | jq 'length')" -eq "$checkpoint_expected_count" ] ||
		fail "$checkpoint_schema has an unexpected historical $checkpoint_operation Job count at its exact watch checkpoint"
	printf '%s\n' "$watched_checkpoint_uids" >"$checkpoint_file"
}

checkpoint_schema_job_watch() {
	checkpoint_schema=$1
	checkpoint_file=$2
	establish_watch_barrier jobs "$TEST_NAMESPACE" job e2e-fault-push-postgresql
	snapshot_watch jobs
	watched_checkpoint_uids=$(jq -c -s \
		--arg schema "$checkpoint_schema" '
      [.[] |
        select(.type == "ADDED") |
        select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
        .object.metadata.uid] | unique | sort
    ' "$WATCH_SNAPSHOT_FILE")
	[ "$(printf '%s\n' "$watched_checkpoint_uids" | jq 'length')" -gt 0 ] ||
		fail "$checkpoint_schema has no historical lifecycle Jobs at its exact deletion checkpoint"
	printf '%s\n' "$watched_checkpoint_uids" >"$checkpoint_file"
}

watch_new_uid_count() {
	watch_schema=$1
	watch_operation=$2
	watch_checkpoint=$3
	snapshot_watch jobs
	if watch_count_result=$(jq -s \
			--slurpfile before "$watch_checkpoint" \
			--arg schema "$watch_schema" \
			--arg operation "$watch_operation" '
          [.[] |
            select(.type == "ADDED") |
            select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
            select(.object.metadata.labels["operator.ptah.dev/operation"] == $operation) |
            .object.metadata.uid as $uid |
            select(($before[0] | index($uid)) == null) |
            $uid] | unique | length
		' "$WATCH_SNAPSHOT_FILE" 2>/dev/null); then
		printf '%s\n' "$watch_count_result"
		return 0
	fi
	fail "could not read a complete Kubernetes Job watch snapshot for $watch_schema $watch_operation"
}

single_new_watched_job_uid() {
	new_uid_schema=$1
	new_uid_operation=$2
	new_uid_checkpoint=$3
	snapshot_watch jobs
	jq -er -s \
		--slurpfile before "$new_uid_checkpoint" \
		--arg schema "$new_uid_schema" \
		--arg operation "$new_uid_operation" '
      [.[] |
        select(.type == "ADDED") |
        select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
        select(.object.metadata.labels["operator.ptah.dev/operation"] == $operation) |
        .object.metadata.uid as $uid |
        select(($before[0] | index($uid)) == null) |
        $uid] | unique |
      if length == 1 then .[0] else error("expected exactly one new watched Job UID") end
    ' "$WATCH_SNAPSHOT_FILE"
}

wait_for_one_new_watched_job() {
	new_watch_schema=$1
	new_watch_operation=$2
	new_watch_checkpoint=$3
	new_watch_description=$4
	new_watch_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$new_watch_deadline" ]; do
		maybe_audit_fault_runtime
		new_watch_count=$(watch_new_uid_count "$new_watch_schema" "$new_watch_operation" "$new_watch_checkpoint")
		[ "$new_watch_count" -le 1 ] ||
			fail "$new_watch_schema created more than one new $new_watch_operation Job after the exact checkpoint"
		[ "$new_watch_count" -ne 1 ] || return 0
		sleep 1
	done
	fail "timed out waiting for $new_watch_description"
}

assert_no_overlapping_operation_pods() {
	jq -s -e '
      reduce .[] as $event (
        {active: {}, valid: true};
        ($event.object.metadata.labels["operator.ptah.dev/schema"] // "") as $schema |
        ($event.object.metadata.uid // "") as $uid |
        ($event.object.status.phase // "") as $phase |
        if $schema == "" or $uid == "" then .
        elif $event.type == "DELETED" or $phase == "Succeeded" or $phase == "Failed" then
          del(.active[$schema][$uid])
        else
          .active[$schema][$uid] = true |
          if ((.active[$schema] // {}) | length) > 1 then .valid = false else . end
        end
      ) | .valid
    ' "$WORK_DIR/watch-pods.jsonl" >/dev/null ||
		fail "Kubernetes Pod watch proves overlapping operation Pods for one PtahSchema"
}

assert_no_overlapping_operation_jobs() {
	jq -s -e '
      reduce .[] as $event (
        {active: {}, valid: true};
        ($event.object.metadata.labels["operator.ptah.dev/schema"] // "") as $schema |
        ($event.object.metadata.uid // "") as $uid |
        ($event.object.status.conditions // [] |
          any((.type == "Complete" or .type == "Failed") and .status == "True")) as $terminal |
        if $schema == "" or $uid == "" then .
        elif $event.type == "DELETED" or $terminal then
          del(.active[$schema][$uid])
        else
          .active[$schema][$uid] = true |
          if ((.active[$schema] // {}) | length) > 1 then .valid = false else . end
        end
      ) | .valid
    ' "$WORK_DIR/watch-jobs.jsonl" >/dev/null ||
		fail "Kubernetes Job watch proves overlapping operation Jobs for one PtahSchema"
}

assert_initial_read_chain_watch_order() {
	chain_schema=$1
	snapshot_watch jobs
	jq -s -e \
		--arg schema "$chain_schema" '
      def added($operation):
        [to_entries[] | select(
          .value.type == "ADDED" and
          .value.object.metadata.labels["operator.ptah.dev/schema"] == $schema and
          .value.object.metadata.labels["operator.ptah.dev/operation"] == $operation)];
      added("resolve") as $resolves |
      added("verify") as $verifies |
      added("observe") as $observes |
      ($resolves | sort_by(.key) | .[0]) as $resolveAdded |
      ($verifies | sort_by(.key) | .[0]) as $verifyAdded |
      ($observes | sort_by(.key) | .[0]) as $observeAdded |
      (to_entries | map(select(
        .value.object.metadata.uid == $resolveAdded.value.object.metadata.uid and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True")))) | .[0]) as $resolveComplete |
      (to_entries | map(select(
        .value.object.metadata.uid == $verifyAdded.value.object.metadata.uid and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True")))) | .[0]) as $verifyComplete |
      $resolveAdded != null and $verifyAdded != null and $observeAdded != null and
      $resolveComplete != null and $verifyComplete != null and
      ($resolveAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] | length) > 0 and
      ($verifyAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] | length) > 0 and
      ($observeAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] | length) > 0 and
      $resolveAdded.key < $resolveComplete.key and
      $resolveComplete.key < $verifyAdded.key and
      $verifyAdded.key < $verifyComplete.key and
      $verifyComplete.key < $observeAdded.key
    ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
		fail "$chain_schema did not complete Resolve and Verify before its first database Observe Job"
}

watch_has_outcome_unknown() {
	unknown_schema=$1
	unknown_operation=$2
	snapshot_watch schemas
	jq -s -e \
		--arg schema "$unknown_schema" \
		--arg operation "$unknown_operation" '
      any(.[];
        .type == "MODIFIED" and
        .object.metadata.name == $schema and
        .object.status.pendingObservation.outcome == "OutcomeUnknown" and
        .object.status.pendingObservation.applyOperationID == $operation and
        (.object.status.conditions | any(
          .type == "Applying" and .status == "False" and .reason == "OutcomeUnknown")))
	' "$WATCH_SNAPSHOT_FILE" >/dev/null 2>&1
}

wait_for_outcome_unknown_watch() {
	unknown_schema=$1
	unknown_operation=$2
	unknown_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$unknown_deadline" ]; do
		maybe_audit_fault_runtime
		if watch_has_outcome_unknown "$unknown_schema" "$unknown_operation"; then
			return 0
		fi
		sleep 1
	done
	fail "schema watch never recorded durable OutcomeUnknown proof for $unknown_schema"
}

wait_for_apply_binding_in_schema_watch() {
	binding_schema=$1
	binding_job_uid=$2
	binding_description=$3
	binding_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$binding_deadline" ]; do
		maybe_audit_fault_runtime
		snapshot_watch schemas
		binding_count=$(jq -r -s \
			--arg schema "$binding_schema" \
			--arg jobUID "$binding_job_uid" '
          [.[] | select(
            .object.metadata.name == $schema and
            .object.status.activeOperation.type == "Apply" and
            .object.status.activeOperation.jobUID == $jobUID and
            (.object.status.activeOperation.id | length) > 0 and
            ((.object.status.activeOperation.leaseEpoch // "") |
              test("^v1-[0-9a-f]{32}$")) and
            .object.status.activeOperation.coordinationDigest ==
              .object.status.target.coordinationDigest)] |
          unique_by([.object.status.activeOperation.id,
            .object.status.activeOperation.leaseEpoch]) | length
        ' "$WATCH_SNAPSHOT_FILE")
		case "$binding_count" in
		0) ;;
		1)
			WATCH_ACTIVE_OPERATION_ID=$(jq -er -s \
				--arg schema "$binding_schema" \
				--arg jobUID "$binding_job_uid" '
                  [.[] | select(
                    .object.metadata.name == $schema and
                    .object.status.activeOperation.type == "Apply" and
                    .object.status.activeOperation.jobUID == $jobUID)] |
                  map(.object.status.activeOperation.id) | unique | .[0]
                ' "$WATCH_SNAPSHOT_FILE")
			WATCH_ACTIVE_LEASE_EPOCH=$(jq -er -s \
				--arg schema "$binding_schema" \
				--arg jobUID "$binding_job_uid" \
				--arg operation "$WATCH_ACTIVE_OPERATION_ID" '
                  [.[] | select(
                    .object.metadata.name == $schema and
                    .object.status.activeOperation.type == "Apply" and
                    .object.status.activeOperation.jobUID == $jobUID and
                    .object.status.activeOperation.id == $operation)] |
                  map(.object.status.activeOperation.leaseEpoch) | unique | .[0]
                ' "$WATCH_SNAPSHOT_FILE")
			return 0
			;;
		*) fail "$binding_description recorded multiple active-operation bindings" ;;
		esac
		sleep 1
	done
	fail "timed out waiting for $binding_description in the schema watch"
}

wait_for_schema() {
	wait_schema=$1
	wait_expression=$2
	wait_description=$3
	wait_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		maybe_audit_fault_runtime
		if wait_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$wait_schema" -o json 2>/dev/null); then
			if printf '%s\n' "$wait_object" | jq -e "$wait_expression" >/dev/null; then
				return 0
			fi
		fi
		sleep 1
	done
	fail "timed out waiting for $wait_schema: $wait_description"
}

wait_for_absence() {
	absent_resource=$1
	absent_name=$2
	absent_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$absent_deadline" ]; do
		maybe_audit_fault_runtime
		if absent_result=$(k -n "$TEST_NAMESPACE" get "$absent_resource" "$absent_name" \
			-o name --ignore-not-found 2>/dev/null); then
			[ -n "$absent_result" ] || return 0
		else
			sleep 1
			continue
		fi
		if [ -z "$absent_result" ]; then
			return 0
		fi
		sleep 1
	done
	fail "timed out waiting for $absent_resource $absent_name to be deleted"
}

wait_for_watch_count_greater_than() {
	count_schema=$1
	count_operation=$2
	count_before=$3
	count_description=$4
	count_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$count_deadline" ]; do
		maybe_audit_fault_runtime
		count_now=$(watch_added_uid_count "$count_schema" "$count_operation")
		[ "$count_now" -gt "$count_before" ] && return 0
		sleep 1
	done
	fail "timed out waiting for $count_description"
}

wait_for_publisher_job() {
	publisher_job=$1
	publisher_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$publisher_deadline" ]; do
		maybe_audit_fault_runtime
		publisher_object=$(k -n "$TEST_NAMESPACE" get job "$publisher_job" -o json 2>/dev/null || true)
		if [ -n "$publisher_object" ]; then
			if printf '%s\n' "$publisher_object" | jq -e '.status.conditions // [] | any(.type == "Complete" and .status == "True")' >/dev/null; then
				return 0
			fi
			if printf '%s\n' "$publisher_object" | jq -e '.status.conditions // [] | any(.type == "Failed" and .status == "True")' >/dev/null; then
				fail "schema publisher Job $publisher_job failed"
			fi
		fi
		sleep 1
	done
	fail "timed out waiting for schema publisher Job $publisher_job"
}

wait_for_operation_job_terminal() {
	terminal_schema=$1
	terminal_operation=$2
	terminal_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$terminal_deadline" ]; do
		maybe_audit_fault_runtime
		terminal_jobs=$(k -n "$TEST_NAMESPACE" get jobs \
			-l "operator.ptah.dev/schema=${terminal_schema},operator.ptah.dev/operation=${terminal_operation}" \
			-o json)
		terminal_count=$(printf '%s\n' "$terminal_jobs" | jq '.items | length')
		[ "$terminal_count" -le 1 ] ||
			fail "$terminal_schema created more than one $terminal_operation Job"
		if [ "$terminal_count" -eq 1 ] && printf '%s\n' "$terminal_jobs" | jq -e '
        .items[0].status.conditions // [] |
        any((.type == "Complete" or .type == "Failed") and .status == "True")
      ' >/dev/null; then
			TERMINAL_OPERATION_ID=$(printf '%s\n' "$terminal_jobs" | jq -r '.items[0].metadata.annotations["operator.ptah.dev/operation-id"]')
			TERMINAL_JOB_NAME=$(printf '%s\n' "$terminal_jobs" | jq -er '.items[0].metadata.name')
			TERMINAL_JOB_UID=$(printf '%s\n' "$terminal_jobs" | jq -er '.items[0].metadata.uid')
			if [ -z "$TERMINAL_OPERATION_ID" ] || [ "$TERMINAL_OPERATION_ID" = null ]; then
				fail "$terminal_schema terminal Job lacks its operation identity"
			fi
			return 0
		fi
		sleep 1
	done
	fail "timed out waiting for the terminal $terminal_operation Job for $terminal_schema"
}

wait_for_exact_job_terminal() {
	exact_job_name=$1
	exact_job_uid=$2
	exact_job_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$exact_job_deadline" ]; do
		maybe_audit_fault_runtime
		exact_job_object=$(k -n "$TEST_NAMESPACE" get job "$exact_job_name" -o json 2>/dev/null || true)
		if [ -n "$exact_job_object" ]; then
			printf '%s\n' "$exact_job_object" | jq -e \
				--arg uid "$exact_job_uid" '.metadata.uid == $uid' >/dev/null ||
				fail "Job $exact_job_name changed UID before reaching a terminal state"
			if printf '%s\n' "$exact_job_object" | jq -e '
          .status.conditions // [] |
          any((.type == "Complete" or .type == "Failed") and .status == "True")
        ' >/dev/null; then
				return 0
			fi
		fi
		sleep 1
	done
	fail "timed out waiting for exact Job $exact_job_name to become terminal"
}

capture_exact_job_result() {
	result_job_name=$1
	result_job_uid=$2
	result_operation=$3
	result_output=$4
	wait_for_exact_job_terminal "$result_job_name" "$result_job_uid"
	result_job_object=$(k -n "$TEST_NAMESPACE" get job "$result_job_name" -o json)
	printf '%s\n' "$result_job_object" | jq -e \
		--arg uid "$result_job_uid" \
		--arg operation "$result_operation" '
      .metadata.uid == $uid and
      .metadata.labels["operator.ptah.dev/operation"] == $operation and
      (.metadata.annotations["operator.ptah.dev/operation-id"] | length > 0) and
      .spec.podReplacementPolicy == "Failed" and .spec.backoffLimit == 0 and
      (.status.conditions // [] | any(.type == "Complete" and .status == "True"))
    ' >/dev/null || fail "exact $result_operation Job $result_job_name did not transport one result"
	FAULT_RESULT_OPERATION_ID=$(printf '%s\n' "$result_job_object" |
		jq -er '.metadata.annotations["operator.ptah.dev/operation-id"]')
	result_pods=$(k -n "$TEST_NAMESPACE" get pods -l "job-name=${result_job_name}" -o json)
	FAULT_RESULT_POD=$(printf '%s\n' "$result_pods" | jq -er \
		--arg uid "$result_job_uid" \
		--arg operationID "$FAULT_RESULT_OPERATION_ID" '
      [.items[] |
        select(.metadata.ownerReferences // [] | any(
          .kind == "Job" and .uid == $uid and .controller == true)) |
        select(.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID)] |
      if length == 1 then .[0].metadata.name
      else error("exact Job does not own one operation-bound Pod") end
    ')
	FAULT_RESULT_POD_UID=$(printf '%s\n' "$result_pods" | jq -er \
		--arg name "$FAULT_RESULT_POD" '
      .items[] | select(.metadata.name == $name) |
      ([.status.initContainerStatuses // [], .status.containerStatuses // []] | add) as $statuses |
      if all($statuses[]; (.restartCount // 0) == 0) and
        ([.status.containerStatuses[] |
          select(.name == "ptah" and .state.terminated.exitCode == 0)] | length) == 1
      then .metadata.uid else error("result Pod did not terminate exactly once") end
    ')
	result_log=$WORK_DIR/exact-result.log
	if ! k -n "$TEST_NAMESPACE" logs pod/"$FAULT_RESULT_POD" -c ptah >"$result_log" 2>&1; then
		fail "could not read exact $result_operation result from $FAULT_RESULT_POD"
	fi
	scan_fault_file "$result_log" "the exact $result_operation runner transport"
	"$RESULT_ASSERT_BINARY" \
		--logs "$result_log" \
		--operation "$result_operation" \
		--operation-id "$FAULT_RESULT_OPERATION_ID" >"$result_output"
	chmod 600 "$result_output"
	scan_fault_file "$result_output" "the validated $result_operation runner result"
	jq -e \
		--arg operation "$result_operation" \
		--arg operationID "$FAULT_RESULT_OPERATION_ID" '
      .protocolVersion == 4 and .operation == $operation and
      .operationId == $operationID and .truncation == null
    ' "$result_output" >/dev/null ||
		fail "runner result lost its protocol-v4 binding or complete-output guarantee"
	result_schema=$(printf '%s\n' "$result_job_object" |
		jq -er '.metadata.labels["operator.ptah.dev/schema"]')
	result_binding_deadline=$(deadline_from_now)
	result_binding_proven=false
	while [ "$(date +%s)" -lt "$result_binding_deadline" ]; do
		snapshot_watch schemas
		result_schema_watch=$WATCH_SNAPSHOT_FILE
		snapshot_watch jobs
		result_job_watch=$WATCH_SNAPSHOT_FILE
		if jq -s -e \
			--arg schema "$result_schema" \
			--arg operation "$result_operation" \
			--arg operationID "$FAULT_RESULT_OPERATION_ID" \
			--arg jobUID "$result_job_uid" \
			--slurpfile jobs "$result_job_watch" '
          any(.[].object;
            .metadata.name == $schema and
            (.status.activeOperation.type // "" | ascii_downcase) == $operation and
            .status.activeOperation.id == $operationID and
            .status.activeOperation.jobUID == $jobUID) and
          any($jobs[];
            .type == "ADDED" and .object.metadata.uid == $jobUID and
            .object.metadata.labels["operator.ptah.dev/schema"] == $schema and
            .object.metadata.labels["operator.ptah.dev/operation"] == $operation and
            .object.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID)
		        ' "$result_schema_watch" >/dev/null; then
			result_binding_proven=true
			break
		fi
		maybe_audit_fault_runtime
		sleep 1
	done
	[ "$result_binding_proven" = true ] ||
		fail "exact $result_operation result was not bound to the persisted active operation and ADDED Job"
	record_audited_uid "$AUDITED_FAULT_JOBS_FILE" "$result_job_uid"
	record_audited_uid "$AUDITED_FAULT_PODS_FILE" "$FAULT_RESULT_POD_UID"
	record_audited_uid "$SHARED_AUDITED_JOBS_FILE" "$result_job_uid"
}

assert_successful_apply_result() {
	apply_result_schema=$1
	apply_result_operation_id=$2
	apply_result_job_uid=$3
	apply_result_pod_uid=$4
	apply_result_file=$5
	apply_result_deadline=$(deadline_from_now)
	apply_result_proven=false
	while [ "$(date +%s)" -lt "$apply_result_deadline" ]; do
		snapshot_watch schemas
		if jq -s -e \
			--arg schema "$apply_result_schema" \
			--arg operationID "$apply_result_operation_id" \
			--arg jobUID "$apply_result_job_uid" \
			--arg podUID "$apply_result_pod_uid" \
			--slurpfile result "$apply_result_file" '
          $result[0] as $result |
          [.[] | select(.object.metadata.name == $schema) | .object] as $schemas |
          ($schemas | map(select(
            .status.activeOperation.type == "Apply" and
            .status.activeOperation.id == $operationID and
            .status.activeOperation.jobUID == $jobUID and
            .status.plan != null)) | .[0]) as $apply |
          ($schemas | map(select(
            .status.pendingObservation.outcome == "ApplySucceeded" and
            .status.pendingObservation.applyOperationID == $operationID and
            .status.pendingObservation.applyJobUID == $jobUID)) | .[0]) as $pendingObject |
          $pendingObject.status.pendingObservation as $pending |
          $apply != null and $pendingObject != null and
          $apply.status.activeOperation.dispatchNotAfter != null and
          $apply.status.activeOperation.executionNotAfter != null and
          $apply.status.activeOperation.executionNotAfter ==
            $apply.status.activeOperation.dispatchNotAfter and
          $apply.status.activeOperation.terminationGracePeriodSeconds == 30 and
          $result.operationId == $apply.status.activeOperation.id and
          $result.error == null and $result.childExitCode == 0 and
          ($result.mutationStarted // false) == true and
          ($result.uncertain // false) == false and
          ($result.planOutcome // "") == "" and $result.truncation == null and
          $result.planContentDigest == $apply.status.plan.contentDigest and
          $result.coordinationDigest == $apply.status.activeOperation.coordinationDigest and
          $result.coordinationDigest == $apply.status.plan.coordinationDigest and
          $result.targetIdentityDigest == $apply.status.activeOperation.targetIdentityDigest and
          $result.targetIdentityDigest == $apply.status.plan.targetIdentityDigest and
          $pending.plan == $apply.status.plan and
          $pending.applyJobName == $apply.status.activeOperation.jobName and
          $pending.applyJobUID == $jobUID and
          $pending.applyPodUIDs == [$podUID] and $pending.applyPodCount == 1 and
          $pending.coordinationDigest == $result.coordinationDigest and
          $pending.plan.targetIdentityDigest == $result.targetIdentityDigest and
          $pending.leaseEpoch == $apply.status.activeOperation.leaseEpoch and
          $pendingObject.status.phase == "VerifyingConvergence" and
          $pendingObject.status.applied == null and
          $pendingObject.status.pendingLockRelease == null and
          all($pendingObject.status.conditions[];
            if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
        ' "$WATCH_SNAPSHOT_FILE" >/dev/null; then
			apply_result_proven=true
			break
		fi
		maybe_audit_fault_runtime
		sleep 1
	done
	[ "$apply_result_proven" = true ] ||
		fail "$apply_result_schema did not harvest one exact successful Apply result into its persisted proof snapshot"
}

assert_fault_convergence_result_pair() {
	converged_schema=$1
	converged_observe_checkpoint=$2
	converged_plan_checkpoint=$3
	converged_apply_operation=$4
	converged_lease_name=$5
	converged_lease_uid=$6
	converged_lease_holder=$7
	converged_lease_epoch=$8
	converged_blocked_schema=${9:-}
	[ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 1 ] ||
		fail "$converged_schema convergence proof started without its scheduling barrier"
	wait_for_one_new_watched_job "$converged_schema" observe "$converged_observe_checkpoint" \
		"one exact post-Apply Observe Job for $converged_schema"
	CONVERGED_OBSERVE_JOB_UID=$(single_new_watched_job_uid \
		"$converged_schema" observe "$converged_observe_checkpoint")
	assert_read_workload_blocked "$CONVERGED_OBSERVE_JOB_UID" \
		"the post-Apply Observe Job for $converged_schema"
	pause_controller_status_writes
	stop_read_workload_barrier
	converged_observe_job=$(k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${converged_schema},operator.ptah.dev/operation=observe" \
		-o json | jq -er --arg uid "$CONVERGED_OBSERVE_JOB_UID" '
          [.items[] | select(.metadata.uid == $uid)] |
          if length == 1 then .[0].metadata.name
          else error("post-Apply Observe Job UID is not live exactly once") end
        ')
	converged_observe_result=$WORK_DIR/${converged_schema}-post-apply-observe-result.json
	capture_exact_job_result "$converged_observe_job" "$CONVERGED_OBSERVE_JOB_UID" \
		observe "$converged_observe_result"
	converged_held_observe=$(k -n "$TEST_NAMESPACE" get ptahschema "$converged_schema" -o json)
	printf '%s\n' "$converged_held_observe" | jq -e \
		--arg observeUID "$CONVERGED_OBSERVE_JOB_UID" \
		--arg observeOperation "$FAULT_RESULT_OPERATION_ID" \
		--arg applyOperation "$converged_apply_operation" \
		--arg leaseEpoch "$converged_lease_epoch" '
      .status.activeOperation.type == "Observe" and
      .status.activeOperation.id == $observeOperation and
      .status.activeOperation.jobUID == $observeUID and
      .status.activeOperation.leaseEpoch == $leaseEpoch and
      .status.pendingObservation.outcome == "ApplySucceeded" and
      .status.pendingObservation.applyOperationID == $applyOperation and
      .status.pendingObservation.leaseEpoch == $leaseEpoch and
      (.status.pendingObservation.planRequired // false) == false and
      .status.phase == "VerifyingConvergence" and .status.applied == null and
      .status.pendingLockRelease == null and
      all(.status.conditions[];
        if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
    ' >/dev/null ||
		fail "$converged_schema harvested its Observe while status writes were held"
	assert_lease_held_without_release "$converged_lease_name" "$converged_lease_uid" \
		"$converged_lease_holder" "$converged_lease_epoch"
	if [ -n "$converged_blocked_schema" ]; then
		[ "$(watch_added_uid_count "$converged_blocked_schema" apply)" -eq 0 ] ||
			fail "$converged_blocked_schema dispatched while $converged_schema retained its Observe proof Lease"
	fi
	start_read_workload_barrier
	resume_controller_status_writes ||
		fail "could not restore controller status-write RBAC after the Observe/Lease proof boundary"
	wait_for_one_new_watched_job "$converged_schema" plan "$converged_plan_checkpoint" \
		"one exact post-Apply Plan Job for $converged_schema"
	CONVERGED_PLAN_JOB_UID=$(single_new_watched_job_uid \
		"$converged_schema" plan "$converged_plan_checkpoint")
	assert_read_workload_blocked "$CONVERGED_PLAN_JOB_UID" \
		"the post-Apply Plan Job for $converged_schema"
	pause_controller_status_writes
	stop_read_workload_barrier
	converged_plan_job=$(k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${converged_schema},operator.ptah.dev/operation=plan" \
		-o json | jq -er --arg uid "$CONVERGED_PLAN_JOB_UID" '
          [.items[] | select(.metadata.uid == $uid)] |
          if length == 1 then .[0].metadata.name
          else error("post-Apply Plan Job UID is not live exactly once") end
        ')
	converged_plan_result=$WORK_DIR/${converged_schema}-post-apply-plan-result.json
	capture_exact_job_result "$converged_plan_job" "$CONVERGED_PLAN_JOB_UID" \
		plan "$converged_plan_result"
	converged_held_schema=$(k -n "$TEST_NAMESPACE" get ptahschema "$converged_schema" -o json)
	printf '%s\n' "$converged_held_schema" | jq -e \
		--arg planUID "$CONVERGED_PLAN_JOB_UID" \
		--arg planOperation "$FAULT_RESULT_OPERATION_ID" \
		--arg applyOperation "$converged_apply_operation" \
		--arg leaseEpoch "$converged_lease_epoch" '
      .status.activeOperation.type == "Plan" and
      .status.activeOperation.id == $planOperation and
      .status.activeOperation.jobUID == $planUID and
      .status.activeOperation.leaseEpoch == $leaseEpoch and
      .status.pendingObservation.outcome == "ApplySucceeded" and
      .status.pendingObservation.applyOperationID == $applyOperation and
      .status.pendingObservation.leaseEpoch == $leaseEpoch and
      .status.pendingObservation.planRequired == true and
      .status.phase == "VerifyingConvergence" and .status.applied == null and
      .status.pendingLockRelease == null and
      all(.status.conditions[];
        if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
    ' >/dev/null ||
		fail "$converged_schema harvested its Plan while status writes were held"
	assert_lease_held_without_release "$converged_lease_name" "$converged_lease_uid" \
		"$converged_lease_holder" "$converged_lease_epoch"
	if [ -n "$converged_blocked_schema" ]; then
		[ "$(watch_added_uid_count "$converged_blocked_schema" apply)" -eq 0 ] ||
			fail "$converged_blocked_schema dispatched while $converged_schema retained its Plan proof Lease"
		# Keep the contender's Apply Pod unscheduled after the proof holder
		# releases. This creates a deterministic boundary for binding the
		# contender to its exact Lease before its stale-plan preflight runs.
		start_read_workload_barrier
	fi
	resume_controller_status_writes ||
		fail "could not restore controller status-write RBAC after the Plan/Lease proof boundary"
	wait_for_in_sync "$converged_schema"
	converged_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$converged_schema" -o json)
	printf '%s\n' "$converged_schema_object" | jq -e \
		--slurpfile observe "$converged_observe_result" \
		--slurpfile plan "$converged_plan_result" '
          $observe[0] as $observe | $plan[0] as $plan |
          .status.phase == "InSync" and .status.pendingObservation == null and
          .status.activeOperation == null and .status.plan == null and
          (.status.conditions | any(
            .type == "InSync" and .status == "True" and .reason == "ScopedConverged")) and
          $observe.error == null and $observe.childExitCode == 0 and
          $observe.stdout == "" and ($observe.observedDrift // false) == false and
          ($observe.highestDriftSeverity // "") == "" and
          ($observe.driftFindingCount // 0) == 0 and
          ($observe.observedDialect | IN("postgres", "postgresql")) and
          $observe.coordinationDigest == .status.target.coordinationDigest and
          $observe.targetIdentityDigest == .status.target.identityDigest and
          $observe.driftReportDigest == .status.target.driftReportDigest and
          $plan.error == null and $plan.childExitCode == 0 and
          $plan.planOutcome == "NoChanges" and $plan.stdout == "" and
          ($plan.planContentDigest // "") == "" and
          $plan.coordinationDigest == .status.target.coordinationDigest and
          $plan.targetIdentityDigest == .status.target.identityDigest
        ' >/dev/null ||
		fail "$converged_schema did not carry exact successful protocol-v4 Observe and NoChanges Plan results"
	snapshot_watch jobs
	jq -s -e \
		--arg observeUID "$CONVERGED_OBSERVE_JOB_UID" \
		--arg planUID "$CONVERGED_PLAN_JOB_UID" '
          (to_entries | map(select(
            .value.object.metadata.uid == $observeUID and
            (.value.object.status.conditions // [] | any(
              .type == "Complete" and .status == "True"))) | .key) | min) as $observeComplete |
          (to_entries | map(select(
            .value.type == "ADDED" and .value.object.metadata.uid == $planUID) | .key) | min) as $planAdded |
          $observeComplete != null and $planAdded != null and $observeComplete < $planAdded
        ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
		fail "$converged_schema Plan was not dispatched after its exact completed Observe result"
}

capture_uncertain_read_proof_pair() {
	uncertain_schema=$1
	uncertain_apply_operation=$2
	uncertain_apply_job_name=$3
	uncertain_apply_job_uid=$4
	uncertain_apply_pod_uid=$5
	uncertain_lease_name=$6
	uncertain_lease_uid=$7
	uncertain_lease_holder=$8
	uncertain_lease_epoch=$9
	uncertain_observe_checkpoint=${10}
	uncertain_plan_checkpoint=${11}
	uncertain_apply_pod_count=${12:-1}
	case "$uncertain_apply_pod_count" in
	0) uncertain_apply_pod_uids='[]' ;;
	1) uncertain_apply_pod_uids=$(jq -cn --arg uid "$uncertain_apply_pod_uid" '[$uid]') ;;
	*) fail "$uncertain_schema recovery proof has an unsupported Apply Pod evidence count" ;;
	esac
	[ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 1 ] ||
		fail "$uncertain_schema recovery proof started without its scheduling barrier"
	[ "$STATUS_RBAC_PAUSED" -eq 0 ] ||
		fail "$uncertain_schema recovery proof started while status writes were still denied"
	wait_for_outcome_unknown_watch "$uncertain_schema" "$uncertain_apply_operation"
	wait_for_one_new_watched_job "$uncertain_schema" observe "$uncertain_observe_checkpoint" \
		"one exact read-only Observe Job after the uncertain Apply for $uncertain_schema"
	UNCERTAIN_OBSERVE_JOB_UID=$(single_new_watched_job_uid \
		"$uncertain_schema" observe "$uncertain_observe_checkpoint")
	assert_read_workload_blocked "$UNCERTAIN_OBSERVE_JOB_UID" \
		"the recovery Observe Job for $uncertain_schema"
	pause_controller_status_writes
	stop_read_workload_barrier
	uncertain_observe_job=$(k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${uncertain_schema},operator.ptah.dev/operation=observe" \
		-o json | jq -er --arg uid "$UNCERTAIN_OBSERVE_JOB_UID" '
          [.items[] | select(.metadata.uid == $uid)] |
          if length == 1 then .[0].metadata.name
          else error("recovery Observe Job UID is not live exactly once") end
        ')
	UNCERTAIN_OBSERVE_RESULT=$WORK_DIR/${uncertain_schema}-uncertain-observe-result.json
	capture_exact_job_result "$uncertain_observe_job" "$UNCERTAIN_OBSERVE_JOB_UID" \
		observe "$UNCERTAIN_OBSERVE_RESULT"
	UNCERTAIN_OBSERVE_OPERATION_ID=$FAULT_RESULT_OPERATION_ID
	uncertain_held_observe=$(k -n "$TEST_NAMESPACE" get ptahschema "$uncertain_schema" -o json)
	printf '%s\n' "$uncertain_held_observe" | jq -e \
		--arg observeUID "$UNCERTAIN_OBSERVE_JOB_UID" \
		--arg observeOperation "$UNCERTAIN_OBSERVE_OPERATION_ID" \
		--arg applyOperation "$uncertain_apply_operation" \
		--arg applyJobName "$uncertain_apply_job_name" \
		--arg applyJobUID "$uncertain_apply_job_uid" \
		--argjson applyPodUIDs "$uncertain_apply_pod_uids" \
		--argjson applyPodCount "$uncertain_apply_pod_count" \
		--arg leaseEpoch "$uncertain_lease_epoch" '
      .status.activeOperation.type == "Observe" and
      .status.activeOperation.id == $observeOperation and
      .status.activeOperation.jobUID == $observeUID and
      .status.activeOperation.leaseEpoch == $leaseEpoch and
      .status.pendingObservation.outcome == "OutcomeUnknown" and
      .status.pendingObservation.applyOperationID == $applyOperation and
      .status.pendingObservation.applyJobName == $applyJobName and
      .status.pendingObservation.applyJobUID == $applyJobUID and
      (.status.pendingObservation.applyPodUIDs // []) == $applyPodUIDs and
      (.status.pendingObservation.applyPodCount // 0) == $applyPodCount and
      .status.pendingObservation.leaseEpoch == $leaseEpoch and
      (.status.pendingObservation.planRequired // false) == false and
      .status.phase == "VerifyingConvergence" and .status.applied == null and
      .status.pendingLockRelease == null and
      all(.status.conditions[];
        if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
    ' >/dev/null ||
		fail "$uncertain_schema did not retain its exact uncertain Apply snapshot through Observe"
	assert_lease_held_without_release "$uncertain_lease_name" "$uncertain_lease_uid" \
		"$uncertain_lease_holder" "$uncertain_lease_epoch"

	start_read_workload_barrier
	resume_controller_status_writes ||
		fail "could not restore controller status-write RBAC after $uncertain_schema Observe proof"
	wait_for_one_new_watched_job "$uncertain_schema" plan "$uncertain_plan_checkpoint" \
		"one exact read-only Plan Job after the uncertain Apply for $uncertain_schema"
	UNCERTAIN_PLAN_JOB_UID=$(single_new_watched_job_uid \
		"$uncertain_schema" plan "$uncertain_plan_checkpoint")
	assert_read_workload_blocked "$UNCERTAIN_PLAN_JOB_UID" \
		"the recovery Plan Job for $uncertain_schema"
	pause_controller_status_writes
	stop_read_workload_barrier
	uncertain_plan_job=$(k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${uncertain_schema},operator.ptah.dev/operation=plan" \
		-o json | jq -er --arg uid "$UNCERTAIN_PLAN_JOB_UID" '
          [.items[] | select(.metadata.uid == $uid)] |
          if length == 1 then .[0].metadata.name
          else error("recovery Plan Job UID is not live exactly once") end
        ')
	UNCERTAIN_PLAN_RESULT=$WORK_DIR/${uncertain_schema}-uncertain-plan-result.json
	capture_exact_job_result "$uncertain_plan_job" "$UNCERTAIN_PLAN_JOB_UID" \
		plan "$UNCERTAIN_PLAN_RESULT"
	UNCERTAIN_PLAN_OPERATION_ID=$FAULT_RESULT_OPERATION_ID
	uncertain_held_plan=$(k -n "$TEST_NAMESPACE" get ptahschema "$uncertain_schema" -o json)
	printf '%s\n' "$uncertain_held_plan" | jq -e \
		--arg planUID "$UNCERTAIN_PLAN_JOB_UID" \
		--arg planOperation "$UNCERTAIN_PLAN_OPERATION_ID" \
		--arg applyOperation "$uncertain_apply_operation" \
		--arg applyJobName "$uncertain_apply_job_name" \
		--arg applyJobUID "$uncertain_apply_job_uid" \
		--argjson applyPodUIDs "$uncertain_apply_pod_uids" \
		--argjson applyPodCount "$uncertain_apply_pod_count" \
		--arg leaseEpoch "$uncertain_lease_epoch" '
      .status.activeOperation.type == "Plan" and
      .status.activeOperation.id == $planOperation and
      .status.activeOperation.jobUID == $planUID and
      .status.activeOperation.leaseEpoch == $leaseEpoch and
      .status.pendingObservation.outcome == "OutcomeUnknown" and
      .status.pendingObservation.applyOperationID == $applyOperation and
      .status.pendingObservation.applyJobName == $applyJobName and
      .status.pendingObservation.applyJobUID == $applyJobUID and
      (.status.pendingObservation.applyPodUIDs // []) == $applyPodUIDs and
      (.status.pendingObservation.applyPodCount // 0) == $applyPodCount and
      .status.pendingObservation.leaseEpoch == $leaseEpoch and
      .status.pendingObservation.planRequired == true and
      .status.phase == "VerifyingConvergence" and .status.applied == null and
      .status.pendingLockRelease == null and
      all(.status.conditions[];
        if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
    ' >/dev/null ||
		fail "$uncertain_schema did not retain its exact uncertain Apply snapshot through Plan"
	assert_lease_held_without_release "$uncertain_lease_name" "$uncertain_lease_uid" \
		"$uncertain_lease_holder" "$uncertain_lease_epoch"
	resume_controller_status_writes ||
		fail "could not restore controller status-write RBAC after $uncertain_schema Plan proof"
}

wait_for_manual_drift_contract() {
	manual_schema=$1
	manual_approval=$2
	manual_operation=$3
	manual_old_plan_uid=$4
	manual_old_actual_fingerprint=$5
	manual_observe_checkpoint=$6
	manual_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$manual_deadline" ]; do
		maybe_audit_fault_runtime
		manual_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$manual_schema" -o json 2>/dev/null || true)
		manual_approval_object=$(k -n "$TEST_NAMESPACE" get ptahschemaapproval "$manual_approval" -o json 2>/dev/null || true)
		if [ -n "$manual_schema_object" ] && [ -n "$manual_approval_object" ]; then
			manual_new_plan=$(printf '%s\n' "$manual_schema_object" | jq -r '.status.plan.name // ""')
			if [ -n "$manual_new_plan" ] && \
				printf '%s\n' "$manual_schema_object" | jq -e \
				--arg oldUID "$manual_old_plan_uid" '
          .status.pendingObservation == null and .status.activeOperation == null and
          .status.pendingLockRelease == null and .status.applied == null and
          .status.phase == "AwaitingApproval" and .status.plan.uid != $oldUID and
          .status.plan.approval == null
		' >/dev/null && printf '%s\n' "$manual_approval_object" | jq -e '
		  (.status.conditions | any(
		    .type == "Consumed" and .status == "True" and
		    .reason == "DispatchCommitted")) and
		  (.status.conditions | any(
		    .type == "Stale" and .status == "True" and
		    .reason == "PlanNoLongerCurrent"))
		' >/dev/null && [ "$(watch_new_uid_count "$manual_schema" observe "$manual_observe_checkpoint")" -eq 1 ]; then
				manual_new_plan_object=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$manual_new_plan" -o json 2>/dev/null || true)
				if [ -n "$manual_new_plan_object" ] && printf '%s\n' "$manual_new_plan_object" | jq -e \
					--arg oldActual "$manual_old_actual_fingerprint" \
					--arg oldUID "$manual_old_plan_uid" '
              .metadata.uid != $oldUID and
              .spec.actualStateFingerprint != $oldActual and
					(.spec.actualStateFingerprint | test("^sha256:[0-9a-f]{64}$"))
				' >/dev/null; then
					watch_has_outcome_unknown "$manual_schema" "$manual_operation" ||
						fail "$manual_schema lost its required OutcomeUnknown history"
					return 0
				fi
			fi
		fi
		sleep 1
	done
	fail "manual drift did not converge through OutcomeUnknown to one fresh unapproved plan"
}

assert_approval_consumed() {
	consumed_approval=$1
	consumed_plan_uid=$2
	k -n "$TEST_NAMESPACE" get ptahschemaapproval "$consumed_approval" -o json |
		jq -e \
			--arg planUID "$consumed_plan_uid" '
        .spec.planRef.uid == $planUID and
        (.status.conditions | any(
          .type == "Consumed" and .status == "True" and .reason == "DispatchCommitted")) and
        (.status.conditions | any(
          .type == "Accepted" and .status == "False" and
          .reason == "PlanNoLongerCurrent")) and
        (.status.conditions | any(
          .type == "Stale" and .status == "True" and
          .reason == "PlanNoLongerCurrent"))
      ' >/dev/null || fail "$consumed_approval was not durably consumed by the exact dispatched plan"
}

publish_fault_schema() {
	publish_engine=$1
	publish_dialect=$2
	publish_reference=$3
	publish_source="$ROOT_DIR/testdata/e2e/${publish_engine}-fault-v1.sql"
	publish_configmap="e2e-fault-${publish_engine}-source"
	publish_job="e2e-fault-push-${publish_engine}"
	[ -f "$publish_source" ] || fail "fault schema fixture is missing: $publish_source"
	k -n "$TEST_NAMESPACE" create configmap "$publish_configmap" \
		--from-file="schema.sql=${publish_source}" >/dev/null
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$publish_job" \
		--arg image "$EXECUTOR_IMAGE" \
		--arg configMap "$publish_configmap" \
		--arg reference "$publish_reference" \
		--arg dialect "$publish_dialect" \
		--arg registrySecret "$REGISTRY_AUTH_SECRET" \
		--arg pullSecret "$REGISTRY_PULL_SECRET" '
      def secretEnv($name; $key):
        {name: $name, valueFrom: {secretKeyRef: {name: $registrySecret, key: $key}}};
      {
        apiVersion: "batch/v1", kind: "Job",
        metadata: {
          namespace: $namespace, name: $name,
          labels: {"app.kubernetes.io/component": "e2e-fault-schema-publisher"}
        },
        spec: {
          backoffLimit: 0, activeDeadlineSeconds: 300,
          template: {
            metadata: {labels: {"app.kubernetes.io/component": "e2e-fault-schema-publisher"}},
            spec: {
              restartPolicy: "Never", automountServiceAccountToken: false,
              imagePullSecrets: [{name: $pullSecret}],
              securityContext: {
                runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
                seccompProfile: {type: "RuntimeDefault"}
              },
              containers: [{
                name: "publisher", image: $image, imagePullPolicy: "IfNotPresent",
                command: ["/usr/local/bin/ptah"],
                args: [
                  "schema", "push", $reference, "--schema-file", "/schema/schema.sql",
                  "--dialect", $dialect, "--version", "fault-v1", "--plain-http"
                ],
                env: [
                  {name: "HOME", value: "/work"},
                  {name: "TMPDIR", value: "/work"},
                  secretEnv("PTAH_OCI_USERNAME"; "username"),
                  secretEnv("PTAH_OCI_PASSWORD"; "password"),
                  secretEnv("PTAH_OCI_REGISTRY"; "registry")
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
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	wait_for_publisher_job "$publish_job"
	k -n "$TEST_NAMESPACE" logs job/"$publish_job" >"$LOG_FILE" 2>&1 ||
		fail "could not read schema publisher result for $publish_engine"
	publish_digest=$(sed -n 's/^Digest: \(sha256:[0-9a-f]\{64\}\)$/\1/p' "$LOG_FILE" | tail -n 1)
	printf '%s\n' "$publish_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "schema publisher $publish_job did not report an immutable digest"
	: >"$LOG_FILE"
}

publish_credential_principal_artifact() {
	principal_reference=$1
	principal_publisher=e2e-push-credential-principal
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$principal_publisher" \
		--arg image "$FIXTURE_IMAGE" \
		--arg reference "$principal_reference" \
		--arg schemaSecret "$PRINCIPAL_SCHEMA_SECRET" \
		--arg registrySecret "$REGISTRY_AUTH_SECRET" \
		--arg pullSecret "$REGISTRY_PULL_SECRET" '
      def secretEnv($name; $key):
        {name: $name, valueFrom: {secretKeyRef: {name: $registrySecret, key: $key}}};
      {
        apiVersion: "batch/v1", kind: "Job",
        metadata: {
          namespace: $namespace, name: $name,
          labels: {"app.kubernetes.io/component": "e2e-handcrafted-schema-publisher"}
        },
        spec: {
          backoffLimit: 0, activeDeadlineSeconds: 300,
          template: {
            metadata: {labels: {"app.kubernetes.io/component": "e2e-handcrafted-schema-publisher"}},
            spec: {
              restartPolicy: "Never", automountServiceAccountToken: false,
              imagePullSecrets: [{name: $pullSecret}],
              securityContext: {
                runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
                seccompProfile: {type: "RuntimeDefault"}
              },
              containers: [{
                name: "publisher", image: $image, imagePullPolicy: "IfNotPresent",
                command: ["/e2e-handcraft-oci"],
                args: [$reference, "/schema/schema.hcl"],
                env: [
                  secretEnv("PTAH_OCI_USERNAME"; "username"),
                  secretEnv("PTAH_OCI_PASSWORD"; "password"),
                  secretEnv("PTAH_OCI_REGISTRY"; "registry")
                ],
                securityContext: {
                  allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                  capabilities: {drop: ["ALL"]}
                },
                volumeMounts: [{name: "schema", mountPath: "/schema", readOnly: true}]
              }],
              volumes: [{name: "schema", secret: {
                secretName: $schemaSecret,
                items: [{key: "schema.hcl", path: "schema.hcl", mode: 288}]
              }}]
            }
          }
        }
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	wait_for_publisher_job "$principal_publisher"
	k -n "$TEST_NAMESPACE" get job "$principal_publisher" -o json | jq -e \
		--arg image "$FIXTURE_IMAGE" \
		--arg reference "$principal_reference" \
		--arg schemaSecret "$PRINCIPAL_SCHEMA_SECRET" \
		--arg registrySecret "$REGISTRY_AUTH_SECRET" '
      .spec.backoffLimit == 0 and
      .spec.template.spec.automountServiceAccountToken == false and
      (.spec.template.spec.containers | length) == 1 and
      (.spec.template.spec.containers[0] as $container |
        $container.image == $image and
        $container.command == ["/e2e-handcraft-oci"] and
        $container.args == [$reference, "/schema/schema.hcl"] and
        ([$container.env[] | .valueFrom.secretKeyRef.name] | unique) == [$registrySecret]) and
      (.spec.template.spec.volumes | any(
        .name == "schema" and .secret.secretName == $schemaSecret)) and
      ([.spec.template.spec | .. | objects | .secretKeyRef?.name? |
        select(. != null and . != $registrySecret)] | length) == 0
    ' >/dev/null || fail "handcrafted publisher crossed its schema/registry isolation boundary"
	if ! k -n "$TEST_NAMESPACE" logs job/"$principal_publisher" >"$LOG_FILE" 2>&1; then
		fail "could not read the handcrafted OCI publisher result"
	fi
	scan_fault_file "$LOG_FILE" "the handcrafted OCI publisher log"
	principal_digest_count=$(grep -Ec '^Digest: sha256:[0-9a-f]{64}$' "$LOG_FILE" || true)
	[ "$principal_digest_count" -eq 1 ] ||
		fail "handcrafted OCI publisher did not emit exactly one immutable digest"
	PRINCIPAL_ARTIFACT_DIGEST=$(sed -n 's/^Digest: \(sha256:[0-9a-f]\{64\}\)$/\1/p' "$LOG_FILE")
	: >"$LOG_FILE"
}

create_schema() {
	schema_name=$1
	schema_engine=$2
	schema_secret=$3
	schema_reference=$4
	schema_coordination_key=$5
	schema_failure_retry=${6:-5s}
	schema_active_deadline=${7:-$FAULT_ACTIVE_DEADLINE_SECONDS}
	schema_lock_timeout=${8:-60s}
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$schema_name" \
		--arg engine "$schema_engine" \
		--arg secret "$schema_secret" \
		--arg reference "$schema_reference" \
		--arg coordinationKey "$schema_coordination_key" \
		--arg registrySecret "$REGISTRY_AUTH_SECRET" \
		--arg pullSecret "$REGISTRY_PULL_SECRET" \
		--arg failureRetry "$schema_failure_retry" \
		--arg lockTimeout "$schema_lock_timeout" \
		--argjson activeDeadline "$schema_active_deadline" '
      {
        apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
        metadata: {namespace: $namespace, name: $name},
        spec: {
          target: {
            engine: $engine, coordinationKey: $coordinationKey,
            urlFrom: {name: $secret, key: "url"}
          },
          desired: {
            ociRef: $reference,
            registryAuthFrom: {
              name: $registrySecret, mode: "Environment",
              usernameKey: "username", passwordKey: "password", registryKey: "registry"
            },
            verificationPolicyFrom: {name: "e2e-verification-policy", key: "policy.yaml"},
            transport: {plainHTTP: true}
          },
          policy: {
            apply: "OnApproval", allowDestructive: false, driftSeverity: "all",
            lockTimeout: $lockTimeout, transactionMode: "file"
          },
          interval: "1h",
          execution: {
		    activeDeadlineSeconds: $activeDeadline, failureRetryInterval: $failureRetry, connectTimeout: "30s",
            imagePullSecrets: [{name: $pullSecret}]
          }
        }
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

wait_for_plan() {
	plan_schema=$1
	wait_for_schema "$plan_schema" '
      .status.phase == "AwaitingApproval" and
      .status.activeOperation == null and
      .status.plan.name != null
	' "a non-destructive plan awaiting exact approval"
	PLAN_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$plan_schema" -o jsonpath='{.status.plan.name}')
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$PLAN_NAME" -o json |
		jq -e '
      .spec.destructive == false and .spec.statementCount > 0 and
      (.status.conditions | any(.type == "Ready" and .status == "True"))
    ' >/dev/null || fail "$plan_schema did not produce a ready non-destructive plan"
}

create_approval() {
	approval_schema=$1
	approval_name=$2
	approval_schema_uid=$(k -n "$TEST_NAMESPACE" get ptahschema "$approval_schema" -o jsonpath='{.metadata.uid}')
	approval_plan=$(k -n "$TEST_NAMESPACE" get ptahschema "$approval_schema" -o jsonpath='{.status.plan.name}')
	[ -n "$approval_plan" ] || fail "$approval_schema has no plan to approve"
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
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	k -n "$TEST_NAMESPACE" get ptahschemaapproval "$approval_name" -o json |
		jq -e \
			--arg schema "$approval_schema" \
			--arg plan "$approval_plan" \
			--arg fingerprint "$approval_fingerprint" '
      .spec.schemaRef.name == $schema and .spec.planRef.name == $plan and
      .spec.planFingerprint == $fingerprint and
      (.spec.artifactDigest | test("^sha256:[0-9a-f]{64}$")) and
      (.spec.coordinationDigest | test("^sha256:[0-9a-f]{64}$")) and
      .spec.runnerProtocolVersion == 4
    ' >/dev/null || fail "$approval_name was not hydrated against the exact current plan"
}

wait_for_apply_pod() {
	active_schema=$1
	wait_for_schema "$active_schema" '
      .status.phase == "Applying" and
      .status.activeOperation.type == "Apply" and
      .status.activeOperation.dispatchStarted == true and
      .status.activeOperation.jobUID != null and .status.activeOperation.jobUID != ""
    ' "one dispatched Apply Job"
	ACTIVE_OPERATION_ID=$(k -n "$TEST_NAMESPACE" get ptahschema "$active_schema" -o jsonpath='{.status.activeOperation.id}')
	ACTIVE_JOB_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$active_schema" -o jsonpath='{.status.activeOperation.jobName}')
	ACTIVE_JOB_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$active_schema" -o jsonpath='{.status.activeOperation.jobUID}')
	k -n "$TEST_NAMESPACE" get job "$ACTIVE_JOB_NAME" -o json |
		jq -e \
			--arg uid "$ACTIVE_JOB_UID" \
			--arg schema "$active_schema" \
			--arg operationID "$ACTIVE_OPERATION_ID" '
      .metadata.uid == $uid and
      .metadata.labels["operator.ptah.dev/schema"] == $schema and
      .metadata.labels["operator.ptah.dev/operation"] == "apply" and
      .metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and
      .spec.podReplacementPolicy == "Failed" and .spec.backoffLimit == 0
    ' >/dev/null || fail "$active_schema Apply Job does not preserve the single-Pod failure contract"
	active_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$active_deadline" ]; do
		maybe_audit_fault_runtime
		active_pods=$(k -n "$TEST_NAMESPACE" get pods -l "job-name=${ACTIVE_JOB_NAME}" -o json)
		active_count=$(printf '%s\n' "$active_pods" | jq '[.items[] | select(.metadata.deletionTimestamp == null)] | length')
		[ "$active_count" -le 1 ] || fail "$active_schema has overlapping executor Pods"
		if [ "$active_count" -eq 1 ] && printf '%s\n' "$active_pods" | jq -e '
        .items[0].status.phase == "Running" and
        (.items[0].status.containerStatuses | any(.name == "ptah" and .state.running != null))
      ' >/dev/null; then
			ACTIVE_POD_NAME=$(printf '%s\n' "$active_pods" | jq -r '.items[0].metadata.name')
			ACTIVE_POD_UID=$(printf '%s\n' "$active_pods" | jq -r '.items[0].metadata.uid')
			printf '%s\n' "$active_pods" | jq -e \
				--arg operationID "$ACTIVE_OPERATION_ID" '
          .items[0].metadata.annotations["operator.ptah.dev/operation-id"] == $operationID
        ' >/dev/null || fail "$active_schema executor Pod lost its full operation identity"
			return 0
		fi
		sleep 1
	done
	fail "timed out waiting for the running Apply Pod for $active_schema"
}

wait_for_blocked_apply_pod() {
	blocked_apply_schema=$1
	blocked_apply_deadline_seconds=$2
	[ "$READ_WORKLOAD_BARRIER_ACTIVE" -eq 1 ] ||
		fail "$blocked_apply_schema timeout Apply started without the scheduling barrier"
	wait_for_schema "$blocked_apply_schema" '
      .status.phase == "Applying" and
      .status.activeOperation.type == "Apply" and
      .status.activeOperation.dispatchStarted == true and
      .status.activeOperation.jobUID != null and .status.activeOperation.jobUID != ""
    ' "one dispatched Apply Job held behind the timeout scheduling barrier"
	ACTIVE_OPERATION_ID=$(k -n "$TEST_NAMESPACE" get ptahschema "$blocked_apply_schema" \
		-o jsonpath='{.status.activeOperation.id}')
	ACTIVE_JOB_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$blocked_apply_schema" \
		-o jsonpath='{.status.activeOperation.jobName}')
	ACTIVE_JOB_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$blocked_apply_schema" \
		-o jsonpath='{.status.activeOperation.jobUID}')
	k -n "$TEST_NAMESPACE" get job "$ACTIVE_JOB_NAME" -o json | jq -e \
		--arg uid "$ACTIVE_JOB_UID" \
		--arg schema "$blocked_apply_schema" \
		--arg operationID "$ACTIVE_OPERATION_ID" \
		--argjson deadline "$blocked_apply_deadline_seconds" '
      .metadata.uid == $uid and
      .metadata.labels["operator.ptah.dev/schema"] == $schema and
      .metadata.labels["operator.ptah.dev/operation"] == "apply" and
      .metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and
      .spec.activeDeadlineSeconds == $deadline and
      .spec.template.spec.activeDeadlineSeconds == $deadline and
      .spec.parallelism == 1 and .spec.completions == 1 and
      .spec.backoffLimit == 0 and .spec.podReplacementPolicy == "Failed"
    ' >/dev/null ||
		fail "$blocked_apply_schema timeout Apply Job lost its exact deadline or one-shot contract"

	blocked_apply_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$blocked_apply_deadline" ]; do
		maybe_audit_fault_runtime
		blocked_apply_pods=$(k -n "$TEST_NAMESPACE" get pods \
			-l "batch.kubernetes.io/controller-uid=${ACTIVE_JOB_UID}" -o json)
		blocked_apply_count=$(printf '%s\n' "$blocked_apply_pods" | jq '.items | length')
		[ "$blocked_apply_count" -le 1 ] ||
			fail "$blocked_apply_schema created overlapping timeout Apply Pods"
		if [ "$blocked_apply_count" -eq 1 ] && printf '%s\n' "$blocked_apply_pods" | jq -e \
			--arg jobName "$ACTIVE_JOB_NAME" \
			--arg jobUID "$ACTIVE_JOB_UID" \
			--arg operationID "$ACTIVE_OPERATION_ID" \
			--arg barrierKey "$READ_WORKLOAD_BARRIER_KEY" '
          .items[0] as $pod |
          $pod.metadata.deletionTimestamp == null and
          $pod.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and
          ($pod.metadata.ownerReferences // [] | length == 1 and
            .[0].apiVersion == "batch/v1" and .[0].kind == "Job" and
            .[0].name == $jobName and .[0].uid == $jobUID and .[0].controller == true) and
          ($pod.spec.nodeName // "") == "" and $pod.status.phase == "Pending" and
          ([ $pod.status.initContainerStatuses // [],
             $pod.status.containerStatuses // [],
             $pod.status.ephemeralContainerStatuses // [] ] | add |
            all(.[]; .state.running == null and .state.terminated == null and
              (.restartCount // 0) == 0)) and
          all(($pod.spec.tolerations // [])[]; .key != $barrierKey)
        ' >/dev/null; then
			ACTIVE_POD_NAME=$(printf '%s\n' "$blocked_apply_pods" | jq -er '.items[0].metadata.name')
			ACTIVE_POD_UID=$(printf '%s\n' "$blocked_apply_pods" | jq -er '.items[0].metadata.uid')
			assert_read_workload_blocked "$ACTIVE_JOB_UID" \
				"the exact timeout Apply Job for $blocked_apply_schema"
			return 0
		fi
		sleep 1
	done
	fail "timed out waiting for the blocked Apply Pod for $blocked_apply_schema"
}

record_deadline_pending_pod_evidence() {
	deadline_pod_name=$1
	deadline_pod_uid=$2
	deadline_job_name=$3
	deadline_job_uid=$4
	deadline_pod_object=$(k -n "$TEST_NAMESPACE" get pod "$deadline_pod_name" -o json)
	printf '%s\n' "$deadline_pod_object" | jq -e \
		--arg podUID "$deadline_pod_uid" \
		--arg jobName "$deadline_job_name" \
		--arg jobUID "$deadline_job_uid" '
      .metadata.uid == $podUID and .metadata.deletionTimestamp == null and
      (.metadata.ownerReferences // [] | length == 1 and
        .[0].apiVersion == "batch/v1" and .[0].kind == "Job" and
        .[0].name == $jobName and .[0].uid == $jobUID and .[0].controller == true) and
      (.spec.nodeName // "") == "" and
      ([.status.initContainerStatuses // [], .status.containerStatuses // [],
        .status.ephemeralContainerStatuses // []] | add |
        all(.[]; .state.running == null and .state.terminated == null and
          (.restartCount // 0) == 0))
    ' >/dev/null ||
		fail "timeout Apply Pod $deadline_pod_name lacks exact never-started pre-deadline evidence"
	printf '%s\n' "$deadline_pod_object" >"$RESOURCE_FILE"
	scan_fault_file "$RESOURCE_FILE" "the exact never-started pre-deadline Apply Pod"
	record_audited_uid "$AUDITED_FAULT_PODS_FILE" "$deadline_pod_uid"
	record_audited_uid "$FULLY_AUDITED_FAULT_PODS_FILE" "$deadline_pod_uid"
	: >"$RESOURCE_FILE"
}

wait_for_exact_pod_absence_after_evidence() {
	absent_pod_name=$1
	absent_pod_uid=$2
	absent_pod_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$absent_pod_deadline" ]; do
		absent_pod_object=$(k -n "$TEST_NAMESPACE" get pod "$absent_pod_name" -o json 2>/dev/null || true)
		[ -n "$absent_pod_object" ] || return 0
		printf '%s\n' "$absent_pod_object" | jq -e \
			--arg uid "$absent_pod_uid" '.metadata.uid == $uid' >/dev/null ||
			fail "timeout Apply Pod $absent_pod_name was replaced before deletion completed"
		sleep 1
	done
	fail "timed out waiting for timeout Apply Pod $absent_pod_name to be deleted"
}

wait_for_deadline_job_terminal_and_audit() {
	deadline_terminal_name=$1
	deadline_terminal_uid=$2
	deadline_terminal_seconds=$3
	deadline_terminal_pod_uid=$4
	[ -n "$deadline_terminal_pod_uid" ] ||
		fail "timeout Apply Job $deadline_terminal_name has no exact fully audited Pod UID"
	deadline_terminal_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$deadline_terminal_deadline" ]; do
		deadline_terminal_object=$(k -n "$TEST_NAMESPACE" get job "$deadline_terminal_name" -o json 2>/dev/null || true)
		if [ -n "$deadline_terminal_object" ]; then
			printf '%s\n' "$deadline_terminal_object" | jq -e \
				--arg uid "$deadline_terminal_uid" '.metadata.uid == $uid' >/dev/null ||
				fail "timeout Apply Job $deadline_terminal_name changed UID before terminal audit"
			if printf '%s\n' "$deadline_terminal_object" | jq -e \
				--arg uid "$deadline_terminal_uid" \
				--argjson deadline "$deadline_terminal_seconds" '
              ([.status.conditions // [] | .[] | select(
                .type == "Failed" and .status == "True" and
                .reason == "DeadlineExceeded")] | .[0]) as $condition |
              .metadata.uid == $uid and
              .spec.activeDeadlineSeconds == $deadline and
              .spec.template.spec.activeDeadlineSeconds == $deadline and
              .status.startTime != null and $condition != null and
              (($condition.lastTransitionTime | fromdateiso8601) -
                (.status.startTime | fromdateiso8601)) >= ($deadline - 1)
            ' >/dev/null; then
				printf '%s\n' "$deadline_terminal_object" >"$RESOURCE_FILE"
				scan_fault_file "$RESOURCE_FILE" "the exact DeadlineExceeded Apply Job"
				grep -Fx "$deadline_terminal_pod_uid" "$FULLY_AUDITED_FAULT_PODS_FILE" >/dev/null ||
					fail "timeout Apply Job $deadline_terminal_name lacks its fully audited Pod UID"
				record_audited_uid "$AUDITED_FAULT_JOBS_FILE" "$deadline_terminal_uid"
				record_audited_uid "$SHARED_AUDITED_JOBS_FILE" "$deadline_terminal_uid"
				record_audited_uid "$SHARED_FULLY_AUDITED_JOBS_FILE" "$deadline_terminal_uid"
				: >"$RESOURCE_FILE"
				return 0
			fi
		fi
		sleep 1
	done
	fail "timeout Apply Job $deadline_terminal_name never reached Failed/DeadlineExceeded"
}

assert_active_pod_ephemeral_container_rejected() {
	active_schema=$1
	if [ -z "$ACTIVE_OPERATION_ID" ] || [ -z "$ACTIVE_JOB_NAME" ] ||
		[ -z "$ACTIVE_JOB_UID" ] || [ -z "$ACTIVE_POD_NAME" ] || [ -z "$ACTIVE_POD_UID" ]; then
		fail "cannot test ephemeral-container admission without an exact active operation identity"
	fi

	active_pod=$(k -n "$TEST_NAMESPACE" get pod "$ACTIVE_POD_NAME" -o json)
	printf '%s\n' "$active_pod" | jq -e \
		--arg podUID "$ACTIVE_POD_UID" \
		--arg jobName "$ACTIVE_JOB_NAME" \
		--arg jobUID "$ACTIVE_JOB_UID" \
		--arg operationID "$ACTIVE_OPERATION_ID" '
      .metadata.uid == $podUID and
      .metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and
      (.metadata.ownerReferences // [] | length == 1 and .[0].apiVersion == "batch/v1" and
        .[0].kind == "Job" and .[0].name == $jobName and .[0].uid == $jobUID and
        .[0].controller == true) and
      ((.spec.ephemeralContainers // []) | length == 0)
    ' >/dev/null || fail "$active_schema ephemeral-container test lost its exact active Pod identity"
	k -n "$TEST_NAMESPACE" get ptahschema "$active_schema" -o json | jq -e \
		--arg operationID "$ACTIVE_OPERATION_ID" \
		--arg jobName "$ACTIVE_JOB_NAME" \
		--arg jobUID "$ACTIVE_JOB_UID" '
      .status.activeOperation.id == $operationID and
      .status.activeOperation.jobName == $jobName and
      .status.activeOperation.jobUID == $jobUID and
      .status.activeOperation.dispatchStarted == true
    ' >/dev/null || fail "$active_schema changed active operation before its ephemeral-container test"

	printf '%s\n' "$active_pod" | jq '
      .spec.ephemeralContainers = [{
        name: "ptah-admission-must-deny",
        image: "invalid.invalid/ptah-admission-must-deny@sha256:0000000000000000000000000000000000000000000000000000000000000000",
        imagePullPolicy: "Never",
        command: ["/bin/sh"],
        targetContainerName: "ptah"
      }] |
      {
        apiVersion: "v1", kind: "Pod",
        metadata: {
          namespace: .metadata.namespace,
          name: .metadata.name,
          uid: .metadata.uid,
          resourceVersion: .metadata.resourceVersion
        },
        spec: {ephemeralContainers: .spec.ephemeralContainers}
      }
    ' >"$RESOURCE_FILE"
	if k replace --raw \
		"/api/v1/namespaces/${TEST_NAMESPACE}/pods/${ACTIVE_POD_NAME}/ephemeralcontainers" \
		-f "$RESOURCE_FILE" >"$LOG_FILE" 2>&1; then
		fail "$active_schema operation Pod admitted an out-of-envelope ephemeral container"
	fi
	if ! grep -F 'vpodintent.operator.ptah.dev' "$LOG_FILE" >/dev/null ||
		! grep -F 'denied the request' "$LOG_FILE" >/dev/null; then
		fail "$active_schema ephemeral-container request failed outside the Pod-intent webhook"
	fi

	k -n "$TEST_NAMESPACE" get pod "$ACTIVE_POD_NAME" -o json | jq -e \
		--arg podUID "$ACTIVE_POD_UID" \
		--arg jobUID "$ACTIVE_JOB_UID" '
      .metadata.uid == $podUID and
      (.metadata.ownerReferences // [] | any(
        .apiVersion == "batch/v1" and .kind == "Job" and
        .uid == $jobUID and .controller == true)) and
      ((.spec.ephemeralContainers // []) |
        all(.name != "ptah-admission-must-deny"))
    ' >/dev/null || fail "$active_schema retained the rejected ephemeral container or changed Pod identity"
}

checkpoint_leases() {
	lease_checkpoint=$1
	k -n "$OPERATOR_NAMESPACE" get leases \
		-l 'app.kubernetes.io/managed-by=ptah-operator,operator.ptah.dev/coordination=database-target' \
		-o json | jq '[.items[].metadata.uid] | sort' >"$lease_checkpoint"
}

load_single_new_released_lease() {
	lease_checkpoint=$1
	lease_description=$2
	lease_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$lease_deadline" ]; do
		maybe_audit_fault_runtime
		lease_object=$(k -n "$OPERATOR_NAMESPACE" get leases \
			-l 'app.kubernetes.io/managed-by=ptah-operator,operator.ptah.dev/coordination=database-target' \
			-o json)
		lease_new_count=$(printf '%s\n' "$lease_object" | jq \
			--slurpfile before "$lease_checkpoint" '
        [.items[] | .metadata.uid as $uid | select(($before[0] | index($uid)) == null)] | length
      ')
		if [ "$lease_new_count" -eq 1 ]; then
			LEASE_NAME=$(printf '%s\n' "$lease_object" | jq -r \
				--slurpfile before "$lease_checkpoint" '
              [.items[] | .metadata.uid as $uid | select(($before[0] | index($uid)) == null)][0].metadata.name
            ')
			LEASE_UID=$(printf '%s\n' "$lease_object" | jq -r \
				--slurpfile before "$lease_checkpoint" '
              [.items[] | .metadata.uid as $uid | select(($before[0] | index($uid)) == null)][0].metadata.uid
            ')
			LEASE_HOLDER=$(printf '%s\n' "$lease_object" | jq -r \
				--slurpfile before "$lease_checkpoint" '
              [.items[] | .metadata.uid as $uid | select(($before[0] | index($uid)) == null)][0].spec.holderIdentity
            ')
			LEASE_EPOCH=$(printf '%s\n' "$lease_object" | jq -r \
				--slurpfile before "$lease_checkpoint" '
              [.items[] | .metadata.uid as $uid | select(($before[0] | index($uid)) == null)][0].metadata.annotations["operator.ptah.dev/lease-epoch"]
            ')
			[ -z "$LEASE_HOLDER" ] || [ "$LEASE_HOLDER" = null ] ||
				fail "$lease_description was not released after its initial Plan"
			printf '%s\n' "$LEASE_EPOCH" | grep -Eq '^v1-[0-9a-f]{32}$' ||
				fail "$lease_description has no valid released acquisition epoch"
			return 0
		fi
		[ "$lease_new_count" -lt 1 ] ||
			fail "$lease_description created $lease_new_count target Leases, expected exactly one"
		sleep 1
	done
	fail "timed out waiting for $lease_description"
}

wait_for_lease_reacquisition() {
	reacquire_name=$1
	reacquire_uid=$2
	reacquire_previous_epoch=$3
	reacquire_description=$4
	reacquire_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$reacquire_deadline" ]; do
		maybe_audit_fault_runtime
		if reacquire_object=$(k -n "$OPERATOR_NAMESPACE" get lease "$reacquire_name" -o json 2>/dev/null); then
			if printf '%s\n' "$reacquire_object" | jq -e \
				--arg uid "$reacquire_uid" \
				--arg oldEpoch "$reacquire_previous_epoch" '
              .metadata.uid == $uid and
              (.spec.holderIdentity | type == "string" and length > 0) and
              (.metadata.annotations["operator.ptah.dev/lease-epoch"] |
                test("^v1-[0-9a-f]{32}$") and . != $oldEpoch)
            ' >/dev/null; then
				LEASE_NAME=$reacquire_name
				LEASE_UID=$reacquire_uid
				LEASE_HOLDER=$(printf '%s\n' "$reacquire_object" | jq -er '.spec.holderIdentity')
				LEASE_EPOCH=$(printf '%s\n' "$reacquire_object" |
					jq -er '.metadata.annotations["operator.ptah.dev/lease-epoch"]')
				return 0
			fi
		fi
		sleep 1
	done
	fail "timed out waiting for $reacquire_description to reuse its released target Lease"
}

load_held_lease_for_epoch() {
	held_epoch=$1
	held_description=$2
	held_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$held_deadline" ]; do
		maybe_audit_fault_runtime
		held_leases=$(k -n "$OPERATOR_NAMESPACE" get leases \
			-l 'app.kubernetes.io/managed-by=ptah-operator,operator.ptah.dev/coordination=database-target' \
			-o json)
		held_count=$(printf '%s\n' "$held_leases" | jq \
			--arg epoch "$held_epoch" '
          [.items[] | select(
            .metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch and
            (.spec.holderIdentity | type == "string" and length > 0))] | length
        ')
		case "$held_count" in
		0) ;;
		1)
			LEASE_NAME=$(printf '%s\n' "$held_leases" | jq -er \
				--arg epoch "$held_epoch" '
                  .items[] | select(
                    .metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch and
                    (.spec.holderIdentity | type == "string" and length > 0)) |
                  .metadata.name
                ')
			LEASE_UID=$(printf '%s\n' "$held_leases" | jq -er \
				--arg epoch "$held_epoch" '
                  .items[] | select(
                    .metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch and
                    (.spec.holderIdentity | type == "string" and length > 0)) |
                  .metadata.uid
                ')
			LEASE_HOLDER=$(printf '%s\n' "$held_leases" | jq -er \
				--arg epoch "$held_epoch" '
                  .items[] | select(
                    .metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch and
                    (.spec.holderIdentity | type == "string" and length > 0)) |
                  .spec.holderIdentity
                ')
			LEASE_EPOCH=$held_epoch
			assert_lease_identity "$LEASE_NAME" "$LEASE_UID" "$LEASE_HOLDER" "$LEASE_EPOCH"
			return 0
			;;
		*) fail "$held_description matched multiple live target Leases for one epoch" ;;
		esac
		sleep 1
	done
	fail "timed out waiting for $held_description"
}

assert_same_lease_reacquired_in_watch() {
	reused_uid=$1
	first_holder=$2
	first_epoch=$3
	contender_schema=$4
	contender_operation=$5
	contender_job_uid=$6
	contender_epoch=$7
	snapshot_watch schemas
	reacquired_schema_watch=$WATCH_SNAPSHOT_FILE
	snapshot_watch jobs
	reacquired_job_watch=$WATCH_SNAPSHOT_FILE
	snapshot_watch leases
	reacquired_lease_watch=$WATCH_SNAPSHOT_FILE
	jq -s -e \
		--arg uid "$reused_uid" \
		--arg firstHolder "$first_holder" \
		--arg firstEpoch "$first_epoch" \
		--arg schema "$contender_schema" \
		--arg operation "$contender_operation" \
		--arg jobUID "$contender_job_uid" \
		--arg contenderEpoch "$contender_epoch" \
		--slurpfile schemas "$reacquired_schema_watch" \
		--slurpfile jobs "$reacquired_job_watch" '
	  [to_entries[]] as $allEvents |
	  [$allEvents[] | select(.value.object.metadata.uid == $uid)] as $events |
	  ($events | map(select(
	    .value.object.spec.holderIdentity == $firstHolder and
	    .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $firstEpoch)) |
	    .[0]) as $first |
	  $first.value.object.metadata.name as $leaseName |
	  ($events | map(select(
	    .key > $first.key and
	    (.value.object.spec.holderIdentity // "") == "")) | .[0]) as $released |
	  ($allEvents | map(select(
	    .key > $released.key and
	    .value.object.metadata.name == $leaseName and
	    (.value.object.spec.holderIdentity // "") != "")) |
	    .[0]) as $reacquired |
	  ([$schemas[] | select(
	    .object.metadata.name == $schema and
	    .object.status.activeOperation.type == "Apply" and
	    .object.status.activeOperation.id == $operation and
	    .object.status.activeOperation.jobUID == $jobUID and
	    .object.status.activeOperation.leaseEpoch == $contenderEpoch and
	    .object.status.activeOperation.coordinationDigest ==
	      .object.status.target.coordinationDigest)] | .[0]) as $active |
	  ([$jobs[] | select(
	    .type == "ADDED" and .object.metadata.uid == $jobUID and
	    .object.metadata.labels["operator.ptah.dev/schema"] == $schema and
	    .object.metadata.labels["operator.ptah.dev/operation"] == "apply" and
	    .object.metadata.annotations["operator.ptah.dev/operation-id"] == $operation)] |
	    .[0]) as $jobAdded |
	  $first != null and $released != null and $reacquired != null and
	  $reacquired.value.type == "MODIFIED" and
	  $reacquired.value.object.metadata.uid == $uid and
	  $reacquired.value.object.spec.holderIdentity != $firstHolder and
	  $reacquired.value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] ==
	    $contenderEpoch and
	  $reacquired.value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] !=
	    $firstEpoch and
	  all($allEvents[];
	    if .key > $released.key and .key < $reacquired.key and
	      .value.object.metadata.name == $leaseName then
	      .value.type != "DELETED" and .value.object.metadata.uid == $uid and
	      (.value.object.spec.holderIdentity // "") == ""
	    else true end) and
	  $active != null and $jobAdded != null
	' "$reacquired_lease_watch" >/dev/null ||
		fail "target Lease was not released and reacquired by the exact contender on the same UID"
}

assert_lease_identity() {
	identity_name=$1
	identity_uid=$2
	identity_holder=$3
	identity_epoch=$4
	k -n "$OPERATOR_NAMESPACE" get lease "$identity_name" -o json |
		jq -e \
			--arg uid "$identity_uid" \
			--arg holder "$identity_holder" \
			--arg epoch "$identity_epoch" '
      .metadata.uid == $uid and .spec.holderIdentity == $holder and
      .metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch
    ' >/dev/null || fail "target Lease identity or holder changed across a controller restart"
}

assert_lease_held_without_release() {
	held_name=$1
	held_uid=$2
	held_holder=$3
	held_epoch=$4
	assert_lease_identity "$held_name" "$held_uid" "$held_holder" "$held_epoch"
	establish_watch_barrier leases "$OPERATOR_NAMESPACE" lease "$held_name"
	assert_lease_identity "$held_name" "$held_uid" "$held_holder" "$held_epoch"
	snapshot_watch leases
	jq -s -e \
		--arg uid "$held_uid" \
		--arg holder "$held_holder" \
		--arg epoch "$held_epoch" '
      [to_entries[] | select(.value.object.metadata.uid == $uid)] as $events |
      ($events | map(select(
        .value.object.spec.holderIdentity == $holder and
        .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch)) |
        .[0]) as $acquired |
      $acquired != null and
      all($events[];
        if .key >= $acquired.key then
          .value.type != "DELETED" and
          .value.object.spec.holderIdentity == $holder and
          .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $epoch
        else true end)
    ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
		fail "target Lease was released or replaced before the controlled proof boundary"
}

assert_post_apply_proof_history() {
	proof_schema=$1
	proof_apply_operation=$2
	proof_apply_job_uid=$3
	proof_lease_uid=$4
	proof_lease_holder=$5
	proof_lease_epoch=$6
	proof_observe_job_uid=$7
	proof_plan_job_uid=$8
	snapshot_watch schemas
	proof_schema_watch=$WATCH_SNAPSHOT_FILE
	snapshot_watch jobs
	proof_job_watch=$WATCH_SNAPSHOT_FILE
	snapshot_watch leases
	proof_lease_watch=$WATCH_SNAPSHOT_FILE
	jq -s -e \
		--arg schema "$proof_schema" \
		--arg applyOperation "$proof_apply_operation" \
		--arg applyJobUID "$proof_apply_job_uid" \
		--arg leaseUID "$proof_lease_uid" \
		--arg leaseHolder "$proof_lease_holder" \
		--arg leaseEpoch "$proof_lease_epoch" \
		--arg observeJobUID "$proof_observe_job_uid" \
		--arg planJobUID "$proof_plan_job_uid" \
		--slurpfile jobs "$proof_job_watch" \
		--slurpfile leases "$proof_lease_watch" '
      def active_matches_pending($active; $pending):
        $active.coordinationDigest == $pending.coordinationDigest and
        $active.targetIdentityDigest == $pending.plan.targetIdentityDigest and
        $active.target == $pending.target and
        $active.source == $pending.source and
        ($active.observationDev // null) == ($pending.dev // null) and
        ($active.observationExclude // []) == ($pending.exclude // []) and
        ($active.observationSeverity // "") == ($pending.driftSeverity // "") and
        ($active.observationConnectTimeout // "0s") == ($pending.connectTimeout // "0s") and
        ($active.observationLockTimeout // "0s") == ($pending.lockTimeout // "0s") and
        $active.leaseDurationSeconds == $pending.leaseDurationSeconds and
        $active.leaseEpoch == $pending.leaseEpoch and
        ($active.leaseContinuityLost // false) == false;
      [.[] | select(.object.metadata.name == $schema) | .object] as $schemas |
	      ($schemas | map(select(
        .status.activeOperation.type == "Apply" and
        .status.activeOperation.id == $applyOperation and
        .status.activeOperation.jobUID == $applyJobUID and
        .status.activeOperation.leaseEpoch == $leaseEpoch)) | .[0]) as $apply |
      ($schemas | map(select(
        .status.pendingObservation.outcome == "ApplySucceeded" and
        .status.pendingObservation.applyOperationID == $applyOperation and
        .status.pendingObservation.applyJobUID == $applyJobUID and
        .status.pendingObservation.leaseEpoch == $leaseEpoch)) | .[0]) as $pendingObject |
      $pendingObject.status.pendingObservation as $pending |
      ($schemas | map(select(
        .status.activeOperation.type == "Observe" and
        .status.activeOperation.jobUID == $observeJobUID and
        .status.pendingObservation.applyOperationID == $applyOperation and
        .status.pendingObservation.leaseEpoch == $leaseEpoch)) | .[0]) as $observe |
      ($schemas | map(select(
        .status.activeOperation.type == "Plan" and
        .status.activeOperation.jobUID == $planJobUID and
        .status.pendingObservation.applyOperationID == $applyOperation and
        .status.pendingObservation.leaseEpoch == $leaseEpoch and
	        .status.pendingObservation.planRequired == true)) | .[0]) as $plan |
	      ($schemas | map(select(
	        .status.pendingLockRelease.operationID == $applyOperation and
	        .status.pendingLockRelease.leaseEpoch == $leaseEpoch and
	        .status.pendingLockRelease.coordinationDigest ==
	          $pending.coordinationDigest)) | .[0]) as $releaseRequest |
	      ($jobs | to_entries | map(select(
	        .value.type == "ADDED" and
	        .value.object.metadata.uid == $observe.status.activeOperation.jobUID)) | .[0]) as $observeAdded |
	      ($jobs | to_entries | map(select(
        .value.object.metadata.uid == $observe.status.activeOperation.jobUID and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True")))) | .[0]) as $observeComplete |
      ($jobs | to_entries | map(select(
        .value.type == "ADDED" and
        .value.object.metadata.uid == $plan.status.activeOperation.jobUID)) | .[0]) as $planAdded |
      ($jobs | to_entries | map(select(
        .value.object.metadata.uid == $plan.status.activeOperation.jobUID and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True")))) | .[-1]) as $planComplete |
      ($leases | to_entries | map(select(
        .value.object.metadata.uid == $leaseUID))) as $leaseEvents |
      ($leaseEvents | map(select(
        .value.object.spec.holderIdentity == $leaseHolder and
        .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $leaseEpoch)) |
        .[0]) as $held |
      ($leaseEvents | map(select(
        .key > $held.key and
        (.value.object.spec.holderIdentity // "") == "")) | .[0]) as $released |
      $apply != null and $pendingObject != null and $observe != null and $plan != null and
	      $releaseRequest != null and $apply.status.applied == null and
	      $apply.status.pendingLockRelease == null and
      $pending.plan == $apply.status.plan and
	      $pending.applyJobName == $apply.status.activeOperation.jobName and
	      $pending.applyJobUID == $applyJobUID and
      $pending.coordinationDigest == $apply.status.activeOperation.coordinationDigest and
      $pending.target == $apply.status.activeOperation.target and
      $pending.source == $apply.status.activeOperation.source and
      ($pending.dev // null) == ($apply.status.activeOperation.observationDev // null) and
      ($pending.exclude // []) == ($apply.status.activeOperation.observationExclude // []) and
      ($pending.driftSeverity // "") == ($apply.status.activeOperation.observationSeverity // "") and
      ($pending.connectTimeout // "0s") == ($apply.status.activeOperation.observationConnectTimeout // "0s") and
      ($pending.lockTimeout // "0s") == ($apply.status.activeOperation.observationLockTimeout // "0s") and
      $pending.leaseDurationSeconds == $apply.status.activeOperation.leaseDurationSeconds and
      ($apply.status.activeOperation.leaseContinuityLost // false) == false and
      active_matches_pending($observe.status.activeOperation; $observe.status.pendingObservation) and
      active_matches_pending($plan.status.activeOperation; $plan.status.pendingObservation) and
	      all($schemas[];
	        if .status.pendingObservation.applyOperationID == $applyOperation then
	          .status.phase == "VerifyingConvergence" and
	          .status.applied == null and .status.pendingLockRelease == null and
	          all(.status.conditions[];
	            if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
	        else true end) and
	      $releaseRequest.status.pendingObservation == null and
	      $releaseRequest.status.activeOperation == null and
      any($schemas[];
        .status.phase == "InSync" and .status.pendingObservation == null and
	        .status.activeOperation == null and .status.pendingLockRelease == null and
        .status.applied.artifactDigest == $pending.plan.artifactDigest and
        .status.applied.planFingerprint == $pending.plan.fingerprint and
        .status.applied.coordinationDigest == $pending.plan.coordinationDigest and
        .status.applied.targetIdentityDigest == $pending.plan.targetIdentityDigest and
        .status.applied.ptahVersion == $pending.plan.ptahVersion and
        .status.applied.executorImage == $pending.plan.executorImage and
        .status.applied.runnerImage == $pending.plan.runnerImage and
        .status.applied.runnerProtocolVersion == $pending.plan.runnerProtocolVersion and
        (.status.conditions | any(
          .type == "InSync" and .status == "True" and .reason == "ScopedConverged"))) and
	      $observeAdded != null and $observeComplete != null and $planAdded != null and
	      ($observe.status.activeOperation.id | length) > 0 and
	      ($plan.status.activeOperation.id | length) > 0 and
	      $observeAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] ==
	        $observe.status.activeOperation.id and
	      $planAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] ==
	        $plan.status.activeOperation.id and
	      $observeComplete.key < $planAdded.key and
	      $planComplete != null and
      $held != null and $released != null and
      $released.value.type == "MODIFIED" and
      $released.value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $leaseEpoch and
	      all($leaseEvents[];
	        if .key >= $held.key and .key < $released.key then
          .value.type != "DELETED" and
          .value.object.spec.holderIdentity == $leaseHolder and
          .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $leaseEpoch
	        else true end)
    ' "$proof_schema_watch" >/dev/null ||
		fail "$proof_schema did not retain one immutable Apply Lease and proof snapshot through Observe and Plan"
}

assert_uncertain_apply_proof_history() {
	unknown_schema=$1
	unknown_apply_operation=$2
	unknown_apply_job_uid=$3
	unknown_lease_uid=$4
	unknown_lease_holder=$5
	unknown_lease_epoch=$6
	unknown_observe_job_uid=$7
	unknown_plan_job_uid=$8
	unknown_fresh_plan_uid=$9
	unknown_apply_pod_uid=${10}
	unknown_plan_mode=${11:-same-plan}
	unknown_old_actual_fingerprint=${12:-}
	unknown_apply_pod_count=${13:-1}
	case "$unknown_apply_pod_count" in
	0) unknown_apply_pod_uids='[]' ;;
	1) unknown_apply_pod_uids=$(jq -cn --arg uid "$unknown_apply_pod_uid" '[$uid]') ;;
	*) fail "$unknown_schema proof history has an unsupported Apply Pod evidence count" ;;
	esac
	unknown_final_schema=$WORK_DIR/${unknown_schema}-final-schema.json
	unknown_fresh_plan=$WORK_DIR/${unknown_schema}-fresh-plan.json
	k -n "$TEST_NAMESPACE" get ptahschema "$unknown_schema" -o json >"$unknown_final_schema"
	case "$unknown_plan_mode" in
	no-changes) printf '%s\n' '{}' >"$unknown_fresh_plan" ;;
	same-plan | manual-drift)
		k -n "$TEST_NAMESPACE" get ptahschemaplan \
			"$(jq -er '.status.plan.name' "$unknown_final_schema")" -o json >"$unknown_fresh_plan"
		;;
	*) fail "unsupported uncertain proof plan mode: $unknown_plan_mode" ;;
	esac
	snapshot_watch schemas
	unknown_schema_watch=$WATCH_SNAPSHOT_FILE
	snapshot_watch jobs
	unknown_job_watch=$WATCH_SNAPSHOT_FILE
	snapshot_watch leases
	unknown_lease_watch=$WATCH_SNAPSHOT_FILE
	jq -s -e \
		--arg schema "$unknown_schema" \
		--arg applyOperation "$unknown_apply_operation" \
		--arg applyJobUID "$unknown_apply_job_uid" \
		--arg leaseUID "$unknown_lease_uid" \
		--arg leaseHolder "$unknown_lease_holder" \
		--arg leaseEpoch "$unknown_lease_epoch" \
		--arg observeJobUID "$unknown_observe_job_uid" \
		--arg planJobUID "$unknown_plan_job_uid" \
		--arg freshPlanUID "$unknown_fresh_plan_uid" \
		--argjson applyPodUIDs "$unknown_apply_pod_uids" \
		--argjson applyPodCount "$unknown_apply_pod_count" \
		--arg planMode "$unknown_plan_mode" \
		--arg oldActual "$unknown_old_actual_fingerprint" \
		--slurpfile jobs "$unknown_job_watch" \
		--slurpfile leases "$unknown_lease_watch" \
		--slurpfile finalSchema "$unknown_final_schema" \
		--slurpfile freshPlan "$unknown_fresh_plan" '
      def pending_binding_matches($candidate; $origin):
        $candidate.outcome == $origin.outcome and
        $candidate.applyOperationID == $origin.applyOperationID and
        $candidate.applyJobName == $origin.applyJobName and
        $candidate.applyJobUID == $origin.applyJobUID and
	        $candidate.applyPodUIDs == $origin.applyPodUIDs and
	        $candidate.applyPodCount == $origin.applyPodCount and
        $candidate.applyGeneration == $origin.applyGeneration and
        ($candidate.observeAfter // null) == ($origin.observeAfter // null) and
        $candidate.plan == $origin.plan and
        $candidate.target == $origin.target and
        $candidate.coordinationDigest == $origin.coordinationDigest and
        $candidate.source == $origin.source and
        ($candidate.dev // null) == ($origin.dev // null) and
        ($candidate.exclude // []) == ($origin.exclude // []) and
        ($candidate.driftSeverity // "") == ($origin.driftSeverity // "") and
        ($candidate.connectTimeout // "0s") == ($origin.connectTimeout // "0s") and
        ($candidate.lockTimeout // "0s") == ($origin.lockTimeout // "0s") and
        $candidate.leaseDurationSeconds == $origin.leaseDurationSeconds and
        $candidate.leaseEpoch == $origin.leaseEpoch;
      def active_matches_pending($active; $pending):
        $active.coordinationDigest == $pending.coordinationDigest and
        $active.targetIdentityDigest == $pending.plan.targetIdentityDigest and
        $active.target == $pending.target and
        $active.source == $pending.source and
        ($active.observationDev // null) == ($pending.dev // null) and
        ($active.observationExclude // []) == ($pending.exclude // []) and
        ($active.observationSeverity // "") == ($pending.driftSeverity // "") and
        ($active.observationConnectTimeout // "0s") == ($pending.connectTimeout // "0s") and
        ($active.observationLockTimeout // "0s") == ($pending.lockTimeout // "0s") and
        $active.leaseDurationSeconds == $pending.leaseDurationSeconds and
        $active.leaseEpoch == $pending.leaseEpoch and
        ($active.leaseContinuityLost // false) == false;
      def final_awaits_fresh($final; $fresh; $uid):
        $final.status.phase == "AwaitingApproval" and
        $final.status.activeOperation == null and $final.status.pendingObservation == null and
        $final.status.pendingLockRelease == null and $final.status.applied == null and
        $final.status.plan.uid == $uid and $final.status.plan.name == $fresh.metadata.name and
        $final.status.plan.uid == $fresh.metadata.uid and $final.status.plan.approval == null and
        $fresh.spec.schemaRef.name == $final.metadata.name and
        $fresh.spec.schemaRef.uid == $final.metadata.uid and
        $final.status.plan.fingerprint == $fresh.spec.fingerprint and
        $final.status.plan.contentDigest == $fresh.spec.contentDigest and
        $final.status.plan.artifactDigest == $fresh.spec.artifactDigest and
        $final.status.plan.coordinationDigest == $fresh.spec.coordinationDigest and
        $final.status.plan.targetIdentityDigest == $fresh.spec.targetIdentityDigest and
        $final.status.plan.actualStateFingerprint == $fresh.spec.actualStateFingerprint and
        $final.status.plan.desiredStateFingerprint == $fresh.spec.desiredStateFingerprint and
        $final.status.plan.policyFingerprint == $fresh.spec.policyFingerprint and
        $final.status.plan.verificationPolicyUID == $fresh.spec.verificationPolicyUID and
        $final.status.plan.verificationPolicyDigest == $fresh.spec.verificationPolicyDigest and
        $final.status.plan.ptahVersion == $fresh.spec.ptahVersion and
        $final.status.plan.executorImage == $fresh.spec.executorImage and
        $final.status.plan.runnerImage == $fresh.spec.runnerImage and
        $final.status.plan.runnerProtocolVersion == $fresh.spec.runnerProtocolVersion and
        $final.status.plan.destructive == $fresh.spec.destructive and
        $final.status.plan.statementCount == $fresh.spec.statementCount and
        $final.status.plan.createdAt == $fresh.metadata.creationTimestamp;
      def immutable_plan_inputs_match($fresh; $old):
        $fresh.spec.artifactDigest == $old.artifactDigest and
        $fresh.spec.coordinationDigest == $old.coordinationDigest and
        $fresh.spec.targetIdentityDigest == $old.targetIdentityDigest and
        $fresh.spec.desiredStateFingerprint == $old.desiredStateFingerprint and
        $fresh.spec.policyFingerprint == $old.policyFingerprint and
        $fresh.spec.verificationPolicyUID == $old.verificationPolicyUID and
        $fresh.spec.verificationPolicyDigest == $old.verificationPolicyDigest and
        $fresh.spec.ptahVersion == $old.ptahVersion and
        $fresh.spec.executorImage == $old.executorImage and
        $fresh.spec.runnerImage == $old.runnerImage and
        $fresh.spec.runnerProtocolVersion == $old.runnerProtocolVersion;
      [.[] | select(.object.metadata.name == $schema) | .object] as $schemas |
	      ($schemas | to_entries | map(select(
	        .value.status.activeOperation.type == "Apply" and
	        .value.status.activeOperation.id == $applyOperation and
	        .value.status.activeOperation.jobUID == $applyJobUID and
	        .value.status.activeOperation.leaseEpoch == $leaseEpoch)) | .[0]) as $apply |
      ($schemas | to_entries | map(select(
        .value.status.pendingObservation.outcome == "OutcomeUnknown" and
        .value.status.pendingObservation.applyOperationID == $applyOperation and
        .value.status.pendingObservation.applyJobUID == $applyJobUID and
        .value.status.pendingObservation.leaseEpoch == $leaseEpoch)) | .[0]) as $unknown |
      $unknown.value.status.pendingObservation as $origin |
      ($schemas | to_entries | map(select(
        .key > $unknown.key and
        .value.status.activeOperation.type == "Observe" and
        .value.status.activeOperation.jobUID == $observeJobUID and
        .value.status.pendingObservation.applyOperationID == $applyOperation and
        .value.status.pendingObservation.leaseEpoch == $leaseEpoch)) | .[0]) as $observe |
      ($schemas | to_entries | map(select(
        .key > $observe.key and
        .value.status.activeOperation.type == "Plan" and
        .value.status.activeOperation.jobUID == $planJobUID and
        .value.status.pendingObservation.applyOperationID == $applyOperation and
        .value.status.pendingObservation.leaseEpoch == $leaseEpoch and
        .value.status.pendingObservation.planRequired == true)) | .[0]) as $plan |
	      ($schemas | to_entries | map(select(
	        .key > $plan.key and
	        .value.status.pendingLockRelease.operationID == $applyOperation and
	        .value.status.pendingLockRelease.leaseEpoch == $leaseEpoch and
	        .value.status.pendingLockRelease.coordinationDigest ==
	          $origin.coordinationDigest)) | .[0]) as $releaseRequest |
      ($jobs | to_entries | map(select(
        .value.type == "ADDED" and .value.object.metadata.uid == $observeJobUID)) | .[0]) as $observeAdded |
      ($jobs | to_entries | map(select(
        .value.object.metadata.uid == $observeJobUID and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True")))) | .[0]) as $observeComplete |
      ($jobs | to_entries | map(select(
        .value.type == "ADDED" and .value.object.metadata.uid == $planJobUID)) | .[0]) as $planAdded |
      ($jobs | to_entries | map(select(
        .value.object.metadata.uid == $planJobUID and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True")))) | .[-1]) as $planComplete |
      ($leases | to_entries | map(select(
        .value.object.metadata.uid == $leaseUID))) as $leaseEvents |
      ($leaseEvents | map(select(
        .value.object.spec.holderIdentity == $leaseHolder and
        .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $leaseEpoch)) |
        .[0]) as $held |
      ($leaseEvents | map(select(
        .key > $held.key and
        (.value.object.spec.holderIdentity // "") == "")) | .[0]) as $released |
	      $apply != null and $unknown != null and $observe != null and $plan != null and
	      $releaseRequest != null and
	      $apply.value.status.activeOperation.dispatchNotAfter != null and
	      $apply.value.status.activeOperation.executionNotAfter ==
	        $apply.value.status.activeOperation.dispatchNotAfter and
	      $apply.value.status.activeOperation.terminationGracePeriodSeconds == 30 and
	      $origin.applyJobName == $apply.value.status.activeOperation.jobName and
	      $origin.applyJobUID == $applyJobUID and
	      $origin.applyPodUIDs == $applyPodUIDs and
	      $origin.applyPodCount == $applyPodCount and
	      $unknown.value.status.phase == "VerifyingConvergence" and
	      $unknown.value.status.applied == null and
	      $unknown.value.status.pendingLockRelease == null and
      pending_binding_matches($observe.value.status.pendingObservation; $origin) and
      pending_binding_matches($plan.value.status.pendingObservation; $origin) and
      active_matches_pending($observe.value.status.activeOperation; $observe.value.status.pendingObservation) and
      active_matches_pending($plan.value.status.activeOperation; $plan.value.status.pendingObservation) and
	      all([$unknown.value, $observe.value, $plan.value][];
	        .status.phase == "VerifyingConvergence" and
	        .status.applied == null and .status.pendingLockRelease == null and
	        all(.status.conditions[];
	          if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)) and
	      $releaseRequest.value.status.pendingObservation == null and
	      $releaseRequest.value.status.activeOperation == null and
      $observeAdded != null and $observeComplete != null and
      $planAdded != null and $planComplete != null and
      ($observe.value.status.activeOperation.id | length) > 0 and
      ($plan.value.status.activeOperation.id | length) > 0 and
      $observeAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] ==
        $observe.value.status.activeOperation.id and
      $planAdded.value.object.metadata.annotations["operator.ptah.dev/operation-id"] ==
        $plan.value.status.activeOperation.id and
      $observeComplete.key < $planAdded.key and
      $held != null and $released != null and
      $released.value.type == "MODIFIED" and
      $released.value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $leaseEpoch and
	      all($leaseEvents[];
	        if .key >= $held.key and .key < $released.key then
          .value.type != "DELETED" and
          .value.object.spec.holderIdentity == $leaseHolder and
          .value.object.metadata.annotations["operator.ptah.dev/lease-epoch"] == $leaseEpoch
	        else true end) and
      ($finalSchema[0] as $final | $freshPlan[0] as $fresh |
        if $planMode == "no-changes" then
          $final.status.phase == "InSync" and $final.status.activeOperation == null and
          $final.status.pendingObservation == null and $final.status.pendingLockRelease == null and
          $final.status.applied == null and $final.status.plan == null and
          ($final.status.conditions | any(
            .type == "InSync" and .status == "True" and .reason == "ScopedConverged"))
        elif $planMode == "same-plan" then
          final_awaits_fresh($final; $fresh; $freshPlanUID) and
          $fresh.spec.fingerprint == $origin.plan.fingerprint and
          $fresh.spec.contentDigest == $origin.plan.contentDigest and
          $fresh.spec.actualStateFingerprint == $origin.plan.actualStateFingerprint and
          $fresh.spec.destructive == $origin.plan.destructive and
          $fresh.spec.statementCount == $origin.plan.statementCount and
          immutable_plan_inputs_match($fresh; $origin.plan)
        elif $planMode == "manual-drift" then
          final_awaits_fresh($final; $fresh; $freshPlanUID) and
          $oldActual != "" and $origin.plan.actualStateFingerprint == $oldActual and
          $fresh.spec.actualStateFingerprint != $oldActual and
          immutable_plan_inputs_match($fresh; $origin.plan)
        else false end) and
      all($schemas[]; .status.applied == null)
    ' "$unknown_schema_watch" >/dev/null ||
		fail "$unknown_schema did not retain one uncertain Apply Lease and immutable proof snapshot through Observe and Plan"
}

start_pg_barrier() {
	barrier_database=$1
	barrier_token=$2
	assert_safe_name "$barrier_token"
	barrier_pid_file=$WORK_DIR/pg-barrier-${barrier_token}.pid
	[ ! -e "$barrier_pid_file" ] || fail "PostgreSQL metadata barrier $barrier_token already exists"
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
		sh -ec 'PGAPPNAME="$2" PGPASSWORD="$POSTGRES_PASSWORD" psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -c "BEGIN; LOCK TABLE e2e_widgets IN ACCESS SHARE MODE; SELECT pg_sleep($3); ROLLBACK"' \
		sh "$barrier_database" "$barrier_token" "$FAULT_BARRIER_SECONDS" >"$WORK_DIR/${barrier_token}.log" 2>&1 &
	barrier_pid=$!
	printf '%s\n' "$barrier_pid" >"$barrier_pid_file"
	PG_BARRIER_TOKENS="${PG_BARRIER_TOKENS} ${barrier_token}"
	wait_query_equals postgresql postgres \
		"SELECT count(*) FROM pg_stat_activity WHERE application_name='${barrier_token}' AND wait_event='PgSleep'" \
		1 "the PostgreSQL metadata barrier to hold its table lock"
}

stop_pg_barrier() {
	barrier_token=$1
	assert_safe_name "$barrier_token"
	barrier_pid_file=$WORK_DIR/pg-barrier-${barrier_token}.pid
	[ -f "$barrier_pid_file" ] || fail "PostgreSQL metadata barrier $barrier_token is not active"
	barrier_pid=$(tr -d '[:space:]' <"$barrier_pid_file")
	printf '%s\n' "$barrier_pid" | grep -Eq '^[1-9][0-9]*$' ||
		fail "PostgreSQL metadata barrier $barrier_token has an invalid process identity"
	terminated=$(pg_query postgres "SELECT count(*) FROM (SELECT pg_terminate_backend(pid) AS terminated FROM pg_stat_activity WHERE application_name='${barrier_token}') AS attempts WHERE terminated" 2>/dev/null || true)
	terminated=$(printf '%s' "$terminated" | tr -d '[:space:]')
	[ "$terminated" = 1 ] || fail "could not terminate PostgreSQL metadata barrier $barrier_token"
	wait_query_equals postgresql postgres \
		"SELECT count(*) FROM pg_stat_activity WHERE application_name='${barrier_token}'" \
		0 "the PostgreSQL metadata barrier backend to terminate"
	stop_pid "$barrier_pid"
	rm -f -- "$barrier_pid_file"
}

start_mysql_barrier() {
	barrier_database=$1
	barrier_token=$2
	assert_safe_name "$barrier_token"
	barrier_guard="${barrier_token}_guard"
	barrier_ready="${barrier_token}_ready"
	MYSQL_BARRIER_READY_LOCK=$barrier_ready
	barrier_sql="SELECT GET_LOCK('${barrier_guard}', 0); LOCK TABLES e2e_widgets READ; SELECT GET_LOCK('${barrier_ready}', 0); DO SLEEP(${FAULT_BARRIER_SECONDS}); UNLOCK TABLES; SELECT RELEASE_LOCK('${barrier_ready}'); SELECT RELEASE_LOCK('${barrier_guard}')"
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"' \
		sh "$barrier_database" "$barrier_sql" >"$WORK_DIR/${barrier_token}.log" 2>&1 &
	MYSQL_BARRIER_PID=$!
	wait_query_equals mysql mysql \
		"SELECT IF(IS_USED_LOCK('${barrier_ready}') IS NULL, 0, 1)" \
		1 "the MySQL metadata barrier to hold its table lock"
}

stop_mysql_barrier() {
	barrier_ready=$MYSQL_BARRIER_READY_LOCK
	[ -n "$barrier_ready" ] || return 0
	barrier_id=$(mysql_root_query mysql "SELECT IS_USED_LOCK('${barrier_ready}')" | tr -d '[:space:]')
	printf '%s\n' "$barrier_id" | grep -Eq '^[1-9][0-9]*$' ||
		fail "could not identify the MySQL metadata barrier connection"
	mysql_root_query mysql "KILL ${barrier_id}" >/dev/null
	stop_pid "$MYSQL_BARRIER_PID"
	MYSQL_BARRIER_PID=
	MYSQL_BARRIER_READY_LOCK=
}

assert_pg_apply_lock_wait() {
	lock_database=$1
	wait_query_equals postgresql "$lock_database" \
		"SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.classid=0 AND l.objid=${PTAH_PG_APPLY_LOCK_KEY} AND l.objsubid=1 AND l.granted AND a.datname='${lock_database}'" \
		1 "the exact PostgreSQL Ptah advisory lock"
	wait_query_equals postgresql "$lock_database" \
		"SELECT count(*) FROM pg_locks advisory JOIN pg_stat_activity activity ON activity.pid=advisory.pid JOIN pg_locks ddl ON ddl.pid=advisory.pid WHERE advisory.locktype='advisory' AND advisory.classid=0 AND advisory.objid=${PTAH_PG_APPLY_LOCK_KEY} AND advisory.objsubid=1 AND advisory.granted AND activity.datname='${lock_database}' AND ddl.locktype='relation' AND ddl.relation='public.e2e_widgets'::regclass AND ddl.mode='AccessExclusiveLock' AND NOT ddl.granted" \
		1 "the PostgreSQL Apply backend to block on the metadata barrier after acquiring its advisory lock"
}

assert_mysql_apply_lock_wait() {
	lock_database=$1
	wait_query_equals mysql mysql \
		"SELECT IF(IS_USED_LOCK('ptah_schema_apply') IS NULL, 0, 1)" \
		1 "the exact MySQL Ptah advisory lock"
	wait_query_equals mysql mysql \
		"SELECT count(*) FROM information_schema.processlist WHERE ID=IS_USED_LOCK('ptah_schema_apply') AND DB='${lock_database}' AND STATE LIKE '%metadata lock%'" \
		1 "the MySQL Apply backend to block on the metadata barrier after acquiring its advisory lock"
}

assert_active_identity() {
	identity_schema=$1
	identity_operation=$2
	identity_job=$3
	identity_job_uid=$4
	identity_pod=$5
	identity_pod_uid=$6
	wait_for_schema "$identity_schema" \
		".status.activeOperation.id == \"${identity_operation}\" and .status.activeOperation.jobName == \"${identity_job}\" and .status.activeOperation.jobUID == \"${identity_job_uid}\"" \
		"the original active operation after the controller restart"
	actual_pod_uid=$(k -n "$TEST_NAMESPACE" get pod "$identity_pod" -o jsonpath='{.metadata.uid}')
	[ "$actual_pod_uid" = "$identity_pod_uid" ] || fail "$identity_schema executor Pod was replaced across the controller restart"
}

wait_for_in_sync() {
	sync_schema=$1
	wait_for_schema "$sync_schema" '
      .status.phase == "InSync" and .status.pendingObservation == null and
      .status.activeOperation == null and .status.pendingLockRelease == null and
	  (.status.conditions | any(.type == "InSync" and .status == "True" and .reason == "ScopedConverged"))
    ' "post-apply observation to prove convergence"
}

run_credential_principal_refusal() {
	principal_database=e2e_fault_pg_principal
	principal_database_secret=e2e-fault-pg-principal-db
	principal_schema=e2e-fault-pg-principal
	principal_reference="oci://${REGISTRY_HOST}/schemas/credential-principal:stable"
	principal_plan_checkpoint=$WORK_DIR/principal-plan-checkpoint.json
	principal_apply_checkpoint=$WORK_DIR/principal-apply-checkpoint.json

	create_database postgresql "$principal_database" "$principal_database_secret"
	principal_fingerprint_before=$(postgres_schema_fingerprint "$principal_database" | tr -d '[:space:]')
	printf '%s\n' "$principal_fingerprint_before" | grep -Eq '^[0-9a-f]{32}$' ||
		fail "could not fingerprint the credential-principal test database"
	principal_role_before=$(pg_query "$principal_database" \
		"SELECT count(*) FROM pg_roles WHERE rolname='${PRINCIPAL_ROLE_NAME}'" | tr -d '[:space:]')
	[ "$principal_role_before" = 0 ] || fail "credential-principal fixture role already exists"
	publish_credential_principal_artifact "$principal_reference"
	checkpoint_operation_watch "$principal_schema" plan 0 "$principal_plan_checkpoint"
	checkpoint_operation_watch "$principal_schema" apply 0 "$principal_apply_checkpoint"
	create_schema "$principal_schema" PostgreSQL "$principal_database_secret" \
		"$principal_reference" e2e/fault/credential-principal 1h
	wait_for_one_new_watched_job "$principal_schema" plan "$principal_plan_checkpoint" \
		"one fail-closed Plan Job for the credential-bearing principal artifact"
	principal_plan_uid=$(single_new_watched_job_uid \
		"$principal_schema" plan "$principal_plan_checkpoint")
	principal_plan_job=$(k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${principal_schema},operator.ptah.dev/operation=plan" \
		-o json | jq -er --arg uid "$principal_plan_uid" '
		  [.items[] | select(.metadata.uid == $uid)] |
		  if length == 1 then .[0].metadata.name
		  else error("credential-principal Plan Job is not live exactly once") end
		')
	principal_result=$WORK_DIR/credential-principal-plan-result.json
	capture_exact_job_result "$principal_plan_job" "$principal_plan_uid" plan "$principal_result"
	PRINCIPAL_REFUSAL_SCHEMA=$principal_schema
	PRINCIPAL_PLAN_UID=$principal_plan_uid
	PRINCIPAL_PLAN_POD_UID=$FAULT_RESULT_POD_UID
	wait_for_schema "$principal_schema" '
	  (.spec.execution.failureRetryInterval | IN("1h", "1h0m0s")) and
	  .status.phase == "Failed" and
	  .status.activeOperation.type == "Plan" and
	  .status.activeOperation.attempt == 2 and
	  (.status.activeOperation.jobUID == null or .status.activeOperation.jobUID == "") and
	  (.status.conditions | any(
	    .type == "ReconciliationFailed" and .status == "True" and .reason == "OperationFailed")) and
	  (.status.nextReconciliationTime | fromdateiso8601) > (now + 3500)
	' "the credential-bearing principal Plan to fail closed with a long retry barrier"
	principal_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$principal_schema" -o json)
	printf '%s\n' "$principal_schema_object" | jq -e \
		--slurpfile result "$principal_result" \
		--arg digest "$PRINCIPAL_ARTIFACT_DIGEST" '
	  $result[0] as $result |
	  .status.source.digest == $digest and .status.source.verified == true and
	  .status.source.artifactType == "application/vnd.stokaro.ptah.schema.v1" and
	  (.status.target.driftReportDigest | test("^sha256:[0-9a-f]{64}$")) and
	  .status.plan == null and
	  .status.activeOperation.id == $result.operationId and
	  $result.childExitCode == 0 and $result.stdout == "" and
	  ($result.planContentDigest // "") == "" and
	  ($result.planOutcome // "") == "" and
	  ($result.mutationStarted // false) == false and
	  ($result.uncertain // false) == false and
	  $result.truncation == null and $result.error.code == "invalid_plan_output" and
	  $result.coordinationDigest == .status.target.coordinationDigest and
	  $result.targetIdentityDigest == .status.target.identityDigest
	' >/dev/null || fail "credential-bearing principal artifact did not reach the exact Plan refusal"
	principal_schema_uid=$(printf '%s\n' "$principal_schema_object" | jq -er '.metadata.uid')
	[ "$(k -n "$TEST_NAMESPACE" get ptahschemaplans -o json | jq \
		--arg uid "$principal_schema_uid" '[.items[] | select(.spec.schemaRef.uid == $uid)] | length')" -eq 0 ] ||
		fail "credential-bearing principal refusal published a PtahSchemaPlan"
	[ "$(k -n "$TEST_NAMESPACE" get configmaps \
		-l "operator.ptah.dev/schema=${principal_schema}" -o json | jq '.items | length')" -eq 0 ] ||
		fail "credential-bearing principal refusal published a plan chunk"
	[ "$(watch_new_uid_count "$principal_schema" apply "$principal_apply_checkpoint")" -eq 0 ] ||
		fail "credential-bearing principal refusal dispatched an Apply Job"
	[ "$(watch_new_uid_count "$principal_schema" plan "$principal_plan_checkpoint")" -eq 1 ] ||
		fail "credential-bearing principal refusal retried before its one-hour barrier"
	if [ "$(watch_added_uid_count "$principal_schema" resolve)" -ne 1 ] ||
		[ "$(watch_added_uid_count "$principal_schema" verify)" -ne 1 ] ||
		[ "$(watch_added_uid_count "$principal_schema" observe)" -ne 1 ]; then
		fail "credential-bearing principal refusal did not reach Plan through one exact read-only chain"
	fi
	[ "$(k -n "$TEST_NAMESPACE" get ptahschemaapprovals -o json | jq \
		--arg uid "$principal_schema_uid" '[.items[] | select(.spec.schemaRef.uid == $uid)] | length')" -eq 0 ] ||
		fail "credential-bearing principal refusal created an approval"
	principal_fingerprint_after=$(postgres_schema_fingerprint "$principal_database" | tr -d '[:space:]')
	[ "$principal_fingerprint_after" = "$principal_fingerprint_before" ] ||
		fail "credential-bearing principal refusal changed database SQL state"
	principal_role_after=$(pg_query "$principal_database" \
		"SELECT count(*) FROM pg_roles WHERE rolname='${PRINCIPAL_ROLE_NAME}'" | tr -d '[:space:]')
	[ "$principal_role_after" = 0 ] ||
		fail "credential-bearing principal refusal created its database role"
	# The CRD deliberately caps failure retries at one hour, shorter than the
	# suite's maximum budget. Suspend after the exact refusal evidence so a
	# second Plan cannot be hidden by an immediate-only count assertion.
	k -n "$TEST_NAMESPACE" patch ptahschema "$principal_schema" --type=merge \
		-p '{"spec":{"suspend":true}}' >/dev/null
	wait_for_schema "$principal_schema" '
	  .spec.suspend == true and .status.phase == "Suspended" and
	  .status.activeOperation == null and .status.pendingObservation == null and
	  (.status.conditions | any(
	    .type == "Suspended" and .status == "True" and .reason == "Requested"))
	' "the credential-bearing principal refusal to become durably suspended"
	[ "$(watch_new_uid_count "$principal_schema" plan "$principal_plan_checkpoint")" -eq 1 ] ||
		fail "suspending the credential-bearing principal refusal dispatched another Plan Job"
	[ "$(watch_new_uid_count "$principal_schema" apply "$principal_apply_checkpoint")" -eq 0 ] ||
		fail "suspending the credential-bearing principal refusal dispatched an Apply Job"
	audit_fault_runtime
}

create_credential_principal_secret
materialize_fault_credential_patterns
printf '%s\n' 'e2e faults: starting resourceVersion watches and fault-injection acceptance'
k -n "$TEST_NAMESPACE" get service "$PG_SERVICE" "$MYSQL_SERVICE" >/dev/null
k -n "$TEST_NAMESPACE" get secret "$PG_BASE_SECRET" "$MYSQL_BASE_SECRET" "$REGISTRY_AUTH_SECRET" "$REGISTRY_PULL_SECRET" >/dev/null
k -n "$TEST_NAMESPACE" get ptahschema e2e-suspended-schema >/dev/null
k -n "$TEST_NAMESPACE" get ptahschemaapproval e2e-approval >/dev/null
k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$CONTROLLER_NAME" --timeout="${TIMEOUT_SECONDS}s" >/dev/null

REGISTRY_HOST="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000"
PG_REFERENCE="oci://${REGISTRY_HOST}/schemas/fault-postgresql:stable"
MYSQL_REFERENCE="oci://${REGISTRY_HOST}/schemas/fault-mysql:stable"
publish_fault_schema postgresql postgres "$PG_REFERENCE"
publish_fault_schema mysql mysql "$MYSQL_REFERENCE"
create_watch_heartbeat_lease
start_watches
start_watch_heartbeat
audit_fault_runtime
run_credential_principal_refusal

PG_RESTART_DB=e2e_fault_pg_restart
PG_RESTART_SECRET=e2e-fault-pg-restart-db
PG_RESTART_SCHEMA=e2e-fault-pg-restart
PG_PARALLEL_DB=e2e_fault_pg_parallel
PG_PARALLEL_SECRET=e2e-fault-pg-parallel-db
PG_PARALLEL_SCHEMA=e2e-fault-pg-parallel
MYSQL_UNKNOWN_DB=e2e_fault_my_unknown
MYSQL_UNKNOWN_SECRET=e2e-fault-my-unknown-db
MYSQL_UNKNOWN_SCHEMA=e2e-fault-my-unknown
MYSQL_UNKNOWN_APPROVAL=e2e-fault-my-unknown-approval
MYSQL_TIMEOUT_DB=e2e_fault_my_timeout
MYSQL_TIMEOUT_SECRET=e2e-fault-my-timeout-db
MYSQL_TIMEOUT_SCHEMA=e2e-fault-my-timeout
MYSQL_TIMEOUT_APPROVAL=e2e-fault-my-timeout-approval

create_database postgresql "$PG_RESTART_DB" "$PG_RESTART_SECRET"
create_database postgresql "$PG_PARALLEL_DB" "$PG_PARALLEL_SECRET"
create_database mysql "$MYSQL_UNKNOWN_DB" "$MYSQL_UNKNOWN_SECRET"
create_database mysql "$MYSQL_TIMEOUT_DB" "$MYSQL_TIMEOUT_SECRET"

PG_RESTART_LEASES_BEFORE=$WORK_DIR/pg-restart-leases-before.json
checkpoint_leases "$PG_RESTART_LEASES_BEFORE"
create_schema "$PG_RESTART_SCHEMA" PostgreSQL "$PG_RESTART_SECRET" "$PG_REFERENCE" e2e/fault/pg-restart
wait_for_plan "$PG_RESTART_SCHEMA"
load_single_new_released_lease "$PG_RESTART_LEASES_BEFORE" \
	"the PostgreSQL restart schema's initial Plan target lock"
PG_RESTART_IDLE_LEASE_NAME=$LEASE_NAME
PG_RESTART_IDLE_LEASE_UID=$LEASE_UID
PG_RESTART_IDLE_LEASE_EPOCH=$LEASE_EPOCH

PG_PARALLEL_LEASES_BEFORE=$WORK_DIR/pg-parallel-leases-before.json
checkpoint_leases "$PG_PARALLEL_LEASES_BEFORE"
create_schema "$PG_PARALLEL_SCHEMA" PostgreSQL "$PG_PARALLEL_SECRET" "$PG_REFERENCE" e2e/fault/pg-parallel
wait_for_plan "$PG_PARALLEL_SCHEMA"
load_single_new_released_lease "$PG_PARALLEL_LEASES_BEFORE" \
	"the parallel PostgreSQL schema's initial Plan target lock"
PG_PARALLEL_IDLE_LEASE_NAME=$LEASE_NAME
PG_PARALLEL_IDLE_LEASE_UID=$LEASE_UID
PG_PARALLEL_IDLE_LEASE_EPOCH=$LEASE_EPOCH

MYSQL_LEASES_BEFORE=$WORK_DIR/mysql-unknown-leases-before.json
checkpoint_leases "$MYSQL_LEASES_BEFORE"
create_schema "$MYSQL_UNKNOWN_SCHEMA" MySQL "$MYSQL_UNKNOWN_SECRET" "$MYSQL_REFERENCE" e2e/fault/mysql-unknown
wait_for_plan "$MYSQL_UNKNOWN_SCHEMA"
load_single_new_released_lease "$MYSQL_LEASES_BEFORE" \
	"the MySQL uncertain schema's initial Plan target lock"
MYSQL_IDLE_LEASE_NAME=$LEASE_NAME
MYSQL_IDLE_LEASE_UID=$LEASE_UID
MYSQL_IDLE_LEASE_EPOCH=$LEASE_EPOCH
MYSQL_PREFAULT_SCHEMA_FINGERPRINT=$(mysql_schema_fingerprint "$MYSQL_UNKNOWN_DB" | tr -d '[:space:]')
printf '%s\n' "$MYSQL_PREFAULT_SCHEMA_FINGERPRINT" | grep -Eq '^[0-9a-f]{32}$' ||
	fail "could not fingerprint the complete pre-fault MySQL schema and index state"

MYSQL_TIMEOUT_LEASES_BEFORE=$WORK_DIR/mysql-timeout-leases-before.json
checkpoint_leases "$MYSQL_TIMEOUT_LEASES_BEFORE"
create_schema "$MYSQL_TIMEOUT_SCHEMA" MySQL "$MYSQL_TIMEOUT_SECRET" "$MYSQL_REFERENCE" \
	e2e/fault/mysql-timeout 1h "$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS" 30s
wait_for_plan "$MYSQL_TIMEOUT_SCHEMA"
load_single_new_released_lease "$MYSQL_TIMEOUT_LEASES_BEFORE" \
	"the Kubernetes-timeout schema's initial Plan target lock"
MYSQL_TIMEOUT_IDLE_LEASE_NAME=$LEASE_NAME
MYSQL_TIMEOUT_IDLE_LEASE_UID=$LEASE_UID
MYSQL_TIMEOUT_IDLE_LEASE_EPOCH=$LEASE_EPOCH
MYSQL_TIMEOUT_PREFAULT_SCHEMA_FINGERPRINT=$(mysql_schema_fingerprint "$MYSQL_TIMEOUT_DB" | tr -d '[:space:]')
printf '%s\n' "$MYSQL_TIMEOUT_PREFAULT_SCHEMA_FINGERPRINT" | grep -Eq '^[0-9a-f]{32}$' ||
	fail "could not fingerprint the pre-timeout MySQL schema and index state"
PG_RESTART_OBSERVE_CHECKPOINT=$WORK_DIR/pg-restart-observe-checkpoint.json
PG_RESTART_PLAN_CHECKPOINT=$WORK_DIR/pg-restart-plan-checkpoint.json
PG_PARALLEL_OBSERVE_CHECKPOINT=$WORK_DIR/pg-parallel-observe-checkpoint.json
PG_PARALLEL_PLAN_CHECKPOINT=$WORK_DIR/pg-parallel-plan-checkpoint.json
checkpoint_operation_watch "$PG_RESTART_SCHEMA" observe 1 "$PG_RESTART_OBSERVE_CHECKPOINT"
checkpoint_operation_watch "$PG_RESTART_SCHEMA" plan 1 "$PG_RESTART_PLAN_CHECKPOINT"
checkpoint_operation_watch "$PG_PARALLEL_SCHEMA" observe 1 "$PG_PARALLEL_OBSERVE_CHECKPOINT"
checkpoint_operation_watch "$PG_PARALLEL_SCHEMA" plan 1 "$PG_PARALLEL_PLAN_CHECKPOINT"
assert_database_column postgresql "$PG_RESTART_DB" fault_token 0
assert_database_column postgresql "$PG_PARALLEL_DB" fault_token 0
assert_database_column mysql "$MYSQL_UNKNOWN_DB" fault_token 0
MYSQL_ORIGINAL_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o jsonpath='{.status.plan.uid}')
[ -n "$MYSQL_ORIGINAL_PLAN_UID" ] || fail "uncertain-Apply schema did not persist its original plan UID"
MYSQL_PREFAULT_LAST_OBSERVED_AT=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o jsonpath='{.status.target.lastObservedAt}')
[ -n "$MYSQL_PREFAULT_LAST_OBSERVED_AT" ] || fail "uncertain-Apply schema lacks its pre-fault observation timestamp"
MYSQL_OBSERVE_CHECKPOINT=$WORK_DIR/mysql-uncertain-observe-checkpoint.json
checkpoint_operation_watch "$MYSQL_UNKNOWN_SCHEMA" observe 1 "$MYSQL_OBSERVE_CHECKPOINT"
MYSQL_PLAN_JOB_CHECKPOINT=$WORK_DIR/mysql-uncertain-plan-checkpoint.json
checkpoint_operation_watch "$MYSQL_UNKNOWN_SCHEMA" plan 1 "$MYSQL_PLAN_JOB_CHECKPOINT"
MYSQL_TIMEOUT_ORIGINAL_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema \
	"$MYSQL_TIMEOUT_SCHEMA" -o jsonpath='{.status.plan.uid}')
[ -n "$MYSQL_TIMEOUT_ORIGINAL_PLAN_UID" ] ||
	fail "Kubernetes-timeout schema did not persist its original plan UID"
MYSQL_TIMEOUT_OBSERVE_CHECKPOINT=$WORK_DIR/mysql-timeout-observe-checkpoint.json
MYSQL_TIMEOUT_PLAN_CHECKPOINT=$WORK_DIR/mysql-timeout-plan-checkpoint.json
checkpoint_operation_watch "$MYSQL_TIMEOUT_SCHEMA" observe 1 "$MYSQL_TIMEOUT_OBSERVE_CHECKPOINT"
checkpoint_operation_watch "$MYSQL_TIMEOUT_SCHEMA" plan 1 "$MYSQL_TIMEOUT_PLAN_CHECKPOINT"
assert_database_column mysql "$MYSQL_TIMEOUT_DB" fault_token 0

printf '%s\n' 'e2e faults: forcing one real Kubernetes Apply Job deadline'
start_read_workload_barrier
create_approval "$MYSQL_TIMEOUT_SCHEMA" "$MYSQL_TIMEOUT_APPROVAL"
wait_for_blocked_apply_pod "$MYSQL_TIMEOUT_SCHEMA" \
	"$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS"
MYSQL_TIMEOUT_OPERATION_ID=$ACTIVE_OPERATION_ID
MYSQL_TIMEOUT_JOB_NAME=$ACTIVE_JOB_NAME
MYSQL_TIMEOUT_JOB_UID=$ACTIVE_JOB_UID
MYSQL_TIMEOUT_POD_NAME=$ACTIVE_POD_NAME
MYSQL_TIMEOUT_POD_UID=$ACTIVE_POD_UID
record_deadline_pending_pod_evidence "$MYSQL_TIMEOUT_POD_NAME" \
	"$MYSQL_TIMEOUT_POD_UID" "$MYSQL_TIMEOUT_JOB_NAME" "$MYSQL_TIMEOUT_JOB_UID"
wait_for_lease_reacquisition "$MYSQL_TIMEOUT_IDLE_LEASE_NAME" \
	"$MYSQL_TIMEOUT_IDLE_LEASE_UID" "$MYSQL_TIMEOUT_IDLE_LEASE_EPOCH" \
	"the Kubernetes-timeout Apply"
MYSQL_TIMEOUT_LEASE_NAME=$LEASE_NAME
MYSQL_TIMEOUT_LEASE_UID=$LEASE_UID
MYSQL_TIMEOUT_LEASE_HOLDER=$LEASE_HOLDER
MYSQL_TIMEOUT_LEASE_EPOCH=$LEASE_EPOCH
wait_for_deadline_job_terminal_and_audit "$MYSQL_TIMEOUT_JOB_NAME" \
	"$MYSQL_TIMEOUT_JOB_UID" "$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS" \
	"$MYSQL_TIMEOUT_POD_UID"
wait_for_exact_pod_absence_after_evidence "$MYSQL_TIMEOUT_POD_NAME" \
	"$MYSQL_TIMEOUT_POD_UID"
capture_uncertain_read_proof_pair "$MYSQL_TIMEOUT_SCHEMA" \
	"$MYSQL_TIMEOUT_OPERATION_ID" "$MYSQL_TIMEOUT_JOB_NAME" "$MYSQL_TIMEOUT_JOB_UID" \
	"$MYSQL_TIMEOUT_POD_UID" "$MYSQL_TIMEOUT_LEASE_NAME" "$MYSQL_TIMEOUT_LEASE_UID" \
	"$MYSQL_TIMEOUT_LEASE_HOLDER" "$MYSQL_TIMEOUT_LEASE_EPOCH" \
	"$MYSQL_TIMEOUT_OBSERVE_CHECKPOINT" "$MYSQL_TIMEOUT_PLAN_CHECKPOINT" 0
MYSQL_TIMEOUT_RECOVERY_OBSERVE_UID=$UNCERTAIN_OBSERVE_JOB_UID
MYSQL_TIMEOUT_RECOVERY_PLAN_UID=$UNCERTAIN_PLAN_JOB_UID
wait_for_schema "$MYSQL_TIMEOUT_SCHEMA" '
  .status.pendingObservation == null and .status.activeOperation == null and
  .status.phase == "AwaitingApproval" and .status.plan.name != null and
  .status.plan.approval == null and .status.applied == null and
  .status.pendingLockRelease == null
' "a fresh unapproved plan after the Kubernetes Apply Job deadline"
MYSQL_TIMEOUT_FRESH_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema \
	"$MYSQL_TIMEOUT_SCHEMA" -o jsonpath='{.status.plan.uid}')
[ -n "$MYSQL_TIMEOUT_FRESH_PLAN_UID" ] ||
	fail "Kubernetes-timeout recovery did not publish an immutable fresh plan"
assert_approval_consumed "$MYSQL_TIMEOUT_APPROVAL" "$MYSQL_TIMEOUT_ORIGINAL_PLAN_UID"
[ "$(watch_added_uid_count "$MYSQL_TIMEOUT_SCHEMA" apply)" -eq 1 ] ||
	fail "Kubernetes-timeout recovery replayed or replaced its Apply Job"
[ "$(watch_added_pod_uid_count "$MYSQL_TIMEOUT_SCHEMA" apply)" -eq 1 ] ||
	fail "Kubernetes-timeout recovery created a replacement Apply Pod"
[ "$(watch_new_uid_count "$MYSQL_TIMEOUT_SCHEMA" observe "$MYSQL_TIMEOUT_OBSERVE_CHECKPOINT")" -eq 1 ] ||
	fail "Kubernetes-timeout recovery did not retain exactly one fresh Observe Job"
[ "$(watch_new_uid_count "$MYSQL_TIMEOUT_SCHEMA" plan "$MYSQL_TIMEOUT_PLAN_CHECKPOINT")" -eq 1 ] ||
	fail "Kubernetes-timeout recovery did not retain exactly one fresh Plan Job"
MYSQL_TIMEOUT_POSTRECOVERY_SCHEMA_FINGERPRINT=$(mysql_schema_fingerprint \
	"$MYSQL_TIMEOUT_DB" | tr -d '[:space:]')
[ "$MYSQL_TIMEOUT_POSTRECOVERY_SCHEMA_FINGERPRINT" = \
	"$MYSQL_TIMEOUT_PREFAULT_SCHEMA_FINGERPRINT" ] ||
	fail "Kubernetes-timeout recovery changed the database before fresh approval"
assert_database_column mysql "$MYSQL_TIMEOUT_DB" fault_token 0

start_pg_barrier "$PG_RESTART_DB" e2e_fault_pg_restart_barrier
start_pg_barrier "$PG_PARALLEL_DB" e2e_fault_pg_parallel_barrier
start_mysql_barrier "$MYSQL_UNKNOWN_DB" e2e_fault_my_unknown_barrier

create_approval "$PG_RESTART_SCHEMA" e2e-fault-pg-restart-approval
wait_for_apply_pod "$PG_RESTART_SCHEMA"
assert_active_pod_ephemeral_container_rejected "$PG_RESTART_SCHEMA"
PG_OPERATION_ID=$ACTIVE_OPERATION_ID
PG_JOB_NAME=$ACTIVE_JOB_NAME
PG_JOB_UID=$ACTIVE_JOB_UID
PG_POD_NAME=$ACTIVE_POD_NAME
PG_POD_UID=$ACTIVE_POD_UID
assert_pg_apply_lock_wait "$PG_RESTART_DB"
wait_for_lease_reacquisition "$PG_RESTART_IDLE_LEASE_NAME" \
	"$PG_RESTART_IDLE_LEASE_UID" "$PG_RESTART_IDLE_LEASE_EPOCH" \
	"the PostgreSQL restart Apply"
PG_LEASE_NAME=$LEASE_NAME
PG_LEASE_UID=$LEASE_UID
PG_LEASE_HOLDER=$LEASE_HOLDER
PG_LEASE_EPOCH=$LEASE_EPOCH

create_approval "$PG_PARALLEL_SCHEMA" e2e-fault-pg-parallel-approval
wait_for_apply_pod "$PG_PARALLEL_SCHEMA"
PG_PARALLEL_OPERATION_ID=$ACTIVE_OPERATION_ID
PG_PARALLEL_JOB_NAME=$ACTIVE_JOB_NAME
PG_PARALLEL_JOB_UID=$ACTIVE_JOB_UID
PG_PARALLEL_POD_NAME=$ACTIVE_POD_NAME
PG_PARALLEL_POD_UID=$ACTIVE_POD_UID
assert_pg_apply_lock_wait "$PG_PARALLEL_DB"
wait_for_lease_reacquisition "$PG_PARALLEL_IDLE_LEASE_NAME" \
	"$PG_PARALLEL_IDLE_LEASE_UID" "$PG_PARALLEL_IDLE_LEASE_EPOCH" \
	"the parallel PostgreSQL Apply"
PG_PARALLEL_LEASE_NAME=$LEASE_NAME
PG_PARALLEL_LEASE_UID=$LEASE_UID
PG_PARALLEL_LEASE_HOLDER=$LEASE_HOLDER
PG_PARALLEL_LEASE_EPOCH=$LEASE_EPOCH

create_approval "$MYSQL_UNKNOWN_SCHEMA" "$MYSQL_UNKNOWN_APPROVAL"
wait_for_apply_pod "$MYSQL_UNKNOWN_SCHEMA"
MYSQL_OPERATION_ID=$ACTIVE_OPERATION_ID
MYSQL_JOB_NAME=$ACTIVE_JOB_NAME
MYSQL_JOB_UID=$ACTIVE_JOB_UID
MYSQL_POD_NAME=$ACTIVE_POD_NAME
MYSQL_POD_UID=$ACTIVE_POD_UID
assert_mysql_apply_lock_wait "$MYSQL_UNKNOWN_DB"
wait_for_lease_reacquisition "$MYSQL_IDLE_LEASE_NAME" \
	"$MYSQL_IDLE_LEASE_UID" "$MYSQL_IDLE_LEASE_EPOCH" \
	"the uncertain MySQL Apply"
MYSQL_LEASE_NAME=$LEASE_NAME
MYSQL_LEASE_UID=$LEASE_UID
MYSQL_LEASE_HOLDER=$LEASE_HOLDER
MYSQL_LEASE_EPOCH=$LEASE_EPOCH
if [ "$PG_LEASE_UID" = "$PG_PARALLEL_LEASE_UID" ] ||
	[ "$PG_LEASE_UID" = "$MYSQL_LEASE_UID" ] ||
	[ "$PG_PARALLEL_LEASE_UID" = "$MYSQL_LEASE_UID" ]; then
	fail "distinct coordination keys collapsed into one target Lease"
fi

# Three Apply Pods and three independent operator Leases are live at this
# checkpoint. Both same-engine Pods hold database-local native advisory locks
# and block independently on their own metadata barriers.
assert_pg_apply_lock_wait "$PG_RESTART_DB"
assert_pg_apply_lock_wait "$PG_PARALLEL_DB"
assert_mysql_apply_lock_wait "$MYSQL_UNKNOWN_DB"

printf '%s\n' 'e2e faults: restarting the manager while three independent Apply Pods are active'
audit_fault_runtime
load_ready_manager_pod_uids
OLD_MANAGER_POD_UIDS=$MANAGER_POD_UIDS
load_ready_manager_leader
OLD_MANAGER_POD_NAME=$MANAGER_POD_NAME
OLD_MANAGER_POD_UID=$MANAGER_POD_UID
start_follow_logs "$OPERATOR_NAMESPACE" "$OLD_MANAGER_POD_NAME" manager-restart "$OLD_MANAGER_POD_UID"
k -n "$OPERATOR_NAMESPACE" rollout restart deployment/"$CONTROLLER_NAME" >/dev/null
k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$CONTROLLER_NAME" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
finish_follow_logs "old manager logs through the restart"
load_ready_manager_pod_uids
assert_manager_pods_replaced "$OLD_MANAGER_POD_UIDS" "$MANAGER_POD_UIDS"
load_ready_manager_leader "$OLD_MANAGER_POD_UID"
audit_fault_runtime
assert_active_identity "$PG_RESTART_SCHEMA" "$PG_OPERATION_ID" "$PG_JOB_NAME" "$PG_JOB_UID" "$PG_POD_NAME" "$PG_POD_UID"
assert_active_identity "$PG_PARALLEL_SCHEMA" "$PG_PARALLEL_OPERATION_ID" "$PG_PARALLEL_JOB_NAME" "$PG_PARALLEL_JOB_UID" "$PG_PARALLEL_POD_NAME" "$PG_PARALLEL_POD_UID"
assert_active_identity "$MYSQL_UNKNOWN_SCHEMA" "$MYSQL_OPERATION_ID" "$MYSQL_JOB_NAME" "$MYSQL_JOB_UID" "$MYSQL_POD_NAME" "$MYSQL_POD_UID"
assert_lease_identity "$PG_LEASE_NAME" "$PG_LEASE_UID" "$PG_LEASE_HOLDER" "$PG_LEASE_EPOCH"
assert_lease_identity "$PG_PARALLEL_LEASE_NAME" "$PG_PARALLEL_LEASE_UID" \
	"$PG_PARALLEL_LEASE_HOLDER" "$PG_PARALLEL_LEASE_EPOCH"
assert_lease_identity "$MYSQL_LEASE_NAME" "$MYSQL_LEASE_UID" "$MYSQL_LEASE_HOLDER" "$MYSQL_LEASE_EPOCH"
assert_pg_apply_lock_wait "$PG_RESTART_DB"
assert_pg_apply_lock_wait "$PG_PARALLEL_DB"
assert_mysql_apply_lock_wait "$MYSQL_UNKNOWN_DB"
[ "$(watch_added_uid_count "$PG_RESTART_SCHEMA" apply)" -eq 1 ] || fail "manager restart created a second PostgreSQL Apply Job"
[ "$(watch_added_uid_count "$PG_PARALLEL_SCHEMA" apply)" -eq 1 ] || fail "manager restart created a second parallel PostgreSQL Apply Job"
[ "$(watch_added_uid_count "$MYSQL_UNKNOWN_SCHEMA" apply)" -eq 1 ] || fail "manager restart created a second MySQL Apply Job"

printf '%s\n' 'e2e faults: terminating one active Apply runner without deleting its Pod'
audit_fault_runtime
start_read_workload_barrier
start_follow_logs "$TEST_NAMESPACE" "$MYSQL_POD_NAME" mysql-uncertain "$MYSQL_POD_UID"
# The API stream may close non-zero when signaling PID 1 tears down the same
# container. Natural log EOF below is the authoritative termination proof.
k -n "$TEST_NAMESPACE" exec pod/"$MYSQL_POD_NAME" -c ptah -- \
	/bin/kill -TERM 1 >/dev/null 2>&1 || true
finish_follow_logs "the signaled active MySQL Apply Pod logs through termination"
stop_mysql_barrier
wait_for_operation_job_terminal "$MYSQL_UNKNOWN_SCHEMA" apply
[ "$TERMINAL_OPERATION_ID" = "$MYSQL_OPERATION_ID" ] ||
	fail "uncertain MySQL Apply terminal Job changed its operation identity"
audit_protected_terminal_job "$MYSQL_JOB_NAME" "$MYSQL_JOB_UID" "$MYSQL_POD_UID"
wait_for_outcome_unknown_watch "$MYSQL_UNKNOWN_SCHEMA" "$MYSQL_OPERATION_ID"
wait_for_one_new_watched_job "$MYSQL_UNKNOWN_SCHEMA" observe "$MYSQL_OBSERVE_CHECKPOINT" \
	"a read-only Observe Job after the uncertain Apply"
MYSQL_RECOVERY_OBSERVE_UID=$(single_new_watched_job_uid \
	"$MYSQL_UNKNOWN_SCHEMA" observe "$MYSQL_OBSERVE_CHECKPOINT")
assert_read_workload_blocked "$MYSQL_RECOVERY_OBSERVE_UID" \
	"the read-only Observe Job after the uncertain Apply"
pause_controller_status_writes
stop_read_workload_barrier
MYSQL_RECOVERY_OBSERVE_JOB=$(k -n "$TEST_NAMESPACE" get jobs \
	-l "operator.ptah.dev/schema=${MYSQL_UNKNOWN_SCHEMA},operator.ptah.dev/operation=observe" \
	-o json | jq -er --arg uid "$MYSQL_RECOVERY_OBSERVE_UID" '
    [.items[] | select(.metadata.uid == $uid)] |
    if length == 1 then .[0].metadata.name else error("recovery Observe Job UID is not live exactly once") end
  ')
wait_for_exact_job_terminal "$MYSQL_RECOVERY_OBSERVE_JOB" "$MYSQL_RECOVERY_OBSERVE_UID"
k -n "$TEST_NAMESPACE" get job "$MYSQL_RECOVERY_OBSERVE_JOB" -o json |
	jq -e --arg uid "$MYSQL_RECOVERY_OBSERVE_UID" '
      .metadata.uid == $uid and
      (.status.conditions // [] | any(.type == "Complete" and .status == "True")) and
      (.metadata.annotations["operator.ptah.dev/operation-id"] | length > 0)
    ' >/dev/null || fail "the exact recovery Observe Job did not complete successfully"
MYSQL_RECOVERY_OBSERVE_RESULT=$WORK_DIR/mysql-recovery-observe-result.json
capture_exact_job_result "$MYSQL_RECOVERY_OBSERVE_JOB" "$MYSQL_RECOVERY_OBSERVE_UID" \
	observe "$MYSQL_RECOVERY_OBSERVE_RESULT"
MYSQL_HELD_OBSERVE=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o json)
printf '%s\n' "$MYSQL_HELD_OBSERVE" | jq -e \
	--arg observeUID "$MYSQL_RECOVERY_OBSERVE_UID" \
	--arg observeOperation "$FAULT_RESULT_OPERATION_ID" \
	--arg applyOperation "$MYSQL_OPERATION_ID" \
	--arg applyPodUID "$MYSQL_POD_UID" \
	--arg leaseEpoch "$MYSQL_LEASE_EPOCH" '
    .status.activeOperation.type == "Observe" and
    .status.activeOperation.id == $observeOperation and
    .status.activeOperation.jobUID == $observeUID and
    .status.activeOperation.leaseEpoch == $leaseEpoch and
    .status.pendingObservation.outcome == "OutcomeUnknown" and
    .status.pendingObservation.applyOperationID == $applyOperation and
    .status.pendingObservation.applyPodUIDs == [$applyPodUID] and
    .status.pendingObservation.applyPodCount == 1 and
    .status.pendingObservation.leaseEpoch == $leaseEpoch and
    (.status.pendingObservation.planRequired // false) == false and
    .status.applied == null and .status.pendingLockRelease == null and
    .status.phase == "VerifyingConvergence" and
    all(.status.conditions[];
      if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
  ' >/dev/null ||
	fail "uncertain MySQL Observe was harvested while status writes were held"
assert_lease_held_without_release "$MYSQL_LEASE_NAME" "$MYSQL_LEASE_UID" \
	"$MYSQL_LEASE_HOLDER" "$MYSQL_LEASE_EPOCH"
start_read_workload_barrier
resume_controller_status_writes ||
	fail "could not restore controller status-write RBAC after uncertain MySQL Observe"
wait_for_one_new_watched_job "$MYSQL_UNKNOWN_SCHEMA" plan "$MYSQL_PLAN_JOB_CHECKPOINT" \
	"a Plan Job derived after the exact recovery Observe"
MYSQL_RECOVERY_PLAN_JOB_UID=$(single_new_watched_job_uid \
	"$MYSQL_UNKNOWN_SCHEMA" plan "$MYSQL_PLAN_JOB_CHECKPOINT")
assert_read_workload_blocked "$MYSQL_RECOVERY_PLAN_JOB_UID" \
	"the read-only Plan Job after the uncertain Apply"
pause_controller_status_writes
stop_read_workload_barrier
MYSQL_RECOVERY_PLAN_JOB=$(k -n "$TEST_NAMESPACE" get jobs \
	-l "operator.ptah.dev/schema=${MYSQL_UNKNOWN_SCHEMA},operator.ptah.dev/operation=plan" \
	-o json | jq -er --arg uid "$MYSQL_RECOVERY_PLAN_JOB_UID" '
    [.items[] | select(.metadata.uid == $uid)] |
    if length == 1 then .[0].metadata.name else error("recovery Plan Job UID is not live exactly once") end
  ')
wait_for_exact_job_terminal "$MYSQL_RECOVERY_PLAN_JOB" "$MYSQL_RECOVERY_PLAN_JOB_UID"
k -n "$TEST_NAMESPACE" get job "$MYSQL_RECOVERY_PLAN_JOB" -o json |
	jq -e --arg uid "$MYSQL_RECOVERY_PLAN_JOB_UID" '
      .metadata.uid == $uid and
      (.status.conditions // [] | any(.type == "Complete" and .status == "True")) and
      (.metadata.annotations["operator.ptah.dev/operation-id"] | length > 0)
    ' >/dev/null || fail "the exact recovery Plan Job did not complete successfully"
MYSQL_RECOVERY_PLAN_RESULT=$WORK_DIR/mysql-recovery-plan-result.json
capture_exact_job_result "$MYSQL_RECOVERY_PLAN_JOB" "$MYSQL_RECOVERY_PLAN_JOB_UID" \
	plan "$MYSQL_RECOVERY_PLAN_RESULT"
MYSQL_HELD_SCHEMA=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o json)
printf '%s\n' "$MYSQL_HELD_SCHEMA" | jq -e \
	--arg planUID "$MYSQL_RECOVERY_PLAN_JOB_UID" \
	--arg planOperation "$FAULT_RESULT_OPERATION_ID" \
	--arg applyOperation "$MYSQL_OPERATION_ID" \
	--arg applyPodUID "$MYSQL_POD_UID" \
	--arg leaseEpoch "$MYSQL_LEASE_EPOCH" '
    .status.activeOperation.type == "Plan" and
    .status.activeOperation.id == $planOperation and
    .status.activeOperation.jobUID == $planUID and
    .status.activeOperation.leaseEpoch == $leaseEpoch and
    .status.pendingObservation.outcome == "OutcomeUnknown" and
    .status.pendingObservation.applyOperationID == $applyOperation and
    .status.pendingObservation.applyPodUIDs == [$applyPodUID] and
    .status.pendingObservation.applyPodCount == 1 and
    .status.pendingObservation.leaseEpoch == $leaseEpoch and
    .status.pendingObservation.planRequired == true and
    .status.applied == null and .status.pendingLockRelease == null and
    .status.phase == "VerifyingConvergence" and
    all(.status.conditions[];
      if (.type == "InSync" or .type == "Ready") then .status != "True" else true end)
  ' >/dev/null ||
	fail "uncertain MySQL proof Plan was harvested while status writes were held"
assert_lease_held_without_release "$MYSQL_LEASE_NAME" "$MYSQL_LEASE_UID" \
	"$MYSQL_LEASE_HOLDER" "$MYSQL_LEASE_EPOCH"
resume_controller_status_writes ||
	fail "could not restore controller status-write RBAC after uncertain MySQL proof"
wait_for_schema "$MYSQL_UNKNOWN_SCHEMA" '
  .status.pendingObservation == null and .status.activeOperation == null and
  .status.phase == "AwaitingApproval" and .status.plan.name != null and
  .status.applied == null and .status.pendingLockRelease == null
' "read-only observation and a fresh unapproved plan after the uncertain Apply"
snapshot_watch jobs
jq -s -e \
	--arg observeUID "$MYSQL_RECOVERY_OBSERVE_UID" \
	--arg planUID "$MYSQL_RECOVERY_PLAN_JOB_UID" '
    (to_entries | map(select(
      .value.object.metadata.uid == $observeUID and
      (.value.object.status.conditions // [] |
        any(.type == "Complete" and .status == "True")))) | map(.key) | min) as $observeComplete |
    (to_entries | map(select(
      .value.type == "ADDED" and .value.object.metadata.uid == $planUID)) |
      map(.key) | min) as $planAdded |
    $observeComplete != null and $planAdded != null and $observeComplete < $planAdded
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "recovery Plan Job was not ordered after the exact completed Observe Job"
MYSQL_FRESH_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o jsonpath='{.status.plan.uid}')
[ -n "$MYSQL_FRESH_PLAN_UID" ] ||
	fail "uncertain MySQL Apply recovery did not publish an immutable plan"
MYSQL_FRESH_PLAN_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o jsonpath='{.status.plan.name}')
MYSQL_POSTFAULT_SCHEMA=$(k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o json)
printf '%s\n' "$MYSQL_POSTFAULT_SCHEMA" | jq -e \
	--slurpfile observe "$MYSQL_RECOVERY_OBSERVE_RESULT" \
	--arg before "$MYSQL_PREFAULT_LAST_OBSERVED_AT" '
	  $observe[0] as $observe |
      (.status.target.lastObservedAt | fromdateiso8601) > ($before | fromdateiso8601) and
      .status.plan.approval == null and
	  (.status.target.driftReportDigest | test("^sha256:[0-9a-f]{64}$")) and
	  $observe.error == null and $observe.stdout == "" and
	  ($observe.childExitCode == 0 or $observe.childExitCode == 1) and
	  $observe.observedDrift == true and $observe.driftFindingCount > 0 and
	  ($observe.observedDialect | IN("mysql", "mariadb")) and
	  $observe.coordinationDigest == .status.target.coordinationDigest and
	  $observe.targetIdentityDigest == .status.target.identityDigest and
	  $observe.driftReportDigest == .status.target.driftReportDigest
    ' >/dev/null || fail "recovery Observe did not advance the target observation evidence"
MYSQL_RECOVERY_PLAN_DOCUMENT=$WORK_DIR/mysql-recovery-plan.json
jq -jr '.stdout' "$MYSQL_RECOVERY_PLAN_RESULT" >"$MYSQL_RECOVERY_PLAN_DOCUMENT"
chmod 600 "$MYSQL_RECOVERY_PLAN_DOCUMENT"
scan_fault_file "$MYSQL_RECOVERY_PLAN_DOCUMENT" "the exact recovery native plan"
k -n "$TEST_NAMESPACE" get ptahschemaplan "$MYSQL_FRESH_PLAN_NAME" -o json | jq -e \
	--slurpfile result "$MYSQL_RECOVERY_PLAN_RESULT" \
	--slurpfile document "$MYSQL_RECOVERY_PLAN_DOCUMENT" \
	--arg uid "$MYSQL_FRESH_PLAN_UID" '
	  $result[0] as $result | $document[0] as $document |
	  .metadata.uid == $uid and $result.error == null and
	  $result.childExitCode == 0 and $result.planOutcome == "Changes" and
	  $result.planContentDigest == .spec.contentDigest and
	  $result.coordinationDigest == .spec.coordinationDigest and
	  $result.targetIdentityDigest == .spec.targetIdentityDigest and
	  .spec.actualStateFingerprint == $document.from_fingerprint and
	  .spec.desiredStateFingerprint == $document.to_fingerprint
	' >/dev/null || fail "fresh MySQL plan is not bound to the exact recovery Plan result"
assert_approval_consumed "$MYSQL_UNKNOWN_APPROVAL" "$MYSQL_ORIGINAL_PLAN_UID"
[ "$(watch_added_uid_count "$MYSQL_UNKNOWN_SCHEMA" apply)" -eq 1 ] ||
	fail "the uncertain MySQL Apply was replayed without a fresh approval"
[ "$(watch_added_pod_uid_count "$MYSQL_UNKNOWN_SCHEMA" apply)" -eq 1 ] ||
	fail "podReplacementPolicy=Failed allowed a second executor Pod for the failed Apply Job"
assert_database_column mysql "$MYSQL_UNKNOWN_DB" fault_token 0
MYSQL_POSTRECOVERY_SCHEMA_FINGERPRINT=$(mysql_schema_fingerprint "$MYSQL_UNKNOWN_DB" | tr -d '[:space:]')
[ "$MYSQL_POSTRECOVERY_SCHEMA_FINGERPRINT" = "$MYSQL_PREFAULT_SCHEMA_FINGERPRINT" ] ||
	fail "uncertain MySQL recovery changed a column or index outside the planned mutation"

start_read_workload_barrier
stop_pg_barrier e2e_fault_pg_restart_barrier
PG_RESTART_APPLY_RESULT=$WORK_DIR/pg-restart-apply-result.json
capture_exact_job_result "$PG_JOB_NAME" "$PG_JOB_UID" apply "$PG_RESTART_APPLY_RESULT"
[ "$FAULT_RESULT_OPERATION_ID" = "$PG_OPERATION_ID" ] ||
	fail "restarted PostgreSQL Apply result changed its immutable operation identity"
assert_successful_apply_result "$PG_RESTART_SCHEMA" "$PG_OPERATION_ID" \
	"$PG_JOB_UID" "$PG_POD_UID" "$PG_RESTART_APPLY_RESULT"
assert_fault_convergence_result_pair "$PG_RESTART_SCHEMA" \
	"$PG_RESTART_OBSERVE_CHECKPOINT" "$PG_RESTART_PLAN_CHECKPOINT" \
	"$PG_OPERATION_ID" "$PG_LEASE_NAME" "$PG_LEASE_UID" "$PG_LEASE_HOLDER" "$PG_LEASE_EPOCH"
PG_RESTART_PROOF_OBSERVE_JOB_UID=$CONVERGED_OBSERVE_JOB_UID
PG_RESTART_PROOF_PLAN_JOB_UID=$CONVERGED_PLAN_JOB_UID

start_read_workload_barrier
stop_pg_barrier e2e_fault_pg_parallel_barrier
PG_PARALLEL_APPLY_RESULT=$WORK_DIR/pg-parallel-apply-result.json
capture_exact_job_result "$PG_PARALLEL_JOB_NAME" "$PG_PARALLEL_JOB_UID" \
	apply "$PG_PARALLEL_APPLY_RESULT"
[ "$FAULT_RESULT_OPERATION_ID" = "$PG_PARALLEL_OPERATION_ID" ] ||
	fail "parallel PostgreSQL Apply result changed its immutable operation identity"
assert_successful_apply_result "$PG_PARALLEL_SCHEMA" "$PG_PARALLEL_OPERATION_ID" \
	"$PG_PARALLEL_JOB_UID" "$PG_PARALLEL_POD_UID" "$PG_PARALLEL_APPLY_RESULT"
assert_fault_convergence_result_pair "$PG_PARALLEL_SCHEMA" \
	"$PG_PARALLEL_OBSERVE_CHECKPOINT" "$PG_PARALLEL_PLAN_CHECKPOINT" \
	"$PG_PARALLEL_OPERATION_ID" "$PG_PARALLEL_LEASE_NAME" "$PG_PARALLEL_LEASE_UID" \
	"$PG_PARALLEL_LEASE_HOLDER" "$PG_PARALLEL_LEASE_EPOCH"
PG_PARALLEL_PROOF_OBSERVE_JOB_UID=$CONVERGED_OBSERVE_JOB_UID
PG_PARALLEL_PROOF_PLAN_JOB_UID=$CONVERGED_PLAN_JOB_UID
[ "$(watch_added_uid_count "$PG_RESTART_SCHEMA" apply)" -eq 1 ] ||
	fail "the restarted PostgreSQL Apply was duplicated"
[ "$(watch_added_uid_count "$PG_PARALLEL_SCHEMA" apply)" -eq 1 ] ||
	fail "the independent PostgreSQL Apply was duplicated"
[ "$(watch_added_pod_uid_count "$PG_RESTART_SCHEMA" apply)" -eq 1 ] ||
	fail "the restarted PostgreSQL Apply created a second executor Pod"
[ "$(watch_added_pod_uid_count "$PG_PARALLEL_SCHEMA" apply)" -eq 1 ] ||
	fail "the independent PostgreSQL Apply created a second executor Pod"
assert_database_column postgresql "$PG_RESTART_DB" fault_token 1
assert_database_column postgresql "$PG_PARALLEL_DB" fault_token 1

PG_ALIAS_DB=e2e_fault_pg_alias
PG_ALIAS_SECRET_A=e2e-fault-pg-alias-fqdn-db
PG_ALIAS_SECRET_B=e2e-fault-pg-alias-short-db
PG_ALIAS_SCHEMA_A=e2e-fault-pg-alias-a
PG_ALIAS_SCHEMA_B=e2e-fault-pg-alias-b
create_database postgresql "$PG_ALIAS_DB" "$PG_ALIAS_SECRET_A"
create_url_secret postgresql "$PG_ALIAS_DB" "$PG_ALIAS_SECRET_B" short
[ "$(k -n "$TEST_NAMESPACE" get secret "$PG_ALIAS_SECRET_A" -o jsonpath='{.data.url}')" != \
	"$(k -n "$TEST_NAMESPACE" get secret "$PG_ALIAS_SECRET_B" -o jsonpath='{.data.url}')" ] ||
	fail "coordination alias Secrets contain identical routes"
ALIAS_LEASES_BEFORE=$WORK_DIR/alias-leases-before.json
checkpoint_leases "$ALIAS_LEASES_BEFORE"
create_schema "$PG_ALIAS_SCHEMA_A" PostgreSQL "$PG_ALIAS_SECRET_A" "$PG_REFERENCE" e2e/fault/shared-alias
create_schema "$PG_ALIAS_SCHEMA_B" PostgreSQL "$PG_ALIAS_SECRET_B" "$PG_REFERENCE" e2e/fault/shared-alias
wait_for_plan "$PG_ALIAS_SCHEMA_A"
ALIAS_A_IDENTITY=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_A" -o jsonpath='{.status.target.identityDigest}')
ALIAS_A_COORDINATION=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_A" -o jsonpath='{.status.target.coordinationDigest}')
wait_for_plan "$PG_ALIAS_SCHEMA_B"
ALIAS_B_IDENTITY=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_B" -o jsonpath='{.status.target.identityDigest}')
ALIAS_B_COORDINATION=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_B" -o jsonpath='{.status.target.coordinationDigest}')
load_single_new_released_lease "$ALIAS_LEASES_BEFORE" \
	"the shared alias realm's initial Plan target lock"
ALIAS_IDLE_LEASE_NAME=$LEASE_NAME
ALIAS_IDLE_LEASE_UID=$LEASE_UID
ALIAS_IDLE_LEASE_EPOCH=$LEASE_EPOCH
if [ -z "$ALIAS_A_IDENTITY" ] || [ -z "$ALIAS_B_IDENTITY" ] ||
	[ "$ALIAS_A_IDENTITY" = "$ALIAS_B_IDENTITY" ]; then
	fail "the two database URL aliases did not retain distinct target identities"
fi
printf '%s\n' "$ALIAS_A_COORDINATION" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "the first database URL alias has no persisted coordination digest"
[ "$ALIAS_A_COORDINATION" = "$ALIAS_B_COORDINATION" ] ||
	fail "one coordination key produced different persisted realms across database URL aliases"

start_pg_barrier "$PG_ALIAS_DB" e2e_fault_pg_alias_barrier
ALIAS_A_OBSERVE_CHECKPOINT=$WORK_DIR/alias-a-observe-checkpoint.json
ALIAS_A_PLAN_CHECKPOINT=$WORK_DIR/alias-a-plan-checkpoint.json
ALIAS_B_OBSERVE_CHECKPOINT=$WORK_DIR/alias-b-observe-checkpoint.json
ALIAS_B_PLAN_CHECKPOINT=$WORK_DIR/alias-b-plan-checkpoint.json
checkpoint_operation_watch "$PG_ALIAS_SCHEMA_A" observe 1 "$ALIAS_A_OBSERVE_CHECKPOINT"
checkpoint_operation_watch "$PG_ALIAS_SCHEMA_A" plan 1 "$ALIAS_A_PLAN_CHECKPOINT"
checkpoint_operation_watch "$PG_ALIAS_SCHEMA_B" observe 1 "$ALIAS_B_OBSERVE_CHECKPOINT"
checkpoint_operation_watch "$PG_ALIAS_SCHEMA_B" plan 1 "$ALIAS_B_PLAN_CHECKPOINT"
ALIAS_A_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_A" \
	-o jsonpath='{.status.plan.uid}')
create_approval "$PG_ALIAS_SCHEMA_A" e2e-fault-pg-alias-a-approval
wait_for_apply_pod "$PG_ALIAS_SCHEMA_A"
ALIAS_A_OPERATION_ID=$ACTIVE_OPERATION_ID
ALIAS_A_JOB_NAME=$ACTIVE_JOB_NAME
ALIAS_A_JOB_UID=$ACTIVE_JOB_UID
ALIAS_A_POD_UID=$ACTIVE_POD_UID
assert_pg_apply_lock_wait "$PG_ALIAS_DB"
wait_for_lease_reacquisition "$ALIAS_IDLE_LEASE_NAME" "$ALIAS_IDLE_LEASE_UID" \
	"$ALIAS_IDLE_LEASE_EPOCH" "the shared alias holder Apply"
ALIAS_LEASE_NAME=$LEASE_NAME
ALIAS_LEASE_UID=$LEASE_UID
ALIAS_LEASE_HOLDER=$LEASE_HOLDER
ALIAS_LEASE_EPOCH=$LEASE_EPOCH

ALIAS_B_APPROVAL=e2e-fault-pg-alias-b-approval
ALIAS_B_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_B" -o jsonpath='{.status.plan.uid}')
[ -n "$ALIAS_B_PLAN_UID" ] || fail "shared-alias contender did not persist its original plan UID"
ALIAS_B_PLAN_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_B" -o jsonpath='{.status.plan.name}')
ALIAS_B_PLAN_OBJECT=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$ALIAS_B_PLAN_NAME" -o json)
ALIAS_B_OLD_PLAN_CONTENT_DIGEST=$(printf '%s\n' "$ALIAS_B_PLAN_OBJECT" | jq -er '.spec.contentDigest')
ALIAS_B_OLD_COORDINATION_DIGEST=$(printf '%s\n' "$ALIAS_B_PLAN_OBJECT" | jq -er '.spec.coordinationDigest')
ALIAS_B_OLD_TARGET_IDENTITY_DIGEST=$(printf '%s\n' "$ALIAS_B_PLAN_OBJECT" | jq -er '.spec.targetIdentityDigest')
create_approval "$PG_ALIAS_SCHEMA_B" "$ALIAS_B_APPROVAL"
wait_for_schema "$PG_ALIAS_SCHEMA_B" '
  .status.activeOperation.type == "Apply" and
  (.status.activeOperation.jobUID == null or .status.activeOperation.jobUID == "") and
  .status.activeOperation.dispatchStarted != true
' "an undispatched Apply claim blocked by the shared target Lease"
sleep 8
ALIAS_B_JOB_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_B" -o jsonpath='{.status.activeOperation.jobName}')
if k -n "$TEST_NAMESPACE" get job "$ALIAS_B_JOB_NAME" >/dev/null 2>&1; then
	fail "the shared coordination key allowed the alias contender to dispatch an Apply Job"
fi
[ "$(watch_added_uid_count "$PG_ALIAS_SCHEMA_B" apply)" -eq 0 ] ||
	fail "the shared coordination key allowed an alias contender Apply Job"
assert_lease_identity "$ALIAS_LEASE_NAME" "$ALIAS_LEASE_UID" \
	"$ALIAS_LEASE_HOLDER" "$ALIAS_LEASE_EPOCH"

# Releasing the first holder must make the exact contender progress once. This
# distinguishes real Lease contention from a builder, RBAC, or reconcile stall.
start_read_workload_barrier
stop_pg_barrier e2e_fault_pg_alias_barrier
ALIAS_A_APPLY_RESULT=$WORK_DIR/alias-a-apply-result.json
capture_exact_job_result "$ALIAS_A_JOB_NAME" "$ALIAS_A_JOB_UID" apply \
	"$ALIAS_A_APPLY_RESULT"
assert_successful_apply_result "$PG_ALIAS_SCHEMA_A" "$ALIAS_A_OPERATION_ID" \
	"$ALIAS_A_JOB_UID" "$ALIAS_A_POD_UID" "$ALIAS_A_APPLY_RESULT"
assert_fault_convergence_result_pair "$PG_ALIAS_SCHEMA_A" \
	"$ALIAS_A_OBSERVE_CHECKPOINT" "$ALIAS_A_PLAN_CHECKPOINT" \
	"$ALIAS_A_OPERATION_ID" "$ALIAS_LEASE_NAME" "$ALIAS_LEASE_UID" \
	"$ALIAS_LEASE_HOLDER" "$ALIAS_LEASE_EPOCH" "$PG_ALIAS_SCHEMA_B"
ALIAS_A_PROOF_OBSERVE_JOB_UID=$CONVERGED_OBSERVE_JOB_UID
ALIAS_A_PROOF_PLAN_JOB_UID=$CONVERGED_PLAN_JOB_UID
assert_approval_consumed e2e-fault-pg-alias-a-approval "$ALIAS_A_PLAN_UID"
establish_watch_barrier schemas "$TEST_NAMESPACE" ptahschema "$PG_ALIAS_SCHEMA_A"
establish_watch_barrier jobs "$TEST_NAMESPACE" job "$ALIAS_A_JOB_NAME"
establish_watch_barrier leases "$OPERATOR_NAMESPACE" lease "$ALIAS_LEASE_NAME"
assert_post_apply_proof_history "$PG_ALIAS_SCHEMA_A" "$ALIAS_A_OPERATION_ID" \
	"$ALIAS_A_JOB_UID" "$ALIAS_LEASE_UID" "$ALIAS_LEASE_HOLDER" \
	"$ALIAS_LEASE_EPOCH" "$ALIAS_A_PROOF_OBSERVE_JOB_UID" "$ALIAS_A_PROOF_PLAN_JOB_UID"
wait_for_watch_count_greater_than "$PG_ALIAS_SCHEMA_B" apply 0 \
	"the shared-alias contender to dispatch after the holder released its Lease"
snapshot_watch jobs
ALIAS_B_JOB_UID=$(jq -er -s \
	--arg schema "$PG_ALIAS_SCHEMA_B" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique |
    if length == 1 then .[0] else error("expected exactly one alias contender Apply Job UID") end
  ' "$WATCH_SNAPSHOT_FILE")
wait_for_apply_binding_in_schema_watch "$PG_ALIAS_SCHEMA_B" "$ALIAS_B_JOB_UID" \
	"the exact shared-alias contender Apply binding"
ALIAS_B_OPERATION_ID=$WATCH_ACTIVE_OPERATION_ID
ALIAS_B_LEASE_EPOCH=$WATCH_ACTIVE_LEASE_EPOCH
assert_read_workload_blocked "$ALIAS_B_JOB_UID" \
	"the stale shared-alias contender Apply Job"
load_held_lease_for_epoch "$ALIAS_B_LEASE_EPOCH" \
	"the exact shared-alias contender Apply Lease"
ALIAS_B_LEASE_NAME=$LEASE_NAME
ALIAS_B_LEASE_UID=$LEASE_UID
ALIAS_B_LEASE_HOLDER=$LEASE_HOLDER
[ "$ALIAS_B_LEASE_UID" = "$ALIAS_LEASE_UID" ] ||
	fail "the shared-alias contender acquired a different target Lease UID"
ALIAS_B_DATABASE_FINGERPRINT_BEFORE=$(postgres_schema_fingerprint "$PG_ALIAS_DB" | tr -d '[:space:]')
pause_controller_status_writes
stop_read_workload_barrier
wait_for_operation_job_terminal "$PG_ALIAS_SCHEMA_B" apply
[ "$TERMINAL_JOB_UID" = "$ALIAS_B_JOB_UID" ] ||
	fail "shared-alias contender terminal Job changed its exact UID"
ALIAS_B_JOB_NAME=$TERMINAL_JOB_NAME
ALIAS_B_APPLY_RESULT=$WORK_DIR/alias-b-stale-apply-result.json
capture_exact_job_result "$ALIAS_B_JOB_NAME" "$ALIAS_B_JOB_UID" apply \
	"$ALIAS_B_APPLY_RESULT"
[ "$FAULT_RESULT_OPERATION_ID" = "$ALIAS_B_OPERATION_ID" ] ||
	fail "shared-alias contender Apply result changed its immutable operation identity"
ALIAS_B_POD_UID=$FAULT_RESULT_POD_UID
jq -e \
	--arg contentDigest "$ALIAS_B_OLD_PLAN_CONTENT_DIGEST" \
	--arg coordinationDigest "$ALIAS_B_OLD_COORDINATION_DIGEST" \
	--arg targetIdentityDigest "$ALIAS_B_OLD_TARGET_IDENTITY_DIGEST" '
      .childExitCode == 2 and .stdout == "" and
      .planContentDigest == $contentDigest and
      .coordinationDigest == $coordinationDigest and
      .targetIdentityDigest == $targetIdentityDigest and
      (.mutationStarted // false) == true and
      (.uncertain // false) == true and
      (.planOutcome // "") == "" and .truncation == null and
      .error.code == "stale_plan"
    ' "$ALIAS_B_APPLY_RESULT" >/dev/null ||
	fail "shared-alias contender did not return the exact conservative stale-plan result"
ALIAS_B_DATABASE_FINGERPRINT_AFTER_APPLY=$(postgres_schema_fingerprint "$PG_ALIAS_DB" | tr -d '[:space:]')
[ "$ALIAS_B_DATABASE_FINGERPRINT_AFTER_APPLY" = "$ALIAS_B_DATABASE_FINGERPRINT_BEFORE" ] ||
	fail "the stale shared-alias contender changed the database schema"

start_read_workload_barrier
resume_controller_status_writes ||
	fail "could not restore status-write RBAC for shared-alias uncertainty proof"
capture_uncertain_read_proof_pair "$PG_ALIAS_SCHEMA_B" "$ALIAS_B_OPERATION_ID" \
	"$ALIAS_B_JOB_NAME" "$ALIAS_B_JOB_UID" "$ALIAS_B_POD_UID" \
	"$ALIAS_B_LEASE_NAME" "$ALIAS_B_LEASE_UID" "$ALIAS_B_LEASE_HOLDER" \
	"$ALIAS_B_LEASE_EPOCH" "$ALIAS_B_OBSERVE_CHECKPOINT" "$ALIAS_B_PLAN_CHECKPOINT"
ALIAS_B_RECOVERY_OBSERVE_UID=$UNCERTAIN_OBSERVE_JOB_UID
ALIAS_B_RECOVERY_PLAN_UID=$UNCERTAIN_PLAN_JOB_UID
ALIAS_B_RECOVERY_OBSERVE_RESULT=$UNCERTAIN_OBSERVE_RESULT
ALIAS_B_RECOVERY_PLAN_RESULT=$UNCERTAIN_PLAN_RESULT
wait_for_in_sync "$PG_ALIAS_SCHEMA_B"
assert_approval_consumed "$ALIAS_B_APPROVAL" "$ALIAS_B_PLAN_UID"
ALIAS_B_FINAL_SCHEMA=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_ALIAS_SCHEMA_B" -o json)
printf '%s\n' "$ALIAS_B_FINAL_SCHEMA" | jq -e \
	--slurpfile observe "$ALIAS_B_RECOVERY_OBSERVE_RESULT" \
	--slurpfile plan "$ALIAS_B_RECOVERY_PLAN_RESULT" '
      $observe[0] as $observe | $plan[0] as $plan |
      .status.phase == "InSync" and .status.activeOperation == null and
      .status.pendingObservation == null and .status.pendingLockRelease == null and
      .status.applied == null and .status.plan == null and
      ($observe.error == null and $observe.childExitCode == 0 and
        $observe.stdout == "" and ($observe.observedDrift // false) == false and
        ($observe.highestDriftSeverity // "") == "" and
        ($observe.driftFindingCount // 0) == 0 and
        ($observe.observedDialect | IN("postgres", "postgresql")) and
        $observe.coordinationDigest == .status.target.coordinationDigest and
        $observe.targetIdentityDigest == .status.target.identityDigest and
        $observe.driftReportDigest == .status.target.driftReportDigest) and
      ($plan.error == null and $plan.childExitCode == 0 and
        $plan.planOutcome == "NoChanges" and $plan.stdout == "" and
        ($plan.planContentDigest // "") == "" and
        $plan.coordinationDigest == .status.target.coordinationDigest and
        $plan.targetIdentityDigest == .status.target.identityDigest)
    ' >/dev/null ||
	fail "shared-alias stale recovery did not finish through exact read-only Observe and Plan results"
k -n "$TEST_NAMESPACE" get ptahschemaapproval "$ALIAS_B_APPROVAL" -o json | jq -e '
    (.status.conditions | any(
      .type == "Consumed" and .status == "True" and .reason == "DispatchCommitted")) and
    (.status.conditions | any(
      .type == "Stale" and .status == "True" and .reason == "PlanNoLongerCurrent"))
  ' >/dev/null || fail "shared-alias consumed approval was not marked stale after recovery"
ALIAS_B_DATABASE_FINGERPRINT_AFTER_PROOF=$(postgres_schema_fingerprint "$PG_ALIAS_DB" | tr -d '[:space:]')
[ "$ALIAS_B_DATABASE_FINGERPRINT_AFTER_PROOF" = "$ALIAS_B_DATABASE_FINGERPRINT_BEFORE" ] ||
	fail "the shared-alias read-only proof changed the database schema"
establish_watch_barrier schemas "$TEST_NAMESPACE" ptahschema "$PG_ALIAS_SCHEMA_B"
ALIAS_B_RECOVERY_PLAN_JOB=$(k -n "$TEST_NAMESPACE" get jobs -o json | jq -er \
	--arg uid "$ALIAS_B_RECOVERY_PLAN_UID" '
      [.items[] | select(.metadata.uid == $uid)] |
      if length == 1 then .[0].metadata.name
      else error("shared-alias recovery Plan Job UID is not live exactly once") end
    ')
establish_watch_barrier jobs "$TEST_NAMESPACE" job "$ALIAS_B_RECOVERY_PLAN_JOB"
establish_watch_barrier leases "$OPERATOR_NAMESPACE" lease "$ALIAS_B_LEASE_NAME"
snapshot_watch jobs
jq -s -e \
	--arg holderUID "$ALIAS_A_JOB_UID" \
	--arg contenderUID "$ALIAS_B_JOB_UID" '
    (to_entries | map(select(
      .value.object.metadata.uid == $holderUID and
      (.value.object.status.conditions // [] |
        any(.type == "Complete" and .status == "True")))) | map(.key) | min) as $holderComplete |
    (to_entries | map(select(
      .value.type == "ADDED" and .value.object.metadata.uid == $contenderUID)) |
      map(.key) | min) as $contenderAdded |
    $holderComplete != null and $contenderAdded != null and $holderComplete < $contenderAdded
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "shared-alias contender dispatched before the exact holder Job completed"
[ "$(watch_added_uid_count "$PG_ALIAS_SCHEMA_A" apply)" -eq 1 ] ||
	fail "the shared-alias holder did not execute exactly one Apply Job"
[ "$(watch_added_uid_count "$PG_ALIAS_SCHEMA_B" apply)" -eq 1 ] ||
	fail "the shared-alias contender did not dispatch exactly once after Lease release"
[ "$(watch_added_pod_uid_count "$PG_ALIAS_SCHEMA_B" apply)" -eq 1 ] ||
	fail "the shared-alias stale Apply created more than one executor Pod"
snapshot_watch pods
jq -s -e \
	--arg schema "$PG_ALIAS_SCHEMA_B" \
	--arg uid "$ALIAS_B_POD_UID" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique == [$uid]
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "shared-alias stale Apply history does not contain exactly its result Pod UID"
assert_same_lease_reacquired_in_watch "$ALIAS_LEASE_UID" \
	"$ALIAS_LEASE_HOLDER" "$ALIAS_LEASE_EPOCH" "$PG_ALIAS_SCHEMA_B" \
	"$ALIAS_B_OPERATION_ID" "$ALIAS_B_JOB_UID" "$ALIAS_B_LEASE_EPOCH"
assert_uncertain_apply_proof_history "$PG_ALIAS_SCHEMA_B" "$ALIAS_B_OPERATION_ID" \
	"$ALIAS_B_JOB_UID" "$ALIAS_B_LEASE_UID" "$ALIAS_B_LEASE_HOLDER" \
	"$ALIAS_B_LEASE_EPOCH" "$ALIAS_B_RECOVERY_OBSERVE_UID" \
	"$ALIAS_B_RECOVERY_PLAN_UID" "" "$ALIAS_B_POD_UID" no-changes
assert_database_column postgresql "$PG_ALIAS_DB" fault_token 1

# Keep a consumed, user-owned approval whose schema no longer exists. It is a
# side-effect-free sentinel for closing the approval watch at an exact API
# resourceVersion after all operational scenarios.
audit_fault_runtime
for alias_b_fully_audited_job_uid in \
	"$ALIAS_B_JOB_UID" "$ALIAS_B_RECOVERY_OBSERVE_UID" "$ALIAS_B_RECOVERY_PLAN_UID"; do
	[ -n "$alias_b_fully_audited_job_uid" ] ||
		fail "shared-alias cascade boundary has an empty exact Job UID"
	grep -Fx "$alias_b_fully_audited_job_uid" "$SHARED_FULLY_AUDITED_JOBS_FILE" >/dev/null ||
		fail "shared-alias cascade boundary would delete a Job without a full container audit"
done
k -n "$TEST_NAMESPACE" delete ptahschema "$PG_ALIAS_SCHEMA_B" --wait=false >/dev/null
wait_for_absence ptahschema "$PG_ALIAS_SCHEMA_B"
k -n "$TEST_NAMESPACE" get ptahschemaapproval "$ALIAS_B_APPROVAL" >/dev/null ||
	fail "the user-owned consumed approval unexpectedly disappeared with its schema"

PG_DELETE_DB=e2e_fault_pg_delete
PG_DELETE_SECRET=e2e-fault-pg-delete-db
PG_DELETE_SCHEMA=e2e-fault-pg-delete
create_database postgresql "$PG_DELETE_DB" "$PG_DELETE_SECRET"
create_schema "$PG_DELETE_SCHEMA" PostgreSQL "$PG_DELETE_SECRET" "$PG_REFERENCE" e2e/fault/delete
wait_for_plan "$PG_DELETE_SCHEMA"
assert_database_column postgresql "$PG_DELETE_DB" fault_token 0
DELETE_FINGERPRINT_BEFORE=$(postgres_schema_fingerprint "$PG_DELETE_DB" | tr -d '[:space:]')
printf '%s\n' "$DELETE_FINGERPRINT_BEFORE" | grep -Eq '^[0-9a-f]{32}$' ||
	fail "could not fingerprint the deletion-test database"
DELETED_SCHEMA_NAME=$PG_DELETE_SCHEMA
DELETED_DATABASE_NAME=$PG_DELETE_DB
DELETED_DATABASE_FINGERPRINT=$DELETE_FINGERPRINT_BEFORE
DELETED_JOB_CHECKPOINT=$WORK_DIR/deleted-schema-job-checkpoint.json
checkpoint_schema_job_watch "$PG_DELETE_SCHEMA" "$DELETED_JOB_CHECKPOINT"
audit_fault_runtime
k -n "$TEST_NAMESPACE" delete ptahschema "$PG_DELETE_SCHEMA" --wait=false >/dev/null
wait_for_absence ptahschema "$PG_DELETE_SCHEMA"
DELETE_FINGERPRINT_AFTER=$(postgres_schema_fingerprint "$PG_DELETE_DB" | tr -d '[:space:]')
[ "$DELETE_FINGERPRINT_AFTER" = "$DELETE_FINGERPRINT_BEFORE" ] ||
	fail "deleting an AwaitingApproval PtahSchema changed the database schema"
assert_database_column postgresql "$PG_DELETE_DB" fault_token 0

PG_MANUAL_DB=e2e_fault_pg_manual
PG_MANUAL_SECRET=e2e-fault-pg-manual-db
PG_MANUAL_SCHEMA=e2e-fault-pg-manual
PG_MANUAL_APPROVAL=e2e-fault-pg-manual-approval
create_database postgresql "$PG_MANUAL_DB" "$PG_MANUAL_SECRET"
MANUAL_LEASES_BEFORE=$WORK_DIR/manual-leases-before.json
checkpoint_leases "$MANUAL_LEASES_BEFORE"
create_schema "$PG_MANUAL_SCHEMA" PostgreSQL "$PG_MANUAL_SECRET" "$PG_REFERENCE" e2e/fault/manual
wait_for_plan "$PG_MANUAL_SCHEMA"
load_single_new_released_lease "$MANUAL_LEASES_BEFORE" \
	"the manual-drift schema's initial Plan target lock"
MANUAL_IDLE_LEASE_NAME=$LEASE_NAME
MANUAL_IDLE_LEASE_UID=$LEASE_UID
MANUAL_IDLE_LEASE_EPOCH=$LEASE_EPOCH
MANUAL_OLD_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_MANUAL_SCHEMA" -o jsonpath='{.status.plan.uid}')
[ -n "$MANUAL_OLD_PLAN_UID" ] || fail "manual-drift schema did not persist the old plan UID"
MANUAL_OLD_PLAN_NAME=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_MANUAL_SCHEMA" -o jsonpath='{.status.plan.name}')
MANUAL_OLD_ACTUAL_FINGERPRINT=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$MANUAL_OLD_PLAN_NAME" -o jsonpath='{.spec.actualStateFingerprint}')
MANUAL_OLD_PLAN_CONTENT_DIGEST=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$MANUAL_OLD_PLAN_NAME" -o jsonpath='{.spec.contentDigest}')
MANUAL_OLD_COORDINATION_DIGEST=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$MANUAL_OLD_PLAN_NAME" -o jsonpath='{.spec.coordinationDigest}')
MANUAL_OLD_TARGET_IDENTITY_DIGEST=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$MANUAL_OLD_PLAN_NAME" -o jsonpath='{.spec.targetIdentityDigest}')
printf '%s\n' "$MANUAL_OLD_ACTUAL_FINGERPRINT" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "manual-drift schema did not persist its old actual-state fingerprint"
printf '%s\n' "$MANUAL_OLD_PLAN_CONTENT_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "manual-drift schema did not persist its old plan content digest"
printf '%s\n' "$MANUAL_OLD_COORDINATION_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "manual-drift schema did not persist its old coordination digest"
printf '%s\n' "$MANUAL_OLD_TARGET_IDENTITY_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "manual-drift schema did not persist its old target identity digest"
MANUAL_OBSERVE_CHECKPOINT=$WORK_DIR/manual-observe-checkpoint.json
MANUAL_PLAN_CHECKPOINT=$WORK_DIR/manual-plan-checkpoint.json
checkpoint_operation_watch "$PG_MANUAL_SCHEMA" observe 1 "$MANUAL_OBSERVE_CHECKPOINT"
checkpoint_operation_watch "$PG_MANUAL_SCHEMA" plan 1 "$MANUAL_PLAN_CHECKPOINT"
pause_controller_status_writes
create_approval "$PG_MANUAL_SCHEMA" "$PG_MANUAL_APPROVAL"
sleep 3
[ "$(watch_added_uid_count "$PG_MANUAL_SCHEMA" apply)" -eq 0 ] ||
	fail "manual-drift Apply dispatched while controller status writes were denied"
pg_query "$PG_MANUAL_DB" 'ALTER TABLE e2e_widgets DROP COLUMN enabled' >/dev/null
assert_database_column postgresql "$PG_MANUAL_DB" enabled 0
assert_database_column postgresql "$PG_MANUAL_DB" fault_token 0
MANUAL_FINGERPRINT_BEFORE=$(postgres_schema_fingerprint "$PG_MANUAL_DB" | tr -d '[:space:]')
start_read_workload_barrier
resume_controller_status_writes || fail "could not restore controller status-write RBAC after the manual-drift barrier"
wait_for_watch_count_greater_than "$PG_MANUAL_SCHEMA" apply 0 \
	"the manual-drift Apply Job to be created behind the scheduling barrier"
snapshot_watch jobs
MANUAL_APPLY_JOB_UID=$(jq -er -s \
	--arg schema "$PG_MANUAL_SCHEMA" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique |
    if length == 1 then .[0] else error("expected one manual-drift Apply Job UID") end
  ' "$WATCH_SNAPSHOT_FILE")
wait_for_apply_binding_in_schema_watch "$PG_MANUAL_SCHEMA" "$MANUAL_APPLY_JOB_UID" \
	"the exact manual-drift Apply binding"
MANUAL_OPERATION_ID=$WATCH_ACTIVE_OPERATION_ID
MANUAL_LEASE_EPOCH=$WATCH_ACTIVE_LEASE_EPOCH
MANUAL_APPLY_JOB_NAME=$(k -n "$TEST_NAMESPACE" get jobs -o json | jq -er \
	--arg uid "$MANUAL_APPLY_JOB_UID" '
      [.items[] | select(.metadata.uid == $uid)] |
      if length == 1 then .[0].metadata.name
      else error("manual-drift Apply Job UID is not live exactly once") end
    ')
assert_read_workload_blocked "$MANUAL_APPLY_JOB_UID" \
	"the manual-drift stale Apply Job"
wait_for_lease_reacquisition "$MANUAL_IDLE_LEASE_NAME" "$MANUAL_IDLE_LEASE_UID" \
	"$MANUAL_IDLE_LEASE_EPOCH" "the manual-drift stale Apply"
MANUAL_LEASE_NAME=$LEASE_NAME
MANUAL_LEASE_UID=$LEASE_UID
MANUAL_LEASE_HOLDER=$LEASE_HOLDER
[ "$LEASE_EPOCH" = "$MANUAL_LEASE_EPOCH" ] ||
	fail "manual-drift Apply schema binding and live Lease epoch differ"
pause_controller_status_writes
stop_read_workload_barrier
wait_for_operation_job_terminal "$PG_MANUAL_SCHEMA" apply
if [ "$TERMINAL_OPERATION_ID" != "$MANUAL_OPERATION_ID" ] ||
	[ "$TERMINAL_JOB_UID" != "$MANUAL_APPLY_JOB_UID" ]; then
	fail "manual-drift terminal Apply changed its exact operation or Job identity"
fi
MANUAL_STALE_RESULT=$WORK_DIR/manual-stale-apply-result.json
capture_exact_job_result "$MANUAL_APPLY_JOB_NAME" "$MANUAL_APPLY_JOB_UID" apply "$MANUAL_STALE_RESULT"
[ "$FAULT_RESULT_OPERATION_ID" = "$MANUAL_OPERATION_ID" ] ||
	fail "manual-drift Apply result changed its immutable operation identity"
MANUAL_APPLY_POD_UID=$FAULT_RESULT_POD_UID
jq -e \
	--arg contentDigest "$MANUAL_OLD_PLAN_CONTENT_DIGEST" \
	--arg coordinationDigest "$MANUAL_OLD_COORDINATION_DIGEST" \
	--arg targetIdentityDigest "$MANUAL_OLD_TARGET_IDENTITY_DIGEST" '
	  .childExitCode == 2 and .stdout == "" and
	  .planContentDigest == $contentDigest and
	  .coordinationDigest == $coordinationDigest and
	  .targetIdentityDigest == $targetIdentityDigest and
	  (.mutationStarted // false) == true and
	  (.uncertain // false) == true and
	  (.planOutcome // "") == "" and .truncation == null and
	  .error.code == "stale_plan"
	' "$MANUAL_STALE_RESULT" >/dev/null ||
	fail "manual-drift Apply did not return the exact conservative stale-plan result"
MANUAL_FINGERPRINT_AFTER=$(postgres_schema_fingerprint "$PG_MANUAL_DB" | tr -d '[:space:]')
[ "$MANUAL_FINGERPRINT_AFTER" = "$MANUAL_FINGERPRINT_BEFORE" ] ||
	fail "a stale approved plan executed SQL after the database changed manually"
assert_database_column postgresql "$PG_MANUAL_DB" enabled 0
assert_database_column postgresql "$PG_MANUAL_DB" fault_token 0
start_read_workload_barrier
resume_controller_status_writes ||
	fail "could not restore status-write RBAC for manual-drift uncertainty proof"
capture_uncertain_read_proof_pair "$PG_MANUAL_SCHEMA" "$MANUAL_OPERATION_ID" \
	"$MANUAL_APPLY_JOB_NAME" "$MANUAL_APPLY_JOB_UID" "$MANUAL_APPLY_POD_UID" \
	"$MANUAL_LEASE_NAME" "$MANUAL_LEASE_UID" "$MANUAL_LEASE_HOLDER" \
	"$MANUAL_LEASE_EPOCH" "$MANUAL_OBSERVE_CHECKPOINT" "$MANUAL_PLAN_CHECKPOINT"
MANUAL_FRESH_OBSERVE_UID=$UNCERTAIN_OBSERVE_JOB_UID
MANUAL_FRESH_PLAN_JOB_UID=$UNCERTAIN_PLAN_JOB_UID
MANUAL_FRESH_OBSERVE_RESULT=$UNCERTAIN_OBSERVE_RESULT
MANUAL_FRESH_PLAN_RESULT=$UNCERTAIN_PLAN_RESULT
wait_for_manual_drift_contract "$PG_MANUAL_SCHEMA" "$PG_MANUAL_APPROVAL" \
	"$MANUAL_OPERATION_ID" "$MANUAL_OLD_PLAN_UID" \
	"$MANUAL_OLD_ACTUAL_FINGERPRINT" "$MANUAL_OBSERVE_CHECKPOINT"
MANUAL_FRESH_SCHEMA=$(k -n "$TEST_NAMESPACE" get ptahschema "$PG_MANUAL_SCHEMA" -o json)
MANUAL_FRESH_PLAN_NAME=$(printf '%s\n' "$MANUAL_FRESH_SCHEMA" | jq -er '.status.plan.name')
MANUAL_FRESH_PLAN_UID=$(printf '%s\n' "$MANUAL_FRESH_SCHEMA" | jq -er '.status.plan.uid')
printf '%s\n' "$MANUAL_FRESH_SCHEMA" | jq -e \
	--slurpfile observe "$MANUAL_FRESH_OBSERVE_RESULT" '
      $observe[0] as $observe |
      $observe.error == null and
      ($observe.childExitCode == 0 or $observe.childExitCode == 1) and
      $observe.stdout == "" and $observe.observedDrift == true and
      $observe.driftFindingCount > 0 and
      ($observe.observedDialect | IN("postgres", "postgresql")) and
      $observe.coordinationDigest == .status.target.coordinationDigest and
      $observe.targetIdentityDigest == .status.target.identityDigest and
      $observe.driftReportDigest == .status.target.driftReportDigest
    ' >/dev/null || fail "manual-drift fresh Observe result is not bound to its exact target evidence"
MANUAL_FRESH_PLAN_DOCUMENT=$WORK_DIR/manual-fresh-plan.json
jq -jr '.stdout' "$MANUAL_FRESH_PLAN_RESULT" >"$MANUAL_FRESH_PLAN_DOCUMENT"
chmod 600 "$MANUAL_FRESH_PLAN_DOCUMENT"
scan_fault_file "$MANUAL_FRESH_PLAN_DOCUMENT" "the manual-drift fresh native plan"
k -n "$TEST_NAMESPACE" get ptahschemaplan "$MANUAL_FRESH_PLAN_NAME" -o json | jq -e \
	--slurpfile result "$MANUAL_FRESH_PLAN_RESULT" \
	--slurpfile document "$MANUAL_FRESH_PLAN_DOCUMENT" \
	--arg oldUID "$MANUAL_OLD_PLAN_UID" '
      $result[0] as $result | $document[0] as $document |
      .metadata.uid != $oldUID and $result.error == null and
      $result.childExitCode == 0 and $result.planOutcome == "Changes" and
      $result.planContentDigest == .spec.contentDigest and
      $result.coordinationDigest == .spec.coordinationDigest and
      $result.targetIdentityDigest == .spec.targetIdentityDigest and
      .spec.actualStateFingerprint == $document.from_fingerprint and
      .spec.desiredStateFingerprint == $document.to_fingerprint
    ' >/dev/null || fail "manual-drift fresh plan is not bound to the exact Plan result"
assert_approval_consumed "$PG_MANUAL_APPROVAL" "$MANUAL_OLD_PLAN_UID"
MANUAL_FINGERPRINT_AFTER_PROOF=$(postgres_schema_fingerprint "$PG_MANUAL_DB" | tr -d '[:space:]')
[ "$MANUAL_FINGERPRINT_AFTER_PROOF" = "$MANUAL_FINGERPRINT_BEFORE" ] ||
	fail "manual-drift read-only Observe or Plan changed the database schema"
assert_database_column postgresql "$PG_MANUAL_DB" enabled 0
assert_database_column postgresql "$PG_MANUAL_DB" fault_token 0
snapshot_watch jobs
jq -s -e \
	--arg observeUID "$MANUAL_FRESH_OBSERVE_UID" \
	--arg planUID "$MANUAL_FRESH_PLAN_JOB_UID" '
      (to_entries | map(select(
        .value.object.metadata.uid == $observeUID and
        (.value.object.status.conditions // [] | any(
          .type == "Complete" and .status == "True"))) | .key) | min) as $observeComplete |
      (to_entries | map(select(
        .value.type == "ADDED" and .value.object.metadata.uid == $planUID) | .key) | min) as $planAdded |
      $observeComplete != null and $planAdded != null and $observeComplete < $planAdded
    ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "manual-drift fresh Plan was dispatched before the exact Observe result completed"
[ "$(watch_added_uid_count "$PG_MANUAL_SCHEMA" apply)" -eq 1 ] ||
	fail "manual drift caused a blind Apply replay"
[ "$(watch_added_pod_uid_count "$PG_MANUAL_SCHEMA" apply)" -eq 1 ] ||
	fail "manual drift created a replacement Apply Pod"
watch_has_outcome_unknown "$PG_MANUAL_SCHEMA" "$MANUAL_OPERATION_ID" ||
	fail "manual drift lost its conservative OutcomeUnknown history"
MANUAL_FRESH_PLAN_JOB=$(k -n "$TEST_NAMESPACE" get jobs -o json | jq -er \
	--arg uid "$MANUAL_FRESH_PLAN_JOB_UID" '
      [.items[] | select(.metadata.uid == $uid)] |
      if length == 1 then .[0].metadata.name
      else error("manual-drift recovery Plan Job UID is not live exactly once") end
    ')
establish_watch_barrier schemas "$TEST_NAMESPACE" ptahschema "$PG_MANUAL_SCHEMA"
establish_watch_barrier jobs "$TEST_NAMESPACE" job "$MANUAL_FRESH_PLAN_JOB"
establish_watch_barrier leases "$OPERATOR_NAMESPACE" lease "$MANUAL_LEASE_NAME"
assert_uncertain_apply_proof_history "$PG_MANUAL_SCHEMA" "$MANUAL_OPERATION_ID" \
	"$MANUAL_APPLY_JOB_UID" "$MANUAL_LEASE_UID" "$MANUAL_LEASE_HOLDER" \
	"$MANUAL_LEASE_EPOCH" "$MANUAL_FRESH_OBSERVE_UID" \
	"$MANUAL_FRESH_PLAN_JOB_UID" "$MANUAL_FRESH_PLAN_UID" \
	"$MANUAL_APPLY_POD_UID" manual-drift "$MANUAL_OLD_ACTUAL_FINGERPRINT"

# Keep the no-SQL deletion proof open through every later reconciliation and
# fault scenario, rather than accepting only a short quiet interval.
snapshot_watch jobs
jq -s -e \
	--slurpfile before "$DELETED_JOB_CHECKPOINT" \
	--arg schema "$DELETED_SCHEMA_NAME" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      .object.metadata.uid as $uid |
      select(($before[0] | index($uid)) == null)] |
    length == 0
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "$DELETED_SCHEMA_NAME created an operation Job at some point after deletion began"
DELETE_FINGERPRINT_FINAL=$(postgres_schema_fingerprint "$DELETED_DATABASE_NAME" | tr -d '[:space:]')
[ "$DELETE_FINGERPRINT_FINAL" = "$DELETED_DATABASE_FINGERPRINT" ] ||
	fail "the deletion-test database changed during the full post-delete observation window"
assert_database_column postgresql "$DELETED_DATABASE_NAME" fault_token 0

# Recheck uncertainty safety after every later reconcile in the suite. A
# consumed approval must remain ineligible indefinitely, not only until the
# first recovery Observe finishes.
assert_approval_consumed "$MYSQL_UNKNOWN_APPROVAL" "$MYSQL_ORIGINAL_PLAN_UID"
k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_UNKNOWN_SCHEMA" -o json |
	jq -e '
      .status.activeOperation == null and .status.pendingObservation == null and
	  .status.pendingLockRelease == null and
	  .status.plan.uid != null and .status.plan.approval == null
    ' >/dev/null || fail "uncertain MySQL Apply later became active or rebound its consumed approval"
assert_database_column mysql "$MYSQL_UNKNOWN_DB" fault_token 0
MYSQL_FINAL_SCHEMA_FINGERPRINT=$(mysql_schema_fingerprint "$MYSQL_UNKNOWN_DB" | tr -d '[:space:]')
[ "$MYSQL_FINAL_SCHEMA_FINGERPRINT" = "$MYSQL_PREFAULT_SCHEMA_FINGERPRINT" ] ||
	fail "uncertain MySQL Apply changed a column or index during the delayed-replay window"
assert_approval_consumed "$MYSQL_TIMEOUT_APPROVAL" "$MYSQL_TIMEOUT_ORIGINAL_PLAN_UID"
k -n "$TEST_NAMESPACE" get ptahschema "$MYSQL_TIMEOUT_SCHEMA" -o json |
	jq -e '
      .status.activeOperation == null and .status.pendingObservation == null and
      .status.pendingLockRelease == null and .status.applied == null and
      .status.phase == "AwaitingApproval" and
      .status.plan.uid != null and .status.plan.approval == null
    ' >/dev/null ||
	fail "Kubernetes-timeout recovery later became active or rebound its consumed approval"
MYSQL_TIMEOUT_FINAL_SCHEMA_FINGERPRINT=$(mysql_schema_fingerprint \
	"$MYSQL_TIMEOUT_DB" | tr -d '[:space:]')
[ "$MYSQL_TIMEOUT_FINAL_SCHEMA_FINGERPRINT" = \
	"$MYSQL_TIMEOUT_PREFAULT_SCHEMA_FINGERPRINT" ] ||
	fail "Kubernetes-timeout recovery changed the database during the delayed-replay window"
assert_database_column mysql "$MYSQL_TIMEOUT_DB" fault_token 0

audit_fault_runtime
PG_DATABASE_SENTINEL_POD=$(k -n "$TEST_NAMESPACE" get pods \
	-l "app.kubernetes.io/name=${PG_SERVICE},app.kubernetes.io/component=e2e-database" \
	-o json | jq -er '
    [.items[] | select(.metadata.deletionTimestamp == null)] |
    if length == 1 then .[0].metadata.name else error("expected one PostgreSQL database Pod") end
  ')
establish_watch_barrier approvals "$TEST_NAMESPACE" ptahschemaapproval "$ALIAS_B_APPROVAL"
establish_watch_barrier schemas "$TEST_NAMESPACE" ptahschema e2e-suspended-schema
establish_watch_barrier leases "$OPERATOR_NAMESPACE" lease "$PG_LEASE_NAME"
establish_watch_barrier jobs "$TEST_NAMESPACE" job e2e-fault-push-postgresql
establish_watch_barrier pods "$TEST_NAMESPACE" pod "$PG_DATABASE_SENTINEL_POD"
stop_watch_heartbeat
assert_watches_alive
stop_watches
audit_fault_runtime
for watch_file in $WATCH_FILES; do
	validate_watch_file "$watch_file"
	scan_fault_file "$watch_file" "fault-test Kubernetes watch history"
done
snapshot_watch jobs
jq -s -e \
	--slurpfile before "$DELETED_JOB_CHECKPOINT" \
	--arg schema "$DELETED_SCHEMA_NAME" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      .object.metadata.uid as $uid |
      select(($before[0] | index($uid)) == null)] |
    length == 0
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "$DELETED_SCHEMA_NAME created an operation Job after the deletion evidence boundary"
if [ "$(watch_added_uid_count "$PG_RESTART_SCHEMA" apply)" -ne 1 ] ||
	[ "$(watch_added_pod_uid_count "$PG_RESTART_SCHEMA" apply)" -ne 1 ]; then
	fail "the restarted PostgreSQL Apply history is not exactly one Job and Pod UID"
fi
if [ "$(watch_added_uid_count "$PG_PARALLEL_SCHEMA" apply)" -ne 1 ] ||
	[ "$(watch_added_pod_uid_count "$PG_PARALLEL_SCHEMA" apply)" -ne 1 ]; then
	fail "the parallel PostgreSQL Apply history is not exactly one Job and Pod UID"
fi
if [ "$(watch_added_uid_count "$PG_ALIAS_SCHEMA_A" apply)" -ne 1 ] ||
	[ "$(watch_added_uid_count "$PG_ALIAS_SCHEMA_B" apply)" -ne 1 ] ||
	[ "$(watch_added_pod_uid_count "$PG_ALIAS_SCHEMA_A" apply)" -ne 1 ] ||
	[ "$(watch_added_pod_uid_count "$PG_ALIAS_SCHEMA_B" apply)" -ne 1 ]; then
	fail "the shared-alias Apply history contains a delayed replay"
fi
snapshot_watch pods
jq -s -e \
	--arg schema "$PG_ALIAS_SCHEMA_B" \
	--arg uid "$ALIAS_B_POD_UID" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique == [$uid]
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "shared-alias stale Apply final history changed its exact Pod UID"
[ "$(watch_added_uid_count "$PG_MANUAL_SCHEMA" apply)" -eq 1 ] ||
	fail "manual drift caused a delayed Apply replay"
[ "$(watch_added_pod_uid_count "$PG_MANUAL_SCHEMA" apply)" -eq 1 ] ||
	fail "manual drift created a delayed replacement Apply Pod"
snapshot_watch jobs
jq -s -e \
	--arg schema "$PG_MANUAL_SCHEMA" \
	--arg uid "$MANUAL_APPLY_JOB_UID" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique == [$uid]
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "manual-drift Apply history does not contain exactly its original Job UID"
snapshot_watch pods
jq -s -e \
	--arg schema "$PG_MANUAL_SCHEMA" \
	--arg uid "$MANUAL_APPLY_POD_UID" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique == [$uid]
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "manual-drift Apply history does not contain exactly its original Pod UID"
[ "$(watch_new_uid_count "$PG_MANUAL_SCHEMA" observe "$MANUAL_OBSERVE_CHECKPOINT")" -eq 1 ] ||
	fail "manual drift did not retain exactly one fresh Observe Job"
[ "$(watch_new_uid_count "$PG_MANUAL_SCHEMA" plan "$MANUAL_PLAN_CHECKPOINT")" -eq 1 ] ||
	fail "manual drift did not retain exactly one fresh Plan Job"
if [ "$(watch_new_uid_count "$PG_RESTART_SCHEMA" observe "$PG_RESTART_OBSERVE_CHECKPOINT")" -ne 1 ] ||
	[ "$(watch_new_uid_count "$PG_RESTART_SCHEMA" plan "$PG_RESTART_PLAN_CHECKPOINT")" -ne 1 ]; then
	fail "restarted PostgreSQL convergence did not retain exactly one proof Observe and Plan Job"
fi
if [ "$(watch_new_uid_count "$PG_PARALLEL_SCHEMA" observe "$PG_PARALLEL_OBSERVE_CHECKPOINT")" -ne 1 ] ||
	[ "$(watch_new_uid_count "$PG_PARALLEL_SCHEMA" plan "$PG_PARALLEL_PLAN_CHECKPOINT")" -ne 1 ]; then
	fail "parallel PostgreSQL convergence did not retain exactly one proof Observe and Plan Job"
fi
if [ "$(watch_new_uid_count "$PG_ALIAS_SCHEMA_A" observe "$ALIAS_A_OBSERVE_CHECKPOINT")" -ne 1 ] ||
	[ "$(watch_new_uid_count "$PG_ALIAS_SCHEMA_A" plan "$ALIAS_A_PLAN_CHECKPOINT")" -ne 1 ]; then
	fail "shared-alias holder did not retain exactly one proof Observe and Plan Job"
fi
if [ "$(watch_new_uid_count "$PG_ALIAS_SCHEMA_B" observe "$ALIAS_B_OBSERVE_CHECKPOINT")" -ne 1 ] ||
	[ "$(watch_new_uid_count "$PG_ALIAS_SCHEMA_B" plan "$ALIAS_B_PLAN_CHECKPOINT")" -ne 1 ]; then
	fail "shared-alias stale contender did not retain exactly one recovery Observe and Plan Job"
fi
if [ "$(watch_new_uid_count "$MYSQL_UNKNOWN_SCHEMA" observe "$MYSQL_OBSERVE_CHECKPOINT")" -ne 1 ] ||
	[ "$(watch_new_uid_count "$MYSQL_UNKNOWN_SCHEMA" plan "$MYSQL_PLAN_JOB_CHECKPOINT")" -ne 1 ]; then
	fail "uncertain Apply recovery did not retain exactly one fresh Observe and Plan Job"
fi
[ "$(watch_added_uid_count "$MYSQL_UNKNOWN_SCHEMA" apply)" -eq 1 ] ||
	fail "the uncertain MySQL Apply was replayed during a later reconciliation"
[ "$(watch_added_pod_uid_count "$MYSQL_UNKNOWN_SCHEMA" apply)" -eq 1 ] ||
	fail "the uncertain MySQL Apply Job created a later replacement Pod"
if [ "$(watch_added_uid_count "$MYSQL_TIMEOUT_SCHEMA" apply)" -ne 1 ] ||
	[ "$(watch_added_pod_uid_count "$MYSQL_TIMEOUT_SCHEMA" apply)" -ne 1 ]; then
	fail "Kubernetes-timeout Apply history contains a replacement or delayed replay"
fi
if [ "$(watch_new_uid_count "$MYSQL_TIMEOUT_SCHEMA" observe "$MYSQL_TIMEOUT_OBSERVE_CHECKPOINT")" -ne 1 ] ||
	[ "$(watch_new_uid_count "$MYSQL_TIMEOUT_SCHEMA" plan "$MYSQL_TIMEOUT_PLAN_CHECKPOINT")" -ne 1 ]; then
	fail "Kubernetes-timeout recovery did not retain exactly one fresh Observe and Plan Job"
fi
snapshot_watch jobs
jq -s -e \
	--arg schema "$MYSQL_TIMEOUT_SCHEMA" \
	--arg uid "$MYSQL_TIMEOUT_JOB_UID" \
	--argjson deadline "$FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS" '
    ([.[] |
       select(.type == "ADDED") |
       select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
       select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
       .object.metadata.uid] | unique) == [$uid] and
    any(.[];
      .object.metadata.uid == $uid and
      .object.spec.activeDeadlineSeconds == $deadline and
      .object.spec.template.spec.activeDeadlineSeconds == $deadline and
      (.object.status.conditions // [] | any(
        .type == "Failed" and .status == "True" and
        .reason == "DeadlineExceeded")))
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "Kubernetes-timeout Apply history lost its exact DeadlineExceeded Job UID"
snapshot_watch pods
jq -s -e \
	--arg schema "$MYSQL_TIMEOUT_SCHEMA" \
	--arg uid "$MYSQL_TIMEOUT_POD_UID" '
    ([.[] |
       select(.type == "ADDED") |
       select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
       select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
       .object.metadata.uid] | unique) == [$uid] and
    all(.[] | select(.object.metadata.uid == $uid);
      (.object.spec.nodeName // "") == "" and
      ([.object.status.initContainerStatuses // [],
        .object.status.containerStatuses // [],
        .object.status.ephemeralContainerStatuses // []] | add |
        all(.[]; .state.running == null and .state.terminated == null and
          (.restartCount // 0) == 0)))
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "Kubernetes-timeout Apply history lost its exact never-started original Pod UID"
MANUAL_FINGERPRINT_FINAL=$(postgres_schema_fingerprint "$PG_MANUAL_DB" | tr -d '[:space:]')
[ "$MANUAL_FINGERPRINT_FINAL" = "$MANUAL_FINGERPRINT_BEFORE" ] ||
	fail "manual drift or its delayed read-only proof changed the database schema"
assert_database_column postgresql "$PG_MANUAL_DB" enabled 0
assert_database_column postgresql "$PG_MANUAL_DB" fault_token 0
snapshot_watch jobs
jq -s -e \
	--arg schema "$MYSQL_UNKNOWN_SCHEMA" \
	--arg uid "$MYSQL_JOB_UID" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique == [$uid]
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "uncertain MySQL Apply history does not contain exactly the original Job UID"
snapshot_watch pods
jq -s -e \
	--arg schema "$MYSQL_UNKNOWN_SCHEMA" \
	--arg uid "$MYSQL_POD_UID" '
    [.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique == [$uid]
	' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "uncertain MySQL Apply history does not contain exactly the original Pod UID"
if [ -z "$PRINCIPAL_REFUSAL_SCHEMA" ] || [ -z "$PRINCIPAL_PLAN_UID" ] ||
	[ -z "$PRINCIPAL_PLAN_POD_UID" ]; then
	fail "credential-bearing principal refusal did not retain its exact result identities"
fi
snapshot_watch jobs
jq -s -e \
	--arg schema "$PRINCIPAL_REFUSAL_SCHEMA" \
	--arg planUID "$PRINCIPAL_PLAN_UID" '
    ([.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "plan") |
      .object.metadata.uid] | unique) == [$planUID] and
    ([.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique | length) == 0
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "credential-bearing principal refusal did not remain one Plan and zero Apply Jobs"
snapshot_watch pods
jq -s -e \
	--arg schema "$PRINCIPAL_REFUSAL_SCHEMA" \
	--arg planUID "$PRINCIPAL_PLAN_POD_UID" '
    ([.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "plan") |
      .object.metadata.uid] | unique) == [$planUID] and
    ([.[] |
      select(.type == "ADDED") |
      select(.object.metadata.labels["operator.ptah.dev/schema"] == $schema) |
      select(.object.metadata.labels["operator.ptah.dev/operation"] == "apply") |
      .object.metadata.uid] | unique | length) == 0
  ' "$WATCH_SNAPSHOT_FILE" >/dev/null ||
	fail "credential-bearing principal refusal did not remain one Plan and zero Apply Pods"
assert_initial_read_chain_watch_order "$PG_RESTART_SCHEMA"
assert_uncertain_apply_proof_history "$MYSQL_UNKNOWN_SCHEMA" "$MYSQL_OPERATION_ID" \
	"$MYSQL_JOB_UID" "$MYSQL_LEASE_UID" "$MYSQL_LEASE_HOLDER" "$MYSQL_LEASE_EPOCH" \
	"$MYSQL_RECOVERY_OBSERVE_UID" "$MYSQL_RECOVERY_PLAN_JOB_UID" "$MYSQL_FRESH_PLAN_UID" \
	"$MYSQL_POD_UID"
assert_uncertain_apply_proof_history "$MYSQL_TIMEOUT_SCHEMA" \
	"$MYSQL_TIMEOUT_OPERATION_ID" "$MYSQL_TIMEOUT_JOB_UID" "$MYSQL_TIMEOUT_LEASE_UID" \
	"$MYSQL_TIMEOUT_LEASE_HOLDER" "$MYSQL_TIMEOUT_LEASE_EPOCH" \
	"$MYSQL_TIMEOUT_RECOVERY_OBSERVE_UID" "$MYSQL_TIMEOUT_RECOVERY_PLAN_UID" \
	"$MYSQL_TIMEOUT_FRESH_PLAN_UID" "$MYSQL_TIMEOUT_POD_UID" same-plan "" 0
assert_post_apply_proof_history "$PG_RESTART_SCHEMA" "$PG_OPERATION_ID" "$PG_JOB_UID" \
	"$PG_LEASE_UID" "$PG_LEASE_HOLDER" "$PG_LEASE_EPOCH" \
	"$PG_RESTART_PROOF_OBSERVE_JOB_UID" "$PG_RESTART_PROOF_PLAN_JOB_UID"
assert_post_apply_proof_history "$PG_PARALLEL_SCHEMA" "$PG_PARALLEL_OPERATION_ID" \
	"$PG_PARALLEL_JOB_UID" "$PG_PARALLEL_LEASE_UID" \
	"$PG_PARALLEL_LEASE_HOLDER" "$PG_PARALLEL_LEASE_EPOCH" \
	"$PG_PARALLEL_PROOF_OBSERVE_JOB_UID" "$PG_PARALLEL_PROOF_PLAN_JOB_UID"
assert_no_overlapping_operation_pods
assert_no_overlapping_operation_jobs
assert_fault_audit_complete
record_fault_jobs_for_parent

printf '%s\n' 'e2e faults: PASS watches, Kubernetes deadline recovery, stale-plan preflight, native lock barriers, restart identity, uncertain recovery, deletion, Pod serialization, credential audit, and coordination realms'
