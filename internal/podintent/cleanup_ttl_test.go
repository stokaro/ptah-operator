package podintent_test

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A read-only Job is created with its cleanup TTL, so its Pod has to be
// admitted with one; an Apply Job is created with none, so a TTL on it before
// its Pod starts is one the manager did not write at creation.
func TestValidationHandlerAdmitsTheCreationTTLOnlyOnAReadOnlyJob(t *testing.T) {
	t.Parallel()

	short, long := workload.CleanupTTLSeconds, int32(86400)
	for _, test := range []struct {
		name    string
		fixture func(*testing.T) (*podintent.ValidationHandler, *corev1.Pod)
		ttl     *int32
		denial  string
	}{
		{
			name: "a history read with its cleanup TTL",
			fixture: func(t *testing.T) (*podintent.ValidationHandler, *corev1.Pod) {
				return validationHandlerFixtureForMigration(t, "history-job")
			},
			ttl: &short,
		},
		{
			name: "a history read with no TTL, as an earlier release created it",
			fixture: func(t *testing.T) (*podintent.ValidationHandler, *corev1.Pod) {
				return validationHandlerFixtureForMigration(t, "history-job")
			},
		},
		{
			name: "a history read with the long TTL",
			fixture: func(t *testing.T) (*podintent.ValidationHandler, *corev1.Pod) {
				return validationHandlerFixtureForMigration(t, "history-job")
			},
			ttl:    &long,
			denial: "a read-only Job's cleanup TTL is 300 seconds",
		},
		{
			name:    "an Apply with the cleanup TTL",
			fixture: validationHandlerFixture,
			ttl:     &short,
			denial:  "Job cleanup policy must not change before Pod admission",
		},
		{
			name:    "an Apply with no TTL",
			fixture: validationHandlerFixture,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, pod := test.fixture(t)
			api := handler.Reader.(client.Client)
			job := &batchv1.Job{}
			if err := api.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, job); err != nil {
				t.Fatal(err)
			}
			job.Spec.TTLSecondsAfterFinished = test.ttl
			if err := api.Update(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			response := handler.Handle(context.Background(), podRequest(t, pod))
			if test.denial == "" {
				if !response.Allowed {
					t.Fatalf("Handle() denied the Pod: %#v", response.Result)
				}
				return
			}
			if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, test.denial) {
				t.Fatalf("Handle() = %t %#v, want a denial naming %q", response.Allowed, response.Result, test.denial)
			}
		})
	}
}

// The envelope reads the operation from the Job's Pod template, which a Job
// cannot change after it is created. Every operation of both families is
// walked, along with the labels a template could carry that do not name a
// read-only operation of the Job's own family.
func TestActiveJobTakesItsCreationTTLFromTheTemplatesOperation(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixtureForMigration(t, "history-job")
	base := &batchv1.Job{}
	if err := handler.Reader.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, base); err != nil {
		t.Fatal(err)
	}
	if err := podintent.ValidateActiveJob(base); err != nil {
		t.Fatalf("the fixture's Job is refused before any TTL: %v", err)
	}
	schema, migration := workload.ComponentSchemaOperation, workload.ComponentMigrationOperation
	for _, test := range []struct {
		component, operation string
		readOnly             bool
	}{
		{schema, "resolve", true},
		{schema, "verify", true},
		{schema, "observe", true},
		{schema, "plan", true},
		{schema, "apply", false},
		{migration, "resolve", true},
		{migration, "verify", true},
		{migration, "history", true},
		{migration, "apply", false},
		// Each of these names a read of the other family, or no operation
		// either family knows.
		{schema, "history", false},
		{migration, "observe", false},
		{migration, "plan", false},
		{schema, "Observe", false},
		{schema, "", false},
		{schema, "rollback", false},
		{"", "observe", false},
		{"operator", "observe", false},
	} {
		t.Run(test.component+"/"+test.operation, func(t *testing.T) {
			t.Parallel()
			job := base.DeepCopy()
			job.Spec.Template.Labels[workload.LabelComponent] = test.component
			job.Spec.Template.Labels[workload.LabelOperation] = test.operation
			if err := podintent.ValidateActiveJob(job); err != nil {
				t.Fatalf("the relabeled Job is refused before any TTL: %v", err)
			}
			ttl := workload.CleanupTTLSeconds
			job.Spec.TTLSecondsAfterFinished = &ttl
			err := podintent.ValidateActiveJob(job)
			if test.readOnly && err != nil {
				t.Fatalf("a read-only Job with its cleanup TTL is refused: %v", err)
			}
			if !test.readOnly && (err == nil || !strings.Contains(err.Error(), "Job cleanup policy must not change")) {
				t.Fatalf("ValidateActiveJob() = %v, want the cleanup policy refusal", err)
			}
			if test.readOnly {
				over := ttl + 1
				job.Spec.TTLSecondsAfterFinished = &over
				if err := podintent.ValidateActiveJob(job); err == nil || !strings.Contains(err.Error(), "cleanup TTL is 300 seconds") {
					t.Fatalf("ValidateActiveJob() with TTL %d = %v, want a refusal naming 300", over, err)
				}
			}
		})
	}
}
