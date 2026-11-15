package workload

import (
	"encoding/csv"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/kubeletenv"
)

// processEnvironment is what the operation container's process reads, after
// the kubelet resolved the Job's variables. A Secret's value is not known
// here and is taken as empty; no literal may reference one.
func processEnvironment(t *testing.T, job *batchv1.Job) map[string]string {
	t.Helper()
	values, err := kubeletenv.Values(job.Spec.Template.Spec.Containers[0].Env,
		func(corev1.EnvVar) (string, bool, error) { return "", true, nil })
	if err != nil {
		t.Fatal(err)
	}
	return values
}

// processList is the list a variable carries, decoded from what the process
// reads the way the runner decodes it.
func processList(t *testing.T, job *batchv1.Job, name string) []string {
	t.Helper()
	value, present := processEnvironment(t, job)[name]
	if !present {
		t.Fatalf("the process reads no %s", name)
	}
	reader := csv.NewReader(strings.NewReader(value))
	reader.FieldsPerRecord = -1
	record, err := reader.Read()
	if err != nil {
		t.Fatalf("the process reads %s = %q, which is not one CSV record: %v", name, value, err)
	}
	return record
}

// A literal reaches the process as written, whatever $ it holds and whatever
// variable it names, including one declared before it, which the kubelet
// would otherwise substitute.
func TestLiteralEnvReachesTheProcessAsWritten(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"a$$b", "$(PTAH_ACTOR)", "$(EARLIER)", "$$(EARLIER)", "x$", "$", "$(", "$(EARLIER", "$x", "no operator", "",
	} {
		env := []corev1.EnvVar{literalEnv("EARLIER", "substituted"), literalEnv("PTAH_ACTOR", "someone"), literalEnv("V", value)}
		got, err := kubeletenv.Values(env, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got["V"] != value {
			t.Errorf("literalEnv(%q) reaches the process as %q", value, got["V"])
		}
	}
}

// What a person writes into a resource reaches the runner through literal
// variables: a migration's drift-check exclude list, a schema's exclude list
// and protected tables, and a source's address, path and branch. The API
// admits $ in each, as PostgreSQL and MySQL admit it in an identifier, so each
// is held to what the process reads once the kubelet expanded it. Before
// literalEnv escaped $, a$$b reached Ptah as a$b, a selector that matches
// another table or none.
func TestResourceValuesReachTheProcessAsWritten(t *testing.T) {
	t.Parallel()
	t.Run("a migration's drift-check exclude list", func(t *testing.T) {
		t.Parallel()
		// The Job declares its variables by name, so HOME comes before the
		// list and the kubelet would substitute $(HOME). PTAH_ACTOR comes
		// after it, and would not be.
		exclude := []string{"public.a$$b", "$(PTAH_ACTOR)", "$(HOME)", "audit.log$", "$"}
		migration := migrationFixture()
		migration.Spec.Policy.DriftCheck = &operatorv1alpha1.MigrationDriftCheck{
			Shadow: operatorv1alpha1.DatabaseTargetRef{URLFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "ledger-shadow"}, Key: "url",
			}},
			Exclude: exclude,
		}
		plan := migrationPlanFixture()
		plan.Spec.ShadowIdentityDigest = digest('5')
		job, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationApply), plan)
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Sorted(slices.Values(exclude))
		if got := processList(t, job, runner.EnvDriftExclude); !slices.Equal(got, want) {
			t.Fatalf("the process reads %s = %q, want %q", runner.EnvDriftExclude, got, want)
		}
	})
	t.Run("a schema's exclude list and protected tables", func(t *testing.T) {
		t.Parallel()
		// The protected tables are identifiers, which the API admits $ in
		// but not a parenthesis.
		exclude := []string{"public.a$$b", "$(PTAH_ACTOR)", "$(HOME)", "audit.log$", "$"}
		protected := []string{"public.a$$b", "x$", "audit.log$"}
		operation := operationFixture(operatorv1alpha1.OperationPlan)
		operation.ObservationExclude = exclude
		operation.ObservationProtectedTables = protected
		job, err := builderFixture().Build(schemaFixture(), operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := processList(t, job, "PTAH_EXCLUDE"); !slices.Equal(got, exclude) {
			t.Errorf("the process reads PTAH_EXCLUDE = %q, want %q", got, exclude)
		}
		if got := processList(t, job, "PTAH_PROTECTED_TABLES"); !slices.Equal(got, protected) {
			t.Errorf("the process reads PTAH_PROTECTED_TABLES = %q, want %q", got, protected)
		}
	})
	// A source's address, path and branch reach the fetch the same way, and
	// the API admits $ in each.
	t.Run("a Git source", func(t *testing.T) {
		t.Parallel()
		spec := gitHTTPSSpec("db/$$(HOME)/schema$.hcl")
		spec.URL, spec.Ref.Branch = "https://git.example.com/acme/a$$b.git", "release/$(PTAH_OPERATION_ID)"
		job, err := builderFixture().Build(gitSchemaFixture(spec), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatal(err)
		}
		read := processEnvironment(t, job)
		for name, want := range map[string]string{
			runner.EnvGitURL: spec.URL, runner.EnvGitPath: spec.Path, runner.EnvGitBranch: spec.Ref.Branch,
		} {
			if read[name] != want {
				t.Errorf("the process reads %s = %q, want %q", name, read[name], want)
			}
		}
	})
	t.Run("an HTTPS source", func(t *testing.T) {
		t.Parallel()
		spec := httpsFileSpec()
		spec.URL, spec.Path = "https://files.example.com/app/a$$b/$(HOME)", "schema$.hcl"
		job, err := builderFixture().Build(httpsSchemaFixture(spec), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatal(err)
		}
		read := processEnvironment(t, job)
		for name, want := range map[string]string{runner.EnvHTTPSURL: spec.URL, runner.EnvHTTPSPath: spec.Path} {
			if read[name] != want {
				t.Errorf("the process reads %s = %q, want %q", name, read[name], want)
			}
		}
	})
}
