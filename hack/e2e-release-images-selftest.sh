#!/bin/sh
# Each case is a script evaluated against the extracted driver functions, so
# its variables are read through eval and its expressions are quoted on purpose.
# shellcheck disable=SC2016,SC2034,SC2329

set -eu

# Prove what the harness refuses when it is told to install a prepared release.
#
# E2E_RELEASE_MANIFEST replaces the candidate operator, its runner and the
# executor with the digests a Release prepare run built, and the chart with the
# release's chart asset. What makes that a statement about the release is a set
# of refusals in hack/e2e-kind.sh: the manifest is complete and unambiguous, the
# executor is the one the catalog pins, the harness revision changed no runtime
# source of the release, the registry holds the digest that was released, and
# the chart is the asset byte for byte.
#
# This extracts those functions from the driver and runs each refusal beside
# the control it must admit, against a scratch Git repository and a fake copy
# tool. Nothing here reaches a registry or a cluster.
#
# Usage: hack/e2e-release-images-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

selftest_fail() {
	printf 'e2e release images self-test: %s\n' "$*" >&2
	exit 1
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-release-images-selftest.XXXXXX")
trap 'rm -rf -- "$WORK_DIR"' EXIT

FUNCTIONS_FILE=$WORK_DIR/functions.sh
for function_name in sha256 is_pinned_image release_manifest_field read_release_manifest \
	verify_release_chart copy_release_image; do
	awk -v name="$function_name" '
		$0 == name "() {" { capture = 1 }
		capture { print }
		capture && /^\}$/ { exit }
	' "$ROOT_DIR/hack/e2e-kind.sh" >>"$FUNCTIONS_FILE"
	grep -Eq "^$function_name\\(\\) \\{$" "$FUNCTIONS_FILE" ||
		selftest_fail "the extraction from hack/e2e-kind.sh does not carry $function_name"
done
RUNTIME_PATHS_LINE=$(grep -E '^RELEASE_RUNTIME_PATHS=' "$ROOT_DIR/hack/e2e-kind.sh")
[ -n "$RUNTIME_PATHS_LINE" ] || selftest_fail "the driver declares no release runtime paths"
printf '%s\n' "$RUNTIME_PATHS_LINE" >>"$FUNCTIONS_FILE"

# A repository whose first commit is the release source, whose second changes
# only the harness, and whose third changes a runtime source.
REPOSITORY=$WORK_DIR/repository
mkdir -p "$REPOSITORY/internal/controller" "$REPOSITORY/hack" "$REPOSITORY/charts/ptah-operator"
git -C "$REPOSITORY" init --quiet
git -C "$REPOSITORY" config user.email selftest@example.invalid
git -C "$REPOSITORY" config user.name selftest
git -C "$REPOSITORY" config commit.gpgsign false
printf 'package controller\n' >"$REPOSITORY/internal/controller/reconcile.go"
printf 'name: ptah-operator\n' >"$REPOSITORY/charts/ptah-operator/Chart.yaml"
printf '#!/bin/sh\n' >"$REPOSITORY/hack/e2e-kind.sh"
git -C "$REPOSITORY" add -A
git -C "$REPOSITORY" commit --quiet -m release
RELEASE_COMMIT=$(git -C "$REPOSITORY" rev-parse HEAD)
printf '#!/bin/sh\n# a harness change\n' >"$REPOSITORY/hack/e2e-kind.sh"
git -C "$REPOSITORY" commit --quiet -am harness
HARNESS_COMMIT=$(git -C "$REPOSITORY" rev-parse HEAD)
printf 'package controller\n// a runtime change\n' >"$REPOSITORY/internal/controller/reconcile.go"
git -C "$REPOSITORY" commit --quiet -am runtime
RUNTIME_COMMIT=$(git -C "$REPOSITORY" rev-parse HEAD)

PTAH_PIN=1111111111111111111111111111111111111111
OPERATOR_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
EXECUTOR_DIGEST=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CHART_DIGEST=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
GOOD_MANIFEST=$WORK_DIR/release-manifest.txt
cat >"$GOOD_MANIFEST" <<EOF
version=0.2.0
source-sha=$RELEASE_COMMIT
image=ghcr.io/stokaro/ptah-operator@$OPERATOR_DIGEST
executor=ghcr.io/stokaro/ptah-operator-executor@$EXECUTOR_DIGEST
executor-ptah-commit=$PTAH_PIN
executor-ptah-version=v0.9.0
chart-asset=ptah-operator-0.2.0.tgz
chart-asset-sha256=$CHART_DIGEST
EOF

# run_case runs one script against the extracted functions in a subshell, with
# this test's defaults set first so the script can override any of them, and
# prints what it reported.
run_case() {
	(
		fail() {
			printf 'e2e: %s\n' "$*" >&2
			exit 1
		}
		# shellcheck source=/dev/null
		. "$FUNCTIONS_FILE"
		SOURCE_REPOSITORY_ROOT=$REPOSITORY
		CONTROLLER_REVISION=$HARNESS_COMMIT
		E2E_RELEASE_MANIFEST=$GOOD_MANIFEST
		E2E_PTAH_REVISION=$PTAH_PIN
		E2E_PTAH_VERSION=
		E2E_EXECUTOR_IMAGE=
		E2E_RUNNER_IMAGE=
		E2E_STOP_AFTER=
		RELEASE_CHART_SHA256=
		eval "$1"
	) 2>&1
}

expect_admitted() {
	if ! admitted_output=$(run_case "$2"); then
		selftest_fail "$1 was refused: $admitted_output"
	fi
	printf '%s\n' "$admitted_output"
}

expect_refused() {
	if refused_output=$(run_case "$3"); then
		selftest_fail "$1 was admitted: $refused_output"
	fi
	printf '%s\n' "$refused_output" | grep -qF "$2" ||
		selftest_fail "$1 was refused for another reason: $refused_output"
}

manifest_variant() {
	variant_file=$WORK_DIR/manifest-$1.txt
	sed "$2" "$GOOD_MANIFEST" >"$variant_file"
	printf '%s' "$variant_file"
}

# The control: a harness commit that changed only hack/ installs the release,
# and the run takes the executor's Ptah version from the manifest.
admitted=$(expect_admitted 'a harness-only change' \
	'read_release_manifest; printf "version=%s source=%s chart=%s\n" "$E2E_PTAH_VERSION" "$RELEASE_SOURCE_SHA" "$RELEASE_CHART_SHA256"')
printf '%s\n' "$admitted" | grep -qF "version=v0.9.0 source=$RELEASE_COMMIT chart=$CHART_DIGEST" ||
	selftest_fail "the admitted manifest was read as: $admitted"
# The release source itself is admitted too: no change is no change.
expect_admitted 'the release source as harness' \
	'CONTROLLER_REVISION=$RELEASE_COMMIT; read_release_manifest' >/dev/null

expect_refused 'a runtime change' 'internal/controller/reconcile.go' \
	'CONTROLLER_REVISION=$RUNTIME_COMMIT; read_release_manifest'
expect_refused 'a runtime change' 'changes runtime sources of release' \
	'CONTROLLER_REVISION=$RUNTIME_COMMIT; read_release_manifest'

missing=$(manifest_variant missing '/^executor=/d')
expect_refused 'a missing executor' 'exactly one executor' \
	'E2E_RELEASE_MANIFEST=$missing; read_release_manifest'
duplicate=$(manifest_variant duplicate '/^image=/p')
expect_refused 'a repeated image' 'exactly one image' \
	'E2E_RELEASE_MANIFEST=$duplicate; read_release_manifest'
image_tag=$(manifest_variant image-tag 's/^image=.*/image=ghcr.io\/stokaro\/ptah-operator:v0.2.0/')
expect_refused 'an image named by tag' 'release image is not pinned by digest' \
	'E2E_RELEASE_MANIFEST=$image_tag; read_release_manifest'
executor_tag=$(manifest_variant executor-tag 's/^executor=.*/executor=ghcr.io\/stokaro\/ptah-operator-executor:v0.2.0/')
expect_refused 'an executor named by tag' 'release executor is not pinned by digest' \
	'E2E_RELEASE_MANIFEST=$executor_tag; read_release_manifest'
other_ptah=$(manifest_variant ptah 's/^executor-ptah-commit=.*/executor-ptah-commit=2222222222222222222222222222222222222222/')
expect_refused 'another Ptah commit' 'the catalog pins' \
	'E2E_RELEASE_MANIFEST=$other_ptah; read_release_manifest'
short_source=$(manifest_variant short 's/^source-sha=\(.\{12\}\).*/source-sha=\1/')
expect_refused 'an abbreviated source' 'not a full commit' \
	'E2E_RELEASE_MANIFEST=$short_source; read_release_manifest'
unknown_source=$(manifest_variant unknown "s/^source-sha=.*/source-sha=$(printf '%040d' 7)/")
expect_refused 'a source the repository lacks' 'fetch it first' \
	'E2E_RELEASE_MANIFEST=$unknown_source; read_release_manifest'
bad_chart=$(manifest_variant chart 's/^chart-asset-sha256=.*/chart-asset-sha256=nothex/')
expect_refused 'a malformed chart digest' 'not a SHA-256' \
	'E2E_RELEASE_MANIFEST=$bad_chart; read_release_manifest'
expect_refused 'a conflicting Ptah version' 'differs from the release executor' \
	'E2E_PTAH_VERSION=v0.8.0; read_release_manifest'
expect_refused 'a second executor' 'would name others' \
	'E2E_EXECUTOR_IMAGE=example.invalid/ptah@$EXECUTOR_DIGEST; read_release_manifest'
expect_refused 'an image export' 'exports this run' \
	'E2E_STOP_AFTER=images; read_release_manifest'

expect_admitted 'the release chart' \
	'RELEASE_CHART_SHA256=$CHART_DIGEST; verify_release_chart $CHART_DIGEST' >/dev/null
expect_refused 'another chart' 'release chart asset is' \
	'RELEASE_CHART_SHA256=$CHART_DIGEST; verify_release_chart dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'

# A copy tool that answers from the environment. It requires the arguments
# that keep the copy a byte-for-byte one and reports the digest it was told to.
FAKE_BIN=$WORK_DIR/bin
mkdir -p "$FAKE_BIN"
cat >"$FAKE_BIN/go" <<'FAKE'
#!/bin/sh
set -eu
arguments=" $* "
for required in ' run ./hack/imagecopy ' ' -to-plain-http ' ' -require-platforms linux/amd64,linux/arm64 ' \
	" -from $FAKE_EXPECTED_SOURCE " ' -to-password-file '; do
	case "$arguments" in
		*"$required"*) ;;
		*) printf 'fake go: missing %s in%s\n' "$required" "$arguments" >&2; exit 3 ;;
	esac
done
[ "$FAKE_COPY_FAILS" = 0 ] || exit 1
printf '{"source":"%s","digest":"%s","mediaType":"application/vnd.oci.image.index.v1+json"}\n' \
	"$FAKE_EXPECTED_SOURCE" "$FAKE_COPY_DIGEST"
FAKE
chmod +x "$FAKE_BIN/go"

COPY_SCRIPT='PATH=$FAKE_BIN:$PATH
REMOTE_REGISTRY=127.0.0.1:5000
REGISTRY_HOST=e2e-registry.test:5000
IMAGE_TAG=selftest
REGISTRY_USERNAME=user
REGISTRY_PASSWORD_FILE=$WORK_DIR/password
RELEASE_IMAGES_FILE=$WORK_DIR/release-images.jsonl
copy_release_image "ghcr.io/stokaro/ptah-operator@$OPERATOR_DIGEST" ptah-operator
printf "ref=%s\n" "$PUSHED_IMAGE_REF"'
FAKE_EXPECTED_SOURCE="ghcr.io/stokaro/ptah-operator@$OPERATOR_DIGEST"
export FAKE_EXPECTED_SOURCE FAKE_COPY_DIGEST FAKE_COPY_FAILS
: >"$WORK_DIR/release-images.jsonl"

FAKE_COPY_DIGEST=$OPERATOR_DIGEST
FAKE_COPY_FAILS=0
copied=$(expect_admitted 'an unchanged copy' "$COPY_SCRIPT")
printf '%s\n' "$copied" | grep -qF "ref=e2e-registry.test:5000/ptah-operator@$OPERATOR_DIGEST" ||
	selftest_fail "the copied image is referenced as: $copied"
grep -qF "\"digest\":\"$OPERATOR_DIGEST\"" "$WORK_DIR/release-images.jsonl" ||
	selftest_fail "the copy was not recorded"

FAKE_COPY_DIGEST=$EXECUTOR_DIGEST
expect_refused 'a registry holding other bytes' 'the task registry holds' "$COPY_SCRIPT"
FAKE_COPY_DIGEST=$OPERATOR_DIGEST
FAKE_COPY_FAILS=1
expect_refused 'a failed copy' 'could not copy release image' "$COPY_SCRIPT"

printf '%s\n' 'e2e release images self-test: PASS a release is installed only by its own digests, chart and runtime source'
