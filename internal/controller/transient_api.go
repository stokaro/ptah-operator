package controller

import (
	"context"
	"errors"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// transientAPIError reports a read the API server may answer next time: it was
// unavailable, timed out, throttled the client or failed internally, or the
// connection to it broke. A claim that meets one returns it, and the workqueue
// retries the pass with the handoff it found still in status. Ending the pass
// Failed instead would end a Blocked resource's pass somewhere other than
// Blocked, and clear the date of its block (#1160).
//
// Every kind is named. An error that is not an API status is not a transport
// error for that reason alone: the verification policy binding refuses a
// mutable ConfigMap or a missing key with plain errors of its own, and those
// are faults a retry does not fix. A refusal the API server gives, such as
// NotFound or Forbidden, is not transient either.
func transientAPIError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err) || apierrors.IsUnexpectedServerError(err) {
		return true
	}
	// client-go reports a request that never reached an answer as a
	// *url.Error, which is a net.Error, around the dial or read that failed.
	var transport net.Error
	return errors.As(err, &transport) || utilnet.IsProbableEOF(err) || utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err) || errors.Is(err, context.DeadlineExceeded)
}
