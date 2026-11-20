package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The repeatable migration readings are the migrations the reconciler stores
// when a history read finds a repeatable migration it stops on or one it lists
// without stopping, and when it reads an Apply that ran one. The e2e predicates that recognize those states
// in a cluster are tested against them (test/e2e/migrations_repeatable_test.go),
// so they cannot pass against a state the controller never writes.
var repeatableReadings = map[string]func(*testing.T) *operatorv1alpha1.PtahMigration{
	// A failed attempt inside a transaction, its file still in the artifact,
	// as a PostgreSQL run that failed at the view leaves it.
	"migration-repeatable-unfinished.json": func(t *testing.T) *operatorv1alpha1.PtahMigration {
		return readStoppingRepeatable(t, dataplane.MigrationRepeatable{
			Identity: "item_names", Path: "R__item_names.sql", Checksum: repeatableContentB,
			AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateFailed,
		})
	},
	// An applied record whose file the artifact no longer carries, which the
	// history lists and nothing stops on.
	"migration-repeatable-orphaned.json": func(t *testing.T) *operatorv1alpha1.PtahMigration {
		stored, _ := readRepeatableHistory(t, operatorv1alpha1.ApplyPolicyAlways, nil, syncedHistoryWith(orphanedRecord("item_names")))
		if stored.Status.Phase != operatorv1alpha1.MigrationPhaseInSync {
			t.Fatalf("the fixture did not converge: phase %s", stored.Status.Phase)
		}
		return stored
	},
	// An approved run of version 3 and then item_names, both applied.
	"migration-repeatable-applied.json": func(t *testing.T) *operatorv1alpha1.PtahMigration {
		record := dataplane.MigrationRepeatable{Identity: "item_names", Path: "R__item_names.sql", Checksum: repeatableContentB,
			AppliedChecksum: repeatableContentB, State: dataplane.RepeatableStateApplied}
		after := syncedHistoryWith(record)
		after.CurrentVersion = 3
		after.Migrations = append(after.Migrations,
			dataplane.MigrationRecord{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied})
		stored := harvestRepeatableApply(t,
			[]operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "checksum-3"}},
			[]operatorv1alpha1.PlannedRepeatableMigration{{Identity: "item_names", Checksum: repeatableContentB, AppliedChecksum: repeatableContentA}},
			dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeApplied, Planned: []int64{3}, Applied: []int64{3},
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{{Identity: "item_names", Path: "R__item_names.sql",
					Checksum: repeatableContentB, AppliedChecksum: repeatableContentA, Reason: dataplane.RepeatableReasonChanged}},
				AppliedRepeatables: []string{"item_names"}, Status: &after},
		).migration
		if stored.Status.LastRun == nil || stored.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied {
			t.Fatalf("the fixture recorded no applied run: %+v", stored.Status.LastRun)
		}
		return stored
	},
	// A run of item_names alone that failed inside a transaction, as
	// PostgreSQL runs it.
	"migration-repeatable-failed.json": func(t *testing.T) *operatorv1alpha1.PtahMigration {
		return harvestFailedRepeatable(t, dataplane.MigrationOutcomeFailed, false)
	},
	// The same run outside a transaction, as MySQL runs every repeatable
	// migration.
	"migration-repeatable-unknown.json": func(t *testing.T) *operatorv1alpha1.PtahMigration {
		return harvestFailedRepeatable(t, dataplane.MigrationOutcomeUnknown, true)
	},
}

// harvestFailedRepeatable is the migration one pass over an Apply of
// item_names alone leaves, when the runner reported the outcome given and a
// record of a failed attempt.
func harvestFailedRepeatable(t *testing.T, outcome string, noTransaction bool) *operatorv1alpha1.PtahMigration {
	t.Helper()
	after := syncedHistoryWith(dataplane.MigrationRepeatable{Identity: "item_names", Path: "R__item_names.sql",
		Checksum: repeatableContentB, AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateFailed,
		NoTransaction: noTransaction})
	stored := harvestRepeatableApply(t, nil,
		[]operatorv1alpha1.PlannedRepeatableMigration{{Identity: "item_names", Checksum: repeatableContentB, AppliedChecksum: repeatableContentA}},
		dataplane.MigrationRunReport{Outcome: outcome,
			PlannedRepeatables: []dataplane.MigrationRepeatableWork{{Identity: "item_names", Path: "R__item_names.sql",
				Checksum: repeatableContentB, AppliedChecksum: repeatableContentA, Reason: dataplane.RepeatableReasonChanged}},
			Status: &after},
	).migration
	if stored.Status.LastRun == nil || stored.Status.LastRun.Outcome == operatorv1alpha1.MigrationRunOutcomeApplied {
		t.Fatalf("the fixture recorded no failed run: %+v", stored.Status.LastRun)
	}
	return stored
}

// readStoppingRepeatable is the migration a history read that lists the
// record given leaves, which has to be one it stops on.
func readStoppingRepeatable(t *testing.T, record dataplane.MigrationRepeatable) *operatorv1alpha1.PtahMigration {
	t.Helper()
	stored, _ := readRepeatableHistory(t, operatorv1alpha1.ApplyPolicyAlways, nil, syncedHistoryWith(record))
	if stored.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("the fixture did not stop: phase %s", stored.Status.Phase)
	}
	return stored
}

// TestTheRepeatableReadingsAreWhatTheReconcilerStores keeps each reading to
// the document the reconciler stores. Condition transition times, the
// history's read time and the journal come from the wall clock and are left
// out.
func TestTheRepeatableReadingsAreWhatTheReconcilerStores(t *testing.T) {
	t.Parallel()

	for name, read := range repeatableReadings {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stored := read(t)
			status := stored.Status.DeepCopy()
			status.Conditions = slices.Clone(status.Conditions)
			for index := range status.Conditions {
				status.Conditions[index].LastTransitionTime = metav1.Time{}
			}
			if status.History != nil {
				status.History.ObservedAt = metav1.Time{}
			}
			status.NextReconciliationTime = nil
			status.Journal = nil
			keepReading(t, name, map[string]any{
				"apiVersion": operatorv1alpha1.GroupVersion.String(),
				"kind":       "PtahMigration",
				"metadata":   map[string]any{"namespace": stored.Namespace, "name": stored.Name, "generation": stored.Generation},
				"status":     status,
			})
		})
	}
}

// TestTheRepeatablePlanReadingIsWhatTheReconcilerPublishes keeps
// testdata/migration-repeatable-plan.json to the plan a history read publishes
// for a pending version and a changed repeatable migration under OnApproval.
func TestTheRepeatablePlanReadingIsWhatTheReconcilerPublishes(t *testing.T) {
	t.Parallel()

	report := syncedHistoryWith(dataplane.MigrationRepeatable{Identity: "item_names", Path: "R__item_names.sql",
		Checksum: repeatableContentB, AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateChanged})
	report.TotalMigrations, report.PendingMigrations = 2, []int64{3}
	report.Migrations = append(report.Migrations,
		dataplane.MigrationRecord{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending})
	_, api := readRepeatableHistory(t, operatorv1alpha1.ApplyPolicyOnApproval, nil, report)
	plans := publishedPlans(t, api)
	if len(plans) != 1 {
		t.Fatalf("the history read published %d plans, want 1", len(plans))
	}
	plan := plans[0]
	keepReading(t, "migration-repeatable-plan.json", map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       "PtahMigrationPlan",
		"metadata":   map[string]any{"namespace": plan.Namespace, "name": plan.Name},
		"spec":       plan.Spec,
	})
}

// TestTheRepeatableEventReadingIsWhatTheReconcilerEmits keeps
// testdata/migration-repeatable-orphaned-event.json to the Event a history
// read emits for an applied record of item_names that no file carries. The
// e2e predicate that recognizes that Event in a cluster is tested against it.
func TestTheRepeatableEventReadingIsWhatTheReconcilerEmits(t *testing.T) {
	t.Parallel()

	reconciler, _, migration := repeatableHistoryReader(t, operatorv1alpha1.ApplyPolicyAlways, nil, nil,
		syncedHistoryWith(orphanedRecord("item_names")))
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	events := recordedEvents(recorder)
	if len(events) != 1 {
		t.Fatalf("the history read emitted %q, want one Event", events)
	}
	// The fake recorder writes "<type> <reason> <message>".
	eventType, rest, _ := strings.Cut(events[0], " ")
	reason, message, _ := strings.Cut(rest, " ")
	keepReading(t, "migration-repeatable-orphaned-event.json", map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"type":       eventType,
		"reason":     reason,
		"message":    message,
	})
}

// keepReading compares a reading with testdata/<name>, and writes it there
// first under -update.
func keepReading(t *testing.T, name string, reading any) {
	t.Helper()
	data, err := json.MarshalIndent(reading, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", name)
	if *updateRefusalReading {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: run go test ./internal/controller -run TestTheRepeatable -update", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("the reconciler writes another document than %s; run go test ./internal/controller "+
			"-run TestTheRepeatable -update and review the diff:\n%s", path, data)
	}
}
