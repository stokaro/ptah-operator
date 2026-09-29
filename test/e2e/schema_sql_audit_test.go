package e2e

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const schemaAuditDatabase = "ptah_audit_schema"

func schemaAuditReading(t *testing.T, engine, operation string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", engine+"-schema-"+operation+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func schemaAuditActor(operation string) operationSQLClient {
	return operationSQLClient{resourceUID: "schema", jobUID: "job", podUID: "pod", operation: operation}
}

func TestSchemaSQLContractHasActualWitnessesAtThePinnedSource(t *testing.T) {
	t.Parallel()
	var contract struct {
		PtahCommit string `json:"ptahCommit"`
	}
	if err := json.Unmarshal(schemaSQLContract, &contract); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "support", "ptah.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Releases []struct {
			Operator string `json:"operator"`
			Verified []struct {
				PtahCommit string `json:"ptahCommit"`
			} `json:"verified"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	pins := 0
	for _, release := range catalog.Releases {
		if release.Operator == "edge" {
			for _, verified := range release.Verified {
				pins++
				if verified.PtahCommit != contract.PtahCommit {
					t.Fatal("schema SQL contract needs review against the new executor source")
				}
			}
		}
	}
	if pins != 1 {
		t.Fatalf("expected one executor pin, got %d", pins)
	}
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			policy, err := newSchemaSQLPolicy(engine, schemaAuditDatabase)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[schemaSQLKey]bool{}
			for _, variant := range []string{"", "destructive-", "exclusion-wide-", "exclusion-narrow-"} {
				for _, operation := range []string{"observe", "plan"} {
					actor := schemaAuditActor(operation)
					clients := map[string]operationSQLClient{mysqlAuditHost: actor}
					var counts map[string]int
					if engine == "postgresql" {
						counts, err = postgresStatementRefusalSQL(schemaAuditReading(t, engine, variant+operation), schemaAuditDatabase, clients, schemaDiagnosticActor,
							func(a operationSQLClient, sql, parameters string) bool {
								seen[schemaSQLKey{a.operation, "query", sql, parameters}] = true
								return policy.postgres(a, sql, parameters)
							}, nil)
					} else {
						rows, parseErr := mysqlStatementJournal(schemaAuditReading(t, engine, variant+operation))
						if parseErr != nil {
							t.Fatal(parseErr)
						}
						before := mysqlAuditBaseline()
						counts, err = mysqlStatementRefusalSQL(before, append(slices.Clone(before), rows...), schemaAuditDatabase, mysqlAuditUser, clients, true, schemaDiagnosticActor,
							func(a operationSQLClient, command, sql, database string) bool {
								seen[schemaSQLKey{a.operation, command, sql, ""}] = true
								return policy.mysql(a, command, sql, database)
							})
					}
					want := map[string]map[string]int{"postgresql": {"observe": 45, "plan": 82}, "mysql": {"observe": 14, "plan": 27}}[engine][operation]
					if err != nil || len(counts) != 1 || counts[mysqlAuditHost] != want {
						t.Fatalf("%s %s%s received SQL: counts=%v, error=%v", engine, variant, operation, counts, err)
					}
				}
			}
			if len(seen) != len(policy.allowed) || len(seen) == 0 {
				t.Fatalf("witnessed %d of %d declarations", len(seen), len(policy.allowed))
			}
			for key := range policy.allowed {
				if !seen[key] {
					t.Fatal("a permitted diagnostic has no actual executor witness")
				}
			}
		})
	}
}

func TestSchemaSQLContractRefusesBroaderStatementsAndActors(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"postgresql", "mysql"} {
		policy, err := newSchemaSQLPolicy(engine, schemaAuditDatabase)
		if err != nil {
			t.Fatal(err)
		}
		accepts := func(actor operationSQLClient, key schemaSQLKey) bool {
			if engine == "postgresql" {
				return policy.postgres(actor, key.statement, key.parameters)
			}
			return policy.mysql(actor, key.command, key.statement, schemaAuditDatabase)
		}
		for key := range policy.allowed {
			actor := schemaAuditActor(key.operation)
			if !accepts(actor, key) {
				t.Fatal("declared diagnostic control refused")
			}
			for _, bad := range []string{
				"SELECT mutating_function()", "WITH changed AS (DELETE FROM e2e_widgets RETURNING *) SELECT * FROM changed",
				"COMMIT", "ALTER TABLE e2e_widgets ADD COLUMN unauthorized TEXT", key.statement + "; DROP TABLE e2e_widgets",
			} {
				changed := key
				changed.statement = bad
				if accepts(actor, changed) {
					t.Fatal("broader SQL was permitted")
				}
			}
			for _, bad := range []operationSQLClient{
				{jobUID: "job", podUID: "pod", operation: actor.operation},
				{jobUID: "job", podUID: "pod", operation: "apply"}, {jobUID: "job", podUID: "pod", operation: "history"},
				{jobUID: "job", operation: actor.operation}, {podUID: "pod", operation: actor.operation},
			} {
				if accepts(bad, key) {
					t.Fatal("SQL lacked its diagnostic Job and Pod")
				}
			}
			if engine == "postgresql" {
				changed := key
				changed.parameters += "Parameters: $1 = 'other'"
				if accepts(actor, changed) {
					t.Fatal("changed bind parameters passed")
				}
				for _, from := range []string{"pg_temp", "\"id\" bigint", "ptah_column_probe_0"} {
					if strings.Contains(key.statement, from) {
						changed = key
						changed.statement = strings.ReplaceAll(key.statement, from, "unauthorized")
						if accepts(actor, changed) {
							t.Fatal("changed temporary probe passed")
						}
					}
				}
			} else if policy.mysql(actor, key.command, key.statement, "other") {
				t.Fatal("diagnostics on another database passed")
			}
		}
	}
	for _, database := range []string{"", "db' OR 1=1", strings.Repeat("a", 64)} {
		if _, err := newSchemaSQLPolicy("mysql", database); err == nil {
			t.Fatal("uncontrolled identifier passed")
		}
	}
	if _, err := newSchemaSQLPolicy("sqlite", "database"); err == nil {
		t.Fatal("undeclared engine passed")
	}
}

func TestPostgresSchemaSQLAuditRejectsUnattributedAndIncompleteWindows(t *testing.T) {
	t.Parallel()
	policy, err := newSchemaSQLPolicy("postgresql", schemaAuditDatabase)
	if err != nil {
		t.Fatal(err)
	}
	raw := schemaAuditReading(t, "postgresql", "plan")
	clients := map[string]operationSQLClient{mysqlAuditHost: schemaAuditActor("plan")}
	check := func(raw []byte) error {
		_, err := postgresStatementRefusalSQL(raw, schemaAuditDatabase, clients, schemaDiagnosticActor, policy.postgres, nil)
		return err
	}
	if err := check(raw); err != nil {
		t.Fatal(err)
	}
	for name, broken := range map[string][]byte{
		"empty": nil, "truncated": raw[:len(raw)-10],
		"unknown client":       []byte(strings.ReplaceAll(string(raw), mysqlAuditHost, "172.19.0.4")),
		"another database":     []byte(strings.ReplaceAll(string(raw), schemaAuditDatabase, "other")),
		"missing database":     []byte(strings.ReplaceAll(string(raw), schemaAuditDatabase, "")),
		"unapproved local SQL": []byte(strings.ReplaceAll(string(raw), mysqlAuditHost, "127.0.0.1")),
	} {
		t.Run(name, func(t *testing.T) {
			if check(broken) == nil {
				t.Fatal("incomplete or unattributed SQL passed")
			}
		})
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var rows []map[string]any
	for {
		var row map[string]any
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	for _, row := range rows {
		// Each permitted record must cease to pass after an appended statement.
		message, _ := row["message"].(string)
		if !strings.HasPrefix(message, "statement: ") && !strings.HasPrefix(message, "execute ") {
			continue
		}
		row["message"] = message + "; SELECT private_password_canary()"
		modified, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if err := check(modified); err == nil || strings.Contains(err.Error(), "private_password_canary") {
			t.Fatal("mutated SQL passed or leaked its text")
		}
	}
}

func TestMySQLSchemaSQLAuditRequiresWholeSessions(t *testing.T) {
	t.Parallel()
	policy, err := newSchemaSQLPolicy("mysql", schemaAuditDatabase)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := mysqlStatementJournal(schemaAuditReading(t, "mysql", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	before := mysqlAuditBaseline()
	clients := map[string]operationSQLClient{mysqlAuditHost: schemaAuditActor("plan")}
	check := func(r []mysqlStatementRecord) error {
		_, err := mysqlStatementRefusalSQL(before, append(slices.Clone(before), r...), schemaAuditDatabase, mysqlAuditUser, clients, true, schemaDiagnosticActor, policy.mysql)
		return err
	}
	if err := check(rows); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]mysqlStatementRecord) []mysqlStatementRecord{
		"missing Connect": func(r []mysqlStatementRecord) []mysqlStatementRecord { return r[1:] },
		"missing Quit":    func(r []mysqlStatementRecord) []mysqlStatementRecord { return r[:len(r)-1] },
		"empty control": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			return []mysqlStatementRecord{r[0], r[len(r)-1]}
		},
		"other account": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			for i := range r {
				r[i].Client = strings.ReplaceAll(r[i].Client, mysqlAuditUser, "other")
			}
			return r
		},
		"unknown host": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			for i := range r {
				r[i].Client = strings.ReplaceAll(r[i].Client, mysqlAuditHost, "172.19.0.4")
			}
			return r
		},
		"other database": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			sql, _ := hex.DecodeString(r[0].ArgumentHex)
			r[0].ArgumentHex = hex.EncodeToString([]byte(strings.ReplaceAll(string(sql), schemaAuditDatabase, "other")))
			return r
		},
		"reversed rows": func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1], r[2] = r[2], r[1]; return r },
	} {
		t.Run(name, func(t *testing.T) {
			if check(mutate(slices.Clone(rows))) == nil {
				t.Fatal("incomplete or unrelated session passed")
			}
		})
	}
	for i, row := range rows {
		if row.Command != "Query" && row.Command != "Prepare" && row.Command != "Execute" {
			continue
		}
		broken := slices.Clone(rows)
		sql, _ := hex.DecodeString(row.ArgumentHex)
		broken[i].ArgumentHex = hex.EncodeToString(append(sql, []byte("; SELECT private_password_canary()")...))
		if err := check(broken); err == nil || strings.Contains(err.Error(), "private_password_canary") {
			t.Fatal("mutated SQL passed or leaked its text")
		}
	}
}

func TestSchemaReplacementSQLControlsRequireBothOperationsOfBothUIDs(t *testing.T) {
	t.Parallel()
	clients, counts := map[string]operationSQLClient{}, map[string]int{}
	for _, uid := range []string{"old", "current"} {
		for _, op := range []string{"observe", "plan"} {
			key := uid + op
			clients[key] = operationSQLClient{resourceUID: uid, jobUID: key, podUID: key, operation: op}
			counts[key] = 1
		}
	}
	if err := schemaReplacementSQLControls(clients, counts, "old", "current"); err != nil {
		t.Fatal(err)
	}
	for key, actor := range clients {
		counts[key] = 0
		if schemaReplacementSQLControls(clients, counts, "old", "current") == nil {
			t.Fatal("missing diagnostic SQL passed")
		}
		counts[key] = 1
		for _, bad := range []operationSQLClient{
			{resourceUID: "other", jobUID: actor.jobUID, podUID: actor.podUID, operation: actor.operation},
			{resourceUID: actor.resourceUID, jobUID: actor.jobUID, podUID: actor.podUID, operation: "apply"},
			{resourceUID: actor.resourceUID, podUID: actor.podUID, operation: actor.operation},
		} {
			clients[key] = bad
			if schemaReplacementSQLControls(clients, counts, "old", "current") == nil {
				t.Fatal("unbound control passed")
			}
		}
		clients[key] = actor
	}
	if schemaReplacementSQLControls(clients, counts, "old", "old") == nil {
		t.Fatal("same UID counted as replacement")
	}
}

func TestSchemaSQLResultControlsRequireEachExactJobAndPod(t *testing.T) {
	t.Parallel()
	initialObserve := operationSQLClient{resourceUID: "schema", jobUID: "observe", podUID: "observe-pod", operation: "observe"}
	initialPlan := operationSQLClient{resourceUID: "schema", jobUID: "old-plan", podUID: "old-plan-pod", operation: "plan"}
	freshPlan := operationSQLClient{resourceUID: "schema", jobUID: "fresh-plan", podUID: "fresh-plan-pod", operation: "plan"}
	required := []operationSQLClient{initialObserve, initialPlan, freshPlan}
	clients := map[string]operationSQLClient{"10.0.0.1": initialObserve, "10.0.0.2": initialPlan, "10.0.0.3": freshPlan}
	counts := map[string]int{"10.0.0.1": 45, "10.0.0.2": 82, "10.0.0.3": 82}
	if err := schemaRequiredSQLControls(clients, counts, required); err != nil {
		t.Fatal(err)
	}
	for host, count := range counts {
		counts[host] = 0
		if schemaRequiredSQLControls(clients, counts, required) == nil {
			t.Fatal("another Job's SQL substituted for a missing result control")
		}
		counts[host] = count
	}
	for name, mutate := range map[string]func(*operationSQLClient){
		"another resource":  func(a *operationSQLClient) { a.resourceUID = "other" },
		"another Job":       func(a *operationSQLClient) { a.jobUID = "other" },
		"another Pod":       func(a *operationSQLClient) { a.podUID = "other" },
		"another operation": func(a *operationSQLClient) { a.operation = "observe" },
		"missing resource":  func(a *operationSQLClient) { a.resourceUID = "" },
		"missing Job":       func(a *operationSQLClient) { a.jobUID = "" },
		"missing Pod":       func(a *operationSQLClient) { a.podUID = "" },
		"Apply":             func(a *operationSQLClient) { a.operation = "apply" },
	} {
		t.Run(name, func(t *testing.T) {
			broken := slices.Clone(required)
			mutate(&broken[2])
			if schemaRequiredSQLControls(clients, counts, broken) == nil {
				t.Fatal("unrelated or incomplete result control passed")
			}
		})
	}
	for _, broken := range [][]operationSQLClient{nil, {initialPlan, initialPlan},
		{initialPlan, {resourceUID: "schema", jobUID: "other", podUID: initialPlan.podUID, operation: "plan"}},
		{initialPlan, {resourceUID: "schema", jobUID: initialPlan.jobUID, podUID: "other", operation: "plan"}},
	} {
		if schemaRequiredSQLControls(clients, counts, broken) == nil {
			t.Fatal("empty or duplicate controls passed")
		}
	}
	clients["10.0.0.4"], counts["10.0.0.4"] = freshPlan, 82
	if schemaRequiredSQLControls(clients, counts, required) == nil {
		t.Fatal("ambiguous client identity passed")
	}
}
