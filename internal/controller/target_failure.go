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
// the kind and, for a MySQL user that cannot see every object, the kinds of
// object, each from a closed vocabulary, are all that cross from the Pod.
func resultFailure(failure *runner.ResultError, url corev1.SecretKeySelector) error {
	if kind, known := dataplane.ParseTargetFailureKind(failure.Code); known {
		return &targetFailureError{kind: kind, url: url, unseenObjects: failure.UnseenObjects}
	}
	return resultCodeError{code: failure.Code, message: fmt.Sprintf("%s: %s", failure.Code, bounded(failure.Message, 512))}
}

// targetFailureError is a failure that names why an operation could not open
// its target. It reads as the operator's sentence. The path that records the
// failure reads the kind from it, so the sentence and DatabaseReachable land in
// one status write.
type targetFailureError struct {
	kind dataplane.TargetFailureKind
	url  corev1.SecretKeySelector
	// unseenObjects are the kinds of object a permission_denied read could
	// not see, when the runner named them.
	unseenObjects []string
}

func (e *targetFailureError) Error() string {
	if len(e.unseenObjects) != 0 {
		return targetfailure.UnseenObjectsMessage(e.kind, e.unseenObjects, e.url)
	}
	return targetfailure.Message(e.kind, e.url)
}

// uncertainApplyEvent is the text of the ApplyOutcomeUnknown Event. A result that
// named a kind says why the Pod that reported it could not open the target, and
// keeps as much of the sentence as any failure Event. Every other Apply error
// keeps the fixed text, because what it holds may quote the runner. Either way the
// outcome stays unknown: one Pod's report does not speak for its Job.
func uncertainApplyEvent(failure error) string {
	const fixed = "Apply outcome is uncertain; observing database state"
	var target *targetFailureError
	if errors.As(failure, &target) {
		return fixed + ": " + bounded(target.Error(), 512)
	}
	return fixed
}

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

// planReadTheTarget reports an error code the runner writes for a Plan only after
// a read came back from the target, so the failed Plan still opened it. The retry
// is another Plan with no Observe before it, and a refusal that repeats would keep
// an old reason for as long as it repeats. invalid_plan_output is not one of these:
// the runner also writes it for a child that never reached the target.
//
// desired_schema_invalid is one. Ptah connects and pings before it loads the
// file, and the runner writes the code only where the failed sentence ends with
// the comparison's or a declaration's fault, which a connect failure never does.
// desired_source_unreadable is not. A parse error also comes after a reply, but
// the runner writes the code when the Plan failed in a way no target failure
// kind names and the file would not have parsed either. A server that drops the
// connection is such a failure, and Ptah v0.23.0 names no kind for it on either
// engine.
func planReadTheTarget(code string) bool {
	switch code {
	case "plan_refused", "unstable_plan", "credential_leak", "plan_seal_failed", runner.CodeDesiredSchemaInvalid:
		return true
	}
	return false
}

// failureReadTheTarget reports an error code the runner writes for operation
// only after a read came back from the target. An Observe has one:
// desired_schema_invalid, for the same reason as a Plan. Ptah's drift connects
// and reads the database before it compares, and the runner writes the code
// only where the command's sentence ends with that comparison's fault. An
// Observe runs before a Plan and compares the same way, so a declaration that
// cannot be compared usually stops there, and its retry is another Observe.
func failureReadTheTarget(operation operatorv1alpha1.OperationType, code string) bool {
	switch operation {
	case operatorv1alpha1.OperationPlan:
		return planReadTheTarget(code)
	case operatorv1alpha1.OperationObserve:
		return code == runner.CodeDesiredSchemaInvalid
	}
	return false
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
