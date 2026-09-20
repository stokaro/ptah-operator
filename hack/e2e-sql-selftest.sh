#!/bin/sh

set -eu

# Prove the statement guard before trusting what it protects.
#
# The guarded statements in the migrations and reference-data phases are the
# phase's own setup: an external edit, a column undone by hand, a revision row
# taken out of the history. The exit status of a pipeline belongs to its last
# stage, so a guard on a helper that ends in `tr` reads tr and never the exec:
# the statement is skipped in silence and the phase dies ten minutes later
# accusing the operator of not reaching a phase it was never given. Capturing
# the value does not help either -- command substitution takes the function's
# status, and the function still ends in the pipe.
#
# So this runs hack/e2e-sql.sh with a kubectl of its own and refuses it unless
#
#   - a statement whose exec failed returns what the exec returned, so the
#     `|| fail` the call sites write fires, and
#   - a value still arrives trimmed, with the database and the statement it
#     was asked for.
#
# Usage: hack/e2e-sql-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

fail() {
	printf 'e2e sql self-test: %s\n' "$*" >&2
	exit 1
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-sql-selftest.XXXXXX")
trap 'rm -rf -- "$WORK_DIR"' EXIT

# A statement whose exec failed must fail, read through the guard the call
# sites write rather than through $? on its own.
status=0
(
	k() {
		printf 'error: unable to upgrade connection\n' >&2
		return 7
	}
	# shellcheck source=hack/e2e-sql.sh
	. "$ROOT_DIR/hack/e2e-sql.sh"
	sql_statement postgresql e2e e2e-postgresql widgets \
		"ALTER TABLE e2e_migration_widgets DROP COLUMN weight" >/dev/null ||
		exit 9
) 2>/dev/null || status=$?
[ "$status" -eq 9 ] ||
	fail "the guard on a statement whose exec failed did not fire: the phase saw status $status"

# The value helper keeps the trim. The two clients pad and terminate a value
# differently, so every comparison in both phases is written against a value
# with no whitespace in it.
observed=$(
	k() {
		for sql_arg in "$@"; do
			printf '%s\n' "$sql_arg"
		done >"$WORK_DIR/argv.txt"
		printf ' 42 \n'
	}
	# shellcheck source=hack/e2e-sql.sh
	. "$ROOT_DIR/hack/e2e-sql.sh"
	sql_value mysql e2e e2e-mysql countries "SELECT count(*) FROM countries"
)
[ "$observed" = 42 ] ||
	fail "the value helper returned \"$observed\", and the phases compare against 42"

# The database and the statement are the two arguments a phase chooses per
# call. A helper that reached the client with either of them missing would run
# the right SQL against the wrong database.
observed_database=$(tail -n 2 "$WORK_DIR/argv.txt" | head -n 1)
observed_statement=$(tail -n 1 "$WORK_DIR/argv.txt")
[ "$observed_database" = countries ] ||
	fail "the client was asked for database \"$observed_database\", want countries"
[ "$observed_statement" = "SELECT count(*) FROM countries" ] ||
	fail "the client was asked to run \"$observed_statement\""

printf '%s\n' 'e2e sql self-test: PASS a failed statement fails the guard, and a value still arrives trimmed'
