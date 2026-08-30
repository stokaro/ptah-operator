package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeJobs struct{}

func (fakeJobs) NameFor(_ *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) (string, error) {
	return "ptah-" + strings.ToLower(string(operation.Type)) + "-test", nil
}

func (fakeJobs) Build(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus, _ *operatorv1alpha1.PtahSchemaPlan) (*batchv1.Job, error) {
	controller := true
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: operation.JobName,
		OwnerReferences: []metav1.OwnerReference{{UID: schema.UID, Controller: &controller}},
	}}, nil
}

func (fakeJobs) ExecutionBinding() (string, string, string, int32) {
	return "v0.3.0", "example.invalid/ptah@" + testDigest, "example.invalid/operator@" + testDigest, 1
}

type staticLogs struct{ content []byte }

func (l staticLogs) Read(context.Context, string, string, string) ([]byte, error) {
	return append([]byte(nil), l.content...), nil
}

func TestSetupRejectsInvalidLockNamespace(t *testing.T) {
	t.Parallel()

	reconciler := &SchemaReconciler{LockNamespace: ""}
	if err := reconciler.SetupWithManager(nil); err == nil {
		t.Fatal("SetupWithManager() accepted an empty target lock namespace")
	}
}

func TestPendingSchemaClaimsResolveBeforeCreatingJob(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatal("Reconcile() did not immediately continue a persisted claim")
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("active operation = %#v, want Resolve", actual.Status.ActiveOperation)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseResolving {
		t.Fatalf("phase = %s, want Resolving", actual.Status.Phase)
	}
	if !contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatal("operation claim lacks deletion-safe finalizer")
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("claim reconciliation created a Job before persisting status")
	}
}

func TestResolveResultAdvancesToVerification(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: testDigest, InputFingerprint: testDigest,
		JobName: "resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	report, _ := json.Marshal(map[string]any{
		"reference": schema.Spec.Desired.OCIRef, "pinned_reference": "oci://registry.example/team/schema@" + testDigest,
		"digest": testDigest, "media_type": "application/vnd.oci.image.manifest.v1+json", "size": 321,
	})
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: testDigest, ChildExitCode: 0, Stdout: string(report),
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseVerifying {
		t.Fatalf("status after resolve = %#v", actual.Status)
	}
	if actual.Status.Source.Digest != testDigest || actual.Status.Source.Verified {
		t.Fatalf("source status = %#v", actual.Status.Source)
	}
	if contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatal("completed operation retained the transient finalizer")
	}
}

func TestFailedApplyWithMissingFrameForcesReadOnlyObservation(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	schema.Status.Target.IdentityDigest = testDigest
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Fingerprint: testDigest, ContentDigest: testDigest}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: testDigest, InputFingerprint: testDigest,
		JobName: "apply-job", JobUID: "job-uid", StartedAt: metav1.NewTime(time.Now().Add(-time.Minute)), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobFailed)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
		t.Fatalf("uncertain apply status = %#v", actual.Status)
	}
	condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying)
	if condition == nil || condition.Reason != "OutcomeUnknown" {
		t.Fatalf("Applying condition = %#v", condition)
	}
}

func TestApplyInputsChangedAfterDispatchRemainOutcomeUnknown(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	schema.Status.Target.IdentityDigest = testDigest
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Fingerprint: testDigest, ContentDigest: testDigest}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: testDigest, InputFingerprint: testDigest,
		JobName: "apply-job", JobUID: "job-uid", StartedAt: metav1.NewTime(time.Now().Add(-time.Minute)), Attempt: 1,
	}
	bindActiveInput(t, schema)
	// A suspend request increments generation after the mutating Job already
	// exists. The stale claim must not be treated like an undispatched plan.
	schema.Generation++
	schema.Spec.Suspend = true
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
		t.Fatalf("changed-input apply status = %#v", actual.Status)
	}
	condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying)
	if condition == nil || condition.Reason != "OutcomeUnknown" {
		t.Fatalf("Applying condition = %#v", condition)
	}
}

func TestApplyLockCoordinatesTheSameTargetAcrossSchemaNamespaces(t *testing.T) {
	t.Parallel()

	first := schemaFixture()
	first.Namespace = "team-a"
	first.UID = "schema-a-uid"
	first.Status.Target.IdentityDigest = testDigest
	first.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "operation-a",
	}
	second := first.DeepCopy()
	second.Namespace = "team-b"
	second.Name = "second"
	second.UID = "schema-b-uid"
	second.Status.ActiveOperation.ID = "operation-b"

	reconciler, api := fakeReconciler(t, staticLogs{})
	reconciler.Locks = targetlock.New(api, api, nil)
	acquired, _, err := reconciler.acquireApplyLock(context.Background(), first)
	if err != nil {
		t.Fatalf("acquire first target lock: %v", err)
	}
	if !acquired {
		t.Fatal("first target lock was not acquired")
	}
	acquired, requeueAfter, err := reconciler.acquireApplyLock(context.Background(), second)
	if err != nil {
		t.Fatalf("acquire contending target lock: %v", err)
	}
	if acquired || requeueAfter <= 0 {
		t.Fatalf("cross-namespace target lock = acquired %t, requeue %s; want contention", acquired, requeueAfter)
	}

	leases := &coordinationv1.LeaseList{}
	if err := api.List(context.Background(), leases, client.InNamespace(reconciler.LockNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) != 1 {
		t.Fatalf("coordination namespace contains %d target Leases, want 1", len(leases.Items))
	}
	for _, namespace := range []string{first.Namespace, second.Namespace} {
		local := &coordinationv1.LeaseList{}
		if err := api.List(context.Background(), local, client.InNamespace(namespace)); err != nil {
			t.Fatal(err)
		}
		if len(local.Items) != 0 {
			t.Fatalf("schema namespace %q contains target Leases", namespace)
		}
	}
}

func TestPostApplyObserveClaimPreservesConvergencePhase(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		Digest:            testDigest, Verified: true, ArtifactType: "application/vnd.stokaro.ptah.schema.v1",
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence ||
		actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("post-apply observation claim = %#v", actual.Status)
	}
}

func TestAlwaysPolicyMakesSafePlanReadyWithoutClaimingApprovalIsRequired(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	plan := &operatorv1alpha1.PtahSchemaPlan{Spec: operatorv1alpha1.PtahSchemaPlanSpec{Destructive: false}}

	setPlanPolicyStatus(schema, plan)

	if schema.Status.Phase != operatorv1alpha1.PhaseReadyToApply {
		t.Fatalf("phase = %q, want ReadyToApply", schema.Status.Phase)
	}
	condition := findCondition(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "NotRequired" {
		t.Fatalf("ApprovalRequired condition = %#v", condition)
	}
}

func TestAlwaysPolicyStillRequiresApprovalForDestructivePlan(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	schema.Spec.Policy.AllowDestructive = true
	plan := &operatorv1alpha1.PtahSchemaPlan{Spec: operatorv1alpha1.PtahSchemaPlanSpec{Destructive: true}}

	setPlanPolicyStatus(schema, plan)

	if schema.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
		t.Fatalf("phase = %q, want AwaitingApproval", schema.Status.Phase)
	}
	condition := findCondition(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("ApprovalRequired condition = %#v", condition)
	}
}

func schemaFixture() *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid", Generation: 1},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:  operatorv1alpha1.DatabaseEnginePostgreSQL,
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef:                 "oci://registry.example/team/schema:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml"},
			},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{ObservedGeneration: 1},
	}
}

func terminalWorkload(schema *operatorv1alpha1.PtahSchema, conditionType batchv1.JobConditionType) (*batchv1.Job, *corev1.Pod) {
	controller := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Status.ActiveOperation.JobName, UID: "job-uid", OwnerReferences: []metav1.OwnerReference{{UID: schema.UID, Controller: &controller}}},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: job.Name + "-pod", UID: "pod-uid",
		Labels: map[string]string{"job-name": job.Name}, OwnerReferences: []metav1.OwnerReference{{UID: job.UID, Controller: &controller}},
	}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}}}
	return job, pod
}

func fakeReconciler(t *testing.T, logs PodLogReader, objects ...client.Object) (*SchemaReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{operatorv1alpha1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahSchemaPlan{}, &operatorv1alpha1.PtahSchemaApproval{}, &batchv1.Job{}).
		WithIndex(&operatorv1alpha1.PtahSchemaApproval{}, approvalSchemaIndex, func(object client.Object) []string {
			approval := object.(*operatorv1alpha1.PtahSchemaApproval)
			return []string{approval.Spec.SchemaRef.Name}
		}).
		WithObjects(objects...).Build()
	clock := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	reconciler := &SchemaReconciler{
		Client: api, APIReader: api, Scheme: scheme, Logs: logs, Jobs: fakeJobs{},
		LockNamespace: "ptah-system",
		Clock:         func() time.Time { return clock },
	}
	return reconciler, api
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func bindActiveInput(t *testing.T, schema *operatorv1alpha1.PtahSchema) {
	t.Helper()
	inputs, err := operationInputs(schema, schema.Status.ActiveOperation.Type)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fingerprint.DigestCanonicalJSON(inputs)
	if err != nil {
		t.Fatal(err)
	}
	schema.Status.ActiveOperation.InputFingerprint = digest
}
