package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// TestAProofThatLostTheDatabaseProvesNothing covers what happens when the
// database realm's Lease changes epochs underneath a post-Apply proof.
//
// The proof is a read-only pass whose whole purpose is to say that this Apply
// is why the database looks the way it does. It can only say that if nothing
// else held the database in between. A new lease epoch is exactly the evidence
// that something did, so the proof stops being about this Apply: its outcome
// is downgraded to unknown, the plan it would have re-derived is no longer
// required, and the resource says a fresh observation is needed.
//
// What that costs is deliberate and is the reason the downgrade has to happen
// here. A proof still marked ApplySucceeded would converge and credit this
// Apply in status.applied on the strength of a reading taken after somebody
// else had the database.
//
// Neither the downgrade nor the epoch reconciliation around it was covered.
func TestAProofThatLostTheDatabaseProvesNothing(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name         string
		recordEpoch  func(seeded string) string
		wantOutcome  operatorv1alpha1.PendingObservationOutcome
		wantLost     bool
		wantRequired bool
	}{
		{
			// Nothing else held the database, so the proof still speaks for
			// the Apply that owed it.
			name:         "the lease the proof was taken under still stands",
			recordEpoch:  func(seeded string) string { return seeded },
			wantOutcome:  operatorv1alpha1.PendingObservationApplySucceeded,
			wantLost:     false,
			wantRequired: true,
		},
		{
			name:         "the lease changed epochs under the proof",
			recordEpoch:  func(string) string { return testLeaseEpochOther },
			wantOutcome:  operatorv1alpha1.PendingObservationOutcomeUnknown,
			wantLost:     true,
			wantRequired: false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			schema := safetyPostApplyObserveSchema(t)
			schema.Status.PendingObservation.Outcome = operatorv1alpha1.PendingObservationApplySucceeded
			schema.Status.PendingObservation.PlanRequired = true
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationObserve, ID: "post-apply-observe",
				JobName: "post-apply-observe-job", StartedAt: metav1.Now(), Attempt: 1,
			}
			bindActiveInput(t, schema)
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			reconciler.Locks = targetlock.New(api, api, nil)

			// Fixture seeding takes the Lease and records its epoch, so the
			// recorded one is moved afterwards or this proves nothing.
			stored := safetyGetSchema(t, api, schema)
			seeded := stored.Status.PendingObservation.LeaseEpoch
			if seeded == "" {
				t.Fatal("the fixture recorded no lease epoch, so changing it proves nothing")
			}
			stored.Status.PendingObservation.LeaseEpoch = row.recordEpoch(seeded)
			stored.Status.ActiveOperation.LeaseEpoch = ""
			if err := api.Status().Update(ctx, stored); err != nil {
				t.Fatal(err)
			}

			for range 3 {
				if _, _, err := reconciler.acquirePendingObservationLock(
					ctx, safetyGetSchema(t, api, schema),
				); err != nil {
					t.Fatalf("acquirePendingObservationLock() error = %v", err)
				}
			}

			actual := safetyGetSchema(t, api, schema)
			pending := actual.Status.PendingObservation
			if pending == nil || actual.Status.ActiveOperation == nil {
				t.Fatalf("the proof was discarded rather than settled: %#v", actual.Status)
			}
			if pending.Outcome != row.wantOutcome {
				t.Fatalf("outcome = %q, want %q", pending.Outcome, row.wantOutcome)
			}
			if pending.PlanRequired != row.wantRequired {
				t.Fatalf("planRequired = %t, want %t", pending.PlanRequired, row.wantRequired)
			}
			if actual.Status.ActiveOperation.LeaseContinuityLost != row.wantLost {
				t.Fatalf("leaseContinuityLost = %t, want %t",
					actual.Status.ActiveOperation.LeaseContinuityLost, row.wantLost)
			}
			if pending.LeaseEpoch != seeded {
				t.Fatalf("lease epoch = %q, want the one the database now holds (%q)",
					pending.LeaseEpoch, seeded)
			}
			condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync)
			if !row.wantLost {
				if condition != nil && condition.Reason == string(operatorv1alpha1.ReasonLeaseContinuityLost) {
					t.Fatalf("an intact proof was reported as having lost the database: %#v", condition)
				}
				return
			}
			if condition == nil || condition.Status != metav1.ConditionUnknown ||
				condition.Reason != string(operatorv1alpha1.ReasonLeaseContinuityLost) {
				t.Fatalf("InSync condition = %#v, want unknown with LeaseContinuityLost", condition)
			}
		})
	}
}
