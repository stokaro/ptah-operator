package historyview_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/historyview"
)

// A schema's ApplyFinished entry says where the verdict on that Apply is: the
// Verified entry of its plan that follows it, the reading the status still
// owes, or neither. The join runs forward by plan name and stops at another
// Apply of the plan, and only the newest Apply can owe the pending reading.
func TestApplyFinishedSaysWhereItsProofIs(t *testing.T) {
	t.Parallel()

	applied := func(offset time.Duration, plan string) operatorv1alpha1.JournalEntry {
		return operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalApplyFinished, At: at(offset), Plan: plan, Outcome: operatorv1alpha1.JournalSucceeded,
		}
	}
	verified := func(offset time.Duration, plan string, outcome operatorv1alpha1.JournalOutcome) operatorv1alpha1.JournalEntry {
		return operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalVerified, At: at(offset), Plan: plan, Outcome: outcome}
	}
	const (
		verifiedText = "Proof:              the Verified entry at "
		owedText     = "Proof:              still owed: status.pendingObservation holds the reading of the database after it"
		missingText  = "Proof:              not in the journal: no Verified entry of its plan follows it"
	)
	tests := []struct {
		name    string
		entries []operatorv1alpha1.JournalEntry
		pending string
		// want is the proof of each ApplyFinished entry, in order: a state,
		// and for Verified the minute of the entry that settled it.
		want []historyview.Proof
	}{
		{
			name:    "a Verified entry follows the Apply",
			entries: []operatorv1alpha1.JournalEntry{applied(time.Minute, "plan-a"), verified(2*time.Minute, "plan-a", operatorv1alpha1.JournalInSync)},
			want:    []historyview.Proof{{State: historyview.ProofVerified, At: ptr(at(2 * time.Minute)), Outcome: operatorv1alpha1.JournalInSync}},
		},
		{
			name:    "the status still owes the reading",
			entries: []operatorv1alpha1.JournalEntry{applied(time.Minute, "plan-a")},
			pending: "plan-a",
			want:    []historyview.Proof{{State: historyview.ProofOwed}},
		},
		{
			name:    "neither the journal nor the status has it",
			entries: []operatorv1alpha1.JournalEntry{applied(time.Minute, "plan-a")},
			want:    []historyview.Proof{{State: historyview.ProofNotInJournal}},
		},
		{
			name: "an earlier Verified entry of the plan proved an earlier Apply",
			entries: []operatorv1alpha1.JournalEntry{
				verified(time.Minute, "plan-a", operatorv1alpha1.JournalInSync), applied(2*time.Minute, "plan-a"),
			},
			pending: "plan-a",
			want:    []historyview.Proof{{State: historyview.ProofOwed}},
		},
		{
			name:    "a Verified entry of another plan",
			entries: []operatorv1alpha1.JournalEntry{applied(time.Minute, "plan-a"), verified(2*time.Minute, "plan-b", operatorv1alpha1.JournalInSync)},
			want:    []historyview.Proof{{State: historyview.ProofNotInJournal}},
		},
		{
			name:    "the status owes the reading of another plan",
			entries: []operatorv1alpha1.JournalEntry{applied(time.Minute, "plan-a")},
			pending: "plan-b",
			want:    []historyview.Proof{{State: historyview.ProofNotInJournal}},
		},
		{
			name: "another Apply of the plan stops the join",
			entries: []operatorv1alpha1.JournalEntry{
				applied(time.Minute, "plan-a"), applied(2*time.Minute, "plan-a"),
				verified(3*time.Minute, "plan-a", operatorv1alpha1.JournalDriftRemains),
			},
			want: []historyview.Proof{
				{State: historyview.ProofNotInJournal},
				{State: historyview.ProofVerified, At: ptr(at(3 * time.Minute)), Outcome: operatorv1alpha1.JournalDriftRemains},
			},
		},
		{
			name:    "only the newest Apply owes the pending reading",
			entries: []operatorv1alpha1.JournalEntry{applied(time.Minute, "plan-a"), applied(2*time.Minute, "plan-b")},
			pending: "plan-a",
			want:    []historyview.Proof{{State: historyview.ProofNotInJournal}, {State: historyview.ProofNotInJournal}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := &operatorv1alpha1.PtahSchema{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "storefront", UID: "schema-uid", CreationTimestamp: at(-time.Hour)},
				Status: operatorv1alpha1.PtahSchemaStatus{
					Journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(0), Entries: test.entries},
				},
			}
			if test.pending != "" {
				schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
					Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: "op-1",
					Plan: operatorv1alpha1.CurrentPlanStatus{Name: test.pending},
				}
			}
			view, err := historyview.Load(context.Background(), reader(t, nil, schema), historyview.Schema, "team-a", "storefront")
			if err != nil {
				t.Fatal(err)
			}
			text := render(t, view)
			var proofs []historyview.Proof
			for _, entry := range view.Journal.Entries {
				if entry.Type != operatorv1alpha1.JournalApplyFinished {
					if entry.Proof != nil {
						t.Errorf("the %s entry carries a proof: %+v", entry.Type, *entry.Proof)
					}
					continue
				}
				if entry.Proof == nil {
					t.Fatalf("the ApplyFinished entry at %s carries no proof", entry.At)
				}
				proofs = append(proofs, *entry.Proof)
			}
			if len(proofs) != len(test.want) {
				t.Fatalf("%d ApplyFinished entries, want %d", len(proofs), len(test.want))
			}
			counts := map[historyview.ProofState]int{}
			for index, want := range test.want {
				got := proofs[index]
				counts[want.State]++
				if got.State != want.State || got.Outcome != want.Outcome ||
					(want.At == nil) != (got.At == nil) || (want.At != nil && !want.At.Equal(got.At)) {
					t.Errorf("Apply %d: proof %+v, want %+v", index, got, want)
				}
			}
			// -o json carries the same proofs.
			var document strings.Builder
			if err := historyview.Render(&document, view, historyview.JSON); err != nil {
				t.Fatal(err)
			}
			var decoded historyview.View
			if err := json.Unmarshal([]byte(document.String()), &decoded); err != nil {
				t.Fatal(err)
			}
			for index, entry := range decoded.Journal.Entries {
				if !equality.Semantic.DeepEqual(entry.Proof, view.Journal.Entries[index].Proof) {
					t.Errorf("entry %d: the JSON carries proof %+v, the view %+v", index, entry.Proof, view.Journal.Entries[index].Proof)
				}
			}
			// The text says the same, once for each Apply.
			for state, line := range map[historyview.ProofState]string{
				historyview.ProofVerified: verifiedText, historyview.ProofOwed: owedText, historyview.ProofNotInJournal: missingText,
			} {
				if got := strings.Count(text, line); got != counts[state] {
					t.Errorf("the text says %q %d times, want %d:\n%s", line, got, counts[state], text)
				}
			}
		})
	}
}

// A migration has no ApplyFinished entry, and a migration's Verified entry is
// joined to its run by the operation ID, so no migration entry carries a
// schema's proof.
func TestAMigrationEntryCarriesNoSchemaProof(t *testing.T) {
	t.Parallel()

	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "orders-uid", CreationTimestamp: at(-time.Hour)},
		Status: operatorv1alpha1.PtahMigrationStatus{Journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(0), Entries: []operatorv1alpha1.JournalEntry{
			{Type: operatorv1alpha1.JournalApplyFinished, At: at(time.Minute), Plan: "plan-a", Outcome: operatorv1alpha1.JournalSucceeded},
		}}},
	}
	view, err := historyview.Load(context.Background(), reader(t, nil, migration), historyview.Migration, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if proof := view.Journal.Entries[0].Proof; proof != nil {
		t.Fatalf("a migration entry carries a proof: %+v", *proof)
	}
}

// A migration run entry says what it does not record. One a release before
// 0.2.10 wrote names no Apply claim and no Job, and is joined to no run; one
// written since that names no Job ran as no Job the API server confirmed.
func TestRunEntriesSayWhatTheyDoNotRecord(t *testing.T) {
	t.Parallel()

	legacy := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: at(time.Hour), Plan: "ptah-mplan-orders-1", Outcome: operatorv1alpha1.JournalSucceeded,
	}
	current := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: at(2 * time.Hour), Plan: "ptah-mplan-orders-2",
		Outcome:     operatorv1alpha1.JournalUnknown,
		OperationID: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
	}
	text := render(t, viewOf(historyview.Migration, legacy, current))
	legacyAt, currentAt := strings.Index(text, "2026-10-01T10:00:00Z  RunFinished"), strings.Index(text, "2026-10-01T11:00:00Z  RunFinished")
	end := strings.Index(text, "\nApprovals")
	if legacyAt < 0 || currentAt < legacyAt || end < currentAt {
		t.Fatalf("the history does not list both run entries in order:\n%s", text)
	}
	legacyText, currentText := text[legacyAt:currentAt], text[currentAt:end]

	if want := "Operation:          not recorded; an entry a release before 0.2.10 wrote names no Apply claim, " +
		"so it is not joined to a run\n"; !strings.Contains(legacyText, want) {
		t.Errorf("the legacy entry does not say %q:\n%s", want, legacyText)
	}
	if strings.Contains(legacyText, "Job:") {
		t.Errorf("the legacy entry says something about a Job it never recorded:\n%s", legacyText)
	}
	for _, want := range []string{
		"Operation:          sha256:3333333333333333333333333333333333333333333333333333333333333333\n",
		"Job:                none confirmed: the API server never confirmed one was created\n",
	} {
		if !strings.Contains(currentText, want) {
			t.Errorf("the entry written since 0.2.10 does not say %q:\n%s", want, currentText)
		}
	}
}
