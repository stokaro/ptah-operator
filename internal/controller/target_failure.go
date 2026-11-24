package controller

import (
	"errors"
	"fmt"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
	"github.com/stokaro/ptah-operator/internal/urlsource"
)

// operationURLSource is where an operation read its target URL from: the
// place its claim bound when it dispatched, and the resource's when a claim
// carries none.
func operationURLSource(bound *operatorv1alpha1.DatabaseTargetBinding, spec operatorv1alpha1.DatabaseTargetSpec) urlsource.Source {
	if bound != nil {
		if source := urlsource.OfBinding(*bound); source.Validate() == nil {
			return source
		}
	}
	return urlsource.OfTarget(spec)
}

// codeMissingTarget is the runner's code for an operation that had no target
// URL to open: the variable naming the URL file was unset, or the file it
// named was not there.
const codeMissingTarget = "missing_target"

// resultFailure is how a runner result's error reads in status and in an
// Event: its code and its bounded message, as the runner wrote them, or, for
// a target the operation could not open, the operator's own sentence
// (targetfailure.Message). The runner's message for that code is not read, so
// the kind and, for a MySQL user that cannot see every object, the kinds of
// object, each from a closed vocabulary, are all that cross from the Pod.
//
// A URL from a SecretProviderClass that the runner found missing is the
// operator's sentence too. The class decides which files the driver writes,
// so an objectName the class does not write mounts cleanly and leaves no file,
// and the runner's words name only the variable it could not set. The runner
// does not say whether the file was missing or empty, so the sentence names
// both.
func resultFailure(failure *runner.ResultError, url urlsource.Source) error {
	if kind, known := dataplane.ParseTargetFailureKind(failure.Code); known {
		return &targetFailureError{kind: kind, url: url, unseenObjects: failure.UnseenObjects}
	}
	if failure.Code == codeMissingTarget && url.Class != nil {
		return resultCodeError{code: failure.Code, message: fmt.Sprintf(
			"%s: the operation found no URL in file %q (missing or empty) in the volume of SecretProviderClass %q. "+
				"Check that the class writes an object under that name, as its objectName or its objectAlias",
			failure.Code, url.Class.ObjectName, url.Class.Name)}
	}
	return resultCodeError{code: failure.Code, message: resultCodeMessage(failure.Code, failure.Message)}
}

// schemaResultFailure is resultFailure for a schema operation, which also
// names the development database a Plan read. The runner refuses a Plan whose
// development database file holds no URL (runner.CodeMissingDev) before Ptah
// starts, and the sentence names the place the URL comes from, for the reason
// resultFailure names a target's: a SecretProviderClass object, missing or
// empty, or a Secret key, which the kubelet holds present, holding nothing.
func schemaResultFailure(failure *runner.ResultError, operation *operatorv1alpha1.ActiveOperationStatus,
	spec operatorv1alpha1.DatabaseTargetSpec,
) error {
	if failure.Code == runner.CodeMissingDev && operation.ObservationDev != nil {
		dev := urlsource.OfRef(*operation.ObservationDev)
		switch {
		case dev.Class != nil:
			return resultCodeError{code: failure.Code, message: fmt.Sprintf(
				"%s: the Plan found no URL in file %q (missing or empty) in the volume of SecretProviderClass %q, "+
					"and computes no plan without its development database. Check that the class writes an object "+
					"under that name, as its objectName or its objectAlias",
				failure.Code, dev.Class.ObjectName, dev.Class.Name)}
		case dev.Secret != nil:
			return resultCodeError{code: failure.Code, message: fmt.Sprintf(
				"%s: the development database Secret key holds no URL, and the Plan computes no plan without its "+
					"development database. Check the URL that %s",
				failure.Code, dev.Holder())}
		}
	}
	return resultFailure(failure, operationURLSource(operation.Target, spec))
}

// resultCodeMessage is how a runner's code and message read in status: the
// code, then the message as the runner wrote it, bounded. A frame's error and
// an init container's access refusal read the same through it.
func resultCodeMessage(code, message string) string {
	return fmt.Sprintf("%s: %s", code, bounded(message, 512))
}

// targetFailureError is a failure that names why an operation could not open
// its target. It reads as the operator's sentence. The path that records the
// failure reads the kind from it, so the sentence and DatabaseReachable land in
// one status write.
type targetFailureError struct {
	kind dataplane.TargetFailureKind
	url  urlsource.Source
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

// applyEndingSentence is how an Apply ended, in the operator's own words. It
// quotes nothing the runner wrote, so the ApplyOutcomeUnknown Event of a deleted
// PtahSchema may carry it: the status goes with the resource, and the Event is
// what says how the Apply ended.
type applyEndingSentence string

func (e applyEndingSentence) Error() string { return string(e) }

// uncertainApplyEvent is the text of the ApplyOutcomeUnknown Event. A result that
// named a kind says why the Pod that reported it could not open the target, and
// keeps as much of the sentence as any failure Event. Every other Apply error
// keeps the fixed text, because what it holds may quote the runner. Either way the
// outcome stays unknown: one Pod's report does not speak for its Job.
//
// A deleting schema is never observed again, so its text says so in place of
// observing database state, and names how the Apply ended when the failure
// carries the operator's sentence (applyEndingSentence). It is keyed on the
// deletion rather than on the sentence, so an ending that carries none still
// says the right thing.
//
// What the schema last reported about a Pod that never ran follows, when the
// failure carries it (withLastPodReport). That is the condition's own message,
// the kubelet's or the API server's words about names, and on a deleted schema
// this Event is the only place it is left.
func uncertainApplyEvent(failure error, deleting bool) string {
	text := "Apply outcome is uncertain; observing database state"
	if deleting {
		text = "Apply outcome is uncertain; the PtahSchema is being deleted, so nothing will observe the database"
		var ending applyEndingSentence
		if errors.As(failure, &ending) {
			text += ": " + string(ending)
		}
	}
	var target *targetFailureError
	if errors.As(failure, &target) {
		text += ": " + bounded(target.Error(), 512)
	}
	var report *lastPodReportError
	if errors.As(failure, &report) {
		text += "; last reported before that: " + bounded(report.reported, 512)
	}
	return text
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
// plan_refused is one too: Ptah refuses a plan once it has compared the schema
// with the database, and the dry run of a saved plan runs only after that plan.
// A Plan that ends with either is not retried: the schema stops
// Blocked (refuseSchemaSource), and the read that opened the target is recorded
// there.
// desired_source_unreadable is not. A parse error also comes after a reply, but
// the runner writes the code when the Plan failed in a way no target failure
// kind names and the file would not have parsed either. A server that drops the
// connection is such a failure, and Ptah v0.23.0 names no kind for it on either
// engine.
func planReadTheTarget(code string) bool {
	switch code {
	case runner.CodePlanRefused, "unstable_plan", "credential_leak", "plan_seal_failed", runner.CodeDesiredSchemaInvalid:
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
// cannot be compared usually stops there, and the schema stops Blocked.
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
