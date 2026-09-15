package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// realmSchemaFixture is a PtahSchema claiming one coordination realm, with just
// enough spec to reach the realm census.
func realmSchemaFixture(name, key string, shared bool) *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-b", Name: name, UID: types.UID("schema-" + name), Generation: 1,
		},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: key,
				SharedRealm:     shared,
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
				},
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.example/team/schema:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
			Interval: metav1.Duration{Duration: 10 * time.Minute},
		},
	}
}

func TestRealmCensusCountsEveryClaimantOfOneDatabase(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	sameRealm := realmSchemaFixture("orders", migration.Spec.Target.CoordinationKey, false)
	otherRealm := realmSchemaFixture("billing", "team-b/billing-primary", false)
	deleting := realmSchemaFixture("retired", migration.Spec.Target.CoordinationKey, false)
	removal := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &removal
	deleting.Finalizers = []string{"example.invalid/retain"}

	_, api := fakeMigrationReconciler(t, nil, migration, sameRealm, otherRealm, deleting)

	census, err := takeRealmCensus(
		context.Background(), api,
		migration.Spec.Target.Engine, migration.Spec.Target.CoordinationKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if census.Schemas != 1 || census.Migrations != 1 {
		t.Fatalf("census = %+v, want one schema and one migration", census)
	}
	if census.Undeclared != 2 {
		t.Fatalf("census.Undeclared = %d, want both claimants undeclared", census.Undeclared)
	}
	if !census.conflict() {
		t.Fatal("two undeclared claimants of one realm did not conflict")
	}
	// The key is the reader's own, but another namespace's object names are
	// not: the message carries counts and kinds and nothing else.
	message := census.message()
	for _, leaked := range []string{"orders", "billing", "team-b", migration.Spec.Target.CoordinationKey} {
		if strings.Contains(message, leaked) {
			t.Fatalf("realm conflict message named %q: %s", leaked, message)
		}
	}
}

func TestRealmCensusAcceptsOneClaimantAndAMutualDeclaration(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	_, alone := fakeMigrationReconciler(t, nil, migration)
	census, err := takeRealmCensus(
		context.Background(), alone,
		migration.Spec.Target.Engine, migration.Spec.Target.CoordinationKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if census.total() != 1 || census.conflict() {
		t.Fatalf("a single claimant conflicted: %+v", census)
	}

	shared := migrationFixture()
	shared.Spec.Target.SharedRealm = true
	peer := realmSchemaFixture("orders", shared.Spec.Target.CoordinationKey, true)
	_, both := fakeMigrationReconciler(t, nil, shared, peer)
	census, err = takeRealmCensus(
		context.Background(), both,
		shared.Spec.Target.Engine, shared.Spec.Target.CoordinationKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if census.total() != 2 || census.conflict() {
		t.Fatalf("two claimants that both declared the realm shared conflicted: %+v", census)
	}
}

// One claimant that has not declared the realm shared blocks every claimant,
// itself included. That is what makes the declaration mutual without naming
// anybody: a resource cannot widen its own permission alone.
func TestRealmCensusRefusesAOneSidedDeclaration(t *testing.T) {
	t.Parallel()

	declared := migrationFixture()
	declared.Spec.Target.SharedRealm = true
	silent := realmSchemaFixture("orders", declared.Spec.Target.CoordinationKey, false)
	_, api := fakeMigrationReconciler(t, nil, declared, silent)

	census, err := takeRealmCensus(
		context.Background(), api,
		declared.Spec.Target.Engine, declared.Spec.Target.CoordinationKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !census.conflict() {
		t.Fatalf("a one-sided declaration was accepted: %+v", census)
	}
	if census.Undeclared != 1 {
		t.Fatalf("census.Undeclared = %d, want the one silent claimant", census.Undeclared)
	}
}

func TestMigrationBlocksOnAContestedRealmWithoutDispatchingAJob(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	peer := realmSchemaFixture("orders", migration.Spec.Target.CoordinationKey, false)
	reconciler, api := fakeMigrationReconciler(t, nil, migration, peer, verificationPolicyConfigMap())

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(migration),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a contested realm did not schedule another verdict: %#v", result)
	}

	persisted := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", persisted.Status.Phase)
	}
	if persisted.Status.ActiveOperation != nil {
		t.Fatal("a contested realm claimed an operation")
	}
	blocked := findCondition(persisted.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionTrue ||
		blocked.Reason != string(operatorv1alpha1.ReasonRealmConflict) {
		t.Fatalf("Blocked condition = %#v, want True/RealmConflict", blocked)
	}
	if persisted.Status.NextReconciliationTime == nil {
		t.Fatal("a contested realm left no time to look again")
	}

	// Declaring the realm shared on both sides ends the refusal.
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(peer), peer); err != nil {
		t.Fatal(err)
	}
	peer.Spec.Target.SharedRealm = true
	if err := api.Update(context.Background(), peer); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatal(err)
	}
	persisted.Spec.Target.SharedRealm = true
	// An API server bumps the generation for a spec write, which is what makes
	// the next pass start the evidence chain again instead of waiting out the
	// refusal's own deadline.
	persisted.Generation++
	if err := api.Update(context.Background(), persisted); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(migration),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase == operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatal("a realm both claimants declared shared stayed blocked")
	}
}

func TestSchemaBlocksOnAContestedRealmWithoutDispatchingAJob(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	peer := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-b", Name: "orders", UID: types.UID("migration-peer"), Generation: 1,
		},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          schema.Spec.Target.Engine,
				CoordinationKey: schema.Spec.Target.CoordinationKey,
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
		},
	}
	reconciler, api := fakeReconciler(t, nil, schema, peer)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(schema),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a contested realm did not schedule another verdict: %#v", result)
	}

	persisted := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase != operatorv1alpha1.PhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", persisted.Status.Phase)
	}
	if persisted.Status.ActiveOperation != nil {
		t.Fatal("a contested realm claimed an operation")
	}
	// The approval webhook reads Blocked with ApprovalRequired false, so no
	// request beginning after this patch can authorize a plan.
	approvalRequired := findCondition(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if approvalRequired == nil || approvalRequired.Status != metav1.ConditionFalse ||
		approvalRequired.Reason != string(operatorv1alpha1.ReasonRealmConflict) {
		t.Fatalf("ApprovalRequired = %#v, want False/RealmConflict", approvalRequired)
	}
	ready := findCondition(persisted.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != string(operatorv1alpha1.ReasonRealmConflict) {
		t.Fatalf("Ready = %#v, want False/RealmConflict", ready)
	}

	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a contested realm created %d Jobs", len(jobs.Items))
	}
}
