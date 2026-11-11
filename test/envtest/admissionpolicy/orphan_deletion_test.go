package admissionpolicy_test

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The guard on a deletion that would orphan a PtahSchema's or a
// PtahMigration's dependents. The garbage collector would have to take the
// resource's owner reference off the PtahResultRecords it owns, which the
// controller-write webhook refuses, so the orphan finalizer would never come
// off. The guard refuses the deletion instead, for everyone: the
// propagationPolicy that asks for it, the legacy orphanDependents field, and a
// deletion that names no propagation on a resource that already carries the
// orphan finalizer, which the API server would honor.
//
// A deletion that names Background or Foreground is admitted, and so is one
// that names nothing on a resource without the finalizer. On a resource that
// carries it, a named propagation takes the finalizer off in the same write.
// The namespace controller names Background on every collection it deletes.
//
// envtest runs no garbage collector, so these rows prove the verdict and not
// what the deletion then does.
const (
	orphanDeletionMessage  = "cannot orphan its dependents"
	orphanFinalizerMessage = "carries the orphan finalizer"
)

var (
	// The orphaning pair carries the orphan finalizer the API server puts on a
	// resource deleted with --cascade=orphan, and that a person can write on a
	// live one.
	tenantOrphaningSchema    *operatorv1alpha1.PtahSchema
	tenantOrphaningMigration *operatorv1alpha1.PtahMigration
)

func setupOrphaningTenant(ctx context.Context) error {
	schema, migration := &operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}
	for _, tenant := range []struct {
		object *unstructured.Unstructured
		typed  client.Object
	}{
		{declared("PtahSchema", "orders-orphaning", ""), schema},
		{declared("PtahMigration", "ledger-orphaning", ""), migration},
	} {
		tenant.object.SetFinalizers([]string{metav1.FinalizerOrphanDependents})
		if err := env.Admin.Create(ctx, tenant.object); err != nil {
			return fmt.Errorf("create the orphaning %s: %w", tenant.object.GetKind(), err)
		}
		if err := env.Admin.Get(ctx, client.ObjectKeyFromObject(tenant.object), tenant.typed); err != nil {
			return fmt.Errorf("read the orphaning %s back: %w", tenant.object.GetKind(), err)
		}
	}
	tenantOrphaningSchema, tenantOrphaningMigration = schema, migration
	return nil
}

// orphanDependents sets the legacy field of DeleteOptions. Each request gets
// its own options, because the client writes the rest of a delete's options
// into the ones it is handed.
type orphanDependents bool

func (o orphanDependents) ApplyToDelete(options *client.DeleteOptions) {
	orphan := bool(o)
	options.Raw = &metav1.DeleteOptions{OrphanDependents: &orphan} //nolint:staticcheck // The deprecated field is one of the requests the guard refuses.
}

// deleteStored deletes the stored object with options, as a dry run.
func deleteStored(object client.Object, options ...client.DeleteOption) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		return api.Delete(ctx, object.DeepCopyObject().(client.Object), append(options, client.DryRunAll)...)
	}
}

// deleteNamespaceCollection is what the namespace controller sends for each
// kind in a namespace it deletes: one collection deletion with background
// propagation, as a dry run here.
func deleteNamespaceCollection(kind client.Object) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		return api.DeleteAllOf(ctx, kind, client.InNamespace(tenantNamespace),
			client.PropagationPolicy(metav1.DeletePropagationBackground), client.DryRunAll)
	}
}

func orphanDeletionRows(t *testing.T, c *catalog) {
	guard := policy(t, "ptah-operator-orphan-deletion-guard-")
	user := policyenv.User()
	administrator := policyenv.Identity{Username: "envtest-cluster-administrator", Groups: []string{"system:masters"}}
	namespaceController := policyenv.ServiceAccount("kube-system", "namespace-controller")
	orphan := client.PropagationPolicy(metav1.DeletePropagationOrphan)
	background := client.PropagationPolicy(metav1.DeletePropagationBackground)
	foreground := client.PropagationPolicy(metav1.DeletePropagationForeground)

	const (
		migrationOrphanRow           = "ordinary user deletes a PtahMigration with propagationPolicy Orphan"
		schemaOrphanRow              = "ordinary user deletes a PtahSchema with propagationPolicy Orphan"
		migrationLegacyOrphanRow     = "ordinary user deletes a PtahMigration with orphanDependents"
		schemaLegacyOrphanRow        = "ordinary user deletes a PtahSchema with orphanDependents"
		administratorOrphanRow       = "cluster administrator deletes a PtahMigration with propagationPolicy Orphan"
		migrationCarriesFinalizerRow = "ordinary user deletes a PtahMigration that carries the orphan finalizer, naming no propagation"
		schemaCarriesFinalizerRow    = "ordinary user deletes a PtahSchema that carries the orphan finalizer, naming no propagation"

		migrationBackgroundRow   = "ordinary user deletes a PtahMigration with propagationPolicy Background"
		schemaBackgroundRow      = "ordinary user deletes a PtahSchema with propagationPolicy Background"
		migrationForegroundRow   = "ordinary user deletes a PtahMigration with propagationPolicy Foreground"
		schemaForegroundRow      = "ordinary user deletes a PtahSchema with propagationPolicy Foreground"
		migrationNoOptionsRow    = "ordinary user deletes a PtahMigration naming no propagation"
		schemaNoOptionsRow       = "ordinary user deletes a PtahSchema naming no propagation"
		migrationClearsOrphanRow = "ordinary user deletes a PtahMigration that carries the orphan finalizer with propagationPolicy Background"
		schemaLegacyCascadeRow   = "ordinary user deletes a PtahSchema that carries the orphan finalizer with orphanDependents false"
		namespaceMigrationsRow   = "namespace controller deletes every PtahMigration in a namespace"
		namespaceSchemasRow      = "namespace controller deletes every PtahSchema in a namespace"
		propagationPolicyAlone   = `has(request.options.propagationPolicy) && request.options.propagationPolicy == "Orphan"`
		orphanDependentsAlone    = `has(request.options.orphanDependents) && request.options.orphanDependents`
	)

	// Every way of asking for orphaned dependents is refused, the cluster
	// administrator's included.
	for _, row := range []struct {
		name    string
		as      policyenv.Identity
		object  client.Object
		option  client.DeleteOption
		message string
	}{
		{migrationOrphanRow, user, tenantMigration, orphan, orphanDeletionMessage},
		{schemaOrphanRow, user, tenantSchema, orphan, orphanDeletionMessage},
		{migrationLegacyOrphanRow, user, tenantMigration, orphanDependents(true), orphanDeletionMessage},
		{schemaLegacyOrphanRow, user, tenantSchema, orphanDependents(true), orphanDeletionMessage},
		{administratorOrphanRow, administrator, tenantMigration, orphan, orphanDeletionMessage},
	} {
		c.row(policyenv.Row{Name: row.name, Deny: []string{guard}, Message: row.message, Do: as(row.as, deleteStored(row.object, row.option))})
	}
	c.row(policyenv.Row{
		Name: migrationCarriesFinalizerRow, Deny: []string{guard}, Message: orphanFinalizerMessage,
		Do: as(user, deleteStored(tenantOrphaningMigration)),
	})
	c.row(policyenv.Row{
		Name: schemaCarriesFinalizerRow, Deny: []string{guard}, Message: orphanFinalizerMessage,
		Do: as(user, deleteStored(tenantOrphaningSchema)),
	})

	// Every other deletion goes through: the default propagation and the
	// foreground one, a deletion that names none, a named propagation on a
	// resource that carries the finalizer, and the namespace controller's,
	// which reaches the orphaning pair as well.
	c.row(policyenv.Row{Name: migrationBackgroundRow, Do: as(user, deleteStored(tenantMigration, background))})
	c.row(policyenv.Row{Name: schemaBackgroundRow, Do: as(user, deleteStored(tenantSchema, background))})
	c.row(policyenv.Row{Name: migrationForegroundRow, Do: as(user, deleteStored(tenantMigration, foreground))})
	c.row(policyenv.Row{Name: schemaForegroundRow, Do: as(user, deleteStored(tenantSchema, foreground))})
	c.row(policyenv.Row{Name: migrationNoOptionsRow, Do: as(user, deleteStored(tenantMigration))})
	c.row(policyenv.Row{Name: schemaNoOptionsRow, Do: as(user, deleteStored(tenantSchema))})
	c.row(policyenv.Row{Name: migrationClearsOrphanRow, Do: as(user, deleteStored(tenantOrphaningMigration, background))})
	c.row(policyenv.Row{Name: schemaLegacyCascadeRow, Do: as(user, deleteStored(tenantOrphaningSchema, orphanDependents(false)))})
	c.row(policyenv.Row{Name: namespaceMigrationsRow, Do: as(namespaceController, deleteNamespaceCollection(&operatorv1alpha1.PtahMigration{}))})
	c.row(policyenv.Row{Name: namespaceSchemasRow, Do: as(namespaceController, deleteNamespaceCollection(&operatorv1alpha1.PtahSchema{}))})

	refused := []string{
		migrationOrphanRow, schemaOrphanRow, migrationLegacyOrphanRow, schemaLegacyOrphanRow, administratorOrphanRow,
		migrationCarriesFinalizerRow, schemaCarriesFinalizerRow,
	}
	admitted := []string{
		migrationBackgroundRow, schemaBackgroundRow, migrationForegroundRow, schemaForegroundRow,
		migrationNoOptionsRow, schemaNoOptionsRow, migrationClearsOrphanRow, schemaLegacyCascadeRow,
		namespaceMigrationsRow, namespaceSchemasRow,
	}
	c.mutation(policyenv.Mutation{
		Name: "orphan-deletion guard binding dropped", Policies: []string{guard},
		Apply: policyenv.DropBinding(guard), Breaks: refused,
	})
	c.mutation(policyenv.Mutation{
		Name: "orphan-deletion guard refuses what it matches", Policies: []string{guard},
		Apply: policyenv.RefuseEverything(guard), Breaks: admitted,
	})
	// Each way of asking is read on its own: a guard that reads only one of the
	// two fields admits what the other one asks for.
	c.mutation(policyenv.Mutation{
		Name: "orphan-deletion guard reads no orphanDependents", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"orphanRequested": propagationPolicyAlone}),
		Breaks: []string{migrationLegacyOrphanRow, schemaLegacyOrphanRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "orphan-deletion guard reads no propagationPolicy", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"orphanRequested": orphanDependentsAlone}),
		Breaks: []string{migrationOrphanRow, schemaOrphanRow, administratorOrphanRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "orphan-deletion guard reads no finalizer", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"orphanFinalizer": "false"}),
		Breaks: []string{migrationCarriesFinalizerRow, schemaCarriesFinalizerRow}, Admits: true,
	})
	// A named propagation is what admits a deletion of a resource that carries
	// the finalizer: read as named nowhere, the guard refuses those too.
	c.mutation(policyenv.Mutation{
		Name: "orphan-deletion guard reads no named propagation", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"propagationNamed": "false"}),
		Breaks: []string{migrationClearsOrphanRow, schemaLegacyCascadeRow, namespaceMigrationsRow, namespaceSchemasRow},
	})
}
