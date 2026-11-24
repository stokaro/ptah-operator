package e2e

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	alBlockedAlert = "PtahOperatorResourceBlocked"
	// alBlockedAfter is the profile's blockedAfterSeconds.
	alBlockedAfter = 60 * time.Second
	// alBlockedInterval is the row's refresh cadence. It is shorter than the
	// threshold, so the phase leaves Blocked at least once before the alert
	// may fire and again while it fires: a rule over the phase could not
	// outlast it.
	alBlockedInterval = 30 * time.Second
	alBlockedMetric   = "ptah_operator_blocked_seconds"
	// alBlockedAgeTolerance is how far one scrape's age may sit from the age
	// its timestamp gives: the collector reads the clock during the scrape,
	// and the manager and Prometheus share one kind host.
	alBlockedAgeTolerance = alScrapeTimeout + refusalClockSkew
)

// alBlockedFixture is a copy of the native schema producer that Ptah's own
// verification refuses: its own database and coordination key, no realm, the
// row's interval, and the policy ConfigMap the row names.
func alBlockedFixture(template *ptahv1.PtahSchema, name, secret, policy string) (*ptahv1.PtahSchema, error) {
	if template == nil || template.Spec.Desired.VerificationPolicyFrom == nil || name == "" || secret == "" || policy == "" {
		return nil, errors.New("the blocked fixture needs a producer with a verification policy, a name, a Secret and a policy")
	}
	r := &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: template.Namespace, Name: name}, Spec: *template.Spec.DeepCopy()}
	r.Spec.Suspend = false
	r.Spec.Interval.Duration = alBlockedInterval
	r.Spec.Dev = nil
	r.Spec.Target.RealmRef = nil
	r.Spec.Target.CoordinationKey = "e2e/alerting/blocked/" + name
	alSecretTarget(&r.Spec.Target, secret)
	r.Spec.Desired.VerificationPolicyFrom.Name = policy
	r.Spec.Policy = ptahv1.ReconciliationPolicy{Apply: ptahv1.ApplyPolicyOnApproval, AllowDestructive: true, LockTimeout: template.Spec.Policy.LockTimeout}
	return r, nil
}

// alBlockedLeftovers names every resource, not suspended, that already
// carries status.blockedSince. The alert has no resource label, so any of
// them fires it for its family whatever a row does.
func alBlockedLeftovers(schemas []ptahv1.PtahSchema, migrations []ptahv1.PtahMigration) []string {
	var names []string
	dated := func(kind, namespace, name string, suspended bool, since *metav1.Time) {
		if !suspended && since != nil {
			names = append(names, kind+" "+namespace+"/"+name+" since "+since.UTC().Format(time.RFC3339))
		}
	}
	for _, schema := range schemas {
		dated("PtahSchema", schema.Namespace, schema.Name, schema.Spec.Suspend, schema.Status.BlockedSince)
	}
	for _, migration := range migrations {
		dated("PtahMigration", migration.Namespace, migration.Name, migration.Spec.Suspend, migration.Status.BlockedSince)
	}
	slices.Sort(names)
	return names
}

// alBlockedUnresolved names every blocked notification the receiver got
// firing and has not yet got resolved: its family and the start Alertmanager
// gives both. A repeat of a firing notification is the same incident.
func alBlockedUnresolved(deliveries []alDelivery) []string {
	open := map[string]bool{}
	for _, d := range deliveries {
		if d.AlertName != alBlockedAlert {
			continue
		}
		key := d.Labels["family"] + " since " + d.StartsAt.UTC().Format(time.RFC3339Nano)
		switch d.Status {
		case "firing":
			open[key] = true
		case "resolved":
			delete(open, key)
		}
	}
	names := slices.Collect(maps.Keys(open))
	slices.Sort(names)
	return names
}

// alBlockedPass is one refresh of a blocked schema: the Resolve its interval
// claimed, observed at phase Resolving, and the write that ended it at Blocked
// again.
type alBlockedPass struct {
	id                   string
	started              time.Time
	claimedVersion       string
	blockedAgain         string
	generation           int64
	observedResolvePhase bool
}

// alBlockedWalk is what the row's schema history says about its block.
type alBlockedWalk struct {
	// since is the date the first refused pass wrote, and generation the
	// generation it was refused at.
	since      time.Time
	generation int64
	// refresh is the first complete refresh pass whose Resolve claim started
	// at or after the bound the walk was given. Its id is empty until one
	// has.
	refresh alBlockedPass
	// settled dates the write that cleared the date: the transition of
	// ApprovalRequired to True on the pass that ended at AwaitingApproval.
	// It is zero while the date stands.
	settled           time.Time
	settledGeneration int64
}

// errAlBlockedUnseen is a history that holds no state of the row's schema:
// right after the create, the watch has not delivered it yet.
var errAlBlockedUnseen = errors.New("the history holds no state of the row's schema")

// alBlockedHistory walks the stored states of one schema. The first state
// that carries status.blockedSince has to be the refusal at Blocked, and every
// later state carries that same date -- through each refresh pass and through
// every step of the pass a spec edit starts -- until the first state that
// carries none. That state has to be the pass
// ending at AwaitingApproval, and no state after it carries a date again.
// Anything else is an error, not a reading still to come.
func alBlockedHistory(history []*ptahv1.PtahSchema, uid types.UID, after time.Time) (alBlockedWalk, error) {
	var walk alBlockedWalk
	var claimed *alBlockedPass
	examined := 0
	for _, state := range history {
		if state == nil || uid == "" || state.UID != uid {
			continue
		}
		examined++
		since := state.Status.BlockedSince
		switch {
		case walk.since.IsZero() && since == nil:
			continue
		case walk.since.IsZero():
			if err := nativelyRefusedSchema(state); err != nil {
				return walk, fmt.Errorf("resourceVersion %s dated a block that is not Ptah's refusal: %w", state.ResourceVersion, err)
			}
			walk.since, walk.generation = since.Time, state.Generation
			continue
		case !walk.settled.IsZero():
			if since != nil {
				return walk, fmt.Errorf("resourceVersion %s dated a block again, at %s, after the pass that cleared it", state.ResourceVersion, since.UTC().Format(time.RFC3339))
			}
			continue
		case since == nil:
			approval := meta.FindStatusCondition(state.Status.Conditions, ptahv1.ConditionApprovalRequired)
			if state.Status.Phase != ptahv1.PhaseAwaitingApproval || state.Status.ActiveOperation != nil || approval == nil ||
				approval.Status != metav1.ConditionTrue || approval.ObservedGeneration != state.Generation || state.Status.ObservedGeneration != state.Generation {
				return walk, fmt.Errorf("resourceVersion %s cleared the date at phase %s, before the pass that ends at AwaitingApproval", state.ResourceVersion, state.Status.Phase)
			}
			walk.settled, walk.settledGeneration = approval.LastTransitionTime.Time, state.Generation
			continue
		case !since.Time.Equal(walk.since):
			return walk, fmt.Errorf("resourceVersion %s moved status.blockedSince from %s to %s at phase %s", state.ResourceVersion,
				walk.since.UTC().Format(time.RFC3339), since.UTC().Format(time.RFC3339), state.Status.Phase)
		case state.Status.ActiveOperation == nil && slices.Contains([]ptahv1.ReconciliationPhase{
			ptahv1.PhaseAwaitingApproval, ptahv1.PhaseReadyToApply, ptahv1.PhaseInSync,
		}, state.Status.Phase):
			return walk, fmt.Errorf("resourceVersion %s carried status.blockedSince into %s, where no person is needed", state.ResourceVersion, state.Status.Phase)
		}
		if walk.refresh.id != "" || state.Generation != walk.generation {
			continue
		}
		active := state.Status.ActiveOperation
		switch {
		case active != nil && active.Type == ptahv1.OperationResolve && !active.StartedAt.Time.Before(after) && (claimed == nil || claimed.id != active.ID):
			claimed = &alBlockedPass{id: active.ID, started: active.StartedAt.Time, claimedVersion: state.ResourceVersion, generation: state.Generation}
			claimed.observedResolvePhase = state.Status.Phase == ptahv1.PhaseResolving
		case active != nil && claimed != nil && active.ID == claimed.id:
			claimed.observedResolvePhase = claimed.observedResolvePhase || state.Status.Phase == ptahv1.PhaseResolving
		case active == nil && claimed != nil && nativelyRefusedSchema(state) == nil:
			// A pass whose Resolving state the watch did not deliver is not
			// the one the row observes; the next one may be. A handoff
			// between the pass's steps carries no claim and is not Blocked,
			// so it keeps the pass open.
			if claimed.observedResolvePhase {
				claimed.blockedAgain = state.ResourceVersion
				walk.refresh = *claimed
			}
			claimed = nil
		}
	}
	if examined == 0 {
		return walk, errAlBlockedUnseen
	}
	return walk, nil
}

// alBlockedDelivered is the firing notification on time: no earlier than the
// threshold after the persisted date, and within the detection slack after
// it.
func alBlockedDelivered(firing alDelivery, since time.Time) bool {
	threshold := since.Add(alBlockedAfter)
	return !since.IsZero() && !firing.StartsAt.Before(threshold.Add(-refusalClockSkew)) &&
		!firing.ReceivedAt.Before(firing.StartsAt) && !firing.ReceivedAt.After(threshold.Add(alDetectionSlack))
}

// alBlockedCleared is the one alert that fired before the refresh pass ending
// only after the date cleared, within the hold and the detection slack. A
// resolution between the two would carry an earlier end, and a new firing
// another start.
func alBlockedCleared(firing, resolved alDelivery, refreshed, settled time.Time) bool {
	return !settled.IsZero() && !refreshed.IsZero() && !firing.StartsAt.IsZero() && firing.StartsAt.Before(refreshed) &&
		resolved.StartsAt.Equal(firing.StartsAt) && !resolved.EndsAt.Before(settled) &&
		!resolved.ReceivedAt.Before(resolved.EndsAt) && !resolved.ReceivedAt.After(settled.Add(alStateReadGapHold+alDetectionSlack))
}

// alBlockedAges holds every native sample of the schema family's
// ptah_operator_blocked_seconds between from and to to the age the persisted
// date gives at that sample's own timestamp. A gauge that a refresh pass
// reset, or that dated the block by anything else, leaves a sample off it.
// At least one series has to cover the whole window, one scrape from each
// end, and each series read has to have a sample in it.
func alBlockedAges(body []byte, since, from, to, queriedAt time.Time) (int, error) {
	if since.IsZero() || !from.Before(to) || to.After(queriedAt) {
		return 0, errors.New("the blocked-age window is not inside the history read")
	}
	series, err := alAdmissionNativeMatrix(body)
	if err != nil {
		return 0, err
	}
	counted, covered := 0, false
	for _, row := range series {
		if row.Metric["__name__"] != alBlockedMetric || row.Metric["family"] != "schema" || row.Metric["job"] != alScrapeJob {
			return 0, fmt.Errorf("the blocked-age history holds another series: %v", row.Metric)
		}
		samples, err := alNativeSamples(row, from, queriedAt, false, false)
		if err != nil {
			return 0, err
		}
		var inside []alAdmissionSample
		for _, sample := range samples {
			if sample.at.Before(from) || sample.at.After(to) {
				continue
			}
			want := sample.at.Sub(since).Seconds()
			if math.Abs(sample.value-want) > alBlockedAgeTolerance.Seconds() {
				return 0, fmt.Errorf("the age read %.3fs at %s, and the persisted date gives %.3fs", sample.value, sample.at.Format(time.RFC3339Nano), want)
			}
			inside = append(inside, sample)
		}
		if len(inside) == 0 {
			return 0, fmt.Errorf("series %v has no sample inside the window", row.Metric)
		}
		counted += len(inside)
		covered = covered || inside[0].at.Sub(from) <= alAdmissionSampleGap && to.Sub(inside[len(inside)-1].at) <= alAdmissionSampleGap
	}
	if counted == 0 {
		return 0, errors.New("the blocked-age history holds no sample")
	}
	if !covered {
		return counted, errors.New("no series covers the whole window")
	}
	return counted, nil
}
