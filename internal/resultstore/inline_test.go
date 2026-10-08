package resultstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSmallResultUsesOneDurableRecord(t *testing.T) {
	for _, size := range []int{1, InlinePayloadBytes, InlinePayloadBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := newStore(t)
			s.Client = admissionOnlyClient{Client: s.Client, store: s}
			payload := bytes.Repeat([]byte("r"), size)
			receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
			if err != nil {
				t.Fatal(err)
			}
			list := &api.PtahResultRecordList{}
			if err := s.Client.List(t.Context(), list); err != nil {
				t.Fatal(err)
			}
			want := 1
			name, _ := Name(binding())
			if size > InlinePayloadBytes {
				want, name = 3, name+"-complete"
			}
			if len(list.Items) != want || receipt.Name != name {
				t.Fatalf("size %d stored %d records, want %d; receipt=%+v", size, len(list.Items), want, receipt)
			}
			b, got, read, err := (Store{Reader: s.Reader}).LoadAttempt(t.Context(), binding().Namespace, binding().UID, binding().OperationID, binding().JobName)
			if err != nil || b != binding() || !bytes.Equal(got, payload) || read != receipt {
				t.Fatalf("fresh store did not read exact durable result: %v", err)
			}
		})
	}
}

func TestInlinePublicationWriteAndReadbackFailures(t *testing.T) {
	payload := []byte("small result")
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-write-response=%t", after), func(t *testing.T) {
			s := newStore(t)
			broken := Store{Client: &failingClient{Client: s.Client, at: 1, after: after}, Reader: s.Reader}
			if receipt, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, errLostWrite) || receipt != (Receipt{}) {
				t.Fatalf("failed write acknowledged: %v %v", receipt, err)
			}
			_, previous, err := s.Load(t.Context(), binding())
			if after && err != nil || !after && !errors.Is(err, ErrIncomplete) {
				t.Fatalf("atomic write has unexpected durability: %v", err)
			}
			receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
			if err != nil || after && receipt != previous {
				t.Fatalf("retry did not preserve committed identity: %v", err)
			}
		})
	}
	t.Run("final readback", func(t *testing.T) {
		s := newStore(t)
		reader := &inlineReadbackFailure{Reader: s.Reader}
		broken := Store{Client: s.Client, Reader: reader}
		if receipt, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, errLostWrite) || receipt != (Receipt{}) {
			t.Fatalf("failed readback acknowledged: %v %v", receipt, err)
		}
		if got, _, err := s.Load(t.Context(), binding()); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("failed acknowledgment lost durable bytes: %v", err)
		}
	})
}

type inlineReadbackFailure struct {
	client.Reader
}

func (r *inlineReadbackFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return errLostWrite
}

func TestInlinePublicationChecksAuthorityBeforeCommitAndRetry(t *testing.T) {
	s := newStore(t)
	payload := []byte("authorized result")
	denied := errors.New("authority revoked")
	refuse := func(context.Context) error { return denied }
	if receipt, err := s.PublishAuthorized(t.Context(), binding(), payload, digest(payload), refuse); !errors.Is(err, denied) || receipt != (Receipt{}) {
		t.Fatalf("revoked publication acknowledged: %v %v", receipt, err)
	}
	if _, _, err := s.Load(t.Context(), binding()); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("revoked result was persisted: %v", err)
	}
	before, err := s.PublishAuthorized(t.Context(), binding(), payload, digest(payload), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := s.PublishAuthorized(t.Context(), binding(), payload, digest(payload), refuse); !errors.Is(err, denied) || receipt != (Receipt{}) {
		t.Fatalf("revoked retry acknowledged: %v %v", receipt, err)
	}
	if got, receipt, err := s.Load(t.Context(), binding()); err != nil || receipt != before || !bytes.Equal(got, payload) {
		t.Fatalf("revocation erased evidence: %v", err)
	}
}

func TestInlineWriterResumesExistingChunkedResult(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			s := newStore(t)
			s.Client = admissionOnlyClient{Client: s.Client, store: s}
			legacy := s
			if partial {
				legacy.Client = &failingClient{Client: s.Client, at: 2}
			}
			payload := []byte("older receiver used chunks")
			name, _ := Name(binding())
			previous, err := legacy.publishChunks(t.Context(), binding(), name, payload, digest(payload), nil)
			if partial && !errors.Is(err, errLostWrite) || !partial && err != nil {
				t.Fatal(err)
			}
			other := []byte("different bytes")
			if _, err := s.Publish(t.Context(), binding(), other, digest(other)); !errors.Is(err, ErrConflict) {
				t.Fatalf("inline writer replaced reserved bytes: %v", err)
			}
			receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
			if err != nil || receipt.Name != name+"-complete" || !partial && receipt != previous {
				t.Fatalf("inline writer failed to resume chunked receipt: %v", err)
			}
			if got, _, err := s.Load(t.Context(), binding()); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("resumed bytes changed: %v", err)
			}
		})
	}
}

func TestInlineAdmissionRejectsMalformedPayloadAndChildRecords(t *testing.T) {
	for _, fault := range []string{"digest", "size", "chunks", "empty", "oversize", "owner", "child"} {
		t.Run(fault, func(t *testing.T) {
			s := newStore(t)
			payload := []byte("inline bytes")
			if _, err := s.Publish(t.Context(), binding(), payload, digest(payload)); err != nil {
				t.Fatal(err)
			}
			name, _ := Name(binding())
			stored := &api.PtahResultRecord{}
			if err := s.Reader.Get(t.Context(), client.ObjectKey{Namespace: binding().Namespace, Name: name}, stored); err != nil {
				t.Fatal(err)
			}
			candidate := stored.DeepCopy()
			var m manifest
			if err := json.Unmarshal(candidate.Spec.Data, &m); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "digest":
				m.Digest = digest([]byte("other"))
			case "size":
				m.Size++
			case "chunks":
				m.Chunks = []chunk{{Digest: digest(payload), Size: len(payload)}}
			case "empty":
				m.Inline = nil
			case "oversize":
				m.Inline = bytes.Repeat([]byte("x"), InlinePayloadBytes+1)
				m.Digest, m.Size = digest(m.Inline), int64(len(m.Inline))
			case "owner":
				candidate.OwnerReferences[0].UID = "foreign"
			}
			candidate.Spec.Data, _ = json.Marshal(m)
			if fault == "child" {
				candidate = record(binding().Namespace, name+"-000", "chunk", owner(apiVersion, "PtahResultRecord", name, stored.UID), payload)
			}
			if _, got, err := s.ValidateRecordCreate(t.Context(), candidate); err == nil || got != nil {
				t.Fatal("malformed inline publication admitted")
			}
		})
	}
}
