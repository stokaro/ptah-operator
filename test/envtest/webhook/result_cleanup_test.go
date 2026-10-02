package webhook_test

import (
	"context"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
