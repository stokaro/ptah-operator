package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestMigrationReconcilerClaimsResolveFirst(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ExecutionBinding == nil {
		t.Fatal("the first reconciliation published no execution binding")
	}
	// The binding is its own durable boundary, so the claim lands next.
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual = readMigration(t, api, migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("active operation = %#v, want a Resolve claim", operation)
	}
	if operation.JobName == "" || !strings.HasPrefix(operation.JobName, "ptah-m-resolve-") {
		t.Fatalf("claimed Job name = %q", operation.JobName)
	}
	if operation.Source != nil || operation.Target != nil {
		t.Fatal("a Resolve claim carries neither a resolved artifact nor a database target")
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseResolving {
		t.Fatalf("phase = %q, want Resolving", actual.Status.Phase)
	}
	if !contains(actual.Finalizers, migrationOperationFinalizer) {
		t.Fatal("a claim in flight did not take the operation finalizer")
	}
}

func TestMigrationResolveResultAdvancesToVerification(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: operation.ID, ChildExitCode: 0,
		ResolvedDigest:    testDigest,
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json",
		ResolvedSize:      321,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the consumed claim was retained: %#v", actual.Status.ActiveOperation)
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseVerifying {
		t.Fatalf("phase = %q, want Verifying", actual.Status.Phase)
	}
	if actual.Status.Artifact == nil || actual.Status.Artifact.Digest != testDigest {
		t.Fatalf("artifact binding = %#v", actual.Status.Artifact)
	}
	harvested := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Spec.TTLSecondsAfterFinished == nil || *harvested.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatal("the consumed Job was not scheduled for cleanup")
	}
}

func TestMigrationVerifyRefusesAnArtifactOfAnotherType(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifying
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationVerify)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationVerify,
		OperationID: operation.ID, ChildExitCode: 0,
		ObservedArtifactType: dataplane.SchemaArtifactType,
		ResolvedDigest:       testDigest,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase == operatorv1alpha1.MigrationPhaseReading {
		t.Fatal("a schema artifact was accepted as a migration artifact")
	}
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != 2 {
		t.Fatalf("active operation after the refusal = %#v", actual.Status.ActiveOperation)
	}
}

func TestMigrationHistoryResultClassifiesWhatTheDatabaseSaid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		report     dataplane.MigrationStatusReport
		wantPhase  operatorv1alpha1.MigrationPhase
		wantReady  metav1.ConditionStatus
		wantReason operatorv1alpha1.ConditionReason
		wantPlan   bool
		// wantOutOfOrder is the exact set the history publishes, because which
		// migration arrived late is what a person decides from.
		wantOutOfOrder []int64
	}{
		{
			name: "every migration is applied",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 2,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:  operatorv1alpha1.MigrationPhaseInSync,
			wantReady:  metav1.ConditionTrue,
			wantReason: operatorv1alpha1.ReasonHistoryMatched,
		},
		{
			name: "the artifact carries migrations the database does not",
			report: dataplane.MigrationStatusReport{
				ContractVersion:   dataplane.SupportedMigrationStatusContract,
				CurrentVersion:    2,
				TotalMigrations:   3,
				HasPendingChanges: true,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
				},
			},
			wantPhase:  operatorv1alpha1.MigrationPhaseAwaitingApproval,
			wantReady:  metav1.ConditionFalse,
			wantReason: operatorv1alpha1.ReasonAwaitingApproval,
			wantPlan:   true,
		},
		{
			name: "an interrupted run left a dirty row",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 2,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateDirty},
				},
				DirtyRevision: &dataplane.MigrationDirty{Version: 3, Applied: 2, Total: 5},
			},
			wantPhase:  operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:  metav1.ConditionFalse,
			wantReason: operatorv1alpha1.ReasonHistoryDirty,
		},
		{
			name: "an applied migration was modified after it ran",
			report: dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract,
				CurrentVersion:  3,
				TotalMigrations: 2,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateModified},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:  operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:  metav1.ConditionFalse,
			wantReason: operatorv1alpha1.ReasonHistoryModified,
		},
		{
			// Ptah executes in linear order and refuses the whole run while a
			// pending migration sorts below the current version, so a plan for
			// this history would be a sequence nobody could execute.
			name: "a migration arrived below the version the database applied",
			report: dataplane.MigrationStatusReport{
				ContractVersion:   dataplane.SupportedMigrationStatusContract,
				CurrentVersion:    3,
				TotalMigrations:   3,
				HasPendingChanges: true,
				Migrations: []dataplane.MigrationRecord{
					{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateOutOfOrder},
					{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
				},
			},
			wantPhase:      operatorv1alpha1.MigrationPhaseBlocked,
			wantReady:      metav1.ConditionFalse,
			wantReason:     operatorv1alpha1.ReasonHistoryOutOfOrder,
			wantOutOfOrder: []int64{2},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := test.report
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q (conditions %#v)", actual.Status.Phase, test.wantPhase, actual.Status.Conditions)
			}
			if actual.Status.History == nil {
				t.Fatal("the history was not published")
			}
			if actual.Status.History.CurrentVersion != test.report.CurrentVersion {
				t.Fatalf("current version = %d, want %d", actual.Status.History.CurrentVersion, test.report.CurrentVersion)
			}
			if !slices.Equal(actual.Status.History.OutOfOrderVersions, test.wantOutOfOrder) {
				t.Fatalf("out-of-order versions = %v, want %v",
					actual.Status.History.OutOfOrderVersions, test.wantOutOfOrder)
			}
			ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady)
			if ready == nil || ready.Status != test.wantReady || ready.Reason != string(test.wantReason) {
				t.Fatalf("Ready condition = %#v, want %s/%s", ready, test.wantReady, test.wantReason)
			}
			if actual.Status.NextReconciliationTime == nil {
				t.Fatal("a settled migration scheduled no next reconciliation")
			}
			if !test.wantPlan {
				if actual.Status.Plan != nil {
					t.Fatalf("a history with nothing to plan published %#v", actual.Status.Plan)
				}
				return
			}
			if actual.Status.Plan == nil {
				t.Fatal("a pending sequence published no plan")
			}
			plans := &operatorv1alpha1.PtahMigrationPlanList{}
			if err := api.List(context.Background(), plans); err != nil {
				t.Fatal(err)
			}
			if len(plans.Items) != 1 {
				t.Fatalf("published plans = %d, want one", len(plans.Items))
			}
			plan := plans.Items[0]
			if plan.Name != actual.Status.Plan.Name || plan.Spec.MigrationRef.UID != migration.UID {
				t.Fatalf("plan identity = %#v", plan.ObjectMeta)
			}
			if plan.Spec.HistoryFingerprint != actual.Status.History.Fingerprint {
				t.Fatal("the plan does not name the history it was computed against")
			}
			if len(plan.Spec.Migrations) != 1 || plan.Spec.Migrations[0].Version != 3 {
				t.Fatalf("planned sequence = %#v", plan.Spec.Migrations)
			}
			if plan.Spec.TargetIdentityDigest != testDigest || plan.Spec.ArtifactDigest != testDigest {
				t.Fatalf("plan bindings = %#v", plan.Spec)
			}
		})
	}
}

func TestMigrationReconcilerRefusesAnUnsupportedEngine(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Target.Engine = "cockroachdb"
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatal("an unsupported engine still claimed an operation")
	}
}

func TestMigrationReconcilerSuspendsWithoutClaiming(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Suspend = true
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseSuspended || actual.Status.ActiveOperation != nil {
		t.Fatalf("status = %#v, want a suspended migration with no claim", actual.Status)
	}
}

func TestMigrationReconcilerRetiresAClaimUnderAChangedExecutionBinding(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Status.Artifact = resolvedMigrationArtifact()
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a claim authorized under a retired execution binding survived")
	}
	if actual.Status.ExecutionBinding == nil ||
		actual.Status.ExecutionBinding.ExecutorImage != "example.invalid/ptah@"+testDigest {
		t.Fatalf("execution binding = %#v", actual.Status.ExecutionBinding)
	}
}

func TestMigrationReconcilerDiscardsAClaimWhoseInputsChanged(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	operation.InputFingerprint = "sha256:" + strings.Repeat("b", 64)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil {
		t.Fatal("a claim whose inputs changed was still dispatched")
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a stale claim created %d Jobs", len(jobs.Items))
	}
}

func TestMigrationDispatchPersistsTheSnapshotBeforeTheJob(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	operation.AdmissionSnapshot = nil
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
	reconciler.Jobs = workloadBuilderForMigrations()

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.AdmissionSnapshot == nil {
		t.Fatalf("dispatch did not persist the Pod admission snapshot: phase=%q operation=%#v conditions=%#v",
			actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("the Job was created before its snapshot was durable")
	}

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("dispatched Jobs = %d, want one", len(jobs.Items))
	}
	created := jobs.Items[0]
	if created.Name != actual.Status.ActiveOperation.JobName {
		t.Fatalf("Job name = %q, want the claimed %q", created.Name, actual.Status.ActiveOperation.JobName)
	}
	if created.Labels[workload.LabelMigration] != migration.Name ||
		created.Labels[workload.LabelComponent] != workload.ComponentMigrationOperation {
		t.Fatalf("Job labels = %#v", created.Labels)
	}
}

// fixedClock is the test clock the reconciler and its Lease share, so a Lease
// acquired in one reconciliation is still held in the next.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func migrationRequest(migration *operatorv1alpha1.PtahMigration) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}
}

func readMigration(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
	t.Helper()

	actual := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(migration), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

func migrationFrame(t *testing.T, result runner.Result) []byte {
	t.Helper()

	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func fakeMigrationReconciler(
	t *testing.T,
	logs PodLogReader,
	objects ...client.Object,
) (*MigrationReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		operatorv1alpha1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme,
		nodev1.AddToScheme, schedulingv1.AddToScheme, coordinationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "default", UID: "default-service-account-uid", ResourceVersion: "1",
	}})
	api := fake.NewClientBuilder().WithScheme(scheme).
		// A real API server stamps a UID on every object it accepts, and the
		// controller refuses a Job without one. The fake client does not, so a
		// Job it created would be refused by its own creator.
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context,
				writer client.WithWatch,
				object client.Object,
				options ...client.CreateOption,
			) error {
				if object.GetUID() == "" {
					object.SetUID(types.UID("created-" + object.GetName()))
				}
				// The controller-write guard re-derives a migration plan from
				// the migration's own STORED status and refuses one it cannot
				// reproduce. A fake client that accepted a plan built from a
				// status this process had not written yet would let every test
				// pass against a sequence a cluster refuses, which is exactly
				// what happened: the plan was created before the history that
				// justifies it was persisted, and only a live cluster said so.
				if plan, ok := object.(*operatorv1alpha1.PtahMigrationPlan); ok {
					stored := &operatorv1alpha1.PtahMigration{}
					key := client.ObjectKey{Namespace: plan.Namespace, Name: plan.Spec.MigrationRef.Name}
					if err := writer.Get(ctx, key, stored); err != nil {
						return fmt.Errorf("guard: read the migration a plan names: %w", err)
					}
					if stored.Status.Artifact == nil || stored.Status.History == nil ||
						stored.Status.ExecutionBinding == nil {
						return fmt.Errorf(
							"guard: the stored migration has no resolved artifact, history, and execution binding to plan from")
					}
				}
				return writer.Create(ctx, object, options...)
			},
		}).
		WithStatusSubresource(
			&operatorv1alpha1.PtahMigration{}, &operatorv1alpha1.PtahMigrationPlan{},
			&operatorv1alpha1.PtahMigrationApproval{}, &batchv1.Job{},
		).
		WithObjects(objects...).Build()
	clock := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	testClock := fixedClock{now: clock}
	reconciler := &MigrationReconciler{
		Client: api, APIReader: api, Scheme: scheme, Logs: logs, Jobs: fakeJobs{},
		LockNamespace:    "ptah-system",
		Clock:            testClock.Now,
		AdmissionOptions: podintent.DefaultOptions(),
	}
	reconciler.Locks = targetlock.New(api, api, testClock)
	return reconciler, api
}

func workloadBuilderForMigrations() workload.Builder {
	return workload.Builder{
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            "example.invalid/operator@" + testDigest,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
	}
}

func migrationFixture() *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "orders", UID: types.UID("migration-uid"), Generation: 1,
		},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "team-a/orders-primary",
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
				},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.example/team/migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
			Execution: operatorv1alpha1.ExecutionSpec{ActiveDeadlineSeconds: 900},
		},
	}
}

// verificationPolicyConfigMap is the immutable policy object a Verify claim is
// fingerprinted against. A policy that changes after the claim is what makes
// the claim's result stale.
func verificationPolicyConfigMap() *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "verification", UID: types.UID("verification-policy-uid"), ResourceVersion: "1",
		},
		Immutable: &immutable,
		Data:      map[string]string{"policy.yaml": "requireDigestPin: true"},
	}
}

func migrationExecutionBinding() *operatorv1alpha1.ExecutionBindingStatus {
	return &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  testExecutionBindingID,
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
		PtahVersion:            "v0.3.0",
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            "example.invalid/operator@" + testDigest,
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
}

func resolvedMigrationArtifact() *operatorv1alpha1.OCIArtifactAccessBinding {
	return &operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		Digest:            testDigest,
	}
}

// migrationClaim persists a claim the way the controller would, including the
// input fingerprint the reconciler recomputes before it dispatches.
func migrationClaim(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	operationType operatorv1alpha1.MigrationOperationType,
) *operatorv1alpha1.MigrationOperationStatus {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reconciler := &MigrationReconciler{
		Jobs:      fakeJobs{},
		APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(verificationPolicyConfigMap()).Build(),
	}
	fingerprintValue, err := reconciler.migrationInputFingerprint(context.Background(), migration, operationType)
	if err != nil {
		t.Fatal(err)
	}
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:               operationType,
		ID:                 testDigest,
		InputFingerprint:   fingerprintValue,
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
		StartedAt:          metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)),
		Attempt:            1,
	}
	if operationType != operatorv1alpha1.MigrationOperationResolve {
		operation.Source = migrationSourceBinding(migration)
	}
	if operationType == operatorv1alpha1.MigrationOperationHistory {
		coordinationDigest, digestErr := fingerprint.DatabaseCoordinationDigest(
			string(migration.Spec.Target.Engine), migration.Spec.Target.CoordinationKey,
		)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		operation.CoordinationDigest = coordinationDigest
		operation.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		}
	}
	name, err := (fakeJobs{}).NameForMigration(migration, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	migration.Status.ActiveOperation = operation
	migration.Status.ObservedGeneration = migration.Generation
	return operation
}

// terminalMigrationWorkload is the Job and Pod a consumed claim reads its
// result from, carrying the exact admission evidence the claim persisted.
func terminalMigrationWorkload(
	migration *operatorv1alpha1.PtahMigration,
	conditionType batchv1.JobConditionType,
) (*batchv1.Job, *corev1.Pod) {
	operation := migration.Status.ActiveOperation
	ensureMigrationAdmissionSnapshot(migration)
	annotations := map[string]string{
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
	}
	controller := true
	blockDeletion := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: operation.JobName, UID: "job-uid", Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration",
				Name: migration.Name, UID: migration.UID,
				Controller: &controller, BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec:   batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}}},
	}
	operation.JobUID = job.UID
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: migration.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"),
		GenerateName: job.Name + "-", UID: "pod-uid",
		Labels: map[string]string{"job-name": job.Name}, Annotations: annotations,
		OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}, Spec: corev1.PodSpec{
		ServiceAccountName: "default", Priority: &priority, PreemptionPolicy: &preemption,
		Tolerations: []corev1.Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
			{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		},
	}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}}}
	return job, pod
}

func ensureMigrationAdmissionSnapshot(migration *operatorv1alpha1.PtahMigration) {
	operation := migration.Status.ActiveOperation
	if operation.AdmissionSnapshot != nil {
		return
	}
	preemption := corev1.PreemptLowerPriority
	templateDigest, err := podintent.DigestTemplate(&corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{workload.AnnotationExecutionBindingID: operation.ExecutionBindingID},
	}})
	if err != nil {
		panic(err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{Object: operatorv1alpha1.AdmissionObjectBinding{
			Name: "default", UID: "default-service-account-uid", ResourceVersion: "1",
		}},
		PriorityClass:                       operatorv1alpha1.PriorityClassAdmissionSnapshot{Value: 0, PreemptionPolicy: &preemption},
		DefaultTolerationsEnabled:           true,
		DefaultNotReadyTolerationSeconds:    300,
		DefaultUnreachableTolerationSeconds: 300,
	}
	digest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		panic(err)
	}
	snapshot.Digest = digest
	operation.AdmissionSnapshot = snapshot
}
