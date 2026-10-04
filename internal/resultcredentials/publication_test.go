package resultcredentials

import (
	"strings"
	"testing"

	operatorapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"k8s.io/apimachinery/pkg/types"
)

func TestIssuedCredentialPublishesTheOriginalRetiringApply(t *testing.T) {
	f := resulttest.New(t, "schema-apply-admitted-scheduling")
	api := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, api)
	issued, err := issuer.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	schema := f.Subject.(*operatorapi.PtahSchema)
	op := schema.Status.ActiveOperation
	schema.Status.PendingObservation = &operatorapi.PendingObservationStatus{
		Outcome: operatorapi.PendingObservationOutcomeUnknown, ApplyOperationID: op.ID,
		ApplyJobName: op.JobName, ApplyJobUID: op.JobUID, ApplyGeneration: schema.Generation,
		ApplyPodCount: 1, ApplyPodUIDs: []types.UID{f.Pod.UID},
		AdmissionSnapshot: op.AdmissionSnapshot, Plan: *schema.Status.Plan, Target: *op.Target,
	}
	schema.Status.PendingBindingRetirement = &operatorapi.BindingRetirementStatus{
		RetiredEpoch: op.ExecutionBindingID,
		Job:          &operatorapi.RetiredJobStatus{Operation: operatorapi.OperationApply, Name: op.JobName, UID: op.JobUID},
	}
	schema.Status.ActiveOperation, schema.Status.Plan = nil, nil
	schema.Status.ExecutionBinding.Epoch = "v1-" + strings.Repeat("4", 32)
	if err := api.Update(t.Context(), schema); err != nil {
		t.Fatal(err)
	}
	if identity, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); err != nil || identity != f.Identity {
		t.Fatalf("original credential could not publish after binding rotation: %v", err)
	}
	retained, err := issuer.Ensure(t.Context(), f.Identity)
	if err != nil || retained != issued {
		t.Fatalf("retiring Apply did not retain its original credential: %v", err)
	}
}

func TestPublicationRequiresCanonicalCredentialAndLiveAuthority(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	api := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, api)
	if _, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); err == nil {
		t.Fatal("missing canonical credential authorized publication")
	}
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	if identity, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); err != nil || identity != f.Identity {
		t.Fatalf("issued credential not authorized: %v", err)
	}
	for name, change := range map[string]func(*resultstore.Binding){
		"Pod UID":      func(b *resultstore.Binding) { b.PodUID = "other" },
		"Job UID":      func(b *resultstore.Binding) { b.JobUID = "other" },
		"generation":   func(b *resultstore.Binding) { b.Generation++ },
		"namespace":    func(b *resultstore.Binding) { b.Namespace = "other" },
		"resource UID": func(b *resultstore.Binding) { b.UID = "other" },
		"operation":    func(b *resultstore.Binding) { b.Operation = "plan" },
		"operation ID": func(b *resultstore.Binding) { b.OperationID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			b := f.Identity.Binding
			change(&b)
			if _, err := issuer.AuthorizePublication(t.Context(), b); err == nil {
				t.Fatal("foreign binding authorized")
			}
		})
	}
	f.Subject.SetGeneration(f.Subject.GetGeneration() + 1)
	if err := api.Update(t.Context(), f.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding); err == nil {
		t.Fatal("issued credential survived authority retirement")
	}
}
