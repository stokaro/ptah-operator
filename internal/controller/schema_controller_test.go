package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	testDigest             = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCoordinationKey    = "team-a/app-database"
	testLeaseEpoch         = "v1-11111111111111111111111111111111"
	testLeaseEpochOther    = "v1-22222222222222222222222222222222"
	testExecutionBindingID = "v1-33333333333333333333333333333333"
	testPolicyUID          = types.UID("verification-policy-v1-uid")
)

var testCoordinationDigest = mustTestCoordinationDigest()

type fakeJobs struct{}

type changedTemplateJobs struct{ fakeJobs }

type failingBuildJobs struct{ fakeJobs }

type executionBindingJobs struct {
	fakeJobs
	ptahVersion   string
	executorImage string
	runnerImage   string
	protocol      int32
}

func (changedTemplateJobs) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	job, err := (fakeJobs{}).Build(schema, operation, plan)
	if err != nil {
		return nil, err
	}
	if job.Spec.Template.Annotations == nil {
		job.Spec.Template.Annotations = map[string]string{}
	}
	job.Spec.Template.Annotations["operator.ptah.dev/rebuilt-template"] = "changed"
	return job, nil
}

func (failingBuildJobs) Build(
	*operatorv1alpha1.PtahSchema,
	operatorv1alpha1.ActiveOperationStatus,
	*operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	return nil, errors.New("injected Job build failure")
}

func (fakeJobs) NameFor(_ *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) (string, error) {
	return "ptah-" + strings.ToLower(string(operation.Type)) + "-test", nil
}

func (fakeJobs) Build(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus, _ *operatorv1alpha1.PtahSchemaPlan) (*batchv1.Job, error) {
	annotations := map[string]string{workload.AnnotationExecutionBindingID: operation.ExecutionBindingID}
	if operation.AdmissionSnapshot != nil {
		annotations[workload.AnnotationAdmissionSnapshotDigest] = operation.AdmissionSnapshot.Digest
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: operation.JobName,
		Annotations:     annotations,
		OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)},
	}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}}}, nil
}

func (fakeJobs) ExecutionBinding() (string, string, string, int32) {
	return "v0.3.0", "example.invalid/ptah@" + testDigest, "example.invalid/operator@" + testDigest, int32(runner.ProtocolVersion)
}

func (jobs executionBindingJobs) ExecutionBinding() (string, string, string, int32) {
	return jobs.ptahVersion, jobs.executorImage, jobs.runnerImage, jobs.protocol
}

func (jobs executionBindingJobs) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	if operation.Type == operatorv1alpha1.OperationApply && plan != nil &&
		(plan.Spec.PtahVersion != jobs.ptahVersion || plan.Spec.ExecutorImage != jobs.executorImage ||
			plan.Spec.RunnerImage != jobs.runnerImage || plan.Spec.RunnerProtocolVersion != jobs.protocol) {
		return nil, errors.New("plan execution binding does not match the Job builder")
	}
	return (fakeJobs{}).Build(schema, operation, plan)
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

func TestInitialExecutionBindingIsDurableBeforeOperationClaim(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Status = operatorv1alpha1.PtahSchemaStatus{}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("persist initial execution binding: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("initial execution-binding result = %#v, want durable-boundary requeue", result)
	}
	bound := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), bound); err != nil {
		t.Fatal(err)
	}
	if bound.Status.ExecutionBinding == nil || !validExecutionBindingID(bound.Status.ExecutionBinding.Epoch) ||
		bound.Status.ActiveOperation != nil || len(bound.Finalizers) != 0 {
		t.Fatalf("initial execution binding was not isolated from the operation claim: %#v", bound)
	}
	epoch := bound.Status.ExecutionBinding.Epoch

	restarted := *reconciler
	if _, err := restarted.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim operation after restart: %v", err)
	}
	claimed := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.Status.ExecutionBinding == nil || claimed.Status.ExecutionBinding.Epoch != epoch ||
		claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
		claimed.Status.ActiveOperation.ExecutionBindingID != epoch {
		t.Fatalf("restart did not reuse the durable epoch for Resolve: %#v", claimed.Status)
	}
}

func TestExecutionBindingIDValidationMatchesStatusSchema(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		id   string
		want bool
	}{
		{id: testExecutionBindingID, want: true},
		{id: "v1-3333333333333333333333333333333A"},
		{id: "v2-33333333333333333333333333333333"},
		{id: "v1-3333"},
		{id: ""},
	} {
		if got := validExecutionBindingID(test.id); got != test.want {
			t.Fatalf("validExecutionBindingID(%q) = %t, want %t", test.id, got, test.want)
		}
	}
}

func TestSourceRefreshConditionsPreserveLastKnownEvidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fixture := func() (*operatorv1alpha1.PtahSchema, *corev1.ConfigMap) {
		schema := schemaFixture()
		resolvedAt := metav1.NewTime(now.Add(-20 * time.Minute))
		verifiedAt := metav1.NewTime(now.Add(-19 * time.Minute))
		observedAt := metav1.NewTime(now.Add(-18 * time.Minute))
		appliedAt := metav1.NewTime(now.Add(-17 * time.Minute))
		succeededAt := metav1.NewTime(now.Add(-16 * time.Minute))
		dueAt := metav1.NewTime(now)
		policyBytes := []byte("policy")
		schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			RequestedReference:       schema.Spec.Desired.OCIRef,
			ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
			Digest:                   testDigest,
			MediaType:                "application/vnd.oci.image.manifest.v1+json",
			ArtifactType:             dataplane.SchemaArtifactType,
			Size:                     321,
			Verified:                 true,
			VerificationPolicyUID:    testPolicyUID,
			VerificationPolicyDigest: fingerprint.DigestBytes(policyBytes),
			ResolvedAt:               &resolvedAt,
			VerifiedAt:               &verifiedAt,
		}
		schema.Status.Target = operatorv1alpha1.TargetStatus{
			Engine:               schema.Spec.Target.Engine,
			CoordinationDigest:   testCoordinationDigest,
			IdentityDigest:       testDigest,
			DriftReportDigest:    testDigest,
			LastObservedAt:       &observedAt,
			HighestDriftSeverity: "none",
		}
		schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
			Name: "retained-plan", UID: "retained-plan-uid", Fingerprint: testDigest,
			ContentDigest: testDigest, ArtifactDigest: testDigest,
			CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
			ExecutionBindingID: testExecutionBindingID,
			PtahVersion:        "v0.3.0", ExecutorImage: "example.invalid/ptah@" + testDigest,
			RunnerImage:           "example.invalid/operator@" + testDigest,
			RunnerProtocolVersion: int32(runner.ProtocolVersion), CreatedAt: observedAt,
		}
		schema.Status.Applied = &operatorv1alpha1.AppliedStatus{
			ArtifactDigest: testDigest, PlanFingerprint: testDigest,
			CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
			ExecutionBindingID: testExecutionBindingID,
			PtahVersion:        "v0.3.0", ExecutorImage: "example.invalid/ptah@" + testDigest,
			RunnerImage:           "example.invalid/operator@" + testDigest,
			RunnerProtocolVersion: int32(runner.ProtocolVersion), CompletedAt: appliedAt,
		}
		schema.Status.LastSuccessfulReconciliation = &succeededAt
		schema.Status.NextReconciliationTime = &dueAt
		setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionTrue, "DigestPinned", "Resolved")
		setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, "PolicySatisfied", "Verified")
		setCondition(schema, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionTrue, "Observed", "Database observed")
		setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "ScopedChanges", "Managed scope differs")
		setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, "ChangesPlanned", "Plan is ready")
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "Waiting", "Approval is required")
		setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, "ScopedChanges", "Managed scope differs")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "AwaitingApproval", "Waiting for approval")
		immutable := true
		return schema, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
			Immutable:  &immutable,
			Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: string(policyBytes)},
		}
	}
	assertEvidence := func(t *testing.T, want, actual *operatorv1alpha1.PtahSchema) {
		t.Helper()
		normalizeEvidenceTimes := func(schema *operatorv1alpha1.PtahSchema) {
			for _, timestamp := range []*metav1.Time{
				schema.Status.Source.ResolvedAt,
				schema.Status.Source.VerifiedAt,
				schema.Status.Target.LastObservedAt,
				schema.Status.LastSuccessfulReconciliation,
			} {
				if timestamp != nil {
					timestamp.Time = timestamp.Time.UTC()
				}
			}
			if schema.Status.Plan != nil {
				schema.Status.Plan.CreatedAt.Time = schema.Status.Plan.CreatedAt.Time.UTC()
			}
			if schema.Status.Applied != nil {
				schema.Status.Applied.CompletedAt.Time = schema.Status.Applied.CompletedAt.Time.UTC()
			}
		}
		want = want.DeepCopy()
		actual = actual.DeepCopy()
		normalizeEvidenceTimes(want)
		normalizeEvidenceTimes(actual)
		if !reflect.DeepEqual(actual.Status.Source, want.Status.Source) ||
			!reflect.DeepEqual(actual.Status.Target, want.Status.Target) ||
			!reflect.DeepEqual(actual.Status.Plan, want.Status.Plan) ||
			!reflect.DeepEqual(actual.Status.Applied, want.Status.Applied) ||
			!reflect.DeepEqual(actual.Status.LastSuccessfulReconciliation, want.Status.LastSuccessfulReconciliation) {
			t.Fatalf("source refresh changed retained evidence:\nwant %#v\n got %#v", want.Status, actual.Status)
		}
		verified := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
		if verified == nil || verified.Status != metav1.ConditionTrue || verified.Reason != "PolicySatisfied" {
			t.Fatalf("ArtifactVerified = %#v, want retained historical verification", verified)
		}
		for _, conditionType := range []string{
			operatorv1alpha1.ConditionDatabaseReachable,
			operatorv1alpha1.ConditionDriftDetected,
			operatorv1alpha1.ConditionApprovalRequired,
		} {
			before := findCondition(want.Status.Conditions, conditionType)
			after := findCondition(actual.Status.Conditions, conditionType)
			if before == nil || after == nil || before.Status != after.Status || before.Reason != after.Reason || before.Message != after.Message {
				t.Fatalf("%s = %#v, want retained condition %#v", conditionType, after, before)
			}
		}
	}
	assertFreshnessUnknown := func(t *testing.T, schema *operatorv1alpha1.PtahSchema, artifactReason, dependentReason string) {
		t.Helper()
		for _, check := range []struct {
			conditionType string
			reason        string
		}{
			{conditionType: operatorv1alpha1.ConditionArtifactResolved, reason: artifactReason},
			{conditionType: operatorv1alpha1.ConditionPlanReady, reason: dependentReason},
			{conditionType: operatorv1alpha1.ConditionInSync, reason: dependentReason},
		} {
			condition := findCondition(schema.Status.Conditions, check.conditionType)
			if condition == nil || condition.Status != metav1.ConditionUnknown || condition.Reason != check.reason {
				t.Fatalf("%s = %#v, want Unknown/%s", check.conditionType, condition, check.reason)
			}
		}
	}

	t.Run("claim", func(t *testing.T) {
		schema, policyConfig := fixture()
		want := schema.DeepCopy()
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if !result.Requeue {
			t.Fatalf("Reconcile() result = %#v, want immediate Resolve dispatch", result)
		}
		actual := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
			t.Fatal(err)
		}
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
			actual.Status.Phase != operatorv1alpha1.PhaseResolving {
			t.Fatalf("refresh claim status = %#v", actual.Status)
		}
		assertEvidence(t, want, actual)
		assertFreshnessUnknown(t, actual, "Refreshing", "SourceRefreshPending")
	})

	t.Run("failure", func(t *testing.T) {
		schema, policyConfig := fixture()
		want := schema.DeepCopy()
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("claim Reconcile() error = %v", err)
		}
		claimed := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), claimed); err != nil {
			t.Fatal(err)
		}
		result, err := reconciler.retryOperation(context.Background(), claimed, nil, errors.New("registry unavailable"))
		if err != nil {
			t.Fatalf("retryOperation() error = %v", err)
		}
		if result.RequeueAfter != failureRetry(claimed) {
			t.Fatalf("retryOperation() result = %#v", result)
		}
		actual := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
			t.Fatal(err)
		}
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
			actual.Status.ActiveOperation.Attempt != 2 || actual.Status.Phase != operatorv1alpha1.PhaseFailed ||
			actual.Status.NextReconciliationTime == nil {
			t.Fatalf("failed refresh status = %#v", actual.Status)
		}
		assertEvidence(t, want, actual)
		assertFreshnessUnknown(t, actual, "RefreshFailed", "SourceFreshnessUnknown")
	})

	t.Run("terminal runner failure", func(t *testing.T) {
		schema, policyConfig := fixture()
		want := schema.DeepCopy()
		schema.Finalizers = []string{activeOperationFinalizer}
		schema.Status.Phase = operatorv1alpha1.PhaseResolving
		schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationResolve, ID: "refresh-resolve", JobName: "refresh-resolve",
			JobUID: "job-uid", StartedAt: metav1.NewTime(now), Attempt: 1,
		}
		bindActiveInput(t, schema)
		job, pod := terminalWorkload(schema, batchv1.JobComplete)
		frame, err := runner.MarshalFrame(runner.Result{
			ProtocolVersion: runner.ProtocolVersion,
			Operation:       runner.OperationResolve,
			OperationID:     schema.Status.ActiveOperation.ID,
			ChildExitCode:   1,
			Error: &runner.ResultError{
				Code: "registry_unavailable", Message: "registry is unavailable",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, policyConfig, job, pod)
		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if result.RequeueAfter != failureRetry(schema) {
			t.Fatalf("Reconcile() result = %#v", result)
		}
		actual := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
			t.Fatal(err)
		}
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != 2 ||
			actual.Status.Phase != operatorv1alpha1.PhaseFailed {
			t.Fatalf("terminal refresh failure status = %#v", actual.Status)
		}
		assertEvidence(t, want, actual)
		assertFreshnessUnknown(t, actual, "RefreshFailed", "SourceFreshnessUnknown")
	})

	t.Run("configuration failure", func(t *testing.T) {
		schema, policyConfig := fixture()
		want := schema.DeepCopy()
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("claim Reconcile() error = %v", err)
		}
		claimed := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), claimed); err != nil {
			t.Fatal(err)
		}
		result, err := reconciler.operationFailure(context.Background(), claimed, errors.New("invalid resolver configuration"))
		if err != nil {
			t.Fatalf("operationFailure() error = %v", err)
		}
		if result.RequeueAfter != failureRetry(claimed) {
			t.Fatalf("operationFailure() result = %#v", result)
		}
		actual := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
			t.Fatal(err)
		}
		assertEvidence(t, want, actual)
		assertFreshnessUnknown(t, actual, "RefreshFailed", "SourceFreshnessUnknown")
	})

	t.Run("persisted claim build failure", func(t *testing.T) {
		schema, policyConfig := fixture()
		want := schema.DeepCopy()
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("claim Reconcile() error = %v", err)
		}
		reconciler.Jobs = failingBuildJobs{}
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatalf("build-failure Reconcile() error = %v", err)
		}
		if result.RequeueAfter != failureRetry(schema) {
			t.Fatalf("build-failure Reconcile() result = %#v", result)
		}
		actual := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
			t.Fatal(err)
		}
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
			actual.Status.Phase != operatorv1alpha1.PhaseFailed {
			t.Fatalf("build-failure refresh status = %#v", actual.Status)
		}
		assertEvidence(t, want, actual)
		assertFreshnessUnknown(t, actual, "RefreshFailed", "SourceFreshnessUnknown")
	})

	t.Run("suspended before dispatch", func(t *testing.T) {
		schema, policyConfig := fixture()
		want := schema.DeepCopy()
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("claim Reconcile() error = %v", err)
		}
		claimed := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), claimed); err != nil {
			t.Fatal(err)
		}
		claimed.Spec.Suspend = true
		if err := api.Update(context.Background(), claimed); err != nil {
			t.Fatal(err)
		}
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatalf("suspend Reconcile() error = %v", err)
		}
		if result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("suspend Reconcile() result = %#v", result)
		}
		actual := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
			t.Fatal(err)
		}
		if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseSuspended {
			t.Fatalf("suspended refresh status = %#v", actual.Status)
		}
		assertEvidence(t, want, actual)
		assertFreshnessUnknown(t, actual, "RefreshSuspended", "SourceFreshnessUnknown")
	})
}

func TestInitialSourceResolutionFailureHasNoRefreshEvidence(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "initial-resolve", JobName: "initial-resolve", Attempt: 1,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	result, err := reconciler.operationFailure(context.Background(), schema, errors.New("resolver configuration is invalid"))
	if err != nil {
		t.Fatalf("operationFailure() error = %v", err)
	}
	if result.RequeueAfter != failureRetry(schema) {
		t.Fatalf("operationFailure() result = %#v", result)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		conditionType string
		status        metav1.ConditionStatus
		reason        string
	}{
		{operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionFalse, "ResolveFailed"},
		{operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "SourceUnresolved"},
		{operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, "SourceResolutionUnknown"},
	}
	for _, check := range checks {
		condition := findCondition(actual.Status.Conditions, check.conditionType)
		if condition == nil || condition.Status != check.status || condition.Reason != check.reason {
			t.Fatalf("%s = %#v, want %s/%s", check.conditionType, condition, check.status, check.reason)
		}
	}
}

func TestPersistedAdmissionSnapshotRejectsChangedRebuiltTemplateBeforeDispatch(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("snapshot Reconcile() error = %v", err)
	}
	persisted := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.ActiveOperation == nil || persisted.Status.ActiveOperation.AdmissionSnapshot == nil {
		t.Fatalf("snapshot was not persisted before dispatch: %#v", persisted.Status.ActiveOperation)
	}
	if persisted.Status.ActiveOperation.DispatchStarted {
		t.Fatal("snapshot reconciliation started dispatch")
	}

	reconciler.Jobs = changedTemplateJobs{}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("changed-template Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Phase != operatorv1alpha1.PhasePending || actual.Status.ActiveOperation != nil {
		t.Fatalf("changed rebuilt template was not discarded before dispatch: %#v", actual.Status)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("changed rebuilt template created %d Jobs", len(jobs.Items))
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
	for _, test := range []struct {
		name        string
		childExit   int
		requirement string
	}{
		{name: "native refusal", childExit: 2, requirement: "require_signature"},
		{name: "runner digest pin refusal", childExit: 0, requirement: "require_digest_pin"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
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
				MediaType:          "application/vnd.oci.image.manifest.v1+json",
				Size:               321,
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
				ChildExitCode:            test.childExit,
				ResolvedDigest:           testDigest,
				VerificationRequirements: []string{test.requirement},
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
				actual.Status.Source.RequestedReference != schema.Spec.Desired.OCIRef ||
				actual.Status.Source.ResolvedReference != "oci://registry.example/team/schema@"+testDigest ||
				actual.Status.Source.Digest != testDigest ||
				actual.Status.Source.MediaType != "application/vnd.oci.image.manifest.v1+json" ||
				actual.Status.Source.Size != 321 ||
				verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != "PolicyRefused" ||
				inSync == nil || inSync.Status != metav1.ConditionFalse || inSync.Reason != "PolicyRefused" ||
				!strings.Contains(verified.Message, test.requirement) {
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

			reconciler.Clock = func() time.Time { return unchanged.Status.NextReconciliationTime.Time }
			result, err = reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("due blocked Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("due blocked Reconcile() result = %#v, want Resolve claim", result)
			}
			refreshing := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), refreshing); err != nil {
				t.Fatal(err)
			}
			if refreshing.Status.ActiveOperation == nil ||
				refreshing.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				refreshing.Status.Phase != operatorv1alpha1.PhaseResolving {
				t.Fatalf("due verification-refusal refresh status = %#v", refreshing.Status)
			}
		})
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

func TestBlockedPolicyWaitsUntilPersistedRefreshDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name        string
		apply       operatorv1alpha1.ApplyPolicy
		destructive bool
	}{
		{name: "destructive changes disabled", apply: operatorv1alpha1.ApplyPolicyAlways, destructive: true},
		{name: "apply disabled", apply: operatorv1alpha1.ApplyPolicyNever},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := blockedPolicySchema(test.apply, test.destructive)
			next := metav1.NewTime(now.Add(4 * time.Minute))
			schema.Status.NextReconciliationTime = &next
			wantPlan := schema.Status.Plan.DeepCopy()
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.Requeue || result.RequeueAfter != 4*time.Minute {
				t.Fatalf("Reconcile() result = %#v, want persisted-deadline wait", result)
			}
			actual := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
				t.Fatal(err)
			}
			if actual.Status.ActiveOperation != nil || actual.Status.NextReconciliationTime == nil ||
				!actual.Status.NextReconciliationTime.Time.Equal(next.Time) || !reflect.DeepEqual(actual.Status.Plan, wantPlan) {
				t.Fatalf("blocked status changed before deadline: %#v", actual.Status)
			}
		})
	}
}

func TestDueBlockedPolicyClaimsResolveWithoutClearingCurrentPlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name        string
		apply       operatorv1alpha1.ApplyPolicy
		destructive bool
	}{
		{name: "destructive changes disabled", apply: operatorv1alpha1.ApplyPolicyAlways, destructive: true},
		{name: "apply disabled", apply: operatorv1alpha1.ApplyPolicyNever},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := blockedPolicySchema(test.apply, test.destructive)
			dueAt := metav1.NewTime(now)
			schema.Status.NextReconciliationTime = &dueAt
			wantPlan := schema.Status.Plan.DeepCopy()
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !result.Requeue {
				t.Fatalf("Reconcile() result = %#v, want Resolve claim", result)
			}
			actual := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
				t.Fatal(err)
			}
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
				actual.Status.Phase != operatorv1alpha1.PhaseResolving || actual.Status.NextReconciliationTime != nil ||
				!reflect.DeepEqual(actual.Status.Plan, wantPlan) {
				t.Fatalf("due blocked refresh status = %#v", actual.Status)
			}
		})
	}
}

func TestLegacyBlockedStatusWithoutDeadlineClaimsResolveImmediately(t *testing.T) {
	t.Parallel()
	schema := blockedPolicySchema(operatorv1alpha1.ApplyPolicyAlways, true)
	wantPlan := schema.Status.Plan.DeepCopy()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("Reconcile() result = %#v, want immediate Resolve claim", result)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
		!reflect.DeepEqual(actual.Status.Plan, wantPlan) {
		t.Fatalf("legacy blocked refresh status = %#v", actual.Status)
	}
}

func TestWaitBlockedPersistsFutureRefreshDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	schema := schemaFixture()
	schema.Spec.Interval = metav1.Duration{Duration: 7 * time.Minute}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	current := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), current); err != nil {
		t.Fatal(err)
	}

	result, err := reconciler.waitBlocked(context.Background(), current, "ApplyDisabled", "Policy records plans but does not apply them")
	if err != nil {
		t.Fatalf("waitBlocked() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 7*time.Minute {
		t.Fatalf("waitBlocked() result = %#v", result)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	wantNext := now.Add(7 * time.Minute)
	if actual.Status.Phase != operatorv1alpha1.PhaseBlocked || actual.Status.NextReconciliationTime == nil ||
		!actual.Status.NextReconciliationTime.Time.Equal(wantNext) {
		t.Fatalf("persisted blocked status = %#v, want deadline %s", actual.Status, wantNext)
	}

	restartResult, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("restart Reconcile() error = %v", err)
	}
	if restartResult.Requeue || restartResult.RequeueAfter != 7*time.Minute {
		t.Fatalf("restart Reconcile() result = %#v, want persisted-deadline wait", restartResult)
	}
	afterRestart := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), afterRestart); err != nil {
		t.Fatal(err)
	}
	if afterRestart.Status.ActiveOperation != nil || afterRestart.Status.NextReconciliationTime == nil ||
		!afterRestart.Status.NextReconciliationTime.Time.Equal(wantNext) {
		t.Fatalf("restart changed blocked deadline: %#v", afterRestart.Status)
	}
}

func blockedPolicySchema(apply operatorv1alpha1.ApplyPolicy, destructive bool) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Interval = metav1.Duration{Duration: 10 * time.Minute}
	schema.Spec.Policy.Apply = apply
	schema.Spec.Policy.AllowDestructive = !destructive
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "blocked-plan", UID: "blocked-plan-uid", Fingerprint: testDigest,
		ContentDigest: testDigest, ArtifactDigest: testDigest, Destructive: destructive,
		ExecutionBindingID: testExecutionBindingID,
		PtahVersion:        "v0.3.0", ExecutorImage: "example.invalid/ptah@" + testDigest,
		RunnerImage: "example.invalid/operator@" + testDigest, RunnerProtocolVersion: int32(runner.ProtocolVersion),
	}
	if destructive {
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "PolicyBlocked", "Plan is blocked by destructive-change policy")
	} else {
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "ApplyDisabled", "Plan is ready but apply is disabled")
	}
	return schema
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
		Status: operatorv1alpha1.PtahSchemaStatus{
			ObservedGeneration: 1,
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch: testExecutionBindingID, PtahVersion: "v0.3.0",
				ExecutorImage:         "example.invalid/ptah@" + testDigest,
				RunnerImage:           "example.invalid/operator@" + testDigest,
				RunnerProtocolVersion: int32(runner.ProtocolVersion),
			},
		},
	}
}

func terminalWorkload(schema *operatorv1alpha1.PtahSchema, conditionType batchv1.JobConditionType) (*batchv1.Job, *corev1.Pod) {
	ensureTestAdmissionSnapshot(schema)
	annotations := map[string]string{
		workload.AnnotationAdmissionSnapshotDigest: schema.Status.ActiveOperation.AdmissionSnapshot.Digest,
		workload.AnnotationExecutionBindingID:      schema.Status.ActiveOperation.ExecutionBindingID,
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Status.ActiveOperation.JobName, UID: "job-uid", Annotations: annotations, OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)}},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}}},
	}
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: job.Name + "-pod", UID: "pod-uid",
		Labels: map[string]string{"job-name": job.Name}, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}, Spec: corev1.PodSpec{
		ServiceAccountName: "default", Priority: &priority, PreemptionPolicy: &preemption,
		Tolerations: []corev1.Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
			{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		},
	}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}}}
	return job, pod
}

func ensureTestAdmissionSnapshot(schema *operatorv1alpha1.PtahSchema) {
	if schema.Status.ActiveOperation.AdmissionSnapshot != nil {
		return
	}
	policy := corev1.PreemptLowerPriority
	templateDigest, err := podintent.DigestTemplate(&corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{
			workload.AnnotationExecutionBindingID: schema.Status.ActiveOperation.ExecutionBindingID,
		},
	}})
	if err != nil {
		panic(err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{Object: operatorv1alpha1.AdmissionObjectBinding{
			Name: "default", UID: "default-service-account-uid", ResourceVersion: "1",
		}},
		PriorityClass:                       operatorv1alpha1.PriorityClassAdmissionSnapshot{Value: 0, PreemptionPolicy: &policy},
		DefaultTolerationsEnabled:           true,
		DefaultNotReadyTolerationSeconds:    300,
		DefaultUnreachableTolerationSeconds: 300,
		ExtendedResourceTolerationEnabled:   false,
		AlwaysPullImagesEnabled:             false,
	}
	copy := *snapshot
	digest, err := fingerprint.DigestCanonicalJSON(copy)
	if err != nil {
		panic(err)
	}
	snapshot.Digest = digest
	schema.Status.ActiveOperation.AdmissionSnapshot = snapshot
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
	if err := validatePodIntent(pod, job, schema.Status.ActiveOperation.AdmissionSnapshot); err != nil {
		t.Fatalf("Pod fixture: %v", err)
	}
}

func TestValidateJobIntentAcceptsSemanticQuantityRoundTrip(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	size := *resource.NewQuantity(2<<20, resource.BinarySI)
	expected := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      "custom-ca-observe",
			Labels:    map[string]string{"intent": "fixed"},
			Annotations: map[string]string{
				"intent": "fixed",
			},
			OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "registry-ca-snapshot",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory, SizeLimit: &size,
			}},
		}}}}},
	}
	payload, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	actual := &batchv1.Job{}
	if err := json.Unmarshal(payload, actual); err != nil {
		t.Fatal(err)
	}
	actual.UID = "job-uid"
	if err := validateJobIntent(actual, expected, schema); err != nil {
		t.Fatalf("validateJobIntent() rejected API-equivalent quantity: %v", err)
	}

	changed := actual.DeepCopy()
	changedSize := resource.MustParse("3Mi")
	changed.Spec.Template.Spec.Volumes[0].EmptyDir.SizeLimit = &changedSize
	if err := validateJobIntent(changed, expected, schema); err == nil {
		t.Fatal("validateJobIntent() accepted a changed custom-CA snapshot size")
	}

	changed = actual.DeepCopy()
	changed.Spec.Template.Spec.NodeSelector = map[string]string{}
	if err := validateJobIntent(changed, expected, schema); err == nil {
		t.Fatal("validateJobIntent() treated an empty map as an absent immutable field")
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
	for _, add := range []func(*runtime.Scheme) error{operatorv1alpha1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme, nodev1.AddToScheme, schedulingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	hasDefaultServiceAccount := false
	for _, object := range objects {
		if serviceAccount, ok := object.(*corev1.ServiceAccount); ok && serviceAccount.Namespace == "team-a" && serviceAccount.Name == "default" {
			hasDefaultServiceAccount = true
		}
	}
	if !hasDefaultServiceAccount {
		objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "default", UID: "default-service-account-uid", ResourceVersion: "1",
		}})
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
		LockNamespace:    "ptah-system",
		Clock:            testClock.Now,
		AdmissionOptions: podintent.DefaultOptions(),
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
	if schema.Status.ExecutionBinding == nil {
		schema.Status.ExecutionBinding = &operatorv1alpha1.ExecutionBindingStatus{
			Epoch: testExecutionBindingID, PtahVersion: "v0.3.0",
			ExecutorImage:         "example.invalid/ptah@" + testDigest,
			RunnerImage:           "example.invalid/operator@" + testDigest,
			RunnerProtocolVersion: int32(runner.ProtocolVersion),
		}
	}
	if operation.ExecutionBindingID == "" {
		operation.ExecutionBindingID = schema.Status.ExecutionBinding.Epoch
	}
	if schema.Status.Plan != nil && schema.Status.Plan.ExecutionBindingID == "" {
		schema.Status.Plan.ExecutionBindingID = schema.Status.ExecutionBinding.Epoch
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
