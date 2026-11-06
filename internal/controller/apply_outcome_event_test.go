package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

const uncertainApplyFixedEvent = "Warning ApplyOutcomeUnknown Apply outcome is uncertain; observing database state"

// harvestApplyFailure runs the pass that reads an Apply frame carrying failure,
// and returns what it wrote and what it said.
func harvestApplyFailure(
	t *testing.T,
	failure *runner.ResultError,
) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchema, *record.FakeRecorder) {
	t.Helper()
	schema, plan, policy := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
		ChildExitCode: 2, CoordinationDigest: schema.Status.ActiveOperation.CoordinationDigest, Error: failure,
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policy, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return schema, safetyGetSchema(t, api, schema), recorder
}

// assertUnaccountedApply holds what the decision on #794 keeps: the record, the
// outcome and the conditions say an unknown outcome, whatever the Event's words.
func assertUnaccountedApply(t *testing.T, actual *operatorv1alpha1.PtahSchema) {
	t.Helper()
	pending := actual.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("PendingObservation = %#v, want OutcomeUnknown", pending)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
		t.Fatalf("Phase = %q, want VerifyingConvergence", actual.Status.Phase)
	}
	applying := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying)
	if applying == nil || applying.Status != metav1.ConditionFalse || applying.Reason != string(operatorv1alpha1.ReasonOutcomeUnknown) {
		t.Fatalf("Applying = %#v, want False OutcomeUnknown", applying)
	}
	for _, conditionType := range []string{operatorv1alpha1.ConditionReconciliationFailed, operatorv1alpha1.ConditionReady} {
		condition := meta.FindStatusCondition(actual.Status.Conditions, conditionType)
		if condition == nil || condition.Reason != string(operatorv1alpha1.ReasonApplyOutcomeUnknown) {
			t.Fatalf("%s = %#v, want reason ApplyOutcomeUnknown", conditionType, condition)
		}
	}
}

// An Apply whose result names a kind says why in the Event, for each kind. The
// frame's own message stays in the Pod.
func TestAnUncertainApplyEventCarriesTheKindsSentence(t *testing.T) {
	t.Parallel()
	for _, kind := range dataplane.TargetFailureKinds() {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			schema, actual, recorder := harvestApplyFailure(t, &runner.ResultError{Code: string(kind), Message: frameText})
			assertUnaccountedApply(t, actual)
			want := uncertainApplyFixedEvent + ": " + targetfailure.Message(kind, schema.Spec.Target.URLFrom)
			if event := findEvent(recorder, "Warning ApplyOutcomeUnknown"); event != want {
				t.Fatalf("ApplyOutcomeUnknown event = %q, want %q", event, want)
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}

// Any other Apply error keeps the fixed text: a code the operator does not read
// as a kind carries nothing about the target and nothing of the runner's words.
func TestAnUncertainApplyEventKeepsTheFixedTextForAnyOtherError(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]*runner.ResultError{
		"an Apply that failed before any statement": {Code: "apply_failed", Message: "ptah reported that the apply failed before it sent any statement"},
		"an Apply whose statements may have run":    {Code: "apply_outcome_unknown", Message: "how far they got is unknown"},
		"a code that resembles a kind":              {Code: "Database_Unreachable", Message: frameText},
		"a kind outside the vocabulary":             {Code: "quota_exhausted", Message: frameText},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, actual, recorder := harvestApplyFailure(t, failure)
			assertUnaccountedApply(t, actual)
			want := uncertainApplyFixedEvent
			if runner.LeavesReportedError(failure.Code) {
				// The run wrote Ptah's sentence, and the Event says where. That sentence
				// is still not in the Event.
				want += ". What Ptah reported is in the Pod log"
			}
			if event := findEvent(recorder, "Warning ApplyOutcomeUnknown"); event != want && !strings.HasPrefix(event, want+" for ") {
				t.Fatalf("ApplyOutcomeUnknown event = %q, want %q", event, want)
			}
		})
	}
	t.Run("an Apply whose Job is gone", func(t *testing.T) {
		t.Parallel()
		schema := safetyApplySchema(t)
		schema.Status.ActiveOperation.DispatchStarted = true
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		reconciler.Locks = targetlock.New(api, api, nil)
		recorder := record.NewFakeRecorder(32)
		reconciler.Recorder = recorder
		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		assertUnaccountedApply(t, safetyGetSchema(t, api, schema))
		if event := findEvent(recorder, "Warning ApplyOutcomeUnknown"); event != uncertainApplyFixedEvent {
			t.Fatalf("ApplyOutcomeUnknown event = %q, want the fixed text", event)
		}
	})
}

// The Event keeps as much of the sentence as every other failure Event, 512
// bytes, so the fixed words in front of it cost the sentence nothing.
func TestAnUncertainApplyEventBoundsTheSentence(t *testing.T) {
	t.Parallel()
	long := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: strings.Repeat("n", 253)}, Key: strings.Repeat("k", 253),
	}
	sentence := targetfailure.Message(dataplane.TargetTLSFailed, long)
	if len(sentence) <= 512 {
		t.Fatalf("the sentence is %d bytes, so this row bounds nothing", len(sentence))
	}
	got := uncertainApplyEvent(resultFailure(&runner.ResultError{Code: string(dataplane.TargetTLSFailed)}, long))
	if want := "Apply outcome is uncertain; observing database state: " + bounded(sentence, 512); got != want {
		t.Fatalf("uncertainApplyEvent() = %q, want %q", got, want)
	}
}

// With the database still closed, the Apply that could not open it runs nothing
// more: the resource goes through the read-only proof, an Observe that fails the
// same way and then one that succeeds, and a Plan, with an Apply claim at no point.
func TestNoApplyRunsBeforeTheProofOfAnApplyThatCouldNotOpenItsTarget(t *testing.T) {
	t.Parallel()
	schema, plan, policy := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	pod.Status.Phase = corev1.PodFailed
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
		ChildExitCode: 2, CoordinationDigest: schema.Status.ActiveOperation.CoordinationDigest,
		Error: &runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable), Message: frameText},
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policy, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	ctx := context.Background()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	pass := func(what string) *operatorv1alpha1.PtahSchema {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() %s: %v", what, err)
		}
		stored := safetyGetSchema(t, api, schema)
		if active := stored.Status.ActiveOperation; active != nil && active.Type == operatorv1alpha1.OperationApply {
			t.Fatalf("%s claimed an Apply before the proof completed: %#v", what, active)
		}
		err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: "ptah-apply-test"}, &batchv1.Job{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("%s dispatched another Apply Job before the proof completed: %v", what, err)
		}
		return stored
	}

	harvested := pass("the pass that harvests the Apply")
	assertUnaccountedApply(t, harvested)
	if harvested.Status.ActiveOperation != nil {
		t.Fatalf("the harvest kept a claim: %#v", harvested.Status.ActiveOperation)
	}

	observing := pass("the pass after the harvest")
	if active := observing.Status.ActiveOperation; active == nil || active.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("the pass after the harvest claimed %#v, want the proof's Observe", active)
	}

	// The database stays closed: the proof's Observe fails with the same kind.
	if _, err := reconciler.retryOperation(ctx, observing, nil, resultFailure(
		&runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable)}, schema.Spec.Target.URLFrom,
	)); err != nil {
		t.Fatalf("retry the proof's Observe: %v", err)
	}
	retrying := pass("the pass after the proof's Observe failed")
	if active := retrying.Status.ActiveOperation; active == nil || active.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("the failed proof left %#v, want its Observe claimed again", active)
	}
	assertDatabaseUnreached(t, retrying.Status.Conditions, operatorv1alpha1.ReasonDatabaseUnreachable,
		targetfailure.Message(dataplane.TargetDatabaseUnreachable, schema.Spec.Target.URLFrom))
	if pending := retrying.Status.PendingObservation; pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("the failed proof left PendingObservation = %#v, want OutcomeUnknown", pending)
	}

	// The database answers. The proof goes on to its Plan, and still no Apply.
	if _, err := reconciler.consumeResult(ctx, retrying, nil, runner.Result{
		Operation:            runner.OperationObserve,
		CoordinationDigest:   retrying.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: retrying.Status.PendingObservation.Plan.TargetIdentityDigest,
		DriftReportDigest:    testDigest,
		ObservedDialect:      "postgresql",
	}, nil, 0); err != nil {
		t.Fatalf("complete the proof's Observe: %v", err)
	}
	planning := pass("the pass after the proof's Observe")
	if active := planning.Status.ActiveOperation; active == nil || active.Type != operatorv1alpha1.OperationPlan {
		t.Fatalf("the pass after the proof's Observe claimed %#v, want the proof's Plan", active)
	}
	if planning.Status.PendingObservation == nil {
		t.Fatal("the proof's Observe settled the record, and only its Plan may")
	}

	if _, err := reconciler.consumeResult(ctx, planning, nil, runner.Result{
		Operation:            runner.OperationPlan,
		CoordinationDigest:   planning.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: planning.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	}, nil, 0); err != nil {
		t.Fatalf("complete the proof's Plan: %v", err)
	}
	proved := safetyGetSchema(t, api, schema)
	if proved.Status.PendingObservation != nil || proved.Status.ActiveOperation != nil {
		t.Fatalf("the completed proof left %#v and %#v", proved.Status.PendingObservation, proved.Status.ActiveOperation)
	}
	if err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: "ptah-apply-test"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the proof dispatched an Apply Job: %v", err)
	}
	assertDatabaseReached(t, proved.Status.Conditions, "Database schema was observed")
}
