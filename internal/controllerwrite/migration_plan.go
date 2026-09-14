package controllerwrite

import (
	"context"
	"errors"
	"reflect"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
)

var (
	migrationPlanResource = metav1.GroupVersionResource{
		Group:    operatorv1alpha1.GroupVersion.Group,
		Version:  operatorv1alpha1.GroupVersion.Version,
		Resource: "ptahmigrationplans",
	}
	migrationPlanKind = metav1.GroupVersionKind{
		Group:   operatorv1alpha1.GroupVersion.Group,
		Version: operatorv1alpha1.GroupVersion.Version,
		Kind:    "PtahMigrationPlan",
	}
)

// validateMigrationPlanCreate admits only the manifest the migration's own
// status accounts for. The plan is what an approval names and what an Apply
// carries out, so it is checked against the resource that published it rather
// than trusted because the manager sent it.
func (v *Validator) validateMigrationPlanCreate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) != 0 {
		return badRequestf("PtahMigrationPlan create unexpectedly contains an old object")
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := decodeObject(req.Object.Raw, plan, migrationPlanKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &plan.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(plan.Status, operatorv1alpha1.PtahMigrationPlanStatus{}) {
		return denyf("new PtahMigrationPlan must not inject status")
	}
	owner, ownerKind, err := subjectOwner(plan.OwnerReferences)
	if err != nil || ownerKind != "PtahMigration" {
		return denyf("PtahMigrationPlan does not have one exact PtahMigration controller owner")
	}
	migration, err := v.readMigration(ctx, plan.Namespace, owner, false)
	if err != nil {
		return err
	}
	if err := validateMigrationPlanMetadata(plan, migration); err != nil {
		return denyf("PtahMigrationPlan metadata is invalid: %v", err)
	}
	if err := validateMigrationPlanShape(plan, migration); err != nil {
		return denyf("PtahMigrationPlan manifest is invalid: %v", err)
	}
	return nil
}

func validateMigrationPlanMetadata(
	plan *operatorv1alpha1.PtahMigrationPlan,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if plan == nil || migration == nil {
		return errors.New("plan metadata inputs are incomplete")
	}
	if plan.Namespace != migration.Namespace ||
		plan.Spec.MigrationRef.Name != migration.Name || plan.Spec.MigrationRef.UID != migration.UID {
		return errors.New("plan migration reference does not match its namespace and owner")
	}
	if plan.DeletionTimestamp != nil || plan.DeletionGracePeriodSeconds != nil {
		return errors.New("a deleting plan cannot be published")
	}
	wantName, err := migrationplan.Name(plan.Spec.Fingerprint)
	if err != nil || plan.Name != wantName {
		return errors.New("plan name is not derived from its fingerprint")
	}
	expected, err := migrationplan.Desired(migration, plan.Spec)
	if err != nil {
		return err
	}
	actual := plan.ObjectMeta.DeepCopy()
	scrubCreateServerMetadata(actual)
	if !reflect.DeepEqual(actual, &expected.ObjectMeta) {
		return errors.New("plan metadata contains fields outside the immutable manifest contract")
	}
	return nil
}

// validateMigrationPlanShape re-derives the plan's identity from the migration
// status it claims to have been decided from. A plan nobody can reproduce is a
// plan nothing can check, so a fingerprint that does not follow from the
// bindings in the same object is refused.
func validateMigrationPlanShape(
	plan *operatorv1alpha1.PtahMigrationPlan,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if plan.Spec.ContractVersion != migrationplan.ContractVersion {
		return errors.New("plan contract version is not the one this manager publishes")
	}
	binding := migration.Status.ExecutionBinding
	history := migration.Status.History
	artifact := migration.Status.Artifact
	if binding == nil || history == nil || artifact == nil {
		return errors.New("the migration has no resolved artifact, history, and execution binding to plan from")
	}
	if plan.Spec.HistoryFingerprint != history.Fingerprint {
		return errors.New("plan history fingerprint does not match the history the migration read")
	}
	if plan.Spec.CurrentVersion != history.CurrentVersion {
		return errors.New("plan current version does not match the history the migration read")
	}
	if plan.Spec.TargetIdentityDigest != history.TargetIdentityDigest {
		return errors.New("plan target identity does not match the database the history came from")
	}
	if plan.Spec.ArtifactDigest != artifact.Digest {
		return errors.New("plan artifact digest does not match the resolved artifact")
	}
	if plan.Spec.ExecutionBindingID != binding.Epoch ||
		plan.Spec.ControllerImage != binding.ControllerImage ||
		plan.Spec.ControllerRevision != binding.ControllerRevision ||
		plan.Spec.ControllerStateVersion != binding.ControllerStateVersion ||
		plan.Spec.PtahVersion != binding.PtahVersion ||
		plan.Spec.ExecutorImage != binding.ExecutorImage ||
		plan.Spec.RunnerImage != binding.RunnerImage ||
		plan.Spec.RunnerProtocolVersion != binding.RunnerProtocolVersion {
		return errors.New("plan execution binding is not the migration's current one")
	}
	if len(plan.Spec.Migrations) == 0 || len(plan.Spec.Migrations) > migrationplan.MaxMigrations {
		return errors.New("plan carries no executable sequence")
	}
	if int(history.PendingCount) != len(plan.Spec.Migrations) {
		return errors.New("plan sequence length does not match the pending count the history reported")
	}
	sequenceDigest, err := migrationplan.SequenceDigest(plan.Spec.Migrations)
	if err != nil {
		return err
	}
	wantFingerprint, err := migrationplan.Binding{
		MigrationUID:             migration.UID,
		HistoryFingerprint:       plan.Spec.HistoryFingerprint,
		SequenceDigest:           sequenceDigest,
		ArtifactDigest:           plan.Spec.ArtifactDigest,
		CoordinationDigest:       plan.Spec.CoordinationDigest,
		TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
		PolicyFingerprint:        plan.Spec.PolicyFingerprint,
		VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
		VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		ExecutionBindingID:       plan.Spec.ExecutionBindingID,
		ControllerImage:          plan.Spec.ControllerImage,
		ControllerRevision:       plan.Spec.ControllerRevision,
		ControllerStateVersion:   plan.Spec.ControllerStateVersion,
		PtahVersion:              plan.Spec.PtahVersion,
		ExecutorImage:            plan.Spec.ExecutorImage,
		RunnerImage:              plan.Spec.RunnerImage,
		RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
	}.Fingerprint()
	if err != nil {
		return err
	}
	if plan.Spec.Fingerprint != wantFingerprint {
		return errors.New("plan fingerprint does not follow from the bindings it carries")
	}
	return nil
}
