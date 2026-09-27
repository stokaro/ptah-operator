package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func specWriterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func writerTestSchema(annotations map[string]string) *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", Annotations: annotations},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "prod/team-a/app",
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "app-db"}, Key: "url"},
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/app:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml",
				},
			},
		},
	}
}

func writerTestMigration(annotations map[string]string) *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", Annotations: annotations},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "prod/team-a/app",
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "app-db"}, Key: "url"},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml",
				},
			},
		},
	}
}

func createWriterRequest(t *testing.T, object any, user authenticationv1.UserInfo) cradmission.Request {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "admission-uid", Namespace: "team-a", Name: "app",
		Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}, UserInfo: user,
	}}
}

func updateWriterRequest(t *testing.T, oldObject, newObject any, user authenticationv1.UserInfo) cradmission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(oldObject)
	if err != nil {
		t.Fatal(err)
	}
	newRaw, err := json.Marshal(newObject)
	if err != nil {
		t.Fatal(err)
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "admission-uid", Namespace: "team-a", Name: "app",
		Operation: admissionv1.Update, Object: runtime.RawExtension{Raw: newRaw},
		OldObject: runtime.RawExtension{Raw: oldRaw}, UserInfo: user,
	}}
}

// TestSchemaSpecWriterStampsAndOverwritesOnCreate proves that a create is
// always stamped from the authenticated request, whether or not the payload
// carried a forged value for either reserved annotation: the mutating webhook
// overwrites what the request carried, so an author cannot name someone else.
func TestSchemaSpecWriterStampsAndOverwritesOnCreate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		annotations map[string]string
	}{
		{name: "no annotations carried"},
		{name: "a forged writer carried", annotations: map[string]string{
			LastSpecWriterUsernameAnnotation: "mallory@example.com",
			LastSpecWriterUIDAnnotation:      "forged-uid",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler := &SchemaSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
			schema := writerTestSchema(test.annotations)
			request := createWriterRequest(t, schema, authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"})
			response := handler.Handle(context.Background(), request)
			if !response.Allowed {
				t.Fatalf("Handle() denied a create: %#v", response.Result)
			}
			patchJSON, err := json.Marshal(response.Patches)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"alice@example.com", "idp-123"} {
				if !containsJSON(patchJSON, want) {
					t.Fatalf("stamp patch %s does not contain %q", patchJSON, want)
				}
			}
			if containsJSON(patchJSON, "mallory@example.com") || containsJSON(patchJSON, "forged-uid") {
				t.Fatalf("stamp patch %s still carries the forged identity", patchJSON)
			}
		})
	}
}

// TestSchemaSpecWriterRestampsOnlyWhenSpecChanges proves the other half of the
// contract: an update that changes spec is restamped from its own requester,
// and an update that does not -- a finalizer, a label -- restores exactly what
// was recorded before, discarding whatever the request's own payload carried
// for the two reserved annotations. That is what keeps the manager's own
// finalizer and status writes from overwriting the recorded author, since
// neither changes spec.
func TestSchemaSpecWriterRestampsOnlyWhenSpecChanges(t *testing.T) {
	t.Parallel()

	recorded := map[string]string{
		LastSpecWriterUsernameAnnotation: "alice@example.com",
		LastSpecWriterUIDAnnotation:      "idp-123",
	}

	t.Run("a spec-changing update is restamped from its own requester", func(t *testing.T) {
		t.Parallel()
		handler := &SchemaSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
		old := writerTestSchema(recorded)
		updated := writerTestSchema(recorded)
		updated.Spec.Suspend = true
		request := updateWriterRequest(t, old, updated, authenticationv1.UserInfo{Username: "bob@example.com", UID: "idp-456"})
		response := handler.Handle(context.Background(), request)
		if !response.Allowed {
			t.Fatalf("Handle() denied a spec-changing update: %#v", response.Result)
		}
		patchJSON, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		if !containsJSON(patchJSON, "bob@example.com") || !containsJSON(patchJSON, "idp-456") {
			t.Fatalf("restamp patch %s does not name the new requester", patchJSON)
		}
		if containsJSON(patchJSON, "alice@example.com") {
			t.Fatalf("restamp patch %s still names the previous writer", patchJSON)
		}
	})

	t.Run("a metadata-only update discards a forged writer and keeps the recorded one", func(t *testing.T) {
		t.Parallel()
		handler := &SchemaSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
		old := writerTestSchema(recorded)
		forged := map[string]string{
			LastSpecWriterUsernameAnnotation: "mallory@example.com",
			LastSpecWriterUIDAnnotation:      "forged-uid",
			"unrelated-label":                "kept",
		}
		updated := writerTestSchema(forged)
		request := updateWriterRequest(t, old, updated, authenticationv1.UserInfo{Username: "mallory@example.com", UID: "forged-uid"})
		response := handler.Handle(context.Background(), request)
		if !response.Allowed {
			t.Fatalf("Handle() denied a metadata-only update: %#v", response.Result)
		}
		patchJSON, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		if containsJSON(patchJSON, "mallory@example.com") || containsJSON(patchJSON, "forged-uid") {
			t.Fatalf("metadata-only patch %s let a forged writer through", patchJSON)
		}
	})
}

// TestMigrationSpecWriterStampsAndOverwritesOnCreate mirrors the schema
// handler's create contract for PtahMigration.
func TestMigrationSpecWriterStampsAndOverwritesOnCreate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		annotations map[string]string
	}{
		{name: "no annotations carried"},
		{name: "a forged writer carried", annotations: map[string]string{
			LastSpecWriterUsernameAnnotation: "mallory@example.com",
			LastSpecWriterUIDAnnotation:      "forged-uid",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler := &MigrationSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
			migration := writerTestMigration(test.annotations)
			request := createWriterRequest(t, migration, authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"})
			response := handler.Handle(context.Background(), request)
			if !response.Allowed {
				t.Fatalf("Handle() denied a create: %#v", response.Result)
			}
			patchJSON, err := json.Marshal(response.Patches)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"alice@example.com", "idp-123"} {
				if !containsJSON(patchJSON, want) {
					t.Fatalf("stamp patch %s does not contain %q", patchJSON, want)
				}
			}
			if containsJSON(patchJSON, "mallory@example.com") || containsJSON(patchJSON, "forged-uid") {
				t.Fatalf("stamp patch %s still carries the forged identity", patchJSON)
			}
		})
	}
}

// TestSchemaSpecWriterProducesNoPatchForAFinalizerOnlyUpdate proves the
// property the chart's controller-write guard depends on without knowing this
// webhook exists: the manager's own finalizer add or remove changes nothing
// but metadata.finalizers, so when the recorded writer already matches --
// which it always does, since only this webhook ever sets it -- there is
// nothing to correct, and the mutation is an empty patch. A webhook that
// rewrote metadata.annotations here even to the same bytes would still risk
// looking like a change to a validator that compares the two objects, so the
// patch itself, not just the final value, has to be empty.
func TestSchemaSpecWriterProducesNoPatchForAFinalizerOnlyUpdate(t *testing.T) {
	t.Parallel()

	recorded := map[string]string{
		LastSpecWriterUsernameAnnotation: "alice@example.com",
		LastSpecWriterUIDAnnotation:      "idp-123",
	}
	handler := &SchemaSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
	old := writerTestSchema(recorded)
	updated := writerTestSchema(recorded)
	updated.Finalizers = []string{"operator.ptah.run/active-operation"}
	// The manager's own identity is irrelevant to the outcome: the guard is
	// about what changed, not who changed it.
	request := updateWriterRequest(t, old, updated, authenticationv1.UserInfo{Username: "system:serviceaccount:ptah-system:ptah-operator"})
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied a finalizer-only update: %#v", response.Result)
	}
	if len(response.Patches) != 0 {
		patchJSON, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("a finalizer-only update with an already-correct writer produced a patch: %s", patchJSON)
	}
}

// TestMigrationSpecWriterRestampsOnlyWhenSpecChanges mirrors the schema
// handler's update contract for PtahMigration.
func TestMigrationSpecWriterRestampsOnlyWhenSpecChanges(t *testing.T) {
	t.Parallel()

	recorded := map[string]string{
		LastSpecWriterUsernameAnnotation: "alice@example.com",
		LastSpecWriterUIDAnnotation:      "idp-123",
	}

	t.Run("a spec-changing update is restamped from its own requester", func(t *testing.T) {
		t.Parallel()
		handler := &MigrationSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
		old := writerTestMigration(recorded)
		updated := writerTestMigration(recorded)
		updated.Spec.Suspend = true
		request := updateWriterRequest(t, old, updated, authenticationv1.UserInfo{Username: "bob@example.com", UID: "idp-456"})
		response := handler.Handle(context.Background(), request)
		if !response.Allowed {
			t.Fatalf("Handle() denied a spec-changing update: %#v", response.Result)
		}
		patchJSON, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		if !containsJSON(patchJSON, "bob@example.com") || !containsJSON(patchJSON, "idp-456") {
			t.Fatalf("restamp patch %s does not name the new requester", patchJSON)
		}
		if containsJSON(patchJSON, "alice@example.com") {
			t.Fatalf("restamp patch %s still names the previous writer", patchJSON)
		}
	})

	t.Run("a metadata-only update discards a forged writer and keeps the recorded one", func(t *testing.T) {
		t.Parallel()
		handler := &MigrationSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
		old := writerTestMigration(recorded)
		forged := map[string]string{
			LastSpecWriterUsernameAnnotation: "mallory@example.com",
			LastSpecWriterUIDAnnotation:      "forged-uid",
			"unrelated-label":                "kept",
		}
		updated := writerTestMigration(forged)
		request := updateWriterRequest(t, old, updated, authenticationv1.UserInfo{Username: "mallory@example.com", UID: "forged-uid"})
		response := handler.Handle(context.Background(), request)
		if !response.Allowed {
			t.Fatalf("Handle() denied a metadata-only update: %#v", response.Result)
		}
		patchJSON, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		if containsJSON(patchJSON, "mallory@example.com") || containsJSON(patchJSON, "forged-uid") {
			t.Fatalf("metadata-only patch %s let a forged writer through", patchJSON)
		}
	})
}

// TestMigrationSpecWriterProducesNoPatchForAFinalizerOnlyUpdate mirrors the
// schema handler's finalizer-only contract for PtahMigration.
func TestMigrationSpecWriterProducesNoPatchForAFinalizerOnlyUpdate(t *testing.T) {
	t.Parallel()

	recorded := map[string]string{
		LastSpecWriterUsernameAnnotation: "alice@example.com",
		LastSpecWriterUIDAnnotation:      "idp-123",
	}
	handler := &MigrationSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))}
	old := writerTestMigration(recorded)
	updated := writerTestMigration(recorded)
	updated.Finalizers = []string{"operator.ptah.run/migration-operation"}
	request := updateWriterRequest(t, old, updated, authenticationv1.UserInfo{Username: "system:serviceaccount:ptah-system:ptah-operator"})
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied a finalizer-only update: %#v", response.Result)
	}
	if len(response.Patches) != 0 {
		patchJSON, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("a finalizer-only update with an already-correct writer produced a patch: %s", patchJSON)
	}
}

// The webhook records who wrote the spec; it must not edit the spec. A patch
// computed from a re-encoded typed object rewrote the author's own values -- an
// interval of "10m" came back as "10m0s", and a field this binary does not know
// was dropped -- so every operation has to stay under the two reserved keys.
func TestSpecWriterPatchesOnlyItsAnnotations(t *testing.T) {
	t.Parallel()

	user := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	for _, test := range []struct {
		name    string
		handler cradmission.Handler
		raw     string
	}{
		{
			name:    "schema",
			handler: &SchemaSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))},
			raw: `{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahSchema",` +
				`"metadata":{"namespace":"team-a","name":"app","annotations":{"kept":"yes"}},` +
				`"spec":{"interval":"10m","futureField":{"added":"by a newer API"},` +
				`"policy":{"allowDestructive":false,"lockTimeout":"30s"}}}`,
		},
		{
			name:    "migration without annotations",
			handler: &MigrationSpecWriterHandler{Decoder: cradmission.NewDecoder(specWriterScheme(t))},
			raw: `{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration",` +
				`"metadata":{"namespace":"team-a","name":"app"},` +
				`"spec":{"interval":"10m","futureField":1}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID: "admission-uid", Namespace: "team-a", Name: "app",
				Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: []byte(test.raw)}, UserInfo: user,
			}}
			response := test.handler.Handle(context.Background(), request)
			if !response.Allowed {
				t.Fatalf("Handle() denied a create: %#v", response.Result)
			}
			if len(response.Patches) == 0 {
				t.Fatal("Handle() stamped nothing on a create")
			}
			for _, patch := range response.Patches {
				if !strings.HasPrefix(patch.Path, "/metadata/annotations") {
					t.Fatalf("patch %s %s reaches outside the reserved annotations", patch.Operation, patch.Path)
				}
				if patch.Path == "/metadata/annotations" && patch.Operation != "add" {
					t.Fatalf("patch %s %s replaces the whole annotation map", patch.Operation, patch.Path)
				}
			}
		})
	}
}

// stampSpecWriter records user on object the way the webhook's patch does, for
// fixtures that need a resource whose spec someone already wrote.
func stampSpecWriter(object metav1.Object, user authenticationv1.UserInfo) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[LastSpecWriterUsernameAnnotation] = user.Username
	if user.UID != "" {
		annotations[LastSpecWriterUIDAnnotation] = user.UID
	} else {
		delete(annotations, LastSpecWriterUIDAnnotation)
	}
	object.SetAnnotations(annotations)
}
