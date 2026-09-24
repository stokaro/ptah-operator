package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The controller does not refuse an approval that disagrees with the plan; it
// declines to count it, and the migration goes on waiting. Eleven conditions
// decide that, written as one disjunction, so any approval skipped for any
// reason marks the whole branch measured.
//
// A part that stops working does not produce an error anybody sees. It
// produces a dispatch: an Apply carried out under an approval that authorized
// something else, or that was withdrawn, or that nobody signed.
//
// Each row moves exactly one thing about the approval and requires the
// migration to keep waiting, which is what this filter refusing something
// looks like.
func TestAnApprovalThatDisagreesWithThePlanDoesNotCount(t *testing.T) {
	t.Parallel()

	t.Run("a matching approval is counted", func(t *testing.T) {
		t.Parallel()

		migration, plan := awaitingApprovalFixture(t)
		approval := migrationApprovalFor(migration, plan)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
			verificationPolicyConfigMap())

		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		operation := readMigration(t, api, migration).Status.ActiveOperation
		if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply {
			t.Fatalf("a matching approval was not counted, so nothing below proves anything: %#v", operation)
		}
	})

	for _, row := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahMigrationApproval)
	}{
		{
			name: "it is on its way out",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				deleted := metav1.Now()
				approval.DeletionTimestamp = &deleted
				approval.Finalizers = []string{"e2e.test/hold"}
			},
		},
		{
			name: "it authorizes another migration",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.MigrationRef.UID = "another-migration-uid"
			},
		},
		{
			name: "it authorizes another plan",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.PlanRef.UID = "another-plan-uid"
			},
		},
		{
			// The plan object is the one named and its content is not the one
			// approved.
			name: "the plan it names has been recomputed",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.PlanFingerprint = "sha256:" + strings.Repeat("e", 64)
			},
		},
		{
			name: "it was given under another database history",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.HistoryFingerprint = "sha256:" + strings.Repeat("e", 64)
			},
		},
		{
			name: "it was given under another execution binding",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.ExecutionBindingID = "v1-" + strings.Repeat("e", 32)
			},
		},
		{
			name: "it was given against another database",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.TargetIdentityDigest = "sha256:" + strings.Repeat("e", 64)
			},
		},
		{
			name: "it was given against another artifact",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.ArtifactDigest = "sha256:" + strings.Repeat("e", 64)
			},
		},
		{
			name: "it was given under another apply policy",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.PolicyFingerprint = "sha256:" + strings.Repeat("e", 64)
			},
		},
		{
			// An approval with nobody's name on it records no decision.
			name: "nobody is named as having agreed",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.Approver.Username = "   "
			},
		},
		{
			name: "the mutating webhook stamped no request",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Spec.MutationRequestUID = ""
			},
		},
		{
			// Withdrawn.
			name: "it has been marked stale",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Status.Conditions = append(approval.Status.Conditions, metav1.Condition{
					Type: operatorv1alpha1.ConditionApprovalStale, Status: metav1.ConditionTrue,
					Reason: "Superseded", Message: "a newer plan replaced the approved one",
					LastTransitionTime: metav1.Now(),
				})
			},
		},
		{
			// Spent: one approval authorizes one run.
			name: "it has already been consumed",
			change: func(approval *operatorv1alpha1.PtahMigrationApproval) {
				approval.Status.Conditions = append(approval.Status.Conditions, metav1.Condition{
					Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
					Reason: "Consumed", Message: "an earlier Apply carried it out",
					LastTransitionTime: metav1.Now(),
				})
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			approval := migrationApprovalFor(migration, plan)
			row.change(approval)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
				verificationPolicyConfigMap())

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if operation := actual.Status.ActiveOperation; operation != nil {
				t.Fatalf("an Apply was dispatched under an approval that does not authorize it: %#v",
					operation)
			}
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
				t.Fatalf("phase = %q, want the migration still waiting for an approval",
					actual.Status.Phase)
			}
		})
	}
}
