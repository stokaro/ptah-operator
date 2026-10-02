package resultcredentials

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCredentialCleanupPreservesRecoveryAfterClaimRetires(t *testing.T) {
	for _, family := range []string{"schema-apply-admitted-scheduling", "migration-apply-admitted-scheduling"} {
		t.Run(family, func(t *testing.T) {
			f := resulttest.New(t, family)
			c := &credentialAPI{Client: f.Client(t)}
			issuer, _, _, _ := testIssuer(t, c)
			if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
				t.Fatal(err)
			}
			canonical := getCredential(t, c, f)
			projection := credentialProjection(canonical)
			projection.UID = "projection-uid"
			switch owner := f.Subject.(type) {
			case *api.PtahSchema:
				owner.Status.ActiveOperation = nil
				owner.Status.PendingObservation = &api.PendingObservationStatus{ApplyOperationID: f.Identity.Binding.OperationID}
			case *api.PtahMigration:
				owner.Status.ActiveOperation = nil
				owner.Status.UnresolvedRun = &api.UnresolvedMigrationRunStatus{OperationID: f.Identity.Binding.OperationID}
			}
			if err := c.Update(t.Context(), f.Subject); err != nil {
				t.Fatal(err)
			}
			record := &api.PtahResultRecord{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(canonical), record); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRecordDelete(t.Context(), secretForbiddenReader{Reader: c}, record); !errors.Is(err, resultretention.ErrPinned) {
				t.Fatalf("unsettled operation lost its credential record: %v", err)
			}
			// Even a lost parent must not let collection erase the projection
			// while the resource still needs the original execution evidence.
			if err := c.Client.Delete(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			if err := ValidateDelete(t.Context(), secretForbiddenReader{Reader: c}, projection); !errors.Is(err, resultretention.ErrPinned) {
				t.Fatalf("unsettled operation lost its orphaned projection: %v", err)
			}
		})
	}
}

func TestCredentialForegroundFinalizerRemoval(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, c)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	credential := getCredential(t, c, f)
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Secret, *corev1.Secret)
		allow  bool
	}{
		{name: "remove foreground finalizer", allow: true},
		{name: "owner has not started deleting", mutate: func(a, b *corev1.Secret) { a.DeletionTimestamp = nil }},
		{name: "clear deletion timestamp", mutate: func(a, b *corev1.Secret) { b.DeletionTimestamp = nil }},
		{name: "drop custom finalizer", mutate: func(a, b *corev1.Secret) { a.Finalizers = []string{"example.test/retain"} }},
		{name: "drop extra finalizer", mutate: func(a, b *corev1.Secret) { a.Finalizers = append(a.Finalizers, "example.test/retain") }},
		{name: "add finalizer", mutate: func(a, b *corev1.Secret) { a.Finalizers, b.Finalizers = nil, a.Finalizers }},
		{name: "change owner", mutate: func(a, b *corev1.Secret) { b.OwnerReferences[0].UID = "another-owner" }},
		{name: "change key", mutate: func(a, b *corev1.Secret) { b.Data["tls.key"] = []byte("changed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := credential.DeepCopy()
			now := metav1.Now()
			a.DeletionTimestamp, a.Finalizers = &now, []string{metav1.FinalizerDeleteDependents}
			b := a.DeepCopy()
			b.Finalizers = nil
			if test.mutate != nil {
				test.mutate(a, b)
			}
			if err := ValidateUpdate(a, b); (err == nil) != test.allow {
				t.Fatalf("Secret allow=%v: %v", test.allow, err)
			}
			oldRecord, err := credentialRecord(a)
			if err != nil {
				t.Fatal(err)
			}
			nextRecord, err := credentialRecord(b)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRecordUpdate(oldRecord, nextRecord); (err == nil) != test.allow {
				t.Fatalf("record allow=%v: %v", test.allow, err)
			}
		})
	}
}

type cleanupUnavailableReader struct {
	client.Reader
	owner bool
}

func (r cleanupUnavailableReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	_, record := object.(*api.PtahResultRecord)
	if record != r.owner {
		return errors.New("API unavailable")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func TestProjectionCleanupRequiresRetiredOwnerAndAbsentCanonicalRecord(t *testing.T) {
	for _, family := range []string{"schema-observe", "migration-history"} {
		for _, test := range []struct {
			name        string
			active      bool
			parent      string
			unavailable string
			expired     bool
			allow       bool
		}{
			{name: "active record", active: true, parent: "present"},
			{name: "retired record still retained", parent: "present"},
			{name: "retired record absent", parent: "absent", allow: true},
			{name: "active record absent", active: true, parent: "absent"},
			{name: "retired record replaced", parent: "replaced", allow: true},
			{name: "active record replaced", active: true, parent: "replaced"},
			{name: "record API unavailable", parent: "absent", unavailable: "record"},
			{name: "owner API unavailable", parent: "absent", unavailable: "owner"},
			{name: "expired retired credential", parent: "absent", expired: true, allow: true},
			{name: "expired active credential", parent: "absent", expired: true, active: true},
		} {
			t.Run(family+"/"+test.name, func(t *testing.T) {
				f := resulttest.New(t, family)
				c := &credentialAPI{Client: f.Client(t)}
				issuer, _, _, _ := testIssuer(t, c)
				if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
					t.Fatal(err)
				}
				canonical := getCredential(t, c, f)
				projection := credentialProjection(canonical)
				projection.UID = "projection-uid"
				if test.expired {
					expired, err := issuer.issue(f.Identity, canonical.Name, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					projection.Data = expired.Data
					pair, err := tls.X509KeyPair(expired.Data["tls.crt"], expired.Data["tls.key"])
					if err != nil {
						t.Fatal(err)
					}
					if _, err := resultdelivery.ClientIdentity(pair); err == nil {
						t.Fatal("expired reporting authority was accepted")
					}
				}
				if !test.active {
					switch owner := f.Subject.(type) {
					case *api.PtahSchema:
						owner.Status.ActiveOperation = nil
					case *api.PtahMigration:
						owner.Status.ActiveOperation = nil
					}
					if err := c.Update(t.Context(), f.Subject); err != nil {
						t.Fatal(err)
					}
				}
				record := &api.PtahResultRecord{}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(canonical), record); err != nil {
					t.Fatal(err)
				}
				if test.parent != "present" {
					if err := c.Delete(t.Context(), record); err != nil {
						t.Fatal(err)
					}
					if test.parent == "replaced" {
						record.UID, record.ResourceVersion = "replacement", ""
						if err := c.Client.Create(t.Context(), record); err != nil {
							t.Fatal(err)
						}
					}
				}
				var reader client.Reader = secretForbiddenReader{Reader: c}
				if test.unavailable != "" {
					reader = cleanupUnavailableReader{Reader: reader, owner: test.unavailable == "owner"}
				}
				err := ValidateDelete(t.Context(), reader, projection)
				if (err == nil) != test.allow {
					t.Fatalf("allow=%v, error=%v", test.allow, err)
				}
				if test.allow {
					for _, mutation := range []func(*corev1.Secret){
						func(s *corev1.Secret) { s.OwnerReferences[0].Name += "-other" },
						func(s *corev1.Secret) { s.Annotations[AnnotationOperationID] = "another-operation" },
						func(s *corev1.Secret) { s.Data["tls.key"] = []byte("invalid") },
						func(s *corev1.Secret) { s.Namespace = "another-namespace" },
					} {
						changed := projection.DeepCopy()
						mutation(changed)
						if err := ValidateDelete(t.Context(), reader, changed); err == nil {
							t.Fatal("mismatched orphan projection admitted")
						}
					}
				}
			})
		}
	}
}

func TestProjectionCreateRejectsLegacyResourceOwner(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, c)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	legacy := getCredential(t, c, f)
	if err := issuer.ValidateCreate(t.Context(), legacy); err == nil {
		t.Fatal("new projection bypasses canonical record ownership")
	}
	// An existing legacy Secret remains usable: issuance reads the record and
	// attempts the new projection, tolerating AlreadyExists without Secret GET.
	current := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(legacy), current); err != nil {
		t.Fatal(err)
	}
	current.OwnerReferences = legacy.OwnerReferences
	if err := c.Client.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); err != nil {
		t.Fatal(err)
	}
}
