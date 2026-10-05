package e2e

import (
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAlPlanStoreRecoveryWindowFromNativeScrapes(t *testing.T) {
	t.Parallel()
	// Native samples from CI 37287095881, job 111691203546. The excerpt
	// retains the missing gauges and the healthy scrapes after them.
	body, err := os.ReadFile("../../testdata/e2e/readings/prometheus-plan-store-export-gap.json")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{alPlanStoreMetric, "up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	leader := "ptah-runtime-generated-name-prefix-boundary-proof-e94b5d90rq48t"
	pods := []string{leader, "ptah-runtime-generated-name-prefix-boundary-proof-e94b5d90bzpk4"}
	at := time.Unix(1791204717, 0).UTC()
	read := func(started time.Time) (alPlanStoreHistory, error) {
		return alReadPlanStoreRecoveryHistory(groups[alPlanStoreMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, started, at)
	}
	if _, err := read(at.Add(-100 * time.Second)); err == nil || !strings.Contains(err.Error(), "missing scrapes") {
		t.Fatalf("a gap inside the measured window passed: %v", err)
	}
	h, err := read(at)
	if err != nil || h.latest != 142629098 || !h.crossedLower.IsZero() || !h.clearedLower.IsZero() {
		t.Fatalf("fresh above-threshold baseline after export work = %+v, %v", h, err)
	}
}

func TestAlPlanStoreRecoveryRejectsTheCurrentCIMissingGauge(t *testing.T) {
	t.Parallel()
	// The failed latest scrape from CI 37315953826, job 111785841643,
	// still cannot establish a fresh baseline or complete a recovery window.
	body, err := os.ReadFile("../../testdata/e2e/readings/prometheus-plan-store-stale-gauge.json")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{alPlanStoreMetric, "up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	leader := "ptah-runtime-generated-name-prefix-boundary-proof-aaf9efecvzzjs"
	pods := []string{leader, "ptah-runtime-generated-name-prefix-boundary-proof-aaf9efect5pps"}
	at := time.Unix(1791219460, 999000227).UTC()
	for _, started := range []time.Time{at.Add(-20 * time.Second), at} {
		_, err := alReadPlanStoreRecoveryHistory(groups[alPlanStoreMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, started, at)
		if err == nil || !strings.Contains(err.Error(), "before a fresh scrape") {
			t.Fatalf("missing native gauge supplied recovery authority: %v", err)
		}
	}
}

func TestAlPlanStoreRecoveryKeepsNativeDeadlines(t *testing.T) {
	t.Parallel()
	start := time.Unix(1800000000, 0).UTC()
	at := start.Add(31 * time.Second)
	fixture := func() ([]alAdmissionSeries, []alAdmissionSeries, []alAdmissionSeries) {
		var gauge, up, durations []alAdmissionSeries
		for i, pod := range []string{"leader", "follower"} {
			series := func(metric string) alAdmissionSeries {
				return alAdmissionSeries{Metric: map[string]string{"__name__": metric, "job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", i)}}
			}
			g, u, d := series(alPlanStoreMetric), series("up"), series("scrape_duration_seconds")
			for seconds := -15; seconds <= 30; seconds += 5 {
				when := start.Add(time.Duration(seconds) * time.Second)
				size := alPlanStoreLimit + 1
				if seconds >= 20 {
					size--
				}
				g.Values = append(g.Values, alAdmissionHistorySampleForTest(when, fmt.Sprint(size)))
				u.Values = append(u.Values, alAdmissionHistorySampleForTest(when, "1"))
				d.Values = append(d.Values, alAdmissionHistorySampleForTest(when, "0.125"))
			}
			if pod == "leader" {
				gauge = append(gauge, g)
			}
			up, durations = append(up, u), append(durations, d)
		}
		return gauge, up, durations
	}
	read := func(g, u, d []alAdmissionSeries) (alPlanStoreHistory, error) {
		return alReadPlanStoreRecoveryHistory(alAdmissionHistoryBodyForTest(t, g), alAdmissionHistoryBodyForTest(t, u), alAdmissionHistoryBodyForTest(t, d), []string{"leader", "follower"}, "leader", start, at)
	}
	g, u, d := fixture()
	h, err := read(g, u, d)
	if err != nil || !h.crossedLower.IsZero() || !h.clearedLower.Equal(start.Add(15*time.Second)) || !h.clearedUpper.Equal(start.Add(20*time.Second+125*time.Millisecond)) {
		t.Fatalf("native pruning bracket = %+v, %v", h, err)
	}
	firing := alDelivery{StartsAt: start.Add(-time.Minute)}
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: start.Add(20 * time.Second), ReceivedAt: start.Add(29 * time.Second)}
	if !alPlanStoreResolved(firing, resolved, h, start.Add(time.Second)) {
		t.Fatal("the original incident's bounded recovery was refused")
	}
	for _, mutation := range []string{"early resolution", "new incident", "late resolution", "cleared before pruning"} {
		t.Run(mutation, func(t *testing.T) {
			r, reading, pruning := resolved, h, start.Add(time.Second)
			switch mutation {
			case "early resolution":
				r.EndsAt = h.clearedLower.Add(-time.Nanosecond)
			case "new incident":
				r.StartsAt = r.StartsAt.Add(time.Nanosecond)
			case "late resolution":
				r.ReceivedAt = h.clearedLower.Add(alDetectionSlack + time.Nanosecond)
				reading.through = r.ReceivedAt
			case "cleared before pruning":
				pruning = h.clearedUpper.Add(time.Nanosecond)
			}
			if alPlanStoreResolved(firing, r, reading, pruning) {
				t.Fatal("recovery escaped its native bound or incident")
			}
		})
	}
	for _, mutation := range []string{"below-limit baseline", "gap during pruning", "threshold crossed again"} {
		t.Run(mutation, func(t *testing.T) {
			g, u, d := fixture()
			switch mutation {
			case "below-limit baseline":
				g[0].Values[1] = alAdmissionHistorySampleForTest(start.Add(-10*time.Second), "0")
			case "gap during pruning":
				g[0].Values = append(g[0].Values[:5], g[0].Values[6:]...)
			case "threshold crossed again":
				g[0].Values[len(g[0].Values)-1] = alAdmissionHistorySampleForTest(start.Add(30*time.Second), fmt.Sprint(alPlanStoreLimit+1))
			}
			if _, err := read(g, u, d); err == nil {
				t.Fatal("incomplete or contradictory recovery history was accepted")
			}
		})
	}
}

func TestAlPlanStoreIncidentCannotResolveDuringExport(t *testing.T) {
	t.Parallel()
	firing := alDelivery{AlertName: alPlanStoreAlert, Status: "firing", Receiver: "operations",
		StartsAt: time.Unix(1800000000, 0), ReceivedAt: time.Unix(1800000005, 0),
		Labels:      map[string]string{"operator_namespace": "operator", "operator_metrics_service": "metrics", "severity": "warning"},
		Annotations: map[string]string{"runbook_url": "https://example.com/#prune-plans"}}
	if alPlanStoreIncidentHeld(nil, 0, firing) || alPlanStoreIncidentHeld([]alDelivery{firing}, -1, firing) {
		t.Fatal("an absent original firing counted as retained history")
	}
	for _, mutation := range []string{"repeat", "updated summary", "another installation", "resolved", "new incident", "receiver", "labels", "runbook", "truncated prefix"} {
		t.Run(mutation, func(t *testing.T) {
			next := firing
			next.Labels, next.Annotations = maps.Clone(firing.Labels), maps.Clone(firing.Annotations)
			next.ReceivedAt = next.ReceivedAt.Add(time.Minute)
			switch mutation {
			case "updated summary":
				// The chart includes the current byte total in this annotation.
				next.Annotations["summary"] = "Retained schema plans hold 136MiB"
			case "another installation":
				next.Labels["operator_namespace"] = "other"
			case "resolved":
				next.Status, next.EndsAt = "resolved", next.ReceivedAt
			case "new incident":
				next.StartsAt = next.StartsAt.Add(time.Second)
			case "receiver":
				next.Receiver = "other"
			case "labels":
				next.Labels["severity"] = "critical"
			case "runbook":
				next.Annotations["runbook_url"] = "https://example.com/wrong"
			}
			deliveries := []alDelivery{firing, next}
			if mutation == "truncated prefix" {
				deliveries = deliveries[1:]
			}
			if got, want := alPlanStoreIncidentHeld(deliveries, 0, firing), mutation == "repeat" || mutation == "updated summary" || mutation == "another installation"; got != want {
				t.Fatalf("retained incident = %t, want %t", got, want)
			}
		})
	}
}
