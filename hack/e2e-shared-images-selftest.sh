#!/bin/sh

set -eu

# Prove what the harness refuses when it is handed prepared images.
#
# A matrix that builds its images once has to be told which images they are, and
# the answer arrives as files. Everything that makes that safe is a refusal in
# hack/e2e-kind.sh: the manifest's commit is this run's commit, the executor is
# the one the compatibility catalog pins, the loaded image is the one the
# manifest declared, and each image says which role it plays so the synthetic
# next release cannot be installed as the candidate. A refusal nothing exercises
# is a comment.
#
# This extracts those functions from the driver and runs them against a Docker
# that answers from a file, so every refusal is measured rather than read.
#
# Usage: hack/e2e-shared-images-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

fail() {
	printf 'e2e shared images self-test: %s\n' "$*" >&2
	exit 1
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-shared-images-selftest.XXXXXX")
trap 'rm -rf -- "$WORK_DIR"' EXIT

FUNCTIONS_FILE=$WORK_DIR/functions.sh
awk '
	/^image_identity\(\) \{$/ { capture = 1 }
	capture { print }
	/wrote the four task images/ { finishing = 1 }
	finishing && /^\}$/ { exit }
' "$ROOT_DIR/hack/e2e-kind.sh" >"$FUNCTIONS_FILE"
for required_function in image_identity image_label_value load_prebuilt_images export_task_images; do
	grep -q "^$required_function() {" "$FUNCTIONS_FILE" ||
		fail "the extraction from hack/e2e-kind.sh does not carry $required_function"
done

# A Docker that answers from files. It serves the two reads the refusals make --
# an image's identity and one of its labels -- and treats load, tag and save as
# the transfers they are. Nothing here reaches a daemon.
FAKE_BIN=$WORK_DIR/bin
mkdir -p "$FAKE_BIN"
cat >"$FAKE_BIN/docker" <<'FAKE'
#!/bin/sh
set -eu
state=$FAKE_IMAGE_STATE
key() { printf '%s' "$1" | tr -c 'A-Za-z0-9' '_'; }
# Drop the context selector the harness always passes.
[ "${1:-}" = --context ] && shift 2
case "${1:-}" in
	image)
		[ "${2:-}" = inspect ] || exit 2
		shift 2
		format=
		[ "${1:-}" = --format ] && { format=$2; shift 2; }
		reference=$1
		case "$format" in
			*'.Id'*)
				identity_file="$state/$(key "$reference").id"
				[ -f "$identity_file" ] || exit 1
				cat "$identity_file"
				;;
			*Labels*)
				label=$(printf '%s' "$format" | sed -n 's/.*"\(.*\)".*/\1/p')
				label_file="$state/$(key "$reference").$(key "$label")"
				[ -f "$label_file" ] && cat "$label_file"
				;;
			*)
				identity_file="$state/$(key "$reference").id"
				[ -f "$identity_file" ] || exit 1
				;;
		esac
		;;
	load)
		# The tarball's own name is what a load would publish; the state says
		# what appeared.
		exit 0
		;;
	tag)
		printf '%s' "$(cat "$state/$(key "$2").id" 2>/dev/null || printf 'sha256:unknown')" \
			>"$state/$(key "$3").id"
		;;
	save)
		shift
		[ "${1:-}" = --output ] || exit 2
		: >"$2"
		;;
	*) exit 2 ;;
esac
FAKE
chmod 0755 "$FAKE_BIN/docker"

EXPECTED_REVISION=1111111111111111111111111111111111111111
PTAH_PIN=2222222222222222222222222222222222222222
PREPARED_DIR=$WORK_DIR/task-images
mkdir -p "$PREPARED_DIR"

# write_prepared writes a manifest and the image state a load would produce.
# Every case starts from what a correct preparation job leaves behind and then
# changes exactly one thing, which is the only way the message it earns is
# attributable to that thing.
write_prepared() {
	prepared_revision=${1:-$EXPECTED_REVISION}
	prepared_ptah=${2:-$PTAH_PIN}
	prepared_next_sequence=${3:-2}
	rm -rf "$PREPARED_DIR" "$WORK_DIR/state"
	mkdir -p "$PREPARED_DIR" "$WORK_DIR/state"
	for prepared_role in operator next-operator fixture executor; do
		: >"$PREPARED_DIR/$prepared_role.tar"
	done
	jq -n \
		--arg revision "$prepared_revision" \
		--arg ptah "$prepared_ptah" \
		--arg sequence "$prepared_next_sequence" \
		'{operatorRevision: $revision, ptahCommit: $ptah, ptahVersion: "v0.7.0",
		  currentReleaseSequence: "1", nextReleaseSequence: $sequence,
		  images: [
		    {role: "operator", file: "operator.tar", reference: "prepared/operator:1", identity: "sha256:aa"},
		    {role: "next-operator", file: "next-operator.tar", reference: "prepared/next:1", identity: "sha256:bb"},
		    {role: "fixture", file: "fixture.tar", reference: "prepared/fixture:1", identity: "sha256:cc"},
		    {role: "executor", file: "executor.tar", reference: "prepared/executor:1", identity: "sha256:dd"}
		  ]}' >"$PREPARED_DIR/images.json"
	state_write prepared/operator:1 sha256:aa operator "$prepared_revision" 1
	state_write prepared/next:1 sha256:bb next-operator "$prepared_revision" "$prepared_next_sequence"
	state_write prepared/fixture:1 sha256:cc fixture "$prepared_revision" ''
	state_executor prepared/executor:1 sha256:dd "$prepared_ptah"
}

state_key() {
	printf '%s' "$1" | tr -c 'A-Za-z0-9' '_'
}

state_write() {
	state_reference=$1
	state_identity=$2
	state_role=$3
	state_revision=$4
	state_sequence=$5
	state_name=$(state_key "$state_reference")
	printf '%s' "$state_identity" >"$WORK_DIR/state/$state_name.id"
	printf '%s' "$state_role" >"$WORK_DIR/state/$state_name.$(state_key ptah.run/e2e-role)"
	printf '%s' "$state_revision" \
		>"$WORK_DIR/state/$state_name.$(state_key ptah.run/e2e-operator-revision)"
	if [ -n "$state_sequence" ]; then
		printf '%s' "$state_sequence" \
			>"$WORK_DIR/state/$state_name.$(state_key ptah.run/e2e-release-sequence)"
	fi
}

state_executor() {
	state_name=$(state_key "$1")
	printf '%s' "$2" >"$WORK_DIR/state/$state_name.id"
	printf '%s' executor >"$WORK_DIR/state/$state_name.$(state_key ptah.run/e2e-role)"
	printf '%s' "$3" >"$WORK_DIR/state/$state_name.$(state_key ptah.run/e2e-ptah-commit)"
}

# run_load runs the driver's own loading path against the prepared directory.
# The names below are read by the functions this sources, which shellcheck
# cannot follow, and the stubs are called from them for the same reason.
# shellcheck disable=SC2034,SC2329
run_load() {
	(
		PATH="$FAKE_BIN:$PATH"
		export PATH
		FAKE_IMAGE_STATE=$WORK_DIR/state
		export FAKE_IMAGE_STATE
		DOCKER_CONTEXT=selftest
		CONTROLLER_REVISION=$EXPECTED_REVISION
		E2E_PTAH_REVISION=$PTAH_PIN
		E2E_PTAH_VERSION=
		E2E_PREBUILT_IMAGE_DIR=$PREPARED_DIR
		CURRENT_RELEASE_SEQUENCE=1
		NEXT_RELEASE_SEQUENCE=2
		OPERATOR_IMAGE=local/operator:run
		NEXT_OPERATOR_IMAGE=local/next:run
		FIXTURE_BUILD_IMAGE=local/fixture:run
		PTAH_IMAGE=local/executor:run
		TASK_IMAGE_ROLES='operator next-operator fixture executor'
		IMAGE_CREATED=0
		add_created_image() { :; }
		fail() {
			printf 'refused: %s\n' "$*"
			exit 1
		}
		task_image_for_role() {
			case $1 in
				operator) printf '%s' "$OPERATOR_IMAGE" ;;
				next-operator) printf '%s' "$NEXT_OPERATOR_IMAGE" ;;
				fixture) printf '%s' "$FIXTURE_BUILD_IMAGE" ;;
				executor) printf '%s' "$PTAH_IMAGE" ;;
				*) fail "no task image has the role $1" ;;
			esac
		}
		# shellcheck source=/dev/null
		. "$FUNCTIONS_FILE"
		load_prebuilt_images
		printf 'loaded %s\n' "$E2E_PTAH_VERSION"
	)
}

expect_refusal() {
	expected=$1
	refusal_status=0
	refusal_output=$(run_load 2>&1) || refusal_status=$?
	[ "$refusal_status" -ne 0 ] ||
		fail "images were loaded that should have been refused: $expected"
	case "$refusal_output" in
		*"$expected"*) ;;
		*) fail "the refusal reads \"$refusal_output\", and should name: $expected" ;;
	esac
}

# The correct preparation loads, and reports the Ptah version out of the
# manifest, because nothing else in the job knows it.
write_prepared
load_status=0
load_output=$(run_load 2>&1) || load_status=$?
[ "$load_status" -eq 0 ] ||
	fail "a correct set of prepared images was refused: $load_output"
case "$load_output" in
	*"loaded v0.7.0"*) ;;
	*) fail "the Ptah version was not read from the manifest: $load_output" ;;
esac

# An artifact from another commit. This is the substitution the transfer makes
# possible, and the one a content check on the images alone would pass.
write_prepared 3333333333333333333333333333333333333333
expect_refusal "were built from 3333333333333333333333333333333333333333"

# An executor built from a Ptah the catalog does not pin.
write_prepared "$EXPECTED_REVISION" 4444444444444444444444444444444444444444
expect_refusal "carries Ptah 4444444444444444444444444444444444444444"

# A next release that is not the sequence this run upgrades to.
write_prepared "$EXPECTED_REVISION" "$PTAH_PIN" 7
expect_refusal "sequence 7 and this run expects 2"

# A tarball whose loaded image is not the one the manifest declared.
write_prepared
printf '%s' sha256:ee >"$WORK_DIR/state/$(state_key prepared/operator:1).id"
expect_refusal "the manifest declares sha256:aa"

# An image that does not say which role it plays, which is what would let the
# synthetic next release be installed as the candidate.
write_prepared
rm -f "$WORK_DIR/state/$(state_key prepared/next:1).$(state_key ptah.run/e2e-role)"
expect_refusal "does not declare that role"

# An image whose own label disagrees with the manifest it travelled with.
write_prepared
printf '%s' 5555555555555555555555555555555555555555 \
	>"$WORK_DIR/state/$(state_key prepared/fixture:1).$(state_key ptah.run/e2e-operator-revision)"
expect_refusal "does not declare revision $EXPECTED_REVISION"

# A manifest that names a file the transfer did not carry.
write_prepared
rm -f "$PREPARED_DIR/fixture.tar"
expect_refusal "is missing from"

# A manifest with an entry missing its identity.
write_prepared
jq '.images |= map(if .role == "executor" then del(.identity) else . end)' \
	"$PREPARED_DIR/images.json" >"$PREPARED_DIR/images.json.next"
mv "$PREPARED_DIR/images.json.next" "$PREPARED_DIR/images.json"
expect_refusal "carries no complete entry for the executor image"

printf '%s\n' 'e2e shared images self-test: PASS prepared images are refused unless they are this run'
