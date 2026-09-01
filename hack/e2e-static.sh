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
MYSQL_REFUSAL_REWRITE_FILTER=$WORK_DIR/mysql-refusal-rewrite.jq
MYSQL_REFUSAL_SOURCE=$WORK_DIR/mysql-refusal-source.json
MYSQL_REFUSAL_REWRITTEN=$WORK_DIR/mysql-refusal-rewritten.json
MYSQL_REFUSAL_NULL_SOURCE=$WORK_DIR/mysql-refusal-null-source.json
MYSQL_REFUSAL_NULL_REWRITTEN=$WORK_DIR/mysql-refusal-null-rewritten.json
CLEANUP_DIAGNOSTIC_FILTER=$WORK_DIR/cleanup-diagnostic.jq
CLEANUP_SCHEMA_FIXTURE=$WORK_DIR/cleanup-schemas.json
CLEANUP_EVENT_FIXTURE=$WORK_DIR/cleanup-events.json
CLEANUP_JOB_FIXTURE=$WORK_DIR/cleanup-jobs.json
CLEANUP_LEASE_FIXTURE=$WORK_DIR/cleanup-leases.json
CLEANUP_DIAGNOSTIC_FIXTURE=$WORK_DIR/cleanup-diagnostic.json
CLEANUP_SCAN_RUNNER=$WORK_DIR/cleanup-scan-runner.sh
CLEANUP_EMPTY_PATTERNS=$WORK_DIR/cleanup-empty-patterns.txt
CLEANUP_SAFE_PATTERNS=$WORK_DIR/cleanup-safe-patterns.txt
CLEANUP_MATCH_PATTERNS=$WORK_DIR/cleanup-match-patterns.txt
CLEANUP_UNSAFE_PROJECTION=$WORK_DIR/cleanup-unsafe-projection.json

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

sed -n '/# mysql-refusal-job-rewrite-begin/,/# mysql-refusal-job-rewrite-end/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh" | sed '1d;$d' >"$MYSQL_REFUSAL_REWRITE_FILTER"
[ -s "$MYSQL_REFUSAL_REWRITE_FILTER" ] || {
	printf '%s\n' 'e2e static: MySQL refusal Job rewrite filter is missing' >&2
	exit 1
}
jq -n '
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
' >"$MYSQL_REFUSAL_SOURCE"
jq \
	--arg namespace test-namespace \
	--arg name test-name \
	--arg schema test-schema \
	--arg operation observe \
	--arg operationID test-operation-id \
	--arg secret test-secret \
	-f "$MYSQL_REFUSAL_REWRITE_FILTER" "$MYSQL_REFUSAL_SOURCE" \
	>"$MYSQL_REFUSAL_REWRITTEN"
jq -e '
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
' "$MYSQL_REFUSAL_REWRITTEN" >/dev/null || {
	printf '%s\n' 'e2e static: MySQL refusal rewrite changed absent initContainers or unrelated Pod semantics' >&2
	exit 1
}

sed -n '/# cleanup-diagnostic-projection-begin/,/# cleanup-diagnostic-projection-end/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh" | sed '1d;$d' >"$CLEANUP_DIAGNOSTIC_FILTER"
[ -s "$CLEANUP_DIAGNOSTIC_FILTER" ] || {
	printf '%s\n' 'e2e static: cleanup diagnostic projection filter is missing' >&2
	exit 1
}
jq -n '
  {
    items: [
      {
        apiVersion: "operator.ptah.dev/v1alpha1",
        kind: "PtahSchema",
        metadata: {name: "schema-a", uid: "schema-uid-a", generation: 4},
        spec: {unsafeSentinel: "DO_NOT_PRINT_CREDENTIAL_SENTINEL"},
        status: {
          observedGeneration: 4,
          phase: "Failed",
          nextReconciliationTime: "2026-09-01T00:00:00Z",
          activeOperation: {
            leaseEpoch: "v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            leaseContinuityLost: true
          },
          pendingObservation: {leaseEpoch: "malformed-DO_NOT_PRINT_CREDENTIAL_SENTINEL"},
          pendingLockRelease: {leaseEpoch: "v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
          conditions: [{
            type: "ReconciliationFailed",
            status: "True",
            reason: "OperationFailed",
            message: "invalid_plan_output: DO_NOT_PRINT_CREDENTIAL_SENTINEL"
          }]
        }
      },
      {
        apiVersion: "operator.ptah.dev/v1alpha1",
        kind: "PtahSchema",
        metadata: {name: "schema-b", uid: "schema-uid-b", generation: 2},
        status: {
          observedGeneration: 2,
          phase: "not-a-phase-DO_NOT_PRINT_CREDENTIAL_SENTINEL",
          activeOperation: {leaseEpoch: "v1-NOT-AN-EPOCH"},
          conditions: [{
            type: "ReconciliationFailed",
            status: "True",
            reason: "OperationFailed",
            message: "bad__code: DO_NOT_PRINT_CREDENTIAL_SENTINEL"
          }]
        }
      }
    ]
  }
' >"$CLEANUP_SCHEMA_FIXTURE"
jq -n '
  {
    items: [
      {
        involvedObject: {
          apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
          name: "schema-a", uid: "schema-uid-a"
        },
        type: "Warning", reason: "OperationFailed",
        message: "invalid_target: DO_NOT_PRINT_CREDENTIAL_SENTINEL",
        lastTimestamp: "2026-09-01T00:00:01Z"
      },
      {
        involvedObject: {
          apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
          name: "schema-a", uid: "schema-uid-a"
        },
        type: "Warning", reason: "PlanStale",
        message: "DO_NOT_PRINT_CREDENTIAL_SENTINEL",
        lastTimestamp: "2026-09-01T00:00:02Z"
      },
      {
        involvedObject: {
          apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
          name: "schema-a", uid: "schema-uid-a"
        },
        type: "Warning", reason: "LeaseContinuityLost",
        message: "DO_NOT_PRINT_CREDENTIAL_SENTINEL",
        lastTimestamp: "2026-09-01T00:00:03Z"
      },
      {
        involvedObject: {
          apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
          name: "schema-a", uid: "wrong-schema-uid"
        },
        type: "Warning", reason: "ReconciliationFailed",
        message: "wrong_uid_code: DO_NOT_PRINT_CREDENTIAL_SENTINEL"
      },
      {
        involvedObject: {
          apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
          name: "schema-b", uid: "schema-uid-b"
        },
        type: "Warning", reason: "OperationFailed",
        message: "bad__code: DO_NOT_PRINT_CREDENTIAL_SENTINEL"
      }
    ]
  }
' >"$CLEANUP_EVENT_FIXTURE"
jq -n '
  {
    items: [
      {
        metadata: {
          name: "job-a", uid: "job-uid-a", creationTimestamp: "2026-09-01T00:00:00Z",
          ownerReferences: [{
            apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
            uid: "schema-uid-a", controller: true
          }],
          labels: {"operator.ptah.dev/operation": "plan"},
          annotations: {
            "operator.ptah.dev/operation-id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "operator.ptah.dev/input-fingerprint": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
            unsafe: "DO_NOT_PRINT_CREDENTIAL_SENTINEL"
          }
        },
        status: {
          startTime: "2026-09-01T00:00:00Z",
          completionTime: "2026-09-01T00:00:01Z",
          conditions: [{type: "Failed", status: "True", message: "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}]
        }
      },
      {
        metadata: {
          name: "job-b", uid: "job-uid-b",
          ownerReferences: [{
            apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
            uid: "schema-uid-b", controller: true
          }],
          labels: {"operator.ptah.dev/operation": "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}
        },
        status: {}
      }
    ]
  }
' >"$CLEANUP_JOB_FIXTURE"
jq -n '
  {
    items: [
      {
        metadata: {
          name: "lease-a", uid: "lease-uid-a", resourceVersion: "10",
          labels: {
            "app.kubernetes.io/managed-by": "ptah-operator",
            "operator.ptah.dev/coordination": "database-target"
          },
          annotations: {"operator.ptah.dev/lease-epoch": "v1-cccccccccccccccccccccccccccccccc"}
        },
        spec: {
          holderIdentity: "ptah-h-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
          leaseDurationSeconds: 960,
          leaseTransitions: 1
        }
      },
      {
        metadata: {
          name: "lease-b", uid: "lease-uid-b", resourceVersion: "11",
          labels: {
            "app.kubernetes.io/managed-by": "ptah-operator",
            "operator.ptah.dev/coordination": "database-target"
          },
          annotations: {"operator.ptah.dev/lease-epoch": "malformed-DO_NOT_PRINT_CREDENTIAL_SENTINEL"}
        },
        spec: {holderIdentity: "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}
      },
      {
        metadata: {
          name: "lease-unmanaged", uid: "lease-uid-unmanaged",
          labels: {"app.kubernetes.io/managed-by": "someone-else"}
        },
        spec: {holderIdentity: "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}
      }
    ]
  }
' >"$CLEANUP_LEASE_FIXTURE"
jq -n \
	--slurpfile schemas "$CLEANUP_SCHEMA_FIXTURE" \
	--slurpfile events "$CLEANUP_EVENT_FIXTURE" \
	--slurpfile jobs "$CLEANUP_JOB_FIXTURE" \
	--slurpfile leases "$CLEANUP_LEASE_FIXTURE" \
	-f "$CLEANUP_DIAGNOSTIC_FILTER" >"$CLEANUP_DIAGNOSTIC_FIXTURE"
if grep -F 'DO_NOT_PRINT_CREDENTIAL_SENTINEL' "$CLEANUP_DIAGNOSTIC_FIXTURE" >/dev/null; then
	printf '%s\n' 'e2e static: cleanup diagnostic projection disclosed its sentinel' >&2
	exit 1
fi
jq -e '
  (.schemas | length) == 2 and
  .schemas[0].uid == "schema-uid-a" and
  .schemas[0].failure.code == "invalid_plan_output" and
  .schemas[0].expectedEpochs == {
    activeOperation: "v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    pendingObservation: null,
    pendingLockRelease: "v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  } and
  .schemas[0].leaseContinuityLost == true and
  ([.schemas[0].events[].reason] | sort) ==
    ["LeaseContinuityLost", "OperationFailed", "PlanStale"] and
  ([.schemas[].events[].code] | index("wrong_uid_code")) == null and
  .schemas[1].phase == null and .schemas[1].failure.code == null and
  .schemas[1].expectedEpochs.activeOperation == null and
  .schemas[1].jobs[0].operation == null and
  (.leases | length) == 2 and
  .leases[0].epoch == "v1-cccccccccccccccccccccccccccccccc" and
  .leases[0].holderPresent == true and .leases[0].holderHashShape == true and
  .leases[1].epoch == null and
  .leases[1].holderPresent == true and .leases[1].holderHashShape == false
' "$CLEANUP_DIAGNOSTIC_FIXTURE" >/dev/null || {
	printf '%s\n' 'e2e static: cleanup diagnostic normalization or exact-UID filtering failed' >&2
	exit 1
}

sed -n '/^suppress_cleanup_diagnostics()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh" >"$CLEANUP_SCAN_RUNNER"
sed -n '/^emit_scanned_cleanup_diagnostic()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh" >>"$CLEANUP_SCAN_RUNNER"
# shellcheck disable=SC2016 # Arguments expand when the generated runner executes.
printf '%s\n' 'emit_scanned_cleanup_diagnostic "$1" "$2"' >>"$CLEANUP_SCAN_RUNNER"
chmod 700 "$CLEANUP_SCAN_RUNNER"
: >"$CLEANUP_EMPTY_PATTERNS"
printf '%s\n' 'NEVER_MATCH_CLEANUP_DIAGNOSTIC' >"$CLEANUP_SAFE_PATTERNS"
printf '%s\n' 'DO_NOT_PRINT_CREDENTIAL_SENTINEL' >"$CLEANUP_MATCH_PATTERNS"
jq -n '{unsafe: "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}' >"$CLEANUP_UNSAFE_PROJECTION"
assert_cleanup_diagnostic_suppressed() {
	suppression_projection=$1
	suppression_patterns=$2
	if suppression_output=$(sh "$CLEANUP_SCAN_RUNNER" \
		"$suppression_projection" "$suppression_patterns" 2>&1); then
		:
	else
		printf '%s\n' 'e2e static: cleanup diagnostic suppression returned nonzero' >&2
		exit 1
	fi
	[ "$suppression_output" = \
		'e2e data plane: credential-safe reconciliation diagnostics suppressed' ] || {
		printf '%s\n' 'e2e static: cleanup diagnostic suppression emitted nonfixed output' >&2
		exit 1
	}
}
assert_cleanup_diagnostic_suppressed "$CLEANUP_DIAGNOSTIC_FIXTURE" \
	"$CLEANUP_EMPTY_PATTERNS"
assert_cleanup_diagnostic_suppressed "$CLEANUP_UNSAFE_PROJECTION" \
	"$CLEANUP_MATCH_PATTERNS"
assert_cleanup_diagnostic_suppressed "$WORK_DIR/missing-cleanup-projection.json" \
	"$CLEANUP_SAFE_PATTERNS"
if cleanup_safe_output=$(sh "$CLEANUP_SCAN_RUNNER" \
	"$CLEANUP_DIAGNOSTIC_FIXTURE" "$CLEANUP_SAFE_PATTERNS" 2>&1); then
	:
else
	printf '%s\n' 'e2e static: safe cleanup diagnostic projection returned nonzero' >&2
	exit 1
fi
cleanup_safe_header=$(printf '%s\n' "$cleanup_safe_output" | sed -n '1p')
cleanup_safe_json=$(printf '%s\n' "$cleanup_safe_output" | sed -n '2p')
cleanup_safe_lines=$(printf '%s\n' "$cleanup_safe_output" | awk 'END { print NR }')

if [ "$cleanup_safe_header" = \
	'e2e data plane: credential-safe reconciliation diagnostic projection' ] &&
	[ "$cleanup_safe_lines" -eq 2 ] &&
	[ "$cleanup_safe_json" = "$(jq -c . "$CLEANUP_DIAGNOSTIC_FIXTURE")" ]; then
	:
else
	printf '%s\n' 'e2e static: safe cleanup diagnostic projection was not emitted exactly' >&2
	exit 1
fi
jq '.spec.template.spec.initContainers = null' "$MYSQL_REFUSAL_SOURCE" \
	>"$MYSQL_REFUSAL_NULL_SOURCE"
jq \
	--arg namespace test-namespace \
	--arg name test-name \
	--arg schema test-schema \
	--arg operation observe \
	--arg operationID test-operation-id \
	--arg secret test-secret \
	-f "$MYSQL_REFUSAL_REWRITE_FILTER" "$MYSQL_REFUSAL_NULL_SOURCE" \
	>"$MYSQL_REFUSAL_NULL_REWRITTEN"
jq -e '
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
' "$MYSQL_REFUSAL_NULL_REWRITTEN" >/dev/null || {
	printf '%s\n' 'e2e static: MySQL refusal rewrite changed null initContainers or unrelated Pod semantics' >&2
	exit 1
}

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
		grep -F 'note ' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		grep -F 'enabled ' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		if grep -Fi 'e2e_widgets_name_idx' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null; then
			printf '%s\n' 'e2e static: MySQL v4 must remove only the plain named index' >&2
			exit 1
		fi
		for mysql_fixture in mysql-v3.sql mysql-v4.sql mysql-fault-v1.sql; do
			fixture_path="$ROOT_DIR/testdata/e2e/$mysql_fixture"
			if [ "$(grep -Fxc '  UNIQUE KEY e2e_widgets_name_unique (name)' \
				"$fixture_path")" -ne 1 ] ||
				[ "$(grep -Eic 'e2e_widgets_name_unique' "$fixture_path")" -ne 1 ]; then
				printf 'e2e static: %s must contain exactly one unique name index\n' \
					"$mysql_fixture" >&2
				exit 1
			fi
			if grep -E '(^|[[:space:]])(--|#)|/\*|\*/' "$fixture_path" >/dev/null; then
				printf 'e2e static: %s must not hide index evidence in SQL comments\n' \
					"$mysql_fixture" >&2
				exit 1
			fi
		done
		for mysql_fixture in mysql-v3.sql mysql-fault-v1.sql; do
			fixture_path="$ROOT_DIR/testdata/e2e/$mysql_fixture"
			if [ "$(grep -Ec '^CREATE INDEX e2e_widgets_name_idx ON e2e_widgets \(name\);$' \
				"$fixture_path")" -ne 1 ] ||
				[ "$(grep -Eic 'e2e_widgets_name_idx' "$fixture_path")" -ne 1 ]; then
				printf 'e2e static: %s must contain exactly one standalone plain named index\n' \
					"$mysql_fixture" >&2
				exit 1
			fi
		done
		grep -F 'executor-underclassified DROP INDEX' \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
		grep -F 'DROP INDEX was not conservatively elevated to destructive' \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
		grep -F "index_name='e2e_widgets_name_unique' GROUP BY index_name HAVING count(*)=1 AND sum(non_unique=0 AND column_name='name' AND seq_in_index=1 AND expression IS NULL AND sub_part IS NULL)=1" \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
		grep -F "index_name='e2e_widgets_name_idx' GROUP BY index_name HAVING count(*)=1 AND sum(non_unique=1 AND column_name='name' AND seq_in_index=1 AND expression IS NULL AND sub_part IS NULL)=1" \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
		grep -F '(.statements | length) == 1' \
			"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
		grep -F '(.statements[0].sql | contains("e2e_widgets_name_idx"))' \
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
done
for lifecycle_marker in \
	'wait_for_in_sync' \
	'assert_periodic_noop' \
	'set_reconcile_interval_and_assert_noop' \
	'checkpoint_schema_jobs' \
	'job_count_between_checkpoints' \
	'schema_job_count_between_checkpoints' \
	'capture_blocked_refresh_boundary' \
	'prepare_blocked_refresh_cadence' \
	'report_blocked_refresh_diagnostics' \
	'restore_blocked_refresh_cadence' \
	'E2E_APPROVAL_INTERVAL' \
	'E2E_STALE_APPROVAL_INTERVAL' \
	'E2E_TAG_MOVE_INTERVAL' \
	'E2E_QUIESCENT_INTERVAL' \
	'E2E_BLOCKED_REFRESH_SECONDS' \
	'one generation-triggered no-op cycle after selecting interval' \
	'suspend_schema_for_tag_move' \
	'resume_schema_after_tag_move' \
	'a quiescent suspension before moving the mutable tag' \
	'assert_approval_consumed' \
	'PlanNoLongerCurrent' \
	'DestructiveChangesDisabled' \
	'three complete scheduled blocked refresh cycles' \
	'the quiescent blocked cadence restore' \
	'changed blocked evidence while restoring a quiet cadence' \
	'created: .metadata.creationTimestamp' \
	'operation_rank' \
	'did not preserve ordered interval-spaced blocked refresh cycles' \
	'.status.nextReconciliationTime != null' \
	'blocked refresh changed its current plan evidence' \
	'immutable destructive plan changed during refresh' \
	'the persisted mutable-tag polling deadline to elapse without a spec change' \
	'scheduled tag refresh depended on a spec generation change' \
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
for reconciliation_cadence_marker in \
	"RECONCILE_INTERVAL=\${E2E_RECONCILE_INTERVAL:-1m}" \
	"TAG_MOVE_INTERVAL=\${E2E_TAG_MOVE_INTERVAL:-2m}" \
	"APPROVAL_INTERVAL=\${E2E_APPROVAL_INTERVAL:-5m}" \
	"STALE_APPROVAL_INTERVAL=\${E2E_STALE_APPROVAL_INTERVAL:-4m}" \
	"QUIESCENT_INTERVAL=\${E2E_QUIESCENT_INTERVAL:-30m}" \
	"BLOCKED_REFRESH_SECONDS=\${E2E_BLOCKED_REFRESH_SECONDS:-30}" \
	"minimum_gate_timeout=\$((BLOCKED_REFRESH_SECONDS * 3 + 120))" \
	"--arg interval \"\$APPROVAL_INTERVAL\"" \
	"set_reconcile_interval_and_assert_noop \"\$lifecycle_schema\" \"\$digest_v1\"" \
	"assert_periodic_noop \"\$lifecycle_schema\" \"\$PERIODIC_NOOP_CHECKPOINT\"" \
	"suspend_schema_for_tag_move \"\$lifecycle_schema\" \"\$APPROVAL_INTERVAL\""; do
	grep -F -- "$reconciliation_cadence_marker" \
		"$ROOT_DIR/hack/e2e-dataplane.sh" >/dev/null
done
for atomic_checkpoint_function in assert_periodic_noop set_reconcile_interval_and_assert_noop; do
	atomic_checkpoint_section=$(sed -n \
		"/^${atomic_checkpoint_function}()/,/^}/p" \
		"$ROOT_DIR/hack/e2e-dataplane.sh")
	printf '%s\n' "$atomic_checkpoint_section" | grep -F 'checkpoint_schema_jobs' >/dev/null
	if printf '%s\n' "$atomic_checkpoint_section" | grep -F 'checkpoint_jobs' >/dev/null; then
		printf 'e2e static: %s uses non-atomic operation checkpoints\n' \
			"$atomic_checkpoint_function" >&2
		exit 1
	fi
done
assert_plan_section=$(sed -n '/^assert_plan()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
for bounded_plan_marker in \
	"plan_after_checkpoint=\${8:-}" \
	'pause_controller_status_writes' \
	"checkpoint_schema_jobs \"\$plan_schema\" \"\$plan_after_checkpoint\"" \
	"\"\$observe_result_file\" \"\$plan_after_checkpoint\"" \
	"\"\$plan_result_file\" \"\$plan_after_checkpoint\""; do
	printf '%s\n' "$assert_plan_section" | grep -F "$bounded_plan_marker" >/dev/null
done
scheduled_tag_section=$(sed -n \
	"/scheduled tag proof lacks a status-write barrier/,/plan_v2=\$CURRENT_PLAN/p" \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
for scheduled_tag_marker in \
	'scheduled tag proof lacks a status-write barrier' \
	'the persisted mutable-tag polling deadline to elapse without a spec change' \
	"checkpoint_schema_jobs \"\$lifecycle_schema\" \"\$v2_checkpoint\"" \
	'v2_after_checkpoint=' \
	'assert_one_job_between_checkpoints' \
	'resume_controller_status_writes' \
	'scheduled tag refresh depended on a spec generation change'; do
	printf '%s\n' "$scheduled_tag_section" | grep -F "$scheduled_tag_marker" >/dev/null
done
if printf '%s\n' "$scheduled_tag_section" |
	grep -E 'patch ptahschema|resume_schema_after_tag_move' >/dev/null; then
	printf '%s\n' 'e2e static: scheduled tag discovery contains a PtahSchema spec mutation' >&2
	exit 1
fi
if printf '%s\n' "$scheduled_tag_section" |
	grep -F "assert_one_new_job \"\$lifecycle_schema\"" >/dev/null; then
	printf '%s\n' 'e2e static: scheduled tag discovery lacks an upper Job checkpoint' >&2
	exit 1
fi
static_reject_marker() {
	static_section=$1
	static_marker=$2
	static_context=$3
	if printf '%s\n' "$static_section" | grep -F -- "$static_marker" >/dev/null; then
		printf 'e2e static: %s contains forbidden marker %s\n' \
			"$static_context" "$static_marker" >&2
		exit 1
	fi
}
static_require_count() {
	static_section=$1
	static_marker=$2
	static_expected=$3
	static_context=$4
	static_actual=$(printf '%s\n' "$static_section" | grep -Fc -- "$static_marker" || true)
	[ "$static_actual" -eq "$static_expected" ] || {
		printf 'e2e static: %s has %s occurrences of %s, expected %s\n' \
			"$static_context" "$static_actual" "$static_marker" "$static_expected" >&2
		exit 1
	}
}

static_require_order() {
	static_section=$1
	static_context=$2
	shift 2
	static_after=0
	for static_marker do
		static_line=$(printf '%s\n' "$static_section" | grep -Fn -- "$static_marker" |
			awk -F: -v after="$static_after" '$1 > after { print $1; exit }')
		[ -n "$static_line" ] || {
			printf 'e2e static: %s lacks ordered marker %s after line %s\n' \
				"$static_context" "$static_marker" "$static_after" >&2
			exit 1
		}
		static_after=$static_line
	done
}

blocked_capture_section=$(sed -n '/^capture_blocked_refresh_boundary()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
for blocked_capture_forbidden in \
	'audit_completed_jobs' \
	'audit_runtime_credentials' \
	'wait_for_schema' \
	'wait_for_one_new_job' \
	'new_job_count_since' \
	'all_new_jobs_complete' \
	'checkpoint_jobs' \
	'pause_controller_status_writes'; do
	static_reject_marker "$blocked_capture_section" "$blocked_capture_forbidden" \
		'blocked refresh boundary before its Job checkpoint'
done
static_require_count "$blocked_capture_section" \
	"checkpoint_schema_jobs \"\$blocked_schema\" \"\$BLOCKED_GATE_CHECKPOINT\"" 1 \
	'blocked refresh atomic Job checkpoint'
static_require_order "$blocked_capture_section" 'blocked refresh stable boundary capture' \
	"blocked_capture_headroom=\$(((BLOCKED_REFRESH_SECONDS * 2 + 2) / 3))" \
	"blocked_post_checkpoint_headroom=\$((BLOCKED_REFRESH_SECONDS / 2))" \
	"resume_schema_after_tag_move \"\$blocked_schema\"" \
	'blocked_expected_generation=' \
	"blocked_candidate=\$(k -n \"\$TEST_NAMESPACE\" get ptahschema \"\$blocked_schema\"" \
	".status.observedGeneration == \$generation" \
	"((.status.nextReconciliationTime | fromdateiso8601) - \$now >= \$headroom)" \
	'blocked_persisted_deadline=' \
	'BLOCKED_GATE_CHECKPOINT=' \
	"checkpoint_schema_jobs \"\$blocked_schema\" \"\$BLOCKED_GATE_CHECKPOINT\"" \
	"assert_one_job_between_checkpoints \"\$blocked_schema\" \"\$blocked_operation\"" \
	"assert_no_job_between_checkpoints \"\$blocked_schema\" apply" \
	"blocked_stable_object=\$(k -n \"\$TEST_NAMESPACE\" get ptahschema \"\$blocked_schema\" -o json)" \
	".status.nextReconciliationTime == \$deadline" \
	"((.status.nextReconciliationTime | fromdateiso8601) - \$now >= \$headroom)" \
	'crossed or changed its persisted blocked refresh boundary during capture'
blocked_prepare_section=$(sed -n '/^prepare_blocked_refresh_cadence()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_order "$blocked_prepare_section" 'blocked refresh post-boundary audit' \
	"checkpoint_schema_jobs \"\$blocked_schema\" \"\$blocked_generation_checkpoint\"" \
	"capture_blocked_refresh_boundary \"\$blocked_schema\" \"\$blocked_generation_checkpoint\"" \
	'audit_runtime_credentials'
for blocked_prepare_forbidden in 'wait_for_schema' 'wait_for_one_new_job'; do
	static_reject_marker "$blocked_prepare_section" "$blocked_prepare_forbidden" \
		'blocked refresh preparation'
done
destructive_gate_section=$(sed -n '/^assert_destructive_gate()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
for destructive_exact_marker in \
	"\"\$gate_resolve_count\" -eq 3" \
	"\"\$gate_verify_count\" -eq 3" \
	"\"\$gate_observe_count\" -eq 3" \
	"\"\$gate_plan_count\" -eq 3" \
	"(\$new | length) == 12" \
	"(\$resolves | length) == 3" \
	"(\$verifies | length) == 3" \
	"(\$observes | length) == 3" \
	"(\$plans | length) == 3" \
	"(\$new | map(.operation)) =="; do
	static_require_count "$destructive_gate_section" "$destructive_exact_marker" 1 \
		'exact blocked refresh chains'
done
static_reject_marker "$destructive_gate_section" '[0:12]' \
	'exact blocked refresh chains'
blocked_diagnostic_section=$(sed -n '/^report_blocked_refresh_diagnostics()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
for blocked_diagnostic_marker in \
	'ObservedJobTimelineDiagnostic' \
	'def safe_operation:' \
	'operation_rank' \
	'{name, uid, operation: (.operation | safe_operation), created}' \
	'BLOCKED_REFRESH_DIAGNOSTIC_FILE' \
	'emit_scanned_cleanup_diagnostic' \
	'CREDENTIAL_PATTERNS_FILE'; do
	printf '%s\n' "$blocked_diagnostic_section" | grep -F "$blocked_diagnostic_marker" >/dev/null
done
for blocked_diagnostic_forbidden in \
	'.message' \
	'logs ' \
	' logs' \
	'secret' \
	'.data' \
	'.stringData' \
	'{name, uid, operation, created}' \
	'>&2'; do
	static_reject_marker "$blocked_diagnostic_section" "$blocked_diagnostic_forbidden" \
		'credential-free blocked refresh diagnostics'
done
cleanup_projection_section=$(sed -n '/^project_cleanup_diagnostic_files()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
for cleanup_projection_marker in \
	'def safe_code:' \
	'def safe_epoch:' \
	".involvedObject.uid == \$schema.metadata.uid" \
	'.reason == "PlanStale"' \
	'.reason == "LeaseContinuityLost"' \
	'.metadata.ownerReferences // [] | any(' \
	'operator.ptah.dev/coordination"] == "database-target"' \
	'holderPresent:' \
	'holderHashShape:'; do
	printf '%s\n' "$cleanup_projection_section" | grep -F "$cleanup_projection_marker" >/dev/null
done
for cleanup_projection_forbidden in \
	'message:' \
	'holderIdentity:' \
	'.spec.containers' \
	'.spec.initContainers' \
	'.data' \
	'.stringData'; do
	static_reject_marker "$cleanup_projection_section" "$cleanup_projection_forbidden" \
		'credential-safe cleanup projection'
done
cleanup_collect_section=$(sed -n '/^collect_credential_safe_diagnostics()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_order "$cleanup_collect_section" 'generic cleanup diagnostic pipeline' \
	'get ptahschemas -o json' \
	'get events -o json' \
	'get jobs -o json' \
	'get leases' \
	'project_cleanup_diagnostic_files' \
	'emit_scanned_cleanup_diagnostic'
for cleanup_collect_forbidden in \
	'fail ' \
	'scan_file_for_credentials' \
	' logs' \
	'describe ' \
	'get secret'; do
	static_reject_marker "$cleanup_collect_section" "$cleanup_collect_forbidden" \
		'nonfatal generic cleanup diagnostics'
done
collect_diagnostics_section=$(sed -n '/^collect_diagnostics()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_order "$collect_diagnostics_section" 'generic failure diagnostics' \
	'collect_credential_safe_diagnostics' \
	'get ptahschemas,ptahschemaplans,ptahschemaapprovals -o wide'
static_require_count "$destructive_gate_section" \
	"report_blocked_refresh_diagnostics \"\$gate_schema\" \"\$gate_refresh_checkpoint\"" 6 \
	'blocked refresh failure diagnostics'
static_require_order "$destructive_gate_section" 'strict blocked refresh overflow failure' \
	"\"\$gate_resolve_count\" -gt 3" \
	"report_blocked_refresh_diagnostics \"\$gate_schema\" \"\$gate_refresh_checkpoint\"" \
	'created work beyond three exact blocked refresh chains'
static_require_order "$destructive_gate_section" 'blocked refresh exact success checkpoint' \
	'gate_after_checkpoint=' \
	"checkpoint_schema_jobs \"\$gate_schema\" \"\$gate_after_checkpoint\"" \
	"gate_final_count=\$(job_count_between_checkpoints \"\$gate_schema\"" \
	"\"\$gate_refresh_checkpoint\" \"\$gate_after_checkpoint\")" \
	"[ \"\$gate_final_count\" -ne 3 ]" \
	'crossed the exact three-chain success boundary' \
	"gate_final_apply_count=\$(job_count_between_checkpoints \"\$gate_schema\" apply" \
	"[ \"\$gate_final_apply_count\" -eq 0 ]" \
	"gate_final_schema_count=\$(schema_job_count_between_checkpoints \"\$gate_schema\"" \
	"[ \"\$gate_final_schema_count\" -eq 12 ]" \
	'return 0'
static_reject_marker "$destructive_gate_section" \
	'select(.operation == "resolve" or .operation == "verify"' \
	'exact blocked refresh all-Job proof'

mysql_refusal_rewrite_section=$(sed -n '/^rewrite_mysql_refusal_job()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
mysql_refusal_script=$(sed -n '1,$p' "$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_count "$mysql_refusal_script" \
	'# mysql-refusal-job-rewrite-begin' 1 'MySQL refusal rewrite filter opening marker'
static_require_count "$mysql_refusal_script" \
	'# mysql-refusal-job-rewrite-end' 1 'MySQL refusal rewrite filter closing marker'
static_require_order "$mysql_refusal_rewrite_section" 'null-safe MySQL refusal Job rewrite' \
	'.template.spec.containers |= map(rewrite_env)' \
	'if (.template.spec | has("initContainers")) and' \
	'.template.spec.initContainers != null then' \
	'.template.spec.initContainers |= map(rewrite_env)' \
	'else' \
	'end)'
static_require_count "$mysql_refusal_rewrite_section" \
	'if (.template.spec | has("initContainers")) and' 1 \
	'MySQL refusal optional initContainers guard'
mysql_refusal_probe_section=$(sed -n \
	'/^assert_mysql_refusal_rewrite_without_init_containers()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_order "$mysql_refusal_probe_section" \
	'MySQL refusal rewrite missing-initContainers probe' \
	'rewrite_mysql_refusal_job test-namespace test-name test-schema observe' \
	'(.spec.template.spec | has("initContainers") | not)' \
	'.spec.backoffLimit == 7' \
	'.spec.template.spec.restartPolicy == "Never"' \
	'name: "PRESERVED", value: "exact"' \
	'changed absent initContainers or unrelated Pod semantics' \
	'refusal_null_rewrite_probe=' \
	'.spec.template.spec.initContainers == null' \
	'changed null initContainers or unrelated Pod semantics'
mysql_refusal_section=$(sed -n '/^run_mysql_dsn_refusal()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_order "$mysql_refusal_section" 'MySQL refusal Job rewrite use' \
	'assert_mysql_refusal_rewrite_without_init_containers' \
	"printf '%s\\n' \"\$refusal_source\"" \
	"rewrite_mysql_refusal_job \"\$TEST_NAMESPACE\" \"\$refusal_name\""

for runtime_audit_script in e2e-dataplane.sh e2e-faults.sh; do
	case "$runtime_audit_script" in
	e2e-dataplane.sh)
		runtime_audit_function=audit_runtime_credentials
		runtime_snapshot_variable=audit_pods_snapshot
		runtime_owner_uid_variable=audit_controller_job_uid
		runtime_job_ledger=FULLY_AUDITED_JOBS_FILE
		runtime_broad_job_ledger=AUDITED_JOBS_FILE
		runtime_generic_end='^}'
	;;
	e2e-faults.sh)
		runtime_audit_function=audit_fault_runtime
		runtime_snapshot_variable=fault_audit_pods
		runtime_owner_uid_variable=audit_pod_job_uid
		runtime_job_ledger=SHARED_FULLY_AUDITED_JOBS_FILE
		runtime_broad_job_ledger=SHARED_AUDITED_JOBS_FILE
		runtime_generic_end='^[[:space:]]*terminal_jobs='
	;;
	esac
	runtime_audit_function_section=$(sed -n \
		"/^${runtime_audit_function}()/,/^}/p" \
		"$ROOT_DIR/hack/$runtime_audit_script")
	runtime_audit_generic_section=$(printf '%s\n' "$runtime_audit_function_section" |
		sed -n "/^[[:space:]]*${runtime_snapshot_variable}=/,/${runtime_generic_end}/p")
	runtime_snapshot_marker="${runtime_snapshot_variable}=\$(k -n \"\$TEST_NAMESPACE\" get pods -o json)"
	static_require_count "$runtime_audit_generic_section" "$runtime_snapshot_marker" 1 \
		"$runtime_audit_function JSON Pod snapshot"
	static_require_count "$runtime_audit_generic_section" 'get pods -o json' 1 \
		"$runtime_audit_function generic Pod list"
	static_reject_marker "$runtime_audit_generic_section" 'get pods -o name' \
		"$runtime_audit_function generic Pod audit"
	static_reject_marker "$runtime_audit_generic_section" "for audit_pod in \$(" \
		"$runtime_audit_function generic Pod audit"
	runtime_ledger_marker="grep -Fx \"\$${runtime_owner_uid_variable}\" \"\$${runtime_job_ledger}\""
	runtime_terminal_phase_marker="if [ \"\$audit_snapshot_phase\" = Succeeded ] || [ \"\$audit_snapshot_phase\" = Failed ]; then"
	runtime_owner_uid_marker="[ \"\$${runtime_owner_uid_variable}\" != \"-\" ]"
	runtime_live_get_marker="audit_pod_object=\$(k -n \"\$TEST_NAMESPACE\" get pod \"\$audit_pod_name\""
	runtime_post_get_marker="audit_pod_after=\$(k -n \"\$TEST_NAMESPACE\" get pod \"\$audit_pod_name\""
	runtime_uid_marker=".metadata.uid == \$uid"
	runtime_object_write_marker="printf '%s\\n' \"\$audit_pod_object\" >\"\$RESOURCE_FILE\""
	runtime_skip_section=$(printf '%s\n' "$runtime_audit_generic_section" |
		sed -n "/^[[:space:]]*if \\[[[:space:]]*\"\\\$audit_snapshot_phase\" = Succeeded/,/audit_pod_object=/p")
	static_require_count "$runtime_skip_section" "$runtime_ledger_marker" 1 \
		"$runtime_audit_function full-ledger Pod skip"
	runtime_skip_greps=$(printf '%s\n' "$runtime_skip_section" | grep -F 'grep -' || true)
	static_reject_marker "$runtime_skip_greps" "\$${runtime_broad_job_ledger}" \
		"$runtime_audit_function broad Job-ledger authorization"
	case "$runtime_audit_script" in
	e2e-dataplane.sh)
		runtime_resource_scan_marker="scan_file_for_credentials \"\$RESOURCE_FILE\""
		runtime_log_scan_marker="scan_file_for_credentials \"\$LOG_FILE\""
		static_require_order "$runtime_audit_generic_section" "$runtime_audit_function full-ledger Pod audit" \
			"$runtime_snapshot_marker" '.metadata.ownerReferences' \
			'.apiVersion == "batch/v1" and .kind == "Job" and .controller == true' \
			'elif length == 1 then .[0]' \
			"$runtime_terminal_phase_marker" "$runtime_owner_uid_marker" \
			"$runtime_ledger_marker" 'continue' "$runtime_live_get_marker" \
			"fail \"unaudited exact Pod \$audit_pod_name UID \$audit_pod_uid disappeared before log audit\"" "$runtime_uid_marker" \
			"fail \"unaudited exact Pod \$audit_pod_name changed identity before its log audit\"" "$runtime_object_write_marker" \
			"$runtime_resource_scan_marker" "$runtime_log_scan_marker" \
			"$runtime_post_get_marker" "fail \"unaudited exact Pod \$audit_pod_name UID \$audit_pod_uid disappeared during log audit\"" \
			"$runtime_uid_marker" "fail \"unaudited exact Pod \$audit_pod_name changed identity during its log audit\""
	;;
	e2e-faults.sh)
		static_reject_marker "$runtime_skip_greps" "\$AUDITED_FAULT_PODS_FILE" \
			'fault generic broad Pod-ledger authorization'
		static_reject_marker "$runtime_skip_greps" "\$AUDITED_FAULT_JOBS_FILE" \
			'fault generic broad Job-ledger authorization'
		runtime_resource_scan_marker="scan_fault_file \"\$RESOURCE_FILE\""
		runtime_log_scan_marker="scan_fault_file \"\$LOG_FILE\""
		static_require_order "$runtime_audit_generic_section" "$runtime_audit_function full-ledger Pod audit" \
			"$runtime_snapshot_marker" '.metadata.ownerReferences' \
			'.apiVersion == "batch/v1" and .kind == "Job" and .controller == true' \
			'elif length == 1 then .[0]' \
			"$runtime_terminal_phase_marker" "$runtime_owner_uid_marker" \
			"$runtime_ledger_marker" \
			"record_audited_uid \"\$AUDITED_FAULT_PODS_FILE\" \"\$audit_pod_uid\"" \
			"record_audited_uid \"\$FULLY_AUDITED_FAULT_PODS_FILE\" \"\$audit_pod_uid\"" \
			"record_audited_uid \"\$AUDITED_FAULT_JOBS_FILE\" \"\$${runtime_owner_uid_variable}\"" \
			'continue' "$runtime_live_get_marker" \
			"fail \"unaudited fault-test Pod \$audit_pod_name UID \$audit_pod_uid disappeared before its log audit\"" "$runtime_uid_marker" \
			"fail \"unaudited fault-test Pod \$audit_pod_name was replaced before UID \$audit_pod_uid was audited\"" "$runtime_object_write_marker" \
			"$runtime_resource_scan_marker" "$runtime_log_scan_marker" \
			"$runtime_post_get_marker" "fail \"unaudited fault-test Pod \$audit_pod_name UID \$audit_pod_uid disappeared during its log audit\"" \
			"$runtime_uid_marker" "fail \"unaudited fault-test Pod \$audit_pod_name changed identity during its log audit\""
	;;
	esac
done
dataplane_script=$(sed -n '1,$p' "$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_order "$dataplane_script" 'data-plane full-audit ledger wiring' \
	"FULLY_AUDITED_JOBS_FILE=\$WORK_DIR/fully-audited-jobs.txt" \
	": >\"\$FULLY_AUDITED_JOBS_FILE\"" \
	"E2E_FULLY_AUDITED_JOBS_FILE=\$FULLY_AUDITED_JOBS_FILE"
observed_audit_section=$(sed -n '/^assert_observed_jobs_audited()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_require_count "$observed_audit_section" "grep -Fx \"\$observed_uid\" \"\$FULLY_AUDITED_JOBS_FILE\"" 1 \
	'observed Job full-audit assertion'
static_reject_marker "$observed_audit_section" \
	"grep -Fx \"\$observed_uid\" \"\$AUDITED_JOBS_FILE\"" \
	'observed Job broad-ledger assertion'
completed_job_audit_section=$(sed -n '/^audit_completed_jobs()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
completed_full_write_marker="printf '%s\\n' \"\$audit_uid\" >>\"\$FULLY_AUDITED_JOBS_FILE\""
static_require_order "$completed_job_audit_section" 'completed Job full-audit commit' \
	"grep -Fx \"\$audit_uid\" \"\$FULLY_AUDITED_JOBS_FILE\"" 'continue' \
	"audit_job_object=\$(k -n \"\$TEST_NAMESPACE\" get job \"\$audit_name\"" \
	"audit_owned_pods=\$(printf '%s\\n' \"\$audit_pods\"" "--arg uid \"\$audit_uid\"" \
	".uid == \$uid and .controller == true))" "printf '%s\\n%s\\n' \"\$audit_job_object\" \"\$audit_owned_pods\" >\"\$RESOURCE_FILE\"" \
	"scan_file_for_credentials \"\$RESOURCE_FILE\"" ".metadata.uid == \$podUID" \
	"fail \"exact Pod \$audit_pod_name UID \$audit_pod_uid lacks complete terminal evidence\"" "audit_containers=\$(printf '%s\\n' \"\$audit_pod_object\"" \
	"scan_file_for_credentials \"\$LOG_FILE\"" ".metadata.uid == \$podUID" \
	"fail \"exact Pod \$audit_pod_name changed identity during its log audit\"" ".metadata.uid == \$uid" \
	"fail \"terminal Job \$audit_name changed identity during its exact Pod audit\"" "$completed_full_write_marker"
static_require_count "$dataplane_script" \
	"$completed_full_write_marker" 1 'data-plane full Job write sites'
capture_result_section=$(sed -n '/^capture_one_new_job_result()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-dataplane.sh")
static_reject_marker "$capture_result_section" 'FULLY_AUDITED' \
	'result transport capture full-audit claim'
fault_runtime_audit_function_section=$(sed -n '/^audit_fault_runtime()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-faults.sh")
fault_script=$(sed -n '1,$p' "$ROOT_DIR/hack/e2e-faults.sh")
static_require_order "$fault_script" 'fault full-audit ledger initialization' \
	"SHARED_FULLY_AUDITED_JOBS_FILE=\${E2E_FULLY_AUDITED_JOBS_FILE:-}" \
	"FULLY_AUDITED_FAULT_PODS_FILE=\$WORK_DIR/fully-audited-pod-uids.txt" \
	": >\"\$FULLY_AUDITED_FAULT_PODS_FILE\""
fault_generic_audit_section=$(printf '%s\n' "$fault_runtime_audit_function_section" |
	sed -n '/^[[:space:]]*fault_audit_pods=/,/^[[:space:]]*terminal_jobs=/p')
fault_full_pod_record_marker="record_audited_uid \"\$FULLY_AUDITED_FAULT_PODS_FILE\" \"\$audit_pod_uid\""
static_require_count "$fault_generic_audit_section" "$fault_full_pod_record_marker" 2 \
	'fault generic full-Pod write sites'
fault_terminal_pod_arm=$(printf '%s\n' "$fault_generic_audit_section" |
	sed -n '/^[[:space:]]*Succeeded | Failed)/,/^[[:space:]]*;;/p')
static_require_count "$fault_terminal_pod_arm" "$fault_full_pod_record_marker" 1 \
	'fault terminal Pod-arm full-audit writes'
static_require_order "$fault_terminal_pod_arm" 'fault terminal Pod-arm promotion' \
	"\$declared == \$terminated" "$fault_full_pod_record_marker" ';;'
static_require_order "$fault_generic_audit_section" 'fault full-Pod commit' \
	"scan_fault_file \"\$LOG_FILE\"" \
	"audit_pod_after=\$(k -n \"\$TEST_NAMESPACE\" get pod \"\$audit_pod_name\"" \
	"\$declared == \$terminated" "$fault_full_pod_record_marker"
fault_capture_result_section=$(sed -n '/^capture_exact_job_result()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-faults.sh")
static_reject_marker "$fault_capture_result_section" 'FULLY_AUDITED' \
	'exact result capture full-audit claim'
fault_terminal_job_section=$(printf '%s\n' "$fault_runtime_audit_function_section" |
	sed -n '/^[[:space:]]*terminal_jobs=/,/^}/p')
fault_terminal_ledger_marker="grep -Fx \"\$audit_job_uid\" \"\$SHARED_FULLY_AUDITED_JOBS_FILE\""
fault_shared_full_write_marker="record_audited_uid \"\$SHARED_FULLY_AUDITED_JOBS_FILE\""
fault_terminal_skip_section=$(printf '%s\n' "$fault_terminal_job_section" |
	sed -n '/^[[:space:]]*if grep -Fx/,/audit_job_object=/p')
static_require_count "$fault_terminal_skip_section" "$fault_terminal_ledger_marker" 1 \
	'fault terminal full-ledger Job skip'
fault_terminal_skip_greps=$(printf '%s\n' "$fault_terminal_skip_section" |
	grep -F 'grep -' || true)
static_reject_marker "$fault_terminal_skip_greps" "\$AUDITED_FAULT_JOBS_FILE" \
	'fault terminal local broad-ledger authorization'
static_reject_marker "$fault_terminal_skip_greps" "\$SHARED_AUDITED_JOBS_FILE" \
	'fault terminal shared broad-ledger authorization'
static_require_order "$fault_terminal_job_section" 'fault full Job promotion' \
	"$fault_terminal_ledger_marker" \
	"record_audited_uid \"\$AUDITED_FAULT_JOBS_FILE\" \"\$audit_job_uid\"" \
	'continue' "audit_job_object=\$(k -n \"\$TEST_NAMESPACE\" get job \"\$audit_job_name\"" ".metadata.uid == \$uid" \
	"audit_job_pods=\$(k -n \"\$TEST_NAMESPACE\" get pods -o json" \
	".uid == \$uid and .controller == true))" \
	"grep -Fx \"\$audit_job_pod_uid\" \"\$FULLY_AUDITED_FAULT_PODS_FILE\"" \
	"printf '%s\\n' \"\$audit_job_object\" >\"\$RESOURCE_FILE\"" \
	"printf '%s\\n' \"\$audit_job_pods\" >>\"\$RESOURCE_FILE\"" \
	"scan_fault_file \"\$RESOURCE_FILE\"" ".metadata.uid == \$uid" \
	"fail \"terminal fault-test Job \$audit_job_name changed UID during its Pod audit\"" \
	"${fault_shared_full_write_marker} \"\$audit_job_uid\""
deadline_pending_section=$(sed -n '/^record_deadline_pending_pod_evidence()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-faults.sh")
static_require_order "$deadline_pending_section" 'deadline full-Pod evidence' \
	".metadata.uid == \$podUID and .metadata.deletionTimestamp == null" \
	".[0].uid == \$jobUID and .[0].controller == true" \
	'(.spec.nodeName // "") == ""' \
	'.state.running == null and .state.terminated == null' \
	"fail \"timeout Apply Pod \$deadline_pod_name lacks exact never-started pre-deadline evidence\"" \
	"printf '%s\\n' \"\$deadline_pod_object\" >\"\$RESOURCE_FILE\"" \
	"scan_fault_file \"\$RESOURCE_FILE\" \"the exact never-started pre-deadline Apply Pod\"" \
	"record_audited_uid \"\$FULLY_AUDITED_FAULT_PODS_FILE\" \"\$deadline_pod_uid\""
deadline_terminal_section=$(sed -n '/^wait_for_deadline_job_terminal_and_audit()/,/^}/p' \
	"$ROOT_DIR/hack/e2e-faults.sh")
static_require_order "$deadline_terminal_section" 'DeadlineExceeded full Job promotion' \
	"deadline_terminal_pod_uid=\$4" '.reason == "DeadlineExceeded"' ".metadata.uid == \$uid" \
	"printf '%s\\n' \"\$deadline_terminal_object\" >\"\$RESOURCE_FILE\"" \
	"scan_fault_file \"\$RESOURCE_FILE\" \"the exact DeadlineExceeded Apply Job\"" \
	"grep -Fx \"\$deadline_terminal_pod_uid\" \"\$FULLY_AUDITED_FAULT_PODS_FILE\"" \
	"${fault_shared_full_write_marker} \"\$deadline_terminal_uid\""
static_require_count "$fault_script" \
	"record_audited_uid \"\$FULLY_AUDITED_FAULT_PODS_FILE\"" 3 \
	'fault full-Pod write sites'
static_require_count "$fault_script" "$fault_shared_full_write_marker" 2 \
	'fault shared full-Job write sites'
static_require_count "$fault_runtime_audit_function_section" \
	"$fault_shared_full_write_marker" 1 'fault runtime full-Job writes'
static_require_count "$deadline_terminal_section" "$fault_shared_full_write_marker" 1 \
	'deadline full-Job writes'
deadline_call_section=$(sed -n \
	"/^wait_for_deadline_job_terminal_and_audit \"\\\$MYSQL_TIMEOUT_JOB_NAME\"/,/^wait_for_exact_pod_absence_after_evidence/p" \
	"$ROOT_DIR/hack/e2e-faults.sh")
static_require_order "$deadline_call_section" 'exact deadline call path' \
	"\"\$MYSQL_TIMEOUT_POD_UID\"" \
	"wait_for_exact_pod_absence_after_evidence \"\$MYSQL_TIMEOUT_POD_NAME\""
static_reject_marker "$fault_script" 'wait_for_exact_pod_absence_without_audit' \
	'deadline evidence helper naming'
alias_b_cascade_section=$(sed -n \
	"/^# Keep a consumed, user-owned approval/,/delete ptahschema \"\\\$PG_ALIAS_SCHEMA_B\"/p" \
	"$ROOT_DIR/hack/e2e-faults.sh")
static_require_order "$alias_b_cascade_section" 'shared-alias cascade boundary' \
	'audit_fault_runtime' \
	"\"\$ALIAS_B_JOB_UID\" \"\$ALIAS_B_RECOVERY_OBSERVE_UID\" \"\$ALIAS_B_RECOVERY_PLAN_UID\"" \
	"[ -n \"\$alias_b_fully_audited_job_uid\" ]" \
	"grep -Fx \"\$alias_b_fully_audited_job_uid\" \"\$SHARED_FULLY_AUDITED_JOBS_FILE\"" \
	"delete ptahschema \"\$PG_ALIAS_SCHEMA_B\""
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
	'three complete scheduled blocked refresh cycles' \
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
