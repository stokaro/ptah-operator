package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func soakCycleFixture() (*scenarios, cycleEvidence) {
	s := soakScenarios()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	s.soakWindow = &window{Name: "soak", Start: start, End: end, Outcome: map[string]string{"completedRounds": "9"}}
	for round := 1; round <= 9; round++ {
		at := start.Add(time.Duration(round-1)*10*time.Minute + 10*time.Second)
		s.windows = append(s.windows, window{Name: fmt.Sprintf("soak update %02d", round), Start: at, End: at.Add(2 * time.Minute),
			Outcome: map[string]string{"converged": "2m0s", "moved": "10", "round": fmt.Sprint(round)}})
	}
	evidence := cycleEvidence{}
	for _, family := range []string{"schema", "migration"} {
		for _, ns := range s.in.namespaces {
			evidence.Histories = append(evidence.Histories, cycleHistory{Family: family, Namespace: ns, StartedAt: start.Add(-time.Minute), EndedAt: end.Add(time.Second)})
		}
		for i := range 10 {
			name := s.schemaName(i)
			if family == "migration" {
				name = s.migrationName(i)
			}
			ns := s.in.namespaceFor(i)
			uid := family + fmt.Sprint(i)
			life := cycleLifetime{Family: family, Namespace: ns, Name: name, UID: uid, CreatedAt: start.Add(-time.Hour), FirstSeenAt: start.Add(-time.Minute)}
			if i < s.load.ChangeBatch {
				// A slot the rounds move: two read-only cycles in the gap after
				// each round converges, eighteen in all, short of the count the
				// unchanged slots are held to.
				evidence.Lifetimes = append(evidence.Lifetimes, life)
				for round := range 9 {
					for _, minute := range []int{4, 7} {
						at := start.Add(time.Duration(round*10+minute) * time.Minute)
						evidence.Completed = append(evidence.Completed, completedCycle{Family: family, Namespace: ns, Name: name, UID: uid, StartedAt: at.Add(-30 * time.Second), CompletedAt: at, ConfirmedAt: at})
					}
				}
				continue
			}
			if i < 8 {
				evidence.Lifetimes = append(evidence.Lifetimes, life)
				for n := range 30 {
					at := start.Add(time.Duration(n*2+1) * time.Minute)
					evidence.Completed = append(evidence.Completed, completedCycle{Family: family, Namespace: ns, Name: name, UID: uid, StartedAt: at.Add(-30 * time.Second), CompletedAt: at, ConfirmedAt: at})
				}
				continue
			}
			for round := 1; round <= 9; round++ {
				created := start.Add(time.Duration((round-1)*10+1) * time.Minute)
				deleted := created.Add(-500 * time.Millisecond)
				life.DeletedSeenAt = &deleted
				evidence.Lifetimes = append(evidence.Lifetimes, life)
				next := fmt.Sprintf("%s-round-%d", uid, round)
				s.churnProofs = append(s.churnProofs, churnProof{Round: round, Family: family, Namespace: ns, Name: name, OldUID: life.UID, NewUID: next, DeleteSubmittedAt: created.Add(-time.Second), CreatedAt: created, ExportPath: "retained.json", ExportSHA256: strings.Repeat("b", 64), GarbageCollected: true})
				life = cycleLifetime{Family: family, Namespace: ns, Name: name, UID: next, CreatedAt: created, FirstSeenAt: created.Add(time.Millisecond)}
				for n := 1; n <= 4; n++ {
					at := created.Add(time.Duration(n*2) * time.Minute)
					evidence.Completed = append(evidence.Completed, completedCycle{Family: family, Namespace: ns, Name: name, UID: next, StartedAt: at.Add(-30 * time.Second), CompletedAt: at, ConfirmedAt: at})
				}
			}
			evidence.Lifetimes = append(evidence.Lifetimes, life)
		}
	}
	return s, evidence
}

func TestSoakCountsCyclesAcrossOnlyDeclaredLifetimes(t *testing.T) {
	s, evidence := soakCycleFixture()
	if err := s.validateSoakCycles(evidence); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"too few cycles", "unchanged slot with only gap cycles", "moved slot misses a gap", "gap holds only straddling cycles", "missing update round", "unconverged update round", "no cycle in long incarnation", "short soak", "missing churn", "same UID", "missing export", "unfinished GC", "missing lifetime", "extra replacement", "wrong namespace", "shifted cadence", "no watch", "watch gap", "cycle outside window"} {
		t.Run(name, func(t *testing.T) {
			s, evidence := soakCycleFixture()
			switch name {
			case "too few cycles":
				evidence.Completed = withoutCycles(evidence.Completed, 1, func(c completedCycle) bool { return c.Name == s.schemaName(5) })
			case "unchanged slot with only gap cycles":
				// The moved slots' rule does not reach the unchanged ones.
				evidence.Completed = withoutCycles(evidence.Completed, 12, func(c completedCycle) bool { return c.Name == s.migrationName(6) })
			case "moved slot misses a gap":
				gap := s.windows[3]
				evidence.Completed = withoutCycles(evidence.Completed, -1, func(c completedCycle) bool {
					return c.Name == s.schemaName(0) && c.StartedAt.After(gap.End) && c.CompletedAt.Before(s.windows[4].Start)
				})
			case "gap holds only straddling cycles":
				// The cycles are there, but one began before the round converged
				// and the other ended after the next round began.
				for i, c := range evidence.Completed {
					if c.Name != s.migrationName(2) || !c.StartedAt.After(s.windows[5].End) || !c.CompletedAt.Before(s.windows[6].Start) {
						continue
					}
					if c.CompletedAt.Sub(s.windows[5].End) < 3*time.Minute {
						evidence.Completed[i].StartedAt = s.windows[5].End.Add(-time.Second)
					} else {
						evidence.Completed[i].CompletedAt = s.windows[6].Start.Add(time.Second)
						evidence.Completed[i].ConfirmedAt = evidence.Completed[i].CompletedAt
					}
				}
			case "missing update round":
				s.windows = append(s.windows[:4], s.windows[5:]...)
			case "unconverged update round":
				s.windows[2].Outcome = map[string]string{"error": "timed out", "round": "3"}
			case "no cycle in long incarnation":
				uid := s.churnProofs[0].NewUID
				var cycles []completedCycle
				for _, cycle := range evidence.Completed {
					if cycle.UID != uid {
						cycles = append(cycles, cycle)
					}
				}
				evidence.Completed = cycles // The slot still has 32 cycles, but one incarnation has none.
			case "short soak":
				s.soakWindow.Outcome["completedRounds"] = "8"
			case "missing churn":
				s.churnProofs = s.churnProofs[1:]
			case "same UID":
				s.churnProofs[0].NewUID = s.churnProofs[0].OldUID
			case "missing export":
				s.churnProofs[0].ExportSHA256 = ""
			case "unfinished GC":
				s.churnProofs[0].GarbageCollected = false
			case "missing lifetime":
				evidence.Lifetimes = evidence.Lifetimes[1:]
			case "extra replacement":
				extra := evidence.Lifetimes[0]
				extra.UID = "unexplained"
				evidence.Lifetimes = append(evidence.Lifetimes, extra)
			case "wrong namespace":
				s.churnProofs[0].Namespace = "other"
			case "shifted cadence":
				s.churnProofs[0].DeleteSubmittedAt = s.soakWindow.Start.Add(-time.Second)
			case "no watch":
				evidence.Histories = nil
			case "watch gap":
				evidence.Histories[0].Error = "expired history"
			case "cycle outside window":
				for i, c := range evidence.Completed {
					if c.Name == s.schemaName(5) {
						evidence.Completed[i].ConfirmedAt = s.soakWindow.End.Add(time.Second)
						break
					}
				}
			}
			if err := s.validateSoakCycles(evidence); err == nil {
				t.Fatal("incomplete soak accepted")
			}
		})
	}
}

// withoutCycles drops the first n cycles drop selects, or all of them when n
// is negative.
func withoutCycles(cycles []completedCycle, n int, drop func(completedCycle) bool) []completedCycle {
	var kept []completedCycle
	for _, c := range cycles {
		if n != 0 && drop(c) {
			n--
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// The split itself: a moved slot passes on eighteen cycles because it came
// back to read-only refresh after every round, and the count still binds the
// slots the rounds never touch.
func TestTheCycleTargetSplitsMovedFromUnchangedSlots(t *testing.T) {
	s, evidence := soakCycleFixture()
	counts, problems := cyclesInWindow(*s.soakWindow, evidence)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	moved := 0
	for _, c := range counts {
		if c.Name == s.schemaName(0) {
			moved = c.Completed
		}
	}
	if moved >= s.load.Soak.MinimumCycles {
		t.Fatalf("the fixture's moved slot has %d cycles; it has to fall short of %d to measure the split", moved, s.load.Soak.MinimumCycles)
	}
	if got := s.cycleTargetProblems(*s.soakWindow, counts, evidence); len(got) != 0 {
		t.Fatalf("a moved slot that refreshed after every round was refused: %v", got)
	}
}
