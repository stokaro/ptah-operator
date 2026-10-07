package resultdelivery

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type publicationBoundary struct {
	Publisher
	enter func()
}

type publicationRefusal struct {
	name     string
	chunked  bool
	refuseAt int32
}

func publicationRefusals() []publicationRefusal {
	return []publicationRefusal{
		{"inline/before body", false, 1},
		{"inline/before write", false, 2},
		{"chunked/before body", true, 1},
		{"chunked/before first write", true, 2},
		{"chunked/before completion", true, 3},
	}
}

func testChunkedPayload(t *testing.T, identity Identity) []byte {
	t.Helper()
	payload, err := Encode(identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(identity.Binding.Operation),
		OperationID: identity.Binding.OperationID, ChildExitCode: -1,
		Error: &runner.ResultError{Code: "refused", Message: strings.Repeat("r", resultstore.InlinePayloadBytes+1)}})
	if err != nil || len(payload) <= resultstore.InlinePayloadBytes || len(payload) > resultstore.ChunkBytes {
		t.Fatalf("fixture must require exactly one chunk: bytes=%d err=%v", len(payload), err)
	}
	return payload
}

func (p publicationBoundary) PublishAuthorized(ctx context.Context, binding resultstore.Binding, payload []byte, digest string, check func(context.Context) error) (resultstore.Receipt, error) {
	p.enter()
	return p.Publisher.PublishAuthorized(ctx, binding, payload, digest, check)
}

type publicationWriteBoundary struct {
	client.Client
	after func(client.Object)
}

func (c publicationWriteBoundary) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	c.after(object)
	return nil
}

func TestTokenRevocationBeforeCommitRefusesCompletion(t *testing.T) {
	for _, row := range []struct {
		name, boundary string
		chunked, retry bool
		wantRecords    int
	}{
		{name: "before body", boundary: "request"},
		{name: "inline before write", boundary: "publish"},
		{name: "inline retry before receipt", boundary: "publish", retry: true, wantRecords: 1},
		{name: "chunked before first write", boundary: "publish", chunked: true},
		{name: "chunked before completion", boundary: "chunk", chunked: true, wantRecords: 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			identity := testIdentity()
			store, certs := testStore(t), testCertificates(t, identity)
			var armed, revoked atomic.Bool
			store.Client = publicationWriteBoundary{Client: store.Client, after: func(object client.Object) {
				if armed.Load() && row.boundary == "chunk" && object.(*api.PtahResultRecord).Spec.Type == "chunk" {
					revoked.Store(true)
				}
			}}
			publisher := publicationBoundary{Publisher: store, enter: func() {
				if armed.Load() && row.boundary == "publish" {
					revoked.Store(true)
				}
			}}
			receiver, err := NewReceiver(ReceiverConfig{Store: publisher, MaxConcurrent: 1, Timeout: time.Second,
				AuthenticateToken: func(context.Context, string, Identity) error {
					if revoked.Load() {
						return ErrAuthority
					}
					return nil
				}, Authorize: func(context.Context, Identity) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			payload := testPayload(t, identity)
			if row.chunked {
				payload = testChunkedPayload(t, identity)
			}
			server := startTokenReceiver(t, receiver, certs, nil)
			sender := testTokenSender(t, server.URL, identity, certs, func() (string, error) { return "bound.token", nil })
			var original resultstore.Receipt
			if row.retry {
				original, err = sender.Send(t.Context(), payload)
				if err != nil {
					t.Fatal(err)
				}
			}
			armed.Store(true)
			if row.boundary == "request" {
				revoked.Store(true)
			}
			receipt, err := sender.Send(t.Context(), payload)
			if err == nil || err.Error() != "result receiver returned HTTP 403" || receipt != (resultstore.Receipt{}) {
				t.Fatalf("revoked upload returned a receipt or another result: receipt=%+v err=%v", receipt, err)
			}
			got, saved, err := store.Load(t.Context(), identity.Binding)
			if row.retry {
				if err != nil || saved != original || !bytes.Equal(got, payload) {
					t.Fatalf("revoked retry changed existing evidence: %v", err)
				}
			} else if !errors.Is(err, resultstore.ErrIncomplete) {
				t.Fatalf("revoked upload committed a result: %v", err)
			}
			records := &api.PtahResultRecordList{}
			if err := store.Reader.List(t.Context(), records); err != nil {
				t.Fatal(err)
			}
			if len(records.Items) != row.wantRecords {
				t.Fatalf("revoked upload left %d records, want %d", len(records.Items), row.wantRecords)
			}
		})
	}
}
