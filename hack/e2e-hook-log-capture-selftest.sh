#!/bin/sh
# shellcheck disable=SC2034 # The extracted capture reads these fixture globals.

set -eu

# Prove the hook-log capture before trusting the record it prints.
#
# The identity hook's log is the only thing that says why a refused upgrade was
# refused, and the capture races the cluster from both ends: the Pod object
# exists before its container does, and Helm deletes this hook within a moment
# of the Job completing. A run on Kubernetes 1.35 hit that and printed
# captureStatus "log-read-failed" over the SHA-256 of the empty string, which
# named neither end and left the phase failing with nothing to read.
#
# This extracts read_identity_hook_log and identity_hook_pod_presence out of
# hack/e2e-crd-upgrade.sh, drives them with a stubbed kubectl, and refuses them
# unless
#
#   - a read that fails and then succeeds while the Pod stays is captured,
#   - a read that never succeeds while the Pod stays still fails, at the
#     deadline, with a record that names that state,
#   - a Pod that went away is reported as gone rather than as a failed read,
#   - an API server that could not answer is not mistaken for a Pod that went
#     away,
#   - a clean read of an empty log is never retried into something else, and
#   - every state the capture can reach is one the phase's record accepts.
#
# Usage: hack/e2e-hook-log-capture-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
SOURCE_FILE=$ROOT_DIR/hack/e2e-crd-upgrade.sh
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-hook-log-selftest.XXXXXX")
FUNCTIONS_FILE=$WORK_DIR/functions.sh
STUB_BIN_DIR=$WORK_DIR/bin

# A refused parameter expansion or an unset name under set -u ends the shell
# without setting $?, so an EXIT trap that reports $? reads the previous
# command's success and a self-test that never finished reports a pass. The
# latch is set where this script reaches its own end; the trap trusts it.
PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-operator-hook-log-selftest.*) rm -rf -- "$WORK_DIR" ;;
	*)
		printf 'e2e hook-log self-test: refusing to remove unexpected work directory %s\n' \
			"$WORK_DIR" >&2
		status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

test_fail() {
	printf 'e2e hook-log self-test: %s\n' "$*" >&2
	exit 1
}

for command_name in jq sed grep awk mktemp sort tr wc; do
	command -v "$command_name" >/dev/null 2>&1 ||
		test_fail "required command is not installed: $command_name"
done

: >"$FUNCTIONS_FILE"
for function_name in identity_hook_pod_presence read_identity_hook_log; do
	function_section=$(sed -n "/^${function_name}()/,/^}/p" "$SOURCE_FILE")
	[ -n "$function_section" ] || test_fail "could not extract $function_name"
	printf '%s\n' "$function_section" >>"$FUNCTIONS_FILE" ||
		test_fail "could not stage $function_name"
done
# shellcheck source=/dev/null
. "$FUNCTIONS_FILE"

# The stub answers the two calls the capture makes. A log read fails for the
# first STUB_LOG_FAILURES attempts and then prints STUB_LOG_TEXT; a Pod read
# answers as STUB_POD_STATE says. Both count their attempts, because the claim
# is as much about how often the capture asked as about what it concluded.
mkdir -p "$STUB_BIN_DIR"
cat >"$STUB_BIN_DIR/kubectl" <<'STUB'
#!/bin/sh
set -u
kubectl_mode=
for kubectl_arg in "$@"; do
	case "$kubectl_arg" in
	logs)
		kubectl_mode=logs
		break
		;;
	get)
		kubectl_mode=get
		break
		;;
	esac
done
case "$kubectl_mode" in
logs)
	attempts=$(cat "$STUB_STATE_DIR/log-attempts" 2>/dev/null || printf '0')
	attempts=$((attempts + 1))
	printf '%s\n' "$attempts" >"$STUB_STATE_DIR/log-attempts"
	if [ "$attempts" -le "$STUB_LOG_FAILURES" ]; then
		printf 'error: container identity-probe in pod %s is waiting to start\n' \
			"$STUB_POD_NAME" >&2
		exit 1
	fi
	[ -z "$STUB_LOG_TEXT" ] || printf '%s\n' "$STUB_LOG_TEXT"
	exit 0
	;;
get)
	attempts=$(cat "$STUB_STATE_DIR/get-attempts" 2>/dev/null || printf '0')
	attempts=$((attempts + 1))
	printf '%s\n' "$attempts" >"$STUB_STATE_DIR/get-attempts"
	case "$STUB_POD_STATE" in
	present)
		printf '{"metadata":{"name":"%s","uid":"%s"}}\n' \
			"$STUB_POD_NAME" "$STUB_POD_UID"
		exit 0
		;;
	replaced)
		printf '{"metadata":{"name":"%s","uid":"%s"}}\n' \
			"$STUB_POD_NAME" 99999999-9999-9999-9999-999999999999
		exit 0
		;;
	gone)
		exit 0
		;;
	unknown)
		printf 'error: the server could not answer for pod %s\n' "$STUB_POD_NAME" >&2
		exit 1
		;;
	esac
	;;
esac
printf 'stub kubectl: unexpected invocation: %s\n' "$*" >&2
exit 64
STUB
chmod 0755 "$STUB_BIN_DIR/kubectl"
PATH=$STUB_BIN_DIR:$PATH
export PATH

E2E_KUBECONFIG=$WORK_DIR/kubeconfig
E2E_OPERATOR_NAMESPACE=ptah-system
STUB_POD_NAME=ptah-hook-identity-v1-f4dde565bbcb-x7q2d
STUB_POD_UID=11111111-2222-3333-4444-555555555555
export STUB_POD_NAME STUB_POD_UID

# Every scenario is one full run of the capture against a fresh stub state,
# with its own deadline: a scenario that has to reach a successful read needs
# room for the attempts before it, and one that measures the deadline itself
# needs the deadline to arrive while the self-test is still running.
run_capture() {
	scenario=$1
	STUB_LOG_FAILURES=$2
	STUB_LOG_TEXT=$3
	STUB_POD_STATE=$4
	capture_interrupted=$5
	IDENTITY_HOOK_LOG_READ_DEADLINE_SECONDS=$6
	export STUB_LOG_FAILURES STUB_LOG_TEXT STUB_POD_STATE
	STUB_STATE_DIR=$WORK_DIR/$scenario
	export STUB_STATE_DIR
	mkdir -p "$STUB_STATE_DIR"
	printf '0\n' >"$STUB_STATE_DIR/log-attempts"
	printf '0\n' >"$STUB_STATE_DIR/get-attempts"
	IDENTITY_HOOK_LOG_FILE=$STUB_STATE_DIR/identity-hook.log
	IDENTITY_HOOK_CAPTURE_ERRORS_FILE=$STUB_STATE_DIR/identity-hook-capture-errors
	IDENTITY_HOOK_RECHECK_FILE=$STUB_STATE_DIR/identity-hook-recheck.json
	: >"$IDENTITY_HOOK_LOG_FILE"
	: >"$IDENTITY_HOOK_CAPTURE_ERRORS_FILE"
	: >"$IDENTITY_HOOK_RECHECK_FILE"
	capture_child_pid=
	capture_status=not-set
	read_identity_hook_log "$STUB_POD_NAME" "$STUB_POD_UID"
}

log_attempts() {
	tr -d '[:space:]' <"$STUB_STATE_DIR/log-attempts"
}

assert_status() {
	[ "$capture_status" = "$1" ] ||
		test_fail "$2: captureStatus is $capture_status, want $1"
}

# A read that fails while the Pod is still there is the harness losing a race,
# not evidence that is missing. Before this, the first answer was the only one:
# one attempt, an empty file, and a record that said log-read-failed.
run_capture transient 2 'ptah-crd-manager: verify pre-staged hook workloads: not found' present 0 20
assert_status captured 'a read that succeeded on the third attempt'
[ "$(log_attempts)" -eq 3 ] ||
	test_fail "a transient read failure took $(log_attempts) attempts, want 3"
grep -Fqx 'ptah-crd-manager: verify pre-staged hook workloads: not found' \
	"$IDENTITY_HOOK_LOG_FILE" ||
	test_fail "the captured log is not the line the hook printed"

# The retry must not turn a missing proof into a pass. A read that keeps
# failing while the Pod stays still ends as a failure, at the deadline, with
# an empty log and a record that names the state.
run_capture persistent 99 'unreachable' present 0 2
assert_status log-read-failed 'a read that never succeeded'
[ "$(log_attempts)" -ge 2 ] ||
	test_fail "a persistent read failure took $(log_attempts) attempts, want at least 2"
[ ! -s "$IDENTITY_HOOK_LOG_FILE" ] ||
	test_fail "a read that never succeeded left evidence behind"

# A Pod that went away and a read that failed are different facts. The capture
# stops on the first retry and says which one it was.
run_capture deleted 99 'unreachable' gone 0 20
assert_status pod-deleted 'a Pod that was deleted before its log could be read'
[ "$(log_attempts)" -eq 1 ] ||
	test_fail "a deleted Pod took $(log_attempts) log attempts, want 1"

# Same name, different UID: the Pod we validated is gone, whatever took its
# place.
run_capture replaced 99 'unreachable' replaced 0 20
assert_status pod-deleted 'a Pod replaced by another of the same name'

# An API server that could not answer is not a Pod that went away. Guessing
# either way here is how a record comes to blame the wrong thing, so the
# capture keeps asking and reports the read it could not make.
run_capture unknown 99 'unreachable' unknown 0 2
assert_status log-read-failed 'a Pod read the API server could not answer'
[ "$(log_attempts)" -ge 2 ] ||
	test_fail "an unanswerable Pod read took $(log_attempts) attempts, want at least 2"

# A hook that produced no evidence has to go on reading as one. A stream that
# ended by itself having printed nothing is settled, and is not retried.
run_capture empty 0 '' present 0 20
assert_status log-empty 'a clean read of an empty log'
[ "$(log_attempts)" -eq 1 ] ||
	test_fail "an empty log took $(log_attempts) attempts, want 1"

# A capture the phase tore down reports the teardown, not a verdict about the
# hook.
run_capture interrupted 99 'unreachable' present 1 20
assert_status terminated 'a capture interrupted by the phase'

# The record is only as good as the set it validates against. Every state the
# capture assigns has to be a state emit_identity_hook_diagnostic accepts, and
# every state it accepts has to be one the capture can reach: a status missing
# from the case list is printed as invalid-status, and one that lingers there
# is a state nothing produces.
ASSIGNED_FILE=$WORK_DIR/assigned-statuses
ACCEPTED_FILE=$WORK_DIR/accepted-statuses
grep -E '^[[:space:]]*capture_status=[a-z][a-z-]*$' "$SOURCE_FILE" |
	sed -e 's/^[[:space:]]*capture_status=//' | sort -u >"$ASSIGNED_FILE"
ASSIGNED_COUNT=$(grep -c . "$ASSIGNED_FILE" || true)
[ "$ASSIGNED_COUNT" -ge 10 ] ||
	test_fail "found only $ASSIGNED_COUNT capture states in $SOURCE_FILE"
# pod-identity-invalid belongs to this case list and to no other in the file,
# so it is what names the list rather than a shape another case shares.
ACCEPTED_LINE=$(grep -E '^[[:space:]]*captured \|.*pod-identity-invalid.*\) ;;$' "$SOURCE_FILE" || true)
[ "$(printf '%s\n' "$ACCEPTED_LINE" | grep -c .)" -eq 1 ] ||
	test_fail "want exactly one capture-status case list in $SOURCE_FILE"
printf '%s\n' "$ACCEPTED_LINE" | tr '|' '\n' |
	sed -e 's/[^a-z-]//g' -e '/^$/d' | sort -u >"$ACCEPTED_FILE"
UNACCEPTED=$(grep -Fxv -f "$ACCEPTED_FILE" "$ASSIGNED_FILE" | tr '\n' ' ' || true)
[ -z "$UNACCEPTED" ] ||
	test_fail "the capture reaches states the record rejects: $UNACCEPTED"
UNREACHED=$(grep -Fxv -f "$ASSIGNED_FILE" "$ACCEPTED_FILE" | tr '\n' ' ' || true)
[ -z "$UNREACHED" ] ||
	test_fail "the record accepts states the capture cannot reach: $UNREACHED"
grep -Fqx pod-deleted "$ASSIGNED_FILE" ||
	test_fail "the capture no longer distinguishes a deleted Pod from a failed read"

printf '%s\n' 'e2e hook-log self-test: PASS a racing read is retried, a deleted Pod is named as one, and a missing log still fails'
PHASE_COMPLETED=1
