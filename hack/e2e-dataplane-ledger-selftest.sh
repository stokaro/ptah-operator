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

for command_name in jq sed grep mktemp; do
	command -v "$command_name" >/dev/null 2>&1 ||
		test_fail "required command is not installed: $command_name"
done

: >"$FUNCTIONS_FILE"
for function_name in \
	k record_observed_jobs assert_observed_jobs_audited new_job_count_since assert_no_new_jobs \
	materialize_terminal_job_records materialize_owned_pod_records materialize_manager_pod_names \
	all_new_jobs_complete; do
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
CHECKPOINT_FILE=$WORK_DIR/checkpoint.json
AUDITED_FAULT_JOBS_FILE=$WORK_DIR/fault-audited-jobs.txt
AUDITED_FAULT_PODS_FILE=$WORK_DIR/fault-audited-pods.txt
SHARED_OBSERVED_JOBS_FILE=$WORK_DIR/shared-observed-jobs.jsonl
INITIAL_FAULT_JOBS_FILE=$WORK_DIR/initial-fault-jobs.json
FAULT_MANAGER_POD_NAMES_FILE=$WORK_DIR/fault-manager-pod-names.txt
FAULT_TERMINAL_JOB_RECORDS_FILE=$WORK_DIR/fault-terminal-job-records.tsv
FAULT_JOB_POD_UIDS_FILE=$WORK_DIR/fault-job-pod-uids.txt

fail() {
	printf 'fixture failure: %s\n' "$*" >&2
	exit 97
}

reset_fixture() {
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
	printf '%s\n' '[]' >"$CHECKPOINT_FILE"
	printf '%s\n' '{"apiVersion":"batch/v1","kind":"JobList","items":[]}' >"$INITIAL_FAULT_JOBS_FILE"
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
assert_successful_paths
fault_successful_paths

printf '%s\n' 'e2e ledger self-test: PASS'
