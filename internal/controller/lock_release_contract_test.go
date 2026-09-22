package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// One obligation, two families. Every row below runs against both, because the
// asymmetries that made this worth sharing were invisible while each family
// was measured on its own: each implementation read correctly, and they did
// different things.
//
// The table is the contract. A family added to it has to satisfy every row, so
// a third kind cannot quietly arrive with a release of its own.

const contractDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// lockOwnerUnderTest is one family's resource behind the shared interface,
// with a way to read back what the obligation wrote.
type lockOwnerUnderTest struct {
	family string
	owner  mutationlifecycle.LockOwner
	object client.Object
}

func schemaUnderTest() lockOwnerUnderTest {
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "orders", UID: types.UID("schema-uid")},
	}
	return lockOwnerUnderTest{family: "PtahSchema", owner: schemaLockOwner{schema}, object: schema}
}

func migrationUnderTest() lockOwnerUnderTest {
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "orders", UID: types.UID("migration-uid")},
	}
	return lockOwnerUnderTest{family: "PtahMigration", owner: migrationLockOwner{migration}, object: migration}
}

// lockContractScheme carries only what the Lease side of this contract needs.
func lockContractScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func bothFamilies() []func() lockOwnerUnderTest {
	return []func() lockOwnerUnderTest{schemaUnderTest, migrationUnderTest}
}

// A binding that could not be acted on is refused rather than recorded. A
// record missing its epoch is refused by the locker itself, so writing one
// down would mean a realm claimed for the full lease with a record that says
// it is being handed back.
func TestAnIncompleteLockBindingIsRefused(t *testing.T) {
	t.Parallel()

	complete := mutationlifecycle.LockBinding{
		CoordinationDigest:   contractDigest,
		OperationID:          "sha256:abc",
		LeaseEpoch:           "v1-" + strings.Repeat("0", 32),
		LeaseDurationSeconds: 960,
	}
	for _, row := range []struct {
		missing string
		mutate  func(*mutationlifecycle.LockBinding)
	}{
		{"coordination digest", func(b *mutationlifecycle.LockBinding) { b.CoordinationDigest = "" }},
		{"operation", func(b *mutationlifecycle.LockBinding) { b.OperationID = "" }},
		{"lease epoch", func(b *mutationlifecycle.LockBinding) { b.LeaseEpoch = "" }},
		{"lease duration", func(b *mutationlifecycle.LockBinding) { b.LeaseDurationSeconds = 0 }},
	} {
		t.Run("without a "+row.missing, func(t *testing.T) {
			t.Parallel()
			binding := complete
			row.mutate(&binding)
			release, err := mutationlifecycle.OwedRelease(binding)
			if err == nil {
				t.Fatalf("a binding with no %s was recorded as owed: %#v", row.missing, release)
			}
			if !errors.Is(err, mutationlifecycle.ErrIncompleteBinding) {
				t.Fatalf("the refusal does not say the binding is incomplete: %v", err)
			}
			if release != nil {
				t.Fatalf("the refusal still produced a record: %#v", release)
			}
		})
	}

	release, err := mutationlifecycle.OwedRelease(complete)
	if err != nil {
		t.Fatalf("a complete binding was refused: %v", err)
	}
	if release.CoordinationDigest != complete.CoordinationDigest ||
		release.OperationID != complete.OperationID ||
		release.LeaseEpoch != complete.LeaseEpoch ||
		release.LeaseDurationSeconds != complete.LeaseDurationSeconds {
		t.Fatalf("the record does not reproduce the binding: %#v", release)
	}
}

// Owing nothing does nothing, in both families. A pass that released
// something it never took would free a realm another claimant holds.
func TestOwingNothingReleasesNothing(t *testing.T) {
	t.Parallel()

	for _, build := range bothFamilies() {
		subject := build()
		t.Run(subject.family, func(t *testing.T) {
			t.Parallel()
			persisted := false
			err := mutationlifecycle.CompleteRelease(context.Background(), nil, "ptah-system", subject.owner,
				func(context.Context) error { persisted = true; return nil })
			if err != nil {
				t.Fatalf("owing nothing returned %v", err)
			}
			if persisted {
				t.Fatal("owing nothing still wrote a status patch")
			}
		})
	}
}

// The record is cleared only after the release succeeded, and the clearing is
// persisted. Both families answer the same way.
func TestAnOwedReleaseIsClearedOnlyAfterItSucceeds(t *testing.T) {
	t.Parallel()

	for _, build := range bothFamilies() {
		subject := build()
		t.Run(subject.family, func(t *testing.T) {
			t.Parallel()

			api := fake.NewClientBuilder().WithScheme(lockContractScheme(t)).Build()
			locks := targetlock.New(api, api, nil)
			acquired, err := locks.Acquire(context.Background(), targetlock.Request{
				CoordinationNamespace: "ptah-system",
				CoordinationDigest:    contractDigest,
				Holder:                targetlock.Holder{SchemaUID: subject.object.GetUID(), OperationID: "sha256:abc"},
				Duration:              960_000_000_000,
			})
			if err != nil || !acquired.Acquired {
				t.Fatalf("seed the Lease: acquired=%t err=%v", acquired.Acquired, err)
			}

			owed, err := mutationlifecycle.OwedRelease(mutationlifecycle.LockBinding{
				CoordinationDigest:   contractDigest,
				OperationID:          "sha256:abc",
				LeaseEpoch:           acquired.Epoch,
				LeaseDurationSeconds: 960,
			})
			if err != nil {
				t.Fatal(err)
			}
			subject.owner.SetOwedLockRelease(owed)

			persisted := 0
			if err := mutationlifecycle.CompleteRelease(context.Background(), locks, "ptah-system", subject.owner,
				func(context.Context) error { persisted++; return nil }); err != nil {
				t.Fatalf("CompleteRelease() error = %v", err)
			}
			if subject.owner.OwedLockRelease() != nil {
				t.Fatalf("the record survived a release that succeeded: %#v", subject.owner.OwedLockRelease())
			}
			if persisted != 1 {
				t.Fatalf("the cleared record was persisted %d times", persisted)
			}

			leases := &coordinationv1.LeaseList{}
			if err := api.List(context.Background(), leases, client.InNamespace("ptah-system")); err != nil {
				t.Fatal(err)
			}
			if len(leases.Items) == 0 {
				t.Fatal("no Lease exists, so this proved nothing about handing one back")
			}
			for _, lease := range leases.Items {
				if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
					t.Fatalf("Lease %s is still held by %q", lease.Name, *lease.Spec.HolderIdentity)
				}
			}
		})
	}
}

// A record that cannot be acted on is refused rather than silently dropped,
// and the record stays. Dropping it would leave the realm claimed with nothing
// saying so, which is the state this obligation exists to prevent.
func TestAnUnusableRecordKeepsTheObligation(t *testing.T) {
	t.Parallel()

	for _, build := range bothFamilies() {
		subject := build()
		t.Run(subject.family, func(t *testing.T) {
			t.Parallel()

			api := fake.NewClientBuilder().WithScheme(lockContractScheme(t)).Build()
			subject.owner.SetOwedLockRelease(&operatorv1alpha1.TargetLockReleaseStatus{
				CoordinationDigest: contractDigest,
				OperationID:        "sha256:abc",
				// No epoch: the locker refuses such a request outright.
				LeaseDurationSeconds: 960,
			})
			persisted := false
			err := mutationlifecycle.CompleteRelease(context.Background(), targetlock.New(api, api, nil),
				"ptah-system", subject.owner, func(context.Context) error { persisted = true; return nil })
			if err == nil {
				t.Fatal("a record with no epoch was accepted")
			}
			if !errors.Is(err, mutationlifecycle.ErrIncompleteBinding) {
				t.Fatalf("the refusal does not say the record is incomplete: %v", err)
			}
			if subject.owner.OwedLockRelease() == nil {
				t.Fatal("the refusal dropped the obligation it refused to act on")
			}
			if persisted {
				t.Fatal("a refused release still wrote a status patch")
			}
		})
	}
}

// A release that fails keeps the obligation, in both families. This is the row
// the record exists for: without it a single API error at that instant costs
// every other claimant of the database the whole lease duration, and nothing
// in status says the realm is still claimed.
//
// It is also what holds the order. Clearing the record before the release is
// known to have worked passes every other row in this table and fails here.
func TestAReleaseThatFailsKeepsTheObligation(t *testing.T) {
	t.Parallel()

	for _, build := range bothFamilies() {
		subject := build()
		t.Run(subject.family, func(t *testing.T) {
			t.Parallel()

			refused := errors.New("the API server refused the write")
			api := fake.NewClientBuilder().
				WithScheme(lockContractScheme(t)).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						return refused
					},
				}).
				Build()
			locks := targetlock.New(api, api, nil)
			// The Lease is created rather than updated, so the interceptor
			// above lets the acquisition through and refuses only the release.
			acquired, err := locks.Acquire(context.Background(), targetlock.Request{
				CoordinationNamespace: "ptah-system",
				CoordinationDigest:    contractDigest,
				Holder:                targetlock.Holder{SchemaUID: subject.object.GetUID(), OperationID: "sha256:abc"},
				Duration:              960_000_000_000,
			})
			if err != nil || !acquired.Acquired {
				t.Fatalf("seed the Lease: acquired=%t err=%v", acquired.Acquired, err)
			}

			owed, err := mutationlifecycle.OwedRelease(mutationlifecycle.LockBinding{
				CoordinationDigest:   contractDigest,
				OperationID:          "sha256:abc",
				LeaseEpoch:           acquired.Epoch,
				LeaseDurationSeconds: 960,
			})
			if err != nil {
				t.Fatal(err)
			}
			subject.owner.SetOwedLockRelease(owed)

			persisted := false
			if err := mutationlifecycle.CompleteRelease(context.Background(), locks, "ptah-system", subject.owner,
				func(context.Context) error { persisted = true; return nil }); err == nil {
				t.Fatal("a release the locker refused was reported as done")
			}
			if subject.owner.OwedLockRelease() == nil {
				t.Fatal("a release that failed dropped the obligation, so nothing will hand the realm back")
			}
			if persisted {
				t.Fatal("a release that failed still wrote the cleared record")
			}
		})
	}
}
