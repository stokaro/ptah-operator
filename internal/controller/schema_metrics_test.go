package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

type telemetryObservation struct {
	reconciliations []telemetry.ReconciliationResult
	drifts          []telemetry.DriftOutcome
	plans           []bool
	approvals       []telemetry.ApprovalOutcome
	applies         []telemetry.ApplyOutcome
	operations      []telemetry.OperationOutcome
	failures        []telemetry.FailureCategory
}

type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("API unavailable")
}

func (o *telemetryObservation) ObserveReconciliation(result telemetry.ReconciliationResult) {
	o.reconciliations = append(o.reconciliations, result)
}

func (o *telemetryObservation) ObserveDrift(_ operatorv1alpha1.DatabaseEngine, outcome telemetry.DriftOutcome) {
	o.drifts = append(o.drifts, outcome)
}

func (o *telemetryObservation) ObservePlan(_ operatorv1alpha1.DatabaseEngine, destructive bool) {
	o.plans = append(o.plans, destructive)
}

func (o *telemetryObservation) ObserveApproval(outcome telemetry.ApprovalOutcome) {
	o.approvals = append(o.approvals, outcome)
}

func (o *telemetryObservation) ObserveApply(outcome telemetry.ApplyOutcome) {
	o.applies = append(o.applies, outcome)
}

func (o *telemetryObservation) ObserveOperation(_ operatorv1alpha1.OperationType, outcome telemetry.OperationOutcome, _ time.Duration) {
	o.operations = append(o.operations, outcome)
}

func (o *telemetryObservation) ObserveFailure(_ telemetry.FailureStage, category telemetry.FailureCategory) {
	o.failures = append(o.failures, category)
}

func TestReconcileOutcomeTelemetry(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		schema := schemaFixture()
		reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
		observations := &telemetryObservation{}
		reconciler.Telemetry = observations
		if _, err := reconciler.Reconcile(context.Background(), requestFor(schema)); err != nil {
			t.Fatal(err)
		}
		if len(observations.reconciliations) != 1 || observations.reconciliations[0] != telemetry.ReconciliationSucceeded {
			t.Fatalf("reconciliation observations = %#v, want success", observations.reconciliations)
		}
	})

	t.Run("error", func(t *testing.T) {
		schema := schemaFixture()
		reconciler, reader := fakeReconciler(t, staticLogs{}, schema)
		observations := &telemetryObservation{}
		reconciler.APIReader = failingReader{Reader: reader}
		reconciler.Telemetry = observations
		if _, err := reconciler.Reconcile(context.Background(), requestFor(schema)); err == nil {
			t.Fatal("Reconcile() error = nil, want API failure")
		}
		if len(observations.reconciliations) != 1 || observations.reconciliations[0] != telemetry.ReconciliationFailed {
			t.Fatalf("reconciliation observations = %#v, want error", observations.reconciliations)
		}
		if len(observations.failures) != 1 || observations.failures[0] != telemetry.FailureInfrastructure {
			t.Fatalf("failure observations = %#v, want infrastructure", observations.failures)
		}
	})
}

func requestFor(schema *operatorv1alpha1.PtahSchema) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
}

func TestPlanAndApprovalRequiredTransitionIsReportedOnce(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	// A previous plan can be invalidated while the ApprovalRequired condition
	// remains true. Publishing the replacement must still produce one event.
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "Waiting", "An earlier plan required approval")
	reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
	recorder := record.NewFakeRecorder(10)
	observations := &telemetryObservation{}
	reconciler.Recorder = recorder
	reconciler.Telemetry = observations

	after := schema.DeepCopy()
	after.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "plan", UID: types.UID("plan-uid"), Fingerprint: testDigest,
		Destructive: true,
	}
	setCondition(after, operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, "Published", "Plan is ready")
	setCondition(after, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "PlanReady", "Approval is required")
	if err := reconciler.patchStatus(context.Background(), schema, after); err != nil {
		t.Fatal(err)
	}
	assertEvent(t, recorder, corev1.EventTypeNormal+" ApprovalRequired")
	if len(observations.plans) != 1 || !observations.plans[0] {
		t.Fatalf("plan observations = %#v, want one destructive plan", observations.plans)
	}
	if len(observations.approvals) != 1 || observations.approvals[0] != telemetry.ApprovalRequired {
		t.Fatalf("approval observations = %#v, want required", observations.approvals)
	}

	unchanged := after.DeepCopy()
	if err := reconciler.patchStatus(context.Background(), after, unchanged); err != nil {
		t.Fatal(err)
	}
	assertNoEvent(t, recorder)
	if len(observations.plans) != 1 || len(observations.approvals) != 1 {
		t.Fatalf("unchanged status repeated telemetry: plans=%#v approvals=%#v", observations.plans, observations.approvals)
	}
}

func TestApprovalAcceptedTransitionIsReportedOnce(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: "plan", UID: "plan-uid", Fingerprint: testDigest}
	reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
	recorder := record.NewFakeRecorder(10)
	observations := &telemetryObservation{}
	reconciler.Recorder = recorder
	reconciler.Telemetry = observations

	after := schema.DeepCopy()
	after.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
	if err := reconciler.patchStatus(context.Background(), schema, after); err != nil {
		t.Fatal(err)
	}
	assertEvent(t, recorder, corev1.EventTypeNormal+" ApprovalAccepted")
	if len(observations.approvals) != 1 || observations.approvals[0] != telemetry.ApprovalAccepted {
		t.Fatalf("approval observations = %#v, want accepted", observations.approvals)
	}
}

func TestStalePlanAndVerificationPolicyTransitionsAreReported(t *testing.T) {
	t.Parallel()
	t.Run("plan", func(t *testing.T) {
		schema := schemaFixture()
		schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
			Name: "plan", UID: "plan-uid", Fingerprint: testDigest,
			Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"},
		}
		setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, "Published", "Plan is ready")
		reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
		recorder := record.NewFakeRecorder(10)
		observations := &telemetryObservation{}
		reconciler.Recorder = recorder
		reconciler.Telemetry = observations

		after := schema.DeepCopy()
		after.Status.Plan = nil
		setCondition(after, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "Stale", "Plan is stale")
		if err := reconciler.patchStatus(context.Background(), schema, after); err != nil {
			t.Fatal(err)
		}
		assertEvent(t, recorder, corev1.EventTypeWarning+" PlanStale")
		if len(observations.approvals) != 0 {
			t.Fatalf("plan invalidation double-counted approval status telemetry: %#v", observations.approvals)
		}
	})

	t.Run("verification policy", func(t *testing.T) {
		schema := schemaFixture()
		setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, "PolicySatisfied", "Verified")
		reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
		recorder := record.NewFakeRecorder(10)
		reconciler.Recorder = recorder

		after := schema.DeepCopy()
		setCondition(after, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyChanged", "Policy changed")
		if err := reconciler.patchStatus(context.Background(), schema, after); err != nil {
			t.Fatal(err)
		}
		assertEvent(t, recorder, corev1.EventTypeWarning+" VerificationPolicyInvalidated")
	})
}

func TestConsumedApprovalRecordsAcceptedAndCurrentConditions(t *testing.T) {
	t.Parallel()
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "approval", UID: "approval-uid", Generation: 1},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, approval)
	if err := reconciler.markApprovalConsumed(context.Background(), approval); err != nil {
		t.Fatal(err)
	}

	actual := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), types.NamespacedName{Namespace: approval.Namespace, Name: approval.Name}, actual); err != nil {
		t.Fatal(err)
	}
	accepted := metav1.ConditionFalse
	stale := metav1.ConditionTrue
	consumed := metav1.ConditionFalse
	for _, condition := range actual.Status.Conditions {
		switch condition.Type {
		case operatorv1alpha1.ConditionApprovalAccepted:
			accepted = condition.Status
		case operatorv1alpha1.ConditionApprovalStale:
			stale = condition.Status
		case operatorv1alpha1.ConditionApprovalConsumed:
			consumed = condition.Status
		}
	}
	if accepted != metav1.ConditionTrue || stale != metav1.ConditionFalse || consumed != metav1.ConditionTrue {
		t.Fatalf("approval conditions: accepted=%s stale=%s consumed=%s", accepted, stale, consumed)
	}
}

func TestStaleApprovalIsReportedOnce(t *testing.T) {
	t.Parallel()
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "approval", UID: "approval-uid", Generation: 1},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, approval)
	recorder := record.NewFakeRecorder(10)
	observations := &telemetryObservation{}
	reconciler.Recorder = recorder
	reconciler.Telemetry = observations

	if err := reconciler.markApprovalStale(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	assertEvent(t, recorder, corev1.EventTypeWarning+" ApprovalStale")
	if len(observations.approvals) != 1 || observations.approvals[0] != telemetry.ApprovalStale {
		t.Fatalf("approval observations = %#v, want stale", observations.approvals)
	}
	if err := reconciler.markApprovalStale(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	assertNoEvent(t, recorder)
	if len(observations.approvals) != 1 {
		t.Fatalf("stale approval repeated telemetry: %#v", observations.approvals)
	}

	actual := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), types.NamespacedName{Namespace: approval.Namespace, Name: approval.Name}, actual); err != nil {
		t.Fatal(err)
	}
	if !conditionIs(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalStale, metav1.ConditionTrue) ||
		!conditionIs(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalAccepted, metav1.ConditionFalse) {
		t.Fatalf("stale approval conditions = %#v", actual.Status.Conditions)
	}
}

func TestCurrentApprovalRecoveryRevalidatesTheObject(t *testing.T) {
	t.Parallel()
	t.Run("repairs crash window", func(t *testing.T) {
		schema, plan, approval := currentApprovalFixture()
		reconciler, api := fakeReconciler(t, staticLogs{}, approval)

		valid, err := reconciler.ensureCurrentApproval(context.Background(), schema, plan)
		if err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("ensureCurrentApproval() = false, want recovered approval")
		}
		actual := &operatorv1alpha1.PtahSchemaApproval{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), actual); err != nil {
			t.Fatal(err)
		}
		if !conditionIs(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalAccepted, metav1.ConditionTrue) ||
			!conditionIs(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed, metav1.ConditionTrue) ||
			!conditionIs(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalStale, metav1.ConditionFalse) {
			t.Fatalf("recovered approval conditions = %#v", actual.Status.Conditions)
		}
	})

	t.Run("rejects deleted object", func(t *testing.T) {
		schema, plan, _ := currentApprovalFixture()
		reconciler, _ := fakeReconciler(t, staticLogs{})
		valid, err := reconciler.ensureCurrentApproval(context.Background(), schema, plan)
		if err != nil {
			t.Fatal(err)
		}
		if valid {
			t.Fatal("ensureCurrentApproval() accepted a deleted approval")
		}
	})

	t.Run("rejects same name with new UID", func(t *testing.T) {
		schema, plan, approval := currentApprovalFixture()
		approval.UID = "replacement-uid"
		reconciler, api := fakeReconciler(t, staticLogs{}, approval)
		valid, err := reconciler.ensureCurrentApproval(context.Background(), schema, plan)
		if err != nil {
			t.Fatal(err)
		}
		if valid {
			t.Fatal("ensureCurrentApproval() accepted a replacement approval")
		}
		actual := &operatorv1alpha1.PtahSchemaApproval{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), actual); err != nil {
			t.Fatal(err)
		}
		if conditionIs(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed, metav1.ConditionTrue) {
			t.Fatal("replacement approval was marked consumed")
		}
	})
}

func TestFindApprovalPrefersCurrentMatchBeforeStaleCleanup(t *testing.T) {
	t.Parallel()
	schema, plan, current := currentApprovalFixture()
	current.CreationTimestamp = metav1.NewTime(time.Date(2026, 8, 30, 11, 1, 0, 0, time.UTC))
	stale := current.DeepCopy()
	stale.Name = "older-stale-approval"
	stale.UID = "older-stale-uid"
	stale.CreationTimestamp = metav1.NewTime(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC))
	stale.Spec.PlanFingerprint = "sha256:old-plan"
	reconciler, api := fakeReconciler(t, staticLogs{}, stale, current)
	recorder := record.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	actual, err := reconciler.findApproval(context.Background(), schema, plan)
	if err != nil {
		t.Fatal(err)
	}
	if actual == nil || actual.UID != current.UID {
		t.Fatalf("findApproval() = %#v, want current approval UID %q", actual, current.UID)
	}
	assertNoEvent(t, recorder)
	unchanged := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(stale), unchanged); err != nil {
		t.Fatal(err)
	}
	if conditionIs(unchanged.Status.Conditions, operatorv1alpha1.ConditionApprovalStale, metav1.ConditionTrue) {
		t.Fatal("matching approval was delayed by synchronous stale cleanup")
	}
}

func TestInvalidApprovalClearsApplyClaimWithoutStartingAJob(t *testing.T) {
	t.Parallel()
	schema, _, _ := currentApprovalFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Target.IdentityDigest = testDigest
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: testDigest, JobName: "apply-job",
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)), Attempt: 1,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Locks = targetlock.New(api, api, nil)
	reconciler.LockNamespace = "operator-locks"
	recorder := record.NewFakeRecorder(10)
	observations := &telemetryObservation{}
	reconciler.Recorder = recorder
	reconciler.Telemetry = observations

	if _, err := reconciler.approvalBecameInvalid(context.Background(), schema); err != nil {
		t.Fatal(err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil || actual.Status.Plan.Approval != nil ||
		actual.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
		t.Fatalf("status after approval revocation = %#v", actual.Status)
	}
	if contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatal("approval revocation retained the active-operation finalizer")
	}
	if len(observations.applies) != 1 || observations.applies[0] != telemetry.ApplyStale {
		t.Fatalf("apply observations = %#v, want stale", observations.applies)
	}
	assertEvent(t, recorder, corev1.EventTypeNormal+" ApprovalRequired")
	assertEvent(t, recorder, corev1.EventTypeWarning+" ApprovalRevoked")
}

func currentApprovalFixture() (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, *operatorv1alpha1.PtahSchemaApproval) {
	schema := schemaFixture()
	approvedAt := metav1.NewTime(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC))
	approver := operatorv1alpha1.ApprovalIdentity{Username: "approver@example.com", UID: "approver-uid", Groups: []string{"schema-approvers"}}
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "plan", UID: "plan-uid"},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			SchemaRef:   operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint: "sha256:plan", ArtifactDigest: "sha256:artifact", TargetIdentityDigest: "sha256:target",
			ActualStateFingerprint: "sha256:actual", DesiredStateFingerprint: "sha256:desired",
			PolicyFingerprint: "sha256:policy", VerificationPolicyDigest: "sha256:verification-policy",
			PtahVersion: "v0.3.0", ExecutorImage: "example.invalid/ptah@sha256:executor",
			RunnerImage: "example.invalid/operator@sha256:runner", RunnerProtocolVersion: 1,
		},
	}
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "approval", UID: "approval-uid"},
		Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
			SchemaRef:       plan.Spec.SchemaRef,
			PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint: plan.Spec.Fingerprint, ArtifactDigest: plan.Spec.ArtifactDigest,
			TargetIdentityDigest: plan.Spec.TargetIdentityDigest, ActualStateFingerprint: plan.Spec.ActualStateFingerprint,
			DesiredStateFingerprint: plan.Spec.DesiredStateFingerprint, PolicyFingerprint: plan.Spec.PolicyFingerprint,
			VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest, PtahVersion: plan.Spec.PtahVersion,
			ExecutorImage: plan.Spec.ExecutorImage, RunnerImage: plan.Spec.RunnerImage,
			RunnerProtocolVersion: plan.Spec.RunnerProtocolVersion, Approver: approver, ApprovedAt: approvedAt,
			AdmissionRequestUID: "admission-request-uid",
		},
	}
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint,
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{
			Name: approval.Name, UID: "approval-uid", Approver: approver, ApprovedAt: approvedAt,
		},
	}
	return schema, plan, approval
}

func conditionIs(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}

func assertEvent(t *testing.T, recorder *record.FakeRecorder, prefix string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.HasPrefix(event, prefix) {
			t.Fatalf("event = %q, want prefix %q", event, prefix)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event with prefix %q", prefix)
	}
}

func assertNoEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected repeated event %q", event)
	case <-time.After(20 * time.Millisecond):
	}
}
