package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

// policyRunJournal is the journal of a migration apply: Always ran, as the
// isolated-node row read it.
func policyRunJournal(t *testing.T) *ptahv1alpha1.LifecycleJournal {
	t.Helper()
	content, err := os.ReadFile("../../testdata/e2e/readings/isolated-node-durable-migration.json")
	if err != nil {
		t.Fatal(err)
	}
	var migration ptahv1alpha1.PtahMigration
	if err := json.Unmarshal(content, &migration); err != nil {
		t.Fatal(err)
	}
	return migration.Status.Journal
}

// approvedSchemaApplyJournal is the journal entry the operator writes when an
// approved schema Apply settles, produced by the journal code itself from the
// status write that ends the claim, since no reading predates the entry's
// operation ID.
func approvedSchemaApplyJournal(t *testing.T, approvalUID types.UID, operationID string) *ptahv1alpha1.LifecycleJournal {
	t.Helper()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	plan := ptahv1alpha1.CurrentPlanStatus{Name: "ptah-plan-a", UID: "plan-uid", Fingerprint: "sha256:" + strings.Repeat("f", 64)}
	if approvalUID != "" {
		plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{
			Name: "e2e-approval", UID: approvalUID, Approver: ptahv1alpha1.ApprovalIdentity{Username: schemaReadingApprover},
		}
	}
	before := &ptahv1alpha1.PtahSchemaStatus{ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{
		Type: ptahv1alpha1.OperationApply, ID: operationID, StartedAt: metav1.NewTime(now.Add(-time.Minute)),
	}}
	after := &ptahv1alpha1.PtahSchemaStatus{PendingObservation: &ptahv1alpha1.PendingObservationStatus{
		Outcome: ptahv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: operationID, Plan: plan,
	}}
	entries := lifecyclejournal.SchemaEntries(before, after, nil, now)
	return &ptahv1alpha1.LifecycleJournal{Entries: entries}
}

func TestFinishedRunOfFindsTheRunAnApprovalOrAPolicyAuthorized(t *testing.T) {
	t.Parallel()

	approved := mpRecorded(t).Journal
	schemaOperation := "sha256:" + strings.Repeat("5a", 32)
	for _, row := range []struct {
		name        string
		journal     *ptahv1alpha1.LifecycleJournal
		entryType   ptahv1alpha1.JournalEntryType
		approvalUID types.UID
		want        string
	}{
		{
			name: "a migration run a person approved", journal: approved, entryType: ptahv1alpha1.JournalRunFinished,
			approvalUID: "6ff1f350-6a90-49f7-a124-1d02bc0d1743", want: "sha256:9798b1a53fe9673ffee44b6b38900db3fbeec94c414e81dfd6786d8424d94a4e",
		},
		{
			name: "a migration run its policy allowed", journal: policyRunJournal(t), entryType: ptahv1alpha1.JournalRunFinished,
			want: "sha256:065d4435d418c50b8c095faa55a8185fa4db2410b9d129782a1aecfd5e18734a",
		},
		{
			name: "a schema Apply a person approved", journal: approvedSchemaApplyJournal(t, "schema-approval-uid", schemaOperation),
			entryType: ptahv1alpha1.JournalApplyFinished, approvalUID: "schema-approval-uid", want: schemaOperation,
		},
		{
			name: "a schema Apply its policy allowed", journal: approvedSchemaApplyJournal(t, "", schemaOperation),
			entryType: ptahv1alpha1.JournalApplyFinished, want: schemaOperation,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			entry, err := finishedRunOf(row.journal, row.entryType, row.approvalUID)
			if err != nil {
				t.Fatalf("finishedRunOf() refused the operator's journal: %v", err)
			}
			if entry.OperationID != row.want {
				t.Fatalf("finishedRunOf() = operation %q, want %q", entry.OperationID, row.want)
			}
		})
	}
}

func TestFinishedRunOfRefusesARunItCannotJoin(t *testing.T) {
	t.Parallel()

	const approvalUID = types.UID("6ff1f350-6a90-49f7-a124-1d02bc0d1743")
	finished := func(journal *ptahv1alpha1.LifecycleJournal) *ptahv1alpha1.JournalEntry {
		for index := range journal.Entries {
			if journal.Entries[index].Type == ptahv1alpha1.JournalRunFinished {
				return &journal.Entries[index]
			}
		}
		t.Fatal("the reading holds no RunFinished entry")
		return nil
	}
	for _, row := range []struct {
		name        string
		mutate      func(*ptahv1alpha1.LifecycleJournal)
		entryType   ptahv1alpha1.JournalEntryType
		approvalUID types.UID
		says        string
	}{
		{name: "another approval", entryType: ptahv1alpha1.JournalRunFinished, approvalUID: "another-uid", says: "holds 0 RunFinished entries under approval UID another-uid"},
		// The approved run is not one its policy allowed.
		{name: "no approval where a person approved", entryType: ptahv1alpha1.JournalRunFinished, says: "holds 0 RunFinished entries under no approval"},
		{name: "a schema's entry type on a migration", entryType: ptahv1alpha1.JournalApplyFinished, approvalUID: approvalUID, says: "holds 0 ApplyFinished entries"},
		{
			name:      "a run journaled twice",
			mutate:    func(j *ptahv1alpha1.LifecycleJournal) { j.Entries = append(j.Entries, *finished(j)) },
			entryType: ptahv1alpha1.JournalRunFinished, approvalUID: approvalUID, says: "holds 2 RunFinished entries",
		},
		{
			name:      "a run of unknown outcome",
			mutate:    func(j *ptahv1alpha1.LifecycleJournal) { finished(j).Outcome = ptahv1alpha1.JournalUnknown },
			entryType: ptahv1alpha1.JournalRunFinished, approvalUID: approvalUID, says: `records outcome "Unknown"`,
		},
		{
			name: "a run the policy authorized under an approval",
			mutate: func(j *ptahv1alpha1.LifecycleJournal) {
				finished(j).Authorization = ptahv1alpha1.MigrationRunAuthorizedByPolicy
			},
			entryType: ptahv1alpha1.JournalRunFinished, approvalUID: approvalUID, says: `records authorization "Policy", want "Approval"`,
		},
		{
			name:      "a run with no operation",
			mutate:    func(j *ptahv1alpha1.LifecycleJournal) { finished(j).OperationID = "" },
			entryType: ptahv1alpha1.JournalRunFinished, approvalUID: approvalUID, says: `names operation ""`,
		},
		{
			name:      "an operation that is not a claim's",
			mutate:    func(j *ptahv1alpha1.LifecycleJournal) { finished(j).OperationID = "sha256:ABC" },
			entryType: ptahv1alpha1.JournalRunFinished, approvalUID: approvalUID, says: `names operation "sha256:ABC"`,
		},
		{
			name:      "no journal",
			mutate:    func(j *ptahv1alpha1.LifecycleJournal) { j.Entries = nil },
			entryType: ptahv1alpha1.JournalRunFinished, approvalUID: approvalUID, says: "holds 0 RunFinished entries",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			journal := mpRecorded(t).Journal
			if row.mutate != nil {
				row.mutate(journal)
			}
			_, err := finishedRunOf(journal, row.entryType, row.approvalUID)
			if err == nil {
				t.Fatal("finishedRunOf() accepted it")
			}
			if !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
			}
		})
	}
	t.Run("a policy run where the row wants a person", func(t *testing.T) {
		t.Parallel()
		journal := policyRunJournal(t)
		finished(journal).Authorization = ptahv1alpha1.MigrationRunAuthorizedByApproval
		if _, err := finishedRunOf(journal, ptahv1alpha1.JournalRunFinished, ""); err == nil ||
			!strings.Contains(err.Error(), `records authorization "Approval", want "Policy"`) {
			t.Fatalf("finishedRunOf() = %v, want a refusal of the authorization", err)
		}
	})
	if _, err := finishedRunOf(nil, ptahv1alpha1.JournalRunFinished, approvalUID); err == nil {
		t.Fatal("finishedRunOf() accepted a resource that keeps no journal")
	}
}
