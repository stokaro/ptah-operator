#!/bin/sh

set -eu

ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-hook-evidence.XXXXXX")

# A refused parameter expansion (${VAR:?...}) or an unset name under set -u
# ends the shell without setting $?, so an EXIT trap that reports $? reads the
# previous command's success and a script that never finished reports a pass.
# The latch is set where the script reaches its own end; the trap trusts it.
PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
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
		--arg expected_name ptah-crd-reconcile \
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
      "name": "ptah-crd-reconcile",
      "kind": "ServiceAccount",
      "weight": -110,
      "events": ["pre-install", "pre-upgrade", "pre-rollback"],
      "last_run": {
        "phase": "Succeeded",
        "started_at": "2026-01-01T00:00:00Z",
        "completed_at": "2026-01-01T00:00:01Z"
      }
    },
    {
      "name": "ptah-crd-reconcile",
      "kind": "Job",
      "weight": null,
      "events": ["pre-install", "pre-upgrade", "pre-rollback"],
      "last_run": {
        "phase": "Failed",
        "started_at": "2026-01-01T00:00:02Z",
        "completed_at": "2026-01-01T00:00:03Z"
      }
    },
    {
      "name": "later-hook",
      "kind": "Job",
      "weight": 5,
      "events": ["pre-upgrade"],
      "last_run": {"phase": ""}
    }
  ]
}
EOF

evaluate "$WORK_DIR/valid.json"
expect_rejected wrong-revision '.version = 8'
expect_rejected not-failed '.info.status = "deployed"'
expect_rejected wrong-name '.hooks[1].name = "other-reconcile"'
expect_rejected wrong-kind '.hooks[1].kind = "Pod"'
expect_rejected wrong-weight '.hooks[1].weight = -60'
expect_rejected wrong-event '.hooks[1].events = ["post-upgrade"]'
expect_rejected never-started '.hooks[1].last_run.started_at = ""'
expect_rejected two-failures '.hooks[0].last_run.phase = "Failed"'
expect_rejected no-failure '.hooks[1].last_run.phase = "Succeeded"'
expect_rejected later-hook-ran '.hooks[2].last_run = .hooks[0].last_run'
expect_rejected malformed-later-weight '.hooks[2].weight = "not-a-weight"'

PHASE_COMPLETED=1
printf '%s\n' 'failed hook evidence self-test: PASS'
