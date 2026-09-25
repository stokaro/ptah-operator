package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// phaseFamilies says which resource family each lifecycle phase proves.
//
// It is the one thing here that is a judgement rather than a reading, so it is
// written once and held to the driver: TestEveryPhaseTheDriverRunsDeclaresItsFamilies
// fails on a phase with no entry and on an entry for a phase the driver
// stopped running. An empty list is a real answer -- the release lifecycle
// phases prove installation, failover, certificate handoff and uninstall,
// which belong to no resource family -- and it is distinguished from a missing
// one by the entry existing at all.
var phaseFamilies = map[string][]string{
	"upgrade":                   {},
	"ha":                        {},
	"uninstall":                 {},
	"cert-rotation":             {},
	"assert":                    {"PtahSchema"},
	"dataplane":                 {"PtahSchema"},
	"migrations-postgresql":     {"PtahMigration"},
	"migrations-mysql":          {"PtahMigration"},
	"reference-data-postgresql": {"PtahSchema"},
	"reference-data-mysql":      {"PtahSchema"},
	// The alerts it drives to a receiver are about both families: an Apply
	// nobody accounted for is a migration's, the stalled operation a schema's.
	"alerting": {"PtahMigration", "PtahSchema"},
}

func main() {
	root := flag.String("root", ".", "repository root to read the catalogs and the driver from")
	operator := flag.String("operator", "edge", "the Ptah catalog row naming the candidate")
	check := flag.Bool("check", false, "verify the coverage is complete and print nothing")
	record := flag.Bool("record", false, "print the whole acceptance record rather than the coverage table alone")
	profilePath := flag.String("profile", "", "a declared acceptance profile to fill the record from")
	flag.Parse()

	var declared *profile
	if *profilePath != "" {
		read, err := readProfile(*profilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "acceptance coverage: %v\n", err)
			os.Exit(1)
		}
		declared = read
	}

	table, err := buildCoverage(*root, *operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acceptance coverage: %v\n", err)
		os.Exit(1)
	}
	if *check {
		return
	}
	if *record {
		fmt.Print(table.recordMarkdown(*root, declared))
		return
	}
	fmt.Print(table.markdown())
}

// coverageCell is one required cell of the PA-01 table: one suite on one
// supported Kubernetes minor.
type coverageCell struct {
	minor     string
	suite     suite
	phases    []coveredPhase
	prepared  []string
	ciJobName string
}

// coveredPhase is one phase a suite runs for coverage, with what it proves.
type coveredPhase struct {
	name      string
	script    string
	engine    string
	families  []string
	scenarios []string
}

type coverage struct {
	minors []string
	cells  []coverageCell
	ptah   ptahVerification
}

func buildCoverage(root, operator string) (*coverage, error) {
	minors, err := readMinors(root)
	if err != nil {
		return nil, err
	}
	suites, err := readSuites(root)
	if err != nil {
		return nil, err
	}
	driver, err := readDriverPhases(root)
	if err != nil {
		return nil, err
	}
	ptah, err := readPtahIdentity(root, operator)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]driverPhase, len(driver))
	engines := map[string]bool{}
	for _, phase := range driver {
		byName[phase.name] = phase
		if phase.engine != "" {
			engines[phase.engine] = true
		}
	}
	if err := verifyFamilyDeclarations(driver); err != nil {
		return nil, err
	}

	result := &coverage{minors: minors, ptah: ptah}
	for _, minor := range minors {
		for _, item := range suites {
			if len(item.Phases) == 0 {
				return nil, fmt.Errorf("suite %q covers no phase", item.Name)
			}
			cell := coverageCell{
				minor:     minor,
				suite:     item,
				prepared:  item.Prepare,
				ciJobName: fmt.Sprintf("Kubernetes %s %s", minor, item.Name),
			}
			for _, name := range item.Phases {
				phase, found := byName[name]
				if !found {
					return nil, fmt.Errorf("suite %q lists phase %q, which the driver does not run", item.Name, name)
				}
				scenarios, scenarioErr := readScenarios(root, phase.script)
				if scenarioErr != nil {
					return nil, scenarioErr
				}
				cell.phases = append(cell.phases, coveredPhase{
					name:      phase.name,
					script:    phase.script,
					engine:    phase.engine,
					families:  phaseFamilies[phase.name],
					scenarios: scenariosForEngine(scenarios, phase.engine, engines),
				})
			}
			result.cells = append(result.cells, cell)
		}
	}
	return result, nil
}

// verifyFamilyDeclarations holds the one declared table to the driver.
func verifyFamilyDeclarations(driver []driverPhase) error {
	running := make(map[string]bool, len(driver))
	var problems []string
	for _, phase := range driver {
		running[phase.name] = true
		if _, declared := phaseFamilies[phase.name]; !declared {
			problems = append(problems, fmt.Sprintf("phase %q declares no resource family", phase.name))
		}
	}
	for name := range phaseFamilies {
		if !running[name] {
			problems = append(problems, fmt.Sprintf("phase %q is declared and the driver does not run it", name))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

func (c *coverage) markdown() string {
	var out strings.Builder
	out.WriteString("### PA-01 coverage\n\n")
	fmt.Fprintf(&out, "Derived from `support/kubernetes.json`, `support/e2e-suites.json`, "+
		"`support/ptah.json` and `hack/e2e-kind.sh`. %d supported minors and %d suites make "+
		"%d required cells. Preparation phases are listed because they explain what a suite "+
		"stands up, and they are not coverage: a phase counts where a suite lists it under "+
		"`phases` and nowhere else.\n\n",
		len(c.minors), len(c.cells)/len(c.minors), len(c.cells))
	fmt.Fprintf(&out, "Ptah build under test: %s (`%s`), runner protocol %d.\n\n",
		c.ptah.PtahRelease, c.ptah.PtahCommit, c.ptah.RunnerProtocolVersion)
	out.WriteString("| Kubernetes minor | Suite | Phase | Engine | Families | Scenarios | Prepared by | CI job (evidence) |\n")
	out.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, cell := range c.cells {
		for index, phase := range cell.phases {
			minor, suiteName, prepared, job := "", "", "", ""
			if index == 0 {
				minor = cell.minor
				suiteName = cell.suite.Name
				prepared = joinOrDash(cell.prepared)
				job = "`" + cell.ciJobName + "`"
			}
			fmt.Fprintf(&out, "| %s | %s | `%s` | %s | %s | %s | %s | %s |\n",
				minor, suiteName, phase.name,
				valueOrDash(phase.engine),
				joinOrDash(phase.families),
				scenarioList(phase.scenarios),
				prepared, job)
		}
	}
	out.WriteString("\nA cell passes when its CI job succeeded on the candidate commit. " +
		"Record the run and attempt: a job name alone does not say which run it was, and a " +
		"canceled or skipped required job is no pass.\n")
	return out.String()
}

// scenariosForEngine drops the scenarios a phase does not run.
//
// One script carries both engines and the driver names which one a phase runs,
// so the marks inside it are a superset of what any single phase executed.
// Listing all of them would claim MySQL coverage from the PostgreSQL job and
// PostgreSQL coverage from the MySQL one -- two engines reported and one
// exercised, which is the kind of claim a coverage table exists to prevent.
//
// A scenario that names no engine belongs to whichever phase ran it.
func scenariosForEngine(scenarios []string, engine string, known map[string]bool) []string {
	if engine == "" {
		return scenarios
	}
	kept := make([]string, 0, len(scenarios))
	for _, scenario := range scenarios {
		foreign := false
		for candidate := range known {
			if candidate != engine && strings.Contains(scenario, candidate) {
				foreign = true
				break
			}
		}
		if !foreign {
			kept = append(kept, scenario)
		}
	}
	return kept
}

func scenarioList(scenarios []string) string {
	if len(scenarios) == 0 {
		return "the phase is its own scenario"
	}
	quoted := make([]string, 0, len(scenarios))
	for _, scenario := range scenarios {
		quoted = append(quoted, "`"+scenario+"`")
	}
	return strings.Join(quoted, ", ")
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "—"
	}
	return strings.Join(values, ", ")
}

func valueOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
