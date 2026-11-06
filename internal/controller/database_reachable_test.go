package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// statusWrites keeps the conditions of every status patch the controller sent,
// as the API server stored them, so a test can say which write carried what.
type statusWrites struct {
	mu     sync.Mutex
	writes [][]metav1.Condition
}

func (s *statusWrites) record(object client.Object) {
	var conditions []metav1.Condition
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		conditions = typed.Status.Conditions
	case *operatorv1alpha1.PtahMigration:
		conditions = typed.Status.Conditions
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, append([]metav1.Condition(nil), conditions...))
}

// recordingStatusWrites wraps api so each status patch is recorded.
func recordingStatusWrites(api client.WithWatch) (client.WithWatch, *statusWrites) {
	writes := &statusWrites{}
	return interceptor.NewClient(api, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, inner client.Client, subResource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption,
		) error {
			if err := inner.SubResource(subResource).Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			writes.record(object)
			return nil
		},
	}), writes
}

// assertSameWrite holds every write that carries the sentence on failureType to
// carrying DatabaseReachable beside it, and refuses to pass on no such write.
func (s *statusWrites) assertSameWrite(t *testing.T, failureType, sentence string, reason operatorv1alpha1.ConditionReason) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	found := 0
	for index, conditions := range s.writes {
		failed := meta.FindStatusCondition(conditions, failureType)
		if failed == nil || failed.Message != sentence {
			continue
		}
		found++
		reachable := meta.FindStatusCondition(conditions, operatorv1alpha1.ConditionDatabaseReachable)
		if reachable == nil || reachable.Status != metav1.ConditionFalse || reachable.Reason != string(reason) {
			t.Fatalf("write %d of %d carries the failure on %s and DatabaseReachable = %#v", index, len(s.writes), failureType, reachable)
		}
	}
	if found == 0 {
		t.Fatalf("none of %d status writes carries the sentence on %s, so nothing was compared", len(s.writes), failureType)
	}
}

func assertDatabaseUnreached(t *testing.T, conditions []metav1.Condition, reason operatorv1alpha1.ConditionReason, sentence string) {
	t.Helper()
	reachable := meta.FindStatusCondition(conditions, operatorv1alpha1.ConditionDatabaseReachable)
	if reachable == nil || reachable.Status != metav1.ConditionFalse || reachable.Reason != string(reason) || reachable.Message != sentence {
		t.Fatalf("DatabaseReachable = %#v, want False %s with %q", reachable, reason, sentence)
	}
}

func assertDatabaseReached(t *testing.T, conditions []metav1.Condition, message string) {
	t.Helper()
	reachable := meta.FindStatusCondition(conditions, operatorv1alpha1.ConditionDatabaseReachable)
	if reachable == nil || reachable.Status != metav1.ConditionTrue || reachable.Reason != string(operatorv1alpha1.ReasonObserved) ||
		reachable.Message != message {
		t.Fatalf("DatabaseReachable = %#v, want True Observed with %q", reachable, message)
	}
}

// previousReachability is what a resource may carry before a pass: the zero
// case is a target nothing ever opened.
func previousReachability() map[string]*metav1.Condition {
	dated := metav1.NewTime(time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))
	return map[string]*metav1.Condition{
		"no condition yet": nil,
		"a target that answered": {
			Type: operatorv1alpha1.ConditionDatabaseReachable, Status: metav1.ConditionTrue, ObservedGeneration: 1,
			Reason: string(operatorv1alpha1.ReasonObserved), Message: "Database schema was observed", LastTransitionTime: dated,
		},
		"a target that refused": {
			Type: operatorv1alpha1.ConditionDatabaseReachable, Status: metav1.ConditionFalse, ObservedGeneration: 1,
			Reason: string(operatorv1alpha1.ReasonDatabaseNotFound), Message: "database_not_found: an earlier pass", LastTransitionTime: dated,
		},
	}
}

func assertReachabilityKept(t *testing.T, conditions []metav1.Condition, previous *metav1.Condition) {
	t.Helper()
	reachable := meta.FindStatusCondition(conditions, operatorv1alpha1.ConditionDatabaseReachable)
	switch {
	case previous == nil && reachable != nil:
		t.Fatalf("a failure with no kind wrote DatabaseReachable = %#v", reachable)
	case previous != nil && (reachable == nil || !reachable.LastTransitionTime.Time.Equal(previous.LastTransitionTime.Time) ||
		reachable.Status != previous.Status || reachable.Reason != previous.Reason || reachable.Message != previous.Message ||
		reachable.ObservedGeneration != previous.ObservedGeneration):
		t.Fatalf("a failure with no kind moved DatabaseReachable to %#v, want %#v", reachable, previous)
	}
}

// failuresWithNoKind are what the controller must read as saying nothing about
// the target: codes the runner writes for other failures, and values that only
// resemble a kind or that a later Ptah could add.
func failuresWithNoKind() map[string]*runner.ResultError {
	return map[string]*runner.ResultError{
		"an exit status":                 {Code: "child_exit", Message: "ptah exited with code 2"},
		"a code that resembles a kind":   {Code: "Database_Unreachable", Message: "ptah exited with code 2"},
		"a kind outside the vocabulary":  {Code: "quota_exhausted", Message: frameText},
		"an output the operator refused": {Code: "invalid_plan_output", Message: "plan command did not complete successfully"},
	}
}

// targetOpeningSchema is a schema whose claim on operation finished, with the
// frame its runner wrote.
type targetOpeningSchema struct {
	name       string
	failedWith operatorv1alpha1.ConditionReason
	build      func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object)
	operation  runner.Operation
}

// targetOpeningSchemaPaths are the schema operations that open the target, in
// the two stages each runs in.
func targetOpeningSchemaPaths() []targetOpeningSchema {
	readOnly := func(operationType operatorv1alpha1.OperationType, id string) func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object) {
		return func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object) {
			t.Helper()
			schema := safetyApplySchema(t)
			schema.Status.Phase = operatorv1alpha1.PhaseObserving
			schema.Status.Plan = nil
			schema.Status.Source.Verified = true
			schema.Status.Source.Digest = testDigest
			schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
			schema.Status.Target.DriftReportDigest = testDigest
			schema.Status.ActiveOperation.Type = operationType
			schema.Status.ActiveOperation.ID = id
			schema.Status.ActiveOperation.JobName = id + "-job"
			schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
			bindActiveInput(t, schema)
			return schema, nil
		}
	}
	proof := func(operationType operatorv1alpha1.OperationType, id string) func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object) {
		return func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object) {
			t.Helper()
			schema := safetyPostApplyObserveSchema(t)
			schema.Status.PendingObservation.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
			schema.Status.PendingObservation.PlanRequired = operationType == operatorv1alpha1.OperationPlan
			schema.Status.ActiveOperation.Type = operationType
			schema.Status.ActiveOperation.ID = id
			schema.Status.ActiveOperation.JobName = id + "-job"
			bindActiveInput(t, schema)
			return schema, nil
		}
	}
	return []targetOpeningSchema{
		{
			name: "Observe", failedWith: operatorv1alpha1.ReasonOperationFailed, operation: runner.OperationObserve,
			build: readOnly(operatorv1alpha1.OperationObserve, "observe-operation"),
		},
		{
			name: "Plan", failedWith: operatorv1alpha1.ReasonOperationFailed, operation: runner.OperationPlan,
			build: readOnly(operatorv1alpha1.OperationPlan, "plan-operation"),
		},
		{
			name: "the observation after an uncertain Apply", failedWith: operatorv1alpha1.ReasonOperationFailed, operation: runner.OperationObserve,
			build: proof(operatorv1alpha1.OperationObserve, "proof-observe-operation"),
		},
		{
			name: "the Plan after an uncertain Apply", failedWith: operatorv1alpha1.ReasonOperationFailed, operation: runner.OperationPlan,
			build: proof(operatorv1alpha1.OperationPlan, "proof-plan-operation"),
		},
		{
			name: "Apply", failedWith: operatorv1alpha1.ReasonApplyOutcomeUnknown, operation: runner.OperationApply,
			build: func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object) {
				t.Helper()
				schema, plan, policy := dispatchedApplyWithPlan(t)
				return schema, []client.Object{plan, policy}
			},
		},
	}
}

// harvestSchemaFailure runs the pass that reads a failed frame for path, and
// returns the status it left and every status write it sent.
func harvestSchemaFailure(
	t *testing.T,
	path targetOpeningSchema,
	failure *runner.ResultError,
	previous *metav1.Condition,
) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchema, *statusWrites) {
	t.Helper()
	schema, others := path.build(t)
	if previous != nil {
		meta.SetStatusCondition(&schema.Status.Conditions, *previous.DeepCopy())
		stored := meta.FindStatusCondition(schema.Status.Conditions, previous.Type)
		stored.LastTransitionTime = previous.LastTransitionTime
	}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: path.operation, OperationID: schema.Status.ActiveOperation.ID,
		ChildExitCode: 2, CoordinationDigest: schema.Status.ActiveOperation.CoordinationDigest, Error: failure,
	})
	objects := append([]client.Object{schema, job, pod}, others...)
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, objects...)
	reconciler.Locks = targetlock.New(api, api, nil)
	recording, writes := recordingStatusWrites(api.(client.WithWatch))
	reconciler.Client = recording
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return schema, safetyGetSchema(t, api, schema), writes
}

// Every kind has a reason of its own: a reason two kinds share is a question an
// alert cannot ask, and a kind with none would leave the condition stale.
func TestEveryTargetFailureKindHasItsOwnReason(t *testing.T) {
	t.Parallel()
	want := map[dataplane.TargetFailureKind]string{
		dataplane.TargetDatabaseUnreachable:  "DatabaseUnreachable",
		dataplane.TargetAuthenticationFailed: "AuthenticationFailed",
		dataplane.TargetDatabaseNotFound:     "DatabaseNotFound",
		dataplane.TargetPermissionDenied:     "PermissionDenied",
		dataplane.TargetTLSFailed:            "TLSFailed",
	}
	for _, kind := range dataplane.TargetFailureKinds() {
		if reason := databaseReachableReasons[kind]; string(reason) != want[kind] || reason == "" {
			t.Errorf("kind %s has the DatabaseReachable reason %q, want %q", kind, reason, want[kind])
		}
	}
	if len(databaseReachableReasons) != len(dataplane.TargetFailureKinds()) {
		t.Fatalf("%d reasons for %d kinds", len(databaseReachableReasons), len(dataplane.TargetFailureKinds()))
	}
}

// Each kind reaches DatabaseReachable on each schema operation that opens the
// target, in the write that records the failure, and the conditions a consumer
// already matches keep the reason they had.
func TestASchemaOperationSetsDatabaseReachableToTheKind(t *testing.T) {
	t.Parallel()
	for _, path := range targetOpeningSchemaPaths() {
		for _, kind := range dataplane.TargetFailureKinds() {
			t.Run(path.name+"/"+string(kind), func(t *testing.T) {
				t.Parallel()
				schema, actual, writes := harvestSchemaFailure(t, path, &runner.ResultError{Code: string(kind), Message: frameText}, nil)
				sentence := targetfailure.Message(kind, schema.Spec.Target.URLFrom)
				reason := databaseReachableReasons[kind]
				assertDatabaseUnreached(t, actual.Status.Conditions, reason, sentence)
				writes.assertSameWrite(t, operatorv1alpha1.ConditionReconciliationFailed, sentence, reason)
				for _, conditionType := range []string{operatorv1alpha1.ConditionReconciliationFailed, operatorv1alpha1.ConditionReady} {
					condition := meta.FindStatusCondition(actual.Status.Conditions, conditionType)
					if condition == nil || condition.Reason != string(path.failedWith) {
						t.Fatalf("%s = %#v, want reason %s", conditionType, condition, path.failedWith)
					}
				}
			})
		}
	}
}

// A failure that names no kind says nothing about the target, whatever the
// condition said before it.
func TestASchemaFailureWithNoKindLeavesDatabaseReachable(t *testing.T) {
	t.Parallel()
	for _, path := range targetOpeningSchemaPaths() {
		for failureName, failure := range failuresWithNoKind() {
			for previousName, previous := range previousReachability() {
				t.Run(path.name+"/"+failureName+"/"+previousName, func(t *testing.T) {
					t.Parallel()
					_, actual, writes := harvestSchemaFailure(t, path, failure, previous)
					if len(writes.writes) == 0 {
						t.Fatal("the pass wrote no status, so it did not read the failure")
					}
					failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
					if failed == nil || failed.Status != metav1.ConditionTrue {
						t.Fatalf("ReconciliationFailed = %#v, want the failure recorded", failed)
					}
					assertReachabilityKept(t, actual.Status.Conditions, previous)
				})
			}
		}
	}
}

// A move from one kind to another changes the reason and not the time, which is
// what the documentation warns a reader dating an outage about.
func TestMovingBetweenKindsKeepsTheTransitionTime(t *testing.T) {
	t.Parallel()
	previous := previousReachability()["a target that refused"]
	path := targetOpeningSchemaPaths()[0]
	schema, actual, _ := harvestSchemaFailure(t, path,
		&runner.ResultError{Code: string(dataplane.TargetAuthenticationFailed), Message: frameText}, previous)
	assertDatabaseUnreached(t, actual.Status.Conditions, operatorv1alpha1.ReasonAuthenticationFailed,
		targetfailure.Message(dataplane.TargetAuthenticationFailed, schema.Spec.Target.URLFrom))
	reachable := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable)
	if !reachable.LastTransitionTime.Time.Equal(previous.LastTransitionTime.Time) {
		t.Fatalf("lastTransitionTime moved to %v, want %v kept", reachable.LastTransitionTime, previous.LastTransitionTime)
	}
}

// unreachableSchema marks a stored schema as a target an earlier pass could not
// open, dated before the fixed clock so a rewrite shows.
func unreachableSchema(schema *operatorv1alpha1.PtahSchema) {
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionDatabaseReachable, Status: metav1.ConditionFalse,
		Reason:  string(operatorv1alpha1.ReasonAuthenticationFailed),
		Message: targetfailure.Message(dataplane.TargetAuthenticationFailed, schema.Spec.Target.URLFrom),
	})
}

// The operation that opens the target after a failure sets True, whichever it
// is. A Plan retried after the database came back has no Observe before it, so
// without this the condition stays False for a whole spec.interval.
func TestTheNextOperationThatOpensTheTargetSetsDatabaseReachable(t *testing.T) {
	t.Parallel()

	const observed = "Database schema was observed"
	t.Run("a retried Plan that finds no changes", func(t *testing.T) {
		t.Parallel()
		schema, objects := convergedProofPlan(t, operatorv1alpha1.PendingObservationApplySucceeded)
		unreachableSchema(schema)
		reconciler, api := fakeReconciler(t, staticLogs{}, objects...)
		stored := safetyGetSchema(t, api, schema)
		if _, err := reconciler.consumeResult(context.Background(), stored, nil, runner.Result{
			Operation:            runner.OperationPlan,
			CoordinationDigest:   stored.Status.PendingObservation.CoordinationDigest,
			TargetIdentityDigest: stored.Status.PendingObservation.Plan.TargetIdentityDigest,
			PlanOutcome:          runner.PlanOutcomeNoChanges,
		}, nil, 0); err != nil {
			t.Fatal(err)
		}
		assertDatabaseReached(t, safetyGetSchema(t, api, schema).Status.Conditions, observed)
	})
	t.Run("a retried Plan that publishes a plan", func(t *testing.T) {
		t.Parallel()
		schema := schemaFixture()
		schema.Finalizers = []string{activeOperationFinalizer}
		schema.Status.Phase = operatorv1alpha1.PhasePlanning
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			ResolvedReference: "oci://registry.example/team/schema@" + testDigest, Digest: testDigest, Verified: true,
			ArtifactType: "application/vnd.stokaro.ptah.schema.v1", VerificationPolicyUID: testPolicyUID,
			VerificationPolicyDigest: fingerprint.DigestBytes([]byte("policy")),
		}
		schema.Status.Target = operatorv1alpha1.TargetStatus{
			CoordinationDigest: testCoordinationDigest, IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
		}
		schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationPlan, ID: "retried-plan-operation", JobName: "retried-plan-job", JobUID: "job-uid",
			StartedAt: metav1.Now(), Attempt: 2, PlanSealPublicKeyDigest: planSealPublicKeyDigest(testSchemaSealKey.PublicKey()),
		}
		bindActiveInput(t, schema)
		unreachableSchema(schema)
		document := safetyPlanDocument(t, "observed-state")
		frame := safetyRunnerFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: schema.Status.ActiveOperation.ID,
			Stdout:             safetySealPlan(t, schema.Status.ActiveOperation, document),
			CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
			PlanContentDigest: fingerprint.DigestBytes(document), PlanOutcome: runner.PlanOutcomeChanges,
		})
		job, pod := terminalWorkload(schema, batchv1.JobComplete)
		policy := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID,
			},
			Immutable: ptr(true),
			Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
		}
		reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policy)
		reconciler.Plans = planstore.Store{Client: api, Reader: api}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
		for pass := range 4 {
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
			if safetyGetSchema(t, api, schema).Status.Plan != nil {
				break
			}
		}
		actual := safetyGetSchema(t, api, schema)
		if actual.Status.Plan == nil {
			t.Fatalf("the retried Plan published nothing: %#v", actual.Status.Conditions)
		}
		assertDatabaseReached(t, actual.Status.Conditions, observed)
	})
	t.Run("a successful Observe", func(t *testing.T) {
		t.Parallel()
		schema := safetyPostApplyObserveSchema(t)
		unreachableSchema(schema)
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		stored := safetyGetSchema(t, api, schema)
		if _, err := reconciler.consumeResult(context.Background(), stored, nil, runner.Result{
			Operation:            runner.OperationObserve,
			CoordinationDigest:   stored.Status.PendingObservation.CoordinationDigest,
			TargetIdentityDigest: stored.Status.PendingObservation.Plan.TargetIdentityDigest,
			DriftReportDigest:    testDigest,
			ObservedDialect:      "postgresql",
		}, nil, 0); err != nil {
			t.Fatal(err)
		}
		assertDatabaseReached(t, safetyGetSchema(t, api, schema).Status.Conditions, observed)
	})
	t.Run("a successful Apply", func(t *testing.T) {
		t.Parallel()
		schema := safetyApplySchema(t)
		schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
		schema.Status.ActiveOperation.JobUID = "apply-job-uid"
		unreachableSchema(schema)
		operation := schema.Status.ActiveOperation.DeepCopy()
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		reconciler.Locks = targetlock.New(api, api, nil)
		stored := safetyGetSchema(t, api, schema)
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: operation.JobName, UID: "apply-job-uid", ResourceVersion: "1"}}
		job.Spec.TTLSecondsAfterFinished = ptr(int32(jobCleanupTTLSeconds))
		if _, err := reconciler.consumeResult(context.Background(), stored, job, runner.Result{
			Operation: runner.OperationApply, CoordinationDigest: operation.CoordinationDigest,
			TargetIdentityDigest: operation.TargetIdentityDigest,
		}, nil, 0); err != nil {
			t.Fatal(err)
		}
		assertDatabaseReached(t, safetyGetSchema(t, api, schema).Status.Conditions, "The Apply opened the database")
	})
}

// A Plan that finishes a proof under a binding a rotation has since retired has
// opened the target, and what it read is historical all the same: Unknown wins.
func TestAProofUnderARetiredBindingLeavesDatabaseReachableUnknown(t *testing.T) {
	t.Parallel()
	schema, objects := convergedProofPlan(t, operatorv1alpha1.PendingObservationOutcomeUnknown)
	schema.Status.ExecutionBinding.Epoch = "v1-55555555555555555555555555555555"
	schema.Status.ActiveOperation.ExecutionBindingID = schema.Status.ExecutionBinding.Epoch
	reconciler, api := fakeReconciler(t, staticLogs{}, objects...)
	stored := safetyGetSchema(t, api, schema)
	if _, err := reconciler.consumeResult(context.Background(), stored, nil, runner.Result{
		Operation:            runner.OperationPlan,
		CoordinationDigest:   stored.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: stored.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	}, nil, 0); err != nil {
		t.Fatal(err)
	}
	reachable := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable)
	if reachable == nil || reachable.Status != metav1.ConditionUnknown ||
		reachable.Reason != string(operatorv1alpha1.ReasonExecutionBindingChanged) {
		t.Fatalf("DatabaseReachable = %#v, want Unknown ExecutionBindingChanged", reachable)
	}
}

func setMigrationReachability(migration *operatorv1alpha1.PtahMigration, previous *metav1.Condition) {
	if previous == nil {
		return
	}
	migration.Status.Conditions = append(migration.Status.Conditions, *previous.DeepCopy())
}

// historyMigration is a migration whose History run finished with frame's
// failure, and the reconciler that reads it.
func historyMigration(
	t *testing.T,
	failure *runner.ResultError,
	previous *metav1.Condition,
) (*operatorv1alpha1.PtahMigration, *MigrationReconciler, client.WithWatch, *statusWrites) {
	t.Helper()
	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Finalizers = []string{migrationOperationFinalizer}
	setMigrationReachability(migration, previous)
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
		OperationID: operation.ID, ChildExitCode: 2, Error: failure,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap())
	recording, writes := recordingStatusWrites(api)
	reconciler.Client = recording
	return migration, reconciler, api, writes
}

// Each kind reaches DatabaseReachable on a migration's History run, in the write
// that names the failure on Progressing, whose reason stays what it was.
func TestAMigrationHistorySetsDatabaseReachableToTheKind(t *testing.T) {
	t.Parallel()
	for _, kind := range dataplane.TargetFailureKinds() {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			migration, reconciler, api, writes := historyMigration(t, &runner.ResultError{Code: string(kind), Message: frameText}, nil)
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			sentence := targetfailure.Message(kind, migration.Spec.Target.URLFrom)
			reason := databaseReachableReasons[kind]
			assertDatabaseUnreached(t, actual.Status.Conditions, reason, sentence)
			writes.assertSameWrite(t, operatorv1alpha1.ConditionMigrationProgressing, sentence, reason)
			progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != string(operatorv1alpha1.ReasonOperationFailed) {
				t.Fatalf("Progressing = %#v, want True OperationFailed", progressing)
			}
		})
	}
}

func TestAMigrationHistoryFailureWithNoKindLeavesDatabaseReachable(t *testing.T) {
	t.Parallel()
	for failureName, failure := range failuresWithNoKind() {
		for previousName, previous := range previousReachability() {
			t.Run(failureName+"/"+previousName, func(t *testing.T) {
				t.Parallel()
				migration, reconciler, api, writes := historyMigration(t, failure, previous)
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				if len(writes.writes) == 0 {
					t.Fatal("the pass wrote no status, so it did not read the failure")
				}
				actual := readMigration(t, api, migration)
				progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
				if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonOperationFailed) {
					t.Fatalf("Progressing = %#v, want the failure recorded", progressing)
				}
				assertReachabilityKept(t, actual.Status.Conditions, previous)
			})
		}
	}
}

// Each kind reaches DatabaseReachable on an Apply that stopped before it selected
// anything, in the write that records the run. Ready and Progressing keep
// OperationFailed.
func TestAMigrationApplySetsDatabaseReachableToTheKind(t *testing.T) {
	t.Parallel()
	for _, kind := range dataplane.TargetFailureKinds() {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			migration, reconciler, api, _ := notStartedApply(t, operatorv1alpha1.ApplyPolicyOnApproval,
				&runner.ResultError{Code: string(kind), Message: frameText})
			recording, writes := recordingStatusWrites(api)
			reconciler.Client = recording
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			sentence := targetfailure.Message(kind, migration.Spec.Target.URLFrom)
			reason := databaseReachableReasons[kind]
			assertDatabaseUnreached(t, actual.Status.Conditions, reason, sentence)
			writes.assertSameWrite(t, operatorv1alpha1.ConditionMigrationReady, sentence, reason)
			for _, conditionType := range []string{operatorv1alpha1.ConditionMigrationReady, operatorv1alpha1.ConditionMigrationProgressing} {
				condition := meta.FindStatusCondition(actual.Status.Conditions, conditionType)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != string(operatorv1alpha1.ReasonOperationFailed) {
					t.Fatalf("%s = %#v, want False OperationFailed", conditionType, condition)
				}
			}
		})
	}
}

// An Apply that stopped before selecting for any other reason ran nothing and
// did not necessarily open the target, so it says nothing either way.
func TestAMigrationApplyStoppedWithNoKindLeavesDatabaseReachable(t *testing.T) {
	t.Parallel()
	for failureName, failure := range failuresWithNoKind() {
		for previousName, previous := range previousReachability() {
			t.Run(failureName+"/"+previousName, func(t *testing.T) {
				t.Parallel()
				migration, reconciler, api, _ := notStartedApply(t, operatorv1alpha1.ApplyPolicyOnApproval, failure)
				stored := readMigration(t, api, migration)
				setMigrationReachability(stored, previous)
				if err := api.Status().Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				actual := readMigration(t, api, migration)
				if run := actual.Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed {
					t.Fatalf("LastRun = %#v, want the run recorded", run)
				}
				assertReachabilityKept(t, actual.Status.Conditions, previous)
			})
		}
	}
}

// The frame the runner does not write, a kind with no run report, still records
// its sentence on the run nobody accounted for. DatabaseReachable agrees with it.
func TestAMigrationApplyWithAKindAndNoRunSetsDatabaseReachable(t *testing.T) {
	t.Parallel()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 2, MutationStarted: true, Uncertain: true,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		Error:                &runner.ResultError{Code: string(dataplane.TargetTLSFailed), Message: frameText},
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	recording, writes := recordingStatusWrites(api)
	reconciler.Client = recording
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	sentence := targetfailure.Message(dataplane.TargetTLSFailed, migration.Spec.Target.URLFrom)
	if run := actual.Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown || run.Message != sentence {
		t.Fatalf("LastRun = %#v, want an unknown run that says %q", run, sentence)
	}
	assertDatabaseUnreached(t, actual.Status.Conditions, operatorv1alpha1.ReasonTLSFailed, sentence)
	writes.assertSameWrite(t, operatorv1alpha1.ConditionMigrationBlocked, sentence, operatorv1alpha1.ReasonTLSFailed)
}

func reportOf(outcome string, planned, applied []int64) *dataplane.MigrationRunReport {
	report := &dataplane.MigrationRunReport{
		ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
		Outcome: outcome, Planned: planned, Applied: applied,
	}
	return report
}

// An Apply whose report shows it selected something opened the target, whatever
// came of it, and sets True over an earlier refusal.
func TestAMigrationApplyThatRanSetsDatabaseReachable(t *testing.T) {
	t.Parallel()
	unreachable := previousReachability()["a target that refused"]
	for _, test := range []struct {
		name      string
		report    *dataplane.MigrationRunReport
		uncertain bool
	}{
		{name: "an applied run", report: reportOf(dataplane.MigrationOutcomeApplied, []int64{3}, []int64{3})},
		{name: "a partial run", report: reportOf(dataplane.MigrationOutcomePartial, []int64{3}, nil), uncertain: true},
		{name: "an unknown run", report: reportOf(dataplane.MigrationOutcomeUnknown, []int64{3}, nil), uncertain: true},
		{name: "a run that failed after it read the history", report: func() *dataplane.MigrationRunReport {
			report := reportOf(dataplane.MigrationOutcomeFailed, []int64{3}, nil)
			history := pendingMigrationHistory()
			report.Status = &history
			return report
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			setMigrationReachability(migration, unreachable)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				OperationID: operation.ID, MutationStarted: true, Uncertain: test.uncertain,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
				MigrationRun:         test.report,
			})
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap())
			holdMigrationApplyLease(t, reconciler, api, migration)
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.LastRun == nil {
				t.Fatal("the run was not recorded")
			}
			assertDatabaseReached(t, actual.Status.Conditions, "The Apply opened the database")
		})
	}
}

// A successful history read sets True over an earlier refusal.
func TestAMigrationHistoryReadSetsDatabaseReachable(t *testing.T) {
	t.Parallel()
	migration, _ := awaitingApprovalFixture(t)
	setMigrationReachability(migration, previousReachability()["a target that refused"])
	actual, _ := readMigrationHistory(t, migration, pendingMigrationHistory())
	assertDatabaseReached(t, actual.Status.Conditions, "The migration history was read")
}

// A run read from its termination summary cannot show it reached the database:
// the summary cannot tell a first migration that failed from a target that could
// not be opened. So it claims nothing, and the history read that follows decides.
func TestATerminationSummaryClaimsNothingAboutTheTarget(t *testing.T) {
	t.Parallel()
	run := summaryMigrationApply(runner.Summary{
		Migration: &runner.MigrationSummary{Outcome: dataplane.MigrationOutcomeFailed},
		ErrorCode: string(dataplane.TargetAuthenticationFailed),
	}).run
	if run == nil || run.openedTarget || run.targetFailure != nil {
		t.Fatalf("the summary's run = %+v, want no claim about the target", run)
	}
}

// DatabaseReachable is Unknown on a migration where a schema's is: no access to
// the target is even attempted for an unsupported engine, and a retired
// execution binding makes the last reading historical.
func TestAMigrationDatabaseReachableIsUnknownWithoutEvidence(t *testing.T) {
	t.Parallel()

	t.Run("an unsupported engine", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Spec.Target.Engine = "cockroachdb"
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		setMigrationReachability(migration, previousReachability()["a target that answered"])
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		reachable := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationDatabaseReachable)
		if reachable == nil || reachable.Status != metav1.ConditionUnknown || reachable.Reason != string(operatorv1alpha1.ReasonUnsupportedEngine) {
			t.Fatalf("DatabaseReachable = %#v, want Unknown UnsupportedEngine", reachable)
		}
	})
	t.Run("another blocked reason says nothing", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
		stored := readMigration(t, api, migration)
		if _, err := reconciler.migrationBlocked(context.Background(), stored, operatorv1alpha1.ReasonApplyOutcomeUnknown, "a run nobody accounted for"); err != nil {
			t.Fatal(err)
		}
		if reachable := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationDatabaseReachable); reachable != nil {
			t.Fatalf("DatabaseReachable = %#v, want none", reachable)
		}
	})
	for name, claimed := range map[string]bool{"a retired binding with no claim": false, "a retired binding with a read-only claim": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
			migration.Status.Artifact = resolvedMigrationArtifact()
			setMigrationReachability(migration, previousReachability()["a target that answered"])
			if claimed {
				migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
				migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			reachable := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationDatabaseReachable)
			if reachable == nil || reachable.Status != metav1.ConditionUnknown || reachable.Reason != string(operatorv1alpha1.ReasonExecutionBindingChanged) {
				t.Fatalf("DatabaseReachable = %#v, want Unknown ExecutionBindingChanged", reachable)
			}
		})
	}
	t.Run("the first binding retires nothing", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Status.ExecutionBinding = nil
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual := readMigration(t, api, migration)
		if actual.Status.ExecutionBinding == nil {
			t.Fatal("the pass published no binding, so it did not reach the write")
		}
		if reachable := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationDatabaseReachable); reachable != nil {
			t.Fatalf("DatabaseReachable = %#v, want none before any operation ran", reachable)
		}
	})
}
