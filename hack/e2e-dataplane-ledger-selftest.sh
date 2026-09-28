#!/bin/sh
# shellcheck disable=SC2034,SC2329 # Extracted helpers consume fixture globals and command stubs dynamically.

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

# The runner protocol this tree speaks. support/ptah.json records it for the
# release under test, and hack/verifyptahsupport holds that record to
# runner.ProtocolVersion, so an assertion reads it here rather than writing
# the number down a second time.
RUNNER_PROTOCOL_VERSION=$(jq -er '
  [.releases[] | select(.operator == "edge") | .verified[].runnerProtocolVersion] | unique |
  if length == 1 and (.[0] | type) == "number" then .[0]
  else error("support/ptah.json must record exactly one runner protocol version for edge") end
' "$ROOT_DIR/support/ptah.json")
FAULT_SOURCE_FILE=$ROOT_DIR/hack/e2e-faults.sh
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-ledger-selftest.XXXXXX")
FUNCTIONS_FILE=$WORK_DIR/functions.sh
ERROR_FILE=$WORK_DIR/error.txt
OUTPUT_FILE=$WORK_DIR/output.txt

# A refused parameter expansion (${VAR:?...}) or an unset name under set -u
# ends the shell without setting $?, so an EXIT trap that reports $? reads the
# previous command's success and a script that never finished reports a pass.
# The latch is set where the script reaches its own end; the trap trusts it.
PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
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

for command_name in jq sed grep mktemp wc tr dd cmp base64 go; do
	command -v "$command_name" >/dev/null 2>&1 ||
		test_fail "required command is not installed: $command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	test_fail "sha256sum or shasum is required"
fi

# The data-plane phase is Go now, and test/e2e holds its helpers to their cases.
# The fault phase is still a script, and it carries helpers of its own: the
# result-transport reader, the plan-document rebuild and the sealed-payload
# proof it once shared with the data plane, and the ledgers it hands back to
# the phase that runs it. They are measured here, extracted from the script
# they run in.
: >"$FUNCTIONS_FILE"
for function_name in \
	sha256 k read_result_transport rebuild_plan_document assert_plan_result_stdout_is_sealed \
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
TRANSPORT_LOG_FILE=$WORK_DIR/transport.log
TRANSPORT_RESULT_FILE=$WORK_DIR/transport-result.json
TRANSPORT_READS_FILE=$WORK_DIR/transport-reads.txt
TRANSPORT_FRAME_FILE=$WORK_DIR/transport-frame.log
TRANSPORT_WRONG_FRAME_FILE=$WORK_DIR/transport-wrong-frame.log
TRANSPORT_ARRIVAL_PREFIXES_FILE=$WORK_DIR/transport-arrival-prefixes.txt
AUDITED_FAULT_JOBS_FILE=$WORK_DIR/fault-audited-jobs.txt
AUDITED_FAULT_PODS_FILE=$WORK_DIR/fault-audited-pods.txt
SHARED_OBSERVED_JOBS_FILE=$WORK_DIR/shared-observed-jobs.jsonl
INITIAL_FAULT_JOBS_FILE=$WORK_DIR/initial-fault-jobs.json
FAULT_MANAGER_POD_NAMES_FILE=$WORK_DIR/fault-manager-pod-names.txt
FAULT_TERMINAL_JOB_RECORDS_FILE=$WORK_DIR/fault-terminal-job-records.tsv
FAULT_JOB_POD_UIDS_FILE=$WORK_DIR/fault-job-pod-uids.txt
TEST_OPERATION_ID=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TEST_COORDINATION_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
TEST_DIVERGENT_COORDINATION_DIGEST=${TEST_COORDINATION_DIGEST%?}d

fail() {
	printf 'fixture failure: %s\n' "$*" >&2
	exit 97
}

reset_fixture() {
	: >"$TRANSPORT_READS_FILE"
	rm -f -- "$TRANSPORT_LOG_FILE" "$TRANSPORT_RESULT_FILE" "$TRANSPORT_RESULT_FILE.err"
	: >"$AUDITED_FAULT_JOBS_FILE"
	: >"$AUDITED_FAULT_PODS_FILE"
	: >"$SHARED_OBSERVED_JOBS_FILE"
	: >"$WORK_DIR/watch-jobs.jsonl"
	: >"$WORK_DIR/watch-pods.jsonl"
	: >"$FAULT_MANAGER_POD_NAMES_FILE"
	: >"$FAULT_TERMINAL_JOB_RECORDS_FILE"
	: >"$FAULT_JOB_POD_UIDS_FILE"
	printf '%s\n' '{"apiVersion":"batch/v1","kind":"JobList","items":[]}' >"$INITIAL_FAULT_JOBS_FILE"
}

# The transports a runner container log can hold when it is read, built from
# the frame test/e2e/resultassert is given below. A log still arriving is a
# prefix of the complete one: the container runtime copies the runner's last
# write in, and a read taken while it does ends anywhere inside the frame. A log
# that is present and wrong is all there and does not hold together, and reading
# it again would never repair it.
#
# The frames are real and the parser is the real one. The shell under test
# decides whether to read a log again from the words the parser refused it in,
# so a stand-in parser that words a refusal differently proves the stand-in and
# says nothing about the system. That is exactly how the bounded re-read below
# went from #155 until now without ever running: the stand-in answered a
# footerless log with "never finished arriving", and the binary answered it with
# a sentence the re-read does not wait for.
write_result_transport_frames() {
	transport_payload=$(jq -cn \
		--argjson runnerProtocolVersion "$RUNNER_PROTOCOL_VERSION" \
		--arg operationID "$TEST_OPERATION_ID" \
		--arg coordinationDigest "$TEST_COORDINATION_DIGEST" '
      {protocolVersion: $runnerProtocolVersion, operation: "plan", operationId: $operationID,
       childExitCode: 0, stdout: "", coordinationDigest: $coordinationDigest,
       planOutcome: "NoChanges"}') ||
		test_fail "could not build the transport result payload"
	# The divergent payload differs from the declared one in a single byte of a
	# digest and not in length. A payload of another length would end somewhere
	# else and be refused as a frame still arriving, which is the opposite of
	# what the wrong transport is here to measure.
	transport_divergent_payload=$(jq -cn \
		--argjson runnerProtocolVersion "$RUNNER_PROTOCOL_VERSION" \
		--arg operationID "$TEST_OPERATION_ID" \
		--arg coordinationDigest "$TEST_DIVERGENT_COORDINATION_DIGEST" '
      {protocolVersion: $runnerProtocolVersion, operation: "plan", operationId: $operationID,
       childExitCode: 0, stdout: "", coordinationDigest: $coordinationDigest,
       planOutcome: "NoChanges"}') ||
		test_fail "could not build the divergent transport result payload"
	transport_payload_bytes=$(printf '%s' "$transport_payload" | wc -c | tr -d '[:space:]')
	transport_divergent_bytes=$(printf '%s' "$transport_divergent_payload" | wc -c | tr -d '[:space:]')
	[ "$transport_payload_bytes" -eq "$transport_divergent_bytes" ] ||
		test_fail "the divergent transport payload is $transport_divergent_bytes bytes and the declared one is $transport_payload_bytes"
	transport_payload_digest=$(printf '%s' "$transport_payload" | sha256)
	printf 'PTAH_RUNNER_RESULT_V1 %s %s\n%s\nPTAH_RUNNER_RESULT_END_V1\n' \
		"$transport_payload_bytes" "$transport_payload_digest" "$transport_payload" \
		>"$TRANSPORT_FRAME_FILE"
	printf 'PTAH_RUNNER_RESULT_V1 %s %s\n%s\nPTAH_RUNNER_RESULT_END_V1\n' \
		"$transport_payload_bytes" "$transport_payload_digest" "$transport_divergent_payload" \
		>"$TRANSPORT_WRONG_FRAME_FILE"
	transport_header_bytes=$(sed -n '1p' "$TRANSPORT_FRAME_FILE" | wc -c | tr -d '[:space:]')
	transport_frame_bytes=$(wc -c <"$TRANSPORT_FRAME_FILE" | tr -d '[:space:]')
	# The byte counts a container log passes through while the frame arrives:
	# nothing, a header cut inside its own line, a header with none of its
	# payload, a payload short of the length the header declares, a payload with
	# no footer behind it, a footer half arrived, and the frame. Each is a read
	# the parser has to answer with a reason the re-read waits for.
	TRANSPORT_FOOTERLESS_BYTES=$((transport_header_bytes + transport_payload_bytes))
	printf '%s\n' \
		0 \
		"$((transport_header_bytes - 10))" \
		"$transport_header_bytes" \
		"$((transport_header_bytes + transport_payload_bytes / 2))" \
		"$TRANSPORT_FOOTERLESS_BYTES" \
		"$((TRANSPORT_FOOTERLESS_BYTES + 10))" \
		"$transport_frame_bytes" \
		>"$TRANSPORT_ARRIVAL_PREFIXES_FILE"
	TRANSPORT_ARRIVAL_READS=$(wc -l <"$TRANSPORT_ARRIVAL_PREFIXES_FILE" | tr -d '[:space:]')
}

emit_transport_prefix() {
	dd if="$TRANSPORT_FRAME_FILE" bs=1 count="$1" 2>/dev/null
}

emit_complete_transport() {
	cat "$TRANSPORT_FRAME_FILE"
}

emit_incomplete_transport() {
	emit_transport_prefix "$TRANSPORT_FOOTERLESS_BYTES"
}

emit_wrong_transport() {
	cat "$TRANSPORT_WRONG_FRAME_FILE"
}

emit_two_frame_transport() {
	cat "$TRANSPORT_FRAME_FILE" "$TRANSPORT_FRAME_FILE"
}

emit_twice_closed_transport() {
	cat "$TRANSPORT_FRAME_FILE"
	printf '%s\n' PTAH_RUNNER_RESULT_END_V1
}

# The parser the extracted shell calls is the one the phase calls. Building it
# here costs a compile and removes the only place this self-test could have
# agreed with itself and not with the system.
RESULT_ASSERT_BINARY=$WORK_DIR/resultassert
go -C "$ROOT_DIR" build -o "$RESULT_ASSERT_BINARY" ./test/e2e/resultassert ||
	test_fail "could not build test/e2e/resultassert"
write_result_transport_frames

# The transport fixtures drive the bounded re-read on a clock of their own, so
# a self-test that measures a thirty-second window does not wait one.
transport_clock_stub() {
	TRANSPORT_CLOCK=1000
	# shellcheck disable=SC2317 # Invoked indirectly by the helper under test.
	date() {
		case " $* " in
		*' +%s '*) printf '%s\n' "$TRANSPORT_CLOCK" ;;
		*) command date "$@" ;;
		esac
	}
	# shellcheck disable=SC2317 # Invoked indirectly by the helper under test.
	sleep() {
		TRANSPORT_CLOCK=$((TRANSPORT_CLOCK + $1))
	}
}

transport_read_count() {
	wc -l <"$TRANSPORT_READS_FILE" | tr -d '[:space:]'
}

# The log arrives one prefix at a time, exactly as the container runtime copies
# the runner's last write in. Every read before the last is a shape the parser
# has to answer with a reason the bounded re-read waits for; the last is the
# frame, and the read settles on it.
transport_settles_after_an_incomplete_read() (
	reset_fixture
	transport_clock_stub
	# shellcheck disable=SC2317 # Invoked indirectly by the helper under test.
	kubectl() {
		printf '%s\n' read >>"$TRANSPORT_READS_FILE"
		transport_prefix=$(sed -n "$(transport_read_count)p" "$TRANSPORT_ARRIVAL_PREFIXES_FILE")
		[ -n "$transport_prefix" ] ||
			test_fail "the arrival sequence ran out after $(transport_read_count) reads"
		emit_transport_prefix "$transport_prefix"
	}
	read_result_transport pod-under-audit "$TRANSPORT_LOG_FILE" plan \
		"$TEST_OPERATION_ID" "$TRANSPORT_RESULT_FILE"
	[ "$(transport_read_count)" -eq "$TRANSPORT_ARRIVAL_READS" ] ||
		test_fail "settled transport read the container log $(transport_read_count) times, want $TRANSPORT_ARRIVAL_READS"
	grep -Fx PTAH_RUNNER_RESULT_END_V1 "$TRANSPORT_LOG_FILE" >/dev/null ||
		test_fail "settled transport retained a log whose frame never closed"
	jq -e --argjson runnerProtocolVersion "$RUNNER_PROTOCOL_VERSION" \
		'.protocolVersion == $runnerProtocolVersion' "$TRANSPORT_RESULT_FILE" >/dev/null ||
		test_fail "settled transport did not retain its validated result"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
transport_frame_that_never_arrives() (
	reset_fixture
	transport_clock_stub
	kubectl() {
		printf '%s\n' read >>"$TRANSPORT_READS_FILE"
		emit_incomplete_transport
	}
	read_result_transport pod-under-audit "$TRANSPORT_LOG_FILE" plan \
		"$TEST_OPERATION_ID" "$TRANSPORT_RESULT_FILE"
)

# The three shapes that are present and wrong. Each is refused on its first
# read, because no later read of the same container log can reduce a digest
# that does not match or a marker the log carries twice.
# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
transport_frame_that_is_present_and_wrong() (
	transport_wrong_emitter=$1
	reset_fixture
	transport_clock_stub
	kubectl() {
		printf '%s\n' read >>"$TRANSPORT_READS_FILE"
		"$transport_wrong_emitter"
	}
	read_result_transport pod-under-audit "$TRANSPORT_LOG_FILE" plan \
		"$TEST_OPERATION_ID" "$TRANSPORT_RESULT_FILE"
)

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

fault_successful_paths() (
	reset_fixture
	printf '%s\n' \
		'{"type":"ADDED","object":{"metadata":{"uid":"fault-job-1","name":"job-1","labels":{"operator.ptah.run/schema":"schema-1","operator.ptah.run/operation":"observe"}}}}' \
		>"$WORK_DIR/watch-jobs.jsonl"
	printf '%s\n' '{"type":"ADDED","object":{"metadata":{"uid":"fault-pod-1"}}}' \
		>"$WORK_DIR/watch-pods.jsonl"
	printf '%s\n' fault-job-1 >"$AUDITED_FAULT_JOBS_FILE"
	printf '%s\n' fault-pod-1 >"$AUDITED_FAULT_PODS_FILE"
	assert_fault_audit_complete
	record_fault_jobs_for_parent
	record_fault_jobs_for_parent
	printf '%s\n' \
		'{"apiVersion":"batch/v1","kind":"JobList","items":[{"metadata":{"uid":"fault-job-2","name":"job-2","labels":{"operator.ptah.run/schema":"schema-2","operator.ptah.run/operation":"plan"}}}]}' \
		>"$INITIAL_FAULT_JOBS_FILE"
	record_initial_job_list_for_parent "$INITIAL_FAULT_JOBS_FILE"
	jq -e -s '
      length == 2 and
      ([.[].uid] | sort) == ["fault-job-1", "fault-job-2"]
    ' "$SHARED_OBSERVED_JOBS_FILE" >/dev/null ||
		test_fail "fault helpers did not produce one valid parent record per Job UID"
)

# Since runner protocol 7 a Plan result's stdout is the plan sealed to the
# manager's key, so the phases read the document back from the plan's
# PtahSchemaPlanChunk objects and prove the stdout is not the document. The
# fixture is one document split into two chunks inside a statement, so that a
# rebuild that read one chunk, or read both in the wrong order, cannot pass. The first
# statement quotes an identifier, so the JSON form a plan document spells it
# in differs from the SQL, and both forms are expected among the patterns the
# sealed-payload proof searches for.
REBUILD_PLAN_DOCUMENT_FILE=$WORK_DIR/rebuild-plan-document.json
REBUILD_PLAN_CHUNK_0_FILE=$WORK_DIR/rebuild-plan-chunk-0.bin
REBUILD_PLAN_CHUNK_1_FILE=$WORK_DIR/rebuild-plan-chunk-1.bin
REBUILD_PLAN_OBJECT_FILE=$WORK_DIR/rebuild-plan-object.json
REBUILT_PLAN_OUTPUT_FILE=$WORK_DIR/rebuilt-plan.json
SEALED_PLAN_RESULT_FILE=$WORK_DIR/sealed-plan-result.json
SEALED_PLAN_PATTERNS_FILE=$WORK_DIR/sealed-plan-result-plan-text-patterns.txt
REBUILD_PLAN_SPLIT_AT=60
printf '%s' '{"format_version":1,"dialect":"postgres","from_fingerprint":"sha256:1111111111111111111111111111111111111111111111111111111111111111","to_fingerprint":"sha256:2222222222222222222222222222222222222222222222222222222222222222","destructive":false,"statements":[{"sql":"CREATE TABLE \"widgets\" (id integer PRIMARY KEY)","severity":"safe"},{"sql":"COMMIT","severity":"safe"}]}' \
	>"$REBUILD_PLAN_DOCUMENT_FILE"
jq -e '.statements[0].sql == "CREATE TABLE \"widgets\" (id integer PRIMARY KEY)"' \
	"$REBUILD_PLAN_DOCUMENT_FILE" >/dev/null ||
	test_fail "the rebuild fixture document does not quote its identifier as intended"
dd if="$REBUILD_PLAN_DOCUMENT_FILE" of="$REBUILD_PLAN_CHUNK_0_FILE" bs=1 count="$REBUILD_PLAN_SPLIT_AT" 2>/dev/null
dd if="$REBUILD_PLAN_DOCUMENT_FILE" of="$REBUILD_PLAN_CHUNK_1_FILE" bs=1 skip="$REBUILD_PLAN_SPLIT_AT" 2>/dev/null
REBUILD_PLAN_DOCUMENT_BYTES=$(wc -c <"$REBUILD_PLAN_DOCUMENT_FILE" | tr -d ' ')
REBUILD_PLAN_CHUNK_1_BYTES=$((REBUILD_PLAN_DOCUMENT_BYTES - REBUILD_PLAN_SPLIT_AT))
[ "$REBUILD_PLAN_CHUNK_1_BYTES" -gt 0 ] || test_fail "the rebuild fixture document is too short to split"
REBUILD_PLAN_DIGEST="sha256:$(sha256 <"$REBUILD_PLAN_DOCUMENT_FILE")"
REBUILD_PLAN_CHUNK_0_BASE64=$(jq -Rrs '@base64' "$REBUILD_PLAN_CHUNK_0_FILE")
REBUILD_PLAN_CHUNK_1_BASE64=$(jq -Rrs '@base64' "$REBUILD_PLAN_CHUNK_1_FILE")
# What a sealed payload looks like to this proof: base64 that is not the
# document. The digest is used as the bytes only because it is handy.
SEALED_PLAN_STDOUT=$(printf '%s%s' "$REBUILD_PLAN_DIGEST" "$REBUILD_PLAN_DIGEST" | jq -Rrs '@base64')
REBUILD_STUB_CHUNK_1_FIELD=data
REBUILD_STUB_CHUNK_1_BASE64=$REBUILD_PLAN_CHUNK_1_BASE64

write_rebuild_plan_object() {
	jq -n --arg digest "$REBUILD_PLAN_DIGEST" --argjson chunks "$1" '{
      metadata: {name: "plan-2", uid: "plan-uid-2", resourceVersion: "201"},
      spec: {destructive: false, contentDigest: $digest, chunks: $chunks}
    }' >"$REBUILD_PLAN_OBJECT_FILE"
}

write_rebuild_plan_object_in_order() {
	write_rebuild_plan_object "$(jq -n \
		--argjson size0 "$REBUILD_PLAN_SPLIT_AT" --argjson size1 "$REBUILD_PLAN_CHUNK_1_BYTES" '[
      {name: "plan-2-000", index: 0, size: $size0},
      {name: "plan-2-001", index: 1, size: $size1}
    ]')"
}

write_sealed_plan_result() {
	jq -n --arg stdout "$1" --arg digest "$REBUILD_PLAN_DIGEST" \
		'{stdout: $stdout, planOutcome: "Changes", planContentDigest: $digest}' \
		>"$SEALED_PLAN_RESULT_FILE"
}

emit_rebuild_chunk() {
	jq -n --arg name "$1" --arg field "$2" --arg value "$3" '{
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchemaPlanChunk",
      metadata: {name: $name, ownerReferences: [{
        apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchemaPlan",
        name: "plan-2", uid: "plan-uid-2", controller: true}]},
      spec: {($field): $value}
    }'
}

# shellcheck disable=SC2317 # Extracted helpers invoke this test-local kubectl stub dynamically.
rebuild_plan_kubectl() {
	rebuild_stub_verb=
	rebuild_stub_kind=
	rebuild_stub_name=
	for rebuild_stub_argument in "$@"; do
		case "$rebuild_stub_argument" in
		get) [ -n "$rebuild_stub_verb" ] || rebuild_stub_verb=$rebuild_stub_argument ;;
		ptahschemaplanchunk) [ -n "$rebuild_stub_kind" ] || rebuild_stub_kind=$rebuild_stub_argument ;;
		plan-2-*) [ -n "$rebuild_stub_name" ] || rebuild_stub_name=$rebuild_stub_argument ;;
		esac
	done
	case "$rebuild_stub_verb:$rebuild_stub_kind:$rebuild_stub_name" in
	get:ptahschemaplanchunk:plan-2-000) emit_rebuild_chunk plan-2-000 data "$REBUILD_PLAN_CHUNK_0_BASE64" ;;
	get:ptahschemaplanchunk:plan-2-001) emit_rebuild_chunk plan-2-001 "$REBUILD_STUB_CHUNK_1_FIELD" "$REBUILD_STUB_CHUNK_1_BASE64" ;;
	*) return 45 ;;
	esac
}

# shellcheck disable=SC2317 # Extracted helpers invoke these test-local kubectl stubs dynamically.
plan_document_rebuild_successful_path() (
	reset_fixture
	kubectl() { rebuild_plan_kubectl "$@"; }
	write_rebuild_plan_object_in_order
	rebuild_plan_document "$REBUILD_PLAN_OBJECT_FILE" "$REBUILT_PLAN_OUTPUT_FILE"
	[ "$REBUILT_PLAN_CHUNK_COUNT" -eq 2 ] ||
		test_fail "the plan rebuild counted $REBUILT_PLAN_CHUNK_COUNT chunks rather than the two it read"
	cmp -s "$REBUILT_PLAN_OUTPUT_FILE" "$REBUILD_PLAN_DOCUMENT_FILE" ||
		test_fail "the plan rebuilt from two chunks is not the document they were split from"
	[ "sha256:$(sha256 <"$REBUILT_PLAN_OUTPUT_FILE")" = "$REBUILD_PLAN_DIGEST" ] ||
		test_fail "the rebuilt plan does not hash to the fixture's content digest"
	write_sealed_plan_result "$SEALED_PLAN_STDOUT"
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILT_PLAN_OUTPUT_FILE" schema-2
	[ "$(wc -l <"$SEALED_PLAN_PATTERNS_FILE" | tr -d ' ')" -eq 3 ] ||
		test_fail "the sealed-payload proof searched for $(wc -l <"$SEALED_PLAN_PATTERNS_FILE" | tr -d ' ') patterns rather than the statement in both spellings and the document key"
	grep -Fx 'CREATE TABLE "widgets" (id integer PRIMA' "$SEALED_PLAN_PATTERNS_FILE" >/dev/null ||
		test_fail "the sealed-payload proof did not search for the statement's first forty characters as SQL"
	grep -Fx 'CREATE TABLE \"widgets\" (id integer PRIMA' "$SEALED_PLAN_PATTERNS_FILE" >/dev/null ||
		test_fail "the sealed-payload proof did not search for the statement's first forty characters as a plan document spells them"
	grep -Fx '"format_version"' "$SEALED_PLAN_PATTERNS_FILE" >/dev/null ||
		test_fail "the sealed-payload proof did not search for the plan document's own key"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
plan_document_rebuild_with_no_chunks() (
	reset_fixture
	kubectl() { rebuild_plan_kubectl "$@"; }
	write_rebuild_plan_object '[]'
	rebuild_plan_document "$REBUILD_PLAN_OBJECT_FILE" "$REBUILT_PLAN_OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
plan_document_rebuild_with_chunks_out_of_order() (
	reset_fixture
	kubectl() { rebuild_plan_kubectl "$@"; }
	write_rebuild_plan_object "$(jq -n \
		--argjson size0 "$REBUILD_PLAN_SPLIT_AT" --argjson size1 "$REBUILD_PLAN_CHUNK_1_BYTES" '[
      {name: "plan-2-001", index: 1, size: $size1},
      {name: "plan-2-000", index: 0, size: $size0}
    ]')"
	rebuild_plan_document "$REBUILD_PLAN_OBJECT_FILE" "$REBUILT_PLAN_OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
plan_document_rebuild_with_missing_chunk_data() (
	reset_fixture
	kubectl() { rebuild_plan_kubectl "$@"; }
	REBUILD_STUB_CHUNK_1_FIELD=other
	write_rebuild_plan_object_in_order
	rebuild_plan_document "$REBUILD_PLAN_OBJECT_FILE" "$REBUILT_PLAN_OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
plan_document_rebuild_with_short_chunk() (
	reset_fixture
	kubectl() { rebuild_plan_kubectl "$@"; }
	REBUILD_STUB_CHUNK_1_BASE64=$(dd if="$REBUILD_PLAN_CHUNK_1_FILE" bs=1 count=5 2>/dev/null | jq -Rrs '@base64')
	write_rebuild_plan_object_in_order
	rebuild_plan_document "$REBUILD_PLAN_OBJECT_FILE" "$REBUILT_PLAN_OUTPUT_FILE"
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
sealed_plan_result_with_empty_stdout() (
	reset_fixture
	write_sealed_plan_result ''
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILD_PLAN_DOCUMENT_FILE" schema-2
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
sealed_plan_result_with_plaintext_stdout() (
	reset_fixture
	write_sealed_plan_result "$(cat "$REBUILD_PLAN_DOCUMENT_FILE")"
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILD_PLAN_DOCUMENT_FILE" schema-2
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
sealed_plan_result_with_statement_text_in_stdout() (
	reset_fixture
	write_sealed_plan_result "${SEALED_PLAN_STDOUT}CREATE TABLE \"widgets\" (id integer PRIMARY KEY)"
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILD_PLAN_DOCUMENT_FILE" schema-2
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
sealed_plan_result_with_json_escaped_statement_in_stdout() (
	reset_fixture
	write_sealed_plan_result "${SEALED_PLAN_STDOUT}CREATE TABLE \\\"widgets\\\" (id integer PRIMARY KEY)"
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILD_PLAN_DOCUMENT_FILE" schema-2
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
sealed_plan_result_with_format_version_in_stdout() (
	reset_fixture
	write_sealed_plan_result "${SEALED_PLAN_STDOUT}{\"format_version\":1}"
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILD_PLAN_DOCUMENT_FILE" schema-2
)

# shellcheck disable=SC2317 # Invoked indirectly through expect_failure below.
sealed_plan_result_for_a_document_without_statements() (
	reset_fixture
	write_sealed_plan_result "$SEALED_PLAN_STDOUT"
	jq -c '.statements = []' "$REBUILD_PLAN_DOCUMENT_FILE" >"$REBUILT_PLAN_OUTPUT_FILE"
	assert_plan_result_stdout_is_sealed "$SEALED_PLAN_RESULT_FILE" "$REBUILT_PLAN_OUTPUT_FILE" schema-2
)

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
expect_failure 'fault manager Pod projection jq failure' \
	'could not capture exact manager Pod names for fault credential audit' \
	fault_manager_pod_projection_with_jq_failure
expect_failure 'fault terminal Job projection jq failure' \
	'could not capture exact terminal Job identities for fault credential audit' \
	fault_terminal_job_projection_with_jq_failure
expect_failure 'fault owned Pod projection jq failure' \
	'could not capture exact owned Pod UIDs for fault credential audit' \
	fault_job_pod_projection_with_jq_failure
expect_failure 'result frame that never finishes arriving' \
	'the plan result frame from pod-under-audit could not be read' \
	transport_frame_that_never_arrives
grep -F 'never finished arriving' "$ERROR_FILE" >/dev/null ||
	test_fail 'transport that never arrived was refused without the parser reason'
[ "$(transport_read_count)" -gt 1 ] ||
	test_fail 'transport that may still be arriving was refused on its first read'
expect_failure 'result frame that is present and wrong' \
	'the plan result frame from pod-under-audit could not be read' \
	transport_frame_that_is_present_and_wrong emit_wrong_transport
grep -F 'does not match the digest its header declares' "$ERROR_FILE" >/dev/null ||
	test_fail 'wrong transport was refused without the parser reason'
[ "$(transport_read_count)" -eq 1 ] ||
	test_fail 'transport that is present and wrong was read more than once'
expect_failure 'result transport carrying two frames' \
	'the plan result frame from pod-under-audit could not be read' \
	transport_frame_that_is_present_and_wrong emit_two_frame_transport
grep -F 'carries 2 result frame headers rather than one' "$ERROR_FILE" >/dev/null ||
	test_fail 'two-frame transport was refused without the parser reason'
[ "$(transport_read_count)" -eq 1 ] ||
	test_fail 'transport carrying two frames was read more than once'
expect_failure 'result transport whose frame is closed twice' \
	'the plan result frame from pod-under-audit could not be read' \
	transport_frame_that_is_present_and_wrong emit_twice_closed_transport
grep -F 'carries 2 result frame footers rather than one' "$ERROR_FILE" >/dev/null ||
	test_fail 'twice-closed transport was refused without the parser reason'
[ "$(transport_read_count)" -eq 1 ] ||
	test_fail 'transport whose frame is closed twice was read more than once'
transport_settles_after_an_incomplete_read
plan_document_rebuild_successful_path
expect_failure 'plan rebuild from a plan with no chunks' \
	'plan-2 has no plan chunks to rebuild its document from' \
	plan_document_rebuild_with_no_chunks
expect_failure 'plan rebuild from chunks listed out of order' \
	'plan-2 chunk at position 0 carries index 1' \
	plan_document_rebuild_with_chunks_out_of_order
expect_failure 'plan rebuild from a chunk missing its data' \
	'plan-2-001 has no spec.data to rebuild plan-2 chunk 1 from' \
	plan_document_rebuild_with_missing_chunk_data
expect_failure 'plan rebuild from a chunk shorter than its manifest' \
	"plan-2 chunk 1 decoded to 5 bytes; its manifest says $REBUILD_PLAN_CHUNK_1_BYTES" \
	plan_document_rebuild_with_short_chunk
expect_failure 'sealed-payload proof over an empty stdout' \
	'schema-2 Plan result carries no sealed payload in stdout' \
	sealed_plan_result_with_empty_stdout
expect_failure 'sealed-payload proof over a plaintext stdout' \
	'schema-2 Plan result carries the plan document itself in stdout, not a sealed payload' \
	sealed_plan_result_with_plaintext_stdout
expect_failure 'sealed-payload proof over a stdout carrying statement SQL' \
	'schema-2 Plan result stdout carries plan text in the clear' \
	sealed_plan_result_with_statement_text_in_stdout
expect_failure 'sealed-payload proof over a stdout carrying a JSON-spelled statement' \
	'schema-2 Plan result stdout carries plan text in the clear' \
	sealed_plan_result_with_json_escaped_statement_in_stdout
expect_failure 'sealed-payload proof over a stdout carrying the document key' \
	'schema-2 Plan result stdout carries plan text in the clear' \
	sealed_plan_result_with_format_version_in_stdout
expect_failure 'sealed-payload proof over a document without statements' \
	'schema-2 plan document has no statements to check the sealed payload against' \
	sealed_plan_result_for_a_document_without_statements
fault_successful_paths

PHASE_COMPLETED=1
printf '%s\n' 'e2e ledger self-test: PASS'
