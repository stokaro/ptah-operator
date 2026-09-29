package e2e

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestChangedMigrationApprovalRequiresFreshEvidence(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: ptahv1alpha1.PtahMigrationStatus{
			ObservedGeneration: 2, Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval,
			Plan:       &ptahv1alpha1.ImmutableObjectReference{Name: "new", UID: "new-uid"},
			Conditions: []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionTrue, Reason: "AwaitingApproval"}},
		},
	}
	if !changedMigrationApprovalRefused(base, "old-uid", 2, false) {
		t.Fatal("rejected a new plan awaiting approval for the edited generation")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"old status":        func(r *ptahv1alpha1.PtahMigration) { r.Status.ObservedGeneration = 1 },
		"later edit":        func(r *ptahv1alpha1.PtahMigration) { r.Generation = 3 },
		"old plan":          func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan.UID = "old-uid" },
		"no plan":           func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan = nil },
		"unnamed plan":      func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan.Name = "" },
		"unidentified plan": func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan.UID = "" },
		"unexplained phase": func(r *ptahv1alpha1.PtahMigration) { r.Status.Conditions = nil },
		"active operation": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource := base.DeepCopy()
			mutate(resource)
			if changedMigrationApprovalRefused(resource, "old-uid", 2, false) {
				t.Fatal("accepted a reading that does not prove the changed approval gate")
			}
		})
	}
	disabled := base.DeepCopy()
	disabled.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked
	disabled.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ApplyDisabled"},
		{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "ApplyDisabled"},
	}
	if !changedMigrationApprovalRefused(disabled, "old-uid", 2, true) || changedMigrationApprovalRefused(base, "old-uid", 2, true) {
		t.Fatal("disabled policy must have its own refusal, not merely await approval")
	}
}

func TestChangedSchemaApprovalRequiresFreshEvidence(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: ptahv1alpha1.PtahSchemaStatus{
			ObservedGeneration: 2, Phase: ptahv1alpha1.PhaseAwaitingApproval,
			Plan:       &ptahv1alpha1.CurrentPlanStatus{Name: "new", UID: "new-uid"},
			Conditions: []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionTrue, Reason: "PlanReady"}},
		},
	}
	if !changedSchemaApprovalRefused(base, "old-uid", 2) {
		t.Fatal("rejected a new plan awaiting approval for the edited generation")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"old status":        func(r *ptahv1alpha1.PtahSchema) { r.Status.ObservedGeneration = 1 },
		"later edit":        func(r *ptahv1alpha1.PtahSchema) { r.Generation = 3 },
		"old plan":          func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan.UID = "old-uid" },
		"no plan":           func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan = nil },
		"unexplained phase": func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions = nil },
		"active operation":  func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} },
	} {
		t.Run(name, func(t *testing.T) {
			resource := base.DeepCopy()
			mutate(resource)
			if changedSchemaApprovalRefused(resource, "old-uid", 2) {
				t.Fatal("accepted a reading that does not prove the changed approval gate")
			}
		})
	}
}
