package e2e

import (
	"errors"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	validationfield "k8s.io/apimachinery/pkg/util/validation/field"
)

func TestAdmissionAnsweredOnlyWhenTheWebhookWasReached(t *testing.T) {
	t.Parallel()
	resource := schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemaapprovals"}
	denied := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden,
		Message: `admission webhook "mapproval.operator.ptah.run" denied the request: approval names no schema`}}
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: resource.Group, Kind: "PtahSchemaApproval"}, "e2e-admission-probe",
		validationfield.ErrorList{validationfield.Required(validationfield.NewPath("spec", "schemaRef"), "")})
	for name, err := range map[string]error{"admitted": nil, "denied by the webhook": denied, "refused by schema validation after mutation": invalid} {
		if !admissionAnswered(err) {
			t.Errorf("%s: not counted as an answer", name)
		}
	}
	// The run that needed this gate: the ClusterIP had no endpoint yet.
	refused := apierrors.NewInternalError(errors.New(`failed calling webhook "vapproval.operator.ptah.run": failed to call webhook: Post "https://webhook.svc:443/validate": dial tcp 10.96.207.67:443: connect: connection refused`))
	for name, err := range map[string]error{
		"connection refused": refused,
		"webhook timeout":    apierrors.NewTimeoutError("context deadline exceeded", 1),
		"unavailable":        apierrors.NewServiceUnavailable("no endpoints"),
		"a client error that names a webhook call": &apierrors.StatusError{ErrStatus: metav1.Status{Code: http.StatusBadRequest, Message: `failed calling webhook "mapproval.operator.ptah.run"`}},
		"not an API status":                        errors.New("dial tcp: i/o timeout"),
	} {
		if admissionAnswered(err) {
			t.Errorf("%s: counted as an answer", name)
		}
	}
}
