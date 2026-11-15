//go:build e2e

package e2e

import (
	"errors"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// driftCheckProof runs a migration under a drift check against a database and
// a shadow of their own. The database also holds a table no migration
// declares, as one another tool writes to, and the check excludes it. The
// first two migrations apply under apply: Always. The third is approved, and
// while its Apply waits behind the closed gate a column nobody migrated is
// added: the approval was given for a schema that no longer stands, and only
// the check at Apply time can see it, since the revision rows the plan read
// are unchanged. The check refuses the run before the migration runs, on the
// one difference the column is. Once the column is dropped, an approval of the
// plan the resource asks about again applies the third migration under the
// same check, against a clean applied history and beside the excluded table.
func (m *migrationRun) driftCheckProof() {
	m.t.Helper()
	name, database, shadow := "e2e-drift-"+m.engine.name, "ptah_e2e_drift", "ptah_e2e_drift_shadow"
	secret, shadowSecret := "e2e-"+m.engine.name+"-drift-db", "e2e-"+m.engine.name+"-drift-shadow-db"
	m.isolatedDatabase(database, secret)
	m.isolatedDatabase(shadow, shadowSecret)
	// The URL names no search_path, so Ptah reads every schema and the
	// selector starts with the schema's name. The table belongs to the
	// migration user, as a table its own tool created would.
	const unmigrated, excluded = "e2e_unmigrated_queue", "public.e2e_unmigrated_queue"
	for _, statement := range []string{
		"CREATE TABLE " + unmigrated + " (id INTEGER PRIMARY KEY)",
		"ALTER TABLE " + unmigrated + " OWNER TO " + migrationDatabaseUser,
	} {
		if _, err := m.sqlStatement(database, statement); err != nil {
			m.fatalf("the table no migration declares could not be created: %v", err)
		}
	}
	unmigratedTables := func() string {
		return m.query("SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = '"+unmigrated+"'", database)
	}
	m.publish("drift-older", m.fixtureDir("-older"), m.reference("-drift-older"))
	m.openApplyGate()
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference("-drift-older"),
		coordinationKey: "e2e/drift/" + m.engine.name, apply: "Always", interval: "1h",
		execution: gatedExecution(),
		edit: func(spec map[string]any) {
			spec["policy"].(map[string]any)["driftCheck"] = map[string]any{
				"shadow":  map[string]any{"urlFrom": map[string]any{"name": shadowSecret, "key": "url"}},
				"exclude": []any{excluded},
			}
		},
	}))
	revisions := func() string { return m.query(restoreRevisionsQuery(m.engine.name), database) }
	firstColor := func() string { return m.query("SELECT color FROM e2e_migration_widgets WHERE id=1", database) }
	initial := m.waitForGenerationInSync(name)
	if !restoreInSyncApplied(initial.Status) || revisions() != "1,2" {
		m.fatalf("%s did not apply its first two migrations under the drift check; the database records [%s]", name, revisions())
	}
	if unmigratedTables() != "1" {
		m.fatalf("the table %s left %s before the first checked Apply, so the run proves nothing about excluding it", unmigrated, database)
	}

	// The three-migration artifact the phase published, and a decision on [3].
	m.patchMigration(name, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": m.reference("")}, "policy": map[string]any{"apply": "OnApproval"},
	}})
	approved := m.waitForRestorePlan(name, "", nil)
	plan := m.planOf(approved)
	if versions := planVersionList(plan); versions != "3" {
		m.fatalf("the plan %s published approves [%s], and this row needs [3]", name, versions)
	}
	if bound := m.migration(name).Status.History; bound == nil || !presentIs(plan.Spec.ShadowIdentityDigest, bound.ShadowIdentityDigest) {
		m.fatalf("the plan %s does not bind the shadow identity the history read recorded", approved)
	}
	m.closeApplyGate()
	m.approveGated(name, approved, "drift")
	jobUID := m.waitForRestoreApplyHeld(name)
	m.logf("adding a column nobody migrated to the %s database under a held Apply", m.engine.kind)
	if _, err := m.sqlStatement(database, "ALTER TABLE e2e_migration_widgets ADD COLUMN nickname TEXT"); err != nil {
		m.fatalf("the out-of-band column could not be added: %v", err)
	}
	m.openApplyGate()
	refused := m.waitForDriftCheck(name, "the drift check's refusal of the approved run", func(resource *ptahv1alpha1.PtahMigration) error {
		return driftCheckRefusedRun(resource.Status, jobUID)
	})
	m.printReading("drift refused", map[string]any{"metadata": readingMetadata(refused), "status": refused.Status})
	// The column is one difference. The excluded table and its primary key
	// would be two more, as the runner harness measured Ptah v0.28.0 count
	// such a table.
	if message := refused.Status.LastRun.Message; !strings.HasPrefix(message, driftFoundMessage+"1 difference ") {
		m.fatalf("the refused run says %q, want the one difference the column is and not the excluded table", message)
	}
	// The message points at the Pod log, which names what differs.
	m.check(driftSentenceLogged(m.executorLog(refused.Status.LastRun)), "read Ptah's drift sentence in the refused run's Pod log")
	// Migration 3 began nowhere: its revision row, its value and its log
	// entries are all absent.
	if revisions() != "1,2" || firstColor() != "unset" ||
		m.query("SELECT count(*) FROM schema_migrations_log WHERE version = 3", database) != "0" {
		m.fatalf("the refused run reached migration 3 in %s", database)
	}

	// The schema back as the history has it, and a decision of its own.
	if _, err := m.sqlStatement(database, "ALTER TABLE e2e_migration_widgets DROP COLUMN nickname"); err != nil {
		m.fatalf("the out-of-band column could not be dropped: %v", err)
	}
	var again string
	asked := m.waitForDriftCheck(name, "a decision asked for after the refused run", func(resource *ptahv1alpha1.PtahMigration) error {
		var ok bool
		if again, ok = decisionAfterRun(resource.Status, jobUID); !ok {
			return errors.New("it asks for no decision after the refused run")
		}
		return nil
	})
	m.printReading("drift decision", map[string]any{"metadata": readingMetadata(asked), "status": asked.Status})
	fresh := m.planOf(again)
	if versions := planVersionList(fresh); versions != "3" {
		m.fatalf("after the refusal %s asks to approve [%s], and the history needs [3]", name, versions)
	}
	m.check(m.approve(name+"-current", name, fresh.Name, string(fresh.UID), fresh.Spec.Fingerprint),
		"approve the plan the drift-checked migration asks about again")
	converged := m.waitForDriftCheck(name, "the held-back migration applied under the drift check", func(resource *ptahv1alpha1.PtahMigration) error {
		return driftCheckedRunApplied(resource, jobUID, 3)
	})
	m.printReading("drift checked", map[string]any{"metadata": readingMetadata(converged), "status": converged.Status})
	job := &batchv1.Job{}
	m.check(m.get(converged.Status.LastRun.JobName, job), "read the drift-checked Apply Job")
	if !mountsShadow(job, shadowSecret) {
		m.fatalf("the Apply Job %s mounts no shadow database, so it was not asked to check", job.Name)
	}
	if !carriesDriftExclude(job, excluded) {
		m.fatalf("the Apply Job %s does not hand its runner the selector %s, so the check was not asked to leave it out", job.Name, excluded)
	}
	if revisions() != "1,2,3" || firstColor() != "blue" {
		m.fatalf("%s did not apply migration 3 after the drift check passed; the database records [%s]", name, revisions())
	}
	if unmigratedTables() != "1" {
		m.fatalf("the table %s left %s before the checked Apply of migration 3, so the run proves nothing about excluding it", unmigrated, database)
	}
	if tables := m.query("SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()", shadow); tables != "0" {
		m.fatalf("the shadow database %s holds %s tables after the checks, and Ptah empties it", shadow, tables)
	}
	m.mfDeleteMigration(name)
	m.retireFixtureApproval(name+"-approval", converged.UID)
	m.retireFixtureApproval(name+"-current", converged.UID)
	m.closeApplyGate()
	m.logf("PASS %s refused an approved Apply on a column added after approval, applied nothing, and applied it under the same check once the schema matched, beside a table the check excludes", m.engine.kind)
}

// waitForDriftCheck reads the migration until match accepts it and returns
// the document it accepted. A wait that runs out names what the last reading
// lacked.
func (m *migrationRun) waitForDriftCheck(name, description string, match func(*ptahv1alpha1.PtahMigration) error) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	var matched *ptahv1alpha1.PtahMigration
	refusal := errors.New("the migration was never read")
	if !m.within(migrationPoll, func() bool {
		resource := m.migration(name)
		if refusal = match(resource); refusal != nil {
			return false
		}
		matched = resource
		return true
	}) {
		m.reportGatedState(name, false)
		m.fatalf("%s did not reach %s within %s: %v", name, description, waitTimeout, refusal)
	}
	return matched
}

// executorLog reads the executor's log of a run's Pod, as the command its
// message ends with does.
func (m *migrationRun) executorLog(run *ptahv1alpha1.MigrationRunStatus) []byte {
	m.t.Helper()
	pods := &corev1.PodList{}
	m.check(m.list(pods, client.MatchingLabels{"job-name": run.JobName}), "list the Pods of Job %s", run.JobName)
	owned := ownedPods(pods.Items, run.JobUID)
	if len(owned) != 1 {
		m.fatalf("Job %s owns %d Pods, want the one that ran", run.JobName, len(owned))
	}
	log, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, owned[0].Name, "ptah")
	m.check(err, "read the executor log of Job %s", run.JobName)
	m.scan(log, "the executor log of Job "+run.JobName)
	return log
}
