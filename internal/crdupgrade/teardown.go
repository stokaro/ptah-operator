package crdupgrade

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
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
)

const hookIdentityProbeMarkerWeight = "-125"

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
// remove the ConfigMaps a release keeps across upgrades. Update returns the
// release activation to its bootstrap state before it is deleted.
type ConfigMapTeardownClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// SecretTeardownClient deletes the certificate staging Secret by name. It
// cannot read the Secret, which holds a pending CA private key.
type SecretTeardownClient interface {
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// ReleaseTeardown deletes, by exact name, what a release keeps outside Helm's
// own deletion. Helm is told to keep the admission guards and their bindings,
// the hook identity probe, the admission inventory marker, the parent-origin
// readiness marker and the release activation parameter across upgrades, so an
// uninstall that left them to Helm would leave them behind. The certificate
// staging Secret is an ordinary release object, but its own guard refuses a
// Helm deletion, so it is deleted here once that guard is gone.
//
// The caller stops the runtime first: no controller may run once the guards
// that fence its writes are gone.
type ReleaseTeardown struct {
	rollout    *RolloutGuard
	policies   ValidatingAdmissionPolicyTeardownClient
	bindings   ValidatingAdmissionPolicyBindingTeardownClient
	configMaps ConfigMapTeardownClient
	secrets    SecretTeardownClient
}

// NewReleaseTeardown constructs the deletion of a release's retained
// inventory. Every supplied client is used for both the check and the deletion
// so the two cannot run as different API identities.
func NewReleaseTeardown(
	rollout *RolloutGuard,
	policies ValidatingAdmissionPolicyTeardownClient,
	bindings ValidatingAdmissionPolicyBindingTeardownClient,
	configMaps ConfigMapTeardownClient,
	secrets SecretTeardownClient,
) *ReleaseTeardown {
	return &ReleaseTeardown{
		rollout:    rollout,
		policies:   policies,
		bindings:   bindings,
		configMaps: configMaps,
		secrets:    secrets,
	}
}

// Preflight reads every object of the inventory and checks the ones present
// against the contract this release compiles, without changing anything. An
// object that is already gone is accepted, so a retry after a partial
// uninstall passes. Callers run it before stopping the runtime, so an
// inventory this release cannot delete fails before any downtime.
func (t *ReleaseTeardown) Preflight(ctx context.Context) error {
	targets, err := t.targets()
	if err != nil {
		return err
	}
	return t.preflight(ctx, targets)
}

func (t *ReleaseTeardown) preflight(ctx context.Context, targets []teardownTarget) error {
	for _, target := range targets {
		if target.inspect == nil {
			continue
		}
		if err := t.retry(ctx, target, func(attemptCtx context.Context) (bool, error) {
			_, _, inspectErr := target.inspect(attemptCtx)
			return inspectErr == nil, inspectErr
		}); err != nil {
			return fmt.Errorf("preflight teardown %s/%s: %w", target.kind, target.name, err)
		}
	}
	return nil
}

// Teardown repeats the preflight, then deletes the inventory in order: every
// guard binding, then every guard policy, then the objects those guards
// protected, and the release activation guard and its parameter last. Each
// object is read again, checked, and deleted with the UID and resourceVersion
// of that read. An object already gone is skipped, so a retry after a partial
// run deletes the rest.
//
// A refusal from an admission policy is retried until the context ends: the
// guard that refused has just been deleted, and an API server may still
// evaluate its cached copy for a moment.
func (t *ReleaseTeardown) Teardown(ctx context.Context) error {
	targets, err := t.targets()
	if err != nil {
		return err
	}
	if err := t.preflight(ctx, targets); err != nil {
		return err
	}
	for _, target := range targets {
		if err := t.retry(ctx, target, func(attemptCtx context.Context) (bool, error) {
			options := metav1.DeleteOptions{}
			if target.inspect != nil {
				identity, found, inspectErr := target.inspect(attemptCtx)
				if inspectErr != nil {
					return false, inspectErr
				}
				if !found {
					return true, nil
				}
				if target.prepare != nil {
					prepared, prepareErr := target.prepare(attemptCtx)
					if prepareErr != nil {
						return false, prepareErr
					}
					identity = prepared
				}
				options = identity.deleteOptions()
			}
			deleteErr := target.delete(attemptCtx, options)
			if deleteErr == nil || apierrors.IsNotFound(deleteErr) {
				return true, nil
			}
			return false, deleteErr
		}); err != nil {
			return fmt.Errorf("delete teardown %s/%s: %w", target.kind, target.name, err)
		}
	}
	return nil
}

// retry runs attempt until it reports done, retrying only errors a later
// attempt can clear. Anything else, including an object that differs from its
// contract, stops the teardown at once.
func (t *ReleaseTeardown) retry(
	ctx context.Context,
	target teardownTarget,
	attempt func(context.Context) (bool, error),
) error {
	var last error
	err := wait.PollUntilContextCancel(ctx, t.rollout.PollEvery, true, func(attemptCtx context.Context) (bool, error) {
		done, attemptErr := attempt(attemptCtx)
		if attemptErr == nil {
			return done, nil
		}
		if !retryableTeardownError(attemptErr) {
			return false, attemptErr
		}
		last = attemptErr
		return false, nil
	})
	if err != nil && last != nil && ctx.Err() != nil {
		return fmt.Errorf("%w; the last attempt said: %w", err, last)
	}
	return err
}

// retryableTeardownError reports an error that says nothing about the object
// itself: a precondition that changed under the read, a refusal by an
// admission policy the teardown has just deleted, or an API server that did
// not answer.
func retryableTeardownError(err error) bool {
	switch {
	case apierrors.IsConflict(err),
		apierrors.IsServerTimeout(err),
		apierrors.IsTimeout(err),
		apierrors.IsTooManyRequests(err),
		apierrors.IsServiceUnavailable(err),
		apierrors.IsInternalError(err),
		apierrors.IsUnexpectedServerError(err):
		return true
	case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
		return strings.Contains(err.Error(), "ValidatingAdmissionPolicy")
	}
	return utilnet.IsConnectionRefused(err) || utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err)
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

// teardownTarget is one object of the inventory. A target without inspect is
// deleted by name without being read. prepare, where set, runs between the
// read and the delete and returns the identity the delete must match.
type teardownTarget struct {
	kind    string
	name    string
	inspect func(context.Context) (teardownIdentity, bool, error)
	prepare func(context.Context) (teardownIdentity, error)
	delete  func(context.Context, metav1.DeleteOptions) error
}

type teardownGuardContract struct {
	name          string
	parameterized bool
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

	targets := make([]teardownTarget, 0, len(contracts)*2+5)
	activationName := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	var activationContract *teardownGuardContract
	for _, contract := range contracts {
		if contract.name == activationName {
			candidate := contract
			activationContract = &candidate
			continue
		}
		if contract.parameterized {
			targets = append(targets, t.bindingTarget(contract))
		}
	}
	if activationContract == nil {
		return nil, fmt.Errorf("release activation teardown contract is missing")
	}
	for _, contract := range contracts {
		if contract.name != activationName && !contract.parameterized {
			targets = append(targets, t.bindingTarget(contract))
		}
	}
	for _, contract := range contracts {
		if contract.name != activationName {
			targets = append(targets, t.policyTarget(contract))
		}
	}
	if guard.CertificateRuntimeEnabled {
		staging, stagingErr := NewStagingSecretGuard(guard).contract()
		if stagingErr != nil {
			return nil, fmt.Errorf("derive certificate staging Secret: %w", stagingErr)
		}
		targets = append(targets, t.stagingSecretTarget(staging.stagingSecretName))
	}
	targets = append(targets, t.hookIdentityProbeMarkerTarget(guard))
	targets = append(targets, t.parentOriginReadinessMarkerTarget(NewParentWorkloadGuard(guard)))
	// Keep the activation self-guard bound until every earlier policy that
	// consults the parameter is unbound.
	targets = append(targets, t.bindingTarget(*activationContract))
	targets = append(targets, t.policyTarget(*activationContract))
	targets = append(targets, t.admissionConvergenceMarkerTarget(NewAdmissionConvergenceGuard(guard)))
	targets = append(targets, t.activationTarget(guard.releaseActivationGuard(), guard))
	return targets, nil
}

func (t *ReleaseTeardown) validatedGuard() (*RolloutGuard, error) {
	if t == nil || t.rollout == nil || t.policies == nil || t.bindings == nil || t.configMaps == nil || t.secrets == nil {
		return nil, fmt.Errorf("release teardown clients and rollout identity are required")
	}
	guard := *t.rollout
	guard.Policies = t.policies
	guard.Bindings = t.bindings
	if err := guard.validateIdentity(); err != nil {
		return nil, fmt.Errorf("validate release teardown identity: %w", err)
	}
	if guard.PollEvery <= 0 {
		return nil, errors.New("release teardown poll interval must be positive")
	}
	return &guard, nil
}

// releaseTeardownGuardNames lists every admission guard a release keeps: the
// names the uninstall deletes, and the names its ClusterRole may delete. Each
// guard's policy and binding share the name.
func releaseTeardownGuardNames(guard *RolloutGuard) []string {
	names := []string{
		ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName),
		RolloutGuardPolicyName(guard.ReleaseSequence),
		RuntimeGuardPolicyName(guard.ReleaseSequence),
		RuntimePodGuardPolicyName(guard.ReleaseSequence),
		HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ParentReplicaSetGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ParentHookJobOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName),
		ParentHookPodOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName),
		ParentHookJobContractPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ControllerJobWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ControllerChunkWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ControllerPlanWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		ControllerMigrationPlanWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
		CertificateMutatingWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName),
		CertificateValidatingWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName),
		NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName),
	}
	if guard.CertificateRuntimeEnabled {
		names = append(names, StagingSecretGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName))
	}
	return names
}

func teardownGuardContracts(guard *RolloutGuard) ([]teardownGuardContract, error) {
	activation := guard.releaseActivationGuard()
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
	controllerWriteName := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	namespaceName := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)

	if _, err := guard.runtimePodIdentityPolicy(); err != nil {
		return nil, fmt.Errorf("build release teardown runtime Pod contract: %w", err)
	}

	// This literal is the exact admission inventory this release installs and
	// keeps. Teardown never discovers what to delete from a label selector or
	// a name prefix.
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
	if guard.CertificateRuntimeEnabled {
		staging := NewStagingSecretGuard(guard)
		stagingPolicy, stagingBinding, stagingErr := staging.ExpectedObjects()
		if stagingErr != nil {
			return nil, fmt.Errorf("build release teardown staging Secret guard contract: %w", stagingErr)
		}
		contracts = append(contracts, teardownGuardContract{
			name: stagingPolicy.Name,
			verifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return staging.verifyPolicy(policy, stagingPolicy)
			},
			verifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return staging.verifyBinding(binding, stagingBinding)
			},
		})
	}
	want := make(map[string]bool, len(contracts))
	for _, name := range releaseTeardownGuardNames(guard) {
		want[name] = true
	}
	for index, contract := range contracts {
		if contract.name == "" || contract.verifyPolicy == nil || contract.verifyBinding == nil {
			return nil, fmt.Errorf("release teardown guard contract %d is incomplete", index)
		}
		if !want[contract.name] {
			return nil, fmt.Errorf("release teardown guard %s is missing from the named inventory", contract.name)
		}
		delete(want, contract.name)
	}
	if len(want) != 0 {
		return nil, fmt.Errorf("release teardown names %d guards it has no contract for", len(want))
	}
	return contracts, nil
}

func parentTeardownContract(entry parentGuardEntry) teardownGuardContract {
	return teardownGuardContract{
		name: entry.name, verifyPolicy: entry.verifyPolicy, verifyBinding: entry.verifyBinding,
	}
}

func (t *ReleaseTeardown) bindingTarget(contract teardownGuardContract) teardownTarget {
	const kind = "ValidatingAdmissionPolicyBinding"
	return teardownTarget{
		kind: kind, name: contract.name,
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
		kind: kind, name: contract.name,
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

// configMapTarget reads, checks and deletes one retained ConfigMap.
func (t *ReleaseTeardown) configMapTarget(name string, verify func(*corev1.ConfigMap) error) teardownTarget {
	const kind = "ConfigMap"
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
			if err := verify(object); err != nil {
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

func (t *ReleaseTeardown) stagingSecretTarget(name string) teardownTarget {
	return teardownTarget{
		kind: "Secret", name: name,
		delete: func(ctx context.Context, options metav1.DeleteOptions) error {
			return t.secrets.Delete(ctx, name, options)
		},
	}
}

func (t *ReleaseTeardown) admissionConvergenceMarkerTarget(guard *AdmissionConvergenceGuard) teardownTarget {
	name := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)
	return t.configMapTarget(name, guard.verifyMarker)
}

func (t *ReleaseTeardown) activationTarget(activation *ReleaseActivationGuard, guard *RolloutGuard) teardownTarget {
	verify := func(object *corev1.ConfigMap) error {
		if object.Namespace != guard.ReleaseNamespace {
			return fmt.Errorf("release activation ConfigMap is in namespace %q, expected %q", object.Namespace, guard.ReleaseNamespace)
		}
		identity, err := activation.verifyActivationObject(object)
		if err != nil {
			return err
		}
		return activation.verifyCandidateCompatibility(identity)
	}
	target := t.configMapTarget(ReleaseActivationName, verify)
	// Return the parameter to the state a fresh install starts from before it
	// is deleted. An API server can go on serving a policy parameter after it
	// is deleted, and a reinstall in this namespace needs what it serves to be
	// the bootstrap state rather than the sequence this release last
	// activated.
	target.prepare = func(ctx context.Context) (teardownIdentity, error) {
		object, err := t.configMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
		if err != nil {
			return teardownIdentity{}, fmt.Errorf("re-read release activation before its reset: %w", err)
		}
		if err := verify(object); err != nil {
			return teardownIdentity{}, err
		}
		bootstrap := ReleaseActivationBootstrapData()
		if !reflect.DeepEqual(object.Data, bootstrap) {
			reset := object.DeepCopy()
			reset.Data = bootstrap
			updated, updateErr := t.configMaps.Update(ctx, reset, metav1.UpdateOptions{})
			if updateErr != nil {
				return teardownIdentity{}, fmt.Errorf("return release activation to its bootstrap state: %w", updateErr)
			}
			object = updated
		}
		return deletionIdentity("ConfigMap", ReleaseActivationName, object)
	}
	return target
}

func (t *ReleaseTeardown) hookIdentityProbeMarkerTarget(guard *RolloutGuard) teardownTarget {
	name := HookIdentityProbeObjectName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	policyName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	return t.configMapTarget(name, func(object *corev1.ConfigMap) error {
		return verifyHookIdentityProbeMarker(object, guard, name, policyName)
	})
}

func (t *ReleaseTeardown) parentOriginReadinessMarkerTarget(guard *ParentWorkloadGuard) teardownTarget {
	name := ParentOriginReadyMarkerName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)
	return t.configMapTarget(name, guard.verifyReadinessMarker)
}

func verifyHookIdentityProbeMarker(object *corev1.ConfigMap, guard *RolloutGuard, name, policyName string) error {
	wantAnnotations := map[string]string{
		"helm.sh/hook":                           "pre-install,pre-upgrade",
		"helm.sh/hook-weight":                    hookIdentityProbeMarkerWeight,
		"helm.sh/resource-policy":                "keep",
		"operator.ptah.run/hook-identity-policy": policyName,
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
