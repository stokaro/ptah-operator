package e2e

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	gitResolveRefusalFixture = runner.CodeInvalidGitAccess + ": the Git credential is not granted for the source's host"
	ociResolveRefusalFixture = runner.CodeInvalidOCIAccess + ": registry credential is not granted for the OCI source authority"
	missingSecretFixture     = `secret "e2e-fetch-refused-database" not found`
)

// fetchRefusedFixture is a schema whose Observe fetch refused its source's
// access, as the rows wait for it: the Git source resolved, the Observe
// still claimed, and ReconciliationFailed saying what the step's record says.
func fetchRefusedFixture(message string) *ptahv1alpha1.PtahSchema {
	return &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status: ptahv1alpha1.PtahSchemaStatus{
			Phase:           ptahv1alpha1.PhaseFailed,
			Source:          ptahv1alpha1.SchemaSourceStatus{Git: &ptahv1alpha1.GitSourceRevision{Commit: strings.Repeat("3", 40)}},
			ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve, JobName: "e2e-fetch-refused-observe-2"},
			Conditions: []metav1.Condition{{
				Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue, ObservedGeneration: 1,
				Reason: string(ptahv1alpha1.ReasonOperationFailed), Message: message,
			}},
		},
	}
}

// TestRefusedAtFetchHoldsTheStepToTheResolvesWords accepts the refusal the
// Git row waits for and refuses each reading that would pass a row that did
// not prove it: the generic sentence a step that exited reads in, the Resolve's
// own refusal, another step's, another sentence, and a refusal a later Resolve
// could have made.
func TestRefusedAtFetchHoldsTheStepToTheResolvesWords(t *testing.T) {
	t.Parallel()
	refused := "the fetch-git-source step refused the source's access, so the run never started: " + gitResolveRefusalFixture
	if got := accessStepRefusal(gitFetchInit, gitResolveRefusalFixture); got != refused {
		t.Fatalf("accessStepRefusal() = %q, want %q", got, refused)
	}
	if err := refusedAtFetch(fetchRefusedFixture(refused), 1, gitFetchInit, gitResolveRefusalFixture); err != nil {
		t.Fatalf("refusedAtFetch() refused the reading the row waits for: %v", err)
	}
	for _, row := range []struct {
		name           string
		schema         func() *ptahv1alpha1.PtahSchema
		step, resolved string
	}{
		{name: "the step named by its exit status",
			schema: func() *ptahv1alpha1.PtahSchema {
				return fetchRefusedFixture("the fetch-git-source step exited 2, so the run never started")
			}},
		{name: "the Resolve's refusal", schema: func() *ptahv1alpha1.PtahSchema { return fetchRefusedFixture(gitResolveRefusalFixture) }},
		{name: "another step's refusal", schema: func() *ptahv1alpha1.PtahSchema {
			return fetchRefusedFixture(accessStepRefusal(httpsFetchInit, gitResolveRefusalFixture))
		}},
		{name: "another sentence", schema: func() *ptahv1alpha1.PtahSchema {
			return fetchRefusedFixture(accessStepRefusal(gitFetchInit, runner.CodeInvalidGitAccess+": an ssh:// Git source needs known hosts"))
		}},
		{name: "the sentence cut short", schema: func() *ptahv1alpha1.PtahSchema {
			return fetchRefusedFixture(strings.TrimSuffix(refused, " host"))
		}},
		{name: "a Resolve's refusal under another code", step: sourceGuardStep, resolved: gitResolveRefusalFixture,
			schema: func() *ptahv1alpha1.PtahSchema {
				return fetchRefusedFixture(accessStepRefusal(sourceGuardStep, gitResolveRefusalFixture))
			}},
		{name: "a step that checks no access", step: "fetch-schema", resolved: ociResolveRefusalFixture,
			schema: func() *ptahv1alpha1.PtahSchema {
				return fetchRefusedFixture(accessStepRefusal("fetch-schema", ociResolveRefusalFixture))
			}},
		// A step with no code would hold the Resolve's refusal to an empty
		// one, which only the step's own check refuses.
		{name: "a step that checks no access, with no code", step: "fetch-schema", resolved: ": the artifact is of another type",
			schema: func() *ptahv1alpha1.PtahSchema {
				return fetchRefusedFixture(accessStepRefusal("fetch-schema", ": the artifact is of another type"))
			}},
		{name: "no failure", schema: func() *ptahv1alpha1.PtahSchema {
			schema := fetchRefusedFixture(refused)
			schema.Status.Conditions[0].Status = metav1.ConditionFalse
			return schema
		}},
		{name: "another reason", schema: func() *ptahv1alpha1.PtahSchema {
			schema := fetchRefusedFixture(refused)
			schema.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonSucceeded)
			return schema
		}},
		{name: "an earlier generation", schema: func() *ptahv1alpha1.PtahSchema {
			schema := fetchRefusedFixture(refused)
			schema.Generation, schema.Status.Conditions[0].ObservedGeneration = 2, 1
			return schema
		}},
		{name: "a source no Resolve recorded", schema: func() *ptahv1alpha1.PtahSchema {
			schema := fetchRefusedFixture(refused)
			schema.Status.Source.Git = nil
			return schema
		}},
		{name: "a Resolve claimed", schema: func() *ptahv1alpha1.PtahSchema {
			schema := fetchRefusedFixture(refused)
			schema.Status.ActiveOperation.Type = ptahv1alpha1.OperationResolve
			return schema
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			step, resolved := gitFetchInit, gitResolveRefusalFixture
			if row.step != "" {
				step, resolved = row.step, row.resolved
			}
			schema := row.schema()
			if err := refusedAtFetch(schema, schema.Generation, step, resolved); err == nil {
				t.Fatalf("refusedAtFetch() accepted %+v", schema.Status.Conditions)
			}
		})
	}
}

// TestHeldBeforeFetchWaitsForTheKubeletsHold accepts an Observe the kubelet
// holds for the missing database Secret after a Resolve, and refuses a schema
// whose Resolve has not recorded the source, a hold of another operation, and
// a Pod that waits for something else.
func TestHeldBeforeFetchWaitsForTheKubeletsHold(t *testing.T) {
	t.Parallel()
	held := func() *ptahv1alpha1.PtahSchema {
		return &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
			Source:          ptahv1alpha1.SchemaSourceStatus{ResolvedReference: "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64)},
			ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve, JobName: "e2e-fetch-refused-observe-1"},
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "PodConfigurationError",
				Message: `the Pod of Job e2e-fetch-refused-observe-1 cannot start: MountVolume.SetUp failed for volume "database-url" : ` + missingSecretFixture,
			}},
		}}
	}
	if err := heldBeforeFetch(held(), missingSecretFixture); err != nil {
		t.Fatalf("heldBeforeFetch() refused the hold: %v", err)
	}
	for name, change := range map[string]func(*ptahv1alpha1.PtahSchema){
		"no source recorded": func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Source.ResolvedReference = "" },
		"a Resolve held": func(schema *ptahv1alpha1.PtahSchema) {
			schema.Status.ActiveOperation.Type = ptahv1alpha1.OperationResolve
		},
		"no Job dispatched": func(schema *ptahv1alpha1.PtahSchema) { schema.Status.ActiveOperation.JobName = "" },
		"no operation":      func(schema *ptahv1alpha1.PtahSchema) { schema.Status.ActiveOperation = nil },
		"another missing Secret": func(schema *ptahv1alpha1.PtahSchema) {
			schema.Status.Conditions[0].Message = `secret "other" not found`
		},
		"a Pod that started": func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].Status = metav1.ConditionTrue },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := held()
			change(schema)
			if err := heldBeforeFetch(schema, missingSecretFixture); err == nil {
				t.Fatalf("heldBeforeFetch() accepted %+v", schema.Status)
			}
		})
	}
}

// TestFailedAtReadsOnlyAFailureOfTheGeneration accepts any failure of the
// generation the rows hold, which ends their wait, and refuses a schema that
// has not failed, one whose failure is of another generation, and one with no
// failure recorded at all.
func TestFailedAtReadsOnlyAFailureOfTheGeneration(t *testing.T) {
	t.Parallel()
	generic := "the fetch-git-source step exited 2, so the run never started"
	if !failedAt(fetchRefusedFixture(generic), 1) {
		t.Fatal("failedAt() refused a failure of the generation")
	}
	for name, change := range map[string]func(*ptahv1alpha1.PtahSchema){
		"not failed":         func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].Status = metav1.ConditionFalse },
		"another generation": func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].ObservedGeneration = 2 },
		"no failure":         func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := fetchRefusedFixture(generic)
			change(schema)
			if failedAt(schema, 1) {
				t.Fatalf("failedAt() accepted %+v", schema.Status.Conditions)
			}
		})
	}
}
