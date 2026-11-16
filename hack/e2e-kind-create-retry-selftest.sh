#!/bin/sh

set -eu

# Prove that the bootstrap creates its kind cluster once more when kind lost its
# own race for a host port, and for no other failure.
#
# kind v0.33.0 gives every control plane of a cluster with more than one a
# random host port. It picks the port by listening on port 0 and releases it
# before docker run binds it, while the nodes are created concurrently, so two
# control planes can draw the same port and Docker refuses the second with
# "port is already allocated" before any phase runs.
#
# This drives hack/e2e-kind.sh's create_kind_cluster with a stubbed kind and
# refuses it unless
#
#   - a create that failed with that message is deleted and created once more,
#     and a second attempt that succeeds ends the step,
#   - a create that failed with anything else fails the run at once, with
#     kind's own exit status, and
#   - a second port failure after the retry fails the run.
#
# Every row also holds what a retry must not disturb: the cluster is claimed for
# the teardown before kind runs, the debug-log follower starts once, the second
# attempt finds no kubeconfig from the first, and kind's output reaches the
# run's log while kind is still running.
#
# Every row runs, and each one that fails says so, so a mutated driver shows
# every row it breaks.
#
# Usage: hack/e2e-kind-create-retry-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
CLUSTER=selftest-ha

selftest_fail() {
	printf 'e2e kind create retry self-test: %s\n' "$*" >&2
	exit 1
}

SELFTEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-kind-create-retry-selftest.XXXXXX")
trap 'rm -rf -- "$SELFTEST_DIR"' EXIT

# The functions, extracted from the driver so this measures what runs rather
# than a copy of it.
FUNCTIONS_FILE=$SELFTEST_DIR/functions.sh
for function_name in run_kind_create_cluster create_kind_cluster; do
	awk -v name="$function_name" '
		$0 == name "() {" { capture = 1 }
		capture { print }
		capture && /^\}$/ { exit }
	' "$ROOT_DIR/hack/e2e-kind.sh" >>"$FUNCTIONS_FILE"
	grep -Eq "^$function_name\\(\\) \\{$" "$FUNCTIONS_FILE" ||
		selftest_fail "the extraction from hack/e2e-kind.sh does not carry $function_name"
done

# The line the stubbed kind writes first, and waits to see in the run's log.
started_line() {
	printf 'stub kind: attempt %s is creating cluster %s' "$1" "$CLUSTER"
}

# run_create runs create_kind_cluster against a kind that answers each create
# with the next outcome named in the arguments:
#
#   created    the cluster exists and kind wrote the kubeconfig
#   port-race  Docker refused a control plane's binding, as job 131814297976
#              saw it, and the kubeconfig holds something
#   in-use     the pinned load-balancer port was taken: a port failure too,
#              and not the one kind's race produces
#
# A create beyond the named outcomes fails with a message of its own.
#
# The driver's function runs in a subshell under set -eu, as it does in the
# driver, and its output goes to one file, which stands in for the CI log.
run_create() {
	RUN_DIR=$SELFTEST_DIR/$1
	shift
	STUB_DIR=$RUN_DIR/stub
	mkdir -p "$STUB_DIR"
	printf '0\n' >"$STUB_DIR/creates"
	: >"$STUB_DIR/deletes"
	: >"$STUB_DIR/followers"
	: >"$STUB_DIR/unexpected"
	run_attempt=0
	for run_outcome in "$@"; do
		run_attempt=$((run_attempt + 1))
		printf '%s\n' "$run_outcome" >"$STUB_DIR/outcome-$run_attempt"
	done
	set +e
	# shellcheck disable=SC2094 # The stub reads the log the run is writing; that read is the streaming check.
	(
		set -e
		# The names below are read by the extracted functions, and the stubs
		# below them are called by those functions. shellcheck follows
		# neither.
		# shellcheck disable=SC2034
		WORK_DIR=$RUN_DIR
		# shellcheck disable=SC2034
		CLUSTER_NAME=$CLUSTER
		# shellcheck disable=SC2034
		KIND_NODE_IMAGE=kindest/node:selftest
		# shellcheck disable=SC2034
		KIND_CONFIG=$RUN_DIR/kind.yaml
		KUBECONFIG_FILE=$RUN_DIR/kubeconfig
		CLUSTER_CREATED=0

		# shellcheck disable=SC2317,SC2329
		kind() {
			case "$1 ${2:-}" in
				'create cluster')
					stub_attempt=$(($(cat "$STUB_DIR/creates") + 1))
					printf '%s\n' "$stub_attempt" >"$STUB_DIR/creates"
					if [ -e "$KUBECONFIG_FILE" ]; then
						stub_kubeconfig=present
					else
						stub_kubeconfig=absent
					fi
					printf 'claimed=%s kubeconfig=%s\n' "$CLUSTER_CREATED" "$stub_kubeconfig" \
						>"$STUB_DIR/seen-$stub_attempt"
					# kind is still running here. Output the driver held
					# back until kind ended would not be in the log yet.
					started_line "$stub_attempt" >&2
					printf '\n' >&2
					stub_stream=held
					stub_poll=0
					while [ "$stub_poll" -lt 50 ]; do
						if grep -Fq "$(started_line "$stub_attempt")" "$RUN_DIR/output"; then
							stub_stream=streamed
							break
						fi
						stub_poll=$((stub_poll + 1))
						sleep 0.1
					done
					printf '%s\n' "$stub_stream" >"$STUB_DIR/stream-$stub_attempt"
					stub_outcome=unscripted
					[ ! -f "$STUB_DIR/outcome-$stub_attempt" ] ||
						stub_outcome=$(cat "$STUB_DIR/outcome-$stub_attempt")
					case "$stub_outcome" in
						created)
							printf '%s\n' 'stub kubeconfig' >"$KUBECONFIG_FILE"
							printf 'Set kubectl context to "kind-%s"\n' "$CLUSTER" >&2
							return 0
							;;
						port-race)
							# kind writes no kubeconfig before its nodes
							# exist. This stands in for anything a failed
							# attempt could leave there.
							printf '%s\n' 'stale kubeconfig' >"$KUBECONFIG_FILE"
							printf 'ERROR: failed to create cluster: command "docker run --name %s-control-plane2 ..." failed with error: exit status 125\n' "$CLUSTER" >&2
							printf 'docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint %s-control-plane2 (0f3c): Bind for 127.0.0.1:35105 failed: port is already allocated\n' "$CLUSTER" >&2
							return 1
							;;
						in-use)
							printf 'ERROR: failed to create cluster: command "docker run --name %s-external-load-balancer ..." failed with error: exit status 125\n' "$CLUSTER" >&2
							printf '%s\n' 'docker: Error response from daemon: failed to bind host port for 127.0.0.1:29123:172.18.0.6:6443/tcp: address already in use' >&2
							# kind exits 1 on every failure. Another status
							# shows the one the run reports is kind's own,
							# not the pipe's.
							return 4
							;;
						*)
							printf 'stub kind: attempt %s was not scripted\n' "$stub_attempt" >&2
							return 97
							;;
					esac
					;;
				'delete cluster')
					shift 2
					printf '%s after attempt %s\n' "$*" "$(cat "$STUB_DIR/creates")" >>"$STUB_DIR/deletes"
					;;
				*)
					printf '%s\n' "$*" >>"$STUB_DIR/unexpected"
					return 2
					;;
			esac
		}

		# shellcheck disable=SC2317,SC2329
		debug_logs_start_following() {
			printf 'started with claimed=%s\n' "$CLUSTER_CREATED" >>"$STUB_DIR/followers"
		}

		# shellcheck disable=SC2317,SC2329
		fail() {
			printf '%s\n' "$*" >"$STUB_DIR/failure"
			exit 9
		}

		# shellcheck source=/dev/null
		. "$FUNCTIONS_FILE"
		create_kind_cluster
	) >"$RUN_DIR/output" 2>&1
	RUN_STATUS=$?
	set -e
	RUN_CREATES=$(cat "$STUB_DIR/creates")
	RUN_DELETES=$(cat "$STUB_DIR/deletes")
	RUN_FOLLOWERS=$(cat "$STUB_DIR/followers")
	RUN_UNEXPECTED=$(cat "$STUB_DIR/unexpected")
	RUN_OUTPUT=$(cat "$RUN_DIR/output")
	RUN_FAILURE=
	[ ! -f "$STUB_DIR/failure" ] || RUN_FAILURE=$(cat "$STUB_DIR/failure")
}

RETRY_NOTICE='e2e: kind create cluster lost a race for a host port ("port is already allocated"); deleting the half-made cluster and creating it once more'

# What every run holds, whatever kind answered: the teardown owns the cluster
# before kind runs, the follower starts once and before kind, nothing but
# create and delete reached kind, and each attempt's output reached the log
# while kind was running.
assert_common() {
	[ "$RUN_FOLLOWERS" = 'started with claimed=1' ] ||
		selftest_fail "$1: the debug-log follower was started as [$RUN_FOLLOWERS], want once, after the cluster was claimed"
	[ -z "$RUN_UNEXPECTED" ] ||
		selftest_fail "$1: kind was called with [$RUN_UNEXPECTED], which is neither a create nor a delete"
	common_attempt=1
	while [ "$common_attempt" -le "$RUN_CREATES" ]; do
		common_seen=$(cat "$RUN_DIR/stub/seen-$common_attempt")
		case "$common_seen" in
			claimed=1\ *) ;;
			*) selftest_fail "$1: attempt $common_attempt ran with [$common_seen], so a failure there would not reach the teardown" ;;
		esac
		common_stream=$(cat "$RUN_DIR/stub/stream-$common_attempt")
		[ "$common_stream" = streamed ] ||
			selftest_fail "$1: the first line of attempt $common_attempt was not in the run's log after 5s while kind was still running; the driver holds kind's output back"
		case "$RUN_OUTPUT" in
			*"$(started_line "$common_attempt")"*) ;;
			*) selftest_fail "$1: the run's log does not carry attempt $common_attempt's output: $RUN_OUTPUT" ;;
		esac
		common_attempt=$((common_attempt + 1))
	done
}

# Row: the race happened once. The half-made cluster is deleted with this
# run's kubeconfig, after the first attempt, and the second attempt starts
# with no kubeconfig and succeeds.
row_port_race_then_created() {
	row=port-race-then-created
	run_create "$row" port-race created
	[ "$RUN_STATUS" -eq 0 ] ||
		selftest_fail "$row: ended with status $RUN_STATUS, want a created cluster: $RUN_FAILURE"
	[ "$RUN_CREATES" -eq 2 ] ||
		selftest_fail "$row: kind create cluster ran $RUN_CREATES times, want twice"
	[ "$RUN_DELETES" = "--name $CLUSTER --kubeconfig $RUN_DIR/kubeconfig after attempt 1" ] ||
		selftest_fail "$row: the half-made cluster was deleted as [$RUN_DELETES], want once after attempt 1, with this run's kubeconfig"
	row_seen=$(cat "$RUN_DIR/stub/seen-2")
	[ "$row_seen" = 'claimed=1 kubeconfig=absent' ] ||
		selftest_fail "$row: the second attempt ran with [$row_seen], want no kubeconfig left from the first"
	[ "$(grep -cxF "$RETRY_NOTICE" "$RUN_DIR/output" || true)" -eq 1 ] ||
		selftest_fail "$row: the run's log does not say once that it is creating the cluster again and why: $RUN_OUTPUT"
	case "$RUN_OUTPUT" in
		*'Bind for 127.0.0.1:35105 failed: port is already allocated'*) ;;
		*) selftest_fail "$row: the run's log does not carry the refusal that caused the retry: $RUN_OUTPUT" ;;
	esac
	assert_common "$row"
}

# Row: a port that another process holds is a failure the retry is not for, so
# the run ends at the first attempt and reports kind's status.
row_other_failure_not_retried() {
	row=other-failure-not-retried
	run_create "$row" in-use created
	[ "$RUN_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $RUN_STATUS, want the run to fail"
	[ "$RUN_CREATES" -eq 1 ] ||
		selftest_fail "$row: kind create cluster ran $RUN_CREATES times, want once"
	[ -z "$RUN_DELETES" ] ||
		selftest_fail "$row: the cluster was deleted as [$RUN_DELETES] before the run failed; the teardown owns that"
	[ "$RUN_FAILURE" = 'kind create cluster failed with status 4; its output is above' ] ||
		selftest_fail "$row: the run failed with [$RUN_FAILURE], want kind's own status 4 and no retry"
	if grep -qxF "$RETRY_NOTICE" "$RUN_DIR/output"; then
		selftest_fail "$row: the run's log announces a retry for a failure that is not a port race"
	fi
	case "$RUN_OUTPUT" in
		*'address already in use'*) ;;
		*) selftest_fail "$row: the run's log does not carry kind's failure: $RUN_OUTPUT" ;;
	esac
	assert_common "$row"
}

# Row: the race happened twice. One retry is all there is, so the second
# failure ends the run.
row_two_port_races_fail() {
	row=two-port-races-fail
	run_create "$row" port-race port-race created
	[ "$RUN_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $RUN_STATUS, want the run to fail"
	[ "$RUN_CREATES" -eq 2 ] ||
		selftest_fail "$row: kind create cluster ran $RUN_CREATES times, want twice"
	[ "$RUN_DELETES" = "--name $CLUSTER --kubeconfig $RUN_DIR/kubeconfig after attempt 1" ] ||
		selftest_fail "$row: the half-made cluster was deleted as [$RUN_DELETES], want once after attempt 1"
	[ "$RUN_FAILURE" = 'kind create cluster failed again with status 1 after one retry for an allocated host port; its output is above' ] ||
		selftest_fail "$row: the run failed with [$RUN_FAILURE], want the failure after the one retry"
	assert_common "$row"
}

# Each row runs in a subshell under set -e of its own, so one that fails ends
# itself and not the others.
FAILED_ROWS=
run_row() {
	set +e
	(
		set -e
		"$1"
	)
	row_status=$?
	set -e
	[ "$row_status" -eq 0 ] || FAILED_ROWS="$FAILED_ROWS $1"
}

run_row row_port_race_then_created
run_row row_other_failure_not_retried
run_row row_two_port_races_fail
[ -z "$FAILED_ROWS" ] || selftest_fail "FAIL:$FAILED_ROWS"

printf '%s\n' 'e2e kind create retry self-test: PASS a port race is created once more, another failure is not, and a second race fails the run'
