package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func testHistogram(counts map[float64]float64, total, sum float64) histogram {
	return histogram{count: total, sum: sum, buckets: counts}
}

// A histogram knows no finer than its buckets, so a quantile is the bound of
// the bucket that holds it.
func TestAQuantileIsTheBoundOfTheBucketThatHoldsIt(t *testing.T) {
	h := testHistogram(map[float64]float64{0.1: 50, 0.5: 90, 1: 99, math.Inf(1): 100}, 100, 20)
	for q, want := range map[float64]float64{0.5: 0.1, 0.9: 0.5, 0.95: 1, 1: math.Inf(1)} {
		if got, ok := h.quantile(q); !ok || got != want {
			t.Errorf("quantile(%v) = %v, %v; want %v", q, got, ok, want)
		}
	}
	if _, ok := (histogram{}).quantile(0.5); ok {
		t.Error("an empty histogram produced a quantile")
	}
}

// A manager that restarted inside a window starts its counters from zero, and
// the growth is the new process's reading rather than a negative difference.
func TestGrowthAcrossARestartIsTheNewProcessesReading(t *testing.T) {
	if got := counterDelta(3, 10); got != 3 {
		t.Errorf("counterDelta after a reset = %v, want 3", got)
	}
	if got := counterDelta(15, 10); got != 5 {
		t.Errorf("counterDelta = %v, want 5", got)
	}
	earlier := testHistogram(map[float64]float64{1: 8, math.Inf(1): 10}, 10, 5)
	later := testHistogram(map[float64]float64{1: 2, math.Inf(1): 3}, 3, 1)
	if got := later.since(earlier); got.count != 3 || got.buckets[1] != 2 {
		t.Errorf("since after a reset = %+v, want the later reading whole", got)
	}
	grown := testHistogram(map[float64]float64{1: 12, math.Inf(1): 15}, 15, 9)
	if got := grown.since(earlier); got.count != 5 || got.buckets[1] != 4 || got.sum != 4 {
		t.Errorf("since = %+v, want count 5, first bucket 4, sum 4", got)
	}
}

// The window counts the Jobs created inside it and no others, and a manager
// replaced inside it is a second process whose CPU adds to the first's.
func TestAWindowCountsWhatHappenedInsideIt(t *testing.T) {
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }
	ptr := func(value time.Time) *time.Time { return &value }
	w := window{Name: "steady state", Start: at(0), End: at(120)}
	samples := []sample{
		{At: at(0), PodsPending: 1, Managers: map[string]managerReading{"old": {RSSBytes: 100, CPUSeconds: 10}}},
		{At: at(60), PodsPending: 4, Managers: map[string]managerReading{"old": {RSSBytes: 300, CPUSeconds: 40}}},
		{At: at(90), Managers: map[string]managerReading{"new": {RSSBytes: 50, CPUSeconds: 1}}},
		{At: at(120), Managers: map[string]managerReading{"new": {RSSBytes: 80, CPUSeconds: 31}}},
		{At: at(180), PodsPending: 99, Managers: map[string]managerReading{"new": {RSSBytes: 9999, CPUSeconds: 999}}},
	}
	jobs := []jobRecord{
		{Created: at(-30), Finished: ptr(at(10))},
		{Created: at(5), Started: ptr(at(7)), Finished: ptr(at(25))},
		{Created: at(10), Started: ptr(at(11)), Finished: ptr(at(40))},
		{Created: at(70), Started: ptr(at(90)), Finished: ptr(at(95)), Failed: true},
		{Created: at(200)},
	}
	got := cost(w, samples, jobs)
	if got.JobsCreated != 3 || got.JobsFailed != 1 {
		t.Errorf("jobs created %d failed %d, want 3 and 1", got.JobsCreated, got.JobsFailed)
	}
	if got.JobsPerMinutePeak != 2 {
		t.Errorf("peak jobs per minute = %d, want 2", got.JobsPerMinutePeak)
	}
	if got.JobCompletionSeconds.Count != 2 || got.JobCompletionSeconds.Max != 30 {
		t.Errorf("completion = %+v, want two completed Jobs, the slowest 30s", got.JobCompletionSeconds)
	}
	if got.PodsPendingMax != 4 || got.ManagerRSSMaxBytes != 300 {
		t.Errorf("pending %d rss %v, want 4 and 300: the sample after the window leaked in", got.PodsPendingMax, got.ManagerRSSMaxBytes)
	}
	// 30 CPU-seconds on the old process and 30 on the new one, over 120s.
	if math.Abs(got.ManagerCPUCores-0.5) > 1e-9 {
		t.Errorf("manager cores = %v, want 0.5", got.ManagerCPUCores)
	}
}

func TestAWorkloadThatRunsNothingIsRefused(t *testing.T) {
	good := workload{
		Name: "w", Schemas: 1, Migrations: 1, ChangeBatch: 1,
		Interval: duration{time.Minute}, Settle: duration{time.Minute},
		SteadyState: duration{time.Minute}, SampleEvery: duration{time.Second},
	}
	if err := good.validate(); err != nil {
		t.Fatalf("a valid workload was refused: %v", err)
	}
	for name, mutate := range map[string]func(*workload){
		"no resources":       func(w *workload) { w.Schemas, w.Migrations = 0, 0 },
		"a batch too large":  func(w *workload) { w.ChangeBatch = 2 },
		"no sampling period": func(w *workload) { w.SampleEvery = duration{} },
		"a negative outage":  func(w *workload) { w.Outage = duration{-time.Second} },
		"no name":            func(w *workload) { w.Name = "" },
	} {
		w := good
		mutate(&w)
		if err := w.validate(); err == nil {
			t.Errorf("%s: the workload was accepted", name)
		}
	}
}

func TestAPrefixMatchesOnlyWhereItIsAsked(t *testing.T) {
	families, err := parseScrape([]byte(strings.Join([]string{
		`# TYPE workqueue_depth gauge`,
		`workqueue_depth{name="ptahschema"} 3`,
		`workqueue_depth{name="ptahmigration"} 5`,
		`# TYPE rest_client_requests_total counter`,
		`rest_client_requests_total{code="429"} 2`,
		`rest_client_requests_total{code="200"} 40`,
		``,
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := families.value("rest_client_requests_total", map[string]string{"code": "429"}); got != 2 {
		t.Errorf("429s = %v, want 2", got)
	}
	if got, _ := families.value("workqueue_depth", map[string]string{"name": "ptah*"}); got != 8 {
		t.Errorf("depth under a prefix = %v, want 8", got)
	}
	if got := families.maxBy("workqueue_depth", "name"); got["ptahmigration"] != 5 || len(got) != 2 {
		t.Errorf("depth by queue = %v", got)
	}
}
