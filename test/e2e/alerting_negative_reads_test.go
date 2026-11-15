package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The quiet window of CI run 43704948556 failed on Kubernetes 1.37 with one
// alert firing, PtahOperatorUnresolvedViewReadFailures, and nothing new at the
// receiver. The row printed no native history, so the reading is built from
// the history the same run's unresolved-apply row printed, at a time both
// families read zero, with the leader's scrape at 11:05:14.670 made a declared
// failed read. testdata/e2e/readings/README.md says how it was built.
const (
	quietLeader    = "ptah-runtime-generated-name-prefix-boundary-proof-3f4e628cfsncq"
	quietFollower  = "ptah-runtime-generated-name-prefix-boundary-proof-3f4e628cxd7dq"
	quietFailed    = "1794740714.670"
	quietNamespace = "ptah-system-1-37-0-ci-43704948556-1-1-37-alerting-op-3f4e628c84"
	quietService   = "ptah-runtime-generated-name-prefix-boundary-proof-3f4e6-metrics"
	// The instant answer for the one alert the failing poll listed at
	// 11:19:37, firing with only its severity label: an ALERTS series
	// carries that label, the alert name and its state. Its sample is dated
	// at the instant the reading's alert was first seen.
	quietAlertAnswer = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"ALERTS","alertname":"PtahOperatorUnresolvedViewReadFailures","alertstate":"firing","severity":"warning"},"value":[1794740721,"1"]}]}}`
)

func quietInstant(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// quietScrape is a scrape timestamp of the stored reading as the native parser
// reads it.
func quietScrape(t *testing.T, value string) time.Time {
	t.Helper()
	at := quietInstant(t, "2026-11-15T11:06:27Z")
	samples, err := alNativeSamples(alAdmissionSeries{Values: [][]json.RawMessage{{json.RawMessage(value), json.RawMessage(`"0"`)}}}, at, at, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return samples[0].at
}

// quietScrapeAfter is the leader scrape n intervals after the failed read.
func quietScrapeAfter(n int) string {
	return fmt.Sprintf("%.3f", 1794740714.670+float64(5*n))
}

// quietPlan says what each scrape of the stored reading carries. The leader's
// failed scrapes carry no census and a view of 0, every other leader scrape a
// view of 1. The counter of each Pod rises by one at each scrape its rises
// list. A missing scrape is one Prometheus never committed, so none of the
// Pod's series has it.
type quietPlan struct {
	failed  []string
	rises   map[string][]string
	missing map[string][]string
	// view and census replace the value a leader scrape reads.
	view   map[string]string
	census map[string]map[string]string
	extra  []alAdmissionSeries
}

func quietDeclared() quietPlan {
	return quietPlan{failed: []string{quietFailed}, rises: map[string][]string{quietLeader: {quietFailed}}}
}

type quietReading struct {
	t      *testing.T
	stored []alAdmissionSeries
}

func newQuietReading(t *testing.T) quietReading {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/e2e/readings/prometheus-quiet-window-declared-read.json")
	if err != nil {
		t.Fatal(err)
	}
	series, err := alAdmissionNativeMatrix(raw)
	if err != nil {
		t.Fatal(err)
	}
	return quietReading{t: t, stored: series}
}

// series rebuilds the reading the plan describes from the stored scrapes.
// The leader's census reads what the stored reading holds, and 0 at the
// scrape the stored reading made its failed read, which is what both
// families read there before it was.
func (r quietReading) series(plan quietPlan) []alAdmissionSeries {
	r.t.Helper()
	scrapes := map[string][]json.RawMessage{}
	for _, s := range r.stored {
		if s.Metric["__name__"] == "scrape_duration_seconds" {
			for _, sample := range s.Values {
				scrapes[s.Metric["pod"]] = append(scrapes[s.Metric["pod"]], sample[0])
			}
		}
	}
	text := func(value string) json.RawMessage { return json.RawMessage(fmt.Sprintf("%q", value)) }
	var out []alAdmissionSeries
	for _, s := range r.stored {
		pod, name := s.Metric["pod"], s.Metric["__name__"]
		stored := map[string]json.RawMessage{}
		for _, sample := range s.Values {
			stored[string(sample[0])] = sample[1]
		}
		rebuilt := alAdmissionSeries{Metric: maps.Clone(s.Metric)}
		count := 0
		for _, at := range scrapes[pod] {
			key := string(at)
			if slices.Contains(plan.rises[pod], key) {
				count++
			}
			if slices.Contains(plan.missing[pod], key) {
				continue
			}
			failed := pod == quietLeader && slices.Contains(plan.failed, key)
			value, ok := stored[key]
			switch name {
			case alUnresolvedMetric:
				if failed {
					continue
				}
				if !ok {
					value = text("0")
				}
				if override, found := plan.census[s.Metric["family"]][key]; found {
					value = text(override)
				}
			case alViewSyncedMetric:
				if pod == quietLeader {
					value = text("1")
					if failed {
						value = text("0")
					}
					if override, found := plan.view[key]; found {
						value = text(override)
					}
				}
			case alViewReadFailuresMetric:
				value = text(fmt.Sprint(count))
			}
			if !ok && name != alUnresolvedMetric {
				r.t.Fatalf("the stored %s has no sample at %s", name, key)
			}
			rebuilt.Values = append(rebuilt.Values, []json.RawMessage{at, value})
		}
		out = append(out, rebuilt)
	}
	return append(out, plan.extra...)
}

func (r quietReading) groups(plan quietPlan) map[string][]byte {
	r.t.Helper()
	groups, err := alSplitHistorySnapshot(alAdmissionHistoryBodyForTest(r.t, r.series(plan)), alScrapeJob, alUnresolvedHistoryMetrics)
	if err != nil {
		r.t.Fatal(err)
	}
	return groups
}

// The window's timeline around the stored failed read. The first look comes
// after the read, and the window closes before the last scrape the reading
// holds.
type quietTimeline struct {
	firstLook, closed, queriedAt time.Time
	seen                         alQuietSeen
}

func newQuietTimeline(t *testing.T) quietTimeline {
	t.Helper()
	installation := quietInstallation()
	firing := alDelivery{Receiver: installation.receiver, Status: "firing", AlertName: alViewReadAlert,
		Labels:   map[string]string{"alertname": alViewReadAlert, "severity": "warning", "operator_namespace": quietNamespace, "operator_metrics_service": quietService},
		StartsAt: quietInstant(t, "2026-11-15T11:05:19.974Z"), ReceivedAt: quietInstant(t, "2026-11-15T11:05:24.985Z")}
	resolved := firing
	resolved.Status, resolved.EndsAt, resolved.ReceivedAt = "resolved", quietInstant(t, "2026-11-15T11:06:09.974Z"), quietInstant(t, "2026-11-15T11:06:14.985Z")
	return quietTimeline{
		firstLook: quietInstant(t, "2026-11-15T11:05:10Z"),
		closed:    quietInstant(t, "2026-11-15T11:06:22Z"),
		queriedAt: quietInstant(t, "2026-11-15T11:06:27Z"),
		seen: alQuietSeen{
			// The first poll saw the alert firing and nothing at the receiver,
			// as the failing poll of the run did.
			firing:        []time.Time{quietInstant(t, "2026-11-15T11:05:21Z"), quietInstant(t, "2026-11-15T11:05:45Z"), quietInstant(t, "2026-11-15T11:06:08Z")},
			notifications: []alDelivery{firing, resolved},
		},
	}
}

func quietInstallation() alQuietInstallation {
	return alQuietInstallation{receiver: "sink", namespace: quietNamespace, metricsService: quietService}
}

// read reads the history the row would read for this timeline: from the
// lookback the row computes from it, with the census judged from its first
// look.
func (r quietReading) read(plan quietPlan, timeline quietTimeline) (alQuietWindowHistory, error) {
	r.t.Helper()
	return alReadQuietWindowHistory(r.groups(plan), []string{quietLeader, quietFollower}, quietLeader, alQuietLookback(timeline.firstLook, timeline.seen), timeline.firstLook, timeline.queriedAt)
}

func TestAlQuietWindowAcceptsTheCIReadFailureOnlyWhenDeclared(t *testing.T) {
	t.Parallel()
	reading := newQuietReading(t)
	timeline := newQuietTimeline(t)
	// The plan reproduces the stored reading, so every row below changes only
	// what it names.
	if got := reading.series(quietDeclared()); !reflect.DeepEqual(got, reading.stored) {
		t.Fatal("the declared plan does not rebuild the stored reading")
	}
	if none, err := alNoActiveAlerts([]byte(quietAlertAnswer)); err != nil || none {
		t.Fatalf("the CI answer no longer reproduces the old row's refusal: %v, %v", none, err)
	}
	if firing, err := alQuietFiring([]byte(quietAlertAnswer)); err != nil || !firing {
		t.Fatalf("the CI answer is not the allowed alert: %v, %v", firing, err)
	}
	h, err := reading.read(quietDeclared(), timeline)
	want := []alDeclaredRead{{lower: quietScrape(t, "1794740709.670"), scrape: quietScrape(t, quietFailed)}}
	if err != nil || !reflect.DeepEqual(h.declared, want) || !h.through.Equal(quietScrape(t, "1794740783.831")) {
		t.Fatalf("history across the declared failed read = %+v, %v", h, err)
	}
	if err := alQuietWindowVerdict(h, timeline.seen, timeline.closed); err != nil {
		t.Fatalf("the declared failed read does not explain what the window saw: %v", err)
	}
	// Five failed reads in a row end inside the hold: from 11:05:14.670 to the
	// completion of the read at 11:05:39.670.
	five := quietDeclared()
	for n := 1; n < 5; n++ {
		five.failed = append(five.failed, quietScrapeAfter(n))
		five.rises[quietLeader] = append(five.rises[quietLeader], quietScrapeAfter(n))
	}
	if h, err := reading.read(five, timeline); err != nil || len(h.declared) != 5 || alQuietWindowVerdict(h, timeline.seen, timeline.closed) != nil {
		t.Fatalf("a run of failed reads the hold covers was refused: %+v, %v", h, err)
	}
	for _, row := range []struct {
		name   string
		mutate func(*quietPlan)
		want   string
		is     error
	}{
		{name: "undeclared gap", mutate: func(p *quietPlan) {
			p.rises, p.view = nil, map[string]string{quietFailed: "1"}
		}, want: "unresolved gauge omitted a native scrape"},
		{name: "counter did not rise", mutate: func(p *quietPlan) { p.rises = nil }, want: "unresolved gauge omitted a native scrape"},
		{name: "view synchronized at the failed read", mutate: func(p *quietPlan) { p.view = map[string]string{quietFailed: "1"} },
			want: "unresolved gauge omitted a native scrape"},
		{name: "counted by the follower", mutate: func(p *quietPlan) {
			p.rises = map[string][]string{quietFollower: {"1794740713.831"}}
		}, want: "unresolved gauge omitted a native scrape"},
		{name: "counted one scrape late", mutate: func(p *quietPlan) {
			p.rises = map[string][]string{quietLeader: {quietScrapeAfter(1)}}
		}, want: "unresolved gauge omitted a native scrape"},
		{name: "counter also rose at a read", mutate: func(p *quietPlan) {
			p.rises[quietLeader] = append(p.rises[quietLeader], quietScrapeAfter(6))
		}, want: "the leader counted a failed read in a scrape that read the state"},
		{name: "counter also rose on the follower", mutate: func(p *quietPlan) {
			p.rises[quietFollower] = []string{"1794740743.831"}
		}, want: "a follower published new state-read failures"},
		{name: "missed leader scrape", mutate: func(p *quietPlan) {
			p.missing = map[string][]string{quietLeader: {quietScrapeAfter(6)}}
		}, want: "missing scrapes"},
		{name: "missed follower scrape", mutate: func(p *quietPlan) {
			p.missing = map[string][]string{quietFollower: {"1794740743.831"}}
		}, want: "missing scrapes"},
		{name: "run as long as the hold", mutate: func(p *quietPlan) {
			// From 11:05:14.670 to the completion of the read at 11:05:44.670.
			for n := 1; n < 6; n++ {
				p.failed = append(p.failed, quietScrapeAfter(n))
				p.rises[quietLeader] = append(p.rises[quietLeader], quietScrapeAfter(n))
			}
		}, is: errAlReadGapOutlastedHold},
		{name: "census changed at the recovery read", mutate: func(p *quietPlan) {
			p.census = map[string]map[string]string{"migration": {quietScrapeAfter(1): "1"}}
		}, want: "the migration unresolved census changed during the quiet window"},
		{name: "census changed after the recovery read", mutate: func(p *quietPlan) {
			p.census = map[string]map[string]string{"schema": {quietScrapeAfter(9): "1"}}
		}, want: "the schema unresolved census changed during the quiet window"},
		{name: "census counted two records", mutate: func(p *quietPlan) {
			p.census = map[string]map[string]string{"schema": {quietScrapeAfter(1): "2"}}
		}, want: "unresolved gauge acquired another incident"},
		{name: "view unsynchronized at the recovery read", mutate: func(p *quietPlan) {
			p.view = map[string]string{quietScrapeAfter(1): "0"}
		}, want: "unresolved gauge was read without a synchronized view"},
		{name: "a third family", mutate: func(p *quietPlan) {
			for _, s := range reading.stored {
				if s.Metric["__name__"] == alUnresolvedMetric && s.Metric["family"] == "schema" {
					other := alAdmissionSeries{Metric: maps.Clone(s.Metric), Values: s.Values}
					other.Metric["family"] = "other"
					p.extra = append(p.extra, other)
				}
			}
		}, want: "the unresolved census must name the schema and migration families and no other"},
	} {
		t.Run(row.name, func(t *testing.T) {
			plan := quietDeclared()
			row.mutate(&plan)
			_, err := reading.read(plan, timeline)
			if row.is != nil && !errors.Is(err, row.is) || row.is == nil && (err == nil || !strings.Contains(err.Error(), row.want)) {
				t.Fatalf("a reading the leader did not declare as the window allows was accepted or misread: %v", err)
			}
		})
	}
	// A run still open when the window closes has not read the state since,
	// so the row waits for the read that ends it rather than judging the end.
	t.Run("run open at the close", func(t *testing.T) {
		plan := quietDeclared()
		for n := 11; n <= 14; n++ {
			plan.failed = append(plan.failed, quietScrapeAfter(n))
			plan.rises[quietLeader] = append(plan.rises[quietLeader], quietScrapeAfter(n))
		}
		h, err := reading.read(plan, timeline)
		if err != nil || len(h.declared) != 5 {
			t.Fatalf("an open run inside the hold was refused: %+v, %v", h, err)
		}
		if err := alQuietWindowVerdict(h, timeline.seen, timeline.closed); !errors.Is(err, errAlQuietWindowOpen) {
			t.Fatalf("a history that stops reading before the close was judged: %v", err)
		}
	})
	// The census before the first look is the preceding row's: here it reads
	// that row's incident from the lookback's baseline, 11:03:14.670, to the
	// last scrape before the first look, 11:05:09.670. The window's own census
	// is still zero throughout, and a first look before that last scrape makes
	// it the window's.
	t.Run("preceding incident in the lookback", func(t *testing.T) {
		plan := quietDeclared()
		plan.census = map[string]map[string]string{"migration": {}}
		for n := -24; n < 0; n++ {
			plan.census["migration"][quietScrapeAfter(n)] = "1"
		}
		open := 0
		for _, s := range reading.series(plan) {
			if s.Metric["__name__"] == alUnresolvedMetric && s.Metric["family"] == "migration" {
				for _, sample := range s.Values {
					if _, planned := plan.census["migration"][string(sample[0])]; planned && string(sample[1]) == `"1"` {
						open++
					}
				}
			}
		}
		if open != 24 || !quietScrape(t, quietScrapeAfter(-24)).Equal(quietScrape(t, "1794740594.670")) {
			t.Fatalf("the plan placed the preceding incident at %d scrapes, want 24 from the baseline", open)
		}
		h, err := reading.read(plan, timeline)
		if err != nil || alQuietWindowVerdict(h, timeline.seen, timeline.closed) != nil {
			t.Fatalf("an incident the preceding row resolved before the first look was refused: %+v, %v", h, err)
		}
		early := timeline
		early.firstLook = quietScrape(t, quietScrapeAfter(-1)).Add(-time.Millisecond)
		if _, err := reading.read(plan, early); err == nil || !strings.Contains(err.Error(), "the migration unresolved census changed during the quiet window") {
			t.Fatalf("a census the window itself read was not judged: %v", err)
		}
	})
}

// Single declared reads 55 seconds apart keep the alert firing from
// 11:03:24.974, before the window, to its resolution inside the window. The resolution carries that start, so the history has to reach the
// read that explains it, more than the resolution deadline before the first
// look, and through the preceding row's incident, which the census still
// reads at the history's baseline.
func TestAlQuietWindowReachesTheReadBehindAResolvedStart(t *testing.T) {
	t.Parallel()
	reading := newQuietReading(t)
	first, second, third := "1794740599.670", "1794740654.670", "1794740709.670"
	plan := quietPlan{failed: []string{first, second, third}, rises: map[string][]string{quietLeader: {first, second, third}}}
	timeline := newQuietTimeline(t)
	timeline.firstLook = quietInstant(t, "2026-11-15T11:05:20Z")
	resolved := timeline.seen.notifications[1]
	resolved.StartsAt, resolved.EndsAt, resolved.ReceivedAt = quietInstant(t, "2026-11-15T11:03:24.974Z"), quietInstant(t, "2026-11-15T11:06:04.974Z"), quietInstant(t, "2026-11-15T11:06:09.985Z")
	timeline.seen = alQuietSeen{firing: []time.Time{quietInstant(t, "2026-11-15T11:05:21Z"), quietInstant(t, "2026-11-15T11:06:00Z")}, notifications: []alDelivery{resolved}}
	if !quietInstallation().allows(resolved) {
		t.Fatal("the chain's resolution is not one the window allows")
	}
	lookback := alQuietLookback(timeline.firstLook, timeline.seen)
	if !lookback.Equal(resolved.StartsAt.Add(-alViewReadResolution)) || !lookback.Before(timeline.firstLook.Add(-alViewReadResolution)) {
		t.Fatalf("the lookback %s does not start from the resolution's start", lookback)
	}
	h, err := reading.read(plan, timeline)
	if err != nil || len(h.declared) != 3 || !h.declared[0].scrape.Equal(quietScrape(t, first)) {
		t.Fatalf("history across the chain = %+v, %v", h, err)
	}
	if err := alQuietWindowVerdict(h, timeline.seen, timeline.closed); err != nil {
		t.Fatalf("a chain of declared reads that kept the alert firing into the window was refused: %v", err)
	}
	// A chain too long for one range query is refused by the reader's bound.
	long := timeline
	long.seen.notifications = slices.Clone(long.seen.notifications)
	long.seen.notifications[0].StartsAt = timeline.queriedAt.Add(-alAdmissionHistoryWindow + alViewReadResolution)
	if _, err := reading.read(plan, long); err == nil || !strings.Contains(err.Error(), "does not cover its interval") {
		t.Fatalf("a lookback beyond one range query was read: %v", err)
	}
}

// What the lookback is for: the alert fires at the first look, and the read
// that explains it lies before the baseline of a history started there. Only
// the lookback keeps that read. The read here is the leader scrape at
// 11:04:14.670, and the first look comes either 15 seconds after it or at
// the verdict's own deadline, 105 seconds after the scrape before it.
func TestAlQuietWindowLookbackHoldsTheReadBeforeTheFirstLook(t *testing.T) {
	t.Parallel()
	reading := newQuietReading(t)
	failed := quietScrapeAfter(-12)
	plan := quietPlan{failed: []string{failed}, rises: map[string][]string{quietLeader: {failed}}}
	read := quietScrape(t, failed)
	for name, firstLook := range map[string]time.Time{
		"15 seconds after the read": read.Add(15*time.Second + 330*time.Millisecond),
		"at the deadline":           read.Add(-alScrapeInterval + alViewReadResolution),
	} {
		t.Run(name, func(t *testing.T) {
			timeline := newQuietTimeline(t)
			timeline.firstLook, timeline.closed = firstLook, quietInstant(t, "2026-11-15T11:06:05Z")
			timeline.seen = alQuietSeen{firing: []time.Time{firstLook}}
			// A history from the first look takes its baseline at the last
			// scrape two intervals before it, which has to be later than the
			// read.
			if next := read.Add(alScrapeInterval); next.After(firstLook.Add(-2 * alScrapeInterval)) {
				t.Fatal("the first look does not leave the read before the baseline of a history started there")
			}
			unread, err := alReadQuietWindowHistory(reading.groups(plan), []string{quietLeader, quietFollower}, quietLeader, firstLook, firstLook, timeline.queriedAt)
			if err != nil || len(unread.declared) != 0 || alQuietWindowVerdict(unread, timeline.seen, timeline.closed) == nil {
				t.Fatalf("a history from the first look still explained the alert: %+v, %v", unread, err)
			}
			h, err := reading.read(plan, timeline)
			if err != nil || len(h.declared) != 1 || !h.declared[0].scrape.Equal(read) {
				t.Fatalf("history behind the first look = %+v, %v", h, err)
			}
			if err := alQuietWindowVerdict(h, timeline.seen, timeline.closed); err != nil {
				t.Fatalf("the read before the first look does not explain the alert firing at it: %v", err)
			}
		})
	}
}

func TestAlQuietWindowVerdictNeedsADeclaredReadForEveryInstant(t *testing.T) {
	t.Parallel()
	reading := newQuietReading(t)
	timeline := newQuietTimeline(t)
	h, err := reading.read(quietDeclared(), timeline)
	if err != nil {
		t.Fatal(err)
	}
	lower := quietScrape(t, "1794740709.670")
	undeclared, err := reading.read(quietPlan{}, timeline)
	if err != nil || len(undeclared.declared) != 0 {
		t.Fatalf("the reading without its failed read = %+v, %v", undeclared, err)
	}
	if err := alQuietWindowVerdict(undeclared, alQuietSeen{}, timeline.closed); err != nil {
		t.Fatalf("a window that saw nothing needs no declared read: %v", err)
	}
	late := timeline.seen
	late.notifications = slices.Clone(late.notifications)
	late.notifications[1].ReceivedAt = lower.Add(alViewReadResolution)
	if err := alQuietWindowVerdict(h, late, timeline.closed); err != nil {
		t.Fatalf("a resolution received at the profile's deadline was refused: %v", err)
	}
	for _, row := range []struct {
		name    string
		history alQuietWindowHistory
		seen    func(alQuietSeen) alQuietSeen
		closed  time.Time
		is      error
	}{
		{name: "seen with no declared read", history: undeclared},
		{name: "firing seen at the scrape before the failure", seen: func(s alQuietSeen) alQuietSeen {
			s.firing = []time.Time{lower}
			return s
		}},
		{name: "firing seen after the deadline", seen: func(s alQuietSeen) alQuietSeen {
			s.firing = []time.Time{lower.Add(alViewReadResolution + time.Millisecond)}
			return s
		}},
		{name: "notification started before the failure", seen: func(s alQuietSeen) alQuietSeen {
			s.notifications = slices.Clone(s.notifications)
			s.notifications[0].StartsAt = lower
			return s
		}},
		{name: "resolution received after the deadline", seen: func(s alQuietSeen) alQuietSeen {
			s.notifications = slices.Clone(s.notifications)
			s.notifications[1].ReceivedAt = lower.Add(alViewReadResolution + time.Millisecond)
			return s
		}},
		{name: "resolution ended after the deadline", seen: func(s alQuietSeen) alQuietSeen {
			s.notifications = slices.Clone(s.notifications)
			s.notifications[1].EndsAt = lower.Add(alViewReadResolution + time.Millisecond)
			return s
		}},
		{name: "history short of the close", closed: h.through.Add(time.Nanosecond), is: errAlQuietWindowOpen},
	} {
		t.Run(row.name, func(t *testing.T) {
			history, seen, closed := h, timeline.seen, timeline.closed
			if row.history.through != (time.Time{}) {
				history = row.history
			}
			if row.seen != nil {
				seen = row.seen(seen)
			}
			if !row.closed.IsZero() {
				closed = row.closed
			}
			err := alQuietWindowVerdict(history, seen, closed)
			if row.is != nil && !errors.Is(err, row.is) || row.is == nil && (err == nil || !strings.Contains(err.Error(), "with no failed read the leader declared")) {
				t.Fatalf("what the window saw was accepted without a declared read to explain it: %v", err)
			}
		})
	}
}

func TestAlQuietFiringAllowsOnlyTheStateReadAlert(t *testing.T) {
	t.Parallel()
	answer := func(metrics ...string) []byte {
		var result []string
		for _, metric := range metrics {
			result = append(result, `{"metric":`+metric+`,"value":[1794740721,"1"]}`)
		}
		return []byte(`{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(result, ",") + `]}}`)
	}
	const allowed = `{"__name__":"ALERTS","alertname":"PtahOperatorUnresolvedViewReadFailures","alertstate":"firing","severity":"warning"}`
	if firing, err := alQuietFiring(answer()); err != nil || firing {
		t.Fatalf("an empty answer = %v, %v", firing, err)
	}
	if firing, err := alQuietFiring(answer(allowed)); err != nil || !firing {
		t.Fatalf("the state-read alert = %v, %v", firing, err)
	}
	// An answer that names another alert is an incident. The rest are
	// answers Prometheus did not give, which the row reports as such.
	for _, row := range []struct {
		name     string
		body     []byte
		incident bool
	}{
		{"unresolved Apply", answer(`{"__name__":"ALERTS","alertname":"PtahOperatorUnresolvedApply","alertstate":"firing","family":"migration","severity":"critical"}`), true},
		{"view not synchronized", answer(allowed, `{"__name__":"ALERTS","alertname":"PtahOperatorUnresolvedViewNotSynced","alertstate":"firing","severity":"warning"}`), true},
		{"operation stalled", answer(`{"__name__":"ALERTS","alertname":"PtahOperatorOperationStalled","alertstate":"firing","family":"schema","operation":"Resolve","severity":"warning"}`), true},
		{"state-read alert with a family", answer(`{"__name__":"ALERTS","alertname":"PtahOperatorUnresolvedViewReadFailures","alertstate":"firing","family":"schema","severity":"warning"}`), true},
		{"state-read alert at another severity", answer(`{"__name__":"ALERTS","alertname":"PtahOperatorUnresolvedViewReadFailures","alertstate":"firing","severity":"critical"}`), true},
		{"null result", []byte(`{"status":"success","data":{"resultType":"vector","result":null}}`), false},
		{"range answer", []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), false},
		{"error answer", []byte(`{"status":"error","data":{"resultType":"vector","result":[]}}`), false},
		{"not JSON", []byte(`ALERTS`), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := alQuietFiring(row.body)
			if err == nil || errors.Is(err, errAlQuietIncident) != row.incident {
				t.Fatalf("an answer the quiet window cannot allow was accepted or misread: %v", err)
			}
		})
	}
}

func TestAlQuietInstallationAllowsOnlyItsStateReadNotifications(t *testing.T) {
	t.Parallel()
	installation := quietInstallation()
	seen := newQuietTimeline(t).seen
	for _, d := range seen.notifications {
		if !installation.allows(d) {
			t.Fatalf("the state-read %s notification was refused", d.Status)
		}
	}
	for _, row := range []struct {
		name   string
		mutate func(*alDelivery)
	}{
		{"another installation", func(d *alDelivery) { d.Labels["operator_namespace"] = "ptah-system-other" }},
		{"another metrics Service", func(d *alDelivery) { d.Labels["operator_metrics_service"] = "other-metrics" }},
		{"no installation", func(d *alDelivery) { delete(d.Labels, "operator_namespace") }},
		{"another alert", func(d *alDelivery) {
			d.AlertName, d.Labels["alertname"] = alViewNotSynced, alViewNotSynced
		}},
		{"an alert named apart from its labels", func(d *alDelivery) { d.AlertName = alUnresolvedApply }},
		{"a label the alert does not carry", func(d *alDelivery) { d.Labels["family"] = "schema" }},
		{"another severity", func(d *alDelivery) { d.Labels["severity"] = "critical" }},
		{"another receiver", func(d *alDelivery) { d.Receiver = "other" }},
		{"unknown status", func(d *alDelivery) { d.Status = "suppressed" }},
		{"no start", func(d *alDelivery) { d.StartsAt = time.Time{} }},
		{"received before it started", func(d *alDelivery) { d.ReceivedAt = d.StartsAt.Add(-time.Millisecond) }},
	} {
		for _, original := range seen.notifications {
			t.Run(row.name+"/"+original.Status, func(t *testing.T) {
				d := original
				d.Labels = maps.Clone(original.Labels)
				row.mutate(&d)
				if installation.allows(d) {
					t.Fatal("a notification the quiet window does not allow was accepted")
				}
			})
		}
	}
	resolved := seen.notifications[1]
	resolved.EndsAt = time.Time{}
	if installation.allows(resolved) {
		t.Fatal("a resolution with no end was accepted")
	}
}

// Without a chain of declared reads, the window's native history runs from
// the resolution deadline before the first look to the read after the close,
// and one range query has to hold all of it. A chain moves the lookback
// earlier, and the reader refuses one that no longer fits.
func TestAlQuietWindowFitsOneNativeHistory(t *testing.T) {
	t.Parallel()
	if alViewReadResolution != 105*time.Second {
		t.Fatalf("the state-read alert resolves %s after its last failure, the profile says 105 seconds", alViewReadResolution)
	}
	if span := alViewReadResolution + alNegativeWindow + alDetectionSlack + alQuietCloseWait + 2*alScrapeInterval; span+time.Minute > alAdmissionHistoryWindow {
		t.Fatalf("the quiet window's history spans %s, more than a minute short of the %s one query reads", span, alAdmissionHistoryWindow)
	}
}
