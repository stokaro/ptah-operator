//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/resultframe"
)

func migrationExecutorRecorder[T client.Object](m *migrationRun, watcher client.WithWatch, name, namespace string,
	newList func() client.ObjectList,
) *watchRecorder[T] {
	m.t.Helper()
	list := newList()
	m.check(watcher.List(m.ctx, list, client.InNamespace(namespace)), "list the migration executor %s watch boundary", name)
	if list.GetResourceVersion() == "" {
		m.fatalf("the migration executor %s list has no resourceVersion", name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &watchRecorder[T]{name: "migration-executor-" + name, namespace: namespace, newList: newList,
		watcher: watcher, ctx: ctx, cancel: cancel, quiet: true, done: make(chan struct{})}
	go r.run(list.GetResourceVersion())
	m.t.Cleanup(r.abort)
	return r
}

func migrationExecutorWatchBarrier[T client.Object](m *migrationRun, r *watchRecorder[T], object T) {
	m.t.Helper()
	m.check(m.cluster.Client.Get(m.ctx, client.ObjectKeyFromObject(object), object), "read the %s watch barrier", r.name)
	m.check(m.mergePatch(object, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		"operator.ptah.run/e2e-executor-watch": fmt.Sprint(time.Now().UnixNano()),
	}}}), "write the %s watch barrier", r.name)
	version, uid := object.GetResourceVersion(), object.GetUID()
	if version == "" || uid == "" {
		m.fatalf("the %s watch barrier has no API identity", r.name)
	}
	m.poll("the "+r.name+" exact watch barrier", time.Second, func() bool {
		m.check(r.alive(), "the %s recorder stopped", r.name)
		return slices.ContainsFunc(r.snapshot(), func(event watchEvent[T]) bool {
			return event.Object.GetUID() == uid && event.Object.GetResourceVersion() == version
		})
	})
}

// Hold the real DDL on a table created by the first authorized migration. A
// unique server-side marker is acquired only after the table lock is held.
func (m *migrationRun) runningMigrationTableBarrier(database string) func() {
	m.t.Helper()
	token := "e2e_migration_executor_" + m.engine.name
	command := []string{"-n", m.in.TestNamespace, "exec", "deployment/" + m.engine.service, "--", "sh", "-ec"}
	ready := "SELECT pid::text FROM pg_stat_activity WHERE datname='" + database + "' AND application_name='" + token + "' AND wait_event='PgSleep'"
	cleanupQuery := "SELECT pid::text FROM pg_stat_activity WHERE datname='" + database + "' AND application_name='" + token + "'"
	if m.engine.name == "postgresql" {
		command = append(command, `PGAPPNAME="$2" PGPASSWORD="$POSTGRES_PASSWORD" psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -c 'BEGIN; LOCK TABLE e2e_migration_widgets IN ACCESS SHARE MODE; SELECT pg_sleep(600); ROLLBACK'`, "sh", database, token)
	} else {
		command = append(command, `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"`, "sh", database,
			"SELECT GET_LOCK('"+token+"_session', 0); LOCK TABLES e2e_migration_widgets READ; SELECT GET_LOCK('"+token+"', 0); DO SLEEP(600); UNLOCK TABLES")
		ready = "SELECT IS_USED_LOCK('" + token + "')"
		cleanupQuery = "SELECT IS_USED_LOCK('" + token + "_session')"
	}
	background := startKubectlBackground(m.t, m.cluster.Kubeconfig, command...)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		defer background.stop()
		output, err := databaseSQL(ctx, m.cluster, m.in.TestNamespace, m.engine, database, cleanupQuery)
		if err != nil {
			m.t.Errorf("read the migration table barrier during cleanup: %v", err)
			return
		}
		pid := trimmedSQL(output)
		if decimalCount.MatchString(pid) && pid != "0" {
			statement := "SELECT pg_terminate_backend(" + pid + ")"
			if m.engine.name == "mysql" {
				statement = "KILL " + pid
			}
			_, err = databaseSQL(ctx, m.cluster, m.in.TestNamespace, m.engine, database, statement)
			if err != nil {
				m.t.Errorf("release the exact migration table barrier: %v", err)
			}
		}
		background.stop()
		m.scan(background.output.Bytes(), "migration table barrier output")
	}
	m.t.Cleanup(release)
	m.poll("the migration table barrier's server-side ready marker", time.Second, func() bool {
		if background.exited() {
			m.scan(background.output.Bytes(), "early migration table barrier output")
			m.fatalf("the migration table barrier exited before it held its table")
		}
		pid := m.query(ready, database)
		return decimalCount.MatchString(pid) && pid != "0"
	})
	return release
}

func (m *migrationRun) runningMigrationBackend(database string) string {
	statement := "SELECT DISTINCT a.pid::text || '/' || a.client_addr::text FROM pg_locks held JOIN pg_stat_activity a ON a.pid=held.pid JOIN pg_locks waiting ON waiting.pid=a.pid WHERE held.locktype='advisory' AND held.granted AND NOT waiting.granted AND waiting.locktype='relation' AND waiting.relation='e2e_migration_widgets'::regclass AND waiting.mode='AccessExclusiveLock' AND a.datname='" + database + "'"
	if m.engine.name == "mysql" {
		statement = "SELECT CONCAT(ID, '/', SUBSTRING_INDEX(HOST, ':', 1)) FROM information_schema.processlist WHERE ID=IS_USED_LOCK('ptah_migrate') AND DB='" + database + "' AND STATE LIKE '%metadata lock%'"
	}
	output, err := m.sqlStatement(database, statement)
	m.check(err, "read the advisory-lock holder blocked in the authorized migration DDL")
	return trimmedSQL(output)
}

func (m *migrationRun) runningExecutorImageChange() {
	m.t.Helper()
	name, database := "e2e-running-executor-"+m.engine.name, "ptah_e2e_running_executor"
	secret, seedReference := name+"-db", m.reference("-running-executor-seed")
	replacement, original := m.executorVariant(), m.in.ExecutorImage
	deployments := &appsv1.DeploymentList{}
	m.check(m.cluster.Client.List(m.ctx, deployments, client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "find the running migration executor's manager")
	if len(deployments.Items) != 1 {
		m.fatalf("the running migration transition needs one manager Deployment")
	}
	manager := client.ObjectKeyFromObject(&deployments.Items[0])
	scenario := m.t
	scenario.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := setControllerExecutor(ctx, m.cluster, manager.Namespace, manager.Name, replacement, original); err != nil {
			scenario.Errorf("restore the running migration executor configuration: %v", err)
		}
	})
	m.isolatedDatabase(database, secret)
	seed, err := os.MkdirTemp(m.workDir, "migration-executor-seed-")
	m.check(err, "create the first-migration publication directory")
	m.t.Cleanup(func() { _ = os.RemoveAll(seed) })
	for _, file := range []string{"0000000001_create_widgets.up.sql", "0000000001_create_widgets.down.sql"} {
		content, err := os.ReadFile(filepath.Join(m.fixtureDir(""), file))
		m.check(err, "read the exact seed migration %s", file)
		m.check(os.WriteFile(filepath.Join(seed, file), content, 0o600), "stage the seed migration %s", file)
	}
	m.publish("running-executor-seed", seed, seedReference)
	m.mustCreate(m.migrationDocument(migrationSpec{name: name, secret: secret, reference: seedReference,
		coordinationKey: "e2e/running-executor/" + m.engine.name, apply: "OnApproval", interval: "1h", lockTimeout: "4m"}))
	waitPlan := func() *ptahv1alpha1.PtahMigrationPlan {
		resource := m.waitForMigration(name, "a matching running-executor approval gate", time.Second, func(resource *ptahv1alpha1.PtahMigration) bool {
			return resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && resource.Status.Plan != nil && resource.Status.ActiveOperation == nil
		})
		return m.planOf(resource.Status.Plan.Name)
	}
	seedPlan := waitPlan()
	if !slices.Equal(plannedMigrationVersions(seedPlan), []int64{1}) {
		m.fatalf("the seed plan did not select just migration 1")
	}
	m.check(m.approve(name+"-seed", name, seedPlan.Name, string(seedPlan.UID), seedPlan.Spec.Fingerprint), "authorize the populated table fixture")
	m.waitForGenerationInSync(name)
	if m.query(restoreRevisionsQuery(m.engine.name), database) != "1" || m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" {
		m.fatalf("the seed Apply did not create its table, rows and version-1 history")
	}
	watcher, err := client.NewWithWatch(m.cluster.Config, client.Options{Scheme: m.cluster.Scheme})
	m.check(err, "open direct API watches for the running migration")
	migrations := migrationExecutorRecorder[*ptahv1alpha1.PtahMigration](m, watcher, "resources", m.in.TestNamespace, func() client.ObjectList { return &ptahv1alpha1.PtahMigrationList{} })
	jobs := migrationExecutorRecorder[*batchv1.Job](m, watcher, "jobs", m.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := migrationExecutorRecorder[*corev1.Pod](m, watcher, "pods", m.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	leases := migrationExecutorRecorder[*coordinationv1.Lease](m, watcher, "leases", manager.Namespace, func() client.ObjectList { return &coordinationv1.LeaseList{} })
	recorders := []recorder{migrations, jobs, pods, leases}
	m.patchMigration(name, map[string]any{"spec": map[string]any{"artifact": map[string]any{"ociRef": m.reference("")}, "interval": "15s"}})
	plan := waitPlan()
	if !slices.Equal(plannedMigrationVersions(plan), []int64{2, 3}) {
		m.fatalf("the controlled Apply did not select the exact remaining migrations")
	}
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: m.engine.name}
	sqlBefore := audit.snapshot()
	releaseTable := m.runningMigrationTableBarrier(database)
	m.check(m.approve(name+"-running", name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "authorize the original executor's pending sequence")
	jobUID, _ := m.waitForStopRowApply(name)
	backend := ""
	m.poll("the original migration executor inside its blocked DDL", time.Second, func() bool {
		backend = m.runningMigrationBackend(database)
		return backend != ""
	})
	pod := m.readStopRowPod(jobUID)
	pid, address, ok := strings.Cut(backend, "/")
	if !ok || !decimalCount.MatchString(pid) || pid == "0" || address == "" || address != pod.Status.PodIP || pod.Status.Phase != corev1.PodRunning {
		m.fatalf("the database did not identify the exact running migration Pod's PID and address")
	}
	before := m.migration(name)
	if before.Status.ActiveOperation == nil || string(before.Status.ActiveOperation.JobUID) != jobUID {
		m.fatalf("the running migration did not retain its exact dispatched claim")
	}
	migrationExecutorWatchBarrier(m, migrations, before)
	operation := before.Status.ActiveOperation
	job := &batchv1.Job{}
	m.check(m.get(operation.JobName, job), "read the original migration Apply Job")
	if job.UID != operation.JobUID || !jobUsesExecutor(job, original) || job.Annotations[annotationBindingID] != before.Status.ExecutionBinding.Epoch ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahMigration", name, before.UID) ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
		m.fatalf("the original workload is not the migration's authorized image, epoch and ownership chain")
	}
	m.check(migrationExecutorApplyInputs(job, plan), "bind the actual original Apply inputs to its approved plan")
	leaseList := &coordinationv1.LeaseList{}
	m.check(m.cluster.Client.List(m.ctx, leaseList, client.InNamespace(manager.Namespace)), "read the original migration's realm")
	heldLeases := heldLeasesAtEpoch(leaseList.Items, operation.LeaseEpoch)
	if len(heldLeases) != 1 {
		m.fatalf("the original running migration has no unique held realm")
	}
	lease := heldLeases[0]
	rolloutExecutorManagers(m.t, m.ctx, m.cluster, manager, original, replacement, m.scan)
	held := m.waitForMigration(name, "Unknown with the original running claim retained", time.Second, func(resource *ptahv1alpha1.PtahMigration) bool {
		for _, recorder := range recorders {
			m.check(recorder.alive(), "the running migration watch stopped")
		}
		return migrationExecutorHeld(before, resource) == nil
	})
	migrationExecutorWatchBarrier(m, migrations, held)
	m.check(migrationExecutorHeldWindow(migrations.snapshot(), before, held.ResourceVersion), "protect the complete running-migration window")
	heldLease := &coordinationv1.Lease{}
	heldLease.Name, heldLease.Namespace = lease.name, manager.Namespace
	migrationExecutorWatchBarrier(m, leases, heldLease)
	if !leaseIs(heldLease, lease) || !leaseHeldWithoutRelease(leases.snapshot(), lease.uid, lease.holder, lease.epoch) ||
		m.runningMigrationBackend(database) != backend || m.widgetColumnCount("color", database) != "0" {
		m.fatalf("the original SQL process or realm changed before its table barrier was released")
	}
	liveJob, livePod := &batchv1.Job{}, &corev1.Pod{}
	m.check(m.get(job.Name, liveJob), "read the still-running original Apply Job")
	m.check(m.get(pod.Name, livePod), "read the still-running original Apply Pod")
	if liveJob.UID != job.UID || !jobUsesExecutor(liveJob, original) || livePod.UID != pod.UID || livePod.Status.Phase != corev1.PodRunning {
		m.fatalf("the manager rollout replaced or terminated the old authorized executor")
	}
	releaseTable()
	m.poll("the original executor's complete successful result", time.Second, func() bool {
		m.check(m.get(job.Name, liveJob), "read the original terminal Apply Job")
		m.check(m.get(pod.Name, livePod), "read the original terminal Apply Pod")
		if liveJob.UID != job.UID || livePod.UID != pod.UID {
			m.fatalf("the original migration workload was replaced before its result was retained")
		}
		if !jobComplete(liveJob) || !resultTransportPod(livePod) {
			return false
		}
		logs, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, pod.Name, "ptah")
		m.check(err, "read the exact old executor's result")
		m.scan(logs, "the old migration executor's result")
		result, err := resultframe.Parse(logs, runner.OperationMigrationApply, operation.ID)
		if resultframe.StillArriving(err) {
			return false
		}
		m.check(err, "parse the old migration executor's complete result")
		if result.ChildExitCode != 0 || result.Error != nil || result.CoordinationDigest != operation.CoordinationDigest || result.TargetIdentityDigest != plan.Spec.TargetIdentityDigest ||
			result.MigrationRun == nil || result.MigrationRun.Outcome != dataplane.MigrationOutcomeApplied ||
			!slices.Equal(result.MigrationRun.Planned, []int64{2, 3}) || !slices.Equal(result.MigrationRun.Applied, []int64{2, 3}) {
			m.fatalf("the old executor did not complete its exact authorized sequence on its planned database")
		}
		return true
	})
	audit.assertRecords(sqlBefore, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": job.Name}, jobUID), true)
	audit.close()
	current := m.waitForMigration(name, "fresh History accounting for Unknown under the replacement executor", time.Second, func(resource *ptahv1alpha1.PtahMigration) bool {
		return migrationExecutorHistoryRecovery(before, held, resource, replacement) == nil
	})
	// Read the successful new-epoch History frame itself, not only the status
	// the manager derived from it. Repeated diagnostics may produce more than
	// one frame; at least one must equal the final retained history.
	m.poll("the new executor's exact converged History result", time.Second, func() bool {
		for _, event := range jobs.snapshot() {
			historyJob := event.Object
			if historyJob.Labels[labelMigration] != name || historyJob.Labels[labelOperation] != "history" || !jobComplete(historyJob) ||
				historyJob.Annotations[annotationBindingID] != current.Status.ExecutionBinding.Epoch || !jobUsesExecutor(historyJob, replacement) {
				continue
			}
			historyPod := audit.terminalPod(map[string]string{"job-name": historyJob.Name}, string(historyJob.UID))
			logs, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, historyPod.Name, "ptah")
			m.check(err, "read the replacement executor's exact History frame")
			m.scan(logs, "replacement migration History result")
			result, err := resultframe.Parse(logs, runner.OperationMigrationHistory, historyJob.Annotations[annotationOperationID])
			if resultframe.StillArriving(err) {
				continue
			}
			m.check(err, "parse the replacement migration History frame")
			if result.ChildExitCode != 0 || result.Error != nil || result.CoordinationDigest != operation.CoordinationDigest ||
				result.TargetIdentityDigest != current.Status.History.TargetIdentityDigest || result.MigrationHistory == nil {
				m.fatalf("the replacement executor's History lost its original database binding")
			}
			fingerprint, err := migrationplan.HistoryFingerprint(*result.MigrationHistory)
			m.check(err, "fingerprint the replacement executor's actual History")
			if fingerprint == current.Status.History.Fingerprint && result.MigrationHistory.CurrentVersion == 3 && len(result.MigrationHistory.Pending()) == 0 {
				return true
			}
		}
		return false
	})
	if m.query(restoreRevisionsQuery(m.engine.name), database) != "1,2,3" || m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id=1", database) != "blue" {
		m.fatalf("the original Apply and fresh History did not match the actual schema, rows and revisions")
	}
	rolloutExecutorManagers(m.t, m.ctx, m.cluster, manager, replacement, original, m.scan)
	migrationExecutorWatchBarrier(m, migrations, current)
	migrationExecutorWatchBarrier(m, jobs, liveJob)
	migrationExecutorWatchBarrier(m, pods, livePod)
	migrationExecutorWatchBarrier(m, leases, heldLease)
	for _, r := range recorders {
		r.requestStop()
	}
	deadline := time.Now().Add(35 * time.Second)
	for _, r := range recorders {
		m.check(r.await(time.Until(deadline)), "close the migration executor %s watch at natural EOF", r.stem())
		history, count, err := r.history()
		m.check(err, "encode the migration executor %s closed history", r.stem())
		if count == 0 {
			m.fatalf("the migration executor %s history recorded nothing", r.stem())
		}
		m.scan(history, r.stem()+" closed history")
	}
	if !migrationExecutorNoReplay(jobs.snapshot(), pods.snapshot(), name, jobUID, string(pod.UID)) {
		m.fatalf("the closed migration executor history contains a second Apply or overlapping work")
	}
	m.check(leaseHeldUntilRelease(leases.snapshot(), lease), "retain the original migration realm through its safe release")
	m.logf("PASS %s running Apply executor change: original Job=%s Pod=%s retained; fresh History settled Unknown without replay", m.engine.kind, jobUID, pod.UID)
}

func plannedMigrationVersions(plan *ptahv1alpha1.PtahMigrationPlan) []int64 {
	var versions []int64
	for _, migration := range plan.Spec.Migrations {
		versions = append(versions, migration.Version)
	}
	return versions
}
