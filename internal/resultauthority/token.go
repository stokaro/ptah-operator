package resultauthority

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TokenReviewer submits a TokenReview to the API server. The receiver's own
// identity needs create on tokenreviews; the runner needs no API permissions.
type TokenReviewer interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// TokenVerifier authenticates a Pod, not its permission to publish a result.
// The receiver must separately verify the immutable first-Pod pin and current
// operation authority. Reader bypasses the informer cache. Reviews are never
// cached: expiration and bound-object deletion must affect later requests and
// the check immediately before publication completes.
type TokenVerifier struct {
	Reviews TokenReviewer
	Reader  client.Reader
}

func (v TokenVerifier) Verify(ctx context.Context, token string, identity resultdelivery.Identity) error {
	if v.Reviews == nil || v.Reader == nil {
		return errors.New("result token authentication is unavailable")
	}
	if !resultdelivery.ValidToken(token) {
		return resultdelivery.ErrAuthority
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	review, err := v.Reviews.Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token: token, Audiences: []string{resultdelivery.TokenAudience},
	}}, metav1.CreateOptions{})
	if err != nil {
		// API errors may include submitted fields. Do not propagate a token or
		// an authenticator's diagnostic into HTTP responses, logs, or Events.
		return errors.New("result token authentication is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if review == nil || review.Status.Error != "" {
		return errors.New("result token authentication is unavailable")
	}
	status := review.Status
	if !status.Authenticated || len(status.Audiences) != 1 || status.Audiences[0] != resultdelivery.TokenAudience || status.User.UID == "" {
		return resultdelivery.ErrAuthority
	}
	b := identity.Binding
	username := strings.Split(status.User.Username, ":")
	if len(username) != 4 || username[0] != "system" || username[1] != "serviceaccount" || username[2] != b.Namespace || username[3] == "" {
		return resultdelivery.ErrAuthority
	}
	name, uid := status.User.Extra["authentication.kubernetes.io/pod-name"], status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if len(name) != 1 || len(uid) != 1 || name[0] != b.PodName || uid[0] != string(b.PodUID) || name[0] == "" || uid[0] == "" {
		return resultdelivery.ErrAuthority
	}
	pod := &corev1.Pod{}
	if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.PodName}, pod); err != nil {
		if errors.Is(readError(err), resultdelivery.ErrAuthority) {
			return resultdelivery.ErrAuthority
		}
		return errors.New("result token Pod verification is unavailable")
	}
	if pod.UID != b.PodUID || pod.Namespace != b.Namespace || pod.Spec.ServiceAccountName != username[3] {
		return resultdelivery.ErrAuthority
	}
	return ctx.Err()
}
