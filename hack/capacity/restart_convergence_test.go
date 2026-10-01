package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

type restartReplay struct {
	Start     time.Time      `json:"start"`
	Deadline  time.Time      `json:"deadline"`
	Histories []cycleHistory `json:"histories"`
}

func restartReplayFixture(t *testing.T) (*scenarios, restartReplay) {
	t.Helper()
	raw, err := os.ReadFile("testdata/restart-convergence.json")
	if err != nil {
		t.Fatal(err)
	}
	var f restartReplay
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	s := &scenarios{in: inputs{namespace: "ptah-capacity-bbe8803caf-a", namespaces: []string{"ptah-capacity-bbe8803caf-a", "ptah-capacity-bbe8803caf-b"}}, load: workload{Schemas: 10, Migrations: 10, Settle: duration{3 * time.Minute}}}
	return s, f
}

func TestRestartConvergenceReplaysStaggeredRecovery(t *testing.T) {
	s, f := restartReplayFixture(t)
	at, missing, err := s.restartConvergence(f.Histories, f.Start, f.Deadline, nil)
	if err != nil || len(missing) != 0 {
		t.Fatalf("retained recovery refused: %v %v", missing, err)
	}
	want := 174*time.Second + 467357646*time.Nanosecond
	if at.Sub(f.Start) != want {
		t.Fatalf("recovery = %s, want %s", at.Sub(f.Start), want)
	}
	// By the end of the recovery window, other resources are already doing
	// their next normal refresh. Requiring simultaneous idle status fails.
	latest := map[string]cycleReading{}
	families := map[string]string{}
	for _, h := range f.Histories {
		for _, r := range h.Readings {
			if !r.ReceivedAt.After(f.Deadline) {
				latest[r.UID] = r
				families[r.UID] = h.Family
			}
		}
		s.recorders = append(s.recorders, &cycleRecorder{history: h})
	}
	active := 0
	for uid, r := range latest {
		if !cycleReady(families[uid], r) {
			active++
		}
	}
	if len(latest) != 20 || active == 0 {
		t.Fatalf("fixture does not reproduce staggered refresh: %d resources, %d active", len(latest), active)
	}
	// An expired wall-clock deadline does not erase timely retained proof.
	// It also must not report only the duration spent waiting after approval.
	got, err := s.waitRestartConverged(t.Context(), f.Start)
	if err != nil || got != "2m54.467357646s" {
		t.Fatalf("live wait replay: %q, %v", got, err)
	}
}

func TestRestartConvergenceRefusesInvalidEvidence(t *testing.T) {
	for _, name := range []string{"no histories", "missing stream", "duplicate stream", "watch error", "late watch", "empty workload", "missing resource", "unexpected resource", "wrong namespace", "no baseline", "UID replacement", "generation changed", "deleted", "stale completion", "same fault second", "future completion", "old generation", "old condition", "not ready", "active operation", "pending release", "unresolved", "initial list only", "late convergence", "out of order", "unbound read", "old read", "future read", "wrong read", "missing read"} {
		t.Run(name, func(t *testing.T) {
			s, f := restartReplayFixture(t)
			switch name {
			case "no histories":
				f.Histories = nil
			case "missing stream":
				f.Histories = f.Histories[1:]
			case "duplicate stream":
				f.Histories = append(f.Histories, f.Histories[0])
			case "watch error":
				f.Histories[0].Error = "lost cursor"
			case "late watch":
				f.Histories[0].StartedAt = f.Start.Add(time.Second)
			case "empty workload":
				s.load.Schemas, s.load.Migrations = 0, 0
			case "late convergence":
				f.Deadline = f.Start.Add(125 * time.Second)
			case "out of order":
				f.Histories[0].Readings[0], f.Histories[0].Readings[1] = f.Histories[0].Readings[1], f.Histories[0].Readings[0]
			default:
				h := &f.Histories[0]
				target := h.Readings[0].Name
				var rows []cycleReading
				for _, r := range h.Readings {
					if r.Name == target {
						if name == "missing resource" || name == "no baseline" && r.ReceivedAt.Before(f.Start) {
							continue
						}
						if name == "unexpected resource" {
							r.Name = "unexpected"
						}
						if name == "wrong namespace" {
							r.Namespace = "wrong"
						}
						if !r.ReceivedAt.Before(f.Start) {
							switch name {
							case "unbound read":
								if r.Operation != nil {
									r.Operation.JobUID = ""
								}
							case "old read":
								if r.Operation != nil {
									r.Operation.StartedAt = f.Start.Add(-time.Second)
								}
							case "future read":
								if r.Operation != nil {
									r.Operation.StartedAt = r.ReceivedAt.Add(time.Second)
								}
							case "wrong read":
								if r.Operation != nil {
									r.Operation.Type = "Resolve"
								}
							case "missing read":
								if r.Operation != nil {
									continue
								}
							case "UID replacement":
								r.UID = "replacement"
							case "generation changed":
								r.Generation++
							case "deleted":
								r.Event = "DELETED"
							case "stale completion":
								r.CompletedAt = f.Start.Add(-time.Second)
							case "same fault second":
								r.CompletedAt = f.Start.Truncate(time.Second)
							case "future completion":
								r.CompletedAt = r.ReceivedAt.Add(time.Second)
							case "old generation":
								r.ObservedGeneration--
							case "old condition":
								for i := range r.Conditions {
									r.Conditions[i].ObservedGeneration--
								}
							case "not ready":
								r.Conditions = nil
							case "active operation":
								r.Operation = &cycleOperation{Type: "Resolve"}
							case "pending release":
								r.PendingRelease = true
							case "unresolved":
								r.Unsafe = true
							case "initial list only":
								r.Event = "INITIAL"
							}
						}
					}
					rows = append(rows, r)
				}
				h.Readings = rows
			}
			_, missing, err := s.restartConvergence(f.Histories, f.Start, f.Deadline, nil)
			if err == nil && len(missing) == 0 {
				t.Fatal("invalid restart evidence passed")
			}
		})
	}
}

func recoveredClaimFixture(t *testing.T) (*scenarios, restartReplay, []jobRecord) {
	t.Helper()
	raw, err := os.ReadFile("testdata/restart-recovered-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		restartReplay
		Jobs []jobRecord `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	s := &scenarios{in: inputs{namespace: "ptah-capacity-837dbe646e-a", namespaces: []string{"ptah-capacity-837dbe646e-a", "ptah-capacity-837dbe646e-b"}}, load: workload{Schemas: 10, Migrations: 10, Settle: duration{3 * time.Minute}}}
	return s, f.restartReplay, f.Jobs
}

func TestRestartConvergenceAcceptsRecoveredClaimWithNewJob(t *testing.T) {
	s, f, jobs := recoveredClaimFixture(t)
	_, missing, err := s.restartConvergence(f.Histories, f.Start, f.Deadline, nil)
	if err != nil || len(missing) != 1 || missing[0] != "schema/ptah-capacity-837dbe646e-b/capacity-schema-007" {
		t.Fatalf("fixture no longer reproduces the claim-time false negative: %v %v", missing, err)
	}
	at, missing, err := s.restartConvergence(f.Histories, f.Start, f.Deadline, jobs)
	if err != nil || len(missing) != 0 || !at.After(f.Start) || at.After(f.Deadline) {
		t.Fatalf("post-fault Job rejected: %s %v %v", at, missing, err)
	}
	if want := 177*time.Second + 950996257*time.Nanosecond; at.Sub(f.Start) != want {
		t.Fatalf("replayed recovery = %s, want %s", at.Sub(f.Start), want)
	}
	t.Logf("all twenty resources recovered in %s", at.Sub(f.Start))
	for _, h := range f.Histories {
		s.recorders = append(s.recorders, &cycleRecorder{history: h})
	}
	s.restartJobs = func() []jobRecord { return jobs }
	got, err := s.waitRestartConverged(t.Context(), f.Start)
	if err != nil || got != at.Sub(f.Start).String() {
		t.Fatalf("live wait did not use retained Job identities: %q %v", got, err)
	}
}

func TestRestartConvergenceRefusesUnprovenRecoveredJob(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "UID", "name", "namespace", "family", "resource", "operation", "old creation", "fault second", "future creation", "unfinished", "failed", "finish before creation", "future finish"} {
		t.Run(name, func(t *testing.T) {
			s, f, jobs := recoveredClaimFixture(t)
			found := false
			for i := range jobs {
				j := &jobs[i]
				if j.UID != "fab529d0-bb03-4de0-b83e-496692ba11fb" {
					continue
				}
				found = true
				switch name {
				case "missing":
					jobs = append(jobs[:i], jobs[i+1:]...)
				case "duplicate":
					jobs = append(jobs, *j)
				case "UID":
					j.UID = "different"
				case "name":
					j.Name = "different"
				case "namespace":
					j.Namespace = "different"
				case "family":
					j.Family = "migration"
				case "resource":
					j.Resource = "capacity-schema-008"
				case "operation":
					j.Operation = "observe"
				case "old creation":
					j.Created = f.Start.Add(-time.Second)
				case "fault second":
					j.Created = f.Start.Truncate(time.Second)
				case "future creation":
					j.Created = f.Deadline.Add(time.Second)
				case "unfinished":
					j.Finished = nil
				case "failed":
					j.Failed = true
				case "finish before creation":
					at := j.Created.Add(-time.Second)
					j.Finished = &at
				case "future finish":
					at := f.Deadline.Add(time.Second)
					j.Finished = &at
				}
				break
			}
			if !found {
				t.Fatal("fixture lost the recovered Job")
			}
			_, missing, err := s.restartConvergence(f.Histories, f.Start, f.Deadline, jobs)
			if err == nil && len(missing) == 0 {
				t.Fatal("unproven recovered Job passed")
			}
		})
	}
}
