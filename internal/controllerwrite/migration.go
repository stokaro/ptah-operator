package controllerwrite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/podintent"
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
	if err := validateMigrationAdmissionSnapshot(operation, expected); err != nil {
		return denyf("migration operation Pod admission snapshot is invalid: %v", err)
	}
	if err := validateJobIntent(job, expected, migrationSubject(migration), true); err != nil {
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
	if job.Namespace != migration.Namespace || operation.JobName != expectedName || job.Name != expectedName ||
		operation.JobUID == "" || operation.JobUID != job.UID {
		return errors.New("Job name, namespace, or UID does not match the persisted operation claim")
	}
	if _, err := exactNamedControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahMigration",
		migration.Name,
		migration.UID,
	); err != nil {
		return fmt.Errorf("Job owner does not match the current migration UID: %w", err)
	}
	if operation.ExecutionBindingID != migration.Status.ExecutionBinding.Epoch {
		return errors.New("operation claim was authorized under a retired execution binding")
	}
	if err := validateCurrentExecutionEnvelope(migration.Status.ExecutionBinding, job.Annotations); err != nil {
		return err
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return fmt.Errorf("persisted Pod admission snapshot is invalid: %w", err)
	}

	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   workload.ComponentMigrationOperation,
		workload.LabelMigration:   migration.Name,
		workload.LabelOperation:   strings.ToLower(string(operation.Type)),
		workload.LabelOperationID: workload.OperationIDLabelValue(operation.ID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return errors.New("Job labels do not match the persisted operation claim")
	}
	if err := validateControllerEnvelopeValues(job.Annotations); err != nil {
		return err
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             operation.ID,
		workload.AnnotationInputFingerprint:        operation.InputFingerprint,
		workload.AnnotationPtahVersion:             job.Annotations[workload.AnnotationPtahVersion],
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
		workload.AnnotationControllerImage:         job.Annotations[workload.AnnotationControllerImage],
		workload.AnnotationControllerRevision:      job.Annotations[workload.AnnotationControllerRevision],
		workload.AnnotationControllerStateVersion:  job.Annotations[workload.AnnotationControllerStateVersion],
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) {
		return errors.New("Job annotations are not the exact current operation envelope")
	}
	if !reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return errors.New("Job Pod template annotations differ from the current operation envelope")
	}
	normalized := job.DeepCopy()
	if err := normalizeJobForComparison(normalized, true); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return errors.New("Job Pod template labels differ from the persisted operation claim")
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	if err != nil {
		return fmt.Errorf("digest claimed Job Pod template: %w", err)
	}
	if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
		return errors.New("Job Pod template does not match the persisted admission snapshot")
	}
	return nil
}

func validateMigrationAdmissionSnapshot(
	operation *operatorv1alpha1.MigrationOperationStatus,
	expected *batchv1.Job,
) error {
	if operation == nil || expected == nil {
		return errors.New("migration operation or reconstructed Job is missing")
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return err
	}
	templateDigest, err := podintent.DigestTemplate(&expected.Spec.Template)
	if err != nil {
		return err
	}
	if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
		return errors.New("snapshot template digest does not match the reconstructed Job")
	}
	return nil
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

// validateMigrationJobDetach authorizes the one write that removes a
// PtahMigration from its Apply Job's owner references.
//
// It exists for foreground cascading deletion, which removes an owner's
// dependents before the owner. An Apply Job left owned is handed to the
// garbage collector while the controller is still waiting to find out whether
// its executor can still write, which stops SQL between statements; the
// resource's finalizer cannot prevent it, because the finalizer holds the
// owner and the dependent goes first.
//
// Everything about it is narrow. The resource has to be one that is going
// away, the Job has to be the exact instance its claim dispatched, exactly
// that owner reference may go, and nothing else about the Job may change. A
// Job detached while its resource is alive would be a Job nothing owns and
// nothing collects, which is why the deletion timestamp is required rather
// than assumed.
func (v *Validator) validateMigrationJobDetach(
	ctx context.Context,
	oldJob, job *batchv1.Job,
	owner metav1.OwnerReference,
) error {
	if _, _, err := subjectOwner(job.OwnerReferences); err == nil {
		return denyf("migration Job detach left an operator controller owner in place")
	}
	for _, kept := range job.OwnerReferences {
		if kept.UID == owner.UID {
			return denyf("migration Job detach did not remove the PtahMigration owner")
		}
	}
	if len(oldJob.OwnerReferences)-len(job.OwnerReferences) != 1 {
		return denyf("migration Job detach removed more than the PtahMigration owner")
	}
	migration, err := v.readMigration(ctx, oldJob.Namespace, owner, true)
	if err != nil {
		return err
	}
	if migration.DeletionTimestamp == nil {
		return denyf("migration Job detach is only for a PtahMigration that is being deleted")
	}
	operation := migration.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply {
		return denyf("migration Job detach has no Apply claim to account for")
	}
	if operation.JobName != oldJob.Name || operation.JobUID != oldJob.UID {
		return denyf("Job is not the exact active migration operation instance")
	}
	if err := validateOnlyOwnerReferencesChanged(oldJob, job); err != nil {
		return denyf("migration Job detach changes fields outside the owner references: %v", err)
	}
	return nil
}
