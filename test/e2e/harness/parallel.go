package harness

import "testing"

// ParallelCase owns the test state and fixtures used by one parallel lane.
type ParallelCase struct {
	Name string
	Run  func(*testing.T)
}

// ParallelPair overlaps two independent lanes and joins them before returning.
// A skipped or filtered lane fails the pair, just as it fails a Scenario.
// The caller must keep cluster-wide faults outside the pair and isolate any
// database-server locks; distinct database names do not always isolate locks.
func ParallelPair(t *testing.T, name string, cases [2]ParallelCase) bool {
	t.Helper()
	var finished [2]bool
	passed := t.Run(name, func(t *testing.T) {
		for i, row := range cases {
			t.Run(row.Name, func(t *testing.T) {
				t.Parallel()
				row.Run(t)
				finished[i] = !t.Skipped()
			})
		}
	})
	for i, done := range finished {
		if !done {
			t.Errorf("e2e: parallel lane %s did not finish", cases[i].Name)
			passed = false
		}
	}
	return passed
}
