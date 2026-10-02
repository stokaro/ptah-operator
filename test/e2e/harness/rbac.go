package harness

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateAfterRoleBinding waits for a newly installed RoleBinding to reach the
// API server's authorizer. Only a definitive Forbidden response is retryable:
// retrying an ambiguous write failure could duplicate a successful request.
// The same identity, object, and admission options are used on every attempt.
func CreateAfterRoleBinding(ctx context.Context, writer client.Client, object client.Object, opts ...client.CreateOption) error {
	return Wait(ctx, "the new RoleBinding to authorize creation", time.Minute, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := writer.Create(ctx, object, opts...)
			if apierrors.IsForbidden(err) {
				return false, "waiting for the new identity's create permission: " + err.Error(), nil
			}
			return err == nil, "create the object under the granted identity", err
		})
}
