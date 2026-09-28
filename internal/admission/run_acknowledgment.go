package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// RunAcknowledgmentHandler stamps the authenticated identity onto a
// PtahMigrationRunAcknowledgment and refuses one that names a run the
// migration is not waiting on.
//
// The acknowledgment is how a person settles status.unresolvedRun. It
// replaces a write to the status subresource, which recorded no identity and
// needed exactly the authority the manager holds; the chart now refuses that
// write to anyone but the manager. What the acknowledgment carries instead is
// the decision -- which migration, which run -- and the stamp this handler
// writes from the request the API server authenticated.
//
// The binding is checked against the migration read directly from the API
// server in both admission passes, and the controller checks it again before
// it settles anything. An acknowledgment admitted for a run that a reading
// settles first is answered as stale rather than applied to a later run: a
// run's operation ID is never reused.
type RunAcknowledgmentHandler struct {
	Reader  client.Reader
	Decoder cradmission.Decoder
	Clock   Clock
	Mutate  bool
}

// Handle implements controller-runtime admission.Handler.
func (h *RunAcknowledgmentHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h.Reader == nil || h.Decoder == nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("run acknowledgment webhook is not initialized"))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Denied("only create and metadata-preserving updates are supported")
	}

	acknowledgment := &operatorv1alpha1.PtahMigrationRunAcknowledgment{}
	if err := h.Decoder.Decode(req, acknowledgment); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode run acknowledgment: %w", err))
	}
	if acknowledgment.Namespace != req.Namespace {
		return cradmission.Denied("acknowledgment namespace does not match the admission request")
	}

	if req.Operation == admissionv1.Update {
		previous := &operatorv1alpha1.PtahMigrationRunAcknowledgment{}
		if err := h.Decoder.DecodeRaw(req.OldObject, previous); err != nil {
			return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode previous run acknowledgment: %w", err))
		}
		if !reflect.DeepEqual(acknowledgment.Spec, previous.Spec) {
			return cradmission.Denied("acknowledgment spec is immutable; create a new acknowledgment")
		}
		return cradmission.Allowed("acknowledgment metadata update preserves the immutable decision")
	}

	if h.Mutate {
		h.stamp(acknowledgment, req.UserInfo, req.UID)
		if err := h.validateAcknowledgmentBinding(ctx, acknowledgment); err != nil {
			return denialFor(err)
		}
		mutated, err := json.Marshal(acknowledgment)
		if err != nil {
			return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("encode stamped acknowledgment: %w", err))
		}
		return cradmission.PatchResponseFromRaw(req.Object.Raw, mutated)
	}

	if err := acknowledgmentIdentityMatchesRequest(acknowledgment.Spec, req.UserInfo); err != nil {
		return cradmission.Denied(err.Error())
	}
	if err := h.validateAcknowledgmentBinding(ctx, acknowledgment); err != nil {
		return denialFor(err)
	}
	return cradmission.Allowed("acknowledgment names the run the migration records as unresolved")
}

func (h *RunAcknowledgmentHandler) stamp(
	acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment,
	user authenticationv1.UserInfo,
	requestUID types.UID,
) {
	clock := h.Clock
	if clock == nil {
		clock = realClock{}
	}
	acknowledgment.Spec.AcknowledgedBy = operatorv1alpha1.ApprovalIdentity{
		Username: strings.TrimSpace(user.Username),
		UID:      strings.TrimSpace(user.UID),
		Groups:   normalizedGroups(user.Groups),
	}
	acknowledgment.Spec.AcknowledgedAt = metav1.NewTime(clock.Now().UTC())
	acknowledgment.Spec.MutationRequestUID = string(requestUID)
}

func acknowledgmentIdentityMatchesRequest(
	spec operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec,
	user authenticationv1.UserInfo,
) error {
	if strings.TrimSpace(user.Username) == "" {
		return fmt.Errorf("authenticated acknowledgment username is empty")
	}
	if spec.AcknowledgedBy.Username != strings.TrimSpace(user.Username) ||
		spec.AcknowledgedBy.UID != strings.TrimSpace(user.UID) ||
		!slices.Equal(spec.AcknowledgedBy.Groups, normalizedGroups(user.Groups)) {
		return fmt.Errorf("reserved acknowledgedBy identity fields do not match the authenticated request")
	}
	if strings.TrimSpace(spec.MutationRequestUID) == "" {
		return fmt.Errorf("acknowledgment identity was not stamped by the mutating admission webhook")
	}
	if spec.AcknowledgedAt.IsZero() {
		return fmt.Errorf("acknowledgedAt was not stamped by the admission webhook")
	}
	return nil
}

// validateAcknowledgmentBinding refuses an acknowledgment of anything but the
// run the named migration records as unresolved right now.
func (h *RunAcknowledgmentHandler) validateAcknowledgmentBinding(
	ctx context.Context,
	acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment,
) error {
	reference := acknowledgment.Spec.MigrationRef
	if strings.TrimSpace(reference.Name) == "" || reference.UID == "" {
		return fmt.Errorf("acknowledgment must explicitly identify the migration name and UID")
	}
	if strings.TrimSpace(acknowledgment.Spec.OperationID) == "" {
		return fmt.Errorf("acknowledgment must name the operationID of the unresolved run")
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: acknowledgment.Namespace, Name: reference.Name}, migration); err != nil {
		return fmt.Errorf("read referenced migration: %w", err)
	}
	if migration.DeletionTimestamp != nil {
		return fmt.Errorf("referenced migration is being deleted")
	}
	if migration.UID != reference.UID {
		return fmt.Errorf("referenced migration UID does not match; the migration was replaced")
	}
	unresolved := migration.Status.UnresolvedRun
	if unresolved == nil {
		return fmt.Errorf("referenced migration records no unresolved run")
	}
	if unresolved.OperationID != acknowledgment.Spec.OperationID {
		return fmt.Errorf("referenced migration's unresolved run is operation %s, not the one this acknowledgment names",
			unresolved.OperationID)
	}
	return nil
}
