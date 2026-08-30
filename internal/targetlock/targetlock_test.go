package targetlock_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stokaro/ptah-operator/internal/targetlock"
)

func TestAcquireCreatesOwnerNeutralLease(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 123456000, time.FixedZone("test", 2*60*60))}
	locker, kubeClient := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")

	result, err := locker.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !result.Acquired || result.Contention != nil {
		t.Fatalf("Acquire() result = %#v, want acquired", result)
	}

	lease := getLease(t, kubeClient, request)
	if len(lease.Name) > 63 || !strings.HasPrefix(lease.Name, "ptah-t-") {
		t.Fatalf("Lease name %q is not a bounded Ptah target name", lease.Name)
	}
	if len(lease.OwnerReferences) != 0 {
		t.Fatalf("OwnerReferences = %#v, want owner-neutral Lease", lease.OwnerReferences)
	}
	if got := lease.Labels["operator.ptah.dev/coordination"]; got != "database-target" {
		t.Fatalf("coordination label = %q", got)
	}
	if lease.Spec.HolderIdentity == nil || !strings.HasPrefix(*lease.Spec.HolderIdentity, "ptah-h-") {
		t.Fatalf("HolderIdentity = %#v", lease.Spec.HolderIdentity)
	}
	if strings.Contains(*lease.Spec.HolderIdentity, request.Holder.OperationID) {
		t.Fatal("HolderIdentity disclosed the raw operation ID")
	}
	assertLeaseTimes(t, lease, clock.now, clock.now)
	if got := dereferenceInt32(lease.Spec.LeaseDurationSeconds); got != 30 {
		t.Fatalf("LeaseDurationSeconds = %d, want 30", got)
	}
	if got := dereferenceInt32(lease.Spec.LeaseTransitions); got != 0 {
		t.Fatalf("LeaseTransitions = %d, want 0", got)
	}
}

func TestAcquireReturnsTypedContentionUntilExpiration(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, kubeClient := newLocker(t, clock)
	first := lockRequest("target-a", "operation-a")
	mustAcquire(t, locker, first)
	original := getLease(t, kubeClient, first)

	clock.now = clock.now.Add(11 * time.Second)
	second := lockRequest("target-a", "operation-b")
	result, err := locker.Acquire(context.Background(), second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if result.Acquired || result.Contention == nil {
		t.Fatalf("Acquire() result = %#v, want contention", result)
	}
	if got, want := result.Contention.RequeueAfter, 19*time.Second; got != want {
		t.Fatalf("RequeueAfter = %s, want %s", got, want)
	}

	unchanged := getLease(t, kubeClient, first)
	if dereferenceString(unchanged.Spec.HolderIdentity) != dereferenceString(original.Spec.HolderIdentity) {
		t.Fatal("contending acquisition changed the holder")
	}
	assertLeaseTimes(t, unchanged, time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC))
}

func TestAcquireTakesOverExpiredLease(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: started}
	locker, kubeClient := newLocker(t, clock)
	first := lockRequest("target-a", "operation-a")
	mustAcquire(t, locker, first)
	firstHolder := dereferenceString(getLease(t, kubeClient, first).Spec.HolderIdentity)

	clock.now = started.Add(30 * time.Second)
	second := lockRequest("target-a", "operation-b")
	mustAcquire(t, locker, second)

	lease := getLease(t, kubeClient, second)
	if got := dereferenceString(lease.Spec.HolderIdentity); got == firstHolder || got == "" {
		t.Fatalf("HolderIdentity = %q, want new holder", got)
	}
	if got := dereferenceInt32(lease.Spec.LeaseTransitions); got != 1 {
		t.Fatalf("LeaseTransitions = %d, want 1", got)
	}
	assertLeaseTimes(t, lease, clock.now, clock.now)
}

func TestAcquireRenewsSameHolder(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: started}
	locker, kubeClient := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	mustAcquire(t, locker, request)

	clock.now = started.Add(20 * time.Second)
	request.Duration = 45 * time.Second
	mustAcquire(t, locker, request)

	lease := getLease(t, kubeClient, request)
	assertLeaseTimes(t, lease, started, clock.now)
	if got := dereferenceInt32(lease.Spec.LeaseDurationSeconds); got != 45 {
		t.Fatalf("LeaseDurationSeconds = %d, want 45", got)
	}
	if got := dereferenceInt32(lease.Spec.LeaseTransitions); got != 0 {
		t.Fatalf("LeaseTransitions = %d, want 0", got)
	}
}

func TestStaleReleaserCannotClearNewHolder(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: started}
	locker, kubeClient := newLocker(t, clock)
	stale := lockRequest("target-a", "operation-a")
	mustAcquire(t, locker, stale)

	clock.now = started.Add(31 * time.Second)
	current := lockRequest("target-a", "operation-b")
	mustAcquire(t, locker, current)
	wantedHolder := dereferenceString(getLease(t, kubeClient, current).Spec.HolderIdentity)

	if err := locker.Release(context.Background(), stale); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	lease := getLease(t, kubeClient, current)
	if got := dereferenceString(lease.Spec.HolderIdentity); got != wantedHolder {
		t.Fatalf("HolderIdentity = %q, want current holder %q", got, wantedHolder)
	}
}

func TestReleaseClearsOnlyMatchingHolderAndToleratesMissingLease(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, kubeClient := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")

	if err := locker.Release(context.Background(), request); err != nil {
		t.Fatalf("Release() missing Lease error = %v", err)
	}
	mustAcquire(t, locker, request)
	clock.now = clock.now.Add(time.Second)
	if err := locker.Release(context.Background(), request); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	lease := getLease(t, kubeClient, request)
	if got := dereferenceString(lease.Spec.HolderIdentity); got != "" {
		t.Fatalf("HolderIdentity = %q, want empty", got)
	}
}

func TestInvalidDigestIsRejectedWithoutEchoingInput(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Now()}
	locker, _ := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	request.TargetIdentityDigest = "postgres://user:super-secret@database/example"

	_, err := locker.Acquire(context.Background(), request)
	if !errors.Is(err, targetlock.ErrInvalidRequest) {
		t.Fatalf("Acquire() error = %v, want ErrInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), request.TargetIdentityDigest) {
		t.Fatalf("Acquire() error disclosed invalid input: %v", err)
	}
}

func TestLeaseDurationAllowsMaximumJobDeadlineWithGrace(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, _ := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	request.Duration = 86460 * time.Second

	result, err := locker.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !result.Acquired {
		t.Fatalf("Acquire() result = %#v, want acquired", result)
	}
}

func TestLeaseDurationRejectsValueAboveMaximum(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, _ := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	request.Duration = targetlock.MaxLeaseDuration + time.Second

	_, err := locker.Acquire(context.Background(), request)
	if !errors.Is(err, targetlock.ErrInvalidRequest) {
		t.Fatalf("Acquire() error = %v, want ErrInvalidRequest", err)
	}
}

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func newLocker(t *testing.T, clock *fakeClock) (*targetlock.Locker, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return targetlock.New(kubeClient, kubeClient, clock), kubeClient
}

func lockRequest(target, operation string) targetlock.Request {
	sum := sha256.Sum256([]byte(target))
	return targetlock.Request{
		CoordinationNamespace: "ptah-system",
		TargetIdentityDigest:  "sha256:" + hex.EncodeToString(sum[:]),
		Holder: targetlock.Holder{
			SchemaUID:   types.UID("c78d1c70-8f66-4c07-a49b-4ee621dc2280"),
			OperationID: operation,
		},
		Duration: 30 * time.Second,
	}
}

func mustAcquire(t *testing.T, locker *targetlock.Locker, request targetlock.Request) {
	t.Helper()
	result, err := locker.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !result.Acquired || result.Contention != nil {
		t.Fatalf("Acquire() result = %#v, want acquired", result)
	}
}

func getLease(t *testing.T, kubeClient client.Client, request targetlock.Request) *coordinationv1.Lease {
	t.Helper()
	name, err := targetlock.LeaseName(request.TargetIdentityDigest)
	if err != nil {
		t.Fatalf("LeaseName() error = %v", err)
	}
	lease := &coordinationv1.Lease{}
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: request.CoordinationNamespace, Name: name}, lease); err != nil {
		t.Fatalf("Get(Lease) error = %v", err)
	}
	return lease
}

func assertLeaseTimes(t *testing.T, lease *coordinationv1.Lease, acquired, renewed time.Time) {
	t.Helper()
	if lease.Spec.AcquireTime == nil || !lease.Spec.AcquireTime.Time.Equal(acquired) {
		t.Fatalf("AcquireTime = %#v, want %s", lease.Spec.AcquireTime, acquired)
	}
	if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(renewed) {
		t.Fatalf("RenewTime = %#v, want %s", lease.Spec.RenewTime, renewed)
	}
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func dereferenceInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}
