package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// leaseReleaseFloor is the least the Pod's grace period has to leave after the
// manager's graceful shutdown: the leader Lease is released after every
// runnable has stopped, and the kubelet's kill must not race it. 0.2.2 left
// ten seconds, and the release landed.
const leaseReleaseFloor = 10 * time.Second

// A stopping manager finishes a dispatch it began, so its graceful shutdown is
// mutationlifecycle.DispatchStopBound, and the Pod's grace period has to cover
// that shutdown and the Lease release after it. The row reads the options the
// manager is built with, not the constant, so a shutdown set to anything else
// fails it.
func TestTheManagerPodOutlastsADispatchItStops(t *testing.T) {
	t.Parallel()

	options := withStopTiming(ctrl.Options{})
	if options.GracefulShutdownTimeout == nil {
		t.Fatal("the manager sets no graceful shutdown, so controller-runtime's 30-second default cuts a dispatch it stops")
	}
	shutdown := *options.GracefulShutdownTimeout
	if shutdown != mutationlifecycle.DispatchStopBound {
		t.Fatalf("the manager's graceful shutdown is %s, want DispatchStopBound %s", shutdown, mutationlifecycle.DispatchStopBound)
	}
	if !options.LeaderElectionReleaseOnCancel {
		t.Fatal("the manager does not release the Lease when it stops, so the grace left for the release covers nothing")
	}
	grace := managerTerminationGrace(t)
	if slack := grace - shutdown; slack < leaseReleaseFloor {
		t.Fatalf("the manager Pod's grace period %s leaves %s after a %s shutdown, want at least %s for the Lease release",
			grace, slack, shutdown, leaseReleaseFloor)
	}
}

var podGraceLine = regexp.MustCompile(`(?m)^      terminationGracePeriodSeconds: ([0-9]+)$`)

// managerTerminationGrace is the grace period the chart gives the manager Pod.
func managerTerminationGrace(t *testing.T) time.Duration {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator", "templates", "deployment.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // The path is the repository's own chart.
	if err != nil {
		t.Fatalf("read the manager Deployment template: %v", err)
	}
	matches := podGraceLine.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		t.Fatalf("the manager Deployment template sets terminationGracePeriodSeconds %d times, want once", len(matches))
	}
	seconds, err := strconv.Atoi(string(matches[0][1]))
	if err != nil {
		t.Fatalf("parse the manager Pod's terminationGracePeriodSeconds: %v", err)
	}
	return time.Duration(seconds) * time.Second
}
