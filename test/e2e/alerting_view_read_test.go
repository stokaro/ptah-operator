package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestAlViewReadRuleChangesOnlyList(t *testing.T) {
	t.Parallel()
	role := &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahschemas"}, Verbs: []string{"get", "list", "watch", "patch"},
	}}}
	before := role.DeepCopy()
	index, held, err := alViewReadRule(role, "ptahschemas")
	if err != nil || index != 0 || !reflect.DeepEqual(held, []string{"get", "watch", "patch"}) || !reflect.DeepEqual(role, before) {
		t.Fatalf("changed another permission or the original: %d %v %v", index, held, err)
	}
	for name, mutate := range map[string]func(*rbacv1.ClusterRole){
		"no rule":          func(r *rbacv1.ClusterRole) { r.Rules = nil },
		"duplicate rule":   func(r *rbacv1.ClusterRole) { r.Rules = append(r.Rules, r.Rules[0]) },
		"mixed resources":  func(r *rbacv1.ClusterRole) { r.Rules[0].Resources = append(r.Rules[0].Resources, "ptahmigrations") },
		"mixed API groups": func(r *rbacv1.ClusterRole) { r.Rules[0].APIGroups = append(r.Rules[0].APIGroups, "example.org") },
		"name restriction": func(r *rbacv1.ClusterRole) { r.Rules[0].ResourceNames = []string{"one"} },
		"no list":          func(r *rbacv1.ClusterRole) { r.Rules[0].Verbs = []string{"get", "watch"} },
		"no watch":         func(r *rbacv1.ClusterRole) { r.Rules[0].Verbs = []string{"get", "list"} },
		"wildcard":         func(r *rbacv1.ClusterRole) { r.Rules[0].Verbs = append(r.Rules[0].Verbs, "*") },
	} {
		t.Run(name, func(t *testing.T) {
			r := role.DeepCopy()
			mutate(r)
			if _, _, err := alViewReadRule(r, "ptahschemas"); err == nil {
				t.Fatal("unsafe or vacuous fault accepted")
			}
		})
	}
}

type alViewHistoryFixture struct {
	started, queried        time.Time
	pods                    []string
	leader                  string
	counters, up, durations []alAdmissionSeries
}

func alViewHistoryFixtureForTest() alViewHistoryFixture {
	f := alViewHistoryFixture{started: time.Unix(1800000000, 0).UTC(), pods: []string{"manager-a", "manager-b"}, leader: "manager-a"}
	f.queried = f.started.Add(31 * time.Second)
	for index, pod := range f.pods {
		makeSeries := func(metric string) alAdmissionSeries {
			return alAdmissionSeries{Metric: map[string]string{"__name__": metric, "job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", index+1)}}
		}
		counter, up, duration := makeSeries(alViewReadMetric), makeSeries("up"), makeSeries("scrape_duration_seconds")
		for second := -10; second <= 30; second += 5 {
			at := f.started.Add(time.Duration(second) * time.Second)
			value := 0
			if pod == f.leader {
				value = 4
				if second >= 5 {
					value++
				}
				if second >= 10 {
					value++
				}
			}
			counter.Values = append(counter.Values, alAdmissionHistorySampleForTest(at, fmt.Sprint(value)))
			up.Values = append(up.Values, alAdmissionHistorySampleForTest(at, "1"))
			duration.Values = append(duration.Values, alAdmissionHistorySampleForTest(at, "0.125"))
		}
		f.counters = append(f.counters, counter)
		f.up = append(f.up, up)
		f.durations = append(f.durations, duration)
	}
	return f
}

func (f alViewHistoryFixture) read(t *testing.T) (alViewReadHistory, error) {
	t.Helper()
	return alReadViewHistory(alAdmissionHistoryBodyForTest(t, f.counters), alAdmissionHistoryBodyForTest(t, f.up), alAdmissionHistoryBodyForTest(t, f.durations),
		f.pods, f.leader, f.started, f.queried)
}

func TestAlViewReadHistoryDatesNativeFailures(t *testing.T) {
	t.Parallel()
	f := alViewHistoryFixtureForTest()
	r, err := f.read(t)
	if err != nil || !r.firstLower.Equal(f.started) || !r.lastLower.Equal(f.started.Add(5*time.Second)) ||
		!r.lastUpper.Equal(f.started.Add(10*time.Second+125*time.Millisecond)) || !r.scrapedThrough.Equal(f.started.Add(30*time.Second)) {
		t.Fatalf("native failure intervals = %+v, %v", r, err)
	}
	// The preceding native scrape may predate injection. Clamping it to the
	// poll would shift both the deadline and the rolling-window boundary.
	offset := alViewHistoryFixtureForTest()
	offset.started = offset.started.Add(2 * time.Second)
	reading, readErr := offset.read(t)
	if readErr != nil || !reading.firstLower.Equal(f.started) {
		t.Fatalf("the native lower bound was moved to the injection poll: %+v %v", reading, readErr)
	}
	startup := alViewHistoryFixtureForTest()
	for i := range startup.up {
		startup.up[i].Values = append([][]json.RawMessage{alAdmissionHistorySampleForTest(f.started.Add(-15*time.Second), "0")}, startup.up[i].Values...)
		startup.durations[i].Values = append([][]json.RawMessage{alAdmissionHistorySampleForTest(f.started.Add(-15*time.Second), "0.125")}, startup.durations[i].Values...)
	}
	if reading, err := startup.read(t); err != nil || !reading.firstLower.Equal(f.started) {
		t.Fatalf("unrelated startup scrape changed the measured failure: %+v %v", reading, err)
	}
	// A positive counter already present at baseline is not a new failure.
	for i := range f.counters[0].Values {
		f.counters[0].Values[i][1] = json.RawMessage(`"4"`)
	}
	r, err = f.read(t)
	if err != nil || !r.firstLower.IsZero() || !r.lastLower.IsZero() {
		t.Fatalf("baseline was counted as a new failure: %+v %v", r, err)
	}
}

func TestAlViewReadHistoryRefusesIncompleteOrChangedObservations(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*alViewHistoryFixture){
		"no counter":         func(f *alViewHistoryFixture) { f.counters = nil },
		"missing follower":   func(f *alViewHistoryFixture) { f.up = f.up[:1] },
		"duplicate manager":  func(f *alViewHistoryFixture) { f.counters[1].Metric["pod"] = f.pods[0] },
		"duplicate instance": func(f *alViewHistoryFixture) { f.counters[1].Metric["instance"] = f.counters[0].Metric["instance"] },
		"wrong job":          func(f *alViewHistoryFixture) { f.counters[0].Metric["job"] = "other" },
		"missing baseline":   func(f *alViewHistoryFixture) { f.counters[0].Values = f.counters[0].Values[1:] },
		"missing scrape": func(f *alViewHistoryFixture) {
			f.counters[0].Values = append(f.counters[0].Values[:4], f.counters[0].Values[5:]...)
		},
		"counter reset":             func(f *alViewHistoryFixture) { f.counters[0].Values[6][1] = json.RawMessage(`"0"`) },
		"counter fraction":          func(f *alViewHistoryFixture) { f.counters[0].Values[6][1] = json.RawMessage(`"6.5"`) },
		"counter NaN":               func(f *alViewHistoryFixture) { f.counters[0].Values[6][1] = json.RawMessage(`"NaN"`) },
		"follower failure":          func(f *alViewHistoryFixture) { f.counters[1].Values[8][1] = json.RawMessage(`"1"`) },
		"failed scrape":             func(f *alViewHistoryFixture) { f.up[0].Values[5][1] = json.RawMessage(`"0"`) },
		"missing duration":          func(f *alViewHistoryFixture) { f.durations[0].Values = f.durations[0].Values[1:] },
		"changed duration identity": func(f *alViewHistoryFixture) { f.durations[0].Metric["instance"] = "another:8080" },
		"expired scrape":            func(f *alViewHistoryFixture) { f.durations[0].Values[5][1] = json.RawMessage(`"5"`) },
		"stale history":             func(f *alViewHistoryFixture) { f.queried = f.queried.Add(6 * time.Second) },
		"no recorded leader":        func(f *alViewHistoryFixture) { f.leader = "another" },
		"duplicate expected Pod":    func(f *alViewHistoryFixture) { f.pods[1] = f.pods[0] },
		"no followers":              func(f *alViewHistoryFixture) { f.pods = f.pods[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			f := alViewHistoryFixtureForTest()
			mutate(&f)
			if result, err := f.read(t); err == nil {
				t.Fatalf("invalid history accepted: %+v", result)
			}
		})
	}
}
