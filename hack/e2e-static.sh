#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-e2e-static.XXXXXX")
RENDERED_WEBHOOKS=$WORK_DIR/webhooks.yaml
ROTATOR_RENDER=$WORK_DIR/rotator.yaml
ROTATOR_RECREATE_RENDER=$WORK_DIR/rotator-recreate.yaml
OBSOLETE_RENDER=$WORK_DIR/obsolete-webhooks.yaml
OBSOLETE_ERROR=$WORK_DIR/obsolete-webhooks.err
MUTABLE_MANAGER_ERROR=$WORK_DIR/mutable-manager.err
LEADER_ELECTION_ERROR=$WORK_DIR/leader-election.err
DEFAULT_RBAC_RENDER=$WORK_DIR/default-rbac.yaml
SHARED_RBAC_RENDER=$WORK_DIR/shared-rbac.yaml
CONTROLLER_JOB_FIXTURE=$WORK_DIR/controller-jobs.json
PUBLISHER_JOB_FIXTURE=$WORK_DIR/publisher-job.json
NEGATIVE_FIXTURE=$WORK_DIR/negative-job.json

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
		"${TMPDIR:-/tmp}"/ptah-operator-e2e-static.*) rm -rf -- "$WORK_DIR" ;;
		*)
			printf 'e2e static: refusing to remove unexpected work directory %s\n' "$WORK_DIR" >&2
			status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

command -v helm >/dev/null 2>&1 || {
	printf '%s\n' 'e2e static: Helm is required for rendered webhook checks' >&2
	exit 1
}
command -v shellcheck >/dev/null 2>&1 || {
	printf '%s\n' 'e2e static: shellcheck is required' >&2
	exit 1
}

for script in "$ROOT_DIR"/hack/e2e-*.sh; do
	sh -n "$script"
done

shellcheck "$ROOT_DIR"/hack/e2e-*.sh

grep -F '__API_SERVER_PORT__' "$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" >/dev/null
grep -F 'application/vnd.stokaro.ptah.schema.v1' \
	"$ROOT_DIR/testdata/e2e/verification-policy.yaml" >/dev/null
if grep -F 'require_digest_pin: true' "$ROOT_DIR/testdata/e2e/verification-policy.yaml" >/dev/null; then
	printf '%s\n' 'e2e static: mutable-tag policy cannot require the requested reference to be pinned' >&2
	exit 1
fi
grep -F './cmd/ptah' "$ROOT_DIR/test/e2e/Dockerfile.ptah" >/dev/null
grep -Eq '^FROM .*@sha256:[0-9a-f]{64}' "$ROOT_DIR/test/e2e/Dockerfile.ptah"
grep -F './cmd/manager' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F './cmd/ptah-runner' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F './cmd/ptah-cert-rotator' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F './test/e2e/handcraftoci' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F 'e2e-fixture' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
[ "$(grep -Ec '^FROM .*@sha256:[0-9a-f]{64}' "$ROOT_DIR/test/e2e/Dockerfile.operator")" -eq 3 ]
operator_stage=$(sed -n '/ AS operator$/,/ AS fixture$/p' "$ROOT_DIR/test/e2e/Dockerfile.operator")
printf '%s\n' "$operator_stage" | grep -F 'COPY --from=builder /out/manager /manager' >/dev/null
printf '%s\n' "$operator_stage" |
	grep -F 'COPY --from=builder /out/ptah-cert-rotator /ptah-cert-rotator' >/dev/null
if printf '%s\n' "$operator_stage" | grep -F 'e2e-handcraft-oci' >/dev/null; then
	printf '%s\n' 'e2e static: controller image stage contains the test-only OCI publisher' >&2
	exit 1
fi
fixture_stage=$(sed -n '/ AS fixture$/,$p' "$ROOT_DIR/test/e2e/Dockerfile.operator")
printf '%s\n' "$fixture_stage" |
	grep -F 'COPY --from=builder /out/e2e-handcraft-oci /e2e-handcraft-oci' >/dev/null
if printf '%s\n' "$fixture_stage" | grep -Eq '/out/(manager|ptah-runner|ptah-cert-rotator)'; then
	printf '%s\n' 'e2e static: isolated fixture image stage contains an operator binary' >&2
	exit 1
fi
grep -F -- '--target operator' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F -- '--target fixture' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F 'the controller image contains the test-only OCI publisher' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F 'the isolated fixture image contains an operator binary' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
for packaged_chart_marker in \
	"go -C \"\$ROOT_DIR\" run ./hack/chartpackage" \
	"helm show chart \"\$CHART_PACKAGE\"" \
	"\"\$CHART_PACKAGE\" \\" \
	'installing release-form chart'; do
	grep -F -- "$packaged_chart_marker" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
done
grep -F 'LeaderElectionNamespace: targetLockNamespace' "$ROOT_DIR/cmd/manager/main.go" >/dev/null
grep -F 'ptah-operator.operator.ptah.dev' "$ROOT_DIR/cmd/manager/main.go" >/dev/null
for ha_marker in \
	"assert_can_i no \"\$FOREIGN_NAMESPACE\"" \
	'holder_is_ready_manager_pod' \
	'manager leader Lease did not move to a ready replica' \
	'leader Pod failover did not increment leaseTransitions' \
	'wait_for_admitted_operation_pod' \
	'operator.ptah.dev/admission-snapshot-digest' \
	'--cascade=background' \
	'wait --for=delete pod' \
	'background Job deletion left orphan operation Pods' \
	'admitted post-failover operation'; do
	grep -F -- "$ha_marker" "$ROOT_DIR/hack/e2e-ha.sh" >/dev/null
done
for approval_plan_marker in \
	"policy_uid=\$(k -n \"\$TEST_NAMESPACE\" get configmap" \
	"verificationPolicyUID: \$verificationPolicyUID" \
	"publishedChunks: [{name: \$chunkName, uid: \$chunkUID, index: 0}]" \
	"\"operator.ptah.dev/plan\": \$planName" \
	'binaryData: {chunk: "eA=="}'; do
	grep -F -- "$approval_plan_marker" "$ROOT_DIR/hack/e2e-assert.sh" >/dev/null
done
grep -F 'unset REGISTRY_PASSWORD' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
if grep -F 'E2E_REGISTRY_PASSWORD' "$ROOT_DIR/hack/e2e-kind.sh" \
	"$ROOT_DIR/hack/e2e-dataplane.sh" "$ROOT_DIR/hack/e2e-faults.sh" >/dev/null; then
	printf '%s\n' 'e2e static: registry password is handed off through the host environment' >&2
	exit 1
fi
for secret_script in e2e-dataplane.sh e2e-faults.sh; do
	grep -F 'grep -F -f' "$ROOT_DIR/hack/$secret_script" >/dev/null
	grep -F -- '--rawfile' "$ROOT_DIR/hack/$secret_script" >/dev/null
done
for pinned_input in E2E_REGISTRY_IMAGE E2E_POSTGRES_SOURCE_IMAGE E2E_MYSQL_SOURCE_IMAGE; do
	grep -E "^${pinned_input}=.*@sha256:[0-9a-f]{64}" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
done
for engine in postgresql mysql; do
	for revision in v1 v2 v3 v4; do
		fixture="$ROOT_DIR/testdata/e2e/${engine}-${revision}.sql"
		[ -s "$fixture" ] || {
			printf 'e2e static: missing schema fixture %s\n' "$fixture" >&2
			exit 1
		}
	done
	grep -F 'note ' "$ROOT_DIR/testdata/e2e/${engine}-v2.sql" >/dev/null
	grep -F 'enabled ' "$ROOT_DIR/testdata/e2e/${engine}-v3.sql" >/dev/null
	if [ "$engine" = mysql ]; then
		grep -F 'UNIQUE KEY e2e_widgets_name_unique' \
			"$ROOT_DIR/testdata/e2e/mysql-v3.sql" >/dev/null
		grep -F 'KEY e2e_widgets_name_idx' \
			"$ROOT_DIR/testdata/e2e/mysql-v3.sql" >/dev/null
		grep -F 'UNIQUE KEY e2e_widgets_name_unique' \
			"$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		grep -F 'note ' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		grep -F 'enabled ' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		if grep -F 'e2e_widgets_name_idx' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null; then
			printf '%s\n' 'e2e static: MySQL v4 must remove only the plain named index' >&2
			exit 1
		fi
		grep -F 'executor-underclassified DROP INDEX' \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
		grep -F 'DROP INDEX was not conservatively elevated to destructive' \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
	fi
	if [ "$engine" = postgresql ] &&
		grep -E 'note |enabled ' "$ROOT_DIR/testdata/e2e/${engine}-v4.sql" >/dev/null; then
		printf '%s\n' 'e2e static: PostgreSQL v4 must exercise destructive column removals' >&2
		exit 1
	fi
	[ -s "$ROOT_DIR/testdata/e2e/${engine}-fault-v1.sql" ] || {
		printf 'e2e static: missing %s fault schema fixture\n' "$engine" >&2
		exit 1
	}
	grep -F 'fault_token ' "$ROOT_DIR/testdata/e2e/${engine}-fault-v1.sql" >/dev/null
	if [ "$engine" = mysql ]; then
		grep -F 'UNIQUE KEY e2e_widgets_name_unique' \
			"$ROOT_DIR/testdata/e2e/mysql-fault-v1.sql" >/dev/null
		grep -F 'KEY e2e_widgets_name_idx' \
			"$ROOT_DIR/testdata/e2e/mysql-fault-v1.sql" >/dev/null
	fi
done
for lifecycle_marker in \
	'wait_for_in_sync' \
		'assert_periodic_noop' \
		'assert_approval_consumed' \
		'PlanNoLongerCurrent' \
		'DestructiveChangesDisabled' \
	'audit_runtime_credentials' \
	'create_admission_fixtures' \
	'admission-snapshot-digest' \
	'lacks exact LimitRange, ServiceAccount, RuntimeClass, or default-toleration admission' \
	'registryAuthFrom' \
		'coordinationKey' \
		'.status.target.driftReportDigest != ""' \
		'.spec.runnerProtocolVersion == 3' \
		'e2e-faults.sh' \
	'run_mysql_dsn_refusal' \
	'audit_started_containers' \
	'assert_active_pod_ephemeral_container_rejected' \
	'/ephemeralcontainers' \
	'unaudited nonterminal containers' \
	'controller == true' \
	'changed identity during its log audit' \
	'checkpoint_jobs'; do
	grep -F "$lifecycle_marker" "$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
done
for protocol_script in e2e-dataplane.sh e2e-faults.sh; do
	case "$protocol_script" in
	e2e-dataplane.sh) converged_result_occurrences=1 ;;
	e2e-faults.sh) converged_result_occurrences=2 ;;
	esac
	for converged_result_marker in \
		"(\$observe.observedDrift // false) == false" \
		"(\$observe.highestDriftSeverity // \"\") == \"\"" \
		"(\$observe.driftFindingCount // 0) == 0"; do
		grep -F "$converged_result_marker" "$ROOT_DIR/hack/$protocol_script" >/dev/null
		[ "$(grep -Fc "$converged_result_marker" "$ROOT_DIR/hack/$protocol_script")" -eq \
			"$converged_result_occurrences" ] || {
			printf 'e2e static: expected %s converged result markers in %s\n' \
				"$converged_result_occurrences" "$protocol_script" >&2
			exit 1
		}
	done
done
for admission_marker in \
	'.immutable = true' \
	'empty target Secret key' \
	'empty development target Secret key' \
	'empty verification policy ConfigMap key' \
	'empty OCI CA ConfigMap key' \
	'a rejected empty reference key created an operation Job' \
	'whitespace-only managed-scope selector' \
	'leading whitespace in managed-scope selector' \
	'trailing whitespace in managed-scope selector' \
	'control character in managed-scope selector' \
	'overlong managed-scope selector' \
	'duplicate managed-scope selector' \
	'a rejected managed-scope selector created an operation Job' \
	'schema-name schema-uid plan-name plan-uid plan-fingerprint' \
	'approval with a conflicting derived artifact binding' \
	'approval with a conflicting derived protocol binding' \
	'E2E_TEST_NAMESPACE and E2E_FOREIGN_NAMESPACE must differ' \
	'del(.metadata.ownerReferences, .metadata.finalizers)' \
	'foreign plan disappeared, changed, or entered deletion' \
	'cross-namespace approval refusal created an approval'; do
	grep -F "$admission_marker" "$ROOT_DIR/hack/e2e-assert.sh" >/dev/null
done
for fault_marker in \
	'assert_no_overlapping_operation_jobs' \
	'assert_no_overlapping_operation_pods' \
	'assert_fault_audit_complete' \
	'FAULT_TIMEOUT_ACTIVE_DEADLINE_SECONDS' \
	'record_deadline_pending_pod_evidence' \
	'wait_for_deadline_job_terminal_and_audit' \
	'.reason == "DeadlineExceeded"' \
	'Kubernetes-timeout recovery replayed or replaced its Apply Job' \
	'start_read_workload_barrier' \
	'assert_read_workload_blocked' \
	'record_initial_job_list_for_parent' \
	'assert_approval_consumed' \
	'finish_follow_logs' \
	'PlanNoLongerCurrent' \
	'podReplacementPolicy == "Failed"' \
	'timeoutSeconds=30' \
	'watch_heartbeat_loop' \
	'e2e-fault-watch-heartbeat' \
	'capture_exact_job_result' \
	'.truncation == null' \
	'assert_successful_apply_result' \
	'assert_fault_convergence_result_pair' \
	'capture_uncertain_read_proof_pair' \
	'matches_test_taint' \
	'assert_lease_held_without_release' \
	'establish_watch_barrier leases' \
	'assert_initial_read_chain_watch_order' \
	'assert_post_apply_proof_history' \
	'assert_uncertain_apply_proof_history' \
	'mysql_schema_fingerprint' \
	'operator.ptah.dev/lease-epoch' \
	'ALIAS_B_OPERATION_ID' \
	'ALIAS_B_LEASE_EPOCH' \
	'shared-alias consumed approval was not marked stale after recovery' \
	'assert_same_lease_reacquired_in_watch' \
	'manual drift lost its conservative OutcomeUnknown history' \
	'manual-drift read-only Observe or Plan changed the database schema' \
	'error.code == "invalid_plan_output"' \
	'error.code == "stale_plan"' \
	'manual drift did not retain exactly one fresh Plan Job' \
	'credential-bearing principal refusal to become durably suspended' \
	'credential-bearing principal refusal did not remain one Plan and zero Apply Jobs' \
	'credential-bearing principal refusal did not remain one Plan and zero Apply Pods'; do
	grep -F "$fault_marker" "$ROOT_DIR/hack/e2e-faults.sh" >/dev/null
done
for durable_mysql_marker in \
	'assert_mysql_destructive_refusal_durable' \
	'all_new_jobs_complete' \
	'three complete scheduled destructive refusals' \
	'MySQL DROP INDEX did not remain durably blocked'; do
	grep -F "$durable_mysql_marker" "$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
done

jq -n '
  def secretEnv($name; $secret; $key):
    {name: $name, valueFrom: {secretKeyRef: {name: $secret, key: $key}}};
  def registryEnv:
    [
      secretEnv("PTAH_OCI_USERNAME"; "registry-auth"; "username"),
      secretEnv("PTAH_OCI_PASSWORD"; "registry-auth"; "password"),
      secretEnv("PTAH_OCI_TOKEN"; "registry-auth"; "token"),
      secretEnv("PTAH_OCI_REGISTRY"; "registry-auth"; "registry"),
      {name: "PTAH_PLAIN_HTTP", value: "true"}
    ];
  def databaseEnv:
    [secretEnv("PTAH_DB_URL"; "database-url"; "url")];
  def observeEnv:
    databaseEnv + [{name: "PTAH_EXPECTED_DATABASE_ENGINE", value: "PostgreSQL"}];
  def job($operation; $mainEnv; $fetchEnv):
    {
      metadata: {
        uid: ("uid-" + $operation),
        labels: {"operator.ptah.dev/operation": $operation}
      },
      spec: {
        backoffLimit: 0,
        podReplacementPolicy: "Failed",
        template: {spec: {
          restartPolicy: "Never",
          containers: [{name: "ptah", env: $mainEnv}],
          initContainers:
            ([{name: "install-runner", env: []}] +
              (if $fetchEnv == null then [] else [{name: "fetch-schema", env: $fetchEnv}] end))
        }}
      }
    };
  {items: [
    job("resolve"; registryEnv; null),
    job("verify"; registryEnv; null),
    job("observe"; observeEnv; registryEnv),
    job("plan"; observeEnv; registryEnv),
    job("apply"; databaseEnv; null)
  ]}
' >"$CONTROLLER_JOB_FIXTURE"
jq -e \
	--arg databaseSecret database-url \
	--arg registrySecret registry-auth \
	--argjson requireApply true \
	-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" \
	"$CONTROLLER_JOB_FIXTURE" >/dev/null
for missing_container in resolve:ptah observe:fetch-schema; do
	negative_operation=${missing_container%%:*}
	negative_container=${missing_container#*:}
	jq \
		--arg operation "$negative_operation" \
		--arg container "$negative_container" '
      (.items[] |
        select(.metadata.labels["operator.ptah.dev/operation"] == $operation) |
        .spec.template.spec.containers) |= map(select(.name != $container)) |
      (.items[] |
        select(.metadata.labels["operator.ptah.dev/operation"] == $operation) |
        .spec.template.spec.initContainers) |= map(select(.name != $container))
    ' "$CONTROLLER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
	if jq -e \
		--arg databaseSecret database-url \
		--arg registrySecret registry-auth \
		--argjson requireApply true \
		-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" \
		"$NEGATIVE_FIXTURE" >/dev/null; then
		printf 'e2e static: controller isolation accepted %s without %s\n' \
			"$negative_operation" "$negative_container" >&2
		exit 1
	fi
done
jq '
  (.items[] |
    select(.metadata.labels["operator.ptah.dev/operation"] == "observe") |
    .spec.template.spec.containers[] |
    select(.name == "ptah") |
    .env) |= map(select(.name != "PTAH_EXPECTED_DATABASE_ENGINE"))
' "$CONTROLLER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
if jq -e \
	--arg databaseSecret database-url \
	--arg registrySecret registry-auth \
	--argjson requireApply true \
	-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" \
	"$NEGATIVE_FIXTURE" >/dev/null; then
	printf '%s\n' 'e2e static: controller isolation accepted Observe without the expected database engine' >&2
	exit 1
fi
jq '
  (.items[] |
    select(.metadata.labels["operator.ptah.dev/operation"] == "plan") |
    .spec.template.spec.containers[] |
    select(.name == "ptah") |
    .env) |= map(select(.name != "PTAH_EXPECTED_DATABASE_ENGINE"))
' "$CONTROLLER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
if jq -e \
	--arg databaseSecret database-url \
	--arg registrySecret registry-auth \
	--argjson requireApply true \
	-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" \
	"$NEGATIVE_FIXTURE" >/dev/null; then
	printf '%s\n' 'e2e static: controller isolation accepted Plan without the expected database engine' >&2
	exit 1
fi
for invalid_policy in missing terminating-or-failed; do
	case "$invalid_policy" in
	missing)
		jq 'del(.items[0].spec.podReplacementPolicy)' \
			"$CONTROLLER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
		;;
	terminating-or-failed)
		jq '.items[0].spec.podReplacementPolicy = "TerminatingOrFailed"' \
			"$CONTROLLER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
		;;
	esac
	if jq -e \
		--arg databaseSecret database-url \
		--arg registrySecret registry-auth \
		--argjson requireApply true \
		-f "$ROOT_DIR/testdata/e2e/controller-job-isolation.jq" \
		"$NEGATIVE_FIXTURE" >/dev/null; then
		printf 'e2e static: controller isolation accepted %s pod replacement policy\n' \
			"$invalid_policy" >&2
		exit 1
	fi
done

jq -n '
  def secretEnv($name; $key):
    {name: $name, valueFrom: {secretKeyRef: {name: "registry-auth", key: $key}}};
  {spec: {template: {spec: {containers: [{
    name: "publisher",
    image: "e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000",
    env: [
      {name: "HOME", value: "/work"},
      {name: "TMPDIR", value: "/work"},
      secretEnv("PTAH_OCI_USERNAME"; "username"),
      secretEnv("PTAH_OCI_PASSWORD"; "password"),
      secretEnv("PTAH_OCI_REGISTRY"; "registry")
    ]
  }]}}}}
' >"$PUBLISHER_JOB_FIXTURE"
jq -e \
	--arg image e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--arg registrySecret registry-auth \
	-f "$ROOT_DIR/testdata/e2e/publisher-job-isolation.jq" \
	"$PUBLISHER_JOB_FIXTURE" >/dev/null
jq '
  .spec.template.spec.containers[0].env += [{
    name: "PTAH_DEV_URL",
    valueFrom: {secretKeyRef: {name: "database-url", key: "url"}}
  }]
' "$PUBLISHER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
if jq -e \
	--arg image e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--arg registrySecret registry-auth \
	-f "$ROOT_DIR/testdata/e2e/publisher-job-isolation.jq" \
	"$NEGATIVE_FIXTURE" >/dev/null; then
	printf '%s\n' 'e2e static: publisher isolation accepted a development database credential' >&2
	exit 1
fi

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	>/dev/null 2>"$MUTABLE_MANAGER_ERROR"; then
	printf '%s\n' 'e2e static: chart accepted an unpinned manager image' >&2
	exit 1
fi
grep -F 'image.digest must pin the manager' "$MUTABLE_MANAGER_ERROR" >/dev/null

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set replicaCount=2 \
	--set leaderElection=false \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	>/dev/null 2>"$LEADER_ELECTION_ERROR"; then
	printf '%s\n' 'e2e static: chart accepted multiple replicas without leader election' >&2
	exit 1
fi
grep -F 'replicaCount' "$LEADER_ELECTION_ERROR" >/dev/null

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/rbac.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$DEFAULT_RBAC_RENDER"

helm template ptah-e2e-ha "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e-ha \
	--show-only templates/rbac.yaml \
	--set-string coordination.namespace=ptah-e2e \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$SHARED_RBAC_RENDER"

for rbac_render in "$DEFAULT_RBAC_RENDER" "$SHARED_RBAC_RENDER"; do
	[ "$(grep -c '^kind: Role$' "$rbac_render")" -eq 1 ] || {
		printf 'e2e static: %s does not render exactly one manager Lease Role\n' "$rbac_render" >&2
		exit 1
	}
	[ "$(grep -c '^kind: RoleBinding$' "$rbac_render")" -eq 1 ] || {
		printf 'e2e static: %s does not render exactly one manager Lease RoleBinding\n' "$rbac_render" >&2
		exit 1
	}
	if awk '
      /^kind: ClusterRole$/ {cluster_role = 1; next}
      /^---$/ {cluster_role = 0}
      cluster_role && /resources: \["leases"\]/ {found = 1}
      END {exit found ? 0 : 1}
    ' "$rbac_render"; then
		printf 'e2e static: %s grants cluster-wide manager Lease access\n' "$rbac_render" >&2
		exit 1
	fi
	lease_verbs=$(awk '
      /resources: \["leases"\]/ {
        if (getline > 0) print
        exit
      }
    ' "$rbac_render" | tr -d '[:space:]')
	[ "$lease_verbs" = 'verbs:["get","create","update"]' ] || {
		printf 'e2e static: %s manager Lease verbs are not exact\n' "$rbac_render" >&2
		exit 1
	}
done

default_role_namespace=$(awk '
  /^kind: Role$/ {role = 1; next}
  role && /^  namespace:/ {print $2; exit}
' "$DEFAULT_RBAC_RENDER")
[ "$default_role_namespace" = ptah-e2e ] || {
	printf 'e2e static: default manager Lease Role namespace = %s, want ptah-e2e\n' \
		"$default_role_namespace" >&2
	exit 1
}
shared_role_namespace=$(awk '
  /^kind: Role$/ {role = 1; next}
  role && /^  namespace:/ {print $2; exit}
' "$SHARED_RBAC_RENDER")
[ "$shared_role_namespace" = ptah-e2e ] || {
	printf 'e2e static: shared manager Lease Role namespace = %s, want ptah-e2e\n' \
		"$shared_role_namespace" >&2
	exit 1
}
shared_subject_namespace=$(awk '
  /^kind: RoleBinding$/ {binding = 1; next}
  binding && /^    namespace:/ {print $2; exit}
' "$SHARED_RBAC_RENDER")
[ "$shared_subject_namespace" = ptah-e2e-ha ] || {
	printf 'e2e static: cross-namespace Lease RoleBinding subject = %s, want ptah-e2e-ha\n' \
		"$shared_subject_namespace" >&2
	exit 1
}

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
		--namespace ptah-e2e \
		--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
		--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$RENDERED_WEBHOOKS"
[ "$(grep -Fc 'resources: ["ptahschemas/finalizers", "ptahschemaplans/finalizers"]' \
	"$RENDERED_WEBHOOKS")" -eq 1 ] || {
	printf '%s\n' 'e2e static: rendered controller role lacks its exact owner-finalizer resources' >&2
	exit 1
}
finalizer_verbs=$(awk '
  index($0, "resources: [\"ptahschemas/finalizers\", \"ptahschemaplans/finalizers\"]") {
    if (getline > 0) print
    exit
  }
' "$RENDERED_WEBHOOKS" | tr -d '[:space:]')
[ "$finalizer_verbs" = 'verbs:["update"]' ] || {
	printf '%s\n' 'e2e static: rendered controller role lacks exact owner-finalizer update authorization' >&2
	exit 1
}
[ "$(grep -c '^kind: MutatingWebhookConfiguration$' "$RENDERED_WEBHOOKS")" -eq 1 ]
[ "$(grep -c '^kind: ValidatingWebhookConfiguration$' "$RENDERED_WEBHOOKS")" -eq 1 ]
[ "$(grep -c '^[[:space:]]*failurePolicy: Fail$' "$RENDERED_WEBHOOKS")" -eq 3 ]
grep -F 'name: vpodintent.operator.ptah.dev' "$RENDERED_WEBHOOKS" >/dev/null
grep -F 'path: /validate-v1-pod-ptah-operation-intent' "$RENDERED_WEBHOOKS" >/dev/null
grep -F 'resources: ["pods", "pods/ephemeralcontainers", "pods/resize"]' "$RENDERED_WEBHOOKS" >/dev/null
grep -F 'operations: ["CREATE", "UPDATE"]' "$RENDERED_WEBHOOKS" >/dev/null
grep -F 'name: job-owned-pod' "$RENDERED_WEBHOOKS" >/dev/null
if grep -F 'objectSelector:' "$RENDERED_WEBHOOKS" >/dev/null; then
	printf '%s\n' 'e2e static: Pod intent admission relies on a mutable object selector' >&2
	exit 1
fi
grep -F -- '--default-tolerations-enabled=true' "$RENDERED_WEBHOOKS" >/dev/null
grep -F -- '--extended-resource-toleration-enabled=false' "$RENDERED_WEBHOOKS" >/dev/null
grep -F -- '--always-pull-images-enabled=false' "$RENDERED_WEBHOOKS" >/dev/null
for admission_resource in \
	'resources: ["serviceaccounts"]' \
	'resources: ["limitranges"]' \
	'resources: ["runtimeclasses"]' \
	'resources: ["priorityclasses"]'; do
	grep -F "$admission_resource" "$RENDERED_WEBHOOKS" >/dev/null
done
pods_verbs=$(awk '
  index($0, "resources: [\"pods\"]") {
    if (getline > 0) print
    exit
  }
' "$RENDERED_WEBHOOKS" | tr -d '[:space:]')
[ "$pods_verbs" = 'verbs:["get","list","watch"]' ] || {
	printf '%s\n' 'e2e static: controller Pod RBAC exceeds read-only evidence access' >&2
	exit 1
}
if grep -Eq '^[[:space:]]*failurePolicy: (Ignore|FailOpen)$' "$RENDERED_WEBHOOKS"; then
	printf '%s\n' 'e2e static: rendered admission webhooks are not fail-closed' >&2
	exit 1
fi

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/certificate-rotation.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	>"$ROTATOR_RENDER"
[ "$(grep -c '^kind: Deployment$' "$ROTATOR_RENDER")" -eq 1 ]
if grep -Eq '^kind: (Job|CronJob)$' "$ROTATOR_RENDER"; then
	printf '%s\n' 'e2e static: certificate rotator is blocked by Job Pod admission' >&2
	exit 1
fi
for rotator_marker in \
		'--run-interval=6h' \
	'--operation-timeout=15m' \
	'--retry-initial=5s' \
	'--retry-max=5m' \
	'--health-bind-address=:8081' \
	'path: /healthz' \
	'path: /readyz' \
	'name: health' \
	'containerPort: 8081'; do
	grep -F -- "$rotator_marker" "$ROTATOR_RENDER" >/dev/null
done
for default_forbidden_marker in \
		'kind: ValidatingAdmissionPolicy' \
		'kind: ValidatingAdmissionPolicyBinding' \
		'verbs: ["create"]' \
		'validatingadmissionpolicies' \
		'validatingadmissionpolicybindings' \
		'--recreate-missing-secret' \
		'--secret-create-policy-name=' \
		'--secret-create-policy-binding-name=' \
		'--secret-create-service-account-name='; do
	if grep -F -- "$default_forbidden_marker" "$ROTATOR_RENDER" >/dev/null; then
		printf 'e2e static: default certificate lifecycle contains opt-in marker %s\n' \
			"$default_forbidden_marker" >&2
		exit 1
	fi
done

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/certificate-rotation.yaml \
	--set certificateRotation.recreateMissingSecret=true \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	>"$ROTATOR_RECREATE_RENDER"
for recreation_marker in \
		'kind: ValidatingAdmissionPolicy' \
		'kind: ValidatingAdmissionPolicyBinding' \
		'failurePolicy: Fail' \
		'validationActions: [Deny]' \
		'verbs: ["create"]' \
		'operator.ptah.dev/generated-webhook-certificate' \
		'certificate rotator Secret CREATE is outside its exact recovery contract' \
		'--recreate-missing-secret=true' \
		'--secret-create-policy-name=' \
		'--secret-create-policy-binding-name=' \
		'--secret-create-service-account-name='; do
	grep -F -- "$recreation_marker" "$ROTATOR_RECREATE_RENDER" >/dev/null
done
grep -F 'StartedChecker()' "$ROOT_DIR/cmd/manager/main.go" >/dev/null
grep -F -- '--set certificateRotation.recreateMissingSecret=true' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
for recovery_marker in \
	'ptah-rotator-unauthorized' \
	'--dry-run=server' \
	"delete secret \"\$SECRET_NAME\"" \
	'operator.ptah.dev/generated-webhook-certificate' \
	'did not contract after Secret recreation'; do
	grep -F -- "$recovery_marker" "$ROOT_DIR/hack/e2e-cert-rotation.sh" >/dev/null
done
for helm_lookup_marker in \
	"E2E_CHART_PACKAGE=\$CHART_PACKAGE" \
	"E2E_TEST_NAMESPACE=\$TEST_NAMESPACE" \
	'generate_upgrade_ca mutating' \
	'generate_upgrade_ca approval-validating' \
	'generate_upgrade_ca pod-validating' \
	'assert_entry_bundle mutatingwebhookconfiguration' \
	'assert_entry_bundle validatingwebhookconfiguration' \
	'--reuse-values --wait --timeout 5m' \
	"caBundle for \${webhook_name} gained another entry" \
	'assert_approval_admission_callable "after the Helm upgrade"'; do
	grep -F -- "$helm_lookup_marker" "$ROOT_DIR/hack/e2e-kind.sh" \
		"$ROOT_DIR/hack/e2e-cert-rotation.sh" >/dev/null
done
for per_entry_marker in \
	'"mutating"' \
	'"approvalValidating"' \
	'"podValidating"' \
	'ptah-operator.webhookEntryCABundle'; do
	grep -F -- "$per_entry_marker" "$ROOT_DIR/charts/ptah-operator/templates/webhook.yaml" >/dev/null
done
if grep -F 'longestExistingBundle' "$ROOT_DIR/charts/ptah-operator/templates/webhook.yaml" >/dev/null; then
	printf '%s\n' 'e2e static: Helm webhook trust recovery selects or cross-copies a global bundle' >&2
	exit 1
fi

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
		--namespace ptah-e2e \
		--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
		--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	--set-string webhook.failurePolicy=Ignore \
	>"$OBSOLETE_RENDER" 2>"$OBSOLETE_ERROR"; then
	[ "$(grep -c '^[[:space:]]*failurePolicy: Fail$' "$OBSOLETE_RENDER")" -eq 3 ] || {
		printf '%s\n' 'e2e static: obsolete webhook.failurePolicy changed the rendered fail-closed contract' >&2
		exit 1
	}
	if grep -Eq '^[[:space:]]*failurePolicy: (Ignore|FailOpen)$' "$OBSOLETE_RENDER"; then
		printf '%s\n' 'e2e static: obsolete webhook.failurePolicy enabled fail-open admission' >&2
		exit 1
	fi
else
	grep -F 'failurePolicy' "$OBSOLETE_ERROR" >/dev/null || {
		printf '%s\n' 'e2e static: obsolete webhook.failurePolicy render failed for an unrelated reason' >&2
		exit 1
	}
fi

printf '%s\n' 'e2e static: PASS'
