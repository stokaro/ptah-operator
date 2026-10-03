package e2e

import (
	"errors"
	"strings"
)

// Only the exact Apply whose result refused the stale plan may use the
// saved-plan diagnostic contract. Other Applies remain unauthorized, and
// Observe/Plan retain their own narrower operation permissions.
type schemaStaleSQL struct {
	policy                  *schemaSQLPolicy
	refused                 operationSQLClient
	lock, columns, unlocked bool
}

func newSchemaStaleSQL(policy *schemaSQLPolicy, refused operationSQLClient) (*schemaStaleSQL, error) {
	if policy == nil || refused.resourceUID == "" || refused.jobUID == "" || refused.podUID == "" || refused.operation != "apply" {
		return nil, errors.New("stale-plan SQL audit needs the exact refused Apply identity")
	}
	return &schemaStaleSQL{policy: policy, refused: refused}, nil
}

func (s *schemaStaleSQL) acceptsActor(actor operationSQLClient) bool {
	return actor == s.refused || (actor.resourceUID == s.refused.resourceUID && schemaDiagnosticActor(actor))
}

func (s *schemaStaleSQL) postgres(actor operationSQLClient, statement, parameters string) bool {
	if !s.acceptsActor(actor) {
		return false
	}
	if actor != s.refused {
		return s.policy.postgres(actor, statement, parameters)
	}
	if s.policy.engine != "postgresql" || !s.policy.allowed[schemaSQLKey{"stale-apply", "query", statement, parameters}] {
		return false
	}
	s.record("query", statement)
	return true
}

func (s *schemaStaleSQL) mysql(actor operationSQLClient, command, statement, database string) bool {
	if !s.acceptsActor(actor) {
		return false
	}
	if actor != s.refused {
		return s.policy.mysql(actor, command, statement, database)
	}
	if s.policy.engine != "mysql" || database != s.policy.database || !s.policy.allowed[schemaSQLKey{"stale-apply", command, statement, ""}] {
		return false
	}
	s.record(command, statement)
	return true
}

// Classification here measures required executions only after the entire
// statement and parameters passed the exact contract above. Prepare alone
// cannot establish that a lock or schema read reached execution.
func (s *schemaStaleSQL) record(command, statement string) {
	if command != "query" && command != "Execute" && command != "Query" {
		return
	}
	switch statement {
	case "SELECT pg_try_advisory_lock($1)", "SELECT GET_LOCK('ptah_schema_apply', 60)":
		s.lock = true
	case "SELECT pg_advisory_unlock($1)", "SELECT RELEASE_LOCK('ptah_schema_apply')":
		s.unlocked = true
	}
	if strings.Contains(statement, "FROM information_schema.columns col") ||
		strings.Contains(statement, "FROM information_schema.COLUMNS") && strings.Contains(statement, "ORDINAL_POSITION") {
		s.columns = true
	}
}

func (s *schemaStaleSQL) complete() error {
	if !s.lock || !s.columns || !s.unlocked {
		return errors.New("stale-plan SQL audit did not observe the refused Apply's lock, column read and unlock executions")
	}
	return nil
}

const postgresSchemaFingerprintSQL = `
    SELECT md5(COALESCE(string_agg(
      table_schema || '.' || table_name || '.' || column_name || ':' || data_type || ':' || is_nullable,
      ',' ORDER BY table_schema, table_name, ordinal_position), ''))
    FROM information_schema.columns
    WHERE table_schema = 'public'`

// The surrounding journal permits these harness reads only on loopback.
// Operator clients cannot borrow them, and no local mutation is exempted.
func postgresSchemaDriftHarnessRead(statement, parameters string) bool {
	if parameters != "" {
		return false
	}
	switch statement {
	case postgresSchemaFingerprintSQL,
		"SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='enabled'",
		"SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='fault_token'":
		return true
	}
	return false
}
