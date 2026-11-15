package admissionpolicy_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

const (
	credentialEnvironmentMessage = "reads every credential from a file in a Secret volume"
	workloadShapeMessage         = "rejected an unsafe workload shape"
	// volumeBound is the clause of the Job guard that bounds how many volumes
	// a Job carries beside its result credentials.
	volumeBound = `volume.name != "result-credentials").size() <= `
)

// planJob is the Plan Job the manager dispatches for the tenant schema with
// everything a Plan may carry: an Environment-mode registry credential and a
// registry CA beside the database and development database URLs. Each
// credential is a Secret volume, so this is the most volumes a Job of this
// release carries.
func planJob(edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		schema := tenantSchema.DeepCopy()
		schema.Spec.Desired.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
			Name: "orders-registry", Mode: operatorv1alpha1.RegistryAuthEnvironment,
		}
		schema.Spec.Desired.Transport.CAFrom = &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "orders-registry-ca"}, Key: "ca.pem",
		}
		schema.Spec.Dev = &operatorv1alpha1.DatabaseTargetRef{URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "orders-development"}, Key: "url",
		}}
		reference := "oci://registry.example/acme/schema@" + digest("a")
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			RequestedReference: schema.Spec.Desired.OCIRef, ResolvedReference: reference, Digest: digest("a"),
			Verified: true, VerificationPolicyUID: "policy-uid", VerificationPolicyDigest: digest("d"),
		}
		operation := operatorv1alpha1.ActiveOperationStatus{
			Type:                 operatorv1alpha1.OperationPlan,
			ID:                   "operation-01",
			InputFingerprint:     digest("7"),
			ExecutionBindingID:   executionBindingID,
			StartedAt:            metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
			Attempt:              1,
			AdmissionSnapshot:    &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("b"), TemplateDigest: digest("c")},
			CoordinationDigest:   digest("6"),
			TargetIdentityDigest: digest("5"),
			Target:               &operatorv1alpha1.DatabaseTargetBinding{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFrom: schema.Spec.Target.URLFrom},
			Source: &operatorv1alpha1.SourceAccessBinding{
				ResolvedReference: reference, Digest: digest("a"),
				RegistryAuthFrom: schema.Spec.Desired.RegistryAuthFrom.DeepCopy(),
				Transport:        *schema.Spec.Desired.Transport.DeepCopy(),
			},
			ObservationDev:       schema.Spec.Dev.DeepCopy(),
			LeaseDurationSeconds: 960,
		}
		builder := managerBuilder()
		for index := range builder.PlanSealPublicKey {
			builder.PlanSealPublicKey[index] = byte(index + 1)
		}
		job, err := builder.Build(schema, operation, nil)
		if err != nil {
			return nil, err
		}
		if edit != nil {
			edit(job)
		}
		return job, nil
	}
}

// shadowVolume is the volume a drift check's shadow database URL is mounted
// from.
const shadowVolume = "shadow-database-url"

// driftCheckedJob is the migration history read the manager dispatches when
// the policy asks for a drift check, as an Apply when asApply is set: both
// mount the shadow database URL from a Secret of its own. It refuses to
// return a Job without that mount, since every row built on it measures the
// mount.
func driftCheckedJob(asApply bool, edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		migration := tenantMigration.DeepCopy()
		migration.Spec.Policy.DriftCheck = &operatorv1alpha1.MigrationDriftCheck{Shadow: operatorv1alpha1.DatabaseTargetRef{
			URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ledger-shadow"}, Key: "url"},
		}}
		job, err := migrationHistoryJobFor(migration)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(job.Spec.Template.Spec.Volumes, func(volume corev1.Volume) bool { return volume.Name == shadowVolume }) {
			return nil, fmt.Errorf("the drift-checked history Job mounts no %s volume", shadowVolume)
		}
		if asApply {
			job = asApplyJob(job, "ptah-m-", "history", "drift")
		}
		if edit != nil {
			edit(job)
		}
		return job, nil
	}
}

// operationContainer is the Job's one operation container.
func operationContainer(job *batchv1.Job) *corev1.Container {
	return &job.Spec.Template.Spec.Containers[0]
}

// credentialDeliveryRows hold the Job guard to where a Job of this release
// keeps its credentials: each in a file of a Secret volume, the database URL
// only on an operation that reads the database, and none in a container's
// environment. A Job a release before this one created still has its
// credentials in its environment, and the manager still writes its cleanup
// TTL.
func credentialDeliveryRows(t *testing.T, c *catalog) {
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	manager := env.Manager()

	const (
		fullPlanRow           = "manager dispatches a Plan Job carrying every volume a Plan may"
		overBoundRow          = "manager dispatches a Plan Job carrying one volume more than a Plan may"
		urlInEnvironmentRow   = "manager dispatches an Observe Job reading its database URL from its environment"
		ungrantedSecretRow    = "manager dispatches an Observe Job reading a Secret into a variable no grant names"
		httpsTokenRow         = "manager dispatches an Observe Job carrying an HTTPS token in its environment"
		resolveDatabaseRow    = "manager dispatches a Resolve Job mounting a database URL"
		observeDevelopmentRow = "manager dispatches an Observe Job mounting a development database URL"
		databaseAsConfigRow   = "manager dispatches an Observe Job mounting its database URL from a ConfigMap"
		releasedCleanupRow    = "manager gives a finished Job a release before file delivery created the short cleanup TTL"

		shadowHistoryRow       = "manager dispatches a migration History Job mounting its drift check's shadow database"
		shadowApplyRow         = "manager dispatches a migration Apply Job mounting its drift check's shadow database"
		shadowResolveRow       = "manager dispatches a migration Resolve Job mounting a shadow database URL"
		shadowSchemaRow        = "manager dispatches a schema Apply Job mounting a shadow database URL"
		shadowAsConfigRow      = "manager dispatches a migration History Job mounting its shadow database URL from a ConfigMap"
		shadowInEnvironmentRow = "manager dispatches a migration History Job carrying its shadow database URL in its environment"
	)
	databaseSecret := tenantSchema.Spec.Target.URLFrom
	c.row(policyenv.Row{Name: fullPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		job, err := planJob(nil)()
		// The row measures the bound only while the Job carries that many
		// volumes.
		if err == nil && len(job.(*batchv1.Job).Spec.Template.Spec.Volumes) != 9 {
			return nil, fmt.Errorf("the Plan Job carries %d volumes, not the nine the bound admits", len(job.(*batchv1.Job).Spec.Template.Spec.Volumes))
		}
		return job, err
	}))})
	c.row(policyenv.Row{
		Name: overBoundRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(planJob(func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "verification-policy", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "ptah-verification-policy"},
				}},
			})
		}))),
	})
	c.row(policyenv.Row{
		Name: urlInEnvironmentRow, Deny: []string{jobGuard}, Message: credentialEnvironmentMessage,
		Do: as(manager, dryRunCreate(ociObserveJob(func(job *batchv1.Job) {
			container := operationContainer(job)
			for index, variable := range container.Env {
				if variable.Name == runner.EnvDatabaseURLFile {
					container.Env[index] = corev1.EnvVar{Name: runner.EnvDatabaseURL, ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: databaseSecret.DeepCopy(),
					}}
				}
			}
		}))),
	})
	c.row(policyenv.Row{
		Name: ungrantedSecretRow, Deny: []string{jobGuard}, Message: credentialEnvironmentMessage,
		Do: as(manager, dryRunCreate(ociObserveJob(func(job *batchv1.Job) {
			container := operationContainer(job)
			container.Env = append(container.Env, corev1.EnvVar{Name: "DATABASE_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: databaseSecret.LocalObjectReference, Key: "password"},
			}})
		}))),
	})
	// A literal value, not a Secret: the names clause alone refuses it, so a
	// credential the runner reads that the guard does not name is admitted.
	c.row(policyenv.Row{
		Name: httpsTokenRow, Deny: []string{jobGuard}, Message: credentialEnvironmentMessage,
		Do: as(manager, dryRunCreate(ociObserveJob(func(job *batchv1.Job) {
			container := operationContainer(job)
			container.Env = append(container.Env, corev1.EnvVar{Name: runner.EnvHTTPSToken, Value: "token"})
		}))),
	})
	databaseVolume := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: databaseSecret.Name, Items: []corev1.KeyToPath{{Key: databaseSecret.Key, Path: "url"}},
		}}}
	}
	c.row(policyenv.Row{
		Name: resolveDatabaseRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			job, err := resolveJob()
			if err == nil {
				job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, databaseVolume("database-url"))
			}
			return job, err
		})),
	})
	c.row(policyenv.Row{
		Name: observeDevelopmentRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(ociObserveJob(func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, databaseVolume("development-database-url"))
		}))),
	})
	c.row(policyenv.Row{
		Name: databaseAsConfigRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(ociObserveJob(func(job *batchv1.Job) {
			for index := range job.Spec.Template.Spec.Volumes {
				if volume := &job.Spec.Template.Spec.Volumes[index]; volume.Name == "database-url" {
					volume.VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: databaseSecret.Name},
					}}
				}
			}
		}))),
	})
	shadowSecretVolume := corev1.Volume{Name: shadowVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: "ledger-shadow", Items: []corev1.KeyToPath{{Key: "url", Path: "url"}},
	}}}
	c.row(policyenv.Row{Name: shadowHistoryRow, Do: as(manager, dryRunCreate(driftCheckedJob(false, nil)))})
	c.row(policyenv.Row{Name: shadowApplyRow, Do: as(manager, dryRunCreate(driftCheckedJob(true, nil)))})
	c.row(policyenv.Row{
		Name: shadowResolveRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			job, err := migrationResolveJob()
			if err == nil {
				job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, shadowSecretVolume)
			}
			return job, err
		})),
	})
	// An Apply, so the operation alone does not refuse it: the component does.
	c.row(policyenv.Row{
		Name: shadowSchemaRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			job, err := resolveJob()
			if err != nil {
				return nil, err
			}
			job = asApplyJob(job, "ptah-", "resolve", "shadow")
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, shadowSecretVolume)
			return job, nil
		})),
	})
	c.row(policyenv.Row{
		Name: shadowAsConfigRow, Deny: []string{jobGuard}, Message: workloadShapeMessage,
		Do: as(manager, dryRunCreate(driftCheckedJob(false, func(job *batchv1.Job) {
			for index := range job.Spec.Template.Spec.Volumes {
				if volume := &job.Spec.Template.Spec.Volumes[index]; volume.Name == shadowVolume {
					volume.VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "ledger-shadow"},
					}}
				}
			}
		}))),
	})
	// A literal value, so the names clause alone refuses it.
	c.row(policyenv.Row{
		Name: shadowInEnvironmentRow, Deny: []string{jobGuard}, Message: credentialEnvironmentMessage,
		Do: as(manager, dryRunCreate(driftCheckedJob(false, func(job *batchv1.Job) {
			container := operationContainer(job)
			container.Env = append(container.Env, corev1.EnvVar{Name: runner.EnvShadowDatabaseURL, Value: "postgres://shadow.example/ledger"})
		}))),
	})
	c.row(policyenv.Row{Name: releasedCleanupRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return as(manager, patchStored(cleanupFixtures.released, func(stored *batchv1.Job) {
			stored.Spec.TTLSecondsAfterFinished = ptr.To(int32(300))
		}))(ctx, env)
	}})

	c.mutation(policyenv.Mutation{
		Name: "Job write guard admits a credential in a container's environment", Policies: []string{jobGuard},
		Apply:  policyenv.DropValidations(jobGuard, credentialEnvironmentMessage),
		Breaks: []string{urlInEnvironmentRow, ungrantedSecretRow, httpsTokenRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the HTTPS token among the credentials", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, credentialEnvironmentMessage,
			`"PTAH_HTTPS_TOKEN"`, `"PTAH_HTTPS_TOKEN_FORGOTTEN"`),
		Breaks: []string{httpsTokenRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard holds a cleanup TTL to the rule for a create", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, credentialEnvironmentMessage, `request.operation != "CREATE" || `, ""),
		Breaks: []string{releasedCleanupRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard bounds a Job at one volume fewer", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, workloadShapeMessage, volumeBound+"9", volumeBound+"8"),
		Breaks: []string{fullPlanRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard bounds a Job at one volume more", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, workloadShapeMessage, volumeBound+"9", volumeBound+"10"),
		Breaks: []string{overBoundRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a database URL on any operation", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, workloadShapeMessage,
			`volume.name == "database-url" && has(volume.secret) && (`, `volume.name == "database-url" && has(volume.secret) && (true || `),
		Breaks: []string{resolveDatabaseRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a development database URL on any operation", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, workloadShapeMessage,
			`volume.name == "development-database-url" && has(volume.secret) && object.metadata.labels["operator.ptah.run/operation"] == "plan"`,
			`volume.name == "development-database-url" && has(volume.secret)`),
		Breaks: []string{observeDevelopmentRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a database URL from any source", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, workloadShapeMessage,
			`volume.name == "database-url" && has(volume.secret)`, `volume.name == "database-url"`),
		Breaks: []string{databaseAsConfigRow}, Admits: true,
	})
	const (
		shadowClause    = `(volume.name == "shadow-database-url" && has(volume.secret) && `
		shadowComponent = `object.metadata.labels["app.kubernetes.io/component"] == "migration-operation" && `
		shadowOperation = `object.metadata.labels["operator.ptah.run/operation"] in ["history", "apply"])`
	)
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts no shadow database", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, workloadShapeMessage,
			shadowClause, `(volume.name == "shadow-database-url-retired" && has(volume.secret) && `),
		Breaks: []string{shadowHistoryRow, shadowApplyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a shadow database only on a history read", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, workloadShapeMessage,
			shadowComponent+shadowOperation, shadowComponent+`object.metadata.labels["operator.ptah.run/operation"] == "history")`),
		Breaks: []string{shadowApplyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a shadow database on any migration operation", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, workloadShapeMessage,
			shadowComponent+shadowOperation, shadowComponent+"true)"),
		Breaks: []string{shadowResolveRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a shadow database on a schema Job", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, workloadShapeMessage, shadowClause+shadowComponent, shadowClause),
		Breaks: []string{shadowSchemaRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard mounts a shadow database from any source", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, workloadShapeMessage, shadowClause, `(volume.name == "shadow-database-url" && `),
		Breaks: []string{shadowAsConfigRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the shadow database among the credentials", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, credentialEnvironmentMessage,
			`"PTAH_SHADOW_DB"`, `"PTAH_SHADOW_DB_FORGOTTEN"`),
		Breaks: []string{shadowInEnvironmentRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard binding dropped, for where a Job keeps its credentials", Policies: []string{jobGuard},
		Apply: policyenv.DropBinding(jobGuard),
		Breaks: []string{
			overBoundRow, urlInEnvironmentRow, ungrantedSecretRow, httpsTokenRow, resolveDatabaseRow, observeDevelopmentRow, databaseAsConfigRow,
			shadowResolveRow, shadowSchemaRow, shadowAsConfigRow, shadowInEnvironmentRow,
		},
	})
}
