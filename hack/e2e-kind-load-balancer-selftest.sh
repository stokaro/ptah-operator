#!/bin/sh

set -eu

# Prove that the bootstrap gives the Docker host kind's load balancer image
# itself, as the pinned image, and removes only what it pulled.
#
# kind v0.33.0 runs the load balancer of a cluster with more than one control
# plane from docker.io/envoyproxy/envoy:v1.36.2 and leaves the pull to docker
# run, which tries once. support/kubernetes.json pins that name by digest. This
# drives hack/e2e-kind.sh's own functions against a stubbed Docker host and
# refuses them unless
#
#   - an image the host lacks is pulled by the pinned digest, with the
#     retries the node image gets, given the name kind runs, and recorded as
#     this run's, and a pull that fails three times fails the run,
#   - a name the host already has is used only when it is the pinned image,
#     is never pulled or tagged over, and is not removed afterwards,
#   - a configuration with one control plane, which kind gives no load
#     balancer, prepares nothing, while the driver's own template prepares,
#   - a load balancer kind ran under another name or from another image fails
#     the run, and
#   - the cleanup removes the image this run pulled, counts an image that is
#     already gone as removed, and reports one it could not remove.
#
# It also derives the two names from the manifest's pin with the driver's own
# lines, for the pin in the repository and for a registry with a port.
#
# Every row runs, and each one that fails says so, so a mutated driver shows
# every row it breaks.
#
# Usage: hack/e2e-kind-load-balancer-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
CLUSTER=selftest-ha
DRIVER=$ROOT_DIR/hack/e2e-kind.sh

selftest_fail() {
	printf 'e2e kind load balancer self-test: %s\n' "$*" >&2
	exit 1
}

SELFTEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-kind-load-balancer-selftest.XXXXXX")
trap 'rm -rf -- "$SELFTEST_DIR"' EXIT

# The functions, extracted from the driver so this measures what runs rather
# than a copy of it.
FUNCTIONS_FILE=$SELFTEST_DIR/functions.sh
for function_name in pull_image_with_retries prepare_kind_load_balancer_image \
	assert_kind_load_balancer_image remove_kind_load_balancer_image; do
	awk -v name="$function_name" '
		$0 == name "() {" { capture = 1 }
		capture { print }
		capture && /^\}$/ { exit }
	' "$DRIVER" >>"$FUNCTIONS_FILE"
	grep -Eq "^$function_name\\(\\) \\{$" "$FUNCTIONS_FILE" ||
		selftest_fail "the extraction from hack/e2e-kind.sh does not carry $function_name"
done

# The two lines that derive the names from the pin, extracted the same way.
NAMES_FILE=$SELFTEST_DIR/names.sh
grep -E '^KIND_LOAD_BALANCER_(REFERENCE|PINNED)=' "$DRIVER" >"$NAMES_FILE" || :
[ "$(wc -l <"$NAMES_FILE" | tr -d ' ')" -eq 2 ] ||
	selftest_fail "hack/e2e-kind.sh does not derive KIND_LOAD_BALANCER_REFERENCE and KIND_LOAD_BALANCER_PINNED in one line each"

PIN=$(jq -er '.kindLoadBalancer.image' "$ROOT_DIR/support/kubernetes.json") ||
	selftest_fail "support/kubernetes.json pins no kindLoadBalancer.image"
# The names the driver must derive, worked out here without its expansions.
REFERENCE=$(printf '%s\n' "$PIN" | sed 's/@sha256:.*$//')
PINNED=$(printf '%s\n' "$PIN" | sed -E 's/:[^:/@]+@sha256:/@sha256:/')
[ "$REFERENCE" != "$PIN" ] && [ "$PINNED" != "$PIN" ] ||
	selftest_fail "the pin $PIN is not a name:tag@sha256:digest this self-test can split"

# The image the stubbed registry serves under the pinned digest, and another.
PINNED_ID=sha256:pinned-image
OTHER_ID=sha256:other-image

ref_file() {
	printf '%s' "$1" | tr '/:@' '___'
}

# host_has puts an image on the stubbed Docker host under a name.
host_has() {
	printf '%s\n' "$2" >"$STUB_DIR/images/$(ref_file "$1")"
}

# new_row gives a row a Docker host with no image on it and the driver's own
# kind configuration, which has three control planes.
new_row() {
	ROW_DIR=$SELFTEST_DIR/$1
	STUB_DIR=$ROW_DIR/stub
	mkdir -p "$STUB_DIR/images"
	: >"$STUB_DIR/calls"
	printf '0\n' >"$STUB_DIR/pull-failures"
	ROW_CONFIG=$ROW_DIR/kind.yaml
	cp "$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" "$ROW_CONFIG"
	ROW_CREATED=0
	ROW_ID=
}

# run_steps runs the named driver functions in order against the row's
# Docker host, in a subshell under set -eu, as the driver runs them. remove
# runs in a list, as the cleanup calls it, and its status is recorded.
run_steps() {
	set +e
	(
		set -eu
		# The names below are read by the extracted functions, and the stubs
		# below them are called by those functions. shellcheck follows
		# neither.
		# shellcheck disable=SC2034
		DOCKER_CONTEXT=selftest-context
		# shellcheck disable=SC2034
		SELECTED_DOCKER_CONTEXT=selftest
		# shellcheck disable=SC2034
		CLUSTER_NAME=$CLUSTER
		# shellcheck disable=SC2034
		EXPECTED_KIND_VERSION=v0.33.0
		# shellcheck disable=SC2034
		KIND_CONFIG=$ROW_CONFIG
		# The row that derives these names runs in a subshell of its own.
		# shellcheck disable=SC2030
		KIND_LOAD_BALANCER_REFERENCE=$REFERENCE
		# shellcheck disable=SC2030
		KIND_LOAD_BALANCER_PINNED=$PINNED
		KIND_LOAD_BALANCER_IMAGE_CREATED=$ROW_CREATED
		KIND_LOAD_BALANCER_IMAGE_ID=$ROW_ID

		record_state() {
			printf 'created=%s id=%s\n' "$KIND_LOAD_BALANCER_IMAGE_CREATED" \
				"${KIND_LOAD_BALANCER_IMAGE_ID:-none}" >"$STUB_DIR/state"
		}

		# shellcheck disable=SC2317,SC2329
		docker() {
			[ "$1" != --context ] || shift 2
			printf '%s\n' "$*" >>"$STUB_DIR/calls"
			case "$1" in
				image)
					case "$2" in
						inspect)
							shift 2
							stub_format=
							if [ "$1" = --format ]; then
								stub_format=$2
								shift 2
							fi
							if [ ! -f "$STUB_DIR/images/$(ref_file "$1")" ]; then
								printf 'Error response from daemon: No such image: %s\n' "$1" >&2
								return 1
							fi
							[ -z "$stub_format" ] || cat "$STUB_DIR/images/$(ref_file "$1")"
							;;
						rm)
							if [ -f "$STUB_DIR/rm-refused" ]; then
								printf 'Error response from daemon: conflict: unable to remove repository reference "%s" (must force) - container 0f3c is using its referenced image\n' "$3" >&2
								return 1
							fi
							if [ ! -f "$STUB_DIR/images/$(ref_file "$3")" ]; then
								printf 'Error response from daemon: No such image: %s\n' "$3" >&2
								return 1
							fi
							# Removing the last tag removes the image under every
							# name, as Docker 29.7.2 did with the containerd image
							# store and as the classic store does.
							stub_id=$(cat "$STUB_DIR/images/$(ref_file "$3")")
							for stub_file in "$STUB_DIR"/images/*; do
								[ -f "$stub_file" ] || continue
								[ "$(cat "$stub_file")" != "$stub_id" ] || rm -f -- "$stub_file"
							done
							;;
						*) return 2 ;;
					esac
					;;
				pull)
					[ "$2" = "$PINNED" ] || return 2
					stub_failures=$(cat "$STUB_DIR/pull-failures")
					if [ "$stub_failures" -gt 0 ]; then
						printf '%s\n' "$((stub_failures - 1))" >"$STUB_DIR/pull-failures"
						printf '%s\n' 'failed to copy: httpReadSeeker: failed open: failed to do request: EOF' >&2
						return 1
					fi
					host_has "$2" "$PINNED_ID"
					;;
				tag)
					[ -f "$STUB_DIR/images/$(ref_file "$2")" ] || return 1
					cp "$STUB_DIR/images/$(ref_file "$2")" "$STUB_DIR/images/$(ref_file "$3")"
					;;
				container)
					[ "$2" = inspect ] && [ -f "$STUB_DIR/container" ] || return 1
					cat "$STUB_DIR/container"
					;;
				info) ;;
				*) return 2 ;;
			esac
		}

		# shellcheck disable=SC2317,SC2329
		sleep() {
			printf 'sleep %s\n' "$1" >>"$STUB_DIR/calls"
		}

		# shellcheck disable=SC2317,SC2329
		fail() {
			printf '%s\n' "$*" >"$STUB_DIR/failure"
			record_state
			exit 9
		}

		# shellcheck source=/dev/null
		. "$FUNCTIONS_FILE"
		record_state
		for step in "$@"; do
			case "$step" in
				prepare) prepare_kind_load_balancer_image ;;
				assert) assert_kind_load_balancer_image ;;
				remove)
					step_status=0
					remove_kind_load_balancer_image || step_status=$?
					printf '%s\n' "$step_status" >"$STUB_DIR/remove-status"
					;;
			esac
			record_state
		done
	) >"$ROW_DIR/output" 2>&1
	ROW_STATUS=$?
	set -e
	ROW_STATE=$(cat "$STUB_DIR/state" 2>/dev/null || printf 'unrecorded')
	ROW_FAILURE=
	[ ! -f "$STUB_DIR/failure" ] || ROW_FAILURE=$(cat "$STUB_DIR/failure")
	ROW_REMOVE_STATUS=
	[ ! -f "$STUB_DIR/remove-status" ] || ROW_REMOVE_STATUS=$(cat "$STUB_DIR/remove-status")
	ROW_OUTPUT=$(cat "$ROW_DIR/output")
	ROW_IMAGES=$(cd "$STUB_DIR/images" && find . -type f | sed 's|^\./||' | LC_ALL=C sort | tr '\n' ' ')
}

# calls counts the Docker calls the row made that match an extended regular
# expression over the whole line.
calls() {
	grep -Ecx -- "$1" "$STUB_DIR/calls" || :
}

# quote escapes what an extended regular expression would read as an operator.
quote() {
	# shellcheck disable=SC2016 # The expression is sed's; nothing in it expands.
	printf '%s\n' "$1" | sed 's/[][\.*^$(){}?+|]/\\&/g'
}

PULL_PINNED="pull $(quote "$PINNED")"
TAG_PINNED="tag $(quote "$PINNED") $(quote "$REFERENCE")"
RM_ANY='image rm .*'
PREPARED_NOTICE="e2e: kind load balancer image $REFERENCE is the pinned $PINNED"

# The line kind's load balancer reports when it runs the prepared image.
container_runs() {
	printf '%s %s\n' "$1" "$2" >"$STUB_DIR/container"
}

# derive_names prints the two names the driver's own lines derive from the pin
# $1. Those lines assign both, which shellcheck cannot see through the source.
# shellcheck disable=SC2031,SC2034
derive_names() (
	KIND_LOAD_BALANCER_IMAGE=$1
	# shellcheck source=/dev/null
	. "$NAMES_FILE"
	printf '%s %s\n' "$KIND_LOAD_BALANCER_REFERENCE" "$KIND_LOAD_BALANCER_PINNED"
)

# Row: the driver's lines split the repository's pin, and a pin whose registry
# carries a port, into the name kind runs and the pinned name.
row_derived_names() {
	row='derived-names'
	derived=$(derive_names "$PIN")
	[ "$derived" = "$REFERENCE $PINNED" ] ||
		selftest_fail "$row: the driver derived [$derived] from $PIN, want [$REFERENCE $PINNED]"
	port_digest=sha256:$(printf '%064d' 7)
	derived=$(derive_names "registry.example:5000/kind/haproxy:2.9-alpine@$port_digest")
	[ "$derived" = "registry.example:5000/kind/haproxy:2.9-alpine registry.example:5000/kind/haproxy@$port_digest" ] ||
		selftest_fail "$row: the driver derived [$derived] from a pin whose registry has a port"
}

# Row: the host has neither name. Two failed pulls are retried, the third
# succeeds, the image gets the name kind runs, the run records it as its own,
# kind's load balancer runs it, and the cleanup removes it.
row_absent_pulled_with_retries() {
	row=absent-pulled-with-retries
	new_row "$row"
	printf '2\n' >"$STUB_DIR/pull-failures"
	container_runs "$REFERENCE" "$PINNED_ID"
	run_steps prepare assert remove
	[ "$ROW_STATUS" -eq 0 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want a prepared image: $ROW_FAILURE $ROW_OUTPUT"
	[ "$(calls "$PULL_PINNED")" -eq 3 ] && [ "$(calls 'pull .*')" -eq 3 ] ||
		selftest_fail "$row: pulled as [$(grep '^pull ' "$STUB_DIR/calls" || :)], want the pinned name three times"
	[ "$(calls 'sleep 5')" -eq 2 ] ||
		selftest_fail "$row: waited [$(grep '^sleep ' "$STUB_DIR/calls" || :)] between pulls, want 5 seconds twice"
	[ "$(calls "$TAG_PINNED")" -eq 1 ] && [ "$(calls 'tag .*')" -eq 1 ] ||
		selftest_fail "$row: tagged as [$(grep '^tag ' "$STUB_DIR/calls" || :)], want the pinned image named $REFERENCE once"
	[ "$ROW_STATE" = "created=1 id=$PINNED_ID" ] ||
		selftest_fail "$row: left [$ROW_STATE], want the image recorded as this run's, by its ID"
	case "$ROW_OUTPUT" in
		*"$PREPARED_NOTICE"*) ;;
		*) selftest_fail "$row: the run's log does not say which image kind will run: $ROW_OUTPUT" ;;
	esac
	[ "$ROW_REMOVE_STATUS" = 0 ] && [ -z "$ROW_IMAGES" ] ||
		selftest_fail "$row: the cleanup returned [$ROW_REMOVE_STATUS] and left [$ROW_IMAGES], want both names gone and success"
}

# Row: every pull fails. The run fails after the third, names the image, and
# has already recorded it as its own, so the cleanup removes what a pull that
# failed late could have left.
row_absent_pull_fails() {
	row=absent-pull-fails
	new_row "$row"
	printf '5\n' >"$STUB_DIR/pull-failures"
	run_steps prepare
	[ "$ROW_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the run to fail"
	[ "$ROW_FAILURE" = "kind load balancer image $PINNED could not be pulled in 3 attempts" ] ||
		selftest_fail "$row: failed with [$ROW_FAILURE], want the image and the three attempts named"
	[ "$(calls 'pull .*')" -eq 3 ] && [ "$(calls 'tag .*')" -eq 0 ] ||
		selftest_fail "$row: made $(calls 'pull .*') pulls and $(calls 'tag .*') tags, want three pulls and no tag"
	[ "$ROW_STATE" = 'created=1 id=none' ] ||
		selftest_fail "$row: left [$ROW_STATE], want the image recorded as this run's before the pull"
}

# Row: the host has the name kind runs, and it is the pinned image. Nothing is
# pulled or tagged, the image is not this run's, and the cleanup leaves it.
row_named_and_pinned_used() {
	row=named-and-pinned-used
	new_row "$row"
	host_has "$REFERENCE" "$PINNED_ID"
	host_has "$PINNED" "$PINNED_ID"
	container_runs "$REFERENCE" "$PINNED_ID"
	run_steps prepare assert remove
	[ "$ROW_STATUS" -eq 0 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the image used as it is: $ROW_FAILURE"
	[ "$(calls 'pull .*')" -eq 0 ] && [ "$(calls 'tag .*')" -eq 0 ] && [ "$(calls "$RM_ANY")" -eq 0 ] ||
		selftest_fail "$row: made $(calls 'pull .*') pulls, $(calls 'tag .*') tags and $(calls "$RM_ANY") removals of an image the host already had"
	[ "$ROW_STATE" = "created=0 id=$PINNED_ID" ] ||
		selftest_fail "$row: left [$ROW_STATE], want the image used and not recorded as this run's"
	[ "$ROW_REMOVE_STATUS" = 0 ] ||
		selftest_fail "$row: the cleanup returned [$ROW_REMOVE_STATUS] for an image it was not to touch"
}

# Row: the host's name kind runs holds another image. kind would run that, so
# the run fails, and it neither pulls nor moves the name.
row_named_other_image_refused() {
	row=named-other-image-refused
	new_row "$row"
	host_has "$REFERENCE" "$OTHER_ID"
	host_has "$PINNED" "$PINNED_ID"
	run_steps prepare
	[ "$ROW_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the run to fail"
	[ "$ROW_FAILURE" = "$REFERENCE on the Docker host is image $OTHER_ID, not the pinned $PINNED ($PINNED_ID)" ] ||
		selftest_fail "$row: failed with [$ROW_FAILURE], want both images named"
	[ "$(calls 'pull .*')" -eq 0 ] && [ "$(calls 'tag .*')" -eq 0 ] ||
		selftest_fail "$row: pulled or tagged over a name the host already had"
	[ "$ROW_STATE" = 'created=0 id=none' ] ||
		selftest_fail "$row: left [$ROW_STATE], want nothing recorded"
}

# Row: the host has the name kind runs and not the pinned digest, which a
# build or a load under that name leaves. Nothing vouches for it, so the run
# fails without pulling over it.
row_named_without_pinned_refused() {
	row=named-without-pinned-refused
	new_row "$row"
	host_has "$REFERENCE" "$OTHER_ID"
	run_steps prepare
	[ "$ROW_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the run to fail"
	[ "$ROW_FAILURE" = "the Docker host has $REFERENCE but not the pinned $PINNED" ] ||
		selftest_fail "$row: failed with [$ROW_FAILURE], want the missing pinned image named"
	[ "$(calls 'pull .*')" -eq 0 ] && [ "$(calls 'tag .*')" -eq 0 ] ||
		selftest_fail "$row: pulled or tagged over a name the host already had"
}

# Row: the host has the pinned image under its digest only. It is given the
# name kind runs without a pull and is not recorded as this run's.
row_pinned_only_tagged() {
	row=pinned-only-tagged
	new_row "$row"
	host_has "$PINNED" "$PINNED_ID"
	run_steps prepare
	[ "$ROW_STATUS" -eq 0 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the image named: $ROW_FAILURE"
	[ "$(calls 'pull .*')" -eq 0 ] && [ "$(calls "$TAG_PINNED")" -eq 1 ] ||
		selftest_fail "$row: made $(calls 'pull .*') pulls and $(calls "$TAG_PINNED") tags, want no pull and one tag"
	[ "$ROW_STATE" = "created=0 id=$PINNED_ID" ] ||
		selftest_fail "$row: left [$ROW_STATE], want an image this run did not pull"
}

# Row: one control plane, which kind gives no load balancer. Nothing reaches
# Docker, and the check after kind has nothing to hold.
row_one_control_plane_skipped() {
	row=one-control-plane-skipped
	new_row "$row"
	printf '%s\n' 'kind: Cluster' 'apiVersion: kind.x-k8s.io/v1alpha4' 'nodes:' \
		'  - role: control-plane' '  - role: worker' >"$ROW_CONFIG"
	run_steps prepare assert
	[ "$ROW_STATUS" -eq 0 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS: $ROW_FAILURE"
	[ ! -s "$STUB_DIR/calls" ] ||
		selftest_fail "$row: called Docker as [$(tr '\n' ';' <"$STUB_DIR/calls")] for a cluster with no load balancer"
	[ "$ROW_STATE" = 'created=0 id=none' ] ||
		selftest_fail "$row: left [$ROW_STATE], want nothing prepared"
}

# Row: the driver's configuration with the isolation worker appended still
# counts as one with a load balancer.
row_isolation_template_prepared() {
	row=isolation-template-prepared
	new_row "$row"
	cat "$ROOT_DIR/testdata/e2e/kind-isolation-worker.yaml.tmpl" >>"$ROW_CONFIG"
	run_steps prepare
	[ "$ROW_STATUS" -eq 0 ] && [ "$(calls "$PULL_PINNED")" -eq 1 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS and $(calls "$PULL_PINNED") pulls, want the image pulled once: $ROW_FAILURE"
}

# Row: kind ran its load balancer under another name, which is what a pin read
# from another kind version leaves. The run fails and says what to pin.
row_kind_ran_another_name_refused() {
	row=kind-ran-another-name-refused
	new_row "$row"
	host_has "$REFERENCE" "$PINNED_ID"
	host_has "$PINNED" "$PINNED_ID"
	container_runs docker.io/envoyproxy/envoy:v9.9.9 "$OTHER_ID"
	run_steps prepare assert
	[ "$ROW_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the run to fail"
	[ "$ROW_FAILURE" = "kind ran its load balancer as 'docker.io/envoyproxy/envoy:v9.9.9 $OTHER_ID', not the pinned $REFERENCE ($PINNED_ID); pin the image kind v0.33.0 names in pkg/cluster/internal/loadbalancer" ] ||
		selftest_fail "$row: failed with [$ROW_FAILURE], want what kind ran and what to pin"
}

# Row: kind ran the name but not the prepared image, as it would after the
# name moved between the preparation and the create.
row_kind_ran_another_image_refused() {
	row=kind-ran-another-image-refused
	new_row "$row"
	host_has "$REFERENCE" "$PINNED_ID"
	host_has "$PINNED" "$PINNED_ID"
	container_runs "$REFERENCE" "$OTHER_ID"
	run_steps prepare assert
	[ "$ROW_STATUS" -eq 9 ] ||
		selftest_fail "$row: ended with status $ROW_STATUS, want the run to fail"
	case "$ROW_FAILURE" in
		"kind ran its load balancer as '$REFERENCE $OTHER_ID', not the pinned $REFERENCE ($PINNED_ID);"*) ;;
		*) selftest_fail "$row: failed with [$ROW_FAILURE], want the image kind ran named" ;;
	esac
}

# Row: the image this run pulled cannot be removed, as when another cluster
# on the host started from it. The cleanup reports it and fails.
row_removal_refused_reported() {
	row=removal-refused-reported
	new_row "$row"
	host_has "$REFERENCE" "$PINNED_ID"
	host_has "$PINNED" "$PINNED_ID"
	: >"$STUB_DIR/rm-refused"
	ROW_CREATED=1
	run_steps remove
	[ "$ROW_REMOVE_STATUS" = 1 ] ||
		selftest_fail "$row: the cleanup returned [$ROW_REMOVE_STATUS], want a failure"
	case "$ROW_OUTPUT" in
		*"e2e: could not remove kind load balancer image $REFERENCE from Docker context selftest"*) ;;
		*) selftest_fail "$row: the run's log does not name the image it could not remove: $ROW_OUTPUT" ;;
	esac
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

run_row row_derived_names
run_row row_absent_pulled_with_retries
run_row row_absent_pull_fails
run_row row_named_and_pinned_used
run_row row_named_other_image_refused
run_row row_named_without_pinned_refused
run_row row_pinned_only_tagged
run_row row_one_control_plane_skipped
run_row row_isolation_template_prepared
run_row row_kind_ran_another_name_refused
run_row row_kind_ran_another_image_refused
run_row row_removal_refused_reported
[ -z "$FAILED_ROWS" ] || selftest_fail "FAIL:$FAILED_ROWS"

printf '%s\n' 'e2e kind load balancer self-test: PASS the pinned image is pulled with retries and named, a name on the host is used only when pinned, and only a pulled image is removed'
