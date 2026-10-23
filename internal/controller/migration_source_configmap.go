package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// resolveMigrationConfigMapSource is Resolve for a migration directory held
// in a ConfigMap. It copies the selected files into a snapshot, records the
// snapshot as the artifact every later operation reads, and hands the
// resource to the history read. A ConfigMap source has no verification
// policy, so there is no Verify step and no ArtifactVerified condition.
func (r *MigrationReconciler) resolveMigrationConfigMapSource(ctx context.Context, migration *operatorv1alpha1.PtahMigration) (ctrl.Result, error) {
	owner := sourcesnapshot.Owner{Kind: "PtahMigration", Name: migration.Name, UID: migration.UID}
	policy, policyBinding, err := sourcePolicyFor(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact)
	if err != nil {
		if errors.Is(err, errSourceRefused) {
			return r.migrationOperationFailure(ctx, migration, err)
		}
		return ctrl.Result{}, err
	}
	resolved, err := readConfigMapSource(ctx, r.directReader(), r.Client, migration.Namespace, owner, *migration.Spec.Artifact.ConfigMap, false, policy)
	if err != nil {
		if errors.Is(err, errSourceRefused) {
			return r.migrationOperationFailure(ctx, migration, err)
		}
		return ctrl.Result{}, err
	}
	before := migration.DeepCopy()
	migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Snapshot: resolved.snapshot.DeepCopy()}
	migration.Status.ConfigMap = resolved.revision.DeepCopy()
	migration.Status.SourcePolicy = policyBinding
	migration.Status.ObservedGeneration = migration.Generation
	now := r.now()
	slot, slotted := refreshSlot(ctx, r.Client, migration.UID, migrationInterval(migration))
	migration.Status.NextReconciliationTime = claimDeadline(migration.Status.NextReconciliationTime,
		true, false, now, migrationInterval(migration), slot, slotted)
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	meta.RemoveStatusCondition(&migration.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonSnapshotTaken,
		fmt.Sprintf("ConfigMap %s was copied into snapshot %s; the history is read next", resolved.revision.Name, resolved.snapshot.Name))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.retireMigrationSnapshots(ctx, migration)
	ctrl.LoggerFrom(ctx).Info("ConfigMap source read", "configMap", resolved.revision.Name,
		"snapshot", resolved.snapshot.Name, "contentDigest", resolved.snapshot.ContentDigest)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// retireMigrationSnapshots deletes the migration's snapshots its stored status
// no longer names: neither the artifact nor the active operation. It runs
// after every resolution is stored, as retireSchemaSnapshots does for a
// schema.
func (r *MigrationReconciler) retireMigrationSnapshots(ctx context.Context, migration *operatorv1alpha1.PtahMigration) {
	owner := sourcesnapshot.Owner{Kind: "PtahMigration", Name: migration.Name, UID: migration.UID}
	keep := snapshotNames(migration.Status.Artifact)
	if operation := migration.Status.ActiveOperation; operation != nil {
		keep = append(keep, snapshotNames(operation.Source)...)
	}
	if err := retireSnapshots(ctx, r.directReader(), r.Client, migration.Namespace, owner, sourcesnapshot.LabelMigration, keep...); err != nil {
		r.event(migration, corev1.EventTypeWarning, "SourceSnapshotRetirementFailed", "%s", bounded(err.Error(), 512))
	}
}
