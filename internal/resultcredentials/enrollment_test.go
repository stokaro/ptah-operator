package resultcredentials

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func enrollmentMap(i *Issuer) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "operator", Name: "result-enrollment", UID: "policy-uid", ResourceVersion: "1"}, Data: EnrollmentData(i.ca.Raw, i.serverTrust)}
}
func bindEnrollment(t *testing.T, issuer *Issuer, reader client.Reader) *Issuer {
	t.Helper()
	policy, err := NewEnrollmentPolicy(reader, "operator", "result-enrollment")
	if err != nil {
		t.Fatal(err)
	}
	return issuer.WithEnrollmentPolicy(policy)
}
func TestEnrollmentRefusesStaleSignerWithoutChangingIssuedCredentials(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	apiClient := &credentialAPI{Client: f.Client(t)}
	original, ca, roots, trust := testIssuer(t, apiClient)
	policy := enrollmentMap(original)
	policy.ResourceVersion = ""
	if err := apiClient.Create(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	issuer := bindEnrollment(t, original, apiClient)
	if original.enrollment != nil {
		t.Fatal("binding mutated another issuer")
	}
	issued, err := issuer.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	secret := getCredential(t, apiClient, f)
	record, err := credentialRecord(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.ValidateRecordCreate(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	nextCA, _, _ := testCA(t, 7*24*time.Hour)
	policy.Data = EnrollmentData(nextCA.Certificate[0], trust)
	if err := apiClient.Update(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	if err := issuer.ValidateRecordCreate(t.Context(), record); !errors.Is(err, ErrCredential) {
		t.Fatalf("stale admission continued enrollment: %v", err)
	}
	if got, err := issuer.Ensure(t.Context(), f.Identity); err != nil || got != issued {
		t.Fatalf("policy advancement invalidated an issued credential: %v", err)
	}
	if err := issuer.ValidateCreate(t.Context(), secret); err != nil {
		t.Fatalf("existing projection lost authorization: %v", err)
	}
	if _, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); err != nil {
		t.Fatalf("existing operation lost delivery: %v", err)
	}

	// A replica that still has the old CA must read the same API policy before
	// generating a new canonical credential, even if its local trust is usable.
	freshAPI := &credentialAPI{Client: f.Client(t, policy)}
	stale, err := New(freshAPI, freshAPI, ca, roots, trust)
	if err != nil {
		t.Fatal(err)
	}
	stale = bindEnrollment(t, stale, freshAPI)
	if _, err := stale.Ensure(t.Context(), f.Identity); !errors.Is(err, ErrCredential) || freshAPI.creates.Load() != 0 {
		t.Fatalf("stale signer created a new record: calls=%d err=%v", freshAPI.creates.Load(), err)
	}
}

type enrollmentReader struct {
	client.Reader
	calls  int
	mutate func(*corev1.ConfigMap)
	err    error
	wait   bool
}

func (r *enrollmentReader) Get(ctx context.Context, key client.ObjectKey, out client.Object, opts ...client.GetOption) error {
	if policy, ok := out.(*corev1.ConfigMap); ok {
		r.calls++
		if r.wait {
			<-ctx.Done()
			return ctx.Err()
		}
		if r.err != nil {
			return r.err
		}
		if err := r.Reader.Get(ctx, key, out, opts...); err != nil {
			return err
		}
		if r.mutate != nil {
			r.mutate(policy)
		}
		return nil
	}
	return r.Reader.Get(ctx, key, out, opts...)
}
func TestEnrollmentRefusesUnavailableOrInvalidPolicy(t *testing.T) {
	for _, scenario := range []string{"missing", "unavailable", "canceled", "empty UID", "empty version", "terminating", "wrong namespace", "extra field", "binary data", "old bundle", "old signer"} {
		t.Run(scenario, func(t *testing.T) {
			f := resulttest.New(t, "schema-observe")
			apiClient := &credentialAPI{Client: f.Client(t)}
			issuer, _, _, _ := testIssuer(t, apiClient)
			policy := enrollmentMap(issuer)
			policy.ResourceVersion = ""
			if scenario != "missing" {
				if err := apiClient.Create(t.Context(), policy); err != nil {
					t.Fatal(err)
				}
			}
			reader := &enrollmentReader{Reader: apiClient}
			reader.mutate = func(p *corev1.ConfigMap) {
				switch scenario {
				case "empty UID":
					p.UID = ""
				case "empty version":
					p.ResourceVersion = ""
				case "terminating":
					now := metav1.Now()
					p.DeletionTimestamp = &now
				case "wrong namespace":
					p.Namespace = "tenant"
				case "extra field":
					p.Data["other"] = "unexpected"
				case "binary data":
					p.BinaryData = map[string][]byte{"unexpected": {1}}
				case "old bundle":
					p.Data["serverTrust"] = "sha256:other"
				case "old signer":
					p.Data["clientCA"] = "sha256:other"
				}
			}
			if scenario == "unavailable" {
				reader.err = errors.New("API unavailable")
			}
			if scenario == "canceled" {
				reader.wait = true
			}
			issuer = bindEnrollment(t, issuer, reader)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			if _, err := issuer.Ensure(ctx, f.Identity); err == nil || apiClient.creates.Load() != 0 {
				t.Fatalf("unsafe enrollment: %v", err)
			}
			if reader.calls != 1 {
				t.Fatalf("did not read the live policy: %d", reader.calls)
			}
		})
	}
}
func TestEnrollmentRechecksPolicyBeforeCanonicalWrite(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	apiClient := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, apiClient)
	policy := enrollmentMap(issuer)
	policy.ResourceVersion = ""
	if err := apiClient.Create(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	reader := &enrollmentReader{Reader: apiClient}
	reader.mutate = func(p *corev1.ConfigMap) {
		if reader.calls > 1 {
			p.Data["clientCA"] = "retired"
		}
	}
	issuer = bindEnrollment(t, issuer, reader)
	if _, err := issuer.Ensure(t.Context(), f.Identity); !errors.Is(err, ErrCredential) || apiClient.creates.Load() != 0 || reader.calls != 2 {
		t.Fatalf("policy changed during issuance: calls=%d err=%v", reader.calls, err)
	}
}
func TestEnrollmentOverlapDoesNotAuthorizeNewRecordsFromOldSigner(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	old, _, oldRoots, serverTrust := testIssuer(t, c)
	if _, err := old.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	record := &api.PtahResultRecord{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(getCredential(t, c, f)), record); err != nil {
		t.Fatal(err)
	}
	nextCA, _, _ := testCA(t, 7*24*time.Hour)
	parsed, err := x509.ParseCertificate(nextCA.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	oldRoots.AddCert(parsed)
	next, err := New(c, c, nextCA, oldRoots, serverTrust)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.ValidateRecordCreate(t.Context(), record); !errors.Is(err, ErrCredential) {
		t.Fatalf("overlap authorized old signer enrollment: %v", err)
	}
	if err := next.ValidateCreate(t.Context(), getCredential(t, c, f)); err != nil {
		t.Fatalf("overlap refused existing projection: %v", err)
	}
}
