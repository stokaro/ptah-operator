package admissionpolicy_test

import (
	"context"
	"fmt"
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
		resolveDatabaseRow    = "manager dispatches a Resolve Job mounting a database URL"
		observeDevelopmentRow = "manager dispatches an Observe Job mounting a development database URL"
		databaseAsConfigRow   = "manager dispatches an Observe Job mounting its database URL from a ConfigMap"
		releasedCleanupRow    = "manager gives a finished Job a release before file delivery created the short cleanup TTL"
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
	c.row(policyenv.Row{Name: releasedCleanupRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return as(manager, patchStored(cleanupFixtures.released, func(stored *batchv1.Job) {
			stored.Spec.TTLSecondsAfterFinished = ptr.To(int32(300))
		}))(ctx, env)
	}})

	c.mutation(policyenv.Mutation{
		Name: "Job write guard admits a credential in a container's environment", Policies: []string{jobGuard},
		Apply:  policyenv.DropValidations(jobGuard, credentialEnvironmentMessage),
		Breaks: []string{urlInEnvironmentRow, ungrantedSecretRow}, Admits: true,
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
	c.mutation(policyenv.Mutation{
		Name: "Job write guard binding dropped, for where a Job keeps its credentials", Policies: []string{jobGuard},
		Apply: policyenv.DropBinding(jobGuard),
		Breaks: []string{
			overBoundRow, urlInEnvironmentRow, ungrantedSecretRow, resolveDatabaseRow, observeDevelopmentRow, databaseAsConfigRow,
		},
	})
}
