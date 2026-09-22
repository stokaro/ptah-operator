package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// One migration, from nothing to InSync, measured.
//
// The operator documents an alert on an apply whose outcome nobody
// established, and the migration family used to report no apply outcome at
// all: a dispatched run that ended Unknown left `applies=[] failures=[]` and
// one successful reconciliation, which reads exactly like a cluster where
// nothing went wrong.
//
// This is the whole-lifecycle counterpart: every operation the family performs
// contributes its duration under its own name, and the plan is counted once.
func TestMigrationLifecycleReportsEveryStageItRan(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	harness := newMigrationLifecycle(t, migration)
	// The execution binding is published by the constructor's settle, before
	// there is an observer to report it to. Everything measured here happens
	// after.
	observed := &telemetryObservation{}
	harness.reconciler.Telemetry = observed

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0,
		ResolvedDigest:    testDigest,
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 0,
		ObservedArtifactType: dataplane.MigrationArtifactType, ResolvedDigest: testDigest,
	})
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationHistory: &dataplane.MigrationStatusReport{
			ContractVersion: dataplane.SupportedMigrationStatusContract,
			CurrentVersion:  2, TotalMigrations: 2, HasPendingChanges: true,
			Migrations: []dataplane.MigrationRecord{
				{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
				{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
			},
		},
	})
	harness.claimed(operatorv1alpha1.MigrationOperationApply)
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationApply, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up", Outcome: dataplane.MigrationOutcomeApplied,
			Planned: []int64{3}, Applied: []int64{3},
		},
	})
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationHistory: &dataplane.MigrationStatusReport{
			ContractVersion: dataplane.SupportedMigrationStatusContract,
			CurrentVersion:  3, TotalMigrations: 2,
			Migrations: []dataplane.MigrationRecord{
				{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
				{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
			},
		},
	})
	harness.phase(operatorv1alpha1.MigrationPhaseInSync)

	// History is named, not folded into an operation the schema family has.
	wantOperations := []telemetry.Operation{
		telemetry.OperationResolve,
		telemetry.OperationVerify,
		telemetry.OperationHistory,
		telemetry.OperationApply,
		telemetry.OperationHistory,
	}
	if !reflect.DeepEqual(observed.operationLabels, wantOperations) {
		t.Fatalf("operation labels = %#v, want %#v", observed.operationLabels, wantOperations)
	}
	for index, outcome := range observed.operations {
		if outcome != telemetry.OperationSucceeded {
			t.Fatalf("operation %d outcome = %q, want success", index, outcome)
		}
	}
	// One sample per operation. The harness clock does not advance, so the
	// elapsed time is zero here on purpose; that it is the claim's elapsed
	// time rather than zero by accident is measured in the table below, where
	// the claim starts a minute before the clock reads.
	if len(observed.durations) != len(wantOperations) {
		t.Fatalf("duration samples = %d, want one per operation (%d)",
			len(observed.durations), len(wantOperations))
	}
	for index, duration := range observed.durations {
		if duration < 0 {
			t.Fatalf("operation %d duration = %s, want a non-negative elapsed time", index, duration)
		}
	}
	wantApplies := []telemetry.ApplyOutcome{telemetry.ApplyStarted, telemetry.ApplyCompleted}
	if !reflect.DeepEqual(observed.applies, wantApplies) {
		t.Fatalf("apply observations = %#v, want %#v", observed.applies, wantApplies)
	}
	wantPlans := []telemetry.PlanImpact{telemetry.PlanImpactUnknown}
	if !reflect.DeepEqual(observed.plans, wantPlans) {
		t.Fatalf("plan observations = %#v, want one plan of unexamined impact", observed.plans)
	}
	if len(observed.approvals) != 0 {
		t.Fatalf("approval observations = %#v, want none under an Always policy", observed.approvals)
	}
	if len(observed.failures) != 0 {
		t.Fatalf("failure observations = %#v, want none from a lifecycle that succeeded", observed.failures)
	}
	if len(observed.drifts) != 0 {
		t.Fatalf("drift observations = %#v, want none: a migration reads its own history", observed.drifts)
	}
	wantFamilies := []telemetry.ResourceFamily{telemetry.FamilyMigration}
	if !reflect.DeepEqual(observed.observedFamilies(), wantFamilies) {
		t.Fatalf("families = %#v, want every observation labeled migration", observed.observedFamilies())
	}
}

// The documented alert watches `applies_total{outcome="uncertain"}`, and both
// unretryable outcomes have to reach it. Partial and Unknown differ only in
// whether the run could name what it committed; neither may be replayed, and
// an operator who is not told about either finds out from the database.
func TestMigrationApplyOutcomesReachTheDocumentedAlert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		report        *dataplane.MigrationRunReport
		wantApplies   []telemetry.ApplyOutcome
		wantFailures  []telemetry.FailureCategory
		wantOperation telemetry.OperationOutcome
		wantStages    []telemetry.FailureStage
	}{
		{
			name: "the database recorded every planned migration",
			report: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up", Outcome: dataplane.MigrationOutcomeApplied,
				Planned: []int64{3}, Applied: []int64{3},
			},
			wantApplies:   []telemetry.ApplyOutcome{telemetry.ApplyCompleted},
			wantOperation: telemetry.OperationSucceeded,
		},
		{
			name: "a migration failed and committed nothing",
			report: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up", Outcome: dataplane.MigrationOutcomeFailed,
				Planned: []int64{3},
			},
			// A run that said what it did is not uncertain. It is an operation
			// failure, and the history read that follows settles it.
			wantApplies:   nil,
			wantFailures:  []telemetry.FailureCategory{telemetry.FailureOperation},
			wantStages:    []telemetry.FailureStage{telemetry.FailureStageApply},
			wantOperation: telemetry.OperationSucceeded,
		},
		{
			name: "a migration committed some of its statements",
			report: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up", Outcome: dataplane.MigrationOutcomePartial,
				Planned: []int64{3},
			},
			wantApplies:   []telemetry.ApplyOutcome{telemetry.ApplyUncertain},
			wantFailures:  []telemetry.FailureCategory{telemetry.FailureUncertain},
			wantStages:    []telemetry.FailureStage{telemetry.FailureStageApply},
			wantOperation: telemetry.OperationUncertain,
		},
		{
			name:          "the run left no readable account",
			report:        nil,
			wantApplies:   []telemetry.ApplyOutcome{telemetry.ApplyUncertain},
			wantFailures:  []telemetry.FailureCategory{telemetry.FailureUncertain},
			wantStages:    []telemetry.FailureStage{telemetry.FailureStageApply},
			wantOperation: telemetry.OperationUncertain,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			result := runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
				MigrationRun:         test.report,
			}
			var logs []byte
			if test.report == nil {
				result.Uncertain = true
				result.MutationStarted = true
			} else {
				logs = migrationFrame(t, result)
			}
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
			)
			holdMigrationApplyLease(t, reconciler, api, migration)
			observed := &telemetryObservation{}
			reconciler.Telemetry = observed

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if !reflect.DeepEqual(observed.applies, test.wantApplies) {
				t.Fatalf("apply observations = %#v, want %#v", observed.applies, test.wantApplies)
			}
			if !reflect.DeepEqual(observed.failures, test.wantFailures) {
				t.Fatalf("failure observations = %#v, want %#v", observed.failures, test.wantFailures)
			}
			if !reflect.DeepEqual(observed.failureStages, test.wantStages) {
				t.Fatalf("failure stages = %#v, want %#v", observed.failureStages, test.wantStages)
			}
			wantOperations := []telemetry.OperationOutcome{test.wantOperation}
			if !reflect.DeepEqual(observed.operations, wantOperations) {
				t.Fatalf("operation observations = %#v, want %#v", observed.operations, wantOperations)
			}
			wantLabels := []telemetry.Operation{telemetry.OperationApply}
			if !reflect.DeepEqual(observed.operationLabels, wantLabels) {
				t.Fatalf("operation labels = %#v, want %#v", observed.operationLabels, wantLabels)
			}
			// applyClaimFor starts the claim a minute before the fixed clock
			// reads, so the sample is the claim's elapsed time and not the
			// zero a helper that forgot to pass it would record.
			if len(observed.durations) != 1 || observed.durations[0] != time.Minute {
				t.Fatalf("duration samples = %#v, want the claim's one minute", observed.durations)
			}
		})
	}
}

// A resource waiting for a person is reconciled on its interval for as long as
// it waits. A counter incremented from the status a pass read would climb once
// per pass, and an operator counting published plans would be reading the
// reconciliation cadence instead.
func TestMigrationPlanAndApprovalCountersDoNotClimbOnRoutineRequeues(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	observed := &telemetryObservation{}
	reconciler.Telemetry = observed

	for pass := 0; pass < 4; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
		t.Fatalf("phase = %q, want the resource still waiting for its approval", actual.Status.Phase)
	}
	if len(observed.plans) != 0 {
		t.Fatalf("plan observations = %#v, want none: no plan was published by these passes", observed.plans)
	}
	if len(observed.applies) != 0 {
		t.Fatalf("apply observations = %#v, want none: nothing was dispatched", observed.applies)
	}
	if len(observed.approvals) > 1 {
		t.Fatalf("approval observations = %#v, want at most the one transition into waiting", observed.approvals)
	}
}

// The approval counters follow a person's decision, not the condition that
// records the requirement.
//
// The ApprovalRequired condition also goes False under an Always policy, where
// the requirement was waived and nobody approved anything, so counting that
// transition would report approvals on a resource no person ever looked at.
func TestMigrationApprovalCountersFollowTheDecision(t *testing.T) {
	t.Parallel()

	t.Run("a decision that authorized an Apply", func(t *testing.T) {
		t.Parallel()
		migration, plan := awaitingApprovalFixture(t)
		approval := migrationApprovalFor(migration, plan)
		reconciler, api := fakeMigrationReconciler(
			t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap(),
		)
		observed := &telemetryObservation{}
		reconciler.Telemetry = observed

		// The decision is consumed at the dispatch boundary, several passes in:
		// the claim, the database lock, the admission snapshot, then the Job.
		// Reconciling more times than that is what proves the counter does not
		// climb with the passes.
		key := types.NamespacedName{Namespace: approval.Namespace, Name: approval.Name}
		consumed := &operatorv1alpha1.PtahMigrationApproval{}
		for pass := 0; pass < 8; pass++ {
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() pass %d error = %v", pass, err)
			}
		}
		if err := api.Get(context.Background(), key, consumed); err != nil {
			t.Fatal(err)
		}
		if !meta.IsStatusConditionTrue(consumed.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
			t.Fatalf("the decision was not consumed, so this proof measured nothing: %#v",
				consumed.Status.Conditions)
		}
		want := []telemetry.ApprovalOutcome{telemetry.ApprovalAccepted}
		if !reflect.DeepEqual(observed.approvals, want) {
			t.Fatalf("approval observations = %#v, want %#v", observed.approvals, want)
		}
	})

	t.Run("a requirement nobody has answered yet", func(t *testing.T) {
		t.Parallel()
		// The resource reaches AwaitingApproval from a published plan, which is
		// the transition an operator counts to know how many decisions are owed.
		migration := migrationFixture()
		migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
		harness := newMigrationLifecycle(t, migration)
		observed := &telemetryObservation{}
		harness.reconciler.Telemetry = observed

		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		harness.answer(runner.Result{
			Operation: runner.OperationResolve, ChildExitCode: 0,
			ResolvedDigest:    testDigest,
			ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
			ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
		})
		harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		harness.answer(runner.Result{
			Operation: runner.OperationVerify, ChildExitCode: 0,
			ObservedArtifactType: dataplane.MigrationArtifactType, ResolvedDigest: testDigest,
		})
		harness.claimed(operatorv1alpha1.MigrationOperationHistory)
		harness.answer(runner.Result{
			Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
			CoordinationDigest:   harness.coordinationDigest(),
			TargetIdentityDigest: testDigest,
			MigrationHistory: &dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  2, TotalMigrations: 2, HasPendingChanges: true,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
				},
			},
		})
		harness.phase(operatorv1alpha1.MigrationPhaseAwaitingApproval)

		want := []telemetry.ApprovalOutcome{telemetry.ApprovalRequired}
		if !reflect.DeepEqual(observed.approvals, want) {
			t.Fatalf("approval observations = %#v, want %#v", observed.approvals, want)
		}
		if len(observed.plans) != 1 || observed.plans[0] != telemetry.PlanImpactUnknown {
			t.Fatalf("plan observations = %#v, want the one published plan", observed.plans)
		}
		if len(observed.applies) != 0 {
			t.Fatalf("apply observations = %#v, want none while the decision is owed", observed.applies)
		}
	})
}

// A plan that stops being current takes the decision owed for it with it.
//
// The requirement has to have been outstanding. A plan discarded under an
// Always policy, or before anyone was asked, costs nobody an approval, and
// counting one there would report decisions nobody had made.
func TestADiscardedMigrationPlanRetiresTheDecisionOwedForIt(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	// The condition the controller writes when it publishes a plan that needs
	// a decision. The fixture starts from the phase; this is the rest of what
	// AwaitingApproval means on a real resource.
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired,
		metav1.ConditionTrue, operatorv1alpha1.ReasonAwaitingApproval, "1 planned migration needs an approval")
	// Somebody else advanced the database between planning and execution.
	migration.Status.History.Fingerprint = "sha256:" + strings.Repeat("c", 64)
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap(),
	)
	observed := &telemetryObservation{}
	reconciler.Telemetry = observed

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if actual := readMigration(t, api, migration); actual.Status.Plan != nil {
		t.Fatal("a plan the evidence no longer supports was retained, so this proof measured nothing")
	}
	want := []telemetry.ApprovalOutcome{telemetry.ApprovalStale}
	if !reflect.DeepEqual(observed.approvals, want) {
		t.Fatalf("approval observations = %#v, want %#v", observed.approvals, want)
	}
}

// A read-only operation that has to be retried is an operation failure at its
// own stage. History is the stage a schema does not have, and mapping the
// migration enum through the schema one reported it as the controller's own
// failure -- which is where an operator looks for a bug in the operator.
func TestARetriedMigrationOperationIsNamedAtItsOwnStage(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Finalizers = []string{migrationOperationFinalizer}
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{}, migration, job, pod, verificationPolicyConfigMap(),
	)
	observed := &telemetryObservation{}
	reconciler.Telemetry = observed

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != 2 {
		t.Fatalf("the operation was not retried, so this proof measured nothing: %#v",
			actual.Status.ActiveOperation)
	}
	wantStages := []telemetry.FailureStage{telemetry.FailureStageHistory}
	if !reflect.DeepEqual(observed.failureStages, wantStages) {
		t.Fatalf("failure stages = %#v, want %#v", observed.failureStages, wantStages)
	}
	wantFailures := []telemetry.FailureCategory{telemetry.FailureOperation}
	if !reflect.DeepEqual(observed.failures, wantFailures) {
		t.Fatalf("failure observations = %#v, want %#v", observed.failures, wantFailures)
	}
	// A retried claim has not finished, so it contributes no duration.
	if len(observed.operations) != 0 {
		t.Fatalf("operation observations = %#v, want none from a claim still in flight", observed.operations)
	}
}

// Suspension and a changed input both retire a claim, and they are not the
// same thing. A suspended resource was stopped by a person; a claim whose
// inputs moved was stale. Reporting both as one outcome loses the distinction
// an operator uses to tell an intervention from a race.
func TestARetiredMigrationClaimSaysWhyItWasRetired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		suspend      bool
		wantOutcome  telemetry.OperationOutcome
		wantFailures []telemetry.FailureCategory
		wantStages   []telemetry.FailureStage
	}{
		{
			name:        "a person stopped the resource",
			suspend:     true,
			wantOutcome: telemetry.OperationCanceled,
		},
		{
			name:         "the inputs moved while the Job ran",
			wantOutcome:  telemetry.OperationStale,
			wantFailures: []telemetry.FailureCategory{telemetry.FailureStaleInput},
			wantStages:   []telemetry.FailureStage{telemetry.FailureStageHistory},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			if test.suspend {
				migration.Spec.Suspend = true
			} else {
				// The fingerprint the claim was decided from no longer matches
				// what the resource now says.
				operation.InputFingerprint = "sha256:" + strings.Repeat("d", 64)
			}
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{}, migration, job, pod, verificationPolicyConfigMap(),
			)
			observed := &telemetryObservation{}
			reconciler.Telemetry = observed

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if actual := readMigration(t, api, migration); actual.Status.ActiveOperation != nil {
				t.Fatalf("the claim was kept, so this proof measured nothing: %#v",
					actual.Status.ActiveOperation)
			}
			wantOperations := []telemetry.OperationOutcome{test.wantOutcome}
			if !reflect.DeepEqual(observed.operations, wantOperations) {
				t.Fatalf("operation observations = %#v, want %#v", observed.operations, wantOperations)
			}
			wantLabels := []telemetry.Operation{telemetry.OperationHistory}
			if !reflect.DeepEqual(observed.operationLabels, wantLabels) {
				t.Fatalf("operation labels = %#v, want %#v", observed.operationLabels, wantLabels)
			}
			if !reflect.DeepEqual(observed.failures, test.wantFailures) {
				t.Fatalf("failure observations = %#v, want %#v", observed.failures, test.wantFailures)
			}
			if !reflect.DeepEqual(observed.failureStages, test.wantStages) {
				t.Fatalf("failure stages = %#v, want %#v", observed.failureStages, test.wantStages)
			}
		})
	}
}
