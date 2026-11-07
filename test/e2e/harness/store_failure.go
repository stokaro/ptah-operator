package harness

import (
	"context"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// storeFailureBackoff waits about half a minute in all, which covers etcd
// electing a leader on a busy runner.
var storeFailureBackoff = apiwait.Backoff{Steps: 6, Duration: time.Second, Factor: 2, Jitter: 0.1}

// isStoreFailure is an API server saying that etcd did not finish a write in
// time. Nothing in the request was wrong, and the single-node etcd of a kind
// cluster on a busy runner answers this now and then.
func isStoreFailure(err error) bool {
	if !apierrors.IsInternalError(err) && !apierrors.IsServerTimeout(err) && !apierrors.IsTimeout(err) {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "etcdserver: request timed out") ||
		strings.Contains(message, "etcdserver: leader changed")
}

// withStoreFailureRetries retries a Patch that etcd did not finish. A row writes
// a patch to arrange a state, and one that fails this way says nothing about the
// operator, while the phase stops at it and hides every row behind it. Only Patch
// is retried: it means the same thing if the first attempt did land, where a
// repeated Create would read as AlreadyExists and an Update as a conflict. Every
// other error returns at once, so a refusal a row asserts stays a refusal.
func withStoreFailureRetries(direct client.WithWatch, backoff apiwait.Backoff) client.Client {
	return interceptor.NewClient(direct, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			return retry.OnError(backoff, isStoreFailure, func() error {
				return c.Patch(ctx, object, patch, options...)
			})
		},
	})
}
