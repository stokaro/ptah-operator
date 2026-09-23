package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A Job that produced more than one executor Pod is an outcome nobody can
// account for, in both families.
//
// A Job runs a second Pod when the first one dies, and each Pod is an executor
// that may have opened the database. One result frame is one Pod's account and
// cannot speak for the other, so there is nothing to read that settles which
// of them ran what. Retrying here dispatches a third.
//
// Neither family tested it. Removing the mutating branch from either harvest
// passed the whole suite, which is the gap these close. The assertions name
// the refusal as well as the outcome, because a fixture that reaches an
// unaccounted record by some other route would otherwise look like coverage --
// the first draft of this did exactly that, diverting at the plan lookup long
// before the Pods were counted.

// dispatchedApplyWithPlan is an Apply that crossed its dispatch boundary with
// every binding the pass revalidates still intact, so the harvest is reached
// rather than a refusal on the way to it.
func dispatchedApplyWithPlan(t *testing.T) (
	*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, client.Object,
) {
	t.Helper()

	schema, plan, _, policyConfig := safetyApprovalFixture(t)
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	target := databaseTargetBinding(schema.Spec.Target)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "apply-operation", JobName: "apply-job",
		StartedAt: metav1.Now(), Attempt: 1,
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		Target: &target, LeaseDurationSeconds: 960,
		DispatchStarted: true, JobUID: "job-uid",
	}
	bindActiveInput(t, schema)
	return schema, plan, policyConfig
}

// secondExecutorPod is the Pod a Job runs after the first one died.
func secondExecutorPod(job *batchv1.Job, first *corev1.Pod) *corev1.Pod {
	second := first.DeepCopy()
	second.Name = generatedTerminalPodName(job.Name, "def34")
	second.UID = "second-pod-uid"
	return second
}

func TestASchemaApplyWithTwoExecutorPodsIsUnaccountedFor(t *testing.T) {
	t.Parallel()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	second := secondExecutorPod(job, pod)

	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig, job, pod, second)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := safetyGetSchema(t, api, schema)
	pending := actual.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("the Apply was not recorded as unaccounted for: %#v", pending)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the claim survived as %#v, so the pass retried rather than accounted",
			actual.Status.ActiveOperation)
	}
	if pending.ApplyPodCount != 2 {
		t.Errorf("the record kept %d Pod(s); both are why nothing settles this", pending.ApplyPodCount)
	}
	failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || !strings.Contains(failed.Message, "multiple executor Pods") {
		t.Fatalf("the resource does not say the Job produced more than one executor: %#v", failed)
	}
}

func TestAMigrationApplyWithTwoExecutorPodsIsUnaccountedFor(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	second := pod.DeepCopy()
	second.Name = generatedTerminalPodName(job.Name, "def34")
	second.UID = "second-pod-uid"

	reconciler, api := fakeMigrationReconciler(t, staticLogs{},
		migration, plan, verificationPolicyConfigMap(), job, pod, second)
	holdMigrationApplyLease(t, reconciler, api, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, migration)
	if actual.Status.UnresolvedRun == nil {
		t.Fatalf("the Apply was not recorded as unaccounted for: phase=%q lastRun=%#v",
			actual.Status.Phase, actual.Status.LastRun)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the claim survived as %#v, so the pass retried rather than accounted",
			actual.Status.ActiveOperation)
	}
	blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionTrue {
		t.Fatalf("the resource is not blocked on the unaccounted run: %#v", blocked)
	}
}
