package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	ptah "github.com/stokaro/ptah-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func backlogFixture() (approvalBacklogProof, []cycleHistory) {
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	proof := approvalBacklogProof{StartedAt: start, WaitingAt: start.Add(time.Minute), FirstApprovalAt: start.Add(3 * time.Minute)}
	var histories []cycleHistory
	for _, family := range []string{"schema", "migration"} {
		for ns := range 2 {
			h := cycleHistory{Family: family, Namespace: fmt.Sprintf("work-%d", ns), StartedAt: start.Add(-time.Minute), Cursor: "last"}
			for index := range 10 {
				if index%2 != ns {
					continue
				}
				kind := "PtahSchema"
				if family == "migration" {
					kind = "PtahMigration"
				}
				o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "operator.ptah.run/v1alpha1", "kind": kind, "metadata": map[string]any{"name": fmt.Sprintf("capacity-%s-%03d", family, index), "namespace": h.Namespace, "uid": fmt.Sprintf("%s-%d", family, index), "resourceVersion": fmt.Sprintf("patch-%d", index), "generation": int64(2)}, "spec": map[string]any{"policy": map[string]any{"apply": "OnApproval"}}}}
				final := o.DeepCopy()
				final.SetResourceVersion(fmt.Sprintf("final-%d", index))
				proof.After = append(proof.After, final)
				if index >= 5 {
					continue
				}
				approval := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "approve-" + string(o.GetUID()), "namespace": h.Namespace, "uid": "approval-" + string(o.GetUID()), "creationTimestamp": proof.FirstApprovalAt.Format(time.RFC3339)}, "spec": map[string]any{"planRef": map[string]any{"name": "plan-" + string(o.GetUID()), "uid": "plan-uid-" + string(o.GetUID())}, "planFingerprint": "fingerprint"}}}
				m := backlogMember{Family: family, Expected: o, Approval: approval}
				proof.Members = append(proof.Members, m)
				r := cycleReading{UID: string(o.GetUID()), Name: o.GetName(), Namespace: h.Namespace, Generation: 2, ResourceVersion: o.GetResourceVersion(), ReceivedAt: start.Add(time.Minute)}
				h.Readings = append(h.Readings, r)
				r.ResourceVersion = "apply-" + string(o.GetUID())
				r.ReceivedAt = proof.FirstApprovalAt.Add(2 * time.Second)
				r.Operation = &cycleOperation{Type: "Apply", ID: "claim-" + string(o.GetUID()), JobName: "job-" + string(o.GetUID()), JobUID: "job-uid-" + string(o.GetUID()), StartedAt: proof.FirstApprovalAt.Add(time.Second), ApprovalRef: &ptah.ImmutableObjectReference{Name: approval.GetName(), UID: approval.GetUID()}, PlanRef: &ptah.ImmutableObjectReference{Name: "plan-" + string(o.GetUID()), UID: types.UID("plan-uid-" + string(o.GetUID()))}}
				if family == "schema" {
					r.SchemaPlan = &cycleSchemaPlan{ImmutableObjectReference: *r.Operation.PlanRef, Approval: r.Operation.ApprovalRef}
					r.Operation.ApprovalRef, r.Operation.PlanRef = nil, nil
				}
				h.Readings = append(h.Readings, r)
				r.Operation = nil
				r.ResourceVersion = final.GetResourceVersion()
				r.ReceivedAt = proof.FirstApprovalAt.Add(time.Minute)
				h.Readings = append(h.Readings, r)
			}
			histories = append(histories, h)
		}
	}
	return proof, histories
}

func TestBacklogHistoryRequiresExactApprovalAndCompletePopulation(t *testing.T) {
	for _, mode := range []string{"valid", "no population", "wrong family count", "duplicate member", "missing history", "failed history", "late history", "no cursor", "duplicate history", "missing patch", "missing final", "no Apply", "early Apply", "future Apply", "changed Job", "wrong approval", "wrong plan", "replay", "changed generation", "missing dispatch", "wrong identity"} {
		t.Run(mode, func(t *testing.T) {
			p, h := backlogFixture()
			wantError, wantReady := true, false
			switch mode {
			case "valid":
				wantError, wantReady = false, true
			case "no population":
				p.Members = nil
			case "wrong family count":
				p.Members[0].Family = "migration"
			case "duplicate member":
				p.Members[1] = p.Members[0]
			case "missing history":
				h = h[1:]
			case "failed history":
				h[0].Error = "expired cursor"
			case "late history":
				h[0].StartedAt = p.StartedAt.Add(time.Second)
			case "no cursor":
				h[0].Cursor = ""
			case "duplicate history":
				h = append(h, h[0])
			case "missing patch":
				h[0].Readings = h[0].Readings[1:]
				wantError = false
			case "missing final":
				h[0].Readings[2].ResourceVersion = "different"
				wantError = false
			case "no Apply":
				h[0].Readings[1].Operation = nil
				wantError = false
			case "early Apply":
				h[0].Readings[1].Operation.StartedAt = p.FirstApprovalAt.Add(-time.Second)
			case "future Apply":
				h[0].Readings[1].Operation.StartedAt = h[0].Readings[1].ReceivedAt.Add(time.Second)
			case "changed Job":
				r := h[0].Readings[1]
				op := *r.Operation
				op.JobUID = "another-job"
				r.Operation = &op
				h[0].Readings = append(h[0].Readings, r)
			case "wrong approval":
				h[0].Readings[1].SchemaPlan.Approval.UID = "other-approval"
			case "wrong plan":
				h[0].Readings[1].SchemaPlan.UID = "other-plan"
			case "replay":
				r := h[0].Readings[1]
				op := *r.Operation
				op.ID = "replay"
				r.Operation = &op
				h[0].Readings = append(h[0].Readings, r)
			case "changed generation":
				h[0].Readings[1].Generation++
			case "missing dispatch":
				h[0].Readings[1].Operation.JobUID = ""
				wantError = false
			case "wrong identity":
				h[0].Readings[1].Namespace = "other"
			}
			ready, err := validateBacklogHistory(p, h)
			if (err != nil) != wantError || ready != wantReady {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestBacklogRequiresTheSuccessfulBoundApplyJob(t *testing.T) {
	for _, mode := range []string{"valid", "wrong uid", "wrong owner", "old Job", "unfinished", "failed", "wrong namespace", "wrong name", "active", "extra completion"} {
		t.Run(mode, func(t *testing.T) {
			p, h := backlogFixture()
			m := p.Members[0]
			op := *h[0].Readings[1].Operation
			controller := true
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: op.JobName, Namespace: m.Expected.GetNamespace(), UID: types.UID(op.JobUID), CreationTimestamp: metav1.NewTime(op.StartedAt), OwnerReferences: []metav1.OwnerReference{{Name: m.Expected.GetName(), UID: m.Expected.GetUID(), APIVersion: "operator.ptah.run/v1alpha1", Kind: "PtahSchema", Controller: &controller}}}, Status: batchv1.JobStatus{Succeeded: 1, CompletionTime: &metav1.Time{Time: op.StartedAt.Add(time.Second)}, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}}}
			switch mode {
			case "wrong uid":
				job.UID = "replacement"
			case "wrong owner":
				job.OwnerReferences[0].UID = "replacement"
			case "old Job":
				job.CreationTimestamp = metav1.NewTime(op.StartedAt.Add(-time.Second))
			case "unfinished":
				job.Status.CompletionTime = nil
			case "failed":
				job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobFailed, Status: "True"})
			case "wrong namespace":
				job.Namespace = "other"
			case "wrong name":
				job.Name = "other"
			case "active":
				job.Status.Active = 1
			case "extra completion":
				job.Status.Succeeded = 2
			}
			if err := validateBacklogApplyJob(m, op, job); (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
		})
	}
}

func TestBacklogApprovalAllowsOnlyAdmissionStamps(t *testing.T) {
	for _, mode := range []string{"valid", "changed plan", "missing identity", "changed namespace", "changed name"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := backlogFixture()
			request := p.Members[0].Approval.DeepCopy()
			_ = unstructured.SetNestedMap(request.Object, map[string]any{"name": "resource", "uid": "uid"}, "spec", "schemaRef")
			admitted := request.DeepCopy()
			_ = unstructured.SetNestedField(admitted.Object, "approver", "spec", "approver", "username")
			_ = unstructured.SetNestedField(admitted.Object, "2026-10-02T00:03:00Z", "spec", "approvedAt")
			_ = unstructured.SetNestedField(admitted.Object, "request-uid", "spec", "mutationRequestUID")
			switch mode {
			case "changed plan":
				_ = unstructured.SetNestedField(admitted.Object, "other", "spec", "planRef", "uid")
			case "missing identity":
				unstructured.RemoveNestedField(admitted.Object, "spec", "mutationRequestUID")
			case "changed namespace":
				admitted.SetNamespace("other")
			case "changed name":
				admitted.SetName("other")
			}
			if backlogAdmissionMatches(request, admitted, "schema") != (mode == "valid") {
				t.Fatal("wrong approval admission verdict")
			}
		})
	}
}

func TestBacklogRequiresConsumedUnchangedApproval(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "replaced", "changed spec", "not consumed"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := backlogFixture()
			m := p.Members[0]
			m.ConsumedApproval = m.Approval.DeepCopy()
			_ = unstructured.SetNestedSlice(m.ConsumedApproval.Object, []any{map[string]any{"type": "Consumed", "status": "True"}}, "status", "conditions")
			switch mode {
			case "missing":
				m.ConsumedApproval = nil
			case "replaced":
				m.ConsumedApproval.SetUID("other")
			case "changed spec":
				_ = unstructured.SetNestedField(m.ConsumedApproval.Object, "other", "spec", "planFingerprint")
			case "not consumed":
				unstructured.RemoveNestedField(m.ConsumedApproval.Object, "status")
			}
			if err := consumedBacklogApproval(m); (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
		})
	}
}

func TestBacklogFleetCannotShrinkOrReplace(t *testing.T) {
	for _, mode := range []string{"valid", "empty", "missing member", "extra member", "duplicate", "replacement", "spec changed", "generation changed"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := backlogFixture()
			var after []*unstructured.Unstructured
			for _, o := range p.After {
				after = append(after, o.DeepCopy())
			}
			switch mode {
			case "empty":
				after = nil
			case "missing member":
				after = after[1:]
			case "extra member":
				after = append(after, after[0])
			case "duplicate":
				after[1] = after[0]
			case "replacement":
				after[0].SetUID("replacement")
			case "spec changed":
				_ = unstructured.SetNestedField(after[0].Object, "Always", "spec", "policy", "apply")
			case "generation changed":
				after[0].SetGeneration(3)
			}
			if err := unchangedBacklogFleet(p.After, after); (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
		})
	}
}

func TestBacklogWorkloadRequiresTheDeclaredFleetAndInputs(t *testing.T) {
	w, err := loadWorkload("../../support/capacity/backlog.json")
	if err != nil || !w.ApprovalBacklog || !w.UnrelatedObjects {
		t.Fatal(w, err)
	}
	for _, mode := range []string{"too small", "wrong batch", "mixed soak"} {
		t.Run(mode, func(t *testing.T) {
			v := w
			switch mode {
			case "too small":
				v.Schemas = 5
			case "wrong batch":
				v.ChangeBatch = 1
			case "mixed soak":
				v.Soak = &soakWorkload{}
			}
			if v.validate() == nil {
				t.Fatal("accepted a different backlog population")
			}
		})
	}
	if requireInputs(inputs{namespace: "work", operatorNamespace: "operator", schemaRefs: [2]string{"a", "b"}, migrationRefs: [2]string{"a", "b"}}, w, t.TempDir()) == nil {
		t.Fatal("backlog accepted unpopulated fixtures")
	}
}

func TestBacklogWatchRetainsNativeApprovalBinding(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-approved-migration.json")
	if err != nil {
		t.Fatal(err)
	}
	var object unstructured.Unstructured
	if err = json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	r, err := readCycle("migration", "MODIFIED", &object, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.Operation == nil || r.Operation.Type != "Apply" || r.Operation.ApprovalRef == nil || r.Operation.ApprovalRef.UID != "aa5d04f1-4caa-4da5-91ed-409345a46ee7" || r.Operation.PlanRef == nil || r.Operation.PlanRef.UID != "571b6197-0c2c-4b98-95e5-7078fef9d011" {
		t.Fatalf("native binding was lost: %+v", r.Operation)
	}
}

func TestBacklogListsTheEntireDeclaredFleet(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "extra", "suspended", "wrong namespace", "duplicate UID"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := backlogFixture()
			var objects []runtime.Object
			for _, o := range p.After {
				o.SetLabels(map[string]string{capacityLabel: "backlog"})
				objects = append(objects, o)
			}
			switch mode {
			case "missing":
				objects = objects[1:]
			case "extra":
				extra := p.After[0].DeepCopy()
				extra.SetName("extra")
				extra.SetUID("extra")
				objects = append(objects, extra)
			case "suspended":
				_ = unstructured.SetNestedField(p.After[0].Object, true, "spec", "suspend")
			case "wrong namespace":
				p.After[0].SetNamespace("outside")
			case "duplicate UID":
				p.After[1].SetUID(p.After[0].GetUID())
			}
			s := &scenarios{in: inputs{namespace: "work-0", namespaces: []string{"work-0", "work-1"}}, load: workload{Name: "backlog"}, dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList"}, objects...)}
			fleet, err := s.backlogFleet(t.Context())
			if (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
			if err == nil && len(fleet) != 20 {
				t.Fatal("wrong fleet count")
			}
		})
	}
}

func TestBacklogWatchRetainsNativeSchemaPlanBinding(t *testing.T) {
	o := nativeSuspendedFreshness(t)
	r, err := readCycle("schema", "MODIFIED", o, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.SchemaPlan == nil || r.SchemaPlan.UID != "20a0fd7e-5d3e-47d5-adb1-416764db4edb" || r.SchemaPlan.Approval == nil || r.SchemaPlan.Approval.UID != "d5d400c5-498d-43a4-9224-dacf2590278b" {
		t.Fatalf("native schema status.plan binding lost: %+v", r.SchemaPlan)
	}
}

func TestBacklogRejectsMissingFamilySpecificBindings(t *testing.T) {
	for _, mode := range []string{"schema plan", "schema approval", "migration plan", "migration approval"} {
		t.Run(mode, func(t *testing.T) {
			p, h := backlogFixture()
			switch mode {
			case "schema plan":
				h[0].Readings[1].SchemaPlan = nil
			case "schema approval":
				h[0].Readings[1].SchemaPlan.Approval = nil
			case "migration plan":
				h[2].Readings[1].Operation.PlanRef = nil
			case "migration approval":
				h[2].Readings[1].Operation.ApprovalRef = nil
			}
			if _, err := validateBacklogHistory(p, h); err == nil {
				t.Fatal("missing family-specific binding accepted")
			}
		})
	}
}

func TestBacklogBudgetStartsAtTheFirstServerTimestamp(t *testing.T) {
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"admission first", "creation first", "old admission", "future admission", "malformed admission", "missing creation"} {
		t.Run(mode, func(t *testing.T) {
			o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"approvedAt": base.Add(time.Second).Format(time.RFC3339)}}}
			o.SetCreationTimestamp(metav1.NewTime(base.Add(2 * time.Second)))
			switch mode {
			case "creation first":
				o.SetCreationTimestamp(metav1.NewTime(base.Add(time.Second)))
				_ = unstructured.SetNestedField(o.Object, base.Add(2*time.Second).Format(time.RFC3339), "spec", "approvedAt")
			case "old admission":
				_ = unstructured.SetNestedField(o.Object, base.Add(-time.Second).Format(time.RFC3339), "spec", "approvedAt")
			case "future admission":
				_ = unstructured.SetNestedField(o.Object, base.Add(4*time.Second).Format(time.RFC3339), "spec", "approvedAt")
			case "malformed admission":
				_ = unstructured.SetNestedField(o.Object, "not-a-time", "spec", "approvedAt")
			case "missing creation":
				o.SetCreationTimestamp(metav1.Time{})
			}
			at, err := backlogAdmissionTime(o, base, base.Add(3*time.Second))
			valid := mode == "admission first" || mode == "creation first"
			if (err == nil) != valid || valid && !at.Equal(base.Add(time.Second)) {
				t.Fatalf("at=%s err=%v", at, err)
			}
		})
	}
}

func TestBacklogFailureRetainsItsOutcomeWithoutInventingExecution(t *testing.T) {
	s := &scenarios{
		load:        workload{ApprovalBacklog: true, Name: "backlog", Settle: duration{time.Second}, Interval: duration{time.Second}},
		in:          inputs{namespace: "work-0", namespaces: []string{"work-0", "work-1"}, catalog: testInputCatalog()},
		evidenceDir: t.TempDir(), recorders: []*cycleRecorder{{}},
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList"}),
		checkpoint: func(context.Context, int, string) (databaseCheckpoint, error) {
			t.Fatal("empty fleet reached database mutation preparation")
			return databaseCheckpoint{}, nil
		},
	}
	if s.approvalBacklog(t.Context()) == nil {
		t.Fatal("empty fleet passed")
	}
	if s.backlogProof == nil || s.backlogProof.Error == "" || !s.backlogProof.FirstApprovalAt.IsZero() || len(s.windows) != 1 || s.windows[0].Outcome["error"] == "" {
		t.Fatal("failed preparation invented execution or lost the error")
	}
	raw, err := os.ReadFile(filepath.Join(s.evidenceDir, "approval-backlog/outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained approvalBacklogProof
	if err = json.Unmarshal(raw, &retained); err != nil || retained.Error == "" || retained.FinishedAt.IsZero() {
		t.Fatal(retained, err)
	}
}
