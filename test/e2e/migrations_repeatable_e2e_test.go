//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// repeatableMigrationsProof runs a migration whose artifact carries a
// repeatable migration, a view, through every state the operator reads from
// Ptah's record of it, against a database of its own. The first plan holds
// version 1 and the view, after it, and runs once it is approved. A changed
// view runs under apply: Always with no approval, alone. A view that names a
// column the table does not have fails: on PostgreSQL inside a transaction,
// which took no effect, and on MySQL outside one, which leaves the run for a
// person to account for. Either way the record stops the resource, and the
// condition names Ptah's repair command, which the row runs as written after
// it corrects the file; the corrected view then runs. Last, an artifact with no
// repeatable file leaves an applied record no file carries, which stops
// nothing: the resource converges, its history names the record, and one
// Warning Event names the command that would delete it.
func (m *migrationRun) repeatableMigrationsProof() {
	m.t.Helper()
	name, database, secret := "e2e-repeatable-"+m.engine.name, "ptah_e2e_repeatable", "e2e-"+m.engine.name+"-repeatable-db"
	transactional := m.engine.name == "postgresql"
	m.isolatedDatabase(database, secret)
	// Each stage publishes to a reference of its own, and the rows after
	// this one read nothing publish records. The references are written out:
	// the artifact check in hack names an artifact by its literal suffix.
	published := m.published
	defer func() { m.published = published }()
	m.publish("repeatable-first", m.repeatableFixture("first"), m.reference("-repeatable-first"))
	m.publish("repeatable-changed", m.repeatableFixture("changed"), m.reference("-repeatable-changed"))
	m.publish("repeatable-failing", m.repeatableFixture("failing"), m.reference("-repeatable-failing"))
	m.publish("repeatable-removed", m.repeatableFixture("removed"), m.reference("-repeatable-removed"))
	record := func(column string) string {
		return m.query("SELECT "+column+" FROM schema_migrations_repeatable WHERE identity = '"+repeatableIdentity+"'", database)
	}
	records := func() string { return m.query("SELECT count(*) FROM schema_migrations_repeatable", database) }
	schema := "table_schema = current_schema()"
	if m.engine.name == "mysql" {
		schema = "table_schema = database()"
	}
	viewColumns := func() string {
		return m.query("SELECT count(*) FROM information_schema.columns WHERE "+schema+" AND table_name = 'e2e_item_names'", database)
	}

	// Version 1 and the view, approved.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference("-repeatable-first"),
		coordinationKey: "e2e/repeatable/" + m.engine.name, apply: "OnApproval", interval: "15s",
	}))
	firstPlan := m.planOf(m.waitForRestorePlan(name, "", nil))
	m.check(repeatablePlanned(firstPlan, []int64{1}, repeatableIdentity, ""), "read the first plan of %s", name)
	m.approveGated(name, firstPlan.Name, "repeatable")
	first := m.waitForRepeatable(name, "version 1 and the view applied", func(resource *ptahv1alpha1.PtahMigration) error {
		return settledOnRepeatableRun(resource, "", []int64{1}, []string{repeatableIdentity})
	})
	m.printReading("repeatable applied", map[string]any{"metadata": readingMetadata(first), "status": first.Status})
	firstChecksum := firstPlan.Spec.Repeatables[0].Checksum
	if state, checksum := record("state"), record("checksum"); state != "applied" || checksum != firstChecksum {
		m.fatalf("the database records %s as %q with content %q, and the approved run applied %s", repeatableIdentity, state, checksum, firstChecksum)
	}
	if columns := viewColumns(); columns != "1" {
		m.fatalf("the view has %s columns after the first run, want 1", columns)
	}
	m.assertRepeatableLogged(database, first.Status.LastRun.OperationID, "started", "applied")

	// The changed view, under apply: Always.
	m.patchMigration(name, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": m.reference("-repeatable-changed")}, "policy": map[string]any{"apply": "Always"},
	}})
	changed := m.waitForRepeatable(name, "the changed view applied without an approval", func(resource *ptahv1alpha1.PtahMigration) error {
		return settledOnRepeatableRun(resource, string(first.Status.LastRun.JobUID), nil, []string{repeatableIdentity})
	})
	if authorization := changed.Status.LastRun.Authorization; authorization == nil ||
		authorization.Kind != ptahv1alpha1.MigrationRunAuthorizedByPolicy {
		m.fatalf("the run of the changed view was authorized by %+v, want the Always policy", authorization)
	}
	changedPlan := m.runPlan(changed)
	m.check(repeatablePlanned(changedPlan, nil, repeatableIdentity, firstChecksum), "read the plan of the changed view")
	changedChecksum := changedPlan.Spec.Repeatables[0].Checksum
	if state, checksum := record("state"), record("checksum"); state != "applied" || checksum != changedChecksum {
		m.fatalf("the database records %s as %q with content %q, and the run applied %s", repeatableIdentity, state, checksum, changedChecksum)
	}
	if columns := viewColumns(); columns != "2" {
		m.fatalf("the view has %s columns after the changed run, want 2", columns)
	}
	m.assertRepeatableLogged(database, changed.Status.LastRun.OperationID, "started", "applied")

	// A view that cannot be created.
	m.patchMigration(name, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": m.reference("-repeatable-failing")},
	}})
	var force string
	stopped := m.waitForRepeatable(name, "the failed view stopping the migration", func(resource *ptahv1alpha1.PtahMigration) error {
		if resource.Status.ObservedGeneration != resource.Generation {
			return fmt.Errorf("the status was written for generation %d, and the migration is at %d",
				resource.Status.ObservedGeneration, resource.Generation)
		}
		if err := repeatableAttemptRecorded(resource.Status, string(changed.Status.LastRun.JobUID), repeatableIdentity, transactional); err != nil {
			return err
		}
		var err error
		force, err = repeatableStop(resource.Status, repeatableIdentity)
		return err
	})
	m.printReading("repeatable unfinished", map[string]any{"metadata": readingMetadata(stopped), "status": stopped.Status})
	if state := record("state"); state != "failed" {
		m.fatalf("the database records %s as %q after the failed run, want failed", repeatableIdentity, state)
	}
	m.assertRepeatableLogged(database, stopped.Status.LastRun.OperationID, "started", "failed")
	// What a person establishes before the repair: the attempt left the view
	// as the run before it made it.
	if columns := viewColumns(); columns != "2" {
		m.fatalf("the view has %s columns after the failed attempt, want the 2 the run before it left", columns)
	}

	// The corrected file, the record repaired with the command the condition
	// names, and the run accounted for where it may have left work behind.
	m.patchMigration(name, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": m.reference("-repeatable-changed")},
	}})
	m.waitForRepeatable(name, "the corrected file still stopped by the record", func(resource *ptahv1alpha1.PtahMigration) error {
		if resource.Status.ObservedGeneration != resource.Generation {
			return fmt.Errorf("the status was written for generation %d, and the migration is at %d",
				resource.Status.ObservedGeneration, resource.Generation)
		}
		_, err := repeatableStop(resource.Status, repeatableIdentity)
		return err
	})
	m.runRepeatableRepair("unfinished", secret, "repeatable-changed", force)
	if count := records(); count != "0" {
		m.fatalf("the repair left %s records of repeatable migrations in %s", count, database)
	}
	if !transactional {
		m.acknowledgeUnresolvedRun(name, stopped.Status.UnresolvedRun.OperationID)
	}
	recovered := m.waitForRepeatable(name, "the corrected view applied after the repair", func(resource *ptahv1alpha1.PtahMigration) error {
		return settledOnRepeatableRun(resource, string(stopped.Status.LastRun.JobUID), nil, []string{repeatableIdentity})
	})
	// The repair deleted the record, so the plan reads the file as new.
	m.check(repeatablePlanned(m.runPlan(recovered), nil, repeatableIdentity, ""), "read the plan after the repair")
	if state, checksum := record("state"), record("checksum"); state != "applied" || checksum != changedChecksum {
		m.fatalf("the database records %s as %q with content %q after the repair, want the corrected content %s",
			repeatableIdentity, state, checksum, changedChecksum)
	}
	m.assertRepeatableLogged(database, recovered.Status.LastRun.OperationID, "started", "applied")

	// An artifact that no longer carries the file. Ptah leaves the applied
	// record alone and reports nothing to do, so the migration converges as it
	// would without the record: nothing runs, the history names the record and
	// one Warning Event names the command that deletes it.
	m.patchMigration(name, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": m.reference("-repeatable-removed")},
	}})
	previousJob := string(recovered.Status.LastRun.JobUID)
	orphaned := m.waitForRepeatable(name, "the migration converged beside the record no file carries", func(resource *ptahv1alpha1.PtahMigration) error {
		return orphanedRepeatableSettled(resource, previousJob, repeatableIdentity)
	})
	m.printReading("repeatable orphaned", map[string]any{"metadata": readingMetadata(orphaned), "status": orphaned.Status})
	if state, checksum := record("state"), record("checksum"); state != "applied" || checksum != changedChecksum {
		m.fatalf("the database records %s as %q with content %q beside an artifact without its file, want the applied record of %s kept",
			repeatableIdentity, state, checksum, changedChecksum)
	}
	// Each read after the first lists the record again. The first later read
	// is one an operator that names the record on every read would name it
	// in, and the read after it comes long after that Event would have been
	// stored, so the Events are read only then.
	later := orphaned
	for _, which := range []string{"first", "second"} {
		before := later
		later = m.waitForRepeatable(name, "the "+which+" history read after the one that listed the record",
			func(resource *ptahv1alpha1.PtahMigration) error {
				if err := orphanedRepeatableSettled(resource, previousJob, repeatableIdentity); err != nil {
					return err
				}
				if read := resource.Status.History.ObservedAt; !read.After(before.Status.History.ObservedAt.Time) {
					return fmt.Errorf("the history was last read at %s, which is not after %s",
						read.UTC().Format(time.RFC3339), before.Status.History.ObservedAt.UTC().Format(time.RFC3339))
				}
				return nil
			})
	}
	events := &corev1.EventList{}
	m.check(m.list(events), "list the Events of %s", name)
	m.check(orphanedRepeatableEvent(events.Items, later.UID, repeatableIdentity),
		"read the Event that names the record of %s no file carries", repeatableIdentity)
	// Ptah ran nothing for the record, so the view stays as the last run left it.
	if columns := viewColumns(); columns != "2" {
		m.fatalf("the view has %s columns beside the record no file carries, want the 2 the last run left", columns)
	}
	m.mfDeleteMigration(name)
	m.retireFixtureApproval(name+"-approval", later.UID)
	m.logf("PASS %s planned, approved and applied a repeatable migration after its version, applied a changed one under Always, "+
		"stopped on a failed attempt and settled after the repair command its condition named, and converged beside a record "+
		"no file carries, which one Warning Event named",
		m.engine.kind)
}

// repeatableFixture is a stage of the repeatable migration fixtures, a
// directory under testdata/e2e/migrations/repeatable: the view first, the view
// with a column added, the view naming a column the table does not have, and
// version 1 with no repeatable file at all. Both engines publish the same
// directories.
func (m *migrationRun) repeatableFixture(stage string) string {
	return filepath.Join(repositoryRoot, "testdata", "e2e", "migrations", "repeatable", stage)
}

// runPlan reads the plan the last run carried out.
func (m *migrationRun) runPlan(resource *ptahv1alpha1.PtahMigration) *ptahv1alpha1.PtahMigrationPlan {
	m.t.Helper()
	run := resource.Status.LastRun
	if run == nil || run.PlanRef == nil || run.PlanRef.Name == "" {
		m.fatalf("the last run of %s names no plan", resource.Name)
	}
	plan := m.planOf(run.PlanRef.Name)
	if plan.UID != run.PlanRef.UID {
		m.fatalf("the plan %s is not the one the run of %s carried out", plan.Name, resource.Name)
	}
	return plan
}

// waitForRepeatable reads the migration until match accepts it and returns
// the document it accepted. A wait that runs out names what the last reading
// lacked.
func (m *migrationRun) waitForRepeatable(name, description string, match func(*ptahv1alpha1.PtahMigration) error) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	var matched, last *ptahv1alpha1.PtahMigration
	refusal := errors.New("the migration was never read")
	if !m.within(migrationPoll, func() bool {
		last = m.migration(name)
		if refusal = match(last); refusal != nil {
			return false
		}
		matched = last
		return true
	}) {
		m.reportMigration(last)
		m.fatalf("%s did not reach %s within %s: %v", name, description, waitTimeout, refusal)
	}
	return matched
}

// assertRepeatableLogged holds the database's log entries about the row's
// repeatable migration, for the run of the operation given, to the states
// given.
func (m *migrationRun) assertRepeatableLogged(database, operation string, states ...string) {
	m.t.Helper()
	output, err := m.sqlStatement(database, m.engine.repeatableLogQuery(repeatableIdentity))
	m.check(err, "read the entries of %s in schema_migrations_log in %s", repeatableIdentity, database)
	m.check(repeatableRunLogged(output, operation, states...), "the log of %s in %s", repeatableIdentity, database)
}

// runRepeatableRepair runs the repair command a condition named, as a person
// runs it from a checkout of the migration directory: the files of the stage
// given, mounted from the ConfigMap its publisher read, and the database URL
// from the row's Secret. The Job holds a database credential and no registry
// credential.
func (m *migrationRun) runRepeatableRepair(what, secret, stage, command string) {
	m.t.Helper()
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "ptah" {
		m.fatalf("the condition named %q, which is not a ptah command", command)
	}
	configMapName := "e2e-migrations-" + m.engine.name + "-" + stage
	configMap := &corev1.ConfigMap{}
	m.check(m.get(configMapName, configMap), "read the migration files of %s", stage)
	var files []string
	for key := range configMap.Data {
		if strings.HasSuffix(key, ".sql") {
			files = append(files, key)
		}
	}
	slices.Sort(files)
	if len(files) == 0 {
		m.fatalf("the ConfigMap %s carries no migration file", configMapName)
	}
	var mounts []any
	for _, file := range files {
		mounts = append(mounts, map[string]any{"name": "migrations", "mountPath": "/migrations/" + file, "subPath": file, "readOnly": true})
	}
	mounts = append(mounts, map[string]any{"name": "work", "mountPath": "/work"})
	args := make([]any, 0, len(fields)+5)
	for _, field := range fields[1:] {
		args = append(args, field)
	}
	args = append(args, "--migrations-dir", "/migrations", "--dir-format", "ptah", "--db-url", "$(PTAH_E2E_TARGET_URL)")
	name := "e2e-repeatable-repair-" + m.engine.name + "-" + what
	labels := map[string]any{"app.kubernetes.io/component": "e2e-migration-repairer"}
	m.logf("running %q against the %s database", command, m.engine.kind)
	m.check(m.create(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": int64(0), "activeDeadlineSeconds": int64(300),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}},
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name": "repairer", "image": m.in.ExecutorImage, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/usr/local/bin/ptah"},
						"args":    args,
						"env": []any{
							map[string]any{"name": "HOME", "value": "/work"},
							map[string]any{"name": "TMPDIR", "value": "/work"},
							map[string]any{"name": "PTAH_E2E_TARGET_URL", "valueFrom": map[string]any{
								"secretKeyRef": map[string]any{"name": secret, "key": "url"},
							}},
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": mounts,
					}},
					"volumes": []any{
						map[string]any{"name": "migrations", "configMap": map[string]any{"name": configMapName}},
						map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
					},
				},
			},
		},
	}), "create Job %s", name)
	job := &batchv1.Job{}
	m.check(m.get(name, job), "read Job %s", name)
	if !adopterIsolated(job, secret) {
		m.fatalf("the repair Job %s did not keep every credential but the database's out of the process that runs SQL", name)
	}
	m.poll("the repair Job "+name+" to finish", 3*time.Second, func() bool {
		current := &batchv1.Job{}
		if m.get(name, current) != nil {
			return false
		}
		if current.Status.Succeeded > 0 {
			return true
		}
		if current.Status.Failed > 0 {
			// The repairer holds a database URL, so its words are printed only
			// through the scanner that refuses one.
			logs, _ := m.readJobLogs(current)
			m.scan(logs, "the repair log")
			_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: the repair said:\n%s\n", logs)
			m.fatalf("Ptah refused the repair command %q the condition named", command)
		}
		return false
	})
	m.deleteAndWait(job, "the finished repair Job "+name)
}
