//go:build e2e

package e2e

import (
	"time"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// readRequestProof asks the main migration, whose next read is an hour away,
// to read its history now: once after a person deletes the newest revision
// row outside the operator, and once after they put it back. Each read has to
// come from the request, inside readRequestBound and before the read the
// interval set, and neither may start an Apply, although the first reading
// leaves a migration pending.
func (m *migrationRun) readRequestProof() {
	m.t.Helper()
	name := m.migrationName()
	database := m.migrationDatabase()
	current := m.migration(name)
	if current.Spec.Policy.Apply != ptahv1alpha1.ApplyPolicyOnApproval {
		m.fatalf("%s applies under %q, so a revision deleted for this row would be run again", name, current.Spec.Policy.Apply)
	}
	var restore any // a null merge patch removes an interval the resource did not set
	if interval := current.Spec.Interval.Duration; interval > 0 {
		restore = interval.String()
	}
	m.patchMigration(name, map[string]any{"spec": map[string]any{"interval": "1h"}})
	settled := m.waitForMigration(name, "a settled history with an hour to its next read", migrationPoll,
		func(resource *ptahv1alpha1.PtahMigration) bool {
			next := resource.Status.NextReconciliationTime
			return resource.Status.ObservedGeneration == resource.Generation &&
				resource.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && resource.Status.ActiveOperation == nil &&
				next != nil && next.After(time.Now().Add(45*time.Minute)) &&
				resource.Status.History != nil && resource.Status.History.CurrentVersion == 3
		})
	intervalRead := settled.Status.NextReconciliationTime.Time
	applies := m.applyJobUIDs(name)

	m.logf("deleting the %s revision of migration 3 behind the operator's back and asking for a read", m.engine.kind)
	if _, err := m.sqlStatement(database, "CREATE TABLE e2e_read_request_revision AS SELECT * FROM schema_migrations WHERE version = 3"); err != nil {
		m.fatalf("could not keep the %s revision of migration 3: %v", m.engine.name, err)
	}
	if _, err := m.sqlStatement(database, "DELETE FROM schema_migrations WHERE version = 3"); err != nil {
		m.fatalf("could not delete the %s revision of migration 3: %v", m.engine.name, err)
	}
	requested, value := m.requestRead(name)
	m.waitForMigration(name, "the deleted revision, read on request", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		return historyReadOnRequest(resource, value, requested, intervalRead, 2)
	})
	m.assertNoNewApplyJob(applies, "after a read request found migration 3 pending", name)

	m.logf("putting the %s revision back and asking again", m.engine.kind)
	if _, err := m.sqlStatement(database, "INSERT INTO schema_migrations SELECT * FROM e2e_read_request_revision"); err != nil {
		m.fatalf("could not put the %s revision of migration 3 back: %v", m.engine.name, err)
	}
	if _, err := m.sqlStatement(database, "DROP TABLE e2e_read_request_revision"); err != nil {
		m.fatalf("could not drop the kept %s revision: %v", m.engine.name, err)
	}
	requested, value = m.requestRead(name)
	m.waitForMigration(name, "the restored revision, read on request", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		return historyReadOnRequest(resource, value, requested, intervalRead, 3)
	})
	m.assertNoNewApplyJob(applies, "after a read request found the history restored", name)

	m.patchMigration(name, map[string]any{"spec": map[string]any{"interval": restore}})
	m.waitForMigration(name, "the history settled on its own interval again", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		return resource.Status.ObservedGeneration == resource.Generation &&
			resource.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && resource.Status.ActiveOperation == nil &&
			resource.Status.Plan == nil && resource.Status.History != nil && resource.Status.History.CurrentVersion == 3
	})
	m.logf("PASS %s history read on request after a revision was deleted and restored, without an Apply", m.engine.kind)
}

// requestRead annotates the migration with a read request, the instant a
// person would write, and returns that instant and the value.
func (m *migrationRun) requestRead(name string) (time.Time, string) {
	m.t.Helper()
	requested := time.Now().UTC().Truncate(time.Second)
	value := requested.Format(time.RFC3339)
	m.patchMigration(name, map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{ptahv1alpha1.ReadRequestAnnotation: value},
	}})
	return requested, value
}
