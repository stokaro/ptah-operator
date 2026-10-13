package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Sweep the fleet before waiting. Waiting for each resource in name order can
// keep an early resource suspended for several later resources' full cycles.
// Requests remain sequential, and every write still exports and binds UID/RV.
func (s *scenarios) suspendMaintenance(ctx context.Context, round int) ([]pausedResource, []retentionArchive, error) {
	type pendingResource struct {
		family         string
		index, attempt int
	}
	var pending []pendingResource
	for _, family := range []struct {
		name  string
		count int
	}{{"schema", s.load.Schemas}, {"migration", s.load.Migrations}} {
		for index := range family.count {
			pending = append(pending, pendingResource{family: family.name, index: index})
		}
	}
	var paused []pausedResource
	var archives []retentionArchive
	directory := filepath.Join(s.evidenceDir, "retention", fmt.Sprintf("round-%02d", round))
	for len(pending) > 0 {
		var remaining []pendingResource
		for _, next := range pending {
			resource, archive, err := s.tryExportAndSuspendTo(ctx, next.family, next.index, round, next.attempt, directory)
			if resource.target.uid != "" {
				paused = append(paused, resource)
			}
			if archive.Path != "" {
				archives = append(archives, archive)
			}
			if errors.Is(err, errMaintenancePending) || apierrors.IsConflict(err) {
				next.attempt++
				remaining = append(remaining, next)
			} else if err != nil {
				return paused, archives, err
			}
		}
		pending = remaining
		if len(pending) > 0 {
			if err := waitCapacityPoll(ctx); err != nil {
				return paused, archives, fmt.Errorf("maintenance has %d resources still waiting for suspension: %w", len(pending), err)
			}
		}
	}
	return paused, archives, nil
}
