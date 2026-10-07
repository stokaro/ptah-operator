package webhook_test

import (
	"bytes"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestInlineResultRequiresProtocolAndLiveAdmission(t *testing.T) {
	for _, fault := range []string{"none", "invalid protocol", "revoked before create"} {
		t.Run(fault, func(t *testing.T) {
			f, identity, store := publicationFixture(t, true)
			payload := publicationPayload(t, identity)
			publishing := store
			if fault == "invalid protocol" {
				// Valid storage digest and identity do not make a valid result.
				payload = []byte(`{"not":"a runner result"}`)
			}
			if fault == "revoked before create" {
				publishing.Client = publicationWriter{Client: store.Client, before: func(*api.PtahResultRecord) error {
					f.schema.Status.ActiveOperation = nil
					writeStatus(t, f.schema)
					return nil
				}}
			}
			receipt, err := publishing.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload))
			if fault != "none" {
				requireDenied(t, err, controllerWriteWebhook, "immutable operation binding")
				if receipt != (resultstore.Receipt{}) {
					t.Fatal("refused publication returned a receipt")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				name, _ := resultstore.Name(identity.Binding)
				if receipt.Name != name {
					t.Fatal("small result did not use its atomic root receipt")
				}
				got, loaded, err := (resultstore.Store{Reader: admin}).Load(t.Context(), identity.Binding)
				if err != nil || loaded != receipt || !bytes.Equal(got, payload) {
					t.Fatalf("independent readback lost the exact receipt: %v", err)
				}
			}
			list := &api.PtahResultRecordList{}
			if err := admin.List(t.Context(), list, client.InNamespace(f.namespace)); err != nil {
				t.Fatal(err)
			}
			want := 1 // The enrolled credential remains in each refusal case.
			if fault == "none" {
				want++
			}
			if len(list.Items) != want {
				t.Fatalf("stored %d records, want %d", len(list.Items), want)
			}
		})
	}
}
