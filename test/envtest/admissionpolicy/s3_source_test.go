package admissionpolicy_test

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// s3SourceSpec is the tenant schema's S3 source: a file stored as itself,
// behind a key pair, under a private CA.
func s3SourceSpec() *operatorv1alpha1.S3SourceSpec {
	return &operatorv1alpha1.S3SourceSpec{
		Endpoint: "https://objects.example.com:9000", Region: "us-east-1", Bucket: "app-schemas",
		Key: "app/1.4.0/schema.hcl", Digest: digest("d"), Format: operatorv1alpha1.HTTPSFormatFile, Path: "schema.hcl",
		AuthFrom: &operatorv1alpha1.S3AuthSource{Name: "schemas-keys"},
		Transport: operatorv1alpha1.S3TransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "objects-ca"}, Key: "ca.pem",
		}},
	}
}

// s3PlanSource is what a plan computed from the tenant's S3 source binds.
func s3PlanSource() *operatorv1alpha1.PlanSourceBinding {
	spec := s3SourceSpec()
	return sourcebinding.S3PlanSource(spec.Endpoint, spec.Bucket, spec.Key, spec.Path, digest("7"))
}

// s3SchemaJob is a Job the manager dispatches for the tenant schema whose
// source is S3: the Resolve Job, which holds the key pair in its operation
// container, or an Observe Job, whose fetch container holds it.
func s3SchemaJob(operationType operatorv1alpha1.OperationType, edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		spec := s3SourceSpec()
		schema := tenantSchema.DeepCopy()
		schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: spec}
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{S3: &operatorv1alpha1.S3SourceRevision{
			Endpoint: spec.Endpoint, Region: spec.Region, Bucket: spec.Bucket, Key: spec.Key, VersionID: "served-version",
			Format: spec.Format, Path: spec.Path, Digest: spec.Digest, ContentDigest: digest("7"),
		}}
		operation := operatorv1alpha1.ActiveOperationStatus{
			Type:               operationType,
			ID:                 "operation-01",
			InputFingerprint:   digest("7"),
			ExecutionBindingID: executionBindingID,
			StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
			Attempt:            1,
			AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("b"), TemplateDigest: digest("c")},
		}
		if operationType == operatorv1alpha1.OperationObserve {
			operation.CoordinationDigest = digest("6")
			operation.Target = &operatorv1alpha1.DatabaseTargetBinding{
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFrom: schema.Spec.Target.URLFrom,
			}
			operation.Source = &operatorv1alpha1.SourceAccessBinding{S3: sourcebinding.S3Access(*spec, *schema.Status.Source.S3)}
		}
		job, err := managerBuilder().Build(schema, operation, nil)
		if err != nil {
			return nil, err
		}
		if edit != nil {
			edit(job)
		}
		return job, nil
	}
}

func withS3Credential(job *batchv1.Job) {
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "s3-credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "schemas-keys"}},
	})
}

func withS3CA(job *batchv1.Job) {
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "s3-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "objects-ca"},
		}},
	})
}

// s3SourceRows hold the Job guard to where an S3 source's key pair and CA
// may sit: in the Resolve Job's operation container, or in the fetch
// container of a later operation, beside nothing of a registry, a snapshot, a
// Git or an HTTPS source, and nowhere else; and the key pair to a file, never
// the environment. The plan guards admit a plan computed from an S3 source by
// the digest of its endpoint, bucket, key and path, and nothing else as its
// revision.
func s3SourceRows(t *testing.T, c *catalog) {
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	planGuard := policy(t, "ptah-operator-plan-write-guard-")
	migrationPlanGuard := policy(t, "ptah-operator-migration-plan-write-guard-")
	manager := env.Manager()

	const (
		resolveRow               = "manager dispatches the Resolve Job for an S3 source"
		observeRow               = "manager dispatches the Observe Job for an S3 source"
		observeRegistryRow       = "manager dispatches an Observe Job that fetches an S3 source beside a registry credential"
		observeHTTPSRow          = "manager dispatches an Observe Job that fetches an S3 source beside an HTTPS credential"
		credentialOnOCIRow       = "manager dispatches an OCI Observe Job mounting an S3 credential"
		caOnSnapshotRow          = "manager dispatches a ConfigMap-source Observe Job mounting an S3 CA"
		credentialOnGitRow       = "manager dispatches a Git Observe Job mounting an S3 credential"
		credentialOnHTTPSRow     = "manager dispatches an HTTPS Observe Job mounting an S3 credential"
		credentialOnVerifyRow    = "manager dispatches a Job for an operation that reaches no object, mounting an S3 credential"
		credentialAsConfigRow    = "manager dispatches a Resolve Job mounting its S3 credential from a ConfigMap"
		keyInEnvironmentRow      = "manager dispatches a Resolve Job carrying an S3 secret key in its environment"
		planRow                  = "manager publishes a plan computed from an S3 source"
		migrationPlanRow         = "manager publishes a migration plan computed from an S3 source"
		endpointPlanRow          = "manager publishes an S3 plan whose revision is not a digest"
		endpointMigrationPlanRow = "manager publishes an S3 migration plan whose revision is not a digest"
		shape                    = "rejected an unsafe workload shape"
		manifest                 = "rejected an unsafe manifest shape"
	)
	c.row(policyenv.Row{Name: resolveRow, Do: as(manager, dryRunCreate(s3SchemaJob(operatorv1alpha1.OperationResolve, nil)))})
	c.row(policyenv.Row{Name: observeRow, Do: as(manager, dryRunCreate(s3SchemaJob(operatorv1alpha1.OperationObserve, nil)))})
	c.row(policyenv.Row{
		Name: observeRegistryRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(s3SchemaJob(operatorv1alpha1.OperationObserve, func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "registry-docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "registry"}},
			})
		}))),
	})
	c.row(policyenv.Row{
		Name: observeHTTPSRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(s3SchemaJob(operatorv1alpha1.OperationObserve, withHTTPSCredential))),
	})
	c.row(policyenv.Row{
		Name: credentialOnOCIRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(ociObserveJob(withS3Credential))),
	})
	c.row(policyenv.Row{
		Name: caOnSnapshotRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(snapshotObserveJob(withS3CA))),
	})
	c.row(policyenv.Row{
		Name: credentialOnGitRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationObserve, withS3Credential))),
	})
	c.row(policyenv.Row{
		Name: credentialOnHTTPSRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(httpsSchemaJob(operatorv1alpha1.OperationObserve, withS3Credential))),
	})
	c.row(policyenv.Row{
		Name: credentialOnVerifyRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			job, err := resolveJob()
			if err == nil {
				asVerifyJob(job)
				withS3Credential(job)
			}
			return job, err
		})),
	})
	c.row(policyenv.Row{
		Name: credentialAsConfigRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(s3SchemaJob(operatorv1alpha1.OperationResolve, func(job *batchv1.Job) {
			for index := range job.Spec.Template.Spec.Volumes {
				if volume := &job.Spec.Template.Spec.Volumes[index]; volume.Name == "s3-credentials" {
					volume.VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "schemas-keys"},
					}}
				}
			}
		}))),
	})
	// A literal value, so the names clause alone refuses it.
	c.row(policyenv.Row{
		Name: keyInEnvironmentRow, Deny: []string{jobGuard}, Message: credentialEnvironmentMessage,
		Do: as(manager, dryRunCreate(s3SchemaJob(operatorv1alpha1.OperationResolve, func(job *batchv1.Job) {
			container := operationContainer(job)
			container.Env = append(container.Env, corev1.EnvVar{Name: runner.EnvS3SecretAccessKey, Value: "secret"})
		}))),
	})
	c.row(policyenv.Row{Name: planRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceSchemaPlan(s3PlanSource())
	}))})
	c.row(policyenv.Row{Name: migrationPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceMigrationPlan(s3PlanSource())
	}))})
	endpointRevision := func() *operatorv1alpha1.PlanSourceBinding {
		source := s3PlanSource()
		source.Revision = s3SourceSpec().Endpoint
		return source
	}
	c.row(policyenv.Row{
		Name: endpointPlanRow, Deny: []string{planGuard}, Message: manifest,
		Do: as(manager, dryRunCreate(func() (client.Object, error) { return sourceSchemaPlan(endpointRevision()) })),
	})
	c.row(policyenv.Row{
		Name: endpointMigrationPlanRow, Deny: []string{migrationPlanGuard}, Message: manifest,
		Do: as(manager, dryRunCreate(func() (client.Object, error) { return sourceMigrationPlan(endpointRevision()) })),
	})

	// The fetch branch is the only place the guard admits an S3 fetch:
	// renaming its container in the rule turns the admitted Observe Job
	// away.
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the S3 fetch", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, shape, `["install-runner", "fetch-s3-source"]`, `["install-runner", "fetch-s3-source-gone"]`),
		Breaks: []string{observeRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the S3 credential volume", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"https-credentials", "s3-credentials"] && has(volume.secret)`, `"https-credentials"] && has(volume.secret)`),
		Breaks: []string{resolveRow, observeRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the S3 CA volume", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"https-ca", "s3-ca"] && has(volume.configMap)`, `"https-ca"] && has(volume.configMap)`),
		Breaks: []string{resolveRow, observeRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets an S3 fetch carry an HTTPS credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"fetch-s3-source"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["source-snapshot", "registry-docker-config", "registry-credentials", "registry-ca", "registry-ca-snapshot", "git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca"])`,
			`"fetch-s3-source"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["source-snapshot", "registry-docker-config", "registry-credentials", "registry-ca", "registry-ca-snapshot", "git-credentials", "git-ca", "git-known-hosts"])`),
		Breaks: []string{observeHTTPSRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets an OCI fetch carry an S3 credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"fetch-schema"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca", "s3-credentials", "s3-ca"])`,
			`"fetch-schema"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca"])`),
		Breaks: []string{credentialOnOCIRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets a snapshot copy carry an S3 CA", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"https-credentials", "https-ca", "s3-credentials", "s3-ca"])) || (dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "fetch-git-source"]`,
			`"https-credentials", "https-ca"])) || (dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "fetch-git-source"]`),
		Breaks: []string{caOnSnapshotRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets a Git fetch carry an S3 credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"registry-ca-snapshot", "https-credentials", "https-ca", "s3-credentials", "s3-ca"]))`,
			`"registry-ca-snapshot", "https-credentials", "https-ca"]))`),
		Breaks: []string{credentialOnGitRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets an HTTPS fetch carry an S3 credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"git-known-hosts", "s3-credentials", "s3-ca"]))`, `"git-known-hosts"]))`),
		Breaks: []string{credentialOnHTTPSRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets any operation mount an S3 credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`== "resolve" || !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca", "s3-credentials", "s3-ca"])`,
			`== "resolve" || !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca"])`),
		Breaks: []string{credentialOnVerifyRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the S3 secret key among the credentials", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, credentialEnvironmentMessage,
			`"PTAH_S3_SECRET_ACCESS_KEY"`, `"PTAH_S3_SECRET_ACCESS_KEY_FORGOTTEN"`),
		Breaks: []string{keyInEnvironmentRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard forgets the S3 kind", Policies: []string{planGuard},
		Apply:  policyenv.RewriteValidations(planGuard, manifest, `kind in ["Git", "HTTPS", "S3"]`, `kind in ["Git", "HTTPS"]`),
		Breaks: []string{planRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard forgets the S3 kind", Policies: []string{migrationPlanGuard},
		Apply:  policyenv.RewriteValidations(migrationPlanGuard, manifest, `kind in ["Git", "HTTPS", "S3"]`, `kind in ["Git", "HTTPS"]`),
		Breaks: []string{migrationPlanRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard takes any S3 revision", Policies: []string{planGuard},
		Apply: policyenv.RewriteValidations(planGuard, manifest,
			`kind in ["Git", "HTTPS", "S3"] && dyn(object).spec.source.revision.matches("^sha256:[0-9a-f]{64}$")`,
			`kind in ["Git", "HTTPS", "S3"] && dyn(object).spec.source.revision != ""`),
		Breaks: []string{endpointPlanRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard takes any S3 revision", Policies: []string{migrationPlanGuard},
		Apply: policyenv.RewriteValidations(migrationPlanGuard, manifest,
			`kind in ["Git", "HTTPS", "S3"] && dyn(object).spec.source.revision.matches("^sha256:[0-9a-f]{64}$")`,
			`kind in ["Git", "HTTPS", "S3"] && dyn(object).spec.source.revision != ""`),
		Breaks: []string{endpointMigrationPlanRow}, Admits: true,
	})
}
