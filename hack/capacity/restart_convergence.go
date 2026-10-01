package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Restart recovery is the first fresh, accepted convergence of each original
// resource. Normal refreshes need not leave the whole fleet idle at once.
// Use the continuous watch so that approval recovery cannot hide an earlier
// fleet completion, and measure from manager deletion, not from this wait.
func (s *scenarios) waitRestartConverged(ctx context.Context, start time.Time) (string, error) {
	deadline := start.Add(s.load.Settle.Duration)
	for {
		var histories []cycleHistory
		for _, recorder := range s.recorders {
			histories = append(histories, recorder.snapshot())
		}
		var jobs []jobRecord
		if s.restartJobs != nil {
			jobs = s.restartJobs()
		}
		completed, missing, err := s.restartConvergence(histories, start, deadline, jobs)
		if err != nil {
			return "", err
		}
		if len(missing) == 0 {
			return completed.Sub(start).String(), nil
		}
		if !time.Now().Before(deadline) {
			return "not within " + s.load.Settle.String(), fmt.Errorf("restart did not recover within %s: %v", s.load.Settle, missing)
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return "", err
		}
	}
}

// Replay the same evidence used by the live wait. A replacement or spec change
// cannot stand in for recovery of the workload that existed at the fault.
func (s *scenarios) restartConvergence(histories []cycleHistory, start, deadline time.Time, jobs []jobRecord) (time.Time, []string, error) {
	if start.IsZero() || !deadline.After(start) || s.load.Schemas+s.load.Migrations == 0 {
		return time.Time{}, nil, fmt.Errorf("restart convergence requires a nonempty workload and a bounded fault window")
	}
	jobByUID := make(map[string]jobRecord, len(jobs))
	for _, job := range jobs {
		if job.UID == "" {
			return time.Time{}, nil, fmt.Errorf("restart Job evidence lacks a UID")
		}
		if _, exists := jobByUID[job.UID]; exists {
			return time.Time{}, nil, fmt.Errorf("restart Job evidence repeats UID %s", job.UID)
		}
		jobByUID[job.UID] = job
	}
	type slot struct {
		before cycleReading
		first  time.Time
		read   *cycleOperation
	}
	wanted := map[string]*slot{}
	for i := range s.load.Schemas {
		wanted["schema/"+s.in.namespaceFor(i)+"/"+s.schemaName(i)] = &slot{}
	}
	for i := range s.load.Migrations {
		wanted["migration/"+s.in.namespaceFor(i)+"/"+s.migrationName(i)] = &slot{}
	}
	streams := map[string]bool{}
	for _, h := range histories {
		stream := h.Family + "/" + h.Namespace
		if streams[stream] || h.Error != "" || h.StartedAt.IsZero() || h.StartedAt.After(start) {
			return time.Time{}, nil, fmt.Errorf("restart has incomplete or duplicate watch history for %s: %s", stream, h.Error)
		}
		streams[stream] = true
		var previous time.Time
		for _, r := range h.Readings {
			if r.ReceivedAt.Before(previous) || r.Namespace != h.Namespace {
				return time.Time{}, nil, fmt.Errorf("restart watch history is unordered or crossed namespace: %s", stream)
			}
			previous = r.ReceivedAt
			if r.ReceivedAt.After(deadline) {
				break
			}
			key := stream + "/" + r.Name
			state, ok := wanted[key]
			if !ok {
				return time.Time{}, nil, fmt.Errorf("unexpected restart workload resource %s", key)
			}
			if r.UID == "" || r.Generation < 1 || r.ResourceVersion == "" {
				return time.Time{}, nil, fmt.Errorf("restart reading lacks resource identity: %s", key)
			}
			if r.ReceivedAt.Before(start) {
				state.before = r
				continue
			}
			if state.before.UID == "" || state.before.Event == "DELETED" || r.Event == "DELETED" || r.UID != state.before.UID || r.Generation != state.before.Generation {
				return time.Time{}, nil, fmt.Errorf("restart workload identity changed or lacks a baseline: %s", key)
			}
			// Plan and History read the database. A claim can survive manager
			// deletion before its Job is created, so retain the binding even
			// when the claim predates the fault. Freshness is decided against
			// both the claim and the exact Job at accepted convergence.
			if r.Unsafe {
				state.read = nil
			}
			if op := r.Operation; op != nil {
				state.read = nil
				want := "Plan"
				if h.Family == "migration" {
					want = "History"
				}
				if !r.Unsafe && r.ObservedGeneration == r.Generation && op.Type == want && op.ID != "" && op.JobName != "" && op.JobUID != "" && !op.StartedAt.IsZero() && !op.StartedAt.After(r.ReceivedAt) {
					state.read = op
				}
			}
			if state.first.IsZero() && state.read != nil && r.Event != "INITIAL" && cycleReady(h.Family, r) && r.CompletedAt.After(state.read.StartedAt) && !r.CompletedAt.After(r.ReceivedAt) && freshRestartRead(h.Family, r, *state.read, start, jobByUID) {
				state.first = r.ReceivedAt
			}
		}
	}
	var completed time.Time
	var missing []string
	for name, state := range wanted {
		if state.first.IsZero() {
			missing = append(missing, name)
		} else if state.first.After(completed) {
			completed = state.first
		}
	}
	sort.Strings(missing)
	return completed, missing, nil
}

// A new claim proves freshness by itself. For a claim recovered across manager
// replacement, only creation of its exact Job after the fault can supply that
// bound: Job start/finish and status update times alone cannot date its SQL.
func freshRestartRead(family string, reading cycleReading, op cycleOperation, fault time.Time, jobs map[string]jobRecord) bool {
	if op.StartedAt.After(fault) {
		return true
	}
	job, ok := jobs[op.JobUID]
	return ok && job.UID == op.JobUID && job.Namespace == reading.Namespace && job.Name == op.JobName &&
		job.Family == family && job.Resource == reading.Name && job.Operation == strings.ToLower(op.Type) &&
		job.Created.After(fault) && !job.Created.Before(op.StartedAt) &&
		reading.CompletedAt.After(job.Created) &&
		job.Finished != nil && !job.Failed && !job.Finished.Before(job.Created) &&
		!job.Finished.After(reading.CompletedAt) && !job.Finished.After(reading.ReceivedAt)
}
