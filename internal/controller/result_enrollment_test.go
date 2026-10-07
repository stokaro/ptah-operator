package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type enrollmentAPI struct{ client.Client }

func (c enrollmentAPI) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	object.SetUID("pin-uid")
	return c.Client.Create(ctx, object, opts...)
}

type enrollmentProbe struct {
	resultcredentials.PodBindings
	calls int
}

func (p *enrollmentProbe) Ensure(ctx context.Context, identity resultdelivery.Identity) (resultcredentials.Credential, error) {
	p.calls++
	return p.PodBindings.Ensure(ctx, identity)
}

type enrollmentReader struct {
	client.Reader
	reads int
	fail  error
}

func (r *enrollmentReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.reads++
	if r.fail != nil {
		return r.fail
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func (r *enrollmentReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.reads++
	return r.Reader.List(ctx, list, opts...)
}

func enrollmentTrust(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCompletedPodEnrollmentIsNotRepeatedByJobEvents(t *testing.T) {
	for _, name := range []string{"schema-observe", "migration-history"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.NewPodToken(t, name, enrollmentTrust(t))
			c := enrollmentAPI{f.Client(t)}
			reader := &enrollmentReader{Reader: c}
			issuer := &enrollmentProbe{PodBindings: resultcredentials.PodBindings{Writer: c, Reader: reader}}
			if ready, err := invokeIssuance(t.Context(), f, reader, issuer); err != nil || !ready || issuer.calls != 1 {
				t.Fatalf("first enrollment: ready=%v calls=%d err=%v", ready, issuer.calls, err)
			}
			reader.reads = 0
			for range 5 {
				if ready, err := invokeIssuance(t.Context(), f, reader, issuer); err != nil || !ready {
					t.Fatalf("existing enrollment: ready=%v err=%v", ready, err)
				}
			}
			if issuer.calls != 1 || reader.reads > 10 {
				t.Fatalf("Job events repeated enrollment: calls=%d reads=%d, want one issuance and at most two reads per event", issuer.calls, reader.reads)
			}
		})
	}
}

func TestObservedEnrollmentDoesNotGrantPublicationAuthority(t *testing.T) {
	for _, name := range []string{"schema-observe", "migration-history"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.NewPodToken(t, name, enrollmentTrust(t))
			c := enrollmentAPI{f.Client(t)}
			issuer := &enrollmentProbe{PodBindings: resultcredentials.PodBindings{Writer: c, Reader: c}}
			if ready, err := invokeIssuance(t.Context(), f, c, issuer); err != nil || !ready {
				t.Fatal(ready, err)
			}
			// Reconciliation can still hold the old subject snapshot while the
			// direct API has already revoked the claim. Observing an existing
			// public pin must not give the runner permission to publish.
			revoked := f.Subject.DeepCopyObject().(client.Object)
			switch object := revoked.(type) {
			case *api.PtahSchema:
				object.Status.ActiveOperation = nil
			case *api.PtahMigration:
				object.Status.ActiveOperation = nil
			}
			if err := c.Update(t.Context(), revoked); err != nil {
				t.Fatal(err)
			}
			if ready, err := invokeIssuance(t.Context(), f, c, issuer); err != nil || !ready || issuer.calls != 1 {
				t.Fatalf("observing the persisted pin reissued it: ready=%v calls=%d err=%v", ready, issuer.calls, err)
			}
			if _, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("retired claim retained publication authority: %v", err)
			}
		})
	}
}

func TestExistingPodEnrollmentStillRefusesReplacementAndReadFailure(t *testing.T) {
	for _, scenario := range []string{"replacement Pod", "unavailable API", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := resulttest.NewPodToken(t, "schema-observe", enrollmentTrust(t))
			c := enrollmentAPI{f.Client(t)}
			issuer := &enrollmentProbe{PodBindings: resultcredentials.PodBindings{Writer: c, Reader: c}}
			if ready, err := invokeIssuance(t.Context(), f, c, issuer); err != nil || !ready {
				t.Fatal(ready, err)
			}
			reader := &enrollmentReader{Reader: c}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "replacement Pod":
				if err := c.Delete(t.Context(), f.Pod); err != nil {
					t.Fatal(err)
				}
				replacement := f.Pod.DeepCopy()
				replacement.UID, replacement.ResourceVersion = "replacement-pod", ""
				if err := c.Client.Create(t.Context(), replacement); err != nil {
					t.Fatal(err)
				}
			case "unavailable API":
				reader.fail = errors.New("API unavailable")
			case "canceled":
				cancel()
			}
			if ready, err := invokeIssuance(ctx, f, reader, issuer); err == nil || ready || issuer.calls != 1 {
				t.Fatalf("changed or unreadable enrollment reached the issuer: ready=%v calls=%d err=%v", ready, issuer.calls, err)
			}
		})
	}
}
