package e2e

import (
	"errors"
	"fmt"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const alLockAlert = "PtahOperatorLockReleaseOwed"
const alLockPendingFor = 60 * time.Second

func alLockApprovalReady(object client.Object) bool {
	r := alNegativeReading(object)
	return r.claim.uid != "" && r.claim.generation > 0 && r.observed == r.claim.generation && r.claim.id == "" &&
		r.interval == time.Hour && r.policy == ptahv1.ApplyPolicyOnApproval && r.plan && r.waiting && !r.readAt.IsZero() &&
		!r.suspended && !r.approved && !r.applied && !r.unresolved && !r.failed
}

// A successful read-only Plan can publish changes or prove convergence without
// creating a plan object. Both outcomes must belong to the current generation.
func alLockSiblingReady(v *ptahv1.PtahSchema) bool {
	if v.UID == "" || v.Generation < 1 || v.Status.ObservedGeneration != v.Generation || v.Spec.Policy.Apply != ptahv1.ApplyPolicyNever || v.Spec.Suspend || v.Status.ActiveOperation != nil || v.Status.PendingLockRelease != nil || v.Status.PendingObservation != nil || v.Status.Applied != nil {
		return false
	}
	currentTrue := func(kind string) bool {
		for _, c := range v.Status.Conditions {
			if c.Type == kind {
				return c.ObservedGeneration == v.Generation && c.Status == metav1.ConditionTrue
			}
		}
		return false
	}
	if v.Status.Plan != nil {
		return v.Status.Plan.Name != "" && v.Status.Plan.UID != "" && v.Status.Plan.Approval == nil && currentTrue(ptahv1.ConditionPlanReady)
	}
	return currentTrue(ptahv1.ConditionInSync) && currentTrue(ptahv1.ConditionReady)
}

func alLockTransportFinished(pod *corev1.Pod) (time.Time, error) {
	if pod.UID == "" || pod.Status.Phase != corev1.PodSucceeded || len(pod.Spec.Containers) == 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return time.Time{}, errors.New("no complete successful executor")
	}
	var finished time.Time
	for _, group := range []struct {
		containers []corev1.Container
		statuses   []corev1.ContainerStatus
	}{
		{pod.Spec.InitContainers, pod.Status.InitContainerStatuses}, {pod.Spec.Containers, pod.Status.ContainerStatuses},
	} {
		if len(group.containers) != len(group.statuses) {
			return time.Time{}, errors.New("executor omitted a container outcome")
		}
		wanted := map[string]bool{}
		for _, c := range group.containers {
			if c.Name == "" || wanted[c.Name] {
				return time.Time{}, errors.New("ambiguous executor containers")
			}
			wanted[c.Name] = true
		}
		for _, s := range group.statuses {
			terminal := s.State.Terminated
			if !wanted[s.Name] || terminal == nil || terminal.ExitCode != 0 || s.RestartCount != 0 || s.LastTerminationState.Terminated != nil ||
				terminal.StartedAt.IsZero() || terminal.FinishedAt.IsZero() || terminal.FinishedAt.Before(&terminal.StartedAt) {
				return time.Time{}, errors.New("executor has a missing, failed or repeated container")
			}
			delete(wanted, s.Name)
			if terminal.FinishedAt.Time.After(finished) {
				finished = terminal.FinishedAt.Time
			}
		}
	}
	return finished, nil
}

// The same Lease's watch orders release before the sibling's acquisition.
// Job creation timestamps have only second precision and cannot establish
// that ordering against the Lease's microsecond timestamp.
func alLockHandoff(history []*coordinationv1.Lease, original *coordinationv1.Lease, next ptahv1.TargetLockReleaseStatus, released time.Time) bool {
	if next.LeaseEpoch == "" || next.LeaseEpoch == original.Annotations[annotationLeaseEpoch] || released.IsZero() {
		return false
	}
	seenRelease := false
	for _, lease := range history {
		if lease.UID != original.UID || lease.Namespace != original.Namespace || lease.Name != original.Name {
			continue
		}
		if lease.Spec.HolderIdentity == nil || lease.Spec.RenewTime == nil {
			return false
		}
		if lease.Annotations[annotationLeaseEpoch] == original.Annotations[annotationLeaseEpoch] && *lease.Spec.HolderIdentity == "" && lease.Spec.RenewTime.Time.Equal(released) {
			seenRelease = true
		}
		if lease.Annotations[annotationLeaseEpoch] == next.LeaseEpoch && *lease.Spec.HolderIdentity != "" {
			return seenRelease && !lease.Spec.RenewTime.Time.Before(released) && original.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != *original.Spec.HolderIdentity
		}
	}
	return false
}

type alLockState struct {
	observed    int64
	claim       alStalledClaim
	lock        ptahv1.TargetLockReleaseStatus
	owed        bool
	completed   time.Time
	finishedJob string
	unresolved  bool
}

func alLockReading(object client.Object) alLockState {
	r := alLockState{claim: alStalledReading(object)}
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		r.observed = v.Status.ObservedGeneration
		if op := v.Status.ActiveOperation; op != nil {
			r.lock = ptahv1.TargetLockReleaseStatus{CoordinationDigest: op.CoordinationDigest, OperationID: op.ID, LeaseEpoch: op.LeaseEpoch, LeaseDurationSeconds: op.LeaseDurationSeconds}
		}
		if v.Status.PendingLockRelease != nil {
			r.lock = *v.Status.PendingLockRelease
			r.owed = true
		}
		r.unresolved = v.Status.PendingObservation != nil
		if v.Status.Applied != nil {
			r.completed = v.Status.Applied.CompletedAt.Time
		}
	case *ptahv1.PtahMigration:
		r.observed = v.Status.ObservedGeneration
		if op := v.Status.ActiveOperation; op != nil {
			r.lock = ptahv1.TargetLockReleaseStatus{CoordinationDigest: op.CoordinationDigest, OperationID: op.ID, LeaseEpoch: op.LeaseEpoch, LeaseDurationSeconds: op.LeaseDurationSeconds}
		}
		if v.Status.PendingLockRelease != nil {
			r.lock = *v.Status.PendingLockRelease
			r.owed = true
		}
		r.unresolved = v.Status.UnresolvedRun != nil
		if run := v.Status.LastRun; run != nil && run.Outcome == ptahv1.MigrationRunOutcomeApplied && run.FinishedAt != nil {
			r.completed = run.FinishedAt.Time
			r.finishedJob = string(run.JobUID)
		}
	}
	return r
}

func (r alLockState) claimed() bool {
	c := r.claim
	return (c.family == "schema" || c.family == "migration") && c.uid != "" && c.generation > 0 && r.observed == c.generation && c.name != "" && c.namespace != "" && c.operation == "Apply" && c.id != "" && c.jobUID != "" && c.jobName != "" && !c.started.IsZero() &&
		r.lock.OperationID == c.id && r.lock.CoordinationDigest != "" && r.lock.LeaseEpoch != "" && r.lock.LeaseDurationSeconds > 0 && !r.owed
}

func (r alLockState) owes(before alLockState) bool {
	return before.claimed() && before.claim.sameResource(r.claim) && r.observed == r.claim.generation && r.claim.id == "" && r.owed && r.lock == before.lock && !r.unresolved && !r.completed.IsZero() &&
		!r.completed.Before(before.claim.started) && (r.claim.family == "schema" || r.finishedJob == string(before.claim.jobUID))
}

func alLockWorkload(job *batchv1.Job, pod *corev1.Pod, claim alStalledClaim) bool {
	kind := "PtahSchema"
	if claim.family == "migration" {
		kind = "PtahMigration"
	}
	owner := metav1.GetControllerOf(job)
	parent := metav1.GetControllerOf(pod)
	return (claim.family == "schema" || claim.family == "migration") && claim.id != "" && claim.jobUID != "" && claim.uid != "" && job.UID == claim.jobUID && job.Namespace == claim.namespace && job.Name == claim.jobName && job.DeletionTimestamp == nil &&
		job.Annotations["operator.ptah.run/operation-id"] == claim.id && job.Labels["operator.ptah.run/operation"] == strings.ToLower(claim.operation) &&
		owner != nil && owner.UID == claim.uid && owner.Name == claim.name && owner.Kind == kind && owner.APIVersion == ptahv1.GroupVersion.String() &&
		pod.UID != "" && pod.Namespace == job.Namespace && pod.DeletionTimestamp == nil && parent != nil && parent.UID == job.UID && parent.Kind == "Job" && parent.Name == job.Name && parent.APIVersion == "batch/v1" &&
		pod.Annotations["operator.ptah.run/operation-id"] == claim.id && pod.Labels["operator.ptah.run/operation"] == strings.ToLower(claim.operation)
}

func alLockHeld(lease, original *coordinationv1.Lease, binding ptahv1.TargetLockReleaseStatus) bool {
	return original.UID != "" && lease.UID == original.UID && lease.Name == original.Name && lease.Namespace == original.Namespace && lease.DeletionTimestamp == nil &&
		original.Spec.HolderIdentity != nil && *original.Spec.HolderIdentity != "" && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == *original.Spec.HolderIdentity &&
		lease.Annotations[annotationLeaseEpoch] == binding.LeaseEpoch && lease.Spec.LeaseDurationSeconds != nil && *lease.Spec.LeaseDurationSeconds == binding.LeaseDurationSeconds
}

func alLockReleased(lease, original *coordinationv1.Lease, after time.Time) (time.Time, bool) {
	if original.UID == "" || lease.UID != original.UID || lease.Namespace != original.Namespace || lease.Name != original.Name || lease.DeletionTimestamp != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "" ||
		lease.Annotations[annotationLeaseEpoch] != original.Annotations[annotationLeaseEpoch] || lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.After(after) {
		return time.Time{}, false
	}
	return lease.Spec.RenewTime.Time, true
}

// Use the controller's completed-result timestamp, written with its owed
// release, as the start. Lease.RenewTime dates the actual release update.
func alLockDelivered(d alDelivery, completed time.Time) bool {
	return !completed.IsZero() && !d.StartsAt.Before(completed.Add(alLockPendingFor)) && !d.ReceivedAt.Before(d.StartsAt) && !d.ReceivedAt.After(completed.Add(alLockPendingFor+alDetectionSlack))
}

func alLockReleasePolicy(lease *coordinationv1.Lease, manager string) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
	name := "ptah-e2e-alert-lock-" + string(lease.UID)
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy:    ptr.To(admissionv1.Fail),
		MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{Operations: []admissionv1.OperationType{admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{"coordination.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"leases"}, Scope: ptr.To(admissionv1.NamespacedScope)}}}}},
		MatchConditions:  []admissionv1.MatchCondition{{Name: "exact-lease-and-manager", Expression: fmt.Sprintf("object.metadata.uid == %q && object.metadata.name == %q && object.metadata.namespace == %q && request.userInfo.username == %q", lease.UID, lease.Name, lease.Namespace, manager)}},
		Validations:      []admissionv1.Validation{{Expression: "!(has(oldObject.spec.holderIdentity) && oldObject.spec.holderIdentity != '' && (!has(object.spec.holderIdentity) || object.spec.holderIdentity == ''))", Message: releaseFaultMessage, Reason: ptr.To(metav1.StatusReasonForbidden)}},
	}}
	return policy, &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}}}
}
