package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func postgresMigrationAuditReading(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "postgresql-migration-history-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPostgresMigrationSQLAuditAcceptsOnlyTheHistoryContract(t *testing.T) {
	t.Parallel()
	raw := postgresMigrationAuditReading(t)
	const database, host = "ptah_e2e_retarget", "10.244.3.97"
	clients := map[string]migrationSQLClient{host: {jobUID: "job", podUID: "pod", operation: "history"}}
	counts, err := postgresMigrationRefusalSQL(raw, database, clients)
	if err != nil || counts[host] != 17 || len(counts) != 1 {
		t.Fatalf("actual History reading: counts=%v error=%v", counts, err)
	}
	for name, statement := range map[string]string{
		"DDL":                 `ALTER TABLE widgets ADD COLUMN rogue text`,
		"write then rollback": `BEGIN; INSERT INTO widgets VALUES (1); ROLLBACK`,
		"read function":       `SELECT mutating_function()`,
		"write CTE":           `WITH gone AS (DELETE FROM widgets RETURNING *) SELECT * FROM gone`,
		"second statement":    "SELECT version(); DROP TABLE widgets",
		"comment suffix":      "-- ping\nDROP TABLE widgets",
		"changed table":       strings.ReplaceAll(postgresHistoryCreate, "schema_migrations", "widgets"),
		"changed default":     strings.ReplaceAll(postgresHistoryCreate, "DEFAULT 1", "DEFAULT mutating_function()"),
	} {
		t.Run(name, func(t *testing.T) {
			for _, field := range []string{"message", "statement"} {
				row := map[string]string{"dbname": database, "remote_host": host, field: statement}
				if field == "message" {
					row[field] = "statement: " + statement
				}
				encoded, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := postgresMigrationRefusalSQL(append(append([]byte{}, raw...), encoded...), database, clients); err == nil {
					t.Fatalf("accepted unauthorized SQL in %s", field)
				}
			}
		})
	}
	for name, replacement := range map[string]string{
		"another schema": "Parameters: $1 = 'other', $2 = 'schema_migrations'",
		"another table":  "Parameters: $1 = '', $2 = 'widgets'",
		"missing params": "",
		"extra params":   "Parameters: $1 = '', $2 = 'schema_migrations', $3 = 'rogue'",
	} {
		t.Run(name, func(t *testing.T) {
			row, err := json.Marshal(map[string]string{
				"dbname": database, "remote_host": host, "message": "execute <unnamed>: " + postgresHistoryOwner,
				"detail": replacement,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := postgresMigrationRefusalSQL(append(append([]byte{}, raw...), row...), database, clients); err == nil {
				t.Fatal("accepted changed extended-protocol parameters")
			}
		})
	}
}

func TestPostgresMigrationSQLAuditRequiresCompleteAttribution(t *testing.T) {
	t.Parallel()
	raw := postgresMigrationAuditReading(t)
	const database, host = "ptah_e2e_retarget", "10.244.3.97"
	clients := map[string]migrationSQLClient{host: {jobUID: "job", podUID: "pod", operation: "history"}}
	for name, broken := range map[string][]byte{
		"empty":                  nil,
		"truncated":              append(append([]byte{}, raw...), []byte(`{"message":`)...),
		"missing database":       []byte(strings.ReplaceAll(string(raw), `"dbname": "`+database+`"`, `"dbname": ""`)),
		"wrong database":         []byte(strings.ReplaceAll(string(raw), database, "other")),
		"unknown client":         []byte(strings.ReplaceAll(string(raw), host, "10.244.3.98")),
		"client hostname":        []byte(strings.ReplaceAll(string(raw), host, "worker.test")),
		"no SQL text":            []byte(`{"dbname":"ptah_e2e_retarget","remote_host":"10.244.3.97","message":"statement: "}`),
		"no executed SQL":        []byte(`{"dbname":"ptah_e2e_retarget","remote_host":"10.244.3.97","message":"execute <unnamed>"}`),
		"conflicting statements": []byte(`{"dbname":"ptah_e2e_retarget","remote_host":"10.244.3.97","message":"statement: SELECT version()","statement":"DROP TABLE widgets"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := postgresMigrationRefusalSQL(broken, database, clients); err == nil {
				t.Fatal("incomplete or unrelated audit passed")
			}
		})
	}
	for _, actor := range []migrationSQLClient{
		{jobUID: "job", podUID: "pod", operation: "apply"},
		{jobUID: "job", podUID: "pod", operation: "verify"},
		{jobUID: "", podUID: "pod", operation: "history"},
		{jobUID: "job", podUID: "", operation: "history"},
	} {
		if _, err := postgresMigrationRefusalSQL(raw, database, map[string]migrationSQLClient{host: actor}); err == nil {
			t.Fatal("unattributed or unauthorized operation SQL passed")
		}
	}
	if _, err := postgresMigrationRefusalSQL(raw, "", clients); err == nil {
		t.Fatal("missing database scope passed")
	}
	if _, err := postgresMigrationRefusalSQL(raw, database, nil); err == nil {
		t.Fatal("missing client inventory passed")
	}
	const secret = "private-audit-password"
	_, err := postgresMigrationRefusalSQL(append(append([]byte{}, raw...), []byte(`{"statement":"`+secret)...), database, clients)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("malformed SQL exposed credential-bearing journal contents")
	}
}

func TestPostgresMigrationSQLAuditBoundsHarnessReads(t *testing.T) {
	t.Parallel()
	raw := postgresMigrationAuditReading(t)
	clients := map[string]migrationSQLClient{"10.244.3.97": {jobUID: "job", podUID: "pod", operation: "history"}}
	const row = `{"dbname":"ptah_e2e_retarget","remote_host":"127.0.0.1","message":"statement: SELECT count(*) FROM schema_migrations"}`
	counts, err := postgresMigrationRefusalSQL(append(append([]byte{}, raw...), row...), "ptah_e2e_retarget", clients)
	if err != nil || counts["127.0.0.1"] != 1 || counts["10.244.3.97"] != 17 {
		t.Fatalf("harness diagnostic control: %v %v", counts, err)
	}
	for _, bad := range []string{
		strings.ReplaceAll(row, "SELECT count(*) FROM schema_migrations", "DELETE FROM schema_migrations"),
		strings.ReplaceAll(row, "SELECT count(*) FROM schema_migrations", "SELECT mutating_function()"),
		strings.ReplaceAll(row, "127.0.0.1", "10.244.3.97"),
	} {
		if _, err := postgresMigrationRefusalSQL(append(append([]byte{}, raw...), bad...), "ptah_e2e_retarget", clients); err == nil {
			t.Fatal("a loopback exception authorized mutation or a runner diagnostic")
		}
	}
	if _, err := postgresMigrationRefusalSQL([]byte(row), "ptah_e2e_retarget", clients); err == nil {
		t.Fatal("harness reads replaced the required History control")
	}
}

func TestMigrationSQLClientsRequireTheExactOwnershipChain(t *testing.T) {
	t.Parallel()
	migration := &ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: "test", UID: "resource-uid"}}
	owner := func(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}
	}
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "history", Namespace: "test", UID: "job-uid",
		Labels:          map[string]string{labelMigration: "migration", labelOperation: "history"},
		OwnerReferences: []metav1.OwnerReference{owner(ptahSchemaAPIVersion, "PtahMigration", "migration", "resource-uid")}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "history-pod", Namespace: "test", UID: "pod-uid",
		Labels:          map[string]string{labelMigration: "migration", labelOperation: "history"},
		OwnerReferences: []metav1.OwnerReference{owner("batch/v1", "Job", "history", "job-uid")}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, PodIP: "10.244.3.97"}}
	clients, err := migrationSQLClients(migration, []batchv1.Job{job}, []corev1.Pod{pod})
	if err != nil || clients["10.244.3.97"] != (migrationSQLClient{jobUID: "job-uid", podUID: "pod-uid", operation: "history", migrationUID: "resource-uid"}) {
		t.Fatalf("valid ownership chain: %v %v", clients, err)
	}
	for name, mutate := range map[string]func(*batchv1.Job, *corev1.Pod){
		"another migration UID":  func(j *batchv1.Job, _ *corev1.Pod) { j.OwnerReferences[0].UID = "other" },
		"another migration name": func(j *batchv1.Job, _ *corev1.Pod) { j.OwnerReferences[0].Name = "other" },
		"another namespace":      func(j *batchv1.Job, _ *corev1.Pod) { j.Namespace = "other" },
		"another Job UID":        func(_ *batchv1.Job, p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"another Pod family":     func(_ *batchv1.Job, p *corev1.Pod) { p.Labels[labelMigration] = "other" },
		"missing Pod UID":        func(_ *batchv1.Job, p *corev1.Pod) { p.UID = "" },
		"wrong operation":        func(_ *batchv1.Job, p *corev1.Pod) { p.Labels[labelOperation] = "apply" },
		"missing operation": func(j *batchv1.Job, p *corev1.Pod) {
			delete(j.Labels, labelOperation)
			delete(p.Labels, labelOperation)
		},
		"running Pod":      func(_ *batchv1.Job, p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"missing address":  func(_ *batchv1.Job, p *corev1.Pod) { p.Status.PodIP = "" },
		"hostname address": func(_ *batchv1.Job, p *corev1.Pod) { p.Status.PodIP = "worker.test" },
	} {
		t.Run(name, func(t *testing.T) {
			j, p := job.DeepCopy(), pod.DeepCopy()
			mutate(j, p)
			if _, err := migrationSQLClients(migration, []batchv1.Job{*j}, []corev1.Pod{*p}); err == nil {
				t.Fatal("unrelated or incomplete client identity passed")
			}
		})
	}
	if _, err := migrationSQLClients(migration, []batchv1.Job{job}, []corev1.Pod{pod, pod}); err == nil {
		t.Fatal("reused Pod IP passed")
	}
	if _, err := migrationSQLClients(migration, nil, []corev1.Pod{pod}); err == nil {
		t.Fatal("missing Job inventory passed")
	}
}
