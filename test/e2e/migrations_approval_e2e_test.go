//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (m *migrationRun) statusBarrier() *controllerStatusBarrier {
	m.t.Helper()
	deployments := &appsv1.DeploymentList{}
	m.check(m.cluster.Client.List(m.ctx, deployments,
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "find the installed controller")
	if len(deployments.Items) != 1 {
		m.fatalf("expected one controller Deployment, found %d", len(deployments.Items))
	}
	deployment := &deployments.Items[0]
	sa := deployment.Spec.Template.Spec.ServiceAccountName
	if sa == "" {
		m.fatalf("controller Deployment has no ServiceAccount")
	}
	barrier := &controllerStatusBarrier{
		cluster: m.cluster, role: deployment.Name,
		user:      "system:serviceaccount:" + deployment.Namespace + ":" + sa,
		namespace: m.in.TestNamespace, resource: "ptahmigrations",
	}
	t := m.t
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := barrier.resume(ctx); err != nil {
			t.Errorf("restore migration status writes: %v", err)
		}
	})
	return barrier
}

// approvalInputChange proves that an admitted approval cannot survive a
// policy or artifact edit before an Apply is dispatched. The status barrier leaves
// admission available but prevents the controller from persisting a claim.
// A fresh approval then executes the current artifact on the same database.
//
// On PostgreSQL the artifact row makes its original approval with kubectl ptah
// approve --wait, and that wait has to end with exit 8, naming the plan that ran
// in place of the one it approved.
func (m *migrationRun) approvalInputChange(field string) {
	m.t.Helper()
	suffix := "approval-" + field
	name := "e2e-" + suffix + "-" + m.engine.name
	var edit map[string]any
	var verification *verificationPolicyFixture
	var originalPolicy, currentPolicy verificationPolicyIdentity
	reference := m.reference("")
	var oldDigest, newDigest string
	switch field {
	case "policy":
		edit = map[string]any{"policy": map[string]any{"apply": "Never"}}
	case "transaction-mode":
		edit = map[string]any{"policy": map[string]any{"transactionMode": "none"}}
	case "verification-policy-uid", "verification-policy-content":
		verification = newVerificationPolicyFixture(m.t, m.ctx, m.cluster, m.in.TestNamespace, name+"-policy", migrationArtifactType)
		originalPolicy = verification.identity(verificationPolicyKey)
		if field == "verification-policy-content" {
			edit = map[string]any{"artifact": map[string]any{"verificationPolicyFrom": map[string]any{"key": narrowedPolicyKey}}}
		}
	case "artifact":
		reference = m.reference("-approval-artifact-original")
		changedReference := m.reference("-approval-artifact-current")
		oldDigest = m.publish("approval-artifact-original", m.fixtureDir("-older"), reference)
		newDigest = m.publish("approval-artifact-current", m.fixtureDir(""), changedReference)
		if oldDigest == newDigest {
			m.fatalf("the approval artifact fixtures have the same digest")
		}
		edit = map[string]any{"artifact": map[string]any{"ociRef": changedReference}}
	default:
		m.fatalf("unsupported approval input change %q", field)
	}
	database := "ptah_e2e_" + strings.ReplaceAll(suffix, "-", "_")
	secret := "e2e-" + m.engine.name + "-" + suffix + "-db"
	var auditUser string
	if m.engine.name == "mysql" {
		auditUser = m.isolatedMySQLAuditDatabase(database, secret)
	} else {
		m.isolatedDatabase(database, secret)
	}
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: m.engine.name}
	var beforeRefusal []byte
	var mysqlBeforeRefusal []mysqlStatementRecord
	if m.engine.name == "postgresql" {
		audit.snapshot()
		beforeRefusal = audit.pgPrefix
	} else {
		mysqlBeforeRefusal = audit.mysqlStatementSnapshot()
	}
	inventory := &migrationSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}}
	waitForDecision := func(description string, match func(*ptahv1alpha1.PtahMigration) bool) *ptahv1alpha1.PtahMigration {
		return m.waitForMigration(name, description, migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
			m.captureMigrationSQLInventory(name, inventory)
			return match(resource)
		})
	}
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: reference,
		coordinationKey: "e2e/" + suffix + "/" + m.engine.name,
		apply:           "OnApproval", interval: "1h",
		edit: func(spec map[string]any) {
			if verification != nil {
				spec["artifact"].(map[string]any)["verificationPolicyFrom"] = map[string]any{"name": verification.object.Name, "key": verificationPolicyKey}
			}
		},
	}))
	before := waitForDecision("a plan awaiting approval",
		func(resource *ptahv1alpha1.PtahMigration) bool {
			return resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval &&
				resource.Status.ActiveOperation == nil && resource.Status.Plan != nil
		})
	oldPlan := m.planOf(before.Status.Plan.Name)
	if oldPlan.UID == "" || oldPlan.Spec.Fingerprint == "" {
		m.fatalf("%s published a plan without an identity", name)
	}
	barrier := m.statusBarrier()
	m.check(barrier.pause(m.ctx), "hold migration status writes before approval")
	var wait *approveWait
	if field == "artifact" && m.engine.name == "postgresql" {
		// The person who approved the plan this row makes stale waits for its run. The
		// wait reads status and the approval and never the database, so one engine
		// holds it.
		wait = m.startApproveWait(name, name+"-old", oldPlan)
	} else {
		m.check(m.approve(name+"-old", name, oldPlan.Name, string(oldPlan.UID), oldPlan.Spec.Fingerprint),
			"admit the original approval")
	}
	if current := m.migration(name); current.Status.ActiveOperation != nil {
		m.fatalf("%s claimed an operation before the input changed", name)
	}
	m.assertNoNewApplyJob(nil, "before the input changed", name)
	if field == "verification-policy-uid" {
		verification.replaceIdentity()
		currentPolicy = verification.identity(verificationPolicyKey)
	} else {
		m.patchMigration(name, map[string]any{"spec": edit})
		if field == "verification-policy-content" {
			currentPolicy = verification.identity(narrowedPolicyKey)
		}
	}
	changed := m.migration(name)
	if field == "verification-policy-uid" && changed.Generation != before.Generation {
		m.fatalf("%s verification policy replacement depended on a resource generation change", name)
	}
	if field != "verification-policy-uid" && changed.Generation <= before.Generation {
		m.fatalf("%s input edit did not change its generation", name)
	}
	m.check(barrier.resume(m.ctx), "resume reconciliation with the changed input")
	refused := waitForDecision("the changed input to invalidate the approved plan",
		func(resource *ptahv1alpha1.PtahMigration) bool {
			return changedMigrationApprovalRefused(resource, oldPlan.UID, changed.Generation, field == "policy")
		})
	newPlan := m.planOf(refused.Status.Plan.Name)
	if newPlan.Spec.Fingerprint == oldPlan.Spec.Fingerprint {
		m.fatalf("%s reused the fingerprint after changing %s", name, field)
	}
	if field == "artifact" {
		if err := changedMigrationArtifactPlan(oldPlan, newPlan, oldDigest, newDigest); err != nil {
			m.fatalf("%s did not bind a new decision to the changed artifact: %v", name, err)
		}
	}
	if verification != nil {
		m.check(changedVerificationPolicyDecision(migrationVerificationDecision(oldPlan), migrationVerificationDecision(newPlan),
			originalPolicy, currentPolicy, field), "bind a fresh migration decision to the changed verification policy")
	}
	m.assertNoNewApplyJob(nil, "under the obsolete approval", name)
	m.assertDatabaseUnmigrated(name, database)
	if field == "policy" {
		// Keep the restored policy distinct from the original approved policy.
		// Restoring byte-for-byte input is a different question from approving
		// the changed policy this row proves.
		m.patchMigration(name, map[string]any{"spec": map[string]any{"policy": map[string]any{
			"apply": "OnApproval", "lockTimeout": "45s",
		}}})
		generation := m.migration(name).Generation
		refused = waitForDecision("a fresh plan under the restored approval gate",
			func(resource *ptahv1alpha1.PtahMigration) bool {
				return changedMigrationApprovalRefused(resource, oldPlan.UID, generation, false)
			})
		newPlan = m.planOf(refused.Status.Plan.Name)
	}
	m.assertNoNewApplyJob(nil, "before the fresh approval", name)
	m.assertDatabaseUnmigrated(name, database)
	beforeApply := audit.snapshot()
	if m.engine.name == "postgresql" {
		m.assertPostgresMigrationRefusalSQL(audit, beforeRefusal, database, refused, inventory)
	} else {
		m.assertMySQLMigrationRefusalSQL(audit, mysqlBeforeRefusal, database, auditUser, refused, inventory)
	}
	m.check(m.approve(name+"-current", name, newPlan.Name, string(newPlan.UID), newPlan.Spec.Fingerprint),
		"approve the changed input")
	converged := m.waitForGenerationInSync(name)
	jobs := m.applyJobUIDs(name)
	if len(jobs) != 1 {
		m.fatalf("%s created %d Apply Jobs, want one after the fresh approval", name, len(jobs))
	}
	run := converged.Status.LastRun
	if run == nil || string(run.JobUID) != jobs[0] || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied {
		m.fatalf("%s did not record its freshly approved Apply as successful", name)
	}
	audit.assertRecords(beforeApply, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": run.JobName}, string(run.JobUID)), true)
	audit.close()
	if history := m.query(restoreRevisionsQuery(m.engine.name), database); history != "1,2,3" {
		m.fatalf("%s fresh approval did not record the complete current sequence", name)
	}
	if count := m.query("SELECT count(*) FROM e2e_migration_widgets", database); count != "3" {
		m.fatalf("%s fresh approval did not seed exactly three rows: %s", name, count)
	}
	if color := m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", database); color != "blue" {
		m.fatalf("%s fresh approval did not apply the final migration: %s", name, color)
	}
	if wait != nil {
		m.assertApproveWaitStopped(wait, oldPlan.Name, newPlan.Name)
	}
	m.finishFixture(name)
	m.logf("PASS %s %s changed after approval: no Apply before a fresh decision; fresh decision applied", m.engine.kind, field)
}

// approveWaitTimeout bounds the wait the row starts. It outlasts the rest of the
// row, so a timeout says the operator never ended the run rather than that the
// row was slow.
const approveWaitTimeout = 30 * time.Minute

// approveWait is kubectl ptah approve --wait, left running while the row goes on.
type approveWait struct {
	command        *exec.Cmd
	stdout, stderr lockedBuffer
	done           chan struct{}
	err            error
}

// startApproveWait approves the plan with the plugin, as a person does who stays for
// the run, and stops the process once admission has stored the approval. Stopped, it
// reads nothing until assertApproveWaitStopped lets it go on, so the first document it
// reads after the approval is the one in which another plan has run and been read:
// the order in which the replacement's run used to read as success. A wait that kept
// reading would see the replacement published first and stop on PlanSuperseded,
// which the unit rows of internal/operate and cmd/kubectl-ptah hold.
func (m *migrationRun) startApproveWait(migration, approval string, plan *ptahv1alpha1.PtahMigrationPlan) *approveWait {
	m.t.Helper()
	wait := &approveWait{done: make(chan struct{})}
	wait.command = exec.CommandContext(m.ctx, m.kubectlPtah, "approve", "migration/"+migration, //nolint:gosec // The phase's own build.
		"--plan-fingerprint", plan.Spec.Fingerprint, "--name", approval,
		"--wait", "--wait-timeout", approveWaitTimeout.String(),
		"--kubeconfig", m.in.Kubeconfig, "-n", m.in.TestNamespace)
	wait.command.Stdout, wait.command.Stderr = &wait.stdout, &wait.stderr
	m.check(wait.command.Start(), "start kubectl ptah approve --wait")
	go func() {
		wait.err = wait.command.Wait()
		close(wait.done)
	}()
	t := m.t
	t.Cleanup(func() {
		// A row that failed with the process stopped leaves no process behind.
		select {
		case <-wait.done:
		default:
			_ = wait.command.Process.Kill()
			<-wait.done
		}
	})

	stored := &ptahv1alpha1.PtahMigrationApproval{}
	m.poll("admission to store approval "+approval, time.Second, func() bool {
		select {
		case <-wait.done:
			m.fatalf("kubectl ptah approve --wait ended before approval %s was stored: %v\n%s", approval, wait.err, m.approveWaitOutput(wait))
		default:
		}
		return m.get(approval, stored) == nil
	})
	if stored.Spec.PlanRef.UID != plan.UID || stored.Spec.PlanFingerprint != plan.Spec.Fingerprint {
		m.fatalf("kubectl ptah approve stored approval %s for plan %s, not for plan %s", approval, stored.Spec.PlanRef.Name, plan.Name)
	}
	if err := wait.command.Process.Signal(syscall.SIGSTOP); err != nil {
		m.fatalf("stop kubectl ptah approve --wait: %v\n%s", err, m.approveWaitOutput(wait))
	}
	m.logf("approval %s of plan %s stored; its approve --wait is stopped until another plan has run", approval, plan.Name)
	return wait
}

// assertApproveWaitStopped lets the stopped approve --wait read again, once the plan
// that replaced the approved one has run and been read, and holds what it said.
func (m *migrationRun) assertApproveWaitStopped(wait *approveWait, approved, ran string) {
	m.t.Helper()
	if err := wait.command.Process.Signal(syscall.SIGCONT); err != nil {
		m.fatalf("let kubectl ptah approve --wait read again: %v\n%s", err, m.approveWaitOutput(wait))
	}
	select {
	case <-wait.done:
	case <-time.After(2 * time.Minute):
		m.fatalf("kubectl ptah approve --wait for plan %s did not end within two minutes of reading again, after plan %s ran:\n%s",
			approved, ran, m.approveWaitOutput(wait))
	case <-m.ctx.Done():
		m.fatalf("the phase's bound ended while approve --wait read again: %v", m.ctx.Err())
	}
	code := 0
	var exit *exec.ExitError
	switch {
	case wait.err == nil:
	case errors.As(wait.err, &exit):
		code = exit.ExitCode()
	default:
		m.fatalf("kubectl ptah approve --wait ended with no exit status: %v\n%s", wait.err, m.approveWaitOutput(wait))
	}
	output := m.approveWaitOutput(wait)
	if err := approveWaitStoppedOnAnotherRun(code, string(wait.stderr.Bytes()), approved, ran); err != nil {
		m.fatalf("kubectl ptah approve --wait for plan %s, after plan %s ran: %v\n%s", approved, ran, err, output)
	}
	m.logf("PASS approve --wait for the stale plan %s exited %d once plan %s had run:\n%s", approved, code, ran, output)
}

// approveWaitOutput is what the plugin wrote, scanned before anything prints it.
func (m *migrationRun) approveWaitOutput(wait *approveWait) string {
	m.t.Helper()
	stdout, stderr := wait.stdout.Bytes(), wait.stderr.Bytes()
	m.scan(stdout, "the approve --wait output")
	m.scan(stderr, "the approve --wait diagnostics")
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
}
