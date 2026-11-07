package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// status.plan goes nil on every read of the database and comes back as the same
// object when nothing changed, so a Warning on it going away fired on every
// interval and after every Apply: one resource collected 34 of them against 5
// plans (#820). A reader told to act on a Warning like that learns to ignore it,
// so PlanStale now means another plan took the place of one a reader could have
// approved.

// planStaleEvents drains the recorder and returns the PlanStale Warnings it held.
func planStaleEvents(recorder *record.FakeRecorder) []string {
	var warnings []string
	for len(recorder.Events) > 0 {
		if event := <-recorder.Events; strings.HasPrefix(event, corev1.EventTypeWarning+" PlanStale ") {
			warnings = append(warnings, event)
		}
	}
	return warnings
}

func TestPlanStaleWarnsOnlyWhenAnApprovablePlanWasReplaced(t *testing.T) {
	t.Parallel()

	published := func(plan string) operatorv1alpha1.JournalEntry {
		return operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalPlanPublished, At: metav1.Now(), Plan: plan}
	}
	approved := func(plan string) operatorv1alpha1.JournalEntry {
		return operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApprovalAccepted, At: metav1.Now(), Plan: plan, Approval: "approve-" + plan}
	}
	applied := operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApplyFinished, At: metav1.Now(), Plan: "plan-a"}
	waiting := func(status metav1.ConditionStatus, reason operatorv1alpha1.ConditionReason) func(*operatorv1alpha1.PtahSchema) {
		return func(schema *operatorv1alpha1.PtahSchema) {
			setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, status, reason, "fixture")
		}
	}
	current := func(name string) *operatorv1alpha1.CurrentPlanStatus {
		return &operatorv1alpha1.CurrentPlanStatus{Name: name, UID: "uid-" + types.UID(name), Fingerprint: testDigest}
	}

	for _, row := range []struct {
		name          string
		journal       []operatorv1alpha1.JournalEntry
		approval      func(*operatorv1alpha1.PtahSchema)
		before, after *operatorv1alpha1.CurrentPlanStatus
		wantReplaced  string
	}{
		{
			name:     "a read clears the plan it is waiting on",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval: waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			before:   current("plan-a"),
		},
		{
			name:     "the same plan comes back after the read",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval: waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			after:    current("plan-a"),
		},
		{
			name:         "another plan comes back after the read",
			journal:      []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval:     waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			after:        current("plan-b"),
			wantReplaced: "plan-a",
		},
		{
			name:         "another plan takes its place in one write",
			journal:      []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval:     waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			before:       current("plan-a"),
			after:        current("plan-b"),
			wantReplaced: "plan-a",
		},
		{
			// The Apply claim moved ApprovalRequired to False and was withdrawn
			// before dispatch: the journal is what still says a person approved.
			name:         "an approved plan withdrawn before it ran",
			journal:      []operatorv1alpha1.JournalEntry{published("plan-a"), approved("plan-a")},
			approval:     waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonSatisfied),
			after:        current("plan-b"),
			wantReplaced: "plan-a",
		},
		{
			// An approval of an earlier plan says nothing about this one.
			name:     "a plan Always claimed, after an earlier plan was approved",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-z"), approved("plan-z"), published("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonSatisfied),
			after:    current("plan-b"),
		},
		{
			name:     "a plan apply policy Always never waited on",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonNotRequired),
			after:    current("plan-b"),
		},
		{
			name:     "a plan apply policy Never only records",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonApplyDisabled),
			after:    current("plan-b"),
		},
		{
			name:     "a plan allowDestructive blocks",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonDestructiveChangesDisabled),
			after:    current("plan-b"),
		},
		{
			name:     "the plan after one an Apply ran",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a"), approved("plan-a"), applied},
			approval: waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			after:    current("plan-b"),
		},
		{
			name:     "the first plan a resource publishes",
			approval: waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			after:    current("plan-a"),
		},
		{
			// A journal that lost its entry to a bound, or a resource from before
			// the journal, names no plan to have replaced.
			name:     "a journal that names no plan",
			journal:  []operatorv1alpha1.JournalEntry{{Type: operatorv1alpha1.JournalOperationFailed, At: metav1.Now(), Reason: "OperationFailed"}},
			approval: waiting(metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady),
			after:    current("plan-a"),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			schema.Status.Plan = row.before
			if row.journal != nil {
				schema.Status.Journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: metav1.Now(), Entries: row.journal}
			}
			row.approval(schema)
			reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder

			after := schema.DeepCopy()
			after.Status.Plan = row.after
			after.Status.Phase = operatorv1alpha1.PhaseObserving
			// The write that publishes a plan moves ApprovalRequired to what the
			// new plan asks, and the decision is about the plan it replaced.
			flipped := metav1.ConditionTrue
			if meta.IsStatusConditionTrue(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired) {
				flipped = metav1.ConditionFalse
			}
			setCondition(after, operatorv1alpha1.ConditionApprovalRequired, flipped, operatorv1alpha1.ReasonPlanReady, "the new plan")
			if err := reconciler.patchStatus(context.Background(), schema, after); err != nil {
				t.Fatalf("patchStatus() error = %v", err)
			}

			warnings := planStaleEvents(recorder)
			if row.wantReplaced == "" {
				if len(warnings) != 0 {
					t.Fatalf("PlanStale Warnings = %q, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "Plan "+row.wantReplaced+" will not be applied") ||
				!strings.Contains(warnings[0], "plan "+row.after.Name+" replaced it") {
				t.Fatalf("PlanStale Warnings = %q, want one naming %s and %s", warnings, row.wantReplaced, row.after.Name)
			}
		})
	}
}

// The rows above hand patchStatus each transition. This one walks a waiting plan
// through the harvests that really clear and republish it, so a change to what
// those writes carry cannot leave the table describing a state nothing reaches.
func TestAScheduledReadWarnsOnlyWhenItChangesTheWaitingPlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema := planHarvestFixture(t, operatorv1alpha1.ApplyPolicyOnApproval, safetyPlanDocument(t, "observed-state"))
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder

	claim := func(operation operatorv1alpha1.OperationType, id string) *operatorv1alpha1.PtahSchema {
		t.Helper()
		claimed := safetyGetSchema(t, api, schema)
		claimed.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operation, ID: id, JobName: id + "-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
		}
		bindActiveInput(t, claimed)
		if err := api.Status().Update(ctx, claimed); err != nil {
			t.Fatalf("store the %s claim: %v", operation, err)
		}
		return safetyGetSchema(t, api, schema)
	}
	read := func(id string) {
		t.Helper()
		observing := claim(operatorv1alpha1.OperationObserve, id)
		if _, err := reconciler.consumeResult(ctx, observing, nil, runner.Result{
			Operation: runner.OperationObserve, CoordinationDigest: testCoordinationDigest,
			TargetIdentityDigest: testDigest, DriftReportDigest: testDigest, ObservedDialect: "postgresql",
			ObservedDrift: true, HighestDriftSeverity: "safe", DriftFindingCount: 1,
			DriftFindings: []runner.DriftFindingSummary{{Category: "tables_added", Count: 1, Severity: "safe"}},
		}, nil, 0); err != nil {
			t.Fatalf("harvest Observe %s: %v", id, err)
		}
		if plan := safetyGetSchema(t, api, schema).Status.Plan; plan != nil {
			t.Fatalf("Observe %s left plan %s in status; this row is about the read clearing it", id, plan.Name)
		}
	}
	plan := func(id, observed string) string {
		t.Helper()
		planning := claim(operatorv1alpha1.OperationPlan, id)
		document := safetyPlanDocument(t, observed)
		if _, err := reconciler.consumeResultWithTransport(ctx, planning, nil, runner.Result{
			Operation: runner.OperationPlan, Stdout: string(document),
			CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
			PlanContentDigest: fingerprint.DigestBytes(document), PlanOutcome: runner.PlanOutcomeChanges,
		}, nil, 0, true); err != nil {
			t.Fatalf("harvest Plan %s: %v", id, err)
		}
		published := safetyGetSchema(t, api, schema)
		if published.Status.Plan == nil || published.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
			t.Fatalf("Plan %s left phase %s and plan %v, want a plan awaiting approval", id, published.Status.Phase, published.Status.Plan)
		}
		return published.Status.Plan.Name
	}

	first := plan("plan-1", "observed-state")
	if warnings := planStaleEvents(recorder); len(warnings) != 0 {
		t.Fatalf("the first plan recorded %q", warnings)
	}

	read("observe-2")
	if warnings := planStaleEvents(recorder); len(warnings) != 0 {
		t.Fatalf("clearing a waiting plan to read the database recorded %q", warnings)
	}
	if same := plan("plan-2", "observed-state"); same != first {
		t.Fatalf("the unchanged plan came back as %s, not %s; this row needs the same plan", same, first)
	}
	if warnings := planStaleEvents(recorder); len(warnings) != 0 {
		t.Fatalf("publishing the same plan again recorded %q", warnings)
	}

	read("observe-3")
	second := plan("plan-3", "moved-state")
	if second == first {
		t.Fatalf("a plan from another observed state kept the name %s; this row needs a replacement", first)
	}
	warnings := planStaleEvents(recorder)
	if len(warnings) != 1 || !strings.Contains(warnings[0], first) || !strings.Contains(warnings[0], second) {
		t.Fatalf("PlanStale Warnings = %q, want one naming %s and %s", warnings, first, second)
	}

	// The write that names the new plan is the only one that replaces anything.
	stored := safetyGetSchema(t, api, schema)
	updated := stored.DeepCopy()
	updated.Status.LastAttemptTime = &metav1.Time{Time: metav1.Now().Time}
	if err := reconciler.patchStatus(ctx, stored, updated); err != nil {
		t.Fatal(err)
	}
	if warnings := planStaleEvents(recorder); len(warnings) != 0 {
		t.Fatalf("a later write naming the same plan recorded %q", warnings)
	}
}
