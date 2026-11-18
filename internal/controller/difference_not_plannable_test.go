// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// The sentence the runner reports for the plan of notes the pinned Ptah v0.32.0
// saved against PostgreSQL 16 for a generated column whose expression changed,
// which internal/runner/testdata/note-plan holds. The runner's
// TestAPlanOfNotesIsADifferenceNotPlannable reads that capture to this
// sentence.
const unplannableNote = "ptah found a difference it cannot plan: WARNING: Generated column public.tasks.code changed, " +
	"but ALTER COLUMN SET EXPRESSION requires target capability alter_generated_column_expression, unavailable on " +
	"this target (PostgreSQL added it in 17); manual migration required."

// planningSchema is a Plan run whose Job has finished with frame: a verified
// source and an observed target.
func planningSchema(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := safetyApplySchema(t)
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Status.Plan = nil
	schema.Status.Source.Verified = true
	schema.Status.Source.Digest = testDigest
	schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
	schema.Status.Target.DriftReportDigest = testDigest
	schema.Status.ActiveOperation.Type = operatorv1alpha1.OperationPlan
	schema.Status.ActiveOperation.ID = "plan-operation"
	schema.Status.ActiveOperation.JobName = "plan-job"
	schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
	schema.Status.Source.VerificationPolicyUID = testPolicyUID
	schema.Status.Source.VerificationPolicyDigest = fingerprint.DigestBytes([]byte("policy"))
	bindActiveInput(t, schema)
	return schema
}

func reconcilePlanResult(t *testing.T, schema *operatorv1alpha1.PtahSchema, result runner.Result) (*operatorv1alpha1.PtahSchema, *record.FakeRecorder, time.Time) {
	t.Helper()
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, result)
	// The policy the source was verified under, unchanged, so that a Plan that
	// converges reads as converged.
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID,
		},
		Immutable: ptr(true),
		Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policy)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(schema),
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return safetyGetSchema(t, api, schema), recorder, reconciler.now()
}

// A plan of notes says the database differs and Ptah planned nothing for it.
// The resource is not in sync, there is nothing to approve, and the notes say
// what to do. The control is the same Plan reporting no changes, which reads as
// in sync: without it, a fixture that never reached convergence would pass the
// row.
func TestAPlanOfNotesIsNotInSync(t *testing.T) {
	t.Parallel()

	schema := planningSchema(t)
	actual, recorder, now := reconcilePlanResult(t, schema, runner.Result{
		ProtocolVersion: runner.ProtocolVersion,
		Operation:       runner.OperationPlan,
		OperationID:     schema.Status.ActiveOperation.ID,
		Error:           &runner.ResultError{Code: runner.CodeDifferenceNotPlannable, Message: unplannableNote},
	})
	if actual.Status.Plan != nil {
		t.Fatalf("a plan of notes was published: %#v", actual.Status.Plan)
	}
	if meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionInSync) ||
		actual.Status.Phase == operatorv1alpha1.PhaseInSync {
		t.Fatalf("a plan of notes reads as in sync: phase %s, %#v", actual.Status.Phase, actual.Status.Conditions)
	}
	for _, expected := range []struct {
		condition string
		status    metav1.ConditionStatus
	}{
		{operatorv1alpha1.ConditionDriftDetected, metav1.ConditionTrue},
		{operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse},
		{operatorv1alpha1.ConditionInSync, metav1.ConditionFalse},
		{operatorv1alpha1.ConditionReady, metav1.ConditionFalse},
	} {
		if !conditionMatches(actual.Status.Conditions, expected.condition, expected.status, operatorv1alpha1.ReasonDifferenceNotPlannable) {
			t.Fatalf("%s is not %s with %s: %#v", expected.condition, expected.status,
				operatorv1alpha1.ReasonDifferenceNotPlannable, actual.Status.Conditions)
		}
	}
	if ready := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReady); !strings.Contains(ready.Message, unplannableNote) {
		t.Fatalf("Ready does not carry the notes: %q", ready.Message)
	}
	// Nothing went wrong, and nothing is waiting for a person to approve.
	if failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed != nil &&
		failed.Status == metav1.ConditionTrue {
		t.Fatalf("a plan of notes was reported as a failure: %#v", failed)
	}
	if meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired) {
		t.Fatalf("a plan of notes asks for an approval: %#v", actual.Status.Conditions)
	}
	// The operation ends, and the next read is a whole interval away, not a
	// retry within the second that would find the same difference.
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the plan of notes left an operation to retry: %#v", actual.Status.ActiveOperation)
	}
	next := actual.Status.NextReconciliationTime
	if next == nil || next.Sub(now) < interval(actual)-time.Second {
		t.Fatalf("the next read is at %v, want %v after %v", next, interval(actual), now)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "DifferenceNotPlannable") || !strings.Contains(event, "public.tasks.code") {
			t.Fatalf("Event = %q, want DifferenceNotPlannable with the note", event)
		}
	default:
		t.Fatal("no Event says why nothing was planned")
	}

	control := planningSchema(t)
	converged, _, _ := reconcilePlanResult(t, control, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          control.Status.ActiveOperation.ID,
		CoordinationDigest:   control.Status.Target.CoordinationDigest,
		TargetIdentityDigest: control.Status.Target.IdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	})
	if !meta.IsStatusConditionTrue(converged.Status.Conditions, operatorv1alpha1.ConditionInSync) {
		t.Fatalf("the control did not reach InSync, so the row measured nothing: phase %s, %#v",
			converged.Status.Phase, converged.Status.Conditions)
	}
}
