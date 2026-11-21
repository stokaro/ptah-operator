package controller

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// assertBlockedStop holds err to a StopBlocked in phase Blocked that names reason
// and says, the words the reconciler stored for it. readEnded is a wait for a read
// whose own read ended there.
func assertBlockedStop(t *testing.T, err error, reason operatorv1alpha1.ConditionReason, says string, readEnded bool) {
	t.Helper()
	var stop *operate.StopError
	if !errors.As(err, &stop) || stop.Reason != operate.StopBlocked || stop.Phase != "Blocked" ||
		stop.Held != string(reason) || stop.ReadEnded != readEnded ||
		!strings.Contains(stop.Message, string(reason)+": ") || !strings.Contains(stop.Message, says) {
		t.Fatalf("wait = %v, want a %s stop in phase Blocked naming %s and %q, with ReadEnded %t",
			err, operate.StopBlocked, reason, says, readEnded)
	}
}

// assertReadTimesOutOnTheReadRefusal holds a wait for a read to its timeout, and
// the timeout to the read refusal that holds the resource.
func assertReadTimesOutOnTheReadRefusal(t *testing.T, err error, reason operatorv1alpha1.ConditionReason) {
	t.Helper()
	var timedOut *operate.TimeoutError
	if !errors.As(err, &timedOut) || !strings.Contains(timedOut.Error(), string(reason)) {
		t.Fatalf("wait = %v, want a timeout that names %s", err, reason)
	}
}

// blockedConfigMapSchema is a schema on a ConfigMap source whose reads the
// reconciler takes with the real Job builder, each Job run through the runner
// by the read's Ptah. A ConfigMap source is read in the pass, so a read that
// ends on the file is one Job, the Observe, and a read that ends on a refused
// plan is two, the Observe that found the change and the Plan.
type blockedConfigMapSchema struct {
	t          *testing.T
	refusal    schemaRefusalRow
	reconciler *SchemaReconciler
	api        client.Client
	schema     *operatorv1alpha1.PtahSchema
	now        time.Time
}

func newBlockedConfigMapSchema(t *testing.T, refusal schemaRefusalRow) *blockedConfigMapSchema {
	t.Helper()
	source := "CREATE TABLE events (id bigint PRIMARY KEY, created_at timestamptz DEFAULT 'now');\n"
	if refusal.code == runner.CodeDesiredSchemaInvalid {
		source = "CREATE DOMAIN moment AS timestamp DEFAULT 'now';\nCREATE TABLE events (id bigint PRIMARY KEY, created_at moment NOT NULL);\n"
	}
	schema := configMapSchemaFixture()
	schema.Spec.Interval = metav1.Duration{Duration: time.Hour}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap(source))
	builder, key := leaderBuilder(t)
	reconciler.Client = uidByNameClient{Client: api}
	reconciler.Jobs = builder
	reconciler.SealKey = key
	reconciler.Locks = targetlock.New(api, api, nil)
	h := &blockedConfigMapSchema{t: t, refusal: refusal, reconciler: reconciler, api: api, schema: schema, now: reconciler.now()}
	reconciler.Clock = func() time.Time { return h.now }
	return h
}

func (h *blockedConfigMapSchema) stored() *operatorv1alpha1.PtahSchema {
	h.t.Helper()
	return storedSchemaOf(h.t, h.api, h.schema)
}

// reads takes passes until each operation of a read is claimed, runs its Job
// against the read's Ptah, and takes the pass that harvests the last, which has
// to leave the schema at rest in Blocked on the refusal. Every operation before
// the refused one has to succeed. The reconciler's clock stands still, so the
// read is moved a minute past the last: two reads that each run a Job are never
// within one second.
func (h *blockedConfigMapSchema) reads() *operatorv1alpha1.PtahSchema {
	h.t.Helper()
	h.now = h.now.Add(time.Minute)
	operations := []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve}
	if !slices.Contains(h.refusal.operations, operatorv1alpha1.OperationObserve) {
		operations = append(operations, operatorv1alpha1.OperationPlan)
	}
	ptah := h.readPtah()
	for index, operation := range operations {
		job := reconcileUntilOperationJob(h.t, h.reconciler, h.api, h.schema, operation)
		result, frame := runCapturedRead(h.t, h.reconciler, job, operation, ptah)
		if refused := index == len(operations)-1; refused && (result.Error == nil || result.Error.Code != h.refusal.code) {
			h.t.Fatalf("the %s returned %#v, want %s", operation, result.Error, h.refusal.code)
		} else if !refused && result.Error != nil {
			h.t.Fatalf("the %s before the refused %s returned %#v", operation, operations[len(operations)-1], result.Error)
		}
		finishBuiltJob(h.t, h.reconciler, h.api, job, frame)
	}
	blocked := reconcileSchema(h.t, h.reconciler, h.api, h.schema)
	ready := meta.FindStatusCondition(blocked.Status.Conditions, operatorv1alpha1.ConditionReady)
	if blocked.Status.Phase != operatorv1alpha1.PhaseBlocked || blocked.Status.ActiveOperation != nil ||
		ready == nil || ready.Reason != string(h.refusal.reason) || !strings.Contains(ready.Message, h.refusal.says) {
		h.t.Fatalf("the read did not stop on its file: phase %s, claim %#v, Ready %#v",
			blocked.Status.Phase, blocked.Status.ActiveOperation, ready)
	}
	return blocked
}

// readPtah answers every command of the read. A refusal an Observe meets is
// the refusal's own Ptah. A plan refusal follows an Observe that found the
// change, which viewObservedPtah answers.
func (h *blockedConfigMapSchema) readPtah() runner.Executor {
	if slices.Contains(h.refusal.operations, operatorv1alpha1.OperationObserve) {
		return h.refusal.ptah(h.t)
	}
	return viewObservedPtah{
		observe: capturedPtah{t: h.t, drift: harnessDocument(h.t, "differs-view-invoker-unstated-postgres", "observe", "1-schema-drift.json")},
		plan:    h.refusal.ptah(h.t),
	}
}

// viewObservedPtah answers `schema drift` as the pinned Ptah answered the runner
// harness row differs-view-invoker-unstated-postgres, whose plan the plan
// refusal row's Ptah refuses: the report finds the view changed. Every other
// command goes to plan.
type viewObservedPtah struct {
	observe capturedPtah
	plan    runner.Executor
}

func (p viewObservedPtah) Execute(ctx context.Context, spec runner.CommandSpec, stdout, stderr io.Writer) (int, error) {
	if len(spec.Args) >= 2 && spec.Args[0] == "schema" && spec.Args[1] == "drift" {
		return p.observe.Execute(ctx, spec, stdout, stderr)
	}
	return p.plan.Execute(ctx, spec, stdout, stderr)
}

// requestRead writes the read request as kubectl ptah refresh does, and returns
// the stored resource, which carries it.
func (h *blockedConfigMapSchema) requestRead() *operatorv1alpha1.PtahSchema {
	h.t.Helper()
	annotateSchema(h.t, h.api, h.stored(), requestedAt)
	return h.stored()
}

func (h *blockedConfigMapSchema) ref() operate.Ref {
	return operate.Ref{Family: operate.FamilySchema, Namespace: h.schema.Namespace, Name: h.schema.Name}
}

// The read a request starts on a schema stopped on its file, or on a plan Ptah
// refused, ends on the same refusal again, and the wait refresh --wait makes
// stops on it, by the stamp the read moved: status.lastAttemptTime, which each
// claim and the refusal write. The refused plan is met at the Plan, after an
// Observe that found the view changed, so its read is two Jobs. The refusal
// from before the request, with the request taken and no read claimed yet,
// does not stop it.
func TestAWaitForAReadStopsOnTheReadRefusalItsSchemaReadEndedIn(t *testing.T) {
	t.Parallel()
	for _, refusal := range schemaRefusalRows() {
		t.Run(refusal.name+"/the read ended in it", func(t *testing.T) {
			t.Parallel()
			h := newBlockedConfigMapSchema(t, refusal)
			first := h.reads()
			requested := h.requestRead()

			second := h.reads()

			if second.Status.LastReadRequest != requestedAt || second.Status.LastAttemptTime == nil ||
				second.Status.LastAttemptTime.Equal(first.Status.LastAttemptTime) {
				t.Fatalf("lastReadRequest %q, lastAttemptTime %v after %v; want the request taken and the stamp moved",
					second.Status.LastReadRequest, second.Status.LastAttemptTime, first.Status.LastAttemptTime)
			}
			_, err := waitOn(h.api, h.ref(), readWaitOptions(requested))
			assertBlockedStop(t, err, refusal.reason, refusal.says, true)
		})
		t.Run(refusal.name+"/the refusal from before the request", func(t *testing.T) {
			t.Parallel()
			h := newBlockedConfigMapSchema(t, refusal)
			h.reads()
			requested := h.requestRead()
			answered := reconcileSchema(t, h.reconciler, h.api, h.schema)
			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil ||
				answered.Status.Phase != operatorv1alpha1.PhaseBlocked {
				t.Fatalf("lastReadRequest %q, operation %#v, phase %s; want the request answered and no claim yet",
					answered.Status.LastReadRequest, answered.Status.ActiveOperation, answered.Status.Phase)
			}

			_, err := waitOn(h.api, h.ref(), readWaitOptions(requested))

			assertReadTimesOutOnTheReadRefusal(t, err, refusal.reason)
		})
		t.Run(refusal.name+"/a wait for in-sync on the schema already stopped", func(t *testing.T) {
			t.Parallel()
			h := newBlockedConfigMapSchema(t, refusal)
			h.reads()

			_, err := waitOn(h.api, h.ref(), operate.WaitOptions{Target: operate.TargetInSync})

			assertBlockedStop(t, err, refusal.reason, refusal.says, false)
		})
	}
}

// migrationReadRefusal is a history read's refusal for the lifecycle harness to
// answer with, and what the reconciler stores for it.
type migrationReadRefusal struct {
	name   string
	reason operatorv1alpha1.ConditionReason
	answer func(*migrationLifecycle) runner.Result
	// says is in the Blocked condition the refusal writes.
	says string
}

func migrationReadRefusals(t *testing.T) []migrationReadRefusal {
	t.Helper()
	unread := capturedDirectoryReason(t, "scratch-unread.json")
	return []migrationReadRefusal{
		{
			name: "a directory Ptah cannot read", reason: operatorv1alpha1.ReasonMigrationDirectoryUnreadable, says: unread,
			answer: func(h *migrationLifecycle) runner.Result {
				return runner.Result{
					Operation: runner.OperationMigrationHistory, ChildExitCode: 2,
					CoordinationDigest: h.coordinationDigest(), TargetIdentityDigest: testDigest,
					Error: &runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: unread},
				}
			},
		},
		{
			name: "a repeatable migration whose last attempt is unfinished", reason: operatorv1alpha1.ReasonRepeatableMigrationUnfinished,
			says: "ptah migrations repair --repeatable item_names",
			answer: func(h *migrationLifecycle) runner.Result {
				return historyAnswer(h, syncedHistoryWith(dataplane.MigrationRepeatable{
					Identity: "item_names", Path: "R__item_names.sql", Checksum: repeatableContentB,
					AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateFailed,
				}))
			},
		},
		{
			name: "a revision recorded dirty", reason: operatorv1alpha1.ReasonHistoryDirty,
			says: "Revision 1 is recorded dirty; a person has to decide what the interrupted run did",
			answer: func(h *migrationLifecycle) runner.Result {
				report := inSyncReport()
				report.DirtyRevision = &dataplane.MigrationDirty{Version: 1, Applied: 1, Total: 2}
				return historyAnswer(h, report)
			},
		},
		{
			name: "a database ahead of the artifact", reason: operatorv1alpha1.ReasonHistoryAhead,
			says: "publish an artifact that carries the versions the database already applied",
			answer: func(h *migrationLifecycle) runner.Result {
				report := inSyncReport()
				report.CurrentVersion = 2
				return historyAnswer(h, report)
			},
		},
	}
}

// readsInto takes the migration through a read that ends in refusal: Resolve,
// Verify, and the history read that refuses.
func (h *migrationLifecycle) readsInto(refusal migrationReadRefusal) *operatorv1alpha1.PtahMigration {
	h.t.Helper()
	h.claimed(operatorv1alpha1.MigrationOperationResolve)
	h.answer(resolvedTo(testDigest))
	h.claimed(operatorv1alpha1.MigrationOperationVerify)
	h.answer(acceptedBy(testDigest))
	h.claimed(operatorv1alpha1.MigrationOperationHistory)
	h.answer(refusal.answer(h))
	stored := h.migration()
	progressing := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if stored.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || stored.Status.ActiveOperation != nil ||
		progressing == nil || progressing.Status != metav1.ConditionFalse || progressing.Reason != string(refusal.reason) {
		h.t.Fatalf("the read did not stop: phase %s, claim %#v, Progressing %#v",
			stored.Status.Phase, stored.Status.ActiveOperation, progressing)
	}
	return stored
}

// backdateReadRefusal dates the read refusal the reconciler stored by earlier.
// The reconciler stamps Progressing from the wall clock, so two refusals a test
// drives within one second would carry one stamp, which two reads that each run a
// Job for every step never do. The fixture's refusal is dated apart instead.
func (h *migrationLifecycle) backdateReadRefusal(by time.Duration) metav1.Time {
	h.t.Helper()
	migration := h.migration()
	progressing := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionFalse {
		h.t.Fatalf("Progressing = %#v, want a read refusal to backdate", progressing)
	}
	progressing.LastTransitionTime = metav1.NewTime(progressing.LastTransitionTime.Add(-by))
	if err := h.api.Status().Update(context.Background(), migration); err != nil {
		h.t.Fatal(err)
	}
	return progressing.LastTransitionTime
}

// The read a request starts on a migration stopped on its directory or its
// history ends there again, and the wait refresh --wait makes stops on it, by the
// stamp the read moved: the lastTransitionTime of Progressing, which the claim
// takes to True and the refusal back to False. Blocked and Ready keep the reason
// through the read. The refusal from before the request, with the request taken
// and no read claimed yet, does not stop it.
func TestAWaitForAReadStopsOnTheReadRefusalItsMigrationReadEndedIn(t *testing.T) {
	t.Parallel()
	for _, refusal := range migrationReadRefusals(t) {
		t.Run(refusal.name+"/the read ended in it", func(t *testing.T) {
			t.Parallel()
			harness := newMigrationLifecycle(t, migrationFixture())
			clock := movableMigrationClock(harness.reconciler, harness.api)
			harness.readsInto(refusal)
			floor := harness.backdateReadRefusal(time.Hour)
			harness.dueNow(clock)
			requested := harness.requestRead()

			claimed := false
			reads := 0
			ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}
			// The read runs while the wait polls: its first poll finds the request
			// not yet taken, and the read ends before the second.
			operator := readThenRun(harness.api, &reads, func() {
				harness.claimed(operatorv1alpha1.MigrationOperationResolve)
				stored := harness.migration()
				blocked := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
				claimed = blocked != nil && blocked.Reason == string(refusal.reason)
				harness.answer(resolvedTo(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationVerify)
				harness.answer(acceptedBy(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationHistory)
				harness.answer(refusal.answer(harness))
			})

			_, err := waitOn(operator, ref, readWaitOptions(requested))

			assertBlockedStop(t, err, refusal.reason, refusal.says, true)
			ended := harness.migration()
			progressing := meta.FindStatusCondition(ended.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if !claimed || ended.Status.LastReadRequest != requestedAt || progressing.LastTransitionTime.Equal(&floor) {
				t.Fatalf("Blocked kept through the claim %t, lastReadRequest %q, Progressing at %v after %v; want the request "+
					"taken, the refusal's reason on Blocked while the read ran, and the stamp moved",
					claimed, ended.Status.LastReadRequest, progressing.LastTransitionTime, floor)
			}
		})
		t.Run(refusal.name+"/the refusal from before the request", func(t *testing.T) {
			t.Parallel()
			harness := newMigrationLifecycle(t, migrationFixture())
			clock := movableMigrationClock(harness.reconciler, harness.api)
			harness.readsInto(refusal)
			harness.backdateReadRefusal(time.Hour)
			harness.dueNow(clock)
			requested := harness.requestRead()
			answered := reconcileMigration(t, harness.reconciler, harness.api, requested)
			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
				t.Fatalf("lastReadRequest %q, operation %#v; want the request answered and no claim yet",
					answered.Status.LastReadRequest, answered.Status.ActiveOperation)
			}
			ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

			_, err := waitOn(harness.api, ref, readWaitOptions(requested))

			assertReadTimesOutOnTheReadRefusal(t, err, refusal.reason)
		})
		t.Run(refusal.name+"/a wait for in-sync on the migration already stopped", func(t *testing.T) {
			t.Parallel()
			harness := newMigrationLifecycle(t, migrationFixture())
			harness.readsInto(refusal)
			ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

			_, err := waitOn(harness.api, ref, operate.WaitOptions{Target: operate.TargetInSync})

			assertBlockedStop(t, err, refusal.reason, refusal.says, false)
		})
	}
}

// readThenRun is api for a wait to read through: its first Get of the resource is
// answered, and run then takes the passes that follow before the wait reads again.
func readThenRun(api client.Client, reads *int, run func()) client.Reader {
	return interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if err := c.Get(ctx, key, object, options...); err != nil {
				return err
			}
			*reads++
			if *reads == 1 {
				run()
			}
			return nil
		},
	})
}

// A schema's Plan refuses beside the reading its Observe recorded: a read a
// request started that ends at the Plan keeps the Observe's lastObservedAt, moves
// lastAttemptTime and leaves lastReadRequest at the request, and the wait
// refresh --wait makes stops on it, though the reading is fresh. The rule keys on
// the request the document says was taken last: one taken after this wait's
// request leaves the refusal to that request's wait, and this one says its
// request was replaced (TestAWaitForAReadStopsOnTheBlockedItsReadEndedIn in
// internal/operate holds the wait that saw its own request taken first to done).
func TestAWaitForAReadStopsOnAPlanRefusalBesideItsReading(t *testing.T) {
	t.Parallel()
	relativeTime := capturedSchemaReason(t, "validate-relative-time.txt")
	failure := runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: relativeTime}
	// The request is at 11:58 and the reconciler's clock reads 12:00. The read's
	// Observe recorded its reading at 11:59, and its Plan was claimed at 11:59:30.
	observed := metav1.NewTime(readRequestClock.Add(-time.Minute))
	claimedAt := metav1.NewTime(readRequestClock.Add(-30 * time.Second))
	const newer = "2026-08-30T11:58:30Z"
	for _, row := range []struct {
		name string
		// taken is the request the operator answered last.
		taken    string
		wantStop bool
	}{
		{name: "the read the request started", taken: requestedAt, wantStop: true},
		{name: "a newer request taken", taken: newer},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema, reconciler, api, _ := unreadableReadFixture(t, operatorv1alpha1.OperationPlan, runner.OperationPlan, &failure,
				func(schema *operatorv1alpha1.PtahSchema) {
					schema.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: row.taken}
					schema.Status.LastReadRequest = row.taken
					schema.Status.Target.LastObservedAt = observed.DeepCopy()
					schema.Status.LastAttemptTime = claimedAt.DeepCopy()
				})
			// What refresh read when it wrote the request: the schema at rest in
			// sync, with an earlier reading and an earlier attempt.
			since := schema.DeepCopy()
			since.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
			since.Status.LastReadRequest = ""
			since.Status.ActiveOperation = nil
			since.Status.Phase = operatorv1alpha1.PhaseInSync
			since.Status.Target.LastObservedAt = earlierInstant.DeepCopy()
			since.Status.LastAttemptTime = earlierInstant.DeepCopy()

			actual := reconcileSchema(t, reconciler, api, schema)

			ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReady)
			if actual.Status.Phase != operatorv1alpha1.PhaseBlocked || actual.Status.ActiveOperation != nil || ready == nil ||
				ready.Reason != string(operatorv1alpha1.ReasonSchemaSourceUnreadable) ||
				actual.Status.Target.LastObservedAt == nil || !actual.Status.Target.LastObservedAt.Equal(&observed) ||
				actual.Status.LastAttemptTime == nil || actual.Status.LastAttemptTime.Equal(&claimedAt) ||
				actual.Status.LastReadRequest != row.taken {
				t.Fatalf("phase %s, claim %#v, Ready %#v, lastObservedAt %v, lastAttemptTime %v, lastReadRequest %q; "+
					"want the Plan refused beside the reading at %v, the attempt moved from %v and the request %q taken",
					actual.Status.Phase, actual.Status.ActiveOperation, ready, actual.Status.Target.LastObservedAt,
					actual.Status.LastAttemptTime, actual.Status.LastReadRequest, observed, claimedAt, row.taken)
			}
			ref := operate.Ref{Family: operate.FamilySchema, Namespace: schema.Namespace, Name: schema.Name}

			_, err := waitOn(api, ref, readWaitOptions(since))

			if row.wantStop {
				assertBlockedStop(t, err, operatorv1alpha1.ReasonSchemaSourceUnreadable, relativeTime, true)
				return
			}
			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopReadReplaced {
				t.Fatalf("wait = %v, want a %s stop", err, operate.StopReadReplaced)
			}
		})
	}
}
