package admissionpolicy_test

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const gitSourceCommit = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"

// gitSourceSpec is the tenant schema's Git source: a branch over https://
// with a token and a CA.
func gitSourceSpec() *operatorv1alpha1.GitSourceSpec {
	return &operatorv1alpha1.GitSourceSpec{
		URL: "https://git.example.com/acme/app.git", Ref: operatorv1alpha1.GitRef{Branch: "main"}, Path: "db/schema.hcl",
		AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "git-token"},
		Transport: operatorv1alpha1.GitTransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "git-ca"}, Key: "ca.pem",
		}},
	}
}

// gitSchemaJob is a Job the manager dispatches for the tenant schema whose
// source is Git: the Resolve Job, which holds the credential in its operation
// container, or an Observe Job, whose fetch container holds it.
func gitSchemaJob(operationType operatorv1alpha1.OperationType, edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		spec := gitSourceSpec()
		schema := tenantSchema.DeepCopy()
		schema.Spec.Desired = operatorv1alpha1.SourceSpec{Git: spec}
		revision := &operatorv1alpha1.GitSourceRevision{
			URL: spec.URL, Ref: spec.Ref, Path: spec.Path, Commit: gitSourceCommit, ContentDigest: digest("7"),
		}
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: revision}
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
			operation.Source = &operatorv1alpha1.SourceAccessBinding{Git: &operatorv1alpha1.GitSourceBinding{
				URL: spec.URL, Ref: spec.Ref, Commit: gitSourceCommit, Path: spec.Path, ContentDigest: digest("7"),
				AuthFrom: spec.AuthFrom.DeepCopy(), Transport: *spec.Transport.DeepCopy(),
			}}
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

// ociObserveJob is the Observe Job the manager dispatches for the tenant
// schema's OCI source: a guard and a fetch that holds the registry
// credential.
func ociObserveJob(edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		schema := tenantSchema.DeepCopy()
		reference := "oci://registry.example/acme/schema@" + digest("a")
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			RequestedReference: schema.Spec.Desired.OCIRef, ResolvedReference: reference, Digest: digest("a"),
			Verified: true, VerificationPolicyUID: "policy-uid", VerificationPolicyDigest: digest("d"),
		}
		operation := operatorv1alpha1.ActiveOperationStatus{
			Type:               operatorv1alpha1.OperationObserve,
			ID:                 "operation-01",
			InputFingerprint:   digest("7"),
			ExecutionBindingID: executionBindingID,
			StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
			Attempt:            1,
			AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("b"), TemplateDigest: digest("c")},
			CoordinationDigest: digest("6"),
			Target:             &operatorv1alpha1.DatabaseTargetBinding{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFrom: schema.Spec.Target.URLFrom},
			Source:             &operatorv1alpha1.SourceAccessBinding{ResolvedReference: reference, Digest: digest("a")},
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

// asVerifyJob relabels the tenant schema's Resolve Job as a Verify Job: an
// operation that reaches no repository, which the guard admits only without a
// Git credential or trust.
func asVerifyJob(job *batchv1.Job) {
	const operationLabel = "operator.ptah.run/operation"
	job.Name = "ptah-verify-" + job.Name[len("ptah-resolve-"):]
	job.Labels[operationLabel] = "verify"
	job.Spec.Template.Labels[operationLabel] = "verify"
}

func withGitCredential(job *batchv1.Job) {
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "git-credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "git-token"}},
	})
}
