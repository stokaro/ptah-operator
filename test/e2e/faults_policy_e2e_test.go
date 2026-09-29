//go:build e2e

package e2e

import ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"

// destructivePolicyChanges uses isolated copies of the populated v3 databases
// and the v4 artifacts already published by each engine lifecycle. Revoking
// allowDestructive after admission must stop the approved destructive change.
func (f *faultRun) destructivePolicyChanges() {
	f.t.Helper()
	for _, engine := range []string{"postgresql", "mysql"} {
		name := "e2e-destructive-policy-" + engine
		database := "e2e_destructive_policy"
		secret := name + "-db"
		kind, dialect := "PostgreSQL", "postgres"
		if engine == "mysql" {
			kind, dialect = "MySQL", "mysql"
		}
		f.createDatabase(engine, database, secret)
		f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'policy-control', 'preserve-before-approval')")
		beforeDatabase := f.fingerprint(engine, database, "the populated database before destructive-policy changes")
		sqlWindow := f.startSchemaRefusalWindow(name, engine, database, secret)
		initialJobs := f.checkpointJobs(name, "")
		digest := f.schema("e2e-" + engine).Status.Source.Digest
		f.createSchema(faultSchema{
			name: name, engine: kind, reference: f.registryReference(engine), secret: secret,
			coordinationKey: "e2e/destructive-policy/" + engine, allowDestructive: true,
		})
		generation := f.schema(name).Generation
		approved := sqlWindow.waitForSchema("a destructive plan awaiting approval", func(schema *ptahv1alpha1.PtahSchema) bool {
			return destructivePolicyGate(schema, generation, true)
		})
		old := f.schemaPlan(approved.Status.Plan.Name)
		f.check(committedPlan(old, name, digest, dialect, true, f.controller, f.stateVersion()), "read the original destructive plan")
		initialObserve := sqlWindow.resultControl(approved, "observe", initialJobs)
		initialPlan := sqlWindow.resultControl(approved, "plan", initialJobs)
		revokedJobs := f.checkpointJobs(name, "")
		beforeApply := f.checkpointOperationWatch(name, "apply", 0)
		f.pauseStatusWrites()
		f.createApproval(name, name+"-original")
		if f.schema(name).Status.ActiveOperation != nil {
			f.fatalf("%s claimed an operation before destructive permission was revoked", name)
		}
		f.checkpointOperationWatch(name, "apply", 0)
		f.patchSchema(name, map[string]any{"spec": map[string]any{"policy": map[string]any{"allowDestructive": false}}})
		revokedGeneration := f.schema(name).Generation
		if revokedGeneration <= generation {
			f.fatalf("%s revocation did not advance the generation", name)
		}
		f.mustResumeStatusWrites("resume after revoking destructive permission")
		refused := sqlWindow.waitForSchema("the revoked destructive permission to refuse the admitted decision", func(schema *ptahv1alpha1.PtahSchema) bool {
			return destructivePolicyGate(schema, revokedGeneration, false) && schema.Status.Plan.UID != old.UID
		})
		blocked := f.schemaPlan(refused.Status.Plan.Name)
		revokedPlan := sqlWindow.resultControl(refused, "plan", revokedJobs)
		f.check(changedDestructivePolicyPlan(old, blocked), "bind the destructive refusal to the policy edit")
		unconsumed := &ptahv1alpha1.PtahSchemaApproval{}
		f.check(f.get(name+"-original", unconsumed), "read the refused destructive approval")
		if conditionStatus(unconsumed.Status.Conditions, "Consumed", "True") {
			f.fatalf("%s consumed approval after destructive permission was revoked", name)
		}
		f.checkpointOperationWatch(name, "apply", 0)
		// Keep one uninterrupted SQL window across refusal and restoration.
		// The final database assertions run after every received statement has
		// been checked; attempted or rolled-back writes cannot hide in between.
		restoredJobs := f.checkpointJobs(name, "")

		// Do not restore the exact originally approved policy: this control
		// requires a new decision, with a different lock timeout.
		f.patchSchema(name, map[string]any{"spec": map[string]any{"policy": map[string]any{
			"allowDestructive": true, "lockTimeout": "45s",
		}}})
		freshGeneration := f.schema(name).Generation
		if freshGeneration <= revokedGeneration {
			f.fatalf("%s restoration did not advance the generation", name)
		}
		current := sqlWindow.waitForSchema("a fresh destructive approval gate", func(schema *ptahv1alpha1.PtahSchema) bool {
			return destructivePolicyGate(schema, freshGeneration, true) && schema.Status.Plan.UID != old.UID && schema.Status.Plan.UID != blocked.UID
		})
		fresh := f.schemaPlan(current.Status.Plan.Name)
		restoredPlan := sqlWindow.resultControl(current, "plan", restoredJobs)
		f.check(committedPlan(fresh, name, digest, dialect, true, f.controller, f.stateVersion()), "read the fresh destructive plan")
		f.check(changedDestructivePolicyPlan(old, fresh), "bind the fresh decision to the changed destructive policy")
		// The disabled policy stops before approval lookup. Stale cleanup is
		// observable once the restored policy reaches its new approval gate.
		f.waitForApproval(name+"-original", "the previous destructive decision to become stale", func(approval *ptahv1alpha1.PtahSchemaApproval) bool {
			return conditionIs(approval.Status.Conditions, "Stale", "True", "PlanNoLongerCurrent")
		})
		f.checkpointOperationWatch(name, "apply", 0)
		sqlWindow.assert(current, initialObserve, initialPlan, revokedPlan, restoredPlan)
		f.assertDestructivePolicyDatabaseUnchanged(engine, database, beforeDatabase)
		assertSQL := f.approvedSchemaSQLControl(name, engine,
			currentPlan{name: fresh.Name, uid: string(fresh.UID), fingerprint: fresh.Spec.Fingerprint}, beforeApply)
		f.createApproval(name, name+"-current")
		f.waitForApprovedPlanConverged(name, digest, fresh.Spec.Fingerprint, string(fresh.UID), "the freshly approved destructive plan to converge")
		f.assertApprovalConsumed(name+"-current", string(fresh.UID))
		assertSQL()
		f.waitForWatchCountAbove(name, "apply", 0, "the freshly approved destructive Apply to enter the watch")
		f.checkpointOperationWatch(name, "apply", 1)
		if f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='policy-control'") != "1" ||
			f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" {
			f.fatalf("%s destructive Apply changed the preserved row", name)
		}
		if engine == "postgresql" {
			f.assertColumn(engine, database, "note", 0)
			f.assertColumn(engine, database, "enabled", 0)
		} else {
			if f.query(engine, database, "SELECT count(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND index_name='e2e_widgets_name_idx'") != "0" ||
				f.query(engine, database, "SELECT count(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND index_name='e2e_widgets_name_unique'") != "1" ||
				f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE note='preserve-before-approval'") != "1" {
				f.fatalf("%s did not drop only the approved index while preserving the row and unique index", name)
			}
			f.assertColumn(engine, database, "note", 1)
			f.assertColumn(engine, database, "enabled", 1)
		}
		f.logf("PASS %s revoked destructive permission after approval: no Apply or database change; one fresh approval executed the destructive plan and preserved the row", engine)
	}
}

func (f *faultRun) assertDestructivePolicyDatabaseUnchanged(engine, database, fingerprint string) {
	f.t.Helper()
	if f.fingerprint(engine, database, "the database under revoked destructive authority") != fingerprint ||
		f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" ||
		f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='policy-control' AND note='preserve-before-approval'") != "1" {
		f.fatalf("%s destructive-policy change mutated the database before fresh approval", engine)
	}
}
