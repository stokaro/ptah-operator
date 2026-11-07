package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// errFailedRead is what the attempt these fixtures failed said. The controller
// writes the retry the test then reads, so a fixture cannot describe a state
// the controller never stores.
var errFailedRead = errors.New("desired_source_unreadable: expected '(' after table name")

// retryDelay is the retry delay every fixture here waits out, and refreshPeriod
// the interval: far enough apart that a test can stand on either side of one
// without crossing the other.
const (
	retryDelay    = time.Minute
	refreshPeriod = time.Hour
)

// failedConfigMapSchemaRead is a schema whose source was read from a ConfigMap
// and whose Observe failed once. The reconciler made every step of it, and the
// retry delay has not passed.
func failedConfigMapSchemaRead(t *testing.T) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
	t.Helper()
	schema := configMapSchemaFixture()
	schema.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE orders PARTITION OF nothing;\n"))
	if resolved := reconcileSchema(t, reconciler, api, schema); resolved.Status.Phase != operatorv1alpha1.PhaseObserving {
		t.Fatalf("the fixture did not read its source: phase %s", resolved.Status.Phase)
	}
	claimed := reconcileSchema(t, reconciler, api, schema)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve {
		t.Fatalf("the fixture did not claim its Observe: %#v", claimed.Status.ActiveOperation)
	}
	if _, err := reconciler.retryOperation(context.Background(), claimed, nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	failed := storedSchema(t, api, schema)
	if failed.Status.Phase != operatorv1alpha1.PhaseFailed || failed.Status.ActiveOperation == nil ||
		failed.Status.ActiveOperation.Attempt != 2 || failed.Status.ActiveOperation.JobUID != "" {
		t.Fatalf("the fixture is not a claim between attempts: %#v", failed.Status)
	}
	return failed, reconciler, api
}

// failedOCISchemaRead is a schema whose Observe failed once, reading an OCI
// artifact resolved resolvedAgo before the clock. A Git source reads the same
// fields.
func failedOCISchemaRead(t *testing.T, resolvedAgo time.Duration) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
	t.Helper()
	schema := readOnlySchemaClaim(t, operatorv1alpha1.OperationObserve)
	schema.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	schema.Status.ActiveOperation.LeaseEpoch = ""
	resolved := metav1.NewTime(readRequestClock.Add(-resolvedAgo))
	schema.Status.Source.ResolvedAt = &resolved
	schema.Status.Source.VerificationPolicyUID = testPolicyUID
	schema.Status.Source.VerificationPolicyDigest = fingerprint.DigestBytes([]byte("policy"))
	bindActiveInput(t, schema)
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policy)
	if _, err := reconciler.retryOperation(context.Background(), storedSchema(t, api, schema), nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	return storedSchema(t, api, schema), reconciler, api
}

func storedSchema(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.PtahSchema {
	t.Helper()
	stored := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// endRetryDelay moves the clock past the delay a failed attempt asked for.
func endRetryDelay(t *testing.T, reconciler *SchemaReconciler, schema *operatorv1alpha1.PtahSchema) {
	t.Helper()
	next := schema.Status.NextReconciliationTime
	if next == nil {
		t.Fatal("the fixture carries no retry deadline")
	}
	reached := next.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
}

// assertClaimStands holds a reconcile pass to leaving the claim it was given:
// the same operation, on the same source.
func assertClaimStands(t *testing.T, actual, want *operatorv1alpha1.PtahSchema) {
	t.Helper()
	got := actual.Status.ActiveOperation
	if got == nil || got.ID != want.Status.ActiveOperation.ID || got.Type != want.Status.ActiveOperation.Type ||
		got.InputFingerprint != want.Status.ActiveOperation.InputFingerprint {
		t.Fatalf("the claim did not stand: got %#v, want %#v", got, want.Status.ActiveOperation)
	}
	if digest, wantDigest := sourceDigestOf(t, actual), sourceDigestOf(t, want); digest != wantDigest {
		t.Fatalf("the source moved under a claim that stands: %#v, want %#v", actual.Status.Source, want.Status.Source)
	}
}

func sourceDigestOf(t *testing.T, schema *operatorv1alpha1.PtahSchema) string {
	t.Helper()
	digest, err := fingerprint.DigestCanonicalJSON(schema.Status.Source)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// annotateSchema writes a read request the way kubectl ptah refresh does: on the
// main resource, leaving status to the manager.
func annotateSchema(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, value string) {
	t.Helper()
	stored := storedSchema(t, api, schema)
	stored.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: value}
	if err := api.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

// The reproduction of stokaro/ptah-operator#839. A file the Observe could not
// read is repaired by an edit, and nothing else happens: no spec edit, no
// suspension. The edit wakes the schema, the failed claim gives way at once, and
// the next pass copies the repaired file into a new snapshot.
func TestAFailedReadGivesWayToAnEditedConfigMap(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedConfigMapSchemaRead(t)
	first := failed.Status.Source.Snapshot.DeepCopy()

	// Nothing moved, so this is the claim waiting out its delay.
	assertClaimStands(t, reconcileSchema(t, reconciler, api, failed), failed)

	editSource(t, api, func(source *corev1.ConfigMap) { source.Data["schema.sql"] = "CREATE TABLE orders (id int);\n" })

	gaveWay := reconcileSchema(t, reconciler, api, failed)
	if gaveWay.Status.ActiveOperation != nil || gaveWay.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("an edited source left phase %s, operation %#v; want Pending and no claim",
			gaveWay.Status.Phase, gaveWay.Status.ActiveOperation)
	}
	reread := reconcileSchema(t, reconciler, api, failed)
	if reread.Status.Phase != operatorv1alpha1.PhaseObserving || reread.Status.Source.Snapshot == nil {
		t.Fatalf("the source was not read again: phase %s, source %#v", reread.Status.Phase, reread.Status.Source)
	}
	if reread.Status.Source.Snapshot.ContentDigest == first.ContentDigest {
		t.Fatalf("the snapshot still holds the file the attempt could not read: %#v", reread.Status.Source.Snapshot)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("the failed claim dispatched on its old snapshot: %d Jobs, %v", len(jobs.Items), err)
	}
}

// An edit to a key the source does not select moves the ConfigMap and not the
// snapshot. The claim gives way as it would for any edit, and the Resolve that
// follows finds the same content and keeps the plan that waits for approval.
func TestAFailedReadKeepsThePlanWhenTheSelectedFilesDidNotChange(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedConfigMapSchemaRead(t)
	stored := failed.DeepCopy()
	binding := stored.Status.ExecutionBinding
	stored.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "waiting-plan", UID: "waiting-plan-uid", ExecutionBindingID: binding.Epoch,
		ControllerStateVersion: binding.ControllerStateVersion, PtahVersion: binding.PtahVersion,
		ExecutorImage: binding.ExecutorImage, RunnerProtocolVersion: binding.RunnerProtocolVersion,
	}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	snapshot := stored.Status.Source.Snapshot.DeepCopy()

	editSource(t, api, func(source *corev1.ConfigMap) { source.Data["README"] = "another note\n" })

	gaveWay := reconcileSchema(t, reconciler, api, failed)
	if gaveWay.Status.ActiveOperation != nil || gaveWay.Status.Plan == nil || gaveWay.Status.Plan.Name != "waiting-plan" {
		t.Fatalf("giving way left operation %#v, plan %#v; want no claim and the plan kept", gaveWay.Status.ActiveOperation, gaveWay.Status.Plan)
	}
	reread := reconcileSchema(t, reconciler, api, failed)
	if reread.Status.Plan == nil || reread.Status.Plan.Name != "waiting-plan" || reread.Status.Source.Snapshot == nil ||
		*reread.Status.Source.Snapshot != *snapshot {
		t.Fatalf("the same files retired the plan or the snapshot: plan %#v, snapshot %#v", reread.Status.Plan, reread.Status.Source.Snapshot)
	}
}

// A source policy is read the moment it changes, with a claim or without one.
func TestAFailedReadGivesWayToAMovedSourcePolicy(t *testing.T) {
	t.Parallel()
	schema := configMapSchemaFixture()
	schema.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	withSourcePolicy(&schema.Spec.Desired, "source-policy")
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE a (id int);\n"),
		sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 1000\n"))
	reconcileSchema(t, reconciler, api, schema)
	claimed := reconcileSchema(t, reconciler, api, schema)
	if claimed.Status.ActiveOperation == nil {
		t.Fatalf("the fixture did not claim its Observe: %#v", claimed.Status)
	}
	if _, err := reconciler.retryOperation(context.Background(), claimed, nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	if waiting := reconcileSchema(t, reconciler, api, schema); waiting.Status.ActiveOperation == nil {
		t.Fatal("an unmoved policy retired the claim")
	}
	old := &corev1.ConfigMap{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "source-policy"}, old); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(context.Background(), sourcePolicyConfigMap("source-policy", "new-policy-uid", "maxContentBytes: 2000\n")); err != nil {
		t.Fatal(err)
	}
	if gaveWay := reconcileSchema(t, reconciler, api, schema); gaveWay.Status.ActiveOperation != nil {
		t.Fatalf("a replaced source policy left the claim %#v", gaveWay.Status.ActiveOperation)
	}
}

// A commit or a digest is only seen moved by reading it, so an OCI or a Git
// source gets the read its interval owes it. The retry delay is the pace: a
// claim past its interval and inside its delay waits, and one inside its
// interval runs again on what it holds.
func TestAFailedReadRereadsAMovableSourceAtItsInterval(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		resolvedAgo time.Duration
		delayEnded  bool
		gaveWay     bool
	}{
		{name: "inside the delay and past the interval", resolvedAgo: 2 * refreshPeriod},
		{name: "past the delay and inside the interval", resolvedAgo: refreshPeriod / 2, delayEnded: true},
		{name: "past the delay and past the interval", resolvedAgo: 2 * refreshPeriod, delayEnded: true, gaveWay: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			failed, reconciler, api := failedOCISchemaRead(t, row.resolvedAgo)
			if row.delayEnded {
				endRetryDelay(t, reconciler, failed)
			}
			actual := reconcileSchema(t, reconciler, api, failed)
			if !row.gaveWay {
				assertClaimStands(t, actual, failed)
				return
			}
			if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhasePending {
				t.Fatalf("phase %s, operation %#v; want Pending and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
			}
			next := reconcileSchema(t, reconciler, api, failed)
			if next.Status.ActiveOperation == nil || next.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("the pass after it claimed %#v, want a Resolve", next.Status.ActiveOperation)
			}
		})
	}
}

// A Resolve reads the source on every attempt, so a moved source or a passed
// interval asks nothing of it, and giving way to another Resolve would only skip
// the retry delay. A person's request still ends it: the next read after the
// request is what answers it, and a claim that stands answers nothing.
func TestAFailedResolveGivesWayOnlyToARequest(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	resolved := metav1.NewTime(readRequestClock.Add(-2 * refreshPeriod))
	schema.Status.Source.ResolvedAt = &resolved
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "resolve-operation", JobName: "resolve-job",
		StartedAt: metav1.NewTime(readRequestClock), Attempt: 1, ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	bindActiveInput(t, schema)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	if _, err := reconciler.retryOperation(context.Background(), storedSchema(t, api, schema), nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	failed := storedSchema(t, api, schema)
	endRetryDelay(t, reconciler, failed)

	if reason, err := reconciler.failedSchemaReadGivesWay(context.Background(), failed, reconciler.now()); err != nil || reason != "" {
		t.Fatalf("a failed Resolve gave way to its own interval: %q, %v", reason, err)
	}

	annotateSchema(t, api, schema, requestedAt)
	if actual := reconcileSchema(t, reconciler, api, failed); actual.Status.ActiveOperation != nil {
		t.Fatalf("a failed Resolve left a person's request unanswered: operation %#v", actual.Status.ActiveOperation)
	}
}

// kubectl ptah refresh while a read fails. A request is answered once no
// operation is running and no retry delay is pending, and a failed read is what
// used to keep the first of those from ever coming true.
func TestAReadRequestIsAnsweredBetweenFailedAttempts(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedOCISchemaRead(t, refreshPeriod/2)
	annotateSchema(t, api, failed, requestedAt)

	waiting := reconcileSchema(t, reconciler, api, failed)
	assertClaimStands(t, waiting, failed)
	if waiting.Status.LastReadRequest != "" {
		t.Fatalf("the request was answered inside the retry delay: %q", waiting.Status.LastReadRequest)
	}

	endRetryDelay(t, reconciler, failed)
	gaveWay := reconcileSchema(t, reconciler, api, failed)
	if gaveWay.Status.ActiveOperation != nil {
		t.Fatalf("a request past the delay left the claim %#v", gaveWay.Status.ActiveOperation)
	}
	answered := reconcileSchema(t, reconciler, api, failed)
	if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
		t.Fatalf("lastReadRequest %q, operation %#v; want the request answered before the read starts",
			answered.Status.LastReadRequest, answered.Status.ActiveOperation)
	}
	claimed := reconcileSchema(t, reconciler, api, failed)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("the answered request claimed %#v, want a Resolve", claimed.Status.ActiveOperation)
	}
}

// What kubectl ptah refresh and its wait see. The request is written while the
// read fails, and the wait reports the operator has not taken it until the retry
// delay ends. From then on the wait is about the read and not about the request.
func TestRefreshWhileAReadFailsIsTakenWhenTheDelayEnds(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedOCISchemaRead(t, refreshPeriod/2)
	ref := operate.Ref{Family: operate.FamilySchema, Namespace: failed.Namespace, Name: failed.Name}
	refresh, err := operate.Refresh(context.Background(), api, ref, reconciler.now())
	if err != nil {
		t.Fatal(err)
	}
	waiting := func() string {
		t.Helper()
		_, waitErr := operate.Wait(context.Background(), api, ref, operate.WaitOptions{
			Target: operate.TargetRead, Timeout: time.Minute, Since: refresh.Object, ReadRequest: refresh.Request,
			Clock: &waitClock{now: readRequestClock},
		})
		timeout := &operate.TimeoutError{}
		if !errors.As(waitErr, &timeout) {
			t.Fatalf("wait = %v, want it to time out on the read in flight", waitErr)
		}
		return timeout.Waiting
	}

	reconcileSchema(t, reconciler, api, failed)
	const notTaken = "the operator to take read request"
	if said := waiting(); !strings.Contains(said, notTaken) {
		t.Fatalf("inside the retry delay the wait says %q, want it waiting for the operator to take the request", said)
	}

	endRetryDelay(t, reconciler, failed)
	for range 3 {
		reconcileSchema(t, reconciler, api, failed)
	}
	if said := waiting(); strings.Contains(said, notTaken) {
		t.Fatalf("after the retry delay the wait says %q, want the request taken", said)
	}
	if actual := storedSchema(t, api, failed); actual.Status.LastReadRequest != operatorv1alpha1.ReadRequestToken(refresh.Request) {
		t.Fatalf("lastReadRequest = %q, want %q", actual.Status.LastReadRequest, refresh.Request)
	}
}

// A value that is not an instant starts nothing, so it cannot end a claim, and
// nothing has to end for the operator to say so. A failing read answers it as a
// resource at rest does: recorded as answered, with one Warning Event, inside the
// retry delay and after it.
func TestAReadRequestThatIsNotAnInstantIsAnsweredWhileAReadFails(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedOCISchemaRead(t, refreshPeriod/2)
	recorder := record.NewFakeRecorder(8)
	reconciler.Recorder = recorder
	annotateSchema(t, api, failed, "now please")

	answered := reconcileSchema(t, reconciler, api, failed)
	assertClaimStands(t, answered, failed)
	if answered.Status.LastReadRequest != "now please" {
		t.Fatalf("lastReadRequest = %q, want the value recorded", answered.Status.LastReadRequest)
	}
	if !answered.Status.NextReconciliationTime.Equal(failed.Status.NextReconciliationTime) {
		t.Fatalf("next = %v, want the retry deadline %v kept", answered.Status.NextReconciliationTime, failed.Status.NextReconciliationTime)
	}
	if event := findEvent(recorder, corev1.EventTypeWarning+" ReadRequestInvalid"); !strings.Contains(event, `"now please"`) {
		t.Fatalf("Warning Event = %q, want it to name the value", event)
	}

	endRetryDelay(t, reconciler, failed)
	assertClaimStands(t, reconcileSchema(t, reconciler, api, failed), failed)
	if event := findEvent(recorder, corev1.EventTypeWarning+" ReadRequestInvalid"); event != "" {
		t.Fatalf("the same value was reported twice: %q", event)
	}
}

// The read that proves an Apply keeps its binding, and an answer leaves it
// alone. An Apply between attempts is not a read that fails, so the filter
// refuses it, and the request waits for the claim to end as it always did.
func TestAReadRequestThatIsNotAnInstantWhileAClaimOwesItsBinding(t *testing.T) {
	t.Parallel()
	t.Run("the read that proves an Apply answers it", func(t *testing.T) {
		t.Parallel()
		schema := safetyPostApplyObserveSchema(t)
		schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		if _, err := reconciler.retryOperation(context.Background(), storedSchema(t, api, schema), nil, errFailedRead); err != nil {
			t.Fatal(err)
		}
		failed := storedSchema(t, api, schema)
		annotateSchema(t, api, failed, "now please")

		actual := reconcileSchema(t, reconciler, api, schema)

		assertClaimStands(t, actual, failed)
		if actual.Status.LastReadRequest != "now please" {
			t.Fatalf("lastReadRequest = %q, want the value recorded", actual.Status.LastReadRequest)
		}
		if actual.Status.PendingObservation == nil || actual.Status.PendingObservation.ApplyOperationID != "apply-operation" {
			t.Fatalf("the proof was lost: %#v", actual.Status.PendingObservation)
		}
	})
	t.Run("an Apply does not", func(t *testing.T) {
		t.Parallel()
		schema := safetyApplySchema(t)
		schema.Status.Phase = operatorv1alpha1.PhaseFailed
		schema.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: "now please"}
		if _, unreadable := unreadableRequestOfFailedSchemaRead(schema); unreadable {
			t.Fatal("an Apply claim would answer the request")
		}
	})
	t.Run("an instant does not: it gives way when the delay ends", func(t *testing.T) {
		t.Parallel()
		failed, _, api := failedOCISchemaRead(t, refreshPeriod/2)
		annotateSchema(t, api, failed, requestedAt)
		if _, unreadable := unreadableRequestOfFailedSchemaRead(storedSchema(t, api, failed)); unreadable {
			t.Fatal("an instant would be answered as a value that starts nothing")
		}
	})
}

// What keeps its binding, each against a source edited under it. Each row is a
// claim the filter must refuse, beside the rows above that it accepts.
func TestAClaimThatOwesItsBindingKeepsItWhenTheSourceMoves(t *testing.T) {
	t.Parallel()
	source := func(t *testing.T, schema *operatorv1alpha1.PtahSchema) {
		t.Helper()
		schema.Spec.Desired = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
			Name:  "app-schema",
			Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "schema.sql"}},
		}}
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{
			Name: "app-schema", UID: "configmap-uid", ResourceVersion: "read-before-the-edit",
		}}
		schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	}
	edit := func(configMap *corev1.ConfigMap) { configMap.Data["schema.sql"] = "CREATE TABLE b (id int);\n" }

	t.Run("an Apply", func(t *testing.T) {
		t.Parallel()
		schema := safetyApplySchema(t)
		source(t, schema)
		schema.Status.Phase = operatorv1alpha1.PhaseFailed
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE a (id int);\n"))
		reason, err := reconciler.failedSchemaReadGivesWay(context.Background(), storedSchema(t, api, schema), reconciler.now())
		if err != nil || reason != "" {
			t.Fatalf("an Apply claim gave way: %q, %v", reason, err)
		}
	})

	t.Run("the read that proves an Apply", func(t *testing.T) {
		t.Parallel()
		schema := safetyPostApplyObserveSchema(t)
		source(t, schema)
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE a (id int);\n"))
		if _, err := reconciler.retryOperation(context.Background(), storedSchema(t, api, schema), nil, errFailedRead); err != nil {
			t.Fatal(err)
		}
		failed := storedSchema(t, api, schema)
		if failed.Status.Phase != operatorv1alpha1.PhaseFailed || failed.Status.ActiveOperation.JobUID != "" {
			t.Fatalf("the fixture is not a claim between attempts: %#v", failed.Status)
		}
		actual := reconcileSchema(t, reconciler, api, schema)
		assertClaimStands(t, actual, failed)
		if actual.Status.PendingObservation == nil || actual.Status.PendingObservation.ApplyOperationID != "apply-operation" {
			t.Fatalf("the proof was lost: %#v", actual.Status.PendingObservation)
		}
	})

	t.Run("an attempt whose Job exists and whose UID was not recorded", func(t *testing.T) {
		t.Parallel()
		failed, reconciler, api := failedConfigMapSchemaRead(t)
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Namespace: failed.Namespace, Name: failed.Status.ActiveOperation.JobName, UID: "lost-write-uid",
		}}
		if err := api.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		editSource(t, api, edit)
		assertClaimStands(t, reconcileSchema(t, reconciler, api, failed), failed)
	})

	t.Run("an attempt whose recorded Job is gone", func(t *testing.T) {
		t.Parallel()
		failed, reconciler, api := failedConfigMapSchemaRead(t)
		stored := failed.DeepCopy()
		stored.Status.ActiveOperation.JobUID = "gone-uid"
		if err := api.Status().Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		editSource(t, api, edit)
		endRetryDelay(t, reconciler, failed)
		actual := reconcileSchema(t, reconciler, api, failed)
		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.ID != failed.Status.ActiveOperation.ID {
			t.Fatalf("a dispatched attempt gave way before its Pods were known to have stopped: %#v", actual.Status.ActiveOperation)
		}
	})

	t.Run("a suspended resource, which retires the claim as a suspension does", func(t *testing.T) {
		t.Parallel()
		failed, reconciler, api := failedConfigMapSchemaRead(t)
		suspended := failed.DeepCopy()
		suspended.Spec.Suspend = true
		if err := api.Update(context.Background(), suspended); err != nil {
			t.Fatal(err)
		}
		editSource(t, api, edit)
		if actual := reconcileSchema(t, reconciler, api, failed); actual.Status.Phase != operatorv1alpha1.PhaseSuspended {
			t.Fatalf("phase %s, want Suspended", actual.Status.Phase)
		}
	})
}

// failedConfigMapMigrationRead is a migration whose history read of a ConfigMap
// source failed once. The reconciler made every step of it.
func failedConfigMapMigrationRead(t *testing.T) (*operatorv1alpha1.PtahMigration, *MigrationReconciler, client.Client) {
	t.Helper()
	migration := migrationFixture()
	migration.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	migration.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
		Name:  "app-schema",
		Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "0001.up.sql", Path: "0001_init.up.sql"}},
	}}
	source := sourceConfigMap("unused")
	source.Data = map[string]string{"0001.up.sql": "-- +ptah no_transaction=sometimes\nCREATE TABLE a (id int);\n"}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, source)
	for range 3 {
		reconcileMigration(t, reconciler, api, migration)
	}
	claimed := readMigration(t, api, migration)
	if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationHistory {
		t.Fatalf("the fixture did not claim its history read: %#v", claimed.Status)
	}
	if _, err := reconciler.retryMigrationOperation(context.Background(), claimed, nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	failed := readMigration(t, api, migration)
	if failed.Status.ActiveOperation == nil || failed.Status.ActiveOperation.RetryNotBefore == nil || failed.Status.ActiveOperation.JobUID != "" {
		t.Fatalf("the fixture is not a claim between attempts: %#v", failed.Status)
	}
	return failed, reconciler, api
}

func reconcileMigration(t *testing.T, reconciler *MigrationReconciler, api client.Client, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigration {
	t.Helper()
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return readMigration(t, api, migration)
}

func endMigrationRetryDelay(t *testing.T, reconciler *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) {
	t.Helper()
	notBefore := migration.Status.ActiveOperation.RetryNotBefore
	if notBefore == nil {
		t.Fatal("the fixture carries no retry deadline")
	}
	reached := notBefore.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
}

func TestAFailedMigrationReadGivesWayToAnEditedConfigMap(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedConfigMapMigrationRead(t)
	first := failed.Status.Artifact.Snapshot.DeepCopy()

	waiting := reconcileMigration(t, reconciler, api, failed)
	if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != failed.Status.ActiveOperation.ID {
		t.Fatalf("an unmoved source retired the claim: %#v", waiting.Status.ActiveOperation)
	}

	editSource(t, api, func(source *corev1.ConfigMap) { source.Data["0001.up.sql"] = "CREATE TABLE a (id int);\n" })

	gaveWay := reconcileMigration(t, reconciler, api, failed)
	if gaveWay.Status.ActiveOperation != nil || gaveWay.Status.Phase != operatorv1alpha1.MigrationPhasePending {
		t.Fatalf("an edited source left phase %s, operation %#v; want Pending and no claim",
			gaveWay.Status.Phase, gaveWay.Status.ActiveOperation)
	}
	reread := reconcileMigration(t, reconciler, api, failed)
	if reread.Status.Phase != operatorv1alpha1.MigrationPhaseReading || reread.Status.Artifact == nil ||
		reread.Status.Artifact.Snapshot == nil || reread.Status.Artifact.Snapshot.ContentDigest == first.ContentDigest {
		t.Fatalf("the source was not read again: phase %s, artifact %#v", reread.Status.Phase, reread.Status.Artifact)
	}
}

// failedOCIMigrationRead is a migration whose operation failed once, with its
// next read of the source nextReadIn from the clock.
func failedOCIMigrationRead(
	t *testing.T,
	nextReadIn time.Duration,
	operationType operatorv1alpha1.MigrationOperationType,
) (*operatorv1alpha1.PtahMigration, *MigrationReconciler, client.Client) {
	t.Helper()
	migration := migrationFixture()
	migration.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	migration.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	migration.Finalizers = []string{migrationOperationFinalizer}
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	next := metav1.NewTime(readRequestClock.Add(nextReadIn))
	migration.Status.NextReconciliationTime = &next
	migrationClaim(t, migration, operationType)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap())
	if _, err := reconciler.retryMigrationOperation(context.Background(), readMigration(t, api, migration), nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	return readMigration(t, api, migration), reconciler, api
}

func TestAFailedMigrationReadRereadsAMovableSourceAtItsInterval(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		nextReadIn time.Duration
		delayEnded bool
		gaveWay    bool
	}{
		{name: "inside the delay and past the interval", nextReadIn: -time.Hour},
		{name: "past the delay and inside the interval", nextReadIn: 30 * time.Minute, delayEnded: true},
		{name: "past the delay and past the interval", nextReadIn: -time.Hour, delayEnded: true, gaveWay: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			failed, reconciler, api := failedOCIMigrationRead(t, row.nextReadIn, operatorv1alpha1.MigrationOperationHistory)
			if row.delayEnded {
				endMigrationRetryDelay(t, reconciler, failed)
			}
			actual := reconcileMigration(t, reconciler, api, failed)
			if !row.gaveWay {
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.ID != failed.Status.ActiveOperation.ID {
					t.Fatalf("the claim did not stand: %#v", actual.Status.ActiveOperation)
				}
				return
			}
			if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhasePending {
				t.Fatalf("phase %s, operation %#v; want Pending and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
			}
			next := reconcileMigration(t, reconciler, api, failed)
			if next.Status.ActiveOperation == nil || next.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationResolve {
				t.Fatalf("the pass after it claimed %#v, want a Resolve", next.Status.ActiveOperation)
			}
		})
	}
}

// A ConfigMap says when it moved, so a read of one has no interval to give way
// to: past both the retry delay and the interval, it runs again on what it holds.
func TestAFailedReadOfAConfigMapGivesWayToNoInterval(t *testing.T) {
	t.Parallel()
	failed, reconciler, _ := failedConfigMapSchemaRead(t)
	reached := reconciler.now().Add(2 * refreshPeriod)
	if reason, err := reconciler.failedSchemaReadGivesWay(context.Background(), failed, reached); err != nil || reason != "" {
		t.Fatalf("a schema's read of an unmoved ConfigMap gave way: %q, %v", reason, err)
	}

	failedMigration, migrations, _ := failedConfigMapMigrationRead(t)
	later := migrations.now().Add(2 * refreshPeriod)
	migrations.Clock = func() time.Time { return later }
	if reason, err := migrations.failedMigrationReadGivesWay(context.Background(), failedMigration); err != nil || reason != "" {
		t.Fatalf("a migration's read of an unmoved ConfigMap gave way: %q, %v", reason, err)
	}
}

// A failed migration Resolve reads the source on every attempt, as a schema's
// does, so its own interval asks nothing of it.
func TestAFailedMigrationResolveDoesNotGiveWayToItsOwnInterval(t *testing.T) {
	t.Parallel()
	failed, reconciler, _ := failedOCIMigrationRead(t, -time.Hour, operatorv1alpha1.MigrationOperationResolve)
	endMigrationRetryDelay(t, reconciler, failed)
	if reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), failed); err != nil || reason != "" {
		t.Fatalf("a failed Resolve gave way to its own interval: %q, %v", reason, err)
	}
}

func TestAFailedMigrationReadGivesWayToAMovedSourcePolicy(t *testing.T) {
	t.Parallel()
	failed, reconciler, api := failedConfigMapMigrationRead(t)
	if reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), failed); err != nil || reason != "" {
		t.Fatalf("an unmoved policy gave way: %q, %v", reason, err)
	}
	withSourcePolicy(&failed.Spec.Artifact, "source-policy")
	if err := api.Create(context.Background(), sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 2000\n")); err != nil {
		t.Fatal(err)
	}
	if reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), failed); err != nil || reason != failedReadSourceMoved {
		t.Fatalf("a moved policy did not end the claim: %q, %v", reason, err)
	}
}

// The history read after an Apply has no refresh deadline. Its first failure
// starts the interval, so one that keeps failing still gets the read its source
// is owed, and no sooner.
func TestAFailedReadWithNoDeadlineStartsTheIntervalAtItsFirstFailure(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	migration.Spec.Interval = metav1.Duration{Duration: refreshPeriod}
	migration.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
	migration.Finalizers = []string{migrationOperationFinalizer}
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifyingHistory
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap())
	if _, err := reconciler.retryMigrationOperation(context.Background(), readMigration(t, api, migration), nil, errFailedRead); err != nil {
		t.Fatal(err)
	}
	failed := readMigration(t, api, migration)
	if next := failed.Status.NextReconciliationTime; next == nil || !next.Time.Equal(reconciler.now().Add(refreshPeriod)) {
		t.Fatalf("next = %v, want an interval from the failure", next)
	}
	endMigrationRetryDelay(t, reconciler, failed)
	if reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), failed); err != nil || reason != "" {
		t.Fatalf("the read gave way before its interval: %q, %v", reason, err)
	}
	reached := failed.Status.NextReconciliationTime.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
	if reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), failed); err != nil || reason != failedReadIntervalEnded {
		t.Fatalf("the read did not give way past its interval: %q, %v", reason, err)
	}
}

func TestAMigrationReadRequestIsAnsweredBetweenFailedAttempts(t *testing.T) {
	t.Parallel()
	for _, operationType := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationResolve,
	} {
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()
			failed, reconciler, api := failedOCIMigrationRead(t, 30*time.Minute, operationType)
			annotated := failed.DeepCopy()
			annotated.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
			if err := api.Update(context.Background(), annotated); err != nil {
				t.Fatal(err)
			}

			waiting := reconcileMigration(t, reconciler, api, failed)
			if waiting.Status.ActiveOperation == nil || waiting.Status.LastReadRequest != "" {
				t.Fatalf("the request was answered inside the retry delay: %q, %#v", waiting.Status.LastReadRequest, waiting.Status.ActiveOperation)
			}

			endMigrationRetryDelay(t, reconciler, failed)
			if gaveWay := reconcileMigration(t, reconciler, api, failed); gaveWay.Status.ActiveOperation != nil {
				t.Fatalf("a request past the delay left the claim %#v", gaveWay.Status.ActiveOperation)
			}
			answered := reconcileMigration(t, reconciler, api, failed)
			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
				t.Fatalf("lastReadRequest %q, operation %#v; want the request answered before the read starts",
					answered.Status.LastReadRequest, answered.Status.ActiveOperation)
			}
			claimed := reconcileMigration(t, reconciler, api, failed)
			if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationResolve {
				t.Fatalf("the answered request claimed %#v, want a Resolve", claimed.Status.ActiveOperation)
			}
		})
	}
}

// The same answer for a migration: at once, inside the retry delay, once, and
// the claim stands past the delay.
func TestAMigrationReadRequestThatIsNotAnInstantIsAnsweredWhileAReadFails(t *testing.T) {
	t.Parallel()
	for _, operationType := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationResolve,
	} {
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()
			failed, reconciler, api := failedOCIMigrationRead(t, 30*time.Minute, operationType)
			recorder := record.NewFakeRecorder(8)
			reconciler.Recorder = recorder
			annotated := failed.DeepCopy()
			annotated.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: "now please"}
			if err := api.Update(context.Background(), annotated); err != nil {
				t.Fatal(err)
			}
			stands := func(actual *operatorv1alpha1.PtahMigration) {
				t.Helper()
				got := actual.Status.ActiveOperation
				if got == nil || got.ID != failed.Status.ActiveOperation.ID || !got.RetryNotBefore.Equal(failed.Status.ActiveOperation.RetryNotBefore) {
					t.Fatalf("the claim did not stand: got %#v, want %#v", got, failed.Status.ActiveOperation)
				}
			}

			answered := reconcileMigration(t, reconciler, api, failed)
			stands(answered)
			if answered.Status.LastReadRequest != "now please" {
				t.Fatalf("lastReadRequest = %q, want the value recorded", answered.Status.LastReadRequest)
			}
			if event := findEvent(recorder, corev1.EventTypeWarning+" ReadRequestInvalid"); !strings.Contains(event, `"now please"`) {
				t.Fatalf("Warning Event = %q, want it to name the value", event)
			}

			again := reconcileMigration(t, reconciler, api, failed)
			stands(again)
			if event := findEvent(recorder, corev1.EventTypeWarning+" ReadRequestInvalid"); event != "" {
				t.Fatalf("the same value was reported twice: %q", event)
			}
		})
	}
	t.Run("an Apply does not answer it", func(t *testing.T) {
		t.Parallel()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		operation.DispatchStarted = false
		notBefore := metav1.NewTime(readRequestClock.Add(retryDelay))
		operation.RetryNotBefore = &notBefore
		migration.Status.ActiveOperation = operation
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseFailed
		migration.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: "now please"}
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

		actual := reconcileMigration(t, reconciler, api, migration)

		if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationApply {
			t.Fatalf("the Apply claim did not stand: %#v", actual.Status.ActiveOperation)
		}
		if actual.Status.LastReadRequest != "" {
			t.Fatalf("an Apply answered the request: %q", actual.Status.LastReadRequest)
		}
	})
}

// What a migration keeps: a claim on its first attempt has not failed, and an
// Apply is bound to what was approved. Each stands against a moved source.
func TestAMigrationClaimThatOwesItsBindingKeepsItWhenTheSourceMoves(t *testing.T) {
	t.Parallel()
	moved := func(migration *operatorv1alpha1.PtahMigration) {
		migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
			Name:  "app-schema",
			Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "0001_init.up.sql"}},
		}}
		migration.Status.ConfigMap = &operatorv1alpha1.ConfigMapSourceRevision{
			Name: "app-schema", UID: "configmap-uid", ResourceVersion: "read-before-the-edit",
		}
	}
	t.Run("an Apply", func(t *testing.T) {
		t.Parallel()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		operation.DispatchStarted = false
		notBefore := metav1.NewTime(readRequestClock.Add(retryDelay))
		operation.RetryNotBefore = &notBefore
		migration.Status.ActiveOperation = operation
		moved(migration)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap(), sourceConfigMap("x"))
		reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), readMigration(t, api, migration))
		if err != nil || reason != "" {
			t.Fatalf("an Apply claim gave way: %q, %v", reason, err)
		}
	})
	t.Run("a first attempt", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: retryDelay}
		migration.Finalizers = []string{migrationOperationFinalizer}
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		migration.Status.ObservedGeneration = migration.Generation
		migration.Status.Artifact = resolvedMigrationArtifact()
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
		migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
		moved(migration)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap(), sourceConfigMap("x"))
		reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), readMigration(t, api, migration))
		if err != nil || reason != "" {
			t.Fatalf("a claim that has not failed gave way: %q, %v", reason, err)
		}
	})
	t.Run("an attempt whose recorded Job is gone", func(t *testing.T) {
		t.Parallel()
		failed, reconciler, api := failedConfigMapMigrationRead(t)
		stored := failed.DeepCopy()
		stored.Status.ActiveOperation.JobUID = "gone-uid"
		if err := api.Status().Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		editSource(t, api, func(source *corev1.ConfigMap) { source.Data["0001.up.sql"] = "CREATE TABLE b (id int);\n" })
		reason, err := reconciler.failedMigrationReadGivesWay(context.Background(), readMigration(t, api, failed))
		if err != nil || reason != "" {
			t.Fatalf("a dispatched attempt gave way before its Pods were known to have stopped: %q, %v", reason, err)
		}
	})
}
