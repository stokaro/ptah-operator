package e2e

import (
	"testing"
	"time"
)

func TestLogReadBoundUsesRequestTimes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		readings []logStallReading
		accepted bool
	}{
		{"one bounded read", []logStallReading{{State: "started", At: start}, {State: "canceled", At: start.Add(60 * time.Second)}}, true},
		{"no request", nil, false},
		{"only listening", []logStallReading{{State: "listening", At: start}}, false},
		{"read still stuck", []logStallReading{{State: "started", At: start}}, false},
		{"immediate failure", []logStallReading{{State: "started", At: start}, {State: "canceled", At: start.Add(time.Second)}}, false},
		{"late cancellation", []logStallReading{{State: "started", At: start}, {State: "canceled", At: start.Add(76 * time.Second)}}, false},
		{"second read cannot hide an unbounded first", []logStallReading{{State: "started", At: start}, {State: "started", At: start.Add(50 * time.Second)}, {State: "canceled", At: start.Add(110 * time.Second)}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if duration, accepted := logReadDuration(test.readings); accepted != test.accepted {
				t.Fatalf("duration=%s accepted=%t, want %t", duration, accepted, test.accepted)
			}
		})
	}
}
