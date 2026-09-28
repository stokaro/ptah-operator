package controllerwrite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The cleanup path judges a terminal Job against its claim without rebuilding
// it, so what spec.execution.podMetadata declared when the Job was built is
// not re-derived from the spec, which may have changed since. The claim's
// template digest is what pins it: a Job carrying exactly the declared keys
// is the claim's Job, and one carrying a reserved key beyond the claim, or a
// key the snapshot never saw, is not.

func declaredPodMetadata() *operatorv1alpha1.PodMetadataSpec {
	return &operatorv1alpha1.PodMetadataSpec{
		Labels:      map[string]operatorv1alpha1.PodLabelValue{"acme.example/team": "platform"},
		Annotations: map[string]operatorv1alpha1.PodAnnotationValue{"sidecar.istio.io/inject": "false"},
	}
}

func TestValidationHandlerAllowsCleanupOfAJobCarryingDeclaredPodMetadata(t *testing.T) {
	t.Parallel()

	for _, operationType := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationApply,
	} {
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()

			schema, _, oldJob, job := currentCleanupFixtureWith(t, operationType, func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Execution.PodMetadata = declaredPodMetadata()
			})
			if oldJob.Labels["acme.example/team"] != "platform" || oldJob.Spec.Template.Annotations["sidecar.istio.io/inject"] != "false" {
				t.Fatalf("the fixture's Job carries no declared metadata, so nothing below proves anything: %v %v",
					oldJob.Labels, oldJob.Spec.Template.Annotations)
			}
			// The spec moved on after dispatch: the declaration is judged by
			// the claim, not by what the resource says now.
			schema.Generation++
			schema.Spec.Execution.PodMetadata = nil
			handler := handlerFixture(t, staticJobBuilder{err: errors.New("mutable inputs are not rebuilt on cleanup")}, schema)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)

			response := handler.Handle(context.Background(), request)
			if !response.Allowed {
				t.Fatalf("Handle() denied cleanup of the %s Job carrying its declared metadata: %#v", operationType, response.Result)
			}
		})
	}
}

func TestValidationHandlerRefusesCleanupOfAJobWithMetadataBeyondItsClaim(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name   string
		mutate func(job *batchv1.Job)
		want   string
	}{
		{
			name: "a reserved label beyond the claim",
			mutate: func(job *batchv1.Job) {
				job.Labels["app.kubernetes.io/name"] = "ptah-operator"
				job.Spec.Template.Labels["app.kubernetes.io/name"] = "ptah-operator"
			},
			want: "reserved key",
		},
		{
			name: "a reserved annotation beyond the claim",
			mutate: func(job *batchv1.Job) {
				job.Annotations["kubernetes.io/limit-ranger"] = "LimitRanger plugin set: cpu request for container ptah"
				job.Spec.Template.Annotations["kubernetes.io/limit-ranger"] = "LimitRanger plugin set: cpu request for container ptah"
			},
			// A reserved annotation beyond the envelope is a shape no builder
			// writes, and the envelope check says so before the claim is read.
			want: "does not carry the operation envelope",
		},
		{
			name: "a declarable label the snapshot never saw",
			mutate: func(job *batchv1.Job) {
				job.Labels["acme.example/added"] = "later"
				job.Spec.Template.Labels["acme.example/added"] = "later"
			},
			want: "does not match the persisted admission snapshot",
		},
		{
			name: "a declared label on the Job and not on its template",
			mutate: func(job *batchv1.Job) {
				delete(job.Spec.Template.Labels, "acme.example/team")
			},
			want: "template labels differ",
		},
		{
			name: "a declared annotation on the template and not on the Job",
			mutate: func(job *batchv1.Job) {
				delete(job.Annotations, "sidecar.istio.io/inject")
			},
			want: "template annotations differ",
		},
		{
			name: "more declared labels than the API allows",
			mutate: func(job *batchv1.Job) {
				for i := 0; i <= operatorv1alpha1.MaxPodMetadataEntries; i++ {
					key := "declared.example/" + strings.Repeat("k", i+1)
					job.Labels[key] = "v"
					job.Spec.Template.Labels[key] = "v"
				}
			},
			want: "declared keys exceed",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, _, oldJob, job := currentCleanupFixtureWith(t, operatorv1alpha1.OperationResolve, func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Execution.PodMetadata = declaredPodMetadata()
			})
			for _, candidate := range []*batchv1.Job{oldJob, job} {
				row.mutate(candidate)
			}
			handler := handlerFixture(t, staticJobBuilder{err: errors.New("mutable inputs are not rebuilt on cleanup")}, schema)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)

			response := handler.Handle(context.Background(), request)
			if response.Allowed {
				t.Fatalf("Handle() allowed cleanup of a Job with %s", row.name)
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, row.want) {
				t.Fatalf("refusal = %#v, want one naming %q", response.Result, row.want)
			}
		})
	}
}

func TestValidationHandlerAllowsMigrationCleanupOfAJobCarryingDeclaredPodMetadata(t *testing.T) {
	t.Parallel()

	migration, job := migrationJobFixtureWith(t, operatorv1alpha1.MigrationOperationHistory, func(migration *operatorv1alpha1.PtahMigration) {
		migration.Spec.Execution.PodMetadata = declaredPodMetadata()
	})
	if job.Labels["acme.example/team"] != "platform" {
		t.Fatalf("the fixture's Job carries no declared metadata: %v", job.Labels)
	}
	terminal := withGeneratedJobIdentity(job)
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	migration.Status.ActiveOperation.JobUID = terminal.UID
	migration.Spec.Execution.PodMetadata = nil
	handler := migrationHandlerFixture(t, migration, []client.Object{terminal}...)

	cleaned := terminal.DeepCopy()
	ttl := int32(300)
	cleaned.Spec.TTLSecondsAfterFinished = &ttl
	response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
	if !response.Allowed {
		t.Fatalf("the cleanup TTL on a migration Job carrying its declared metadata was denied: %s", responseMessage(response))
	}

	beyond := terminal.DeepCopy()
	beyond.Labels["app.kubernetes.io/name"] = "ptah-operator"
	beyond.Spec.Template.Labels["app.kubernetes.io/name"] = "ptah-operator"
	cleanedBeyond := beyond.DeepCopy()
	cleanedBeyond.Spec.TTLSecondsAfterFinished = &ttl
	response = handler.Handle(context.Background(), migrationUpdateRequest(t, beyond, cleanedBeyond))
	if response.Allowed || !strings.Contains(responseMessage(response), "reserved key") {
		t.Fatalf("a migration Job with a reserved label beyond its claim was judged %t: %s", response.Allowed, responseMessage(response))
	}
}
