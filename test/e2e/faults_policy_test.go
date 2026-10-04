package e2e

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestDestructivePolicyGateRequiresTheCurrentDecision(t *testing.T) {
	t.Parallel()
	for _, allowed := range []bool{false, true} {
		base := &ptahv1alpha1.PtahSchema{
			ObjectMeta: metav1.ObjectMeta{Generation: 7},
			Spec: ptahv1alpha1.PtahSchemaSpec{Policy: ptahv1alpha1.ReconciliationPolicy{
				Apply: ptahv1alpha1.ApplyPolicyOnApproval, AllowDestructive: allowed,
			}},
			Status: ptahv1alpha1.PtahSchemaStatus{
				ObservedGeneration: 7, Phase: ptahv1alpha1.PhaseBlocked,
				Plan:       &ptahv1alpha1.CurrentPlanStatus{Name: "plan", UID: "plan-uid", Fingerprint: "fingerprint", Destructive: true},
				Conditions: []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionFalse, Reason: "DestructiveChangesDisabled"}},
			},
		}
		if allowed {
			base.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval
			base.Status.Conditions[0].Status = metav1.ConditionTrue
			base.Status.Conditions[0].Reason = "PlanReady"
		}
		if !destructivePolicyGate(base, 7, allowed) {
			t.Fatalf("refused a current destructive policy gate: allowed=%t", allowed)
		}
		for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
			"stale generation":       func(s *ptahv1alpha1.PtahSchema) { s.Generation++ },
			"stale observation":      func(s *ptahv1alpha1.PtahSchema) { s.Status.ObservedGeneration-- },
			"wrong permission":       func(s *ptahv1alpha1.PtahSchema) { s.Spec.Policy.AllowDestructive = !allowed },
			"automatic apply":        func(s *ptahv1alpha1.PtahSchema) { s.Spec.Policy.Apply = ptahv1alpha1.ApplyPolicyAlways },
			"active operation":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} },
			"no plan":                func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil },
			"no plan name":           func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Name = "" },
			"no plan UID":            func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.UID = "" },
			"no fingerprint":         func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Fingerprint = "" },
			"non-destructive plan":   func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Destructive = false },
			"approval already taken": func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{} },
			"no refusal or gate":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
			"unknown condition":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionUnknown },
		} {
			candidate := base.DeepCopy()
			edit(candidate)
			if destructivePolicyGate(candidate, 7, allowed) {
				t.Errorf("accepted %s: allowed=%t", name, allowed)
			}
		}
		if destructivePolicyGate(nil, 7, allowed) || destructivePolicyGate(base, 0, allowed) {
			t.Fatal("accepted a missing resource or generation")
		}
		candidate := base.DeepCopy()
		if allowed {
			candidate.Status.Phase = ptahv1alpha1.PhaseInSync
		} else {
			candidate.Status.Conditions[0].Reason = "ApplyDisabled"
		}
		if destructivePolicyGate(candidate, 7, allowed) {
			t.Fatalf("accepted an unrelated phase or refusal: allowed=%t", allowed)
		}
	}
}

func TestChangedDestructivePolicyKeepsTheDatabaseAndArtifact(t *testing.T) {
	t.Parallel()
	digest := func(letter string) string { return "sha256:" + strings.Repeat(letter, 64) }
	old := &ptahv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{UID: "old-plan"},
		Spec: ptahv1alpha1.PtahSchemaPlanSpec{
			SchemaRef:   ptahv1alpha1.ImmutableObjectReference{Name: "schema", UID: "schema-uid"},
			Fingerprint: "old-fingerprint", PolicyFingerprint: "old-policy", ArtifactDigest: digest("a"),
			TargetIdentityDigest: digest("b"), CoordinationDigest: digest("c"), ActualStateFingerprint: digest("d"),
			Destructive: true, StatementCount: 2,
		},
	}
	fresh := old.DeepCopy()
	fresh.UID, fresh.Spec.Fingerprint, fresh.Spec.PolicyFingerprint = "new-plan", "new-fingerprint", "new-policy"
	if err := changedDestructivePolicyPlan(old, fresh); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"same plan":         func(p *ptahv1alpha1.PtahSchemaPlan) { p.UID = old.UID },
		"missing identity":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.UID = "" },
		"same fingerprint":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Fingerprint = old.Spec.Fingerprint },
		"empty fingerprint": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Fingerprint = "" },
		"same policy":       func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = old.Spec.PolicyFingerprint },
		"empty policy":      func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = "" },
		"another schema":    func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.SchemaRef.UID = "replacement" },
		"another artifact":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = digest("e") },
		"another target":    func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.TargetIdentityDigest = digest("e") },
		"another realm":     func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.CoordinationDigest = digest("e") },
		"changed database":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ActualStateFingerprint = digest("e") },
		"safe plan":         func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Destructive = false },
		"empty plan":        func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = 0 },
		"negative count":    func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := fresh.DeepCopy()
			edit(candidate)
			if changedDestructivePolicyPlan(old, candidate) == nil {
				t.Fatal("accepted an unrelated or obsolete destructive decision")
			}
		})
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"empty schema":   func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.SchemaRef.UID = "" },
		"empty artifact": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = "" },
		"empty target":   func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.TargetIdentityDigest = "" },
		"empty realm":    func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.CoordinationDigest = "" },
		"empty state":    func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ActualStateFingerprint = "" },
	} {
		left, right := old.DeepCopy(), fresh.DeepCopy()
		edit(left)
		edit(right)
		if changedDestructivePolicyPlan(left, right) == nil {
			t.Errorf("accepted matching missing bindings: %s", name)
		}
	}
	if changedDestructivePolicyPlan(nil, fresh) == nil || changedDestructivePolicyPlan(old, nil) == nil {
		t.Fatal("accepted a missing plan")
	}
}
