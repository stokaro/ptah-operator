// Command record runs the demonstration scenarios against a live lab and
// writes what happened.
//
// It is the middle of three parts. `demo/scenarios` declares what a scenario
// is, this program executes it and checks the cluster reached the state the
// scenario claims, and the site plays back what this program wrote. Neither
// end holds a second copy: a transcript on the page is this program's output,
// and a green check on the page is an expectation that held here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "record: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("record", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	var (
		root        = flags.String("root", ".", "the repository root")
		environment = flags.String("environment", "demo/.lab/environment", "the lab environment written by the bootstrap")
		scenarios   = flags.String("scenarios", "demo/scenarios", "the directory holding scenario definitions")
		output      = flags.String("output", "demo/recordings/runs.json", "where to write the recording")
		only        = flags.String("only", "", "record one scenario by id, for iterating on it")
		checkOnly   = flags.Bool("check", false, "load and validate the scenarios, run nothing")
		stepTimeout = flags.Duration("step-timeout", 5*time.Minute, "how long one command may take")
		quiet       = flags.Bool("quiet", false, "say nothing but failures")
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	loaded, err := loadScenarios(*scenarios)
	if err != nil {
		return err
	}
	if *only != "" {
		loaded = filterScenarios(loaded, *only)
		if len(loaded) == 0 {
			return fmt.Errorf("no scenario has id %q", *only)
		}
	}
	if *checkOnly {
		fmt.Fprintf(diagnostics, "record: %d scenarios, %d steps, all valid\n", len(loaded), countSteps(loaded))
		return nil
	}

	outputFile := *output
	if !filepath.IsAbs(outputFile) {
		outputFile = filepath.Join(*root, outputFile)
	}
	// Before the lab, because a tree the recording could not name is a reason
	// to stop that costs nothing to find out.
	source, err := sourceIdentity(*root, outputFile)
	if err != nil {
		return err
	}

	environmentFile := *environment
	if !filepath.IsAbs(environmentFile) {
		environmentFile = filepath.Join(*root, environmentFile)
	}
	live, err := loadLab(environmentFile)
	if err != nil {
		return err
	}
	variables, err := live.environment(*root)
	if err != nil {
		return err
	}

	log := func(format string, arguments ...any) {
		fmt.Fprintf(diagnostics, "record: "+format+"\n", arguments...)
	}
	if *quiet {
		log = func(string, ...any) {}
	}

	// A recorder interrupted halfway leaves a cluster mid-scenario, which the
	// next run's reset returns to a known state. What must not survive is a
	// half-written recording, so the file is written once, at the end.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rewriter, err := newNormalizer(live)
	if err != nil {
		return err
	}
	recorder := &recorder{
		lab:          live,
		normalizer:   rewriter,
		root:         *root,
		environment:  variables,
		stepTimeout:  *stepTimeout,
		pollInterval: 2 * time.Second,
		log:          log,
	}

	recorder.source = source
	record := runRecord{Lab: live.describe()}
	// Re-recording one scenario keeps the rest, so iterating on a scenario
	// does not cost a full pass over nine of them. The lab has to be the one
	// the file was written against: a recording that mixed two labs would
	// carry one set of versions and two sets of transcripts.
	var existing *runRecord
	if *only != "" {
		existing, err = readRecord(outputFile)
		if err != nil {
			return err
		}
		if existing != nil && !sameLab(existing.Lab, record.Lab) {
			return fmt.Errorf(
				"%s was recorded against another lab, so one scenario cannot be replaced in it; "+
					"record the whole set", outputFile)
		}
	}
	started := time.Now()
	for _, one := range loaded {
		recorded, err := recorder.run(ctx, one)
		record.Scenarios = append(record.Scenarios, recorded)
		if err != nil {
			writeRecordDiagnostics(diagnostics, record)
			return err
		}
		log("scenario %s: %d checks passed", one.ID, len(recorded.Checks))
	}
	record.RecordedAt = started.UTC().Format(time.RFC3339)
	if existing != nil {
		record.Scenarios = merge(existing.Scenarios, record.Scenarios)
	}
	record.Source = commonSource(record.Scenarios)

	if err := writeRecord(outputFile, record); err != nil {
		return err
	}
	log("wrote %s: %d scenarios, %d checks", outputFile, len(record.Scenarios), record.checks())
	return nil
}

// runRecord is everything one recording session produced.
//
// What it carries beyond the transcripts is the answer to "of what?": the
// Kubernetes version, the operator image and revision, the executor image and
// the Ptah build inside it. A transcript without that pairing is a screenshot.
type runRecord struct {
	RecordedAt string            `json:"recordedAt"`
	Lab        map[string]string `json:"lab"`
	// Source is the commit every scenario in the record was made at, and is
	// absent when they were not all made at one. A record that named a commit
	// eight of its nine transcripts predate would be the wrong kind of true.
	Source    map[string]string `json:"source,omitempty"`
	Scenarios []recording       `json:"scenarios"`
	Order     []string          `json:"order"`
}

func (r runRecord) checks() int {
	total := 0
	for _, one := range r.Scenarios {
		total += len(one.Checks)
	}
	return total
}

// sourceIdentity records which commit of this repository the scenarios and the
// recorder came from.
//
// It refuses a tree that does not match that commit. A recording names a commit
// so that somebody can check the commit out and produce the same transcripts;
// recorded from edited files, the name is of a tree that never existed and the
// claim cannot be checked. The output file is the exception, because writing it
// is what the recorder is doing.
func sourceIdentity(root, output string) (map[string]string, error) {
	if err := refuseDirtyTree(root, output); err != nil {
		return nil, err
	}
	identity := map[string]string{}
	for name, arguments := range map[string]string{
		"commit":   "git -C %s rev-parse HEAD",
		"describe": "git -C %s describe --tags --always",
	} {
		value, err := gitOutput(fmt.Sprintf(arguments, root))
		if err != nil {
			return nil, err
		}
		identity[name] = value
	}
	return identity, nil
}

// refuseDirtyTree reports every tracked change that is not the recording itself.
func refuseDirtyTree(root, output string) error {
	status, err := gitLines(fmt.Sprintf("git -C %s status --porcelain --untracked-files=no", root))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, output)
	if err != nil {
		relative = output
	}
	relative = filepath.ToSlash(relative)

	var changed []string
	for _, line := range status {
		// Porcelain v1: two status characters, a space, then the path.
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == relative {
			continue
		}
		changed = append(changed, path)
	}
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the tree has changes the recording would not name:\n  %s\n\n"+
			"A recording names a commit so it can be reproduced from it. Commit these first",
		strings.Join(changed, "\n  "))
}

// commonSource returns the commit every scenario was recorded at, or nothing
// when they disagree.
func commonSource(recorded []recording) map[string]string {
	if len(recorded) == 0 {
		return nil
	}
	first := recorded[0].Source
	for _, one := range recorded[1:] {
		if !maps.Equal(one.Source, first) {
			return nil
		}
	}
	return first
}

func loadScenarios(directory string) ([]scenario, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the scenario directory: %w", err)
	}
	var loaded []scenario
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var one scenario
		decoder := yaml.NewDecoder(strings.NewReader(string(content)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&one); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if err := one.validate(); err != nil {
			return nil, fmt.Errorf("%s:\n%w", entry.Name(), err)
		}
		if want := one.ID + ".yaml"; entry.Name() != want {
			return nil, fmt.Errorf(
				"%s declares id %q, so the file has to be named %s; the id is the scenario's "+
					"address in a URL and the file name is how a reader finds it", entry.Name(), one.ID, want)
		}
		loaded = append(loaded, one)
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("%s holds no scenario", directory)
	}
	return loaded, nil
}

func filterScenarios(loaded []scenario, id string) []scenario {
	var kept []scenario
	for _, one := range loaded {
		if one.ID == id {
			kept = append(kept, one)
		}
	}
	return kept
}

func countSteps(loaded []scenario) int {
	total := 0
	for _, one := range loaded {
		total += len(one.Steps)
	}
	return total
}

func writeRecord(path string, record runRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// writeRecordDiagnostics prints the checks of a failed run.
//
// A run that stopped is the one worth reading: it says which claim stopped
// holding, and the scenario file is where that claim is written down.
func writeRecordDiagnostics(diagnostics io.Writer, record runRecord) {
	for _, one := range record.Scenarios {
		for _, result := range one.Checks {
			status := "ok  "
			if !result.Passed {
				status = "FAIL"
			}
			fmt.Fprintf(diagnostics, "  %s %s step %d: %s\n", status, result.Scenario, result.Step, result.Claim)
			if result.Detail != "" {
				fmt.Fprintf(diagnostics, "       %s\n", strings.ReplaceAll(result.Detail, "\n", "\n       "))
			}
		}
	}
}

// readRecord reads a recording that is already there, and reports its absence
// as nothing rather than as a failure.
func readRecord(path string) (*runRecord, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record runRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &record, nil
}

// sameLab reports whether two recordings were made against the same lab.
func sameLab(first, second map[string]string) bool {
	return maps.Equal(first, second)
}

// merge replaces the scenarios that were re-recorded and keeps the rest, in the
// order the file already had.
func merge(existing, recorded []recording) []recording {
	replaced := map[string]recording{}
	for _, one := range recorded {
		replaced[one.ID] = one
	}
	merged := make([]recording, 0, len(existing)+len(recorded))
	for _, one := range existing {
		if fresh, ok := replaced[one.ID]; ok {
			merged = append(merged, fresh)
			delete(replaced, one.ID)
			continue
		}
		merged = append(merged, one)
	}
	for _, one := range recorded {
		if _, pending := replaced[one.ID]; pending {
			merged = append(merged, one)
			delete(replaced, one.ID)
		}
	}
	return merged
}
