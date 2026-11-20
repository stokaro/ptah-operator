package workload

import (
	"reflect"
	"slices"
	"testing"

	"golang.org/x/mod/semver"
	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A version-dependent argument is passed for a Ptah version that has it and
// for no other, by semantic version precedence, and a described commit counts
// as the tag it was built from or after.
func TestAPtahArgumentFollowsThePtahVersion(t *testing.T) {
	t.Parallel()

	skip := []string{"--skip-validation"}
	for _, row := range []struct {
		version string
		want    []string
	}{
		{version: "v0.33.1"},
		{version: "v0.33.9"},
		{version: "v0.3.0"},
		{version: "v0.34.0", want: skip},
		{version: "v0.34.1", want: skip},
		{version: "v0.40.0", want: skip},
		{version: "v1.0.0", want: skip},
		{version: "v0.34", want: skip},
		{version: "0.34.0", want: skip},
		{version: "0.33.1"},
		{version: "v0.34.0+build.7", want: skip},
		{version: "v0.34.0-3-gabcdef1", want: skip},
		{version: "v0.34.0-3-gabcdef1-dirty", want: skip},
		{version: "v0.34.0-dirty", want: skip},
		{version: "v0.33.1-12-g0123456789ab"},
		{version: "v0.33.1-dirty"},
		{version: "v0.34.0-rc.1"},
		{version: "v0.34.0-3-gnothex"},
		// Not a semantic version, as the runner harness names its build:
		// nothing orders it, so it has everything this build declares.
		{version: "harness", want: skip},
	} {
		t.Run(row.version, func(t *testing.T) {
			t.Parallel()
			if got := ptahArguments(row.version, migrationPullArguments); !reflect.DeepEqual(got, row.want) {
				t.Fatalf("ptahArguments(%q) = %q, want %q", row.version, got, row.want)
			}
		})
	}
}

// A since that is not a canonical release would order wrong without saying
// so: semver.Compare puts an invalid version below every valid one, so a
// since without its v would be passed to every Ptah, and a pre-release since
// to the release before it.
func TestEveryPtahArgumentNamesTheReleaseThatHasIt(t *testing.T) {
	t.Parallel()

	declared := map[string][]ptahArgument{"migrations pull": migrationPullArguments}
	examined := 0
	for command, arguments := range declared {
		for _, each := range arguments {
			examined++
			if !semver.IsValid(each.since) || semver.Canonical(each.since) != each.since || semver.Prerelease(each.since) != "" {
				t.Errorf("%s %s: since %q is not a canonical release vMAJOR.MINOR.PATCH", command, each.argument, each.since)
			}
			if each.argument == "" {
				t.Errorf("%s: an entry since %s names no argument", command, each.since)
			}
		}
	}
	if examined == 0 {
		t.Fatal("no version-dependent argument is declared, so nothing was examined")
	}
}

// The OCI fetch of a History and of an Apply passes --skip-validation for a
// Ptah that has it, whichever way the builder comes by the version: from its
// own configuration, or from the binding a claim recorded, which is how a
// manager that moved to v0.34.0 rebuilds the Job its predecessor dispatched
// for v0.33.1, and the other way round.
func TestTheMigrationPullFollowsThePtahVersionItIsBuiltFor(t *testing.T) {
	t.Parallel()

	pull := func(claim operatorv1alpha1.MigrationOperationStatus, extra ...string) []string {
		return append(append(slices.Clone(ociFetchPrefix),
			"migrations", "pull", claim.Source.ResolvedReference, "--out", migrationsPath), extra...)
	}
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory,
		operatorv1alpha1.MigrationOperationApply,
	} {
		for _, row := range []struct {
			name       string
			recorded   string
			configured string
			extra      []string
		}{
			{name: "v0.33.1 configured", recorded: "v0.33.1", configured: "v0.33.1"},
			{name: "v0.34.0 configured", recorded: "v0.34.0", configured: "v0.34.0", extra: []string{"--skip-validation"}},
			{name: "v0.33.1 recorded, v0.34.0 configured", recorded: "v0.33.1", configured: "v0.34.0"},
			{name: "v0.34.0 recorded, v0.33.1 configured", recorded: "v0.34.0", configured: "v0.33.1", extra: []string{"--skip-validation"}},
		} {
			t.Run(string(operation)+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				_, migration := migrationUnderPtah(row.recorded, 'b')
				claim := migrationOperationFixture(operation)
				var plan *operatorv1alpha1.PtahMigrationPlan
				if operation == operatorv1alpha1.MigrationOperationApply {
					plan = migrationPlanFixture()
				}
				var job *batchv1.Job
				var err error
				if row.recorded == row.configured {
					builder, _ := migrationUnderPtah(row.configured, 'b')
					job, err = builder.BuildMigration(migration, claim, plan)
				} else {
					builder, _ := migrationUnderPtah(row.configured, 'c')
					job, err = builder.BuildMigrationUnderBinding(migration.Status.ExecutionBinding, migration, claim, plan)
				}
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				fetch := requireContainer(t, job.Spec.Template.Spec.InitContainers, migrationFetchContainerName)
				if want := pull(claim, row.extra...); !reflect.DeepEqual(fetch.Args, want) {
					t.Fatalf("fetch args = %q, want %q", fetch.Args, want)
				}
				if got := job.Annotations[AnnotationPtahVersion]; got != row.recorded {
					t.Fatalf("the Job is built for Ptah %q, want the recorded %q", got, row.recorded)
				}
			})
		}
	}
}
