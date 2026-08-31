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
	if got, want := result.Contention.RequeueAfter, 30*time.Second; got != want {
		t.Fatalf("RequeueAfter = %s, want %s", got, want)
	}

	unchanged := getLease(t, kubeClient, first)
	if dereferenceString(unchanged.Spec.HolderIdentity) != dereferenceString(original.Spec.HolderIdentity) {
		t.Fatal("contending acquisition changed the holder")
	}
	assertLeaseTimes(t, unchanged, time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC))
}

func TestAcquireTakesOverOnlyAfterLocallyObservedLeaseDuration(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: started}
	locker, kubeClient := newLocker(t, clock)
	first := lockRequest("target-a", "operation-a")
	mustAcquire(t, locker, first)
	firstHolder := dereferenceString(getLease(t, kubeClient, first).Spec.HolderIdentity)

	clock.now = started.Add(30 * time.Second)
	second := lockRequest("target-a", "operation-b")
	mustObserveContention(t, locker, second, 30*time.Second)
	clock.now = clock.now.Add(30 * time.Second)
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

func TestAcquireFailsClosedOnMalformedForeignLease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*coordinationv1.Lease)
	}{
		{
			name: "missing duration",
			mutate: func(lease *coordinationv1.Lease) {
				lease.Spec.LeaseDurationSeconds = nil
			},
		},
		{
			name: "nonpositive duration",
			mutate: func(lease *coordinationv1.Lease) {
				zero := int32(0)
				lease.Spec.LeaseDurationSeconds = &zero
			},
		},
		{
			name: "missing timestamps",
			mutate: func(lease *coordinationv1.Lease) {
				lease.Spec.RenewTime = nil
				lease.Spec.AcquireTime = nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
			locker, kubeClient := newLocker(t, clock)
			first := lockRequest("target-a", "operation-a")
			mustAcquire(t, locker, first)
			lease := getLease(t, kubeClient, first)
			originalHolder := dereferenceString(lease.Spec.HolderIdentity)
			test.mutate(lease)
			if err := kubeClient.Update(context.Background(), lease); err != nil {
				t.Fatalf("Update(malformed Lease) error = %v", err)
			}

			second := lockRequest("target-a", "operation-b")
			result, err := locker.Acquire(context.Background(), second)
			if !errors.Is(err, targetlock.ErrMalformedLease) {
				t.Fatalf("Acquire() result = %#v, error = %v, want ErrMalformedLease", result, err)
			}
			if got := dereferenceString(getLease(t, kubeClient, first).Spec.HolderIdentity); got != originalHolder {
				t.Fatalf("malformed foreign Lease holder = %q, want unchanged %q", got, originalHolder)
			}
		})
	}
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

func TestAcquireNeverAdoptsExpectedEpochForNewLease(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, _ := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	request.ExpectedEpoch = "v1-11111111111111111111111111111111"

	result := mustAcquireResult(t, locker, request)
	if result.Epoch == request.ExpectedEpoch || !result.ContinuityLost {
		t.Fatalf("Acquire() result = %#v, want a fresh epoch and continuity loss", result)
	}
}

func TestAcquireDetectsCrashBeforeEpochStatusPatch(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	firstManager, kubeClient := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	request.ExpectedEpoch = "v1-11111111111111111111111111111111"

	first := mustAcquireResult(t, firstManager, request)
	if !first.ContinuityLost || first.Epoch == request.ExpectedEpoch {
		t.Fatalf("first Acquire() = %#v, want fresh acquisition", first)
	}

	// Simulate a manager crash after the Lease CAS but before status records
	// the API-assigned epoch. A new manager must preserve the actual Lease epoch
	// and continue reporting loss against the stale expected token.
	secondManager := targetlock.New(kubeClient, kubeClient, clock)
	second := mustAcquireResult(t, secondManager, request)
	if second.Epoch != first.Epoch || !second.ContinuityLost {
		t.Fatalf("second Acquire() = %#v, want stable epoch %q with continuity loss", second, first.Epoch)
	}
}

func TestStaleReleaserCannotClearNewHolder(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: started}
	locker, kubeClient := newLocker(t, clock)
	stale := lockRequest("target-a", "operation-a")
	staleResult := mustAcquireResult(t, locker, stale)
	stale.ExpectedEpoch = staleResult.Epoch

	clock.now = started.Add(31 * time.Second)
	current := lockRequest("target-a", "operation-b")
	mustObserveContention(t, locker, current, 30*time.Second)
	clock.now = clock.now.Add(30 * time.Second)
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

func TestStaleSameHolderReleaseCannotClearNewEpoch(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, kubeClient := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	first := mustAcquireResult(t, locker, request)

	currentRelease := request
	currentRelease.ExpectedEpoch = first.Epoch
	if err := locker.Release(context.Background(), currentRelease); err != nil {
		t.Fatalf("first Release() error = %v", err)
	}
	second := mustAcquireResult(t, locker, request)
	if second.Epoch == first.Epoch {
		t.Fatalf("reacquisition kept epoch %q", first.Epoch)
	}

	staleRelease := request
	staleRelease.ExpectedEpoch = first.Epoch
	if err := locker.Release(context.Background(), staleRelease); err != nil {
		t.Fatalf("stale Release() error = %v", err)
	}
	lease := getLease(t, kubeClient, request)
	if dereferenceString(lease.Spec.HolderIdentity) == "" {
		t.Fatal("stale same-holder Release cleared the new acquisition")
	}

	currentRelease.ExpectedEpoch = second.Epoch
	if err := locker.Release(context.Background(), currentRelease); err != nil {
		t.Fatalf("current Release() error = %v", err)
	}
	if got := dereferenceString(getLease(t, kubeClient, request).Spec.HolderIdentity); got != "" {
		t.Fatalf("current Release left holder %q", got)
	}
}

func TestForeignLeaseRenewalRestartsLocalExpiryObservation(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	holderClock := &fakeClock{now: started}
	holder, kubeClient := newLocker(t, holderClock)
	request := lockRequest("target-a", "operation-a")
	mustAcquire(t, holder, request)

	contenderClock := &fakeClock{now: started.Add(10 * time.Second)}
	contender := targetlock.New(kubeClient, kubeClient, contenderClock)
	second := lockRequest("target-a", "operation-b")
	mustObserveContention(t, contender, second, 30*time.Second)

	holderClock.now = holderClock.now.Add(20 * time.Second)
	mustAcquire(t, holder, request)
	contenderClock.now = contenderClock.now.Add(20 * time.Second)
	mustObserveContention(t, contender, second, 30*time.Second)

	contenderClock.now = contenderClock.now.Add(29 * time.Second)
	mustObserveContention(t, contender, second, time.Second)
	contenderClock.now = contenderClock.now.Add(time.Second)
	mustAcquire(t, contender, second)
}

func TestLeaderFailoverIgnoresForeignWallClockSkew(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	holderClock := &fakeClock{now: started}
	holder, kubeClient := newLocker(t, holderClock)
	first := lockRequest("target-a", "operation-a")
	mustAcquire(t, holder, first)

	// A newly elected leader whose wall clock is far ahead must still observe
	// an unchanged record for one complete local Lease duration.
	fastClock := &fakeClock{now: started.Add(24 * time.Hour)}
	fastLeader := targetlock.New(kubeClient, kubeClient, fastClock)
	second := lockRequest("target-a", "operation-b")
	mustObserveContention(t, fastLeader, second, 30*time.Second)
	fastClock.now = fastClock.now.Add(29 * time.Second)
	mustObserveContention(t, fastLeader, second, time.Second)
	fastClock.now = fastClock.now.Add(time.Second)
	mustAcquire(t, fastLeader, second)

	// A third leader whose clock is far behind applies the same rule to the
	// record written by the fast leader instead of trusting its future time.
	slowClock := &fakeClock{now: started.Add(-24 * time.Hour)}
	slowLeader := targetlock.New(kubeClient, kubeClient, slowClock)
	third := lockRequest("target-a", "operation-c")
	mustObserveContention(t, slowLeader, third, 30*time.Second)
	slowClock.now = slowClock.now.Add(30 * time.Second)
	mustAcquire(t, slowLeader, third)
}

func TestReleaseClearsOnlyMatchingHolderAndToleratesMissingLease(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	locker, kubeClient := newLocker(t, clock)
	request := lockRequest("target-a", "operation-a")
	request.ExpectedEpoch = "v1-11111111111111111111111111111111"

	if err := locker.Release(context.Background(), request); err != nil {
		t.Fatalf("Release() missing Lease error = %v", err)
	}
	request.ExpectedEpoch = ""
	result := mustAcquireResult(t, locker, request)
	request.ExpectedEpoch = result.Epoch
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
	request.CoordinationDigest = "postgres://user:super-secret@database/example"

	_, err := locker.Acquire(context.Background(), request)
	if !errors.Is(err, targetlock.ErrInvalidRequest) {
		t.Fatalf("Acquire() error = %v, want ErrInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), request.CoordinationDigest) {
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
		CoordinationDigest:    "sha256:" + hex.EncodeToString(sum[:]),
		Holder: targetlock.Holder{
			SchemaUID:   types.UID("c78d1c70-8f66-4c07-a49b-4ee621dc2280"),
			OperationID: operation,
		},
		Duration: 30 * time.Second,
	}
}

func mustAcquire(t *testing.T, locker *targetlock.Locker, request targetlock.Request) {
	t.Helper()
	_ = mustAcquireResult(t, locker, request)
}

func mustAcquireResult(t *testing.T, locker *targetlock.Locker, request targetlock.Request) targetlock.AcquireResult {
	t.Helper()
	result, err := locker.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !result.Acquired || result.Contention != nil {
		t.Fatalf("Acquire() result = %#v, want acquired", result)
	}
	return result
}

func mustObserveContention(t *testing.T, locker *targetlock.Locker, request targetlock.Request, remaining time.Duration) {
	t.Helper()
	result, err := locker.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if result.Acquired || result.Contention == nil || result.Contention.RequeueAfter != remaining {
		t.Fatalf("Acquire() result = %#v, want %s contention", result, remaining)
	}
}

func getLease(t *testing.T, kubeClient client.Client, request targetlock.Request) *coordinationv1.Lease {
	t.Helper()
	name, err := targetlock.LeaseName(request.CoordinationDigest)
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
