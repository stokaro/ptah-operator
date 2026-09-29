package e2e

import "time"

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
