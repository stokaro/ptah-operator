//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// Use the driver's host-side address of the same task registry. The publisher
// verifies both manifests by digest; only their reachability address differs
// from the reference the cluster uses. Credentials travel through environment.
func (m *migrationRun) executorVariant() string {
	m.t.Helper()
	repository, digest, ok := strings.Cut(m.in.ExecutorImage, "@")
	path, sameRegistry := strings.CutPrefix(repository, m.registryHost+"/")
	if !ok || !sameRegistry || path == "" || m.in.RegistryHostAddress == "" || !sha256Pattern.MatchString(digest) {
		m.fatalf("executor variant must use the driver's pinned task registry image")
	}
	content, err := os.ReadFile(m.in.RegistryCredentialsFile)
	m.check(err, "read executor variant registry credentials")
	credentials, err := parseRegistryCredentials(content)
	m.check(err, "decode executor variant registry credentials")
	m.protect(credentials.Password)
	root, err := filepath.Abs(repositoryRoot)
	m.check(err, "locate the executor variant publisher")
	hostSource := m.in.RegistryHostAddress + "/" + path + "@" + digest
	command := exec.CommandContext(m.ctx, "go", "-C", root, "run", "./test/e2e/handcraftoci", "executor-variant", hostSource) //nolint:gosec // Argument vector, no credentials.
	command.Env = append(os.Environ(), "PTAH_OCI_REGISTRY="+m.in.RegistryHostAddress,
		"PTAH_OCI_USERNAME="+credentials.Username, "PTAH_OCI_PASSWORD="+credentials.Password)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	m.scan(stdout.Bytes(), "executor variant publisher output")
	m.scan(stderr.Bytes(), "executor variant publisher diagnostics")
	m.check(err, "publish the synthetic executor identity")
	published, err := executorVariantReference(hostSource, stdout.String())
	m.check(err, "verify the host-side executor publication")
	_, newDigest, _ := strings.Cut(published, "@")
	replacement := repository + "@" + newDigest
	m.logf("executor identity fixture: original=%s replacement=%s", m.in.ExecutorImage, replacement)
	return replacement
}

func (m *migrationRun) executorHistoryControl(resource *ptahv1alpha1.PtahMigration, plan *ptahv1alpha1.PtahMigrationPlan,
	inventory *migrationSQLInventory,
) operationSQLClient {
	m.t.Helper()
	m.captureMigrationSQLInventory(resource.Name, inventory)
	var selected []*batchv1.Job
	for _, job := range inventory.jobs {
		if job.Labels[labelOperation] == "history" && job.Annotations[annotationBindingID] == plan.Spec.ExecutionBindingID &&
			job.Annotations["operator.ptah.run/ptah-version"] == plan.Spec.PtahVersion && jobUsesExecutor(&job, plan.Spec.ExecutorImage) {
			selected = append(selected, job.DeepCopy())
		}
	}
	if len(selected) != 1 || !conditionTrue(selected[0].Status.Conditions, batchv1.JobComplete) {
		m.fatalf("executor decision needs exactly one completed History under its image and epoch")
	}
	job := selected[0]
	var pods []*corev1.Pod
	for _, pod := range inventory.pods {
		if ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
			pods = append(pods, pod.DeepCopy())
		}
	}
	if len(pods) != 1 || !resultTransportPod(pods[0]) || pods[0].Annotations[annotationOperationID] != job.Annotations[annotationOperationID] {
		m.fatalf("executor History did not retain one exact successful result Pod")
	}
	pod := pods[0]
	m.poll("the executor History's complete result", time.Second, func() bool {
		logs, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, pod.Name, "ptah")
		m.check(err, "read the exact executor History result")
		m.scan(logs, "executor History result")
		result, err := readOperationResult(m.ctx, m.cluster.Client, job, pod, runner.OperationMigrationHistory, job.Annotations[annotationOperationID], logs)
		if operationResultPending(err) {
			return false
		}
		m.check(err, "parse the executor History result")
		m.check(migrationHistoryMatchesExecutorPlan(result, plan), "bind the exact History result to the published plan")
		return true
	})
	return operationSQLClient{resourceUID: string(resource.UID), jobUID: string(job.UID), podUID: string(pod.UID), operation: "history"}
}

func (m *migrationRun) executorImageChange() {
	m.t.Helper()
	m.executionComponentChange(executionComponentChange{"executor-image", m.in.ExecutorImage, m.executorVariant()})
}

func (m *migrationRun) ptahVersionChange() {
	m.t.Helper()
	replacement, err := ptahVersionAlias(m.in.PtahVersion)
	m.check(err, "derive the same pinned Ptah build's alternate version declaration")
	m.executionComponentChange(executionComponentChange{"ptah-version", m.in.PtahVersion, replacement})
}

func (m *migrationRun) executionComponentChange(change executionComponentChange) {
	m.t.Helper()
	name := "e2e-migration-" + change.argument + "-change-" + m.engine.name
	database, secret := "ptah_e2e_"+strings.ReplaceAll(change.argument, "-", "_")+"_change", name+"-db"
	deployments := &appsv1.DeploymentList{}
	m.check(m.cluster.Client.List(m.ctx, deployments, client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "find the executor rollout Deployment")
	if len(deployments.Items) != 1 {
		m.fatalf("executor transition needs one exact manager Deployment")
	}
	key := client.ObjectKeyFromObject(&deployments.Items[0])
	barrier := m.statusBarrier()
	scenario := m.t
	scenario.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := setControllerExecutionComponent(ctx, m.cluster, key, change.reverse()); err != nil {
			scenario.Errorf("restore the migration executor identity: %v", err)
		}
	})
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
	m.mustCreate(m.migrationDocument(migrationSpec{name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/" + change.argument + "/" + m.engine.name, apply: "OnApproval", interval: "1h"}))
	before := wait("the original executor's approval gate", func(resource *ptahv1alpha1.PtahMigration) bool {
		return resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && resource.Status.Plan != nil && resource.Status.ActiveOperation == nil
	})
	old := m.planOf(before.Status.Plan.Name)
	initialHistory := m.executorHistoryControl(before, old, inventory)
	m.check(barrier.pause(m.ctx), "hold the admitted migration approval before dispatch")
	m.check(m.approve(name+"-old", name, old.Name, string(old.UID), old.Spec.Fingerprint), "admit the original executor's decision")
	if m.migration(name).Status.ActiveOperation != nil {
		m.fatalf("executor transition lost the undispatched migration boundary")
	}
	m.assertNoNewApplyJob(nil, "before the executor changed", name)
	rolloutExecutionManagers(m.t, m.ctx, m.cluster, key, change, m.scan)
	m.check(barrier.resume(m.ctx), "resume reconciliation under the replacement executor")
	current := wait("the replacement executor's approval gate", func(resource *ptahv1alpha1.PtahMigration) bool {
		return changedMigrationApprovalRefused(resource, old.UID, before.Generation, false)
	})
	fresh := m.planOf(current.Status.Plan.Name)
	m.check(changedMigrationExecutionDecision(before, current, old, fresh, change), "bind the replacement migration decision")
	currentHistory := m.executorHistoryControl(current, fresh, inventory)
	m.assertNoNewApplyJob(nil, "under the original executor's approval", name)
	stale := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-old", stale), "read the original executor's approval")
	if stale.Spec.MigrationRef.UID != before.UID || stale.Spec.PlanRef.UID != old.UID || stale.Spec.PlanFingerprint != old.Spec.Fingerprint || conditionStatus(stale.Status.Conditions, "Consumed", "True") {
		m.fatalf("the old executor's approval changed or was consumed")
	}
	m.assertDatabaseUnmigrated(name, database)
	beforeApply := audit.snapshot()
	clients, err := inventory.clients(current)
	m.check(err, "attribute both executor epochs' diagnostic SQL")
	var counts map[string]int
	if m.engine.name == "postgresql" {
		if len(pgBefore) == 0 || !bytes.HasPrefix(audit.pgPrefix, pgBefore) {
			m.fatalf("executor transition lost its original PostgreSQL journal")
		}
		counts, err = inventory.postgresRefusalSQL(audit.pgPrefix[len(pgBefore):], database, clients, "")
	} else {
		counts, err = mysqlMigrationRefusalSQL(mysqlBefore, audit.mysqlStatementSnapshot(), database, user, clients)
	}
	m.check(err, "refuse unauthorized SQL through the executor transition")
	m.check(migrationExecutorSQLControls(clients, counts, initialHistory, currentHistory), "observe both exact History results in server SQL")
	m.reportMigrationRefusalSQL(current, clients, counts)
	m.check(m.approve(name+"-current", name, fresh.Name, string(fresh.UID), fresh.Spec.Fingerprint), "approve the replacement executor's plan")
	converged := m.waitForGenerationInSync(name)
	jobs, run := m.applyJobUIDs(name), converged.Status.LastRun
	if len(jobs) != 1 || run == nil || string(run.JobUID) != jobs[0] || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied ||
		!slices.Equal(run.AppliedVersions, []int64{1, 2, 3}) {
		m.fatalf("the replacement executor did not record exactly one successful Apply")
	}
	job := &batchv1.Job{}
	m.check(m.get(run.JobName, job), "read the replacement executor's Apply Job")
	if job.UID != run.JobUID || !jobUsesExecutor(job, fresh.Spec.ExecutorImage) || job.Annotations[annotationBindingID] != fresh.Spec.ExecutionBindingID ||
		job.Annotations["operator.ptah.run/ptah-version"] != fresh.Spec.PtahVersion ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahMigration", current.Name, current.UID) {
		m.fatalf("the fresh migration Apply lost its resource, image or execution epoch")
	}
	m.check(migrationExecutorApplyInputs(job, fresh), "read the approved plan inputs from the actual Apply Job")
	m.scanObject(job, "the replacement executor Apply Job")
	m.scan(m.jobLogs(job), "the replacement executor Apply result")
	consumed := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-current", consumed), "read the consumed replacement-executor approval")
	if consumed.Spec.MigrationRef.UID != current.UID || consumed.Spec.PlanRef.UID != fresh.UID || consumed.Spec.PlanFingerprint != fresh.Spec.Fingerprint ||
		!conditionStatus(consumed.Status.Conditions, "Consumed", "True") {
		m.fatalf("the replacement executor did not consume its exact approval")
	}
	audit.assertRecords(beforeApply, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": run.JobName}, string(run.JobUID)), true)
	audit.close()
	if m.query(restoreRevisionsQuery(m.engine.name), database) != "1,2,3" || m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", database) != "blue" {
		m.fatalf("the freshly approved executor did not converge from the database")
	}
	m.finishFixture(name)
	rolloutExecutionManagers(m.t, m.ctx, m.cluster, key, change.reverse(), m.scan)
	m.logf("PASS %s %s changed after migration approval: old decision refused; fresh decision applied once", m.engine.kind, change.argument)
}
