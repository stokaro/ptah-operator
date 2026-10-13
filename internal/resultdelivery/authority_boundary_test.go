package resultdelivery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
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

type uploadReadBoundary struct {
	io.ReadCloser
	afterRead func()
}

func (b uploadReadBoundary) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.afterRead()
	}
	return n, err
}

func TestUploadAuthenticatesBeforeBodyAndRejectsRevokedAuthorityAfterBody(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, authenticated := range []bool{false, true} {
			name := "inline"
			if chunked {
				name = "chunked"
			}
			if authenticated {
				name += "/revoked while reading"
			} else {
				name += "/invalid token"
			}
			t.Run(name, func(t *testing.T) {
				identity := testIdentity()
				store, certs := testStore(t), testCertificates(t, identity)
				var bodyRead, revoked atomic.Bool
				var authorizations atomic.Int32
				receiver, err := NewReceiver(ReceiverConfig{Store: store, MaxConcurrent: 1, Timeout: time.Second,
					AuthenticateToken: func(context.Context, string, Identity) error {
						if !authenticated {
							return ErrAuthority
						}
						return nil
					}, Authorize: func(context.Context, Identity) error {
						authorizations.Add(1)
						if revoked.Load() {
							return ErrAuthority
						}
						return nil
					}})
				if err != nil {
					t.Fatal(err)
				}
				server := startTokenReceiver(t, receiver, certs, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						r.Body = uploadReadBoundary{ReadCloser: r.Body, afterRead: func() {
							bodyRead.Store(true)
							revoked.Store(true)
						}}
						next.ServeHTTP(w, r)
					})
				})
				payload := testPayload(t, identity)
				if chunked {
					payload = testChunkedPayload(t, identity)
				}
				sender := testTokenSender(t, server.URL, identity, certs, func() (string, error) { return "bound.token", nil })
				if _, err := sender.Send(t.Context(), payload); err == nil || err.Error() != "result receiver returned HTTP 403" {
					t.Fatalf("refused upload was acknowledged: %v", err)
				}
				wantChecks := int32(0)
				if authenticated {
					wantChecks = 1
				}
				if bodyRead.Load() != authenticated || authorizations.Load() != wantChecks {
					t.Fatalf("body read=%v authority checks=%d; authenticated=%v", bodyRead.Load(), authorizations.Load(), authenticated)
				}
				records := &api.PtahResultRecordList{}
				if err := store.Reader.List(t.Context(), records); err != nil || len(records.Items) != 0 {
					t.Fatalf("refused upload left persistent records: %d, %v", len(records.Items), err)
				}
			})
		}
	}
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
