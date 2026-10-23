package admissionpolicy_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
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

// controllerRows holds the seven guards on the manager's own writes. Each one
// matches only the manager's ServiceAccount, lets through exactly what the
// manager's code writes, and refuses the same write with one field changed.
// An ordinary user's writes are outside them, which the widened-match
// mutations prove matters.
//
// The object guards carry this release's manager image and controller-state
// version as literals. A write stamped by another release is refused, and the
// mutations that put the other release's value into the policy show that the
// literal is what refuses it.
func controllerRows(t *testing.T, c *catalog) {
	writeGuard := policy(t, "ptah-operator-controller-write-guard-")
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	chunkGuard := policy(t, "ptah-operator-chunk-write-guard-")
	projectionGuard := policy(t, "ptah-operator-projection-write-guard-")
	snapshotDeleteGuard := policy(t, "ptah-operator-snapshot-delete-guard-")
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

	// Jobs: the builder's own Jobs, one of them made privileged, and the same
	// Job built by another manager release.
	const (
		schemaJobRow           = "manager dispatches the Job the builder builds for a PtahSchema"
		migrationJobRow        = "manager dispatches the Job the builder builds for a PtahMigration"
		declaredMetadataJobRow = "manager dispatches a builder Job carrying the Pod metadata the schema declares"
		privilegedJobRow       = "manager dispatches a builder Job made privileged"
		reservedLabelJobRow    = "manager dispatches a builder Job carrying a label under app.kubernetes.io"
		foreignImageJobRow     = "manager dispatches a Job stamped with another manager's image"
		foreignStateJobRow     = "manager dispatches a Job stamped with another controller-state version"
		userJobRow             = "ordinary user creates a privileged Job"
	)
	foreignImage := "ghcr.io/stokaro/ptah-operator@" + digest("9")
	foreignState := managerBuilder().ControllerStateVersion + 1
	builtBy := func(edit func(*workload.Builder)) func() (client.Object, error) {
		return func() (client.Object, error) {
			builder := managerBuilder()
			edit(&builder)
			return resolveJobBuiltBy(builder)
		}
	}
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
	// The metadata spec.execution.podMetadata declares, under the operator's
	// own, and a label the builder refuses to write, put on the Job the way
	// only a manager of another build could: the guard is the layer that
	// refuses it without the builder.
	declared := func() (client.Object, error) {
		return resolveJobDeclaring(&operatorv1alpha1.PodMetadataSpec{
			Labels:      map[string]operatorv1alpha1.PodLabelValue{"acme.example/team": "platform"},
			Annotations: map[string]operatorv1alpha1.PodAnnotationValue{"sidecar.istio.io/inject": "false"},
		})
	}
	reservedLabel := func() (client.Object, error) {
		job, err := resolveJob()
		if err != nil {
			return nil, err
		}
		job.Labels["app.kubernetes.io/name"] = "ptah-operator"
		job.Spec.Template.Labels["app.kubernetes.io/name"] = "ptah-operator"
		return job, nil
	}
	c.row(policyenv.Row{Name: schemaJobRow, Do: as(manager, dryRunCreate(func() (client.Object, error) { return resolveJob() }))})
	c.row(policyenv.Row{Name: migrationJobRow, Do: as(manager, dryRunCreate(func() (client.Object, error) { return migrationResolveJob() }))})
	c.row(policyenv.Row{Name: declaredMetadataJobRow, Do: as(manager, dryRunCreate(declared))})
	c.row(policyenv.Row{
		Name: privilegedJobRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(privileged)),
	})
	c.row(policyenv.Row{
		Name: reservedLabelJobRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(reservedLabel)),
	})
	c.row(policyenv.Row{
		Name: foreignImageJobRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(builtBy(func(builder *workload.Builder) { builder.ControllerImage = foreignImage }))),
	})
	c.row(policyenv.Row{
		Name: foreignStateJobRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(builtBy(func(builder *workload.Builder) { builder.ControllerStateVersion = foreignState }))),
	})
	c.row(policyenv.Row{Name: userJobRow, Do: as(policyenv.User(), dryRunCreate(privileged))})

	// Plans: the plan store's own publication and projection, a plan stamped
	// by another manager image, a chunk that carries more than the store
	// writes, and a ConfigMap that is not a plan projection.
	const (
		publishRow               = "manager publishes a two-chunk plan through the plan store"
		projectRow               = "manager projects a two-chunk plan for an Apply through the plan store"
		foreignPlanRow           = "manager creates a plan stamped with another manager's image"
		foreignStatePlanRow      = "manager creates a plan stamped with another controller-state version"
		annotatedChunkRow        = "manager creates a plan chunk that carries an annotation"
		notChunkRow              = "manager creates a ConfigMap that is not a plan projection"
		migrationPlanRow         = "manager publishes a migration plan"
		foreignMigrationRow      = "manager creates a migration plan stamped with another manager's image"
		foreignStateMigrationRow = "manager creates a migration plan stamped with another controller-state version"
	)
	releaseState := managerBuilder().ControllerStateVersion
	c.row(policyenv.Row{Name: publishRow, Do: as(manager, publishPlan)})
	c.row(policyenv.Row{Name: projectRow, Do: as(manager, projectPlan)})
	c.row(policyenv.Row{
		Name: annotatedChunkRow, Deny: []string{chunkGuard}, Message: "rejected an unsafe PtahSchemaPlanChunk shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			plan, chunks, err := schemaPlan(harness.ManagerImage, releaseState)
			if err != nil {
				return nil, err
			}
			plan.UID = "a1f5c0de-0000-4000-8000-000000000001"
			chunk := planstore.DesiredChunk(plan, plan.Spec.Chunks[0], chunks[0])
			chunk.Annotations = map[string]string{"example.com/note": "not what the store writes"}
			return chunk, nil
		})),
	})
	c.row(policyenv.Row{
		Name: foreignPlanRow, Deny: []string{planGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			plan, _, err := schemaPlan(foreignImage, releaseState)
			return plan, err
		})),
	})
	c.row(policyenv.Row{
		Name: foreignStatePlanRow, Deny: []string{planGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			plan, _, err := schemaPlan(harness.ManagerImage, foreignState)
			return plan, err
		})),
	})
	c.row(policyenv.Row{
		Name: notChunkRow, Deny: []string{projectionGuard}, Message: "rejected an unsafe ConfigMap shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			return &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "orders-settings"},
				Data:       map[string]string{"database": "orders"},
			}, nil
		})),
	})
	c.row(policyenv.Row{Name: migrationPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return migrationPlan(harness.ManagerImage, releaseState)
	}))})
	c.row(policyenv.Row{
		Name: foreignMigrationRow, Deny: []string{migrationPlanGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			return migrationPlan(foreignImage, releaseState)
		})),
	})
	c.row(policyenv.Row{
		Name: foreignStateMigrationRow, Deny: []string{migrationPlanGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			return migrationPlan(harness.ManagerImage, foreignState)
		})),
	})

	// ConfigMap sources: the snapshot the manager writes and later deletes,
	// the Job that reads it, and the plans computed from it. Each refusal is a
	// write the API server's own schema admits, so the guard is what refuses
	// it.
	const (
		snapshotRow            = "manager writes the snapshot of a ConfigMap source"
		snapshotDataRow        = "manager writes a source snapshot that also holds a data key"
		snapshotOwnerRow       = "manager writes a source snapshot owned by a Deployment"
		snapshotDeleteRow      = "manager deletes a source snapshot nothing names"
		settingsDeleteRow      = "manager deletes a ConfigMap that is not a source snapshot"
		userSettingsDeleteRow  = "ordinary user deletes a ConfigMap that is not a source snapshot"
		snapshotJobRow         = "manager dispatches the Observe Job for a ConfigMap source"
		snapshotJobVolumeRow   = "manager dispatches an Observe Job mounting a ConfigMap that is not a snapshot"
		snapshotJobFetchRow    = "manager dispatches an Observe Job that copies a snapshot beside a registry credential"
		sourcePlanRow          = "manager publishes a plan computed from a ConfigMap"
		sourceMigrationPlanRow = "manager publishes a migration plan computed from a ConfigMap"
	)
	snapshot := func(edit func(*corev1.ConfigMap)) func() (client.Object, error) {
		return func() (client.Object, error) {
			object, _, err := sourceSnapshot("CREATE TABLE customers (id int);\n")
			if err != nil {
				return nil, err
			}
			if edit != nil {
				edit(object)
			}
			return object, nil
		}
	}
	c.row(policyenv.Row{Name: snapshotRow, Do: as(manager, dryRunCreate(snapshot(nil)))})
	c.row(policyenv.Row{
		Name: snapshotDataRow, Deny: []string{projectionGuard}, Message: "rejected an unsafe ConfigMap shape",
		Do: as(manager, dryRunCreate(snapshot(func(object *corev1.ConfigMap) {
			object.Data = map[string]string{"schema.sql": "DROP TABLE orders;\n"}
		}))),
	})
	c.row(policyenv.Row{
		Name: snapshotOwnerRow, Deny: []string{projectionGuard}, Message: "rejected an unsafe ConfigMap shape",
		Do: as(manager, dryRunCreate(snapshot(func(object *corev1.ConfigMap) {
			object.OwnerReferences[0].APIVersion = "apps/v1"
			object.OwnerReferences[0].Kind = "Deployment"
		}))),
	})
	c.row(policyenv.Row{Name: snapshotDeleteRow, Do: as(manager, dryRunDelete(tenantSnapshot))})
	c.row(policyenv.Row{
		Name: settingsDeleteRow, Deny: []string{snapshotDeleteGuard}, Message: "is not a source snapshot",
		Do: as(manager, dryRunDelete(tenantSettings)),
	})
	c.row(policyenv.Row{Name: userSettingsDeleteRow, Do: as(policyenv.User(), dryRunDelete(tenantSettings))})
	c.row(policyenv.Row{Name: snapshotJobRow, Do: as(manager, dryRunCreate(snapshotObserveJob(nil)))})
	c.row(policyenv.Row{
		Name: snapshotJobVolumeRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(snapshotObserveJob(func(job *batchv1.Job) {
			for index := range job.Spec.Template.Spec.Volumes {
				if volume := &job.Spec.Template.Spec.Volumes[index]; volume.ConfigMap != nil && volume.Name == "source-snapshot" {
					volume.ConfigMap.Name = tenantSettings.Name
				}
			}
		}))),
	})
	c.row(policyenv.Row{
		Name: snapshotJobFetchRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(snapshotObserveJob(func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "registry-docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "registry"}},
			})
		}))),
	})
	c.row(policyenv.Row{Name: sourcePlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceSchemaPlan(configMapPlanSource())
	}))})

	// Git sources: the Resolve Job holds the credential in its operation
	// container; every later operation fetches the commit in an init
	// container beside nothing of a registry or a snapshot.
	const (
		gitResolveJobRow          = "manager dispatches the Resolve Job for a Git source"
		gitPlanRow                = "manager publishes a plan computed from a Git source"
		gitMigrationPlanRow       = "manager publishes a migration plan computed from a Git source"
		gitBranchPlanRow          = "manager publishes a Git plan whose revision is not a digest"
		gitBranchMigrationPlanRow = "manager publishes a Git migration plan whose revision is not a digest"
		gitObserveJobRow          = "manager dispatches the Observe Job for a Git source"
		gitObserveRegistryRow     = "manager dispatches an Observe Job that fetches a Git source beside a registry credential"
		gitObserveSnapshotRow     = "manager dispatches an Observe Job that fetches a Git source beside a snapshot"
		gitCredentialOnSnapshot   = "manager dispatches a ConfigMap-source Observe Job mounting a Git credential"
		ociObserveJobRow          = "manager dispatches the Observe Job for an OCI source"
		gitCredentialOnOCIJob     = "manager dispatches an OCI Observe Job mounting a Git credential"
		verifyJobRow              = "manager dispatches a Job for an operation that reaches no repository"
		gitCredentialOnVerify     = "manager dispatches a Job for an operation that reaches no repository, mounting a Git credential"
		gitCredentialAsConfig     = "manager dispatches a Resolve Job mounting its Git credential from a ConfigMap"
	)
	c.row(policyenv.Row{Name: gitResolveJobRow, Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationResolve, nil)))})
	c.row(policyenv.Row{Name: gitObserveJobRow, Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationObserve, nil)))})
	c.row(policyenv.Row{
		Name: gitObserveRegistryRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationObserve, func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "registry-docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "registry"}},
			})
		}))),
	})
	c.row(policyenv.Row{
		Name: gitObserveSnapshotRow, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationObserve, func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "source-snapshot", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: tenantSnapshot.Name},
				}},
			})
		}))),
	})
	c.row(policyenv.Row{
		Name: gitCredentialOnSnapshot, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(snapshotObserveJob(withGitCredential))),
	})
	c.row(policyenv.Row{Name: ociObserveJobRow, Do: as(manager, dryRunCreate(ociObserveJob(nil)))})
	c.row(policyenv.Row{
		Name: gitCredentialOnOCIJob, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(ociObserveJob(withGitCredential))),
	})
	c.row(policyenv.Row{Name: verifyJobRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		job, err := resolveJob()
		if err == nil {
			asVerifyJob(job)
		}
		return job, err
	}))})
	c.row(policyenv.Row{
		Name: gitCredentialOnVerify, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			job, err := resolveJob()
			if err == nil {
				asVerifyJob(job)
				withGitCredential(job)
			}
			return job, err
		})),
	})
	c.row(policyenv.Row{
		Name: gitCredentialAsConfig, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationResolve, func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "git-credentials", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "git-token"},
				}},
			})
		}))),
	})
	c.row(policyenv.Row{Name: sourceMigrationPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceMigrationPlan(configMapPlanSource())
	}))})
	// A plan computed from a Git source binds the digest of its repository
	// and path as its revision, and a plan that names anything else is not one
	// the manager computed.
	c.row(policyenv.Row{Name: gitPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceSchemaPlan(gitPlanSource())
	}))})
	c.row(policyenv.Row{Name: gitMigrationPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceMigrationPlan(gitPlanSource())
	}))})
	branchRevision := func() *operatorv1alpha1.PlanSourceBinding {
		source := gitPlanSource()
		source.Revision = "main"
		return source
	}
	c.row(policyenv.Row{
		Name: gitBranchPlanRow, Deny: []string{planGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) { return sourceSchemaPlan(branchRevision()) })),
	})
	c.row(policyenv.Row{
		Name: gitBranchMigrationPlanRow, Deny: []string{migrationPlanGuard}, Message: "rejected an unsafe manifest shape",
		Do: as(manager, dryRunCreate(func() (client.Object, error) { return sourceMigrationPlan(branchRevision()) })),
	})

	// A mutation that writes another release's value into a policy has to turn
	// the row that carries that value from refused to admitted. Admits holds it
	// to that: a policy the rewrite left malformed would refuse the row too,
	// and that would prove nothing about the value.
	carriesImage := func(name, policy, row string) policyenv.Mutation {
		return policyenv.Mutation{
			Name: name, Policies: []string{policy},
			Apply:  policyenv.SetVariables(policy, map[string]string{"releaseControllerImage": strconv.Quote(foreignImage)}),
			Breaks: []string{row}, Admits: true,
		}
	}
	carriesState := func(name, policy, row string) policyenv.Mutation {
		return policyenv.Mutation{
			Name: name, Policies: []string{policy},
			Apply: policyenv.SetVariables(policy, map[string]string{
				"releaseControllerStateString": strconv.Quote(strconv.Itoa(int(foreignState))),
				"releaseControllerState":       strconv.Itoa(int(foreignState)),
			}),
			Breaks: []string{row}, Admits: true,
		}
	}

	c.mutation(policyenv.Mutation{
		Name: "controller write guard binding dropped", Policies: []string{writeGuard},
		Apply:  policyenv.DropBinding(writeGuard),
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
		Apply: policyenv.DropBinding(jobGuard),
		Breaks: []string{
			privilegedJobRow, reservedLabelJobRow, foreignImageJobRow, foreignStateJobRow,
			snapshotJobVolumeRow, snapshotJobFetchRow,
		},
	})
	c.mutation(carriesImage("Job write guard carries another manager's image", jobGuard, foreignImageJobRow))
	c.mutation(carriesState("Job write guard carries another controller-state version", jobGuard, foreignStateJobRow))
	c.mutation(policyenv.Mutation{
		Name: "Job write guard matches every identity", Policies: []string{jobGuard},
		Apply: policyenv.WidenMatch(jobGuard), Breaks: []string{userJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard refuses what it matches", Policies: []string{jobGuard},
		Apply:  policyenv.RefuseEverything(jobGuard),
		Breaks: []string{schemaJobRow, migrationJobRow, declaredMetadataJobRow, snapshotJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "chunk write guard binding dropped", Policies: []string{chunkGuard},
		Apply: policyenv.DropBinding(chunkGuard), Breaks: []string{annotatedChunkRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "chunk write guard refuses what it matches", Policies: []string{chunkGuard},
		Apply: policyenv.RefuseEverything(chunkGuard), Breaks: []string{publishRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "projection write guard binding dropped", Policies: []string{projectionGuard},
		Apply:  policyenv.DropBinding(projectionGuard),
		Breaks: []string{notChunkRow, snapshotDataRow, snapshotOwnerRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "projection write guard refuses what it matches", Policies: []string{projectionGuard},
		Apply: policyenv.RefuseEverything(projectionGuard), Breaks: []string{projectRow, snapshotRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard binding dropped", Policies: []string{planGuard},
		Apply: policyenv.DropBinding(planGuard), Breaks: []string{foreignPlanRow, foreignStatePlanRow, gitBranchPlanRow},
	})
	c.mutation(carriesImage("plan write guard carries another manager's image", planGuard, foreignPlanRow))
	c.mutation(carriesState("plan write guard carries another controller-state version", planGuard, foreignStatePlanRow))
	c.mutation(policyenv.Mutation{
		Name: "plan write guard refuses what it matches", Policies: []string{planGuard},
		Apply: policyenv.RefuseEverything(planGuard), Breaks: []string{publishRow, sourcePlanRow, gitPlanRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard binding dropped", Policies: []string{migrationPlanGuard},
		Apply:  policyenv.DropBinding(migrationPlanGuard),
		Breaks: []string{foreignMigrationRow, foreignStateMigrationRow, gitBranchMigrationPlanRow},
	})
	c.mutation(carriesImage("migration plan write guard carries another manager's image", migrationPlanGuard, foreignMigrationRow))
	c.mutation(carriesState("migration plan write guard carries another controller-state version", migrationPlanGuard, foreignStateMigrationRow))
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard refuses what it matches", Policies: []string{migrationPlanGuard},
		Apply:  policyenv.RefuseEverything(migrationPlanGuard),
		Breaks: []string{migrationPlanRow, sourceMigrationPlanRow, gitMigrationPlanRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "snapshot delete guard binding dropped", Policies: []string{snapshotDeleteGuard},
		Apply: policyenv.DropBinding(snapshotDeleteGuard), Breaks: []string{settingsDeleteRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "snapshot delete guard matches every identity", Policies: []string{snapshotDeleteGuard},
		Apply: policyenv.WidenMatch(snapshotDeleteGuard), Breaks: []string{userSettingsDeleteRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "snapshot delete guard refuses what it matches", Policies: []string{snapshotDeleteGuard},
		Apply: policyenv.RefuseEverything(snapshotDeleteGuard), Breaks: []string{snapshotDeleteRow},
	})
}

// schemaPlan is the plan the controller prepares for the tenant schema, with
// a fingerprint of its own each time: a publication is not repeatable under
// one name, and a mutation sends its rows many times. controllerImage and
// controllerStateVersion stamp it as a manager release's.
func schemaPlan(controllerImage string, controllerStateVersion int32) (*operatorv1alpha1.PtahSchemaPlan, [][]byte, error) {
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
		ControllerStateVersion:   controllerStateVersion,
		PtahVersion:              builder.PtahVersion,
		ExecutorImage:            builder.ExecutorImage,
		RunnerImage:              builder.RunnerImage,
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		Dialect:                  "postgresql",
		StatementCount:           1,
	}
	// One byte past a full chunk, so the first chunk is exactly the size the
	// chunk and projection guards have to admit: each guard reads the base64
	// the API server carries, not the bytes the store wrote.
	content := bytes.Repeat([]byte("x"), planstore.ChunkBytes+1)
	return planstore.Prepare(tenantSchema, spec, content)
}

// publishPlan publishes a plan the way the schema controller does: through the
// plan store, whose writes -- the plan, each chunk, the plan's status -- are the
// manager's.
func publishPlan(ctx context.Context, api client.Client) error {
	plan, chunks, err := schemaPlan(harness.ManagerImage, managerBuilder().ControllerStateVersion)
	if err != nil {
		return err
	}
	if len(chunks) != 2 {
		return fmt.Errorf("the plan splits into %d chunks, want 2", len(chunks))
	}
	_, err = planstore.Store{Client: api, Reader: api}.Publish(ctx, plan, chunks)
	return err
}

// projectPlan writes the ConfigMaps an Apply mounts a plan through, the way
// the schema controller does before it creates the Apply Job. The writes are
// dry runs of the projection alone, against a plan the API server never
// stored: the projection guard reads nothing but the ConfigMap, so this row
// depends on that guard and on no other.
func projectPlan(ctx context.Context, api client.Client) error {
	plan, chunks, err := schemaPlan(harness.ManagerImage, managerBuilder().ControllerStateVersion)
	if err != nil {
		return err
	}
	if len(chunks) != 2 {
		return fmt.Errorf("the plan splits into %d chunks, want 2", len(chunks))
	}
	plan.UID = "a1f5c0de-0000-4000-8000-000000000002"
	store := planstore.Store{Client: client.NewDryRunClient(api), Reader: api}
	return store.Project(ctx, plan, bytes.Join(chunks, nil))
}

// migrationPlan is the plan the migration controller publishes, built by the
// same function. The manager's image, revision and runner image are recorded
// on the plan and bind nothing, so the fingerprint leaves them out.
func migrationPlan(controllerImage string, controllerStateVersion int32) (*operatorv1alpha1.PtahMigrationPlan, error) {
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
		ControllerStateVersion:   controllerStateVersion,
		PtahVersion:              builder.PtahVersion,
		ExecutorImage:            builder.ExecutorImage,
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
		ControllerImage:          controllerImage,
		ControllerRevision:       builder.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              builder.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		Migrations:               planned,
		CreatedAt:                metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
	})
}
