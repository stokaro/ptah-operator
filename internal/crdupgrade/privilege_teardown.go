package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

const privilegeTeardownBindingPageSize = 256

// PrivilegeRoleBindingClient is the cluster-wide RoleBinding API used by
// privilege teardown. Delete includes the namespace because Kubernetes cannot
// delete through a RoleBinding client constructed for NamespaceAll.
type PrivilegeRoleBindingClient interface {
	List(context.Context, metav1.ListOptions) (*rbacv1.RoleBindingList, error)
	Delete(context.Context, string, string, metav1.DeleteOptions) error
}

// PrivilegeClusterRoleBindingClient is the cluster-scoped binding API used by
// privilege teardown.
type PrivilegeClusterRoleBindingClient interface {
	List(context.Context, metav1.ListOptions) (*rbacv1.ClusterRoleBindingList, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// PrivilegeRoleClient is the read-only, namespace-explicit Role API used to
// prove that every temporary namespaced permission exactly matches the
// candidate release before any binding is removed.
type PrivilegeRoleClient interface {
	Get(context.Context, string, string, metav1.GetOptions) (*rbacv1.Role, error)
}

// PrivilegeClusterRoleClient is the exact ClusterRole API used to revoke the
// cleanup identity's temporary RBAC authority after all other privilege has
// been removed.
type PrivilegeClusterRoleClient interface {
	Get(context.Context, string, metav1.GetOptions) (*rbacv1.ClusterRole, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// PrivilegeServiceAccountClient is scoped to the release namespace. It is
// used only for the exact chart-created identities compiled into the release.
type PrivilegeServiceAccountClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ServiceAccount, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// PrivilegeTeardownConfig contains the candidate-specific cleanup identities
// rendered by Helm. The privilege name is self-revoked here; the residual
// names remain for admission teardown and revocation checks.
type PrivilegeTeardownConfig struct {
	CleanupServiceAccountName string
	CleanupPrivilegeName      string
	ResidualGuardName         string
	ResidualReleaseRoleName   string
	ResidualDiscoveryRoleName string
	DiscoveryNamespace        string
}

// PrivilegeTeardown removes every exact RoleBinding, ClusterRoleBinding, and
// chart-created ServiceAccount that can keep a retired runtime or privileged
// hook identity usable. It self-revokes the cleanup identity's temporary RBAC
// mutation authority, retaining only the separate guard-teardown boundary.
type PrivilegeTeardown struct {
	rollout            *RolloutGuard
	contract           RuntimeAdmissionContract
	roleBindings       PrivilegeRoleBindingClient
	clusterBindings    PrivilegeClusterRoleBindingClient
	roles              PrivilegeRoleClient
	clusterRoles       PrivilegeClusterRoleClient
	serviceAccounts    PrivilegeServiceAccountClient
	cleanupAccountName string
	cleanupPrivilege   string
	residualGuard      string
	residualRelease    string
	residualDiscovery  string
	discoveryNamespace string
}

// NewPrivilegeTeardown constructs the fail-closed privilege phase that must
// finish before ReleaseTeardown removes any admission guard.
func NewPrivilegeTeardown(
	rollout *RolloutGuard,
	contract RuntimeAdmissionContract,
	config PrivilegeTeardownConfig,
	roleBindings PrivilegeRoleBindingClient,
	clusterBindings PrivilegeClusterRoleBindingClient,
	roles PrivilegeRoleClient,
	clusterRoles PrivilegeClusterRoleClient,
	serviceAccounts PrivilegeServiceAccountClient,
) *PrivilegeTeardown {
	return &PrivilegeTeardown{
		rollout:            rollout,
		contract:           contract,
		roleBindings:       roleBindings,
		clusterBindings:    clusterBindings,
		roles:              roles,
		clusterRoles:       clusterRoles,
		serviceAccounts:    serviceAccounts,
		cleanupAccountName: config.CleanupServiceAccountName,
		cleanupPrivilege:   config.CleanupPrivilegeName,
		residualGuard:      config.ResidualGuardName,
		residualRelease:    config.ResidualReleaseRoleName,
		residualDiscovery:  config.ResidualDiscoveryRoleName,
		discoveryNamespace: config.DiscoveryNamespace,
	}
}

// Preflight verifies the complete paginated binding inventory and every
// chart-created ServiceAccount without changing cluster state.
func (t *PrivilegeTeardown) Preflight(ctx context.Context) error {
	_, err := t.inspect(ctx)
	return err
}

// Teardown repeats the complete preflight, removes exact privilege bindings,
// confirms that API storage contains only cleanup access, and then deletes
// retired chart-created ServiceAccounts with UID and resource-version
// preconditions. The caller must separately establish authorizer convergence
// before removing the admission guards; a storage LIST is not that barrier.
func (t *PrivilegeTeardown) Teardown(ctx context.Context) error {
	state, err := t.inspect(ctx)
	if err != nil {
		return err
	}

	// Namespaced privilege is revoked first. Each cleanup privilege binding is
	// removed later through the authority it grants in its own namespace.
	for _, target := range state.normalRoleBindings {
		if err := t.roleBindings.Delete(ctx, target.namespace, target.name, target.identity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete RoleBinding/%s/%s: %w", target.namespace, target.name, err)
		}
	}
	for _, target := range state.normalClusterBindings {
		if err := t.clusterBindings.Delete(ctx, target.name, target.identity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ClusterRoleBinding/%s: %w", target.name, err)
		}
	}

	bindingsAfterDelete, err := t.inspectBindings(ctx)
	if err != nil {
		return fmt.Errorf("verify privilege bindings after deletion: %w", err)
	}
	if len(bindingsAfterDelete.normalClusterBindings) != 0 || len(bindingsAfterDelete.normalRoleBindings) != 0 {
		return errors.New("protected privilege bindings remain after exact deletion")
	}

	for _, account := range t.serviceAccountContracts() {
		if !account.remove {
			continue
		}
		identity, found, err := t.inspectServiceAccount(ctx, account)
		if err != nil {
			return fmt.Errorf("re-verify ServiceAccount/%s: %w", account.name, err)
		}
		if !found {
			continue
		}
		if err := t.serviceAccounts.Delete(ctx, account.name, identity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ServiceAccount/%s: %w", account.name, err)
		}
	}

	bindingsAfterAccounts, err := t.inspectBindings(ctx)
	if err != nil {
		return fmt.Errorf("verify privilege bindings after ServiceAccount deletion: %w", err)
	}
	if len(bindingsAfterAccounts.normalClusterBindings) != 0 || len(bindingsAfterAccounts.normalRoleBindings) != 0 {
		return errors.New("protected privilege bindings reappeared during ServiceAccount deletion")
	}

	for _, target := range bindingsAfterAccounts.privilegeRoleBindings {
		if err := t.roleBindings.Delete(ctx, target.namespace, target.name, target.identity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete cleanup privilege RoleBinding/%s/%s: %w", target.namespace, target.name, err)
		}
	}

	bindingsAfterRoleRevocation, err := t.inspectBindings(ctx)
	if err != nil {
		return fmt.Errorf("verify cleanup privilege RoleBinding revocation: %w", err)
	}
	if len(bindingsAfterRoleRevocation.normalClusterBindings) != 0 ||
		len(bindingsAfterRoleRevocation.normalRoleBindings) != 0 ||
		len(bindingsAfterRoleRevocation.privilegeRoleBindings) != 0 {
		return errors.New("a namespaced protected privilege binding remains after cleanup self-revocation")
	}
	if bindingsAfterRoleRevocation.privilegeClusterBinding != nil {
		target := bindingsAfterRoleRevocation.privilegeClusterBinding
		if err := t.clusterBindings.Delete(ctx, target.name, target.identity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete cleanup privilege ClusterRoleBinding/%s: %w", target.name, err)
		}
	}
	privilegeRoleIdentity, privilegeRoleFound, err := t.inspectAuthorizationClusterRole(ctx, t.cleanupPrivilegeContract())
	if err != nil {
		return fmt.Errorf("re-verify cleanup privilege ClusterRole/%s: %w", t.cleanupPrivilege, err)
	}
	if privilegeRoleFound {
		if err := t.clusterRoles.Delete(ctx, t.cleanupPrivilege, privilegeRoleIdentity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete cleanup privilege ClusterRole/%s: %w", t.cleanupPrivilege, err)
		}
	}

	finalBindings, err := t.inspectBindings(ctx)
	if err != nil {
		return fmt.Errorf("verify final privilege binding inventory: %w", err)
	}
	if len(finalBindings.normalClusterBindings) != 0 || len(finalBindings.normalRoleBindings) != 0 ||
		len(finalBindings.privilegeRoleBindings) != 0 || finalBindings.privilegeClusterBinding != nil {
		return errors.New("a non-residual protected privilege binding remains after cleanup self-revocation")
	}
	if _, found, err := t.inspectAuthorizationClusterRole(ctx, t.cleanupPrivilegeContract()); err != nil {
		return fmt.Errorf("verify cleanup privilege ClusterRole removal: %w", err)
	} else if found {
		return fmt.Errorf("cleanup privilege ClusterRole/%s remains after self-revocation", t.cleanupPrivilege)
	}
	if _, err := t.inspect(ctx); err != nil {
		return fmt.Errorf("verify retained residual privilege boundary: %w", err)
	}
	if _, found, err := t.inspectServiceAccount(ctx, privilegeServiceAccountContract{
		name:      t.cleanupAccountName,
		required:  true,
		component: "crd-manager-teardown",
	}); err != nil {
		return fmt.Errorf("verify retained cleanup ServiceAccount/%s: %w", t.cleanupAccountName, err)
	} else if !found {
		return fmt.Errorf("retained cleanup ServiceAccount/%s is missing", t.cleanupAccountName)
	}
	return nil
}

// RetireCleanupServiceAccount removes the last credential source only after
// the complete residual privilege boundary has been re-read and verified.
// A retry is expected to run from a Helm-recreated exact ServiceAccount; an
// initially missing account is therefore not accepted as proof of this step.
func (t *PrivilegeTeardown) RetireCleanupServiceAccount(ctx context.Context) error {
	state, err := t.inspect(ctx)
	if err != nil {
		return fmt.Errorf("verify residual privilege boundary before cleanup ServiceAccount retirement: %w", err)
	}
	if len(state.normalClusterBindings) != 0 || len(state.normalRoleBindings) != 0 ||
		len(state.privilegeRoleBindings) != 0 || state.privilegeClusterBinding != nil ||
		state.privilegeClusterRole != nil || len(state.serviceAccounts) != 0 {
		return errors.New("non-residual privilege remains before cleanup ServiceAccount retirement")
	}

	contract := privilegeServiceAccountContract{
		name:      t.cleanupAccountName,
		required:  true,
		component: "crd-manager-teardown",
	}
	identity, found, err := t.inspectServiceAccount(ctx, contract)
	if err != nil {
		return fmt.Errorf("re-verify cleanup ServiceAccount/%s before retirement: %w", t.cleanupAccountName, err)
	}
	if !found {
		return fmt.Errorf("cleanup ServiceAccount/%s is missing before retirement", t.cleanupAccountName)
	}
	if err := t.serviceAccounts.Delete(ctx, t.cleanupAccountName, identity.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete cleanup ServiceAccount/%s: %w", t.cleanupAccountName, err)
	}
	account, err := t.serviceAccounts.Get(ctx, t.cleanupAccountName, metav1.GetOptions{})
	// A prompt authenticator may invalidate this Pod's bound token before the
	// confirmation GET. That is stronger retirement evidence than NotFound;
	// the caller's direct endpoint observer performs the remaining barrier.
	if apierrors.IsNotFound(err) || apierrors.IsUnauthorized(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify cleanup ServiceAccount/%s retirement: %w", t.cleanupAccountName, err)
	}
	if account == nil {
		return fmt.Errorf("verify cleanup ServiceAccount/%s retirement: API returned a nil object", t.cleanupAccountName)
	}
	return fmt.Errorf("cleanup ServiceAccount/%s remains after exact retirement", t.cleanupAccountName)
}

type privilegeTeardownState struct {
	normalClusterBindings   []privilegeBindingTarget
	normalRoleBindings      []privilegeBindingTarget
	privilegeClusterBinding *privilegeBindingTarget
	privilegeRoleBindings   []privilegeBindingTarget
	privilegeClusterRole    *privilegeBindingTarget
	serviceAccounts         []privilegeBindingTarget
}

type privilegeBindingTarget struct {
	name      string
	namespace string
	identity  teardownIdentity
}

type privilegeBindingContract struct {
	name               string
	namespace          string
	roleRef            rbacv1.RoleRef
	subject            rbacv1.Subject
	fixedSubjects      []rbacv1.Subject
	predecessorSubject *rbacv1.Subject
	component          string
	cluster            bool
	retained           bool
	selfRevoke         bool
	required           bool
	controllerOrder    int
	controllerBinding  bool
}

type privilegeServiceAccountContract struct {
	name        string
	component   string
	remove      bool
	required    bool
	external    bool
	expectedUID types.UID
}

type privilegeAuthorizationContract struct {
	name           string
	namespace      string
	component      string
	cluster        bool
	retired        bool
	probeSubject   string
	probeSubjects  []string
	rules          []rbacv1.PolicyRule
	alternateRules []rbacv1.PolicyRule
}

type privilegeControllerBindingState struct {
	order       int
	candidate   bool
	description string
}

func (t *PrivilegeTeardown) inspect(ctx context.Context) (privilegeTeardownState, error) {
	if err := t.validate(); err != nil {
		return privilegeTeardownState{}, err
	}
	state, err := t.inspectBindings(ctx)
	if err != nil {
		return privilegeTeardownState{}, err
	}
	allowMissingPrivilegeRole := len(state.normalClusterBindings) == 0 && state.privilegeClusterBinding == nil
	privilegeRole, privilegeRoleFound, err := t.inspectAuthorization(ctx, state, allowMissingPrivilegeRole)
	if err != nil {
		return privilegeTeardownState{}, err
	}
	if privilegeRoleFound {
		state.privilegeClusterRole = &privilegeBindingTarget{name: t.cleanupPrivilege, identity: privilegeRole}
	}
	for _, account := range t.serviceAccountContracts() {
		identity, found, err := t.inspectServiceAccount(ctx, account)
		if err != nil {
			return privilegeTeardownState{}, fmt.Errorf("verify ServiceAccount/%s: %w", account.name, err)
		}
		if account.required && !found {
			return privilegeTeardownState{}, fmt.Errorf("required ServiceAccount/%s is missing", account.name)
		}
		if found && account.remove {
			state.serviceAccounts = append(state.serviceAccounts, privilegeBindingTarget{name: account.name, identity: identity})
		}
	}
	if (len(state.normalClusterBindings) != 0 || state.privilegeClusterBinding != nil) && state.privilegeClusterRole == nil {
		return privilegeTeardownState{}, errors.New("cleanup privilege ClusterRole is required while its ClusterRoleBinding or cluster privilege targets remain")
	}
	if len(state.normalClusterBindings) != 0 && state.privilegeClusterBinding == nil {
		return privilegeTeardownState{}, errors.New("cleanup privilege ClusterRoleBinding is required while cluster privilege targets remain")
	}
	privilegeRoleBindings := make(map[string]struct{}, len(state.privilegeRoleBindings))
	for _, binding := range state.privilegeRoleBindings {
		privilegeRoleBindings[binding.namespace] = struct{}{}
	}
	needsReleasePrivilege := len(state.serviceAccounts) != 0
	needsCoordinationPrivilege := false
	for _, binding := range state.normalRoleBindings {
		if binding.namespace == t.rollout.ReleaseNamespace {
			needsReleasePrivilege = true
		}
		if binding.namespace == t.rollout.CoordinationNamespace && t.rollout.CoordinationNamespace != t.rollout.ReleaseNamespace {
			needsCoordinationPrivilege = true
		}
	}
	if _, found := privilegeRoleBindings[t.rollout.ReleaseNamespace]; needsReleasePrivilege && !found {
		return privilegeTeardownState{}, fmt.Errorf("cleanup privilege RoleBinding/%s/%s is required while release privilege targets remain", t.rollout.ReleaseNamespace, t.cleanupPrivilege)
	}
	if _, found := privilegeRoleBindings[t.rollout.CoordinationNamespace]; needsCoordinationPrivilege && !found {
		return privilegeTeardownState{}, fmt.Errorf("cleanup privilege RoleBinding/%s/%s is required while coordination privilege targets remain", t.rollout.CoordinationNamespace, t.cleanupPrivilege)
	}
	return state, nil
}

func (t *PrivilegeTeardown) inspectBindings(ctx context.Context) (privilegeTeardownState, error) {
	contracts := t.bindingContracts()
	roleContracts := make(map[string]privilegeBindingContract, len(contracts))
	clusterContracts := make(map[string]privilegeBindingContract, len(contracts))
	for _, contract := range contracts {
		key := privilegeBindingKey(contract.namespace, contract.name)
		if contract.cluster {
			clusterContracts[contract.name] = contract
		} else {
			roleContracts[key] = contract
		}
	}

	roleBindings, err := listPrivilegeRoleBindings(ctx, t.roleBindings)
	if err != nil {
		return privilegeTeardownState{}, err
	}

	clusterBindings, err := listPrivilegeClusterRoleBindings(ctx, t.clusterBindings)
	if err != nil {
		return privilegeTeardownState{}, err
	}

	protected := t.protectedSubjects()
	seenRequiredRoles := map[string]bool{}
	seenRequiredClusters := map[string]bool{}
	state := privilegeTeardownState{}
	controllerStates := make([]privilegeControllerBindingState, 0, 3)

	for index := range roleBindings {
		binding := &roleBindings[index]
		key := privilegeBindingKey(binding.Namespace, binding.Name)
		contract, expected := roleContracts[key]
		if !expected {
			if privilegeBindingTouchesProtected(binding.Subjects, protected) {
				return privilegeTeardownState{}, fmt.Errorf("foreign RoleBinding/%s/%s names a protected ServiceAccount", binding.Namespace, binding.Name)
			}
			continue
		}
		identity, err := t.verifyRoleBinding(binding, contract)
		if err != nil {
			return privilegeTeardownState{}, err
		}
		if contract.controllerBinding {
			candidate, stateErr := privilegeControllerBindingCandidate(binding.Subjects, contract)
			if stateErr != nil {
				return privilegeTeardownState{}, stateErr
			}
			controllerStates = append(controllerStates, privilegeControllerBindingState{
				order: contract.controllerOrder, candidate: candidate,
				description: "RoleBinding/" + binding.Namespace + "/" + binding.Name,
			})
		}
		if contract.required {
			seenRequiredRoles[key] = true
		}
		if contract.selfRevoke {
			state.privilegeRoleBindings = append(state.privilegeRoleBindings, privilegeBindingTarget{
				name: binding.Name, namespace: binding.Namespace, identity: identity,
			})
		} else if !contract.retained {
			state.normalRoleBindings = append(state.normalRoleBindings, privilegeBindingTarget{
				name: binding.Name, namespace: binding.Namespace, identity: identity,
			})
		}
	}

	for index := range clusterBindings {
		binding := &clusterBindings[index]
		contract, expected := clusterContracts[binding.Name]
		if !expected {
			if privilegeBindingTouchesProtected(binding.Subjects, protected) {
				return privilegeTeardownState{}, fmt.Errorf("foreign ClusterRoleBinding/%s names a protected ServiceAccount", binding.Name)
			}
			continue
		}
		identity, err := t.verifyClusterRoleBinding(binding, contract)
		if err != nil {
			return privilegeTeardownState{}, err
		}
		if contract.controllerBinding {
			candidate, stateErr := privilegeControllerBindingCandidate(binding.Subjects, contract)
			if stateErr != nil {
				return privilegeTeardownState{}, stateErr
			}
			controllerStates = append(controllerStates, privilegeControllerBindingState{
				order: contract.controllerOrder, candidate: candidate,
				description: "ClusterRoleBinding/" + binding.Name,
			})
		}
		if contract.required {
			seenRequiredClusters[binding.Name] = true
		}
		if contract.selfRevoke {
			state.privilegeClusterBinding = &privilegeBindingTarget{name: binding.Name, identity: identity}
		} else if !contract.retained {
			state.normalClusterBindings = append(state.normalClusterBindings, privilegeBindingTarget{
				name: binding.Name, identity: identity,
			})
		}
	}

	for _, contract := range contracts {
		if !contract.required {
			continue
		}
		if contract.cluster && !seenRequiredClusters[contract.name] {
			return privilegeTeardownState{}, fmt.Errorf("required ClusterRoleBinding/%s is missing", contract.name)
		}
		if !contract.cluster && !seenRequiredRoles[privilegeBindingKey(contract.namespace, contract.name)] {
			return privilegeTeardownState{}, fmt.Errorf("required RoleBinding/%s/%s is missing", contract.namespace, contract.name)
		}
	}
	if err := validatePrivilegeControllerBindingPrefix(controllerStates); err != nil {
		return privilegeTeardownState{}, err
	}

	order := make(map[string]int, len(contracts))
	for index, contract := range contracts {
		order[privilegeBindingOrderKey(contract.cluster, contract.namespace, contract.name)] = index
	}
	sort.Slice(state.normalClusterBindings, func(i, j int) bool {
		return order[privilegeBindingOrderKey(true, "", state.normalClusterBindings[i].name)] <
			order[privilegeBindingOrderKey(true, "", state.normalClusterBindings[j].name)]
	})
	sort.Slice(state.normalRoleBindings, func(i, j int) bool {
		return order[privilegeBindingOrderKey(false, state.normalRoleBindings[i].namespace, state.normalRoleBindings[i].name)] <
			order[privilegeBindingOrderKey(false, state.normalRoleBindings[j].namespace, state.normalRoleBindings[j].name)]
	})
	sort.Slice(state.privilegeRoleBindings, func(i, j int) bool {
		return order[privilegeBindingOrderKey(false, state.privilegeRoleBindings[i].namespace, state.privilegeRoleBindings[i].name)] <
			order[privilegeBindingOrderKey(false, state.privilegeRoleBindings[j].namespace, state.privilegeRoleBindings[j].name)]
	})
	return state, nil
}

func privilegeControllerBindingCandidate(subjects []rbacv1.Subject, contract privilegeBindingContract) (bool, error) {
	candidateSubjects := append([]rbacv1.Subject{contract.subject}, contract.fixedSubjects...)
	if reflect.DeepEqual(subjects, candidateSubjects) {
		return true, nil
	}
	predecessorSubjects := append([]rbacv1.Subject(nil), contract.fixedSubjects...)
	if contract.predecessorSubject != nil {
		predecessorSubjects = append([]rbacv1.Subject{*contract.predecessorSubject}, contract.fixedSubjects...)
	}
	if contract.predecessorSubject != nil && reflect.DeepEqual(subjects, predecessorSubjects) {
		return false, nil
	}
	kind := "RoleBinding"
	name := contract.namespace + "/" + contract.name
	if contract.cluster {
		kind = "ClusterRoleBinding"
		name = contract.name
	}
	return false, fmt.Errorf("%s/%s differs from the exact candidate or predecessor controller subject contract", kind, name)
}

func validatePrivilegeControllerBindingPrefix(states []privilegeControllerBindingState) error {
	sort.Slice(states, func(i, j int) bool { return states[i].order < states[j].order })
	seenPredecessor := false
	for _, state := range states {
		if !state.candidate {
			seenPredecessor = true
			continue
		}
		if seenPredecessor {
			return fmt.Errorf("controller RBAC bindings are not a valid candidate prefix at %s", state.description)
		}
	}
	return nil
}

func (t *PrivilegeTeardown) bindingContracts() []privilegeBindingContract {
	serviceAccountSubject := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: t.rollout.ReleaseNamespace}
	}
	roleRef := func(kind, name string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kind, Name: name}
	}
	controller := t.rollout.ControllerDeploymentName
	hook := t.rollout.HookServiceAccountName
	bootstrap := privilegeHookBindingName(hook, 53, "-bootstrap")
	probe := privilegeHookBindingName(hook, 57, "-probe")
	quiesce, _ := TeardownQuiesceJobName(hook)

	var predecessorSubject *rbacv1.Subject
	if t.rollout.PreviousControllerServiceAccountName != "" {
		previous := serviceAccountSubject(t.rollout.PreviousControllerServiceAccountName)
		predecessorSubject = &previous
	}
	contracts := []privilegeBindingContract{
		{
			name: controller, roleRef: roleRef("ClusterRole", controller),
			subject:            serviceAccountSubject(t.contract.ControllerServiceAccountName),
			predecessorSubject: predecessorSubject, cluster: true,
			controllerBinding: predecessorSubject != nil, controllerOrder: 0,
		},
	}
	if t.contract.CertificateRuntimeEnabled {
		certificate := t.contract.CertificateServiceAccountName
		contracts = append(contracts, privilegeBindingContract{
			name: certificate, roleRef: roleRef("ClusterRole", certificate), subject: serviceAccountSubject(certificate), component: "certificate-rotation", cluster: true,
		})
	}
	runtimeBinding := privilegeBindingContract{
		name: controller + "-runtime-admission", namespace: t.rollout.ReleaseNamespace,
		roleRef: roleRef("Role", controller+"-runtime-admission"),
		subject: serviceAccountSubject(t.contract.ControllerServiceAccountName),
		fixedSubjects: []rbacv1.Subject{
			serviceAccountSubject(t.contract.CertificateServiceAccountName),
		},
	}
	coordinationOrder := 1
	if predecessorSubject != nil {
		runtimeBinding.controllerBinding = true
		if t.rollout.PreviousControllerReleaseSequence >= 1 {
			runtimeBinding.predecessorSubject = predecessorSubject
			runtimeBinding.controllerOrder = 1
			coordinationOrder = 2
		} else {
			// Sequence zero did not have this binding. Its presence therefore
			// proves ordinary apply reached the candidate-only post-cutover RBAC.
			runtimeBinding.controllerOrder = 2
		}
	}
	contracts = append(contracts,
		privilegeBindingContract{name: hook, roleRef: roleRef("ClusterRole", hook), subject: serviceAccountSubject(hook), component: "crd-manager", cluster: true},
		privilegeBindingContract{name: bootstrap, roleRef: roleRef("ClusterRole", bootstrap), subject: serviceAccountSubject(hook), component: "hook-identity-bootstrap", cluster: true},
		privilegeBindingContract{name: quiesce, roleRef: roleRef("ClusterRole", quiesce), subject: serviceAccountSubject(hook), component: "crd-manager-teardown-quiesce", cluster: true},
		privilegeBindingContract{name: t.cleanupPrivilege, roleRef: roleRef("ClusterRole", t.cleanupPrivilege), subject: serviceAccountSubject(t.cleanupAccountName), component: "crd-manager-teardown", cluster: true, selfRevoke: true},
		privilegeBindingContract{name: t.residualGuard, roleRef: roleRef("ClusterRole", t.residualGuard), subject: serviceAccountSubject(t.cleanupAccountName), component: "crd-manager-teardown", cluster: true, retained: true, required: true},
		runtimeBinding,
		privilegeBindingContract{
			name: controller, namespace: t.rollout.CoordinationNamespace,
			roleRef: roleRef("Role", controller), subject: serviceAccountSubject(t.contract.ControllerServiceAccountName),
			predecessorSubject: predecessorSubject, controllerBinding: predecessorSubject != nil,
			controllerOrder: coordinationOrder,
		},
	)
	if t.contract.CertificateRuntimeEnabled {
		certificate := t.contract.CertificateServiceAccountName
		contracts = append(contracts, privilegeBindingContract{
			name: certificate, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", certificate), subject: serviceAccountSubject(certificate), component: "certificate-rotation",
		})
	}
	contracts = append(contracts,
		privilegeBindingContract{name: hook, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", hook), subject: serviceAccountSubject(hook), component: "crd-manager"},
		privilegeBindingContract{name: bootstrap, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", bootstrap), subject: serviceAccountSubject(hook), component: "hook-identity-bootstrap"},
		privilegeBindingContract{name: probe, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", probe), subject: serviceAccountSubject(hook), component: "crd-manager"},
		privilegeBindingContract{name: quiesce, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", quiesce), subject: serviceAccountSubject(hook), component: "crd-manager-teardown-quiesce"},
		privilegeBindingContract{name: t.cleanupPrivilege, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", t.cleanupPrivilege), subject: serviceAccountSubject(t.cleanupAccountName), component: "crd-manager-teardown", selfRevoke: true},
		privilegeBindingContract{name: t.residualRelease, namespace: t.rollout.ReleaseNamespace, roleRef: roleRef("Role", t.residualRelease), subject: serviceAccountSubject(t.cleanupAccountName), component: "crd-manager-teardown", retained: true, required: true},
		privilegeBindingContract{name: t.residualDiscovery, namespace: t.discoveryNamespace, roleRef: roleRef("Role", t.residualDiscovery), subject: serviceAccountSubject(t.cleanupAccountName), component: "crd-manager-teardown", retained: true, required: true},
	)
	if t.rollout.CoordinationNamespace != t.rollout.ReleaseNamespace {
		contracts = append(contracts, privilegeBindingContract{
			name: t.cleanupPrivilege, namespace: t.rollout.CoordinationNamespace,
			roleRef: roleRef("Role", t.cleanupPrivilege), subject: serviceAccountSubject(t.cleanupAccountName),
			component: "crd-manager-teardown", selfRevoke: true,
		})
	}
	return contracts
}

func (t *PrivilegeTeardown) serviceAccountContracts() []privilegeServiceAccountContract {
	accounts := make([]privilegeServiceAccountContract, 0, 5)
	if t.contract.ControllerServiceAccountCreate {
		accounts = append(accounts, privilegeServiceAccountContract{name: t.contract.ControllerServiceAccountName, remove: true})
	}
	if t.rollout.PreviousControllerServiceAccountName != "" {
		accounts = append(accounts, privilegeServiceAccountContract{
			name:        t.rollout.PreviousControllerServiceAccountName,
			external:    true,
			expectedUID: t.rollout.PreviousControllerServiceAccountUID,
		})
	}
	if t.contract.CertificateRuntimeEnabled {
		accounts = append(accounts, privilegeServiceAccountContract{
			name: t.contract.CertificateServiceAccountName, component: "certificate-rotation", remove: true,
		})
	}
	accounts = append(accounts,
		privilegeServiceAccountContract{name: t.rollout.HookServiceAccountName, component: "crd-manager", remove: true},
		privilegeServiceAccountContract{name: t.cleanupAccountName, component: "crd-manager-teardown", required: true},
	)
	return accounts
}

func (t *PrivilegeTeardown) retiredAuthorizationContracts() []privilegeAuthorizationContract {
	controller := t.rollout.ControllerDeploymentName
	hook := t.rollout.HookServiceAccountName
	bootstrap := privilegeHookBindingName(hook, 53, "-bootstrap")
	probe := privilegeHookBindingName(hook, 57, "-probe")
	crdNames := []string{
		"ptahschemaapprovals.operator.ptah.dev",
		"ptahschemaplans.operator.ptah.dev",
		"ptahschemas.operator.ptah.dev",
	}
	runtimeGuardNames := t.runtimeAdmissionGuardNames()
	hookServiceAccounts := []string{t.contract.ControllerServiceAccountName, t.contract.CertificateServiceAccountName}
	if t.rollout.PreviousControllerServiceAccountName != "" {
		hookServiceAccounts = append(hookServiceAccounts, t.rollout.PreviousControllerServiceAccountName)
	}
	contracts := []privilegeAuthorizationContract{
		{
			name: controller, cluster: true, retired: true, probeSubject: "controller",
			rules:          currentControllerClusterRoleRules(t.rollout),
			alternateRules: legacyControllerClusterRoleRules(),
		},
		{
			name: controller + "-runtime-admission", namespace: t.rollout.ReleaseNamespace, retired: true, probeSubject: "controller",
			probeSubjects: func() []string {
				if t.contract.CertificateRuntimeEnabled {
					return []string{"certificate"}
				}
				return nil
			}(),
			rules: currentControllerRuntimeRoleRules(t.rollout, t.contract),
		},
		{
			name: controller, namespace: t.rollout.CoordinationNamespace, retired: true, probeSubject: "controller",
			rules: currentControllerCoordinationRoleRules(),
		},
		{
			name: hook, component: "crd-manager", cluster: true, retired: true, probeSubject: "hook-quiesce",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get", "update"}),
				privilegePolicyRule([]string{"operator.ptah.dev"}, []string{"ptahschemas", "ptahschemaplans", "ptahschemaapprovals"}, nil, []string{"list"}),
				privilegePolicyRule(
					[]string{"admissionregistration.k8s.io"},
					[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
					[]string{AdmissionConfigurationName},
					[]string{"get", "update"},
				),
				privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies"}, currentCRDManagerAdmissionGuardNames(t.rollout), []string{"get"}),
				privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicybindings"}, currentCRDManagerAdmissionGuardNames(t.rollout), []string{"get"}),
				privilegePolicyRule([]string{"scheduling.k8s.io"}, []string{"priorityclasses"}, nil, []string{"get", "list"}),
				privilegePolicyRule([]string{"rbac.authorization.k8s.io"}, []string{"clusterrolebindings"}, nil, []string{"list"}),
				privilegePolicyRule([]string{"rbac.authorization.k8s.io"}, []string{"clusterrolebindings"}, []string{controller}, []string{"get", "patch"}),
				privilegePolicyRule([]string{"rbac.authorization.k8s.io"}, []string{"rolebindings"}, nil, []string{"list"}),
				privilegePolicyRule([]string{"rbac.authorization.k8s.io"}, []string{"rolebindings"}, []string{controller, controller + "-runtime-admission"}, []string{"get", "patch"}),
				privilegePolicyRule([]string{"rbac.authorization.k8s.io"}, []string{"clusterroles"}, []string{controller}, []string{"get"}),
				privilegePolicyRule([]string{"rbac.authorization.k8s.io"}, []string{"roles"}, []string{controller, controller + "-runtime-admission"}, []string{"get"}),
				privilegePolicyRule([]string{"authorization.k8s.io"}, []string{"subjectaccessreviews"}, nil, []string{"create"}),
				privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list"}),
			},
		},
		{
			name: hook, namespace: t.rollout.ReleaseNamespace, component: "crd-manager", retired: true, probeSubject: "hook-quiesce",
			rules: func() []rbacv1.PolicyRule {
				rules := []rbacv1.PolicyRule{
					privilegePolicyRule(
						[]string{"apps"}, []string{"deployments"},
						[]string{t.rollout.ControllerDeploymentName, t.rollout.CertificateDeploymentName},
						[]string{"get", "update"},
					),
					privilegePolicyRule([]string{"apps"}, []string{"replicasets"}, nil, []string{"list"}),
					privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
					privilegePolicyRule(
						[]string{""}, []string{"serviceaccounts"},
						hookServiceAccounts,
						[]string{"get"},
					),
					privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
					privilegePolicyRule([]string{""}, []string{"resourcequotas"}, nil, []string{"list"}),
					privilegePolicyRule(
						[]string{""}, []string{"configmaps"},
						[]string{ReleaseActivationName, AdmissionConvergenceMarkerName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence)},
						[]string{"get", "update"},
					),
				}
				if t.rollout.PreviousControllerReleaseSequence > 0 {
					rules = append(rules, privilegePolicyRule(
						[]string{""}, []string{"configmaps"},
						[]string{AdmissionConvergenceMarkerName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.PreviousControllerReleaseSequence)},
						[]string{"get", "delete"},
					))
				}
				return rules
			}(),
		},
		{
			name: bootstrap, component: "hook-identity-bootstrap", cluster: true, retired: true, probeSubject: "hook-quiesce",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies"}, t.bootstrapAdmissionGuardNames(), []string{"get"}),
				privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicybindings"}, t.bootstrapAdmissionGuardNames(), []string{"get"}),
			},
		},
		{
			name: bootstrap, namespace: t.rollout.ReleaseNamespace, component: "hook-identity-bootstrap", retired: true, probeSubject: "hook-quiesce",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{""}, []string{"configmaps"}, []string{HookIdentityProbeObjectName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage)}, []string{"get", "update"}),
				privilegePolicyRule([]string{"batch"}, []string{"jobs"}, nil, []string{"list"}),
				privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
			},
		},
		{
			name: probe, namespace: t.rollout.ReleaseNamespace, component: "crd-manager", retired: true, probeSubject: "hook-quiesce",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"apps"}, []string{"deployments"}, nil, []string{"create"}),
			},
		},
	}

	if t.contract.CertificateRuntimeEnabled {
		certificate := t.contract.CertificateServiceAccountName
		certificateClusterRules := []rbacv1.PolicyRule{
			privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get"}),
			privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"mutatingwebhookconfigurations"}, []string{AdmissionConfigurationName}, []string{"get", "update"}),
			privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingwebhookconfigurations"}, []string{AdmissionConfigurationName}, []string{"get", "update"}),
			privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"}, runtimeGuardNames, []string{"get"}),
			privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list"}),
			privilegePolicyRule([]string{"scheduling.k8s.io"}, []string{"priorityclasses"}, nil, []string{"get", "list"}),
		}
		certificateRoleRules := []rbacv1.PolicyRule{
			privilegePolicyRule(
				[]string{""},
				[]string{"secrets"},
				[]string{t.rollout.WebhookSecretName, t.certificateStagingSecretName()},
				[]string{"get", "update"},
			),
		}
		if t.certificateRecreatesMissingSecret() {
			certificateRoleRules = append(certificateRoleRules, privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"create"}))
			certificateClusterRules = append(certificateClusterRules, privilegePolicyRule(
				[]string{"admissionregistration.k8s.io"},
				[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
				[]string{certificate},
				[]string{"get"},
			))
		}
		certificateRoleRules = append(certificateRoleRules,
			privilegePolicyRule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{t.certificateLeaseName()}, []string{"get", "update"}),
			privilegePolicyRule(
				[]string{""}, []string{"serviceaccounts"},
				[]string{t.contract.ControllerServiceAccountName, certificate},
				[]string{"get"},
			),
			privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
		)
		contracts = append(contracts,
			privilegeAuthorizationContract{name: certificate, component: "certificate-rotation", cluster: true, retired: true, probeSubject: "certificate", rules: certificateClusterRules},
			privilegeAuthorizationContract{name: certificate, namespace: t.rollout.ReleaseNamespace, component: "certificate-rotation", retired: true, probeSubject: "certificate", rules: certificateRoleRules},
		)
	}
	return contracts
}

func (t *PrivilegeTeardown) authorizationContracts() []privilegeAuthorizationContract {
	contracts := t.retiredAuthorizationContracts()
	return append(contracts, t.teardownAuthorizationContracts()...)
}

// RevokedPrivilegeMutationGrant is one mutating RBAC tuple that an exact
// teardown convergence probe must observe as no longer allowed. ClusterWide
// means the source grant came from a ClusterRole and therefore applies in any
// namespace appropriate for the resource. An empty ResourceNames slice means
// the source rule was not name-bounded.
type RevokedPrivilegeMutationGrant struct {
	SubjectName    string
	Namespace      string
	ClusterWide    bool
	APIGroup       string
	Resource       string
	Subresource    string
	Verb           string
	ResourceNames  []string
	NonResourceURL string
}

// RevokedPrivilegeMutationGrants compiles the mutating portions of every
// exact role whose binding is removed before the authorization convergence
// barrier. It is the shared completeness contract between storage preflight
// and live SAR/SelfSAR probes.
func RevokedPrivilegeMutationGrants(
	rollout *RolloutGuard,
	contract RuntimeAdmissionContract,
) ([]RevokedPrivilegeMutationGrant, error) {
	if rollout == nil {
		return nil, errors.New("rollout guard is required for revoked privilege grants")
	}
	if rollout.ReleaseNamespace == "" || rollout.CoordinationNamespace == "" {
		return nil, errors.New("release and coordination namespaces are required for revoked privilege grants")
	}
	if contract.Namespace != rollout.ReleaseNamespace {
		return nil, fmt.Errorf("runtime admission namespace %q differs from release namespace %q", contract.Namespace, rollout.ReleaseNamespace)
	}
	cleanupAccount, err := TeardownServiceAccountName(rollout.HookServiceAccountName, rollout.ReleaseSequence)
	if err != nil {
		return nil, fmt.Errorf("derive cleanup ServiceAccount for revoked privilege grants: %w", err)
	}
	cleanupPrivilege, err := TeardownPrivilegeRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return nil, fmt.Errorf("derive cleanup privilege for revoked privilege grants: %w", err)
	}
	residual, err := TeardownGuardRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return nil, fmt.Errorf("derive residual guard for revoked privilege grants: %w", err)
	}
	discovery, err := TeardownDiscoveryRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return nil, fmt.Errorf("derive residual discovery role for revoked privilege grants: %w", err)
	}
	teardown := &PrivilegeTeardown{
		rollout:            rollout,
		contract:           contract,
		cleanupAccountName: cleanupAccount,
		cleanupPrivilege:   cleanupPrivilege,
		residualGuard:      residual,
		residualRelease:    residual,
		residualDiscovery:  discovery,
		discoveryNamespace: metav1.NamespaceDefault,
	}
	if contract.CertificateRuntimeEnabled && teardown.certificateLeaseName() == "" {
		return nil, errors.New("certificate rotation --lease-name is required for revoked privilege grants")
	}

	var grants []RevokedPrivilegeMutationGrant
	for _, authorization := range teardown.authorizationContracts() {
		if authorization.probeSubject == "" {
			continue
		}
		subjects := append([]string{authorization.probeSubject}, authorization.probeSubjects...)
		if authorization.probeSubject == "controller" && rollout.PreviousControllerServiceAccountName != "" {
			subjects = append(subjects, "previous-controller")
		}
		rules := authorization.rules
		if rollout.PreviousControllerServiceAccountName != "" {
			rules = exactPrivilegeAuthorizationRules(authorization)
		}
		for _, subject := range subjects {
			for _, rule := range rules {
				for _, verb := range rule.Verbs {
					if verb == "get" || verb == "list" || verb == "watch" {
						continue
					}
					for _, group := range rule.APIGroups {
						for _, resource := range rule.Resources {
							base, subresource, _ := strings.Cut(resource, "/")
							grants = append(grants, RevokedPrivilegeMutationGrant{
								SubjectName:   subject,
								Namespace:     authorization.namespace,
								ClusterWide:   authorization.cluster,
								APIGroup:      group,
								Resource:      base,
								Subresource:   subresource,
								Verb:          verb,
								ResourceNames: append([]string(nil), rule.ResourceNames...),
							})
						}
					}
					for _, path := range rule.NonResourceURLs {
						grants = append(grants, RevokedPrivilegeMutationGrant{
							SubjectName:    subject,
							ClusterWide:    true,
							Verb:           verb,
							NonResourceURL: path,
						})
					}
				}
			}
		}
	}
	return grants, nil
}

func exactPrivilegeAuthorizationRules(contract privilegeAuthorizationContract) []rbacv1.PolicyRule {
	rules := append([]rbacv1.PolicyRule(nil), contract.rules...)
	for _, alternate := range contract.alternateRules {
		duplicate := false
		for _, rule := range rules {
			if reflect.DeepEqual(rule, alternate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			rules = append(rules, alternate)
		}
	}
	return rules
}

func (t *PrivilegeTeardown) teardownAuthorizationContracts() []privilegeAuthorizationContract {
	hook := t.rollout.HookServiceAccountName
	quiesce, _ := TeardownQuiesceJobName(hook)
	bootstrap := privilegeHookBindingName(hook, 53, "-bootstrap")
	probe := privilegeHookBindingName(hook, 57, "-probe")
	clusterRoleNames := []string{t.rollout.ControllerDeploymentName}
	roleNames := []string{
		t.rollout.ControllerDeploymentName + "-runtime-admission",
		t.rollout.ControllerDeploymentName,
	}
	if t.contract.CertificateRuntimeEnabled {
		clusterRoleNames = append(clusterRoleNames, t.contract.CertificateServiceAccountName)
		roleNames = append(roleNames, t.contract.CertificateServiceAccountName)
	}
	clusterRoleNames = append(clusterRoleNames, hook, bootstrap, quiesce, t.cleanupPrivilege, t.residualGuard)
	roleNames = append(roleNames, hook, bootstrap, probe, quiesce, t.cleanupPrivilege, t.residualRelease, t.residualDiscovery)

	contracts := []privilegeAuthorizationContract{
		{
			name: quiesce, component: "crd-manager-teardown-quiesce", cluster: true, probeSubject: "hook-quiesce",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule(
					[]string{"discovery.k8s.io"},
					[]string{"endpointslices"},
					nil,
					[]string{"list"},
				),
				privilegePolicyRule(
					[]string{"admissionregistration.k8s.io"},
					[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
					[]string{AdmissionConfigurationName},
					[]string{"get"},
				),
				privilegePolicyRule(
					[]string{"admissionregistration.k8s.io"},
					[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
					t.privilegeAdmissionGuardNames(),
					[]string{"get"},
				),
				privilegePolicyRule(
					[]string{rbacv1.GroupName},
					[]string{"rolebindings", "clusterrolebindings"},
					nil,
					[]string{"list"},
				),
				privilegePolicyRule(
					[]string{rbacv1.GroupName},
					[]string{"clusterroles"},
					clusterRoleNames,
					[]string{"get"},
				),
				privilegePolicyRule(
					[]string{rbacv1.GroupName},
					[]string{"roles"},
					roleNames,
					[]string{"get"},
				),
			},
		},
		{
			name: quiesce, namespace: t.rollout.ReleaseNamespace, component: "crd-manager-teardown-quiesce", probeSubject: "hook-quiesce",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule(
					[]string{"apps"}, []string{"deployments"},
					[]string{t.rollout.ControllerDeploymentName, t.rollout.CertificateDeploymentName},
					[]string{"get", "update"},
				),
				privilegePolicyRule([]string{"apps"}, []string{"replicasets"}, nil, []string{"list"}),
				privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
				privilegePolicyRule([]string{""}, []string{"serviceaccounts"}, t.privilegeServiceAccountNames(), []string{"get"}),
				privilegePolicyRule(
					[]string{""}, []string{"configmaps"},
					[]string{
						ReleaseActivationName,
						AdmissionConvergenceMarkerName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence),
						t.retirementMarkerName(),
					},
					[]string{"get", "update"},
				),
				privilegePolicyRule(
					[]string{""}, []string{"configmaps"},
					[]string{
						HookIdentityProbeObjectName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
					},
					[]string{"get"},
				),
			},
		},
		t.cleanupPrivilegeContract(),
		{
			name: t.cleanupPrivilege, namespace: t.rollout.ReleaseNamespace, component: "crd-manager-teardown", probeSubject: "cleanup",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule(
					[]string{rbacv1.GroupName}, []string{"rolebindings"},
					t.releaseRoleBindingDeletionNames(), []string{"delete"},
				),
				privilegePolicyRule(
					[]string{""}, []string{"serviceaccounts"},
					t.removedServiceAccountNames(), []string{"delete"},
				),
			},
		},
	}
	if t.rollout.CoordinationNamespace != t.rollout.ReleaseNamespace {
		contracts = append(contracts, privilegeAuthorizationContract{
			name: t.cleanupPrivilege, namespace: t.rollout.CoordinationNamespace, component: "crd-manager-teardown", probeSubject: "cleanup",
			rules: []rbacv1.PolicyRule{privilegePolicyRule(
				[]string{rbacv1.GroupName}, []string{"rolebindings"},
				[]string{t.rollout.ControllerDeploymentName, t.cleanupPrivilege}, []string{"delete"},
			)},
		})
	}
	contracts = append(contracts,
		privilegeAuthorizationContract{
			name: t.residualGuard, component: "crd-manager-teardown", cluster: true,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{rbacv1.GroupName}, []string{"rolebindings", "clusterrolebindings"}, nil, []string{"list"}),
				privilegePolicyRule([]string{rbacv1.GroupName}, []string{"clusterrolebindings"}, []string{t.cleanupPrivilege}, []string{"get", "delete"}),
				privilegePolicyRule([]string{rbacv1.GroupName}, []string{"clusterroles"}, clusterRoleNames, []string{"get"}),
				privilegePolicyRule([]string{rbacv1.GroupName}, []string{"clusterroles"}, []string{t.cleanupPrivilege}, []string{"delete"}),
				privilegePolicyRule([]string{rbacv1.GroupName}, []string{"roles"}, roleNames, []string{"get"}),
				privilegePolicyRule(
					[]string{"admissionregistration.k8s.io"},
					[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
					[]string{AdmissionConfigurationName},
					[]string{"get"},
				),
				privilegePolicyRule(
					[]string{"admissionregistration.k8s.io"},
					[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
					t.privilegeAdmissionGuardNames(),
					[]string{"get"},
				),
				privilegePolicyRule(
					[]string{"authorization.k8s.io"},
					[]string{"subjectaccessreviews", "selfsubjectaccessreviews"},
					nil,
					[]string{"create"},
				),
			},
		},
		privilegeAuthorizationContract{
			name: t.residualRelease, namespace: t.rollout.ReleaseNamespace, component: "crd-manager-teardown",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
				privilegePolicyRule([]string{""}, []string{"serviceaccounts"}, t.privilegeServiceAccountNames(), []string{"get"}),
				privilegePolicyRule([]string{""}, []string{"serviceaccounts"}, []string{t.cleanupAccountName}, []string{"delete"}),
				privilegePolicyRule(
					[]string{""}, []string{"configmaps"},
					[]string{HookIdentityProbeObjectName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage)},
					[]string{"get"},
				),
				privilegePolicyRule(
					[]string{""}, []string{"configmaps"},
					[]string{ReleaseActivationName},
					[]string{"get", "delete"},
				),
				privilegePolicyRule(
					[]string{""}, []string{"configmaps"},
					[]string{AdmissionConvergenceMarkerName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence)},
					[]string{"get", "update", "delete"},
				),
				privilegePolicyRule(
					[]string{""}, []string{"configmaps"},
					[]string{t.retirementMarkerName()},
					[]string{"get", "update"},
				),
			},
		},
		privilegeAuthorizationContract{
			name: t.residualDiscovery, namespace: t.discoveryNamespace, component: "crd-manager-teardown",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list", "watch"}),
			},
		},
	)
	return contracts
}

func (t *PrivilegeTeardown) cleanupPrivilegeContract() privilegeAuthorizationContract {
	return privilegeAuthorizationContract{
		name: t.cleanupPrivilege, component: "crd-manager-teardown", cluster: true, probeSubject: "cleanup",
		rules: []rbacv1.PolicyRule{privilegePolicyRule(
			[]string{rbacv1.GroupName}, []string{"clusterrolebindings"},
			t.clusterRoleBindingDeletionNames(), []string{"delete"},
		)},
	}
}

func (t *PrivilegeTeardown) privilegeAdmissionGuardNames() []string {
	names := currentRetainedAdmissionGuardNames(t.rollout)
	names = append(names, legacyControllerGuardNames(t.rollout.ReleaseNamespace, t.rollout.ReleaseName)...)
	for _, fence := range []TeardownFence{TeardownFenceA, TeardownFenceB} {
		name, _ := TeardownRetirementFenceName(
			fence,
			t.rollout.ReleaseNamespace,
			t.rollout.ReleaseName,
			t.rollout.ReleaseSequence,
			t.rollout.ManagerImage,
		)
		names = append(names, name)
	}
	if t.contract.CertificateRuntimeEnabled && t.certificateRecreatesMissingSecret() {
		names = append(names, t.contract.CertificateServiceAccountName)
	}
	return names
}

func (t *PrivilegeTeardown) retirementMarkerName() string {
	name, _ := TeardownRetirementProbeName(
		t.rollout.ReleaseNamespace,
		t.rollout.ReleaseName,
		t.rollout.ReleaseSequence,
		t.rollout.ManagerImage,
	)
	return name
}

func (t *PrivilegeTeardown) runtimeAdmissionGuardNames() []string {
	names := currentControllerRuntimeGuardNames(t.rollout)
	return append(names,
		legacyParentHookPodOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		legacyParentHookJobOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
	)
}

func (t *PrivilegeTeardown) bootstrapAdmissionGuardNames() []string {
	return []string{
		HookIdentityGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		HookIdentityProbeGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ServiceAccountObjectGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ServiceAccountOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerJobWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerChunkWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerPlanWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		CertificateMutatingWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		CertificateValidatingWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		NamespaceDeletionGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ParentReplicaSetGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ParentHookPodOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ParentHookJobOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ParentHookJobContractPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
	}
}

func (t *PrivilegeTeardown) certificateLeaseName() string {
	const prefix = "--lease-name="
	for _, argument := range t.rollout.CertificateArgs {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func (t *PrivilegeTeardown) certificateStagingSecretName() string {
	const prefix = "--staging-secret-name="
	for _, argument := range t.rollout.CertificateArgs {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func (t *PrivilegeTeardown) certificateRecreatesMissingSecret() bool {
	for _, argument := range t.rollout.CertificateArgs {
		if argument == "--recreate-missing-secret=true" {
			return true
		}
	}
	return false
}

func (t *PrivilegeTeardown) clusterRoleBindingDeletionNames() []string {
	hook := t.rollout.HookServiceAccountName
	names := []string{t.rollout.ControllerDeploymentName}
	if t.contract.CertificateRuntimeEnabled {
		names = append(names, t.contract.CertificateServiceAccountName)
	}
	quiesce, _ := TeardownQuiesceJobName(hook)
	return append(names,
		hook,
		privilegeHookBindingName(hook, 53, "-bootstrap"),
		quiesce,
	)
}

func (t *PrivilegeTeardown) releaseRoleBindingDeletionNames() []string {
	hook := t.rollout.HookServiceAccountName
	names := []string{t.rollout.ControllerDeploymentName + "-runtime-admission"}
	if t.rollout.CoordinationNamespace == t.rollout.ReleaseNamespace {
		names = append(names, t.rollout.ControllerDeploymentName)
	}
	if t.contract.CertificateRuntimeEnabled {
		names = append(names, t.contract.CertificateServiceAccountName)
	}
	quiesce, _ := TeardownQuiesceJobName(hook)
	return append(names,
		hook,
		privilegeHookBindingName(hook, 53, "-bootstrap"),
		privilegeHookBindingName(hook, 57, "-probe"),
		quiesce,
		t.cleanupPrivilege,
	)
}

func (t *PrivilegeTeardown) privilegeServiceAccountNames() []string {
	names := make([]string, 0, 4)
	for _, contract := range t.serviceAccountContracts() {
		names = append(names, contract.name)
	}
	return names
}

func (t *PrivilegeTeardown) removedServiceAccountNames() []string {
	names := make([]string, 0, 3)
	for _, contract := range t.serviceAccountContracts() {
		if contract.remove {
			names = append(names, contract.name)
		}
	}
	return names
}

func privilegePolicyRule(apiGroups, resources, resourceNames, verbs []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		APIGroups:     apiGroups,
		Resources:     resources,
		ResourceNames: resourceNames,
		Verbs:         verbs,
	}
}

func (t *PrivilegeTeardown) inspectAuthorization(
	ctx context.Context,
	state privilegeTeardownState,
	allowMissingPrivilegeRole bool,
) (teardownIdentity, bool, error) {
	var privilegeIdentity teardownIdentity
	privilegeFound := false
	for _, contract := range t.authorizationContracts() {
		var (
			identity teardownIdentity
			found    bool
			err      error
		)
		if contract.cluster {
			identity, found, err = t.inspectAuthorizationClusterRole(ctx, contract)
		} else {
			identity, found, err = t.inspectAuthorizationRole(ctx, contract)
		}
		kind := "Role"
		qualifiedName := contract.namespace + "/" + contract.name
		if contract.cluster {
			kind = "ClusterRole"
			qualifiedName = contract.name
		}
		if err != nil {
			return teardownIdentity{}, false, fmt.Errorf("verify %s/%s: %w", kind, qualifiedName, err)
		}
		isPrivilegeClusterRole := contract.cluster && contract.name == t.cleanupPrivilege
		retiredRoleRequired := contract.retired && retiredAuthorizationBindingPresent(contract, state)
		if !found && (retiredRoleRequired || (!contract.retired && (!isPrivilegeClusterRole || !allowMissingPrivilegeRole))) {
			return teardownIdentity{}, false, fmt.Errorf("required %s/%s is missing", kind, qualifiedName)
		}
		if isPrivilegeClusterRole && found {
			privilegeIdentity = identity
			privilegeFound = true
		}
	}
	return privilegeIdentity, privilegeFound, nil
}

func retiredAuthorizationBindingPresent(contract privilegeAuthorizationContract, state privilegeTeardownState) bool {
	if contract.cluster {
		for _, binding := range state.normalClusterBindings {
			if binding.name == contract.name {
				return true
			}
		}
		return false
	}
	for _, binding := range state.normalRoleBindings {
		if binding.namespace == contract.namespace && binding.name == contract.name {
			return true
		}
	}
	return false
}

type privilegeProtectedSubjects struct {
	serviceAccounts map[string]struct{}
	users           map[string]struct{}
	namespaceGroup  string
}

func (t *PrivilegeTeardown) protectedSubjects() privilegeProtectedSubjects {
	names := []string{
		t.contract.ControllerServiceAccountName,
		t.rollout.HookServiceAccountName,
		t.cleanupAccountName,
	}
	if t.rollout.PreviousControllerServiceAccountName != "" {
		names = append(names, t.rollout.PreviousControllerServiceAccountName)
	}
	if t.contract.CertificateRuntimeEnabled {
		names = append(names, t.contract.CertificateServiceAccountName)
	}
	protected := privilegeProtectedSubjects{
		serviceAccounts: make(map[string]struct{}, len(names)),
		users:           make(map[string]struct{}, len(names)),
		namespaceGroup:  "system:serviceaccounts:" + t.rollout.ReleaseNamespace,
	}
	for _, name := range names {
		protected.serviceAccounts[privilegeServiceAccountKey(t.rollout.ReleaseNamespace, name)] = struct{}{}
		protected.users["system:serviceaccount:"+t.rollout.ReleaseNamespace+":"+name] = struct{}{}
	}
	return protected
}

func (t *PrivilegeTeardown) verifyRoleBinding(binding *rbacv1.RoleBinding, contract privilegeBindingContract) (teardownIdentity, error) {
	if binding == nil || binding.Name != contract.name || binding.Namespace != contract.namespace {
		return teardownIdentity{}, fmt.Errorf("RoleBinding/%s/%s has an unexpected identity", contract.namespace, contract.name)
	}
	if err := t.verifyMetadata("RoleBinding", binding.ObjectMeta, contract.component); err != nil {
		return teardownIdentity{}, err
	}
	if !reflect.DeepEqual(binding.RoleRef, contract.roleRef) {
		return teardownIdentity{}, fmt.Errorf("RoleBinding/%s/%s differs from the exact privilege contract", contract.namespace, contract.name)
	}
	if _, err := privilegeControllerBindingCandidate(binding.Subjects, contract); err != nil {
		return teardownIdentity{}, err
	}
	return deletionIdentity("RoleBinding", contract.name, binding)
}

func (t *PrivilegeTeardown) verifyClusterRoleBinding(binding *rbacv1.ClusterRoleBinding, contract privilegeBindingContract) (teardownIdentity, error) {
	if binding == nil || binding.Name != contract.name || binding.Namespace != "" {
		return teardownIdentity{}, fmt.Errorf("ClusterRoleBinding/%s has an unexpected identity", contract.name)
	}
	if err := t.verifyMetadata("ClusterRoleBinding", binding.ObjectMeta, contract.component); err != nil {
		return teardownIdentity{}, err
	}
	if !reflect.DeepEqual(binding.RoleRef, contract.roleRef) {
		return teardownIdentity{}, fmt.Errorf("ClusterRoleBinding/%s differs from the exact privilege contract", contract.name)
	}
	if _, err := privilegeControllerBindingCandidate(binding.Subjects, contract); err != nil {
		return teardownIdentity{}, err
	}
	return deletionIdentity("ClusterRoleBinding", contract.name, binding)
}

func (t *PrivilegeTeardown) inspectServiceAccount(ctx context.Context, contract privilegeServiceAccountContract) (teardownIdentity, bool, error) {
	account, err := t.serviceAccounts.Get(ctx, contract.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return teardownIdentity{}, false, nil
	}
	if err != nil {
		return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
	}
	if account == nil {
		return teardownIdentity{}, false, errors.New("API returned a nil object")
	}
	if account.Name != contract.name || account.Namespace != t.rollout.ReleaseNamespace {
		return teardownIdentity{}, false, fmt.Errorf("ServiceAccount/%s has an unexpected identity", contract.name)
	}
	if !contract.external {
		if err := t.verifyMetadata("ServiceAccount", account.ObjectMeta, contract.component); err != nil {
			return teardownIdentity{}, false, err
		}
	}
	var identity teardownIdentity
	if contract.external {
		if account.DeletionTimestamp != nil {
			return teardownIdentity{}, false, fmt.Errorf("ServiceAccount/%s deletion is already in progress", contract.name)
		}
		if account.UID == "" || account.ResourceVersion == "" {
			return teardownIdentity{}, false, fmt.Errorf("ServiceAccount/%s lacks an identity UID or resource version", contract.name)
		}
		identity = teardownIdentity{uid: account.UID, resourceVersion: account.ResourceVersion}
	} else {
		identity, err = deletionIdentity("ServiceAccount", contract.name, account)
	}
	if err == nil && contract.expectedUID != "" && identity.uid != contract.expectedUID {
		return teardownIdentity{}, false, fmt.Errorf(
			"ServiceAccount/%s UID changed from %q to %q",
			contract.name,
			contract.expectedUID,
			identity.uid,
		)
	}
	return identity, true, err
}

func (t *PrivilegeTeardown) inspectAuthorizationClusterRole(ctx context.Context, contract privilegeAuthorizationContract) (teardownIdentity, bool, error) {
	if !contract.cluster {
		return teardownIdentity{}, false, errors.New("authorization contract is not cluster-scoped")
	}
	role, err := t.clusterRoles.Get(ctx, contract.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return teardownIdentity{}, false, nil
	}
	if err != nil {
		return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
	}
	if role == nil {
		return teardownIdentity{}, false, errors.New("API returned a nil object")
	}
	if role.Name != contract.name || role.Namespace != "" {
		return teardownIdentity{}, false, fmt.Errorf("ClusterRole/%s has an unexpected identity", contract.name)
	}
	if err := t.verifyMetadata("ClusterRole", role.ObjectMeta, contract.component); err != nil {
		return teardownIdentity{}, false, err
	}
	if !reflect.DeepEqual(role.Rules, contract.rules) &&
		(len(contract.alternateRules) == 0 || !reflect.DeepEqual(role.Rules, contract.alternateRules)) {
		return teardownIdentity{}, false, fmt.Errorf("ClusterRole/%s policy rules differ from the exact ordered privilege contract", contract.name)
	}
	identity, err := deletionIdentity("ClusterRole", contract.name, role)
	return identity, true, err
}

func (t *PrivilegeTeardown) inspectAuthorizationRole(ctx context.Context, contract privilegeAuthorizationContract) (teardownIdentity, bool, error) {
	if contract.cluster {
		return teardownIdentity{}, false, errors.New("authorization contract is not namespace-scoped")
	}
	role, err := t.roles.Get(ctx, contract.namespace, contract.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return teardownIdentity{}, false, nil
	}
	if err != nil {
		return teardownIdentity{}, false, fmt.Errorf("get object: %w", err)
	}
	if role == nil {
		return teardownIdentity{}, false, errors.New("API returned a nil object")
	}
	if role.Name != contract.name || role.Namespace != contract.namespace {
		return teardownIdentity{}, false, fmt.Errorf("Role/%s/%s has an unexpected identity", contract.namespace, contract.name)
	}
	if err := t.verifyMetadata("Role", role.ObjectMeta, contract.component); err != nil {
		return teardownIdentity{}, false, err
	}
	if !reflect.DeepEqual(role.Rules, contract.rules) &&
		(len(contract.alternateRules) == 0 || !reflect.DeepEqual(role.Rules, contract.alternateRules)) {
		return teardownIdentity{}, false, fmt.Errorf("Role/%s/%s policy rules differ from the exact ordered privilege contract", contract.namespace, contract.name)
	}
	identity, err := deletionIdentity("Role", contract.name, role)
	return identity, true, err
}

func (t *PrivilegeTeardown) verifyMetadata(kind string, metadata metav1.ObjectMeta, component string) error {
	if metadata.GenerateName != "" ||
		metadata.Annotations[helmReleaseNameAnnotation] != t.rollout.ReleaseName ||
		metadata.Annotations[helmReleaseNamespaceAnnotation] != t.rollout.ReleaseNamespace ||
		metadata.Labels[managedByLabel] != "Helm" ||
		metadata.Labels[instanceLabel] != t.rollout.ReleaseName ||
		(component != "" && metadata.Labels["app.kubernetes.io/component"] != component) {
		return fmt.Errorf("%s/%s has foreign or incomplete Helm ownership", kind, metadata.Name)
	}
	return nil
}

func (t *PrivilegeTeardown) validate() error {
	if t == nil || t.rollout == nil || t.roleBindings == nil || t.clusterBindings == nil || t.roles == nil || t.clusterRoles == nil || t.serviceAccounts == nil {
		return errors.New("privilege teardown clients and rollout identity are required")
	}
	if err := t.rollout.validateIdentity(); err != nil {
		return fmt.Errorf("validate privilege teardown identity: %w", err)
	}
	if t.rollout.PreviousControllerServiceAccountName != "" {
		if t.rollout.PreviousControllerServiceAccountName == t.rollout.ControllerServiceAccountName {
			return errors.New("previous and candidate controller ServiceAccounts must differ")
		}
		if t.rollout.PreviousControllerServiceAccountUID == "" {
			return errors.New("previous controller ServiceAccount UID is required for privilege teardown")
		}
	}
	if t.rollout.ReleaseSequence > 1 {
		return fmt.Errorf("privilege teardown for release sequence %d requires an explicit predecessor privilege inventory", t.rollout.ReleaseSequence)
	}
	cleanup, err := TeardownServiceAccountName(t.rollout.HookServiceAccountName, t.rollout.ReleaseSequence)
	if err != nil {
		return fmt.Errorf("derive cleanup ServiceAccount identity: %w", err)
	}
	if cleanup != t.cleanupAccountName {
		return fmt.Errorf("cleanup ServiceAccount name %q differs from candidate identity %q", t.cleanupAccountName, cleanup)
	}
	wantPrivilege, err := TeardownPrivilegeRoleName(t.rollout.HookServiceAccountName)
	if err != nil {
		return fmt.Errorf("derive cleanup privilege identity: %w", err)
	}
	wantResidual, err := TeardownGuardRoleName(t.rollout.HookServiceAccountName)
	if err != nil {
		return fmt.Errorf("derive residual guard identity: %w", err)
	}
	if t.cleanupPrivilege != wantPrivilege {
		return fmt.Errorf("cleanup privilege name %q differs from candidate identity %q", t.cleanupPrivilege, wantPrivilege)
	}
	if t.residualGuard != wantResidual {
		return fmt.Errorf("residual guard name %q differs from candidate identity %q", t.residualGuard, wantResidual)
	}
	if t.residualRelease != t.residualGuard {
		return fmt.Errorf("residual release Role name %q differs from residual guard identity %q", t.residualRelease, t.residualGuard)
	}
	wantDiscovery, err := TeardownDiscoveryRoleName(t.rollout.HookServiceAccountName)
	if err != nil {
		return fmt.Errorf("derive residual discovery identity: %w", err)
	}
	if t.residualDiscovery != wantDiscovery {
		return fmt.Errorf("residual discovery Role name %q differs from candidate identity %q", t.residualDiscovery, wantDiscovery)
	}
	if t.discoveryNamespace != metav1.NamespaceDefault {
		return fmt.Errorf("residual discovery namespace %q differs from required namespace %q", t.discoveryNamespace, metav1.NamespaceDefault)
	}
	configuredNames := map[string]string{
		"cleanup ServiceAccount":  t.cleanupAccountName,
		"cleanup privilege":       t.cleanupPrivilege,
		"residual guard":          t.residualGuard,
		"residual release Role":   t.residualRelease,
		"residual discovery Role": t.residualDiscovery,
	}
	for description, name := range configuredNames {
		if problems := utilvalidation.IsDNS1123Label(name); len(problems) != 0 {
			return fmt.Errorf("%s name %q is invalid: %s", description, name, strings.Join(problems, "; "))
		}
	}
	if t.cleanupAccountName == t.cleanupPrivilege || t.cleanupAccountName == t.residualGuard ||
		t.cleanupAccountName == t.residualDiscovery || t.cleanupPrivilege == t.residualGuard ||
		t.cleanupPrivilege == t.residualDiscovery || t.residualGuard == t.residualDiscovery {
		return errors.New("cleanup ServiceAccount, privilege, residual guard, and discovery names must differ")
	}
	if t.contract.Namespace != t.rollout.ReleaseNamespace {
		return fmt.Errorf("runtime admission namespace %q differs from release namespace %q", t.contract.Namespace, t.rollout.ReleaseNamespace)
	}
	if t.contract.ControllerServiceAccountName != t.rollout.ControllerServiceAccountName {
		return fmt.Errorf("runtime admission controller ServiceAccount %q differs from rollout identity %q", t.contract.ControllerServiceAccountName, t.rollout.ControllerServiceAccountName)
	}
	if t.contract.CertificateServiceAccountName != t.rollout.CertificateDeploymentName {
		return fmt.Errorf("runtime admission certificate ServiceAccount %q differs from rollout identity %q", t.contract.CertificateServiceAccountName, t.rollout.CertificateDeploymentName)
	}
	if t.contract.CertificateRuntimeEnabled {
		leaseArguments := 0
		stagingSecretArguments := 0
		recreateArguments := 0
		for _, argument := range t.rollout.CertificateArgs {
			if strings.HasPrefix(argument, "--lease-name=") {
				leaseArguments++
				value := strings.TrimPrefix(argument, "--lease-name=")
				if value == "" || value != strings.TrimSpace(value) {
					return errors.New("certificate rotation --lease-name must have a nonempty, unpadded value")
				}
			}
			if strings.HasPrefix(argument, "--staging-secret-name=") {
				stagingSecretArguments++
				value := strings.TrimPrefix(argument, "--staging-secret-name=")
				if value == "" || value != strings.TrimSpace(value) {
					return errors.New("certificate rotation --staging-secret-name must have a nonempty, unpadded value")
				}
				if value == t.rollout.WebhookSecretName {
					return errors.New("certificate rotation staging and serving Secret names must differ")
				}
			}
			if strings.HasPrefix(argument, "--recreate-missing-secret=") {
				recreateArguments++
				if argument != "--recreate-missing-secret=true" && argument != "--recreate-missing-secret=false" {
					return errors.New("certificate rotation --recreate-missing-secret must be exactly true or false")
				}
			}
		}
		if leaseArguments != 1 {
			return fmt.Errorf("certificate rotation requires exactly one --lease-name argument, found %d", leaseArguments)
		}
		if stagingSecretArguments != 1 {
			return fmt.Errorf("certificate rotation requires exactly one --staging-secret-name argument, found %d", stagingSecretArguments)
		}
		if recreateArguments > 1 {
			return fmt.Errorf("certificate rotation allows at most one --recreate-missing-secret argument, found %d", recreateArguments)
		}
	}
	names := []string{t.contract.ControllerServiceAccountName, t.contract.CertificateServiceAccountName, t.rollout.HookServiceAccountName, cleanup}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" || name != strings.TrimSpace(name) {
			return errors.New("protected ServiceAccount names must be nonempty and unpadded")
		}
		if _, found := seen[name]; found {
			return fmt.Errorf("protected ServiceAccount name %q is reused", name)
		}
		seen[name] = struct{}{}
	}
	bindingIdentities := map[string]struct{}{}
	for _, binding := range t.bindingContracts() {
		if binding.name == "" || (!binding.cluster && binding.namespace == "") || (binding.cluster && binding.namespace != "") {
			return errors.New("privilege binding contract contains an incomplete identity")
		}
		key := privilegeBindingOrderKey(binding.cluster, binding.namespace, binding.name)
		if _, found := bindingIdentities[key]; found {
			return fmt.Errorf("privilege binding contract reuses %q", binding.name)
		}
		bindingIdentities[key] = struct{}{}
	}
	authorizationIdentities := map[string]struct{}{}
	for _, authorization := range t.authorizationContracts() {
		if authorization.name == "" || (!authorization.cluster && authorization.namespace == "") || (authorization.cluster && authorization.namespace != "") || len(authorization.rules) == 0 {
			return errors.New("privilege authorization contract contains an incomplete identity or empty policy")
		}
		key := privilegeBindingOrderKey(authorization.cluster, authorization.namespace, authorization.name)
		if _, found := authorizationIdentities[key]; found {
			return fmt.Errorf("privilege authorization contract reuses %q", authorization.name)
		}
		authorizationIdentities[key] = struct{}{}
		for ruleIndex, rule := range authorization.rules {
			if len(rule.APIGroups) == 0 || len(rule.Resources) == 0 || len(rule.Verbs) == 0 || len(rule.NonResourceURLs) != 0 {
				return fmt.Errorf("privilege authorization contract %q rule %d is incomplete", authorization.name, ruleIndex)
			}
		}
	}
	return nil
}

// TeardownDiscoveryRoleName returns the candidate-specific Role and
// RoleBinding name used only to discover the default Kubernetes Service API
// endpoints for per-server authorization convergence checks.
func TeardownDiscoveryRoleName(hookServiceAccountName string) (string, error) {
	parts := teardownHookIdentityPattern.FindStringSubmatch(hookServiceAccountName)
	if len(parts) != 4 || parts[1] == "" {
		return "", fmt.Errorf("hook ServiceAccount does not encode a candidate release identity")
	}
	base := parts[1]
	if len(base) > 20 {
		base = strings.TrimSuffix(base[:20], "-")
	}
	if base == "" {
		return "", fmt.Errorf("hook ServiceAccount cannot form a teardown discovery role name")
	}
	name := base + "-cleanup-discovery-v" + parts[2] + "-" + parts[3]
	if len(name) > 63 {
		return "", fmt.Errorf("teardown discovery role name exceeds 63 characters")
	}
	return name, nil
}

func privilegeBindingTouchesProtected(subjects []rbacv1.Subject, protected privilegeProtectedSubjects) bool {
	for _, subject := range subjects {
		switch subject.Kind {
		case rbacv1.ServiceAccountKind:
			if _, found := protected.serviceAccounts[privilegeServiceAccountKey(subject.Namespace, subject.Name)]; found {
				return true
			}
		case rbacv1.UserKind:
			if _, found := protected.users[subject.Name]; found {
				return true
			}
		case rbacv1.GroupKind:
			// A namespace-specific ServiceAccount group is a release-local
			// privilege grant and must be absent. Cluster-wide ambient groups
			// such as system:authenticated and system:serviceaccounts are not
			// release-owned identities and remain outside this exact teardown.
			if subject.Name == protected.namespaceGroup {
				return true
			}
		}
	}
	return false
}

func privilegeHookBindingName(name string, limit int, suffix string) string {
	if len(name) > limit {
		name = name[:limit]
	}
	return strings.TrimSuffix(name, "-") + suffix
}

func privilegeBindingKey(namespace, name string) string {
	return namespace + "\x00" + name
}

func privilegeBindingOrderKey(cluster bool, namespace, name string) string {
	return fmt.Sprintf("%t\x00%s\x00%s", cluster, namespace, name)
}

func privilegeServiceAccountKey(namespace, name string) string {
	return namespace + "\x00" + name
}

func listPrivilegeRoleBindings(ctx context.Context, client PrivilegeRoleBindingClient) ([]rbacv1.RoleBinding, error) {
	items := []rbacv1.RoleBinding{}
	seenObjects := map[string]struct{}{}
	err := paginatePrivilegeBindings(ctx, "RoleBindings cluster-wide", func(ctx context.Context, options metav1.ListOptions) (metav1.ListMeta, int, error) {
		page, err := client.List(ctx, options)
		if err != nil {
			return metav1.ListMeta{}, 0, err
		}
		if page == nil {
			return metav1.ListMeta{}, 0, errors.New("API returned a nil page")
		}
		for index := range page.Items {
			item := page.Items[index]
			key := privilegeBindingKey(item.Namespace, item.Name)
			if _, found := seenObjects[key]; found {
				return metav1.ListMeta{}, 0, fmt.Errorf("duplicate RoleBinding/%s/%s across inventory pages", item.Namespace, item.Name)
			}
			seenObjects[key] = struct{}{}
			items = append(items, *item.DeepCopy())
		}
		return page.ListMeta, len(page.Items), nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func listPrivilegeClusterRoleBindings(ctx context.Context, client PrivilegeClusterRoleBindingClient) ([]rbacv1.ClusterRoleBinding, error) {
	items := []rbacv1.ClusterRoleBinding{}
	seenObjects := map[string]struct{}{}
	err := paginatePrivilegeBindings(ctx, "ClusterRoleBindings", func(ctx context.Context, options metav1.ListOptions) (metav1.ListMeta, int, error) {
		page, err := client.List(ctx, options)
		if err != nil {
			return metav1.ListMeta{}, 0, err
		}
		if page == nil {
			return metav1.ListMeta{}, 0, errors.New("API returned a nil page")
		}
		for index := range page.Items {
			item := page.Items[index]
			if _, found := seenObjects[item.Name]; found {
				return metav1.ListMeta{}, 0, fmt.Errorf("duplicate ClusterRoleBinding/%s across inventory pages", item.Name)
			}
			seenObjects[item.Name] = struct{}{}
			items = append(items, *item.DeepCopy())
		}
		return page.ListMeta, len(page.Items), nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func paginatePrivilegeBindings(
	ctx context.Context,
	description string,
	readPage func(context.Context, metav1.ListOptions) (metav1.ListMeta, int, error),
) error {
	continueToken := ""
	seenTokens := map[string]struct{}{}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("list %s: %w", description, err)
		}
		metadata, count, err := readPage(ctx, metav1.ListOptions{
			Limit:    privilegeTeardownBindingPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", description, err)
		}
		if count > privilegeTeardownBindingPageSize {
			return fmt.Errorf("list %s returned an oversized page with %d objects", description, count)
		}
		next := metadata.Continue
		if next == "" {
			if metadata.RemainingItemCount != nil && *metadata.RemainingItemCount > 0 {
				return fmt.Errorf("list %s ended with %d unreturned objects", description, *metadata.RemainingItemCount)
			}
			return nil
		}
		if count == 0 {
			return fmt.Errorf("list %s returned an empty page with a continuation token", description)
		}
		if next == continueToken {
			return fmt.Errorf("list %s repeated its current continuation token", description)
		}
		if _, found := seenTokens[next]; found {
			return fmt.Errorf("list %s repeated a previous continuation token", description)
		}
		seenTokens[next] = struct{}{}
		continueToken = next
	}
}
