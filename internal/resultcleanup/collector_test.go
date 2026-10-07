package resultcleanup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var errWrite = errors.New("lost API response")

type observedClient struct {
	client.Client
	now          time.Time
	creates      int
	deletes      []string
	failAt       int
	after        bool
	failCreate   bool
	beforeDelete func(client.Object)
	admission    *Policy
}

func (c *observedClient) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	c.creates++
	if c.failCreate {
		return errWrite
	}
	o.SetUID(types.UID(fmt.Sprintf("record-%d", c.creates)))
	o.SetCreationTimestamp(metav1.NewTime(c.now))
	return c.Client.Create(ctx, o, opts...)
}
func (c *observedClient) Delete(ctx context.Context, o client.Object, opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != o.GetUID() || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != o.GetResourceVersion() {
		return errors.New("deletion lacks exact UID and resource version")
	}
	if c.beforeDelete != nil {
		c.beforeDelete(o)
	}
	if c.admission == nil {
		return errors.New("test API deletion guard is not configured")
	}
	if err := c.admission.AuthorizeDelete(ctx, o.(*api.PtahResultRecord)); err != nil {
		return err
	}
	c.deletes = append(c.deletes, o.(*api.PtahResultRecord).Spec.Type)
	fail := c.failAt == len(c.deletes)
	if fail && !c.after {
		return errWrite
	}
	err := c.Client.Delete(ctx, o, opts...)
	if err == nil && fail {
		return errWrite
	}
	return err
}

type cleanupFixture struct {
	f      *resulttest.Fixture
	c      *observedClient
	p      Policy
	marker *api.PtahResultRecord
}

func fixture(t *testing.T) cleanupFixture {
	t.Helper()
	f := resulttest.New(t, "schema-plan-dev-fence-scheduling")
	// The consumed result has no active claim and its Job has already expired.
	f.Subject.(*api.PtahSchema).Status.ActiveOperation = nil
	base := f.Client(t)
	if err := base.Delete(t.Context(), f.Job); err != nil {
		t.Fatal(err)
	}
	c := &observedClient{Client: base, now: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)}
	store := resultstore.Store{Client: c, Reader: c}
	payload := bytes.Repeat([]byte("x"), resultstore.ChunkBytes+1)
	if _, err := store.Publish(t.Context(), f.Identity.Binding, payload, fmt.Sprintf("sha256:%x", sha256.Sum256(payload))); err != nil {
		t.Fatal(err)
	}
	name, _ := resultstore.Name(f.Identity.Binding)
	intent := &api.PtahResultRecord{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: name}, intent); err != nil {
		t.Fatal(err)
	}
	marker, err := resultretention.Record(f.Identity.Binding, resultretention.Source{Name: name, UID: intent.UID, Type: "intent"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Reader: c, Window: time.Hour, Now: func() time.Time { return c.now }}
	return cleanupFixture{f, c, p, marker}
}
func (f cleanupFixture) retire(t *testing.T) {
	t.Helper()
	if err := f.c.Create(t.Context(), f.marker); err != nil {
		t.Fatal(err)
	}
	f.c.now = f.c.now.Add(2 * time.Hour)
}
func (f cleanupFixture) collector(t *testing.T) *Collector {
	t.Helper()
	f.c.admission = &f.p
	c, err := New(f.c, f.p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func remaining(t *testing.T, c client.Client) []api.PtahResultRecord {
	t.Helper()
	list := &api.PtahResultRecordList{}
	if err := c.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestCollectionResumesEveryDeleteBoundary(t *testing.T) {
	// Complete, two chunks, intent, and fence. Restart after each possible
	// failed write, including successful deletion whose response was lost.
	for at := 1; at <= 5; at++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/after=%v", at, after), func(t *testing.T) {
				f := fixture(t)
				f.retire(t)
				f.c.failAt = at
				f.c.after = after
				if err := f.collector(t).collect(t.Context(), f.marker, f.f.Identity.Binding); !errors.Is(err, errWrite) {
					t.Fatalf("wanted interrupted deletion: %v", err)
				}
				f.c.failAt = 0
				if err := f.collector(t).collect(t.Context(), f.marker, f.f.Identity.Binding); err != nil {
					t.Fatal(err)
				}
				if got := remaining(t, f.c); len(got) != 0 {
					t.Fatalf("%d records remain", len(got))
				}
				if f.c.deletes[0] != "complete" || f.c.deletes[len(f.c.deletes)-1] != "retired" {
					t.Fatalf("unsafe deletion order: %v", f.c.deletes)
				}
			})
		}
	}
}

func TestRetirementStartsAtPersistedMarker(t *testing.T) {
	f := fixture(t)
	// Old payloads are not immediately disposable when first discovered.
	f.c.now = f.c.now.Add(24 * time.Hour)
	c := f.collector(t)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.c.deletes) != 0 || len(remaining(t, f.c)) != 5 {
		t.Fatal("new retirement bypassed its window")
	}
	f.c.now = f.c.now.Add(time.Hour)
	// A fresh leader must continue without the previous scan's memory.
	c = f.collector(t)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(remaining(t, f.c)) != 0 {
		t.Fatal("eligible publication was not collected")
	}
}

func TestPinsJobAgeAndAPIErrorsRetainEvidence(t *testing.T) {
	for _, reason := range []string{"active", "job", "late-child", "longer-window", "source-replaced", "foreign-owner", "list-error"} {
		t.Run(reason, func(t *testing.T) {
			f := fixture(t)
			f.retire(t)
			switch reason {
			case "active":
				resource := f.f.Subject.(*api.PtahSchema)
				resource.Status.ActiveOperation = &api.ActiveOperationStatus{ID: f.f.Identity.Binding.OperationID}
				if err := f.c.Update(t.Context(), resource); err != nil {
					t.Fatal(err)
				}
			case "job":
				f.f.Job.ResourceVersion = ""
				if err := f.c.Client.Create(t.Context(), f.f.Job); err != nil {
					t.Fatal(err)
				}
			case "longer-window":
				f.p.Window = 3 * time.Hour
			case "source-replaced":
				record := &api.PtahResultRecord{}
				if err := f.c.Get(t.Context(), client.ObjectKey{Namespace: f.marker.Namespace, Name: f.marker.Name[:len(f.marker.Name)-8]}, record); err != nil {
					t.Fatal(err)
				}
				record.UID = "replacement"
				if err := f.c.Update(t.Context(), record); err != nil {
					t.Fatal(err)
				}
			case "late-child", "foreign-owner":
				for _, record := range remaining(t, f.c) {
					if record.Spec.Type == "complete" {
						if reason == "late-child" {
							record.CreationTimestamp = metav1.NewTime(f.c.now)
						} else {
							record.OwnerReferences[0].UID = "another-intent"
						}
						if err := f.c.Update(t.Context(), &record); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "list-error":
				f.p.Reader = listFailure{Reader: f.c}
			}
			if err := f.collector(t).collect(t.Context(), f.marker, f.f.Identity.Binding); err == nil {
				t.Fatal("unsafe deletion allowed")
			}
			if len(f.c.deletes) != 0 {
				t.Fatalf("deleted evidence: %v", f.c.deletes)
			}
		})
	}
}

type listFailure struct{ client.Reader }

type cleanupReadCounter struct {
	client.Reader
	gets int
}

func (r *cleanupReadCounter) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets++
	return r.Reader.Get(ctx, key, object, opts...)
}

func TestIntentDefersItsCredentialScanUntilTheRetirementDeadline(t *testing.T) {
	f := resulttest.New(t, "schema-plan-dev-fence-scheduling")
	c := &observedClient{Client: f.Client(t), now: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	credential, err := (resultcredentials.PodBindings{Writer: c, Reader: c}).Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	subject := f.Subject.(*api.PtahSchema)
	active := subject.Status.ActiveOperation
	subject.Status.ActiveOperation = nil
	if err := c.Update(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	if err := c.Client.Delete(t.Context(), f.Job); err != nil {
		t.Fatal(err)
	}
	payload := []byte("retained outcome")
	if _, err := (resultstore.Store{Client: c, Reader: c}).Publish(t.Context(), f.Identity.Binding, payload, fmt.Sprintf("sha256:%x", sha256.Sum256(payload))); err != nil {
		t.Fatal(err)
	}
	name, _ := resultstore.Name(f.Identity.Binding)
	intent, pin := &api.PtahResultRecord{}, &api.PtahResultRecord{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: name}, intent); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: credential.Name}, pin); err != nil {
		t.Fatal(err)
	}
	marker, err := resultretention.Record(f.Identity.Binding, resultretention.Source{Name: intent.Name, UID: intent.UID, Type: "intent"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	reader := &cleanupReadCounter{Reader: c}
	p := Policy{Reader: reader, Window: time.Hour, Now: func() time.Time { return c.now }}
	c.admission = &p
	collector, err := New(c, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	collector.pending = []metav1.PartialObjectMetadata{{ObjectMeta: intent.ObjectMeta}}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A name-based hint may also defer a restored credential with a new UID.
	// It must never shorten that new object's own retention window.
	c.now = c.now.Add(15 * time.Minute)
	if err := c.Client.Delete(t.Context(), pin); err != nil {
		t.Fatal(err)
	}
	pin.ResourceVersion = ""
	if err := c.Create(t.Context(), pin); err != nil {
		t.Fatal(err)
	}
	if pin.UID == credential.UID {
		t.Fatal("credential replacement did not receive a new UID")
	}
	before := reader.gets
	collector.pending = []metav1.PartialObjectMetadata{{ObjectMeta: pin.ObjectMeta}}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.gets != before {
		t.Fatalf("credential repeated %d reads of the already known retirement window", reader.gets-before)
	}
	// The shared deadline only defers reads. A restored active claim still
	// protects the credential once that deadline has elapsed.
	subject.Status.ActiveOperation = active
	if err := c.Update(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	c.now = marker.CreationTimestamp.Add(time.Hour)
	collector.pending = []metav1.PartialObjectMetadata{{ObjectMeta: pin.ObjectMeta}}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.gets == before || len(c.deletes) != 0 {
		t.Fatal("deferred credential skipped the restored live pin")
	}
	subject.Status.ActiveOperation = nil
	if err := c.Update(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(time.Minute)
	collector.pending = []metav1.PartialObjectMetadata{{ObjectMeta: pin.ObjectMeta}}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(pin)
	if err := c.Get(t.Context(), key, &api.PtahResultRecord{}); err != nil {
		t.Fatalf("replacement credential lost its own retention window: %v", err)
	}
	c.now = pin.CreationTimestamp.Add(time.Hour)
	collector.pending = []metav1.PartialObjectMetadata{{ObjectMeta: pin.ObjectMeta}}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), key, &api.PtahResultRecord{}); !apierrors.IsNotFound(err) {
		t.Fatalf("eligible replacement credential was never collected: %v", err)
	}
}

func TestYoungRetirementMetadataNeedsNoRetryHint(t *testing.T) {
	f := fixture(t)
	if err := f.c.Create(t.Context(), f.marker); err != nil {
		t.Fatal(err)
	}
	reader := &cleanupReadCounter{Reader: f.c}
	f.p.Reader = reader
	c := f.collector(t)
	c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: f.marker.ObjectMeta}}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.gets != 0 || len(c.next) != 0 || len(f.c.deletes) != 0 {
		t.Fatalf("unexpired retirement used API reads or retry hints: reads=%d hints=%d deletes=%d", reader.gets, len(c.next), len(f.c.deletes))
	}
	// Metadata only postpones collection. Once the earliest possible window
	// ends, a restored active claim must still prevent deletion.
	resource := f.f.Subject.(*api.PtahSchema)
	resource.Status.ActiveOperation = &api.ActiveOperationStatus{ID: f.f.Identity.Binding.OperationID}
	if err := f.c.Update(t.Context(), resource); err != nil {
		t.Fatal(err)
	}
	f.c.now = f.c.now.Add(f.p.Window)
	c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: f.marker.ObjectMeta}}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.gets == 0 || len(f.c.deletes) != 0 {
		t.Fatal("elapsed metadata bound bypassed the live recovery pin check")
	}
}

func TestFullRetryHintsKeepUnexpiredRecordsDeferred(t *testing.T) {
	for _, expired := range []int{0, 2048} {
		t.Run(fmt.Sprintf("expired=%d", expired), func(t *testing.T) {
			f := fixture(t)
			if err := f.c.Create(t.Context(), f.marker); err != nil {
				t.Fatal(err)
			}
			reader := &cleanupReadCounter{Reader: f.c}
			f.p.Reader = reader
			c := f.collector(t)
			deadline := f.c.now.Add(time.Hour)
			c.next[retryHintKey{uid: f.marker.UID}] = deadline
			for i := 1; i < 4096; i++ {
				next := deadline
				if i <= expired {
					next = f.c.now
				}
				c.next[retryHintKey{uid: types.UID(fmt.Sprintf("retained-%d", i))}] = next
			}
			// A newly listed root can disappear before its direct read. Filling
			// the hint budget must not forget the retained roots beside it.
			c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{
				Namespace: f.marker.Namespace, Name: "already-gone", UID: "already-gone",
			}}}
			if err := c.Step(t.Context()); err != nil {
				t.Fatal(err)
			}
			before := reader.gets
			c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: f.marker.ObjectMeta}}
			if err := c.Step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if reader.gets != before {
				t.Fatalf("retained root was read %d times before its deadline after hint overflow", reader.gets-before)
			}
			if len(c.next) > 4096 {
				t.Fatalf("retry hints exceeded their memory bound: %d", len(c.next))
			}
			// The hint never authorizes deletion. At its deadline, a newly
			// restored active operation must be read and retain the evidence.
			resource := f.f.Subject.(*api.PtahSchema)
			resource.Status.ActiveOperation = &api.ActiveOperationStatus{ID: f.f.Identity.Binding.OperationID}
			if err := f.c.Update(t.Context(), resource); err != nil {
				t.Fatal(err)
			}
			f.c.now = deadline
			c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: f.marker.ObjectMeta}}
			if err := c.Step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if reader.gets == before || len(f.c.deletes) != 0 {
				t.Fatal("expired hint bypassed the live recovery pin check")
			}
		})
	}
}

func (listFailure) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("API unavailable")
}

func TestMarkerWriteFailureNeverDeletes(t *testing.T) {
	f := fixture(t)
	f.c.failCreate = true
	if err := f.collector(t).Step(t.Context()); !errors.Is(err, errWrite) {
		t.Fatalf("marker failure: %v", err)
	}
	if len(f.c.deletes) != 0 || len(remaining(t, f.c)) != 4 {
		t.Fatal("failed retirement deleted evidence")
	}
}

func TestForegroundRetirementPreservesPinsAndWindow(t *testing.T) {
	for _, reason := range []string{"eligible", "before-window", "active", "job", "longer-window", "late-child", "foreign-owner", "foreign-binding", "list-error"} {
		t.Run(reason, func(t *testing.T) {
			f := fixture(t)
			objects := []client.Object{f.f.Subject, f.f.Pod}
			for _, record := range remaining(t, f.c) {
				if record.Spec.Type == "intent" {
					if err := f.p.AuthorizeForegroundRetirement(t.Context(), &record); err != nil {
						t.Fatal(err)
					}
					if err := f.p.AuthorizeDelete(t.Context(), &record); err == nil {
						t.Fatal("ordinary DELETE bypasses foreground retirement")
					}
					record.DeletionTimestamp = &metav1.Time{Time: f.c.now}
					record.Finalizers = []string{metav1.FinalizerDeleteDependents}
					if reason == "foreign-binding" {
						record.Spec.Data = bytes.ReplaceAll(record.Spec.Data, []byte(`"jobUID":"job-uid"`), []byte(`"jobUID":"replacement-job"`))
					}
				}
				if record.Spec.Type == "complete" {
					if reason == "foreign-owner" {
						record.OwnerReferences[0].UID = "another-intent"
					}
					if reason == "late-child" {
						record.CreationTimestamp = metav1.NewTime(f.c.now.Add(2 * time.Hour))
					}
				}
				objects = append(objects, record.DeepCopy())
			}
			f.c.now = f.c.now.Add(2 * time.Hour)
			switch reason {
			case "before-window":
				f.c.now = f.c.now.Add(-time.Hour - time.Second)
			case "active":
				f.f.Subject.(*api.PtahSchema).Status.ActiveOperation = &api.ActiveOperationStatus{ID: f.f.Identity.Binding.OperationID}
			case "job":
				objects = append(objects, f.f.Job)
			case "longer-window":
				f.p.Window = 3 * time.Hour
			}
			f.c.Client = fake.NewClientBuilder().WithScheme(f.c.Scheme()).WithObjects(objects...).Build()
			if reason == "list-error" {
				f.p.Reader = listFailure{Reader: f.c}
			}
			err := f.collector(t).collect(t.Context(), nil, f.f.Identity.Binding)
			if reason == "eligible" {
				if err != nil {
					t.Fatal(err)
				}
				if len(f.c.deletes) != 3 || len(remaining(t, f.c)) != 1 {
					t.Fatal("eligible members must disappear while the API foreground root remains")
				}
			} else if err == nil || len(f.c.deletes) != 0 {
				t.Fatalf("unsafe foreground cleanup: err=%v deleted=%v", err, f.c.deletes)
			}
		})
	}
}

func TestLegacyChildrenAndOrphanCleanup(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(fmt.Sprint(orphan), func(t *testing.T) {
			f := fixture(t)
			f.retire(t)
			for _, r := range remaining(t, f.c) {
				if r.Spec.Type == "retired" {
					continue
				}
				delete(r.Labels, resultstore.LabelAttempt)
				if err := f.c.Update(t.Context(), &r); err != nil {
					t.Fatal(err)
				}
				if orphan && r.Spec.Type == "intent" {
					if err := f.c.Client.Delete(t.Context(), &r); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.collector(t).Step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(remaining(t, f.c)) != 0 {
				t.Fatal("legacy or orphan records remain")
			}
		})
	}
}

func TestCleanupMetricsContainOnlyBoundedLabels(t *testing.T) {
	f := fixture(t)
	reg := prometheus.NewRegistry()
	f.c.admission = &f.p
	c, err := New(f.c, f.p, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !c.NeedLeaderElection() {
		t.Fatal("collector must run only on leader")
	}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || len(families[0].Metric) == 0 {
		t.Fatal("missing cleanup metrics")
	}
	for _, m := range families[0].Metric {
		for _, l := range m.Label {
			if l.GetName() != "action" && l.GetName() != "outcome" {
				t.Fatalf("unbounded label: %s", l.GetName())
			}
		}
	}
}

type pagedReader struct {
	client.Reader
	calls   []client.ListOptions
	endless bool
}

func (r *pagedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	options := (&client.ListOptions{}).ApplyOptions(opts)
	r.calls = append(r.calls, *options)
	metadata, ok := list.(*metav1.PartialObjectMetadataList)
	if !ok {
		return errors.New("collector requested payload list")
	}
	if options.Limit != 64 && options.Limit != 128 {
		return errors.New("unbounded metadata list")
	}
	if options.Limit == 128 {
		if r.endless {
			metadata.Continue = "more"
		}
		return nil
	}
	// API pagination may produce an empty page with a continuation token.
	if options.Continue == "" {
		metadata.Continue = "next"
	}
	return nil
}

func TestScanPaginatesBeforeSwitchingQueries(t *testing.T) {
	f := fixture(t)
	reader := &pagedReader{Reader: f.c}
	f.p.Reader = reader
	c := f.collector(t)
	for range 3 {
		if err := c.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(reader.calls) != 3 || reader.calls[0].Continue != "" || reader.calls[1].Continue != "next" || reader.calls[2].Continue != "" {
		t.Fatalf("pagination lost: %+v", reader.calls)
	}
	if reader.calls[0].LabelSelector.String() != reader.calls[1].LabelSelector.String() || reader.calls[0].LabelSelector.String() == reader.calls[2].LabelSelector.String() {
		t.Fatal("query changed before its last page")
	}
}

func TestIncompleteMemberScanRefusesDeletion(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	reader := &pagedReader{Reader: f.c, endless: true}
	f.p.Reader = reader
	if err := f.collector(t).collect(t.Context(), f.marker, f.f.Identity.Binding); !errors.Is(err, ErrRetained) {
		t.Fatalf("incomplete scan: %v", err)
	}
	if len(f.c.deletes) != 0 || len(reader.calls) != 1 {
		t.Fatal("incomplete scan proceeded or exceeded its bound")
	}
}

func TestCanceledScanDoesNotDelete(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.collector(t).Step(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if len(f.c.deletes) != 0 {
		t.Fatal("canceled cleanup deleted evidence")
	}
}

func TestForeignRestoredIntentCannotLoseChildren(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	b := f.f.Identity.Binding
	// Restore left a credential-sourced fence but omitted that credential.
	// A foreign publication at the same attempt name is not its evidence.
	marker, err := resultretention.Record(b, resultretention.Source{Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName), UID: "missing-credential", Type: "credential"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	marker.UID, marker.ResourceVersion, marker.CreationTimestamp = f.marker.UID, f.marker.ResourceVersion, f.marker.CreationTimestamp
	if err := f.c.Update(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	name, _ := resultstore.Name(b)
	intent := &api.PtahResultRecord{}
	if err := f.c.Get(t.Context(), client.ObjectKey{Namespace: b.Namespace, Name: name}, intent); err != nil {
		t.Fatal(err)
	}
	intent.Spec.Data = bytes.ReplaceAll(intent.Spec.Data, []byte(`"jobUID":"`+string(b.JobUID)+`"`), []byte(`"jobUID":"replacement-job"`))
	changed, err := resultstore.StoredBinding(intent)
	if err != nil || changed.JobUID != "replacement-job" {
		t.Fatalf("invalid foreign fixture: %v", err)
	}
	if err := f.c.Update(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err := f.collector(t).collect(t.Context(), marker, b); !errors.Is(err, ErrRetained) {
		t.Fatalf("foreign restored publication: %v", err)
	}
	if len(f.c.deletes) != 0 {
		t.Fatal("foreign publication lost children before intent binding was checked")
	}
}
