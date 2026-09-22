package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repositoryRoot = "../.."

// The one declared table in this tool is the family each phase proves, and it
// is held to the driver in both directions: a phase the driver runs with no
// entry, and an entry for a phase the driver stopped running.
func TestEveryPhaseTheDriverRunsDeclaresItsFamilies(t *testing.T) {
	t.Parallel()
	driver, err := readDriverPhases(repositoryRoot)
	if err != nil {
		t.Fatalf("read the driver phases: %v", err)
	}
	if len(driver) < len(phaseFamilies) {
		t.Fatalf("the driver runs %d phases and %d are declared", len(driver), len(phaseFamilies))
	}
	if err := verifyFamilyDeclarations(driver); err != nil {
		t.Fatalf("the declared families and the driver disagree: %v", err)
	}
}

// Reading the check again catches a reasoning error and misses the one that
// matters, so it is handed the two ways the table and the driver drift apart.
func TestVerifyFamilyDeclarationsRefusesWhatWouldPassVacuously(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		driver []driverPhase
		want   string
	}{
		{
			name:   "a phase the driver runs and nothing declares",
			driver: []driverPhase{{name: "reseed", script: "e2e-reseed.sh"}},
			want:   `phase "reseed" declares no resource family`,
		},
		{
			name:   "no phases at all, which would pass over nothing",
			driver: nil,
			want:   "is declared and the driver does not run it",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := verifyFamilyDeclarations(test.driver)
			if err == nil {
				t.Fatal("the check accepted a driver that disagrees with the declared table")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refusal = %v, want it to name %q", err, test.want)
			}
		})
	}
}

// Every supported minor runs every suite, and preparation is not coverage.
func TestTheCoverageTableNamesEveryRequiredCell(t *testing.T) {
	t.Parallel()
	table, err := buildCoverage(repositoryRoot, "edge")
	if err != nil {
		t.Fatalf("build the coverage table: %v", err)
	}
	suites, err := readSuites(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := len(table.minors) * len(suites)
	if len(table.cells) != want {
		t.Fatalf("the table names %d cells, want %d minors times %d suites",
			len(table.cells), len(table.minors), len(suites))
	}
	for _, cell := range table.cells {
		if len(cell.phases) == 0 {
			t.Fatalf("cell %s covers no phase", cell.ciJobName)
		}
		prepared := make(map[string]bool, len(cell.prepared))
		for _, name := range cell.prepared {
			prepared[name] = true
		}
		for _, phase := range cell.phases {
			if prepared[phase.name] {
				t.Fatalf("cell %s counts %q as coverage and as preparation",
					cell.ciJobName, phase.name)
			}
		}
	}
	// The Ptah build under test is part of the candidate's identity, so an
	// empty one would leave the table naming cells against nothing.
	if table.ptah.PtahRelease == "" || table.ptah.RunnerProtocolVersion == 0 {
		t.Fatalf("the table names no Ptah build: %#v", table.ptah)
	}
}

// One script carries both engines and the driver says which one a phase runs.
// Listing every mark inside it would report two engines from a job that
// exercised one.
func TestScenariosForEngineDropsTheOtherEngine(t *testing.T) {
	t.Parallel()
	known := map[string]bool{"postgresql": true, "mysql": true}
	marks := []string{"migration-policy", "postgresql-migrations", "mysql-transaction-mode", "mysql-migrations"}
	tests := []struct {
		name   string
		engine string
		want   []string
	}{
		{
			name:   "the PostgreSQL job",
			engine: "postgresql",
			want:   []string{"migration-policy", "postgresql-migrations"},
		},
		{
			name:   "the MySQL job",
			engine: "mysql",
			want:   []string{"migration-policy", "mysql-transaction-mode", "mysql-migrations"},
		},
		{
			name:   "a phase the driver hands no engine keeps every mark",
			engine: "",
			want:   marks,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := scenariosForEngine(marks, test.engine, known)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("scenarios = %v, want %v", got, test.want)
			}
		})
	}
}

// The engine belongs to the phase the driver hands it to, and to no other.
// A reader that let one travel to the next phase would report an engine-bound
// run for a phase that never received one.
func TestReadDriverPhasesBindsAnEngineToOnePhase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source []string
	}{
		{
			// The shape the driver writes today: a blank line between the
			// environment blocks.
			name: "phases separated by a blank line",
			source: []string{
				`E2E_KUBECONFIG=$KUBECONFIG_FILE \`,
				`E2E_ENGINE=postgresql \`,
				"\trun_recorded_phase migrations-postgresql \"$ROOT_DIR/hack/e2e-migrations.sh\"",
				``,
				`E2E_KUBECONFIG=$KUBECONFIG_FILE \`,
				"\trun_recorded_phase uninstall \"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"",
				``,
			},
		},
		{
			// And the shape that makes the reset load-bearing: nothing between
			// the two calls to clear the engine on the way past.
			name: "phases with nothing between them",
			source: []string{
				`E2E_ENGINE=postgresql \`,
				"\trun_recorded_phase migrations-postgresql \"$ROOT_DIR/hack/e2e-migrations.sh\"",
				"\trun_recorded_phase uninstall \"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			phases := parseDriverPhases(strings.Join(test.source, "\n"))
			if len(phases) != 2 {
				t.Fatalf("read %d phases, want 2: %#v", len(phases), phases)
			}
			if phases[0].name != "migrations-postgresql" || phases[0].engine != "postgresql" ||
				phases[0].script != "e2e-migrations.sh" {
				t.Fatalf("first phase = %#v", phases[0])
			}
			if phases[1].engine != "" {
				t.Fatalf("the engine travelled to %q, which the driver hands none", phases[1].name)
			}
		})
	}
}

// The rendered table is what a reader pastes into the acceptance record, so
// the header and the statement about preparation have to survive.
func TestTheRenderedTableSaysWhatPreparationIsWorth(t *testing.T) {
	t.Parallel()
	table, err := buildCoverage(repositoryRoot, "edge")
	if err != nil {
		t.Fatal(err)
	}
	rendered := table.markdown()
	for _, want := range []string{
		"### PA-01 coverage",
		"| Kubernetes minor | Suite | Phase | Engine | Families | Scenarios | Prepared by | CI job (evidence) |",
		"they are not coverage",
		"`Kubernetes " + table.minors[0] + " lifecycle`",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the rendered table does not carry %q", want)
		}
	}
}

// The evidence column names a CI job, so it has to be spelled the way the
// workflow spells it.
//
// A renamed job leaves every cell pointing at a job nobody can find, which is
// the one column of the table a reader cannot check for themselves. Reading
// the workflow's own name template is what keeps the two together.
func TestTheEvidenceColumnNamesTheJobTheWorkflowRuns(t *testing.T) {
	t.Parallel()
	workflow := readRepositoryFile(t, filepath.Join(".github", "workflows", "ci.yml"))
	const template = "name: Kubernetes ${{ matrix.minor }} ${{ matrix.suite }}"
	if !strings.Contains(workflow, template) {
		t.Fatalf(".github/workflows/ci.yml does not name its matrix job %q,"+
			" so the evidence column points at a job that does not exist", template)
	}
	table, err := buildCoverage(repositoryRoot, "edge")
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range table.cells {
		want := "Kubernetes " + cell.minor + " " + cell.suite.Name
		if cell.ciJobName != want {
			t.Fatalf("cell evidence is %q, want %q", cell.ciJobName, want)
		}
	}
}

func readRepositoryFile(t *testing.T, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot, relative)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(content)
}
