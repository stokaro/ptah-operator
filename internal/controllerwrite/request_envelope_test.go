package controllerwrite_test

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The validator's contract is about one exact object written by one exact
// identity, and the admission request says which object that is. The API
// server may present a request converted from an equivalent resource or kind,
// and it reports the subresource a write reached under two names.
//
// So the request envelope is checked before anything inside it. Each of these
// refusals could be removed with the package staying green -- they sit in the
// same branch as a refusal a test does reach, which is all coverage can see.
//
// What they hold: a write that arrives as something else is not the write the
// contract describes, and nothing further inside the request can establish
// that it is.
func TestValidationHandlerRefusesARequestThatIsNotTheOneItContractsFor(t *testing.T) {
	t.Parallel()

	jobKind := metav1.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	jobResource := metav1.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

	for _, row := range []struct {
		name     string
		envelope func(*cradmission.Request)
		message  string
	}{
		{
			// A write that reached a subresource is outside the contract
			// whichever way round the API server reports it.
			name: "the write reached a subresource",
			envelope: func(request *cradmission.Request) {
				request.SubResource = "status"
			},
			message: "subresources are not permitted",
		},
		{
			name: "the write reached a subresource under its original name",
			envelope: func(request *cradmission.Request) {
				request.RequestSubResource = "status"
			},
			message: "subresources are not permitted",
		},
		{
			// The kind and the resource disagree, so one of them is not what
			// the request is really about.
			name: "the kind does not match the resource",
			envelope: func(request *cradmission.Request) {
				request.Kind = metav1.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}
			},
			message: "kind does not match its resource",
		},
		{
			// Presented as a Job, sent as something the API server considers
			// equivalent. The contract is about the resource as written.
			name: "the resource was converted from an equivalent one",
			envelope: func(request *cradmission.Request) {
				original := metav1.GroupVersionResource{Group: "batch", Version: "v1beta1", Resource: "jobs"}
				request.RequestResource = &original
			},
			message: "converted or equivalent resource",
		},
		{
			name: "the kind was converted from an equivalent one",
			envelope: func(request *cradmission.Request) {
				original := metav1.GroupVersionKind{Group: "batch", Version: "v1beta1", Kind: "Job"}
				request.RequestKind = &original
			},
			message: "converted or equivalent kind",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationResolve)
			expected := expectedJob(schema, schema.Status.ActiveOperation)
			candidate := withGeneratedJobIdentity(expected)
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

			exact := requestFor(t, admissionv1.Create, candidate)
			if response := handler.Handle(context.Background(), exact); !response.Allowed {
				t.Fatalf("the exact request was denied, so refusing a changed one proves nothing: %#v",
					response.Result)
			}

			changed := requestFor(t, admissionv1.Create, candidate)
			row.envelope(&changed)
			if changed.Kind == jobKind && changed.Resource == jobResource &&
				changed.SubResource == "" && changed.RequestSubResource == "" &&
				changed.RequestResource == nil && changed.RequestKind == nil {
				t.Fatal("the row changed nothing about the request envelope")
			}

			response := handler.Handle(context.Background(), changed)
			if response.Allowed {
				t.Fatal("a request that is not the one the contract describes was admitted")
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, row.message) {
				t.Fatalf("refusal = %#v, want one naming %q", response.Result, row.message)
			}
		})
	}
}
