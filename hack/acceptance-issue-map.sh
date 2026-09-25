#!/bin/sh
# Report the state of every issue the #242 review map links, per requirement.
#
# The map deliberately does not record issue states: a link identifies related
# work, not a passing result, and a copied state goes stale the moment someone
# closes something. So the states are read when the report runs and never
# stored.
#
# This exists because #242's second completion criterion -- complete and verify
# the applicable fixes linked in the review issue map -- is otherwise 47 issues
# checked by hand, which is the kind of work a person does once and then
# believes.
#
# The report awards nothing. A closed issue is not evidence about a candidate;
# it only means the review finding behind it is no longer open.

set -eu

unset CDPATH

: "${ACCEPTANCE_MAP_ISSUE:=242}"
: "${ACCEPTANCE_MAP_REPO:=stokaro/ptah-operator}"

# Where the map comes from, and how one issue's state is read. Both are
# overridable so the self-test runs offline.
: "${ACCEPTANCE_MAP_BODY_CMD:=gh issue view $ACCEPTANCE_MAP_ISSUE --repo $ACCEPTANCE_MAP_REPO --json body --jq .body}"
: "${ACCEPTANCE_MAP_STATE_CMD:=}"

read_state() {
	read_state_number=$1
	if [ -n "$ACCEPTANCE_MAP_STATE_CMD" ]; then
		$ACCEPTANCE_MAP_STATE_CMD "$read_state_number"
		return
	fi
	gh issue view "$read_state_number" --repo "$ACCEPTANCE_MAP_REPO" --json state --jq .state 2>/dev/null ||
		printf 'UNKNOWN\n'
}

body=$($ACCEPTANCE_MAP_BODY_CMD)
if [ -z "$body" ]; then
	printf 'acceptance issue map: the issue body is empty, so no map was read\n' >&2
	exit 1
fi

# The map's rows are "| PA-NN | Area | [#N](...), [#M](...) |". Anything else in
# the body is not a row, and a body with no rows is a refusal rather than an
# empty report: it means the map moved and this script is now reading nothing.
rows=$(printf '%s\n' "$body" | grep -E '^\| PA-[0-9]{2} \|' || true)
if [ -z "$rows" ]; then
	printf 'acceptance issue map: the body carries no requirement rows; the map shape changed\n' >&2
	exit 1
fi

printf '# Review issue map, with the state read now\n\n'
printf 'Read from %s issue %s. A closed issue is not evidence about a candidate.\n\n' \
	"$ACCEPTANCE_MAP_REPO" "$ACCEPTANCE_MAP_ISSUE"
printf '| Requirement | Area | Linked | Open | Still open |\n'
printf '| --- | --- | --- | --- | --- |\n'

all_numbers=$(mktemp)
open_numbers=$(mktemp)
trap 'rm -f "$all_numbers" "$open_numbers"' EXIT
: >"$all_numbers"
: >"$open_numbers"

printf '%s\n' "$rows" | while IFS= read -r row; do
	requirement=$(printf '%s' "$row" | awk -F'|' '{gsub(/ /,"",$2); print $2}')
	area=$(printf '%s' "$row" | awk -F'|' '{sub(/^ /,"",$3); sub(/ $/,"",$3); print $3}')
	numbers=$(printf '%s' "$row" | grep -oE '\[#[0-9]+\]' | tr -d '[]#' | sort -n -u)
	linked=0
	open_count=0
	open_list=""
	for number in $numbers; do
		linked=$((linked + 1))
		printf '%s\n' "$number" >>"$all_numbers"
		state=$(read_state "$number")
		case "$state" in
		OPEN)
			open_count=$((open_count + 1))
			open_list="$open_list #$number"
			printf '%s\n' "$number" >>"$open_numbers"
			;;
		CLOSED) ;;
		*)
			open_count=$((open_count + 1))
			open_list="$open_list #$number($state)"
			printf '%s\n' "$number" >>"$open_numbers"
			;;
		esac
	done
	if [ -z "$open_list" ]; then
		open_list="—"
	else
		open_list=$(printf '%s' "$open_list" | sed 's/^ //')
	fi
	printf '| %s | %s | %d | %d | %s |\n' "$requirement" "$area" "$linked" "$open_count" "$open_list"
done

distinct_all=$(sort -n -u "$all_numbers" | wc -l | tr -d ' ')
distinct_open=$(sort -n -u "$open_numbers" | wc -l | tr -d ' ')
printf '\n%s distinct issues are linked; %s of them are not closed.\n' "$distinct_all" "$distinct_open"
