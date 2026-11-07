package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// failingApprovalList refuses a List of migration approvals while fail holds.
type failingApprovalList struct {
	client.Reader
	fail *atomic.Bool
}

func (f failingApprovalList) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*operatorv1alpha1.PtahMigrationApprovalList); ok && f.fail.Load() {
		return errors.New("etcdserver: request timed out")
	}
	return f.Reader.List(ctx, list, options...)
}

// staleCache is a cache that has not seen any migration approval yet, as one
// has not right after admission stored it.
type staleCache struct{ client.Reader }

func (c staleCache) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if approvals, ok := list.(*operatorv1alpha1.PtahMigrationApprovalList); ok {
		approvals.Items = nil
		return nil
	}
	return c.Reader.List(ctx, list, options...)
}

// answerFrame puts a result frame where the controller reads it, for a test that
// reconciles by hand rather than through settle.
func answerFrame(t *testing.T, harness *migrationLifecycle, result runner.Result) {
	t.Helper()
	operation := harness.migration().Status.ActiveOperation
	if operation == nil {
		t.Fatal("no claim to answer")
	}
	result.ProtocolVersion = runner.ProtocolVersion
	result.OperationID = operation.ID
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	harness.logs.content = frame
}

func reconcileOnce(harness *migrationLifecycle) error {
	_, err := harness.reconciler.Reconcile(context.Background(), migrationRequest(harness.migrationObject()))
	return err
}

func twoPending() dataplane.MigrationStatusReport {
	report := pendingReport()
	report.TotalMigrations = 2
	report.Migrations = append(report.Migrations, dataplane.MigrationRecord{
		Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStatePending,
	})
	return report
}

func approvalNamed(
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
	name string,
) *operatorv1alpha1.PtahMigrationApproval {
	approval := migrationApprovalFor(migration, plan)
	approval.Name, approval.UID = name, types.UID(name+"-uid")
	return approval
}

func createApprovals(t *testing.T, api client.Client, approvals ...*operatorv1alpha1.PtahMigrationApproval) {
	t.Helper()
	for _, approval := range approvals {
		if err := api.Create(context.Background(), approval); err != nil {
			t.Fatal(err)
		}
	}
}

func isStale(t *testing.T, api client.Client, approval *operatorv1alpha1.PtahMigrationApproval) bool {
	t.Helper()
	return meta.IsStatusConditionTrue(readMigrationApproval(t, api, approval).Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
}

// readToTheHistory answers a due read up to its History claim, which it leaves
// unanswered.
func readToTheHistory(harness *migrationLifecycle, clock *fixedClock) {
	harness.t.Helper()
	harness.dueNow(clock)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
}

// A pass that stops between the history's write and the plan's leaves Planning
// with the plan it began with and an interval to wait. Under OnApproval that is
// the gate closed for the interval, since admission takes an approval only at
// AwaitingApproval. The next pass reads again instead, once, and the read ends at
// the gate.
func TestAPublicationThePassDidNotFinishIsReadAgainAtOnce(t *testing.T) {
	t.Parallel()

	harness, clock, plan := waitingForAnApproval(t)
	readToTheHistory(harness, clock)
	var fail atomic.Bool
	fail.Store(true)
	harness.reconciler.APIReader = failingApprovalList{Reader: harness.api, fail: &fail}
	answerFrame(t, harness, historyAnswer(harness, pendingReport()))
	if err := reconcileOnce(harness); err == nil {
		t.Fatal("the failed List was not returned")
	}
	stopped := harness.migration()
	progressing := meta.FindStatusCondition(stopped.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if stopped.Status.Phase != operatorv1alpha1.MigrationPhasePlanning || stopped.Status.ActiveOperation != nil ||
		progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonMigrationsPending) ||
		stopped.Status.Plan == nil || stopped.Status.Plan.UID != plan.UID ||
		stopped.Status.NextReconciliationTime == nil || !stopped.Status.NextReconciliationTime.After(clock.now) {
		t.Fatalf("the fixture is not a publication left between its writes: phase %s, claim %#v, Progressing %#v, plan %#v, next %v",
			stopped.Status.Phase, stopped.Status.ActiveOperation, progressing, stopped.Status.Plan, stopped.Status.NextReconciliationTime)
	}
	fail.Store(false)

	// One claim, and no more however often the resource is reconciled before its
	// Job ends: the claim rewrote what asked for it.
	var claimed *operatorv1alpha1.MigrationOperationStatus
	for pass := range 4 {
		if err := reconcileOnce(harness); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		operation := harness.migration().Status.ActiveOperation
		if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationResolve {
			t.Fatalf("pass %d claimed %#v, want a Resolve before the interval", pass, operation)
		}
		if claimed != nil && (operation.ID != claimed.ID || operation.Attempt != claimed.Attempt) {
			t.Fatalf("pass %d claimed again: %#v after %#v", pass, operation, claimed)
		}
		claimed = operation
	}
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(historyAnswer(harness, pendingReport()))
	open := harness.migration()
	if open.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval || open.Status.ActiveOperation != nil ||
		open.Status.Plan == nil || open.Status.Plan.UID != plan.UID {
		t.Fatalf("phase %s, claim %#v, plan %#v, want the same plan back at the gate",
			open.Status.Phase, open.Status.ActiveOperation, open.Status.Plan)
	}
	wantMigrationCondition(t, open, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonAwaitingApproval)
}

// Planning is where a plan rests under apply policy Never, and that rest is
// not a publication left unfinished: nothing reads again before the interval.
func TestAPlanThePolicyHoldsIsNotReadAgainBeforeItsInterval(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyNever
	harness := newMigrationLifecycle(t, migration)
	movableMigrationClock(harness.reconciler, harness.api)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(historyAnswer(harness, pendingReport()))
	resting := harness.migration()
	if resting.Status.Phase != operatorv1alpha1.MigrationPhasePlanning || resting.Status.Plan == nil {
		t.Fatalf("phase %s, plan %#v, want a plan resting under Never", resting.Status.Phase, resting.Status.Plan)
	}
	wantMigrationCondition(t, resting, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyDisabled)
	for pass := range 3 {
		if err := reconcileOnce(harness); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if again := harness.migration(); again.Status.ActiveOperation != nil || again.ResourceVersion != resting.ResourceVersion {
			t.Fatalf("pass %d before the interval wrote %s -> %s and claimed %#v",
				pass, resting.ResourceVersion, again.ResourceVersion, again.Status.ActiveOperation)
		}
	}
}

// A mark that fails part way ends the pass before the write it comes ahead of,
// so the next pass harvests the same answer and finishes it, and each approval
// is counted once. On the refusal and on a reading that left no plan the claim
// is still there; on a published plan the status does not yet name it.
func TestAMarkThatFailsLeavesTheReadToFinishIt(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"refusal", "replaced plan", "nothing left"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			harness, clock, plan := waitingForAnApproval(t)
			observed := &telemetryObservation{}
			harness.reconciler.Telemetry = observed
			waiting := harness.migration()
			first, second := approvalNamed(waiting, plan, "a-first"), approvalNamed(waiting, plan, "b-second")
			createApprovals(t, harness.api, first, second)
			harness.dueNow(clock)
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)

			conflict := errors.New("the object has been modified")
			var fails atomic.Int32
			fails.Store(1)
			api := harness.api
			harness.reconciler.Client = interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(
					ctx context.Context, inner client.Client, subResource string, object client.Object, patch client.Patch,
					options ...client.SubResourcePatchOption,
				) error {
					if approval, ok := object.(*operatorv1alpha1.PtahMigrationApproval); ok && approval.Name == second.Name && fails.Add(-1) >= 0 {
						return conflict
					}
					return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
				},
			})
			switch path {
			case "refusal":
				answerFrame(t, harness, refusedBy(testDigest))
			default:
				harness.answer(acceptedBy(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationHistory)
				report := inSyncReport()
				if path == "replaced plan" {
					report = twoPending()
				}
				answerFrame(t, harness, historyAnswer(harness, report))
			}
			if err := reconcileOnce(harness); !errors.Is(err, conflict) {
				t.Fatalf("first pass err = %v, want the conflict", err)
			}
			if !isStale(t, api, first) || isStale(t, api, second) {
				t.Fatal("the fixture is not a mark that failed part way")
			}
			stopped := harness.migration()
			switch path {
			case "replaced plan":
				if stopped.Status.Plan == nil || stopped.Status.Plan.UID != plan.UID || stopped.Status.Phase != operatorv1alpha1.MigrationPhasePlanning {
					t.Fatalf("phase %s, plan %#v: the status named the new plan before every other approval was withdrawn",
						stopped.Status.Phase, stopped.Status.Plan)
				}
				harness.claimed(operatorv1alpha1.MigrationOperationResolve)
				harness.answer(resolvedTo(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationVerify)
				harness.answer(acceptedBy(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationHistory)
				harness.answer(historyAnswer(harness, twoPending()))
			default:
				if stopped.Status.ActiveOperation == nil {
					t.Fatalf("phase %s with no claim: the claim went before every approval was withdrawn", stopped.Status.Phase)
				}
				harness.settle()
			}
			for _, approval := range []*operatorv1alpha1.PtahMigrationApproval{first, second} {
				if !isStale(t, api, approval) {
					t.Fatalf("%s is not stale after the retry", approval.Name)
				}
			}
			if count := staleCount(observed); count != 2 {
				t.Fatalf("stale counted %d times in %v, want 2", count, observed.approvals)
			}
		})
	}
}

// The writer lists approvals from the API server, and only those that can still
// authorize a run. Each row holds one of those to an approval it has to leave as
// it is, beside one it has to withdraw, so a row that marks nothing fails too.
func TestTheApprovalsARefusalWithdraws(t *testing.T) {
	t.Parallel()

	refusal := refusedBy(testDigest)
	plan := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "ptah-mplan-previous", UID: types.UID("previous-plan-uid")},
	}

	t.Run("one the cache has not seen yet", func(t *testing.T) {
		t.Parallel()
		migration, reconciler, api, _ := verifyingMigration(t, refusal)
		fresh := approvalNamed(migration, plan, "fresh")
		createApprovals(t, api, fresh)
		reconciler.CachedReader = staleCache{Reader: api}
		if err := reconcileMigrationOnce(reconciler, migration); err != nil {
			t.Fatal(err)
		}
		if !isStale(t, api, fresh) {
			t.Fatal("an approval the cache had not seen was left able to authorize a run")
		}
	})

	t.Run("one deleted before its mark", func(t *testing.T) {
		t.Parallel()
		migration, reconciler, api, _ := verifyingMigration(t, refusal)
		gone, kept := approvalNamed(migration, plan, "a-gone"), approvalNamed(migration, plan, "b-kept")
		createApprovals(t, api, gone, kept)
		observed := &telemetryObservation{}
		reconciler.Telemetry = observed
		reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, inner client.Client, subResource string, object client.Object, patch client.Patch,
				options ...client.SubResourcePatchOption,
			) error {
				if object.GetName() == gone.Name {
					return apierrors.NewNotFound(operatorv1alpha1.GroupVersion.WithResource("ptahmigrationapprovals").GroupResource(), gone.Name)
				}
				return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
			},
		})
		if err := reconcileMigrationOnce(reconciler, migration); err != nil {
			t.Fatalf("a deleted approval failed the refusal: %v", err)
		}
		if !isStale(t, api, kept) || readMigration(t, api, migration).Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
			t.Fatal("the refusal stopped at an approval that was gone")
		}
		if count := staleCount(observed); count != 1 {
			t.Fatalf("stale counted %d times, want the one approval marked", count)
		}
	})

	t.Run("one being deleted", func(t *testing.T) {
		t.Parallel()
		migration, reconciler, api, _ := verifyingMigration(t, refusal)
		deleting, kept := approvalNamed(migration, plan, "a-deleting"), approvalNamed(migration, plan, "b-kept")
		deleting.Finalizers = []string{"operator.ptah.run/test-hold"}
		createApprovals(t, api, deleting, kept)
		if err := api.Delete(context.Background(), deleting); err != nil {
			t.Fatal(err)
		}
		if readMigrationApproval(t, api, deleting).DeletionTimestamp == nil {
			t.Fatal("the fixture is not an approval being deleted")
		}
		observed := &telemetryObservation{}
		reconciler.Telemetry = observed
		if err := reconcileMigrationOnce(reconciler, migration); err != nil {
			t.Fatal(err)
		}
		if isStale(t, api, deleting) || !isStale(t, api, kept) {
			t.Fatalf("deleting stale %v, kept stale %v, want only the one that can still authorize a run marked",
				isStale(t, api, deleting), isStale(t, api, kept))
		}
		if count := staleCount(observed); count != 1 {
			t.Fatalf("stale counted %d times, want 1", count)
		}
	})

	// Another writer between the List and the mark: here a run that spends the
	// approval. A mark without the resourceVersion it read would replace the
	// conditions that writer added.
	t.Run("one another writer changed after the list", func(t *testing.T) {
		t.Parallel()
		migration, reconciler, api, _ := verifyingMigration(t, refusal)
		spent := approvalNamed(migration, plan, "spent")
		createApprovals(t, api, spent)
		var raced atomic.Bool
		reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, inner client.Client, subResource string, object client.Object, patch client.Patch,
				options ...client.SubResourcePatchOption,
			) error {
				if _, ok := object.(*operatorv1alpha1.PtahMigrationApproval); ok && raced.CompareAndSwap(false, true) {
					current := &operatorv1alpha1.PtahMigrationApproval{}
					if err := inner.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
						return err
					}
					meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
						Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
						Reason: string(operatorv1alpha1.ReasonDispatchCommitted), Message: "The approved plan was dispatched",
						LastTransitionTime: metav1.Now(),
					})
					if err := inner.Status().Update(ctx, current); err != nil {
						return err
					}
				}
				return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
			},
		})
		if err := reconcileMigrationOnce(reconciler, migration); !apierrors.IsConflict(err) {
			t.Fatalf("err = %v, want the conflict a stale base answers", err)
		}
		if err := reconcileMigrationOnce(reconciler, migration); err != nil {
			t.Fatal(err)
		}
		stored := readMigrationApproval(t, api, spent)
		if !meta.IsStatusConditionTrue(stored.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) ||
			meta.IsStatusConditionTrue(stored.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
			t.Fatalf("conditions = %#v, want the run's Consumed kept and no mark on a spent approval", stored.Status.Conditions)
		}
	})
}

func reconcileMigrationOnce(reconciler *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
	_, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
	return err
}

// A plan that waited for a person and a reading that leaves no plan: nothing is
// left to approve, Ready and ApprovalRequired say so together, and the next plan
// that waits counts as a decision owed again.
func TestAReadingThatLeavesNoPlanLeavesNothingToApprove(t *testing.T) {
	t.Parallel()

	dirty := func() dataplane.MigrationStatusReport {
		report := inSyncReport()
		report.DirtyRevision = &dataplane.MigrationDirty{Version: 1, Applied: 1, Total: 2}
		return report
	}
	for _, row := range []struct {
		name   string
		report func() dataplane.MigrationStatusReport
		reason operatorv1alpha1.ConditionReason
	}{
		{"a reading in sync", inSyncReport, operatorv1alpha1.ReasonHistoryMatched},
		{"a reading a person has to repair", dirty, operatorv1alpha1.ReasonHistoryDirty},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			harness, clock, _ := waitingForAnApproval(t)
			observed := &telemetryObservation{}
			harness.reconciler.Telemetry = observed
			readToTheHistory(harness, clock)
			harness.answer(historyAnswer(harness, row.report()))
			left := harness.migration()
			if left.Status.Plan != nil {
				t.Fatalf("plan = %#v, want none", left.Status.Plan)
			}
			wantMigrationCondition(t, left, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, row.reason)

			readToTheHistory(harness, clock)
			harness.answer(historyAnswer(harness, twoPending()))
			waiting := harness.migration()
			if waiting.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval || waiting.Status.Plan == nil {
				t.Fatalf("phase %s, plan %#v, want another plan waiting", waiting.Status.Phase, waiting.Status.Plan)
			}
			required := 0
			for _, outcome := range observed.approvals {
				if outcome == telemetry.ApprovalRequired {
					required++
				}
			}
			if required != 1 {
				t.Fatalf("approval observations = %v, want the second plan's decision counted once", observed.approvals)
			}
		})
	}

	t.Run("a resource that never had the condition", func(t *testing.T) {
		t.Parallel()
		harness := newMigrationLifecycle(t, migrationFixture())
		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		harness.answer(resolvedTo(testDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		harness.answer(acceptedBy(testDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationHistory)
		harness.answer(historyAnswer(harness, inSyncReport()))
		if condition := meta.FindStatusCondition(harness.migration().Status.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired); condition != nil {
			t.Fatalf("ApprovalRequired = %#v, want none written where no plan ever waited", condition)
		}
	})
}

// An approval authorizes one run of its plan. A run that spends one withdraws the
// plan's others, so a run that fails with nothing applied is not run again under a
// second approval when the read after it ends at the same plan, with nobody having
// seen the failure. An approval of another plan is not the plan's.
func TestAnApprovalSpentOnARunWithdrawsTheOthersOfItsPlan(t *testing.T) {
	t.Parallel()

	failedRun := func(harness *migrationLifecycle) runner.Result {
		return runner.Result{
			Operation: runner.OperationMigrationApply, ChildExitCode: 1,
			CoordinationDigest: harness.coordinationDigest(), TargetIdentityDigest: testDigest,
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up", Outcome: dataplane.MigrationOutcomeFailed,
				Error: &dataplane.CommandError{Message: "refused"},
			},
			Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
		}
	}
	spentAndWithdrawn := func(t *testing.T, harness *migrationLifecycle, first, second, other *operatorv1alpha1.PtahMigrationApproval) {
		t.Helper()
		spent := readMigrationApproval(t, harness.api, first)
		if !meta.IsStatusConditionTrue(spent.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) ||
			meta.IsStatusConditionTrue(spent.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
			t.Fatalf("the spent approval reads %#v", spent.Status.Conditions)
		}
		duplicate := meta.FindStatusCondition(readMigrationApproval(t, harness.api, second).Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
		if duplicate == nil || duplicate.Status != metav1.ConditionTrue || duplicate.Reason != string(operatorv1alpha1.ReasonSupersededApproval) {
			t.Fatalf("the plan's other approval reads Stale = %#v, want True/SupersededApproval", duplicate)
		}
		if isStale(t, harness.api, other) {
			t.Fatal("an approval of another plan was withdrawn")
		}
	}

	t.Run("a run that fails with nothing applied", func(t *testing.T) {
		t.Parallel()

		harness, clock, plan := waitingForAnApproval(t)
		observed := &telemetryObservation{}
		harness.reconciler.Telemetry = observed
		waiting := harness.migration()
		first, second := approvalNamed(waiting, plan, "a-first"), approvalNamed(waiting, plan, "b-second")
		first.CreationTimestamp = metav1.NewTime(first.CreationTimestamp.Add(-1e9))
		other := approvalNamed(waiting, plan, "c-other")
		other.Spec.PlanRef = operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-other", UID: "other-plan-uid"}
		createApprovals(t, harness.api, first, second, other)

		harness.claimed(operatorv1alpha1.MigrationOperationApply)
		if claim := harness.migration().Status.ActiveOperation; claim.ApprovalRef == nil || claim.ApprovalRef.UID != first.UID {
			t.Fatalf("the claim spends %#v, want the oldest approval", claim.ApprovalRef)
		}
		spentAndWithdrawn(t, harness, first, second, other)
		if count := staleCount(observed); count != 1 {
			t.Fatalf("stale counted %d times in %v at the claim, want the one duplicate", count, observed.approvals)
		}
		harness.answer(failedRun(harness))

		harness.dueNow(clock)
		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		harness.answer(resolvedTo(testDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		harness.answer(acceptedBy(testDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationHistory)
		harness.answer(historyAnswer(harness, pendingReport()))
		harness.settle()
		after := harness.migration()
		if after.Status.Plan == nil || after.Status.Plan.UID != plan.UID {
			t.Fatalf("plan = %#v, want the same plan back, which is the case this row is for", after.Status.Plan)
		}
		if operation := after.Status.ActiveOperation; operation != nil {
			t.Fatalf("claimed %#v after a failed run, with nobody having decided again", operation)
		}
		if after.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
			t.Fatalf("phase = %q, want the plan waiting for a person", after.Status.Phase)
		}
		// The read ended at this plan, which withdraws the approval of the other
		// one, as any read does.
		if !isStale(t, harness.api, other) || staleCount(observed) != 2 {
			t.Fatalf("approval observations = %v, want the duplicate and the other plan's approval stale", observed.approvals)
		}
	})

	// The duplicates go after the approval is spent, and a pass that stops in
	// between finishes them from an approval already spent, counting it once.
	t.Run("a mark that fails after the approval is spent", func(t *testing.T) {
		t.Parallel()

		harness, _, plan := waitingForAnApproval(t)
		observed := &telemetryObservation{}
		harness.reconciler.Telemetry = observed
		waiting := harness.migration()
		first, second := approvalNamed(waiting, plan, "a-first"), approvalNamed(waiting, plan, "b-second")
		first.CreationTimestamp = metav1.NewTime(first.CreationTimestamp.Add(-1e9))
		other := approvalNamed(waiting, plan, "c-other")
		other.Spec.PlanRef = operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-other", UID: "other-plan-uid"}
		createApprovals(t, harness.api, first, second, other)

		conflict := errors.New("the object has been modified")
		var fails atomic.Int32
		fails.Store(1)
		api := harness.api
		harness.reconciler.Client = interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, inner client.Client, subResource string, object client.Object, patch client.Patch,
				options ...client.SubResourcePatchOption,
			) error {
				if object.GetName() == second.Name && fails.Add(-1) >= 0 {
					return conflict
				}
				return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
			},
		})
		var failed bool
		for pass := 0; pass < 12 && !failed; pass++ {
			failed = errors.Is(reconcileOnce(harness), conflict)
		}
		if !failed {
			t.Fatal("the mark never failed, so this row measured nothing")
		}
		if claim := harness.migration().Status.ActiveOperation; claim == nil || claim.DispatchStarted {
			t.Fatalf("claim = %#v, want the Apply held before dispatch", claim)
		}
		harness.claimed(operatorv1alpha1.MigrationOperationApply)
		spentAndWithdrawn(t, harness, first, second, other)
		accepted := 0
		for _, outcome := range observed.approvals {
			if outcome == telemetry.ApprovalAccepted {
				accepted++
			}
		}
		if accepted != 1 || staleCount(observed) != 1 {
			t.Fatalf("approval observations = %v, want one accepted and one stale", observed.approvals)
		}
	})
}
