package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	alNegativeWindow   = 10 * time.Minute
	alNegativeInterval = 2 * time.Minute
	// A refresh may perform Resolve, Verify, Observe and Plan. Every claim
	// still owes the frozen stalled threshold, and its scheduled interval
	// still owes the overdue threshold; this is only a liveness cross-check.
	alNegativeFreshness = alNegativeInterval + 4*alStalledAfter
	// alViewReadResolution is the profile's resolution deadline for the
	// state-read alert, counted from the last failure: the rule's 60-second
	// window and the delivery allowance. A failed read explains what the quiet
	// window sees of that alert for this long after the scrape before it, so
	// the window's native history starts this long before its first look.
	alViewReadResolution = alViewReadWindow + alDetectionSlack
	// alQuietCloseWait bounds the wait for a history that reads the state past
	// the window's close: a declared run still open then ends within the hold,
	// and the next scrape completes within an interval and its timeout.
	alQuietCloseWait = alStateReadGapHold + 2*alScrapeInterval + alScrapeTimeout
)

var errAlQuietWindowOpen = errors.New("the native history has not read the state past the close of the quiet window")

type alNegativeState struct {
	claim                                    alStalledClaim
	observed                                 int64
	suspended, applied, unresolved, approved bool
	failed                                   bool
	policy                                   ptahv1.ApplyPolicy
	interval                                 time.Duration
	plan                                     bool
	waiting, disabled                        bool
	readAt                                   time.Time
}

func alNegativeReading(object client.Object) alNegativeState {
	r := alNegativeState{claim: alStalledReading(object)}
	var conditions []metav1.Condition
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		r.observed, r.suspended, r.policy, r.interval = v.Status.ObservedGeneration, v.Spec.Suspend, v.Spec.Policy.Apply, v.Spec.Interval.Duration
		r.applied, r.unresolved = v.Status.Applied != nil, v.Status.PendingObservation != nil
		if v.Status.Plan != nil {
			r.plan = v.Status.Plan.Name != "" && v.Status.Plan.UID != ""
			r.approved = v.Status.Plan.Approval != nil
		}
		if v.Status.Target.LastObservedAt != nil {
			r.readAt = v.Status.Target.LastObservedAt.Time
		}
		if v.Status.ActiveOperation != nil {
			r.failed = v.Status.ActiveOperation.Attempt > 1
		}
		conditions = v.Status.Conditions
	case *ptahv1.PtahMigration:
		r.observed, r.suspended, r.policy, r.interval = v.Status.ObservedGeneration, v.Spec.Suspend, v.Spec.Policy.Apply, v.Spec.Interval.Duration
		r.applied, r.unresolved = v.Status.LastRun != nil, v.Status.UnresolvedRun != nil
		if v.Status.ActiveOperation != nil {
			r.failed = v.Status.ActiveOperation.Attempt > 1 || v.Status.ActiveOperation.RetryNotBefore != nil
		}
		r.plan = v.Status.Plan != nil && v.Status.Plan.Name != "" && v.Status.Plan.UID != ""
		if v.Status.History != nil {
			r.readAt = v.Status.History.ObservedAt.Time
		}
		conditions = v.Status.Conditions
	}
	for _, c := range conditions {
		if c.ObservedGeneration != object.GetGeneration() {
			continue
		}
		r.failed = r.failed || c.Type == "ReconciliationFailed" && c.Status == metav1.ConditionTrue
		r.waiting = r.waiting || c.Type == "ApprovalRequired" && c.Status == metav1.ConditionTrue
		r.disabled = r.disabled || c.Type == "Ready" && c.Status == metav1.ConditionFalse && c.Reason == "ApplyDisabled"
	}
	return r
}

func (r alNegativeState) safe() bool {
	if r.claim.uid == "" || r.claim.generation <= 0 || r.claim.namespace == "" || (r.claim.family != "schema" && r.claim.family != "migration") || r.observed != r.claim.generation || r.suspended || r.applied || r.unresolved || r.approved || r.failed || r.interval != alNegativeInterval {
		return false
	}
	if r.policy != ptahv1.ApplyPolicyOnApproval && r.policy != ptahv1.ApplyPolicyNever {
		return false
	}
	if (r.claim.id == "") != (r.claim.operation == "") {
		return false
	}
	switch r.claim.operation {
	case "", "Resolve", "Verify":
	case "Observe", "Plan":
		if r.claim.family != "schema" {
			return false
		}
	case "History":
		if r.claim.family != "migration" {
			return false
		}
	default:
		return false
	}
	return r.claim.phase != "Failed"
}

// A direct read must still identify the original control. Collection watches
// can ignore unrelated UIDs, but a replacement returned for this name cannot.
func (r alNegativeState) unchanged(before alNegativeState) bool {
	return before.claim.sameResource(r.claim) && r.policy == before.policy && r.safe()
}

func (r alNegativeState) gated() bool {
	return r.safe() && r.claim.id == "" && r.plan && !r.readAt.IsZero() &&
		(r.policy == ptahv1.ApplyPolicyOnApproval && r.waiting || r.policy == ptahv1.ApplyPolicyNever && r.disabled)
}

// A pre-existing unresolved incident is not created by these controls. Only
// an exact repeat of that recorded incident may appear in the quiet window.
// Changed counts, labels, starts, receiver, or a different rule are refused.
func alNegativeRepeat(d alDelivery, baseline map[string]alDelivery) bool {
	old, ok := baseline[d.Labels["family"]]
	return ok && d.AlertName == alUnresolvedApply && old.AlertName == d.AlertName && d.Status == old.Status && (d.Status == "firing" || d.Status == "resolved" && !d.EndsAt.IsZero() && d.EndsAt.Equal(old.EndsAt)) &&
		d.Receiver == old.Receiver && !d.StartsAt.IsZero() && d.StartsAt.Equal(old.StartsAt) && maps.Equal(d.Labels, old.Labels) && maps.Equal(d.Annotations, old.Annotations)
}

// Each fixture starts with a clean identity and status, but uses the actual
// source and executor configuration already exercised by its producer phase.
func alNegativeFixture(template client.Object, name, secret string, policy ptahv1.ApplyPolicy) (client.Object, error) {
	if policy != ptahv1.ApplyPolicyOnApproval && policy != ptahv1.ApplyPolicyNever {
		return nil, errors.New("unsupported negative-control policy")
	}
	metadata := metav1.ObjectMeta{Namespace: template.GetNamespace(), Name: name}
	switch v := template.(type) {
	case *ptahv1.PtahSchema:
		r := &ptahv1.PtahSchema{ObjectMeta: metadata, Spec: *v.Spec.DeepCopy()}
		r.Spec.Suspend = false
		r.Spec.Interval.Duration = alNegativeInterval
		r.Spec.Dev = nil
		r.Spec.Target.RealmRef = nil
		r.Spec.Target.CoordinationKey = "e2e/negative/" + name
		r.Spec.Target.URLFrom.Name = secret
		r.Spec.Policy = ptahv1.ReconciliationPolicy{Apply: policy, AllowDestructive: true, LockTimeout: v.Spec.Policy.LockTimeout}
		return r, nil
	case *ptahv1.PtahMigration:
		r := &ptahv1.PtahMigration{ObjectMeta: metadata, Spec: *v.Spec.DeepCopy()}
		r.Spec.Suspend = false
		r.Spec.Interval.Duration = alNegativeInterval
		r.Spec.Target.RealmRef = nil
		r.Spec.Target.CoordinationKey = "e2e/negative/" + name
		r.Spec.Target.URLFrom.Name = secret
		r.Spec.Policy = ptahv1.MigrationPolicy{Apply: policy, LockTimeout: v.Spec.Policy.LockTimeout}
		return r, nil
	}
	return nil, errors.New("unsupported negative-control family")
}

// alNegativeDatabaseUnchanged holds a control that never applied to an empty
// database.
func alNegativeDatabaseUnchanged(family, tables, historyRows string) bool {
	switch family {
	// Ptah's History reads the revision table only when it exists and never
	// creates it, so a migration that was never applied leaves its database
	// as empty as a schema that was never applied.
	case "schema", "migration":
		return strings.TrimSpace(tables) == "" && historyRows == ""
	default:
		return false
	}
}

// alQuietInstallation names the notifications the quiet window allows: the
// state-read alert's, from this installation, to its receiver.
type alQuietInstallation struct {
	receiver, namespace, metricsService string
}

// allows is a firing or resolved notification of the state-read alert from
// this installation's Prometheus to its receiver, carrying the alert's own
// labels and no other.
func (q alQuietInstallation) allows(d alDelivery) bool {
	labels := map[string]string{"alertname": alViewReadAlert, "severity": "warning", "operator_namespace": q.namespace, "operator_metrics_service": q.metricsService}
	if d.AlertName != alViewReadAlert || d.Receiver != q.receiver || !maps.Equal(d.Labels, labels) || d.StartsAt.IsZero() || d.ReceivedAt.Before(d.StartsAt) {
		return false
	}
	switch d.Status {
	case "firing":
		return true
	case "resolved":
		return !d.EndsAt.Before(d.StartsAt)
	}
	return false
}

// alQuietFiring reads an instant answer for ALERTS{alertstate="firing"}. The
// state-read alert, with its own labels and no other, is the one alert the
// quiet window allows; any other series is an incident. It reports whether
// the allowed alert was firing. An answer that is not an explicit instant
// vector is an error, so an empty result cannot stand in for one Prometheus
// never gave.
func alQuietFiring(body []byte) (bool, error) {
	var answer struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return false, fmt.Errorf("the alert answer is not JSON: %w", err)
	}
	if answer.Status != "success" || answer.Data.ResultType != "vector" || answer.Data.Result == nil {
		return false, errors.New("the alert query did not return an explicit instant vector")
	}
	allowed := map[string]string{"__name__": "ALERTS", "alertname": alViewReadAlert, "alertstate": "firing", "severity": "warning"}
	for _, series := range answer.Data.Result {
		if !maps.Equal(series.Metric, allowed) {
			return false, fmt.Errorf("%s is firing with labels %v", series.Metric["alertname"], series.Metric)
		}
	}
	return len(answer.Data.Result) != 0, nil
}

// alQuietSeen is what the quiet window saw of the one alert it allows: the
// instants Prometheus answered with it firing, and its notifications.
type alQuietSeen struct {
	firing        []time.Time
	notifications []alDelivery
}

// alQuietWindowHistory is what one native snapshot says about the leader's
// state reads from the window's lookback to the instant it was read.
type alQuietWindowHistory struct {
	// declared are the leader's declared failed reads, in scrape order.
	declared []alDeclaredRead
	// through is the last leader scrape that read the state, which every
	// manager's scrapes also reach.
	through time.Time
}

// alReadQuietWindowHistory judges the leader's state reads from from to
// queriedAt. Each family's census is read as the unresolved row reads its
// gauge: every manager scrapes without a gap, a scrape without the gauge counts
// only where the leader declared it, with its view at 0 and its read-failure
// counter risen, a run of declared reads ends within the rules' hold, and every
// reading reports a synchronized view. Here the census must read zero
// throughout, as it did before the window, and the counter, which the rule
// reads on every manager, must not rise on a follower.
func alReadQuietWindowHistory(groups map[string][]byte, pods []string, leader string, from, queriedAt time.Time) (alQuietWindowHistory, error) {
	var q alQuietWindowHistory
	census, err := alAdmissionNativeMatrix(groups[alUnresolvedMetric])
	if err != nil {
		return q, err
	}
	families := map[string][]alAdmissionSeries{}
	for _, series := range census {
		families[series.Metric["family"]] = append(families[series.Metric["family"]], series)
	}
	if len(families) != 2 {
		return q, errors.New("the unresolved census must name the schema and migration families and no other")
	}
	for _, family := range []string{"schema", "migration"} {
		gauge, err := alMatrixAnswer(families[family])
		if err != nil {
			return q, err
		}
		h, err := alReadUnresolvedHistory(gauge, groups[alViewSyncedMetric], groups[alViewReadFailuresMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, family, from, queriedAt)
		if err != nil {
			return q, fmt.Errorf("%s census: %w", family, err)
		}
		if !h.appeared.IsZero() {
			return q, fmt.Errorf("the %s unresolved census changed during the quiet window", family)
		}
		q.declared, q.through = h.declared, h.through
	}
	if _, err := alReadViewHistory(groups[alViewReadFailuresMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, from, queriedAt); err != nil {
		return q, err
	}
	return q, nil
}

// alQuietWindowVerdict holds what the quiet window saw of the state-read
// alert to the failed reads the leader declared. The rule fires only on a
// counter that rose within its 60-second window, and the profile resolves it
// within alViewReadResolution of the last failure, so every instant the alert
// was seen firing, and every start, receipt and end of its notifications,
// must fall within that span after the scrape before a declared failed read.
// A history that has not read the state past the window's close cannot judge
// its end, and says so with errAlQuietWindowOpen.
func alQuietWindowVerdict(h alQuietWindowHistory, seen alQuietSeen, closed time.Time) error {
	if h.through.Before(closed) {
		return errAlQuietWindowOpen
	}
	instants := slices.Clone(seen.firing)
	for _, d := range seen.notifications {
		instants = append(instants, d.StartsAt, d.ReceivedAt)
		if d.Status == "resolved" {
			instants = append(instants, d.EndsAt)
		}
	}
	for _, at := range instants {
		if !slices.ContainsFunc(h.declared, func(r alDeclaredRead) bool {
			return r.lower.Before(at) && !at.After(r.lower.Add(alViewReadResolution))
		}) {
			return fmt.Errorf("%s was seen at %s with no failed read the leader declared in the %s before it",
				alViewReadAlert, at.Format(time.RFC3339Nano), alViewReadResolution)
		}
	}
	return nil
}
