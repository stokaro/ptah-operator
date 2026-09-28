package controllerwrite_test

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A claim made before its binding rotated authorizes no Job under the new
// binding, and the Job-create webhook refuses to admit one. The real builder
// refuses to rebuild such a claim too, so these rows rebuild through a static
// builder that does not: what refuses them is the webhook holding the claim
// to the binding in force, which it used to leave to the builder.
func TestTheJobCreateWebhookHoldsTheClaimToTheBindingInForce(t *testing.T) {
	t.Parallel()

	const rotated = "v1-99999999999999999999999999999999"
	for _, row := range []struct {
		name  string
		stage func(t *testing.T) (client.Object, *batchv1.Job, *operatorv1alpha1.ExecutionBindingStatus)
	}{
		{
			name: "PtahSchema",
			stage: func(t *testing.T) (client.Object, *batchv1.Job, *operatorv1alpha1.ExecutionBindingStatus) {
				schema := schemaFixture(operatorv1alpha1.OperationResolve)
				return schema, expectedJob(schema, schema.Status.ActiveOperation), schema.Status.ExecutionBinding
			},
		},
		{
			name: "PtahMigration",
			stage: func(t *testing.T) (client.Object, *batchv1.Job, *operatorv1alpha1.ExecutionBindingStatus) {
				migration, job := migrationJobFixture(t, operatorv1alpha1.MigrationOperationHistory)
				return migration, job, migration.Status.ExecutionBinding
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			owner, job, binding := row.stage(t)
			candidate := withGeneratedJobIdentity(job)
			admit := func() (bool, string) {
				handler := handlerFixture(t, staticJobBuilder{job: job}, owner, migrationServiceAccount())
				response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, candidate))
				return response.Allowed, responseMessage(response)
			}
			if allowed, message := admit(); !allowed {
				t.Fatalf("the Job of a claim made under the binding in force was denied, so nothing below proves anything: %s", message)
			}

			binding.Epoch = rotated
			allowed, message := admit()
			if allowed || !strings.Contains(message, "no longer in force") {
				t.Fatalf("the Job of a claim made before the binding rotated = allowed %t (%s), want a refusal naming the epoch in force",
					allowed, message)
			}
		})
	}
}
