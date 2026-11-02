package apirefusal_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/internal/apirefusal"
)

func statusError(code int32, reason metav1.StatusReason, message string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: code, Reason: reason, Message: message,
	}}
}

// The answers the API server gives a write, each read for the two questions
// the plugin asks of it. The refusals that are not RBAC's are the mistakes a
// looser reading makes: admission answers 403 too, and a webhook that passes
// on a refusal of its own read quotes an authorizer.
func TestTheAnswersToAWrite(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		answer   error
		definite bool
		byRBAC   bool
	}{
		{"RBAC refuses a create", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemaapprovals.operator.ptah.run is forbidden: User "bob" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "application"`),
			true, true},
		{"RBAC refuses a patch of a named object", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob" cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "application"`),
			true, true},
		// The API server writes the username with %q.
		{"RBAC refuses a user with a quote in the name", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob \"b\" o'brien" cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "application"`),
			true, true},
		// A wrapped answer is still the answer.
		{"RBAC's refusal wrapped by the caller", fmt.Errorf("change ptahschema/storefront: %w", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob" cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "application"`)),
			true, true},
		{"a webhook refuses it", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`admission webhook "vptahschemaapproval.operator.ptah.run" denied the request: an approver may not approve a plan for a change they authored`),
			true, false},
		{"a webhook passes on the manager's own refusal", statusError(http.StatusForbidden, "",
			`admission webhook "mapproval.operator.ptah.run" denied the request: read referenced plan: ptahschemaplans.operator.ptah.run "p" is forbidden: User "system:serviceaccount:ptah-system:ptah-operator" cannot get resource "ptahschemaplans" in API group "operator.ptah.run" in the namespace "application"`),
			true, false},
		{"a ValidatingAdmissionPolicy refuses it", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemas.operator.ptah.run "storefront" is forbidden: ValidatingAdmissionPolicy 'ptah-apply-policy-guard' with binding 'ptah-apply-policy-guard' denied request: spec.policy.apply: Always applies plans without an approval`),
			true, false},
		{"the schema refuses a field", statusError(http.StatusUnprocessableEntity, metav1.StatusReasonInvalid,
			`PtahSchema.operator.ptah.run "storefront" is invalid: spec.suspend: Invalid value`), true, false},
		{"the object is gone", apierrors.NewNotFound(schemaResource(), "storefront"), true, false},
		{"the credentials are not accepted", apierrors.NewUnauthorized("Unauthorized"), true, false},
		{"the API server is too busy", apierrors.NewTooManyRequests("the server has received too many requests", 1), true, false},
		{"the server failed", apierrors.NewInternalError(errors.New("etcd")), false, false},
		{"the server timed out storing it", statusError(http.StatusGatewayTimeout, metav1.StatusReasonTimeout, "the request timed out"), false, false},
		{"no answer in time", context.DeadlineExceeded, false, false},
		{"the connection dropped", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := apirefusal.Definite(test.answer); got != test.definite {
				t.Errorf("Definite(%v) = %t, want %t", test.answer, got, test.definite)
			}
			if got := apirefusal.ByRBAC(test.answer); got != test.byRBAC {
				t.Errorf("ByRBAC(%v) = %t, want %t", test.answer, got, test.byRBAC)
			}
		})
	}
}

func schemaResource() schema.GroupResource {
	return schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemas"}
}
