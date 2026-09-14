package crdupgrade

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

// These digests freeze every typed Spec plus the required ownership metadata of
// the guards a first-version release installs. An edit to a guard constructor
// changes an object a cluster already holds, so the pair has to move in the
// same commit as the constructor and be looked at while it does.
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
		names[0]: {"cf9e2c768d4f0851294f5831f6ff149f11155e20816e06676fcad3422af7f51d", "b531c582c889c22ff66b323ecf37c562207d59e6eafb1cf65ccd863168ec9c0f"},
		names[1]: {"0284062a1f89245d1aa4c1fe354722afe394128b91e8db45d03b6eb603d1918a", "1309153cb6ab9823a163ebd43e8d39f8e8018fc26ba6fb4ce79ea5e66653822d"},
		names[2]: {"955d6f49ad242d1c9d35edde6e9eecbb81bcda488f12d0cc2dfa21280ae9358c", "e241b50faedaa261f166ed11cf31a5561234447b17a56e6e34bf203ae4a8c5c6"},
		names[3]: {"2313facdf5554ca350d56dc7d3399fe3eef8fe78e1fdea4aa3a954e05fde281c", "3a3d613c7e4714424019785ca22a9c850636b65a97cc4e44f6167530038ffa49"},
		names[4]: {"9d6a490f76781bfcd64f0437e495f2d39e1714ce7f65808fc83120212f72a1c2", "29edb28a724f4357a201bbcb447c5617768965c680bb7e685a7953a5bc7b96eb"},
		names[5]: {"c25e45ae579fc097081fa0a383d912becac3e19624ccf9c86be34913c4984f34", "45e5ce3ce699c930fdf99cd0f2a2409b58662432b69eaa2668df2398dbced2da"},
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
