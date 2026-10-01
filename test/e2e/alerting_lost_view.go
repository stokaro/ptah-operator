package e2e

import (
	"errors"
	"maps"
	"slices"
	"time"
)

type alLostViewHistory struct {
	transition, firstFailure, fired, through time.Time
}

// ALERTS records the rule's native evaluations. Merge pending and firing
// samples so a missing evaluation or a reset cannot hide inside two series.
// A failed scrape of the original leader, if present, starts the delivery
// deadline earlier than the first evaluation of the unavailable view.
func alLostViewFiring(alertBody, upBody []byte, pods []string, leader string, started, queriedAt time.Time) (alLostViewHistory, error) {
	r := alLostViewHistory{}
	if started.IsZero() || !queriedAt.After(started) || queriedAt.Sub(started)+2*alScrapeInterval >= alAdmissionHistoryWindow {
		return r, errors.New("lost-view history does not cover the fault")
	}
	series, err := alAdmissionNativeMatrix(alertBody)
	if err != nil {
		return r, err
	}
	type evaluation struct {
		at    time.Time
		state string
	}
	var evaluations []evaluation
	states := map[string]bool{}
	var labels map[string]string
	for _, item := range series {
		state := item.Metric["alertstate"]
		if item.Metric["__name__"] != "ALERTS" || item.Metric["alertname"] != alViewNotSynced || (state != "pending" && state != "firing") || states[state] {
			return r, errors.New("lost-view history has unexpected or duplicate alert series")
		}
		states[state] = true
		identity := maps.Clone(item.Metric)
		delete(identity, "alertstate")
		if labels != nil && !maps.Equal(labels, identity) {
			return r, errors.New("lost-view alert identity changed between states")
		}
		labels = identity
		// An earlier, resolved incident can still be in the query window.
		// Check ordering globally and continuity only in this fault's interval.
		samples, err := alNativeSamples(item, queriedAt, queriedAt, true, false)
		if err != nil {
			return r, err
		}
		for _, sample := range samples {
			if sample.at.Before(started) {
				continue
			}
			if sample.value != 1 {
				return r, errors.New("lost-view alert has a non-active sample")
			}
			evaluations = append(evaluations, evaluation{sample.at, state})
		}
	}
	slices.SortFunc(evaluations, func(a, b evaluation) int { return a.at.Compare(b.at) })
	if len(evaluations) < 2 || evaluations[0].state != "pending" {
		return r, errors.New("lost-view history has no pending transition")
	}
	r.transition = evaluations[0].at
	for i, sample := range evaluations {
		if i > 0 && (!sample.at.After(evaluations[i-1].at) || sample.at.Sub(evaluations[i-1].at) > alAdmissionSampleGap) {
			return r, errors.New("lost-view history missed or duplicated an evaluation")
		}
		if sample.state == "pending" && !r.fired.IsZero() {
			return r, errors.New("lost-view incident restarted after firing")
		}
		if sample.state == "firing" && r.fired.IsZero() {
			r.fired = sample.at
		}
		r.through = sample.at
	}
	if r.fired.Before(r.transition.Add(alViewUnsyncedFor)) || r.fired.After(r.transition.Add(alViewUnsyncedFor+alAdmissionSampleGap)) || queriedAt.Sub(r.through) > alAdmissionSampleGap {
		return r, errors.New("lost-view firing lacks its complete frozen pending interval")
	}
	up, err := alAdmissionNativeMatrix(upBody)
	if err != nil {
		return r, err
	}
	wanted := map[string]bool{}
	for _, name := range pods {
		if name == "" || wanted[name] {
			return r, errors.New("lost-view fault needs distinct original Pods")
		}
		wanted[name] = true
	}
	if len(wanted) < 2 || !wanted[leader] || len(up) != len(wanted) {
		return r, errors.New("lost-view fault omitted an original manager")
	}
	seen := map[string]bool{}
	for _, item := range up {
		pod := item.Metric["pod"]
		if item.Metric["__name__"] != "up" || item.Metric["job"] != alScrapeJob || item.Metric["instance"] == "" || !wanted[pod] || seen[pod] {
			return r, errors.New("lost-view fault has an unexpected manager target")
		}
		seen[pod] = true
		samples, err := alNativeSamples(item, started, queriedAt, true, false)
		if err != nil {
			return r, err
		}
		var baseline time.Time
		failed := false
		for _, sample := range samples {
			if sample.value != 0 && sample.value != 1 {
				return r, errors.New("invalid lost-view scrape health")
			}
			if sample.at.Before(started) {
				if sample.value == 1 {
					baseline = sample.at
				} else {
					baseline = time.Time{}
				}
				continue
			}
			if sample.value == 0 {
				failed = true
				if pod == leader && r.firstFailure.IsZero() {
					r.firstFailure = sample.at
				}
			} else if failed || pod == leader && !sample.at.Before(r.transition) {
				return r, errors.New("a removed manager remained healthy during view loss")
			}
		}
		if baseline.IsZero() || started.Sub(baseline) > alAdmissionSampleGap {
			return r, errors.New("lost-view fault has no fresh healthy original target")
		}
	}
	return r, nil
}

func alLostViewDelivered(d alDelivery, r alLostViewHistory) bool {
	start := r.transition
	if !r.firstFailure.IsZero() && r.firstFailure.Before(start) {
		start = r.firstFailure
	}
	return !r.transition.IsZero() && !r.fired.IsZero() && !d.StartsAt.Before(r.fired) &&
		!d.ReceivedAt.Before(d.StartsAt) && !d.ReceivedAt.After(start.Add(alViewUnsyncedFor+alDetectionSlack)) && !r.through.Before(d.ReceivedAt)
}

// Replacement Pods have new identities and therefore new series. Every
// matching up/duration sample must remain healthy; the first synchronized
// sample of their elected leader starts the resolution allowance.
func alLostViewRecovery(viewBody, upBody, durationBody []byte, pods []string, leader string, restored, queriedAt time.Time) (alScrapeHistory, error) {
	r := alScrapeHistory{scrapedThrough: queriedAt}
	if restored.IsZero() || !queriedAt.After(restored) || queriedAt.Sub(restored)+2*alScrapeInterval >= alAdmissionHistoryWindow {
		return r, errors.New("lost-view recovery is outside the retained window")
	}
	wanted := map[string]bool{}
	for _, name := range pods {
		if name == "" || wanted[name] {
			return r, errors.New("lost-view recovery needs distinct replacement Pods")
		}
		wanted[name] = true
	}
	if len(wanted) < 2 || !wanted[leader] {
		return r, errors.New("lost-view recovery needs the new leader and followers")
	}
	type reading struct {
		instance string
		samples  []alAdmissionSample
	}
	read := func(body []byte, metric string, integer bool) (map[string]reading, error) {
		series, err := alAdmissionNativeMatrix(body)
		if err != nil {
			return nil, err
		}
		result := map[string]reading{}
		instances := map[string]bool{}
		for _, item := range series {
			pod, instance := item.Metric["pod"], item.Metric["instance"]
			if item.Metric["__name__"] != metric || item.Metric["job"] != alScrapeJob || !wanted[pod] || instance == "" || result[pod].instance != "" || instances[instance] {
				return nil, errors.New("lost-view recovery has an unexpected or duplicate target")
			}
			samples, err := alAdmissionNativeSamples(item, restored, queriedAt, integer)
			if err != nil {
				return nil, err
			}
			if samples[0].at.Before(restored) {
				return nil, errors.New("a replacement target predates recovery")
			}
			result[pod] = reading{instance, samples}
			instances[instance] = true
		}
		if len(result) != len(wanted) {
			return nil, errors.New("lost-view recovery omitted a manager")
		}
		return result, nil
	}
	view, err := read(viewBody, "ptah_operator_unresolved_view_synced", true)
	if err != nil {
		return r, err
	}
	up, err := read(upBody, "up", true)
	if err != nil {
		return r, err
	}
	durations, err := read(durationBody, "scrape_duration_seconds", false)
	if err != nil {
		return r, err
	}
	for pod, gauge := range view {
		health, duration := up[pod], durations[pod]
		if health.instance != gauge.instance || duration.instance != gauge.instance || len(health.samples) != len(gauge.samples) || len(duration.samples) != len(gauge.samples) {
			return r, errors.New("lost-view recovery lost matching native scrapes")
		}
		for i, sample := range gauge.samples {
			if !health.samples[i].at.Equal(sample.at) || !duration.samples[i].at.Equal(sample.at) || health.samples[i].value != 1 || duration.samples[i].value > alScrapeTimeout.Seconds() || (sample.value != 0 && sample.value != 1) {
				return r, errors.New("lost-view recovery has an unhealthy or mismatched scrape")
			}
			if pod != leader && sample.value != 0 {
				return r, errors.New("a replacement follower claimed the synchronized view")
			}
			if pod == leader {
				if sample.value == 1 && r.recovered.IsZero() {
					r.recovered = sample.at
				}
				if sample.value == 0 && !r.recovered.IsZero() {
					return r, errors.New("the recovered leader lost its view again")
				}
			}
		}
		r.scrapedThrough = earlierTime(r.scrapedThrough, gauge.samples[len(gauge.samples)-1].at)
	}
	if r.recovered.IsZero() {
		return r, errors.New("no replacement leader has a synchronized scrape")
	}
	r.leaderInstance = view[leader].instance
	return r, nil
}
