package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

type barrierConflictClient struct {
	client.Client
	patches     int
	beforePatch func() error
}

func (c *barrierConflictClient) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	if err := c.beforePatch(); err != nil {
		return err
	}
	return c.Client.Patch(ctx, object, patch, opts...)
}

func TestStoredStateBarrierRetriesConcurrentStatus(t *testing.T) {
	c, original := newBarrierConflictClient(t)
	c.beforePatch = func() error {
		if c.patches != 1 {
			return nil
		}
		live := &ptahv1.PtahSchema{}
		if err := c.Client.Get(t.Context(), client.ObjectKeyFromObject(original), live); err != nil {
			return err
		}
		live.Status.Phase = ptahv1.PhaseBlocked
		return c.Client.Status().Update(t.Context(), live)
	}
	observed := original.DeepCopy()
	if err := writeStoredStateWatchBarrier(t.Context(), c, observed); err != nil {
		t.Fatal(err)
	}
	live := &ptahv1.PtahSchema{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(original), live); err != nil {
		t.Fatal(err)
	}
	if c.patches != 2 || live.Status.Phase != ptahv1.PhaseBlocked ||
		live.Annotations["preserved"] != "value" || live.Annotations[annotationWatchBarrier] == "" ||
		observed.UID != original.UID || observed.ResourceVersion != live.ResourceVersion {
		t.Fatal("barrier did not preserve the concurrent status and return its exact committed identity")
	}
}

func TestStoredStateBarrierRefusesReplacementAndOtherFailures(t *testing.T) {
	resource := schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemas"}
	for _, mode := range []string{"replacement", "forbidden", "persistent conflict"} {
		t.Run(mode, func(t *testing.T) {
			c, original := newBarrierConflictClient(t)
			c.beforePatch = func() error {
				if mode == "forbidden" {
					return apierrors.NewForbidden(resource, original.Name, errors.New("denied"))
				}
				if mode == "replacement" {
					if err := c.Client.Delete(t.Context(), original); err != nil {
						return err
					}
					replacement := original.DeepCopy()
					replacement.UID, replacement.ResourceVersion = "replacement", ""
					if err := c.Client.Create(t.Context(), replacement); err != nil {
						return err
					}
				}
				return apierrors.NewConflict(resource, original.Name, errors.New("concurrent write"))
			}
			err := writeStoredStateWatchBarrier(t.Context(), c, original.DeepCopy())
			if err == nil {
				t.Fatal("barrier accepted a failed write")
			}
			wantPatches := 1
			if mode == "persistent conflict" {
				wantPatches = retry.DefaultRetry.Steps
				if !apierrors.IsConflict(err) {
					t.Fatal(err)
				}
			}
			if c.patches != wantPatches || (mode == "forbidden" && !apierrors.IsForbidden(err)) ||
				(mode == "replacement" && !strings.Contains(err.Error(), "changed identity")) {
				t.Fatalf("patches=%d error=%v", c.patches, err)
			}
			live := &ptahv1.PtahSchema{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(original), live); err != nil {
				t.Fatal(err)
			}
			if live.Annotations[annotationWatchBarrier] != "" {
				t.Fatal("failed barrier wrote its annotation")
			}
		})
	}
}

func newBarrierConflictClient(t *testing.T) (*barrierConflictClient, *ptahv1.PtahSchema) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ptahv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	original := &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{
		Name: "schema", Namespace: "test", UID: "original", ResourceVersion: "1",
		Annotations: map[string]string{"preserved": "value"},
	}}
	c := &barrierConflictClient{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&ptahv1.PtahSchema{}).WithObjects(original).Build()}
	return c, original
}
