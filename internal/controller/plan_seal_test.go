package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// planSealMismatchFixture stands a schema up with a completed Plan Job whose
// frame is sealed to sealedTo, and whose claim records digestOnClaim as the
// key it was dispatched under. The reconciler it returns holds
// testSchemaSealKey, so a caller that wants a restart -- a claim naming a key
// this reconciler no longer holds -- passes a sealedTo and digestOnClaim that
// agree with each other and disagree with testSchemaSealKey.
func planSealMismatchFixture(
	t *testing.T,
	sealedTo planseal.PublicKey,
	digestOnClaim string,
) (*SchemaReconciler, client.Client, *operatorv1alpha1.PtahSchema) {
	t.Helper()

	const declaredRowValue = "restart-witness@example.com"
	policyBytes := "policy"
	policyDigest := fingerprint.DigestBytes([]byte(policyBytes))
	planDocument := safetyPlanDocumentWithStatement(t, "observed-state",
		`INSERT INTO users (email) VALUES ('`+declaredRowValue+`')`)

	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
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
		Type:                    operatorv1alpha1.OperationPlan,
		ID:                      "restart-plan-operation",
		JobName:                 "restart-plan-job",
		JobUID:                  "job-uid",
		StartedAt:               metav1.Now(),
		Attempt:                 1,
		PlanSealPublicKeyDigest: digestOnClaim,
	}
	bindActiveInput(t, schema)
	sealed, err := planseal.Seal(planDocument, sealedTo)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		Stdout:               sealed,
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
	return reconciler, api, schema
}

// TestRestartedManagerRePlansRatherThanWaitingOnAnUnopenablePlan proves the
// direction #449 left open: a Plan Job sealed to a key the current process no
// longer holds -- because the manager that dispatched it has since restarted
// and generated a new one -- is retried rather than published, waited on, or
// treated as a fault requiring a person. Plan is read-only, so a fresh attempt
// sealed to the current key costs nothing the stale attempt did not already
// cost.
func TestRestartedManagerRePlansRatherThanWaitingOnAnUnopenablePlan(t *testing.T) {
	t.Parallel()

	staleKey, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	// The claim faithfully records the key the stale process dispatched
	// under; this reconciler's own key (testSchemaSealKey, from
	// fakeReconciler) is a different one, exactly as it would be after a
	// restart.
	reconciler, api, schema := planSealMismatchFixture(t, staleKey.PublicKey(), planSealPublicKeyDigest(staleKey.PublicKey()))
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("Reconcile() result = %#v, want a retry backoff rather than an immediate wait or a terminal stop", result)
	}

	after := safetyGetSchema(t, api, schema)
	if after.Status.Plan != nil {
		t.Fatalf("status.plan = %#v, want nothing published from a payload this process cannot open", after.Status.Plan)
	}
	if after.Status.ActiveOperation == nil || after.Status.ActiveOperation.Type != operatorv1alpha1.OperationPlan {
		t.Fatalf("active operation = %#v, want the same Plan claim kept for a fresh attempt", after.Status.ActiveOperation)
	}
	// retryOperation clears these so the next reconciliation dispatches a
	// fresh Job -- sealed, this time, to the key this process actually holds.
	if after.Status.ActiveOperation.Attempt <= schema.Status.ActiveOperation.Attempt ||
		after.Status.ActiveOperation.JobUID != "" || after.Status.ActiveOperation.DispatchStarted {
		t.Fatalf("active operation after the mismatch = %#v, want a fresh attempt with no Job", after.Status.ActiveOperation)
	}
	// The exact wording of the claim's own digest check, distinct from
	// planseal's generic decode-failure message: this proves the mismatch was
	// caught by comparing the claim's recorded digest, not merely by an
	// incidental decryption failure that happened to be reported the same way.
	condition := findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if condition == nil || !strings.Contains(condition.Message, "plan was sealed to a manager key this process does not hold") {
		t.Fatalf("ReconciliationFailed = %#v, want the claim's own key-mismatch message", condition)
	}
}

// TestClaimDoesNotRecordAKeyDigestBeforeTheOneDispatchAttempt proves the other
// half of the same claim: nothing is recorded until the Job that digest
// describes is actually about to be created, so a claim written and then
// abandoned before dispatch (a crash, a stale reconcile) never carries a
// digest for a Job that was never sealed to it.
func TestClaimDoesNotRecordAKeyDigestBeforeTheOneDispatchAttempt(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		Digest:            testDigest,
		Verified:          true,
	}
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		CoordinationDigest: testCoordinationDigest, IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	if _, err := reconciler.claim(context.Background(), schema, operatorv1alpha1.OperationPlan); err != nil {
		t.Fatalf("claim() error = %v", err)
	}
	claimed := safetyGetSchema(t, api, schema)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationPlan {
		t.Fatalf("active operation = %#v, want a Plan claim", claimed.Status.ActiveOperation)
	}
	if claimed.Status.ActiveOperation.PlanSealPublicKeyDigest != "" {
		t.Fatalf("PlanSealPublicKeyDigest = %q at claim time, want empty until dispatch",
			claimed.Status.ActiveOperation.PlanSealPublicKeyDigest)
	}

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	want := planSealPublicKeyDigest(reconciler.SealKey.PublicKey())
	// A few passes resolve the admission snapshot and the target lock before
	// one crosses the one-dispatch-attempt boundary. That boundary's own Job
	// is built by fakeJobs, too bare to pass immutable-intent validation --
	// unrelated to sealing -- so the attempt is retried; the digest is
	// recorded in the same status patch as the boundary itself, before that
	// outcome is known, so it survives the retry that follows within the same
	// reconciliation.
	var afterDispatchAttempt *operatorv1alpha1.PtahSchema
	for pass := range 3 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
		afterDispatchAttempt = safetyGetSchema(t, api, schema)
	}
	if afterDispatchAttempt.Status.ActiveOperation == nil {
		t.Fatalf("active operation was lost: %#v", afterDispatchAttempt.Status)
	}
	if afterDispatchAttempt.Status.ActiveOperation.PlanSealPublicKeyDigest != want {
		t.Fatalf("PlanSealPublicKeyDigest after the dispatch attempt = %q, want %q (this reconciler's own key)",
			afterDispatchAttempt.Status.ActiveOperation.PlanSealPublicKeyDigest, want)
	}
}
