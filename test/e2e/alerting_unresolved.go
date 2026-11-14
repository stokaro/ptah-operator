package e2e

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	alUnresolvedMetric = "ptah_operator_unresolved_attempts"
	alViewSyncedMetric = "ptah_operator_unresolved_view_synced"
)

// alUnresolvedHistoryMetrics are the series one unresolved history snapshot
// reads.
var alUnresolvedHistoryMetrics = []string{alUnresolvedMetric, alViewSyncedMetric, alViewReadFailuresMetric, "up", "scrape_duration_seconds"}

var errAlReadGapOutlastedHold = errors.New("a declared failed read outlasted the rules' hold")

type alUnresolvedHistory struct {
	// appeared and cleared are when the leader's scrape that first read the
	// incident, and the one that first read it gone, completed.
	appeared, cleared time.Time
	// appearedScrape is the first incident scrape's own timestamp, and
	// withheldSince the first of the declared failed reads directly before
	// it, if any: those reads kept the gauge from reading the incident.
	appearedScrape, withheldSince time.Time
	// heldFrom is the first leader scrape after the last one that read the
	// incident. The rule cannot have stopped seeing it earlier, so its hold
	// started no earlier either.
	heldFrom time.Time
	// through is the last scrape that read the gauge.
	through time.Time
	latest  int
	// declared counts the leader's declared failed reads since the baseline.
	declared int
}

// Both the healthy zero baseline and the incident must be native leader
// samples. A missing gauge, unhealthy follower, or gap is not a zero.
//
// A scrape whose state read failed publishes no gauge: the manager never
// presents a partial census. Such a scrape is accepted only where the leader
// declared it in that same scrape, with its view reported unsynchronized and
// its read-failure counter risen from the scrape before; any other missing
// gauge is a scrape this history lost. A declared gap establishes nothing
// about the incident. The rule holds a firing alert through it for
// alStateReadGapHold, so a gap that lasts as long as that, from its first
// failed read to the completion of the scrape that reads again, may have
// resolved the incident and fails the history rather than stretching what it
// proves.
func alReadUnresolvedHistory(gaugeBody, syncedBody, failuresBody, upBody, durationBody []byte, pods []string, leader, family string, started, queriedAt time.Time) (alUnresolvedHistory, error) {
	h := alUnresolvedHistory{}
	if family != "schema" && family != "migration" {
		return h, errors.New("unknown unresolved family")
	}
	health, err := alReadScrapeHistory(upBody, durationBody, pods, leader, started, queriedAt)
	if err != nil {
		return h, err
	}
	if !health.firstFailure.IsZero() {
		return h, errors.New("unresolved proof lost a healthy scrape")
	}
	series, err := alAdmissionNativeMatrix(gaugeBody)
	if err != nil {
		return h, err
	}
	if len(series) != 1 || series[0].Metric["__name__"] != alUnresolvedMetric || series[0].Metric["family"] != family || series[0].Metric["job"] != alScrapeJob || series[0].Metric["pod"] != leader || series[0].Metric["instance"] != health.leaderInstance {
		return h, errors.New("unresolved gauge must name the original leader and family alone")
	}
	since := started.Add(-2 * alScrapeInterval)
	// The scrape history already proves a fresh committed scrape and no gap
	// between scrapes. Gaps in the gauge are judged against those scrapes
	// below, so the parser checks only order: a gap boundary at queriedAt
	// exempts every sample.
	values, err := alNativeSamples(series[0], queriedAt, queriedAt, true, false)
	if err != nil {
		return h, err
	}
	if len(values) < 2 || values[0].at.After(since) {
		return h, errors.New("unresolved gauge has no complete zero baseline")
	}
	for len(values) > 1 && !values[1].at.After(since) {
		values = values[1:]
	}
	durations, err := alAdmissionNativeMatrix(durationBody)
	if err != nil {
		return h, err
	}
	var times []alAdmissionSample
	for _, s := range durations {
		if s.Metric["pod"] == leader {
			times, err = alAdmissionNativeSamples(s, since, queriedAt, false)
			if err != nil {
				return h, err
			}
		}
	}
	// The baseline is the last reading at or before since, and the scrapes
	// from it on are the ones the gauge must account for.
	for len(times) != 0 && times[0].at.Before(values[0].at) {
		times = times[1:]
	}
	if len(times) == 0 || !times[0].at.Equal(values[0].at) {
		return h, errors.New("unresolved gauge lost its scrape identity")
	}
	synced, err := alLeaderViewSynced(syncedBody, leader, health.leaderInstance, queriedAt)
	if err != nil {
		return h, err
	}
	// A nil body would declare no failure. The view is always read here, so
	// an absent counter must not stand in for a declared nothing.
	if failuresBody == nil {
		return h, errors.New("unresolved history has no read-failure counter")
	}
	failures, err := alReadFailureCounts(failuresBody, leader, health.leaderInstance, queriedAt)
	if err != nil {
		return h, err
	}
	next := 0
	var gapFrom, lastRead time.Time
	for i, scrape := range times {
		completed := scrape.at.Add(time.Duration(scrape.value * float64(time.Second)))
		if next == len(values) || !values[next].at.Equal(scrape.at) {
			if next < len(values) && values[next].at.Before(scrape.at) {
				return h, errors.New("unresolved gauge lost its scrape identity")
			}
			// times[0] is the baseline reading, so i is at least 1 here.
			now, counted := failures[scrape.at.UnixNano()]
			before, wasCounted := failures[times[i-1].at.UnixNano()]
			view, reported := synced[scrape.at.UnixNano()]
			if !counted || !wasCounted || now <= before || !reported || view != 0 {
				return h, errors.New("unresolved gauge omitted a native scrape")
			}
			if gapFrom.IsZero() {
				gapFrom = scrape.at
			}
			if !h.appeared.IsZero() && h.cleared.IsZero() && h.heldFrom.IsZero() {
				h.heldFrom = scrape.at
			}
			h.declared++
			continue
		}
		v := values[next]
		next++
		if view, reported := synced[scrape.at.UnixNano()]; !reported || view != 1 {
			return h, errors.New("unresolved gauge was read without a synchronized view")
		}
		if !gapFrom.IsZero() && completed.Sub(gapFrom) >= alStateReadGapHold {
			return h, errAlReadGapOutlastedHold
		}
		if v.value > 1 {
			return h, errors.New("unresolved gauge acquired another incident")
		}
		if !v.at.After(started) && v.value != 0 {
			return h, errors.New("an unresolved incident preceded fault injection")
		}
		if v.value == 1 {
			if !h.cleared.IsZero() {
				return h, errors.New("unresolved incident recurred after recovery")
			}
			if h.appeared.IsZero() {
				h.appeared, h.appearedScrape, h.withheldSince = completed, v.at, gapFrom
			}
			// A gap inside the incident ended with the incident read again.
			h.heldFrom = time.Time{}
		} else if !h.appeared.IsZero() && h.cleared.IsZero() {
			h.cleared = completed
			if h.heldFrom.IsZero() {
				h.heldFrom = v.at
			}
		}
		gapFrom, lastRead = time.Time{}, v.at
		h.latest = int(v.value)
	}
	if next != len(values) {
		return h, errors.New("unresolved gauge lost its scrape identity")
	}
	if !gapFrom.IsZero() && queriedAt.Sub(gapFrom) >= alStateReadGapHold {
		return h, errAlReadGapOutlastedHold
	}
	// What a failed read did not observe is not covered: the history ends at
	// the last scrape that carried the gauge.
	h.through = earlierTime(health.scrapedThrough, lastRead)
	return h, nil
}

// alLeaderViewSynced reads the leader's synchronized-view gauge by scrape
// timestamp. Followers report their own unsynchronized view and are not read.
func alLeaderViewSynced(body []byte, leader, instance string, queriedAt time.Time) (map[int64]float64, error) {
	series, err := alAdmissionNativeMatrix(body)
	if err != nil {
		return nil, err
	}
	views := map[int64]float64{}
	found := false
	for _, row := range series {
		if row.Metric["__name__"] != alViewSyncedMetric || row.Metric["job"] != alScrapeJob {
			return nil, errors.New("view history includes another metric or scrape job")
		}
		if row.Metric["pod"] != leader {
			continue
		}
		if row.Metric["instance"] != instance || found {
			return nil, errors.New("the synchronized view must come from the original leader alone")
		}
		found = true
		// Only order is checked here: a scrape without its view sample is
		// refused where the history reads that scrape.
		samples, err := alNativeSamples(row, queriedAt, queriedAt, true, false)
		if err != nil {
			return nil, err
		}
		for _, sample := range samples {
			if sample.value != 0 && sample.value != 1 {
				return nil, errors.New("the synchronized view is neither 0 nor 1")
			}
			views[sample.at.UnixNano()] = sample.value
		}
	}
	return views, nil
}

// withheldAfter is how long declared failed reads directly before the first
// incident scrape kept the gauge from reading an incident recorded at
// recorded. A failed read before the record existed withheld nothing.
func (h alUnresolvedHistory) withheldAfter(recorded time.Time) time.Duration {
	if h.withheldSince.IsZero() {
		return 0
	}
	from := laterTime(h.withheldSince, recorded)
	if !h.appearedScrape.After(from) {
		return 0
	}
	return h.appearedScrape.Sub(from)
}

// The controller's persisted timestamps date both bounds. Neither a later
// poll nor a later scrape may extend the frozen 45-second delivery target;
// only reads the leader declared failed may, by the time they withheld the
// incident, since the rule cannot fire on a count it was not given.
func alUnresolvedDelivered(d alDelivery, h alUnresolvedHistory, recorded time.Time) bool {
	return !recorded.IsZero() && !h.appeared.IsZero() && h.cleared.IsZero() && h.latest == 1 && !h.appeared.Before(recorded) &&
		!d.StartsAt.Before(recorded) && !d.ReceivedAt.Before(d.StartsAt) && !d.ReceivedAt.After(recorded.Add(alDetectionSlack+h.withheldAfter(recorded)))
}

// The rule holds the incident for alStateReadGapHold after it last saw it, so
// the receiver hears of the resolution no earlier than that after the first
// scrape that did not read it, and no later than the hold plus the delivery
// target after the record was accounted for.
func alUnresolvedCleared(firing, resolved alDelivery, h alUnresolvedHistory, recorded, accounted time.Time) bool {
	return !recorded.IsZero() && accounted.After(recorded) && !h.cleared.IsZero() && !h.cleared.Before(accounted) && h.latest == 0 && !h.heldFrom.IsZero() &&
		!firing.StartsAt.IsZero() && resolved.StartsAt.Equal(firing.StartsAt) && !resolved.EndsAt.Before(accounted) &&
		!resolved.EndsAt.Before(h.heldFrom.Add(alStateReadGapHold)) && !resolved.ReceivedAt.Before(resolved.EndsAt) &&
		!resolved.ReceivedAt.After(accounted.Add(alStateReadGapHold+alDetectionSlack)) && !h.through.Before(resolved.ReceivedAt)
}

// Validate the closed pre-acknowledgment interval, including events between
// direct reads. Once latched, the exact run must never vanish or change.
func alUnresolvedMigrationRecord(events []watchEvent[*ptahv1.PtahMigration], name string, uid types.UID, original *ptahv1.MigrationOperationStatus, target string) (*ptahv1.UnresolvedMigrationRunStatus, error) {
	if !sha256Pattern.MatchString(target) || uid == "" || original == nil || original.PlanRef == nil || original.ID == "" || original.JobUID == "" {
		return nil, errors.New("missing original unresolved Apply identity")
	}
	var first *ptahv1.UnresolvedMigrationRunStatus
	for _, e := range events {
		v := e.Object
		if v == nil || v.Name != name {
			continue
		}
		if v.UID != uid || v.DeletionTimestamp != nil || e.Type == watch.Deleted {
			return nil, errors.New("unresolved migration was replaced or deleted")
		}
		r := v.Status.UnresolvedRun
		if r == nil {
			if first != nil {
				return nil, errors.New("unresolved run disappeared before acknowledgment")
			}
			continue
		}
		var copied ptahv1.UnresolvedMigrationRunStatus
		copyMatches := json.Unmarshal([]byte(v.Annotations[ptahv1.UnresolvedRunAnnotation]), &copied) == nil && equality.Semantic.DeepEqual(r, &copied)
		if r.TargetIdentityDigest != target || r.OperationID != original.ID || r.JobUID != original.JobUID || r.JobName != original.JobName || !equality.Semantic.DeepEqual(r.PlanRef, *original.PlanRef) || unresolvedRunRecorded(v.Status, original.JobName, string(original.JobUID)) != nil || !copyMatches {
			return nil, errors.New("unresolved record lost its exact run or recovery copy")
		}
		if first == nil {
			first = r.DeepCopy()
		} else if !equality.Semantic.DeepEqual(first, r) {
			return nil, errors.New("the latched unresolved record changed")
		}
	}
	if first == nil {
		return nil, errors.New("history contains no persisted unresolved run")
	}
	return first, nil
}

// A historical LastRun is expected after recovery. Reusing the initial-plan
// predicate would wait forever because that predicate forbids any prior run.
func alUnresolvedMigrationReady(v *ptahv1.PtahMigration, original *ptahv1.MigrationOperationStatus) bool {
	if v == nil || original == nil || original.PlanRef == nil || v.UID == "" || v.Generation < 1 || v.Status.ObservedGeneration != v.Generation || v.Spec.Policy.Apply != ptahv1.ApplyPolicyOnApproval || v.Spec.Suspend || v.Status.ActiveOperation != nil || v.Status.PendingLockRelease != nil || v.Status.UnresolvedRun != nil || v.Status.Plan == nil || v.Status.Plan.UID == "" || v.Status.Plan.UID == original.PlanRef.UID || v.Status.Plan.Name == "" {
		return false
	}
	r := v.Status.ResolvedRun
	if r == nil || r.ResolvedAt.IsZero() || r.OperationID != original.ID || r.Resolution != ptahv1.MigrationRunResolvedByAcknowledgment || r.AcknowledgmentRef == nil || r.AcknowledgmentRef.UID == "" || r.AcknowledgedBy == nil || r.AcknowledgedBy.Username == "" || !historyReadAfter(v, r.ResolvedAt.Time) {
		return false
	}
	if _, exists := v.Annotations[ptahv1.UnresolvedRunAnnotation]; exists {
		return false
	}
	for _, c := range v.Status.Conditions {
		if c.Type == ptahv1.ConditionApprovalRequired && c.Status == metav1.ConditionTrue && c.ObservedGeneration == v.Generation {
			return true
		}
	}
	return false
}

// The repair may remove migration 3's unfinished revision only because its
// exact fixture has no persistent effect. Fail before publication if it changes.
func alUnresolvedSleepFixture(engine string, body []byte) bool {
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			lines = append(lines, line)
		}
	}
	expected := "SELECT pg_sleep(45);"
	if engine == "mysql" {
		expected = "SELECT SLEEP(45);"
	} else if engine != "postgresql" {
		return false
	}
	return strings.Join(lines, " ") == expected
}
