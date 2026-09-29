package e2e

import (
	"strings"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSchemaRetargetRefusalIsBeforeChildDispatch(t *testing.T) {
	t.Parallel()
	previous := "sha256:" + strings.Repeat("a", 64)
	valid := runner.Result{TargetIdentityDigest: "sha256:" + strings.Repeat("b", 64), ChildExitCode: -1,
		Error: &runner.ResultError{Code: "target_binding_mismatch"}}
	if !schemaRetargetRefused(valid, previous) {
		t.Fatal("rejected target-binding refusal before child dispatch")
	}
	for name, edit := range map[string]func(*runner.Result){
		"same target":        func(r *runner.Result) { r.TargetIdentityDigest = previous },
		"missing target":     func(r *runner.Result) { r.TargetIdentityDigest = "" },
		"another error":      func(r *runner.Result) { r.Error = &runner.ResultError{Code: "stale_plan"} },
		"no refusal":         func(r *runner.Result) { r.Error = nil },
		"child ran":          func(r *runner.Result) { r.ChildExitCode = 0 },
		"mutation began":     func(r *runner.Result) { r.MutationStarted = true },
		"uncertain work":     func(r *runner.Result) { r.Uncertain = true },
		"truncated evidence": func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} },
	} {
		t.Run(name, func(t *testing.T) {
			reading := valid
			edit(&reading)
			if schemaRetargetRefused(reading, previous) {
				t.Fatal("accepted a reading that does not prove refusal before dispatch")
			}
		})
	}
}

func TestFreshApprovalWaitsForObservedConvergence(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseInSync, ObservedGeneration: 2,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
	}}
	if !freshApprovalConverged(base) {
		t.Fatal("rejected current observed convergence")
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"old generation": func(r *ptahv1alpha1.PtahSchema) { r.Status.ObservedGeneration = 1 },
		"not ready":      func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions = nil },
		"still applying": func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} },
		"awaiting observation": func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			reading := base.DeepCopy()
			edit(reading)
			if freshApprovalConverged(reading) {
				t.Fatal("accepted unfinished convergence")
			}
		})
	}
}

func TestRetargetProofKeepsTheDispatchedApplyBinding(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		PendingObservation: &ptahv1alpha1.PendingObservationStatus{
			Outcome:          ptahv1alpha1.PendingObservationOutcomeUnknown,
			ApplyOperationID: "apply-original", ApplyJobUID: "job-original",
			Plan: ptahv1alpha1.CurrentPlanStatus{TargetIdentityDigest: ftDigest},
		},
		Conditions: []metav1.Condition{{
			Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue,
			Reason:  string(ptahv1alpha1.ReasonOperationFailed),
			Message: "post-apply observation target does not match the applied plan",
		}},
	}}
	accepts := func(r *ptahv1alpha1.PtahSchema) bool {
		return retargetProofRefused(r, "apply-original", "job-original", ftDigest)
	}
	ftRefusesEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"proof abandoned", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation = nil }},
		{"another Apply", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation.ApplyOperationID = "another-apply" }},
		{"another Job", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation.ApplyJobUID = "another-job" }},
		{"substituted target accepted", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation.Plan.TargetIdentityDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{"success attributed", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationApplySucceeded
		}},
		{"no refusal", func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions = nil }},
		{"old refusal cleared", func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions[0].Status = metav1.ConditionFalse }},
		{"another failure", func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions[0].Message = "database is unavailable" }},
		{"Apply failure alone", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonApplyOutcomeUnknown)
		}},
	})
	// A retry can change the phase and the active observation while this
	// refusal remains true; neither is evidence that the proof was settled.
	ftAcceptsEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"retry observing", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Phase = ptahv1alpha1.PhaseObserving
			r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
	})
	for _, bindings := range [][3]string{{"", "job-original", ftDigest}, {"apply-original", "", ftDigest}, {"apply-original", "job-original", ""}} {
		if retargetProofRefused(base, bindings[0], bindings[1], bindings[2]) {
			t.Fatal("accepted missing expected proof bindings")
		}
	}
}

func TestTargetPlanWaitsForTheOriginalProofToFinish(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseAwaitingApproval,
		Plan:  &ptahv1alpha1.CurrentPlanStatus{Name: "current-plan", TargetIdentityDigest: ftDigest},
	}}
	ftRefusesEach(t, base.DeepCopy, func(r *ptahv1alpha1.PtahSchema) bool {
		return targetPlanAwaitingApproval(r, ftDigest)
	}, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"proof still owed", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"another target", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Plan.TargetIdentityDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{"no plan", func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan = nil }},
		{"work still active", func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} }},
		{"no approval gate", func(r *ptahv1alpha1.PtahSchema) { r.Status.Phase = ptahv1alpha1.PhasePlanning }},
	})
	if targetPlanAwaitingApproval(base, "") {
		t.Fatal("accepted a missing expected target")
	}
}
