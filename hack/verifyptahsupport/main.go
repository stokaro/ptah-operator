// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command verify-ptah-support validates the Ptah compatibility catalogue and
// exports the one value the pipeline reads from it.
//
// The catalogue is the canonical answer to "which Ptah build does this operator
// version work with", and it separates three states a table usually blurs: what
// is DECLARED, what was VERIFIED, and what nobody has data for. An unverified
// combination is not an incompatible one, and a single verified build is not a
// supported range.
//
// The program performs no network requests. It reads support/ptah.json, the
// lifecycle script and the workflow, and refuses a catalogue that is malformed,
// self-contradictory, or no longer the only place the tested Ptah commit is
// written down.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	catalogPath   = "support/ptah.json"
	lifecyclePath = "hack/e2e-kind.sh"
	workflowPath  = ".github/workflows/ci.yml"
	// The audited workflow contract describes the lifecycle job's environment
	// literally, so it held a copy of the commit too -- one nothing compared
	// against the catalogue. It names the support job's output now, and this
	// path keeps it that way.
	workflowContractPath = "hack/verify-kubernetes-support.go"
	// schemaVersion is the shape this program understands. A catalogue written
	// for a later shape is refused rather than read with the fields this
	// program happens to recognize.
	schemaVersion = 1
	// ptahVersionLimit mirrors the chart's own limit on execution.ptahVersion,
	// so a catalogue cannot record an identity the chart would refuse to bind.
	ptahVersionLimit = 128
	edgeVersion      = "edge"
)

var (
	commitPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	releasePattern  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	datePattern     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	describePattern = regexp.MustCompile(`-g([0-9a-f]{7,40})$`)
	// looseCommit finds a bare commit anywhere in a file. The cross-file checks
	// use it to refuse a second copy of the pin rather than to read one.
	looseCommit = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
)

// catalog is support/ptah.json.
type catalog struct {
	SchemaVersion int                 `json:"schemaVersion"`
	LastVerified  string              `json:"lastVerified"`
	Axis          string              `json:"axis"`
	Releases      []release           `json:"releases"`
	Evidence      map[string]evidence `json:"evidence"`
}

// release is one operator version and everything claimed about it.
type release struct {
	Operator         string        `json:"operator"`
	Stage            string        `json:"stage"`
	Documentation    documentation `json:"documentation"`
	Declared         declared      `json:"declared"`
	Verified         []verified    `json:"verified"`
	UnverifiedReason string        `json:"unverifiedReason,omitempty"`
	Limitations      []string      `json:"limitations,omitempty"`
}

// documentation says whether this operator version has a published guide, which
// is what stops a compatibility table rendering a link to a page nobody built.
type documentation struct {
	Published bool   `json:"published"`
	Source    string `json:"source,omitempty"`
}

// declared is the support claim, which is a promise rather than a measurement.
type declared struct {
	Range     *string `json:"range"`
	Statement string  `json:"statement"`
}

// verified is one measurement: a Ptah build this operator version was actually
// run against, and what ran.
type verified struct {
	PtahRelease  *string `json:"ptahRelease"`
	PtahCommit   string  `json:"ptahCommit"`
	PtahDescribe string  `json:"ptahDescribe"`
	Evidence     string  `json:"evidence"`
	Scope        string  `json:"scope"`
}

// evidence is what stands behind a verified row.
type evidence struct {
	What string `json:"what"`
	Pin  string `json:"pin"`
}

func main() {
	var output string
	var now string
	flag.StringVar(&output, "output", "", "what to print: empty to verify only, or `commit` for the verified edge Ptah commit")
	flag.StringVar(&now, "now", "", "validation date as YYYY-MM-DD; defaults to today in UTC")
	flag.Parse()

	if err := run(output, now); err != nil {
		fmt.Fprintf(os.Stderr, "verify-ptah-support: %v\n", err)
		os.Exit(1)
	}
}

func run(output, now string) error {
	loaded, err := load(catalogPath)
	if err != nil {
		return err
	}
	today, err := validationDate(now)
	if err != nil {
		return err
	}
	if err := validate(loaded, today); err != nil {
		return err
	}
	if err := checkSinglePin(loaded, []string{lifecyclePath, workflowPath, workflowContractPath}); err != nil {
		return err
	}

	switch output {
	case "":
		return nil
	case "commit":
		commit, err := edgeCommit(loaded)
		if err != nil {
			return err
		}
		fmt.Println(commit)
		return nil
	default:
		return fmt.Errorf("unknown -output %q", output)
	}
}

func load(path string) (catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return catalog{}, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var loaded catalog
	if err := decoder.Decode(&loaded); err != nil {
		return catalog{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return loaded, nil
}

func validationDate(now string) (time.Time, error) {
	if now == "" {
		return time.Now().UTC().Truncate(24 * time.Hour), nil
	}
	parsed, err := time.Parse(time.DateOnly, now)
	if err != nil {
		return time.Time{}, fmt.Errorf("-now must be YYYY-MM-DD: %w", err)
	}
	return parsed, nil
}

// validate holds the catalogue to its own shape.
func validate(loaded catalog, today time.Time) error {
	var problems []error

	if loaded.SchemaVersion != schemaVersion {
		return fmt.Errorf("%s declares schema version %d; this program reads %d",
			catalogPath, loaded.SchemaVersion, schemaVersion)
	}
	if strings.TrimSpace(loaded.Axis) == "" {
		problems = append(problems, errors.New("axis is empty; a table with no stated axis invites the wrong reading"))
	}
	problems = append(problems, validateDate(loaded.LastVerified, today)...)
	problems = append(problems, validateReleases(loaded)...)
	problems = append(problems, validateEvidenceIsUsed(loaded)...)

	return errors.Join(problems...)
}

func validateDate(lastVerified string, today time.Time) []error {
	if !datePattern.MatchString(lastVerified) {
		return []error{fmt.Errorf("lastVerified %q is not a YYYY-MM-DD date", lastVerified)}
	}
	parsed, err := time.Parse(time.DateOnly, lastVerified)
	if err != nil {
		return []error{fmt.Errorf("lastVerified %q is not a date: %w", lastVerified, err)}
	}
	if parsed.After(today) {
		return []error{fmt.Errorf("lastVerified %s is in the future", lastVerified)}
	}
	return nil
}

func validateReleases(loaded catalog) []error {
	var problems []error
	if len(loaded.Releases) == 0 {
		return []error{errors.New("the catalogue lists no operator version; an empty table claims nothing and reads as complete")}
	}

	seen := make(map[string]bool, len(loaded.Releases))
	edges := 0
	for _, entry := range loaded.Releases {
		if seen[entry.Operator] {
			problems = append(problems, fmt.Errorf("operator version %q appears twice; two rows for one version contradict each other by construction", entry.Operator))
		}
		seen[entry.Operator] = true
		if entry.Operator == edgeVersion {
			edges++
		}
		problems = append(problems, validateRelease(entry, loaded.Evidence)...)
	}
	if edges != 1 {
		problems = append(problems, fmt.Errorf("the catalogue names %s %d times; the development state is one row and is always present", edgeVersion, edges))
	}
	return problems
}

func validateRelease(entry release, evidenceByName map[string]evidence) []error {
	var problems []error
	name := entry.Operator

	switch {
	case name == edgeVersion:
		if entry.Stage != "development" {
			problems = append(problems, fmt.Errorf("%s has stage %q; edge is the development state", name, entry.Stage))
		}
	case releasePattern.MatchString(name):
		if entry.Stage != "released" {
			problems = append(problems, fmt.Errorf("%s has stage %q; a tagged version is released", name, entry.Stage))
		}
	default:
		problems = append(problems, fmt.Errorf("operator version %q is neither %s nor vMAJOR.MINOR.PATCH", name, edgeVersion))
	}

	problems = append(problems, validateDocumentation(name, entry.Documentation)...)
	problems = append(problems, validateDeclared(name, entry.Declared)...)
	problems = append(problems, validateVerified(name, entry, evidenceByName)...)

	for index, limitation := range entry.Limitations {
		if strings.TrimSpace(limitation) == "" {
			problems = append(problems, fmt.Errorf("%s limitation %d is empty", name, index))
		}
	}
	return problems
}

// validateDocumentation keeps the published flag and its source together. A
// version whose guide was never built must say so, because the compatibility
// table reads this field to decide whether it may render a link at all.
func validateDocumentation(name string, doc documentation) []error {
	source := strings.TrimSpace(doc.Source)
	if doc.Published && source == "" {
		return []error{fmt.Errorf("%s publishes documentation and names no source revision", name)}
	}
	if !doc.Published && source != "" {
		return []error{fmt.Errorf("%s names documentation source %q and does not publish it", name, source)}
	}
	return nil
}

func validateDeclared(name string, claim declared) []error {
	if claim.Range == nil {
		if strings.TrimSpace(claim.Statement) == "" {
			return []error{fmt.Errorf("%s declares no supported range and does not say why", name)}
		}
		return nil
	}
	if strings.TrimSpace(*claim.Range) == "" {
		return []error{fmt.Errorf("%s declares an empty range; absence is written as null, which reads differently", name)}
	}
	return nil
}

// validateVerified holds the measurements, and refuses the two shapes that turn
// a measurement into a guess: a claim with no evidence behind it, and an empty
// list with no statement of what is missing.
func validateVerified(name string, entry release, evidenceByName map[string]evidence) []error {
	var problems []error

	if len(entry.Verified) == 0 {
		if strings.TrimSpace(entry.UnverifiedReason) == "" {
			problems = append(problems, fmt.Errorf(
				"%s records no verified Ptah build and does not say which check is missing; "+
					"an empty list reads as compatible with everything", name))
		}
		return problems
	}
	if strings.TrimSpace(entry.UnverifiedReason) != "" {
		problems = append(problems, fmt.Errorf(
			"%s records verified builds and an unverified reason; one of the two is stale", name))
	}

	seen := make(map[string]bool, len(entry.Verified))
	for _, measurement := range entry.Verified {
		if seen[measurement.PtahCommit] {
			problems = append(problems, fmt.Errorf("%s verifies commit %s twice", name, measurement.PtahCommit))
		}
		seen[measurement.PtahCommit] = true
		problems = append(problems, validateMeasurement(name, measurement, entry.Declared, evidenceByName)...)
	}
	return problems
}

func validateMeasurement(name string, measurement verified, claim declared, evidenceByName map[string]evidence) []error {
	var problems []error

	if !commitPattern.MatchString(measurement.PtahCommit) {
		problems = append(problems, fmt.Errorf(
			"%s verifies Ptah commit %q, which is not an exact 40-character lowercase Git commit",
			name, measurement.PtahCommit))
	}
	if measurement.PtahRelease != nil && !releasePattern.MatchString(*measurement.PtahRelease) {
		problems = append(problems, fmt.Errorf(
			"%s names verified Ptah release %q, which is not vMAJOR.MINOR.PATCH; a build that is not a release is written as null",
			name, *measurement.PtahRelease))
	}
	problems = append(problems, validateDescribe(name, measurement)...)

	if strings.TrimSpace(measurement.Scope) == "" {
		problems = append(problems, fmt.Errorf("%s verifies %s and does not say what ran", name, measurement.PtahCommit))
	}
	if _, found := evidenceByName[measurement.Evidence]; !found {
		problems = append(problems, fmt.Errorf(
			"%s cites evidence %q, which the catalogue does not describe", name, measurement.Evidence))
	}
	if measurement.PtahRelease != nil && claim.Range == nil && strings.TrimSpace(claim.Statement) == "" {
		problems = append(problems, fmt.Errorf("%s verified a release and declares neither a range nor a reason", name))
	}
	return problems
}

// validateDescribe ties the human-readable identity to the commit it names.
//
// The string is what `git describe --tags --always` calls the commit in a
// complete Ptah checkout, and it is in the catalogue so a reader sees something
// other than forty hex characters. A shallow checkout describes the same commit
// as a bare abbreviation, so the shape is not fixed -- what must hold is that
// whatever is written here is an identity OF this commit.
func validateDescribe(name string, measurement verified) []error {
	described := strings.TrimSpace(measurement.PtahDescribe)
	if described == "" {
		return []error{fmt.Errorf("%s verifies %s with no readable identity for it", name, measurement.PtahCommit)}
	}
	if len(described) > ptahVersionLimit {
		return []error{fmt.Errorf(
			"%s describes %s in %d bytes; the chart binds at most %d",
			name, measurement.PtahCommit, len(described), ptahVersionLimit)}
	}
	if !commitPattern.MatchString(measurement.PtahCommit) {
		return nil
	}
	if match := describePattern.FindStringSubmatch(described); match != nil {
		if !strings.HasPrefix(measurement.PtahCommit, match[1]) {
			return []error{fmt.Errorf(
				"%s describes commit %s as %q, whose abbreviation names a different commit",
				name, measurement.PtahCommit, described)}
		}
		return nil
	}
	if strings.HasPrefix(measurement.PtahCommit, described) {
		return nil
	}
	return []error{fmt.Errorf(
		"%s describes commit %s as %q, which is neither a describe of it nor an abbreviation of it",
		name, measurement.PtahCommit, described)}
}

// validateEvidenceIsUsed refuses an evidence entry nothing cites. A description
// of a check that backs no claim is a check somebody stopped running.
func validateEvidenceIsUsed(loaded catalog) []error {
	cited := make(map[string]bool, len(loaded.Evidence))
	for _, entry := range loaded.Releases {
		for _, measurement := range entry.Verified {
			cited[measurement.Evidence] = true
		}
	}
	var problems []error
	for name, description := range loaded.Evidence {
		if !cited[name] {
			problems = append(problems, fmt.Errorf("evidence %q backs no verified row", name))
		}
		if strings.TrimSpace(description.What) == "" {
			problems = append(problems, fmt.Errorf("evidence %q does not say what runs", name))
		}
		if strings.TrimSpace(description.Pin) == "" {
			problems = append(problems, fmt.Errorf("evidence %q does not say how the tested build and this claim stay one declaration", name))
		}
	}
	return problems
}

// checkSinglePin refuses a second copy of the tested Ptah commit.
//
// The catalogue is only worth reading while it is the declaration the suite
// actually builds from. A commit written into the lifecycle script or the
// workflow as well would keep working on the day the two disagree, and the
// published matrix would then name a build nothing ran.
func checkSinglePin(loaded catalog, paths []string) error {
	commit, err := edgeCommit(loaded)
	if err != nil {
		return err
	}

	var problems []error
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", path, err))
			continue
		}
		for _, found := range looseCommit.FindAllString(string(raw), -1) {
			if found == commit {
				problems = append(problems, fmt.Errorf(
					"%s writes the verified Ptah commit down a second time; %s is the one declaration and the file has to read it",
					path, catalogPath))
				break
			}
		}
	}
	return errors.Join(problems...)
}

// edgeCommit is the Ptah build the development state is verified against, which
// is the commit the lifecycle suite builds its executor from.
func edgeCommit(loaded catalog) (string, error) {
	for _, entry := range loaded.Releases {
		if entry.Operator != edgeVersion {
			continue
		}
		if len(entry.Verified) == 0 {
			return "", fmt.Errorf("%s records no verified Ptah build for %s", catalogPath, edgeVersion)
		}
		return entry.Verified[0].PtahCommit, nil
	}
	return "", fmt.Errorf("%s names no %s row", catalogPath, edgeVersion)
}
