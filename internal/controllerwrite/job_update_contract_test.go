package controllerwrite_test

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The operator is permitted exactly one update to a Job it created: stamping
// the cleanup TTL onto a Job that has none. Everything else about the object
// has to be the same object, and the transition has to be that transition.
//
// Both conditions are written as disjunctions, so one way of failing exercises
// the whole branch and coverage marks it measured. Six of the parts could be
// removed with the package green: an update whose old object names another
// namespace or another name, whose old object carries no identity at all,
// whose new object carries a different one, whose old object already had a
// TTL, or whose new object has none.
//
// The last two are what keeps the permission to a single write. A Job that
// already carries a TTL has had its one update; letting the operator restamp
// it turns a one-shot permission into an open one over the field that decides
// when the evidence of a run is collected.
func TestValidationHandlerAllowsOnlyTheExactCleanupUpdate(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		change  func(oldJob, job *batchv1.Job)
		message string
	}{
		{
			name: "the old object belongs to another namespace",
			change: func(oldJob, _ *batchv1.Job) {
				oldJob.Namespace = "somebody-elses-namespace"
			},
			message: "does not preserve the request namespace",
		},
		{
			name: "the old object carries another name",
			change: func(oldJob, _ *batchv1.Job) {
				oldJob.Name += "-2"
			},
			message: "does not preserve the request namespace",
		},
		{
			// Cleared on both sides, so the comparison between them cannot
			// decide it and the requirement that the old object have an
			// identity at all is what refuses.
			name: "neither object carries an identity",
			change: func(oldJob, job *batchv1.Job) {
				oldJob.UID = ""
				job.UID = ""
			},
			message: "does not preserve the request namespace",
		},
		{
			// A different UID under the same name is a different Job, and the
			// permission is to stamp the one the operator created.
			name: "the update names another object",
			change: func(_, job *batchv1.Job) {
				job.UID = "a-job-created-after-this-one"
			},
			message: "does not preserve the request namespace",
		},
		{
			// It has had its one update already.
			name: "the old object already carries a cleanup TTL",
			change: func(oldJob, _ *batchv1.Job) {
				existing := int32(300)
				oldJob.Spec.TTLSecondsAfterFinished = &existing
			},
			message: "not the exact nil-to-300 cleanup TTL transition",
		},
		{
			name: "the update removes the TTL instead of stamping it",
			change: func(_, job *batchv1.Job) {
				job.Spec.TTLSecondsAfterFinished = nil
			},
			message: "not the exact nil-to-300 cleanup TTL transition",
		},
		{
			name: "the update stamps some other TTL",
			change: func(_, job *batchv1.Job) {
				other := int32(301)
				job.Spec.TTLSecondsAfterFinished = &other
			},
			message: "not the exact nil-to-300 cleanup TTL transition",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
			oldJob.Status.Conditions = nil
			job.Status.Conditions = nil
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

			exact := requestFor(t, admissionv1.Update, job)
			exact.OldObject = rawObject(t, oldJob)
			if response := handler.Handle(context.Background(), exact); !response.Allowed {
				t.Fatalf("the exact cleanup update was denied, so refusing a changed one proves "+
					"nothing: %#v", response.Result)
			}

			row.change(oldJob, job)
			changed := requestFor(t, admissionv1.Update, job)
			changed.OldObject = rawObject(t, oldJob)

			response := handler.Handle(context.Background(), changed)
			if response.Allowed {
				t.Fatal("a Job update outside the one permitted write was admitted")
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, row.message) {
				t.Fatalf("refusal = %#v, want one naming %q", response.Result, row.message)
			}
		})
	}
}
