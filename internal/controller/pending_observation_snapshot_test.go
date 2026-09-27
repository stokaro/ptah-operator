package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// status.pendingObservation.admissionSnapshot is optional in the API because
// this path writes a pending observation without one.
//
// An Apply claim persists its admission snapshot on the pass after it is
// written, and only then marks the dispatch. Before that, a Job may already
// stand under the name the claim reserved: anyone who can create a Job in the
// namespace can take the name. The claim did not create it, but a Job it does
// not own exactly under an Apply's name is one the controller refuses to guess
// about, so the Apply is finished as outcome-unknown and the database is read
// before anything else runs. The pending observation that records it comes
// from a claim that never had a snapshot to keep.
//
// If this row starts failing because the observation carries a snapshot, the
// path changed, and the field may be required after all.
func TestAnUndispatchedApplyWhoseJobNameIsTakenOwesProofWithoutASnapshot(t *testing.T) {
	t.Parallel()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
		Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
	}
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	executionNotAfter := metav1.NewTime(started.Add(leaseDuration(schema) - time.Minute))
	target := databaseTargetBinding(schema.Spec.Target)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "undispatched-apply", JobName: "undispatched-apply-job",
		StartedAt: metav1.NewTime(started), Attempt: 1,
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		LeaseDurationSeconds: int32(leaseDuration(schema) / time.Second), LeaseEpoch: testLeaseEpoch,
		ExecutionNotAfter: &executionNotAfter, TerminationGracePeriodSeconds: int64(applyTerminationGrace / time.Second),
		Target: &target, Source: artifactAccessBinding(schema),
		ObservationExclude:        append([]string(nil), schema.Spec.Policy.Exclude...),
		ObservationSeverity:       schema.Spec.Policy.DriftSeverity,
		ObservationDev:            schema.Spec.Dev.DeepCopy(),
		ObservationConnectTimeout: schema.Spec.Execution.ConnectTimeout,
		ObservationLockTimeout:    schema.Spec.Policy.LockTimeout,
	}
	bindActiveInput(t, schema)
	operation := schema.Status.ActiveOperation
	if operation.AdmissionSnapshot != nil || operation.DispatchStarted || operation.JobUID != "" {
		t.Fatalf("the fixture claim already crossed its dispatch boundary, so nothing here is about an "+
			"undispatched one: %#v", operation)
	}
	squatter := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: operation.JobName, UID: "squatter-job-uid",
	}}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig, squatter)
	reconciler.Locks = targetlock.New(api, api, nil)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	pending := actual.Status.PendingObservation
	if pending == nil || actual.Status.ActiveOperation != nil {
		t.Fatalf("the Apply whose Job name was taken was not finished as owing proof: %#v", actual.Status)
	}
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying); condition == nil ||
		condition.Reason != string(operatorv1alpha1.ReasonOutcomeUnknown) {
		t.Fatalf("Applying = %#v, want OutcomeUnknown", condition)
	}
	// The Job under the reserved name is what sent the claim here, and not some
	// other way an Apply ends unknown.
	const cause = "lost schema ownership"
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); condition == nil ||
		!strings.Contains(condition.Message, cause) {
		t.Fatalf("ReconciliationFailed = %#v, want one naming %q", condition, cause)
	}
	if pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown ||
		pending.ApplyOperationID != operation.ID {
		t.Fatalf("pending observation = %#v, want outcome-unknown proof for %s", pending, operation.ID)
	}
	if pending.AdmissionSnapshot != nil {
		t.Fatalf("pending observation carries an admission snapshot %#v, which an undispatched claim "+
			"never persisted", pending.AdmissionSnapshot)
	}
}
