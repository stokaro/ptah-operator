package workload

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// releasedJobDirectory holds the Jobs v0.2.10 dispatches for the golden cases,
// as that release's builder writes them: every credential in a secretKeyRef
// and the OCI fetch run as Ptah itself. They were testdata/jobs before file
// delivery, and that release's builder writes the same bytes.
const releasedJobDirectory = "testdata/jobs-v0.2.10"

// A Job v0.2.10 dispatched is still running when the manager is upgraded, and
// the new manager holds it to the Job its claim rebuilds. Every golden case is
// read back in the released shape, and the rebuilt Job, rewritten, has to be
// that Job byte for byte.
func TestCarryCredentialDeliveryRebuildsEveryReleasedJob(t *testing.T) {
	t.Parallel()
	cases := goldenJobCases()
	entries, err := os.ReadDir(releasedJobDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(cases) {
		t.Fatalf("%s holds %d Jobs, want one for each of the %d golden cases", releasedJobDirectory, len(entries), len(cases))
	}
	rewritten := 0
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			released, err := os.ReadFile(filepath.Join(releasedJobDirectory, test.name+".json"))
			if err != nil {
				t.Fatalf("read the released Job: %v", err)
			}
			live := &batchv1.Job{}
			if err := json.Unmarshal(released, live); err != nil {
				t.Fatal(err)
			}
			expected, err := test.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			// v0.2.10 created every Job without a TTL. The claim matcher takes a
			// stored read-only Job without one (jobclaim), so the rewrite leaves
			// the TTL alone, and the comparison here is of everything else.
			expected.Spec.TTLSecondsAfterFinished = nil
			differed := !bytes.Equal(marshalJob(t, expected), released)
			if got := CarryCredentialDelivery(expected, live); got != differed {
				t.Fatalf("CarryCredentialDelivery() = %v for a released Job that differs from the rebuild: %v", got, differed)
			}
			if got := marshalJob(t, expected); !bytes.Equal(got, released) {
				line, want, have := firstDifferentLine(released, got)
				t.Fatalf("the rewritten Job differs from the released one first at line %d:\n want: %s\n  got: %s", line, want, have)
			}
			if differed {
				rewritten++
			}
		})
	}
	// Run after the subtests: they are not parallel, so the count is final.
	if rewritten == 0 {
		t.Fatal("no released Job differed from its rebuild, so nothing measured the rewrite")
	}
}

// A Job this release built is never rewritten, and the rewrite changes nothing
// a released Job does not differ in: a credential read from another Secret or
// key, or a fetch running something else, still fails the comparison.
func TestCarryCredentialDeliveryLeavesEverythingElse(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	// The Plan Job carries the database URL, the development URL and a
	// registry credential at once.
	build := func(t *testing.T) *batchv1.Job {
		t.Helper()
		schema := schemaFixture()
		setGoldenScheduling(&schema.Spec.Execution)
		operation := operationFixture(operatorv1alpha1.OperationPlan)
		operation.ObservationProtectedTables = []string{"public.orders", "audit.log"}
		job, err := builder.Build(schema, operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	released := func(t *testing.T) *batchv1.Job {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(releasedJobDirectory, "schema-plan-dev-fence-scheduling.json"))
		if err != nil {
			t.Fatal(err)
		}
		job := &batchv1.Job{}
		if err := json.Unmarshal(content, job); err != nil {
			t.Fatal(err)
		}
		return job
	}

	t.Run("a Job of this release", func(t *testing.T) {
		t.Parallel()
		expected, live := build(t), build(t)
		before := marshalJob(t, expected)
		if CarryCredentialDelivery(expected, live) {
			t.Fatal("CarryCredentialDelivery() rewrote the rebuild of a Job this release built")
		}
		if !bytes.Equal(marshalJob(t, expected), before) {
			t.Fatal("CarryCredentialDelivery() changed the rebuild of a Job this release built")
		}
	})

	for name, edit := range map[string]func(*batchv1.Job){
		"the database URL from another Secret": func(job *batchv1.Job) {
			releasedVariable(job, "ptah", runner.EnvDatabaseURL).ValueFrom.SecretKeyRef.Name = "another-database"
		},
		"the database URL from another key": func(job *batchv1.Job) {
			releasedVariable(job, "ptah", runner.EnvDatabaseURL).ValueFrom.SecretKeyRef.Key = "password"
		},
		"the database URL made optional": func(job *batchv1.Job) {
			optional := true
			releasedVariable(job, "ptah", runner.EnvDatabaseURL).ValueFrom.SecretKeyRef.Optional = &optional
		},
		"the registry password from another key": func(job *batchv1.Job) {
			releasedVariable(job, fetchContainerName, runner.EnvOCIPassword).ValueFrom.SecretKeyRef.Key = "user"
		},
		"the registry token required": func(job *batchv1.Job) {
			releasedVariable(job, fetchContainerName, runner.EnvOCIToken).ValueFrom.SecretKeyRef.Optional = nil
		},
		"a fetch that runs another command": func(job *batchv1.Job) {
			for index := range job.Spec.Template.Spec.InitContainers {
				if container := &job.Spec.Template.Spec.InitContainers[index]; container.Name == fetchContainerName {
					container.Args = append(container.Args, "--insecure")
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expected, live := build(t), released(t)
			edit(live)
			if !CarryCredentialDelivery(expected, live) {
				t.Fatal("CarryCredentialDelivery() did not rewrite the rebuild for a released Job")
			}
			if bytes.Equal(marshalJob(t, expected), marshalJob(t, live)) {
				t.Fatal("the rewritten rebuild equals a released Job edited outside where its credentials sit")
			}
		})
	}
}

func releasedVariable(job *batchv1.Job, container, name string) *corev1.EnvVar {
	template := &job.Spec.Template.Spec
	for _, containers := range [][]corev1.Container{template.InitContainers, template.Containers} {
		for index := range containers {
			if containers[index].Name != container {
				continue
			}
			for variable := range containers[index].Env {
				if containers[index].Env[variable].Name == name {
					return &containers[index].Env[variable]
				}
			}
		}
	}
	panic("the released Job has no " + name + " in " + container)
}

func marshalJob(t *testing.T, job *batchv1.Job) []byte {
	t.Helper()
	content, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}

// Every credential the builder writes is named by a variable the runner reads,
// and no container of any golden Job carries a credential in its environment.
func TestBuildNamesEveryCredentialByFile(t *testing.T) {
	t.Parallel()
	values := map[string]bool{}
	for _, credential := range runner.CredentialFiles() {
		values[credential.Value] = true
	}
	files := 0
	for _, test := range goldenJobCases() {
		job, err := test.build()
		if err != nil {
			t.Fatalf("%s: build: %v", test.name, err)
		}
		template := job.Spec.Template.Spec
		for _, container := range slices.Concat(template.InitContainers, template.Containers) {
			for _, variable := range container.Env {
				if values[variable.Name] {
					t.Errorf("%s: container %s carries %s in its environment", test.name, container.Name, variable.Name)
				}
				if strings.HasSuffix(variable.Name, "_FILE") && strings.HasPrefix(variable.Value, "/credentials/") {
					files++
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("no golden Job names a credential file, so nothing was examined")
	}
}
