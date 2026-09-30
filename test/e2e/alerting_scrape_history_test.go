package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type alScrapeHistoryFixture struct {
	started, queried    time.Time
	up, durations, view []alAdmissionSeries
}

func alScrapeHistoryFixtureForTest(recovered bool) alScrapeHistoryFixture {
	f := alScrapeHistoryFixture{started: time.Unix(1800000000, 0).UTC()}
	last := 90
	if recovered {
		last = 150
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
	return alReadScrapeHistory(alAdmissionHistoryBodyForTest(t, f.up), alAdmissionHistoryBodyForTest(t, f.durations),
		[]string{"leader", "follower"}, "leader", f.started, f.queried)
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
