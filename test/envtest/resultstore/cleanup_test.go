package resultstore_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	recordapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The unit suite checks DELETE admission and recovery pins. This suite checks
// the API server's UID/RV preconditions on metadata-only collector deletions.
func TestCollectorUsesListedVersionsForDeletion(t *testing.T) {
	for _, change := range []string{"none", "resource-version", "replacement-uid"} {
		t.Run(change, func(t *testing.T) {
			b := newBinding(t)
			payload := bytes.Repeat([]byte("p"), resultstore.ChunkBytes+1)
			if _, err := (resultstore.Store{Client: api, Reader: api}).Publish(t.Context(), b, payload, digest(payload)); err != nil {
				t.Fatal(err)
			}
			name, _ := resultstore.Name(b)
			intent := &recordapi.PtahResultRecord{}
			if err := api.Get(t.Context(), client.ObjectKey{Namespace: b.Namespace, Name: name}, intent); err != nil {
				t.Fatal(err)
			}
			marker, err := resultretention.Record(b, resultretention.Source{Name: name, UID: intent.UID, Type: "intent"}, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := api.Create(t.Context(), marker); err != nil {
				t.Fatal(err)
			}
			reader := &changedMetadataReader{Reader: api, root: client.ObjectKeyFromObject(intent)}
			changed := false
			if change != "none" {
				reader.afterList = func(list *metav1.PartialObjectMetadataList) {
					for i := range list.Items {
						meta := &list.Items[i]
						if meta.Namespace != b.Namespace || meta.Name != name+"-complete" {
							continue
						}
						reader.afterList = nil
						changed = true
						current := &recordapi.PtahResultRecord{}
						if err := api.Get(t.Context(), client.ObjectKeyFromObject(meta), current); err != nil {
							t.Fatal(err)
						}
						if change == "resource-version" {
							current.Annotations = map[string]string{"test.ptah.run/changed": "true"}
							if err := api.Update(t.Context(), current); err != nil {
								t.Fatal(err)
							}
						} else {
							if err := api.Delete(t.Context(), current, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
								t.Fatal(err)
							}
							current.UID, current.ResourceVersion = "", ""
							current.CreationTimestamp = metav1.Time{}
							if err := api.Create(t.Context(), current); err != nil {
								t.Fatal(err)
							}
							if current.UID == meta.UID {
								t.Fatal("replacement reused the original UID")
							}
							// Isolate the UID precondition: even if the supplied RV
							// matches the replacement, the original UID must refuse it.
							meta.ResourceVersion = current.ResourceVersion
						}
					}
				}
			}
			policy := resultcleanup.Policy{Reader: reader, Window: time.Hour, Now: func() time.Time { return marker.CreationTimestamp.Add(2 * time.Hour) }}
			collector, err := resultcleanup.New(api, policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = collector.Step(t.Context())
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if !changed || !apierrors.IsConflict(err) {
					t.Fatalf("changed record was not retained by its delete precondition: changed=%t err=%v", changed, err)
				}
				list := &recordapi.PtahResultRecordList{}
				if err := api.List(t.Context(), list, client.InNamespace(b.Namespace)); err != nil {
					t.Fatal(err)
				}
				if len(list.Items) != 5 {
					t.Fatalf("stale collection removed evidence: %d records remain, want 5", len(list.Items))
				}
				// A new scan can collect the current versions after a conflict.
				fresh, err := resultcleanup.New(api, policy, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := fresh.Step(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			list := &recordapi.PtahResultRecordList{}
			if err := api.List(t.Context(), list, client.InNamespace(b.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 0 {
				t.Fatalf("collection left %d records", len(list.Items))
			}
		})
	}
}

type changedMetadataReader struct {
	client.Reader
	root      client.ObjectKey
	afterList func(*metav1.PartialObjectMetadataList)
}

func (r *changedMetadataReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if metadata, ok := list.(*metav1.PartialObjectMetadataList); ok {
		// Put the intent alone on the root page so the same Step cannot retry
		// this attempt through its retirement entry after the injected conflict.
		for _, item := range metadata.Items {
			if client.ObjectKeyFromObject(&item) == r.root {
				metadata.Items = []metav1.PartialObjectMetadata{item}
				metadata.Continue = ""
				break
			}
		}
		if r.afterList != nil {
			r.afterList(metadata)
		}
	}
	return nil
}
