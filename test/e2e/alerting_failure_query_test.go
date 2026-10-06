package e2e

import (
	"strings"
	"testing"
)

func TestAlFailuresQueriesOneScrapeSnapshot(t *testing.T) {
	f := alFailureHistoryFixtureForTest()
	// A scrape with an earlier timestamp can commit between API calls even
	// when their evaluation time is identical. Model that append boundary:
	// the first counter reading predates the health readings by one sample.
	before := append([]alAdmissionSeries(nil), f.counters...)
	before[0].Values = before[0].Values[:len(before[0].Values)-1]
	oldCounters := alAdmissionHistoryBodyForTest(t, before)
	up := alAdmissionHistoryBodyForTest(t, f.up)
	durations := alAdmissionHistoryBodyForTest(t, f.durations)
	all := append(append(append([]alAdmissionSeries(nil), f.counters...), f.up...), f.durations...)
	snapshot := alAdmissionHistoryBodyForTest(t, all)
	requests := 0
	query := func(expression string) ([]byte, error) {
		requests++
		switch {
		case strings.HasPrefix(expression, alFailuresMetric+"{"):
			return oldCounters, nil
		case strings.HasPrefix(expression, "up{"):
			return up, nil
		case strings.HasPrefix(expression, "scrape_duration_seconds{"):
			return durations, nil
		default:
			return snapshot, nil
		}
	}
	history, err := alQueryFailuresHistory(query, []string{"leader", "follower"}, "leader", "schema", f.started, f.queried)
	if err != nil || history.increments != 5 {
		t.Fatalf("one native scrape snapshot = %+v, %v", history, err)
	}
	if requests != 1 {
		t.Fatalf("read %d independent storage snapshots, want one", requests)
	}
}

func TestAlFailuresSnapshotRejectsIncompleteOrUnrelatedSeries(t *testing.T) {
	for name, mutate := range map[string]func(*alFailureHistoryFixture){
		"missing latest sample": func(f *alFailureHistoryFixture) {
			f.counters[0].Values = f.counters[0].Values[:len(f.counters[0].Values)-1]
		},
		"unknown metric": func(f *alFailureHistoryFixture) {
			f.counters[0].Metric["__name__"] = "unrelated_total"
		},
		"wrong family": func(f *alFailureHistoryFixture) {
			f.counters[0].Metric["family"] = "migration"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := alFailureHistoryFixtureForTest()
			mutate(&f)
			all := append(append(f.counters, f.up...), f.durations...)
			body := alAdmissionHistoryBodyForTest(t, all)
			query := func(string) ([]byte, error) { return body, nil }
			if _, err := alQueryFailuresHistory(query, []string{"leader", "follower"}, "leader", "schema", f.started, f.queried); err == nil {
				t.Fatal("accepted an incomplete or unrelated scrape snapshot")
			}
		})
	}
}
