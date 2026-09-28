package controller

import (
	"context"
	"encoding/json"
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
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
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
	// A real dispatch's persisted snapshot is resolved from the Job template
	// it actually built, sealed to the key that Job was dispatched under --
	// sealedTo here, before the restart -- not to testSchemaSealKey,
	// terminalWorkload's default. Pre-set it so the snapshot terminalWorkload
	// finds already there, and the Job it builds around it, agree on sealedTo
	// before either is ever read.
	schema.Status.ActiveOperation.AdmissionSnapshot = testAdmissionSnapshotFor(schema.Status.ActiveOperation, sealedTo)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	// terminalWorkload still builds its container from testSchemaSealKey; the
	// snapshot above already reflects sealedTo, so only the Job's (and,
	// through the same slice, the Pod's) own container needs the same
	// rewrite for the two to agree exactly as a real dispatch's always do.
	setJobPlanSealKeyEnv(t, job, sealedTo)
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

// setJobPlanSealKeyEnv overwrites the Plan seal key environment variable on
// job's main container, failing the test if the fixture carries none to
// overwrite.
func setJobPlanSealKeyEnv(t *testing.T, job *batchv1.Job, key planseal.PublicKey) {
	t.Helper()
	for _, containers := range [][]corev1.Container{job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers} {
		for index := range containers {
			for envIndex := range containers[index].Env {
				if containers[index].Env[envIndex].Name == runner.EnvPlanSealPublicKey {
					containers[index].Env[envIndex].Value = key.Encode()
					return
				}
			}
		}
	}
	t.Fatal("test fixture Job carries no seal key to overwrite")
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

// TestHarvestRefusesAPlanSealedForAnotherOperation reproduces the review's
// second finding directly: a NaCl sealed box carries no associated data, so a
// plan validly sealed to the manager's real key for one operation opens and
// digest-checks just as well when presented as another's. Only the envelope
// bound inside the plaintext -- checked after Open, against the operation
// being harvested -- can refuse it; the Job UID and owner-reference checks
// this fixture leaves untouched are not what is under test here.
func TestHarvestRefusesAPlanSealedForAnotherOperation(t *testing.T) {
	t.Parallel()

	const declaredRowValueB = "operation-b-row@example.com"
	planDocumentB := safetyPlanDocumentWithStatement(t, "observed-state",
		`INSERT INTO users (email) VALUES ('`+declaredRowValueB+`')`)
	// Sealed with the real key this reconciler holds (testSchemaSealKey), for
	// a different operation ID and Job name than the one it will be harvested
	// under -- a genuinely valid seal, swapped into the wrong harvest.
	sealedForB, err := planseal.SealPlan(
		planDocumentB,
		planseal.Envelope{OperationID: "operation-b", JobName: "plan-job-b"},
		testSchemaSealKey.PublicKey(),
	)
	if err != nil {
		t.Fatalf("SealPlan() error = %v", err)
	}

	policyBytes := "policy"
	policyDigest := fingerprint.DigestBytes([]byte(policyBytes))
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
	// operation-a is what this harvest is actually for; the sealed payload
	// above names operation-b.
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:                    operatorv1alpha1.OperationPlan,
		ID:                      "operation-a",
		JobName:                 "plan-job-a",
		JobUID:                  "job-uid",
		StartedAt:               metav1.Now(),
		Attempt:                 1,
		PlanSealPublicKeyDigest: planSealPublicKeyDigest(testSchemaSealKey.PublicKey()),
	}
	bindActiveInput(t, schema)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		Stdout:               sealedForB,
		CoordinationDigest:   schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		// Self-consistent with the swapped-in Stdout, the way a genuine frame
		// for operation A's own plan would be -- proving the digest match
		// alone is not what refuses this.
		PlanContentDigest: fingerprint.DigestBytes(planDocumentB),
		PlanOutcome:       runner.PlanOutcomeChanges,
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
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	after := safetyGetSchema(t, api, schema)
	if after.Status.Plan != nil {
		t.Fatalf("status.plan = %#v, want nothing published from another operation's sealed plan", after.Status.Plan)
	}
	condition := findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if condition == nil || !strings.Contains(condition.Message, "envelope") {
		t.Fatalf("ReconciliationFailed = %#v, want a message naming the envelope mismatch", condition)
	}
	scan := safetyGetSchema(t, api, schema)
	statusBytes, err := json.Marshal(scan.Status)
	if err != nil {
		t.Fatalf("Marshal(status) error = %v", err)
	}
	if strings.Contains(string(statusBytes), declaredRowValueB) {
		t.Fatalf("status carries operation B's declared row value: %s", statusBytes)
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

// planClaimFixture stands a schema up with claim as its active Plan
// operation, complete enough for a real workload.Builder to build the Job the
// claim authorizes, and returns it with the verification policy ConfigMap the
// schema names. The claim's Job name is filled in from the claim.
func planClaimFixture(
	t *testing.T, claim *operatorv1alpha1.ActiveOperationStatus,
) (*operatorv1alpha1.PtahSchema, *corev1.ConfigMap) {
	t.Helper()

	policyBytes := "policy"
	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Spec.Target.URLFrom = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
	}
	schema.Spec.Target.CoordinationKey = testCoordinationKey
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
		Digest:                   testDigest,
		ArtifactType:             dataplane.SchemaArtifactType,
		Verified:                 true,
		VerificationPolicyUID:    testPolicyUID,
		VerificationPolicyDigest: fingerprint.DigestBytes([]byte(policyBytes)),
	}
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		CoordinationDigest: testCoordinationDigest,
		IdentityDigest:     testDigest,
		DriftReportDigest:  safetyOtherDigest,
	}
	schema.Status.ActiveOperation = claim
	bindActiveInput(t, schema)
	jobName, err := workload.NameFor(schema, *claim)
	if err != nil {
		t.Fatalf("NameFor() error = %v", err)
	}
	claim.JobName = jobName

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
	return schema, policyConfigMap
}

// TestANewLeaderResyncsTheSealKeyDigestBeforeCreatingTheJob reproduces the
// review's other finding: a crash between the status write that records
// DispatchStarted and PlanSealPublicKeyDigest and the Job Create that follows
// it leaves a claim naming the crashed process's key with no Job under it.
// DispatchStarted is already set, so nothing retires the claim, and the next
// leader creates the Job sealed to its own key. The digest the claim records
// has to move with it before that Create: admission checks the Job's key
// against the claim, and harvest checks the claim against the process
// harvesting, and neither would ever hold against the crashed process's
// digest.
//
// The crashed leader's snapshot was resolved from its own template, which
// forces the snapshot refresh a manager change forces, so the row runs the
// whole path a new leader takes: refresh, resolve, resync, create, commit.
func TestANewLeaderResyncsTheSealKeyDigestBeforeCreatingTheJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	crashedLeader, crashedLeaderKey := leaderBuilder(t)
	newLeader, newLeaderKey := leaderBuilder(t)

	claim := &operatorv1alpha1.ActiveOperationStatus{
		Type:                    operatorv1alpha1.OperationPlan,
		ID:                      "crash-before-create-operation",
		Attempt:                 1,
		StartedAt:               metav1.Now(),
		DispatchStarted:         true,
		PlanSealPublicKeyDigest: planSealPublicKeyDigest(crashedLeaderKey.PublicKey()),
	}
	schema, policyConfigMap := planClaimFixture(t, claim)
	// No Job: the crash landed between recording the boundary and creating it.
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfigMap)
	reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "new-leader-job-uid"}
	reconciler.Plans = planstore.Store{Client: api, Reader: api}

	// The crashed leader resolved the snapshot from the template it built,
	// sealed to its own key, before it recorded the boundary and died.
	crashedJob, err := crashedLeader.Build(schema.DeepCopy(), *claim, nil)
	if err != nil {
		t.Fatalf("Build(crashed leader) error = %v", err)
	}
	snapshot, err := podintent.Resolve(ctx, api, schema.Namespace, &crashedJob.Spec.Template, reconciler.AdmissionOptions)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	stored := safetyGetSchema(t, api, schema)
	stored.Status.ActiveOperation.AdmissionSnapshot = snapshot
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatalf("persist the crashed leader's snapshot: %v", err)
	}

	reconciler.Jobs = newLeader
	reconciler.SealKey = newLeaderKey
	job := reconcileUntilASchemaJobExists(t, reconciler, api, schema)

	after := safetyGetSchema(t, api, schema)
	operation := after.Status.ActiveOperation
	if operation == nil || operation.ID != claim.ID || operation.Attempt != claim.Attempt || operation.JobUID != job.UID {
		t.Fatalf("the claim did not commit to the Job the new leader created: %#v", operation)
	}
	if condition := findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); condition != nil {
		t.Fatalf("the new leader's dispatch was refused: %#v", condition)
	}
	liveKey, ok := jobPlanSealKeyEnv(job)
	if !ok {
		t.Fatal("the dispatched Job carries no seal key")
	}
	if liveKey == crashedLeaderKey.PublicKey().Encode() {
		t.Fatal("the Job was sealed to the crashed process's key, which no process holds")
	}
	if liveKey != newLeaderKey.PublicKey().Encode() {
		t.Fatalf("dispatched Job seal key = %q, want the new leader's own", liveKey)
	}
	if want := planSealPublicKeyDigest(newLeaderKey.PublicKey()); operation.PlanSealPublicKeyDigest != want {
		t.Fatalf("PlanSealPublicKeyDigest after dispatch = %q, want the new leader's %q", operation.PlanSealPublicKeyDigest, want)
	}
	// The check admission and adoption both run, against the claim as it
	// now stands and the Job as it was created.
	rebuilt, err := newLeader.Build(after.DeepCopy(), *operation, nil)
	if err != nil {
		t.Fatalf("Build(new leader) error = %v", err)
	}
	if err := workload.CarrySealedPlanKey(rebuilt, job, *operation); err != nil {
		t.Fatalf("the created Job does not hold against its own claim: %v", err)
	}
}

// jobPlanSealKeyEnv is the value of job's Plan seal key environment variable,
// wherever it carries one.
func jobPlanSealKeyEnv(job *batchv1.Job) (string, bool) {
	for _, containers := range [][]corev1.Container{job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers} {
		for _, container := range containers {
			for _, env := range container.Env {
				if env.Name == runner.EnvPlanSealPublicKey {
					return env.Value, true
				}
			}
		}
	}
	return "", false
}

// leaderBuilder is a real workload.Builder for one manager process: the
// identity every fixture in this package runs as, and a seal key generated
// for this process alone, the way every replica generates its own at
// startup (internal/planseal).
func leaderBuilder(t *testing.T) (workload.Builder, planseal.KeyPair) {
	t.Helper()
	key, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return workload.Builder{
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            testRunnerImage,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
		PlanSealPublicKey:      key.PublicKey(),
	}, key
}

// TestANewLeaderAdoptsAPlanJobTheOldLeaderDispatched reproduces the review's
// blocker through Reconcile: a leadership change mid-Plan -- a rolling
// upgrade of the default two-replica Deployment is enough -- hands the claim
// to a process whose seal key differs from the one that dispatched the Job.
// The old leader dispatches through the real path with a real builder, so
// the claim's snapshot, its key digest and the Job's own annotations are
// what a dispatch leaves behind. The new leader must keep that running Job
// rather than retire it as one whose intent changed.
//
// Its result is another matter: the payload is sealed to a key the new
// leader never held, so harvest retires the attempt under the claim's own
// key-mismatch reason, the way
// TestRestartedManagerRePlansRatherThanWaitingOnAnUnopenablePlan proves for
// a restart.
func TestANewLeaderAdoptsAPlanJobTheOldLeaderDispatched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	oldLeader, oldLeaderKey := leaderBuilder(t)
	newLeader, newLeaderKey := leaderBuilder(t)

	claim := &operatorv1alpha1.ActiveOperationStatus{
		Type:      operatorv1alpha1.OperationPlan,
		ID:        "leadership-change-plan-operation",
		Attempt:   1,
		StartedAt: metav1.Now(),
	}
	schema, policyConfigMap := planClaimFixture(t, claim)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfigMap)
	// The fake API server stamps no UID, and the controller refuses a created
	// Job without one.
	reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "old-leader-job-uid"}
	reconciler.Plans = planstore.Store{Client: api, Reader: api}
	reconciler.Jobs = oldLeader
	reconciler.SealKey = oldLeaderKey

	// The old leader dispatches through the real path: the snapshot, the
	// dispatch boundary and the key digest land on the claim the way a
	// dispatch records them, and the Job carries what its builder wrote.
	liveJob := reconcileUntilASchemaJobExists(t, reconciler, api, schema)
	dispatched := safetyGetSchema(t, api, schema)
	operation := dispatched.Status.ActiveOperation
	if operation == nil || operation.ID != claim.ID || operation.JobUID == "" ||
		operation.JobUID != liveJob.UID || operation.Attempt != claim.Attempt {
		t.Fatalf("the old leader's dispatch did not commit Job %q to its claim: %#v", liveJob.UID, operation)
	}
	if operation.PlanSealPublicKeyDigest != planSealPublicKeyDigest(oldLeaderKey.PublicKey()) {
		t.Fatalf("PlanSealPublicKeyDigest = %q, want the old leader's key", operation.PlanSealPublicKeyDigest)
	}
	if key, ok := jobPlanSealKeyEnv(liveJob); !ok || key != oldLeaderKey.PublicKey().Encode() {
		t.Fatalf("the dispatched Job carries seal key %q, want the old leader's", key)
	}
	// Adoption is checked only while the claim's inputs hold, and only a
	// rebuild that differs from the live Job can refuse it: both have to be
	// true here, or the passes below prove nothing.
	current, err := reconciler.operationInputFingerprint(dispatched, operatorv1alpha1.OperationPlan)
	if err != nil || current != operation.InputFingerprint {
		t.Fatalf("the claim's inputs moved after dispatch (%q, want %q, err %v), so adoption is never checked",
			current, operation.InputFingerprint, err)
	}
	rebuilt, err := newLeader.Build(dispatched.DeepCopy(), *operation, nil)
	if err != nil {
		t.Fatalf("Build(new leader) error = %v", err)
	}
	if validateJobIntent(liveJob, rebuilt, dispatched) == nil {
		t.Fatal("the two leaders build the same Job, so nothing below proves anything")
	}

	// Leadership moves. The new leader holds its own builder and its own
	// key, and nothing else about it differs.
	reconciler.Jobs = newLeader
	reconciler.SealKey = newLeaderKey
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() (adoption pass) error = %v", err)
	}
	adopted := safetyGetSchema(t, api, schema)
	if adopted.Status.ActiveOperation == nil ||
		adopted.Status.ActiveOperation.JobUID != liveJob.UID ||
		adopted.Status.ActiveOperation.Attempt != operation.Attempt {
		t.Fatalf("the new leader did not keep the old leader's running Job: %#v", adopted.Status.ActiveOperation)
	}
	if condition := findCondition(adopted.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); condition != nil {
		t.Fatalf("the new leader refused the old leader's running Plan Job: %#v", condition)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(ctx, jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].UID != liveJob.UID {
		t.Fatalf("Jobs after the adoption pass = %d, want the old leader's one Job left running", len(jobs.Items))
	}

	// The Job completes. Its payload is sealed to the key it was dispatched
	// under, fixed when the Job was built and unaffected by who leads when
	// the runner inside it finishes.
	planDocument := safetyPlanDocument(t, "observed-state")
	sealed, err := planseal.SealPlan(planDocument,
		planseal.Envelope{OperationID: operation.ID, JobName: operation.JobName}, oldLeaderKey.PublicKey())
	if err != nil {
		t.Fatalf("SealPlan() error = %v", err)
	}
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          operation.ID,
		ChildExitCode:        0,
		Stdout:               sealed,
		CoordinationDigest:   schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		PlanContentDigest:    fingerprint.DigestBytes(planDocument),
		PlanOutcome:          runner.PlanOutcomeChanges,
	})
	terminal := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(liveJob), terminal); err != nil {
		t.Fatal(err)
	}
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(ctx, terminalPodForJob(terminal)); err != nil {
		t.Fatal(err)
	}
	reconciler.Logs = staticLogs{content: frame}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() (harvest pass) error = %v", err)
	}
	final := safetyGetSchema(t, api, schema)
	if final.Status.Plan != nil {
		t.Fatalf("status.plan = %#v, want nothing published from a payload this process cannot open", final.Status.Plan)
	}
	condition := findCondition(final.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if condition == nil || !strings.Contains(condition.Message, "plan was sealed to a manager key this process does not hold") {
		t.Fatalf("harvest = %#v, want the claim's own key-mismatch retry", condition)
	}
	if final.Status.ActiveOperation == nil || final.Status.ActiveOperation.JobUID != "" ||
		final.Status.ActiveOperation.Attempt != operation.Attempt+1 {
		t.Fatalf("harvest did not retire the unopenable Job under a fresh attempt: %#v", final.Status.ActiveOperation)
	}
}

// terminalPodForJob is the Pod the Job controller would have run from job's
// own template: the template's metadata and spec, the defaults admission
// adds to every Pod, and an executor that terminated.
func terminalPodForJob(job *batchv1.Job) *corev1.Pod {
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	spec := job.Spec.Template.Spec.DeepCopy()
	spec.Priority = &priority
	spec.PreemptionPolicy = &preemption
	spec.Tolerations = append(spec.Tolerations,
		corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
	)
	labels := map[string]string{"job-name": job.Name}
	for key, value := range job.Spec.Template.Labels {
		labels[key] = value
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"),
			GenerateName: job.Name + "-", UID: "pod-" + job.UID,
			Labels:          labels,
			Annotations:     job.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
		},
		Spec: *spec,
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
}
