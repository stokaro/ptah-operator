package dataplane

import (
	"errors"
	"fmt"
	"regexp"
)

// The outcomes of the drift check `migrations up --check-drift` reports, as
// Ptah v0.28.0 spells them.
const (
	MigrationDriftClean   = "clean"
	MigrationDriftDrifted = "drifted"
	MigrationDriftUnable  = "unable"
)

// MigrationDriftCheck is the drift_check object of a run document. Ptah writes
// it when --check-drift was passed and the check ran, before any pending
// migration: a run whose check did not come back clean applied nothing.
//
// Message, and each finding's message and names, are Ptah's text: a message
// can carry what the driver said about the shadow database, its host or its
// user, so the frame keeps a summary of it and nothing else.
type MigrationDriftCheck struct {
	Outcome  string                  `json:"outcome"`
	Replayed []int64                 `json:"replayed,omitempty"`
	Stage    string                  `json:"stage,omitempty"`
	Reason   string                  `json:"reason,omitempty"`
	Message  string                  `json:"message,omitempty"`
	Findings []MigrationDriftFinding `json:"findings,omitempty"`
}

// MigrationDriftFinding is one difference a drifted check found, or one object
// an unable check could not compare.
type MigrationDriftFinding struct {
	Kind string `json:"kind"`
}

// MigrationDriftSummary is a drift check in the terms a frame carries: the
// outcome, the stage and reason codes Ptah gave an unable one, and how many
// findings there were. It carries no text and no object name.
type MigrationDriftSummary struct {
	Outcome  string `json:"outcome"`
	Stage    string `json:"stage,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Findings int    `json:"findings,omitempty"`
}

// driftCode is a stage or a reason as Ptah spells them: a stage in kebab case
// ("schema-match"), a reason in snake case ("not_clean"). Ptah treats the set
// as open, so the shape is checked rather than the members.
var driftCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// maxDriftFindings bounds the count a summary may carry. A schema has far
// fewer objects than this; a count above it is not a report of one.
const maxDriftFindings = 1 << 20

// Summary is the frame's account of the check.
func (c *MigrationDriftCheck) Summary() *MigrationDriftSummary {
	if c == nil {
		return nil
	}
	return &MigrationDriftSummary{Outcome: c.Outcome, Stage: c.Stage, Reason: c.Reason, Findings: len(c.Findings)}
}

// Validate holds a summary to the shape each outcome has: a clean check names
// no stage, reason or finding, a drifted one names at least one finding and no
// stage or reason, and an unable one names a stage and a reason.
func (s MigrationDriftSummary) Validate() error {
	if s.Findings < 0 || s.Findings > maxDriftFindings {
		return fmt.Errorf("drift check reports %d findings", s.Findings)
	}
	switch s.Outcome {
	case MigrationDriftClean:
		if s.Stage != "" || s.Reason != "" || s.Findings != 0 {
			return errors.New("a clean drift check names a stage, a reason or a finding")
		}
	case MigrationDriftDrifted:
		if s.Stage != "" || s.Reason != "" || s.Findings == 0 {
			return errors.New("a drifted check names no finding, or names a stage or a reason")
		}
	case MigrationDriftUnable:
		if !driftCode.MatchString(s.Stage) || !driftCode.MatchString(s.Reason) {
			return errors.New("an unable drift check lacks a stage or a reason code")
		}
	default:
		return fmt.Errorf("drift check reports outcome %q, which this operator does not know", s.Outcome)
	}
	return nil
}

// Refused reports a check that stopped the run: drifted or unable.
func (s *MigrationDriftSummary) Refused() bool {
	return s != nil && s.Outcome != MigrationDriftClean
}

// validateDriftCheck holds a run's drift check to the run around it. A check
// that did not come back clean stopped the run before any pending migration,
// so the run failed, applied nothing and sent no statement.
func (r MigrationRunReport) validateDriftCheck() error {
	if r.DriftCheck == nil {
		return nil
	}
	summary := r.DriftCheck.Summary()
	if err := summary.Validate(); err != nil {
		return err
	}
	for _, finding := range r.DriftCheck.Findings {
		if !driftCode.MatchString(finding.Kind) {
			return errors.New("a drift finding lacks a kind code")
		}
	}
	if summary.Refused() && (r.Outcome != MigrationOutcomeFailed || len(r.Applied) != 0 || r.Failure != nil) {
		return fmt.Errorf("a drift check that came back %s stopped a run that reports %s, an applied version or a failed statement",
			summary.Outcome, r.Outcome)
	}
	return nil
}
