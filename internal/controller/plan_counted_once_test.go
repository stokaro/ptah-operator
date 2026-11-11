package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// A waiting plan is read again at every interval. The Observe that starts the
// read clears status.plan, and the Plan that ends it publishes the same plan
// when the database did not change. The plan counter, the required-approval
// counter and the ApprovalRequired Event compared the status before a write
// with the status after it, so the plan coming back read as a new one: one
// resource recorded 5 ApprovalRequired Events across 2 plans (#871).

// scheduledWalk is a planWalk on a clock the row moves, so the read starts where
// the controller starts it: at the deadline the waiting plan persisted.
type scheduledWalk struct {
	*planWalk
	clock        *fixedClock
	observations *telemetryObservation
	events       map[string]int
}

func newScheduledWalk(t *testing.T) *scheduledWalk {
	t.Helper()
	walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
	clock := &fixedClock{now: walk.reconciler.now()}
	walk.reconciler.Clock = clock.Now
	walk.reconciler.Locks = targetlock.New(walk.api, walk.api, clock)
	observations := &telemetryObservation{}
	walk.reconciler.Telemetry = observations
	// What the Resolve that first read this artifact recorded. Without it the
	// next Resolve reads the same digest as a changed source and clears the plan
	// on its own, ahead of the Observe these rows are about.
	stored := walk.stored()
	stored.Status.Source.RequestedReference = stored.Spec.Desired.OCIRef
	if err := walk.api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	return &scheduledWalk{planWalk: walk, clock: clock, observations: observations, events: map[string]int{}}
}

// eventCount drains the recorder into the Events the walk has recorded so far,
// and returns how many carry reason.
func (w *scheduledWalk) eventCount(eventType, reason string) int {
	for len(w.recorder.Events) > 0 {
		fields := strings.SplitN(<-w.recorder.Events, " ", 3)
		w.events[fields[0]+" "+fields[1]]++
	}
	return w.events[eventType+" "+reason]
}

// wait is the pass before the deadline that finds no approval for the plan.
func (w *scheduledWalk) wait() {
	w.t.Helper()
	plan := w.stored().Status.Plan
	if _, err := w.reconciler.Reconcile(context.Background(), requestFor(w.schema)); err != nil {
		w.t.Fatalf("the pass before the deadline: %v", err)
	}
	waiting := w.stored()
	asked := meta.FindStatusCondition(waiting.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if waiting.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval || waiting.Status.ActiveOperation != nil ||
		waiting.Status.Plan == nil || plan == nil || waiting.Status.Plan.Name != plan.Name ||
		asked == nil || asked.Status != metav1.ConditionTrue || asked.Reason != string(operatorv1alpha1.ReasonWaiting) {
		w.t.Fatalf("the pass before the deadline left phase %s, claim %+v, plan %+v and ApprovalRequired %+v; want plan %+v waiting",
			waiting.Status.Phase, waiting.Status.ActiveOperation, waiting.Status.Plan, asked, plan)
	}
}

// claimed runs the pass that has to claim operation, and returns what it stored.
func (w *scheduledWalk) claimed(operation operatorv1alpha1.OperationType) *operatorv1alpha1.PtahSchema {
	w.t.Helper()
	if _, err := w.reconciler.Reconcile(context.Background(), requestFor(w.schema)); err != nil {
		w.t.Fatalf("the pass that claims %s: %v", operation, err)
	}
	claimed := w.stored()
	if claim := claimed.Status.ActiveOperation; claim == nil || claim.Type != operation {
		w.t.Fatalf("phase %s claimed %+v, want %s", claimed.Status.Phase, claim, operation)
	}
	return claimed
}

// scheduledRead moves the clock past the deadline the waiting plan persisted and
// reads the database again. It returns the name of the plan that read published.
func (w *scheduledWalk) scheduledRead(document []byte) string {
	w.t.Helper()
	waiting := w.stored()
	if waiting.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval || waiting.Status.NextReconciliationTime == nil {
		w.t.Fatalf("phase %s with deadline %v, want a plan waiting until a deadline", waiting.Status.Phase, waiting.Status.NextReconciliationTime)
	}
	w.clock.now = waiting.Status.NextReconciliationTime.Add(time.Second)
	published := w.refresh(document)
	if published.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
		w.t.Fatalf("the read ended in phase %s with plan %s, want a plan awaiting approval", published.Status.Phase, published.Status.Plan.Name)
	}
	return published.Status.Plan.Name
}

// refresh answers each claim the controller makes until it publishes a plan
// again: a Resolve and a Verify of the same artifact, an Observe, and a Plan of
// document. It returns what the Plan stored.
func (w *scheduledWalk) refresh(document []byte) *operatorv1alpha1.PtahSchema {
	w.t.Helper()
	ctx := context.Background()
	if _, err := w.reconciler.consumeResult(ctx, w.claimed(operatorv1alpha1.OperationResolve), nil, runner.Result{
		Operation: runner.OperationResolve, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	}, nil, 0); err != nil {
		w.t.Fatalf("harvest Resolve: %v", err)
	}
	verify := w.claimed(operatorv1alpha1.OperationVerify)
	if _, err := w.reconciler.consumeResult(ctx, verify, nil, runner.Result{
		Operation: runner.OperationVerify, ResolvedDigest: testDigest, ObservedArtifactType: dataplane.SchemaArtifactType,
		VerificationPolicyDigest: verify.Status.ActiveOperation.VerificationPolicyDigest,
	}, nil, 0); err != nil {
		w.t.Fatalf("harvest Verify: %v", err)
	}
	if _, err := w.reconciler.consumeResult(ctx, w.claimed(operatorv1alpha1.OperationObserve), nil, runner.Result{
		Operation: runner.OperationObserve, CoordinationDigest: testCoordinationDigest,
		TargetIdentityDigest: testDigest, DriftReportDigest: testDigest, ObservedDialect: "postgresql",
		ObservedDrift: true, HighestDriftSeverity: "safe", DriftFindingCount: 1,
		DriftFindings: []runner.DriftFindingSummary{{Category: "tables_added", Count: 1, Severity: "safe"}},
	}, nil, 0); err != nil {
		w.t.Fatalf("harvest Observe: %v", err)
	}
	if plan := w.stored().Status.Plan; plan != nil {
		w.t.Fatalf("the Observe left plan %s in status; this row is about the read clearing it", plan.Name)
	}
	if _, err := w.reconciler.consumeResultWithTransport(ctx, w.claimed(operatorv1alpha1.OperationPlan), nil, runner.Result{
		Operation: runner.OperationPlan, Stdout: string(document),
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		PlanContentDigest: fingerprint.DigestBytes(document), PlanOutcome: runner.PlanOutcomeChanges,
	}, nil, 0, true); err != nil {
		w.t.Fatalf("harvest Plan: %v", err)
	}
	published := w.stored()
	if published.Status.Plan == nil {
		w.t.Fatalf("the read ended in phase %s with no plan", published.Status.Phase)
	}
	return published
}

// assertCounted holds the walk to the plans it published and the times a plan
// asked for an approval: one plan observation for each plan, and one
// ApprovalRequired Event and one required approval each time one asked.
func (w *scheduledWalk) assertCounted(step string, plans, asked int) {
	w.t.Helper()
	if events := w.eventCount(corev1.EventTypeNormal, "ApprovalRequired"); events != asked {
		w.t.Errorf("%s: %d ApprovalRequired Events, want %d", step, events, asked)
	}
	if want := slices.Repeat([]telemetry.PlanImpact{telemetry.PlanNonDestructive}, plans); !slices.Equal(w.observations.plans, want) {
		w.t.Errorf("%s: plan observations = %q, want %q", step, w.observations.plans, want)
	}
	if want := slices.Repeat([]telemetry.ApprovalOutcome{telemetry.ApprovalRequired}, asked); !slices.Equal(w.observations.approvals, want) {
		w.t.Errorf("%s: approval observations = %q, want %q", step, w.observations.approvals, want)
	}
}

func TestAScheduledReadCountsAWaitingPlanOnce(t *testing.T) {
	t.Parallel()

	type read struct {
		observed string
		// another says the read finds the database changed, so it plans another plan.
		another bool
	}
	for _, row := range []struct {
		name  string
		reads []read
	}{
		{
			name:  "two reads that find the database unchanged",
			reads: []read{{observed: "observed-state"}, {observed: "observed-state"}},
		},
		{
			name:  "a read that finds the database changed",
			reads: []read{{observed: "moved-state", another: true}},
		},
		{
			name:  "a read that finds the database changed, then one that finds it unchanged",
			reads: []read{{observed: "moved-state", another: true}, {observed: "moved-state"}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			walk := newScheduledWalk(t)
			current := walk.waitingPlan("plan-1", "observed-state")
			walk.wait()
			plans := 1
			walk.assertCounted("the first plan", plans, plans)

			for index, read := range row.reads {
				published := walk.scheduledRead(safetyPlanDocument(t, read.observed))
				if read.another == (published == current) {
					t.Fatalf("read %d published %s after %s, want another plan: %t", index+1, published, current, read.another)
				}
				walk.wait()
				if read.another {
					plans++
				}
				current = published
				walk.assertCounted(fmt.Sprintf("after read %d", index+1), plans, plans)
			}
		})
	}
}

// A plan is still counted once when something withdrew its requirement and a
// later read publishes it again, but the requirement coming back is a person
// being asked again, so it records another ApprovalRequired and counts another
// required approval. A revoked approval is the third way back, and
// TestInvalidApprovalClearsApplyClaimWithoutStartingAJob holds its Event.
func TestTheSamePlanAsksAgainOnlyWhenItsRequirementCameBack(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		withdraw func(t *testing.T, walk *scheduledWalk)
		reason   operatorv1alpha1.ConditionReason
	}{
		{
			name: "the database converged, then drifted back",
			withdraw: func(t *testing.T, walk *scheduledWalk) {
				t.Helper()
				walk.read("observe-converged")
				walk.converge("plan-converged")
			},
			reason: operatorv1alpha1.ReasonNoChanges,
		},
		{
			name: "a realm refusal lifted",
			withdraw: func(t *testing.T, walk *scheduledWalk) {
				t.Helper()
				if _, err := walk.reconciler.schemaRealmBlocked(context.Background(), walk.stored(), realmRefusal{
					Reason: operatorv1alpha1.ReasonRealmConflict, Message: "refused", Approval: "withheld", Apply: "withheld",
				}); err != nil {
					t.Fatalf("schemaRealmBlocked() error = %v", err)
				}
			},
			reason: operatorv1alpha1.ReasonRealmConflict,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			walk := newScheduledWalk(t)
			first := walk.waitingPlan("plan-1", "observed-state")
			walk.assertCounted("the first plan", 1, 1)

			row.withdraw(t, walk)
			withdrawn := meta.FindStatusCondition(walk.stored().Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
			if withdrawn == nil || withdrawn.Status != metav1.ConditionFalse || withdrawn.Reason != string(row.reason) {
				t.Fatalf("ApprovalRequired after the withdrawal = %+v, want False/%s", withdrawn, row.reason)
			}
			walk.assertCounted("the withdrawal", 1, 1)

			walk.read("observe-again")
			if again := walk.waitingPlan("plan-again", "observed-state"); again != first {
				t.Fatalf("the unchanged plan came back as %s, not %s; this row needs the same plan", again, first)
			}
			walk.assertCounted("the plan back", 1, 2)
		})
	}
}

// spec.policy.apply is not part of the policy a plan is computed under, so the
// read after an edit of it publishes the plan already in status under the same
// name, and the journal records nothing. A plan that starts asking for an
// approval there is still a person being asked, each time it starts.
func TestAnApplyPolicyEditAsksForThePlanAlreadyPublished(t *testing.T) {
	t.Parallel()

	walk := newScheduledWalk(t)
	setApply := func(apply operatorv1alpha1.ApplyPolicy) {
		t.Helper()
		edited := walk.stored()
		edited.Spec.Policy.Apply = apply
		// The fake client does not manage the generation the controller reads a
		// spec edit from.
		edited.Generation++
		if err := walk.api.Update(context.Background(), edited); err != nil {
			t.Fatalf("set apply %s: %v", apply, err)
		}
	}
	document := safetyPlanDocument(t, "observed-state")
	setApply(operatorv1alpha1.ApplyPolicyNever)
	first := walk.publish("plan-1", document)
	if first.Status.Phase != operatorv1alpha1.PhaseBlocked {
		t.Fatalf("the plan under Never left phase %s, want Blocked", first.Status.Phase)
	}
	walk.assertCounted("the plan under Never", 1, 0)

	for _, step := range []struct {
		apply operatorv1alpha1.ApplyPolicy
		phase operatorv1alpha1.ReconciliationPhase
		asked int
	}{
		{apply: operatorv1alpha1.ApplyPolicyOnApproval, phase: operatorv1alpha1.PhaseAwaitingApproval, asked: 1},
		{apply: operatorv1alpha1.ApplyPolicyNever, phase: operatorv1alpha1.PhaseBlocked, asked: 1},
		{apply: operatorv1alpha1.ApplyPolicyOnApproval, phase: operatorv1alpha1.PhaseAwaitingApproval, asked: 2},
	} {
		setApply(step.apply)
		published := walk.refresh(document)
		if published.Status.Plan.Name != first.Status.Plan.Name || published.Status.Phase != step.phase {
			t.Fatalf("the read after apply %s published %s in phase %s, want %s in phase %s",
				step.apply, published.Status.Plan.Name, published.Status.Phase, first.Status.Plan.Name, step.phase)
		}
		walk.assertCounted(fmt.Sprintf("apply %s, asked %d", step.apply, step.asked), 1, step.asked)
	}
}
