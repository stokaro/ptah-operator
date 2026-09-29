package e2e

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestRetargetRecoveryNeedsAFreshDecisionAfterAccountingForTheRun(t *testing.T) {
	resolvedAt := metav1.NewTime(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	original := &ptahv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "old-plan", Namespace: "test", UID: "old-plan-uid"},
		Spec: ptahv1alpha1.PtahMigrationPlanSpec{
			MigrationRef: ptahv1alpha1.ImmutableObjectReference{Name: "migration", UID: "migration-uid"},
			Fingerprint:  proofDigest("1"), ArtifactDigest: proofDigest("2"), TargetIdentityDigest: proofDigest("3"),
			Migrations: []ptahv1alpha1.PlannedMigration{{Version: 1}},
		},
	}
	plan := original.DeepCopy()
	plan.Name, plan.UID = "new-plan", "new-plan-uid"
	plan.Spec.Fingerprint, plan.Spec.HistoryFingerprint, plan.Spec.TargetIdentityDigest = proofDigest("4"), proofDigest("5"), proofDigest("6")
	resource := &ptahv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: "test", UID: "migration-uid", Generation: 1},
		Status: ptahv1alpha1.PtahMigrationStatus{
			ObservedGeneration: 1, Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval,
			Plan:     &ptahv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			Artifact: &ptahv1alpha1.OCIArtifactAccessBinding{Digest: plan.Spec.ArtifactDigest},
			History: &ptahv1alpha1.MigrationHistoryStatus{
				ObservedAt: metav1.NewTime(resolvedAt.Add(2 * time.Second)), Fingerprint: plan.Spec.HistoryFingerprint,
				TargetIdentityDigest: plan.Spec.TargetIdentityDigest,
			},
			ResolvedRun: &ptahv1alpha1.ResolvedMigrationRunStatus{
				OperationID: proofDigest("7"), Resolution: ptahv1alpha1.MigrationRunResolvedByAcknowledgment,
				AcknowledgmentRef: &ptahv1alpha1.ImmutableObjectReference{Name: "accounted-for", UID: "ack-uid"},
				AcknowledgedBy:    &ptahv1alpha1.ApprovalIdentity{Username: "approver"}, ResolvedAt: resolvedAt,
			},
			Conditions: []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionTrue}},
		},
	}
	operation := resource.Status.ResolvedRun.OperationID
	if err := retargetRecoveryPlan(resource, plan, original, operation); err != nil {
		t.Fatalf("a fresh approval gate after acknowledgment was refused: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigrationPlan){
		"old generation": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Generation++ },
		"already dispatched": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
		},
		"still unresolved": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{}
		},
		"unresolved copy remains": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Annotations = map[string]string{ptahv1alpha1.UnresolvedRunAnnotation: "{}"}
		},
		"another resource": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.MigrationRef.UID = "other"
		},
		"another plan": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) { p.UID = "other" },
		"old fingerprint": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Fingerprint = original.Spec.Fingerprint
		},
		"old target": func(r *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.TargetIdentityDigest = original.Spec.TargetIdentityDigest
			r.Status.History.TargetIdentityDigest = original.Spec.TargetIdentityDigest
		},
		"missing history": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.History = nil },
		"old history": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.History.ObservedAt = resolvedAt
		},
		"different history": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.History.Fingerprint = proofDigest("8")
		},
		"another artifact": func(r *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.ArtifactDigest = proofDigest("8")
			r.Status.Artifact.Digest = p.Spec.ArtifactDigest
		},
		"another sequence":   func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations[0].Version++ },
		"missing resolution": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.ResolvedRun = nil },
		"another run settled": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ResolvedRun.OperationID = proofDigest("8")
		},
		"no person": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ResolvedRun.AcknowledgedBy = nil
		},
		"no acknowledgment": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ResolvedRun.AcknowledgmentRef = nil
		},
		"automatic resolution": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ResolvedRun.Resolution = ptahv1alpha1.MigrationRunResolvedByHistoryRead
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, p := resource.DeepCopy(), plan.DeepCopy()
			mutate(r, p)
			if err := retargetRecoveryPlan(r, p, original, operation); err == nil {
				t.Fatal("invalid recovery evidence passed")
			}
		})
	}
}
