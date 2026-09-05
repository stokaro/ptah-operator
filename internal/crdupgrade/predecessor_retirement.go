package crdupgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// PredecessorRetirementInventoryVersion identifies both the sealed marker
	// schema and the semantic projection used to digest retained objects. A
	// future projection must use a new version without redefining existing
	// sealed markers.
	PredecessorRetirementInventoryVersion = "1"

	// PredecessorRetirementInventoryVersionAnnotation is present on both the
	// unsealed and sealed forms of every sequence-scoped convergence marker.
	PredecessorRetirementInventoryVersionAnnotation = "operator.ptah.dev/predecessor-retirement-inventory-version"

	// PredecessorRetirementInventoryDataKey is absent from the exact unsealed
	// marker and contains canonical compact JSON in the immutable sealed form.
	PredecessorRetirementInventoryDataKey = "predecessor-retirement-inventory"

	predecessorRetirementSemanticVersion = "1"
	predecessorRetirementSealManager     = "ptah-predecessor-retirement-v1"
)

var (
	predecessorRetirementDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	predecessorCleanupIdentityPattern  = regexp.MustCompile(`^(.+)-cleanup-v([1-9][0-9]*)-([0-9a-f]{12})$`)
)

// PredecessorRetirementPolicyClient is the exact policy API surface required
// to seal and retire a release inventory.
type PredecessorRetirementPolicyClient interface {
	ValidatingAdmissionPolicyReader
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// PredecessorRetirementBindingClient is the exact binding API surface
// required to seal and retire a release inventory.
type PredecessorRetirementBindingClient interface {
	ValidatingAdmissionPolicyBindingReader
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// PredecessorRetirementConfigMapClient is the exact ConfigMap API surface
// required to seal the current marker and retire predecessor markers.
type PredecessorRetirementConfigMapClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// PredecessorRetirementProbe identifies one content-versioned dry-run denial
// that a direct API-server barrier must prove absent after deleting every
// predecessor binding.
type PredecessorRetirementProbe struct {
	PolicyName   string
	BindingName  string
	FieldManager string
	Message      string
}

// HasExactDenial reports whether err is the exact attributed denial for this
// predecessor policy and binding. A caller must not treat a generic Forbidden
// response as convergence evidence.
func (p PredecessorRetirementProbe) HasExactDenial(err error) bool {
	return hasExactValidatingAdmissionPolicyDenial(err, p.PolicyName, p.BindingName, p.Message)
}

// PredecessorRetirementBarrierTarget is the immutable input to an
// all-API-server binding-retirement proof. The barrier must re-read and verify
// the marker through each direct endpoint, issue every listed dry-run probe,
// and prove that none returns its exact predecessor denial.
type PredecessorRetirementBarrierTarget struct {
	marker *corev1.ConfigMap
	probes []PredecessorRetirementProbe
}

// MarkerName returns the exact sealed predecessor marker name.
func (t PredecessorRetirementBarrierTarget) MarkerName() string {
	if t.marker == nil {
		return ""
	}
	return t.marker.Name
}

// Marker returns a defensive copy of the exact sealed predecessor marker.
func (t PredecessorRetirementBarrierTarget) Marker() *corev1.ConfigMap {
	if t.marker == nil {
		return nil
	}
	return t.marker.DeepCopy()
}

// Probes returns the fixed candidate-pair probe inventory in canonical order.
func (t PredecessorRetirementBarrierTarget) Probes() []PredecessorRetirementProbe {
	return slices.Clone(t.probes)
}

// VerifyMarker rejects endpoint-local drift from the sealed shared-storage
// snapshot used to derive this barrier.
func (t PredecessorRetirementBarrierTarget) VerifyMarker(actual *corev1.ConfigMap) error {
	if t.marker == nil || actual == nil {
		return errors.New("predecessor retirement barrier marker is missing")
	}
	if !reflect.DeepEqual(actual, t.marker) {
		return fmt.Errorf("predecessor retirement barrier ConfigMap/%s changed", t.marker.Name)
	}
	return nil
}

// PredecessorRetirementBarrier proves binding deletion through every directly
// addressed API server before any predecessor policy can be deleted.
type PredecessorRetirementBarrier func(context.Context, PredecessorRetirementBarrierTarget) error

type predecessorRetirementInventory struct {
	Version string                                `json:"version"`
	Entries []predecessorRetirementInventoryEntry `json:"entries"`
}

type predecessorRetirementInventoryEntry struct {
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	UID    types.UID `json:"uid"`
	Digest string    `json:"digest"`
}

type predecessorSemanticMetadata struct {
	Name            string                  `json:"name"`
	Namespace       string                  `json:"namespace"`
	GenerateName    string                  `json:"generateName"`
	Labels          map[string]string       `json:"labels"`
	Annotations     map[string]string       `json:"annotations"`
	OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
	Finalizers      []string                `json:"finalizers"`
}

type predecessorPolicySemantic struct {
	Version    string                                                `json:"version"`
	APIVersion string                                                `json:"apiVersion"`
	Kind       string                                                `json:"kind"`
	Metadata   predecessorSemanticMetadata                           `json:"metadata"`
	Spec       admissionregistrationv1.ValidatingAdmissionPolicySpec `json:"spec"`
}

type predecessorBindingSemantic struct {
	Version    string                                                       `json:"version"`
	APIVersion string                                                       `json:"apiVersion"`
	Kind       string                                                       `json:"kind"`
	Metadata   predecessorSemanticMetadata                                  `json:"metadata"`
	Spec       admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec `json:"spec"`
}

type predecessorConfigMapSemantic struct {
	Version    string                      `json:"version"`
	APIVersion string                      `json:"apiVersion"`
	Kind       string                      `json:"kind"`
	Metadata   predecessorSemanticMetadata `json:"metadata"`
	Immutable  bool                        `json:"immutable"`
	Data       map[string]string           `json:"data"`
	BinaryData map[string][]byte           `json:"binaryData"`
}

func predecessorMetadata(metadata metav1.ObjectMeta) predecessorSemanticMetadata {
	return predecessorSemanticMetadata{
		Name:            metadata.Name,
		Namespace:       metadata.Namespace,
		GenerateName:    metadata.GenerateName,
		Labels:          metadata.Labels,
		Annotations:     metadata.Annotations,
		OwnerReferences: metadata.OwnerReferences,
		Finalizers:      metadata.Finalizers,
	}
}

func predecessorPolicyDigest(object *admissionregistrationv1.ValidatingAdmissionPolicy) (string, error) {
	if object == nil {
		return "", errors.New("predecessor retirement policy is nil")
	}
	return predecessorSemanticDigest(predecessorPolicySemantic{
		Version:    predecessorRetirementSemanticVersion,
		APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
		Kind:       "ValidatingAdmissionPolicy",
		Metadata:   predecessorMetadata(object.ObjectMeta),
		Spec:       object.Spec,
	})
}

func predecessorBindingDigest(object *admissionregistrationv1.ValidatingAdmissionPolicyBinding) (string, error) {
	if object == nil {
		return "", errors.New("predecessor retirement binding is nil")
	}
	return predecessorSemanticDigest(predecessorBindingSemantic{
		Version:    predecessorRetirementSemanticVersion,
		APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
		Kind:       "ValidatingAdmissionPolicyBinding",
		Metadata:   predecessorMetadata(object.ObjectMeta),
		Spec:       object.Spec,
	})
}

func predecessorConfigMapDigest(object *corev1.ConfigMap) (string, error) {
	if object == nil {
		return "", errors.New("predecessor retirement ConfigMap is nil")
	}
	return predecessorSemanticDigest(predecessorConfigMapSemantic{
		Version:    predecessorRetirementSemanticVersion,
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata:   predecessorMetadata(object.ObjectMeta),
		Immutable:  object.Immutable != nil && *object.Immutable,
		Data:       object.Data,
		BinaryData: object.BinaryData,
	})
}

func predecessorSemanticDigest(object any) (string, error) {
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", fmt.Errorf("marshal predecessor retirement semantic projection: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest), nil
}

func encodePredecessorRetirementInventory(inventory predecessorRetirementInventory) (string, error) {
	if err := validatePredecessorRetirementInventory(inventory, nil); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		return "", fmt.Errorf("marshal predecessor retirement inventory: %w", err)
	}
	return string(encoded), nil
}

func decodePredecessorRetirementInventory(raw string, expected []predecessorRetirementInventoryEntry) (predecessorRetirementInventory, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return predecessorRetirementInventory{}, errors.New("predecessor retirement inventory is empty or padded")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var inventory predecessorRetirementInventory
	if err := decoder.Decode(&inventory); err != nil {
		return predecessorRetirementInventory{}, fmt.Errorf("decode predecessor retirement inventory: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return predecessorRetirementInventory{}, errors.New("predecessor retirement inventory has trailing JSON")
		}
		return predecessorRetirementInventory{}, fmt.Errorf("decode predecessor retirement inventory suffix: %w", err)
	}
	if err := validatePredecessorRetirementInventory(inventory, expected); err != nil {
		return predecessorRetirementInventory{}, err
	}
	canonical, err := json.Marshal(inventory)
	if err != nil {
		return predecessorRetirementInventory{}, fmt.Errorf("canonicalize predecessor retirement inventory: %w", err)
	}
	if !bytes.Equal(canonical, []byte(raw)) {
		return predecessorRetirementInventory{}, errors.New("predecessor retirement inventory is not canonical compact JSON")
	}
	return inventory, nil
}

func validatePredecessorRetirementInventory(inventory predecessorRetirementInventory, expected []predecessorRetirementInventoryEntry) error {
	if inventory.Version != PredecessorRetirementInventoryVersion {
		return fmt.Errorf("predecessor retirement inventory version %q is unsupported", inventory.Version)
	}
	if expected != nil && len(inventory.Entries) != len(expected) {
		return fmt.Errorf("predecessor retirement inventory has %d entries, want %d", len(inventory.Entries), len(expected))
	}
	seen := make(map[string]struct{}, len(inventory.Entries))
	for index, entry := range inventory.Entries {
		if entry.Kind != "ValidatingAdmissionPolicy" && entry.Kind != "ValidatingAdmissionPolicyBinding" && entry.Kind != "ConfigMap" {
			return fmt.Errorf("predecessor retirement inventory entry %d has unsupported kind %q", index, entry.Kind)
		}
		if entry.Name == "" || entry.Name != strings.TrimSpace(entry.Name) {
			return fmt.Errorf("predecessor retirement inventory entry %d has an invalid name", index)
		}
		if entry.UID == "" || string(entry.UID) != strings.TrimSpace(string(entry.UID)) || len(entry.UID) > 128 {
			return fmt.Errorf("predecessor retirement inventory entry %d has an invalid UID", index)
		}
		if !predecessorRetirementDigestPattern.MatchString(entry.Digest) {
			return fmt.Errorf("predecessor retirement inventory entry %d has an invalid semantic digest", index)
		}
		identity := entry.Kind + "\n" + entry.Name
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("predecessor retirement inventory entry %d duplicates %s/%s", index, entry.Kind, entry.Name)
		}
		seen[identity] = struct{}{}
		if expected != nil && (entry.Kind != expected[index].Kind || entry.Name != expected[index].Name) {
			return fmt.Errorf("predecessor retirement inventory entry %d is %s/%s, want %s/%s", index, entry.Kind, entry.Name, expected[index].Kind, expected[index].Name)
		}
	}
	return nil
}

// unsealedMarker returns the only marker shape Helm may create. The pinned
// hook seals this object after the complete stored-object and direct-endpoint
// proofs and before candidate activation.
func (g *AdmissionConvergenceGuard) unsealedMarker() *corev1.ConfigMap {
	name := AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence)
	metadata := g.markerMetadata(name)
	if metadata.Annotations == nil {
		metadata.Annotations = map[string]string{}
	}
	metadata.Annotations[PredecessorRetirementInventoryVersionAnnotation] = PredecessorRetirementInventoryVersion
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metadata,
		Data: map[string]string{
			admissionConvergenceExpectedDataKey: strconv.FormatInt(int64(g.ReleaseSequence), 10),
			admissionConvergenceAttemptDataKey:  hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
		},
	}
}

func (g *AdmissionConvergenceGuard) verifyUnsealedMarker(marker *corev1.ConfigMap) error {
	if err := g.verifyMarkerMetadata(marker); err != nil {
		return err
	}
	expected := g.unsealedMarker()
	if !reflect.DeepEqual(marker.Data, expected.Data) || len(marker.BinaryData) != 0 ||
		(marker.Immutable != nil && *marker.Immutable) {
		return fmt.Errorf("admission convergence ConfigMap/%s differs from the exact marker contract (unsealed form)", expected.Name)
	}
	return nil
}

func (g *AdmissionConvergenceGuard) verifySealedMarker(marker *corev1.ConfigMap) (predecessorRetirementInventory, error) {
	if err := g.verifyMarkerMetadata(marker); err != nil {
		return predecessorRetirementInventory{}, err
	}
	expectedEntries := predecessorRetirementExpectedEntries(
		g.ReleaseNamespace,
		g.ReleaseName,
		g.ReleaseSequence,
		g.ManagerImage,
	)
	expectedBase := g.unsealedMarker().Data
	exactData := len(marker.Data) == len(expectedBase)+1
	for key, value := range expectedBase {
		if marker.Data[key] != value {
			exactData = false
			break
		}
	}
	if !exactData {
		return predecessorRetirementInventory{}, fmt.Errorf("admission convergence ConfigMap/%s differs from the exact sealed marker data contract", marker.Name)
	}
	if marker.Immutable == nil || !*marker.Immutable || len(marker.BinaryData) != 0 {
		return predecessorRetirementInventory{}, fmt.Errorf("admission convergence ConfigMap/%s differs from the exact sealed marker contract", marker.Name)
	}
	inventory, err := decodePredecessorRetirementInventory(marker.Data[PredecessorRetirementInventoryDataKey], expectedEntries)
	if err != nil {
		return predecessorRetirementInventory{}, fmt.Errorf("verify admission convergence ConfigMap/%s inventory: %w", marker.Name, err)
	}
	return inventory, nil
}

func (g *AdmissionConvergenceGuard) verifyMarkerMetadata(marker *corev1.ConfigMap) error {
	expected := g.unsealedMarker()
	if marker == nil || marker.Name != expected.Name || marker.Namespace != expected.Namespace || marker.GenerateName != "" ||
		marker.UID == "" || marker.ResourceVersion == "" || marker.DeletionTimestamp != nil || marker.DeletionGracePeriodSeconds != nil ||
		!reflect.DeepEqual(marker.Annotations, expected.Annotations) || !reflect.DeepEqual(marker.Labels, expected.Labels) ||
		len(marker.OwnerReferences) != 0 || len(marker.Finalizers) != 0 {
		return fmt.Errorf("admission convergence ConfigMap/%s has foreign or incomplete ownership and differs from the exact marker contract", expected.Name)
	}
	return nil
}

func admissionConvergenceGuardFromSealedMarker(
	releaseNamespace, releaseName string,
	releaseSequence int32,
	marker *corev1.ConfigMap,
) (*AdmissionConvergenceGuard, error) {
	if marker == nil {
		return nil, errors.New("sealed predecessor admission convergence marker is nil")
	}
	managerImage := marker.Annotations[ManagerImageAnnotation]
	if !admissionConvergenceManagerImagePattern.MatchString(managerImage) {
		return nil, errors.New("sealed predecessor admission convergence marker has an invalid manager identity")
	}
	cleanup := marker.Annotations[admissionConvergenceCleanupAnnotation]
	parts := predecessorCleanupIdentityPattern.FindStringSubmatch(cleanup)
	if len(parts) != 4 {
		return nil, errors.New("sealed predecessor admission convergence marker has an invalid cleanup identity")
	}
	parsedSequence, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || int32(parsedSequence) != releaseSequence {
		return nil, errors.New("sealed predecessor admission convergence marker cleanup sequence differs")
	}
	wantDigest := hookIdentityDigest(releaseNamespace, releaseName, releaseSequence, managerImage)[:12]
	if parts[3] != wantDigest {
		return nil, errors.New("sealed predecessor admission convergence marker cleanup digest differs")
	}
	hookServiceAccount := parts[1] + "-crd-v" + parts[2] + "-" + parts[3]
	wantCleanup, err := TeardownServiceAccountName(hookServiceAccount, releaseSequence)
	if err != nil || wantCleanup != cleanup {
		return nil, errors.New("sealed predecessor admission convergence marker cleanup identity is not derivable")
	}
	return &AdmissionConvergenceGuard{
		ReleaseName:               releaseName,
		ReleaseNamespace:          releaseNamespace,
		ReleaseSequence:           releaseSequence,
		ManagerImage:              managerImage,
		HookServiceAccountName:    hookServiceAccount,
		CleanupServiceAccountName: cleanup,
	}, nil
}

type predecessorRetirementPairBlueprint struct {
	name          string
	policy        *admissionregistrationv1.ValidatingAdmissionPolicy
	binding       *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	verifyPolicy  func(*admissionregistrationv1.ValidatingAdmissionPolicy) error
	verifyBinding func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error
}

func predecessorRetirementPairBlueprints(rollout *RolloutGuard) ([]predecessorRetirementPairBlueprint, error) {
	if rollout == nil {
		return nil, errors.New("predecessor retirement rollout identity is nil")
	}
	if err := rollout.validateIdentity(); err != nil {
		return nil, fmt.Errorf("validate predecessor retirement rollout identity: %w", err)
	}

	rolloutName := RolloutGuardPolicyName(rollout.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(rollout.ReleaseSequence)
	runtimePodName := RuntimePodGuardPolicyName(rollout.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	parentReplicaSetName := ParentReplicaSetGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	parentHookContractName := ParentHookJobContractPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	serviceAccountName := ServiceAccountOriginGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	controllerWriteName := ControllerWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	runtimePodPolicy, err := rollout.runtimePodIdentityPolicy()
	if err != nil {
		return nil, fmt.Errorf("build predecessor retirement runtime Pod policy: %w", err)
	}
	runtimePodBinding, err := rollout.runtimePodIdentityBinding()
	if err != nil {
		return nil, fmt.Errorf("build predecessor retirement runtime Pod binding: %w", err)
	}

	parentEntries := NewParentWorkloadGuard(rollout).entries()
	parentByName := make(map[string]parentGuardEntry, len(parentEntries))
	for _, entry := range parentEntries {
		parentByName[entry.name] = entry
	}
	parentReplicaSet, found := parentByName[parentReplicaSetName]
	if !found {
		return nil, errors.New("predecessor retirement runtime parent contract is missing")
	}
	parentHookContract, found := parentByName[parentHookContractName]
	if !found {
		return nil, errors.New("predecessor retirement hook parent contract is missing")
	}

	serviceAccount := NewServiceAccountOriginGuard(rollout)
	serviceAccountPolicy, err := serviceAccount.policy()
	if err != nil {
		return nil, fmt.Errorf("build predecessor retirement ServiceAccount-origin policy: %w", err)
	}
	controllerWrite := NewControllerWriteGuard(rollout)
	controllerObjects := NewControllerObjectGuard(rollout)
	controllerObjectByName := make(map[string]controllerObjectGuardEntry)
	for _, entry := range controllerObjects.entries() {
		controllerObjectByName[entry.name] = entry
	}

	blueprints := []predecessorRetirementPairBlueprint{
		{
			name: rolloutName, policy: rollout.policy(rollout.ControllerStateVersion, rollout.AdmissionContractVersion), binding: rollout.binding(rolloutName),
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				_, _, verifyErr := rollout.verifyPolicy(actual)
				return verifyErr
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return rollout.verifyBinding(actual, rolloutName)
			},
		},
		{
			name: runtimeName, policy: rollout.runtimePolicy(rollout.ControllerStateVersion, rollout.ReleaseSequence, rollout.ManagerImage), binding: rollout.binding(runtimeName),
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				_, _, _, verifyErr := rollout.verifyRuntimePolicy(actual)
				return verifyErr
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return rollout.verifyBinding(actual, runtimeName)
			},
		},
		{
			name: runtimePodName, policy: runtimePodPolicy, binding: runtimePodBinding,
			verifyPolicy: rollout.verifyRuntimePodIdentityPolicy, verifyBinding: rollout.verifyRuntimePodIdentityBinding,
		},
		{
			name: hookName, policy: rollout.hookIdentityPolicy(), binding: rollout.binding(hookName),
			verifyPolicy: rollout.verifyHookIdentityPolicy,
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return rollout.verifyBinding(actual, hookName)
			},
		},
		{
			name: hookProbeName, policy: rollout.hookIdentityProbePolicy(), binding: rollout.binding(hookProbeName),
			verifyPolicy: rollout.verifyHookIdentityProbePolicy,
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return rollout.verifyBinding(actual, hookProbeName)
			},
		},
		{
			name: parentReplicaSet.name, policy: parentReplicaSet.policy, binding: parentReplicaSet.binding,
			verifyPolicy: parentReplicaSet.verifyPolicy, verifyBinding: parentReplicaSet.verifyBinding,
		},
		{
			name: parentHookContract.name, policy: parentHookContract.policy, binding: parentHookContract.binding,
			verifyPolicy: parentHookContract.verifyPolicy, verifyBinding: parentHookContract.verifyBinding,
		},
		{
			name: serviceAccountName, policy: serviceAccountPolicy, binding: serviceAccount.binding(),
			verifyPolicy: serviceAccount.verifyPolicy, verifyBinding: serviceAccount.verifyBinding,
		},
		{
			name: controllerWriteName, policy: controllerWrite.policy(), binding: controllerWrite.binding(),
			verifyPolicy: controllerWrite.verifyPolicy, verifyBinding: controllerWrite.verifyBinding,
		},
	}

	for _, name := range []string{
		ControllerJobWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ControllerChunkWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ControllerPlanWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
	} {
		entry, found := controllerObjectByName[name]
		if !found {
			return nil, fmt.Errorf("predecessor retirement controller object contract %s is missing", name)
		}
		blueprints = append(blueprints, predecessorRetirementPairBlueprint{
			name:    entry.name,
			policy:  controllerObjects.policy(entry),
			binding: controllerObjects.binding(entry),
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return controllerObjects.verifyPolicy(entry, actual)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return controllerObjects.verifyBinding(entry, actual)
			},
		})
	}

	if len(blueprints) != 12 {
		return nil, fmt.Errorf("predecessor retirement candidate pair inventory has %d entries, want 12", len(blueprints))
	}
	return blueprints, nil
}

func predecessorRetirementExpectedEntries(releaseNamespace, releaseName string, sequence int32, managerImage string) []predecessorRetirementInventoryEntry {
	pairNames := []string{
		RolloutGuardPolicyName(sequence),
		RuntimeGuardPolicyName(sequence),
		RuntimePodGuardPolicyName(sequence),
		HookIdentityGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		HookIdentityProbeGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ParentReplicaSetGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ParentHookJobContractPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ServiceAccountOriginGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ControllerWriteGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ControllerJobWriteGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ControllerChunkWriteGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
		ControllerPlanWriteGuardPolicyName(releaseNamespace, releaseName, sequence, managerImage),
	}
	entries := make([]predecessorRetirementInventoryEntry, 0, len(pairNames)*2+1)
	for _, name := range pairNames {
		entries = append(entries,
			predecessorRetirementInventoryEntry{Kind: "ValidatingAdmissionPolicy", Name: name},
			predecessorRetirementInventoryEntry{Kind: "ValidatingAdmissionPolicyBinding", Name: name},
		)
	}
	entries = append(entries, predecessorRetirementInventoryEntry{
		Kind: "ConfigMap",
		Name: HookIdentityProbeObjectName(releaseNamespace, releaseName, sequence, managerImage),
	})
	return entries
}

func predecessorHookProbeObject(rollout *RolloutGuard) *corev1.ConfigMap {
	name := HookIdentityProbeObjectName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	policyName := HookIdentityProbeGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: rollout.ReleaseNamespace,
			Annotations: map[string]string{
				"helm.sh/hook":                           "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                    hookIdentityProbeMarkerWeight,
				"helm.sh/resource-policy":                "keep",
				"operator.ptah.dev/hook-identity-policy": policyName,
			},
			Labels: map[string]string{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 rollout.ReleaseName,
				"app.kubernetes.io/component": "hook-identity-probe",
			},
		},
		Data: map[string]string{"probe": "ready-for-denial-proof"},
	}
}

// PredecessorRetirement seals the current release's exact live admission
// inventory and retires the immediately preceding sealed inventory. It never
// discovers objects by labels or prefixes.
type PredecessorRetirement struct {
	rollout    *RolloutGuard
	policies   PredecessorRetirementPolicyClient
	bindings   PredecessorRetirementBindingClient
	configMaps PredecessorRetirementConfigMapClient
}

// NewPredecessorRetirement constructs a release-scoped inventory manager. The
// same clients are used to attest, re-read, and delete objects so verification
// cannot accidentally occur through a different API identity.
func NewPredecessorRetirement(
	rollout *RolloutGuard,
	policies PredecessorRetirementPolicyClient,
	bindings PredecessorRetirementBindingClient,
	configMaps PredecessorRetirementConfigMapClient,
) *PredecessorRetirement {
	return &PredecessorRetirement{
		rollout:    rollout,
		policies:   policies,
		bindings:   bindings,
		configMaps: configMaps,
	}
}

// SealCurrent verifies all twelve candidate-scoped policy/binding pairs and
// the hook denial-probe ConfigMap against their constructors, records the live
// UID and semantic digest of each object, and makes the marker immutable. The
// caller must complete the pre-seal direct-endpoint proof first and repeat it
// against the sealed marker before activation.
func (r *PredecessorRetirement) SealCurrent(ctx context.Context) error {
	rollout, err := r.validatedRollout()
	if err != nil {
		return err
	}
	guard := NewAdmissionConvergenceGuard(rollout)
	name := AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence)
	marker, err := r.configMaps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get current admission convergence marker for sealing: %w", err)
	}
	if marker == nil {
		return fmt.Errorf("current admission convergence ConfigMap/%s is nil", name)
	}

	if marker.Immutable != nil && *marker.Immutable {
		stored, verifyErr := guard.verifySealedMarker(marker)
		if verifyErr != nil {
			return verifyErr
		}
		live, inspectErr := r.currentInventory(ctx, rollout)
		if inspectErr != nil {
			return inspectErr
		}
		if !reflect.DeepEqual(stored, live) {
			return fmt.Errorf("sealed admission convergence ConfigMap/%s inventory differs from exact live objects", name)
		}
		return nil
	}
	if err := guard.verifyUnsealedMarker(marker); err != nil {
		return err
	}

	inventory, err := r.currentInventory(ctx, rollout)
	if err != nil {
		return err
	}
	encoded, err := encodePredecessorRetirementInventory(inventory)
	if err != nil {
		return err
	}
	sealed := marker.DeepCopy()
	sealed.Data = cloneStringMap(marker.Data)
	sealed.Data[PredecessorRetirementInventoryDataKey] = encoded
	immutable := true
	sealed.Immutable = &immutable
	updated, err := r.configMaps.Update(ctx, sealed, metav1.UpdateOptions{FieldManager: predecessorRetirementSealManager})
	if err != nil {
		return fmt.Errorf("seal current admission convergence marker: %w", err)
	}
	if updated == nil {
		return fmt.Errorf("seal current admission convergence ConfigMap/%s returned nil", name)
	}
	if updated.UID != marker.UID {
		return fmt.Errorf("sealed admission convergence ConfigMap/%s changed UID", name)
	}
	stored, err := guard.verifySealedMarker(updated)
	if err != nil {
		return fmt.Errorf("verify sealed admission convergence marker response: %w", err)
	}
	if !reflect.DeepEqual(stored, inventory) {
		return fmt.Errorf("sealed admission convergence ConfigMap/%s response changed its inventory", name)
	}
	return nil
}

// VerifyCurrentSealed re-reads the exact sealed marker and all attested live
// objects. Runtime and post-seal endpoint proofs use this check to ensure no
// object changed after the inventory was captured.
func (r *PredecessorRetirement) VerifyCurrentSealed(ctx context.Context) error {
	rollout, err := r.validatedRollout()
	if err != nil {
		return err
	}
	guard := NewAdmissionConvergenceGuard(rollout)
	name := AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence)
	marker, err := r.configMaps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get sealed current admission convergence marker: %w", err)
	}
	stored, err := guard.verifySealedMarker(marker)
	if err != nil {
		return err
	}
	live, err := r.currentInventory(ctx, rollout)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, live) {
		return fmt.Errorf("sealed admission convergence ConfigMap/%s inventory differs from exact live objects", name)
	}
	return nil
}

// Preflight verifies the complete sealed predecessor inventory and accepted
// retry cursor without mutating any object. An absent predecessor marker is
// the durable completed state because this state machine deletes it last.
func (r *PredecessorRetirement) Preflight(ctx context.Context) error {
	_, err := r.preflightPredecessor(ctx)
	return err
}

// Retire deletes every predecessor binding, invokes the mandatory direct
// all-API-server convergence barrier, then deletes policies, the hook probe,
// and finally the sealed inventory marker. Every delete uses the UID and
// resourceVersion from an immediate exact re-read.
func (r *PredecessorRetirement) Retire(ctx context.Context, barrier PredecessorRetirementBarrier) error {
	if barrier == nil {
		return errors.New("predecessor retirement admission convergence barrier is required")
	}
	snapshot, err := r.preflightPredecessor(ctx)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}

	for _, pair := range snapshot.pairs {
		if err := r.deleteBinding(ctx, pair.binding); err != nil {
			return err
		}
	}

	if snapshot.hasPolicy() {
		target := PredecessorRetirementBarrierTarget{
			marker: snapshot.marker.DeepCopy(),
			probes: predecessorRetirementProbes(snapshot.guard, snapshot.pairs),
		}
		if err := barrier(ctx, target); err != nil {
			return fmt.Errorf("prove predecessor admission binding retirement: %w", err)
		}
		refreshed, err := r.preflightPredecessor(ctx)
		if err != nil {
			return fmt.Errorf("verify predecessor inventory after binding convergence: %w", err)
		}
		if refreshed == nil || refreshed.marker.UID != snapshot.marker.UID ||
			refreshed.marker.Data[PredecessorRetirementInventoryDataKey] != snapshot.marker.Data[PredecessorRetirementInventoryDataKey] {
			return errors.New("sealed predecessor inventory changed during binding convergence")
		}
		for _, pair := range refreshed.pairs {
			if pair.binding.present {
				return fmt.Errorf("predecessor binding %s reappeared after convergence", pair.binding.entry.Name)
			}
		}
		snapshot = refreshed
	}

	for _, pair := range snapshot.pairs {
		if err := r.deletePolicy(ctx, pair.policy); err != nil {
			return err
		}
	}
	if err := r.deleteConfigMap(ctx, snapshot.probe, "predecessor hook probe"); err != nil {
		return err
	}

	final, err := r.preflightPredecessor(ctx)
	if err != nil {
		return fmt.Errorf("verify predecessor inventory before marker retirement: %w", err)
	}
	if final == nil {
		return nil
	}
	if final.hasAnyAttestedObject() {
		return errors.New("predecessor inventory retains objects before marker retirement")
	}
	return r.deleteSealedMarker(ctx, final)
}

func (r *PredecessorRetirement) validatedRollout() (*RolloutGuard, error) {
	if r == nil || r.rollout == nil || r.policies == nil || r.bindings == nil || r.configMaps == nil {
		return nil, errors.New("predecessor retirement clients and rollout identity are required")
	}
	copy := *r.rollout
	copy.Policies = r.policies
	copy.Bindings = r.bindings
	copy.ConfigMapDeleter = r.configMaps
	copy.ControllerArgs = slices.Clone(r.rollout.ControllerArgs)
	copy.CertificateArgs = slices.Clone(r.rollout.CertificateArgs)
	copy.RuntimeDeploymentConfigExpressions = slices.Clone(r.rollout.RuntimeDeploymentConfigExpressions)
	copy.RuntimePodConfigExpressions = slices.Clone(r.rollout.RuntimePodConfigExpressions)
	if err := copy.validateIdentity(); err != nil {
		return nil, fmt.Errorf("validate predecessor retirement identity: %w", err)
	}
	if copy.PreviousControllerReleaseSequence < 0 || copy.PreviousControllerReleaseSequence >= copy.ReleaseSequence {
		return nil, fmt.Errorf("predecessor release sequence %d is invalid for candidate %d", copy.PreviousControllerReleaseSequence, copy.ReleaseSequence)
	}
	if copy.PreviousControllerReleaseSequence > 0 && copy.PreviousControllerReleaseSequence+1 != copy.ReleaseSequence {
		return nil, fmt.Errorf("candidate release sequence %d does not immediately follow predecessor %d", copy.ReleaseSequence, copy.PreviousControllerReleaseSequence)
	}
	return &copy, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (r *PredecessorRetirement) currentInventory(ctx context.Context, rollout *RolloutGuard) (predecessorRetirementInventory, error) {
	blueprints, err := predecessorRetirementPairBlueprints(rollout)
	if err != nil {
		return predecessorRetirementInventory{}, err
	}
	entries := make([]predecessorRetirementInventoryEntry, 0, len(blueprints)*2+1)
	for _, blueprint := range blueprints {
		policy, err := r.policies.Get(ctx, blueprint.name, metav1.GetOptions{})
		if err != nil {
			return predecessorRetirementInventory{}, fmt.Errorf("get current predecessor-retirement policy %s: %w", blueprint.name, err)
		}
		if err := verifyCurrentRetirementPolicy(policy, blueprint); err != nil {
			return predecessorRetirementInventory{}, err
		}
		policyDigest, err := predecessorPolicyDigest(policy)
		if err != nil {
			return predecessorRetirementInventory{}, err
		}
		entries = append(entries, predecessorRetirementInventoryEntry{
			Kind: "ValidatingAdmissionPolicy", Name: blueprint.name, UID: policy.UID, Digest: policyDigest,
		})

		binding, err := r.bindings.Get(ctx, blueprint.name, metav1.GetOptions{})
		if err != nil {
			return predecessorRetirementInventory{}, fmt.Errorf("get current predecessor-retirement binding %s: %w", blueprint.name, err)
		}
		if err := verifyCurrentRetirementBinding(binding, blueprint); err != nil {
			return predecessorRetirementInventory{}, err
		}
		bindingDigest, err := predecessorBindingDigest(binding)
		if err != nil {
			return predecessorRetirementInventory{}, err
		}
		entries = append(entries, predecessorRetirementInventoryEntry{
			Kind: "ValidatingAdmissionPolicyBinding", Name: blueprint.name, UID: binding.UID, Digest: bindingDigest,
		})
	}

	probeName := HookIdentityProbeObjectName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	probe, err := r.configMaps.Get(ctx, probeName, metav1.GetOptions{})
	if err != nil {
		return predecessorRetirementInventory{}, fmt.Errorf("get current predecessor-retirement hook probe: %w", err)
	}
	if probe == nil {
		return predecessorRetirementInventory{}, fmt.Errorf("current predecessor-retirement hook probe ConfigMap/%s is nil", probeName)
	}
	if err := verifyHookIdentityProbeMarker(probe, rollout, probeName, HookIdentityProbeGuardPolicyName(
		rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage,
	)); err != nil {
		return predecessorRetirementInventory{}, err
	}
	if err := verifyRetirementLiveIdentity("ConfigMap", probeName, probe); err != nil {
		return predecessorRetirementInventory{}, err
	}
	probeDigest, err := predecessorConfigMapDigest(probe)
	if err != nil {
		return predecessorRetirementInventory{}, err
	}
	entries = append(entries, predecessorRetirementInventoryEntry{
		Kind: "ConfigMap", Name: probeName, UID: probe.UID, Digest: probeDigest,
	})

	inventory := predecessorRetirementInventory{Version: PredecessorRetirementInventoryVersion, Entries: entries}
	if err := validatePredecessorRetirementInventory(inventory, predecessorRetirementExpectedEntries(
		rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage,
	)); err != nil {
		return predecessorRetirementInventory{}, err
	}
	return inventory, nil
}

func verifyCurrentRetirementPolicy(
	actual *admissionregistrationv1.ValidatingAdmissionPolicy,
	blueprint predecessorRetirementPairBlueprint,
) error {
	if err := verifyRetirementLiveIdentity("ValidatingAdmissionPolicy", blueprint.name, actual); err != nil {
		return err
	}
	if err := blueprint.verifyPolicy(actual); err != nil {
		return fmt.Errorf("verify current predecessor-retirement policy %s: %w", blueprint.name, err)
	}
	return nil
}

func verifyCurrentRetirementBinding(
	actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	blueprint predecessorRetirementPairBlueprint,
) error {
	if err := verifyRetirementLiveIdentity("ValidatingAdmissionPolicyBinding", blueprint.name, actual); err != nil {
		return err
	}
	if err := blueprint.verifyBinding(actual); err != nil {
		return fmt.Errorf("verify current predecessor-retirement binding %s: %w", blueprint.name, err)
	}
	return nil
}

func verifyRetirementLiveIdentity(kind, name string, object metav1.Object) error {
	if object == nil {
		return fmt.Errorf("predecessor-retirement %s/%s is nil", kind, name)
	}
	if object.GetName() != name || (kind != "ConfigMap" && object.GetNamespace() != "") ||
		object.GetGenerateName() != "" || object.GetUID() == "" || object.GetResourceVersion() == "" ||
		object.GetDeletionTimestamp() != nil || object.GetDeletionGracePeriodSeconds() != nil ||
		len(object.GetOwnerReferences()) != 0 || len(object.GetFinalizers()) != 0 {
		return fmt.Errorf("predecessor-retirement %s/%s has an invalid live identity", kind, name)
	}
	return nil
}

type predecessorRetirementLiveObject struct {
	entry           predecessorRetirementInventoryEntry
	resourceVersion string
	present         bool
}

type predecessorRetirementLivePair struct {
	policy  predecessorRetirementLiveObject
	binding predecessorRetirementLiveObject
}

type predecessorRetirementSnapshot struct {
	guard  *AdmissionConvergenceGuard
	marker *corev1.ConfigMap
	pairs  []predecessorRetirementLivePair
	probe  predecessorRetirementLiveObject
}

func (s *predecessorRetirementSnapshot) hasPolicy() bool {
	for _, pair := range s.pairs {
		if pair.policy.present {
			return true
		}
	}
	return false
}

func (s *predecessorRetirementSnapshot) hasAnyAttestedObject() bool {
	if s.probe.present {
		return true
	}
	for _, pair := range s.pairs {
		if pair.policy.present || pair.binding.present {
			return true
		}
	}
	return false
}

func (r *PredecessorRetirement) preflightPredecessor(ctx context.Context) (*predecessorRetirementSnapshot, error) {
	rollout, err := r.validatedRollout()
	if err != nil {
		return nil, err
	}
	sequence := rollout.PreviousControllerReleaseSequence
	if sequence == 0 {
		return nil, nil
	}
	name := AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, sequence)
	marker, err := r.configMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sealed predecessor admission convergence marker: %w", err)
	}
	guard, err := admissionConvergenceGuardFromSealedMarker(rollout.ReleaseNamespace, rollout.ReleaseName, sequence, marker)
	if err != nil {
		return nil, err
	}
	inventory, err := guard.verifySealedMarker(marker)
	if err != nil {
		return nil, err
	}

	pairs := make([]predecessorRetirementLivePair, 0, 12)
	for index := 0; index < len(inventory.Entries)-1; index += 2 {
		policyEntry := inventory.Entries[index]
		bindingEntry := inventory.Entries[index+1]
		policy, err := r.inspectPolicy(ctx, policyEntry)
		if err != nil {
			return nil, err
		}
		binding, err := r.inspectBinding(ctx, bindingEntry)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, predecessorRetirementLivePair{policy: policy, binding: binding})
	}
	probe, err := r.inspectConfigMap(ctx, inventory.Entries[len(inventory.Entries)-1])
	if err != nil {
		return nil, err
	}
	snapshot := &predecessorRetirementSnapshot{guard: guard, marker: marker.DeepCopy(), pairs: pairs, probe: probe}
	if err := validatePredecessorRetirementState(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validatePredecessorRetirementState(snapshot *predecessorRetirementSnapshot) error {
	if snapshot == nil || snapshot.marker == nil || len(snapshot.pairs) != 12 {
		return errors.New("predecessor retirement snapshot is incomplete")
	}
	seenPresentBinding := false
	anyBinding := false
	for _, pair := range snapshot.pairs {
		if pair.binding.present {
			seenPresentBinding = true
			anyBinding = true
		} else if seenPresentBinding {
			return fmt.Errorf("predecessor retirement binding inventory is sparse at %s", pair.binding.entry.Name)
		}
	}
	if anyBinding {
		for _, pair := range snapshot.pairs {
			if !pair.policy.present {
				return fmt.Errorf("predecessor retirement policy %s is absent before all bindings", pair.policy.entry.Name)
			}
		}
		if !snapshot.probe.present {
			return errors.New("predecessor hook probe is absent before all bindings and policies")
		}
		return nil
	}

	seenPresentPolicy := false
	anyPolicy := false
	for _, pair := range snapshot.pairs {
		if pair.policy.present {
			seenPresentPolicy = true
			anyPolicy = true
		} else if seenPresentPolicy {
			return fmt.Errorf("predecessor retirement policy inventory is sparse at %s", pair.policy.entry.Name)
		}
	}
	if anyPolicy && !snapshot.probe.present {
		return errors.New("predecessor hook probe is absent before all policies")
	}
	return nil
}

func predecessorRetirementProbes(
	guard *AdmissionConvergenceGuard,
	pairs []predecessorRetirementLivePair,
) []PredecessorRetirementProbe {
	attempt := hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	probes := make([]PredecessorRetirementProbe, 0, len(pairs))
	for _, pair := range pairs {
		probe := newAdmissionConvergenceDependencyProbe(pair.policy.entry.Name, attempt)
		probes = append(probes, PredecessorRetirementProbe{
			PolicyName: probe.PolicyName, BindingName: probe.PolicyName,
			FieldManager: probe.FieldManager, Message: probe.Message,
		})
	}
	return probes
}

func (r *PredecessorRetirement) inspectPolicy(
	ctx context.Context,
	entry predecessorRetirementInventoryEntry,
) (predecessorRetirementLiveObject, error) {
	object, err := r.policies.Get(ctx, entry.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return predecessorRetirementLiveObject{entry: entry}, nil
	}
	if err != nil {
		return predecessorRetirementLiveObject{}, fmt.Errorf("get predecessor policy %s: %w", entry.Name, err)
	}
	if err := verifyAttestedLiveObject(entry, object, predecessorPolicyDigest); err != nil {
		return predecessorRetirementLiveObject{}, err
	}
	return predecessorRetirementLiveObject{
		entry: entry, resourceVersion: object.ResourceVersion, present: true,
	}, nil
}

func (r *PredecessorRetirement) inspectBinding(
	ctx context.Context,
	entry predecessorRetirementInventoryEntry,
) (predecessorRetirementLiveObject, error) {
	object, err := r.bindings.Get(ctx, entry.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return predecessorRetirementLiveObject{entry: entry}, nil
	}
	if err != nil {
		return predecessorRetirementLiveObject{}, fmt.Errorf("get predecessor binding %s: %w", entry.Name, err)
	}
	if err := verifyAttestedLiveObject(entry, object, predecessorBindingDigest); err != nil {
		return predecessorRetirementLiveObject{}, err
	}
	return predecessorRetirementLiveObject{
		entry: entry, resourceVersion: object.ResourceVersion, present: true,
	}, nil
}

func (r *PredecessorRetirement) inspectConfigMap(
	ctx context.Context,
	entry predecessorRetirementInventoryEntry,
) (predecessorRetirementLiveObject, error) {
	object, err := r.configMaps.Get(ctx, entry.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return predecessorRetirementLiveObject{entry: entry}, nil
	}
	if err != nil {
		return predecessorRetirementLiveObject{}, fmt.Errorf("get predecessor ConfigMap %s: %w", entry.Name, err)
	}
	if err := verifyAttestedLiveObject(entry, object, predecessorConfigMapDigest); err != nil {
		return predecessorRetirementLiveObject{}, err
	}
	return predecessorRetirementLiveObject{
		entry: entry, resourceVersion: object.ResourceVersion, present: true,
	}, nil
}

type predecessorDigestible interface {
	metav1.Object
}

func verifyAttestedLiveObject[T predecessorDigestible](
	entry predecessorRetirementInventoryEntry,
	object T,
	digest func(T) (string, error),
) error {
	if reflect.ValueOf(object).IsNil() {
		return fmt.Errorf("attested predecessor %s/%s is nil", entry.Kind, entry.Name)
	}
	if object.GetName() != entry.Name || (entry.Kind != "ConfigMap" && object.GetNamespace() != "") ||
		object.GetGenerateName() != "" || object.GetUID() != entry.UID || object.GetResourceVersion() == "" ||
		object.GetDeletionTimestamp() != nil || object.GetDeletionGracePeriodSeconds() != nil ||
		len(object.GetOwnerReferences()) != 0 || len(object.GetFinalizers()) != 0 {
		return fmt.Errorf("attested predecessor %s/%s has a foreign or incomplete live identity", entry.Kind, entry.Name)
	}
	actualDigest, err := digest(object)
	if err != nil {
		return err
	}
	if actualDigest != entry.Digest {
		return fmt.Errorf("attested predecessor %s/%s differs from sealed semantic digest", entry.Kind, entry.Name)
	}
	return nil
}

func (r *PredecessorRetirement) deleteBinding(ctx context.Context, observed predecessorRetirementLiveObject) error {
	if !observed.present {
		return nil
	}
	fresh, err := r.inspectBinding(ctx, observed.entry)
	if err != nil {
		return fmt.Errorf("verify predecessor binding %s before deletion: %w", observed.entry.Name, err)
	}
	if !fresh.present {
		return nil
	}
	err = r.bindings.Delete(ctx, fresh.entry.Name, predecessorDeleteOptions(fresh))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete predecessor binding %s: %w", fresh.entry.Name, err)
	}
	return nil
}

func (r *PredecessorRetirement) deletePolicy(ctx context.Context, observed predecessorRetirementLiveObject) error {
	if !observed.present {
		return nil
	}
	fresh, err := r.inspectPolicy(ctx, observed.entry)
	if err != nil {
		return fmt.Errorf("verify predecessor policy %s before deletion: %w", observed.entry.Name, err)
	}
	if !fresh.present {
		return nil
	}
	err = r.policies.Delete(ctx, fresh.entry.Name, predecessorDeleteOptions(fresh))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete predecessor policy %s: %w", fresh.entry.Name, err)
	}
	return nil
}

func (r *PredecessorRetirement) deleteConfigMap(
	ctx context.Context,
	observed predecessorRetirementLiveObject,
	description string,
) error {
	if !observed.present {
		return nil
	}
	if observed.entry.Name == "" || observed.entry.UID == "" || observed.resourceVersion == "" {
		return fmt.Errorf("%s deletion identity is incomplete", description)
	}
	fresh, err := r.inspectConfigMap(ctx, observed.entry)
	if err != nil {
		return fmt.Errorf("verify %s before deletion: %w", description, err)
	}
	if !fresh.present {
		return nil
	}
	observed = fresh
	err = r.configMaps.Delete(ctx, observed.entry.Name, predecessorDeleteOptions(observed))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s ConfigMap/%s: %w", description, observed.entry.Name, err)
	}
	return nil
}

func (r *PredecessorRetirement) deleteSealedMarker(ctx context.Context, snapshot *predecessorRetirementSnapshot) error {
	if snapshot == nil || snapshot.guard == nil || snapshot.marker == nil {
		return errors.New("sealed predecessor inventory marker deletion state is incomplete")
	}
	marker, err := r.configMaps.Get(ctx, snapshot.marker.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get sealed predecessor inventory marker before deletion: %w", err)
	}
	if marker == nil || marker.UID != snapshot.marker.UID {
		return errors.New("sealed predecessor inventory marker has a foreign live identity")
	}
	if _, err := snapshot.guard.verifySealedMarker(marker); err != nil {
		return fmt.Errorf("verify sealed predecessor inventory marker before deletion: %w", err)
	}
	if marker.Data[PredecessorRetirementInventoryDataKey] != snapshot.marker.Data[PredecessorRetirementInventoryDataKey] {
		return errors.New("sealed predecessor inventory marker changed before deletion")
	}
	live := predecessorRetirementLiveObject{
		entry: predecessorRetirementInventoryEntry{
			Kind: "ConfigMap", Name: marker.Name, UID: marker.UID,
		},
		resourceVersion: marker.ResourceVersion,
		present:         true,
	}
	err = r.configMaps.Delete(ctx, marker.Name, predecessorDeleteOptions(live))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete sealed predecessor inventory ConfigMap/%s: %w", marker.Name, err)
	}
	return nil
}

func predecessorDeleteOptions(object predecessorRetirementLiveObject) metav1.DeleteOptions {
	uid := object.entry.UID
	resourceVersion := object.resourceVersion
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion,
	}}
}
