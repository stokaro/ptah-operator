package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

type clusterWideRoleBindingClient struct {
	clientset kubernetes.Interface
}

type namespaceExplicitRoleClient struct {
	clientset kubernetes.Interface
}

func (c clusterWideRoleBindingClient) List(ctx context.Context, options metav1.ListOptions) (*rbacv1.RoleBindingList, error) {
	return c.clientset.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, options)
}

func (c clusterWideRoleBindingClient) Delete(ctx context.Context, namespace, name string, options metav1.DeleteOptions) error {
	return c.clientset.RbacV1().RoleBindings(namespace).Delete(ctx, name, options)
}

func (c namespaceExplicitRoleClient) Get(ctx context.Context, namespace, name string, options metav1.GetOptions) (*rbacv1.Role, error) {
	return c.clientset.RbacV1().Roles(namespace).Get(ctx, name, options)
}

func newTeardownPhases(
	clientset kubernetes.Interface,
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) (*crdupgrade.ReleaseTeardown, *crdupgrade.PrivilegeTeardown, error) {
	if clientset == nil || rollout == nil {
		return nil, nil, fmt.Errorf("teardown client and rollout identity are required")
	}
	cleanupServiceAccount, err := crdupgrade.TeardownServiceAccountName(rollout.HookServiceAccountName, rollout.ReleaseSequence)
	if err != nil {
		return nil, nil, fmt.Errorf("derive cleanup ServiceAccount: %w", err)
	}
	cleanupPrivilege, err := crdupgrade.TeardownPrivilegeRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return nil, nil, fmt.Errorf("derive cleanup privilege: %w", err)
	}
	residualGuard, err := crdupgrade.TeardownGuardRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return nil, nil, fmt.Errorf("derive residual guard: %w", err)
	}
	residualDiscovery, err := crdupgrade.TeardownDiscoveryRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return nil, nil, fmt.Errorf("derive residual discovery role: %w", err)
	}

	admission := clientset.AdmissionregistrationV1()
	release := crdupgrade.NewReleaseTeardown(
		rollout,
		admission.MutatingWebhookConfigurations(),
		admission.ValidatingWebhookConfigurations(),
		admission.ValidatingAdmissionPolicies(),
		admission.ValidatingAdmissionPolicyBindings(),
		clientset.CoreV1().ConfigMaps(rollout.ReleaseNamespace),
	)
	privilege := crdupgrade.NewPrivilegeTeardown(
		rollout,
		contract,
		crdupgrade.PrivilegeTeardownConfig{
			CleanupServiceAccountName: cleanupServiceAccount,
			CleanupPrivilegeName:      cleanupPrivilege,
			ResidualGuardName:         residualGuard,
			ResidualReleaseRoleName:   residualGuard,
			ResidualDiscoveryRoleName: residualDiscovery,
			DiscoveryNamespace:        corev1.NamespaceDefault,
		},
		clusterWideRoleBindingClient{clientset: clientset},
		clientset.RbacV1().ClusterRoleBindings(),
		namespaceExplicitRoleClient{clientset: clientset},
		clientset.RbacV1().ClusterRoles(),
		clientset.CoreV1().ServiceAccounts(rollout.ReleaseNamespace),
	)
	return release, privilege, nil
}
