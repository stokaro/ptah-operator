package controllerwrite_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
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

// The workload builder writes the operation envelope on every Job it makes:
// eight annotations on a read-only operation's Job, ten on an Apply's. A Job
// that is the claim's own instance by name and UID but carries anything else
// was not made by the builder, and a Job rebuilt from the claim is compared
// with it annotation for annotation, so a rebuild could only refuse it too.
//
// The refusal therefore comes first. Rebuilding an Apply's Job starts with a
// direct read of its plan and of every chunk, and that read would be spent on
// an answer already known; the plan-read count is what tells this refusal apart
// from the rebuild it replaced, since both end in a denial.
func TestValidationHandlerRefusesCleanupOfJobOutsideTheOperationEnvelope(t *testing.T) {
	t.Parallel()

	const outsideEnvelope = "does not carry the operation envelope"
	for _, row := range []struct {
		name          string
		operationType operatorv1alpha1.OperationType
		change        func(annotations map[string]string)
		want          string
	}{
		{
			name:          "a read-only Job without its admission snapshot digest",
			operationType: operatorv1alpha1.OperationResolve,
			change: func(annotations map[string]string) {
				delete(annotations, workload.AnnotationAdmissionSnapshotDigest)
			},
			want: outsideEnvelope,
		},
		{
			name:          "an Apply Job without its plan fingerprint",
			operationType: operatorv1alpha1.OperationApply,
			change: func(annotations map[string]string) {
				delete(annotations, workload.AnnotationPlanFingerprint)
			},
			want: outsideEnvelope,
		},
		{
			name:          "an Apply Job without its controller provenance",
			operationType: operatorv1alpha1.OperationApply,
			change: func(annotations map[string]string) {
				delete(annotations, workload.AnnotationControllerImage)
				delete(annotations, workload.AnnotationControllerRevision)
				delete(annotations, workload.AnnotationControllerStateVersion)
			},
			want: outsideEnvelope,
		},
		{
			// A reserved key beyond the envelope is a shape no builder writes.
			name:          "a read-only Job with an operator annotation the builder never writes",
			operationType: operatorv1alpha1.OperationResolve,
			change: func(annotations map[string]string) {
				annotations["operator.ptah.run/added-after-dispatch"] = "true"
			},
			want: outsideEnvelope,
		},
		{
			// A key spec.execution.podMetadata could have declared is inside
			// the envelope's shape, so what refuses it is the claim: the
			// snapshot pinned a template without it.
			name:          "a read-only Job with an annotation the resource never declared",
			operationType: operatorv1alpha1.OperationResolve,
			change: func(annotations map[string]string) {
				annotations["example.test/added-after-dispatch"] = "true"
			},
			want: "does not match the persisted admission snapshot",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, expected, oldJob, job := currentCleanupFixture(t, row.operationType)
			reader := &planReadCounter{Reader: fake.NewClientBuilder().
				WithScheme(controllerWriteScheme(t)).WithObjects(schema).Build()}
			handler := handlerWithReader(staticJobBuilder{job: expected}, reader)

			exact := requestFor(t, admissionv1.Update, job)
			exact.OldObject = rawObject(t, oldJob)
			if response := handler.Handle(context.Background(), exact); !response.Allowed {
				t.Fatalf("the exact cleanup update was denied, so refusing a changed one proves "+
					"nothing: %#v", response.Result)
			}

			for _, candidate := range []*batchv1.Job{oldJob, job} {
				row.change(candidate.Annotations)
				row.change(candidate.Spec.Template.Annotations)
			}
			changed := requestFor(t, admissionv1.Update, job)
			changed.OldObject = rawObject(t, oldJob)

			response := handler.Handle(context.Background(), changed)
			if response.Allowed {
				t.Fatal("a cleanup update for a Job outside the operation envelope was admitted")
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, row.want) {
				t.Fatalf("refusal = %#v, want one naming %q", response.Result, row.want)
			}
			if reads := reader.planReads.Load(); reads != 0 {
				t.Fatalf("the refusal read the plan %d times to reach an answer the annotations already gave", reads)
			}
		})
	}
}

// planReadCounter counts the direct reads of a PtahSchemaPlan.
type planReadCounter struct {
	client.Reader

	planReads atomic.Int32
}

func (r *planReadCounter) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, isPlan := object.(*operatorv1alpha1.PtahSchemaPlan); isPlan {
		r.planReads.Add(1)
	}
	return r.Reader.Get(ctx, key, object, options...)
}
