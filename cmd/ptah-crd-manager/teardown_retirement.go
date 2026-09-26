package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func newConfiguredTeardownRetirementGuard(
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) (*crdupgrade.TeardownRetirementGuard, error) {
	if rollout == nil {
		return nil, errors.New("teardown retirement rollout identity is required")
	}
	guard := crdupgrade.NewTeardownRetirementGuard(rollout)
	recreateMissingSecret, err := optionalExactBooleanRuntimeArgument(rollout.CertificateArgs, "--recreate-missing-secret=")
	if err != nil {
		return nil, fmt.Errorf("read certificate recovery retirement contract: %w", err)
	}
	if !contract.CertificateRuntimeEnabled {
		if recreateMissingSecret {
			return nil, errors.New("certificate recovery is enabled while the certificate runtime is disabled")
		}
		return guard, nil
	}
	if contract.CertificateServiceAccountName != rollout.CertificateDeploymentName {
		return nil, fmt.Errorf(
			"runtime admission certificate ServiceAccount %q differs from rollout identity %q",
			contract.CertificateServiceAccountName,
			rollout.CertificateDeploymentName,
		)
	}
	if !recreateMissingSecret {
		return guard, nil
	}

	policyName, err := exactRuntimeArgument(rollout.CertificateArgs, "--secret-create-policy-name=")
	if err != nil {
		return nil, err
	}
	bindingName, err := exactRuntimeArgument(rollout.CertificateArgs, "--secret-create-policy-binding-name=")
	if err != nil {
		return nil, err
	}
	serviceAccountName, err := exactRuntimeArgument(rollout.CertificateArgs, "--secret-create-service-account-name=")
	if err != nil {
		return nil, err
	}
	if policyName != bindingName {
		return nil, errors.New("certificate recovery policy and binding names must be identical for retirement")
	}
	if serviceAccountName != rollout.CertificateDeploymentName || policyName != rollout.CertificateDeploymentName {
		return nil, fmt.Errorf(
			"certificate recovery policy, binding, and ServiceAccount must equal the exact certificate runtime identity %q",
			rollout.CertificateDeploymentName,
		)
	}
	config := certificateRecoveryRetirementConfig(rollout, policyName, bindingName, serviceAccountName)
	pair := crdupgrade.TeardownOriginalPairVerifier{
		Name: policyName,
		VerifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
			if policy == nil {
				return errors.New("certificate recovery retirement policy is nil")
			}
			if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicy", policyName, rollout, policy); err != nil {
				return err
			}
			return certrotation.VerifySecretCreatePolicyContract(policy, config)
		},
		VerifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
			if binding == nil {
				return errors.New("certificate recovery retirement binding is nil")
			}
			if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicyBinding", bindingName, rollout, binding); err != nil {
				return err
			}
			return certrotation.VerifySecretCreateBindingContract(binding, config)
		},
	}
	return guard.WithOriginalPairs(pair)
}

func certificateRecoveryRetirementConfig(
	rollout *crdupgrade.RolloutGuard,
	policyName, bindingName, serviceAccountName string,
) certrotation.Config {
	return certrotation.Config{
		Namespace:                      rollout.ReleaseNamespace,
		ReleaseName:                    rollout.ReleaseName,
		SecretName:                     rollout.WebhookSecretName,
		SecretCreatePolicyName:         policyName,
		SecretCreatePolicyBindingName:  bindingName,
		SecretCreateServiceAccountName: serviceAccountName,
		RecreateMissingSecret:          true,
	}
}

type teardownRetirementMetadataObject interface {
	metav1.Object
}

func verifyCertificateRecoveryRetirementMetadata(
	kind, name string,
	rollout *crdupgrade.RolloutGuard,
	object teardownRetirementMetadataObject,
) error {
	if rollout == nil || object == nil {
		return fmt.Errorf("certificate recovery retirement %s/%s is nil", kind, name)
	}
	annotations := object.GetAnnotations()
	labels := object.GetLabels()
	if object.GetName() != name || object.GetNamespace() != "" || object.GetGenerateName() != "" ||
		object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil ||
		object.GetDeletionGracePeriodSeconds() != nil || len(object.GetOwnerReferences()) != 0 || len(object.GetFinalizers()) != 0 ||
		len(annotations) != 2 || annotations["meta.helm.sh/release-name"] != rollout.ReleaseName ||
		annotations["meta.helm.sh/release-namespace"] != rollout.ReleaseNamespace || len(labels) != 6 ||
		labels["app.kubernetes.io/managed-by"] != "Helm" || labels["app.kubernetes.io/instance"] != rollout.ReleaseName ||
		labels["app.kubernetes.io/component"] != "certificate-rotation" ||
		!exactNonemptyMetadataValue(labels["helm.sh/chart"]) || !exactNonemptyMetadataValue(labels["app.kubernetes.io/name"]) ||
		!exactNonemptyMetadataValue(labels["app.kubernetes.io/version"]) {
		return fmt.Errorf("certificate recovery retirement %s/%s has foreign or incomplete Helm ownership", kind, name)
	}
	return nil
}

func exactNonemptyMetadataValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func exactRuntimeArgument(arguments []string, prefix string) (string, error) {
	found := ""
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, prefix) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("runtime argument %s is duplicated", prefix)
		}
		found = strings.TrimPrefix(argument, prefix)
		if found == "" || found != strings.TrimSpace(found) {
			return "", fmt.Errorf("runtime argument %s has an empty or padded value", prefix)
		}
	}
	if found == "" {
		return "", fmt.Errorf("runtime argument %s is required", prefix)
	}
	return found, nil
}

func optionalExactBooleanRuntimeArgument(arguments []string, prefix string) (bool, error) {
	found := false
	value := false
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, prefix) {
			continue
		}
		if found {
			return false, fmt.Errorf("runtime argument %s is duplicated", prefix)
		}
		found = true
		switch strings.TrimPrefix(argument, prefix) {
		case "true":
			value = true
		case "false":
			value = false
		default:
			return false, fmt.Errorf("runtime argument %s must be exactly true or false", prefix)
		}
	}
	return value, nil
}

type teardownRetirementConfigMapClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

type teardownRetirementFinalizer struct {
	configMaps       teardownRetirementConfigMapClient
	markers          []crdupgrade.TeardownRetirementMarkerTarget
	activationName   string
	verifyActivation func(*corev1.ConfigMap) error
}

// newTeardownRetirementFinalizer creates the only mutating component in the
// final phase. Its client surface cannot mutate VAP/VAPB resources. Supplied
// secondary markers are deleted first and activation is the final mutation.
// The dedicated retirement marker is Helm-owned and is left for Helm to delete.
func newTeardownRetirementFinalizer(
	configMaps teardownRetirementConfigMapClient,
	guard *crdupgrade.TeardownRetirementGuard,
	markers ...crdupgrade.TeardownRetirementMarkerTarget,
) (*teardownRetirementFinalizer, error) {
	if configMaps == nil || guard == nil {
		return nil, errors.New("teardown retirement finalizer dependencies are required")
	}
	dedicated, err := guard.MarkerTarget()
	if err != nil {
		return nil, fmt.Errorf("derive dedicated teardown retirement marker: %w", err)
	}
	seen := map[string]struct{}{dedicated.Name: {}}
	for _, marker := range markers {
		if marker.Name == "" || marker.Name != strings.TrimSpace(marker.Name) || marker.Verify == nil {
			return nil, errors.New("teardown retirement finalizer marker is incomplete")
		}
		if marker.Name == crdupgrade.ReleaseActivationName {
			return nil, errors.New("teardown retirement marker collides with release activation")
		}
		if _, duplicate := seen[marker.Name]; duplicate {
			return nil, fmt.Errorf("teardown retirement marker %q is duplicated", marker.Name)
		}
		seen[marker.Name] = struct{}{}
	}
	return &teardownRetirementFinalizer{
		configMaps:       configMaps,
		markers:          append([]crdupgrade.TeardownRetirementMarkerTarget(nil), markers...),
		activationName:   crdupgrade.ReleaseActivationName,
		verifyActivation: guard.VerifyFinalActivation,
	}, nil
}

func (f *teardownRetirementFinalizer) Finalize(ctx context.Context) error {
	if f == nil || f.configMaps == nil || f.activationName == "" || f.verifyActivation == nil {
		return errors.New("teardown retirement finalizer is incomplete")
	}
	if ctx == nil {
		return errors.New("teardown retirement finalizer context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	present, err := f.preflight(ctx)
	if err != nil {
		return err
	}
	if !present[len(present)-1] {
		return nil
	}
	for index, marker := range f.markers {
		if !present[index] {
			continue
		}
		if _, err := f.getExactActivation(ctx); err != nil {
			return fmt.Errorf("reverify release activation before marker deletion: %w", err)
		}
		object, err := f.configMaps.Get(ctx, marker.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("re-read teardown retirement marker %s: %w", marker.Name, err)
		}
		if err := verifyTeardownRetirementConfigMapIdentity(marker.Name, object, marker.Verify); err != nil {
			return err
		}
		if err := f.configMaps.Delete(ctx, marker.Name, teardownRetirementDeleteOptions(object)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete teardown retirement marker %s: %w", marker.Name, err)
		}
	}
	activation, err := f.getExactActivation(ctx)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-read release activation for final deletion: %w", err)
	}
	// Return the parameter to the state a fresh install starts from, then delete
	// it. Kubernetes keeps serving the last value it saw for that object while
	// something was reading it, so the bindings are deliberately still bound
	// here: what the API server goes on serving after the delete is the state a
	// reinstall in this namespace needs, not the sequence this release last
	// activated. The state above has already been verified, so the reset is the
	// last thing that changes it.
	bootstrap := activation.DeepCopy()
	bootstrap.Data = crdupgrade.ReleaseActivationBootstrapData()
	if !reflect.DeepEqual(activation.Data, bootstrap.Data) {
		updated, updateErr := f.configMaps.Update(ctx, bootstrap, metav1.UpdateOptions{})
		if updateErr != nil {
			return fmt.Errorf("return release activation to its bootstrap state: %w", updateErr)
		}
		activation = updated
	}
	if err := f.configMaps.Delete(ctx, f.activationName, teardownRetirementDeleteOptions(activation)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete release activation as final API mutation: %w", err)
	}
	return nil
}

func (f *teardownRetirementFinalizer) preflight(ctx context.Context) ([]bool, error) {
	targets := make([]crdupgrade.TeardownRetirementMarkerTarget, 0, len(f.markers)+1)
	targets = append(targets, f.markers...)
	targets = append(targets, crdupgrade.TeardownRetirementMarkerTarget{Name: f.activationName, Verify: f.verifyActivation})
	present := make([]bool, len(targets))
	for index, target := range targets {
		object, err := f.configMaps.Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get teardown retirement ConfigMap/%s: %w", target.Name, err)
		}
		if err := verifyTeardownRetirementConfigMapIdentity(target.Name, object, target.Verify); err != nil {
			return nil, err
		}
		present[index] = true
	}
	activationPresent := present[len(present)-1]
	if !activationPresent {
		for index := range f.markers {
			if present[index] {
				return nil, fmt.Errorf("teardown retirement terminal state retains ConfigMap/%s after release activation is absent", f.markers[index].Name)
			}
		}
		return present, nil
	}
	sawPresent := false
	for index := range f.markers {
		if present[index] {
			sawPresent = true
			continue
		}
		if sawPresent {
			return nil, fmt.Errorf("teardown retirement deletion state has a non-contiguous absence at ConfigMap/%s", f.markers[index].Name)
		}
	}
	return present, nil
}

func (f *teardownRetirementFinalizer) getExactActivation(ctx context.Context) (*corev1.ConfigMap, error) {
	object, err := f.configMaps.Get(ctx, f.activationName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := verifyTeardownRetirementConfigMapIdentity(f.activationName, object, f.verifyActivation); err != nil {
		return nil, err
	}
	return object, nil
}

func verifyTeardownRetirementConfigMapIdentity(name string, object *corev1.ConfigMap, verify func(*corev1.ConfigMap) error) error {
	if object == nil || object.Name != name || object.UID == "" || object.ResourceVersion == "" {
		return fmt.Errorf("teardown retirement ConfigMap/%s has incomplete immutable deletion identity", name)
	}
	if err := verify(object); err != nil {
		return fmt.Errorf("verify teardown retirement ConfigMap/%s: %w", name, err)
	}
	return nil
}

func teardownRetirementDeleteOptions(object *corev1.ConfigMap) metav1.DeleteOptions {
	uid := object.UID
	resourceVersion := object.ResourceVersion
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}}
}
