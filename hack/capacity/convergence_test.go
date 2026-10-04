package main

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func convergenceScenarios(t *testing.T) *scenarios {
	t.Helper()
	s := soakScenarios()
	var objects []runtime.Object
	for _, family := range []string{"schema", "migration"} {
		for i := range 10 {
			object := soakObject(t, s, family, i)
			object.Object["status"].(map[string]any)["phase"] = "InSync"
			if family == "schema" {
				_ = unstructured.SetNestedField(object.Object, digestOf(s.schemaReference(i, 0)), "status", "applied", "artifactDigest")
			}
			objects = append(objects, object)
		}
	}
	s.dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList"}, objects...)
	return s
}

func TestConvergenceRemembersEachFreshResultAcrossOverlappingRefreshes(t *testing.T) {
	s := convergenceScenarios(t)
	client := s.dynamic.(*dynamicfake.FakeDynamicClient)
	reads := map[string]int{}
	last, err := client.Tracker().Get(migrationResource, s.in.namespaceFor(9), s.migrationName(9))
	if err != nil {
		t.Fatal(err)
	}
	waiting := last.(*unstructured.Unstructured).DeepCopy()
	_ = unstructured.SetNestedField(waiting.Object, "ReadingHistory", "status", "phase")
	_ = unstructured.SetNestedMap(waiting.Object, map[string]any{"type": "History", "id": "refresh", "startedAt": time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}, "status", "activeOperation")
	if err := client.Tracker().Update(migrationResource, waiting, waiting.GetNamespace()); err != nil {
		t.Fatal(err)
	}
	// The last resource finishes only after the first starts its next refresh.
	// There is no instant at which all 20 resources are idle.
	client.PrependReactor("get", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		reads[name]++
		if name == s.migrationName(9) && reads[name] == 1 {
			object, err := client.Tracker().Get(a.GetResource(), a.GetNamespace(), name)
			if err != nil {
				return true, nil, err
			}
			busy := object.(*unstructured.Unstructured).DeepCopy()
			_ = unstructured.SetNestedMap(busy.Object, map[string]any{"type": "History", "id": "refresh", "startedAt": time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}, "status", "activeOperation")
			first, err := client.Tracker().Get(schemaResource, s.in.namespaceFor(0), s.schemaName(0))
			if err != nil {
				return true, nil, err
			}
			refreshing := first.(*unstructured.Unstructured).DeepCopy()
			_ = unstructured.SetNestedField(refreshing.Object, "Observing", "status", "phase")
			_ = unstructured.SetNestedMap(refreshing.Object, map[string]any{"type": "Observe", "id": "next-refresh", "startedAt": time.Now().UTC().Format(time.RFC3339)}, "status", "activeOperation")
			if err := client.Tracker().Update(schemaResource, refreshing, refreshing.GetNamespace()); err != nil {
				return true, nil, err
			}
			if err := client.Tracker().Update(migrationResource, last, waiting.GetNamespace()); err != nil {
				return true, nil, err
			}
			return true, busy, nil
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.waitConverged(ctx, time.Now().Add(-2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 20 || reads[s.schemaName(0)] != 1 || reads[s.migrationName(9)] != 2 {
		t.Fatal("convergence did not retain all original accepted readings", reads)
	}
}

func TestConvergenceInventoryRefusesIncompleteOrChangedInputs(t *testing.T) {
	for _, mode := range []string{"missing", "UID", "generation", "deleting", "reference", "changed reference", "missing changed resource"} {
		t.Run(mode, func(t *testing.T) {
			s := convergenceScenarios(t)
			client := s.dynamic.(*dynamicfake.FakeDynamicClient)
			resource := client.Resource(schemaResource).Namespace(s.in.namespaceFor(0))
			object, err := resource.Get(context.Background(), s.schemaName(0), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			references := map[string]string{}
			switch mode {
			case "missing":
				err = resource.Delete(context.Background(), object.GetName(), metav1.DeleteOptions{})
			case "UID":
				object.SetUID("")
			case "generation":
				object.SetGeneration(0)
			case "deleting":
				now := metav1.Now()
				object.SetDeletionTimestamp(&now)
			case "reference":
				unstructured.RemoveNestedField(object.Object, "spec", "desired", "ociRef")
			case "changed reference":
				references["PtahSchema/"+object.GetName()] = "oci://registry/changed@sha256:" + strings.Repeat("f", 64)
			case "missing changed resource":
				references["PtahSchema/absent"] = s.schemaReference(0, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != "missing" {
				_, err = resource.Update(context.Background(), object, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.convergenceTargets(context.Background(), references); err == nil {
				t.Fatalf("%s inventory passed", mode)
			}
		})
	}
}
