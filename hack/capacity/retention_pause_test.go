package main

import (
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRetentionRefusesInconsistentWorkloadPlanOwnership(t *testing.T) {
	for _, mode := range []string{"valid", "missing owner", "wrong namespace", "wrong family", "wrong spec owner", "wrong owner kind"} {
		t.Run(mode, func(t *testing.T) {
			s, _, paused, inventory := maintenanceFixture(t)
			for _, entry := range inventory.Objects {
				if entry.Resource != migrationPlanResource || entry.Object.GetName() != "old-plan" {
					continue
				}
				switch mode {
				case "missing owner":
					entry.Object.SetOwnerReferences(nil)
				case "wrong namespace":
					entry.Object.SetNamespace("elsewhere")
				case "wrong family":
					paused[0].target.family = "schema"
				case "wrong spec owner":
					_ = unstructured.SetNestedField(entry.Object.Object, "other", "spec", "migrationRef", "uid")
				case "wrong owner kind":
					owners := entry.Object.GetOwnerReferences()
					owners[0].Kind = "PtahSchema"
					entry.Object.SetOwnerReferences(owners)
				}
			}
			if err := s.validateMaintenanceInventory(inventory, paused); (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestMaintenanceWaitsForTerminalJobs(t *testing.T) {
	s, _, paused, _ := maintenanceFixture(t)
	for _, mode := range []string{"pending", "active", "complete", "failed", "complete but active"} {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "one"}}
		switch mode {
		case "active":
			job.Status.Active = 1
		case "complete", "complete but active":
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
			if mode == "complete but active" {
				job.Status.Active = 1
			}
		case "failed":
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
		}
		s.clientset = kubefake.NewClientset(job)
		ok, err := s.maintenanceQuiet(t.Context(), paused)
		if err != nil || ok != (mode == "complete" || mode == "failed") {
			t.Fatal(mode, ok, err)
		}
	}
}

func TestMaintenanceRecoversAmbiguousSuspensionAndFreshConflicts(t *testing.T) {
	for _, mode := range []string{"lost committed response", "failed before write", "conflict", "changed spec"} {
		t.Run(mode, func(t *testing.T) {
			s, fake, paused, _ := maintenanceFixture(t)
			// The real API defaults suspend=false before the first request.
			original, _ := fake.Tracker().Get(migrationResource, "one", paused[0].target.name)
			object := original.(*unstructured.Unstructured).DeepCopy()
			object.SetGeneration(1)
			_ = unstructured.SetNestedField(object.Object, false, "spec", "suspend")
			_ = unstructured.SetNestedField(object.Object, int64(1), "status", "observedGeneration")
			if err := fake.Tracker().Update(migrationResource, object, "one"); err != nil {
				t.Fatal(err)
			}
			s.load.Schemas = 10
			s.load.Migrations = 10
			s.inputReader = clientfake.NewClientBuilder().Build()
			calls := 0
			fake.PrependReactor("patch", "ptahmigrations", func(a clienttesting.Action) (bool, runtime.Object, error) {
				calls++
				if calls == 1 && mode == "failed before write" {
					return true, nil, errors.New("transport failed")
				}
				if calls == 1 && mode == "conflict" {
					object.SetResourceVersion("18")
					if err := fake.Tracker().Update(migrationResource, object, "one"); err != nil {
						t.Fatal(err)
					}
					return true, nil, apierrors.NewConflict(migrationResource.GroupResource(), object.GetName(), errors.New("status advanced"))
				}
				updated := object.DeepCopy()
				updated.SetGeneration(2)
				_ = unstructured.SetNestedField(updated.Object, true, "spec", "suspend")
				if mode == "changed spec" {
					_ = unstructured.SetNestedField(updated.Object, "changed", "spec", "interval")
				}
				if err := fake.Tracker().Update(migrationResource, updated, "one"); err != nil {
					t.Fatal(err)
				}
				if mode == "lost committed response" {
					return true, nil, errors.New("response lost")
				}
				return true, updated, nil
			})
			recovered, archive, err := s.exportAndSuspend(t.Context(), "migration", 0, 0)
			if (err == nil) != (mode == "conflict") || recovered.target.uid == "" || archive.Path == "" {
				t.Fatal("lost recovery identity", mode, recovered, archive, err)
			}
			if e := verifyRetentionArchives(s.evidenceDir, []retentionArchive{archive}); e != nil {
				t.Fatal(e)
			}
			fake.PrependReactor("patch", "ptahmigrations", func(a clienttesting.Action) (bool, runtime.Object, error) {
				updated := object.DeepCopy()
				updated.SetGeneration(3)
				if err := fake.Tracker().Update(migrationResource, updated, "one"); err != nil {
					t.Fatal(err)
				}
				return true, updated, nil
			})
			_, resumeErr := s.resumeMaintenanceResource(t.Context(), recovered)
			if (resumeErr == nil) != (mode != "changed spec") {
				t.Fatal(mode, resumeErr)
			}
			if mode != "changed spec" {
				raw, _ := fake.Tracker().Get(migrationResource, "one", object.GetName())
				suspended, _, _ := unstructured.NestedBool(raw.(*unstructured.Unstructured).Object, "spec", "suspend")
				if suspended {
					t.Fatal("resource left suspended")
				}
			}
		})
	}
}
