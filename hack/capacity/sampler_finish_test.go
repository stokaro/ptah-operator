package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func finishJob(name string, complete bool) map[string]any {
	status := map[string]any{"startTime": "2026-10-01T00:00:01Z"}
	if complete {
		status["conditions"] = []any{map[string]any{
			"type": "Complete", "status": "True", "lastTransitionTime": "2026-10-01T00:00:04Z",
		}}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": "work", "uid": name + "-uid", "creationTimestamp": "2026-10-01T00:00:00Z",
			"labels": map[string]any{"operator.ptah.run/operation": "Plan", "operator.ptah.run/schema": "capacity-schema-000"},
		},
		"status": status,
	}
}

// The final API reading must find both a Job that completed after the prior
// reading and a Job that did not exist in that reading. Ending the scenario
// halfway through collection must not cancel either reading.
func TestSamplerFinishesInflightReadAndCollectsFinalJobs(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	var reads atomic.Int32
	s := measurementFixture(t, "", completeProcessMetrics, func(path string, list map[string]any) {
		if path != "/apis/batch/v1/namespaces/work/jobs" {
			return
		}
		if reads.Add(1) == 1 {
			close(entered)
			<-release
			list["items"] = []any{finishJob("already-running", false)}
		} else {
			list["items"] = []any{finishJob("already-running", true), finishJob("created-after-last-sample", true)}
		}
	})
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	s.every = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	finish, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- s.run(ctx, finish) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("initial collection did not reach the Job API")
	}
	close(finish)
	unblock.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("sampler did not finish")
	}
	samples, jobs := s.snapshot()
	if reads.Load() != 2 || len(samples) != 2 || len(jobs) != 2 {
		t.Fatalf("final collection missing: reads=%d samples=%d jobs=%+v", reads.Load(), len(samples), jobs)
	}
	for _, sample := range samples {
		if len(sample.Incomplete) != 0 {
			t.Fatalf("normal completion canceled a read: %+v", sample.Incomplete)
		}
	}
	for _, job := range jobs {
		if job.UID != job.Name+"-uid" || job.Namespace != "work" || job.Finished == nil || job.Failed ||
			job.Finished.Format(time.RFC3339) != "2026-10-01T00:00:04Z" {
			t.Errorf("lost the API's terminal Job evidence: %+v", job)
		}
	}
}

func TestSamplerFailsWhenFinalInventoryCannotBeRead(t *testing.T) {
	s := measurementFixture(t, "/apis/batch/v1/namespaces/work/jobs", completeProcessMetrics)
	s.every = time.Hour
	finish := make(chan struct{})
	close(finish)
	if err := s.run(t.Context(), finish); err == nil {
		t.Fatal("a failed final Job read became a successful run")
	}
	samples, _ := s.snapshot()
	if len(samples) != 1 || !slices.Contains(samples[0].Incomplete, sourceJobs) {
		t.Fatalf("failed final read disappeared: %+v", samples)
	}
}

func TestSamplerBoundsFinalCollectionAndRetainsItsFailure(t *testing.T) {
	s := measurementFixture(t, "", completeProcessMetrics)
	s.every, s.collectionTimeout = time.Hour, time.Second
	var entered atomic.Bool
	s.scrapeAPI = func(ctx context.Context, _ corev1.Pod) (scrape, error) {
		entered.Store(true)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	finish := make(chan struct{})
	close(finish)
	started := time.Now()
	err := s.run(t.Context(), finish)
	if !entered.Load() || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 5*time.Second {
		t.Fatalf("final collection was not bounded: elapsed=%s error=%v", time.Since(started), err)
	}
	samples, _ := s.snapshot()
	if len(samples) != 1 || !slices.Contains(samples[0].Incomplete, sourceAPI) || !slices.Contains(samples[0].Incomplete, sourceJobs) {
		t.Fatalf("timed-out collection lost its missing sources: %+v", samples)
	}
}
