package resultstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	recordapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func binding() Binding {
	return Binding{Namespace: "tenant", Kind: "PtahSchema", Name: "schema", UID: "schema-uid", Generation: 2,
		ExecutionBindingID: "v1-" + strings.Repeat("a", 32), InputFingerprint: "sha256:" + strings.Repeat("b", 64),
		Operation: "plan", OperationID: "operation-1", JobName: "plan-attempt-1", JobUID: "job-uid", PodName: "plan-pod", PodUID: "pod-uid"}
}

type identifiedClient struct {
	client.Client
	sequence atomic.Int64
}

func (c *identifiedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	obj.SetUID(types.UID(fmt.Sprintf("stored-%d", c.sequence.Add(1))))
	return c.Client.Create(ctx, obj, opts...)
}

func newStore(t *testing.T) Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := recordapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := &identifiedClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	return Store{Client: c, Reader: c}
}

type failingClient struct {
	client.Client
	at    int
	after bool
	calls int
}

var errLostWrite = errors.New("API write response unavailable")

func (c *failingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.calls++
	if c.calls == c.at && !c.after {
		return errLostWrite
	}
	err := c.Client.Create(ctx, obj, opts...)
	if c.calls == c.at && err == nil {
		return errLostWrite
	}
	return err
}

func TestResumeEveryPublicationBoundary(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), ChunkBytes*2+17)
	// Intent, three chunks, and completion each have a before-write failure
	// and a persisted-write/lost-response failure. Only the latter completion
	// may be readable before the receiver retries.
	for at := 1; at <= 5; at++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("write-%d/after-%t", at, after), func(t *testing.T) {
				s := newStore(t)
				broken := Store{Client: &failingClient{Client: s.Client, at: at, after: after}, Reader: s.Reader}
				if receipt, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, errLostWrite) || receipt != (Receipt{}) {
					t.Fatalf("failed publication returned a receipt or lost its error: receipt=%v err=%v", receipt, err)
				}
				_, _, err := s.Load(t.Context(), binding())
				if at == 5 && after {
					if err != nil {
						t.Fatalf("persisted completion lost: %v", err)
					}
				} else if !errors.Is(err, ErrIncomplete) {
					t.Fatalf("partial publication was readable: %v", err)
				}
				// A fresh Store has no in-memory publication state or process key.
				restarted := Store{Client: s.Client, Reader: s.Reader}
				receipt, err := restarted.Publish(t.Context(), binding(), payload, digest(payload))
				if err != nil {
					t.Fatal(err)
				}
				got, loaded, err := restarted.Load(t.Context(), binding())
				if err != nil || !bytes.Equal(got, payload) || loaded != receipt {
					t.Fatalf("readback disagrees: %v", err)
				}
				retry, err := restarted.Publish(t.Context(), binding(), payload, digest(payload))
				if err != nil || retry != receipt {
					t.Fatalf("retry changed receipt: %v", err)
				}
			})
		}
	}
}

func TestConflictingBytesCannotReplacePartialPublication(t *testing.T) {
	s := newStore(t)
	payload := []byte("first result")
	broken := Store{Client: &failingClient{Client: s.Client, at: 2}, Reader: s.Reader}
	if _, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, errLostWrite) {
		t.Fatal(err)
	}
	other := []byte("other result")
	if _, err := s.Publish(t.Context(), binding(), other, digest(other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting partial write: %v", err)
	}
	if _, err := s.Publish(t.Context(), binding(), payload, digest(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(t.Context(), binding(), other, digest(other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting committed write: %v", err)
	}
	got, _, err := s.Load(t.Context(), binding())
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("original bytes lost: %v", err)
	}
}

func TestPublicationBindsEveryIdentity(t *testing.T) {
	changes := map[string]func(*Binding){
		"namespace":    func(b *Binding) { b.Namespace = "other" },
		"kind":         func(b *Binding) { b.Kind = "PtahMigration"; b.Operation = "migration-history" },
		"name":         func(b *Binding) { b.Name = "other" },
		"uid":          func(b *Binding) { b.UID = "other" },
		"generation":   func(b *Binding) { b.Generation++ },
		"epoch":        func(b *Binding) { b.ExecutionBindingID = "v1-" + strings.Repeat("c", 32) },
		"fingerprint":  func(b *Binding) { b.InputFingerprint = "sha256:" + strings.Repeat("c", 64) },
		"operation":    func(b *Binding) { b.Operation = "apply" },
		"operation id": func(b *Binding) { b.OperationID = "other" },
		"job name":     func(b *Binding) { b.JobName = "other" },
		"job uid":      func(b *Binding) { b.JobUID = "other" },
		"pod name":     func(b *Binding) { b.PodName = "other" },
		"pod uid":      func(b *Binding) { b.PodUID = "other" },
	}
	s := newStore(t)
	payload := []byte("result")
	if _, err := s.Publish(t.Context(), binding(), payload, digest(payload)); err != nil {
		t.Fatal(err)
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			b := binding()
			change(&b)
			got, receipt, err := s.Load(t.Context(), b)
			if err == nil || len(got) != 0 || receipt != (Receipt{}) {
				t.Fatal("changed identity obtained result")
			}
			originalName, _ := Name(binding())
			changedName, _ := Name(b)
			if changedName == originalName {
				if _, err := s.Publish(t.Context(), b, payload, digest(payload)); !errors.Is(err, ErrConflict) {
					t.Fatalf("same attempt replaced binding: %v", err)
				}
			}
		})
	}
}

func TestStorageDamageCannotProduceReceipt(t *testing.T) {
	for _, damage := range []string{"missing chunk", "replaced chunk", "corrupt chunk", "changed owner", "empty chunk", "oversized chunk", "deleting chunk", "wrong type", "missing complete", "corrupt complete", "corrupt manifest"} {
		t.Run(damage, func(t *testing.T) {
			s := newStore(t)
			b := binding()
			payload := bytes.Repeat([]byte("p"), ChunkBytes+1)
			if _, err := s.Publish(t.Context(), b, payload, digest(payload)); err != nil {
				t.Fatal(err)
			}
			name, _ := Name(b)
			target := chunkName(name, 1)
			if strings.Contains(damage, "complete") {
				target = name + "-complete"
			}
			if damage == "corrupt manifest" {
				target = name
			}
			obj := &recordapi.PtahResultRecord{}
			if err := s.Reader.Get(t.Context(), client.ObjectKey{Namespace: b.Namespace, Name: target}, obj); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(damage, "missing") {
				if err := s.Client.Delete(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			} else {
				switch damage {
				case "replaced chunk":
					obj.UID = "replacement"
				case "changed owner":
					obj.OwnerReferences[0].UID = "replacement"
				case "empty chunk":
					obj.Spec.Data = nil
				case "oversized chunk":
					obj.Spec.Data = bytes.Repeat([]byte("x"), ChunkBytes+1)
				case "deleting chunk":
					obj.Finalizers = []string{"test.example/hold"}
				case "wrong type":
					obj.Spec.Type = "credential"
				default:
					obj.Spec.Data[0] ^= 1
				}
				// The fake deliberately permits corruption a real immutable
				// record refuses, to test readback independently of admission.
				if err := s.Client.Update(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
				if damage == "deleting chunk" {
					if err := s.Client.Delete(t.Context(), obj); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, receipt, err := s.Load(t.Context(), b)
			if err == nil || len(got) != 0 || receipt != (Receipt{}) {
				t.Fatal("damaged storage produced a result")
			}
			if damage != "missing complete" {
				if _, err := s.Publish(t.Context(), b, payload, digest(payload)); err == nil {
					t.Fatal("retry concealed committed storage damage")
				}
			}
		})
	}
}

func TestConcurrentIdenticalAndConflictingDelivery(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		t.Run(fmt.Sprint(conflicting), func(t *testing.T) {
			s := newStore(t)
			var wg sync.WaitGroup
			receipts := make([]Receipt, 12)
			errs := make([]error, 12)
			for i := range receipts {
				wg.Go(func() {
					payload := []byte("same result")
					if conflicting && i%2 == 1 {
						payload = []byte("different result")
					}
					receipts[i], errs[i] = s.Publish(t.Context(), binding(), payload, digest(payload))
				})
			}
			wg.Wait()
			got, receipt, err := s.Load(t.Context(), binding())
			if err != nil {
				t.Fatal(err)
			}
			succeeded, rejected := 0, 0
			for i, err := range errs {
				if err == nil {
					succeeded++
					if receipts[i] != receipt {
						t.Fatal("concurrent delivery changed receipt")
					}
					want := "same result"
					if conflicting && i%2 == 1 {
						want = "different result"
					}
					if string(got) != want {
						t.Fatal("receipt acknowledged different bytes")
					}
				} else if errors.Is(err, ErrConflict) {
					rejected++
				} else {
					t.Fatal(err)
				}
			}
			wantSuccess := 12
			if conflicting {
				wantSuccess = 6
			}
			if succeeded != wantSuccess || rejected != 12-wantSuccess {
				t.Fatalf("success=%d conflicts=%d", succeeded, rejected)
			}
		})
	}
}

func TestSizeDigestAndInvalidBindingRefusedBeforeWrites(t *testing.T) {
	for _, row := range []struct {
		name    string
		b       Binding
		payload []byte
		sum     string
	}{
		{"empty", binding(), nil, digest(nil)},
		{"maximum plus one", binding(), make([]byte, MaxPayloadBytes+1), "sha256:" + strings.Repeat("a", 64)},
		{"digest", binding(), []byte("bytes"), digest([]byte("other"))},
		{"missing identity", Binding{}, []byte("bytes"), digest([]byte("bytes"))},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newStore(t)
			if _, err := s.Publish(t.Context(), row.b, row.payload, row.sum); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid upload: %v", err)
			}
			list := &recordapi.PtahResultRecordList{}
			if err := s.Client.List(t.Context(), list); err != nil || len(list.Items) != 0 {
				t.Fatal("invalid upload wrote objects")
			}
		})
	}
}

func TestMaximumPayloadSurvivesNewStore(t *testing.T) {
	s := newStore(t)
	payload := bytes.Repeat([]byte("x"), int(MaxPayloadBytes))
	receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
	if err != nil {
		t.Fatal(err)
	}
	got, loaded, err := (Store{Reader: s.Reader}).Load(t.Context(), binding())
	if err != nil || loaded != receipt || !bytes.Equal(got, payload) {
		t.Fatalf("maximum payload read: %v", err)
	}
	list := &recordapi.PtahResultRecordList{}
	if err := s.Client.List(t.Context(), list); err != nil || len(list.Items) != maxChunks+2 {
		t.Fatalf("unexpected object count: %v", err)
	}
}

func TestCanceledPublicationReturnsNoReceipt(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// client-go honors cancellation. The fake does not; this wrapper supplies
	// that boundary without changing the storage algorithm under test.
	s.Client = canceledClient{s.Client}
	if receipt, err := s.Publish(ctx, binding(), []byte("bytes"), digest([]byte("bytes"))); !errors.Is(err, context.Canceled) || receipt != (Receipt{}) {
		t.Fatalf("canceled publication: %v %v", receipt, err)
	}
}

func TestFinalReadbackFailurePreventsAcknowledgment(t *testing.T) {
	s := newStore(t)
	payload := []byte("persisted before the readback fails")
	reader := &failedReadback{Reader: s.Reader}
	broken := Store{Client: s.Client, Reader: reader}
	if receipt, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, errLostWrite) || receipt != (Receipt{}) {
		t.Fatalf("failed final readback was acknowledged: %v", err)
	}
	got, receipt, err := s.Load(t.Context(), binding())
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("stored publication lost: %v", err)
	}
	retry, err := s.Publish(t.Context(), binding(), payload, digest(payload))
	if err != nil || retry != receipt {
		t.Fatalf("retry failed to recover the receipt: %v", err)
	}
}

type failedReadback struct {
	client.Reader
	chunkReads int
}

func (r *failedReadback) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if strings.HasSuffix(key.Name, "-000") {
		r.chunkReads++
		if r.chunkReads == 2 {
			return errLostWrite
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestSupportedOperationsAndMissingAuthority(t *testing.T) {
	for kind, operations := range map[string][]string{
		"PtahSchema":    {"resolve", "verify", "observe", "plan", "apply"},
		"PtahMigration": {"resolve", "verify", "migration-history", "migration-apply"},
	} {
		for _, operation := range operations {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				b := binding()
				b.Kind, b.Operation = kind, operation
				s := newStore(t)
				payload := []byte("validated result")
				if _, err := s.Publish(t.Context(), b, payload, digest(payload)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	for name, change := range map[string]func(*Binding){
		"namespace":    func(b *Binding) { b.Namespace = "" },
		"kind":         func(b *Binding) { b.Kind = "Job" },
		"name":         func(b *Binding) { b.Name = "" },
		"uid":          func(b *Binding) { b.UID = "" },
		"generation":   func(b *Binding) { b.Generation = 0 },
		"epoch":        func(b *Binding) { b.ExecutionBindingID = "" },
		"fingerprint":  func(b *Binding) { b.InputFingerprint = "" },
		"operation":    func(b *Binding) { b.Operation = "migration-apply" },
		"operation id": func(b *Binding) { b.OperationID = "" },
		"job name":     func(b *Binding) { b.JobName = "" },
		"job uid":      func(b *Binding) { b.JobUID = "" },
		"pod name":     func(b *Binding) { b.PodName = "" },
		"pod uid":      func(b *Binding) { b.PodUID = "" },
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			b := binding()
			change(&b)
			s := newStore(t)
			payload := []byte("validated result")
			if _, err := s.Publish(t.Context(), b, payload, digest(payload)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid authority persisted: %v", err)
			}
			list := &recordapi.PtahResultRecordList{}
			if err := s.Client.List(t.Context(), list); err != nil || len(list.Items) != 0 {
				t.Fatal("invalid authority wrote objects")
			}
		})
	}
}

type canceledClient struct{ client.Client }

func (c canceledClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

// The API runs admission before storage can answer AlreadyExists. Its refusal
// of different bytes must remain a permanent content conflict at the receiver.
type admittedClient struct {
	client.Client
	store Store
}

func (c *admittedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, _, err := c.store.ValidateRecordCreate(ctx, obj.(*recordapi.PtahResultRecord)); err != nil {
		return apierrors.NewForbidden(schema.GroupResource{Group: recordapi.GroupVersion.Group, Resource: "ptahresultrecords"}, obj.GetName(), err)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestAdmissionConflictKeepsPermanentPublicationError(t *testing.T) {
	s := newStore(t)
	s.Client = &admittedClient{Client: s.Client, store: s}
	payload := []byte("original")
	receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
	if err != nil {
		t.Fatal(err)
	}
	other := []byte("conflicting")
	if got, err := s.Publish(t.Context(), binding(), other, digest(other)); !errors.Is(err, ErrConflict) || got != (Receipt{}) {
		t.Fatalf("admission refusal must be a permanent conflict without a receipt: %v %v", got, err)
	}
	if got, err := s.Publish(t.Context(), binding(), payload, digest(payload)); err != nil || got != receipt {
		t.Fatalf("identical admitted retry changed receipt: %v %v", got, err)
	}
	// A failed write of identical bytes is not a conflict and must not be
	// converted to a receipt merely because the old publication is readable.
	broken := Store{Client: &failingClient{Client: s.Client, at: 1}, Reader: s.Reader}
	if got, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, errLostWrite) || got != (Receipt{}) {
		t.Fatalf("unrelated write failure changed: %v %v", got, err)
	}
	got, retained, err := s.Load(t.Context(), binding())
	if err != nil || !bytes.Equal(got, payload) || retained != receipt {
		t.Fatalf("original publication changed: %v", err)
	}
}
