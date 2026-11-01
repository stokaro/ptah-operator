package operate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/apirefusal"
)

// SuspendResult is what SetSuspend asked for and what the API server
// answered. Generation is the requested state, the one the controller has to
// observe; ObservedGeneration and Phase are what it had observed when the
// answer was written.
type SuspendResult struct {
	Ref Ref
	// Suspend is the value spec.suspend was asked to hold.
	Suspend bool
	// Changed is false when spec.suspend already held it and nothing was
	// sent.
	Changed bool
	UID     types.UID
	// Generation is the spec generation after the change.
	Generation         int64
	ObservedGeneration int64
	Phase              string
	// Running is the operation in flight, if any. Suspension does not stop
	// it: an Apply that has started is observed to its end.
	Running string
	// Object is the resource as the answer carried it, a baseline for a
	// wait that follows.
	Object client.Object
}

// SetSuspend sets spec.suspend on the resource ref names and nothing else.
// Suspending prevents new Jobs; it does not cancel an Apply already running,
// which the controller observes to its end before the resource reads
// Suspended. Resuming lets the controller read the database again and decide
// from what it finds.
//
// When spec.suspend already holds the value nothing is sent and Changed is
// false. Otherwise one JSON patch tests metadata.uid and adds spec.suspend,
// so a resource replaced under the name since it was read returns
// ErrReplaced and is not changed.
func SetSuspend(ctx context.Context, c client.Client, ref Ref, suspend bool) (SuspendResult, error) {
	object, err := ref.newObject()
	if err != nil {
		return SuspendResult{}, err
	}
	if err := c.Get(ctx, ref.key(), object); err != nil {
		return SuspendResult{}, ref.failed("read", err)
	}
	read, err := snapshotOf(object)
	if err != nil {
		return SuspendResult{}, err
	}
	if read.suspend == suspend {
		return suspendResult(ref, suspend, false, read), nil
	}
	patch, err := json.Marshal([]patchOperation{
		{Op: "test", Path: "/metadata/uid", Value: read.uid},
		{Op: "add", Path: "/spec/suspend", Value: suspend},
	})
	if err != nil {
		return SuspendResult{}, err
	}
	if err := c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(FieldManager)); err != nil {
		return SuspendResult{}, classifyPatchError(ctx, c, ref, read.uid, nil, err)
	}
	written, err := snapshotOf(object)
	if err != nil {
		return SuspendResult{}, err
	}
	return suspendResult(ref, suspend, true, written), nil
}

func suspendResult(ref Ref, suspend, changed bool, s snapshot) SuspendResult {
	return SuspendResult{
		Ref: ref, Suspend: suspend, Changed: changed, UID: s.uid,
		Generation: s.generation, ObservedGeneration: s.observedGeneration,
		Phase: s.phase, Running: s.operation, Object: s.object,
	}
}

// RefreshResult is the read request Refresh wrote.
type RefreshResult struct {
	Ref Ref
	// Request is the annotation value written. A wait for the read it asks
	// for names it.
	Request string
	// Previous is the value the annotation held before, empty when none.
	Previous string
	UID      types.UID
	// Suspended reports spec.suspend: a suspended resource reads nothing, and
	// the request is answered after it is resumed.
	Suspended bool
	// Object is the resource as the answer carried it, the baseline a wait
	// for this request starts from.
	Object client.Object
}

// Refresh asks the resource for a fresh read of its database by setting the
// operator.ptah.run/read-requested-at annotation, and changes nothing else.
//
// The value is now in RFC 3339 with nanoseconds, in UTC, moved past the
// annotation's current value and past the request status last answered when
// either is not earlier. Two calls within one instant therefore write two
// requests, and a value status already records as answered is never written
// again: the controller would take it as no request at all.
//
// The controller answers the request when no operation is running, the
// resource is not suspended and no retry delay is pending. A read that was
// already running when the request arrived does not answer it. That the
// annotation was written says nothing about whether the read completed; Wait
// with TargetRead does.
func Refresh(ctx context.Context, c client.Client, ref Ref, now time.Time) (RefreshResult, error) {
	const attempts = 3
	var lastErr error
	for range attempts {
		object, err := ref.newObject()
		if err != nil {
			return RefreshResult{}, err
		}
		if err := c.Get(ctx, ref.key(), object); err != nil {
			return RefreshResult{}, ref.failed("read", err)
		}
		read, err := snapshotOf(object)
		if err != nil {
			return RefreshResult{}, err
		}
		request := NextReadRequest(now, read.readRequest, read.lastReadRequest)
		// The API server leaves an empty map out of what it returns, so a
		// resource with no annotations has no member to add a key to.
		bare := len(object.GetAnnotations()) == 0
		operations := []patchOperation{{Op: "test", Path: "/metadata/uid", Value: read.uid}}
		if bare {
			// The whole map is added only where there is none. The test makes
			// that true at the API server too: an annotation another writer
			// added since the read fails it rather than being replaced.
			// Kubernetes applies JSON patches with a library whose test
			// operation accepts null for a member that is absent.
			operations = append(operations,
				patchOperation{Op: "test", Path: "/metadata/annotations", Value: nil},
				patchOperation{Op: "add", Path: "/metadata/annotations",
					Value: map[string]string{operatorv1alpha1.ReadRequestAnnotation: request}},
			)
		} else {
			operations = append(operations, patchOperation{
				Op: "add", Path: "/metadata/annotations/" + pointerEscape(operatorv1alpha1.ReadRequestAnnotation), Value: request,
			})
		}
		patch, err := json.Marshal(operations)
		if err != nil {
			return RefreshResult{}, err
		}
		err = c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(FieldManager))
		if err == nil {
			written, err := snapshotOf(object)
			if err != nil {
				return RefreshResult{}, err
			}
			return RefreshResult{
				Ref: ref, Request: request, Previous: read.readRequest, UID: written.uid,
				Suspended: written.suspend, Object: written.object,
			}, nil
		}
		lastErr = classifyPatchError(ctx, c, ref, read.uid, &bare, err)
		if !errors.Is(lastErr, errAnnotationsChanged) {
			return RefreshResult{}, lastErr
		}
	}
	return RefreshResult{}, lastErr
}

// errAnnotationsChanged is a patch built for a resource with no annotations
// that met some, or for one with annotations that met none. Either way it is
// built again from a fresh read.
var errAnnotationsChanged = errors.New("annotations were added or removed since the resource was read")

// NextReadRequest is the read request value Refresh writes at now: now in
// UTC, moved one nanosecond past current and answered whenever either is an
// RFC 3339 instant not earlier than now.
func NextReadRequest(now time.Time, current, answered string) string {
	at := now.UTC()
	for _, prior := range []string{current, answered} {
		if instant, err := time.Parse(time.RFC3339, prior); err == nil && !at.After(instant) {
			at = instant.UTC().Add(time.Nanosecond)
		}
	}
	return at.Format(time.RFC3339Nano)
}

type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// pointerEscape escapes one JSON Pointer reference token (RFC 6901).
func pointerEscape(token string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(token)
}

// classifyPatchError says what a refused patch means. The API server answers
// a failed test operation with 422 and a message from the patch library, so
// rather than reading the message the resource is read again: another UID, or
// none, is a resource replaced or removed since it was read. A patch built
// for annotations that are no longer there, or for none where there are now
// some, met another writer and is worth building again; bare is what the
// patch assumed, nil for a patch that does not touch annotations.
func classifyPatchError(ctx context.Context, c client.Client, ref Ref, uid types.UID, bare *bool, patchErr error) error {
	sent := sentError{patchErr}
	if apierrors.IsNotFound(patchErr) {
		return ref.failed("change", sent)
	}
	current, err := ref.newObject()
	if err != nil {
		return err
	}
	if err := c.Get(ctx, ref.key(), current); err != nil {
		if apierrors.IsNotFound(err) {
			return ref.failed("change", err)
		}
		return ref.failed("change", sent)
	}
	if current.GetUID() != uid {
		return ref.failed("change", ErrReplaced)
	}
	if bare != nil && *bare != (len(current.GetAnnotations()) == 0) {
		return ref.failed("change", errors.Join(errAnnotationsChanged, sent))
	}
	return ref.failed("change", sent)
}

// sentError is the API server's answer to a patch it was sent, or the error
// that took the place of an answer.
type sentError struct{ err error }

func (e sentError) Error() string { return e.err.Error() }
func (e sentError) Unwrap() error { return e.err }

// Answer is what an error SetSuspend or Refresh returned says about the
// change.
type Answer string

const (
	// NotRead is a resource that could not be read. Nothing was sent.
	NotRead Answer = "NotRead"
	// Absent is a resource that is not there, before the change was sent or
	// when it arrived. Nothing was changed.
	Absent Answer = "Absent"
	// Replaced is ErrReplaced: the resource under the name is another one
	// than the one read. Nothing was changed.
	Replaced Answer = "Replaced"
	// Forbidden is RBAC refusing the change to the identity that sent it.
	// Nothing was changed.
	Forbidden Answer = "Forbidden"
	// Refused is any other definite refusal of the change: admission, the
	// API schema, a credential the API server did not accept, or a request
	// it was too busy to take. Nothing was changed.
	Refused Answer = "Refused"
	// Unknown is a change that was sent and that no answer settled: a
	// timeout, a server error or a lost connection. It may have been
	// stored.
	Unknown Answer = "Unknown"
)

// Classify reads an error SetSuspend or Refresh returned. It reads the
// API server's refusal the way kubectl ptah approve does (internal/apirefusal),
// so a person refused by RBAC is told so in the same terms by every command
// that writes.
func Classify(err error) Answer {
	var sent sentError
	switch {
	case errors.Is(err, ErrReplaced):
		return Replaced
	case apierrors.IsNotFound(err):
		return Absent
	case !errors.As(err, &sent):
		return NotRead
	case !apirefusal.Definite(sent.err):
		return Unknown
	case apirefusal.ByRBAC(sent.err):
		return Forbidden
	default:
		return Refused
	}
}
