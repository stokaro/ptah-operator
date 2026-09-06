package crdupgrade

import (
	"fmt"
	"os"
	"testing"
)

func TestZZDump(t *testing.T) {
	managerImage := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	guard := &RolloutGuard{
		ReleaseName: "ptah", ReleaseNamespace: "ptah-system",
		HookServiceAccountName:               "ptah-controller-crd-v1-" + hookIdentityDigest("ptah-system", "ptah", 1, managerImage)[:12],
		ControllerServiceAccountName:         "candidate-controller",
		PreviousControllerServiceAccountName: "legacy-controller",
		ControllerDeploymentName:             "ptah-controller",
		CertificateDeploymentName:            "ptah-controller-cert-rotator",
		ReleaseSequence:                      1, ManagerImage: managerImage,
	}
	names := legacyControllerGuardNames(guard.ReleaseNamespace, guard.ReleaseName)
	objects, err := legacyControllerGuardObjects(guard, names)
	if err != nil {
		t.Fatalf("построение: %v", err)
	}
	f, _ := os.Create(os.Getenv("ZZ_DUMP"))
	defer f.Close()
	for _, pair := range objects {
		for i, v := range pair.policy.Spec.Validations {
			fmt.Fprintf(f, "%s\tV%d\t%s\n", pair.policy.Name, i, v.Expression)
		}
		for i, v := range pair.policy.Spec.Variables {
			fmt.Fprintf(f, "%s\tX%d\t%s\n", pair.policy.Name, i, v.Expression)
		}
	}
}
