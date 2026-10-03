package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLAuditReadsTheDatabaseServerReadings(t *testing.T) {
	pg, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "postgresql-sql-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts, _, err := postgresAuditCounts(pg, "unused")
	if err != nil || counts.clients["172.17.0.20"] != 1 || counts.records != 1 {
		t.Fatalf("PostgreSQL 17 reading: %+v %v", counts, err)
	}
	mysql, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "mysql-sql-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts, err = mysqlAuditCounts(mysql)
	if err != nil || counts.clients["172.17.0.19"] != 3 || counts.clients["127.0.0.1"] != 8 || counts.records != 8923 {
		t.Fatalf("MySQL 8.4 reading: %+v %v", counts, err)
	}
}

func TestPostgresAuditCountsEveryReceivedSQLRecord(t *testing.T) {
	// The first record uses PostgreSQL 17's actual JSON field names. Other
	// rows exercise extended-protocol execution and statements that failed
	// before Execute, which log_statement=all alone does not record.
	raw := []byte(`{"remote_host":"10.244.1.7","message":"statement: SELECT 42"}
{"remote_host":"10.244.1.7","message":"execute <unnamed>: SELECT $1"}
{"remote_host":"10.244.1.7","message":"execute named: WITH gone AS (DELETE FROM widgets RETURNING *) SELECT * FROM gone"}
{"remote_host":"10.244.1.7","message":"syntax error","statement":"BROKEN SQL"}
{"remote_host":"10.244.1.7","message":"statement: SELECT mutating_function()"}
{"remote_host":"10.244.1.8","message":"statement: ALTER TABLE widgets ADD COLUMN name text"}
{"remote_host":"127.0.0.1","message":"statement: SELECT 'ptah_e2e_audit_1'"}
{"message":"checkpoint complete"}
`)
	counts, marker, err := postgresAuditCounts(raw, "ptah_e2e_audit_1")
	if err != nil || !marker || counts.clients["10.244.1.7"] != 5 || counts.clients["10.244.1.8"] != 1 || counts.records != 7 {
		t.Fatalf("unexpected audit counts: %+v marker=%t error=%v", counts, marker, err)
	}
	if _, found, err := postgresAuditCounts(raw, "another_marker"); err != nil || found {
		t.Fatalf("a different collector marker passed: found=%t error=%v", found, err)
	}
	for name, broken := range map[string]string{
		"empty": "", "truncated": string(raw) + `{"message":`,
		"hostname": `{"remote_host":"runner.example","message":"statement: SELECT 42"}`,
		"no SQL":   `{"message":"checkpoint complete"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := postgresAuditCounts([]byte(broken), "ptah_e2e_audit_1"); err == nil {
				t.Fatal("incomplete audit passed")
			}
		})
	}
}

func TestMySQLAuditUsesTheClientAddressAndAddsItsSessions(t *testing.T) {
	raw := []byte(`{"client":"ptah_e2e[ptah_e2e] @  [10.244.1.7]","count":4}
{"client":"second[second] @ worker.example [10.244.1.7]","count":2}
{"client":"ptah_e2e[ptah_e2e] @ worker.example [10.244.1.8]","count":1}
{"client":"root[root] @ localhost []","count":3}
`)
	counts, err := mysqlAuditCounts(raw)
	if err != nil || counts.clients["10.244.1.7"] != 6 || counts.clients["10.244.1.8"] != 1 || counts.records != 10 {
		t.Fatalf("unexpected audit counts: %+v error=%v", counts, err)
	}
	for _, broken := range []string{"", `{}`, `{"client":"user [bad]","count":1}`, `{"client":"user [10.244.1.7]","count":0}`,
		`{"client":"user [10.244.1.7]","count":-1}`, string(raw) + `{"client":`} {
		if _, err := mysqlAuditCounts([]byte(broken)); err == nil {
			t.Fatal("invalid audit passed")
		}
	}
}

func TestSQLAuditDeltaRejectsMissingOrLostEvidence(t *testing.T) {
	before := sqlAuditCounts{clients: map[string]int64{"10.244.1.7": 3, "10.244.1.8": 5}, records: 8}
	after := sqlAuditCounts{clients: map[string]int64{"10.244.1.7": 3, "10.244.1.8": 8}, records: 11}
	if count, err := sqlAuditDelta(before, after, "10.244.1.7"); err != nil || count != 0 {
		t.Fatalf("zero SQL was not accepted: %d %v", count, err)
	}
	if count, err := sqlAuditDelta(before, after, "10.244.1.8"); err != nil || count != 3 {
		t.Fatalf("positive SQL was not counted: %d %v", count, err)
	}
	for _, sample := range []struct {
		before, after sqlAuditCounts
		ip            string
	}{
		{before, after, ""}, {sqlAuditCounts{}, after, "10.244.1.7"}, {before, sqlAuditCounts{}, "10.244.1.7"},
		{after, before, "10.244.1.7"},
		{before, sqlAuditCounts{clients: map[string]int64{"10.244.1.7": 2, "10.244.1.8": 20}, records: 22}, "10.244.1.8"},
	} {
		if _, err := sqlAuditDelta(sample.before, sample.after, sample.ip); err == nil {
			t.Fatal("incomplete or rolled-back audit passed")
		}
	}
}

func TestAuditParserErrorsDoNotQuoteSQL(t *testing.T) {
	const secret = "private-fixture-password"
	_, _, pgErr := postgresAuditCounts([]byte(`{"message":"`+secret), "marker")
	_, mysqlErr := mysqlAuditCounts([]byte(`{"client":"` + secret))
	for _, err := range []error{pgErr, mysqlErr} {
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("audit parser did not safely reject malformed data: %v", err)
		}
	}
}

func TestPostgresAuditPrefixDiagnosticsRetainTheRefusalWithoutSQL(t *testing.T) {
	const secret = "private-fixture-password"
	before := []byte(`{"message":"statement: SELECT '` + secret + `'"}` + "\n")
	for name, after := range map[string][]byte{
		"identical": append([]byte(nil), before...),
		"append":    append(append([]byte(nil), before...), []byte(`{"message":"checkpoint complete"}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := postgresAuditPrefixError(before, after); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := postgresAuditPrefixError(nil, before); err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string]struct {
		after  []byte
		offset string
	}{
		"empty read":              {nil, "firstMismatchOffset=0"},
		"short read":              {before[:7], "firstMismatchOffset=7"},
		"replaced first byte":     {append([]byte("X"), before[1:]...), "firstMismatchOffset=0"},
		"changed middle":          {append(append(append([]byte(nil), before[:12]...), 'X'), before[13:]...), "firstMismatchOffset=12"},
		"replacement then growth": {append([]byte("X"), before...), "firstMismatchOffset=0"},
	} {
		t.Run(name, func(t *testing.T) {
			err := postgresAuditPrefixError(before, row.after)
			if err == nil {
				t.Fatal("lost journal evidence was accepted")
			}
			message := err.Error()
			for _, required := range []string{row.offset, "previousBytes=", "currentBytes=", "previousSHA256=", "currentPrefixSHA256="} {
				if !strings.Contains(message, required) {
					t.Fatalf("missing safe diagnostic %s", required)
				}
			}
			for _, forbidden := range []string{secret, "SELECT", "statement:", string(before)} {
				if strings.Contains(message, forbidden) {
					t.Fatal("SQL or a fixture credential reached the diagnostic")
				}
			}
		})
	}
}

func TestSQLAuditEvidenceRetainsNativeCounts(t *testing.T) {
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", engine+"-sql-audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var counts sqlAuditCounts
			if engine == "postgresql" {
				counts, _, err = postgresAuditCounts(raw, "unused")
			} else {
				counts, err = mysqlAuditCounts(raw)
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"sqlBefore": counts})
			if err != nil {
				t.Fatal(err)
			}
			var proof struct {
				Before struct {
					Clients map[string]int64 `json:"clients"`
					Records *int64           `json:"records"`
				} `json:"sqlBefore"`
			}
			if err := json.Unmarshal(body, &proof); err != nil {
				t.Fatal(err)
			}
			if proof.Before.Records == nil || *proof.Before.Records != counts.records || len(proof.Before.Clients) != len(counts.clients) || len(proof.Before.Clients) == 0 {
				t.Fatalf("native audit counts disappeared from retained evidence: %s", body)
			}
			for client, want := range counts.clients {
				if got, ok := proof.Before.Clients[client]; !ok || got != want {
					t.Fatalf("client %s lost its exact count", client)
				}
			}
		})
	}
}
