package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

type alScrapeHistoryFixture struct {
	started, restored, queried time.Time
	up, durations, view        []alAdmissionSeries
}

func TestAlScrapeReloadHistoryFromPrometheus(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/e2e/readings/prometheus-scrape-reload-history.json")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{"up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	loaded := time.Date(2026, 10, 4, 14, 34, 55, 922214000, time.UTC)
	queried := time.Date(2026, 10, 4, 14, 36, 16, 151037000, time.UTC)
	history, err := alReadScrapeHistoryWithReloads(groups["up"], groups["scrape_duration_seconds"],
		[]string{"leader", "follower"}, "leader", loaded, queried, loaded, time.Time{})
	if err != nil || history.firstFailure.Sub(time.Date(2026, 10, 4, 14, 35, 0, 849000000, time.UTC)).Abs() > time.Microsecond {
		t.Fatalf("lost the native failure after the old target's final in-flight scrape: %+v %v", history, err)
	}
}

func TestAlScrapeReloadRequestPrecedesTheLastHealthyScrape(t *testing.T) {
	t.Parallel()
	// These native CI observations retain a healthy scrape after the reload
	// request started. The target then changes its five-second scrape offset.
	body, err := os.ReadFile("../../testdata/e2e/readings/prometheus-scrape-request-boundary.json")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{"up", "scrape_duration_seconds"})
	if err != nil {
		t.Fatal(err)
	}
	loaded := time.Date(2026, 10, 5, 4, 57, 4, 156234648, time.UTC)
	queried := time.Date(2026, 10, 5, 4, 57, 21, 403000000, time.UTC)
	history, err := alReadScrapeHistoryWithReloads(groups["up"], groups["scrape_duration_seconds"],
		[]string{"leader", "follower"}, "leader", loaded, queried, loaded, time.Time{})
	if err != nil || history.firstFailure.Sub(time.Date(2026, 10, 5, 4, 57, 15, 403000000, time.UTC)).Abs() > time.Microsecond {
		t.Fatalf("the reload request replaced the actual native transition: %+v %v", history, err)
	}
}

func alScrapeHistoryFixtureForTest(recovered bool) alScrapeHistoryFixture {
	f := alScrapeHistoryFixture{started: time.Unix(1800000000, 0).UTC()}
	last := 90
	if recovered {
		last = 150
		f.restored = f.started.Add(97 * time.Second)
	}
	f.queried = f.started.Add(time.Duration(last+1) * time.Second)
	for index, pod := range []string{"leader", "follower"} {
		series := func(metric string) alAdmissionSeries {
			return alAdmissionSeries{Metric: map[string]string{"__name__": metric, "job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", index+1)}}
		}
		up, duration, view := series("up"), series("scrape_duration_seconds"), series("ptah_operator_unresolved_view_synced")
		for second := -10; second <= last; second += 5 {
			at := f.started.Add(time.Duration(second) * time.Second)
			health := "1"
			if pod == "leader" && second >= 5 && second < 100 {
				health = "0"
			}
			up.Values = append(up.Values, alAdmissionHistorySampleForTest(at, health))
			duration.Values = append(duration.Values, alAdmissionHistorySampleForTest(at, "0.125"))
			if pod == "leader" && health == "1" {
				view.Values = append(view.Values, alAdmissionHistorySampleForTest(at, "1"))
			}
		}
		f.up, f.durations = append(f.up, up), append(f.durations, duration)
		if pod == "leader" {
			f.view = append(f.view, view)
		}
	}
	return f
}

func (f alScrapeHistoryFixture) read(t *testing.T) (alScrapeHistory, error) {
	t.Helper()
	return alReadScrapeHistoryWithReloads(alAdmissionHistoryBodyForTest(t, f.up), alAdmissionHistoryBodyForTest(t, f.durations),
		[]string{"leader", "follower"}, "leader", f.started, f.queried, f.started, f.restored)
}

func TestAlScrapeHistoryMeasuresNativeFailureAndSynchronizedRecovery(t *testing.T) {
	t.Parallel()
	f := alScrapeHistoryFixtureForTest(true)
	history, err := f.read(t)
	if err != nil || !history.firstFailure.Equal(f.started.Add(5*time.Second)) || !history.recovered.Equal(f.started.Add(100*time.Second)) ||
		!history.scrapedThrough.Equal(f.started.Add(150*time.Second)) {
		t.Fatalf("lost native failure/recovery times: %+v %v", history, err)
	}
	if err := alRecoveredScrapeSynced(alAdmissionHistoryBodyForTest(t, f.view), "leader", history, f.queried); err != nil {
		t.Fatal("a view present in the first recovered scrape was refused", err)
	}
	offset := f
	offset.started = offset.started.Add(2 * time.Second)
	shifted, err := offset.read(t)
	if err != nil || !shifted.firstFailure.Equal(history.firstFailure) || !shifted.recovered.Equal(history.recovered) {
		t.Fatal("poll timing shifted a native fault or recovery", err)
	}
	firingFixture := alScrapeHistoryFixtureForTest(false)
	firingHistory, err := firingFixture.read(t)
	if err != nil || !firingHistory.recovered.IsZero() {
		t.Fatal("a held failed target appeared recovered", err)
	}
	firing := alDelivery{StartsAt: f.started.Add(65 * time.Second), ReceivedAt: f.started.Add(110 * time.Second)}
	if !alScrapeFailureDelivered(firing, firingHistory) {
		t.Fatal("delivery at the frozen 105-second failure bound was refused")
	}
	for _, mutate := range []func(*alDelivery){
		func(d *alDelivery) { d.StartsAt = d.StartsAt.Add(-time.Nanosecond) },
		func(d *alDelivery) { d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond) },
		func(d *alDelivery) { d.ReceivedAt = d.StartsAt.Add(-time.Nanosecond) },
	} {
		bad := firing
		mutate(&bad)
		if alScrapeFailureDelivered(bad, firingHistory) {
			t.Fatal("an early firing, late delivery or reversed timestamp passed")
		}
	}
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: history.recovered.Add(time.Second), ReceivedAt: f.started.Add(145 * time.Second)}
	if !alScrapeFailureCleared(firing, resolved, history) {
		t.Fatal("resolution at the frozen 45-second recovery bound was refused")
	}
	for _, mutate := range []func(*alDelivery){
		func(d *alDelivery) { d.StartsAt = d.StartsAt.Add(time.Nanosecond) },
		func(d *alDelivery) { d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond) },
		func(d *alDelivery) { d.EndsAt = history.recovered.Add(-time.Nanosecond) },
		func(d *alDelivery) { d.EndsAt = d.ReceivedAt.Add(time.Nanosecond) },
	} {
		bad := resolved
		mutate(&bad)
		if alScrapeFailureCleared(firing, bad, history) {
			t.Fatal("an unrelated, late or premature resolution passed")
		}
	}
	if alScrapeFailureCleared(firing, resolved, firingHistory) {
		t.Fatal("an unrecovered target supplied a resolved proof")
	}
	noFault := alScrapeHistoryFixtureForTest(false)
	for i := range noFault.up[0].Values {
		noFault.up[0].Values[i][1] = json.RawMessage(`"1"`)
	}
	healthy, err := noFault.read(t)
	if err != nil || alScrapeFailureDelivered(firing, healthy) {
		t.Fatal("healthy target history stood in for the injected failure", err)
	}
	history.scrapedThrough = resolved.ReceivedAt.Add(-time.Nanosecond)
	if alScrapeFailureCleared(firing, resolved, history) {
		t.Fatal("incomplete recovery observations passed")
	}
}

func TestAlScrapeHistoryRefusesMissingOrDifferentFaults(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*alScrapeHistoryFixture){
		"empty":             func(f *alScrapeHistoryFixture) { f.up = nil },
		"missing follower":  func(f *alScrapeHistoryFixture) { f.up = f.up[:1] },
		"duplicate target":  func(f *alScrapeHistoryFixture) { f.up = append(f.up, f.up[0]) },
		"same address":      func(f *alScrapeHistoryFixture) { f.up[1].Metric["instance"] = f.up[0].Metric["instance"] },
		"wrong job":         func(f *alScrapeHistoryFixture) { f.up[0].Metric["job"] = "other" },
		"wrong metric":      func(f *alScrapeHistoryFixture) { f.up[0].Metric["__name__"] = "other" },
		"follower failure":  func(f *alScrapeHistoryFixture) { f.up[1].Values[4][1] = json.RawMessage(`"0"`) },
		"failed baseline":   func(f *alScrapeHistoryFixture) { f.up[0].Values[0][1] = json.RawMessage(`"0"`) },
		"nonbinary":         func(f *alScrapeHistoryFixture) { f.up[0].Values[4][1] = json.RawMessage(`"2"`) },
		"refailed":          func(f *alScrapeHistoryFixture) { f.up[0].Values[len(f.up[0].Values)-1][1] = json.RawMessage(`"0"`) },
		"missing baseline":  func(f *alScrapeHistoryFixture) { f.up[0].Values = f.up[0].Values[1:] },
		"missing scrape":    func(f *alScrapeHistoryFixture) { f.up[0].Values = append(f.up[0].Values[:4], f.up[0].Values[5:]...) },
		"stale":             func(f *alScrapeHistoryFixture) { f.queried = f.queried.Add(10 * time.Second) },
		"duration address":  func(f *alScrapeHistoryFixture) { f.durations[0].Metric["instance"] = "other" },
		"missing duration":  func(f *alScrapeHistoryFixture) { f.durations[0].Values = f.durations[0].Values[1:] },
		"duration too long": func(f *alScrapeHistoryFixture) { f.durations[0].Values[4][1] = json.RawMessage(`"5"`) },
	} {
		t.Run(name, func(t *testing.T) {
			f := alScrapeHistoryFixtureForTest(true)
			mutate(&f)
			if _, err := f.read(t); err == nil {
				t.Fatal("incomplete history or a different fault passed")
			}
		})
	}
	for name, mutate := range map[string]func(*alScrapeHistoryFixture){
		"missing view":     func(f *alScrapeHistoryFixture) { f.view = nil },
		"another Pod":      func(f *alScrapeHistoryFixture) { f.view[0].Metric["pod"] = "replacement" },
		"another endpoint": func(f *alScrapeHistoryFixture) { f.view[0].Metric["instance"] = "replacement" },
		"not synced":       func(f *alScrapeHistoryFixture) { f.view[0].Values[3][1] = json.RawMessage(`"0"`) },
		"first recovered absent": func(f *alScrapeHistoryFixture) {
			f.view[0].Values = append(f.view[0].Values[:3], f.view[0].Values[4:]...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := alScrapeHistoryFixtureForTest(true)
			history, err := f.read(t)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&f)
			if alRecoveredScrapeSynced(alAdmissionHistoryBodyForTest(t, f.view), "leader", history, f.queried) == nil {
				t.Fatal("healthy HTTP stood in for the exact synchronized leader scrape")
			}
		})
	}
}

func TestAlScrapeHistoryBindsScheduleChangesToReloads(t *testing.T) {
	t.Parallel()
	fixture := func() alScrapeHistoryFixture {
		f := alScrapeHistoryFixtureForTest(true)
		base := f.started
		// The observed URL change moved the leader's scrape offset by 1.18s.
		// Restoration can choose another offset while the follower stays fixed.
		for _, series := range []*alAdmissionSeries{&f.up[0], &f.durations[0], &f.view[0]} {
			for _, raw := range series.Values {
				var timestamp float64
				if err := json.Unmarshal(raw[0], &timestamp); err != nil {
					t.Fatal(err)
				}
				if timestamp >= float64(base.Add(100*time.Second).Unix()) {
					timestamp += 2.5
				} else if timestamp >= float64(base.Add(5*time.Second).Unix()) {
					timestamp += 1.18
				}
				raw[0] = json.RawMessage(fmt.Sprintf("%.3f", timestamp))
			}
		}
		f.started = base.Add(2 * time.Second)
		f.restored = base.Add(98 * time.Second)
		f.queried = base.Add(154 * time.Second)
		return f
	}
	f := fixture()
	history, err := f.read(t)
	if err != nil {
		t.Fatal("fresh observations on both sides of each reload were refused", err)
	}
	if err := alRecoveredScrapeSynced(alAdmissionHistoryBodyForTest(t, f.view), "leader", history, f.queried); err != nil {
		t.Fatal("the native recovered view was lost", err)
	}
	if _, err := alReadScrapeHistory(alAdmissionHistoryBodyForTest(t, f.up), alAdmissionHistoryBodyForTest(t, f.durations),
		[]string{"leader", "follower"}, "leader", f.started, f.queried); err == nil {
		t.Fatal("a schedule change without a configuration reload passed")
	}
	t.Run("discovery applies the loaded configuration later", func(t *testing.T) {
		f := fixture()
		baseline := f.started.Add(-17 * time.Second)
		for i := range f.up {
			f.up[i].Values = append([][]json.RawMessage{alAdmissionHistorySampleForTest(baseline, "1")}, f.up[i].Values...)
			f.durations[i].Values = append([][]json.RawMessage{alAdmissionHistorySampleForTest(baseline, "0.125")}, f.durations[i].Values...)
		}
		// The old target still scraped after the config reload; discovery
		// applied the new URL on its next five-second refresh.
		f.started = f.started.Add(-4 * time.Second)
		if _, err := f.read(t); err != nil {
			t.Fatal("a bounded discovery refresh was refused", err)
		}
	})
	for name, mutate := range map[string]func(*alScrapeHistoryFixture){
		"last healthy scrape": func(f *alScrapeHistoryFixture) { removeAlScrapeSample(f, 0, 2) },
		"first failed scrape": func(f *alScrapeHistoryFixture) { removeAlScrapeSample(f, 0, 3) },
		"during failure":      func(f *alScrapeHistoryFixture) { removeAlScrapeSample(f, 0, 4) },
		"first recovery":      func(f *alScrapeHistoryFixture) { removeAlScrapeSample(f, 0, 22) },
		"after recovery":      func(f *alScrapeHistoryFixture) { removeAlScrapeSample(f, 0, 23) },
		"follower scrape":     func(f *alScrapeHistoryFixture) { removeAlScrapeSample(f, 1, 3) },
		"unrecorded restore":  func(f *alScrapeHistoryFixture) { f.restored = time.Time{} },
		"restore dated late":  func(f *alScrapeHistoryFixture) { f.restored = f.restored.Add(10 * time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			f := fixture()
			mutate(&f)
			if _, err := f.read(t); err == nil {
				t.Fatal("missing observations or an unbound transition passed")
			}
		})
	}
}

func removeAlScrapeSample(f *alScrapeHistoryFixture, pod, sample int) {
	for _, series := range []*alAdmissionSeries{&f.up[pod], &f.durations[pod]} {
		series.Values = append(series.Values[:sample], series.Values[sample+1:]...)
	}
}
