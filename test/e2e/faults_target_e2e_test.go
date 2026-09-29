//go:build e2e

package e2e

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// targetSecretChanges holds each approved Apply Pod before it can start,
// rewrites its URL Secret, and requires the runner's target-binding refusal.
// The original and substituted databases start alike, so a schema diff alone
// cannot detect this change of authority.
func (f *faultRun) targetSecretChanges() {
	f.t.Helper()
	for _, engine := range []string{"postgresql", "mysql"} {
		name := "e2e-target-secret-" + engine
		database, other := "e2e_target_secret", "e2e_target_secret_other"
		secret := name + "-db"
		f.createDatabase(engine, database, secret)
		f.createDatabase(engine, other, name+"-other-db")
		kind, reference := "PostgreSQL", f.pgReference
		if engine == "mysql" {
			kind, reference = "MySQL", f.mysqlReference
		}
		before := f.fingerprint(engine, database, "the originally approved database")
		otherBefore := f.fingerprint(engine, other, "the database substituted after approval")
		audit := &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: engine}
		initial := audit.snapshot()
		f.createSchema(faultSchema{name: name, engine: kind, reference: reference, secret: secret, coordinationKey: "e2e/target-secret/" + engine})
		old := f.schemaPlan(f.waitForPlan(name))
		planned := audit.snapshot()
		audit.assertRecords(initial, planned, audit.terminalPod(map[string]string{labelSchema: name, labelOperation: "plan"}, ""), true)
		f.startReadBarrier()
		f.createApproval(name, name+"-original")
		active := f.waitForSchema(name, "the Apply claimed behind the scheduling barrier", applyDispatched).Status.ActiveOperation
		jobName, jobUID, operationID := active.JobName, string(active.JobUID), active.ID
		f.assertReadBlocked(jobUID, "the approved Apply before its target Secret changes")
		// Admission has already bound the approval and the Job is immutable.
		// The Pod has not reached a node and cannot have read the old Secret.
		object := &corev1.Secret{}
		object.Namespace, object.Name = f.in.TestNamespace, secret
		patch := f.jsonBytes(map[string]any{"stringData": map[string]any{"url": f.databaseURLFor(engine, other)}})
		if err := f.cluster.Client.Patch(f.ctx, object, client.RawPatch(types.MergePatchType, patch)); err != nil {
			f.fatalf("could not rewrite the isolated %s target Secret", engine)
		}
		f.pauseStatusWrites()
		beforeRefusal := audit.snapshot()
		f.stopReadBarrier()
		refused := f.captureExactJobResult(jobName, jobUID, "apply")
		if refused.operationID != operationID || !schemaRetargetRefused(refused.result, old.Spec.TargetIdentityDigest) {
			f.fatalf("%s did not return the target-binding refusal before invoking the executor", name)
		}
		audit.assertRecords(beforeRefusal, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": jobName}, jobUID), false)
		if f.fingerprint(engine, database, "original database after refusal") != before ||
			f.fingerprint(engine, other, "substituted database after refusal") != otherBefore {
			f.fatalf("%s changed a database under the obsolete approval", name)
		}
		f.assertColumn(engine, database, "fault_token", 0)
		f.assertColumn(engine, other, "fault_token", 0)
		f.mustResumeStatusWrites("restore status writes after the target-binding refusal")
		f.waitForSchema(name, "the substituted target to be refused as post-Apply proof", func(resource *ptahv1alpha1.PtahSchema) bool {
			return retargetProofRefused(resource, operationID, jobUID, old.Spec.TargetIdentityDigest)
		})
		if f.addedJobCount(name, "apply") != 1 {
			f.fatalf("%s replayed its Apply while the original target was unavailable", name)
		}
		// A runner's refusal describes one Pod attempt, not every attempt a
		// dispatched Job could have made. Restore the original database so the
		// controller can finish the read-only proof it still owes that target.
		patch = f.jsonBytes(map[string]any{"stringData": map[string]any{"url": f.databaseURLFor(engine, database)}})
		if err := f.cluster.Client.Patch(f.ctx, object, client.RawPatch(types.MergePatchType, patch)); err != nil {
			f.fatalf("could not restore the isolated %s target Secret for post-Apply proof", engine)
		}
		f.waitForSchema(name, "read-only proof of the originally approved target to finish", func(resource *ptahv1alpha1.PtahSchema) bool {
			return targetPlanAwaitingApproval(resource, old.Spec.TargetIdentityDigest)
		})
		if f.fingerprint(engine, database, "original database after post-Apply proof") != before ||
			f.fingerprint(engine, other, "substituted database after post-Apply proof") != otherBefore {
			f.fatalf("%s changed a database while recovering its refused Apply", name)
		}
		// Move the declared target only after the old proof has settled. A new
		// Secret reference advances the generation and requests a new plan
		// immediately instead of waiting for the periodic read of Secret data.
		f.patchSchema(name, map[string]any{"spec": map[string]any{"target": map[string]any{
			"urlFrom": map[string]any{"name": name + "-other-db"},
		}}})
		current := f.waitForSchema(name, "a fresh plan for the changed target", func(resource *ptahv1alpha1.PtahSchema) bool {
			return targetPlanAwaitingApproval(resource, refused.result.TargetIdentityDigest) && resource.Status.Plan.UID != old.UID &&
				resource.Status.ObservedGeneration == resource.Generation
		})
		fresh := f.schemaPlan(current.Status.Plan.Name)
		if fresh.Spec.TargetIdentityDigest == old.Spec.TargetIdentityDigest {
			f.fatalf("%s did not bind its new plan to the substituted target", name)
		}
		if f.addedJobCount(name, "apply") != 1 {
			f.fatalf("%s replayed the obsolete approval", name)
		}
		beforeFresh := audit.snapshot()
		applyBefore := f.checkpointOperationWatch(name, "apply", 1)
		f.createApproval(name, name+"-current")
		f.waitForSchema(name, "a fresh approval of the substituted target to converge", freshApprovalConverged)
		f.waitForWatchCountAbove(name, "apply", 1, "the freshly approved target's Apply to enter the retained watch")
		freshUID := f.waitForOneNewWatchedJob(name, "apply", applyBefore, "the freshly approved target's Apply")
		freshName := f.liveJobName(freshUID, "the freshly approved target's Apply", operationJobs(name, "apply"))
		audit.assertRecords(beforeFresh, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": freshName}, freshUID), true)
		f.assertColumn(engine, database, "fault_token", 0)
		f.assertColumn(engine, other, "fault_token", 1)
		if f.fingerprint(engine, database, "original database after the fresh approval") != before {
			f.fatalf("%s changed the originally approved database", name)
		}
		if f.addedJobCount(name, "apply") != 2 {
			f.fatalf("%s did not execute exactly the refused and freshly approved Jobs", name)
		}
		f.logf("PASS %s target Secret changed after approval: runner refused; original target proved unchanged; fresh approval changed only its newly declared target", engine)
		audit.close()
	}
}

func (f *faultRun) mysqlDriftBeforeDispatch() {
	f.t.Helper()
	const name, database, secret = "e2e-mysql-approved-drift", "e2e_mysql_approved_drift", "e2e-mysql-approved-drift-db"
	f.createDatabase("mysql", database, secret)
	f.createSchema(faultSchema{name: name, engine: "MySQL", reference: f.mysqlReference, secret: secret, coordinationKey: "e2e/mysql-approved-drift"})
	old := f.schemaPlan(f.waitForPlan(name))
	f.startReadBarrier()
	f.createApproval(name, name+"-original")
	active := f.waitForSchema(name, "the approved Apply behind the scheduling barrier", applyDispatched).Status.ActiveOperation
	jobName, jobUID := active.JobName, string(active.JobUID)
	f.assertReadBlocked(jobUID, "the MySQL Apply before external drift")
	f.query("mysql", database, "ALTER TABLE e2e_widgets DROP COLUMN enabled")
	f.assertColumn("mysql", database, "enabled", 0)
	changed := f.fingerprint("mysql", database, "MySQL after external drift")
	f.pauseStatusWrites()
	f.stopReadBarrier()
	refused := f.captureExactJobResult(jobName, jobUID, "apply")
	if err := stalePlanResult(refused.result, old.Spec.ContentDigest, old.Spec.CoordinationDigest, old.Spec.TargetIdentityDigest); err != nil {
		f.fatalf("MySQL did not refuse the exact stale plan: %v", err)
	}
	if f.fingerprint("mysql", database, "MySQL after stale-plan refusal") != changed {
		f.fatalf("the stale MySQL plan changed its database")
	}
	f.assertColumn("mysql", database, "fault_token", 0)
	f.mustResumeStatusWrites("restore status writes after the MySQL stale-plan refusal")
	current := f.waitForSchema(name, "a fresh plan after MySQL drift", func(resource *ptahv1alpha1.PtahSchema) bool {
		return planAwaitingApproval(resource) && resource.Status.Plan.UID != old.UID
	})
	fresh := f.schemaPlan(current.Status.Plan.Name)
	if !manualFreshPlan(fresh, string(old.UID), old.Spec.ActualStateFingerprint) {
		f.fatalf("the MySQL recovery did not bind a fresh actual state")
	}
	if f.addedJobCount(name, "apply") != 1 {
		f.fatalf("the stale MySQL Apply was replayed")
	}
	if f.fingerprint("mysql", database, "MySQL after recovery observations") != changed {
		f.fatalf("MySQL recovery observations changed the database")
	}
	f.createApproval(name, name+"-current")
	f.waitForSchema(name, "the fresh MySQL approval to converge", freshApprovalConverged)
	f.waitForWatchCountAbove(name, "apply", 1, "the fresh MySQL Apply to enter the retained watch")
	f.assertColumn("mysql", database, "enabled", 1)
	f.assertColumn("mysql", database, "fault_token", 1)
	if f.addedJobCount(name, "apply") != 2 {
		f.fatalf("MySQL did not run exactly the refused and freshly approved Jobs")
	}
	f.logf("PASS MySQL drift after approval: stale plan refused; fresh approval applied against the changed database")
}
