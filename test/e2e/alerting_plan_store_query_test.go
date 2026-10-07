package e2e

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestAlPlanStoreWaitsForTheOriginalScrapeCommit(t *testing.T) {
	t.Parallel()
	// CI 37630860698, job 112828125266: all three leader series end at
	// 14:43:20.165; the query at 14:43:26.689 has no next committed scrape.
	body, err := os.ReadFile("../../testdata/e2e/readings/prometheus-plan-store-pending-scrape.json")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := alAdmissionNativeMatrix(body)
	if err != nil {
		t.Fatal(err)
	}
	leader := "ptah-runtime-generated-name-prefix-boundary-proof-52dd5e22g5wrr"
	pods := []string{leader, "ptah-runtime-generated-name-prefix-boundary-proof-52dd5e22p698k"}
	at := time.Unix(1791384206, 689510281).UTC()
	started := time.Unix(1791384200, 335828091).UTC()
	groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{alPlanStoreMetric, "up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alReadPlanStoreRecoveryHistory(groups[alPlanStoreMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, started, at); !errors.Is(err, errAlHistoryNotFresh) {
		t.Fatalf("original native snapshot no longer reproduces the failure: %v", err)
	}
	// Model the still-running scrape committing its matching native samples.
	// Their timestamp belongs to the original query, even though that query
	// could not read them before the commit. No interpolation fills the tail.
	completed := func(t *testing.T) []alAdmissionSeries {
		t.Helper()
		out, err := alAdmissionNativeMatrix(body)
		if err != nil {
			t.Fatal(err)
		}
		for i := range out {
			if out[i].Metric["pod"] != leader {
				continue
			}
			value := "142630492"
			switch out[i].Metric["__name__"] {
			case "up":
				value = "1"
			case "scrape_duration_seconds":
				value = "2"
			}
			out[i].Values = append(out[i].Values, alAdmissionHistorySampleForTest(time.Unix(1791384205, 165000000).UTC(), value))
		}
		return out
	}
	for _, mutation := range []string{"completed", "missing gauge", "unhealthy", "timed out", "interior gap", "future sample"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			next := completed(t)
			for i := range next {
				if next[i].Metric["pod"] != leader {
					continue
				}
				last := len(next[i].Values) - 1
				switch {
				case mutation == "missing gauge" && next[i].Metric["__name__"] == alPlanStoreMetric:
					next[i].Values = next[i].Values[:last]
				case mutation == "unhealthy" && next[i].Metric["__name__"] == "up":
					next[i].Values[last] = alAdmissionHistorySampleForTest(time.Unix(1791384205, 165000000).UTC(), "0")
				case mutation == "timed out" && next[i].Metric["__name__"] == "scrape_duration_seconds":
					next[i].Values[last] = alAdmissionHistorySampleForTest(time.Unix(1791384205, 165000000).UTC(), "4.1")
				case mutation == "interior gap":
					next[i].Values = append(next[i].Values[:last-1], next[i].Values[last:]...)
				case mutation == "future sample":
					next[i].Values[last] = alAdmissionHistorySampleForTest(at.Add(time.Second), "1")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			h, _, err := alQueryPlanStoreHistory(ctx, func(ctx context.Context, queriedAt time.Time) ([]byte, error) {
				calls++
				if !queriedAt.Equal(at) {
					t.Fatal("waiting for a commit moved the measurement window")
				}
				deadline, ok := ctx.Deadline()
				if calls > 1 && (!ok || time.Until(deadline) > alScrapeTimeout) {
					t.Fatal("query outlives the configured scrape timeout")
				}
				if calls == 1 {
					return alAdmissionHistoryBodyForTest(t, rows), nil
				}
				if calls > 2 {
					cancel()
					return nil, ctx.Err()
				}
				return alAdmissionHistoryBodyForTest(t, next), nil
			}, pods, leader, started, at, true)
			if mutation == "completed" {
				if err != nil || calls != 2 || h.latest != 142630492 || !h.clearedLower.IsZero() || h.through.UnixMilli() != 1791384204880 {
					t.Fatalf("completed original scrape = %+v, calls=%d, error=%v", h, calls, err)
				}
			} else if err == nil {
				t.Fatal("waiting accepted an incomplete or invalid history")
			}
		})
	}
}

func TestAlPlanStoreQueryDoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()
	queryError := errors.New("query transport failed")
	for _, failedQuery := range []bool{false, true} {
		calls := 0
		_, _, err := alQueryPlanStoreHistory(context.Background(), func(context.Context, time.Time) ([]byte, error) {
			calls++
			if failedQuery {
				return nil, queryError
			}
			return []byte(`{"status":"error"}`), nil
		}, []string{"leader", "follower"}, "leader", time.Now(), time.Now(), true)
		if err == nil || calls != 1 || failedQuery && !errors.Is(err, queryError) {
			t.Fatalf("permanent query failure: calls=%d, error=%v", calls, err)
		}
	}
}
