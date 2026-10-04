package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestSoakBatchRequiresEveryUpdateBindingAndActualPlanStore(t *testing.T) {
	for _, round := range []int{1, 9} {
		for _, mode := range []string{"bound status", "wrong previous artifact", "replaced UID", "unchanged generation"} {
			t.Run(fmt.Sprintf("round-%d/%s", round, mode), func(t *testing.T) {
				s := soakScenarios()
				objects := []runtime.Object{}
				for _, family := range []string{"schema", "migration"} {
					for i := range 5 {
						object := soakObject(t, s, family, i)
						field, ref := "desired", s.schemaReference(i, round-1)
						if family == "migration" {
							field, ref = "artifact", s.migrationReference(i, round-1)
						}
						if mode == "wrong previous artifact" {
							ref = "oci://wrong@sha256:" + strings.Repeat("f", 64)
						}
						if err := unstructured.SetNestedField(object.Object, ref, "spec", field, "ociRef"); err != nil {
							t.Fatal(err)
						}
						objects = append(objects, object)
					}
				}
				fake := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
				s.dynamic = fake
				submitted := 0
				fake.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
					patch := action.(clienttesting.PatchAction)
					submitted++
					original, err := fake.Tracker().Get(action.GetResource(), action.GetNamespace(), patch.GetName())
					if err != nil {
						t.Fatal(err)
					}
					object := original.(*unstructured.Unstructured).DeepCopy()
					var value map[string]any
					if err := json.Unmarshal(patch.GetPatch(), &value); err != nil {
						t.Fatal(err)
					}
					metadata := value["metadata"].(map[string]any)
					if metadata["uid"] != string(object.GetUID()) || metadata["resourceVersion"] != object.GetResourceVersion() {
						t.Fatal("update lacks UID/RV binding")
					}
					field, source := "desired", "source"
					if action.GetResource() == migrationResource {
						field, source = "artifact", "artifact"
					}
					reference := value["spec"].(map[string]any)[field].(map[string]any)["ociRef"].(string)
					_ = unstructured.SetNestedField(object.Object, reference, "spec", field, "ociRef")
					_ = unstructured.SetNestedField(object.Object, digestOf(reference), "status", source, "digest")
					object.SetGeneration(2)
					if mode == "unchanged generation" {
						object.SetGeneration(1)
					}
					object.SetResourceVersion("18")
					if mode == "replaced UID" {
						object.SetUID("replacement")
					}
					_ = unstructured.SetNestedField(object.Object, object.GetGeneration(), "status", "observedGeneration")
					conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
					for _, c := range conditions {
						c.(map[string]any)["observedGeneration"] = object.GetGeneration()
					}
					_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
					completed := time.Now().UTC().Format(time.RFC3339Nano)
					if source == "source" {
						_ = unstructured.SetNestedField(object.Object, completed, "status", "lastSuccessfulReconciliation")
						_ = unstructured.SetNestedField(object.Object, digestOf(reference), "status", "applied", "artifactDigest")
					} else {
						_ = unstructured.SetNestedField(object.Object, completed, "status", "history", "observedAt")
					}
					if err := fake.Tracker().Update(action.GetResource(), object, object.GetNamespace()); err != nil {
						t.Fatal(err)
					}
					return true, object, nil
				})
				err := s.changeRound(t.Context(), round)
				if err == nil {
					t.Fatal("batch passed without actual applied plans")
				}
				switch mode {
				case "bound status":
					if submitted != 10 || !strings.Contains(err.Error(), "input plan reader is missing") {
						t.Fatal("batch did not update and verify all ten resources", submitted, err)
					}
				case "wrong previous artifact":
					if submitted != 0 {
						t.Fatal("overwrote a changed source")
					}
				default:
					if submitted != 1 {
						t.Fatal("continued after identity failure", submitted)
					}
				}
				if submitted > 0 && (len(s.windows) != 1 || s.windows[0].Outcome["error"] == "") {
					t.Fatal("failed mutation disappeared from the report")
				}
				if mode == "bound status" {
					for i := range 5 {
						for _, resource := range []struct {
							family string
							name   string
						}{{"schema", s.schemaName(i)}, {"migration", s.migrationName(i)}} {
							gvr, ref, field := schemaResource, s.schemaReference(i, round), "desired"
							if resource.family == "migration" {
								gvr, ref, field = migrationResource, s.migrationReference(i, round), "artifact"
							}
							got, err := fake.Resource(gvr).Namespace(s.in.namespaceFor(i)).Get(t.Context(), resource.name, metav1.GetOptions{})
							if err != nil {
								t.Fatal(err)
							}
							actual, _, _ := unstructured.NestedString(got.Object, "spec", field, "ociRef")
							if actual != ref {
								t.Fatal("wrong input version", actual, ref)
							}
						}
					}
				}
			})
		}
	}
}
