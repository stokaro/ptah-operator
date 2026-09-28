package admissionpolicy_test

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The guard on who may turn the approval requirement off. spec.policy.apply:
// Always applies plans with no approval, and it sits on the desired-state
// resource, so the chart refuses arriving at Always -- a create that sets it,
// an update that moves to it -- from every identity outside its exempt groups.
// A write that leaves Always where it was is admitted: the manager's finalizer
// patches and an author's ordinary edits have to keep working on a resource an
// administrator set to Always, or the administrator's choice would wedge it.
//
// The chart renders its default exempt group, system:masters, so the
// administrator here is an impersonated member of it. The mutations that
// exempt another group instead, and then nobody, are what show the literal the
// chart rendered is what admits and refuses them.
const (
	applyPolicyGuardMessage = "reserves that choice"
	otherAdministrators     = "envtest-other-administrators"
)

func applyPolicyRows(t *testing.T, c *catalog) {
	guard := policy(t, "ptah-operator-apply-policy-guard-")
	author := policyenv.User()
	// The same person, carrying a group the chart did not exempt.
	authorInOtherGroup := policyenv.Identity{Username: policyenv.OrdinaryUser, Groups: []string{otherAdministrators}}
	administrator := policyenv.Identity{Username: "envtest-apply-policy-administrator", Groups: []string{"system:masters"}}
	manager := env.Manager()

	const (
		createSchemaRow         = "author creates a PtahSchema that applies Always"
		createMigrationRow      = "author creates a PtahMigration that applies Always"
		switchSchemaRow         = "author switches a PtahSchema to Always"
		switchMigrationRow      = "author switches a PtahMigration to Always"
		otherGroupRow           = "author in a group the chart did not exempt switches a PtahSchema to Always"
		createOnApprovalRow     = "author creates a PtahSchema that applies OnApproval"
		editAlwaysRow           = "author edits a PtahSchema that already applies Always"
		finalizerAlwaysRow      = "manager adds its finalizer to a PtahMigration that applies Always"
		returnRow               = "author returns a PtahSchema from Always to OnApproval"
		administratorCreateRow  = "apply-policy administrator creates a PtahSchema that applies Always"
		administratorSwitchRow  = "apply-policy administrator switches a PtahMigration to Always"
		administratorEditRow    = "apply-policy administrator edits a PtahMigration that already applies Always"
		administratorReturnsRow = "apply-policy administrator returns a PtahMigration from Always to OnApproval"
	)
	selectAlways := func(schema *operatorv1alpha1.PtahSchema) {
		schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	}
	selectAlwaysForMigration := func(migration *operatorv1alpha1.PtahMigration) {
		migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	}
	create := func(kind, name string, apply operatorv1alpha1.ApplyPolicy) func() (client.Object, error) {
		return func() (client.Object, error) { return declared(kind, name, apply), nil }
	}

	// Arriving at Always is refused, however it is arrived at.
	c.row(policyenv.Row{
		Name: createSchemaRow, Deny: []string{guard}, Message: applyPolicyGuardMessage,
		Do: as(author, dryRunCreate(create("PtahSchema", "orders-by-author", operatorv1alpha1.ApplyPolicyAlways))),
	})
	c.row(policyenv.Row{
		Name: createMigrationRow, Deny: []string{guard}, Message: applyPolicyGuardMessage,
		Do: as(author, dryRunCreate(create("PtahMigration", "ledger-by-author", operatorv1alpha1.ApplyPolicyAlways))),
	})
	c.row(policyenv.Row{
		Name: switchSchemaRow, Deny: []string{guard}, Message: applyPolicyGuardMessage,
		Do: as(author, patchStored(tenantSchema, selectAlways)),
	})
	c.row(policyenv.Row{
		Name: switchMigrationRow, Deny: []string{guard}, Message: applyPolicyGuardMessage,
		Do: as(author, patchStored(tenantMigration, selectAlwaysForMigration)),
	})
	c.row(policyenv.Row{
		Name: otherGroupRow, Deny: []string{guard}, Message: applyPolicyGuardMessage,
		Do: as(authorInOtherGroup, patchStored(tenantSchema, selectAlways)),
	})

	// Everything else an author does is outside the guard's purpose: asking
	// for approvals, editing a resource an administrator set to Always, and
	// making one safer. The manager's finalizer patch is the write that would
	// wedge an Always resource if the guard read the value rather than the
	// transition.
	c.row(policyenv.Row{Name: createOnApprovalRow, Do: as(author, dryRunCreate(
		create("PtahSchema", "orders-reviewed", operatorv1alpha1.ApplyPolicyOnApproval)))})
	c.row(policyenv.Row{Name: editAlwaysRow, Do: as(author, patchStored(tenantAlwaysSchema, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Spec.Desired.OCIRef = "oci://registry.example/acme/orders-unattended:1.5.0"
	}))})
	c.row(policyenv.Row{Name: finalizerAlwaysRow, Do: as(manager, patchStored(tenantAlwaysMigration, func(migration *operatorv1alpha1.PtahMigration) {
		controllerutil.AddFinalizer(migration, migrationFinalizer)
	}))})
	c.row(policyenv.Row{Name: returnRow, Do: as(author, patchStored(tenantAlwaysSchema, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	}))})

	// The exempt group makes the choice, and keeps every other right too.
	c.row(policyenv.Row{Name: administratorCreateRow, Do: as(administrator, dryRunCreate(
		create("PtahSchema", "orders-by-administrator", operatorv1alpha1.ApplyPolicyAlways)))})
	c.row(policyenv.Row{Name: administratorSwitchRow, Do: as(administrator, patchStored(tenantMigration, selectAlwaysForMigration))})
	c.row(policyenv.Row{Name: administratorEditRow, Do: as(administrator, patchStored(tenantAlwaysMigration, func(migration *operatorv1alpha1.PtahMigration) {
		migration.Spec.Artifact.OCIRef = "oci://registry.example/acme/ledger-unattended:1.5.0"
	}))})
	c.row(policyenv.Row{Name: administratorReturnsRow, Do: as(administrator, patchStored(tenantAlwaysMigration, func(migration *operatorv1alpha1.PtahMigration) {
		migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	}))})

	c.mutation(policyenv.Mutation{
		Name: "apply-policy guard binding dropped", Policies: []string{guard},
		Apply:  policyenv.DropBinding(guard),
		Breaks: []string{createSchemaRow, createMigrationRow, switchSchemaRow, switchMigrationRow, otherGroupRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "apply-policy guard refuses what it matches", Policies: []string{guard},
		Apply: policyenv.RefuseEverything(guard),
		Breaks: []string{
			createOnApprovalRow, editAlwaysRow, finalizerAlwaysRow, returnRow,
			administratorCreateRow, administratorSwitchRow, administratorEditRow, administratorReturnsRow,
		},
	})
	// The rendered list is what exempts: with another group in it, the author
	// who carries that group is admitted; with nobody in it, the administrator
	// is refused like anyone else.
	c.mutation(policyenv.Mutation{
		Name: "apply-policy guard exempts another group", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"exemptGroups": `["` + otherAdministrators + `"]`}),
		Breaks: []string{otherGroupRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "apply-policy guard exempts nobody", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"exemptGroups": "[]"}),
		Breaks: []string{administratorCreateRow, administratorSwitchRow},
	})
}
