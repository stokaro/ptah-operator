package e2e

import (
	"time"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

type logStallReading struct {
	State string    `json:"state"`
	Path  string    `json:"path"`
	At    time.Time `json:"at"`
}

// logReadDuration uses the fixture's request timestamps, not when the
// harness happened to notice them. The first read must have actually hung.
func logReadDuration(readings []logStallReading) (time.Duration, bool) {
	var started time.Time
	for _, reading := range readings {
		if reading.State == "started" && started.IsZero() {
			started = reading.At
		}
		if reading.State == "canceled" && !started.IsZero() {
			duration := reading.At.Sub(started)
			return duration, duration >= 50*time.Second && duration <= 75*time.Second
		}
	}
	return 0, false
}

// schemaReadProgress dates convergence by the persisted observation, with
// the independent approval as its lower bound. A reading from before the
// fault or after the progress deadline cannot satisfy the row.
func schemaReadProgress(schema *ptahv1alpha1.PtahSchema, approvedAt, deadline time.Time) bool {
	return freshApprovalConverged(schema) && schema.Status.Applied != nil &&
		!schema.Status.Applied.CompletedAt.Time.Before(approvedAt) &&
		!schema.Status.Applied.CompletedAt.Time.After(deadline)
}
