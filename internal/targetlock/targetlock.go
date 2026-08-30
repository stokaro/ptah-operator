// Package targetlock serializes database operations that address the same
// credential-free target identity. Locks are deliberately owner-neutral so
// different Ptah resource kinds can coordinate through the same Lease.
package targetlock

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// MinLeaseDuration prevents hot-looping on locks that cannot be renewed in
	// time under normal API server latency.
	MinLeaseDuration = 5 * time.Second
	// MaxLeaseDuration covers the maximum 24-hour Job active deadline plus a
	// scheduling grace period. This keeps a running Job protected through a
	// manager outage while still bounding recovery from an abandoned Lease.
	MaxLeaseDuration = 24*time.Hour + time.Minute

	leaseNamePrefix = "ptah-t-"
	maxAttempts     = 5

	managedByLabel    = "app.kubernetes.io/managed-by"
	coordinationLabel = "operator.ptah.dev/coordination"
)

var (
	// ErrInvalidRequest means the lock request did not meet the package's
	// credential-free input contract.
	ErrInvalidRequest = errors.New("invalid target lock request")
	// ErrConcurrentUpdate means repeated compare-and-swap attempts raced with
	// another writer. Callers should reconcile again from fresh state.
	ErrConcurrentUpdate = errors.New("target lease changed concurrently")
)

// Clock supplies reconciliation time and makes expiration behavior testable.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Holder identifies one resource operation. Its persisted representation is a
// hash of both fields, so even an accidentally sensitive OperationID cannot be
// copied into a Lease.
type Holder struct {
	SchemaUID   types.UID
	OperationID string
}

// Request contains the complete inputs needed to acquire or release a target
// lock. CoordinationNamespace must be common to all managed resources, while
// TargetIdentityDigest must be an OCI-style SHA-256 digest computed from a
// credential-free target identity.
type Request struct {
	// CoordinationNamespace is shared by every resource that may address the
	// same database. Using the schema's own namespace here would permit two
	// namespaces to apply concurrently to one target.
	CoordinationNamespace string
	TargetIdentityDigest  string
	Holder                Holder
	Duration              time.Duration
}

// Contention describes a live lock held by a different operation. It does not
// disclose the other holder's identity.
type Contention struct {
	RequeueAfter time.Duration
}

// AcquireResult distinguishes successful acquisition from expected lock
// contention without using errors for normal coordination.
type AcquireResult struct {
	Acquired   bool
	Contention *Contention
}

// Locker coordinates through coordination.k8s.io/v1 Lease objects. Reader
// should be the manager's uncached APIReader so every decision uses current
// resourceVersion state; Writer performs compare-and-swap updates.
type Locker struct {
	reader client.Reader
	writer client.Writer
	clock  Clock
}

// New constructs a Locker. A nil clock selects the wall clock.
func New(reader client.Reader, writer client.Writer, clock Clock) *Locker {
	if clock == nil {
		clock = wallClock{}
	}
	return &Locker{reader: reader, writer: writer, clock: clock}
}

// LeaseName derives a deterministic DNS label from all 256 bits of a SHA-256
// target identity. The base32 encoding is lower-case and has no padding.
func LeaseName(targetIdentityDigest string) (string, error) {
	digest, err := decodeSHA256(targetIdentityDigest)
	if err != nil {
		return "", err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest)
	return leaseNamePrefix + strings.ToLower(encoded), nil
}

// Acquire creates, renews, or takes over an expired Lease. An unexpired Lease
// held by another operation returns a typed contention result and no error.
func (l *Locker) Acquire(ctx context.Context, request Request) (AcquireResult, error) {
	validated, err := l.validate(request)
	if err != nil {
		return AcquireResult{}, err
	}

	for range maxAttempts {
		lease := &coordinationv1.Lease{}
		err = l.reader.Get(ctx, client.ObjectKey{Namespace: request.CoordinationNamespace, Name: validated.leaseName}, lease)
		if apierrors.IsNotFound(err) {
			lease = newLease(request.CoordinationNamespace, validated, l.now())
			if err = l.writer.Create(ctx, lease); err == nil {
				return AcquireResult{Acquired: true}, nil
			}
			if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
				continue
			}
			return AcquireResult{}, fmt.Errorf("create target lease: %w", err)
		}
		if err != nil {
			return AcquireResult{}, fmt.Errorf("read target lease: %w", err)
		}

		now := l.now()
		currentHolder := dereference(lease.Spec.HolderIdentity)
		if currentHolder != "" && currentHolder != validated.holderIdentity {
			if expiration, ok := expiresAt(lease); ok && now.Before(expiration) {
				return AcquireResult{
					Contention: &Contention{RequeueAfter: expiration.Sub(now)},
				}, nil
			}
		}

		sameHolder := currentHolder == validated.holderIdentity
		updateLease(lease, validated, now, sameHolder)
		if err = l.writer.Update(ctx, lease); err == nil {
			return AcquireResult{Acquired: true}, nil
		}
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			continue
		}
		return AcquireResult{}, fmt.Errorf("update target lease: %w", err)
	}

	return AcquireResult{}, fmt.Errorf("acquire target lease: %w", ErrConcurrentUpdate)
}

// Release clears a Lease only while it is still held by the requesting
// operation. A missing Lease or a Lease acquired by another holder is already
// a successful release from this caller's perspective.
func (l *Locker) Release(ctx context.Context, request Request) error {
	validated, err := l.validate(request)
	if err != nil {
		return err
	}

	for range maxAttempts {
		lease := &coordinationv1.Lease{}
		err = l.reader.Get(ctx, client.ObjectKey{Namespace: request.CoordinationNamespace, Name: validated.leaseName}, lease)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read target lease for release: %w", err)
		}
		if dereference(lease.Spec.HolderIdentity) != validated.holderIdentity {
			return nil
		}

		empty := ""
		now := metav1.NewMicroTime(monotonicLeaseTime(l.now(), lease))
		lease.Spec.HolderIdentity = &empty
		lease.Spec.RenewTime = &now
		if err = l.writer.Update(ctx, lease); err == nil {
			return nil
		}
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			continue
		}
		return fmt.Errorf("release target lease: %w", err)
	}

	return fmt.Errorf("release target lease: %w", ErrConcurrentUpdate)
}

type validatedRequest struct {
	leaseName       string
	holderIdentity  string
	durationSeconds int32
}

func (l *Locker) validate(request Request) (validatedRequest, error) {
	if l == nil || l.reader == nil || l.writer == nil || l.clock == nil {
		return validatedRequest{}, fmt.Errorf("%w: locker is not initialized", ErrInvalidRequest)
	}
	if problems := validation.IsDNS1123Label(request.CoordinationNamespace); len(problems) != 0 {
		return validatedRequest{}, fmt.Errorf("%w: coordination namespace is invalid", ErrInvalidRequest)
	}
	leaseName, err := LeaseName(request.TargetIdentityDigest)
	if err != nil {
		return validatedRequest{}, err
	}
	holderIdentity, err := request.Holder.identity()
	if err != nil {
		return validatedRequest{}, err
	}
	if request.Duration < MinLeaseDuration || request.Duration > MaxLeaseDuration || request.Duration%time.Second != 0 {
		return validatedRequest{}, fmt.Errorf("%w: duration must be whole seconds between %s and %s", ErrInvalidRequest, MinLeaseDuration, MaxLeaseDuration)
	}
	return validatedRequest{
		leaseName:       leaseName,
		holderIdentity:  holderIdentity,
		durationSeconds: int32(request.Duration / time.Second),
	}, nil
}

func (h Holder) identity() (string, error) {
	if len(h.SchemaUID) == 0 || len(h.SchemaUID) > 128 || len(h.OperationID) == 0 || len(h.OperationID) > 256 {
		return "", fmt.Errorf("%w: holder is invalid", ErrInvalidRequest)
	}
	sum := sha256.New()
	_, _ = sum.Write([]byte(h.SchemaUID))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write([]byte(h.OperationID))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil))
	return "ptah-h-" + strings.ToLower(encoded), nil
}

func decodeSHA256(value string) ([]byte, error) {
	algorithm, encoded, ok := strings.Cut(value, ":")
	if !ok || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return nil, fmt.Errorf("%w: target identity digest must be SHA-256", ErrInvalidRequest)
	}
	digest, err := hex.DecodeString(encoded)
	if err != nil || len(digest) != sha256.Size {
		return nil, fmt.Errorf("%w: target identity digest must be SHA-256", ErrInvalidRequest)
	}
	return digest, nil
}

func newLease(namespace string, request validatedRequest, now time.Time) *coordinationv1.Lease {
	holder := request.holderIdentity
	duration := request.durationSeconds
	acquired := metav1.NewMicroTime(now)
	renewed := acquired
	transitions := int32(0)
	return &coordinationv1.Lease{
		TypeMeta: metav1.TypeMeta{
			APIVersion: coordinationv1.SchemeGroupVersion.String(),
			Kind:       "Lease",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      request.leaseName,
			Labels: map[string]string{
				managedByLabel:    "ptah-operator",
				coordinationLabel: "database-target",
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &duration,
			AcquireTime:          &acquired,
			RenewTime:            &renewed,
			LeaseTransitions:     &transitions,
		},
	}
}

func updateLease(lease *coordinationv1.Lease, request validatedRequest, now time.Time, sameHolder bool) {
	now = monotonicLeaseTime(now, lease)
	holder := request.holderIdentity
	duration := request.durationSeconds
	timestamp := metav1.NewMicroTime(now)

	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &duration
	lease.Spec.RenewTime = &timestamp
	if sameHolder {
		if lease.Spec.AcquireTime == nil {
			lease.Spec.AcquireTime = &timestamp
		}
		if lease.Spec.LeaseTransitions == nil {
			transitions := int32(0)
			lease.Spec.LeaseTransitions = &transitions
		}
		return
	}

	lease.Spec.AcquireTime = &timestamp
	transitions := int32(1)
	if lease.Spec.LeaseTransitions != nil && *lease.Spec.LeaseTransitions < int32(^uint32(0)>>1) {
		transitions = *lease.Spec.LeaseTransitions + 1
	}
	lease.Spec.LeaseTransitions = &transitions
}

func expiresAt(lease *coordinationv1.Lease) (time.Time, bool) {
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return time.Time{}, false
	}
	if lease.Spec.RenewTime != nil {
		return lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second), true
	}
	if lease.Spec.AcquireTime != nil {
		return lease.Spec.AcquireTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second), true
	}
	return time.Time{}, false
}

func monotonicLeaseTime(now time.Time, lease *coordinationv1.Lease) time.Time {
	now = now.UTC()
	if lease.Spec.RenewTime != nil && lease.Spec.RenewTime.After(now) {
		return lease.Spec.RenewTime.Time
	}
	if lease.Spec.AcquireTime != nil && lease.Spec.AcquireTime.After(now) {
		return lease.Spec.AcquireTime.Time
	}
	return now
}

func (l *Locker) now() time.Time {
	return l.clock.Now().UTC().Truncate(time.Microsecond)
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
