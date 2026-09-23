package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The intent check is what stands between a claim and an object that merely
// occupies the name it reserved. It runs twice on the path that matters: once
// on the Job the operator has just created, and once on every later pass that
// finds one standing.
//
// Its comparisons are written as disjunctions, so one difference exercises the
// whole branch and coverage reads it as measured. Each of the four could be
// deleted with the package staying green: a Job differing only in its name,
// its namespace, its labels or its annotations would have been accepted as the
// one the claim described. The rows below put one difference at a time.
//
// The annotations row is the one with teeth. They carry the execution binding
// the Job belongs to and the digest of the Pod template admission approved, so
// a Job whose annotations differ was admitted under a different intent.

func intentRows() []struct {
	name   string
	differ func(*batchv1.Job)
} {
	return []struct {
		name   string
		differ func(*batchv1.Job)
	}{
		{
			name:   "another name",
			differ: func(job *batchv1.Job) { job.Name += "-2" },
		},
		{
			name:   "another namespace",
			differ: func(job *batchv1.Job) { job.Namespace = "somebody-elses-namespace" },
		},
		{
			name: "another owner",
			differ: func(job *batchv1.Job) {
				job.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema",
					Name: "another-resource", UID: types.UID("another-resource-uid"),
					Controller: ptr(true), BlockOwnerDeletion: ptr(true),
				}}
			},
		},
		{
			name: "no owner at all",
			differ: func(job *batchv1.Job) {
				job.OwnerReferences = nil
			},
		},
		{
			name: "another label",
			differ: func(job *batchv1.Job) {
				if job.Labels == nil {
					job.Labels = map[string]string{}
				}
				job.Labels["operator.ptah.run/operation"] = "something-else"
			},
		},
		{
			name: "another execution binding annotation",
			differ: func(job *batchv1.Job) {
				job.Annotations[workload.AnnotationExecutionBindingID] =
					"v1-99999999999999999999999999999999"
			},
		},
		{
			name: "another workload spec",
			differ: func(job *batchv1.Job) {
				job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers,
					corev1.Container{Name: "sidecar", Image: "example.invalid/anything@sha256:0"})
			},
		},
		{
			name:   "no identity",
			differ: func(job *batchv1.Job) { job.UID = "" },
		},
	}
}

func TestASchemaJobIsOnlyItsClaimsWhenEveryPartMatches(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	operation := schema.Status.ActiveOperation
	expected, err := (fakeJobs{}).Build(schema, *operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected.UID = "job-uid"
	expected.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema",
		Name: schema.Name, UID: schema.UID, Controller: ptr(true), BlockOwnerDeletion: ptr(true),
	}}
	if err := validateJobIntent(expected.DeepCopy(), expected, schema); err != nil {
		t.Fatalf("an exact copy of the claim's Job was refused, so nothing below proves anything: %v", err)
	}

	for _, row := range intentRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			actual := expected.DeepCopy()
			row.differ(actual)
			if err := validateJobIntent(actual, expected, schema); err == nil {
				t.Fatalf("a Job with %s was accepted as the one the claim described", row.name)
			}
		})
	}
}

func TestAMigrationJobIsOnlyItsClaimsWhenEveryPartMatches(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	expected, err := (fakeJobs{}).BuildMigration(migration, *operation, plan)
	if err != nil {
		t.Fatal(err)
	}
	expected.UID = "job-uid"
	if err := validateMigrationJobIntent(expected.DeepCopy(), expected, migration); err != nil {
		t.Fatalf("an exact copy of the claim's Job was refused, so nothing below proves anything: %v", err)
	}

	for _, row := range intentRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			actual := expected.DeepCopy()
			row.differ(actual)
			if err := validateMigrationJobIntent(actual, expected, migration); err == nil {
				t.Fatalf("a Job with %s was accepted as the one the claim described", row.name)
			}
		})
	}
}
