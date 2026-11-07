//go:build e2e

package e2e

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// applyAsksForTheRealmLease holds what a reader meets when an approved Apply
// loses the realm Lease (#822). The holder here is written by the row, which is
// what lets it say when the Lease came free: a sibling's Plan frees it at a moment
// nothing outside can date. The resource names the Lease it waits for and no Job
// exists, and the Job appears within seconds of the Lease coming free, with
// minutes still on the clock the Lease had.
func (m *migrationRun) applyAsksForTheRealmLease() {
	m.t.Helper()
	name := "e2e-lock-wait-" + m.engine.name
	database := "ptah_e2e_lock_wait"
	secret := name + "-db"
	if m.engine.name == "mysql" {
		m.isolatedMySQLAuditDatabase(database, secret)
	} else {
		m.isolatedDatabase(database, secret)
	}
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/lock-wait/" + m.engine.name,
		apply:           "OnApproval", interval: "1h",
	}))
	waiting := m.waitForMigration(name, "a plan awaiting approval", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		return resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval &&
			resource.Status.ActiveOperation == nil && resource.Status.Plan != nil
	})
	plan := m.planOf(waiting.Status.Plan.Name)
	leaseName, err := targetlock.LeaseName(plan.Spec.CoordinationDigest)
	m.check(err, "name the realm Lease of %s", name)

	// The Leases live beside the manager.
	managers := &appsv1.DeploymentList{}
	m.check(m.cluster.Client.List(m.ctx, managers, client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "find the installed controller")
	if len(managers.Items) != 1 {
		m.fatalf("expected one controller Deployment, found %d", len(managers.Items))
	}
	namespace := managers.Items[0].Namespace

	// The read that made the plan may have left the Lease behind with no holder,
	// and may not have.
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: leaseName}}
	holder, duration, now := "a-sibling-in-this-realm", int32(300), metav1.NowMicro()
	held := func() {
		lease.Spec = coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, AcquireTime: &now, RenewTime: &now}
	}
	key := types.NamespacedName{Namespace: namespace, Name: leaseName}
	switch getErr := m.cluster.Client.Get(m.ctx, key, lease); {
	case apierrors.IsNotFound(getErr):
		held()
		m.check(m.cluster.Client.Create(m.ctx, lease, client.FieldOwner(harness.FieldOwner)), "take realm Lease %s", leaseName)
	case getErr != nil:
		m.fatalf("the realm Lease %s could not be read: %v", leaseName, getErr)
	default:
		held()
		m.check(m.cluster.Client.Update(m.ctx, lease, client.FieldOwner(harness.FieldOwner)), "take realm Lease %s", leaseName)
	}

	m.check(m.approve(name+"-approval", name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "approve plan %s", plan.Name)
	contended := m.waitForMigration(name, "the Apply to say it is waiting for Lease "+leaseName, time.Second, func(resource *ptahv1alpha1.PtahMigration) bool {
		return migrationLockWaitReported(resource, leaseName)
	})
	jobName := contended.Status.ActiveOperation.JobName

	// Two more looks find the same Lease, and the claim keeps waiting.
	m.sleep(12 * time.Second)
	jobs := &batchv1.JobList{}
	m.check(m.list(jobs, client.MatchingFields{"metadata.name": jobName}), "look for Job %s", jobName)
	if len(jobs.Items) != 0 {
		m.fatalf("Job %s exists with Lease %s held by another operation", jobName, leaseName)
	}
	if !migrationLockWaitReported(m.migration(name), leaseName) {
		m.fatalf("%s stopped saying it waits for Lease %s while the Lease was still held", name, leaseName)
	}

	// A holder that finishes empties the Lease, as the manager's own release does.
	// The API server dates a manager's entry only when the write gives it a field,
	// and removing the holder gives it none, so the write renews the Lease as well.
	m.check(m.mergePatch(lease, map[string]any{"spec": map[string]any{"holderIdentity": nil, "renewTime": metav1.NowMicro()}}),
		"free realm Lease %s", leaseName)
	free, err := leaseLastWrittenBy(lease, harness.FieldOwner)
	m.check(err, "date the write that freed Lease %s", leaseName)

	settled := m.waitForMigration(name, "the Apply to run once the Lease came free", migrationPoll, func(resource *ptahv1alpha1.PtahMigration) bool {
		return resource.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && resource.Status.ActiveOperation == nil
	})
	job := &batchv1.Job{}
	m.check(m.get(jobName, job), "read Job %s", jobName)
	m.check(jobCreatedPromptlyAfter(job, free, lockWaitBound), "date Job %s against Lease %s", jobName, leaseName)
	if conditionIs(settled.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "WaitingForDatabaseLock") {
		m.fatalf("%s still says it is waiting for the Lease after the Apply ran", name)
	}
	m.logf("PASS %s: an Apply that lost the Lease named it, and its Job was created %s after it came free",
		m.engine.name, job.CreationTimestamp.Sub(free).Round(time.Second))
}
