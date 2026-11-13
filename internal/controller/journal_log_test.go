package controller

import (
	"context"
	"reflect"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// loggedEntry is one entry a JournalLog was handed, with the resource it named.
type loggedEntry struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
	entry     operatorv1alpha1.JournalEntry
}

// journalLines is a JournalLog that keeps what it was handed, in order.
type journalLines struct {
	mu      sync.Mutex
	entries []loggedEntry
}

func (l *journalLines) Entries(kind string, object metav1.Object, entries []operatorv1alpha1.JournalEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range entries {
		l.entries = append(l.entries, loggedEntry{
			kind: kind, namespace: object.GetNamespace(), name: object.GetName(), uid: object.GetUID(), entry: *entry.DeepCopy(),
		})
	}
}

func (l *journalLines) logged() []loggedEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]loggedEntry(nil), l.entries...)
}

// assertLoggedOnce holds the log to the entries the stored journal gained
// since it held from entries: each one logged once, in order, as stored, under
// the resource that stores it, and nothing else.
func assertLoggedOnce(t *testing.T, lines *journalLines, kind string, stored client.Object, journal *operatorv1alpha1.LifecycleJournal, from int) {
	t.Helper()
	if journal == nil || len(journal.Entries) <= from {
		t.Fatalf("the stored journal %#v gained no entry; the row needs one", journal)
	}
	gained := journal.Entries[from:]
	logged := lines.logged()
	if len(logged) != len(gained) {
		t.Fatalf("logged %d entries for the %d the journal gained:\nlogged %#v\nstored %#v", len(logged), len(gained), logged, gained)
	}
	for index, line := range logged {
		if line.kind != kind || line.namespace != stored.GetNamespace() || line.name != stored.GetName() || line.uid != stored.GetUID() {
			t.Fatalf("line %d names %s %s/%s UID %s, want %s %s/%s UID %s", index, line.kind, line.namespace, line.name, line.uid,
				kind, stored.GetNamespace(), stored.GetName(), stored.GetUID())
		}
		if !reflect.DeepEqual(line.entry, gained[index]) {
			t.Fatalf("line %d holds %#v, want the stored entry %#v", index, line.entry, gained[index])
		}
	}
}

func journalLength(journal *operatorv1alpha1.LifecycleJournal) int {
	if journal == nil {
		return 0
	}
	return len(journal.Entries)
}

// A schema that publishes a plan, reads the database twice and publishes the
// same plan and then a replacement stores two entries across seven status
// writes. The log holds those two, each once, as the API server stored them.
func TestEachSchemaEntryIsLoggedOnceAsStored(t *testing.T) {
	t.Parallel()

	walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
	lines := &journalLines{}
	walk.reconciler.JournalLog = lines
	from := journalLength(walk.stored().Status.Journal)

	first := walk.waitingPlan("plan-1", "observed-state")
	walk.read("observe-2")
	if same := walk.waitingPlan("plan-2", "observed-state"); same != first {
		t.Fatalf("the unchanged plan came back as %s, not %s; this row needs the same plan", same, first)
	}
	walk.read("observe-3")
	if replaced := walk.waitingPlan("plan-3", "moved-state"); replaced == first {
		t.Fatalf("a plan from another observed state kept the name %s; this row needs a replacement", first)
	}
	stored := walk.stored()
	if gained := journalLength(stored.Status.Journal) - from; gained != 2 {
		t.Fatalf("the walk stored %d entries, want the two publications: %#v", gained, stored.Status.Journal)
	}
	assertLoggedOnce(t, lines, "PtahSchema", stored, stored.Status.Journal, from)

	// A write that stores no entry logs none.
	updated := stored.DeepCopy()
	updated.Status.LastAttemptTime = &metav1.Time{Time: metav1.Now().Time}
	if err := walk.reconciler.patchStatus(context.Background(), stored, updated); err != nil {
		t.Fatal(err)
	}
	assertLoggedOnce(t, lines, "PtahSchema", stored, stored.Status.Journal, from)
}

// The Apply that ends stores its ApplyFinished in the write that retires its
// claim, and that write logs it.
func TestTheApplyThatEndsLogsItsEntry(t *testing.T) {
	t.Parallel()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply,
		OperationID: schema.Status.ActiveOperation.ID, ChildExitCode: 0, MutationStarted: true,
		CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	})
	from := journalLength(schema.Status.Journal)
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	lines := &journalLines{}
	reconciler.JournalLog = lines

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stored := safetyGetSchema(t, api, schema)
	assertLoggedOnce(t, lines, "PtahSchema", stored, stored.Status.Journal, from)
	if newest := stored.Status.Journal.Entries[len(stored.Status.Journal.Entries)-1]; newest.Type != operatorv1alpha1.JournalApplyFinished ||
		newest.Outcome != operatorv1alpha1.JournalSucceeded {
		t.Fatalf("the newest entry is %#v, want the Apply's end", newest)
	}
}

// conflictingStatusWrites refuses the first status patch as a conflict, as the
// API server does when another write landed first, and lets the rest through.
func conflictingStatusWrites(api client.WithWatch, resource string) client.WithWatch {
	var once sync.Once
	return interceptor.NewClient(api, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, inner client.Client, subResource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption,
		) error {
			var conflict error
			once.Do(func() {
				conflict = apierrors.NewConflict(schema.GroupResource{Group: operatorv1alpha1.GroupVersion.Group, Resource: resource},
					object.GetName(), nil)
			})
			if conflict != nil {
				return conflict
			}
			return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
	})
}

// schemaPublishing is the status write that publishes a plan on schema as
// stored: a transition that records one PlanPublished.
func schemaPublishing(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) (before, after *operatorv1alpha1.PtahSchema) {
	t.Helper()
	before = safetyGetSchema(t, api, schema)
	after = before.DeepCopy()
	after.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: "ptah-plan-a", UID: "plan-uid", CreatedAt: metav1.Now(), StatementCount: 2}
	return before, after
}

// A status write that conflicted stored nothing and logs nothing. The pass
// after it reads what is stored and makes the entry again, and its write logs
// it once.
func TestAConflictedSchemaStatusWriteLogsNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api := fakeReconciler(t, staticLogs{}, schemaFixture())
	reconciler.Client = conflictingStatusWrites(api.(client.WithWatch), "ptahschemas")
	lines := &journalLines{}
	reconciler.JournalLog = lines

	before, after := schemaPublishing(t, api, schemaFixture())
	if err := reconciler.patchStatus(ctx, before, after); !apierrors.IsConflict(err) {
		t.Fatalf("patchStatus() = %v, want the conflict", err)
	}
	if logged := lines.logged(); len(logged) != 0 {
		t.Fatalf("a conflicted write logged %#v", logged)
	}
	if stored := safetyGetSchema(t, api, schemaFixture()); stored.Status.Journal != nil {
		t.Fatalf("a conflicted write stored %#v", stored.Status.Journal)
	}

	before, after = schemaPublishing(t, api, schemaFixture())
	if err := reconciler.patchStatus(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	stored := safetyGetSchema(t, api, schemaFixture())
	assertLoggedOnce(t, lines, "PtahSchema", stored, stored.Status.Journal, 0)
}

// A line says what the API server kept, which is what it answered the write
// with: a field the CRD does not declare is pruned there. The row has the
// answer differ from what was sent, and the line follows the answer.
func TestALineHoldsWhatTheAPIServerAnswered(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api := fakeReconciler(t, staticLogs{}, schemaFixture())
	reconciler.Client = interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, inner client.Client, subResource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption,
		) error {
			if err := inner.SubResource(subResource).Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			answered := object.(*operatorv1alpha1.PtahSchema)
			answered.Status.Journal.Entries[len(answered.Status.Journal.Entries)-1].Statements = 0
			return nil
		},
	})
	lines := &journalLines{}
	reconciler.JournalLog = lines

	before, after := schemaPublishing(t, api, schemaFixture())
	if err := reconciler.patchStatus(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	logged := lines.logged()
	if len(logged) != 1 || logged[0].entry.Type != operatorv1alpha1.JournalPlanPublished || logged[0].entry.Statements != 0 {
		t.Fatalf("logged %#v, want the PlanPublished the answer held, with no statement count", logged)
	}
	if !reflect.DeepEqual(logged[0].entry, after.Status.Journal.Entries[0]) {
		t.Fatalf("logged %#v, want the entry the write's answer holds: %#v", logged[0].entry, after.Status.Journal.Entries[0])
	}
}

// A failure repeated in a row is one entry whose count moves. Each write that
// moves it logs the entry as it then stands.
func TestAFoldedSchemaFailureLogsEachCount(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api := fakeReconciler(t, staticLogs{}, schemaFixture())
	lines := &journalLines{}
	reconciler.JournalLog = lines

	for attempt := int32(1); attempt <= 3; attempt++ {
		before := safetyGetSchema(t, api, schemaFixture())
		after := before.DeepCopy()
		after.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationObserve, ID: "observe-operation", StartedAt: metav1.Now(), Attempt: attempt,
		}
		setCondition(after, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, "ObservationFailed", "the observation failed")
		if err := reconciler.patchStatus(ctx, before, after); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		stored := safetyGetSchema(t, api, schemaFixture())
		if count := journalLength(stored.Status.Journal); count != 1 {
			t.Fatalf("attempt %d: the journal holds %d entries, want the one failure: %#v", attempt, count, stored.Status.Journal)
		}
		newest := stored.Status.Journal.Entries[0]
		if attempt > 1 && newest.Count != attempt {
			t.Fatalf("attempt %d: the failure counts %d", attempt, newest.Count)
		}
		logged := lines.logged()
		if len(logged) != int(attempt) {
			t.Fatalf("attempt %d: logged %d lines, want one for each count the entry reached", attempt, len(logged))
		}
		if line := logged[len(logged)-1]; !reflect.DeepEqual(line.entry, newest) {
			t.Fatalf("attempt %d: logged %#v, want the entry as stored %#v", attempt, line.entry, newest)
		}
	}
}

// A migration run that ends stores its RunFinished in the write that retires
// its claim, and that write logs it.
func TestAMigrationRunThatEndsLogsItsEntry(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	approvedClaim(operation, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	logs := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0, CoordinationDigest: operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun:         migrationRunReport("applied", 3),
	})
	from := journalLength(migration.Status.Journal)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	lines := &journalLines{}
	reconciler.JournalLog = lines

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stored := readMigration(t, api, migration)
	assertLoggedOnce(t, lines, "PtahMigration", stored, stored.Status.Journal, from)
	ends := journalEntriesOf(stored, operatorv1alpha1.JournalRunFinished)
	if len(ends) != 1 || ends[0].Outcome != operatorv1alpha1.JournalSucceeded || ends[0].Approver != "alice@example.test" {
		t.Fatalf("the journal ends the run as %#v, want one Succeeded end naming its approver", ends)
	}
}

// The same for a migration: a conflicted write logs nothing, and the write
// that stores the entry logs it once.
func TestAConflictedMigrationStatusWriteLogsNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration := migrationFixture()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
	reconciler.Client = conflictingStatusWrites(api, "ptahmigrations")
	lines := &journalLines{}
	reconciler.JournalLog = lines

	publishing := func() (before, after *operatorv1alpha1.PtahMigration) {
		before = readMigration(t, api, migration)
		after = before.DeepCopy()
		after.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: "ptah-migration-plan-a", UID: "plan-uid"}
		return before, after
	}
	before, after := publishing()
	if err := reconciler.patchMigrationStatus(ctx, before, after); !apierrors.IsConflict(err) {
		t.Fatalf("patchMigrationStatus() = %v, want the conflict", err)
	}
	if logged := lines.logged(); len(logged) != 0 {
		t.Fatalf("a conflicted write logged %#v", logged)
	}
	before, after = publishing()
	if err := reconciler.patchMigrationStatus(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	stored := readMigration(t, api, migration)
	assertLoggedOnce(t, lines, "PtahMigration", stored, stored.Status.Journal, journalLength(before.Status.Journal))
}
