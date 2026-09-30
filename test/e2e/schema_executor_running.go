package e2e

import (
	"errors"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The old executable may still write. A new epoch therefore owes the entire
// original Apply, its immutable horizon and its uninterrupted realm claim.
func schemaExecutorRetirement(before, current *ptahv1alpha1.PtahSchema, podUID types.UID, replacement string) error {
	if before == nil || current == nil || before.UID == "" || current.UID != before.UID ||
		current.Name != before.Name || current.Namespace != before.Namespace || current.Generation != before.Generation ||
		!equality.Semantic.DeepEqual(current.Spec, before.Spec) || podUID == "" {
		return errors.New("executor retirement changed or lost the schema identity")
	}
	old, next, active := before.Status.ExecutionBinding, current.Status.ExecutionBinding, before.Status.ActiveOperation
	if old == nil || next == nil || !executionEpoch.MatchString(old.Epoch) || old.ExecutorImage == replacement || next.ExecutorImage != replacement ||
		!executionEpoch.MatchString(next.Epoch) || next.Epoch == old.Epoch || old.PtahVersion != next.PtahVersion ||
		old.RunnerProtocolVersion != next.RunnerProtocolVersion || old.ControllerStateVersion != next.ControllerStateVersion ||
		active == nil || active.Type != ptahv1alpha1.OperationApply || !active.DispatchStarted || active.JobUID == "" ||
		active.ID == "" || active.JobName == "" || active.AdmissionSnapshot == nil || active.CoordinationDigest == "" || active.LeaseDurationSeconds < 5 || active.ExecutionBindingID != old.Epoch || before.Status.Plan == nil || active.Target == nil || active.Source == nil ||
		active.ExecutionNotAfter == nil || active.ExecutionNotAfter.IsZero() || active.TerminationGracePeriodSeconds < 1 {
		return errors.New("executor retirement has no original dispatched Apply under a different epoch")
	}
	pending := current.Status.PendingObservation
	if pending == nil || pending.Outcome != ptahv1alpha1.PendingObservationOutcomeUnknown || current.Status.ActiveOperation != nil ||
		current.Status.PendingLockRelease != nil || current.Status.Applied != nil || current.Status.Plan != nil ||
		!noneConverged(current.Status.Conditions) || !slices.Contains(current.Finalizers, lifecycleGuardActiveOperationFinalizer) ||
		pending.ApplyOperationID != active.ID || pending.ApplyJobName != active.JobName || pending.ApplyJobUID != active.JobUID ||
		pending.ApplyGeneration != before.Generation || pending.ApplyPodCount != 1 || !slices.Equal(pending.ApplyPodUIDs, []types.UID{podUID}) ||
		!equality.Semantic.DeepEqual(pending.Plan, *before.Status.Plan) || !equality.Semantic.DeepEqual(pending.AdmissionSnapshot, active.AdmissionSnapshot) ||
		!equality.Semantic.DeepEqual(pending.Target, *active.Target) || !equality.Semantic.DeepEqual(pending.Source, *active.Source) ||
		!equality.Semantic.DeepEqual(pending.Dev, active.ObservationDev) ||
		!slices.Equal(pending.Exclude, active.ObservationExclude) || !slices.Equal(pending.ProtectedTables, active.ObservationProtectedTables) ||
		pending.DriftSeverity != active.ObservationSeverity || pending.ConnectTimeout != active.ObservationConnectTimeout ||
		pending.LockTimeout != active.ObservationLockTimeout || pending.PlanRequired ||
		pending.CoordinationDigest != active.CoordinationDigest || pending.LeaseEpoch == "" || pending.LeaseEpoch != active.LeaseEpoch ||
		pending.LeaseDurationSeconds != active.LeaseDurationSeconds || pending.ObserveAfter == nil ||
		!pending.ObserveAfter.Time.Equal(active.ExecutionNotAfter.Add(time.Duration(active.TerminationGracePeriodSeconds)*time.Second)) {
		return errors.New("executor retirement lost or released immutable evidence while the original Apply can write")
	}
	return nil
}

// Bound the complete held window by resourceVersions delivered by the watch.
// A later correct status cannot hide an earlier loss of the running claim.
func schemaExecutorHeldWindow(events []watchEvent[*ptahv1alpha1.PtahSchema], before *ptahv1alpha1.PtahSchema,
	podUID types.UID, replacement, endVersion string,
) error {
	if before == nil || before.ResourceVersion == "" || endVersion == "" || endVersion == before.ResourceVersion ||
		before.Status.ExecutionBinding == nil || before.Status.ActiveOperation == nil {
		return errors.New("executor window has no exact API boundaries")
	}
	started, rotated := false, false
	newEpoch := ""
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
		if event.Type == watch.Deleted || current.Status.ExecutionBinding == nil || current.Generation != before.Generation || !equality.Semantic.DeepEqual(current.Spec, before.Spec) {
			return errors.New("executor window lost its resource or binding")
		}
		if current.Status.ExecutionBinding.Epoch == before.Status.ExecutionBinding.Epoch {
			if rotated || !equality.Semantic.DeepEqual(current.Status.ActiveOperation, before.Status.ActiveOperation) ||
				!equality.Semantic.DeepEqual(current.Status.Plan, before.Status.Plan) || current.Status.PendingObservation != nil || current.Status.Applied != nil ||
				!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, before.Status.ExecutionBinding) ||
				!slices.Contains(current.Finalizers, lifecycleGuardActiveOperationFinalizer) {
				return errors.New("executor window discarded or rewrote the original running claim")
			}
		} else {
			if err := schemaExecutorRetirement(before, current, podUID, replacement); err != nil {
				return err
			}
			if rotated && current.Status.ExecutionBinding.Epoch != newEpoch {
				return errors.New("executor window rotated more than once")
			}
			rotated, newEpoch = true, current.Status.ExecutionBinding.Epoch
		}
		if current.ResourceVersion == endVersion {
			if !rotated {
				return errors.New("executor window never observed the new epoch")
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
