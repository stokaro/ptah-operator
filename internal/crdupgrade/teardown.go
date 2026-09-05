package crdupgrade

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const hookIdentityProbeMarkerWeight = "-125"

// MutatingWebhookTeardownClient is the exact API surface required to verify
// and remove the release-owned mutating admission singleton.
type MutatingWebhookTeardownClient interface {
	Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.MutatingWebhookConfiguration, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// ValidatingWebhookTeardownClient is the exact API surface required to verify
// and remove the release-owned validating admission singleton.
type ValidatingWebhookTeardownClient interface {
	Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingWebhookConfiguration, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// ValidatingAdmissionPolicyTeardownClient is the exact API surface required
// to verify and remove the release-owned admission policies.
type ValidatingAdmissionPolicyTeardownClient interface {
	ValidatingAdmissionPolicyReader
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// ValidatingAdmissionPolicyBindingTeardownClient is the exact API surface
// required to verify and remove the release-owned admission policy bindings.
type ValidatingAdmissionPolicyBindingTeardownClient interface {
	ValidatingAdmissionPolicyBindingReader
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// ConfigMapTeardownClient is the exact API surface required to verify and
// remove the retained release-activation parameter.
type ConfigMapTeardownClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// ReleaseTeardown removes only the exact admission resources compiled into a
// release. The caller must invoke Preflight before quiescing both runtime
// Deployments, then prove that no protected runtime Pods remain before
// invoking Teardown.
type ReleaseTeardown struct {
	rollout    *RolloutGuard
	mutating   MutatingWebhookTeardownClient
	validating ValidatingWebhookTeardownClient
	policies   ValidatingAdmissionPolicyTeardownClient
	bindings   ValidatingAdmissionPolicyBindingTeardownClient
	configMaps ConfigMapTeardownClient
}

// NewReleaseTeardown constructs a fail-closed release admission teardown.
// Every supplied client is used for both the ownership check and deletion so
// callers cannot accidentally verify through a different API identity.
func NewReleaseTeardown(
	rollout *RolloutGuard,
	mutating MutatingWebhookTeardownClient,
	validating ValidatingWebhookTeardownClient,
	policies ValidatingAdmissionPolicyTeardownClient,
	bindings ValidatingAdmissionPolicyBindingTeardownClient,
	configMaps ConfigMapTeardownClient,
) *ReleaseTeardown {
	return &ReleaseTeardown{
		rollout:    rollout,
		mutating:   mutating,
		validating: validating,
		policies:   policies,
		bindings:   bindings,
		configMaps: configMaps,
	}
}

// Preflight performs the complete exact inventory, ownership, object-shape,
// and resumability check without mutating any object. Callers run it before
// quiescing the release so an unsafe teardown cannot cause avoidable downtime.
func (t *ReleaseTeardown) Preflight(ctx context.Context) error {
	_, _, err := t.preflight(ctx)
	return err
}

// Teardown repeats the complete preflight after runtime quiescence, then
// removes the inventory in a fail-safe order. After deleting the final
// sentinel binding, waitForAdmissionConvergence must prove on every directly
// addressed API server that no earlier retained VAP/VAPB pair remains active.
// Each object is re-read immediately before deletion and deleted with UID and
// resource-version preconditions.
func (t *ReleaseTeardown) Teardown(ctx context.Context, waitForAdmissionConvergence func(context.Context) error) error {
	if waitForAdmissionConvergence == nil {
		return fmt.Errorf("release teardown admission convergence waiter is required")
	}
	targets, present, err := t.preflight(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 || !anyTeardownTargetPresent(present) {
		return nil
	}

	for _, target := range targets {
		identity, found, inspectErr := target.inspect(ctx)
		if inspectErr != nil {
			return fmt.Errorf("re-verify teardown %s/%s: %w", target.kind, target.name, inspectErr)
		}
		if !found {
			// A NotFound is safe here only when the complete preflight either
			// observed this exact object or established that it belonged to the
			// already-deleted contiguous prefix.
			if target.admissionConvergenceBoundary {
				if waitErr := waitForAdmissionConvergence(ctx); waitErr != nil {
					return fmt.Errorf("wait for admission policy cache convergence: %w", waitErr)
				}
			}
			continue
		}
		deleteOptions := identity.deleteOptions()
		if deleteErr := target.delete(ctx, deleteOptions); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete teardown %s/%s: %w", target.kind, target.name, deleteErr)
		}
		if target.admissionConvergenceBoundary {
			if waitErr := waitForAdmissionConvergence(ctx); waitErr != nil {
				return fmt.Errorf("wait for admission policy cache convergence: %w", waitErr)
			}
		}
	}
	return nil
}

func (t *ReleaseTeardown) preflight(ctx context.Context) ([]teardownTarget, []bool, error) {
	targets, err := t.targets()
	if err != nil {
		return nil, nil, err
	}

	present := make([]bool, len(targets))
	for index, target := range targets {
		_, found, inspectErr := target.inspect(ctx)
		if inspectErr != nil {
			return nil, nil, fmt.Errorf("preflight teardown %s/%s: %w", target.kind, target.name, inspectErr)
		}
		present[index] = found
	}
	if !anyTeardownTargetPresent(present) {
		return targets, present, nil
	}
	if err := validateOptionalTeardownGroups(targets, present); err != nil {
		return nil, nil, err
	}
	// A retry may observe only a contiguous prefix already removed by an
	// earlier invocation. Optional predecessor groups keep their own cursor and
	// do not make an otherwise complete current-release inventory look sparse.
	seenPresent := false
	for index, found := range present {
		if targets[index].optionalGroup != "" {
			continue
		}
		if found {
			seenPresent = true
			continue
		}
		if seenPresent {
			return nil, nil, fmt.Errorf(
				"release teardown inventory is incomplete: %s/%s is missing after a retained object",
				targets[index].kind,
				targets[index].name,
			)
		}
	}
	return targets, present, nil
}

func validateOptionalTeardownGroups(targets []teardownTarget, present []bool) error {
	seenPresent := map[string]bool{}
	for index, target := range targets {
		if target.optionalGroup == "" {
			continue
		}
		if present[index] {
			seenPresent[target.optionalGroup] = true
			continue
		}
		if seenPresent[target.optionalGroup] {
			return fmt.Errorf(
				"release teardown optional inventory %s is incomplete: %s/%s is missing after a retained object",
				target.optionalGroup,
				target.kind,
				target.name,
			)
		}
	}
	return nil
}

func anyTeardownTargetPresent(present []bool) bool {
	for _, found := range present {
		if found {
			return true
		}
	}
	return false
}

type teardownIdentity struct {
	uid             types.UID
	resourceVersion string
}

func (i teardownIdentity) deleteOptions() metav1.DeleteOptions {
	uid := i.uid
	resourceVersion := i.resourceVersion
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID:             &uid,
		ResourceVersion: &resourceVersion,
	}}
}

type teardownTarget struct {
	kind                         string
	name                         string
	optionalGroup                string
	admissionConvergenceBoundary bool
	inspect                      func(context.Context) (teardownIdentity, bool, error)
	delete                       func(context.Context, metav1.DeleteOptions) error
}

type teardownGuardContract struct {
	name          string
	parameterized bool
	sentinel      bool
	optionalGroup string
	verifyPolicy  func(*admissionregistrationv1.ValidatingAdmissionPolicy) error
	verifyBinding func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error
}

func (t *ReleaseTeardown) targets() ([]teardownTarget, error) {
	guard, err := t.validatedGuard()
	if err != nil {
		return nil, err
	}
	contracts, err := teardownGuardContracts(guard)
	if err != nil {
		return nil, err
	}
	expectedAdmission := teardownRuntimeInvariants(guard)

	targets := make([]teardownTarget, 0, 2+len(contracts)*2+3)
	targets = append(targets,
		t.mutatingWebhookTarget(expectedAdmission),
		t.validatingWebhookTarget(expectedAdmission),
	)
	activationName := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	var activationContract *teardownGuardContract
	for _, contract := range contracts {
		if contract.name == activationName {
			candidate := contract
			activationContract = &candidate
			continue
		}
		if contract.parameterized && !contract.sentinel {
			targets = append(targets, t.bindingTarget(contract))
		}
	}
	if activationContract == nil {
		return nil, fmt.Errorf("release activation teardown contract is missing")
	}
	for _, contract := range contracts {
		if contract.name != activationName && !contract.parameterized && !contract.sentinel {
			targets = append(targets, t.bindingTarget(contract))
		}
	}
	for _, contract := range contracts {
		if contract.name != activationName && !contract.sentinel {
			targets = append(targets, t.policyTarget(contract))
		}
	}
	// Keep the activation self-guard bound until every earlier policy that
	// consults the parameter is unbound. The final sentinel binding is deleted
	// only after all earlier bindings and policies, establishing the cache-order
	// fence used by the direct endpoint probes.
	targets = append(targets, t.hookIdentityProbeMarkerTarget(guard))
	targets = append(targets, t.bindingTarget(*activationContract))
	targets = append(targets, t.policyTarget(*activationContract))
	var sentinelContract *teardownGuardContract
	for _, contract := range contracts {
		if contract.sentinel {
			candidate := contract
			sentinelContract = &candidate
			break
		}
	}
	if sentinelContract == nil {
		return nil, fmt.Errorf("release teardown admission convergence sentinel contract is missing")
	}
	sentinelBinding := t.bindingTarget(*sentinelContract)
	sentinelBinding.admissionConvergenceBoundary = true
	targets = append(targets, sentinelBinding)
	targets = append(targets, t.policyTarget(*sentinelContract))
	targets = append(targets, t.admissionConvergenceMarkerTarget(NewAdmissionConvergenceGuard(guard)))
	targets = append(targets, t.activationTarget(guard.releaseActivationGuard(), guard))
	return targets, nil
}

func (t *ReleaseTeardown) validatedGuard() (*RolloutGuard, error) {
	if t == nil || t.rollout == nil || t.mutating == nil || t.validating == nil ||
		t.policies == nil || t.bindings == nil || t.configMaps == nil {
		return nil, fmt.Errorf("release teardown clients and rollout identity are required")
	}
	guard := *t.rollout
	guard.Policies = t.policies
	guard.Bindings = t.bindings
	if err := guard.validateIdentity(); err != nil {
		return nil, fmt.Errorf("validate release teardown identity: %w", err)
	}
	if guard.ReleaseSequence != 1 {
		return nil, fmt.Errorf("release teardown sequence %d has no explicit predecessor identity inventory; refusing incomplete cleanup", guard.ReleaseSequence)
	}
	return &guard, nil
}

func teardownGuardContracts(guard *RolloutGuard) ([]teardownGuardContract, error) {
	activation := guard.releaseActivationGuard()
	serviceAccountObject := NewServiceAccountObjectGuard(guard)
	serviceAccount := NewServiceAccountOriginGuard(guard)
	controllerWrite := NewControllerWriteGuard(guard)
	controllerObjects := NewControllerObjectGuard(guard)
	certificateWrite := NewCertificateWriteGuard(guard)
	parentEntries := NewParentWorkloadGuard(guard).entries()
	parentByName := make(map[string]parentGuardEntry, len(parentEntries))
	for _, entry := range parentEntries {
		parentByName[entry.name] = entry
	}
	namespace := NewNamespaceDeletionGuard(guard)

	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	runtimePodName := RuntimePodGuardPolicyName(guard.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	activationName := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentReplicaSetName := ParentReplicaSetGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	parentHookOriginName := ParentHookJobOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookPodOriginName := ParentHookPodOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookContractName := ParentHookJobContractPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	serviceAccountName := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	serviceAccountObjectName := ServiceAccountObjectGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	controllerWriteName := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	namespaceName := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)

	if _, err := guard.runtimePodIdentityPolicy(); err != nil {
		return nil, fmt.Errorf("build release teardown runtime Pod contract: %w", err)
	}
	serviceAccountObjectPolicy, serviceAccountObjectBinding, err := serviceAccountObject.ExpectedObjects()
	if err != nil {
		return nil, fmt.Errorf("build release teardown ServiceAccount object contract: %w", err)
	}

	// This literal is the exact admission inventory known by this release.
	// When the release sequence advances, predecessor-generation contracts are
	// appended here explicitly; teardown must never discover deletion targets
	// from a label selector or an unbounded name prefix.
	contracts := []teardownGuardContract{
		{
			name: activationName, parameterized: true,
			verifyPolicy: activation.verifyPolicy, verifyBinding: activation.verifyBinding,
		},
		{
			name: rolloutName, parameterized: true,
			verifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				_, _, verifyErr := guard.verifyPolicy(policy)
				return verifyErr
			},
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return guard.verifyBinding(binding, rolloutName)
			},
		},
		{
			name: runtimeName, parameterized: true,
			verifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				_, _, _, verifyErr := guard.verifyRuntimePolicy(policy)
				return verifyErr
			},
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return guard.verifyBinding(binding, runtimeName)
			},
		},
		{
			name: runtimePodName, parameterized: true,
			verifyPolicy: guard.verifyRuntimePodIdentityPolicy, verifyBinding: guard.verifyRuntimePodIdentityBinding,
		},
		{
			name:         hookName,
			verifyPolicy: guard.verifyHookIdentityPolicy,
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return guard.verifyBinding(binding, hookName)
			},
		},
		{
			name:         hookProbeName,
			verifyPolicy: guard.verifyHookIdentityProbePolicy,
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return guard.verifyBinding(binding, hookProbeName)
			},
		},
		parentTeardownContract(parentByName[parentReplicaSetName]),
		parentTeardownContract(parentByName[parentHookOriginName]),
		parentTeardownContract(parentByName[parentHookPodOriginName]),
		parentTeardownContract(parentByName[parentHookContractName]),
		{
			name: serviceAccountObjectName,
			verifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return serviceAccountObject.verifyPolicy(policy, serviceAccountObjectPolicy)
			},
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return serviceAccountObject.verifyBinding(binding, serviceAccountObjectBinding)
			},
		},
		{
			name: serviceAccountName, parameterized: true,
			verifyPolicy: serviceAccount.verifyPolicy, verifyBinding: serviceAccount.verifyBinding,
		},
		{
			name: controllerWriteName, parameterized: true,
			verifyPolicy: controllerWrite.verifyPolicy, verifyBinding: controllerWrite.verifyBinding,
		},
	}
	for _, entry := range controllerObjects.entries() {
		entry := entry
		contracts = append(contracts, teardownGuardContract{
			name: entry.name, parameterized: true,
			verifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return controllerObjects.verifyPolicy(entry, policy)
			},
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return controllerObjects.verifyBinding(entry, binding)
			},
		})
	}
	contracts = append(contracts, legacyControllerTeardownContracts(guard)...)
	for _, entry := range certificateWrite.entries() {
		entry := entry
		contracts = append(contracts, teardownGuardContract{
			name: entry.name,
			verifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return certificateWrite.verifyPolicy(entry, policy)
			},
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return certificateWrite.verifyBinding(entry, binding)
			},
		})
	}
	contracts = append(contracts, teardownGuardContract{
		name:         namespaceName,
		verifyPolicy: namespace.verifyPolicy, verifyBinding: namespace.verifyBinding,
	})
	admissionConvergence := NewAdmissionConvergenceGuard(guard)
	contracts = append(contracts, teardownGuardContract{
		name: AdmissionConvergencePolicyName(guard.ReleaseNamespace, guard.ReleaseName), sentinel: true,
		verifyPolicy: admissionConvergence.verifyPolicy, verifyBinding: admissionConvergence.verifyBinding,
	})
	for index, contract := range contracts {
		if contract.name == "" || contract.verifyPolicy == nil || contract.verifyBinding == nil {
			return nil, fmt.Errorf("release teardown guard contract %d is incomplete", index)
		}
	}
	return contracts, nil
}

func parentTeardownContract(entry parentGuardEntry) teardownGuardContract {
	return teardownGuardContract{
		name: entry.name, verifyPolicy: entry.verifyPolicy, verifyBinding: entry.verifyBinding,
	}
}

func teardownRuntimeInvariants(guard *RolloutGuard) RuntimeInvariants {
	return RuntimeInvariants{
		ReleaseName:                  guard.ReleaseName,
		ReleaseNamespace:             guard.ReleaseNamespace,
		CoordinationNamespace:        guard.CoordinationNamespace,
		LeaderElection:               guard.LeaderElection,
		LeaderElectionID:             guard.LeaderElectionID,
		WebhookServiceName:           guard.WebhookServiceName,
		WebhookTimeoutSeconds:        guard.WebhookTimeoutSeconds,
		HookServiceAccountName:       guard.HookServiceAccountName,
		ControllerServiceAccountName: guard.ControllerServiceAccountName,
		ControllerDeploymentName:     guard.ControllerDeploymentName,
		CertificateDeploymentName:    guard.CertificateDeploymentName,
		ControllerStateVersion:       guard.ControllerStateVersion,
		AdmissionContractVersion:     guard.AdmissionContractVersion,
		ReleaseSequence:              guard.ReleaseSequence,
	}
}

func (t *ReleaseTeardown) mutatingWebhookTarget(expected RuntimeInvariants) teardownTarget {
	const kind = "MutatingWebhookConfiguration"
	return teardownTarget{
		kind: kind, name: AdmissionConfigurationName,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.mutating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			if err := verifyHelmOwnership(kind, object.ObjectMeta, expected); err != nil {
				return teardownIdentity{}, false, err
			}
			if err := verifyAnnotations(kind, object.Name, object.Annotations, expected.annotations()); err != nil {
				return teardownIdentity{}, false, err
			}
			if err := verifyMutatingWebhookContract(object, expected); err != nil {
				return teardownIdentity{}, false, err
			}
			identity, err := deletionIdentity(kind, AdmissionConfigurationName, object)
			return identity, true, err
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.mutating.Delete(ctx, AdmissionConfigurationName, options)
		},
	}
}

func (t *ReleaseTeardown) validatingWebhookTarget(expected RuntimeInvariants) teardownTarget {
	const kind = "ValidatingWebhookConfiguration"
	return teardownTarget{
		kind: kind, name: AdmissionConfigurationName,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.validating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			if err := verifyHelmOwnership(kind, object.ObjectMeta, expected); err != nil {
				return teardownIdentity{}, false, err
			}
			if err := verifyAnnotations(kind, object.Name, object.Annotations, expected.annotations()); err != nil {
				return teardownIdentity{}, false, err
			}
			if err := verifyValidatingWebhookContract(object, expected); err != nil {
				return teardownIdentity{}, false, err
			}
			identity, err := deletionIdentity(kind, AdmissionConfigurationName, object)
			return identity, true, err
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.validating.Delete(ctx, AdmissionConfigurationName, options)
		},
	}
}

func (t *ReleaseTeardown) bindingTarget(contract teardownGuardContract) teardownTarget {
	const kind = "ValidatingAdmissionPolicyBinding"
	return teardownTarget{
		kind: kind, name: contract.name, optionalGroup: contract.optionalGroup,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.bindings.Get(ctx, contract.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			if err := contract.verifyBinding(object); err != nil {
				return teardownIdentity{}, false, err
			}
			identity, err := deletionIdentity(kind, contract.name, object)
			return identity, true, err
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.bindings.Delete(ctx, contract.name, options)
		},
	}
}

func (t *ReleaseTeardown) policyTarget(contract teardownGuardContract) teardownTarget {
	const kind = "ValidatingAdmissionPolicy"
	return teardownTarget{
		kind: kind, name: contract.name, optionalGroup: contract.optionalGroup,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.policies.Get(ctx, contract.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			if err := contract.verifyPolicy(object); err != nil {
				return teardownIdentity{}, false, err
			}
			identity, err := deletionIdentity(kind, contract.name, object)
			return identity, true, err
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.policies.Delete(ctx, contract.name, options)
		},
	}
}

func (t *ReleaseTeardown) admissionConvergenceMarkerTarget(guard *AdmissionConvergenceGuard) teardownTarget {
	const kind = "ConfigMap"
	name := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)
	return teardownTarget{
		kind: kind, name: name,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.configMaps.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			if err := guard.verifyMarker(object); err != nil {
				return teardownIdentity{}, false, err
			}
			identity, err := deletionIdentity(kind, name, object)
			return identity, true, err
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.configMaps.Delete(ctx, name, options)
		},
	}
}

func (t *ReleaseTeardown) activationTarget(activation *ReleaseActivationGuard, guard *RolloutGuard) teardownTarget {
	const kind = "ConfigMap"
	return teardownTarget{
		kind: kind, name: ReleaseActivationName,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.configMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			identity, err := activation.verifyActivationObject(object)
			if err != nil {
				return teardownIdentity{}, false, err
			}
			if err := activation.verifyCandidateCompatibility(identity); err != nil {
				return teardownIdentity{}, false, err
			}
			deleteIdentity, err := deletionIdentity(kind, ReleaseActivationName, object)
			if err != nil {
				return teardownIdentity{}, false, err
			}
			if object.Namespace != guard.ReleaseNamespace {
				return teardownIdentity{}, false, fmt.Errorf("release activation ConfigMap is in namespace %q, expected %q", object.Namespace, guard.ReleaseNamespace)
			}
			return deleteIdentity, true, nil
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			var lastCacheDenial error
			err := wait.PollUntilContextCancel(ctx, guard.PollEvery, true, func(pollCtx context.Context) (bool, error) {
				deleteErr := t.configMaps.Delete(pollCtx, ReleaseActivationName, options)
				if deleteErr == nil || apierrors.IsNotFound(deleteErr) {
					return true, nil
				}
				if !apierrors.IsForbidden(deleteErr) || !strings.Contains(deleteErr.Error(), releaseActivationGuardDenialMessage()) {
					return false, deleteErr
				}
				// The binding and policy were deleted immediately before this
				// target, but admission may still evaluate a stale cached copy.
				// Retry only its exact denial and keep the original UID/resource-
				// version preconditions on every attempt.
				lastCacheDenial = deleteErr
				return false, nil
			})
			if err != nil && lastCacheDenial != nil {
				return fmt.Errorf("wait for release activation self-guard cache propagation after %v: %w", lastCacheDenial, err)
			}
			return err
		},
	}
}

func (t *ReleaseTeardown) hookIdentityProbeMarkerTarget(guard *RolloutGuard) teardownTarget {
	const kind = "ConfigMap"
	name := HookIdentityProbeObjectName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	policyName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	return teardownTarget{
		kind: kind, name: name,
		inspect: func(ctx context.Context) (teardownIdentity, bool, error) {
			object, err := t.configMaps.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return teardownIdentity{}, false, nil
			}
			if err != nil {
				return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
			}
			if object == nil {
				return teardownIdentity{}, false, fmt.Errorf("API returned a nil object")
			}
			if err := verifyHookIdentityProbeMarker(object, guard, name, policyName); err != nil {
				return teardownIdentity{}, false, err
			}
			identity, err := deletionIdentity(kind, name, object)
			return identity, true, err
		},
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.configMaps.Delete(ctx, name, options)
		},
	}
}

func verifyHookIdentityProbeMarker(object *corev1.ConfigMap, guard *RolloutGuard, name, policyName string) error {
	wantAnnotations := map[string]string{
		"helm.sh/hook":                           "pre-install,pre-upgrade",
		"helm.sh/hook-weight":                    hookIdentityProbeMarkerWeight,
		"helm.sh/resource-policy":                "keep",
		"operator.ptah.dev/hook-identity-policy": policyName,
	}
	wantLabels := map[string]string{
		managedByLabel:                rolloutGuardManagedBy,
		instanceLabel:                 guard.ReleaseName,
		"app.kubernetes.io/component": "hook-identity-probe",
	}
	if object.Name != name || object.Namespace != guard.ReleaseNamespace || object.GenerateName != "" ||
		!reflect.DeepEqual(object.Annotations, wantAnnotations) || !reflect.DeepEqual(object.Labels, wantLabels) {
		return fmt.Errorf("hook identity probe marker ConfigMap/%s has foreign or incomplete ownership", name)
	}
	if !reflect.DeepEqual(object.Data, map[string]string{"probe": "ready-for-denial-proof"}) ||
		len(object.BinaryData) != 0 || (object.Immutable != nil && *object.Immutable) ||
		len(object.OwnerReferences) != 0 || len(object.Finalizers) != 0 {
		return fmt.Errorf("hook identity probe marker ConfigMap/%s data and metadata shape is not exact", name)
	}
	return nil
}

func deletionIdentity(kind, name string, object metav1.Object) (teardownIdentity, error) {
	if object.GetName() != name {
		return teardownIdentity{}, fmt.Errorf("%s has name %q, expected %q", kind, object.GetName(), name)
	}
	if object.GetDeletionTimestamp() != nil {
		return teardownIdentity{}, fmt.Errorf("%s/%s deletion is already in progress", kind, name)
	}
	if grace := object.GetDeletionGracePeriodSeconds(); grace != nil && *grace != 0 {
		return teardownIdentity{}, fmt.Errorf("%s/%s has a nonzero deletion grace period", kind, name)
	}
	if len(object.GetFinalizers()) != 0 {
		return teardownIdentity{}, fmt.Errorf("%s/%s has finalizers", kind, name)
	}
	if len(object.GetOwnerReferences()) != 0 {
		return teardownIdentity{}, fmt.Errorf("%s/%s has unexpected owner references", kind, name)
	}
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		return teardownIdentity{}, fmt.Errorf("%s/%s lacks a deletion UID or resource version", kind, name)
	}
	return teardownIdentity{uid: object.GetUID(), resourceVersion: object.GetResourceVersion()}, nil
}
