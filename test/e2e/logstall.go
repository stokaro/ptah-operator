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

// schemaReadProgress dates convergence by the persisted observation, with
// the independent approval as its lower bound. A reading from before the
// fault or after the progress deadline cannot satisfy the row.
func schemaReadProgress(schema *ptahv1alpha1.PtahSchema, approvedAt, deadline time.Time) bool {
	return freshApprovalConverged(schema) && schema.Status.Applied != nil &&
		!schema.Status.Applied.CompletedAt.Time.Before(approvedAt) &&
		!schema.Status.Applied.CompletedAt.Time.After(deadline)
}

// diagnosticLogHeld proves a real diagnostic request reached the injected
// endpoint and remains unfinished. Extra requests or a closed stream cannot
// stand in for independence from one continuously unavailable log.
func diagnosticLogHeld(readings []logStallReading) bool {
	started := 0
	for _, reading := range readings {
		switch reading.State {
		case "listening":
		case "started":
			started++
		default:
			return false
		}
	}
	return started == 1
}
