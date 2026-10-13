package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMaintenanceSuspendsIdlePeersBeforeWaiting(t *testing.T) {
	for _, mode := range []string{"active operation", "status conflict", "lost suspension response"} {
		t.Run(mode, func(t *testing.T) {
			s, fake, _, _ := maintenanceFixture(t)
			s.load.Schemas, s.load.Migrations = 10, 10
			s.inputReader = clientfake.NewClientBuilder().Build()
			first, second := s.migrationName(0), s.migrationName(1)
			for index, name := range []string{first, second} {
				object := soakObject(t, s, "migration", index)
				object.SetGeneration(1)
				_ = unstructured.SetNestedField(object.Object, int64(1), "status", "observedGeneration")
				_ = unstructured.SetNestedField(object.Object, false, "spec", "suspend")
				if index == 0 {
					if err := fake.Tracker().Update(migrationResource, object, "one"); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := fake.Tracker().Create(migrationResource, object, "one"); err != nil {
						t.Fatal(err)
					}
					original, err := fake.Tracker().Get(migrationPlanResource, "one", "current-plan")
					if err != nil {
						t.Fatal(err)
					}
					plan := original.(*unstructured.Unstructured).DeepCopy()
					plan.SetName("second-plan")
					plan.SetUID("second-plan-uid")
					plan.Object["spec"] = map[string]any{"migrationRef": map[string]any{"name": name, "uid": string(object.GetUID())}}
					if err := fake.Tracker().Create(migrationPlanResource, plan, "one"); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.load.Schemas, s.load.Migrations = 0, 2
			secondSuspended := false
			var writes []string
			fake.PrependReactor("get", "ptahmigrations", func(a clienttesting.Action) (bool, runtime.Object, error) {
				name := a.(clienttesting.GetAction).GetName()
				if name != first || secondSuspended || mode != "active operation" {
					return false, nil, nil
				}
				original, err := fake.Tracker().Get(migrationResource, "one", name)
				if err != nil {
					t.Fatal(err)
				}
				object := original.(*unstructured.Unstructured).DeepCopy()
				_ = unstructured.SetNestedMap(object.Object, map[string]any{"type": "History"}, "status", "activeOperation")
				return true, object, nil
			})
			fake.PrependReactor("patch", "ptahmigrations", func(a clienttesting.Action) (bool, runtime.Object, error) {
				patch := a.(clienttesting.PatchAction)
				if patch.GetName() == first && !secondSuspended && mode == "status conflict" {
					return true, nil, apierrors.NewConflict(migrationResource.GroupResource(), first, errors.New("status advanced"))
				}
				original, err := fake.Tracker().Get(migrationResource, "one", patch.GetName())
				if err != nil {
					t.Fatal(err)
				}
				object := original.(*unstructured.Unstructured).DeepCopy()
				var body struct {
					Metadata struct{ UID, ResourceVersion string }
					Spec     map[string]any
				}
				if err := json.Unmarshal(patch.GetPatch(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Metadata.UID != string(object.GetUID()) || body.Metadata.ResourceVersion != object.GetResourceVersion() || !reflect.DeepEqual(body.Spec, map[string]any{"suspend": true}) {
					t.Fatalf("suspension lost its preconditions or changed another input: %s", patch.GetPatch())
				}
				object.SetGeneration(2)
				_ = unstructured.SetNestedField(object.Object, true, "spec", "suspend")
				if err := fake.Tracker().Update(migrationResource, object, "one"); err != nil {
					t.Fatal(err)
				}
				writes = append(writes, patch.GetName())
				secondSuspended = secondSuspended || patch.GetName() == second
				if mode == "lost suspension response" && patch.GetName() == second {
					return true, nil, errors.New("response lost after write")
				}
				return true, object, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			paused, archives, err := s.suspendMaintenance(ctx, 0)
			if (err != nil) != (mode == "lost suspension response") || len(paused) != 2 || len(archives) != 2 {
				t.Fatalf("lost suspension or recovery population: paused=%d archives=%d err=%v", len(paused), len(archives), err)
			}
			if mode != "lost suspension response" && !reflect.DeepEqual(writes, []string{second, first}) {
				t.Fatalf("idle peer waited behind a busy resource: %v", writes)
			}
			if err := verifyRetentionArchives(s.evidenceDir, archives); err != nil {
				t.Fatal(err)
			}
		})
	}
}
