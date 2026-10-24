package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// readRequest is what the read-requested-at annotation asks of a resource,
// against the request its status records as answered.
type readRequest struct {
	// token is the value as status records it.
	token string
	// pending is a value status does not record as answered.
	pending bool
	// instant is whether the value is an RFC 3339 instant, the only kind
	// that starts a read.
	instant bool
}

func readRequestOf(annotations map[string]string, answered string) readRequest {
	value, ok := annotations[operatorv1alpha1.ReadRequestAnnotation]
	if !ok {
		return readRequest{}
	}
	token := readRequestToken(value)
	if token == answered {
		return readRequest{token: token}
	}
	_, err := time.Parse(time.RFC3339, value)
	return readRequest{token: token, pending: true, instant: err == nil}
}

// readRequestToken is how status records an annotation value: as given up to
// MaxReadRequestLength bytes, as its SHA-256 digest beyond, so any value can
// be recorded as answered and none is answered twice.
func readRequestToken(value string) string {
	if len(value) <= operatorv1alpha1.MaxReadRequestLength {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// readOnRequest says what a pending read request does in a phase that has no
// operation running: whether this pass answers it, and whether answering it
// brings the next read forward to now.
//
// A read the resource is already part-way through does not answer it, since
// it may have read the database before the change the request is about, so
// those phases leave the request for the read that starts after them. A
// failed resource waits out its retry delay first, and the request is
// answered by the read that starts when the delay ends.
type readOnRequest struct {
	answer   bool
	schedule bool
}

func schemaReadOnRequest(phase operatorv1alpha1.ReconciliationPhase, nextRead bool) readOnRequest {
	switch phase {
	case operatorv1alpha1.PhaseVerifying, operatorv1alpha1.PhaseObserving,
		operatorv1alpha1.PhaseVerifyingConvergence, operatorv1alpha1.PhasePlanning:
		return readOnRequest{}
	case operatorv1alpha1.PhaseInSync, operatorv1alpha1.PhaseBlocked,
		operatorv1alpha1.PhaseReadyToApply, operatorv1alpha1.PhaseAwaitingApproval:
		return readOnRequest{answer: true, schedule: true}
	case operatorv1alpha1.PhaseFailed:
		return readOnRequest{answer: nextRead}
	default:
		// Every other phase starts a fresh Resolve on this pass.
		return readOnRequest{answer: true}
	}
}

func migrationReadOnRequest(phase operatorv1alpha1.MigrationPhase, nextRead bool) readOnRequest {
	switch phase {
	case operatorv1alpha1.MigrationPhaseVerifying, operatorv1alpha1.MigrationPhaseReading,
		operatorv1alpha1.MigrationPhaseVerifyingHistory:
		return readOnRequest{}
	case operatorv1alpha1.MigrationPhaseInSync, operatorv1alpha1.MigrationPhaseBlocked,
		operatorv1alpha1.MigrationPhaseAwaitingApproval, operatorv1alpha1.MigrationPhasePlanning:
		return readOnRequest{answer: true, schedule: true}
	case operatorv1alpha1.MigrationPhaseFailed:
		return readOnRequest{answer: nextRead}
	default:
		return readOnRequest{answer: true}
	}
}

// readRequestChanged wakes a controller whose other filters ignore metadata
// when the read request annotation changes, since an annotation does not
// move the generation.
func readRequestChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(update event.UpdateEvent) bool {
			return update.ObjectOld.GetAnnotations()[operatorv1alpha1.ReadRequestAnnotation] !=
				update.ObjectNew.GetAnnotations()[operatorv1alpha1.ReadRequestAnnotation]
		},
	}
}
