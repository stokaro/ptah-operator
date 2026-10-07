package webhook_test

import (
	"context"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResultCollectorInTerminatingNamespace(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := publicationPayload(t, identity)
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
	c, err := resultcleanup.New(store.Client, p, nil)
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
			if err := admin.Update(t.Context(), withoutFence); err == nil {
				t.Fatal("early finalizer removal discarded the retirement fence")
			}
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
	c, err = resultcleanup.New(store.Client, p, nil)
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
	f, identity, store := publicationFixture(t, true)
	payload := publicationPayload(t, identity)
	receipt, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload))
	if err != nil {
		t.Fatal(err)
	}
	p := cleanupPolicy()
	p.Reader = namespaceReader{store.Reader, f.namespace}
	step := func() {
		t.Helper()
		c, err := resultcleanup.New(store.Client, p, nil)
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
	if got := len(list()); got != 4 {
		t.Fatalf("active result changed: %d records", got)
	}
	op := f.schema.Status.ActiveOperation.DeepCopy()
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	step()
	if got := len(list()); got != 5 {
		t.Fatalf("retirement was not persisted: %d records", got)
	}
	// Real API creation times start the wait. No clock override yet.
	if _, loaded, err := store.Load(t.Context(), identity.Binding); err != nil || loaded != receipt {
		t.Fatalf("early cleanup erased receipt: %v", err)
	}
	cleanupClockOffset.Store(int64(2 * time.Hour))
	defer cleanupClockOffset.Store(0)
	step()
	if got := len(list()); got != 5 {
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
	if got := len(list()); got != 5 {
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
	collector, err := resultcleanup.New(writer, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDenied(t, collector.Step(t.Context()), controllerWriteWebhook, "retention has not authorized deletion")
	if writer.calls != 1 || len(list()) != 5 {
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
