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
	for _, name := range []string{"too few cycles", "no cycle in long incarnation", "short soak", "missing churn", "same UID", "missing export", "unfinished GC", "missing lifetime", "extra replacement", "wrong namespace", "shifted cadence", "no watch", "watch gap", "cycle outside window"} {
		t.Run(name, func(t *testing.T) {
			s, evidence := soakCycleFixture()
			switch name {
			case "too few cycles":
				evidence.Completed = evidence.Completed[1:]
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
				evidence.Completed[0].ConfirmedAt = s.soakWindow.End.Add(time.Second)
			}
			if err := s.validateSoakCycles(evidence); err == nil {
				t.Fatal("incomplete soak accepted")
			}
		})
	}
}
