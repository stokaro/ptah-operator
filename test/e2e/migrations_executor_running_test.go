package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestMigrationExecutorPodBarrierRefusesManagedWorkloads(t *testing.T) {
	t.Parallel()
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "test", UID: "database-uid",
		Labels: map[string]string{"app.kubernetes.io/name": "mysql"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if _, err := migrationExecutorPodBarrierSource([]corev1.Pod{pod}, "test", "mysql"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"managed Apply": func(p *corev1.Pod) { p.Labels["app.kubernetes.io/managed-by"] = "ptah-operator" },
		"migration":     func(p *corev1.Pod) { p.Labels[labelMigration] = "uncertain" },
		"schema":        func(p *corev1.Pod) { p.Labels[labelSchema] = "uncertain" },
		"Job owner": func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "apply", UID: "job-uid"}}
		},
		"wrong namespace": func(p *corev1.Pod) { p.Namespace = "other" },
		"wrong database":  func(p *corev1.Pod) { p.Labels["app.kubernetes.io/name"] = "postgresql" },
		"missing UID":     func(p *corev1.Pod) { p.UID = "" },
		"not running":     func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending },
		"deleting":        func(p *corev1.Pod) { at := metav1.Now(); p.DeletionTimestamp = &at },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if _, err := migrationExecutorPodBarrierSource([]corev1.Pod{*changed}, "test", "mysql"); err == nil {
				t.Fatal("an invalid or managed Pod became the writable watch sentinel")
			}
		})
	}
	for _, pods := range [][]corev1.Pod{nil, {pod, pod}} {
		if _, err := migrationExecutorPodBarrierSource(pods, "test", "mysql"); err == nil {
			t.Fatal("an empty or ambiguous database collection passed")
		}
	}
}

func TestMigrationExecutorPodWatchNeedsTheExactOriginalReading(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "apply", Namespace: "test", UID: "original", ResourceVersion: "123"}}
	for _, kind := range []watch.EventType{watch.Added, watch.Modified} {
		if !migrationExecutorPodWatchReached([]watchEvent[*corev1.Pod]{{Type: kind, Object: pod.DeepCopy()}}, pod) {
			t.Fatal("the exact original Pod reading was not recognized")
		}
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"sentinel only":   func(p *corev1.Pod) { p.Name, p.UID = "database", "database-uid" },
		"replacement":     func(p *corev1.Pod) { p.UID = "replacement" },
		"old version":     func(p *corev1.Pod) { p.ResourceVersion = "122" },
		"other namespace": func(p *corev1.Pod) { p.Namespace = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if migrationExecutorPodWatchReached([]watchEvent[*corev1.Pod]{{Type: watch.Modified, Object: changed}}, pod) {
				t.Fatal("a different Pod or resourceVersion closed the original workload history")
			}
		})
	}
	for _, events := range [][]watchEvent[*corev1.Pod]{nil, {{Type: watch.Deleted, Object: pod}}, {{Type: watch.Added, Object: nil}}} {
		if migrationExecutorPodWatchReached(events, pod) {
			t.Fatal("an absent or deleted Pod reading passed")
		}
	}
}

func runningMigrationExecutorFixture() (*ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigration) {
	before, _, _, _ := migrationReplacementFixture()
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	before.ResourceVersion, before.Finalizers = "10", []string{migrationFinalizer}
	before.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: ftEpoch, ExecutorImage: "old", PtahVersion: "v0.3.0", RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	before.Status.History = &ptahv1alpha1.MigrationHistoryStatus{TargetIdentityDigest: digest, CurrentVersion: 1, AppliedCount: 1, PendingCount: 2}
	horizon := metav1.NewTime(at.Add(5 * time.Minute))
	before.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{
		Type: ptahv1alpha1.MigrationOperationApply, ID: digest, JobName: "original-apply", JobUID: "J",
		StartedAt: metav1.NewTime(at), ExecutionNotAfter: &horizon, DispatchStarted: true,
		ExecutionBindingID: ftEpoch, PlanRef: before.Status.Plan.DeepCopy(),
		AdmissionSnapshot: &ptahv1alpha1.PodAdmissionSnapshot{}, LeaseEpoch: ftEpoch, LeaseDurationSeconds: 360,
		Source: &ptahv1alpha1.OCIArtifactAccessBinding{}, Target: &ptahv1alpha1.DatabaseTargetBinding{},
	}
	held := before.DeepCopy()
	held.ResourceVersion, held.Status.Plan = "20", nil
	recorded := metav1.NewTime(at.Add(time.Minute))
	held.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeUnknown,
		JobName: "original-apply", JobUID: "J", StartedAt: metav1.NewTime(at), FinishedAt: &recorded, DispatchedBy: &ptahv1alpha1.ManagerRecord{}}
	held.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeUnknown,
		OperationID: digest, JobName: "original-apply", JobUID: "J", PlanRef: *before.Status.Plan.DeepCopy(),
		TargetIdentityDigest: digest, RecordedAt: recorded, DispatchedBy: &ptahv1alpha1.ManagerRecord{}}
	held.Status.Conditions = []metav1.Condition{{Type: "Blocked", Status: metav1.ConditionTrue, Reason: "ApplyOutcomeUnknown"}, {Type: "Ready", Status: metav1.ConditionFalse}}
	copyMigrationExecutorRecord(held)
	current := held.DeepCopy()
	current.ResourceVersion, current.Status.ActiveOperation, current.Status.UnresolvedRun = "30", nil, nil
	delete(current.Annotations, ptahv1alpha1.UnresolvedRunAnnotation)
	current.Status.ExecutionBinding.Epoch, current.Status.ExecutionBinding.ExecutorImage = ftOtherEpoch, "replacement"
	observed := metav1.NewTime(at.Add(2 * time.Minute))
	current.Status.History = &ptahv1alpha1.MigrationHistoryStatus{TargetIdentityDigest: digest, ObservedAt: observed, CurrentVersion: 3, AppliedCount: 3}
	current.Status.ResolvedRun = &ptahv1alpha1.ResolvedMigrationRunStatus{OperationID: digest, Outcome: ptahv1alpha1.MigrationRunOutcomeUnknown,
		Resolution: ptahv1alpha1.MigrationRunResolvedByHistoryRead, ResolvedAt: observed}
	current.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "HistoryMatched"}}
	return before, held, current
}

func copyMigrationExecutorRecord(resource *ptahv1alpha1.PtahMigration) {
	content, err := json.Marshal(resource.Status.UnresolvedRun)
	if err != nil {
		panic(err)
	}
	resource.Annotations = map[string]string{ptahv1alpha1.UnresolvedRunAnnotation: string(content)}
}

func TestRunningMigrationExecutorKeepsItsEntireClaimAndUnknownRecord(t *testing.T) {
	t.Parallel()
	before, held, _ := runningMigrationExecutorFixture()
	if err := migrationExecutorHeld(before, held); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"lost claim":              func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation = nil },
		"new Apply":               func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation.ID = "other" },
		"changed deadline":        func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation.ExecutionNotAfter = nil },
		"lost finalizer":          func(r *ptahv1alpha1.PtahMigration) { r.Finalizers = nil },
		"rotated binding":         func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.Epoch = ftOtherEpoch },
		"new image":               func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.ExecutorImage = "replacement" },
		"lost realm":              func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation.LeaseEpoch = "" },
		"lost admission snapshot": func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation.AdmissionSnapshot = nil },
		"premature release": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		},
		"new plan":            func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan = before.Status.Plan.DeepCopy() },
		"ready while unknown": func(r *ptahv1alpha1.PtahMigration) { r.Status.Conditions[1].Status = metav1.ConditionTrue },
		"old resource UID":    func(r *ptahv1alpha1.PtahMigration) { r.UID = "replacement" },
		"spec edit":           func(r *ptahv1alpha1.PtahMigration) { r.Spec.Suspend = true },
		"another generation":  func(r *ptahv1alpha1.PtahMigration) { r.Generation++ },
		"missing outcome":     func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun = nil },
		"rewritten Applied outcome": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied
		},
		"applied versions attributed to Unknown": func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.AppliedVersions = []int64{2, 3} },
		"missing unresolved record":              func(r *ptahv1alpha1.PtahMigration) { r.Status.UnresolvedRun = nil },
		"unrelated target": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.UnresolvedRun.TargetIdentityDigest = "sha256:" + strings.Repeat("b", 64)
			copyMigrationExecutorRecord(r)
		},
		"unrelated plan": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.UnresolvedRun.PlanRef.UID = "other"
			copyMigrationExecutorRecord(r)
		},
		"unrelated Job": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.UnresolvedRun.JobUID = "other"
			copyMigrationExecutorRecord(r)
		},
		"missing metadata copy": func(r *ptahv1alpha1.PtahMigration) { r.Annotations = nil },
		"partial metadata copy": func(r *ptahv1alpha1.PtahMigration) {
			r.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] = `{"operationID":"x"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			current := held.DeepCopy()
			mutate(current)
			if migrationExecutorHeld(before, current) == nil {
				t.Fatal("lost running-Apply protection passed")
			}
		})
	}
}

func TestRunningMigrationExecutorChecksEveryWatchedTransition(t *testing.T) {
	t.Parallel()
	before, held, _ := runningMigrationExecutorFixture()
	end := held.DeepCopy()
	end.ResourceVersion = "21"
	events := []watchEvent[*ptahv1alpha1.PtahMigration]{ftEvent(watch.Modified, before), ftEvent(watch.Modified, held), ftEvent(watch.Modified, end)}
	if err := migrationExecutorHeldWindow(events, before, "21"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"lost claim between correct endpoints":  func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation = nil },
		"lost record between correct endpoints": func(r *ptahv1alpha1.PtahMigration) { r.Status.UnresolvedRun = nil },
		"rewritten recording time": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.UnresolvedRun.RecordedAt = metav1.NewTime(r.Status.UnresolvedRun.RecordedAt.Add(time.Second))
			copyMigrationExecutorRecord(r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := held.DeepCopy()
			bad.ResourceVersion = "intermediate"
			mutate(bad)
			broken := slices.Clone(events)
			broken = append(broken[:2], ftEvent(watch.Modified, bad), broken[2])
			if migrationExecutorHeldWindow(broken, before, "21") == nil {
				t.Fatal("a correct final reading hid a lost intermediate protection")
			}
		})
	}
	for _, broken := range [][]watchEvent[*ptahv1alpha1.PtahMigration]{nil, events[1:], events[:2], {ftEvent(watch.Modified, before), ftEvent(watch.Deleted, end)}} {
		if migrationExecutorHeldWindow(broken, before, "21") == nil {
			t.Fatal("a missing watch barrier or deletion passed")
		}
	}
}

func TestRunningMigrationExecutorNeedsFreshHistoryWithoutReattribution(t *testing.T) {
	t.Parallel()
	before, held, current := runningMigrationExecutorFixture()
	if err := migrationExecutorHistoryRecovery(before, held, current, "replacement"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"old image":         func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.ExecutorImage = "old" },
		"old epoch":         func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.Epoch = ftEpoch },
		"another component": func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.PtahVersion = "other" },
		"replayed Apply": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ActiveOperation = before.Status.ActiveOperation.DeepCopy()
		},
		"still unknown":        func(r *ptahv1alpha1.PtahMigration) { r.Status.UnresolvedRun = held.Status.UnresolvedRun.DeepCopy() },
		"restore copy remains": func(r *ptahv1alpha1.PtahMigration) { r.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] = "old" },
		"old history": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.History.ObservedAt = held.Status.UnresolvedRun.RecordedAt
		},
		"another database": func(r *ptahv1alpha1.PtahMigration) { r.Status.History.TargetIdentityDigest = "other" },
		"work remains":     func(r *ptahv1alpha1.PtahMigration) { r.Status.History.PendingCount = 1 },
		"dirty history":    func(r *ptahv1alpha1.PtahMigration) { r.Status.History.Dirty = true },
		"modified history": func(r *ptahv1alpha1.PtahMigration) { r.Status.History.ModifiedVersions = []int64{1} },
		"rewritten success": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied
		},
		"another run resolved": func(r *ptahv1alpha1.PtahMigration) { r.Status.ResolvedRun.OperationID = "other" },
		"acknowledgment instead of History": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ResolvedRun.Resolution = ptahv1alpha1.MigrationRunResolvedByAcknowledgment
		},
		"no matching History time": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ResolvedRun.ResolvedAt = held.Status.UnresolvedRun.RecordedAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := current.DeepCopy()
			mutate(r)
			if migrationExecutorHistoryRecovery(before, held, r, "replacement") == nil {
				t.Fatal("an unaccounted or reattributed execution passed as fresh recovery")
			}
		})
	}
}

func TestMigrationExecutorNoReplayUsesOnlyTheOwnedMigrationHistory(t *testing.T) {
	t.Parallel()
	job := ftJob("J", "resource", "apply")
	job.Labels = map[string]string{labelMigration: "resource", labelOperation: "apply"}
	finished := job.DeepCopy()
	finished.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	pod := ftPod("P", "resource", "apply", corev1.PodRunning)
	pod.Labels = job.Labels
	stopped := pod.DeepCopy()
	stopped.Status.Phase = corev1.PodSucceeded
	jobs := []watchEvent[*batchv1.Job]{ftEvent(watch.Added, job), ftEvent(watch.Modified, finished)}
	pods := []watchEvent[*corev1.Pod]{ftEvent(watch.Added, pod), ftEvent(watch.Modified, stopped)}
	if !migrationExecutorNoReplay(jobs, pods, "resource", "J", "P") || migrationExecutorNoReplay(nil, nil, "resource", "J", "P") ||
		migrationExecutorNoReplay(jobs, pods, "unrelated", "J", "P") {
		t.Fatal("migration no-replay did not require the exact recorded Apply workload")
	}
	second := job.DeepCopy()
	second.UID = "second"
	if migrationExecutorNoReplay(append(jobs, ftEvent(watch.Added, second)), pods, "resource", "J", "P") {
		t.Fatal("a second migration Apply passed")
	}
	secondPod := pod.DeepCopy()
	secondPod.UID = "second"
	if migrationExecutorNoReplay(jobs, append(pods, ftEvent(watch.Added, secondPod)), "resource", "J", "P") ||
		migrationExecutorNoReplay(jobs, nil, "resource", "J", "P") || migrationExecutorNoReplay(jobs[1:], pods, "resource", "J", "P") {
		t.Fatal("an unobserved or replayed migration Pod passed")
	}
	reading := job.DeepCopy()
	reading.UID, reading.Labels[labelOperation] = "history", "history"
	if migrationExecutorNoReplay([]watchEvent[*batchv1.Job]{jobs[0], ftEvent(watch.Added, reading), jobs[1]}, pods, "resource", "J", "P") {
		t.Fatal("History overlapping the old Apply passed")
	}
	if _, mutated := job.Labels[labelSchema]; mutated {
		t.Fatal("the predicate changed an observed watch object")
	}
}
