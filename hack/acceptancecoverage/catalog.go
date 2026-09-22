// Package main derives the acceptance coverage table from the catalogs and
// the driver, rather than from a list written beside them.
//
// PA-01 of the release acceptance protocol asks for a table naming every
// required cell, the scenarios it executed, and the evidence that it passed.
// Every part of that except the evidence is already written down somewhere in
// this repository: the supported Kubernetes minors in support/kubernetes.json,
// the suites and their phases in support/e2e-suites.json, the script each
// phase runs and the engine it is handed in hack/e2e-kind.sh, and the
// scenarios inside the longest phases in their own stopwatch marks.
//
// A table typed out by hand from those four sources is a fifth copy, and the
// first one to go stale. This reads them.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// suite is one acceptance suite as the catalog declares it.
type suite struct {
	Name    string   `json:"name"`
	Slug    string   `json:"slug"`
	Summary string   `json:"summary"`
	Phases  []string `json:"phases"`
	Prepare []string `json:"prepare"`
}

type suiteCatalog struct {
	Suites []suite `json:"suites"`
}

// kubernetesRelease is one supported minor.
type kubernetesRelease struct {
	Minor     string `json:"minor"`
	NodeImage string `json:"nodeImage"`
}

// The window size is deliberately not read. hack/verify-kubernetes-support.go
// owns that policy and refuses a catalog whose releases disagree with it, and
// make verify-source runs it before anything reads this table.
type kubernetesCatalog struct {
	Releases []kubernetesRelease `json:"releases"`
}

// ptahVerification is one Ptah build a row of the Ptah catalog claims.
type ptahVerification struct {
	PtahRelease           string `json:"ptahRelease"`
	PtahCommit            string `json:"ptahCommit"`
	RunnerProtocolVersion int    `json:"runnerProtocolVersion"`
}

type ptahRelease struct {
	Operator string             `json:"operator"`
	Verified []ptahVerification `json:"verified"`
}

type ptahCatalog struct {
	Releases []ptahRelease `json:"releases"`
}

// driverPhase is one phase the lifecycle driver runs: the script that carries
// it and the engine the driver hands it, if any.
type driverPhase struct {
	name   string
	script string
	engine string
}

var (
	recordedPhase = regexp.MustCompile(`^\s*run_recorded_phase (\S+) "\$ROOT_DIR/hack/(e2e-[a-z-]+\.sh)"`)
	phaseEngine   = regexp.MustCompile(`^E2E_ENGINE=([a-z]+) \\$`)
	nestedPhase   = regexp.MustCompile(`"\$ROOT_DIR/hack/(e2e-[a-z-]+\.sh)"`)
	scenarioMark  = regexp.MustCompile(`(?m)^\s*timing_next scenario (\S+)`)
)

// readSuites reads the suite catalog.
func readSuites(root string) ([]suite, error) {
	var catalog suiteCatalog
	if err := readJSON(filepath.Join(root, "support", "e2e-suites.json"), &catalog); err != nil {
		return nil, err
	}
	if len(catalog.Suites) == 0 {
		return nil, fmt.Errorf("the suite catalog declares no suite")
	}
	return catalog.Suites, nil
}

// readMinors reads the supported Kubernetes minors.
func readMinors(root string) ([]string, error) {
	var catalog kubernetesCatalog
	if err := readJSON(filepath.Join(root, "support", "kubernetes.json"), &catalog); err != nil {
		return nil, err
	}
	if len(catalog.Releases) == 0 {
		return nil, fmt.Errorf("the Kubernetes catalog declares no supported minor")
	}
	minors := make([]string, 0, len(catalog.Releases))
	for _, release := range catalog.Releases {
		if release.Minor == "" {
			return nil, fmt.Errorf("the Kubernetes catalog lists a release with no minor")
		}
		minors = append(minors, release.Minor)
	}
	return minors, nil
}

// readPtahIdentity reads the Ptah build the candidate's own row claims. The
// row is the one named "edge" while a release is being prepared from master;
// a candidate cut from a tag names itself.
//
// A row records what each lifecycle verified, appended, so the last entry is
// the build the matrix ran most recently. A row that carries several is a row
// whose older entries are history.
func readPtahIdentity(root, operator string) (ptahVerification, error) {
	var catalog ptahCatalog
	if err := readJSON(filepath.Join(root, "support", "ptah.json"), &catalog); err != nil {
		return ptahVerification{}, err
	}
	for _, release := range catalog.Releases {
		if release.Operator != operator {
			continue
		}
		if len(release.Verified) == 0 {
			return ptahVerification{}, fmt.Errorf("the Ptah catalog verifies no build for operator %q", operator)
		}
		return release.Verified[len(release.Verified)-1], nil
	}
	return ptahVerification{}, fmt.Errorf("the Ptah catalog has no row for operator %q", operator)
}

// readDriverPhases reads which script carries each phase and which engine the
// driver hands it. The driver is the authority on both: a phase asked to run
// with no engine refuses rather than covering one engine and reporting two.
func readDriverPhases(root string) ([]driverPhase, error) {
	source, err := os.ReadFile(filepath.Join(root, "hack", "e2e-kind.sh")) //nolint:gosec // A path built from the repository root.
	if err != nil {
		return nil, err
	}
	phases := parseDriverPhases(string(source))
	if len(phases) == 0 {
		return nil, fmt.Errorf("the driver runs no recorded phase")
	}
	return phases, nil
}

// parseDriverPhases reads the driver's own record of what it runs: the phase
// name, the script that carries it, and the engine the environment block in
// front of the call hands it.
func parseDriverPhases(source string) []driverPhase {
	var phases []driverPhase
	engine := ""
	for _, line := range strings.Split(source, "\n") {
		if match := phaseEngine.FindStringSubmatch(line); match != nil {
			engine = match[1]
			continue
		}
		match := recordedPhase.FindStringSubmatch(line)
		if match == nil {
			// Any line that is not part of an environment block ends it, so an
			// engine named for one phase cannot travel to the next.
			if !strings.HasSuffix(strings.TrimRight(line, " \t"), `\`) {
				engine = ""
			}
			continue
		}
		phases = append(phases, driverPhase{name: match[1], script: match[2], engine: engine})
		engine = ""
	}
	return phases
}

// readScenarios reads the stopwatch marks a phase script names, following the
// one level of nesting the driver's longest phase uses: fault injection runs
// inside the data plane and its scenarios belong to that phase.
func readScenarios(root, script string) ([]string, error) {
	source, err := os.ReadFile(filepath.Join(root, "hack", script)) //nolint:gosec // A path built from the repository root.
	if err != nil {
		return nil, err
	}
	scenarios := markedScenarios(string(source))
	for _, nested := range nestedScripts(string(source), script) {
		nestedSource, nestedErr := os.ReadFile(filepath.Join(root, "hack", nested)) //nolint:gosec // A path built from the repository root.
		if nestedErr != nil {
			return nil, nestedErr
		}
		scenarios = append(scenarios, markedScenarios(string(nestedSource))...)
	}
	return scenarios, nil
}

func markedScenarios(source string) []string {
	var scenarios []string
	for _, match := range scenarioMark.FindAllStringSubmatch(source, -1) {
		scenarios = append(scenarios, match[1])
	}
	return scenarios
}

// nestedScripts names the phase scripts a phase script runs inside itself.
// The stopwatch helper and the SQL wrappers are libraries rather than phases,
// and a script never nests itself.
func nestedScripts(source, self string) []string {
	library := map[string]bool{
		"e2e-timing.sh":                   true,
		"e2e-sql.sh":                      true,
		"e2e-kubernetes-support-image.sh": true,
		self:                              true,
	}
	seen := make(map[string]bool)
	var nested []string
	for _, match := range nestedPhase.FindAllStringSubmatch(source, -1) {
		name := match[1]
		if library[name] || seen[name] {
			continue
		}
		seen[name] = true
		nested = append(nested, name)
	}
	sort.Strings(nested)
	return nested
}

func readJSON(path string, into any) error {
	content, err := os.ReadFile(path) //nolint:gosec // A path built from the repository root.
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, into); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
