package resultcredentials

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
)

func TestCredentialCreateAdmission(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, c)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	secret := getCredential(t, c, f)
	secret.UID = ""
	if err := issuer.ValidateCreate(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	var disabled *Issuer
	if err := disabled.ValidateCreate(t.Context(), secret); err == nil {
		t.Fatal("disabled issuer admitted a credential")
	}
	other, _, _, _ := testIssuer(t, c)
	if err := other.ValidateCreate(t.Context(), secret); err == nil {
		t.Fatal("foreign signer admitted")
	}
	for name, mutate := range map[string]func(*corev1.Secret){
		"pod UID":       func(s *corev1.Secret) { s.Annotations[AnnotationPodUID] = "replacement" },
		"pod name":      func(s *corev1.Secret) { s.Annotations[AnnotationPodName] = "replacement" },
		"operation":     func(s *corev1.Secret) { s.Annotations[AnnotationOperationID] = "replacement" },
		"job UID":       func(s *corev1.Secret) { s.Annotations[AnnotationJobUID] = "replacement" },
		"owner":         func(s *corev1.Secret) { s.OwnerReferences[0].UID = "replacement" },
		"mutable":       func(s *corev1.Secret) { s.Immutable = nil },
		"finalizer":     func(s *corev1.Secret) { s.Finalizers = []string{"other/finalizer"} },
		"string data":   func(s *corev1.Secret) { s.StringData = map[string]string{"tls.key": "replacement"} },
		"generate name": func(s *corev1.Secret) { s.GenerateName = "credential-" },
		"wrong name":    func(s *corev1.Secret) { s.Name += "-other" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := secret.DeepCopy()
			mutate(copy)
			if err := issuer.ValidateCreate(t.Context(), copy); err == nil {
				t.Fatal("tampered credential admitted")
			}
		})
	}
	subject := f.Subject.(*api.PtahSchema).DeepCopy()
	subject.Status.ActiveOperation = nil
	if err := c.Update(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	if err := issuer.ValidateCreate(t.Context(), secret); err == nil {
		t.Fatal("retired credential admitted")
	}
}

func TestCredentialUpdateAdmission(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, c)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	old := getCredential(t, c, f)
	bookkeeping := old.DeepCopy()
	bookkeeping.ResourceVersion = "new"
	bookkeeping.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "other"}}
	if err := ValidateUpdate(old, bookkeeping); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1.Secret){
		"drop binding":    func(s *corev1.Secret) { s.Annotations = nil },
		"replace binding": func(s *corev1.Secret) { s.Annotations[AnnotationPodUID] = "replacement" },
		"drop labels":     func(s *corev1.Secret) { s.Labels = nil },
		"drop owner":      func(s *corev1.Secret) { s.OwnerReferences = nil },
		"orphan":          func(s *corev1.Secret) { s.OwnerReferences[0].BlockOwnerDeletion = nil },
		"replace key":     func(s *corev1.Secret) { s.Data["tls.key"] = []byte("replacement") },
		"add finalizer":   func(s *corev1.Secret) { s.Finalizers = []string{"other/finalizer"} },
		"replace UID":     func(s *corev1.Secret) { s.UID = "replacement" },
	} {
		t.Run(name, func(t *testing.T) {
			next := old.DeepCopy()
			mutate(next)
			if err := ValidateUpdate(old, next); err == nil {
				t.Fatal("binding mutation admitted")
			}
		})
	}
}

type unavailableCredentialReader struct{ client.Reader }

func (r unavailableCredentialReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("unavailable")
}

func TestCredentialDeleteAdmission(t *testing.T) {
	for _, name := range []string{"schema-observe", "migration-history"} {
		t.Run(name, func(t *testing.T) {
			for _, state := range []string{"active", "terminating", "generation changed", "lease lost", "Pod and Job absent", "retired", "next operation", "owner absent", "replacement owner", "API unavailable"} {
				t.Run(state, func(t *testing.T) {
					f := resulttest.New(t, name)
					c := &credentialAPI{Client: f.Client(t)}
					issuer, _, _, _ := testIssuer(t, c)
					if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
						t.Fatal(err)
					}
					secret := getCredential(t, c, f)
					subject := f.Subject.DeepCopyObject().(client.Object)
					allow := false
					var reader client.Reader = c
					switch state {
					case "terminating":
						subject.SetFinalizers([]string{"test/retain"})
						if err := c.Update(t.Context(), subject); err != nil {
							t.Fatal(err)
						}
						if err := c.Delete(t.Context(), subject); err != nil {
							t.Fatal(err)
						}
					case "generation changed":
						subject.SetGeneration(subject.GetGeneration() + 1)
					case "lease lost":
						switch s := subject.(type) {
						case *api.PtahSchema:
							s.Status.ActiveOperation.LeaseContinuityLost = true
						case *api.PtahMigration:
							s.Status.ActiveOperation.LeaseContinuityLost = true
						}
					case "Pod and Job absent":
						if err := c.Delete(t.Context(), f.Pod); err != nil {
							t.Fatal(err)
						}
						if err := c.Delete(t.Context(), f.Job); err != nil {
							t.Fatal(err)
						}
					case "retired":
						allow = true
						switch s := subject.(type) {
						case *api.PtahSchema:
							s.Status.ActiveOperation = nil
						case *api.PtahMigration:
							s.Status.ActiveOperation = nil
						}
					case "next operation":
						allow = true
						switch s := subject.(type) {
						case *api.PtahSchema:
							s.Status.ActiveOperation.ID = "next"
						case *api.PtahMigration:
							s.Status.ActiveOperation.ID = "next"
						}
					case "owner absent":
						allow = true
						if err := c.Delete(t.Context(), subject); err != nil {
							t.Fatal(err)
						}
					case "replacement owner":
						allow = true
						subject.SetUID("replacement")
					case "API unavailable":
						reader = unavailableCredentialReader{Reader: c}
					}
					if state != "terminating" && state != "owner absent" {
						if err := c.Update(t.Context(), subject); err != nil {
							t.Fatal(err)
						}
					}
					err := ValidateDelete(t.Context(), reader, secret)
					if (err == nil) != allow {
						t.Fatalf("allow=%v, error=%v", allow, err)
					}
					record, err := credentialRecord(secret)
					if err != nil {
						t.Fatal(err)
					}
					if err := ValidateRecordDelete(t.Context(), reader, record); (err == nil) != allow {
						t.Fatalf("record allow=%v, error=%v", allow, err)
					}
				})
			}
		})
	}
}

func TestCredentialRecordAdmission(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, c)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	secret := getCredential(t, c, f)
	record, err := credentialRecord(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.ValidateRecordCreate(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	var disabled *Issuer
	if err := disabled.ValidateRecordCreate(t.Context(), record); err == nil {
		t.Fatal("disabled issuer admitted record")
	}
	bookkeeping := record.DeepCopy()
	bookkeeping.ResourceVersion = "next"
	if err := ValidateRecordUpdate(record, bookkeeping); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*api.PtahResultRecord){
		"foreign Pod":        func(r *api.PtahResultRecord) { r.Annotations[AnnotationPodUID] = "other" },
		"foreign owner":      func(r *api.PtahResultRecord) { r.OwnerReferences[0].UID = "other" },
		"foreign role":       func(r *api.PtahResultRecord) { r.Spec.Type = "chunk" },
		"invalid bytes":      func(r *api.PtahResultRecord) { r.Spec.Data = []byte("invalid") },
		"noncanonical bytes": func(r *api.PtahResultRecord) { r.Spec.Data = append(r.Spec.Data, ' ') },
		"finalizer":          func(r *api.PtahResultRecord) { r.Finalizers = []string{"other/finalizer"} },
		"additional label":   func(r *api.PtahResultRecord) { r.Labels["other"] = "value" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := record.DeepCopy()
			mutate(changed)
			if err := issuer.ValidateRecordCreate(t.Context(), changed); err == nil {
				t.Fatal("invalid record admitted")
			}
			if err := ValidateRecordUpdate(record, changed); err == nil {
				t.Fatal("record mutation admitted")
			}
		})
	}
	if err := ValidateRecordDelete(t.Context(), c, record); err == nil {
		t.Fatal("active record deletion admitted")
	}
	// Another valid certificate for the same identity is not the persisted key.
	other, err := issuer.issue(f.Identity, secret.Name, time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.ValidateCreate(t.Context(), other); err == nil {
		t.Fatal("projection replaced the canonical key with another trusted key")
	}
	owner := f.Subject.(*api.PtahSchema).DeepCopy()
	owner.Status.ActiveOperation = nil
	if err := c.Update(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordDelete(t.Context(), c, record); err != nil {
		t.Fatalf("retired record cleanup refused: %v", err)
	}
}
