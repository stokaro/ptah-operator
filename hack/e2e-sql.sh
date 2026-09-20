#!/bin/sh

# The SQL the acceptance phases run against the databases they stood up,
# sourced by the phases that need it.
#
# Two shapes, because the exit status of a pipeline belongs to its last stage.
# A helper that ends in `tr` reports tr, so a `|| fail` guard on one never sees
# the exec: a statement that failed is skipped in silence and the phase dies at
# its next wait, accusing the operator of not reaching a phase the harness
# never set up. Capturing the value does not help, because command substitution
# takes the function's status and the function still ends in the pipe. So a
# statement run for its status ends in the exec, and the callers that read a
# value keep the trim.
#
# hack/e2e-sql-selftest.sh is what keeps that split true.

# sql_statement runs one statement and reports what the exec reported. Use it
# wherever the phase guards the statement instead of comparing its output.
sql_statement() {
	sql_engine=$1
	sql_namespace=$2
	sql_service=$3
	sql_database=$4
	sql_text=$5
	case "$sql_engine" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$sql_namespace" exec deployment/"$sql_service" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"' \
			sh "$sql_database" "$sql_text"
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$sql_namespace" exec deployment/"$sql_service" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"' \
			sh "$sql_database" "$sql_text"
		;;
	*)
		# A phase that ran one engine because nobody named one is coverage
		# nobody would notice was gone.
		printf 'e2e sql: unsupported engine %s\n' "$sql_engine" >&2
		return 1
		;;
	esac
}

# sql_value runs one statement for what it printed. The two clients pad and
# terminate a value differently, so the trim is what lets one comparison mean
# the same thing on both. A caller catches a failed exec by comparing the value
# it did not get.
sql_value() {
	sql_statement "$@" | tr -d '[:space:]'
}
