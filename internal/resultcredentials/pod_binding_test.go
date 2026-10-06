package resultcredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPodBindingStoresNoSecretForAnyOperation(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			c := &credentialAPI{Client: f.Client(t)}
			pins := PodBindings{Writer: c, Reader: secretForbiddenReader{Reader: c}}
			if _, err := pins.AuthorizePublication(t.Context(), f.Identity.Binding); !errors.Is(err, resultauthority.ErrNotReady) {
				t.Fatalf("missing pin did not wait for enrollment: %v", err)
			}
			first, err := pins.Ensure(t.Context(), f.Identity)
			if err != nil || first.UID == "" || !first.NotAfter.IsZero() {
				t.Fatalf("pin failed: %+v, %v", first, err)
			}
			if next, err := pins.Ensure(t.Context(), f.Identity); err != nil || next != first || c.creates.Load() != 1 || c.projections.Load() != 0 {
				t.Fatalf("repeated enrollment changed the pin or created a Secret: %+v, %v", next, err)
			}
			secrets := &corev1.SecretList{}
			if err := c.List(t.Context(), secrets); err != nil || len(secrets.Items) != 0 {
				t.Fatalf("pinning created Secret objects: %v", err)
			}
			stored := &api.PtahResultRecord{}
			if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Pod.Namespace, Name: first.Name}, stored); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(stored.Spec.Data, []byte("tls.key")) || bytes.Contains(stored.Spec.Data, []byte("token")) {
				t.Fatal("the public pin carries secret material")
			}
			if identity, err := StoredRecordIdentity(stored); err != nil || identity != f.Identity {
				t.Fatalf("retention cannot read the exact pin: %v", err)
			}
			if identity, err := pins.AuthorizePublication(t.Context(), f.Identity.Binding); err != nil || identity != f.Identity {
				t.Fatalf("the original operation cannot publish: %v", err)
			}
			if err := pins.ValidateRecordCreate(t.Context(), stored); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRecordDelete(t.Context(), c, stored); err == nil {
				t.Fatal("the active first-Pod pin could be deleted")
			}
		})
	}
}

func TestPodBindingSurvivesLostAcknowledgmentAndConcurrentEnrollment(t *testing.T) {
	f := resulttest.New(t, "migration-apply-admitted-scheduling")
	c := &credentialAPI{Client: f.Client(t)}
	pins := PodBindings{Writer: c, Reader: c}
	c.lostACK.Store(true)
	if _, err := pins.Ensure(t.Context(), f.Identity); err == nil {
		t.Fatal("a lost CREATE response was acknowledged")
	}
	first, err := pins.Ensure(t.Context(), f.Identity)
	if err != nil || c.creates.Load() != 1 {
		t.Fatalf("retry did not recover the first pin: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if next, err := pins.Ensure(t.Context(), f.Identity); err != nil || next != first {
				t.Errorf("concurrent enrollment changed the winner: %+v, %v", next, err)
			}
		})
	}
	wg.Wait()
}

func TestPodBindingRefusesReplacementAfterOriginalPodDeletion(t *testing.T) {
	f := resulttest.New(t, "schema-apply-admitted-scheduling")
	c := &credentialAPI{Client: f.Client(t)}
	pins := PodBindings{Writer: c, Reader: c}
	if _, err := pins.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), f.Pod); err != nil {
		t.Fatal(err)
	}
	if _, err := pins.AuthorizePublication(t.Context(), f.Identity.Binding); err == nil {
		t.Fatal("a deleted Pod retained live publication authority")
	}
	replacement := f.Pod.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "replacement-pod-uid", ""
	if err := c.Client.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	identity := f.Identity
	identity.Binding.PodUID = replacement.UID
	// With only the replacement present, the live graph alone admits it.
	// The retained first-Pod pin is what must refuse re-enrollment.
	if err := (resultauthority.Authorizer{Reader: c}).Check(t.Context(), identity); err != nil {
		t.Fatalf("replacement did not reach the immutable pin check: %v", err)
	}
	if _, err := pins.Ensure(t.Context(), identity); !errors.Is(err, ErrCredential) {
		t.Fatalf("replacement enrollment was not refused by the pin: %v", err)
	}
	if _, err := pins.AuthorizePublication(t.Context(), identity.Binding); !errors.Is(err, ErrCredential) {
		t.Fatalf("replacement publication was not refused by the pin: %v", err)
	}
}

func TestPodBindingRejectsChangedIdentityMetadataAndNoncanonicalBytes(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := &credentialAPI{Client: f.Client(t)}
	pins := PodBindings{Writer: c, Reader: c}
	credential, err := pins.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	stored := &api.PtahResultRecord{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Pod.Namespace, Name: credential.Name}, stored); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*api.PtahResultRecord){
		"Pod annotation":     func(r *api.PtahResultRecord) { r.Annotations[AnnotationPodUID] = "other" },
		"owner":              func(r *api.PtahResultRecord) { r.OwnerReferences[0].UID = "other" },
		"record name":        func(r *api.PtahResultRecord) { r.Name += "-other" },
		"extra label":        func(r *api.PtahResultRecord) { r.Labels["other"] = "value" },
		"noncanonical bytes": func(r *api.PtahResultRecord) { r.Spec.Data = append(r.Spec.Data, '\n') },
		"extra field":        func(r *api.PtahResultRecord) { r.Spec.Data = append([]byte(`{"token":"secret",`), r.Spec.Data[1:]...) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := stored.DeepCopy()
			mutate(changed)
			if err := pins.ValidateRecordCreate(t.Context(), changed); err == nil {
				t.Fatal("changed pin passed admission")
			}
			if err := ValidateRecordUpdate(stored, changed); err == nil {
				t.Fatal("changed pin passed update admission")
			}
		})
	}
	bookkeeping := stored.DeepCopy()
	bookkeeping.ResourceVersion = "later"
	bookkeeping.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "apiserver"}}
	if err := ValidateRecordUpdate(stored, bookkeeping); err != nil {
		t.Fatalf("ordinary API bookkeeping was refused: %v", err)
	}
	if !reflect.DeepEqual(stored.Spec, bookkeeping.Spec) {
		t.Fatal("fixture changed the immutable spec")
	}
}

func TestPodBindingChecksAuthorityAtTheWriteAndReturnBoundaries(t *testing.T) {
	for _, operation := range []string{"schema-observe", "migration-history"} {
		for _, existing := range []bool{false, true} {
			for _, revoked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/existing=%v/revoked=%v", operation, existing, revoked), func(t *testing.T) {
					f := resulttest.New(t, operation)
					c := &credentialAPI{Client: f.Client(t)}
					pins := PodBindings{Writer: c, Reader: c}
					if existing {
						if _, err := pins.Ensure(t.Context(), f.Identity); err != nil {
							t.Fatal(err)
						}
					}
					pin, err := podBindingRecord(f.Identity)
					if err != nil {
						t.Fatal(err)
					}
					reads, createsBefore := 0, c.creates.Load()
					pins.Reader = interceptor.NewClient(c.Client.(client.WithWatch), interceptor.Funcs{
						Get: func(ctx context.Context, base client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
							reads++
							err := base.Get(ctx, key, object, opts...)
							if key == client.ObjectKeyFromObject(pin) && revoked {
								// Revoke after the pin read, including a NotFound read.
								// A prior authorization cannot permit a later write or return.
								subject := f.Subject.DeepCopyObject().(client.Object)
								if e := base.Get(ctx, client.ObjectKeyFromObject(subject), subject); e != nil {
									t.Fatal(e)
								}
								switch value := subject.(type) {
								case *api.PtahSchema:
									value.Status.ActiveOperation = nil
								case *api.PtahMigration:
									value.Status.ActiveOperation = nil
								}
								if e := base.Update(ctx, subject); e != nil {
									t.Fatal(e)
								}
							}
							return err
						},
						List: func(ctx context.Context, base client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
							reads++
							return base.List(ctx, list, opts...)
						},
					})
					credential, err := pins.Ensure(t.Context(), f.Identity)
					if revoked {
						if err == nil || credential.UID != "" || c.creates.Load() != createsBefore {
							t.Fatalf("revoked operation wrote or received a pin: credential=%+v err=%v", credential, err)
						}
						return
					}
					if err != nil || credential.UID == "" {
						t.Fatal("valid operation was refused", err)
					}
					// One seven-read authority check before return; creating a pin
					// also needs a check before CREATE and its persisted readback.
					budget := 8
					if !existing {
						budget = 16
					}
					if reads > budget {
						t.Fatalf("enrollment used %d reads, budget %d", reads, budget)
					}
				})
			}
		}
	}
}
