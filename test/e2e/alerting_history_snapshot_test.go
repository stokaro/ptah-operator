package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHistorySnapshotKeepsScrapeCommitsTogether(t *testing.T) {
	f := alScrapeHistoryFixtureForTest(true)
	// Both HTTP requests can use the same evaluation time while a scrape with
	// an earlier timestamp commits between them. This is not a lost scrape.
	beforeCommit := append([]alAdmissionSeries(nil), f.up...)
	beforeCommit[0].Values = beforeCommit[0].Values[:len(beforeCommit[0].Values)-1]
	if _, err := alReadScrapeHistory(alAdmissionHistoryBodyForTest(t, beforeCommit), alAdmissionHistoryBodyForTest(t, f.durations),
		[]string{"leader", "follower"}, "leader", f.started, f.queried); err == nil {
		t.Fatal("separate storage snapshots unexpectedly matched")
	}
	rows := append(append([]alAdmissionSeries(nil), f.up...), f.durations...)
	body := alAdmissionHistoryBodyForTest(t, rows)
	groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{"up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := alReadScrapeHistory(groups["up"], groups["scrape_duration_seconds"], []string{"leader", "follower"}, "leader", f.started, f.queried)
	if err != nil || !h.scrapedThrough.Equal(f.started.Add(150*time.Second)) {
		t.Fatalf("one complete snapshot lost native observations: %+v %v", h, err)
	}
	// A real hole in that snapshot still fails; the split must not align away
	// missing evidence or trim both groups to their common timestamps.
	rows[2].Values = rows[2].Values[:len(rows[2].Values)-1]
	groups, err = alSplitHistorySnapshot(alAdmissionHistoryBodyForTest(t, rows), alScrapeJob, []string{"up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alReadScrapeHistory(groups["up"], groups["scrape_duration_seconds"], []string{"leader", "follower"}, "leader", f.started, f.queried); err == nil {
		t.Fatal("a genuinely missing scrape duration was accepted")
	}
}

func TestHistorySnapshotRejectsPartialAndUnexpectedResponses(t *testing.T) {
	f := alScrapeHistoryFixtureForTest(true)
	for name, mutate := range map[string]func(map[string]any){
		"warning": func(v map[string]any) { v["warnings"] = []string{"partial data"} },
		"info":    func(v map[string]any) { v["infos"] = []string{"omitted samples"} },
		"error":   func(v map[string]any) { v["status"] = "error" },
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if err := json.Unmarshal(alAdmissionHistoryBodyForTest(t, f.up), &v); err != nil {
				t.Fatal(err)
			}
			mutate(v)
			body, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := alSplitHistorySnapshot(body, alScrapeJob, []string{"up"}); err == nil {
				t.Fatal("incomplete response accepted")
			}
		})
	}
	body := alAdmissionHistoryBodyForTest(t, f.up)
	for _, metrics := range [][]string{{"other"}, {"up", "up"}, {""}} {
		if _, err := alSplitHistorySnapshot(body, alScrapeJob, metrics); err == nil {
			t.Fatal("unexpected metric accepted")
		}
	}
	if _, err := alSplitHistorySnapshot(body, "another-job", []string{"up"}); err == nil {
		t.Fatal("another scrape job accepted")
	}
	query := alHistorySnapshotQuery(alScrapeJob, `,family=~"schema|"`, []string{alUnresolvedMetric, "up", "scrape_duration_seconds"})
	if strings.Contains(query, ":") || !strings.HasSuffix(query, "[900s]") {
		t.Fatalf("native range query became a resampling subquery: %s", query)
	}
}
