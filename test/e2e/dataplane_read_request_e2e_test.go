//go:build e2e

package e2e

import (
	"time"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// readRequestKeepsTheWaitingPlan asks a schema whose plan waits for a person,
// and whose next read is far away, to read its database now. The request
// refreshes the evidence the approval is checked against, as the interval
// would: exactly one Observe Job, dated after the request and before the
// interval's read, no Apply, and the same plan back at the gate, by name, UID
// and fingerprint, for the approval the row creates next to apply.
//
// The wait ends when the read has: an observation recorded after the request
// and the plan waiting again with no operation running. An Observe Job that
// exists is not enough. Until it runs, the status still holds the gate the
// read started from, and an approval created then is refused while the
// schema is Observing.
func (d *dataPlane) readRequestKeepsTheWaitingPlan(schema, digest, planName, planUID, fingerprint string) {
	d.t.Helper()
	waiting := &ptahv1alpha1.PtahSchema{}
	d.check(d.get(schema, waiting), "read PtahSchema %s", schema)
	if waiting.Status.NextReconciliationTime == nil {
		d.fatalf("%s waits for a person with no next read to tell a read on request from", schema)
	}
	intervalRead := waiting.Status.NextReconciliationTime.Time
	if !intervalRead.After(time.Now().Add(readRequestBound)) {
		d.fatalf("%s reads again at %s, too soon to tell a read on request from the interval's", schema, intervalRead)
	}
	before := d.checkpointJobs(schema, "")
	requested := time.Now().UTC().Truncate(time.Second)
	value := requested.Format(time.RFC3339)
	d.patchSchema(schema, map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{ptahv1alpha1.ReadRequestAnnotation: value},
	}})
	d.waitForSchema(schema, "its waiting plan back at the gate after a read on request", func(s *ptahv1alpha1.PtahSchema) bool {
		plan := s.Status.Plan
		return schemaObservedOnRequest(s, value, requested, intervalRead) && grantOnlyApprovalGate(s, digest) &&
			plan.Name == planName && string(plan.UID) == planUID && plan.Fingerprint == fingerprint
	})
	after := d.checkpointJobs(schema, "")
	if err := observedOnRequest(d.observed.between(schema, "observe", before, after), requested, intervalRead); err != nil {
		d.fatalf("%s: %v", schema, err)
	}
	d.assertNoJobBetween(schema, "apply", before, after)
	d.logf("PASS a read on request refreshed %s and kept its waiting plan %s", schema, planName)
}
