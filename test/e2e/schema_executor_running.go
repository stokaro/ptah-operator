package e2e

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
)

// Database reads return one PID and a host address, without an inet netmask.
// The process must still belong to the exact live workload under test.
func executorBackendMatchesPod(backend string, pod *corev1.Pod, uid types.UID) bool {
	if pod == nil || uid == "" || pod.UID != uid || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	pid, host, ok := strings.Cut(backend, "/")
	if !ok || !decimalCount.MatchString(pid) {
		return false
	}
	process, err := strconv.ParseUint(pid, 10, 64)
	if err != nil || process == 0 {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	podAddress, err := netip.ParseAddr(pod.Status.PodIP)
	return err == nil && address == podAddress
}

// runningSchemaClaim is the dispatched Apply the rollout meets: every field
// the replacement manager reads it by, written before the rollout, with the
// one Pod that runs it.
func runningSchemaClaim(before *ptahv1alpha1.PtahSchema, podUID types.UID) error {
	if before == nil || before.UID == "" || podUID == "" {
		return errors.New("the rollout has no original schema or Apply Pod")
	}
	old, active := before.Status.ExecutionBinding, before.Status.ActiveOperation
	if old == nil || !executionEpoch.MatchString(old.Epoch) || before.Status.Plan == nil || before.Status.Plan.ExecutionBindingID != old.Epoch ||
		active == nil || active.Type != ptahv1alpha1.OperationApply || !active.DispatchStarted || active.JobUID == "" ||
		active.ID == "" || active.JobName == "" || active.AdmissionSnapshot == nil || active.CoordinationDigest == "" ||
		active.LeaseDurationSeconds < 5 || active.LeaseEpoch == "" || active.ExecutionBindingID != old.Epoch ||
		active.Target == nil || active.Source == nil || active.ExecutionNotAfter == nil || active.ExecutionNotAfter.IsZero() ||
		active.TerminationGracePeriodSeconds < 1 {
		return errors.New("the rollout has no original dispatched Apply under the recorded binding")
	}
	return nil
}

// sameRunningSchema is a reading of the schema the Apply was dispatched for,
// with no edit since.
func sameRunningSchema(before, current *ptahv1alpha1.PtahSchema) bool {
	return before != nil && current != nil && before.UID != "" && current.UID == before.UID &&
		current.Name == before.Name && current.Namespace == before.Namespace && current.Generation == before.Generation &&
		equality.Semantic.DeepEqual(current.Spec, before.Spec)
}

// schemaClaimHeld is a reading in which the original claim still stands as the
// dispatching manager wrote it: the claim, the binding and the plan are
// unchanged, and nothing records the Apply's result yet.
func schemaClaimHeld(before, current *ptahv1alpha1.PtahSchema) error {
	if !sameRunningSchema(before, current) ||
		!equality.Semantic.DeepEqual(current.Status.ActiveOperation, before.Status.ActiveOperation) ||
		!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, before.Status.ExecutionBinding) ||
		!equality.Semantic.DeepEqual(current.Status.Plan, before.Status.Plan) ||
		!equality.Semantic.DeepEqual(current.Status.Applied, before.Status.Applied) ||
		current.Status.PendingObservation != nil || current.Status.PendingBindingRetirement != nil ||
		current.Status.PendingLockRelease != nil || !slices.Contains(current.Finalizers, lifecycleGuardActiveOperationFinalizer) {
		return errors.New("the schema released, rewrote or settled its original claim while the executor ran")
	}
	return nil
}

// schemaExecutorWaiting is the replacement manager holding the binding back for
// the running Apply. The claim stands as before, and Applying carries the words
// only the replacement writes, so the reading cannot be one the dispatching
// manager left behind.
func schemaExecutorWaiting(before, current *ptahv1alpha1.PtahSchema, podUID types.UID) error {
	if err := runningSchemaClaim(before, podUID); err != nil {
		return err
	}
	if err := schemaClaimHeld(before, current); err != nil {
		return err
	}
	applying := meta.FindStatusCondition(current.Status.Conditions, ptahv1alpha1.ConditionApplying)
	if applying == nil || applying.Status != metav1.ConditionTrue || applying.Reason != string(ptahv1alpha1.ReasonApprovedPlan) ||
		!strings.HasSuffix(applying.Message, controller.RotationWaitWords) {
		return errors.New("applying does not say the binding waits for the running Apply")
	}
	return nil
}

// schemaExecutorWaitWindow holds every watched reading from before to the
// waiting one to the original claim. A later correct reading cannot hide an
// earlier one that let the claim go.
func schemaExecutorWaitWindow(events []watchEvent[*ptahv1alpha1.PtahSchema], before *ptahv1alpha1.PtahSchema,
	podUID types.UID, endVersion string,
) error {
	if before == nil || before.ResourceVersion == "" || endVersion == "" || endVersion == before.ResourceVersion {
		return errors.New("executor window has no exact API boundaries")
	}
	started := false
	for _, event := range events {
		current := event.Object
		if current == nil || current.UID != before.UID {
			continue
		}
		if current.ResourceVersion == before.ResourceVersion {
			started = true
		}
		if !started {
			continue
		}
		if event.Type == watch.Deleted {
			return errors.New("executor window saw the schema deleted")
		}
		if err := schemaClaimHeld(before, current); err != nil {
			return err
		}
		if current.ResourceVersion == endVersion {
			return schemaExecutorWaiting(before, current, podUID)
		}
	}
	return errors.New("executor window did not reach both watch barriers")
}

// schemaApplyRead is the proof the replacement owes once it has read the
// original Apply's result: what the run reported, for the Apply, its Job and
// its one Pod, under the plan and the binding the Apply ran under.
func schemaApplyRead(before *ptahv1alpha1.PtahSchema, pending *ptahv1alpha1.PendingObservationStatus, podUID types.UID) error {
	active := before.Status.ActiveOperation
	if pending == nil || pending.Outcome != ptahv1alpha1.PendingObservationApplySucceeded || pending.ApplyFailure != nil ||
		pending.ApplyOperationID != active.ID || pending.ApplyJobName != active.JobName || pending.ApplyJobUID != active.JobUID ||
		pending.ApplyGeneration != before.Generation || pending.ApplyPodCount != 1 || !slices.Equal(pending.ApplyPodUIDs, []types.UID{podUID}) ||
		!equality.Semantic.DeepEqual(pending.Plan, *before.Status.Plan) || pending.DispatchedBy == nil ||
		!equality.Semantic.DeepEqual(pending.AdmissionSnapshot, active.AdmissionSnapshot) ||
		!equality.Semantic.DeepEqual(pending.Target, *active.Target) || !equality.Semantic.DeepEqual(pending.Source, *active.Source) ||
		pending.CoordinationDigest != active.CoordinationDigest || pending.LeaseEpoch != active.LeaseEpoch ||
		pending.LeaseDurationSeconds != active.LeaseDurationSeconds {
		return errors.New("the proof the Apply owes does not record what the original Apply reported under the binding it ran under")
	}
	return nil
}

// schemaAppliedByOriginal is the record the proof writes for the original
// Apply: its plan, under the binding it ran under, never the one that replaced
// it.
func schemaAppliedByOriginal(before *ptahv1alpha1.PtahSchema, applied *ptahv1alpha1.AppliedStatus) error {
	plan := before.Status.Plan
	if applied == nil || plan == nil || applied.PlanRef.Name != plan.Name || applied.PlanRef.UID != plan.UID ||
		applied.PlanFingerprint != plan.Fingerprint || applied.ExecutionBindingID != plan.ExecutionBindingID ||
		applied.ExecutionBindingID != before.Status.ExecutionBinding.Epoch || applied.ExecutorImage != plan.ExecutorImage ||
		applied.DispatchedBy == nil || applied.CompletedAt.IsZero() {
		return errors.New("the applied record does not name the original Apply's plan under the binding it ran under")
	}
	return nil
}

// schemaExecutorReadWindow holds every watched reading from before to the
// settled one to the order the rollout owes: the claim stands until the
// Apply's result is read, the write that reads it keeps the binding that
// authorized it, nothing is claimed under that binding afterwards, the binding
// moves once and only after the result is read, and the proof credits the
// original Apply's plan. No reading records the Apply's outcome as unknown.
func schemaExecutorReadWindow(events []watchEvent[*ptahv1alpha1.PtahSchema], before *ptahv1alpha1.PtahSchema,
	podUID types.UID, replacement, endVersion string,
) error {
	if runningSchemaClaim(before, podUID) != nil || before.ResourceVersion == "" || endVersion == "" || endVersion == before.ResourceVersion {
		return errors.New("executor window has no exact API boundaries")
	}
	old, active := before.Status.ExecutionBinding, before.Status.ActiveOperation
	started, read, rotated := false, false, ""
	var applied *ptahv1alpha1.AppliedStatus
	for _, event := range events {
		current := event.Object
		if current == nil || current.UID != before.UID {
			continue
		}
		if current.ResourceVersion == before.ResourceVersion {
			started = true
		}
		if !started {
			continue
		}
		if event.Type == watch.Deleted || !sameRunningSchema(before, current) || current.Status.ExecutionBinding == nil {
			return errors.New("executor window lost its schema or binding")
		}
		binding, pending := current.Status.ExecutionBinding, current.Status.PendingObservation
		switch {
		case !read && pending == nil:
			if err := schemaClaimHeld(before, current); err != nil {
				return err
			}
		case !read:
			if err := schemaApplyRead(before, pending, podUID); err != nil {
				return err
			}
			if !equality.Semantic.DeepEqual(binding, old) || current.Status.ActiveOperation != nil {
				return errors.New("executor window moved the binding or kept a claim in the write that read the Apply")
			}
			read = true
		case pending != nil:
			if applied != nil || pending.Outcome != ptahv1alpha1.PendingObservationApplySucceeded ||
				pending.ApplyOperationID != active.ID || pending.ApplyJobUID != active.JobUID ||
				!equality.Semantic.DeepEqual(pending.Plan, *before.Status.Plan) ||
				!equality.Semantic.DeepEqual(current.Status.Applied, before.Status.Applied) {
				return errors.New("executor window rewrote what the original Apply reported, or credited it before its proof")
			}
		default:
			if err := schemaAppliedByOriginal(before, current.Status.Applied); err != nil {
				return err
			}
			if applied != nil && !equality.Semantic.DeepEqual(applied, current.Status.Applied) {
				return errors.New("executor window rewrote the record of the original Apply")
			}
			applied = current.Status.Applied
		}
		if read {
			if binding.Epoch == old.Epoch {
				if rotated != "" || !equality.Semantic.DeepEqual(binding, old) {
					return errors.New("executor window moved the binding back")
				}
				if current.Status.ActiveOperation != nil {
					return errors.New("executor window claimed new work under the binding it retires")
				}
			} else {
				if binding.ExecutorImage != replacement || (rotated != "" && binding.Epoch != rotated) {
					return errors.New("executor window moved the binding to something other than the replacement, or twice")
				}
				rotated = binding.Epoch
				if claim := current.Status.ActiveOperation; claim != nil && claim.ExecutionBindingID != rotated {
					return errors.New("executor window claimed work under another binding than the current one")
				}
			}
		}
		if current.ResourceVersion == endVersion {
			if !read || rotated == "" || applied == nil {
				return errors.New("executor window never read the Apply, moved the binding after it and proved it")
			}
			return nil
		}
	}
	return errors.New("executor window did not reach both watch barriers")
}

func schemaExecutorNoReplay(jobs []watchEvent[*batchv1.Job], pods []watchEvent[*corev1.Pod], name, jobUID, podUID string) bool {
	jobEvents := slices.DeleteFunc(slices.Clone(jobs), func(e watchEvent[*batchv1.Job]) bool { return e.Object == nil || e.Object.Labels[labelSchema] != name })
	podEvents := slices.DeleteFunc(slices.Clone(pods), func(e watchEvent[*corev1.Pod]) bool { return e.Object == nil || e.Object.Labels[labelSchema] != name })
	return name != "" && jobUID != "" && podUID != "" && slices.Equal(addedUIDs(jobEvents, name, "apply"), []string{jobUID}) &&
		slices.Equal(addedUIDs(podEvents, name, "apply"), []string{podUID}) &&
		!operationJobsOverlap(jobEvents) && !operationPodsOverlap(podEvents)
}

// The original Apply's result first owes its proof: an Observe and a Plan
// with the evidence the Apply reported still pending. Once that proof credits
// the original plan, the new binding refreshes the source and verifies
// convergence itself. Both stages run under the new binding, and neither can
// start before the original Apply's Job finished. Distinguish the two jobs by
// the persisted claims, not by UID sort order.
func schemaExecutorRecoveryPair(events []watchEvent[*ptahv1alpha1.PtahSchema], before *ptahv1alpha1.PtahSchema,
	jobs []*batchv1.Job, replacement string, applyFinished time.Time,
) error {
	if before == nil || before.Status.ActiveOperation == nil || before.Status.Plan == nil || len(jobs) != 2 ||
		jobs[0] == nil || jobs[1] == nil || jobs[0].UID == "" || jobs[0].UID == jobs[1].UID || jobs[0].Name == jobs[1].Name || applyFinished.IsZero() {
		return errors.New("recovery needs distinct proof and current-source jobs")
	}
	stages := map[bool]bool{}
	newEpoch := ""
	for _, job := range jobs {
		matched := false
		if job.UID == "" {
			return errors.New("recovery job has no UID")
		}
		for _, event := range events {
			current := event.Object
			if current == nil || !sameRunningSchema(before, current) {
				continue
			}
			active := current.Status.ActiveOperation
			if active == nil || active.JobUID != job.UID || active.JobName != job.Name || active.ID != job.Annotations[annotationOperationID] {
				continue
			}
			if active.ID == "" || current.Status.ExecutionBinding == nil ||
				before.Status.ExecutionBinding == nil || current.Status.ExecutionBinding.Epoch == before.Status.ExecutionBinding.Epoch ||
				current.Status.ExecutionBinding.ExecutorImage != replacement || active.ExecutionBindingID != current.Status.ExecutionBinding.Epoch ||
				job.Annotations[annotationBindingID] != active.ExecutionBindingID || !jobUsesExecutor(job, replacement) ||
				job.Namespace != before.Namespace || job.Labels[labelSchema] != before.Name ||
				strings.ToLower(string(active.Type)) != job.Labels[labelOperation] ||
				(active.Type != ptahv1alpha1.OperationObserve && active.Type != ptahv1alpha1.OperationPlan) ||
				active.StartedAt.Time.Before(applyFinished) || job.CreationTimestamp.Time.Before(applyFinished) {
				return errors.New("recovery job is not bound to a claim of the new binding made after the original Apply finished")
			}
			if newEpoch != "" && newEpoch != active.ExecutionBindingID {
				return errors.New("recovery changed execution epoch between stages")
			}
			newEpoch = active.ExecutionBindingID
			pending := current.Status.PendingObservation
			proof := pending != nil
			if proof {
				if pending.Outcome != ptahv1alpha1.PendingObservationApplySucceeded || pending.ApplyJobUID != before.Status.ActiveOperation.JobUID ||
					pending.ApplyOperationID != before.Status.ActiveOperation.ID || !equality.Semantic.DeepEqual(pending.Plan, *before.Status.Plan) ||
					!equality.Semantic.DeepEqual(current.Status.Applied, before.Status.Applied) {
					return errors.New("recovery proof lost what the original Apply reported, or credited it before the proof")
				}
			} else if !current.Status.Source.Verified {
				return errors.New("fresh reconciliation did not verify its source")
			} else if err := schemaAppliedByOriginal(before, current.Status.Applied); err != nil {
				return err
			}
			if stages[proof] {
				return errors.New("recovery repeated one stage and omitted the other")
			}
			stages[proof], matched = true, true
			break
		}
		if !matched {
			return errors.New("recovery job has no exact persisted operation claim")
		}
	}
	if !stages[true] || !stages[false] {
		return errors.New("recovery missed the original Apply's proof or the fresh source reconciliation")
	}
	return nil
}
