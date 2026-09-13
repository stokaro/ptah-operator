#!/bin/sh
# The lab's answers about versions, read from the repository's own declarations.
#
# There is no second list here. Which Kubernetes minors are supported, which
# kind release pins their node images, and which Ptah build the operator is
# verified against are all declared once -- in support/kubernetes.json and
# support/ptah.json -- and this file reads them. A demo that pinned its own
# versions would be a support claim nobody validates.

# support_kind_version prints the kind release the support manifest pins.
support_kind_version() {
	jq -er '.kindVersion' "$LAB_ROOT/support/kubernetes.json"
}

# support_newest_minor prints the newest supported Kubernetes minor, which is
# what the lab runs unless the caller names another.
support_newest_minor() {
	jq -er '[.releases[].minor] | last' "$LAB_ROOT/support/kubernetes.json"
}

# support_newest_version prints the exact Kubernetes release the lab runs.
#
# Read out of the newest minor's node image, which is where the patch is
# declared: the manifest names a minor and a digest-pinned image, and the image
# tag carries the release. The harness demands an exact x.y.z and refuses a
# release outside the window, so a version written down anywhere else stops
# working the day the window moves.
support_newest_version() {
	support_node_image "$(support_newest_minor)" |
		sed -n 's|^kindest/node:v\([0-9][0-9.]*\)@sha256:[0-9a-f]\{64\}$|\1|p' |
		grep -E '^[0-9]+\.[0-9]+\.[0-9]+$'
}

# support_node_image prints the digest-pinned node image for one minor, and
# fails when that minor is not in the window.
support_node_image() {
	minor=$1
	jq -er --arg minor "$minor" \
		'.releases[] | select(.minor == $minor) | .nodeImage' \
		"$LAB_ROOT/support/kubernetes.json"
}

# support_minors prints every supported minor, one per line.
support_minors() {
	jq -er '.releases[].minor' "$LAB_ROOT/support/kubernetes.json"
}

# support_ptah_commit prints the Ptah commit the operator's development state is
# verified against. The executor the lab installs is built from it, so the demo
# shows the pairing the compatibility matrix publishes rather than a pairing
# nobody measured.
support_ptah_commit() {
	jq -er '[.releases[] | select(.operator == "edge") | .verified[].ptahCommit] | first' \
		"$LAB_ROOT/support/ptah.json"
}

# support_ptah_describe prints the readable identity of that commit.
support_ptah_describe() {
	jq -er '[.releases[] | select(.operator == "edge") | .verified[].ptahDescribe] | first' \
		"$LAB_ROOT/support/ptah.json"
}
