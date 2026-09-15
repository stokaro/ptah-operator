package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestMigrationLifecycleReachesInSyncThroughEveryStage drives one migration
// from nothing to InSync the way a cluster would: each operation is claimed,
// dispatched, answered by a result frame, and consumed, and the next phase is
// whatever the previous answer made true.
//
// It is the closest thing to the end-to-end proof that does not need a
// database: what it does not cover is the Job actually running, which is the
// e2e suite's job.
func TestMigrationLifecycleReachesInSyncThroughEveryStage(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	harness := newMigrationLifecycle(t, migration)

	harness.step("the execution binding is published", func() {
		if harness.migration().Status.ExecutionBinding == nil {
			t.Fatal("no execution binding")
		}
	})
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0,
		ResolvedDigest:    testDigest,
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	harness.phase(operatorv1alpha1.MigrationPhaseVerifying)

	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 0,
		ObservedArtifactType: dataplane.MigrationArtifactType, ResolvedDigest: testDigest,
	})
	harness.phase(operatorv1alpha1.MigrationPhaseReading)

	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	pending := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  2, TotalMigrations: 2, HasPendingChanges: true,
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
		},
	}
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationHistory:     &pending,
	})
	// Always publishes the plan and claims the Apply in the same settle: there
	// is no decision left to wait for.
	if harness.migration().Status.Plan == nil {
		t.Fatal("a pending sequence published no plan")
	}
	harness.claimed(operatorv1alpha1.MigrationOperationApply)
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationApply, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up", Outcome: dataplane.MigrationOutcomeApplied,
			Planned: []int64{3}, Applied: []int64{3},
		},
	})
	// The run's claim is not the verdict: the controller records the evidence
	// and reads the history again, which is the next claim below.
	if run := harness.migration().Status.LastRun; run == nil ||
		run.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied {
		t.Fatalf("last run = %#v", harness.migration().Status.LastRun)
	}

	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	settled := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  3, TotalMigrations: 2,
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
		},
	}
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationHistory:     &settled,
	})
	harness.phase(operatorv1alpha1.MigrationPhaseInSync)

	actual := harness.migration()
	if actual.Status.Plan != nil {
		t.Fatal("an executed plan was retained after the history confirmed it")
	}
	if actual.Status.History == nil || actual.Status.History.PendingCount != 0 ||
		actual.Status.History.CurrentVersion != 3 {
		t.Fatalf("history = %#v", actual.Status.History)
	}
	if contains(actual.Finalizers, migrationOperationFinalizer) {
		t.Fatal("the resource kept the operation finalizer with nothing in flight")
	}
}

// A database that has run more than the artifact carries is the rolled-back
// deployment, the tag moved to yesterday's build, the branch whose migrations
// were never merged. Nothing is pending there, because pending is a statement
// about the artifact's own migrations, and that is exactly how this read as
// success before: every sentence true, the verdict wrong.
func TestAHistoryPastTheArtifactIsRefusedRatherThanCalledInSync(t *testing.T) {
	t.Parallel()

	harness := newMigrationLifecycle(t, migrationFixture())
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0,
		ResolvedDigest:    testDigest,
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 0,
		ObservedArtifactType: dataplane.MigrationArtifactType, ResolvedDigest: testDigest,
	})

	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(runner.Result{
		Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationHistory: &dataplane.MigrationStatusReport{
			ContractVersion: dataplane.SupportedMigrationStatusContract,
			CurrentVersion:  2, TotalMigrations: 1,
			Migrations: []dataplane.MigrationRecord{
				{Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStateApplied},
			},
		},
	})

	actual := harness.migration()
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked for a database the artifact cannot account for",
			actual.Status.Phase)
	}
	blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionTrue ||
		blocked.Reason != string(operatorv1alpha1.ReasonHistoryAhead) {
		t.Fatalf("blocked condition = %#v, want True with HistoryAhead", blocked)
	}
	// Both numbers a reader has to compare are in the message, because only one
	// of them is a field.
	if !strings.Contains(blocked.Message, "version 2") || !strings.Contains(blocked.Message, "version 1") {
		t.Fatalf("blocked message = %q, want the two versions that disagree", blocked.Message)
	}
	if meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady) {
		t.Fatal("a database the artifact cannot account for was reported ready")
	}
	if actual.Status.Plan != nil {
		t.Fatal("a plan was published for a history this artifact cannot continue")
	}
	if actual.Status.History == nil || actual.Status.History.CurrentVersion != 2 ||
		actual.Status.History.AppliedCount != 1 {
		t.Fatalf("history = %#v, want the reading that disagrees with itself kept as read",
			actual.Status.History)
	}
}

// migrationLifecycle reconciles until something changes, and answers whatever
// Job the controller dispatched.
type migrationLifecycle struct {
	t          *testing.T
	reconciler *MigrationReconciler
	api        client.Client
	logs       *replayLogs
	key        client.ObjectKey
}

type replayLogs struct{ content []byte }

func (l *replayLogs) Read(context.Context, string, string, string) ([]byte, error) {
	return append([]byte(nil), l.content...), nil
}

func newMigrationLifecycle(t *testing.T, migration *operatorv1alpha1.PtahMigration) *migrationLifecycle {
	t.Helper()

	logs := &replayLogs{}
	reconciler, api := fakeMigrationReconciler(t, logs, migration, verificationPolicyConfigMap())
	harness := &migrationLifecycle{
		t: t, reconciler: reconciler, api: api, logs: logs,
		key: client.ObjectKeyFromObject(migration),
	}
	harness.settle()
	return harness
}

// settle reconciles until the controller stops asking to be requeued.
func (h *migrationLifecycle) settle() {
	h.t.Helper()

	for attempt := 0; attempt < 12; attempt++ {
		result, err := h.reconciler.Reconcile(context.Background(), migrationRequest(h.migrationObject()))
		if err != nil {
			h.t.Fatalf("Reconcile() error = %v", err)
		}
		if !result.Requeue && result.RequeueAfter == 0 {
			return
		}
		if result.RequeueAfter > 0 && !result.Requeue {
			return
		}
	}
	actual := h.migration()
	h.t.Fatalf("the controller never settled: phase=%q operation=%#v conditions=%#v",
		actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
}

func (h *migrationLifecycle) migration() *operatorv1alpha1.PtahMigration {
	h.t.Helper()

	actual := &operatorv1alpha1.PtahMigration{}
	if err := h.api.Get(context.Background(), h.key, actual); err != nil {
		h.t.Fatal(err)
	}
	return actual
}

func (h *migrationLifecycle) migrationObject() *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{
		Namespace: h.key.Namespace, Name: h.key.Name,
	}}
}

func (h *migrationLifecycle) step(name string, check func()) {
	h.t.Helper()
	h.t.Log(name)
	check()
}

func (h *migrationLifecycle) phase(want operatorv1alpha1.MigrationPhase) {
	h.t.Helper()

	if got := h.migration().Status.Phase; got != want {
		h.t.Fatalf("phase = %q, want %q", got, want)
	}
}

func (h *migrationLifecycle) coordinationDigest() string {
	h.t.Helper()

	operation := h.migration().Status.ActiveOperation
	if operation == nil {
		return ""
	}
	return operation.CoordinationDigest
}

// claimed settles the controller, asserts which operation it claimed, and then
// plays the part Kubernetes plays: the Job the controller created gets a Pod
// that ran, and a terminal status.
func (h *migrationLifecycle) claimed(want operatorv1alpha1.MigrationOperationType) {
	h.t.Helper()

	h.settle()
	migration := h.migration()
	operation := migration.Status.ActiveOperation
	if operation == nil || operation.Type != want {
		h.t.Fatalf("active operation = %#v, want a %s claim (phase %q, plan %#v, conditions %#v)",
			operation, want, migration.Status.Phase, migration.Status.Plan, migration.Status.Conditions)
	}
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: migration.Namespace, Name: operation.JobName}
	if err := h.api.Get(context.Background(), key, job); err != nil {
		h.t.Fatalf("the %s claim dispatched no Job: %v", want, err)
	}
	if job.Status.Conditions == nil {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}
		if err := h.api.Status().Update(context.Background(), job); err != nil {
			h.t.Fatal(err)
		}
	}
	pods := &corev1.PodList{}
	if err := h.api.List(context.Background(), pods, client.InNamespace(migration.Namespace)); err != nil {
		h.t.Fatal(err)
	}
	for index := range pods.Items {
		for _, owner := range pods.Items[index].OwnerReferences {
			if owner.UID == job.UID {
				return
			}
		}
	}
	if err := h.api.Create(context.Background(), terminalPodFor(job)); err != nil {
		h.t.Fatal(err)
	}
}

// terminalPodFor is the Pod the Job controller would have created and run: one
// attempt, the Job's own template metadata, and an executor that terminated.
func terminalPodFor(job *batchv1.Job) *corev1.Pod {
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"),
			GenerateName: job.Name + "-", UID: "pod-" + job.UID,
			Labels:          map[string]string{"job-name": job.Name},
			Annotations:     job.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "default", Priority: &priority, PreemptionPolicy: &preemption,
			Tolerations: []corev1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
				{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
			},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: executorContainerName, State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
			},
		}}},
	}
}

// answer puts one result frame where the controller reads it, then settles.
func (h *migrationLifecycle) answer(result runner.Result) {
	h.t.Helper()

	operation := h.migration().Status.ActiveOperation
	if operation == nil {
		h.t.Fatal("no operation is waiting for a result")
	}
	result.ProtocolVersion = runner.ProtocolVersion
	result.OperationID = operation.ID
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		h.t.Fatal(err)
	}
	h.logs.content = frame
	h.settle()
}
