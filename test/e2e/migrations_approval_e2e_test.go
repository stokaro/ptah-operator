//go:build e2e

package e2e

import (
	"context"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
// policy edit before an Apply is dispatched. The status barrier leaves
// admission available but prevents the controller from persisting a claim.
// A fresh approval then executes the same artifact on the same database.
func (m *migrationRun) approvalInputChange(field string) {
	m.t.Helper()
	suffix := "approval-" + field
	name := "e2e-" + suffix + "-" + m.engine.name
	database := "ptah_e2e_" + strings.ReplaceAll(suffix, "-", "_")
	secret := "e2e-" + m.engine.name + "-" + suffix + "-db"
	m.isolatedDatabase(database, secret)
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/" + suffix + "/" + m.engine.name,
		apply:           "OnApproval", interval: "1h",
	}))
	before := m.waitForMigration(name, "a plan awaiting approval", migrationPoll,
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
	m.check(m.approve(name+"-old", name, oldPlan.Name, string(oldPlan.UID), oldPlan.Spec.Fingerprint),
		"admit the original approval")
	if current := m.migration(name); current.Status.ActiveOperation != nil {
		m.fatalf("%s claimed an operation before the input changed", name)
	}
	m.assertNoNewApplyJob(nil, "before the input changed", name)
	policy := map[string]any{"transactionMode": "none"}
	if field == "policy" {
		policy = map[string]any{"apply": "Never"}
	}
	m.patchMigration(name, map[string]any{"spec": map[string]any{"policy": policy}})
	changed := m.migration(name)
	if changed.Generation <= before.Generation {
		m.fatalf("%s input edit did not change its generation", name)
	}
	m.check(barrier.resume(m.ctx), "resume reconciliation with the changed input")
	refused := m.waitForMigration(name, "the changed input to invalidate the approved plan", migrationPoll,
		func(resource *ptahv1alpha1.PtahMigration) bool {
			return changedMigrationApprovalRefused(resource, oldPlan.UID, changed.Generation, field == "policy")
		})
	newPlan := m.planOf(refused.Status.Plan.Name)
	if newPlan.Spec.Fingerprint == oldPlan.Spec.Fingerprint {
		m.fatalf("%s reused the fingerprint after changing %s", name, field)
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
		refused = m.waitForMigration(name, "a fresh plan under the restored approval gate", migrationPoll,
			func(resource *ptahv1alpha1.PtahMigration) bool {
				return changedMigrationApprovalRefused(resource, oldPlan.UID, generation, false)
			})
		newPlan = m.planOf(refused.Status.Plan.Name)
	}
	m.assertNoNewApplyJob(nil, "before the fresh approval", name)
	m.assertDatabaseUnmigrated(name, database)
	m.check(m.approve(name+"-current", name, newPlan.Name, string(newPlan.UID), newPlan.Spec.Fingerprint),
		"approve the changed input")
	m.waitForGenerationInSync(name)
	if jobs := m.applyJobUIDs(name); len(jobs) != 1 {
		m.fatalf("%s created %d Apply Jobs, want one after the fresh approval", name, len(jobs))
	}
	if count := m.query("SELECT count(*) FROM e2e_migration_widgets", database); count != "3" {
		m.fatalf("%s fresh approval did not seed exactly three rows: %s", name, count)
	}
	if color := m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", database); color != "blue" {
		m.fatalf("%s fresh approval did not apply the final migration: %s", name, color)
	}
	m.logf("PASS %s %s changed after approval: no Apply before a fresh decision; fresh decision applied", m.engine.kind, field)
}
