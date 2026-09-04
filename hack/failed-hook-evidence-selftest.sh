#!/bin/sh

set -eu

ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-hook-evidence.XXXXXX")

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-hook-evidence.*) rm -rf -- "$WORK_DIR" ;;
	*) status=1 ;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

evaluate() {
	jq -e \
		--argjson expected_revision 7 \
		--arg expected_name ptah-crd-preflight \
		--argjson expected_weight -60 \
		--arg expected_identity_name ptah-hook-identity \
		--argjson expected_identity_weight -105 \
		-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$1" >/dev/null
}

expect_rejected() {
	name=$1
	filter=$2
	fixture=$WORK_DIR/$name.json
	jq "$filter" "$WORK_DIR/valid.json" >"$fixture"
	if evaluate "$fixture"; then
		printf 'failed hook evidence self-test: accepted %s\n' "$name" >&2
		exit 1
	fi
}

cat >"$WORK_DIR/valid.json" <<'EOF'
{
  "version": 7,
  "info": {"status": "failed"},
  "hooks": [
    {
	      "name": "ptah-hook-identity",
	      "kind": "Job",
	      "weight": -105,
      "events": ["pre-upgrade"],
      "last_run": {
        "phase": "Succeeded",
        "started_at": "2026-01-01T00:00:00Z",
        "completed_at": "2026-01-01T00:00:01Z"
      }
    },
    {
      "name": "ptah-crd-preflight",
      "kind": "Job",
      "weight": -60,
      "events": ["pre-upgrade"],
      "last_run": {
        "phase": "Failed",
        "started_at": "2026-01-01T00:00:02Z",
        "completed_at": "2026-01-01T00:00:03Z"
      }
    },
    {
      "name": "later-hook",
      "kind": "Job",
      "weight": null,
      "events": ["pre-upgrade"],
      "last_run": {"phase": ""}
    }
  ]
}
EOF

evaluate "$WORK_DIR/valid.json"
expect_rejected wrong-revision '.version = 8'
expect_rejected wrong-name '.hooks[1].name = "other-preflight"'
expect_rejected wrong-weight '.hooks[1].weight = -59'
expect_rejected wrong-event '.hooks[1].events = ["post-upgrade"]'
expect_rejected missing-identity '.hooks[0].name = "other-identity"'
expect_rejected failed-identity '.hooks[0].last_run.phase = "Failed"'
expect_rejected wrong-identity-weight '.hooks[0].weight = -104'
expect_rejected two-failures '.hooks[2].last_run = .hooks[1].last_run'
expect_rejected later-hook-ran '.hooks[2].last_run = .hooks[0].last_run'
expect_rejected malformed-later-weight '.hooks[2].weight = "not-a-weight"'

printf '%s\n' 'failed hook evidence self-test: PASS'
