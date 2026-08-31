package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const safetyOtherDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type failNthSchemaStatusPatchClient struct {
	client.Client
	failAt  int
	patches int
}

func (c *failNthSchemaStatusPatchClient) Status() client.SubResourceWriter {
	return &failNthSchemaStatusPatchWriter{
		SubResourceWriter: c.Client.Status(),
		client:            c,
	}
}

type failNthSchemaStatusPatchWriter struct {
	client.SubResourceWriter
	client *failNthSchemaStatusPatchClient
}

func (w *failNthSchemaStatusPatchWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	if _, ok := object.(*operatorv1alpha1.PtahSchema); ok {
		w.client.patches++
		if w.client.patches == w.client.failAt {
			return errors.New("injected PtahSchema status patch failure")
		}
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func TestMissingDispatchedApplyJobForcesObservationWithoutRecreation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		dispatchStarted bool
		jobUID          types.UID
	}{
		{name: "dispatch boundary persisted", dispatchStarted: true},
		{name: "Job UID persisted", jobUID: "missing-job-uid"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := safetyApplySchema(t)
			schema.Status.ActiveOperation.DispatchStarted = test.dispatchStarted
			schema.Status.ActiveOperation.JobUID = test.jobUID
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			reconciler.Locks = targetlock.New(api, api, nil)

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want immediate post-apply observation", result)
			}

			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation != nil {
				t.Fatalf("ActiveOperation = %#v, want cleared uncertain Apply", actual.Status.ActiveOperation)
			}
			if actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
				t.Fatalf("Phase = %q, want %q", actual.Status.Phase, operatorv1alpha1.PhaseVerifyingConvergence)
			}
			if actual.Status.PendingObservation == nil || actual.Status.PendingObservation.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
				t.Fatalf("PendingObservation = %#v, want OutcomeUnknown", actual.Status.PendingObservation)
			}
			if actual.Status.PendingObservation.ObserveAfter == nil {
				t.Fatal("uncertain missing Job did not persist a safe observation boundary")
			}
			if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying); condition == nil || condition.Reason != "OutcomeUnknown" {
				t.Fatalf("Applying condition = %#v, want OutcomeUnknown", condition)
			}

			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
				t.Fatalf("List(Jobs) error = %v", err)
			}
			if len(jobs.Items) != 0 {
				t.Fatalf("Jobs = %#v, want no recreated Apply Job", jobs.Items)
			}

			result, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() while waiting error = %v", err)
			}
			if result.RequeueAfter != maxLockContentionPoll {
				t.Fatalf("safe-boundary requeue = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
			}
			actual = safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation != nil {
				t.Fatalf("early proof operation = %#v, want none before ObserveAfter", actual.Status.ActiveOperation)
			}
			if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
				t.Fatalf("List(Jobs) after waiting reconcile error = %v", err)
			}
			if len(jobs.Items) != 0 {
				t.Fatalf("Jobs before ObserveAfter = %#v, want none", jobs.Items)
			}
		})
	}
}

func TestMissingTerminalApplyPodWaitsThroughAbsoluteExecutionHorizon(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	started := time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)
	schema.Status.ActiveOperation.StartedAt = metav1.NewTime(started)
	schema.Status.ActiveOperation.DispatchNotAfter = nil
	schema.Status.ActiveOperation.ExecutionNotAfter = nil
	schema.Status.ActiveOperation.TerminationGracePeriodSeconds = 0
	schema.Status.ActiveOperation.JobUID = "job-uid"
	schema.Status.ActiveOperation.DispatchStarted = true
	bindActiveInput(t, schema)
	job, _ := terminalWorkload(schema, batchv1.JobComplete)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("Reconcile() result = %#v, want outcome-unknown proof", result)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.PendingObservation == nil || actual.Status.PendingObservation.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("PendingObservation = %#v", actual.Status.PendingObservation)
	}
	wantHorizon := started.Add(15*time.Minute + 30*time.Second)
	if actual.Status.PendingObservation.ObserveAfter == nil || !actual.Status.PendingObservation.ObserveAfter.Time.Equal(wantHorizon) {
		t.Fatalf("ObserveAfter = %#v, want %s", actual.Status.PendingObservation.ObserveAfter, wantHorizon)
	}

	result, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() before execution horizon error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("proof operation started before execution horizon: %#v", actual.Status.ActiveOperation)
	}
}

func TestMultipleApplyPodsArePersistedAndForceOutcomeUnknown(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	schema.Status.ActiveOperation.StartedAt = metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC))
	schema.Status.ActiveOperation.DispatchNotAfter = nil
	schema.Status.ActiveOperation.ExecutionNotAfter = nil
	schema.Status.ActiveOperation.TerminationGracePeriodSeconds = 0
	schema.Status.ActiveOperation.JobUID = "job-uid"
	schema.Status.ActiveOperation.DispatchStarted = true
	bindActiveInput(t, schema)
	job, firstPod := terminalWorkload(schema, batchv1.JobComplete)
	secondPod := firstPod.DeepCopy()
	secondPod.Name = job.Name + "-pod-2"
	secondPod.UID = "pod-uid-2"
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, firstPod, secondPod)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	pending := actual.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.ApplyPodCount != 2 {
		t.Fatalf("PendingObservation = %#v", pending)
	}
	wantUIDs := []types.UID{"pod-uid", "pod-uid-2"}
	if !reflect.DeepEqual(pending.ApplyPodUIDs, wantUIDs) {
		t.Fatalf("ApplyPodUIDs = %q, want %q", pending.ApplyPodUIDs, wantUIDs)
	}
	if pending.ObserveAfter == nil {
		t.Fatal("multiple Apply Pods did not retain the absolute execution horizon")
	}
}

func TestExactOwnerPodWithInvalidIntentIsCountedButNeverTrusted(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	job.Spec.Template.Labels = map[string]string{workload.LabelOperationID: "expected-operation"}
	delete(pod.Labels, "job-name")
	reconciler, _ := fakeReconciler(t, staticLogs{}, schema, job, pod)

	evidence, selected, err := reconciler.collectTerminalPodEvidence(context.Background(), schema, job)
	if !errors.Is(err, errTerminalPodIntent) {
		t.Fatalf("collectTerminalPodEvidence() error = %v, want immutable-intent refusal", err)
	}
	if selected != nil || evidence.Trusted {
		t.Fatalf("selected = %#v, evidence = %#v; invalid Pod must never supply logs", selected, evidence)
	}
	if evidence.PodCount != 1 || !reflect.DeepEqual(evidence.PodUIDs, []types.UID{"pod-uid"}) {
		t.Fatalf("evidence = %#v, want exact-owner Pod retained", evidence)
	}
}

func TestPersistedTargetLockReleaseRecoversAfterManagerCrash(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.PendingLockRelease = &operatorv1alpha1.TargetLockReleaseStatus{
		CoordinationDigest:   testCoordinationDigest,
		OperationID:          "completed-plan-operation",
		LeaseDurationSeconds: 960,
		LeaseEpoch:           testLeaseEpoch,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	request := targetlock.Request{
		CoordinationNamespace: reconciler.LockNamespace,
		CoordinationDigest:    testCoordinationDigest,
		Holder: targetlock.Holder{
			SchemaUID: schema.UID, OperationID: schema.Status.PendingLockRelease.OperationID,
		},
		Duration: 960 * time.Second,
	}
	acquired, err := reconciler.Locks.Acquire(context.Background(), request)
	if err != nil || !acquired.Acquired {
		t.Fatalf("Acquire() = %#v, %v", acquired, err)
	}
	stored := safetyGetSchema(t, api, schema)
	stored.Status.PendingLockRelease.LeaseEpoch = acquired.Epoch
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("persist release epoch: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.PendingLockRelease != nil {
		t.Fatalf("PendingLockRelease = %#v, want cleared", actual.Status.PendingLockRelease)
	}
	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
		t.Fatalf("read released Lease: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "" {
		t.Fatalf("Lease holder = %#v, want empty", lease.Spec.HolderIdentity)
	}
}

func TestTargetLockReleaseWaitsForOwnerStatusPatch(t *testing.T) {
	t.Parallel()

	schema := safetyLockedOperationSchema(operatorv1alpha1.OperationPlan)
	schema.Spec.Suspend = true
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	failing := &failNthSchemaStatusPatchClient{Client: api, failAt: 1}
	reconciler.Client = failing
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("Reconcile() succeeded despite an injected owner status patch failure")
	}
	unchanged := safetyGetSchema(t, api, schema)
	if unchanged.Status.ActiveOperation == nil || unchanged.Status.PendingLockRelease != nil {
		t.Fatalf("status crossed the release boundary after a failed patch: %#v", unchanged.Status)
	}
	if !contains(unchanged.Finalizers, activeOperationFinalizer) {
		t.Fatal("failed owner status patch removed the safety finalizer")
	}
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder == "" {
		t.Fatal("failed owner status patch released the database-realm Lease")
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	completed := safetyGetSchema(t, api, schema)
	if completed.Status.ActiveOperation != nil || completed.Status.PendingLockRelease != nil {
		t.Fatalf("retry did not drain the release transition: %#v", completed.Status)
	}
	if contains(completed.Finalizers, activeOperationFinalizer) {
		t.Fatal("completed suspended transition retained the safety finalizer")
	}
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder != "" {
		t.Fatalf("retry left Lease holder %q", holder)
	}
}

func TestPersistedReleaseCannotClearNewEpochAfterMarkerPatchFailure(t *testing.T) {
	t.Parallel()

	schema := safetyLockedOperationSchema(operatorv1alpha1.OperationPlan)
	schema.Spec.Suspend = true
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	failing := &failNthSchemaStatusPatchClient{Client: api, failAt: 2}
	reconciler.Client = failing
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("Reconcile() succeeded despite an injected release-marker patch failure")
	}
	persisted := safetyGetSchema(t, api, schema)
	if persisted.Status.ActiveOperation != nil || persisted.Status.PendingLockRelease == nil {
		t.Fatalf("release marker was not durable after its clear failed: %#v", persisted.Status)
	}
	oldEpoch := persisted.Status.PendingLockRelease.LeaseEpoch
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder != "" {
		t.Fatalf("first release left Lease holder %q", holder)
	}

	reacquire := targetlock.Request{
		CoordinationNamespace: reconciler.LockNamespace,
		CoordinationDigest:    persisted.Status.PendingLockRelease.CoordinationDigest,
		Holder: targetlock.Holder{
			SchemaUID: persisted.UID, OperationID: persisted.Status.PendingLockRelease.OperationID,
		},
		Duration: time.Duration(persisted.Status.PendingLockRelease.LeaseDurationSeconds) * time.Second,
	}
	newer, err := reconciler.Locks.Acquire(context.Background(), reacquire)
	if err != nil || !newer.Acquired {
		t.Fatalf("reacquire Lease = %#v, %v", newer, err)
	}
	if newer.Epoch == oldEpoch {
		t.Fatalf("reacquisition reused released epoch %q", oldEpoch)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("release-marker retry Reconcile() error = %v", err)
	}
	cleared := safetyGetSchema(t, api, schema)
	if cleared.Status.PendingLockRelease != nil {
		t.Fatalf("PendingLockRelease = %#v, want cleared", cleared.Status.PendingLockRelease)
	}
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder == "" {
		t.Fatal("stale persisted release cleared the same holder's newer epoch")
	}
}

func TestDeletionRetainsLeaseOwnerUntilReleaseMarkerIsDurable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		newSchema  func(*testing.T) *operatorv1alpha1.PtahSchema
		ownerAlive func(*operatorv1alpha1.PtahSchema) bool
	}{
		{
			name: "undispatched Apply",
			newSchema: func(t *testing.T) *operatorv1alpha1.PtahSchema {
				return safetyLockedOperationSchema(operatorv1alpha1.OperationApply)
			},
			ownerAlive: func(schema *operatorv1alpha1.PtahSchema) bool {
				return schema.Status.ActiveOperation != nil
			},
		},
		{
			name: "ordinary Plan",
			newSchema: func(t *testing.T) *operatorv1alpha1.PtahSchema {
				return safetyLockedOperationSchema(operatorv1alpha1.OperationPlan)
			},
			ownerAlive: func(schema *operatorv1alpha1.PtahSchema) bool {
				return schema.Status.ActiveOperation != nil
			},
		},
		{
			name: "pending post-Apply observation",
			newSchema: func(t *testing.T) *operatorv1alpha1.PtahSchema {
				schema := safetyPostApplyObserveSchema(t)
				schema.Status.ActiveOperation = nil
				schema.Status.PendingObservation.ApplyJobName = ""
				schema.Status.PendingObservation.ApplyJobUID = ""
				schema.Status.PendingObservation.ObserveAfter = nil
				return schema
			},
			ownerAlive: func(schema *operatorv1alpha1.PtahSchema) bool {
				return schema.Status.PendingObservation != nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := test.newSchema(t)
			deletedAt := metav1.NewTime(time.Date(2026, 8, 30, 12, 1, 0, 0, time.UTC))
			schema.DeletionTimestamp = &deletedAt
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			failing := &failNthSchemaStatusPatchClient{Client: api, failAt: 1}
			reconciler.Client = failing
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

			if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
				t.Fatal("Reconcile() succeeded despite an injected deletion status patch failure")
			}
			unchanged := safetyGetSchema(t, api, schema)
			if !test.ownerAlive(unchanged) || unchanged.Status.PendingLockRelease != nil {
				t.Fatalf("deletion crossed the release boundary after a failed patch: %#v", unchanged.Status)
			}
			if !contains(unchanged.Finalizers, activeOperationFinalizer) {
				t.Fatal("failed deletion status patch removed the safety finalizer")
			}
			if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder == "" {
				t.Fatal("failed deletion status patch released the database-realm Lease")
			}

			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("retry Reconcile() error = %v", err)
			}
			completed := &operatorv1alpha1.PtahSchema{}
			err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), completed)
			if err != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("read deleted schema: %v", err)
			}
			if err == nil && (contains(completed.Finalizers, activeOperationFinalizer) ||
				completed.Status.ActiveOperation != nil || completed.Status.PendingObservation != nil ||
				completed.Status.PendingLockRelease != nil) {
				t.Fatalf("retry did not complete deletion safety work: %#v", completed)
			}
			if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder != "" {
				t.Fatalf("retry left Lease holder %q", holder)
			}
		})
	}
}

func TestOrphanedPendingApplyPodBlocksProofPastTimeHorizon(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	schema.Status.ActiveOperation = nil
	schema.Status.PendingObservation.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
	past := metav1.NewTime(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC))
	schema.Status.PendingObservation.ObserveAfter = &past
	operationID := schema.Status.PendingObservation.ApplyOperationID
	labels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelOperation:   "apply",
		workload.LabelOperationID: workload.OperationIDLabelValue(operationID),
	}
	annotations := map[string]string{workload.AnnotationOperationID: operationID}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       schema.Namespace,
			Name:            schema.Status.PendingObservation.ApplyJobName,
			UID:             schema.Status.PendingObservation.ApplyJobUID,
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
		}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       schema.Namespace,
			Name:            "orphaned-apply-pod",
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("orphaned Pod requeue = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("proof operation = %#v, want none while Apply Pod is pending", actual.Status.ActiveOperation)
	}
	if !contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatal("pending Apply Pod lost the safety finalizer")
	}

	currentPod := &corev1.Pod{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(pod), currentPod); err != nil {
		t.Fatal(err)
	}
	currentPod.Status.Phase = corev1.PodFailed
	if err := api.Status().Update(context.Background(), currentPod); err != nil {
		t.Fatalf("mark orphaned Pod terminal: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() after Pod termination error = %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("post-terminal proof operation = %#v, want Observe", actual.Status.ActiveOperation)
	}
}

func TestLateApplyPodInvalidatesAlreadyClaimedProof(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	schema.Status.PendingObservation.ApplyPodUIDs = []types.UID{"initial-apply-pod-uid"}
	schema.Status.PendingObservation.ApplyPodCount = 1
	bindActiveInput(t, schema)
	proofJob, proofPod := terminalWorkload(schema, batchv1.JobComplete)
	applyJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace,
		Name:      schema.Status.PendingObservation.ApplyJobName,
		UID:       schema.Status.PendingObservation.ApplyJobUID,
	}}
	lateApplyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       schema.Namespace,
			Name:            "late-apply-pod",
			UID:             "late-apply-pod-uid",
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(applyJob)},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, proofJob, proofPod, lateApplyPod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("late Apply Pod requeue = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("active proof = %#v, want retained but unconsumed Observe", actual.Status.ActiveOperation)
	}
	wantPodUIDs := []types.UID{"initial-apply-pod-uid", lateApplyPod.UID}
	if actual.Status.PendingObservation == nil || actual.Status.PendingObservation.ApplyPodCount != 2 ||
		!reflect.DeepEqual(actual.Status.PendingObservation.ApplyPodUIDs, wantPodUIDs) {
		t.Fatalf("late Apply Pod evidence = %#v", actual.Status.PendingObservation)
	}

	currentPod := &corev1.Pod{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(lateApplyPod), currentPod); err != nil {
		t.Fatal(err)
	}
	currentPod.Status.Phase = corev1.PodFailed
	if err := api.Status().Update(context.Background(), currentPod); err != nil {
		t.Fatalf("mark late Apply Pod terminal: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() after late Pod termination error = %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil || actual.Status.PendingObservation == nil {
		t.Fatalf("stale proof status = %#v; want durable pending proof and no active operation", actual.Status)
	}
	if !contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatal("stale proof removed the post-Apply safety finalizer")
	}
	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
		t.Fatalf("read pending proof Lease: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Fatal("stale proof released its database-realm Lease")
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() fresh proof claim error = %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("fresh proof operation = %#v, want Observe", actual.Status.ActiveOperation)
	}
}

func TestChildPreMutationRefusalAfterApplyDispatchRequiresProof(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "stale-approval", UID: "stale-approval-uid"}
	schema.Status.ActiveOperation.DispatchStarted = true
	schema.Status.ActiveOperation.JobUID = "job-uid"
	policyBinding, err := policyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "current-plan", UID: "current-plan-uid"},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              schema.Status.Plan.Fingerprint,
			ArtifactDigest:           schema.Status.Source.Digest,
			CoordinationDigest:       schema.Status.Plan.CoordinationDigest,
			TargetIdentityDigest:     schema.Status.Plan.TargetIdentityDigest,
			PolicyFingerprint:        policyBinding,
			VerificationPolicyUID:    testPolicyUID,
			VerificationPolicyDigest: testDigest,
		},
	}
	schema.Status.Plan.Name = plan.Name
	schema.Status.Plan.UID = plan.UID
	schema.Status.Plan.PolicyFingerprint = policyBinding
	schema.Status.Plan.VerificationPolicyUID = plan.Spec.VerificationPolicyUID
	schema.Status.Plan.VerificationPolicyDigest = plan.Spec.VerificationPolicyDigest
	bindActiveInput(t, schema)
	approval := &operatorv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: "stale-approval", UID: "stale-approval-uid", Generation: 1,
	}}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion,
		Operation:       runner.OperationApply,
		OperationID:     schema.Status.ActiveOperation.ID,
		ChildExitCode:   0,
		Error: &runner.ResultError{
			Code:    "target_binding_mismatch",
			Message: "database target identity changed after planning",
		},
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, approval, plan)
	reconciler.Locks = targetlock.New(api, api, nil)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("Reconcile() result = %#v, want fresh observation", result)
	}

	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil {
		t.Fatalf("outcome-unknown Apply status = %#v; want active operation cleared and plan retained for proof", actual.Status)
	}
	if actual.Status.PendingObservation == nil || actual.Status.PendingObservation.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("PendingObservation = %#v, want OutcomeUnknown", actual.Status.PendingObservation)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
		t.Fatalf("Phase = %q, want %q", actual.Status.Phase, operatorv1alpha1.PhaseVerifyingConvergence)
	}
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying); condition == nil || condition.Reason != "OutcomeUnknown" {
		t.Fatalf("Applying condition = %#v, want OutcomeUnknown", condition)
	}
}

func TestStalePlanEvidenceForcesFreshObservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		resultCoordination string
		resultTarget       string
	}{
		{
			name:               "target identity changed",
			resultCoordination: testCoordinationDigest,
			resultTarget:       safetyOtherDigest,
		},
		{
			name:               "coordination realm changed",
			resultCoordination: safetyOtherDigest,
			resultTarget:       testDigest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			policyBytes := "policy"
			policyDigest := fingerprint.DigestBytes([]byte(policyBytes))
			schema := schemaFixture()
			schema.Finalizers = []string{activeOperationFinalizer}
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
				Digest:                   testDigest,
				Verified:                 true,
				VerificationPolicyUID:    testPolicyUID,
				VerificationPolicyDigest: policyDigest,
			}
			schema.Status.Target = operatorv1alpha1.TargetStatus{
				CoordinationDigest: testCoordinationDigest,
				IdentityDigest:     testDigest,
				DriftReportDigest:  safetyOtherDigest,
			}
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type:      operatorv1alpha1.OperationPlan,
				ID:        "plan-operation",
				JobName:   "plan-job",
				JobUID:    "job-uid",
				StartedAt: metav1.Now(),
				Attempt:   1,
			}
			bindActiveInput(t, schema)
			planDocument := safetyPlanDocument(t, "observed-state")
			frame := safetyRunnerFrame(t, runner.Result{
				ProtocolVersion:      runner.ProtocolVersion,
				Operation:            runner.OperationPlan,
				OperationID:          schema.Status.ActiveOperation.ID,
				ChildExitCode:        0,
				Stdout:               string(planDocument),
				CoordinationDigest:   test.resultCoordination,
				TargetIdentityDigest: test.resultTarget,
				PlanContentDigest:    fingerprint.DigestBytes(planDocument),
				PlanOutcome:          runner.PlanOutcomeChanges,
			})
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			immutable := true
			policyConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: schema.Namespace,
					Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
					UID:       testPolicyUID,
				},
				Immutable: &immutable,
				Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: policyBytes},
			}
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfigMap)

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want immediate observation", result)
			}

			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseObserving {
				t.Fatalf("stale plan status = %#v, want cleared operation in Observing", actual.Status)
			}
			if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionPlanReady); condition == nil || condition.Reason != "StaleObservation" {
				t.Fatalf("PlanReady condition = %#v, want StaleObservation", condition)
			}
			if actual.Status.NextReconciliationTime != nil {
				t.Fatalf("NextReconciliationTime = %s, want no failed-operation delay", actual.Status.NextReconciliationTime)
			}
		})
	}
}

func TestPolicyReplacementCannotProduceInSyncFromNoChangePlan(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
		Digest:                   testDigest,
		ArtifactType:             dataplane.SchemaArtifactType,
		Verified:                 true,
		VerificationPolicyUID:    "old-policy-uid",
		VerificationPolicyDigest: fingerprint.DigestBytes([]byte("old-policy")),
	}
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		CoordinationDigest: testCoordinationDigest,
		IdentityDigest:     testDigest,
		DriftReportDigest:  safetyOtherDigest,
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:      operatorv1alpha1.OperationPlan,
		ID:        "policy-race-plan",
		JobName:   "policy-race-plan-job",
		JobUID:    "job-uid",
		StartedAt: metav1.Now(),
		Attempt:   1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		CoordinationDigest:   schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	})
	immutable := true
	replacement := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
			UID:       "new-policy-uid",
		},
		Immutable: &immutable,
		Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "new-policy"},
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, replacement)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.Source.Verified || actual.Status.Phase != operatorv1alpha1.PhaseVerifying {
		t.Fatalf("policy replacement status = %#v", actual.Status)
	}
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync); condition != nil && condition.Status == metav1.ConditionTrue {
		t.Fatalf("InSync condition = %#v, must not be true under an unverified replacement policy", condition)
	}
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified); condition == nil || condition.Reason != "PolicyChanged" {
		t.Fatalf("ArtifactVerified condition = %#v, want PolicyChanged", condition)
	}
}

func TestPendingProofCompletesBeforeAChangedPolicyTakesEffect(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	schema.Status.PendingObservation.PlanRequired = true
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:      operatorv1alpha1.OperationPlan,
		ID:        "post-apply-plan",
		JobName:   "post-apply-plan-job",
		JobUID:    "job-uid",
		StartedAt: metav1.Now(),
		Attempt:   1,
	}
	bindActiveInput(t, schema)
	schema.Generation = schema.Status.PendingObservation.ApplyGeneration + 1
	schema.Spec.Desired.VerificationPolicyFrom.Name = "verification-policy-v2"
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		CoordinationDigest:   schema.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: schema.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.PendingObservation != nil || actual.Status.ActiveOperation != nil {
		t.Fatalf("completed proof was not committed: %#v", actual.Status)
	}
	if actual.Status.Applied == nil || actual.Status.Applied.PlanFingerprint != "plan-fingerprint" {
		t.Fatalf("Applied = %#v, want attribution from the completed immutable proof", actual.Status.Applied)
	}
	if actual.Status.Source.Verified || actual.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("new policy was not scheduled after proof completion: %#v", actual.Status)
	}
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync); condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != "PolicyChanged" {
		t.Fatalf("InSync condition = %#v, want PolicyChanged after completed proof", condition)
	}
}

func TestApprovalReservationRequiresFreshGenerationPassBeforeApply(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
		Digest:                   testDigest,
		Verified:                 true,
		ArtifactType:             "application/vnd.stokaro.ptah.schema.v1",
		VerificationPolicyDigest: fingerprint.DigestBytes([]byte("policy")),
		VerificationPolicyUID:    testPolicyUID,
	}
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		Engine:         schema.Spec.Target.Engine,
		IdentityDigest: testDigest,
	}
	policyBinding, err := policyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "approved-plan", UID: "approved-plan-uid", Generation: 1},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          1,
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              "sha256:plan",
			ContentDigest:            testDigest,
			ArtifactDigest:           schema.Status.Source.Digest,
			TargetIdentityDigest:     schema.Status.Target.IdentityDigest,
			ActualStateFingerprint:   safetyOtherDigest,
			DesiredStateFingerprint:  "sha256:desired",
			PolicyFingerprint:        policyBinding,
			VerificationPolicyUID:    schema.Status.Source.VerificationPolicyUID,
			VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "example.invalid/ptah@" + testDigest,
			RunnerImage:              "example.invalid/operator@" + testDigest,
			RunnerProtocolVersion:    int32(runner.ProtocolVersion),
			Dialect:                  "postgresql",
		},
	}
	schema.Status.Plan = currentPlanStatus(plan, metav1.NewTime(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC)))
	approvedAt := metav1.NewTime(time.Date(2026, 8, 30, 11, 30, 0, 0, time.UTC))
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "approval", UID: "approval-uid"},
		Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
			SchemaRef:                plan.Spec.SchemaRef,
			PlanRef:                  operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint:          plan.Spec.Fingerprint,
			ArtifactDigest:           plan.Spec.ArtifactDigest,
			TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
			ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
			DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
			PolicyFingerprint:        plan.Spec.PolicyFingerprint,
			VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
			VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
			PtahVersion:              plan.Spec.PtahVersion,
			ExecutorImage:            plan.Spec.ExecutorImage,
			RunnerImage:              plan.Spec.RunnerImage,
			RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
			Approver:                 operatorv1alpha1.ApprovalIdentity{Username: "approver@example.com"},
			ApprovedAt:               approvedAt,
			MutationRequestUID:       "approval-request-uid",
		},
	}
	policyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("approval reservation Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("approval reservation result = %#v, want a fresh pass", result)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.Plan == nil || actual.Status.Plan.Approval == nil {
		t.Fatal("approval was not reserved")
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("Apply was claimed in the approval reservation pass: %#v", actual.Status.ActiveOperation)
	}

	actual.Spec.Desired.OCIRef = "oci://registry.example/team/schema:new"
	actual.Generation++
	if err := api.Update(context.Background(), actual); err != nil {
		t.Fatalf("change desired generation: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("fresh-generation Reconcile() error = %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("changed generation claimed %#v, want Resolve before Apply", actual.Status.ActiveOperation)
	}
}

func TestPostApplyObservationProofSurvivesInterruptions(t *testing.T) {
	t.Parallel()

	t.Run("operation retry", func(t *testing.T) {
		t.Parallel()

		schema := safetyPostApplyObserveSchema(t)
		job, pod := terminalWorkload(schema, batchv1.JobComplete)
		frame := safetyRunnerFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion,
			Operation:       runner.OperationObserve,
			OperationID:     schema.Status.ActiveOperation.ID,
			ChildExitCode:   0,
			Error:           &runner.ResultError{Code: "execution_error", Message: "temporary observation failure"},
		})
		reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
		reconciler.Locks = targetlock.New(api, api, nil)

		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual := safetyGetSchema(t, api, schema)
		safetyAssertPendingProof(t, actual)
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve || actual.Status.ActiveOperation.Attempt != 2 {
			t.Fatalf("ActiveOperation = %#v, want Observe retry attempt 2", actual.Status.ActiveOperation)
		}
		if actual.Status.Applied != nil {
			t.Fatalf("Applied = %#v, want no convergence claim", actual.Status.Applied)
		}
	})

	t.Run("suspension", func(t *testing.T) {
		t.Parallel()

		schema := safetyPostApplyObserveSchema(t)
		schema.Status.ActiveOperation = nil
		schema.Spec.Suspend = true
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		reconciler.Locks = targetlock.New(api, api, nil)

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if result.RequeueAfter != maxLockContentionPoll {
			t.Fatalf("suspended proof requeue = %s, want Lease renewal cadence %s", result.RequeueAfter, maxLockContentionPoll)
		}
		actual := safetyGetSchema(t, api, schema)
		safetyAssertPendingProof(t, actual)
		if actual.Status.Phase != operatorv1alpha1.PhaseSuspended {
			t.Fatalf("Phase = %q, want %q", actual.Status.Phase, operatorv1alpha1.PhaseSuspended)
		}
		if actual.Status.Applied != nil {
			t.Fatalf("Applied = %#v, want no convergence claim", actual.Status.Applied)
		}
		leaseName, err := targetlock.LeaseName(testCoordinationDigest)
		if err != nil {
			t.Fatal(err)
		}
		lease := &coordinationv1.Lease{}
		if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
			t.Fatalf("suspended proof Lease: %v", err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			t.Fatal("suspended proof released its database-realm Lease")
		}
	})

	t.Run("target mismatch", func(t *testing.T) {
		t.Parallel()

		schema := safetyPostApplyObserveSchema(t)
		job, pod := terminalWorkload(schema, batchv1.JobComplete)
		frame := safetyRunnerFrame(t, runner.Result{
			ProtocolVersion:      runner.ProtocolVersion,
			Operation:            runner.OperationObserve,
			OperationID:          schema.Status.ActiveOperation.ID,
			ChildExitCode:        0,
			CoordinationDigest:   testCoordinationDigest,
			TargetIdentityDigest: safetyOtherDigest,
			DriftReportDigest:    safetyOtherDigest,
			ObservedDialect:      "postgresql",
		})
		reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
		reconciler.Locks = targetlock.New(api, api, nil)

		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual := safetyGetSchema(t, api, schema)
		safetyAssertPendingProof(t, actual)
		if actual.Status.Applied != nil {
			t.Fatalf("Applied = %#v, want mismatched target evidence rejected", actual.Status.Applied)
		}
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != 2 {
			t.Fatalf("ActiveOperation = %#v, want observation retry", actual.Status.ActiveOperation)
		}
	})
}

func TestPendingProofRenewsLeaseDuringLongRetryDelay(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	clock := &safetyClock{now: started}
	schema := safetyPostApplyObserveSchema(t)
	schema.Status.Phase = operatorv1alpha1.PhaseFailed
	next := metav1.NewTime(started.Add(time.Hour))
	schema.Status.NextReconciliationTime = &next

	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Clock = clock.Now
	reconciler.Locks = targetlock.New(api, api, clock)
	acquired, _, err := reconciler.acquirePendingObservationLock(context.Background(), schema)
	if err != nil || !acquired {
		t.Fatalf("initial pending lock = acquired %v, error %v", acquired, err)
	}

	clock.now = started.Add(15 * time.Minute)
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("retry requeue = %s, want lock-renewal cap %s", result.RequeueAfter, maxLockContentionPoll)
	}

	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
		t.Fatalf("Get(Lease) error = %v", err)
	}
	if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(clock.now) {
		t.Fatalf("RenewTime = %#v, want %s", lease.Spec.RenewTime, clock.now)
	}
}

func TestApplyLockDurationIsImmutableAfterClaim(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Spec.Execution.ActiveDeadlineSeconds = 300
	schema.Status.Phase = operatorv1alpha1.PhaseReadyToApply
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Fingerprint:          "plan-fingerprint",
		ContentDigest:        testDigest,
		CoordinationDigest:   testCoordinationDigest,
		TargetIdentityDigest: testDigest,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.claim(context.Background(), schema, operatorv1alpha1.OperationApply); err != nil {
		t.Fatalf("claim(Apply) error = %v", err)
	}
	claimed := safetyGetSchema(t, api, schema)
	if claimed.Status.ActiveOperation == nil {
		t.Fatal("Apply claim did not persist ActiveOperation")
	}
	const wantedDuration = int32(360)
	if claimed.Status.ActiveOperation.LeaseDurationSeconds != wantedDuration {
		t.Fatalf("persisted lease duration = %d, want %d", claimed.Status.ActiveOperation.LeaseDurationSeconds, wantedDuration)
	}

	claimed.Spec.Execution.ActiveDeadlineSeconds = 5
	if err := api.Update(context.Background(), claimed); err != nil {
		t.Fatalf("Update(PtahSchema) error = %v", err)
	}
	mutated := safetyGetSchema(t, api, schema)
	acquired, _, err := reconciler.acquireApplyLock(context.Background(), mutated)
	if err != nil {
		t.Fatalf("acquireApplyLock() error = %v", err)
	}
	if !acquired {
		mutated = safetyGetSchema(t, api, schema)
		acquired, _, err = reconciler.acquireApplyLock(context.Background(), mutated)
		if err != nil {
			t.Fatalf("second acquireApplyLock() error = %v", err)
		}
	}
	if !acquired {
		t.Fatal("acquireApplyLock() did not acquire the target lock")
	}

	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatalf("LeaseName() error = %v", err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
		t.Fatalf("Get(Lease) error = %v", err)
	}
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds != wantedDuration {
		t.Fatalf("LeaseDurationSeconds = %#v, want persisted %d", lease.Spec.LeaseDurationSeconds, wantedDuration)
	}
}

func TestTargetLockContentionRequeueIsCapped(t *testing.T) {
	t.Parallel()

	first := safetyApplySchema(t)
	first.Status.ActiveOperation.ID = "first-operation"
	first.Status.ActiveOperation.LeaseDurationSeconds = 960
	first.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
	second := safetyApplySchema(t)
	second.Name = "second"
	second.UID = "second-schema-uid"
	second.Status.ActiveOperation.ID = "second-operation"
	second.Status.ActiveOperation.LeaseDurationSeconds = 960
	second.Status.ActiveOperation.LeaseEpoch = testLeaseEpochOther

	reconciler, api := fakeReconciler(t, staticLogs{}, first, second)
	reconciler.Locks = targetlock.New(api, api, nil)
	acquired, _, err := reconciler.acquireApplyLock(context.Background(), first)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	if !acquired {
		t.Fatal("first lock was not acquired")
	}

	acquired, requeueAfter, err := reconciler.acquireApplyLock(context.Background(), second)
	if err != nil {
		t.Fatalf("acquire contending lock: %v", err)
	}
	if acquired {
		t.Fatal("contending lock was acquired")
	}
	if requeueAfter != maxLockContentionPoll {
		t.Fatalf("contention requeue = %s, want cap %s", requeueAfter, maxLockContentionPoll)
	}
}

func TestFreshLeaseEpochPersistenceAlwaysSchedulesJobDispatch(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	schema.Finalizers = nil
	schema.Status.ActiveOperation = nil
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Status.Target.DriftReportDigest = safetyOtherDigest
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Locks = targetlock.New(api, api, nil)

	stored := safetyGetSchema(t, api, schema)
	claimResult, err := reconciler.claim(context.Background(), stored, operatorv1alpha1.OperationPlan)
	if err != nil {
		t.Fatalf("claim(Plan) error = %v", err)
	}
	if !claimResult.Requeue {
		t.Fatalf("claim(Plan) result = %#v, want an immediate reconciliation", claimResult)
	}

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if result.RequeueAfter != statusPatchRequeue {
		t.Fatalf("epoch persistence result = %#v, want explicit requeue %s", result, statusPatchRequeue)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("first epoch persistence created %d Jobs", len(jobs.Items))
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("admission snapshot persistence created %d Jobs", len(jobs.Items))
	}
	afterSnapshot := safetyGetSchema(t, api, schema)
	if afterSnapshot.Status.ActiveOperation == nil || afterSnapshot.Status.ActiveOperation.AdmissionSnapshot == nil ||
		afterSnapshot.Status.ActiveOperation.DispatchStarted {
		t.Fatalf("admission boundary = %#v, want persisted snapshot before dispatch", afterSnapshot.Status.ActiveOperation)
	}
	boundDigest := afterSnapshot.Status.ActiveOperation.AdmissionSnapshot.Digest
	serviceAccount := &corev1.ServiceAccount{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: "default"}, serviceAccount); err != nil {
		t.Fatalf("Get(default ServiceAccount) error = %v", err)
	}
	serviceAccount.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "changed-after-snapshot"}}
	if err := api.Update(context.Background(), serviceAccount); err != nil {
		t.Fatalf("mutate ServiceAccount after admission snapshot: %v", err)
	}

	// A fresh reconciler has no in-memory admission state. It must dispatch from
	// the persisted snapshot without re-resolving changed cluster resources.
	restarted := *reconciler
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("third reconciliation created %d Jobs, want 1", len(jobs.Items))
	}
	if got := jobs.Items[0].Annotations[workload.AnnotationAdmissionSnapshotDigest]; got != boundDigest {
		t.Fatalf("restarted dispatch admission digest = %q, want persisted %q", got, boundDigest)
	}
	afterDispatch := safetyGetSchema(t, api, schema)
	if got := afterDispatch.Status.ActiveOperation.AdmissionSnapshot.Digest; got != boundDigest {
		t.Fatalf("restarted reconciliation replaced admission digest = %q, want %q", got, boundDigest)
	}
}

func TestPendingProofEpochRecoveryAlwaysSchedulesProgress(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	schema.Finalizers = nil
	schema.Status.ActiveOperation = nil
	schema.Status.PendingObservation.LeaseEpoch = ""
	past := metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC))
	schema.Status.PendingObservation.ObserveAfter = &past
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Locks = targetlock.New(api, api, nil)

	stored := safetyGetSchema(t, api, schema)
	stored.Status.PendingObservation.LeaseEpoch = testLeaseEpoch
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("persist stale pending epoch: %v", err)
	}

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if result.RequeueAfter != statusPatchRequeue {
		t.Fatalf("pending epoch recovery result = %#v, want explicit requeue %s", result, statusPatchRequeue)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	progressed := safetyGetSchema(t, api, schema)
	if progressed.Status.ActiveOperation == nil || progressed.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("pending proof did not claim a fresh observation: %#v", progressed.Status)
	}
}

func safetyApplySchema(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	schema.Status.Target.CoordinationDigest = testCoordinationDigest
	schema.Status.Target.IdentityDigest = testDigest
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Fingerprint:          "plan-fingerprint",
		ContentDigest:        testDigest,
		ArtifactDigest:       testDigest,
		CoordinationDigest:   testCoordinationDigest,
		TargetIdentityDigest: testDigest,
	}
	target := databaseTargetBinding(schema.Spec.Target)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:                 operatorv1alpha1.OperationApply,
		ID:                   "apply-operation",
		JobName:              "apply-job",
		StartedAt:            metav1.Now(),
		Attempt:              1,
		CoordinationDigest:   testCoordinationDigest,
		TargetIdentityDigest: testDigest,
		Target:               &target,
		LeaseDurationSeconds: 960,
	}
	bindActiveInput(t, schema)
	return schema
}

func safetyLockedOperationSchema(operation operatorv1alpha1.OperationType) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	if operation == operatorv1alpha1.OperationApply {
		schema.Status.Phase = operatorv1alpha1.PhaseApplying
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:                 operation,
		ID:                   "durable-release-operation",
		JobName:              "missing-durable-release-job",
		StartedAt:            metav1.Now(),
		Attempt:              1,
		CoordinationDigest:   testCoordinationDigest,
		TargetIdentityDigest: testDigest,
		LeaseDurationSeconds: 960,
		LeaseEpoch:           testLeaseEpoch,
	}
	return schema
}

func safetyLeaseHolder(
	t *testing.T,
	api client.Client,
	namespace string,
	coordinationDigest string,
) string {
	t.Helper()
	leaseName, err := targetlock.LeaseName(coordinationDigest)
	if err != nil {
		t.Fatalf("LeaseName() error = %v", err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: leaseName}, lease); err != nil {
		t.Fatalf("Get(Lease) error = %v", err)
	}
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

type safetyClock struct {
	now time.Time
}

func (c *safetyClock) Now() time.Time { return c.now }

func safetyPostApplyObserveSchema(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
		Digest:                   testDigest,
		Verified:                 true,
		ArtifactType:             "application/vnd.stokaro.ptah.schema.v1",
		VerificationPolicyUID:    testPolicyUID,
		VerificationPolicyDigest: testDigest,
	}
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome:          operatorv1alpha1.PendingObservationApplySucceeded,
		ApplyOperationID: "apply-operation",
		ApplyJobName:     "apply-job",
		ApplyJobUID:      "apply-job-uid",
		ApplyGeneration:  schema.Generation,
		Plan: operatorv1alpha1.CurrentPlanStatus{
			Fingerprint:              "plan-fingerprint",
			ContentDigest:            testDigest,
			ArtifactDigest:           testDigest,
			CoordinationDigest:       testCoordinationDigest,
			TargetIdentityDigest:     testDigest,
			VerificationPolicyUID:    testPolicyUID,
			VerificationPolicyDigest: testDigest,
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "example.invalid/ptah@" + testDigest,
			RunnerImage:              "example.invalid/operator@" + testDigest,
			RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		},
		Source: operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: schema.Status.Source.ResolvedReference,
			Digest:            schema.Status.Source.Digest,
		},
		Target:               databaseTargetBinding(schema.Spec.Target),
		CoordinationDigest:   testCoordinationDigest,
		LeaseDurationSeconds: 960,
		LeaseEpoch:           testLeaseEpoch,
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:      operatorv1alpha1.OperationObserve,
		ID:        "observe-operation",
		JobName:   "observe-job",
		JobUID:    "job-uid",
		StartedAt: metav1.Now(),
		Attempt:   1,
	}
	bindActiveInput(t, schema)
	return schema
}

func safetyAssertPendingProof(t *testing.T, schema *operatorv1alpha1.PtahSchema) {
	t.Helper()
	if schema.Status.PendingObservation == nil {
		t.Fatal("PendingObservation was lost")
	}
	if schema.Status.PendingObservation.ApplyOperationID != "apply-operation" ||
		schema.Status.PendingObservation.Plan.TargetIdentityDigest != testDigest {
		t.Fatalf("PendingObservation binding changed: %#v", schema.Status.PendingObservation)
	}
}

func safetyPlanDocument(t *testing.T, fromFingerprint string) []byte {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"format_version":   1,
		"name":             "safety-plan",
		"dialect":          "postgresql",
		"from_fingerprint": fromFingerprint,
		"to_fingerprint":   "desired-state",
		"destructive":      false,
		"statements": []map[string]any{{
			"sql": "CREATE TABLE safety_test (id bigint)", "severity": "safe", "reason": "test",
		}},
	})
	if err != nil {
		t.Fatalf("Marshal(plan) error = %v", err)
	}
	return document
}

func safetyRunnerFrame(t *testing.T, result runner.Result) []byte {
	t.Helper()
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	return frame
}

func safetyGetSchema(t *testing.T, api client.Client, expected *operatorv1alpha1.PtahSchema) *operatorv1alpha1.PtahSchema {
	t.Helper()
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(expected), actual); err != nil {
		t.Fatalf("Get(PtahSchema) error = %v", err)
	}
	return actual
}
