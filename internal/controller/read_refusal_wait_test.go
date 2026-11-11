package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The wait kubectl ptah refresh --wait makes, over what the reconciler stored: it
// waits from the answer to the patch that wrote the request.
func readWaitOptions(since client.Object) operate.WaitOptions {
	return operate.WaitOptions{Target: operate.TargetRead, Since: since, ReadRequest: requestedAt}
}

// assertReadTimesOutOnTheRefusal holds a wait for a read to its timeout, and the
// timeout to the refusal that holds the resource.
func assertReadTimesOutOnTheRefusal(t *testing.T, err error) {
	t.Helper()
	var timedOut *operate.TimeoutError
	if !errors.As(err, &timedOut) || !strings.Contains(timedOut.Error(), "PolicyRefused") {
		t.Fatalf("wait = %v, want a timeout that names the refusal", err)
	}
}

// backdateMigrationRefusal dates the refusal the reconciler stored by earlier. The
// reconciler stamps ArtifactVerified from the wall clock, so two refusals a test
// drives within one second would carry one stamp, which two reads that each run a
// Job for every step never do. The fixture's refusal is dated apart instead.
func (h *migrationLifecycle) backdateMigrationRefusal(by time.Duration) {
	h.t.Helper()
	migration := h.migration()
	refused := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified)
	if refused == nil || refused.Reason != string(operatorv1alpha1.ReasonPolicyRefused) {
		h.t.Fatalf("ArtifactVerified = %#v, want a refusal to backdate", refused)
	}
	refused.LastTransitionTime = metav1.NewTime(refused.LastTransitionTime.Add(-by))
	if err := h.api.Status().Update(context.Background(), migration); err != nil {
		h.t.Fatal(err)
	}
}

// requestRead writes the read request as kubectl ptah refresh does, and returns
// the stored resource, which carries it.
func (h *migrationLifecycle) requestRead() *operatorv1alpha1.PtahMigration {
	h.t.Helper()
	requested := h.migration()
	requested.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
	if err := h.api.Update(context.Background(), requested); err != nil {
		h.t.Fatal(err)
	}
	return h.migration()
}

func migrationRefusalStamp(t *testing.T, migration *operatorv1alpha1.PtahMigration) metav1.Time {
	t.Helper()
	refused := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified)
	if refused == nil || refused.Status != metav1.ConditionFalse || refused.Reason != string(operatorv1alpha1.ReasonPolicyRefused) {
		t.Fatalf("ArtifactVerified = %#v, want a refusal", refused)
	}
	return refused.LastTransitionTime
}

// A read request on a migration the policy refused. The operator takes it, starts
// the read, and the read ends in the policy's refusal again: the wait refresh
// --wait makes stops on that refusal, which the read it asked for ended in.
func TestAWaitForAReadStopsOnTheRefusalItsMigrationReadEndedIn(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	harness.backdateMigrationRefusal(time.Hour)
	before := migrationRefusalStamp(t, harness.migration())
	requested := harness.requestRead()

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(refusedBy(testDigest))
	refused := harness.migration()
	if refused.Status.LastReadRequest != requestedAt || refused.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked ||
		migrationRefusalStamp(t, refused).Time.Equal(before.Time) {
		t.Fatalf("lastReadRequest %q, phase %s, refusal at %s; want the request answered and a refusal of the read after it",
			refused.Status.LastReadRequest, refused.Status.Phase, migrationRefusalStamp(t, refused))
	}
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

	_, err := waitOn(harness.api, ref, readWaitOptions(requested))

	var stop *operate.StopError
	if !errors.As(err, &stop) || stop.Target != operate.TargetRead {
		t.Fatalf("wait = %v, want the wait for a read stopped", err)
	}
	assertRefusalStopsWait(t, err, "require_signature")
}

// The refusal the resource stood on when the request was written does not end the
// wait, wherever the read the request starts has got to.
func TestAWaitForAReadDoesNotStopOnTheMigrationRefusalFromBeforeTheRequest(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		move func(*testing.T, *migrationLifecycle, *operatorv1alpha1.PtahMigration)
		// atRest is a row that ends where the refusal holds the resource, which
		// the timeout names.
		atRest bool
	}{
		{"the operator took the request and has not claimed the read", func(t *testing.T, harness *migrationLifecycle, requested *operatorv1alpha1.PtahMigration) {
			t.Helper()
			answered := reconcileMigration(t, harness.reconciler, harness.api, requested)
			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
				t.Fatalf("lastReadRequest %q, operation %#v; want the request answered and no claim yet",
					answered.Status.LastReadRequest, answered.Status.ActiveOperation)
			}
		}, true},
		{"the read claimed its Resolve", func(t *testing.T, harness *migrationLifecycle, _ *operatorv1alpha1.PtahMigration) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		}, false},
		{"its Verify is under way", func(t *testing.T, harness *migrationLifecycle, _ *operatorv1alpha1.PtahMigration) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			harness := newMigrationLifecycle(t, migrationFixture())
			clock := movableMigrationClock(harness.reconciler, harness.api)
			harness.refuse(clock)
			harness.backdateMigrationRefusal(time.Hour)
			requested := harness.requestRead()
			row.move(t, harness, requested)
			ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

			_, err := waitOn(harness.api, ref, readWaitOptions(requested))

			if row.atRest {
				assertReadTimesOutOnTheRefusal(t, err)
				return
			}
			assertWaitKeepsRunning(t, err)
		})
	}
}

// A read already under way when the request is written does not answer it: the
// operator takes the request once that read has ended, here in a refusal, and
// starts another. The wait reads the document in which the operator took the
// request, which shows the refusal of the read under way at rest, and goes on to
// the refusal of the read after it. Each refusal is dated apart from the last, as
// reads that run Jobs are.
func TestAWaitForAReadSkipsTheMigrationRefusalOfAReadUnderWayAtTheRequest(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	harness.backdateMigrationRefusal(2 * time.Hour)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	requested := harness.requestRead()
	if requested.Status.ActiveOperation == nil {
		t.Fatal("the row wrote its request with no read under way, so it measures nothing")
	}

	var answering *operatorv1alpha1.PtahMigration
	reads := 0
	operator := interceptor.NewClient(harness.api.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			reads++
			switch reads {
			case 1:
				// The read under way ends in a refusal, and the operator takes the
				// request in the pass after it.
				harness.answer(resolvedTo(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationVerify)
				harness.answerUntilTaken(refusedBy(testDigest))
				harness.backdateMigrationRefusal(time.Hour)
			case 2:
				harness.claimed(operatorv1alpha1.MigrationOperationResolve)
				harness.answer(resolvedTo(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationVerify)
				harness.answer(refusedBy(testDigest))
			}
			if err := c.Get(ctx, key, object, options...); err != nil {
				return err
			}
			if reads == 1 {
				answering = object.(*operatorv1alpha1.PtahMigration).DeepCopy()
			}
			return nil
		},
	})
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

	_, err := waitOn(operator, ref, readWaitOptions(requested))

	assertRefusalStopsWait(t, err, "require_signature")
	if reads != 2 || answering == nil || answering.Status.LastReadRequest != requestedAt ||
		answering.Status.ActiveOperation != nil || answering.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("the wait stopped at read %d after reading %#v first; want it to read the request taken beside the "+
			"refusal of the read under way, and stop at the next", reads, answering)
	}
}

// answerUntilTaken answers the claim in flight with result and reconciles one pass
// at a time until the operator has taken the read request, and no further.
func (h *migrationLifecycle) answerUntilTaken(result runner.Result) {
	h.t.Helper()
	operation := h.migration().Status.ActiveOperation
	if operation == nil {
		h.t.Fatal("no operation is waiting for a result")
	}
	result.ProtocolVersion = runner.ProtocolVersion
	result.OperationID = operation.ID
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		h.t.Fatal(err)
	}
	h.logs.content = frame
	for range 12 {
		if taken := reconcileMigration(h.t, h.reconciler, h.api, h.migrationObject()); taken.Status.LastReadRequest == requestedAt {
			return
		}
	}
	h.t.Fatalf("the operator never took the read request: %#v", h.migration().Status)
}

// schemaLifecycle is migrationLifecycle for a schema: it reconciles until the
// controller settles, and answers the Job the controller dispatched.
type schemaLifecycle struct {
	t          *testing.T
	reconciler *SchemaReconciler
	api        client.Client
	logs       *replayLogs
	key        client.ObjectKey
}

// refusedSchemaLifecycle is refusedSchema, with the Resolve before the refusal
// harvested an hour before the reconciler's clock, and a reconciler that can
// dispatch and harvest the reads after it.
func refusedSchemaLifecycle(t *testing.T) *schemaLifecycle {
	t.Helper()
	refused, reconciler, api := refusedSchema(t)
	logs := &replayLogs{}
	reconciler.Logs = logs
	// A real API server stamps a UID on every object it accepts, and the
	// controller refuses a Job without one.
	reconciler.Client = interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if object.GetUID() == "" {
				object.SetUID(types.UID("created-" + object.GetName()))
			}
			return c.Create(ctx, object, options...)
		},
	})
	h := &schemaLifecycle{t: t, reconciler: reconciler, api: api, logs: logs, key: client.ObjectKeyFromObject(refused)}
	stored := h.schema()
	resolved := metav1.NewTime(reconciler.now().Add(-time.Hour))
	stored.Status.Source.ResolvedAt = &resolved
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *schemaLifecycle) schema() *operatorv1alpha1.PtahSchema {
	h.t.Helper()
	stored := &operatorv1alpha1.PtahSchema{}
	if err := h.api.Get(context.Background(), h.key, stored); err != nil {
		h.t.Fatal(err)
	}
	return stored
}

// settle reconciles until the controller stops asking to be requeued at once.
func (h *schemaLifecycle) settle() {
	h.t.Helper()
	for range 12 {
		result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
		if err != nil {
			h.t.Fatalf("Reconcile() error = %v", err)
		}
		if result.RequeueAfter == 0 || result.RequeueAfter > statusPatchRequeue {
			return
		}
	}
	h.t.Fatalf("the controller never settled: %#v", h.schema().Status)
}

// claimed settles the controller, asserts which operation it claimed, and plays
// the part Kubernetes plays: the Job it created ran a Pod and ended.
func (h *schemaLifecycle) claimed(want operatorv1alpha1.OperationType) {
	h.t.Helper()
	h.settle()
	schema := h.schema()
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != want {
		h.t.Fatalf("active operation = %#v, want a %s claim (phase %q, conditions %#v)",
			operation, want, schema.Status.Phase, schema.Status.Conditions)
	}
	job := &batchv1.Job{}
	if err := h.api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: operation.JobName}, job); err != nil {
		h.t.Fatalf("the %s claim dispatched no Job: %v", want, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := h.api.Status().Update(context.Background(), job); err != nil {
		h.t.Fatal(err)
	}
	if err := h.api.Create(context.Background(), terminalPodFor(job)); err != nil {
		h.t.Fatal(err)
	}
}

// answer puts one result frame where the controller reads it, then settles.
func (h *schemaLifecycle) answer(result runner.Result) {
	h.t.Helper()
	operation := h.schema().Status.ActiveOperation
	if operation == nil {
		h.t.Fatal("no operation is waiting for a result")
	}
	result.ProtocolVersion = runner.ProtocolVersion
	result.OperationID = operation.ID
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		h.t.Fatal(err)
	}
	h.logs.content = frame
	h.settle()
}

// readsAgain takes the schema through the Resolve the read starts, to the Verify
// it claims next.
func (h *schemaLifecycle) readsAgain() {
	h.t.Helper()
	h.claimed(operatorv1alpha1.OperationResolve)
	h.answer(runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	h.claimed(operatorv1alpha1.OperationVerify)
}

// refuses answers the Verify in flight with the policy's refusal of the artifact.
func (h *schemaLifecycle) refuses() {
	h.t.Helper()
	operation := h.schema().Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationVerify {
		h.t.Fatalf("operation = %#v, want a Verify to refuse", operation)
	}
	h.answer(runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 2, ResolvedDigest: testDigest,
		VerificationRequirements: []string{"require_signature"}, VerificationPolicyDigest: operation.VerificationPolicyDigest,
		Error: &runner.ResultError{Code: "verification_refused", Message: "artifact does not satisfy the verification policy"},
	})
}

// requestRead writes the read request as kubectl ptah refresh does, and returns
// the stored resource, which carries it.
func (h *schemaLifecycle) requestRead() *operatorv1alpha1.PtahSchema {
	h.t.Helper()
	annotateSchema(h.t, h.api, h.schema(), requestedAt)
	return h.schema()
}

// The read a request starts on a schema the policy refused ends in the policy's
// refusal again, and the wait refresh --wait makes stops on it. The schema's
// ArtifactVerified stays False from Pending to PolicyRefused and keeps its
// lastTransitionTime, so what dates the refusal is the Resolve before it.
func TestAWaitForAReadStopsOnTheRefusalItsSchemaReadEndedIn(t *testing.T) {
	t.Parallel()
	harness := refusedSchemaLifecycle(t)
	before := harness.schema()
	requested := harness.requestRead()

	harness.readsAgain()
	harness.refuses()
	refused := harness.schema()
	verified := meta.FindStatusCondition(refused.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	wasVerified := meta.FindStatusCondition(before.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	if refused.Status.LastReadRequest != requestedAt || refused.Status.Phase != operatorv1alpha1.PhaseBlocked ||
		verified == nil || verified.Reason != string(operatorv1alpha1.ReasonPolicyRefused) ||
		refused.Status.Source.ResolvedAt == nil || refused.Status.Source.ResolvedAt.Equal(before.Status.Source.ResolvedAt) ||
		!verified.LastTransitionTime.Equal(&wasVerified.LastTransitionTime) {
		t.Fatalf("lastReadRequest %q, phase %s, ArtifactVerified %#v, resolvedAt %s after %s; want the request answered, "+
			"and a refusal of the read after it beside a Resolve of its own and the condition's first transition",
			refused.Status.LastReadRequest, refused.Status.Phase, verified, refused.Status.Source.ResolvedAt, before.Status.Source.ResolvedAt)
	}
	ref := operate.Ref{Family: operate.FamilySchema, Namespace: harness.key.Namespace, Name: harness.key.Name}

	_, err := waitOn(harness.api, ref, readWaitOptions(requested))

	var stop *operate.StopError
	if !errors.As(err, &stop) || stop.Target != operate.TargetRead {
		t.Fatalf("wait = %v, want the wait for a read stopped", err)
	}
	assertRefusalStopsWait(t, err, "require_signature")
}

func TestAWaitForAReadDoesNotStopOnTheSchemaRefusalFromBeforeTheRequest(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		move   func(*testing.T, *schemaLifecycle)
		atRest bool
	}{
		{"the operator took the request and has not claimed the read", func(t *testing.T, harness *schemaLifecycle) {
			t.Helper()
			answered := reconcileSchema(t, harness.reconciler, harness.api, harness.schema())
			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
				t.Fatalf("lastReadRequest %q, operation %#v; want the request answered and no claim yet",
					answered.Status.LastReadRequest, answered.Status.ActiveOperation)
			}
		}, true},
		{"the read claimed its Resolve", func(t *testing.T, harness *schemaLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.OperationResolve)
		}, false},
		{"its Verify is under way", func(t *testing.T, harness *schemaLifecycle) {
			t.Helper()
			harness.readsAgain()
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			harness := refusedSchemaLifecycle(t)
			requested := harness.requestRead()
			row.move(t, harness)
			ref := operate.Ref{Family: operate.FamilySchema, Namespace: harness.key.Namespace, Name: harness.key.Name}

			_, err := waitOn(harness.api, ref, readWaitOptions(requested))

			if row.atRest {
				assertReadTimesOutOnTheRefusal(t, err)
				return
			}
			assertWaitKeepsRunning(t, err)
		})
	}
}
