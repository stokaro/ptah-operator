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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

func TestMigrationApplyWaitsForTheApprovalItsPolicyRequires(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("an unapproved plan was dispatched: %#v", actual.Status.ActiveOperation)
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
		t.Fatalf("phase = %q, want AwaitingApproval", actual.Status.Phase)
	}
}

func TestMigrationApplyClaimsTheApprovedPlan(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply {
		t.Fatalf("active operation = %#v, want an Apply claim", operation)
	}
	if operation.PlanRef == nil || operation.PlanRef.UID != plan.UID {
		t.Fatalf("the claim does not name the plan it carries out: %#v", operation.PlanRef)
	}
	if operation.ApprovalRef == nil || operation.ApprovalRef.UID != approval.UID {
		t.Fatalf("the claim does not name the approval that authorized it: %#v", operation.ApprovalRef)
	}
	if operation.DispatchNotAfter == nil || operation.ExecutionNotAfter == nil {
		t.Fatal("the claim carries no dispatch and execution bounds")
	}
	if !operation.DispatchNotAfter.After(operation.StartedAt.Time) {
		t.Fatal("the dispatch bound is not after the claim")
	}
	if operation.LeaseEpoch == "" || operation.LeaseDurationSeconds < 1 {
		t.Fatalf("the claim carries no database lock binding: %#v", operation)
	}
	if int64(operation.LeaseDurationSeconds) <= migrationActiveDeadline(migration) {
		t.Fatal("the Lease does not outlast the Pod deadline it authorizes")
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseApplying {
		t.Fatalf("phase = %q, want Applying", actual.Status.Phase)
	}
}

// An approval wakes the migration it names. The test above is what makes the
// wake worth having: with the next reading not yet due, the reconcile it
// produces claims the approved plan, where without it the approval waited out
// spec.interval before the controller looked at it at all.
func TestAnApprovalWakesTheMigrationItNames(t *testing.T) {
	t.Parallel()

	approval := func(name string) *operatorv1alpha1.PtahMigrationApproval {
		return &operatorv1alpha1.PtahMigrationApproval{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "orders-approval"},
			Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
				MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: name, UID: "migration-uid"},
			},
		}
	}

	requests := migrationForApproval(context.Background(), approval("orders"))
	want := types.NamespacedName{Namespace: "shop", Name: "orders"}
	if len(requests) != 1 || requests[0].NamespacedName != want {
		t.Fatalf("an approval for %s woke %v", want, requests)
	}
	// The namespace is the approval's own. A reference cannot name a migration
	// elsewhere, and a wake that crossed namespaces would reconcile the wrong one.
	if got := requests[0].Namespace; got != approval("orders").Namespace {
		t.Fatalf("the wake left the approval's namespace for %q", got)
	}

	if requests := migrationForApproval(context.Background(), approval("")); len(requests) != 0 {
		t.Fatalf("an approval that names no migration woke %v", requests)
	}
	if requests := migrationForApproval(context.Background(), &operatorv1alpha1.PtahSchemaApproval{}); len(requests) != 0 {
		t.Fatalf("an object of another kind woke %v", requests)
	}
}

func TestMigrationApplyRunsWithoutApprovalOnlyWhenThePolicySaysAlways(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		policy    operatorv1alpha1.ApplyPolicy
		wantClaim bool
	}{
		{policy: operatorv1alpha1.ApplyPolicyAlways, wantClaim: true},
		{policy: operatorv1alpha1.ApplyPolicyNever, wantClaim: false},
	} {
		t.Run(string(test.policy), func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			migration.Spec.Policy.Apply = test.policy
			migration.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
			repolicyPlan(t, migration, plan)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			claimed := actual.Status.ActiveOperation != nil
			if claimed != test.wantClaim {
				t.Fatalf("claimed = %t, want %t (phase %q)", claimed, test.wantClaim, actual.Status.Phase)
			}
		})
	}
}

func TestMigrationApplyDiscardsAPlanTheEvidenceNoLongerSupports(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	// Somebody else advanced the database between planning and execution.
	migration.Status.History.Fingerprint = "sha256:" + strings.Repeat("c", 64)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a plan computed against a moved history was dispatched")
	}
	if actual.Status.Plan != nil {
		t.Fatal("a plan the evidence no longer supports was retained")
	}
}

func TestMigrationApplyEvidenceDecidesWhatHappensNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		report      *dataplane.MigrationRunReport
		wantOutcome operatorv1alpha1.MigrationRunOutcome
		wantPhase   operatorv1alpha1.MigrationPhase
		// wantProgressing separates a run that is still being confirmed from one
		// that has stopped and may not be retried. Reporting progress on the
		// second says the opposite of what Blocked says on the same object
		// (stokaro/ptah-operator#94).
		wantProgressing       metav1.ConditionStatus
		wantProgressingReason operatorv1alpha1.ConditionReason
	}{
		{
			name: "the database recorded every planned migration",
			report: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up",
				Outcome:         dataplane.MigrationOutcomeApplied,
				Planned:         []int64{3},
				Applied:         []int64{3},
			},
			wantOutcome:           operatorv1alpha1.MigrationRunOutcomeApplied,
			wantPhase:             operatorv1alpha1.MigrationPhaseVerifyingHistory,
			wantProgressing:       metav1.ConditionTrue,
			wantProgressingReason: operatorv1alpha1.ReasonVerifyingConvergence,
		},
		{
			name: "a migration failed and committed nothing",
			report: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up",
				Outcome:         dataplane.MigrationOutcomeFailed,
				Planned:         []int64{3},
			},
			wantOutcome:           operatorv1alpha1.MigrationRunOutcomeFailed,
			wantPhase:             operatorv1alpha1.MigrationPhaseVerifyingHistory,
			wantProgressing:       metav1.ConditionTrue,
			wantProgressingReason: operatorv1alpha1.ReasonVerifyingConvergence,
		},
		{
			name: "a migration committed some of its statements",
			report: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up",
				Outcome:         dataplane.MigrationOutcomePartial,
				Planned:         []int64{3},
			},
			wantOutcome:           operatorv1alpha1.MigrationRunOutcomePartial,
			wantPhase:             operatorv1alpha1.MigrationPhaseBlocked,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonApplyOutcomeUnknown,
		},
		{
			name:                  "the run left no readable account",
			report:                nil,
			wantOutcome:           operatorv1alpha1.MigrationRunOutcomeUnknown,
			wantPhase:             operatorv1alpha1.MigrationPhaseBlocked,
			wantProgressing:       metav1.ConditionFalse,
			wantProgressingReason: operatorv1alpha1.ReasonApplyOutcomeUnknown,
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
			if test.report == nil {
				result.Uncertain = true
				result.MutationStarted = true
			}
			var logs []byte
			if test.report != nil {
				logs = migrationFrame(t, result)
			}
			// An Apply whose Pod left no frame at all is the unreadable case: the
			// runner refuses to emit a migration frame without its report.
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
			)
			holdMigrationApplyLease(t, reconciler, api, migration)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.LastRun == nil {
				t.Fatalf("the run left no evidence in status: phase=%q operation=%#v conditions=%#v",
					actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
			}
			if actual.Status.LastRun.Outcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", actual.Status.LastRun.Outcome, test.wantOutcome)
			}
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q", actual.Status.Phase, test.wantPhase)
			}
			if actual.Status.ActiveOperation != nil {
				t.Fatal("the finished Apply claim was retained")
			}
			if actual.Status.Plan != nil {
				t.Fatal("the executed plan was retained")
			}
			if test.wantPhase == operatorv1alpha1.MigrationPhaseBlocked &&
				!meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked) {
				t.Fatal("an unrecoverable outcome did not block the resource")
			}
			progressing := meta.FindStatusCondition(
				actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Status != test.wantProgressing ||
				progressing.Reason != string(test.wantProgressingReason) {
				t.Fatalf("Progressing condition = %#v, want %s/%s",
					progressing, test.wantProgressing, test.wantProgressingReason)
			}
			// Every terminal outcome hands the database back, including the two
			// that stop the resource: a run that is over holds nothing, and the
			// next claimant is whoever asks first rather than whoever waits
			// longest.
			assertDatabaseHandedBack(t, reconciler, api)
		})
	}
}

func TestMigrationApplyIsNeverRecreatedOnceDispatched(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	operation.JobUID = ""
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("a dispatched Apply was recreated")
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	if actual.Status.LastRun == nil || actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("last run = %#v, want an unknown outcome", actual.Status.LastRun)
	}
}

// awaitingApprovalFixture returns a migration whose plan is published and whose
// policy requires an approval, plus that plan.
func awaitingApprovalFixture(t *testing.T) (*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan) {
	t.Helper()

	migration := migrationFixture()
	migration.Finalizers = []string{migrationOperationFinalizer}
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseAwaitingApproval
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.History = &operatorv1alpha1.MigrationHistoryStatus{
		ObservedAt:           metav1.Now(),
		ContractVersion:      dataplane.SupportedMigrationStatusContract,
		CurrentVersion:       2,
		Fingerprint:          "sha256:" + strings.Repeat("5", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("6", 64),
		AppliedCount:         1,
		PendingCount:         1,
	}
	next := metav1.NewTime(time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC))
	migration.Status.NextReconciliationTime = &next
	plan := publishedPlanFor(t, migration)
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}
	return migration, plan
}

func publishedPlanFor(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
) *operatorv1alpha1.PtahMigrationPlan {
	t.Helper()

	binding := migration.Status.ExecutionBinding
	planned := []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "checksum-3"}}
	sequenceDigest, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		t.Fatal(err)
	}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest(
		string(migration.Spec.Target.Engine), migration.Spec.Target.CoordinationKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	policyFingerprint, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	policyBinding := verificationPolicyConfigMap()
	policyDigest := fingerprint.DigestBytes([]byte(policyBinding.Data["policy.yaml"]))
	planBinding := migrationplan.Binding{
		MigrationUID:             migration.UID,
		HistoryFingerprint:       migration.Status.History.Fingerprint,
		SequenceDigest:           sequenceDigest,
		ArtifactDigest:           migration.Status.Artifact.Digest,
		CoordinationDigest:       coordinationDigest,
		TargetIdentityDigest:     migration.Status.History.TargetIdentityDigest,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyBinding.UID,
		VerificationPolicyDigest: policyDigest,
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
	}
	planFingerprint, err := planBinding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrationplan.Desired(migration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion:          migrationplan.ContractVersion,
		MigrationRef:             operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
		Fingerprint:              planFingerprint,
		Migrations:               planned,
		HistoryFingerprint:       planBinding.HistoryFingerprint,
		CurrentVersion:           migration.Status.History.CurrentVersion,
		ArtifactDigest:           planBinding.ArtifactDigest,
		CoordinationDigest:       planBinding.CoordinationDigest,
		TargetIdentityDigest:     planBinding.TargetIdentityDigest,
		PolicyFingerprint:        planBinding.PolicyFingerprint,
		VerificationPolicyUID:    planBinding.VerificationPolicyUID,
		VerificationPolicyDigest: planBinding.VerificationPolicyDigest,
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		CreatedAt:                metav1.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.UID = types.UID("migration-plan-uid")
	return plan
}

// repolicyPlan republishes the plan under the migration's current policy, so a
// policy change in a test does not read as a stale plan.
func repolicyPlan(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) {
	t.Helper()

	updated := publishedPlanFor(t, migration)
	plan.Name = updated.Name
	plan.Spec = updated.Spec
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}
}

func migrationApprovalFor(
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) *operatorv1alpha1.PtahMigrationApproval {
	return &operatorv1alpha1.PtahMigrationApproval{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: "orders-approval", UID: types.UID("approval-uid"),
			CreationTimestamp: metav1.Now(),
		},
		Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
			MigrationRef:             operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
			PlanRef:                  operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint:          plan.Spec.Fingerprint,
			HistoryFingerprint:       plan.Spec.HistoryFingerprint,
			ArtifactDigest:           plan.Spec.ArtifactDigest,
			CoordinationDigest:       plan.Spec.CoordinationDigest,
			TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
			PolicyFingerprint:        plan.Spec.PolicyFingerprint,
			VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
			VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
			ExecutionBindingID:       plan.Spec.ExecutionBindingID,
			ControllerImage:          plan.Spec.ControllerImage,
			ControllerRevision:       plan.Spec.ControllerRevision,
			ControllerStateVersion:   plan.Spec.ControllerStateVersion,
			PtahVersion:              plan.Spec.PtahVersion,
			ExecutorImage:            plan.Spec.ExecutorImage,
			RunnerImage:              plan.Spec.RunnerImage,
			RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
			Approver:                 operatorv1alpha1.ApprovalIdentity{Username: "operator@example.test"},
			ApprovedAt:               metav1.Now(),
			MutationRequestUID:       "mutation-request-uid",
		},
	}
}

// applyClaimFor persists the Apply claim the controller would have written.
func applyClaimFor(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) *operatorv1alpha1.MigrationOperationStatus {
	t.Helper()

	reconciler := &MigrationReconciler{
		Jobs:      fakeJobs{},
		APIReader: migrationPolicyReader(t),
	}
	inputFingerprint, err := reconciler.migrationInputFingerprint(
		context.Background(), migration, operatorv1alpha1.MigrationOperationApply,
	)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)
	dispatchNotAfter := metav1.NewTime(startedAt.Add(10 * time.Minute))
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:                 operatorv1alpha1.MigrationOperationApply,
		ID:                   testDigest,
		InputFingerprint:     inputFingerprint,
		ExecutionBindingID:   migration.Status.ExecutionBinding.Epoch,
		StartedAt:            metav1.NewTime(startedAt),
		Attempt:              1,
		Source:               migrationSourceBinding(migration),
		CoordinationDigest:   plan.Spec.CoordinationDigest,
		LeaseEpoch:           "v1-" + strings.Repeat("2", 32),
		LeaseDurationSeconds: 960,
		DispatchStarted:      true,
		DispatchNotAfter:     &dispatchNotAfter,
		ExecutionNotAfter:    dispatchNotAfter.DeepCopy(),
		PlanRef:              &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		},
	}
	name, err := (fakeJobs{}).NameForMigration(migration, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	migration.Status.ActiveOperation = operation
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseApplying
	return operation
}

// assertDatabaseHandedBack fails while any database Lease the reconciler
// coordinates through still names a holder.
//
// A release that does not clear the holder is invisible: the run reports its
// evidence, the resource settles, and nothing in status says the database is
// still marked busy. What it costs is paid by the next claimant, which waits
// out the whole lease duration for a run that finished minutes ago.
func assertDatabaseHandedBack(t *testing.T, reconciler *MigrationReconciler, api client.Client) {
	t.Helper()

	leases := &coordinationv1.LeaseList{}
	if err := api.List(context.Background(), leases, client.InNamespace(reconciler.LockNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) == 0 {
		t.Fatal("no database Lease exists, so this proved nothing about handing one back")
	}
	for _, lease := range leases.Items {
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
			t.Fatalf("Lease %s is still held by %q after the run finished",
				lease.Name, *lease.Spec.HolderIdentity)
		}
	}
}

// assertDatabaseStillHeld is the inverse: it fails once a database Lease the
// reconciler coordinates through has lost its holder.
//
// The release is only safe after nothing can write any more. While the
// dispatched Pod may still be executing SQL, handing the database back is
// permission for a second writer, and the Lease is the only thing standing
// between them.
func assertDatabaseStillHeld(t *testing.T, reconciler *MigrationReconciler, api client.Client) {
	t.Helper()

	leases := &coordinationv1.LeaseList{}
	if err := api.List(context.Background(), leases, client.InNamespace(reconciler.LockNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) == 0 {
		t.Fatal("no database Lease exists, so this proved nothing about holding one")
	}
	for _, lease := range leases.Items {
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			t.Fatalf("Lease %s was handed back while the dispatched Apply's Pod could still be running", lease.Name)
		}
	}
}

// holdMigrationApplyLease puts the database lock in the state a dispatched
// Apply left it: held by this claim, under the epoch the claim recorded.
func holdMigrationApplyLease(
	t *testing.T,
	reconciler *MigrationReconciler,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
) {
	t.Helper()

	operation := migration.Status.ActiveOperation
	result, err := reconciler.Locks.Acquire(context.Background(), targetlock.Request{
		CoordinationNamespace: reconciler.LockNamespace,
		CoordinationDigest:    operation.CoordinationDigest,
		Holder:                targetlock.Holder{SchemaUID: migration.UID, OperationID: operation.ID},
		Duration:              time.Duration(operation.LeaseDurationSeconds) * time.Second,
	})
	if err != nil || !result.Acquired {
		t.Fatalf("seed the database lock: acquired=%t err=%v", result.Acquired, err)
	}
	stored := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(migration), stored); err != nil {
		t.Fatal(err)
	}
	before := stored.DeepCopy()
	stored.Status.ActiveOperation.LeaseEpoch = result.Epoch
	if err := api.Status().Patch(context.Background(), stored, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	operation.LeaseEpoch = result.Epoch
}

// migrationPolicyReader reads only the verification policy a claim is
// fingerprinted against.
func migrationPolicyReader(t *testing.T) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(verificationPolicyConfigMap()).Build()
}

func TestMigrationApplyOutlivingItsComponentsIsUncertainNotDiscarded(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	// The executor rolled out while the Apply was in flight.
	migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a dispatched Apply claim survived the rollout that retired its components")
	}
	if actual.Status.LastRun == nil ||
		actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("last run = %#v, want an unknown outcome rather than no record at all", actual.Status.LastRun)
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	if !meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked) {
		t.Fatal("the resource was not blocked after a run nobody can account for")
	}
	// The evidence is written under the binding that authorized the run; the
	// binding moves on the next pass, when no claim is in flight.
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual = readMigration(t, api, migration)
	if binding := actual.Status.ExecutionBinding; binding == nil ||
		binding.ExecutorImage != "example.invalid/ptah@"+testDigest {
		t.Fatalf("execution binding = %#v, want the current components", binding)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a claim was taken while the resource is blocked on an unknown run")
	}
}

// runningExecutorPod puts the Pod in the state the API server reports while
// the executor is still there: a phase that is not terminal, and a container
// that has not exited.
func runningExecutorPod(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
}

// uncertainApplyUnderBindingChange stands up a dispatched Apply that a rollout
// retires: the claim is in flight, an execution component moved under it, and
// the database Lease is still held by the claim. The reconcile that follows
// takes the uncertain path with whatever workload the caller left in the
// namespace, which is what each row varies.
func uncertainApplyUnderBindingChange(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
	dispatched ...client.Object,
) (*MigrationReconciler, client.WithWatch) {
	t.Helper()

	migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
	objects := append([]client.Object{migration, plan, verificationPolicyConfigMap()}, dispatched...)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, objects...)
	holdMigrationApplyLease(t, reconciler, api, migration)
	return reconciler, api
}

// An Apply the controller cannot read is still an Apply that may be running.
// Handing the database back there is the one thing the Lease exists to
// prevent: the next claimant acquires it and runs DDL beside a live executor.
//
// Two separate facts keep it, and each row rests on one of them alone. A row
// that satisfied both would pass with either half of the gate deleted, which
// is how the Pod read came to be unproven in the first place.
func TestUncertainMigrationApplyKeepsTheDatabaseWhileItsPodMayRun(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		workload func(job *batchv1.Job, pod *corev1.Pod)
	}{
		{
			// Both facts: the Job is unfinished and its Pod is executing SQL.
			name: "the Job has not finished and its Pod is running",
			workload: func(job *batchv1.Job, pod *corev1.Pod) {
				job.Status.Conditions = nil
				job.Status.Active = 1
				runningExecutorPod(pod)
			},
		},
		{
			// Only the Pod says so. A Job reports Complete once its successes
			// are counted, and the API server has not finished with the Pod
			// that earned them.
			name: "the Job reports Complete before its Pod has stopped",
			workload: func(_ *batchv1.Job, pod *corev1.Pod) {
				runningExecutorPod(pod)
			},
		},
		{
			// Only the Job says so. Every Pod it owns has stopped, and under a
			// backoff limit an unfinished Job starts the next one.
			name: "the Job has not finished and will start another Pod",
			workload: func(job *batchv1.Job, pod *corev1.Pod) {
				job.Status.Conditions = nil
				job.Status.Failed = 1
				pod.Status.Phase = corev1.PodFailed
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			row.workload(job, pod)
			reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan, job, pod)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
				t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
			}
			assertDatabaseStillHeld(t, reconciler, api)
		})
	}
}

// A Pod read that failed is not a Pod that stopped. Everything else here says
// the run is over -- a terminal Job, and the Pod the API server has finished
// with -- and the one read that would confirm it does not answer. Releasing on
// that reading costs the database; keeping it costs one lease duration, which
// migrationApplyLeaseGrace already sizes to outlive the Pod.
func TestUncertainMigrationApplyKeepsTheDatabaseWhenItsPodsCannotBeRead(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan, job, pod)
	reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
		List: func(
			ctx context.Context,
			reader client.WithWatch,
			list client.ObjectList,
			options ...client.ListOption,
		) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errors.New("injected Pod list failure")
			}
			return reader.List(ctx, list, options...)
		},
	})

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	assertDatabaseStillHeld(t, reconciler, api)
}

// A create that fails is not proof that nothing was created: a client timeout
// or a 5xx after the write persisted leaves a Job running under the name the
// claim reserved, and no UID on the claim to recognize it by. The name is
// derived from the claim, so reading it is what tells the two apart.
func TestUncertainMigrationApplyKeepsTheDatabaseWhenItsCreateMayHaveLanded(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	job.Status.Conditions = nil
	job.Status.Active = 1
	runningExecutorPod(pod)
	// The create never reported back, so the claim carries the name it reserved
	// and nothing else -- while the Job under that name executes SQL.
	operation.JobUID = ""
	reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	assertDatabaseStillHeld(t, reconciler, api)
}

// The same read, when it cannot be made at all. A NotFound is what says the
// create never landed; an error says only that nobody knows, and a claim whose
// reserved name may hold a running Job is the case the gate exists for.
func TestUncertainMigrationApplyKeepsTheDatabaseWhenItsReservedNameCannotBeRead(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	// The create never reported back, so the reserved name is all the claim
	// carries -- and the read that would say what is under it fails.
	operation.JobUID = ""
	reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan)
	reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
		Get: func(
			ctx context.Context,
			reader client.WithWatch,
			key client.ObjectKey,
			object client.Object,
			options ...client.GetOption,
		) error {
			if _, ok := object.(*batchv1.Job); ok {
				return errors.New("injected dispatched Apply Job read failure")
			}
			return reader.Get(ctx, key, object, options...)
		},
	})

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	assertDatabaseStillHeld(t, reconciler, api)
}

// The other direction of the same read, and the reason it is a read rather
// than a refusal to release: a claim whose reserved name holds nothing
// dispatched nothing. Keeping the database there would strand it for a whole
// lease duration over a run that never started, which is the common uncertain
// case rather than the rare one.
func TestUncertainMigrationApplyHandsTheDatabaseBackWhenNothingWasCreated(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	// applyClaimFor leaves the claim as dispatch does before the create: a
	// reserved Job name, no UID, and no object in the namespace.
	applyClaimFor(t, migration, plan)
	reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	assertDatabaseHandedBack(t, reconciler, api)
}

// A Job under the claim's name is this claim's Job only if its UID says so. The
// name is derived from the claim, so a later Job holds it too -- and reading
// that Job's condition would keep the database over a run this claim never
// dispatched, until a lease duration expired. The identity check is what sends
// the gate to the Pod read instead, where the UID the claim recorded owns
// nothing and the database goes back to whoever asks for it next.
func TestUncertainMigrationApplyHandsTheDatabaseBackWhenAnotherJobHoldsTheName(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	// The object under the reserved name is a Job that replaced this claim's:
	// the same name, a UID the claim never saw, and a Pod of its own running
	// now. The claim keeps the UID it dispatched.
	job.UID = "replacement-job-uid"
	job.Status.Conditions = nil
	job.Status.Active = 1
	pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
	runningExecutorPod(pod)
	if operation.JobUID == job.UID {
		t.Fatal("the claim names the Job that replaced it, so this proves nothing about identity")
	}
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	assertDatabaseHandedBack(t, reconciler, api)
}

// Losing lock continuity retires the claim without the Job in hand: the
// reconcile that handles it passes nil, and the claim's UID is all that names
// what was dispatched. A Job the scheduler has not reached owns no Pod yet, so
// reading Pods alone answers "nothing is running" about a run that is about to
// start, and the database would be handed to the next claimant in front of it.
func TestDispatchedApplyMayStillWriteReadsTheJobTheCallerDidNotPass(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, _ := terminalMigrationWorkload(migration, batchv1.JobComplete)
	// Dispatched, not finished, and owning no Pod yet.
	job.Status.Conditions = nil
	reconciler, _ := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap(), job)

	if !reconciler.dispatchedApplyMayStillWrite(context.Background(), migration.Namespace, operation, nil) {
		t.Fatal("the gate handed the database back for a dispatched Job it never read")
	}
}

// The Apply Job carries the approved plan's identity to the runner, which
// refuses to open the database without it. So the plan is re-read at dispatch,
// and a claim whose plan is gone or replaced dispatches nothing.
func TestMigrationPlanForJobBindsDispatchToTheApprovedPlan(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	reconciler, _ := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

	read, err := reconciler.migrationPlanForJob(context.Background(), migration, operation)
	if err != nil {
		t.Fatalf("migrationPlanForJob() error = %v", err)
	}
	if read == nil || read.UID != plan.UID || read.Spec.Fingerprint != plan.Spec.Fingerprint {
		t.Fatalf("migrationPlanForJob() = %#v, want the plan the claim named", read)
	}

	history := &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationHistory}
	if read, err := reconciler.migrationPlanForJob(context.Background(), migration, history); read != nil || err != nil {
		t.Fatalf("a history operation carried plan %#v (error %v)", read, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*operatorv1alpha1.MigrationOperationStatus)
	}{
		{
			name:   "the claim names no plan",
			mutate: func(operation *operatorv1alpha1.MigrationOperationStatus) { operation.PlanRef = nil },
		},
		{
			name: "the plan was replaced after the claim",
			mutate: func(operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.PlanRef.UID = types.UID("a-newer-plan")
			},
		},
		{
			name: "the plan no longer exists",
			mutate: func(operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.PlanRef.Name = "ptah-mplan-gone"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := operation.DeepCopy()
			test.mutate(claim)
			read, err := reconciler.migrationPlanForJob(context.Background(), migration, claim)
			if err == nil || read != nil {
				t.Fatalf("migrationPlanForJob() = %#v, %v, want a refusal", read, err)
			}
		})
	}
}

func TestAcceptanceReviewReplacedPolicyInvalidatesApprovedMigration(t *testing.T) {
	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	policy := verificationPolicyConfigMap()
	policy.UID = "replacement-policy-uid"
	policy.Data["policy.yaml"] = "requireDigestPin: true\nrequireSignature: true"
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, policy)
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatal(err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil && actual.Status.ActiveOperation.Type == operatorv1alpha1.MigrationOperationApply {
		t.Fatalf("approved Apply was claimed using deleted policy UID %q while live policy UID is %q", plan.Spec.VerificationPolicyUID, policy.UID)
	}
}

func TestAcceptanceReviewTransactionModeInvalidatesPlan(t *testing.T) {
	migration, plan := awaitingApprovalFixture(t)
	before, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	migration.Spec.Policy.TransactionMode = "none"
	after, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("changing PostgreSQL migration transaction mode from default per-file to none does not change the policy fingerprint")
	}
	reconciler, _ := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	if _, err := reconciler.currentMigrationPlan(context.Background(), migration); err == nil {
		t.Error("the old plan remains current after changing migration transaction semantics")
	}
}

// undispatchedApplyClaim is the claim as it stands between the decision and
// the Job: a reserved name, no Job UID, and nothing consumed.
func undispatchedApplyClaim(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) *operatorv1alpha1.MigrationOperationStatus {
	t.Helper()

	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = false
	operation.JobUID = ""
	return operation
}

// reconcileUntilTheApplyClaimIsGone runs the reconciler until the Apply claim
// is retired, and stops there so a later pass cannot claim something else and
// muddy what the Job list proves.
func reconcileUntilTheApplyClaimIsGone(
	t *testing.T,
	reconciler *MigrationReconciler,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
) *operatorv1alpha1.PtahMigration {
	t.Helper()

	actual := migration
	for pass := 0; pass < 4; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual = readMigration(t, api, migration)
		operation := actual.Status.ActiveOperation
		if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply {
			return actual
		}
	}
	return actual
}

func assertNoMigrationJobDispatched(t *testing.T, api client.Client) {
	t.Helper()

	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("%d Jobs were dispatched from a claim that should have been discarded", len(jobs.Items))
	}
}

// The decision boundary is not the last one. A policy that stops binding after
// the claim was written and before the Job exists has to be caught here too,
// because the claim's input fingerprint names the policy object and never its
// identity or its bytes.
//
// Each way of stopping is measured on its own. A deletion and a recreation
// carries the same bytes under a new UID, and only the UID half of the
// comparison sees it; bytes that differ under the same UID are what the digest
// half is for; a check that kept one half would pass the other case unchanged.
// A policy deleted and not recreated is neither: there is nothing to compare,
// and failing to read the terms the plan was decided under is itself a refusal.
func TestMigrationApplyWillNotDispatchUnderAVerificationPolicyThatNoLongerBinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// live is the verification policy as it stands when the claim reaches
		// the dispatch boundary. A nil return is the object deleted and left
		// deleted, which is the case the issue names first.
		live func() *corev1.ConfigMap
	}{
		{
			name: "recreated under a new UID",
			live: func() *corev1.ConfigMap {
				policy := verificationPolicyConfigMap()
				policy.UID = "replacement-policy-uid"
				return policy
			},
		},
		{
			name: "different content under the same UID",
			live: func() *corev1.ConfigMap {
				policy := verificationPolicyConfigMap()
				policy.Data["policy.yaml"] = "requireDigestPin: true\nrequireSignature: true"
				return policy
			},
		},
		{
			name: "deleted and not recreated",
			live: func() *corev1.ConfigMap { return nil },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			undispatchedApplyClaim(t, migration, plan)
			objects := []client.Object{migration, plan}
			if live := test.live(); live != nil {
				objects = append(objects, live)
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, objects...)

			actual := reconcileUntilTheApplyClaimIsGone(t, reconciler, api, migration)
			if actual.Status.ActiveOperation != nil {
				t.Fatalf("the Apply claim survived a verification policy that no longer binds: %#v",
					actual.Status.ActiveOperation)
			}
			assertNoMigrationJobDispatched(t, api)
		})
	}
}

// The watch is how soon a replaced policy is noticed, so it has to name the
// migrations that read this ConfigMap and no others. A map that woke every
// migration in the namespace would read as working and measure nothing.
func TestVerificationPolicyWakesOnlyTheMigrationsThatReadIt(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{operatorv1alpha1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	reader := migrationFixture()
	other := migrationFixture()
	other.Name = "invoices"
	other.UID = "other-migration-uid"
	other.Spec.Artifact.VerificationPolicyFrom.Name = "other-verification"
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&operatorv1alpha1.PtahMigration{}, migrationPolicyIndex, func(object client.Object) []string {
			migration := object.(*operatorv1alpha1.PtahMigration)
			if migration.Spec.Artifact.VerificationPolicyFrom.Name == "" {
				return nil
			}
			return []string{migration.Spec.Artifact.VerificationPolicyFrom.Name}
		}).
		WithObjects(reader, other).Build()
	reconciler := &MigrationReconciler{Client: api, APIReader: api, Scheme: scheme}

	requests := reconciler.migrationsForVerificationPolicy(context.Background(), verificationPolicyConfigMap())
	if len(requests) != 1 || requests[0].Name != reader.Name || requests[0].Namespace != reader.Namespace {
		t.Fatalf("the policy woke %#v, want only %s/%s", requests, reader.Namespace, reader.Name)
	}
	if woken := reconciler.migrationsForVerificationPolicy(context.Background(), &corev1.Secret{}); woken != nil {
		t.Fatalf("a Secret woke %#v", woken)
	}
}

// A claim decided under one transaction mode does not dispatch once the mode
// is edited. What catches that is generation, not the mode: the API server
// bumps generation on every spec edit and migrationInputFingerprint already
// carried it, so this boundary was never open for a spec field. Deleting
// transaction_mode from that fingerprint leaves this test passing.
//
// The bump is written out for that reason. A fixture that edits the spec and
// leaves generation alone measures a state Kubernetes cannot produce, and a
// test standing on it would read as proof of a hole that was never there.
func TestMigrationApplyWillNotDispatchUnderAChangedTransactionMode(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	undispatchedApplyClaim(t, migration, plan)
	migration.Spec.Policy.TransactionMode = "none"
	migration.Generation++ // what the API server stamps on a spec edit
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

	actual := reconcileUntilTheApplyClaimIsGone(t, reconciler, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the Apply claim survived a changed transaction mode: %#v", actual.Status.ActiveOperation)
	}
	assertNoMigrationJobDispatched(t, api)
}

// An Apply that is already running is left alone. Editing the mode under it
// cannot unrun the SQL, and retiring the claim while its Pod writes is how the
// database ends up with a run nothing is waiting for.
func TestMigrationApplyKeepsADispatchedRunWhenTheTransactionModeChanges(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, _ := terminalMigrationWorkload(migration, batchv1.JobComplete)
	// Dispatched and still running.
	job.Status.Conditions = nil
	migration.Spec.Policy.TransactionMode = "none"
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap(), job)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.JobUID != operation.JobUID {
		t.Fatalf("a running Apply was retired by a policy edit: %#v", actual.Status.ActiveOperation)
	}
}

// A claim refused at the dispatch boundary has already taken the database: the
// Lease is acquired on the pass that reaches dispatch, before any Job exists.
// Clearing the claim without handing it back leaves every claimant on that
// database -- this one included, under the new operation ID its next Apply
// carries -- waiting out the full lease duration for a run that never started.
// Every refusal at the dispatch boundary hands the database back.
//
// The Lease is taken on the pass that reaches dispatch, before any Job exists,
// so a claim refused there holds a realm nothing is using. One row per refusal
// rather than one row for the mechanism: a release reached on one path says
// nothing about the other four, and it is the path a claim takes that decides
// whether it ever gets there.
func TestMigrationApplyRefusedAtDispatchHandsBackTheDatabase(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// jobs replaces the Job builder, for the refusals that are about
		// building rather than about the claim.
		jobs MigrationJobBuilder
		// refuse arranges the state that makes this pass refuse, and returns
		// the objects the API server holds besides the migration itself.
		refuse func(
			t *testing.T,
			migration *operatorv1alpha1.PtahMigration,
			plan *operatorv1alpha1.PtahMigrationPlan,
		) []client.Object
	}{
		{
			// Suspension is not a refusal for a running Apply -- that claim is
			// left to finish -- but a claim that has not dispatched yet is
			// retired here.
			name: "the resource was suspended before dispatch",
			refuse: func(_ *testing.T, migration *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				migration.Spec.Suspend = true
				return []client.Object{plan, verificationPolicyConfigMap()}
			},
		},
		{
			// connectTimeout is in the operation's input fingerprint and not in
			// the policy fingerprint, so this reaches the input check rather
			// than invalidating the plan on the way.
			name: "the operation inputs changed after the claim",
			refuse: func(_ *testing.T, migration *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				migration.Spec.Execution.ConnectTimeout = metav1.Duration{Duration: 47 * time.Second}
				return []client.Object{plan, verificationPolicyConfigMap()}
			},
		},
		{
			name: "the verification policy was replaced after the approval",
			refuse: func(_ *testing.T, _ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				replaced := verificationPolicyConfigMap()
				replaced.UID = "replacement-policy-uid"
				return []client.Object{plan, replaced}
			},
		},
		{
			// The plan is still there and still binds its policy, so this gets
			// past the checks above and refuses on the plan being on its way
			// out. A finalizer is what leaves a deletion timestamp to see.
			name: "the plan the claim named is being deleted",
			refuse: func(t *testing.T, _ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				t.Helper()
				plan.Finalizers = append(plan.Finalizers, "e2e.test/hold")
				plan.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC)}
				return []client.Object{plan, verificationPolicyConfigMap()}
			},
		},
		{
			// Nothing about the claim is wrong here; the Job it needs cannot
			// be built. The claim is retired all the same, and so is the
			// database it had taken.
			name: "the Job the claim needs cannot be built",
			jobs: failingMigrationBuildJobs{},
			refuse: func(_ *testing.T, _ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				return []client.Object{plan, verificationPolicyConfigMap()}
			},
		},
		{
			// A snapshot whose stored digest no longer matches its own
			// contents. This one is a failure rather than a discard -- the
			// claim is retired either way, and so is the database.
			name: "the persisted admission snapshot does not match its own contents",
			refuse: func(_ *testing.T, migration *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				ensureMigrationAdmissionSnapshot(migration)
				migration.Status.ActiveOperation.AdmissionSnapshot.TemplateDigest =
					"sha256:" + strings.Repeat("e", 64)
				return []client.Object{plan, verificationPolicyConfigMap()}
			},
		},
		{
			// The snapshot is persisted before the Job that carries its digest
			// exists, and the Job is rebuilt from the spec on the pass that
			// dispatches it. A snapshot that is internally consistent and
			// names another template means the two disagree about what would
			// be admitted.
			name: "the rebuilt Pod template differs from the admission snapshot",
			refuse: func(t *testing.T, migration *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) []client.Object {
				t.Helper()
				ensureMigrationAdmissionSnapshot(migration)
				snapshot := migration.Status.ActiveOperation.AdmissionSnapshot
				snapshot.TemplateDigest = "sha256:" + strings.Repeat("e", 64)
				// Re-stamped, or the snapshot is refused for disagreeing with
				// itself and never reaches the comparison this row is about.
				snapshot.Digest = ""
				digest, err := fingerprint.DigestCanonicalJSON(*snapshot)
				if err != nil {
					t.Fatal(err)
				}
				snapshot.Digest = digest
				return []client.Object{plan, verificationPolicyConfigMap()}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			operation := undispatchedApplyClaim(t, migration, plan)
			objects := append([]client.Object{migration}, row.refuse(t, migration, plan)...)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, objects...)
			if row.jobs != nil {
				reconciler.Jobs = row.jobs
			}

			actual := reconcileUntilTheApplyClaimIsGone(t, reconciler, api, migration)
			if actual.Status.ActiveOperation != nil {
				t.Fatalf("the Apply claim survived its refusal: %#v", actual.Status.ActiveOperation)
			}
			assertNoMigrationJobDispatched(t, api)

			// Another resource addressing the same database must be able to
			// take it immediately. Nothing ran, so nothing is left to
			// serialize against.
			other, err := reconciler.Locks.Acquire(context.Background(), targetlock.Request{
				CoordinationNamespace: reconciler.LockNamespace,
				CoordinationDigest:    operation.CoordinationDigest,
				Holder:                targetlock.Holder{SchemaUID: "other-resource", OperationID: "other-apply"},
				Duration:              time.Duration(operation.LeaseDurationSeconds) * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !other.Acquired {
				t.Fatalf("the database stayed held by a claim refused before it dispatched anything (phase %s)",
					actual.Status.Phase)
			}
		})
	}
}

// failingMigrationBuildJobs refuses to build a migration Job, which is how a
// dispatch fails for a reason that has nothing to do with the claim.
type failingMigrationBuildJobs struct{ fakeJobs }

func (failingMigrationBuildJobs) BuildMigration(
	*operatorv1alpha1.PtahMigration,
	operatorv1alpha1.MigrationOperationStatus,
	*operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, error) {
	return nil, errors.New("injected migration Job build failure")
}

// An uncertain run whose Job is already gone still has to say what ran.
//
// status.lastRun exists so a person can see what happened without the Job, and
// the commonest way a run becomes uncertain is the Job being missing -- which
// is when the claim is the only thing left that knows its name and UID. A
// record that named neither would send that person looking with nothing to
// search for.
func TestAnUncertainRunNamesTheJobFromItsClaimWhenTheJobIsGone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	operation.JobUID = "the-job-that-ran"
	// No Job object: this is the claim after its Job was collected.
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, migration)
	run := actual.Status.LastRun
	if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("last run = %#v, want the unknown outcome recorded", run)
	}
	if run.JobName != operation.JobName || run.JobUID != operation.JobUID {
		t.Fatalf("the record names Job %q/%q, want the one the claim dispatched %q/%q",
			run.JobName, run.JobUID, operation.JobName, operation.JobUID)
	}
	unresolved := actual.Status.UnresolvedRun
	if unresolved == nil || unresolved.JobName != operation.JobName || unresolved.JobUID != operation.JobUID {
		t.Fatalf("the unresolved record names Job %#v, want the one the claim dispatched", unresolved)
	}
}

// The mirror: a claim that started a dispatch and never recorded a UID names no
// Job at all. The name it reserved is not evidence that anything was created
// under it, and a record that named one would point a person at a Job that may
// never have existed.
func TestAnUncertainRunNamesNoJobWhenTheClaimRecordedNone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	operation.JobUID = ""
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run := readMigration(t, api, migration).Status.LastRun
	if run == nil {
		t.Fatal("a dispatch whose answer never came back recorded no run at all")
	}
	if run.JobName != "" || run.JobUID != "" {
		t.Fatalf("the record names Job %q/%q for a claim that never recorded one", run.JobName, run.JobUID)
	}
}

// A Job that took the reserved name after this claim's was gone is a later
// attempt, and it did not perform the run this record is about. Naming it
// would point whoever has to account for that run at the wrong execution.
func TestAnUncertainRunNamesItsOwnJobNotTheOneThatTookTheName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	// terminalMigrationWorkload records its Job on the claim, so the claim is
	// left naming the Job that ran. Then a different object is put under that
	// name, still running, so nothing about it settles this claim.
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	job.UID = "a-later-attempt"
	if operation.JobUID == job.UID || operation.JobUID == "" {
		t.Fatalf("the claim and the Job under its name are not distinguishable: claim %q, job %q",
			operation.JobUID, job.UID)
	}
	job.Status.Conditions = nil
	pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap(), job, pod)
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run := readMigration(t, api, migration).Status.LastRun
	if run == nil {
		t.Fatal("the uncertain run recorded nothing")
	}
	if run.JobUID != operation.JobUID {
		t.Fatalf("the record names Job %q, want the one this claim dispatched %q",
			run.JobUID, operation.JobUID)
	}
}

// A retried read-only attempt waits out spec.execution.failureRetryInterval.
//
// The delay is carried on the claim rather than only in the requeue that
// scheduled it. A manager that restarted, and a Job or watch event that arrives
// early, both re-enter reconciliation at once, and a delay that lived only in
// the queue would be lost with it -- which is how a failing operation becomes a
// tight loop against whatever it is failing on.
func TestARetriedMigrationOperationWaitsOutTheFailureInterval(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration := migrationFixture()
	migration.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: 90 * time.Second}
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Finalizers = []string{migrationOperationFinalizer}
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	// A Job that failed, which is what sends a read-only operation to a retry.
	job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, job, pod, verificationPolicyConfigMap())

	result, err := reconciler.Reconcile(ctx, migrationRequest(migration))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter < 80*time.Second {
		t.Fatalf("the retry was scheduled in %s, want about the 90s the resource asked for", result.RequeueAfter)
	}
	retried := readMigration(t, api, migration)
	claim := retried.Status.ActiveOperation
	if claim == nil || claim.RetryNotBefore == nil {
		t.Fatalf("the retried claim carries no not-before time: %#v", claim)
	}

	// The restart: a fresh reconciler and a fresh API server reading exactly
	// what was persisted, reconciling immediately as a watch event would.
	restarted, restartedAPI := fakeMigrationReconciler(t, staticLogs{}, retried, verificationPolicyConfigMap())
	// As many passes as the dispatch below needs, so "no Job" is a statement
	// about the deadline rather than about the Pod admission snapshot being
	// its own durable boundary. One pass would not dispatch either way.
	for pass := 0; pass < 3; pass++ {
		result, err := restarted.Reconcile(ctx, migrationRequest(migration))
		if err != nil {
			t.Fatalf("Reconcile() after restart error = %v", err)
		}
		if result.RequeueAfter < 80*time.Second {
			t.Fatalf("a restarted manager scheduled the retry in %s, want the remaining delay", result.RequeueAfter)
		}
		if after := jobNamesFor(t, restartedAPI, migration); len(after) != 0 {
			t.Fatalf("a Job was dispatched before the retry deadline: %v", after)
		}
	}

	// And after the deadline it goes.
	past := retried.DeepCopy()
	notBefore := metav1.NewTime(restarted.now().Add(-time.Second))
	past.Status.ActiveOperation.RetryNotBefore = &notBefore
	dispatching, dispatchingAPI := fakeMigrationReconciler(t, staticLogs{}, past, verificationPolicyConfigMap())
	// Two passes, because the Pod admission snapshot is its own durable
	// boundary: it is persisted before the Job that carries its digest exists.
	dispatched := false
	for pass := 0; pass < 3 && !dispatched; pass++ {
		if _, err := dispatching.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() past the deadline error = %v", err)
		}
		dispatched = len(jobNamesFor(t, dispatchingAPI, migration)) > 0
	}
	if !dispatched {
		probe := readMigration(t, dispatchingAPI, migration)
		t.Fatalf("no Job after the retry deadline passed: phase=%s claim=%#v",
			probe.Status.Phase, probe.Status.ActiveOperation)
	}
}

// jobNamesFor lists the Jobs standing in the resource's namespace.
func jobNamesFor(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) []string {
	t.Helper()

	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(migration.Namespace)); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		names = append(names, job.Name)
	}
	return names
}
