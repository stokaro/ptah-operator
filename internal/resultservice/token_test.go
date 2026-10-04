package resultservice

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/runner"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type tokenReviewFunc func(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)

func (f tokenReviewFunc) Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	return f(ctx, review, opts)
}

type withoutSecretWrites struct{ *identifyingAPI }

func (c withoutSecretWrites) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, secret := obj.(*corev1.Secret); secret {
		return errors.New("Secret CREATE forbidden")
	}
	return c.identifyingAPI.Create(ctx, obj, opts...)
}

func TestPodTokenServicePinsPublishesAndRestartsWithoutSecrets(t *testing.T) {
	config := mountedTrust(t)
	trust, err := os.ReadFile(filepath.Join(config.CertificateDirectory, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	f := resulttest.NewPodToken(t, "schema-observe", trust)
	c := withoutSecretWrites{&identifyingAPI{Client: f.Client(t)}}
	var reviews atomic.Int64
	config.TokenReviews = tokenReviewFunc(func(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
		reviews.Add(1)
		if review.Spec.Token != "bound.pod.token" || len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != resultdelivery.TokenAudience {
			return nil, errors.New("wrong token request")
		}
		return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{resultdelivery.TokenAudience}, User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:" + f.Pod.Namespace + ":" + f.Pod.Spec.ServiceAccountName, UID: "account-uid",
			Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {f.Pod.Name}, "authentication.kubernetes.io/pod-uid": {string(f.Pod.UID)}},
		}}}, nil
	})
	s, err := New(config, c, noSecrets{Reader: c})
	if err != nil {
		t.Fatal(err)
	}
	stop := running(t, s)
	if s.trust.Load().tls.ClientCAs != nil || s.trust.Load().tls.ClientAuth != tls.NoClientCert {
		t.Fatal("token receiver still requires client certificates")
	}
	if _, err := s.AuthorizePublication(t.Context(), f.Identity.Binding); !errors.Is(err, resultauthority.ErrNotReady) {
		t.Fatalf("unpinned Pod authorized: %v", err)
	}
	issued, err := s.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	record := &api.PtahResultRecord{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Pod.Namespace, Name: issued.Name}, record); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRecordCreate(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if s.ValidateCreate(t.Context(), &corev1.Secret{}) == nil {
		t.Fatal("token service admitted a credential Secret")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(s.ServerTrust())
	policy := resultdelivery.RetryPolicy{Attempts: 2, Interval: time.Millisecond, AttemptTimeout: time.Second, TotalTimeout: 2 * time.Second}
	newSender := func(identity resultdelivery.Identity) *resultdelivery.Sender {
		sender, err := resultdelivery.NewTokenSender("https://"+s.address, identity, &tls.Config{RootCAs: roots}, policy, func() (string, error) { return "bound.pod.token", nil })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sender.Close)
		return sender
	}
	sender := newSender(f.Identity)
	if err := sender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	value := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}}
	payload, err := resultdelivery.Encode(f.Identity, value)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	s, err = New(config, c, noSecrets{Reader: c})
	if err != nil {
		t.Fatal(err)
	}
	stop = running(t, s)
	defer stop()
	again, err := s.Ensure(t.Context(), f.Identity)
	if err != nil || again != issued {
		t.Fatalf("restart replaced Pod pin: %v", err)
	}
	second, err := newSender(f.Identity).Send(t.Context(), payload)
	if err != nil || second != first {
		t.Fatalf("restart changed durable receipt: %v", err)
	}
	identity, err := resultcredentials.StoredRecordIdentity(record)
	if err != nil || identity != f.Identity {
		t.Fatal("record does not retain only the exact public identity")
	}
	secrets := &corev1.SecretList{}
	if err := c.List(t.Context(), secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("operations created Secrets: %d, %v", len(secrets.Items), err)
	}
	for name, mutate := range map[string]func(*resultdelivery.Identity){
		"foreign resource": func(i *resultdelivery.Identity) { i.Binding.UID = "foreign-resource-uid" },
		"generation":       func(i *resultdelivery.Identity) { i.Binding.Generation++ },
		"epoch": func(i *resultdelivery.Identity) {
			i.Binding.ExecutionBindingID = "v1-00000000000000000000000000000000"
		},
		"operation attempt": func(i *resultdelivery.Identity) { i.Binding.OperationID = "foreign-attempt" },
		"replaced Job":      func(i *resultdelivery.Identity) { i.Binding.JobUID = "replacement-job-uid" },
	} {
		t.Run(name, func(t *testing.T) {
			identity := f.Identity
			mutate(&identity)
			before := reviews.Load()
			if err := newSender(identity).Check(t.Context()); err == nil || err.Error() != "result receiver preflight returned HTTP 403" {
				t.Fatalf("changed operation claim was not a terminal authority refusal: %v", err)
			}
			if got := reviews.Load() - before; got != 1 {
				t.Fatalf("invalid immutable claim was retried %d times", got)
			}
		})
	}
	t.Run("valid claim still waits for its pin", func(t *testing.T) {
		if err := c.Delete(t.Context(), record); err != nil {
			t.Fatal(err)
		}
		before := reviews.Load()
		if err := newSender(f.Identity).Check(t.Context()); err == nil || err.Error() != "result receiver preflight returned HTTP 503" {
			t.Fatalf("enrollment delay lost its temporary refusal: %v", err)
		}
		if got := reviews.Load() - before; got != 2 {
			t.Fatalf("valid unpinned claim did not use its bounded retry: %d reviews", got)
		}
	})
}
