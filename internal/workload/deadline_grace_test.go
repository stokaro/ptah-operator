package workload

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The property the grace exists for, stated once: a mutating Job outlives the
// window that authorized it, so a Pod that starts late is stopped by the
// runner's refusal, which names itself in a frame, rather than by Kubernetes'
// DeadlineExceeded, which leaves no frame at all.
func TestAMutatingJobOutlivesTheWindowThatAuthorizedIt(t *testing.T) {
	t.Parallel()

	startedAt := metav1.NewTime(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	for _, window := range []time.Duration{30 * time.Second, 5 * time.Minute, time.Hour} {
		notAfter := metav1.NewTime(startedAt.Add(window))
		deadline, err := boundedDeadline(900, true, startedAt, &notAfter)
		if err != nil {
			t.Fatalf("window %s: %v", window, err)
		}
		if want := int64((window + JobDeadlineGrace) / time.Second); deadline != want {
			t.Errorf("window %s: Job deadline = %d, want %d", window, deadline, want)
		}
	}

	// And never past what the API accepts, which the typed Job guard also
	// enforces: a window at the ceiling plus the grace would be refused.
	ceiling := metav1.NewTime(startedAt.Add(time.Duration(maximumActiveDeadlineSeconds) * time.Second))
	deadline, err := boundedDeadline(900, true, startedAt, &ceiling)
	if err != nil {
		t.Fatalf("ceiling: %v", err)
	}
	if deadline != maximumActiveDeadlineSeconds {
		t.Errorf("at the ceiling the Job deadline = %d, want %d", deadline, maximumActiveDeadlineSeconds)
	}

	// A read-only operation is not bounded by a window and keeps its fallback.
	if deadline, err := boundedDeadline(900, false, startedAt, &ceiling); err != nil || deadline != 900 {
		t.Errorf("read-only deadline = %d, %v, want 900", deadline, err)
	}
}
