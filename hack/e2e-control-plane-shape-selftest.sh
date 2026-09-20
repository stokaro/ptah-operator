#!/bin/sh

set -eu

# Prove that the bootstrap waits for the control plane and still refuses a wrong
# one.
#
# A three-control-plane kind cluster turns its nodes Ready before the second and
# third control planes have started their static pods, so a single snapshot read
# at that moment legitimately holds fewer than nine of them. The check used to
# assert a steady state over that snapshot and reported "feature gates are not
# confined to the API server" when the real reading was two API servers.
#
# This drives hack/e2e-kind.sh's wait_for_control_plane_component_shape with a
# stubbed cluster and refuses it unless
#
#   - an incomplete snapshot is waited out and the complete one that follows
#     passes,
#   - a snapshot that never completes fails at the deadline, naming how many of
#     each component were seen and how many were ready, and
#   - a complete snapshot that is wrong is refused on the first reading rather
#     than polled until the deadline.
#
# Usage: hack/e2e-control-plane-shape-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
CLUSTER=selftest-ha
EXPECTED_GATES=GenericWorkload=true

fail() {
	printf 'e2e control-plane shape self-test: %s\n' "$*" >&2
	exit 1
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-control-plane-shape-selftest.XXXXXX")
trap 'rm -rf -- "$WORK_DIR"' EXIT

# The wait, extracted from the driver so this measures what runs rather than a
# copy of it.
FUNCTION_FILE=$WORK_DIR/wait-for-control-plane-component-shape.sh
awk '
	/^wait_for_control_plane_component_shape\(\) \{$/ { capture = 1 }
	capture { print }
	capture && /^\}$/ { exit }
' "$ROOT_DIR/hack/e2e-kind.sh" >"$FUNCTION_FILE"
grep -q '^wait_for_control_plane_component_shape() {' "$FUNCTION_FILE" ||
	fail "the extraction from hack/e2e-kind.sh does not carry wait_for_control_plane_component_shape"
grep -q '^}$' "$FUNCTION_FILE" ||
	fail "the extraction from hack/e2e-kind.sh is not a complete function body"

# One static pod as the API server reports it. The command carries the options
# the assertion is about, and an unready pod is one whose container has not
# started, which is what a control plane looks like while it is joining.
component_pod() {
	jq -n --arg component "$1" --arg node "$2" --arg readiness "$3" \
		--arg gates "$4" --arg runtime "$5" '
	  {
	    metadata: {
	      name: ($component + "-" + $node),
	      labels: {component: $component},
	      annotations: {"kubernetes.io/config.mirror": "mirror-hash"}
	    },
	    spec: {
	      nodeName: $node,
	      containers: [{
	        name: $component,
	        command: (["/usr/local/bin/" + $component]
	          + (if $gates == "" then [] else ["--feature-gates=" + $gates] end)
	          + (if $runtime == "" then [] else ["--runtime-config=" + $runtime] end))
	      }]
	    },
	    status: (if $readiness == "ready" then {
	        phase: "Running",
	        conditions: [{type: "Ready", status: "True"}],
	        containerStatuses: [{
	          name: $component,
	          ready: true,
	          state: {running: {startedAt: "2026-09-20T10:00:00Z"}}
	        }]
	      } else {
	        phase: "Pending",
	        conditions: [{type: "Ready", status: "False"}],
	        containerStatuses: []
	      } end)
	  }'
}

# The three static pods a joined control-plane node runs, as kind starts them.
control_plane_node_pods() {
	component_pod kube-apiserver "$1" ready "$EXPECTED_GATES" api/all=true
	component_pod kube-controller-manager "$1" ready '' ''
	component_pod kube-scheduler "$1" ready '' ''
}

snapshot() {
	jq -s '{items: .}' >"$1"
}

# Snapshot 1: the first control plane has started its static pods and the other
# two have not. This is the reading the bootstrap used to fail on.
{
	control_plane_node_pods "$CLUSTER-control-plane"
} | snapshot "$WORK_DIR/one-control-plane.json"

# Snapshot 2: two of three, which is what run 35505450675 held.
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
} | snapshot "$WORK_DIR/two-control-planes.json"

# The steady state.
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	control_plane_node_pods "$CLUSTER-control-plane3"
} | snapshot "$WORK_DIR/complete.json"

# Nine pods, and one scheduler that never becomes ready.
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	component_pod kube-apiserver "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" api/all=true
	component_pod kube-controller-manager "$CLUSTER-control-plane3" ready '' ''
	component_pod kube-scheduler "$CLUSTER-control-plane3" pending '' ''
} | snapshot "$WORK_DIR/scheduler-never-ready.json"

# The wrong clusters: complete, ready, and carrying something no amount of
# waiting repairs.
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	component_pod kube-apiserver "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" api/all=true
	component_pod kube-controller-manager "$CLUSTER-control-plane3" ready '' ''
	component_pod kube-scheduler "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" ''
} | snapshot "$WORK_DIR/scheduler-gates.json"
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	component_pod kube-apiserver "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" api/all=true
	component_pod kube-controller-manager "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" ''
	component_pod kube-scheduler "$CLUSTER-control-plane3" ready '' ''
} | snapshot "$WORK_DIR/controller-manager-gates.json"
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	component_pod kube-apiserver "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" ''
	component_pod kube-controller-manager "$CLUSTER-control-plane3" ready '' ''
	component_pod kube-scheduler "$CLUSTER-control-plane3" ready '' ''
} | snapshot "$WORK_DIR/runtime-config-replaced.json"
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	component_pod kube-apiserver "$CLUSTER-control-plane3" ready ExpandedDNSConfig=true api/all=true
	component_pod kube-controller-manager "$CLUSTER-control-plane3" ready '' ''
	component_pod kube-scheduler "$CLUSTER-control-plane3" ready '' ''
} | snapshot "$WORK_DIR/api-server-other-gates.json"
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	component_pod kube-apiserver "$CLUSTER-control-plane3" ready "$EXPECTED_GATES" api/all=true
	component_pod kube-controller-manager "$CLUSTER-control-plane3" ready '' ''
	component_pod kube-scheduler "$CLUSTER-worker" ready '' ''
} | snapshot "$WORK_DIR/scheduler-off-the-control-plane.json"
{
	control_plane_node_pods "$CLUSTER-control-plane"
	control_plane_node_pods "$CLUSTER-control-plane2"
	control_plane_node_pods "$CLUSTER-control-plane3"
	component_pod kube-apiserver "$CLUSTER-control-plane4" ready "$EXPECTED_GATES" api/all=true
} | snapshot "$WORK_DIR/fourth-api-server.json"

# Run the wait against a sequence of snapshots. The clock is a counter rather
# than the wall clock, so a deadline is a number of readings and the self-test
# waits for nothing. The reading count is what proves whether it polled.
#
# Arguments: the deadline in stub seconds, then one snapshot per reading. The
# last one is repeated for every reading after it.
run_wait() {
	run_deadline=$1
	shift
	printf '0\n' >"$WORK_DIR/readings"
	printf '0\n' >"$WORK_DIR/clock"
	run_index=0
	for run_snapshot in "$@"; do
		run_index=$((run_index + 1))
		cp "$run_snapshot" "$WORK_DIR/reading-$run_index.json"
	done
	printf '%s\n' "$run_index" >"$WORK_DIR/reading-count"
	run_status=0
	(
		# The names below are read by the extracted wait, and the stubs below
		# them are called by it. shellcheck follows neither.
		# shellcheck disable=SC2034
		CLUSTER_NAME=$CLUSTER
		# shellcheck disable=SC2034
		KUBECONFIG_FILE=$WORK_DIR/kubeconfig
		# shellcheck disable=SC2034
		CONTROL_PLANE_SHAPE_DEADLINE_SECONDS=$run_deadline

		# One reading per call, and the last snapshot stands in for every
		# reading after the sequence runs out.
		# shellcheck disable=SC2317,SC2329
		kubectl() {
			stub_reading=$(($(cat "$WORK_DIR/readings") + 1))
			printf '%s\n' "$stub_reading" >"$WORK_DIR/readings"
			stub_available=$(cat "$WORK_DIR/reading-count")
			[ "$stub_reading" -le "$stub_available" ] || stub_reading=$stub_available
			cat "$WORK_DIR/reading-$stub_reading.json"
		}

		# A clock that advances one second per reading, so the deadline is
		# reached without waiting for it.
		# shellcheck disable=SC2317,SC2329
		date() {
			stub_now=$(($(cat "$WORK_DIR/clock") + 1))
			printf '%s\n' "$stub_now" >"$WORK_DIR/clock"
			printf '%s\n' "$stub_now"
		}

		# shellcheck disable=SC2317,SC2329
		sleep() {
			:
		}

		# shellcheck disable=SC2317,SC2329
		fail() {
			printf '%s\n' "$*" >"$WORK_DIR/failure"
			exit 9
		}

		# shellcheck source=/dev/null
		. "$FUNCTION_FILE"
		wait_for_control_plane_component_shape "$EXPECTED_GATES"
	) >"$WORK_DIR/output" 2>&1 || run_status=$?
	RUN_STATUS=$run_status
	RUN_READINGS=$(cat "$WORK_DIR/readings")
	RUN_FAILURE=
	[ ! -f "$WORK_DIR/failure" ] || RUN_FAILURE=$(cat "$WORK_DIR/failure")
	rm -f "$WORK_DIR/failure"
}

# A complete cluster passes on the first reading: waiting is what an incomplete
# snapshot costs, not what every run costs.
run_wait 30 "$WORK_DIR/complete.json"
[ "$RUN_STATUS" -eq 0 ] ||
	fail "a complete control plane was refused with status $RUN_STATUS: $RUN_FAILURE"
[ "$RUN_READINGS" -eq 1 ] ||
	fail "a complete control plane took $RUN_READINGS readings, want one"

# The failure that was reported as feature gates: the readings before the
# steady state are waited out, and the one that holds it passes.
run_wait 30 \
	"$WORK_DIR/one-control-plane.json" \
	"$WORK_DIR/two-control-planes.json" \
	"$WORK_DIR/complete.json"
[ "$RUN_STATUS" -eq 0 ] ||
	fail "a control plane that was still joining was refused with status $RUN_STATUS: $RUN_FAILURE"
[ "$RUN_READINGS" -eq 3 ] ||
	fail "the wait took $RUN_READINGS readings to reach the steady state, want three"

# A shape that never completes fails at the deadline, and the message is the
# count that was missing rather than a verdict about feature gates.
run_wait 4 "$WORK_DIR/two-control-planes.json"
[ "$RUN_STATUS" -eq 9 ] ||
	fail "a control plane that never completed ended with status $RUN_STATUS, want the deadline"
[ "$RUN_READINGS" -gt 1 ] ||
	fail "the deadline was reached after $RUN_READINGS reading, so nothing was waited for"
case "$RUN_FAILURE" in
	*"kube-apiserver 2 seen 2 ready"*) ;;
	*) fail "the deadline did not say how many API servers were seen: $RUN_FAILURE" ;;
esac
case "$RUN_FAILURE" in
	*"kube-controller-manager 2 seen 2 ready"*"kube-scheduler 2 seen 2 ready"*) ;;
	*) fail "the deadline did not say what the other components held: $RUN_FAILURE" ;;
esac
case "$RUN_FAILURE" in
	*"in 4s"*) ;;
	*) fail "the deadline did not say how long it waited: $RUN_FAILURE" ;;
esac
case "$RUN_FAILURE" in
	*"did not reach three ready kube-apiserver"*) ;;
	*) fail "the deadline did not say what it was waiting for: $RUN_FAILURE" ;;
esac

# Nine pods and one that never becomes ready is the same deadline, and the
# counts separate what was seen from what was ready.
run_wait 4 "$WORK_DIR/scheduler-never-ready.json"
[ "$RUN_STATUS" -eq 9 ] ||
	fail "a scheduler that never became ready ended with status $RUN_STATUS, want the deadline"
case "$RUN_FAILURE" in
	*"kube-scheduler 3 seen 2 ready"*) ;;
	*) fail "the deadline did not separate the schedulers seen from the ones ready: $RUN_FAILURE" ;;
esac

# A cluster that is wrong is refused on the reading that shows it. Polling one
# of these to a deadline would turn a real refusal into a vague timeout, so the
# reading count is asserted as well as the message.
assert_refused_at_once() {
	refusal_snapshot=$1
	refusal_expectation=$2
	run_wait 30 "$WORK_DIR/$refusal_snapshot.json"
	[ "$RUN_STATUS" -eq 9 ] ||
		fail "$refusal_snapshot ended with status $RUN_STATUS, want a refusal"
	[ "$RUN_READINGS" -eq 1 ] ||
		fail "$refusal_snapshot was polled $RUN_READINGS times instead of being refused at once"
	case "$RUN_FAILURE" in
		*"$refusal_expectation"*) ;;
		*) fail "$refusal_snapshot was refused with [$RUN_FAILURE], want $refusal_expectation" ;;
	esac
}

assert_refused_at_once scheduler-gates \
	"kube-scheduler pod kube-scheduler-$CLUSTER-control-plane3 carries --feature-gates=$EXPECTED_GATES"
assert_refused_at_once controller-manager-gates \
	"kube-controller-manager pod kube-controller-manager-$CLUSTER-control-plane3 carries --feature-gates=$EXPECTED_GATES"
assert_refused_at_once runtime-config-replaced \
	"kube-apiserver pod kube-apiserver-$CLUSTER-control-plane3 carries 0 --runtime-config options"
assert_refused_at_once api-server-other-gates \
	"carries --feature-gates=ExpandedDNSConfig=true, expected --feature-gates=$EXPECTED_GATES"
assert_refused_at_once scheduler-off-the-control-plane \
	"runs on $CLUSTER-worker, which is not a control plane"
# A fourth API server is a complete reading, not a growing one, so waiting for
# it would turn a cluster that is wrong into a timeout that says nothing.
assert_refused_at_once fourth-api-server \
	"runs on $CLUSTER-control-plane4, which is not a control plane"

printf '%s\n' 'e2e control-plane shape self-test: PASS a joining control plane is waited out, a deadline names the counts, and a wrong one is refused at once'
