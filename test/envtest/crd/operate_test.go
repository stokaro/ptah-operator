package crd_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// The changes kubectl ptah suspend, resume and refresh send are JSON patches
// that test metadata.uid first. The unit tests apply them with the JSON patch
// library the fake client links; here the API server applies them, moves the
// generation, records the field manager, and answers a failed test.
func TestKubectlPtahChangesAgainstTheAPIServer(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	typed, err := client.NewWithWatch(plane.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := newNamespace(t, "operate")
	for _, row := range []struct {
		family operate.Family
		base   func() *unstructured.Unstructured
	}{
		{family: operate.FamilySchema, base: schemaBase(namespace)},
		{family: operate.FamilyMigration, base: migrationBase(namespace)},
	} {
		t.Run(string(row.family), func(t *testing.T) {
			ctx := context.Background()
			created := row.base()
			if err := api.Create(ctx, created); err != nil {
				t.Fatalf("store %s: %v", created.GetKind(), err)
			}
			if len(created.GetAnnotations()) != 0 {
				t.Fatalf("the base carries annotations %v, so the bare refresh path is not measured", created.GetAnnotations())
			}
			ref := operate.Ref{Family: row.family, Namespace: namespace, Name: created.GetName()}

			suspended, err := operate.SetSuspend(ctx, typed, ref, true)
			if err != nil {
				t.Fatalf("suspend: %v", err)
			}
			stored := reread(t, created)
			if suspend, _, _ := unstructured.NestedBool(stored.Object, "spec", "suspend"); !suspend {
				t.Fatalf("spec.suspend is not true after suspend: %v", stored.Object["spec"])
			}
			if !suspended.Changed || suspended.Generation != created.GetGeneration()+1 || stored.GetGeneration() != suspended.Generation {
				t.Fatalf("suspend reported generation %d (changed %t); created at %d, stored at %d",
					suspended.Generation, suspended.Changed, created.GetGeneration(), stored.GetGeneration())
			}
			requireManaged(t, stored, `"f:suspend"`)

			refreshed, err := operate.Refresh(ctx, typed, ref, time.Now())
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			stored = reread(t, created)
			want := map[string]string{operatorv1alpha1.ReadRequestAnnotation: refreshed.Request}
			if !maps.Equal(stored.GetAnnotations(), want) || stored.GetGeneration() != suspended.Generation {
				t.Fatalf("after refresh: annotations %v at generation %d, want %v at %d",
					stored.GetAnnotations(), stored.GetGeneration(), want, suspended.Generation)
			}
			requireManaged(t, stored, `"f:`+operatorv1alpha1.ReadRequestAnnotation+`"`)

			// Between each read and its patch the resource is deleted and
			// created again under the name, suspended and annotated, so a
			// change to it would show.
			var refusals []error
			replacing := interceptor.NewClient(typed, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := api.Delete(ctx, row.base()); err != nil {
						return err
					}
					replacement := row.base()
					set(t, replacement, true, "spec", "suspend")
					replacement.SetAnnotations(map[string]string{"team": "shop"})
					if err := api.Create(ctx, replacement); err != nil {
						return err
					}
					err := c.Patch(ctx, obj, patch, opts...)
					refusals = append(refusals, err)
					return err
				},
			})
			if _, err := operate.SetSuspend(ctx, replacing, ref, false); !errors.Is(err, operate.ErrReplaced) {
				t.Fatalf("resume of a replaced resource: err = %v, want ErrReplaced", err)
			}
			if _, err := operate.Refresh(ctx, replacing, ref, time.Now()); !errors.Is(err, operate.ErrReplaced) {
				t.Fatalf("refresh of a replaced resource: err = %v, want ErrReplaced", err)
			}
			for _, refusal := range refusals {
				var status apierrors.APIStatus
				if !errors.As(refusal, &status) || status.Status().Code != http.StatusUnprocessableEntity {
					t.Fatalf("the API server answered a failed uid test with %v, want 422", refusal)
				}
			}
			if len(refusals) != 2 {
				t.Fatalf("measured %d refused patches, want 2", len(refusals))
			}
			replacement := reread(t, created)
			suspend, _, _ := unstructured.NestedBool(replacement.Object, "spec", "suspend")
			if replacement.GetUID() == stored.GetUID() || !suspend ||
				!maps.Equal(replacement.GetAnnotations(), map[string]string{"team": "shop"}) {
				t.Fatalf("the replacement was changed: uid %s, suspend %t, annotations %v",
					replacement.GetUID(), suspend, replacement.GetAnnotations())
			}

			// A refresh that read no annotations meets some another writer
			// added: the API server refuses the test for an absent map, and
			// the patch built again keeps theirs.
			replacement.SetAnnotations(nil)
			if err := api.Update(ctx, replacement); err != nil {
				t.Fatalf("clear the annotations: %v", err)
			}
			var answers []error
			racing := interceptor.NewClient(typed, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if len(answers) == 0 {
						current := reread(t, created)
						current.SetAnnotations(map[string]string{"owner": "team-b"})
						if err := api.Update(ctx, current); err != nil {
							return err
						}
					}
					err := c.Patch(ctx, obj, patch, opts...)
					answers = append(answers, err)
					return err
				},
			})
			raced, err := operate.Refresh(ctx, racing, ref, time.Now())
			if err != nil {
				t.Fatalf("refresh against another writer: %v", err)
			}
			var status apierrors.APIStatus
			if len(answers) != 2 || !errors.As(answers[0], &status) || status.Status().Code != http.StatusUnprocessableEntity || answers[1] != nil {
				t.Fatalf("the API server answered %v, want a 422 and then success", answers)
			}
			want = map[string]string{"owner": "team-b", operatorv1alpha1.ReadRequestAnnotation: raced.Request}
			if got := reread(t, created).GetAnnotations(); !maps.Equal(got, want) {
				t.Fatalf("annotations %v, want %v", got, want)
			}
		})
	}
}

// requireManaged fails unless the manager kubectl-ptah owns the field, as
// the API server records it.
func requireManaged(t *testing.T, object *unstructured.Unstructured, field string) {
	t.Helper()
	for _, entry := range object.GetManagedFields() {
		if entry.Manager == operate.FieldManager && entry.FieldsV1 != nil && bytes.Contains(entry.FieldsV1.GetRawBytes(), []byte(field)) {
			return
		}
	}
	t.Fatalf("no managedFields entry of %s holds %s: %+v", operate.FieldManager, field, object.GetManagedFields())
}
