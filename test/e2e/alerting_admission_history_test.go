package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type alAdmissionHistoryFixture struct {
	started, queriedAt      time.Time
	targets                 []string
	up, counters, durations []alAdmissionSeries
}

func alAdmissionHistoryFixtureForTest() alAdmissionHistoryFixture {
	f := alAdmissionHistoryFixture{
		started: time.Unix(1800000000, 0).UTC(), targets: []string{"10.0.0.1:6443", "10.0.0.2:6443"},
	}
	f.queriedAt = f.started.Add(16 * time.Second)
	for _, target := range f.targets {
		series := alAdmissionSeries{Metric: map[string]string{"__name__": "up", "job": alAPIServerJob, "instance": target}}
		for second := -10; second <= 15; second += 5 {
			series.Values = append(series.Values, alAdmissionHistorySampleForTest(f.started.Add(time.Duration(second)*time.Second), "1"))
		}
		f.up = append(f.up, series)
		duration := alAdmissionSeries{Metric: map[string]string{"__name__": "scrape_duration_seconds", "job": alAPIServerJob, "instance": target}}
		for second := -10; second <= 15; second += 5 {
			duration.Values = append(duration.Values, alAdmissionHistorySampleForTest(f.started.Add(time.Duration(second)*time.Second), "0.125"))
		}
		f.durations = append(f.durations, duration)
	}
	counter := alAdmissionSeries{Metric: map[string]string{
		"__name__": alAdmissionCounterMetric, "job": alAPIServerJob, "instance": f.targets[0],
		"name": alApprovalWebhook, "error_type": "calling_webhook_error", "operation": "UPDATE", "type": "validating", "rejection_code": "0",
	}}
	for second := -5; second <= 15; second += 5 {
		value := "4"
		if second >= 5 {
			value = "5"
		}
		counter.Values = append(counter.Values, alAdmissionHistorySampleForTest(f.started.Add(time.Duration(second)*time.Second), value))
	}
	f.counters = []alAdmissionSeries{counter}
	return f
}

func alAdmissionHistorySampleForTest(at time.Time, value string) []json.RawMessage {
	return []json.RawMessage{json.RawMessage(fmt.Sprintf("%.3f", float64(at.UnixMilli())/1000)),
		json.RawMessage(fmt.Sprintf("%q", value))}
}

func alAdmissionHistoryBodyForTest(t *testing.T, series []alAdmissionSeries) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "success", "data": map[string]any{"resultType": "matrix", "result": series},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (f alAdmissionHistoryFixture) read(t *testing.T, previous *alAdmissionHistory) (alAdmissionHistory, error) {
	t.Helper()
	return alReadAdmissionHistory(alAdmissionHistoryBodyForTest(t, f.counters), alAdmissionHistoryBodyForTest(t, f.up), alAdmissionHistoryBodyForTest(t, f.durations),
		f.targets, f.started, f.queriedAt, previous)
}

func TestAlAdmissionHistoryDatesTheNativeIncrement(t *testing.T) {
	t.Parallel()
	f := alAdmissionHistoryFixtureForTest()
	reading, err := f.read(t, nil)
	if err != nil || !reading.approvalIncreased || !reading.lastLower.Equal(f.started) ||
		!reading.lastUpper.Equal(f.started.Add(5*time.Second+125*time.Millisecond)) || !reading.scrapedThrough.Equal(f.started.Add(15*time.Second)) {
		t.Fatalf("native admission interval = %+v, %v", reading, err)
	}
	// A different webhook on the other API server is also part of the chart's
	// admission incident. Its later rejection must be included in recovery.
	other := alAdmissionHistoryFixtureForTest().counters[0]
	other.Metric["instance"], other.Metric["name"] = f.targets[1], "mschema.operator.ptah.run"
	for i := range other.Values {
		value := "7"
		if i == len(other.Values)-1 {
			value = "8"
		}
		other.Values[i][1] = json.RawMessage(fmt.Sprintf("%q", value))
	}
	f.counters = append(f.counters, other)
	reading, err = f.read(t, nil)
	if err != nil || !reading.lastLower.Equal(f.started.Add(10*time.Second)) || !reading.lastUpper.Equal(f.started.Add(15*time.Second+125*time.Millisecond)) {
		t.Fatalf("the other API server's later rejection was lost: %+v, %v", reading, err)
	}
}

func TestAlAdmissionHistoryNeverInventsAnAbsentZero(t *testing.T) {
	t.Parallel()
	for _, values := range [][]string{nil, {"1"}, {"1", "1"}, {"0", "0"}} {
		f := alAdmissionHistoryFixtureForTest()
		f.counters = nil
		if len(values) != 0 {
			series := alAdmissionHistoryFixtureForTest().counters[0]
			series.Values = nil
			for i, value := range values {
				at := f.started.Add(15*time.Second + time.Duration(i-len(values)+1)*5*time.Second)
				series.Values = append(series.Values, alAdmissionHistorySampleForTest(at, value))
			}
			f.counters = []alAdmissionSeries{series}
		}
		reading, err := f.read(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		if reading.approvalIncreased || !reading.lastLower.IsZero() ||
			alAdmissionResolutionWithinBounds(reading, f.queriedAt, f.started) {
			t.Fatalf("absent, first, or unchanged values %v supplied a passing increase: %+v", values, reading)
		}
	}
	f := alAdmissionHistoryFixtureForTest()
	f.counters[0].Values = [][]json.RawMessage{
		alAdmissionHistorySampleForTest(f.started.Add(10*time.Second), "1"),
		alAdmissionHistorySampleForTest(f.started.Add(15*time.Second), "2"),
	}
	reading, err := f.read(t, nil)
	if err != nil || !reading.approvalIncreased || !reading.lastLower.Equal(f.started.Add(10*time.Second)) {
		t.Fatalf("a real increase after the first nonzero sample was refused: %+v, %v", reading, err)
	}
}

func TestAlAdmissionHistoryRefusesIncompleteOrChangedReadings(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*alAdmissionHistoryFixture){
		"no API servers":                     func(f *alAdmissionHistoryFixture) { f.targets = nil },
		"duplicate expected API":             func(f *alAdmissionHistoryFixture) { f.targets[1] = f.targets[0] },
		"missing API server":                 func(f *alAdmissionHistoryFixture) { f.up = f.up[:1] },
		"duplicate API server":               func(f *alAdmissionHistoryFixture) { f.up[1] = f.up[0] },
		"unhealthy native scrape":            func(f *alAdmissionHistoryFixture) { f.up[0].Values[3][1] = json.RawMessage(`"0"`) },
		"missing scrape duration target":     func(f *alAdmissionHistoryFixture) { f.durations = f.durations[:1] },
		"duplicate duration target":          func(f *alAdmissionHistoryFixture) { f.durations[1] = f.durations[0] },
		"another duration metric":            func(f *alAdmissionHistoryFixture) { f.durations[0].Metric["__name__"] = "other_seconds" },
		"duration beyond configured timeout": func(f *alAdmissionHistoryFixture) { f.durations[0].Values[3][1] = json.RawMessage(`"4.1"`) },
		"scrape timestamp and duration disagree": func(f *alAdmissionHistoryFixture) {
			f.durations[0].Values[3][0] = json.RawMessage(fmt.Sprintf("%.3f", float64(f.started.Add(4*time.Second).UnixMilli())/1000))
		},
		"skip one native scrape": func(f *alAdmissionHistoryFixture) {
			f.up[0].Values = append(f.up[0].Values[:3], f.up[0].Values[4:]...)
		},
		"no pre-fault coverage": func(f *alAdmissionHistoryFixture) { f.up[0].Values = f.up[0].Values[3:] },
		"stale target":          func(f *alAdmissionHistoryFixture) { f.up[0].Values = f.up[0].Values[:4] },
		"other scrape job":      func(f *alAdmissionHistoryFixture) { f.up[0].Metric["job"] = alScrapeJob },
		"other up metric":       func(f *alAdmissionHistoryFixture) { f.up[0].Metric["__name__"] = "some_gauge" },
		"counter reset":         func(f *alAdmissionHistoryFixture) { f.counters[0].Values[3][1] = json.RawMessage(`"1"`) },
		"duplicate counter":     func(f *alAdmissionHistoryFixture) { f.counters = append(f.counters, f.counters[0]) },
		"wrong counter":         func(f *alAdmissionHistoryFixture) { f.counters[0].Metric["__name__"] = "another_total" },
		"wrong counter job":     func(f *alAdmissionHistoryFixture) { f.counters[0].Metric["job"] = alScrapeJob },
		"unobserved API server": func(f *alAdmissionHistoryFixture) { f.counters[0].Metric["instance"] = "10.0.0.9:6443" },
		"another webhook":       func(f *alAdmissionHistoryFixture) { f.counters[0].Metric["name"] = "unrelated.example.com" },
		"policy denial":         func(f *alAdmissionHistoryFixture) { f.counters[0].Metric["error_type"] = "no_error" },
		"extrapolated value":    func(f *alAdmissionHistoryFixture) { f.counters[0].Values[3][1] = json.RawMessage(`"2.5"`) },
		"negative count":        func(f *alAdmissionHistoryFixture) { f.counters[0].Values[3][1] = json.RawMessage(`"-1"`) },
		"not a number":          func(f *alAdmissionHistoryFixture) { f.counters[0].Values[3][1] = json.RawMessage(`"NaN"`) },
		"infinity":              func(f *alAdmissionHistoryFixture) { f.counters[0].Values[3][1] = json.RawMessage(`"+Inf"`) },
		"fractional up":         func(f *alAdmissionHistoryFixture) { f.up[0].Values[3][1] = json.RawMessage(`"0.5"`) },
		"unsafe integer precision": func(f *alAdmissionHistoryFixture) {
			f.counters[0].Values[3][1] = json.RawMessage(`"9007199254740992"`)
		},
		"missing values":  func(f *alAdmissionHistoryFixture) { f.counters[0].Values = nil },
		"stale counter":   func(f *alAdmissionHistoryFixture) { f.counters[0].Values = f.counters[0].Values[:3] },
		"duplicate stamp": func(f *alAdmissionHistoryFixture) { f.counters[0].Values[3][0] = f.counters[0].Values[2][0] },
		"future stamp": func(f *alAdmissionHistoryFixture) {
			f.counters[0].Values[4] = alAdmissionHistorySampleForTest(f.queriedAt.Add(time.Second), "5")
		},
		"histogram input": func(f *alAdmissionHistoryFixture) {
			f.counters[0].Histograms = []json.RawMessage{json.RawMessage(`[]`)}
		},
		"missing fault start":        func(f *alAdmissionHistoryFixture) { f.started = time.Time{} },
		"observation precedes fault": func(f *alAdmissionHistoryFixture) { f.queriedAt = f.started.Add(-time.Second) },
		"fault left history window":  func(f *alAdmissionHistoryFixture) { f.started = f.queriedAt.Add(-15 * time.Minute) },
	} {
		t.Run(name, func(t *testing.T) {
			f := alAdmissionHistoryFixtureForTest()
			mutate(&f)
			if reading, err := f.read(t, nil); err == nil {
				t.Fatalf("invalid native history passed: %+v", reading)
			}
		})
	}
	f := alAdmissionHistoryFixtureForTest()
	goodUp := alAdmissionHistoryBodyForTest(t, f.up)
	for _, body := range []string{
		`{}`, `{"status":"error","data":{"resultType":"matrix","result":[]}}`,
		`{"status":"success","warnings":["partial"],"data":{"resultType":"matrix","result":[]}}`,
		`{"status":"success","infos":["omitted"],"data":{"resultType":"matrix","result":[]}}`,
		`{"status":"success","error":"partial","data":{"resultType":"matrix","result":[]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[]}}`,
	} {
		if reading, err := alReadAdmissionHistory([]byte(body), goodUp, alAdmissionHistoryBodyForTest(t, f.durations), f.targets, f.started, f.queriedAt, nil); err == nil {
			t.Fatalf("incomplete query passed: %s, %+v", body, reading)
		}
	}
}

func TestAlAdmissionHistoryPreservesObservedSeriesAndBounds(t *testing.T) {
	t.Parallel()
	f := alAdmissionHistoryFixtureForTest()
	previous, err := f.read(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*alAdmissionHistoryFixture){
		"disappeared counter": func(f *alAdmissionHistoryFixture) { f.counters = nil },
		"changed labels":      func(f *alAdmissionHistoryFixture) { f.counters[0].Metric["operation"] = "CREATE" },
		"older response": func(f *alAdmissionHistoryFixture) {
			f.counters[0].Values = f.counters[0].Values[:len(f.counters[0].Values)-1]
		},
		"increment dropped": func(f *alAdmissionHistoryFixture) { f.counters[0].Values = f.counters[0].Values[2:] },
	} {
		t.Run(name, func(t *testing.T) {
			next := alAdmissionHistoryFixtureForTest()
			mutate(&next)
			if reading, err := next.read(t, &previous); err == nil {
				t.Fatalf("observed series or interval was lost: %+v", reading)
			}
		})
	}
}

func TestAlAdmissionResolutionUsesTheFrozenWindowAndEarlierEdge(t *testing.T) {
	t.Parallel()
	if alAdmissionWindow != 5*time.Minute || alAdmissionRecovery != 345*time.Second {
		t.Fatal("the proof accelerated or extended the frozen admission target")
	}
	at := time.Unix(1800000000, 0).UTC()
	reading := alAdmissionHistory{
		lastLower: at, lastUpper: at.Add(5 * time.Second), approvalIncreased: true,
		scrapedThrough: at.Add(400 * time.Second),
	}
	for _, row := range []struct {
		name       string
		seconds    int
		wantWithin bool
	}{
		{"whole window", 300, true}, {"within delivery slack", 320, true}, {"deadline", 345, true},
		{"before whole window", 299, false}, {"late but inside later edge deadline", 346, false},
		{"late poll cannot grant time", 390, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := alAdmissionResolutionWithinBounds(reading, at.Add(time.Duration(row.seconds)*time.Second), at.Add(time.Minute)); got != row.wantWithin {
				t.Fatalf("received after %ds: within=%t, want %t", row.seconds, got, row.wantWithin)
			}
		})
	}
	for name, mutate := range map[string]func(*alAdmissionHistory){
		"no observed increase":  func(r *alAdmissionHistory) { r.approvalIncreased = false },
		"no lower bound":        func(r *alAdmissionHistory) { r.lastLower = time.Time{} },
		"inverted interval":     func(r *alAdmissionHistory) { r.lastUpper = at.Add(-time.Second) },
		"unobserved resolution": func(r *alAdmissionHistory) { r.scrapedThrough = at.Add(310 * time.Second) },
	} {
		r := reading
		mutate(&r)
		if alAdmissionResolutionWithinBounds(r, at.Add(320*time.Second), at.Add(time.Minute)) {
			t.Errorf("%s passed recovery", name)
		}
	}
	if alAdmissionResolutionWithinBounds(reading, at.Add(320*time.Second), at.Add(321*time.Second)) {
		t.Fatal("a resolution before restoration supplied recovery evidence")
	}
}
