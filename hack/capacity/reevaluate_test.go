package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// soakReport writes the fixture as a retained report, the way the driver
// writes report.json, and returns its path.
func soakReport(t *testing.T, s *scenarios, evidence cycleEvidence, namespaces any) string {
	t.Helper()
	rep := report{Cycles: evidence, Workload: s.load, Environment: map[string]any{"workloadNamespaces": namespaces}}
	rep.Scenarios = append(rep.Scenarios, scenarioCost{window: *s.soakWindow})
	for _, w := range s.windows {
		rep.Scenarios = append(rep.Scenarios, scenarioCost{window: w})
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reevaluated(t *testing.T, path string) cycleVerdict {
	t.Helper()
	var out bytes.Buffer
	if err := reevaluateCycles(path, &out); err != nil {
		t.Fatal(err)
	}
	var verdict cycleVerdict
	if err := json.Unmarshal(out.Bytes(), &verdict); err != nil {
		t.Fatal(err)
	}
	return verdict
}

// A retained report is judged by the code a live run uses, and the verdict
// names the bytes it judged.
func TestARetainedSoakIsJudgedByTheLiveRule(t *testing.T) {
	s, evidence := soakCycleFixture()
	path := soakReport(t, s, evidence, fixtureNamespaces(s))
	if verdict := reevaluated(t, path); verdict.Verdict != "PASS" || len(verdict.Problems) != 0 || len(verdict.ReportSHA256) != 64 {
		t.Fatalf("verdict = %+v, want a PASS naming the report digest", verdict)
	}

	s, evidence = soakCycleFixture()
	gap := s.windows[7]
	evidence.Completed = withoutCycles(evidence.Completed, -1, func(c completedCycle) bool {
		return c.Name == s.migrationName(3) && c.StartedAt.After(gap.End) && c.CompletedAt.Before(s.windows[8].Start)
	})
	verdict := reevaluated(t, soakReport(t, s, evidence, fixtureNamespaces(s)))
	if verdict.Verdict != "FAIL" || len(verdict.Problems) != 1 || !strings.Contains(verdict.Problems[0], "capacity-migration-003") ||
		!strings.Contains(verdict.Problems[0], "update round 8 converging and update round 9") {
		t.Fatalf("verdict = %+v, want the one missing gap named", verdict)
	}
}

func TestAReportThatCannotBeJudgedIsRefused(t *testing.T) {
	s, evidence := soakCycleFixture()
	for name, path := range map[string]string{
		"no namespaces": soakReport(t, s, evidence, nil),
		"bad namespace": soakReport(t, s, evidence, []any{"work-a", 7}),
		"not a soak":    func() string { c := *s; c.load.Soak = nil; return soakReport(t, &c, evidence, fixtureNamespaces(s)) }(),
		"no soak window": func() string {
			c := *s
			w := *c.soakWindow
			w.Name = "steady state"
			c.soakWindow = &w
			return soakReport(t, &c, evidence, fixtureNamespaces(s))
		}(),
		"unreadable bytes": func() string {
			p := filepath.Join(t.TempDir(), "report.json")
			_ = os.WriteFile(p, []byte("{"), 0o600)
			return p
		}(),
	} {
		if err := reevaluateCycles(path, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: a report that cannot be judged was judged", name)
		}
	}
}

func fixtureNamespaces(s *scenarios) []any {
	names := make([]any, 0, len(s.in.namespaces))
	for _, name := range s.in.namespaces {
		names = append(names, name)
	}
	return names
}
