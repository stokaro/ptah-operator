package crdupgrade

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A guard the release has just installed can refuse the synthetic bootstrap
// baseline for a moment. That baseline is invented for the probe and carries no
// predecessor, so the wait outlasts the refusal instead of failing the install.
func TestWaitEnforcedOutlastsABootstrapBaselineRefusal(t *testing.T) {
	guard, _, _, deployments := readyRolloutGuard()
	guard.PollEvery = time.Millisecond
	policyName := RolloutGuardPolicyName(guard.ReleaseSequence)
	denialMessage := rolloutGuardProbeDenialMessage(guard.ReleaseSequence)
	deployments.dryCreateResults = []error{
		exactPolicyDenialError(policyName, policyName, denialMessage),
		nil,
		exactPolicyDenialError(policyName, policyName, denialMessage),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := guard.waitEnforced(ctx, policyName, denialMessage); err != nil {
		t.Fatalf("waitEnforced() error = %v, want the refused baseline to be waited out", err)
	}
	if deployments.dryCreates != 3 {
		t.Fatalf("dry-run creates = %d, want 3: a refused baseline, an accepted one, and the sentinel", deployments.dryCreates)
	}
}

// The wait is bounded: a baseline that never becomes acceptable reports the
// refusal it kept seeing rather than a bare deadline.
func TestWaitEnforcedReportsAPersistentBootstrapRefusal(t *testing.T) {
	guard, _, _, deployments := readyRolloutGuard()
	guard.PollEvery = time.Millisecond
	policyName := RolloutGuardPolicyName(guard.ReleaseSequence)
	denialMessage := rolloutGuardProbeDenialMessage(guard.ReleaseSequence)
	for range 64 {
		deployments.dryCreateResults = append(deployments.dryCreateResults,
			exactPolicyDenialError(policyName, policyName, denialMessage))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := guard.waitEnforced(ctx, policyName, denialMessage)
	if err == nil {
		t.Fatal("a baseline that is always refused was reported as enforced")
	}
	if !strings.Contains(err.Error(), "prove baseline Deployment is accepted") {
		t.Fatalf("waitEnforced() error = %v, want the kept refusal", err)
	}
}
