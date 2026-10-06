package e2e

import (
	"errors"
	"slices"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
)

const (
	alViewReadAlert  = "PtahOperatorUnresolvedViewReadFailures"
	alViewReadMetric = "ptah_operator_unresolved_view_read_failures_total"
	alViewReadWindow = 60 * time.Second
)

var errAlViewReadBaseline = errors.New("read-failure history has no complete pre-fault baseline")

// Change only list on the one root-resource rule. Reads by name, watches,
// metadata/status writes, and every other resource retain their exact grants.
func alViewReadRule(role *rbacv1.ClusterRole, resource string) (int, []string, error) {
	index, err := statusRuleIndex(role, resource)
	if err != nil {
		return -1, nil, err
	}
	rule := role.Rules[index]
	if !slices.Equal(rule.Resources, []string{resource}) || len(rule.ResourceNames) != 0 ||
		!slices.Contains(rule.Verbs, "list") || !slices.Contains(rule.Verbs, "get") ||
		!slices.Contains(rule.Verbs, "watch") || slices.Contains(rule.Verbs, "*") {
		return -1, nil, errors.New("the read fault needs one explicit root-resource list grant")
	}
	return index, slices.DeleteFunc(slices.Clone(rule.Verbs), func(verb string) bool { return verb == "list" }), nil
}

type alViewReadHistory struct {
	firstLower, lastLower, lastUpper time.Time
	scrapedThrough                   time.Time
}

// Native counter increments date the failure rather than the poll that found
// it. All managers must keep healthy, continuous scrapes; a missing follower,
// a reset, or an initial positive sample without its baseline cannot pass.
func alReadViewHistory(counterBody, upBody, durationBody []byte, pods []string, leader string, started, queriedAt time.Time) (alViewReadHistory, error) {
	history := alViewReadHistory{scrapedThrough: queriedAt}
	if started.IsZero() || queriedAt.Before(started) || queriedAt.Sub(started)+2*alScrapeInterval >= alAdmissionHistoryWindow {
		return history, errors.New("read-failure history does not cover the fault interval")
	}
	wanted := map[string]bool{}
	for _, pod := range pods {
		if pod == "" || wanted[pod] {
			return history, errors.New("read-failure history needs distinct manager Pods")
		}
		wanted[pod] = true
	}
	if len(wanted) < 2 || !wanted[leader] {
		return history, errors.New("read-failure history needs the leader and its followers")
	}
	since := started.Add(-2 * alScrapeInterval)
	type targetSamples struct {
		instance string
		values   []alAdmissionSample
	}
	read := func(body []byte, metric string, integer bool) (map[string]targetSamples, error) {
		series, err := alAdmissionNativeMatrix(body)
		if err != nil {
			return nil, err
		}
		result, instances := map[string]targetSamples{}, map[string]bool{}
		for _, item := range series {
			pod, instance := item.Metric["pod"], item.Metric["instance"]
			if item.Metric["__name__"] != metric || item.Metric["job"] != alScrapeJob || !wanted[pod] ||
				instance == "" || result[pod].instance != "" || instances[instance] {
				return nil, errors.New("read-failure history has an unexpected or duplicate manager series")
			}
			values, err := alAdmissionNativeSamples(item, since, queriedAt, integer)
			if err != nil {
				return nil, err
			}
			if len(values) < 2 || values[0].at.After(since) {
				return nil, errAlViewReadBaseline
			}
			// Startup may precede the counter's first successful scrape. Keep
			// the same last pre-fault baseline and the complete interval after
			// it, without requiring unrelated startup histories to be equal.
			for len(values) > 1 && !values[1].at.After(since) {
				values = values[1:]
			}
			result[pod] = targetSamples{instance: instance, values: values}
			instances[instance] = true
		}
		if len(result) != len(wanted) {
			return nil, errors.New("read-failure history omitted a manager")
		}
		return result, nil
	}
	counters, err := read(counterBody, alViewReadMetric, true)
	if err != nil {
		return history, err
	}
	up, err := read(upBody, "up", true)
	if err != nil {
		return history, err
	}
	durations, err := read(durationBody, "scrape_duration_seconds", false)
	if err != nil {
		return history, err
	}
	for pod, counter := range counters {
		health, duration := up[pod], durations[pod]
		if health.instance != counter.instance || duration.instance != counter.instance ||
			len(health.values) != len(counter.values) || len(duration.values) != len(counter.values) {
			return history, errors.New("manager history lost a matching scrape")
		}
		for index, sample := range counter.values {
			if !health.values[index].at.Equal(sample.at) || !duration.values[index].at.Equal(sample.at) ||
				duration.values[index].value > alScrapeTimeout.Seconds() {
				return history, errors.New("manager history has mismatched or timed-out scrapes")
			}
			if sample.at.Before(since) {
				continue
			}
			if health.values[index].value != 1 {
				return history, errors.New("a manager scrape failed during the API-read fault")
			}
			if index == 0 {
				continue
			}
			previous := counter.values[index-1]
			if sample.value < previous.value {
				return history, errors.New("a read-failure counter reset during the proof")
			}
			if sample.at.Before(started) || sample.value == previous.value {
				continue
			}
			if pod != leader {
				return history, errors.New("a follower published new state-read failures")
			}
			lower := previous.at
			if history.firstLower.IsZero() {
				history.firstLower = lower
			}
			history.lastLower = lower
			history.lastUpper = sample.at.Add(time.Duration(duration.values[index].value * float64(time.Second)))
		}
		history.scrapedThrough = earlierTime(history.scrapedThrough, counter.values[len(counter.values)-1].at)
	}
	return history, nil
}
