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

// blockedProof is a schema whose proof read another target, as the reconciler
// left it: Blocked, with the claim between two attempts and the retry delay ahead.
func blockedProof(
	t *testing.T,
	outcome operatorv1alpha1.PendingObservationOutcome,
	objects ...client.Object,
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
	reconciler, api := fakeReconciler(t, staticLogs{content: movedTargetFrame(t, schema, safetyOtherDigest, nil)},
		append([]client.Object{schema, job, pod, policy}, objects...)...)
	reconciler.Locks = targetlock.New(api, api, nil)
	blocked := reconcileSchema(t, reconciler, api, schema)
	if !schemaProofBlocked(blocked) {
		t.Fatalf("the fixture did not block: phase %s, conditions %#v", blocked.Status.Phase, blocked.Status.Conditions)
	}
	return blocked, reconciler, api
}

func setSettledApply(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, value string) {
	t.Helper()
	stored := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
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
			for _, says := range []string{"put the first login or address back", operatorv1alpha1.SettledApplyAnnotation + "=apply-operation", "kubectl annotate ptahschema " + schema.Name} {
				if !strings.Contains(failed.Message, says) {
					t.Errorf("the message does not say %q: %q", says, failed.Message)
				}
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
		{"no target named", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := safetyPostApplyObserveSchema(t)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			frame := movedTargetFrame(t, schema, row.digest, &runner.ResultError{Code: "execution_error", Message: "the server went away"})
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			actual := reconcileSchema(t, reconciler, api, schema)
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed {
				t.Fatalf("phase %s, want Failed", actual.Status.Phase)
			}
			if failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed == nil ||
				failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) {
				t.Fatalf("ReconciliationFailed = %#v, want OperationFailed", failed)
			}
			setSettledApply(t, api, actual, "apply-operation")
			again := reconcileSchema(t, reconciler, api, actual)
			if again.Status.PendingObservation == nil {
				t.Fatal("a person ended a proof that is not stopped by a moved target")
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
			reached := blocked.Status.NextReconciliationTime.Add(time.Second)
			reconciler.Clock = func() time.Time { return reached }

			observed := finishProofAttempt(t, reconciler, api, blocked, "second")
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
			proved := finishProofAttempt(t, reconciler, api, blocked, "third")
			if proved.Status.Phase != operatorv1alpha1.PhaseInSync || proved.Status.PendingObservation != nil {
				t.Fatalf("the proof did not finish: phase %s, %#v", proved.Status.Phase, proved.Status.PendingObservation)
			}
			if credited := proved.Status.Applied != nil; credited != row.credit {
				t.Fatalf("applied = %#v, want credited %t", proved.Status.Applied, row.credit)
			}
		})
	}
}

// finishProofAttempt stands up the terminal Job and Pod of the claim the schema
// stored, with a result read through the proof's own target, and takes one pass.
func finishProofAttempt(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	name string,
) *operatorv1alpha1.PtahSchema {
	t.Helper()
	stored := storedSchemaOf(t, api, schema)
	job, pod := terminalWorkload(stored, batchv1.JobComplete)
	job.UID, pod.UID = types.UID(name+"-job-uid"), types.UID(name+"-pod-uid")
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
	reconciler.Logs = staticLogs{content: movedTargetFrame(t, stored, testDigest, nil)}
	return reconcileSchema(t, reconciler, api, schema)
}

// A person settles the proof by naming its Apply. Nothing of it stays: the claim,
// the proof, the plan, the Lease. The approval it consumed goes stale, no Apply is
// credited, and the journal says nobody established what the Apply did.
func TestAPersonSettlesAProofThatReadsAnotherTarget(t *testing.T) {
	t.Parallel()
	for _, outcome := range []operatorv1alpha1.PendingObservationOutcome{
		operatorv1alpha1.PendingObservationApplySucceeded, operatorv1alpha1.PendingObservationOutcomeUnknown,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()
			blocked, reconciler, api := blockedProof(t, outcome)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder
			setSettledApply(t, api, blocked, "apply-operation")

			settled := reconcileSchema(t, reconciler, api, blocked)

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
			if event := findEvent(recorder, corev1.EventTypeNormal+" ProofSettled"); !strings.Contains(event, "apply-operation") {
				t.Fatalf("ProofSettled Event = %q, want it to name the Apply", event)
			}
			entry := lastJournalEntry(t, settled)
			if entry.Type != operatorv1alpha1.JournalVerified || entry.Outcome != operatorv1alpha1.JournalUnknown {
				t.Fatalf("journal ends with %#v, want Verified Unknown", entry)
			}
			if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
				t.Fatalf("the settled proof kept its Lease: holder %q", holder)
			}

			// The resource starts again from the source, under whatever the
			// Secret names now.
			finalizerGone := reconcileSchema(t, reconciler, api, blocked)
			if finalizerGone.Status.ActiveOperation == nil || finalizerGone.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("the pass after it claimed %#v, want a Resolve", finalizerGone.Status.ActiveOperation)
			}
		})
	}
}

// An approval the Apply consumed does not bind anything once its proof is given up.
func TestSettlingAProofRetiresTheApprovalThePlanRecorded(t *testing.T) {
	t.Parallel()
	approval := &operatorv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "recorded-approval", UID: "recorded-approval-uid", Generation: 1,
	}}
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationOutcomeUnknown, approval)
	stored := blocked.DeepCopy()
	stored.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "post-apply-plan", UID: "post-apply-plan-uid",
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: approval.Name, UID: approval.UID},
	}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	setSettledApply(t, api, blocked, "apply-operation")
	if settled := reconcileSchema(t, reconciler, api, blocked); settled.Status.Plan != nil {
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

// Each row is something the settlement must refuse, beside the test above that it
// accepts. None of them ends the proof.
func TestWhatASettlementRefuses(t *testing.T) {
	t.Parallel()
	const applyID = "apply-operation"

	t.Run("no annotation", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("a value that names another Apply", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, "an-earlier-apply")
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("an empty value", func(t *testing.T) {
		t.Parallel()
		blocked, _, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, "")
		stored := storedSchemaOf(t, api, blocked)
		stored.Status.PendingObservation.ApplyOperationID = ""
		if proofSettlementAsked(stored) {
			t.Fatal("an empty value matched an empty operation ID")
		}
	})

	t.Run("a refusal that has cleared", func(t *testing.T) {
		t.Parallel()
		blocked, _, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		setSettledApply(t, api, blocked, applyID)
		stored := storedSchemaOf(t, api, blocked)
		if !proofSettlementAsked(stored) {
			t.Fatal("the fixture does not ask for a settlement")
		}
		meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed).Status = metav1.ConditionFalse
		if proofSettlementAsked(stored) {
			t.Fatal("a settlement was taken with the refusal gone")
		}
		meta.RemoveStatusCondition(&stored.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
		if proofSettlementAsked(stored) {
			t.Fatal("a settlement was taken with no refusal recorded")
		}
	})

	t.Run("a proof failing for another reason", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		stored := blocked.DeepCopy()
		setFailure(stored, operatorv1alpha1.ReasonOperationFailed, errFailedProof)
		if err := api.Status().Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		setSettledApply(t, api, blocked, applyID)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("an attempt whose Job exists", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Namespace: blocked.Namespace, Name: blocked.Status.ActiveOperation.JobName, UID: "lost-write-uid",
		}}
		if err := api.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		setSettledApply(t, api, blocked, applyID)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("an attempt whose Job is recorded", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		stored := blocked.DeepCopy()
		stored.Status.ActiveOperation.JobUID = "running-uid"
		if err := api.Status().Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		setSettledApply(t, api, blocked, applyID)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("an Apply Pod that may still run, with a claim", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		if err := api.Create(context.Background(), runningApplyPod()); err != nil {
			t.Fatal(err)
		}
		setSettledApply(t, api, blocked, applyID)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("an Apply Pod that may still run, with no claim", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		if err := api.Create(context.Background(), runningApplyPod()); err != nil {
			t.Fatal(err)
		}
		retireProofClaim(t, api, blocked)
		setSettledApply(t, api, blocked, applyID)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})

	t.Run("an execution horizon that has not passed, with no claim", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
		retireProofClaim(t, api, blocked)
		stored := storedSchemaOf(t, api, blocked)
		after := metav1.NewTime(reconciler.now().Add(time.Hour))
		stored.Status.PendingObservation.ObserveAfter = &after
		if err := api.Status().Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		setSettledApply(t, api, blocked, applyID)
		assertProofStillOwed(t, reconcileSchema(t, reconciler, api, blocked))
	})
}

// A suspension retires the claim and keeps the proof. Settling touches no database,
// so it needs neither a claim nor a resource that is running, and a person who
// suspended the resource can still end what it owes.
func TestASuspendedResourceCanStillBeSettled(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	retireProofClaim(t, api, blocked)
	suspended := storedSchemaOf(t, api, blocked)
	suspended.Spec.Suspend = true
	if err := api.Update(context.Background(), suspended); err != nil {
		t.Fatal(err)
	}
	setSettledApply(t, api, blocked, "apply-operation")
	settled := reconcileSchema(t, reconciler, api, blocked)
	if settled.Status.PendingObservation != nil {
		t.Fatalf("the proof outlived a person's statement under suspension: %#v", settled.Status.PendingObservation)
	}
	if settled.Status.PendingLockRelease != nil {
		t.Fatalf("the Lease was not handed back: %#v", settled.Status.PendingLockRelease)
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
		t.Fatalf("the settled proof kept its Lease: holder %q", holder)
	}
	if next := reconcileSchema(t, reconciler, api, blocked); next.Status.Phase != operatorv1alpha1.PhaseSuspended || next.Status.ActiveOperation != nil {
		t.Fatalf("a suspended resource went on: phase %s, %#v", next.Status.Phase, next.Status.ActiveOperation)
	}
}

// A later Apply is not settled by the statement about an earlier one. The value is
// the proof's own, and it stays on the resource after the settlement.
func TestASettlementDoesNotCarryOverToALaterApply(t *testing.T) {
	t.Parallel()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	setSettledApply(t, api, blocked, "apply-operation")
	later := storedSchemaOf(t, api, blocked)
	later.Status.PendingObservation.ApplyOperationID = "a-later-apply"
	if err := api.Status().Update(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if actual := reconcileSchema(t, reconciler, api, blocked); actual.Status.PendingObservation == nil ||
		actual.Status.PendingObservation.ApplyOperationID != "a-later-apply" {
		t.Fatalf("the statement about an earlier Apply ended a later proof: %#v", actual.Status.PendingObservation)
	}
}

var errFailedProof = errors.New("the server went away")

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

// retireProofClaim is the claim gone, as a suspension or a discarded read leaves a
// proof: the record stands and nothing is in flight.
func retireProofClaim(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) {
	t.Helper()
	stored := storedSchemaOf(t, api, schema)
	stored.Status.ActiveOperation = nil
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
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
