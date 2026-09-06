#!/bin/sh

set -eu

fail() {
	printf 'Kubernetes support lookup: %s\n' "$*" >&2
	exit 1
}

[ "$#" -eq 2 ] || fail "usage: $0 SUPPORT_MANIFEST KUBERNETES_VERSION"
support_manifest=$1
kubernetes_version=$2

[ -f "$support_manifest" ] || fail "support manifest is not a regular file: $support_manifest"
printf '%s\n' "$kubernetes_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
	fail "Kubernetes version must be an exact major.minor.patch version"
kubernetes_minor=${kubernetes_version%.*}

resolved_image=$(jq -er \
	--arg minor "$kubernetes_minor" \
	--arg version "$kubernetes_version" '
if .schemaVersion != 1 or
   (.windowSize | type) != "number" or
   (.releases | type) != "array" or
   .windowSize != (.releases | length)
then error("invalid Kubernetes support manifest structure")
else
  [
    .releases[]
    | select((.minor | type) == "string" and .minor == $minor)
    | select((.nodeImage | type) == "string")
    | .nodeImage
    | select(startswith("kindest/node:v" + $version + "@sha256:"))
    | select(test("@sha256:[0-9a-f]{64}$"))
  ]
  | if length == 1
    then .[0]
    else error("Kubernetes version is not an exact unique support-manifest member")
    end
end
' "$support_manifest") ||
	fail "Kubernetes $kubernetes_version is not an exact unique release in $support_manifest"

printf '%s\n' "$resolved_image"
