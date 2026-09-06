package crdupgrade

// White-box testing required: verifyAdmissionConvergenceDependencyMetadata is
// reached only through a live dependency read, and the shape under test is
// what a typed client-go read returns.

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A typed client-go read leaves TypeMeta empty; the Go type is the identity.
// A live dependency is a Helm hook and carries Helm's hook annotations, which
// are not ownership.
func TestVerifyAdmissionConvergenceDependencyMetadataAcceptsATypedRead(t *testing.T) {
	t.Parallel()
	expected := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ptah-operator-rollout-guard-v1",
			Annotations: map[string]string{"operator.ptah.dev/release-name": "ptah"},
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "ptah-operator"},
		},
	}
	live := expected.DeepCopy()
	live.TypeMeta = metav1.TypeMeta{}
	// The chart renders the dependency as a Helm hook.
	live.Annotations["helm.sh/hook"] = "pre-install,pre-upgrade"
	live.Annotations["helm.sh/hook-weight"] = "-50"
	live.Annotations["helm.sh/resource-policy"] = "keep"
	if err := verifyAdmissionConvergenceDependencyMetadata(live.ObjectMeta, expected); err != nil {
		t.Fatalf("a dependency read through the typed client was refused: %v", err)
	}
	foreign := live.DeepCopy()
	foreign.Annotations["meta.helm.sh/release-name"] = "other"
	if err := verifyAdmissionConvergenceDependencyMetadata(foreign.ObjectMeta, expected); err == nil {
		t.Fatal("a dependency with foreign ownership was accepted")
	}
	incomplete := live.DeepCopy()
	delete(incomplete.Annotations, "operator.ptah.dev/release-name")
	if err := verifyAdmissionConvergenceDependencyMetadata(incomplete.ObjectMeta, expected); err == nil {
		t.Fatal("a dependency with incomplete ownership was accepted")
	}
}
