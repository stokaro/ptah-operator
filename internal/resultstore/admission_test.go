package resultstore

import (
	"bytes"
	"context"
	"encoding/json"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sync"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type validatingClient struct {
	client.Client
	store   Store
	t       *testing.T
	payload []byte
	seen    map[string]int
}

func (c *validatingClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	record := object.(*api.PtahResultRecord)
	b, payload, err := c.store.ValidateRecordCreate(ctx, record)
	if err != nil {
		return err
	}
	if b != binding() || (record.Spec.Type == "complete" && !bytes.Equal(payload, c.payload)) || (record.Spec.Type != "complete" && payload != nil) {
		c.t.Fatal("admission returned a foreign binding or incomplete payload")
	}
	c.seen[record.Spec.Type]++
	return c.Client.Create(ctx, object, options...)
}

func TestPublicationAdmissionAndIdenticalRetry(t *testing.T) {
	store := newStore(t)
	payload := bytes.Repeat([]byte("p"), ChunkBytes+7)
	guard := &validatingClient{Client: store.Client, store: store, t: t, payload: payload, seen: map[string]int{}}
	guarded := Store{Client: guard, Reader: store.Reader}
	first, err := guarded.Publish(t.Context(), binding(), payload, digest(payload))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := guarded.Publish(t.Context(), binding(), payload, digest(payload))
	if err != nil || first != repeated {
		t.Fatalf("identical retry: %v", err)
	}
	if guard.seen["intent"] != 2 || guard.seen["chunk"] != 2 || guard.seen["complete"] != 1 {
		t.Fatalf("missing validation: %v", guard.seen)
	}
}

func publishedRecords(t *testing.T) (Store, map[string]*api.PtahResultRecord) {
	t.Helper()
	s := newStore(t)
	payload := []byte("publication bytes")
	if _, err := s.Publish(t.Context(), binding(), payload, digest(payload)); err != nil {
		t.Fatal(err)
	}
	list := &api.PtahResultRecordList{}
	if err := s.Client.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	records := map[string]*api.PtahResultRecord{}
	for _, r := range list.Items {
		records[r.Spec.Type] = r.DeepCopy()
	}
	if len(records) != 3 {
		t.Fatal("fixture did not publish each role")
	}
	return s, records
}

func TestPublicationAdmissionRefusesRecordChanges(t *testing.T) {
	s, records := publishedRecords(t)
	for role, original := range records {
		t.Run(role, func(t *testing.T) {
			if _, _, err := s.ValidateRecordCreate(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			changes := map[string]func(*api.PtahResultRecord){
				"name":             func(r *api.PtahResultRecord) { r.Name += "-other" },
				"namespace":        func(r *api.PtahResultRecord) { r.Namespace = "other" },
				"owner UID":        func(r *api.PtahResultRecord) { r.OwnerReferences[0].UID = "foreign" },
				"owner controller": func(r *api.PtahResultRecord) { r.OwnerReferences[0].Controller = nil },
				"labels":           func(r *api.PtahResultRecord) { r.Labels["extra"] = "extra" },
				"annotations":      func(r *api.PtahResultRecord) { r.Annotations = map[string]string{"extra": "extra"} },
				"finalizer":        func(r *api.PtahResultRecord) { r.Finalizers = []string{"operator.ptah.run/extra"} },
				"generate name":    func(r *api.PtahResultRecord) { r.GenerateName = "extra-" },
				"empty bytes":      func(r *api.PtahResultRecord) { r.Spec.Data = nil },
				"changed bytes":    func(r *api.PtahResultRecord) { r.Spec.Data = append(r.Spec.Data, ' ') },
				"foreign role":     func(r *api.PtahResultRecord) { r.Spec.Type = "credential" },
			}
			for name, change := range changes {
				t.Run(name, func(t *testing.T) {
					candidate := original.DeepCopy()
					change(candidate)
					if _, payload, err := s.ValidateRecordCreate(t.Context(), candidate); err == nil || payload != nil {
						t.Fatal("changed record admitted or exposed payload")
					}
					if err := ValidateRecordUpdate(original, candidate); err == nil {
						t.Fatal("changed record update admitted")
					}
				})
			}
			next := original.DeepCopy()
			next.ResourceVersion = "other"
			next.ManagedFields = nil
			if err := ValidateRecordUpdate(original, next); err != nil {
				t.Fatalf("bookkeeping update: %v", err)
			}
		})
	}
}

func TestPublicationAdmissionRefusesDamagedCompletion(t *testing.T) {
	for _, damage := range []string{"chunk UID", "manifest UID", "manifest digest", "missing chunk", "corrupt chunk", "missing intent", "foreign chunk metadata", "manifest geometry", "unknown manifest field"} {
		t.Run(damage, func(t *testing.T) {
			s, records := publishedRecords(t)
			candidate := records["complete"].DeepCopy()
			switch damage {
			case "chunk UID", "manifest UID", "manifest digest":
				var c completion
				_ = json.Unmarshal(candidate.Spec.Data, &c)
				if damage == "chunk UID" {
					c.ChunkUIDs[0] = "other"
				}
				if damage == "manifest UID" {
					c.ManifestUID = "other"
				}
				if damage == "manifest digest" {
					c.ManifestDigest = digest([]byte("other"))
				}
				candidate.Spec.Data, _ = json.Marshal(c)
			case "missing chunk":
				if err := s.Client.Delete(t.Context(), records["chunk"]); err != nil {
					t.Fatal(err)
				}
			case "missing intent":
				if err := s.Client.Delete(t.Context(), records["intent"]); err != nil {
					t.Fatal(err)
				}
			case "corrupt chunk", "foreign chunk metadata":
				r := records["chunk"].DeepCopy()
				if damage == "corrupt chunk" {
					r.Spec.Data = []byte("corrupt")
				} else {
					r.Labels["extra"] = "extra"
				}
				if err := s.Client.Update(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			case "manifest geometry", "unknown manifest field":
				r := records["intent"].DeepCopy()
				if damage == "manifest geometry" {
					var m manifest
					_ = json.Unmarshal(r.Spec.Data, &m)
					m.Chunks[0].Size++
					r.Spec.Data, _ = json.Marshal(m)
				} else {
					r.Spec.Data = append(r.Spec.Data[:len(r.Spec.Data)-1], []byte(",\"extra\":true}")...)
				}
				if err := s.Client.Update(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			}
			if _, payload, err := s.ValidateRecordCreate(t.Context(), candidate); err == nil || payload != nil {
				t.Fatal("damaged completion admitted or leaked partial payload")
			}
		})
	}
}

func TestCompletedPublicationCannotBeHealed(t *testing.T) {
	for _, role := range []string{"intent", "chunk"} {
		t.Run(role, func(t *testing.T) {
			s, records := publishedRecords(t)
			r := records[role]
			if err := s.Client.Delete(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ValidateRecordCreate(t.Context(), r); err == nil {
				t.Fatal("missing completed member can be recreated")
			}
		})
	}
}

// Simulate another identical completion becoming visible between admission's
// read and the API's final CREATE. This is not a missing-chunk repair.
type completionRaceReader struct {
	client.Reader
	name  string
	reads int
}

func (r *completionRaceReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if key.Name == r.name {
		r.reads++
		if r.reads == 1 {
			return apierrors.NewNotFound(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahresultrecords"}, key.Name)
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}
func TestAdmissionAllowsConcurrentIdenticalCompletion(t *testing.T) {
	s, records := publishedRecords(t)
	reader := &completionRaceReader{Reader: s.Reader, name: records["complete"].Name}
	s.Reader = reader
	if _, payload, err := s.ValidateRecordCreate(t.Context(), records["complete"]); err != nil || !bytes.Equal(payload, []byte("publication bytes")) {
		t.Fatalf("identical completion race refused: %v", err)
	}
}

type admissionOnlyClient struct {
	client.Client
	store Store
}

func (c admissionOnlyClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, _, err := c.store.ValidateRecordCreate(ctx, object.(*api.PtahResultRecord)); err != nil {
		return err
	}
	return c.Client.Create(ctx, object, options...)
}
func TestConcurrentPublicationsPassAdmission(t *testing.T) {
	s := newStore(t)
	s.Client = admissionOnlyClient{Client: s.Client, store: s}
	payload := bytes.Repeat([]byte("x"), ChunkBytes+17)
	var workers sync.WaitGroup
	type outcome struct {
		receipt Receipt
		err     error
	}
	results := make(chan outcome, 4)
	for range 4 {
		workers.Go(func() {
			receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
			results <- outcome{receipt, err}
		})
	}
	workers.Wait()
	close(results)
	var first Receipt
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if first == (Receipt{}) {
			first = result.receipt
		}
		if result.receipt != first {
			t.Fatal("concurrent identical writes yielded different receipts")
		}
	}
}
