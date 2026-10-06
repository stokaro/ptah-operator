package e2e

import (
	"errors"
	"time"
)

type alScrapeHistory struct {
	firstFailure, recovered, scrapedThrough time.Time
	leaderInstance                          string
}

// The selected target keeps its address and labels during this fault. Require
// one continuous failed interval, healthy followers, and a real healthy baseline.
// Reload completion and test polling never date the first failed or restored scrape.
func alReadScrapeHistory(upBody, durationBody []byte, pods []string, leader string, started, queriedAt time.Time) (alScrapeHistory, error) {
	return alReadScrapeHistoryWithReloads(upBody, durationBody, pods, leader, started, queriedAt, time.Time{}, time.Time{})
}

func alReadScrapeHistoryWithReloads(upBody, durationBody []byte, pods []string, leader string, started, queriedAt, loaded, restored time.Time) (alScrapeHistory, error) {
	history := alScrapeHistory{scrapedThrough: queriedAt}
	if started.IsZero() || queriedAt.Before(started) || queriedAt.Sub(started)+2*alScrapeInterval >= alAdmissionHistoryWindow {
		return history, errors.New("scrape fault history does not cover its interval")
	}
	wanted := map[string]bool{}
	for _, pod := range pods {
		if pod == "" || wanted[pod] {
			return history, errors.New("scrape fault history needs distinct manager Pods")
		}
		wanted[pod] = true
	}
	if len(wanted) < 2 || !wanted[leader] {
		return history, errors.New("scrape fault history needs the leader and its followers")
	}
	since := started.Add(-2 * alScrapeInterval)
	type reading struct {
		instance string
		samples  []alAdmissionSample
	}
	read := func(body []byte, metric string, integer bool) (map[string]reading, error) {
		series, err := alAdmissionNativeMatrix(body)
		if err != nil {
			return nil, err
		}
		result, instances := map[string]reading{}, map[string]bool{}
		for _, item := range series {
			pod, instance := item.Metric["pod"], item.Metric["instance"]
			if item.Metric["__name__"] != metric || item.Metric["job"] != alScrapeJob || !wanted[pod] || instance == "" ||
				result[pod].instance != "" || instances[instance] {
				return nil, errors.New("scrape fault history has an unexpected or duplicate target")
			}
			gapSince := since
			if pod == leader && !loaded.IsZero() {
				// Changing the URL changes Prometheus's target hash and scrape
				// offset. Validate this target's gaps against the reload instants
				// below; its durations must have the same exact timestamps.
				gapSince = queriedAt
			}
			samples, err := alAdmissionNativeSamples(item, gapSince, queriedAt, integer)
			if err != nil {
				return nil, err
			}
			if len(samples) < 2 || samples[0].at.After(since) {
				return nil, errors.New("scrape fault history has no pre-fault baseline")
			}
			for len(samples) > 1 && !samples[1].at.After(since) {
				samples = samples[1:]
			}
			result[pod] = reading{instance, samples}
			instances[instance] = true
		}
		if len(result) != len(wanted) {
			return nil, errors.New("scrape fault history omitted a manager")
		}
		return result, nil
	}
	up, err := read(upBody, "up", true)
	if err != nil {
		return history, err
	}
	durations, err := read(durationBody, "scrape_duration_seconds", false)
	if err != nil {
		return history, err
	}
	for pod, health := range up {
		duration := durations[pod]
		if duration.instance != health.instance || len(duration.samples) != len(health.samples) {
			return history, errors.New("scrape fault history lost a target's matching durations")
		}
		for index, sample := range health.samples {
			if !duration.samples[index].at.Equal(sample.at) || duration.samples[index].value > alScrapeTimeout.Seconds() ||
				(sample.value != 0 && sample.value != 1) {
				return history, errors.New("scrape fault history has invalid or mismatched samples")
			}
			if index > 0 && sample.at.After(since) {
				previous := health.samples[index-1]
				if pod == leader && !loaded.IsZero() && sample.value != previous.value && !sample.at.Before(started) {
					reload := loaded
					if sample.value == 1 {
						reload = restored
					}
					// Each target URL has its own five-second schedule. The reload
					// instant dates the HTTP request, and the old target may still
					// scrape afterward. Bound the transition between those native
					// observations, not from the earlier request. Only this gap may
					// span two intervals; each side and every follower stay strict.
					transitionGap := alScrapeInterval + alAdmissionSampleGap
					if reload.IsZero() || sample.at.Before(reload) ||
						reload.Sub(previous.at) > alAdmissionSampleGap ||
						sample.at.Sub(previous.at) > transitionGap {
						return history, errors.New("scrape transition has no fresh observations around its configuration reload")
					}
				} else if sample.at.Sub(previous.at) > alAdmissionSampleGap {
					return history, errors.New("scrape fault history has missing scrapes outside a configuration reload")
				}
			}
			if sample.at.Before(started) || pod != leader {
				if sample.value != 1 {
					return history, errors.New("a follower or the pre-fault baseline was unavailable")
				}
				continue
			}
			if sample.value == 0 {
				if !history.recovered.IsZero() {
					return history, errors.New("the leader scrape failed again after recovery")
				}
				if history.firstFailure.IsZero() {
					history.firstFailure = sample.at
				}
			} else if !history.firstFailure.IsZero() && history.recovered.IsZero() {
				history.recovered = sample.at
			}
		}
		history.scrapedThrough = earlierTime(history.scrapedThrough, health.samples[len(health.samples)-1].at)
	}
	history.leaderInstance = up[leader].instance
	return history, nil
}

func alScrapeFailureDelivered(delivery alDelivery, history alScrapeHistory) bool {
	return !history.firstFailure.IsZero() && history.recovered.IsZero() &&
		!delivery.StartsAt.Before(history.firstFailure.Add(alViewUnsyncedFor)) && !delivery.ReceivedAt.Before(delivery.StartsAt) &&
		!delivery.ReceivedAt.After(history.firstFailure.Add(alViewUnsyncedFor+alDetectionSlack))
}

func alScrapeFailureCleared(firing, resolved alDelivery, history alScrapeHistory) bool {
	return !history.recovered.IsZero() && !firing.StartsAt.IsZero() && resolved.StartsAt.Equal(firing.StartsAt) &&
		!resolved.EndsAt.Before(history.recovered) && !resolved.ReceivedAt.Before(resolved.EndsAt) &&
		!resolved.ReceivedAt.After(history.recovered.Add(alDetectionSlack)) && !history.scrapedThrough.Before(resolved.ReceivedAt)
}

// A successful HTTP scrape alone cannot establish that the leader's state
// synchronized. Require that gauge in the same first recovered native scrape.
func alRecoveredScrapeSynced(body []byte, leader string, history alScrapeHistory, queriedAt time.Time) error {
	series, err := alAdmissionNativeMatrix(body)
	if err != nil {
		return err
	}
	if len(series) != 1 || history.recovered.IsZero() || series[0].Metric["__name__"] != "ptah_operator_unresolved_view_synced" ||
		series[0].Metric["job"] != alScrapeJob || series[0].Metric["pod"] != leader || series[0].Metric["instance"] != history.leaderInstance {
		return errors.New("recovered view has no exact leader scrape")
	}
	// A missing view during the failed HTTP interval is expected. From the
	// recovered scrape onward its samples must again be complete and fresh.
	samples, err := alAdmissionNativeSamples(series[0], history.recovered, queriedAt, true)
	if err != nil {
		return err
	}
	found := false
	for _, sample := range samples {
		if !sample.at.Before(history.recovered) && sample.value != 1 {
			return errors.New("the recovered leader lost its synchronized view")
		}
		found = found || sample.at.Equal(history.recovered)
	}
	if !found {
		return errors.New("the first recovered scrape omitted the synchronized leader view")
	}
	return nil
}
