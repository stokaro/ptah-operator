package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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
