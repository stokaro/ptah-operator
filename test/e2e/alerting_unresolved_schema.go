package e2e

import (
	"errors"
	"slices"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type alSchemaUnresolvedTrace struct{ recorded, observed, accounted time.Time }

// Applying changes from True to False when Unknown is persisted. Its first
// transition dates firing; the successful proof Plan's LastAttemptTime dates
// resolution. Later polls and refreshes must not move either deadline.
func alReadSchemaUnresolvedTrace(events []watchEvent[*ptahv1.PtahSchema], before *ptahv1.PtahSchema, recovered bool) (alSchemaUnresolvedTrace, error) {
	h := alSchemaUnresolvedTrace{}
	if before == nil || before.UID == "" || before.Status.ActiveOperation == nil || before.Status.Plan == nil || before.Status.ActiveOperation.Target == nil || before.Status.ActiveOperation.Source == nil {
		return h, errors.New("no original schema Apply binding")
	}
	apply := before.Status.ActiveOperation
	applying := meta.FindStatusCondition(before.Status.Conditions, ptahv1.ConditionApplying)
	if apply.Type != ptahv1.OperationApply || apply.ID == "" || apply.JobUID == "" || apply.ExecutionNotAfter == nil || apply.TerminationGracePeriodSeconds < 1 || applying == nil || applying.Status != metav1.ConditionTrue {
		return h, errors.New("the original schema never entered Apply")
	}
	var origin *ptahv1.PendingObservationStatus
	var proof, observe *ptahv1.ActiveOperationStatus
	var seenPod string
	for _, e := range events {
		v := e.Object
		if v == nil || v.Name != before.Name || v.Namespace != before.Namespace {
			continue
		}
		if v.UID != before.UID || e.Type == watch.Deleted || v.DeletionTimestamp != nil {
			return h, errors.New("unresolved schema was replaced or deleted")
		}
		expected := before.Spec.DeepCopy()
		expected.Suspend = v.Spec.Suspend
		if !equality.Semantic.DeepEqual(*expected, v.Spec) || v.Generation < before.Generation || v.Generation > before.Generation+2 {
			return h, errors.New("unresolved schema changed more than suspension")
		}
		p := v.Status.PendingObservation
		if p == nil {
			if origin == nil {
				continue
			}
			if h.accounted.IsZero() {
				if proof == nil || proof.Type != ptahv1.OperationPlan || h.observed.IsZero() || v.Status.LastAttemptTime == nil || !v.Status.LastAttemptTime.After(h.observed) || v.Status.LastAttemptTime.Before(&proof.StartedAt) || v.Status.ActiveOperation != nil || v.Status.Applied != nil || v.Spec.Suspend {
					return h, errors.New("Unknown disappeared without a completed read-only proof")
				}
				h.accounted = v.Status.LastAttemptTime.Time
			}
			if v.Status.Applied != nil || v.Status.ActiveOperation != nil && v.Status.ActiveOperation.Type == ptahv1.OperationApply {
				return h, errors.New("recovery acquired mutation authority before fresh approval")
			}
			continue
		}
		if !h.accounted.IsZero() {
			return h, errors.New("schema became unresolved again")
		}
		if origin == nil {
			c := meta.FindStatusCondition(v.Status.Conditions, ptahv1.ConditionApplying)
			if p.Outcome != ptahv1.PendingObservationOutcomeUnknown || p.ApplyOperationID != apply.ID || p.ApplyJobName != apply.JobName || p.ApplyJobUID != apply.JobUID || p.ApplyGeneration != before.Generation || !equality.Semantic.DeepEqual(p.Plan, *before.Status.Plan) || !equality.Semantic.DeepEqual(p.AdmissionSnapshot, apply.AdmissionSnapshot) || activeMatchesPending(apply, p) != nil || !slices.Equal(apply.ObservationProtectedTables, p.ProtectedTables) || p.ObserveAfter == nil || !p.ObserveAfter.Time.Equal(apply.ExecutionNotAfter.Add(time.Duration(apply.TerminationGracePeriodSeconds)*time.Second)) || c == nil || c.Status != metav1.ConditionFalse || c.Reason != string(ptahv1.ReasonOutcomeUnknown) || !c.LastTransitionTime.After(applying.LastTransitionTime.Time) {
				return h, errors.New("Unknown lost the original Apply or its persisted transition")
			}
			origin = p.DeepCopy()
			h.recorded = c.LastTransitionTime.Time
		}
		// Pod discovery can follow the Unknown write. Evidence may grow from
		// zero to one original executor, but it must not shrink or name two.
		if p.ApplyPodCount != int32(len(p.ApplyPodUIDs)) || len(p.ApplyPodUIDs) > 1 {
			return h, errors.New("unknown Apply has ambiguous Pod evidence")
		}
		if len(p.ApplyPodUIDs) == 1 {
			uid := string(p.ApplyPodUIDs[0])
			if uid == "" || seenPod != "" && seenPod != uid {
				return h, errors.New("unknown Apply changed its executor Pod")
			}
			seenPod = uid
		} else if seenPod != "" {
			return h, errors.New("unknown Apply lost recorded Pod evidence")
		}
		copy, binding := p.DeepCopy(), origin.DeepCopy()
		copy.PlanRequired = binding.PlanRequired
		copy.ApplyPodUIDs, binding.ApplyPodUIDs = nil, nil
		copy.ApplyPodCount, binding.ApplyPodCount = 0, 0
		if !equality.Semantic.DeepEqual(copy, binding) || v.Status.Applied != nil {
			return h, errors.New("the immutable unknown Apply binding changed")
		}
		active := v.Status.ActiveOperation
		if active == nil {
			continue
		}
		if active.Type == ptahv1.OperationApply {
			if active.ID != apply.ID || active.JobUID != apply.JobUID {
				return h, errors.New("Unknown replayed Apply")
			}
			continue
		}
		if v.Spec.Suspend || (active.Type != ptahv1.OperationObserve && active.Type != ptahv1.OperationPlan) || active.ID == "" || active.JobName == "" || activeMatchesPending(active, p) != nil || !slices.Equal(active.ObservationProtectedTables, p.ProtectedTables) || active.StartedAt.Before(&metav1.Time{Time: h.recorded}) || p.ObserveAfter != nil && active.StartedAt.Before(p.ObserveAfter) {
			return h, errors.New("recovery did not preserve its read-only target and execution horizon")
		}
		// Claims are persisted before Kubernetes assigns a Job UID. Validate
		// their binding above, but count execution only after that UID exists.
		if active.Type == ptahv1.OperationObserve && active.JobUID != "" {
			observe = active.DeepCopy()
			proof = nil
		}
		if active.Type == ptahv1.OperationPlan {
			at := v.Status.Target.LastObservedAt
			if observe == nil || !p.PlanRequired || at == nil || !at.After(h.recorded) || at.Before(&observe.StartedAt) || active.StartedAt.Before(at) || v.Status.Target.IdentityDigest != p.Plan.TargetIdentityDigest {
				return h, errors.New("recovery Plan has no fresh observation of the original target")
			}
			h.observed = at.Time
			if active.JobUID != "" {
				proof = active.DeepCopy()
			}
		}
	}
	if origin == nil || recovered && h.accounted.IsZero() || !recovered && !h.accounted.IsZero() {
		return h, errors.New("schema history does not reach the required recovery boundary")
	}
	return h, nil
}
