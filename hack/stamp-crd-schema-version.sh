#!/bin/sh

set -eu

schema_version=${1:?CRD schema version is required}
shift
controller_state_version=${1:?controller-state version is required}
shift
GO_BINARY=${GO:-go}
ROOT_DIR=$(cd -- "$(dirname -- "$0")/.." && pwd)
CALLER_DIR=$(pwd -P)

case "$schema_version" in
	'' | 0 | 0* | *[!0-9]*)
		printf 'stamp CRD schema version: %s is not a positive exact decimal version\n' "$schema_version" >&2
		exit 1
		;;
esac
case "$controller_state_version" in
	'' | 0 | 0* | *[!0-9]*)
		printf 'stamp controller-state version: %s is not a positive exact decimal version\n' "$controller_state_version" >&2
		exit 1
		;;
esac
[ "$#" -gt 0 ] || {
	printf '%s\n' 'stamp CRD schema version: at least one CRD file is required' >&2
	exit 1
}

STAMP_TEMP=$(mktemp "${TMPDIR:-/tmp}/ptah-crd-schema-version.XXXXXX")
cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$STAMP_TEMP" in
		"${TMPDIR:-/tmp}"/ptah-crd-schema-version.*) rm -f -- "$STAMP_TEMP" ;;
		*) status=1 ;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

for crd_file in "$@"; do
	[ -f "$crd_file" ] || {
		printf 'stamp CRD schema version: CRD file does not exist: %s\n' "$crd_file" >&2
		exit 1
	}
	case "$crd_file" in
	/*) digest_file=$crd_file ;;
	*) digest_file=$CALLER_DIR/$crd_file ;;
	esac
	digest=$(cd "$ROOT_DIR" && "$GO_BINARY" run ./hack/crdschemadigest "$digest_file")
	case "$digest" in
	sha256:????????????????????????????????????????????????????????????????) ;;
	*)
		printf 'stamp CRD schema version: computed invalid schema digest for %s: %s\n' "$crd_file" "$digest" >&2
		exit 1
		;;
	esac
	if ! awk -v schema_version="$schema_version" -v controller_state_version="$controller_state_version" -v digest="$digest" '
      /^    operator[.]ptah[.]dev\/crd-schema-version:/ {next}
	  /^    operator[.]ptah[.]dev\/crd-schema-digest:/ {next}
	  /^    operator[.]ptah[.]dev\/controller-state-version:/ {next}
      /^  annotations:$/ && !stamped {
        print
		print "    operator.ptah.dev/controller-state-version: \"" controller_state_version "\""
		print "    operator.ptah.dev/crd-schema-version: \"" schema_version "\""
		print "    operator.ptah.dev/crd-schema-digest: \"" digest "\""
        stamped = 1
        next
      }
      {print}
      END {if (!stamped) exit 42}
    ' "$crd_file" >"$STAMP_TEMP"; then
		printf 'stamp CRD schema version: metadata annotations block is missing: %s\n' "$crd_file" >&2
		exit 1
	fi
	chmod 0644 "$STAMP_TEMP"
	mv -- "$STAMP_TEMP" "$crd_file"
	STAMP_TEMP=$(mktemp "${TMPDIR:-/tmp}/ptah-crd-schema-version.XXXXXX")
done
