package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// readRequestClock is the instant fakeReconciler and fakeMigrationReconciler
// decide on. A fixture's own times are dated apart from it, so a comparison
// with what the code stamped is not between two equal instants.
var readRequestClock = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

const requestedAt = "2026-08-30T11:58:00Z"

func TestAReadRequestIsReadFromTheAnnotationAgainstWhatStatusAnswered(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", operatorv1alpha1.MaxReadRequestLength+1)
	for _, test := range []struct {
		name        string
		annotations map[string]string
		answered    string
		want        readRequest
	}{
		{name: "no annotation", annotations: nil, want: readRequest{}},
		{name: "another annotation", annotations: map[string]string{"example.com/other": requestedAt}, want: readRequest{}},
		{name: "an instant not answered", annotations: map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt},
			want: readRequest{token: requestedAt, pending: true, instant: true}},
		{name: "an instant with an offset", annotations: map[string]string{operatorv1alpha1.ReadRequestAnnotation: "2026-08-30T13:58:00+02:00"},
			want: readRequest{token: "2026-08-30T13:58:00+02:00", pending: true, instant: true}},
		{name: "the instant answered", annotations: map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}, answered: requestedAt,
			want: readRequest{token: requestedAt}},
		{name: "a date without a time", annotations: map[string]string{operatorv1alpha1.ReadRequestAnnotation: "2026-08-30"},
			want: readRequest{token: "2026-08-30", pending: true}},
		{name: "a value past the bound", annotations: map[string]string{operatorv1alpha1.ReadRequestAnnotation: long},
			want: readRequest{token: readRequestToken(long), pending: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := readRequestOf(test.annotations, test.answered); got != test.want {
				t.Fatalf("readRequestOf() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestAReadRequestTokenFitsTheStatusField(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("y", 4096)
	token := readRequestToken(long)
	if !strings.HasPrefix(token, "sha256:") || len(token) != len("sha256:")+64 {
		t.Fatalf("readRequestToken(4096 bytes) = %q, want a sha256 digest", token)
	}
	if readRequestToken(long+"z") == token {
		t.Fatal("two long values gave one token, so the second would read as answered")
	}
	if exact := strings.Repeat("z", operatorv1alpha1.MaxReadRequestLength); readRequestToken(exact) != exact {
		t.Fatalf("a value at the bound is recorded as %q, not as given", readRequestToken(exact))
	}
}

func TestOnlyTheReadRequestAnnotationWakesASchema(t *testing.T) {
	t.Parallel()
	changed := readRequestChanged()
	with := func(annotations map[string]string) *operatorv1alpha1.PtahSchema {
		schema := schemaFixture()
		schema.Annotations = annotations
		return schema
	}
	requested := map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
	for _, test := range []struct {
		name     string
		old, new map[string]string
		want     bool
	}{
		{name: "a request added", old: nil, new: requested, want: true},
		{name: "a request changed", old: requested, new: map[string]string{operatorv1alpha1.ReadRequestAnnotation: "2026-08-30T11:59:00Z"}, want: true},
		{name: "a request removed", old: requested, new: nil, want: true},
		{name: "the same request", old: requested, new: requested, want: false},
		{name: "another annotation", old: nil, new: map[string]string{"example.com/other": "x"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := changed.Update(event.UpdateEvent{ObjectOld: with(test.old), ObjectNew: with(test.new)}); got != test.want {
				t.Fatalf("Update() = %t, want %t", got, test.want)
			}
		})
	}
	if changed.Create(event.CreateEvent{Object: with(requested)}) || changed.Delete(event.DeleteEvent{Object: with(requested)}) ||
		changed.Generic(event.GenericEvent{Object: with(requested)}) {
		t.Fatal("the read request filter passed an event other than an update")
	}
}

// requestedSchema is a schema in phase that waits until its next read an
// hour from the test clock, with a read requested.
func requestedSchema(phase operatorv1alpha1.ReconciliationPhase, value string) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Interval = metav1.Duration{Duration: time.Hour}
	schema.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: value}
	schema.Status.Phase = phase
	next := metav1.NewTime(readRequestClock.Add(time.Hour))
	schema.Status.NextReconciliationTime = &next
	return schema
}

func reconcileSchemaOnce(t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema) (*operatorv1alpha1.PtahSchema, time.Duration) {
	t.Helper()
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	return actual, result.RequeueAfter
}

func TestASchemaReadRequestBringsTheIntervalReadForward(t *testing.T) {
	t.Parallel()
	for _, phase := range []operatorv1alpha1.ReconciliationPhase{
		operatorv1alpha1.PhaseInSync, operatorv1alpha1.PhaseBlocked,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			schema := requestedSchema(phase, requestedAt)
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			recorder := record.NewFakeRecorder(8)
			reconciler.Recorder = recorder

			answered, requeue := reconcileSchemaOnce(t, reconciler, api, schema)

			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
				t.Fatalf("after the request: lastReadRequest=%q activeOperation=%#v", answered.Status.LastReadRequest, answered.Status.ActiveOperation)
			}
			if next := answered.Status.NextReconciliationTime; next == nil || !next.Time.Equal(readRequestClock) {
				t.Fatalf("next read = %v, want it brought forward from an hour away to %v", next, readRequestClock)
			}
			if requeue != statusPatchRequeue {
				t.Fatalf("requeue = %s, want the next pass after %s", requeue, statusPatchRequeue)
			}
			if event := findEvent(recorder, corev1.EventTypeNormal+" ReadRequested"); event == "" {
				t.Fatal("no ReadRequested Event")
			}

			claimed, _ := reconcileSchemaOnce(t, reconciler, api, answered)

			if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("the pass after the request claimed %#v, want the interval's Resolve", claimed.Status.ActiveOperation)
			}
			if claimed.Status.LastReadRequest != requestedAt {
				t.Fatalf("the Resolve claim lost the answered request: %q", claimed.Status.LastReadRequest)
			}
		})
	}
}

func TestAnAnsweredSchemaReadRequestIsNotAnsweredAgain(t *testing.T) {
	t.Parallel()
	schema := requestedSchema(operatorv1alpha1.PhaseInSync, requestedAt)
	schema.Status.LastReadRequest = requestedAt
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	actual, requeue := reconcileSchemaOnce(t, reconciler, api, schema)

	if actual.Status.ActiveOperation != nil {
		t.Fatalf("an answered request claimed %#v", actual.Status.ActiveOperation)
	}
	if next := actual.Status.NextReconciliationTime; next == nil || !next.Time.Equal(readRequestClock.Add(time.Hour)) {
		t.Fatalf("an answered request moved the next read to %v", next)
	}
	if requeue != time.Hour {
		t.Fatalf("requeue = %s, want the interval's deadline an hour away", requeue)
	}
}

// A read that started before the request may have read the database before
// the change the request is about, and a failed resource waits out its retry
// delay; neither answers the request.
func TestASchemaReadRequestWaitsForAReadThatStartsAfterIt(t *testing.T) {
	t.Parallel()
	for _, phase := range []operatorv1alpha1.ReconciliationPhase{
		operatorv1alpha1.PhaseObserving, operatorv1alpha1.PhasePlanning, operatorv1alpha1.PhaseFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			schema := requestedSchema(phase, requestedAt)
			reconciler, api := fakeReconciler(t, staticLogs{}, schema)

			actual, _ := reconcileSchemaOnce(t, reconciler, api, schema)

			if actual.Status.LastReadRequest != "" {
				t.Fatalf("phase %s answered the request: %q", phase, actual.Status.LastReadRequest)
			}
			if next := actual.Status.NextReconciliationTime; next != nil && next.Time.Equal(readRequestClock) {
				t.Fatalf("phase %s brought the next read forward to now", phase)
			}
		})
	}
}

func TestAFailedSchemaAnswersARequestWhenItsRetryDelayEnds(t *testing.T) {
	t.Parallel()
	schema := requestedSchema(operatorv1alpha1.PhaseFailed, requestedAt)
	ended := metav1.NewTime(readRequestClock.Add(-time.Minute))
	schema.Status.NextReconciliationTime = &ended
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	answered, _ := reconcileSchemaOnce(t, reconciler, api, schema)

	if answered.Status.LastReadRequest != requestedAt || !answered.Status.NextReconciliationTime.Time.Equal(ended.Time) {
		t.Fatalf("lastReadRequest=%q next=%v, want the request answered and the retry's deadline kept",
			answered.Status.LastReadRequest, answered.Status.NextReconciliationTime)
	}
	claimed, _ := reconcileSchemaOnce(t, reconciler, api, answered)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("the retry claimed %#v, want a Resolve", claimed.Status.ActiveOperation)
	}
}

func TestASchemaReadRequestThatIsNotAnInstantStartsNothingAndSaysSoOnce(t *testing.T) {
	t.Parallel()
	schema := requestedSchema(operatorv1alpha1.PhaseInSync, "now please")
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	recorder := record.NewFakeRecorder(8)
	reconciler.Recorder = recorder

	answered, _ := reconcileSchemaOnce(t, reconciler, api, schema)

	if answered.Status.LastReadRequest != "now please" || !answered.Status.NextReconciliationTime.Time.Equal(readRequestClock.Add(time.Hour)) {
		t.Fatalf("lastReadRequest=%q next=%v, want the value recorded and the interval kept",
			answered.Status.LastReadRequest, answered.Status.NextReconciliationTime)
	}
	if event := findEvent(recorder, corev1.EventTypeWarning+" ReadRequestInvalid"); !strings.Contains(event, `"now please"`) {
		t.Fatalf("Warning Event = %q, want it to name the value", event)
	}
	again, _ := reconcileSchemaOnce(t, reconciler, api, answered)
	if again.Status.ActiveOperation != nil {
		t.Fatalf("a value that is not an instant started %#v", again.Status.ActiveOperation)
	}
	if event := findEvent(recorder, corev1.EventTypeWarning+" ReadRequestInvalid"); event != "" {
		t.Fatalf("the same value was reported twice: %q", event)
	}
}

// A request on a plan that waits for approval refreshes the evidence the
// approval is checked against, as the interval does, and keeps the plan.
func TestASchemaReadRequestKeepsThePlanThatWaitsForApproval(t *testing.T) {
	t.Parallel()
	schema := blockedPolicySchema(operatorv1alpha1.ApplyPolicyOnApproval, false)
	schema.Spec.Interval = metav1.Duration{Duration: time.Hour}
	schema.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	next := metav1.NewTime(readRequestClock.Add(time.Hour))
	schema.Status.NextReconciliationTime = &next
	wantPlan := schema.Status.Plan.DeepCopy()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)

	answered, _ := reconcileSchemaOnce(t, reconciler, api, schema)
	claimed, _ := reconcileSchemaOnce(t, reconciler, api, answered)

	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("a request on a waiting plan claimed %#v, want the refreshing Resolve", claimed.Status.ActiveOperation)
	}
	if claimed.Status.Plan == nil || claimed.Status.Plan.Name != wantPlan.Name || claimed.Status.Plan.UID != wantPlan.UID {
		t.Fatalf("the request retired the plan: %#v", claimed.Status.Plan)
	}
}

func requestedMigration(phase operatorv1alpha1.MigrationPhase) *operatorv1alpha1.PtahMigration {
	migration := migrationFixture()
	migration.Spec.Interval = metav1.Duration{Duration: time.Hour}
	migration.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = phase
	next := metav1.NewTime(readRequestClock.Add(time.Hour))
	migration.Status.NextReconciliationTime = &next
	return migration
}

func TestAMigrationReadRequestBringsTheHistoryReadForward(t *testing.T) {
	t.Parallel()
	migration := requestedMigration(operatorv1alpha1.MigrationPhaseInSync)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	answered := readMigration(t, api, migration)
	if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil ||
		answered.Status.NextReconciliationTime == nil || !answered.Status.NextReconciliationTime.Time.Equal(readRequestClock) {
		t.Fatalf("after the request: lastReadRequest=%q next=%v activeOperation=%#v",
			answered.Status.LastReadRequest, answered.Status.NextReconciliationTime, answered.Status.ActiveOperation)
	}
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(answered)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	claimed := readMigration(t, api, migration)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("the pass after the request claimed %#v, want the interval's Resolve", claimed.Status.ActiveOperation)
	}
}

func TestAMigrationReadRequestWaitsForAReadThatStartsAfterIt(t *testing.T) {
	t.Parallel()
	for _, phase := range []operatorv1alpha1.MigrationPhase{
		operatorv1alpha1.MigrationPhaseReading, operatorv1alpha1.MigrationPhaseFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			migration := requestedMigration(phase)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			if actual := readMigration(t, api, migration); actual.Status.LastReadRequest != "" {
				t.Fatalf("phase %s answered the request: %q", phase, actual.Status.LastReadRequest)
			}
		})
	}
}
