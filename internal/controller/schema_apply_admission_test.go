package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// builtSchemaApplyJobPath is the schema Apply Job the workload builder writes.
// The builder's golden test fails when the file and the builder disagree, so a
// test here that reads it measures the checks against what the builder
// produces, not against a copy of the envelope written by hand.
var builtSchemaApplyJobPath = filepath.Join("..", "workload", "testdata", "jobs", "schema-apply-admitted-scheduling.json")

// A successor that finds the predecessor's Apply Job across an execution
// binding change holds it to the exact envelope the builder writes before it
// adopts the Job's UID and schedules its cleanup, and the cleanup passes the
// controller-write webhook, which holds it to the same envelope. When both fell
// behind the builder, the successor left the quiesced Apply Job alone forever.
func TestASuccessorAdoptsAndRetiresTheApplyJobTheBuilderWrote(t *testing.T) {
	t.Parallel()

	job, snapshot := builtSchemaApplyJob(t)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	schema := schemaOwning(job)
	annotations := job.Annotations
	schema.Status.Phase = operatorv1alpha1.PhasePending
	schema.Status.ActiveOperation = nil
	schema.Status.ExecutionBinding.Epoch = "v1-99999999999999999999999999999999"
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome:           operatorv1alpha1.PendingObservationOutcomeUnknown,
		ApplyOperationID:  annotations[workload.AnnotationOperationID],
		ApplyJobName:      job.Name,
		AdmissionSnapshot: snapshot,
		Plan:              planBindingOf(t, job),
	}
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonExecutionBindingChanged, "retired")
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse,
		operatorv1alpha1.ReasonExecutionBindingChanged, "retired")
	if schema.Status.PendingObservation.Plan.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		t.Fatal("the fixture did not retire the epoch the built Job belongs to")
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job)
	reconciler.Client = admittedJobWrites(api.(client.WithWatch), refusingJobBuilder{})

	stored := safetyGetSchema(t, api, schema)
	adopted, err := reconciler.adoptRetiredPredecessorApplyJobUID(context.Background(), stored, stored.Status.PendingObservation)
	if err != nil || !adopted {
		t.Fatalf("adoptRetiredPredecessorApplyJobUID() = %t, %v; the successor did not recognize the built Apply Job", adopted, err)
	}
	stored = safetyGetSchema(t, api, schema)
	if stored.Status.PendingObservation.ApplyJobUID != job.UID {
		t.Fatalf("adopted Job UID = %q, want %q", stored.Status.PendingObservation.ApplyJobUID, job.UID)
	}
	retired, err := reconciler.cleanupRetiredPredecessorApplyJob(context.Background(), stored, stored.Status.PendingObservation)
	if err != nil || !retired {
		t.Fatalf("cleanupRetiredPredecessorApplyJob() = %t, %v; the built Apply Job was not retired", retired, err)
	}
	assertCleanupScheduled(t, api, job)
}

// The claim that dispatched an Apply schedules its Job's cleanup from the
// claim, without rebuilding the Job, and may do so while the Apply still runs
// because the TTL counts only from the terminal condition. Both cases go
// through the webhook's classification of the Job's annotations, which has to
// recognize the Apply Job the builder writes.
func TestTheApplyClaimSchedulesCleanupForTheJobTheBuilderWrote(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		conditions []batchv1.JobCondition
	}{
		{name: "running"},
		{name: "complete", conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			job, snapshot := builtSchemaApplyJob(t)
			job.Status.Conditions = test.conditions
			schema := schemaOwning(job)
			annotations := job.Annotations
			plan := planBindingOf(t, job)
			binding := schema.Status.ExecutionBinding
			binding.Epoch = plan.ExecutionBindingID
			binding.ControllerImage = plan.ControllerImage
			binding.ControllerRevision = plan.ControllerRevision
			binding.ControllerStateVersion = plan.ControllerStateVersion
			binding.PtahVersion = plan.PtahVersion
			schema.Status.Plan = &plan
			operation := &operatorv1alpha1.ActiveOperationStatus{
				Type:               operatorv1alpha1.OperationApply,
				ID:                 annotations[workload.AnnotationOperationID],
				InputFingerprint:   annotations[workload.AnnotationInputFingerprint],
				ExecutionBindingID: annotations[workload.AnnotationExecutionBindingID],
				Attempt:            1,
				JobName:            job.Name,
				JobUID:             job.UID,
				AdmissionSnapshot:  snapshot,
			}
			if name, err := workload.NameFor(schema, *operation); err != nil || name != job.Name {
				t.Fatalf("the claim names Job %q (%v), want the built %q", name, err, job.Name)
			}
			schema.Status.ActiveOperation = operation
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, job)
			reconciler.Client = admittedJobWrites(api.(client.WithWatch), refusingJobBuilder{})

			stored := &batchv1.Job{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
				t.Fatal(err)
			}
			if err := reconciler.markJobHarvested(context.Background(), stored); err != nil {
				t.Fatalf("markJobHarvested() error = %v", err)
			}
			assertCleanupScheduled(t, api, job)
		})
	}
}

// builtSchemaApplyJob returns the built schema Apply Job with the identity the
// API server gives it, and the admission snapshot digested from its template.
// Only the snapshot digest annotation's value changes; the builder's is a
// placeholder that no snapshot hashes to.
func builtSchemaApplyJob(t *testing.T) (*batchv1.Job, *operatorv1alpha1.PodAdmissionSnapshot) {
	t.Helper()

	data, err := os.ReadFile(builtSchemaApplyJobPath)
	if err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(data, job); err != nil {
		t.Fatal(err)
	}
	if job.Labels[workload.LabelOperation] != "apply" || len(job.OwnerReferences) != 1 {
		t.Fatalf("%s is not a schema Apply Job with one owner", builtSchemaApplyJobPath)
	}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	preemption := corev1.PreemptLowerPriority
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{Object: operatorv1alpha1.AdmissionObjectBinding{
			Name: "default", UID: "default-service-account-uid", ResourceVersion: "1",
		}},
		PriorityClass:                       operatorv1alpha1.PriorityClassAdmissionSnapshot{Value: 0, PreemptionPolicy: &preemption},
		DefaultTolerationsEnabled:           true,
		DefaultNotReadyTolerationSeconds:    300,
		DefaultUnreachableTolerationSeconds: 300,
	}
	if snapshot.Digest, err = fingerprint.DigestCanonicalJSON(*snapshot); err != nil {
		t.Fatal(err)
	}
	job.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest

	job.UID = types.UID("built-apply-job-uid")
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
	}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	return job, snapshot
}

// schemaOwning is the schema the built Job's controller reference names.
func schemaOwning(job *batchv1.Job) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	owner := job.OwnerReferences[0]
	schema.Namespace = job.Namespace
	schema.Name = owner.Name
	schema.UID = owner.UID
	return schema
}

// planBindingOf is the current plan binding the built Job's annotations name.
func planBindingOf(t *testing.T, job *batchv1.Job) operatorv1alpha1.CurrentPlanStatus {
	t.Helper()

	annotations := job.Annotations
	stateVersion, err := strconv.ParseInt(annotations[workload.AnnotationControllerStateVersion], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return operatorv1alpha1.CurrentPlanStatus{
		Name:                   "ptah-plan-built",
		UID:                    "built-plan-uid",
		Fingerprint:            annotations[workload.AnnotationPlanFingerprint],
		ContentDigest:          annotations[workload.AnnotationPlanContentDigest],
		ExecutionBindingID:     annotations[workload.AnnotationExecutionBindingID],
		ControllerImage:        annotations[workload.AnnotationControllerImage],
		ControllerRevision:     annotations[workload.AnnotationControllerRevision],
		ControllerStateVersion: int32(stateVersion),
		PtahVersion:            annotations[workload.AnnotationPtahVersion],
	}
}

func assertCleanupScheduled(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	stored := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.TTLSecondsAfterFinished == nil || *stored.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatalf("the Job's cleanup TTL = %v, want %d", stored.Spec.TTLSecondsAfterFinished, jobCleanupTTLSeconds)
	}
}

// refusingJobBuilder stands in for the builder where the webhook must decide
// from the claim alone. A path that rebuilds the Job is the wrong path for
// these writes, and fails here instead of passing on a rebuild.
type refusingJobBuilder struct{}

func (refusingJobBuilder) Build(
	*operatorv1alpha1.PtahSchema,
	operatorv1alpha1.ActiveOperationStatus,
	*operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	return nil, errors.New("this cleanup must be decided from the claim, not by rebuilding the Job")
}

func (refusingJobBuilder) BuildMigration(
	*operatorv1alpha1.PtahMigration,
	operatorv1alpha1.MigrationOperationStatus,
	*operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, error) {
	return nil, errors.New("this cleanup must be decided from the claim, not by rebuilding the Job")
}
