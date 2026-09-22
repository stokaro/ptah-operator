// Package mutationlifecycle holds the obligations both resource families carry
// when an operation may have changed a database.
//
// The two controllers share Job construction, framing, fingerprints and
// Leases, and used to state each obligation twice. Using the same components
// does not preserve the same guarantees: an obligation written twice drifts,
// and the drift is invisible because each copy reads correctly on its own. The
// database-realm Lease is where that has already cost something -- one family
// retried a failed release and the other dropped it -- so it is the first
// obligation to have one implementation.
package mutationlifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// LockBinding is the part of a claim that decides a release. Every field is
// required, and the completeness check is the point of the type: a release
// naming no epoch is refused outright by the locker, and one naming no
// duration cannot reproduce the request that acquired the Lease. A partial
// binding is not a weaker release -- it is no release at all, and the realm
// stays claimed until the Lease expires.
type LockBinding struct {
	CoordinationDigest   string
	OperationID          string
	LeaseEpoch           string
	LeaseDurationSeconds int32
}

// LockOwner is a resource that can owe a database realm back. Both families
// implement it over their own status field.
type LockOwner interface {
	// UID identifies the holder the Lease was taken under.
	UID() types.UID
	// OwedLockRelease is the release this resource still owes, or nil.
	OwedLockRelease() *operatorv1alpha1.TargetLockReleaseStatus
	// SetOwedLockRelease records or clears it.
	SetOwedLockRelease(*operatorv1alpha1.TargetLockReleaseStatus)
}

// ErrIncompleteBinding is returned for a binding that cannot produce a release.
var ErrIncompleteBinding = errors.New("lock binding is incomplete")

// OwedRelease turns a claim's lock binding into the durable record, refusing
// one that could not be acted on.
func OwedRelease(binding LockBinding) (*operatorv1alpha1.TargetLockReleaseStatus, error) {
	if binding.CoordinationDigest == "" || binding.OperationID == "" ||
		binding.LeaseEpoch == "" || binding.LeaseDurationSeconds == 0 {
		return nil, fmt.Errorf("persist target lock release: %w", ErrIncompleteBinding)
	}
	return &operatorv1alpha1.TargetLockReleaseStatus{
		CoordinationDigest:   binding.CoordinationDigest,
		OperationID:          binding.OperationID,
		LeaseDurationSeconds: binding.LeaseDurationSeconds,
		LeaseEpoch:           binding.LeaseEpoch,
	}, nil
}

// CompleteRelease hands the realm back and clears the record only once the
// release succeeded. A failure leaves the record standing, so the next pass
// tries again instead of leaving every other claimant to wait out the Lease.
//
// persist writes the cleared record. It is the caller's own status patch
// because the two families patch differently, and it runs only after the
// locker returned nil: clearing first would lose the obligation on a crash
// between the two, which is the whole reason the record exists.
func CompleteRelease(
	ctx context.Context,
	locks *targetlock.Locker,
	coordinationNamespace string,
	owner LockOwner,
	persist func(context.Context) error,
) error {
	release := owner.OwedLockRelease()
	if release == nil {
		return nil
	}
	if locks == nil {
		return errors.New("release database target lock: target locker is not configured")
	}
	if release.CoordinationDigest == "" || release.OperationID == "" ||
		release.LeaseEpoch == "" || release.LeaseDurationSeconds == 0 {
		return fmt.Errorf("release database target lock: persisted %w", ErrIncompleteBinding)
	}
	if err := locks.Release(ctx, targetlock.Request{
		CoordinationNamespace: coordinationNamespace,
		CoordinationDigest:    release.CoordinationDigest,
		Holder: targetlock.Holder{
			SchemaUID:   owner.UID(),
			OperationID: release.OperationID,
		},
		Duration:      time.Duration(release.LeaseDurationSeconds) * time.Second,
		ExpectedEpoch: release.LeaseEpoch,
	}); err != nil {
		return fmt.Errorf("release database target lock: %w", err)
	}
	owner.SetOwedLockRelease(nil)
	return persist(ctx)
}
