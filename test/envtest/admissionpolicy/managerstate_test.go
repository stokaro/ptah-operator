package admissionpolicy_test

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The guards on the state the manager decides from. Status on every operator
// kind is written by the manager's ServiceAccount and nobody else -- not an
// author, not an administrator -- and the copy of a migration's unresolved-run
// record, which a restore that drops status keeps, is changed by the manager
// alone once the resource exists. A person settles an unresolved run with a
// PtahMigrationRunAcknowledgment, which records who.
//
// The manager's username is a literal each policy carries in a variable, and
// the mutations that write the ordinary user's name there show that literal is
// what admits the manager and refuses everyone else.
const (
	statusGuardMessage        = "Ptah status is written only by the operator's manager"
	unresolvedRunGuardMessage = "only the manager changes it"
)

// patchStatus patches the stored status of object as edit changes it, as a
// dry run.
func patchStatus[T client.Object](object T, edit func(T)) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		current, err := stored(ctx, object)
		if err != nil {
			return err
		}
		patched := current.DeepCopyObject().(T)
		edit(patched)
		return api.Status().Patch(ctx, patched, client.MergeFrom(current), client.DryRunAll)
	}
}

func managerStateRows(t *testing.T, c *catalog) {
	statusGuard := policy(t, "ptah-operator-status-write-guard-")
	copyGuard := policy(t, "ptah-operator-unresolved-run-guard-")
	writeGuard := policy(t, "ptah-operator-controller-write-guard-")
	user := policyenv.User()
	administrator := policyenv.Identity{Username: "envtest-cluster-administrator", Groups: []string{"system:masters"}}
	manager := env.Manager()

	const (
		userClearsRunRow          = "ordinary user clears a PtahMigration's unresolved run through status"
		administratorClearsRunRow = "cluster administrator clears a PtahMigration's unresolved run through status"
		userSchemaStatusRow       = "ordinary user rewrites a PtahSchema's status"
		userAcknowledgmentRow     = "ordinary user answers a PtahMigrationRunAcknowledgment through status"
		managerMigrationStatusRow = "manager records PtahMigration status"
		managerAnswersRow         = "manager answers a PtahMigrationRunAcknowledgment"

		userAddsCopyRow        = "ordinary user adds an unresolved-run copy to a PtahMigration"
		userRemovesCopyRow     = "ordinary user removes the unresolved-run copy from a PtahMigration"
		userRewritesCopyRow    = "ordinary user rewrites the unresolved-run copy on a PtahMigration"
		userEditsBesideCopyRow = "ordinary user edits a PtahMigration that carries an unresolved-run copy"
		userRestoresRow        = "ordinary user creates a PtahMigration that carries an unresolved-run copy"
		managerWritesCopyRow   = "manager writes the unresolved-run copy onto a PtahMigration"
		managerRemovesCopyRow  = "manager removes the unresolved-run copy from a PtahMigration"

		managerCopiesOntoSchemaRow = "manager writes the unresolved-run copy onto a PtahSchema"
		managerCopiesAndMoreRow    = "manager writes the unresolved-run copy and another annotation onto a PtahMigration"
	)
	clearRun := func(migration *operatorv1alpha1.PtahMigration) { migration.Status.UnresolvedRun = nil }
	setCopy := func(value string) func(*operatorv1alpha1.PtahMigration) {
		return func(migration *operatorv1alpha1.PtahMigration) {
			if migration.Annotations == nil {
				migration.Annotations = map[string]string{}
			}
			migration.Annotations[operatorv1alpha1.UnresolvedRunAnnotation] = value
		}
	}
	removeCopy := func(migration *operatorv1alpha1.PtahMigration) {
		delete(migration.Annotations, operatorv1alpha1.UnresolvedRunAnnotation)
	}

	// Status: the old recovery, and every other hand on it.
	c.row(policyenv.Row{
		Name: userClearsRunRow, Deny: []string{statusGuard}, Message: statusGuardMessage,
		Do: as(user, patchStatus(tenantRestoredMigration, clearRun)),
	})
	c.row(policyenv.Row{
		Name: administratorClearsRunRow, Deny: []string{statusGuard}, Message: statusGuardMessage,
		Do: as(administrator, patchStatus(tenantRestoredMigration, clearRun)),
	})
	c.row(policyenv.Row{
		Name: userSchemaStatusRow, Deny: []string{statusGuard}, Message: statusGuardMessage,
		Do: as(user, patchStatus(tenantSchema, func(schema *operatorv1alpha1.PtahSchema) {
			schema.Status.ObservedGeneration = schema.Generation
		})),
	})
	c.row(policyenv.Row{
		Name: userAcknowledgmentRow, Deny: []string{statusGuard}, Message: statusGuardMessage,
		Do: as(user, patchStatus(tenantAcknowledgment, func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
			acknowledgment.Status.ObservedGeneration = acknowledgment.Generation
		})),
	})
	c.row(policyenv.Row{Name: managerMigrationStatusRow, Do: as(manager, patchStatus(tenantRestoredMigration, clearRun))})
	c.row(policyenv.Row{Name: managerAnswersRow, Do: as(manager, patchStatus(tenantAcknowledgment,
		func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
			acknowledgment.Status.ObservedGeneration = acknowledgment.Generation
		}))})

	// The copy: nobody but the manager moves it once the resource exists.
	c.row(policyenv.Row{
		Name: userAddsCopyRow, Deny: []string{copyGuard}, Message: unresolvedRunGuardMessage,
		Do: as(user, patchStored(tenantMigration, setCopy(`{"outcome":"Unknown"}`))),
	})
	c.row(policyenv.Row{
		Name: userRemovesCopyRow, Deny: []string{copyGuard}, Message: unresolvedRunGuardMessage,
		Do: as(user, patchStored(tenantRestoredMigration, removeCopy)),
	})
	c.row(policyenv.Row{
		Name: userRewritesCopyRow, Deny: []string{copyGuard}, Message: unresolvedRunGuardMessage,
		Do: as(user, patchStored(tenantRestoredMigration, setCopy(`{"outcome":"Partial"}`))),
	})
	c.row(policyenv.Row{Name: userEditsBesideCopyRow, Do: as(user, patchStored(tenantRestoredMigration,
		func(migration *operatorv1alpha1.PtahMigration) { migration.Spec.Suspend = true }))})
	c.row(policyenv.Row{Name: userRestoresRow, Do: as(user, dryRunCreate(func() (client.Object, error) {
		value, err := unresolvedRunCopy()
		if err != nil {
			return nil, err
		}
		restored := declared("PtahMigration", "ledger-restored-again", "")
		restored.SetAnnotations(map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: value})
		return restored, nil
	}))})
	c.row(policyenv.Row{Name: managerWritesCopyRow, Do: as(manager, patchStored(tenantMigration, setCopy(`{"outcome":"Unknown"}`)))})
	c.row(policyenv.Row{Name: managerRemovesCopyRow, Do: as(manager, patchStored(tenantRestoredMigration, removeCopy))})

	// The controller write guard lets the manager move the copy on a
	// migration and nothing beside it, and nothing at all on a schema.
	c.row(policyenv.Row{
		Name: managerCopiesOntoSchemaRow, Deny: []string{writeGuard}, Message: "rejected a desired-state mutation",
		Do: as(manager, patchStored(tenantSchema, func(schema *operatorv1alpha1.PtahSchema) {
			schema.Annotations = map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: `{"outcome":"Unknown"}`}
		})),
	})
	c.row(policyenv.Row{
		Name: managerCopiesAndMoreRow, Deny: []string{writeGuard}, Message: "rejected a desired-state mutation",
		Do: as(manager, patchStored(tenantMigration, func(migration *operatorv1alpha1.PtahMigration) {
			setCopy(`{"outcome":"Unknown"}`)(migration)
			migration.Annotations["example.com/note"] = "written by the manager"
		})),
	})

	c.mutation(policyenv.Mutation{
		Name: "status guard binding dropped", Policies: []string{statusGuard},
		Apply:  policyenv.DropBinding(statusGuard),
		Breaks: []string{userClearsRunRow, administratorClearsRunRow, userSchemaStatusRow, userAcknowledgmentRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "status guard refuses what it matches", Policies: []string{statusGuard},
		Apply:  policyenv.RefuseEverything(statusGuard),
		Breaks: []string{managerMigrationStatusRow, managerAnswersRow, "manager records PtahSchema status"},
	})
	// The rendered username is what admits: with the ordinary user's written
	// there, the user's status write goes through, and the manager's does not.
	c.mutation(policyenv.Mutation{
		Name: "status guard names the ordinary user as the manager", Policies: []string{statusGuard},
		Apply:  policyenv.SetVariables(statusGuard, map[string]string{"manager": `"` + policyenv.OrdinaryUser + `"`}),
		Breaks: []string{userClearsRunRow, userSchemaStatusRow, userAcknowledgmentRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "status guard names nobody as the manager", Policies: []string{statusGuard},
		Apply:  policyenv.SetVariables(statusGuard, map[string]string{"manager": `"nobody"`}),
		Breaks: []string{managerMigrationStatusRow, managerAnswersRow},
	})

	c.mutation(policyenv.Mutation{
		Name: "unresolved-run guard binding dropped", Policies: []string{copyGuard},
		Apply:  policyenv.DropBinding(copyGuard),
		Breaks: []string{userAddsCopyRow, userRemovesCopyRow, userRewritesCopyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "unresolved-run guard refuses what it matches", Policies: []string{copyGuard},
		Apply:  policyenv.RefuseEverything(copyGuard),
		Breaks: []string{userEditsBesideCopyRow, managerWritesCopyRow, managerRemovesCopyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "unresolved-run guard names the ordinary user as the manager", Policies: []string{copyGuard},
		Apply:  policyenv.SetVariables(copyGuard, map[string]string{"manager": `"` + policyenv.OrdinaryUser + `"`}),
		Breaks: []string{userAddsCopyRow, userRemovesCopyRow, userRewritesCopyRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "unresolved-run guard names nobody as the manager", Policies: []string{copyGuard},
		Apply:  policyenv.SetVariables(copyGuard, map[string]string{"manager": `"nobody"`}),
		Breaks: []string{managerWritesCopyRow, managerRemovesCopyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "controller write guard binding dropped under the copy", Policies: []string{writeGuard},
		Apply:  policyenv.DropBinding(writeGuard),
		Breaks: []string{managerCopiesOntoSchemaRow, managerCopiesAndMoreRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "controller write guard owns no annotation", Policies: []string{writeGuard},
		Apply:  policyenv.SetVariables(writeGuard, map[string]string{"managedAnnotation": `""`}),
		Breaks: []string{managerWritesCopyRow, managerRemovesCopyRow},
	})
}
