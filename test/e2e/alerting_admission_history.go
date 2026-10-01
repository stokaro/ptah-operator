package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	alAdmissionCounterMetric = "apiserver_admission_webhook_rejection_count"
	alAdmissionHistoryWindow = 15 * time.Minute
	// One missed five-second scrape leaves a ten-second gap. Permit clock
	// jitter, but never use such a missing observation as a healthy reading.
	alAdmissionSampleGap = alScrapeInterval + time.Second
)

var alAdmissionCounterHistory = fmt.Sprintf(`%s{job=%q,name=~".*operator\\.ptah\\.run",error_type="calling_webhook_error"}[%ds]`,
	alAdmissionCounterMetric, alAPIServerJob, int(alAdmissionHistoryWindow/time.Second))

var alAdmissionUpHistory = fmt.Sprintf(`up{job=%q}[%ds]`, alAPIServerJob, int(alAdmissionHistoryWindow/time.Second))
var alAdmissionScrapeHistory = fmt.Sprintf(`scrape_duration_seconds{job=%q}[%ds]`, alAPIServerJob, int(alAdmissionHistoryWindow/time.Second))

type alAdmissionSample struct {
	at    time.Time
	value float64
}

type alAdmissionSeries struct {
	Metric     map[string]string   `json:"metric"`
	Values     [][]json.RawMessage `json:"values"`
	Histograms []json.RawMessage   `json:"histograms"`
}

type alAdmissionCounter struct {
	at    time.Time
	value float64
}

// Native counter samples bracket an increment between the preceding scrape
// and completion of the scrape that includes it. The last real rejection is no earlier
// than lastLower. Starting its deadline there is conservative; starting at
// lastUpper or at the poll would grant time the profile did not allow.
type alAdmissionHistory struct {
	lastLower         time.Time
	lastUpper         time.Time
	scrapedThrough    time.Time
	approvalIncreased bool
	counters          map[string]alAdmissionCounter
}

func alAdmissionNativeMatrix(body []byte) ([]alAdmissionSeries, error) {
	var response struct {
		Status   string
		Error    string
		Warnings []string
		Infos    []string
		Data     struct {
			ResultType string
			Result     []alAdmissionSeries
		}
	}
	if json.Unmarshal(body, &response) != nil || response.Status != "success" || response.Error != "" ||
		len(response.Warnings) != 0 || len(response.Infos) != 0 || response.Data.ResultType != "matrix" {
		return nil, errors.New("admission history needs a complete native range-vector response")
	}
	return response.Data.Result, nil
}

func alAdmissionNativeSamples(series alAdmissionSeries, since, queriedAt time.Time, integer bool) ([]alAdmissionSample, error) {
	return alNativeSamples(series, since, queriedAt, integer, true)
}

// Ended series are expected when an alert changes state or a Pod disappears.
// Their callers must check coverage against the native transition they prove.
func alNativeSamples(series alAdmissionSeries, since, queriedAt time.Time, integer, fresh bool) ([]alAdmissionSample, error) {
	if len(series.Values) == 0 || len(series.Histograms) != 0 {
		return nil, errors.New("admission history needs native float samples")
	}
	samples := make([]alAdmissionSample, 0, len(series.Values))
	for _, raw := range series.Values {
		var timestamp float64
		var number string
		if len(raw) != 2 || json.Unmarshal(raw[0], &timestamp) != nil || timestamp <= 0 ||
			math.IsInf(timestamp, 0) || math.IsNaN(timestamp) || json.Unmarshal(raw[1], &number) != nil {
			return nil, errors.New("admission history has an unreadable native sample")
		}
		seconds, fraction := math.Modf(timestamp)
		at := time.Unix(int64(seconds), int64(math.Round(fraction*float64(time.Second)))).UTC()
		value, err := strconv.ParseFloat(number, 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 || integer && math.Trunc(value) != value || value >= 1<<53 ||
			at.After(queriedAt) || !at.After(queriedAt.Add(-alAdmissionHistoryWindow)) {
			return nil, errors.New("admission history has an invalid or out-of-window sample")
		}
		if len(samples) != 0 {
			previous := samples[len(samples)-1]
			if !at.After(previous.at) || at.After(since) && at.Sub(previous.at) > alAdmissionSampleGap {
				return nil, errors.New("admission history has duplicate, unordered, or missing scrapes")
			}
		}
		samples = append(samples, alAdmissionSample{at: at, value: value})
	}
	if fresh && queriedAt.Sub(samples[len(samples)-1].at) > alAdmissionSampleGap {
		return nil, errors.New("admission history ended before a fresh scrape")
	}
	return samples, nil
}

func alReadAdmissionHistory(counterBody, upBody, durationBody []byte, expected []string, started, queriedAt time.Time, previous *alAdmissionHistory) (alAdmissionHistory, error) {
	reading := alAdmissionHistory{scrapedThrough: queriedAt, counters: map[string]alAdmissionCounter{}}
	if started.IsZero() || queriedAt.Before(started) || queriedAt.Sub(started)+2*alScrapeInterval >= alAdmissionHistoryWindow {
		return reading, errors.New("admission history does not cover the fault interval")
	}
	wanted := map[string]bool{}
	for _, target := range expected {
		if target == "" || wanted[target] {
			return reading, errors.New("admission history needs distinct API server targets")
		}
		wanted[target] = true
	}
	up, err := alAdmissionNativeMatrix(upBody)
	if err != nil {
		return reading, err
	}
	if len(wanted) == 0 || len(up) != len(wanted) {
		return reading, errors.New("admission history does not cover every API server")
	}
	since := started.Add(-2 * alScrapeInterval)
	durationSeries, err := alAdmissionNativeMatrix(durationBody)
	if err != nil {
		return reading, err
	}
	if len(durationSeries) != len(wanted) {
		return reading, errors.New("admission history lacks an API server's scrape durations")
	}
	durations := map[string]map[int64]time.Duration{}
	for _, series := range durationSeries {
		instance := series.Metric["instance"]
		if series.Metric["__name__"] != "scrape_duration_seconds" || series.Metric["job"] != alAPIServerJob ||
			!wanted[instance] || durations[instance] != nil {
			return reading, errors.New("admission history has an unexpected scrape-duration series")
		}
		samples, err := alAdmissionNativeSamples(series, since, queriedAt, false)
		if err != nil {
			return reading, err
		}
		durations[instance] = map[int64]time.Duration{}
		for _, sample := range samples {
			if sample.value > alScrapeTimeout.Seconds() {
				return reading, errors.New("an admission scrape exceeded its configured timeout")
			}
			durations[instance][sample.at.UnixNano()] = time.Duration(math.Round(sample.value * float64(time.Second)))
		}
	}
	completedAt := func(instance string, at time.Time) (time.Time, bool) {
		duration, exists := durations[instance][at.UnixNano()]
		return at.Add(duration), exists
	}
	seen := map[string]bool{}
	for _, series := range up {
		instance := series.Metric["instance"]
		if series.Metric["__name__"] != "up" || series.Metric["job"] != alAPIServerJob || !wanted[instance] || seen[instance] {
			return reading, errors.New("admission history has an unexpected or repeated API server")
		}
		samples, err := alAdmissionNativeSamples(series, since, queriedAt, true)
		if err != nil {
			return reading, err
		}
		if samples[0].at.After(since) || len(samples) < 2 {
			return reading, errors.New("admission history lacks a pre-fault API server scrape")
		}
		for _, sample := range samples {
			if _, exists := completedAt(instance, sample.at); !exists {
				return reading, errors.New("admission history lost the exact scrape's duration")
			}
			if !sample.at.Before(since) && sample.value != 1 {
				return reading, errors.New("admission history includes an unhealthy API server scrape")
			}
		}
		reading.scrapedThrough = earlierTime(reading.scrapedThrough, samples[len(samples)-1].at)
		seen[instance] = true
	}
	counters, err := alAdmissionNativeMatrix(counterBody)
	if err != nil {
		return reading, err
	}
	for _, series := range counters {
		if series.Metric["__name__"] != alAdmissionCounterMetric || series.Metric["job"] != alAPIServerJob ||
			!wanted[series.Metric["instance"]] || !strings.HasSuffix(series.Metric["name"], ".operator.ptah.run") ||
			series.Metric["error_type"] != "calling_webhook_error" {
			return reading, errors.New("admission history includes another counter or webhook scope")
		}
		keyBytes, err := json.Marshal(series.Metric)
		if err != nil {
			return reading, errors.New("admission history has unreadable series labels")
		}
		key := string(keyBytes)
		if _, duplicate := reading.counters[key]; duplicate {
			return reading, errors.New("admission history repeats a counter series")
		}
		samples, err := alAdmissionNativeSamples(series, since, queriedAt, true)
		if err != nil {
			return reading, err
		}
		latest := samples[len(samples)-1]
		reading.counters[key] = alAdmissionCounter{at: latest.at, value: latest.value}
		reading.scrapedThrough = earlierTime(reading.scrapedThrough, latest.at)
		// A series first seen at one is not an observed zero-to-one increase.
		// Its first value supplies only an upper bound. A later native increase
		// is required before this reading can support the recovery deadline.
		if samples[0].value > 0 && !samples[0].at.Before(started) {
			completed, exists := completedAt(series.Metric["instance"], samples[0].at)
			if !exists {
				return reading, errors.New("admission history lost the first counter scrape's duration")
			}
			reading.lastUpper = laterTime(reading.lastUpper, completed)
		}
		for i := 1; i < len(samples); i++ {
			before, after := samples[i-1], samples[i]
			if after.at.Before(since) {
				continue
			}
			if after.value < before.value {
				return reading, errors.New("admission history contains a counter reset")
			}
			if after.at.Before(started) || after.value == before.value {
				continue
			}
			completed, exists := completedAt(series.Metric["instance"], after.at)
			if !exists {
				return reading, errors.New("admission history lost an increased counter's scrape duration")
			}
			reading.lastLower = laterTime(reading.lastLower, before.at)
			reading.lastUpper = laterTime(reading.lastUpper, completed)
			if series.Metric["name"] == alApprovalWebhook {
				reading.approvalIncreased = true
			}
		}
	}
	if previous != nil {
		for key, before := range previous.counters {
			after, exists := reading.counters[key]
			if !exists || after.at.Before(before.at) || after.value < before.value {
				return reading, errors.New("admission history lost or rewound an observed counter")
			}
		}
		if reading.lastLower.Before(previous.lastLower) || reading.lastUpper.Before(previous.lastUpper) ||
			reading.scrapedThrough.Before(previous.scrapedThrough) {
			return reading, errors.New("admission history lost its observed rejection interval")
		}
	}
	return reading, nil
}

func alAdmissionResolutionWithinBounds(reading alAdmissionHistory, receivedAt, restoredAt time.Time) bool {
	return reading.approvalIncreased && !reading.lastLower.IsZero() && !reading.lastUpper.Before(reading.lastLower) &&
		!receivedAt.Before(restoredAt) && !receivedAt.Before(reading.lastLower.Add(alAdmissionWindow)) &&
		!receivedAt.After(reading.lastLower.Add(alAdmissionRecovery)) && !reading.scrapedThrough.Before(receivedAt)
}

func earlierTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
