package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// kubectl ptah set-source --for approval on a migration that waits for an
// approval of the previous artifact's plan. The reconciler runs from the
// stored change into the state it settles in, and the wait the command makes
// reads what it stored. A source check the controller never satisfies would
// only time out here. On this path the generation already keeps the wait off
// the previous plan; the operate and envtest rows hold the source check to
// documents that pass the generation and a fresh reading.
func TestSetSourceWaitReachesThePlanFromTheNewArtifact(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	repository := "oci://registry.example/team/migrations@"
	migration := migrationFixture()
	migration.Spec.Artifact.OCIRef = repository + oldDigest
	harness := newMigrationLifecycle(t, migration)
	clock := movableMigrationClock(harness.reconciler, harness.api)

	// readTo answers the read-only cycle with the artifact digest names, each
	// reading a minute after the last.
	readTo := func(digest string) {
		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		harness.answer(runner.Result{
			Operation: runner.OperationResolve, ChildExitCode: 0, ResolvedDigest: digest,
			ResolvedReference: repository + digest, ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
		})
		harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		harness.answer(runner.Result{
			Operation: runner.OperationVerify, ChildExitCode: 0,
			ObservedArtifactType: dataplane.MigrationArtifactType, ResolvedDigest: digest,
		})
		clock.now = clock.now.Add(time.Minute)
		harness.claimed(operatorv1alpha1.MigrationOperationHistory)
		pending := dataplane.MigrationStatusReport{
			ContractVersion: dataplane.SupportedMigrationStatusContract,
			CurrentVersion:  2, TotalMigrations: 2, HasPendingChanges: true,
			Migrations: []dataplane.MigrationRecord{
				{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
				{Version: 3, Checksum: "checksum-" + digest[7:10], State: dataplane.MigrationStatePending},
			},
		}
		harness.answer(runner.Result{
			Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
			CoordinationDigest: harness.coordinationDigest(), TargetIdentityDigest: testDigest, MigrationHistory: &pending,
		})
	}

	readTo(oldDigest)
	harness.phase(operatorv1alpha1.MigrationPhaseAwaitingApproval)
	previous := harness.migration()
	if previous.Status.Plan == nil || previous.Status.Artifact == nil || previous.Status.Artifact.Digest != oldDigest {
		t.Fatalf("the previous artifact left plan %#v and artifact %#v, so the row starts nowhere", previous.Status.Plan, previous.Status.Artifact)
	}

	// The change as the plugin sends it, through an API server that moves
	// the generation on a spec patch, which the fake one does not.
	bumping := interceptor.NewClient(harness.api.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			obj.SetGeneration(obj.GetGeneration() + 1)
			if err := c.Update(ctx, obj); err != nil {
				return err
			}
			return c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		},
	})
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: migration.Namespace, Name: migration.Name}
	pinned, err := operate.ParsePinnedReference(repository + newDigest)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := operate.SetSource(ctx, bumping, ref, pinned)
	if err != nil || !changed.Changed || changed.Generation != previous.Generation+1 {
		t.Fatalf("set-source = %+v, %v; want a change to generation %d", changed, err, previous.Generation+1)
	}
	options := operate.WaitOptions{
		Target: operate.TargetApproval, Timeout: time.Minute, Interval: 10 * time.Second,
		Clock: &waitClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		Since: changed.Object, Generation: changed.Generation, Source: pinned.Reference,
	}

	// Before the operator acts, what is stored is the previous artifact's
	// plan: the wait does not take it.
	var timedOut *operate.TimeoutError
	if _, err := operate.Wait(ctx, harness.api, ref, options); !errors.As(err, &timedOut) {
		t.Fatalf("a wait on the previous artifact's plan = %v, want a timeout", err)
	}

	clock.now = clock.now.Add(time.Minute)
	readTo(newDigest)
	harness.phase(operatorv1alpha1.MigrationPhaseAwaitingApproval)

	options.Clock = &waitClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	result, err := operate.Wait(ctx, harness.api, ref, options)
	if err != nil || result.Reads != 1 {
		t.Fatalf("a wait on the plan from the new artifact = %+v, %v; want it reached on the first read", result, err)
	}
	// What the wait's source check reads, status.artifact, is the artifact
	// the published plan was computed from.
	settled := harness.migration()
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := harness.api.Get(ctx, client.ObjectKey{Namespace: migration.Namespace, Name: settled.Status.Plan.Name}, plan); err != nil {
		t.Fatal(err)
	}
	if plan.Spec.ArtifactDigest != newDigest || settled.Status.Artifact.Digest != newDigest ||
		settled.Status.Plan.Name == previous.Status.Plan.Name {
		t.Fatalf("plan %s binds %s, status.artifact is %s; want a new plan from %s",
			plan.Name, plan.Spec.ArtifactDigest, settled.Status.Artifact.Digest, newDigest)
	}
}

// A schema's plan is what a set-source wait for approval holds to the pinned
// digest. The reconciler harvests a Plan Job for a schema whose spec asks for
// that digest, and the wait reaches the plan it published on its first read:
// the plan status carries the artifact digest the operator resolved.
func TestSetSourceWaitReachesTheSchemaPlanTheOperatorPublished(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema := planHarvestFixture(t, operatorv1alpha1.ApplyPolicyOnApproval, safetyPlanDocument(t, "observed-state"))
	pinned, err := operate.ParsePinnedReference("oci://registry.example/team/schema@" + testDigest)
	if err != nil {
		t.Fatal(err)
	}
	stored := safetyGetSchema(t, api, schema)
	stored.Spec.Desired.OCIRef = pinned.Reference
	if err := api.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	// The reading this cycle's plan is computed from, and the baseline: the
	// resource as set-source's answer carried it, a minute before that
	// reading.
	read := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	stored = safetyGetSchema(t, api, schema)
	stored.Status.Target.LastObservedAt = &read
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	baseline := safetyGetSchema(t, api, schema)
	earlier := metav1.NewTime(read.Add(-time.Minute))
	baseline.Status.Target.LastObservedAt = &earlier

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	published := safetyGetSchema(t, api, schema)
	if published.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval || published.Status.Plan == nil {
		t.Fatalf("phase %s with plan %#v, want a plan waiting for approval", published.Status.Phase, published.Status.Plan)
	}

	ref := operate.Ref{Family: operate.FamilySchema, Namespace: schema.Namespace, Name: schema.Name}
	result, err := operate.Wait(ctx, api, ref, operate.WaitOptions{
		Target: operate.TargetApproval, Timeout: time.Minute, Interval: 10 * time.Second,
		Clock: &waitClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		Since: baseline, Generation: baseline.Generation, Source: pinned.Reference,
	})
	if err != nil || result.Reads != 1 || published.Status.Plan.ArtifactDigest != pinned.Digest {
		t.Fatalf("a wait on the plan the operator published = %+v, %v (plan binds %s); want it reached on the first read",
			result, err, published.Status.Plan.ArtifactDigest)
	}
}
