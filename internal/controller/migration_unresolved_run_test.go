package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// partialApplyReconciler is the fixture unresolvedMigrationRun records its run
// through, stopped before the Reconcile so a test can put an interceptor in
// front of the reconciler's writes.
func partialApplyReconciler(t *testing.T) (*MigrationReconciler, client.WithWatch, *operatorv1alpha1.PtahMigration) {
	t.Helper()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	logs := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomePartial,
			Planned:         []int64{3},
		},
	})
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	return reconciler, api, migration
}

// migrationWrites records, in order, the writes the reconciler makes to a
// PtahMigration: "copy" for a metadata write that leaves the copy on the
// object, "uncopy" for one that takes it off, and "status" for a status write,
// suffixed with whether that status carries a record.
type migrationWrites struct {
	mu     sync.Mutex
	writes []string
	// failStatusWithRecord refuses a status write that stores a record, and
	// failUncopy refuses the metadata write that removes the copy: each is a
	// manager that stopped between two writes.
	failStatusWithRecord bool
	failUncopy           bool
}

func (w *migrationWrites) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, api client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok {
				_, copied := migration.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]
				if !copied && w.failUncopy {
					return errors.New("the manager stopped before the copy came off")
				}
				w.record(map[bool]string{true: "copy", false: "uncopy"}[copied])
			}
			return api.Patch(ctx, object, patch, options...)
		},
		SubResourcePatch: func(
			ctx context.Context, api client.Client, subResource string, object client.Object, patch client.Patch,
			options ...client.SubResourcePatchOption,
		) error {
			if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok && subResource == "status" {
				withRecord := migration.Status.UnresolvedRun != nil
				if withRecord && w.failStatusWithRecord {
					return errors.New("the manager stopped before status stored the record")
				}
				w.record(map[bool]string{true: "status+record", false: "status"}[withRecord])
			}
			return api.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
	}
}

func (w *migrationWrites) record(write string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, write)
}

func (w *migrationWrites) index(write string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for index, recorded := range w.writes {
		if recorded == write {
			return index
		}
	}
	return -1
}

// The copy is the record a restore keeps, so it has to exist whenever the
// record does. It is written first: status never stores a record the copy
// does not already hold, and the copy is exactly that record.
func TestAnUnresolvedRunIsCopiedOntoMetadataBeforeStatusStoresIt(t *testing.T) {
	t.Parallel()

	reconciler, api, migration := partialApplyReconciler(t)
	writes := &migrationWrites{}
	reconciler.Client = interceptor.NewClient(api, writes.funcs())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, migration)
	if actual.Status.UnresolvedRun == nil {
		t.Fatal("the partial run recorded nothing, so this measured no copy")
	}
	copied, stored := writes.index("copy"), writes.index("status+record")
	if copied < 0 || stored < 0 || copied > stored {
		t.Fatalf("writes = %v; the copy has to land before the status write that stores the record", writes.writes)
	}
	decoded, err := decodeUnresolvedRunCopy(actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation])
	if err != nil {
		t.Fatalf("the copy does not read back: %v", err)
	}
	if !equality.Semantic.DeepEqual(decoded, actual.Status.UnresolvedRun) {
		t.Fatalf("copy = %#v\nrecord = %#v", decoded, actual.Status.UnresolvedRun)
	}
}

// A manager that stops after the copy and before the status write leaves a
// copy of the claim status still carries. That is not a restore: the claim is
// there, and harvesting it again writes the record the copy already names.
func TestACopyOfTheLiveClaimIsNotReadAsARestore(t *testing.T) {
	t.Parallel()

	reconciler, api, migration := partialApplyReconciler(t)
	writes := &migrationWrites{failStatusWithRecord: true}
	reconciler.Client = interceptor.NewClient(api, writes.funcs())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err == nil {
		t.Fatal("the status write was refused and the pass reported success")
	}
	stopped := readMigration(t, api, migration)
	if _, copied := stopped.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; !copied {
		t.Fatalf("writes = %v; the copy did not land, so this measured nothing", writes.writes)
	}
	if stopped.Status.UnresolvedRun != nil || stopped.Status.ActiveOperation == nil {
		t.Fatalf("the stop left status = %#v, want the claim and no record", stopped.Status)
	}

	reconciler.Client = api
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() after the stop error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.UnresolvedRun == nil || actual.Status.UnresolvedRun.OperationID != stopped.Status.ActiveOperation.ID {
		t.Fatalf("the harvest after the stop recorded %#v", actual.Status.UnresolvedRun)
	}
	if actual.Status.LastRun == nil || actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomePartial {
		t.Fatalf("the record came from a restore rather than from the run's own evidence: last run %#v", actual.Status.LastRun)
	}
}

// restoredWithoutStatus is what a restore that drops status recreates: the
// object's spec and metadata, and a status that says nothing.
func restoredWithoutStatus(recorded *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
	restored := migrationFixture()
	restored.UID = types.UID("restored-migration-uid")
	restored.Annotations = map[string]string{
		operatorv1alpha1.UnresolvedRunAnnotation: recorded.Annotations[operatorv1alpha1.UnresolvedRunAnnotation],
	}
	return restored
}

// A restore that drops status keeps the copy, and the record comes back from
// it before the resource plans anything: the migration the run may already
// have executed is still pending, and without the record it would be planned
// again.
func TestARestoreWithoutStatusBringsTheRecordBack(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	if recorded.Status.UnresolvedRun == nil {
		t.Fatal("the fixture recorded no unresolved run")
	}
	restored := restoredWithoutStatus(recorded)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, restored, verificationPolicyConfigMap())
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(restored)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, restored)
	if !equality.Semantic.DeepEqual(actual.Status.UnresolvedRun, recorded.Status.UnresolvedRun) {
		t.Fatalf("restored record = %#v\nwant %#v", actual.Status.UnresolvedRun, recorded.Status.UnresolvedRun)
	}
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
	}
	assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonApplyOutcomeUnknown)
	if event := findEvent(recorder, corev1.EventTypeWarning+" UnresolvedRunRestored"); !strings.Contains(event,
		recorded.Status.UnresolvedRun.OperationID) {
		t.Fatalf("the restore was not reported by the run it names: %q", event)
	}

	// And it holds: the reading that follows finds the migration pending and
	// publishes no plan for it.
	actual.Status.ExecutionBinding = migrationExecutionBinding()
	actual.Status.Artifact = resolvedMigrationArtifact()
	actual.Status.History = recorded.Status.History.DeepCopy()
	reading, _ := readMigrationHistory(t, actual, pendingMigrationHistory())
	if reading.Status.UnresolvedRun == nil || reading.Status.Plan != nil {
		t.Fatalf("the restored record did not refuse the pending migration: record %#v, plan %#v",
			reading.Status.UnresolvedRun, reading.Status.Plan)
	}
}

// A copy of a run status says was settled is what an interrupted removal
// leaves. It goes, and nothing is restored from it.
func TestACopyOfASettledRunIsRemovedRatherThanRestored(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	settled := recorded.DeepCopy()
	settled.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
		OperationID: recorded.Status.UnresolvedRun.OperationID,
		Outcome:     recorded.Status.UnresolvedRun.Outcome,
		Resolution:  operatorv1alpha1.MigrationRunResolvedByHistoryRead,
		ResolvedAt:  metav1.NewTime(time.Date(2026, 8, 30, 12, 30, 0, 0, time.UTC)),
	}
	settled.Status.UnresolvedRun = nil
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, settled, verificationPolicyConfigMap())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(settled)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, settled)
	if _, copied := actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; copied {
		t.Fatal("the copy of a settled run stayed on the resource")
	}
	if actual.Status.UnresolvedRun != nil {
		t.Fatalf("a settled run was restored from its leftover copy: %#v", actual.Status.UnresolvedRun)
	}
}

// A copy this manager cannot read is a record it cannot act on, and it holds
// the resource exactly as one it can read would.
func TestAnUnreadableCopyHoldsTheResource(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"not JSON":             "orders",
		"an unknown field":     `{"outcome":"Unknown","operationID":"` + testDigest + `","planRef":{"name":"p","uid":"u"},"recordedAt":"2026-08-30T12:00:00Z","newField":1}`,
		"a settled outcome":    `{"outcome":"Applied","operationID":"` + testDigest + `","planRef":{"name":"p","uid":"u"},"recordedAt":"2026-08-30T12:00:00Z"}`,
		"no operation ID":      `{"outcome":"Unknown","operationID":"orders","planRef":{"name":"p","uid":"u"},"recordedAt":"2026-08-30T12:00:00Z"}`,
		"a record and a spare": `{"outcome":"Unknown","operationID":"` + testDigest + `","planRef":{"name":"p","uid":"u"},"recordedAt":"2026-08-30T12:00:00Z"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			migration := migrationFixture()
			migration.Annotations = map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: value}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap())
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonApplyOutcomeUnknown)
			if actual.Status.ActiveOperation != nil {
				t.Fatalf("an operation was claimed under a copy nobody can read: %#v", actual.Status.ActiveOperation)
			}
			if actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation] != value {
				t.Fatal("the unreadable copy was rewritten or removed")
			}
		})
	}
}

// runAcknowledgment is what admission leaves in the API server after a person
// created an acknowledgment: the decision and the stamp.
func runAcknowledgment(
	migration *operatorv1alpha1.PtahMigration,
	name, operationID string,
	created time.Time,
) *operatorv1alpha1.PtahMigrationRunAcknowledgment {
	return &operatorv1alpha1.PtahMigrationRunAcknowledgment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: name, UID: types.UID(name + "-uid"),
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
			OperationID:  operationID,
			AcknowledgedBy: operatorv1alpha1.ApprovalIdentity{
				Username: "dba@example.test", UID: "dba-uid", Groups: []string{"dba", "system:authenticated"},
			},
			AcknowledgedAt:     metav1.NewTime(created),
			MutationRequestUID: "mutation-" + name,
		},
	}
}

func readAcknowledgment(
	t *testing.T,
	api client.Client,
	acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment,
) *operatorv1alpha1.PtahMigrationRunAcknowledgment {
	t.Helper()

	actual := &operatorv1alpha1.PtahMigrationRunAcknowledgment{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(acknowledgment), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

// A person's acknowledgment of the run the record names settles it, in that
// person's name, and the resource reads the database again before it plans.
func TestAnAcknowledgmentSettlesTheRunItNamesInTheNameOfWhoMadeIt(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	created := time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)
	acknowledgment := runAcknowledgment(recorded, "orders-run-acknowledged", recorded.Status.UnresolvedRun.OperationID, created)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, recorded, acknowledgment, verificationPolicyConfigMap())
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	writes := &migrationWrites{}
	reconciler.Client = interceptor.NewClient(api, writes.funcs())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(recorded)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, recorded)
	if actual.Status.UnresolvedRun != nil {
		t.Fatalf("the acknowledged record stayed: %#v", actual.Status.UnresolvedRun)
	}
	resolved := actual.Status.ResolvedRun
	if resolved == nil || resolved.Resolution != operatorv1alpha1.MigrationRunResolvedByAcknowledgment ||
		resolved.OperationID != recorded.Status.UnresolvedRun.OperationID ||
		resolved.Outcome != operatorv1alpha1.MigrationRunOutcomePartial {
		t.Fatalf("resolution = %#v", resolved)
	}
	if resolved.AcknowledgmentRef == nil || resolved.AcknowledgmentRef.UID != acknowledgment.UID ||
		resolved.AcknowledgmentRef.Name != acknowledgment.Name {
		t.Fatalf("the resolution does not name the acknowledgment that made it: %#v", resolved.AcknowledgmentRef)
	}
	if resolved.AcknowledgedBy == nil ||
		!equality.Semantic.DeepEqual(*resolved.AcknowledgedBy, acknowledgment.Spec.AcknowledgedBy) {
		t.Fatalf("the resolution records %#v, want the stamped identity %#v",
			resolved.AcknowledgedBy, acknowledgment.Spec.AcknowledgedBy)
	}
	if _, copied := actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; copied {
		t.Fatal("the copy of an acknowledged run stayed, so a restore would bring it back")
	}
	settled, uncopied := writes.index("status"), writes.index("uncopy")
	if settled < 0 || uncopied < 0 || uncopied < settled {
		t.Fatalf("writes = %v; the copy has to come off after status settles the run", writes.writes)
	}
	answered := readAcknowledgment(t, api, acknowledgment)
	if !meta.IsStatusConditionTrue(answered.Status.Conditions, operatorv1alpha1.ConditionAcknowledgmentConsumed) {
		t.Fatalf("the acknowledgment was not answered as consumed: %#v", answered.Status.Conditions)
	}
	if event := findEvent(recorder, corev1.EventTypeNormal+" UnresolvedRunAcknowledged"); !strings.Contains(event,
		"dba@example.test") {
		t.Fatalf("the settlement was not reported in the acknowledger's name: %q", event)
	}
	// The acknowledgment accounts for the database; it does not say what the
	// database now holds. The pass starts the evidence chain again and
	// dispatches no Apply.
	if operation := actual.Status.ActiveOperation; operation == nil ||
		operation.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("after the acknowledgment the resource claimed %#v, want a fresh Resolve", operation)
	}
}

// An acknowledgment that names another run settles nothing and is answered as
// stale; so is one admission never stamped.
func TestAnAcknowledgmentOfAnotherRunSettlesNothing(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	created := time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)
	otherRun := runAcknowledgment(recorded, "orders-other-run", safetyOtherDigest, created)
	unstamped := runAcknowledgment(recorded, "orders-unstamped", recorded.Status.UnresolvedRun.OperationID, created)
	unstamped.Spec.MutationRequestUID = ""
	otherMigration := runAcknowledgment(recorded, "orders-other-migration", recorded.Status.UnresolvedRun.OperationID, created)
	otherMigration.Spec.MigrationRef.UID = "recreated-migration-uid"
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, recorded, otherRun, unstamped, otherMigration,
		verificationPolicyConfigMap())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(recorded)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, recorded)
	if actual.Status.UnresolvedRun == nil || actual.Status.ResolvedRun != nil {
		t.Fatalf("an acknowledgment of something else settled the record: unresolved %#v, resolved %#v",
			actual.Status.UnresolvedRun, actual.Status.ResolvedRun)
	}
	for _, acknowledgment := range []*operatorv1alpha1.PtahMigrationRunAcknowledgment{otherRun, unstamped} {
		answered := readAcknowledgment(t, api, acknowledgment)
		if !meta.IsStatusConditionTrue(answered.Status.Conditions, operatorv1alpha1.ConditionAcknowledgmentStale) {
			t.Fatalf("%s was not answered as stale: %#v", acknowledgment.Name, answered.Status.Conditions)
		}
	}
	// An acknowledgment of another object under the same name is that
	// object's to answer.
	if answered := readAcknowledgment(t, api, otherMigration); len(answered.Status.Conditions) != 0 {
		t.Fatalf("an acknowledgment of another migration was answered by this one: %#v", answered.Status.Conditions)
	}
}

// A pass that settled the run and stopped before answering the acknowledgment
// leaves the answer to the next pass, which gives it from the resolution.
func TestAnAcknowledgmentIsAnsweredFromTheResolutionItMade(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	created := time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)
	first := runAcknowledgment(recorded, "orders-first", recorded.Status.UnresolvedRun.OperationID, created)
	second := runAcknowledgment(recorded, "orders-second", recorded.Status.UnresolvedRun.OperationID, created.Add(time.Minute))
	settled := recorded.DeepCopy()
	settled.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
		OperationID:       recorded.Status.UnresolvedRun.OperationID,
		Outcome:           recorded.Status.UnresolvedRun.Outcome,
		Resolution:        operatorv1alpha1.MigrationRunResolvedByAcknowledgment,
		AcknowledgmentRef: &operatorv1alpha1.ImmutableObjectReference{Name: first.Name, UID: first.UID},
		AcknowledgedBy:    first.Spec.AcknowledgedBy.DeepCopy(),
		ResolvedAt:        metav1.NewTime(created),
	}
	settled.Status.UnresolvedRun = nil
	delete(settled.Annotations, operatorv1alpha1.UnresolvedRunAnnotation)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, settled, first, second, verificationPolicyConfigMap())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(settled)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if answered := readAcknowledgment(t, api, first); !meta.IsStatusConditionTrue(answered.Status.Conditions,
		operatorv1alpha1.ConditionAcknowledgmentConsumed) {
		t.Fatalf("the acknowledgment that settled the run was not answered as consumed: %#v", answered.Status.Conditions)
	}
	answered := readAcknowledgment(t, api, second)
	stale := meta.FindStatusCondition(answered.Status.Conditions, operatorv1alpha1.ConditionAcknowledgmentStale)
	if stale == nil || stale.Status != metav1.ConditionTrue || !strings.Contains(stale.Message, "already settled") {
		t.Fatalf("the second acknowledgment of a settled run was answered %#v", answered.Status.Conditions)
	}
}

// The reading that settles a record says so, and takes the copy with it.
func TestAReadingThatSettlesTheRunRecordsItAndRemovesTheCopy(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	if _, copied := recorded.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; !copied {
		t.Fatal("the fixture carries no copy, so this measured nothing")
	}
	report := pendingMigrationHistory()
	report.HasPendingChanges = false
	report.PendingMigrations = nil
	report.CurrentVersion = 3
	report.TotalMigrations = 3
	report.Migrations = []dataplane.MigrationRecord{
		{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
		{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
	}
	actual, _ := readMigrationHistory(t, recorded, report)
	if actual.Status.UnresolvedRun != nil {
		t.Fatalf("a reading with nothing pending left the record: %#v", actual.Status.UnresolvedRun)
	}
	resolved := actual.Status.ResolvedRun
	if resolved == nil || resolved.Resolution != operatorv1alpha1.MigrationRunResolvedByHistoryRead ||
		resolved.OperationID != recorded.Status.UnresolvedRun.OperationID ||
		resolved.AcknowledgedBy != nil || resolved.AcknowledgmentRef != nil {
		t.Fatalf("resolution = %#v", resolved)
	}
	if _, copied := actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; copied {
		t.Fatal("the copy of a run a reading settled stayed on the resource")
	}
}

// A manager that stops between settling the run and removing its copy leaves
// a copy status says was settled. The next pass removes it.
func TestAnInterruptedCopyRemovalIsFinishedOnTheNextPass(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	created := time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)
	acknowledgment := runAcknowledgment(recorded, "orders-run-acknowledged", recorded.Status.UnresolvedRun.OperationID, created)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, recorded, acknowledgment, verificationPolicyConfigMap())
	writes := &migrationWrites{failUncopy: true}
	reconciler.Client = interceptor.NewClient(api, writes.funcs())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(recorded)); err == nil {
		t.Fatal("the copy removal was refused and the pass reported success")
	}
	stopped := readMigration(t, api, recorded)
	if stopped.Status.ResolvedRun == nil || stopped.Status.UnresolvedRun != nil {
		t.Fatalf("the stop left status = %#v, want the run settled", stopped.Status)
	}
	if _, copied := stopped.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; !copied {
		t.Fatal("the copy came off despite the stop, so this measured nothing")
	}

	reconciler.Client = api
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(recorded)); err != nil {
		t.Fatalf("Reconcile() after the stop error = %v", err)
	}
	actual := readMigration(t, api, recorded)
	if _, copied := actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; copied {
		t.Fatal("the leftover copy stayed")
	}
	if actual.Status.UnresolvedRun != nil {
		t.Fatalf("the settled run was restored from its leftover copy: %#v", actual.Status.UnresolvedRun)
	}
	if answered := readAcknowledgment(t, api, acknowledgment); !meta.IsStatusConditionTrue(answered.Status.Conditions,
		operatorv1alpha1.ConditionAcknowledgmentConsumed) {
		t.Fatalf("the acknowledgment was left unanswered: %#v", answered.Status.Conditions)
	}
}
