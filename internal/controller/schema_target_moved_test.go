package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A target the store moves to after safetyOtherDigest, for the rows where it
// moves twice.
const thirdTargetDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

// What a stop says when the annotation holds a value that is not the one it prints.
const anotherValueClause = "the settled-apply annotation holds another value, which settles nothing on this target; "

// What a stop says when the annotation already holds the value it prints.
const thisValueClause = "the annotation already holds this value, so the next read of this target gives the proof up; "

// annotationHeld is what a stop says the settled-apply annotation holds.
type annotationHeld int

const (
	// heldNothing is a stop that says nothing about the annotation.
	heldNothing annotationHeld = iota
	// heldAnother is a stop that says the annotation holds another value.
	heldAnother
	// heldThis is a stop that says the annotation already holds its value.
	heldThis
)

// movedTargetFrame is what a proof's read says when the URL Secret names digest:
// a read the child finished, or one it could not, whose error is childError.
func movedTargetFrame(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
	digest string,
	childError *runner.ResultError,
) []byte {
	t.Helper()
	result := runner.Result{
		ProtocolVersion: runner.ProtocolVersion, OperationID: schema.Status.ActiveOperation.ID,
		ChildExitCode: 0, CoordinationDigest: schema.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: digest, Error: childError,
	}
	if schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationPlan {
		result.Operation = runner.OperationPlan
		if childError == nil {
			result.PlanOutcome = runner.PlanOutcomeNoChanges
		}
	} else {
		result.Operation = runner.OperationObserve
		if childError == nil {
			result.DriftReportDigest, result.ObservedDialect = safetyOtherDigest, "postgresql"
		}
	}
	return safetyRunnerFrame(t, result)
}

// proofFixture is a schema with a proof Observe whose Job finished, and the
// reconciler that has not looked at it yet.
func proofFixture(
	t *testing.T,
	outcome operatorv1alpha1.PendingObservationOutcome,
	firstRead []byte,
) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
	t.Helper()
	schema := safetyPostApplyObserveSchema(t)
	schema.Status.PendingObservation.Outcome = outcome
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: time.Minute}
	policyDigest := fingerprint.DigestBytes([]byte("policy"))
	schema.Status.Source.VerificationPolicyDigest = policyDigest
	schema.Status.PendingObservation.Plan.VerificationPolicyDigest = policyDigest
	bindActiveInput(t, schema)
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
	}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	if firstRead == nil {
		firstRead = movedTargetFrame(t, schema, safetyOtherDigest, nil)
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: firstRead}, schema, job, pod, policy)
	reconciler.Locks = targetlock.New(api, api, nil)
	return schema, reconciler, api
}

// blockedProof is a schema whose proof read another target, as the reconciler
// left it: Blocked, with the claim between two attempts and the retry delay ahead.
func blockedProof(
	t *testing.T,
	outcome operatorv1alpha1.PendingObservationOutcome,
) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
	t.Helper()
	schema, reconciler, api := proofFixture(t, outcome, nil)
	blocked := reconcileSchema(t, reconciler, api, schema)
	if !schemaProofBlocked(blocked) {
		t.Fatalf("the fixture did not block: phase %s, conditions %#v", blocked.Status.Phase, blocked.Status.Conditions)
	}
	return blocked, reconciler, api
}

// movedValue is what the fixtures' stop on safetyOtherDigest prints.
func movedValue() string { return settledApplyValue("apply-operation", safetyOtherDigest) }

func setSettledApply(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, value string) {
	t.Helper()
	stored := storedSchemaOf(t, api, schema)
	if stored.Annotations == nil {
		stored.Annotations = map[string]string{}
	}
	stored.Annotations[operatorv1alpha1.SettledApplyAnnotation] = value
	if err := api.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

func assertProofStillOwed(t *testing.T, actual *operatorv1alpha1.PtahSchema) {
	t.Helper()
	safetyAssertPendingProof(t, actual)
	if actual.Status.Applied != nil {
		t.Fatalf("a read of another target credited the Apply: %#v", actual.Status.Applied)
	}
	if actual.Status.PendingLockRelease != nil {
		t.Fatalf("the proof gave up its Lease while it is owed: %#v", actual.Status.PendingLockRelease)
	}
}

// nextProofRead stands up the terminal Job and Pod of the claim the schema stored,
// with a result that names digest, moves the clock past the retry delay, and takes
// one pass. The fake builder gives every attempt of an operation one name, so a Job
// a finished attempt left under it goes first; a real builder names each one apart.
func nextProofRead(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	attempt, digest string,
	childError *runner.ResultError,
) *operatorv1alpha1.PtahSchema {
	t.Helper()
	return proofReadEnding(t, reconciler, api, schema, attempt, proofReadShape{
		ending: batchv1.JobComplete,
		log: func(stored *operatorv1alpha1.PtahSchema) []byte {
			return movedTargetFrame(t, stored, digest, childError)
		},
	})
}

// proofReadShape is how one attempt of a proof's read ended, for the rows where it
// did not simply finish.
type proofReadShape struct {
	ending batchv1.JobConditionType
	log    func(*operatorv1alpha1.PtahSchema) []byte
	// pod shapes the attempt's Pod. Returning nil leaves the Job with none, as a
	// Job's deadline does.
	pod func(*corev1.Pod) *corev1.Pod
	// startedLongAgo puts the attempt past the grace a Job's missing Pod gets.
	startedLongAgo bool
}

func proofReadEnding(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	attempt string,
	shape proofReadShape,
) *operatorv1alpha1.PtahSchema {
	t.Helper()
	stored := storedSchemaOf(t, api, schema)
	if next := stored.Status.NextReconciliationTime; next != nil {
		reached := next.Add(time.Second)
		reconciler.Clock = func() time.Time { return reached }
	}
	if shape.startedLongAgo {
		stored.Status.ActiveOperation.StartedAt = metav1.NewTime(reconciler.Clock().Add(-2 * terminalPodGrace))
	}
	stale := &batchv1.Job{}
	err := api.Get(context.Background(), client.ObjectKey{Namespace: stored.Namespace, Name: stored.Status.ActiveOperation.JobName}, stale)
	if err == nil {
		if err := api.Delete(context.Background(), stale); err != nil {
			t.Fatal(err)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	job, pod := terminalWorkload(stored, shape.ending)
	job.UID, pod.UID = types.UID(attempt+"-job-uid"), types.UID(attempt+"-pod-uid")
	pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
	if err := api.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name}}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if shape.pod != nil {
		pod = shape.pod(pod)
	}
	stored.Status.ActiveOperation.JobUID = job.UID
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if pod != nil {
		if err := api.Create(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
	}
	reconciler.Logs = staticLogs{content: shape.log(stored)}
	return reconcileSchema(t, reconciler, api, schema)
}

// settledApplyCommand is the command a stop prints for value, in the fixtures'
// namespace and name, as it ends the message.
func settledApplyCommand(schema *operatorv1alpha1.PtahSchema, value string) string {
	return "kubectl annotate ptahschema " + schema.Name + " -n " + schema.Namespace + " " +
		operatorv1alpha1.SettledApplyAnnotation + "=" + value + " --overwrite"
}

// assertStopPrints holds a resource to a stop whose message ends with the command
// for value, and to the clause, if any, about what the annotation holds.
func assertStopPrints(t *testing.T, actual *operatorv1alpha1.PtahSchema, value string, held annotationHeld) {
	t.Helper()
	assertProofStillOwed(t, actual)
	if !schemaProofBlocked(actual) {
		t.Fatalf("phase %s, want the resource stopped", actual.Status.Phase)
	}
	failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonTargetIdentityChanged) {
		t.Fatalf("ReconciliationFailed = %#v, want TargetIdentityChanged", failed)
	}
	if !strings.HasSuffix(failed.Message, settledApplyCommand(actual, value)) {
		t.Fatalf("the stop prints %q, want it to end with the command for %q", failed.Message, value)
	}
	assertHeldClause(t, failed.Message, held)
}

// assertHeldClause holds a stop's message to what it says the annotation holds.
func assertHeldClause(t *testing.T, message string, held annotationHeld) {
	t.Helper()
	if said := strings.Contains(message, anotherValueClause); said != (held == heldAnother) {
		t.Fatalf("says the annotation holds another value = %t, want %t: %q", said, held == heldAnother, message)
	}
	if said := strings.Contains(message, thisValueClause); said != (held == heldThis) {
		t.Fatalf("says the annotation already holds this value = %t, want %t: %q", said, held == heldThis, message)
	}
}

// assertSettled holds a resource to a proof a read gave up.
func assertSettled(t *testing.T, actual *operatorv1alpha1.PtahSchema) {
	t.Helper()
	if actual.Status.PendingObservation != nil || actual.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("the read did not take the statement: phase %s, %#v", actual.Status.Phase, actual.Status.PendingObservation)
	}
}

// The reproduction of stokaro/ptah-operator#819. A proof's read names another
// target. Before, every attempt ended Failed with the same sentence and nothing
// said what would end it. Whether the Apply finished or not, and whether the new
// login could connect or not, the read cannot prove the Apply, so the resource
// stops, keeps what it owes, and names both ways out.
func TestAProofThatReadsAnotherTargetStopsAndSaysHowToEndIt(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		operation  operatorv1alpha1.OperationType
		outcome    operatorv1alpha1.PendingObservationOutcome
		childError *runner.ResultError
	}{
		{
			name: "the Apply finished and the read went through", operation: operatorv1alpha1.OperationObserve,
			outcome: operatorv1alpha1.PendingObservationApplySucceeded,
		},
		{
			name: "the runner refused the Apply and the read went through", operation: operatorv1alpha1.OperationObserve,
			outcome: operatorv1alpha1.PendingObservationOutcomeUnknown,
		},
		{
			name: "the new login could not connect", operation: operatorv1alpha1.OperationObserve,
			outcome:    operatorv1alpha1.PendingObservationApplySucceeded,
			childError: &runner.ResultError{Code: "execution_error", Message: "password authentication failed"},
		},
		{
			name: "the proof's Plan read it", operation: operatorv1alpha1.OperationPlan,
			outcome: operatorv1alpha1.PendingObservationApplySucceeded,
		},
		{
			name: "the proof's Plan could not connect", operation: operatorv1alpha1.OperationPlan,
			outcome:    operatorv1alpha1.PendingObservationApplySucceeded,
			childError: &runner.ResultError{Code: "execution_error", Message: "password authentication failed"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := safetyPostApplyObserveSchema(t)
			schema.Status.PendingObservation.Outcome = row.outcome
			if row.operation == operatorv1alpha1.OperationPlan {
				schema.Status.PendingObservation.PlanRequired = true
				schema.Status.ActiveOperation.Type = operatorv1alpha1.OperationPlan
				bindActiveInput(t, schema)
			}
			attempt := schema.Status.ActiveOperation.Attempt
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			reconciler, api := fakeReconciler(t, staticLogs{content: movedTargetFrame(t, schema, safetyOtherDigest, row.childError)}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)

			actual := reconcileSchema(t, reconciler, api, schema)

			assertProofStillOwed(t, actual)
			if actual.Status.Phase != operatorv1alpha1.PhaseBlocked {
				t.Fatalf("phase %s, want Blocked", actual.Status.Phase)
			}
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonTargetIdentityChanged) {
				t.Fatalf("ReconciliationFailed = %#v, want TargetIdentityChanged", failed)
			}
			for _, says := range []string{
				"put the first URL back",
				"login, host, port, database or URL parameter",
				"under apply: Always, applies the next plan with no approval unless it is destructive or changes privileges",
			} {
				if !strings.Contains(failed.Message, says) {
					t.Errorf("the message does not say %q: %q", says, failed.Message)
				}
			}
			if command := settledApplyCommand(schema, movedValue()); !strings.HasSuffix(failed.Message, command) {
				t.Errorf("the message does not end with %q: %q", command, failed.Message)
			}
			ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(operatorv1alpha1.ReasonTargetIdentityChanged) {
				t.Fatalf("Ready = %#v, want it to name the reason", ready)
			}
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != row.operation || actual.Status.ActiveOperation.Attempt != attempt+1 {
				t.Fatalf("the read does not go on: %#v", actual.Status.ActiveOperation)
			}
		})
	}
}

// The command is the last thing the message says, a person runs it as it stands,
// and a settlement reads its value back from there. So it has to fit at the longest
// name and namespace the API admits, with either clause about the annotation.
func TestTheStopPrintsTheWholeCommandAtTheLongestNames(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("b", 64)
	operationID := "sha256:" + strings.Repeat("a", 64)
	for _, row := range []struct {
		name string
		held string
		says annotationHeld
	}{
		{name: "another value", held: "another value", says: heldAnother},
		{name: "this value", held: settledApplyValue(operationID, digest), says: heldThis},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := safetyPostApplyObserveSchema(t)
			schema.Name, schema.Namespace = strings.Repeat("n", 253), strings.Repeat("s", 63)
			schema.Status.PendingObservation.ApplyOperationID = operationID
			schema.Annotations = map[string]string{operatorv1alpha1.SettledApplyAnnotation: row.held}
			message := movedTargetFailure(schema, digest).Error()
			assertHeldClause(t, message, row.says)
			if bounded(message, 1024) != message {
				t.Fatalf("the message is %d bytes, and a condition keeps 1024", len(message))
			}
			if command := settledApplyCommand(schema, settledApplyValue(operationID, digest)); !strings.HasSuffix(message, command) {
				t.Fatalf("the message does not end with %q: %q", command, message)
			}
		})
	}
}

// stokaro/ptah-operator#894. A statement holds until the proof ends, so a stop can
// find the annotation already holding the value it prints, and the next read of
// the target gives the proof up with no new write. The stop says so, rather than
// ask for a command already run. Any other value is one the stop does not print,
// and settles nothing. A value is one or the other, so a stop says one clause at
// most, and none when nothing is written.
func TestAStopSaysWhatTheAnnotationAlreadyHolds(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		value *string
		says  annotationHeld
	}{
		{name: "nothing written", says: heldNothing},
		{name: "the value the stop prints", value: ptr(movedValue()), says: heldThis},
		{name: "an empty value", value: ptr(""), says: heldAnother},
		{name: "the Apply's ID alone", value: ptr("apply-operation"), says: heldAnother},
		{name: "another Apply on this target", value: ptr(settledApplyValue("an-earlier-apply", safetyOtherDigest)), says: heldAnother},
		{name: "this Apply on another target", value: ptr(settledApplyValue("apply-operation", thirdTargetDigest)), says: heldAnother},
		{name: "a value cut short", value: ptr(movedValue()[:len(movedValue())-1]), says: heldAnother},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := safetyPostApplyObserveSchema(t)
			if row.value != nil {
				schema.Annotations = map[string]string{operatorv1alpha1.SettledApplyAnnotation: *row.value}
			}
			message := movedTargetFailure(schema, safetyOtherDigest).Error()
			assertHeldClause(t, message, row.says)
			if command := settledApplyCommand(schema, movedValue()); !strings.HasSuffix(message, command) {
				t.Fatalf("the message does not end with %q: %q", command, message)
			}
		})
	}
}

// An Event keeps 512 bytes and the command runs past them at ordinary names, so an
// Event that carried it would hand a person a value cut short, which kubectl accepts
// and nothing refuses. It names the condition instead.
func TestTheStopsEventNamesTheConditionAndPrintsNoCommand(t *testing.T) {
	t.Parallel()
	schema, reconciler, api := proofFixture(t, operatorv1alpha1.PendingObservationApplySucceeded, nil)
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	reconcileSchema(t, reconciler, api, schema)
	event := findEvent(recorder, corev1.EventTypeWarning+" OperationFailed")
	if !strings.Contains(event, operatorv1alpha1.ConditionReconciliationFailed) || !strings.Contains(event, "another target") {
		t.Fatalf("the Event = %q, want it to name the failure and the condition", event)
	}
	for _, never := range []string{"kubectl", operatorv1alpha1.SettledApplyAnnotation, safetyOtherDigest} {
		if strings.Contains(event, never) {
			t.Fatalf("the Event carries %q: %q", never, event)
		}
	}
}

// The filter on the digest: the proof's own target, failing for another reason,
// or a result that names no target, each stays an ordinary failed read that no
// person can settle.
func TestAProofFailingForAnotherReasonIsNotBlocked(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		digest string
	}{
		{"its own target", testDigest},
		{"no target named, as a URL that does not parse", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			failure := &runner.ResultError{Code: "execution_error", Message: "the server went away"}
			schema, reconciler, api := proofFixture(t, operatorv1alpha1.PendingObservationApplySucceeded, nil)
			reconciler.Logs = staticLogs{content: movedTargetFrame(t, schema, row.digest, failure)}
			actual := reconcileSchema(t, reconciler, api, schema)
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed {
				t.Fatalf("phase %s, want Failed", actual.Status.Phase)
			}
			if failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed == nil ||
				failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) {
				t.Fatalf("ReconciliationFailed = %#v, want OperationFailed", failed)
			}
		})
	}
}

// A blocked proof waits out the retry delay as a failed read does, renews the
// Lease it holds meanwhile, and reads again when the delay ends.
func TestABlockedProofKeepsItsPaceAndItsLease(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	claim := blocked.Status.ActiveOperation.DeepCopy()
	// As many passes as a dispatch needs, so no Job is a statement about the delay
	// rather than about the admission snapshot being its own durable boundary.
	for range 3 {
		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(blocked)})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter <= 0 || result.RequeueAfter > maxLockContentionPoll {
			t.Fatalf("requeue %s, want the Lease's cadence, at most %s", result.RequeueAfter, maxLockContentionPoll)
		}
	}
	waiting := storedSchemaOf(t, api, blocked)
	if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.JobName != claim.JobName ||
		waiting.Status.Phase != operatorv1alpha1.PhaseBlocked {
		t.Fatalf("the proof did not wait: phase %s, %#v", waiting.Status.Phase, waiting.Status.ActiveOperation)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(blocked.Namespace)); err != nil {
		t.Fatal(err)
	}
	for index := range jobs.Items {
		if jobs.Items[index].Name == claim.JobName {
			t.Fatalf("attempt %d dispatched inside the retry delay", claim.Attempt)
		}
	}

	if proofLeaseHolder(t, reconciler, api) == "" {
		t.Fatal("the blocked proof released its Lease")
	}

	reached := waiting.Status.NextReconciliationTime.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
	for range 3 {
		reconcileSchema(t, reconciler, api, blocked)
	}
	read := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: blocked.Namespace, Name: claim.JobName}, read); err != nil {
		t.Fatalf("the proof did not read again past its delay: %v", err)
	}
}

// Putting the first login back is the way out that loses nothing, after an Apply
// that finished as well as one that was refused. The next read matches, the
// failure goes, and the proof runs on; a finished Apply is credited.
func TestAProofFinishesWhenTheFirstTargetIsBack(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		outcome operatorv1alpha1.PendingObservationOutcome
		credit  bool
	}{
		{operatorv1alpha1.PendingObservationApplySucceeded, true},
		{operatorv1alpha1.PendingObservationOutcomeUnknown, false},
	} {
		t.Run(string(row.outcome), func(t *testing.T) {
			t.Parallel()
			blocked, reconciler, api := blockedProof(t, row.outcome)

			observed := nextProofRead(t, reconciler, api, blocked, "second", testDigest, nil)
			if observed.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence || observed.Status.PendingObservation == nil ||
				!observed.Status.PendingObservation.PlanRequired {
				t.Fatalf("the read of the first target did not carry the proof on: phase %s, %#v", observed.Status.Phase, observed.Status.PendingObservation)
			}
			if failed := meta.FindStatusCondition(observed.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed == nil ||
				failed.Status != metav1.ConditionFalse {
				t.Fatalf("the moved target is still reported: %#v", failed)
			}

			claimed := reconcileSchema(t, reconciler, api, blocked)
			if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationPlan {
				t.Fatalf("the proof did not go on to its Plan: %#v", claimed.Status.ActiveOperation)
			}
			proved := nextProofRead(t, reconciler, api, blocked, "third", testDigest, nil)
			if proved.Status.Phase != operatorv1alpha1.PhaseInSync || proved.Status.PendingObservation != nil {
				t.Fatalf("the proof did not finish: phase %s, %#v", proved.Status.Phase, proved.Status.PendingObservation)
			}
			if credited := proved.Status.Applied != nil; credited != row.credit {
				t.Fatalf("applied = %#v, want credited %t", proved.Status.Applied, row.credit)
			}
		})
	}
}

// A person settles the proof with the value the stop printed. It is taken at the
// next read that names that target, whether or not the login there could connect.
// Nothing of the proof stays: the claim, the proof, the plan, the Lease. No Apply is
// credited, and the journal says nobody established what the Apply did.
func TestAPersonSettlesAProofThatReadsAnotherTarget(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		outcome    operatorv1alpha1.PendingObservationOutcome
		childError *runner.ResultError
	}{
		{name: "a finished Apply", outcome: operatorv1alpha1.PendingObservationApplySucceeded},
		{name: "a refused Apply", outcome: operatorv1alpha1.PendingObservationOutcomeUnknown},
		{
			name: "a login that cannot connect", outcome: operatorv1alpha1.PendingObservationApplySucceeded,
			childError: &runner.ResultError{Code: "execution_error", Message: "password authentication failed"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			blocked, reconciler, api := blockedProof(t, row.outcome)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder
			setSettledApply(t, api, blocked, movedValue())

			// Written and not yet read: the operator has only the last read to go
			// on, so it waits for one that names this target.
			assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))

			settled := nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, row.childError)

			if settled.Status.PendingObservation != nil || settled.Status.ActiveOperation != nil || settled.Status.Plan != nil ||
				settled.Status.Applied != nil || settled.Status.PendingLockRelease != nil {
				t.Fatalf("settling left %#v", settled.Status)
			}
			if settled.Status.Phase != operatorv1alpha1.PhasePending {
				t.Fatalf("phase %s, want Pending", settled.Status.Phase)
			}
			for _, conditionType := range []string{
				operatorv1alpha1.ConditionReady, operatorv1alpha1.ConditionInSync, operatorv1alpha1.ConditionReconciliationFailed,
			} {
				if condition := meta.FindStatusCondition(settled.Status.Conditions, conditionType); condition == nil ||
					condition.Reason != string(operatorv1alpha1.ReasonProofSettled) {
					t.Fatalf("%s = %#v, want ProofSettled", conditionType, condition)
				}
			}
			if failed := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed.Status != metav1.ConditionFalse {
				t.Fatalf("ReconciliationFailed = %#v, want the failure gone", failed)
			}
			event := findEvent(recorder, corev1.EventTypeNormal+" ProofSettled")
			for _, says := range []string{"apply-operation", "under apply: Always the next plan applies with no approval"} {
				if !strings.Contains(event, says) {
					t.Fatalf("ProofSettled Event = %q, want it to say %q", event, says)
				}
			}
			entry := lastJournalEntry(t, settled)
			if entry.Type != operatorv1alpha1.JournalVerified || entry.Outcome != operatorv1alpha1.JournalUnknown {
				t.Fatalf("journal ends with %#v, want Verified Unknown", entry)
			}
			if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
				t.Fatalf("the settled proof kept its Lease: holder %q", holder)
			}

			// The resource starts again from the source, under whatever the
			// Secret holds now.
			next := reconcileSchema(t, reconciler, api, blocked)
			if next.Status.ActiveOperation == nil || next.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("the pass after it claimed %#v, want a Resolve", next.Status.ActiveOperation)
			}
		})
	}
}

// An approval the Apply consumed does not bind anything once its proof is given up.
func TestSettlingAProofRetiresTheApprovalThePlanRecorded(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationOutcomeUnknown)
	approval := &operatorv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "recorded-approval", UID: "recorded-approval-uid", Generation: 1,
	}}
	if err := api.Create(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	stored := blocked.DeepCopy()
	stored.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "post-apply-plan", UID: "post-apply-plan-uid",
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: approval.Name, UID: approval.UID},
	}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	setSettledApply(t, api, blocked, movedValue())
	if settled := nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil); settled.Status.Plan != nil {
		t.Fatalf("the plan outlived its proof: %#v", settled.Status.Plan)
	}
	actual := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), actual); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
		t.Fatalf("the approval is not stale: %#v", actual.Status.Conditions)
	}
}

// The database is down and the proof is failing, a person reads the proof's ID from
// status and writes it, and nothing happens. Later the store moves the Secret by
// mistake. Before, the stop and the settlement followed with no write by anyone,
// and under apply: Always the next plan ran on that target. The ID names no target,
// so it arms nothing. The ID of the Apply's own claim, read during the Apply, is the
// same value.
func TestTheApplysIDAloneArmsNothing(t *testing.T) {
	t.Parallel()
	failure := &runner.ResultError{Code: "execution_error", Message: "the server went away"}
	schema, reconciler, api := proofFixture(t, operatorv1alpha1.PendingObservationApplySucceeded, nil)
	reconciler.Logs = staticLogs{content: movedTargetFrame(t, schema, testDigest, failure)}
	down := reconcileSchema(t, reconciler, api, schema)
	if down.Status.Phase != operatorv1alpha1.PhaseFailed {
		t.Fatalf("the fixture is not a proof failing on its own target: phase %s", down.Status.Phase)
	}

	setSettledApply(t, api, schema, "apply-operation")
	assertProofStillOwed(t, reconcileSchema(t, reconciler, api, schema))

	first := nextProofRead(t, reconciler, api, schema, "second", safetyOtherDigest, nil)
	assertStopPrints(t, first, movedValue(), heldAnother)
	// Every pass and every later read, with no write by anyone.
	assertProofStillOwed(t, reconcileSchema(t, reconciler, api, schema))
	assertStopPrints(t, nextProofRead(t, reconciler, api, schema, "third", safetyOtherDigest, nil), movedValue(), heldAnother)
}

// The digest is a plain SHA-256 of the URL, so a person can work the value out and
// write it before the resource stops. That does not skip the stop that says what
// settling does. It is a statement all the same, and the read after that stop takes
// it with no further write.
func TestAValueWorkedOutInAdvanceStillMeetsTheStop(t *testing.T) {
	t.Parallel()
	schema, reconciler, api := proofFixture(t, operatorv1alpha1.PendingObservationApplySucceeded, nil)
	setSettledApply(t, api, schema, movedValue())
	assertStopPrints(t, reconcileSchema(t, reconciler, api, schema), movedValue(), heldThis)
	assertSettled(t, nextProofRead(t, reconciler, api, schema, "second", safetyOtherDigest, nil))
}

// The stop has to have printed the value, and the last stop at that. A value for a
// target the resource last stopped elsewhere from has to meet its own stop first.
func TestASettlementWaitsForAStopThatPrintedItsValue(t *testing.T) {
	t.Parallel()
	t.Run("the store moved on and came back", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		third := settledApplyValue("apply-operation", thirdTargetDigest)
		assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "second", thirdTargetDigest, nil), third, heldAnother)
		// The condition prints the third target's value, so this read of the second
		// target is no read a person has seen a stop for.
		assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "third", safetyOtherDigest, nil), movedValue(), heldThis)
		assertSettled(t, nextProofRead(t, reconciler, api, blocked, "fourth", safetyOtherDigest, nil))
	})
	t.Run("a value worked out for a target the only stop did not name", func(t *testing.T) {
		t.Parallel()
		schema, reconciler, api := proofFixture(t, operatorv1alpha1.PendingObservationApplySucceeded, nil)
		reconciler.Logs = staticLogs{content: movedTargetFrame(t, schema, thirdTargetDigest, nil)}
		setSettledApply(t, api, schema, movedValue())
		third := settledApplyValue("apply-operation", thirdTargetDigest)
		assertStopPrints(t, reconcileSchema(t, reconciler, api, schema), third, heldAnother)
		assertStopPrints(t, nextProofRead(t, reconciler, api, schema, "second", safetyOtherDigest, nil), movedValue(), heldThis)
		assertSettled(t, nextProofRead(t, reconciler, api, schema, "third", safetyOtherDigest, nil))
	})
}

// The value is a person's statement about this Apply and this target, and it holds
// until the proof ends. A store that goes back to the first target and then to the
// second again stops there and settles, with no new write, because that is what the
// statement said. That stop says the annotation already holds its value (#894).
// Taking the annotation off is how to take it back.
func TestAStatementHoldsUntilTheProofEnds(t *testing.T) {
	t.Parallel()
	failure := &runner.ResultError{Code: "execution_error", Message: "the server went away"}
	t.Run("the first target answered in between", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		assertStopPrints(t, blocked, movedValue(), heldNothing)
		setSettledApply(t, api, blocked, movedValue())
		observed := nextProofRead(t, reconciler, api, blocked, "second", testDigest, nil)
		safetyAssertPendingProof(t, observed)
		if failed := meta.FindStatusCondition(observed.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed == nil ||
			failed.Status != metav1.ConditionFalse {
			t.Fatalf("the read of the first target left %#v", failed)
		}
		claimed := reconcileSchema(t, reconciler, api, blocked)
		if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationPlan {
			t.Fatalf("the proof did not go on to its Plan: %#v", claimed.Status.ActiveOperation)
		}
		assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "third", safetyOtherDigest, nil), movedValue(), heldThis)
		assertSettled(t, nextProofRead(t, reconciler, api, blocked, "fourth", safetyOtherDigest, nil))
	})
	t.Run("the first target was down in between", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		down := nextProofRead(t, reconciler, api, blocked, "second", testDigest, failure)
		assertProofStillOwed(t, down)
		if down.Status.Phase != operatorv1alpha1.PhaseFailed {
			t.Fatalf("phase %s, want Failed", down.Status.Phase)
		}
		assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "third", safetyOtherDigest, nil), movedValue(), heldThis)
		assertSettled(t, nextProofRead(t, reconciler, api, blocked, "fourth", safetyOtherDigest, nil))
	})
	t.Run("the annotation came off", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		stored := storedSchemaOf(t, api, blocked)
		delete(stored.Annotations, operatorv1alpha1.SettledApplyAnnotation)
		if err := api.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil), movedValue(), heldNothing)
	})
}

// A read that ends without its Job completing names no target, whatever its log says
// and whatever the annotation holds. It is an ordinary failed read: the message no
// longer prints a command, and a later read of the target stops the resource again
// before it settles.
func TestAReadWhoseJobFailedNamesNoTarget(t *testing.T) {
	t.Parallel()
	reachedDeadline := func(*corev1.Pod) *corev1.Pod { return nil }
	fetchRefused := func(pod *corev1.Pod) *corev1.Pod {
		pod.Status.ContainerStatuses = nil
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "fetch", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}}
		return pod
	}
	noFrame := func(*operatorv1alpha1.PtahSchema) []byte { return nil }
	for _, row := range []struct {
		name  string
		shape proofReadShape
		says  string
	}{
		{
			name: "the Job failed with a frame that names the target",
			shape: proofReadShape{ending: batchv1.JobFailed, log: func(stored *operatorv1alpha1.PtahSchema) []byte {
				return movedTargetFrame(t, stored, safetyOtherDigest, nil)
			}},
			says: "Job failed",
		},
		{
			name:  "the Job reached its deadline and took its Pod",
			shape: proofReadShape{ending: batchv1.JobFailed, log: noFrame, pod: reachedDeadline, startedLongAgo: true},
			says:  "read Observe result",
		},
		{
			name:  "the source fetch failed",
			shape: proofReadShape{ending: batchv1.JobFailed, log: noFrame, pod: fetchRefused},
			says:  "the fetch step exited 1",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
			setSettledApply(t, api, blocked, movedValue())

			failed := proofReadEnding(t, reconciler, api, blocked, "second", row.shape)

			assertProofStillOwed(t, failed)
			if failed.Status.Phase != operatorv1alpha1.PhaseFailed {
				t.Fatalf("phase %s, want Failed", failed.Status.Phase)
			}
			condition := meta.FindStatusCondition(failed.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if condition == nil || condition.Reason != string(operatorv1alpha1.ReasonOperationFailed) ||
				!strings.Contains(condition.Message, row.says) || strings.Contains(condition.Message, "kubectl") {
				t.Fatalf("ReconciliationFailed = %#v, want OperationFailed saying %q and no command", condition, row.says)
			}
			assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "third", safetyOtherDigest, nil), movedValue(), heldThis)
			assertSettled(t, nextProofRead(t, reconciler, api, blocked, "fourth", safetyOtherDigest, nil))
		})
	}
}

// A Secret deleted after the stop: the next attempt's Pod cannot start. Ready says so
// and ReconciliationFailed goes on printing the stop's command, which does nothing,
// because no read has named the target. The value waits for a read that does.
func TestAStopKeepsItsCommandWhileThePodCannotStart(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	setSettledApply(t, api, blocked, movedValue())
	stored := storedSchemaOf(t, api, blocked)
	reached := stored.Status.NextReconciliationTime.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
	stale := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: stored.Namespace, Name: stored.Status.ActiveOperation.JobName}}
	if err := api.Delete(context.Background(), stale); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	job, _ := terminalWorkload(stored, batchv1.JobComplete)
	job.UID = "starting-job-uid"
	job.Status = batchv1.JobStatus{Active: 1}
	stored.Status.ActiveOperation.JobUID = job.UID
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod := configurationErrorPod(job, createContainerConfigError, missingSecretText)
	if err := api.Create(context.Background(), pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := api.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	actual := reconcileSchema(t, reconciler, api, blocked)

	assertStopPrints(t, actual, movedValue(), heldNothing)
	ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) {
		t.Fatalf("Ready = %#v, want PodConfigurationError", ready)
	}
}

// Each row is a read the settlement must refuse, beside the test above that it
// accepts. None of them ends the proof.
func TestWhatASettlementRefuses(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		value string
		read  string
	}{
		{name: "no statement", read: safetyOtherDigest},
		{name: "the Apply's ID alone", value: "apply-operation", read: safetyOtherDigest},
		{name: "the target alone", value: safetyOtherDigest, read: safetyOtherDigest},
		{name: "another Apply on this target", value: settledApplyValue("an-earlier-apply", safetyOtherDigest), read: safetyOtherDigest},
		{name: "this Apply on the plan's own target", value: settledApplyValue("apply-operation", testDigest), read: safetyOtherDigest},
		{name: "a value cut short", value: movedValue()[:len(movedValue())-1], read: safetyOtherDigest},
		{name: "a store that moved again since the stop", value: movedValue(), read: thirdTargetDigest},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
			if row.value != "" {
				setSettledApply(t, api, blocked, row.value)
			}
			actual := nextProofRead(t, reconciler, api, blocked, "second", row.read, nil)
			held := heldNothing
			if row.value != "" {
				held = heldAnother
			}
			assertStopPrints(t, actual, settledApplyValue("apply-operation", row.read), held)
		})
	}

	t.Run("an empty value", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, "")
		assertStopPrints(t, nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil), movedValue(), heldAnother)
	})

	t.Run("the first target back", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		actual := nextProofRead(t, reconciler, api, blocked, "second", testDigest, nil)
		safetyAssertPendingProof(t, actual)
		if !actual.Status.PendingObservation.PlanRequired {
			t.Fatalf("the proof did not go on under its own target: %#v", actual.Status.PendingObservation)
		}
	})

	t.Run("a refusal that has cleared", func(t *testing.T) {
		t.Parallel()
		blocked, _, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		stored := storedSchemaOf(t, api, blocked)
		if !proofSettlementAsked(stored, safetyOtherDigest) {
			t.Fatal("the fixture does not ask for a settlement")
		}
		meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed).Status = metav1.ConditionFalse
		if proofSettlementAsked(stored, safetyOtherDigest) {
			t.Fatal("a settlement was taken with the refusal gone")
		}
		meta.RemoveStatusCondition(&stored.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
		if proofSettlementAsked(stored, safetyOtherDigest) {
			t.Fatal("a settlement was taken with no refusal recorded")
		}
	})

	t.Run("a proof failing for another reason since the stop", func(t *testing.T) {
		t.Parallel()
		blocked, _, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		stored := storedSchemaOf(t, api, blocked)
		setFailure(stored, operatorv1alpha1.ReasonOperationFailed, errors.New("the server went away"))
		if proofSettlementAsked(stored, safetyOtherDigest) {
			t.Fatal("a settlement was taken on a failure that is not the stop")
		}
	})

	t.Run("a stop that printed another value", func(t *testing.T) {
		t.Parallel()
		blocked, _, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, movedValue())
		stored := storedSchemaOf(t, api, blocked)
		setFailure(stored, operatorv1alpha1.ReasonTargetIdentityChanged, movedTargetFailure(stored, thirdTargetDigest))
		if proofSettlementAsked(stored, safetyOtherDigest) {
			t.Fatal("a settlement was taken on a stop that printed another target's value")
		}
	})

	t.Run("an Apply Pod that may still run", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		if err := api.Create(context.Background(), runningApplyPod()); err != nil {
			t.Fatal(err)
		}
		setSettledApply(t, api, blocked, movedValue())
		assertProofStillOwed(t, nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil))
	})
}

// A read a suspension finds under way runs to its end, since a proof outranks a
// suspension, so a statement is still taken from it. The resource settles, and then
// stands suspended with nothing claimed and nothing held.
func TestAReadUnderWayWhenSuspendedStillTakesTheStatement(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	suspended := storedSchemaOf(t, api, blocked)
	suspended.Spec.Suspend = true
	if err := api.Update(context.Background(), suspended); err != nil {
		t.Fatal(err)
	}
	setSettledApply(t, api, blocked, movedValue())

	assertSettled(t, nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil))

	next := reconcileSchema(t, reconciler, api, blocked)
	if next.Status.Phase != operatorv1alpha1.PhaseSuspended || next.Status.ActiveOperation != nil {
		t.Fatalf("phase %s, claim %#v; want Suspended and no claim", next.Status.Phase, next.Status.ActiveOperation)
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
		t.Fatalf("the settled proof kept its Lease: holder %q", holder)
	}
}

// A suspension retires the claim of a proof waiting out its retry delay and keeps
// the proof. Nothing else wakes a suspended resource, so it has to keep its own
// passes, or the Lease goes unrenewed and another claimant can take the database
// and cost the Apply its credit. It starts no reads, so with none under way a
// statement waits for the first read after it resumes.
func TestASuspendedProofKeepsItsLeaseAndTakesTheStatementAfterItResumes(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	suspended := storedSchemaOf(t, api, blocked)
	suspended.Spec.Suspend = true
	if err := api.Update(context.Background(), suspended); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(blocked)}
	for pass := range 3 {
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter <= 0 || result.RequeueAfter > maxLockContentionPoll {
			t.Fatalf("pass %d: requeue %s, want the Lease's cadence, at most %s", pass, result.RequeueAfter, maxLockContentionPoll)
		}
	}
	actual := storedSchemaOf(t, api, blocked)
	assertProofStillOwed(t, actual)
	if actual.Status.Phase != operatorv1alpha1.PhaseSuspended || actual.Status.ActiveOperation != nil {
		t.Fatalf("phase %s, claim %#v; want Suspended and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	if proofLeaseHolder(t, reconciler, api) == "" {
		t.Fatal("the suspended proof released its Lease")
	}

	setSettledApply(t, api, blocked, movedValue())
	assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))

	resumed := storedSchemaOf(t, api, blocked)
	resumed.Spec.Suspend = false
	if err := api.Update(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	claimed := reconcileSchema(t, reconciler, api, blocked)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("the resumed proof claimed %#v, want its Observe", claimed.Status.ActiveOperation)
	}
	if settled := nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil); settled.Status.PendingObservation != nil {
		t.Fatalf("the first read after resuming did not take the statement: %#v", settled.Status.PendingObservation)
	}
}

// The Event says what happened, and a Lease release that fails after the status
// write does not unsay it: the record retries the release, and the proof is gone.
func TestASettlementIsReportedWhenItsLeaseReleaseFails(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	reconciler.Locks = targetlock.New(api, leaseReleaseRefuser{Client: api}, nil)
	setSettledApply(t, api, blocked, movedValue())

	stored := storedSchemaOf(t, api, blocked)
	if next := stored.Status.NextReconciliationTime; next != nil {
		reached := next.Add(time.Second)
		reconciler.Clock = func() time.Time { return reached }
	}
	job, pod := terminalWorkload(stored, batchv1.JobComplete)
	job.UID, pod.UID = "second-job-uid", "second-pod-uid"
	pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
	stored.Status.ActiveOperation.JobUID = job.UID
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconciler.Logs = staticLogs{content: movedTargetFrame(t, stored, safetyOtherDigest, nil)}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(blocked)}); err == nil {
		t.Fatal("a failed Lease release was not reported to the queue")
	}
	settled := storedSchemaOf(t, api, blocked)
	if settled.Status.PendingObservation != nil || settled.Status.PendingLockRelease == nil {
		t.Fatalf("the settlement was not stored with the release it owes: %#v", settled.Status)
	}
	if event := findEvent(recorder, corev1.EventTypeNormal+" ProofSettled"); event == "" {
		t.Fatal("the settlement was stored and no Event says so")
	}
}

// leaseReleaseRefuser fails the write that gives a Lease up and no other.
type leaseReleaseRefuser struct {
	client.Client
}

func (c leaseReleaseRefuser) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if lease, ok := object.(*coordinationv1.Lease); ok && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == "" {
		return errors.New("the API server is unavailable")
	}
	return c.Client.Update(ctx, object, options...)
}

func storedSchemaOf(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.PtahSchema {
	t.Helper()
	stored := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// proofLeaseHolder is who holds the database realm the fixtures' proof took, or ""
// when nobody does.
func proofLeaseHolder(t *testing.T, reconciler *SchemaReconciler, api client.Client) string {
	t.Helper()
	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return ""
		}
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// runningApplyPod is a Pod of the Job the fixture's proof names, still running.
func runningApplyPod() *corev1.Pod {
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "apply-job-pod", UID: "apply-pod-uid",
			Labels: map[string]string{"job-name": "apply-job"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "apply-job", UID: "apply-job-uid", Controller: &yes, BlockOwnerDeletion: &yes,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func lastJournalEntry(t *testing.T, schema *operatorv1alpha1.PtahSchema) operatorv1alpha1.JournalEntry {
	t.Helper()
	if schema.Status.Journal == nil || len(schema.Status.Journal.Entries) == 0 {
		t.Fatalf("no journal: %#v", schema.Status.Journal)
	}
	entries := schema.Status.Journal.Entries
	return entries[len(entries)-1]
}
