package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	ptah "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

func faultReading(t *testing.T, engine string) (*ptah.PtahMigration, *ptah.PtahMigration) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/e2e/readings/uncertain-migration-retained-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var readings map[string]*ptah.PtahMigration
	if err := json.Unmarshal(raw, &readings); err != nil {
		t.Fatal(err)
	}
	current := readings[engine].DeepCopy()
	current.APIVersion, current.Kind = "operator.ptah.run/v1alpha1", "PtahMigration"
	current.UID, current.Namespace, current.Generation = "original-migration", "one", 3
	current.Status.ObservedGeneration = current.Generation
	current.Spec.Suspend = true
	copy, err := json.Marshal(current.Status.UnresolvedRun)
	if err != nil {
		t.Fatal(err)
	}
	current.Annotations = map[string]string{ptah.UnresolvedRunAnnotation: string(copy)}
	original := current.DeepCopy()
	original.Generation--
	original.Spec.Suspend = false
	original.Status.History = &ptah.MigrationHistoryStatus{TargetIdentityDigest: current.Status.UnresolvedRun.TargetIdentityDigest}
	original.Status.UnresolvedRun, original.Annotations = nil, nil
	current.Status.ActiveOperation = nil
	return original, current
}

func faultObject(t *testing.T, v any) *unstructured.Unstructured {
	t.Helper()
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(v)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: object}
}

func TestRetentionFaultRequiresOriginalUnknownRunAndMetadataCopy(t *testing.T) {
	for _, engine := range []string{"mysql", "postgresql"} {
		for _, mode := range []string{"valid", "replaced", "active", "stale generation", "resumed", "missing copy", "changed copy", "wrong operation", "wrong job", "wrong plan", "wrong target", "old recording", "wrong last run", "wrong started time"} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				original, current := faultReading(t, engine)
				switch mode {
				case "replaced":
					current.UID = "other"
				case "active":
					current.Status.ActiveOperation = original.Status.ActiveOperation.DeepCopy()
				case "stale generation":
					current.Status.ObservedGeneration--
				case "resumed":
					current.Spec.Suspend = false
				case "missing copy":
					current.Annotations = nil
				case "changed copy":
					current.Annotations[ptah.UnresolvedRunAnnotation] = "{}"
				case "wrong operation":
					current.Status.UnresolvedRun.OperationID = "other"
				case "wrong job":
					current.Status.UnresolvedRun.JobUID = "other"
				case "wrong plan":
					current.Status.UnresolvedRun.PlanRef.UID = "other"
				case "wrong target":
					current.Status.UnresolvedRun.TargetIdentityDigest = "other"
				case "old recording":
					current.Status.UnresolvedRun.RecordedAt = original.Status.ActiveOperation.StartedAt
				case "wrong last run":
					current.Status.LastRun.Outcome = ptah.MigrationRunOutcomeApplied
				case "wrong started time":
					current.Status.LastRun.StartedAt = metav1.NewTime(current.Status.LastRun.StartedAt.Add(-time.Second))
				}
				// Keep both copies mutually consistent: target, time and claim assertions
				// must reject their mutations independently of the metadata comparison.
				if mode != "missing copy" && mode != "changed copy" {
					raw, _ := json.Marshal(current.Status.UnresolvedRun)
					current.Annotations[ptah.UnresolvedRunAnnotation] = string(raw)
				}
				if got := faultUnresolved(original, faultObject(t, current)); got != (mode == "valid") {
					t.Fatalf("accepted=%v", got)
				}
			})
		}
	}
}

func TestRetentionFaultPreservesPendingApprovalAndUnresolvedEvidence(t *testing.T) {
	for _, mode := range []string{"valid", "empty before", "empty after", "changed approval UID", "changed approval spec", "consumed before", "consumed after", "missing unknown", "missing copy", "changed migration spec", "wrong namespace"} {
		t.Run(mode, func(t *testing.T) {
			_, native := faultReading(t, "postgresql")
			migration := faultObject(t, native)
			approval := pinSource("PtahSchemaApproval")
			approval.Object["spec"] = map[string]any{"planRef": map[string]any{"name": "plan", "uid": "plan-uid"}}
			inventory := func() retentionInventory {
				return retentionInventory{Objects: []retainedObject{{schemaApprovalResource, approval.DeepCopy()}, {migrationResource, migration.DeepCopy()}}}
			}
			before, after := inventory(), inventory()
			switch mode {
			case "empty before":
				before.Objects = nil
			case "empty after":
				after.Objects = nil
			case "changed approval UID":
				after.Objects[0].Object.SetUID("replacement")
			case "changed approval spec":
				_ = unstructured.SetNestedField(after.Objects[0].Object.Object, "replacement", "spec", "planRef", "uid")
			case "consumed before", "consumed after":
				object := after.Objects[0].Object
				if mode == "consumed before" {
					object = before.Objects[0].Object
				}
				_ = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"type": "Consumed", "status": "True"}}, "status", "conditions")
			case "missing unknown":
				unstructured.RemoveNestedField(after.Objects[1].Object.Object, "status", "unresolvedRun")
			case "missing copy":
				after.Objects[1].Object.SetAnnotations(nil)
			case "changed migration spec":
				_ = unstructured.SetNestedField(after.Objects[1].Object.Object, false, "spec", "suspend")
			case "wrong namespace":
				after.Objects[1].Object.SetNamespace("other")
			}
			if err := faultEvidencePreserved(before, after, approval, migration); (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
		})
	}
}

func TestRetentionFaultRefusesReplayedOrMissingApply(t *testing.T) {
	for _, mode := range []string{"valid", "original expired", "replay expired", "empty", "wrong name", "wrong namespace", "no claim"} {
		t.Run(mode, func(t *testing.T) {
			original, _ := faultReading(t, "postgresql")
			op := original.Status.ActiveOperation
			start := op.StartedAt.Add(-time.Second)
			job := jobRecord{Namespace: original.Namespace, Name: op.JobName, UID: string(op.JobUID), Resource: original.Name, Family: "migration", Operation: "apply", Created: op.StartedAt.Time}
			// The first record may have disappeared from the current API list; the
			// retained sampler record must still count and must expose a later replay.
			jobs := []jobRecord{job}
			switch mode {
			case "original expired":
				jobs = append(jobs, job)
			case "replay expired":
				replay := job
				replay.UID = "replayed"
				jobs = append(jobs, replay)
			case "empty":
				jobs = nil
			case "wrong name":
				jobs[0].Name = "other"
			case "wrong namespace":
				jobs[0].Namespace = "other"
			case "no claim":
				original.Status.ActiveOperation = nil
			}
			if err := faultNoReplay(jobs, original, start); (err == nil) != (mode == "valid" || mode == "original expired") {
				t.Fatal(err)
			}
		})
	}
}

func TestRetentionFaultPatchBindsIdentityAndUnrelatedSpec(t *testing.T) {
	for _, mode := range []string{"valid", "UID", "namespace", "generation", "spec"} {
		t.Run(mode, func(t *testing.T) {
			s, fake, paused, _ := maintenanceFixture(t)
			original, err := s.dynamic.Resource(migrationResource).Namespace("one").Get(t.Context(), paused[0].target.name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			fake.PrependReactor("patch", "ptahmigrations", func(action clienttesting.Action) (bool, runtime.Object, error) {
				var patch map[string]any
				if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &patch); err != nil {
					t.Fatal(err)
				}
				meta := patch["metadata"].(map[string]any)
				if meta["uid"] != string(original.GetUID()) || meta["resourceVersion"] != original.GetResourceVersion() {
					t.Fatal("unconditional patch")
				}
				result := original.DeepCopy()
				result.SetGeneration(original.GetGeneration() + 1)
				_ = unstructured.SetNestedField(result.Object, false, "spec", "suspend")
				switch mode {
				case "UID":
					result.SetUID("other")
				case "namespace":
					result.SetNamespace("other")
				case "generation":
					result.SetGeneration(original.GetGeneration())
				case "spec":
					_ = unstructured.SetNestedField(result.Object, "other", "spec", "artifact", "ociRef")
				}
				return true, result, nil
			})
			_, err = s.patchFaultSpec(t.Context(), migrationResource, original, map[string]any{"suspend": false})
			if (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
		})
	}
}
