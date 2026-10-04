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

const alUnresolvedMetric = "ptah_operator_unresolved_attempts"

type alUnresolvedHistory struct {
	appeared, cleared, through time.Time
	latest                     int
}

// Both the healthy zero baseline and the incident must be native leader
// samples. A missing gauge, unhealthy follower, or gap is not a zero.
func alReadUnresolvedHistory(gaugeBody, upBody, durationBody []byte, pods []string, leader, family string, started, queriedAt time.Time) (alUnresolvedHistory, error) {
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
	h.through = health.scrapedThrough
	series, err := alAdmissionNativeMatrix(gaugeBody)
	if err != nil {
		return h, err
	}
	if len(series) != 1 || series[0].Metric["__name__"] != alUnresolvedMetric || series[0].Metric["family"] != family || series[0].Metric["job"] != alScrapeJob || series[0].Metric["pod"] != leader || series[0].Metric["instance"] != health.leaderInstance {
		return h, errors.New("unresolved gauge must name the original leader and family alone")
	}
	since := started.Add(-2 * alScrapeInterval)
	values, err := alAdmissionNativeSamples(series[0], since, queriedAt, true)
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
	for len(times) > 1 && !times[1].at.After(since) {
		times = times[1:]
	}
	if len(values) != len(times) {
		return h, errors.New("unresolved gauge omitted a native scrape")
	}
	for i, v := range values {
		if !v.at.Equal(times[i].at) || v.value > 1 {
			return h, errors.New("unresolved gauge lost its scrape or acquired another incident")
		}
		if !v.at.After(started) && v.value != 0 {
			return h, errors.New("an unresolved incident preceded fault injection")
		}
		if v.value == 1 {
			if !h.cleared.IsZero() {
				return h, errors.New("unresolved incident recurred after recovery")
			}
			if h.appeared.IsZero() {
				h.appeared = v.at.Add(time.Duration(times[i].value * float64(time.Second)))
			}
		} else if !h.appeared.IsZero() && h.cleared.IsZero() {
			h.cleared = v.at.Add(time.Duration(times[i].value * float64(time.Second)))
		}
		h.latest = int(v.value)
	}
	return h, nil
}

// The controller's persisted timestamps date both bounds. Neither a later
// poll nor a later scrape may extend the frozen 45-second delivery target.
func alUnresolvedDelivered(d alDelivery, h alUnresolvedHistory, recorded time.Time) bool {
	return !recorded.IsZero() && !h.appeared.IsZero() && h.cleared.IsZero() && h.latest == 1 && !h.appeared.Before(recorded) &&
		!d.StartsAt.Before(recorded) && !d.ReceivedAt.Before(d.StartsAt) && !d.ReceivedAt.After(recorded.Add(alDetectionSlack))
}
func alUnresolvedCleared(firing, resolved alDelivery, h alUnresolvedHistory, recorded, accounted time.Time) bool {
	return !recorded.IsZero() && accounted.After(recorded) && !h.cleared.IsZero() && !h.cleared.Before(accounted) && h.latest == 0 &&
		!firing.StartsAt.IsZero() && resolved.StartsAt.Equal(firing.StartsAt) && !resolved.EndsAt.Before(accounted) &&
		!resolved.ReceivedAt.Before(resolved.EndsAt) && !resolved.ReceivedAt.After(accounted.Add(alDetectionSlack)) && !h.through.Before(resolved.ReceivedAt)
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
