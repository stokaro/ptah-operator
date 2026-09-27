package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
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

// specWriterValues says what the two reserved annotations must hold after a
// request: stamped from the requester when the request creates the resource or
// changes its spec, and carried unchanged from the stored object otherwise, so
// a finalizer patch or a label edit can neither erase the recorded writer nor
// forge a new one. A key absent from the result must be absent from the object.
func specWriterValues(req cradmission.Request) (map[string]string, error) {
	values := map[string]string{}
	if req.Operation == admissionv1.Update {
		var current, stored specWriterView
		if err := json.Unmarshal(req.Object.Raw, &current); err != nil {
			return nil, fmt.Errorf("decode the request object: %w", err)
		}
		if err := json.Unmarshal(req.OldObject.Raw, &stored); err != nil {
			return nil, fmt.Errorf("decode the stored object: %w", err)
		}
		if reflect.DeepEqual(current.Spec, stored.Spec) {
			for _, key := range specWriterAnnotations {
				if value, ok := stored.Metadata.Annotations[key]; ok {
					values[key] = value
				}
			}
			return values, nil
		}
	}
	values[LastSpecWriterUsernameAnnotation] = strings.TrimSpace(req.UserInfo.Username)
	if uid := strings.TrimSpace(req.UserInfo.UID); uid != "" {
		values[LastSpecWriterUIDAnnotation] = uid
	}
	return values, nil
}

// specWriterAnnotations is the order the patch names the reserved keys in.
var specWriterAnnotations = []string{LastSpecWriterUsernameAnnotation, LastSpecWriterUIDAnnotation}

// specWriterView is the part of a PtahSchema or a PtahMigration the webhook
// reads. The spec is compared as the JSON the API server sent rather than
// through the Go types, so a field this binary does not know still counts as a
// change to it.
type specWriterView struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec any `json:"spec"`
}

// specWriterResponse admits the request with a patch that touches the two
// reserved annotations and nothing else.
//
// The patch is built from the keys rather than from the difference between
// the request and a re-encoded typed object. A round trip through the Go types
// rewrites whatever it does not reproduce byte for byte: an interval of "10m"
// comes back as "10m0s", and a field this binary does not know is dropped. The
// author's spec would then be edited by a webhook whose only job is to record
// who wrote it.
func specWriterResponse(req cradmission.Request) cradmission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Denied("only create and update are supported")
	}
	want, err := specWriterValues(req)
	if err != nil {
		return cradmission.Errored(http.StatusBadRequest, err)
	}
	var current specWriterView
	if err := json.Unmarshal(req.Object.Raw, &current); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode the request object: %w", err))
	}
	have := current.Metadata.Annotations
	if have == nil {
		if len(want) == 0 {
			return cradmission.Allowed("")
		}
		return cradmission.Patched("", jsonpatch.NewOperation("add", "/metadata/annotations", want))
	}
	var patches []jsonpatch.JsonPatchOperation
	for _, key := range specWriterAnnotations {
		path := "/metadata/annotations/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
		wanted, keep := want[key]
		present, found := have[key]
		switch {
		case keep && (!found || present != wanted):
			patches = append(patches, jsonpatch.NewOperation("add", path, wanted))
		case !keep && found:
			patches = append(patches, jsonpatch.NewOperation("remove", path, nil))
		}
	}
	if len(patches) == 0 {
		return cradmission.Allowed("")
	}
	return cradmission.Patched("", patches...)
}

// SchemaSpecWriterHandler stamps the identity that created a PtahSchema, or
// last changed its spec, so the approval webhook can refuse a self-approval
// where spec.policy.requireDistinctApprover asks for one.
type SchemaSpecWriterHandler struct {
	Decoder cradmission.Decoder
}

// Handle implements controller-runtime admission.Handler.
func (h *SchemaSpecWriterHandler) Handle(_ context.Context, req cradmission.Request) cradmission.Response {
	if h.Decoder == nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("spec-writer webhook is not initialized"))
	}
	return specWriterResponse(req)
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
	return specWriterResponse(req)
}

// refuseSelfApproval enforces the four-eyes control once a resource opts into
// it: an approver who is exactly the identity writerAnnotations recorded as
// the resource's last spec writer is refused, by kind and by a clear reason.
// A resource that opted in but carries no recorded writer -- only possible if
// the mutating webhook that stamps one was not yet installed when its spec
// was last written -- is refused rather than guessed at.
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
