#!/bin/sh

set -eu

# Prove the stopwatch before trusting what it says.
#
# Timing wraps the code that decides whether the operator works, so the risk is
# not an inaccurate duration: it is a measurement that swallows a failure. This
# runs hack/e2e-timing.sh the way the harness does and refuses it unless
#
#   - a measured command's exit status survives the call that closes its stage,
#   - a stage left open by a failure is recorded as a failure,
#   - an unwritable ledger costs the durations and nothing else, and
#   - a phase run with no ledger named behaves exactly as it did before.
#
# Usage: hack/e2e-timing-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

fail() {
	printf 'e2e timing self-test: %s\n' "$*" >&2
	exit 1
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-timing-selftest.XXXXXX")
trap 'rm -rf -- "$WORK_DIR"' EXIT

# A measured command that fails must still fail. The status is read after the
# stage is closed, which is exactly where a wrapper that returns its own status
# would report a pass.
status=0
(
	E2E_TIMING_LEDGER=$WORK_DIR/status.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin phase measured
	false
	timing_end pass
) || status=$?
[ "$status" -eq 1 ] ||
	fail "a failing measured command reported status $status through timing_end"
grep -q '"name":"measured"' "$WORK_DIR/status.jsonl" ||
	fail "the measured stage was not recorded"

# The same for the mark that opens the next stage: the bootstrap is a sequence,
# and a mark between two of its steps must not absolve the step before it.
status=0
(
	E2E_TIMING_LEDGER=$WORK_DIR/sequence.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin bootstrap first
	false
	timing_next bootstrap second
) || status=$?
[ "$status" -eq 1 ] ||
	fail "a failing step before timing_next reported status $status"

# A stage the run died in is recorded as a failure rather than dropped.
(
	E2E_TIMING_LEDGER=$WORK_DIR/abandoned.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin phase interrupted-phase
	timing_abandon fail
)
grep -q '"name":"interrupted-phase","outcome":"fail"' "$WORK_DIR/abandoned.jsonl" ||
	fail "an abandoned stage was not recorded as a failure"

# A stage reopened without being closed is the shape a crash leaves. It is
# recorded as interrupted, so a reader sees the gap instead of a missing row.
(
	E2E_TIMING_LEDGER=$WORK_DIR/reopened.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin scenario left-open
	timing_begin scenario next-one
	timing_end pass
)
grep -q '"name":"left-open","outcome":"interrupted"' "$WORK_DIR/reopened.jsonl" ||
	fail "a stage that was never closed was not recorded as interrupted"

# A ledger that cannot be written costs the durations. The run continues, and
# says so once rather than on every stage.
unwritable=$WORK_DIR/unwritable/ledger.jsonl
status=0
# The group carries the redirection: a command substitution is expanded before
# a redirection on the assignment itself would apply, so the stopwatch's own
# report would go to the terminal instead of the file this reads.
{
	# shellcheck disable=SC2030 # The ledger name is meant to be local to this probe.
	report=$(
		E2E_TIMING_LEDGER=$unwritable
		export E2E_TIMING_LEDGER
		# shellcheck source=hack/e2e-timing.sh
		. "$ROOT_DIR/hack/e2e-timing.sh"
		timing_begin phase first
		timing_end pass
		timing_begin phase second
		timing_end pass
		printf 'the phases ran\n'
	) || status=$?
} 2>"$WORK_DIR/unwritable.err"
[ "$status" -eq 0 ] ||
	fail "an unwritable ledger ended the run with status $status"
[ "$report" = "the phases ran" ] ||
	fail "an unwritable ledger changed what the run printed"
[ "$(grep -c 'cannot be appended to' "$WORK_DIR/unwritable.err")" -eq 1 ] ||
	fail "an unwritable ledger was reported $(grep -c 'cannot be appended to' "$WORK_DIR/unwritable.err") times, want once"

# The call an exit handler makes reaches it through a branch, so $? there is the
# status of the test that chose the branch. A run that succeeded must not exit 1
# because the else branch was taken, which is exactly what it used to do.
status=0
(
	E2E_TIMING_LEDGER=$WORK_DIR/handler.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	handler_status=0
	if [ "$handler_status" -ne 0 ]; then
		timing_abandon fail
	else
		timing_abandon pass
	fi
	exit "$handler_status"
) || status=$?
[ "$status" -eq 0 ] ||
	fail "an exit handler that closed no open stage ended a successful run with status $status"

# The same call with a stage open still records it, and still returns nothing of
# its own to the handler.
status=0
# shellcheck disable=SC2030 # The ledger name is meant to be local to this probe.
(
	E2E_TIMING_LEDGER=$WORK_DIR/handler-open.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin phase open-at-exit
	handler_status=0
	if [ "$handler_status" -ne 0 ]; then
		timing_abandon fail
	else
		timing_abandon pass
	fi
	exit "$handler_status"
) || status=$?
[ "$status" -eq 0 ] ||
	fail "an exit handler that closed an open stage ended a successful run with status $status"
grep -q '"name":"open-at-exit","outcome":"pass"' "$WORK_DIR/handler-open.jsonl" ||
	fail "the stage open at exit was not recorded"

# Exercise the driver's actual EXIT handler. Testing timing_abandon alone
# missed a teardown that called timing_begin and then exited without closing
# its stage. These probes never contact Docker or create a cluster.
sed -n '/^cleanup() {$/,/^}$/p' "$ROOT_DIR/hack/e2e-kind.sh" >"$WORK_DIR/cleanup.sh"
grep -q '^cleanup() {$' "$WORK_DIR/cleanup.sh" || fail "no driver cleanup handler was extracted"
printf '%s\n' '#!/bin/sh' 'set -eu' >"$WORK_DIR/cleanup-probe.sh"
cat >>"$WORK_DIR/cleanup-probe.sh" <<'PROBE'
probe_root=$1
probe_case=$2
probe_status=$3
probe_cleanup_failure=$4
E2E_KEEP_ON_FAILURE=$5
E2E_TIMING_LEDGER=$6
probe_source=$7
TMPDIR=$probe_root
WORK_DIR=$(mktemp -d "$TMPDIR/ptah-operator-e2e.XXXXXX")
printf '%s\n' "$WORK_DIR" >"$probe_root/$probe_case.workdir"
# shellcheck disable=SC1091 # Supplied by the self-test from this checkout.
. "$probe_source/hack/e2e-timing.sh"
PHASE_COMPLETED=1
RELEASE_CHART_OUTPUT_TEMP=
EXTERNAL_PG_CREATED=0
REGISTRY_CREATED=0
IMAGE_AUDIT_CONTAINER_CREATED=0
CLUSTER_CREATED=$probe_cleanup_failure
KIND_NODE_IMAGE_CREATED=0
KIND_NETWORK_CREATED=0
TUNNEL_PID=
IMAGE_CREATED=0
CREATED_IMAGE_REFS=
TASK_CLAIM_CREATE_STARTED=0
CLUSTER_NAME=timing-probe
KUBECONFIG_FILE=$WORK_DIR/kubeconfig
REGISTRY_CONTAINER=timing-probe-registry
EXTERNAL_PG_CONTAINER=timing-probe-database
collect_diagnostics() { :; }
debug_logs_stop_following() { :; }
kind() { return "$probe_cleanup_failure"; }
docker() { printf 'unexpected Docker call\n' >>"$probe_root/unexpected-docker"; return 1; }
# shellcheck disable=SC1091 # The actual handler extracted above.
. "$probe_root/cleanup.sh"
PROBE
# The handler belongs to the driver, not this file. Emit the trap as data so
# the static source check looks for its completion latch in the driver.
# shellcheck disable=SC2016 # The probe expands this when it runs.
printf '%s\n' 'trap cleanup EXIT' 'exit "$probe_status"' >>"$WORK_DIR/cleanup-probe.sh"
for probe_case in success failed-phase failed-cleanup both-failed retained unwritable; do
	probe_status=0
	probe_cleanup_failure=0
	probe_keep=0
	probe_want_status=0
	probe_outcome=pass
	probe_ledger=$WORK_DIR/$probe_case.jsonl
	case "$probe_case" in
		failed-phase) probe_status=23; probe_want_status=23 ;;
		failed-cleanup) probe_cleanup_failure=1; probe_want_status=1; probe_outcome=fail ;;
		both-failed) probe_status=23; probe_cleanup_failure=1; probe_want_status=23; probe_outcome=fail ;;
		retained) probe_status=23; probe_keep=1; probe_want_status=23; probe_outcome=retained ;;
		unwritable) probe_ledger=$WORK_DIR/missing-directory/ledger.jsonl ;;
	esac
	probe_actual_status=0
	sh "$WORK_DIR/cleanup-probe.sh" "$WORK_DIR" "$probe_case" "$probe_status" \
		"$probe_cleanup_failure" "$probe_keep" "$probe_ledger" "$ROOT_DIR" \
		>"$WORK_DIR/$probe_case.out" 2>"$WORK_DIR/$probe_case.err" || probe_actual_status=$?
	[ "$probe_actual_status" -eq "$probe_want_status" ] ||
		fail "$probe_case cleanup returned $probe_actual_status, want $probe_want_status"
	if [ "$probe_case" = unwritable ]; then
		grep -q 'cannot be appended to' "$WORK_DIR/$probe_case.err" ||
			fail "the unwritable teardown ledger was not reported"
	else
		[ "$(wc -l <"$probe_ledger" | tr -d ' ')" -eq 1 ] ||
			fail "$probe_case teardown did not write exactly one row"
		grep -q '"kind":"bootstrap","name":"teardown","outcome":"'"$probe_outcome"'"' "$probe_ledger" ||
			fail "$probe_case teardown did not record $probe_outcome"
	fi
	probe_workdir=$(cat "$WORK_DIR/$probe_case.workdir")
	if [ "$probe_keep" -eq 1 ]; then
		[ -d "$probe_workdir" ] || fail "the retained cleanup probe deleted its work directory"
	else
		[ ! -e "$probe_workdir" ] || fail "$probe_case cleanup did not remove its work directory"
	fi
done
[ ! -e "$WORK_DIR/unexpected-docker" ] || fail "a cleanup timing probe tried to use Docker"

# A phase run by hand names no ledger. Every call is then a no-op, and the
# phase behaves as it did before there was a stopwatch.
status=0
(
	unset E2E_TIMING_LEDGER
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin phase unmeasured
	timing_next bootstrap also-unmeasured
	timing_abandon fail
	timing_end pass
) || status=$?
[ "$status" -eq 0 ] ||
	fail "with no ledger named the stopwatch ended the run with status $status"

# The names in a row are the names the harness passed. A label that arrived
# with a quote in it cannot break the line it is written on.
(
	E2E_TIMING_LEDGER=$WORK_DIR/label.jsonl
	export E2E_TIMING_LEDGER
	: >"$E2E_TIMING_LEDGER"
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin phase 'a "quoted" \name'
	timing_end pass
)
[ "$(wc -l <"$WORK_DIR/label.jsonl")" -eq 1 ] ||
	fail "a label with a quote in it was written as more than one row"
grep -q '"name":"a -quoted- -name"' "$WORK_DIR/label.jsonl" ||
	fail "a label with a quote in it was not replaced: $(cat "$WORK_DIR/label.jsonl")"

# The report the tool publishes is what a reader compares, so the self-test
# ends where the data does: a ledger and a context through hack/e2etiming.
E2E_TIMING_LEDGER=$WORK_DIR/report.jsonl
export E2E_TIMING_LEDGER
: >"$E2E_TIMING_LEDGER"
(
	# shellcheck source=hack/e2e-timing.sh
	. "$ROOT_DIR/hack/e2e-timing.sh"
	timing_begin phase reported
	timing_end pass
)
jq -n '{runID: "selftest", operatorRevision: "0000000", kubernetes: "1.37.0", suite: "lifecycle"}' \
	>"$WORK_DIR/context.json"
go -C "$ROOT_DIR" run ./hack/e2etiming \
	-ledger "$E2E_TIMING_LEDGER" -context "$WORK_DIR/context.json" \
	-output "$WORK_DIR/report.json" -summary "$WORK_DIR/report.md" ||
	fail "the timing report could not be published"
jq -e '.records | length == 1 and (.[0].runID == "selftest" and .[0].name == "reported")' \
	"$WORK_DIR/report.json" >/dev/null ||
	fail "the published report does not carry the stage with its run: $(cat "$WORK_DIR/report.json")"
grep -q '| reported |' "$WORK_DIR/report.md" ||
	fail "the published summary does not name the stage"

# An empty ledger is a reportable fact. A summary step that passed over it
# would publish a run whose stages nobody recorded as a run with no stages.
if go -C "$ROOT_DIR" run ./hack/e2etiming \
	-ledger /dev/null -context "$WORK_DIR/context.json" -summary - >/dev/null 2>&1; then
	fail "an empty ledger was published as a report"
fi

printf '%s\n' 'e2e timing self-test: PASS status survives, failures are recorded, and the report carries its run'
