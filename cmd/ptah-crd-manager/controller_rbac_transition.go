package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

type controllerRBACClient struct {
	client kubernetes.Interface
}

func newControllerRBACClient(client kubernetes.Interface) *controllerRBACClient {
	return &controllerRBACClient{client: client}
}

func (c *controllerRBACClient) ListRoleBindings(
	ctx context.Context,
	options metav1.ListOptions,
) (*rbacv1.RoleBindingList, error) {
	return c.client.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, options)
}

func (c *controllerRBACClient) ListClusterRoleBindings(
	ctx context.Context,
	options metav1.ListOptions,
) (*rbacv1.ClusterRoleBindingList, error) {
	return c.client.RbacV1().ClusterRoleBindings().List(ctx, options)
}

func (c *controllerRBACClient) GetRole(
	ctx context.Context,
	namespace, name string,
	options metav1.GetOptions,
) (*rbacv1.Role, error) {
	return c.client.RbacV1().Roles(namespace).Get(ctx, name, options)
}

func (c *controllerRBACClient) GetClusterRole(
	ctx context.Context,
	name string,
	options metav1.GetOptions,
) (*rbacv1.ClusterRole, error) {
	return c.client.RbacV1().ClusterRoles().Get(ctx, name, options)
}

func (c *controllerRBACClient) GetServiceAccount(
	ctx context.Context,
	namespace, name string,
	options metav1.GetOptions,
) (*corev1.ServiceAccount, error) {
	return c.client.CoreV1().ServiceAccounts(namespace).Get(ctx, name, options)
}

func (c *controllerRBACClient) PatchRoleBinding(
	ctx context.Context,
	namespace, name string,
	patchType types.PatchType,
	data []byte,
	options metav1.PatchOptions,
) (*rbacv1.RoleBinding, error) {
	return c.client.RbacV1().RoleBindings(namespace).Patch(ctx, name, patchType, data, options)
}

func (c *controllerRBACClient) PatchClusterRoleBinding(
	ctx context.Context,
	name string,
	patchType types.PatchType,
	data []byte,
	options metav1.PatchOptions,
) (*rbacv1.ClusterRoleBinding, error) {
	return c.client.RbacV1().ClusterRoleBindings().Patch(ctx, name, patchType, data, options)
}

type controllerRBACTransitionPhase interface {
	Preflight(context.Context) error
	Transition(context.Context) error
	VerifyComplete(context.Context) error
	HasPredecessor() bool
	RequiresCredentialGrace(crdupgrade.ReleaseActivationState, bool) (bool, error)
}

type authorizationConvergenceWaiter interface {
	Validate() error
	Wait(context.Context) error
}

func completeControllerRBACCutover(
	ctx context.Context,
	waitForNoPods func(context.Context) error,
	transition controllerRBACTransitionPhase,
	requiresCredentialGrace bool,
	waitForContinuousCredentialFence func(context.Context) error,
	newConvergenceBarrier func(context.Context) (authorizationConvergenceWaiter, error),
) error {
	if waitForNoPods == nil || transition == nil || waitForContinuousCredentialFence == nil || newConvergenceBarrier == nil {
		return fmt.Errorf("controller RBAC cutover dependencies are required")
	}
	if requiresCredentialGrace {
		if err := waitForContinuousCredentialFence(ctx); err != nil {
			return fmt.Errorf("wait for continuous controller credential fence before RBAC transition: %w", err)
		}
	} else {
		if err := waitForNoPods(ctx); err != nil {
			return fmt.Errorf("wait for namespace-wide runtime Pod quiescence: %w", err)
		}
	}
	if err := transition.Transition(ctx); err != nil {
		return fmt.Errorf("move exact controller RBAC bindings to candidate identity: %w", err)
	}
	if transition.HasPredecessor() {
		barrier, err := newConvergenceBarrier(ctx)
		if err != nil {
			return fmt.Errorf("prepare predecessor authorization convergence barrier: %w", err)
		}
		if barrier == nil {
			return fmt.Errorf("predecessor authorization convergence barrier is nil")
		}
		if err := barrier.Validate(); err != nil {
			return fmt.Errorf("validate predecessor authorization convergence barrier: %w", err)
		}
		if err := barrier.Wait(ctx); err != nil {
			return fmt.Errorf("wait for predecessor authorization revocation on every API server: %w", err)
		}
	}
	if err := transition.VerifyComplete(ctx); err != nil {
		return fmt.Errorf("reverify controller RBAC identities before activation: %w", err)
	}
	return nil
}

var _ controllerRBACTransitionPhase = (*crdupgrade.ControllerRBACTransition)(nil)
