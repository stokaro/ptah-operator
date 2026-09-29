//go:build e2e

package e2e

import ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"

func (f *faultRun) exclusionPolicyChanges() {
	f.t.Helper()
	for _, engine := range []string{"postgresql", "mysql"} {
		name, database := "e2e-exclusion-policy-"+engine, "e2e_exclusion_policy"
		secret := name + "-db"
		kind, dialect, reference := "PostgreSQL", "postgres", f.pgReference
		if engine == "mysql" {
			kind, dialect, reference = "MySQL", "mysql", f.mysqlReference
		}
		f.createDatabase(engine, database, secret)
		f.query(engine, database, "CREATE TABLE "+excludedPolicyTable+" (id bigint NOT NULL PRIMARY KEY, note varchar(255)); INSERT INTO "+excludedPolicyTable+" VALUES (701, 'outside-managed-scope')")
		f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'scope-control', 'preserved-managed-row')")
		beforeDatabase := f.fingerprint(engine, database, "the database before narrowing the managed scope")
		f.createSchema(faultSchema{name: name, engine: kind, reference: reference, secret: secret, coordinationKey: "e2e/exclusion-policy/" + engine})
		f.patchSchema(name, map[string]any{"spec": map[string]any{"policy": map[string]any{"allowDestructive": true}}})
		generation := f.schema(name).Generation
		approved := f.waitForSchema(name, "a destructive plan including the table later excluded", func(schema *ptahv1alpha1.PtahSchema) bool {
			return destructivePolicyGate(schema, generation, true)
		})
		old := f.schemaPlan(approved.Status.Plan.Name)
		digest := approved.Status.Source.Digest
		f.check(committedPlan(old, name, digest, dialect, true, f.controller, f.stateVersion()), "read the original scope's plan")
		oldDocument := f.exclusionPlanDocument(old)
		beforeApply := f.checkpointOperationWatch(name, "apply", 0)
		f.pauseStatusWrites()
		f.createApproval(name, name+"-original")
		if f.schema(name).Status.ActiveOperation != nil {
			f.fatalf("%s claimed an operation before the exclusion changed", name)
		}
		f.checkpointOperationWatch(name, "apply", 0)
		f.patchSchema(name, map[string]any{"spec": map[string]any{"policy": map[string]any{"exclude": []string{excludedPolicyTable}}}})
		changedGeneration := f.schema(name).Generation
		if changedGeneration <= generation {
			f.fatalf("%s exclusion edit did not advance the generation", name)
		}
		f.mustResumeStatusWrites("resume after narrowing the approved managed scope")
		current := f.waitForSchema(name, "a fresh decision for the narrowed scope", func(schema *ptahv1alpha1.PtahSchema) bool {
			return changedSchemaApprovalRefused(schema, string(old.UID), changedGeneration) && !schema.Status.Plan.Destructive
		})
		fresh := f.schemaPlan(current.Status.Plan.Name)
		f.check(committedPlan(fresh, name, digest, dialect, false, f.controller, f.stateVersion()), "read the narrowed scope's plan")
		f.check(changedExcludedPolicyPlan(old, fresh), "bind the new approval to the changed scope")
		f.check(excludedPolicyDocuments(oldDocument, f.exclusionPlanDocument(fresh)), "verify the excluded table's DROP was removed")
		f.waitForApproval(name+"-original", "the wider-scope approval to become stale", func(approval *ptahv1alpha1.PtahSchemaApproval) bool {
			return conditionIs(approval.Status.Conditions, "Stale", "True", "PlanNoLongerCurrent")
		})
		f.checkpointOperationWatch(name, "apply", 0)
		if f.fingerprint(engine, database, "the unchanged database under the obsolete scope approval") != beforeDatabase {
			f.fatalf("%s changed the database before approval of the narrowed scope", name)
		}
		f.assertExclusionPolicyRows(engine, database)
		f.assertColumn(engine, database, "fault_token", 0)
		assertSQL := f.approvedSchemaSQLControl(name, engine,
			currentPlan{name: fresh.Name, uid: string(fresh.UID), fingerprint: fresh.Spec.Fingerprint}, beforeApply)
		f.createApproval(name, name+"-current")
		f.waitForApprovedPlanConverged(name, digest, fresh.Spec.Fingerprint, string(fresh.UID), "the freshly approved narrowed scope to converge")
		f.assertApprovalConsumed(name+"-current", string(fresh.UID))
		assertSQL()
		f.waitForWatchCountAbove(name, "apply", 0, "the narrowed scope's Apply to enter the watch")
		f.checkpointOperationWatch(name, "apply", 1)
		f.assertColumn(engine, database, "fault_token", 1)
		f.assertExclusionPolicyRows(engine, database)
		f.logf("PASS %s scope narrowed after approval: obsolete DROP never applied; fresh approval added the managed column and preserved both rows and the excluded table", engine)
	}
}

func (f *faultRun) exclusionPlanDocument(plan *ptahv1alpha1.PtahSchemaPlan) planDocument {
	f.t.Helper()
	raw, _ := f.rebuildPlanDocument(plan)
	f.scan(raw, "the exclusion policy plan")
	document, err := parsePlanDocument(raw)
	f.check(err, "parse the exclusion policy plan")
	f.check(planBoundToDocument(plan, document, sha256Digest(raw)), "bind the exclusion policy plan to its native document")
	return document
}

func (f *faultRun) assertExclusionPolicyRows(engine, database string) {
	f.t.Helper()
	if f.query(engine, database, "SELECT count(*) FROM "+excludedPolicyTable) != "1" ||
		f.query(engine, database, "SELECT count(*) FROM "+excludedPolicyTable+" WHERE id=701 AND note='outside-managed-scope'") != "1" ||
		f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" ||
		f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='scope-control' AND note='preserved-managed-row'") != "1" {
		f.fatalf("%s exclusion policy did not preserve the managed and excluded rows", engine)
	}
}
