package policy

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

func TestConfigMapDigestUsesExactProjectedBytes(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	immutable := true
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "policy", UID: "policy-v1-uid"},
		Immutable:  &immutable,
		Data:       map[string]string{"policy.yaml": "version: 1\n"},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	binding, err := ConfigMapBinding(context.Background(), reader, "team-a", corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.UID != configMap.UID || binding.Digest != fingerprint.DigestBytes([]byte("version: 1\n")) {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestConfigMapBindingRejectsMutableOrUnidentifiedPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		uid       string
		immutable *bool
		want      string
	}{
		{name: "mutable", uid: "policy-uid", want: "immutable: true"},
		{name: "missing UID", immutable: ptr(true), want: "stable object identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "policy", UID: types.UID(test.uid)},
				Immutable:  test.immutable,
				Data:       map[string]string{"policy.yaml": "version: 1\n"},
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
			_, err := ConfigMapBinding(context.Background(), reader, "team-a", corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml",
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ConfigMapBinding() error = %v, want %q", err, test.want)
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }
