package controller

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// unreadableRequestOfFailedMigrationRead is unreadableRequestOfFailedSchemaRead
// for a migration, whose retried claim carries its retry deadline.
func unreadableRequestOfFailedMigrationRead(migration *operatorv1alpha1.PtahMigration) (readRequest, bool) {
	operation := migration.Status.ActiveOperation
	if migrationOperation(operation).Mutating || operation.RetryNotBefore == nil {
		return readRequest{}, false
	}
	request := readRequestOf(migration.Annotations, migration.Status.LastReadRequest)
	return request, request.unreadable()
}

// failedMigrationReadGivesWay is failedSchemaReadGivesWay for a migration, under
// the rules written beside that function. It runs where a retried claim would be
// dispatched, so no Job exists under the claim's name. A retry leaves a deadline
// the migration's next reconciliation time already holds and starts one if it
// holds none, as the history read after an Apply does not, so that field is the
// interval's deadline here.
func (r *MigrationReconciler) failedMigrationReadGivesWay(ctx context.Context, migration *operatorv1alpha1.PtahMigration) (string, error) {
	operation := migration.Status.ActiveOperation
	if migrationOperation(operation).Mutating || operation.RetryNotBefore == nil || operation.JobUID != "" {
		return "", nil
	}
	resolving := operation.Type == operatorv1alpha1.MigrationOperationResolve
	if !resolving {
		moved, err := configMapSourceMoved(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.ConfigMap, migration.Status.ConfigMap)
		if err != nil {
			return "", err
		}
		if !moved {
			moved, err = sourcePolicyMoved(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact,
				migration.Status.SourcePolicy, migration.Status.Artifact != nil)
			if err != nil {
				return "", err
			}
		}
		if moved {
			return failedReadSourceMoved, nil
		}
	}
	now := r.now()
	if !due(operation.RetryNotBefore, now) {
		return "", nil
	}
	if request := readRequestOf(migration.Annotations, migration.Status.LastReadRequest); request.pending && request.instant {
		return failedReadRequested, nil
	}
	if !resolving && migration.Spec.Artifact.ConfigMap == nil && due(migration.Status.NextReconciliationTime, now) {
		return failedReadIntervalEnded, nil
	}
	return "", nil
}

// giveWayMigrationToResolve retires the failed claim. The next reconciliation
// starts the read-only chain again from resolution.
func (r *MigrationReconciler) giveWayMigrationToResolve(ctx context.Context, migration *operatorv1alpha1.PtahMigration, reason string) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	result, err := r.discardMigrationOperation(ctx, migration, telemetry.OperationStale, errors.New("the failed read gave way: "+reason))
	if err != nil {
		return result, err
	}
	r.event(migration, corev1.EventTypeNormal, "ReadRestarted",
		"The source is read again after %d failed attempts of %s: %s", operation.Attempt-1, operation.Type, reason)
	return result, nil
}
