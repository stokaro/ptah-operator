package e2e

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestSchemaDiagnosticResultAfterPlanFailure(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../testdata/e2e/readings/repeated-observe-after-plan-failure.json")
	if err != nil {
		t.Fatal(err)
	}
	var reading struct {
		Jobs []struct {
			Name, UID, Created, InputFingerprint string
		}
	}
	if err := json.Unmarshal(data, &reading); err != nil {
		t.Fatal(err)
	}
	if len(reading.Jobs) != 2 {
		t.Fatal("the captured retry must contain both successful Observe Jobs")
	}
	_, schema, _, _ := schemaReplacementFixture()
	inventory := &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}}
	var records []observedJob
	for _, row := range reading.Jobs {
		created, err := time.Parse(time.RFC3339, row.Created)
		if err != nil {
			t.Fatal(err)
		}
		job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: row.Name, Namespace: schema.Namespace, UID: types.UID(row.UID),
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{labelSchema: schema.Name, labelOperation: "observe"},
			Annotations:       map[string]string{"operator.ptah.run/input-fingerprint": row.InputFingerprint},
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchema", Name: schema.Name, UID: schema.UID, Controller: ptr.To(true)}}},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
		inventory.jobs[job.UID] = job
		records = append(records, observedJob{UID: row.UID, Name: row.Name, Created: row.Created, Schema: schema.Name, Operation: "observe"})
	}
	// A failed Plan caused the controller to observe again. List order is not
	// execution order; select the later success and retain both SQL identities.
	for _, order := range [][]observedJob{records, {records[1], records[0]}} {
		selected, err := inventory.diagnosticResult(schema, "observe", order)
		if err != nil || selected != records[0] || len(inventory.jobs) != 2 {
			t.Fatalf("repeated Observe lost its latest result or SQL history: selected=%+v, error=%v", selected, err)
		}
	}
}

func TestSchemaDiagnosticResultAfterFailedAttempt(t *testing.T) {
	t.Parallel()
	_, schema, _, _ := schemaReplacementFixture()
	for _, operation := range []string{"observe", "plan"} {
		t.Run(operation, func(t *testing.T) {
			failed := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: schema.Namespace, UID: "failed",
				Labels:          map[string]string{labelSchema: schema.Name, labelOperation: operation},
				Annotations:     map[string]string{"operator.ptah.run/input-fingerprint": "sha256:" + strings.Repeat("a", 64)},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchema", Name: schema.Name, UID: schema.UID, Controller: ptr.To(true)}}},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
			success := failed.DeepCopy()
			success.Name, success.UID = "success", "success"
			success.Status.Conditions[0].Type = batchv1.JobComplete
			records := []observedJob{
				{UID: "failed", Name: "failed", Schema: schema.Name, Operation: operation},
				{UID: "success", Name: "success", Schema: schema.Name, Operation: operation},
			}
			inventory := &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{failed.UID: failed, success.UID: *success}}
			for _, order := range [][]observedJob{records, {records[1], records[0]}} {
				selected, err := inventory.diagnosticResult(schema, operation, order)
				if err != nil || selected != records[1] || len(inventory.jobs) != 2 {
					t.Fatalf("successful retry lost or failed attempt discarded: selected=%+v, error=%v", selected, err)
				}
			}
			if selected, err := inventory.diagnosticResult(schema, operation, records[1:]); err != nil || selected != records[1] {
				t.Fatalf("ordinary diagnostic failed: selected=%+v, error=%v", selected, err)
			}
			for name, change := range map[string]func(*batchv1.Job){
				"two successful attempts": func(job *batchv1.Job) { job.Status.Conditions[0].Type = batchv1.JobComplete },
				"unfinished attempt":      func(job *batchv1.Job) { job.Status.Conditions = nil },
				"conflicting outcome": func(job *batchv1.Job) {
					job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
				},
				"different inputs": func(job *batchv1.Job) {
					job.Annotations["operator.ptah.run/input-fingerprint"] = "sha256:" + strings.Repeat("b", 64)
				},
				"missing inputs":    func(job *batchv1.Job) { job.Annotations = nil },
				"foreign owner":     func(job *batchv1.Job) { job.OwnerReferences[0].UID = "other" },
				"foreign namespace": func(job *batchv1.Job) { job.Namespace = "other" },
				"replaced Job":      func(job *batchv1.Job) { job.UID = "replacement" },
			} {
				t.Run(name, func(t *testing.T) {
					changed := failed.DeepCopy()
					change(changed)
					inventory := &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{failed.UID: *changed, success.UID: *success}}
					if _, err := inventory.diagnosticResult(schema, operation, records); err == nil {
						t.Fatal("invalid attempt accepted")
					}
				})
			}
			for _, subset := range [][]observedJob{nil, records[:1], {records[1], records[1]}} {
				if _, err := inventory.diagnosticResult(schema, operation, subset); err == nil {
					t.Fatal("missing or duplicate result accepted")
				}
			}
			if _, err := inventory.diagnosticResult(schema, "apply", records); err == nil {
				t.Fatal("Apply used diagnostic retry selection")
			}
			delete(inventory.jobs, failed.UID)
			if _, err := inventory.diagnosticResult(schema, operation, records); err == nil {
				t.Fatal("a missing attempt was silently ignored")
			}
		})
	}
}

func TestSchemaFailedObserveKeepsSQLAuditAndSuccessfulControl(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			policy, err := newSchemaSQLPolicy(engine, schemaAuditDatabase)
			if err != nil {
				t.Fatal(err)
			}
			success := schemaAuditActor("observe")
			failed := success
			failed.jobUID, failed.podUID = "failed-job", "failed-pod"
			clients := map[string]operationSQLClient{mysqlAuditHost: success, "10.0.0.2": failed}
			raw := schemaAuditReading(t, engine, "observe")
			failedRaw := []byte(strings.ReplaceAll(string(raw), mysqlAuditHost, "10.0.0.2"))
			check := func(journal []byte) (map[string]int, error) {
				if engine == "postgresql" {
					return postgresStatementRefusalSQL(journal, schemaAuditDatabase, clients, schemaDiagnosticActor, policy.postgres, nil)
				}
				rows, err := mysqlStatementJournal(journal)
				if err != nil {
					return nil, err
				}
				// The second Pod has its own MySQL connections; the copied
				// native journal must not reuse the first Pod's thread IDs.
				for index := range rows {
					if strings.Contains(rows[index].Client, "[10.0.0.2]") {
						rows[index].Thread += 1000
					}
				}
				before := mysqlAuditBaseline()
				return mysqlStatementRefusalSQL(before, append(before, rows...), schemaAuditDatabase, mysqlAuditUser, clients, true, schemaDiagnosticActor, policy.mysql)
			}
			counts, err := check(append(slices.Clone(failedRaw), raw...))
			if err != nil || counts["10.0.0.2"] == 0 || counts[mysqlAuditHost] == 0 {
				t.Fatalf("both diagnostic attempts must stay in the audit: counts=%v, error=%v", counts, err)
			}
			if err := schemaRequiredSQLControls(clients, counts, []operationSQLClient{success}); err != nil {
				t.Fatal(err)
			}
			delete(counts, mysqlAuditHost)
			if schemaRequiredSQLControls(clients, counts, []operationSQLClient{success}) == nil {
				t.Fatal("failed diagnostic SQL substituted for the successful result control")
			}
			mutation := schemaAuditReading(t, engine, "drift-apply-current")
			mutation = []byte(strings.ReplaceAll(string(mutation), mysqlAuditHost, "10.0.0.2"))
			if _, err := check(append(slices.Clone(raw), mutation...)); err == nil {
				t.Fatal("failed Observe was allowed to mutate the database")
			}
		})
	}
}
