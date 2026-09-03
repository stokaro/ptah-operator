#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

KUBECONFIG_FILE=${E2E_KUBECONFIG:-}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:-}
HELM_RELEASE=${E2E_HELM_RELEASE:-}
CHART_PACKAGE=${E2E_CHART_PACKAGE:-}
PTAH_VERSION=${E2E_PTAH_VERSION:-}
CONTROLLER_IMAGE=${E2E_CONTROLLER_IMAGE:-}
CONTROLLER_REVISION=${E2E_CONTROLLER_REVISION:-}
CONTROLLER_STATE_VERSION=${E2E_CONTROLLER_STATE_VERSION:-}
EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
RUNNER_IMAGE=${E2E_RUNNER_IMAGE:-}
FIXTURE_IMAGE=${E2E_FIXTURE_IMAGE:-}
POSTGRES_IMAGE=${E2E_POSTGRES_IMAGE:-}
MYSQL_IMAGE=${E2E_MYSQL_IMAGE:-}
REGISTRY_IP=${E2E_REGISTRY_IP:-}
REGISTRY_SERVICE=${E2E_REGISTRY_SERVICE:-registry}
REGISTRY_PORT=${E2E_REGISTRY_PORT:-}
REGISTRY_CREDENTIALS_FILE=${E2E_REGISTRY_CREDENTIALS_FILE:-}
DOCKER_CONTEXT=${E2E_DOCKER_CONTEXT:-}
REGISTRY_CONTAINER_ID=${E2E_REGISTRY_CONTAINER_ID:-}
EXTERNAL_PG_CONTAINER_ID=${E2E_EXTERNAL_POSTGRES_CONTAINER_ID:-}
EXTERNAL_PG_IP=${E2E_EXTERNAL_POSTGRES_IP:-}
EXTERNAL_PG_SERVICE=${E2E_EXTERNAL_POSTGRES_SERVICE:-}
EXTERNAL_PG_IMAGE=${E2E_EXTERNAL_POSTGRES_IMAGE:-}
EXTERNAL_PG_OWNER=${E2E_EXTERNAL_POSTGRES_OWNER:-}
EXTERNAL_PG_CREDENTIALS_FILE=${E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE:-}
TLS_PROXY_SERVICE=${E2E_TLS_PROXY_SERVICE:-}
TLS_PROXY_CA_FILE=${E2E_TLS_PROXY_CA_FILE:-}
TLS_PROXY_CERT_FILE=${E2E_TLS_PROXY_CERT_FILE:-}
TLS_PROXY_KEY_FILE=${E2E_TLS_PROXY_KEY_FILE:-}
RECONCILE_INTERVAL=${E2E_RECONCILE_INTERVAL:-1m}
TAG_MOVE_INTERVAL=${E2E_TAG_MOVE_INTERVAL:-2m}
APPROVAL_INTERVAL=${E2E_APPROVAL_INTERVAL:-5m}
STALE_APPROVAL_INTERVAL=${E2E_STALE_APPROVAL_INTERVAL:-4m}
QUIESCENT_INTERVAL=${E2E_QUIESCENT_INTERVAL:-30m}
BLOCKED_REFRESH_SECONDS=${E2E_BLOCKED_REFRESH_SECONDS:-30}
BLOCKED_REFRESH_INTERVAL=${BLOCKED_REFRESH_SECONDS}s
TIMEOUT_SECONDS=${E2E_TIMEOUT_SECONDS:-600}
TLS_PROXY_ENDPOINT_WAIT_ATTEMPTS=60
ADMISSION_RUNTIME_CLASS=ptah-e2e-runtime
ADMISSION_RUNTIME_TAINT=operator.ptah.dev/e2e-runtime
DIGEST_PIN_POLICY_NAME=e2e-digest-pin-verification-policy

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

require_mode_0600_regular_file() {
	mode_file=$1
	mode_description=$2
	if [ ! -f "$mode_file" ] || [ -L "$mode_file" ]; then
		fail "$mode_description must name a regular non-symlink file"
	fi
	if mode_value=$(stat -c '%a' "$mode_file" 2>/dev/null); then
		:
	else
		mode_value=$(stat -f '%Lp' "$mode_file" 2>/dev/null) ||
			fail "could not inspect $mode_description permissions"
	fi
	[ "$mode_value" = 600 ] || fail "$mode_description must have mode 0600"
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

for command_name in docker kubectl helm jq awk sed grep tr cksum mktemp date sleep tail stat wc cmp cut go env curl; do
	require_command "$command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "sha256sum or shasum is required"
fi
for value_name in \
	KUBECONFIG_FILE OPERATOR_NAMESPACE TEST_NAMESPACE HELM_RELEASE EXECUTOR_IMAGE \
	RUNNER_IMAGE CONTROLLER_IMAGE CONTROLLER_REVISION CONTROLLER_STATE_VERSION \
	FIXTURE_IMAGE POSTGRES_IMAGE MYSQL_IMAGE REGISTRY_IP REGISTRY_CREDENTIALS_FILE \
	REGISTRY_PORT CHART_PACKAGE PTAH_VERSION DOCKER_CONTEXT REGISTRY_CONTAINER_ID \
	EXTERNAL_PG_CONTAINER_ID EXTERNAL_PG_IP EXTERNAL_PG_SERVICE EXTERNAL_PG_IMAGE \
	EXTERNAL_PG_OWNER EXTERNAL_PG_CREDENTIALS_FILE \
	TLS_PROXY_SERVICE TLS_PROXY_CA_FILE TLS_PROXY_CERT_FILE TLS_PROXY_KEY_FILE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
is_pinned_image "$CONTROLLER_IMAGE" ||
	fail "E2E_CONTROLLER_IMAGE must be pinned by a lowercase SHA-256 digest"
[ "$(printf '%s' "$CONTROLLER_REVISION" | wc -c | tr -d '[:space:]')" -le 128 ] ||
	fail "E2E_CONTROLLER_REVISION must be at most 128 bytes"
[ "$CONTROLLER_REVISION" = "$(printf '%s' "$CONTROLLER_REVISION" | tr -d '[:cntrl:]')" ] ||
	fail "E2E_CONTROLLER_REVISION must not contain control characters"
printf '%s' "$CONTROLLER_REVISION" | grep -Eq '^[^[:space:]](.*[^[:space:]])?$' ||
	fail "E2E_CONTROLLER_REVISION must not be empty or have edge whitespace"
printf '%s\n' "$CONTROLLER_STATE_VERSION" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_CONTROLLER_STATE_VERSION must be a positive integer"
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
require_mode_0600_regular_file "$REGISTRY_CREDENTIALS_FILE" E2E_REGISTRY_CREDENTIALS_FILE
require_mode_0600_regular_file "$EXTERNAL_PG_CREDENTIALS_FILE" \
	E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE
case "$DOCKER_CONTEXT" in
default | orbstack | '') fail "E2E_DOCKER_CONTEXT must name an explicit nonlocal Docker context" ;;
esac
docker --context "$DOCKER_CONTEXT" context inspect "$DOCKER_CONTEXT" >/dev/null ||
	fail "E2E_DOCKER_CONTEXT cannot be inspected"
docker_endpoint=$(docker --context "$DOCKER_CONTEXT" context inspect \
	--format '{{ (index .Endpoints "docker").Host }}' "$DOCKER_CONTEXT")
case "$docker_endpoint" in
ssh://*) ;;
*) fail "E2E_DOCKER_CONTEXT must use an SSH endpoint" ;;
esac
[ -f "$CHART_PACKAGE" ] || fail "E2E_CHART_PACKAGE does not name a chart package"
ptah_version_length=$(printf '%s' "$PTAH_VERSION" | wc -c | tr -d '[:space:]')
if [ "$ptah_version_length" -lt 1 ] || [ "$ptah_version_length" -gt 128 ]; then
	fail "E2E_PTAH_VERSION must contain between 1 and 128 bytes"
fi
printf '%s\n' "$PTAH_VERSION" | grep -Eq '^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$' ||
	fail "E2E_PTAH_VERSION must not contain control or edge-whitespace characters"
printf '%s\n' "$REGISTRY_CONTAINER_ID" "$EXTERNAL_PG_CONTAINER_ID" |
	awk 'length($0) == 64 && $0 ~ /^[0-9a-f]+$/ {valid++} END {exit valid == 2 ? 0 : 1}' ||
	fail "Docker fixture IDs must be exact 64-character lowercase IDs"
printf '%s\n' "$EXTERNAL_PG_IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' ||
	fail "E2E_EXTERNAL_POSTGRES_IP must be an IPv4 address on the kind Docker network"
printf '%s\n' "$EXTERNAL_PG_SERVICE" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' ||
	fail "E2E_EXTERNAL_POSTGRES_SERVICE must be a DNS label"
printf '%s\n' "$EXTERNAL_PG_OWNER" | grep -Eq '^[0-9A-Za-z._-]+$' ||
	fail "E2E_EXTERNAL_POSTGRES_OWNER contains unsupported characters"
is_pinned_image "$EXTERNAL_PG_IMAGE" ||
	fail "E2E_EXTERNAL_POSTGRES_IMAGE must be digest-pinned"
printf '%s\n' "$TLS_PROXY_SERVICE" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' ||
	fail "E2E_TLS_PROXY_SERVICE must be a DNS label"
for tls_proxy_file in "$TLS_PROXY_CA_FILE" "$TLS_PROXY_CERT_FILE" "$TLS_PROXY_KEY_FILE"; do
	require_mode_0600_regular_file "$tls_proxy_file" "TLS proxy input file"
done
if [ "$TLS_PROXY_CA_FILE" = "$TLS_PROXY_CERT_FILE" ] ||
	[ "$TLS_PROXY_CA_FILE" = "$TLS_PROXY_KEY_FILE" ] ||
	[ "$TLS_PROXY_CERT_FILE" = "$TLS_PROXY_KEY_FILE" ]; then
	fail "TLS proxy CA, certificate, and private key must be separate files"
fi
jq -e '
  type == "object" and
  (keys == ["password", "username"]) and
  (.username | type == "string" and length > 0 and test("^[A-Za-z0-9_.-]+$")) and
  (.password | type == "string" and length > 0 and contains("\\n") | not)
' "$REGISTRY_CREDENTIALS_FILE" >/dev/null ||
	fail "E2E_REGISTRY_CREDENTIALS_FILE has an invalid shape"
REGISTRY_USERNAME=$(jq -er '.username' "$REGISTRY_CREDENTIALS_FILE")
REGISTRY_PASSWORD=$(jq -er '.password' "$REGISTRY_CREDENTIALS_FILE")
jq -e \
	--arg authority "${EXTERNAL_PG_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5432" '
  type == "object" and
  (keys == ["database", "password", "url", "username"]) and
  (.username | type == "string" and test("^[A-Za-z0-9_.-]+$") and length > 0) and
  (.password | type == "string" and test("^[A-Za-z0-9_.-]+$") and length > 0) and
  (.database | type == "string" and test("^[A-Za-z0-9_.-]+$") and length > 0) and
  .url == ("postgres://" + .username + ":" + .password + "@" + $authority + "/" +
    .database + "?sslmode=disable")
' "$EXTERNAL_PG_CREDENTIALS_FILE" >/dev/null ||
	fail "E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE has an invalid or misbound shape"
for image in "$EXECUTOR_IMAGE" "$RUNNER_IMAGE" "$FIXTURE_IMAGE" "$POSTGRES_IMAGE" "$MYSQL_IMAGE"; do
	is_pinned_image "$image" || fail "data-plane images must be pinned by a lowercase SHA-256 digest: $image"
done
printf '%s\n' "$REGISTRY_IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' ||
	fail "E2E_REGISTRY_IP must be an IPv4 address on the kind Docker network"
printf '%s\n' "$REGISTRY_PORT" | grep -Eq '^[0-9]+$' ||
	fail "E2E_REGISTRY_PORT must be numeric"
if [ "$REGISTRY_PORT" -lt 1024 ] || [ "$REGISTRY_PORT" -gt 65535 ]; then
	fail "E2E_REGISTRY_PORT must be between 1024 and 65535"
fi
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
deployed_controller_image=$(k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" -o json |
	jq -er '
      [.spec.template.spec.containers[] | select(.name == "manager").args[]? |
        select(startswith("--controller-image=")) | ltrimstr("--controller-image=")] as $images |
      if ($images | length) == 1 then $images[0]
      else error("manager must have exactly one --controller-image argument") end
    ')
[ "$deployed_controller_image" = "$CONTROLLER_IMAGE" ] ||
	fail "manager controller image argument does not match E2E_CONTROLLER_IMAGE"
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-data-e2e.XXXXXX")
chmod 700 "$WORK_DIR"
umask 077
RESOURCE_FILE=$WORK_DIR/resource.json
SECRET_FILE=$WORK_DIR/database-secrets.json
LOG_FILE=$WORK_DIR/logs.txt
# The broad ledger also records targeted result evidence. Only the full ledger
# certifies every exact owned Pod and started-container log for TTL/GC skips.
AUDITED_JOBS_FILE=$WORK_DIR/audited-jobs.txt
FULLY_AUDITED_JOBS_FILE=$WORK_DIR/fully-audited-jobs.txt
OBSERVED_JOBS_FILE=$WORK_DIR/observed-jobs.jsonl
CREDENTIAL_PATTERNS_FILE=$WORK_DIR/credential-patterns.txt
PG_PASSWORD_FILE=$WORK_DIR/postgresql.password
PG_URL_FILE=$WORK_DIR/postgresql.url
CUSTOM_CA_PG_URL_FILE=$WORK_DIR/custom-ca-postgresql.url
MYSQL_PASSWORD_FILE=$WORK_DIR/mysql.password
MYSQL_ROOT_PASSWORD_FILE=$WORK_DIR/mysql-root.password
MYSQL_URL_FILE=$WORK_DIR/mysql.url
REGISTRY_PASSWORD_FILE=$WORK_DIR/registry.password
RESULT_ASSERT_BINARY=$WORK_DIR/e2e-resultassert
RESULT_LOG_FILE=$WORK_DIR/runner-result.log
REGISTRY_OUTAGE_EVIDENCE_BEFORE=$WORK_DIR/registry-outage-evidence-before.json
REGISTRY_OUTAGE_EVIDENCE_AFTER=$WORK_DIR/registry-outage-evidence-after.json
REGISTRY_OUTAGE_SCHEMA_INPUT=$WORK_DIR/registry-outage-schema-input.json
REGISTRY_OUTAGE_PLAN_INPUT=$WORK_DIR/registry-outage-plan-input.json
ADMISSION_ERROR_FILE=$WORK_DIR/admission-error.txt
CLEANUP_SCHEMA_FILE=$WORK_DIR/cleanup-schemas.json
CLEANUP_EVENT_FILE=$WORK_DIR/cleanup-events.json
CLEANUP_JOB_FILE=$WORK_DIR/cleanup-jobs.json
CLEANUP_LEASE_FILE=$WORK_DIR/cleanup-leases.json
CLEANUP_DIAGNOSTIC_FILE=$WORK_DIR/cleanup-diagnostic.json
BLOCKED_REFRESH_DIAGNOSTIC_FILE=$WORK_DIR/blocked-refresh-diagnostic.json
TLS_PROXY_RESOURCE_FILE=$WORK_DIR/tls-proxy-resource.json
MYSQL_DESTRUCTIVE_SCHEMA=
MYSQL_DESTRUCTIVE_PLAN=
MYSQL_DESTRUCTIVE_PLAN_UID=
MYSQL_DESTRUCTIVE_APPLY_CHECKPOINT=
MYSQL_DESTRUCTIVE_DIGEST=
PERIODIC_NOOP_CHECKPOINT=
: >"$AUDITED_JOBS_FILE"
: >"$FULLY_AUDITED_JOBS_FILE"
: >"$OBSERVED_JOBS_FILE"
RBAC_PAUSED=0
RBAC_RULE_INDEX=
RBAC_ORIGINAL_VERBS=
RBAC_STATUS_API_GROUPS=
RBAC_STATUS_RESOURCES=
EPHEMERAL_SUBRESOURCE_TESTED=0
TLS_PROXY_POD_NAME=
TLS_PROXY_POD_UID=
TLS_PROXY_POD_IP=
TLS_PROXY_CONTAINER_ID=

mkdir -p "$WORK_DIR/go-cache"
env GOCACHE="$WORK_DIR/go-cache" go build -trimpath \
	-o "$RESULT_ASSERT_BINARY" ./test/e2e/resultassert

suppress_cleanup_diagnostics() {
	printf '%s\n' 'e2e data plane: credential-safe reconciliation diagnostics suppressed' >&2
	return 0
}

emit_scanned_cleanup_diagnostic() {
	emit_diagnostic_file=$1
	emit_patterns_file=$2
	if [ ! -f "$emit_patterns_file" ] || [ ! -s "$emit_patterns_file" ]; then
		suppress_cleanup_diagnostics
		return 0
	fi
	if grep -q '^$' "$emit_patterns_file" >/dev/null 2>&1; then
		emit_pattern_status=0
	else
		emit_pattern_status=$?
	fi
	case "$emit_pattern_status" in
	0)
		suppress_cleanup_diagnostics
		return 0
		;;
	1) ;;
	*)
		suppress_cleanup_diagnostics
		return 0
		;;
	esac
	if grep -F -f "$emit_patterns_file" "$emit_diagnostic_file" >/dev/null 2>&1; then
		emit_scan_status=0
	else
		emit_scan_status=$?
	fi
	case "$emit_scan_status" in
	0)
		suppress_cleanup_diagnostics
		return 0
		;;
	1) ;;
	*)
		suppress_cleanup_diagnostics
		return 0
		;;
	esac
	if [ ! -s "$emit_diagnostic_file" ]; then
		suppress_cleanup_diagnostics
		return 0
	fi
	if ! emit_diagnostic=$(jq -c . "$emit_diagnostic_file" 2>/dev/null); then
		suppress_cleanup_diagnostics
		return 0
	fi
	printf '%s\n' 'e2e data plane: credential-safe reconciliation diagnostic projection' >&2
	printf '%s\n' "$emit_diagnostic" >&2
	return 0
}

project_cleanup_diagnostic_files() {
	project_schema_file=$1
	project_event_file=$2
	project_job_file=$3
	project_lease_file=$4
	jq -n \
		--slurpfile schemas "$project_schema_file" \
		--slurpfile events "$project_event_file" \
		--slurpfile jobs "$project_job_file" \
		--slurpfile leases "$project_lease_file" '
          # cleanup-diagnostic-projection-begin
          def safe_code:
            if type == "string" then
              (try capture("^(?<code>[a-z][a-z0-9_]{0,63}):").code catch null) as $code |
              if ($code | type) == "string" and
                  ($code | test("^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$"))
              then $code else null end
            else null end;
          def safe_epoch:
            if type == "string" and test("^v1-[0-9a-f]{32}$") then . else null end;
          def safe_digest:
            if type == "string" and test("^sha256:[0-9a-f]{64}$") then . else null end;
          def safe_phase:
            if . == "Pending" or . == "Resolving" or . == "Verifying" or
                . == "Observing" or . == "Planning" or . == "ReadyToApply" or
                . == "AwaitingApproval" or
                . == "Applying" or . == "VerifyingConvergence" or . == "InSync" or
                . == "Blocked" or . == "Suspended" or . == "Failed"
            then . else null end;
          def safe_operation:
            if . == "resolve" or . == "verify" or . == "observe" or
                . == "plan" or . == "apply" then . else null end;
          {
            schemas: [
              ($schemas[0].items // [])[] |
              select((.metadata.uid // "") | type == "string" and length > 0) |
              . as $schema |
              {
                name: $schema.metadata.name,
                uid: $schema.metadata.uid,
                generation: $schema.metadata.generation,
                observedGeneration: $schema.status.observedGeneration,
                phase: ($schema.status.phase | safe_phase),
                nextReconciliationTime: $schema.status.nextReconciliationTime,
                failure: ([
                  ($schema.status.conditions // [])[] |
                  select(.type == "ReconciliationFailed" and .status == "True") |
                  select(.reason == "OperationFailed" or .reason == "ConfigurationError" or
                    .reason == "ApplyOutcomeUnknown") |
                  {
                    condition: "ReconciliationFailed",
                    reason,
                    code: (.message | safe_code)
                  }
                ] | .[-1] // null),
                expectedEpochs: {
                  activeOperation: ($schema.status.activeOperation.leaseEpoch | safe_epoch),
                  pendingObservation: ($schema.status.pendingObservation.leaseEpoch | safe_epoch),
                  pendingLockRelease: ($schema.status.pendingLockRelease.leaseEpoch | safe_epoch)
                },
                leaseContinuityLost: ($schema.status.activeOperation.leaseContinuityLost // false),
                events: [
                  ($events[0].items // [])[] |
                  select(.involvedObject.apiVersion == "operator.ptah.dev/v1alpha1" and
                    .involvedObject.kind == "PtahSchema" and
                    .involvedObject.name == $schema.metadata.name and
                    .involvedObject.uid == $schema.metadata.uid) |
                  select(.reason == "OperationFailed" or .reason == "ReconciliationFailed" or
                    .reason == "ApplyOutcomeUnknown" or .reason == "PlanStale" or
                    .reason == "LeaseContinuityLost") |
                  {
                    type: (if .type == "Normal" or .type == "Warning" then .type else null end),
                    reason,
                    code: (if .reason == "OperationFailed" or .reason == "ReconciliationFailed" or
                      .reason == "ApplyOutcomeUnknown"
                      then (.message | safe_code) else null end),
                    timestamp: (.eventTime // .lastTimestamp // .firstTimestamp // null)
                  }
                ],
                jobs: [
                  ($jobs[0].items // [])[] |
                  select(.metadata.ownerReferences // [] | any(
                    .apiVersion == "operator.ptah.dev/v1alpha1" and
                    .kind == "PtahSchema" and .uid == $schema.metadata.uid and
                    .controller == true)) |
                  {
                    name: .metadata.name,
                    uid: .metadata.uid,
                    operation: (.metadata.labels["operator.ptah.dev/operation"] | safe_operation),
                    created: .metadata.creationTimestamp,
                    started: .status.startTime,
                    completed: .status.completionTime,
                    complete: ((.status.conditions // []) | any(
                      .type == "Complete" and .status == "True")),
                    failed: ((.status.conditions // []) | any(
                      .type == "Failed" and .status == "True")),
                    operationIDHashShape: ((.metadata.annotations["operator.ptah.dev/operation-id"] // "") |
                      type == "string" and test("^sha256:[0-9a-f]{64}$")),
                    inputFingerprint: (.metadata.annotations["operator.ptah.dev/input-fingerprint"] |
                      safe_digest)
                  }
                ]
              }
            ],
            leases: [
              ($leases[0].items // [])[] |
              select(.metadata.labels["app.kubernetes.io/managed-by"] == "ptah-operator" and
                .metadata.labels["operator.ptah.dev/coordination"] == "database-target") |
              {
                name: .metadata.name,
                uid: .metadata.uid,
                resourceVersion: .metadata.resourceVersion,
                created: .metadata.creationTimestamp,
                epoch: (.metadata.annotations["operator.ptah.dev/lease-epoch"] | safe_epoch),
                holderPresent: ((.spec.holderIdentity // "") | type == "string" and length > 0),
                holderHashShape: ((.spec.holderIdentity // "") |
                  type == "string" and test("^ptah-h-[a-z2-7]{52}$")),
                leaseDurationSeconds: .spec.leaseDurationSeconds,
                acquireTime: .spec.acquireTime,
                renewTime: .spec.renewTime,
                transitions: .spec.leaseTransitions
              }
            ]
          }
          # cleanup-diagnostic-projection-end
        '
}

collect_credential_safe_diagnostics() {
	if [ ! -f "$CREDENTIAL_PATTERNS_FILE" ] || [ ! -s "$CREDENTIAL_PATTERNS_FILE" ]; then
		suppress_cleanup_diagnostics
		return 0
	fi
	: >"$CLEANUP_SCHEMA_FILE"
	: >"$CLEANUP_EVENT_FILE"
	: >"$CLEANUP_JOB_FILE"
	: >"$CLEANUP_LEASE_FILE"
	: >"$CLEANUP_DIAGNOSTIC_FILE"
	chmod 600 "$CLEANUP_SCHEMA_FILE" "$CLEANUP_EVENT_FILE" "$CLEANUP_JOB_FILE" \
		"$CLEANUP_LEASE_FILE" "$CLEANUP_DIAGNOSTIC_FILE" || {
		suppress_cleanup_diagnostics
		return 0
	}
	if ! k -n "$TEST_NAMESPACE" get ptahschemas -o json >"$CLEANUP_SCHEMA_FILE" 2>/dev/null ||
		! k -n "$TEST_NAMESPACE" get events -o json >"$CLEANUP_EVENT_FILE" 2>/dev/null ||
		! k -n "$TEST_NAMESPACE" get jobs -o json >"$CLEANUP_JOB_FILE" 2>/dev/null ||
		! k -n "$OPERATOR_NAMESPACE" get leases \
			-l 'app.kubernetes.io/managed-by=ptah-operator,operator.ptah.dev/coordination=database-target' \
			-o json >"$CLEANUP_LEASE_FILE" 2>/dev/null; then
		suppress_cleanup_diagnostics
		return 0
	fi
	if ! project_cleanup_diagnostic_files "$CLEANUP_SCHEMA_FILE" "$CLEANUP_EVENT_FILE" \
		"$CLEANUP_JOB_FILE" "$CLEANUP_LEASE_FILE" >"$CLEANUP_DIAGNOSTIC_FILE" 2>/dev/null; then
		suppress_cleanup_diagnostics
		return 0
	fi
	emit_scanned_cleanup_diagnostic "$CLEANUP_DIAGNOSTIC_FILE" \
		"$CREDENTIAL_PATTERNS_FILE"
	return 0
}

collect_diagnostics() {
	printf '%s\n' 'e2e data plane: collecting failure diagnostics' >&2
	collect_credential_safe_diagnostics
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
		--arg podUID "$active_pod_uid" \
		--arg controllerImage "$CONTROLLER_IMAGE" \
		--arg controllerRevision "$CONTROLLER_REVISION" \
		--arg controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .metadata.uid == $jobUID and
      .status.active >= 1 and
      .metadata.annotations["operator.ptah.dev/controller-image"] == $controllerImage and
      .metadata.annotations["operator.ptah.dev/controller-revision"] == $controllerRevision and
      .metadata.annotations["operator.ptah.dev/controller-state-version"] == $controllerStateVersion and
      .spec.template.metadata.annotations["operator.ptah.dev/controller-image"] == $controllerImage and
      .spec.template.metadata.annotations["operator.ptah.dev/controller-revision"] == $controllerRevision and
      .spec.template.metadata.annotations["operator.ptah.dev/controller-state-version"] == $controllerStateVersion and
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
	scan_file_for_credentials "$ADMISSION_ERROR_FILE" \
		"the ephemeral-container admission refusal"
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

	# The new object no longer matches the objectSelector. This PATCH can reach
	# the handler only because admission selection also evaluates oldObject.
	if k -n "$TEST_NAMESPACE" label pod "$active_pod_name" \
		'app.kubernetes.io/managed-by-' >"$ADMISSION_ERROR_FILE" 2>&1; then
		fail "Pod intent admission allowed exact active Pod $active_pod_name UID $active_pod_uid to remove its selector identity"
	fi
	scan_file_for_credentials "$ADMISSION_ERROR_FILE" \
		"the managed-identity label-removal admission refusal"
	grep -F 'vpodintent.operator.ptah.dev' "$ADMISSION_ERROR_FILE" >/dev/null ||
		fail "managed-identity label removal did not reach the Pod intent webhook through oldObject"
	grep -F 'removed its managed workload identity' "$ADMISSION_ERROR_FILE" >/dev/null ||
		fail "Pod intent webhook rejected managed-identity label removal for an unexpected reason"
	k -n "$TEST_NAMESPACE" get pod "$active_pod_name" -o json | jq -e \
		--arg podUID "$active_pod_uid" \
		--arg jobUID "$active_job_uid" '
      .metadata.uid == $podUID and
      .metadata.labels["app.kubernetes.io/managed-by"] == "ptah-operator" and
      .metadata.labels["app.kubernetes.io/component"] == "schema-operation" and
      any(.metadata.ownerReferences[]?;
        .apiVersion == "batch/v1" and .kind == "Job" and
        .uid == $jobUID and .controller == true)
	' >/dev/null || fail "active Pod identity changed during the oldObject selector test"
	printf '%s\n' 'e2e data plane: PASS active Pod oldObject selector enforcement'
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
		if grep -Fx "$audit_uid" "$FULLY_AUDITED_JOBS_FILE" >/dev/null 2>&1; then
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
				--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" \
				--arg controllerImage "$CONTROLLER_IMAGE" \
				--arg controllerRevision "$CONTROLLER_REVISION" \
				--arg controllerStateVersion "$CONTROLLER_STATE_VERSION" '
              (.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] // "") as $digest |
              ($digest | test("^sha256:[0-9a-f]{64}$")) and
              .spec.template.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] == $digest and
              .metadata.annotations["operator.ptah.dev/controller-image"] == $controllerImage and
              .metadata.annotations["operator.ptah.dev/controller-revision"] == $controllerRevision and
              .metadata.annotations["operator.ptah.dev/controller-state-version"] == $controllerStateVersion and
              .spec.template.metadata.annotations["operator.ptah.dev/controller-image"] == $controllerImage and
              .spec.template.metadata.annotations["operator.ptah.dev/controller-revision"] == $controllerRevision and
              .spec.template.metadata.annotations["operator.ptah.dev/controller-state-version"] == $controllerStateVersion and
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
					--arg pullSecret "$REGISTRY_PULL_SECRET" \
					--arg controllerImage "$CONTROLLER_IMAGE" \
					--arg controllerRevision "$CONTROLLER_REVISION" \
					--arg controllerStateVersion "$CONTROLLER_STATE_VERSION" '
                  (.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] // "") as $digest |
                  ($digest | test("^sha256:[0-9a-f]{64}$")) and
                  .metadata.annotations["operator.ptah.dev/controller-image"] == $controllerImage and
                  .metadata.annotations["operator.ptah.dev/controller-revision"] == $controllerRevision and
                  .metadata.annotations["operator.ptah.dev/controller-state-version"] == $controllerStateVersion and
                  .spec.runtimeClassName == $runtimeClass and
                  .spec.serviceAccountName == "default" and
                  .spec.automountServiceAccountToken == false and
                  .spec.nodeSelector["kubernetes.io/os"] == "linux" and
                  .spec.overhead.memory == "8Mi" and
                  .spec.imagePullSecrets == [{name: $pullSecret}] and
                  all((.spec.volumes // [])[];
                    all((.projected.sources // [])[]?;
                      .serviceAccountToken == null)) and
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
		if ! grep -Fx "$audit_uid" "$AUDITED_JOBS_FILE" >/dev/null 2>&1; then
			printf '%s\n' "$audit_uid" >>"$AUDITED_JOBS_FILE"
		fi
		if ! grep -Fx "$audit_uid" "$FULLY_AUDITED_JOBS_FILE" >/dev/null 2>&1; then
			printf '%s\n' "$audit_uid" >>"$FULLY_AUDITED_JOBS_FILE"
		fi
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
	audit_pods_snapshot=$(k -n "$TEST_NAMESPACE" get pods -o json)
	if ! audit_pod_records=$(printf '%s\n' "$audit_pods_snapshot" | jq -r '
	    .items[] |
	    ([.metadata.ownerReferences[]? |
	      select(.apiVersion == "batch/v1" and .kind == "Job" and .controller == true) |
	      .uid]) as $controllerJobUIDs |
	    ($controllerJobUIDs |
	      if length == 0 then "-"
	      elif length == 1 then .[0]
	      else error("Pod has multiple controlling batch/v1 Job owners") end) as $controllerJobUID |
	    [.metadata.name, .metadata.uid, (.status.phase // "Unknown"), $controllerJobUID] | @tsv
	  '); then
		fail "could not capture exact test Pod identities for credential audit"
	fi
	printf '%s\n' "$audit_pod_records" | while IFS="$(printf '\t')" read -r \
		audit_pod_name audit_pod_uid \
		audit_snapshot_phase audit_controller_job_uid; do
		[ -n "$audit_pod_name" ] || continue
		if [ "$audit_snapshot_phase" = Succeeded ] || [ "$audit_snapshot_phase" = Failed ]; then
			if [ "$audit_controller_job_uid" != "-" ]; then
				if grep -Fx "$audit_controller_job_uid" "$FULLY_AUDITED_JOBS_FILE" >/dev/null 2>&1; then
					continue
				fi
			fi
		fi
		audit_pod="pod/$audit_pod_name"
		if ! audit_pod_object=$(k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" \
			-o json 2>/dev/null); then
			fail "unaudited exact Pod $audit_pod_name UID $audit_pod_uid disappeared before log audit"
		fi
		printf '%s\n' "$audit_pod_object" | jq -e \
			--arg uid "$audit_pod_uid" '.metadata.uid == $uid' >/dev/null ||
			fail "unaudited exact Pod $audit_pod_name changed identity before its log audit"
		printf '%s\n' "$audit_pod_object" >"$RESOURCE_FILE"
		scan_file_for_credentials "$RESOURCE_FILE" \
			"unaudited exact Pod $audit_pod_name UID $audit_pod_uid"
		: >"$RESOURCE_FILE"
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
		if ! audit_pod_after=$(k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" \
			-o json 2>/dev/null); then
			fail "unaudited exact Pod $audit_pod_name UID $audit_pod_uid disappeared during log audit"
		fi
		printf '%s\n' "$audit_pod_after" | jq -e \
			--arg uid "$audit_pod_uid" '.metadata.uid == $uid' >/dev/null ||
			fail "unaudited exact Pod $audit_pod_name changed identity during its log audit"
	done
}

assert_observed_jobs_audited() {
	jq -er '.uid' "$OBSERVED_JOBS_FILE" | while IFS= read -r observed_uid; do
		[ -n "$observed_uid" ] || continue
		grep -Fx "$observed_uid" "$FULLY_AUDITED_JOBS_FILE" >/dev/null ||
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

schema_job_count_between_checkpoints() {
	bounded_schema=$1
	bounded_before=$2
	bounded_after=$3
	jq -s \
		--slurpfile before "$bounded_before" \
		--slurpfile after "$bounded_after" \
		--arg schema "$bounded_schema" '
      [.[] |
        select(.schema == $schema) |
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

assert_read_only_chain_between_checkpoints() {
	chain_schema=$1
	chain_before=$2
	chain_after=$3
	k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${chain_schema}" -o json |
		jq -e \
			--slurpfile before "$chain_before" \
			--slurpfile after "$chain_after" '
          def in_boundary:
            .metadata.uid as $uid |
            ($after[0] | index($uid)) != null and
            ($before[0] | index($uid)) == null;
          def bounded($operation):
            [.items[] |
              select(.metadata.labels["operator.ptah.dev/operation"] == $operation) |
              select(in_boundary)] |
            if length == 1 then .[0] else error("read-only operation is not exact") end;
          [.items[] | select(in_boundary)] as $all |
          bounded("resolve") as $resolve |
          bounded("verify") as $verify |
          bounded("observe") as $observe |
          bounded("plan") as $plan |
          [$resolve, $verify, $observe, $plan] as $jobs |
          ($all | length) == 4 and
          all($jobs[];
            (.status.conditions // [] | any(.type == "Complete" and .status == "True")) and
            (.status.conditions // [] | all(.type != "Failed" or .status != "True"))) and
          $resolve.status.completionTime <= $verify.status.startTime and
          $verify.status.completionTime <= $observe.status.startTime and
          $observe.status.completionTime <= $plan.status.startTime
        ' >/dev/null ||
		fail "$chain_schema did not preserve one exact sequential Resolve, Verify, Observe, Plan chain"
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
      .protocolVersion == 4 and .operation == $operation and
      .operationId == $operationID and .truncation == null
    ' "$result_output" >/dev/null ||
		fail "validated result lost its protocol-v4 binding or complete-output guarantee"
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

assert_registry_container_contract() {
	registry_expected_running=$1
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.Id}}' "$REGISTRY_CONTAINER_ID")" = "$REGISTRY_CONTAINER_ID" ] ||
		fail "registry container identity changed"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.State.Running}}' "$REGISTRY_CONTAINER_ID")" = "$registry_expected_running" ] ||
		fail "registry container running state is not $registry_expected_running"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{index .Config.Labels \"operator.ptah.dev/e2e-owner\"}}' \
		"$REGISTRY_CONTAINER_ID")" = "$EXTERNAL_PG_OWNER" ] ||
		fail "registry container lost its task owner label"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{index .Config.Labels \"operator.ptah.dev/e2e-component\"}}' \
		"$REGISTRY_CONTAINER_ID")" = registry ] ||
		fail "registry container lost its component label"
	if [ "$registry_expected_running" = true ]; then
		docker --context "$DOCKER_CONTEXT" container inspect \
			--format '{{json .NetworkSettings.Networks}}' "$REGISTRY_CONTAINER_ID" |
			jq -e --arg address "$REGISTRY_IP" '
          keys == ["kind"] and .kind.IPAddress == $address
        ' >/dev/null || fail "registry container left its exact kind-network address"
	fi
}

wait_for_registry_http_ready() {
	registry_ready_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$registry_ready_deadline" ]; do
		registry_ready_status=$(curl --silent --output /dev/null --write-out '%{http_code}' \
			--connect-timeout 2 --max-time 5 \
			"http://127.0.0.1:${REGISTRY_PORT}/v2/" 2>/dev/null || true)
		if [ "$registry_ready_status" = 401 ]; then
			return 0
		fi
		sleep 1
	done
	fail "authenticated registry HTTP API did not become ready after restart"
}

assert_external_pg_container_contract() {
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.Id}}' "$EXTERNAL_PG_CONTAINER_ID")" = "$EXTERNAL_PG_CONTAINER_ID" ] ||
		fail "external PostgreSQL container identity changed"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.State.Running}}' "$EXTERNAL_PG_CONTAINER_ID")" = true ] ||
		fail "external PostgreSQL container is not running"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.HostConfig.RestartPolicy.Name}}' "$EXTERNAL_PG_CONTAINER_ID")" = no ] ||
		fail "external PostgreSQL container gained a restart policy"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.HostConfig.PublishAllPorts}}' "$EXTERNAL_PG_CONTAINER_ID")" = false ] ||
		fail "external PostgreSQL container publishes all ports"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.Config.Image}}' "$EXTERNAL_PG_CONTAINER_ID")" = "$EXTERNAL_PG_IMAGE" ] ||
		fail "external PostgreSQL container lost its digest-pinned image"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{index .Config.Labels \"operator.ptah.dev/e2e-owner\"}}' \
		"$EXTERNAL_PG_CONTAINER_ID")" = "$EXTERNAL_PG_OWNER" ] ||
		fail "external PostgreSQL container lost its task owner label"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{index .Config.Labels \"operator.ptah.dev/e2e-component\"}}' \
		"$EXTERNAL_PG_CONTAINER_ID")" = external-postgresql ] ||
		fail "external PostgreSQL container lost its component label"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .HostConfig.PortBindings}}' "$EXTERNAL_PG_CONTAINER_ID" |
		jq -e '. == null or . == {}' >/dev/null ||
		fail "external PostgreSQL container has host port bindings"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .NetworkSettings.Ports}}' "$EXTERNAL_PG_CONTAINER_ID" |
		jq -e 'to_entries | all(.value == null)' >/dev/null ||
		fail "external PostgreSQL container exposes a host port"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .HostConfig.Tmpfs}}' "$EXTERNAL_PG_CONTAINER_ID" |
		jq -e 'keys == ["/var/lib/postgresql/data"]' >/dev/null ||
		fail "external PostgreSQL data directory is not an exact tmpfs"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .Mounts}}' "$EXTERNAL_PG_CONTAINER_ID" |
		jq -e '
      length == 1 and .[0].Type == "tmpfs" and
      .[0].Destination == "/var/lib/postgresql/data"
    ' >/dev/null || fail "external PostgreSQL container has a persistent or unexpected mount"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .NetworkSettings.Networks}}' "$EXTERNAL_PG_CONTAINER_ID" |
		jq -e --arg address "$EXTERNAL_PG_IP" '
      keys == ["kind"] and .kind.IPAddress == $address
    ' >/dev/null || fail "external PostgreSQL container left its exact kind-network address"
}

external_pg_query() {
	external_query=$1
	assert_external_pg_container_contract
	docker --context "$DOCKER_CONTEXT" exec "$EXTERNAL_PG_CONTAINER_ID" \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atqc "$1"' \
		sh "$external_query"
}

assert_external_pg_server_version() {
	external_version_num=$(external_pg_query 'SHOW server_version_num')
	external_version_num=$(printf '%s' "$external_version_num" | tr -d '[:space:]')
	printf '%s\n' "$external_version_num" | grep -Eq '^17[0-9]{4}$' ||
		fail "external PostgreSQL fixture is not major version 17"
}

assert_external_pg_not_hosted_in_kubernetes() {
	k -n "$TEST_NAMESPACE" get deployments,statefulsets,pods,jobs -o json |
		jq -e \
			--arg service "$EXTERNAL_PG_SERVICE" \
			--arg image "$EXTERNAL_PG_IMAGE" '
          [.items[] | select(
            .metadata.name == $service or
            .metadata.labels["app.kubernetes.io/component"] == "e2e-external-database" or
            any(.spec.template.spec.containers[]?; .image == $image) or
            any(.spec.containers[]?; .image == $image)
          )] | length == 0
        ' >/dev/null || fail "a Kubernetes workload hosts or impersonates external PostgreSQL"
}

create_external_postgresql_endpoint() {
	printf '%s\n' 'e2e data plane: creating selectorless external PostgreSQL endpoint'
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$EXTERNAL_PG_SECRET" \
		--arg owner "$EXTERNAL_PG_OWNER" \
		--slurpfile credentials "$EXTERNAL_PG_CREDENTIALS_FILE" '
      {
        apiVersion: "v1", kind: "Secret", immutable: true,
        metadata: {
          namespace: $namespace, name: $name,
          labels: {
            "app.kubernetes.io/component": "e2e-external-database",
            "operator.ptah.dev/e2e-owner": $owner
          }
        },
        type: "Opaque", stringData: {url: $credentials[0].url}
      }
    ' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k create -f "$SECRET_FILE" >/dev/null
	: >"$SECRET_FILE"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$EXTERNAL_PG_SERVICE" \
		--arg owner "$EXTERNAL_PG_OWNER" '
      {
        apiVersion: "v1", kind: "Service",
        metadata: {
          namespace: $namespace, name: $name,
          labels: {
            "app.kubernetes.io/name": $name,
            "app.kubernetes.io/component": "e2e-external-database",
            "operator.ptah.dev/e2e-owner": $owner
          }
        },
        spec: {
          ports: [{name: "postgresql", port: 5432, protocol: "TCP", targetPort: 5432}]
        }
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	external_service_uid=$(k -n "$TEST_NAMESPACE" get service "$EXTERNAL_PG_SERVICE" \
		-o jsonpath='{.metadata.uid}')
	[ -n "$external_service_uid" ] || fail "external PostgreSQL Service has no UID"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "${EXTERNAL_PG_SERVICE}-docker" \
		--arg service "$EXTERNAL_PG_SERVICE" \
		--arg serviceUID "$external_service_uid" \
		--arg owner "$EXTERNAL_PG_OWNER" \
		--arg address "$EXTERNAL_PG_IP" '
      {
        apiVersion: "discovery.k8s.io/v1", kind: "EndpointSlice",
        metadata: {
          namespace: $namespace, name: $name,
	          labels: {
	            "kubernetes.io/service-name": $service,
	            "endpointslice.kubernetes.io/managed-by": "ptah-operator-e2e",
	            "app.kubernetes.io/component": "e2e-external-database",
            "operator.ptah.dev/e2e-owner": $owner
          },
          ownerReferences: [{
            apiVersion: "v1", kind: "Service", name: $service, uid: $serviceUID,
            controller: true, blockOwnerDeletion: false
          }]
        },
        addressType: "IPv4",
        endpoints: [{addresses: [$address], conditions: {ready: true}}],
        ports: [{name: "postgresql", port: 5432, protocol: "TCP"}]
      }
    ' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null

	k -n "$TEST_NAMESPACE" get secret "$EXTERNAL_PG_SECRET" -o json |
		jq -e \
			--arg owner "$EXTERNAL_PG_OWNER" \
			--slurpfile credentials "$EXTERNAL_PG_CREDENTIALS_FILE" '
          .immutable == true and .type == "Opaque" and
          .metadata.labels["app.kubernetes.io/component"] == "e2e-external-database" and
          .metadata.labels["operator.ptah.dev/e2e-owner"] == $owner and
          (.data | keys) == ["url"] and
          .data.url == ($credentials[0].url | @base64)
        ' >/dev/null || fail "external PostgreSQL Secret lost its exact URL-only binding"
	k -n "$TEST_NAMESPACE" get service "$EXTERNAL_PG_SERVICE" -o json |
		jq -e \
			--arg owner "$EXTERNAL_PG_OWNER" '
          (.spec | has("selector") | not) and
          .metadata.labels["app.kubernetes.io/component"] == "e2e-external-database" and
          .metadata.labels["operator.ptah.dev/e2e-owner"] == $owner and
          .spec.ports == [{name: "postgresql", port: 5432, protocol: "TCP", targetPort: 5432}]
        ' >/dev/null || fail "external PostgreSQL Service is not an exact selectorless route"
	k -n "$TEST_NAMESPACE" get endpointslice "${EXTERNAL_PG_SERVICE}-docker" -o json |
		jq -e \
			--arg service "$EXTERNAL_PG_SERVICE" \
			--arg serviceUID "$external_service_uid" \
			--arg owner "$EXTERNAL_PG_OWNER" \
			--arg address "$EXTERNAL_PG_IP" '
	          .addressType == "IPv4" and
	          .metadata.labels["kubernetes.io/service-name"] == $service and
	          .metadata.labels["endpointslice.kubernetes.io/managed-by"] == "ptah-operator-e2e" and
	          .metadata.labels["app.kubernetes.io/component"] == "e2e-external-database" and
          .metadata.labels["operator.ptah.dev/e2e-owner"] == $owner and
          .metadata.ownerReferences == [{
            apiVersion: "v1", kind: "Service", name: $service, uid: $serviceUID,
            controller: true, blockOwnerDeletion: false
          }] and
          .endpoints == [{addresses: [$address], conditions: {ready: true}}] and
          .ports == [{name: "postgresql", port: 5432, protocol: "TCP"}]
        ' >/dev/null || fail "external PostgreSQL EndpointSlice lost its exact Docker route"
	assert_external_pg_not_hosted_in_kubernetes
	assert_external_pg_container_contract
	assert_external_pg_server_version
	external_initial_table_count=$(external_pg_query \
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='e2e_widgets'")
	external_initial_table_count=$(printf '%s' "$external_initial_table_count" | tr -d '[:space:]')
	[ "$external_initial_table_count" = 0 ] ||
		fail "external PostgreSQL fixture was not empty before reconciliation"
	external_role_superuser=$(external_pg_query \
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user")
	external_role_superuser=$(printf '%s' "$external_role_superuser" | tr -d '[:space:]')
	[ "$external_role_superuser" = f ] ||
		fail "external PostgreSQL fixture login is a superuser"
	external_database_owner=$(external_pg_query \
		"SELECT pg_get_userbyid(datdba) = current_user FROM pg_database WHERE datname = current_database()")
	external_database_owner=$(printf '%s' "$external_database_owner" | tr -d '[:space:]')
	[ "$external_database_owner" = t ] ||
		fail "external PostgreSQL fixture login does not retain database ownership"
}

create_authenticated_tls_proxy() {
	printf '%s\n' 'e2e data plane: creating authenticated HTTPS custom-CA registry proxy'
	if ! k -n "$TEST_NAMESPACE" create secret tls "$TLS_PROXY_CERT_SECRET" \
		--cert="$TLS_PROXY_CERT_FILE" --key="$TLS_PROXY_KEY_FILE" \
		--dry-run=client -o json >"$RESOURCE_FILE"; then
		fail "could not render the task-scoped TLS proxy certificate Secret"
	fi
	jq '.immutable = true' "$RESOURCE_FILE" >"$TLS_PROXY_RESOURCE_FILE"
	k create -f "$TLS_PROXY_RESOURCE_FILE" >/dev/null
	: >"$RESOURCE_FILE"
	: >"$TLS_PROXY_RESOURCE_FILE"

	if ! k -n "$TEST_NAMESPACE" create configmap "$TLS_PROXY_CA_CONFIGMAP" \
		--from-file="ca.pem=${TLS_PROXY_CA_FILE}" \
		--dry-run=client -o json >"$RESOURCE_FILE"; then
		fail "could not render the task-scoped TLS proxy CA ConfigMap"
	fi
	jq '.immutable = true' "$RESOURCE_FILE" >"$TLS_PROXY_RESOURCE_FILE"
	k create -f "$TLS_PROXY_RESOURCE_FILE" >/dev/null
	: >"$RESOURCE_FILE"
	: >"$TLS_PROXY_RESOURCE_FILE"

	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg image "$FIXTURE_IMAGE" \
		--arg service "$TLS_PROXY_SERVICE" \
		--arg upstream "http://${REGISTRY_HOST}" \
		--arg certificateSecret "$TLS_PROXY_CERT_SECRET" \
		--arg caConfigMap "$TLS_PROXY_CA_CONFIGMAP" \
		--arg goodAuthSecret "$TLS_PROXY_GOOD_AUTH_SECRET" \
		--arg badCAAuthSecret "$TLS_PROXY_BAD_CA_AUTH_SECRET" \
		--arg badAuthoritySecret "$TLS_PROXY_BAD_AUTHORITY_SECRET" \
		--arg registryAuthority "$TLS_PROXY_AUTHORITY" \
		--arg wrongRegistryAuthority "$TLS_PROXY_WRONG_AUTHORITY" \
		--arg caSHA256 "$TLS_PROXY_CA_SHA256" \
		--arg badCASHA256 "$TLS_PROXY_BAD_CA_SHA256" \
		--arg registryUsername "$REGISTRY_USERNAME" \
		--rawfile registryPassword "$REGISTRY_PASSWORD_FILE" \
		--arg pullSecret "$REGISTRY_PULL_SECRET" '
    def labels: {
      "app.kubernetes.io/name": $service,
      "app.kubernetes.io/component": "e2e-tls-registry-proxy"
    };
    def authSecret($name; $authority; $caDigest): {
      apiVersion: "v1", kind: "Secret",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      type: "Opaque",
      stringData: {
        username: $registryUsername,
        password: $registryPassword,
        registry: $authority,
        caSHA256: $caDigest
      }
    };
    {
      apiVersion: "v1", kind: "List", items: [
        authSecret($goodAuthSecret; $registryAuthority; $caSHA256),
        authSecret($badCAAuthSecret; $registryAuthority; $badCASHA256),
        authSecret($badAuthoritySecret; $wrongRegistryAuthority; $caSHA256),
        {
          apiVersion: "apps/v1", kind: "Deployment",
          metadata: {namespace: $namespace, name: $service, labels: labels},
          spec: {
            replicas: 1,
            selector: {matchLabels: labels},
            template: {
              metadata: {labels: labels},
              spec: {
                automountServiceAccountToken: false,
                enableServiceLinks: false,
                imagePullSecrets: [{name: $pullSecret}],
                terminationGracePeriodSeconds: 10,
                securityContext: {
                  runAsNonRoot: true,
                  runAsUser: 65532,
                  runAsGroup: 65532,
                  fsGroup: 65532,
                  fsGroupChangePolicy: "OnRootMismatch",
                  seccompProfile: {type: "RuntimeDefault"}
                },
                containers: [{
                  name: "tls-registry-proxy",
                  image: $image,
                  imagePullPolicy: "IfNotPresent",
                  command: ["/e2e-handcraft-oci"],
                  args: [
                    "tls-proxy",
                    "--listen=:5443",
                    ("--upstream=" + $upstream),
                    "--cert-file=/tls/tls.crt",
                    "--key-file=/tls/tls.key"
                  ],
                  ports: [
                    {name: "tls", containerPort: 5443, protocol: "TCP"},
                    {name: "admin", containerPort: 8081, protocol: "TCP"}
                  ],
                  resources: {
                    requests: {cpu: "10m", memory: "16Mi"},
                    limits: {cpu: "100m", memory: "64Mi"}
                  },
                  readinessProbe: {tcpSocket: {port: "tls"}, initialDelaySeconds: 1, periodSeconds: 2},
                  livenessProbe: {tcpSocket: {port: "admin"}, initialDelaySeconds: 2, periodSeconds: 5},
                  securityContext: {
                    allowPrivilegeEscalation: false,
                    readOnlyRootFilesystem: true,
                    runAsNonRoot: true,
                    runAsUser: 65532,
                    runAsGroup: 65532,
                    capabilities: {drop: ["ALL"]},
                    seccompProfile: {type: "RuntimeDefault"}
                  },
                  volumeMounts: [{name: "tls", mountPath: "/tls", readOnly: true}]
                }],
                volumes: [{
                  name: "tls",
                  secret: {
                    secretName: $certificateSecret,
                    defaultMode: 288,
                    items: [
                      {key: "tls.crt", path: "tls.crt", mode: 288},
                      {key: "tls.key", path: "tls.key", mode: 288}
                    ]
                  }
                }]
              }
            }
          }
        },
        {
          apiVersion: "v1", kind: "Service",
          metadata: {namespace: $namespace, name: $service},
          spec: {
            ipFamilyPolicy: "SingleStack",
            selector: labels,
            ports: [{name: "tls", port: 5443, targetPort: "tls", protocol: "TCP"}]
          }
        }
      ]
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	: >"$RESOURCE_FILE"

	k -n "$TEST_NAMESPACE" rollout status deployment/"$TLS_PROXY_SERVICE" \
		--timeout="${TIMEOUT_SECONDS}s"
	k -n "$TEST_NAMESPACE" get configmap "$TLS_PROXY_CA_CONFIGMAP" -o json |
		jq -e --rawfile expectedCA "$TLS_PROXY_CA_FILE" '
          .immutable == true and (.data | keys) == ["ca.pem"] and
          .data["ca.pem"] == $expectedCA
        ' \
		>/dev/null || fail "TLS proxy CA ConfigMap is not an immutable single-key trust bundle"
	k -n "$TEST_NAMESPACE" get secrets \
		"$TLS_PROXY_GOOD_AUTH_SECRET" \
		"$TLS_PROXY_BAD_CA_AUTH_SECRET" \
		"$TLS_PROXY_BAD_AUTHORITY_SECRET" -o json |
		jq -e \
			--arg good "$TLS_PROXY_GOOD_AUTH_SECRET" \
			--arg badCA "$TLS_PROXY_BAD_CA_AUTH_SECRET" \
			--arg badAuthority "$TLS_PROXY_BAD_AUTHORITY_SECRET" \
			--arg authority "$TLS_PROXY_AUTHORITY" \
			--arg wrongAuthority "$TLS_PROXY_WRONG_AUTHORITY" \
			--arg caSHA256 "$TLS_PROXY_CA_SHA256" \
			--arg badCASHA256 "$TLS_PROXY_BAD_CA_SHA256" '
          def named($name):
            [.items[] | select(.metadata.name == $name)] |
            if length == 1 then .[0] else error("missing exact auth Secret") end;
          def fixed_shape($secret):
            $secret.immutable == true and $secret.type == "Opaque" and
            ($secret.data | keys | sort) == ["caSHA256", "password", "registry", "username"];
          named($good) as $goodSecret |
          named($badCA) as $badCASecret |
          named($badAuthority) as $badAuthoritySecret |
          ($authority != $wrongAuthority) and ($caSHA256 != $badCASHA256) and
          fixed_shape($goodSecret) and fixed_shape($badCASecret) and
          fixed_shape($badAuthoritySecret) and
          $goodSecret.data.registry == ($authority | @base64) and
          $goodSecret.data.caSHA256 == ($caSHA256 | @base64) and
          $badCASecret.data.registry == ($authority | @base64) and
          $badCASecret.data.caSHA256 == ($badCASHA256 | @base64) and
          $badAuthoritySecret.data.registry == ($wrongAuthority | @base64) and
          $badAuthoritySecret.data.caSHA256 == ($caSHA256 | @base64) and
          ($goodSecret.data.username | length) > 0 and
          ($goodSecret.data.password | length) > 0 and
          $badCASecret.data.username == $goodSecret.data.username and
          $badAuthoritySecret.data.username == $goodSecret.data.username and
          $badCASecret.data.password == $goodSecret.data.password and
          $badAuthoritySecret.data.password == $goodSecret.data.password
        ' >/dev/null ||
		fail "TLS proxy auth Secrets lost their orthogonal fixed authority and CA grants"
	k -n "$TEST_NAMESPACE" get deployment "$TLS_PROXY_SERVICE" -o json |
		jq -e \
			--arg image "$FIXTURE_IMAGE" \
			--arg certificateSecret "$TLS_PROXY_CERT_SECRET" \
			--arg upstream "http://${REGISTRY_HOST}" \
			--arg pullSecret "$REGISTRY_PULL_SECRET" '
          .spec.replicas == 1 and
          .spec.template.spec.automountServiceAccountToken == false and
          .spec.template.spec.enableServiceLinks == false and
          .spec.template.spec.imagePullSecrets == [{name: $pullSecret}] and
          .spec.template.spec.securityContext.runAsNonRoot == true and
          .spec.template.spec.securityContext.runAsUser == 65532 and
		  .spec.template.spec.securityContext.runAsGroup == 65532 and
		  .spec.template.spec.securityContext.fsGroup == 65532 and
		  .spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" and
		  .spec.template.spec.securityContext.seccompProfile == {type: "RuntimeDefault"} and
		  (.spec.template.spec.containers | length) == 1 and
          .spec.template.spec.containers[0] as $container |
          $container.image == $image and $container.command == ["/e2e-handcraft-oci"] and
          $container.args == [
            "tls-proxy", "--listen=:5443", ("--upstream=" + $upstream),
            "--cert-file=/tls/tls.crt", "--key-file=/tls/tls.key"
		  ] and ($container.env // []) == [] and ($container.envFrom // []) == [] and
		  $container.securityContext.allowPrivilegeEscalation == false and
		  $container.securityContext.readOnlyRootFilesystem == true and
		  $container.securityContext.runAsNonRoot == true and
		  $container.securityContext.runAsUser == 65532 and
		  $container.securityContext.runAsGroup == 65532 and
		  $container.securityContext.capabilities.drop == ["ALL"] and
		  $container.securityContext.seccompProfile == {type: "RuntimeDefault"} and
          $container.volumeMounts == [{name: "tls", readOnly: true, mountPath: "/tls"}] and
          .spec.template.spec.volumes == [{
            name: "tls", secret: {
              secretName: $certificateSecret, defaultMode: 288,
              items: [
                {key: "tls.crt", path: "tls.crt", mode: 288},
                {key: "tls.key", path: "tls.key", mode: 288}
              ]
            }
          }]
        ' >/dev/null || fail "TLS registry proxy Deployment lost its hardened credential-free contract"
	k -n "$TEST_NAMESPACE" get service "$TLS_PROXY_SERVICE" -o json |
		jq -e --arg service "$TLS_PROXY_SERVICE" '
          .spec.ipFamilyPolicy == "SingleStack" and
          (.spec.ipFamilies | length) == 1 and
          .spec.publishNotReadyAddresses != true and
          .spec.selector == {
            "app.kubernetes.io/name": $service,
            "app.kubernetes.io/component": "e2e-tls-registry-proxy"
          } and
          (.spec.ports | length) == 1 and
          .spec.ports[0].name == "tls" and .spec.ports[0].port == 5443 and
          .spec.ports[0].targetPort == "tls" and .spec.ports[0].protocol == "TCP"
        ' >/dev/null || fail "TLS proxy Service $TLS_PROXY_SERVICE lost its exact single-stack routing contract"
}

tls_proxy_request_count() {
	assert_tls_proxy_identity_stable
	count=$(k get --raw \
		"/api/v1/namespaces/${TEST_NAMESPACE}/pods/http:${TLS_PROXY_POD_NAME}:8081/proxy/") ||
		fail "could not read the credential-free TLS proxy request counter through the exact Pod API proxy"
	printf '%s\n' "$count" | grep -Eq '^(0|[1-9][0-9]*)$' ||
		fail "TLS proxy request counter returned a non-integer response"
	assert_tls_proxy_identity_stable
	printf '%s\n' "$count"
}

capture_tls_proxy_identity() {
	tls_proxy_pods=$(k -n "$TEST_NAMESPACE" get pods \
		-l "app.kubernetes.io/name=${TLS_PROXY_SERVICE},app.kubernetes.io/component=e2e-tls-registry-proxy" \
		-o json)
	if ! tls_proxy_identity=$(printf '%s\n' "$tls_proxy_pods" | jq -ec '
	  [.items[] | select(.metadata.deletionTimestamp == null)] as $live |
	  if ($live | length) == 1 and
	      $live[0].status.phase == "Running" and
	      ($live[0].status.podIP | type == "string" and length > 0) and
	      ($live[0].status.conditions | any(.type == "Ready" and .status == "True")) and
	      ([$live[0].status.containerStatuses[]? | select(
	        .name == "tls-registry-proxy" and .ready == true and
	        .restartCount == 0 and (.containerID | length) > 0 and
	        .state.running != null)] | length) == 1
	  then {
	    name: $live[0].metadata.name,
	    uid: $live[0].metadata.uid,
	    podIP: $live[0].status.podIP,
	    containerID: ($live[0].status.containerStatuses[] |
	      select(.name == "tls-registry-proxy") | .containerID)
	  } else error("expected one ready proxy Pod") end
    '); then
		fail "TLS registry proxy does not have one exact zero-restart ready Pod"
	fi
	TLS_PROXY_POD_NAME=$(printf '%s\n' "$tls_proxy_identity" | jq -er '.name')
	TLS_PROXY_POD_UID=$(printf '%s\n' "$tls_proxy_identity" | jq -er '.uid')
	TLS_PROXY_POD_IP=$(printf '%s\n' "$tls_proxy_identity" | jq -er '.podIP')
	TLS_PROXY_CONTAINER_ID=$(printf '%s\n' "$tls_proxy_identity" | jq -er '.containerID')
	if [ -z "$TLS_PROXY_POD_NAME" ] || [ -z "$TLS_PROXY_POD_UID" ] ||
		[ -z "$TLS_PROXY_POD_IP" ] || [ -z "$TLS_PROXY_CONTAINER_ID" ]; then
		fail "TLS registry proxy exact Pod identity is incomplete"
	fi
	wait_for_tls_proxy_service_endpoints
	assert_tls_proxy_identity_stable
}

tls_proxy_service_endpoints_match() {
	k -n "$TEST_NAMESPACE" get endpointslices \
		-l "kubernetes.io/service-name=${TLS_PROXY_SERVICE}" -o json 2>/dev/null |
		jq -e \
			--arg namespace "$TEST_NAMESPACE" \
			--arg name "$TLS_PROXY_POD_NAME" \
			--arg uid "$TLS_PROXY_POD_UID" \
			--arg podIP "$TLS_PROXY_POD_IP" \
			-f "$ROOT_DIR/testdata/e2e/tls-proxy-service-endpoints.jq" >/dev/null
}

wait_for_tls_proxy_service_endpoints() {
	tls_proxy_endpoint_attempt=0
	while [ "$tls_proxy_endpoint_attempt" -lt "$TLS_PROXY_ENDPOINT_WAIT_ATTEMPTS" ]; do
		if tls_proxy_service_endpoints_match; then
			return 0
		fi
		tls_proxy_endpoint_attempt=$((tls_proxy_endpoint_attempt + 1))
		if [ "$tls_proxy_endpoint_attempt" -lt "$TLS_PROXY_ENDPOINT_WAIT_ATTEMPTS" ]; then
			sleep 1
		fi
	done
	fail "TLS proxy Service $TLS_PROXY_SERVICE did not converge on the captured exact Pod"
}

assert_tls_proxy_service_endpoints() {
	tls_proxy_service_endpoints_match ||
		fail "TLS proxy Service $TLS_PROXY_SERVICE can route outside the captured exact Pod"
}

assert_tls_proxy_identity_stable() {
	if [ -z "$TLS_PROXY_POD_NAME" ] || [ -z "$TLS_PROXY_POD_UID" ] ||
		[ -z "$TLS_PROXY_POD_IP" ] || [ -z "$TLS_PROXY_CONTAINER_ID" ]; then
		fail "TLS registry proxy identity was not captured before the counter window"
	fi
	tls_proxy_pods=$(k -n "$TEST_NAMESPACE" get pods \
		-l "app.kubernetes.io/name=${TLS_PROXY_SERVICE},app.kubernetes.io/component=e2e-tls-registry-proxy" \
		-o json)
	printf '%s\n' "$tls_proxy_pods" |
		jq -e \
			--arg name "$TLS_PROXY_POD_NAME" \
			--arg uid "$TLS_PROXY_POD_UID" \
			--arg podIP "$TLS_PROXY_POD_IP" \
			--arg containerID "$TLS_PROXY_CONTAINER_ID" '
	      [.items[] | select(.metadata.deletionTimestamp == null)] as $live |
	      ($live | length) == 1 and
	      $live[0].status.phase == "Running" and
	      ($live[0].status.conditions | any(.type == "Ready" and .status == "True")) and
	      $live[0].metadata.name == $name and $live[0].metadata.uid == $uid and
	      $live[0].status.podIP == $podIP and
	      ([$live[0].status.containerStatuses[] | select(
	        .name == "tls-registry-proxy" and .restartCount == 0 and
	        .ready == true and .containerID == $containerID and
	        .state.running != null)] | length) == 1
        ' >/dev/null ||
		fail "TLS registry proxy Pod or container identity changed inside the counter window"
	assert_tls_proxy_service_endpoints
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

create_digest_pin_policy_fixture() {
	digest_pin_policy_file="$ROOT_DIR/testdata/e2e/verification-policy-digest-pin.yaml"
	[ -f "$digest_pin_policy_file" ] ||
		fail "digest-pin verification policy fixture is missing"
	k -n "$TEST_NAMESPACE" create configmap "$DIGEST_PIN_POLICY_NAME" \
		--from-file="policy.yaml=${digest_pin_policy_file}" \
		--dry-run=client -o json | jq '.immutable = true' | k create -f - >/dev/null
	k -n "$TEST_NAMESPACE" get configmap "$DIGEST_PIN_POLICY_NAME" -o json |
		jq -e '.immutable == true and (.data["policy.yaml"] | contains("require_digest_pin: true"))' \
			>/dev/null || fail "digest-pin verification policy ConfigMap is not immutable or strict"
	k -n "$TEST_NAMESPACE" get secret "$DIGEST_PIN_DOCKER_AUTH_SECRET" -o json |
		jq -e \
			--arg registry "$REGISTRY_HOST" \
			--arg username "$REGISTRY_USERNAME" \
			--rawfile password "$REGISTRY_PASSWORD_FILE" '
          .immutable == true and .type == "kubernetes.io/dockerconfigjson" and
          (.data | keys | sort) == [".dockerconfigjson", "allowPlainHTTP", "registry"] and
          .data.registry == ($registry | @base64) and
          .data.allowPlainHTTP == ("true" | @base64) and
          (.data[".dockerconfigjson"] | @base64d | fromjson) as $config |
          ($config | keys) == ["auths"] and
          ($config.auths | keys) == [$registry] and
          $config.auths[$registry].username == $username and
          $config.auths[$registry].password == $password and
          $config.auths[$registry].auth == (($username + ":" + $password) | @base64)
        ' >/dev/null ||
		fail "digest-pin Docker config Secret lost its fixed credential-owner grants"
}

credential_suffix=$(printf '%s' "$TEST_NAMESPACE" | cksum | awk '{print $1}')
PG_USER=ptah_e2e
PG_DATABASE=ptah_e2e
PG_PASSWORD="e2ePg${credential_suffix}Q7"
PG_SECRET=e2e-postgresql-db
PG_SERVICE=e2e-postgresql
PG_URL="postgres://${PG_USER}:${PG_PASSWORD}@${PG_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5432/${PG_DATABASE}?sslmode=disable"
CUSTOM_CA_PG_DATABASE=ptah_e2e_custom_ca
CUSTOM_CA_PG_SECRET=e2e-postgresql-custom-ca-db
CUSTOM_CA_PG_URL="postgres://${PG_USER}:${PG_PASSWORD}@${PG_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5432/${CUSTOM_CA_PG_DATABASE}?sslmode=disable"
CUSTOM_CA_COORDINATION_KEY=e2e/custom-ca/app
MYSQL_USER=ptah_e2e
MYSQL_DATABASE=ptah_e2e
MYSQL_PASSWORD="e2eMy${credential_suffix}Q7"
MYSQL_ROOT_PASSWORD="e2eMyRoot${credential_suffix}Q7"
MYSQL_SECRET=e2e-mysql-db
MYSQL_SERVICE=e2e-mysql
MYSQL_URL="mysql://${MYSQL_USER}:${MYSQL_PASSWORD}@tcp(${MYSQL_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:3306)/${MYSQL_DATABASE}"
EXTERNAL_PG_SECRET=e2e-postgresql-external-db
EXTERNAL_PG_SCHEMA=e2e-postgresql-external
EXTERNAL_PG_COORDINATION_KEY=e2e/postgresql-external/app
REGISTRY_AUTH_SECRET=e2e-registry-auth
REGISTRY_PULL_SECRET=e2e-registry-pull
DIGEST_PIN_DOCKER_AUTH_SECRET=e2e-registry-digest-pin-docker-auth
REGISTRY_HOST="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000"
TLS_PROXY_AUTHORITY="${TLS_PROXY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5443"
TLS_PROXY_REFERENCE="oci://${TLS_PROXY_AUTHORITY}/schemas/postgresql:stable"
TLS_PROXY_CA_CONFIGMAP=e2e-registry-tls-ca
TLS_PROXY_CERT_SECRET=e2e-registry-tls-server
TLS_PROXY_GOOD_AUTH_SECRET=e2e-registry-tls-auth
TLS_PROXY_BAD_CA_AUTH_SECRET=e2e-registry-tls-auth-bad-ca
TLS_PROXY_BAD_AUTHORITY_SECRET=e2e-registry-tls-auth-bad-authority
TLS_PROXY_WRONG_AUTHORITY=registry-mismatch.invalid:5443
TLS_PROXY_CA_SHA256="sha256:$(sha256 <"$TLS_PROXY_CA_FILE")"
TLS_PROXY_BAD_CA_SHA256=sha256:0000000000000000000000000000000000000000000000000000000000000000
printf '%s\n' "$TLS_PROXY_CA_SHA256" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "TLS proxy CA does not have a lowercase SHA-256 digest"
[ "$TLS_PROXY_CA_SHA256" != "$TLS_PROXY_BAD_CA_SHA256" ] ||
	fail "fixed mismatched TLS proxy CA grant unexpectedly matched the generated CA"
[ "$TLS_PROXY_WRONG_AUTHORITY" != "$TLS_PROXY_AUTHORITY" ] ||
	fail "fixed mismatched TLS proxy registry authority unexpectedly matched the live authority"
	if [ "$CUSTOM_CA_PG_DATABASE" = "$PG_DATABASE" ] ||
		[ "$CUSTOM_CA_PG_SECRET" = "$PG_SECRET" ]; then
		fail "custom-CA acceptance must use a distinct PostgreSQL database and Secret"
	fi

printf '%s' "$PG_PASSWORD" >"$PG_PASSWORD_FILE"
printf '%s' "$PG_URL" >"$PG_URL_FILE"
printf '%s' "$CUSTOM_CA_PG_URL" >"$CUSTOM_CA_PG_URL_FILE"
printf '%s' "$MYSQL_PASSWORD" >"$MYSQL_PASSWORD_FILE"
printf '%s' "$MYSQL_ROOT_PASSWORD" >"$MYSQL_ROOT_PASSWORD_FILE"
printf '%s' "$MYSQL_URL" >"$MYSQL_URL_FILE"
printf '%s' "$REGISTRY_PASSWORD" >"$REGISTRY_PASSWORD_FILE"
printf '%s\n' \
	"$REGISTRY_PASSWORD" \
	"$PG_PASSWORD" "$PG_URL" "$CUSTOM_CA_PG_URL" \
	"$MYSQL_PASSWORD" "$MYSQL_ROOT_PASSWORD" "$MYSQL_URL" \
	>"$CREDENTIAL_PATTERNS_FILE"
jq -er '.password, .url' "$EXTERNAL_PG_CREDENTIALS_FILE" >>"$CREDENTIAL_PATTERNS_FILE"
chmod 600 \
	"$PG_PASSWORD_FILE" "$PG_URL_FILE" "$CUSTOM_CA_PG_URL_FILE" \
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
		--arg customCAPGDatabase "$CUSTOM_CA_PG_DATABASE" \
		--rawfile customCAPGURL "$CUSTOM_CA_PG_URL_FILE" \
		--arg customCAPGSecret "$CUSTOM_CA_PG_SECRET" \
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
		--arg registryPullSecret "$REGISTRY_PULL_SECRET" \
		--arg digestPinDockerAuthSecret "$DIGEST_PIN_DOCKER_AUTH_SECRET" '
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
            username: $registryUsername, password: $registryPassword,
            registry: $registryHost, allowPlainHTTP: "true"
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
		  metadata: {namespace: $namespace, name: $digestPinDockerAuthSecret},
		  immutable: true,
		  type: "kubernetes.io/dockerconfigjson",
		  stringData: {
		    ".dockerconfigjson": ({auths: {
		      ($registryHost): {
		        username: $registryUsername,
		        password: $registryPassword,
		        auth: (($registryUsername + ":" + $registryPassword) | @base64)
		      }
		    }} | tojson),
		    registry: $registryHost,
		    allowPlainHTTP: "true"
		  }
		},
		{
		  apiVersion: "v1", kind: "Secret",
		  metadata: {namespace: $namespace, name: $pgSecret},
		  type: "Opaque",
		  stringData: {username: $pgUser, password: $pgPassword, database: $pgDatabase, url: $pgURL}
		},
		{
		  apiVersion: "v1", kind: "Secret",
		  metadata: {namespace: $namespace, name: $customCAPGSecret},
		  immutable: true,
		  type: "Opaque",
		  stringData: {
		    username: $pgUser, password: $pgPassword,
		    database: $customCAPGDatabase, url: $customCAPGURL
		  }
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

create_custom_ca_database() {
	k -n "$TEST_NAMESPACE" get secrets "$PG_SECRET" "$CUSTOM_CA_PG_SECRET" -o json |
		jq -e \
			--arg primary "$PG_SECRET" \
			--arg custom "$CUSTOM_CA_PG_SECRET" \
			--arg username "$PG_USER" \
			--arg database "$CUSTOM_CA_PG_DATABASE" \
			--rawfile expectedURL "$CUSTOM_CA_PG_URL_FILE" '
          def named($name):
            [.items[] | select(.metadata.name == $name)] |
            if length == 1 then .[0] else error("missing exact database Secret") end;
          named($primary) as $primarySecret |
          named($custom) as $customSecret |
          $customSecret.immutable == true and $customSecret.type == "Opaque" and
          ($customSecret.data | keys | sort) == ["database", "password", "url", "username"] and
          $customSecret.data.username == ($username | @base64) and
          $customSecret.data.database == ($database | @base64) and
          $customSecret.data.url == ($expectedURL | @base64) and
          $customSecret.data.password == $primarySecret.data.password
        ' >/dev/null ||
		fail "custom-CA PostgreSQL Secret lost its distinct fixed target binding"

	# shellcheck disable=SC2016 # Variables expand inside the database container.
	custom_ca_database_count=$(k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "SELECT count(*) FROM pg_database WHERE datname='"'"'ptah_e2e_custom_ca'"'"'"')
	custom_ca_database_count=$(printf '%s' "$custom_ca_database_count" | tr -d '[:space:]')
	case "$custom_ca_database_count" in
	0)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE ptah_e2e_custom_ca"' \
			>/dev/null
		;;
	1) ;;
	*) fail "custom-CA PostgreSQL database lookup returned an unexpected result" ;;
	esac
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	custom_ca_database_result=$(k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d ptah_e2e_custom_ca -Atqc "SELECT current_database()"')
	custom_ca_database_result=$(printf '%s' "$custom_ca_database_result" | tr -d '[:space:]')
	[ "$custom_ca_database_result" = "$CUSTOM_CA_PG_DATABASE" ] ||
		fail "custom-CA PostgreSQL database did not become independently queryable"
}

custom_ca_database_schema_fingerprint() {
	custom_ca_catalog_file=$WORK_DIR/custom-ca-postgresql-catalog.json
	custom_ca_catalog_query="WITH user_namespaces AS (
SELECT oid, nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'
), relations AS (
SELECT n.nspname, c.relname, c.relkind::text, c.relpersistence::text,
CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid, true) ELSE '' END AS definition
FROM pg_class c JOIN user_namespaces n ON n.oid = c.relnamespace
), columns AS (
SELECT n.nspname, c.relname, a.attnum, a.attname, format_type(a.atttypid, a.atttypmod) AS data_type,
a.attnotnull, a.attidentity::text, a.attgenerated::text, COALESCE(pg_get_expr(d.adbin, d.adrelid), '') AS default_expression
FROM pg_class c JOIN user_namespaces n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
), constraints AS (
SELECT n.nspname, c.relname, x.conname, x.contype::text, pg_get_constraintdef(x.oid, true) AS definition
FROM pg_constraint x JOIN pg_class c ON c.oid = x.conrelid
JOIN user_namespaces n ON n.oid = c.relnamespace
), indexes AS (
SELECT n.nspname, c.relname, i.relname AS index_name, pg_get_indexdef(i.oid) AS definition
FROM pg_index x JOIN pg_class c ON c.oid = x.indrelid
JOIN pg_class i ON i.oid = x.indexrelid JOIN user_namespaces n ON n.oid = c.relnamespace
), types AS (
SELECT n.nspname, t.typname, t.typtype::text, t.typcategory::text,
COALESCE((SELECT json_agg(e.enumlabel ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid), '[]'::json) AS enum_labels
FROM pg_type t JOIN user_namespaces n ON n.oid = t.typnamespace WHERE t.typtype <> 'b'
), routines AS (
SELECT n.nspname, p.proname, pg_get_function_identity_arguments(p.oid) AS arguments,
pg_get_function_result(p.oid) AS result, p.prokind::text, pg_get_functiondef(p.oid) AS definition
FROM pg_proc p JOIN user_namespaces n ON n.oid = p.pronamespace WHERE p.prokind IN ('f','p')
)
SELECT json_build_object(
'schemas', (SELECT COALESCE(json_agg(nspname ORDER BY nspname), '[]'::json) FROM user_namespaces),
'relations', (SELECT COALESCE(json_agg(relations ORDER BY nspname, relname), '[]'::json) FROM relations),
'columns', (SELECT COALESCE(json_agg(columns ORDER BY nspname, relname, attnum), '[]'::json) FROM columns),
'constraints', (SELECT COALESCE(json_agg(constraints ORDER BY nspname, relname, conname), '[]'::json) FROM constraints),
'indexes', (SELECT COALESCE(json_agg(indexes ORDER BY nspname, relname, index_name), '[]'::json) FROM indexes),
'types', (SELECT COALESCE(json_agg(types ORDER BY nspname, typname), '[]'::json) FROM types),
'routines', (SELECT COALESCE(json_agg(routines ORDER BY nspname, proname, arguments), '[]'::json) FROM routines)
)"
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	k -n "$TEST_NAMESPACE" exec deployment/"$PG_SERVICE" -- \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d ptah_e2e_custom_ca -v ON_ERROR_STOP=1 -Atqc "$1"' \
		sh "$custom_ca_catalog_query" >"$custom_ca_catalog_file"
	scan_file_for_credentials "$custom_ca_catalog_file" \
		"the custom-CA PostgreSQL schema fingerprint"
	custom_ca_catalog_fingerprint=$(sha256 <"$custom_ca_catalog_file")
	: >"$custom_ca_catalog_file"
	printf 'sha256:%s\n' "$custom_ca_catalog_fingerprint"
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
		sh "SELECT count(*) FROM (SELECT index_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND index_name='e2e_widgets_name_unique' GROUP BY index_name HAVING count(*)=1 AND sum(non_unique=0 AND column_name='name' AND seq_in_index=1 AND expression IS NULL AND sub_part IS NULL)=1) AS exact_index")
	index_actual=$(printf '%s' "$index_actual" | tr -d '[:space:]')
	[ "$index_actual" = "$index_expected" ] ||
		fail "MySQL unique index count is $index_actual, expected $index_expected"
}

assert_mysql_plain_index() {
	index_expected=$1
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	index_actual=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "$1"' \
		sh "SELECT count(*) FROM (SELECT index_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND index_name='e2e_widgets_name_idx' GROUP BY index_name HAVING count(*)=1 AND sum(non_unique=1 AND column_name='name' AND seq_in_index=1 AND expression IS NULL AND sub_part IS NULL)=1) AS exact_index")
	index_actual=$(printf '%s' "$index_actual" | tr -d '[:space:]')
	[ "$index_actual" = "$index_expected" ] ||
		fail "MySQL plain index count is $index_actual, expected $index_expected"
}

assert_mysql_declared_element_order() {
	# This table deliberately declares a named foreign key before an unnamed
	# index whose column collides with that constraint name. MySQL allocates
	# both backing-index names in declaration order.
	# shellcheck disable=SC2016 # Variables expand inside the database container.
	index_order=$(k -n "$TEST_NAMESPACE" exec deployment/"$MYSQL_SERVICE" -- \
		sh -ec 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "$1"' \
		sh "SELECT GROUP_CONCAT(CONCAT(index_name, ':', column_name) ORDER BY index_name, seq_in_index SEPARATOR ',') FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_order_child'")
	index_order=$(printf '%s' "$index_order" | tr -d '\r\n')
	[ "$index_order" = 'b:a,b_2:b' ] ||
		fail "MySQL declaration-order indexes are $index_order, expected b:a,b_2:b"
}

rewrite_mysql_refusal_job() {
	rewrite_namespace=$1
	rewrite_name=$2
	rewrite_schema=$3
	rewrite_operation=$4
	rewrite_operation_id=$5
	rewrite_secret=$6
	jq \
		--arg namespace "$rewrite_namespace" \
		--arg name "$rewrite_name" \
		--arg schema "$rewrite_schema" \
		--arg operation "$rewrite_operation" \
		--arg operationID "$rewrite_operation_id" \
		--arg secret "$rewrite_secret" '
          # mysql-refusal-job-rewrite-begin
          def rewrite_env:
            if (.env | type) == "array" then
              .env |= map(
                if .name == "PTAH_DB_URL" then
                  del(.value) |
                  .valueFrom = {secretKeyRef: {name: $secret, key: "url"}}
                elif .name == "PTAH_OPERATION_ID" then
                  .value = $operationID | del(.valueFrom)
                else . end)
            else
              .
            end;
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
              if (.template.spec | has("initContainers")) and
                  .template.spec.initContainers != null then
                .template.spec.initContainers |= map(rewrite_env)
              else
                .
              end)
          }
          # mysql-refusal-job-rewrite-end
        '
}

assert_mysql_refusal_rewrite_without_init_containers() {
	refusal_rewrite_source=$(jq -n '
        {
          spec: {
            backoffLimit: 7,
            template: {
              metadata: {labels: {preserved: "source-only"}},
              spec: {
                restartPolicy: "Never",
                containers: [{
                  name: "ptah",
                  image: "fixture.invalid/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                  args: ["observe"],
                  env: [
                    {name: "PTAH_DB_URL", value: "mysql://original.invalid"},
                    {name: "PTAH_OPERATION_ID", valueFrom: {fieldRef: {fieldPath: "metadata.uid"}}},
                    {name: "PRESERVED", value: "exact"}
                  ]
                }]
              }
            }
          }
        }
      ')
	refusal_rewrite_probe=$(printf '%s\n' "$refusal_rewrite_source" |
		rewrite_mysql_refusal_job test-namespace test-name test-schema observe \
		test-operation-id test-secret) ||
		fail "MySQL invalid-DSN Job rewrite rejected a source without initContainers"
	printf '%s\n' "$refusal_rewrite_probe" | jq -e '
      (.spec.template.spec | has("initContainers") | not) and
      .spec.backoffLimit == 7 and
      .spec.template.spec.restartPolicy == "Never" and
      .spec.template.spec.containers == [{
        name: "ptah",
        image: "fixture.invalid/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        args: ["observe"],
        env: [
          {name: "PTAH_DB_URL", valueFrom: {secretKeyRef: {name: "test-secret", key: "url"}}},
          {name: "PTAH_OPERATION_ID", value: "test-operation-id"},
          {name: "PRESERVED", value: "exact"}
        ]
      }]
    ' >/dev/null ||
		fail "MySQL invalid-DSN Job rewrite changed absent initContainers or unrelated Pod semantics"
	refusal_null_rewrite_probe=$(printf '%s\n' "$refusal_rewrite_source" |
		jq '.spec.template.spec.initContainers = null' |
		rewrite_mysql_refusal_job test-namespace test-name test-schema observe \
			test-operation-id test-secret) ||
		fail "MySQL invalid-DSN Job rewrite rejected null initContainers"
	printf '%s\n' "$refusal_null_rewrite_probe" | jq -e '
      (.spec.template.spec | has("initContainers")) and
      .spec.template.spec.initContainers == null and
      .spec.backoffLimit == 7 and
      .spec.template.spec.restartPolicy == "Never" and
      (.spec.template.spec.containers[0].env | any(
        .name == "PTAH_DB_URL" and
        .valueFrom.secretKeyRef == {name: "test-secret", key: "url"})) and
      (.spec.template.spec.containers[0].env | any(
        .name == "PTAH_OPERATION_ID" and .value == "test-operation-id")) and
      (.spec.template.spec.containers[0].env | any(
        .name == "PRESERVED" and .value == "exact"))
    ' >/dev/null ||
		fail "MySQL invalid-DSN Job rewrite changed null initContainers or unrelated Pod semantics"
	refusal_mixed_init_rewrite_probe=$(printf '%s\n' "$refusal_rewrite_source" |
		jq '.spec.template.spec.initContainers = [
		  {name: "without-env", image: "fixture.invalid/helper:one"},
		  {name: "null-env", image: "fixture.invalid/helper:two", env: null}
		]' |
		rewrite_mysql_refusal_job test-namespace test-name test-schema observe \
			test-operation-id test-secret) ||
		fail "MySQL invalid-DSN Job rewrite rejected init containers without environment arrays"
	printf '%s\n' "$refusal_mixed_init_rewrite_probe" | jq -e '
	  .spec.template.spec.initContainers == [
	    {name: "without-env", image: "fixture.invalid/helper:one"},
	    {name: "null-env", image: "fixture.invalid/helper:two", env: null}
	  ] and
	  (.spec.template.spec.containers[0].env | any(
	    .name == "PTAH_DB_URL" and
	    .valueFrom.secretKeyRef == {name: "test-secret", key: "url"})) and
	  (.spec.template.spec.containers[0].env | any(
	    .name == "PTAH_OPERATION_ID" and .value == "test-operation-id"))
	' >/dev/null ||
		fail "MySQL invalid-DSN Job rewrite changed helper containers without environment arrays"
}

run_mysql_dsn_refusal() {
	unsafe_secret=e2e-mysql-unsafe-dsn
	unsafe_schema_label=e2e-mysql-dsn-negative
	assert_mysql_refusal_rewrite_without_init_containers
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
		printf '%s\n' "$refusal_source" |
			rewrite_mysql_refusal_job "$TEST_NAMESPACE" "$refusal_name" \
				"$unsafe_schema_label" "$refusal_operation" \
				"$refusal_operation_id" "$unsafe_secret" >"$RESOURCE_FILE"
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
              .protocolVersion == 4 and .operation == $operation and
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
	publish_file=${5:-"$ROOT_DIR/testdata/e2e/${publish_engine}-${publish_revision}.sql"}
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
	resource_policy=${6:-e2e-verification-policy}
	resource_registry_auth_secret=${7:-$REGISTRY_AUTH_SECRET}
	resource_registry_auth_mode=${8:-Environment}
	resource_failure_retry=${9:-5s}
	resource_interval=${10:-$APPROVAL_INTERVAL}
	case "$resource_registry_auth_mode" in
	Environment | DockerConfigJSON) ;;
	*) fail "unsupported E2E registry authentication mode $resource_registry_auth_mode" ;;
	esac
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$resource_schema" \
		--arg engine "$resource_engine" \
		--arg secret "$resource_secret" \
		--arg reference "$resource_reference" \
		--arg coordinationKey "$resource_coordination_key" \
		--arg policy "$resource_policy" \
		--arg registryAuthSecret "$resource_registry_auth_secret" \
		--arg registryAuthMode "$resource_registry_auth_mode" \
		--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" \
		--arg failureRetry "$resource_failure_retry" \
		--arg interval "$resource_interval" '
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
		  registryAuthFrom: (if $registryAuthMode == "DockerConfigJSON" then {
		    name: $registryAuthSecret,
		    mode: "DockerConfigJSON",
		    dockerConfigJSONKey: ".dockerconfigjson",
		    registryKey: "registry"
		  } else {
		    name: $registryAuthSecret,
		    mode: "Environment",
		    usernameKey: "username",
		    passwordKey: "password",
		    registryKey: "registry"
		  } end),
          verificationPolicyFrom: {name: $policy, key: "policy.yaml"},
          transport: {plainHTTP: true}
        },
        policy: {
          apply: "OnApproval", allowDestructive: false, driftSeverity: "all",
          lockTimeout: "30s", transactionMode: "file"
        },
        interval: $interval,
        execution: {
	          activeDeadlineSeconds: 300, failureRetryInterval: $failureRetry, connectTimeout: "30s",
	          runtimeClassName: $runtimeClass
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

create_custom_ca_schema_resource() {
	custom_ca_schema=$1
	custom_ca_auth_secret=$2
	custom_ca_coordination_key=$3
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$custom_ca_schema" \
		--arg databaseSecret "$CUSTOM_CA_PG_SECRET" \
		--arg reference "$TLS_PROXY_REFERENCE" \
		--arg coordinationKey "$custom_ca_coordination_key" \
		--arg registryAuthSecret "$custom_ca_auth_secret" \
		--arg caConfigMap "$TLS_PROXY_CA_CONFIGMAP" \
		--arg runtimeClass "$ADMISSION_RUNTIME_CLASS" '
    {
      apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        target: {
          engine: "PostgreSQL",
          coordinationKey: $coordinationKey,
          urlFrom: {name: $databaseSecret, key: "url"}
        },
        desired: {
          ociRef: $reference,
          registryAuthFrom: {
            name: $registryAuthSecret,
            mode: "Environment",
            usernameKey: "username",
            passwordKey: "password",
            tokenKey: "token",
            registryKey: "registry"
          },
          verificationPolicyFrom: {name: "e2e-verification-policy", key: "policy.yaml"},
          transport: {caFrom: {name: $caConfigMap, key: "ca.pem"}}
        },
        policy: {
          apply: "OnApproval", allowDestructive: false, driftSeverity: "all",
          lockTimeout: "30s", transactionMode: "file"
        },
        interval: "30m",
        execution: {
          activeDeadlineSeconds: 300,
          failureRetryInterval: "30m",
          connectTimeout: "30s",
          runtimeClassName: $runtimeClass
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

assert_custom_ca_completed_pods() {
	custom_ca_schema=$1
	custom_ca_resolved_reference=$2
	k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=${custom_ca_schema}" -o json |
		jq -e \
			--arg databaseSecret "$CUSTOM_CA_PG_SECRET" \
			--arg registrySecret "$TLS_PROXY_GOOD_AUTH_SECRET" \
			--argjson requireApply false \
			-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" >/dev/null ||
		fail "$custom_ca_schema Jobs lost custom-CA credential isolation"
	k -n "$TEST_NAMESPACE" get pods \
		-l "operator.ptah.dev/schema=${custom_ca_schema}" -o json |
		jq -e \
			--arg databaseSecret "$CUSTOM_CA_PG_SECRET" \
			--arg registrySecret "$TLS_PROXY_GOOD_AUTH_SECRET" \
			--arg registryAuthority "$TLS_PROXY_AUTHORITY" \
			--arg caConfigMap "$TLS_PROXY_CA_CONFIGMAP" \
			--arg resolvedReference "$custom_ca_resolved_reference" \
			-f "$ROOT_DIR/testdata/e2e/custom-ca-pod-isolation.jq" >/dev/null ||
		fail "$custom_ca_schema completed Observe/Plan Pods lost guard, CA snapshot, source, or fetch isolation"
}

assert_custom_ca_pre_child_refusal() {
	refusal_schema=$1
	refusal_auth_secret=$2
	refusal_coordination_key=$3
	refusal_description=$4
	refusal_before="$WORK_DIR/${refusal_schema}-before.json"
	refusal_after="$WORK_DIR/${refusal_schema}-after.json"
	refusal_result="$WORK_DIR/${refusal_schema}-resolve-result.json"
	refusal_proxy_count_before=$(tls_proxy_request_count)

	checkpoint_schema_jobs "$refusal_schema" "$refusal_before"
	create_custom_ca_schema_resource "$refusal_schema" "$refusal_auth_secret" \
		"$refusal_coordination_key"
	wait_for_schema "$refusal_schema" '
      .status.phase == "Failed" and .status.nextReconciliationTime != null and
      (.status.conditions | any(
        .type == "ReconciliationFailed" and .status == "True" and
        .reason == "OperationFailed" and
        (.message | startswith("invalid_oci_access:"))))
    ' "$refusal_description to fail before the Resolve child"
	capture_one_new_job_result "$refusal_schema" resolve "$refusal_before" "$refusal_result"
	jq -e '
      .childExitCode == -1 and .stdout == "" and
      .error.code == "invalid_oci_access" and
      (.resolvedDigest // "") == "" and
      (.resolvedReference // "") == "" and
      (.mutationStarted // false) == false and
      (.uncertain // false) == false and .truncation == null
    ' "$refusal_result" >/dev/null ||
		fail "$refusal_schema did not retain the typed pre-child invalid_oci_access result"
	k -n "$TEST_NAMESPACE" patch ptahschema "$refusal_schema" --type=merge \
		--patch '{"spec":{"suspend":true}}' >/dev/null
	wait_for_schema "$refusal_schema" \
		'.status.phase == "Suspended" and .status.activeOperation == null' \
		"$refusal_description fixture to suspend before retry"
	checkpoint_schema_jobs "$refusal_schema" "$refusal_after"
	assert_one_job_between_checkpoints "$refusal_schema" resolve \
		"$refusal_before" "$refusal_after"
	for refusal_operation in verify observe plan apply; do
		assert_no_job_between_checkpoints "$refusal_schema" "$refusal_operation" \
			"$refusal_before" "$refusal_after"
	done
	refusal_job_count=$(schema_job_count_between_checkpoints \
		"$refusal_schema" "$refusal_before" "$refusal_after")
	[ "$refusal_job_count" -eq 1 ] ||
		fail "$refusal_schema created $refusal_job_count total Jobs, expected only one Resolve"
	refusal_proxy_count_after=$(tls_proxy_request_count)
	assert_tls_proxy_identity_stable
	[ "$refusal_proxy_count_after" -eq "$refusal_proxy_count_before" ] ||
		fail "$refusal_schema reached the TLS registry before $refusal_description was refused"
}

assert_authenticated_https_custom_ca() {
	custom_ca_digest=$1
	custom_ca_resolved_reference="${TLS_PROXY_REFERENCE%:*}@${custom_ca_digest}"
	bad_ca_schema=e2e-https-ca-bad-ca
	bad_authority_schema=e2e-https-ca-bad-authority
	good_schema=e2e-https-ca-good
	good_before="$WORK_DIR/${good_schema}-before.json"
	good_after="$WORK_DIR/${good_schema}-after.json"
	good_suspended_after="$WORK_DIR/${good_schema}-suspended-after.json"

	printf '%s\n' 'e2e data plane: checking authenticated HTTPS custom-CA authority grants'
	capture_tls_proxy_identity
	assert_custom_ca_pre_child_refusal \
		"$bad_ca_schema" "$TLS_PROXY_BAD_CA_AUTH_SECRET" \
		"$CUSTOM_CA_COORDINATION_KEY" "its mismatched CA digest grant"
	assert_custom_ca_pre_child_refusal \
		"$bad_authority_schema" "$TLS_PROXY_BAD_AUTHORITY_SECRET" \
		"$CUSTOM_CA_COORDINATION_KEY" "its mismatched registry authority grant"

	good_database_fingerprint_before=$(custom_ca_database_schema_fingerprint)
	printf '%s\n' "$good_database_fingerprint_before" |
		grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "custom-CA PostgreSQL preflight schema fingerprint is invalid"
	good_proxy_count_before=$(tls_proxy_request_count)
	checkpoint_schema_jobs "$good_schema" "$good_before"
	create_custom_ca_schema_resource "$good_schema" "$TLS_PROXY_GOOD_AUTH_SECRET" \
		"$CUSTOM_CA_COORDINATION_KEY"
	assert_plan "$good_schema" "$TLS_PROXY_REFERENCE" "$custom_ca_digest" postgres false \
		"$good_before" "$good_before"
	wait_for_schema "$good_schema" \
		"$(cat "$ROOT_DIR/testdata/e2e/custom-ca-approval-boundary.jq")" \
		"the authenticated HTTPS custom-CA source to reach a nonmutating approval boundary"
	checkpoint_schema_jobs "$good_schema" "$good_after"
	for good_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$good_schema" "$good_operation" \
			"$good_before" "$good_after"
	done
	assert_no_job_between_checkpoints "$good_schema" apply "$good_before" "$good_after"
	assert_custom_ca_completed_pods "$good_schema" "$custom_ca_resolved_reference"
	good_proxy_count_after=$(tls_proxy_request_count)
	assert_tls_proxy_identity_stable
	[ "$good_proxy_count_after" -gt "$good_proxy_count_before" ] ||
		fail "$good_schema did not make authenticated requests through the TLS registry proxy"
	k -n "$TEST_NAMESPACE" patch ptahschema "$good_schema" --type=merge \
		--patch '{"spec":{"suspend":true}}' >/dev/null
	wait_for_schema "$good_schema" \
		'.status.phase == "Suspended" and .status.activeOperation == null' \
		"the authenticated HTTPS custom-CA fixture to suspend without Apply"
	checkpoint_schema_jobs "$good_schema" "$good_suspended_after"
	assert_no_job_between_checkpoints "$good_schema" apply "$good_before" "$good_suspended_after"
	good_database_fingerprint_after=$(custom_ca_database_schema_fingerprint)
	[ "$good_database_fingerprint_after" = "$good_database_fingerprint_before" ] ||
		fail "$good_schema changed the dedicated database before approval"
	audit_runtime_credentials
	printf '%s\n' 'e2e data plane: PASS authenticated HTTPS custom-CA authority and isolation'
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
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" \
			--arg type application/vnd.stokaro.ptah.schema.v1 '
      .status.executionBinding as $binding |
      .status.plan as $plan |
      .status.source.requestedReference == $reference and
      .status.source.digest == $digest and
      .status.source.resolvedReference == ($reference | sub(":[^/:]+$"; "@" + $digest)) and
      .status.source.verified == true and
      .status.source.artifactType == $type and
      .status.source.verificationPolicyDigest != "" and
		.status.target.identityDigest != "" and
		.status.target.driftReportDigest != "" and
      ($binding.epoch | test("^v1-[0-9a-f]{32}$")) and
      $binding.controllerImage == $controllerImage and
      $binding.controllerRevision == $controllerRevision and
      $binding.controllerStateVersion == $controllerStateVersion and
      $plan.executionBindingID == $binding.epoch and
      $plan.controllerImage == $controllerImage and
      $plan.controllerRevision == $controllerRevision and
      $plan.controllerStateVersion == $controllerStateVersion
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
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" \
			--argjson destructive "$plan_destructive" '
      .spec.contractVersion == 3 and
      .spec.schemaRef.name == $schema and
      .spec.artifactDigest == $digest and
      .spec.dialect == $dialect and
	  (.spec.executionBindingID | test("^v1-[0-9a-f]{32}$")) and
	  .spec.controllerImage == $controllerImage and
	  .spec.controllerRevision == $controllerRevision and
	  .spec.controllerStateVersion == $controllerStateVersion and
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
	approval_schema_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$approval_schema" -o json)
	approval_plan_object=$(k -n "$TEST_NAMESPACE" get ptahschemaplan "$approval_plan" -o json)
	approval_schema_uid=$(printf '%s\n' "$approval_schema_object" | jq -er '.metadata.uid')
	approval_plan_uid=$(printf '%s\n' "$approval_plan_object" | jq -er '.metadata.uid')
	approval_fingerprint=$(printf '%s\n' "$approval_plan_object" | jq -er '.spec.fingerprint')
	approval_execution_binding_id=$(printf '%s\n' "$approval_plan_object" | jq -er '.spec.executionBindingID')
	printf '%s\n' "$approval_schema_object" | jq -e \
		--arg plan "$approval_plan" \
		--arg planUID "$approval_plan_uid" \
		--arg fingerprint "$approval_fingerprint" \
		--arg executionBindingID "$approval_execution_binding_id" \
		--arg controllerImage "$CONTROLLER_IMAGE" \
		--arg controllerRevision "$CONTROLLER_REVISION" \
		--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .status.executionBinding as $binding |
      .status.plan as $current |
      ($binding.epoch | test("^v1-[0-9a-f]{32}$")) and
      $binding.epoch == $executionBindingID and
      $binding.controllerImage == $controllerImage and
      $binding.controllerRevision == $controllerRevision and
      $binding.controllerStateVersion == $controllerStateVersion and
      $current.name == $plan and $current.uid == $planUID and
      $current.fingerprint == $fingerprint and
      $current.executionBindingID == $executionBindingID and
      $current.controllerImage == $controllerImage and
      $current.controllerRevision == $controllerRevision and
      $current.controllerStateVersion == $controllerStateVersion
    ' >/dev/null || fail "$approval_schema current plan is not bound to the exact controller identity"
	printf '%s\n' "$approval_plan_object" | jq -e \
		--arg executionBindingID "$approval_execution_binding_id" \
		--arg controllerImage "$CONTROLLER_IMAGE" \
		--arg controllerRevision "$CONTROLLER_REVISION" \
		--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .spec.contractVersion == 3 and
      .spec.executionBindingID == $executionBindingID and
      .spec.controllerImage == $controllerImage and
      .spec.controllerRevision == $controllerRevision and
      .spec.controllerStateVersion == $controllerStateVersion
    ' >/dev/null || fail "$approval_plan is not a current-contract plan with the exact controller identity"
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
			--arg executionBindingID "$approval_execution_binding_id" \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" \
			--arg coordinationKey "$approval_coordination_key" \
			--arg coordinationDigest "$approval_coordination_digest" '
      .spec.schemaRef.name == $schema and .spec.planRef.name == $plan and
      .spec.planFingerprint == $fingerprint and .spec.artifactDigest != "" and
      .spec.coordinationDigest == $coordinationDigest and
      .spec.targetIdentityDigest != "" and
      .spec.actualStateFingerprint != "" and
      .spec.desiredStateFingerprint != "" and .spec.policyFingerprint != "" and
      .spec.verificationPolicyDigest != "" and .spec.ptahVersion != "" and
      .spec.executionBindingID == $executionBindingID and
      .spec.controllerImage == $controllerImage and
      .spec.controllerRevision == $controllerRevision and
      .spec.controllerStateVersion == $controllerStateVersion and
      (.spec.executorImage | test("@sha256:[0-9a-f]{64}$")) and
      (.spec.runnerImage | test("@sha256:[0-9a-f]{64}$")) and
      .spec.runnerProtocolVersion == 4 and .spec.approver.username != "" and
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

assert_source_job_isolation() {
	isolation_schema=$1
	isolation_secret=$2
	isolation_registry_secret=${3:-$REGISTRY_AUTH_SECRET}
	isolation_auth_mode=${4:-Environment}
	isolation_verification_policy=$5
	isolation_requested_reference=$6
	isolation_resolved_reference=$7
	k -n "$TEST_NAMESPACE" get jobs -l "operator.ptah.dev/schema=${isolation_schema}" -o json |
		jq -e \
			--arg databaseSecret "$isolation_secret" \
			--arg registrySecret "$isolation_registry_secret" \
			--arg registryAuthority "$REGISTRY_HOST" \
			--arg authMode "$isolation_auth_mode" \
			--arg executorImage "$EXECUTOR_IMAGE" \
			--arg runnerImage "$RUNNER_IMAGE" \
			--arg verificationPolicy "$isolation_verification_policy" \
			--arg serviceAccountName "" \
			--argjson imagePullSecrets "[]" \
			--arg requestedReference "$isolation_requested_reference" \
			--arg resolvedReference "$isolation_resolved_reference" \
			-f "$ROOT_DIR/testdata/e2e/source-job-isolation.jq" >/dev/null ||
		fail "$isolation_schema source Jobs did not preserve registry/database credential isolation"
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
	k -n "$TEST_NAMESPACE" get ptahschema "$sync_schema" -o json |
		jq -e \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .status.executionBinding as $binding |
      .status.applied as $applied |
      ($binding.epoch | test("^v1-[0-9a-f]{32}$")) and
      $binding.controllerImage == $controllerImage and
      $binding.controllerRevision == $controllerRevision and
      $binding.controllerStateVersion == $controllerStateVersion and
      $applied.executionBindingID == $binding.epoch and
      $applied.controllerImage == $controllerImage and
      $applied.controllerRevision == $controllerRevision and
      $applied.controllerStateVersion == $controllerStateVersion
    ' >/dev/null || fail "$sync_schema applied evidence is not bound to the exact controller identity"
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

capture_blocked_refresh_boundary() {
	blocked_schema=$1
	blocked_generation_checkpoint=$2
	blocked_capture_headroom=$(((BLOCKED_REFRESH_SECONDS * 2 + 2) / 3))
	blocked_post_checkpoint_headroom=$((BLOCKED_REFRESH_SECONDS / 2))
	[ "$blocked_post_checkpoint_headroom" -gt 0 ] ||
		fail "blocked refresh boundary requires positive post-checkpoint headroom"

	resume_schema_after_tag_move "$blocked_schema"
	blocked_generation_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$blocked_schema" -o json)
	blocked_expected_generation=$(printf '%s\n' "$blocked_generation_object" |
		jq -er '.metadata.generation')
	printf '%s\n' "$blocked_expected_generation" | grep -Eq '^[1-9][0-9]*$' ||
		fail "$blocked_schema resumed without an exact positive generation"

	blocked_state_deadline=$(deadline_from_now)
	blocked_persisted_deadline=
	while [ "$(date +%s)" -lt "$blocked_state_deadline" ]; do
		if blocked_candidate=$(k -n "$TEST_NAMESPACE" get ptahschema "$blocked_schema" \
			-o json 2>/dev/null); then
			blocked_now=$(date +%s)
			if printf '%s\n' "$blocked_candidate" | jq -e \
				--argjson generation "$blocked_expected_generation" \
				--arg interval "$BLOCKED_REFRESH_INTERVAL" \
				--argjson now "$blocked_now" \
				--argjson headroom "$blocked_capture_headroom" '
              .metadata.generation == $generation and
              .status.observedGeneration == $generation and
              .spec.suspend == false and .spec.interval == $interval and
              .status.phase == "Blocked" and .status.activeOperation == null and
              .status.pendingObservation == null and
              .status.pendingLockRelease == null and
              .status.nextReconciliationTime != null and
              ((.status.nextReconciliationTime | fromdateiso8601) - $now >= $headroom) and
              (.status.conditions // [] | any(
                .type == "ApprovalRequired" and .status == "False" and
                .reason == "DestructiveChangesDisabled"))
            ' >/dev/null; then
				blocked_persisted_deadline=$(printf '%s\n' "$blocked_candidate" |
					jq -er '.status.nextReconciliationTime')
				break
			fi
			blocked_phase=$(printf '%s\n' "$blocked_candidate" |
				jq -r '.status.phase // ""')
			[ "$blocked_phase" != Failed ] ||
				fail "$blocked_schema entered Failed before the blocked refresh boundary"
		fi
		sleep 1
	done
	[ -n "$blocked_persisted_deadline" ] ||
		fail "timed out waiting for $blocked_schema blocked refresh boundary with future headroom"

	BLOCKED_GATE_CHECKPOINT="$WORK_DIR/${blocked_schema}-blocked-generation-after.json"
	checkpoint_schema_jobs "$blocked_schema" "$BLOCKED_GATE_CHECKPOINT"
	for blocked_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$blocked_schema" "$blocked_operation" \
			"$blocked_generation_checkpoint" "$BLOCKED_GATE_CHECKPOINT"
	done
	assert_no_job_between_checkpoints "$blocked_schema" apply \
		"$blocked_generation_checkpoint" "$BLOCKED_GATE_CHECKPOINT"

	blocked_stable_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$blocked_schema" -o json)
	blocked_stable_now=$(date +%s)
	printf '%s\n' "$blocked_stable_object" | jq -e \
		--argjson generation "$blocked_expected_generation" \
		--arg interval "$BLOCKED_REFRESH_INTERVAL" \
		--arg deadline "$blocked_persisted_deadline" \
		--argjson now "$blocked_stable_now" \
		--argjson headroom "$blocked_post_checkpoint_headroom" '
          .metadata.generation == $generation and
          .status.observedGeneration == $generation and
          .spec.suspend == false and .spec.interval == $interval and
          .status.phase == "Blocked" and .status.activeOperation == null and
          .status.pendingObservation == null and
          .status.pendingLockRelease == null and
          .status.nextReconciliationTime == $deadline and
          ((.status.nextReconciliationTime | fromdateiso8601) - $now >= $headroom) and
          (.status.conditions // [] | any(
            .type == "ApprovalRequired" and .status == "False" and
            .reason == "DestructiveChangesDisabled"))
        ' >/dev/null ||
		fail "$blocked_schema crossed or changed its persisted blocked refresh boundary during capture"
}

prepare_blocked_refresh_cadence() {
	blocked_schema=$1
	suspend_schema_for_tag_move "$blocked_schema" "$BLOCKED_REFRESH_INTERVAL"
	blocked_generation_checkpoint="$WORK_DIR/${blocked_schema}-blocked-generation-before.json"
	checkpoint_schema_jobs "$blocked_schema" "$blocked_generation_checkpoint"
	capture_blocked_refresh_boundary "$blocked_schema" "$blocked_generation_checkpoint"
	audit_runtime_credentials
}

report_blocked_refresh_diagnostics() {
	diagnostic_schema=$1
	diagnostic_checkpoint=$2
	: >"$BLOCKED_REFRESH_DIAGNOSTIC_FILE"
	chmod 600 "$BLOCKED_REFRESH_DIAGNOSTIC_FILE" || {
		suppress_cleanup_diagnostics
		return 0
	}
	if [ -s "$OBSERVED_JOBS_FILE" ]; then
		if jq -c -s \
			--slurpfile before "$diagnostic_checkpoint" \
			--arg schema "$diagnostic_schema" '
              def safe_operation:
                if . == "resolve" or . == "verify" or . == "observe" or
                    . == "plan" or . == "apply" then . else null end;
              def operation_rank:
                if .operation == "resolve" then 0
                elif .operation == "verify" then 1
                elif .operation == "observe" then 2
                elif .operation == "plan" then 3
                else 4 end;
              {
                kind: "ObservedJobTimelineDiagnostic",
                jobs: [
                  .[] |
                  select(.schema == $schema) |
                  .uid as $uid |
                  select(($before[0] | index($uid)) == null) |
                  {name, uid, operation: (.operation | safe_operation), created}
                ] | sort_by([.created, operation_rank, .uid])
              }
			' "$OBSERVED_JOBS_FILE" >"$BLOCKED_REFRESH_DIAGNOSTIC_FILE" 2>/dev/null; then
			emit_scanned_cleanup_diagnostic "$BLOCKED_REFRESH_DIAGNOSTIC_FILE" \
				"$CREDENTIAL_PATTERNS_FILE"
		else
			suppress_cleanup_diagnostics
		fi
	else
		suppress_cleanup_diagnostics
	fi
	return 0
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
		if [ "$gate_resolve_count" -gt 3 ] || [ "$gate_verify_count" -gt 3 ] || \
			[ "$gate_observe_count" -gt 3 ] || [ "$gate_plan_count" -gt 3 ]; then
			report_blocked_refresh_diagnostics "$gate_schema" "$gate_refresh_checkpoint"
			fail "$gate_schema created work beyond three exact blocked refresh chains"
		fi
		if [ "$gate_resolve_count" -eq 3 ] && [ "$gate_verify_count" -eq 3 ] && \
			[ "$gate_observe_count" -eq 3 ] && [ "$gate_plan_count" -eq 3 ] && \
			all_new_jobs_complete "$gate_schema" resolve "$gate_refresh_checkpoint" 3 && \
			all_new_jobs_complete "$gate_schema" verify "$gate_refresh_checkpoint" 3 && \
			all_new_jobs_complete "$gate_schema" observe "$gate_refresh_checkpoint" 3 && \
			all_new_jobs_complete "$gate_schema" plan "$gate_refresh_checkpoint" 3; then
			record_observed_jobs
			if jq -e -s \
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
                select(new_since($before))
              ] | unique_by(.uid) | sort_by([.created, operation_rank]) as $new |
              [$new[] | select(.operation == "resolve")] as $resolves |
              [$new[] | select(.operation == "verify")] as $verifies |
              [$new[] | select(.operation == "observe")] as $observes |
              [$new[] | select(.operation == "plan")] as $plans |
              ($new | length) == 12 and
              ($resolves | length) == 3 and ($verifies | length) == 3 and
              ($observes | length) == 3 and ($plans | length) == 3 and
              (($resolves[1].created | fromdateiso8601) -
                ($resolves[0].created | fromdateiso8601) >= $intervalSeconds) and
              (($resolves[2].created | fromdateiso8601) -
                ($resolves[1].created | fromdateiso8601) >= $intervalSeconds) and
              ($new | map(.operation)) ==
                ["resolve", "verify", "observe", "plan",
                 "resolve", "verify", "observe", "plan",
                 "resolve", "verify", "observe", "plan"]
			' "$OBSERVED_JOBS_FILE" >/dev/null; then
				:
			else
				report_blocked_refresh_diagnostics "$gate_schema" "$gate_refresh_checkpoint"
				fail "$gate_schema did not preserve ordered interval-spaced blocked refresh cycles"
			fi
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
			gate_after_checkpoint="$WORK_DIR/${gate_schema}-blocked-refresh-after.json"
			checkpoint_schema_jobs "$gate_schema" "$gate_after_checkpoint"
			for gate_operation in resolve verify observe plan; do
				gate_final_count=$(job_count_between_checkpoints "$gate_schema" \
					"$gate_operation" "$gate_refresh_checkpoint" "$gate_after_checkpoint")
				if [ "$gate_final_count" -ne 3 ]; then
					report_blocked_refresh_diagnostics "$gate_schema" "$gate_refresh_checkpoint"
					fail "$gate_schema crossed the exact three-chain success boundary with $gate_final_count $gate_operation Jobs"
				fi
			done
			gate_final_apply_count=$(job_count_between_checkpoints "$gate_schema" apply \
				"$gate_refresh_checkpoint" "$gate_after_checkpoint")
			[ "$gate_final_apply_count" -eq 0 ] || {
				report_blocked_refresh_diagnostics "$gate_schema" "$gate_refresh_checkpoint"
				fail "$gate_schema crossed the exact three-chain success boundary with an Apply Job"
			}
			gate_final_schema_count=$(schema_job_count_between_checkpoints "$gate_schema" \
				"$gate_refresh_checkpoint" "$gate_after_checkpoint")
			[ "$gate_final_schema_count" -eq 12 ] || {
				report_blocked_refresh_diagnostics "$gate_schema" "$gate_refresh_checkpoint"
				fail "$gate_schema crossed the exact three-chain success boundary with $gate_final_schema_count total Jobs"
			}
			return 0
		fi
		sleep 2
	done
	report_blocked_refresh_diagnostics "$gate_schema" "$gate_refresh_checkpoint"
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

assert_requested_digest_pin_refusal() {
	digest_pin_reference=$1
	digest_pin_digest=$2
	digest_pin_engine=$3
	digest_pin_secret=$4
	digest_pin_schema=e2e-digest-pin-refusal
	digest_pin_coordination_key=e2e/digest-pin/refusal
	digest_pin_repository=${digest_pin_reference%:*}
	digest_pin_resolved="${digest_pin_repository}@${digest_pin_digest}"
	digest_pin_before="$WORK_DIR/${digest_pin_schema}-before.json"
	digest_pin_after="$WORK_DIR/${digest_pin_schema}-after.json"
	digest_pin_resolve_result="$WORK_DIR/${digest_pin_schema}-resolve-result.json"
	digest_pin_result="$WORK_DIR/${digest_pin_schema}-verify-result.json"
	digest_pin_policy_digest="sha256:$(sha256 <"$ROOT_DIR/testdata/e2e/verification-policy-digest-pin.yaml")"

	printf '%s\n' 'e2e data plane: checking requested-reference digest-pin enforcement'
	checkpoint_schema_jobs "$digest_pin_schema" "$digest_pin_before"
	create_schema_resource "$digest_pin_schema" "$digest_pin_engine" "$digest_pin_secret" \
		"$digest_pin_reference" "$digest_pin_coordination_key" "$DIGEST_PIN_POLICY_NAME" \
		"$DIGEST_PIN_DOCKER_AUTH_SECRET" DockerConfigJSON
	wait_for_schema "$digest_pin_schema" '
      .status.phase == "Blocked" and .status.activeOperation == null and
      .status.pendingObservation == null and .status.pendingLockRelease == null and
      (.status.conditions | any(
        .type == "ArtifactVerified" and .status == "False" and
        .reason == "PolicyRefused" and (.message | contains("require_digest_pin"))))
    ' "a mutable requested reference to be refused by the digest-pin policy"
	checkpoint_schema_jobs "$digest_pin_schema" "$digest_pin_after"
	assert_one_job_between_checkpoints "$digest_pin_schema" resolve \
		"$digest_pin_before" "$digest_pin_after"
	assert_one_job_between_checkpoints "$digest_pin_schema" verify \
		"$digest_pin_before" "$digest_pin_after"
	for digest_pin_database_operation in observe plan apply; do
		assert_no_job_between_checkpoints "$digest_pin_schema" \
			"$digest_pin_database_operation" "$digest_pin_before" "$digest_pin_after"
	done
	capture_one_new_job_result "$digest_pin_schema" resolve "$digest_pin_before" \
		"$digest_pin_resolve_result" "$digest_pin_after"
	jq -e \
		--arg resolved "$digest_pin_resolved" \
		--arg digest "$digest_pin_digest" '
          .childExitCode == 0 and .stdout == "" and .error == null and
          .resolvedDigest == $digest and .resolvedReference == $resolved and
          (.mutationStarted // false) == false and
          (.uncertain // false) == false and .truncation == null
        ' "$digest_pin_resolve_result" >/dev/null ||
		fail "$digest_pin_schema did not complete native Resolve through DockerConfigJSON access"

	k -n "$TEST_NAMESPACE" get ptahschema "$digest_pin_schema" -o json |
		jq -e \
			--arg requested "$digest_pin_reference" \
			--arg resolved "$digest_pin_resolved" \
			--arg digest "$digest_pin_digest" \
			--arg policyDigest "$digest_pin_policy_digest" '
          .status.phase == "Blocked" and
          .status.source.requestedReference == $requested and
          .status.source.resolvedReference == $resolved and
          .status.source.digest == $digest and
          (.status.source.mediaType | type) == "string" and
          (.status.source.mediaType | length) > 0 and
          .status.source.size > 0 and
          (.status.source.artifactType // "") == "" and
          (.status.source.verified // false) == false and
          .status.source.verificationPolicyDigest == $policyDigest and
          .status.plan == null and .status.activeOperation == null and
          .status.pendingObservation == null and .status.pendingLockRelease == null and
          (.status.conditions | any(
            .type == "ArtifactVerified" and .status == "False" and
            .reason == "PolicyRefused" and (.message | contains("require_digest_pin"))))
        ' >/dev/null ||
		fail "$digest_pin_schema lost immutable source evidence or reached database work"

	capture_one_new_job_result "$digest_pin_schema" verify "$digest_pin_before" \
		"$digest_pin_result" "$digest_pin_after"
	jq -e \
		--arg digest "$digest_pin_digest" \
		--arg policyDigest "$digest_pin_policy_digest" '
          .childExitCode == 0 and .stdout == "" and
          .resolvedDigest == $digest and
          .verificationPolicyDigest == $policyDigest and
          .verificationRequirements == ["require_digest_pin"] and
          .error.code == "verification_refused" and
          (.observedArtifactType // "") == "" and
          (.resolvedReference // "") == "" and
          (.resolvedMediaType // "") == "" and
          (.resolvedSize // 0) == 0 and
          (.mutationStarted // false) == false and
          (.uncertain // false) == false and .truncation == null
        ' "$digest_pin_result" >/dev/null ||
		fail "$digest_pin_schema did not preserve the exact runner-enforced refusal contract"
	assert_source_job_isolation "$digest_pin_schema" "$digest_pin_secret" \
		"$DIGEST_PIN_DOCKER_AUTH_SECRET" DockerConfigJSON "$DIGEST_PIN_POLICY_NAME" \
		"$digest_pin_reference" "$digest_pin_resolved"

	k -n "$TEST_NAMESPACE" patch ptahschema "$digest_pin_schema" --type=merge \
		--patch '{"spec":{"suspend":true}}' >/dev/null
	wait_for_schema "$digest_pin_schema" \
		'.status.phase == "Suspended" and .status.activeOperation == null' \
		"the digest-pin refusal fixture to suspend before its refresh interval"
}

snapshot_registry_outage_evidence() {
	outage_schema=$1
	outage_plan_name=$2
	outage_output=$3
	k -n "$TEST_NAMESPACE" get ptahschema "$outage_schema" -o json \
		>"$REGISTRY_OUTAGE_SCHEMA_INPUT"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$outage_plan_name" -o json \
		>"$REGISTRY_OUTAGE_PLAN_INPUT"
	chmod 600 "$REGISTRY_OUTAGE_SCHEMA_INPUT" "$REGISTRY_OUTAGE_PLAN_INPUT"
	scan_file_for_credentials "$REGISTRY_OUTAGE_SCHEMA_INPUT" \
		"registry-outage schema evidence input"
	scan_file_for_credentials "$REGISTRY_OUTAGE_PLAN_INPUT" \
		"registry-outage plan evidence input"
	jq -n \
		--slurpfile schema "$REGISTRY_OUTAGE_SCHEMA_INPUT" \
		--slurpfile plan "$REGISTRY_OUTAGE_PLAN_INPUT" '
          {
            executionBinding: $schema[0].status.executionBinding,
            source: $schema[0].status.source,
            target: $schema[0].status.target,
            plan: {
              current: $schema[0].status.plan,
              resource: {
                name: $plan[0].metadata.name,
                uid: $plan[0].metadata.uid,
                generation: $plan[0].metadata.generation,
                creationTimestamp: $plan[0].metadata.creationTimestamp,
                spec: $plan[0].spec,
                status: $plan[0].status
              }
            },
            applied: $schema[0].status.applied,
            lastSuccessfulReconciliation: $schema[0].status.lastSuccessfulReconciliation
          }
        ' >"$outage_output"
	: >"$REGISTRY_OUTAGE_SCHEMA_INPUT"
	: >"$REGISTRY_OUTAGE_PLAN_INPUT"
	chmod 600 "$outage_output"
	scan_file_for_credentials "$outage_output" "registry-outage retained evidence"
}

wait_for_registry_refresh_failure() {
	outage_schema=$1
	outage_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$outage_deadline" ]; do
		audit_completed_jobs
		if outage_object=$(k -n "$TEST_NAMESPACE" get ptahschema "$outage_schema" -o json 2>/dev/null); then
			if printf '%s\n' "$outage_object" | jq -e '
              .status.phase == "Failed" and
              .status.activeOperation.type == "Resolve" and
              .status.activeOperation.attempt == 2 and
              .status.activeOperation.jobUID == null and
              .status.nextReconciliationTime != null and
              (.status.conditions | any(
                .type == "ArtifactResolved" and .status == "Unknown" and
                .reason == "RefreshFailed")) and
              (.status.conditions | any(
                .type == "ArtifactVerified" and .status == "True" and
                .reason == "PolicySatisfied")) and
              (.status.conditions | any(
                .type == "PlanReady" and .status == "Unknown" and
                .reason == "SourceFreshnessUnknown")) and
              (.status.conditions | any(
                .type == "InSync" and .status == "Unknown" and
                .reason == "SourceFreshnessUnknown")) and
              (.status.conditions | any(
                .type == "Ready" and .status == "False" and
                .reason == "OperationFailed")) and
              (.status.conditions | any(
                .type == "ReconciliationFailed" and .status == "True" and
                .reason == "OperationFailed"))
            ' >/dev/null; then
				return 0
			fi
		fi
		sleep 1
	done
	fail "timed out waiting for one failed registry refresh with unknown freshness"
}

assert_registry_outage_and_recovery() {
	outage_schema=$1
	outage_digest=$2
	outage_plan_name=$3
	outage_plan_uid=$4
	outage_plan_fingerprint=$5
	[ "$RBAC_PAUSED" -eq 1 ] ||
		fail "registry outage proof requires the periodic no-op checkpoint barrier"
	k -n "$TEST_NAMESPACE" get ptahschema "$outage_schema" -o json |
		jq -e \
			--arg digest "$outage_digest" \
			--arg fingerprint "$outage_plan_fingerprint" \
			--arg version "$PTAH_VERSION" \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
          .status.executionBinding as $binding |
          .status.applied as $applied |
          .spec.execution.failureRetryInterval == "45s" and
          .status.phase == "InSync" and .status.source.digest == $digest and
          .status.activeOperation == null and .status.pendingObservation == null and
          .status.pendingLockRelease == null and .status.plan == null and
          .status.applied.artifactDigest == $digest and
          .status.applied.planFingerprint == $fingerprint and
          .status.applied.ptahVersion == $version and
          ($binding.epoch | test("^v1-[0-9a-f]{32}$")) and
          $binding.controllerImage == $controllerImage and
          $binding.controllerRevision == $controllerRevision and
          $binding.controllerStateVersion == $controllerStateVersion and
          $applied.executionBindingID == $binding.epoch and
          $applied.controllerImage == $controllerImage and
          $applied.controllerRevision == $controllerRevision and
          $applied.controllerStateVersion == $controllerStateVersion and
          .status.lastSuccessfulReconciliation != null and
          (.status.conditions | any(
            .type == "ArtifactResolved" and .status == "True" and .reason == "DigestPinned")) and
          (.status.conditions | any(
            .type == "ArtifactVerified" and .status == "True" and .reason == "PolicySatisfied")) and
          (.status.conditions | any(
            .type == "InSync" and .status == "True" and .reason == "ScopedConverged"))
		' >/dev/null || fail "$outage_schema lacks a fresh successful baseline before registry outage"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$outage_plan_name" -o json |
		jq -e \
			--arg uid "$outage_plan_uid" \
			--arg fingerprint "$outage_plan_fingerprint" \
			--arg digest "$outage_digest" \
			--arg version "$PTAH_VERSION" \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
          .metadata.uid == $uid and .spec.fingerprint == $fingerprint and
          .spec.artifactDigest == $digest and .spec.ptahVersion == $version and
          .spec.contractVersion == 3 and
          (.spec.executionBindingID | test("^v1-[0-9a-f]{32}$")) and
          .spec.controllerImage == $controllerImage and
          .spec.controllerRevision == $controllerRevision and
          .spec.controllerStateVersion == $controllerStateVersion and
          (.status.conditions | any(.type == "Ready" and .status == "True"))
        ' >/dev/null || fail "$outage_schema lacks its exact durable applied Plan before outage"
	snapshot_registry_outage_evidence "$outage_schema" "$outage_plan_name" \
		"$REGISTRY_OUTAGE_EVIDENCE_BEFORE"
	outage_before="$WORK_DIR/${outage_schema}-registry-outage-before.json"
	checkpoint_schema_jobs "$outage_schema" "$outage_before"
	assert_registry_container_contract true
	docker --context "$DOCKER_CONTEXT" stop --time=10 "$REGISTRY_CONTAINER_ID" >/dev/null
	assert_registry_container_contract false
	resume_controller_status_writes || fail "could not release the registry-outage timer barrier"
	wait_for_one_new_job "$outage_schema" resolve "$outage_before"
	wait_for_registry_refresh_failure "$outage_schema"
	pause_controller_status_writes
	outage_failed_after="$WORK_DIR/${outage_schema}-registry-outage-failed.json"
	checkpoint_schema_jobs "$outage_schema" "$outage_failed_after"
	assert_one_job_between_checkpoints "$outage_schema" resolve \
		"$outage_before" "$outage_failed_after"
	for outage_forbidden_operation in verify observe plan apply; do
		assert_no_job_between_checkpoints "$outage_schema" "$outage_forbidden_operation" \
			"$outage_before" "$outage_failed_after"
	done
	outage_failed_total=$(schema_job_count_between_checkpoints "$outage_schema" \
		"$outage_before" "$outage_failed_after")
	[ "$outage_failed_total" -eq 1 ] ||
		fail "$outage_schema created $outage_failed_total Jobs during its one-failure outage boundary"
	outage_result="$WORK_DIR/${outage_schema}-registry-outage-result.json"
	capture_one_new_job_result "$outage_schema" resolve "$outage_before" \
		"$outage_result" "$outage_failed_after"
	jq -e '
      .childExitCode != 0 and .error != null and .stdout == "" and
      (.mutationStarted // false) == false and (.uncertain // false) == false and
      .truncation == null
    ' "$outage_result" >/dev/null ||
		fail "$outage_schema registry outage did not produce one exact read-only Resolve failure"
	snapshot_registry_outage_evidence "$outage_schema" "$outage_plan_name" \
		"$REGISTRY_OUTAGE_EVIDENCE_AFTER"
	cmp -s "$REGISTRY_OUTAGE_EVIDENCE_BEFORE" "$REGISTRY_OUTAGE_EVIDENCE_AFTER" ||
		fail "$outage_schema registry outage changed retained execution, source, target, plan, applied, or success evidence"

	recovery_before="$WORK_DIR/${outage_schema}-registry-recovery-before.json"
	checkpoint_schema_jobs "$outage_schema" "$recovery_before"
	docker --context "$DOCKER_CONTEXT" start "$REGISTRY_CONTAINER_ID" >/dev/null
	assert_registry_container_contract true
	wait_for_registry_http_ready
	assert_registry_container_contract true
	resume_controller_status_writes || fail "could not release the registry-recovery timer barrier"
	wait_for_one_new_job "$outage_schema" resolve "$recovery_before"
	wait_for_one_new_job "$outage_schema" verify "$recovery_before"
	wait_for_one_new_job "$outage_schema" observe "$recovery_before"
	wait_for_one_new_job "$outage_schema" plan "$recovery_before"
	wait_for_schema "$outage_schema" \
		".status.phase == \"InSync\" and .status.source.digest == \"$outage_digest\" and .status.activeOperation == null and .status.plan == null" \
		"same-digest registry recovery to converge without Apply"
	recovery_after="$WORK_DIR/${outage_schema}-registry-recovery-after.json"
	checkpoint_schema_jobs "$outage_schema" "$recovery_after"
	for recovery_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$outage_schema" "$recovery_operation" \
			"$recovery_before" "$recovery_after"
	done
	assert_no_job_between_checkpoints "$outage_schema" apply \
		"$recovery_before" "$recovery_after"
	assert_read_only_chain_between_checkpoints "$outage_schema" \
		"$recovery_before" "$recovery_after"
	assert_convergence_result_pair "$outage_schema" "$recovery_before" \
		"$recovery_before" "$recovery_after"
	k -n "$TEST_NAMESPACE" get ptahschema "$outage_schema" -o json |
		jq -e \
			--arg digest "$outage_digest" \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" \
			--slurpfile retained "$REGISTRY_OUTAGE_EVIDENCE_BEFORE" '
          .status.phase == "InSync" and .status.source.digest == $digest and
          .status.executionBinding == $retained[0].executionBinding and
          .status.applied == $retained[0].applied and
          .status.executionBinding.controllerImage == $controllerImage and
          .status.executionBinding.controllerRevision == $controllerRevision and
          .status.executionBinding.controllerStateVersion == $controllerStateVersion and
          .status.applied.controllerImage == $controllerImage and
          .status.applied.controllerRevision == $controllerRevision and
          .status.applied.controllerStateVersion == $controllerStateVersion and
          .status.activeOperation == null and .status.pendingObservation == null and
          .status.pendingLockRelease == null and .status.plan == null and
          (.status.conditions | any(
            .type == "ArtifactResolved" and .status == "True" and .reason == "DigestPinned")) and
          (.status.conditions | any(
            .type == "ArtifactVerified" and .status == "True" and .reason == "PolicySatisfied")) and
          (.status.conditions | any(
            .type == "PlanReady" and .status == "False" and .reason == "NoChanges")) and
          (.status.conditions | any(
            .type == "InSync" and .status == "True" and .reason == "ScopedConverged")) and
          (.status.conditions | any(
            .type == "Ready" and .status == "True" and .reason == "InSync")) and
          (.status.conditions | any(
            .type == "ReconciliationFailed" and .status == "False" and .reason == "Succeeded"))
		' >/dev/null || fail "$outage_schema did not restore exact fresh no-op conditions"
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$outage_plan_name" -o json |
		jq -e \
			--arg uid "$outage_plan_uid" \
			--arg fingerprint "$outage_plan_fingerprint" \
			--arg digest "$outage_digest" '
          .metadata.uid == $uid and .spec.fingerprint == $fingerprint and
          .spec.artifactDigest == $digest and
          (.status.conditions | any(.type == "Ready" and .status == "True"))
        ' >/dev/null || fail "$outage_schema did not retain its exact durable Plan through recovery"
	audit_runtime_credentials
	printf '%s\n' 'e2e data plane: PASS registry outage freshness and exact recovery'
}

wait_for_manager_removed() {
	manager_removal_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$manager_removal_deadline" ]; do
		if ! k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" \
			>/dev/null 2>&1 &&
			! k -n "$OPERATOR_NAMESPACE" get pods \
				-l 'app.kubernetes.io/component=controller' -o name | grep -q .; then
			return 0
		fi
		sleep 1
	done
	fail "manager Deployment was not removed for execution-binding fault injection"
}

upgrade_execution_binding_before_apply() {
	upgrade_schema=$1
	upgrade_reference=$2
	upgrade_digest=$3
	upgrade_dialect=$4
	upgrade_coordination_key=$5
	upgrade_coordination_digest=$6
	upgrade_old_plan=$CURRENT_PLAN
	upgrade_old_plan_uid=$CURRENT_PLAN_UID
	upgrade_old_fingerprint=$CURRENT_PLAN_FINGERPRINT
	upgrade_original_ptah_version=$PTAH_VERSION
	upgrade_old_approval="${upgrade_schema}-old-binding"
	upgrade_before="$WORK_DIR/${upgrade_schema}-binding-upgrade-before.json"
	checkpoint_schema_jobs "$upgrade_schema" "$upgrade_before"
	k -n "$TEST_NAMESPACE" get ptahschema "$upgrade_schema" -o json |
		jq -e --arg planUID "$upgrade_old_plan_uid" --arg version "$PTAH_VERSION" '
          .status.phase == "AwaitingApproval" and
          .status.plan.uid == $planUID and .status.plan.approval == null and
          .status.plan.ptahVersion == $version and
          .status.nextReconciliationTime != null and
          ((.status.nextReconciliationTime | fromdateiso8601) - now) >= 180
        ' >/dev/null || fail "$upgrade_schema lacks a quiescent old-binding approval window"

	pause_controller_status_writes
	create_exact_approval "$upgrade_schema" "$upgrade_old_plan" "$upgrade_old_approval" \
		"$upgrade_coordination_key" "$upgrade_coordination_digest"
	k -n "$TEST_NAMESPACE" get ptahschemaapproval "$upgrade_old_approval" -o json |
		jq -e \
			--arg version "$upgrade_original_ptah_version" \
			--arg executor "$EXECUTOR_IMAGE" \
			--arg runner "$RUNNER_IMAGE" \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
          .spec.ptahVersion == $version and
          .spec.executorImage == $executor and .spec.runnerImage == $runner and
          .spec.runnerProtocolVersion == 4 and
          (.spec.executionBindingID | test("^v1-[0-9a-f]{32}$")) and
          .spec.controllerImage == $controllerImage and
          .spec.controllerRevision == $controllerRevision and
          .spec.controllerStateVersion == $controllerStateVersion
        ' >/dev/null || fail "old approval was not bound to the pre-upgrade execution identity"
	sleep 2
	audit_completed_jobs
	assert_no_new_jobs "$upgrade_schema" apply "$upgrade_before"
	upgrade_approval_object=$(k -n "$TEST_NAMESPACE" get ptahschemaapproval \
		"$upgrade_old_approval" -o json)
	upgrade_recorded_approval=$(printf '%s\n' "$upgrade_approval_object" | jq -c '
      {
        name: .metadata.name,
        uid: .metadata.uid,
        approver: .spec.approver,
        approvedAt: .spec.approvedAt
      }
    ')
	printf '%s\n' "$upgrade_recorded_approval" | jq -e '
      .name != "" and .uid != "" and .approver.username != "" and .approvedAt != null
    ' >/dev/null || fail "old-binding approval lacks an injectable exact identity"

	k -n "$OPERATOR_NAMESPACE" delete deployment "$CONTROLLER_NAME" \
		--cascade=foreground --wait=true >/dev/null
	wait_for_manager_removed
	upgrade_status_patch=$(jq -nc --argjson approval "$upgrade_recorded_approval" \
		'{status: {plan: {approval: $approval}}}')
	k -n "$TEST_NAMESPACE" patch ptahschema "$upgrade_schema" --subresource=status \
		--type=merge -p "$upgrade_status_patch" >/dev/null
	k -n "$TEST_NAMESPACE" get ptahschema "$upgrade_schema" -o json |
		jq -e \
			--arg planUID "$upgrade_old_plan_uid" \
			--argjson approval "$upgrade_recorded_approval" '
          .status.plan.uid == $planUID and .status.plan.approval == $approval and
          .status.activeOperation == null
        ' >/dev/null || fail "old-binding approval was not durably recorded before upgrade"

	UPGRADED_PTAH_VERSION="e2e-binding-$(printf '%s' "$PTAH_VERSION" | sha256 | cut -c1-16)"
	[ "$UPGRADED_PTAH_VERSION" != "$PTAH_VERSION" ] ||
		fail "execution-binding upgrade did not select a distinct Ptah version"
	printf 'e2e data plane: upgrading manager execution binding from %s to %s\n' \
		"$PTAH_VERSION" "$UPGRADED_PTAH_VERSION"
	helm --kubeconfig "$KUBECONFIG_FILE" upgrade "$HELM_RELEASE" "$CHART_PACKAGE" \
		--namespace "$OPERATOR_NAMESPACE" --reuse-values --wait --timeout 5m \
		--set-string execution.ptahVersion="$UPGRADED_PTAH_VERSION" >/dev/null
	k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$CONTROLLER_NAME" \
		--timeout="${TIMEOUT_SECONDS}s" >/dev/null
	k -n "$OPERATOR_NAMESPACE" get deployment "$CONTROLLER_NAME" -o json |
		jq -e \
			--arg version "--ptah-version=${UPGRADED_PTAH_VERSION}" \
			--arg oldVersion "--ptah-version=${PTAH_VERSION}" \
			--arg executor "--executor-image=${EXECUTOR_IMAGE}" \
			--arg runner "--runner-image=${RUNNER_IMAGE}" \
			--arg controllerImage "--controller-image=${CONTROLLER_IMAGE}" '
          [.spec.template.spec.containers[] | select(.name == "manager")] as $manager |
          ($manager | length) == 1 and
          ($manager[0].args | index($version)) != null and
          ($manager[0].args | index($oldVersion)) == null and
          ($manager[0].args | index($executor)) != null and
          ($manager[0].args | index($runner)) != null and
          ($manager[0].args | index($controllerImage)) != null
        ' >/dev/null || fail "manager rollout did not change only the Ptah version binding"
	wait_for_controller_status_authorization yes ||
		fail "Helm upgrade did not restore exact controller status authorization"
	RBAC_PAUSED=0
	PTAH_VERSION=$UPGRADED_PTAH_VERSION

	# shellcheck disable=SC2016 # jq variable is supplied by wait_for_approval.
	wait_for_approval "$upgrade_old_approval" '
      .spec.planRef.uid == $expectedPlanUID and
      .spec.ptahVersion != "" and
      (.status.conditions | any(
        .type == "Accepted" and .status == "False" and
        .reason == "ExecutionBindingChanged")) and
      (.status.conditions | any(
        .type == "Stale" and .status == "True" and
        .reason == "ExecutionBindingChanged")) and
      (.status.conditions | all(.type != "Consumed" or .status != "True"))
    ' "the old execution-binding approval to become stale before Apply" \
		"$upgrade_old_plan_uid"
	upgrade_after="$WORK_DIR/${upgrade_schema}-binding-upgrade-after.json"
	assert_plan "$upgrade_schema" "$upgrade_reference" "$upgrade_digest" "$upgrade_dialect" false \
		"$upgrade_before" "$upgrade_before" "$upgrade_after"
	for upgrade_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$upgrade_schema" "$upgrade_operation" \
			"$upgrade_before" "$upgrade_after"
	done
	assert_no_job_between_checkpoints "$upgrade_schema" apply \
		"$upgrade_before" "$upgrade_after"
	assert_read_only_chain_between_checkpoints "$upgrade_schema" \
		"$upgrade_before" "$upgrade_after"
	[ "$CURRENT_PLAN" != "$upgrade_old_plan" ] ||
		fail "$upgrade_schema reused the old plan name after execution-binding upgrade"
	[ "$CURRENT_PLAN_UID" != "$upgrade_old_plan_uid" ] ||
		fail "$upgrade_schema reused the old plan UID after execution-binding upgrade"
	[ "$CURRENT_PLAN_FINGERPRINT" != "$upgrade_old_fingerprint" ] ||
		fail "$upgrade_schema reused the old fingerprint after execution-binding upgrade"
	k -n "$TEST_NAMESPACE" get ptahschemaplans "$upgrade_old_plan" "$CURRENT_PLAN" -o json |
		jq -e \
			--arg oldName "$upgrade_old_plan" \
			--arg newName "$CURRENT_PLAN" \
			--arg oldVersion "$upgrade_original_ptah_version" \
			--arg newVersion "$PTAH_VERSION" \
			--arg executor "$EXECUTOR_IMAGE" \
			--arg runner "$RUNNER_IMAGE" \
			--arg controllerImage "$CONTROLLER_IMAGE" \
			--arg controllerRevision "$CONTROLLER_REVISION" \
			--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
          def named($name):
            [.items[] | select(.metadata.name == $name)] |
            if length == 1 then .[0] else error("plan identity is not exact") end;
	          named($oldName) as $old | named($newName) as $new |
	          $old.spec.ptahVersion == $oldVersion and
	          $old.spec.ptahVersion != $newVersion and
          $new.spec.ptahVersion == $newVersion and
          $old.spec.executorImage == $executor and $new.spec.executorImage == $executor and
          $old.spec.runnerImage == $runner and $new.spec.runnerImage == $runner and
          $old.spec.runnerProtocolVersion == 4 and $new.spec.runnerProtocolVersion == 4 and
          ($old.spec.executionBindingID | test("^v1-[0-9a-f]{32}$")) and
          ($new.spec.executionBindingID | test("^v1-[0-9a-f]{32}$")) and
          $old.spec.executionBindingID != $new.spec.executionBindingID and
          $old.spec.controllerImage == $controllerImage and
          $new.spec.controllerImage == $controllerImage and
          $old.spec.controllerRevision == $controllerRevision and
          $new.spec.controllerRevision == $controllerRevision and
          $old.spec.controllerStateVersion == $controllerStateVersion and
          $new.spec.controllerStateVersion == $controllerStateVersion
        ' >/dev/null || fail "$upgrade_schema plans did not preserve exact old/new execution bindings"
	[ "$RBAC_PAUSED" -eq 1 ] ||
		fail "fresh binding plan lacks a status barrier before its new approval"
	printf '%s\n' 'e2e data plane: PASS pre-Apply execution-binding upgrade invalidation'
}

assert_external_postgresql_catalog() {
	assert_external_pg_not_hosted_in_kubernetes
	assert_external_pg_container_contract
	assert_external_pg_server_version
	external_columns=$(external_pg_query \
		"SELECT string_agg(column_name || ':' || data_type || ':' || is_nullable, ',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets'")
	external_columns=$(printf '%s' "$external_columns" | tr -d '\r\n')
	[ "$external_columns" = 'id:bigint:NO,name:text:NO' ] ||
		fail "external PostgreSQL columns are $external_columns, expected the exact v1 schema"
	external_primary_key=$(external_pg_query \
		"SELECT count(*) FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu USING (constraint_catalog, constraint_schema, constraint_name, table_catalog, table_schema, table_name) WHERE tc.table_schema='public' AND tc.table_name='e2e_widgets' AND tc.constraint_type='PRIMARY KEY' AND kcu.column_name='id' AND kcu.ordinal_position=1")
	external_primary_key=$(printf '%s' "$external_primary_key" | tr -d '[:space:]')
	[ "$external_primary_key" = 1 ] ||
		fail "external PostgreSQL v1 primary key is not exact"
	external_role_superuser=$(external_pg_query \
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user")
	external_role_superuser=$(printf '%s' "$external_role_superuser" | tr -d '[:space:]')
	[ "$external_role_superuser" = f ] ||
		fail "external PostgreSQL fixture login regained superuser authority"
	external_database_owner=$(external_pg_query \
		"SELECT pg_get_userbyid(datdba) = current_user FROM pg_database WHERE datname = current_database()")
	external_database_owner=$(printf '%s' "$external_database_owner" | tr -d '[:space:]')
	[ "$external_database_owner" = t ] ||
		fail "external PostgreSQL fixture login lost database ownership"
}

run_external_postgresql_lifecycle() {
	external_reference="oci://${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000/schemas/postgresql-external:stable"
	external_coordination_digest=$(coordination_digest postgresql "$EXTERNAL_PG_COORDINATION_KEY")
	external_before="$WORK_DIR/${EXTERNAL_PG_SCHEMA}-before.json"
	checkpoint_schema_jobs "$EXTERNAL_PG_SCHEMA" "$external_before"
	external_digest=$(publish_schema postgresql-external v1 postgres "$external_reference" \
		"$ROOT_DIR/testdata/e2e/postgresql-v1.sql")
	external_lease_before="$WORK_DIR/${EXTERNAL_PG_SCHEMA}-leases-before.json"
	checkpoint_coordination_leases "$external_lease_before"
	create_schema_resource "$EXTERNAL_PG_SCHEMA" PostgreSQL "$EXTERNAL_PG_SECRET" \
		"$external_reference" "$EXTERNAL_PG_COORDINATION_KEY" \
		e2e-verification-policy "$REGISTRY_AUTH_SECRET" Environment 45s "$QUIESCENT_INTERVAL"
	external_after_plan="$WORK_DIR/${EXTERNAL_PG_SCHEMA}-after-plan.json"
	assert_plan "$EXTERNAL_PG_SCHEMA" "$external_reference" "$external_digest" postgres false \
		"$external_before" "$external_before" "$external_after_plan"
	for external_plan_operation in resolve verify observe plan; do
		assert_one_job_between_checkpoints "$EXTERNAL_PG_SCHEMA" \
			"$external_plan_operation" "$external_before" "$external_after_plan"
	done
	assert_no_job_between_checkpoints "$EXTERNAL_PG_SCHEMA" apply \
		"$external_before" "$external_after_plan"
	assert_read_only_chain_between_checkpoints "$EXTERNAL_PG_SCHEMA" \
		"$external_before" "$external_after_plan"
	assert_coordination_boundary "$EXTERNAL_PG_SCHEMA" "$EXTERNAL_PG_COORDINATION_KEY" \
		"$external_coordination_digest"
	assert_job_isolation "$EXTERNAL_PG_SCHEMA" "$EXTERNAL_PG_SECRET" false
	external_plan=$CURRENT_PLAN
	external_plan_uid=$CURRENT_PLAN_UID
	external_apply_before="$WORK_DIR/${EXTERNAL_PG_SCHEMA}-apply-before.json"
	checkpoint_schema_jobs "$EXTERNAL_PG_SCHEMA" "$external_apply_before"
	create_exact_approval "$EXTERNAL_PG_SCHEMA" "$external_plan" \
		"${EXTERNAL_PG_SCHEMA}-v1" "$EXTERNAL_PG_COORDINATION_KEY" \
		"$external_coordination_digest"
	resume_controller_status_writes ||
		fail "could not release external PostgreSQL approval barrier"
	wait_for_one_new_job "$EXTERNAL_PG_SCHEMA" apply "$external_apply_before"
	wait_for_in_sync "$EXTERNAL_PG_SCHEMA" "$external_digest" \
		"$external_apply_before" "$external_apply_before"
	assert_approval_consumed "${EXTERNAL_PG_SCHEMA}-v1" "$external_plan_uid"
	assert_one_new_job "$EXTERNAL_PG_SCHEMA" apply "$external_apply_before"
	assert_coordination_lease_boundary "$EXTERNAL_PG_COORDINATION_KEY" \
		"$external_lease_before"
	assert_job_isolation "$EXTERNAL_PG_SCHEMA" "$EXTERNAL_PG_SECRET" true
	assert_external_postgresql_catalog
	k -n "$TEST_NAMESPACE" patch ptahschema "$EXTERNAL_PG_SCHEMA" --type=merge \
		-p '{"spec":{"suspend":true}}' >/dev/null
	wait_for_schema "$EXTERNAL_PG_SCHEMA" '
      .status.observedGeneration == .metadata.generation and
      .status.phase == "Suspended" and .status.activeOperation == null and
      .status.pendingObservation == null and .status.pendingLockRelease == null
    ' "external PostgreSQL acceptance to suspend after exact convergence"
	assert_external_postgresql_catalog
	audit_runtime_credentials
	printf '%s\n' 'e2e data plane: PASS external PostgreSQL bridge lifecycle'
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
	digest_v1=$(publish_schema "$lifecycle_slug" v1 "$lifecycle_dialect" "$lifecycle_reference")
	if [ "$lifecycle_slug" = postgresql ]; then
		[ "$CUSTOM_CA_COORDINATION_KEY" != "$lifecycle_coordination_key" ] ||
			fail "custom-CA acceptance cannot share the primary lifecycle coordination key"
		assert_authenticated_https_custom_ca "$digest_v1"
		assert_requested_digest_pin_refusal "$lifecycle_reference" "$digest_v1" \
			"$lifecycle_engine" "$lifecycle_secret"
	fi
	checkpoint_coordination_leases "$coordination_lease_checkpoint"
	if [ "$lifecycle_slug" = postgresql ]; then
		create_schema_resource "$lifecycle_schema" "$lifecycle_engine" "$lifecycle_secret" \
			"$lifecycle_reference" "$lifecycle_coordination_key" \
			e2e-verification-policy "$REGISTRY_AUTH_SECRET" Environment 45s "$QUIESCENT_INTERVAL"
	else
		create_schema_resource "$lifecycle_schema" "$lifecycle_engine" "$lifecycle_secret" \
			"$lifecycle_reference" "$lifecycle_coordination_key"
	fi
	assert_plan "$lifecycle_schema" "$lifecycle_reference" "$digest_v1" "$lifecycle_dialect" false \
		"$v1_observe_checkpoint" "$v1_plan_checkpoint"
	assert_coordination_boundary "$lifecycle_schema" "$lifecycle_coordination_key" \
		"$lifecycle_coordination_digest"
	plan_v1=$CURRENT_PLAN
	plan_v1_uid=$CURRENT_PLAN_UID
	plan_v1_fingerprint=$CURRENT_PLAN_FINGERPRINT
	assert_job_isolation "$lifecycle_schema" "$lifecycle_secret" false
	assert_no_new_jobs "$lifecycle_schema" apply "$v1_apply_checkpoint"
	if [ "$lifecycle_slug" = postgresql ]; then
		upgrade_execution_binding_before_apply "$lifecycle_schema" "$lifecycle_reference" \
			"$digest_v1" "$lifecycle_dialect" "$lifecycle_coordination_key" \
			"$lifecycle_coordination_digest"
		plan_v1=$CURRENT_PLAN
		plan_v1_uid=$CURRENT_PLAN_UID
		plan_v1_fingerprint=$CURRENT_PLAN_FINGERPRINT
	fi
	v1_post_observe_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-post-observe.json"
	v1_post_plan_checkpoint="$WORK_DIR/${lifecycle_schema}-v1-post-plan.json"
	checkpoint_jobs "$lifecycle_schema" observe "$v1_post_observe_checkpoint"
	checkpoint_jobs "$lifecycle_schema" plan "$v1_post_plan_checkpoint"
	create_exact_approval "$lifecycle_schema" "$plan_v1" "${lifecycle_schema}-v1" \
		"$lifecycle_coordination_key" "$lifecycle_coordination_digest"
	resume_controller_status_writes || fail "could not release the v1 approval barrier"
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
	if [ "$lifecycle_slug" = mysql ]; then
		assert_mysql_declared_element_order
	fi
	set_reconcile_interval_and_assert_noop "$lifecycle_schema" "$digest_v1" \
		"$RECONCILE_INTERVAL"
	assert_periodic_noop "$lifecycle_schema" "$PERIODIC_NOOP_CHECKPOINT"
	if [ "$lifecycle_slug" = postgresql ]; then
		assert_registry_outage_and_recovery "$lifecycle_schema" "$digest_v1" \
			"$plan_v1" "$plan_v1_uid" "$plan_v1_fingerprint"
	fi
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
create_authenticated_tls_proxy
k -n "$TEST_NAMESPACE" rollout status deployment/"$PG_SERVICE" --timeout="${TIMEOUT_SECONDS}s"
k -n "$TEST_NAMESPACE" rollout status deployment/"$MYSQL_SERVICE" --timeout="${TIMEOUT_SECONDS}s"
wait_for_database postgresql
wait_for_database mysql
create_external_postgresql_endpoint
create_custom_ca_database
report_database_versions
create_admission_fixtures
create_digest_pin_policy_fixture

run_engine_lifecycle postgresql PostgreSQL postgres "$PG_SECRET"
run_external_postgresql_lifecycle
run_engine_lifecycle mysql MySQL mysql "$MYSQL_SECRET"
run_mysql_dsn_refusal
[ "$EPHEMERAL_SUBRESOURCE_TESTED" -eq 1 ] ||
	fail "no UID-bound active operation Pod was available for the admission subresource and oldObject selector tests"
audit_runtime_credentials

printf '%s\n' 'e2e data plane: starting restart and fault-injection acceptance'
E2E_AUDITED_JOBS_FILE=$AUDITED_JOBS_FILE \
E2E_FULLY_AUDITED_JOBS_FILE=$FULLY_AUDITED_JOBS_FILE \
E2E_OBSERVED_JOBS_FILE=$OBSERVED_JOBS_FILE \
E2E_FIXTURE_IMAGE=$FIXTURE_IMAGE \
E2E_RESULT_ASSERT_BINARY=$RESULT_ASSERT_BINARY \
E2E_CONTROLLER_IMAGE=$CONTROLLER_IMAGE \
E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \
E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \
	"$ROOT_DIR/hack/e2e-faults.sh"
assert_mysql_destructive_refusal_durable
assert_external_postgresql_catalog
audit_runtime_credentials
assert_observed_jobs_audited

printf '%s\n' 'e2e data plane: PASS PostgreSQL, external PostgreSQL, MySQL, OCI, restart, and fault lifecycle'
