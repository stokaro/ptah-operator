//go:build e2e

package e2e

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// faultTarget is one schema a fault proof holds, and what the proof pinned of
// it for the scenarios after.
type faultTarget struct {
	schema, database, secret, approval, coordinationKey, barrier string

	// idle is the target Lease the schema's first Plan took and released,
	// and lease the same Lease held again by its Apply.
	idle, lease leaseIdentity
	run         applyRun
	// observeBefore and planBefore checkpoint the Observe and Plan Jobs the
	// schema had before its Apply, so the proof counts only the Apply's.
	observeBefore, planBefore checkpoint
	originalPlanUID           string
	prefaultFingerprint       string
	prefaultObservedAt        time.Time
	// proofObserveUID and proofPlanUID are the Observe and Plan Jobs that
	// proved the Apply, and freshPlanUID the plan an uncertain Apply ended at.
	proofObserveUID, proofPlanUID, freshPlanUID string
	deadlineStartedAt                           string
}

// faultState is what one fault scenario leaves for a later one to hold.
type faultState struct {
	principalSchema, principalPlanUID, principalPlanPodUID string

	pgRestart, pgParallel, mysqlUnknown, mysqlTimeout faultTarget

	aliasA, aliasB string
	aliasBApproval string
	aliasBPodUID   string
	// aliasBefore is each shared-alias schema's Observe and Plan checkpoint.
	aliasBefore map[string][2]checkpoint

	deletedSchema     string
	deletedDatabase   string
	deletedPrint      string
	deletedCheckpoint checkpoint

	manual            faultTarget
	manualPrint       string
	manualJobUID      string
	manualPodUID      string
	manualSQL         *schemaRefusalWindow
	manualSQLControls []operationSQLClient
}

func newFaultTargets() faultState {
	return faultState{
		pgRestart: faultTarget{
			schema: "e2e-fault-pg-restart", database: "e2e_fault_pg_restart", secret: "e2e-fault-pg-restart-db",
			approval: "e2e-fault-pg-restart-approval", coordinationKey: "e2e/fault/pg-restart",
			barrier: "e2e_fault_pg_restart_barrier",
		},
		pgParallel: faultTarget{
			schema: "e2e-fault-pg-parallel", database: "e2e_fault_pg_parallel", secret: "e2e-fault-pg-parallel-db",
			approval: "e2e-fault-pg-parallel-approval", coordinationKey: "e2e/fault/pg-parallel",
			barrier: "e2e_fault_pg_parallel_barrier",
		},
		mysqlUnknown: faultTarget{
			schema: "e2e-fault-my-unknown", database: "e2e_fault_my_unknown", secret: "e2e-fault-my-unknown-db",
			approval: "e2e-fault-my-unknown-approval", coordinationKey: "e2e/fault/mysql-unknown",
			barrier: "e2e_fault_my_unknown_barrier",
		},
		mysqlTimeout: faultTarget{
			schema: "e2e-fault-my-timeout", database: "e2e_fault_my_timeout", secret: "e2e-fault-my-timeout-db",
			approval: "e2e-fault-my-timeout-approval", coordinationKey: "e2e/fault/mysql-timeout",
			barrier: "e2e_fault_my_timeout_barrier",
		},
		manual: faultTarget{
			schema: "e2e-fault-pg-manual", database: "e2e_fault_pg_manual", secret: "e2e-fault-pg-manual-db",
			approval: "e2e-fault-pg-manual-approval", coordinationKey: "e2e/fault/manual",
		},
	}
}

// watches stands the fault injection up: the fixtures it publishes, the
// resourceVersion watches and their heartbeat, the credential-principal
// refusal, and the four schemas that will hold an Apply each, planned and
// checkpointed.
func (f *faultRun) watches() {
	f.t.Helper()
	f.state = newFaultTargets()
	f.createPrincipalSecret()
	f.buildScanner()
	f.logf("starting resourceVersion watches and fault-injection acceptance")
	for _, service := range []string{pgService, mysqlService} {
		f.check(f.get(service, &corev1.Service{}), "read Service %s", service)
	}
	for _, secret := range []string{pgSecret, mysqlSecret, registryAuthSecret, registryPullSecret} {
		f.check(f.get(secret, &corev1.Secret{}), "read Secret %s", secret)
	}
	f.check(f.get(heartbeatSchema, &ptahv1alpha1.PtahSchema{}), "read PtahSchema %s", heartbeatSchema)
	f.check(f.get(heartbeatApproval, &ptahv1alpha1.PtahSchemaApproval{}), "read PtahSchemaApproval %s", heartbeatApproval)
	f.check(f.cluster.WaitForRollout(f.ctx, f.in.OperatorNamespace, f.controllerName, waitTimeout), "wait for the manager rollout")

	f.pgReference = "oci://" + f.registryHost + "/schemas/fault-postgresql:stable"
	f.mysqlReference = "oci://" + f.registryHost + "/schemas/fault-mysql:stable"
	f.publishFaultSchema("postgresql", "postgres", f.pgReference)
	f.publishFaultSchema("mysql", "mysql", f.mysqlReference)
	f.createHeartbeatLease()
	f.startWatches()
	f.startHeartbeat()
	f.auditRuntime()
	f.credentialPrincipalRefusal()

	s := &f.state
	f.createDatabase("postgresql", s.pgRestart.database, s.pgRestart.secret)
	f.createDatabase("postgresql", s.pgParallel.database, s.pgParallel.secret)
	f.createDatabase("mysql", s.mysqlUnknown.database, s.mysqlUnknown.secret)
	f.createDatabase("mysql", s.mysqlTimeout.database, s.mysqlTimeout.secret)

	f.planTarget(&s.pgRestart, faultSchema{engine: "PostgreSQL", reference: f.pgReference},
		"the PostgreSQL restart schema's initial Plan target lock")
	f.planTarget(&s.pgParallel, faultSchema{engine: "PostgreSQL", reference: f.pgReference},
		"the parallel PostgreSQL schema's initial Plan target lock")
	f.planTarget(&s.mysqlUnknown, faultSchema{engine: "MySQL", reference: f.mysqlReference},
		"the MySQL uncertain schema's initial Plan target lock")
	s.mysqlUnknown.prefaultFingerprint = f.fingerprint("mysql", s.mysqlUnknown.database,
		"the complete pre-fault MySQL schema and index state")
	f.planTarget(&s.mysqlTimeout, faultSchema{
		engine: "MySQL", reference: f.mysqlReference,
		failureRetry: "1h", activeDeadline: faultTimeoutDeadlineSeconds, lockTimeout: "30s",
	}, "the Kubernetes-timeout schema's initial Plan target lock")
	s.mysqlTimeout.prefaultFingerprint = f.fingerprint("mysql", s.mysqlTimeout.database,
		"the pre-timeout MySQL schema and index state")

	for _, target := range []*faultTarget{&s.pgRestart, &s.pgParallel} {
		target.observeBefore = f.checkpointOperationWatch(target.schema, "observe", 1)
		target.planBefore = f.checkpointOperationWatch(target.schema, "plan", 1)
	}
	f.assertColumn("postgresql", s.pgRestart.database, "fault_token", 0)
	f.assertColumn("postgresql", s.pgParallel.database, "fault_token", 0)
	f.assertColumn("mysql", s.mysqlUnknown.database, "fault_token", 0)
	unknown := f.schema(s.mysqlUnknown.schema)
	if unknown.Status.Plan == nil || unknown.Status.Plan.UID == "" {
		f.fatalf("uncertain-Apply schema did not persist its original plan UID")
	}
	s.mysqlUnknown.originalPlanUID = string(unknown.Status.Plan.UID)
	if unknown.Status.Target.LastObservedAt == nil {
		f.fatalf("uncertain-Apply schema lacks its pre-fault observation timestamp")
	}
	s.mysqlUnknown.prefaultObservedAt = unknown.Status.Target.LastObservedAt.Time
	s.mysqlUnknown.observeBefore = f.checkpointOperationWatch(s.mysqlUnknown.schema, "observe", 1)
	s.mysqlUnknown.planBefore = f.checkpointOperationWatch(s.mysqlUnknown.schema, "plan", 1)
	timeout := f.schema(s.mysqlTimeout.schema)
	if timeout.Status.Plan == nil || timeout.Status.Plan.UID == "" {
		f.fatalf("Kubernetes-timeout schema did not persist its original plan UID")
	}
	s.mysqlTimeout.originalPlanUID = string(timeout.Status.Plan.UID)
	s.mysqlTimeout.observeBefore = f.checkpointOperationWatch(s.mysqlTimeout.schema, "observe", 1)
	s.mysqlTimeout.planBefore = f.checkpointOperationWatch(s.mysqlTimeout.schema, "plan", 1)
	f.assertColumn("mysql", s.mysqlTimeout.database, "fault_token", 0)
}

// planTarget creates a target's schema, waits for its first plan, and pins
// the target Lease that Plan took and released.
func (f *faultRun) planTarget(target *faultTarget, schema faultSchema, description string) {
	f.t.Helper()
	before := f.checkpointLeases()
	schema.name, schema.secret, schema.coordinationKey = target.schema, target.secret, target.coordinationKey
	f.createSchema(schema)
	f.waitForPlan(target.schema)
	target.idle = f.loadNewReleasedLease(before, description)
}

// credentialPrincipalRefusal publishes an artifact that declares a login role
// with a password and holds the operator to refusing it before any plan
// leaves the Pod: one Plan Job, its result the runner's invalid_plan_output,
// no plan, chunk, projection, approval or Apply, the database untouched, and
// no retry inside the one-hour failure barrier.
func (f *faultRun) credentialPrincipalRefusal() {
	f.t.Helper()
	database, secret := "e2e_fault_pg_principal", "e2e-fault-pg-principal-db"
	schemaName := "e2e-fault-pg-principal"
	reference := "oci://" + f.registryHost + "/schemas/credential-principal:stable"
	roleQuery := "SELECT count(*) FROM pg_roles WHERE rolname='" + f.principalRole + "'"

	f.createDatabase("postgresql", database, secret)
	fingerprint := f.fingerprint("postgresql", database, "the credential-principal test database")
	if f.query("postgresql", database, roleQuery) != "0" {
		f.fatalf("credential-principal fixture role already exists")
	}
	digest := f.publishPrincipalArtifact(reference)
	planBefore := f.checkpointOperationWatch(schemaName, "plan", 0)
	applyBefore := f.checkpointOperationWatch(schemaName, "apply", 0)
	f.createSchema(faultSchema{
		name: schemaName, engine: "PostgreSQL", secret: secret, reference: reference,
		coordinationKey: "e2e/fault/credential-principal", failureRetry: "1h",
	})
	planUID := f.waitForOneNewWatchedJob(schemaName, "plan", planBefore,
		"one fail-closed Plan Job for the credential-bearing principal artifact")
	planJob := f.liveJobName(planUID, "credential-principal Plan Job", operationJobs(schemaName, "plan"))
	captured := f.captureExactJobResult(planJob, planUID, "plan")
	f.state.principalSchema, f.state.principalPlanUID, f.state.principalPlanPodUID = schemaName, planUID, captured.podUID
	f.waitForSchema(schemaName, "the credential-bearing principal Plan to fail closed with a long retry barrier",
		func(schema *ptahv1alpha1.PtahSchema) bool { return principalFailedClosed(schema, time.Now()) })
	schema := f.schema(schemaName)
	if err := principalRefusal(schema, captured.result, digest); err != nil {
		f.fatalf("credential-bearing principal artifact did not reach the exact Plan refusal: %v", err)
	}
	plans := &ptahv1alpha1.PtahSchemaPlanList{}
	f.check(f.list(plans), "list the PtahSchemaPlans")
	for index := range plans.Items {
		if plans.Items[index].Spec.SchemaRef.UID == schema.UID {
			f.fatalf("credential-bearing principal refusal published a PtahSchemaPlan")
		}
	}
	chunks := &ptahv1alpha1.PtahSchemaPlanChunkList{}
	f.check(f.list(chunks, client.MatchingLabels{labelSchema: schemaName}), "list the plan chunks")
	if len(chunks.Items) != 0 {
		f.fatalf("credential-bearing principal refusal published a plan chunk")
	}
	projections := &corev1.ConfigMapList{}
	f.check(f.list(projections, client.MatchingLabels{labelSchema: schemaName}), "list the plan projections")
	if len(projections.Items) != 0 {
		f.fatalf("credential-bearing principal refusal projected a plan into a ConfigMap")
	}
	if f.newWatchedCount(schemaName, "apply", applyBefore) != 0 {
		f.fatalf("credential-bearing principal refusal dispatched an Apply Job")
	}
	if f.newWatchedCount(schemaName, "plan", planBefore) != 1 {
		f.fatalf("credential-bearing principal refusal retried before its one-hour barrier")
	}
	if f.addedJobCount(schemaName, "resolve") != 1 || f.addedJobCount(schemaName, "verify") != 1 ||
		f.addedJobCount(schemaName, "observe") != 1 {
		f.fatalf("credential-bearing principal refusal did not reach Plan through one exact read-only chain")
	}
	approvals := &ptahv1alpha1.PtahSchemaApprovalList{}
	f.check(f.list(approvals), "list the PtahSchemaApprovals")
	for index := range approvals.Items {
		if approvals.Items[index].Spec.SchemaRef.UID == schema.UID {
			f.fatalf("credential-bearing principal refusal created an approval")
		}
	}
	if f.pgFingerprint(database) != fingerprint {
		f.fatalf("credential-bearing principal refusal changed database SQL state")
	}
	if f.query("postgresql", database, roleQuery) != "0" {
		f.fatalf("credential-bearing principal refusal created its database role")
	}
	// The CRD caps failure retries at one hour, shorter than the suite's
	// bound. Suspending after the exact refusal evidence keeps a second Plan
	// from hiding behind a count taken only once.
	f.suspend(schemaName, true)
	f.waitForSchema(schemaName, "the credential-bearing principal refusal to become durably suspended", principalSuspended)
	if f.newWatchedCount(schemaName, "plan", planBefore) != 1 {
		f.fatalf("suspending the credential-bearing principal refusal dispatched another Plan Job")
	}
	if f.newWatchedCount(schemaName, "apply", applyBefore) != 0 {
		f.fatalf("suspending the credential-bearing principal refusal dispatched an Apply Job")
	}
	f.auditRuntime()
}

// jobDeadline lets Kubernetes end one running Apply at its Job's deadline and
// proves the operator treats the outcome as unknown: a read-only recovery, a
// fresh plan nobody approved, and a database no fresh approval touched. It
// then starts the three Applies the restart proofs hold, each blocked on its
// own database barrier after taking its advisory lock and its own target
// Lease.
func (f *faultRun) jobDeadline() {
	f.t.Helper()
	s := &f.state
	timeout := &s.mysqlTimeout
	f.logf("forcing one real Kubernetes Apply Job deadline")
	f.startMySQLBarrier(timeout.database, timeout.barrier)
	f.createApproval(timeout.schema, timeout.approval)
	timeout.run = f.waitForApplyPod(timeout.schema)
	f.startReadBarrier()
	timeout.deadlineStartedAt = f.recordRunningDeadlineEvidence(timeout.schema, faultTimeoutDeadlineSeconds, timeout.run)
	f.startFollowLogs(f.in.TestNamespace, timeout.run.podName, timeout.run.podUID)
	f.assertMySQLApplyLockWait(timeout.database)
	timeout.lease = f.waitForLeaseReacquisition(timeout.idle, "the running Kubernetes-timeout Apply")
	f.waitForDeadlineJobTerminalAndAudit(timeout.run, faultTimeoutDeadlineSeconds, timeout.deadlineStartedAt)
	f.stopMySQLBarrier()
	// Kubernetes deletes the Pods of a Job that exceeds its active deadline,
	// so the mutation boundary may have no terminal Pod to record: the
	// snapshot keeps the Job identity and the Pod or none.
	observe, plan := f.captureUncertainReadProofPair(timeout.schema, timeout.run, timeout.lease,
		timeout.observeBefore, timeout.planBefore, true)
	timeout.proofObserveUID, timeout.proofPlanUID = observe.uid, plan.uid
	fresh := f.waitForSchema(timeout.schema, "a fresh unapproved plan after the Kubernetes Apply Job deadline",
		freshPlanAwaitingApproval)
	if fresh.Status.Plan.UID == "" {
		f.fatalf("Kubernetes-timeout recovery did not publish an immutable fresh plan")
	}
	timeout.freshPlanUID = string(fresh.Status.Plan.UID)
	f.assertApprovalConsumed(timeout.approval, timeout.originalPlanUID)
	if f.addedJobCount(timeout.schema, "apply") != 1 {
		f.fatalf("Kubernetes-timeout recovery replayed or replaced its Apply Job")
	}
	if f.addedPodCount(timeout.schema, "apply") != 1 {
		f.fatalf("Kubernetes-timeout recovery created a replacement Apply Pod")
	}
	if f.newWatchedCount(timeout.schema, "observe", timeout.observeBefore) != 1 {
		f.fatalf("Kubernetes-timeout recovery did not retain exactly one fresh Observe Job")
	}
	if f.newWatchedCount(timeout.schema, "plan", timeout.planBefore) != 1 {
		f.fatalf("Kubernetes-timeout recovery did not retain exactly one fresh Plan Job")
	}
	if f.mysqlFingerprint(timeout.database) != timeout.prefaultFingerprint {
		f.fatalf("Kubernetes-timeout recovery changed the database before fresh approval")
	}
	f.assertColumn("mysql", timeout.database, "fault_token", 0)

	f.startPGBarrier(s.pgRestart.database, s.pgRestart.barrier)
	f.startPGBarrier(s.pgParallel.database, s.pgParallel.barrier)
	f.startMySQLBarrier(s.mysqlUnknown.database, s.mysqlUnknown.barrier)

	f.createApproval(s.pgRestart.schema, s.pgRestart.approval)
	s.pgRestart.run = f.waitForApplyPod(s.pgRestart.schema)
	f.assertEphemeralContainerRejected(s.pgRestart.schema, s.pgRestart.run)
	f.assertPGApplyLockWait(s.pgRestart.database)
	s.pgRestart.lease = f.waitForLeaseReacquisition(s.pgRestart.idle, "the PostgreSQL restart Apply")

	f.createApproval(s.pgParallel.schema, s.pgParallel.approval)
	s.pgParallel.run = f.waitForApplyPod(s.pgParallel.schema)
	f.assertPGApplyLockWait(s.pgParallel.database)
	s.pgParallel.lease = f.waitForLeaseReacquisition(s.pgParallel.idle, "the parallel PostgreSQL Apply")

	f.createApproval(s.mysqlUnknown.schema, s.mysqlUnknown.approval)
	s.mysqlUnknown.run = f.waitForApplyPod(s.mysqlUnknown.schema)
	f.assertMySQLApplyLockWait(s.mysqlUnknown.database)
	s.mysqlUnknown.lease = f.waitForLeaseReacquisition(s.mysqlUnknown.idle, "the uncertain MySQL Apply")
	if s.pgRestart.lease.uid == s.pgParallel.lease.uid || s.pgRestart.lease.uid == s.mysqlUnknown.lease.uid ||
		s.pgParallel.lease.uid == s.mysqlUnknown.lease.uid {
		f.fatalf("distinct coordination keys collapsed into one target Lease")
	}
	// Three Apply Pods and three independent operator Leases are live here.
	// Both same-engine Pods hold database-local native advisory locks and
	// block independently on their own metadata barriers.
	f.assertPGApplyLockWait(s.pgRestart.database)
	f.assertPGApplyLockWait(s.pgParallel.database)
	f.assertMySQLApplyLockWait(s.mysqlUnknown.database)
}

// managerRestart replaces every manager Pod while the three Applies are
// active, and holds each to the operation, Job, Pod and Lease it had: a new
// manager adopts what is running instead of dispatching again.
func (f *faultRun) managerRestart() {
	f.t.Helper()
	s := &f.state
	f.logf("restarting the manager while three independent Apply Pods are active")
	f.auditRuntime()
	before := f.loadReadyManagerPodUIDs()
	leaderName, leaderUID := f.loadReadyManagerLeader("")
	f.startFollowLogs(f.in.OperatorNamespace, leaderName, leaderUID)
	f.replaceManagerPods()
	f.finishFollowLogs("old manager logs through the restart")
	if after := f.loadReadyManagerPodUIDs(); !managerPodsReplaced(before, after) {
		f.fatalf("manager rollout retained or lost a controller Pod UID")
	}
	f.loadReadyManagerLeader(leaderUID)
	f.auditRuntime()
	targets := []*faultTarget{&s.pgRestart, &s.pgParallel, &s.mysqlUnknown}
	for _, target := range targets {
		f.assertActiveIdentity(target.schema, target.run)
	}
	for _, target := range targets {
		f.assertLeaseIdentity(target.lease)
	}
	f.assertPGApplyLockWait(s.pgRestart.database)
	f.assertPGApplyLockWait(s.pgParallel.database)
	f.assertMySQLApplyLockWait(s.mysqlUnknown.database)
	for _, check := range []struct{ schema, message string }{
		{s.pgRestart.schema, "manager restart created a second PostgreSQL Apply Job"},
		{s.pgParallel.schema, "manager restart created a second parallel PostgreSQL Apply Job"},
		{s.mysqlUnknown.schema, "manager restart created a second MySQL Apply Job"},
	} {
		if f.addedJobCount(check.schema, "apply") != 1 {
			f.fatalf("%s", check.message)
		}
	}
}

// runnerTermination ends the uncertain MySQL Apply's runner inside its Pod
// and holds the operator to a read-only recovery at the Apply's Lease epoch;
// then it lets both PostgreSQL Applies finish and proves each converged; then
// it proves two resources that reach one database through different URLs
// serialize through one target Lease, and that the second, dispatched after
// the first changed the database, refuses its stale plan.
func (f *faultRun) runnerTermination() {
	f.t.Helper()
	s := &f.state
	unknown := &s.mysqlUnknown
	f.logf("terminating one active Apply runner without deleting its Pod")
	f.auditRuntime()
	f.startReadBarrier()
	f.startFollowLogs(f.in.TestNamespace, unknown.run.podName, unknown.run.podUID)
	// The exec stream may close non-zero when signaling PID 1 tears down the
	// same container. The log stream's natural end is the termination proof.
	_, _, _ = f.cluster.Kubectl(f.ctx, "-n", f.in.TestNamespace, "exec", "pod/"+unknown.run.podName, "-c", "ptah", "--",
		"/bin/kill", "-TERM", "1")
	f.finishFollowLogs("the signaled active MySQL Apply Pod logs through termination")
	f.stopMySQLBarrier()
	if operationID, _, _ := f.waitForOperationJobTerminal(unknown.schema, "apply"); operationID != unknown.run.operationID {
		f.fatalf("uncertain MySQL Apply terminal Job changed its operation identity")
	}
	f.auditProtectedTerminalJob(unknown.run.jobName, unknown.run.jobUID, unknown.run.podUID)
	f.waitForOutcomeUnknownWatch(unknown.schema, unknown.run.operationID)
	pods := &podEvidence{uids: []string{unknown.run.podUID}}

	observeUID := f.waitForOneNewWatchedJob(unknown.schema, "observe", unknown.observeBefore,
		"a read-only Observe Job after the uncertain Apply")
	observe := f.recoveryProofJob(unknown.schema, "observe", observeUID,
		"the read-only Observe Job after the uncertain Apply", "recovery Observe Job")
	f.heldSnapshot(unknown.schema, heldProof{
		stage: ptahv1alpha1.OperationObserve, operationID: observe.result.operationID, jobUID: observe.uid,
		leaseEpoch: unknown.lease.epoch, outcome: ptahv1alpha1.PendingObservationOutcomeUnknown,
		applyOperationID: unknown.run.operationID, applyPods: pods,
	}, "uncertain MySQL Observe was harvested while status writes were held")
	f.assertLeaseHeldWithoutRelease(unknown.lease)
	f.startReadBarrier()
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after uncertain MySQL Observe")

	planUID := f.waitForOneNewWatchedJob(unknown.schema, "plan", unknown.planBefore,
		"a Plan Job derived after the exact recovery Observe")
	plan := f.recoveryProofJob(unknown.schema, "plan", planUID,
		"the read-only Plan Job after the uncertain Apply", "recovery Plan Job")
	f.heldSnapshot(unknown.schema, heldProof{
		stage: ptahv1alpha1.OperationPlan, operationID: plan.result.operationID, jobUID: plan.uid,
		leaseEpoch: unknown.lease.epoch, outcome: ptahv1alpha1.PendingObservationOutcomeUnknown,
		applyOperationID: unknown.run.operationID, applyPods: pods,
	}, "uncertain MySQL proof Plan was harvested while status writes were held")
	f.assertLeaseHeldWithoutRelease(unknown.lease)
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after uncertain MySQL proof")
	unknown.proofObserveUID, unknown.proofPlanUID = observe.uid, plan.uid

	f.waitForSchema(unknown.schema, "read-only observation and a fresh unapproved plan after the uncertain Apply",
		recoveredAwaitingApproval)
	if !completedBeforeAdded(f.jobs.snapshot(), observe.uid, plan.uid) {
		f.fatalf("recovery Plan Job was not ordered after the exact completed Observe Job")
	}
	postfault := f.schema(unknown.schema)
	if postfault.Status.Plan == nil || postfault.Status.Plan.UID == "" {
		f.fatalf("uncertain MySQL Apply recovery did not publish an immutable plan")
	}
	unknown.freshPlanUID = string(postfault.Status.Plan.UID)
	if err := observationAdvanced(postfault, unknown.prefaultObservedAt); err != nil {
		f.fatalf("recovery Observe did not advance the target observation evidence: %v", err)
	}
	if err := driftedObserveBound(postfault, observe.result.result, "mysql", "mariadb"); err != nil {
		f.fatalf("recovery Observe did not advance the target observation evidence: %v", err)
	}
	fresh := f.schemaPlan(postfault.Status.Plan.Name)
	document, parsed, contentDigest, chunks := f.freshPlanDocument(fresh, "the exact recovery native plan")
	if string(fresh.UID) != unknown.freshPlanUID {
		f.fatalf("fresh MySQL plan is not bound to the exact recovery Plan result: it is UID %s", fresh.UID)
	}
	if err := freshPlanBound(postfault, fresh, plan.result.result, parsed, contentDigest); err != nil {
		f.fatalf("fresh MySQL plan is not bound to the exact recovery Plan result: %v", err)
	}
	f.assertConfidentialPlan(plan.result.result, document, "the MySQL recovery")
	f.logf("the MySQL recovery Plan result preserves confidentiality and the exact %d-chunk plan document", chunks)
	f.assertApprovalConsumed(unknown.approval, unknown.originalPlanUID)
	if f.addedJobCount(unknown.schema, "apply") != 1 {
		f.fatalf("the uncertain MySQL Apply was replayed without a fresh approval")
	}
	if f.addedPodCount(unknown.schema, "apply") != 1 {
		f.fatalf("podReplacementPolicy=Failed allowed a second executor Pod for the failed Apply Job")
	}
	f.assertColumn("mysql", unknown.database, "fault_token", 0)
	if f.mysqlFingerprint(unknown.database) != unknown.prefaultFingerprint {
		f.fatalf("uncertain MySQL recovery changed a column or index outside the planned mutation")
	}

	for _, pair := range []struct {
		target *faultTarget
		label  string
	}{{&s.pgRestart, "restarted PostgreSQL"}, {&s.pgParallel, "parallel PostgreSQL"}} {
		target := pair.target
		f.startReadBarrier()
		f.stopPGBarrier(target.barrier)
		result := f.captureExactJobResult(target.run.jobName, target.run.jobUID, "apply")
		if result.operationID != target.run.operationID {
			f.fatalf("%s Apply result changed its immutable operation identity", pair.label)
		}
		f.assertSuccessfulApplyResult(target.schema, target.run, result)
		observe, plan := f.assertConvergenceResultPair(target.schema, target.observeBefore, target.planBefore,
			target.run.operationID, target.lease, "")
		target.proofObserveUID, target.proofPlanUID = observe.uid, plan.uid
	}
	if f.addedJobCount(s.pgRestart.schema, "apply") != 1 {
		f.fatalf("the restarted PostgreSQL Apply was duplicated")
	}
	if f.addedJobCount(s.pgParallel.schema, "apply") != 1 {
		f.fatalf("the independent PostgreSQL Apply was duplicated")
	}
	if f.addedPodCount(s.pgRestart.schema, "apply") != 1 {
		f.fatalf("the restarted PostgreSQL Apply created a second executor Pod")
	}
	if f.addedPodCount(s.pgParallel.schema, "apply") != 1 {
		f.fatalf("the independent PostgreSQL Apply created a second executor Pod")
	}
	f.assertColumn("postgresql", s.pgRestart.database, "fault_token", 1)
	f.assertColumn("postgresql", s.pgParallel.database, "fault_token", 1)
	f.sharedAlias()
}

// recoveryProofJob lets one held recovery Job run while status writes are
// paused, waits for it to complete, and captures its result.
func (f *faultRun) recoveryProofJob(schema, operation, uid, blocked, live string) harvestedJob {
	f.t.Helper()
	f.assertReadBlocked(uid, blocked)
	f.pauseStatusWrites()
	f.stopReadBarrier()
	name := f.liveJobName(uid, live, operationJobs(schema, operation))
	if !recoveryJobComplete(f.waitForExactJobTerminal(name, uid), uid) {
		f.fatalf("the exact %s did not complete successfully", live)
	}
	return harvestedJob{uid: uid, result: f.captureExactJobResult(name, uid, operation)}
}

// sharedAlias proves the realm rules and the Lease that serializes one
// realm: two resources that reach one database through different URL
// spellings are refused until both declare the realm shared, then one holds
// the target Lease while the other waits without dispatching, and the other,
// dispatched once the first released it, refuses its stale plan and recovers
// read-only through the same Lease.
func (f *faultRun) sharedAlias() {
	f.t.Helper()
	s := &f.state
	database := "e2e_fault_pg_alias"
	secretA, secretB := "e2e-fault-pg-alias-fqdn-db", "e2e-fault-pg-alias-short-db"
	schemaA, schemaB := "e2e-fault-pg-alias-a", "e2e-fault-pg-alias-b"
	approvalA, approvalB := "e2e-fault-pg-alias-a-approval", "e2e-fault-pg-alias-b-approval"
	barrier := "e2e_fault_pg_alias_barrier"
	s.aliasA, s.aliasB, s.aliasBApproval = schemaA, schemaB, approvalB

	f.createDatabase("postgresql", database, secretA)
	f.createURLSecret("postgresql", database, secretB, true)
	if f.secretValue(secretA, "url") == f.secretValue(secretB, "url") {
		f.fatalf("coordination alias Secrets contain identical routes")
	}
	leasesBefore := f.checkpointLeases()
	// Two resources reaching one database through different aliases is what
	// the operator refuses by default. Both are created undeclared first,
	// because the refusal is what this proof owes a reader before the rest of
	// it makes sense: serialization is not ownership, and the operator says so
	// before it serializes anything.
	f.createSchema(faultSchema{name: schemaA, engine: "PostgreSQL", secret: secretA, reference: f.pgReference,
		coordinationKey: "e2e/fault/shared-alias"})
	f.createSchema(faultSchema{name: schemaB, engine: "PostgreSQL", secret: secretB, reference: f.pgReference,
		coordinationKey: "e2e/fault/shared-alias"})
	for _, contested := range []string{schemaA, schemaB} {
		f.waitForSchema(contested, "a refusal to manage a database another resource also claims", realmConflictRefused)
	}
	// The count, not zero. Whichever resource is created first is briefly
	// the only claimant, and the operator starts its read-only chain there
	// and then: a Job dispatched before the second claimant existed is not a
	// contested realm dispatching one, and the refusal cannot undo it. What a
	// refusal owes is that no new work starts while it stands, so the counts
	// are taken once both are refused and compared after the window below.
	jobsOf := func(schema string) int {
		jobs := &batchv1.JobList{}
		f.check(f.list(jobs, client.MatchingLabels{labelSchema: schema}), "list the Jobs of %s", schema)
		return len(jobs.Items)
	}
	contestedA, contestedB := jobsOf(schemaA), jobsOf(schemaB)
	// One declaration is not a contract: the realm stays refused until every
	// claimant made the same statement. A contested realm is looked at again
	// at most a minute later whatever interval the resource runs on, so
	// ninety seconds is long enough for both claimants to look again and
	// still refuse. Waiting it out is the assertion.
	f.patchSchema(schemaA, map[string]any{"spec": map[string]any{"target": map[string]any{"sharedRealm": true}}})
	for window := time.Now().Add(90 * time.Second); time.Now().Before(window); {
		for _, contested := range []string{schemaA, schemaB} {
			if !realmConflictHeld(f.schema(contested)) {
				f.fatalf("%s left the refusal while one claimant had not declared the realm shared", contested)
			}
		}
		f.sleep(10 * time.Second)
	}
	if jobsOf(schemaA) != contestedA {
		f.fatalf("%s started new work while the database realm was contested", schemaA)
	}
	if jobsOf(schemaB) != contestedB {
		f.fatalf("%s started new work while the database realm was contested", schemaB)
	}
	f.patchSchema(schemaB, map[string]any{"spec": map[string]any{"target": map[string]any{"sharedRealm": true}}})
	f.waitForPlan(schemaA)
	targetA := f.schema(schemaA).Status.Target
	f.waitForPlan(schemaB)
	targetB := f.schema(schemaB).Status.Target
	idle := f.loadNewReleasedLease(leasesBefore, "the shared alias realm's initial Plan target lock")
	if targetA.IdentityDigest == "" || targetB.IdentityDigest == "" || targetA.IdentityDigest == targetB.IdentityDigest {
		f.fatalf("the two database URL aliases did not retain distinct target identities")
	}
	if !sha256Pattern.MatchString(targetA.CoordinationDigest) {
		f.fatalf("the first database URL alias has no persisted coordination digest")
	}
	if targetA.CoordinationDigest != targetB.CoordinationDigest {
		f.fatalf("one coordination key produced different persisted realms across database URL aliases")
	}

	f.startPGBarrier(database, barrier)
	observeBeforeA := f.checkpointOperationWatch(schemaA, "observe", 1)
	planBeforeA := f.checkpointOperationWatch(schemaA, "plan", 1)
	observeBeforeB := f.checkpointOperationWatch(schemaB, "observe", 1)
	planBeforeB := f.checkpointOperationWatch(schemaB, "plan", 1)
	s.aliasBefore = map[string][2]checkpoint{
		schemaA: {observeBeforeA, planBeforeA}, schemaB: {observeBeforeB, planBeforeB},
	}
	holder := f.schema(schemaA)
	if holder.Status.Plan == nil || holder.Status.Plan.UID == "" {
		f.fatalf("shared-alias holder did not persist its original plan UID")
	}
	planUIDA := string(holder.Status.Plan.UID)
	f.createApproval(schemaA, approvalA)
	runA := f.waitForApplyPod(schemaA)
	f.assertPGApplyLockWait(database)
	lease := f.waitForLeaseReacquisition(idle, "the shared alias holder Apply")

	contender := f.schema(schemaB)
	if contender.Status.Plan == nil || contender.Status.Plan.UID == "" {
		f.fatalf("shared-alias contender did not persist its original plan UID")
	}
	planUIDB := string(contender.Status.Plan.UID)
	oldPlan := f.schemaPlan(contender.Status.Plan.Name)
	f.createApproval(schemaB, approvalB)
	f.waitForSchema(schemaB, "an undispatched Apply claim blocked by the shared target Lease", undispatchedApplyClaim)
	f.sleep(8 * time.Second)
	claimed := ""
	if latest := f.schema(schemaB); latest.Status.ActiveOperation != nil {
		claimed = latest.Status.ActiveOperation.JobName
	}
	if claimed != "" {
		if err := f.get(claimed, &batchv1.Job{}); err == nil {
			f.fatalf("the shared coordination key allowed the alias contender to dispatch an Apply Job")
		}
	}
	if f.addedJobCount(schemaB, "apply") != 0 {
		f.fatalf("the shared coordination key allowed an alias contender Apply Job")
	}
	f.assertLeaseIdentity(lease)

	// Releasing the first holder must make the exact contender progress once.
	// This separates real Lease contention from a builder, RBAC or reconcile
	// stall.
	f.startReadBarrier()
	f.stopPGBarrier(barrier)
	resultA := f.captureExactJobResult(runA.jobName, runA.jobUID, "apply")
	f.assertSuccessfulApplyResult(schemaA, runA, resultA)
	observeA, planA := f.assertConvergenceResultPair(schemaA, observeBeforeA, planBeforeA, runA.operationID, lease, schemaB)
	f.assertApprovalConsumed(approvalA, planUIDA)
	establishBarrier(f, f.schemas, &ptahv1alpha1.PtahSchema{}, f.in.TestNamespace, schemaA)
	establishBarrier(f, f.jobs, &batchv1.Job{}, f.in.TestNamespace, runA.jobName)
	establishBarrier(f, f.leases, newLease(), f.in.OperatorNamespace, lease.name)
	f.assertPostApplyProofHistory(schemaA, runA.operationID, runA.jobUID, lease, observeA.uid, planA.uid)
	f.waitForWatchCountAbove(schemaB, "apply", 0, "the shared-alias contender to dispatch after the holder released its Lease")
	contenderJobs := addedUIDs(f.jobs.snapshot(), schemaB, "apply")
	if len(contenderJobs) != 1 {
		f.fatalf("expected exactly one alias contender Apply Job UID, found %d", len(contenderJobs))
	}
	jobUIDB := contenderJobs[0]
	operationB, epochB := f.waitForApplyBindingInSchemaWatch(schemaB, jobUIDB, "the exact shared-alias contender Apply binding")
	f.assertReadBlocked(jobUIDB, "the stale shared-alias contender Apply Job")
	leaseB := f.loadHeldLeaseForEpoch(epochB, "the exact shared-alias contender Apply Lease")
	if leaseB.uid != lease.uid {
		f.fatalf("the shared-alias contender acquired a different target Lease UID")
	}
	fingerprint := f.pgFingerprint(database)
	f.pauseStatusWrites()
	f.stopReadBarrier()
	_, jobNameB, terminalUID := f.waitForOperationJobTerminal(schemaB, "apply")
	if terminalUID != jobUIDB {
		f.fatalf("shared-alias contender terminal Job changed its exact UID")
	}
	resultB := f.captureExactJobResult(jobNameB, jobUIDB, "apply")
	if resultB.operationID != operationB {
		f.fatalf("shared-alias contender Apply result changed its immutable operation identity")
	}
	s.aliasBPodUID = resultB.podUID
	if err := stalePlanResult(resultB.result, oldPlan.Spec.ContentDigest, oldPlan.Spec.CoordinationDigest,
		oldPlan.Spec.TargetIdentityDigest); err != nil {
		f.fatalf("shared-alias contender did not return the exact conservative stale-plan result: %v", err)
	}
	if f.pgFingerprint(database) != fingerprint {
		f.fatalf("the stale shared-alias contender changed the database schema")
	}

	f.startReadBarrier()
	f.mustResumeStatusWrites("could not restore status-write RBAC for shared-alias uncertainty proof")
	runB := applyRun{operationID: operationB, jobName: jobNameB, jobUID: jobUIDB, podUID: resultB.podUID}
	observeB, planB := f.captureUncertainReadProofPair(schemaB, runB, leaseB, observeBeforeB, planBeforeB, false)
	f.waitForInSync(schemaB, "ConvergedAfterUnknownOutcome")
	f.assertApprovalConsumed(approvalB, planUIDB)
	if err := contenderRecovered(f.schema(schemaB), observeB.result.result, planB.result.result); err != nil {
		f.fatalf("shared-alias stale recovery did not finish through exact read-only Observe and Plan results: %v", err)
	}
	retired := &ptahv1alpha1.PtahSchemaApproval{}
	f.check(f.get(approvalB, retired), "read PtahSchemaApproval %s", approvalB)
	if !approvalRetired(retired) {
		f.fatalf("shared-alias consumed approval was not marked stale after recovery")
	}
	if f.pgFingerprint(database) != fingerprint {
		f.fatalf("the shared-alias read-only proof changed the database schema")
	}
	establishBarrier(f, f.schemas, &ptahv1alpha1.PtahSchema{}, f.in.TestNamespace, schemaB)
	recoveryPlanJob := f.liveJobName(planB.uid, "shared-alias recovery Plan Job")
	establishBarrier(f, f.jobs, &batchv1.Job{}, f.in.TestNamespace, recoveryPlanJob)
	establishBarrier(f, f.leases, newLease(), f.in.OperatorNamespace, leaseB.name)
	if !completedBeforeAdded(f.jobs.snapshot(), runA.jobUID, jobUIDB) {
		f.fatalf("shared-alias contender dispatched before the exact holder Job completed")
	}
	if f.addedJobCount(schemaA, "apply") != 1 {
		f.fatalf("the shared-alias holder did not execute exactly one Apply Job")
	}
	if f.addedJobCount(schemaB, "apply") != 1 {
		f.fatalf("the shared-alias contender did not dispatch exactly once after Lease release")
	}
	if f.addedPodCount(schemaB, "apply") != 1 {
		f.fatalf("the shared-alias stale Apply created more than one executor Pod")
	}
	if !onlyAdded(f.pods.snapshot(), schemaB, "apply", resultB.podUID) {
		f.fatalf("shared-alias stale Apply history does not contain exactly its result Pod UID")
	}
	if !sameLeaseReacquired(f.leases.snapshot(), f.schemas.snapshot(), f.jobs.snapshot(), leaseReacquisition{
		uid: lease.uid, firstHolder: lease.holder, firstEpoch: lease.epoch,
		schema: schemaB, operationID: operationB, jobUID: jobUIDB, contenderEpoch: epochB,
	}) {
		f.fatalf("target Lease was not released and reacquired by the exact contender on the same UID")
	}
	f.assertUncertainApplyProofHistory(uncertainProof{
		schema: schemaB, applyOperationID: operationB, applyJobUID: jobUIDB, lease: leaseB,
		observeJobUID: observeB.uid, planJobUID: planB.uid,
		applyPods: podEvidence{uids: []string{resultB.podUID}}, mode: uncertainNoChanges,
	})
	f.assertColumn("postgresql", database, "fault_token", 1)

	// Keep a consumed, user-owned approval whose schema no longer exists: a
	// side-effect-free object to close the approval watch on at an exact
	// resourceVersion once every scenario is done.
	f.auditRuntime()
	for _, uid := range []string{jobUIDB, observeB.uid, planB.uid} {
		if uid == "" {
			f.fatalf("shared-alias cascade boundary has an empty exact Job UID")
		}
		if !f.fullyAudited.holds(uid) {
			f.fatalf("shared-alias cascade boundary would delete a Job without a full container audit")
		}
	}
	f.deleteSchema(schemaB)
	if err := f.get(approvalB, &ptahv1alpha1.PtahSchemaApproval{}); err != nil {
		f.fatalf("the user-owned consumed approval unexpectedly disappeared with its schema: %v", err)
	}
}

// jobDeletion removes a held read-only Job and holds the operator to
// dispatching the operation again under a new Job; deletes a schema waiting
// for approval and holds the database untouched; and changes a database by
// hand under an approved plan and holds the operator to refusing the stale
// plan. It then closes the watches and holds their whole history to every
// proof of the fault injection once more, before approving the manual-drift
// recovery plan as its allowed control.
func (f *faultRun) jobDeletion() {
	f.t.Helper()
	f.logf("removing a held read-only Job while its operation is active")
	f.readLoss()
	f.deletionLeavesTheDatabase()
	f.manualDrift()
	f.closingHistory()
	// The complete history above closes the deadline recovery and no-replay
	// proof. Later executor changes would otherwise start unrelated reads
	// with this fixture's 45-second deadline, after its Pod/log watch closed.
	// Suspend only now, so the measured recovery window remains unchanged.
	f.suspend(f.state.mysqlTimeout.schema, true)
	f.waitForSchema(f.state.mysqlTimeout.schema, "the completed deadline fixture to stop creating workloads", quiescentlySuspended)
	f.manualDriftRecovery()
}

// readLoss removes the first held Job of a read chain. A read-only Job that
// disappears while its operation is active once left a schema in Verifying
// forever, with the operation still naming a Job the API server no longer
// had. The controller takes one branch for every read-only operation, so
// holding the first of the chain exercises what Verify would reach. The
// barrier is a NoSchedule taint raised before the schema exists, so the Job is
// held with certainty instead of racing a window a fraction of a second wide.
func (f *faultRun) readLoss() {
	f.t.Helper()
	database, secret, schemaName := "e2e_fault_pg_read_loss", "e2e-fault-pg-read-loss-db", "e2e-fault-pg-read-loss"
	f.createDatabase("postgresql", database, secret)
	f.startReadBarrier()
	f.createSchema(faultSchema{name: schemaName, engine: "PostgreSQL", secret: secret, reference: f.pgReference,
		coordinationKey: "e2e/fault/read-loss"})
	held := f.waitForSchema(schemaName, "a held read-only operation bound to its own Job", heldReadOperation)
	operation := held.Status.ActiveOperation.Type
	name, uid := held.Status.ActiveOperation.JobName, string(held.Status.ActiveOperation.JobUID)
	if name == "" {
		f.fatalf("the held read-only operation did not publish its Job name")
	}
	if uid == "" {
		f.fatalf("the held read-only operation did not publish its Job UID")
	}
	f.assertReadBlocked(uid, "the held "+string(operation)+" Job this proof removes")
	// A held Job is never terminal, so the periodic audit cannot reach it,
	// and the next step destroys it. Audit it where it stands.
	f.auditBlockedReadJob(name, uid)
	job := &batchv1.Job{}
	job.Namespace, job.Name = f.in.TestNamespace, name
	f.check(f.cluster.Client.Delete(f.ctx, job, client.PropagationPolicy("Background")), "delete Job %s", name)
	f.waitForAbsence(&batchv1.Job{}, name)
	// Recovery is a replacement Job, not a cleared field. The controller
	// empties jobUID and delays the redispatch by the failure retry interval,
	// so a wait that only refuses the removed UID is satisfied by that
	// intermediate status, and a regression that never dispatches again would
	// pass it.
	retried := f.waitForSchema(schemaName, "the removed "+string(operation)+" operation to be dispatched again under a new Job",
		func(schema *ptahv1alpha1.PtahSchema) bool { return readOperationRetried(schema, operation, uid) })
	if err := f.get(name, &batchv1.Job{}); err == nil {
		f.fatalf("the removed read-only Job was recreated under its own name")
	}
	retryName, retryUID := retried.Status.ActiveOperation.JobName, string(retried.Status.ActiveOperation.JobUID)
	if retryName == name {
		f.fatalf("the retried read-only operation reused the removed Job name")
	}
	// The replacement is held by the same barrier, which proves the operator
	// dispatched the operation again rather than only forgetting the removed
	// Job. It never runs either, so it is audited where it stands too.
	f.assertReadBlocked(retryUID, "the retried "+string(operation)+" Job")
	f.auditBlockedReadJob(retryName, retryUID)
	f.assertColumn("postgresql", database, "fault_token", 0)
	// Suspension is the documented stop for a read-only operation, taken
	// while the barrier still holds the replacement. Nothing is dispatched
	// after it, so the two audits above stay final.
	f.suspend(schemaName, true)
	f.waitForSchema(schemaName, "the held read-only operation to be discarded by suspension", suspendedReadDiscarded)
	f.deleteSchema(schemaName)
	f.stopReadBarrier()
}

// deletionLeavesTheDatabase deletes a schema waiting for approval, and
// checkpoints its Jobs so the closing history can hold it to starting none
// after.
func (f *faultRun) deletionLeavesTheDatabase() {
	f.t.Helper()
	s := &f.state
	s.deletedDatabase, s.deletedSchema = "e2e_fault_pg_delete", "e2e-fault-pg-delete"
	secret := "e2e-fault-pg-delete-db"
	f.createDatabase("postgresql", s.deletedDatabase, secret)
	f.createSchema(faultSchema{name: s.deletedSchema, engine: "PostgreSQL", secret: secret, reference: f.pgReference,
		coordinationKey: "e2e/fault/delete"})
	f.waitForPlan(s.deletedSchema)
	f.assertColumn("postgresql", s.deletedDatabase, "fault_token", 0)
	s.deletedPrint = f.fingerprint("postgresql", s.deletedDatabase, "the deletion-test database")
	s.deletedCheckpoint = f.checkpointSchemaJobWatch(s.deletedSchema)
	f.auditRuntime()
	f.deleteSchema(s.deletedSchema)
	if f.pgFingerprint(s.deletedDatabase) != s.deletedPrint {
		f.fatalf("deleting an AwaitingApproval PtahSchema changed the database schema")
	}
	f.assertColumn("postgresql", s.deletedDatabase, "fault_token", 0)
}

// manualDrift approves a plan while the controller cannot write status,
// changes the database by hand, and holds the approved Apply to refusing the
// stale plan without running SQL, and the recovery to a fresh plan of the
// changed database.
func (f *faultRun) manualDrift() {
	f.t.Helper()
	s := &f.state
	manual := &s.manual
	f.createDatabase("postgresql", manual.database, manual.secret)
	f.query("postgresql", manual.database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'drift-control', 'preserve-this-row')")
	f.planTarget(manual, faultSchema{engine: "PostgreSQL", reference: f.pgReference},
		"the manual-drift schema's initial Plan target lock")
	schema := f.schema(manual.schema)
	if schema.Status.Plan == nil || schema.Status.Plan.UID == "" {
		f.fatalf("manual-drift schema did not persist the old plan UID")
	}
	manual.originalPlanUID = string(schema.Status.Plan.UID)
	old := f.schemaPlan(schema.Status.Plan.Name)
	for _, digest := range []struct{ value, what string }{
		{old.Spec.ActualStateFingerprint, "old actual-state fingerprint"},
		{old.Spec.ContentDigest, "old plan content digest"},
		{old.Spec.CoordinationDigest, "old coordination digest"},
		{old.Spec.TargetIdentityDigest, "old target identity digest"},
	} {
		if !sha256Pattern.MatchString(digest.value) {
			f.fatalf("manual-drift schema did not persist its %s", digest.what)
		}
	}
	manual.observeBefore = f.checkpointOperationWatch(manual.schema, "observe", 1)
	manual.planBefore = f.checkpointOperationWatch(manual.schema, "plan", 1)
	f.pauseStatusWrites()
	f.createApproval(manual.schema, manual.approval)
	f.sleep(3 * time.Second)
	if f.addedJobCount(manual.schema, "apply") != 0 {
		f.fatalf("manual-drift Apply dispatched while controller status writes were denied")
	}
	f.query("postgresql", manual.database, "ALTER TABLE e2e_widgets DROP COLUMN enabled")
	f.assertColumn("postgresql", manual.database, "enabled", 0)
	f.assertColumn("postgresql", manual.database, "fault_token", 0)
	s.manualPrint = f.pgFingerprint(manual.database)
	s.manualSQL = f.startSchemaDriftWindow(manual.schema, manual.database, "",
		&databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: "postgresql"}, "")
	f.startReadBarrier()
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after the manual-drift barrier")
	f.waitForWatchCountAbove(manual.schema, "apply", 0, "the manual-drift Apply Job to be created behind the scheduling barrier")
	applies := addedUIDs(f.jobs.snapshot(), manual.schema, "apply")
	if len(applies) != 1 {
		f.fatalf("expected one manual-drift Apply Job UID, found %d", len(applies))
	}
	jobUID := applies[0]
	operationID, epoch := f.waitForApplyBindingInSchemaWatch(manual.schema, jobUID, "the exact manual-drift Apply binding")
	jobName := f.liveJobName(jobUID, "manual-drift Apply Job")
	f.assertReadBlocked(jobUID, "the manual-drift stale Apply Job")
	manual.lease = f.waitForLeaseReacquisition(manual.idle, "the manual-drift stale Apply")
	if manual.lease.epoch != epoch {
		f.fatalf("manual-drift Apply schema binding and live Lease epoch differ")
	}
	f.pauseStatusWrites()
	f.stopReadBarrier()
	if terminalOperation, _, terminalUID := f.waitForOperationJobTerminal(manual.schema, "apply"); terminalOperation != operationID || terminalUID != jobUID {
		f.fatalf("manual-drift terminal Apply changed its exact operation or Job identity")
	}
	stale := f.captureExactJobResult(jobName, jobUID, "apply")
	if stale.operationID != operationID {
		f.fatalf("manual-drift Apply result changed its immutable operation identity")
	}
	s.manualJobUID, s.manualPodUID = jobUID, stale.podUID
	if err := stalePlanResult(stale.result, old.Spec.ContentDigest, old.Spec.CoordinationDigest,
		old.Spec.TargetIdentityDigest); err != nil {
		f.fatalf("manual-drift Apply did not return the exact conservative stale-plan result: %v", err)
	}
	if f.pgFingerprint(manual.database) != s.manualPrint {
		f.fatalf("a stale approved plan executed SQL after the database changed manually")
	}
	f.assertColumn("postgresql", manual.database, "enabled", 0)
	f.assertColumn("postgresql", manual.database, "fault_token", 0)
	f.startReadBarrier()
	f.mustResumeStatusWrites("could not restore status-write RBAC for manual-drift uncertainty proof")
	manual.run = applyRun{operationID: operationID, jobName: jobName, jobUID: jobUID, podUID: stale.podUID}
	observe, plan := f.captureUncertainReadProofPair(manual.schema, manual.run, manual.lease,
		manual.observeBefore, manual.planBefore, false)
	manual.proofObserveUID, manual.proofPlanUID = observe.uid, plan.uid
	s.manualSQLControls = []operationSQLClient{
		{resourceUID: string(schema.UID), jobUID: observe.uid, podUID: observe.result.podUID, operation: "observe"},
		{resourceUID: string(schema.UID), jobUID: plan.uid, podUID: plan.result.podUID, operation: "plan"},
	}
	f.waitForManualDriftContract(manual.schema, manual.approval, operationID, manual.originalPlanUID,
		old.Spec.ActualStateFingerprint, manual.observeBefore)
	current := f.schema(manual.schema)
	if current.Status.Plan == nil || current.Status.Plan.Name == "" || current.Status.Plan.UID == "" {
		f.fatalf("manual drift did not settle at a fresh plan")
	}
	manual.freshPlanUID = string(current.Status.Plan.UID)
	if err := driftedObserveBound(current, observe.result.result, "postgres", "postgresql"); err != nil {
		f.fatalf("manual-drift fresh Observe result is not bound to its exact target evidence: %v", err)
	}
	fresh := f.schemaPlan(current.Status.Plan.Name)
	document, parsed, contentDigest, chunks := f.freshPlanDocument(fresh, "the manual-drift fresh native plan")
	if string(fresh.UID) == manual.originalPlanUID {
		f.fatalf("manual-drift fresh plan is not bound to the exact Plan result: it is the old plan")
	}
	if err := freshPlanBound(current, fresh, plan.result.result, parsed, contentDigest); err != nil {
		f.fatalf("manual-drift fresh plan is not bound to the exact Plan result: %v", err)
	}
	f.assertConfidentialPlan(plan.result.result, document, "the manual-drift fresh")
	f.logf("the manual-drift fresh Plan result preserves confidentiality and the exact %d-chunk plan document", chunks)
	f.assertApprovalConsumed(manual.approval, manual.originalPlanUID)
	if f.pgFingerprint(manual.database) != s.manualPrint {
		f.fatalf("manual-drift read-only Observe or Plan changed the database schema")
	}
	f.assertColumn("postgresql", manual.database, "enabled", 0)
	f.assertColumn("postgresql", manual.database, "fault_token", 0)
	if !completedBeforeAdded(f.jobs.snapshot(), observe.uid, plan.uid) {
		f.fatalf("manual-drift fresh Plan was dispatched before the exact Observe result completed")
	}
	if f.addedJobCount(manual.schema, "apply") != 1 {
		f.fatalf("manual drift caused a blind Apply replay")
	}
	if f.addedPodCount(manual.schema, "apply") != 1 {
		f.fatalf("manual drift created a replacement Apply Pod")
	}
	if !outcomeUnknownRecorded(f.schemas.snapshot(), manual.schema, operationID) {
		f.fatalf("manual drift lost its conservative OutcomeUnknown history")
	}
	planJob := f.liveJobName(plan.uid, "manual-drift recovery Plan Job")
	establishBarrier(f, f.schemas, &ptahv1alpha1.PtahSchema{}, f.in.TestNamespace, manual.schema)
	establishBarrier(f, f.jobs, &batchv1.Job{}, f.in.TestNamespace, planJob)
	establishBarrier(f, f.leases, newLease(), f.in.OperatorNamespace, manual.lease.name)
	f.assertUncertainApplyProofHistory(uncertainProof{
		schema: manual.schema, applyOperationID: operationID, applyJobUID: jobUID, lease: manual.lease,
		observeJobUID: observe.uid, planJobUID: plan.uid, freshPlanUID: manual.freshPlanUID,
		applyPods: podEvidence{uids: []string{stale.podUID}}, mode: uncertainManualDrift,
		oldActual: old.Spec.ActualStateFingerprint,
	})
}

// manualDriftRecovery starts after closingHistory has checked the complete
// refusal window. The original watches remain closed and unchanged. The
// ordinary data-plane ledger audits the newly authorized operation.
func (f *faultRun) manualDriftRecovery() {
	f.t.Helper()
	manual := &f.state.manual
	current := f.waitForSchema(manual.schema, "the exact manual-drift recovery plan awaiting approval",
		func(resource *ptahv1alpha1.PtahSchema) bool {
			return planAwaitingApproval(resource) && string(resource.Status.Plan.UID) == manual.freshPlanUID
		})
	fresh := f.schemaPlan(current.Status.Plan.Name)
	if string(fresh.UID) != manual.freshPlanUID || string(fresh.UID) == manual.originalPlanUID {
		f.fatalf("manual-drift recovery read an unrelated plan")
	}
	if f.pgFingerprint(manual.database) != f.state.manualPrint {
		f.fatalf("manual-drift database changed before its fresh authorization")
	}
	audit := &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: "postgresql"}
	beforeSQL := audit.snapshot()
	beforeApply := f.checkpointJobs(manual.schema, "apply")
	approval := manual.approval + "-current"
	f.createApproval(manual.schema, approval)
	f.waitForApprovedPlanConverged(manual.schema, fresh.Spec.ArtifactDigest, fresh.Spec.Fingerprint,
		string(fresh.UID), "the freshly approved manual-drift plan to converge")
	result := f.captureOneNewJobResult(manual.schema, "apply", beforeApply, nil)
	if err := automaticApplyResult(result, fresh.Spec.ContentDigest, fresh.Spec.CoordinationDigest, fresh.Spec.TargetIdentityDigest); err != nil {
		f.fatalf("manual-drift recovery did not execute its exact fresh plan: %v", err)
	}
	completed := f.captured
	if completed.jobUID == f.state.manualJobUID || completed.podUID == f.state.manualPodUID {
		f.fatalf("manual-drift recovery reused the refused Apply identity")
	}
	audit.assertRecords(beforeSQL, audit.snapshot(),
		audit.terminalPod(map[string]string{"job-name": completed.jobName}, completed.jobUID), true)
	audit.close()
	f.dataPlane.assertApprovalConsumed(approval, string(fresh.UID))
	f.assertColumn("postgresql", manual.database, "enabled", 1)
	f.assertColumn("postgresql", manual.database, "fault_token", 1)
	if f.query("postgresql", manual.database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='drift-control' AND note='preserve-this-row'") != "1" ||
		f.query("postgresql", manual.database, "SELECT count(*) FROM e2e_widgets") != "1" {
		f.fatalf("manual-drift recovery changed the preserved row")
	}
	f.assertOneNewJob(manual.schema, "apply", beforeApply)
	f.auditRuntime()
	f.assertObservedJobsAudited()
	f.logf("PASS PostgreSQL manual drift: the closed refusal history contains no replay; fresh approval applied its exact plan and preserved the row")
}

// assertDeletedSchemaStartedNothing holds the deleted schema to no Job the
// watch saw added after the deletion checkpoint.
func (f *faultRun) assertDeletedSchemaStartedNothing(when string) {
	f.t.Helper()
	s := &f.state
	if len(newAddedUIDs(f.jobs.snapshot(), s.deletedSchema, "", s.deletedCheckpoint)) != 0 {
		f.fatalf("%s created an operation Job %s", s.deletedSchema, when)
	}
}

// closingHistory closes every watch at an exact resourceVersion and holds the
// whole history to what each proof claimed: nothing was replayed, replaced or
// dispatched later, no two operations of one schema overlapped, and every
// Job and Pod the watches saw was audited.
func (f *faultRun) closingHistory() {
	f.t.Helper()
	s := &f.state
	// Keep the no-SQL deletion proof open through every later reconciliation
	// and fault scenario, rather than accepting only a short quiet interval.
	f.assertDeletedSchemaStartedNothing("at some point after deletion began")
	if f.pgFingerprint(s.deletedDatabase) != s.deletedPrint {
		f.fatalf("the deletion-test database changed during the full post-delete observation window")
	}
	f.assertColumn("postgresql", s.deletedDatabase, "fault_token", 0)
	// Uncertainty safety holds after every later reconcile: a consumed
	// approval stays ineligible, not only until the first recovery Observe.
	f.assertApprovalConsumed(s.mysqlUnknown.approval, s.mysqlUnknown.originalPlanUID)
	if !uncertainStillSafe(f.schema(s.mysqlUnknown.schema)) {
		f.fatalf("uncertain MySQL Apply later became active or rebound its consumed approval")
	}
	f.assertColumn("mysql", s.mysqlUnknown.database, "fault_token", 0)
	if f.mysqlFingerprint(s.mysqlUnknown.database) != s.mysqlUnknown.prefaultFingerprint {
		f.fatalf("uncertain MySQL Apply changed a column or index during the delayed-replay window")
	}
	f.assertApprovalConsumed(s.mysqlTimeout.approval, s.mysqlTimeout.originalPlanUID)
	if !timeoutRecoveryStillSafe(f.schema(s.mysqlTimeout.schema)) {
		f.fatalf("Kubernetes-timeout recovery later became active or rebound its consumed approval")
	}
	if f.mysqlFingerprint(s.mysqlTimeout.database) != s.mysqlTimeout.prefaultFingerprint {
		f.fatalf("Kubernetes-timeout recovery changed the database during the delayed-replay window")
	}
	f.assertColumn("mysql", s.mysqlTimeout.database, "fault_token", 0)

	f.auditRuntime()
	sentinel := f.databasePod()
	establishBarrier(f, f.approvals, &ptahv1alpha1.PtahSchemaApproval{}, f.in.TestNamespace, s.aliasBApproval)
	establishBarrier(f, f.schemas, &ptahv1alpha1.PtahSchema{}, f.in.TestNamespace, heartbeatSchema)
	establishBarrier(f, f.leases, newLease(), f.in.OperatorNamespace, s.pgRestart.lease.name)
	establishBarrier(f, f.jobs, &batchv1.Job{}, f.in.TestNamespace, faultHeartbeatJob)
	establishBarrier(f, f.pods, &corev1.Pod{}, f.in.TestNamespace, sentinel)
	f.stopHeartbeat()
	f.assertWatchesAlive()
	f.stopWatches()
	f.auditRuntime()
	f.validateAndScanWatches()
	f.assertDeletedSchemaStartedNothing("after the deletion evidence boundary")

	jobs, pods := f.jobs.snapshot(), f.pods.snapshot()
	oneApply := func(schema string) bool {
		return len(addedUIDs(jobs, schema, "apply")) == 1 && len(addedUIDs(pods, schema, "apply")) == 1
	}
	if !oneApply(s.pgRestart.schema) {
		f.fatalf("the restarted PostgreSQL Apply history is not exactly one Job and Pod UID")
	}
	if !oneApply(s.pgParallel.schema) {
		f.fatalf("the parallel PostgreSQL Apply history is not exactly one Job and Pod UID")
	}
	if !oneApply(s.aliasA) || !oneApply(s.aliasB) {
		f.fatalf("the shared-alias Apply history contains a delayed replay")
	}
	if !onlyAdded(pods, s.aliasB, "apply", s.aliasBPodUID) {
		f.fatalf("shared-alias stale Apply final history changed its exact Pod UID")
	}
	manual := &s.manual
	if len(addedUIDs(jobs, manual.schema, "apply")) != 1 {
		f.fatalf("manual drift caused a delayed Apply replay")
	}
	if len(addedUIDs(pods, manual.schema, "apply")) != 1 {
		f.fatalf("manual drift created a delayed replacement Apply Pod")
	}
	if !onlyAdded(jobs, manual.schema, "apply", s.manualJobUID) {
		f.fatalf("manual-drift Apply history does not contain exactly its original Job UID")
	}
	if !onlyAdded(pods, manual.schema, "apply", s.manualPodUID) {
		f.fatalf("manual-drift Apply history does not contain exactly its original Pod UID")
	}
	if f.newWatchedCount(manual.schema, "observe", manual.observeBefore) != 1 {
		f.fatalf("manual drift did not retain exactly one fresh Observe Job")
	}
	if f.newWatchedCount(manual.schema, "plan", manual.planBefore) != 1 {
		f.fatalf("manual drift did not retain exactly one fresh Plan Job")
	}
	for _, retained := range []struct {
		target  *faultTarget
		schema  string
		message string
	}{
		{&s.pgRestart, s.pgRestart.schema, "restarted PostgreSQL convergence did not retain exactly one proof Observe and Plan Job"},
		{&s.pgParallel, s.pgParallel.schema, "parallel PostgreSQL convergence did not retain exactly one proof Observe and Plan Job"},
		{&s.mysqlUnknown, s.mysqlUnknown.schema, "uncertain Apply recovery did not retain exactly one fresh Observe and Plan Job"},
		{&s.mysqlTimeout, s.mysqlTimeout.schema, "Kubernetes-timeout recovery did not retain exactly one fresh Observe and Plan Job"},
	} {
		if f.newWatchedCount(retained.schema, "observe", retained.target.observeBefore) != 1 ||
			f.newWatchedCount(retained.schema, "plan", retained.target.planBefore) != 1 {
			f.fatalf("%s", retained.message)
		}
	}
	f.assertAliasRetained()
	if len(addedUIDs(jobs, s.mysqlUnknown.schema, "apply")) != 1 {
		f.fatalf("the uncertain MySQL Apply was replayed during a later reconciliation")
	}
	if len(addedUIDs(pods, s.mysqlUnknown.schema, "apply")) != 1 {
		f.fatalf("the uncertain MySQL Apply Job created a later replacement Pod")
	}
	timeout := &s.mysqlTimeout
	if !oneApply(timeout.schema) {
		f.fatalf("Kubernetes-timeout Apply history contains a replacement or delayed replay")
	}
	if !deadlineJobHistory(jobs, timeout.schema, timeout.run.jobUID, faultTimeoutDeadlineSeconds) {
		f.fatalf("Kubernetes-timeout Apply history lost its exact DeadlineExceeded Job UID")
	}
	if !deadlinePodHistory(pods, timeout.schema, deadlinePod{
		name: timeout.run.podName, uid: timeout.run.podUID, jobName: timeout.run.jobName, jobUID: timeout.run.jobUID,
		operationID: timeout.run.operationID, startedAt: timeout.deadlineStartedAt,
	}) {
		f.fatalf("Kubernetes-timeout Apply history lost its exact running-to-deleted original Pod UID")
	}
	if f.pgFingerprint(manual.database) != s.manualPrint {
		f.fatalf("manual drift or its delayed read-only proof changed the database schema")
	}
	f.assertColumn("postgresql", manual.database, "enabled", 0)
	f.assertColumn("postgresql", manual.database, "fault_token", 0)
	unknown := &s.mysqlUnknown
	if !onlyAdded(jobs, unknown.schema, "apply", unknown.run.jobUID) {
		f.fatalf("uncertain MySQL Apply history does not contain exactly the original Job UID")
	}
	if !onlyAdded(pods, unknown.schema, "apply", unknown.run.podUID) {
		f.fatalf("uncertain MySQL Apply history does not contain exactly the original Pod UID")
	}
	if s.principalSchema == "" || s.principalPlanUID == "" || s.principalPlanPodUID == "" {
		f.fatalf("credential-bearing principal refusal did not retain its exact result identities")
	}
	if !onlyAdded(jobs, s.principalSchema, "plan", s.principalPlanUID) || len(addedUIDs(jobs, s.principalSchema, "apply")) != 0 {
		f.fatalf("credential-bearing principal refusal did not remain one Plan and zero Apply Jobs")
	}
	if !onlyAdded(pods, s.principalSchema, "plan", s.principalPlanPodUID) || len(addedUIDs(pods, s.principalSchema, "apply")) != 0 {
		f.fatalf("credential-bearing principal refusal did not remain one Plan and zero Apply Pods")
	}
	if !readChainOrdered(jobs, s.pgRestart.schema) {
		f.fatalf("%s did not complete Resolve and Verify before its first database Observe Job", s.pgRestart.schema)
	}
	f.assertUncertainApplyProofHistory(uncertainProof{
		schema: unknown.schema, applyOperationID: unknown.run.operationID, applyJobUID: unknown.run.jobUID,
		lease: unknown.lease, observeJobUID: unknown.proofObserveUID, planJobUID: unknown.proofPlanUID,
		freshPlanUID: unknown.freshPlanUID, applyPods: podEvidence{uids: []string{unknown.run.podUID}},
		mode: uncertainSamePlan,
	})
	f.assertUncertainApplyProofHistory(uncertainProof{
		schema: timeout.schema, applyOperationID: timeout.run.operationID, applyJobUID: timeout.run.jobUID,
		lease: timeout.lease, observeJobUID: timeout.proofObserveUID, planJobUID: timeout.proofPlanUID,
		freshPlanUID: timeout.freshPlanUID, applyPods: podEvidence{uids: []string{timeout.run.podUID}, optional: true},
		mode: uncertainSamePlan,
	})
	for _, target := range []*faultTarget{&s.pgRestart, &s.pgParallel} {
		f.assertPostApplyProofHistory(target.schema, target.run.operationID, target.run.jobUID, target.lease,
			target.proofObserveUID, target.proofPlanUID)
	}
	if operationPodsOverlap(pods) {
		f.fatalf("Kubernetes Pod watch proves overlapping operation Pods for one PtahSchema")
	}
	if operationJobsOverlap(jobs) {
		f.fatalf("Kubernetes Job watch proves overlapping operation Jobs for one PtahSchema")
	}
	f.waitForAuditComplete()
	f.recordJobsForParent()
	if s.manualSQL == nil || len(s.manualSQLControls) != 2 {
		f.fatalf("manual drift lost its SQL window or recovery result controls")
	}
	f.retainSchemaSQLWatch(s.manualSQL)
	s.manualSQL.assertStale(f.schema(manual.schema), operationSQLClient{
		resourceUID: string(s.manualSQL.resourceUID), jobUID: s.manualJobUID, podUID: s.manualPodUID, operation: "apply",
	}, s.manualSQLControls...)
	f.logf("PASS watches, Kubernetes deadline recovery, stale-plan preflight, native lock barriers, restart identity, " +
		"uncertain recovery, deletion, Pod serialization, credential audit, and coordination realms")
}

// assertAliasRetained holds both shared-alias schemas to one Observe and one
// Plan since their checkpoints.
func (f *faultRun) assertAliasRetained() {
	f.t.Helper()
	s := &f.state
	for _, alias := range []struct{ schema, message string }{
		{s.aliasA, "shared-alias holder did not retain exactly one proof Observe and Plan Job"},
		{s.aliasB, "shared-alias stale contender did not retain exactly one recovery Observe and Plan Job"},
	} {
		checkpoints, found := s.aliasBefore[alias.schema]
		if !found || f.newWatchedCount(alias.schema, "observe", checkpoints[0]) != 1 ||
			f.newWatchedCount(alias.schema, "plan", checkpoints[1]) != 1 {
			f.fatalf("%s", alias.message)
		}
	}
}

// newLease is an empty Lease to name a barrier with.
func newLease() *coordinationv1.Lease { return &coordinationv1.Lease{} }
