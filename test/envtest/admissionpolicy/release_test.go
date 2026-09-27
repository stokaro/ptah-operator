package admissionpolicy_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// releaseRows holds the policies that protect the release itself: its
// namespace, and the activation parameter every other guard reads.
func releaseRows(t *testing.T, c *catalog) {
	namespaceGuard := policy(t, "ptah-operator-namespace-deletion-guard-")
	activationGuard := policy(t, "ptah-operator-release-activation-guard-")
	anchor := policy(t, "ptah-operator-parameter-informer-anchor")
	names := env.Chart.Names

	const (
		releaseNamespaceRow = "administrator deletes the release namespace"
		tenantNamespaceRow  = "ordinary user deletes a tenant namespace"
		deactivateRow       = "ordinary user rewrites the release activation"
		reapplyRow          = "release hook reapplies the activation unchanged"
	)
	deleteNamespace := func(name string) func(context.Context, client.Client) error {
		return func(ctx context.Context, api client.Client) error {
			return api.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, client.DryRunAll)
		}
	}
	c.row(policyenv.Row{
		Name: releaseNamespaceRow, Deny: []string{namespaceGuard}, Message: "rejected deletion of the release Namespace",
		Do: func(ctx context.Context, env *policyenv.Env) error {
			return deleteNamespace(names.Namespace)(ctx, env.Admin)
		},
	})
	c.row(policyenv.Row{Name: tenantNamespaceRow, Do: as(policyenv.User(), deleteNamespace(tenantNamespace))})

	activation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: names.Namespace, Name: names.Activation}}
	c.row(policyenv.Row{
		Name: deactivateRow, Deny: []string{activationGuard}, Message: "rejected an unsafe activation transition",
		Do: as(policyenv.User(), func(ctx context.Context, api client.Client) error {
			current, err := stored(ctx, activation)
			if err != nil {
				return err
			}
			current.Data["active-release-sequence"] = "0"
			return api.Update(ctx, current, client.DryRunAll)
		}),
	})
	c.row(policyenv.Row{Name: reapplyRow, Do: as(env.Hook(), func(ctx context.Context, api client.Client) error {
		current, err := stored(ctx, activation)
		if err != nil {
			return err
		}
		return api.Update(ctx, current, client.DryRunAll)
	})})

	c.mutation(policyenv.Mutation{
		Name: "namespace deletion guard binding dropped", Policies: []string{namespaceGuard},
		Apply: policyenv.DropBinding(namespaceGuard), Breaks: []string{releaseNamespaceRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "namespace deletion guard matches every namespace", Policies: []string{namespaceGuard},
		Apply: policyenv.WidenMatch(namespaceGuard), Breaks: []string{tenantNamespaceRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "release activation guard binding dropped", Policies: []string{activationGuard},
		Apply: policyenv.DropBinding(activationGuard), Breaks: []string{deactivateRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "release activation guard refuses what it matches", Policies: []string{activationGuard},
		Apply: policyenv.RefuseEverything(activationGuard), Breaks: []string{reapplyRow},
	})

	c.exempt[anchor] = "it admits every request it matches: it exists so that one policy with a ConfigMap " +
		"parameter stays bound whatever else is removed"
	c.elsewhere[anchor] = "TestParameterInformerAnchor"
}
