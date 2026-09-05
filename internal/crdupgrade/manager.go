package crdupgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

var errPreflightComplete = errors.New("CRD preflight complete")

// Client is the exact cluster-scoped API surface needed to manage the fixed
// Ptah CRD set.
type Client interface {
	Get(context.Context, string, metav1.GetOptions) (*apiextensionsv1.CustomResourceDefinition, error)
	Update(context.Context, *apiextensionsv1.CustomResourceDefinition, metav1.UpdateOptions) (*apiextensionsv1.CustomResourceDefinition, error)
}

// Manager upgrades and verifies the embedded CRD set without ever creating or
// deleting a CRD.
type Manager struct {
	Client       Client
	PollInterval time.Duration
}

// New returns a Manager backed by the apiextensions clientset.
func New(client apiextensionsclientv1.CustomResourceDefinitionInterface) *Manager {
	return &Manager{Client: client, PollInterval: 500 * time.Millisecond}
}

// ReconcileWithStatePreflight rejects a controller downgrade before making
// any CRD API mutation, then reconciles the candidate schemas.
func (m *Manager) ReconcileWithStatePreflight(ctx context.Context, state StoredControllerStateClients, supported int64) error {
	return m.ReconcileWithStatePreflightAndPrepare(ctx, state, supported, nil)
}

// PreflightWithState performs the complete stored-state, compatibility,
// re-read, and server-side dry-run sequence without any persistent mutation.
// Helm runs it before creating the append-only rollout guards.
func (m *Manager) PreflightWithState(ctx context.Context, state StoredControllerStateClients, supported int64) error {
	err := m.ReconcileWithStatePreflightAndPrepare(ctx, state, supported, func(context.Context) error {
		return errPreflightComplete
	})
	if errors.Is(err, errPreflightComplete) {
		return nil
	}
	return err
}

// ReconcileWithStatePreflightAndPrepare performs every CRD and stored-state
// check, including server-side dry runs, before invoking prepare immediately
// ahead of the first real CRD update. The Helm hook uses prepare to establish
// the persistent rollout ratchet, adopt legacy admission metadata, and stop
// old runtime Pods without weakening the mandatory state preflight.
func (m *Manager) ReconcileWithStatePreflightAndPrepare(
	ctx context.Context,
	state StoredControllerStateClients,
	supported int64,
	prepare func(context.Context) error,
) error {
	if m == nil {
		return fmt.Errorf("CRD manager is required")
	}
	if err := m.validate(); err != nil {
		return err
	}
	if supported != int64(controllerstate.CurrentVersion) {
		return fmt.Errorf(
			"supported controller-state version %d does not match compiled version %d",
			supported,
			controllerstate.CurrentVersion,
		)
	}
	if err := VerifyStoredControllerState(ctx, state, supported); err != nil {
		return fmt.Errorf("preflight stored controller state before CRD update: %w", err)
	}
	return m.reconcile(ctx, func() error {
		if err := VerifyStoredControllerState(ctx, state, supported); err != nil {
			return fmt.Errorf("repeat stored controller-state preflight before release cutover: %w", err)
		}
		if prepare != nil {
			if err := prepare(ctx); err != nil {
				return fmt.Errorf("prepare release cutover: %w", err)
			}
		}
		// The Helm cutover stops every old runtime Pod in prepare. Re-read
		// durable state only after that writer is gone so a last successful old
		// reconciliation cannot hide a controller downgrade between the
		// preflight snapshot and the first CRD update.
		if err := VerifyStoredControllerState(ctx, state, supported); err != nil {
			return fmt.Errorf("final stored controller-state preflight after release cutover: %w", err)
		}
		return nil
	})
}

// reconcile exists only as the package-private implementation behind the
// mandatory state-preflight entrypoint. Package tests may omit the preflight;
// production callers cannot accidentally bypass it.
func (m *Manager) reconcile(ctx context.Context, beforeUpdate func() error) error {
	candidates, err := Candidates()
	if err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}

	existingByName := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(candidates))
	for _, candidate := range candidates {
		existing, getErr := m.getRequired(ctx, candidate.Name)
		if getErr != nil {
			return getErr
		}
		existingByName[candidate.Name] = existing
	}
	allowPredecessor, err := recognizedPredecessorTransition(existingByName, candidates)
	if err != nil {
		return fmt.Errorf("recognize legacy CRD set: %w", err)
	}

	updates := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(candidates))
	for _, candidate := range candidates {
		existing := existingByName[candidate.Name]
		matches, matchErr := sameSpec(existing, candidate)
		if matchErr != nil {
			return fmt.Errorf("compare CRD %s: %w", candidate.Name, matchErr)
		}
		if compatibilityErr := compatible(existing, candidate, matches, allowPredecessor); compatibilityErr != nil {
			return fmt.Errorf("CRD %s is not upgrade-compatible: %w", candidate.Name, compatibilityErr)
		}
		identityMatches, identityErr := sameSchemaIdentity(existing, candidate)
		if identityErr != nil {
			return fmt.Errorf("compare CRD %s schema identity: %w", candidate.Name, identityErr)
		}
		if matches && identityMatches {
			continue
		}
		update := existing.DeepCopy()
		candidate.Spec.DeepCopyInto(&update.Spec)
		copySchemaIdentity(update, candidate)
		updates = append(updates, update)
	}

	for _, update := range updates {
		if _, err := m.Client.Update(ctx, update.DeepCopy(), metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return fmt.Errorf("dry-run CRD update %s: %w", update.Name, err)
		}
	}
	// Re-read the complete fixed set after the dry-runs. No CRD may begin its
	// real update after a different CRD acquired a newer or colliding identity.
	for _, candidate := range candidates {
		current, getErr := m.getRequired(ctx, candidate.Name)
		if getErr != nil {
			return getErr
		}
		matches, matchErr := sameSpec(current, candidate)
		if matchErr != nil {
			return fmt.Errorf("compare CRD %s after dry-run: %w", candidate.Name, matchErr)
		}
		if compatibilityErr := compatible(current, candidate, matches, allowPredecessor); compatibilityErr != nil {
			return fmt.Errorf("CRD %s changed incompatibly after dry-run: %w", candidate.Name, compatibilityErr)
		}
	}
	if beforeUpdate != nil {
		if err := beforeUpdate(); err != nil {
			return err
		}
	}
	for _, update := range updates {
		name := update.Name
		candidate := candidateNamed(candidates, name)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, getErr := m.getRequired(ctx, name)
			if getErr != nil {
				return getErr
			}
			currentMatches, matchErr := sameSpec(current, candidate)
			if matchErr != nil {
				return fmt.Errorf("compare CRD %s during update: %w", name, matchErr)
			}
			if compatibilityErr := compatible(current, candidate, currentMatches, allowPredecessor); compatibilityErr != nil {
				return fmt.Errorf("CRD %s changed incompatibly during upgrade: %w", name, compatibilityErr)
			}
			identityMatches, identityErr := sameSchemaIdentity(current, candidate)
			if identityErr != nil {
				return fmt.Errorf("compare CRD %s schema identity during update: %w", name, identityErr)
			}
			if currentMatches && identityMatches {
				return nil
			}
			candidate.Spec.DeepCopyInto(&current.Spec)
			copySchemaIdentity(current, candidate)
			_, updateErr := m.Client.Update(ctx, current, metav1.UpdateOptions{})
			return updateErr
		}); err != nil {
			return fmt.Errorf("update CRD %s: %w", name, err)
		}
	}

	return m.waitReady(ctx, candidates)
}

func incompleteSchemaIdentityError(name string) error {
	return fmt.Errorf(
		"CRD %s has an incomplete owned schema identity (%s and %s must be a pair) and its schema differs from the candidate; refusing unknown legacy schema mutation without offline migration",
		name,
		SchemaVersionAnnotation,
		SchemaDigestAnnotation,
	)
}

// Verify requires every embedded CRD to exist, match the candidate spec, and
// report Established=True and NamesAccepted=True.
func (m *Manager) Verify(ctx context.Context) error {
	candidates, err := Candidates()
	if err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	for _, candidate := range candidates {
		existing, getErr := m.getRequired(ctx, candidate.Name)
		if getErr != nil {
			return getErr
		}
		matches, matchErr := sameSpec(existing, candidate)
		if matchErr != nil {
			return fmt.Errorf("compare CRD %s: %w", candidate.Name, matchErr)
		}
		if !matches {
			return fmt.Errorf("CRD %s does not match the candidate schema", candidate.Name)
		}
		identityMatches, identityErr := sameSchemaIdentity(existing, candidate)
		if identityErr != nil {
			return fmt.Errorf("compare CRD %s schema identity: %w", candidate.Name, identityErr)
		}
		if !identityMatches {
			return fmt.Errorf("CRD %s does not match the candidate schema identity", candidate.Name)
		}
	}
	return m.waitReady(ctx, candidates)
}

func (m *Manager) validate() error {
	if m.Client == nil {
		return fmt.Errorf("CRD client is required")
	}
	if m.PollInterval <= 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	return nil
}

func (m *Manager) getRequired(ctx context.Context, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	crd, err := m.Client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("required CRD %s is missing; refusing to recreate it", name)
	}
	if err != nil {
		return nil, fmt.Errorf("get required CRD %s: %w", name, err)
	}
	return crd, nil
}

func (m *Manager) waitReady(ctx context.Context, candidates []*apiextensionsv1.CustomResourceDefinition) error {
	return wait.PollUntilContextCancel(ctx, m.PollInterval, true, func(pollCtx context.Context) (bool, error) {
		for _, candidate := range candidates {
			existing, err := m.getRequired(pollCtx, candidate.Name)
			if err != nil {
				return false, err
			}
			matches, matchErr := sameSpec(existing, candidate)
			if matchErr != nil {
				return false, fmt.Errorf("compare CRD %s after update: %w", candidate.Name, matchErr)
			}
			if !matches {
				return false, fmt.Errorf("CRD %s changed after the candidate update", candidate.Name)
			}
			identityMatches, identityErr := sameSchemaIdentity(existing, candidate)
			if identityErr != nil {
				return false, fmt.Errorf("compare CRD %s schema identity after update: %w", candidate.Name, identityErr)
			}
			if !identityMatches {
				return false, fmt.Errorf("CRD %s schema identity changed after the candidate update", candidate.Name)
			}
			ready, readyErr := established(existing)
			if readyErr != nil {
				return false, readyErr
			}
			if !ready {
				return false, nil
			}
		}
		return true, nil
	})
}

func compatible(existing, candidate *apiextensionsv1.CustomResourceDefinition, specsMatch, allowPredecessor bool) error {
	if existing.DeletionTimestamp != nil {
		return fmt.Errorf("deletion is already in progress")
	}
	if existing.Spec.Group != candidate.Spec.Group || existing.Spec.Scope != candidate.Spec.Scope ||
		existing.Spec.Names.Plural != candidate.Spec.Names.Plural ||
		existing.Spec.Names.Kind != candidate.Spec.Names.Kind {
		return fmt.Errorf("immutable API identity differs from the candidate")
	}
	candidateVersions := make(map[string]struct{}, len(candidate.Spec.Versions))
	for _, version := range candidate.Spec.Versions {
		candidateVersions[version.Name] = struct{}{}
	}
	for _, storedVersion := range existing.Status.StoredVersions {
		if _, retained := candidateVersions[storedVersion]; !retained {
			return fmt.Errorf("stored version %q is absent from the candidate", storedVersion)
		}
	}
	existingSchemaVersion, err := schemaVersion(existing, true)
	if err != nil {
		return err
	}
	candidateSchemaVersion, err := schemaVersion(candidate, false)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	if existingSchemaVersion > candidateSchemaVersion {
		return fmt.Errorf(
			"schema rollback refused: existing %s=%d is newer than candidate version %d",
			SchemaVersionAnnotation,
			existingSchemaVersion,
			candidateSchemaVersion,
		)
	}
	existingStateVersion, err := controllerStateVersion(existing, true)
	if err != nil {
		return err
	}
	candidateStateVersion, err := controllerStateVersion(candidate, false)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	if existingStateVersion > candidateStateVersion {
		return fmt.Errorf(
			"controller-state rollback refused: existing %s=%d is newer than candidate version %d",
			ControllerStateVersionAnnotation,
			existingStateVersion,
			candidateStateVersion,
		)
	}
	existingDigest, err := schemaDigest(existing, true)
	if err != nil {
		return err
	}
	candidateDigest, err := schemaDigest(candidate, false)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	_, existingHasVersion := existing.Annotations[SchemaVersionAnnotation]
	_, existingHasDigest := existing.Annotations[SchemaDigestAnnotation]
	if !existingHasVersion || !existingHasDigest {
		knownPredecessor, predecessorErr := isKnownPredecessorCRD(existing)
		if predecessorErr != nil {
			return predecessorErr
		}
		if !specsMatch && !(allowPredecessor && knownPredecessor) {
			return incompleteSchemaIdentityError(candidate.Name)
		}
		return nil
	}
	if existingSchemaVersion == candidateSchemaVersion && existingDigest != candidateDigest {
		return fmt.Errorf(
			"schema identity collision refused: %s=%d is already bound to %s, candidate binds it to %s",
			SchemaVersionAnnotation,
			existingSchemaVersion,
			existingDigest,
			candidateDigest,
		)
	}
	return nil
}

func schemaVersion(crd *apiextensionsv1.CustomResourceDefinition, allowMissing bool) (uint64, error) {
	return positiveVersionAnnotation(crd, SchemaVersionAnnotation, allowMissing)
}

func controllerStateVersion(crd *apiextensionsv1.CustomResourceDefinition, allowMissing bool) (uint64, error) {
	return positiveVersionAnnotation(crd, ControllerStateVersionAnnotation, allowMissing)
}

func positiveVersionAnnotation(crd *apiextensionsv1.CustomResourceDefinition, annotation string, allowMissing bool) (uint64, error) {
	if crd == nil {
		return 0, fmt.Errorf("CRD is required")
	}
	raw, found := crd.Annotations[annotation]
	if !found {
		if allowMissing {
			return 0, nil
		}
		return 0, fmt.Errorf("CRD %s is missing required annotation %s", crd.Name, annotation)
	}
	if raw == "" || raw[0] < '1' || raw[0] > '9' {
		return 0, fmt.Errorf("CRD %s annotation %s=%q is not a positive exact decimal version", crd.Name, annotation, raw)
	}
	for _, character := range raw[1:] {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("CRD %s annotation %s=%q is not a positive exact decimal version", crd.Name, annotation, raw)
		}
	}
	version, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("CRD %s annotation %s=%q is not a positive exact decimal version: %w", crd.Name, annotation, raw, err)
	}
	return version, nil
}

func schemaDigest(crd *apiextensionsv1.CustomResourceDefinition, allowMissing bool) (string, error) {
	if crd == nil {
		return "", fmt.Errorf("CRD is required")
	}
	raw, found := crd.Annotations[SchemaDigestAnnotation]
	if !found {
		if allowMissing {
			return "", nil
		}
		return "", fmt.Errorf("CRD %s is missing required annotation %s", crd.Name, SchemaDigestAnnotation)
	}
	if len(raw) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(raw, "sha256:") {
		return "", fmt.Errorf("CRD %s annotation %s=%q is not a lowercase SHA-256 digest", crd.Name, SchemaDigestAnnotation, raw)
	}
	for _, character := range raw[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", fmt.Errorf("CRD %s annotation %s=%q is not a lowercase SHA-256 digest", crd.Name, SchemaDigestAnnotation, raw)
		}
	}
	return raw, nil
}

func validateCandidateIdentity(candidate *apiextensionsv1.CustomResourceDefinition) error {
	if _, err := schemaVersion(candidate, false); err != nil {
		return err
	}
	annotated, err := schemaDigest(candidate, false)
	if err != nil {
		return err
	}
	computed, err := ComputeSchemaDigest(candidate)
	if err != nil {
		return err
	}
	if annotated != computed {
		return fmt.Errorf(
			"CRD %s annotation %s=%q does not match normalized spec digest %q",
			candidate.Name,
			SchemaDigestAnnotation,
			annotated,
			computed,
		)
	}
	stateVersion, err := controllerStateVersion(candidate, false)
	if err != nil {
		return err
	}
	if stateVersion != uint64(controllerstate.CurrentVersion) {
		return fmt.Errorf(
			"CRD %s annotation %s=%d does not match compiled controller-state version %d",
			candidate.Name,
			ControllerStateVersionAnnotation,
			stateVersion,
			controllerstate.CurrentVersion,
		)
	}
	return nil
}

func sameSchemaIdentity(existing, candidate *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	existingVersion, err := schemaVersion(existing, true)
	if err != nil {
		return false, err
	}
	candidateVersion, err := schemaVersion(candidate, false)
	if err != nil {
		return false, err
	}
	existingDigest, err := schemaDigest(existing, true)
	if err != nil {
		return false, err
	}
	candidateDigest, err := schemaDigest(candidate, false)
	if err != nil {
		return false, err
	}
	existingStateVersion, err := controllerStateVersion(existing, true)
	if err != nil {
		return false, err
	}
	candidateStateVersion, err := controllerStateVersion(candidate, false)
	if err != nil {
		return false, err
	}
	_, existingHasVersion := existing.Annotations[SchemaVersionAnnotation]
	_, existingHasDigest := existing.Annotations[SchemaDigestAnnotation]
	_, existingHasStateVersion := existing.Annotations[ControllerStateVersionAnnotation]
	return existingHasVersion && existingHasDigest && existingHasStateVersion &&
		existingVersion == candidateVersion && existingDigest == candidateDigest &&
		existingStateVersion == candidateStateVersion, nil
}

func copySchemaIdentity(target, candidate *apiextensionsv1.CustomResourceDefinition) {
	if target.Annotations == nil {
		target.Annotations = make(map[string]string, 3)
	}
	target.Annotations[SchemaVersionAnnotation] = candidate.Annotations[SchemaVersionAnnotation]
	target.Annotations[SchemaDigestAnnotation] = candidate.Annotations[SchemaDigestAnnotation]
	target.Annotations[ControllerStateVersionAnnotation] = candidate.Annotations[ControllerStateVersionAnnotation]
}

func recognizedPredecessorTransition(
	existingByName map[string]*apiextensionsv1.CustomResourceDefinition,
	candidates []*apiextensionsv1.CustomResourceDefinition,
) (bool, error) {
	if len(existingByName) != len(predecessorSchemaDigests) || len(candidates) != len(predecessorSchemaDigests) {
		return false, nil
	}
	predecessorCount := 0
	for _, candidate := range candidates {
		candidateSchemaVersion, err := schemaVersion(candidate, false)
		if err != nil {
			return false, err
		}
		candidateStateVersion, err := controllerStateVersion(candidate, false)
		if err != nil {
			return false, err
		}
		if candidateSchemaVersion != CurrentCRDSchemaVersion || candidateStateVersion != 1 {
			return false, nil
		}
		existing := existingByName[candidate.Name]
		if existing == nil {
			return false, nil
		}
		known, err := isKnownPredecessorCRD(existing)
		if err != nil {
			return false, err
		}
		if known {
			predecessorCount++
			continue
		}
		specsMatch, err := sameSpec(existing, candidate)
		if err != nil {
			return false, err
		}
		identityMatches, err := sameSchemaIdentity(existing, candidate)
		if err != nil {
			return false, err
		}
		if !specsMatch || !identityMatches {
			return false, nil
		}
	}
	return predecessorCount > 0, nil
}

func isKnownPredecessorCRD(crd *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	if crd == nil {
		return false, fmt.Errorf("CRD is required")
	}
	if _, found := crd.Annotations[SchemaVersionAnnotation]; found {
		return false, nil
	}
	if _, found := crd.Annotations[SchemaDigestAnnotation]; found {
		return false, nil
	}
	if _, found := crd.Annotations[ControllerStateVersionAnnotation]; found {
		return false, nil
	}
	want, found := predecessorSchemaDigests[crd.Name]
	if !found {
		return false, nil
	}
	got, err := ComputeSchemaDigest(crd)
	if err != nil {
		return false, err
	}
	return got == want, nil
}

func sameSpec(existing, candidate *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	existingJSON, err := normalizedSpec(existing)
	if err != nil {
		return false, err
	}
	candidateJSON, err := normalizedSpec(candidate)
	if err != nil {
		return false, err
	}
	return bytes.Equal(existingJSON, candidateJSON), nil
}

func normalizedSpec(crd *apiextensionsv1.CustomResourceDefinition) ([]byte, error) {
	if crd == nil {
		return nil, fmt.Errorf("CRD is required")
	}
	normalized := &apiextensionsv1.CustomResourceDefinition{Spec: *crd.Spec.DeepCopy()}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(normalized)
	encoded, err := json.Marshal(normalized.Spec)
	if err != nil {
		return nil, fmt.Errorf("encode normalized spec: %w", err)
	}
	return encoded, nil
}

// ComputeSchemaDigest returns the deterministic identity stamped into a
// generated CRD. Only the normalized spec participates in the digest.
func ComputeSchemaDigest(crd *apiextensionsv1.CustomResourceDefinition) (string, error) {
	encoded, err := normalizedSpec(crd)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum), nil
}

func established(crd *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	conditions := make(map[apiextensionsv1.CustomResourceDefinitionConditionType]apiextensionsv1.CustomResourceDefinitionCondition, len(crd.Status.Conditions))
	for _, condition := range crd.Status.Conditions {
		conditions[condition.Type] = condition
	}
	if terminating, found := conditions[apiextensionsv1.Terminating]; found && terminating.Status == apiextensionsv1.ConditionTrue {
		return false, fmt.Errorf("CRD %s is terminating: %s", crd.Name, terminating.Message)
	}
	if names, found := conditions[apiextensionsv1.NamesAccepted]; found && names.Status == apiextensionsv1.ConditionFalse {
		return false, fmt.Errorf("CRD %s names were rejected: %s", crd.Name, names.Message)
	}
	names, namesFound := conditions[apiextensionsv1.NamesAccepted]
	establishedCondition, establishedFound := conditions[apiextensionsv1.Established]
	return namesFound && names.Status == apiextensionsv1.ConditionTrue &&
		establishedFound && establishedCondition.Status == apiextensionsv1.ConditionTrue, nil
}

func candidateNamed(candidates []*apiextensionsv1.CustomResourceDefinition, name string) *apiextensionsv1.CustomResourceDefinition {
	for _, candidate := range candidates {
		if candidate.Name == name {
			return candidate
		}
	}
	panic(fmt.Sprintf("candidate %s disappeared from the validated set", name))
}
