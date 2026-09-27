package admissionpolicy_test

import (
	"context"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// credentialRows holds the credential boundary the ServiceAccount origin
// guard keeps: the manager, the certificate rotator and the hooks act only
// through a token the API server minted for one of their own Pods, and the
// manager only while the activation parameter names its release.
func credentialRows(t *testing.T, c *catalog) {
	origin := policy(t, "ptah-operator-service-account-origin-guard-")
	names := env.Chart.Names
	manager := env.Manager()
	unbound := policyenv.ServiceAccount(names.Namespace, names.Manager)
	elsewhere := unbound.BoundTo("orders-debug-shell", "7f6c1d0e-0000-4000-8000-00000000000f")

	const (
		unboundRow   = "manager's ServiceAccount acts through a token bound to no Pod"
		elsewhereRow = "manager's ServiceAccount acts through a token bound to a Pod it does not run in"
		leaseRow     = "manager takes a target Lease"
	)
	addFinalizer := patchStored(tenantSchema, func(schema *operatorv1alpha1.PtahSchema) {
		controllerutil.AddFinalizer(schema, schemaFinalizer)
	})
	c.row(policyenv.Row{
		Name: unboundRow, Deny: []string{origin}, Message: "without workload-bound identity",
		Do: as(unbound, addFinalizer),
	})
	c.row(policyenv.Row{
		Name: elsewhereRow, Deny: []string{origin}, Message: "without workload-bound identity",
		Do: as(elsewhere, addFinalizer),
	})
	c.row(policyenv.Row{Name: leaseRow, Do: as(manager, func(ctx context.Context, api client.Client) error {
		holder := names.ManagerDeployment + "-7d9c5b8f4-x2k9q"
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: names.Namespace, Name: "ptah-target-envtest"},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		}
		return api.Create(ctx, lease, client.DryRunAll)
	})})

	c.mutation(policyenv.Mutation{
		Name: "service account origin guard binding dropped", Policies: []string{origin},
		Apply: policyenv.DropBinding(origin), Breaks: []string{unboundRow, elsewhereRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "service account origin guard parameter reference fails open", Policies: []string{origin},
		Apply: policyenv.RedirectParameters(origin), Breaks: []string{unboundRow, elsewhereRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "service account origin guard refuses what it matches", Policies: []string{origin},
		Apply: policyenv.RefuseEverything(origin), Breaks: []string{leaseRow},
	})
}
