package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// A read-only operation that fails runs again under the same claim, and the
// claim holds what it was made from: the snapshot, commit or digest the last
// Resolve recorded. Nothing ends such a claim by itself, so a claim that keeps
// failing would hold its source for as long as it fails, while the checks a
// resource with no claim makes never run.
//
// So between two attempts a read-only claim gives way to the Resolve those
// checks would start, in the three cases where one is owed: the source moved,
// which is read at once, as it is with no claim; or, once the retry delay has
// passed, a person asked for a read, or the interval that makes a mutable tag
// or branch observable has passed.
//
// Everything else keeps its binding. A mutating claim may be running against
// the plan it was approved for, and the read that proves an Apply is bound to
// what that Apply ran from. A Job under the claim's reserved name is an attempt
// under way, however its status write fared, and it is left to finish.
//
// Giving way retires the claim and nothing else. The plan a person may be
// approving stays, and the Resolve that follows decides whether it still holds,
// as it does at any interval.
const (
	failedReadSourceMoved   = "its source changed"
	failedReadRequested     = "a read was requested"
	failedReadIntervalEnded = "the interval to read its source again has passed"
)

// failedSchemaReadGivesWay says why the failed schema's claim gives way, or ""
// when it stands. now is the instant the caller measures the retry delay by.
func (r *SchemaReconciler) failedSchemaReadGivesWay(ctx context.Context, schema *operatorv1alpha1.PtahSchema, now time.Time) (string, error) {
	operation := schema.Status.ActiveOperation
	if !isReadOnlyOperation(operation) || schema.Status.PendingObservation != nil || operation.JobUID != "" {
		return "", nil
	}
	reason, err := r.schemaFailedReadOwesAResolve(ctx, schema, now)
	if err != nil || reason == "" {
		return "", err
	}
	job := &batchv1.Job{}
	err = r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}, job)
	if err == nil {
		return "", nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read the failed read's Job: %w", err)
	}
	return reason, nil
}

func (r *SchemaReconciler) schemaFailedReadOwesAResolve(ctx context.Context, schema *operatorv1alpha1.PtahSchema, now time.Time) (string, error) {
	resolving := schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationResolve
	if !resolving {
		moved, err := configMapSourceMoved(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.ConfigMap, schema.Status.Source.ConfigMap)
		if err != nil {
			return "", err
		}
		if !moved {
			moved, err = sourcePolicyMoved(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired,
				schema.Status.Source.SourcePolicy, schemaSourceResolved(schema))
			if err != nil {
				return "", err
			}
		}
		if moved {
			return failedReadSourceMoved, nil
		}
	}
	if !due(schema.Status.NextReconciliationTime, now) {
		return "", nil
	}
	if request := readRequestOf(schema.Annotations, schema.Status.LastReadRequest); request.pending && request.instant {
		return failedReadRequested, nil
	}
	if !resolving && schema.Spec.Desired.ConfigMap == nil && intervalPassed(schema.Status.Source.ResolvedAt, interval(schema), now) {
		return failedReadIntervalEnded, nil
	}
	return "", nil
}

// intervalPassed reports that a source resolved at resolvedAt is due to be read
// again. A retry overwrites a schema's next reconciliation time with its own
// deadline, so this is measured from the reading rather than from that field.
func intervalPassed(resolvedAt *metav1.Time, period time.Duration, now time.Time) bool {
	return resolvedAt != nil && !now.Before(resolvedAt.Add(period))
}

// giveWayToResolve retires the failed claim. Pending is the phase every pass
// starts a fresh Resolve from, and the failure the last attempt recorded stays
// on the conditions until a later operation replaces it.
func (r *SchemaReconciler) giveWayToResolve(ctx context.Context, schema *operatorv1alpha1.PtahSchema, reason string) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhasePending
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationStale)
	r.event(schema, corev1.EventTypeNormal, "ReadRestarted",
		"The source is read again after %d failed attempts of %s: %s", operation.Attempt-1, operation.Type, reason)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}
