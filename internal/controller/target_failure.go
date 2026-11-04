package controller

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
)

// operationURLSecret is the Secret key an operation read its target URL from:
// the one its claim bound when it dispatched, and the resource's when a claim
// carries none.
func operationURLSecret(bound *operatorv1alpha1.DatabaseTargetBinding, spec corev1.SecretKeySelector) corev1.SecretKeySelector {
	if bound != nil && bound.URLFrom.Name != "" {
		return bound.URLFrom
	}
	return spec
}

// resultFailure is how a runner result's error reads in status and in an
// Event: its code and its bounded message, as the runner wrote them, or, for
// a target the operation could not open, the operator's own sentence
// (targetfailure.Message). The runner's message for that code is not read, so
// the kind, from a closed vocabulary, is all that crosses from the Pod.
func resultFailure(failure *runner.ResultError, url corev1.SecretKeySelector) error {
	if kind, known := dataplane.ParseTargetFailureKind(failure.Code); known {
		return errors.New(targetfailure.Message(kind, url))
	}
	return fmt.Errorf("%s: %s", failure.Code, bounded(failure.Message, 512))
}
