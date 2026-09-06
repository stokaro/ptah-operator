// Package policy binds verification policy bytes to plans and approvals.
package policy

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// Binding identifies one immutable verification-policy object and the exact
// bytes selected from it. The UID prevents delete-and-recreate aliasing.
type Binding struct {
	UID    types.UID
	Digest string
}

// ConfigMapBinding reads exactly the selected key and returns the immutable
// object UID plus the digest of the bytes a ConfigMap volume would project.
// Verification policies are public configuration, never credentials.
func ConfigMapBinding(ctx context.Context, reader client.Reader, namespace string, selector corev1.ConfigMapKeySelector) (Binding, error) {
	if reader == nil || namespace == "" || selector.Name == "" || selector.Key == "" {
		return Binding{}, fmt.Errorf("verification policy reference is incomplete")
	}
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: selector.Name}, configMap); err != nil {
		return Binding{}, fmt.Errorf("read verification policy ConfigMap: %w", err)
	}
	if configMap.UID == "" || configMap.DeletionTimestamp != nil {
		return Binding{}, fmt.Errorf("verification policy ConfigMap has no stable object identity")
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return Binding{}, fmt.Errorf("verification policy ConfigMap must set immutable: true")
	}
	if value, ok := configMap.BinaryData[selector.Key]; ok {
		return Binding{UID: configMap.UID, Digest: fingerprint.DigestBytes(value)}, nil
	}
	if value, ok := configMap.Data[selector.Key]; ok {
		return Binding{UID: configMap.UID, Digest: fingerprint.DigestBytes([]byte(value))}, nil
	}
	return Binding{}, fmt.Errorf("verification policy ConfigMap key is missing")
}
