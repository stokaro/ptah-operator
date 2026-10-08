package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

func requestSamples() (window, []sample) {
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	w := window{Name: "steady", Start: base, End: base.Add(2 * time.Minute)}
	var samples []sample
	for i := -1; i <= 25; i++ {
		at := base.Add(time.Duration(i) * 5 * time.Second)
		row := sample{At: at, Managers: map[string]managerReading{}}
		for _, name := range []string{"first", "second"} {
			row.Managers[name] = identifiedManager(name, managerReading{
				APIRequests: &apiRequestReading{Total: float64(i+1) * 5, StartedAt: at, FinishedAt: at.Add(time.Millisecond)},
			})
		}
		samples = append(samples, row)
	}
	return w, samples
}

func TestAPIDemandBoundsEverySlidingMinuteAcrossBothManagers(t *testing.T) {
	w, samples := requestSamples()
	// Each replica issues a 3,500-request burst. A whole-scenario average or
	// either replica alone would pass 100/s, but their shared minute cannot.
	for i := range samples {
		// Replicas are scraped sequentially. The later read cannot serve as a
		// baseline for a window that already started during the earlier read.
		second := samples[i].Managers["second"].APIRequests
		second.StartedAt = second.StartedAt.Add(time.Second)
		second.FinishedAt = second.FinishedAt.Add(2 * time.Second)
		if samples[i].At.Sub(w.Start) >= 50*time.Second {
			for _, manager := range samples[i].Managers {
				manager.APIRequests.Total += 3500
			}
		}
	}
	got, problems := managerAPIDemand(w, samples)
	if len(problems) > 0 || got == nil || got.MaxPerSecondUpper < 7000.0/60 || len(got.Windows) < 2 {
		t.Fatalf("burst or replica was lost: %+v %v", got, problems)
	}
	for _, bound := range got.Windows {
		before, after := -1, -1
		for i, s := range samples {
			first, last, _ := requestReadBounds(s)
			if first.Equal(bound.CounterFrom) {
				before = i
			}
			if last.Equal(bound.CounterThrough) {
				after = i
			}
		}
		if before < 0 || after <= before {
			t.Fatal("bound does not name its source readings", bound)
		}
		for name, manager := range samples[before].Managers {
			if manager.APIRequests.FinishedAt.After(bound.StartFrom) || samples[after].Managers[name].APIRequests.StartedAt.Before(bound.StartThrough.Add(time.Minute)) {
				t.Fatal("a replica's scrape does not enclose every covered minute", name, bound)
			}
		}
	}
	for start := w.Start; !start.After(w.End.Add(-time.Minute)); start = start.Add(500 * time.Millisecond) {
		covered := false
		for _, bound := range got.Windows {
			if start.Before(bound.StartFrom) || start.After(bound.StartThrough) {
				continue
			}
			covered = true
			if bound.CounterFrom.After(start) || bound.CounterThrough.Before(start.Add(time.Minute)) ||
				bound.PerSecondUpper != bound.Requests/60 {
				t.Fatal("bound omits edge traffic or divides by its longer scrape span", bound)
			}
			if start.Before(w.Start.Add(50*time.Second)) && bound.PerSecondUpper < 7000.0/60 {
				t.Fatal("a window containing the burst passed its limit", start, bound)
			}
		}
		if !covered {
			t.Fatal("unmeasured window start", start)
		}
	}
}

func TestAPIDemandRefusesGapsResetsAndMissingWindowEdges(t *testing.T) {
	for _, mode := range []string{"missing counter", "counter reset", "process replacement", "failed scrape", "invalid counter", "overlapping scrapes", "missing first", "missing last", "short window"} {
		t.Run(mode, func(t *testing.T) {
			w, samples := requestSamples()
			manager := samples[10].Managers["second"]
			switch mode {
			case "missing counter":
				manager.APIRequests = nil
			case "counter reset":
				manager.APIRequests.Total = 0
			case "process replacement":
				manager.ProcessStartedAt++
			case "failed scrape":
				samples[10].Incomplete = []string{sourceManagers}
			case "invalid counter":
				manager.APIRequests.Total = math.NaN()
			case "overlapping scrapes":
				manager.APIRequests.StartedAt = samples[9].Managers["second"].APIRequests.FinishedAt
			case "missing first":
				w.Start = w.Start.Add(-time.Minute)
			case "missing last":
				w.End = w.End.Add(time.Minute)
			case "short window":
				w.End = w.Start.Add(59 * time.Second)
			}
			samples[10].Managers["second"] = manager
			got, problems := managerAPIDemand(w, samples)
			if got != nil || len(problems) == 0 {
				t.Fatal("incomplete API demand produced a bound", got, problems)
			}
			if string(jsonObject(t, cost(w, samples, nil))["managerAPIDemand"]) != "null" {
				t.Fatal("missing API demand serialized as a measured value")
			}
		})
	}
}

func TestAPIDemandPreservesAMeasuredZero(t *testing.T) {
	w, samples := requestSamples()
	for _, s := range samples {
		for _, m := range s.Managers {
			m.APIRequests.Total = 0
		}
	}
	got, problems := managerAPIDemand(w, samples)
	if got == nil || len(problems) > 0 || got.MaxPerSecondUpper != 0 || len(got.Windows) == 0 {
		t.Fatal("measured zero is unavailable", got, problems)
	}
}

func TestSamplerRetainsTotalRequestsAndScrapeBounds(t *testing.T) {
	for _, mode := range []string{"all statuses", "absent", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			metrics := completeProcessMetrics
			if mode != "absent" {
				metrics += "# TYPE rest_client_requests_total counter\nrest_client_requests_total{code=\"200\"} 40\nrest_client_requests_total{code=\"429\"} 2\n"
			}
			if mode == "invalid" {
				metrics = strings.ReplaceAll(metrics, "} 40", "} NaN")
			}
			s := measurementFixture(t, "", metrics)
			before := time.Now().UTC()
			if err := s.take(context.Background()); err != nil {
				t.Fatal(err)
			}
			after := time.Now().UTC()
			rows, _ := s.snapshot()
			if len(rows) != 1 || len(rows[0].Managers) != 2 {
				t.Fatal("did not read both replicas")
			}
			for _, manager := range rows[0].Managers {
				r := manager.APIRequests
				if mode != "all statuses" {
					if r != nil {
						t.Fatal("missing/invalid request counter became a measured zero")
					}
					continue
				}
				if r == nil || r.Total != 42 || r.StartedAt.Before(before) || r.FinishedAt.Before(r.StartedAt) || r.FinishedAt.After(after) {
					t.Fatal("incorrect total or scrape bounds", r)
				}
			}
		})
	}
}

func TestAPIDemandSurvivesAnIndependentQueueHistogramReset(t *testing.T) {
	w, samples := requestSamples()
	for i := range samples {
		manager := samples[i].Managers["first"]
		// Native readings retained the same process and increasing API count
		// while the queue histogram changed from 10,918 observations to 24.
		count := 10918.0
		if i >= 10 {
			count = 24
		}
		manager.QueueWait = testHistogram(map[float64]float64{1: count}, count, count/2)
		samples[i].Managers["first"] = manager
	}
	if len(managerQueueContinuity(samples)) == 0 || len(managerCounterContinuity(samples)) != 0 {
		t.Fatal("the queue reset must still invalidate queue evidence")
	}
	got, problems := managerAPIDemand(w, samples)
	if got == nil || len(problems) != 0 || got.MaxPerSecondUpper <= 0 {
		t.Fatal("an unrelated queue reset erased continuous API evidence", got, problems)
	}
}
