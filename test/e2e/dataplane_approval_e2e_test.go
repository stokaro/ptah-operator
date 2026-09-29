//go:build e2e

package e2e

import (
	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// changeApprovedSchemaInputs runs before the first allowed Apply. Each edit
// follows an admitted approval while status writes cannot persist an Apply
// claim. The lifecycle then approves the final plan and proves it executes.
func (d *dataPlane) changeApprovedSchemaInputs(schema, slug, key, realm string) {
	d.t.Helper()
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
		current := d.waitForSchema(schema, "a new approval gate after changing "+field,
			func(resource *ptahv1alpha1.PtahSchema) bool {
				return changedSchemaApprovalRefused(resource, oldPlan.uid, generation)
			})
		plan := d.schemaPlan(current.Status.Plan.Name)
		if plan.Spec.Fingerprint == oldPlan.fingerprint {
			d.fatalf("%s reused its approved fingerprint after changing %s", schema, field)
		}
		d.plan = currentPlan{name: plan.Name, uid: string(plan.UID), fingerprint: plan.Spec.Fingerprint}
		d.waitForApproval(approval, "the previous input's approval to become stale",
			func(resource *ptahv1alpha1.PtahSchemaApproval) bool {
				return conditionIs(resource.Status.Conditions, "Stale", "True", "PlanNoLongerCurrent")
			})
		d.assertNoNewJobs(schema, "apply", jobs)
		d.assertDatabaseColumn(slug, "name", 0)
		d.logf("PASS %s %s changed after approval: a new plan requires a fresh decision", schema, field)
	}
}
