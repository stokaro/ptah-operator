package e2e

import (
	"errors"
	"maps"
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
)

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
		r.Spec.Policy = ptahv1.ReconciliationPolicy{Apply: policy, AllowDestructive: true}
		return r, nil
	case *ptahv1.PtahMigration:
		r := &ptahv1.PtahMigration{ObjectMeta: metadata, Spec: *v.Spec.DeepCopy()}
		r.Spec.Suspend = false
		r.Spec.Interval.Duration = alNegativeInterval
		r.Spec.Target.RealmRef = nil
		r.Spec.Target.CoordinationKey = "e2e/negative/" + name
		r.Spec.Target.URLFrom.Name = secret
		r.Spec.Policy = ptahv1.MigrationPolicy{Apply: policy}
		return r, nil
	}
	return nil, errors.New("unsupported negative-control family")
}
