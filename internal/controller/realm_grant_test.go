package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// censusReads records every list the census makes: the namespace it named and
// how many objects came back.
type censusReads struct {
	mu    sync.Mutex
	lists []string
	read  int
}

func (r *censusReads) record(namespace string, items int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lists = append(r.lists, namespace)
	r.read += items
}

// recordingRealmClient is a fake client with the realm indexes, whose lists
// are recorded.
func recordingRealmClient(t testing.TB, objects ...client.Object) (client.Client, *censusReads) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reads := &censusReads{}
	api := withRealmIndexes(fake.NewClientBuilder().WithScheme(scheme)).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
				if err := delegate.List(ctx, list, options...); err != nil {
					return err
				}
				listOptions := client.ListOptions{}
				listOptions.ApplyOptions(options)
				reads.record(listOptions.Namespace, meta.LenList(list))
				return nil
			},
		}).
		Build()
	return api, reads
}

// A realm's index bucket holds every resource in the cluster that names it,
// admitted or not, and the census used to list the whole bucket and drop the
// unlisted ones afterwards: each pass of each admitted claimant copied every
// unlisted claimant out of the cache. Unlisted claimants are what anybody can
// create, so the cost of a census was theirs to set. It now reads the
// namespaces the realm grants, and nothing else.
func TestTheCensusReadsOnlyTheNamespacesTheRealmGrants(t *testing.T) {
	t.Parallel()

	realm := realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-a", "team-c")
	tenant := realmBoundMigrationFixture(realm.Name, true)
	objects := []client.Object{realm, tenant}
	for index := range 20 {
		objects = append(objects, realmBoundSchemaFixture(fmt.Sprintf("unlisted-%d", index), "intruder", realm.Name, false))
	}
	api, reads := recordingRealmClient(t, objects...)

	verdict := censusOf(t, api, tenant.Namespace, tenant.Spec.Target)
	if !verdict.Authorized || verdict.Census.total() != 1 || verdict.conflict() {
		t.Fatalf("the tenant's census = %+v, want it admitted and alone", verdict)
	}
	if len(reads.lists) == 0 {
		t.Fatal("the census listed nothing, so this reads no census")
	}
	for _, namespace := range reads.lists {
		if !slices.Contains(realm.Spec.Namespaces, namespace) {
			t.Fatalf("the census listed namespace %q, which the realm does not grant (lists: %q)", namespace, reads.lists)
		}
	}
	if reads.read != 1 {
		t.Fatalf("the census read %d objects for one admitted claimant among %d unlisted ones", reads.read, len(objects)-2)
	}
}

// A coordination key names a realm in its own namespace, and its census reads
// that namespace alone.
func TestAKeyCensusReadsOnlyItsOwnNamespace(t *testing.T) {
	t.Parallel()

	tenant := migrationFixture()
	elsewhere := realmSchemaFixture("team-b", "orders", tenant.Spec.Target.CoordinationKey, false)
	api, reads := recordingRealmClient(t, tenant, elsewhere)

	verdict := censusOf(t, api, tenant.Namespace, tenant.Spec.Target)
	if verdict.Census.total() != 1 {
		t.Fatalf("the census = %+v, want the tenant alone", verdict)
	}
	if len(reads.lists) == 0 {
		t.Fatal("the census listed nothing, so this reads no census")
	}
	for _, namespace := range reads.lists {
		if namespace != tenant.Namespace {
			t.Fatalf("a key's census listed namespace %q (lists: %q)", namespace, reads.lists)
		}
	}
}

// The migration side of the refusal, through its own reconcile: Blocked with
// the reason on every condition the refusal writes, nothing approvable, no
// claim, no Job, and another look within the re-check ceiling.
func TestMigrationRefusesARealmThatDoesNotAdmitItsNamespace(t *testing.T) {
	t.Parallel()

	realm := realmFixture("orders-primary", operatorv1alpha1.RealmSharingShared, "team-b")
	migration := realmBoundMigrationFixture(realm.Name, false)
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, nil, realm, migration, verificationPolicyConfigMap())

	result, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > realmRecheckCeiling {
		t.Fatalf("a refused realm claim was not re-examined within a minute: %#v", result)
	}
	persisted := readMigration(t, api, migration)
	if persisted.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || persisted.Status.ActiveOperation != nil {
		t.Fatalf("phase = %q, active = %#v, want Blocked with no claim",
			persisted.Status.Phase, persisted.Status.ActiveOperation)
	}
	for _, expected := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue},
		{operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse},
		{operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse},
		{operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse},
	} {
		condition := findCondition(persisted.Status.Conditions, expected.conditionType)
		if condition == nil || condition.Status != expected.status ||
			condition.Reason != string(operatorv1alpha1.ReasonRealmNotAuthorized) {
			t.Fatalf("%s = %#v, want %s/RealmNotAuthorized", expected.conditionType, condition, expected.status)
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

// A standing refusal re-examines its realm every pass and writes its status
// once. Status writes wake the controller that makes them, so one that
// rewrote the refusal every pass would never sleep.
func TestAStandingMigrationRealmRefusalStopsWritingStatus(t *testing.T) {
	t.Parallel()

	for _, standing := range []struct {
		name    string
		reason  operatorv1alpha1.ConditionReason
		objects func() (*operatorv1alpha1.PtahMigration, []client.Object)
	}{
		{
			name:   "a contested key",
			reason: operatorv1alpha1.ReasonRealmConflict,
			objects: func() (*operatorv1alpha1.PtahMigration, []client.Object) {
				migration := migrationFixture()
				migration.Status.ExecutionBinding = migrationExecutionBinding()
				peer := realmSchemaFixture(migration.Namespace, "orders", migration.Spec.Target.CoordinationKey, false)
				return migration, []client.Object{migration, peer, verificationPolicyConfigMap()}
			},
		},
		{
			name:   "a realm that does not admit the namespace",
			reason: operatorv1alpha1.ReasonRealmNotAuthorized,
			objects: func() (*operatorv1alpha1.PtahMigration, []client.Object) {
				migration := realmBoundMigrationFixture("orders-primary", false)
				migration.Status.ExecutionBinding = migrationExecutionBinding()
				return migration, []client.Object{migration, verificationPolicyConfigMap()}
			},
		},
	} {
		t.Run(standing.name, func(t *testing.T) {
			t.Parallel()

			migration, objects := standing.objects()
			reconciler, api := fakeMigrationReconciler(t, nil, objects...)
			request := migrationRequest(migration)
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			blocked := readMigration(t, api, migration)
			assertMigrationBlockedFor(t, blocked, standing.reason)
			settled := blocked.ResourceVersion

			for pass := range 3 {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				again := readMigration(t, api, migration)
				if again.ResourceVersion != settled {
					t.Fatalf("pass %d wrote status again: resourceVersion %s -> %s",
						pass, settled, again.ResourceVersion)
				}
			}
		})
	}
}

// A new grant lifts a standing refusal no sooner than the refusal's own
// deadline, and no later. The pass the realm watch starts finds the census
// passing and the resource Blocked and not yet due, so it waits; the first
// pass at the deadline claims. This holds the documented bound to the code in
// both directions: a pass before the deadline that claimed would be a fast
// path nobody wrote down, and one at the deadline that did not would be a
// refusal outliving its grant.
func TestAGrantLiftsARefusalAtItsDeadline(t *testing.T) {
	t.Parallel()

	migration := realmBoundMigrationFixture("orders-primary", false)
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, nil, migration, verificationPolicyConfigMap())
	clock := movableMigrationClock(reconciler, api)
	request := migrationRequest(migration)
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	refused := readMigration(t, api, migration)
	assertMigrationBlockedFor(t, refused, operatorv1alpha1.ReasonRealmNotAuthorized)
	deadline := refused.Status.NextReconciliationTime
	if deadline == nil {
		t.Fatal("the refusal left no deadline to wait for")
	}

	if err := api.Create(context.Background(),
		realmFixture("orders-primary", operatorv1alpha1.RealmSharingExclusive, migration.Namespace)); err != nil {
		t.Fatal(err)
	}
	// The pass the watch starts, before the deadline.
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	waiting := readMigration(t, api, migration)
	if waiting.Status.ActiveOperation != nil {
		t.Fatal("the grant was acted on before the refusal's deadline, which nothing documents")
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > realmRecheckCeiling {
		t.Fatalf("the admitted resource did not come back within the re-check ceiling: %#v", result)
	}

	// The first pass at the deadline.
	clock.now = deadline.Time
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	claimed := readMigration(t, api, migration)
	if claimed.Status.ActiveOperation == nil ||
		claimed.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("the refusal outlived its grant: phase %q, active %#v", claimed.Status.Phase, claimed.Status.ActiveOperation)
	}
}
