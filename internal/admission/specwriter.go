package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// LastSpecWriterUsernameAnnotation and LastSpecWriterUIDAnnotation record the
// authenticated identity that created a PtahSchema or a PtahMigration, or last
// changed its spec. Only SchemaSpecWriterHandler and MigrationSpecWriterHandler
// write them: each overwrites whatever a request carried for these two keys,
// so an author cannot name someone else, and each leaves them exactly as they
// were on a request that does not change spec, so a metadata-only edit -- a
// finalizer, a label -- cannot smuggle a forged value in either.
//
// The two keys are metadata rather than spec or status. Status is a
// subresource on both kinds, so a write through the main resource can never
// carry it: the API server resets status to its previous value on every
// update and drops it entirely on create. Spec is the author's own to write,
// and stamping into it would mean the mutation and the author's intent share
// one field. Metadata is the one part of the object every write to the main
// resource can carry and that the webhook alone controls.
const (
	LastSpecWriterUsernameAnnotation = "operator.ptah.run/last-spec-writer-username"
	LastSpecWriterUIDAnnotation      = "operator.ptah.run/last-spec-writer-uid"
)

// stampSpecWriter records user as the identity responsible for object's
// current spec, replacing whatever the request carried for the two reserved
// annotations.
func stampSpecWriter(object metav1.Object, user authenticationv1.UserInfo) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[LastSpecWriterUsernameAnnotation] = strings.TrimSpace(user.Username)
	if uid := strings.TrimSpace(user.UID); uid != "" {
		annotations[LastSpecWriterUIDAnnotation] = uid
	} else {
		delete(annotations, LastSpecWriterUIDAnnotation)
	}
	object.SetAnnotations(annotations)
}

// carrySpecWriter copies the two reserved annotations from old onto object
// unchanged, discarding whatever the request's own payload carried for them.
// It is what a request that does not change spec goes through instead of
// stampSpecWriter, so a finalizer patch or a label edit can neither erase the
// recorded writer nor forge a new one.
func carrySpecWriter(object, old metav1.Object) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	previous := old.GetAnnotations()
	for _, key := range []string{LastSpecWriterUsernameAnnotation, LastSpecWriterUIDAnnotation} {
		if value, ok := previous[key]; ok {
			annotations[key] = value
		} else {
			delete(annotations, key)
		}
	}
	object.SetAnnotations(annotations)
}

// SchemaSpecWriterHandler stamps the identity that created a PtahSchema, or
// last changed its spec, so the approval webhook can refuse a self-approval
// where the installation turns that control on.
type SchemaSpecWriterHandler struct {
	Decoder cradmission.Decoder
}

// Handle implements controller-runtime admission.Handler.
func (h *SchemaSpecWriterHandler) Handle(_ context.Context, req cradmission.Request) cradmission.Response {
	if h.Decoder == nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("spec-writer webhook is not initialized"))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Denied("only create and update are supported")
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := h.Decoder.Decode(req, schema); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode schema: %w", err))
	}
	specChanged := req.Operation == admissionv1.Create
	if req.Operation == admissionv1.Update {
		old := &operatorv1alpha1.PtahSchema{}
		if err := h.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode previous schema: %w", err))
		}
		if specChanged = !reflect.DeepEqual(schema.Spec, old.Spec); !specChanged {
			carrySpecWriter(schema, old)
		}
	}
	if specChanged {
		stampSpecWriter(schema, req.UserInfo)
	}
	mutated, err := json.Marshal(schema)
	if err != nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("encode stamped schema: %w", err))
	}
	return cradmission.PatchResponseFromRaw(req.Object.Raw, mutated)
}

// MigrationSpecWriterHandler is SchemaSpecWriterHandler's counterpart for
// PtahMigration.
type MigrationSpecWriterHandler struct {
	Decoder cradmission.Decoder
}

// Handle implements controller-runtime admission.Handler.
func (h *MigrationSpecWriterHandler) Handle(_ context.Context, req cradmission.Request) cradmission.Response {
	if h.Decoder == nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("spec-writer webhook is not initialized"))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Denied("only create and update are supported")
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := h.Decoder.Decode(req, migration); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode migration: %w", err))
	}
	specChanged := req.Operation == admissionv1.Create
	if req.Operation == admissionv1.Update {
		old := &operatorv1alpha1.PtahMigration{}
		if err := h.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode previous migration: %w", err))
		}
		if specChanged = !reflect.DeepEqual(migration.Spec, old.Spec); !specChanged {
			carrySpecWriter(migration, old)
		}
	}
	if specChanged {
		stampSpecWriter(migration, req.UserInfo)
	}
	mutated, err := json.Marshal(migration)
	if err != nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("encode stamped migration: %w", err))
	}
	return cradmission.PatchResponseFromRaw(req.Object.Raw, mutated)
}

// refuseSelfApproval enforces the four-eyes control once the installation
// turns it on: an approver who is exactly the identity writerAnnotations
// recorded as the resource's last spec writer is refused, by kind and by a
// clear reason. A resource with the control on but no recorded writer -- only
// possible if the mutating webhook that stamps one was not yet installed when
// its spec was last written -- is refused rather than guessed at.
func refuseSelfApproval(
	kind string,
	writerAnnotations map[string]string,
	approver operatorv1alpha1.ApprovalIdentity,
) error {
	writerUsername := strings.TrimSpace(writerAnnotations[LastSpecWriterUsernameAnnotation])
	if writerUsername == "" {
		return fmt.Errorf("%s requires a distinct approver, and no spec writer is recorded for it", kind)
	}
	writerUID := writerAnnotations[LastSpecWriterUIDAnnotation]
	if writerUsername == approver.Username && writerUID == approver.UID {
		return fmt.Errorf("%s requires a distinct approver, and %s last changed its spec", kind, approver.Username)
	}
	return nil
}
