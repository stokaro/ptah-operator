package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// realmSchemaFixture is a PtahSchema claiming one namespace's coordination
// key, with just enough spec to reach the realm census.
func realmSchemaFixture(namespace, name, key string, shared bool) *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID("schema-" + namespace + "-" + name), Generation: 1,
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

// realmBoundSchemaFixture is a PtahSchema that names a PtahRealm instead of a
// key of its own.
func realmBoundSchemaFixture(namespace, name, realm string, shared bool) *operatorv1alpha1.PtahSchema {
	schema := realmSchemaFixture(namespace, name, "", shared)
	schema.Spec.Target.CoordinationKey = ""
	schema.Spec.Target.RealmRef = &operatorv1alpha1.PtahRealmReference{Name: realm}
	return schema
}

// realmBoundMigrationFixture is the migration fixture moved onto a PtahRealm.
func realmBoundMigrationFixture(realm string, shared bool) *operatorv1alpha1.PtahMigration {
	migration := migrationFixture()
	migration.Spec.Target.CoordinationKey = ""
	migration.Spec.Target.RealmRef = &operatorv1alpha1.PtahRealmReference{Name: realm}
	migration.Spec.Target.SharedRealm = shared
	return migration
}

func realmFixture(name string, sharing operatorv1alpha1.RealmSharing, namespaces ...string) *operatorv1alpha1.PtahRealm {
	return &operatorv1alpha1.PtahRealm{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("realm-" + name), Generation: 1},
		Spec: operatorv1alpha1.PtahRealmSpec{
			Engine:     operatorv1alpha1.DatabaseEnginePostgreSQL,
			Namespaces: namespaces,
			Sharing:    sharing,
		},
	}
}

func censusOf(t *testing.T, api client.Reader, namespace string, target operatorv1alpha1.DatabaseTargetSpec) realmVerdict {
	t.Helper()
	verdict, err := takeRealmCensus(context.Background(), api, namespace, target)
	if err != nil {
		t.Fatal(err)
	}
	return verdict
}

func TestRealmCensusCountsEveryClaimantOfOneDatabase(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	key := migration.Spec.Target.CoordinationKey
	sameRealm := realmSchemaFixture(migration.Namespace, "orders", key, false)
	otherRealm := realmSchemaFixture(migration.Namespace, "billing", "team-a/billing-primary", false)
	deleting := realmSchemaFixture(migration.Namespace, "retired", key, false)
	removal := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &removal
	deleting.Finalizers = []string{"example.invalid/retain"}
	// Suspension is how a resource steps aside without being deleted, so a
	// suspended claimant is not one.
	suspended := realmSchemaFixture(migration.Namespace, "paused", key, false)
	suspended.Spec.Suspend = true

	_, api := fakeMigrationReconciler(t, nil, migration, sameRealm, otherRealm, deleting, suspended)

	verdict := censusOf(t, api, migration.Namespace, migration.Spec.Target)
	census := verdict.Census
	if census.Schemas != 1 || census.Migrations != 1 {
		t.Fatalf("census = %+v, want one schema and one migration", census)
	}
	if census.Undeclared != 2 {
		t.Fatalf("census.Undeclared = %d, want both claimants undeclared", census.Undeclared)
	}
	if !verdict.conflict() {
		t.Fatal("two undeclared claimants of one realm did not conflict")
	}
	refusal, refused := verdict.refusal()
	if !refused || refusal.Reason != operatorv1alpha1.ReasonRealmConflict {
		t.Fatalf("refusal = %+v, %t, want a RealmConflict", refusal, refused)
	}
	// The key is the reader's own, but the other objects' names are not: the
	// message carries counts and kinds and nothing else.
	for _, leaked := range []string{"orders", "billing", "paused", key} {
		if strings.Contains(refusal.Message, leaked) {
			t.Fatalf("realm conflict message named %q: %s", leaked, refusal.Message)
		}
	}
}

func TestRealmCensusAcceptsOneClaimantAndAMutualDeclaration(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	_, alone := fakeMigrationReconciler(t, nil, migration)
	verdict := censusOf(t, alone, migration.Namespace, migration.Spec.Target)
	if verdict.Census.total() != 1 || verdict.conflict() {
		t.Fatalf("a single claimant conflicted: %+v", verdict)
	}
	if _, refused := verdict.refusal(); refused {
		t.Fatalf("a single claimant of its own namespace key was refused: %+v", verdict)
	}

	shared := migrationFixture()
	shared.Spec.Target.SharedRealm = true
	peer := realmSchemaFixture(shared.Namespace, "orders", shared.Spec.Target.CoordinationKey, true)
	_, both := fakeMigrationReconciler(t, nil, shared, peer)
	verdict = censusOf(t, both, shared.Namespace, shared.Spec.Target)
	if verdict.Census.total() != 2 || verdict.conflict() {
		t.Fatalf("two claimants that both declared the realm shared conflicted: %+v", verdict)
	}
}

// One claimant that has not declared the realm shared blocks every claimant,
// itself included. That is what makes the declaration mutual without naming
// anybody: a resource cannot widen its own permission alone.
func TestRealmCensusRefusesAOneSidedDeclaration(t *testing.T) {
	t.Parallel()

	declared := migrationFixture()
	declared.Spec.Target.SharedRealm = true
	silent := realmSchemaFixture(declared.Namespace, "orders", declared.Spec.Target.CoordinationKey, false)
	_, api := fakeMigrationReconciler(t, nil, declared, silent)

	verdict := censusOf(t, api, declared.Namespace, declared.Spec.Target)
	if !verdict.conflict() {
		t.Fatalf("a one-sided declaration was accepted: %+v", verdict)
	}
	if verdict.Census.Undeclared != 1 {
		t.Fatalf("census.Undeclared = %d, want the one silent claimant", verdict.Census.Undeclared)
	}
}

// The attack issue #445 names, against a coordination key: somebody who can
// create a PtahSchema in another namespace writes the key a tenant uses and
// leaves sharedRealm false. Before the key was scoped to its namespace, that
// refused every resource in the tenant's namespace and told it only a count.
func TestAKeyWrittenInAnotherNamespaceContestsNothing(t *testing.T) {
	t.Parallel()

	tenant := migrationFixture()
	tenant.Status.ExecutionBinding = migrationExecutionBinding()
	intruder := realmSchemaFixture("team-b", "orders", tenant.Spec.Target.CoordinationKey, false)
	reconciler, api := fakeMigrationReconciler(t, nil, tenant, intruder, verificationPolicyConfigMap())

	for _, side := range []struct {
		name      string
		namespace string
		target    operatorv1alpha1.DatabaseTargetSpec
	}{
		{name: "tenant", namespace: tenant.Namespace, target: tenant.Spec.Target},
		{name: "intruder", namespace: intruder.Namespace, target: intruder.Spec.Target},
	} {
		verdict := censusOf(t, api, side.namespace, side.target)
		if verdict.Census.total() != 1 || verdict.conflict() {
			t.Fatalf("the %s's census = %+v, want it alone in its own realm", side.name, verdict)
		}
	}

	// And the reconcile does not stop at the census: the tenant claims its
	// first operation as it would with nobody else in the cluster.
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(tenant)); err != nil {
		t.Fatal(err)
	}
	persisted := readMigration(t, api, tenant)
	if persisted.Status.Phase == operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("a key written in another namespace blocked the tenant: %#v", persisted.Status.Conditions)
	}
	if persisted.Status.ActiveOperation == nil {
		t.Fatal("the tenant claimed nothing, so this does not show the census let it through")
	}
}

// The same attack against a PtahRealm: the intruder names the realm, and the
// realm does not list its namespace. The intruder is refused and the tenant is
// counted alone, so the refusal lands on the only resource that earned it.
func TestAnUnlistedNamespaceIsRefusedAndBlocksNobody(t *testing.T) {
	t.Parallel()

	// Named unlike the tenant, so a message that names the realm it was asked
	// about cannot pass for one that names the tenant.
	realm := realmFixture("shop-primary", operatorv1alpha1.RealmSharingExclusive, "team-a")
	tenant := realmBoundMigrationFixture(realm.Name, false)
	tenant.Status.ExecutionBinding = migrationExecutionBinding()
	intruder := realmBoundSchemaFixture("team-b", "intruder", realm.Name, false)
	reconciler, api := fakeMigrationReconciler(t, nil, realm, tenant, intruder, verificationPolicyConfigMap())

	refused := censusOf(t, api, intruder.Namespace, intruder.Spec.Target)
	refusal, isRefused := refused.refusal()
	if !isRefused || refusal.Reason != operatorv1alpha1.ReasonRealmNotAuthorized {
		t.Fatalf("the intruder's refusal = %+v, %t, want RealmNotAuthorized", refusal, isRefused)
	}
	if refused.Census.total() != 0 {
		t.Fatalf("a refused claimant was told how many others claim the realm: %+v", refused.Census)
	}
	// What the intruder reads names its own namespace and the realm it asked
	// for, and nothing about who else claims it.
	for _, leaked := range []string{tenant.Namespace, tenant.Name} {
		if strings.Contains(refusal.Message+refusal.Approval+refusal.Apply, leaked) {
			t.Fatalf("the refusal named %q: %+v", leaked, refusal)
		}
	}

	admitted := censusOf(t, api, tenant.Namespace, tenant.Spec.Target)
	if !admitted.Authorized || admitted.Census.total() != 1 || admitted.conflict() {
		t.Fatalf("the tenant's census = %+v, want it admitted and alone", admitted)
	}

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(tenant)); err != nil {
		t.Fatal(err)
	}
	persisted := readMigration(t, api, tenant)
	if persisted.Status.Phase == operatorv1alpha1.MigrationPhaseBlocked || persisted.Status.ActiveOperation == nil {
		t.Fatalf("an unlisted claimant kept the tenant from its first claim: phase %q, conditions %#v",
			persisted.Status.Phase, persisted.Status.Conditions)
	}
}

// The schema side of the refusal, through its own reconcile: Blocked, the
// approval fence closed, and no Job.
func TestSchemaRefusesARealmThatDoesNotAdmitItsNamespace(t *testing.T) {
	t.Parallel()

	realm := realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-b")
	schema := schemaFixture()
	schema.Spec.Target.CoordinationKey = ""
	schema.Spec.Target.RealmRef = &operatorv1alpha1.PtahRealmReference{Name: realm.Name}
	reconciler, api := fakeReconciler(t, nil, realm, schema)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(schema),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > realmRecheckCeiling {
		t.Fatalf("a refused realm claim was not re-examined within a minute: %#v", result)
	}
	persisted := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase != operatorv1alpha1.PhaseBlocked || persisted.Status.ActiveOperation != nil {
		t.Fatalf("phase = %q, active = %#v, want Blocked with no claim", persisted.Status.Phase, persisted.Status.ActiveOperation)
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionApprovalRequired, operatorv1alpha1.ConditionApplying, operatorv1alpha1.ConditionReady,
	} {
		condition := findCondition(persisted.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != string(operatorv1alpha1.ReasonRealmNotAuthorized) {
			t.Fatalf("%s = %#v, want False/RealmNotAuthorized", conditionType, condition)
		}
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a claim the realm refused created %d Jobs", len(jobs.Items))
	}
}

// A realm is an authorization, so what is absent is refused rather than
// assumed. Each of these reads as one refusal, with one message, so a tenant
// cannot tell a realm it may not use from a realm that is not there.
func TestARealmAdmitsOnlyTheNamespacesAndTheEngineItNames(t *testing.T) {
	t.Parallel()

	mysql := realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-a")
	mysql.Spec.Engine = operatorv1alpha1.DatabaseEngineMySQL
	for _, refusal := range []struct {
		name  string
		realm *operatorv1alpha1.PtahRealm
	}{
		{name: "no such realm"},
		{name: "another namespace listed", realm: realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-b")},
		{name: "another engine named", realm: mysql},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()

			migration := realmBoundMigrationFixture("orders-primary", false)
			objects := []client.Object{migration}
			if refusal.realm != nil {
				objects = append(objects, refusal.realm)
			}
			_, api := fakeMigrationReconciler(t, nil, objects...)
			verdict := censusOf(t, api, migration.Namespace, migration.Spec.Target)
			got, refused := verdict.refusal()
			if !refused || got.Reason != operatorv1alpha1.ReasonRealmNotAuthorized {
				t.Fatalf("refusal = %+v, %t, want RealmNotAuthorized", got, refused)
			}
			if !strings.Contains(got.Message, `"orders-primary"`) || !strings.Contains(got.Message, `"team-a"`) {
				t.Fatalf("the refusal does not name the realm asked for and the namespace asking: %s", got.Message)
			}
		})
	}

	// And the one that does admit it, spelled the way the digest spells it:
	// "postgres" in the resource and "PostgreSQL" in the realm are one engine.
	admits := realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-a")
	migration := realmBoundMigrationFixture(admits.Name, false)
	migration.Spec.Target.Engine = operatorv1alpha1.DatabaseEngine("postgres")
	_, api := fakeMigrationReconciler(t, nil, admits, migration)
	if verdict := censusOf(t, api, migration.Namespace, migration.Spec.Target); !verdict.Authorized {
		t.Fatalf("a realm refused the namespace and engine it names: %+v", verdict)
	}
}

// Among the namespaces a realm lists, the rules a namespace key has still
// hold, and the realm's own sharing adds one: Exclusive refuses a second
// claimant whatever each declared.
func TestListedNamespacesShareARealmOnTheRulesItStates(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		sharing  operatorv1alpha1.RealmSharing
		declared bool
		conflict bool
	}{
		{name: "shared realm, both declared", sharing: operatorv1alpha1.RealmSharingShared, declared: true, conflict: false},
		{name: "shared realm, neither declared", sharing: operatorv1alpha1.RealmSharingShared, declared: false, conflict: true},
		{name: "exclusive realm, both declared", sharing: operatorv1alpha1.RealmSharingExclusive, declared: true, conflict: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			realm := realmFixture("orders-primary", row.sharing, "team-a", "team-b")
			migration := realmBoundMigrationFixture(realm.Name, row.declared)
			peer := realmBoundSchemaFixture("team-b", "orders", realm.Name, row.declared)
			_, api := fakeMigrationReconciler(t, nil, realm, migration, peer)

			for _, side := range []struct {
				namespace string
				target    operatorv1alpha1.DatabaseTargetSpec
			}{
				{namespace: migration.Namespace, target: migration.Spec.Target},
				{namespace: peer.Namespace, target: peer.Spec.Target},
			} {
				verdict := censusOf(t, api, side.namespace, side.target)
				if verdict.Census.total() != 2 {
					t.Fatalf("census from %s = %+v, want both listed claimants", side.namespace, verdict.Census)
				}
				if verdict.conflict() != row.conflict {
					t.Fatalf("conflict from %s = %t, want %t: %+v", side.namespace, verdict.conflict(), row.conflict, verdict)
				}
				refusal, refused := verdict.refusal()
				if refused != row.conflict {
					t.Fatalf("refused from %s = %t, want %t", side.namespace, refused, row.conflict)
				}
				if refused && refusal.Reason != operatorv1alpha1.ReasonRealmConflict {
					t.Fatalf("refusal from %s = %+v, want RealmConflict", side.namespace, refusal)
				}
				// Either side reads counts, never the other's namespace.
				other := "team-b"
				if side.namespace == "team-b" {
					other = "team-a"
				}
				if strings.Contains(refusal.Message, other) {
					t.Fatalf("the refusal in %s named %s: %s", side.namespace, other, refusal.Message)
				}
			}
		})
	}
}

// Withdrawing a namespace from a realm refuses its next claim. Nothing already
// running is abandoned, because the census runs before a claim and not during
// one; what the withdrawal decides is everything after.
func TestWithdrawingANamespaceRefusesItsNextClaim(t *testing.T) {
	t.Parallel()

	realm := realmFixture("orders-primary", operatorv1alpha1.RealmSharingExclusive, "team-a")
	migration := realmBoundMigrationFixture(realm.Name, false)
	_, api := fakeMigrationReconciler(t, nil, realm, migration)
	if verdict := censusOf(t, api, migration.Namespace, migration.Spec.Target); !verdict.Authorized {
		t.Fatalf("the listed namespace was refused: %+v", verdict)
	}

	stored := &operatorv1alpha1.PtahRealm{}
	if err := api.Get(context.Background(), client.ObjectKey{Name: realm.Name}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Namespaces = []string{"team-b"}
	if err := api.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if verdict := censusOf(t, api, migration.Namespace, migration.Spec.Target); verdict.Authorized {
		t.Fatalf("a namespace the realm no longer lists was admitted: %+v", verdict)
	}
}

func TestMigrationBlocksOnAContestedRealmWithoutDispatchingAJob(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	peer := realmSchemaFixture(migration.Namespace, "orders", migration.Spec.Target.CoordinationKey, false)
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
			Namespace: schema.Namespace, Name: "orders", UID: types.UID("migration-peer"), Generation: 1,
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

// A controller watches its own resource, so a status write it makes wakes it
// again. A standing refusal that restamped its next-reconciliation time every
// pass would patch every pass and wake itself every pass.
func TestAStandingRealmRefusalStopsWritingStatus(t *testing.T) {
	t.Parallel()

	for _, standing := range []struct {
		name    string
		objects func() (*operatorv1alpha1.PtahSchema, []client.Object)
	}{
		{
			name: "a contested key",
			objects: func() (*operatorv1alpha1.PtahSchema, []client.Object) {
				schema := schemaFixture()
				peer := schemaFixture()
				peer.Name = "app-second"
				peer.UID = types.UID("schema-second")
				return schema, []client.Object{schema, peer}
			},
		},
		{
			name: "a realm that does not admit the namespace",
			objects: func() (*operatorv1alpha1.PtahSchema, []client.Object) {
				schema := schemaFixture()
				schema.Spec.Target.CoordinationKey = ""
				schema.Spec.Target.RealmRef = &operatorv1alpha1.PtahRealmReference{Name: "orders-primary"}
				return schema, []client.Object{schema}
			},
		},
	} {
		t.Run(standing.name, func(t *testing.T) {
			t.Parallel()

			schema, objects := standing.objects()
			reconciler, api := fakeReconciler(t, nil, objects...)

			key := client.ObjectKeyFromObject(schema)
			request := ctrl.Request{NamespacedName: key}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			blocked := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), key, blocked); err != nil {
				t.Fatal(err)
			}
			if blocked.Status.Phase != operatorv1alpha1.PhaseBlocked {
				t.Fatalf("phase = %q, want Blocked", blocked.Status.Phase)
			}
			settled := blocked.ResourceVersion

			for pass := range 3 {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				again := &operatorv1alpha1.PtahSchema{}
				if err := api.Get(context.Background(), key, again); err != nil {
					t.Fatal(err)
				}
				if again.ResourceVersion != settled {
					t.Fatalf("pass %d wrote status again: resourceVersion %s -> %s",
						pass, settled, again.ResourceVersion)
				}
			}
		})
	}
}

// Suspending one of two claimants is the way to hand a database to the other
// without declaring anything shared: it runs nothing, so it claims nothing.
func TestSuspendingAClaimantEndsTheConflict(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	peer := realmSchemaFixture(migration.Namespace, "orders", migration.Spec.Target.CoordinationKey, false)
	reconciler, api := fakeMigrationReconciler(t, nil, migration, peer, verificationPolicyConfigMap())

	key := client.ObjectKeyFromObject(migration)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	persisted := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), key, persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", persisted.Status.Phase)
	}

	if err := api.Get(context.Background(), client.ObjectKeyFromObject(peer), peer); err != nil {
		t.Fatal(err)
	}
	peer.Spec.Suspend = true
	if err := api.Update(context.Background(), peer); err != nil {
		t.Fatal(err)
	}
	verdict := censusOf(t, api, migration.Namespace, migration.Spec.Target)
	if verdict.Census.total() != 1 || verdict.conflict() {
		t.Fatalf("a suspended claimant still contested the realm: %+v", verdict)
	}
}

// withRealmIndexes gives a fake client the indexes RegisterRealmIndexes gives
// a manager, derived through the same function. A fake client without them
// fails the census with "no index with name", which is a fixture complaining
// about itself rather than anything the controller did.
// withEventIndex gives the fake client the field selector the API server
// serves on Events, which reportPodAdmission reads a Job's FailedCreate
// Events through.
func withEventIndex(builder *fake.ClientBuilder) *fake.ClientBuilder {
	return builder.WithIndex(&corev1.Event{}, eventInvolvedObjectUIDField, func(object client.Object) []string {
		event, ok := object.(*corev1.Event)
		if !ok || event.InvolvedObject.UID == "" {
			return nil
		}
		return []string{string(event.InvolvedObject.UID)}
	})
}

func withRealmIndexes(builder *fake.ClientBuilder) *fake.ClientBuilder {
	return builder.
		WithIndex(&operatorv1alpha1.PtahSchema{}, RealmDigestIndex, func(object client.Object) []string {
			schema, ok := object.(*operatorv1alpha1.PtahSchema)
			if !ok {
				return nil
			}
			return realmDigestIndexValue(schema.Namespace, schema.Spec.Target)
		}).
		WithIndex(&operatorv1alpha1.PtahMigration{}, RealmDigestIndex, func(object client.Object) []string {
			migration, ok := object.(*operatorv1alpha1.PtahMigration)
			if !ok {
				return nil
			}
			return realmDigestIndexValue(migration.Namespace, migration.Spec.Target)
		})
}

// The index is what membership is now read through, so what it derives is what
// the census can see. These are the ways a resource lands in the wrong realm,
// or in none, without any of the counting above noticing.
func TestTheRealmIndexNamesTheSameRealmTheCensusAsksFor(t *testing.T) {
	t.Parallel()

	postgres := operatorv1alpha1.DatabaseTargetSpec{
		Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "team-a/orders",
	}
	// The digest canonicalizes the engine, so a spelling the API accepts is
	// the same realm rather than a neighbouring one. Two resources that reach
	// one database have to meet in the census whichever way each spelled it.
	lowercase := operatorv1alpha1.DatabaseTargetSpec{
		Engine: operatorv1alpha1.DatabaseEngine("postgresql"), CoordinationKey: "team-a/orders",
	}
	if got, want := realmDigestIndexValue("team-a", lowercase), realmDigestIndexValue("team-a", postgres); len(want) != 1 ||
		len(got) != 1 || got[0] != want[0] {
		t.Fatalf("engine spellings indexed to %v and %v, so one database is two realms", got, want)
	}

	// A different engine on the same key is a different database.
	mysql := operatorv1alpha1.DatabaseTargetSpec{
		Engine: operatorv1alpha1.DatabaseEngineMySQL, CoordinationKey: "team-a/orders",
	}
	if realmDigestIndexValue("team-a", mysql)[0] == realmDigestIndexValue("team-a", postgres)[0] {
		t.Fatal("two engines on one coordination key indexed to one realm")
	}

	// The same key in another namespace is another realm.
	if realmDigestIndexValue("team-b", postgres)[0] == realmDigestIndexValue("team-a", postgres)[0] {
		t.Fatal("one coordination key in two namespaces indexed to one realm")
	}

	// A PtahRealm is one realm from every namespace, and no key's.
	byRealm := operatorv1alpha1.DatabaseTargetSpec{
		Engine:   operatorv1alpha1.DatabaseEnginePostgreSQL,
		RealmRef: &operatorv1alpha1.PtahRealmReference{Name: "orders"},
	}
	fromA, fromB := realmDigestIndexValue("team-a", byRealm), realmDigestIndexValue("team-b", byRealm)
	if len(fromA) != 1 || len(fromB) != 1 || fromA[0] != fromB[0] {
		t.Fatalf("one PtahRealm indexed to %v and %v from two namespaces", fromA, fromB)
	}
	orders := operatorv1alpha1.DatabaseTargetSpec{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "orders"}
	if realmDigestIndexValue("team-a", orders)[0] == fromA[0] {
		t.Fatal("a key named like a realm indexed to the realm")
	}

	// And a target the API would refuse indexes nothing rather than everything.
	// Indexing it under the empty string would put every unindexable resource
	// in one realm together, which is a conflict nobody declared.
	for _, refused := range []operatorv1alpha1.DatabaseTargetSpec{
		{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "Team-A/Orders"},
		{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: ""},
		{Engine: operatorv1alpha1.DatabaseEngine("cassandra"), CoordinationKey: "team-a/orders"},
		{
			Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "team-a/orders",
			RealmRef: &operatorv1alpha1.PtahRealmReference{Name: "orders"},
		},
	} {
		if values := realmDigestIndexValue("team-a", refused); len(values) != 0 {
			t.Fatalf("a target the API refuses indexed to %v", values)
		}
	}
}

// A resource that moves to another realm leaves the one it was in. The index
// is recomputed on update, so this is really a check that the census reads the
// index rather than a remembered answer.
func TestAClaimantThatChangesItsRealmLeavesTheOldOne(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	rival := realmSchemaFixture(migration.Namespace, "orders", migration.Spec.Target.CoordinationKey, false)
	_, api := fakeMigrationReconciler(t, nil, migration, rival)

	if contested := censusOf(t, api, migration.Namespace, migration.Spec.Target); !contested.conflict() {
		t.Fatalf("census = %+v, want the rival claiming the same realm", contested)
	}

	moved := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(),
		types.NamespacedName{Namespace: rival.Namespace, Name: rival.Name}, moved); err != nil {
		t.Fatal(err)
	}
	moved.Spec.Target.CoordinationKey = "team-a/somewhere-else"
	if err := api.Update(context.Background(), moved); err != nil {
		t.Fatal(err)
	}

	alone := censusOf(t, api, migration.Namespace, migration.Spec.Target)
	if alone.Census.Schemas != 0 || alone.Census.total() != 1 || alone.conflict() {
		t.Fatalf("census = %+v, want the migration alone after the rival moved", alone)
	}
}

// Dropping the index from the census changes no answer -- listing everything
// and filtering it gives the same counts, more slowly -- so nothing above
// fails if it goes. This is what fails: a reader with no realm index cannot
// serve the census at all, which is only true while the census asks for one.
func TestTheCensusReadsThroughTheIndex(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	migration := migrationFixture()
	unindexed := fake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).Build()

	_, err := takeRealmCensus(context.Background(), unindexed, migration.Namespace, migration.Spec.Target)
	if err == nil {
		t.Fatal("the census answered without the realm index, so it is scanning again")
	}
	if !strings.Contains(err.Error(), RealmDigestIndex) {
		t.Fatalf("error = %v, want it to name the missing %s index", err, RealmDigestIndex)
	}
}

// A change to a realm wakes the resources that name it, and only those: the
// grant is another object's event, and a claimant that waited out its own
// interval for it would stay refused, or stay admitted, long after it changed.
func TestARealmChangeWakesTheResourcesThatNameIt(t *testing.T) {
	t.Parallel()

	realm := realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-a")
	named := realmBoundSchemaFixture("team-a", "orders", realm.Name, false)
	unlisted := realmBoundSchemaFixture("team-b", "orders", realm.Name, false)
	elsewhere := realmBoundSchemaFixture("team-a", "billing", "billing-primary", false)
	keyed := realmSchemaFixture("team-a", "catalog", realm.Name, false)
	_, api := fakeReconciler(t, nil, realm, named, unlisted, elsewhere, keyed)

	requests := realmClaimantRequests(context.Background(), api,
		func() client.ObjectList { return &operatorv1alpha1.PtahSchemaList{} }, realm)
	var woken []string
	for _, request := range requests {
		woken = append(woken, request.String())
	}
	slices.Sort(woken)
	// The unlisted one is woken too: a grant that starts listing its namespace
	// is exactly the change it is waiting for.
	want := []string{"team-a/orders", "team-b/orders"}
	if !slices.Equal(woken, want) {
		t.Fatalf("a change to %s woke %v, want %v", realm.Name, woken, want)
	}

	if got := realmClaimantRequests(context.Background(), api,
		func() client.ObjectList { return &operatorv1alpha1.PtahSchemaList{} }, named); got != nil {
		t.Fatalf("an object that is not a PtahRealm woke %v", got)
	}
}

// Both alert fixtures need a real next deadline, then a new Resolve claim
// after the missed deadline. Migration's failed-operation backoff has no such
// nextReconciliationTime, so use the realm recheck both controllers persist.
func TestRealmRecheckDeadlineClearsOnFreshResolveAfterGrant(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			var object client.Object
			var api client.Client
			var reconcile func() error
			if family == "schema" {
				s := schemaFixture()
				s.Spec.Target.CoordinationKey = ""
				s.Spec.Target.RealmRef = &operatorv1alpha1.PtahRealmReference{Name: "overdue-realm"}
				s.Spec.Interval.Duration = time.Minute
				r, c := fakeReconciler(t, nil, s)
				r.Clock = func() time.Time { return now }
				object, api = s, c
				reconcile = func() error {
					_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)})
					return err
				}
			} else {
				m := migrationFixture()
				m.Spec.Target.CoordinationKey = ""
				m.Spec.Target.RealmRef = &operatorv1alpha1.PtahRealmReference{Name: "overdue-realm"}
				m.Spec.Interval.Duration = time.Minute
				r, c := fakeMigrationReconciler(t, nil, m)
				r.Clock = func() time.Time { return now }
				object, api = m, c
				reconcile = func() error { _, err := r.Reconcile(context.Background(), migrationRequest(m)); return err }
			}
			read := func() (string, *metav1.Time, *metav1.Time) {
				t.Helper()
				if err := api.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal(err)
				}
				switch r := object.(type) {
				case *operatorv1alpha1.PtahSchema:
					if r.Status.ActiveOperation != nil {
						return string(r.Status.ActiveOperation.Type), r.Status.NextReconciliationTime, &r.Status.ActiveOperation.StartedAt
					}
					return string(r.Status.Phase), r.Status.NextReconciliationTime, nil
				case *operatorv1alpha1.PtahMigration:
					if r.Status.ActiveOperation != nil {
						return string(r.Status.ActiveOperation.Type), r.Status.NextReconciliationTime, &r.Status.ActiveOperation.StartedAt
					}
					return string(r.Status.Phase), r.Status.NextReconciliationTime, nil
				}
				t.Fatal("unknown family")
				return "", nil, nil
			}
			var deadline *metav1.Time
			for range 10 {
				if err := reconcile(); err != nil {
					t.Fatal(err)
				}
				phase, next, active := read()
				if phase == "Blocked" && active == nil {
					deadline = next
					break
				}
			}
			if deadline == nil || !deadline.Time.Equal(now.Add(time.Minute)) {
				t.Fatalf("no native one-minute realm recheck deadline: %v", deadline)
			}
			now = deadline.Add(2 * time.Minute)
			realm := realmFixture("overdue-realm", operatorv1alpha1.RealmSharingExclusive, object.GetNamespace())
			if err := api.Create(context.Background(), realm); err != nil {
				t.Fatal(err)
			}
			for range 10 {
				if err := reconcile(); err != nil {
					t.Fatal(err)
				}
				operation, next, started := read()
				if operation == "Resolve" {
					if next != nil || started == nil || !started.Time.Equal(now) {
						t.Fatal("recovery did not clear the deadline at the native claim time")
					}
					return
				}
			}
			t.Fatal("grant did not produce a new Resolve claim")
		})
	}
}
