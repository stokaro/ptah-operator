package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
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
	asked := func(plan string) operatorv1alpha1.JournalEntry {
		entry := published(plan)
		entry.ApprovalRequired = true
		return entry
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
			// A realm refusal moves ApprovalRequired to False and leaves the plan
			// where it was, so the plan's entry says what it asked.
			name:         "a plan a contested realm withheld",
			journal:      []operatorv1alpha1.JournalEntry{asked("plan-a")},
			approval:     waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonRealmConflict),
			after:        current("plan-b"),
			wantReplaced: "plan-a",
		},
		{
			name:         "a plan a realm that does not admit the resource withheld",
			journal:      []operatorv1alpha1.JournalEntry{asked("plan-a")},
			approval:     waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonRealmNotAuthorized),
			after:        current("plan-b"),
			wantReplaced: "plan-a",
		},
		{
			name:     "a plan that asked for nothing before a realm refusal",
			journal:  []operatorv1alpha1.JournalEntry{published("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonRealmConflict),
			after:    current("plan-b"),
		},
		{
			// Only the refusals answer from the entry. An execution binding change
			// retires the plan itself and says so.
			name:     "a plan an execution binding change retired",
			journal:  []operatorv1alpha1.JournalEntry{asked("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonExecutionBindingChanged),
			after:    current("plan-b"),
		},
		{
			name:     "a reason nobody has decided about",
			journal:  []operatorv1alpha1.JournalEntry{asked("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ConditionReason("SomethingNew")),
			after:    current("plan-b"),
		},
		{
			name:     "a plan the database converged past",
			journal:  []operatorv1alpha1.JournalEntry{asked("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonNoChanges),
			after:    current("plan-b"),
		},
		{
			name:     "an approved plan the database converged past",
			journal:  []operatorv1alpha1.JournalEntry{asked("plan-a"), approved("plan-a")},
			approval: waiting(metav1.ConditionFalse, operatorv1alpha1.ReasonNoChanges),
			after:    current("plan-b"),
		},
		{
			// A resource from before the journal names no plan to have replaced.
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

// planWalk drives a schema through the harvests that really clear and publish its
// plan, so a change to what those writes carry cannot leave a row describing a
// state nothing reaches.
type planWalk struct {
	t          *testing.T
	reconciler *SchemaReconciler
	api        client.Client
	schema     *operatorv1alpha1.PtahSchema
	recorder   *record.FakeRecorder
}

func newPlanWalk(t *testing.T, apply operatorv1alpha1.ApplyPolicy) *planWalk {
	t.Helper()
	reconciler, api, schema := planHarvestFixture(t, apply, safetyPlanDocument(t, "observed-state"))
	recorder := record.NewFakeRecorder(4096)
	reconciler.Recorder = recorder
	return &planWalk{t: t, reconciler: reconciler, api: api, schema: schema, recorder: recorder}
}

func (w *planWalk) stored() *operatorv1alpha1.PtahSchema {
	w.t.Helper()
	return safetyGetSchema(w.t, w.api, w.schema)
}

func (w *planWalk) claim(operation operatorv1alpha1.OperationType, id string) *operatorv1alpha1.PtahSchema {
	w.t.Helper()
	claimed := w.stored()
	claimed.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operation, ID: id, JobName: id + "-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(w.t, claimed)
	if err := w.api.Status().Update(context.Background(), claimed); err != nil {
		w.t.Fatalf("store the %s claim: %v", operation, err)
	}
	return w.stored()
}

// read ends an Observe, which clears the plan the schema was waiting on.
func (w *planWalk) read(id string) {
	w.t.Helper()
	if _, err := w.reconciler.consumeResult(context.Background(), w.claim(operatorv1alpha1.OperationObserve, id), nil, runner.Result{
		Operation: runner.OperationObserve, CoordinationDigest: testCoordinationDigest,
		TargetIdentityDigest: testDigest, DriftReportDigest: testDigest, ObservedDialect: "postgresql",
		ObservedDrift: true, HighestDriftSeverity: "safe", DriftFindingCount: 1,
		DriftFindings: []runner.DriftFindingSummary{{Category: "tables_added", Count: 1, Severity: "safe"}},
	}, nil, 0); err != nil {
		w.t.Fatalf("harvest Observe %s: %v", id, err)
	}
	if plan := w.stored().Status.Plan; plan != nil {
		w.t.Fatalf("Observe %s left plan %s in status; this row is about the read clearing it", id, plan.Name)
	}
}

// publish ends a Plan that computed document, and returns what that write stored.
func (w *planWalk) publish(id string, document []byte) *operatorv1alpha1.PtahSchema {
	w.t.Helper()
	if _, err := w.reconciler.consumeResultWithTransport(context.Background(), w.claim(operatorv1alpha1.OperationPlan, id), nil, runner.Result{
		Operation: runner.OperationPlan, Stdout: string(document),
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		PlanContentDigest: fingerprint.DigestBytes(document), PlanOutcome: runner.PlanOutcomeChanges,
	}, nil, 0, true); err != nil {
		w.t.Fatalf("harvest Plan %s: %v", id, err)
	}
	published := w.stored()
	if published.Status.Plan == nil {
		w.t.Fatalf("Plan %s published nothing: phase %s", id, published.Status.Phase)
	}
	return published
}

// waitingPlan ends a Plan that leaves a plan waiting for approval, and names it.
func (w *planWalk) waitingPlan(id, observed string) string {
	w.t.Helper()
	published := w.publish(id, safetyPlanDocument(w.t, observed))
	if published.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
		w.t.Fatalf("Plan %s left phase %s, want a plan awaiting approval", id, published.Status.Phase)
	}
	return published.Status.Plan.Name
}

// converge ends a Plan that found nothing to do, and returns what it stored.
func (w *planWalk) converge(id string) *operatorv1alpha1.PtahSchema {
	w.t.Helper()
	if _, err := w.reconciler.consumeResultWithTransport(context.Background(), w.claim(operatorv1alpha1.OperationPlan, id), nil, runner.Result{
		Operation: runner.OperationPlan, CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		PlanOutcome: runner.PlanOutcomeNoChanges,
	}, nil, 0, true); err != nil {
		w.t.Fatalf("harvest Plan %s: %v", id, err)
	}
	converged := w.stored()
	if converged.Status.Phase != operatorv1alpha1.PhaseInSync || converged.Status.Plan != nil {
		w.t.Fatalf("Plan %s left phase %s and plan %v, want InSync and no plan", id, converged.Status.Phase, converged.Status.Plan)
	}
	return converged
}

// assertReplacement holds the write that published replacement to the replacement
// it made: one Warning naming both plans and an entry that records it, or neither.
func (w *planWalk) assertReplacement(published *operatorv1alpha1.PtahSchema, replaced string) {
	w.t.Helper()
	entry := newestPlanEntry(w.t, published)
	if entry.Plan != published.Status.Plan.Name || entry.Replaced != replaced {
		w.t.Fatalf("the newest PlanPublished entry = %+v, want plan %s replacing %q", entry, published.Status.Plan.Name, replaced)
	}
	warnings := planStaleEvents(w.recorder)
	if replaced == "" {
		if len(warnings) != 0 {
			w.t.Fatalf("PlanStale Warnings = %q, want none", warnings)
		}
		return
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], replaced) || !strings.Contains(warnings[0], published.Status.Plan.Name) {
		w.t.Fatalf("PlanStale Warnings = %q, want one naming %s and %s", warnings, replaced, published.Status.Plan.Name)
	}
}

func newestPlanEntry(t *testing.T, schema *operatorv1alpha1.PtahSchema) operatorv1alpha1.JournalEntry {
	t.Helper()
	if schema.Status.Journal != nil {
		for index := len(schema.Status.Journal.Entries) - 1; index >= 0; index-- {
			if entry := schema.Status.Journal.Entries[index]; entry.Type == operatorv1alpha1.JournalPlanPublished {
				return entry
			}
		}
	}
	t.Fatalf("the journal holds no PlanPublished entry: %+v", schema.Status.Journal)
	return operatorv1alpha1.JournalEntry{}
}

func TestAScheduledReadWarnsOnlyWhenItChangesTheWaitingPlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)

	first := walk.waitingPlan("plan-1", "observed-state")
	if warnings := planStaleEvents(walk.recorder); len(warnings) != 0 {
		t.Fatalf("the first plan recorded %q", warnings)
	}

	walk.read("observe-2")
	if warnings := planStaleEvents(walk.recorder); len(warnings) != 0 {
		t.Fatalf("clearing a waiting plan to read the database recorded %q", warnings)
	}
	if same := walk.waitingPlan("plan-2", "observed-state"); same != first {
		t.Fatalf("the unchanged plan came back as %s, not %s; this row needs the same plan", same, first)
	}
	if warnings := planStaleEvents(walk.recorder); len(warnings) != 0 {
		t.Fatalf("publishing the same plan again recorded %q", warnings)
	}

	walk.read("observe-3")
	second := walk.waitingPlan("plan-3", "moved-state")
	if second == first {
		t.Fatalf("a plan from another observed state kept the name %s; this row needs a replacement", first)
	}
	walk.assertReplacement(walk.stored(), first)

	// The write that names the new plan is the only one that replaces anything.
	stored := walk.stored()
	updated := stored.DeepCopy()
	updated.Status.LastAttemptTime = &metav1.Time{Time: metav1.Now().Time}
	if err := walk.reconciler.patchStatus(ctx, stored, updated); err != nil {
		t.Fatal(err)
	}
	if warnings := planStaleEvents(walk.recorder); len(warnings) != 0 {
		t.Fatalf("a later write naming the same plan recorded %q", warnings)
	}
}

// A realm refusal closes the approval boundary and moves ApprovalRequired to
// False. When the refusal lifts, a read that finds the database changed publishes
// another plan with that condition still False, and a person who could have
// approved the first plan still has to hear about it. The rows that stay quiet
// hold the same walk to the plans nobody could have approved.
func TestARealmRefusalDoesNotHideAReplacement(t *testing.T) {
	t.Parallel()

	type document func(t *testing.T, observed string) []byte
	plain := document(func(t *testing.T, observed string) []byte { return safetyPlanDocument(t, observed) })
	destructive := document(func(t *testing.T, observed string) []byte { return safetyPlanDocument(t, observed, true) })
	granting := document(func(t *testing.T, observed string) []byte {
		return safetyPlanDocumentWithStatement(t, observed, `GRANT SELECT ON TABLE "public"."orders" TO PUBLIC`)
	})

	for _, row := range []struct {
		name         string
		apply        operatorv1alpha1.ApplyPolicy
		document     document
		reason       operatorv1alpha1.ConditionReason
		refusedEmpty bool
		wantAsked    bool
	}{
		{
			name: "a plan waiting under OnApproval, in a contested realm", apply: operatorv1alpha1.ApplyPolicyOnApproval,
			document: plain, reason: operatorv1alpha1.ReasonRealmConflict, wantAsked: true,
		},
		{
			name: "a plan waiting under OnApproval, in a realm that does not admit the resource", apply: operatorv1alpha1.ApplyPolicyOnApproval,
			document: plain, reason: operatorv1alpha1.ReasonRealmNotAuthorized, wantAsked: true,
		},
		{
			// Only the plan's own entry knows this one asked: Always asks because
			// of what the plan does, and status.plan is gone by the time B arrives.
			name: "a privilege change Always waits on", apply: operatorv1alpha1.ApplyPolicyAlways,
			document: granting, reason: operatorv1alpha1.ReasonRealmConflict, wantAsked: true,
		},
		{
			// A failed read leaves status.plan empty for as long as the database is
			// away, so nothing could put the condition back from the plan.
			name: "a refusal that arrives with no plan in status", apply: operatorv1alpha1.ApplyPolicyOnApproval,
			document: plain, reason: operatorv1alpha1.ReasonRealmConflict, refusedEmpty: true, wantAsked: true,
		},
		{
			name: "a plan Always would apply on its own", apply: operatorv1alpha1.ApplyPolicyAlways,
			document: plain, reason: operatorv1alpha1.ReasonRealmConflict,
		},
		{
			name: "a plan Never only records", apply: operatorv1alpha1.ApplyPolicyNever,
			document: plain, reason: operatorv1alpha1.ReasonRealmConflict,
		},
		{
			name: "a plan allowDestructive blocks", apply: operatorv1alpha1.ApplyPolicyOnApproval,
			document: destructive, reason: operatorv1alpha1.ReasonRealmNotAuthorized,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			walk := newPlanWalk(t, row.apply)
			first := walk.publish("plan-1", row.document(t, "observed-state")).Status.Plan.Name
			if entry := newestPlanEntry(t, walk.stored()); entry.ApprovalRequired != row.wantAsked {
				t.Fatalf("the first plan's entry = %+v, want approvalRequired %t", entry, row.wantAsked)
			}
			planStaleEvents(walk.recorder)
			if row.refusedEmpty {
				walk.read("observe-before-refusal")
			}

			if _, err := walk.reconciler.schemaRealmBlocked(context.Background(), walk.stored(), realmRefusal{
				Reason: row.reason, Message: "refused", Approval: "withheld", Apply: "withheld",
			}); err != nil {
				t.Fatalf("schemaRealmBlocked() error = %v", err)
			}
			refused := walk.stored()
			held := meta.FindStatusCondition(refused.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
			if refused.Status.Phase != operatorv1alpha1.PhaseBlocked || held == nil ||
				held.Status != metav1.ConditionFalse || held.Reason != string(row.reason) {
				t.Fatalf("the refusal left phase %s and ApprovalRequired %+v, want Blocked and False/%s", refused.Status.Phase, held, row.reason)
			}

			walk.read("observe-after-refusal")
			published := walk.publish("plan-2", row.document(t, "moved-state"))
			if published.Status.Plan.Name == first {
				t.Fatalf("a plan from another observed state kept the name %s; this row needs a replacement", first)
			}
			want := ""
			if row.wantAsked {
				want = first
			}
			walk.assertReplacement(published, want)
		})
	}
}

// The recorder's spam filter admits 25 Warnings per object and then one every five
// minutes, so a read that fails for half an hour leaves little for the Warning
// that follows, and a manager killed after the status write sends nothing. Nothing
// derives this Warning a second time, so the write that replaces a plan has to
// record the replacement itself.
func TestALostWarningLeavesTheReplacementOnRecord(t *testing.T) {
	t.Parallel()

	walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
	// A recorder with no channel loses every Event it is given.
	walk.reconciler.Recorder = &record.FakeRecorder{}
	first := walk.waitingPlan("plan-1", "observed-state")
	if entry := newestPlanEntry(t, walk.stored()); entry.Plan != first || entry.Replaced != "" || !entry.ApprovalRequired {
		t.Fatalf("the first plan's entry = %+v, want plan %s, asking for an approval, replacing nothing", entry, first)
	}

	walk.read("observe-2")
	second := walk.waitingPlan("plan-2", "moved-state")
	if entry := newestPlanEntry(t, walk.stored()); entry.Plan != second || entry.Replaced != first || !entry.ApprovalRequired {
		t.Fatalf("the second plan's entry = %+v, want plan %s replacing %s", entry, second, first)
	}
}

// A plan can wait for a person longer than the journal's bounds last. One failed
// read after 31 days, or 64 failures that do not fold, would otherwise take the
// only record of what a later plan replaces.
func TestAJournalBoundKeepsTheWaitingPlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	failedRead := func(t *testing.T, walk *planWalk, operation operatorv1alpha1.OperationType, id string) {
		t.Helper()
		if _, err := walk.reconciler.retryOperation(ctx, walk.claim(operation, id), nil, fmt.Errorf("database down")); err != nil {
			t.Fatalf("retryOperation(%s) error = %v", id, err)
		}
	}
	for _, row := range []struct {
		name string
		age  func(t *testing.T, walk *planWalk)
	}{
		{
			name: "past the age bound",
			age: func(t *testing.T, walk *planWalk) {
				t.Helper()
				stored := walk.stored()
				old := metav1.NewTime(time.Now().Add(-lifecyclejournal.MaxAge - 24*time.Hour))
				stored.Status.Journal.CoveredSince = old
				for index := range stored.Status.Journal.Entries {
					stored.Status.Journal.Entries[index].At = old
				}
				if err := walk.api.Status().Update(ctx, stored); err != nil {
					t.Fatal(err)
				}
				failedRead(t, walk, operatorv1alpha1.OperationObserve, "observe-failed")
			},
		},
		{
			name: "past the entry bound",
			age: func(t *testing.T, walk *planWalk) {
				t.Helper()
				for index := range lifecyclejournal.MaxEntries {
					// Taking turns keeps the failures from folding into one entry.
					operation := operatorv1alpha1.OperationObserve
					if index%2 == 1 {
						operation = operatorv1alpha1.OperationResolve
					}
					failedRead(t, walk, operation, fmt.Sprintf("failed-%d", index))
					cleared := walk.stored()
					clearFailure(cleared)
					cleared.Status.ActiveOperation = nil
					if err := walk.api.Status().Update(ctx, cleared); err != nil {
						t.Fatal(err)
					}
				}
				if count := len(walk.stored().Status.Journal.Entries); count != lifecyclejournal.MaxEntries {
					t.Fatalf("the journal holds %d entries, want the bound of %d", count, lifecyclejournal.MaxEntries)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
			first := walk.waitingPlan("plan-1", "observed-state")
			planStaleEvents(walk.recorder)
			row.age(t, walk)
			if plan, _ := lifecyclejournal.OutstandingPlan(walk.stored().Status.Journal); plan != first {
				t.Fatalf("the journal's outstanding plan = %q after the failures, want %s", plan, first)
			}

			walk.read("observe-3")
			walk.waitingPlan("plan-3", "moved-state")
			walk.assertReplacement(walk.stored(), first)
		})
	}
}

// A plan the database converged past was not replaced by whatever drift comes
// next, however long after, and nobody can approve it meanwhile.
func TestAConvergedDatabaseEndsTheWaitForApproval(t *testing.T) {
	t.Parallel()

	approvalRequired := func(t *testing.T, schema *operatorv1alpha1.PtahSchema) *metav1.Condition {
		t.Helper()
		condition := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
		if condition == nil {
			t.Fatal("no ApprovalRequired condition")
		}
		return condition
	}
	assertNothingWaiting := func(t *testing.T, schema *operatorv1alpha1.PtahSchema) {
		t.Helper()
		if condition := approvalRequired(t, schema); condition.Status != metav1.ConditionFalse || condition.Reason != string(operatorv1alpha1.ReasonNoChanges) {
			t.Fatalf("ApprovalRequired after converging = %s/%s, want False/NoChanges", condition.Status, condition.Reason)
		}
	}

	t.Run("a waiting plan, then a later drift", func(t *testing.T) {
		t.Parallel()
		walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
		first := walk.waitingPlan("plan-1", "observed-state")
		planStaleEvents(walk.recorder)

		walk.read("observe-2")
		assertNothingWaiting(t, walk.converge("plan-none"))

		walk.read("observe-3")
		second := walk.waitingPlan("plan-3", "moved-state")
		if second == first {
			t.Fatalf("a plan from another observed state kept the name %s; this row needs another plan", first)
		}
		walk.assertReplacement(walk.stored(), "")
		if condition := approvalRequired(t, walk.stored()); condition.Status != metav1.ConditionTrue {
			t.Fatalf("ApprovalRequired for the new plan = %s/%s, want True", condition.Status, condition.Reason)
		}
	})

	t.Run("a waiting plan that comes back", func(t *testing.T) {
		t.Parallel()
		walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
		first := walk.waitingPlan("plan-1", "observed-state")
		walk.read("observe-2")
		assertNothingWaiting(t, walk.converge("plan-none"))

		walk.read("observe-3")
		if same := walk.waitingPlan("plan-3", "observed-state"); same != first {
			t.Fatalf("the unchanged plan came back as %s, not %s; this row needs the same plan", same, first)
		}
		if condition := approvalRequired(t, walk.stored()); condition.Status != metav1.ConditionTrue {
			t.Fatalf("ApprovalRequired for the plan that came back = %s/%s, want True", condition.Status, condition.Reason)
		}
		if warnings := planStaleEvents(walk.recorder); len(warnings) != 0 {
			t.Fatalf("the same plan coming back recorded %q", warnings)
		}
	})

	t.Run("an approved plan withdrawn before it ran", func(t *testing.T) {
		t.Parallel()
		walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
		first := walk.waitingPlan("plan-1", "observed-state")
		planStaleEvents(walk.recorder)
		// What an Apply claim leaves when it is withdrawn before dispatch.
		withdrawn := walk.stored()
		withdrawn.Status.Journal.Entries = append(withdrawn.Status.Journal.Entries, operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalApprovalAccepted, At: metav1.Now(), Plan: first, Approval: "approve-first", ApprovalUID: "approval-uid",
		})
		setCondition(withdrawn, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonSatisfied, "fixture")
		if err := walk.api.Status().Update(context.Background(), withdrawn); err != nil {
			t.Fatal(err)
		}

		walk.read("observe-2")
		assertNothingWaiting(t, walk.converge("plan-none"))
		walk.read("observe-3")
		walk.waitingPlan("plan-3", "moved-state")
		walk.assertReplacement(walk.stored(), "")
	})

	t.Run("a plan Never only records", func(t *testing.T) {
		t.Parallel()
		walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyNever)
		walk.publish("plan-1", safetyPlanDocument(t, "observed-state"))
		walk.read("observe-2")
		// A reader told the policy holds a plan that is gone would edit the policy.
		assertNothingWaiting(t, walk.converge("plan-none"))
	})

	// An Apply ended the plan, and every later read finds nothing. The reason the
	// claim wrote still says why no approval is asked for, so no read rewrites it.
	t.Run("a plan an Apply carried out", func(t *testing.T) {
		t.Parallel()
		walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
		first := walk.waitingPlan("plan-1", "observed-state")
		applied := walk.stored()
		applied.Status.Journal.Entries = append(applied.Status.Journal.Entries, operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalApplyFinished, At: metav1.Now(), Plan: first, Outcome: operatorv1alpha1.JournalSucceeded,
		})
		setCondition(applied, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonSatisfied, "fixture")
		if err := walk.api.Status().Update(context.Background(), applied); err != nil {
			t.Fatal(err)
		}

		walk.read("observe-2")
		if condition := approvalRequired(t, walk.converge("plan-none")); condition.Status != metav1.ConditionFalse || condition.Reason != string(operatorv1alpha1.ReasonSatisfied) {
			t.Fatalf("ApprovalRequired after a later read = %s/%s, want False/Satisfied", condition.Status, condition.Reason)
		}
	})
}
