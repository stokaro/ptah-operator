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
		return &targetFailureError{kind: kind, url: url}
	}
	return fmt.Errorf("%s: %s", failure.Code, bounded(failure.Message, 512))
}

// targetFailureError is a failure that names why an operation could not open
// its target. It reads as the operator's sentence. The path that records the
// failure reads the kind from it, so the sentence and DatabaseReachable land in
// one status write.
type targetFailureError struct {
	kind dataplane.TargetFailureKind
	url  corev1.SecretKeySelector
}

func (e *targetFailureError) Error() string { return targetfailure.Message(e.kind, e.url) }

// databaseReachableReasons is the reason DatabaseReachable carries for each
// kind. A consumer keys an alert on it, because a condition's message may
// change without an API version.
var databaseReachableReasons = map[dataplane.TargetFailureKind]operatorv1alpha1.ConditionReason{
	dataplane.TargetDatabaseUnreachable:  operatorv1alpha1.ReasonDatabaseUnreachable,
	dataplane.TargetAuthenticationFailed: operatorv1alpha1.ReasonAuthenticationFailed,
	dataplane.TargetDatabaseNotFound:     operatorv1alpha1.ReasonDatabaseNotFound,
	dataplane.TargetPermissionDenied:     operatorv1alpha1.ReasonPermissionDenied,
	dataplane.TargetTLSFailed:            operatorv1alpha1.ReasonTLSFailed,
}

// databaseUnreached is what DatabaseReachable says after failure. A failure
// that names no kind says nothing about the target: a development database, a
// registry and a kind this operator does not know all end here, so the caller
// leaves the condition as it was.
func databaseUnreached(failure error) (operatorv1alpha1.ConditionReason, string, bool) {
	var target *targetFailureError
	if !errors.As(failure, &target) {
		return "", "", false
	}
	reason, known := databaseReachableReasons[target.kind]
	return reason, target.Error(), known
}
