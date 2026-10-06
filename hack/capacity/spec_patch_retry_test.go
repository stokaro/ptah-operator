package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/retry"
)

func TestCapacitySpecPatchRetriesOnlyUnchangedDesiredState(t *testing.T) {
	for _, resource := range []schema.GroupVersionResource{schemaResource, migrationResource} {
		for _, change := range []string{"status", "UID", "generation", "spec", "deletion", "read failure", "persistent conflict", "write failure"} {
			t.Run(resource.Resource+"/"+change, func(t *testing.T) {
				kind := "PtahSchema"
				if resource == migrationResource {
					kind = "PtahMigration"
				}
				original := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "operator.ptah.run/v1alpha1", "kind": kind,
					"metadata": map[string]any{"name": "work", "namespace": "work", "uid": "original", "resourceVersion": "10", "generation": int64(1)},
					"spec":     map[string]any{"suspend": true, "interval": "2m"},
				}}
				before := original.DeepCopy()
				stored := original.DeepCopy()
				stored.SetResourceVersion("11")
				stored.Object["status"] = map[string]any{"phase": "Suspended"}
				switch change {
				case "UID":
					stored.SetUID("replacement")
				case "generation":
					stored.SetGeneration(3)
				case "spec":
					_ = unstructured.SetNestedField(stored.Object, "5m", "spec", "interval")
				case "deletion":
					now := metav1.Now()
					stored.SetDeletionTimestamp(&now)
				}
				writer := fake.NewSimpleDynamicClient(runtime.NewScheme())
				reads, writes := 0, 0
				writer.PrependReactor("get", resource.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
					reads++
					if change == "read failure" {
						return true, nil, errors.New("read unavailable")
					}
					return true, stored.DeepCopy(), nil
				})
				writer.PrependReactor("patch", resource.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
					writes++
					var patch map[string]any
					if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &patch); err != nil {
						t.Fatal(err)
					}
					meta := patch["metadata"].(map[string]any)
					version := original.GetResourceVersion()
					if writes > 1 {
						version = stored.GetResourceVersion()
					}
					if meta["uid"] != string(original.GetUID()) || meta["resourceVersion"] != version {
						t.Fatal("patch lost identity or retried a stale version", meta)
					}
					if change == "write failure" {
						return true, nil, errors.New("ambiguous write failure")
					}
					if writes == 1 || change == "persistent conflict" {
						return true, nil, apierrors.NewConflict(resource.GroupResource(), original.GetName(), errors.New("status changed"))
					}
					if change != "status" {
						t.Fatal("retried after the desired state or identity changed")
					}
					result := stored.DeepCopy()
					_ = unstructured.SetNestedField(result.Object, false, "spec", "suspend")
					result.SetGeneration(2)
					result.SetResourceVersion("12")
					return true, result, nil
				})
				result, err := patchCapacitySpec(t.Context(), writer, resource, original, map[string]any{"suspend": false})
				switch change {
				case "status":
					if err != nil || result == nil || writes != 2 || reads != 1 {
						t.Fatalf("status-only conflict did not recover: writes=%d reads=%d error=%v", writes, reads, err)
					}
					if !reflect.DeepEqual(result.Object["status"], stored.Object["status"]) {
						t.Fatal("overwrote the controller's status")
					}
				case "persistent conflict":
					if !apierrors.IsConflict(err) || writes != retry.DefaultRetry.Steps || reads != writes-1 {
						t.Fatalf("conflict retry is not bounded: writes=%d reads=%d error=%v", writes, reads, err)
					}
				default:
					if err == nil || writes != 1 {
						t.Fatalf("unsafe failure handling: writes=%d error=%v", writes, err)
					}
					if change == "write failure" && reads != 0 {
						t.Fatal("retried an ambiguous error")
					}
				}
				if !reflect.DeepEqual(original, before) {
					t.Fatal("changed the caller's precondition")
				}
			})
		}
	}
}
