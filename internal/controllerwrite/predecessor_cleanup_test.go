package controllerwrite

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func predecessorDigest(b byte) string {
	return "sha256:" + strings.Repeat(string(b), 64)
}

// predecessorCleanupFixture is the narrowest thing the function accepts: a
// schema at the retirement fence, evidence of an Apply nobody accounted for,
// and the exact Job that evidence names.
func predecessorCleanupFixture() (
	*operatorv1alpha1.PtahSchema,
	*operatorv1alpha1.PendingObservationStatus,
	*batchv1.Job,
) {
	const operationID = "an-apply-nobody-accounted-for"
	retired := "v1-" + strings.Repeat("1", 32)
	current := "v1-" + strings.Repeat("9", 32)

	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid"},
	}
	schema.Status.Phase = operatorv1alpha1.PhasePending
	schema.Status.ExecutionBinding = &operatorv1alpha1.ExecutionBindingStatus{Epoch: current}
	schema.Status.Conditions = []metav1.Condition{
		{
			Type: operatorv1alpha1.ConditionPlanReady, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonExecutionBindingChanged),
		},
		{
			Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonExecutionBindingChanged),
		},
	}

	pending := &operatorv1alpha1.PendingObservationStatus{
		Outcome:          operatorv1alpha1.PendingObservationOutcomeUnknown,
		ApplyOperationID: operationID,
		ApplyJobName:     "ptah-apply-app-0123456789abcdef",
		ApplyJobUID:      types.UID("apply-job-uid"),
		Plan: operatorv1alpha1.CurrentPlanStatus{
			ExecutionBindingID: retired,
			Fingerprint:        predecessorDigest('a'),
			ContentDigest:      predecessorDigest('b'),
			PtahVersion:        "v0.3.0",
		},
	}

	annotations := map[string]string{
		workload.AnnotationOperationID:             operationID,
		workload.AnnotationInputFingerprint:        predecessorDigest('c'),
		workload.AnnotationPtahVersion:             pending.Plan.PtahVersion,
		workload.AnnotationExecutionBindingID:      retired,
		workload.AnnotationPlanFingerprint:         pending.Plan.Fingerprint,
		workload.AnnotationPlanContentDigest:       pending.Plan.ContentDigest,
		workload.AnnotationAdmissionSnapshotDigest: predecessorDigest('d'),
	}
	labels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   "apply",
		workload.LabelOperationID: workload.OperationIDLabelValue(operationID),
	}
	controller := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace, Name: pending.ApplyJobName, UID: pending.ApplyJobUID,
			Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema",
				Name: schema.Name, UID: schema.UID,
				Controller: &controller, BlockOwnerDeletion: &controller,
			}},
		},
	}
	job.Spec.Template.Annotations = copyStringMap(annotations)
	job.Spec.Template.Labels = copyStringMap(labels)
	return schema, pending, job
}

func copyStringMap(source map[string]string) map[string]string {
	target := make(map[string]string, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}

// The predecessor Apply is a run from a binding that has since been retired,
// and whose outcome nobody established. Scheduling its Job's deletion is
// permitted because the operator has already written down everything the Job
// could still tell it -- which is only true while the evidence and the Job
// agree about which run this is.
//
// Every part of that agreement could be removed with the package green.
func TestOnlyTheExactPredecessorApplyJobMayBeCollected(t *testing.T) {
	t.Parallel()

	schema, pending, job := predecessorCleanupFixture()
	if err := validatePredecessorApplyCleanup(schema, pending, job); err != nil {
		t.Fatalf("the exact predecessor Apply was refused, so nothing below proves anything: %v", err)
	}

	for _, row := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PendingObservationStatus, *batchv1.Job)
	}{
		{
			name: "a claim is in flight again",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationObserve,
				}
			},
		},
		{
			name: "the schema has not entered the fence",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				schema.Status.Phase = operatorv1alpha1.PhaseObserving
			},
		},
		{
			name: "the outcome is no longer unknown",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.Outcome = operatorv1alpha1.PendingObservationApplySucceeded
			},
		},
		{
			name: "the observation still owes a plan",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.PlanRequired = true
			},
		},
		{
			name: "a retirement condition gives another reason",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				schema.Status.Conditions[1].Reason = string(operatorv1alpha1.ReasonStale)
			},
		},
		{
			// Not a predecessor: it is the binding in force.
			name: "the Apply belongs to the binding still in force",
			change: func(schema *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.Plan.ExecutionBindingID = schema.Status.ExecutionBinding.Epoch
			},
		},
		{
			name: "the retired epoch is malformed",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.Plan.ExecutionBindingID = "not-an-epoch"
			},
		},
		{
			name: "the evidence names no Apply operation",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.ApplyOperationID = ""
			},
		},
		{
			// The Job under collection is not the Job the evidence describes,
			// so collecting it would take away a record of another run.
			name: "the Job is not the one the evidence names",
			change: func(_ *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, job *batchv1.Job) {
				job.Name += "-beside-it"
			},
		},
		{
			name: "the Job carries another identity",
			change: func(_ *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, job *batchv1.Job) {
				job.UID = "a-job-that-ran-later"
			},
		},
		{
			name: "the persisted plan fingerprint is not a digest",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.Plan.Fingerprint = "sha256:beef"
			},
		},
		{
			name: "the persisted plan content digest is not a digest",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.Plan.ContentDigest = ""
			},
		},
		{
			name: "the persisted data-plane version is ambiguous",
			change: func(_ *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus, _ *batchv1.Job) {
				pending.Plan.PtahVersion = " v0.3.0"
			},
		},
		{
			name: "the Job belongs to another schema",
			change: func(_ *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, job *batchv1.Job) {
				job.OwnerReferences[0].UID = "another-schema-uid"
			},
		},
		{
			name: "the Pod template envelope differs from the Job's",
			change: func(_ *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PendingObservationStatus, job *batchv1.Job) {
				job.Spec.Template.Annotations[workload.AnnotationInputFingerprint] = predecessorDigest('e')
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, pending, job := predecessorCleanupFixture()
			row.change(schema, pending, job)
			if err := validatePredecessorApplyCleanup(schema, pending, job); err == nil {
				t.Fatal("a Job that is not the exact predecessor Apply was cleared for collection")
			}
		})
	}
}
