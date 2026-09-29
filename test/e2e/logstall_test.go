package e2e

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestLogReadBoundUsesRequestTimes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		readings []logStallReading
		accepted bool
	}{
		{"one bounded read", []logStallReading{{State: "started", At: start}, {State: "canceled", At: start.Add(60 * time.Second)}}, true},
		{"no request", nil, false},
		{"only listening", []logStallReading{{State: "listening", At: start}}, false},
		{"read still stuck", []logStallReading{{State: "started", At: start}}, false},
		{"immediate failure", []logStallReading{{State: "started", At: start}, {State: "canceled", At: start.Add(time.Second)}}, false},
		{"late cancellation", []logStallReading{{State: "started", At: start}, {State: "canceled", At: start.Add(76 * time.Second)}}, false},
		{"second read cannot hide an unbounded first", []logStallReading{{State: "started", At: start}, {State: "started", At: start.Add(50 * time.Second)}, {State: "canceled", At: start.Add(110 * time.Second)}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if duration, accepted := logReadDuration(test.readings); accepted != test.accepted {
				t.Fatalf("duration=%s accepted=%t, want %t", duration, accepted, test.accepted)
			}
		})
	}
}

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
