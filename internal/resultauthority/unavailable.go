package resultauthority

import (
	"context"
	"errors"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// LogTransientFailures wraps the receiver's authorization so that a check
// which reached no verdict, which the receiver answers with 503, leaves a line
// saying why. A refusal (ErrAuthority) is the 403 itself and is not logged.
// Only the receiver wraps its check: admission and credential issuance call
// Authorizer.Check directly and answer their own callers.
func LogTransientFailures(authorize func(context.Context, resultdelivery.Identity) error) func(context.Context, resultdelivery.Identity) error {
	return func(ctx context.Context, identity resultdelivery.Identity) error {
		err := authorize(ctx, identity)
		switch {
		case err == nil, errors.Is(err, resultdelivery.ErrAuthority):
		case errors.Is(err, ErrNotReady):
			logUnavailable(ctx, identity, "result authorization is waiting for the Job's adoption or the Pod's pin", nil)
		default:
			logUnavailable(ctx, identity, "result authorization reached no verdict", err)
		}
		return err
	}
}

// logUnavailable says why a verification or an authorization reached no
// verdict, which the receiver answers with 503. It logs the API server's
// status code and reason, or for an error without a status whether the
// deadline passed, and never an error's text or a review's status.error: an
// API error can repeat submitted fields, and status.error is the
// authenticator's own diagnostic. The Pod is the one the request names, which
// a failed TokenReview has not proved.
func logUnavailable(ctx context.Context, identity resultdelivery.Identity, message string, err error) {
	values := []any{"namespace", identity.Binding.Namespace, "pod", identity.Binding.PodName}
	var status apierrors.APIStatus
	switch {
	case errors.As(err, &status):
		values = append(values, "code", status.Status().Code, "reason", string(status.Status().Reason))
	case err != nil:
		values = append(values, "deadlineExceeded", errors.Is(err, context.DeadlineExceeded))
	}
	logf.FromContext(ctx).WithName("result-receiver").Info(message, values...)
}
