package controllerwrite

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// subjectOwner returns the one controller owner of a managed Job together with
// the kind of resource that claimed it. A Job owned by anything else is outside
// the controller write contract, which is what keeps the manager's write
// permission from reaching a workload no claim accounts for.
func subjectOwner(references []metav1.OwnerReference) (metav1.OwnerReference, string, error) {
	if len(references) != 1 {
		return metav1.OwnerReference{}, "", errors.New("ownership graph is not a singleton")
	}
	kind := references[0].Kind
	if kind != "PtahSchema" && kind != "PtahMigration" {
		return metav1.OwnerReference{}, "", errors.New("controller owner is neither a PtahSchema nor a PtahMigration")
	}
	owner, err := exactControllerOwner(references, operatorv1alpha1.GroupVersion.String(), kind)
	if err != nil {
		return metav1.OwnerReference{}, "", err
	}
	return owner, kind, nil
}

func (v *Validator) validateMigrationJobCreate(
	ctx context.Context,
	job *batchv1.Job,
	owner metav1.OwnerReference,
) error {
	migration, err := v.readMigration(ctx, job.Namespace, owner, false)
	if err != nil {
		return err
	}
	operation := migration.Status.ActiveOperation
	if operation == nil || operation.JobName != job.Name || operation.JobUID != "" {
		return denyf("Job does not match a not-yet-created migration operation")
	}
	plan, err := v.migrationPlanForJob(ctx, migration, operation)
	if err != nil {
		return err
	}
	expected, err := v.Jobs.BuildMigration(migration.DeepCopy(), *operation.DeepCopy(), plan)
	if err != nil {
		return denyf("migration operation cannot reconstruct the submitted Job: %v", err)
	}
	claim := jobclaim.MigrationOperation(migration, operation)
	claim.Binding = migration.Status.ExecutionBinding
	claim.Built = expected
	if err := jobclaim.Match(job, claim); err != nil {
		return denyf("Job is outside the migration operation intent: %v", err)
	}
	return nil
}

// migrationPlanForJob re-reads the immutable plan a migration Apply claim
// named, so the reconstructed Job carries the same plan identity the submitted
// one must. Every other migration operation carries no plan.
func (v *Validator) migrationPlanForJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) (*operatorv1alpha1.PtahMigrationPlan, error) {
	if operation.Type != operatorv1alpha1.MigrationOperationApply {
		return nil, nil
	}
	if operation.PlanRef == nil || operation.PlanRef.Name == "" || operation.PlanRef.UID == "" {
		return nil, denyf("migration Apply operation names no immutable plan")
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := client.ObjectKey{Namespace: migration.Namespace, Name: operation.PlanRef.Name}
	if err := v.Reader.Get(ctx, key, plan); err != nil {
		return nil, internalf("directly read migration Apply plan %s/%s: %v", key.Namespace, key.Name, err)
	}
	if plan.UID != operation.PlanRef.UID {
		return nil, denyf("migration Apply plan UID does not match the operation claim")
	}
	return plan, nil
}

// validateMigrationJobUpdate authorizes one write on a migration Job: the
// nil-to-300 cleanup TTL the controller sets once it has harvested the result.
// Everything else about the Job is immutable, and a Job whose claim is gone is
// never rewritten, because a claim is what makes a Job attributable.
func (v *Validator) validateMigrationJobUpdate(
	ctx context.Context,
	oldJob, job *batchv1.Job,
	owner metav1.OwnerReference,
) error {
	newOwner, newKind, err := subjectOwner(job.OwnerReferences)
	if err != nil || newKind != "PtahMigration" || !reflect.DeepEqual(owner, newOwner) {
		return denyf("Job cleanup update changed the PtahMigration controller owner")
	}
	migration, err := v.readMigration(ctx, oldJob.Namespace, owner, true)
	if err != nil {
		return err
	}
	if !jobTerminal(oldJob) {
		return denyf("migration Job cleanup TTL cannot be set before terminal status")
	}
	operation := migration.Status.ActiveOperation
	if operation == nil || operation.JobName != oldJob.Name || operation.JobUID != oldJob.UID {
		return denyf("Job is not the exact active migration operation instance")
	}
	if err := validateClaimBoundMigrationJobCleanup(migration, operation, oldJob); err != nil {
		return denyf("migration Job cleanup is outside the persisted operation claim: %v", err)
	}
	if err := validateOnlyCleanupTTLChanged(oldJob, job); err != nil {
		return denyf("Job cleanup update changes fields outside the cleanup TTL: %v", err)
	}
	return nil
}

// validateClaimBoundMigrationJobCleanup checks the terminal Job against the
// claim without rebuilding it. The claim, the execution binding and the
// persisted admission snapshot are what make the Job attributable; rebuilding
// would additionally require every input the Job was built from to still read
// the same, which a terminal Job no longer needs.
func validateClaimBoundMigrationJobCleanup(
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	job *batchv1.Job,
) error {
	if migration == nil || operation == nil || job == nil || migration.Status.ExecutionBinding == nil {
		return errors.New("migration operation cleanup inputs are incomplete")
	}
	expectedName, err := workload.NameForMigration(migration, *operation.DeepCopy())
	if err != nil {
		return fmt.Errorf("derive claimed Job name: %w", err)
	}
	if operation.JobName != expectedName || operation.JobUID == "" {
		return errors.New("the persisted operation claim does not name the Job it reserved")
	}
	claim := jobclaim.MigrationOperation(migration, operation)
	claim.Binding = migration.Status.ExecutionBinding
	return jobclaim.Match(job, claim)
}

func (v *Validator) readMigration(
	ctx context.Context,
	namespace string,
	owner metav1.OwnerReference,
	allowDeleting bool,
) (*operatorv1alpha1.PtahMigration, error) {
	migration := &operatorv1alpha1.PtahMigration{}
	key := client.ObjectKey{Namespace: namespace, Name: owner.Name}
	if err := v.Reader.Get(ctx, key, migration); err != nil {
		return nil, internalf("directly read PtahMigration %s/%s: %v", key.Namespace, key.Name, err)
	}
	if migration.UID == "" || migration.UID != owner.UID {
		return nil, denyf("controller owner does not match the current PtahMigration UID")
	}
	if !allowDeleting && migration.DeletionTimestamp != nil {
		return nil, denyf("controller owner does not match a current non-deleting PtahMigration UID")
	}
	return migration, nil
}
