package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The record of a run nobody accounted for lives in status, where the manager
// alone writes it, and in a copy on the resource's own metadata, where a
// restore keeps it.
//
// Status is the record every decision reads. The copy exists for the one
// event that loses status without losing the object: a restore that recreates
// the resource from a backup and drops status on the way, which is what Velero
// does unless it is told otherwise. Without the copy, the restored resource
// would read its history, find the migration the run may already have
// executed still pending, and plan it again.
//
// The two are written in an order that makes every gap between them safe to
// stop in. The copy goes on before the status record and comes off after it,
// so a stored status record always has its copy, and a copy with no status
// record is either one a restore brought back or one whose removal was
// interrupted. status.resolvedRun tells those two apart: it names the run it
// settled, and a copy of a run status says was settled is left over.

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// encodeUnresolvedRunCopy is the annotation value for record.
func encodeUnresolvedRunCopy(record *operatorv1alpha1.UnresolvedMigrationRunStatus) (string, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode the unresolved-run record: %w", err)
	}
	return string(encoded), nil
}

// decodeUnresolvedRunCopy reads the annotation back into a record the status
// schema admits. A value that is not exactly a record -- a field this manager
// does not know, an outcome that leaves nothing unresolved, an operation ID
// that is not one -- is refused rather than half read: the copy is written by
// the manager alone, and one this manager cannot read is not one it may act
// on.
func decodeUnresolvedRunCopy(value string) (*operatorv1alpha1.UnresolvedMigrationRunStatus, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(value)))
	decoder.DisallowUnknownFields()
	record := &operatorv1alpha1.UnresolvedMigrationRunStatus{}
	if err := decoder.Decode(record); err != nil {
		return nil, fmt.Errorf("the value is not an unresolved-run record: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("the value carries more than one record")
	}
	switch {
	case record.Outcome != operatorv1alpha1.MigrationRunOutcomePartial &&
		record.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown:
		return nil, fmt.Errorf("outcome %q leaves nothing unresolved", record.Outcome)
	case !sha256Digest.MatchString(record.OperationID):
		return nil, errors.New("operationID is not an operation ID")
	case strings.TrimSpace(record.PlanRef.Name) == "" || strings.TrimSpace(string(record.PlanRef.UID)) == "":
		return nil, errors.New("planRef names no plan")
	case record.TargetIdentityDigest != "" && !sha256Digest.MatchString(record.TargetIdentityDigest):
		return nil, errors.New("targetIdentityDigest is not a digest")
	case len(record.JobName) > 253:
		return nil, errors.New("jobName is longer than a Job name")
	case record.RecordedAt.IsZero():
		return nil, errors.New("recordedAt is missing")
	case record.DispatchedBy != nil && !validManagerRecord(record.DispatchedBy):
		return nil, errors.New("dispatchedBy is not a manager record")
	}
	return record, nil
}

var digestPinnedImage = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

func validManagerRecord(record *operatorv1alpha1.ManagerRecord) bool {
	revision := record.ControllerRevision
	return digestPinnedImage.MatchString(record.ControllerImage) && len(record.ControllerImage) <= 512 &&
		digestPinnedImage.MatchString(record.RunnerImage) && len(record.RunnerImage) <= 512 &&
		revision != "" && len(revision) <= 128 && strings.TrimSpace(revision) == revision
}

// unresolvedRunCopyMatches reports that the copy on migration is exactly
// record.
func unresolvedRunCopyMatches(
	migration *operatorv1alpha1.PtahMigration,
	record *operatorv1alpha1.UnresolvedMigrationRunStatus,
) bool {
	value, present := migration.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]
	if !present {
		return false
	}
	stored, err := decodeUnresolvedRunCopy(value)
	return err == nil && equality.Semantic.DeepEqual(stored, record)
}

// setUnresolvedRunCopy writes value into the copy, or removes it when value is
// nil, with a metadata patch on the main resource. The patch goes through a
// copy of migration and hands back only the new resourceVersion and
// annotations: the API server answers a main-resource patch with the status
// it holds, and the status the caller is about to write is still in memory.
func (r *MigrationReconciler) setUnresolvedRunCopy(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	value *string,
) error {
	current, present := migration.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]
	if (value == nil && !present) || (value != nil && present && current == *value) {
		return nil
	}
	base := migration.DeepCopy()
	patched := migration.DeepCopy()
	if value == nil {
		delete(patched.Annotations, operatorv1alpha1.UnresolvedRunAnnotation)
	} else {
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		patched.Annotations[operatorv1alpha1.UnresolvedRunAnnotation] = *value
	}
	if err := r.Client.Patch(ctx, patched, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if value == nil {
			return fmt.Errorf("remove the copy of a settled unresolved run: %w", err)
		}
		return fmt.Errorf("write the copy of the unresolved run: %w", err)
	}
	migration.ResourceVersion = patched.ResourceVersion
	migration.Annotations = patched.Annotations
	return nil
}

// copyUnresolvedRunBeforeStatus writes the copy of the record after is about
// to store, ahead of the status write that stores it, and returns the base the
// status patch has to be computed from: the metadata patch moved the
// resourceVersion the optimistic lock compares.
func (r *MigrationReconciler) copyUnresolvedRunBeforeStatus(
	ctx context.Context,
	before, after *operatorv1alpha1.PtahMigration,
) (*operatorv1alpha1.PtahMigration, error) {
	record := after.Status.UnresolvedRun
	if record == nil || unresolvedRunCopyMatches(after, record) {
		return before, nil
	}
	value, err := encodeUnresolvedRunCopy(record)
	if err != nil {
		return nil, err
	}
	if err := r.setUnresolvedRunCopy(ctx, after, &value); err != nil {
		return nil, err
	}
	base := before.DeepCopy()
	base.ResourceVersion = after.ResourceVersion
	base.Annotations = after.Annotations
	return base, nil
}

// dropSettledUnresolvedRunCopy removes the copy once the status write that
// accounts for its run has landed: a resolution that names it, or the write
// that retired the claim it names without recording that claim unresolved.
// A copy of any other run is left for reconcileUnresolvedRunCopy, which is
// where a restored record is told from a stale one.
func (r *MigrationReconciler) dropSettledUnresolvedRunCopy(
	ctx context.Context,
	before, after *operatorv1alpha1.PtahMigration,
) error {
	value, present := after.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]
	if !present || after.Status.UnresolvedRun != nil {
		return nil
	}
	stored, err := decodeUnresolvedRunCopy(value)
	if err != nil {
		return nil
	}
	settled := after.Status.ResolvedRun != nil && after.Status.ResolvedRun.OperationID == stored.OperationID
	retired := before.Status.ActiveOperation != nil && before.Status.ActiveOperation.ID == stored.OperationID &&
		after.Status.ActiveOperation == nil
	if !settled && !retired {
		return nil
	}
	return r.setUnresolvedRunCopy(ctx, after, nil)
}

// reconcileUnresolvedRunCopy holds the copy and the record to each other at
// the start of a pass, and reports whether it wrote status. A pass that only
// rewrote or removed the copy goes on: the copy is metadata, and nothing
// below reads it.
//
// Status is the authority whenever it carries a record: a copy that is
// missing or differs is rewritten from it. A copy with no record in status is
// one of three things. A copy of a run status.resolvedRun says was settled is
// left over from an interrupted removal, and goes. A copy of the claim status
// still carries is a record whose status write did not land, and the claim
// will write it again. Anything else is a record status lost, which is what a
// restore without status looks like, and the record is put back.
//
// A copy this manager cannot read latches the resource as firmly as one it
// can: nothing is planned while it stands, and only the manager may remove it.
func (r *MigrationReconciler) reconcileUnresolvedRunCopy(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (bool, error) {
	value, present := migration.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]
	record := migration.Status.UnresolvedRun
	if record != nil {
		if unresolvedRunCopyMatches(migration, record) {
			return false, nil
		}
		encoded, err := encodeUnresolvedRunCopy(record)
		if err != nil {
			return false, err
		}
		return false, r.setUnresolvedRunCopy(ctx, migration, &encoded)
	}
	if !present {
		return false, nil
	}
	stored, err := decodeUnresolvedRunCopy(value)
	if err != nil {
		_, blockedErr := r.migrationBlocked(ctx, migration, operatorv1alpha1.ReasonApplyOutcomeUnknown,
			fmt.Sprintf("The %s annotation records a run nobody accounted for, and this manager cannot read it (%s); "+
				"nothing runs while it stands", operatorv1alpha1.UnresolvedRunAnnotation, bounded(err.Error(), 256)))
		return true, blockedErr
	}
	if resolved := migration.Status.ResolvedRun; resolved != nil && resolved.OperationID == stored.OperationID {
		return false, r.setUnresolvedRunCopy(ctx, migration, nil)
	}
	if operation := migration.Status.ActiveOperation; operation != nil && operation.ID == stored.OperationID {
		return false, nil
	}
	before := migration.DeepCopy()
	migration.Status.UnresolvedRun = stored
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	message := fmt.Sprintf("A %s run nobody accounted for was restored from the %s annotation; "+
		"establish what it did before another Apply", stored.Outcome, operatorv1alpha1.UnresolvedRunAnnotation)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "What the recorded run did is unknown until the database is read")
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "The recorded run may not be retried")
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return true, err
	}
	r.event(migration, corev1.EventTypeWarning, "UnresolvedRunRestored",
		"Restored the record of a %s run nobody accounted for from metadata: operation %s, plan %s",
		stored.Outcome, stored.OperationID, stored.PlanRef.Name)
	return true, nil
}

// acknowledgmentJudged reports an acknowledgment the controller already
// answered.
func acknowledgmentJudged(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) bool {
	return meta.IsStatusConditionTrue(acknowledgment.Status.Conditions, operatorv1alpha1.ConditionAcknowledgmentConsumed) ||
		meta.IsStatusConditionTrue(acknowledgment.Status.Conditions, operatorv1alpha1.ConditionAcknowledgmentStale)
}

// acknowledgmentStamped reports an acknowledgment that carries the identity
// admission stamped on it.
func acknowledgmentStamped(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) bool {
	return strings.TrimSpace(acknowledgment.Spec.AcknowledgedBy.Username) != "" &&
		strings.TrimSpace(acknowledgment.Spec.MutationRequestUID) != "" &&
		!acknowledgment.Spec.AcknowledgedAt.IsZero()
}

// reconcileRunAcknowledgments answers every acknowledgment written for this
// migration, and settles the unresolved run with the one that names it.
//
// It runs only between operations. An acknowledgment is a person's statement
// that the database is accounted for, and the database is read again after it
// before anything is planned, so settling one never has to interrupt a claim.
//
// The candidates come from the cache, and the one acted on is read again from
// the API server: the cache is fast enough to answer "is there one", and the
// decision to lift the latch is not one a cache may make.
func (r *MigrationReconciler) reconcileRunAcknowledgments(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	acknowledgments := &operatorv1alpha1.PtahMigrationRunAcknowledgmentList{}
	if err := r.Client.List(ctx, acknowledgments, client.InNamespace(migration.Namespace)); err != nil {
		return fmt.Errorf("list run acknowledgments: %w", err)
	}
	items := acknowledgments.Items
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreationTimestamp.Before(&items[j].CreationTimestamp)
	})
	unresolved := migration.Status.UnresolvedRun
	resolved := migration.Status.ResolvedRun
	var candidate *operatorv1alpha1.PtahMigrationRunAcknowledgment
	for index := range items {
		acknowledgment := &items[index]
		if acknowledgment.Spec.MigrationRef.UID != migration.UID || acknowledgment.DeletionTimestamp != nil ||
			acknowledgmentJudged(acknowledgment) {
			continue
		}
		switch {
		case unresolved != nil && acknowledgmentStamped(acknowledgment) &&
			acknowledgment.Spec.OperationID == unresolved.OperationID:
			// The oldest settles the run. A second acknowledgment of the same
			// run is answered on the pass after, by the resolution the first
			// one wrote.
			if candidate == nil {
				candidate = acknowledgment
			}
		case resolved != nil && resolved.AcknowledgmentRef != nil && resolved.AcknowledgmentRef.UID == acknowledgment.UID:
			// The resolution landed and the pass that wrote it stopped before
			// answering the acknowledgment that made it.
			if err := r.judgeAcknowledgment(ctx, acknowledgment, true, ""); err != nil {
				return err
			}
		default:
			if err := r.judgeAcknowledgment(ctx, acknowledgment, false, staleAcknowledgmentMessage(migration, acknowledgment)); err != nil {
				return err
			}
		}
	}
	if candidate == nil {
		return nil
	}
	return r.settleUnresolvedRunByAcknowledgment(ctx, migration, candidate)
}

func staleAcknowledgmentMessage(
	migration *operatorv1alpha1.PtahMigration,
	acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment,
) string {
	switch {
	case !acknowledgmentStamped(acknowledgment):
		return "The acknowledgment carries no identity stamped by admission, so it settles nothing"
	case migration.Status.UnresolvedRun != nil:
		return fmt.Sprintf("The migration is waiting on operation %s, not the one this acknowledgment names",
			migration.Status.UnresolvedRun.OperationID)
	case migration.Status.ResolvedRun != nil && migration.Status.ResolvedRun.OperationID == acknowledgment.Spec.OperationID:
		return fmt.Sprintf("The run was already settled (%s)", migration.Status.ResolvedRun.Resolution)
	default:
		return "The migration records no unresolved run for this acknowledgment to settle"
	}
}

// settleUnresolvedRunByAcknowledgment lifts the latch in the name of the
// person who acknowledged it, and asks for a fresh reading of the database
// before anything is planned.
//
// Three writes, in the order a stop between any two of them survives: the
// resolution, which names the acknowledgment and the identity on it; the copy
// coming off, which the status write drives; and the acknowledgment's own
// answer, which the next pass gives from the resolution if this one stops
// first.
func (r *MigrationReconciler) settleUnresolvedRunByAcknowledgment(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	candidate *operatorv1alpha1.PtahMigrationRunAcknowledgment,
) error {
	acknowledgment := &operatorv1alpha1.PtahMigrationRunAcknowledgment{}
	key := types.NamespacedName{Namespace: candidate.Namespace, Name: candidate.Name}
	if err := r.directReader().Get(ctx, key, acknowledgment); err != nil {
		return client.IgnoreNotFound(err)
	}
	if acknowledgment.UID != candidate.UID || acknowledgment.DeletionTimestamp != nil ||
		!equality.Semantic.DeepEqual(acknowledgment.Spec, candidate.Spec) || acknowledgmentJudged(acknowledgment) {
		return nil
	}
	unresolved := migration.Status.UnresolvedRun
	identity := acknowledgment.Spec.AcknowledgedBy
	before := migration.DeepCopy()
	now := metav1.NewTime(r.now())
	migration.Status.UnresolvedRun = nil
	migration.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
		OperationID:       unresolved.OperationID,
		Outcome:           unresolved.Outcome,
		Resolution:        operatorv1alpha1.MigrationRunResolvedByAcknowledgment,
		AcknowledgmentRef: &operatorv1alpha1.ImmutableObjectReference{Name: acknowledgment.Name, UID: acknowledgment.UID},
		AcknowledgedBy:    identity.DeepCopy(),
		ResolvedAt:        now,
	}
	// Due now, so the pass that follows starts the evidence chain again from
	// resolution. The acknowledgment says the database was accounted for; it
	// does not say what the database now holds, and only a reading does.
	migration.Status.NextReconciliationTime = &now
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse,
		operatorv1alpha1.ReasonRunAcknowledged,
		fmt.Sprintf("%s acknowledged the %s run; the database is read again before anything is planned",
			bounded(identity.Username, 256), unresolved.Outcome))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return err
	}
	r.event(migration, corev1.EventTypeNormal, "UnresolvedRunAcknowledged",
		"%s acknowledged the %s run of operation %s with %s",
		bounded(identity.Username, 256), unresolved.Outcome, unresolved.OperationID, acknowledgment.Name)
	return r.judgeAcknowledgment(ctx, acknowledgment, true, "")
}

// judgeAcknowledgment records the controller's answer on the acknowledgment.
func (r *MigrationReconciler) judgeAcknowledgment(
	ctx context.Context,
	acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment,
	consumed bool,
	message string,
) error {
	before := acknowledgment.DeepCopy()
	if consumed {
		meta.SetStatusCondition(&acknowledgment.Status.Conditions, metav1.Condition{
			Type:               operatorv1alpha1.ConditionAcknowledgmentConsumed,
			Status:             metav1.ConditionTrue,
			Reason:             string(operatorv1alpha1.ReasonRunAcknowledged),
			Message:            "The acknowledgment settled the unresolved run it names",
			ObservedGeneration: acknowledgment.Generation,
			LastTransitionTime: metav1.NewTime(r.now()),
		})
	} else {
		meta.SetStatusCondition(&acknowledgment.Status.Conditions, metav1.Condition{
			Type:               operatorv1alpha1.ConditionAcknowledgmentStale,
			Status:             metav1.ConditionTrue,
			Reason:             string(operatorv1alpha1.ReasonRunNotUnresolved),
			Message:            bounded(message, 1024),
			ObservedGeneration: acknowledgment.Generation,
			LastTransitionTime: metav1.NewTime(r.now()),
		})
	}
	acknowledgment.Status.ObservedGeneration = acknowledgment.Generation
	if err := r.Client.Status().Patch(ctx, acknowledgment,
		client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("record the answer to run acknowledgment %s: %w", acknowledgment.Name, err)
	}
	return nil
}

// migrationForAcknowledgment names the migration an acknowledgment was
// written for. Which acknowledgment counts is decided in the reconcile, by
// UID and operation ID, so one that names the wrong run wakes a migration
// that then only answers it.
func migrationForAcknowledgment(_ context.Context, object client.Object) []reconcile.Request {
	acknowledgment, ok := object.(*operatorv1alpha1.PtahMigrationRunAcknowledgment)
	if !ok || acknowledgment.Spec.MigrationRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: acknowledgment.Namespace,
		Name:      acknowledgment.Spec.MigrationRef.Name,
	}}}
}
