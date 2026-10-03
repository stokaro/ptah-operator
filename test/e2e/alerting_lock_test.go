package e2e

import (
	"strings"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func lockAlertFixture(family string) (client.Object, alLockState) {
	at := metav1.NewTime(time.Unix(1800000000, 0).UTC())
	metadata := metav1.ObjectMeta{Namespace: "ns", Name: "owner", UID: "owner-uid", Generation: 2}
	lock := ptahv1.TargetLockReleaseStatus{CoordinationDigest: "sha256:" + strings.Repeat("a", 64), OperationID: "sha256:" + strings.Repeat("b", 64), LeaseEpoch: "v1-" + strings.Repeat("c", 32), LeaseDurationSeconds: 360}
	var object client.Object
	if family == "schema" {
		object = &ptahv1.PtahSchema{ObjectMeta: metadata, Status: ptahv1.PtahSchemaStatus{ObservedGeneration: 2, ActiveOperation: &ptahv1.ActiveOperationStatus{ID: lock.OperationID, Type: ptahv1.OperationApply, StartedAt: at, JobName: "apply", JobUID: "apply-uid", CoordinationDigest: lock.CoordinationDigest, LeaseEpoch: lock.LeaseEpoch, LeaseDurationSeconds: lock.LeaseDurationSeconds}}}
	} else {
		object = &ptahv1.PtahMigration{ObjectMeta: metadata, Status: ptahv1.PtahMigrationStatus{ObservedGeneration: 2, ActiveOperation: &ptahv1.MigrationOperationStatus{ID: lock.OperationID, Type: ptahv1.MigrationOperationApply, StartedAt: at, JobName: "apply", JobUID: "apply-uid", CoordinationDigest: lock.CoordinationDigest, LeaseEpoch: lock.LeaseEpoch, LeaseDurationSeconds: lock.LeaseDurationSeconds}}}
	}
	return object, alLockReading(object)
}

func TestAlLockOwedRequiresTheCompletedOriginalApply(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			object, before := lockAlertFixture(family)
			if !before.claimed() {
				t.Fatal("complete original Apply refused")
			}
			at := metav1.NewTime(before.claim.started.Add(20 * time.Second))
			switch v := object.(type) {
			case *ptahv1.PtahSchema:
				v.Status.ActiveOperation = nil
				v.Status.PendingLockRelease = before.lock.DeepCopy()
				v.Status.Applied = &ptahv1.AppliedStatus{CompletedAt: at}
			case *ptahv1.PtahMigration:
				v.Status.ActiveOperation = nil
				v.Status.PendingLockRelease = before.lock.DeepCopy()
				v.Status.LastRun = &ptahv1.MigrationRunStatus{Outcome: ptahv1.MigrationRunOutcomeApplied, JobUID: before.claim.jobUID, FinishedAt: &at}
			}
			owed := alLockReading(object)
			if !owed.owes(before) {
				t.Fatal("exact completed Apply release refused")
			}
			for name, mutate := range map[string]func(*alLockState){
				"missing obligation":    func(r *alLockState) { r.owed = false },
				"changed epoch":         func(r *alLockState) { r.lock.LeaseEpoch = "other" },
				"changed operation":     func(r *alLockState) { r.lock.OperationID = "other" },
				"changed realm":         func(r *alLockState) { r.lock.CoordinationDigest = "other" },
				"changed duration":      func(r *alLockState) { r.lock.LeaseDurationSeconds++ },
				"active work":           func(r *alLockState) { r.claim.id = "new-operation" },
				"unfinished":            func(r *alLockState) { r.completed = time.Time{} },
				"old completion":        func(r *alLockState) { r.completed = before.claim.started.Add(-time.Second) },
				"unknown Apply":         func(r *alLockState) { r.unresolved = true },
				"replacement":           func(r *alLockState) { r.claim.uid = "replacement" },
				"unobserved generation": func(r *alLockState) { r.observed-- },
			} {
				bad := owed
				mutate(&bad)
				if bad.owes(before) {
					t.Errorf("%s accepted", name)
				}
			}
			if family == "migration" {
				owed.finishedJob = "other"
				if owed.owes(before) {
					t.Fatal("other migration execution accepted")
				}
			}
		})
	}
}

func lockLeaseFixture() (*coordinationv1.Lease, ptahv1.TargetLockReleaseStatus) {
	_, r := lockAlertFixture("schema")
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "operator", Name: "realm", UID: "lease-uid", Annotations: map[string]string{annotationLeaseEpoch: r.lock.LeaseEpoch}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("original-holder"), LeaseDurationSeconds: ptr.To(r.lock.LeaseDurationSeconds), RenewTime: ptr.To(metav1.NewMicroTime(r.claim.started))}}
	return lease, r.lock
}

func TestAlLockReleaseAndHandoffRequireOneNativeLeaseHistory(t *testing.T) {
	t.Parallel()
	original, binding := lockLeaseFixture()
	if !alLockHeld(original, original, binding) {
		t.Fatal("original held Lease refused")
	}
	completed := original.Spec.RenewTime.Time.Add(20 * time.Second)
	released := original.DeepCopy()
	released.Spec.HolderIdentity = ptr.To("")
	released.Spec.RenewTime = ptr.To(metav1.NewMicroTime(completed.Add(100 * time.Second)))
	at, ok := alLockReleased(released, original, completed)
	if !ok {
		t.Fatal("native release refused")
	}
	next := binding
	next.LeaseEpoch = "v1-" + strings.Repeat("d", 32)
	acquired := released.DeepCopy()
	acquired.Annotations[annotationLeaseEpoch] = next.LeaseEpoch
	acquired.Spec.HolderIdentity = ptr.To("sibling-holder")
	acquired.Spec.RenewTime = ptr.To(metav1.NewMicroTime(at.Add(time.Microsecond)))
	if !alLockHandoff([]*coordinationv1.Lease{original, released, acquired}, original, next, at) {
		t.Fatal("ordered same-Lease handoff refused")
	}
	if alLockHandoff([]*coordinationv1.Lease{original, acquired, released}, original, next, at) || alLockHandoff([]*coordinationv1.Lease{original, acquired}, original, next, at) {
		t.Fatal("acquisition without prior release accepted")
	}
	for name, mutate := range map[string]func(*coordinationv1.Lease){
		"replaced Lease":       func(l *coordinationv1.Lease) { l.UID = "replacement" },
		"other namespace":      func(l *coordinationv1.Lease) { l.Namespace = "other" },
		"other epoch":          func(l *coordinationv1.Lease) { l.Annotations[annotationLeaseEpoch] = "other" },
		"holder still present": func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = ptr.To("original-holder") },
		"missing holder":       func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = nil },
		"missing time":         func(l *coordinationv1.Lease) { l.Spec.RenewTime = nil },
		"old time":             func(l *coordinationv1.Lease) { l.Spec.RenewTime = ptr.To(metav1.NewMicroTime(completed)) },
	} {
		bad := released.DeepCopy()
		mutate(bad)
		if _, ok := alLockReleased(bad, original, completed); ok {
			t.Errorf("%s accepted", name)
		}
	}
	policy, _ := alLockReleasePolicy(original, "system:serviceaccount:operator:manager")
	for _, identity := range []string{string(original.UID), original.Name, original.Namespace, "system:serviceaccount:operator:manager"} {
		if !strings.Contains(policy.Spec.MatchConditions[0].Expression, identity) {
			t.Fatalf("fault omitted %s", identity)
		}
	}
}

func TestAlLockNotificationKeepsTheFrozenNativeBounds(t *testing.T) {
	t.Parallel()
	completed := time.Unix(1800000000, 0).UTC()
	firing := alDelivery{StartsAt: completed.Add(alLockPendingFor), ReceivedAt: completed.Add(alLockPendingFor + alDetectionSlack)}
	if !alLockDelivered(firing, completed) {
		t.Fatal("exact firing deadline refused")
	}
	late := firing
	late.ReceivedAt = late.ReceivedAt.Add(time.Nanosecond)
	if alLockDelivered(late, completed) {
		t.Fatal("late firing accepted")
	}
	early := firing
	early.StartsAt = early.StartsAt.Add(-time.Nanosecond)
	if alLockDelivered(early, completed) {
		t.Fatal("early firing hidden by late delivery")
	}
	released := completed.Add(120 * time.Second)
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: released.Add(time.Second), ReceivedAt: released.Add(alDetectionSlack)}
	if !alOverdueCleared(firing, resolved, released) {
		t.Fatal("exact release resolution refused")
	}
	resolved.ReceivedAt = resolved.ReceivedAt.Add(time.Nanosecond)
	if alOverdueCleared(firing, resolved, released) {
		t.Fatal("late release resolution accepted")
	}
}

func TestAlLockWorkloadAndTransportRejectReplacementAndReplay(t *testing.T) {
	t.Parallel()
	_, r := lockAlertFixture("schema")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: r.claim.namespace, Name: r.claim.jobName, UID: r.claim.jobUID, Labels: map[string]string{labelOperation: "apply"}, Annotations: map[string]string{"operator.ptah.run/operation-id": r.claim.id}, OwnerReferences: []metav1.OwnerReference{{APIVersion: ptahv1.GroupVersion.String(), Kind: "PtahSchema", Name: r.claim.name, UID: r.claim.uid, Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: "executor", UID: "pod-uid", Labels: job.Labels, Annotations: job.Annotations, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ptah"}}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(r.claim.started), FinishedAt: metav1.NewTime(r.claim.started.Add(10 * time.Second))}}}}}}
	if !alLockWorkload(job, pod, r.claim) {
		t.Fatal("exact workload refused")
	}
	if _, err := alLockTransportFinished(pod); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"restart":                func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"missing outcome":        func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"failed child transport": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1 },
		"missing start":          func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.Time{} },
		"missing finish":         func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
	} {
		bad := pod.DeepCopy()
		mutate(bad)
		if _, err := alLockTransportFinished(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	bad := pod.DeepCopy()
	bad.OwnerReferences[0].UID = "other-job"
	if alLockWorkload(job, bad, r.claim) {
		t.Fatal("unrelated executor accepted")
	}
}

func TestAlLockSiblingAcceptsConvergenceWithoutAPlan(t *testing.T) {
	t.Parallel()
	v := &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{UID: "sibling", Generation: 2}}
	v.Spec.Policy.Apply = ptahv1.ApplyPolicyNever
	v.Status.ObservedGeneration = 2
	v.Status.Conditions = []metav1.Condition{
		{Type: ptahv1.ConditionInSync, Status: metav1.ConditionTrue, ObservedGeneration: 2},
		{Type: ptahv1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 2},
	}
	if !alLockSiblingReady(v) {
		t.Fatal("successful no-change Plan without a stored plan refused")
	}
	for name, mutate := range map[string]func(*ptahv1.PtahSchema){
		"unobserved generation": func(v *ptahv1.PtahSchema) { v.Status.ObservedGeneration-- },
		"stale convergence":     func(v *ptahv1.PtahSchema) { v.Status.Conditions[0].ObservedGeneration-- },
		"not ready":             func(v *ptahv1.PtahSchema) { v.Status.Conditions[1].Status = metav1.ConditionFalse },
		"absent conditions":     func(v *ptahv1.PtahSchema) { v.Status.Conditions = nil },
		"active operation":      func(v *ptahv1.PtahSchema) { v.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{} },
		"owed release":          func(v *ptahv1.PtahSchema) { v.Status.PendingLockRelease = &ptahv1.TargetLockReleaseStatus{} },
		"unresolved Apply":      func(v *ptahv1.PtahSchema) { v.Status.PendingObservation = &ptahv1.PendingObservationStatus{} },
		"Apply occurred":        func(v *ptahv1.PtahSchema) { v.Status.Applied = &ptahv1.AppliedStatus{} },
		"suspended":             func(v *ptahv1.PtahSchema) { v.Spec.Suspend = true },
	} {
		t.Run(name, func(t *testing.T) {
			bad := v.DeepCopy()
			mutate(bad)
			if alLockSiblingReady(bad) {
				t.Fatal("incomplete sibling accepted")
			}
		})
	}
}

func TestAlLockSiblingRequiresACompletePublishedPlan(t *testing.T) {
	t.Parallel()
	v := &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{UID: "sibling", Generation: 2}}
	v.Spec.Policy.Apply = ptahv1.ApplyPolicyNever
	v.Status.ObservedGeneration = 2
	v.Status.Plan = &ptahv1.CurrentPlanStatus{Name: "plan", UID: "plan-uid"}
	v.Status.Conditions = []metav1.Condition{{Type: ptahv1.ConditionPlanReady, Status: metav1.ConditionTrue, ObservedGeneration: 2}}
	if !alLockSiblingReady(v) {
		t.Fatal("published current plan refused")
	}
	v.Status.Conditions[0].ObservedGeneration--
	if alLockSiblingReady(v) {
		t.Fatal("stale plan condition accepted")
	}
	v.Status.Conditions[0].ObservedGeneration++
	v.Status.Plan.UID = ""
	if alLockSiblingReady(v) {
		t.Fatal("unbound plan accepted")
	}
}
