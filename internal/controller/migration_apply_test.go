package controller

import (
	"context"
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
) (*MigrationReconciler, client.Client) {
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
