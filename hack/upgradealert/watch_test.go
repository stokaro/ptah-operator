package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestWatchPersistsFailureAndCursorBeforeRestart(t *testing.T) {
	s := fixtureState()
	o := &observer{state: s, path: filepath.Join(t.TempDir(), "state.json")}
	stream := watch.NewRaceFreeFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- o.segment(ctx, stream) }()
	job := fixtureJob(s, "failure", time.Second)
	terminateJob(job, batchv1.JobFailed, s.StartedAt.Add(time.Minute))
	job.ResourceVersion = "102"
	stream.Modify(job)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for o.snapshot().ResourceVersion != "102" {
		select {
		case <-tick.C:
		case err := <-done:
			t.Fatalf("watch ended: %v", err)
		case <-deadline.C:
			t.Fatal("watch did not retain its event")
		}
	}
	stream.Stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	restored, err := loadState(o.path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.FailedAt == nil || restored.ResourceVersion != "102" || len(restored.Attempts) != 1 {
		t.Fatal("restart lost failure or cursor")
	}
	recorder := httptest.NewRecorder()
	o.metrics(recorder, httptest.NewRequest("GET", "/metrics", nil))
	text := recorder.Body.String()
	if !strings.Contains(text, `ptah_operator_upgrade_failed{operator_namespace="operator",release="ptah"} 1`) {
		t.Fatal(text)
	}
	for _, forbidden := range []string{"failure", s.Intent.Image, "sha256:", "probe-uid"} {
		if strings.Contains(text, forbidden) {
			t.Fatal("unbounded identity reached metric labels")
		}
	}
}

func TestCompactionIsDurableAndDoesNotRelist(t *testing.T) {
	o := &observer{state: fixtureState(), path: filepath.Join(t.TempDir(), "state.json"), watching: true}
	stream := watch.NewRaceFreeFake()
	stream.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired})
	if err := o.segment(context.Background(), stream); err != nil {
		t.Fatal(err)
	}
	restored, err := loadState(o.path)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.HistoryLost || restored.ResourceVersion != "100" {
		t.Fatal("compaction silently replaced the history boundary")
	}
	recorder := httptest.NewRecorder()
	o.ready(recorder, httptest.NewRequest("GET", "/readyz", nil))
	if recorder.Code != 503 {
		t.Fatal("compacted watch reported readiness")
	}
}

func TestWriteFailureDoesNotAdvancePublishedCursor(t *testing.T) {
	o := &observer{state: fixtureState(), path: filepath.Join(t.TempDir(), "missing", "state.json")}
	s := o.snapshot()
	s.ResourceVersion = "101"
	if err := o.persist(s); err == nil {
		t.Fatal("missing evidence directory accepted")
	}
	if o.snapshot().ResourceVersion != "100" {
		t.Fatal("unretained cursor was published")
	}
}

func TestInvalidReplacementDoesNotDestroyDurableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := fixtureState()
	if err := saveState(path, s); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Deadline = s.Deadline.Add(time.Minute)
	if err := saveState(path, s); err == nil {
		t.Fatal("extended deadline saved")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("invalid replacement destroyed original evidence")
	}
}

// HTTP reachability does not prove that Kubernetes still permits observation.
func TestWatchAuthorizationLossIsVisibleWhileMetricsRemainReachable(t *testing.T) {
	jobs := fake.NewClientset()
	var permit atomic.Bool
	attempts := make(chan struct{}, 10)
	stream := watch.NewRaceFreeFake()
	defer stream.Stop()
	jobs.PrependWatchReactor("jobs", func(action ktesting.Action) (bool, watch.Interface, error) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		if !permit.Load() {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "ptah-crd-manager", fmt.Errorf("observation permission revoked"))
		}
		return true, stream, nil
	})
	o := &observer{state: fixtureState(), path: filepath.Join(t.TempDir(), "state.json"), jobs: jobs}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.watch(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watch did not stop")
		}
	}()
	select {
	case <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatal("watch was not attempted")
	}
	assertReady := func(want int) {
		t.Helper()
		metrics := httptest.NewRecorder()
		o.metrics(metrics, httptest.NewRequest("GET", "/metrics", nil))
		expected := fmt.Sprintf(`ptah_operator_upgrade_observer_ready{operator_namespace="operator",release="ptah"} %d`, want)
		if metrics.Code != 200 || !strings.Contains(metrics.Body.String(), expected) {
			t.Fatalf("reachable metrics did not report observation readiness %d: %s", want, metrics.Body.String())
		}
		ready := httptest.NewRecorder()
		o.ready(ready, httptest.NewRequest("GET", "/readyz", nil))
		if (ready.Code == 200) != (want == 1) {
			t.Fatalf("readiness HTTP status differs: %d", ready.Code)
		}
	}
	assertReady(0)
	permit.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.mu.RLock()
		watching := o.watching
		o.mu.RUnlock()
		if watching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watch did not recover after authorization returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertReady(1)
	s := o.snapshot()
	s.HistoryLost = true
	if err := o.persist(s); err != nil {
		t.Fatal(err)
	}
	assertReady(0)
}
