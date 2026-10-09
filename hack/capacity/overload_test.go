package main

import (
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTheOverloadRefusesAnyManagerRestart(t *testing.T) {
	t.Parallel()
	before := []managerProcess{
		{Pod: "manager-a", PodUID: "a", ContainerID: "containerd://a1", RestartCount: 0},
		{Pod: "manager-b", PodUID: "b", ContainerID: "containerd://b1", RestartCount: 0},
	}
	if err := sameManagers(before, append([]managerProcess(nil), before...)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]managerProcess) []managerProcess{
		"a container restart": func(p []managerProcess) []managerProcess {
			p[1].ContainerID, p[1].RestartCount = "containerd://b2", 1
			return p
		},
		"a replaced Pod": func(p []managerProcess) []managerProcess { p[0].PodUID = "a2"; return p },
		"a lost manager": func(p []managerProcess) []managerProcess { return p[:1] },
		"an OOM kill on record": func(p []managerProcess) []managerProcess {
			p[0].LastReason = "OOMKilled"
			return p
		},
	} {
		after := change(append([]managerProcess(nil), before...))
		reference := before
		if name == "an OOM kill on record" {
			reference = append([]managerProcess(nil), after...)
		}
		if err := sameManagers(reference, after); err == nil {
			t.Fatalf("%s passed", name)
		}
	}
	if err := sameManagers(nil, nil); err == nil {
		t.Fatal("no manager reading passed")
	}
}

func resourceWith(family string, status map[string]any, annotations map[string]string) *unstructured.Unstructured {
	item := &unstructured.Unstructured{Object: map[string]any{"status": status}}
	item.SetNamespace("work")
	item.SetName("capacity-" + family + "-000")
	if annotations != nil {
		item.SetAnnotations(annotations)
	}
	return item
}

func TestAnUnresolvedApplyIsReadFromEachFamilysOwnRecord(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		family     string
		item       *unstructured.Unstructured
		unresolved bool
	}{
		{"a migration recording an unresolved run", "migration", resourceWith("migration", map[string]any{"unresolvedRun": map[string]any{"operationID": "x"}}, nil), true},
		{"a migration carrying only the annotation copy", "migration", resourceWith("migration", map[string]any{}, map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: "{}"}), true},
		{"a settled migration", "migration", resourceWith("migration", map[string]any{"phase": "Ready"}, nil), false},
		{"a schema whose Apply outcome is unknown", "schema", resourceWith("schema", map[string]any{"pendingObservation": map[string]any{"outcome": string(operatorv1alpha1.PendingObservationOutcomeUnknown)}}, nil), true},
		{"a schema proving a successful Apply", "schema", resourceWith("schema", map[string]any{"pendingObservation": map[string]any{"outcome": string(operatorv1alpha1.PendingObservationApplySucceeded)}}, nil), false},
		{"a schema with the migration field name", "schema", resourceWith("schema", map[string]any{"unresolvedRun": map[string]any{}}, nil), false},
	} {
		got := unresolvedRecord(row.family, row.item)
		if (got != "") != row.unresolved {
			t.Fatalf("%s: unresolvedRecord = %q", row.name, got)
		}
	}
}

func TestASecondApplyForOneResourceIsAReplay(t *testing.T) {
	t.Parallel()
	jobs := []jobRecord{
		{Family: "schema", Namespace: "a", Resource: "capacity-schema-000", Operation: "apply"},
		{Family: "schema", Namespace: "a", Resource: "capacity-schema-000", Operation: "plan"},
		{Family: "schema", Namespace: "a", Resource: "capacity-schema-000", Operation: "observe"},
		{Family: "migration", Namespace: "b", Resource: "capacity-migration-001", Operation: "migration-apply"},
		{Family: "migration", Namespace: "b", Resource: "capacity-migration-001", Operation: "migration-history"},
		{Family: "schema", Namespace: "b", Resource: "capacity-schema-001", Operation: "apply"},
	}
	counts := applyJobsPerResource(jobs)
	if len(counts) != 3 || counts["schema/a/capacity-schema-000"] != 1 || counts["migration/b/capacity-migration-001"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	if replayed := replayedApplies(counts); len(replayed) != 0 {
		t.Fatalf("one Apply each read as a replay: %v", replayed)
	}
	jobs = append(jobs, jobRecord{Family: "migration", Namespace: "b", Resource: "capacity-migration-001", Operation: "migration-apply", Failed: true})
	replayed := replayedApplies(applyJobsPerResource(jobs))
	if len(replayed) != 1 || !strings.Contains(replayed[0], "capacity-migration-001 (2)") {
		t.Fatalf("a second Apply, failed or not, was not a replay: %v", replayed)
	}
	// The same name in another namespace is another resource.
	other := applyJobsPerResource([]jobRecord{
		{Family: "schema", Namespace: "a", Resource: "capacity-schema-000", Operation: "apply"},
		{Family: "schema", Namespace: "b", Resource: "capacity-schema-000", Operation: "apply"},
	})
	if replayed := replayedApplies(other); len(replayed) != 0 {
		t.Fatalf("resources in two namespaces were merged: %v", replayed)
	}
}

func TestTheOverloadWorkloadIsTwiceTheAdmittedFleetAndNothingElse(t *testing.T) {
	t.Parallel()
	base := workload{Name: "overload", Schemas: 20, Migrations: 20, Interval: duration{2 * time.Minute}, Settle: duration{20 * time.Minute},
		SteadyState: duration{10 * time.Minute}, SampleEvery: duration{5 * time.Second},
		Overload: &overloadWorkload{Admitted: 10, Recovery: duration{5 * time.Minute}}}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*workload){
		"an uneven fleet":   func(w *workload) { w.Migrations = 18 },
		"no admitted count": func(w *workload) { w.Overload = &overloadWorkload{Recovery: duration{time.Minute}} },
		"no recovery bound": func(w *workload) { w.Overload = &overloadWorkload{Admitted: 10} },
		"a change batch":    func(w *workload) { w.ChangeBatch = 5 },
		"a registry outage": func(w *workload) { w.Outage = duration{3 * time.Minute} },
		"a soak":            func(w *workload) { w.Soak = &soakWorkload{} },
		"a database delay": func(w *workload) {
			w.DatabaseDelay = &databaseDelayWorkload{Delay: duration{30 * time.Second}, Hold: duration{5 * time.Minute}}
		},
		"an approval backlog": func(w *workload) { w.ApprovalBacklog = true },
	} {
		w := base
		overload := *base.Overload
		w.Overload = &overload
		mutate(&w)
		if err := w.validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestTheReturnedFleetIsCountedAtTheAdmittedSize(t *testing.T) {
	t.Parallel()
	s := &scenarios{load: workload{Schemas: 20, Migrations: 20, Overload: &overloadWorkload{Admitted: 10}}}
	if got := s.expectedResources(); got != 40 {
		t.Fatalf("the doubled fleet counts %d", got)
	}
	s.admittedOnly = true
	if got := s.expectedResources(); got != 20 {
		t.Fatalf("the returned fleet counts %d", got)
	}
	plain := &scenarios{load: workload{Schemas: 10, Migrations: 10}, admittedOnly: true}
	if got := plain.expectedResources(); got != 20 {
		t.Fatalf("a workload with no overload counts %d", got)
	}
}
