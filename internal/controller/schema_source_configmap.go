package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// resolveConfigMapSource is Resolve for a ConfigMap source. It reads the
// ConfigMap, copies the selected files into a snapshot, records both, and
// hands the resource to observation: there is no verification policy for such
// a source, so there is no Verify step.
//
// Content that did not change keeps the plan, and another ConfigMap or other
// content retires it, as a moved OCI digest does. Snapshots nothing names any
// more are deleted once the status that stops naming them is stored.
func (r *SchemaReconciler) resolveConfigMapSource(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	owner := sourcesnapshot.Owner{Kind: "PtahSchema", Name: schema.Name, UID: schema.UID}
	resolved, err := readConfigMapSource(ctx, r.directReader(), r.Client, schema.Namespace, owner, *schema.Spec.Desired.ConfigMap, true)
	if err != nil {
		if errors.Is(err, errSourceRefused) {
			return r.configMapSourceRefused(ctx, schema, err)
		}
		return ctrl.Result{}, err
	}
	now := metav1.NewTime(r.now())
	before := schema.DeepCopy()
	previous := schema.Status.Source
	changed := previous.ConfigMap == nil || previous.Snapshot == nil ||
		previous.ConfigMap.UID != resolved.revision.UID || *previous.Snapshot != resolved.snapshot
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ConfigMap:  &resolved.revision,
		Snapshot:   &resolved.snapshot,
		ResolvedAt: &now,
	}
	if changed {
		if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.Plan = nil
		schema.Status.Applied = nil
	}
	schema.Status.ObservedGeneration = schema.Generation
	schema.Status.LastAttemptTime = &now
	clearFailure(schema)
	slot, slotted := refreshSlot(ctx, r.Client, schema.UID, interval(schema))
	schema.Status.NextReconciliationTime = claimDeadline(schema.Status.NextReconciliationTime, true, false, now.Time, interval(schema), slot, slotted)
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionTrue, operatorv1alpha1.ReasonSnapshotTaken,
		fmt.Sprintf("ConfigMap %s was copied into snapshot %s", resolved.revision.Name, resolved.snapshot.Name))
	// A ConfigMap source has no verification policy, and a condition saying
	// it passed one would read as a gate it is not.
	meta.RemoveStatusCondition(&schema.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonOperationInProgress, "Observe operation is next")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	keep := append([]string{resolved.snapshot.Name}, snapshotNames(schemaSnapshotHolders(schema)...)...)
	if err := retireSnapshots(ctx, r.directReader(), r.Client, schema.Namespace, owner, sourcesnapshot.LabelSchema, keep...); err != nil {
		// The snapshot stays until the next read retires it; nothing reads it.
		r.event(schema, corev1.EventTypeWarning, "SourceSnapshotRetirementFailed", "%s", bounded(err.Error(), 512))
	}
	ctrl.LoggerFrom(ctx).Info("ConfigMap source read", "configMap", resolved.revision.Name,
		"snapshot", resolved.snapshot.Name, "contentDigest", resolved.snapshot.ContentDigest)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// schemaSnapshotHolders is every access binding of the schema that may name a
// snapshot an operation or a proof still reads.
func schemaSnapshotHolders(schema *operatorv1alpha1.PtahSchema) []*operatorv1alpha1.SourceAccessBinding {
	holders := []*operatorv1alpha1.SourceAccessBinding{}
	if operation := schema.Status.ActiveOperation; operation != nil {
		holders = append(holders, operation.Source)
	}
	if pending := schema.Status.PendingObservation; pending != nil {
		holders = append(holders, &pending.Source)
	}
	return holders
}

// configMapSourceRefused records a ConfigMap source that gives no snapshot.
// The resource is retried at its failure interval, and sooner when the
// ConfigMap or the resource is edited. A snapshot read earlier stays as
// historical evidence, as a retained OCI digest does.
func (r *SchemaReconciler) configMapSourceRefused(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhaseFailed
	schema.Status.ObservedGeneration = schema.Generation
	now := metav1.NewTime(r.now())
	schema.Status.LastAttemptTime = &now
	next := metav1.NewTime(now.Add(failureRetry(schema)))
	schema.Status.NextReconciliationTime = &next
	if schemaSourceResolved(schema) {
		setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionUnknown, operatorv1alpha1.ReasonRefreshFailed,
			"The ConfigMap source could not be read again; the retained snapshot remains historical evidence")
	} else {
		setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionFalse, operatorv1alpha1.ReasonResolveFailed,
			"The ConfigMap source could not be read")
	}
	setFailure(schema, operatorv1alpha1.ReasonConfigurationError, failure)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonConfigurationError, "The ConfigMap source could not be read")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failureRetry(schema)}, nil
}

// planPolicyError reports a plan whose source authority no longer holds: for
// a plan computed from an OCI artifact, the verification policy object it was
// accepted under changed; for one computed from a ConfigMap, the resolved
// source is no longer the one it binds.
func (r *SchemaReconciler) planPolicyError(ctx context.Context, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) error {
	if plan.Spec.Source != nil {
		if !planSourceIsCurrent(schema, plan.Spec.ArtifactDigest, plan.Spec.Source) {
			return errors.New("the plan was computed from another source")
		}
		return nil
	}
	binding, err := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil {
		return err
	}
	if binding.UID != plan.Spec.VerificationPolicyUID || binding.Digest != plan.Spec.VerificationPolicyDigest {
		return errors.New("verification policy object changed")
	}
	return nil
}

// planSourceIsCurrent reports whether a plan's source binding is what the
// schema's source resolved to.
func planSourceIsCurrent(schema *operatorv1alpha1.PtahSchema, artifactDigest string, source *operatorv1alpha1.PlanSourceBinding) bool {
	currentArtifact, currentSource := sourcebinding.SchemaPlanSource(schema.Status.Source)
	return sourcebinding.SamePlanSource(artifactDigest, source, currentArtifact, currentSource)
}

// schemaSourceResolved reports whether the schema's source was ever read: an
// OCI digest or a ConfigMap snapshot.
func schemaSourceResolved(schema *operatorv1alpha1.PtahSchema) bool {
	return schema.Status.Source.Digest != "" || schema.Status.Source.Snapshot != nil
}

// schemaSourceReady reports whether later operations may read the schema's
// source.
func schemaSourceReady(schema *operatorv1alpha1.PtahSchema) bool {
	return sourcebinding.SchemaSourceReady(schema.Spec.Desired, schema.Status.Source)
}

// addConfigMapSourceInput binds an operation to the ConfigMap its snapshot
// was read from. An OCI source adds nothing, so its operation inputs are the
// ones a manager without ConfigMap sources computed.
func addConfigMapSourceInput(inputs map[string]any, revision *operatorv1alpha1.ConfigMapSourceRevision) {
	if revision != nil {
		inputs["source_configmap_uid"] = string(revision.UID)
	}
}

// configMapSourceMoved reports whether the source ConfigMap changed since it
// was last read: another object, another resourceVersion, or a ConfigMap that
// now exists where none was read. It reads only the metadata, from the API
// server.
func configMapSourceMoved(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	spec *operatorv1alpha1.ConfigMapSourceSpec,
	read *operatorv1alpha1.ConfigMapSourceRevision,
) (bool, error) {
	if spec == nil {
		return false, nil
	}
	current := &metav1.PartialObjectMetadata{}
	current.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: spec.Name}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return read != nil, nil
		}
		return false, fmt.Errorf("read source ConfigMap metadata: %w", err)
	}
	return read == nil || read.Name != current.Name || read.UID != current.UID || read.ResourceVersion != current.ResourceVersion, nil
}
