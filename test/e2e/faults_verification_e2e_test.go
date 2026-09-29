//go:build e2e

package e2e

import ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"

func (f *faultRun) verificationPolicyChanges() {
	f.t.Helper()
	for _, engine := range []string{"postgresql", "mysql"} {
		for _, change := range []string{"uid", "content"} {
			name, database := "e2e-verify-policy-"+change+"-"+engine, "e2e_verify_policy_"+change
			mode, secret := "verification-policy-"+change, name+"-db"
			kind, reference := "PostgreSQL", f.pgReference
			if engine == "mysql" {
				kind, reference = "MySQL", f.mysqlReference
			}
			fixture := newVerificationPolicyFixture(f.t, f.ctx, f.cluster, f.in.TestNamespace, name+"-policy", schemaArtifactType)
			originalPolicy := fixture.identity(verificationPolicyKey)
			f.createDatabase(engine, database, secret)
			f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'verification-control', 'preserved-policy-row')")
			beforeDatabase := f.fingerprint(engine, database, "the database before verification policy replacement")
			f.createSchema(faultSchema{
				name: name, engine: kind, reference: reference, secret: secret,
				coordinationKey: "e2e/verify-policy/" + change + "/" + engine, verificationPolicy: fixture.object.Name,
			})
			old := f.schemaPlan(f.waitForPlan(name))
			before := f.schema(name)
			beforeApply := f.checkpointOperationWatch(name, "apply", 0)
			f.pauseStatusWrites()
			f.createApproval(name, name+"-original")
			if f.schema(name).Status.ActiveOperation != nil {
				f.fatalf("%s claimed an operation before its verification policy changed", name)
			}
			f.checkpointOperationWatch(name, "apply", 0)
			var currentPolicy verificationPolicyIdentity
			if change == "uid" {
				fixture.replaceIdentity()
				currentPolicy = fixture.identity(verificationPolicyKey)
			} else {
				f.patchSchema(name, map[string]any{"spec": map[string]any{"desired": map[string]any{
					"verificationPolicyFrom": map[string]any{"key": narrowedPolicyKey},
				}}})
				currentPolicy = fixture.identity(narrowedPolicyKey)
			}
			changed := f.schema(name)
			if change == "uid" && changed.Generation != before.Generation {
				f.fatalf("%s policy UID replacement depended on a resource generation change", name)
			}
			if change == "content" && changed.Generation <= before.Generation {
				f.fatalf("%s selected-policy edit did not advance the generation", name)
			}
			f.mustResumeStatusWrites("resume after the verification policy changed")
			current := f.waitForSchema(name, "a fresh plan verified under the current policy", func(schema *ptahv1alpha1.PtahSchema) bool {
				return changedSchemaApprovalRefused(schema, string(old.UID), changed.Generation)
			})
			fresh := f.schemaPlan(current.Status.Plan.Name)
			f.check(changedVerificationPolicyDecision(schemaVerificationDecision(old), schemaVerificationDecision(fresh),
				originalPolicy, currentPolicy, mode), "bind the fresh schema plan to the changed verification policy")
			f.waitForApproval(name+"-original", "the old verification policy's approval to become stale", func(approval *ptahv1alpha1.PtahSchemaApproval) bool {
				return conditionIs(approval.Status.Conditions, "Stale", "True", "PlanNoLongerCurrent")
			})
			f.checkpointOperationWatch(name, "apply", 0)
			if f.fingerprint(engine, database, "the database under the obsolete verification approval") != beforeDatabase {
				f.fatalf("%s changed the database under the old policy approval", name)
			}
			f.assertColumn(engine, database, "fault_token", 0)
			f.assertVerificationPolicyRow(engine, database)
			assertSQL := f.approvedSchemaSQLControl(name, engine,
				currentPlan{name: fresh.Name, uid: string(fresh.UID), fingerprint: fresh.Spec.Fingerprint}, beforeApply)
			f.createApproval(name, name+"-current")
			f.waitForApprovedPlanConverged(name, fresh.Spec.ArtifactDigest, fresh.Spec.Fingerprint, string(fresh.UID), "the newly verified and approved schema plan to converge")
			f.assertApprovalConsumed(name+"-current", string(fresh.UID))
			assertSQL()
			f.waitForWatchCountAbove(name, "apply", 0, "the freshly approved verified Apply to enter the watch")
			f.checkpointOperationWatch(name, "apply", 1)
			f.assertColumn(engine, database, "fault_token", 1)
			f.assertVerificationPolicyRow(engine, database)
			f.logf("PASS %s verification policy %s changed after approval: old authority refused; fresh verification and approval applied once and preserved the row", engine, change)
		}
	}
}

func (f *faultRun) assertVerificationPolicyRow(engine, database string) {
	f.t.Helper()
	if f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" ||
		f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='verification-control' AND note='preserved-policy-row'") != "1" {
		f.fatalf("%s verification policy change did not preserve the populated row", engine)
	}
}
