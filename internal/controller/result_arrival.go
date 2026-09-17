package controller

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// A finished Job's log can be read before the container runtime has copied the
// runner's last output into it, and the result frame is that last output. A read
// in that window finds the frame cut off or not there, which reads exactly like a
// runner that failed to say what it did -- and for an Apply that is an outcome the
// controller has to call unknown (#154).
//
// So a frame that may still be arriving is read again, for a bounded window after
// the Job finished. The window is measured from the Job's own terminal condition,
// not from anything this controller stores, so every pass recomputes it and a
// manager restart neither resets nor extends it. Past the window the failure is
// judged the way it always was.
const (
	frameArrivalWindow = 30 * time.Second
	frameArrivalPoll   = 2 * time.Second
)

// awaitFrameArrival answers whether a parse failure should be read again, and
// how soon. A failure that describes wrong bytes, a Job with no recorded finish,
// and a Job that finished longer ago than the window are all judged now.
func awaitFrameArrival(job *batchv1.Job, parseErr error, now time.Time) (time.Duration, bool) {
	if parseErr == nil || !runner.MayStillArrive(parseErr) {
		return 0, false
	}
	finished, ok := jobFinishedAt(job)
	if !ok || now.Sub(finished) >= frameArrivalWindow {
		return 0, false
	}
	return frameArrivalPoll, true
}

// jobFinishedAt is when a Job's terminal condition became true.
func jobFinishedAt(job *batchv1.Job) (time.Time, bool) {
	if job == nil {
		return time.Time{}, false
	}
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) &&
			condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() {
			return condition.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}
