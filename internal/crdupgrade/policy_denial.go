package crdupgrade

import (
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validatingAdmissionPolicyDenialCauseMessage(policyName, bindingName, denialMessage string) string {
	return fmt.Sprintf(
		"ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s",
		policyName,
		bindingName,
		denialMessage,
	)
}

// hasExactValidatingAdmissionPolicyDenial recognizes only the denial envelope
// emitted throughout the supported Kubernetes window. Any server-side shape
// change fails closed until the supported-window contract is updated.
func hasExactValidatingAdmissionPolicyDenial(err error, policyName, bindingName, denialMessage string) bool {
	var statusError apierrors.APIStatus
	if !errors.As(err, &statusError) {
		return false
	}
	status := statusError.Status()
	if status.Status != metav1.StatusFailure || status.Reason != metav1.StatusReasonInvalid || status.Code != 422 ||
		status.Details == nil || len(status.Details.Causes) != 1 {
		return false
	}
	want := validatingAdmissionPolicyDenialCauseMessage(policyName, bindingName, denialMessage)
	cause := status.Details.Causes[0]
	return cause.Type == "" && cause.Field == "" && cause.Message == want
}

// isValidatingAdmissionPolicyDenial reports whether the API server refused the
// request through a ValidatingAdmissionPolicy, without naming which one. It is
// deliberately narrower than IsInvalid: only the envelope an admission policy
// produces qualifies, so a transport failure or a client-side budget never
// looks like a denial a caller may wait out.
func isValidatingAdmissionPolicyDenial(err error) bool {
	var statusError apierrors.APIStatus
	if !errors.As(err, &statusError) {
		return false
	}
	status := statusError.Status()
	if status.Status != metav1.StatusFailure || status.Reason != metav1.StatusReasonInvalid || status.Code != 422 ||
		status.Details == nil || len(status.Details.Causes) == 0 {
		return false
	}
	for _, cause := range status.Details.Causes {
		if strings.Contains(cause.Message, "ValidatingAdmissionPolicy") && strings.Contains(cause.Message, "denied request") {
			return true
		}
	}
	return false
}

// HasExactValidatingAdmissionPolicyDenial exposes the supported-window denial
// envelope check to command adapters that probe a separately owned policy.
// Callers must still validate that policy's immutable stored contract.
func HasExactValidatingAdmissionPolicyDenial(err error, policyName, bindingName, denialMessage string) bool {
	return hasExactValidatingAdmissionPolicyDenial(err, policyName, bindingName, denialMessage)
}
