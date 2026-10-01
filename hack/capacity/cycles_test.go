package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cycleFixture(family string) cycleHistory {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := cycleHistory{Family: family, Namespace: "work", StartedAt: base, EndedAt: base.Add(time.Minute)}
	stages := []string{"Resolve", "Verify", "Observe", "Plan"}
	if family == "migration" {
		stages = []string{"Resolve", "Verify", "History"}
	}
	for i, stage := range stages {
		at := base.Add(time.Duration(i*3+1) * time.Second)
		op := cycleOperation{Type: stage, ID: stage + "-claim", JobName: stage + "-job", JobUID: stage + "-uid", StartedAt: at}
		for _, bound := range []bool{false, true} {
			copy := op
			if !bound {
				copy.JobUID = ""
			}
			h.Readings = append(h.Readings, cycleReading{Event: "MODIFIED", ReceivedAt: at.Add(time.Second), ResourceVersion: fmt.Sprint(len(h.Readings) + 1), Namespace: "work", Name: "slot", UID: "original", CreatedAt: base, Generation: 1, ObservedGeneration: 1, Operation: &copy})
		}
	}
	last := h.Readings[len(h.Readings)-1]
	last.Operation = nil
	last.ResourceVersion = "complete"
	last.CompletedAt = base.Add(15 * time.Second)
	last.ReceivedAt = base.Add(16 * time.Second)
	reason := "InSync"
	if family == "migration" {
		reason = "HistoryMatched"
	}
	last.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: reason, ObservedGeneration: 1}, {Type: "InSync", Status: metav1.ConditionTrue, Reason: "ScopedConverged", ObservedGeneration: 1}}
	h.Readings = append(h.Readings, last)
	return h
}

func TestReadOnlyCyclesRequireTheWholeClaimSequence(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			good := cycleFixture(family)
			completed, lives := completedCycles(good)
			if len(completed) != 1 || len(lives) != 1 || completed[0].UID != "original" {
				t.Fatalf("valid full cycle refused: %+v", completed)
			}
			for name, change := range map[string]func(*cycleHistory){
				"initial state":     func(h *cycleHistory) { h.Readings = h.Readings[len(h.Readings)-1:]; h.Readings[0].Event = "INITIAL" },
				"missing verify":    func(h *cycleHistory) { h.Readings = append(h.Readings[:2], h.Readings[4:]...) },
				"unbound Job":       func(h *cycleHistory) { h.Readings[3].Operation.JobUID = "" },
				"Apply in sequence": func(h *cycleHistory) { h.Readings[2].Operation.Type = "Apply" },
				"new UID": func(h *cycleHistory) {
					for i := 2; i < len(h.Readings); i++ {
						h.Readings[i].UID = "replacement"
					}
				},
				"new generation": func(h *cycleHistory) {
					for i := 2; i < len(h.Readings); i++ {
						h.Readings[i].Generation = 2
						h.Readings[i].ObservedGeneration = 2
					}
				},
				"stale completion":         func(h *cycleHistory) { h.Readings[len(h.Readings)-1].CompletedAt = h.StartedAt },
				"stale condition":          func(h *cycleHistory) { h.Readings[len(h.Readings)-1].Conditions[0].ObservedGeneration = 0 },
				"not ready":                func(h *cycleHistory) { h.Readings[len(h.Readings)-1].Conditions[0].Status = metav1.ConditionFalse },
				"unresolved or post-Apply": func(h *cycleHistory) { h.Readings[len(h.Readings)-1].Unsafe = true },
				"reused Job UID":           func(h *cycleHistory) { h.Readings[len(h.Readings)-2].Operation.JobUID = h.Readings[1].Operation.JobUID },
				"claim identity changes": func(h *cycleHistory) {
					h.Readings[1].Operation.StartedAt = h.Readings[1].Operation.StartedAt.Add(time.Second)
				},
			} {
				t.Run(name, func(t *testing.T) {
					h := cycleFixture(family)
					change(&h)
					if cycles, _ := completedCycles(h); len(cycles) != 0 {
						t.Fatalf("invalid cycle counted: %+v", cycles)
					}
				})
			}
			t.Run("repeat convergence and lock release", func(t *testing.T) {
				h := cycleFixture(family)
				last := h.Readings[len(h.Readings)-1]
				h.Readings[len(h.Readings)-1].PendingRelease = true
				h.Readings = append(h.Readings, last, last)
				if got, _ := completedCycles(h); len(got) != 1 {
					t.Fatalf("one cycle became %d after release/repeated status", len(got))
				}
			})
		})
	}
}

func TestCycleCountsStayWithinUIDLifetimesAndWindow(t *testing.T) {
	h := cycleFixture("migration")
	completed, lives := completedCycles(h)
	replacement := lives[0]
	replacement.UID = "replacement"
	replacement.CreatedAt = h.StartedAt.Add(20 * time.Second)
	replacement.FirstSeenAt = replacement.CreatedAt
	deleted := h.StartedAt.Add(19 * time.Second)
	lives[0].DeletedSeenAt = &deleted
	lives = append(lives, replacement)
	evidence := cycleEvidence{Histories: []cycleHistory{h}, Completed: completed, Lifetimes: lives}
	counts, problems := cyclesInWindow(window{Start: h.StartedAt, End: h.EndedAt}, evidence)
	if len(problems) != 0 || len(counts) != 2 {
		t.Fatalf("valid UID lifetimes refused: %v", problems)
	}
	for _, count := range counts {
		want := 0
		if count.UID == "original" {
			want = 1
		}
		if count.Completed != want {
			t.Fatal("cycle was carried into a replacement")
		}
	}
	counts, problems = cyclesInWindow(window{Start: h.StartedAt.Add(2 * time.Second), End: h.EndedAt}, evidence)
	for _, count := range counts {
		if count.Completed != 0 {
			t.Fatal("cycle crossing the window start was counted")
		}
	}
	evidence.Histories[0].Error = "resource version expired"
	if _, problems = cyclesInWindow(window{Start: h.StartedAt, End: h.EndedAt}, evidence); len(problems) == 0 {
		t.Fatal("lost watch history accepted")
	}
}

func TestNativeMigrationCycleRequiresItsClaims(t *testing.T) {
	body, err := os.ReadFile("testdata/native-migration-cycle.json")
	if err != nil {
		t.Fatal(err)
	}
	var h cycleHistory
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	cycles, _ := completedCycles(h)
	if len(cycles) != 1 || len(cycles[0].Operations) != 3 || cycles[0].UID != "127c798c-b414-48fe-a050-3e9531cc452f" {
		t.Fatalf("native cycle not reproduced: %+v", cycles)
	}
	withoutVerify := h
	withoutVerify.Readings = nil
	for _, reading := range h.Readings {
		if reading.Operation == nil || reading.Operation.Type != "Verify" {
			withoutVerify.Readings = append(withoutVerify.Readings, reading)
		}
	}
	if cycles, _ := completedCycles(withoutVerify); len(cycles) != 0 {
		t.Fatal("native cycle survived removal of verification")
	}
	replay := h
	replay.Readings = append(append([]cycleReading(nil), h.Readings...), h.Readings...)
	if cycles, _ := completedCycles(replay); len(cycles) != 1 {
		t.Fatal("replayed claims counted a second cycle")
	}
}

func TestCycleReportRetainsReplayAndRefusesGaps(t *testing.T) {
	for _, gap := range []bool{false, true} {
		t.Run(fmt.Sprint(gap), func(t *testing.T) {
			h := cycleFixture("migration")
			if gap {
				h.Error = "watch cursor expired"
			}
			completed, lives := completedCycles(h)
			evidence := cycleEvidence{Histories: []cycleHistory{h}, Completed: completed, Lifetimes: lives}
			w := window{Name: "steady", Start: h.StartedAt, End: h.EndedAt}
			counts, problems := cyclesInWindow(w, evidence)
			scenario := scenarioCost{window: w, Samples: 1, RefreshCycles: counts, CycleProblems: problems, Incomplete: map[string]int{}}
			if len(problems) > 0 {
				scenario.Incomplete[sourceCycles] = len(problems)
			}
			dir := t.TempDir()
			if err := writeReport(dir, report{FormatVersion: 3, Cycles: evidence, Scenarios: []scenarioCost{scenario}}); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var restored report
			if err := json.Unmarshal(body, &restored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(evidence, restored.Cycles) {
				t.Fatal("report lost raw cycle evidence")
			}
			replayed, replayedLives := completedCycles(restored.Cycles.Histories[0])
			if !reflect.DeepEqual(replayed, completed) || !reflect.DeepEqual(replayedLives, lives) {
				t.Fatal("retained history cannot reproduce cycles")
			}
			if gap {
				if restored.Scenarios[0].RefreshCycles != nil || len(restored.Scenarios[0].CycleProblems) == 0 {
					t.Fatal("incomplete history published a cycle count")
				}
			} else if !reflect.DeepEqual(counts, restored.Scenarios[0].RefreshCycles) {
				t.Fatal("report changed measured cycle counts")
			}
		})
	}
}

func TestCycleReleasedAfterWindowDoesNotCountInsideIt(t *testing.T) {
	h := cycleFixture("migration")
	last := h.Readings[len(h.Readings)-1]
	h.Readings[len(h.Readings)-1].PendingRelease = true
	last.ReceivedAt = h.StartedAt.Add(40 * time.Second)
	h.Readings = append(h.Readings, last)
	completed, lives := completedCycles(h)
	if len(completed) != 1 {
		t.Fatal("released cycle was not recorded")
	}
	counts, problems := cyclesInWindow(window{Start: h.StartedAt, End: h.StartedAt.Add(30 * time.Second)}, cycleEvidence{Histories: []cycleHistory{h}, Completed: completed, Lifetimes: lives})
	if len(problems) != 0 || len(counts) != 1 || counts[0].Completed != 0 {
		t.Fatalf("cycle still owing a lock release at window end counted: %+v %v", counts, problems)
	}
}
