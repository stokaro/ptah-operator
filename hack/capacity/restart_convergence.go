package main

import (
	"context"
	"fmt"
	"sort"
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
		completed, missing, err := s.restartConvergence(histories, start, deadline)
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
func (s *scenarios) restartConvergence(histories []cycleHistory, start, deadline time.Time) (time.Time, []string, error) {
	if start.IsZero() || !deadline.After(start) || s.load.Schemas+s.load.Migrations == 0 {
		return time.Time{}, nil, fmt.Errorf("restart convergence requires a nonempty workload and a bounded fault window")
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
			// A fresh status timestamp alone may acknowledge work dispatched
			// before the fault. Plan performs two live database reads; History
			// reads the migration ledger. Require the corresponding bound Job
			// to have been claimed after manager deletion.
			if r.Unsafe {
				state.read = nil
			}
			if op := r.Operation; op != nil {
				state.read = nil
				want := "Plan"
				if h.Family == "migration" {
					want = "History"
				}
				if !r.Unsafe && r.ObservedGeneration == r.Generation && op.Type == want && op.ID != "" && op.JobName != "" && op.JobUID != "" && op.StartedAt.After(start) && !op.StartedAt.After(r.ReceivedAt) {
					state.read = op
				}
			}
			if state.first.IsZero() && state.read != nil && r.Event != "INITIAL" && cycleReady(h.Family, r) && r.CompletedAt.After(state.read.StartedAt) && !r.CompletedAt.After(r.ReceivedAt) {
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
