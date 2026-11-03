package controller

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// targetFailureChecks says, for each kind of failure to open the target, where
// a person looks next. Each names the Secret key the operation read its URL
// from by the name and key the resource gave, which are the resource's own
// data; nothing the operation Pod wrote is in it.
var targetFailureChecks = map[dataplane.TargetFailureKind]string{
	dataplane.TargetDatabaseUnreachable: "Check the host and port in the URL that Secret %q holds under key %q, " +
		"and that the database is running and accepts connections from this namespace",
	dataplane.TargetAuthenticationFailed: "Check the user and password in the URL that Secret %q holds under key %q",
	dataplane.TargetDatabaseNotFound: "Check the database name in the URL that Secret %q holds under key %q, " +
		"or create the database",
	dataplane.TargetPermissionDenied: "Check the user in the URL that Secret %q holds under key %q, " +
		"or grant that user access to the database",
	dataplane.TargetTLSFailed: "Check the TLS settings in the URL that Secret %q holds under key %q " +
		"against the certificate the server presents",
}

// targetFailureMessage is what a condition, an Event and kubectl ptah say
// about an operation that could not open its target: the kind, what it means,
// and the Secret to look in. Every word of it is the operator's own.
func targetFailureMessage(kind dataplane.TargetFailureKind, url corev1.SecretKeySelector) string {
	return fmt.Sprintf("%s: %s. "+targetFailureChecks[kind], kind, kind.Description(), url.Name, url.Key)
}

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
// a target the operation could not open, targetFailureMessage. The runner's
// message for that code is not read, so the kind, from a closed vocabulary, is
// all that crosses from the Pod.
func resultFailure(failure *runner.ResultError, url corev1.SecretKeySelector) error {
	if kind, known := dataplane.ParseTargetFailureKind(failure.Code); known {
		return errors.New(targetFailureMessage(kind, url))
	}
	return fmt.Errorf("%s: %s", failure.Code, bounded(failure.Message, 512))
}
