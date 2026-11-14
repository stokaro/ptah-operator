package admissionpolicy_test

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// httpsSourceSpec is the tenant schema's HTTPS source: a file served as
// itself, behind a token, under a private CA.
func httpsSourceSpec() *operatorv1alpha1.HTTPSSourceSpec {
	return &operatorv1alpha1.HTTPSSourceSpec{
		URL: "https://files.example.com/app/1.4.0/schema", Digest: digest("d"), Format: operatorv1alpha1.HTTPSFormatFile,
		Path:     "schema.hcl",
		AuthFrom: &operatorv1alpha1.HTTPSAuthSource{Name: "files-token"},
		Transport: operatorv1alpha1.HTTPSTransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "files-ca"}, Key: "ca.pem",
		}},
	}
}

// httpsPlanSource is what a plan computed from the tenant's HTTPS source
// binds.
func httpsPlanSource() *operatorv1alpha1.PlanSourceBinding {
	spec := httpsSourceSpec()
	return sourcebinding.HTTPSPlanSource(spec.URL, spec.Path, digest("7"))
}

// httpsSchemaJob is a Job the manager dispatches for the tenant schema whose
// source is HTTPS: the Resolve Job, which holds the credential in its
// operation container, or an Observe Job, whose fetch container holds it.
func httpsSchemaJob(operationType operatorv1alpha1.OperationType, edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		spec := httpsSourceSpec()
		schema := tenantSchema.DeepCopy()
		schema.Spec.Desired = operatorv1alpha1.SourceSpec{HTTPS: spec}
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{HTTPS: &operatorv1alpha1.HTTPSSourceRevision{
			URL: spec.URL, Format: spec.Format, Path: spec.Path, Digest: spec.Digest, ContentDigest: digest("7"),
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
			operation.Source = &operatorv1alpha1.SourceAccessBinding{HTTPS: sourcebinding.HTTPSAccess(*spec, *schema.Status.Source.HTTPS)}
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

func withHTTPSCredential(job *batchv1.Job) {
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "https-credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "files-token"}},
	})
}

func withHTTPSCA(job *batchv1.Job) {
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "https-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "files-ca"},
		}},
	})
}

// httpsSourceRows hold the Job guard to where an HTTPS source's credential
// and CA may sit: in the Resolve Job's operation container, or in the fetch
// container of a later operation, beside nothing of a registry, a snapshot or
// a Git source, and nowhere else. The plan guards admit a plan computed from
// an HTTPS source by the digest of its URL and path, and nothing else as its
// revision.
func httpsSourceRows(t *testing.T, c *catalog) {
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	planGuard := policy(t, "ptah-operator-plan-write-guard-")
	migrationPlanGuard := policy(t, "ptah-operator-migration-plan-write-guard-")
	manager := env.Manager()

	const (
		resolveRow            = "manager dispatches the Resolve Job for an HTTPS source"
		observeRow            = "manager dispatches the Observe Job for an HTTPS source"
		observeRegistryRow    = "manager dispatches an Observe Job that fetches an HTTPS source beside a registry credential"
		observeGitRow         = "manager dispatches an Observe Job that fetches an HTTPS source beside a Git credential"
		credentialOnOCIRow    = "manager dispatches an OCI Observe Job mounting an HTTPS credential"
		caOnSnapshotRow       = "manager dispatches a ConfigMap-source Observe Job mounting an HTTPS CA"
		credentialOnGitRow    = "manager dispatches a Git Observe Job mounting an HTTPS credential"
		credentialOnVerifyRow = "manager dispatches a Job for an operation that reaches no download, mounting an HTTPS credential"
		credentialAsConfigRow = "manager dispatches a Resolve Job mounting its HTTPS credential from a ConfigMap"
		planRow               = "manager publishes a plan computed from an HTTPS source"
		migrationPlanRow      = "manager publishes a migration plan computed from an HTTPS source"
		urlPlanRow            = "manager publishes an HTTPS plan whose revision is not a digest"
		urlMigrationPlanRow   = "manager publishes an HTTPS migration plan whose revision is not a digest"
		shape                 = "rejected an unsafe workload shape"
		manifest              = "rejected an unsafe manifest shape"
	)
	c.row(policyenv.Row{Name: resolveRow, Do: as(manager, dryRunCreate(httpsSchemaJob(operatorv1alpha1.OperationResolve, nil)))})
	c.row(policyenv.Row{Name: observeRow, Do: as(manager, dryRunCreate(httpsSchemaJob(operatorv1alpha1.OperationObserve, nil)))})
	c.row(policyenv.Row{
		Name: observeRegistryRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(httpsSchemaJob(operatorv1alpha1.OperationObserve, func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "registry-docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "registry"}},
			})
		}))),
	})
	c.row(policyenv.Row{
		Name: observeGitRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(httpsSchemaJob(operatorv1alpha1.OperationObserve, withGitCredential))),
	})
	c.row(policyenv.Row{
		Name: credentialOnOCIRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(ociObserveJob(withHTTPSCredential))),
	})
	c.row(policyenv.Row{
		Name: caOnSnapshotRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(snapshotObserveJob(withHTTPSCA))),
	})
	c.row(policyenv.Row{
		Name: credentialOnGitRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(gitSchemaJob(operatorv1alpha1.OperationObserve, withHTTPSCredential))),
	})
	c.row(policyenv.Row{
		Name: credentialOnVerifyRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(func() (client.Object, error) {
			job, err := resolveJob()
			if err == nil {
				asVerifyJob(job)
				withHTTPSCredential(job)
			}
			return job, err
		})),
	})
	c.row(policyenv.Row{
		Name: credentialAsConfigRow, Deny: []string{jobGuard}, Message: shape,
		Do: as(manager, dryRunCreate(httpsSchemaJob(operatorv1alpha1.OperationResolve, func(job *batchv1.Job) {
			for index := range job.Spec.Template.Spec.Volumes {
				if volume := &job.Spec.Template.Spec.Volumes[index]; volume.Name == "https-credentials" {
					volume.VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "files-token"},
					}}
				}
			}
		}))),
	})
	c.row(policyenv.Row{Name: planRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceSchemaPlan(httpsPlanSource())
	}))})
	c.row(policyenv.Row{Name: migrationPlanRow, Do: as(manager, dryRunCreate(func() (client.Object, error) {
		return sourceMigrationPlan(httpsPlanSource())
	}))})
	urlRevision := func() *operatorv1alpha1.PlanSourceBinding {
		source := httpsPlanSource()
		source.Revision = httpsSourceSpec().URL
		return source
	}
	c.row(policyenv.Row{
		Name: urlPlanRow, Deny: []string{planGuard}, Message: manifest,
		Do: as(manager, dryRunCreate(func() (client.Object, error) { return sourceSchemaPlan(urlRevision()) })),
	})
	c.row(policyenv.Row{
		Name: urlMigrationPlanRow, Deny: []string{migrationPlanGuard}, Message: manifest,
		Do: as(manager, dryRunCreate(func() (client.Object, error) { return sourceMigrationPlan(urlRevision()) })),
	})

	// The fetch branch is the only place the guard admits an HTTPS fetch:
	// renaming its container in the rule turns the admitted Observe Job
	// away.
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the HTTPS fetch", Policies: []string{jobGuard},
		Apply:  policyenv.RewriteValidations(jobGuard, shape, `["install-runner", "fetch-https-source"]`, `["install-runner", "fetch-https-source-gone"]`),
		Breaks: []string{observeRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard forgets the HTTPS volumes", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"git-credentials", "https-credentials"] && has(volume.secret)`, `"git-credentials"] && has(volume.secret)`),
		Breaks: []string{resolveRow, observeRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets an HTTPS fetch carry a Git credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"fetch-https-source"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["source-snapshot", "registry-docker-config", "registry-credentials", "registry-ca", "registry-ca-snapshot", "git-credentials", `,
			`"fetch-https-source"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["source-snapshot", "registry-docker-config", "registry-credentials", "registry-ca", "registry-ca-snapshot", `),
		Breaks: []string{observeGitRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets an OCI fetch carry an HTTPS credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"fetch-schema"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca"])`,
			`"fetch-schema"] && !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts"])`),
		Breaks: []string{credentialOnOCIRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets a snapshot copy carry an HTTPS CA", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca"])) || (dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "fetch-git-source"]`,
			`"git-credentials", "git-ca", "git-known-hosts"])) || (dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "fetch-git-source"]`),
		Breaks: []string{caOnSnapshotRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets a Git fetch carry an HTTPS credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`"registry-ca-snapshot", "https-credentials", "https-ca"]))`, `"registry-ca-snapshot"]))`),
		Breaks: []string{credentialOnGitRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets any operation mount an HTTPS credential", Policies: []string{jobGuard},
		Apply: policyenv.RewriteValidations(jobGuard, shape,
			`== "resolve" || !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts", "https-credentials", "https-ca"])`,
			`== "resolve" || !dyn(object).spec.template.spec.volumes.exists(volume, volume.name in ["git-credentials", "git-ca", "git-known-hosts"])`),
		Breaks: []string{credentialOnVerifyRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard forgets the HTTPS kind", Policies: []string{planGuard},
		Apply:  policyenv.RewriteValidations(planGuard, manifest, `kind in ["Git", "HTTPS"]`, `kind in ["Git"]`),
		Breaks: []string{planRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "migration plan write guard forgets the HTTPS kind", Policies: []string{migrationPlanGuard},
		Apply:  policyenv.RewriteValidations(migrationPlanGuard, manifest, `kind in ["Git", "HTTPS"]`, `kind in ["Git"]`),
		Breaks: []string{migrationPlanRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "plan write guard takes any HTTPS revision", Policies: []string{planGuard},
		Apply: policyenv.RewriteValidations(planGuard, manifest,
			`kind in ["Git", "HTTPS"] && dyn(object).spec.source.revision.matches("^sha256:[0-9a-f]{64}$")`,
			`kind in ["Git", "HTTPS"] && dyn(object).spec.source.revision != ""`),
		Breaks: []string{urlPlanRow}, Admits: true,
	})
}
