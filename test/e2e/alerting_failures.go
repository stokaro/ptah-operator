package e2e

import (
	"errors"
	"fmt"
	"slices"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	alFailuresAlert  = "PtahOperatorOperationsFailing"
	alFailuresMetric = "ptah_operator_failures_total"
	alFailuresWindow = 5 * time.Minute
	alFailuresCount  = 3
	// Retain the baseline, publication and recovery; the alert window stays five minutes.
	alFailuresHistoryWindow = 30 * time.Minute
)

// Earlier scenarios deliberately increment this counter. Baseline preparation
// may wait for that history to age out; missing scrapes and resets are errors.
var errAlFailuresBaseline = errors.New("operation failures have no complete quiet baseline")

type alFailuresHistory struct {
	increments                                           int
	thresholdLower, lastLower, lastUpper, scrapedThrough time.Time
}

// Resolve failures already have a series from the stalled-operation control.
// Keep a full quiet window before this fault, then account for every increment
// on the original leader. Other categories and followers must remain constant.
// The rule uses increase(), whose extrapolation can cross >3 at the third
// increment. Its preceding scrape is therefore the conservative delivery bound;
// waiting for a fourth observed failure must not move that bound forward.
func alReadFailuresHistory(counterBody, upBody, durationBody []byte, pods []string, leader, family string, started, queriedAt time.Time) (alFailuresHistory, error) {
	h := alFailuresHistory{scrapedThrough: queriedAt}
	since := started.Add(-alFailuresWindow)
	if (family != "schema" && family != "migration") || started.IsZero() || queriedAt.Before(started) || queriedAt.Sub(since)+2*alScrapeInterval >= alFailuresHistoryWindow {
		return h, errors.New("operation-failure history does not cover its baseline and fault")
	}
	wanted := map[string]bool{}
	for _, pod := range pods {
		if pod == "" || wanted[pod] {
			return h, errors.New("operation-failure history needs distinct managers")
		}
		wanted[pod] = true
	}
	if len(wanted) < 2 || !wanted[leader] {
		return h, errors.New("operation-failure history needs its leader and followers")
	}
	type reading struct {
		instance string
		values   []alAdmissionSample
	}
	readHealth := func(body []byte, metric string, integer bool) (map[string]reading, error) {
		series, err := alAdmissionNativeMatrix(body)
		if err != nil {
			return nil, err
		}
		result, instances := map[string]reading{}, map[string]bool{}
		for _, item := range series {
			pod, instance := item.Metric["pod"], item.Metric["instance"]
			if item.Metric["__name__"] != metric || item.Metric["job"] != alScrapeJob || !wanted[pod] || instance == "" || result[pod].instance != "" || instances[instance] {
				return nil, errors.New("operation-failure history has an unexpected or duplicate scrape target")
			}
			values, err := alNativeSamplesWithin(item, since, queriedAt, integer, true, alFailuresHistoryWindow)
			if err != nil {
				return nil, err
			}
			if len(values) < 2 || values[0].at.After(since) {
				return nil, errors.New("operation-failure history lacks its full pre-fault window")
			}
			for len(values) > 1 && !values[1].at.After(since) {
				values = values[1:]
			}
			result[pod] = reading{instance, values}
			instances[instance] = true
		}
		if len(result) != len(wanted) {
			return nil, errors.New("operation-failure history omitted a manager scrape")
		}
		return result, nil
	}
	up, err := readHealth(upBody, "up", true)
	if err != nil {
		return h, err
	}
	duration, err := readHealth(durationBody, "scrape_duration_seconds", false)
	if err != nil {
		return h, err
	}
	for pod, health := range up {
		d := duration[pod]
		if health.instance != d.instance || len(health.values) != len(d.values) {
			return h, errors.New("operation-failure scrapes do not match")
		}
		for i, s := range health.values {
			if s.value != 1 || !s.at.Equal(d.values[i].at) || d.values[i].value > alScrapeTimeout.Seconds() {
				return h, errors.New("operation-failure proof lost a healthy scrape")
			}
		}
		h.scrapedThrough = earlierTime(h.scrapedThrough, health.values[len(health.values)-1].at)
	}
	series, err := alAdmissionNativeMatrix(counterBody)
	if err != nil {
		return h, err
	}
	seen, sawLeader := map[string]bool{}, false
	baselineUnready := false
	for _, item := range series {
		pod, category := item.Metric["pod"], item.Metric["category"]
		key := pod + "/" + category
		if item.Metric["__name__"] != alFailuresMetric || item.Metric["job"] != alScrapeJob || item.Metric["family"] != family || item.Metric["stage"] != "resolve" || !wanted[pod] || item.Metric["instance"] != up[pod].instance || category == "" || seen[key] {
			return h, errors.New("unexpected or duplicate operation-failure counter")
		}
		seen[key] = true
		values, err := alNativeSamplesWithin(item, since, queriedAt, true, true, alFailuresHistoryWindow)
		if err != nil {
			return h, err
		}
		if len(values) < 2 || values[0].at.After(since) {
			baselineUnready = true
		}
		for len(values) > 1 && !values[1].at.After(since) {
			values = values[1:]
		}
		health := up[pod]
		// A newly instantiated counter may lack a complete baseline. Check
		// all samples it does have before allowing the caller to wait.
		durations := duration[pod].values
		for len(health.values) > 1 && health.values[0].at.Before(values[0].at) {
			health.values = health.values[1:]
			durations = durations[1:]
		}
		if len(values) != len(health.values) {
			return h, errors.New("failure counter omitted a native scrape")
		}
		for i, s := range values {
			if !s.at.Equal(health.values[i].at) {
				return h, errors.New("failure counter lost its scrape identity")
			}
			if i == 0 {
				continue
			}
			previous := values[i-1]
			if s.value < previous.value {
				return h, errors.New("failure counter reset")
			}
			if s.value == previous.value {
				continue
			}
			if !s.at.After(started) {
				baselineUnready = true
				continue
			}
			if pod != leader || category != "operation" {
				return h, errors.New("an unrelated failure contaminated the operation proof")
			}
			delta := int(s.value - previous.value)
			if h.increments < alFailuresCount && h.increments+delta >= alFailuresCount {
				h.thresholdLower = previous.at
			}
			h.increments += delta
			h.lastLower = previous.at
			h.lastUpper = s.at.Add(time.Duration(durations[i].value * float64(time.Second)))
		}
		if pod == leader && category == "operation" {
			sawLeader = true
		}
	}
	if !sawLeader {
		return h, errors.New("the leader's operation-failure counter is missing")
	}
	// Retry readiness only after every series passed its integrity checks.
	if baselineUnready {
		return h, errAlFailuresBaseline
	}
	return h, nil
}

func alFailuresHistoryQuery(family string) string {
	return fmt.Sprintf(`%s{job=%q,family=%q,stage="resolve"}[%ds]`, alFailuresMetric, alScrapeJob, family, int(alFailuresHistoryWindow/time.Second))
}

func alFailuresDelivered(d alDelivery, h alFailuresHistory, started time.Time) bool {
	return h.increments > alFailuresCount && !h.thresholdLower.IsZero() && !d.StartsAt.Before(started) && !d.StartsAt.Before(h.thresholdLower) && !d.ReceivedAt.Before(d.StartsAt) && !d.ReceivedAt.After(h.thresholdLower.Add(alDetectionSlack))
}

func alFailuresCleared(firing, resolved alDelivery, h alFailuresHistory) bool {
	return !h.lastLower.IsZero() && resolved.StartsAt.Equal(firing.StartsAt) && !resolved.EndsAt.Before(firing.StartsAt) && !resolved.ReceivedAt.Before(resolved.EndsAt) && !resolved.ReceivedAt.After(h.lastLower.Add(alFailuresWindow+alDetectionSlack))
}

// Distinct Job results, not PromQL extrapolation, establish the required
// number of actual failures inside one rolling window.
func alFailuresInWindow(finished []time.Time) bool {
	if len(finished) <= alFailuresCount {
		return false
	}
	ordered := slices.Clone(finished)
	for _, at := range ordered {
		if at.IsZero() {
			return false
		}
	}
	slices.SortFunc(ordered, func(a, b time.Time) int { return a.Compare(b) })
	last := len(ordered) - 1
	return ordered[last].Before(ordered[last-alFailuresCount].Add(alFailuresWindow))
}

func alFailuresUnchanged(initial, current client.Object) error {
	if initial.GetUID() == "" || initial.GetGeneration() < 1 || current.GetUID() != initial.GetUID() || current.GetGeneration() != initial.GetGeneration() ||
		client.ObjectKeyFromObject(current) != client.ObjectKeyFromObject(initial) || current.GetDeletionTimestamp() != nil {
		return errors.New("the failure consumer was edited, replaced or deleted")
	}
	var same bool
	switch before := initial.(type) {
	case *ptahv1.PtahSchema:
		after, ok := current.(*ptahv1.PtahSchema)
		same = ok && equality.Semantic.DeepEqual(before.Spec, after.Spec)
	case *ptahv1.PtahMigration:
		after, ok := current.(*ptahv1.PtahMigration)
		same = ok && equality.Semantic.DeepEqual(before.Spec, after.Spec)
	}
	if !same || alStalledReading(current).operation == "Apply" {
		return errors.New("the failure consumer changed its inputs or claimed Apply")
	}
	return nil
}

type alFailureResult struct {
	pod      types.UID
	finished time.Time
	failed   bool
	digest   string
}

// Account for every original Resolve and require an observed Job and exactly
// one observed Pod for each retained result. Empty histories cannot pass.
func alFailuresWorkloads(uid types.UID, results map[types.UID]alFailureResult, jobs []watchEvent[*batchv1.Job], pods []watchEvent[*corev1.Pod]) error {
	if uid == "" || len(results) <= alFailuresCount {
		return errors.New("no complete repeated-failure workload inventory")
	}
	seenJobs := map[types.UID]bool{}
	seenPods := map[types.UID]map[types.UID]bool{}
	for _, e := range jobs {
		v := e.Object
		owner := metav1.GetControllerOf(v)
		_, retained := results[v.UID]
		if owner == nil || owner.UID != uid {
			if retained {
				return errors.New("a retained Resolve changed its owner")
			}
			continue
		}
		operation := v.Labels[labelOperation]
		if operation == "apply" {
			return errors.New("closed workload history contains Apply")
		}
		if operation == "resolve" && !retained {
			return errors.New("a Resolve outcome was not retained")
		}
		if retained {
			if operation != "resolve" {
				return errors.New("a retained Resolve changed its operation")
			}
			seenJobs[v.UID] = true
		}
	}
	for _, e := range pods {
		v := e.Object
		owner := metav1.GetControllerOf(v)
		if owner == nil {
			continue
		}
		if _, ok := results[owner.UID]; !ok {
			continue
		}
		if seenPods[owner.UID] == nil {
			seenPods[owner.UID] = map[types.UID]bool{}
		}
		seenPods[owner.UID][v.UID] = true
	}
	for uid, r := range results {
		if uid == "" || r.pod == "" || !seenJobs[uid] || len(seenPods[uid]) != 1 || !seenPods[uid][r.pod] {
			return errors.New("a retained Resolve has a missing or replaced workload")
		}
	}
	return nil
}
