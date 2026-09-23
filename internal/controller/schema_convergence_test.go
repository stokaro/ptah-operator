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
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestConvergenceCreditsOnlyWhatItCanAttribute is where a post-Apply proof is
// paid: the read-only pass comes back with no changes, the schema goes InSync,
// and the Apply that owed the proof is finally credited in status.applied.
//
// No test reached it. Tests set PhaseInSync as a precondition; the controller
// never wrote it, because the one test that completes a proof deliberately
// moves the verification policy first, which skips the convergence branch
// entirely. Every statement below the drift verdict was uncovered.
//
// The distinction the branch encodes is the point. Convergence says what the
// database looks like now. It does not say who made it look that way, so an
// Apply whose outcome was never established converges without being credited
// with anything -- and the condition says so rather than leaving a reader to
// infer it from an absent field.
func TestConvergenceCreditsOnlyWhatItCanAttribute(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name          string
		outcome       operatorv1alpha1.PendingObservationOutcome
		wantAttribute bool
		wantReason    operatorv1alpha1.ConditionReason
	}{
		{
			// The run reported an exact completed mutation and the proof
			// agrees with it, so the plan it applied is what the database now
			// holds.
			name:          "a proven Apply is credited",
			outcome:       operatorv1alpha1.PendingObservationApplySucceeded,
			wantAttribute: true,
			wantReason:    operatorv1alpha1.ReasonScopedConverged,
		},
		{
			// The managed scope converged and nothing establishes that this
			// Apply is why. Crediting it here would turn an unresolved run
			// into a recorded success on the strength of a later reading.
			name:          "an Apply nobody could account for is not",
			outcome:       operatorv1alpha1.PendingObservationOutcomeUnknown,
			wantAttribute: false,
			wantReason:    operatorv1alpha1.ReasonConvergedAfterUnknownOutcome,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := safetyPostApplyObserveSchema(t)
			schema.Status.PendingObservation.Outcome = row.outcome
			schema.Status.PendingObservation.PlanRequired = true
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type:      operatorv1alpha1.OperationPlan,
				ID:        "post-apply-plan",
				JobName:   "post-apply-plan-job",
				JobUID:    "job-uid",
				StartedAt: metav1.Now(),
				Attempt:   1,
			}
			policyConfig := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: schema.Namespace,
					Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
					UID:       testPolicyUID,
				},
				Immutable: ptr(true),
				Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
			}
			policyDigest := fingerprint.DigestBytes([]byte("policy"))
			schema.Status.Source.VerificationPolicyDigest = policyDigest
			schema.Status.PendingObservation.Plan.VerificationPolicyDigest = policyDigest
			bindActiveInput(t, schema)
			wantFingerprint := schema.Status.PendingObservation.Plan.Fingerprint

			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			frame := safetyRunnerFrame(t, runner.Result{
				ProtocolVersion:      runner.ProtocolVersion,
				Operation:            runner.OperationPlan,
				OperationID:          schema.Status.ActiveOperation.ID,
				ChildExitCode:        0,
				CoordinationDigest:   schema.Status.PendingObservation.CoordinationDigest,
				TargetIdentityDigest: schema.Status.PendingObservation.Plan.TargetIdentityDigest,
				PlanOutcome:          runner.PlanOutcomeNoChanges,
			})
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfig)

			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			actual := safetyGetSchema(t, api, schema)
			if actual.Status.Phase != operatorv1alpha1.PhaseInSync {
				t.Fatalf("phase = %q, want %q: %#v", actual.Status.Phase, operatorv1alpha1.PhaseInSync, actual.Status)
			}
			if actual.Status.PendingObservation != nil || actual.Status.ActiveOperation != nil {
				t.Fatalf("convergence left work owing: %#v", actual.Status)
			}
			if actual.Status.LastSuccessfulReconciliation == nil || actual.Status.NextReconciliationTime == nil {
				t.Fatalf("convergence recorded no resync boundary: %#v", actual.Status)
			}
			switch {
			case row.wantAttribute:
				if actual.Status.Applied == nil || actual.Status.Applied.PlanFingerprint != wantFingerprint {
					t.Fatalf("applied = %#v, want attribution to %q", actual.Status.Applied, wantFingerprint)
				}
			default:
				if actual.Status.Applied != nil {
					t.Fatalf("an unresolved Apply was credited by a later reading: %#v", actual.Status.Applied)
				}
			}
			condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync)
			if condition == nil || condition.Status != metav1.ConditionTrue ||
				condition.Reason != string(row.wantReason) {
				t.Fatalf("InSync condition = %#v, want true with reason %q", condition, row.wantReason)
			}
		})
	}
}
