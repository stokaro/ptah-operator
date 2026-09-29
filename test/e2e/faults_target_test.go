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
