package crdupgrade

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	celgo "github.com/google/cel-go/cel"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// The workload builder's golden Jobs: what this release creates, and what
// v0.2.10 created for the same inputs, every credential in a secretKeyRef.
const (
	currentJobDirectory  = "../workload/testdata/jobs"
	releasedJobDirectory = "../workload/testdata/jobs-v0.2.10"
)

// The guard refuses, by name, every variable the runner reads a credential
// into, and no other: a credential the runner learns to read is a name the
// guard has to learn too.
func TestControllerJobCredentialEnvironmentNamesAreTheRunners(t *testing.T) {
	t.Parallel()
	var values []string
	for _, credential := range runner.CredentialFiles() {
		values = append(values, credential.Value)
	}
	slices.Sort(values)
	names := slices.Sorted(slices.Values(controllerJobCredentialEnvironmentNames))
	if !slices.Equal(names, values) {
		t.Fatalf("the guard refuses %v in a Job's environment, want the credentials the runner reads from files, %v", names, values)
	}
	grants := []string{
		runner.EnvOCIAuthRegistryGrant, runner.EnvOCIAllowPlainHTTPGrant, runner.EnvOCICASHA256Grant,
		runner.EnvGitHostGrant, runner.EnvGitCASHA256Grant, runner.EnvGitKnownHostsSHA256Grant,
		runner.EnvHTTPSHostGrant, runner.EnvHTTPSCASHA256Grant,
	}
	slices.Sort(grants)
	if got := slices.Sorted(slices.Values(controllerJobSecretEnvironmentGrants)); !slices.Equal(got, grants) {
		t.Fatalf("the guard reads %v from a Secret into the environment, want the runner's grants, %v", got, grants)
	}
}

// Every Job the builder writes for the golden cases is admitted on create, by
// every validation of the Job guard.
func TestControllerJobGuardAdmitsEveryGoldenJob(t *testing.T) {
	t.Parallel()
	jobs := goldenJobObjects(t, currentJobDirectory)
	for name, job := range jobs {
		for index, validation := range controllerJobWriteValidations("refused") {
			if !evaluateJobValidation(t, validation.Expression, job, nil, "CREATE") {
				t.Errorf("%s: validation %d (%s) refused the Job the builder writes:\n%s", name, index, validation.Message, validation.Expression)
			}
		}
	}
}

// A Job v0.2.10 created is the manager's to clean up after an upgrade, so its
// cleanup TTL is admitted. This release never creates one: a create that
// carries a credential in its environment is refused by the credential
// validation, and by that one alone, so the refusal says why.
func TestControllerJobGuardTakesAReleasedJobOnlyForCleanup(t *testing.T) {
	t.Parallel()
	released := goldenJobObjects(t, releasedJobDirectory)
	current := goldenJobObjects(t, currentJobDirectory)
	refused := 0
	for name, job := range released {
		// v0.2.10 created every Job without a TTL, and this release creates a
		// read-only one with its cleanup TTL, so the two differ there whatever
		// they carry.
		carriesCredential := !jobObjectsEqual(t, job, withoutTTL(t, current[name]))
		for index, validation := range controllerJobWriteValidations("refused") {
			credentialRule := validation.Message == controllerJobCredentialEnvironmentMessage
			admitted := evaluateJobValidation(t, validation.Expression, job, nil, "CREATE")
			switch {
			case credentialRule && carriesCredential && admitted:
				t.Errorf("%s: the credential validation admitted the create of a Job that reads its credentials from its environment", name)
			case credentialRule && !carriesCredential && !admitted:
				t.Errorf("%s: the credential validation refused a released Job that carries no credential", name)
			case !credentialRule && !admitted:
				t.Errorf("%s: validation %d (%s) refused the create of a released Job, so the refusal does not say why", name, index, validation.Message)
			}
			if credentialRule && !admitted {
				refused++
			}

			finished, cleaned := cleanupTTLUpdate(t, job)
			if !evaluateJobValidation(t, validation.Expression, cleaned, finished, "UPDATE") {
				t.Errorf("%s: validation %d (%s) refused the cleanup TTL of a Job v0.2.10 created", name, index, validation.Message)
			}
		}
	}
	if refused == 0 {
		t.Fatal("no released Job carried a credential in its environment, so nothing measured the refusal")
	}
}

// goldenJobObjects reads every Job in directory as the API server hands it to
// a policy, with the admission snapshot annotation a dispatched Job carries:
// the golden cases are built without a claim's snapshot.
func goldenJobObjects(t *testing.T, directory string) map[string]map[string]any {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]map[string]any{}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		job := map[string]any{}
		if err := json.Unmarshal(content, &job); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		snapshot := "sha256:" + strings.Repeat("9", 64)
		metadata := job["metadata"].(map[string]any)
		annotations := metadata["annotations"].(map[string]any)
		annotations["operator.ptah.run/admission-snapshot-digest"] = snapshot
		template := job["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
		template["annotations"] = maps.Clone(annotations)
		jobs[strings.TrimSuffix(entry.Name(), ".json")] = job
	}
	if len(jobs) == 0 {
		t.Fatalf("%s holds no Job", directory)
	}
	return jobs
}

// withoutTTL is job without spec.ttlSecondsAfterFinished.
func withoutTTL(t *testing.T, job map[string]any) map[string]any {
	t.Helper()
	if job == nil {
		return nil
	}
	spec, ok := job["spec"].(map[string]any)
	if !ok {
		t.Fatal("a golden Job has no spec")
	}
	spec = maps.Clone(spec)
	delete(spec, "ttlSecondsAfterFinished")
	copied := maps.Clone(job)
	copied["spec"] = spec
	return copied
}

func jobObjectsEqual(t *testing.T, left, right map[string]any) bool {
	t.Helper()
	if right == nil {
		t.Fatal("a released Job has no current counterpart")
	}
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	return string(a) == string(b)
}

// cleanupTTLUpdate is the one update the manager makes to a Job it created:
// the cleanup TTL, once the Job finished.
func cleanupTTLUpdate(t *testing.T, job map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	finished, cleaned := map[string]any{}, map[string]any{}
	if err := json.Unmarshal(encoded, &finished); err != nil {
		t.Fatal(err)
	}
	finished["metadata"].(map[string]any)["uid"] = "released-job-uid"
	finished["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
	finishedEncoded, err := json.Marshal(finished)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(finishedEncoded, &cleaned); err != nil {
		t.Fatal(err)
	}
	cleaned["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(300)
	return finished, cleaned
}

// evaluateJobValidation is evaluateJobContract for either verb, with the
// release values of the Job's own annotations.
func evaluateJobValidation(t *testing.T, expression string, object, oldObject map[string]any, operation string) bool {
	t.Helper()
	program := compiledJobValidation(t, expression)
	annotations := object["metadata"].(map[string]any)["annotations"].(map[string]any)
	state := annotations["operator.ptah.run/controller-state-version"].(string)
	stateValue, err := strconv.ParseInt(state, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	var old any
	if oldObject != nil {
		old = oldObject
	}
	result, _, err := program.Eval(map[string]any{
		"object":    object,
		"oldObject": old,
		"request":   map[string]any{"operation": operation},
		"variables": map[string]any{
			"releaseControllerImage":       annotations["operator.ptah.run/controller-image"],
			"releaseControllerState":       stateValue,
			"releaseControllerStateString": state,
		},
	})
	if err != nil {
		t.Fatalf("evaluate Job validation: %v", err)
	}
	admitted, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("Job validation result = %T(%v), want bool", result.Value(), result.Value())
	}
	return admitted
}

// jobValidationPrograms holds each validation compiled once: the tests above
// evaluate every validation against every golden Job.
var jobValidationPrograms sync.Map

func compiledJobValidation(t *testing.T, expression string) celgo.Program {
	t.Helper()
	if program, ok := jobValidationPrograms.Load(expression); ok {
		return program.(celgo.Program)
	}
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("params", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile Job validation: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build Job validation: %v", err)
	}
	jobValidationPrograms.Store(expression, program)
	return program
}
