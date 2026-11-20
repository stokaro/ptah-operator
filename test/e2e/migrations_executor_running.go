package e2e

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// The pinned migrator reserves one pool connection for its advisory lock and
// executes DDL on another. Both sessions must belong to the same live executor;
// requiring the DDL backend itself to own the advisory lock can never match.
func runningMigrationBackendSQL(engine, database string) (string, error) {
	if !mysqlAuditIdentifier.MatchString(database) {
		return "", errors.New("running migration needs a plain database identifier")
	}
	switch engine {
	case "postgresql":
		return fmt.Sprintf(`SELECT DISTINCT ddl.pid::text || '/' || host(ddl.client_addr) || '/' || holder.pid::text || '/' || host(holder.client_addr)
FROM pg_stat_activity ddl
JOIN pg_locks waiting ON waiting.pid=ddl.pid
JOIN pg_stat_activity holder ON holder.datid=ddl.datid AND holder.usesysid=ddl.usesysid AND holder.client_addr=ddl.client_addr
JOIN pg_locks held ON held.pid=holder.pid
WHERE ddl.datname='%s' AND NOT waiting.granted AND waiting.locktype='relation'
AND waiting.relation='e2e_migration_widgets'::regclass AND waiting.mode='AccessExclusiveLock'
AND held.locktype='advisory' AND held.granted AND held.classid=0 AND held.objid=2705505214 AND held.objsubid=1`, database), nil
	case "mysql":
		return fmt.Sprintf(`SELECT CONCAT(ddl.ID, '/', SUBSTRING_INDEX(ddl.HOST, ':', 1), '/', holder.ID, '/', SUBSTRING_INDEX(holder.HOST, ':', 1))
FROM information_schema.processlist ddl
JOIN information_schema.processlist holder ON holder.ID=IS_USED_LOCK('ptah_migrate') AND holder.DB=ddl.DB AND holder.USER=ddl.USER
AND SUBSTRING_INDEX(holder.HOST, ':', 1)=SUBSTRING_INDEX(ddl.HOST, ':', 1)
WHERE ddl.DB='%s' AND ddl.STATE LIKE '%%metadata lock%%'`, database), nil
	default:
		return "", errors.New("running migration needs PostgreSQL or MySQL")
	}
}

func migrationExecutorBackendMatchesPod(backend string, pod *corev1.Pod, uid types.UID) bool {
	parts := strings.Split(backend, "/")
	return len(parts) == 4 && executorBackendMatchesPod(strings.Join(parts[:2], "/"), pod, uid) &&
		executorBackendMatchesPod(strings.Join(parts[2:], "/"), pod, uid)
}

// SQL can reach the table barrier before kubelet publishes Running. Keep the
// first Pod identity while waiting for both observations, and return the exact
// reading that matched rather than reopening the window with another GET.
func waitForMigrationExecutorBackend(ctx context.Context, timeout, interval time.Duration,
	read func() (string, *corev1.Pod, error),
) (string, *corev1.Pod, error) {
	var backend string
	var pod *corev1.Pod
	var uid types.UID
	err := harness.Wait(ctx, "the original migration executor's SQL sessions and running Pod", timeout, interval,
		func(context.Context) (bool, string, error) {
			var err error
			backend, pod, err = read()
			if err != nil {
				return false, "", err
			}
			if backend == "" {
				return false, "no blocked DDL and advisory-lock session pair", nil
			}
			if pod == nil || pod.UID == "" {
				return false, "", errors.New("the migration session pair has no identified Apply Pod")
			}
			observed := fmt.Sprintf("backend=%q Pod=%s UID=%s phase=%s IP=%s", backend, pod.Name, pod.UID, pod.Status.Phase, pod.Status.PodIP)
			if uid == "" {
				uid = pod.UID
			}
			if pod.UID != uid {
				return false, observed, fmt.Errorf("the original Apply Pod %s was replaced: %s", uid, observed)
			}
			if pod.Status.Phase == corev1.PodPending || pod.Status.Phase == "" ||
				(pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP == "") {
				return false, observed, nil
			}
			if !migrationExecutorBackendMatchesPod(backend, pod, uid) {
				return false, observed, fmt.Errorf("the database did not bind both sessions to the exact running migration Pod: %s", observed)
			}
			return true, observed, nil
		})
	return backend, pod, err
}

func runningMigrationApprovalPlan(resource *ptahv1alpha1.PtahMigration, plan *ptahv1alpha1.PtahMigrationPlan,
	reference, digest string, versions []int64,
) error {
	if resource == nil || plan == nil || resource.UID == "" || plan.UID == "" || reference == "" ||
		!sha256Pattern.MatchString(digest) || !strings.HasSuffix(reference, "@"+digest) ||
		resource.Spec.Artifact.OCIRef != reference || resource.Namespace != plan.Namespace ||
		resource.Status.ObservedGeneration != resource.Generation || resource.Status.ActiveOperation != nil ||
		resource.Status.UnresolvedRun != nil || resource.Status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval ||
		resource.Status.Plan == nil || *resource.Status.Plan != (ptahv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}) ||
		plan.Spec.MigrationRef != (ptahv1alpha1.ImmutableObjectReference{Name: resource.Name, UID: resource.UID}) ||
		resource.Status.Artifact == nil || resource.Status.Artifact.Digest != digest || plan.Spec.ArtifactDigest != digest {
		return errors.New("running migration has no current approval gate for its own pinned artifact")
	}
	if len(versions) == 0 || len(versions) != len(plan.Spec.Migrations) {
		return errors.New("running migration has no exact selected sequence")
	}
	for index, version := range versions {
		if version < 1 || plan.Spec.Migrations[index].Version != version || plan.Spec.Migrations[index].Checksum == "" {
			return errors.New("running migration plan did not select its exact version and checksum sequence")
		}
	}
	return nil
}

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

// runningMigrationClaim is the dispatched Apply the rollout meets: every
// field the replacement manager reads it by, written before the rollout.
func runningMigrationClaim(before *ptahv1alpha1.PtahMigration) error {
	if before == nil || before.UID == "" || before.Status.ExecutionBinding == nil || before.Status.History == nil ||
		before.Status.Plan == nil || before.Status.ActiveOperation == nil {
		return errors.New("running migration has no exact original identity, plan, history or binding")
	}
	active := before.Status.ActiveOperation
	if active.Type != ptahv1alpha1.MigrationOperationApply || !active.DispatchStarted || active.ID == "" ||
		active.JobName == "" || active.JobUID == "" || active.AdmissionSnapshot == nil || active.ExecutionNotAfter == nil ||
		!active.ExecutionNotAfter.After(active.StartedAt.Time) || active.ExecutionBindingID != before.Status.ExecutionBinding.Epoch ||
		active.LeaseEpoch == "" || active.LeaseDurationSeconds < 5 || active.Source == nil || active.Target == nil ||
		!equality.Semantic.DeepEqual(active.PlanRef, before.Status.Plan) {
		return errors.New("running migration has no dispatched Apply claim under its recorded binding")
	}
	return nil
}

// migrationClaimHeld is a reading in which the original claim still stands as
// the dispatching manager wrote it: the claim, the binding and the plan are
// unchanged, and nothing is recorded about the run yet.
func migrationClaimHeld(before, current *ptahv1alpha1.PtahMigration) error {
	if err := runningMigrationClaim(before); err != nil {
		return err
	}
	if !sameRunningMigration(before, current) ||
		!equality.Semantic.DeepEqual(current.Status.ActiveOperation, before.Status.ActiveOperation) ||
		!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, before.Status.ExecutionBinding) ||
		!equality.Semantic.DeepEqual(current.Status.Plan, before.Status.Plan) ||
		!equality.Semantic.DeepEqual(current.Status.LastRun, before.Status.LastRun) ||
		current.Status.UnresolvedRun != nil || current.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] != "" ||
		current.Status.PendingLockRelease != nil || !slices.Contains(current.Finalizers, migrationFinalizer) {
		return errors.New("running migration released, rewrote or settled its original claim while the executor ran")
	}
	return nil
}

// migrationExecutorWaiting is the replacement manager holding the binding back
// for the running Apply. The claim stands as before, and Progressing carries,
// after the words of an Apply in flight, the words only the replacement
// writes, so the reading cannot be one the dispatching manager left behind.
func migrationExecutorWaiting(before, current *ptahv1alpha1.PtahMigration) error {
	if err := migrationClaimHeld(before, current); err != nil {
		return err
	}
	progressing := meta.FindStatusCondition(current.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		(progressing.Reason != string(ptahv1alpha1.ReasonApprovedPlan) &&
			progressing.Reason != string(ptahv1alpha1.ReasonOperationInProgress)) ||
		!strings.HasSuffix(progressing.Message, controller.RotationWaitWords) {
		return errors.New("progressing does not say the binding waits for the running Apply")
	}
	return nil
}

// migrationExecutorWaitWindow holds every watched reading from before to the
// waiting one to the original claim. A later correct reading cannot hide an
// earlier one that let the claim go.
func migrationExecutorWaitWindow(events []watchEvent[*ptahv1alpha1.PtahMigration], before *ptahv1alpha1.PtahMigration, endVersion string) error {
	if before == nil || before.ResourceVersion == "" || endVersion == "" || endVersion == before.ResourceVersion {
		return errors.New("running migration window has no exact API boundaries")
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
			return errors.New("running migration window saw the resource deleted")
		}
		if err := migrationClaimHeld(before, current); err != nil {
			return err
		}
		if current.ResourceVersion == endVersion {
			return migrationExecutorWaiting(before, current)
		}
	}
	return errors.New("running migration window did not reach both watch barriers")
}

// originalRunRecord is the record of the original Apply in a reading, or nil
// while nothing records it.
func originalRunRecord(before, current *ptahv1alpha1.PtahMigration) *ptahv1alpha1.MigrationRunStatus {
	if last := current.Status.LastRun; last != nil && last.OperationID == before.Status.ActiveOperation.ID {
		return last
	}
	return nil
}

// originalRunAppliedUnderItsBinding is the original Apply recorded as its own
// result: Applied, with the versions it reported, the Job that ran it, and the
// binding that authorized it, never the one that replaced it.
func originalRunAppliedUnderItsBinding(before *ptahv1alpha1.PtahMigration, run *ptahv1alpha1.MigrationRunStatus) error {
	active := before.Status.ActiveOperation
	if run == nil || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied || !slices.Equal(run.AppliedVersions, []int64{2, 3}) ||
		run.JobName != active.JobName || run.JobUID != active.JobUID || run.ExecutionBindingID != before.Status.ExecutionBinding.Epoch ||
		!run.StartedAt.Equal(&active.StartedAt) || run.FinishedAt == nil || run.FinishedAt.IsZero() ||
		run.TargetIdentityDigest != before.Status.History.TargetIdentityDigest || !sha256Pattern.MatchString(run.TargetIdentityDigest) ||
		!equality.Semantic.DeepEqual(run.PlanRef, active.PlanRef) || run.DispatchedBy == nil {
		return errors.New("the original Apply is not recorded as its own Applied result under the binding that authorized it")
	}
	return nil
}

// sameRunRecord compares two readings of one run record. The first reading of
// the database after the run adds its verification, and nothing else changes.
func sameRunRecord(first, later *ptahv1alpha1.MigrationRunStatus) bool {
	if first == nil || later == nil {
		return false
	}
	a, b := first.DeepCopy(), later.DeepCopy()
	a.Verification, b.Verification = nil, nil
	return equality.Semantic.DeepEqual(a, b) &&
		(first.Verification == nil || equality.Semantic.DeepEqual(first.Verification, later.Verification))
}

// migrationExecutorSettledThenRotated is the end of the rollout: the original
// Apply recorded as its own result under the binding that authorized it, the
// binding moved to the replacement executor alone, and the first reading of the
// database under it confirming what the run applied. Nothing was replayed, and
// no record says the run is anyone's to establish.
func migrationExecutorSettledThenRotated(before, current *ptahv1alpha1.PtahMigration, replacement string) error {
	if runningMigrationClaim(before) != nil || current == nil || !sameRunningMigration(before, current) {
		return errors.New("migration recovery lacks the original running execution")
	}
	old, next := before.Status.ExecutionBinding, current.Status.ExecutionBinding
	if next == nil || next.ExecutorImage != replacement || replacement == old.ExecutorImage || next.Epoch == old.Epoch ||
		!executionEpoch.MatchString(next.Epoch) || next.PtahVersion != old.PtahVersion ||
		next.ControllerStateVersion != old.ControllerStateVersion || next.RunnerProtocolVersion != old.RunnerProtocolVersion {
		return errors.New("the binding did not move to the replacement executor alone")
	}
	active := before.Status.ActiveOperation
	if current.Status.ActiveOperation != nil || current.Status.Plan != nil || current.Status.PendingLockRelease != nil ||
		current.Status.UnresolvedRun != nil || current.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] != "" ||
		(current.Status.ResolvedRun != nil && current.Status.ResolvedRun.OperationID == active.ID) {
		return errors.New("the original run was replayed, left in flight or recorded as nobody's")
	}
	run, history := originalRunRecord(before, current), current.Status.History
	if err := originalRunAppliedUnderItsBinding(before, run); err != nil {
		return err
	}
	if history == nil || history.CurrentVersion != 3 || history.AppliedCount != 3 || history.PendingCount != 0 || history.Dirty ||
		len(history.ModifiedVersions) != 0 || len(history.OutOfOrderVersions) != 0 ||
		history.TargetIdentityDigest != run.TargetIdentityDigest || history.ObservedAt.Before(run.FinishedAt) ||
		run.Verification == nil || run.Verification.Result != ptahv1alpha1.MigrationRunVerifiedInSync ||
		run.Verification.CurrentVersion != 3 || run.Verification.ObservedAt.Before(run.FinishedAt) ||
		history.ObservedAt.Before(&run.Verification.ObservedAt) ||
		!conditionIs(current.Status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched") {
		return errors.New("no reading of the database after the run confirmed what it applied")
	}
	return nil
}

// migrationExecutorReadWindow holds every watched reading from before to the
// settled one to the order the rollout owes: the claim stands until the run is
// recorded, the write that records it keeps the binding that authorized it,
// nothing is claimed under that binding afterwards, and the binding moves once
// and only after the run is recorded. No reading leaves the run for a person.
func migrationExecutorReadWindow(events []watchEvent[*ptahv1alpha1.PtahMigration], before *ptahv1alpha1.PtahMigration,
	replacement, endVersion string,
) error {
	if runningMigrationClaim(before) != nil || before.ResourceVersion == "" || endVersion == "" || endVersion == before.ResourceVersion {
		return errors.New("running migration window has no exact API boundaries")
	}
	old := before.Status.ExecutionBinding
	started, rotated := false, ""
	var recorded *ptahv1alpha1.MigrationRunStatus
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
		if event.Type == watch.Deleted || !sameRunningMigration(before, current) || current.Status.ExecutionBinding == nil {
			return errors.New("running migration window lost its resource or binding")
		}
		if current.Status.UnresolvedRun != nil || current.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] != "" {
			return errors.New("running migration window left the original run for a person to establish")
		}
		binding := current.Status.ExecutionBinding
		run := originalRunRecord(before, current)
		switch {
		case run == nil && recorded != nil:
			return errors.New("running migration window lost the record of the original run")
		case run == nil:
			if err := migrationClaimHeld(before, current); err != nil {
				return err
			}
		default:
			if err := originalRunAppliedUnderItsBinding(before, run); err != nil {
				return err
			}
			if recorded == nil && !equality.Semantic.DeepEqual(binding, old) {
				return errors.New("running migration window moved the binding in the write that recorded the run")
			}
			if recorded != nil && !sameRunRecord(recorded, run) {
				return errors.New("running migration window rewrote the record of the original run")
			}
			recorded = run
			if binding.Epoch == old.Epoch {
				if rotated != "" || !equality.Semantic.DeepEqual(binding, old) {
					return errors.New("running migration window moved the binding back")
				}
				if current.Status.ActiveOperation != nil {
					return errors.New("running migration window claimed new work under the binding it retires")
				}
				break
			}
			if binding.ExecutorImage != replacement || (rotated != "" && binding.Epoch != rotated) {
				return errors.New("running migration window moved the binding to something other than the replacement, or twice")
			}
			rotated = binding.Epoch
			if claim := current.Status.ActiveOperation; claim != nil && claim.ExecutionBindingID != rotated {
				return errors.New("running migration window claimed work under another binding than the current one")
			}
		}
		if current.ResourceVersion == endVersion {
			if recorded == nil || rotated == "" {
				return errors.New("running migration window never recorded the run and moved the binding after it")
			}
			return nil
		}
	}
	return errors.New("running migration window did not reach both watch barriers")
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
