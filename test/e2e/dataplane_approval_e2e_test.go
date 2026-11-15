//go:build e2e

package e2e

import (
	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// assertApplyLogNames reads the database's own record of an approved Apply
// and holds it to the approval the Apply consumed and to the journal: one
// apply of the plan with the digest given, every entry naming the username
// admission stamped on the approval, saying admission is where that name came
// from, and carrying the operation ID the journal records for the Apply. The
// approval is read back after the Apply consumed it, so the name the row looks
// for is the one the API server holds and not one the row chose. On MySQL the
// lifecycle's audit account, which holds every privilege on the database,
// writes the log, and the application user reads it.
func (d *dataPlane) assertApplyLogNames(schema, engine, database, approvalName, planDigest string) {
	d.t.Helper()
	approval := &ptahv1alpha1.PtahSchemaApproval{}
	d.check(d.get(approvalName, approval), "read approval %s", approvalName)
	stamped := approval.Spec.Approver.Username
	if !unencodedInTheLog(stamped) {
		d.fatalf("admission stamped approval %s with %q, which the log holds encoded, and this row compares the stamp as it is",
			approvalName, stamped)
	}
	applied, err := finishedRunOf(d.schema(schema).Status.Journal, ptahv1alpha1.JournalApplyFinished, approval.UID)
	if err != nil {
		d.fatalf("%s: the Apply approval %s authorized: %v", schema, approvalName, err)
	}
	query := schemaApplyLogQuery(engine)
	var output string
	if engine == "mysql" {
		output, err = d.mysql(query)
	} else {
		output, err = d.psql(database, query)
	}
	d.check(err, "read ptah_schema_apply_log in %s", database)
	rows, err := parseSchemaApplyLog(output)
	d.check(err, "parse ptah_schema_apply_log in %s", database)
	run := loggedRun{actor: stamped, origin: actorOriginAdmission, reference: applied.OperationID}
	if err := applyLoggedAs(rows, run, planDigest); err != nil {
		d.fatalf("ptah_schema_apply_log in %s does not record %+v for plan digest %s: %v; it holds %+v", database, run, planDigest, err, rows)
	}
	d.logf("PASS %s records the Apply of %s as %q from admission under operation %s in the database's own log",
		schema, planDigest, stamped, applied.OperationID)
}

// approvedSchemaSQLControl starts before approval and returns the check to run
// after convergence. The existing lifecycle assertions verify database effects;
// this binds the received SQL and successful runner result to that exact plan.
func (d *dataPlane) approvedSchemaSQLControl(schema, engine string, selected currentPlan, beforeApply checkpoint) func() {
	d.t.Helper()
	plan := d.schemaPlan(selected.name)
	if selected.uid == "" || selected.fingerprint == "" || string(plan.UID) != selected.uid || plan.Spec.Fingerprint != selected.fingerprint {
		d.fatalf("%s SQL control did not read the selected approval plan", schema)
	}
	audit := &databaseSQLAudit{t: d.t, ctx: d.ctx, cluster: d.cluster, namespace: d.in.TestNamespace, engine: engine}
	beforeSQL := audit.snapshot()
	return func() {
		d.t.Helper()
		// Later lifecycle checks still use the captured Plan workload identity.
		previous := d.captured
		defer func() { d.captured = previous }()
		result := d.captureOneNewJobResult(schema, "apply", beforeApply, nil)
		if err := automaticApplyResult(result, plan.Spec.ContentDigest, plan.Spec.CoordinationDigest, plan.Spec.TargetIdentityDigest); err != nil {
			d.fatalf("%s SQL control did not execute the selected plan: %v", schema, err)
		}
		completed := d.captured
		audit.assertRecords(beforeSQL, audit.snapshot(),
			audit.terminalPod(map[string]string{"job-name": completed.jobName}, completed.jobUID), true)
		audit.close()
	}
}

// changeApprovedSchemaInputs runs before the first allowed Apply. Each edit
// follows an admitted approval while status writes cannot persist an Apply
// claim. The lifecycle then approves the final plan and proves it executes.
func (d *dataPlane) changeApprovedSchemaInputs(schema, key, realm string, window *schemaRefusalWindow) []operationSQLClient {
	d.t.Helper()
	var controls []operationSQLClient
	for _, field := range []string{"policy", "transaction-mode"} {
		before := d.schema(schema)
		oldPlan := d.plan
		jobs := d.checkpointJobs(schema, "")
		d.pauseStatusWrites()
		approval := schema + "-changed-" + field
		d.createExactApproval(schema, oldPlan.name, approval, key, realm)
		if current := d.schema(schema); current.Status.ActiveOperation != nil {
			d.fatalf("%s claimed an operation before changing %s", schema, field)
		}
		policy := map[string]any{"transactionMode": "none"}
		if field == "policy" {
			policy = map[string]any{"lockTimeout": "45s"}
		}
		d.patchSchema(schema, map[string]any{"spec": map[string]any{"policy": policy}})
		generation := d.schema(schema).Generation
		if generation <= before.Generation {
			d.fatalf("%s did not change generation after its %s edit", schema, field)
		}
		d.mustResumeStatusWrites("resume after changing an approved schema input")
		current := window.waitForSchema("a new approval gate after changing "+field,
			func(resource *ptahv1alpha1.PtahSchema) bool {
				return changedSchemaApprovalRefused(resource, oldPlan.uid, generation)
			})
		plan := d.schemaPlan(current.Status.Plan.Name)
		controls = append(controls, window.resultControl(current, "plan", jobs))
		if plan.Spec.Fingerprint == oldPlan.fingerprint {
			d.fatalf("%s reused its approved fingerprint after changing %s", schema, field)
		}
		d.plan = currentPlan{name: plan.Name, uid: string(plan.UID), fingerprint: plan.Spec.Fingerprint}
		d.waitForApproval(approval, "the previous input's approval to become stale",
			func(resource *ptahv1alpha1.PtahSchemaApproval) bool {
				return conditionIs(resource.Status.Conditions, "Stale", "True", "PlanNoLongerCurrent")
			})
		d.assertNoNewJobs(schema, "apply", jobs)
		d.logf("PASS %s %s changed after approval: a new plan requires a fresh decision", schema, field)
	}
	return controls
}
