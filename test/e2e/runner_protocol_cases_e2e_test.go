//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/resultframe"
)

func (f *faultRun) unsupportedRunnerProtocols() {
	f.t.Helper()
	watcher, err := client.NewWithWatch(f.cluster.Config, client.Options{Scheme: f.cluster.Scheme})
	f.check(err, "open the unsupported runner's direct API watches")
	jobs := newStoredStateRecorder[*batchv1.Job](f.t, f.ctx, watcher, "runner-schema-jobs", f.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](f.t, f.ctx, watcher, "runner-schema-pods", f.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	schemas := newStoredStateRecorder[*ptahv1alpha1.PtahSchema](f.t, f.ctx, watcher, "runner-schemas", f.in.TestNamespace, func() client.ObjectList { return &ptahv1alpha1.PtahSchemaList{} })
	replacement, publisher := publishRunnerProtocolVariant(f.t, f.ctx, f.cluster, f.in.TestNamespace, "e2e-schema-runner-variant",
		f.in.FixtureImage, f.in.RunnerImage, nil, f.scan)
	change := executionComponentChange{"runner-image", f.in.RunnerImage, replacement}
	scenario := f.t
	scenario.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := setControllerExecutionComponent(ctx, f.cluster, f.operatorKey(f.controllerName), change.reverse()); err != nil {
			scenario.Errorf("restore the exact supported runner image: %v", err)
		}
	})
	type row struct {
		engine, name, database string
		window                 *schemaRefusalWindow
		before                 *ptahv1alpha1.PtahSchema
		plan                   *ptahv1alpha1.PtahSchemaPlan
		controls               []operationSQLClient
		refused                runnerProtocolApply
		fresh                  runnerProtocolApply
	}
	var rows []*row
	for _, engine := range []string{"postgresql", "mysql"} {
		r := &row{engine: engine, name: "e2e-runner-protocol-" + engine, database: "e2e_runner_protocol"}
		secret := r.name + "-db"
		kind, reference := "PostgreSQL", f.pgReference
		if engine == "mysql" {
			kind, reference = "MySQL", f.mysqlReference
		}
		f.createDatabase(engine, r.database, secret)
		f.query(engine, r.database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'runner-control', 'preserve-this-row')")
		r.window = f.startSchemaRefusalWindow(r.name, engine, r.database, secret)
		r.window.refusedRunnerImage = replacement
		before := f.checkpointJobs(r.name, "")
		f.createSchema(faultSchema{name: r.name, engine: kind, reference: reference, secret: secret,
			coordinationKey: "e2e/runner-protocol/" + engine, activeDeadline: 120})
		r.before = r.window.waitForSchema("the supported runner's original approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
			return storedStateReady(resource, f.stateVersion()) == nil
		})
		r.plan = f.schemaPlan(r.before.Status.Plan.Name)
		r.controls = []operationSQLClient{r.window.resultControl(r.before, "observe", before), r.window.resultControl(r.before, "plan", before)}
		rows = append(rows, r)
	}
	f.pauseStatusWrites()
	for _, r := range rows {
		f.createApproval(r.name, r.name+"-old")
		f.assertNoNewJobs(r.name, "apply", nil)
	}
	f.rolloutExecutionComponent(change)
	f.mustResumeStatusWrites("resume the valid admitted decisions under the incompatible runner")
	for _, r := range rows {
		r.refused = waitRunnerApply(f.t, f.ctx, f.cluster, f.in.TestNamespace, labelSchema, r.name, f.scan)
		f.check(runnerProtocolApplyInputs(r.refused.job, "PtahSchema", r.before, r.before.Status.ExecutionBinding, replacement, r.plan.Spec.Fingerprint, f.controller),
			"retain the supported contract in the actual incompatible-runner Job")
		if !resultTransportPod(r.refused.pod) || !terminatedContainer(r.refused.pod, "ptah", 0) {
			f.fatalf("the incompatible schema runner lost its successful refusal transport")
		}
		logs, err := f.cluster.ContainerLog(f.ctx, f.in.TestNamespace, r.refused.pod.Name, "ptah")
		f.check(err, "read the exact foreign Apply result")
		f.scan(logs, "the complete incompatible runner Apply result")
		f.check(unsupportedRunnerFrame(logs, runner.OperationApply, r.refused.job.Annotations[annotationOperationID]), "receive the exact foreign protocol refusal")
		r.window.waitForSchema("the refused Apply retained for read-only recovery", func(resource *ptahv1alpha1.PtahSchema) bool {
			pending := resource.Status.PendingObservation
			return pending != nil && pending.ApplyJobUID == r.refused.job.UID && pending.Outcome == ptahv1alpha1.PendingObservationOutcomeUnknown
		})
		f.assertApprovalConsumed(r.name+"-old", string(r.plan.UID))
	}
	// Every started container, including a refused read-only authority guard,
	// is audited before the manager that used the fixture is removed.
	f.rolloutExecutionComponent(change.reverse())
	for _, r := range rows {
		current := r.window.waitForSchema("supported read-only recovery to reach a fresh approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
			return storedStateReady(resource, f.stateVersion()) == nil
		})
		if current.UID != r.before.UID || current.Generation != r.before.Generation || !equality.Semantic.DeepEqual(current.Spec, r.before.Spec) ||
			!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, r.before.Status.ExecutionBinding) {
			f.fatalf("runner image recovery changed the approved schema or its supported execution binding")
		}
		storedStateWatchBarrier(f.t, f.ctx, f.cluster, jobs, publisher)
		closeRunnerPodBoundary(f.t, f.ctx, f.cluster, pods, r.engine, f.in.TestNamespace)
		f.check(runnerProtocolNoReplay(runnerJobHistory(jobs), runnerPodHistory(pods), "PtahSchema", r.name, f.in.TestNamespace, current.UID,
			[]runnerProtocolApply{r.refused}), "hold the refused decision without replay before fresh approval")
		r.window.assert(current, r.controls...)
		f.assertColumn(r.engine, r.database, "fault_token", 0)
		if f.query(r.engine, r.database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='runner-control' AND note='preserve-this-row'") != "1" {
			f.fatalf("the incompatible runner changed the preserved database row")
		}
		plan := f.schemaPlan(current.Status.Plan.Name)
		beforeSQL, beforeApply := r.window.audit.snapshot(), f.checkpointJobs(r.name, "apply")
		f.createApproval(r.name, r.name+"-current")
		f.waitForApprovedPlanConverged(r.name, plan.Spec.ArtifactDigest, plan.Spec.Fingerprint, string(plan.UID), "the restored runner to execute the fresh decision")
		result := f.captureOneNewJobResult(r.name, "apply", beforeApply, nil)
		f.check(automaticApplyResult(result, plan.Spec.ContentDigest, plan.Spec.CoordinationDigest, plan.Spec.TargetIdentityDigest), "execute the exact supported schema plan")
		freshJob, freshPod := &batchv1.Job{}, &corev1.Pod{}
		f.check(f.get(f.captured.jobName, freshJob), "retain the supported runner's fresh Apply Job")
		f.check(f.get(f.captured.podName, freshPod), "retain the supported runner's fresh Apply Pod")
		r.fresh = runnerProtocolApply{freshJob, freshPod}
		f.check(runnerProtocolApplyInputs(freshJob, "PtahSchema", current, current.Status.ExecutionBinding, change.original, plan.Spec.Fingerprint, f.controller), "bind the restored runner's actual Apply")
		r.window.audit.assertRecords(beforeSQL, r.window.audit.snapshot(), r.window.audit.terminalPod(map[string]string{"job-name": freshJob.Name}, string(freshJob.UID)), true)
		r.window.audit.close()
		f.assertApprovalConsumed(r.name+"-current", string(plan.UID))
		f.assertOneNewJob(r.name, "apply", beforeApply)
		storedStateWatchBarrier(f.t, f.ctx, f.cluster, schemas, f.schema(r.name))
		storedStateWatchBarrier(f.t, f.ctx, f.cluster, jobs, publisher)
		closeRunnerPodBoundary(f.t, f.ctx, f.cluster, pods, r.engine, f.in.TestNamespace)
		f.check(runnerProtocolNoReplay(runnerJobHistory(jobs), runnerPodHistory(pods), "PtahSchema", r.name, f.in.TestNamespace, current.UID,
			[]runnerProtocolApply{r.refused, {freshJob, freshPod}}), "account for the refused attempt and one fresh authorized Apply")
		f.assertColumn(r.engine, r.database, "fault_token", 1)
		if f.query(r.engine, r.database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='runner-control' AND note='preserve-this-row'") != "1" {
			f.fatalf("the restored runner did not preserve its populated schema database")
		}
		f.logf("PASS %s unsupported runner after schema approval: exact protocol refusal, no unauthorized SQL or replay; protocol 1 restored and fresh approval applied once", r.engine)
	}
	closeRunnerWatches(f.t, []recorder{jobs, pods, schemas}, f.scan)
	for _, r := range rows {
		f.check(runnerProtocolNoReplay(runnerJobHistory(jobs), runnerPodHistory(pods), "PtahSchema", r.name, f.in.TestNamespace, r.before.UID,
			[]runnerProtocolApply{r.refused, r.fresh}), "close the complete schema runner history without replacement or replay")
	}
	f.auditRuntimeCredentials()
}

func runnerJobHistory(r *watchRecorder[*batchv1.Job]) []batchv1.Job {
	var objects []batchv1.Job
	for _, event := range r.snapshot() {
		objects = append(objects, *event.Object)
	}
	return objects
}

func runnerPodHistory(r *watchRecorder[*corev1.Pod]) []corev1.Pod {
	var objects []corev1.Pod
	for _, event := range r.snapshot() {
		objects = append(objects, *event.Object)
	}
	return objects
}

func closeRunnerPodBoundary(t *testing.T, ctx context.Context, cluster *harness.Cluster, pods *watchRecorder[*corev1.Pod], engine, namespace string) {
	t.Helper()
	service := pgService
	if engine == "mysql" {
		service = mysqlService
	}
	databasePods := &corev1.PodList{}
	storedStateCheck(t, cluster.Client.List(ctx, databasePods, client.InNamespace(namespace), client.MatchingLabels{"app.kubernetes.io/name": service}), "read the runner proof's database Pod sentinel")
	sentinel, err := migrationExecutorPodBarrierSource(databasePods.Items, namespace, service)
	storedStateCheck(t, err, "bind the runner proof to its unmanaged live database sentinel")
	storedStateWatchBarrier(t, ctx, cluster, pods, sentinel)
}

func (m *migrationRun) unsupportedRunnerProtocol() {
	m.t.Helper()
	name, database := "e2e-runner-protocol-"+m.engine.name, "ptah_e2e_runner_protocol"
	secret := name + "-db"
	watcher, err := client.NewWithWatch(m.cluster.Config, client.Options{Scheme: m.cluster.Scheme})
	m.check(err, "open the unsupported migration runner's direct API watches")
	jobs := newStoredStateRecorder[*batchv1.Job](m.t, m.ctx, watcher, "runner-migration-jobs", m.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](m.t, m.ctx, watcher, "runner-migration-pods", m.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	migrations := newStoredStateRecorder[*ptahv1alpha1.PtahMigration](m.t, m.ctx, watcher, "runner-migrations", m.in.TestNamespace, func() client.ObjectList { return &ptahv1alpha1.PtahMigrationList{} })
	replacement, publisher := publishRunnerProtocolVariant(m.t, m.ctx, m.cluster, m.in.TestNamespace, "e2e-migration-runner-variant-"+m.engine.name,
		m.in.FixtureImage, m.in.RunnerImage, m.protect, m.scan)
	change := executionComponentChange{"runner-image", m.in.RunnerImage, replacement}
	deployments := &appsv1.DeploymentList{}
	m.check(m.cluster.Client.List(m.ctx, deployments, client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "find the installed runner transition manager")
	if len(deployments.Items) != 1 {
		m.fatalf("unsupported runner transition requires one installed manager Deployment")
	}
	manager := client.ObjectKeyFromObject(&deployments.Items[0])
	barrier := m.statusBarrier()
	scenario := m.t
	scenario.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := setControllerExecutionComponent(ctx, m.cluster, manager, change.reverse()); err != nil {
			scenario.Errorf("restore the migration's supported runner image: %v", err)
		}
	})
	// The shared tag has been deliberately overwritten by earlier refusal
	// rows. This decision owns and pins its pristine selected sequence.
	reference := m.reference("-runner-protocol")
	digest := m.publish("runner-protocol", m.fixtureDir(""), reference)
	reference = strings.TrimSuffix(reference, ":stable") + "@" + digest
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
	wait := func(description string, match func(*ptahv1alpha1.PtahMigration) bool) *ptahv1alpha1.PtahMigration {
		return m.waitForMigration(name, description, migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
			m.captureMigrationSQLInventory(name, inventory)
			return match(resource)
		})
	}
	document := m.migrationDocument(migrationSpec{name: name, secret: secret, reference: reference,
		coordinationKey: "e2e/runner-protocol/" + m.engine.name, apply: "OnApproval", interval: "1h"})
	document["spec"].(map[string]any)["execution"].(map[string]any)["activeDeadlineSeconds"] = int64(120)
	m.mustCreate(document)
	before := wait("the supported migration runner's original approval gate", func(resource *ptahv1alpha1.PtahMigration) bool {
		return storedStateReady(resource, m.stateVersion) == nil
	})
	old := m.planOf(before.Status.Plan.Name)
	m.check(runningMigrationApprovalPlan(before, old, reference, digest, []int64{1, 2, 3}), "bind the runner proof to its own exact sequence")
	initialHistory := m.executorHistoryControl(before, old, inventory)
	m.check(barrier.pause(m.ctx), "hold the original admitted runner decision before dispatch")
	m.check(m.approve(name+"-old", name, old.Name, string(old.UID), old.Spec.Fingerprint), "admit the original supported runner decision")
	m.assertNoNewApplyJob(nil, "before installing the incompatible runner", name)
	rolloutExecutionManagers(m.t, m.ctx, m.cluster, manager, change, m.scan)
	m.check(barrier.resume(m.ctx), "resume the admitted migration under the incompatible runner")
	refused := waitRunnerApply(m.t, m.ctx, m.cluster, m.in.TestNamespace, labelMigration, name, m.scan)
	m.check(runnerProtocolApplyInputs(refused.job, "PtahMigration", before, before.Status.ExecutionBinding, replacement, old.Spec.Fingerprint, m.controller),
		"retain protocol 1 and the exact approved owner in the refused Job")
	m.check(migrationExecutorApplyInputs(refused.job, old), "retain the approved target, history and checksum sequence before the protocol refusal")
	assertRunnerGuardLog(m.t, m.ctx, m.cluster, refused, m.scan)
	unknown := wait("the exact refused migration run recorded as Unknown", func(resource *ptahv1alpha1.PtahMigration) bool {
		return resource.Status.UnresolvedRun != nil && resource.Status.UnresolvedRun.JobUID == refused.job.UID &&
			resource.Status.UnresolvedRun.OperationID == refused.job.Annotations[annotationOperationID] && resource.Status.UnresolvedRun.Outcome == ptahv1alpha1.MigrationRunOutcomeUnknown &&
			resource.Status.LastRun != nil && resource.Status.LastRun.JobUID == refused.job.UID && resource.Status.LastRun.FinishedAt != nil
	})
	oldApproval := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-old", oldApproval), "retain the consumed incompatible-runner approval")
	if oldApproval.Spec.PlanRef.UID != old.UID || oldApproval.Spec.MigrationRef.UID != before.UID || !conditionStatus(oldApproval.Status.Conditions, "Consumed", "True") {
		m.fatalf("the refused runner did not consume its exact admitted decision at dispatch")
	}
	rolloutExecutionManagers(m.t, m.ctx, m.cluster, manager, change.reverse(), m.scan)
	current := wait("a fresh supported History while the exact Unknown latch remains", func(resource *ptahv1alpha1.PtahMigration) bool {
		return resource.Status.ActiveOperation == nil && resource.Status.History != nil &&
			resource.Status.History.ObservedAt.After(unknown.Status.LastRun.FinishedAt.Time) && resource.Status.History.Fingerprint == old.Spec.HistoryFingerprint &&
			resource.Status.UnresolvedRun != nil && resource.Status.UnresolvedRun.OperationID == refused.job.Annotations[annotationOperationID]
	})
	if current.UID != before.UID || current.Generation != before.Generation || !equality.Semantic.DeepEqual(current.Spec, before.Spec) ||
		!equality.Semantic.DeepEqual(current.Status.ExecutionBinding, before.Status.ExecutionBinding) {
		m.fatalf("runner restoration changed the migration's work or supported execution binding")
	}
	freshHistory := m.runnerRecoveryHistoryControl(current, old, inventory, unknown.Status.LastRun.FinishedAt.Time)
	m.captureMigrationSQLInventory(name, inventory)
	clients, refusedJobs, err := runnerRefusalSQLClients(m.t, m.ctx, m.cluster, current, "PtahMigration", current.Status.ExecutionBinding,
		m.controller, replacement, inventory.jobs, inventory.pods, m.scan)
	m.check(err, "attribute the refused guard and both actual History controls")
	acceptsActor := func(actor operationSQLClient) bool {
		return actor.operation == "history" && !refusedJobs[types.UID(actor.jobUID)]
	}
	policy := migrationRefusalSQL{}
	var counts map[string]int
	if m.engine.name == "postgresql" {
		audit.snapshot()
		if len(pgBefore) == 0 || !bytes.HasPrefix(audit.pgPrefix, pgBefore) {
			m.fatalf("unsupported runner proof lost its complete PostgreSQL journal")
		}
		lifetimes, lifetimeErr := operationSQLLifetimes(clients, inventory.pods)
		m.check(lifetimeErr, "retain exact runner-refusal PostgreSQL client lifetimes")
		counts, err = postgresStatementRefusalSQL(audit.pgPrefix[len(pgBefore):], database, clients, acceptsActor, policy.postgres, postgresMigrationHarnessRead, lifetimes)
	} else {
		counts, err = mysqlStatementRefusalSQL(mysqlBefore, audit.mysqlStatementSnapshot(), database, user, clients, true, acceptsActor, policy.mysql)
	}
	m.check(err, "refuse every SQL statement from the incompatible runner and every unauthorized write")
	m.check(migrationExecutorSQLControls(clients, counts, initialHistory, freshHistory), "receive native SQL from both exact supported History Jobs")
	m.reportMigrationRefusalSQL(current, clients, counts)
	audit.close()
	m.assertDatabaseUnmigrated(name, database)
	storedStateWatchBarrier(m.t, m.ctx, m.cluster, migrations, current)
	storedStateWatchBarrier(m.t, m.ctx, m.cluster, jobs, publisher)
	closeRunnerPodBoundary(m.t, m.ctx, m.cluster, pods, m.engine.name, m.in.TestNamespace)
	m.check(runnerProtocolNoReplay(runnerJobHistory(jobs), runnerPodHistory(pods), "PtahMigration", name, m.in.TestNamespace, current.UID,
		[]runnerProtocolApply{refused}), "hold the consumed decision without replay after runner restoration")
	// History still has pending work. The database was inspected above; only
	// a real admitted acknowledgment may account for this Unknown run.
	m.acknowledgeUnresolvedRun(name, refused.job.Annotations[annotationOperationID])
	ready := wait("the acknowledged migration to require a fresh approval", func(resource *ptahv1alpha1.PtahMigration) bool {
		return storedStateReady(resource, m.stateVersion) == nil
	})
	plan := m.planOf(ready.Status.Plan.Name)
	m.check(runningMigrationApprovalPlan(ready, plan, reference, digest, []int64{1, 2, 3}), "retain the exact untouched sequence for fresh authorization")
	beforeApply := audit.snapshot()
	m.check(m.approve(name+"-current", name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "authorize a fresh supported-runner migration Apply")
	converged := m.waitForGenerationInSync(name)
	run := converged.Status.LastRun
	if run == nil || run.JobUID == refused.job.UID || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied || !slices.Equal(run.AppliedVersions, []int64{1, 2, 3}) {
		m.fatalf("the fresh supported runner did not apply the exact three-version sequence")
	}
	freshJob := &batchv1.Job{}
	m.check(m.get(run.JobName, freshJob), "read the restored runner's actual Apply")
	freshPod := audit.terminalPod(map[string]string{"job-name": run.JobName}, string(run.JobUID))
	m.check(runnerProtocolApplyInputs(freshJob, "PtahMigration", ready, ready.Status.ExecutionBinding, change.original, plan.Spec.Fingerprint, m.controller), "bind the fresh supported runner Job")
	m.check(migrationExecutorApplyInputs(freshJob, plan), "execute the exact fresh approved migration selection")
	logs, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, freshPod.Name, "ptah")
	m.check(err, "read the restored runner's exact successful result")
	m.scan(logs, "the complete supported migration Apply result")
	result, err := resultframe.Parse(logs, runner.OperationMigrationApply, freshJob.Annotations[annotationOperationID])
	m.check(err, "parse the actual restored runner result under protocol 1")
	if result.Error != nil || result.ChildExitCode != 0 || result.Truncation != nil || result.MigrationRun == nil || result.MigrationRun.ContractVersion != 1 ||
		result.MigrationRun.Outcome != "applied" || !slices.Equal(result.MigrationRun.Applied, []int64{1, 2, 3}) || result.MigrationRun.Error != "" {
		m.fatalf("the restored runner did not return a complete successful native migration result")
	}
	audit.assertRecords(beforeApply, audit.snapshot(), freshPod, true)
	audit.close()
	freshApproval := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-current", freshApproval), "read the consumed fresh supported-runner decision")
	if freshApproval.UID == oldApproval.UID || freshApproval.Spec.PlanRef.UID != plan.UID || !conditionStatus(freshApproval.Status.Conditions, "Consumed", "True") {
		m.fatalf("the supported runner did not consume a distinct fresh approval")
	}
	storedStateWatchBarrier(m.t, m.ctx, m.cluster, migrations, converged)
	storedStateWatchBarrier(m.t, m.ctx, m.cluster, jobs, publisher)
	closeRunnerPodBoundary(m.t, m.ctx, m.cluster, pods, m.engine.name, m.in.TestNamespace)
	closeRunnerWatches(m.t, []recorder{jobs, pods, migrations}, m.scan)
	m.check(runnerProtocolNoReplay(runnerJobHistory(jobs), runnerPodHistory(pods), "PtahMigration", name, m.in.TestNamespace, before.UID,
		[]runnerProtocolApply{refused, {freshJob, freshPod}}), "account for exactly the refused workload and one new authorized successful Apply")
	if m.query(restoreRevisionsQuery(m.engine.name), database) != "1,2,3" || m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id=1", database) != "blue" {
		m.fatalf("the freshly authorized supported runner lost its native revisions, rows or schema effect")
	}
	m.logf("PASS %s unsupported runner after migration approval: exact pre-fetch refusal, no unauthorized SQL or replay; Unknown explicitly acknowledged; protocol 1 restored and fresh decision applied once", m.engine.kind)
}

func (m *migrationRun) runnerRecoveryHistoryControl(resource *ptahv1alpha1.PtahMigration, plan *ptahv1alpha1.PtahMigrationPlan,
	inventory *migrationSQLInventory, after time.Time,
) operationSQLClient {
	m.t.Helper()
	var selected *batchv1.Job
	for _, job := range inventory.jobs {
		if job.Labels[labelOperation] == "history" && conditionTrue(job.Status.Conditions, batchv1.JobComplete) && job.CreationTimestamp.After(after) &&
			len(job.Spec.Template.Spec.InitContainers) > 0 && job.Spec.Template.Spec.InitContainers[0].Image == m.in.RunnerImage &&
			(selected == nil || job.CreationTimestamp.After(selected.CreationTimestamp.Time)) {
			selected = job.DeepCopy()
		}
	}
	if selected == nil || selected.Annotations[annotationBindingID] != plan.Spec.ExecutionBindingID || !jobUsesExecutor(selected, plan.Spec.ExecutorImage) {
		m.fatalf("supported runner recovery produced no new exact History after the refused run")
	}
	var pod *corev1.Pod
	for _, candidate := range inventory.pods {
		if ownedExactlyOnce(candidate.OwnerReferences, "batch/v1", "Job", selected.Name, selected.UID) {
			if pod != nil {
				m.fatalf("supported recovery History replayed its Pod")
			}
			pod = candidate.DeepCopy()
		}
	}
	if pod == nil || !resultTransportPod(pod) || pod.Annotations[annotationOperationID] != selected.Annotations[annotationOperationID] {
		m.fatalf("supported recovery History lost its exact result transport")
	}
	logs, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, pod.Name, "ptah")
	m.check(err, "read the actual supported recovery History result")
	m.scan(logs, "the complete supported recovery History result")
	result, err := resultframe.Parse(logs, runner.OperationMigrationHistory, selected.Annotations[annotationOperationID])
	m.check(err, "parse the supported recovery History under protocol 1")
	m.check(migrationHistoryMatchesExecutorPlan(result, plan), "bind the actual recovery reading to the untouched target and pristine selected sequence")
	return operationSQLClient{resourceUID: string(resource.UID), jobUID: string(selected.UID), podUID: string(pod.UID), operation: "history"}
}
