package e2e

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The readings internal/controller holds to what the reconciler writes
// (TestTheRepeatableReadingsAreWhatTheReconcilerStores and
// TestTheRepeatablePlanReadingIsWhatTheReconcilerPublishes), so each predicate
// below accepts a state the controller produces. Their repeatable migration is
// item_names, and the applied run applied version 3 before it.
const repeatableReadingIdentity = "item_names"

// rpRead decodes a reading afresh, so a mutation changes one copy only.
func rpRead[T any](t *testing.T, name string) *T {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "internal", "controller", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var document T
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return &document
}

func rpMigration(t *testing.T, name string) *ptahv1alpha1.PtahMigration {
	t.Helper()
	return rpRead[ptahv1alpha1.PtahMigration](t, name)
}

// The applied run passes, and each copy of it with one clause broken is
// refused: no run, another outcome, the previous Job's run, another list of
// versions, and another list of repeatable migrations.
func TestRepeatableRunAppliedRefusesEachClause(t *testing.T) {
	t.Parallel()
	versions, identities := []int64{3}, []string{repeatableReadingIdentity}
	applied := rpMigration(t, "migration-repeatable-applied.json").Status.LastRun
	if err := repeatableRunApplied(applied, "an-earlier-job", versions, identities); err != nil {
		t.Fatalf("the operator's record of the applied run was refused: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string){
		"no run": func(*ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) { return nil, "" },
		"a failed run": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed
			return run, ""
		},
		"a partial run": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.Outcome = ptahv1alpha1.MigrationRunOutcomePartial
			return run, ""
		},
		"the previous Job's run": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			return run, string(run.JobUID)
		},
		"a run with no Job": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.JobUID = ""
			return run, ""
		},
		"no version applied": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.AppliedVersions = nil
			return run, ""
		},
		"no repeatable migration applied": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.AppliedRepeatables = nil
			return run, ""
		},
		"another repeatable migration": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.AppliedRepeatables = []string{"totals"}
			return run, ""
		},
		"one more repeatable migration": func(run *ptahv1alpha1.MigrationRunStatus) (*ptahv1alpha1.MigrationRunStatus, string) {
			run.AppliedRepeatables = append(run.AppliedRepeatables, "totals")
			return run, ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run, previous := mutate(rpMigration(t, "migration-repeatable-applied.json").Status.LastRun)
			if err := repeatableRunApplied(run, previous, versions, identities); err == nil {
				t.Fatalf("a run with %s was accepted", name)
			}
		})
	}
	if repeatableRunApplied(applied, "an-earlier-job", versions, nil) == nil {
		t.Fatal("a row that names no repeatable migration accepted a run")
	}
}

// rpSettled is the drift-checked migration a cluster run captured in sync,
// carrying the applied run the reconciler records. The cluster reading is
// the settled state and the controller reading is the run with a repeatable
// migration in it, which no cluster run has captured yet.
func rpSettled(t *testing.T) *ptahv1alpha1.PtahMigration {
	t.Helper()
	settled := dcRead(t, dcCheckedFile)
	settled.Status.LastRun = rpMigration(t, "migration-repeatable-applied.json").Status.LastRun
	return settled
}

// A settled migration after the applied run passes, the harvest the
// reconciler stores before it reads the history again is refused, and so is
// each copy of the settled state with one clause broken.
func TestSettledOnRepeatableRunRefusesEachClause(t *testing.T) {
	t.Parallel()
	versions, identities := []int64{3}, []string{repeatableReadingIdentity}
	if err := settledOnRepeatableRun(rpSettled(t), "an-earlier-job", versions, identities); err != nil {
		t.Fatalf("the settled migration was refused: %v", err)
	}
	if settledOnRepeatableRun(rpMigration(t, "migration-repeatable-applied.json"), "an-earlier-job", versions, identities) == nil {
		t.Fatal("the harvest before the history read was accepted as settled")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"a stopped migration": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked
		},
		"a status for the generation before": func(resource *ptahv1alpha1.PtahMigration) { resource.Generation++ },
		"an operation in flight": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{ID: "sha256:77"}
		},
		"a run unaccounted for": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{OperationID: "sha256:77"}
		},
		"no history":           func(resource *ptahv1alpha1.PtahMigration) { resource.Status.History = nil },
		"a pending repeatable": func(resource *ptahv1alpha1.PtahMigration) { resource.Status.History.PendingRepeatableCount = 1 },
		"an unfinished repeatable": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.History.UnfinishedRepeatables = []string{"totals"}
		},
		"an orphaned repeatable": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.History.OrphanedRepeatables = []string{"totals"}
		},
		"a run that applied nothing": func(resource *ptahv1alpha1.PtahMigration) { resource.Status.LastRun.AppliedRepeatables = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resource := rpSettled(t)
			mutate(resource)
			if err := settledOnRepeatableRun(resource, "an-earlier-job", versions, identities); err == nil {
				t.Fatalf("a migration with %s was accepted", name)
			}
		})
	}
}

// The plan the reconciler publishes passes, and each copy of it with one
// clause broken is refused.
func TestRepeatablePlannedRefusesEachClause(t *testing.T) {
	t.Parallel()
	read := func() *ptahv1alpha1.PtahMigrationPlan {
		return rpRead[ptahv1alpha1.PtahMigrationPlan](t, "migration-repeatable-plan.json")
	}
	plan := read()
	if len(plan.Spec.Repeatables) != 1 {
		t.Fatalf("the reading holds %d repeatable migrations", len(plan.Spec.Repeatables))
	}
	applied := plan.Spec.Repeatables[0].AppliedChecksum
	if err := repeatablePlanned(plan, []int64{3}, repeatableReadingIdentity, applied); err != nil {
		t.Fatalf("the operator's plan was refused: %v", err)
	}
	// A plan of the repeatable migration alone, and of one the database has
	// no record of, are the plans the row's later stages read.
	alone := read()
	alone.Spec.Migrations = nil
	if err := repeatablePlanned(alone, nil, repeatableReadingIdentity, applied); err != nil {
		t.Fatalf("a plan of the repeatable migration alone was refused: %v", err)
	}
	fresh := read()
	fresh.Spec.Repeatables[0].AppliedChecksum = ""
	if err := repeatablePlanned(fresh, []int64{3}, repeatableReadingIdentity, ""); err != nil {
		t.Fatalf("a plan of a new repeatable migration was refused: %v", err)
	}
	for name, test := range map[string]struct {
		mutate   func(*ptahv1alpha1.PtahMigrationPlan)
		versions []int64
		applied  string
	}{
		"no version":                    {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) { plan.Spec.Migrations = nil }, versions: []int64{3}, applied: applied},
		"a version the row did not ask": {mutate: func(*ptahv1alpha1.PtahMigrationPlan) {}, applied: applied},
		"no repeatable migration": {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) { plan.Spec.Repeatables = nil },
			versions: []int64{3}, applied: applied},
		"two repeatable migrations": {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) {
			plan.Spec.Repeatables = append(plan.Spec.Repeatables, ptahv1alpha1.PlannedRepeatableMigration{
				Identity: "totals", Checksum: plan.Spec.Repeatables[0].Checksum})
		}, versions: []int64{3}, applied: applied},
		"another repeatable migration": {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) { plan.Spec.Repeatables[0].Identity = "totals" },
			versions: []int64{3}, applied: applied},
		"no content digest": {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) { plan.Spec.Repeatables[0].Checksum = "checksum-b" },
			versions: []int64{3}, applied: applied},
		"a record the row says is absent": {mutate: func(*ptahv1alpha1.PtahMigrationPlan) {}, versions: []int64{3}},
		"no record where the row has one": {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) { plan.Spec.Repeatables[0].AppliedChecksum = "" },
			versions: []int64{3}, applied: applied},
		"content the database already applied": {mutate: func(plan *ptahv1alpha1.PtahMigrationPlan) {
			plan.Spec.Repeatables[0].Checksum = plan.Spec.Repeatables[0].AppliedChecksum
		}, versions: []int64{3}, applied: applied},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := read()
			test.mutate(plan)
			if err := repeatablePlanned(plan, test.versions, repeatableReadingIdentity, test.applied); err == nil {
				t.Fatalf("a plan with %s was accepted", name)
			}
		})
	}
}

// The unfinished record the reconciler stops on passes and returns the
// command Ptah repairs that record with, and is refused for the converged
// migration beside a record no file carries, for another repeatable
// migration, and with each clause broken.
func TestRepeatableStopRefusesEachClause(t *testing.T) {
	t.Parallel()
	const file = "migration-repeatable-unfinished.json"
	command, err := repeatableStop(rpMigration(t, file).Status, repeatableReadingIdentity)
	if want := "ptah migrations repair --repeatable item_names --force"; err != nil || command != want {
		t.Fatalf("the stop the reconciler stores read %q, %v; want %q", command, err, want)
	}
	if _, err := repeatableStop(rpMigration(t, "migration-repeatable-orphaned.json").Status, repeatableReadingIdentity); err == nil {
		t.Fatal("the migration converged beside a record no file carries was accepted as stopped")
	}
	if _, err := repeatableStop(rpMigration(t, file).Status, "totals"); err == nil {
		t.Fatal("the stop was accepted for another repeatable migration")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"a migration that is not Blocked": func(status *ptahv1alpha1.PtahMigrationStatus) {
			status.Phase = ptahv1alpha1.MigrationPhaseInSync
		},
		"no Blocked condition": func(status *ptahv1alpha1.PtahMigrationStatus) {
			status.Conditions = rpWithout(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked)
		},
		"a Blocked condition that is False": func(status *ptahv1alpha1.PtahMigrationStatus) {
			rpBlocked(status).Status = metav1.ConditionFalse
		},
		"a dirty revision": func(status *ptahv1alpha1.PtahMigrationStatus) {
			rpBlocked(status).Reason = string(ptahv1alpha1.ReasonHistoryDirty)
		},
		"no history": func(status *ptahv1alpha1.PtahMigrationStatus) { status.History = nil },
		"a second repeatable migration listed": func(status *ptahv1alpha1.PtahMigrationStatus) {
			status.History.UnfinishedRepeatables = append(status.History.UnfinishedRepeatables, "totals")
		},
		"the record listed as orphaned": func(status *ptahv1alpha1.PtahMigrationStatus) {
			status.History.OrphanedRepeatables, status.History.UnfinishedRepeatables = status.History.UnfinishedRepeatables, nil
		},
		"a message about something else": func(status *ptahv1alpha1.PtahMigrationStatus) {
			rpBlocked(status).Message = "note: " + rpBlocked(status).Message
		},
		"a message naming no command": func(status *ptahv1alpha1.PtahMigrationStatus) {
			rpBlocked(status).Message = strings.ReplaceAll(rpBlocked(status).Message, "`", "")
		},
		"a command without --force": func(status *ptahv1alpha1.PtahMigrationStatus) {
			rpBlocked(status).Message = strings.Replace(rpBlocked(status).Message, " --force`", "`", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := rpMigration(t, file).Status
			mutate(&status)
			if _, err := repeatableStop(status, repeatableReadingIdentity); err == nil {
				t.Fatalf("a stop with %s was accepted", name)
			}
		})
	}
}

// rpOrphaned is the migration the reconciler stores after a history read
// finds an applied record no file carries, over the record of the applied run
// before it, whose Job is the previous one.
func rpOrphaned(t *testing.T) (*ptahv1alpha1.PtahMigration, string) {
	t.Helper()
	resource := rpMigration(t, "migration-repeatable-orphaned.json")
	resource.Status.LastRun = rpMigration(t, "migration-repeatable-applied.json").Status.LastRun
	return resource, string(resource.Status.LastRun.JobUID)
}

// The converged migration beside a record no file carries passes, the
// migration stopped on an unfinished record and the one settled on a run with
// nothing listed are refused, and so is each copy of the converged state with
// one clause broken.
func TestOrphanedRepeatableSettledRefusesEachClause(t *testing.T) {
	t.Parallel()
	resource, previous := rpOrphaned(t)
	if err := orphanedRepeatableSettled(resource, previous, repeatableReadingIdentity); err != nil {
		t.Fatalf("the converged migration the reconciler stores was refused: %v", err)
	}
	if orphanedRepeatableSettled(resource, previous, "totals") == nil {
		t.Fatal("the converged migration was accepted for another repeatable migration")
	}
	stopped := rpMigration(t, "migration-repeatable-unfinished.json")
	stopped.Status.LastRun = resource.Status.LastRun
	if orphanedRepeatableSettled(stopped, previous, repeatableReadingIdentity) == nil {
		t.Fatal("the migration stopped on an unfinished record was accepted")
	}
	if orphanedRepeatableSettled(rpSettled(t), previous, repeatableReadingIdentity) == nil {
		t.Fatal("a migration whose history lists no orphaned record was accepted")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration) string{
		"a stopped migration": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked
			return ""
		},
		"a status for the generation before": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Generation++
			return ""
		},
		"a Ready condition that is False": func(resource *ptahv1alpha1.PtahMigration) string {
			meta.FindStatusCondition(resource.Status.Conditions, ptahv1alpha1.ConditionMigrationReady).Status = metav1.ConditionFalse
			return ""
		},
		"no Ready condition": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.Conditions = rpWithout(resource.Status.Conditions, ptahv1alpha1.ConditionMigrationReady)
			return ""
		},
		"a Blocked condition that is True": func(resource *ptahv1alpha1.PtahMigration) string {
			rpBlocked(&resource.Status).Status = metav1.ConditionTrue
			return ""
		},
		"an operation in flight": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{ID: "sha256:77"}
			return ""
		},
		"a run unaccounted for": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{OperationID: "sha256:77"}
			return ""
		},
		"no history": func(resource *ptahv1alpha1.PtahMigration) string { resource.Status.History = nil; return "" },
		"a pending version": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.History.PendingCount = 1
			return ""
		},
		"a pending repeatable": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.History.PendingRepeatableCount = 1
			return ""
		},
		"an unfinished repeatable": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.History.UnfinishedRepeatables = []string{"totals"}
			return ""
		},
		"a second orphaned record": func(resource *ptahv1alpha1.PtahMigration) string {
			resource.Status.History.OrphanedRepeatables = append(resource.Status.History.OrphanedRepeatables, "totals")
			return ""
		},
		"no run":                       func(resource *ptahv1alpha1.PtahMigration) string { resource.Status.LastRun = nil; return "" },
		"a run after the previous one": func(*ptahv1alpha1.PtahMigration) string { return "an-earlier-job" },
		"no previous run named":        func(resource *ptahv1alpha1.PtahMigration) string { resource.Status.LastRun.JobUID = ""; return "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resource, previous := rpOrphaned(t)
			if replaced := mutate(resource); replaced != "" {
				previous = replaced
			} else if name == "no previous run named" {
				previous = ""
			}
			if err := orphanedRepeatableSettled(resource, previous, repeatableReadingIdentity); err == nil {
				t.Fatalf("a migration with %s was accepted", name)
			}
		})
	}
}

// rpEvent is the Event the reconciler emits for an applied record of
// item_names no file carries, as the API server stores it the first time:
// about the object with UID uid, counted once.
func rpEvent(t *testing.T, uid types.UID) corev1.Event {
	t.Helper()
	event := *rpRead[corev1.Event](t, "migration-repeatable-orphaned-event.json")
	event.InvolvedObject = corev1.ObjectReference{Kind: "PtahMigration", Namespace: "team-a", Name: "orders", UID: uid}
	event.Count = 1
	return event
}

// The Event the reconciler emits passes beside unrelated Events, and is
// refused for another object or another repeatable migration, when it was
// emitted twice, and with each clause broken.
func TestOrphanedRepeatableEventRefusesEachClause(t *testing.T) {
	t.Parallel()
	const uid = types.UID("migration-uid")
	unrelated := rpEvent(t, uid)
	unrelated.Reason, unrelated.Message = "PlanPublished", "The plan was published"
	elsewhere := rpEvent(t, "another-uid")
	if err := orphanedRepeatableEvent([]corev1.Event{unrelated, rpEvent(t, uid), elsewhere}, uid, repeatableReadingIdentity); err != nil {
		t.Fatalf("the Event the reconciler emits was refused: %v", err)
	}
	if orphanedRepeatableEvent([]corev1.Event{rpEvent(t, uid)}, uid, "totals") == nil {
		t.Fatal("the Event was accepted for another repeatable migration")
	}
	if orphanedRepeatableEvent([]corev1.Event{rpEvent(t, uid)}, "", repeatableReadingIdentity) == nil {
		t.Fatal("a row that names no object accepted an Event")
	}
	for name, events := range map[string]func() []corev1.Event{
		"no Event":                         func() []corev1.Event { return nil },
		"only Events about another object": func() []corev1.Event { return []corev1.Event{elsewhere, unrelated} },
		"a second Event": func() []corev1.Event {
			second := rpEvent(t, uid)
			second.Name = "orders.second"
			return []corev1.Event{rpEvent(t, uid), second}
		},
		"an Event counted twice": func() []corev1.Event {
			event := rpEvent(t, uid)
			event.Count = 2
			return []corev1.Event{event}
		},
		"an Event in a series": func() []corev1.Event {
			event := rpEvent(t, uid)
			event.Series = &corev1.EventSeries{Count: 2}
			return []corev1.Event{event}
		},
		"a Normal Event": func() []corev1.Event {
			event := rpEvent(t, uid)
			event.Type = corev1.EventTypeNormal
			return []corev1.Event{event}
		},
		"a message naming no command": func() []corev1.Event {
			event := rpEvent(t, uid)
			event.Message = strings.ReplaceAll(event.Message, "`", "")
			return []corev1.Event{event}
		},
		"a command with --force": func() []corev1.Event {
			event := rpEvent(t, uid)
			event.Message = strings.TrimSuffix(event.Message, "`") + " --force`"
			return []corev1.Event{event}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := orphanedRepeatableEvent(events(), uid, repeatableReadingIdentity); err == nil {
				t.Fatalf("the Events with %s were accepted", name)
			}
		})
	}
}

// rpBlocked is the Blocked condition of a reading, which every stop carries.
func rpBlocked(status *ptahv1alpha1.PtahMigrationStatus) *metav1.Condition {
	for index := range status.Conditions {
		if status.Conditions[index].Type == ptahv1alpha1.ConditionMigrationBlocked {
			return &status.Conditions[index]
		}
	}
	panic("the reading carries no Blocked condition")
}

// rpWithout is the conditions without the one of the type given.
func rpWithout(conditions []metav1.Condition, kind string) []metav1.Condition {
	var kept []metav1.Condition
	for _, condition := range conditions {
		if condition.Type != kind {
			kept = append(kept, condition)
		}
	}
	return kept
}

// Each failed attempt the reconciler records passes for the way its engine
// ran it, and is refused for the other way and with each clause broken.
func TestRepeatableAttemptRecordedRefusesEachClause(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		file, other   string
		transactional bool
	}{
		{"migration-repeatable-failed.json", "migration-repeatable-unknown.json", true},
		{"migration-repeatable-unknown.json", "migration-repeatable-failed.json", false},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			if err := repeatableAttemptRecorded(rpMigration(t, test.file).Status, "an-earlier-job", repeatableReadingIdentity, test.transactional); err != nil {
				t.Fatalf("the attempt the reconciler records was refused: %v", err)
			}
			if repeatableAttemptRecorded(rpMigration(t, test.other).Status, "an-earlier-job", repeatableReadingIdentity, test.transactional) == nil {
				t.Fatal("the attempt the other engine records was accepted")
			}
			if repeatableAttemptRecorded(rpMigration(t, test.file).Status, "an-earlier-job", "totals", test.transactional) == nil {
				t.Fatal("the attempt was accepted for another repeatable migration")
			}
			for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus) string{
				"no run": func(status *ptahv1alpha1.PtahMigrationStatus) string { status.LastRun = nil; return "" },
				"the previous Job's run": func(status *ptahv1alpha1.PtahMigrationStatus) string {
					return string(status.LastRun.JobUID)
				},
				"an applied run": func(status *ptahv1alpha1.PtahMigrationStatus) string {
					status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied
					return ""
				},
				"a partial run": func(status *ptahv1alpha1.PtahMigrationStatus) string {
					status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomePartial
					return ""
				},
				"a version applied before it": func(status *ptahv1alpha1.PtahMigrationStatus) string {
					status.LastRun.AppliedVersions = []int64{3}
					return ""
				},
				"a message about a version": func(status *ptahv1alpha1.PtahMigrationStatus) string {
					status.LastRun.Message = "Migration 3 failed inside a transaction, which took no effect"
					return ""
				},
				"the other unresolved state": func(status *ptahv1alpha1.PtahMigrationStatus) string {
					if status.UnresolvedRun == nil {
						status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{OperationID: status.LastRun.OperationID}
					} else {
						status.UnresolvedRun = nil
					}
					return ""
				},
			} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					status := rpMigration(t, test.file).Status
					previous := mutate(&status)
					if previous == "" {
						previous = "an-earlier-job"
					}
					if err := repeatableAttemptRecorded(status, previous, repeatableReadingIdentity, test.transactional); err == nil {
						t.Fatalf("an attempt with %s was accepted", name)
					}
				})
			}
		})
	}
	unknown := rpMigration(t, "migration-repeatable-unknown.json").Status
	unknown.UnresolvedRun.OperationID = "sha256:77"
	if repeatableAttemptRecorded(unknown, "an-earlier-job", repeatableReadingIdentity, false) == nil {
		t.Fatal("an unresolved run of another operation was accepted")
	}
}

// rpLogRow is one line repeatableLogQuery prints.
func rpLogRow(state, reference string) string {
	return state + ":" + strings.ToUpper(hex.EncodeToString([]byte(reference))) + "\n"
}

// The entries of one run pass whatever else the log holds, and the log is
// refused when that run wrote other entries, none, or entries in a shape the
// query does not print.
func TestRepeatableRunLoggedRefusesEachClause(t *testing.T) {
	t.Parallel()
	const run, other = "sha256:aa", "sha256:bb"
	log := rpLogRow("started", other) + rpLogRow("applied", other) + rpLogRow("started", run) + rpLogRow("applied", run)
	if err := repeatableRunLogged(log, run, "started", "applied"); err != nil {
		t.Fatalf("the log of one run among two was refused: %v", err)
	}
	// MySQL's hex() prints upper case and PostgreSQL's encode() lower case.
	if err := repeatableRunLogged(strings.ToLower(log), run, "started", "applied"); err != nil {
		t.Fatalf("the log in lower-case hex was refused: %v", err)
	}
	for name, test := range map[string]struct {
		log    string
		run    string
		states []string
	}{
		"an empty log":              {log: "", run: run, states: []string{"started", "applied"}},
		"only another run":          {log: rpLogRow("started", other) + rpLogRow("applied", other), run: run, states: []string{"started", "applied"}},
		"a failed attempt":          {log: rpLogRow("started", run) + rpLogRow("failed", run), run: run, states: []string{"started", "applied"}},
		"a lost entry":              {log: rpLogRow("started", run), run: run, states: []string{"started", "applied"}},
		"entries out of order":      {log: rpLogRow("applied", run) + rpLogRow("started", run), run: run, states: []string{"started", "applied"}},
		"a row with no reference":   {log: "started:\napplied:\n", run: run, states: []string{"started", "applied"}},
		"a row that is not hex":     {log: "started:" + run + "\n", run: run, states: []string{"started"}},
		"a row with no separator":   {log: "started\n", run: run, states: []string{"started"}},
		"a row asked about no run":  {log: log, states: []string{"started", "applied"}},
		"a row asked about nothing": {log: log, run: run},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := repeatableRunLogged(test.log, test.run, test.states...); err == nil {
				t.Fatalf("a log with %s was accepted", name)
			}
		})
	}
}
