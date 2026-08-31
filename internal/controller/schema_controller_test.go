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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

const (
	testDigest          = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCoordinationKey = "team-a/app-database"
	testLeaseEpoch      = "v1-11111111111111111111111111111111"
	testLeaseEpochOther = "v1-22222222222222222222222222222222"
	testPolicyUID       = types.UID("verification-policy-v1-uid")
)

var testCoordinationDigest = mustTestCoordinationDigest()

type fakeJobs struct{}

func (fakeJobs) NameFor(_ *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) (string, error) {
	return "ptah-" + strings.ToLower(string(operation.Type)) + "-test", nil
}

func (fakeJobs) Build(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus, _ *operatorv1alpha1.PtahSchemaPlan) (*batchv1.Job, error) {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: operation.JobName,
		OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)},
	}}, nil
}

func (fakeJobs) ExecutionBinding() (string, string, string, int32) {
	return "v0.3.0", "example.invalid/ptah@" + testDigest, "example.invalid/operator@" + testDigest, int32(runner.ProtocolVersion)
}

type staticLogs struct{ content []byte }

func (l staticLogs) Read(context.Context, string, string, string) ([]byte, error) {
	return append([]byte(nil), l.content...), nil
}

type failFirstJobCleanupPatchClient struct {
	client.Client
	failed bool
}

func (c *failFirstJobCleanupPatchClient) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	if job, ok := object.(*batchv1.Job); ok && job.Spec.TTLSecondsAfterFinished != nil && !c.failed {
		c.failed = true
		return errors.New("injected Job cleanup patch failure")
	}
	return c.Client.Patch(ctx, object, patch, options...)
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

func TestInvalidExcludeSelectorFailsBeforeCreatingJob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		excludes []string
	}{
		{name: "whitespace", excludes: []string{" audit.*"}},
		{name: "overlong", excludes: []string{strings.Repeat("a", 257)}},
		{name: "duplicate", excludes: []string{"audit.*", "audit.*"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Policy.Exclude = test.excludes
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
				t.Fatal(err)
			}
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed || actual.Status.ActiveOperation != nil {
				t.Fatalf("invalid policy status = %#v", actual.Status)
			}
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 {
				t.Fatalf("invalid policy created %d Jobs", len(jobs.Items))
			}
		})
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
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: testDigest, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
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

func TestResolveResultAcceptsCanonicalDefaultTag(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Spec.Desired.OCIRef = "oci://registry.example/team/schema"
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: testDigest, InputFingerprint: testDigest,
		JobName: "resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion:   runner.ProtocolVersion,
		Operation:         runner.OperationResolve,
		OperationID:       testDigest,
		ChildExitCode:     0,
		ResolvedDigest:    testDigest,
		ResolvedReference: schema.Spec.Desired.OCIRef + "@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json",
		ResolvedSize:      321,
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
	if actual.Status.Phase != operatorv1alpha1.PhaseVerifying || actual.Status.Source.Digest != testDigest ||
		actual.Status.Source.RequestedReference != schema.Spec.Desired.OCIRef {
		t.Fatalf("status after selector-less resolve = %#v", actual.Status)
	}
}

func TestVerificationPolicyRefusalBlocksWithoutHotRetry(t *testing.T) {
	t.Parallel()

	policyBytes := []byte("require_digest_pin: true\nrequire_signature: true\n")
	policyDigest := fingerprint.DigestBytes(policyBytes)
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseVerifying
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		RequestedReference: schema.Spec.Desired.OCIRef,
		ResolvedReference:  "oci://registry.example/team/schema@" + testDigest,
		Digest:             testDigest,
	}
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Fingerprint: testDigest}
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionTrue, "ObservedConverged", "previously converged")
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationVerify, ID: testDigest,
		JobName: "verify-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
		VerificationPolicyUID: testPolicyUID, VerificationPolicyDigest: policyDigest,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion:          runner.ProtocolVersion,
		Operation:                runner.OperationVerify,
		OperationID:              testDigest,
		ChildExitCode:            2,
		ResolvedDigest:           testDigest,
		VerificationRequirements: []string{"require_signature"},
		VerificationPolicyDigest: policyDigest,
		Error:                    &runner.ResultError{Code: "verification_refused", Message: "artifact does not satisfy the verification policy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	policyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: string(policyBytes)},
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfigMap)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Minute {
		t.Fatalf("requeue = %s, want ordinary 10m interval", result.RequeueAfter)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	verified := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	inSync := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync)
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseBlocked ||
		actual.Status.Source.Verified || actual.Status.Plan != nil || actual.Status.NextReconciliationTime == nil ||
		verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != "PolicyRefused" ||
		inSync == nil || inSync.Status != metav1.ConditionFalse || inSync.Reason != "PolicyRefused" ||
		!strings.Contains(verified.Message, "require_signature") {
		t.Fatalf("status after verification refusal = %#v", actual.Status)
	}
	if contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatal("policy refusal retained the transient finalizer")
	}
	harvested := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Spec.TTLSecondsAfterFinished == nil || *harvested.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatalf("Job cleanup TTL = %#v", harvested.Spec.TTLSecondsAfterFinished)
	}

	result, err = reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("blocked Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("blocked refusal hot-looped: %#v", result)
	}
	unchanged := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Status.ActiveOperation != nil {
		t.Fatalf("blocked refusal claimed work before interval: %#v", unchanged.Status.ActiveOperation)
	}
}

func TestVerificationResultCannotOutrunPolicyConfigMapUpdate(t *testing.T) {
	t.Parallel()

	verifiedPolicy := []byte("require_digest_pin: true\n")
	currentPolicy := []byte("require_digest_pin: true\nrequire_signature: true\n")
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseVerifying
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		RequestedReference: schema.Spec.Desired.OCIRef,
		ResolvedReference:  "oci://registry.example/team/schema@" + testDigest,
		Digest:             testDigest,
	}
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Fingerprint: testDigest}
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionTrue, "ObservedConverged", "previously converged")
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationVerify, ID: testDigest,
		JobName: "verify-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
		VerificationPolicyUID: testPolicyUID, VerificationPolicyDigest: fingerprint.DigestBytes(verifiedPolicy),
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion:          runner.ProtocolVersion,
		Operation:                runner.OperationVerify,
		OperationID:              testDigest,
		ChildExitCode:            0,
		ResolvedDigest:           testDigest,
		ObservedArtifactType:     dataplane.SchemaArtifactType,
		VerificationPolicyDigest: fingerprint.DigestBytes(verifiedPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}
	policyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: "verification-policy-v2-uid"},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: string(currentPolicy)},
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfigMap)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatal("changed verification policy did not trigger a fresh verification")
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	inSync := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync)
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseVerifying ||
		actual.Status.Source.Verified || actual.Status.Plan != nil || condition == nil || condition.Reason != "PolicyChanged" ||
		inSync == nil || inSync.Status != metav1.ConditionFalse || inSync.Reason != "PolicyChanged" {
		t.Fatalf("status after verification policy update = %#v", actual.Status)
	}
	harvested := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Spec.TTLSecondsAfterFinished == nil || *harvested.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatalf("Job cleanup TTL = %#v", harvested.Spec.TTLSecondsAfterFinished)
	}
}

func TestCompletedJobCleanupPatchMustSucceedBeforeStatusAdvances(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: testDigest,
		JobName: "resolve-cleanup-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: testDigest, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
	failing := &failFirstJobCleanupPatchClient{Client: api}
	reconciler.Client = failing

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("Reconcile() advanced after an injected Job cleanup patch failure")
	}
	unchanged := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Status.ActiveOperation == nil || unchanged.Status.Phase != operatorv1alpha1.PhaseResolving {
		t.Fatalf("status advanced before cleanup was scheduled: %#v", unchanged.Status)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	completed := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status.ActiveOperation != nil || completed.Status.Phase != operatorv1alpha1.PhaseVerifying {
		t.Fatalf("retry did not consume result: %#v", completed.Status)
	}
	harvested := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Spec.TTLSecondsAfterFinished == nil || *harvested.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatalf("Job cleanup TTL = %#v", harvested.Spec.TTLSecondsAfterFinished)
	}
}

func TestFailedApplyWithMissingFrameForcesReadOnlyObservation(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	schema.Status.Target.CoordinationDigest = testCoordinationDigest
	schema.Status.Target.IdentityDigest = testDigest
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Fingerprint: testDigest, ContentDigest: testDigest, CoordinationDigest: testCoordinationDigest,
	}
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
	schema.Status.Target.CoordinationDigest = testCoordinationDigest
	schema.Status.Target.IdentityDigest = testDigest
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Fingerprint: testDigest, ContentDigest: testDigest, CoordinationDigest: testCoordinationDigest,
	}
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
	first.Status.Target.CoordinationDigest = testCoordinationDigest
	first.Status.Target.IdentityDigest = testDigest
	first.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "operation-a",
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		LeaseDurationSeconds: 960, LeaseEpoch: testLeaseEpoch,
	}
	second := first.DeepCopy()
	second.Namespace = "team-b"
	second.Name = "second"
	second.UID = "schema-b-uid"
	second.Status.ActiveOperation.ID = "operation-b"
	second.Status.ActiveOperation.LeaseEpoch = testLeaseEpochOther

	reconciler, api := fakeReconciler(t, staticLogs{}, first, second)
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
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: "apply-operation",
		ApplyGeneration: 1,
		Plan: operatorv1alpha1.CurrentPlanStatus{
			ArtifactDigest: testDigest, CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		},
		Source: operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: schema.Status.Source.ResolvedReference, Digest: testDigest,
		},
		Target: databaseTargetBinding(schema.Spec.Target), CoordinationDigest: testCoordinationDigest,
		LeaseDurationSeconds: 960, LeaseEpoch: testLeaseEpoch,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Locks = targetlock.New(api, api, nil)

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
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: testCoordinationKey,
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
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Status.ActiveOperation.JobName, UID: "job-uid", OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)}},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: job.Name + "-pod", UID: "pod-uid",
		Labels: map[string]string{"job-name": job.Name}, OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}}}
	return job, pod
}

func TestTerminalWorkloadFixtureMatchesImmutableIntent(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "fixture-operation", JobName: "fixture-job", Attempt: 1,
	}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	expected, err := (fakeJobs{}).Build(schema, *schema.Status.ActiveOperation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJobIntent(job, expected, schema); err != nil {
		t.Fatalf("Job fixture: %v", err)
	}
	if err := validatePodIntent(pod, job); err != nil {
		t.Fatalf("Pod fixture: %v", err)
	}
}

func schemaControllerReference(schema *operatorv1alpha1.PtahSchema) metav1.OwnerReference {
	return *metav1.NewControllerRef(schema, operatorv1alpha1.GroupVersion.WithKind("PtahSchema"))
}

func jobControllerReference(job *batchv1.Job) metav1.OwnerReference {
	return *metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))
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
	testClock := &safetyClock{now: clock}
	reconciler := &SchemaReconciler{
		Client: api, APIReader: api, Scheme: scheme, Logs: logs, Jobs: fakeJobs{},
		LockNamespace: "ptah-system",
		Clock:         testClock.Now,
	}
	reconciler.Locks = targetlock.New(api, api, testClock)
	seedFixtureLocks(t, reconciler, api, objects)
	return reconciler, api
}

func seedFixtureLocks(t *testing.T, reconciler *SchemaReconciler, api client.Client, objects []client.Object) {
	t.Helper()
	for _, object := range objects {
		schema, ok := object.(*operatorv1alpha1.PtahSchema)
		if !ok {
			continue
		}
		var request targetlock.Request
		switch {
		case schema.Status.PendingObservation != nil && schema.Status.PendingObservation.LeaseEpoch != "":
			request = reconciler.pendingLockRequest(schema, schema.Status.PendingObservation)
		case schema.Status.ActiveOperation != nil && schema.Status.ActiveOperation.LeaseEpoch != "" &&
			(schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply || schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationPlan):
			operation := schema.Status.ActiveOperation
			request = targetlock.Request{
				CoordinationNamespace: reconciler.LockNamespace,
				CoordinationDigest:    operation.CoordinationDigest,
				Holder: targetlock.Holder{
					SchemaUID: schema.UID, OperationID: operation.ID,
				},
				Duration: time.Duration(operation.LeaseDurationSeconds) * time.Second,
			}
		default:
			continue
		}
		result, err := reconciler.Locks.Acquire(context.Background(), request)
		if err != nil {
			t.Fatalf("seed fixture target lock: %v", err)
		}
		if !result.Acquired {
			continue
		}
		stored := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
			t.Fatalf("read schema for fixture target lock: %v", err)
		}
		if stored.Status.PendingObservation != nil {
			stored.Status.PendingObservation.LeaseEpoch = result.Epoch
			schema.Status.PendingObservation.LeaseEpoch = result.Epoch
		}
		if stored.Status.ActiveOperation != nil {
			stored.Status.ActiveOperation.LeaseEpoch = result.Epoch
			schema.Status.ActiveOperation.LeaseEpoch = result.Epoch
			if stored.Status.ActiveOperation.InputFingerprint != "" {
				inputs, inputErr := operationInputs(stored, stored.Status.ActiveOperation.Type)
				if inputErr != nil {
					t.Fatalf("rebind fixture operation after target lock acquisition: %v", inputErr)
				}
				digest, digestErr := fingerprint.DigestCanonicalJSON(inputs)
				if digestErr != nil {
					t.Fatalf("fingerprint rebound fixture operation: %v", digestErr)
				}
				stored.Status.ActiveOperation.InputFingerprint = digest
				schema.Status.ActiveOperation.InputFingerprint = digest
			}
		}
		if err := api.Status().Update(context.Background(), stored); err != nil {
			t.Fatalf("persist fixture target lock epoch: %v", err)
		}
	}
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
	operation := schema.Status.ActiveOperation
	if operation == nil {
		t.Fatal("bindActiveInput requires an active operation")
	}
	if operation.Type == operatorv1alpha1.OperationVerify {
		if operation.VerificationPolicyUID == "" {
			operation.VerificationPolicyUID = schema.Status.Source.VerificationPolicyUID
			if operation.VerificationPolicyUID == "" {
				operation.VerificationPolicyUID = testPolicyUID
			}
		}
		if operation.VerificationPolicyDigest == "" {
			operation.VerificationPolicyDigest = schema.Status.Source.VerificationPolicyDigest
			if operation.VerificationPolicyDigest == "" {
				operation.VerificationPolicyDigest = testDigest
			}
		}
	}
	if operation.Type == operatorv1alpha1.OperationApply {
		artifactDigest := testDigest
		if schema.Status.Plan != nil && schema.Status.Plan.ArtifactDigest != "" {
			artifactDigest = schema.Status.Plan.ArtifactDigest
		}
		if schema.Status.Source.Digest == "" {
			schema.Status.Source.Digest = artifactDigest
		}
		if schema.Status.Source.ResolvedReference == "" {
			schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + schema.Status.Source.Digest
		}
		schema.Status.Source.Verified = true
		if schema.Status.Source.VerificationPolicyUID == "" {
			schema.Status.Source.VerificationPolicyUID = testPolicyUID
		}
		if schema.Status.Source.VerificationPolicyDigest == "" {
			schema.Status.Source.VerificationPolicyDigest = testDigest
		}
		if schema.Status.Plan != nil && schema.Status.Plan.ArtifactDigest == "" {
			schema.Status.Plan.ArtifactDigest = schema.Status.Source.Digest
		}
		if schema.Status.Plan != nil && schema.Status.Plan.VerificationPolicyUID == "" {
			schema.Status.Plan.VerificationPolicyUID = schema.Status.Source.VerificationPolicyUID
		}
		if schema.Status.Plan != nil && schema.Status.Plan.VerificationPolicyDigest == "" {
			schema.Status.Plan.VerificationPolicyDigest = schema.Status.Source.VerificationPolicyDigest
		}
		if schema.Status.ActiveOperation.CoordinationDigest == "" {
			schema.Status.ActiveOperation.CoordinationDigest = schema.Status.Target.CoordinationDigest
			if schema.Status.ActiveOperation.CoordinationDigest == "" {
				schema.Status.ActiveOperation.CoordinationDigest = testCoordinationDigest
			}
		}
		if schema.Status.ActiveOperation.TargetIdentityDigest == "" {
			schema.Status.ActiveOperation.TargetIdentityDigest = schema.Status.Target.IdentityDigest
		}
		if schema.Status.Plan != nil && schema.Status.Plan.CoordinationDigest == "" {
			schema.Status.Plan.CoordinationDigest = schema.Status.ActiveOperation.CoordinationDigest
		}
		if schema.Status.Plan != nil && schema.Status.Plan.TargetIdentityDigest == "" {
			schema.Status.Plan.TargetIdentityDigest = schema.Status.ActiveOperation.TargetIdentityDigest
		}
		if schema.Status.ActiveOperation.Target == nil {
			target := databaseTargetBinding(schema.Spec.Target)
			schema.Status.ActiveOperation.Target = &target
		}
		if schema.Status.ActiveOperation.Source == nil {
			schema.Status.ActiveOperation.Source = artifactAccessBinding(schema)
		}
		if schema.Status.ActiveOperation.LeaseDurationSeconds == 0 {
			schema.Status.ActiveOperation.LeaseDurationSeconds = 960
		}
		if schema.Status.ActiveOperation.DispatchNotAfter == nil {
			deadline := metav1.NewTime(schema.Status.ActiveOperation.StartedAt.Add(
				time.Duration(schema.Status.ActiveOperation.LeaseDurationSeconds)*time.Second - time.Minute,
			))
			schema.Status.ActiveOperation.DispatchNotAfter = &deadline
		}
		if schema.Status.ActiveOperation.ExecutionNotAfter == nil {
			schema.Status.ActiveOperation.ExecutionNotAfter = schema.Status.ActiveOperation.DispatchNotAfter.DeepCopy()
		}
		if schema.Status.ActiveOperation.TerminationGracePeriodSeconds == 0 {
			schema.Status.ActiveOperation.TerminationGracePeriodSeconds = 30
		}
	}
	if operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan {
		if schema.Status.Source.Digest == "" {
			schema.Status.Source.Digest = testDigest
		}
		if schema.Status.Source.ResolvedReference == "" {
			schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + schema.Status.Source.Digest
		}
		schema.Status.Source.Verified = true
		if schema.Status.Source.ArtifactType == "" {
			schema.Status.Source.ArtifactType = dataplane.SchemaArtifactType
		}
		if pending := schema.Status.PendingObservation; pending != nil {
			operation.CoordinationDigest = pending.CoordinationDigest
			operation.TargetIdentityDigest = pending.Plan.TargetIdentityDigest
			operation.LeaseDurationSeconds = pending.LeaseDurationSeconds
			operation.LeaseEpoch = pending.LeaseEpoch
			target := pending.Target
			operation.Target = &target
			operation.Source = pending.Source.DeepCopy()
			operation.ObservationExclude = append([]string(nil), pending.Exclude...)
			operation.ObservationSeverity = pending.DriftSeverity
			operation.ObservationDev = pending.Dev.DeepCopy()
			operation.ObservationConnectTimeout = pending.ConnectTimeout
			operation.ObservationLockTimeout = pending.LockTimeout
		} else {
			if operation.CoordinationDigest == "" {
				operation.CoordinationDigest = testCoordinationDigest
			}
			if operation.TargetIdentityDigest == "" {
				operation.TargetIdentityDigest = schema.Status.Target.IdentityDigest
			}
			if operation.Target == nil {
				target := databaseTargetBinding(schema.Spec.Target)
				operation.Target = &target
			}
			if operation.Source == nil {
				operation.Source = artifactAccessBinding(schema)
			}
			if operation.Type == operatorv1alpha1.OperationPlan {
				if operation.LeaseDurationSeconds == 0 {
					operation.LeaseDurationSeconds = 960
				}
				if operation.LeaseEpoch == "" {
					operation.LeaseEpoch = testLeaseEpoch
				}
			}
		}
	}
	if operation.Type == operatorv1alpha1.OperationApply && operation.LeaseEpoch == "" {
		operation.LeaseEpoch = testLeaseEpoch
	}
	if operationNeedsTargetLock(schema) && operation.JobUID != "" {
		operation.DispatchStarted = true
	}
	inputs, err := operationInputs(schema, schema.Status.ActiveOperation.Type)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Type == operatorv1alpha1.OperationVerify {
		inputs["verification_policy_uid"] = string(operation.VerificationPolicyUID)
		inputs["verification_policy_digest"] = operation.VerificationPolicyDigest
	}
	digest, err := fingerprint.DigestCanonicalJSON(inputs)
	if err != nil {
		t.Fatal(err)
	}
	schema.Status.ActiveOperation.InputFingerprint = digest
}

func mustTestCoordinationDigest() string {
	digest, err := fingerprint.DatabaseCoordinationDigest(string(operatorv1alpha1.DatabaseEnginePostgreSQL), testCoordinationKey)
	if err != nil {
		panic(err)
	}
	return digest
}
