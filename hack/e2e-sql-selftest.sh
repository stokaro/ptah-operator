#!/bin/sh
# shellcheck disable=SC2034,SC2329 # The extracted wrappers read these globals and reach these stubs.

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
# So this runs hack/e2e-sql.sh with a kubectl of its own, and then the four
# wrappers the two phases actually call, read out of the phase scripts rather
# than copied. It refuses them unless
#
#   - a statement whose exec failed returns what the exec returned, so the
#     `|| fail` the call sites write fires, and
#   - a value still arrives trimmed, from the database it was asked for.
#
# What a call site does with those helpers is source, and
# verifySQLStatementGuards in hack/verify-kubernetes-support.go reads it: a
# statement that changes a database, handed back to a value helper, is refused
# there.
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

# Neither phase calls the split directly: each wraps it in a helper of its own,
# and the guarded wrapper differs from the value one it sits among by a single
# word. So the four wrappers are read out of the phase scripts rather than
# copied here, and driven with a kubectl of this test's own. A copy would go on
# passing after the phase stopped matching it, which is the defect this whole
# file exists for.
WRAPPERS_FILE=$WORK_DIR/wrappers.sh
: >"$WRAPPERS_FILE"

extract_helper() {
	helper_source=$1
	helper_name=$2
	helper_section=$(sed -n "/^${helper_name}()/,/^}/p" "$helper_source")
	[ -n "$helper_section" ] ||
		fail "could not read $helper_name out of $helper_source"
	printf '%s\n' "$helper_section" >>"$WRAPPERS_FILE" ||
		fail "could not stage $helper_name"
}

extract_helper "$ROOT_DIR/hack/e2e-migrations.sh" migration_query
extract_helper "$ROOT_DIR/hack/e2e-migrations.sh" migration_statement
extract_helper "$ROOT_DIR/hack/e2e-reference-data.sh" reference_query
extract_helper "$ROOT_DIR/hack/e2e-reference-data.sh" reference_statement

# The migrations phase undoes a half-applied migration by hand, on PostgreSQL
# here so the branch the statement path takes is the one asserted.
status=0
(
	ENGINE=postgresql
	TEST_NAMESPACE=e2e
	DATABASE_SERVICE=e2e-postgresql
	MIGRATION_DATABASE=widgets
	k() {
		for sql_arg in "$@"; do
			printf '%s\n' "$sql_arg"
		done >"$WORK_DIR/migration-statement-argv.txt"
		printf 'error: unable to upgrade connection\n' >&2
		return 7
	}
	# shellcheck source=hack/e2e-sql.sh
	. "$ROOT_DIR/hack/e2e-sql.sh"
	# shellcheck source=/dev/null
	. "$WRAPPERS_FILE"
	migration_statement "ALTER TABLE e2e_migration_widgets DROP COLUMN weight" >/dev/null ||
		exit 9
) 2>/dev/null || status=$?
[ "$status" -eq 9 ] ||
	fail "the migrations wrapper did not fail the guard on a failed statement: the phase saw status $status"
observed_database=$(tail -n 2 "$WORK_DIR/migration-statement-argv.txt" | head -n 1)
observed_statement=$(tail -n 1 "$WORK_DIR/migration-statement-argv.txt")
[ "$observed_database" = widgets ] ||
	fail "the migrations wrapper ran its statement against database \"$observed_database\", want widgets"
[ "$observed_statement" = "ALTER TABLE e2e_migration_widgets DROP COLUMN weight" ] ||
	fail "the migrations wrapper asked the client to run \"$observed_statement\""

# The reference-data phase edits a managed row from outside the operator, and a
# proof of a stale approval that never made the edit proves nothing.
status=0
(
	ENGINE=mysql
	TEST_NAMESPACE=e2e
	DATABASE_SERVICE=e2e-mysql
	REFERENCE_DATABASE=countries
	k() {
		for sql_arg in "$@"; do
			printf '%s\n' "$sql_arg"
		done >"$WORK_DIR/reference-statement-argv.txt"
		printf 'ERROR 1146 (42S02): Table does not exist\n' >&2
		return 1
	}
	# shellcheck source=hack/e2e-sql.sh
	. "$ROOT_DIR/hack/e2e-sql.sh"
	# shellcheck source=/dev/null
	. "$WRAPPERS_FILE"
	reference_statement "UPDATE countries SET name = 'Edited outside the operator' WHERE code = 'US'" >/dev/null ||
		exit 9
) 2>/dev/null || status=$?
[ "$status" -eq 9 ] ||
	fail "the reference-data wrapper did not fail the guard on a failed statement: the phase saw status $status"
observed_database=$(tail -n 2 "$WORK_DIR/reference-statement-argv.txt" | head -n 1)
[ "$observed_database" = countries ] ||
	fail "the reference-data wrapper ran its statement against database \"$observed_database\", want countries"

# The value wrappers keep the trim their forty-odd callers compare against, and
# the migrations one still lets a caller name a second database: the adoption
# and branch proofs read their own.
observed=$(
	ENGINE=postgresql
	TEST_NAMESPACE=e2e
	DATABASE_SERVICE=e2e-postgresql
	MIGRATION_DATABASE=widgets
	k() {
		for sql_arg in "$@"; do
			printf '%s\n' "$sql_arg"
		done >"$WORK_DIR/migration-query-argv.txt"
		printf ' 3 \n'
	}
	# shellcheck source=hack/e2e-sql.sh
	. "$ROOT_DIR/hack/e2e-sql.sh"
	# shellcheck source=/dev/null
	. "$WRAPPERS_FILE"
	migration_query "SELECT count(*) FROM e2e_migration_widgets" adopted
)
[ "$observed" = 3 ] ||
	fail "the migrations value wrapper returned \"$observed\", and the phase compares against 3"
observed_database=$(tail -n 2 "$WORK_DIR/migration-query-argv.txt" | head -n 1)
[ "$observed_database" = adopted ] ||
	fail "the migrations value wrapper read database \"$observed_database\", want the one the caller named"

observed=$(
	ENGINE=mysql
	TEST_NAMESPACE=e2e
	DATABASE_SERVICE=e2e-mysql
	REFERENCE_DATABASE=countries
	k() {
		for sql_arg in "$@"; do
			printf '%s\n' "$sql_arg"
		done >"$WORK_DIR/reference-query-argv.txt"
		printf '2\t\n'
	}
	# shellcheck source=hack/e2e-sql.sh
	. "$ROOT_DIR/hack/e2e-sql.sh"
	# shellcheck source=/dev/null
	. "$WRAPPERS_FILE"
	reference_query "SELECT count(*) FROM regions"
)
[ "$observed" = 2 ] ||
	fail "the reference-data value wrapper returned \"$observed\", and the phase compares against 2"
observed_database=$(tail -n 2 "$WORK_DIR/reference-query-argv.txt" | head -n 1)
[ "$observed_database" = countries ] ||
	fail "the reference-data value wrapper read database \"$observed_database\", want countries"

printf '%s\n' 'e2e sql self-test: PASS the phase wrappers fail a guard on a failed statement, and a value still arrives trimmed'
