//go:build e2e

package e2e

import (
	"bytes"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (m *migrationRun) approvalIdentityReplacement() {
	m.t.Helper()
	name, database := "e2e-approval-identity-"+m.engine.name, "ptah_e2e_approval_identity"
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
	waitForPlan := func() *ptahv1alpha1.PtahMigration {
		return m.waitForMigration(name, "the exact resource's approval gate", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
			m.captureMigrationSQLInventory(name, inventory)
			return resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && resource.Status.ActiveOperation == nil &&
				resource.Status.ObservedGeneration == resource.Generation && resource.Status.Plan != nil &&
				conditionStatus(resource.Status.Conditions, "ApprovalRequired", "True")
		})
	}
	document := m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""), coordinationKey: "e2e/approval-identity/" + m.engine.name,
		apply: "OnApproval", interval: "1h",
	})
	m.mustCreate(deepCopyMap(document))
	old := waitForPlan()
	oldPlan := m.planOf(old.Status.Plan.Name)
	barrier := m.statusBarrier()
	m.check(barrier.pause(m.ctx), "hold claims before approving the original migration")
	m.check(m.approve(name+"-old", name, oldPlan.Name, string(oldPlan.UID), oldPlan.Spec.Fingerprint), "approve the original migration")
	originalApproval := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-old", originalApproval), "read the admitted original approval")
	if originalApproval.UID == "" || originalApproval.Spec.MigrationRef.UID != old.UID || originalApproval.Spec.PlanRef.UID != oldPlan.UID ||
		originalApproval.Spec.PlanFingerprint != oldPlan.Spec.Fingerprint || originalApproval.Spec.Approver.Username == "" ||
		originalApproval.Spec.MutationRequestUID == "" || originalApproval.Spec.ApprovedAt.IsZero() || m.migration(name).Status.ActiveOperation != nil {
		m.fatalf("the original decision was not admitted before replacement with claims held")
	}
	m.assertNoNewApplyJob(nil, "before replacing the approved resource", name)
	// Mark deletion while no claim can be persisted. An idle resource may
	// disappear immediately; otherwise resume lets normal finalization finish.
	m.check(m.cluster.Client.Delete(m.ctx, old, client.Preconditions{UID: &old.UID},
		client.PropagationPolicy(metav1.DeletePropagationBackground)), "delete the exact approved migration")
	deleting := &ptahv1alpha1.PtahMigration{}
	err := m.get(name, deleting)
	if !apierrors.IsNotFound(err) && (err != nil || deleting.UID != old.UID || deleting.DeletionTimestamp == nil) {
		m.fatalf("the approved migration was not marked for deletion before claims resumed")
	}
	m.check(barrier.resume(m.ctx), "resume normal deletion of the approved migration")
	m.poll("the original migration to disappear", time.Second, func() bool {
		m.captureMigrationSQLInventory(name, inventory)
		return apierrors.IsNotFound(m.get(name, &ptahv1alpha1.PtahMigration{}))
	})
	m.mustCreate(deepCopyMap(document))
	current := waitForPlan()
	currentPlan := m.planOf(current.Status.Plan.Name)
	m.check(replacedMigrationDecision(old, current, oldPlan, currentPlan), "bind the unchanged work to a new resource identity")
	retained := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(originalApproval.Name, retained), "read the surviving original approval")
	if retained.UID != originalApproval.UID || !reflect.DeepEqual(retained.Spec, originalApproval.Spec) ||
		retained.DeletionTimestamp != nil || conditionStatus(retained.Status.Conditions, "Consumed", "True") {
		m.fatalf("the original approval did not survive unchanged and unconsumed")
	}

	for _, row := range []struct {
		name, resourceUID, planUID, fingerprint, reason string
	}{
		{"resource-uid", string(old.UID), string(currentPlan.UID), currentPlan.Spec.Fingerprint, "approval migration reference does not match the plan"},
		{"plan-uid", string(current.UID), string(oldPlan.UID), currentPlan.Spec.Fingerprint, "referenced plan UID does not match; the plan was replaced"},
		{"fingerprint", string(current.UID), string(currentPlan.UID), oldPlan.Spec.Fingerprint, "approval plan fingerprint does not match the immutable plan"},
	} {
		approvalName := name + "-wrong-" + row.name
		err := m.create(migrationApprovalDocument(m.in.TestNamespace, approvalName, name, row.resourceUID, currentPlan.Name, row.planUID, row.fingerprint))
		if err != nil {
			m.scan([]byte(err.Error()), "the identity approval refusal")
		}
		if !migrationIdentityApprovalRefusal(err, row.reason) {
			m.fatalf("%s approval was not refused by the intended binding guard: %v", row.name, err)
		}
		if !apierrors.IsNotFound(m.get(approvalName, &ptahv1alpha1.PtahMigrationApproval{})) {
			m.fatalf("%s refused approval was stored", row.name)
		}
		m.logf("PASS %s same-name replacement refused approval %s at its binding guard", m.engine.kind, row.name)
	}
	m.captureMigrationSQLInventory(name, inventory)
	m.assertNoNewApplyJob(nil, "under the previous resource's approval", name)
	m.assertDatabaseUnmigrated(name, database)
	beforeApply := audit.snapshot()
	clients, err := inventory.clients(current, old)
	m.check(err, "attribute SQL to both exact resource lifetimes")
	var counts map[string]int
	if m.engine.name == "postgresql" {
		if len(pgBefore) == 0 || !bytes.HasPrefix(audit.pgPrefix, pgBefore) {
			m.fatalf("replacement SQL audit lost its original journal window")
		}
		counts, err = postgresMigrationRefusalSQL(audit.pgPrefix[len(pgBefore):], database, clients)
	} else {
		counts, err = mysqlMigrationRefusalSQL(mysqlBefore, audit.mysqlStatementSnapshot(), database, user, clients)
	}
	m.check(err, "refuse unauthorized SQL across migration replacement")
	m.check(migrationReplacementSQLControls(clients, counts, string(old.UID), string(current.UID)), "observe both replacement SQL controls")
	m.reportMigrationRefusalSQL(current, clients, counts)

	m.check(m.approve(name+"-current", name, currentPlan.Name, string(currentPlan.UID), currentPlan.Spec.Fingerprint), "approve the replacement migration")
	converged := m.waitForGenerationInSync(name)
	jobs, run := m.applyJobUIDs(name), converged.Status.LastRun
	if converged.UID != current.UID || len(jobs) != 1 || run == nil || string(run.JobUID) != jobs[0] || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied {
		m.fatalf("the replacement did not record exactly one freshly approved Apply")
	}
	job := &batchv1.Job{}
	m.check(m.get(run.JobName, job), "read the replacement's successful Apply Job")
	if job.UID != run.JobUID || !ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahMigration", current.Name, current.UID) {
		m.fatalf("the successful Apply belongs to another migration")
	}
	consumed := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(name+"-current", consumed), "read the consumed new-resource approval")
	if consumed.Spec.MigrationRef.UID != current.UID || consumed.Spec.PlanRef.UID != currentPlan.UID ||
		consumed.Spec.PlanFingerprint != currentPlan.Spec.Fingerprint || !conditionStatus(consumed.Status.Conditions, "Consumed", "True") {
		m.fatalf("the successful Apply did not consume the replacement's exact approval")
	}
	audit.assertRecords(beforeApply, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": run.JobName}, string(run.JobUID)), true)
	audit.close()
	if m.query(restoreRevisionsQuery(m.engine.name), database) != "1,2,3" ||
		m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", database) != "blue" {
		m.fatalf("the freshly approved replacement did not converge from the database")
	}
	m.logf("PASS %s same-name migration replacement: oldUID=%s newUID=%s old approval refused; new approval applied once", m.engine.kind, old.UID, current.UID)
}
