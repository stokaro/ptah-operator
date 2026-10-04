package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type alLostViewFixture struct {
	started, queried time.Time
	alerts, up       []alAdmissionSeries
}

func alLostViewFixtureForTest() alLostViewFixture {
	f := alLostViewFixture{started: time.Unix(1800000000, 0).UTC()}
	f.queried = f.started.Add(101 * time.Second)
	for _, state := range []string{"pending", "firing"} {
		s := alAdmissionSeries{Metric: map[string]string{"__name__": "ALERTS", "alertname": alViewNotSynced, "alertstate": state, "severity": "warning"}}
		first, last := 10, 65
		if state == "firing" {
			first, last = 70, 100
		}
		for second := first; second <= last; second += 5 {
			s.Values = append(s.Values, alAdmissionHistorySampleForTest(f.started.Add(time.Duration(second)*time.Second), "1"))
		}
		f.alerts = append(f.alerts, s)
	}
	for i, pod := range []string{"old-leader", "old-follower"} {
		s := alAdmissionSeries{Metric: map[string]string{"__name__": "up", "job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", i+1)}}
		for second := -10; second <= 15; second += 5 {
			value := "1"
			if pod == "old-leader" && second >= 5 {
				value = "0"
			}
			s.Values = append(s.Values, alAdmissionHistorySampleForTest(f.started.Add(time.Duration(second)*time.Second), value))
		}
		f.up = append(f.up, s)
	}
	return f
}

func (f alLostViewFixture) read(t *testing.T) (alLostViewHistory, error) {
	t.Helper()
	return alLostViewFiring(alAdmissionHistoryBodyForTest(t, f.alerts), alAdmissionHistoryBodyForTest(t, f.up), []string{"old-leader", "old-follower"}, "old-leader", f.started, f.queried)
}

func TestAlLostViewRequiresCompleteNativePendingAndFiring(t *testing.T) {
	t.Parallel()
	f := alLostViewFixtureForTest()
	r, err := f.read(t)
	if err != nil || !r.transition.Equal(f.started.Add(10*time.Second)) || !r.firstFailure.Equal(f.started.Add(5*time.Second)) || !r.fired.Equal(f.started.Add(70*time.Second)) {
		t.Fatalf("lost native incident times: %+v %v", r, err)
	}
	firing := alDelivery{StartsAt: r.fired, ReceivedAt: f.started.Add(95 * time.Second)}
	if !alLostViewDelivered(firing, r) {
		t.Fatal("valid native delivery rejected")
	}
	shifted := f
	shifted.started = shifted.started.Add(time.Second)
	other, err := shifted.read(t)
	if err != nil || other != r {
		t.Fatalf("later observation changed native incident: %+v %v", other, err)
	}
	for name, mutate := range map[string]func(*alLostViewFixture){
		"no pending": func(f *alLostViewFixture) { f.alerts = f.alerts[1:] },
		"no firing":  func(f *alLostViewFixture) { f.alerts = f.alerts[:1] },
		"missing evaluation": func(f *alLostViewFixture) {
			f.alerts[0].Values = append(f.alerts[0].Values[:3], f.alerts[0].Values[4:]...)
		},
		"early firing": func(f *alLostViewFixture) {
			f.alerts[0].Values = f.alerts[0].Values[:len(f.alerts[0].Values)-1]
			f.alerts[1].Values = append([][]json.RawMessage{alAdmissionHistorySampleForTest(f.started.Add(65*time.Second), "1")}, f.alerts[1].Values...)
		},
		"duplicate state": func(f *alLostViewFixture) { f.alerts = append(f.alerts, f.alerts[0]) },
		"duplicate evaluation": func(f *alLostViewFixture) {
			f.alerts[1].Values = append([][]json.RawMessage{f.alerts[0].Values[len(f.alerts[0].Values)-1]}, f.alerts[1].Values...)
		},
		"inactive sample":               func(f *alLostViewFixture) { f.alerts[0].Values[2][1] = json.RawMessage(`"0"`) },
		"changed alert identity":        func(f *alLostViewFixture) { f.alerts[1].Metric["severity"] = "critical" },
		"late query":                    func(f *alLostViewFixture) { f.queried = f.queried.Add(10 * time.Second) },
		"missing manager":               func(f *alLostViewFixture) { f.up = f.up[:1] },
		"different target":              func(f *alLostViewFixture) { f.up[1].Metric["pod"] = "other" },
		"duplicate target":              func(f *alLostViewFixture) { f.up[1] = f.up[0] },
		"no baseline":                   func(f *alLostViewFixture) { f.up[0].Values = f.up[0].Values[3:] },
		"unhealthy baseline":            func(f *alLostViewFixture) { f.up[0].Values[1][1] = json.RawMessage(`"0"`) },
		"leader recovered during fault": func(f *alLostViewFixture) { f.up[0].Values[len(f.up[0].Values)-1][1] = json.RawMessage(`"1"`) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := alLostViewFixtureForTest()
			mutate(&bad)
			if _, err := bad.read(t); err == nil {
				t.Fatal("invalid native loss accepted")
			}
		})
	}
	// Service discovery can remove a target before its next scrape. The
	// complete pending transition still dates the unavailable view in that case.
	f = alLostViewFixtureForTest()
	f.up[0].Values = f.up[0].Values[:3]
	r, err = f.read(t)
	if err != nil || !r.firstFailure.IsZero() {
		t.Fatalf("removed target needs no fabricated failed scrape: %+v %v", r, err)
	}
}

func TestAlLostViewDeliveryUsesTheEarlierNativeDeadline(t *testing.T) {
	t.Parallel()
	f := alLostViewFixtureForTest()
	r, err := f.read(t)
	if err != nil {
		t.Fatal(err)
	}
	r.through = f.started.Add(120 * time.Second)
	d := alDelivery{StartsAt: r.fired, ReceivedAt: r.firstFailure.Add(alViewUnsyncedFor + alDetectionSlack)}
	if !alLostViewDelivered(d, r) {
		t.Fatal("exact first-failure deadline rejected")
	}
	for name, mutate := range map[string]func(*alDelivery){
		"one nanosecond late":                  func(d *alDelivery) { d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond) },
		"early firing hidden by late delivery": func(d *alDelivery) { d.StartsAt = r.fired.Add(-time.Nanosecond) },
		"reversed receipt":                     func(d *alDelivery) { d.ReceivedAt = d.StartsAt.Add(-time.Nanosecond) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := d
			mutate(&bad)
			if alLostViewDelivered(bad, r) {
				t.Fatal("invalid notification accepted")
			}
		})
	}
	r.through = d.ReceivedAt.Add(-time.Nanosecond)
	if alLostViewDelivered(d, r) {
		t.Fatal("incomplete evaluation history accepted")
	}
}

type alLostRecoveryFixture struct {
	restored, queried   time.Time
	view, up, durations []alAdmissionSeries
}

func alLostRecoveryFixtureForTest() alLostRecoveryFixture {
	f := alLostRecoveryFixture{restored: time.Unix(1800000120, 0).UTC()}
	f.queried = f.restored.Add(71 * time.Second)
	for i, pod := range []string{"new-leader", "new-follower"} {
		series := func(metric string) alAdmissionSeries {
			return alAdmissionSeries{Metric: map[string]string{"__name__": metric, "job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.1.%d:8080", i+1)}}
		}
		view, up, duration := series("ptah_operator_unresolved_view_synced"), series("up"), series("scrape_duration_seconds")
		for second := 10; second <= 70; second += 5 {
			at := f.restored.Add(time.Duration(second) * time.Second)
			value := "0"
			if pod == "new-leader" && second >= 20 {
				value = "1"
			}
			view.Values = append(view.Values, alAdmissionHistorySampleForTest(at, value))
			up.Values = append(up.Values, alAdmissionHistorySampleForTest(at, "1"))
			duration.Values = append(duration.Values, alAdmissionHistorySampleForTest(at, "0.125"))
		}
		f.view = append(f.view, view)
		f.up = append(f.up, up)
		f.durations = append(f.durations, duration)
	}
	return f
}
func (f alLostRecoveryFixture) read(t *testing.T) (alScrapeHistory, error) {
	t.Helper()
	return alLostViewRecovery(alAdmissionHistoryBodyForTest(t, f.view), alAdmissionHistoryBodyForTest(t, f.up), alAdmissionHistoryBodyForTest(t, f.durations), []string{"new-leader", "new-follower"}, "new-leader", f.restored, f.queried)
}

func TestAlLostViewRecoveryUsesFirstSynchronizedReplacementScrape(t *testing.T) {
	t.Parallel()
	f := alLostRecoveryFixtureForTest()
	r, err := f.read(t)
	if err != nil || !r.recovered.Equal(f.restored.Add(20*time.Second)) || !r.scrapedThrough.Equal(f.restored.Add(70*time.Second)) {
		t.Fatalf("lost first synchronized scrape: %+v %v", r, err)
	}
	firing := alDelivery{StartsAt: f.restored.Add(-time.Minute)}
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: r.recovered.Add(5 * time.Second), ReceivedAt: r.recovered.Add(alDetectionSlack)}
	if !alScrapeFailureCleared(firing, resolved, r) {
		t.Fatal("exact resolution deadline rejected")
	}
	resolved.ReceivedAt = resolved.ReceivedAt.Add(time.Nanosecond)
	if alScrapeFailureCleared(firing, resolved, r) {
		t.Fatal("late resolution accepted")
	}
	for name, mutate := range map[string]func(*alLostRecoveryFixture){
		"missing follower": func(f *alLostRecoveryFixture) { f.view = f.view[:1] },
		"old Pod":          func(f *alLostRecoveryFixture) { f.view[0].Metric["pod"] = "old-leader" },
		"duplicate Pod":    func(f *alLostRecoveryFixture) { f.view[1] = f.view[0] },
		"changed endpoint": func(f *alLostRecoveryFixture) { f.up[0].Metric["instance"] = "other" },
		"failed scrape":    func(f *alLostRecoveryFixture) { f.up[0].Values[4][1] = json.RawMessage(`"0"`) },
		"missing scrape": func(f *alLostRecoveryFixture) {
			f.view[0].Values = append(f.view[0].Values[:3], f.view[0].Values[4:]...)
		},
		"stale history":               func(f *alLostRecoveryFixture) { f.queried = f.queried.Add(10 * time.Second) },
		"scrape before restore":       func(f *alLostRecoveryFixture) { f.restored = f.restored.Add(15 * time.Second) },
		"duration mismatch":           func(f *alLostRecoveryFixture) { f.durations[0].Values[3][0] = json.RawMessage(`1800000146`) },
		"timed out scrape":            func(f *alLostRecoveryFixture) { f.durations[0].Values[3][1] = json.RawMessage(`"5"`) },
		"follower synchronized":       func(f *alLostRecoveryFixture) { f.view[1].Values[4][1] = json.RawMessage(`"1"`) },
		"leader lost synchronization": func(f *alLostRecoveryFixture) { f.view[0].Values[4][1] = json.RawMessage(`"0"`) },
		"leader never synchronized": func(f *alLostRecoveryFixture) {
			for i := range f.view[0].Values {
				f.view[0].Values[i][1] = json.RawMessage(`"0"`)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := alLostRecoveryFixtureForTest()
			mutate(&bad)
			if _, err := bad.read(t); err == nil {
				t.Fatal("invalid native recovery accepted")
			}
		})
	}
}
