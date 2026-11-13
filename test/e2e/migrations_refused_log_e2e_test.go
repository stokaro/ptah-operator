//go:build e2e

package e2e

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// refusedLogProof runs the phase's artifact under apply: Always against a
// database whose schema_migrations_log has an actor column narrower than any
// name the manager records. MySQL refuses every entry the run writes and the
// run applies all the same. The run's record, its journal entry and RunLogged
// say the database's own record of it is incomplete, all from the one status
// write that recorded the run, and the table holds no row of it.
func (m *migrationRun) refusedLogProof() {
	m.t.Helper()
	// Not lost-log, which is the row whose Pod log is lost.
	name, database, secret := "e2e-refused-log-"+m.engine.name, "ptah_e2e_refused_log", "e2e-"+m.engine.name+"-refused-log-db"
	m.isolatedDatabase(database, secret)
	_, err := m.sqlStatement(database, narrowMigrationLogTable)
	m.check(err, "create a narrow schema_migrations_log in %s", database)
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/refused-log/" + m.engine.name, apply: "Always",
	}))
	// lastRun, its journal entry and RunLogged are one status write, so the
	// document that first carries the record carries all three.
	recorded := m.waitForMigration(name, "a run that recorded what its log writes came to", migrationPoll,
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return migration.Status.LastRun != nil && migration.Status.LastRun.MigrationLog != nil &&
				meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationRunLogged) != nil
		})
	m.printReading("refused log", map[string]any{
		"metadata": readingMetadata(recorded), "status": recorded.Status,
	})
	if err := everyLogEntryLost(recorded); err != nil {
		m.fatalf("%s: %v", name, err)
	}
	if rows := m.query("SELECT count(*) FROM schema_migrations_log", database); rows != "0" {
		m.fatalf("schema_migrations_log in %s holds %q rows, and the run reported every entry lost", database, rows)
	}
	lost := recorded.Status.LastRun.MigrationLog.Lost
	view := string(m.kubectlPtahView(name))
	if !strings.Contains(view, "  Migration log:") || !strings.Contains(view, "refused, so the database's record of the run is incomplete") {
		m.fatalf("kubectl ptah migration %s does not say the run's log is incomplete:\n%s", name, view)
	}
	m.mfDeleteMigration(name)
	m.logf("PASS %s refused all %d log entries of an applied run, recorded on lastRun, RunFinished and RunLogged",
		m.engine.kind, lost)
}
