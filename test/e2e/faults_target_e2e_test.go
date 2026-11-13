//go:build e2e

package e2e

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
		// The proof the refused Apply owes is a read, and the runner refuses it
		// against the substituted target before the executor starts, so that
		// server sees nothing from the proof Pod either (#893).
		f.startReadBarrier()
		f.mustResumeStatusWrites("restore status writes after the target-binding refusal")
		proof := f.waitForSchema(name, "the post-Apply proof read claimed behind the scheduling barrier", func(resource *ptahv1alpha1.PtahSchema) bool {
			return proofReadClaimed(resource, operationID)
		}).Status.ActiveOperation
		proofJob, proofUID := proof.JobName, string(proof.JobUID)
		f.assertReadBlocked(proofUID, "the post-Apply proof read of the substituted target")
		f.pauseStatusWrites()
		beforeProof := audit.snapshot()
		f.stopReadBarrier()
		proofRead := f.captureExactJobResult(proofJob, proofUID, strings.ToLower(string(proof.Type)))
		if !schemaRetargetRefused(proofRead.result, old.Spec.TargetIdentityDigest) {
			f.fatalf("%s read the substituted target as the proof of its Apply", name)
		}
		audit.assertRecords(beforeProof, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": proofJob}, proofUID), false)
		f.mustResumeStatusWrites("restore status writes after the refused proof read")
		f.waitForSchema(name, "the substituted target to be refused as post-Apply proof", func(resource *ptahv1alpha1.PtahSchema) bool {
			return retargetProofRefused(resource, ptahv1alpha1.PendingObservationOutcomeUnknown, operationID, jobUID, old.Spec.TargetIdentityDigest)
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

// targetLoginChanges is what a store does when it moves a resource to another
// login: the database stays and the plan's target identity does not. The proof the
// Apply owes cannot be read under the new login, so the resource stops, and each
// row takes one of the two ways out: a person settles an Apply the runner refused,
// and an Apply that finished is proved once the first login is back.
func (f *faultRun) targetLoginChanges() {
	f.t.Helper()
	_, err := f.execIn(pgService, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atqc `+
			`"CREATE ROLE $1 LOGIN SUPERUSER PASSWORD '$POSTGRES_PASSWORD'"`,
		"sh", targetChangeLogin)
	f.check(err, "create the second login %s", targetChangeLogin)
	f.refusedApplySettledUnderANewLogin()
	f.finishedApplyProvedUnderTheFirstLogin()
}

// runPrintedSettleCommand reads the command from the resource's condition and runs
// it, as a person does, rather than writing what the row expects it to say.
func (f *faultRun) runPrintedSettleCommand(name, operationID string) {
	f.t.Helper()
	resource := &ptahv1alpha1.PtahSchema{}
	f.check(f.get(name, resource), "read PtahSchema %s", name)
	failed := meta.FindStatusCondition(resource.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
	if failed == nil {
		f.fatalf("%s reports no failure to read a command from", name)
	}
	arguments, err := printedSettleCommand(failed.Message, name, f.in.TestNamespace, operationID)
	f.check(err, "read the command %s prints", name)
	if _, stderr, err := f.cluster.Kubectl(f.ctx, arguments...); err != nil {
		f.fatalf("the command %s prints failed: %v: %s", name, err, strings.TrimSpace(string(stderr)))
	}
}

// setTargetURL rewrites the URL a resource's Secret holds, as a store does.
func (f *faultRun) setTargetURL(secret, url, description string) {
	f.t.Helper()
	object := &corev1.Secret{}
	object.Namespace, object.Name = f.in.TestNamespace, secret
	patch := f.jsonBytes(map[string]any{"stringData": map[string]any{"url": url}})
	if err := f.cluster.Client.Patch(f.ctx, object, client.RawPatch(types.MergePatchType, patch)); err != nil {
		f.fatalf("could not %s", description)
	}
}

// loginURL is the database's URL under the second login.
func (f *faultRun) loginURL(database string) string {
	f.t.Helper()
	url, err := urlWithLogin(f.databaseURLFor("postgresql", database), targetChangeLogin)
	f.check(err, "name the second login in the URL of %s", database)
	return url
}

// refusedApplySettledUnderANewLogin moves the login behind an approved Apply's
// Pod, so the runner refuses it and the proof reads the new login. The Apply's ID,
// which a person can read from status at any time and writes here before the move,
// settles nothing. The command the stop prints does, and it has to replace that
// value to run at all. The resource reads the database afresh, its next plan binds
// the new login and runs on an approval of its own, and the first approval does not
// carry over.
func (f *faultRun) refusedApplySettledUnderANewLogin() {
	f.t.Helper()
	const name, database, secret = "e2e-target-login-settled", "e2e_target_login_settled", "e2e-target-login-settled-db"
	f.createDatabase("postgresql", database, secret)
	before := f.fingerprint("postgresql", database, "the database under the first login")
	f.createSchema(faultSchema{name: name, engine: "PostgreSQL", reference: f.pgReference, secret: secret, coordinationKey: "e2e/target-login/settled"})
	old := f.schemaPlan(f.waitForPlan(name))
	f.startReadBarrier()
	f.createApproval(name, name+"-first")
	active := f.waitForSchema(name, "the Apply claimed behind the scheduling barrier", applyDispatched).Status.ActiveOperation
	operationID, jobUID := active.ID, string(active.JobUID)
	f.assertReadBlocked(jobUID, "the approved Apply before its login changes")
	f.patchSchema(name, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		ptahv1alpha1.SettledApplyAnnotation: operationID,
	}}})
	f.setTargetURL(secret, f.loginURL(database), "move the Secret to the second login")
	f.stopReadBarrier()

	blocked := f.waitForSchema(name, "the proof to stop on the new login", func(resource *ptahv1alpha1.PtahSchema) bool {
		return retargetProofRefused(resource, ptahv1alpha1.PendingObservationOutcomeUnknown, operationID, jobUID, old.Spec.TargetIdentityDigest)
	})
	if blocked.Status.Applied != nil {
		f.fatalf("%s credited an Apply nothing proved: %+v", name, blocked.Status.Applied)
	}
	if f.addedJobCount(name, "apply") != 1 {
		f.fatalf("%s replayed its Apply under the new login", name)
	}
	if f.fingerprint("postgresql", database, "the database after the refused Apply") != before {
		f.fatalf("%s changed the database under an approval of the first login", name)
	}

	// Blocked with the Apply's ID on it: a read that begins after this document has
	// had the value in front of it and still finds the proof owed.
	f.waitForSchema(name, "a later read to find the proof still owed", func(resource *ptahv1alpha1.PtahSchema) bool {
		return proofReadAgain(resource, operationID, blocked.Status.ActiveOperation.Attempt)
	})
	f.runPrintedSettleCommand(name, operationID)
	current := f.waitForSchema(name, "a plan under the new login", func(resource *ptahv1alpha1.PtahSchema) bool {
		return planUnderAnotherTarget(resource, old.Spec.TargetIdentityDigest)
	})
	if !proofSettledInJournal(current, old.Name) {
		f.fatalf("%s does not record that nobody established the Apply: %+v", name, current.Status.Journal)
	}
	if current.Status.Applied != nil {
		f.fatalf("%s credited an Apply after its proof was given up: %+v", name, current.Status.Applied)
	}
	if f.addedJobCount(name, "apply") != 1 {
		f.fatalf("%s ran an Apply with no approval of the new login's plan", name)
	}
	f.assertColumn("postgresql", database, "fault_token", 0)

	f.createApproval(name, name+"-second")
	f.waitForSchema(name, "the new login's plan to converge", freshApprovalConverged)
	f.assertColumn("postgresql", database, "fault_token", 1)
	if f.addedJobCount(name, "apply") != 2 {
		f.fatalf("%s did not run exactly the refused Apply and the freshly approved one", name)
	}
	f.logf("PASS a login moved after approval: the runner refused, the proof stopped, a person settled it, and a fresh approval ran under the new login")
}

// finishedApplyProvedUnderTheFirstLogin moves the login after the Apply's Pod has
// run to its end and before the manager can read the result, which is the Apply
// nothing could credit: the statement finished, and every read named another
// target. Putting the first login back proves it and credits it.
func (f *faultRun) finishedApplyProvedUnderTheFirstLogin() {
	f.t.Helper()
	const name, database, secret = "e2e-target-login-restored", "e2e_target_login_restored", "e2e-target-login-restored-db"
	f.createDatabase("postgresql", database, secret)
	first := f.databaseURLFor("postgresql", database)
	f.createSchema(faultSchema{name: name, engine: "PostgreSQL", reference: f.pgReference, secret: secret, coordinationKey: "e2e/target-login/restored"})
	old := f.schemaPlan(f.waitForPlan(name))
	f.startReadBarrier()
	f.createApproval(name, name+"-first")
	active := f.waitForSchema(name, "the Apply claimed behind the scheduling barrier", applyDispatched).Status.ActiveOperation
	jobName, operationID, jobUID := active.JobName, active.ID, string(active.JobUID)
	f.assertReadBlocked(jobUID, "the approved Apply before it runs")
	f.pauseStatusWrites()
	f.stopReadBarrier()
	applied := f.captureExactJobResult(jobName, jobUID, "apply")
	f.check(automaticApplyResult(applied.result, old.Spec.ContentDigest, old.Spec.CoordinationDigest, old.Spec.TargetIdentityDigest),
		"hold the finished Apply to its plan")
	f.setTargetURL(secret, f.loginURL(database), "move the Secret to the second login")
	f.mustResumeStatusWrites("restore status writes after the Apply finished")

	blocked := f.waitForSchema(name, "the proof of a finished Apply to stop on the new login", func(resource *ptahv1alpha1.PtahSchema) bool {
		return retargetProofRefused(resource, ptahv1alpha1.PendingObservationApplySucceeded, operationID, jobUID, old.Spec.TargetIdentityDigest)
	})
	if blocked.Status.Applied != nil {
		f.fatalf("%s credited an Apply from a read of another target: %+v", name, blocked.Status.Applied)
	}
	f.assertColumn("postgresql", database, "fault_token", 1)

	f.setTargetURL(secret, first, "put the first login back")
	f.waitForSchema(name, "the proof to finish under the first login", func(resource *ptahv1alpha1.PtahSchema) bool {
		return proofFinishedUnderItsTarget(resource, old.Spec.Fingerprint)
	})
	if f.addedJobCount(name, "apply") != 1 {
		f.fatalf("%s ran a second Apply", name)
	}
	f.logf("PASS a login moved after an Apply finished: the proof stopped, and putting the first login back proved and credited the Apply")
}

// mysqlDriftBeforeDispatch changes a MySQL database by hand while its approved
// Apply waits behind the scheduling barrier: the runner refuses the stale plan
// without running SQL, and a fresh plan of the changed database runs on an
// approval of its own.
//
// It connects as an account that holds the grant support/databases.md shows on
// the database and nothing else. root drops the column.
func (f *faultRun) mysqlDriftBeforeDispatch() {
	f.t.Helper()
	const name, database, secret = "e2e-mysql-approved-drift", "e2e_mysql_approved_drift", "e2e-mysql-approved-drift-db"
	f.createDocumentedDatabase("mysql", database, secret)
	f.query("mysql", database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'drift-control', 'preserve-this-row')")
	audit := &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: "mysql"}
	user := f.schemaMySQLDocumentedAccount(audit, database, secret)
	f.createSchema(faultSchema{name: name, engine: "MySQL", reference: f.mysqlReference, secret: secret, coordinationKey: "e2e/mysql-approved-drift"})
	old := f.schemaPlan(f.waitForPlan(name))
	observeBefore, planBefore := f.checkpointJobs(name, "observe"), f.checkpointJobs(name, "plan")
	f.startReadBarrier()
	f.createApproval(name, name+"-original")
	active := f.waitForSchema(name, "the approved Apply behind the scheduling barrier", applyDispatched).Status.ActiveOperation
	jobName, jobUID := active.JobName, string(active.JobUID)
	f.assertReadBlocked(jobUID, "the MySQL Apply before external drift")
	f.query("mysql", database, "ALTER TABLE e2e_widgets DROP COLUMN enabled")
	f.assertColumn("mysql", database, "enabled", 0)
	changed := f.fingerprint("mysql", database, "MySQL after external drift")
	f.pauseStatusWrites()
	window := f.startSchemaDriftWindow(name, database, user, audit, jobUID)
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
	current := window.waitForSchema("a fresh plan after MySQL drift", func(resource *ptahv1alpha1.PtahSchema) bool {
		return planAwaitingApproval(resource) && resource.Status.Plan.UID != old.UID
	})
	fresh := f.schemaPlan(current.Status.Plan.Name)
	if !manualFreshPlan(fresh, string(old.UID), old.Spec.ActualStateFingerprint) {
		f.fatalf("the MySQL recovery did not bind a fresh actual state")
	}
	if f.addedJobCount(name, "apply") != 1 {
		f.fatalf("the stale MySQL Apply was replayed")
	}
	f.assertApprovalConsumed(name+"-original", string(old.UID))
	if f.fingerprint("mysql", database, "MySQL after recovery observations") != changed {
		f.fatalf("MySQL recovery observations changed the database")
	}
	observe := window.resultControl(current, "observe", observeBefore)
	plan := window.resultControl(current, "plan", planBefore)
	f.retainSchemaSQLWatch(window)
	window.assertStale(current, operationSQLClient{
		resourceUID: string(current.UID), jobUID: jobUID, podUID: refused.podUID, operation: "apply",
	}, observe, plan)
	beforeSQL, beforeApply := audit.snapshot(), f.checkpointJobs(name, "apply")
	f.createApproval(name, name+"-current")
	f.waitForApprovedPlanConverged(name, fresh.Spec.ArtifactDigest, fresh.Spec.Fingerprint,
		string(fresh.UID), "the freshly approved MySQL drift plan to converge")
	result := f.captureOneNewJobResult(name, "apply", beforeApply, nil)
	f.check(automaticApplyResult(result, fresh.Spec.ContentDigest, fresh.Spec.CoordinationDigest, fresh.Spec.TargetIdentityDigest),
		"bind MySQL drift recovery to its exact approved plan")
	completed := f.captured
	if completed.jobUID == jobUID || completed.podUID == refused.podUID {
		f.fatalf("MySQL drift recovery reused the refused Apply identity")
	}
	audit.assertRecords(beforeSQL, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": completed.jobName}, completed.jobUID), true)
	audit.close()
	f.dataPlane.assertApprovalConsumed(name+"-current", string(fresh.UID))
	f.waitForWatchCountAbove(name, "apply", 1, "the fresh MySQL Apply to enter the retained watch")
	f.assertColumn("mysql", database, "enabled", 1)
	f.assertColumn("mysql", database, "fault_token", 1)
	if f.query("mysql", database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='drift-control' AND note='preserve-this-row'") != "1" ||
		f.query("mysql", database, "SELECT count(*) FROM e2e_widgets") != "1" {
		f.fatalf("MySQL drift recovery changed the preserved row")
	}
	f.assertOneNewJob(name, "apply", beforeApply)
	if f.addedJobCount(name, "apply") != 2 {
		f.fatalf("MySQL did not run exactly the refused and freshly approved Jobs")
	}
	f.logf("PASS MySQL drift after approval: stale plan refused; fresh approval applied against the changed database")
}
