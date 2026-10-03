package e2e

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestSchemaProgressRequiresConvergenceInsideTheMeasuredWindow(t *testing.T) {
	t.Parallel()
	approved := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	deadline := approved.Add(180 * time.Second)
	fixture := &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseInSync, ObservedGeneration: 2,
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
			Applied:    &ptahv1alpha1.AppliedStatus{CompletedAt: metav1.NewTime(approved.Add(90 * time.Second))},
		},
	}
	if !schemaReadProgress(fixture, approved, deadline) {
		t.Fatal("convergence inside the progress target was refused")
	}
	for name, change := range map[string]func(*ptahv1alpha1.PtahSchema){
		"an earlier result": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Applied.CompletedAt = metav1.NewTime(approved.Add(-time.Second))
		},
		"late convergence": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Applied.CompletedAt = metav1.NewTime(deadline.Add(time.Second))
		},
		"no applied plan":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = nil },
		"an older generation": func(s *ptahv1alpha1.PtahSchema) { s.Status.ObservedGeneration-- },
		"still active":        func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} },
		"not ready":           func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := fixture.DeepCopy()
			change(wrong)
			if schemaReadProgress(wrong, approved, deadline) {
				t.Fatal("accepted a reading that does not prove independent progress")
			}
		})
	}
}

func TestDiagnosticLogHeld(t *testing.T) {
	for _, test := range []struct {
		name   string
		states []string
		want   bool
	}{
		{"empty", nil, false}, {"listening only", []string{"listening"}, false},
		{"held", []string{"listening", "started"}, true},
		{"closed", []string{"listening", "started", "canceled"}, false},
		{"flush failed", []string{"started", "flush-failed"}, false},
		{"additional reader", []string{"started", "started"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var readings []logStallReading
			for _, state := range test.states {
				readings = append(readings, logStallReading{State: state})
			}
			if got := diagnosticLogHeld(readings); got != test.want {
				t.Fatalf("held=%v want %v", got, test.want)
			}
		})
	}
}
