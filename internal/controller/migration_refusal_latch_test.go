package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func latchedMigration() *operatorv1alpha1.PtahMigration {
	finished := metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	migration := &operatorv1alpha1.PtahMigration{}
	migration.Status.LastRun = &operatorv1alpha1.MigrationRunStatus{
		Outcome:    operatorv1alpha1.MigrationRunOutcomeUnknown,
		FinishedAt: &finished,
	}
	migration.Status.Conditions = []metav1.Condition{{
		Type: operatorv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionTrue,
		Reason: "ApplyOutcomeUnknown", Message: "the run left no readable account",
		LastTransitionTime: finished,
	}}
	return migration
}

// A run whose effect nobody established has to become a durable record --
// status.unresolvedRun -- because the resource will outlive the conditions
// that describe it and the Job that produced it. This is the latch that says
// there is one to adopt.
//
// Every part of it could be removed with the package green, and none fails
// loudly: a latch that stops recognising a run leaves nothing to adopt, so the
// unresolved-work signals report a database nobody has to look at.
func TestARefusedRunLatchesOnlyWhileItIsUnaccountedFor(t *testing.T) {
	t.Parallel()

	if !migrationRunLatchedByRefusal(latchedMigration()) {
		t.Fatal("an unaccounted-for run did not latch, so nothing below proves anything")
	}

	for _, row := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahMigration)
	}{
		{
			name: "there was no run",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.LastRun = nil
			},
		},
		{
			// The run accounted for itself.
			name: "the run applied what it selected",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.LastRun.Outcome = operatorv1alpha1.MigrationRunOutcomeApplied
			},
		},
		{
			name: "the run selected nothing",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.LastRun.Outcome = operatorv1alpha1.MigrationRunOutcomeUpToDate
			},
		},
		{
			// It stopped and said where, which accounts for it.
			name: "the run failed and named the migration it stopped on",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.LastRun.Outcome = operatorv1alpha1.MigrationRunOutcomeFailed
			},
		},
		{
			// Nothing is refusing, so there is nothing latched.
			name: "the resource is not blocked",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.Conditions[0].Status = metav1.ConditionFalse
			},
		},
		{
			name: "no condition refuses at all",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.Conditions = nil
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration := latchedMigration()
			row.change(migration)
			if migrationRunLatchedByRefusal(migration) {
				t.Fatal("a run that is accounted for, or that nothing refuses, was latched as unresolved")
			}
		})
	}

	// A partially applied run is the other outcome nobody can account for: some
	// statements are in the database and the rest are not.
	t.Run("a partial run latches too", func(t *testing.T) {
		t.Parallel()

		migration := latchedMigration()
		migration.Status.LastRun.Outcome = operatorv1alpha1.MigrationRunOutcomePartial
		if !migrationRunLatchedByRefusal(migration) {
			t.Fatal("a partially applied run was not latched as unresolved")
		}
	})

	// The latch releases on evidence, not on time: a reading taken after the
	// run, finding nothing of this artifact left to apply, is what settles it.
	t.Run("a later reading that finds nothing pending settles it", func(t *testing.T) {
		t.Parallel()

		migration := latchedMigration()
		observed := metav1.NewTime(migration.Status.LastRun.FinishedAt.Add(time.Minute))
		migration.Status.History = &operatorv1alpha1.MigrationHistoryStatus{
			ObservedAt: observed,
		}
		if migrationRunLatchedByRefusal(migration) {
			t.Fatal("a run a later reading settled was still latched as unresolved")
		}
	})
}
