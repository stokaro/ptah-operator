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
	jq -e --argjson stoppedAt 3 --arg step fetch-migrations \
		--arg digest sha256:ff --argjson databaseAt 3 --argjson artifactCovers 2 \
		--arg gate operator.ptah.run/e2e-apply-gate \
		-f "$ROOT_DIR/testdata/e2e/$filter" \
		"$WORK_DIR/document.json" >/dev/null ||
		fail "$filter refused a reading it has to accept: $description"
}

refuses() {
	filter=$1
	description=$2
	cat >"$WORK_DIR/document.json"
	if jq -e --argjson stoppedAt 3 --arg step fetch-migrations \
		--arg digest sha256:ff --argjson databaseAt 3 --argjson artifactCovers 2 \
		--arg gate operator.ptah.run/e2e-apply-gate \
		-f "$ROOT_DIR/testdata/e2e/$filter" \
		"$WORK_DIR/document.json" >/dev/null 2>&1; then
		fail "$filter accepted a reading it has to refuse: $description"
	fi
}

# Before any of that: every filter the suite carries has to compile. A jq
# program with a syntax error is a proof that fails after a Kubernetes cluster
# has been built and an hour of lifecycle has run, and it fails with a message
# about jq rather than about the operator.
#
# jq separates the two cases by exit status -- 3 for a program it cannot
# compile, 5 for one that compiled and then met a value it could not handle --
# so the check refuses only the first. Running each filter against null without
# its inputs reaches the compiler and rarely reaches anything else, and where it
# does the runtime complaint is not this check's business.
for filter_file in "$ROOT_DIR"/testdata/e2e/*.jq; do
	# jq resolves a $name when it compiles, so a filter that takes inputs does
	# not compile without them and would be reported as broken. The names it
	# uses are read out of the file and handed back as null: what is being
	# asked is whether the program parses, not what it decides.
	# shellcheck disable=SC2016 # The dollar is jq's, matched literally.
	grep -oE '\$[a-zA-Z_][a-zA-Z0-9_]*' "$filter_file" |
		grep -vx '\$__loc__' | sort -u >"$WORK_DIR/inputs.txt" || true
	set --
	# Redirected rather than piped: a pipeline would build the arguments in a
	# subshell and leave this one with none.
	while read -r filter_input; do
		[ -n "$filter_input" ] || continue
		set -- "$@" --argjson "${filter_input#$}" null
	done <"$WORK_DIR/inputs.txt"
	jq -n "$@" -f "$filter_file" >/dev/null 2>&1 || [ "$?" -ne 3 ] ||
		fail "$(basename "$filter_file") is not a jq program"
done

# The readings a proof must accept come from the operator wherever one can be
# had. This one is the status a lifecycle printed when it failed -- run
# 34990006879 on 23a9c4d, PostgreSQL, the partial row -- with the digests, times
# and names of that one cluster scrubbed out. A document written by hand proves
# the filter agrees with its author; this one proves it agrees with the operator.
#
# It is also the document that broke two proofs at once: the phase is Resolving
# while the refusal holds, and the pending count is one rather than zero.
accepts_file() {
	filter=$1
	description=$2
	reading=$3
	jq -e --argjson stoppedAt 3 --arg step fetch-migrations \
		--arg digest sha256:ff --argjson databaseAt 3 --argjson artifactCovers 2 \
		--arg gate operator.ptah.run/e2e-apply-gate \
		-f "$ROOT_DIR/testdata/e2e/$filter" \
		"$ROOT_DIR/testdata/e2e/readings/$reading" >/dev/null ||
		fail "$filter refused a reading the operator produced: $description"
}

accepts_file migration-partial-refusal.jq 'the reading that broke the hold' \
	partial-run-left-a-dirty-revision.json
accepts_file migration-dirty-reading.jq 'the reading that broke the dirty wait' \
	partial-run-left-a-dirty-revision.json

# The refusal a stopped migration holds.
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
accepts migration-dirty-reading.jq 'dirty at 3, mid-cycle, Ptah counting it pending' <<'JSON'
{"status":{"phase":"Resolving","history":{"dirty":true,"currentVersion":3,"pendingCount":1},
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

# The step that refused an artifact, as distinct from any other failure. The
# rejected cases are the two this had to be told apart from: the message the
# controller produced before it named a boundary at all, and a different step
# failing for its own reasons.
accepts migration-refused-boundary.jq 'the fetch step ended the run' <<'JSON'
{"status":{"conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"the fetch-migrations step exited 2, so the run never started"}]}}
JSON
refuses migration-refused-boundary.jq 'a missing frame, naming no boundary' <<'JSON'
{"status":{"conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"read History result: ptah runner result frame not found"}]}}
JSON
refuses migration-refused-boundary.jq 'a different step failed' <<'JSON'
{"status":{"conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"the install-runner step exited 1, so the run never started"}]}}
JSON
refuses migration-refused-boundary.jq 'the boundary named on another condition' <<'JSON'
{"status":{"conditions":[{"type":"Ready","status":"False","reason":"OperationFailed",
 "message":"the fetch-migrations step exited 2, so the run never started"}]}}
JSON

# What a partial run recorded. The refused cases are the readings that would
# make the row pass while proving something else: a run that finished, a run
# that claimed versions, and no run at all.
accepts_file migration-partial-run-recorded.jq 'the reading the failing run printed' \
	partial-run-left-a-dirty-revision.json
refuses migration-partial-run-recorded.jq 'a run that finished' <<'JSON'
{"status":{"lastRun":{"outcome":"Applied","appliedVersions":[4]}}}
JSON
refuses migration-partial-run-recorded.jq 'a partial that claimed a version' <<'JSON'
{"status":{"lastRun":{"outcome":"Partial","appliedVersions":[4]}}}
JSON
refuses migration-partial-run-recorded.jq 'no run at all' <<'JSON'
{"status":{"phase":"Blocked"}}
JSON

# A database that ran more than the artifact carries. The refused cases are the
# readings this had to be told apart from: an ordinary settled database, where
# the two numbers agree, and one where work is still pending, which is the
# reading the operator used to mistake this for.
accepts migration-history-ahead.jq 'database at 3, artifact covers 2' <<'JSON'
{"status":{"artifact":{"digest":"sha256:ff"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":0},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryAhead"},
               {"type":"Ready","status":"False","reason":"HistoryAhead"}]}}
JSON
refuses migration-history-ahead.jq 'an ordinary settled database' <<'JSON'
{"status":{"artifact":{"digest":"sha256:ff"},
 "history":{"currentVersion":3,"appliedCount":3,"pendingCount":0},
 "conditions":[{"type":"Ready","status":"True","reason":"HistoryMatched"}]}}
JSON
refuses migration-history-ahead.jq 'work still pending' <<'JSON'
{"status":{"artifact":{"digest":"sha256:ff"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":1},
 "conditions":[{"type":"Ready","status":"False","reason":"MigrationsPending"}]}}
JSON
refuses migration-history-ahead.jq 'a plan published anyway' <<'JSON'
{"status":{"artifact":{"digest":"sha256:ff"},"plan":{"name":"ptah-mplan-0","uid":"u"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":0},
 "conditions":[{"type":"Ready","status":"False","reason":"HistoryAhead"}]}}
JSON
refuses migration-history-ahead.jq 'the tag the operator read is another one' <<'JSON'
{"status":{"artifact":{"digest":"sha256:ee"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":0},
 "conditions":[{"type":"Ready","status":"False","reason":"HistoryAhead"}]}}
JSON

# A resource that acted on nothing. Each refused case is one clause failing on
# its own, because a negative claim is satisfied by accident more easily than a
# positive one.
accepts migration-untouched-database.jq 'refused before anything was planned' <<'JSON'
{"status":{"phase":"Reading","history":{"appliedCount":0},
 "conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"the fetch-migrations step exited 2, so the run never started"}]}}
JSON
refuses migration-untouched-database.jq 'a plan was published' <<'JSON'
{"status":{"phase":"Reading","plan":{"name":"ptah-mplan-0","uid":"u"},"history":{"appliedCount":0}}}
JSON
refuses migration-untouched-database.jq 'a run was recorded' <<'JSON'
{"status":{"phase":"Reading","lastRun":{"outcome":"Failed"},"history":{"appliedCount":0}}}
JSON
refuses migration-untouched-database.jq 'it reached the approval gate' <<'JSON'
{"status":{"phase":"AwaitingApproval","history":{"appliedCount":0}}}
JSON
refuses migration-untouched-database.jq 'something was applied' <<'JSON'
{"status":{"phase":"Reading","history":{"appliedCount":1}}}
JSON

# The Apply Pod a closed scheduling gate is holding. Both refusals below are
# mistakes this filter actually made: it read the phase alone, and it was
# written with any(), which is false for an empty list and so accepted a Job
# whose Pod did not exist yet. Either one lets the proof wait out a window
# nothing was holding, and pass with the node selector no longer reaching the
# Pod.
accepts gated-apply-pod.jq 'one Pod the gate is holding' <<'JSON'
{"items":[{"metadata":{"name":"apply-abc"},
 "spec":{"nodeSelector":{"operator.ptah.run/e2e-apply-gate":"open"}},
 "status":{"phase":"Pending"}}]}
JSON
refuses gated-apply-pod.jq 'a Pod carrying no selector, between creation and scheduling' <<'JSON'
{"items":[{"metadata":{"name":"apply-abc"},"spec":{},"status":{"phase":"Pending"}}]}
JSON
refuses gated-apply-pod.jq 'a selector for some other gate' <<'JSON'
{"items":[{"metadata":{"name":"apply-abc"},
 "spec":{"nodeSelector":{"kubernetes.io/os":"linux"}},
 "status":{"phase":"Pending"}}]}
JSON
refuses gated-apply-pod.jq 'the Job has not created a Pod yet' <<'JSON'
{"items":[]}
JSON
refuses gated-apply-pod.jq 'Pending, and already bound to a node' <<'JSON'
{"items":[{"metadata":{"name":"apply-abc"},
 "spec":{"nodeName":"kind-worker","nodeSelector":{"operator.ptah.run/e2e-apply-gate":"open"}},
 "status":{"phase":"Pending"}}]}
JSON
refuses gated-apply-pod.jq 'the runner is already going' <<'JSON'
{"items":[{"metadata":{"name":"apply-abc"},
 "spec":{"nodeName":"kind-worker","nodeSelector":{"operator.ptah.run/e2e-apply-gate":"open"}},
 "status":{"phase":"Running"}}]}
JSON
refuses gated-apply-pod.jq 'one held and one placed' <<'JSON'
{"items":[{"metadata":{"name":"apply-abc"},
 "spec":{"nodeSelector":{"operator.ptah.run/e2e-apply-gate":"open"}},
 "status":{"phase":"Pending"}},
 {"metadata":{"name":"apply-def"},
 "spec":{"nodeName":"kind-worker","nodeSelector":{"operator.ptah.run/e2e-apply-gate":"open"}},
 "status":{"phase":"Pending"}}]}
JSON

printf 'migration refusal filter self-test: PASS\n'
PHASE_COMPLETED=1
