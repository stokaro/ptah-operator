package e2e

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The database is an unmanaged sentinel in the same Pod collection. Managed
// Apply Pods forbid metadata edits, including a test's watch annotation.
func migrationExecutorPodBarrierSource(pods []corev1.Pod, namespace, service string) (*corev1.Pod, error) {
	if len(pods) != 1 || namespace == "" || service == "" {
		return nil, errors.New("the Pod watch needs exactly one database sentinel")
	}
	pod := &pods[0]
	if pod.Name == "" || pod.UID == "" || pod.Namespace != namespace || pod.DeletionTimestamp != nil ||
		pod.Status.Phase != corev1.PodRunning || pod.Labels["app.kubernetes.io/name"] != service ||
		pod.Labels["app.kubernetes.io/managed-by"] == "ptah-operator" ||
		pod.Labels[labelMigration] != "" || pod.Labels[labelSchema] != "" {
		return nil, errors.New("the Pod watch sentinel is not the exact running unmanaged database")
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			return nil, errors.New("the Pod watch sentinel belongs to a Job")
		}
	}
	return pod.DeepCopy(), nil
}

func migrationExecutorPodWatchReached(events []watchEvent[*corev1.Pod], pod *corev1.Pod) bool {
	return pod != nil && pod.UID != "" && pod.ResourceVersion != "" && slices.ContainsFunc(events, func(event watchEvent[*corev1.Pod]) bool {
		return (event.Type == watch.Added || event.Type == watch.Modified) && event.Object != nil &&
			event.Object.UID == pod.UID && event.Object.ResourceVersion == pod.ResourceVersion &&
			event.Object.Name == pod.Name && event.Object.Namespace == pod.Namespace
	})
}

func sameRunningMigration(before, current *ptahv1alpha1.PtahMigration) bool {
	return before != nil && current != nil && before.UID != "" && before.UID == current.UID &&
		before.Name == current.Name && before.Namespace == current.Namespace && before.Generation == current.Generation &&
		equality.Semantic.DeepEqual(before.Spec, current.Spec)
}

// The new manager records Unknown under the old binding. The old executor
// still owns the complete claim and realm until its workload stops.
func migrationExecutorHeld(before, current *ptahv1alpha1.PtahMigration) error {
	if !sameRunningMigration(before, current) || before.Status.ExecutionBinding == nil || before.Status.History == nil ||
		before.Status.Plan == nil || before.Status.ActiveOperation == nil {
		return errors.New("running migration has no exact original identity, plan, history or binding")
	}
	active := before.Status.ActiveOperation
	if active.Type != ptahv1alpha1.MigrationOperationApply || !active.DispatchStarted || active.ID == "" ||
		active.JobName == "" || active.JobUID == "" || active.AdmissionSnapshot == nil || active.ExecutionNotAfter == nil ||
		!active.ExecutionNotAfter.After(active.StartedAt.Time) ||
		active.LeaseEpoch == "" || active.LeaseDurationSeconds < 5 || active.Source == nil || active.Target == nil ||
		!equality.Semantic.DeepEqual(active.PlanRef, before.Status.Plan) ||
		!equality.Semantic.DeepEqual(current.Status.ActiveOperation, active) ||
		!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, before.Status.ExecutionBinding) ||
		!slices.Contains(current.Finalizers, migrationFinalizer) || current.Status.Plan != nil || current.Status.PendingLockRelease != nil ||
		!conditionIs(current.Status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "ApplyOutcomeUnknown") ||
		conditionStatus(current.Status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue) {
		return errors.New("running unknown migration lost or changed its protected original claim")
	}
	last, unresolved := current.Status.LastRun, current.Status.UnresolvedRun
	if last == nil || last.Outcome != ptahv1alpha1.MigrationRunOutcomeUnknown || len(last.AppliedVersions) != 0 || last.JobName != active.JobName || last.JobUID != active.JobUID ||
		!last.StartedAt.Equal(&active.StartedAt) || last.FinishedAt == nil || last.FinishedAt.IsZero() ||
		unresolved == nil || unresolved.Outcome != last.Outcome || unresolved.OperationID != active.ID ||
		unresolved.JobName != active.JobName || unresolved.JobUID != active.JobUID || unresolved.RecordedAt.IsZero() ||
		!equality.Semantic.DeepEqual(unresolved.PlanRef, *active.PlanRef) ||
		unresolved.TargetIdentityDigest != before.Status.History.TargetIdentityDigest || !sha256Pattern.MatchString(unresolved.TargetIdentityDigest) ||
		last.DispatchedBy == nil || !equality.Semantic.DeepEqual(unresolved.DispatchedBy, last.DispatchedBy) {
		return errors.New("running migration's unknown record does not name its original execution")
	}
	var copied ptahv1alpha1.UnresolvedMigrationRunStatus
	if err := json.Unmarshal([]byte(current.Annotations[ptahv1alpha1.UnresolvedRunAnnotation]), &copied); err != nil ||
		!equality.Semantic.DeepEqual(&copied, unresolved) {
		return errors.New("running migration lost or changed the complete metadata copy of its unresolved run")
	}
	return nil
}

func migrationExecutorHeldWindow(events []watchEvent[*ptahv1alpha1.PtahMigration], before *ptahv1alpha1.PtahMigration, endVersion string) error {
	if before == nil || before.ResourceVersion == "" || endVersion == "" || endVersion == before.ResourceVersion {
		return errors.New("running migration window has no exact API boundaries")
	}
	started := false
	var unknown *ptahv1alpha1.PtahMigration
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
		if event.Type == watch.Deleted || !sameRunningMigration(before, current) ||
			!equality.Semantic.DeepEqual(current.Status.ActiveOperation, before.Status.ActiveOperation) ||
			!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, before.Status.ExecutionBinding) ||
			current.Status.PendingLockRelease != nil || !slices.Contains(current.Finalizers, migrationFinalizer) {
			return errors.New("running migration window released or rewrote the live claim")
		}
		if current.Status.UnresolvedRun != nil || unknown != nil {
			if err := migrationExecutorHeld(before, current); err != nil {
				return err
			}
			if unknown != nil && (!equality.Semantic.DeepEqual(unknown.Status.LastRun, current.Status.LastRun) ||
				!equality.Semantic.DeepEqual(unknown.Status.UnresolvedRun, current.Status.UnresolvedRun)) {
				return errors.New("running migration window rewrote terminal evidence")
			}
			unknown = current
		} else if !equality.Semantic.DeepEqual(before.Status.Plan, current.Status.Plan) {
			return errors.New("running migration discarded its plan before recording the unknown result")
		}
		if current.ResourceVersion == endVersion {
			if unknown == nil {
				return errors.New("running migration window never recorded Unknown")
			}
			return nil
		}
	}
	return errors.New("running migration window did not reach both watch barriers")
}

func migrationExecutorHistoryRecovery(before, held, current *ptahv1alpha1.PtahMigration, replacement string) error {
	if migrationExecutorHeld(before, held) != nil || !sameRunningMigration(before, current) {
		return errors.New("migration recovery lacks the original held execution")
	}
	old, next := before.Status.ExecutionBinding, current.Status.ExecutionBinding
	history, resolved := current.Status.History, current.Status.ResolvedRun
	if next == nil || next.ExecutorImage != replacement || replacement == old.ExecutorImage || next.Epoch == old.Epoch ||
		!executionEpoch.MatchString(next.Epoch) || next.PtahVersion != old.PtahVersion ||
		next.ControllerStateVersion != old.ControllerStateVersion || next.RunnerProtocolVersion != old.RunnerProtocolVersion ||
		current.Status.ActiveOperation != nil || current.Status.Plan != nil || current.Status.PendingLockRelease != nil ||
		current.Status.UnresolvedRun != nil || current.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] != "" ||
		!equality.Semantic.DeepEqual(current.Status.LastRun, held.Status.LastRun) ||
		resolved == nil || resolved.OperationID != held.Status.UnresolvedRun.OperationID || resolved.Outcome != ptahv1alpha1.MigrationRunOutcomeUnknown ||
		resolved.Resolution != ptahv1alpha1.MigrationRunResolvedByHistoryRead || resolved.AcknowledgmentRef != nil || resolved.AcknowledgedBy != nil ||
		history == nil || history.CurrentVersion != 3 || history.AppliedCount != 3 || history.PendingCount != 0 || history.Dirty ||
		len(history.ModifiedVersions) != 0 || len(history.OutOfOrderVersions) != 0 ||
		history.TargetIdentityDigest != held.Status.UnresolvedRun.TargetIdentityDigest ||
		!history.ObservedAt.After(held.Status.UnresolvedRun.RecordedAt.Time) || !resolved.ResolvedAt.Equal(&history.ObservedAt) ||
		!conditionIs(current.Status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched") {
		return errors.New("migration recovery replayed, reattributed or failed to freshly account for the original run")
	}
	return nil
}

// Reuse the schema watch's serialization predicates on owned copies; the
// migration label selects the resource before the copies receive its alias.
func migrationExecutorNoReplay(jobs []watchEvent[*batchv1.Job], pods []watchEvent[*corev1.Pod], name, jobUID, podUID string) bool {
	var translatedJobs []watchEvent[*batchv1.Job]
	var translatedPods []watchEvent[*corev1.Pod]
	for _, event := range jobs {
		if event.Object != nil && event.Object.Labels[labelMigration] == name {
			object := event.Object.DeepCopy()
			object.Labels = maps.Clone(object.Labels)
			object.Labels[labelSchema] = name
			translatedJobs = append(translatedJobs, watchEvent[*batchv1.Job]{Type: event.Type, Object: object})
		}
	}
	for _, event := range pods {
		if event.Object != nil && event.Object.Labels[labelMigration] == name {
			object := event.Object.DeepCopy()
			object.Labels = maps.Clone(object.Labels)
			object.Labels[labelSchema] = name
			translatedPods = append(translatedPods, watchEvent[*corev1.Pod]{Type: event.Type, Object: object})
		}
	}
	return schemaExecutorNoReplay(translatedJobs, translatedPods, name, jobUID, podUID)
}
