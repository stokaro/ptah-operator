package resultcredentials

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

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
