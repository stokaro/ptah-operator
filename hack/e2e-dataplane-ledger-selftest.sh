#!/bin/sh
# shellcheck disable=SC2034,SC2329 # Extracted helpers consume fixture globals and command stubs dynamically.

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
SOURCE_FILE=$ROOT_DIR/hack/e2e-dataplane.sh
FAULT_SOURCE_FILE=$ROOT_DIR/hack/e2e-faults.sh
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-ledger-selftest.XXXXXX")
FUNCTIONS_FILE=$WORK_DIR/functions.sh
ERROR_FILE=$WORK_DIR/error.txt
OUTPUT_FILE=$WORK_DIR/output.txt

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-operator-ledger-selftest.*) rm -rf -- "$WORK_DIR" ;;
	*)
		printf 'e2e ledger self-test: refusing to remove unexpected work directory %s\n' \
			"$WORK_DIR" >&2
		status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

test_fail() {
	printf 'e2e ledger self-test: %s\n' "$*" >&2
	exit 1
}

for command_name in jq sed grep mktemp awk stat find cut wc tr ln mv cp chmod; do
	command -v "$command_name" >/dev/null 2>&1 ||
		test_fail "required command is not installed: $command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	test_fail "sha256sum or shasum is required"
fi

: >"$FUNCTIONS_FILE"
for function_name in \
	sha256 require_mode_0600_regular_file require_mode_0700_directory job_evidence_key \
	scan_file_for_credentials \
	validate_job_evidence_directory validate_completed_job_evidence \
	validate_supplied_job_evidence_identity assert_existing_job_evidence_matches_supplied \
	publish_completed_job_evidence \
	assert_live_job_evidence_consistent \
	k record_observed_jobs assert_observed_jobs_audited new_job_count_since assert_no_new_jobs \
	assert_schema_job_boundary_unchanged \
	materialize_archived_schema_jobs \
	materialize_terminal_job_records materialize_owned_pod_records materialize_manager_pod_names \
	all_new_jobs_complete capture_selected_job_result; do
	function_section=$(sed -n "/^${function_name}()/,/^}/p" "$SOURCE_FILE")
	[ -n "$function_section" ] || test_fail "could not extract $function_name"
	printf '%s\n' "$function_section" >>"$FUNCTIONS_FILE" ||
		test_fail "could not stage $function_name"
done
for function_name in \
	assert_fault_audit_complete record_fault_jobs_for_parent record_initial_job_list_for_parent \
	materialize_fault_manager_pod_names materialize_fault_terminal_job_records \
	materialize_fault_job_pod_uids; do
	function_section=$(sed -n "/^${function_name}()/,/^}/p" "$FAULT_SOURCE_FILE")
	[ -n "$function_section" ] || test_fail "could not extract fault helper $function_name"
	printf '%s\n' "$function_section" >>"$FUNCTIONS_FILE" ||
		test_fail "could not stage fault helper $function_name"
done

# shellcheck source=/dev/null
. "$FUNCTIONS_FILE"

TEST_NAMESPACE=ledger-self-test
KUBECONFIG_FILE=$WORK_DIR/kubeconfig
OBSERVED_JOBS_FILE=$WORK_DIR/observed-jobs.jsonl
OBSERVED_JOBS_SNAPSHOT_FILE=$WORK_DIR/observed-jobs-snapshot.json
OBSERVED_JOB_RECORDS_FILE=$WORK_DIR/observed-job-records.jsonl
OBSERVED_JOB_RECORD_FILE=$WORK_DIR/observed-job-record.json
OBSERVED_JOB_UIDS_FILE=$WORK_DIR/observed-job-uids.txt
NEW_JOB_COUNT_FILE=$WORK_DIR/new-job-count.txt
TERMINAL_JOB_RECORDS_FILE=$WORK_DIR/terminal-job-records.tsv
OWNED_POD_RECORDS_FILE=$WORK_DIR/owned-pod-records.tsv
MANAGER_POD_NAMES_FILE=$WORK_DIR/manager-pod-names.txt
COMPLETE_JOB_RECORDS_FILE=$WORK_DIR/complete-job-records.json
COMPLETE_JOB_LINES_FILE=$WORK_DIR/complete-job-records.jsonl
FULLY_AUDITED_JOBS_FILE=$WORK_DIR/fully-audited-jobs.txt
JOB_EVIDENCE_DIR=$WORK_DIR/job-evidence
JOB_EVIDENCE_ENTRIES_FILE=$WORK_DIR/job-evidence-entries.txt
LIVE_JOB_EVIDENCE_ERROR_FILE=$WORK_DIR/live-job-evidence-api-error.txt
CREDENTIAL_PATTERNS_FILE=$WORK_DIR/credential-patterns.txt
ARCHIVED_SCHEMA_JOB_RECORDS_FILE=$WORK_DIR/archived-schema-job-records.jsonl
ARCHIVED_SCHEMA_JOB_LINES_FILE=$WORK_DIR/archived-schema-job-lines.jsonl
CHECKPOINT_FILE=$WORK_DIR/checkpoint.json
AUDITED_FAULT_JOBS_FILE=$WORK_DIR/fault-audited-jobs.txt
AUDITED_FAULT_PODS_FILE=$WORK_DIR/fault-audited-pods.txt
SHARED_OBSERVED_JOBS_FILE=$WORK_DIR/shared-observed-jobs.jsonl
INITIAL_FAULT_JOBS_FILE=$WORK_DIR/initial-fault-jobs.json
FAULT_MANAGER_POD_NAMES_FILE=$WORK_DIR/fault-manager-pod-names.txt
FAULT_TERMINAL_JOB_RECORDS_FILE=$WORK_DIR/fault-terminal-job-records.tsv
FAULT_JOB_POD_UIDS_FILE=$WORK_DIR/fault-job-pod-uids.txt
BOUNDARY_BEFORE_FILE=$WORK_DIR/boundary-before.json
BOUNDARY_EXPECTED_FILE=$WORK_DIR/boundary-expected.json
BOUNDARY_ACTUAL_FILE=$WORK_DIR/boundary-actual.json
ARCHIVED_JOBS_OUTPUT=$WORK_DIR/archived-jobs.json
ARCHIVED_UIDS_OUTPUT=$WORK_DIR/archived-uids.json
TEST_OPERATION_ID=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TEST_CREDENTIAL_PATTERN=credential-selftest-pattern-9f3d14c2

mkdir "$JOB_EVIDENCE_DIR"
chmod 700 "$JOB_EVIDENCE_DIR"
: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"
printf '%s\n' "$TEST_CREDENTIAL_PATTERN" >"$CREDENTIAL_PATTERNS_FILE"

fail() {
	printf 'fixture failure: %s\n' "$*" >&2
	exit 97
}

reset_fixture() {
	chmod 700 "$JOB_EVIDENCE_DIR"
	printf '%s\n' "$TEST_CREDENTIAL_PATTERN" >"$CREDENTIAL_PATTERNS_FILE"
	: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"
	: >"$OBSERVED_JOBS_FILE"
	: >"$FULLY_AUDITED_JOBS_FILE"
	: >"$AUDITED_FAULT_JOBS_FILE"
	: >"$AUDITED_FAULT_PODS_FILE"
	: >"$SHARED_OBSERVED_JOBS_FILE"
	: >"$WORK_DIR/watch-jobs.jsonl"
	: >"$WORK_DIR/watch-pods.jsonl"
	: >"$TERMINAL_JOB_RECORDS_FILE"
	: >"$OWNED_POD_RECORDS_FILE"
	: >"$MANAGER_POD_NAMES_FILE"
	: >"$COMPLETE_JOB_RECORDS_FILE"
	: >"$COMPLETE_JOB_LINES_FILE"
	: >"$FAULT_MANAGER_POD_NAMES_FILE"
	: >"$FAULT_TERMINAL_JOB_RECORDS_FILE"
	: >"$FAULT_JOB_POD_UIDS_FILE"
	find "$JOB_EVIDENCE_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
	printf '%s\n' '[]' >"$CHECKPOINT_FILE"
	printf '%s\n' '{"apiVersion":"batch/v1","kind":"JobList","items":[]}' >"$INITIAL_FAULT_JOBS_FILE"
}

write_valid_job_evidence() {
	fixture_uid=$1
	fixture_schema=$2
	fixture_operation=$3
	fixture_key=$(job_evidence_key "$fixture_uid")
	fixture_archive=$JOB_EVIDENCE_DIR/$fixture_key
	fixture_schema_uid=schema-uid-$fixture_schema
	fixture_job_name=job-$fixture_operation
	fixture_pod_name=${fixture_job_name}-pod
	fixture_pod_uid=pod-$fixture_uid
	fixture_operation_label=$(printf '%s' "$TEST_OPERATION_ID" | sha256 | cut -c1-16)
	mkdir "$fixture_archive"
	chmod 700 "$fixture_archive"
	jq -n \
		--arg uid "$fixture_uid" \
		--arg name "$fixture_job_name" \
		--arg schema "$fixture_schema" \
		--arg schemaUID "$fixture_schema_uid" \
		--arg operation "$fixture_operation" \
		--arg operationID "$TEST_OPERATION_ID" \
		--arg operationLabel "$fixture_operation_label" '
      {
        apiVersion: "batch/v1", kind: "Job",
        metadata: {
          uid: $uid, name: $name,
          labels: {
            "app.kubernetes.io/managed-by": "ptah-operator",
            "app.kubernetes.io/component": "schema-operation",
            "operator.ptah.dev/schema": $schema,
            "operator.ptah.dev/operation": $operation,
            "operator.ptah.dev/operation-id": $operationLabel
          },
          annotations: {"operator.ptah.dev/operation-id": $operationID},
		  ownerReferences: [{
			apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
			uid: $schemaUID, name: $schema, controller: true
          }]
        },
        spec: {
          backoffLimit: 0,
          podReplacementPolicy: "Failed",
          template: {metadata: {
            labels: {
              "operator.ptah.dev/schema": $schema,
              "operator.ptah.dev/operation": $operation,
              "operator.ptah.dev/operation-id": $operationLabel
            },
            annotations: {"operator.ptah.dev/operation-id": $operationID}
          }}
        },
        status: {
          startTime: "2026-01-01T00:00:00Z",
          completionTime: "2026-01-01T00:00:01Z",
          conditions: [{type: "Complete", status: "True"}]
        }
      }
    ' >"$fixture_archive/job.json"
	jq -n \
		--arg uid "$fixture_pod_uid" \
		--arg name "$fixture_pod_name" \
		--arg jobUID "$fixture_uid" \
		--arg jobName "$fixture_job_name" \
		--arg schema "$fixture_schema" \
		--arg operation "$fixture_operation" \
		--arg operationID "$TEST_OPERATION_ID" \
		--arg operationLabel "$fixture_operation_label" '
      {
        apiVersion: "v1", kind: "Pod",
        metadata: {
          uid: $uid, name: $name, generateName: ($jobName + "-"),
          labels: {
            "operator.ptah.dev/schema": $schema,
            "operator.ptah.dev/operation": $operation,
            "operator.ptah.dev/operation-id": $operationLabel
          },
          annotations: {"operator.ptah.dev/operation-id": $operationID},
          ownerReferences: [{
            apiVersion: "batch/v1", kind: "Job", uid: $jobUID,
            name: $jobName, controller: true
          }]
        },
        status: {
          phase: "Succeeded",
          initContainerStatuses: [],
          containerStatuses: [{
            name: "ptah", restartCount: 0,
            state: {terminated: {exitCode: 0}}
          }]
        }
      }
    ' >"$fixture_archive/pod.json"
	printf '%s\n' 'fixture raw transport' >"$fixture_archive/ptah.log"
	jq -n \
		--arg operation "$fixture_operation" \
		--arg operationID "$TEST_OPERATION_ID" '
      {
        protocolVersion: 5,
        operation: $operation,
        operationId: $operationID,
        truncation: null,
        error: null,
        childExitCode: 0,
        stdout: ""
      }
    ' >"$fixture_archive/result.json"
	fixture_job_digest=$(sha256 <"$fixture_archive/job.json")
	fixture_pod_digest=$(sha256 <"$fixture_archive/pod.json")
	fixture_log_digest=$(sha256 <"$fixture_archive/ptah.log")
	fixture_result_digest=$(sha256 <"$fixture_archive/result.json")
	jq -n \
		--arg key "$fixture_key" \
		--arg schema "$fixture_schema" \
		--arg schemaUID "$fixture_schema_uid" \
		--arg operation "$fixture_operation" \
		--arg operationID "$TEST_OPERATION_ID" \
		--arg jobUID "$fixture_uid" \
		--arg jobName "$fixture_job_name" \
		--arg podUID "$fixture_pod_uid" \
		--arg podName "$fixture_pod_name" \
		--arg jobDigest "$fixture_job_digest" \
		--arg podDigest "$fixture_pod_digest" \
		--arg logDigest "$fixture_log_digest" \
		--arg resultDigest "$fixture_result_digest" '
      {
        archiveVersion: 1,
        pathKey: $key,
        schema: $schema,
        operation: $operation,
        operationID: $operationID,
        job: {
          uid: $jobUID, name: $jobName,
          owner: {
            apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchema",
            uid: $schemaUID, name: $schema, controller: true
          }
        },
        pod: {
          uid: $podUID, name: $podName,
          owner: {
            apiVersion: "batch/v1", kind: "Job", uid: $jobUID,
            name: $jobName, controller: true
          }
        },
        digests: {
          jobSHA256: $jobDigest,
          podSHA256: $podDigest,
          rawLogSHA256: $logDigest,
          resultSHA256: $resultDigest
        }
      }
    ' >"$fixture_archive/manifest.json"
	chmod 600 "$fixture_archive/job.json" "$fixture_archive/pod.json" \
		"$fixture_archive/ptah.log" "$fixture_archive/result.json" \
		"$fixture_archive/manifest.json"
	printf '%s\n' "$fixture_archive"
}

require_ignore_not_found_argument() {
	case " $* " in
	*' --ignore-not-found '*) ;;
	*) test_fail "live consistency read omitted --ignore-not-found" ;;
	esac
}

emit_live_job() {
	live_fixture_uid=$1
	live_fixture_name=$2
	live_fixture_operation_id=$3
	jq -cn \
		--arg uid "$live_fixture_uid" \
		--arg name "$live_fixture_name" \
		--arg operationID "$live_fixture_operation_id" '
      {
        apiVersion: "batch/v1", kind: "Job",
        metadata: {
          uid: $uid, name: $name,
          annotations: {"operator.ptah.dev/operation-id": $operationID}
        }
      }
    '
}

emit_live_pod() {
	live_fixture_pod_uid=$1
	live_fixture_pod_name=$2
	live_fixture_job_uid=$3
	live_fixture_job_name=$4
	jq -cn \
		--arg podUID "$live_fixture_pod_uid" \
		--arg podName "$live_fixture_pod_name" \
		--arg jobUID "$live_fixture_job_uid" \
		--arg jobName "$live_fixture_job_name" '
      {
        apiVersion: "v1", kind: "Pod",
        metadata: {
          uid: $podUID, name: $podName,
          ownerReferences: [{
            apiVersion: "batch/v1", kind: "Job",
            uid: $jobUID, name: $jobName, controller: true
          }]
        }
      }
    '
}

emit_one_job_list() {
	printf '%s\n' '{"apiVersion":"batch/v1","kind":"JobList","items":[{"metadata":{"uid":"uid-1","name":"job-1","creationTimestamp":"2026-01-01T00:00:00Z","labels":{"operator.ptah.dev/schema":"schema-1","operator.ptah.dev/operation":"plan"}}}]}'
}

emit_empty_job_list() {
	printf '%s\n' '{"apiVersion":"batch/v1","kind":"JobList","items":[]}'
}

expect_failure() {
	failure_description=$1
	expected_error=$2
	shift 2
	: >"$ERROR_FILE"
	: >"$OUTPUT_FILE"
	if ("$@") >"$OUTPUT_FILE" 2>"$ERROR_FILE"; then
		test_fail "$failure_description unexpectedly succeeded"
	fi
	grep -F "$expected_error" "$ERROR_FILE" >/dev/null || {
		printf 'e2e ledger self-test: %s failed without expected diagnostic: %s\n' \
			"$failure_description" "$expected_error" >&2
		exit 1
	}
}

expect_failure_without_leak() {
	failure_description=$1
	expected_error=$2
	forbidden_error=$3
	shift 3
	expect_failure "$failure_description" "$expected_error" "$@"
	if grep -F "$forbidden_error" \
		"$ERROR_FILE" "$OUTPUT_FILE" "$LIVE_JOB_EVIDENCE_ERROR_FILE" >/dev/null; then
		test_fail "$failure_description leaked raw API stderr"
	fi
}

# These failure fixtures are invoked indirectly by name through expect_failure below.
# Keep each suppression scoped to the corresponding fixture body.
# shellcheck disable=SC2317
record_with_kubectl_failure() (
	reset_fixture
	kubectl() {
		return 41
	}
	record_observed_jobs
)

# shellcheck disable=SC2317
record_with_jq_failure() (
	reset_fixture
	kubectl() {
		emit_one_job_list
	}
	jq() {
		return 42
	}
	record_observed_jobs
)

# shellcheck disable=SC2317
audit_with_jq_failure() (
	reset_fixture
	printf '%s\n' '{"uid":"uid-1","name":"job-1","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	jq() {
		return 43
	}
	assert_observed_jobs_audited
)

# shellcheck disable=SC2317
audit_with_missing_full_evidence() (
	reset_fixture
	printf '%s\n' '{"uid":"uid-1","name":"job-1","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	assert_observed_jobs_audited
)

# shellcheck disable=SC2317
count_with_jq_failure() (
	reset_fixture
	kubectl() {
		emit_one_job_list
	}
	jq() {
		for jq_argument do
			if [ "$jq_argument" = "$OBSERVED_JOBS_FILE" ]; then
				return 44
			fi
		done
		command jq "$@"
	}
	assert_no_new_jobs schema-1 apply "$CHECKPOINT_FILE"
)

# shellcheck disable=SC2317
fault_audit_with_jq_failure() (
	reset_fixture
	printf '%s\n' '{"type":"ADDED","object":{"metadata":{"uid":"fault-job-1"}}}' \
		>"$WORK_DIR/watch-jobs.jsonl"
	jq() {
		return 45
	}
	assert_fault_audit_complete
)

# shellcheck disable=SC2317
fault_audit_with_malformed_watch() (
	reset_fixture
	printf '%s\n' '{' >"$WORK_DIR/watch-jobs.jsonl"
	assert_fault_audit_complete
)

# shellcheck disable=SC2317
fault_audit_with_invalid_uid() (
	reset_fixture
	printf '%s\n' '{"type":"ADDED","object":{"metadata":{"uid":"fault\njob"}}}' \
		>"$WORK_DIR/watch-jobs.jsonl"
	assert_fault_audit_complete
)

# shellcheck disable=SC2317
fault_record_with_jq_failure() (
	reset_fixture
	printf '%s\n' '{"type":"ADDED","object":{"metadata":{"uid":"fault-job-1","name":"job-1"}}}' \
		>"$WORK_DIR/watch-jobs.jsonl"
	jq() {
		return 46
	}
	record_fault_jobs_for_parent
)

# shellcheck disable=SC2317
fault_record_with_malformed_watch() (
	reset_fixture
	printf '%s\n' '{' >"$WORK_DIR/watch-jobs.jsonl"
	record_fault_jobs_for_parent
)

# shellcheck disable=SC2317
fault_initial_with_malformed_list() (
	reset_fixture
	printf '%s\n' '{"items":' >"$INITIAL_FAULT_JOBS_FILE"
	record_initial_job_list_for_parent "$INITIAL_FAULT_JOBS_FILE"
)

# shellcheck disable=SC2317
terminal_job_projection_with_jq_failure() (
	reset_fixture
	jq() {
		return 47
	}
	materialize_terminal_job_records '{"items":[]}'
)

# shellcheck disable=SC2317
owned_pod_projection_with_jq_failure() (
	reset_fixture
	jq() {
		return 48
	}
	materialize_owned_pod_records '{"items":[]}'
)

# shellcheck disable=SC2317
manager_pod_projection_with_jq_failure() (
	reset_fixture
	jq() {
		return 49
	}
	materialize_manager_pod_names '{"items":[]}'
)

# shellcheck disable=SC2317
fault_manager_pod_projection_with_jq_failure() (
	reset_fixture
	jq() {
		return 50
	}
	materialize_fault_manager_pod_names '{"items":[]}'
)

# shellcheck disable=SC2317
fault_terminal_job_projection_with_jq_failure() (
	reset_fixture
	jq() {
		return 51
	}
	materialize_fault_terminal_job_records '{"items":[]}'
)

# shellcheck disable=SC2317
fault_job_pod_projection_with_jq_failure() (
	reset_fixture
	jq() {
		return 52
	}
	materialize_fault_job_pod_uids '{"items":[]}'
)

# shellcheck disable=SC2317
complete_job_projection_with_jq_failure() (
	reset_fixture
	record_observed_jobs() {
		:
	}
	jq() {
		return 53
	}
	all_new_jobs_complete schema-1 plan "$CHECKPOINT_FILE" 1
)

# shellcheck disable=SC2317
complete_job_lines_with_jq_failure() (
	reset_fixture
	record_observed_jobs() {
		:
	}
	printf '%s\n' \
		'{"uid":"uid-1","name":"job-1","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	jq() {
		for jq_argument do
			if [ "$jq_argument" = "$COMPLETE_JOB_RECORDS_FILE" ]; then
				case "$*" in
				*'complete Job records must be an array'*) command jq "$@" ;;
				*) return 54 ;;
				esac
				return
			fi
		done
		command jq "$@"
	}
	all_new_jobs_complete schema-1 plan "$CHECKPOINT_FILE" 1
)

# shellcheck disable=SC2317 # Extracted helper invokes these test-local stubs dynamically.
selected_job_missing_uid() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-other","name":"job-other","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	record_observed_jobs() {
		:
	}
	capture_one_new_job_result() {
		test_fail "missing selected UID reached result capture"
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
selected_job_missing_archive() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-selected","name":"job-selected","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	record_observed_jobs() {
		:
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
selected_job_partial_archive() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-selected","name":"job-plan","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	mv "$fixture_archive/manifest.json" "$WORK_DIR/partial-manifest.json"
	record_observed_jobs() {
		:
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
selected_job_identity_mismatch() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-selected","name":"job-plan","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	jq '.schema = "schema-other"' "$fixture_archive/manifest.json" \
		>"$WORK_DIR/mismatched-manifest.json"
	chmod 600 "$WORK_DIR/mismatched-manifest.json"
	mv "$WORK_DIR/mismatched-manifest.json" "$fixture_archive/manifest.json"
	record_observed_jobs() {
		:
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
selected_job_hash_mismatch() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-selected","name":"job-plan","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	printf '%s\n' 'post-publication mutation' >>"$fixture_archive/result.json"
	record_observed_jobs() {
		:
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
selected_job_schema_owner_uid_mismatch() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	jq '.metadata.ownerReferences[0].uid = "schema-uid-conflicting"' \
		"$fixture_archive/job.json" >"$WORK_DIR/schema-owner-job.json"
	chmod 600 "$WORK_DIR/schema-owner-job.json"
	mv "$WORK_DIR/schema-owner-job.json" "$fixture_archive/job.json"
	fixture_job_digest=$(sha256 <"$fixture_archive/job.json")
	jq --arg digest "$fixture_job_digest" '.digests.jobSHA256 = $digest' \
		"$fixture_archive/manifest.json" >"$WORK_DIR/schema-owner-manifest.json"
	chmod 600 "$WORK_DIR/schema-owner-manifest.json"
	mv "$WORK_DIR/schema-owner-manifest.json" "$fixture_archive/manifest.json"
	validate_completed_job_evidence schema-1 plan uid-selected
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
selected_job_symlink_archive() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-selected","name":"job-plan","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	mv "$fixture_archive/result.json" "$WORK_DIR/symlink-result.json"
	ln -s "$WORK_DIR/symlink-result.json" "$fixture_archive/result.json"
	record_observed_jobs() {
		:
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
job_evidence_root_wrong_mode() (
	reset_fixture
	write_valid_job_evidence uid-selected schema-1 plan >/dev/null
	chmod 755 "$JOB_EVIDENCE_DIR"
	validate_completed_job_evidence schema-1 plan uid-selected
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
job_evidence_archive_wrong_mode() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	chmod 755 "$fixture_archive"
	validate_completed_job_evidence schema-1 plan uid-selected
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
job_evidence_file_wrong_mode() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	chmod 644 "$fixture_archive/ptah.log"
	validate_completed_job_evidence schema-1 plan uid-selected
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
job_evidence_sixth_file_collision() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	printf '%s\n' 'colliding evidence' >"$fixture_archive/collision.txt"
	chmod 600 "$fixture_archive/collision.txt"
	validate_completed_job_evidence schema-1 plan uid-selected
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
job_evidence_directory_symlink() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	fixture_target=$WORK_DIR/evidence-symlink-target
	mv "$fixture_archive" "$fixture_target"
	ln -s "$fixture_target" "$fixture_archive"
	validate_completed_job_evidence schema-1 plan uid-selected
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
job_evidence_credential_escape() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	printf '%s\n' "$TEST_CREDENTIAL_PATTERN" >>"$fixture_archive/ptah.log"
	fixture_log_digest=$(sha256 <"$fixture_archive/ptah.log")
	jq --arg digest "$fixture_log_digest" '.digests.rawLogSHA256 = $digest' \
		"$fixture_archive/manifest.json" >"$WORK_DIR/credential-manifest.json"
	chmod 600 "$WORK_DIR/credential-manifest.json"
	mv "$WORK_DIR/credential-manifest.json" "$fixture_archive/manifest.json"
	validate_completed_job_evidence schema-1 plan uid-selected
)

live_job_and_pod_exact_successful_path() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	kubectl() {
		require_ignore_not_found_argument "$@"
		case " $* " in
		*' get job job-plan '*) emit_live_job uid-selected job-plan "$TEST_OPERATION_ID" ;;
		*' get pod job-plan-pod '*) emit_live_pod pod-uid-selected job-plan-pod uid-selected job-plan ;;
		*) test_fail "unexpected live consistency read: $*" ;;
		esac
	}
	assert_live_job_evidence_consistent \
		"$fixture_archive" job-plan uid-selected job-plan-pod pod-uid-selected \
		"$TEST_OPERATION_ID"
)

live_job_and_pod_exact_gc_absence_successful_path() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	live_calls=$WORK_DIR/live-gc-calls.txt
	: >"$live_calls"
	kubectl() {
		require_ignore_not_found_argument "$@"
		printf '%s\n' "$*" >>"$live_calls"
		return 0
	}
	assert_live_job_evidence_consistent \
		"$fixture_archive" job-plan uid-selected job-plan-pod pod-uid-selected \
		"$TEST_OPERATION_ID"
	[ "$(wc -l <"$live_calls" | tr -d '[:space:]')" -eq 2 ] ||
		test_fail "exact GC absence did not read both archived object identities"
	grep -F ' get job job-plan ' "$live_calls" >/dev/null ||
		test_fail "exact GC absence omitted the archived Job identity"
	grep -F ' get pod job-plan-pod ' "$live_calls" >/dev/null ||
		test_fail "exact GC absence omitted the archived Pod identity"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
live_job_api_failure() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	kubectl() {
		require_ignore_not_found_argument "$@"
		printf '%s\n' 'sensitive-forbidden-api-detail-71c8' >&2
		return 41
	}
	assert_live_job_evidence_consistent \
		"$fixture_archive" job-plan uid-selected job-plan-pod pod-uid-selected \
		"$TEST_OPERATION_ID"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
live_pod_api_failure() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	kubectl() {
		require_ignore_not_found_argument "$@"
		case " $* " in
		*' get job job-plan '*) emit_live_job uid-selected job-plan "$TEST_OPERATION_ID" ;;
		*' get pod job-plan-pod '*)
			printf '%s\n' 'sensitive-timeout-api-detail-3b42' >&2
			return 42
			;;
		*) test_fail "unexpected live consistency read: $*" ;;
		esac
	}
	assert_live_job_evidence_consistent \
		"$fixture_archive" job-plan uid-selected job-plan-pod pod-uid-selected \
		"$TEST_OPERATION_ID"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
live_job_identity_conflict() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	kubectl() {
		require_ignore_not_found_argument "$@"
		emit_live_job uid-conflicting job-plan "$TEST_OPERATION_ID"
	}
	assert_live_job_evidence_consistent \
		"$fixture_archive" job-plan uid-selected job-plan-pod pod-uid-selected \
		"$TEST_OPERATION_ID"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
live_pod_identity_conflict() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	kubectl() {
		require_ignore_not_found_argument "$@"
		case " $* " in
		*' get job job-plan '*) emit_live_job uid-selected job-plan "$TEST_OPERATION_ID" ;;
		*' get pod job-plan-pod '*) emit_live_pod pod-conflicting job-plan-pod uid-other job-other ;;
		*) test_fail "unexpected live consistency read: $*" ;;
		esac
	}
	assert_live_job_evidence_consistent \
		"$fixture_archive" job-plan uid-selected job-plan-pod pod-uid-selected \
		"$TEST_OPERATION_ID"
)

selected_job_gc_fallback_successful_path() (
	reset_fixture
	printf '%s\n' \
		'{"uid":"uid-selected","name":"job-plan","created":"2026-01-01T00:00:00Z","schema":"schema-1","operation":"plan"}' \
		>"$OBSERVED_JOBS_FILE"
	fixture_archive=$(write_valid_job_evidence uid-selected schema-1 plan)
	record_observed_jobs() {
		:
	}
	live_calls=$WORK_DIR/selected-gc-calls.txt
	: >"$live_calls"
	kubectl() {
		require_ignore_not_found_argument "$@"
		printf '%s\n' "$*" >>"$live_calls"
		return 0
	}
	capture_selected_job_result schema-1 plan uid-selected "$OUTPUT_FILE"
	[ "$(wc -l <"$live_calls" | tr -d '[:space:]')" -eq 2 ] ||
		test_fail "GC fallback did not establish exact Job and Pod absence"
	[ "$CAPTURED_JOB_UID" = uid-selected ] ||
		test_fail "GC fallback did not preserve the archived Job UID"
	[ "$CAPTURED_POD_UID" = pod-uid-selected ] ||
		test_fail "GC fallback did not preserve the archived Pod UID"
	[ "$CAPTURED_JOB_EVIDENCE_DIR" = "$fixture_archive" ] ||
		test_fail "GC fallback did not expose the validated archive"
	jq -e \
		--arg operationID "$TEST_OPERATION_ID" '
      .protocolVersion == 5 and .operation == "plan" and
      .operationId == $operationID
    ' "$OUTPUT_FILE" >/dev/null ||
		test_fail "GC fallback did not consume the normalized archived result"
)

prepare_archived_lifecycle_fixture() {
	reset_fixture
	printf '%s\n' '[]' >"$BOUNDARY_BEFORE_FILE"
	while IFS="$(printf '\t')" read -r fixture_uid fixture_operation; do
		write_valid_job_evidence "$fixture_uid" schema-automatic "$fixture_operation" >/dev/null
		printf '{"uid":"%s","name":"job-%s","created":"2026-01-01T00:00:00Z","schema":"schema-automatic","operation":"%s"}\n' \
			"$fixture_uid" "$fixture_operation" "$fixture_operation" \
			>>"$OBSERVED_JOBS_FILE"
	done <<'EOF'
uid-1	resolve
uid-2	verify
uid-3	observe
uid-4	plan
uid-5	apply
uid-6	observe
uid-7	plan
EOF
}

archived_lifecycle_gc_successful_path() (
	prepare_archived_lifecycle_fixture
	materialize_archived_schema_jobs schema-automatic "$BOUNDARY_BEFORE_FILE" 7 \
		"$ARCHIVED_UIDS_OUTPUT" "$ARCHIVED_JOBS_OUTPUT"
	jq -e '
      (.items | length) == 7 and
      ([.items[].metadata.uid] | sort) ==
        ["uid-1", "uid-2", "uid-3", "uid-4", "uid-5", "uid-6", "uid-7"]
    ' "$ARCHIVED_JOBS_OUTPUT" >/dev/null ||
		test_fail "GC-safe lifecycle did not materialize seven exact archived Jobs"
)

prepare_existing_archive_publish_input() {
	publish_fixture_name=$1
	reset_fixture
	EXISTING_ARCHIVE=$(write_valid_job_evidence uid-published schema-1 plan)
	PUBLISH_INPUT=$WORK_DIR/existing-publish-input-$publish_fixture_name
	mkdir "$PUBLISH_INPUT"
	chmod 700 "$PUBLISH_INPUT"
	cp "$EXISTING_ARCHIVE/job.json" "$PUBLISH_INPUT/job.json"
	cp "$EXISTING_ARCHIVE/pod.json" "$PUBLISH_INPUT/pod.json"
	cp "$EXISTING_ARCHIVE/ptah.log" "$PUBLISH_INPUT/ptah.log"
	chmod 600 "$PUBLISH_INPUT/job.json" "$PUBLISH_INPUT/pod.json" \
		"$PUBLISH_INPUT/ptah.log"
}

existing_archive_exact_identity_successful_path() (
	prepare_existing_archive_publish_input exact
	RESULT_ASSERT_BINARY=$WORK_DIR/resultassert-must-not-run
	kubectl() {
		test_fail "matching existing archive unexpectedly recaptured live transport"
	}
	publish_completed_job_evidence \
		"$PUBLISH_INPUT/job.json" "$PUBLISH_INPUT/pod.json" "$PUBLISH_INPUT/ptah.log"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
existing_archive_identity_collision() (
	collision_field=$1
	prepare_existing_archive_publish_input "$collision_field"
	case "$collision_field" in
	schema)
		jq '
          .metadata.labels["operator.ptah.dev/schema"] = "schema-other" |
          .spec.template.metadata.labels["operator.ptah.dev/schema"] = "schema-other" |
          .metadata.ownerReferences[0].name = "schema-other"
        ' "$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		jq '.metadata.labels["operator.ptah.dev/schema"] = "schema-other"' \
			"$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	operation)
		jq '
          .metadata.labels["operator.ptah.dev/operation"] = "verify" |
          .spec.template.metadata.labels["operator.ptah.dev/operation"] = "verify"
        ' "$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		jq '.metadata.labels["operator.ptah.dev/operation"] = "verify"' \
			"$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	operation-id)
		collision_operation_id=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
		collision_operation_label=$(printf '%s' "$collision_operation_id" | sha256 | cut -c1-16)
		jq --arg operationID "$collision_operation_id" \
			--arg operationLabel "$collision_operation_label" '
          .metadata.annotations["operator.ptah.dev/operation-id"] = $operationID |
          .metadata.labels["operator.ptah.dev/operation-id"] = $operationLabel |
          .spec.template.metadata.annotations["operator.ptah.dev/operation-id"] = $operationID |
          .spec.template.metadata.labels["operator.ptah.dev/operation-id"] = $operationLabel
        ' "$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		jq --arg operationID "$collision_operation_id" \
			--arg operationLabel "$collision_operation_label" '
          .metadata.annotations["operator.ptah.dev/operation-id"] = $operationID |
          .metadata.labels["operator.ptah.dev/operation-id"] = $operationLabel
        ' "$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	job-uid)
		jq '.metadata.uid = "uid-colliding"' \
			"$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		jq '.metadata.ownerReferences[0].uid = "uid-colliding"' \
			"$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		collision_key=$(job_evidence_key uid-colliding)
		mv "$EXISTING_ARCHIVE" "$JOB_EVIDENCE_DIR/$collision_key"
		;;
	job-name)
		jq '.metadata.name = "job-colliding"' \
			"$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		jq '
          .metadata.generateName = "job-colliding-" |
          .metadata.ownerReferences[0].name = "job-colliding"
        ' "$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	pod-uid)
		cp "$PUBLISH_INPUT/job.json" "$WORK_DIR/mutated-job.json"
		jq '.metadata.uid = "pod-colliding"' \
			"$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	pod-name)
		cp "$PUBLISH_INPUT/job.json" "$WORK_DIR/mutated-job.json"
		jq '.metadata.name = "job-plan-pod-colliding"' \
			"$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	owner)
		cp "$PUBLISH_INPUT/job.json" "$WORK_DIR/mutated-job.json"
		jq '.metadata.ownerReferences[0].controller = false' \
			"$PUBLISH_INPUT/pod.json" >"$WORK_DIR/mutated-pod.json"
		;;
	schema-owner-uid)
		jq '.metadata.ownerReferences[0].uid = "schema-uid-colliding"' \
			"$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		cp "$PUBLISH_INPUT/pod.json" "$WORK_DIR/mutated-pod.json"
		;;
	schema-owner-missing)
		jq '.metadata.ownerReferences = []' \
			"$PUBLISH_INPUT/job.json" >"$WORK_DIR/mutated-job.json"
		cp "$PUBLISH_INPUT/pod.json" "$WORK_DIR/mutated-pod.json"
		;;
	*) test_fail "unknown existing archive collision field: $collision_field" ;;
	esac
	chmod 600 "$WORK_DIR/mutated-job.json" "$WORK_DIR/mutated-pod.json"
	mv "$WORK_DIR/mutated-job.json" "$PUBLISH_INPUT/job.json"
	mv "$WORK_DIR/mutated-pod.json" "$PUBLISH_INPUT/pod.json"
	RESULT_ASSERT_BINARY=$WORK_DIR/resultassert-must-not-run
	kubectl() {
		test_fail "colliding existing archive unexpectedly recaptured live transport"
	}
	publish_completed_job_evidence \
		"$PUBLISH_INPUT/job.json" "$PUBLISH_INPUT/pod.json" "$PUBLISH_INPUT/ptah.log"
)

archive_publication_uses_uid_bounded_log_during_name_reuse() (
	reset_fixture
	fixture_archive=$(write_valid_job_evidence uid-published schema-1 plan)
	publish_input=$WORK_DIR/publish-input
	mv "$fixture_archive" "$publish_input"
	RESULT_ASSERT_BINARY=$WORK_DIR/resultassert
	{
		printf '%s\n' '#!/bin/sh'
		printf '%s\n' "jq -n --arg operationID '$TEST_OPERATION_ID' '{protocolVersion: 5, operation: \"plan\", operationId: \$operationID, truncation: null, error: null, childExitCode: 0, stdout: \"\"}'"
	} >"$RESULT_ASSERT_BINARY"
	chmod 700 "$RESULT_ASSERT_BINARY"
	kubectl() {
		printf '%s\n' 'replacement Pod raw transport'
	}
	publish_completed_job_evidence \
		"$publish_input/job.json" "$publish_input/pod.json" "$publish_input/ptah.log"
	validate_completed_job_evidence schema-1 plan uid-published
	grep -Fx 'fixture raw transport' "$fixture_archive/ptah.log" >/dev/null ||
		test_fail "publisher did not retain the UID-bounded audited ptah log"
	if grep -F 'replacement Pod raw transport' "$fixture_archive/ptah.log" >/dev/null; then
		test_fail "publisher fetched logs from a same-name replacement Pod"
	fi
	[ -z "$(sed -n '1p' "$FULLY_AUDITED_JOBS_FILE")" ] ||
		test_fail "archive publication wrote the full-audit ledger before its caller committed"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
archived_lifecycle_with_eighth_uid() (
	prepare_archived_lifecycle_fixture
	printf '%s\n' \
		'{"uid":"uid-8","name":"job-plan-replay","created":"2026-01-01T00:00:01Z","schema":"schema-automatic","operation":"plan"}' \
		>>"$OBSERVED_JOBS_FILE"
	materialize_archived_schema_jobs schema-automatic "$BOUNDARY_BEFORE_FILE" 7 \
		"$ARCHIVED_UIDS_OUTPUT" "$ARCHIVED_JOBS_OUTPUT"
)

prepare_schema_boundary_fixture() {
	reset_fixture
	printf '%s\n' '["uid-before"]' >"$BOUNDARY_BEFORE_FILE"
	printf '%s\n' \
		'["uid-1","uid-2","uid-3","uid-4","uid-5","uid-6","uid-7"]' \
		>"$BOUNDARY_EXPECTED_FILE"
	printf '%s\n' \
		'{"uid":"uid-before","schema":"schema-automatic"}' \
		'{"uid":"uid-1","schema":"schema-automatic"}' \
		'{"uid":"uid-2","schema":"schema-automatic"}' \
		'{"uid":"uid-3","schema":"schema-automatic"}' \
		'{"uid":"uid-4","schema":"schema-automatic"}' \
		'{"uid":"uid-5","schema":"schema-automatic"}' \
		'{"uid":"uid-6","schema":"schema-automatic"}' \
		'{"uid":"uid-7","schema":"schema-automatic"}' \
		'{"uid":"uid-unrelated","schema":"schema-other"}' \
		>"$OBSERVED_JOBS_FILE"
}

schema_boundary_successful_path() (
	prepare_schema_boundary_fixture
	assert_schema_job_boundary_unchanged schema-automatic \
		"$BOUNDARY_BEFORE_FILE" "$BOUNDARY_EXPECTED_FILE" 7 "$BOUNDARY_ACTUAL_FILE"
	jq -e '. == ["uid-1","uid-2","uid-3","uid-4","uid-5","uid-6","uid-7"]' \
		"$BOUNDARY_ACTUAL_FILE" >/dev/null ||
		test_fail "schema boundary helper did not materialize the exact expected UID set"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
schema_boundary_with_post_snapshot_historical_uid() (
	prepare_schema_boundary_fixture
	printf '%s\n' '{"uid":"uid-8-historical","schema":"schema-automatic"}' \
		>>"$OBSERVED_JOBS_FILE"
	assert_schema_job_boundary_unchanged schema-automatic \
		"$BOUNDARY_BEFORE_FILE" "$BOUNDARY_EXPECTED_FILE" 7 "$BOUNDARY_ACTUAL_FILE"
)

fault_successful_paths() (
	reset_fixture
	printf '%s\n' \
		'{"type":"ADDED","object":{"metadata":{"uid":"fault-job-1","name":"job-1","labels":{"operator.ptah.dev/schema":"schema-1","operator.ptah.dev/operation":"observe"}}}}' \
		>"$WORK_DIR/watch-jobs.jsonl"
	printf '%s\n' '{"type":"ADDED","object":{"metadata":{"uid":"fault-pod-1"}}}' \
		>"$WORK_DIR/watch-pods.jsonl"
	printf '%s\n' fault-job-1 >"$AUDITED_FAULT_JOBS_FILE"
	printf '%s\n' fault-pod-1 >"$AUDITED_FAULT_PODS_FILE"
	assert_fault_audit_complete
	record_fault_jobs_for_parent
	record_fault_jobs_for_parent
	printf '%s\n' \
		'{"apiVersion":"batch/v1","kind":"JobList","items":[{"metadata":{"uid":"fault-job-2","name":"job-2","labels":{"operator.ptah.dev/schema":"schema-2","operator.ptah.dev/operation":"plan"}}}]}' \
		>"$INITIAL_FAULT_JOBS_FILE"
	record_initial_job_list_for_parent "$INITIAL_FAULT_JOBS_FILE"
	jq -e -s '
      length == 2 and
      ([.[].uid] | sort) == ["fault-job-1", "fault-job-2"]
    ' "$SHARED_OBSERVED_JOBS_FILE" >/dev/null ||
		test_fail "fault helpers did not produce one valid parent record per Job UID"
)

# shellcheck disable=SC2317 # Extracted helpers invoke these test-local kubectl stubs dynamically.
assert_successful_paths() (
	reset_fixture
	kubectl() {
		emit_one_job_list
	}
	record_observed_jobs
	record_observed_jobs
	[ "$(wc -l <"$OBSERVED_JOBS_FILE")" -eq 1 ] ||
		test_fail "successful recording did not deduplicate the Job UID"
	printf '%s\n' uid-1 >"$FULLY_AUDITED_JOBS_FILE"
	assert_observed_jobs_audited
	kubectl() {
		emit_empty_job_list
	}
	: >"$OBSERVED_JOBS_FILE"
	assert_no_new_jobs schema-1 apply "$CHECKPOINT_FILE"
)

expect_failure 'kubectl list failure' \
	'could not list Jobs while recording the observed Job ledger' \
	record_with_kubectl_failure
expect_failure 'record projection jq failure' \
	'could not validate the observed Job ledger snapshot' \
	record_with_jq_failure
expect_failure 'final audit jq failure' \
	'could not validate observed Job UIDs before the final audit assertion' \
	audit_with_jq_failure
expect_failure 'missing full-audit evidence' \
	'observed Job UID uid-1 disappeared without a complete credential audit' \
	audit_with_missing_full_evidence
expect_failure 'new Job count jq failure' \
	'could not assert the absence of new apply Jobs for schema-1' \
	count_with_jq_failure
expect_failure 'fault audit jq failure' \
	'could not validate the fault-test jobs watch before the audit assertion' \
	fault_audit_with_jq_failure
expect_failure 'malformed fault audit watch' \
	'could not validate the fault-test jobs watch before the audit assertion' \
	fault_audit_with_malformed_watch
expect_failure 'invalid fault audit UID' \
	'could not validate the fault-test jobs watch before the audit assertion' \
	fault_audit_with_invalid_uid
expect_failure 'fault parent projection jq failure' \
	'could not validate the fault Job watch before updating the parent ledger' \
	fault_record_with_jq_failure
expect_failure 'malformed fault parent watch' \
	'could not validate the fault Job watch before updating the parent ledger' \
	fault_record_with_malformed_watch
expect_failure 'malformed initial fault list' \
	'could not validate the initial fault Job list before updating the parent ledger' \
	fault_initial_with_malformed_list
expect_failure 'terminal Job projection jq failure' \
	'could not capture exact terminal Job identities for credential audit' \
	terminal_job_projection_with_jq_failure
expect_failure 'owned Pod projection jq failure' \
	'could not capture exact owned Pod identities for credential audit' \
	owned_pod_projection_with_jq_failure
expect_failure 'manager Pod projection jq failure' \
	'could not capture exact manager Pod names for credential audit' \
	manager_pod_projection_with_jq_failure
expect_failure 'fault manager Pod projection jq failure' \
	'could not capture exact manager Pod names for fault credential audit' \
	fault_manager_pod_projection_with_jq_failure
expect_failure 'fault terminal Job projection jq failure' \
	'could not capture exact terminal Job identities for fault credential audit' \
	fault_terminal_job_projection_with_jq_failure
expect_failure 'fault owned Pod projection jq failure' \
	'could not capture exact owned Pod UIDs for fault credential audit' \
	fault_job_pod_projection_with_jq_failure
expect_failure 'complete Job projection jq failure' \
	'could not capture new plan Job records for schema-1' \
	complete_job_projection_with_jq_failure
expect_failure 'complete Job identity projection jq failure' \
	'could not validate new plan Job identities for schema-1' \
	complete_job_lines_with_jq_failure
expect_failure 'selected Job missing from ledger' \
	'has 0 observed plan Jobs with selected UID uid-selected' \
	selected_job_missing_uid
expect_failure 'selected Job missing durable archive' \
	'has no durable evidence archive' \
	selected_job_missing_archive
expect_failure 'selected Job partial durable archive' \
	'is partial or contains colliding material' \
	selected_job_partial_archive
expect_failure 'selected Job archive identity mismatch' \
	'has a mismatched identity or content hash' \
	selected_job_identity_mismatch
expect_failure 'selected Job archive hash mismatch' \
	'has a mismatched identity or content hash' \
	selected_job_hash_mismatch
expect_failure 'selected Job archive schema-owner UID mismatch' \
	'has mismatched exact Job JSON' \
	selected_job_schema_owner_uid_mismatch
expect_failure 'selected Job archive symlink' \
	'must name a regular non-symlink file' \
	selected_job_symlink_archive
expect_failure 'Job evidence root mode' \
	'Job evidence root must have mode 0700' \
	job_evidence_root_wrong_mode
expect_failure 'Job evidence archive mode' \
	'Job evidence archive must have mode 0700' \
	job_evidence_archive_wrong_mode
expect_failure 'Job evidence file mode' \
	'Job evidence raw ptah log must have mode 0600' \
	job_evidence_file_wrong_mode
expect_failure 'Job evidence sixth-file collision' \
	'is partial or contains colliding material' \
	job_evidence_sixth_file_collision
expect_failure 'Job evidence directory symlink' \
	'must name a directory, not a symlink' \
	job_evidence_directory_symlink
expect_failure 'Job evidence credential escape' \
	'a task credential escaped into archived raw ptah log' \
	job_evidence_credential_escape
expect_failure_without_leak 'live Job API failure' \
	'live Job consistency read failed before exact GC absence could be established' \
	'sensitive-forbidden-api-detail-71c8' \
	live_job_api_failure
expect_failure_without_leak 'live Pod API failure' \
	'live Pod consistency read failed before exact GC absence could be established' \
	'sensitive-timeout-api-detail-3b42' \
	live_pod_api_failure
expect_failure 'conflicting live Job identity' \
	'live Job conflicts with its durable evidence archive' \
	live_job_identity_conflict
expect_failure 'conflicting live Pod identity and owner' \
	'live Pod conflicts with its durable evidence archive' \
	live_pod_identity_conflict
expect_failure 'existing archive schema collision' \
	'has a mismatched identity or content hash' \
	existing_archive_identity_collision schema
expect_failure 'existing archive operation collision' \
	'has a mismatched identity or content hash' \
	existing_archive_identity_collision operation
expect_failure 'existing archive operation ID collision' \
	'collides with the supplied immutable identity' \
	existing_archive_identity_collision operation-id
expect_failure 'existing archive Job UID collision' \
	'has a mismatched identity or content hash' \
	existing_archive_identity_collision job-uid
expect_failure 'existing archive Job name collision' \
	'collides with the supplied immutable identity' \
	existing_archive_identity_collision job-name
expect_failure 'existing archive Pod UID collision' \
	'collides with the supplied immutable identity' \
	existing_archive_identity_collision pod-uid
expect_failure 'existing archive Pod name collision' \
	'collides with the supplied immutable identity' \
	existing_archive_identity_collision pod-name
expect_failure 'existing archive Pod owner collision' \
	'mismatched immutable identity or owner binding' \
	existing_archive_identity_collision owner
expect_failure 'existing archive schema-owner UID collision' \
	'has a mismatched identity or content hash' \
	existing_archive_identity_collision schema-owner-uid
expect_failure 'supplied archive missing schema-owner UID' \
	'lacks one exact PtahSchema controller owner UID' \
	existing_archive_identity_collision schema-owner-missing
expect_failure 'archived lifecycle replay UID' \
	'durable Job history is not exactly 7 unique UIDs' \
	archived_lifecycle_with_eighth_uid
expect_failure 'post-snapshot historical Job replay' \
	'schema-automatic durable Job boundary changed after result capture' \
	schema_boundary_with_post_snapshot_historical_uid
assert_successful_paths
live_job_and_pod_exact_successful_path
live_job_and_pod_exact_gc_absence_successful_path
selected_job_gc_fallback_successful_path
archived_lifecycle_gc_successful_path
existing_archive_exact_identity_successful_path
archive_publication_uses_uid_bounded_log_during_name_reuse
schema_boundary_successful_path
fault_successful_paths

printf '%s\n' 'e2e ledger self-test: PASS'
