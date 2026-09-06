package crdupgrade

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

// These digests were produced from the independently rendered predecessor
// chart. They freeze every typed Spec plus the required ownership metadata so
// later edits to current guard constructors cannot redefine historical truth.
func TestLegacyControllerGuardContractsMatchPredecessorGoldens(t *testing.T) {
	t.Parallel()

	managerImage := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	guard := &RolloutGuard{
		ReleaseName:                          "ptah",
		ReleaseNamespace:                     "ptah-system",
		HookServiceAccountName:               "ptah-controller-crd-v1-" + hookIdentityDigest("ptah-system", "ptah", 1, managerImage)[:12],
		ControllerServiceAccountName:         "candidate-controller",
		PreviousControllerServiceAccountName: "legacy-controller",
		PreviousControllerReleaseSequence:    0,
		ControllerDeploymentName:             "ptah-controller",
		CertificateDeploymentName:            "ptah-controller-cert-rotator",
		ReleaseSequence:                      1,
		ManagerImage:                         managerImage,
	}
	names := legacyControllerGuardNames(guard.ReleaseNamespace, guard.ReleaseName)
	objects, err := legacyControllerGuardObjects(guard, names)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		names[0]: {"afb2a8a6e8d54746cc34c873d3ab6b66681db47011a01fdc83dd9707b6dbbeae", "8f2ab8b3c89e91465e319351aa4143cdc6c7b65938ce1f02fdef10dfacdaa2d0"},
		names[1]: {"e25d58e8ebf976ec2080e4aecba17a8ea909cb8b4da153038f043c4e8d381edc", "c28aada9708649a00d81addcf9c82440f561b099fba5732fd0ea04a267ddc74d"},
		names[2]: {"a90a83e0a6b0c51650cd5db642f711a783ef954c9c51ea3cbd6a0e12fe41f38e", "7ca0e4b2545f554bb9b6f5835ad51ee5a893a8eab285e232b0b7a2ede2c0e9f2"},
		names[3]: {"80a68b699065abfaaa2d90ab195972828d6e70c046ee98f961cad8fdd4940d30", "aa89a0613a97378b0c3e8c74edc79080024afc82e1d82788e0c18ee2323e0e81"},
		names[4]: {"dc8f3c713a2b1ae1d776143c9569a7901d0fe8a199bc8b8d433b63164a72a428", "559562f4bbadac47668591559aefc45a517cec76fc8e66d7c31d6350bc586ec6"},
		names[5]: {"3a76d58cd3aec2f2d1b15ba7fc493076496c6ab62d367355bd9a8f66732d141f", "3e6b2065bc960402de6fe32bcd75c2edc30058fd850959b9029fc937ddd634ef"},
	}
	for index, pair := range objects {
		name := names[index]
		t.Run(name, func(t *testing.T) {
			gotPolicy := legacyControllerObjectDigest(t, pair.policy.Name, pair.policy.Annotations, pair.policy.Labels, pair.policy.Spec)
			gotBinding := legacyControllerObjectDigest(t, pair.binding.Name, pair.binding.Annotations, pair.binding.Labels, pair.binding.Spec)
			if gotPolicy != want[name][0] || gotBinding != want[name][1] {
				t.Fatalf("golden digests = policy %s, binding %s; want policy %s, binding %s", gotPolicy, gotBinding, want[name][0], want[name][1])
			}
		})
	}
}

func legacyControllerObjectDigest(t *testing.T, name string, annotations, labels map[string]string, spec any) string {
	t.Helper()
	payload := map[string]any{
		"metadata": map[string]any{"name": name, "annotations": annotations, "labels": labels},
		"spec":     spec,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}
