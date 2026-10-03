package resultauthority

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type reviewFunc func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)

func (f reviewFunc) Create(ctx context.Context, r *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	return f(ctx, r)
}

func tokenReview(f *resulttest.Fixture) *authenticationv1.TokenReview {
	b := f.Identity.Binding
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true, Audiences: []string{resultdelivery.TokenAudience},
		User: authenticationv1.UserInfo{Username: "system:serviceaccount:" + b.Namespace + ":" + f.Pod.Spec.ServiceAccountName, UID: "sa-uid",
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {b.PodName},
				"authentication.kubernetes.io/pod-uid":  {string(b.PodUID)},
			}},
	}}
}

func TestTokenAuthenticationBindsExactPodAndServiceAccount(t *testing.T) {
	for name, mutate := range map[string]func(*authenticationv1.TokenReview){
		"valid":                func(*authenticationv1.TokenReview) {},
		"unauthenticated":      func(r *authenticationv1.TokenReview) { r.Status.Authenticated = false },
		"API audience":         func(r *authenticationv1.TokenReview) { r.Status.Audiences = []string{"https://kubernetes.default.svc"} },
		"no audience":          func(r *authenticationv1.TokenReview) { r.Status.Audiences = nil },
		"extra audience":       func(r *authenticationv1.TokenReview) { r.Status.Audiences = append(r.Status.Audiences, "other") },
		"missing SA UID":       func(r *authenticationv1.TokenReview) { r.Status.User.UID = "" },
		"not a ServiceAccount": func(r *authenticationv1.TokenReview) { r.Status.User.Username = "ordinary-user" },
		"another namespace":    func(r *authenticationv1.TokenReview) { r.Status.User.Username = "system:serviceaccount:other:default" },
		"another SA":           func(r *authenticationv1.TokenReview) { r.Status.User.Username += "-other" },
		"no Pod binding":       func(r *authenticationv1.TokenReview) { r.Status.User.Extra = nil },
		"another Pod name": func(r *authenticationv1.TokenReview) {
			r.Status.User.Extra["authentication.kubernetes.io/pod-name"] = authenticationv1.ExtraValue{"other"}
		},
		"replacement Pod UID": func(r *authenticationv1.TokenReview) {
			r.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"other"}
		},
		"ambiguous Pod binding": func(r *authenticationv1.TokenReview) {
			r.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = append(r.Status.User.Extra["authentication.kubernetes.io/pod-uid"], "other")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, "schema-apply-admitted-scheduling")
			r := tokenReview(f)
			mutate(r)
			calls := 0
			v := TokenVerifier{Reader: f.Client(t), Reviews: reviewFunc(func(ctx context.Context, request *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second || request.Spec.Token != "opaque.token.bytes" || len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != resultdelivery.TokenAudience {
					t.Fatal("TokenReview lost its deadline, credential, or explicit audience")
				}
				return r, nil
			})}
			err := v.Verify(t.Context(), "opaque.token.bytes", f.Identity)
			if calls != 1 || name == "valid" && err != nil || name != "valid" && !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("calls=%d, error=%v", calls, err)
			}
		})
	}
}

func TestTokenAuthenticationRechecksDeletedOrReplacedPod(t *testing.T) {
	for _, replace := range []bool{false, true} {
		f := resulttest.New(t, "migration-history")
		c := f.Client(t)
		calls := 0
		v := TokenVerifier{Reader: c, Reviews: reviewFunc(func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
			calls++
			return tokenReview(f), nil
		})}
		if err := v.Verify(t.Context(), "opaque.token.bytes", f.Identity); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(t.Context(), f.Pod); err != nil {
			t.Fatal(err)
		}
		if replace {
			pod := f.Pod.DeepCopy()
			pod.UID, pod.ResourceVersion = "replacement", ""
			if err := c.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
		}
		if err := v.Verify(t.Context(), "opaque.token.bytes", f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) || calls != 2 {
			t.Fatalf("old Pod authentication was cached: calls=%d, error=%v", calls, err)
		}
	}
}

type unavailablePodReader struct{ client.Reader }

func (r unavailablePodReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("private-authenticator-diagnostic")
}

func TestTokenAuthenticationFailuresAreBoundedAndRedacted(t *testing.T) {
	f := resulttest.New(t, "schema-resolve")
	for _, failure := range []string{"transport", "status", "missing response", "Pod read", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			v := TokenVerifier{Reader: f.Client(t), Reviews: reviewFunc(func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				r := tokenReview(f)
				switch failure {
				case "transport":
					return nil, errors.New("private-authenticator-diagnostic")
				case "status":
					r.Status.Error = "private-authenticator-diagnostic"
				case "missing response":
					return nil, nil
				case "cancellation":
					cancel()
				}
				return r, nil
			})}
			if failure == "Pod read" {
				v.Reader = unavailablePodReader{v.Reader}
			}
			err := v.Verify(ctx, "private.token.bytes", f.Identity)
			if err == nil || errors.Is(err, resultdelivery.ErrAuthority) || strings.Contains(err.Error(), "private") {
				t.Fatalf("temporary authentication failure was accepted, terminal, or leaked data: %v", err)
			}
		})
	}
	// A canceled caller and malformed header cannot issue an API request.
	v := TokenVerifier{Reader: f.Client(t), Reviews: reviewFunc(func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
		t.Fatal("invalid or canceled authentication reached TokenReview")
		return nil, nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := v.Verify(ctx, "opaque.token.bytes", f.Identity); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := v.Verify(t.Context(), "token\nextra", f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatal(err)
	}
}
