//go:build e2e

package e2e

import (
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestSchemaApprovals owns the independent authority changes on both engines.
// The deadline, restart, deletion and shared-realm fault sequence remains in
// TestSchemaFaults, together with every assertion about its retained state.
func TestSchemaApprovals(t *testing.T) {
	run, inputs := harness.Begin(t, phases.SchemaApprovals)
	if inputs.Mode != "full" {
		t.Fatal("schema approvals require full acceptance")
	}
	d := newDataPlane(t, run, inputs)
	f := newFaultRun(d)
	f.fixtureSuffix = "-approvals"
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"watches", f.prepareWatches},
		{"approval-resource-replacement", func() { f.schemaIdentityReplacement() }},
		{"approval-target-secret-change", func() { f.targetSecretChanges() }},
		{"approval-destructive-policy-change", func() { f.destructivePolicyChanges() }},
		{"approval-exclusion-policy-change", func() { f.exclusionPolicyChanges() }},
		{"approval-verification-policy-change", func() { f.verificationPolicyChanges() }},
		{"approval-history-audit", f.closeApprovalWatches},
		{"approval-executor-image-change", func() { f.executorImageChanges() }},
		{"approval-ptah-version-change", func() { f.ptahVersionChanges() }},
		{"unsupported-controller-state-after-approval", func() { f.unsupportedControllerStates() }},
		{"unsupported-runner-protocol-after-approval", func() { f.unsupportedRunnerProtocols() }},
		{"running-apply-executor-image-change", func() { f.runningExecutorImageChanges() }},
	} {
		if !run.Scenario(scenario.name, d.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e schema approvals: PASS every authority change and its executing allowed control on both engines")
}
