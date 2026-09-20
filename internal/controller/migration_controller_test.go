package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestMigrationReconcilerClaimsResolveFirst(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ExecutionBinding == nil {
		t.Fatal("the first reconciliation published no execution binding")
	}
	// The binding is its own durable boundary, so the claim lands next.
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual = readMigration(t, api, migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("active operation = %#v, want a Resolve claim", operation)
	}
	if operation.JobName == "" || !strings.HasPrefix(operation.JobName, "ptah-m-resolve-") {
		t.Fatalf("claimed Job name = %q", operation.JobName)
	}
	if operation.Source != nil || operation.Target != nil {
		t.Fatal("a Resolve claim carries neither a resolved artifact nor a database target")
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseResolving {
		t.Fatalf("phase = %q, want Resolving", actual.Status.Phase)
	}
	if !contains(actual.Finalizers, migrationOperationFinalizer) {
		t.Fatal("a claim in flight did not take the operation finalizer")
	}
}

func TestMigrationResolveResultAdvancesToVerification(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: operation.ID, ChildExitCode: 0,
		ResolvedDigest:    testDigest,
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json",
		ResolvedSize:      321,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the consumed claim was retained: %#v", actual.Status.ActiveOperation)
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseVerifying {
		t.Fatalf("phase = %q, want Verifying", actual.Status.Phase)
	}
	if actual.Status.Artifact == nil || actual.Status.Artifact.Digest != testDigest {
		t.Fatalf("artifact binding = %#v", actual.Status.Artifact)
	}
	harvested := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Spec.TTLSecondsAfterFinished == nil || *harvested.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatal("the consumed Job was not scheduled for cleanup")
	}
}

func TestMigrationVerifyRefusesAnArtifactOfAnotherType(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifying
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationVerify)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationVerify,
		OperationID: operation.ID, ChildExitCode: 0,
		ObservedArtifactType: dataplane.SchemaArtifactType,
		ResolvedDigest:       testDigest,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase == operatorv1alpha1.MigrationPhaseReading {
		t.Fatal("a schema artifact was accepted as a migration artifact")
	}
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != 2 {
		t.Fatalf("active operation after the refusal = %#v", actual.Status.ActiveOperation)
	}
}

func TestMigrationHistoryResultClassifiesWhatTheDatabaseSaid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		report     dataplane.MigrationStatusReport
		wantPhase  operatorv1alpha1.MigrationPhase
		wantReady  metav1.ConditionStatus
		wantReason operatorv1alpha1.ConditionReason
		wantPlan   bool
		// wantOutOfOrder is the exact set the history publishes, because which
		// migration arrived late is what a person decides from.
		wantOutOfOrder []int64
		// wantProgressing is what a dashboard reads to decide whether the
		// resource is still moving. Every row states it, because the defect
		// this covers was a branch that simply left the condition alone
		// (stokaro/ptah-operator#94).
		wantProgressing       metav1.ConditionStatus
		wantProgressingReason operatorv1alpha1.ConditionReason
		// wantDirection is the clause of the Blocked message that tells a
		// person what to do next. Section 6 of the epic asks an error for a
		// safe explanation and a direction for recovery, and three of these
		// histories answered only the first half. Empty means the history
		// does not block, so there is nothing to recover from.
		wantDirection string
	}{
		{
			name: "every migration is applied",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 2,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:             operatorv1alpha1.MigrationPhaseInSync,
			wantReady:             metav1.ConditionTrue,
			wantReason:            operatorv1alpha1.ReasonHistoryMatched,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonHistoryMatched,
		},
		{
			name: "the artifact carries migrations the database does not",
			report: dataplane.MigrationStatusReport{
				ContractVersion:   dataplane.SupportedMigrationStatusContract,
				CurrentVersion:    2,
				TotalMigrations:   3,
				HasPendingChanges: true,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
				},
			},
			wantPhase:             operatorv1alpha1.MigrationPhaseAwaitingApproval,
			wantReady:             metav1.ConditionFalse,
			wantReason:            operatorv1alpha1.ReasonAwaitingApproval,
			wantPlan:              true,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonAwaitingApproval,
		},
		{
			name: "an interrupted run left a dirty row",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 2,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateDirty},
				},
				DirtyRevision: &dataplane.MigrationDirty{Version: 3, Applied: 2, Total: 5},
			},
			wantPhase:             operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:             metav1.ConditionFalse,
			wantReason:            operatorv1alpha1.ReasonHistoryDirty,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonHistoryDirty,
			wantDirection:         "a person has to decide what the interrupted run did",
		},
		{
			name: "an applied migration was modified after it ran",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 2,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateModified},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:             operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:             metav1.ConditionFalse,
			wantReason:            operatorv1alpha1.ReasonHistoryModified,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonHistoryModified,
			wantDirection:         "restore the files the database recorded",
		},
		{
			// Ptah executes in linear order and refuses the whole run while a
			// pending migration sorts below the current version, so a plan for
			// this history would be a sequence nobody could execute.
			name: "a migration arrived below the version the database applied",
			report: dataplane.MigrationStatusReport{
				ContractVersion:   dataplane.SupportedMigrationStatusContract,
				CurrentVersion:    3,
				TotalMigrations:   3,
				HasPendingChanges: true,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateOutOfOrder},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:             operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:             metav1.ConditionFalse,
			wantReason:            operatorv1alpha1.ReasonHistoryOutOfOrder,
			wantOutOfOrder:        []int64{2},
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonHistoryOutOfOrder,
			wantDirection:         "renumber them above it",
		},
		{
			// Nothing here is pending, modified or dirty, because every one of
			// those is a state of a migration the artifact carries. Version 3
			// is not in the artifact at all.
			name: "the database ran a migration the artifact does not carry",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 1,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:             operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:             metav1.ConditionFalse,
			wantReason:            operatorv1alpha1.ReasonHistoryAhead,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonHistoryAhead,
			wantDirection:         "publish an artifact that carries",
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
			report := test.report
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q (conditions %#v)", actual.Status.Phase, test.wantPhase, actual.Status.Conditions)
			}
			if actual.Status.History == nil {
				t.Fatal("the history was not published")
			}
			if actual.Status.History.CurrentVersion != test.report.CurrentVersion {
				t.Fatalf("current version = %d, want %d", actual.Status.History.CurrentVersion, test.report.CurrentVersion)
			}
			if !slices.Equal(actual.Status.History.OutOfOrderVersions, test.wantOutOfOrder) {
				t.Fatalf("out-of-order versions = %v, want %v",
					actual.Status.History.OutOfOrderVersions, test.wantOutOfOrder)
			}
			ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady)
			if ready == nil || ready.Status != test.wantReady || ready.Reason != string(test.wantReason) {
				t.Fatalf("Ready condition = %#v, want %s/%s", ready, test.wantReady, test.wantReason)
			}
			blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			if test.wantDirection != "" && (blocked == nil || !strings.Contains(blocked.Message, test.wantDirection)) {
				t.Fatalf("Blocked message = %q, want it to say %q", blockedMessage(blocked), test.wantDirection)
			}
			progressing := meta.FindStatusCondition(
				actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Status != test.wantProgressing ||
				progressing.Reason != string(test.wantProgressingReason) {
				t.Fatalf("Progressing condition = %#v, want %s/%s",
					progressing, test.wantProgressing, test.wantProgressingReason)
			}
			if actual.Status.NextReconciliationTime == nil {
				t.Fatal("a settled migration scheduled no next reconciliation")
			}
			if !test.wantPlan {
				if actual.Status.Plan != nil {
					t.Fatalf("a history with nothing to plan published %#v", actual.Status.Plan)
				}
				return
			}
			if actual.Status.Plan == nil {
				t.Fatal("a pending sequence published no plan")
			}
			plans := &operatorv1alpha1.PtahMigrationPlanList{}
			if err := api.List(context.Background(), plans); err != nil {
				t.Fatal(err)
			}
			if len(plans.Items) != 1 {
				t.Fatalf("published plans = %d, want one", len(plans.Items))
			}
			plan := plans.Items[0]
			if plan.Name != actual.Status.Plan.Name || plan.Spec.MigrationRef.UID != migration.UID {
				t.Fatalf("plan identity = %#v", plan.ObjectMeta)
			}
			if plan.Spec.HistoryFingerprint != actual.Status.History.Fingerprint {
				t.Fatal("the plan does not name the history it was computed against")
			}
			if len(plan.Spec.Migrations) != 1 || plan.Spec.Migrations[0].Version != 3 {
				t.Fatalf("planned sequence = %#v", plan.Spec.Migrations)
			}
			if plan.Spec.TargetIdentityDigest != testDigest || plan.Spec.ArtifactDigest != testDigest {
				t.Fatalf("plan bindings = %#v", plan.Spec)
			}
		})
	}
}

// TestMigrationPlanningReportsWorkBeforeThePlanIsPublished covers the state
// between the two status writes a history result produces. The resource is
// Planning, it carries no plan yet, and the operator is about to publish one:
// the condition has to say that rather than keep naming the history read that
// already finished (stokaro/ptah-operator#94).
func TestMigrationPlanningReportsWorkBeforeThePlanIsPublished(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonOperationInProgress, "History operation is in progress")
	report := dataplane.MigrationStatusReport{
		ContractVersion:   dataplane.SupportedMigrationStatusContract,
		CurrentVersion:    2,
		TotalMigrations:   3,
		HasPendingChanges: true,
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
		},
	}

	reconciler := &MigrationReconciler{}
	if err := reconciler.recordMigrationHistory(migration, report, testDigest); err != nil {
		t.Fatalf("recordMigrationHistory() error = %v", err)
	}

	if migration.Status.Phase != operatorv1alpha1.MigrationPhasePlanning {
		t.Fatalf("phase = %q, want %q", migration.Status.Phase, operatorv1alpha1.MigrationPhasePlanning)
	}
	if migration.Status.Plan != nil {
		t.Fatalf("a plan was recorded before one was published: %#v", migration.Status.Plan)
	}
	progressing := meta.FindStatusCondition(
		migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		progressing.Reason != string(operatorv1alpha1.ReasonMigrationsPending) {
		t.Fatalf("Progressing condition = %#v, want True/%s",
			progressing, operatorv1alpha1.ReasonMigrationsPending)
	}
}

// TestMigrationApplyPolicySaysWhetherAnythingIsComing measures what a dashboard
// reads once a plan is published. Only one of the three policies means an Apply
// is next; under the other two the resource has stopped where a person has to
// act, and reporting progress there says the opposite of what Ready says on the
// same object (stokaro/ptah-operator#94).
func TestMigrationApplyPolicySaysWhetherAnythingIsComing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		policy                operatorv1alpha1.ApplyPolicy
		wantPhase             operatorv1alpha1.MigrationPhase
		wantProgressing       metav1.ConditionStatus
		wantProgressingReason operatorv1alpha1.ConditionReason
	}{
		{
			name:                  "an approval a person has to write",
			policy:                operatorv1alpha1.ApplyPolicyOnApproval,
			wantPhase:             operatorv1alpha1.MigrationPhaseAwaitingApproval,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonAwaitingApproval,
		},
		{
			name:                  "applying is disabled",
			policy:                operatorv1alpha1.ApplyPolicyNever,
			wantPhase:             operatorv1alpha1.MigrationPhasePlanning,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonApplyDisabled,
		},
		{
			name:                  "an apply is what happens next",
			policy:                operatorv1alpha1.ApplyPolicyAlways,
			wantPhase:             operatorv1alpha1.MigrationPhasePlanning,
			wantProgressing:       metav1.ConditionTrue,
			wantProgressingReason: operatorv1alpha1.ReasonApplyPending,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Policy.Apply = test.policy
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := dataplane.MigrationStatusReport{
				ContractVersion:   dataplane.SupportedMigrationStatusContract,
				CurrentVersion:    2,
				TotalMigrations:   3,
				HasPendingChanges: true,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
				},
			}
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q (conditions %#v)", actual.Status.Phase, test.wantPhase, actual.Status.Conditions)
			}
			if actual.Status.Plan == nil {
				t.Fatal("a pending sequence published no plan to decide about")
			}
			progressing := meta.FindStatusCondition(
				actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Status != test.wantProgressing ||
				progressing.Reason != string(test.wantProgressingReason) {
				t.Fatalf("Progressing condition = %#v, want %s/%s",
					progressing, test.wantProgressing, test.wantProgressingReason)
			}
		})
	}
}

func TestMigrationReconcilerRefusesAnUnsupportedEngine(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Target.Engine = "cockroachdb"
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatal("an unsupported engine still claimed an operation")
	}
}

func TestMigrationReconcilerSuspendsWithoutClaiming(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Suspend = true
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseSuspended || actual.Status.ActiveOperation != nil {
		t.Fatalf("status = %#v, want a suspended migration with no claim", actual.Status)
	}
}

func TestMigrationReconcilerRetiresAClaimUnderAChangedExecutionBinding(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Status.Artifact = resolvedMigrationArtifact()
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a claim authorized under a retired execution binding survived")
	}
	if actual.Status.ExecutionBinding == nil ||
		actual.Status.ExecutionBinding.ExecutorImage != "example.invalid/ptah@"+testDigest {
		t.Fatalf("execution binding = %#v", actual.Status.ExecutionBinding)
	}
}

func TestMigrationReconcilerDiscardsAClaimWhoseInputsChanged(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	operation.InputFingerprint = "sha256:" + strings.Repeat("b", 64)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a claim whose inputs changed was still dispatched")
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a stale claim created %d Jobs", len(jobs.Items))
	}
}

func TestMigrationDispatchPersistsTheSnapshotBeforeTheJob(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	operation.AdmissionSnapshot = nil
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
	reconciler.Jobs = workloadBuilderForMigrations()

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.AdmissionSnapshot == nil {
		t.Fatalf("dispatch did not persist the Pod admission snapshot: phase=%q operation=%#v conditions=%#v",
			actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("the Job was created before its snapshot was durable")
	}

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("dispatched Jobs = %d, want one", len(jobs.Items))
	}
	created := jobs.Items[0]
	if created.Name != actual.Status.ActiveOperation.JobName {
		t.Fatalf("Job name = %q, want the claimed %q", created.Name, actual.Status.ActiveOperation.JobName)
	}
	if created.Labels[workload.LabelMigration] != migration.Name ||
		created.Labels[workload.LabelComponent] != workload.ComponentMigrationOperation {
		t.Fatalf("Job labels = %#v", created.Labels)
	}
}

// fixedClock is the test clock the reconciler and its Lease share, so a Lease
// acquired in one reconciliation is still held in the next.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func migrationRequest(migration *operatorv1alpha1.PtahMigration) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}
}

func readMigration(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
	t.Helper()

	actual := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(migration), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

func migrationFrame(t *testing.T, result runner.Result) []byte {
	t.Helper()

	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// fakeMigrationReconciler builds the reconciler and the fake API server it
// reads and writes through. The client comes back as a WithWatch so a test can
// put an interceptor in front of the reconciler's reads.
func fakeMigrationReconciler(
	t *testing.T,
	logs PodLogReader,
	objects ...client.Object,
) (*MigrationReconciler, client.WithWatch) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		operatorv1alpha1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme,
		nodev1.AddToScheme, schedulingv1.AddToScheme, coordinationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "default", UID: "default-service-account-uid", ResourceVersion: "1",
	}})
	api := fake.NewClientBuilder().WithScheme(scheme).
		// A real API server stamps a UID on every object it accepts, and the
		// controller refuses a Job without one. The fake client does not, so a
		// Job it created would be refused by its own creator.
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context,
				writer client.WithWatch,
				object client.Object,
				options ...client.CreateOption,
			) error {
				if object.GetUID() == "" {
					object.SetUID(types.UID("created-" + object.GetName()))
				}
				// The controller-write guard re-derives a migration plan from
				// the migration's own STORED status and refuses one it cannot
				// reproduce. A fake client that accepted a plan built from a
				// status this process had not written yet would let every test
				// pass against a sequence a cluster refuses, which is exactly
				// what happened: the plan was created before the history that
				// justifies it was persisted, and only a live cluster said so.
				if plan, ok := object.(*operatorv1alpha1.PtahMigrationPlan); ok {
					stored := &operatorv1alpha1.PtahMigration{}
					key := client.ObjectKey{Namespace: plan.Namespace, Name: plan.Spec.MigrationRef.Name}
					if err := writer.Get(ctx, key, stored); err != nil {
						return fmt.Errorf("guard: read the migration a plan names: %w", err)
					}
					if stored.Status.Artifact == nil || stored.Status.History == nil ||
						stored.Status.ExecutionBinding == nil {
						return fmt.Errorf(
							"guard: the stored migration has no resolved artifact, history, and execution binding to plan from")
					}
				}
				return writer.Create(ctx, object, options...)
			},
		}).
		WithStatusSubresource(
			&operatorv1alpha1.PtahMigration{}, &operatorv1alpha1.PtahMigrationPlan{},
			&operatorv1alpha1.PtahMigrationApproval{}, &batchv1.Job{},
		).
		WithObjects(objects...).Build()
	clock := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	testClock := fixedClock{now: clock}
	reconciler := &MigrationReconciler{
		Client: api, APIReader: api, Scheme: scheme, Logs: logs, Jobs: fakeJobs{},
		LockNamespace:    "ptah-system",
		Clock:            testClock.Now,
		AdmissionOptions: podintent.DefaultOptions(),
	}
	reconciler.Locks = targetlock.New(api, api, testClock)
	return reconciler, api
}

func workloadBuilderForMigrations() workload.Builder {
	return workload.Builder{
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            "example.invalid/operator@" + testDigest,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
	}
}

func migrationFixture() *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "orders", UID: types.UID("migration-uid"), Generation: 1,
		},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "team-a/orders-primary",
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
				},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.example/team/migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
			Execution: operatorv1alpha1.ExecutionSpec{ActiveDeadlineSeconds: 900},
		},
	}
}

// verificationPolicyConfigMap is the immutable policy object a Verify claim is
// fingerprinted against. A policy that changes after the claim is what makes
// the claim's result stale.
func verificationPolicyConfigMap() *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "verification", UID: types.UID("verification-policy-uid"), ResourceVersion: "1",
		},
		Immutable: &immutable,
		Data:      map[string]string{"policy.yaml": "requireDigestPin: true"},
	}
}

func migrationExecutionBinding() *operatorv1alpha1.ExecutionBindingStatus {
	return &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  testExecutionBindingID,
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
		PtahVersion:            "v0.3.0",
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            "example.invalid/operator@" + testDigest,
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
}

func resolvedMigrationArtifact() *operatorv1alpha1.OCIArtifactAccessBinding {
	return &operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		Digest:            testDigest,
	}
}

// migrationClaim persists a claim the way the controller would, including the
// input fingerprint the reconciler recomputes before it dispatches.
func migrationClaim(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	operationType operatorv1alpha1.MigrationOperationType,
) *operatorv1alpha1.MigrationOperationStatus {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reconciler := &MigrationReconciler{
		Jobs:      fakeJobs{},
		APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(verificationPolicyConfigMap()).Build(),
	}
	fingerprintValue, err := reconciler.migrationInputFingerprint(context.Background(), migration, operationType)
	if err != nil {
		t.Fatal(err)
	}
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:               operationType,
		ID:                 testDigest,
		InputFingerprint:   fingerprintValue,
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
		StartedAt:          metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)),
		Attempt:            1,
	}
	if operationType != operatorv1alpha1.MigrationOperationResolve {
		operation.Source = migrationSourceBinding(migration)
	}
	if operationType == operatorv1alpha1.MigrationOperationHistory {
		coordinationDigest, digestErr := fingerprint.DatabaseCoordinationDigest(
			string(migration.Spec.Target.Engine), migration.Spec.Target.CoordinationKey,
		)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		operation.CoordinationDigest = coordinationDigest
		operation.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		}
	}
	name, err := (fakeJobs{}).NameForMigration(migration, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	migration.Status.ActiveOperation = operation
	migration.Status.ObservedGeneration = migration.Generation
	return operation
}

// terminalMigrationWorkload is the Job and Pod a consumed claim reads its
// result from, carrying the exact admission evidence the claim persisted.
func terminalMigrationWorkload(
	migration *operatorv1alpha1.PtahMigration,
	conditionType batchv1.JobConditionType,
) (*batchv1.Job, *corev1.Pod) {
	operation := migration.Status.ActiveOperation
	ensureMigrationAdmissionSnapshot(migration)
	annotations := map[string]string{
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
	}
	controller := true
	blockDeletion := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: operation.JobName, UID: "job-uid", Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration",
				Name: migration.Name, UID: migration.UID,
				Controller: &controller, BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec:   batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}}},
	}
	operation.JobUID = job.UID
	// A Pod the API server has finished with reports a terminal phase, and the
	// controller reads that phase to tell a run that is over from one that may
	// still be writing. A fixture without it is a workload only its name calls
	// terminal.
	podPhase := corev1.PodSucceeded
	if conditionType == batchv1.JobFailed {
		podPhase = corev1.PodFailed
	}
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: migration.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"),
		GenerateName: job.Name + "-", UID: "pod-uid",
		Labels: map[string]string{"job-name": job.Name}, Annotations: annotations,
		OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}, Spec: corev1.PodSpec{
		ServiceAccountName: "default", Priority: &priority, PreemptionPolicy: &preemption,
		Tolerations: []corev1.Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
			{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		},
	}, Status: corev1.PodStatus{Phase: podPhase, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}}}
	return job, pod
}

// A Pod whose init container ended it never reaches the runner, so there is no
// frame to read. Saying only that is saying what a crashed runner, an evicted
// node and a truncated log also say; the boundary that failed is the part a
// reader can act on.
func TestAMigrationJobThatFailedBeforeTheRunnerNamesTheBoundary(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
	// The fetch step refused the artifact, so the container that would have
	// spoken never ran.
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: "validate-source-authority", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: "fetch-migrations", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
	}
	pod.Status.ContainerStatuses = nil

	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, job, pod, verificationPolicyConfigMap())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, migration)
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil {
		t.Fatal("the failed attempt left no Progressing condition")
	}
	if !strings.Contains(progressing.Message, "fetch-migrations") {
		t.Fatalf("message = %q, want the step that failed", progressing.Message)
	}
	if strings.Contains(progressing.Message, "frame not found") {
		t.Fatalf("message = %q, still reports a missing frame rather than the boundary", progressing.Message)
	}
	// The container's own output is never carried: it holds registry
	// credentials, so the name and the exit code are all this says.
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != operation.Attempt+1 {
		t.Fatalf("active operation = %#v, want a fresh attempt", actual.Status.ActiveOperation)
	}
}

// A run nobody could read may have executed the migration that is pending now.
// One interval later the controller used to plan it again, which is the blind
// replay the versioned workflow exists to refuse, and which the run's own
// condition had already promised would not happen.
func TestAPendingMigrationIsNotReplayedAfterAnUnreadableRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		outcome   operatorv1alpha1.MigrationRunOutcome
		blocked   bool
		wantPhase operatorv1alpha1.MigrationPhase
	}{
		{
			name:      "a run whose evidence could not be read",
			outcome:   operatorv1alpha1.MigrationRunOutcomeUnknown,
			blocked:   true,
			wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
		},
		{
			name:      "a run that committed some of its statements",
			outcome:   operatorv1alpha1.MigrationRunOutcomePartial,
			blocked:   true,
			wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
		},
		{
			// The refusal is the latch. A resource somebody put right settles
			// through the history, and the next pending migration plans as
			// usual rather than inheriting a refusal nothing renewed.
			name:      "a run whose refusal was already cleared",
			outcome:   operatorv1alpha1.MigrationRunOutcomeUnknown,
			blocked:   false,
			wantPhase: operatorv1alpha1.MigrationPhaseAwaitingApproval,
		},
		{
			name:      "a run that failed and committed nothing",
			outcome:   operatorv1alpha1.MigrationRunOutcomeFailed,
			blocked:   true,
			wantPhase: operatorv1alpha1.MigrationPhaseAwaitingApproval,
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
			finished := metav1.NewTime(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC))
			migration.Status.LastRun = &operatorv1alpha1.MigrationRunStatus{
				Outcome: test.outcome, StartedAt: finished, FinishedAt: &finished,
			}
			if test.blocked {
				meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
					Type: operatorv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionTrue,
					Reason: string(operatorv1alpha1.ReasonApplyOutcomeUnknown), Message: "the run is over",
					ObservedGeneration: migration.Generation,
				})
			}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  2, TotalMigrations: 2, HasPendingChanges: true,
				PendingMigrations: []int64{3},
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
				},
			}
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q", actual.Status.Phase, test.wantPhase)
			}
			if test.wantPhase == operatorv1alpha1.MigrationPhaseBlocked && actual.Status.Plan != nil {
				t.Fatal("a plan was published for a migration a previous run may already have executed")
			}
		})
	}
}

// The refusal above is not the latch. A resource is blocked for whatever reason
// is true right now, and every other refusal -- a realm another resource
// claims, a dirty revision, an applied migration whose file moved, one that
// sorts below the current version -- writes its own reason over it. Reading the
// unresolved run off that reason made a competitor going away, or a history
// fault somebody fixed, look like a database somebody had repaired: the next
// reading planned the pending migration and Always dispatched it, with nothing
// having established what the first run did.
func TestAnUnresolvedMigrationRunOutlivesEveryOtherRefusal(t *testing.T) {
	t.Parallel()

	pending := pendingMigrationHistory()
	interleaved := []struct {
		name string
		// refuse drives the resource through one other refusal and returns the
		// status it persisted. Each one ends with the competitor gone or the
		// history fault repaired, so nothing but the unresolved run is left to
		// refuse the migration that is still pending.
		refuse func(*testing.T, *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration
	}{
		{
			name: "a realm another resource claimed for a while",
			refuse: func(t *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
				t.Helper()

				competitor := realmSchemaFixture("orders", migration.Spec.Target.CoordinationKey, false)
				reconciler, api := fakeMigrationReconciler(
					t, staticLogs{}, migration.DeepCopy(), competitor, verificationPolicyConfigMap(),
				)
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				contested := readMigration(t, api, migration)
				assertMigrationBlockedFor(t, contested, operatorv1alpha1.ReasonRealmConflict)
				return contested
			},
		},
		{
			name: "a revision row a run left dirty",
			refuse: refuseMigrationHistory(
				operatorv1alpha1.ReasonHistoryDirty,
				func(report *dataplane.MigrationStatusReport) {
					report.DirtyRevision = &dataplane.MigrationDirty{Version: 3, Applied: 1, Total: 2}
				},
			),
		},
		{
			name: "an applied migration whose file moved",
			refuse: refuseMigrationHistory(
				operatorv1alpha1.ReasonHistoryModified,
				func(report *dataplane.MigrationStatusReport) {
					report.Migrations[0].State = dataplane.MigrationStateModified
				},
			),
		},
		{
			name: "a migration that arrived below the applied version",
			refuse: refuseMigrationHistory(
				operatorv1alpha1.ReasonHistoryOutOfOrder,
				func(report *dataplane.MigrationStatusReport) {
					report.Migrations[1] = dataplane.MigrationRecord{
						Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStateOutOfOrder,
					}
					report.PendingMigrations = []int64{1}
				},
			),
		},
		{
			name: "a suspension somebody lifted again",
			refuse: func(t *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
				t.Helper()

				suspended := migration.DeepCopy()
				suspended.Spec.Suspend = true
				suspended.Generation++
				reconciler, api := fakeMigrationReconciler(t, staticLogs{}, suspended, verificationPolicyConfigMap())
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(suspended)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				resumed := readMigration(t, api, suspended)
				if resumed.Status.Phase != operatorv1alpha1.MigrationPhaseSuspended {
					t.Fatalf("phase = %q, want Suspended", resumed.Status.Phase)
				}
				resumed.Spec.Suspend = false
				return resumed
			},
		},
		{
			// Nothing at all between the run and the next reading, which is the
			// restart on its own: a manager that came back reads the resource
			// and nothing else.
			name: "nothing but a restarted manager",
			refuse: func(_ *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
				return migration
			},
		},
	}

	for _, outcome := range []operatorv1alpha1.MigrationRunOutcome{
		operatorv1alpha1.MigrationRunOutcomeUnknown,
		operatorv1alpha1.MigrationRunOutcomePartial,
	} {
		for _, policy := range []operatorv1alpha1.ApplyPolicy{
			operatorv1alpha1.ApplyPolicyAlways,
			operatorv1alpha1.ApplyPolicyOnApproval,
		} {
			for _, test := range interleaved {
				t.Run(string(outcome)+"/"+string(policy)+"/"+test.name, func(t *testing.T) {
					t.Parallel()

					migration := unresolvedMigrationRun(t, policy, outcome)
					migration = test.refuse(t, migration)
					// An approval is a decision about what should run next, and
					// says nothing about what the last run did. It is in the
					// namespace for the whole of the reading below.
					var namespace []client.Object
					if policy == operatorv1alpha1.ApplyPolicyOnApproval {
						namespace = append(namespace, migrationApprovalFor(migration, publishedPlanFor(t, migration)))
					}
					actual, api := readMigrationHistory(t, migration, pending, namespace...)

					assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonApplyOutcomeUnknown)
					if actual.Status.Plan != nil {
						t.Fatalf("a plan was published for a migration the %s run may already have executed", outcome)
					}
					plans := &operatorv1alpha1.PtahMigrationPlanList{}
					if err := api.List(context.Background(), plans); err != nil {
						t.Fatal(err)
					}
					if len(plans.Items) != 0 {
						t.Fatalf("%d plans were published after an unresolved %s run", len(plans.Items), outcome)
					}
					// The published-plan count above is what carries this
					// claim. A second pass used to follow it, asserting no
					// Apply was authorized, and it could not fail: it read the
					// API server of the previous pass rather than its own, and
					// with no plan published there was nothing to dispatch
					// either way. An assertion that cannot fail is worse than
					// none, because it reads as coverage.
				})
			}
		}
	}
}

// A resource a manager blocked before the record existed carries its latch in
// the refusal alone. That is the shape an upgrade finds, and letting the first
// refusal that overwrites the reason release the latch would replay exactly the
// run this refuses. The reconcile converts that refusal into the record before
// it takes any refusal of its own, so which refusal comes next -- including one
// added after this was written -- decides nothing.
func TestAPreRecordUnresolvedMigrationRunIsAdoptedBeforeItIsOverwritten(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		refuse func(*testing.T, *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration
	}{
		{
			name: "a realm another resource claimed for a while",
			refuse: func(t *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
				t.Helper()

				competitor := realmSchemaFixture("orders", migration.Spec.Target.CoordinationKey, false)
				reconciler, api := fakeMigrationReconciler(
					t, staticLogs{}, migration.DeepCopy(), competitor, verificationPolicyConfigMap(),
				)
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				contested := readMigration(t, api, migration)
				assertMigrationBlockedFor(t, contested, operatorv1alpha1.ReasonRealmConflict)
				return contested
			},
		},
		{
			name: "a revision row a run left dirty",
			refuse: refuseMigrationHistory(
				operatorv1alpha1.ReasonHistoryDirty,
				func(report *dataplane.MigrationStatusReport) {
					report.DirtyRevision = &dataplane.MigrationDirty{Version: 3, Applied: 1, Total: 2}
				},
			),
		},
		{
			// This refusal is taken above the generation check and before any
			// reading, so an edited spec reaches it on the pass that follows the
			// edit. It says nothing about the database, and putting the engine
			// back leaves the resource exactly where the run left it.
			name: "an engine this operator does not support",
			refuse: func(t *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
				t.Helper()

				engine := migration.Spec.Target.Engine
				flipped := migration.DeepCopy()
				flipped.Spec.Target.Engine = operatorv1alpha1.DatabaseEngine("CockroachDB")
				flipped.Generation++
				reconciler, api := fakeMigrationReconciler(t, staticLogs{}, flipped, verificationPolicyConfigMap())
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(flipped)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				refused := readMigration(t, api, flipped)
				assertMigrationBlockedFor(t, refused, operatorv1alpha1.ReasonUnsupportedEngine)
				refused.Spec.Target.Engine = engine
				refused.Generation++
				return refused
			},
		},
	}

	for _, outcome := range []operatorv1alpha1.MigrationRunOutcome{
		operatorv1alpha1.MigrationRunOutcomeUnknown,
		operatorv1alpha1.MigrationRunOutcomePartial,
	} {
		for _, test := range tests {
			t.Run(string(outcome)+"/"+test.name, func(t *testing.T) {
				t.Parallel()

				migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, outcome)
				// The status an older manager wrote: the outcome and the
				// refusal, and nothing else.
				migration.Status.UnresolvedRun = nil
				migration = test.refuse(t, migration)
				if migration.Status.UnresolvedRun == nil {
					// Not fatal: the reading below is what the missing record
					// costs, and it is the claim this test is about.
					t.Errorf("the refusal that overwrote the %s latch adopted nothing", outcome)
				}
				actual, _ := readMigrationHistory(t, migration, pendingMigrationHistory())
				assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonApplyOutcomeUnknown)
				if actual.Status.Plan != nil {
					t.Fatalf("a plan was published for a migration the %s run may already have executed", outcome)
				}
			})
		}
	}
}

// A person is the one who settles this, so the record has to say what to go and
// look at: which attempt ran, the Job it ran as, the plan it was carrying out,
// and the database it addressed. None of that is recoverable once the Job is
// collected.
func TestAnUnresolvedMigrationRunRecordsWhatMayHaveRun(t *testing.T) {
	t.Parallel()

	for _, outcome := range []operatorv1alpha1.MigrationRunOutcome{
		operatorv1alpha1.MigrationRunOutcomeUnknown,
		operatorv1alpha1.MigrationRunOutcomePartial,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()

			migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, outcome)
			unresolved := migration.Status.UnresolvedRun
			if unresolved == nil {
				t.Fatalf("a run that ended %s recorded nothing to account for", outcome)
			}
			if unresolved.Outcome != outcome {
				t.Fatalf("recorded outcome = %q, want %q", unresolved.Outcome, outcome)
			}
			if unresolved.OperationID != testDigest {
				t.Fatalf("recorded operation = %q, want the Apply claim %q", unresolved.OperationID, testDigest)
			}
			run := migration.Status.LastRun
			if unresolved.JobName != run.JobName || unresolved.JobUID != run.JobUID {
				t.Fatalf("recorded Job = %q/%q, want %q/%q",
					unresolved.JobName, unresolved.JobUID, run.JobName, run.JobUID)
			}
			if unresolved.PlanRef == nil || unresolved.PlanRef.UID != types.UID("migration-plan-uid") {
				t.Fatalf("recorded plan = %#v, want the plan the run was carrying out", unresolved.PlanRef)
			}
			if unresolved.TargetIdentityDigest != migration.Status.History.TargetIdentityDigest {
				t.Fatalf("recorded database = %q, want %q",
					unresolved.TargetIdentityDigest, migration.Status.History.TargetIdentityDigest)
			}
			if unresolved.RecordedAt.IsZero() {
				t.Fatal("the record carries no time")
			}
		})
	}
}

// One thing settles an unresolved run: a reading of the database it names that
// finds nothing of this artifact left to apply. A reading of some other
// database has the same shape and answers a different question, so it settles
// nothing -- and the resource goes on refusing rather than planning.
func TestAnUnresolvedMigrationRunIsSettledOnlyByItsOwnDatabase(t *testing.T) {
	t.Parallel()

	inSync := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  3, TotalMigrations: 2, PendingMigrations: []int64{},
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
		},
	}

	t.Run("a database somebody put right", func(t *testing.T) {
		t.Parallel()

		migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways,
			operatorv1alpha1.MigrationRunOutcomeUnknown)
		settled, _ := readMigrationHistory(t, migration, inSync)
		if settled.Status.Phase != operatorv1alpha1.MigrationPhaseInSync {
			t.Fatalf("phase = %q, want InSync", settled.Status.Phase)
		}
		if settled.Status.UnresolvedRun != nil {
			t.Fatalf("the proof left the run unresolved: %#v", settled.Status.UnresolvedRun)
		}
		// And the resource is an ordinary one again: the next migration the
		// artifact adds is planned rather than inheriting a refusal nothing
		// renewed.
		planning, _ := readMigrationHistory(t, settled, pendingMigrationHistory())
		if planning.Status.Plan == nil {
			t.Fatalf("a settled resource did not plan its next pending migration: phase=%q", planning.Status.Phase)
		}
	})

	t.Run("a reading of another database", func(t *testing.T) {
		t.Parallel()

		migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways,
			operatorv1alpha1.MigrationRunOutcomeUnknown)
		// The resource was repointed after the run: the reading below is of a
		// database that never saw it.
		migration.Status.UnresolvedRun.TargetIdentityDigest = "sha256:" + strings.Repeat("7", 64)
		refused, _ := readMigrationHistory(t, migration, inSync)
		assertMigrationBlockedFor(t, refused, operatorv1alpha1.ReasonApplyOutcomeUnknown)
		if refused.Status.UnresolvedRun == nil {
			t.Fatal("a reading of another database settled the run")
		}
	})
}

// unresolvedMigrationRun dispatches one Apply and lets it end the way nobody
// can act on: Partial reports statements it committed and statements it did
// not, and Unknown leaves no frame at all. Both go through the controller
// rather than being written into status, because what the controller records
// there is the thing under test.
func unresolvedMigrationRun(
	t *testing.T,
	policy operatorv1alpha1.ApplyPolicy,
	outcome operatorv1alpha1.MigrationRunOutcome,
) *operatorv1alpha1.PtahMigration {
	t.Helper()

	migration, plan := awaitingApprovalFixture(t)
	if policy != migration.Spec.Policy.Apply {
		migration.Spec.Policy.Apply = policy
		repolicyPlan(t, migration, plan)
	}
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	var logs []byte
	if outcome == operatorv1alpha1.MigrationRunOutcomePartial {
		logs = migrationFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
			OperationID: operation.ID, ChildExitCode: 0,
			CoordinationDigest:   operation.CoordinationDigest,
			TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up",
				Outcome:         dataplane.MigrationOutcomePartial,
				Planned:         []int64{3},
			},
		})
	}
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.LastRun == nil || actual.Status.LastRun.Outcome != outcome {
		t.Fatalf("last run = %#v, want outcome %q", actual.Status.LastRun, outcome)
	}
	assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonApplyOutcomeUnknown)
	return actual
}

// refuseMigrationHistory reads one history the database itself refuses, and
// then repairs it: the reading that follows is the ordinary one, and the only
// thing left to refuse it is the run nobody accounted for.
func refuseMigrationHistory(
	reason operatorv1alpha1.ConditionReason,
	fault func(*dataplane.MigrationStatusReport),
) func(*testing.T, *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
	return func(t *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
		t.Helper()

		report := pendingMigrationHistory()
		fault(&report)
		refused, _ := readMigrationHistory(t, migration, report)
		assertMigrationBlockedFor(t, refused, reason)
		return refused
	}
}

// readMigrationHistory runs one read-only history cycle from persisted status.
// The reconciler and its API server are built fresh, so every cycle is also a
// manager that restarted between the two.
func readMigrationHistory(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	report dataplane.MigrationStatusReport,
	namespace ...client.Object,
) (*operatorv1alpha1.PtahMigration, client.WithWatch) {
	t.Helper()

	reading := migration.DeepCopy()
	reading.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	reading.Status.ActiveOperation = nil
	reading.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, reading, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(reading, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationHistory:     &report,
	})
	objects := append([]client.Object{reading, job, pod, verificationPolicyConfigMap()}, namespace...)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, objects...)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(reading)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return readMigration(t, api, reading), api
}

// pendingMigrationHistory is the ordinary next reading: one migration applied,
// one pending, and nothing the database itself refuses.
func pendingMigrationHistory() dataplane.MigrationStatusReport {
	return dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  2, TotalMigrations: 2, HasPendingChanges: true,
		PendingMigrations: []int64{3},
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
		},
	}
}

func assertMigrationBlockedFor(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	reason operatorv1alpha1.ConditionReason,
) {
	t.Helper()

	blocked := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionTrue || blocked.Reason != string(reason) {
		t.Fatalf("Blocked condition = %#v, want True/%s", blocked, reason)
	}
}

func ensureMigrationAdmissionSnapshot(migration *operatorv1alpha1.PtahMigration) {
	operation := migration.Status.ActiveOperation
	if operation.AdmissionSnapshot != nil {
		return
	}
	preemption := corev1.PreemptLowerPriority
	templateDigest, err := podintent.DigestTemplate(&corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{workload.AnnotationExecutionBindingID: operation.ExecutionBindingID},
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
		PriorityClass:                       operatorv1alpha1.PriorityClassAdmissionSnapshot{Value: 0, PreemptionPolicy: &preemption},
		DefaultTolerationsEnabled:           true,
		DefaultNotReadyTolerationSeconds:    300,
		DefaultUnreachableTolerationSeconds: 300,
	}
	digest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		panic(err)
	}
	snapshot.Digest = digest
	operation.AdmissionSnapshot = snapshot
}

// blockedMessage keeps the failure above readable when the condition is absent
// entirely, which is a different fault from a message that lost its direction.
func blockedMessage(blocked *metav1.Condition) string {
	if blocked == nil {
		return "<no Blocked condition>"
	}
	return blocked.Message
}

// The state an upgrade actually finds. A manager older than status.unresolvedRun
// held the latch in the Blocked condition's reason, and the defect that record
// exists to fix is that every later refusal overwrote it -- so by the time the
// new manager first reads the object, the reason it left is usually gone. An
// adoption that required its own reason to have survived would adopt the
// objects the defect missed and skip the ones it reached, and those are the
// ones that go on to publish a plan and replay a run nobody accounted for.
func TestAnUnresolvedMigrationRunIsAdoptedUnderARefusalThatOverwroteItBeforeTheUpgrade(t *testing.T) {
	t.Parallel()

	for _, reason := range []operatorv1alpha1.ConditionReason{
		operatorv1alpha1.ReasonRealmConflict,
		operatorv1alpha1.ReasonHistoryDirty,
		operatorv1alpha1.ReasonHistoryModified,
		operatorv1alpha1.ReasonHistoryOutOfOrder,
		operatorv1alpha1.ReasonUnsupportedEngine,
	} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()

			migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways,
				operatorv1alpha1.MigrationRunOutcomeUnknown)
			// The status as the older manager left it: the run's outcome, a
			// Blocked condition, and some other refusal's reason on it. No
			// record, because that manager had none to write.
			migration.Status.UnresolvedRun = nil
			blocked := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			if blocked == nil || blocked.Status != metav1.ConditionTrue {
				t.Fatalf("the fixture is not blocked, so there is no latch to overwrite: %#v", blocked)
			}
			blocked.Reason = string(reason)

			actual, _ := readMigrationHistory(t, migration, pendingMigrationHistory())
			if actual.Status.UnresolvedRun == nil {
				t.Fatal("the upgrade adopted nothing for a run latched under another refusal's reason")
			}
			if actual.Status.Plan != nil {
				t.Fatalf("a plan was published for a migration the unknown run may already have executed: %#v",
					actual.Status.Plan)
			}
			assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonApplyOutcomeUnknown)
		})
	}
}

// Which database the record names decides what can settle it, so it has to be
// the one the run actually opened. The executor reports that in its result
// frame, and it is not always the database the plan was computed against: the
// Secret behind the target can be rewritten between the history reading and the
// Apply. Naming the planned database instead would let a clean reading of it
// settle a run that never touched it, while a reading of the database that was
// touched could never match what was stored.
func TestAnUnresolvedMigrationRunRecordsTheDatabaseTheRunReported(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	repolicyPlan(t, migration, plan)
	operation := applyClaimFor(t, migration, plan)
	planned := migration.Status.History.TargetIdentityDigest
	rotated := "sha256:" + strings.Repeat("d", 64)
	if planned == rotated {
		t.Fatal("the fixture already plans against the rotated database, so this proves nothing")
	}
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	logs := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest: operation.CoordinationDigest,
		// The credential moved: this run opened a database the plan was never
		// computed against.
		TargetIdentityDigest: rotated,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomePartial,
			Planned:         []int64{3},
		},
	})
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	unresolved := actual.Status.UnresolvedRun
	if unresolved == nil {
		t.Fatal("a partial run against a rotated database recorded nothing to account for")
	}
	if unresolved.TargetIdentityDigest != rotated {
		t.Fatalf("recorded database = %q, want the one the run reported %q (the plan was computed against %q)",
			unresolved.TargetIdentityDigest, rotated, planned)
	}
}
