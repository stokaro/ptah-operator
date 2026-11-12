package resultauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
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

type failingPodReader struct {
	client.Reader
	err error
}

func (r failingPodReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

// TestTokenAuthenticationFailuresLogWhyWithoutText reads what the manager's
// production encoder prints for each verification that reaches no verdict.
// The receiver answers all of them with the same 503, so the line is the only
// place that says which it was: the API server's code and reason where it
// answered with a status, whether the deadline passed where it did not. The
// token and the text of an answer appear nowhere, so every secret here is
// spelled "private".
func TestTokenAuthenticationFailuresLogWhyWithoutText(t *testing.T) {
	const token = "private.token.bytes"
	diagnostic := errors.New("private-authenticator-diagnostic for " + token)
	forbidden := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: resource}, token, diagnostic)
	}
	f := resulttest.New(t, "schema-resolve")
	b := f.Identity.Binding
	for name, row := range map[string]struct {
		answer func(context.CancelFunc, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)
		podErr error
		want   map[string]any
	}{
		"TokenReview forbidden": {
			answer: func(context.CancelFunc, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				return nil, forbidden("tokenreviews")
			},
			want: map[string]any{"msg": "TokenReview request failed", "code": 403.0, "reason": "Forbidden"},
		},
		"TokenReview throttled": {
			answer: func(context.CancelFunc, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				return nil, apierrors.NewTooManyRequests(diagnostic.Error(), 1)
			},
			want: map[string]any{"msg": "TokenReview request failed", "code": 429.0, "reason": "TooManyRequests"},
		},
		"TokenReview deadline": {
			answer: func(context.CancelFunc, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				return nil, &url.Error{Op: "Post", URL: "https://private.invalid/" + token, Err: context.DeadlineExceeded}
			},
			want: map[string]any{"msg": "TokenReview request failed", "deadlineExceeded": true},
		},
		"TokenReview transport": {
			answer: func(context.CancelFunc, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				return nil, diagnostic
			},
			want: map[string]any{"msg": "TokenReview request failed", "deadlineExceeded": false},
		},
		"TokenReview status error": {
			answer: func(_ context.CancelFunc, r *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				r.Status.Error = diagnostic.Error()
				return r, nil
			},
			want: map[string]any{"msg": "TokenReview returned an error, which is not logged"},
		},
		"TokenReview missing response": {
			answer: func(context.CancelFunc, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				return nil, nil
			},
			want: map[string]any{"msg": "TokenReview returned no review"},
		},
		"TokenReview outlived its context": {
			answer: func(cancel context.CancelFunc, r *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				cancel()
				return r, nil
			},
			want: map[string]any{"msg": "TokenReview answered after the verification context ended", "deadlineExceeded": false},
		},
		"Pod read forbidden": {
			podErr: forbidden("pods"),
			want:   map[string]any{"msg": "reading the Pod a token is bound to failed", "code": 403.0, "reason": "Forbidden"},
		},
		"Pod read transport": {
			podErr: diagnostic,
			want:   map[string]any{"msg": "reading the Pod a token is bound to failed", "deadlineExceeded": false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			ctx, cancel := context.WithCancel(logf.IntoContext(t.Context(), zap.New(zap.WriteTo(&out))))
			defer cancel()
			v := TokenVerifier{Reader: f.Client(t), Reviews: reviewFunc(func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				if row.answer == nil {
					return tokenReview(f), nil
				}
				return row.answer(cancel, tokenReview(f))
			})}
			if row.podErr != nil {
				v.Reader = failingPodReader{Reader: v.Reader, err: row.podErr}
			}
			if err := v.Verify(ctx, token, f.Identity); err == nil || errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("a verification with no verdict returned %v, want a retryable error", err)
			}
			if strings.Contains(out.String(), "private") {
				t.Fatalf("the log carries the token or the text of an answer:\n%s", out.String())
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != 1 || lines[0] == "" {
				t.Fatalf("want one log line, got %d:\n%s", len(lines), out.String())
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
				t.Fatalf("decode %q: %v", lines[0], err)
			}
			want := maps.Clone(row.want)
			maps.Copy(want, map[string]any{"level": "info", "logger": "result-receiver", "namespace": b.Namespace, "pod": b.PodName})
			delete(got, "ts")
			if !maps.Equal(got, want) {
				t.Fatalf("log line\n got %v\nwant %v", got, want)
			}
		})
	}
	t.Run("verdict", func(t *testing.T) {
		var out bytes.Buffer
		ctx := logf.IntoContext(t.Context(), zap.New(zap.WriteTo(&out)))
		v := TokenVerifier{Reader: f.Client(t), Reviews: reviewFunc(func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
			return tokenReview(f), nil
		})}
		if err := v.Verify(ctx, token, f.Identity); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatalf("a verification that reached its verdict logged:\n%s", out.String())
		}
	})
}
