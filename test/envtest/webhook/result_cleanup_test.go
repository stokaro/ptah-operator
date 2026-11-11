package webhook_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	eventrecord "k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResultCollectorInTerminatingNamespace(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := chunkedPublicationPayload(t, identity)
	if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatal(err)
	}
	name, _ := resultstore.Name(identity.Binding)
	intent := &api.PtahResultRecord{ObjectMeta: metav1.ObjectMeta{Namespace: f.namespace, Name: name}}
	if err := admin.Delete(t.Context(), intent, client.PropagationPolicy(metav1.DeletePropagationForeground)); err == nil {
		t.Fatal("foreground retirement bypassed the active claim")
	}
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	if err := admin.Delete(t.Context(), intent, client.PropagationPolicy(metav1.DeletePropagationForeground)); err == nil {
		t.Fatal("foreground retirement bypassed the original Job")
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: f.namespace, Name: identity.Binding.JobName}}
	if err := admin.Delete(t.Context(), job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	if err := admin.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.namespace}}); err != nil {
		t.Fatal(err)
	}
	p := cleanupPolicy()
	p.Reader = namespaceReader{store.Reader, f.namespace}
	c, err := resultcleanup.New(store.Client, p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Step(t.Context()); err != nil {
		t.Fatalf("retire existing records after namespace deletion: %v", err)
	}
	if err := c.Step(t.Context()); err != nil {
		t.Fatalf("retire credentials after namespace deletion: %v", err)
	}
	list := &api.PtahResultRecordList{}
	if err := store.Reader.List(t.Context(), list, client.InNamespace(f.namespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 4 {
		t.Fatalf("retirement must preserve all bytes: %d records", len(list.Items))
	}
	for _, record := range list.Items {
		if record.Spec.Type == "intent" {
			if record.DeletionTimestamp.IsZero() || len(record.Finalizers) != 1 || record.Finalizers[0] != metav1.FinalizerDeleteDependents {
				t.Fatal("the API did not persist a foreground retirement timestamp")
			}
			withoutFence := record.DeepCopy()
			withoutFence.Finalizers = nil
			requireDenied(t, admin.Update(t.Context(), withoutFence), controllerWriteWebhook, windowDenial(&record))
		}
		if err := admin.Delete(t.Context(), &record); err == nil {
			t.Fatalf("early deletion of %s bypassed the retention window", record.Spec.Type)
		}
	}
	// Envtest has no garbage collector. Advance only the existing policy test
	// clock, then require the actual collector to remove the retained objects.
	cleanupClockOffset.Store(int64(2 * time.Hour))
	defer cleanupClockOffset.Store(0)
	p = cleanupPolicy()
	p.Reader = namespaceReader{store.Reader, f.namespace}
	c, err = resultcleanup.New(store.Client, p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Reader.List(t.Context(), list, client.InNamespace(f.namespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected only the two foreground roots after member collection: %d", len(list.Items))
	}
	// The real garbage collector is absent here. Submit its exact finalizer
	// removal in dependency order; admission must permit it only now.
	for _, kind := range []string{"intent", "credential"} {
		for _, record := range list.Items {
			if record.Spec.Type == kind {
				record.Finalizers = nil
				if err := admin.Update(t.Context(), &record); err != nil {
					t.Fatalf("eligible %s finalizer: %v", kind, err)
				}
			}
		}
	}
	if err := store.Reader.List(t.Context(), list, client.InNamespace(f.namespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("foreground completion left %d records", len(list.Items))
	}
}

// A ResourceQuota on result records that is full refuses the record that
// retires an attempt in a live namespace, and nothing could be collected
// without one, so the quota stayed full for good (#928). The collector starts
// the window with a foreground deletion instead, and the deletionTimestamp the
// API server stamps is the clock every later refusal names. The API server's
// quota admission and the chart's webhook decide here. Envtest runs no garbage
// collector, so the finalizer removal it would send is submitted by hand, in
// dependency order, as in the terminating namespace above.
func TestResultCollectorUnderAFullQuota(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := chunkedPublicationPayload(t, identity)
	if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatal(err)
	}
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: f.namespace, Name: identity.Binding.JobName}}
	if err := admin.Delete(t.Context(), job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	records := func() []api.PtahResultRecord {
		t.Helper()
		list := &api.PtahResultRecordList{}
		if err := store.Reader.List(t.Context(), list, client.InNamespace(f.namespace)); err != nil {
			t.Fatal(err)
		}
		return list.Items
	}
	published := records()
	if len(published) != 4 {
		t.Fatalf("the fixture published %d records, want an intent, a chunk, a completion and a credential", len(published))
	}
	intentName, _ := resultstore.Name(identity.Binding)
	var intent, newest *api.PtahResultRecord
	for i := range published {
		if published[i].Name == intentName {
			intent = &published[i]
		}
		if newest == nil || published[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &published[i]
		}
	}
	if intent == nil {
		t.Fatalf("no intent %s among the published records", intentName)
	}
	fillQuota(t, f.namespace, "results", len(published))
	retirement, err := resultretention.Record(identity.Binding, resultretention.Source{Name: intent.Name, UID: intent.UID, Type: "intent"}, resultretention.MinimumWindow)
	if err != nil {
		t.Fatal(err)
	}
	awaitQuotaRefusal(t, store.Client, retirement, "results")

	// Creation and deletion timestamps have one-second precision. Two seconds
	// apart, a window measured from the deletion ends at another instant than
	// one measured from the creation.
	time.Sleep(time.Until(newest.CreationTimestamp.Add(2 * time.Second)))
	broadcaster := eventrecord.NewBroadcaster()
	t.Cleanup(broadcaster.Shutdown)
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubernetes.NewForConfigOrDie(plane.Config).CoreV1().Events("")})
	p := cleanupPolicy()
	p.Reader = namespaceReader{store.Reader, f.namespace}
	c, err := resultcleanup.New(store.Client, p, nil, broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "ptah-result-collector"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"intent", "credential"} {
		if err := c.Step(t.Context()); err != nil {
			t.Fatalf("retire the %s under a full quota: %v", root, err)
		}
	}
	retained := records()
	if len(retained) != len(published) {
		t.Fatalf("%d records after retirement under a full quota, want the %d published", len(retained), len(published))
	}
	deletions := map[string]time.Time{}
	for _, record := range retained {
		if record.Spec.Type == "retired" {
			t.Fatalf("the quota admitted the retirement record %s", record.Name)
		}
		if record.Spec.Type != "intent" && record.Spec.Type != "credential" {
			continue
		}
		if record.DeletionTimestamp.IsZero() || !slices.Equal(record.Finalizers, []string{metav1.FinalizerDeleteDependents}) {
			t.Fatalf("the %s was not retired by a foreground deletion: deletion=%v finalizers=%v", record.Spec.Type, record.DeletionTimestamp, record.Finalizers)
		}
		if !record.DeletionTimestamp.After(record.CreationTimestamp.Add(time.Second)) {
			t.Fatalf("the %s was deleted at %v and created at %v, too close to tell their windows apart", record.Spec.Type, record.DeletionTimestamp, record.CreationTimestamp)
		}
		deletions[record.Spec.Type] = record.DeletionTimestamp.Time
	}
	if len(deletions) != 2 {
		t.Fatalf("found the deleting roots %v, want the intent and the credential", deletions)
	}
	defer cleanupClockOffset.Store(0)
	for _, record := range retained {
		// Past the window that runs from the record's creation and inside the
		// one that runs from the API server's deletion of its root.
		root := deletions[record.Spec.Type]
		if record.Spec.Type != "credential" {
			root = deletions["intent"]
		}
		cleanupClockOffset.Store(int64(time.Until(record.CreationTimestamp.Add(resultretention.MinimumWindow + time.Second))))
		want := "record " + record.Name + " cannot be deleted before " + root.Add(resultretention.MinimumWindow).UTC().Format(time.RFC3339)
		requireDenied(t, admin.Delete(t.Context(), &record, client.DryRunAll), controllerWriteWebhook, want)
		if !record.DeletionTimestamp.IsZero() {
			withoutFence := record.DeepCopy()
			withoutFence.Finalizers = nil
			requireDenied(t, admin.Update(t.Context(), withoutFence, client.DryRunAll), controllerWriteWebhook, want)
		}
	}
	awaitQuotaEvents(t, f.schema, "ResourceQuota results refused", 2)

	cleanupClockOffset.Store(int64(2 * time.Hour))
	c, err = resultcleanup.New(store.Client, p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	roots := records()
	if len(roots) != 2 {
		t.Fatalf("expected only the two foreground roots after member collection: %d", len(roots))
	}
	for _, kind := range []string{"intent", "credential"} {
		for _, record := range roots {
			if record.Spec.Type == kind {
				record.Finalizers = nil
				if err := admin.Update(t.Context(), &record); err != nil {
					t.Fatalf("eligible %s finalizer: %v", kind, err)
				}
			}
		}
	}
	if left := records(); len(left) != 0 {
		t.Fatalf("foreground completion under a full quota left %d records", len(left))
	}
}

// fillQuota installs a ResourceQuota on result records already at its limit.
// Envtest runs no quota controller, which counts usage on a real cluster, so
// the status it would write is written here: hard and used both at count.
func fillQuota(t *testing.T, namespace, name string, count int) {
	t.Helper()
	hard := corev1.ResourceList{"count/ptahresultrecords.operator.ptah.run": *resource.NewQuantity(int64(count), resource.DecimalSI)}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: corev1.ResourceQuotaSpec{Hard: hard}}
	if err := admin.Create(t.Context(), quota); err != nil {
		t.Fatal(err)
	}
	quota.Status = corev1.ResourceQuotaStatus{Hard: hard, Used: hard}
	if err := admin.Status().Update(t.Context(), quota); err != nil {
		t.Fatal(err)
	}
}

// awaitQuotaRefusal waits until quota admission, which reads quotas through an
// informer, refuses the retirement the collector will send, as a dry run.
func awaitQuotaRefusal(t *testing.T, writer client.Client, retirement *api.PtahResultRecord, quota string) {
	t.Helper()
	want := "exceeded quota: " + quota
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		err = writer.Create(t.Context(), retirement.DeepCopy(), client.DryRunAll)
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), want) {
			return
		}
	}
	t.Fatalf("a dry-run CREATE of %s still answered %v after 30 seconds, want a 403 with %q", retirement.Name, err, want)
}

// quotaExceededReason is the reason the troubleshooting page lists for a
// retirement a quota refused.
const quotaExceededReason = "ResultRetirementQuotaExceeded"

// awaitQuotaEvents waits for count Warning Events on the resource that say
// text. The broadcaster writes them asynchronously.
func awaitQuotaEvents(t *testing.T, subject client.Object, text string, count int) {
	t.Helper()
	var seen []string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		events := &corev1.EventList{}
		if err := admin.List(t.Context(), events, client.InNamespace(subject.GetNamespace())); err != nil {
			t.Fatal(err)
		}
		seen = seen[:0]
		matched := 0
		for _, event := range events.Items {
			seen = append(seen, event.Type+" "+event.Reason+" "+event.InvolvedObject.Kind+"/"+event.InvolvedObject.Name+": "+event.Message)
			if event.Type == corev1.EventTypeWarning && event.Reason == quotaExceededReason &&
				event.InvolvedObject.Kind == "PtahSchema" && event.InvolvedObject.Name == subject.GetName() && event.InvolvedObject.UID == subject.GetUID() &&
				strings.Contains(event.Message, text) {
				matched++
			}
		}
		if matched >= count {
			return
		}
	}
	t.Fatalf("after 30 seconds the namespace holds no %d Warning %s Events on PtahSchema %s saying %q; it holds %q", count, quotaExceededReason, subject.GetName(), text, seen)
}

// windowDenial is the refusal of a record still inside its own window.
func windowDenial(record *api.PtahResultRecord) string {
	return "result publication retention has not authorized deletion: record " + record.Name +
		" cannot be deleted before " + record.CreationTimestamp.Add(resultretention.MinimumWindow).UTC().Format(time.RFC3339)
}

// A namespace's deletion quotes this refusal in its conditions, and the time in
// it is what a person acts on. The policy clock runs twenty minutes ahead of the
// API server that stamped the records, so a time computed from the clock instead
// of the stored creationTimestamp reads differently and fails the row.
func TestRetentionDenialNamesTheEndOfTheWindow(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := publicationPayload(t, identity)
	if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatal(err)
	}
	read := func(name string) *api.PtahResultRecord {
		t.Helper()
		record := &api.PtahResultRecord{}
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: name}, record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	b := identity.Binding
	intentName, _ := resultstore.Name(b)
	intent, credential := read(intentName), read(jobconfig.CredentialName(b.UID, b.OperationID, b.JobName))
	collection := func(record *api.PtahResultRecord) error {
		return admin.DeleteAllOf(t.Context(), &api.PtahResultRecord{}, client.InNamespace(f.namespace), client.MatchingFields{"metadata.name": record.Name}, client.DryRunAll)
	}
	defer cleanupClockOffset.Store(0)

	cleanupClockOffset.Store(int64(20 * time.Minute))
	for _, record := range []*api.PtahResultRecord{intent, credential} {
		t.Run(record.Spec.Type+" inside its window", func(t *testing.T) {
			requireDenied(t, admin.Delete(t.Context(), record, client.DryRunAll), controllerWriteWebhook, windowDenial(record))
			// A namespace's deletion is a collection request, which has no name.
			requireDenied(t, collection(record), controllerWriteWebhook, windowDenial(record))
			// The active claim refuses the retirement a foreground request asks
			// for, and the time the ordinary deletion named still holds.
			requireDenied(t, admin.Delete(t.Context(), record, client.PropagationPolicy(metav1.DeletePropagationForeground), client.DryRunAll), controllerWriteWebhook, windowDenial(record))
		})
	}

	// Past the window the resource still pins the attempt. No time would lift
	// that, so the refusal names none.
	cleanupClockOffset.Store(int64(2 * time.Hour))
	for _, row := range []struct {
		record *api.PtahResultRecord
		reason string
	}{
		{intent, "retention has not authorized deletion"},
		{credential, "retention has not authorized deletion"},
	} {
		t.Run(row.record.Spec.Type+" past its window", func(t *testing.T) {
			for _, err := range []error{admin.Delete(t.Context(), row.record, client.DryRunAll), collection(row.record)} {
				requireDenied(t, err, controllerWriteWebhook, row.reason)
				if strings.Contains(err.Error(), "cannot be deleted before") {
					t.Fatalf("a refusal that is not about time named one: %v", err)
				}
			}
		})
	}
}

// Limit the collector's scan to this fixture's Role grant. The chart's actual
// cluster-wide LIST/DELETE grant is checked by the installation RBAC test.
type namespaceReader struct {
	client.Reader
	namespace string
}

func (r namespaceReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	opts = append(opts, client.InNamespace(r.namespace))
	return r.Reader.List(ctx, list, opts...)
}

func TestResultCollectorThroughAPIAdmission(t *testing.T) {
	t.Run("chunked", func(t *testing.T) { testResultCollectorThroughAPIAdmission(t, false, false) })
	t.Run("inline", func(t *testing.T) { testResultCollectorThroughAPIAdmission(t, true, false) })
	t.Run("chunked, then delivery off", func(t *testing.T) { testResultCollectorThroughAPIAdmission(t, false, true) })
	t.Run("inline, then delivery off", func(t *testing.T) { testResultCollectorThroughAPIAdmission(t, true, true) })
}

func testResultCollectorThroughAPIAdmission(t *testing.T, inline, deliveryOff bool) {
	f, identity, store := publicationFixture(t, true)
	payload := chunkedPublicationPayload(t, identity)
	activeRecords := 4
	if inline {
		payload = publicationPayload(t, identity)
		activeRecords = 2
	}
	receipt, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload))
	if err != nil {
		t.Fatal(err)
	}
	if deliveryOff {
		withDeliveryOff(t)
	}
	p := cleanupPolicy()
	p.Reader = namespaceReader{store.Reader, f.namespace}
	step := func() {
		t.Helper()
		c, err := resultcleanup.New(store.Client, p, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	list := func() []api.PtahResultRecord {
		t.Helper()
		l := &api.PtahResultRecordList{}
		if err := store.Reader.List(t.Context(), l, client.InNamespace(f.namespace)); err != nil {
			t.Fatal(err)
		}
		return l.Items
	}
	// A result still awaiting consumption cannot even be retired.
	step()
	if got := len(list()); got != activeRecords {
		t.Fatalf("active result changed: %d records", got)
	}
	op := f.schema.Status.ActiveOperation.DeepCopy()
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	step()
	if got := len(list()); got != activeRecords+1 {
		t.Fatalf("retirement was not persisted: %d records", got)
	}
	// Real API creation times start the wait. No clock override yet.
	if _, loaded, err := store.Load(t.Context(), identity.Binding); err != nil || loaded != receipt {
		t.Fatalf("early cleanup erased receipt: %v", err)
	}
	cleanupClockOffset.Store(int64(2 * time.Hour))
	defer cleanupClockOffset.Store(0)
	step()
	if got := len(list()); got != activeRecords+1 {
		t.Fatal("original Job no longer fences collection")
	}
	job := &batchv1.Job{}
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: identity.Binding.JobName}, job); err != nil {
		t.Fatal(err)
	}
	if err := admin.Delete(t.Context(), job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	// Reintroduced recovery state is checked at deletion, not only retirement.
	f.schema.Status.ActiveOperation = op
	writeStatus(t, f.schema)
	step()
	if got := len(list()); got != activeRecords+1 {
		t.Fatal("restored claim did not retain evidence")
	}
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	// Restore a pin after the collector has read the object and before its
	// DELETE reaches the API. The actual admission handler must still retain
	// every byte; a collector's earlier eligible snapshot is not authority.
	writer := &beforeResultDelete{Client: store.Client, before: func() {
		f.schema.Status.ActiveOperation = op
		writeStatus(t, f.schema)
	}}
	collector, err := resultcleanup.New(writer, p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDenied(t, collector.Step(t.Context()), controllerWriteWebhook, "retention has not authorized deletion")
	if writer.calls != 1 || len(list()) != activeRecords+1 {
		t.Fatalf("restored pin did not stop the first DELETE: calls=%d", writer.calls)
	}
	if _, loaded, err := store.Load(t.Context(), identity.Binding); err != nil || loaded != receipt {
		t.Fatalf("denied collection changed the retained receipt: %v", err)
	}
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	step()
	if got := list(); len(got) != 0 {
		for _, record := range got {
			t.Logf("retained %s: %v", record.Spec.Type, p.AuthorizeDelete(t.Context(), &record))
		}
		observedJob := &batchv1.Job{}
		err := admin.Get(t.Context(), client.ObjectKeyFromObject(job), observedJob)
		t.Logf("Job after deletion: err=%v deletion=%v finalizers=%v", err, observedJob.DeletionTimestamp, observedJob.Finalizers)
		t.Fatalf("collector left %d records", len(got))
	}
	if _, _, err := store.Load(t.Context(), identity.Binding); err == nil {
		t.Fatal("collected receipt still readable")
	}
	// Envtest runs no garbage collector. The projection's cascading owner is
	// covered separately; this test makes no claim about Secret garbage collection.
}

type beforeResultDelete struct {
	client.Client
	before func()
	calls  int
}

func (w *beforeResultDelete) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	w.calls++
	if w.calls == 1 {
		w.before()
	}
	return w.Client.Delete(ctx, object, options...)
}
