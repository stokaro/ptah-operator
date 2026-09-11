package crdupgrade

import (
	"strings"
	"testing"
)

// The fence guards the markers a sequence may have to retire, and the chart
// renders that list from its own per-sequence inventory. A release installed
// fresh at a sequence with a recorded predecessor has no predecessor object,
// and must still compile the fence the chart rendered.
func TestFenceRetainedMarkersFollowTheSequenceNotTheLivePredecessor(t *testing.T) {
	rollout, _, _, _ := readyRolloutGuard()
	rollout.ReleaseSequence = 2
	base := strings.Split(rollout.HookServiceAccountName, "-crd-v")[0]
	rollout.HookServiceAccountName = base + "-crd-v2-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, 2, rollout.ManagerImage)[:12]
	rollout.ControllerServiceAccountName = "ptah-controller-v2"

	predecessorMarker := AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, 1)

	fresh := NewTeardownRetirementGuard(rollout)
	freshPolicy, _, _, err := fresh.OriginalFencePair(TeardownFenceA)
	if err != nil {
		t.Fatalf("fresh install fence pair error = %v", err)
	}
	if !strings.Contains(freshPolicy.Spec.MatchConditions[0].Expression, predecessorMarker) {
		t.Fatal("a fresh install at sequence 2 does not guard the marker its predecessor sequence may leave")
	}

	// The caller list is a different matter: the chart adds a predecessor
	// identity only when the release was upgraded into, and so does this
	// contract, so an upgrade guards one more caller than a fresh install.
	upgraded := *rollout
	upgraded.PreviousControllerReleaseSequence = 1
	upgraded.PreviousControllerServiceAccountName = "ptah-controller-v1"
	upgradedPolicy, _, _, err := NewTeardownRetirementGuard(&upgraded).OriginalFencePair(TeardownFenceA)
	if err != nil {
		t.Fatalf("upgraded fence pair error = %v", err)
	}
	if !strings.Contains(upgradedPolicy.Spec.MatchConditions[0].Expression, predecessorMarker) {
		t.Fatal("an upgrade does not guard the marker its predecessor left")
	}
	if !strings.Contains(upgradedPolicy.Spec.MatchConditions[0].Expression, "ptah-controller-v1") {
		t.Fatal("an upgrade does not guard the predecessor controller identity")
	}
	if strings.Contains(freshPolicy.Spec.MatchConditions[0].Expression, "ptah-controller-v1") {
		t.Fatal("a fresh install guards a predecessor controller identity that never existed")
	}

	// A predecessor the inventory does not record is still refused.
	foreign := *rollout
	foreign.PreviousControllerReleaseSequence = 7
	foreign.PreviousControllerServiceAccountName = "ptah-controller-v7"
	if _, _, _, err := NewTeardownRetirementGuard(&foreign).OriginalFencePair(TeardownFenceA); err == nil {
		t.Fatal("an unrecorded predecessor sequence compiled a fence")
	}
}
