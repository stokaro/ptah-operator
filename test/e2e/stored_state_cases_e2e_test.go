//go:build e2e

package e2e

import (
	"bytes"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (f *faultRun) unsupportedControllerStates() {
	f.t.Helper()
	for _, engine := range []string{"postgresql", "mysql"} {
		name, database := "e2e-stored-state-"+engine, "e2e_stored_state"
		secret := name + "-db"
		kind, reference, service := "PostgreSQL", f.pgReference, pgService
		if engine == "mysql" {
			kind, reference, service = "MySQL", f.mysqlReference, mysqlService
		}
		f.createDatabase(engine, database, secret)
		f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'state-control', 'preserve-this-row')")
		window := f.startSchemaRefusalWindow(name, engine, database, secret)
		beforeJobs := f.checkpointJobs(name, "")
		f.createSchema(faultSchema{name: name, engine: kind, reference: reference, secret: secret, coordinationKey: "e2e/stored-state/" + engine})
		before := window.waitForSchema("the supported-state approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
			return storedStateReady(resource, f.stateVersion()) == nil
		})
		controls := []operationSQLClient{window.resultControl(before, "observe", beforeJobs), window.resultControl(before, "plan", beforeJobs)}
		barrier := &controllerStatusBarrier{cluster: f.cluster, role: f.controllerName,
			user:      "system:serviceaccount:" + f.in.OperatorNamespace + ":" + f.controllerServiceAccount,
			namespace: f.in.TestNamespace, resource: "ptahschemas"}
		holdUnsupportedStoredState(f.t, f.ctx, f.cluster, barrier, before, service,
			func() client.Object {
				f.createApproval(name, name+"-old")
				approval := &ptahv1alpha1.PtahSchemaApproval{}
				f.check(f.get(name+"-old", approval), "read the admitted supported-state approval")
				return approval
			},
			func(resource client.Object) {
				window.assert(resource.(*ptahv1alpha1.PtahSchema), controls...)
				f.assertColumn(engine, database, "fault_token", 0)
				if f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='state-control' AND note='preserve-this-row'") != "1" {
					f.fatalf("unsupported controller state changed the preserved row")
				}
			}, f.scan)
		current := window.waitForSchema("the restored supported-state approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
			return storedStateReady(resource, f.stateVersion()) == nil
		})
		plan := f.schemaPlan(current.Status.Plan.Name)
		beforeSQL, beforeApply := window.audit.snapshot(), f.checkpointJobs(name, "apply")
		f.createApproval(name, name+"-current")
		f.waitForApprovedPlanConverged(name, plan.Spec.ArtifactDigest, plan.Spec.Fingerprint, string(plan.UID), "the freshly approved supported state to converge")
		result := f.captureOneNewJobResult(name, "apply", beforeApply, nil)
		f.check(automaticApplyResult(result, plan.Spec.ContentDigest, plan.Spec.CoordinationDigest, plan.Spec.TargetIdentityDigest), "execute the exact supported-state plan")
		job := &batchv1.Job{}
		f.check(f.get(f.captured.jobName, job), "read the supported-state Apply Job")
		if job.UID != types.UID(f.captured.jobUID) || job.Annotations[annotationControllerState] != f.controller.stateVersion ||
			job.Annotations[annotationBindingID] != plan.Spec.ExecutionBindingID || !jobUsesExecutor(job, plan.Spec.ExecutorImage) {
			f.fatalf("the supported-state Apply lost its exact execution contract")
		}
		window.audit.assertRecords(beforeSQL, window.audit.snapshot(), window.audit.terminalPod(map[string]string{"job-name": job.Name}, string(job.UID)), true)
		window.audit.close()
		f.assertApprovalConsumed(name+"-current", string(plan.UID))
		f.assertOneNewJob(name, "apply", beforeApply)
		f.assertColumn(engine, database, "fault_token", 1)
		if f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='state-control' AND note='preserve-this-row'") != "1" ||
			f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" {
			f.fatalf("the supported-state control did not preserve the populated database")
		}
		f.logf("PASS %s unsupported controller state after schema approval: runtime refusal retained exact state; no workload or unauthorized SQL; supported state applied once after a new approval", engine)
	}
	f.auditRuntimeCredentials()
}

func (m *migrationRun) unsupportedControllerState() {
	m.t.Helper()
	name, database := "e2e-migration-stored-state-"+m.engine.name, "ptah_e2e_stored_state"
	secret := name + "-db"
	var user string
	if m.engine.name == "mysql" {
		user = m.isolatedMySQLAuditDatabase(database, secret)
	} else {
		m.isolatedDatabase(database, secret)
	}
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: m.engine.name}
	var pgBefore []byte
	var mysqlBefore []mysqlStatementRecord
	if m.engine.name == "postgresql" {
		audit.snapshot()
		pgBefore = audit.pgPrefix
	} else {
		mysqlBefore = audit.mysqlStatementSnapshot()
	}
	inventory := &migrationSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}}
	m.mustCreate(m.migrationDocument(migrationSpec{name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/stored-state/" + m.engine.name, apply: "OnApproval", interval: "1h"}))
	before := m.waitForMigration(name, "the supported-state migration approval gate", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		m.captureMigrationSQLInventory(name, inventory)
		return storedStateReady(resource, m.stateVersion) == nil
	})
	old := m.planOf(before.Status.Plan.Name)
	history := m.executorHistoryControl(before, old, inventory)
	holdUnsupportedStoredState(m.t, m.ctx, m.cluster, m.statusBarrier(), before, m.engine.service,
		func() client.Object {
			m.check(m.approve(name+"-old", name, old.Name, string(old.UID), old.Spec.Fingerprint), "admit the supported-state migration decision")
			approval := &ptahv1alpha1.PtahMigrationApproval{}
			m.check(m.get(name+"-old", approval), "read the admitted supported-state migration approval")
			return approval
		},
		func(object client.Object) {
			resource := object.(*ptahv1alpha1.PtahMigration)
			m.captureMigrationSQLInventory(name, inventory)
			clients, err := inventory.clients(resource)
			m.check(err, "attribute the supported-state History's actual SQL")
			var counts map[string]int
			if m.engine.name == "postgresql" {
				audit.snapshot()
				if len(pgBefore) == 0 || !bytes.HasPrefix(audit.pgPrefix, pgBefore) {
					m.fatalf("the unsupported-state audit lost its PostgreSQL journal")
				}
				counts, err = inventory.postgresRefusalSQL(audit.pgPrefix[len(pgBefore):], database, clients, "")
			} else {
				counts, err = mysqlMigrationRefusalSQL(mysqlBefore, audit.mysqlStatementSnapshot(), database, user, clients)
			}
			m.check(err, "refuse unauthorized SQL while unsupported state was held")
			m.check(storedStateHistorySQLControl(clients, counts, history), "receive SQL from the exact initial History control")
			m.reportMigrationRefusalSQL(resource, clients, counts)
			m.assertDatabaseUnmigrated(name, database)
		}, m.scan)
	current := m.waitForMigration(name, "the restored supported-state migration approval gate", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		return storedStateReady(resource, m.stateVersion) == nil
	})
	plan := m.planOf(current.Status.Plan.Name)
	beforeApply := audit.snapshot()
	m.check(m.approve(name+"-current", name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "approve the supported-state migration plan again")
	converged := m.waitForGenerationInSync(name)
	jobs, run := m.applyJobUIDs(name), converged.Status.LastRun
	if len(jobs) != 1 || run == nil || string(run.JobUID) != jobs[0] || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied ||
		!slices.Equal(run.AppliedVersions, []int64{1, 2, 3}) {
		m.fatalf("the supported-state migration did not Apply exactly once")
	}
	job := &batchv1.Job{}
	m.check(m.get(run.JobName, job), "read the supported-state migration Apply Job")
	m.check(migrationExecutorApplyInputs(job, plan), "bind the supported-state Apply to its exact approved sequence")
	if job.UID != run.JobUID || job.Annotations[annotationControllerState] != m.controller.stateVersion ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahMigration", current.Name, current.UID) {
		m.fatalf("the supported-state migration Apply lost its resource or state version")
	}
	consumed := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-current", consumed), "read the consumed supported-state approval")
	if consumed.Spec.PlanRef.UID != plan.UID || consumed.Spec.PlanFingerprint != plan.Spec.Fingerprint || !conditionStatus(consumed.Status.Conditions, "Consumed", "True") {
		m.fatalf("the supported-state migration did not consume its exact fresh approval")
	}
	audit.assertRecords(beforeApply, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": run.JobName}, string(run.JobUID)), true)
	audit.close()
	if m.query(restoreRevisionsQuery(m.engine.name), database) != "1,2,3" || m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id=1", database) != "blue" {
		m.fatalf("the supported-state migration lost its actual revisions, rows or schema effect")
	}
	m.finishFixture(name)
	m.logf("PASS %s unsupported controller state after migration approval: runtime refusal retained exact state; no workload or unauthorized SQL; supported state applied once after a new approval", m.engine.kind)
}
