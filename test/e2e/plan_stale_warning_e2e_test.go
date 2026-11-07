//go:build e2e

package e2e

import (
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// planStaleOnlyOnReplacement holds PlanStale to its meaning on a real read of a
// real database: two scheduled reads of a waiting plan record no Warning, and
// the read that finds the database changed records exactly one, naming both
// plans (#820). It stays quiet for the reads after that one as well.
func (f *faultRun) planStaleOnlyOnReplacement() {
	f.t.Helper()
	const name, database, secret = "e2e-plan-stale", "e2e_plan_stale", "e2e-plan-stale-db"
	f.createDatabase("postgresql", database, secret)
	f.createSchema(faultSchema{
		name: name, engine: "PostgreSQL", reference: f.pgReference, secret: secret,
		coordinationKey: "e2e/plan-stale/postgresql", interval: "15s",
	})
	first := f.waitForPlan(name)
	waiting := f.waitForSchema(name, "the first plan to be waiting after its read", planAwaitingApproval)
	warningsOf := func(schema *ptahv1alpha1.PtahSchema) []corev1.Event {
		f.t.Helper()
		events := &corev1.EventList{}
		f.mustList(events, client.MatchingFields{"involvedObject.name": name, "reason": "PlanStale"})
		f.scan(f.jsonBytes(events), "the PlanStale Events of "+name)
		return planStaleWarnings(events.Items, schema)
	}
	keptAfterTwoReads := func(plan *ptahv1alpha1.PtahSchema) *ptahv1alpha1.PtahSchema {
		f.t.Helper()
		current := plan
		for range 2 {
			since := *current.Status.Target.LastObservedAt
			current = f.waitForSchema(name, "another scheduled read to end at plan "+plan.Status.Plan.Name, func(resource *ptahv1alpha1.PtahSchema) bool {
				return readKeptPlan(resource, plan.Status.Plan, since)
			})
		}
		return current
	}

	settled := keptAfterTwoReads(waiting)
	if warnings := warningsOf(settled); len(warnings) != 0 {
		f.fatalf("%s kept plan %s across two scheduled reads and recorded %d PlanStale Warnings; first: %q",
			name, first, len(warnings), warnings[0].Message)
	}

	// Dropping a declared column is a change the next plan has to put back, so
	// the plan is non-destructive and still waits for approval.
	f.query("postgresql", database, "ALTER TABLE e2e_widgets DROP COLUMN note")
	replaced := f.waitForSchema(name, "a read of the changed database to replace plan "+first, func(resource *ptahv1alpha1.PtahSchema) bool {
		return planReplacedAwaitingApproval(resource, first)
	})
	second := replaced.Status.Plan.Name
	var verdict error
	f.poll("one PlanStale Warning naming "+first+" and "+second, func() bool {
		verdict = planWarnedReplaced(warningsOf(replaced), first, second)
		return verdict == nil
	})

	settled = keptAfterTwoReads(replaced)
	if verdict = planWarnedReplaced(warningsOf(settled), first, second); verdict != nil {
		f.fatalf("%s after two more reads of plan %s: %v", name, second, verdict)
	}

	// Nothing later should pay for this row's cadence.
	f.patchSchema(name, map[string]any{"spec": map[string]any{"interval": "1h"}})
	f.logf("PASS PlanStale: two reads of a waiting plan recorded nothing, and the read that replaced %s recorded one Warning naming %s", first, second)
}
