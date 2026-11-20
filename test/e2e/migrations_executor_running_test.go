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
	"github.com/stokaro/ptah-operator/internal/controller"
)

func TestRunningMigrationPlanRequiresItsOwnPublishedArtifact(t *testing.T) {
	t.Parallel()
	digest := proofDigest("a")
	reference := "oci://registry.example/task/postgresql-running-executor-current@" + digest
	plan := &ptahv1alpha1.PtahMigrationPlan{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "test", UID: "plan"},
		Spec: ptahv1alpha1.PtahMigrationPlanSpec{
			MigrationRef: ptahv1alpha1.ImmutableObjectReference{Name: "running", UID: "migration"}, ArtifactDigest: digest,
			Migrations: []ptahv1alpha1.PlannedMigration{{Version: 2, Checksum: "second"}, {Version: 3, Checksum: "third"}},
		}}
	resource := &ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "test", UID: "migration", Generation: 2},
		Status: ptahv1alpha1.PtahMigrationStatus{ObservedGeneration: 2, Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval,
			Plan:     &ptahv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			Artifact: &ptahv1alpha1.SourceAccessBinding{Digest: digest},
		}}
	resource.Spec.Artifact.OCIRef = reference
	if err := runningMigrationApprovalPlan(resource, plan, reference, digest, []int64{2, 3}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigrationPlan){
		"shared mutable tag": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Spec.Artifact.OCIRef = "oci://registry.example/task/postgresql:stable"
		},
		"edited shared artifact": func(r *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.Artifact.Digest, p.Spec.ArtifactDigest = proofDigest("b"), proofDigest("b")
		},
		"old generation": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Generation++ },
		"HistoryModified refusal": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked
		},
		"another migration": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.MigrationRef.UID = "other"
		},
		"another plan":      func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) { p.UID = "replacement" },
		"another namespace": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) { p.Namespace = "other" },
		"no source reading": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.Artifact = nil },
		"another source reading": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.Artifact.Digest = proofDigest("c")
		},
		"work already dispatched": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
		},
		"unresolved work": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{}
		},
		"seed plan reused": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations = []ptahv1alpha1.PlannedMigration{{Version: 1, Checksum: "first"}}
		},
		"sequence reordered": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			slices.Reverse(p.Spec.Migrations)
		},
		"unbound file": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations[0].Checksum = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed, candidate := resource.DeepCopy(), plan.DeepCopy()
			mutate(changed, candidate)
			if err := runningMigrationApprovalPlan(changed, candidate, reference, digest, []int64{2, 3}); err == nil {
				t.Fatal("a foreign artifact, stale gate or different selected sequence passed")
			}
		})
	}
	for _, versions := range [][]int64{nil, {0, 3}, {2, 4}} {
		if err := runningMigrationApprovalPlan(resource, plan, reference, digest, versions); err == nil {
			t.Fatal("an absent or different expected sequence passed")
		}
	}
	if err := runningMigrationApprovalPlan(resource, plan, "oci://registry.example/task/postgresql:stable", digest, []int64{2, 3}); err == nil {
		t.Fatal("a mutable expected reference passed")
	}
}

func TestRunningMigrationAcceptsSeparateLockAndDDLConnections(t *testing.T) {
	t.Parallel()
	// The original PostgreSQL server journal records DDL on 8845 and advisory
	// lock acquisition/release on 8836, both from the executor at 10.244.3.66.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "original-executor"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.3.66"}}
	for _, reading := range []string{"8845/10.244.3.66/8836/10.244.3.66", "8845/10.244.3.66/8845/10.244.3.66"} {
		if !migrationExecutorBackendMatchesPod(reading, pod, pod.UID) {
			t.Fatalf("valid session binding refused: %s", reading)
		}
	}
	for _, reading := range []string{
		"", "8845/10.244.3.66", "8845/10.244.3.66/0/10.244.3.66", "0/10.244.3.66/8836/10.244.3.66",
		"8845/10.244.3.66/8836/10.244.3.67", "8845/10.244.3.67/8836/10.244.3.66",
		"8845/10.244.3.66/8836/10.244.3.66\n8846/10.244.3.66/8836/10.244.3.66",
	} {
		if migrationExecutorBackendMatchesPod(reading, pod, pod.UID) {
			t.Fatalf("incomplete, foreign or ambiguous sessions accepted: %s", reading)
		}
	}
	if migrationExecutorBackendMatchesPod("8845/10.244.3.66/8836/10.244.3.66", pod, "replacement") {
		t.Fatal("a replacement Pod accepted")
	}
	for _, engine := range []string{"postgresql", "mysql"} {
		if _, err := runningMigrationBackendSQL(engine, "ptah_e2e_running_executor"); err != nil {
			t.Fatal(err)
		}
		for _, database := range []string{"", "other' OR true", "two databases"} {
			if _, err := runningMigrationBackendSQL(engine, database); err == nil {
				t.Fatal("unsafe database identifier accepted")
			}
		}
	}
	if _, err := runningMigrationBackendSQL("unknown", "db"); err == nil {
		t.Fatal("unknown engine accepted")
	}
}

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

// runningMigrationExecutorFixture is the readings a rollout of the executor
// leaves around a running migration Apply, in order: the claim the dispatching
// manager wrote, the replacement's wait, the write that records the run under
// the binding that authorized it, the write that moves the binding, and the
// first reading of the database under the new binding.
type runningMigrationExecutorReadings struct {
	before, waiting, recorded, rotated, current *ptahv1alpha1.PtahMigration
}

func runningMigrationExecutorFixture() runningMigrationExecutorReadings {
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
		Source: &ptahv1alpha1.SourceAccessBinding{}, Target: &ptahv1alpha1.DatabaseTargetBinding{},
	}
	before.Status.Conditions = []metav1.Condition{{Type: "Progressing", Status: metav1.ConditionTrue, Reason: "ApprovedPlan", Message: "Applying 2 planned migrations"}}

	waiting := before.DeepCopy()
	waiting.ResourceVersion = "20"
	waiting.Status.Conditions[0].Message += controller.RotationWaitWords

	recorded := waiting.DeepCopy()
	recorded.ResourceVersion, recorded.Status.ActiveOperation, recorded.Status.Plan = "30", nil, nil
	finished := metav1.NewTime(at.Add(time.Minute))
	recorded.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{
		OperationID: digest, Outcome: ptahv1alpha1.MigrationRunOutcomeApplied, AppliedVersions: []int64{2, 3},
		JobName: "original-apply", JobUID: "J", ExecutionBindingID: ftEpoch, TargetIdentityDigest: digest,
		PlanRef: before.Status.Plan.DeepCopy(), StartedAt: metav1.NewTime(at), FinishedAt: &finished,
		DispatchedBy: &ptahv1alpha1.ManagerRecord{},
	}
	recorded.Status.Conditions = []metav1.Condition{{Type: "Progressing", Status: metav1.ConditionTrue, Reason: "VerifyingConvergence"}}

	rotated := recorded.DeepCopy()
	rotated.ResourceVersion = "40"
	rotated.Status.ExecutionBinding.Epoch, rotated.Status.ExecutionBinding.ExecutorImage = ftOtherEpoch, "replacement"

	current := rotated.DeepCopy()
	current.ResourceVersion = "60"
	observed := metav1.NewTime(at.Add(2 * time.Minute))
	current.Status.History = &ptahv1alpha1.MigrationHistoryStatus{TargetIdentityDigest: digest, ObservedAt: observed, CurrentVersion: 3, AppliedCount: 3}
	current.Status.LastRun.Verification = &ptahv1alpha1.MigrationRunVerificationStatus{
		ObservedAt: observed, CurrentVersion: 3, Result: ptahv1alpha1.MigrationRunVerifiedInSync,
	}
	current.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "HistoryMatched"}}
	return runningMigrationExecutorReadings{before: before, waiting: waiting, recorded: recorded, rotated: rotated, current: current}
}

func copyMigrationExecutorRecord(resource *ptahv1alpha1.PtahMigration) {
	content, err := json.Marshal(resource.Status.UnresolvedRun)
	if err != nil {
		panic(err)
	}
	resource.Annotations = map[string]string{ptahv1alpha1.UnresolvedRunAnnotation: string(content)}
}

// unresolvedOriginalRun is the record a rotation that did not wait leaves.
func unresolvedOriginalRun(r *ptahv1alpha1.PtahMigration) {
	r.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeUnknown,
		OperationID: "sha256:" + strings.Repeat("a", 64), JobName: "original-apply", JobUID: "J"}
	copyMigrationExecutorRecord(r)
}

func TestRunningMigrationExecutorWaitKeepsTheEntireClaim(t *testing.T) {
	t.Parallel()
	readings := runningMigrationExecutorFixture()
	before, waiting := readings.before, readings.waiting
	if err := migrationExecutorWaiting(before, waiting); err != nil {
		t.Fatal(err)
	}
	afterLeaseWait := waiting.DeepCopy()
	afterLeaseWait.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonOperationInProgress)
	afterLeaseWait.Status.Conditions[0].Message = "Apply operation is in progress" + controller.RotationWaitWords
	if err := migrationExecutorWaiting(before, afterLeaseWait); err != nil {
		t.Fatalf("the wait over the words a Lease wait leaves was refused: %v", err)
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
		"dropped plan":       func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan = nil },
		"run recorded":       func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun = readings.recorded.Status.LastRun.DeepCopy() },
		"recorded unknown":   unresolvedOriginalRun,
		"old resource UID":   func(r *ptahv1alpha1.PtahMigration) { r.UID = "replacement" },
		"spec edit":          func(r *ptahv1alpha1.PtahMigration) { r.Spec.Suspend = true },
		"another generation": func(r *ptahv1alpha1.PtahMigration) { r.Generation++ },
		"the dispatching manager's own words": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.Conditions[0].Message = "Applying 2 planned migrations"
		},
		"a Pod admission refusal's reason": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonPodAdmissionRefused)
		},
		"a lock wait's reason": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonWaitingForDatabaseLock)
		},
		"not progressing": func(r *ptahv1alpha1.PtahMigration) { r.Status.Conditions[0].Status = metav1.ConditionFalse },
	} {
		t.Run(name, func(t *testing.T) {
			current := waiting.DeepCopy()
			mutate(current)
			if migrationExecutorWaiting(before, current) == nil {
				t.Fatal("a reading that is not the replacement's wait for the original claim passed")
			}
		})
	}
}

func TestRunningMigrationExecutorWaitChecksEveryWatchedTransition(t *testing.T) {
	t.Parallel()
	readings := runningMigrationExecutorFixture()
	before, waiting := readings.before, readings.waiting
	held := before.DeepCopy()
	held.ResourceVersion = "15"
	events := []watchEvent[*ptahv1alpha1.PtahMigration]{ftEvent(watch.Modified, before), ftEvent(watch.Modified, held), ftEvent(watch.Modified, waiting)}
	if err := migrationExecutorWaitWindow(events, before, waiting.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"lost claim between correct endpoints":      func(r *ptahv1alpha1.PtahMigration) { r.Status.ActiveOperation = nil },
		"rotated binding between correct endpoints": func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.Epoch = ftOtherEpoch },
		"unknown record between correct endpoints":  unresolvedOriginalRun,
	} {
		t.Run(name, func(t *testing.T) {
			bad := held.DeepCopy()
			mutate(bad)
			broken := slices.Clone(events)
			broken[1] = ftEvent(watch.Modified, bad)
			if migrationExecutorWaitWindow(broken, before, waiting.ResourceVersion) == nil {
				t.Fatal("a correct final reading hid an intermediate one that let the claim go")
			}
		})
	}
	if migrationExecutorWaitWindow(events[:2], before, held.ResourceVersion) == nil {
		t.Fatal("a window that ends before the replacement's wait passed")
	}
	for _, broken := range [][]watchEvent[*ptahv1alpha1.PtahMigration]{nil, events[1:], events[:2], {ftEvent(watch.Modified, before), ftEvent(watch.Deleted, waiting)}} {
		if migrationExecutorWaitWindow(broken, before, waiting.ResourceVersion) == nil {
			t.Fatal("a missing watch barrier or deletion passed")
		}
	}
}

func TestRunningMigrationExecutorSettlesUnderItsBindingThenRotates(t *testing.T) {
	t.Parallel()
	readings := runningMigrationExecutorFixture()
	before, current := readings.before, readings.current
	if err := migrationExecutorSettledThenRotated(before, current, "replacement"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"old image":         func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.ExecutorImage = "old" },
		"old epoch":         func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.Epoch = ftEpoch },
		"another component": func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.PtahVersion = "other" },
		"replayed Apply": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ActiveOperation = before.Status.ActiveOperation.DeepCopy()
		},
		"published plan":   func(r *ptahv1alpha1.PtahMigration) { r.Status.Plan = before.Status.Plan.DeepCopy() },
		"recorded unknown": unresolvedOriginalRun,
		"resolved as nobody's": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ResolvedRun = &ptahv1alpha1.ResolvedMigrationRunStatus{OperationID: before.Status.ActiveOperation.ID}
		},
		"run Unknown": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeUnknown
		},
		"run Failed":          func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed },
		"no applied versions": func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.AppliedVersions = nil },
		"run under the new binding": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.ExecutionBindingID = ftOtherEpoch
		},
		"another run": func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.OperationID = "other" },
		"another Job": func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.JobUID = "other" },
		"another database": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.TargetIdentityDigest = "sha256:" + strings.Repeat("b", 64)
		},
		"no dispatcher":    func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.DispatchedBy = nil },
		"work remains":     func(r *ptahv1alpha1.PtahMigration) { r.Status.History.PendingCount = 1 },
		"dirty history":    func(r *ptahv1alpha1.PtahMigration) { r.Status.History.Dirty = true },
		"modified history": func(r *ptahv1alpha1.PtahMigration) { r.Status.History.ModifiedVersions = []int64{1} },
		"history of another database": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.History.TargetIdentityDigest = "sha256:" + strings.Repeat("b", 64)
		},
		"history read before the run finished": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.History.ObservedAt = metav1.NewTime(r.Status.LastRun.FinishedAt.Add(-time.Second))
		},
		"unverified run": func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.Verification = nil },
		"run verified pending": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.Verification.Result = ptahv1alpha1.MigrationRunVerifiedPending
		},
		"verified before it finished": func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.Verification.ObservedAt = metav1.NewTime(r.Status.LastRun.FinishedAt.Add(-time.Second))
		},
		"not ready": func(r *ptahv1alpha1.PtahMigration) { r.Status.Conditions[0].Status = metav1.ConditionFalse },
	} {
		t.Run(name, func(t *testing.T) {
			r := current.DeepCopy()
			mutate(r)
			if migrationExecutorSettledThenRotated(before, r, "replacement") == nil {
				t.Fatal("a run that is not its own Applied result under its binding, then confirmed, passed")
			}
		})
	}
}

func TestRunningMigrationExecutorReadWindowKeepsTheOrder(t *testing.T) {
	t.Parallel()
	readings := runningMigrationExecutorFixture()
	before := readings.before
	reading := readings.rotated.DeepCopy()
	reading.ResourceVersion = "50"
	reading.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationHistory, ID: "history", ExecutionBindingID: ftOtherEpoch}
	ordered := []*ptahv1alpha1.PtahMigration{before, readings.waiting, readings.recorded, readings.rotated, reading, readings.current}
	window := func(readings []*ptahv1alpha1.PtahMigration) []watchEvent[*ptahv1alpha1.PtahMigration] {
		var events []watchEvent[*ptahv1alpha1.PtahMigration]
		for _, r := range readings {
			events = append(events, ftEvent(watch.Modified, r))
		}
		return events
	}
	end := readings.current.ResourceVersion
	if err := migrationExecutorReadWindow(window(ordered), before, "replacement", end); err != nil {
		t.Fatal(err)
	}
	for name, at := range map[string]struct {
		index  int
		mutate func(*ptahv1alpha1.PtahMigration)
	}{
		"binding moved in the write that recorded the run": {2, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ExecutionBinding = readings.rotated.Status.ExecutionBinding.DeepCopy()
		}},
		"binding moved while the claim stood": {1, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ExecutionBinding = readings.rotated.Status.ExecutionBinding.DeepCopy()
		}},
		"new work claimed under the retired binding": {2, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ActiveOperation = reading.Status.ActiveOperation.DeepCopy()
			r.Status.ActiveOperation.ExecutionBindingID = ftEpoch
		}},
		"work claimed under another binding": {4, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ActiveOperation.ExecutionBindingID = ftEpoch
		}},
		"run recorded unknown": {2, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeUnknown
		}},
		"run left for a person": {3, unresolvedOriginalRun},
		"record lost":           {3, func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun = nil }},
		"record rewritten":      {4, func(r *ptahv1alpha1.PtahMigration) { r.Status.LastRun.FinishedAt = &metav1.Time{Time: time.Now()} }},
		"binding moved back": {4, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
		}},
		"binding moved elsewhere": {4, func(r *ptahv1alpha1.PtahMigration) { r.Status.ExecutionBinding.ExecutorImage = "third" }},
		"claim rewritten while held": {1, func(r *ptahv1alpha1.PtahMigration) {
			r.Status.ActiveOperation.LeaseEpoch = ftOtherEpoch
		}},
	} {
		t.Run(name, func(t *testing.T) {
			broken := make([]*ptahv1alpha1.PtahMigration, len(ordered))
			for i, r := range ordered {
				broken[i] = r.DeepCopy()
			}
			at.mutate(broken[at.index])
			if migrationExecutorReadWindow(window(broken), before, "replacement", end) == nil {
				t.Fatal("a window out of the order the rollout owes passed")
			}
		})
	}
	if migrationExecutorReadWindow(window(ordered[:3]), before, "replacement", readings.recorded.ResourceVersion) == nil {
		t.Fatal("a window that ends before the binding moved passed")
	}
	deleted := window(ordered)
	deleted[len(deleted)-1].Type = watch.Deleted
	for _, broken := range [][]watchEvent[*ptahv1alpha1.PtahMigration]{nil, window(ordered[1:]), window(ordered[:5]), deleted} {
		if migrationExecutorReadWindow(broken, before, "replacement", end) == nil {
			t.Fatal("a missing watch barrier or deletion passed")
		}
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
