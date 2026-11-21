package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// migrationStatusWrites records every status a reconciler writes for a
// migration, in order, so a row can name a write the harness settles past.
type migrationStatusWrites struct {
	mu     sync.Mutex
	writes []operatorv1alpha1.PtahMigrationStatus
}

func recordMigrationStatusWrites(reconciler *MigrationReconciler) *migrationStatusWrites {
	recorded := &migrationStatusWrites{}
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if err := c.SubResource(subResource).Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok && subResource == "status" {
				recorded.mu.Lock()
				recorded.writes = append(recorded.writes, *migration.Status.DeepCopy())
				recorded.mu.Unlock()
			}
			return nil
		},
	})
	return recorded
}

func (w *migrationStatusWrites) since(index int) []operatorv1alpha1.PtahMigrationStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]operatorv1alpha1.PtahMigrationStatus(nil), w.writes[index:]...)
}

func (w *migrationStatusWrites) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

// A migration under apply: Always whose history read finds nothing blocking
// writes Planning, publishes its plan and claims its Apply. The block ended on
// that read: the Planning write clears its date, and neither the plan's
// handoff nor the Apply carries one, so a long healthy Apply never reads as
// blocked, and a block the Apply ends in is dated from that block (#1160).
func TestAMigrationUnderAlwaysCarriesNoBlockDateIntoItsApply(t *testing.T) {
	t.Parallel()
	fixture := migrationFixture()
	fixture.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	harness := newMigrationLifecycle(t, fixture)
	clock := movableMigrationClock(harness.reconciler, harness.api)
	writes := recordMigrationStatusWrites(harness.reconciler)
	harness.refuse(clock)
	blocked := harness.migration().Status.BlockedSince
	if blocked == nil {
		t.Fatal("the refusal dated no block")
	}

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseReading, blocked)
	read := writes.count()
	harness.answer(historyAnswer(harness, pendingReport()))

	applying := harness.migration()
	if claim := applying.Status.ActiveOperation; claim == nil || claim.Type != operatorv1alpha1.MigrationOperationApply {
		t.Fatalf("the pending history under Always left phase %s with claim %+v, want an Apply claim", applying.Status.Phase, claim)
	}
	planning := 0
	for _, status := range writes.since(read) {
		if status.Phase == operatorv1alpha1.MigrationPhasePlanning && status.ActiveOperation == nil {
			planning++
		}
		if status.BlockedSince != nil {
			t.Errorf("a write at phase %s (claim %v) after the read that found nothing blocking carried blockedSince %s",
				status.Phase, status.ActiveOperation != nil, status.BlockedSince)
		}
	}
	if planning == 0 {
		t.Fatalf("no write at Planning without a claim followed the history read, so the row did not see the verdict it is about")
	}
	assertMigrationBlockedSince(t, applying, operatorv1alpha1.MigrationPhaseApplying, nil)
}

// A schema under apply: Never whose plan the destructive-change policy refuses
// is Blocked, and nobody is waited for: under Never the plan would not run
// either way. It carries no date, and the read that ends there clears one an
// earlier block left (#1160).
func TestADestructivePlanUnderApplyNeverCarriesNoBlockedDate(t *testing.T) {
	t.Parallel()
	walk := newScheduledWalk(t)
	setSchemaApply(t, walk, operatorv1alpha1.ApplyPolicyNever)
	document := safetyPlanDocument(t, "observed-state", true)
	published := walk.publish("plan-1", document)
	assertBlockedBy(t, published, operatorv1alpha1.ReasonPolicyBlocked)
	if published.Status.BlockedSince != nil {
		t.Fatalf("the destructive plan under Never dated a block at %s", published.Status.BlockedSince)
	}

	dated := walk.stored()
	earlier := metav1.NewTime(walk.clock.now.Add(-time.Hour).Truncate(time.Second))
	dated.Status.BlockedSince = &earlier
	if err := walk.api.Status().Update(context.Background(), dated); err != nil {
		t.Fatal(err)
	}
	walk.clock.now = published.Status.NextReconciliationTime.Add(time.Second)
	again := walk.refresh(document)
	assertBlockedBy(t, again, operatorv1alpha1.ReasonPolicyBlocked)
	if again.Status.BlockedSince != nil {
		t.Fatalf("the read under Never ended at the destructive block with blockedSince %s, want none", again.Status.BlockedSince)
	}
}

// The same plan under OnApproval waits for a person to narrow the schema or
// allow the change, so it is a block and is dated. Setting apply: Never then
// makes it the policy at work: the pass the edit starts keeps the date while
// it runs and clears it when it ends at the same block.
func TestADestructivePlanUnderOnApprovalIsDatedUntilApplyNever(t *testing.T) {
	t.Parallel()
	walk := newScheduledWalk(t)
	document := safetyPlanDocument(t, "observed-state", true)
	published := walk.publish("plan-1", document)
	assertBlockedBy(t, published, operatorv1alpha1.ReasonPolicyBlocked)
	if published.Status.BlockedSince == nil || !published.Status.BlockedSince.Time.Equal(walk.clock.now.Truncate(time.Second)) {
		t.Fatalf("the destructive plan under OnApproval left blockedSince %v, want the instant it was published (%s)",
			published.Status.BlockedSince, walk.clock.now)
	}
	blocked := published.Status.BlockedSince

	setSchemaApply(t, walk, operatorv1alpha1.ApplyPolicyNever)
	walk.clock.now = walk.clock.now.Add(time.Minute)
	if resolving := walk.claimed(operatorv1alpha1.OperationResolve); !sameInstant(resolving.Status.BlockedSince, blocked) {
		t.Fatalf("the pass the edit started claimed Resolve with blockedSince %v, want the block's %s", resolving.Status.BlockedSince, blocked)
	}
	again := walk.refreshFromResolve(document)
	assertBlockedBy(t, again, operatorv1alpha1.ReasonPolicyBlocked)
	if again.Status.BlockedSince != nil {
		t.Fatalf("the pass after apply: Never ended at the destructive block with blockedSince %s, want none", again.Status.BlockedSince)
	}
}

// Setting apply: Never on a schema Blocked by a read fault removes no cause:
// Ptah still refuses the artifact, a person still has to act, and the date of
// the block stays through the read the edit starts.
func TestAReadFaultUnderApplyNeverKeepsItsDate(t *testing.T) {
	t.Parallel()
	harness := refusedSchemaLifecycle(t)
	clock := movableSchemaClock(harness.reconciler, harness.api)
	refused := harness.schema()
	blocked := refused.Status.BlockedSince
	if refused.Status.Phase != operatorv1alpha1.PhaseBlocked || blocked == nil {
		t.Fatalf("the refusal left phase %s and blockedSince %v, want Blocked and a date", refused.Status.Phase, blocked)
	}
	refused.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyNever
	refused.Generation++
	if err := harness.api.Update(context.Background(), refused); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Minute)

	harness.readsAgain()
	harness.refuses()
	stored := harness.schema()
	assertSchemaBlockedSince(t, stored, operatorv1alpha1.PhaseBlocked, blocked)
	if stored.Status.ObservedGeneration != stored.Generation {
		t.Fatalf("the read after the edit observed generation %d, want %d", stored.Status.ObservedGeneration, stored.Generation)
	}
}

func setSchemaApply(t *testing.T, walk *scheduledWalk, apply operatorv1alpha1.ApplyPolicy) {
	t.Helper()
	edited := walk.stored()
	edited.Spec.Policy.Apply = apply
	edited.Generation++
	if err := walk.api.Update(context.Background(), edited); err != nil {
		t.Fatalf("set apply %s: %v", apply, err)
	}
}

func assertBlockedBy(t *testing.T, schema *operatorv1alpha1.PtahSchema, reason operatorv1alpha1.ConditionReason) {
	t.Helper()
	ready := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady)
	if schema.Status.Phase != operatorv1alpha1.PhaseBlocked || ready == nil || ready.Reason != string(reason) {
		t.Fatalf("phase %s with Ready %+v, want Blocked with reason %s", schema.Status.Phase, ready, reason)
	}
}

// failingPolicyReads answers a read of a ConfigMap with err while it is set,
// through both the cached client and the direct reader.
func failingPolicyReads(reconcilerClient *client.Client, apiReader *client.Reader, failure *atomic.Pointer[error]) {
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
		if _, ok := object.(*corev1.ConfigMap); ok {
			if err := failure.Load(); err != nil {
				return *err
			}
		}
		return c.Get(ctx, key, object, options...)
	}}
	*reconcilerClient = interceptor.NewClient((*reconcilerClient).(client.WithWatch), funcs)
	if *apiReader != nil {
		*apiReader = interceptor.NewClient((*apiReader).(client.WithWatch), funcs)
	}
}

// reconcileUntilError reconciles until a pass returns an error, and returns
// it; a controller that settles without one fails the row.
func reconcileUntilError(t *testing.T, reconcile func() (ctrl.Result, error)) error {
	t.Helper()
	for range 12 {
		result, err := reconcile()
		if err != nil {
			return err
		}
		if result.RequeueAfter == 0 || result.RequeueAfter > statusPatchRequeue {
			return nil
		}
	}
	t.Fatal("the controller never settled")
	return nil
}

func transientPolicyRead() *error {
	err := error(apierrors.NewServiceUnavailable("etcd leader changed"))
	return &err
}

// A refresh pass of a Blocked schema reads the verification policy again when
// it claims Verify. An API server that cannot answer that read this once is no
// verdict on anything: the pass returns the error, the workqueue retries it with
// the handoff to Verify intact, and the block keeps its date. Ending the pass
// Failed instead cleared the date, and the age started over at the next block.
func TestATransientPolicyReadAtTheVerifyClaimKeepsTheSchemasDate(t *testing.T) {
	t.Parallel()
	harness := refusedSchemaLifecycle(t)
	clock := movableSchemaClock(harness.reconciler, harness.api)
	var failure atomic.Pointer[error]
	failingPolicyReads(&harness.reconciler.Client, &harness.reconciler.APIReader, &failure)
	refused := harness.schema()
	blocked := refused.Status.BlockedSince
	if refused.Status.Phase != operatorv1alpha1.PhaseBlocked || blocked == nil {
		t.Fatalf("the refusal left phase %s and blockedSince %v, want Blocked and a date", refused.Status.Phase, blocked)
	}
	clock.now = refused.Status.NextReconciliationTime.Add(time.Minute)

	harness.claimed(operatorv1alpha1.OperationResolve)
	failure.Store(transientPolicyRead())
	harness.logs.content = schemaResultFrame(t, harness.schema(), runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	err := reconcileUntilError(t, func() (ctrl.Result, error) {
		return harness.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: harness.key})
	})
	if !apierrors.IsServiceUnavailable(err) {
		after := harness.schema()
		t.Fatalf("the Verify claim under an unavailable API server returned %v and left phase %s, claim %v, blockedSince %v; "+
			"want the error returned with the handoff and the date intact", err, after.Status.Phase, after.Status.ActiveOperation != nil, after.Status.BlockedSince)
	}
	waiting := harness.schema()
	if waiting.Status.ActiveOperation != nil {
		t.Fatalf("the failed read left a claim %+v", waiting.Status.ActiveOperation)
	}
	assertSchemaBlockedSince(t, waiting, operatorv1alpha1.PhaseVerifying, blocked)

	failure.Store(nil)
	harness.claimed(operatorv1alpha1.OperationVerify)
	harness.refuses()
	assertSchemaBlockedSince(t, harness.schema(), operatorv1alpha1.PhaseBlocked, blocked)
}

// The same read answered with a verdict is a configuration fault the retry
// cannot fix by itself: a policy that is gone ends the pass Failed, as before.
func TestAMissingPolicyAtTheVerifyClaimStillFailsTheSchema(t *testing.T) {
	t.Parallel()
	harness := refusedSchemaLifecycle(t)
	clock := movableSchemaClock(harness.reconciler, harness.api)
	var failure atomic.Pointer[error]
	failingPolicyReads(&harness.reconciler.Client, &harness.reconciler.APIReader, &failure)
	refused := harness.schema()
	clock.now = refused.Status.NextReconciliationTime.Add(time.Minute)

	harness.claimed(operatorv1alpha1.OperationResolve)
	missing := error(apierrors.NewNotFound(corev1.Resource("configmaps"), refused.Spec.Desired.VerificationPolicyFrom.Name))
	failure.Store(&missing)
	harness.answer(runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	failed := harness.schema()
	ready := meta.FindStatusCondition(failed.Status.Conditions, operatorv1alpha1.ConditionReady)
	if failed.Status.Phase != operatorv1alpha1.PhaseFailed || failed.Status.ActiveOperation != nil ||
		ready == nil || ready.Reason != string(operatorv1alpha1.ReasonConfigurationError) {
		t.Fatalf("a missing policy at the Verify claim left phase %s, claim %v, Ready %+v; want Failed with ConfigurationError",
			failed.Status.Phase, failed.Status.ActiveOperation != nil, ready)
	}
}

// The migration family claims Verify the same way, and reads the policy into
// the claim's inputs.
func TestATransientPolicyReadAtTheVerifyClaimKeepsTheMigrationsDate(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	blocked := harness.migration().Status.BlockedSince
	if blocked == nil {
		t.Fatal("the refusal dated no block")
	}
	var failure atomic.Pointer[error]
	failingPolicyReads(&harness.reconciler.Client, &harness.reconciler.APIReader, &failure)

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	failure.Store(transientPolicyRead())
	harness.logs.content = migrationResultFrame(t, harness.migration(), resolvedTo(testDigest))
	err := reconcileUntilError(t, func() (ctrl.Result, error) {
		return harness.reconciler.Reconcile(context.Background(), migrationRequest(harness.migrationObject()))
	})
	if !apierrors.IsServiceUnavailable(err) {
		after := harness.migration()
		t.Fatalf("the Verify claim under an unavailable API server returned %v and left phase %s, claim %v, blockedSince %v; "+
			"want the error returned with the handoff and the date intact", err, after.Status.Phase, after.Status.ActiveOperation != nil, after.Status.BlockedSince)
	}
	waiting := harness.migration()
	if waiting.Status.ActiveOperation != nil {
		t.Fatalf("the failed read left a claim %+v", waiting.Status.ActiveOperation)
	}
	assertMigrationBlockedSince(t, waiting, operatorv1alpha1.MigrationPhaseVerifying, blocked)

	failure.Store(nil)
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(refusedBy(testDigest))
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseBlocked, blocked)
}

func schemaResultFrame(t *testing.T, schema *operatorv1alpha1.PtahSchema, result runner.Result) []byte {
	t.Helper()
	if schema.Status.ActiveOperation == nil {
		t.Fatal("no operation is waiting for a result")
	}
	result.ProtocolVersion = runner.ProtocolVersion
	result.OperationID = schema.Status.ActiveOperation.ID
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func migrationResultFrame(t *testing.T, migration *operatorv1alpha1.PtahMigration, result runner.Result) []byte {
	t.Helper()
	if migration.Status.ActiveOperation == nil {
		t.Fatal("no operation is waiting for a result")
	}
	result.ProtocolVersion = runner.ProtocolVersion
	result.OperationID = migration.Status.ActiveOperation.ID
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
