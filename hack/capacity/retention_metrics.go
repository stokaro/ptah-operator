package main

import (
	"context"
	"fmt"
	"math"
	"time"
)

func validateMaintenanceMetrics(reading sample) error {
	if reading.At.IsZero() || reading.At.After(time.Now().UTC()) {
		return fmt.Errorf("maintenance sample has an invalid timestamp")
	}
	if len(reading.Incomplete) > 0 || len(reading.Managers) != 2 || len(reading.APIServers) != 3 {
		return fmt.Errorf("maintenance lacks a complete two-manager, three-API-server sample")
	}
	seen := map[string]bool{}
	for name, manager := range reading.Managers {
		if seen[manager.PodUID] {
			return fmt.Errorf("maintenance repeats a manager process")
		}
		seen[manager.PodUID] = true
		if _, valid := manager.identity(); !valid {
			return fmt.Errorf("maintenance manager %s lacks process identity", name)
		}
		if manager.RSSBytes <= 0 || math.IsNaN(manager.RSSBytes) || math.IsInf(manager.RSSBytes, 0) || manager.RSSBytes > 192*1024*1024 {
			return fmt.Errorf("maintenance manager %s exceeds the 192 MiB RSS bound or has no measurement", name)
		}
	}
	apiUIDs := map[string]bool{}
	for name, server := range reading.APIServers {
		if name != server.Node || !validAPIIdentity(server.apiIdentity) || apiUIDs[server.PodUID] || server.ScrapeStartedAt.IsZero() || server.ScrapeCompletedAt.Before(server.ScrapeStartedAt) || server.ScrapeCompletedAt.After(time.Now().UTC()) {
			return fmt.Errorf("maintenance lacks distinct, complete API server identities")
		}
		apiUIDs[server.PodUID] = true
	}
	return nil
}

func (s *scenarios) maintenanceMetrics(ctx context.Context, after time.Time) (sample, error) {
	if s.sampleSnapshot == nil {
		return sample{}, fmt.Errorf("maintenance metric collector is missing")
	}
	for {
		for _, reading := range s.sampleSnapshot() {
			if reading.At.Before(after) {
				continue
			}
			return reading, validateMaintenanceMetrics(reading)
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return sample{}, fmt.Errorf("no post-maintenance metric sample: %w", err)
		}
	}
}

func (s *scenarios) validateRetentionPlateau() error {
	if len(s.retentionProofs) != s.load.Soak.Rounds {
		return fmt.Errorf("maintenance did not record every round")
	}
	for i, p := range s.retentionProofs {
		if p.Round != i+1 || p.Error != "" || !p.Resumed || p.Metrics == nil || len(p.Deleted) == 0 || p.QuietAt.IsZero() || !p.FinishedAt.After(p.QuietAt) || p.RetainedPlans == 0 || len(p.PinsBefore) == 0 || len(p.PinsAfter) == 0 || !p.Metrics.At.After(p.QuietAt) || p.Metrics.At.After(p.FinishedAt) {
			return fmt.Errorf("incomplete maintenance round %d", i+1)
		}
		for _, d := range p.Deleted {
			if !d.GarbageCollected {
				return fmt.Errorf("maintenance did not prove garbage collection")
			}
		}
		if err := validateMaintenanceMetrics(*p.Metrics); err != nil {
			return err
		}
		if p.ChunkBytes < 0 || p.ProjectionBytes < 0 || p.ChunkBytes+p.ProjectionBytes > 128*1024*1024 {
			return fmt.Errorf("maintenance payload budget failed")
		}
	}
	if len(s.retentionProofs) < 3 {
		return nil
	} // Short diagnostics cannot establish the three-checkpoint plateau.
	last := s.retentionProofs[len(s.retentionProofs)-3:]
	if s.sampleSnapshot == nil {
		return fmt.Errorf("plateau lacks its continuous manager samples")
	}
	var samples []sample
	for _, reading := range s.sampleSnapshot() {
		if !reading.At.Before(last[0].Metrics.At) && !reading.At.After(last[2].Metrics.At) {
			samples = append(samples, reading)
		}
	}
	if problems := managerCounterContinuity(samples); len(problems) > 0 {
		return fmt.Errorf("plateau has incomplete process continuity: %v", problems)
	}
	for i, p := range last[1:] {
		if p.ChunkBytes+p.ProjectionBytes > last[i].ChunkBytes+last[i].ProjectionBytes {
			return fmt.Errorf("retained payload grew across the final three checkpoints")
		}
		for name, baseline := range last[0].Metrics.Managers {
			current, exists := p.Metrics.Managers[name]
			before, _ := baseline.identity()
			after, _ := current.identity()
			if !exists || before != after || current.RSSBytes > baseline.RSSBytes*1.1 {
				return fmt.Errorf("maintenance manager %s changed identity or grew more than 10%%", name)
			}
		}
	}
	return nil
}
