// Package policy binds verification policy bytes to plans and approvals.
package policy

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// ConfigMapDigest reads exactly the selected key and returns the digest of the
// bytes a ConfigMap volume would project. Verification policies are public
// configuration, never credentials.
func ConfigMapDigest(ctx context.Context, reader client.Reader, namespace string, selector corev1.ConfigMapKeySelector) (string, error) {
	if reader == nil || namespace == "" || selector.Name == "" || selector.Key == "" {
		return "", fmt.Errorf("verification policy reference is incomplete")
	}
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: selector.Name}, configMap); err != nil {
		return "", fmt.Errorf("read verification policy ConfigMap: %w", err)
	}
	if value, ok := configMap.BinaryData[selector.Key]; ok {
		return fingerprint.DigestBytes(value), nil
	}
	if value, ok := configMap.Data[selector.Key]; ok {
		return fingerprint.DigestBytes([]byte(value)), nil
	}
	return "", fmt.Errorf("verification policy ConfigMap key is missing")
}
