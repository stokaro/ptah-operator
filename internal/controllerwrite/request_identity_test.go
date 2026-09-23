package controllerwrite_test

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// An admission request names an object and carries one. The validator refuses
// unless they are the same object, because every check after this one reads
// the carried object while the API server persists the named one: a request
// that names one Job and carries another would be judged on evidence about
// something that is not being written.
//
// The requirement is one disjunction of four parts, so coverage marks it
// measured once any request is refused for any of them. All four could be
// removed with the package green, and so could the check that the Job being
// created is the one the claim reserved a name for.
func TestValidationHandlerRefusesACandidateThatIsNotTheNamedObject(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		change  func(*cradmission.Request, *batchv1.Job)
		message string
	}{
		{
			// Cleared on both sides, so the comparison between them cannot
			// decide it: what refuses is the requirement that the request name
			// a namespace at all. A request that names none names no object.
			name: "the request names no namespace",
			change: func(request *cradmission.Request, job *batchv1.Job) {
				request.Namespace = ""
				job.Namespace = ""
			},
			message: "do not match the admission request",
		},
		{
			name: "the request names no name",
			change: func(request *cradmission.Request, job *batchv1.Job) {
				request.Name = ""
				job.Name = ""
			},
			message: "do not match the admission request",
		},
		{
			// Named in one namespace, carried in another. Only the named one
			// is written.
			name: "the candidate belongs to another namespace",
			change: func(request *cradmission.Request, _ *batchv1.Job) {
				request.Namespace = "somebody-elses-namespace"
			},
			message: "do not match the admission request",
		},
		{
			name: "the candidate carries another name",
			change: func(request *cradmission.Request, _ *batchv1.Job) {
				request.Name += "-2"
			},
			message: "do not match the admission request",
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

			carried := candidate.DeepCopy()
			changed := requestFor(t, admissionv1.Create, carried)
			row.change(&changed, carried)
			// The carried object is re-marshalled after the row edits it, so
			// a row can move both sides of the comparison at once.
			changed.Object = rawObject(t, carried)

			response := handler.Handle(context.Background(), changed)
			if response.Allowed {
				t.Fatal("a request naming one object and carrying another was admitted")
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, row.message) {
				t.Fatalf("refusal = %#v, want one naming %q", response.Result, row.message)
			}
		})
	}
}

// The claim reserves a name before anything is created under it, so the Job
// the operator creates has to be the one it reserved. A create under any other
// name is a dispatch the claim does not account for, and the claim is what a
// later pass reads to decide whether a run may have reached the database.
func TestValidationHandlerRefusesAJobTheClaimDidNotReserve(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	candidate := withGeneratedJobIdentity(expected)
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

	if response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, candidate)); !response.Allowed {
		t.Fatalf("the exact request was denied, so refusing another name proves nothing: %#v",
			response.Result)
	}

	// The claim still names the Job it reserved; this create names another.
	other := candidate.DeepCopy()
	other.Name += "-beside-it"
	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, other))
	if response.Allowed {
		t.Fatal("a Job created beside the one the claim reserved was admitted")
	}
	if response.Result == nil ||
		!strings.Contains(response.Result.Message, "not-yet-created active operation") {
		t.Fatalf("refusal = %#v, want one naming the claim", response.Result)
	}
}
