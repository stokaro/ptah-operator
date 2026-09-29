package e2e

// History edits are refused after the Apply has opened the database and taken
// Ptah's migration lock. Only the exact refused Job gets these additional
// diagnostics; a History Job or a different Apply never inherits them.
type migrationRefusalSQL struct {
	applyJobUID             string
	lock, selection, unlock bool
}

func (r *migrationRefusalSQL) acceptsActor(actor migrationSQLClient) bool {
	return actor.jobUID != "" && actor.podUID != "" &&
		(actor.operation == "history" ||
			(r.applyJobUID != "" && actor.jobUID == r.applyJobUID && actor.operation == "apply"))
}

func (r *migrationRefusalSQL) complete() bool {
	return r.applyJobUID == "" || (r.lock && r.selection && r.unlock)
}

func (r *migrationRefusalSQL) postgres(actor migrationSQLClient, statement, parameters string) bool {
	if postgresMigrationHistoryStatement(statement, parameters) {
		return true
	}
	if actor.operation != "apply" || actor.jobUID != r.applyJobUID || r.applyJobUID == "" {
		return false
	}
	switch {
	case statement == "SELECT pg_try_advisory_lock($1)" && parameters == "Parameters: $1 = '2705505214'":
		r.lock = true
	case statement == "SELECT pg_advisory_unlock($1)" && parameters == "Parameters: $1 = '2705505214'":
		r.unlock = true
	case statement == postgresMigrationUnresolvedRead && parameters == "Parameters: $1 = 'applied'":
		r.selection = true
	default:
		return false
	}
	return true
}

func (r *migrationRefusalSQL) mysql(actor migrationSQLClient, command, statement, database string) bool {
	if mysqlMigrationHistoryStatement(command, statement, database) {
		return true
	}
	if actor.operation != "apply" || actor.jobUID != r.applyJobUID || r.applyJobUID == "" ||
		!mysqlAuditIdentifier.MatchString(database) || len(database) > 64 {
		return false
	}
	if command == "Prepare" {
		return statement == "SELECT GET_LOCK(?, ?)" || statement == "SELECT RELEASE_LOCK(?)" ||
			statement == mysqlMigrationUnresolvedRead(database, "?")
	}
	if command != "Execute" {
		return false
	}
	switch statement {
	case "SELECT GET_LOCK('ptah_migrate', 30)":
		r.lock = true
	case "SELECT RELEASE_LOCK('ptah_migrate')":
		r.unlock = true
	case mysqlMigrationUnresolvedRead(database, "'applied'"):
		r.selection = true
	default:
		return false
	}
	return true
}

// These fixtures use the default revision table and a 30-second migration lock
// timeout. 2705505214 is Ptah's FNV-1a key for its default "ptah_migrate" lock.
// Source and actual refused executions are retained with the journal readings.
const postgresMigrationUnresolvedRead = `SELECT version, description, state, applied, total, COALESCE(error, ''), COALESCE(error_stmt, ''), execution_time_ms, checksum, applied_at
FROM "schema_migrations"
WHERE state <> $1 OR applied <> total OR (applied < 0 OR total < 0 OR applied > total)
ORDER BY version
LIMIT 1`

func mysqlMigrationUnresolvedRead(database, state string) string {
	return "SELECT version, description, state, applied, total, COALESCE(error, ''), COALESCE(error_stmt, ''), execution_time_ms, checksum, applied_at\n" +
		"FROM `" + database + "`.`schema_migrations`\n" +
		"WHERE state <> " + state + " OR applied <> total OR (applied < 0 OR total < 0 OR applied > total)\n" +
		"ORDER BY version\nLIMIT 1"
}

func postgresMigrationRestoreHarnessRead(statement, parameters string) bool {
	return parameters == "" && (statement == "SELECT COALESCE(string_agg(version::text, ',' ORDER BY version), '') FROM schema_migrations" ||
		statement == "SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'e2e_migration_widgets' AND column_name = 'color'")
}
