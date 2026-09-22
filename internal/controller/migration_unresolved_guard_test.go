package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A run nobody accounted for refuses the next Apply where the claim is taken.
//
// The state this builds is one the ordinary path does not produce: a record
// standing beside a plan that is still published and an approval that is still
// valid. A reading that refuses to settle the record normally leaves
// the resource Blocked with no plan, and a claim with no plan cannot be built
// -- two cooperating sites holding one invariant. This proves the invariant
// holds without either of them, which is what makes it a guard rather than a
// consequence.
func TestAnUnresolvedRunRefusesTheNextApply(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome:              operatorv1alpha1.MigrationRunOutcomePartial,
		RecordedAt:           metav1.Now(),
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
	}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("an Apply was claimed while a run nobody accounted for stood: %#v", actual.Status.ActiveOperation)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a Job was created while a run nobody accounted for stood: %d", len(jobs.Items))
	}
	if actual.Status.UnresolvedRun == nil {
		t.Fatal("the refusal cleared the record it was refusing for")
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("the resource reads as %q rather than blocked, so nothing says why it stopped",
			actual.Status.Phase)
	}
	blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionTrue {
		t.Fatalf("the refusal published no Blocked condition: %#v", blocked)
	}
	if !strings.Contains(blocked.Message, string(operatorv1alpha1.MigrationRunOutcomePartial)) {
		t.Fatalf("the refusal does not say which run it is about: %q", blocked.Message)
	}
}
