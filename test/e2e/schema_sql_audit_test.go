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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const schemaAuditDatabase = "ptah_audit_schema"

func TestSchemaSQLInventorySeparatesWindowsByJobUID(t *testing.T) {
	t.Parallel()
	_, resource, _, _ := schemaReplacementFixture()
	owner := func(version, kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: version, Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}
	}
	oldJob := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "same-job-name", Namespace: resource.Namespace, UID: "old-job",
		Labels:          map[string]string{labelSchema: resource.Name, labelOperation: "observe"},
		OwnerReferences: []metav1.OwnerReference{owner(ptahSchemaAPIVersion, "PtahSchema", resource.Name, resource.UID)}}}
	newJob := *oldJob.DeepCopy()
	newJob.UID = "new-job"
	oldPod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-pod", Namespace: resource.Namespace, UID: "old-pod",
		Labels:          map[string]string{labelSchema: resource.Name, labelOperation: "observe"},
		OwnerReferences: []metav1.OwnerReference{owner("batch/v1", "Job", oldJob.Name, oldJob.UID)}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, PodIP: "10.0.0.1"}}
	newPod := *oldPod.DeepCopy()
	newPod.UID, newPod.Name, newPod.OwnerReferences[0].UID, newPod.Status.PodIP = "new-pod", "new-pod", newJob.UID, mysqlAuditHost
	inventory := &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}, excludedJobs: checkpoint{string(oldJob.UID)}}
	if err := inventory.record([]batchv1.Job{oldJob, newJob}, []corev1.Pod{oldPod, newPod}); err != nil {
		t.Fatal(err)
	}
	// A later empty API reading after TTL cleanup must preserve the controls.
	if err := inventory.record(nil, nil); err != nil {
		t.Fatal(err)
	}
	clients, err := inventory.clients(resource)
	if err != nil || len(clients) != 1 || clients[mysqlAuditHost].jobUID != string(newJob.UID) || clients[mysqlAuditHost].podUID != string(newPod.UID) {
		t.Fatalf("the new window lost its exact new workload: clients=%v error=%v", clients, err)
	}
	policy, err := newSchemaSQLPolicy("postgresql", schemaAuditDatabase)
	if err != nil {
		t.Fatal(err)
	}
	raw := schemaAuditReading(t, "postgresql", "observe")
	if _, err := postgresStatementRefusalSQL(raw, schemaAuditDatabase, clients, schemaDiagnosticActor, policy.postgres, nil); err != nil {
		t.Fatal(err)
	}
	oldTraffic := []byte(strings.ReplaceAll(string(raw), mysqlAuditHost, oldPod.Status.PodIP))
	if _, err := postgresStatementRefusalSQL(oldTraffic, schemaAuditDatabase, clients, schemaDiagnosticActor, policy.postgres, nil); err == nil {
		t.Fatal("an earlier Job supplied the new window's SQL control")
	}
	if inventory.record([]batchv1.Job{{}}, nil) == nil || inventory.record(nil, []corev1.Pod{{}}) == nil {
		t.Fatal("an anonymous workload entered the retained evidence")
	}
}

func TestSchemaMySQLAuditAfterACompletedEarlierWindow(t *testing.T) {
	t.Parallel()
	read := func(variant string) []mysqlStatementRecord {
		t.Helper()
		rows, err := mysqlStatementJournal(schemaAuditReading(t, "mysql", variant+"-plan"))
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := append(mysqlAuditBaseline(), read("initial-v1")...)
	window := read("tag-v2")
	policy, err := newSchemaSQLPolicy("mysql", schemaAuditDatabase)
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]operationSQLClient{mysqlAuditHost: schemaAuditActor("plan")}
	check := func(rows []mysqlStatementRecord, unused bool) error {
		counts, err := mysqlStatementRefusalSQL(before, append(slices.Clone(before), rows...), schemaAuditDatabase, mysqlAuditUser, clients, unused, schemaDiagnosticActor, policy.mysql)
		if err == nil && counts[mysqlAuditHost] != 27 {
			t.Fatal("prior SQL was counted as a new diagnostic control")
		}
		return err
	}
	if err := check(window, false); err != nil {
		t.Fatal(err)
	}
	if check(window, true) == nil || check(window[1:], false) == nil || check(window[:len(window)-1], false) == nil {
		t.Fatal("the later window lost its account-use or complete-session boundary")
	}
}

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

// A runner Plan executes the saved plan with --dry-run before publishing it.
// These are the actual validation journals, not another schema plan invocation.
func TestSchemaSQLPlanValidation(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			policy, err := newSchemaSQLPolicy(engine, schemaAuditDatabase)
			if err != nil {
				t.Fatal(err)
			}
			check := func(raw []byte, operation string) (map[string]int, error) {
				clients := map[string]operationSQLClient{mysqlAuditHost: schemaAuditActor(operation)}
				if engine == "postgresql" {
					return postgresStatementRefusalSQL(raw, schemaAuditDatabase, clients, schemaDiagnosticActor, policy.postgres, nil)
				}
				rows, err := mysqlStatementJournal(raw)
				if err != nil {
					return nil, err
				}
				before := mysqlAuditBaseline()
				return mysqlStatementRefusalSQL(before, append(slices.Clone(before), rows...), schemaAuditDatabase, mysqlAuditUser, clients, true, schemaDiagnosticActor, policy.mysql)
			}
			raw := schemaAuditReading(t, engine, "drift-validate-plan")
			counts, err := check(raw, "plan")
			want := map[string]int{"postgresql": 43, "mysql": 17}[engine]
			if err != nil || len(counts) != 1 || counts[mysqlAuditHost] != want {
				t.Fatalf("native Plan validation: counts=%v, error=%v", counts, err)
			}
			for _, operation := range []string{"observe", "apply", "history"} {
				if _, err := check(raw, operation); err == nil {
					t.Fatalf("Plan validation SQL was allowed for %s", operation)
				}
			}
			applied := schemaAuditReading(t, engine, "drift-apply-current")
			if _, err := check(applied, "plan"); err == nil {
				t.Fatal("the successful Apply journal was allowed as Plan validation")
			}
			// Test the actual DDL directly as well: another earlier difference
			// in Apply (such as its 60s lock timeout) must not supply this refusal.
			ddl := 0
			for _, line := range strings.Split(strings.TrimSpace(string(applied)), "\n") {
				var fields struct {
					Message     string `json:"message"`
					Detail      string `json:"detail"`
					Command     string `json:"type"`
					ArgumentHex string `json:"argumentHex"`
				}
				if err := json.Unmarshal([]byte(line), &fields); err != nil {
					t.Fatal(err)
				}
				statement := strings.TrimPrefix(fields.Message, "statement: ")
				if engine == "mysql" {
					decoded, err := hex.DecodeString(fields.ArgumentHex)
					if err != nil {
						t.Fatal(err)
					}
					statement = string(decoded)
				}
				if !strings.HasPrefix(statement, "ALTER TABLE ") {
					continue
				}
				ddl++
				if engine == "postgresql" {
					if policy.postgres(schemaAuditActor("plan"), statement, fields.Detail) {
						t.Fatal("actual Apply DDL was allowed as Plan validation")
					}
				} else if policy.mysql(schemaAuditActor("plan"), fields.Command, statement, schemaAuditDatabase) {
					t.Fatal("actual Apply DDL was allowed as Plan validation")
				}
			}
			if ddl != 2 {
				t.Fatalf("examined %d actual Apply statements, expected both approved column additions", ddl)
			}
		})
	}
}

func TestSchemaPlanValidationLocksHaveExactScope(t *testing.T) {
	t.Parallel()
	pg, err := newSchemaSQLPolicy("postgresql", schemaAuditDatabase)
	if err != nil {
		t.Fatal(err)
	}
	my, err := newSchemaSQLPolicy("mysql", schemaAuditDatabase)
	if err != nil {
		t.Fatal(err)
	}
	actor := schemaAuditActor("plan")
	for _, statement := range []string{"SELECT pg_try_advisory_lock($1)", "SELECT pg_advisory_unlock($1)"} {
		if !pg.postgres(actor, statement, "Parameters: $1 = '1237737229'") {
			t.Fatal("the schema validation lock was refused")
		}
		for _, parameters := range []string{"", "Parameters: $1 = '1237737228'", "Parameters: $1 = '1237737229', $2 = 'other'"} {
			if pg.postgres(actor, statement, parameters) {
				t.Fatal("changed schema lock parameters were permitted")
			}
		}
	}
	for _, statement := range []string{"SELECT GET_LOCK('ptah_schema_apply', 30)", "SELECT RELEASE_LOCK('ptah_schema_apply')"} {
		if !my.mysql(actor, "Execute", statement, schemaAuditDatabase) {
			t.Fatal("the schema validation lock was refused")
		}
		for _, changed := range []string{
			strings.ReplaceAll(statement, "ptah_schema_apply", "ptah_migrate"),
			statement + "; DELETE FROM e2e_widgets",
		} {
			if my.mysql(actor, "Execute", changed, schemaAuditDatabase) {
				t.Fatal("a changed schema validation lock was permitted")
			}
		}
	}
	for _, timeout := range []string{"-1", "0", "45", "60"} {
		if my.mysql(actor, "Execute", "SELECT GET_LOCK('ptah_schema_apply', "+timeout+")", schemaAuditDatabase) {
			t.Fatal("the schema validation lock timeout changed")
		}
	}
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
			type witness struct {
				name, operation string
				count           int
			}
			var readings []witness
			for _, variant := range []string{"", "destructive-", "exclusion-wide-", "exclusion-narrow-", "initial-v1-", "tag-v2-", "tag-v3-", "drift-"} {
				for _, operation := range []string{"observe", "plan"} {
					count := map[string]map[string]int{"postgresql": {"observe": 45, "plan": 82}, "mysql": {"observe": 14, "plan": 27}}[engine][operation]
					if engine == "postgresql" && variant == "initial-v1-" {
						count = map[string]int{"observe": 39, "plan": 75}[operation]
					}
					readings = append(readings, witness{variant + operation, operation, count})
				}
			}
			readings = append(readings, witness{"drift-validate-plan", "plan", map[string]int{"postgresql": 43, "mysql": 17}[engine]},
				witness{"drift-stale-apply", "stale-apply", map[string]int{"postgresql": 43, "mysql": 17}[engine]})
			for _, reading := range readings {
				actor := schemaAuditActor(reading.operation)
				acceptsActor, pg, my := schemaDiagnosticActor, policy.postgres, policy.mysql
				if reading.operation == "stale-apply" {
					actor.operation = "apply"
					stale, err := newSchemaStaleSQL(policy, actor)
					if err != nil {
						t.Fatal(err)
					}
					acceptsActor, pg, my = stale.acceptsActor, stale.postgres, stale.mysql
				}
				clients := map[string]operationSQLClient{mysqlAuditHost: actor}
				var counts map[string]int
				if engine == "postgresql" {
					counts, err = postgresStatementRefusalSQL(schemaAuditReading(t, engine, reading.name), schemaAuditDatabase, clients, acceptsActor,
						func(a operationSQLClient, sql, parameters string) bool {
							seen[schemaSQLKey{reading.operation, "query", sql, parameters}] = true
							return pg(a, sql, parameters)
						}, nil)
				} else {
					rows, parseErr := mysqlStatementJournal(schemaAuditReading(t, engine, reading.name))
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					before := mysqlAuditBaseline()
					counts, err = mysqlStatementRefusalSQL(before, append(slices.Clone(before), rows...), schemaAuditDatabase, mysqlAuditUser, clients, true, acceptsActor,
						func(a operationSQLClient, command, sql, database string) bool {
							seen[schemaSQLKey{reading.operation, command, sql, ""}] = true
							return my(a, command, sql, database)
						})
				}
				if err != nil || len(counts) != 1 || counts[mysqlAuditHost] != reading.count {
					t.Fatalf("%s %s received SQL: counts=%v, error=%v", engine, reading.name, counts, err)
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
		stale, err := newSchemaStaleSQL(policy, schemaAuditActor("apply"))
		if err != nil {
			t.Fatal(err)
		}
		accepts := func(actor operationSQLClient, key schemaSQLKey) bool {
			if key.operation == "stale-apply" {
				if engine == "postgresql" {
					return stale.postgres(actor, key.statement, key.parameters)
				}
				return stale.mysql(actor, key.command, key.statement, schemaAuditDatabase)
			}
			if engine == "postgresql" {
				return policy.postgres(actor, key.statement, key.parameters)
			}
			return policy.mysql(actor, key.command, key.statement, schemaAuditDatabase)
		}
		for key := range policy.allowed {
			actor := schemaAuditActor(key.operation)
			if key.operation == "stale-apply" {
				actor.operation = "apply"
			}
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
