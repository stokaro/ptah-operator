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
