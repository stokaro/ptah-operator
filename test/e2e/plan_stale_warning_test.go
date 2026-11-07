package e2e

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func planStaleFixture() *ptahv1alpha1.PtahSchema {
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Name, schema.UID = "e2e-plan-stale", "schema-uid"
	return schema
}

func planStaleEvent(mutate func(*corev1.Event)) corev1.Event {
	event := corev1.Event{
		InvolvedObject: corev1.ObjectReference{Kind: "PtahSchema", Name: "e2e-plan-stale", UID: "schema-uid"},
		Type:           corev1.EventTypeWarning, Reason: "PlanStale", Count: 1,
		Message: "Plan ptah-plan-old will not be applied: plan ptah-plan-new replaced it before it ran, so an approval of ptah-plan-old no longer counts",
	}
	if mutate != nil {
		mutate(&event)
	}
	return event
}

func TestPlanStaleWarningsCountOnlyThisSchemasWarnings(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*corev1.Event){
		"a schema of the same name and another UID": func(event *corev1.Event) { event.InvolvedObject.UID = "other-uid" },
		"another schema": func(event *corev1.Event) { event.InvolvedObject.Name = "other" },
		"a Job":          func(event *corev1.Event) { event.InvolvedObject.Kind = "Job" },
		"a Normal Event": func(event *corev1.Event) { event.Type = corev1.EventTypeNormal },
		"another reason": func(event *corev1.Event) { event.Reason = "PlanTargetChanged" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if warnings := planStaleWarnings([]corev1.Event{planStaleEvent(mutate)}, planStaleFixture()); len(warnings) != 0 {
				t.Fatalf("planStaleWarnings() counted %v", warnings)
			}
		})
	}
	t.Run("the schema's own", func(t *testing.T) {
		t.Parallel()
		if warnings := planStaleWarnings([]corev1.Event{planStaleEvent(nil)}, planStaleFixture()); len(warnings) != 1 {
			t.Fatalf("planStaleWarnings() = %v, want the one Warning", warnings)
		}
	})
}

func TestPlanReplacedOnRecordHoldsTheJournalToTheReplacement(t *testing.T) {
	t.Parallel()
	published := func(plan, replaced string, asked bool) ptahv1alpha1.JournalEntry {
		return ptahv1alpha1.JournalEntry{Type: ptahv1alpha1.JournalPlanPublished, Plan: plan, Replaced: replaced, ApprovalRequired: asked}
	}
	failed := ptahv1alpha1.JournalEntry{Type: ptahv1alpha1.JournalOperationFailed, Reason: "OperationFailed"}
	for _, row := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		refused bool
	}{
		{
			name:    "the entry that records it",
			journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{published("plan-old", "", true), published("plan-new", "plan-old", true)}},
		},
		{
			name:    "a failed read after it",
			journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{published("plan-new", "plan-old", true), failed}},
		},
		{name: "no journal", refused: true},
		{name: "an empty journal", journal: &ptahv1alpha1.LifecycleJournal{}, refused: true},
		{name: "a journal of no plan", journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{failed}}, refused: true},
		{
			name:    "the new plan with nothing replaced",
			journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{published("plan-new", "", true)}},
			refused: true,
		},
		{
			name:    "the new plan replacing another",
			journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{published("plan-new", "plan-other", true)}},
			refused: true,
		},
		{
			name:    "the new plan asking for nothing",
			journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{published("plan-new", "plan-old", false)}},
			refused: true,
		},
		{
			name:    "another plan published since",
			journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{published("plan-new", "plan-old", true), published("plan-newer", "plan-new", true)}},
			refused: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := planReplacedOnRecord(row.journal, "plan-old", "plan-new")
			if row.refused != (err != nil) {
				t.Fatalf("planReplacedOnRecord() = %v, want refused %t", err, row.refused)
			}
		})
	}
}

func TestPlanWarnedReplacedHoldsTheWarningToOneReplacement(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		warnings []corev1.Event
		refusal  string
	}{
		{name: "the one Warning", warnings: []corev1.Event{planStaleEvent(nil)}},
		{name: "no Warning", refusal: "0 PlanStale"},
		{name: "two Warnings", warnings: []corev1.Event{planStaleEvent(nil), planStaleEvent(nil)}, refusal: "2 PlanStale"},
		{
			// What a Warning on every read looks like once the recorder folds it.
			name:     "one Warning repeated",
			warnings: []corev1.Event{planStaleEvent(func(event *corev1.Event) { event.Count = 34 })},
			refusal:  "recorded 34 times",
		},
		{
			name: "the message of the release that warned on every read",
			warnings: []corev1.Event{planStaleEvent(func(event *corev1.Event) {
				event.Message = "The immutable plan became stale and will not be applied"
			})},
			refusal: "does not name",
		},
		{
			name: "a message naming only the new plan",
			warnings: []corev1.Event{planStaleEvent(func(event *corev1.Event) {
				event.Message = "Plan ptah-plan-new replaced another"
			})},
			refusal: "does not name",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := planWarnedReplaced(row.warnings, "ptah-plan-old", "ptah-plan-new")
			if row.refusal == "" {
				if err != nil {
					t.Fatalf("planWarnedReplaced() = %v, want it accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("planWarnedReplaced() = %v, want a refusal saying %q", err, row.refusal)
			}
		})
	}
}

func TestReadKeptPlanRequiresTheSamePlanAfterALaterRead(t *testing.T) {
	t.Parallel()
	since := metav1.NewTime(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	later := metav1.NewTime(since.Add(30 * time.Second))
	plan := &ptahv1alpha1.CurrentPlanStatus{Name: "ptah-plan-old", UID: "plan-uid", Fingerprint: "sha256:a"}
	waiting := func(mutate func(*ptahv1alpha1.PtahSchema)) *ptahv1alpha1.PtahSchema {
		schema := planStaleFixture()
		schema.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval
		schema.Status.Plan = plan.DeepCopy()
		schema.Status.Target.LastObservedAt = &later
		if mutate != nil {
			mutate(schema)
		}
		return schema
	}
	if !readKeptPlan(waiting(nil), plan, since) {
		t.Fatal("readKeptPlan() refused the same plan waiting after a later read")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"no read since":       func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Target.LastObservedAt = &since },
		"never read":          func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Target.LastObservedAt = nil },
		"mid-read, no plan":   func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Plan = nil },
		"still planning":      func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Phase = ptahv1alpha1.PhasePlanning },
		"another plan":        func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Plan.Name = "ptah-plan-new" },
		"another object":      func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Plan.UID = "other-uid" },
		"another fingerprint": func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Plan.Fingerprint = "sha256:b" },
		"an operation running": func(schema *ptahv1alpha1.PtahSchema) {
			schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if readKeptPlan(waiting(mutate), plan, since) {
				t.Fatal("readKeptPlan() accepted it")
			}
		})
	}
	if !planReplacedAwaitingApproval(waiting(nil), "ptah-plan-other") {
		t.Fatal("planReplacedAwaitingApproval() refused a schema waiting on another plan")
	}
	if planReplacedAwaitingApproval(waiting(nil), plan.Name) {
		t.Fatal("planReplacedAwaitingApproval() accepted the plan it is meant to replace")
	}
	if planReplacedAwaitingApproval(waiting(func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Plan = nil }), plan.Name) {
		t.Fatal("planReplacedAwaitingApproval() accepted a schema with no plan")
	}
}
