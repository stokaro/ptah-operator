package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	approvaladmission "github.com/stokaro/ptah-operator/internal/admission"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
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

type replaceJobBeforeCleanupPatchClient struct {
	client.Client
	replacementUID types.UID
	replaced       bool
}

func (c *replaceJobBeforeCleanupPatchClient) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	job, ok := object.(*batchv1.Job)
	if !ok || job.Spec.TTLSecondsAfterFinished == nil || c.replaced {
		return c.Client.Patch(ctx, object, patch, options...)
	}
	c.replaced = true
	current := &batchv1.Job{}
	key := client.ObjectKeyFromObject(job)
	if err := c.Client.Get(ctx, key, current); err != nil {
		return err
	}
	if err := c.Client.Delete(ctx, current); err != nil {
		return err
	}
	replacement := current.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = c.replacementUID
	replacement.Spec.TTLSecondsAfterFinished = nil
	replacement.OwnerReferences = nil
	if err := c.Client.Create(ctx, replacement); err != nil {
		return err
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

type safetyCountingLogs struct {
	content []byte
	reads   int
}

func (l *safetyCountingLogs) Read(context.Context, string, string, string) ([]byte, error) {
	l.reads++
	return append([]byte(nil), l.content...), nil
}

func bindRetiredReadOnlyJob(job *batchv1.Job, operation *operatorv1alpha1.ActiveOperationStatus) {
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Labels[workload.LabelOperation] = strings.ToLower(string(operation.Type))
	job.Annotations[workload.AnnotationOperationID] = operation.ID
	job.Annotations[workload.AnnotationExecutionBindingID] = operation.ExecutionBindingID
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

type failFirstLeaseUpdateWriter struct {
	client.Writer
	failed bool
}

func (w *failFirstLeaseUpdateWriter) Update(
	ctx context.Context,
	object client.Object,
	options ...client.UpdateOption,
) error {
	if _, ok := object.(*coordinationv1.Lease); ok && !w.failed {
		w.failed = true
		return errors.New("injected Lease update failure")
	}
	return w.Writer.Update(ctx, object, options...)
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

func TestApprovalRevocationLockReleaseSurvivesFailureAndRestart(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	next := metav1.NewTime(time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC))
	schema.Status.NextReconciliationTime = &next
	schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
		Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
	}
	target := databaseTargetBinding(schema.Spec.Target)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "revoked-approval-apply", JobName: "revoked-approval-apply-job",
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)), Attempt: 1,
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest, Target: &target,
		LeaseDurationSeconds: 960, LeaseEpoch: testLeaseEpoch,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
	failingWriter := &failFirstLeaseUpdateWriter{Writer: api}
	reconciler.Locks = targetlock.New(api, failingWriter, nil)
	current := safetyGetSchema(t, api, schema)

	if _, err := reconciler.approvalBecameInvalid(context.Background(), current); err == nil {
		t.Fatal("approvalBecameInvalid() succeeded despite injected Lease release failure")
	}
	if !failingWriter.failed {
		t.Fatal("approvalBecameInvalid() did not attempt to release the held Lease")
	}
	persisted := safetyGetSchema(t, api, schema)
	if persisted.Status.ActiveOperation != nil || persisted.Status.PendingLockRelease == nil ||
		persisted.Status.Plan == nil || persisted.Status.Plan.Approval != nil ||
		!safetyTimesEqual(persisted.Status.NextReconciliationTime, &next) {
		t.Fatalf("failed release boundary was not durable: %#v", persisted.Status)
	}
	if !contains(persisted.Finalizers, activeOperationFinalizer) {
		t.Fatal("failed release removed the active-operation finalizer")
	}
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder == "" {
		t.Fatal("failed release cleared the held Lease")
	}

	restarted := *reconciler
	restarted.Locks = targetlock.New(api, api, nil)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	result, err := restarted.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("release restart Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("release restart result = %#v, want finalizer-cleanup pass", result)
	}
	drained := safetyGetSchema(t, api, schema)
	if drained.Status.PendingLockRelease != nil || !safetyTimesEqual(drained.Status.NextReconciliationTime, &next) {
		t.Fatalf("restart did not drain only the release marker: %#v", drained.Status)
	}
	if holder := safetyLeaseHolder(t, api, restarted.LockNamespace, testCoordinationDigest); holder != "" {
		t.Fatalf("restart left Lease holder %q", holder)
	}

	result, err = restarted.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("finalizer cleanup Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 4*time.Minute {
		t.Fatalf("finalizer cleanup result = %#v, want original refresh deadline", result)
	}
	completed := safetyGetSchema(t, api, schema)
	if contains(completed.Finalizers, activeOperationFinalizer) || completed.Status.PendingLockRelease != nil ||
		completed.Status.ActiveOperation != nil || !safetyTimesEqual(completed.Status.NextReconciliationTime, &next) {
		t.Fatalf("restart cleanup changed refresh state: %#v", completed)
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

func TestDeferredPlanConsumptionPersistsRefreshDeadlineAtomically(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name           string
		apply          operatorv1alpha1.ApplyPolicy
		destructive    bool
		phase          operatorv1alpha1.ReconciliationPhase
		approvalStatus metav1.ConditionStatus
		approvalReason string
	}{
		{
			name: "destructive changes disabled", apply: operatorv1alpha1.ApplyPolicyAlways, destructive: true,
			phase: operatorv1alpha1.PhaseBlocked, approvalStatus: metav1.ConditionFalse, approvalReason: "DestructiveChangesDisabled",
		},
		{
			name: "apply disabled", apply: operatorv1alpha1.ApplyPolicyNever,
			phase: operatorv1alpha1.PhaseBlocked, approvalStatus: metav1.ConditionFalse, approvalReason: "ApplyDisabled",
		},
		{
			name: "approval required", apply: operatorv1alpha1.ApplyPolicyOnApproval,
			phase: operatorv1alpha1.PhaseAwaitingApproval, approvalStatus: metav1.ConditionTrue, approvalReason: "PlanReady",
		},
		{
			name: "automatic apply", apply: operatorv1alpha1.ApplyPolicyAlways,
			phase: operatorv1alpha1.PhaseReadyToApply, approvalStatus: metav1.ConditionFalse, approvalReason: "NotRequired",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policyBytes := "policy"
			policyDigest := fingerprint.DigestBytes([]byte(policyBytes))
			schema := schemaFixture()
			schema.Spec.Interval = metav1.Duration{Duration: 8 * time.Minute}
			schema.Spec.Policy.Apply = test.apply
			schema.Spec.Policy.AllowDestructive = false
			schema.Finalizers = []string{activeOperationFinalizer}
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
				Digest:                   testDigest,
				ArtifactType:             dataplane.SchemaArtifactType,
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
				ID:        "blocked-plan-operation",
				JobName:   "blocked-plan-job",
				JobUID:    "job-uid",
				StartedAt: metav1.Now(),
				Attempt:   1,
			}
			bindActiveInput(t, schema)
			planDocument := safetyPlanDocument(t, "observed-state", test.destructive)
			frame := safetyRunnerFrame(t, runner.Result{
				ProtocolVersion:      runner.ProtocolVersion,
				Operation:            runner.OperationPlan,
				OperationID:          schema.Status.ActiveOperation.ID,
				ChildExitCode:        0,
				Stdout:               string(planDocument),
				CoordinationDigest:   schema.Status.Target.CoordinationDigest,
				TargetIdentityDigest: schema.Status.Target.IdentityDigest,
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
			reconciler.Plans = planstore.Store{Client: api, Reader: api}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want persisted plan-policy transition", result)
			}
			actual := safetyGetSchema(t, api, schema)
			wantNext := reconciler.now().Add(schema.Spec.Interval.Duration)
			approval := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
			if actual.Status.ActiveOperation != nil || actual.Status.Phase != test.phase ||
				actual.Status.Plan == nil || actual.Status.Plan.Destructive != test.destructive ||
				actual.Status.NextReconciliationTime == nil ||
				!actual.Status.NextReconciliationTime.Time.Equal(wantNext) ||
				approval == nil || approval.Status != test.approvalStatus || approval.Reason != test.approvalReason {
				t.Fatalf("atomic deferred Plan status = %#v, want phase %s, deadline %s, and approval reason %s", actual.Status, test.phase, wantNext, test.approvalReason)
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

func TestAwaitingApprovalWaitsForPersistedRefreshDeadlineWithoutSliding(t *testing.T) {
	t.Parallel()

	schema, plan, _, policyConfig := safetyApprovalFixture(t)
	next := metav1.NewTime(time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC))
	schema.Status.NextReconciliationTime = &next
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	reconciler.Clock = func() time.Time { return now }
	wantPlan := safetyGetSchema(t, api, schema).Status.Plan.DeepCopy()
	var firstResourceVersion string
	var firstApprovalTransition metav1.Time

	for reconciliation := 1; reconciliation <= 2; reconciliation++ {
		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", reconciliation, err)
		}
		if result.Requeue || result.RequeueAfter != next.Sub(now) {
			t.Fatalf("Reconcile() pass %d result = %#v, want persisted-deadline wait", reconciliation, result)
		}
		actual := safetyGetSchema(t, api, schema)
		if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval ||
			actual.Status.NextReconciliationTime == nil || !actual.Status.NextReconciliationTime.Time.Equal(next.Time) ||
			!reflect.DeepEqual(actual.Status.Plan, wantPlan) {
			t.Fatalf("AwaitingApproval status changed before deadline on pass %d: %#v", reconciliation, actual.Status)
		}
		approvalRequired := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
		if approvalRequired == nil || approvalRequired.Reason != "Waiting" {
			t.Fatalf("ApprovalRequired condition on pass %d = %#v, want Waiting", reconciliation, approvalRequired)
		}
		if reconciliation == 1 {
			firstResourceVersion = actual.ResourceVersion
			firstApprovalTransition = approvalRequired.LastTransitionTime
		} else if actual.ResourceVersion != firstResourceVersion ||
			!approvalRequired.LastTransitionTime.Time.Equal(firstApprovalTransition.Time) {
			t.Fatalf(
				"unchanged wait mutated object: resourceVersion %q -> %q, approval transition %s -> %s",
				firstResourceVersion, actual.ResourceVersion, firstApprovalTransition.Time, approvalRequired.LastTransitionTime.Time,
			)
		}
		now = now.Add(time.Minute)
	}
}

func TestExpiredAwaitingApprovalRefreshesBeforeExactApproval(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		setDeadline bool
	}{
		{name: "due deadline", setDeadline: true},
		{name: "legacy missing deadline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			if test.setDeadline {
				dueAt := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
				schema.Status.NextReconciliationTime = &dueAt
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)
			wantPlan := safetyGetSchema(t, api, schema).Status.Plan.DeepCopy()

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want Resolve claim", result)
			}
			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				actual.Status.Phase != operatorv1alpha1.PhaseResolving || actual.Status.NextReconciliationTime != nil ||
				!reflect.DeepEqual(actual.Status.Plan, wantPlan) || actual.Status.Plan.Approval != nil {
				t.Fatalf("expired AwaitingApproval status = %#v, want preserved plan and Resolve before approval", actual.Status)
			}
			safetyAssertApprovalNotAuthorized(t, api, approval)
		})
	}
}

func TestReadyToApplyRestartRefreshesExpiredPlanBeforeAutomaticApply(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		setDeadline bool
	}{
		{name: "deadline reached while manager was down", setDeadline: true},
		{name: "legacy missing deadline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, policyConfig := safetyReadyToApplyFixture(t)
			if test.setDeadline {
				dueAt := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
				schema.Status.NextReconciliationTime = &dueAt
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
			wantPlan := safetyGetSchema(t, api, schema).Status.Plan.DeepCopy()
			restarted := *reconciler

			result, err := restarted.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("restart Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("restart Reconcile() result = %#v, want Resolve claim", result)
			}
			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				actual.Status.Phase != operatorv1alpha1.PhaseResolving || actual.Status.NextReconciliationTime != nil ||
				!reflect.DeepEqual(actual.Status.Plan, wantPlan) || actual.Status.Plan.Approval != nil {
				t.Fatalf("expired automatic plan was authorized: %#v, want preserved plan and Resolve", actual.Status)
			}
		})
	}
}

func TestReadyToApplyAuthorizationUsesOneDurableTimestamp(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 8, 30, 12, 1, 0, 0, time.UTC)
	for _, test := range []struct {
		name               string
		clockOffsets       []time.Duration
		expectedOperation  operatorv1alpha1.OperationType
		expectedStartedAt  time.Time
		expectedClockCalls int
	}{
		{
			name: "automatic Apply claimed before deadline", clockOffsets: []time.Duration{-2 * time.Second, -time.Second},
			expectedOperation: operatorv1alpha1.OperationApply,
			expectedStartedAt: deadline.Add(-time.Second), expectedClockCalls: 2,
		},
		{
			name: "deadline reached at automatic Apply boundary", clockOffsets: []time.Duration{-time.Second, 0},
			expectedOperation: operatorv1alpha1.OperationResolve,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, policyConfig := safetyReadyToApplyFixture(t)
			next := metav1.NewTime(deadline)
			schema.Status.NextReconciliationTime = &next
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
			wantPlan := safetyGetSchema(t, api, schema).Status.Plan.DeepCopy()
			clockCalls := 0
			reconciler.Clock = func() time.Time {
				clockCalls++
				if clockCalls <= len(test.clockOffsets) {
					return deadline.Add(test.clockOffsets[clockCalls-1])
				}
				return deadline
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want operation claim", result)
			}
			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != test.expectedOperation ||
				actual.Status.NextReconciliationTime != nil || !reflect.DeepEqual(actual.Status.Plan, wantPlan) ||
				actual.Status.Plan.Approval != nil {
				t.Fatalf("automatic authorization boundary = %#v, want preserved plan and %s", actual.Status, test.expectedOperation)
			}
			if !test.expectedStartedAt.IsZero() &&
				(!actual.Status.ActiveOperation.StartedAt.Time.Equal(test.expectedStartedAt) ||
					!actual.Status.ActiveOperation.StartedAt.Time.Before(deadline) ||
					clockCalls != test.expectedClockCalls) {
				t.Fatalf(
					"automatic Apply = started %s with %d clock calls, want %s with %d calls",
					actual.Status.ActiveOperation.StartedAt.Time, clockCalls, test.expectedStartedAt, test.expectedClockCalls,
				)
			}
		})
	}
}

func TestAwaitingApprovalAuthorizationUsesOneDurableTimestamp(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name               string
		recordedApproval   bool
		clockOffsets       []time.Duration
		expectedOperation  operatorv1alpha1.OperationType
		expectedStartedAt  time.Time
		expectedClockCalls int
	}{
		{
			name: "exact approval reaches deadline before reservation", clockOffsets: []time.Duration{-time.Second, 0},
			expectedOperation: operatorv1alpha1.OperationResolve,
		},
		{
			name: "recorded approval persists its checked pre-deadline time", recordedApproval: true,
			clockOffsets:      []time.Duration{-2 * time.Second, -time.Second},
			expectedOperation: operatorv1alpha1.OperationApply,
			expectedStartedAt: time.Date(2026, 8, 30, 12, 0, 59, 0, time.UTC), expectedClockCalls: 2,
		},
		{
			name: "recorded approval reaches deadline at claim boundary", recordedApproval: true,
			clockOffsets: []time.Duration{-time.Second, 0}, expectedOperation: operatorv1alpha1.OperationResolve,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			deadline := time.Date(2026, 8, 30, 12, 1, 0, 0, time.UTC)
			next := metav1.NewTime(deadline)
			schema.Status.NextReconciliationTime = &next
			if test.recordedApproval {
				schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
					Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
				}
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)
			wantPlan := safetyGetSchema(t, api, schema).Status.Plan.DeepCopy()
			clockCalls := 0
			reconciler.Clock = func() time.Time {
				clockCalls++
				if clockCalls <= len(test.clockOffsets) {
					return deadline.Add(test.clockOffsets[clockCalls-1])
				}
				return deadline
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want operation claim", result)
			}
			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != test.expectedOperation ||
				actual.Status.NextReconciliationTime != nil ||
				!reflect.DeepEqual(actual.Status.Plan, wantPlan) {
				t.Fatalf("authorization boundary status = %#v, want preserved plan and %s", actual.Status, test.expectedOperation)
			}
			if !test.expectedStartedAt.IsZero() &&
				(!actual.Status.ActiveOperation.StartedAt.Time.Equal(test.expectedStartedAt) ||
					!actual.Status.ActiveOperation.StartedAt.Time.Before(deadline) ||
					clockCalls != test.expectedClockCalls) {
				t.Fatalf(
					"Apply authorization = started %s with %d clock calls, want %s with %d calls",
					actual.Status.ActiveOperation.StartedAt.Time, clockCalls, test.expectedStartedAt, test.expectedClockCalls,
				)
			}
			if test.expectedOperation == operatorv1alpha1.OperationResolve {
				safetyAssertApprovalNotAuthorized(t, api, approval)
			}
		})
	}
}

func TestApprovalRevocationPreservesRefreshDeadlineAcrossRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name          string
		deadline      *metav1.Time
		initialResult ctrl.Result
	}{
		{
			name: "future deadline", deadline: ptrTime(metav1.NewTime(now.Add(4 * time.Minute))),
			initialResult: ctrl.Result{RequeueAfter: 4 * time.Minute},
		},
		{name: "due deadline", deadline: ptrTime(metav1.NewTime(now)), initialResult: ctrl.Result{Requeue: true}},
		{name: "legacy missing deadline", initialResult: ctrl.Result{Requeue: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			if test.deadline != nil {
				schema.Status.NextReconciliationTime = test.deadline.DeepCopy()
			}
			schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
				Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
			current := safetyGetSchema(t, api, schema)

			result, err := reconciler.approvalBecameInvalid(context.Background(), current)
			if err != nil {
				t.Fatalf("approvalBecameInvalid() error = %v", err)
			}
			if result != test.initialResult {
				t.Fatalf("approvalBecameInvalid() result = %#v, want %#v", result, test.initialResult)
			}
			afterRevocation := safetyGetSchema(t, api, schema)
			if afterRevocation.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval ||
				afterRevocation.Status.Plan == nil || afterRevocation.Status.Plan.Approval != nil ||
				!safetyTimesEqual(afterRevocation.Status.NextReconciliationTime, test.deadline) {
				t.Fatalf("approval revocation changed refresh evidence: %#v", afterRevocation.Status)
			}

			restarted := *reconciler
			restartResult, err := restarted.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("restart Reconcile() error = %v", err)
			}
			afterRestart := safetyGetSchema(t, api, schema)
			if test.deadline != nil && test.deadline.After(now) {
				if restartResult != test.initialResult || afterRestart.Status.ActiveOperation != nil ||
					!safetyTimesEqual(afterRestart.Status.NextReconciliationTime, test.deadline) {
					t.Fatalf("restart slid future approval deadline: result %#v, status %#v", restartResult, afterRestart.Status)
				}
				return
			}
			if !restartResult.Requeue || afterRestart.Status.ActiveOperation == nil ||
				afterRestart.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				afterRestart.Status.NextReconciliationTime != nil {
				t.Fatalf("restart did not refresh due approval state: result %#v, status %#v", restartResult, afterRestart.Status)
			}
		})
	}
}

func safetyTimesEqual(first, second *metav1.Time) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Time.Equal(second.Time)
}

func safetyAssertApprovalNotAuthorized(
	t *testing.T,
	api client.Client,
	approval *operatorv1alpha1.PtahSchemaApproval,
) {
	t.Helper()

	persisted := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persisted); err != nil {
		t.Fatal(err)
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionApprovalAccepted,
		operatorv1alpha1.ConditionApprovalConsumed,
	} {
		if condition := findCondition(persisted.Status.Conditions, conditionType); condition != nil && condition.Status == metav1.ConditionTrue {
			t.Fatalf("approval condition %s = %#v, want authorization untouched", conditionType, condition)
		}
	}
}

func TestRequeueAtDeadlineNeverReturnsAnEmptyDueResult(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	dueAt := metav1.NewTime(now)
	future := metav1.NewTime(now.Add(2 * time.Minute))
	for _, test := range []struct {
		name   string
		next   *metav1.Time
		result ctrl.Result
	}{
		{name: "future", next: &future, result: ctrl.Result{RequeueAfter: 2 * time.Minute}},
		{name: "due", next: &dueAt, result: ctrl.Result{Requeue: true}},
		{name: "missing", result: ctrl.Result{Requeue: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if actual := requeueAtDeadline(test.next, now); actual != test.result {
				t.Fatalf("requeueAtDeadline() = %#v, want %#v", actual, test.result)
			}
		})
	}
}

func TestFailedActiveOperationUsesOneDeadlineSample(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	schema := schemaFixture()
	schema.Status.Phase = operatorv1alpha1.PhaseFailed
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "failed-resolve", JobName: "failed-resolve-job", Attempt: 1,
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	next := metav1.NewTime(now.Add(time.Second))
	schema.Status.NextReconciliationTime = &next
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	clockCalls := 0
	reconciler.Clock = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return now
		}
		return next.Time
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != time.Second || clockCalls != 1 {
		t.Fatalf("failed-operation wait = %#v with %d clock calls, want 1s from one sample", result, clockCalls)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.ID != schema.Status.ActiveOperation.ID {
		t.Fatalf("failed-operation wait changed active operation: %#v", actual.Status.ActiveOperation)
	}
}

func TestApprovalReservationRequiresFreshGenerationPassBeforeApply(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	next := metav1.NewTime(time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC))
	schema.Status.NextReconciliationTime = &next
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
	if actual.Status.NextReconciliationTime == nil || !actual.Status.NextReconciliationTime.Time.Equal(next.Time) {
		t.Fatalf("approval reservation moved refresh deadline: %#v, want %s", actual.Status.NextReconciliationTime, next.Time)
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

func TestExecutionBindingChangeInvalidatesPlanBeforeApply(t *testing.T) {
	t.Parallel()

	base := executionBindingJobs{
		controllerImage:        testControllerImage,
		ptahVersion:            "v0.3.0",
		executorImage:          "example.invalid/ptah@" + testDigest,
		runnerImage:            "example.invalid/operator@" + testDigest,
		protocol:               int32(runner.ProtocolVersion),
		controllerRevision:     testControllerRevision,
		controllerStateVersion: testControllerStateVersion,
	}
	changes := []struct {
		name   string
		mutate func(*executionBindingJobs)
	}{
		{name: "controller image", mutate: func(binding *executionBindingJobs) {
			binding.controllerImage = "example.invalid/manager@" + safetyOtherDigest
		}},
		{name: "Ptah version", mutate: func(binding *executionBindingJobs) { binding.ptahVersion = "v0.4.0" }},
		{name: "executor image", mutate: func(binding *executionBindingJobs) {
			binding.executorImage = "example.invalid/ptah@" + safetyOtherDigest
		}},
		{name: "runner image", mutate: func(binding *executionBindingJobs) {
			binding.runnerImage = "example.invalid/operator@" + safetyOtherDigest
		}},
		{name: "runner protocol", mutate: func(binding *executionBindingJobs) { binding.protocol++ }},
		{name: "controller revision", mutate: func(binding *executionBindingJobs) { binding.controllerRevision += "-next" }},
		{name: "controller state", mutate: func(binding *executionBindingJobs) { binding.controllerStateVersion++ }},
	}
	for _, change := range changes {
		change := change
		for _, mode := range []struct {
			name            string
			includeApproval bool
			reserveApproval bool
		}{
			{name: "recorded approval", includeApproval: true, reserveApproval: true},
			{name: "unreserved approval", includeApproval: true},
			{name: "automatic apply"},
		} {
			mode := mode
			t.Run(change.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				schema, plan, approval, policyConfig := safetyApprovalFixture(t)
				oldEpoch := schema.Status.ExecutionBinding.Epoch
				future := metav1.NewTime(time.Date(2026, 8, 30, 12, 10, 0, 0, time.UTC))
				schema.Status.NextReconciliationTime = &future
				objects := []client.Object{schema, plan, policyConfig}
				if mode.includeApproval {
					objects = append(objects, approval)
				} else {
					schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
					schema.Status.Phase = operatorv1alpha1.PhaseReadyToApply
				}
				reconciler, api := fakeReconciler(t, staticLogs{}, objects...)
				request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
				retiredPlanUID := schema.Status.Plan.UID
				if mode.reserveApproval {
					if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
						t.Fatalf("reserve approval: %v", err)
					}
					reserved := safetyGetSchema(t, api, schema)
					if reserved.Status.Plan == nil || reserved.Status.Plan.Approval == nil {
						t.Fatal("approval was not durably reserved before the binding change")
					}
				}

				changed := base
				change.mutate(&changed)
				reconciler.Jobs = changed
				result, err := reconciler.Reconcile(context.Background(), request)
				if err != nil {
					t.Fatalf("Reconcile() after execution binding change: %v", err)
				}
				if !result.Requeue {
					t.Fatalf("binding invalidation result = %#v, want immediate full refresh", result)
				}
				actual := safetyGetSchema(t, api, schema)
				if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil || actual.Status.Plan.UID != retiredPlanUID ||
					actual.Status.Phase != operatorv1alpha1.PhasePending ||
					actual.Status.NextReconciliationTime != nil || actual.Status.Source.Verified || actual.Status.Source.VerifiedAt != nil {
					t.Fatalf("durable binding fence status = %#v", actual.Status)
				}
				controllerImage, controllerRevision, controllerStateVersion, ptahVersion, executorImage, runnerImage, protocolVersion := changed.ExecutionBinding()
				wantBinding := &operatorv1alpha1.ExecutionBindingStatus{
					ControllerImage:    controllerImage,
					ControllerRevision: controllerRevision, ControllerStateVersion: controllerStateVersion,
					PtahVersion: ptahVersion, ExecutorImage: executorImage, RunnerImage: runnerImage,
					RunnerProtocolVersion: protocolVersion,
				}
				if actual.Status.ExecutionBinding == nil || actual.Status.ExecutionBinding.Epoch == oldEpoch ||
					!executionBindingComponentsEqual(actual.Status.ExecutionBinding, wantBinding) {
					t.Fatalf("replacement execution binding = %#v, want a fresh epoch for %#v", actual.Status.ExecutionBinding, wantBinding)
				}
				for _, conditionCheck := range []struct {
					conditionType string
					status        metav1.ConditionStatus
				}{
					{conditionType: operatorv1alpha1.ConditionArtifactResolved, status: metav1.ConditionUnknown},
					{conditionType: operatorv1alpha1.ConditionArtifactVerified, status: metav1.ConditionUnknown},
					{conditionType: operatorv1alpha1.ConditionDatabaseReachable, status: metav1.ConditionUnknown},
					{conditionType: operatorv1alpha1.ConditionDriftDetected, status: metav1.ConditionUnknown},
					{conditionType: operatorv1alpha1.ConditionPlanReady, status: metav1.ConditionFalse},
					{conditionType: operatorv1alpha1.ConditionInSync, status: metav1.ConditionUnknown},
					{conditionType: operatorv1alpha1.ConditionReady, status: metav1.ConditionFalse},
				} {
					condition := findCondition(actual.Status.Conditions, conditionCheck.conditionType)
					if condition == nil || condition.Status != conditionCheck.status || condition.Reason != "ExecutionBindingChanged" {
						t.Fatalf("%s = %#v, want %s/ExecutionBindingChanged", conditionCheck.conditionType, condition, conditionCheck.status)
					}
				}
				jobs := &batchv1.JobList{}
				if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 0 {
					t.Fatalf("binding change created %d Jobs before a fresh Resolve claim", len(jobs.Items))
				}
				if mode.includeApproval {
					persistedApproval := &operatorv1alpha1.PtahSchemaApproval{}
					if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persistedApproval); err != nil {
						t.Fatal(err)
					}
					if stale := findCondition(persistedApproval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale); stale != nil && stale.Status == metav1.ConditionTrue {
						t.Fatalf("approval was retired before the schema admission fence became durable: %#v", stale)
					}
				}

				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("retire fenced approvals: %v", err)
				}
				retired := safetyGetSchema(t, api, schema)
				if retired.Status.Plan != nil || retired.Status.ActiveOperation != nil || retired.Status.Phase != operatorv1alpha1.PhasePending {
					t.Fatalf("binding cleanup status = %#v", retired.Status)
				}
				if mode.includeApproval {
					persistedApproval := &operatorv1alpha1.PtahSchemaApproval{}
					if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persistedApproval); err != nil {
						t.Fatal(err)
					}
					stale := findCondition(persistedApproval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
					accepted := findCondition(persistedApproval.Status.Conditions, operatorv1alpha1.ConditionApprovalAccepted)
					if stale == nil || stale.Status != metav1.ConditionTrue || stale.Reason != "ExecutionBindingChanged" ||
						accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != "ExecutionBindingChanged" {
						t.Fatalf("binding-invalid approval conditions = stale %#v, accepted %#v", stale, accepted)
					}
				}

				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("fresh-chain Reconcile() error = %v", err)
				}
				refreshing := safetyGetSchema(t, api, schema)
				if refreshing.Status.ActiveOperation == nil || refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
					refreshing.Status.Phase != operatorv1alpha1.PhaseResolving {
					t.Fatalf("binding change did not start a full Resolve chain: %#v", refreshing.Status)
				}
			})
		}
	}
}

func TestExecutionBindingChangeBeforePlanRestartsFullChainAcrossRestart(t *testing.T) {
	t.Parallel()

	type testCase struct {
		phase          operatorv1alpha1.ReconciliationPhase
		missingBinding bool
	}
	var tests []testCase
	for _, phase := range []operatorv1alpha1.ReconciliationPhase{
		operatorv1alpha1.PhaseVerifying,
		operatorv1alpha1.PhaseObserving,
		operatorv1alpha1.PhasePlanning,
	} {
		tests = append(tests, testCase{phase: phase}, testCase{phase: phase, missingBinding: true})
	}
	for _, test := range tests {
		test := test
		bindingState := "changed"
		if test.missingBinding {
			bindingState = "missing"
		}
		t.Run(string(test.phase)+"/"+bindingState, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			schema.Status.Phase = test.phase
			schema.Status.Plan = nil
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				RequestedReference: schema.Spec.Desired.OCIRef,
				ResolvedReference:  "oci://registry.example/team/schema@" + testDigest,
				Digest:             testDigest,
				ArtifactType:       dataplane.SchemaArtifactType,
				Verified:           test.phase != operatorv1alpha1.PhaseVerifying,
			}
			if test.phase == operatorv1alpha1.PhasePlanning {
				schema.Status.Target = operatorv1alpha1.TargetStatus{
					Engine: schema.Spec.Target.Engine, CoordinationDigest: testCoordinationDigest,
					IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
				}
			}
			schema.Status.Applied = &operatorv1alpha1.AppliedStatus{
				ArtifactDigest: testDigest, PlanFingerprint: safetyOtherDigest,
				CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
				ExecutionBindingID:    schema.Status.ExecutionBinding.Epoch,
				PtahVersion:           schema.Status.ExecutionBinding.PtahVersion,
				ExecutorImage:         schema.Status.ExecutionBinding.ExecutorImage,
				RunnerImage:           schema.Status.ExecutionBinding.RunnerImage,
				RunnerProtocolVersion: schema.Status.ExecutionBinding.RunnerProtocolVersion,
			}
			wantApplied := schema.Status.Applied.DeepCopy()
			oldEpoch := ""
			if test.missingBinding {
				schema.Status.ExecutionBinding = nil
			} else {
				oldEpoch = schema.Status.ExecutionBinding.Epoch
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			reconciler.Jobs = executionBindingJobs{
				ptahVersion: "v0.4.0", executorImage: "example.invalid/ptah@" + safetyOtherDigest,
				runnerImage: "example.invalid/operator@" + safetyOtherDigest,
				protocol:    int32(runner.ProtocolVersion) + 1,
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("persist new execution binding: %v", err)
			}
			fenced := safetyGetSchema(t, api, schema)
			if fenced.Status.Plan != nil || fenced.Status.ActiveOperation != nil ||
				fenced.Status.ExecutionBinding == nil || fenced.Status.ExecutionBinding.Epoch == oldEpoch ||
				fenced.Status.Phase != operatorv1alpha1.PhasePending || fenced.Status.Source.Verified ||
				!reflect.DeepEqual(fenced.Status.Applied, wantApplied) {
				t.Fatalf("pre-plan execution-binding fence = %#v", fenced.Status)
			}
			newEpoch := fenced.Status.ExecutionBinding.Epoch

			restarted := *reconciler
			if _, err := restarted.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("restart full read-only chain: %v", err)
			}
			refreshing := safetyGetSchema(t, api, schema)
			if refreshing.Status.ExecutionBinding == nil || refreshing.Status.ExecutionBinding.Epoch != newEpoch ||
				refreshing.Status.ActiveOperation == nil || refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				refreshing.Status.ActiveOperation.ExecutionBindingID != newEpoch ||
				refreshing.Status.Phase != operatorv1alpha1.PhaseResolving {
				t.Fatalf("restart did not preserve the epoch and restart at Resolve: %#v", refreshing.Status)
			}
		})
	}
}

func TestExecutionBindingChangeRejectsDispatchedReadOnlyJobResult(t *testing.T) {
	t.Parallel()

	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
	} {
		operation := operation
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			schema.Finalizers = []string{activeOperationFinalizer}
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				RequestedReference:       schema.Spec.Desired.OCIRef,
				ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
				Digest:                   testDigest,
				ArtifactType:             dataplane.SchemaArtifactType,
				Verified:                 operation != operatorv1alpha1.OperationVerify,
				VerificationPolicyUID:    testPolicyUID,
				VerificationPolicyDigest: testDigest,
			}
			schema.Status.Target = operatorv1alpha1.TargetStatus{
				Engine: schema.Spec.Target.Engine, CoordinationDigest: testCoordinationDigest,
				IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
			}
			schema.Status.Phase = phaseFor(operation)
			schema.Status.Plan = nil
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operation, ID: "retired-read-only-" + strings.ToLower(string(operation)),
				JobName: "retired-read-only-job-" + strings.ToLower(string(operation)), JobUID: "job-uid",
				StartedAt: metav1.Now(), Attempt: 1,
			}
			bindActiveInput(t, schema)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
			retiredOperation := schema.Status.ActiveOperation.DeepCopy()
			oldEpoch := schema.Status.ExecutionBinding.Epoch
			logs := &safetyCountingLogs{content: []byte("result must not be read")}
			reconciler, api := fakeReconciler(t, logs, schema, job, pod)
			reconciler.Jobs = executionBindingJobs{
				ptahVersion: "v0.4.0", executorImage: "example.invalid/ptah@" + safetyOtherDigest,
				runnerImage: "example.invalid/operator@" + safetyOtherDigest,
				protocol:    int32(runner.ProtocolVersion) + 1,
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("retire dispatched read-only operation: %v", err)
			}
			fenced := safetyGetSchema(t, api, schema)
			if fenced.Status.ActiveOperation == nil ||
				fenced.Status.ActiveOperation.ID != retiredOperation.ID ||
				fenced.Status.ActiveOperation.JobName != retiredOperation.JobName ||
				fenced.Status.ActiveOperation.JobUID != retiredOperation.JobUID ||
				fenced.Status.ActiveOperation.ExecutionBindingID != oldEpoch ||
				fenced.Status.ExecutionBinding == nil || fenced.Status.ExecutionBinding.Epoch == oldEpoch ||
				fenced.Status.Source.Verified || fenced.Status.Plan != nil {
				t.Fatalf("first fence did not retain retired Job identity: %#v", fenced.Status)
			}
			newEpoch := fenced.Status.ExecutionBinding.Epoch
			persistedJob := &batchv1.Job{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
				t.Fatal(err)
			}
			if persistedJob.Spec.TTLSecondsAfterFinished != nil {
				t.Fatalf("first fence touched retired Job: %#v", persistedJob.Spec.TTLSecondsAfterFinished)
			}
			if logs.reads != 0 {
				t.Fatalf("first fence read retired logs %d times", logs.reads)
			}

			restarted := *reconciler
			if _, err := restarted.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("schedule retired Job cleanup: %v", err)
			}
			cleaned := safetyGetSchema(t, api, schema)
			if cleaned.Status.ActiveOperation != nil || cleaned.Status.ExecutionBinding == nil ||
				cleaned.Status.ExecutionBinding.Epoch != newEpoch {
				t.Fatalf("retired Job cleanup boundary = %#v", cleaned.Status)
			}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
				t.Fatal(err)
			}
			if persistedJob.Spec.TTLSecondsAfterFinished == nil ||
				*persistedJob.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
				t.Fatalf("retired Job TTL = %#v, want %d", persistedJob.Spec.TTLSecondsAfterFinished, jobCleanupTTLSeconds)
			}
			if logs.reads != 0 {
				t.Fatalf("cleanup read retired logs %d times", logs.reads)
			}

			if _, err := restarted.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("restart full chain after discarded result: %v", err)
			}
			refreshing := safetyGetSchema(t, api, schema)
			if refreshing.Status.ActiveOperation == nil {
				// A dispatched Plan can leave a separately durable Lease-release
				// marker. Draining that marker is another restart-safe boundary.
				if _, err := restarted.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("continue after retired Plan lock release: %v", err)
				}
				refreshing = safetyGetSchema(t, api, schema)
			}
			if refreshing.Status.ActiveOperation == nil || refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				refreshing.Status.ActiveOperation.ExecutionBindingID != newEpoch {
				t.Fatalf("discarded read-only result did not force Resolve: %#v", refreshing.Status)
			}
			if logs.reads != 0 {
				t.Fatalf("refresh claim read retired logs %d times", logs.reads)
			}
		})
	}
}

func TestRetiredReadOnlyJobCleanupSurvivesStatusPatchCrash(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "retired-resolve",
		JobName: "retired-resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
	logs := &safetyCountingLogs{content: []byte("result must not be read")}
	reconciler, api := fakeReconciler(t, logs, schema, job, pod)
	reconciler.Jobs = executionBindingJobs{
		ptahVersion: "v0.4.0", executorImage: "example.invalid/ptah@" + safetyOtherDigest,
		runnerImage: "example.invalid/operator@" + safetyOtherDigest,
		protocol:    int32(runner.ProtocolVersion) + 1,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("persist execution-binding fence: %v", err)
	}
	fenced := safetyGetSchema(t, api, schema)
	fencedEpoch := fenced.Status.ExecutionBinding.Epoch
	if fenced.Status.ActiveOperation == nil || fenced.Status.ActiveOperation.ID != "retired-resolve" {
		t.Fatalf("first fence lost retired operation: %#v", fenced.Status)
	}

	failTTL := &failFirstJobCleanupPatchClient{Client: api}
	reconciler.Client = failTTL
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "injected Job cleanup patch failure") {
		t.Fatalf("TTL interruption error = %v, want injected cleanup failure", err)
	}
	interruptedTTL := safetyGetSchema(t, api, schema)
	if interruptedTTL.Status.ActiveOperation == nil || interruptedTTL.Status.ActiveOperation.ID != "retired-resolve" ||
		interruptedTTL.Status.ExecutionBinding == nil || interruptedTTL.Status.ExecutionBinding.Epoch != fencedEpoch {
		t.Fatalf("TTL interruption lost durable identity: %#v", interruptedTTL.Status)
	}
	persistedJob := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
		t.Fatal(err)
	}
	if persistedJob.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("failed TTL patch modified Job: %#v", persistedJob.Spec.TTLSecondsAfterFinished)
	}

	failing := &failNthSchemaStatusPatchClient{Client: api, failAt: 1}
	reconciler.Client = failing
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "injected PtahSchema status patch failure") {
		t.Fatalf("cleanup interruption error = %v, want injected status failure", err)
	}
	interrupted := safetyGetSchema(t, api, schema)
	if interrupted.Status.ActiveOperation == nil || interrupted.Status.ActiveOperation.ID != "retired-resolve" ||
		interrupted.Status.ExecutionBinding == nil || interrupted.Status.ExecutionBinding.Epoch != fencedEpoch {
		t.Fatalf("cleanup interruption lost durable identity: %#v", interrupted.Status)
	}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
		t.Fatal(err)
	}
	if persistedJob.Spec.TTLSecondsAfterFinished == nil ||
		*persistedJob.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatalf("cleanup interruption did not persist Job TTL: %#v", persistedJob.Spec.TTLSecondsAfterFinished)
	}

	restarted := *reconciler
	restarted.Client = api
	restarted.APIReader = api
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("restart retired Job cleanup: %v", err)
	}
	cleaned := safetyGetSchema(t, api, schema)
	if cleaned.Status.ActiveOperation != nil || cleaned.Status.ExecutionBinding == nil ||
		cleaned.Status.ExecutionBinding.Epoch != fencedEpoch {
		t.Fatalf("restart generated another epoch or retained cleanup: %#v", cleaned.Status)
	}
	if logs.reads != 0 {
		t.Fatalf("retired cleanup read logs %d times", logs.reads)
	}
}

func TestRetiredReadOnlyJobCleanupDoesNotTouchUnprovenJob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*batchv1.Job)
	}{
		{
			name: "same-name replacement",
			mutate: func(job *batchv1.Job) {
				job.UID = "replacement-job-uid"
			},
		},
		{
			name: "schema owner mismatch",
			mutate: func(job *batchv1.Job) {
				job.OwnerReferences[0].UID = "other-schema-uid"
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			schema.Finalizers = []string{activeOperationFinalizer}
			schema.Status.Phase = operatorv1alpha1.PhaseResolving
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationResolve, ID: "retired-resolve",
				JobName: "retired-resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
			}
			bindActiveInput(t, schema)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
			test.mutate(job)
			logs := &safetyCountingLogs{content: []byte("result must not be read")}
			reconciler, api := fakeReconciler(t, logs, schema, job, pod)
			reconciler.Jobs = executionBindingJobs{
				ptahVersion: "v0.4.0", executorImage: "example.invalid/ptah@" + safetyOtherDigest,
				runnerImage: "example.invalid/operator@" + safetyOtherDigest,
				protocol:    int32(runner.ProtocolVersion) + 1,
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("persist execution-binding fence: %v", err)
			}
			fenced := safetyGetSchema(t, api, schema)
			fencedEpoch := fenced.Status.ExecutionBinding.Epoch
			if fenced.Status.ActiveOperation == nil {
				t.Fatalf("first fence lost retired operation: %#v", fenced.Status)
			}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("discard unproven retired Job identity: %v", err)
			}
			cleaned := safetyGetSchema(t, api, schema)
			if cleaned.Status.ActiveOperation != nil || cleaned.Status.ExecutionBinding == nil ||
				cleaned.Status.ExecutionBinding.Epoch != fencedEpoch {
				t.Fatalf("unproven cleanup boundary = %#v", cleaned.Status)
			}
			persistedJob := &batchv1.Job{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
				t.Fatal(err)
			}
			if persistedJob.Spec.TTLSecondsAfterFinished != nil {
				t.Fatalf("unproven Job was modified: %#v", persistedJob.Spec.TTLSecondsAfterFinished)
			}
			if logs.reads != 0 {
				t.Fatalf("unproven cleanup read logs %d times", logs.reads)
			}
		})
	}
}

func TestRetiredReadOnlyJobCleanupDoesNotPatchConcurrentReplacement(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "retired-resolve",
		JobName: "retired-resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
	logs := &safetyCountingLogs{content: []byte("result must not be read")}
	reconciler, api := fakeReconciler(t, logs, schema, job, pod)
	reconciler.Jobs = executionBindingJobs{
		ptahVersion: "v0.4.0", executorImage: "example.invalid/ptah@" + safetyOtherDigest,
		runnerImage: "example.invalid/operator@" + safetyOtherDigest,
		protocol:    int32(runner.ProtocolVersion) + 1,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("persist execution-binding fence: %v", err)
	}
	fenced := safetyGetSchema(t, api, schema)
	fencedEpoch := fenced.Status.ExecutionBinding.Epoch
	if fenced.Status.ActiveOperation == nil {
		t.Fatalf("first fence lost retired operation: %#v", fenced.Status)
	}

	replacing := &replaceJobBeforeCleanupPatchClient{
		Client: api, replacementUID: "concurrent-replacement-job-uid",
	}
	reconciler.Client = replacing
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("concurrent replacement cleanup error = %v, want conflict", err)
	}
	interrupted := safetyGetSchema(t, api, schema)
	if interrupted.Status.ActiveOperation == nil || interrupted.Status.ActiveOperation.ID != "retired-resolve" ||
		interrupted.Status.ExecutionBinding == nil || interrupted.Status.ExecutionBinding.Epoch != fencedEpoch {
		t.Fatalf("replacement race lost durable cleanup identity: %#v", interrupted.Status)
	}
	replacement := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.UID != replacing.replacementUID || replacement.Spec.TTLSecondsAfterFinished != nil ||
		len(replacement.OwnerReferences) != 0 {
		t.Fatalf("concurrent replacement was modified: %#v", replacement.ObjectMeta)
	}
	if logs.reads != 0 {
		t.Fatalf("replacement race read retired logs %d times", logs.reads)
	}

	restarted := *reconciler
	restarted.Client = api
	restarted.APIReader = api
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("discard replaced retired Job identity: %v", err)
	}
	cleaned := safetyGetSchema(t, api, schema)
	if cleaned.Status.ActiveOperation != nil || cleaned.Status.ExecutionBinding == nil ||
		cleaned.Status.ExecutionBinding.Epoch != fencedEpoch {
		t.Fatalf("replacement cleanup boundary = %#v", cleaned.Status)
	}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("replacement acquired cleanup TTL: %#v", replacement.Spec.TTLSecondsAfterFinished)
	}
}

func TestExecutionBindingChangeFencesLateApprovalAcrossRestart(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	plan.Status.ObservedGeneration = plan.Generation
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:               operatorv1alpha1.ConditionPlanStorageReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Published",
		ObservedGeneration: plan.Generation,
		LastTransitionTime: metav1.Now(),
	})
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
	reconciler.Jobs = executionBindingJobs{
		ptahVersion:   "v0.3.0",
		executorImage: "example.invalid/ptah@" + testDigest,
		runnerImage:   "example.invalid/operator@" + safetyOtherDigest,
		protocol:      int32(runner.ProtocolVersion),
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	retiredPlan := schema.Status.Plan.DeepCopy()

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("persist execution-binding fence: %v", err)
	}
	fenced := safetyGetSchema(t, api, schema)
	if fenced.Status.Plan == nil || fenced.Status.Plan.UID != retiredPlan.UID ||
		fenced.Status.Phase != operatorv1alpha1.PhasePending || fenced.Status.ActiveOperation != nil {
		t.Fatalf("durable fence lost retired plan identity: %#v", fenced.Status)
	}
	approvalRequired := findCondition(fenced.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if approvalRequired == nil || approvalRequired.Status != metav1.ConditionFalse || approvalRequired.Reason != "ExecutionBindingChanged" {
		t.Fatalf("durable approval fence = %#v", approvalRequired)
	}

	postFenceCandidate := approval.DeepCopy()
	postFenceCandidate.Name = "post-fence-approval"
	postFenceCandidate.UID = ""
	raw, err := json.Marshal(postFenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	handler := &approvaladmission.ApprovalHandler{
		Reader: api, Decoder: cradmission.NewDecoder(reconciler.Scheme), Mutate: false,
		ControllerImage:    testControllerImage,
		ControllerRevision: testControllerRevision, ControllerStateVersion: testControllerStateVersion,
	}
	response := handler.Handle(context.Background(), cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "post-fence-admission",
		Namespace: postFenceCandidate.Namespace,
		Name:      postFenceCandidate.Name,
		Operation: admissionv1.Create,
		UserInfo:  authenticationv1.UserInfo{Username: approval.Spec.Approver.Username},
		Object:    runtime.RawExtension{Raw: raw},
	}})
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "no longer current") {
		t.Fatalf("direct-read admission response after durable fence = %#v", response)
	}

	// Model a CREATE that passed admission against the pre-fence resource and
	// committed only after the fence patch. The retained plan identity lets the
	// restarted controller identify and retire this exact late object.
	lateApproval := approval.DeepCopy()
	lateApproval.Name = "late-pre-fence-approval"
	lateApproval.UID = "late-pre-fence-approval-uid"
	if err := api.Create(context.Background(), lateApproval); err != nil {
		t.Fatalf("commit pre-fence approval after fence: %v", err)
	}

	failing := &failNthSchemaStatusPatchClient{Client: api, failAt: 1}
	reconciler.Client = failing
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "injected PtahSchema status patch failure") {
		t.Fatalf("cleanup interruption error = %v, want injected status failure", err)
	}
	interrupted := safetyGetSchema(t, api, schema)
	if interrupted.Status.Plan == nil || interrupted.Status.Plan.UID != retiredPlan.UID ||
		!executionBindingChangeFenced(interrupted) {
		t.Fatalf("cleanup interruption lost durable fence: %#v", interrupted.Status)
	}
	persistedLate := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(lateApproval), persistedLate); err != nil {
		t.Fatal(err)
	}
	stale := findCondition(persistedLate.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
	if stale == nil || stale.Status != metav1.ConditionTrue || stale.Reason != "ExecutionBindingChanged" {
		t.Fatalf("late approval was not retired before plan removal: %#v", stale)
	}

	restarted := *reconciler
	restarted.Client = api
	restarted.APIReader = api
	// Even if the process configuration rolls back to the retired binding, the
	// persisted fence must finish. Reopening the old approval boundary would
	// make the cleanup dependent on process-local rollout timing.
	restarted.Jobs = fakeJobs{}
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("restart execution-binding cleanup: %v", err)
	}
	cleaned := safetyGetSchema(t, api, schema)
	if cleaned.Status.Plan != nil || cleaned.Status.ActiveOperation != nil ||
		cleaned.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("restart did not finish idempotent cleanup: %#v", cleaned.Status)
	}
	rolloutEpoch := cleaned.Status.ExecutionBinding.Epoch
	escapedApproval := approval.DeepCopy()
	escapedApproval.Name = "post-plan-cleanup-approval"
	escapedApproval.UID = "post-plan-cleanup-approval-uid"
	if err := api.Create(context.Background(), escapedApproval); err != nil {
		t.Fatalf("commit approval after retired-plan list: %v", err)
	}
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("persist rollback execution epoch: %v", err)
	}
	rollback := safetyGetSchema(t, api, schema)
	if rollback.Status.ActiveOperation != nil || rollback.Status.ExecutionBinding == nil ||
		rollback.Status.ExecutionBinding.Epoch == rolloutEpoch || rollback.Status.ExecutionBinding.Epoch == retiredPlan.ExecutionBindingID ||
		rollback.Status.ExecutionBinding.RunnerImage != "example.invalid/operator@"+testDigest {
		t.Fatalf("rollback did not create a distinct durable epoch: %#v", rollback.Status)
	}
	if escapedApproval.Spec.ExecutionBindingID == rollback.Status.ExecutionBinding.Epoch {
		t.Fatal("approval admitted under the retired epoch matched the rollback epoch")
	}
	rollbackPlan := retiredPlan.DeepCopy()
	rollbackPlan.ExecutionBindingID = rollback.Status.ExecutionBinding.Epoch
	if approvalMatchesPlanStatus(escapedApproval, rollback, rollbackPlan) {
		t.Fatal("late approval became valid after byte-identical component rollback")
	}
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("start full read-only refresh after late approval: %v", err)
	}
	refreshing := safetyGetSchema(t, api, schema)
	if refreshing.Status.ActiveOperation == nil || refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("restart skipped full Resolve chain: %#v", refreshing.Status)
	}
	persistedEscaped := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(escapedApproval), persistedEscaped); err != nil {
		t.Fatal(err)
	}
	escapedStale := findCondition(persistedEscaped.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
	if escapedStale != nil && escapedStale.Status == metav1.ConditionTrue {
		t.Fatalf("approval committed after the final cleanup LIST was used as safety evidence: %#v", escapedStale)
	}
}

func TestExecutionBindingChangeTakesPrecedenceOverPolicyChange(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	future := metav1.NewTime(time.Date(2026, 8, 30, 12, 10, 0, 0, time.UTC))
	schema.Status.NextReconciliationTime = &future
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reserve approval: %v", err)
	}

	if err := api.Delete(context.Background(), policyConfig); err != nil {
		t.Fatalf("delete old policy: %v", err)
	}
	replacement := policyConfig.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "verification-policy-v2-uid"
	replacement.Data = map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "updated policy"}
	if err := api.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create replacement policy: %v", err)
	}
	reconciler.Jobs = executionBindingJobs{
		ptahVersion:   "v0.3.0",
		executorImage: "example.invalid/ptah@" + testDigest,
		runnerImage:   "example.invalid/operator@" + safetyOtherDigest,
		protocol:      int32(runner.ProtocolVersion),
	}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() after simultaneous policy and binding change: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("binding invalidation result = %#v, want immediate refresh", result)
	}
	actual := safetyGetSchema(t, api, schema)
	verified := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil || actual.Status.Plan.UID != plan.UID || actual.Status.Source.Verified ||
		actual.Status.Phase != operatorv1alpha1.PhasePending || verified == nil ||
		verified.Status != metav1.ConditionUnknown || verified.Reason != "ExecutionBindingChanged" {
		t.Fatalf("combined invalidation fence = %#v", actual.Status)
	}
	approvalRequired := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if approvalRequired == nil || approvalRequired.Status != metav1.ConditionFalse || approvalRequired.Reason != "ExecutionBindingChanged" {
		t.Fatalf("combined invalidation approval fence = %#v", approvalRequired)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retire fenced approvals: %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.Plan != nil || actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("combined invalidation cleanup = %#v", actual.Status)
	}
	persistedApproval := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persistedApproval); err != nil {
		t.Fatal(err)
	}
	stale := findCondition(persistedApproval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
	if stale == nil || stale.Status != metav1.ConditionTrue || stale.Reason != "ExecutionBindingChanged" {
		t.Fatalf("combined invalidation stale approval = %#v", stale)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("start full refresh: %v", err)
	}
	refreshing := safetyGetSchema(t, api, schema)
	if refreshing.Status.ActiveOperation == nil || refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
		refreshing.Status.Phase != operatorv1alpha1.PhaseResolving {
		t.Fatalf("combined invalidation skipped Resolve: %#v", refreshing.Status)
	}
}

func TestExecutionBindingChangeAfterApplyClaimReleasesAuthorizationBeforeDispatch(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	future := metav1.NewTime(time.Date(2026, 8, 30, 12, 10, 0, 0, time.UTC))
	schema.Status.NextReconciliationTime = &future
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reserve approval: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim Apply: %v", err)
	}
	claimed := safetyGetSchema(t, api, schema)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationApply ||
		claimed.Status.ActiveOperation.DispatchStarted || claimed.Status.ActiveOperation.JobUID != "" {
		t.Fatalf("Apply claim boundary = %#v", claimed.Status.ActiveOperation)
	}

	reconciler.Jobs = executionBindingJobs{
		ptahVersion:   "v0.3.0",
		executorImage: "example.invalid/ptah@" + testDigest,
		runnerImage:   "example.invalid/operator@" + safetyOtherDigest,
		protocol:      int32(runner.ProtocolVersion),
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("invalidate claimed Apply: %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil || actual.Status.PendingLockRelease == nil ||
		actual.Status.Phase != operatorv1alpha1.PhasePending || !contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatalf("claimed Apply binding fence = %#v, finalizers %v", actual.Status, actual.Finalizers)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("binding change dispatched %d Jobs from the old approval", len(jobs.Items))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("release retired Apply lock: %v", err)
	}
	released := safetyGetSchema(t, api, schema)
	if released.Status.PendingLockRelease != nil || released.Status.Plan == nil {
		t.Fatalf("lock release crossed plan cleanup boundary: %#v", released.Status)
	}
	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		t.Fatalf("old Apply authorization retained Lease holder %q", *lease.Spec.HolderIdentity)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retire fenced approval and plan: %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.Plan != nil || actual.Status.PendingLockRelease != nil || contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatalf("claimed Apply cleanup = %#v, finalizers %v", actual.Status, actual.Finalizers)
	}
}

func TestExecutionBindingChangeInvalidatesClaimDespiteTargetLockContention(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	future := metav1.NewTime(time.Date(2026, 8, 30, 12, 10, 0, 0, time.UTC))
	schema.Status.NextReconciliationTime = &future
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reserve approval: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim Apply: %v", err)
	}
	claimed := safetyGetSchema(t, api, schema)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.LeaseEpoch == "" {
		t.Fatalf("Apply claim = %#v", claimed.Status.ActiveOperation)
	}
	contender := targetlock.Request{
		CoordinationNamespace: reconciler.LockNamespace,
		CoordinationDigest:    testCoordinationDigest,
		Holder: targetlock.Holder{
			SchemaUID: "other-schema-uid", OperationID: "other-apply",
		},
		Duration: 16 * time.Minute,
	}
	acquired, err := reconciler.Locks.Acquire(context.Background(), contender)
	if err != nil || !acquired.Acquired {
		t.Fatalf("acquire contending holder = %#v, %v", acquired, err)
	}
	wantHolder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest)

	reconciler.Jobs = executionBindingJobs{
		ptahVersion:   "v0.3.0",
		executorImage: "example.invalid/ptah@" + testDigest,
		runnerImage:   "example.invalid/operator@" + safetyOtherDigest,
		protocol:      int32(runner.ProtocolVersion),
	}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() under target-lock contention: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("binding invalidation under contention = %#v", result)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil || actual.Status.PendingLockRelease == nil ||
		actual.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("contended binding fence = %#v", actual.Status)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("release retired contended lock: %v", err)
	}
	if got := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); got != wantHolder {
		t.Fatalf("stale release changed contending holder from %q to %q", wantHolder, got)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retire contended approval and plan: %v", err)
	}
	actual = safetyGetSchema(t, api, schema)
	if actual.Status.Plan != nil || actual.Status.PendingLockRelease != nil || actual.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("contended binding cleanup = %#v", actual.Status)
	}
}

func TestExecutionBindingChangeAfterApplyDispatchNeverRecreatesMutation(t *testing.T) {
	for _, test := range []struct {
		name                string
		deleteJob           bool
		keepConfiguredTuple bool
		eraseOperationEpoch bool
	}{
		{name: "existing Job"},
		{name: "missing Job", deleteJob: true},
		{name: "legacy operation missing epoch", keepConfiguredTuple: true, eraseOperationEpoch: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			oldExecutionEpoch := schema.Status.ExecutionBinding.Epoch
			schema.Finalizers = []string{activeOperationFinalizer}
			schema.Status.Phase = operatorv1alpha1.PhaseApplying
			schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
				Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
			}
			started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
			executionNotAfter := metav1.NewTime(started.Add(leaseDuration(schema) - time.Minute))
			target := databaseTargetBinding(schema.Spec.Target)
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationApply, ID: "dispatched-old-binding-apply", JobName: "dispatched-old-binding-apply-job",
				StartedAt: metav1.NewTime(started), Attempt: 1, DispatchStarted: true,
				CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
				LeaseDurationSeconds: int32(leaseDuration(schema) / time.Second), LeaseEpoch: testLeaseEpoch,
				ExecutionNotAfter: &executionNotAfter, TerminationGracePeriodSeconds: int64(applyTerminationGrace / time.Second),
				Target: &target, Source: artifactAccessBinding(schema),
				ObservationExclude:  append([]string(nil), schema.Spec.Policy.Exclude...),
				ObservationSeverity: schema.Spec.Policy.DriftSeverity, ObservationDev: schema.Spec.Dev.DeepCopy(),
				ObservationConnectTimeout: schema.Spec.Execution.ConnectTimeout,
				ObservationLockTimeout:    schema.Spec.Policy.LockTimeout,
			}
			bindActiveInput(t, schema)
			ensureTestAdmissionSnapshot(schema)
			applyJob, err := (fakeJobs{}).Build(schema, *schema.Status.ActiveOperation, plan)
			if err != nil {
				t.Fatal(err)
			}
			applyJob.UID = "job-uid"
			schema.Status.ActiveOperation.JobUID = applyJob.UID
			if test.eraseOperationEpoch {
				schema.Status.ActiveOperation.ExecutionBindingID = ""
			}
			oldPlan := *schema.Status.Plan
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig, applyJob)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			if test.deleteJob {
				if err := api.Delete(context.Background(), applyJob); err != nil {
					t.Fatalf("delete dispatched Apply Job: %v", err)
				}
			}

			expectedRunnerImage := "example.invalid/operator@" + safetyOtherDigest
			if test.keepConfiguredTuple {
				reconciler.Jobs = fakeJobs{}
				expectedRunnerImage = "example.invalid/operator@" + testDigest
			} else {
				reconciler.Jobs = executionBindingJobs{
					ptahVersion:   "v0.3.0",
					executorImage: "example.invalid/ptah@" + testDigest,
					runnerImage:   expectedRunnerImage,
					protocol:      int32(runner.ProtocolVersion),
				}
			}
			result, err := reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("Reconcile() after dispatched binding change: %v", err)
			}
			if !result.Requeue {
				t.Fatalf("dispatched binding change result = %#v", result)
			}
			actual := safetyGetSchema(t, api, schema)
			pending := actual.Status.PendingObservation
			if actual.Status.ActiveOperation != nil || pending == nil ||
				pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown ||
				pending.PlanRequired || actual.Status.Phase != operatorv1alpha1.PhasePending ||
				actual.Status.ExecutionBinding == nil || actual.Status.ExecutionBinding.Epoch == oldExecutionEpoch ||
				actual.Status.ExecutionBinding.RunnerImage != expectedRunnerImage {
				t.Fatalf("dispatched binding transition = %#v", actual.Status)
			}
			rolloutEpoch := actual.Status.ExecutionBinding.Epoch
			if pending.Plan.Fingerprint != oldPlan.Fingerprint || pending.Plan.ExecutorImage != oldPlan.ExecutorImage ||
				pending.Plan.RunnerImage != oldPlan.RunnerImage || pending.Plan.RunnerProtocolVersion != oldPlan.RunnerProtocolVersion {
				t.Fatalf("pending proof lost immutable old execution evidence: %#v", pending.Plan)
			}
			condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "OutcomeUnknown" {
				t.Fatalf("Applying = %#v, want False/OutcomeUnknown", condition)
			}
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
				t.Fatal(err)
			}
			wantJobs := 1
			if test.deleteJob {
				wantJobs = 0
			}
			if len(jobs.Items) != wantJobs {
				t.Fatalf("Jobs after binding change = %d, want %d and no replacement Apply", len(jobs.Items), wantJobs)
			}
			persistedApproval := &operatorv1alpha1.PtahSchemaApproval{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persistedApproval); err != nil {
				t.Fatal(err)
			}
			consumed := findCondition(persistedApproval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed)
			if consumed == nil || consumed.Status != metav1.ConditionTrue || consumed.Reason != "DispatchCommitted" {
				t.Fatalf("dispatched approval history = %#v", consumed)
			}
			if test.keepConfiguredTuple {
				return
			}

			// Crash immediately after the atomic OutcomeUnknown/new-epoch patch and
			// roll the process configuration back byte-for-byte. The cleanup pass
			// must finish against the rollout epoch, then the rollback must claim a
			// third epoch instead of reopening the original authorization boundary.
			restarted := *reconciler
			restarted.Jobs = fakeJobs{}
			if _, err := restarted.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("finish dispatched-Apply rollout fence after restart: %v", err)
			}
			cleaned := safetyGetSchema(t, api, schema)
			if cleaned.Status.Plan != nil || cleaned.Status.ExecutionBinding == nil ||
				cleaned.Status.ExecutionBinding.Epoch != rolloutEpoch || cleaned.Status.PendingObservation == nil {
				t.Fatalf("restarted rollout cleanup = %#v", cleaned.Status)
			}
			if _, err := restarted.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("persist dispatched-Apply rollback epoch: %v", err)
			}
			rolledBack := safetyGetSchema(t, api, schema)
			if rolledBack.Status.ActiveOperation != nil || rolledBack.Status.PendingObservation == nil ||
				rolledBack.Status.ExecutionBinding == nil || rolledBack.Status.ExecutionBinding.Epoch == rolloutEpoch ||
				rolledBack.Status.ExecutionBinding.Epoch == oldExecutionEpoch ||
				rolledBack.Status.ExecutionBinding.RunnerImage != "example.invalid/operator@"+testDigest {
				t.Fatalf("dispatched-Apply rollback reused an execution epoch: %#v", rolledBack.Status)
			}
		})
	}
}

func TestExecutionBindingChangeDiscardsOldPostApplyProofResult(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	policyBytes := []byte("policy")
	policyDigest := fingerprint.DigestBytes(policyBytes)
	schema.Status.Source.VerificationPolicyDigest = policyDigest
	schema.Status.PendingObservation.Plan.VerificationPolicyDigest = policyDigest
	schema.Status.PendingObservation.PlanRequired = true
	oldBindingID := schema.Status.ExecutionBinding.Epoch
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:      operatorv1alpha1.OperationPlan,
		ID:        "post-apply-binding-plan",
		JobName:   "post-apply-binding-plan-job",
		JobUID:    "job-uid",
		StartedAt: metav1.Now(),
		Attempt:   1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		CoordinationDigest:   schema.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: schema.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	})
	immutable := true
	policyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
			UID:       "verification-policy-v2-uid",
		},
		Immutable: &immutable,
		Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "updated policy"},
	}
	logs := &safetyCountingLogs{content: frame}
	reconciler, api := fakeReconciler(t, logs, schema, job, pod, policyConfig)
	wantPending := schema.Status.PendingObservation.DeepCopy()
	wantPending.PlanRequired = false
	reconciler.Jobs = executionBindingJobs{
		ptahVersion:   "v0.3.0",
		executorImage: "example.invalid/ptah@" + testDigest,
		runnerImage:   "example.invalid/operator@" + safetyOtherDigest,
		protocol:      int32(runner.ProtocolVersion),
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retire old post-Apply proof: %v", err)
	}
	fenced := safetyGetSchema(t, api, schema)
	if !reflect.DeepEqual(fenced.Status.PendingObservation, wantPending) {
		got, _ := json.Marshal(fenced.Status.PendingObservation)
		want, _ := json.Marshal(wantPending)
		t.Fatalf("pending proof changed across first fence: got %s want %s", got, want)
	}
	if fenced.Status.ActiveOperation == nil || fenced.Status.ActiveOperation.ID != "post-apply-binding-plan" ||
		fenced.Status.ActiveOperation.ExecutionBindingID != oldBindingID ||
		fenced.Status.PendingLockRelease != nil || fenced.Status.Phase != operatorv1alpha1.PhasePending ||
		fenced.Status.NextReconciliationTime != nil ||
		fenced.Status.Source.Verified || fenced.Status.Source.VerifiedAt != nil {
		t.Fatalf("post-Apply proof first fence = %#v", fenced.Status)
	}
	if fenced.Status.ExecutionBinding == nil || fenced.Status.ExecutionBinding.Epoch == oldBindingID ||
		fenced.Status.ExecutionBinding.RunnerImage != "example.invalid/operator@"+safetyOtherDigest {
		t.Fatalf("new execution epoch = %#v, old ID %q", fenced.Status.ExecutionBinding, oldBindingID)
	}
	newEpoch := fenced.Status.ExecutionBinding.Epoch
	if fenced.Status.Applied != nil {
		t.Fatalf("old read-only result created Apply attribution: %#v", fenced.Status.Applied)
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionArtifactResolved,
		operatorv1alpha1.ConditionArtifactVerified,
		operatorv1alpha1.ConditionDatabaseReachable,
		operatorv1alpha1.ConditionDriftDetected,
		operatorv1alpha1.ConditionPlanReady,
		operatorv1alpha1.ConditionInSync,
		operatorv1alpha1.ConditionReady,
	} {
		condition := findCondition(fenced.Status.Conditions, conditionType)
		if condition == nil || condition.Reason != "ExecutionBindingChanged" {
			t.Fatalf("%s = %#v, want ExecutionBindingChanged", conditionType, condition)
		}
	}
	persistedJob := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
		t.Fatal(err)
	}
	if persistedJob.Spec.TTLSecondsAfterFinished != nil || logs.reads != 0 {
		t.Fatalf("first fence consumed retired proof: TTL=%#v log reads=%d", persistedJob.Spec.TTLSecondsAfterFinished, logs.reads)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range jobs.Items {
		if jobs.Items[i].Labels[workload.LabelOperation] == "apply" {
			t.Fatalf("post-Apply binding change created Apply Job %s", jobs.Items[i].Name)
		}
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("schedule retired post-Apply proof cleanup: %v", err)
	}
	cleaned := safetyGetSchema(t, api, schema)
	if cleaned.Status.ActiveOperation != nil || cleaned.Status.PendingLockRelease != nil ||
		!reflect.DeepEqual(cleaned.Status.PendingObservation, wantPending) ||
		cleaned.Status.ExecutionBinding == nil || cleaned.Status.ExecutionBinding.Epoch != newEpoch {
		t.Fatalf("post-Apply cleanup lost pending proof or Lease: %#v", cleaned.Status)
	}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persistedJob); err != nil {
		t.Fatal(err)
	}
	if persistedJob.Spec.TTLSecondsAfterFinished == nil ||
		*persistedJob.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds || logs.reads != 0 {
		t.Fatalf("retired proof cleanup: TTL=%#v log reads=%d", persistedJob.Spec.TTLSecondsAfterFinished, logs.reads)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("restart post-Apply proof: %v", err)
	}
	restarted := safetyGetSchema(t, api, schema)
	if restarted.Status.ActiveOperation == nil || restarted.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve ||
		restarted.Status.ActiveOperation.ExecutionBindingID != newEpoch {
		t.Fatalf("post-Apply proof did not restart with Observe under the new epoch: %#v", restarted.Status)
	}
	if _, err := reconciler.consumeResult(context.Background(), restarted, nil, runner.Result{
		Operation:            runner.OperationObserve,
		CoordinationDigest:   restarted.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: restarted.Status.PendingObservation.Plan.TargetIdentityDigest,
		DriftReportDigest:    testDigest,
		ObservedDialect:      "postgresql",
	}, nil, 0); err != nil {
		t.Fatalf("complete new-epoch post-Apply observation: %v", err)
	}
	observed := safetyGetSchema(t, api, schema)
	if observed.Status.ActiveOperation != nil || observed.Status.PendingObservation == nil ||
		!observed.Status.PendingObservation.PlanRequired {
		t.Fatalf("post-Apply observation did not advance to scoped proof: %#v", observed.Status)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim new-epoch post-Apply plan: %v", err)
	}
	planning := safetyGetSchema(t, api, schema)
	if planning.Status.ActiveOperation == nil || planning.Status.ActiveOperation.Type != operatorv1alpha1.OperationPlan ||
		planning.Status.ActiveOperation.ExecutionBindingID != newEpoch {
		t.Fatalf("post-Apply proof did not claim Plan under the new epoch: %#v", planning.Status)
	}
	if _, err := reconciler.consumeResult(context.Background(), planning, nil, runner.Result{
		Operation:            runner.OperationPlan,
		CoordinationDigest:   planning.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: planning.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	}, nil, 0); err != nil {
		t.Fatalf("complete new-epoch post-Apply plan: %v", err)
	}
	proved := safetyGetSchema(t, api, schema)
	if proved.Status.ActiveOperation != nil || proved.Status.PendingObservation != nil || proved.Status.Plan == nil ||
		proved.Status.Applied == nil || proved.Status.Applied.ExecutionBindingID != oldBindingID ||
		proved.Status.Phase != operatorv1alpha1.PhasePending || !executionBindingChangeFenced(proved) {
		t.Fatalf("post-Apply proof did not preserve attribution and schedule current refresh: %#v", proved.Status)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retire historical post-Apply plan after proof: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("start complete current-epoch refresh after proof: %v", err)
	}
	refreshing := safetyGetSchema(t, api, schema)
	if refreshing.Status.PendingObservation != nil || refreshing.Status.Plan != nil ||
		refreshing.Status.ActiveOperation == nil || refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
		refreshing.Status.ActiveOperation.ExecutionBindingID != newEpoch {
		t.Fatalf("post-Apply proof did not lead to a full current-epoch Resolve chain: %#v", refreshing.Status)
	}
}

func safetyApprovalFixture(t *testing.T) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, *operatorv1alpha1.PtahSchemaApproval, *corev1.ConfigMap) {
	t.Helper()

	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
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
		Engine:             schema.Spec.Target.Engine,
		CoordinationDigest: testCoordinationDigest,
		IdentityDigest:     testDigest,
	}
	policyBinding, err := policyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "approved-plan", UID: "approved-plan-uid", Generation: 1},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          fingerprint.CurrentPlanContractVersion,
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              "sha256:plan",
			ContentDigest:            testDigest,
			ArtifactDigest:           schema.Status.Source.Digest,
			CoordinationDigest:       schema.Status.Target.CoordinationDigest,
			TargetIdentityDigest:     schema.Status.Target.IdentityDigest,
			ActualStateFingerprint:   safetyOtherDigest,
			DesiredStateFingerprint:  "sha256:desired",
			PolicyFingerprint:        policyBinding,
			VerificationPolicyUID:    schema.Status.Source.VerificationPolicyUID,
			VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
			ExecutionBindingID:       schema.Status.ExecutionBinding.Epoch,
			ControllerImage:          schema.Status.ExecutionBinding.ControllerImage,
			ControllerRevision:       schema.Status.ExecutionBinding.ControllerRevision,
			ControllerStateVersion:   schema.Status.ExecutionBinding.ControllerStateVersion,
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
			CoordinationDigest:       plan.Spec.CoordinationDigest,
			TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
			ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
			DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
			PolicyFingerprint:        plan.Spec.PolicyFingerprint,
			VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
			VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
			ExecutionBindingID:       plan.Spec.ExecutionBindingID,
			ControllerImage:          plan.Spec.ControllerImage,
			ControllerRevision:       plan.Spec.ControllerRevision,
			ControllerStateVersion:   plan.Spec.ControllerStateVersion,
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
	return schema, plan, approval, policyConfig
}

func safetyReadyToApplyFixture(t *testing.T) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, *corev1.ConfigMap) {
	t.Helper()

	schema, plan, _, policyConfig := safetyApprovalFixture(t)
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	schema.Status.Phase = operatorv1alpha1.PhaseReadyToApply
	schema.Status.Plan.Approval = nil
	return schema, plan, policyConfig
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
		Fingerprint:           "plan-fingerprint",
		ContentDigest:         testDigest,
		ArtifactDigest:        testDigest,
		CoordinationDigest:    testCoordinationDigest,
		TargetIdentityDigest:  testDigest,
		ExecutionBindingID:    schema.Status.ExecutionBinding.Epoch,
		PtahVersion:           schema.Status.ExecutionBinding.PtahVersion,
		ExecutorImage:         schema.Status.ExecutionBinding.ExecutorImage,
		RunnerImage:           schema.Status.ExecutionBinding.RunnerImage,
		RunnerProtocolVersion: schema.Status.ExecutionBinding.RunnerProtocolVersion,
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
		ExecutionBindingID:   schema.Status.ExecutionBinding.Epoch,
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
			Name:                     "post-apply-plan",
			UID:                      "post-apply-plan-uid",
			Fingerprint:              "plan-fingerprint",
			ContentDigest:            testDigest,
			ArtifactDigest:           testDigest,
			CoordinationDigest:       testCoordinationDigest,
			TargetIdentityDigest:     testDigest,
			ActualStateFingerprint:   safetyOtherDigest,
			DesiredStateFingerprint:  testDigest,
			PolicyFingerprint:        safetyOtherDigest,
			VerificationPolicyUID:    testPolicyUID,
			VerificationPolicyDigest: testDigest,
			ExecutionBindingID:       schema.Status.ExecutionBinding.Epoch,
			ControllerImage:          schema.Status.ExecutionBinding.ControllerImage,
			ControllerRevision:       schema.Status.ExecutionBinding.ControllerRevision,
			ControllerStateVersion:   schema.Status.ExecutionBinding.ControllerStateVersion,
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

func safetyPlanDocument(t *testing.T, fromFingerprint string, destructive ...bool) []byte {
	t.Helper()
	isDestructive := len(destructive) > 0 && destructive[0]
	document, err := json.Marshal(map[string]any{
		"format_version":   1,
		"name":             "safety-plan",
		"dialect":          "postgresql",
		"from_fingerprint": fromFingerprint,
		"to_fingerprint":   "desired-state",
		"destructive":      isDestructive,
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
