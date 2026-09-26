package runner

import (
	"errors"
	"strconv"
	"time"
)

// DefaultTerminationGracePeriod is how long an operation Pod is given between
// SIGTERM and SIGKILL, unless a claim recorded a grace of its own.
//
// It is one number with three readers. The workload builders write it into
// every operation Pod's terminationGracePeriodSeconds, the schema controller
// records it on an Apply claim and dates the end of that Apply's execution
// horizon by it, and the runner derives from it how long a child it asked to
// stop may take. A mutating Pod is also told its grace through
// EnvTerminationGracePeriod, so the runner never has to assume that the value
// a claim recorded is this one.
const DefaultTerminationGracePeriod = 30 * time.Second

// MaxTerminationGracePeriod is the longest grace a claim may record, which is
// the bound the API puts on the field that records it.
const MaxTerminationGracePeriod = 300 * time.Second

// ChildStopDelay is how long a child may take to stop after the runner sent
// it SIGTERM, for a Pod given grace between SIGTERM and SIGKILL.
//
// The runner is PID 1 in its container, so the kubelet's SIGTERM reaches the
// runner and not the child. The runner passes it on and waits this long; a
// child still running then is killed. What is left of the grace after that is
// the runner's own: decoding what the child wrote, writing the termination
// summary, and writing the result frame. A third of the grace is kept for that,
// which is ten seconds of the default and far more than a frame takes.
//
// The same delay applies when the child's own execution deadline passes, and
// it is why that is safe: the schema controller does not read the database
// before executionNotAfter plus the recorded grace, and a child that has to be
// dead by executionNotAfter plus two thirds of that grace is dead before then.
func ChildStopDelay(grace time.Duration) time.Duration {
	if grace <= 0 {
		return 0
	}
	return grace - grace/3
}

// terminationGracePeriod returns the grace this Pod was given.
//
// A mutating Pod has to name it. The schema controller dates the moment it may
// read the database after an Apply by the grace the claim recorded, so a runner
// that assumed the default while its Pod carried less could leave a child
// running past that moment. A mutating Pod that does not say is refused before
// the child starts, as a Pod that does not name its deadlines is.
//
// A read-only Pod is built with the default and changes nothing, so a child it
// could not stop in time costs a retry and nothing else.
func terminationGracePeriod(values map[string]string, operation Operation) (time.Duration, error) {
	value, present := values[envTerminationGracePeriod]
	if !present || value == "" {
		if operation.Mutating() {
			return 0, errors.New("the Pod's termination grace period is required")
		}
		return DefaultTerminationGracePeriod, nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > MaxTerminationGracePeriod {
		return 0, errors.New("the Pod's termination grace period must be a whole number of seconds between 1 and 300")
	}
	return time.Duration(seconds) * time.Second, nil
}
