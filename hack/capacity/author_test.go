package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestCapacityAuthorHandoffKeepsIntermediateResourcesSuspended(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		for _, failure := range []string{"", "grant", "resume", "changed UID", "changed spec"} {
			t.Run(family+"/"+failure, func(t *testing.T) {
				installer := fake.NewSimpleDynamicClient(runtime.NewScheme())
				author := fake.NewSimpleDynamicClient(runtime.NewScheme())
				s := &scenarios{dynamic: installer, author: author}
				resource, kind := schemaResource, "PtahSchema"
				if family == "migration" {
					resource, kind = migrationResource, "PtahMigration"
				}
				desired := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "operator.ptah.run/v1alpha1", "kind": kind,
					"metadata": map[string]any{"name": "work", "namespace": "work"},
					"spec":     map[string]any{"suspend": false, "policy": map[string]any{"apply": "Always"}},
				}}
				var stored *unstructured.Unstructured
				author.PrependReactor("create", resource.Resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
					stored = a.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
					paused, _, _ := unstructured.NestedBool(stored.Object, "spec", "suspend")
					apply, _, _ := unstructured.NestedString(stored.Object, "spec", "policy", "apply")
					if !paused || apply != "OnApproval" {
						t.Fatal("author tried to start an intermediate workload or select Always")
					}
					stored.SetUID("original")
					stored.SetResourceVersion("1")
					stored.SetGeneration(1)
					return true, stored.DeepCopy(), nil
				})
				patch := func(actor string) clienttesting.ReactionFunc {
					return func(a clienttesting.Action) (bool, runtime.Object, error) {
						if actor == failure {
							return true, nil, errors.New("write refused")
						}
						raw, _ := json.Marshal(stored.Object)
						updated, err := jsonpatch.MergePatch(raw, a.(clienttesting.PatchAction).GetPatch())
						if err != nil {
							t.Fatal(err)
						}
						if err := stored.UnmarshalJSON(updated); err != nil {
							t.Fatal(err)
						}
						stored.SetGeneration(stored.GetGeneration() + 1)
						stored.SetResourceVersion(fmt.Sprint(stored.GetGeneration()))
						paused, _, _ := unstructured.NestedBool(stored.Object, "spec", "suspend")
						if actor == "grant" && !paused {
							t.Fatal("installer resumed the author's workload")
						}
						if actor == "grant" && failure == "changed UID" {
							stored.SetUID("replacement")
						}
						if actor == "grant" && failure == "changed spec" {
							_ = unstructured.SetNestedField(stored.Object, "unexpected", "spec", "interval")
						}
						return true, stored.DeepCopy(), nil
					}
				}
				installer.PrependReactor("patch", resource.Resource, patch("grant"))
				author.PrependReactor("patch", resource.Resource, patch("resume"))
				got, err := s.createWorkload(t.Context(), resource, desired)
				if failure != "" {
					if err == nil {
						t.Fatal("accepted a refused or altered handoff")
					}
					paused, _, _ := unstructured.NestedBool(stored.Object, "spec", "suspend")
					if !paused {
						t.Fatal("failed handoff resumed the workload")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				paused, _, _ := unstructured.NestedBool(got.Object, "spec", "suspend")
				apply, _, _ := unstructured.NestedString(got.Object, "spec", "policy", "apply")
				if paused || apply != "Always" || got.GetGeneration() != 3 {
					t.Fatal(got)
				}
				if len(installer.Actions()) != 1 || len(author.Actions()) != 2 {
					t.Fatal("unexpected writer", installer.Actions(), author.Actions())
				}
				originalPaused, _, _ := unstructured.NestedBool(desired.Object, "spec", "suspend")
				if originalPaused || desired.GetUID() != "" {
					t.Fatal("modified the caller's desired object")
				}
			})
		}
	}
}

func TestCapacityOrdinaryWritesUseTheAuthor(t *testing.T) {
	installer := fake.NewSimpleDynamicClient(runtime.NewScheme())
	author := fake.NewSimpleDynamicClient(runtime.NewScheme())
	installer.PrependReactor("patch", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("installer must not author input changes")
	})
	for _, resource := range []string{"ptahschemas", "ptahmigrations"} {
		author.PrependReactor("patch", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.Unstructured{}, nil
		})
	}
	s := &scenarios{dynamic: installer, author: author, in: inputs{namespace: "work"}, load: workload{Schemas: 1, Migrations: 1}}
	if err := s.patchReference(t.Context(), schemaResource, s.schemaName(0), "desired", "oci://registry/schema@sha256:changed"); err != nil {
		t.Fatal(err)
	}
	if err := s.patchReference(t.Context(), migrationResource, s.migrationName(0), "artifact", "oci://registry/migration@sha256:changed"); err != nil {
		t.Fatal(err)
	}
	if len(author.Actions()) != 2 || len(installer.Actions()) != 0 {
		t.Fatal("input change used installation privileges")
	}
}
