package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// sameInstant reports two optional instants as equal, nil included.
func sameInstant(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// A schema the verification policy refuses is Blocked, and the read it repeats at
// its interval claims Resolve and Verify before the policy refuses it again.
// Neither claim is Blocked; the date of the block stays through both, at the
// instant the first refusal wrote, and a later clock does not move it.
func TestABlockedSchemaKeepsItsDateThroughTheReadItRepeats(t *testing.T) {
	t.Parallel()
	harness := refusedSchemaLifecycle(t)
	clock := movableSchemaClock(harness.reconciler, harness.api)
	refused := harness.schema()
	blocked := refused.Status.BlockedSince
	if refused.Status.Phase != operatorv1alpha1.PhaseBlocked || blocked == nil {
		t.Fatalf("the refusal left phase %s and blockedSince %v, want Blocked and a date", refused.Status.Phase, blocked)
	}
	next := refused.Status.NextReconciliationTime
	if next == nil {
		t.Fatal("the refusal scheduled no next read")
	}
	clock.now = next.Add(time.Minute)
	if clock.now.Sub(blocked.Time) < time.Minute {
		t.Fatalf("the clock %s is not distinctly after the block %s, so a new date would read as the old one", clock.now, blocked)
	}

	harness.claimed(operatorv1alpha1.OperationResolve)
	assertSchemaBlockedSince(t, harness.schema(), operatorv1alpha1.PhaseResolving, blocked)
	harness.answer(runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	harness.claimed(operatorv1alpha1.OperationVerify)
	assertSchemaBlockedSince(t, harness.schema(), operatorv1alpha1.PhaseVerifying, blocked)
	harness.refuses()
	assertSchemaBlockedSince(t, harness.schema(), operatorv1alpha1.PhaseBlocked, blocked)
}

func assertSchemaBlockedSince(t *testing.T, schema *operatorv1alpha1.PtahSchema, phase operatorv1alpha1.ReconciliationPhase, want *metav1.Time) {
	t.Helper()
	if schema.Status.Phase != phase || !sameInstant(schema.Status.BlockedSince, want) {
		t.Fatalf("phase %s with blockedSince %v, want phase %s with blockedSince %v",
			schema.Status.Phase, schema.Status.BlockedSince, phase, want)
	}
}

func assertMigrationBlockedSince(t *testing.T, migration *operatorv1alpha1.PtahMigration, phase operatorv1alpha1.MigrationPhase, want *metav1.Time) {
	t.Helper()
	if migration.Status.Phase != phase || !sameInstant(migration.Status.BlockedSince, want) {
		t.Fatalf("phase %s with blockedSince %v, want phase %s with blockedSince %v",
			migration.Status.Phase, migration.Status.BlockedSince, phase, want)
	}
}

// A migration the policy refuses keeps the date of its block through the read
// that refuses it again, and loses it on the read that ends in sync.
func TestABlockedMigrationKeepsItsDateUntilAPassEndsElsewhere(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	refused := harness.migration()
	blocked := refused.Status.BlockedSince
	if blocked == nil {
		t.Fatalf("the refusal left phase %s and no blockedSince", refused.Status.Phase)
	}
	clock.now = clock.now.Add(time.Minute)
	if clock.now.Sub(blocked.Time) < time.Minute {
		t.Fatalf("the clock %s is not distinctly after the block %s, so a new date would read as the old one", clock.now, blocked)
	}

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseResolving, blocked)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseVerifying, blocked)
	harness.answer(refusedBy(testDigest))
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseBlocked, blocked)

	harness.dueNow(clock)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseReading, blocked)
	harness.answer(historyAnswer(harness, inSyncReport()))
	assertMigrationBlockedSince(t, harness.migration(), operatorv1alpha1.MigrationPhaseInSync, nil)
}

// A schema under apply: Never reports Blocked with ApplyDisabled whenever it
// has a plan, and nothing about it waits for a person: the plan is recorded on
// purpose. No pass dates it, and a date an earlier block left is cleared by the
// pass that ends there.
func TestASchemaUnderApplyNeverCarriesNoBlockedDate(t *testing.T) {
	t.Parallel()
	walk := newScheduledWalk(t)
	setApply := func(apply operatorv1alpha1.ApplyPolicy) {
		t.Helper()
		edited := walk.stored()
		edited.Spec.Policy.Apply = apply
		edited.Generation++
		if err := walk.api.Update(context.Background(), edited); err != nil {
			t.Fatalf("set apply %s: %v", apply, err)
		}
	}
	document := safetyPlanDocument(t, "observed-state")
	setApply(operatorv1alpha1.ApplyPolicyNever)
	published := walk.publish("plan-1", document)
	assertBlockedBy(t, published, operatorv1alpha1.ReasonApplyDisabled)
	if published.Status.BlockedSince != nil {
		t.Fatalf("the plan under Never dated a block at %s", published.Status.BlockedSince)
	}

	// A block a person had to clear before the read, carried through it.
	dated := walk.stored()
	earlier := metav1.NewTime(walk.clock.now.Add(-time.Hour).Truncate(time.Second))
	dated.Status.BlockedSince = &earlier
	if err := walk.api.Status().Update(context.Background(), dated); err != nil {
		t.Fatal(err)
	}
	walk.clock.now = published.Status.NextReconciliationTime.Add(time.Second)
	again := walk.refresh(document)
	if again.Status.Phase != operatorv1alpha1.PhaseBlocked || again.Status.BlockedSince != nil {
		t.Fatalf("the read under Never left phase %s with blockedSince %v, want Blocked with none",
			again.Status.Phase, again.Status.BlockedSince)
	}
}
