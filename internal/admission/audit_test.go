package admission

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Each stamping webhook names, in the audit annotations of its answer, exactly
// the mutationRequestUID and username its patch writes: the values the API
// server stores, read back out of the patch rather than restated. The
// validating pass beside it and a refused create carry none, since neither
// stamped anything that could be stored.
func TestStampingWebhooksNameTheirStampInTheAuditAnnotations(t *testing.T) {
	t.Parallel()

	// The stamp trims the username, so the annotation has to carry the
	// trimmed value the object stores, not the one the request header held.
	const username = "alice@example.test"
	user := authenticationv1.UserInfo{Username: "  " + username + " ", UID: "idp-alice", Groups: []string{"approvers"}}

	cases := []struct {
		name string
		// identityField is the spec field the stamped username is stored under.
		identityField string
		// create returns the mutating handler, the validating handler beside
		// it, and the create a person sends; with refuse set, a create the
		// binding check refuses after the stamp was written.
		create func(t *testing.T, refuse bool) (mutating, validating cradmission.Handler, request cradmission.Request)
	}{
		{
			name: "PtahSchemaApproval", identityField: "approver",
			create: func(t *testing.T, refuse bool) (cradmission.Handler, cradmission.Handler, cradmission.Request) {
				handler, approval := readyFixture(t, true, true)
				if refuse {
					approval.Spec.PlanFingerprint = "sha256:another-plan"
				}
				validating := *handler
				validating.Mutate = false
				return handler, &validating, requestFor(t, approval, admissionv1.Create)
			},
		},
		{
			name: "PtahMigrationApproval", identityField: "approver",
			create: func(t *testing.T, refuse bool) (cradmission.Handler, cradmission.Handler, cradmission.Request) {
				handler, approval := migrationApprovalFixture(t, true, nil)
				if refuse {
					approval.Spec.PlanFingerprint = "sha256:another-plan"
				}
				validating := *handler
				validating.Mutate = false
				return handler, &validating, migrationApprovalRequest(t, approval, admissionv1.Create)
			},
		},
		{
			name: "PtahMigrationRunAcknowledgment", identityField: "acknowledgedBy",
			create: func(t *testing.T, refuse bool) (cradmission.Handler, cradmission.Handler, cradmission.Request) {
				handler, acknowledgment := runAcknowledgmentFixture(t, true, nil)
				if refuse {
					acknowledgment.Spec.OperationID = "sha256:another-run"
				}
				validating := *handler
				validating.Mutate = false
				return handler, &validating, runAcknowledgmentRequest(t, acknowledgment, admissionv1.Create)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			mutating, validating, request := tc.create(t, false)
			request.UserInfo = user
			response := mutating.Handle(ctx, request)
			if !response.Allowed {
				t.Fatalf("the mutating pass refused the create: %#v", response.Result)
			}
			stored := applyPatches(t, request.Object.Raw, response.Patches)
			stampedUID, _, _ := unstructured.NestedString(stored, "spec", "mutationRequestUID")
			stampedUsername, _, _ := unstructured.NestedString(stored, "spec", tc.identityField, "username")
			if stampedUID != string(request.UID) || stampedUsername != username {
				t.Fatalf("the patch stamps mutationRequestUID %q and username %q; want %q and %q",
					stampedUID, stampedUsername, request.UID, username)
			}
			want := map[string]string{"mutationRequestUID": stampedUID, "username": stampedUsername}
			if !maps.Equal(response.AuditAnnotations, want) {
				t.Fatalf("the mutating answer's audit annotations are %v; want exactly %v", response.AuditAnnotations, want)
			}

			// The validating webhook is a second call with a review UID of its
			// own. It admits the stamped object and stamps nothing.
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			validation := request
			validation.UID = "validating-review-uid"
			validation.Object = runtime.RawExtension{Raw: raw}
			verdict := validating.Handle(ctx, validation)
			if !verdict.Allowed {
				t.Fatalf("the validating pass refused the stamped object: %#v", verdict.Result)
			}
			if verdict.AuditAnnotations != nil {
				t.Fatalf("the validating answer carries audit annotations %v; it stamps nothing", verdict.AuditAnnotations)
			}

			refusing, _, refused := tc.create(t, true)
			refused.UserInfo = user
			refusal := refusing.Handle(ctx, refused)
			if refusal.Allowed {
				t.Fatal("the mutating pass admitted a create its binding check must refuse")
			}
			if refusal.AuditAnnotations != nil {
				t.Fatalf("the refusal carries audit annotations %v; nothing it stamped is stored", refusal.AuditAnnotations)
			}
		})
	}
}

// applyPatches applies a mutating answer's JSON patch to the object it
// answered, as the API server does, and decodes the result.
func applyPatches(t *testing.T, original []byte, patches any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(patches)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := jsonpatch.DecodePatch(encoded)
	if err != nil {
		t.Fatalf("decode the patch %s: %v", encoded, err)
	}
	patched, err := patch.Apply(original)
	if err != nil {
		t.Fatalf("apply the patch %s: %v", encoded, err)
	}
	object := map[string]any{}
	if err := json.Unmarshal(patched, &object); err != nil {
		t.Fatal(err)
	}
	return object
}
