package crdupgrade

import (
	"errors"
	"fmt"

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
