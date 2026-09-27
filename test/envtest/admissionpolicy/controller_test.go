package admissionpolicy_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The finalizers the two controllers own, one per family
// (internal/controller/schema_controller.go and migration_controller.go).
const (
	schemaFinalizer    = "operator.ptah.run/active-operation"
	migrationFinalizer = "operator.ptah.run/migration-operation"
)

// as sends a request as identity.
func as(identity policyenv.Identity, send func(context.Context, client.Client) error) func(context.Context, *policyenv.Env) error {
	return func(ctx context.Context, env *policyenv.Env) error {
		api, err := env.As(identity)
		if err != nil {
			return err
		}
		return send(ctx, api)
	}
}

// patchStored patches the stored form of object as edit changes it, as a dry
// run: the row can be sent any number of times and leaves nothing behind.
func patchStored[T client.Object](object T, edit func(T)) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		current, err := stored(ctx, object)
		if err != nil {
			return err
		}
		patched := current.DeepCopyObject().(T)
		edit(patched)
		return api.Patch(ctx, patched, client.MergeFrom(current), client.DryRunAll)
	}
}

func dryRunCreate(build func() (client.Object, error)) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		object, err := build()
		if err != nil {
			return err
		}
		return api.Create(ctx, object, client.DryRunAll)
	}
}

// controllerRows holds the five guards on the manager's own writes. Each one
// matches only the manager's ServiceAccount, lets through exactly what the
// manager's code writes, and refuses the same write with one field changed.
// An ordinary user's writes are outside them, which the widened-match
// mutations prove matters.
func controllerRows(t *testing.T, c *catalog) {
	writeGuard := policy(t, "ptah-operator-controller-write-guard-")
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	chunkGuard := policy(t, "ptah-operator-chunk-write-guard-")
	planGuard := policy(t, "ptah-operator-plan-write-guard-")
	migrationPlanGuard := policy(t, "ptah-operator-migration-plan-write-guard-")
	manager := env.Manager()

	// PtahSchema and PtahMigration: the manager moves its finalizer and
	// nothing else.
	const (
		schemaFinalizerRow    = "manager adds its finalizer to a PtahSchema"
		migrationFinalizerRow = "manager adds its finalizer to a PtahMigration"
		schemaSpecRow         = "manager changes a PtahSchema's desired state"
		crossFinalizerRow     = "manager adds the schema family's finalizer to a PtahMigration"
		userSpecRow           = "ordinary user changes a PtahSchema's desired state"
		statusRow             = "manager records PtahSchema status"
	)
	c.row(policyenv.Row{Name: schemaFinalizerRow, Do: as(manager, patchStored(tenantSchema, func(schema *operatorv1alpha1.PtahSchema) {
		controllerutil.AddFinalizer(schema, schemaFinalizer)
	}))})
	c.row(policyenv.Row{Name: migrationFinalizerRow, Do: as(manager, patchStored(tenantMigration, func(migration *operatorv1alpha1.PtahMigration) {
		controllerutil.AddFinalizer(migration, migrationFinalizer)
	}))})
	c.row(policyenv.Row{
		Name: schemaSpecRow, Deny: []string{writeGuard}, Message: "rejected a desired-state mutation",
		Do: as(manager, patchStored(tenantSchema, func(schema *operatorv1alpha1.PtahSchema) {
			schema.Spec.Desired.OCIRef = "oci://registry.example/acme/orders-schema:6.6.6"
		})),
	})
	c.row(policyenv.Row{
		Name: crossFinalizerRow, Deny: []string{writeGuard}, Message: "rejected a desired-state mutation",
		Do: as(manager, patchStored(tenantMigration, func(migration *operatorv1alpha1.PtahMigration) {
			controllerutil.AddFinalizer(migration, schemaFinalizer)
		})),
	})
	c.row(policyenv.Row{Name: userSpecRow, Do: as(policyenv.User(), patchStored(tenantSchema, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Spec.Desired.OCIRef = "oci://registry.example/acme/orders-schema:1.5.0"
	}))})
	c.row(policyenv.Row{Name: statusRow, Do: as(manager, func(ctx context.Context, api client.Client) error {
		current, err := stored(ctx, tenantSchema)
		if err != nil {
			return err
		}
		current.Status.ObservedGeneration = current.Generation
		return api.Status().Update(ctx, current, client.DryRunAll)
	})})

	// Jobs: the builder's own Jobs, and one of them made privileged.
	const (
		schemaJobRow     = "manager dispatches the Job the builder builds for a PtahSchema"
		migrationJobRow  = "manager dispatches the Job the builder builds for a PtahMigration"
		privilegedJobRow = "manager dispatches a builder Job made privileged"
		userJobRow       = "ordinary user creates a privileged Job"
	)
	privileged := func() (client.Object, error) {
		job, err := resolveJob()
		if err != nil {
			return nil, err
		}
		allowed := true
		job.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = &allowed
		job.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = &allowed
		return job, nil
	}
	c.row(policyenv.Row{Name: schemaJobRow, Do: as(manager, dryRunCreate(func() (client.Object, error) { return resolveJob() }))})
	c.row(policyenv.Row{Name: migrationJobRow, Do: as(manager, dryRunCreate(func() (client.Object, error) { return migrationResolveJob() }))})
	c.row(policyenv.Row{
		Name: privilegedJobRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(privileged)),
	})
	c.row(policyenv.Row{Name: userJobRow, Do: as(policyenv.User(), dryRunCreate(privileged))})

	// Plans: the plan store's own publication, a plan stamped by another
	// manager image, and a ConfigMap that is not a plan chunk.
	const (
		publishRow          = "manager publishes a two-chunk plan through the plan store"
		foreignPlanRow      = "manager creates a plan stamped with another manager's image"
		notChunkRow         = "manager creates a ConfigMap that is not a plan chunk"
		migrationPlanRow    = "manager publishes a migration plan"
		foreignMigrationRow = "manager creates a migration plan stamped with another manager's image"
	)
	c.row(policyenv.Row{Name: publishRow, Do: as(manager, publishPlan)})
	c.row(policyenv.Row{
		Name: foreignPlanRow, Deny: []string{planGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			plan, _, err := schemaPlan("ghcr.io/stokaro/ptah-operator@" + digest("9"))
			return plan, err
		})),
	})
	c.row(policyenv.Row{
		Name: notChunkRow, Deny: []string{chunkGuard}, Message: "rejected an unsafe ConfigMap shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			return &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "orders-settings"},
				Data:       map[string]string{"database": "orders"},
			}, nil
		})),
	})
	c.row(policyenv.Row{Name: migrationPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return migrationPlan(harness.ManagerImage)
	}))})
	c.row(policyenv.Row{
		Name: foreignMigrationRow, Deny: []string{migrationPlanGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			return migrationPlan("ghcr.io/stokaro/ptah-operator@" + digest("9"))
		})),
	})

	c.mutation(policyenv.Mutation{
		Name: "controller write guard binding dropped", Policies: []string{writeGuard},
		Apply:  policyenv.DropBinding(writeGuard),
		Breaks: []string{schemaSpecRow, crossFinalizerRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "controller write guard parameter reference fails open", Policies: []string{writeGuard},
		Apply:  policyenv.RedirectParameters(writeGuard),
		Breaks: []string{schemaSpecRow, crossFinalizerRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "controller write guard matches every identity", Policies: []string{writeGuard},
		Apply: policyenv.WidenMatch(writeGuard), Breaks: []string{userSpecRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "controller write guard refuses what it matches", Policies: []string{writeGuard},
		Apply: policyenv.RefuseEverything(writeGuard), Breaks: []string{schemaFinalizerRow, migrationFinalizerRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard binding dropped", Policies: []string{jobGuard},
		Apply: policyenv.DropBinding(jobGuard), Breaks: []string{privilegedJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard matches every identity", Policies: []string{jobGuard},
		Apply: policyenv.WidenMatch(jobGuard), Breaks: []string{userJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard refuses what it matches", Policies: []string{jobGuard},
		Apply: policyenv.RefuseEverything(jobGuard), Breaks: []string{schemaJobRow, migrationJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "chunk write guard binding dropped", Policies: []string{chunkGuard},
		Apply: policyenv.DropBinding(chunkGuard), Breaks: []string{notChunkRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "chunk write guard refuses what it matches", Policies: []string{chunkGuard},
		Apply: policyenv.RefuseEverything(chunkGuard), Breaks: []string{publishRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard binding dropped", Policies: []string{planGuard},
		Apply: policyenv.DropBinding(planGuard), Breaks: []string{foreignPlanRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard refuses what it matches", Policies: []string{planGuard},
		Apply: policyenv.RefuseEverything(planGuard), Breaks: []string{publishRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard binding dropped", Policies: []string{migrationPlanGuard},
		Apply: policyenv.DropBinding(migrationPlanGuard), Breaks: []string{foreignMigrationRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard refuses what it matches", Policies: []string{migrationPlanGuard},
		Apply: policyenv.RefuseEverything(migrationPlanGuard), Breaks: []string{migrationPlanRow},
	})
}

// schemaPlan is the plan the controller prepares for the tenant schema, with
// a fingerprint of its own each time: a publication is not repeatable under
// one name, and a mutation sends its rows many times.
func schemaPlan(controllerImage string) (*operatorv1alpha1.PtahSchemaPlan, [][]byte, error) {
	identity := make([]byte, 32)
	if _, err := rand.Read(identity); err != nil {
		return nil, nil, err
	}
	builder := managerBuilder()
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: tenantSchema.Name, UID: tenantSchema.UID},
		Fingerprint:              "sha256:" + hex.EncodeToString(identity),
		ArtifactDigest:           digest("4"),
		CoordinationDigest:       digest("6"),
		TargetIdentityDigest:     digest("7"),
		ActualStateFingerprint:   digest("8"),
		DesiredStateFingerprint:  digest("9"),
		PolicyFingerprint:        digest("b"),
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: digest("c"),
		ExecutionBindingID:       executionBindingID,
		ControllerImage:          controllerImage,
		ControllerRevision:       builder.ControllerRevision,
		ControllerStateVersion:   builder.ControllerStateVersion,
		PtahVersion:              builder.PtahVersion,
		ExecutorImage:            builder.ExecutorImage,
		RunnerImage:              builder.RunnerImage,
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		Dialect:                  "postgresql",
		StatementCount:           1,
	}
	// One byte past a full chunk, so the first chunk is exactly the size the
	// chunk guard has to admit: the guard reads the base64 the API server
	// carries, not the bytes the store wrote.
	content := bytes.Repeat([]byte("x"), planstore.ChunkBytes+1)
	return planstore.Prepare(tenantSchema, spec, content)
}

// publishPlan publishes a plan the way the schema controller does: through the
// plan store, whose writes -- the plan, each chunk, the plan's status -- are the
// manager's.
func publishPlan(ctx context.Context, api client.Client) error {
	plan, chunks, err := schemaPlan(harness.ManagerImage)
	if err != nil {
		return err
	}
	if len(chunks) != 2 {
		return fmt.Errorf("the plan splits into %d chunks, want 2", len(chunks))
	}
	_, err = planstore.Store{Client: api, Reader: api}.Publish(ctx, plan, chunks)
	return err
}

// migrationPlan is the plan the migration controller publishes, built by the
// same function.
func migrationPlan(controllerImage string) (*operatorv1alpha1.PtahMigrationPlan, error) {
	builder := managerBuilder()
	planned := []operatorv1alpha1.PlannedMigration{{Version: 1, Checksum: digest("1")}}
	sequence, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		return nil, err
	}
	binding := migrationplan.Binding{
		MigrationUID:             tenantMigration.UID,
		HistoryFingerprint:       digest("5"),
		SequenceDigest:           sequence,
		ArtifactDigest:           digest("4"),
		CoordinationDigest:       digest("6"),
		TargetIdentityDigest:     digest("7"),
		PolicyFingerprint:        digest("b"),
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: digest("c"),
		ExecutionBindingID:       executionBindingID,
		ControllerImage:          controllerImage,
		ControllerRevision:       builder.ControllerRevision,
		ControllerStateVersion:   builder.ControllerStateVersion,
		PtahVersion:              builder.PtahVersion,
		ExecutorImage:            builder.ExecutorImage,
		RunnerImage:              builder.RunnerImage,
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
	}
	planFingerprint, err := binding.Fingerprint()
	if err != nil {
		return nil, err
	}
	return migrationplan.Desired(tenantMigration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion:          migrationplan.ContractVersion,
		MigrationRef:             operatorv1alpha1.ImmutableObjectReference{Name: tenantMigration.Name, UID: tenantMigration.UID},
		Fingerprint:              planFingerprint,
		HistoryFingerprint:       binding.HistoryFingerprint,
		ArtifactDigest:           binding.ArtifactDigest,
		CoordinationDigest:       binding.CoordinationDigest,
		TargetIdentityDigest:     binding.TargetIdentityDigest,
		PolicyFingerprint:        binding.PolicyFingerprint,
		VerificationPolicyUID:    binding.VerificationPolicyUID,
		VerificationPolicyDigest: binding.VerificationPolicyDigest,
		ExecutionBindingID:       binding.ExecutionBindingID,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		Migrations:               planned,
		CreatedAt:                metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
	})
}
