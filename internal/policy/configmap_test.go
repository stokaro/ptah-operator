package policy

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

func TestConfigMapDigestUsesExactProjectedBytes(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "policy"}, Data: map[string]string{"policy.yaml": "version: 1\n"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	digest, err := ConfigMapDigest(context.Background(), reader, "team-a", corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if digest != fingerprint.DigestBytes([]byte("version: 1\n")) {
		t.Fatalf("digest = %s", digest)
	}
}
