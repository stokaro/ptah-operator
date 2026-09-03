package crdupgrade

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	managedByLabel                 = "app.kubernetes.io/managed-by"
	instanceLabel                  = "app.kubernetes.io/instance"
)

// MutatingWebhookUpdater is the exact API surface used by the one-time
// admission-singleton adoption path.
type MutatingWebhookUpdater interface {
	MutatingWebhookClient
	Update(context.Context, *admissionregistrationv1.MutatingWebhookConfiguration, metav1.UpdateOptions) (*admissionregistrationv1.MutatingWebhookConfiguration, error)
}

// ValidatingWebhookUpdater is the exact API surface used by the one-time
// admission-singleton adoption path.
type ValidatingWebhookUpdater interface {
	ValidatingWebhookClient
	Update(context.Context, *admissionregistrationv1.ValidatingWebhookConfiguration, metav1.UpdateOptions) (*admissionregistrationv1.ValidatingWebhookConfiguration, error)
}

// AdmissionAdopter recognizes the exact annotation-free singleton contract
// from the predecessor release and stamps its immutable ownership tuple before
// any controller rollout. Unknown, partially annotated, or foreign objects are
// never adopted.
type AdmissionAdopter struct {
	Mutating   MutatingWebhookUpdater
	Validating ValidatingWebhookUpdater
	Expected   RuntimeInvariants
}

type singletonAdoptionState struct {
	legacy      bool
	needsUpdate bool
}

// Preflight performs every ownership, contract, and server-side dry-run check
// without persisting the one-time legacy annotation adoption.
func (a *AdmissionAdopter) Preflight(ctx context.Context) error {
	return a.adopt(ctx, false)
}

// Adopt performs a complete read-only preflight before its first API update.
// A new installation, where both singleton objects are absent, needs no work.
func (a *AdmissionAdopter) Adopt(ctx context.Context) error {
	return a.adopt(ctx, true)
}

func (a *AdmissionAdopter) adopt(ctx context.Context, apply bool) error {
	if a == nil || a.Mutating == nil || a.Validating == nil {
		return fmt.Errorf("admission updater clients are required")
	}
	if err := a.Expected.validate(); err != nil {
		return err
	}

	mutating, mutatingErr := a.Mutating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
	validating, validatingErr := a.Validating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
	mutatingMissing := apierrors.IsNotFound(mutatingErr)
	validatingMissing := apierrors.IsNotFound(validatingErr)
	if mutatingMissing && validatingMissing {
		return nil
	}
	if mutatingMissing != validatingMissing {
		return fmt.Errorf("fixed admission singleton is incomplete; both configurations must exist or both must be absent")
	}
	if mutatingErr != nil {
		return fmt.Errorf("get fixed MutatingWebhookConfiguration: %w", mutatingErr)
	}
	if validatingErr != nil {
		return fmt.Errorf("get fixed ValidatingWebhookConfiguration: %w", validatingErr)
	}

	mutatingMetadataState, err := a.classifyForAdoption("MutatingWebhookConfiguration", mutating.ObjectMeta)
	if err != nil {
		return err
	}
	validatingMetadataState, err := a.classifyForAdoption("ValidatingWebhookConfiguration", validating.ObjectMeta)
	if err != nil {
		return err
	}
	legacyExpected := a.Expected
	if mutatingMetadataState.legacy || validatingMetadataState.legacy {
		legacyExpected, err = legacyRuntimeInvariants(mutating, validating, a.Expected)
		if err != nil {
			return err
		}
	}
	mutatingState, err := a.validateMutatingForAdoption(mutating, legacyExpected)
	if err != nil {
		return err
	}
	validatingState, err := a.validateValidatingForAdoption(validating, legacyExpected)
	if err != nil {
		return err
	}
	if !mutatingState.needsUpdate && !validatingState.needsUpdate {
		return nil
	}

	if mutatingState.needsUpdate {
		candidate := mutating.DeepCopy()
		copyExpectedAnnotations(&candidate.ObjectMeta, a.Expected.annotations())
		if _, err := a.Mutating.Update(ctx, candidate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return fmt.Errorf("dry-run legacy MutatingWebhookConfiguration adoption: %w", err)
		}
	}
	if validatingState.needsUpdate {
		candidate := validating.DeepCopy()
		copyExpectedAnnotations(&candidate.ObjectMeta, a.Expected.annotations())
		if _, err := a.Validating.Update(ctx, candidate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return fmt.Errorf("dry-run legacy ValidatingWebhookConfiguration adoption: %w", err)
		}
	}
	if !apply {
		return nil
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, getErr := a.Mutating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		state, validateErr := a.validateMutatingForAdoption(current, legacyExpected)
		if validateErr != nil || !state.needsUpdate {
			return validateErr
		}
		copyExpectedAnnotations(&current.ObjectMeta, a.Expected.annotations())
		_, updateErr := a.Mutating.Update(ctx, current, metav1.UpdateOptions{})
		return updateErr
	}); err != nil {
		return fmt.Errorf("adopt legacy MutatingWebhookConfiguration: %w", err)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, getErr := a.Validating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		state, validateErr := a.validateValidatingForAdoption(current, legacyExpected)
		if validateErr != nil || !state.needsUpdate {
			return validateErr
		}
		copyExpectedAnnotations(&current.ObjectMeta, a.Expected.annotations())
		_, updateErr := a.Validating.Update(ctx, current, metav1.UpdateOptions{})
		return updateErr
	}); err != nil {
		return fmt.Errorf("adopt legacy ValidatingWebhookConfiguration: %w", err)
	}

	mutating, err = a.Mutating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read adopted MutatingWebhookConfiguration: %w", err)
	}
	mutatingFinal, err := a.validateMutatingForAdoption(mutating, legacyExpected)
	if err != nil {
		return fmt.Errorf("verify adopted MutatingWebhookConfiguration: %w", err)
	}
	if mutatingFinal.needsUpdate {
		return fmt.Errorf("verify adopted MutatingWebhookConfiguration: owned annotation tuple is not current")
	}
	validating, err = a.Validating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read adopted ValidatingWebhookConfiguration: %w", err)
	}
	validatingFinal, err := a.validateValidatingForAdoption(validating, legacyExpected)
	if err != nil {
		return fmt.Errorf("verify adopted ValidatingWebhookConfiguration: %w", err)
	}
	if validatingFinal.needsUpdate {
		return fmt.Errorf("verify adopted ValidatingWebhookConfiguration: owned annotation tuple is not current")
	}
	return nil
}

func (a *AdmissionAdopter) validateMutatingForAdoption(
	configuration *admissionregistrationv1.MutatingWebhookConfiguration,
	legacyExpected RuntimeInvariants,
) (singletonAdoptionState, error) {
	if configuration == nil {
		return singletonAdoptionState{}, fmt.Errorf("fixed MutatingWebhookConfiguration is required")
	}
	state, err := a.classifyForAdoption("MutatingWebhookConfiguration", configuration.ObjectMeta)
	if err != nil {
		return singletonAdoptionState{}, err
	}
	if state.legacy {
		if err := verifyMutatingWebhookContract(configuration, legacyExpected); err != nil {
			return singletonAdoptionState{}, err
		}
	}
	return state, nil
}

func (a *AdmissionAdopter) validateValidatingForAdoption(
	configuration *admissionregistrationv1.ValidatingWebhookConfiguration,
	legacyExpected RuntimeInvariants,
) (singletonAdoptionState, error) {
	if configuration == nil {
		return singletonAdoptionState{}, fmt.Errorf("fixed ValidatingWebhookConfiguration is required")
	}
	state, err := a.classifyForAdoption("ValidatingWebhookConfiguration", configuration.ObjectMeta)
	if err != nil {
		return singletonAdoptionState{}, err
	}
	if state.legacy {
		if err := verifyValidatingWebhookContract(configuration, legacyExpected); err != nil {
			return singletonAdoptionState{}, err
		}
	}
	return state, nil
}

func (a *AdmissionAdopter) classifyForAdoption(kind string, metadata metav1.ObjectMeta) (singletonAdoptionState, error) {
	if err := verifyHelmOwnership(kind, metadata, a.Expected); err != nil {
		return singletonAdoptionState{}, err
	}
	return classifyOwnedAnnotations(kind, metadata.Name, metadata.Annotations, a.Expected)
}

func verifyHelmOwnership(kind string, metadata metav1.ObjectMeta, expected RuntimeInvariants) error {
	if metadata.Name != AdmissionConfigurationName ||
		metadata.Annotations[helmReleaseNameAnnotation] != expected.ReleaseName ||
		metadata.Annotations[helmReleaseNamespaceAnnotation] != expected.ReleaseNamespace ||
		metadata.Labels[managedByLabel] != "Helm" ||
		metadata.Labels[instanceLabel] != expected.ReleaseName {
		return fmt.Errorf("fixed admission singleton %s/%s is not owned by Helm release %s/%s", kind, metadata.Name, expected.ReleaseNamespace, expected.ReleaseName)
	}
	return nil
}

func classifyOwnedAnnotations(kind, name string, actual map[string]string, expected RuntimeInvariants) (singletonAdoptionState, error) {
	expectedAnnotations := expected.annotations()
	present := 0
	for key := range expectedAnnotations {
		_, found := actual[key]
		if !found {
			continue
		}
		present++
	}
	if present == 0 {
		return singletonAdoptionState{legacy: true, needsUpdate: true}, nil
	}
	if present != len(expectedAnnotations) {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s has an incomplete owned annotation tuple", kind, name)
	}
	for key, expectedValue := range expected.immutableAnnotations() {
		if actualValue := actual[key]; actualValue != expectedValue {
			return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s annotation %s is %q, expected %q", kind, name, key, actualValue, expectedValue)
		}
	}
	stateVersion, err := positiveDecimalValue(actual[ControllerStateVersionAnnotation])
	if err != nil {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s annotation %s: %w", kind, name, ControllerStateVersionAnnotation, err)
	}
	contractVersion, err := positiveDecimalValue(actual[AdmissionContractVersionAnnotation])
	if err != nil {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s annotation %s: %w", kind, name, AdmissionContractVersionAnnotation, err)
	}
	if stateVersion > uint64(expected.ControllerStateVersion) {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s has newer controller-state version %d", kind, name, stateVersion)
	}
	if contractVersion > uint64(expected.AdmissionContractVersion) {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s has newer admission-contract version %d", kind, name, contractVersion)
	}
	releaseSequence, err := positiveDecimalValue(actual[ReleaseSequenceAnnotation])
	if err != nil {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s annotation %s: %w", kind, name, ReleaseSequenceAnnotation, err)
	}
	if releaseSequence > uint64(expected.ReleaseSequence) {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s has newer release sequence %d", kind, name, releaseSequence)
	}
	actualHook := actual[HookServiceAccountAnnotation]
	if releaseSequence == uint64(expected.ReleaseSequence) {
		if actualHook != expected.HookServiceAccountName {
			return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s annotation %s is %q, expected %q", kind, name, HookServiceAccountAnnotation, actualHook, expected.HookServiceAccountName)
		}
	} else if !historicalHookServiceAccount(actualHook, expected.HookServiceAccountName, releaseSequence, uint64(expected.ReleaseSequence)) {
		return singletonAdoptionState{}, fmt.Errorf("fixed admission singleton %s/%s has invalid historical hook ServiceAccount identity %q", kind, name, actualHook)
	}
	// Owned annotations and webhook spec are one Helm-managed API update. The
	// hook validates monotonic compatibility but never advances either version
	// independently of the contract it identifies.
	return singletonAdoptionState{}, nil
}

func historicalHookServiceAccount(actual, current string, sequence, currentSequence uint64) bool {
	marker := "-crd-v" + strconv.FormatUint(currentSequence, 10) + "-"
	index := strings.LastIndex(current, marker)
	if index < 0 {
		return false
	}
	prefix := current[:index] + "-crd-v" + strconv.FormatUint(sequence, 10) + "-"
	if !strings.HasPrefix(actual, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(actual, prefix)
	return len(suffix) == 12 && regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(suffix)
}

func legacyRuntimeInvariants(
	mutating *admissionregistrationv1.MutatingWebhookConfiguration,
	validating *admissionregistrationv1.ValidatingWebhookConfiguration,
	expected RuntimeInvariants,
) (RuntimeInvariants, error) {
	if len(mutating.Webhooks) != 1 || len(validating.Webhooks) != 2 {
		return RuntimeInvariants{}, fmt.Errorf("legacy fixed admission singleton has unexpected webhook cardinality")
	}
	timeout := mutating.Webhooks[0].TimeoutSeconds
	if timeout == nil || *timeout < 1 || *timeout > 30 {
		return RuntimeInvariants{}, fmt.Errorf("legacy fixed admission singleton has invalid timeoutSeconds")
	}
	for _, webhook := range validating.Webhooks {
		if webhook.TimeoutSeconds == nil || *webhook.TimeoutSeconds != *timeout {
			return RuntimeInvariants{}, fmt.Errorf("legacy fixed admission singleton does not use one timeoutSeconds value")
		}
	}
	expected.WebhookTimeoutSeconds = *timeout
	return expected, nil
}

func positiveDecimalValue(raw string) (uint64, error) {
	if raw == "" || raw[0] < '1' || raw[0] > '9' {
		return 0, fmt.Errorf("%q is not a positive exact decimal version", raw)
	}
	for _, character := range raw[1:] {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("%q is not a positive exact decimal version", raw)
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a positive exact decimal version: %w", raw, err)
	}
	return value, nil
}

func copyExpectedAnnotations(metadata *metav1.ObjectMeta, expected map[string]string) {
	if metadata.Annotations == nil {
		metadata.Annotations = make(map[string]string, len(expected))
	}
	for key, value := range expected {
		metadata.Annotations[key] = value
	}
}
