package migrationplan_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// TestAPlanWithoutAShadowHashesTheDocumentItDidBefore holds the promise a
// policy without a drift check rests on: its plan's identity is the document a
// manager before the check hashed, so its name and its approvals carry over. A
// plan that binds a shadow is another identity, and a shadow that is not a
// digest is refused rather than hashed.
func TestAPlanWithoutAShadowHashesTheDocumentItDidBefore(t *testing.T) {
	t.Parallel()
	binding := completeBinding()
	want, err := fingerprint.DigestCanonicalJSON(map[string]any{
		"contract_version":           binding.ContractVersion(),
		"migration_uid":              string(binding.MigrationUID),
		"history_fingerprint":        binding.HistoryFingerprint,
		"sequence_digest":            binding.SequenceDigest,
		"coordination_digest":        binding.CoordinationDigest,
		"target_identity_digest":     binding.TargetIdentityDigest,
		"policy_fingerprint":         binding.PolicyFingerprint,
		"execution_binding_id":       binding.ExecutionBindingID,
		"controller_state_version":   binding.ControllerStateVersion,
		"ptah_version":               binding.PtahVersion,
		"executor_image":             binding.ExecutorImage,
		"runner_protocol_version":    binding.RunnerProtocolVersion,
		"artifact_digest":            binding.ArtifactDigest,
		"verification_policy_uid":    string(binding.VerificationPolicyUID),
		"verification_policy_digest": binding.VerificationPolicyDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := binding.Fingerprint()
	if err != nil {
		t.Fatalf("the binding without a shadow was refused: %v", err)
	}
	if got != want {
		t.Fatalf("a plan without a shadow hashes %s, and the document a manager before the drift check hashed is %s", got, want)
	}

	shadowed := completeBinding()
	shadowed.ShadowIdentityDigest = digest('2')
	other, err := shadowed.Fingerprint()
	if err != nil {
		t.Fatalf("the binding with a shadow was refused: %v", err)
	}
	if other == got {
		t.Fatal("a plan that binds a shadow shares the identity of one that binds none")
	}

	shadowed.ShadowIdentityDigest = "sha256:short"
	if _, err := shadowed.Fingerprint(); err == nil {
		t.Fatal("a shadow identity that is not a SHA-256 digest was hashed")
	}
}
