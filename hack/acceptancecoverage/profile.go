package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
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
	Exclusions          []exclusion            `json:"exclusions"`
	Decision            string                 `json:"decision"`
}

// exclusion is a requirement the profile narrows out of the supported use.
//
// #242 permits them and fences them: one must narrow the use, name an owner
// and a review date, and say how the excluded configuration is prevented or
// detected. The fence exists because an exclusion is the cheapest way to turn
// a failing requirement into a passing record.
type exclusion struct {
	Requirement string `json:"requirement"`
	Scope       string `json:"scope"`
	Owner       string `json:"owner"`
	ReviewBy    string `json:"reviewBy"`
	Detection   string `json:"detection"`
}

// The protections no exclusion may waive, and the requirement each lives in.
// Read from #242: "An exclusion cannot waive unauthorized mutation, unsafe
// replay or overlap, credential disclosure, or loss of the record of an
// unresolved Apply."
var unwaivable = map[string]string{
	"PA-02": "unauthorized mutation",
	"PA-03": "unsafe replay or overlap, and the record of an unresolved Apply",
	"PA-05": "credential disclosure and the authority boundary",
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
	if err := declared.validateExclusions(known); err != nil {
		return nil, err
	}
	if err := declared.validateDecision(); err != nil {
		return nil, err
	}
	return declared, nil
}

// validateExclusions holds each exclusion to the shape #242 permits.
func (p *profile) validateExclusions(known map[string]bool) error {
	seen := map[string]bool{}
	for _, excluded := range p.Exclusions {
		if !known[excluded.Requirement] {
			return fmt.Errorf(
				"acceptance profile: an exclusion names %q, which is not a requirement this record carries",
				excluded.Requirement)
		}
		if seen[excluded.Requirement] {
			return fmt.Errorf("acceptance profile: %s is excluded twice", excluded.Requirement)
		}
		seen[excluded.Requirement] = true
		if waives, protected := unwaivable[excluded.Requirement]; protected {
			return fmt.Errorf(
				"acceptance profile: %s cannot be excluded, because it carries %s, "+
					"and an exclusion may narrow the supported use without waiving that",
				excluded.Requirement, waives)
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{"scope", excluded.Scope},
			{"owner", excluded.Owner},
			{"reviewBy", excluded.ReviewBy},
			{"detection", excluded.Detection},
		} {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf(
					"acceptance profile: the exclusion for %s names no %s; a permitted exclusion "+
						"narrows the use, identifies an owner and a review date, and says how the "+
						"excluded configuration is prevented or detected",
					excluded.Requirement, field.name)
			}
		}
		if _, err := time.Parse("2006-01-02", excluded.ReviewBy); err != nil {
			return fmt.Errorf(
				"acceptance profile: the exclusion for %s has reviewBy %q, which is not a date; "+
					"an exclusion without one never comes back for review",
				excluded.Requirement, excluded.ReviewBy)
		}
	}
	return nil
}

// validateDecision refuses a decision the requirements do not support.
//
// This is the one #242 turns on: "Every applicable requirement must pass."
// A record can be accepted only where every requirement is accepted or
// explicitly excluded, and where the profile acceptance is stated for is
// fixed.
func (p *profile) validateDecision() error {
	switch p.Decision {
	case "", dispositionNotAssessed, dispositionRejected:
		return nil
	case dispositionAccepted:
	default:
		return fmt.Errorf(
			"acceptance profile: the decision is %q, which is not one of %q, %q or %q",
			p.Decision, dispositionNotAssessed, dispositionRejected, dispositionAccepted)
	}

	if missing := p.unfilled(); len(missing) > 0 {
		return fmt.Errorf(
			"acceptance profile: the decision is accepted while the profile leaves %s unfilled; "+
				"the profile acceptance would be stated for is not yet fixed",
			strings.Join(missing, ", "))
	}
	excluded := map[string]bool{}
	for _, entry := range p.Exclusions {
		excluded[entry.Requirement] = true
	}
	var outstanding []string
	for _, entry := range requirements {
		if excluded[entry.id] {
			continue
		}
		if p.verdictFor(entry.id).Disposition != dispositionAccepted {
			outstanding = append(outstanding, entry.id)
		}
	}
	if len(outstanding) > 0 {
		return fmt.Errorf(
			"acceptance profile: the decision is accepted while %s %s neither accepted nor excluded; "+
				"every applicable requirement must pass",
			strings.Join(outstanding, ", "), plural(len(outstanding)))
	}
	return nil
}

func plural(count int) string {
	if count == 1 {
		return "is"
	}
	return "are"
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
