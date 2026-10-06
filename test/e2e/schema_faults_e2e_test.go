//go:build e2e

package e2e

import (
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestSchemaFaults owns the fault scenarios and their shared watches, barriers,
// and audit ledger. The MySQL and external PostgreSQL lifecycles stay here
// because their closing assertions must hold after fault injection.
func TestSchemaFaults(t *testing.T) {
	run, inputs := harness.Begin(t, phases.SchemaFaults)
	if inputs.Mode != "full" {
		t.Fatal("schema faults require full acceptance")
	}
	d := newDataPlane(t, run, inputs)
	f := newFaultRun(d)
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"external-postgresql-lifecycle", d.externalPostgresqlLifecycle},
		{"mysql-lifecycle", d.mysqlLifecycle},
		{"mysql-dsn-refusal", d.mysqlDSNRefusalScenario},
		{"watches", f.watches},
		{"mysql-drift-before-dispatch", func() { f.mysqlDriftBeforeDispatch() }},
		{"hung-schema-result-read", func() { f.hungResultReads() }},
		{"job-deadline", func() { f.jobDeadline() }},
		{"manager-restart", func() { f.managerRestart() }},
		{"runner-termination", func() { f.runnerTermination() }},
		{"job-deletion", func() { f.jobDeletion() }},
		{"closing-audits", d.closingAudits},
	} {
		if !run.Scenario(scenario.name, d.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e schema faults: PASS restart identity and fault recovery on both engines")
}
