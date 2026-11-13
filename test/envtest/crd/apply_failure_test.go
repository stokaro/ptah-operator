package crd_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// status.pendingObservation.applyFailure is where an Apply stopped, as the run
// reported it, and the schema keeps a position or a code no run produced from
// being published.
func TestPtahSchemaStatusRefusesAMalformedApplyFailure(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "apply-failure")
	schema := schemaBase(namespace)()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store PtahSchema %s: %v", schema.GetName(), err)
	}
	pending := func(failure map[string]any) map[string]any {
		typed := operatorv1alpha1.PendingObservationStatus{
			Outcome: operatorv1alpha1.PendingObservationOutcomeUnknown, ApplyOperationID: settleApplyID,
			ApplyJobName: "ptah-apply-aaa", ApplyJobUID: "apply-job-uid", ApplyGeneration: 1,
			CoordinationDigest: coordinationDigest, LeaseDurationSeconds: 60,
			Target: operatorv1alpha1.DatabaseTargetBinding{Engine: "PostgreSQL", URLFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "application-database"}, Key: "url"}},
			Source: operatorv1alpha1.SourceAccessBinding{
				ResolvedReference: "oci://ghcr.io/example/application-schema@" + artifactDigest, Digest: artifactDigest},
			Plan: operatorv1alpha1.CurrentPlanStatus{
				Name: "ptah-plan-aaa", UID: planUID, Fingerprint: planFingerprint, ContentDigest: contentDigest,
				ArtifactDigest: artifactDigest, CoordinationDigest: coordinationDigest, TargetIdentityDigest: targetIdentityDigest,
				ActualStateFingerprint: actualStateFingerprint, DesiredStateFingerprint: desiredStateFingerprint,
				PolicyFingerprint: policyFingerprint, ExecutionBindingID: executionBindingID, ControllerImage: controllerImage,
				ControllerRevision: controllerRevision, ControllerStateVersion: 2, PtahVersion: ptahVersion,
				ExecutorImage: executorImage, RunnerImage: runnerImage, RunnerProtocolVersion: 5, StatementCount: 1,
				CreatedAt: metav1.NewTime(time.Date(2026, 10, 24, 7, 33, 10, 0, time.UTC)),
			},
		}
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&typed)
		if err != nil {
			t.Fatal(err)
		}
		object["applyFailure"] = failure
		return object
	}
	refused := func(parts ...map[string]any) map[string]any {
		return merged(append([]map[string]any{{"statement": int64(1), "statements": int64(1)}}, parts...)...)
	}
	for _, test := range []struct {
		name    string
		failure map[string]any
		want    cause
	}{
		{"a SQLSTATE that is not one", refused(map[string]any{"sqlState": "23p05"}),
			cause{"status.pendingObservation.applyFailure.sqlState", "should match"}},
		{"a statement before the first", refused(map[string]any{"statement": int64(0)}),
			cause{"status.pendingObservation.applyFailure.statement", "greater than or equal to 1"}},
		{"a run of no statements", refused(map[string]any{"statements": int64(0)}),
			cause{"status.pendingObservation.applyFailure.statements", "greater than or equal to 1"}},
		{"an error number two bytes cannot hold", refused(map[string]any{"errorNumber": int64(65536)}),
			cause{"status.pendingObservation.applyFailure.errorNumber", "less than or equal to 65535"}},
		{"no statement", map[string]any{"statements": int64(1), "sqlState": "23505"},
			cause{"status.pendingObservation.applyFailure.statement", "Required value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, schema)
			set(t, observed, pending(test.failure), "status", "pendingObservation")
			err := api.Status().Update(context.Background(), observed, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Errorf("status of PtahSchema %s: %v", schema.GetName(), mismatch)
			}
		})
	}

	// A field the schema does not declare is pruned without a complaint, so
	// what came back is what says the failure reached the CRD.
	for name, failure := range map[string]map[string]any{
		"a PostgreSQL refusal is published": refused(map[string]any{"sqlState": "23505"}),
		"a MySQL refusal is published":      refused(map[string]any{"statement": int64(2), "statements": int64(3), "sqlState": "23000", "errorNumber": int64(1062)}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, schema)
			set(t, observed, pending(failure), "status", "pendingObservation")
			if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
				t.Fatalf("the API server refused a well-formed apply failure on PtahSchema %s: %v", schema.GetName(), err)
			}
			kept, found, err := unstructured.NestedMap(observed.Object, "status", "pendingObservation", "applyFailure")
			if err != nil || !found {
				t.Fatalf("the apply failure did not come back: %v", err)
			}
			for key, value := range failure {
				if kept[key] != value {
					t.Fatalf("applyFailure came back as %v, want %v", kept, failure)
				}
			}
		})
	}
}
