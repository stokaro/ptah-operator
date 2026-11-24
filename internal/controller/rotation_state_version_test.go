package controller

import (
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// An upgrade that moves the controller-state version, as the release that
// first binds a URL from a SecretProviderClass moves it from 1 to 2, does not
// wait for an Apply its predecessor dispatched: the claim was recorded under
// state this manager reads differently, so the Apply is recorded outcome
// unknown, and the release notes tell a person to let Applies finish first.
// The control row holds the same claim under the state this manager compiles
// to a rotation that waits, so the refusal is the version's and nothing else's.
func TestARotationThatMovesTheControllerStateDoesNotWait(t *testing.T) {
	t.Parallel()

	claimEpoch := "v1-" + strings.Repeat("a", 32)
	configured := &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  "v1-" + strings.Repeat("b", 32),
		ControllerStateVersion: controllerstate.CurrentVersion,
		RunnerProtocolVersion:  runner.ProtocolVersion,
	}
	recorded := configured.DeepCopy()
	recorded.Epoch = claimEpoch
	if !rotationMayWaitFor(recorded, configured, claimEpoch) {
		t.Fatalf("a rotation under the same controller state does not wait: recorded %#v, configured %#v", recorded, configured)
	}
	recorded.ControllerStateVersion = controllerstate.CurrentVersion - 1
	if rotationMayWaitFor(recorded, configured, claimEpoch) {
		t.Fatalf("a rotation from controller state %d to %d waits for a claim recorded under the earlier one",
			recorded.ControllerStateVersion, configured.ControllerStateVersion)
	}
}
