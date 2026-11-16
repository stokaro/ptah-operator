package e2e

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// sourceGuardStep is the init container that checks an OCI source's access
// before the fetch that holds the registry credential. A Git or an HTTPS fetch
// checks the access itself (gitFetchInit, httpsFetchInit).
const sourceGuardStep = "validate-source-authority"

// accessStepCodes is the code each step that checks a source's access refuses
// it under: the code a Resolve's frame gives the same refusal.
var accessStepCodes = map[string]string{
	sourceGuardStep: runner.CodeInvalidOCIAccess,
	gitFetchInit:    runner.CodeInvalidGitAccess,
	httpsFetchInit:  runner.CodeInvalidHTTPSAccess,
}

// accessStepRefusal is what the manager says when the step that checks a
// source's access before a fetch refused it: the step, and then the Resolve's
// own refusal of the same access, whole.
func accessStepRefusal(step, resolveRefusal string) string {
	return "the " + step + " step refused the source's access, so the run never started: " + resolveRefusal
}

// sourceResolved is a schema whose Resolve recorded what it found, of any kind
// of source.
func sourceResolved(source ptahv1alpha1.SchemaSourceStatus) bool {
	return source.Git != nil || source.HTTPS != nil || source.ResolvedReference != ""
}

// heldBeforeFetch is a schema whose Resolve passed and whose Observe the
// kubelet holds before any container starts, for the missing Secret words
// names: Ready False with PodConfigurationError. While it holds, no step has
// checked the source's access for that read.
func heldBeforeFetch(schema *ptahv1alpha1.PtahSchema, words string) error {
	operation := schema.Status.ActiveOperation
	switch {
	case !sourceResolved(schema.Status.Source):
		return errors.New("no Resolve has recorded the source yet")
	case operation == nil || operation.Type != ptahv1alpha1.OperationObserve || operation.JobName == "":
		return fmt.Errorf("the active operation is %+v, not a dispatched Observe", operation)
	case !podConfigurationReported(schema, words):
		return fmt.Errorf("Ready does not say the Pod waits for %q: %+v", words, meta.FindStatusCondition(schema.Status.Conditions, "Ready"))
	}
	return nil
}

// failedAt is a schema whose ReconciliationFailed is True for generation,
// whatever it says.
func failedAt(schema *ptahv1alpha1.PtahSchema, generation int64) bool {
	failed := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
	return failed != nil && failed.Status == metav1.ConditionTrue && failed.ObservedGeneration == generation
}

// refusedAtFetch holds a schema at generation to the refusal of its source's
// access by step, the init container that checks it before an Observe's
// fetch: ReconciliationFailed True with OperationFailed, observed at that
// generation, saying exactly accessStepRefusal(step, resolveRefusal). The
// source is still resolved and the Observe still claimed, so the refusal is
// the fetch's and not a later Resolve's, and resolveRefusal has to be under
// the code step refuses with.
//
// The Resolve's own words, the sentence that names a step only by its exit
// status, and another step's refusal are each refused.
func refusedAtFetch(schema *ptahv1alpha1.PtahSchema, generation int64, step, resolveRefusal string) error {
	code, checks := accessStepCodes[step]
	if !checks {
		return fmt.Errorf("%s is not a step that checks a source's access", step)
	}
	if !strings.HasPrefix(resolveRefusal, code+": ") {
		return fmt.Errorf("the Resolve's refusal %q is not under %s", resolveRefusal, code)
	}
	failed := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
	operation := schema.Status.ActiveOperation
	want := accessStepRefusal(step, resolveRefusal)
	switch {
	case failed == nil || failed.Status != metav1.ConditionTrue:
		return fmt.Errorf("ReconciliationFailed is %+v", failed)
	case failed.ObservedGeneration != generation:
		return fmt.Errorf("ReconciliationFailed is of generation %d, not %d: %s", failed.ObservedGeneration, generation, failed.Message)
	case failed.Reason != string(ptahv1alpha1.ReasonOperationFailed):
		return fmt.Errorf("ReconciliationFailed has the reason %s, want OperationFailed: %s", failed.Reason, failed.Message)
	case failed.Message != want:
		return fmt.Errorf("ReconciliationFailed says %q, want %q", failed.Message, want)
	case !sourceResolved(schema.Status.Source):
		return errors.New("the source is no longer resolved, so a Resolve may have refused it")
	case operation == nil || operation.Type != ptahv1alpha1.OperationObserve:
		return fmt.Errorf("the active operation is %+v, not the Observe whose fetch refused", operation)
	}
	return nil
}
