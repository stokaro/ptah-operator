//go:build e2e

package e2e

import (
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

func TestMigrationRuntimePostgreSQL(t *testing.T) {
	runMigrationRuntimePhase(t, phases.MigrationRuntimePostgreSQL, "postgresql")
}

func TestMigrationRuntimeMySQL(t *testing.T) {
	runMigrationRuntimePhase(t, phases.MigrationRuntimeMySQL, "mysql")
}

// These approval and runtime rows own their databases and decisions. They need an artifact and
// verification policy, not a preceding run of migration lifecycle acceptance.
func runMigrationRuntimePhase(t *testing.T, phase phases.Of[phases.MigrationsInputs], engine string) {
	run, inputs := harness.Begin(t, phase)
	m := newMigrationRun(t, run, inputs, engine)
	m.repository += "-runtime"
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"runtime-artifact", func() {
			m.migrationPolicy()
			m.publish("runtime-source", m.fixtureDir(""), m.reference(""))
		}},
		{"approval-resource-replacement", m.approvalIdentityReplacement},
		{"approval-policy-change", func() { m.approvalInputChange("policy") }},
		{"approval-transaction-mode-change", func() { m.approvalInputChange("transaction-mode") }},
		{"approval-artifact-change", func() { m.approvalInputChange("artifact") }},
		{"approval-verification-policy-uid-change", func() { m.approvalInputChange("verification-policy-uid") }},
		{"approval-verification-policy-content-change", func() { m.approvalInputChange("verification-policy-content") }},
		{"approval-executor-image-change", m.executorImageChange},
		{"approval-ptah-version-change", m.ptahVersionChange},
		{"unsupported-controller-state-after-approval", m.unsupportedControllerState},
		{"unsupported-runner-protocol-after-approval", m.unsupportedRunnerProtocol},
		{"running-apply-executor-image-change", m.runningExecutorImageChange},
	} {
		if !run.Scenario(scenario.name, m.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e migration runtime: PASS %s execution bindings, runtime refusals, and authorized recovery", m.engine.kind)
}
