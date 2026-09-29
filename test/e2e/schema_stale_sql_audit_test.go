package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSchemaStaleSQLUsesExactRefusedApply(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			policy, err := newSchemaSQLPolicy(engine, schemaAuditDatabase)
			if err != nil {
				t.Fatal(err)
			}
			refused := schemaAuditActor("apply")
			check := func(raw []byte, actor operationSQLClient) error {
				t.Helper()
				stale, err := newSchemaStaleSQL(policy, refused)
				if err != nil {
					return err
				}
				clients := map[string]operationSQLClient{mysqlAuditHost: actor}
				if engine == "postgresql" {
					_, err = postgresStatementRefusalSQL(raw, schemaAuditDatabase, clients, stale.acceptsActor, stale.postgres, postgresSchemaDriftHarnessRead)
				} else {
					rows, parseErr := mysqlStatementJournal(raw)
					if parseErr != nil {
						return parseErr
					}
					before := mysqlAuditBaseline()
					_, err = mysqlStatementRefusalSQL(before, append(slices.Clone(before), rows...), schemaAuditDatabase, mysqlAuditUser, clients, true, stale.acceptsActor, stale.mysql)
				}
				if err != nil {
					return err
				}
				return stale.complete()
			}
			raw := schemaAuditReading(t, engine, "drift-stale-apply")
			if err := check(raw, refused); err != nil {
				t.Fatal(err)
			}
			for name, change := range map[string]func(*operationSQLClient){
				"resource": func(a *operationSQLClient) { a.resourceUID = "another-schema" },
				"Job":      func(a *operationSQLClient) { a.jobUID = "another-job" },
				"Pod":      func(a *operationSQLClient) { a.podUID = "another-pod" },
				"Observe":  func(a *operationSQLClient) { a.operation = "observe" },
				"Plan":     func(a *operationSQLClient) { a.operation = "plan" },
				"History":  func(a *operationSQLClient) { a.operation = "history" },
			} {
				t.Run(name, func(t *testing.T) {
					actor := refused
					change(&actor)
					if check(raw, actor) == nil {
						t.Fatal("a different workload supplied the refused Apply's evidence")
					}
				})
			}
			// Remove each required execution from the actual journal. Keeping
			// Prepare cannot replace execution, and other reads cannot replace
			// the column inspection that established the changed schema.
			for name, removes := range map[string]func(string) bool{
				"lock": func(s string) bool {
					return strings.Contains(s, "pg_try_advisory_lock(") || strings.Contains(s, "GET_LOCK(")
				},
				"unlock": func(s string) bool {
					return strings.Contains(s, "pg_advisory_unlock(") || strings.Contains(s, "RELEASE_LOCK(")
				},
				"columns": func(s string) bool {
					return strings.Contains(s, "FROM information_schema.columns col") || strings.Contains(s, "FROM information_schema.COLUMNS") && strings.Contains(s, "ORDINAL_POSITION")
				},
			} {
				t.Run("missing "+name, func(t *testing.T) {
					var kept [][]byte
					removed := 0
					for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
						command, statement := schemaJournalStatement(t, engine, line)
						if command != "Prepare" && removes(statement) {
							removed++
							continue
						}
						kept = append(kept, line)
					}
					if removed == 0 || check(bytes.Join(kept, []byte("\n")), refused) == nil {
						t.Fatal("incomplete refused-Apply execution passed")
					}
				})
			}
			// A real successful Apply shares the diagnostic prefix. It must
			// fail on the actual mutations, including when tested individually.
			applied := schemaAuditReading(t, engine, "drift-apply-current")
			if check(applied, refused) == nil {
				t.Fatal("the mutating Apply was accepted as a refusal")
			}
			ddl := 0
			for _, line := range bytes.Split(bytes.TrimSpace(applied), []byte("\n")) {
				command, statement := schemaJournalStatement(t, engine, line)
				if !strings.HasPrefix(statement, "ALTER TABLE ") {
					continue
				}
				ddl++
				stale, err := newSchemaStaleSQL(policy, refused)
				if err != nil {
					t.Fatal(err)
				}
				if engine == "postgresql" && stale.postgres(refused, statement, "") || engine == "mysql" && stale.mysql(refused, command, statement, schemaAuditDatabase) {
					t.Fatal("actual Apply DDL was permitted")
				}
			}
			if ddl != 2 {
				t.Fatalf("examined %d of the two actual column additions", ddl)
			}
		})
	}
}

func schemaJournalStatement(t *testing.T, engine string, line []byte) (string, string) {
	t.Helper()
	var row struct {
		Message  string `json:"message"`
		Command  string `json:"type"`
		Argument string `json:"argumentHex"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		t.Fatal(err)
	}
	if engine == "postgresql" {
		_, statement, _ := strings.Cut(row.Message, ": ")
		return "query", statement
	}
	value, err := hex.DecodeString(row.Argument)
	if err != nil {
		t.Fatal(err)
	}
	return row.Command, string(value)
}

func TestSchemaStaleSQLAndHarnessPermissionsStayNarrow(t *testing.T) {
	t.Parallel()
	actor := schemaAuditActor("apply")
	for _, engine := range []string{"postgresql", "mysql"} {
		policy, err := newSchemaSQLPolicy(engine, schemaAuditDatabase)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []operationSQLClient{{}, {operation: "apply"}, schemaAuditActor("plan")} {
			if _, err := newSchemaStaleSQL(policy, bad); err == nil {
				t.Fatal("missing refused Apply identity was accepted")
			}
		}
		stale, err := newSchemaStaleSQL(policy, actor)
		if err != nil {
			t.Fatal(err)
		}
		if stale.complete() == nil {
			t.Fatal("empty evidence passed")
		}
		for key := range policy.allowed {
			if key.operation == "stale-apply" {
				continue
			}
			diagnostic := schemaAuditActor(key.operation)
			diagnostic.jobUID, diagnostic.podUID = "diagnostic-job", "diagnostic-pod"
			if engine == "postgresql" && !stale.postgres(diagnostic, key.statement, key.parameters) ||
				engine == "mysql" && !stale.mysql(diagnostic, key.command, key.statement, schemaAuditDatabase) {
				t.Fatal("recovery diagnostics lost their operation-specific permission")
			}
		}
		if stale.complete() == nil {
			t.Fatal("recovery diagnostics substituted for the refused Apply's executions")
		}
		if engine == "mysql" {
			for _, timeout := range []string{"-1", "0", "30", "45"} {
				if stale.mysql(actor, "Execute", "SELECT GET_LOCK('ptah_schema_apply', "+timeout+")", schemaAuditDatabase) {
					t.Fatal("changed Apply lock timeout passed")
				}
			}
			if stale.mysql(actor, "Execute", "SELECT GET_LOCK('ptah_schema_apply', 60)", "another_database") {
				t.Fatal("wrong database passed")
			}
		}
	}
	for _, query := range []string{postgresSchemaFingerprintSQL,
		"SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='enabled'",
		"SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='fault_token'"} {
		if !postgresSchemaDriftHarnessRead(query, "") {
			t.Fatal("exact harness read was refused")
		}
		if postgresSchemaDriftHarnessRead(query+"; DELETE FROM e2e_widgets", "") || postgresSchemaDriftHarnessRead(query, "Parameters: $1 = 'unexpected'") {
			t.Fatal("broader harness query passed")
		}
	}
	if postgresSchemaDriftHarnessRead("SELECT mutating_function()", "") {
		t.Fatal("arbitrary local SQL passed")
	}
}
