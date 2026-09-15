package crdupgrade

// White-box testing required: the contract digest is a chart declaration read
// off the rendered guard, and no exported API returns it.

import (
	"testing"
)

// TestRuntimePodGuardContractDigestCoversTheExecutionBinding measures the
// freeze stokaro/ptah-operator#14 records. The retained runtime Pod guard is
// created once per release sequence and never rewritten, and its contract
// digest covers the manager arguments that carry the execution binding, so a
// `helm upgrade` that changes one of those values on an installed release
// renders a digest the retained guard cannot match. The control is the
// unchanged render: without it, "the digest moved" would also pass for a digest
// that moves on its own.
func TestRuntimePodGuardContractDigestCoversTheExecutionBinding(t *testing.T) {
	t.Parallel()
	baseline := renderedRuntimePodGuardContractDigest(t)
	t.Run("an unchanged render keeps the digest", func(t *testing.T) {
		if digest := renderedRuntimePodGuardContractDigest(t); digest != baseline {
			t.Fatalf("unchanged render digest = %s, want %s", digest, baseline)
		}
	})
	for _, test := range []struct {
		name     string
		override string
	}{
		{
			name:     "ptah version",
			override: "execution.ptahVersion=rbac-cutover-rebound",
		},
		{
			name:     "executor image",
			override: "execution.executorImage=example.invalid/ptah@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		},
		{
			name:     "runner image",
			override: "execution.runnerImage=example.invalid/operator@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		},
		{
			name:     "manager image",
			override: "image.digest=sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := renderedRuntimePodGuardContractDigest(t, "--set-string", test.override)
			if digest == baseline {
				t.Fatalf("%s left the runtime Pod guard contract digest at %s", test.name, baseline)
			}
		})
	}
}

// renderedRuntimePodGuardContractDigest returns the digest the runtime Pod
// guard renders, and refuses a render where the policy and its binding disagree
// about it: the upgrade refusal reads whichever of the two it meets first, so a
// pair that carried two digests would refuse for one value and accept for the
// other.
func renderedRuntimePodGuardContractDigest(t *testing.T, extraArgs ...string) string {
	t.Helper()
	digests := map[string]string{}
	for _, object := range renderControllerRBACCutoverChart(t, extraArgs...) {
		digest, found := object.GetAnnotations()[runtimePodContractDigestAnnotation]
		if !found {
			continue
		}
		kind := object.GetKind()
		if previous, duplicate := digests[kind]; duplicate {
			t.Fatalf("%s carries the runtime Pod contract digest twice: %s and %s", kind, previous, digest)
		}
		digests[kind] = digest
	}
	policy, foundPolicy := digests["ValidatingAdmissionPolicy"]
	binding, foundBinding := digests["ValidatingAdmissionPolicyBinding"]
	if !foundPolicy || !foundBinding {
		t.Fatalf("rendered runtime Pod guard carries %d contract digests, want the policy and its binding", len(digests))
	}
	if policy != binding {
		t.Fatalf("policy digest %s and binding digest %s disagree", policy, binding)
	}
	return policy
}
