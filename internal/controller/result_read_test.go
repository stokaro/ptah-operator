package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// deadlineProbe answers a result read at once, and records the deadline the
// reconciler gave it.
type deadlineProbe struct {
	mu          sync.Mutex
	called      bool
	hasDeadline bool
	budget      time.Duration
}

func (p *deadlineProbe) Read(ctx context.Context, _, _, _ string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.called = true
	deadline, ok := ctx.Deadline()
	p.hasDeadline = ok
	if ok {
		p.budget = time.Until(deadline)
	}
	return nil, errors.New("the result transport failed")
}

func (p *deadlineProbe) observed() (called, hasDeadline bool, budget time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called, p.hasDeadline, p.budget
}

// stalledLogs is a result read that never answers. It returns only when its
// own deadline cancels it, which is the shape of a pod/log response the API
// server proxies from a kubelet that has stopped sending.
type stalledLogs struct {
	entered chan struct{}
	once    sync.Once
}

func newStalledLogs() *stalledLogs {
	return &stalledLogs{entered: make(chan struct{})}
}

func (s *stalledLogs) Read(ctx context.Context, _, _, _ string) ([]byte, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return nil, fmt.Errorf("read executor logs: %w", ctx.Err())
}

// Neither client-go's zero rest.Config.Timeout nor controller-runtime's
// opt-in reconcile timeout bounds the pod/log read, so the reconcile that
// makes it has to carry the bound itself. Both families make that read, and
// both are one worker.
func TestTheResultReadCarriesADeadline(t *testing.T) {
	t.Parallel()

	t.Run("migration", func(t *testing.T) {
		t.Parallel()

		migration := migrationFixture()
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
		migration.Finalizers = []string{migrationOperationFinalizer}
		migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
		job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
		probe := &deadlineProbe{}
		reconciler, _ := fakeMigrationReconciler(t, probe, migration, job, pod, verificationPolicyConfigMap())

		_, _ = reconciler.Reconcile(context.Background(), migrationRequest(migration))
		assertResultReadDeadline(t, probe)
	})

	t.Run("schema", func(t *testing.T) {
		t.Parallel()

		schema := schemaFixture()
		schema.Finalizers = []string{activeOperationFinalizer}
		schema.Status.Phase = operatorv1alpha1.PhaseResolving
		schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationResolve, ID: testDigest, InputFingerprint: testDigest,
			JobName: "resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
		}
		bindActiveInput(t, schema)
		job, pod := terminalWorkload(schema, batchv1.JobComplete)
		probe := &deadlineProbe{}
		reconciler, _ := fakeReconciler(t, probe, schema, job, pod)

		_, _ = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		assertResultReadDeadline(t, probe)
	})
}

func assertResultReadDeadline(t *testing.T, probe *deadlineProbe) {
	t.Helper()

	called, hasDeadline, budget := probe.observed()
	if !called {
		t.Fatal("the reconcile never reached the result reader")
	}
	if !hasDeadline {
		t.Fatal("the result reader was given no deadline, so this read has no time bound at all")
	}
	if budget <= 0 || budget > defaultResultReadTimeout {
		t.Fatalf("the read was given %s, want a positive budget no larger than %s", budget, defaultResultReadTimeout)
	}
}

// A read that never answers ends at its deadline and gives the worker back.
// One worker runs each family and leader election admits one manager, so a
// read with no bound is the whole family's reconciliation, not one resource's.
func TestAStalledResultReadEndsAtItsDeadline(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, _ := fakeMigrationReconciler(t, newStalledLogs(), migration, job, pod, verificationPolicyConfigMap())
	reconciler.ResultReadTimeout = 250 * time.Millisecond

	started := time.Now()
	err := reconcileWithin(t, reconciler, migration, 30*time.Second)
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error = %v, want the read's own deadline", err)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("the read ended after %s, before the bound it was given", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("the read ran for %s, so the bound did not end it", elapsed)
	}
}

// reconcileWithin runs one reconcile and refuses to wait forever for it.
//
// A read with no bound never returns, so a test that simply called Reconcile
// would hang until the package timeout and report ten minutes of the whole
// package instead of the one claim it makes.
func reconcileWithin(
	t *testing.T,
	reconciler *MigrationReconciler,
	migration *operatorv1alpha1.PtahMigration,
	limit time.Duration,
) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		_, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("the reconcile was still inside its result read after %s, so that read has no bound", limit)
		return nil
	}
}

// What a read timeout must not do: decide anything about the database.
//
// An Apply whose result could not be read is exactly as uncertain as it was
// before the read, so the claim, the Job it names and the Lease that keeps
// every other resource off that database all stay as they are, and nothing is
// recorded as having run or converged.
func TestAStalledResultReadKeepsTheApplyClaimAndItsLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, api := fakeMigrationReconciler(
		t, newStalledLogs(), migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	reconciler.ResultReadTimeout = 250 * time.Millisecond
	holdMigrationApplyLease(t, reconciler, api, migration)

	leaseName, err := targetlock.LeaseName(operation.CoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	leaseKey := client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}
	before := &coordinationv1.Lease{}
	if err := api.Get(ctx, leaseKey, before); err != nil {
		t.Fatal(err)
	}

	if err := reconcileWithin(t, reconciler, migration, 30*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error = %v, want the read's own deadline", err)
	}

	actual := readMigration(t, api, migration)
	claim := actual.Status.ActiveOperation
	if claim == nil {
		t.Fatal("an unreadable result discarded the Apply claim")
	}
	if claim.ID != operation.ID || claim.JobUID != operation.JobUID {
		t.Fatalf("the claim changed under an unreadable result: %#v", claim)
	}
	if actual.Status.LastRun != nil {
		t.Fatalf("an unreadable result recorded a run: %#v", actual.Status.LastRun)
	}
	if actual.Status.UnresolvedRun != nil {
		t.Fatalf("an unreadable result retired the claim as unresolved: %#v", actual.Status.UnresolvedRun)
	}

	after := &coordinationv1.Lease{}
	if err := api.Get(ctx, leaseKey, after); err != nil {
		t.Fatalf("the database Lease was released by an unreadable result: %v", err)
	}
	if after.Spec.HolderIdentity == nil || before.Spec.HolderIdentity == nil ||
		*after.Spec.HolderIdentity != *before.Spec.HolderIdentity {
		t.Fatalf("the Lease holder changed: before=%v after=%v", before.Spec.HolderIdentity, after.Spec.HolderIdentity)
	}
}

// The stall belongs to the read, not to the controller. While one resource is
// inside a result read that is not answering, another resource reconciles to
// completion against the same API. Kubernetes still schedules both onto one
// worker, which is why the read has a deadline; what this measures is that
// nothing else in the reconciler is held while it runs.
func TestAStalledResultReadHoldsNothingAnotherResourceNeeds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stalling := migrationFixture()
	stalling.Status.ExecutionBinding = migrationExecutionBinding()
	stalling.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	stalling.Finalizers = []string{migrationOperationFinalizer}
	migrationClaim(t, stalling, operatorv1alpha1.MigrationOperationResolve)
	job, pod := terminalMigrationWorkload(stalling, batchv1.JobComplete)

	other := migrationFixture()
	other.Name = "orders-second"
	other.UID = "orders-second-uid"
	// Its own database realm: two resources claiming one realm is a refusal of
	// its own, and this is about the read, not about the census.
	other.Spec.Target.CoordinationKey = "team-a/orders-secondary"
	other.Status.ExecutionBinding = migrationExecutionBinding()

	logs := newStalledLogs()
	reconciler, api := fakeMigrationReconciler(
		t, logs, stalling, job, pod, other, verificationPolicyConfigMap(),
	)
	reconciler.ResultReadTimeout = 5 * time.Second

	stalled := make(chan error, 1)
	go func() {
		_, err := reconciler.Reconcile(ctx, migrationRequest(stalling))
		stalled <- err
	}()
	select {
	case <-logs.entered:
	case err := <-stalled:
		t.Fatalf("the reconcile finished without reaching the result read: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the reconcile never reached the result read")
	}

	if _, err := reconciler.Reconcile(ctx, migrationRequest(other)); err != nil {
		t.Fatalf("the second resource could not be reconciled while a read stalled: %v", err)
	}
	// The first pass of a resource this controller has not seen adds its
	// finalizer, which is the smallest durable proof that the pass ran.
	actual := readMigration(t, api, other)
	if actual.Status.ActiveOperation == nil || !slices.Contains(actual.Finalizers, migrationOperationFinalizer) {
		t.Fatalf("the second resource was not reconciled: claim=%#v finalizers=%v",
			actual.Status.ActiveOperation, actual.Finalizers)
	}

	select {
	case err := <-stalled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the stalled reconcile ended with %v, want the read's own deadline", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stalled reconcile never ended, so its result read has no bound")
	}
}

// The read may not outlast the Lease the operation it is reading about holds.
//
// An Apply's Lease is spec.execution.activeDeadlineSeconds plus a minute, and
// the API accepts a deadline as low as 30 seconds. A reconcile inside the read
// is a reconcile that is not renewing that Lease, so a ceiling of two minutes
// would let the read run past the point where the realm is handed to whatever
// claims it next.
func TestTheResultReadNeverOutlastsTheOperationsLease(t *testing.T) {
	t.Parallel()

	const minute = time.Minute
	for _, row := range []struct {
		name        string
		configured  time.Duration
		leaseBudget time.Duration
		want        time.Duration
	}{
		{
			// The shortest Lease the API can produce: activeDeadlineSeconds
			// 30 plus a minute of grace. The ceiling sits below it on purpose,
			// so that a read beginning just after a renewal gives the worker
			// time to make the next one.
			name:        "the shortest Lease the API can produce",
			leaseBudget: leaseReadBudget(90),
			want:        defaultResultReadTimeout,
		},
		{
			name:        "the generated default, where the ceiling is what binds",
			leaseBudget: leaseReadBudget(960),
			want:        defaultResultReadTimeout,
		},
		{
			name:        "a budget exactly at the ceiling",
			leaseBudget: defaultResultReadTimeout,
			want:        defaultResultReadTimeout,
		},
		{
			name:        "an operation holding no Lease at all",
			leaseBudget: 0,
			want:        defaultResultReadTimeout,
		},
		{
			name:        "a shorter bound a caller asked for",
			configured:  5 * time.Second,
			leaseBudget: leaseReadBudget(960),
			want:        5 * time.Second,
		},
		{
			name:        "a caller's bound the Lease undercuts",
			configured:  minute,
			leaseBudget: leaseReadBudget(30),
			want:        30 * time.Second,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if actual := boundedResultReadTimeout(row.configured, row.leaseBudget); actual != row.want {
				t.Fatalf("boundedResultReadTimeout(%s, %s) = %s, want %s",
					row.configured, row.leaseBudget, actual, row.want)
			}
		})
	}
}

// The budget comes from the Lease the claim recorded, not from the spec that
// can be edited after it was taken.
//
// A post-Apply Observe renews at status.pendingObservation.leaseDurationSeconds,
// copied from the Apply. Raising spec.execution.activeDeadlineSeconds after
// that Apply started grows nothing about the Lease it holds, so a bound read
// off the spec would exceed it.
func TestTheReadBudgetComesFromTheLeaseTheClaimRecorded(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	// Raised long after the Apply took its Lease.
	schema.Spec.Execution.ActiveDeadlineSeconds = 900
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationObserve, LeaseDurationSeconds: 960,
	}
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		LeaseDurationSeconds: 90,
	}

	if actual := schemaResultReadBudget(schema); actual != 90*time.Second {
		t.Fatalf("the budget derived from the Apply's own Lease is %s, want its 90s", actual)
	}

	// With no pending observation the claim's own Lease is what binds.
	schema.Status.PendingObservation = nil
	if actual := schemaResultReadBudget(schema); actual != 16*time.Minute {
		t.Fatalf("the read budget is %s, want the claim's own Lease", actual)
	}

	// A read-only operation holds no Lease, so there is no budget to derive
	// and the ceiling is what applies.
	schema.Status.ActiveOperation.LeaseDurationSeconds = 0
	if actual := schemaResultReadBudget(schema); actual != 0 {
		t.Fatalf("an operation holding no Lease produced a budget of %s", actual)
	}
}

// And the reconcile hands the reader the claim's own Lease rather than the
// ceiling.
//
// Which operations hold a Lease is settled elsewhere -- a read-only one does
// not, and gets the ceiling -- so this sets the duration on the claim it
// reconciles and checks the number that reaches the reader.
func TestTheReconcileHandsTheReaderTheClaimsLease(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Execution.ActiveDeadlineSeconds = 900
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	// The shortest Lease the API can produce, and a spec that was raised long
	// after it was taken.
	operation.LeaseDurationSeconds = 90
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	probe := &deadlineProbe{}
	reconciler, _ := fakeMigrationReconciler(t, probe, migration, job, pod, verificationPolicyConfigMap())

	_, _ = reconciler.Reconcile(context.Background(), migrationRequest(migration))

	called, hasDeadline, budget := probe.observed()
	if !called || !hasDeadline {
		t.Fatalf("the reconcile gave the reader no deadline: called=%t hasDeadline=%t", called, hasDeadline)
	}
	if budget > defaultResultReadTimeout {
		t.Fatalf("the read was given %s, past the ceiling %s", budget, defaultResultReadTimeout)
	}
	// And enough to fetch a frame the protocol allows to be 48 MiB.
	if needed := time.Duration(runner.MaxResultLogBytes/(1<<20)) * time.Second; budget < needed {
		t.Fatalf("the read was given %s for a result that needs %s at the rate the ceiling assumes", budget, needed)
	}
}

// The shortest configuration the API accepts must still be able to read the
// largest result the protocol allows.
//
// The ceiling is derived from runner.MaxResultLogBytes at a floor of a
// mebibyte a second. A Lease-derived bound below that would mean a Plan that
// completed inside its execution deadline and wrote a valid maximum-size frame
// could never be read at that setting -- and since every retry is given the
// same budget over the same terminal log, never is exactly what it means.
func TestTheShortestLeaseStillClearsAMaximumResult(t *testing.T) {
	t.Parallel()

	needed := time.Duration(runner.MaxResultLogBytes/(1<<20)) * time.Second
	// activeDeadlineSeconds 30, the API minimum, plus the minute of grace.
	shortest := boundedResultReadTimeout(0, leaseReadBudget(90))
	if shortest < needed {
		t.Fatalf("the shortest configuration gets %s to read a result that needs %s, so no attempt at that setting could read one",
			shortest, needed)
	}
}

// The ceiling is the shortest Apply Lease the API can produce, because the
// worker blocked on a read is the worker that owes every other resource of
// this family its Lease renewals. A read-only operation holds no Lease of its
// own, and this is what bounds it.
func TestTheCeilingIsTheShortestLeaseTheFamilyCanOwe(t *testing.T) {
	t.Parallel()

	// activeDeadlineSeconds at its API minimum of 30, plus the grace.
	shortestLeaseOwed := leaseReadBudget(30 + 60)
	// Not merely inside it: a read beginning just after a renewal must still
	// give the worker time to make the next one.
	if headroom := shortestLeaseOwed - defaultResultReadTimeout; headroom < shortestLeaseOwed/3 {
		t.Fatalf("a read may hold the worker for %s of a %s Lease, leaving %s to renew it",
			defaultResultReadTimeout, shortestLeaseOwed, headroom)
	}
	// An unleased operation gets exactly that, and still clears a maximum result.
	unleased := boundedResultReadTimeout(0, 0)
	if unleased != defaultResultReadTimeout {
		t.Fatalf("an unleased read was given %s, want the ceiling %s", unleased, defaultResultReadTimeout)
	}
	if needed := time.Duration(runner.MaxResultLogBytes/(1<<20)) * time.Second; unleased < needed {
		t.Fatalf("an unleased read gets %s for a result that needs %s", unleased, needed)
	}
}
