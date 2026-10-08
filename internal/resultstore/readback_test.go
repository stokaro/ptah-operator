package resultstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type countedResultReader struct {
	client.Reader
	gets map[client.ObjectKey]int
}

type damagedResultReader struct {
	client.Reader
	damage func(*api.PtahResultRecord)
}

func (r damagedResultReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	r.damage(object.(*api.PtahResultRecord))
	return nil
}

func TestInlineReadbackRefusesDamagedStoredIntent(t *testing.T) {
	for name, damage := range map[string]func(*api.PtahResultRecord){
		"missing UID":  func(r *api.PtahResultRecord) { r.UID = "" },
		"wrong owner":  func(r *api.PtahResultRecord) { r.OwnerReferences[0].UID = "other" },
		"wrong type":   func(r *api.PtahResultRecord) { r.Spec.Type = "credential" },
		"deleting":     func(r *api.PtahResultRecord) { now := metav1.Now(); r.DeletionTimestamp = &now },
		"corrupt data": func(r *api.PtahResultRecord) { r.Spec.Data[0] ^= 1 },
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			payload := []byte("exact stored result")
			broken := Store{Client: s.Client, Reader: damagedResultReader{Reader: s.Reader, damage: damage}}
			if receipt, err := broken.Publish(t.Context(), binding(), payload, digest(payload)); !errors.Is(err, ErrConflict) || receipt != (Receipt{}) {
				t.Fatalf("damaged readback acknowledged: %+v, %v", receipt, err)
			}
			b, got, receipt, err := broken.LoadAttempt(t.Context(), binding().Namespace, binding().UID, binding().OperationID, binding().JobName)
			if !errors.Is(err, ErrConflict) || b != (Binding{}) || len(got) != 0 || receipt != (Receipt{}) {
				t.Fatalf("damaged attempt read returned a result: %+v, %v", receipt, err)
			}
			if got, _, err := s.Load(t.Context(), binding()); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("failed readback changed stored evidence: %v", err)
			}
		})
	}
}

func (r *countedResultReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets[key]++
	return r.Reader.Get(ctx, key, object, opts...)
}

func TestInlinePublicationReadsPersistedResultOnce(t *testing.T) {
	s := newStore(t)
	reader := &countedResultReader{Reader: s.Reader, gets: map[client.ObjectKey]int{}}
	s.Reader = reader
	payload := []byte("persisted result")
	name, _ := Name(binding())
	key := client.ObjectKey{Namespace: binding().Namespace, Name: name}
	var previous Receipt
	for attempt := range 2 {
		clear(reader.gets)
		receipt, err := s.Publish(t.Context(), binding(), payload, digest(payload))
		if err != nil || receipt.UID == "" || attempt > 0 && receipt != previous {
			t.Fatalf("publication %d lost its durable receipt: %+v, %v", attempt, receipt, err)
		}
		if reader.gets[key] != 1 || len(reader.gets) != 1 {
			t.Fatalf("publication %d reads: %v; want one direct read of the persisted intent", attempt, reader.gets)
		}
		previous = receipt
	}
}

func TestLoadAttemptReadsEachPersistedRecordOnce(t *testing.T) {
	for _, size := range []int{1, InlinePayloadBytes, InlinePayloadBytes + 1, ChunkBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := newStore(t)
			payload := bytes.Repeat([]byte("r"), size)
			want, err := s.Publish(t.Context(), binding(), payload, digest(payload))
			if err != nil {
				t.Fatal(err)
			}
			reader := &countedResultReader{Reader: s.Reader, gets: map[client.ObjectKey]int{}}
			b, got, receipt, err := (Store{Reader: reader}).LoadAttempt(t.Context(), binding().Namespace, binding().UID, binding().OperationID, binding().JobName)
			if err != nil || b != binding() || receipt != want || !bytes.Equal(got, payload) {
				t.Fatalf("read did not preserve exact binding, payload, and receipt: %v", err)
			}
			count := 1
			if size > InlinePayloadBytes {
				count = 2 + (size+ChunkBytes-1)/ChunkBytes
			}
			if len(reader.gets) != count {
				t.Fatalf("read %d distinct records, want %d", len(reader.gets), count)
			}
			for key, n := range reader.gets {
				if n != 1 {
					t.Errorf("record %s read %d times, want one", key, n)
				}
			}
		})
	}
}
