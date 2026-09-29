package e2e

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const mysqlAuditDatabase, mysqlAuditUser, mysqlAuditHost = "ptah_audit_history", "ptah_audit", "172.19.0.3"

func mysqlMigrationAuditReading(t *testing.T) []mysqlStatementRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "mysql-migration-history-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := mysqlStatementJournal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func mysqlAuditBaseline() []mysqlStatementRecord {
	return []mysqlStatementRecord{{Time: "2026-09-29T20:31:40.000000", Thread: 1,
		Client: "root[root] @ localhost [127.0.0.1]", Command: "Query", ArgumentHex: hex.EncodeToString([]byte("SELECT 1"))}}
}

func mysqlAuditClients() map[string]migrationSQLClient {
	return map[string]migrationSQLClient{mysqlAuditHost: {jobUID: "job", podUID: "pod", operation: "history"}}
}

func TestMySQLMigrationSQLAuditAcceptsActualHistory(t *testing.T) {
	t.Parallel()
	rows, before := mysqlMigrationAuditReading(t), mysqlAuditBaseline()
	if len(rows) != 34 {
		t.Fatalf("expected the complete 34-record reading, got %d", len(rows))
	}
	counts, err := mysqlMigrationRefusalSQL(before, append(slices.Clone(before), rows...), mysqlAuditDatabase, mysqlAuditUser, mysqlAuditClients())
	if err != nil || len(counts) != 1 || counts[mysqlAuditHost] != 14 {
		t.Fatalf("actual History: counts=%v error=%v", counts, err)
	}
	// Before this window an unrelated Pod can have used the same IP. Its old
	// SQL supplies no evidence for this unique account and must not block it.
	before[0].Client = "other[other] @  [" + mysqlAuditHost + "]"
	if _, err := mysqlMigrationRefusalSQL(before, append(slices.Clone(before), rows...), mysqlAuditDatabase, mysqlAuditUser, mysqlAuditClients()); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLMigrationSQLAuditRejectsChangedSQL(t *testing.T) {
	t.Parallel()
	rows, before := mysqlMigrationAuditReading(t), mysqlAuditBaseline()
	for name, statement := range map[string]string{
		"DDL":                 "ALTER TABLE widgets ADD COLUMN rogue TEXT",
		"write then rollback": "BEGIN; INSERT INTO widgets VALUES (1); ROLLBACK",
		"mutating function":   "SELECT mutating_function()",
		"mutating CTE":        "WITH source AS (SELECT 1) DELETE FROM widgets",
		"extra statement":     "SELECT VERSION(); DROP TABLE widgets",
		"comment suffix":      "SELECT VERSION() -- allowed\n; DROP TABLE widgets",
		"changed table":       strings.ReplaceAll(mysqlHistoryCreate, "schema_migrations", "widgets"),
		"changed default":     strings.ReplaceAll(mysqlHistoryCreate, "DEFAULT 1", "DEFAULT 0"),
		"changed engine":      strings.ReplaceAll(mysqlHistoryCreate, "InnoDB", "MyISAM"),
		"changed database":    "SHOW CREATE TABLE `other`.`schema_migrations`",
		"parameter injection": strings.ReplaceAll(mysqlHistoryEngine, "?", "'ptah_audit_history' OR 1=1"),
	} {
		t.Run(name, func(t *testing.T) {
			for _, command := range []string{"Query", "Prepare", "Execute"} {
				broken := slices.Clone(rows)
				broken[1].Command, broken[1].ArgumentHex = command, hex.EncodeToString([]byte(statement))
				if _, err := mysqlMigrationRefusalSQL(before, append(slices.Clone(before), broken...), mysqlAuditDatabase, mysqlAuditUser, mysqlAuditClients()); err == nil {
					t.Fatalf("accepted changed SQL as %s", command)
				}
			}
		})
	}
	for index, row := range rows {
		if row.Command != "Query" && row.Command != "Prepare" && row.Command != "Execute" {
			continue
		}
		statement, _ := hex.DecodeString(row.ArgumentHex)
		for _, other := range []string{"Query", "Prepare", "Execute"} {
			if other != row.Command && mysqlMigrationHistoryStatement(other, string(statement), mysqlAuditDatabase) {
				t.Fatalf("record %d also accepted in wrong protocol command %s", index, other)
			}
		}
		for _, from := range []string{mysqlAuditDatabase, "schema_migrations", "'state'"} {
			if !strings.Contains(string(statement), from) {
				continue
			}
			if mysqlMigrationHistoryStatement(row.Command, strings.ReplaceAll(string(statement), from, "unexpected"), mysqlAuditDatabase) {
				t.Fatalf("record %d accepted changed %s", index, from)
			}
		}
	}
}

func TestMySQLMigrationSQLAuditRequiresCompleteSessions(t *testing.T) {
	t.Parallel()
	rows, before := mysqlMigrationAuditReading(t), mysqlAuditBaseline()
	for name, mutate := range map[string]func([]mysqlStatementRecord) []mysqlStatementRecord{
		"missing connect": func(r []mysqlStatementRecord) []mysqlStatementRecord { return r[1:] },
		"missing quit":    func(r []mysqlStatementRecord) []mysqlStatementRecord { return r[:len(r)-1] },
		"missing initial SQL": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			return []mysqlStatementRecord{r[0], r[len(r)-1]}
		},
		"unknown host": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			for i := range r {
				r[i].Client = strings.ReplaceAll(r[i].Client, mysqlAuditHost, "172.19.0.4")
			}
			return r
		},
		"different account at known IP": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			for i := range r {
				r[i].Client = strings.ReplaceAll(r[i].Client, mysqlAuditUser, "other")
			}
			return r
		},
		"wrong selected database": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			r[0].ArgumentHex = hex.EncodeToString([]byte(mysqlAuditUser + "@" + mysqlAuditHost + " on other using TCP/IP"))
			return r
		},
		"changed session database": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			r[1].Command = "Init DB"
			r[1].ArgumentHex = hex.EncodeToString([]byte("other"))
			return r
		},
		"session ordering": func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1], r[2] = r[2], r[1]; return r },
		"ambiguous time":   func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1].Time = r[0].Time; return r },
		"wrong thread":     func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1].Thread++; return r },
		"unknown protocol": func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1].Command = "Change user"; return r },
		"control with SQL": func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1].Command = "Ping"; return r },
		"duplicate connect": func(r []mysqlStatementRecord) []mysqlStatementRecord {
			r[1].Command = "Connect"
			r[1].Client = r[0].Client
			r[1].ArgumentHex = r[0].ArgumentHex
			return r
		},
		"invalid argument": func(r []mysqlStatementRecord) []mysqlStatementRecord { r[1].ArgumentHex = "ff"; return r },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mysqlMigrationRefusalSQL(before, append(slices.Clone(before), mutate(slices.Clone(rows))...), mysqlAuditDatabase, mysqlAuditUser, mysqlAuditClients()); err == nil {
				t.Fatal("incomplete or unauthorized session passed")
			}
		})
	}
	for _, actor := range []migrationSQLClient{
		{jobUID: "job", podUID: "pod", operation: "apply"}, {podUID: "pod", operation: "history"}, {jobUID: "job", operation: "history"},
	} {
		if _, err := mysqlMigrationRefusalSQL(before, append(slices.Clone(before), rows...), mysqlAuditDatabase, mysqlAuditUser, map[string]migrationSQLClient{mysqlAuditHost: actor}); err == nil {
			t.Fatal("incomplete Job/Pod/operation binding passed")
		}
	}
	late := append(slices.Clone(before), rows[0])
	if _, err := mysqlMigrationRefusalSQL(late, append(slices.Clone(before), rows...), mysqlAuditDatabase, mysqlAuditUser, mysqlAuditClients()); err == nil {
		t.Fatal("window started after the account was used")
	}
}

func TestMySQLStatementJournalPreservesCompleteRecords(t *testing.T) {
	t.Parallel()
	rows := mysqlMigrationAuditReading(t)
	before := append(mysqlAuditBaseline(), rows[0], rows[0])
	after := append(slices.Clone(before), rows[1:]...)
	window, err := mysqlStatementWindow(before, after)
	if err != nil || !reflect.DeepEqual(window, rows[1:]) {
		t.Fatalf("complete multiset difference: %v", err)
	}
	for name, broken := range map[string][]mysqlStatementRecord{
		"lost duplicate": append(mysqlAuditBaseline(), rows...),
		"lost prior row": append(slices.Clone(before[1:]), rows[1:]...),
		"no advance":     before,
		"empty":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mysqlStatementWindow(before, broken); err == nil {
				t.Fatal("incomplete journal passed")
			}
		})
	}
	after[0].ArgumentHex = hex.EncodeToString([]byte("SELECT 2"))
	if _, err := mysqlStatementWindow(before, after); err == nil {
		t.Fatal("replaced SQL passed")
	}
	for _, raw := range []string{"", `{"time":`, `null`, `{}`, `{"time":"private-audit-password"}`} {
		if _, err := mysqlStatementJournal([]byte(raw)); err == nil || strings.Contains(err.Error(), "private-audit-password") {
			t.Fatal("incomplete journal passed or exposed its contents")
		}
	}
	// Empty control arguments are valid, but a missing field means the journal
	// was incompletely captured. They must not become indistinguishable.
	quit, err := json.Marshal(rows[len(rows)-1])
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range []string{
		strings.Replace(string(quit), `,"argumentHex":""`, "", 1),
		strings.Replace(string(quit), `"argumentHex":""`, `"argumentHex":null`, 1),
		strings.Replace(string(quit), `"argumentHex":""`, `"argumentHex":"ff"`, 1),
	} {
		if _, err := mysqlStatementJournal([]byte(broken)); err == nil {
			t.Fatal("missing or invalid control argument bytes passed")
		}
	}
	for _, field := range []string{"time", "thread", "client", "type", "argumentHex"} {
		encoded, err := json.Marshal(rows[1])
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(encoded, &row); err != nil {
			t.Fatal(err)
		}
		row[field] = "private-audit-password"
		if field == "thread" {
			row[field] = 0
		}
		if field == "client" || field == "type" {
			row[field] = ""
		}
		encoded, err = json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mysqlStatementJournal(encoded); err == nil || strings.Contains(err.Error(), "private-audit-password") {
			t.Fatalf("invalid %s passed or exposed data", field)
		}
	}
}
