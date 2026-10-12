package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// cycleVerdict is a retained soak report judged against the cycle target the
// driver applies now.
type cycleVerdict struct {
	Report       string   `json:"report"`
	ReportSHA256 string   `json:"reportSHA256"`
	Rule         string   `json:"rule"`
	Verdict      string   `json:"verdict"`
	Problems     []string `json:"problems"`
}

// reevaluateCycles judges a retained soak report's cycles against the target
// cycleTargetProblems holds a live run to, without a cluster. A profile
// revision that changes the target is applied to measurements already taken,
// by the same code; the report and its original verdict are left as they are.
// Only the cycle evidence is judged: the churn proofs and every other check of
// the original run keep the verdict that run gave them.
func reevaluateCycles(path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if rep.Workload.Soak == nil {
		return errors.New("the report is not a soak measurement")
	}
	names, err := reportNamespaces(rep.Environment["workloadNamespaces"])
	if err != nil {
		return err
	}
	s := &scenarios{load: rep.Workload}
	s.in.namespaces, s.in.namespace = names, names[0]
	for _, cost := range rep.Scenarios {
		s.windows = append(s.windows, cost.window)
		if cost.Name == "soak" {
			w := cost.window
			s.soakWindow = &w
		}
	}
	if s.soakWindow == nil {
		return errors.New("the report has no soak window")
	}
	counts, problems := cyclesInWindow(*s.soakWindow, rep.Cycles)
	problems = append(problems, s.cycleTargetProblems(*s.soakWindow, counts, rep.Cycles)...)
	sum := sha256.Sum256(raw)
	verdict := cycleVerdict{Report: path, ReportSHA256: hex.EncodeToString(sum[:]), Rule: "profile revision 48 soak cycle target", Verdict: "PASS", Problems: problems}
	if len(problems) > 0 {
		verdict.Verdict = "FAIL"
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(verdict)
}

func reportNamespaces(value any) ([]string, error) {
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("the report records no workload namespaces")
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		name, ok := item.(string)
		if !ok || name == "" {
			return nil, errors.New("the report records an invalid workload namespace")
		}
		names = append(names, name)
	}
	return names, nil
}
