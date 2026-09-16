#!/bin/sh

set -eu

# Every gate proves itself before it judges anything, and a filter is a gate.
#
# Two proofs of the migration phase failed on a cluster today for the same
# reason: the filter passed its author's intent and measured something else.
# Reading them again would not have caught either, because both read correctly.
# What catches them is a document they must refuse.
#
# So each filter here is run against one reading it must accept and several it
# must not, and the rejected ones are the mistakes that were actually made: a
# phase demanded of a resource that cycles, and a count that belongs to Ptah's
# bookkeeping rather than to the claim.

ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-refusal-filter.XXXXXX")

# A refused parameter expansion or an unset name under set -u ends the shell
# without setting $?, so the trap reads the previous command's success and a
# script that never finished reports a pass. The latch is set at the end.
PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-refusal-filter.*) rm -rf -- "$WORK_DIR" ;;
	*) status=1 ;;
	esac
	exit "$status"
}
trap cleanup EXIT HUP INT TERM

fail() {
	printf 'migration refusal filter self-test: %s\n' "$*" >&2
	exit 1
}

# accepts and refuses take a filter, a description, and a document on stdin.
accepts() {
	filter=$1
	description=$2
	cat >"$WORK_DIR/document.json"
	jq -e --argjson stoppedAt 3 -f "$ROOT_DIR/testdata/e2e/$filter" \
		"$WORK_DIR/document.json" >/dev/null ||
		fail "$filter refused a reading it has to accept: $description"
}

refuses() {
	filter=$1
	description=$2
	cat >"$WORK_DIR/document.json"
	if jq -e --argjson stoppedAt 3 -f "$ROOT_DIR/testdata/e2e/$filter" \
		"$WORK_DIR/document.json" >/dev/null 2>&1; then
		fail "$filter accepted a reading it has to refuse: $description"
	fi
}

# The refusal a stopped migration holds.
accepts migration-partial-refusal.jq 'blocked, mid-cycle in Resolving' <<'JSON'
{"status":{"phase":"Resolving","activeOperation":{"type":"Resolve"},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"},
               {"type":"Ready","status":"False","reason":"HistoryDirty"}]}}
JSON
accepts migration-partial-refusal.jq 'blocked, between cycles' <<'JSON'
{"status":{"phase":"Blocked",
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"},
               {"type":"Ready","status":"False","reason":"ApplyOutcomeUnknown"}]}}
JSON
refuses migration-partial-refusal.jq 'the refusal lapsed' <<'JSON'
{"status":{"phase":"Blocked",
 "conditions":[{"type":"Blocked","status":"False","reason":"HistoryMatched"},
               {"type":"Ready","status":"False","reason":"MigrationsPending"}]}}
JSON
refuses migration-partial-refusal.jq 'a plan was published while blocked' <<'JSON'
{"status":{"phase":"Blocked","plan":{"name":"ptah-mplan-0","uid":"u"},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"},
               {"type":"Ready","status":"False","reason":"HistoryDirty"}]}}
JSON
refuses migration-partial-refusal.jq 'the resource was called ready' <<'JSON'
{"status":{"phase":"Blocked",
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"},
               {"type":"Ready","status":"True","reason":"HistoryMatched"}]}}
JSON

# The reading a partially applied migration leaves behind.
accepts migration-dirty-reading.jq 'dirty at 3, Ptah counting it pending' <<'JSON'
{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":3,"pendingCount":1},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}
JSON
accepts migration-dirty-reading.jq 'dirty at 3, counted pending by nobody' <<'JSON'
{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":3,"pendingCount":0},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}
JSON
refuses migration-dirty-reading.jq 'no dirty revision' <<'JSON'
{"status":{"phase":"Blocked","history":{"dirty":false,"currentVersion":3},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}
JSON
refuses migration-dirty-reading.jq 'stopped at another version' <<'JSON'
{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":4},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}
JSON
refuses migration-dirty-reading.jq 'blocked for another reason' <<'JSON'
{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":3},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryModified"}]}}
JSON

printf 'migration refusal filter self-test: PASS\n'
PHASE_COMPLETED=1
