package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestVerificationRecordsThePolicyThatSatisfiedIt covers the accepting side of
// artifact verification. The refusing side has a table of its own; the pass
// where the artifact satisfies the policy had no test, so every statement that
// records the verdict was uncovered.
//
// What it records is the reason it matters. A verified artifact is not a flag:
// the status keeps the policy object's UID and the digest of the policy the
// executor actually evaluated, which is what lets a later edit to that object
// retire the verification instead of leaving a stale "verified" standing
// against a policy nobody would recognize.
//
// The second row is the same requirement one step earlier. An executor's
// verdict is evidence about the policy it read, so a verdict that arrives after
// the object moved is evidence about something that is no longer in effect, and
// it does not verify anything.
func TestVerificationRecordsThePolicyThatSatisfiedIt(t *testing.T) {
	t.Parallel()

	policyBytes := []byte("require_digest_pin: true\nrequire_signature: true\n")
	policyDigest := fingerprint.DigestBytes(policyBytes)

	for _, row := range []struct {
		name         string
		published    []byte
		wantVerified bool
	}{
		{
			name:         "the policy the executor read is the one in effect",
			published:    policyBytes,
			wantVerified: true,
		},
		{
			name:         "the policy moved while the executor was reading it",
			published:    []byte("require_digest_pin: true\nrequire_signature: false\n"),
			wantVerified: false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			schema.Finalizers = []string{activeOperationFinalizer}
			schema.Status.Phase = operatorv1alpha1.PhaseVerifying
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				RequestedReference: schema.Spec.Desired.OCIRef,
				ResolvedReference:  "oci://registry.example/team/schema@" + testDigest,
				Digest:             testDigest,
				MediaType:          "application/vnd.oci.image.manifest.v1+json",
				Size:               321,
			}
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationVerify, ID: testDigest,
				JobName: "verify-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
				VerificationPolicyUID: testPolicyUID, VerificationPolicyDigest: policyDigest,
			}
			bindActiveInput(t, schema)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			frame, err := runner.MarshalFrame(runner.Result{
				ProtocolVersion:          runner.ProtocolVersion,
				Operation:                runner.OperationVerify,
				OperationID:              testDigest,
				ChildExitCode:            0,
				ResolvedDigest:           testDigest,
				ObservedArtifactType:     dataplane.SchemaArtifactType,
				VerificationPolicyDigest: policyDigest,
			})
			if err != nil {
				t.Fatal(err)
			}
			policyConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: schema.Namespace,
					Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
					UID:       testPolicyUID,
				},
				Immutable: ptr(true),
				Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: string(row.published)},
			}
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfigMap)

			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			actual := safetyGetSchema(t, api, schema)
			source := actual.Status.Source
			if source.Verified != row.wantVerified {
				t.Fatalf("verified = %t, want %t: %#v", source.Verified, row.wantVerified, actual.Status)
			}
			condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
			if !row.wantVerified {
				if condition != nil && condition.Status == metav1.ConditionTrue {
					t.Fatalf("a verdict about a replaced policy was published as verified: %#v", condition)
				}
				return
			}
			// The verdict is kept against the policy that produced it, not
			// against the reference that names it.
			if source.VerificationPolicyUID != testPolicyUID || source.VerificationPolicyDigest != policyDigest {
				t.Fatalf("source policy identity = %q/%q, want %q/%q",
					source.VerificationPolicyUID, source.VerificationPolicyDigest, testPolicyUID, policyDigest)
			}
			if source.ArtifactType != dataplane.SchemaArtifactType || source.VerifiedAt == nil {
				t.Fatalf("verification recorded no artifact evidence: %#v", source)
			}
			if actual.Status.Phase != operatorv1alpha1.PhaseObserving {
				t.Fatalf("phase = %q, want %q", actual.Status.Phase, operatorv1alpha1.PhaseObserving)
			}
			if condition == nil || condition.Status != metav1.ConditionTrue ||
				condition.Reason != string(operatorv1alpha1.ReasonPolicySatisfied) {
				t.Fatalf("ArtifactVerified condition = %#v, want true with PolicySatisfied", condition)
			}
		})
	}
}
