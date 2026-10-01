//go:build e2e

package e2e

import (
	"context"
	"maps"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

type executorRunningSchema struct {
	engine, name, database, barrier, backend string
	before                                   *ptahv1alpha1.PtahSchema
	run                                      applyRun
	lease                                    leaseIdentity
	audit                                    *databaseSQLAudit
	sqlBefore                                sqlAuditCounts
	observeBefore, planBefore                checkpoint
}

func (f *faultRun) executorApplyBackend(engine, database string) string {
	f.t.Helper()
	if engine == "postgresql" {
		f.assertPGApplyLockWait(database)
		return f.query(engine, database, "SELECT a.pid::text || '/' || host(a.client_addr) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.classid=0 AND l.objid="+strconv.Itoa(pgApplyLockKey)+" AND l.objsubid=1 AND l.granted AND a.datname='"+database+"'")
	}
	f.assertMySQLApplyLockWait(database)
	return f.query(engine, "mysql", "SELECT CONCAT(ID, '/', SUBSTRING_INDEX(HOST, ':', 1)) FROM information_schema.processlist WHERE ID=IS_USED_LOCK('ptah_schema_apply') AND DB='"+database+"' AND STATE LIKE '%metadata lock%'")
}

// Start a separate watch window after the earlier fault histories closed.
// Both original Apply processes remain blocked in their database while every
// manager is replaced with one configured to use the new executor image.
func (previous *faultRun) runningExecutorImageChanges() {
	f := newFaultRun(previous.dataPlane)
	// The earlier fault run created the principal Secret. This independent
	// watch window must rebuild its scanner before any polling or audit.
	f.buildScanner()
	f.pgReference, f.mysqlReference = previous.pgReference, previous.mysqlReference
	f.auditedJobs, f.auditedPods = maps.Clone(previous.auditedJobs), maps.Clone(previous.auditedPods)
	f.fullyAuditedPods = maps.Clone(previous.fullyAuditedPods)
	replacement, original := f.executorVariant("e2e-running-executor-variant"), f.in.ExecutorImage
	scenario := f.t
	scenario.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := setControllerExecutor(ctx, f.cluster, f.in.OperatorNamespace, f.controllerName, replacement, original); err != nil {
			scenario.Errorf("restore executor after the running-Apply proof: %v", err)
		}
	})
	f.startWatches()
	f.startHeartbeat()
	var rows []*executorRunningSchema
	for _, engine := range []string{"postgresql", "mysql"} {
		row := &executorRunningSchema{engine: engine, name: "e2e-running-executor-" + engine,
			database: "e2e_running_executor", barrier: "e2e_running_executor_barrier"}
		secret := row.name + "-db"
		f.createDatabase(engine, row.database, secret)
		f.query(engine, row.database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'running-executor-control', 'preserve-this-row')")
		kind, reference := "PostgreSQL", f.pgReference
		if engine == "mysql" {
			kind, reference = "MySQL", f.mysqlReference
		}
		leases := f.checkpointLeases()
		f.createSchema(faultSchema{name: row.name, engine: kind, secret: secret, reference: reference,
			coordinationKey: "e2e/running-executor/" + engine, activeDeadline: 300, lockTimeout: "4m"})
		f.waitForPlan(row.name)
		row.lease = f.loadNewReleasedLease(leases, "the running-executor schema's initial Plan Lease")
		row.observeBefore, row.planBefore = f.checkpointJobs(row.name, "observe"), f.checkpointJobs(row.name, "plan")
		rows = append(rows, row)
	}
	// Planning is complete before either execution clock starts.
	for _, row := range rows {
		row.audit = &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: row.engine}
		row.sqlBefore = row.audit.snapshot()
		if row.engine == "postgresql" {
			f.startPGBarrier(row.database, row.barrier)
		} else {
			f.startMySQLBarrier(row.database, row.barrier)
		}
		f.createApproval(row.name, row.name+"-approval")
		row.run = f.waitForApplyPod(row.name)
		row.backend = f.executorApplyBackend(row.engine, row.database)
		pod := &corev1.Pod{}
		f.check(f.get(row.run.podName, pod), "read the blocked Apply Pod")
		if !executorBackendMatchesPod(row.backend, pod, types.UID(row.run.podUID)) {
			f.fatalf("the database did not identify the exact original Apply client's PID and Pod address")
		}
		row.lease = f.waitForLeaseReacquisition(row.lease, "the running Apply before its executor change")
		row.before = f.schema(row.name)
		establishBarrier(f, f.schemas, row.before, f.in.TestNamespace, row.name)
		if active := row.before.Status.ActiveOperation; active == nil || string(active.JobUID) != row.run.jobUID || active.ExecutionNotAfter == nil || row.before.Status.Plan == nil {
			f.fatalf("the running Apply lost its immutable claim before the rollout")
		}
		_, err := faultApprovalCommitted(f.unstructuredApproval(row.name+"-approval"), string(row.before.Status.Plan.UID))
		f.check(err, "retain the committed decision while its original Apply is still running")
	}
	f.rolloutExecutor(original, replacement)
	for _, row := range rows {
		f.waitForSchema(row.name, "the original running Apply recorded under the replacement executor", func(resource *ptahv1alpha1.PtahSchema) bool {
			return schemaExecutorRetirement(row.before, resource, types.UID(row.run.podUID), replacement) == nil
		})
		boundary := f.schema(row.name)
		establishBarrier(f, f.schemas, boundary, f.in.TestNamespace, row.name)
		f.check(schemaExecutorHeldWindow(f.schemas.snapshot(), row.before, types.UID(row.run.podUID), replacement, boundary.ResourceVersion), "preserve the running claim through every watched status")
		f.assertLeaseHeldWithoutRelease(row.lease)
		if f.executorApplyBackend(row.engine, row.database) != row.backend {
			f.fatalf("the original database process changed while its executor epoch retired")
		}
		job, pod := &batchv1.Job{}, &corev1.Pod{}
		f.check(f.get(row.run.jobName, job), "read the original running Apply Job")
		f.check(f.get(row.run.podName, pod), "read the original running Apply Pod")
		if string(job.UID) != row.run.jobUID || !jobUsesExecutor(job, original) || string(pod.UID) != row.run.podUID || pod.Status.Phase != corev1.PodRunning {
			f.fatalf("executor change replaced or stopped the controlled Apply before its database barrier was released")
		}
		f.assertColumn(row.engine, row.database, "fault_token", 0)
	}
	// Let the authorized old executables finish. The new manager must prove
	// convergence under its own epoch without executing another Apply.
	f.stopPGBarrier(rows[0].barrier)
	f.releaseMySQLBarrier()
	for _, row := range rows {
		result := f.captureExactJobResult(row.run.jobName, row.run.jobUID, "apply")
		if result.podUID != row.run.podUID || result.result.ChildExitCode != 0 || result.result.Error != nil {
			f.fatalf("the original executor did not complete its authorized Apply")
		}
		row.audit.assertRecords(row.sqlBefore, row.audit.snapshot(), row.audit.terminalPod(map[string]string{"job-name": row.run.jobName}, row.run.jobUID), true)
		row.audit.close()
	}
	// Retain both engines' completed proof transports while polling. Their
	// cleanup TTL must not make an earlier proof disappear during later work.
	captured := map[string]exactResult{}
	captureRecovery := func() {
		for _, row := range rows {
			for operation, before := range map[string]checkpoint{"observe": row.observeBefore, "plan": row.planBefore} {
				for _, event := range f.jobs.snapshot() {
					job := event.Object
					if job == nil || !operationOf(job, row.name, operation) || before.holds(string(job.UID)) || !jobComplete(job) {
						continue
					}
					uid := string(job.UID)
					if _, exists := captured[uid]; !exists {
						captured[uid] = f.captureExactJobResult(job.Name, uid, operation)
					}
				}
			}
		}
	}
	for _, row := range rows {
		active := row.before.Status.ActiveOperation
		horizon := active.ExecutionNotAfter.Add(time.Duration(active.TerminationGracePeriodSeconds) * time.Second)
		for time.Now().Before(horizon) {
			f.maybeAudit()
			current := f.schema(row.name)
			// A read that crosses the horizon may already contain a legitimate
			// diagnostic claim. Its Job timestamp is checked below.
			if !time.Now().Before(horizon) {
				break
			}
			f.check(schemaExecutorRetirement(row.before, current, types.UID(row.run.podUID), replacement), "retain the Apply evidence through its immutable execution horizon")
			f.assertLeaseIdentity(row.lease)
			f.sleep(time.Second)
		}
		// The retired plan cannot declare current convergence. Its proof is
		// followed by Resolve -> Verify -> Observe -> Plan under the new epoch.
		settled := f.waitForSchema(row.name, "fresh source verification and scoped convergence under the replacement executor", func(resource *ptahv1alpha1.PtahSchema) bool {
			captureRecovery()
			return inSyncFor(resource, "ScopedConverged")
		})
		if settled.Status.ExecutionBinding == nil || settled.Status.ExecutionBinding.ExecutorImage != replacement || settled.Status.Applied != nil || settled.Status.Plan != nil {
			f.fatalf("recovery attributed an old executor's Apply to the new epoch")
		}
		f.check(faultApprovalRetiredBy(f.unstructuredApproval(row.name+"-approval"), string(row.before.Status.Plan.UID),
			ptahv1alpha1.ReasonExecutionBindingChanged), "retain the dispatch decision and its executor-change retirement")
		for operation, before := range map[string]checkpoint{"observe": row.observeBefore, "plan": row.planBefore} {
			uids := newAddedUIDs(f.jobs.snapshot(), row.name, operation, before)
			if len(uids) != 2 {
				f.fatalf("expected immutable proof and fresh-source %s Jobs for %s, found %d", operation, row.name, len(uids))
			}
			var jobs []*batchv1.Job
			for _, uid := range uids {
				var job *batchv1.Job
				for _, event := range f.jobs.snapshot() {
					if string(event.Object.UID) == uid {
						job = event.Object
					}
				}
				jobs = append(jobs, job)
				proof, found := captured[uid]
				if !found {
					f.fatalf("the %s recovery result was not retained before collection", operation)
				}
				if operation == "plan" {
					f.check(noChangesPlan(settled, proof.result), "the replacement executor's no-change proof")
				} else {
					dialects := postgresDialects
					if row.engine == "mysql" {
						dialects = []string{"mysql", "mariadb"}
					}
					if proof.result.ObservedDrift {
						f.check(driftedObserveBound(settled, proof.result, dialects...), "bind the replacement executor's drift observation")
					} else {
						f.check(cleanObserve(settled, proof.result, dialects), "bind the replacement executor's clean observation")
					}
				}
			}
			f.check(schemaExecutorRecoveryPair(f.schemas.snapshot(), row.before, jobs, replacement, horizon), "retain both stages of replacement-executor recovery")
		}
		f.assertColumn(row.engine, row.database, "fault_token", 1)
		if f.query(row.engine, row.database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='running-executor-control' AND note='preserve-this-row'") != "1" {
			f.fatalf("the original Apply did not preserve its seeded row")
		}
	}
	f.rolloutExecutor(replacement, original)
	f.jobBarrier()
	establishBarrier(f, f.pods, &corev1.Pod{}, f.in.TestNamespace, f.databasePod())
	f.stopHeartbeat()
	f.stopWatches()
	f.validateAndScanWatches()
	f.recordJobsForParent()
	f.waitForAuditComplete()
	for _, row := range rows {
		if !schemaExecutorNoReplay(f.jobs.snapshot(), f.pods.snapshot(), row.name, row.run.jobUID, row.run.podUID) {
			f.fatalf("the closed running-executor history contains a replacement Apply or overlapping work")
		}
		if err := leaseHeldUntilRelease(f.leases.snapshot(), row.lease); err != nil {
			f.fatalf("the retired Apply lost its uninterrupted Lease: %v", err)
		}
		f.logf("PASS %s running Apply executor change: original Job=%s Pod=%s retained; new epoch proved convergence without replay", row.engine, row.run.jobUID, row.run.podUID)
	}
}
