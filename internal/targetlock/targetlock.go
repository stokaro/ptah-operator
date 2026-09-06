// Package targetlock serializes database operations in the same user-declared
// physical coordination realm. Locks are deliberately owner-neutral so
// different Ptah resource kinds can coordinate through the same Lease.
package targetlock

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	epochAnnotation   = "operator.ptah.dev/lease-epoch"
)

var (
	// ErrInvalidRequest means the lock request did not meet the package's
	// credential-free input contract.
	ErrInvalidRequest = errors.New("invalid target lock request")
	// ErrConcurrentUpdate means repeated compare-and-swap attempts raced with
	// another writer. Callers should reconcile again from fresh state.
	ErrConcurrentUpdate = errors.New("target lease changed concurrently")
	// ErrMalformedLease means a foreign non-empty holder lacks enough valid
	// timing evidence to prove that takeover is safe. Callers must fail closed.
	ErrMalformedLease = errors.New("malformed target lease")
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
// CoordinationDigest must be an OCI-style SHA-256 digest computed from the
// normalized engine and exact stable coordination key.
type Request struct {
	// CoordinationNamespace is shared by every resource that may address the
	// same database. Using the schema's own namespace here would permit two
	// namespaces to apply concurrently to one target.
	CoordinationNamespace string
	CoordinationDigest    string
	Holder                Holder
	Duration              time.Duration
	// ExpectedEpoch is generated and persisted by the caller before dispatch.
	// When non-empty, a successful acquisition reports continuity loss unless
	// the same holder already owns the Lease under this exact epoch.
	ExpectedEpoch string
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
	// Epoch is a random, non-secret acquisition identity that remains stable
	// across renewals and rotates on every release/reacquisition, takeover, or
	// Lease recreation. Consumers persist it before dispatch and reject results
	// produced across an epoch change.
	Epoch string
	// ContinuityLost reports that ExpectedEpoch did not describe an
	// uninterrupted existing acquisition. The caller may hold the Lease now,
	// but must discard any result produced before this acquisition.
	ContinuityLost bool
}

// Locker coordinates through coordination.k8s.io/v1 Lease objects. Reader
// should be the manager's uncached APIReader so every decision uses current
// resourceVersion state; Writer performs compare-and-swap updates.
type Locker struct {
	reader       client.Reader
	writer       client.Writer
	clock        Clock
	observedMu   sync.Mutex
	observations map[types.NamespacedName]leaseObservation
}

// New constructs a Locker. A nil clock selects the wall clock.
func New(reader client.Reader, writer client.Writer, clock Clock) *Locker {
	if clock == nil {
		clock = wallClock{}
	}
	return &Locker{
		reader:       reader,
		writer:       writer,
		clock:        clock,
		observations: make(map[types.NamespacedName]leaseObservation),
	}
}

// LeaseName derives a deterministic DNS label from all 256 bits of a SHA-256
// coordination realm. The base32 encoding is lower-case and has no padding.
func LeaseName(coordinationDigest string) (string, error) {
	digest, err := decodeSHA256(coordinationDigest)
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
	leaseKey := types.NamespacedName{Namespace: request.CoordinationNamespace, Name: validated.leaseName}

	for range maxAttempts {
		lease := &coordinationv1.Lease{}
		err = l.reader.Get(ctx, leaseKey, lease)
		if apierrors.IsNotFound(err) {
			l.forgetObservation(leaseKey)
			lease, createErr := newLease(request.CoordinationNamespace, validated, l.now())
			if createErr != nil {
				return AcquireResult{}, createErr
			}
			if err = l.writer.Create(ctx, lease); err == nil {
				return AcquireResult{
					Acquired: true, Epoch: lease.Annotations[epochAnnotation],
					ContinuityLost: validated.expectedEpoch != "",
				}, nil
			}
			if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
				continue
			}
			return AcquireResult{}, fmt.Errorf("create target lease: %w", err)
		}
		if err != nil {
			return AcquireResult{}, fmt.Errorf("read target lease: %w", err)
		}

		currentHolder := dereference(lease.Spec.HolderIdentity)
		if currentHolder != "" && currentHolder != validated.holderIdentity {
			duration, ok := validForeignLeaseDuration(lease)
			if !ok || !hasLeaseTimestamp(lease) {
				return AcquireResult{}, fmt.Errorf("%w: foreign holder has incomplete timing evidence", ErrMalformedLease)
			}
			remaining := l.observeForeignLease(leaseKey, lease, duration, l.localNow())
			if remaining > 0 {
				return AcquireResult{
					Contention: &Contention{RequeueAfter: remaining},
				}, nil
			}
		} else {
			l.forgetObservation(leaseKey)
		}

		currentEpoch := lease.Annotations[epochAnnotation]
		sameHolder := currentHolder == validated.holderIdentity
		continuous := sameHolder && validEpoch(currentEpoch) &&
			(validated.expectedEpoch == "" || currentEpoch == validated.expectedEpoch)
		continuityLost := validated.expectedEpoch != "" && !continuous
		if err := updateLease(lease, validated, l.now(), sameHolder && validEpoch(currentEpoch)); err != nil {
			return AcquireResult{}, err
		}
		if err = l.writer.Update(ctx, lease); err == nil {
			l.forgetObservation(leaseKey)
			return AcquireResult{Acquired: true, Epoch: lease.Annotations[epochAnnotation], ContinuityLost: continuityLost}, nil
		}
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			continue
		}
		return AcquireResult{}, fmt.Errorf("update target lease: %w", err)
	}

	return AcquireResult{}, fmt.Errorf("acquire target lease: %w", ErrConcurrentUpdate)
}

// Release clears a Lease only while it is still held by the requesting
// operation under the exact persisted acquisition epoch. A missing Lease, a
// Lease acquired by another holder, or a later acquisition by the same holder
// is already a successful release from this caller's perspective.
func (l *Locker) Release(ctx context.Context, request Request) error {
	validated, err := l.validate(request)
	if err != nil {
		return err
	}
	if validated.expectedEpoch == "" {
		return fmt.Errorf("%w: expected epoch is required for release", ErrInvalidRequest)
	}
	leaseKey := types.NamespacedName{Namespace: request.CoordinationNamespace, Name: validated.leaseName}

	for range maxAttempts {
		lease := &coordinationv1.Lease{}
		err = l.reader.Get(ctx, leaseKey, lease)
		if apierrors.IsNotFound(err) {
			l.forgetObservation(leaseKey)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read target lease for release: %w", err)
		}
		if dereference(lease.Spec.HolderIdentity) != validated.holderIdentity ||
			lease.Annotations[epochAnnotation] != validated.expectedEpoch {
			return nil
		}

		empty := ""
		now := metav1.NewMicroTime(monotonicLeaseTime(l.now(), lease))
		lease.Spec.HolderIdentity = &empty
		lease.Spec.RenewTime = &now
		if err = l.writer.Update(ctx, lease); err == nil {
			l.forgetObservation(leaseKey)
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
	expectedEpoch   string
}

func (l *Locker) validate(request Request) (validatedRequest, error) {
	if l == nil || l.reader == nil || l.writer == nil || l.clock == nil {
		return validatedRequest{}, fmt.Errorf("%w: locker is not initialized", ErrInvalidRequest)
	}
	if problems := validation.IsDNS1123Label(request.CoordinationNamespace); len(problems) != 0 {
		return validatedRequest{}, fmt.Errorf("%w: coordination namespace is invalid", ErrInvalidRequest)
	}
	leaseName, err := LeaseName(request.CoordinationDigest)
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
	if request.ExpectedEpoch != "" && !validEpoch(request.ExpectedEpoch) {
		return validatedRequest{}, fmt.Errorf("%w: expected epoch is invalid", ErrInvalidRequest)
	}
	return validatedRequest{
		leaseName:       leaseName,
		holderIdentity:  holderIdentity,
		durationSeconds: int32(request.Duration / time.Second),
		expectedEpoch:   request.ExpectedEpoch,
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
		return nil, fmt.Errorf("%w: coordination digest must be SHA-256", ErrInvalidRequest)
	}
	digest, err := hex.DecodeString(encoded)
	if err != nil || len(digest) != sha256.Size {
		return nil, fmt.Errorf("%w: coordination digest must be SHA-256", ErrInvalidRequest)
	}
	return digest, nil
}

func newLease(namespace string, request validatedRequest, now time.Time) (*coordinationv1.Lease, error) {
	epoch, err := newEpoch()
	if err != nil {
		return nil, err
	}
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
			Annotations: map[string]string{epochAnnotation: epoch},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &duration,
			AcquireTime:          &acquired,
			RenewTime:            &renewed,
			LeaseTransitions:     &transitions,
		},
	}, nil
}

func updateLease(lease *coordinationv1.Lease, request validatedRequest, now time.Time, preserveEpoch bool) error {
	now = monotonicLeaseTime(now, lease)
	holder := request.holderIdentity
	duration := request.durationSeconds
	timestamp := metav1.NewMicroTime(now)

	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &duration
	lease.Spec.RenewTime = &timestamp
	if preserveEpoch {
		if lease.Spec.AcquireTime == nil {
			lease.Spec.AcquireTime = &timestamp
		}
		if lease.Spec.LeaseTransitions == nil {
			transitions := int32(0)
			lease.Spec.LeaseTransitions = &transitions
		}
		return nil
	}
	epoch, err := newEpoch()
	if err != nil {
		return err
	}
	if lease.Annotations == nil {
		lease.Annotations = make(map[string]string)
	}
	lease.Annotations[epochAnnotation] = epoch

	lease.Spec.AcquireTime = &timestamp
	transitions := int32(1)
	if lease.Spec.LeaseTransitions != nil && *lease.Spec.LeaseTransitions < int32(^uint32(0)>>1) {
		transitions = *lease.Spec.LeaseTransitions + 1
	}
	lease.Spec.LeaseTransitions = &transitions
	return nil
}

func newEpoch() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate target lease epoch: %w", err)
	}
	return "v1-" + hex.EncodeToString(entropy[:]), nil
}

func validEpoch(value string) bool {
	if len(value) != len("v1-")+32 || !strings.HasPrefix(value, "v1-") {
		return false
	}
	_, err := hex.DecodeString(value[len("v1-"):])
	return err == nil
}

func validForeignLeaseDuration(lease *coordinationv1.Lease) (time.Duration, bool) {
	if lease.Spec.LeaseDurationSeconds == nil {
		return 0, false
	}
	duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	if duration < MinLeaseDuration || duration > MaxLeaseDuration {
		return 0, false
	}
	return duration, true
}

func hasLeaseTimestamp(lease *coordinationv1.Lease) bool {
	return (lease.Spec.RenewTime != nil && !lease.Spec.RenewTime.IsZero()) ||
		(lease.Spec.AcquireTime != nil && !lease.Spec.AcquireTime.IsZero())
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

// localNow retains the process clock's monotonic component. Foreign Lease
// expiry is measured only from the instant this Locker first observed an
// unchanged record, never from another node's wall-clock timestamp.
func (l *Locker) localNow() time.Time { return l.clock.Now() }

type leaseObservation struct {
	record    leaseRecord
	firstSeen time.Time
}

type leaseRecord struct {
	resourceVersion string
	uid             types.UID
	holder          string
	duration        int32
	acquired        int64
	renewed         int64
	transitions     int32
}

func observedLeaseRecord(lease *coordinationv1.Lease) leaseRecord {
	record := leaseRecord{
		resourceVersion: lease.ResourceVersion,
		uid:             lease.UID,
		holder:          dereference(lease.Spec.HolderIdentity),
	}
	if lease.Spec.LeaseDurationSeconds != nil {
		record.duration = *lease.Spec.LeaseDurationSeconds
	}
	if lease.Spec.AcquireTime != nil {
		record.acquired = lease.Spec.AcquireTime.UnixMicro()
	}
	if lease.Spec.RenewTime != nil {
		record.renewed = lease.Spec.RenewTime.UnixMicro()
	}
	if lease.Spec.LeaseTransitions != nil {
		record.transitions = *lease.Spec.LeaseTransitions
	}
	return record
}

func (l *Locker) observeForeignLease(
	key types.NamespacedName,
	lease *coordinationv1.Lease,
	duration time.Duration,
	now time.Time,
) time.Duration {
	record := observedLeaseRecord(lease)
	l.observedMu.Lock()
	defer l.observedMu.Unlock()
	if l.observations == nil {
		l.observations = make(map[types.NamespacedName]leaseObservation)
	}
	observation, found := l.observations[key]
	if !found || observation.record != record || now.Before(observation.firstSeen) {
		l.observations[key] = leaseObservation{record: record, firstSeen: now}
		return duration
	}
	elapsed := now.Sub(observation.firstSeen)
	if elapsed < duration {
		return duration - elapsed
	}
	delete(l.observations, key)
	return 0
}

func (l *Locker) forgetObservation(key types.NamespacedName) {
	l.observedMu.Lock()
	defer l.observedMu.Unlock()
	delete(l.observations, key)
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
