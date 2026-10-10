package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const restoreAuditDatabase = "ptah_audit_restore"

func restoreAuditReading(t *testing.T, engine string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", engine+"-restored-history-refusal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func restoreAuditClients() map[string]operationSQLClient {
	return map[string]operationSQLClient{mysqlAuditHost: {jobUID: "refused-job", podUID: "refused-pod", operation: "apply"}}
}

func TestPostgresRestoredHistorySQLAuditUsesTheExactRefusedApply(t *testing.T) {
	t.Parallel()
	raw := restoreAuditReading(t, "postgresql")
	counts, err := postgresMigrationRefusalSQLForJob(raw, restoreAuditDatabase, restoreAuditClients(), "refused-job")
	if err != nil || len(counts) != 1 || counts[mysqlAuditHost] != 25 {
		t.Fatalf("actual refused Apply: counts=%v error=%v", counts, err)
	}
	for _, uid := range []string{"", "another-job"} {
		if _, err := postgresMigrationRefusalSQLForJob(raw, restoreAuditDatabase, restoreAuditClients(), uid); err == nil {
			t.Fatal("coordination SQL accepted without its exact refused Apply")
		}
	}
	clients := restoreAuditClients()
	clients[mysqlAuditHost] = operationSQLClient{jobUID: "refused-job", podUID: "pod", operation: "history"}
	if _, err := postgresMigrationRefusalSQLForJob(raw, restoreAuditDatabase, clients, "refused-job"); err == nil {
		t.Fatal("History inherited the Apply's coordination exception")
	}
	// The later History uses the narrower diagnostic contract in the same
	// refusal window. Its SQL cannot supply the refused Apply's lock evidence.
	clients = restoreAuditClients()
	clients["10.244.3.97"] = operationSQLClient{jobUID: "history", podUID: "history-pod", operation: "history"}
	history := bytes.ReplaceAll(postgresMigrationAuditReading(t), []byte("ptah_e2e_retarget"), []byte(restoreAuditDatabase))
	counts, err = postgresMigrationRefusalSQLForJob(append(bytes.Clone(raw), history...), restoreAuditDatabase, clients, "refused-job")
	if err != nil || counts[mysqlAuditHost] != 25 || counts["10.244.3.97"] != 9 {
		t.Fatalf("refused Apply plus later History: %v %v", counts, err)
	}
	if _, err := postgresMigrationRefusalSQLForJob(history, restoreAuditDatabase, clients, "refused-job"); err == nil {
		t.Fatal("History alone replaced the refused Apply evidence")
	}
}

func TestPostgresRestoredHistorySQLAuditRejectsLostOrChangedEvidence(t *testing.T) {
	t.Parallel()
	raw := restoreAuditReading(t, "postgresql")
	for name, replacement := range map[string][2]string{
		"wrong lock":                   {"2705505214", "2705505215"},
		"wrong revision state":         {"Parameters: $1 = 'applied'", "Parameters: $1 = 'failed'"},
		"different revision predicate": {"applied <> total", "applied = total"},
		"different limit":              {"LIMIT 1", "LIMIT 2"},
		"write function":               {"SELECT pg_try_advisory_lock($1)", "SELECT mutating_function($1)"},
		"wrong client":                 {mysqlAuditHost, "172.19.0.4"},
	} {
		t.Run(name, func(t *testing.T) {
			broken := bytes.ReplaceAll(raw, []byte(replacement[0]), []byte(replacement[1]))
			if bytes.Equal(raw, broken) {
				t.Fatal("adverse case changed no records")
			}
			if _, err := postgresMigrationRefusalSQLForJob(broken, restoreAuditDatabase, restoreAuditClients(), "refused-job"); err == nil {
				t.Fatal("changed refusal SQL passed")
			}
		})
	}
	for _, required := range []string{"SELECT pg_try_advisory_lock", "SELECT pg_advisory_unlock", "WHERE state <>"} {
		var kept []byte
		removed := 0
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if bytes.Contains(line, []byte(required)) {
				removed++
				continue
			}
			kept = append(kept, line...)
			kept = append(kept, '\n')
		}
		if removed == 0 {
			t.Fatal("required diagnostic record not found")
		}
		if _, err := postgresMigrationRefusalSQLForJob(kept, restoreAuditDatabase, restoreAuditClients(), "refused-job"); err == nil {
			t.Fatalf("missing %s passed", required)
		}
	}
	for _, sql := range []string{"DELETE FROM schema_migrations", "BEGIN; INSERT INTO widgets VALUES (1); ROLLBACK", "SELECT 'private-audit-password'", "WITH gone AS (DELETE FROM widgets RETURNING *) SELECT * FROM gone"} {
		row, err := json.Marshal(map[string]string{"dbname": restoreAuditDatabase, "remote_host": mysqlAuditHost, "statement": sql})
		if err != nil {
			t.Fatal(err)
		}
		_, err = postgresMigrationRefusalSQLForJob(append(bytes.Clone(raw), row...), restoreAuditDatabase, restoreAuditClients(), "refused-job")
		if err == nil || strings.Contains(err.Error(), "private-audit-password") {
			t.Fatal("unauthorized SQL passed or escaped into an error")
		}
	}
}

func TestMySQLRestoredHistorySQLAuditUsesTheExactRefusedApply(t *testing.T) {
	t.Parallel()
	rows, err := mysqlStatementJournal(restoreAuditReading(t, "mysql"))
	if err != nil {
		t.Fatal(err)
	}
	// The account was already used by the initial successful run. Those
	// retained records must survive unchanged but are outside this window.
	before := append(mysqlAuditBaseline(), mysqlMigrationAuditReading(t)...)
	after := append(slices.Clone(before), rows...)
	counts, err := mysqlMigrationRefusalSQLForJob(before, after, restoreAuditDatabase, mysqlAuditUser, restoreAuditClients(), "refused-job")
	if err != nil || len(rows) != 63 || len(counts) != 1 || counts[mysqlAuditHost] != 28 {
		t.Fatalf("actual refused Apply: counts=%v error=%v", counts, err)
	}
	for _, uid := range []string{"", "another-job"} {
		if _, err := mysqlMigrationRefusalSQLForJob(before, after, restoreAuditDatabase, mysqlAuditUser, restoreAuditClients(), uid); err == nil {
			t.Fatal("coordination SQL accepted without its exact refused Apply")
		}
	}
	clients := restoreAuditClients()
	clients[mysqlAuditHost] = operationSQLClient{jobUID: "refused-job", podUID: "pod", operation: "history"}
	if _, err := mysqlMigrationRefusalSQLForJob(before, after, restoreAuditDatabase, mysqlAuditUser, clients, "refused-job"); err == nil {
		t.Fatal("History inherited the Apply's coordination exception")
	}
}

func TestMySQLRestoredHistorySQLAuditRejectsLostOrChangedEvidence(t *testing.T) {
	t.Parallel()
	rows, err := mysqlStatementJournal(restoreAuditReading(t, "mysql"))
	if err != nil {
		t.Fatal(err)
	}
	before := mysqlAuditBaseline()
	for name, replacement := range map[string][2]string{
		"wrong lock":           {"ptah_migrate", "another_lock"},
		"wrong lock timeout":   {"'ptah_migrate', 30", "'ptah_migrate', 31"},
		"wrong revision state": {"state <> 'applied'", "state <> 'failed'"},
		"another database":     {restoreAuditDatabase, "another_database"},
		"different predicate":  {"applied <> total", "applied = total"},
		"mutating function":    {"SELECT GET_LOCK", "SELECT mutating_function"},
		"extra SQL":            {"SELECT RELEASE_LOCK('ptah_migrate')", "SELECT RELEASE_LOCK('ptah_migrate'); DROP TABLE widgets"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := slices.Clone(rows)
			mutations := 0
			for i, row := range changed {
				sql, err := hex.DecodeString(row.ArgumentHex)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(sql), replacement[0]) {
					continue
				}
				changed[i].ArgumentHex = hex.EncodeToString([]byte(strings.ReplaceAll(string(sql), replacement[0], replacement[1])))
				mutations++
			}
			if mutations == 0 {
				t.Fatal("adverse case changed no records")
			}
			if _, err := mysqlMigrationRefusalSQLForJob(before, append(slices.Clone(before), changed...), restoreAuditDatabase, mysqlAuditUser, restoreAuditClients(), "refused-job"); err == nil {
				t.Fatal("changed refusal SQL passed")
			}
		})
	}
	for _, required := range []string{"SELECT GET_LOCK", "SELECT RELEASE_LOCK", "WHERE state <>"} {
		removed := 0
		var kept []mysqlStatementRecord
		for _, row := range rows {
			sql, err := hex.DecodeString(row.ArgumentHex)
			if err != nil {
				t.Fatal(err)
			}
			if row.Command == "Execute" && strings.Contains(string(sql), required) {
				removed++
				continue
			}
			kept = append(kept, row)
		}
		if removed == 0 {
			t.Fatal("required execution record not found")
		}
		if _, err := mysqlMigrationRefusalSQLForJob(before, append(slices.Clone(before), kept...), restoreAuditDatabase, mysqlAuditUser, restoreAuditClients(), "refused-job"); err == nil {
			t.Fatalf("Prepare without executed %s passed", required)
		}
	}
}

func TestPostgresRestoredHistoryAuditBoundsHarnessReads(t *testing.T) {
	t.Parallel()
	raw := restoreAuditReading(t, "postgresql")
	for _, sql := range []string{restoreRevisionsQuery("postgresql"), "SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'e2e_migration_widgets' AND column_name = 'color'"} {
		row, err := json.Marshal(map[string]string{"dbname": restoreAuditDatabase, "remote_host": "127.0.0.1", "message": "statement: " + sql})
		if err != nil {
			t.Fatal(err)
		}
		counts, err := postgresMigrationRefusalSQLForJob(append(bytes.Clone(raw), row...), restoreAuditDatabase, restoreAuditClients(), "refused-job")
		if err != nil || counts["127.0.0.1"] != 1 {
			t.Fatalf("allowed harness read: %v %v", counts, err)
		}
		if postgresMigrationHarnessRead(sql, "") {
			t.Fatal("restore-only diagnostic widened the earlier approval-input audit")
		}
		row = bytes.ReplaceAll(row, []byte("127.0.0.1"), []byte(mysqlAuditHost))
		if _, err := postgresMigrationRefusalSQLForJob(append(bytes.Clone(raw), row...), restoreAuditDatabase, restoreAuditClients(), "refused-job"); err == nil {
			t.Fatal("runner inherited the harness's diagnostic exception")
		}
	}
}
