package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestChurnDeletionUsesBothPreconditionsAndRefusesForeignReplacement(t *testing.T) {
	for _, mode := range []string{"success", "conflict", "foreign", "retained child"} {
		t.Run(mode, func(t *testing.T) {
			s := soakScenarios()
			s.evidenceDir = t.TempDir()
			original := soakObject(t, s, "migration", 8)
			scheme := runtime.NewScheme()
			if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			s.inputReader = clientfake.NewClientBuilder().WithScheme(scheme).Build()
			initial := []runtime.Object{original}
			if mode == "retained child" {
				initial = append(initial, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahMigrationPlan", "metadata": map[string]any{"name": "old-plan", "namespace": original.GetNamespace(), "uid": "old-plan-uid"}, "spec": map[string]any{"migrationRef": map[string]any{"name": original.GetName(), "uid": string(original.GetUID())}}}})
			}
			fake := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{migrationPlanResource: "PtahMigrationPlanList"}, initial...)
			s.dynamic = fake
			deleted, created := 0, 0
			fake.PrependReactor("delete", "ptahmigrations", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deleted++
				options := action.(clienttesting.DeleteAction).GetDeleteOptions()
				wantRV := "17"
				if mode == "conflict" && deleted > 1 {
					wantRV = "18"
				}
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != original.GetUID() || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != wantRV {
					t.Fatalf("unbound DELETE: %+v", options)
				}
				if mode == "conflict" && deleted == 1 {
					updated := original.DeepCopy()
					updated.SetResourceVersion("18")
					if err := fake.Tracker().Update(migrationResource, updated, original.GetNamespace()); err != nil {
						t.Fatal(err)
					}
					return true, nil, apierrors.NewConflict(migrationResource.GroupResource(), original.GetName(), fmt.Errorf("changed after export"))
				}
				if mode == "foreign" {
					if err := fake.Tracker().Delete(migrationResource, original.GetNamespace(), original.GetName()); err != nil {
						t.Fatal(err)
					}
					replacement := original.DeepCopy()
					replacement.SetUID("foreign")
					if err := fake.Tracker().Create(migrationResource, replacement, original.GetNamespace()); err != nil {
						t.Fatal(err)
					}
					return true, nil, nil
				}
				return false, nil, nil
			})
			fake.PrependReactor("create", "ptahmigrations", func(action clienttesting.Action) (bool, runtime.Object, error) {
				created++
				object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				if object.Object["status"] != nil || object.GetUID() != "" || len(object.GetFinalizers()) != 0 {
					t.Fatal("CREATE copied prior runtime state")
				}
				object.SetUID("new-uid")
				object.SetGeneration(1)
				object.SetCreationTimestamp(metav1.Now())
				if err := fake.Tracker().Create(migrationResource, object, object.GetNamespace()); err != nil {
					t.Fatal(err)
				}
				return true, object, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			target, err := s.churnOne(ctx, "migration", 8, 1)
			if mode == "foreign" || mode == "retained child" {
				if err == nil {
					t.Fatal("unproven replacement or garbage collection passed")
				}
				if mode == "foreign" && created != 0 {
					t.Fatal("overwrote foreign replacement")
				}
				if mode == "retained child" && len(s.churnProofs) != 1 {
					t.Fatalf("failure happened before garbage collection: %v", err)
				}
				if mode == "retained child" && s.churnProofs[0].GarbageCollected {
					t.Fatal("missing garbage collection was recorded as success")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if target.uid != "new-uid" || created != 1 || len(s.churnProofs) != 1 || !s.churnProofs[0].GarbageCollected {
					t.Fatal("replacement evidence incomplete", target, s.churnProofs)
				}
				want := 1
				if mode == "conflict" {
					want = 2
				}
				exports, err := filepath.Glob(filepath.Join(s.evidenceDir, "churn", "*.json"))
				if err != nil {
					t.Fatal(err)
				}
				if deleted != want || len(exports) != want {
					t.Fatal("conflict did not re-read and retain separate exports", deleted, exports)
				}
				for _, path := range exports {
					if raw, err := os.ReadFile(path); err != nil || len(raw) == 0 {
						t.Fatal("empty export", err)
					}
				}
			}
		})
	}
}
