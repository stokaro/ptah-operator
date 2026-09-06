package kubeapi

import (
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// IsClientRateLimitBudgetError identifies a local request that client-go could
// not schedule inside its remaining context budget. The limiter can refuse the
// reservation before the context expires, so ctx.Err() is insufficient.
//
// Neither client-go nor its token bucket exports a typed error for this case.
// Match the exact client-go wrapper and its unwrapped one-token budget error,
// never an API response that happens to contain the same diagnostic text.
// A real client-go request test pins this dependency contract.
func IsClientRateLimitBudgetError(err error) bool {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return false
	}
	const budgetError = "rate: Wait(n=1) would exceed context deadline"
	const clientError = "client rate limiter Wait returned an error: " + budgetError
	for current := err; current != nil; current = errors.Unwrap(current) {
		cause := errors.Unwrap(current)
		if cause != nil && current.Error() == clientError && cause.Error() == budgetError {
			return true
		}
	}
	return false
}
