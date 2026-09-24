package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// A declared profile is the half of the acceptance record this tree cannot
// answer: what was built, what it was installed with, what the deployment
// promises, and which run produced the evidence.
//
// It is read rather than typed into the printed record because #242 turns on
// the difference between a value and a claim. A record assembled by hand can
// say "Accepted" beside a requirement nothing ran for, and nothing notices. A
// record assembled from a declared profile can be refused, and this file is
// where it is.
type profile struct {
	ManagerImageDigest  string                 `json:"managerImageDigest"`
	RunnerImageDigest   string                 `json:"runnerImageDigest"`
	ExecutorImageDigest string                 `json:"executorImageDigest"`
	ChartDigest         string                 `json:"chartDigest"`
	InstallationValues  string                 `json:"installationValues"`
	OperatingTargets    string                 `json:"operatingTargets"`
	RecoveryObjectives  string                 `json:"recoveryObjectives"`
	RunEvidence         string                 `json:"runEvidence"`
	Requirements        map[string]disposition `json:"requirements"`
}

// disposition is one requirement's verdict and what stands behind it.
type disposition struct {
	Disposition string `json:"disposition"`
	Evidence    string `json:"evidence"`
}

// The three verdicts #242 defines. Anything else is a verdict this record
// cannot carry, which is a refusal rather than a value to pass through.
const (
	dispositionNotAssessed = "Not assessed"
	dispositionRejected    = "Rejected"
	dispositionAccepted    = "Accepted for the stated profile"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// readProfile reads and validates a declared profile.
//
// Every refusal here is a way the record could otherwise claim more than the
// deployment did.
func readProfile(path string) (*profile, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the acceptance profile: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	declared := &profile{}
	if err := decoder.Decode(declared); err != nil {
		return nil, fmt.Errorf("read the acceptance profile: %w", err)
	}

	for _, digest := range []struct {
		name  string
		value string
	}{
		{"managerImageDigest", declared.ManagerImageDigest},
		{"runnerImageDigest", declared.RunnerImageDigest},
		{"executorImageDigest", declared.ExecutorImageDigest},
		{"chartDigest", declared.ChartDigest},
	} {
		if digest.value != "" && !digestPattern.MatchString(digest.value) {
			return nil, fmt.Errorf(
				"acceptance profile: %s is %q, which is not a lowercase sha256 digest; "+
					"a record binds results to exact bytes, and a tag does not",
				digest.name, digest.value)
		}
	}

	known := map[string]bool{}
	for _, entry := range requirements {
		known[entry.id] = true
	}
	for _, id := range sortedKeys(declared.Requirements) {
		if !known[id] {
			return nil, fmt.Errorf("acceptance profile: %q is not a requirement this record carries", id)
		}
		verdict := declared.Requirements[id]
		switch verdict.Disposition {
		case dispositionNotAssessed, dispositionRejected, dispositionAccepted:
		case "":
			return nil, fmt.Errorf("acceptance profile: %s names no disposition", id)
		default:
			return nil, fmt.Errorf(
				"acceptance profile: %s is %q, which is not one of %q, %q or %q",
				id, verdict.Disposition, dispositionNotAssessed, dispositionRejected, dispositionAccepted)
		}
		if verdict.Disposition == dispositionAccepted {
			if strings.TrimSpace(verdict.Evidence) == "" {
				return nil, fmt.Errorf(
					"acceptance profile: %s is accepted with no evidence; a closed issue, a merged fix "+
						"and a green run against a pull request are none of them evidence about a candidate",
					id)
			}
			if missing := declared.unfilled(); len(missing) > 0 {
				return nil, fmt.Errorf(
					"acceptance profile: %s is accepted while the profile leaves %s unfilled; "+
						"an unfilled capacity or recovery target leaves acceptance incomplete",
					id, strings.Join(missing, ", "))
			}
		}
	}
	return declared, nil
}

// unfilled names the candidate values the profile has not declared. A record
// cannot accept a requirement while any of them is missing, because the
// profile acceptance would be stated for is not yet fixed.
func (p *profile) unfilled() []string {
	var missing []string
	for _, field := range []struct {
		name  string
		value string
	}{
		{"managerImageDigest", p.ManagerImageDigest},
		{"runnerImageDigest", p.RunnerImageDigest},
		{"executorImageDigest", p.ExecutorImageDigest},
		{"chartDigest", p.ChartDigest},
		{"installationValues", p.InstallationValues},
		{"operatingTargets", p.OperatingTargets},
		{"recoveryObjectives", p.RecoveryObjectives},
		{"runEvidence", p.RunEvidence},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	return missing
}

// verdictFor returns what the profile says about one requirement. An absent
// entry is not assessed, which is what an empty profile means for every row.
func (p *profile) verdictFor(id string) disposition {
	if p == nil {
		return disposition{Disposition: dispositionNotAssessed}
	}
	verdict, declared := p.Requirements[id]
	if !declared || verdict.Disposition == "" {
		return disposition{Disposition: dispositionNotAssessed}
	}
	return verdict
}

func sortedKeys(entries map[string]disposition) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
