package webhook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func publicationFixture(t *testing.T, issueCredential bool) (dispatchFixture, resultdelivery.Identity, resultstore.Store) {
	t.Helper()
	plane.Require(t)
	f := newDispatchFixture(t, "publication", "https://receiver.operator.svc:9444")
	job := f.job.DeepCopy()
	if err := admin.Create(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	pod := podFor(job)
	if err := clientAs(t, jobController).Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	f.schema.Status.ActiveOperation.JobUID = job.UID
	writeStatus(t, f.schema)
	grant(t, f.namespace, "result-publication", managerSubject(t),
		rbacv1.PolicyRule{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahresultrecords", "ptahschemas", "ptahmigrations"}, Verbs: []string{"get"}},
		rbacv1.PolicyRule{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahresultrecords"}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
	)
	apiClient := clientAs(t, manager.username)
	op := f.schema.Status.ActiveOperation
	identity := resultdelivery.Identity{Binding: resultstore.Binding{Namespace: f.namespace, Kind: "PtahSchema", Name: f.schema.Name, UID: f.schema.UID, Generation: f.schema.Generation, ExecutionBindingID: op.ExecutionBindingID, InputFingerprint: op.InputFingerprint, OperationID: op.ID, Operation: "resolve", JobName: job.Name, JobUID: job.UID, PodName: pod.Name, PodUID: pod.UID}}
	if issueCredential {
		issuer, err := resultcredentials.New(apiClient, apiClient, resultSigningCA, resultClientTrust, resultServerTrust)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := issuer.Ensure(t.Context(), identity); err != nil {
			t.Fatal(err)
		}
	}
	return f, identity, resultstore.Store{Client: apiClient, Reader: apiClient}
}

func publicationPayload(t *testing.T, identity resultdelivery.Identity) []byte {
	t.Helper()
	payload, err := resultdelivery.Encode(identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
func publicationDigest(payload []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
}

type publicationWriter struct {
	client.Client
	before func(*api.PtahResultRecord) error
	after  func(*api.PtahResultRecord) error
}

func (w publicationWriter) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	r := object.(*api.PtahResultRecord)
	if w.before != nil {
		if err := w.before(r); err != nil {
			return err
		}
	}
	if err := w.Client.Create(ctx, object, opts...); err != nil {
		return err
	}
	if w.after != nil {
		return w.after(r)
	}
	return nil
}

func TestResultPublicationAdmission(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := publicationPayload(t, identity)
	candidates := map[string]*api.PtahResultRecord{}
	guarded := store
	guarded.Client = publicationWriter{Client: store.Client, before: func(r *api.PtahResultRecord) error { candidates[r.Spec.Type] = r.DeepCopy(); return nil }}
	receipt, err := guarded.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload))
	if err != nil {
		t.Fatalf("actual publication failed through installed guards: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("did not create all publication roles: %v", candidates)
	}
	repeated, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload))
	if err != nil || repeated != receipt {
		t.Fatalf("identical retry: %v", err)
	}
	got, loaded, err := store.Load(t.Context(), identity.Binding)
	if err != nil || loaded != receipt || !bytes.Equal(got, payload) {
		t.Fatalf("publication readback: %v", err)
	}
	for role, candidate := range candidates {
		t.Run(role, func(t *testing.T) {
			requireDenied(t, admin.Create(t.Context(), candidate.DeepCopy(), client.DryRunAll), controllerWriteWebhook, "only the configured operator manager")
			stored := &api.PtahResultRecord{}
			if err := admin.Get(t.Context(), client.ObjectKeyFromObject(candidate), stored); err != nil {
				t.Fatal(err)
			}
			changed := stored.DeepCopy()
			changed.OwnerReferences[0].UID = "other"
			requireDenied(t, admin.Update(t.Context(), changed, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
			if err := admin.Update(t.Context(), stored.DeepCopy(), client.DryRunAll); err != nil {
				t.Fatalf("bookkeeping update: %v", err)
			}
			requireDenied(t, admin.Delete(t.Context(), stored, client.DryRunAll), controllerWriteWebhook, "retention has not authorized deletion")
			requireDenied(t, admin.DeleteAllOf(t.Context(), &api.PtahResultRecord{}, client.InNamespace(f.namespace), client.MatchingFields{"metadata.name": stored.Name}, client.DryRunAll), controllerWriteWebhook, "retention has not authorized deletion")
		})
	}
	t.Run("deletion refusal depends on webhook", func(t *testing.T) {
		r := &api.PtahResultRecord{}
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: receipt.Name}, r); err != nil {
			t.Fatal(err)
		}
		attempt := func() error { return admin.Delete(t.Context(), r, client.DryRunAll) }
		removed, index := removeValidatingEntry(t, controllerWriteWebhook)
		restored := false
		restore := func() {
			if !restored {
				restored = true
				insertValidatingEntry(t, removed, index)
			}
		}
		t.Cleanup(restore)
		if err := eventually(10*time.Second, attempt); err != nil {
			t.Fatalf("removing guard did not admit deletion: %v", err)
		}
		restore()
		if err := eventually(10*time.Second, func() error {
			if attempt() == nil {
				return errors.New("deletion still admitted")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		requireDenied(t, attempt(), controllerWriteWebhook, "retention has not authorized deletion")
	})
	// Retiring SQL authority does not establish that its result was consumed or
	// backed up. Do not delete completed evidence on that condition alone.
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	requireDenied(t, admin.DeleteAllOf(t.Context(), &api.PtahResultRecord{}, client.InNamespace(f.namespace), client.MatchingFields{"metadata.name": receipt.Name}, client.DryRunAll), controllerWriteWebhook, "retention has not authorized deletion")
}

func TestResultPublicationRefusesUnissuedOrRetiredAuthority(t *testing.T) {
	for _, mode := range []string{"unissued", "retired before completion", "invalid protocol"} {
		t.Run(mode, func(t *testing.T) {
			f, identity, store := publicationFixture(t, mode != "unissued")
			payload := publicationPayload(t, identity)
			if mode == "invalid protocol" {
				payload = []byte(`{"not":"a runner result"}`)
			}
			if mode == "retired before completion" {
				store.Client = publicationWriter{Client: store.Client, before: func(r *api.PtahResultRecord) error {
					if r.Spec.Type == "complete" {
						f.schema.Status.ActiveOperation = nil
						writeStatus(t, f.schema)
					}
					return nil
				}}
			}
			_, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload))
			requireDenied(t, err, controllerWriteWebhook, "immutable operation binding")
			name, _ := resultstore.Name(identity.Binding)
			if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: name + "-complete"}, &api.PtahResultRecord{}); !apierrors.IsNotFound(err) {
				t.Fatalf("refused publication has completion: %v", err)
			}
			if _, receipt, err := store.Load(t.Context(), identity.Binding); err == nil || receipt != (resultstore.Receipt{}) {
				t.Fatal("refused publication yielded a receipt")
			}
		})
	}
}

func TestResultPublicationResumesLostAPIWriteResponse(t *testing.T) {
	for _, role := range []string{"intent", "chunk", "complete"} {
		t.Run(role, func(t *testing.T) {
			_, identity, store := publicationFixture(t, true)
			payload := publicationPayload(t, identity)
			lost := errors.New("lost API acknowledgment")
			fired := false
			failing := store
			failing.Client = publicationWriter{Client: store.Client, after: func(r *api.PtahResultRecord) error {
				if r.Spec.Type == role && !fired {
					fired = true
					return lost
				}
				return nil
			}}
			if _, err := failing.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); !errors.Is(err, lost) {
				t.Fatalf("fault did not fire: %v", err)
			}
			if !fired {
				t.Fatal("publication skipped fault")
			}
			if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
				t.Fatalf("guard prevented identical resume: %v", err)
			}
		})
	}
}
