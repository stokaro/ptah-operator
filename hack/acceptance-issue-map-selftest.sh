#!/bin/sh
# The report is only useful if it reads the map it claims to read. These run it
# offline against a fixture body and a stubbed state lookup.

set -eu

unset CDPATH

ROOT_DIR=$(cd -- "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

fail() {
	printf 'acceptance issue map self-test: %s\n' "$1" >&2
	exit 1
}

# Two rows in the shape the map uses, and one line that is not a row.
cat >"$WORK_DIR/body.md" <<'BODY'
## Review issue map

| Requirement | Area | Related issues |
| --- | --- | --- |
| PA-01 | Candidate and coverage | [#203](https://example.invalid/203), [#209](https://example.invalid/209) |
| PA-11 | Installable artifacts | [#237](https://example.invalid/237), [#238](https://example.invalid/238), [#239](https://example.invalid/239) |
BODY

# 209 is open, 238 answers nothing, the rest are closed.
cat >"$WORK_DIR/state.sh" <<'STATE'
#!/bin/sh
case "$1" in
209) printf 'OPEN\n' ;;
238) printf 'MISSING\n' ;;
*) printf 'CLOSED\n' ;;
esac
STATE
chmod +x "$WORK_DIR/state.sh"

report=$(
	ACCEPTANCE_MAP_BODY_CMD="cat $WORK_DIR/body.md" \
		ACCEPTANCE_MAP_STATE_CMD="$WORK_DIR/state.sh" \
		"$ROOT_DIR/hack/acceptance-issue-map.sh"
)

printf '%s\n' "$report" | grep -qE '^\| PA-01 \| Candidate and coverage \| 2 \| 1 \| #209 \|$' ||
	fail "PA-01 did not report two linked issues with #209 open: $report"

# A state the forge did not answer counts as not closed, and says which it was.
# Treating it as closed is how a report starts awarding passes it never read.
printf '%s\n' "$report" | grep -qE '^\| PA-11 \| Installable artifacts \| 3 \| 1 \| #238\(MISSING\) \|$' ||
	fail "an unanswered state was not carried through: $report"

printf '%s\n' "$report" | grep -qF '5 distinct issues are linked; 2 of them are not closed.' ||
	fail "the totals do not count distinct issues: $report"

# A body the map moved out of is a refusal, not an empty table: an empty table
# reads as "nothing is open".
if ACCEPTANCE_MAP_BODY_CMD="printf '## Something else\n'" \
	ACCEPTANCE_MAP_STATE_CMD="$WORK_DIR/state.sh" \
	"$ROOT_DIR/hack/acceptance-issue-map.sh" >/dev/null 2>&1; then
	fail "a body with no requirement rows was reported as an empty map"
fi

if ACCEPTANCE_MAP_BODY_CMD="true" \
	ACCEPTANCE_MAP_STATE_CMD="$WORK_DIR/state.sh" \
	"$ROOT_DIR/hack/acceptance-issue-map.sh" >/dev/null 2>&1; then
	fail "an empty body was reported as an empty map"
fi

printf 'acceptance issue map self-test: PASS every linked issue is read, an unanswered state counts as open, and a moved map refuses\n'
