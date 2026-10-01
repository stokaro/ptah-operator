package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func maintenanceFixture(t *testing.T) (*scenarios, *dynamicfake.FakeDynamicClient, []pausedResource, retentionInventory) {
	t.Helper()
	s := soakScenarios()
	s.evidenceDir = t.TempDir()
	s.in.namespace = "one"
	s.in.namespaces = []string{"one"}
	object := soakObject(t, s, "migration", 0)
	s.load.Schemas = 0
	s.load.Migrations = 1
	object.SetGeneration(2)
	_ = unstructured.SetNestedField(object.Object, true, "spec", "suspend")
	_ = unstructured.SetNestedField(object.Object, int64(2), "status", "observedGeneration")
	_ = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "Suspended", "observedGeneration": int64(2)}}, "status", "conditions")
	_ = unstructured.SetNestedMap(object.Object, map[string]any{"name": "current-plan", "uid": "current-plan-uid"}, "status", "plan")
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	paused := []pausedResource{{target: batchTarget{family: "migration", resource: migrationResource, namespace: "one", name: object.GetName(), uid: object.GetUID(), generation: 2}, spec: spec}}
	objects := []runtime.Object{object}
	for _, name := range []string{"old-plan", "current-plan"} {
		plan := pinSource("PtahMigrationPlan")
		plan.SetName(name)
		plan.SetUID(types.UID(name + "-uid"))
		plan.SetResourceVersion("4")
		plan.Object["spec"] = map[string]any{"migrationRef": map[string]any{"name": object.GetName(), "uid": string(object.GetUID())}}
		controller := true
		plan.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "operator.ptah.run/v1alpha1", Kind: "PtahMigration", Name: object.GetName(), UID: object.GetUID(), Controller: &controller}})
		objects = append(objects, plan)
	}
	child := pinSource("ConfigMap")
	child.SetAPIVersion("v1")
	child.SetName("old-child")
	child.SetUID("child-uid")
	child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "operator.ptah.run/v1alpha1", Kind: "PtahMigrationPlan", Name: "old-plan", UID: "old-plan-uid"}})
	child.Object["binaryData"] = map[string]any{"chunk": "c3Fs"}
	objects = append(objects, child)
	kinds := map[schema.GroupVersionResource]string{schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList", schemaPlanResource: "PtahSchemaPlanList", migrationPlanResource: "PtahMigrationPlanList", planChunkResource: "PtahSchemaPlanChunkList", configMapResource: "ConfigMapList", approvalGVR: "PtahMigrationApprovalList", {Group: schemaResource.Group, Version: schemaResource.Version, Resource: "ptahschemaapprovals"}: "PtahSchemaApprovalList"}
	fake := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
	s.dynamic = fake
	s.clientset = kubefake.NewClientset()
	before, err := s.retentionInventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s, fake, paused, before
}

func TestRetentionPrunesOnlyExportedUnpinnedIdentities(t *testing.T) {
	for _, mode := range []string{"valid", "new approval", "new unresolved copy", "changed spec", "changed UID", "new child", "changed child", "lost archive", "corrupt archive", "lost resource inventory", "active operation", "missing GC", "foreign replacement"} {
		t.Run(mode, func(t *testing.T) {
			s, fake, paused, before := maintenanceFixture(t)
			archive, err := writeRetentionEvidence(s.evidenceDir, "retention/before.json", before)
			if err != nil {
				t.Fatal(err)
			}
			proof := retentionProof{Archives: []retentionArchive{archive}}
			if mode == "lost archive" {
				if err := os.Remove(filepath.Join(s.evidenceDir, archive.Path)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt archive" {
				if err := os.WriteFile(filepath.Join(s.evidenceDir, archive.Path), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "new approval" {
				approval := pinSource("PtahMigrationApproval")
				approval.Object["spec"] = map[string]any{"planRef": map[string]any{"name": "old-plan", "uid": "old-plan-uid"}}
				if err := fake.Tracker().Create(approvalGVR, approval, "one"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "new unresolved copy" || mode == "active operation" {
				raw, _ := fake.Tracker().Get(migrationResource, "one", paused[0].target.name)
				o := raw.(*unstructured.Unstructured).DeepCopy()
				if mode == "new unresolved copy" {
					o.SetAnnotations(map[string]string{"operator.ptah.run/unresolved-run": `{"planRef":{"name":"old-plan","uid":"old-plan-uid"}}`})
				} else {
					_ = unstructured.SetNestedMap(o.Object, map[string]any{"type": "History"}, "status", "activeOperation")
				}
				if err := fake.Tracker().Update(migrationResource, o, "one"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed spec" || mode == "changed UID" {
				raw, _ := fake.Tracker().Get(migrationPlanResource, "one", "old-plan")
				o := raw.(*unstructured.Unstructured).DeepCopy()
				if mode == "changed UID" {
					o.SetUID("replacement")
				} else {
					_ = unstructured.SetNestedField(o.Object, "changed", "spec", "fingerprint")
				}
				if err := fake.Tracker().Update(migrationPlanResource, o, "one"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "new child" || mode == "changed child" {
				raw, _ := fake.Tracker().Get(configMapResource, "one", "old-child")
				o := raw.(*unstructured.Unstructured).DeepCopy()
				if mode == "new child" {
					o.SetName("extra-child")
					o.SetUID("extra")
					err = fake.Tracker().Create(configMapResource, o, "one")
				} else {
					_ = unstructured.SetNestedField(o.Object, "dGFtcGVyZWQ=", "binaryData", "chunk")
					err = fake.Tracker().Update(configMapResource, o, "one")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "lost resource inventory" {
				fake.PrependReactor("list", "ptahmigrations", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahMigrationList"}}, nil
				})
			}
			deleted := 0
			fake.PrependReactor("delete", "ptahmigrationplans", func(a clienttesting.Action) (bool, runtime.Object, error) {
				deleted++
				action := a.(clienttesting.DeleteAction)
				options := action.GetDeleteOptions()
				if action.GetName() != "old-plan" || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "old-plan-uid" || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "4" {
					t.Fatal("unbound or pinned deletion", action.GetName(), options)
				}
				if mode == "foreign replacement" {
					raw, _ := fake.Tracker().Get(migrationPlanResource, "one", "old-plan")
					o := raw.(*unstructured.Unstructured).DeepCopy()
					o.SetUID("foreign")
					if err := fake.Tracker().Delete(migrationPlanResource, "one", "old-plan"); err != nil {
						t.Fatal(err)
					}
					if err := fake.Tracker().Create(migrationPlanResource, o, "one"); err != nil {
						t.Fatal(err)
					}
					return true, nil, nil
				}
				if mode != "missing GC" {
					if err := fake.Tracker().Delete(configMapResource, "one", "old-child"); err != nil {
						t.Fatal(err)
					}
				}
				return false, nil, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err = s.pruneRetention(ctx, paused, before, &proof)
			switch mode {
			case "valid":
				if err != nil || deleted != 1 || len(proof.Deleted) != 1 || !proof.Deleted[0].GarbageCollected {
					t.Fatal(err, deleted, proof)
				}
			case "new approval", "new unresolved copy":
				if err != nil || deleted != 0 {
					t.Fatal("new pin did not protect its plan", err, deleted)
				}
			case "missing GC", "foreign replacement":
				if err == nil || deleted != 1 || len(proof.Deleted) != 1 || proof.Deleted[0].GarbageCollected {
					t.Fatal("unproven GC passed", err, deleted, proof)
				}
			default:
				if err == nil || deleted != 0 {
					t.Fatal("unsafe deletion was not refused", err, deleted)
				}
			}
			if _, err := fake.Tracker().Get(migrationPlanResource, "one", "current-plan"); err != nil {
				t.Fatal("deleted pinned plan", err)
			}
		})
	}
}

func TestMaintenanceQuiescenceIncludesActualJobsAndPods(t *testing.T) {
	s, _, paused, _ := maintenanceFixture(t)
	if ok, err := s.maintenanceQuiet(t.Context(), paused); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodUnknown} {
		s.clientset = kubefake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "still-runs", Namespace: "one"}, Status: corev1.PodStatus{Phase: phase}})
		if ok, err := s.maintenanceQuiet(t.Context(), paused); err != nil || ok {
			t.Fatal("nonterminal Pod ignored", phase, ok, err)
		}
	}
}

func TestMaintenanceResumeContinuesAfterAChangedResource(t *testing.T) {
	s, fake, paused, _ := maintenanceFixture(t)
	original, _ := fake.Tracker().Get(migrationResource, "one", paused[0].target.name)
	next := original.(*unstructured.Unstructured).DeepCopy()
	next.SetName("second")
	next.SetUID("second-uid")
	if err := fake.Tracker().Create(migrationResource, next, "one"); err != nil {
		t.Fatal(err)
	}
	p := paused[0]
	p.target.name = "second"
	p.target.uid = "second-uid"
	paused = append(paused, p)
	changed := original.(*unstructured.Unstructured).DeepCopy()
	changed.SetUID("foreign")
	if err := fake.Tracker().Update(migrationResource, changed, "one"); err != nil {
		t.Fatal(err)
	}
	patched := 0
	fake.PrependReactor("patch", "ptahmigrations", func(a clienttesting.Action) (bool, runtime.Object, error) {
		patched++
		action := a.(clienttesting.PatchAction)
		if action.GetName() != "second" {
			t.Fatal("resumed a foreign replacement")
		}
		var patch map[string]any
		if err := json.Unmarshal(action.GetPatch(), &patch); err != nil {
			t.Fatal(err)
		}
		metadata := patch["metadata"].(map[string]any)
		if metadata["uid"] != "second-uid" || metadata["resourceVersion"] != next.GetResourceVersion() {
			t.Fatal("resume lacks preconditions")
		}
		updated := next.DeepCopy()
		updated.SetGeneration(3)
		_ = unstructured.SetNestedField(updated.Object, false, "spec", "suspend")
		return true, updated, nil
	})
	targets, err := s.resumeMaintenance(t.Context(), paused)
	if err == nil || !strings.Contains(err.Error(), "replaced") || patched != 1 || len(targets) != 1 {
		t.Fatal("remaining resources were abandoned", targets, err, patched)
	}
}

func TestMaintenanceExportReadbackRefusesOverwriteAndPathEscape(t *testing.T) {
	root := t.TempDir()
	a, err := writeRetentionEvidence(root, "round/one.json", map[string]string{"proof": "bytes"})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRetentionArchives(root, []retentionArchive{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRetentionEvidence(root, a.Path, []string{"replace"}); err == nil {
		t.Fatal("overwrote earlier evidence")
	}
	for _, path := range []string{"../outside", "/absolute", ""} {
		if _, err := writeRetentionEvidence(root, path, nil); err == nil {
			t.Fatal(fmt.Sprintf("accepted %q", path))
		}
	}
}
