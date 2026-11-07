package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// assertMigrationCondition holds one condition's message to the whole sentence.
func assertMigrationCondition(t *testing.T, migration *operatorv1alpha1.PtahMigration, conditionType, want string) {
	t.Helper()
	condition := meta.FindStatusCondition(migration.Status.Conditions, conditionType)
	if condition == nil || condition.Message != want {
		t.Fatalf("%s = %#v, want the message %q", conditionType, condition, want)
	}
}

// A person reads these in status. Each sentence that counts migrations agrees
// with its count, measured at one and at two, and zero takes the plural.
func TestARunMessageAgreesWithItsCountOfMigrations(t *testing.T) {
	t.Parallel()
	named := &dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		Migrations:      []dataplane.MigrationRecord{{Version: 9, Description: "Index Orders", State: dataplane.MigrationStateDirty}},
		DirtyRevision:   &dataplane.MigrationDirty{Version: 9, Total: 1},
	}
	for _, row := range []struct {
		name   string
		report dataplane.MigrationRunReport
		want   string
	}{
		{
			name:   "one applied",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeApplied, Applied: []int64{1}},
			want:   "1 migration is recorded applied",
		},
		{
			name:   "two applied",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeApplied, Applied: []int64{1, 2}},
			want:   "2 migrations are recorded applied",
		},
		{
			name:   "failed, none before it, in a migration the report does not name",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed},
			want:   "A migration failed and committed nothing; 0 migrations before it are recorded applied",
		},
		{
			name:   "failed, one before it, in a migration the report does not name",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Applied: []int64{1}},
			want:   "A migration failed and committed nothing; 1 migration before it is recorded applied",
		},
		{
			name:   "failed, two before it, in a migration the report does not name",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Applied: []int64{1, 2}},
			want:   "A migration failed and committed nothing; 2 migrations before it are recorded applied",
		},
		{
			name:   "failed, none before it, in a migration it names",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Status: named},
			want:   "Migration 9 (Index Orders) failed and committed nothing; 0 migrations before it are recorded applied",
		},
		{
			name:   "failed, one before it, in a migration it names",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Applied: []int64{1}, Status: named},
			want:   "Migration 9 (Index Orders) failed and committed nothing; 1 migration before it is recorded applied",
		},
		{
			name:   "failed, two before it, in a migration it names",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Applied: []int64{1, 2}, Status: named},
			want:   "Migration 9 (Index Orders) failed and committed nothing; 2 migrations before it are recorded applied",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := migrationRunMessage(row.report); got != row.want {
				t.Fatalf("migrationRunMessage() = %q, want %q", got, row.want)
			}
		})
	}
}

func TestAPolicySentenceAgreesWithItsCountOfMigrations(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name          string
		policy        operatorv1alpha1.ApplyPolicy
		planned       int
		conditionType string
		want          string
	}{
		{"never, one", operatorv1alpha1.ApplyPolicyNever, 1, operatorv1alpha1.ConditionMigrationReady,
			"1 migration is pending and the apply policy is Never"},
		{"never, two", operatorv1alpha1.ApplyPolicyNever, 2, operatorv1alpha1.ConditionMigrationReady,
			"2 migrations are pending and the apply policy is Never"},
		{"always, one", operatorv1alpha1.ApplyPolicyAlways, 1, operatorv1alpha1.ConditionMigrationReady,
			"1 migration is planned"},
		{"always, two", operatorv1alpha1.ApplyPolicyAlways, 2, operatorv1alpha1.ConditionMigrationReady,
			"2 migrations are planned"},
		{"always, one, what happens next", operatorv1alpha1.ApplyPolicyAlways, 1, operatorv1alpha1.ConditionMigrationProgressing,
			"An Apply of 1 planned migration is what happens next"},
		{"always, two, what happens next", operatorv1alpha1.ApplyPolicyAlways, 2, operatorv1alpha1.ConditionMigrationProgressing,
			"An Apply of 2 planned migrations is what happens next"},
		{"on approval, one", operatorv1alpha1.ApplyPolicyOnApproval, 1, operatorv1alpha1.ConditionMigrationApprovalRequired,
			"1 planned migration needs an approval naming this plan"},
		{"on approval, two", operatorv1alpha1.ApplyPolicyOnApproval, 2, operatorv1alpha1.ConditionMigrationApprovalRequired,
			"2 planned migrations need an approval naming this plan"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Policy.Apply = row.policy
			reconciler := &MigrationReconciler{}
			reconciler.applyMigrationPolicyPhase(migration, row.planned)
			assertMigrationCondition(t, migration, row.conditionType, row.want)
		})
	}
}

func TestAHistorySentenceAgreesWithItsCountOfMigrations(t *testing.T) {
	t.Parallel()
	records := func(state string, count int) []dataplane.MigrationRecord {
		applied := []dataplane.MigrationRecord{{Version: 5, Checksum: "applied", State: dataplane.MigrationStateApplied}}
		for index := range count {
			version := int64(6 + index)
			if state == dataplane.MigrationStateOutOfOrder {
				version = int64(1 + index)
			}
			applied = append(applied, dataplane.MigrationRecord{Version: version, Checksum: "checksum", State: state})
		}
		return applied
	}
	unresolved := &operatorv1alpha1.UnresolvedMigrationRunStatus{Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown}
	for _, row := range []struct {
		name          string
		state         string
		count         int
		unresolved    *operatorv1alpha1.UnresolvedMigrationRunStatus
		conditionType string
		want          string
	}{
		{name: "pending, one", state: dataplane.MigrationStatePending, count: 1,
			conditionType: operatorv1alpha1.ConditionMigrationReady, want: "1 migration is pending"},
		{name: "pending, two", state: dataplane.MigrationStatePending, count: 2,
			conditionType: operatorv1alpha1.ConditionMigrationReady, want: "2 migrations are pending"},
		{name: "publishing, one", state: dataplane.MigrationStatePending, count: 1,
			conditionType: operatorv1alpha1.ConditionMigrationProgressing, want: "A plan for 1 pending migration is being published"},
		{name: "publishing, two", state: dataplane.MigrationStatePending, count: 2,
			conditionType: operatorv1alpha1.ConditionMigrationProgressing, want: "A plan for 2 pending migrations is being published"},
		{name: "after an unresolved run, one", state: dataplane.MigrationStatePending, count: 1, unresolved: unresolved,
			conditionType: operatorv1alpha1.ConditionMigrationBlocked,
			want:          "1 migration is pending and the last run's outcome was Unknown, so it may not run again"},
		{name: "after an unresolved run, two", state: dataplane.MigrationStatePending, count: 2, unresolved: unresolved,
			conditionType: operatorv1alpha1.ConditionMigrationBlocked,
			want:          "2 migrations are pending and the last run's outcome was Unknown, so none may run again"},
		{name: "modified, one", state: dataplane.MigrationStateModified, count: 1,
			conditionType: operatorv1alpha1.ConditionMigrationBlocked,
			want: "1 applied migration no longer matches its file; restore the file the database recorded, " +
				"or publish an artifact whose history matches it"},
		{name: "modified, two", state: dataplane.MigrationStateModified, count: 2,
			conditionType: operatorv1alpha1.ConditionMigrationBlocked,
			want: "2 applied migrations no longer match their files; restore the files the database recorded, " +
				"or publish an artifact whose history matches it"},
		{name: "out of order, one", state: dataplane.MigrationStateOutOfOrder, count: 1,
			conditionType: operatorv1alpha1.ConditionMigrationBlocked,
			want: "1 migration sorts below applied version 5; linear execution refuses it, so renumber it above that version " +
				"or publish it to a database that has not passed that version"},
		{name: "out of order, two", state: dataplane.MigrationStateOutOfOrder, count: 2,
			conditionType: operatorv1alpha1.ConditionMigrationBlocked,
			want: "2 migrations sort below applied version 5; linear execution refuses them, so renumber them above that version " +
				"or publish them to a database that has not passed that version"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Status.UnresolvedRun = row.unresolved
			migration.Status.Conditions = []metav1.Condition{}
			report := dataplane.MigrationStatusReport{
				ContractVersion: dataplane.SupportedMigrationStatusContract, CurrentVersion: 5,
				TotalMigrations: 1 + row.count, Migrations: records(row.state, row.count),
			}
			reconciler := &MigrationReconciler{}
			if err := reconciler.recordMigrationHistory(migration, report, testDigest); err != nil {
				t.Fatal(err)
			}
			assertMigrationCondition(t, migration, row.conditionType, row.want)
		})
	}
}
