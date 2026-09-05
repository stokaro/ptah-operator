package crdupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ControllerRBACClient is the exact API surface used to inventory and move the
// stable controller bindings. The namespace arguments keep a NamespaceAll
// inventory separate from exact namespaced reads and patches.
type ControllerRBACClient interface {
	ListRoleBindings(context.Context, metav1.ListOptions) (*rbacv1.RoleBindingList, error)
	ListClusterRoleBindings(context.Context, metav1.ListOptions) (*rbacv1.ClusterRoleBindingList, error)
	GetRole(context.Context, string, string, metav1.GetOptions) (*rbacv1.Role, error)
	GetClusterRole(context.Context, string, metav1.GetOptions) (*rbacv1.ClusterRole, error)
	GetServiceAccount(context.Context, string, string, metav1.GetOptions) (*corev1.ServiceAccount, error)
	PatchRoleBinding(context.Context, string, string, types.PatchType, []byte, metav1.PatchOptions) (*rbacv1.RoleBinding, error)
	PatchClusterRoleBinding(context.Context, string, types.PatchType, []byte, metav1.PatchOptions) (*rbacv1.ClusterRoleBinding, error)
}

// ControllerRBACTransition moves stable bindings from one immutable
// controller ServiceAccount to the next. It never creates a binding or role.
// The current binary contains an exact contract only for the supported
// release-sequence-zero predecessor; the state-machine representation already
// supports the additional runtime-admission binding introduced in sequence 1.
type ControllerRBACTransition struct {
	rollout   *RolloutGuard
	client    ControllerRBACClient
	contract  controllerRBACTransitionContract
	preflight *controllerRBACTransitionState
	complete  *controllerRBACTransitionState
}

type controllerRBACTransitionContract struct {
	bindings         []controllerRBACBindingContract
	postApplyBinding *controllerRBACBindingContract
	roles            []controllerRBACRoleContract
	postApplyRole    *controllerRBACRoleContract
}

type controllerRBACBindingContract struct {
	name          string
	namespace     string
	roleRef       rbacv1.RoleRef
	cluster       bool
	fixedSubjects []rbacv1.Subject
}

type controllerRBACRoleContract struct {
	name             string
	namespace        string
	cluster          bool
	predecessorRules []rbacv1.PolicyRule
	candidateRules   []rbacv1.PolicyRule
}

type controllerRBACBindingState struct {
	contract        controllerRBACBindingContract
	uid             types.UID
	resourceVersion string
	subject         rbacv1.Subject
}

type controllerRBACTransitionState struct {
	cursor                         int
	coreComplete                   bool
	bindings                       []controllerRBACBindingState
	bindingUIDs                    map[string]types.UID
	bindingResourceVersions        map[string]string
	roleUIDs                       map[string]types.UID
	roleResourceVersions           map[string]string
	serviceAccountUIDs             map[string]types.UID
	serviceAccountResourceVersions map[string]string
}

// NewControllerRBACTransition constructs the fail-closed binding transition.
func NewControllerRBACTransition(
	rollout *RolloutGuard,
	runtimeContract RuntimeAdmissionContract,
	client ControllerRBACClient,
) (*ControllerRBACTransition, error) {
	transition := &ControllerRBACTransition{rollout: cloneControllerRBACRollout(rollout), client: client}
	if err := transition.validateIdentity(); err != nil {
		return nil, err
	}
	contract, err := controllerRBACContract(transition.rollout, runtimeContract)
	if err != nil {
		return nil, err
	}
	transition.contract = contract
	return transition, nil
}

func cloneControllerRBACRollout(rollout *RolloutGuard) *RolloutGuard {
	if rollout == nil {
		return nil
	}
	clone := *rollout
	clone.ControllerArgs = append([]string(nil), rollout.ControllerArgs...)
	clone.CertificateArgs = append([]string(nil), rollout.CertificateArgs...)
	clone.RuntimeDeploymentConfigExpressions = append([]string(nil), rollout.RuntimeDeploymentConfigExpressions...)
	clone.RuntimePodConfigExpressions = append([]string(nil), rollout.RuntimePodConfigExpressions...)
	return &clone
}

// HasPredecessor reports whether this release must revoke an installed
// predecessor controller identity.
func (t *ControllerRBACTransition) HasPredecessor() bool {
	return t != nil && t.rollout != nil && t.rollout.PreviousControllerServiceAccountName != ""
}

// RequiresCredentialGrace decides from the verified activation state and the
// durable preflight inventory whether controller credentials may already have
// been issued. The sole no-grace case is a pristine managed bootstrap whose
// candidate ServiceAccount and every candidate grant are still absent.
func (t *ControllerRBACTransition) RequiresCredentialGrace(activation ReleaseActivationState, protectedPodsRemain bool) (bool, error) {
	if t == nil || t.rollout == nil {
		return false, errors.New("controller RBAC transition is nil")
	}
	if t.preflight == nil {
		return false, errors.New("controller RBAC transition preflight has not completed")
	}
	switch activation.ControllerCredentialPhase {
	case ControllerCredentialsActive:
		if activation.DrainTargetReleaseSequence != 0 || activation.DrainAttempt != "" {
			return false, errors.New("active controller credential state contains drain identity")
		}
	case ControllerCredentialsDraining:
		wantAttempt := hookIdentityDigest(
			t.rollout.ReleaseNamespace,
			t.rollout.ReleaseName,
			t.rollout.ReleaseSequence,
			t.rollout.ManagerImage,
		)
		if activation.DrainTargetReleaseSequence != t.rollout.ReleaseSequence || activation.DrainAttempt != wantAttempt {
			return false, errors.New("controller credential drain state differs from the candidate attempt")
		}
		return true, nil
	default:
		return false, fmt.Errorf("controller credential phase %q is invalid", activation.ControllerCredentialPhase)
	}
	if activation.ActiveReleaseSequence < 0 || activation.ActiveReleaseSequence > t.rollout.ReleaseSequence {
		return false, fmt.Errorf("active controller release sequence %d is outside the candidate contract", activation.ActiveReleaseSequence)
	}
	if activation.ActiveReleaseSequence != 0 || t.HasPredecessor() || !t.rollout.ControllerServiceAccountManaged || protectedPodsRemain {
		return true, nil
	}
	candidateKey := privilegeServiceAccountKey(t.rollout.ReleaseNamespace, t.rollout.ControllerServiceAccountName)
	if _, found := t.preflight.serviceAccountUIDs[candidateKey]; found || len(t.preflight.bindingUIDs) != 0 {
		return true, nil
	}
	return false, nil
}

// Preflight verifies the complete binding inventory and every referenced role
// without changing cluster state.
func (t *ControllerRBACTransition) Preflight(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}
	state, err := t.inspect(ctx)
	if err == nil {
		t.preflight = cloneControllerRBACTransitionState(state)
	}
	return err
}

// Transition advances each stable binding exactly once in the contract order.
// Every step is first dry-run, rebuilt from a fresh full inventory, persisted,
// and then confirmed by another full inventory. A lost successful response is
// accepted only when the observed cursor advanced by exactly one.
func (t *ControllerRBACTransition) Transition(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}
	if t.preflight == nil {
		return errors.New("controller RBAC transition preflight has not completed")
	}
	initial, err := t.inspect(ctx)
	if err != nil {
		return err
	}
	if !controllerRBACStateIdentityEqual(initial, t.preflight) {
		return errors.New("controller RBAC identity changed after preflight")
	}
	for {
		state := initial
		if !t.HasPredecessor() || state.cursor == len(t.contract.bindings) {
			t.complete = cloneControllerRBACTransitionState(state)
			return nil
		}
		if err := t.advance(ctx, state); err != nil {
			return err
		}
		initial, err = t.inspect(ctx)
		if err != nil {
			return err
		}
	}
}

// VerifyComplete rechecks the exact all-candidate state and proves that none
// of the roles or bindings used for the authorization convergence proof was
// replaced before release activation.
func (t *ControllerRBACTransition) VerifyComplete(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}
	if t.complete == nil {
		return errors.New("controller RBAC transition has not completed")
	}
	state, err := t.inspect(ctx)
	if err != nil {
		return err
	}
	if t.HasPredecessor() && state.cursor != len(t.contract.bindings) {
		return fmt.Errorf("controller RBAC transition cursor regressed to %d of %d", state.cursor, len(t.contract.bindings))
	}
	if !reflect.DeepEqual(state.bindingUIDs, t.complete.bindingUIDs) {
		return errors.New("controller RBAC binding UIDs changed after transition")
	}
	if !reflect.DeepEqual(state.bindingResourceVersions, t.complete.bindingResourceVersions) {
		return errors.New("controller RBAC binding resourceVersions changed after transition")
	}
	if !reflect.DeepEqual(state.roleUIDs, t.complete.roleUIDs) {
		return errors.New("controller RBAC role UIDs changed after transition")
	}
	if !reflect.DeepEqual(state.roleResourceVersions, t.complete.roleResourceVersions) {
		return errors.New("controller RBAC role resourceVersions changed after transition")
	}
	if !reflect.DeepEqual(state.serviceAccountUIDs, t.complete.serviceAccountUIDs) ||
		!reflect.DeepEqual(state.serviceAccountResourceVersions, t.complete.serviceAccountResourceVersions) {
		return errors.New("controller ServiceAccount identity changed after transition")
	}
	return nil
}

// PredecessorAuthorizationProbe returns one canonical predecessor subject and
// one check for every distinct grant in the exact predecessor role contracts.
func (t *ControllerRBACTransition) PredecessorAuthorizationProbe() (AuthorizationProbe, error) {
	if err := t.validate(); err != nil {
		return AuthorizationProbe{}, err
	}
	if !t.HasPredecessor() {
		return AuthorizationProbe{}, errors.New("controller RBAC transition has no predecessor")
	}
	checks, err := controllerRBACAuthorizationChecks(t.rollout, t.contract.roles)
	if err != nil {
		return AuthorizationProbe{}, err
	}
	return AuthorizationProbe{
		Subject: AuthorizationSubject{
			Name: "previous-controller",
			User: "system:serviceaccount:" + t.rollout.ReleaseNamespace + ":" + t.rollout.PreviousControllerServiceAccountName,
			UID:  string(t.rollout.PreviousControllerServiceAccountUID),
			Groups: []string{
				"system:serviceaccounts",
				"system:serviceaccounts:" + t.rollout.ReleaseNamespace,
				"system:authenticated",
			},
		},
		Checks: checks,
	}, nil
}

func (t *ControllerRBACTransition) advance(ctx context.Context, before *controllerRBACTransitionState) error {
	target := before.bindings[before.cursor]
	dryPatch, err := controllerRBACSubjectPatch(target, t.candidateSubject())
	if err != nil {
		return err
	}
	dryResult, err := t.patchBinding(ctx, target.contract, dryPatch, metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return fmt.Errorf("dry-run controller RBAC transition for %s: %w", controllerRBACBindingDescription(target.contract), err)
	}
	if controllerRBACPatchResultIsNil(dryResult) {
		return fmt.Errorf("dry-run controller RBAC transition for %s returned a nil object", controllerRBACBindingDescription(target.contract))
	}

	refreshed, err := t.inspect(ctx)
	if err != nil {
		return fmt.Errorf("rebuild controller RBAC transition after dry-run: %w", err)
	}
	if refreshed.cursor != before.cursor {
		return fmt.Errorf("controller RBAC transition cursor changed from %d to %d during dry-run", before.cursor, refreshed.cursor)
	}
	if !reflect.DeepEqual(refreshed.bindingUIDs, before.bindingUIDs) ||
		!reflect.DeepEqual(refreshed.bindingResourceVersions, before.bindingResourceVersions) ||
		!reflect.DeepEqual(refreshed.roleUIDs, before.roleUIDs) ||
		!reflect.DeepEqual(refreshed.roleResourceVersions, before.roleResourceVersions) ||
		!reflect.DeepEqual(refreshed.serviceAccountUIDs, before.serviceAccountUIDs) ||
		!reflect.DeepEqual(refreshed.serviceAccountResourceVersions, before.serviceAccountResourceVersions) {
		return errors.New("controller RBAC role, binding, or ServiceAccount identity changed during dry-run")
	}
	target = refreshed.bindings[refreshed.cursor]
	patch, err := controllerRBACSubjectPatch(target, t.candidateSubject())
	if err != nil {
		return err
	}
	result, patchErr := t.patchBinding(ctx, target.contract, patch, metav1.PatchOptions{})
	after, inspectErr := t.inspect(ctx)
	if inspectErr != nil {
		if patchErr != nil {
			return errors.Join(fmt.Errorf("persist controller RBAC transition: %w", patchErr), fmt.Errorf("verify controller RBAC transition: %w", inspectErr))
		}
		return fmt.Errorf("verify controller RBAC transition: %w", inspectErr)
	}
	if after.cursor != refreshed.cursor+1 {
		if patchErr != nil && after.cursor == refreshed.cursor {
			return fmt.Errorf("persist controller RBAC transition for %s: %w", controllerRBACBindingDescription(target.contract), patchErr)
		}
		return fmt.Errorf("controller RBAC transition cursor advanced from %d to %d, want exactly %d", refreshed.cursor, after.cursor, refreshed.cursor+1)
	}
	if !reflect.DeepEqual(after.bindingUIDs, refreshed.bindingUIDs) ||
		!reflect.DeepEqual(after.roleUIDs, refreshed.roleUIDs) ||
		!controllerRBACBindingResourceVersionsAdvancedExactlyOne(after, refreshed, controllerRBACBindingKey(target.contract)) ||
		!reflect.DeepEqual(after.roleResourceVersions, refreshed.roleResourceVersions) ||
		!reflect.DeepEqual(after.serviceAccountUIDs, refreshed.serviceAccountUIDs) ||
		!reflect.DeepEqual(after.serviceAccountResourceVersions, refreshed.serviceAccountResourceVersions) {
		return errors.New("controller RBAC role, binding, or ServiceAccount identity changed while advancing transition")
	}
	if patchErr == nil && controllerRBACPatchResultIsNil(result) {
		return fmt.Errorf("persist controller RBAC transition for %s returned a nil object", controllerRBACBindingDescription(target.contract))
	}
	return nil
}

func controllerRBACPatchResultIsNil(result any) bool {
	if result == nil {
		return true
	}
	value := reflect.ValueOf(result)
	return (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil()
}

func (t *ControllerRBACTransition) patchBinding(
	ctx context.Context,
	contract controllerRBACBindingContract,
	patch []byte,
	options metav1.PatchOptions,
) (any, error) {
	if contract.cluster {
		return t.client.PatchClusterRoleBinding(ctx, contract.name, types.JSONPatchType, patch, options)
	}
	return t.client.PatchRoleBinding(ctx, contract.namespace, contract.name, types.JSONPatchType, patch, options)
}

func (t *ControllerRBACTransition) inspect(ctx context.Context) (*controllerRBACTransitionState, error) {
	roleBindings, err := listControllerRoleBindings(ctx, t.client)
	if err != nil {
		return nil, err
	}
	clusterBindings, err := listControllerClusterRoleBindings(ctx, t.client)
	if err != nil {
		return nil, err
	}

	protected := controllerRBACProtectedSubjects(t.rollout)
	bindingCapacity := len(t.contract.bindings)
	if t.contract.postApplyBinding != nil {
		bindingCapacity++
	}
	bindingsByKey := make(map[string]controllerRBACBindingState, bindingCapacity)
	contracts := make(map[string]controllerRBACBindingContract, bindingCapacity)
	for _, contract := range t.contract.bindings {
		contracts[controllerRBACBindingKey(contract)] = contract
	}
	if t.contract.postApplyBinding != nil {
		contracts[controllerRBACBindingKey(*t.contract.postApplyBinding)] = *t.contract.postApplyBinding
	}
	for index := range roleBindings {
		binding := &roleBindings[index]
		key := controllerRBACObjectKey(false, binding.Namespace, binding.Name)
		contract, expected := contracts[key]
		if !expected {
			if privilegeBindingTouchesProtected(binding.Subjects, protected) {
				return nil, fmt.Errorf("foreign RoleBinding/%s/%s names a protected controller ServiceAccount", binding.Namespace, binding.Name)
			}
			continue
		}
		state, err := t.verifyRoleBinding(binding, contract)
		if err != nil {
			return nil, err
		}
		bindingsByKey[key] = state
	}
	for index := range clusterBindings {
		binding := &clusterBindings[index]
		key := controllerRBACObjectKey(true, "", binding.Name)
		contract, expected := contracts[key]
		if !expected {
			if privilegeBindingTouchesProtected(binding.Subjects, protected) {
				return nil, fmt.Errorf("foreign ClusterRoleBinding/%s names a protected controller ServiceAccount", binding.Name)
			}
			continue
		}
		state, err := t.verifyClusterRoleBinding(binding, contract)
		if err != nil {
			return nil, err
		}
		bindingsByKey[key] = state
	}

	state := &controllerRBACTransitionState{
		bindings:                       make([]controllerRBACBindingState, 0, len(t.contract.bindings)),
		bindingUIDs:                    make(map[string]types.UID, len(t.contract.bindings)),
		bindingResourceVersions:        make(map[string]string, len(t.contract.bindings)),
		roleUIDs:                       make(map[string]types.UID, len(t.contract.roles)),
		roleResourceVersions:           make(map[string]string, len(t.contract.roles)),
		serviceAccountUIDs:             make(map[string]types.UID, 2),
		serviceAccountResourceVersions: make(map[string]string, 2),
	}
	seenPrevious := false
	presentCoreBindings := 0
	for _, contract := range t.contract.bindings {
		key := controllerRBACBindingKey(contract)
		binding, found := bindingsByKey[key]
		if !found {
			if t.HasPredecessor() {
				return nil, fmt.Errorf("required predecessor %s is missing", controllerRBACBindingDescription(contract))
			}
			continue
		}
		presentCoreBindings++
		state.bindings = append(state.bindings, binding)
		state.bindingUIDs[key] = binding.uid
		state.bindingResourceVersions[key] = binding.resourceVersion
		if !t.HasPredecessor() {
			if !reflect.DeepEqual(binding.subject, t.candidateSubject()) {
				return nil, fmt.Errorf("fresh install %s does not name the exact candidate controller", controllerRBACBindingDescription(contract))
			}
			continue
		}
		switch binding.subject.Name {
		case t.rollout.ControllerServiceAccountName:
			if seenPrevious {
				return nil, errors.New("controller RBAC bindings are not a valid candidate prefix")
			}
			state.cursor++
		case t.rollout.PreviousControllerServiceAccountName:
			seenPrevious = true
		default:
			return nil, fmt.Errorf("%s has an unexpected controller subject", controllerRBACBindingDescription(contract))
		}
	}
	state.coreComplete = presentCoreBindings == len(t.contract.bindings) &&
		(!t.HasPredecessor() || state.cursor == len(t.contract.bindings))

	postApplyBindingPresent := false
	if t.contract.postApplyBinding != nil {
		contract := *t.contract.postApplyBinding
		key := controllerRBACBindingKey(contract)
		if binding, found := bindingsByKey[key]; found {
			postApplyBindingPresent = true
			if !state.coreComplete {
				return nil, fmt.Errorf("candidate-only %s exists before the stable controller binding cutover is complete", controllerRBACBindingDescription(contract))
			}
			if !reflect.DeepEqual(binding.subject, t.candidateSubject()) {
				return nil, fmt.Errorf("candidate-only %s does not name the exact candidate controller", controllerRBACBindingDescription(contract))
			}
			state.bindingUIDs[key] = binding.uid
			state.bindingResourceVersions[key] = binding.resourceVersion
		}
	}

	allowCandidateStableRoles := !t.HasPredecessor() || state.coreComplete
	for _, contract := range t.contract.roles {
		uid, resourceVersion, found, err := t.verifyRole(ctx, contract, allowCandidateStableRoles, t.HasPredecessor())
		if err != nil {
			return nil, err
		}
		if found {
			key := controllerRBACObjectKey(contract.cluster, contract.namespace, contract.name)
			state.roleUIDs[key] = uid
			state.roleResourceVersions[key] = resourceVersion
		}
	}
	if t.contract.postApplyRole != nil {
		contract := *t.contract.postApplyRole
		uid, resourceVersion, found, err := t.verifyRole(ctx, contract, true, postApplyBindingPresent)
		if err != nil {
			return nil, err
		}
		if found && !state.coreComplete {
			return nil, fmt.Errorf(
				"candidate-only Role/%s/%s exists before the stable controller binding cutover is complete",
				contract.namespace,
				contract.name,
			)
		}
		if found {
			key := controllerRBACObjectKey(contract.cluster, contract.namespace, contract.name)
			state.roleUIDs[key] = uid
			state.roleResourceVersions[key] = resourceVersion
		}
	}
	if err := t.inspectServiceAccounts(ctx, state); err != nil {
		return nil, err
	}
	return state, nil
}

func (t *ControllerRBACTransition) inspectServiceAccounts(ctx context.Context, state *controllerRBACTransitionState) error {
	inspect := func(name string, managed, required bool, expectedUID types.UID) error {
		account, err := t.client.GetServiceAccount(ctx, t.rollout.ReleaseNamespace, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if required {
				return fmt.Errorf("required controller ServiceAccount/%s/%s is missing", t.rollout.ReleaseNamespace, name)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("get controller ServiceAccount/%s/%s: %w", t.rollout.ReleaseNamespace, name, err)
		}
		if account == nil || account.Name != name || account.Namespace != t.rollout.ReleaseNamespace ||
			account.UID == "" || account.ResourceVersion == "" || account.DeletionTimestamp != nil ||
			account.DeletionGracePeriodSeconds != nil {
			return fmt.Errorf("controller ServiceAccount/%s/%s has an incomplete or deleting identity", t.rollout.ReleaseNamespace, name)
		}
		if expectedUID != "" && account.UID != expectedUID {
			return fmt.Errorf("controller ServiceAccount/%s/%s UID changed from %q to %q", t.rollout.ReleaseNamespace, name, expectedUID, account.UID)
		}
		helmOwned := account.Annotations[helmReleaseNameAnnotation] == t.rollout.ReleaseName &&
			account.Annotations[helmReleaseNamespaceAnnotation] == t.rollout.ReleaseNamespace &&
			account.Labels[managedByLabel] == "Helm" &&
			account.Labels[instanceLabel] == t.rollout.ReleaseName
		if managed && !helmOwned {
			return fmt.Errorf("managed controller ServiceAccount/%s/%s lacks exact Helm ownership", t.rollout.ReleaseNamespace, name)
		}
		key := privilegeServiceAccountKey(t.rollout.ReleaseNamespace, name)
		state.serviceAccountUIDs[key] = account.UID
		state.serviceAccountResourceVersions[key] = account.ResourceVersion
		return nil
	}

	if t.HasPredecessor() {
		if err := inspect(
			t.rollout.PreviousControllerServiceAccountName,
			t.rollout.PreviousControllerServiceAccountManaged,
			false,
			t.rollout.PreviousControllerServiceAccountUID,
		); err != nil {
			return err
		}
	}
	if t.rollout.ControllerServiceAccountManaged {
		return inspect(t.rollout.ControllerServiceAccountName, true, false, "")
	}
	return inspect(t.rollout.ControllerServiceAccountName, false, true, "")
}

func (t *ControllerRBACTransition) verifyRoleBinding(binding *rbacv1.RoleBinding, contract controllerRBACBindingContract) (controllerRBACBindingState, error) {
	if binding == nil || contract.cluster || binding.Name != contract.name || binding.Namespace != contract.namespace {
		return controllerRBACBindingState{}, fmt.Errorf("RoleBinding/%s/%s has an unexpected identity", contract.namespace, contract.name)
	}
	if err := verifyControllerRBACMetadata("RoleBinding", binding.ObjectMeta, t.rollout, contract.name, contract.namespace); err != nil {
		return controllerRBACBindingState{}, err
	}
	return t.bindingState(contract, binding.RoleRef, binding.Subjects, binding.UID, binding.ResourceVersion)
}

func (t *ControllerRBACTransition) verifyClusterRoleBinding(binding *rbacv1.ClusterRoleBinding, contract controllerRBACBindingContract) (controllerRBACBindingState, error) {
	if binding == nil || !contract.cluster || binding.Name != contract.name || binding.Namespace != "" {
		return controllerRBACBindingState{}, fmt.Errorf("ClusterRoleBinding/%s has an unexpected identity", contract.name)
	}
	if err := verifyControllerRBACMetadata("ClusterRoleBinding", binding.ObjectMeta, t.rollout, contract.name, ""); err != nil {
		return controllerRBACBindingState{}, err
	}
	return t.bindingState(contract, binding.RoleRef, binding.Subjects, binding.UID, binding.ResourceVersion)
}

func (t *ControllerRBACTransition) bindingState(
	contract controllerRBACBindingContract,
	roleRef rbacv1.RoleRef,
	subjects []rbacv1.Subject,
	uid types.UID,
	resourceVersion string,
) (controllerRBACBindingState, error) {
	if !reflect.DeepEqual(roleRef, contract.roleRef) {
		return controllerRBACBindingState{}, fmt.Errorf("%s has an unexpected roleRef", controllerRBACBindingDescription(contract))
	}
	if len(subjects) != 1+len(contract.fixedSubjects) {
		if len(contract.fixedSubjects) == 0 {
			return controllerRBACBindingState{}, fmt.Errorf("%s does not have exactly one subject", controllerRBACBindingDescription(contract))
		}
		return controllerRBACBindingState{}, fmt.Errorf("%s has an unexpected subject count", controllerRBACBindingDescription(contract))
	}
	previous := t.previousSubject()
	candidate := t.candidateSubject()
	if !reflect.DeepEqual(subjects[0], previous) && !reflect.DeepEqual(subjects[0], candidate) {
		return controllerRBACBindingState{}, fmt.Errorf("%s has a foreign controller subject", controllerRBACBindingDescription(contract))
	}
	if !slices.Equal(subjects[1:], contract.fixedSubjects) {
		return controllerRBACBindingState{}, fmt.Errorf("%s has foreign fixed subjects", controllerRBACBindingDescription(contract))
	}
	return controllerRBACBindingState{
		contract:        contract,
		uid:             uid,
		resourceVersion: resourceVersion,
		subject:         subjects[0],
	}, nil
}

func (t *ControllerRBACTransition) verifyRole(
	ctx context.Context,
	contract controllerRBACRoleContract,
	allowCandidate bool,
	required bool,
) (types.UID, string, bool, error) {
	if contract.cluster {
		role, err := t.client.GetClusterRole(ctx, contract.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !required {
			return "", "", false, nil
		}
		if err != nil {
			return "", "", false, fmt.Errorf("get ClusterRole/%s: %w", contract.name, err)
		}
		if role == nil || role.Name != contract.name || role.Namespace != "" {
			return "", "", false, fmt.Errorf("ClusterRole/%s has an unexpected identity", contract.name)
		}
		if err := verifyControllerRBACMetadata("ClusterRole", role.ObjectMeta, t.rollout, contract.name, ""); err != nil {
			return "", "", false, err
		}
		if err := verifyControllerRBACRoleRules("ClusterRole/"+contract.name, role.Rules, contract, allowCandidate); err != nil {
			return "", "", false, err
		}
		return role.UID, role.ResourceVersion, true, nil
	}
	role, err := t.client.GetRole(ctx, contract.namespace, contract.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && !required {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get Role/%s/%s: %w", contract.namespace, contract.name, err)
	}
	if role == nil || role.Name != contract.name || role.Namespace != contract.namespace {
		return "", "", false, fmt.Errorf("Role/%s/%s has an unexpected identity", contract.namespace, contract.name)
	}
	if err := verifyControllerRBACMetadata("Role", role.ObjectMeta, t.rollout, contract.name, contract.namespace); err != nil {
		return "", "", false, err
	}
	if err := verifyControllerRBACRoleRules("Role/"+contract.namespace+"/"+contract.name, role.Rules, contract, allowCandidate); err != nil {
		return "", "", false, err
	}
	return role.UID, role.ResourceVersion, true, nil
}

func verifyControllerRBACRoleRules(
	description string,
	actual []rbacv1.PolicyRule,
	contract controllerRBACRoleContract,
	allowCandidate bool,
) error {
	if len(contract.predecessorRules) != 0 && reflect.DeepEqual(actual, contract.predecessorRules) {
		return nil
	}
	if allowCandidate && len(contract.candidateRules) != 0 && reflect.DeepEqual(actual, contract.candidateRules) {
		return nil
	}
	if allowCandidate {
		return fmt.Errorf("%s rules differ from the exact predecessor and candidate contracts", description)
	}
	return fmt.Errorf("%s rules differ from the exact predecessor contract before the stable binding cutover is complete", description)
}

func (t *ControllerRBACTransition) previousSubject() rbacv1.Subject {
	return controllerRBACServiceAccountSubject(t.rollout.ReleaseNamespace, t.rollout.PreviousControllerServiceAccountName)
}

func (t *ControllerRBACTransition) candidateSubject() rbacv1.Subject {
	return controllerRBACServiceAccountSubject(t.rollout.ReleaseNamespace, t.rollout.ControllerServiceAccountName)
}

func (t *ControllerRBACTransition) validateIdentity() error {
	if t == nil || t.rollout == nil || t.client == nil {
		return errors.New("controller RBAC transition client and rollout identity are required")
	}
	for description, value := range map[string]string{
		"release name":                        t.rollout.ReleaseName,
		"release namespace":                   t.rollout.ReleaseNamespace,
		"coordination namespace":              t.rollout.CoordinationNamespace,
		"hook ServiceAccount name":            t.rollout.HookServiceAccountName,
		"candidate controller ServiceAccount": t.rollout.ControllerServiceAccountName,
		"controller Deployment name":          t.rollout.ControllerDeploymentName,
		"manager image":                       t.rollout.ManagerImage,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("controller RBAC transition %s is empty or padded", description)
		}
	}
	if t.rollout.ReleaseSequence < 1 {
		return errors.New("controller RBAC transition release sequence must be positive")
	}
	if t.rollout.PreviousControllerReleaseSequence < 0 ||
		t.rollout.PreviousControllerReleaseSequence >= t.rollout.ReleaseSequence {
		return errors.New("controller RBAC predecessor release sequence must be non-negative and lower than the candidate")
	}
	if t.rollout.PreviousControllerServiceAccountName != "" && t.rollout.PreviousControllerServiceAccountName == t.rollout.ControllerServiceAccountName {
		return errors.New("controller RBAC predecessor and candidate ServiceAccounts must differ")
	}
	if t.rollout.PreviousControllerServiceAccountName == "" {
		if t.rollout.PreviousControllerServiceAccountUID != "" || t.rollout.PreviousControllerServiceAccountManaged {
			return errors.New("controller RBAC predecessor UID and ownership require a predecessor name")
		}
	} else {
		if t.rollout.PreviousControllerServiceAccountName != strings.TrimSpace(t.rollout.PreviousControllerServiceAccountName) {
			return errors.New("controller RBAC predecessor ServiceAccount name is padded")
		}
		if t.rollout.PreviousControllerServiceAccountUID == "" {
			return errors.New("controller RBAC predecessor ServiceAccount UID is required")
		}
	}
	return nil
}

func (t *ControllerRBACTransition) validate() error {
	if err := t.validateIdentity(); err != nil {
		return err
	}
	if len(t.contract.bindings) == 0 {
		return errors.New("controller RBAC transition binding contract is empty")
	}
	if t.HasPredecessor() && len(t.contract.roles) == 0 {
		return errors.New("controller RBAC transition role contract is empty")
	}
	return nil
}

func controllerRBACContract(
	rollout *RolloutGuard,
	runtimeContract RuntimeAdmissionContract,
) (controllerRBACTransitionContract, error) {
	if runtimeContract.Namespace != rollout.ReleaseNamespace {
		return controllerRBACTransitionContract{}, fmt.Errorf(
			"controller RBAC runtime contract namespace %q differs from release namespace %q",
			runtimeContract.Namespace,
			rollout.ReleaseNamespace,
		)
	}
	if runtimeContract.ControllerServiceAccountName != rollout.ControllerServiceAccountName {
		return controllerRBACTransitionContract{}, fmt.Errorf(
			"controller RBAC runtime contract ServiceAccount %q differs from candidate %q",
			runtimeContract.ControllerServiceAccountName,
			rollout.ControllerServiceAccountName,
		)
	}
	if runtimeContract.CertificateServiceAccountName == "" {
		return controllerRBACTransitionContract{}, errors.New("controller RBAC runtime contract certificate ServiceAccount is required")
	}
	bindingName := rollout.ControllerDeploymentName
	bindings := []controllerRBACBindingContract{
		{name: bindingName, cluster: true, roleRef: controllerRBACRoleRef("ClusterRole", bindingName)},
	}
	runtimeBinding := controllerRBACBindingContract{
		name: bindingName + "-runtime-admission", namespace: rollout.ReleaseNamespace,
		roleRef: controllerRBACRoleRef("Role", bindingName+"-runtime-admission"),
		fixedSubjects: []rbacv1.Subject{
			controllerRBACServiceAccountSubject(rollout.ReleaseNamespace, runtimeContract.CertificateServiceAccountName),
		},
	}
	if rollout.PreviousControllerReleaseSequence >= 1 {
		bindings = append(bindings, runtimeBinding)
	}
	bindings = append(bindings, controllerRBACBindingContract{
		name: bindingName, namespace: rollout.CoordinationNamespace,
		roleRef: controllerRBACRoleRef("Role", bindingName),
	})
	runtimeRole := controllerRBACRoleContract{
		name:           runtimeBinding.name,
		namespace:      rollout.ReleaseNamespace,
		candidateRules: currentControllerRuntimeRoleRules(rollout, runtimeContract),
	}

	if rollout.PreviousControllerServiceAccountName == "" {
		return controllerRBACTransitionContract{
			bindings:         bindings,
			postApplyBinding: &runtimeBinding,
			roles: []controllerRBACRoleContract{
				{name: bindingName, cluster: true, candidateRules: currentControllerClusterRoleRules(rollout)},
				{name: bindingName, namespace: rollout.CoordinationNamespace, candidateRules: currentControllerCoordinationRoleRules()},
			},
			postApplyRole: &runtimeRole,
		}, nil
	}
	if rollout.ReleaseSequence != 1 || rollout.PreviousControllerReleaseSequence != 0 {
		return controllerRBACTransitionContract{}, fmt.Errorf(
			"controller RBAC transition from release sequence %d to %d requires an explicit frozen predecessor role contract",
			rollout.PreviousControllerReleaseSequence,
			rollout.ReleaseSequence,
		)
	}
	contract := controllerRBACTransitionContract{
		bindings:         bindings,
		postApplyBinding: &runtimeBinding,
		roles: []controllerRBACRoleContract{
			{
				name:             bindingName,
				cluster:          true,
				predecessorRules: legacyControllerClusterRoleRules(),
				candidateRules:   currentControllerClusterRoleRules(rollout),
			},
			{
				name:             bindingName,
				namespace:        rollout.CoordinationNamespace,
				predecessorRules: legacyControllerCoordinationRoleRules(),
				candidateRules:   currentControllerCoordinationRoleRules(),
			},
		},
		postApplyRole: &runtimeRole,
	}
	if rollout.PreviousControllerReleaseSequence >= 1 {
		contract.postApplyBinding = nil
		contract.postApplyRole = nil
	}
	return contract, nil
}

func legacyControllerClusterRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{"operator.ptah.dev"}, Resources: []string{"ptahschemas"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
		{APIGroups: []string{"operator.ptah.dev"}, Resources: []string{"ptahschemas/finalizers", "ptahschemaplans/finalizers"}, Verbs: []string{"update"}},
		{APIGroups: []string{"operator.ptah.dev"}, Resources: []string{"ptahschemas/status", "ptahschemaplans/status", "ptahschemaapprovals/status"}, Verbs: []string{"get", "update", "patch"}},
		{APIGroups: []string{"operator.ptah.dev"}, Resources: []string{"ptahschemaplans"}, Verbs: []string{"get", "list", "watch", "create"}},
		{APIGroups: []string{"operator.ptah.dev"}, Resources: []string{"ptahschemaapprovals"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch", "create", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"limitranges"}, Verbs: []string{"list"}},
		{APIGroups: []string{"node.k8s.io"}, Resources: []string{"runtimeclasses"}, Verbs: []string{"get"}},
		{APIGroups: []string{"scheduling.k8s.io"}, Resources: []string{"priorityclasses"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch", "create"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch", "update"}},
	}
}

func legacyControllerCoordinationRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups: []string{"coordination.k8s.io"},
		Resources: []string{"leases"},
		Verbs:     []string{"get", "create", "update"},
	}}
}

func currentControllerClusterRoleRules(rollout *RolloutGuard) []rbacv1.PolicyRule {
	crdNames := []string{
		"ptahschemaapprovals.operator.ptah.dev",
		"ptahschemaplans.operator.ptah.dev",
		"ptahschemas.operator.ptah.dev",
	}
	return []rbacv1.PolicyRule{
		privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get"}),
		privilegePolicyRule(
			[]string{"admissionregistration.k8s.io"},
			[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
			[]string{AdmissionConfigurationName},
			[]string{"get"},
		),
		privilegePolicyRule(
			[]string{"admissionregistration.k8s.io"},
			[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
			currentControllerRuntimeGuardNames(rollout),
			[]string{"get"},
		),
		privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list"}),
		privilegePolicyRule([]string{"operator.ptah.dev"}, []string{"ptahschemas"}, nil, []string{"get", "list", "watch", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.dev"}, []string{"ptahschemas/finalizers", "ptahschemaplans/finalizers"}, nil, []string{"update"}),
		privilegePolicyRule([]string{"operator.ptah.dev"}, []string{"ptahschemas/status", "ptahschemaplans/status", "ptahschemaapprovals/status"}, nil, []string{"get", "update", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.dev"}, []string{"ptahschemaplans"}, nil, []string{"get", "list", "watch", "create"}),
		privilegePolicyRule([]string{"operator.ptah.dev"}, []string{"ptahschemaapprovals"}, nil, []string{"get", "list", "watch"}),
		privilegePolicyRule([]string{"batch"}, []string{"jobs"}, nil, []string{"get", "list", "watch", "create", "patch"}),
		privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"get", "list", "watch"}),
		privilegePolicyRule([]string{""}, []string{"pods/log"}, nil, []string{"get"}),
		privilegePolicyRule([]string{""}, []string{"serviceaccounts"}, nil, []string{"get"}),
		privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
		privilegePolicyRule([]string{"node.k8s.io"}, []string{"runtimeclasses"}, nil, []string{"get"}),
		privilegePolicyRule([]string{"scheduling.k8s.io"}, []string{"priorityclasses"}, nil, []string{"get", "list"}),
		privilegePolicyRule([]string{""}, []string{"configmaps"}, nil, []string{"get", "list", "watch", "create"}),
		privilegePolicyRule([]string{""}, []string{"events"}, nil, []string{"create", "patch", "update"}),
	}
}

func currentControllerRuntimeRoleRules(rollout *RolloutGuard, contract RuntimeAdmissionContract) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		privilegePolicyRule(
			[]string{""},
			[]string{"serviceaccounts"},
			[]string{contract.ControllerServiceAccountName, contract.CertificateServiceAccountName},
			[]string{"get"},
		),
		privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
		privilegePolicyRule(
			[]string{""},
			[]string{"configmaps"},
			[]string{AdmissionConvergenceMarkerName(contract.Namespace, rollout.ReleaseName, rollout.ReleaseSequence)},
			[]string{"get", "update"},
		),
	}
}

func currentControllerCoordinationRoleRules() []rbacv1.PolicyRule {
	return legacyControllerCoordinationRoleRules()
}

func currentControllerRuntimeGuardNames(rollout *RolloutGuard) []string {
	return currentRetainedAdmissionGuardNames(rollout)
}

func currentRetainedAdmissionGuardNames(rollout *RolloutGuard) []string {
	return []string{
		RolloutGuardPolicyName(rollout.ReleaseSequence),
		RuntimeGuardPolicyName(rollout.ReleaseSequence),
		RuntimePodGuardPolicyName(rollout.ReleaseSequence),
		HookIdentityGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		HookIdentityProbeGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ReleaseActivationGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		AdmissionConvergencePolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		ServiceAccountObjectGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		ServiceAccountOriginGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ControllerWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ControllerJobWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ControllerChunkWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ControllerPlanWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		CertificateMutatingWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		CertificateValidatingWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		NamespaceDeletionGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		ParentReplicaSetGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
		ParentHookPodOriginGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		ParentHookJobOriginGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName),
		ParentHookJobContractPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
	}
}

func currentCRDManagerAdmissionGuardNames(rollout *RolloutGuard) []string {
	return currentRetainedAdmissionGuardNames(rollout)
}

func controllerRBACAuthorizationChecks(rollout *RolloutGuard, roles []controllerRBACRoleContract) ([]AuthorizationCheck, error) {
	const probeName = "ptah-controller-rbac-revocation-probe"
	checksByKey := make(map[string]AuthorizationCheck)
	for _, role := range roles {
		for ruleIndex, rule := range role.predecessorRules {
			if len(rule.APIGroups) == 0 || len(rule.Resources) == 0 || len(rule.Verbs) == 0 || len(rule.NonResourceURLs) != 0 {
				return nil, fmt.Errorf("controller RBAC role %q rule %d is not an exact resource grant", role.name, ruleIndex)
			}
			for _, group := range rule.APIGroups {
				for _, combinedResource := range rule.Resources {
					resource, subresource, _ := strings.Cut(combinedResource, "/")
					if resource == "" || group == "*" || resource == "*" || subresource == "*" {
						return nil, fmt.Errorf("controller RBAC role %q contains an unbounded resource grant", role.name)
					}
					for _, verb := range rule.Verbs {
						if verb == "" || verb == "*" {
							return nil, fmt.Errorf("controller RBAC role %q contains an unbounded verb grant", role.name)
						}
						names := rule.ResourceNames
						if len(names) == 0 {
							names = []string{""}
						}
						for _, resourceName := range names {
							name := resourceName
							if name == "" && verb != "list" && verb != "watch" && verb != "create" && verb != "deletecollection" {
								name = probeName
							}
							namespace := role.namespace
							if role.cluster {
								namespace = controllerRBACResourceNamespace(group, resource, rollout.ReleaseNamespace)
							}
							attributes := &authorizationv1.ResourceAttributes{
								Namespace:   namespace,
								Verb:        verb,
								Group:       group,
								Version:     controllerRBACResourceVersion(group),
								Resource:    resource,
								Subresource: subresource,
								Name:        name,
							}
							keyBytes, err := json.Marshal(attributes)
							if err != nil {
								return nil, err
							}
							key := string(keyBytes)
							checksByKey[key] = AuthorizationCheck{ResourceAttributes: attributes}
						}
					}
				}
			}
		}
	}
	keys := make([]string, 0, len(checksByKey))
	for key := range checksByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	checks := make([]AuthorizationCheck, 0, len(keys))
	for index, key := range keys {
		check := checksByKey[key]
		check.Name = fmt.Sprintf("predecessor grant %03d", index+1)
		checks = append(checks, check)
	}
	if len(checks) == 0 {
		return nil, errors.New("controller RBAC predecessor authorization checks are empty")
	}
	return checks, nil
}

func controllerRBACResourceNamespace(group, resource, releaseNamespace string) string {
	switch group + "/" + resource {
	case "node.k8s.io/runtimeclasses", "scheduling.k8s.io/priorityclasses":
		return ""
	default:
		return releaseNamespace
	}
}

func controllerRBACResourceVersion(group string) string {
	if group == "operator.ptah.dev" {
		return "v1alpha1"
	}
	return "v1"
}

func controllerRBACSubjectPatch(binding controllerRBACBindingState, candidate rbacv1.Subject) ([]byte, error) {
	currentSubjects := append([]rbacv1.Subject{binding.subject}, binding.contract.fixedSubjects...)
	candidateSubjects := append([]rbacv1.Subject{candidate}, binding.contract.fixedSubjects...)
	operations := []struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}{
		{Op: "test", Path: "/metadata/uid", Value: string(binding.uid)},
		{Op: "test", Path: "/metadata/resourceVersion", Value: binding.resourceVersion},
		{Op: "test", Path: "/roleRef", Value: binding.contract.roleRef},
		{Op: "test", Path: "/subjects", Value: currentSubjects},
		{Op: "replace", Path: "/subjects", Value: candidateSubjects},
	}
	return json.Marshal(operations)
}

func verifyControllerRBACMetadata(kind string, metadata metav1.ObjectMeta, rollout *RolloutGuard, name, namespace string) error {
	if metadata.Name != name || metadata.Namespace != namespace || metadata.GenerateName != "" ||
		metadata.UID == "" || metadata.ResourceVersion == "" || metadata.DeletionTimestamp != nil ||
		metadata.DeletionGracePeriodSeconds != nil || len(metadata.OwnerReferences) != 0 || len(metadata.Finalizers) != 0 ||
		metadata.Annotations[helmReleaseNameAnnotation] != rollout.ReleaseName ||
		metadata.Annotations[helmReleaseNamespaceAnnotation] != rollout.ReleaseNamespace ||
		metadata.Labels[managedByLabel] != "Helm" || metadata.Labels[instanceLabel] != rollout.ReleaseName {
		return fmt.Errorf("%s/%s has foreign, incomplete, or deleting Helm ownership", kind, name)
	}
	return nil
}

func controllerRBACProtectedSubjects(rollout *RolloutGuard) privilegeProtectedSubjects {
	names := []string{rollout.ControllerServiceAccountName}
	if rollout.PreviousControllerServiceAccountName != "" {
		names = append(names, rollout.PreviousControllerServiceAccountName)
	}
	protected := privilegeProtectedSubjects{
		serviceAccounts: make(map[string]struct{}, len(names)),
		users:           make(map[string]struct{}, len(names)),
		namespaceGroup:  "system:serviceaccounts:" + rollout.ReleaseNamespace,
	}
	for _, name := range names {
		protected.serviceAccounts[privilegeServiceAccountKey(rollout.ReleaseNamespace, name)] = struct{}{}
		protected.users["system:serviceaccount:"+rollout.ReleaseNamespace+":"+name] = struct{}{}
	}
	return protected
}

func controllerRBACServiceAccountSubject(namespace, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: namespace}
}

func controllerRBACRoleRef(kind, name string) rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kind, Name: name}
}

func controllerRBACBindingKey(contract controllerRBACBindingContract) string {
	return controllerRBACObjectKey(contract.cluster, contract.namespace, contract.name)
}

func controllerRBACObjectKey(cluster bool, namespace, name string) string {
	return fmt.Sprintf("%t\x00%s\x00%s", cluster, namespace, name)
}

func controllerRBACBindingDescription(contract controllerRBACBindingContract) string {
	if contract.cluster {
		return "ClusterRoleBinding/" + contract.name
	}
	return "RoleBinding/" + contract.namespace + "/" + contract.name
}

func cloneControllerRBACTransitionState(state *controllerRBACTransitionState) *controllerRBACTransitionState {
	if state == nil {
		return nil
	}
	clone := &controllerRBACTransitionState{cursor: state.cursor, coreComplete: state.coreComplete}
	clone.bindings = append([]controllerRBACBindingState(nil), state.bindings...)
	clone.bindingUIDs = make(map[string]types.UID, len(state.bindingUIDs))
	for key, uid := range state.bindingUIDs {
		clone.bindingUIDs[key] = uid
	}
	clone.bindingResourceVersions = make(map[string]string, len(state.bindingResourceVersions))
	for key, resourceVersion := range state.bindingResourceVersions {
		clone.bindingResourceVersions[key] = resourceVersion
	}
	clone.roleUIDs = make(map[string]types.UID, len(state.roleUIDs))
	for key, uid := range state.roleUIDs {
		clone.roleUIDs[key] = uid
	}
	clone.roleResourceVersions = make(map[string]string, len(state.roleResourceVersions))
	for key, resourceVersion := range state.roleResourceVersions {
		clone.roleResourceVersions[key] = resourceVersion
	}
	clone.serviceAccountUIDs = make(map[string]types.UID, len(state.serviceAccountUIDs))
	for key, uid := range state.serviceAccountUIDs {
		clone.serviceAccountUIDs[key] = uid
	}
	clone.serviceAccountResourceVersions = make(map[string]string, len(state.serviceAccountResourceVersions))
	for key, resourceVersion := range state.serviceAccountResourceVersions {
		clone.serviceAccountResourceVersions[key] = resourceVersion
	}
	return clone
}

func controllerRBACStateIdentityEqual(first, second *controllerRBACTransitionState) bool {
	return first != nil && second != nil &&
		first.cursor == second.cursor &&
		first.coreComplete == second.coreComplete &&
		reflect.DeepEqual(first.bindingUIDs, second.bindingUIDs) &&
		reflect.DeepEqual(first.bindingResourceVersions, second.bindingResourceVersions) &&
		reflect.DeepEqual(first.roleUIDs, second.roleUIDs) &&
		reflect.DeepEqual(first.roleResourceVersions, second.roleResourceVersions) &&
		reflect.DeepEqual(first.serviceAccountUIDs, second.serviceAccountUIDs) &&
		reflect.DeepEqual(first.serviceAccountResourceVersions, second.serviceAccountResourceVersions)
}

func controllerRBACBindingResourceVersionsAdvancedExactlyOne(
	after, before *controllerRBACTransitionState,
	targetKey string,
) bool {
	if after == nil || before == nil ||
		len(after.bindingResourceVersions) != len(before.bindingResourceVersions) {
		return false
	}
	for key, beforeVersion := range before.bindingResourceVersions {
		afterVersion, found := after.bindingResourceVersions[key]
		if !found {
			return false
		}
		if key == targetKey {
			if afterVersion == "" || afterVersion == beforeVersion {
				return false
			}
			continue
		}
		if afterVersion != beforeVersion {
			return false
		}
	}
	return true
}

func listControllerRoleBindings(ctx context.Context, client ControllerRBACClient) ([]rbacv1.RoleBinding, error) {
	items := []rbacv1.RoleBinding{}
	seen := make(map[string]struct{})
	err := paginatePrivilegeBindings(ctx, "RoleBindings cluster-wide", func(ctx context.Context, options metav1.ListOptions) (metav1.ListMeta, int, error) {
		page, err := client.ListRoleBindings(ctx, options)
		if err != nil {
			return metav1.ListMeta{}, 0, err
		}
		if page == nil {
			return metav1.ListMeta{}, 0, errors.New("API returned a nil RoleBinding page")
		}
		for _, item := range page.Items {
			key := privilegeBindingKey(item.Namespace, item.Name)
			if _, duplicate := seen[key]; duplicate {
				return metav1.ListMeta{}, 0, fmt.Errorf("duplicate RoleBinding/%s/%s across inventory pages", item.Namespace, item.Name)
			}
			seen[key] = struct{}{}
			items = append(items, *item.DeepCopy())
		}
		return page.ListMeta, len(page.Items), nil
	})
	return items, err
}

func listControllerClusterRoleBindings(ctx context.Context, client ControllerRBACClient) ([]rbacv1.ClusterRoleBinding, error) {
	items := []rbacv1.ClusterRoleBinding{}
	seen := make(map[string]struct{})
	err := paginatePrivilegeBindings(ctx, "ClusterRoleBindings", func(ctx context.Context, options metav1.ListOptions) (metav1.ListMeta, int, error) {
		page, err := client.ListClusterRoleBindings(ctx, options)
		if err != nil {
			return metav1.ListMeta{}, 0, err
		}
		if page == nil {
			return metav1.ListMeta{}, 0, errors.New("API returned a nil ClusterRoleBinding page")
		}
		for _, item := range page.Items {
			if _, duplicate := seen[item.Name]; duplicate {
				return metav1.ListMeta{}, 0, fmt.Errorf("duplicate ClusterRoleBinding/%s across inventory pages", item.Name)
			}
			seen[item.Name] = struct{}{}
			items = append(items, *item.DeepCopy())
		}
		return page.ListMeta, len(page.Items), nil
	})
	return items, err
}
