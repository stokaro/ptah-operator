package e2e

import (
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// admissionAnswered says whether a server-side dry run reached the operator's
// webhooks. Any client error the API server returns after calling them, a
// webhook's denial or the schema validation that follows mutation, means the
// webhook Service routed the request. A server error, a "failed calling
// webhook" refusal or anything that is not an API status means it did not.
func admissionAnswered(err error) bool {
	if err == nil {
		return true
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	return code >= 400 && code < 500 && !strings.Contains(status.Status().Message, "failed calling webhook")
}
