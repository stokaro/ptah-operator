package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func builderDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}

// sourceContractSchema is a schema that reads its artifact over plain HTTP
// with the registry credential in the mode given, as the digest-pin row's
// schema does.
func sourceContractSchema(mode ptahv1alpha1.RegistryAuthMode) *ptahv1alpha1.PtahSchema {
	artifact := builderDigest('a')
	return &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "schema-uid"},
		Spec: ptahv1alpha1.PtahSchemaSpec{
			Target: ptahv1alpha1.DatabaseTargetSpec{
				Engine: ptahv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "prod/team-a/orders-primary",
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			Desired: ptahv1alpha1.SourceSpec{
				OCIRef:           "oci://registry.example:5000/acme/schema:latest",
				RegistryAuthFrom: &ptahv1alpha1.RegistryAuthSource{Name: "registry-auth", Mode: mode},
				VerificationPolicyFrom: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
				Transport: ptahv1alpha1.OCITransportSpec{PlainHTTP: true},
			},
			Policy: ptahv1alpha1.ReconciliationPolicy{
				DriftSeverity: "destructive", LockTimeout: metav1.Duration{Duration: 12 * time.Second}, TransactionMode: "all",
			},
			Execution: ptahv1alpha1.ExecutionSpec{
				ActiveDeadlineSeconds: 600, ConnectTimeout: metav1.Duration{Duration: 7 * time.Second},
				NodeSelector: map[string]string{"workload": "database"},
			},
		},
		Status: ptahv1alpha1.PtahSchemaStatus{
			ExecutionBinding: &ptahv1alpha1.ExecutionBindingStatus{
				Epoch: "v1-33333333333333333333333333333333", ControllerStateVersion: 1, PtahVersion: "v0.3.0",
				ExecutorImage: "example.invalid/ptah@" + builderDigest('d'), RunnerProtocolVersion: int32(runner.ProtocolVersion),
			},
			Source: ptahv1alpha1.SchemaSourceStatus{
				RequestedReference: "oci://registry.example:5000/acme/schema:latest",
				ResolvedReference:  "oci://registry.example:5000/acme/schema@" + artifact,
				Digest:             artifact, ArtifactType: dataplane.SchemaArtifactType, Verified: true,
				VerificationPolicyUID: "verification-policy-uid", VerificationPolicyDigest: builderDigest('5'),
			},
		},
	}
}

// The source isolation predicate is what the digest-pin row holds the
// operator's Resolve and Verify Jobs to. The Jobs the manager's own builder
// writes, read back through JSON as the API server would return them, have to
// pass it in both registry authentication modes, or the row fails a correct
// operator on the cluster.
func TestSourceJobIsolationAcceptsTheBuildersJobs(t *testing.T) {
	t.Parallel()
	var key planseal.PublicKey
	for index := range key {
		key[index] = byte(index)
	}
	builder := workload.Builder{
		ExecutorImage: "example.invalid/ptah@" + builderDigest('d'), RunnerImage: "example.invalid/operator@" + builderDigest('e'),
		PtahVersion: "v0.3.0", ControllerImage: "example.invalid/manager@" + builderDigest('f'),
		ControllerRevision: "controller-test-revision", ControllerStateVersion: 1, PlanSealPublicKey: key,
	}
	for _, mode := range []ptahv1alpha1.RegistryAuthMode{ptahv1alpha1.RegistryAuthEnvironment, ptahv1alpha1.RegistryAuthDockerConfigJSON} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			schema := sourceContractSchema(mode)
			var jobs []batchv1.Job
			for _, operation := range []ptahv1alpha1.OperationType{ptahv1alpha1.OperationResolve, ptahv1alpha1.OperationVerify} {
				active := ptahv1alpha1.ActiveOperationStatus{
					Type: operation, ID: builderDigest('8'), InputFingerprint: builderDigest('f'),
					ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
					StartedAt:          metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)), Attempt: 1,
				}
				if operation == ptahv1alpha1.OperationVerify {
					active.VerificationPolicyUID = schema.Status.Source.VerificationPolicyUID
					active.VerificationPolicyDigest = schema.Status.Source.VerificationPolicyDigest
				}
				job, err := builder.Build(schema.DeepCopy(), active, nil)
				if err != nil {
					t.Fatalf("Build(%s) error = %v", operation, err)
				}
				encoded, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				var roundTripped batchv1.Job
				if err := json.Unmarshal(encoded, &roundTripped); err != nil {
					t.Fatal(err)
				}
				jobs = append(jobs, roundTripped)
			}
			inputs := sourceJobIsolationInputs{
				databaseSecret: "database-url", registrySecret: schema.Spec.Desired.RegistryAuthFrom.Name,
				registryAuthority: "registry.example:5000", authMode: string(mode),
				executorImage: builder.ExecutorImage, runnerImage: builder.RunnerImage,
				verificationPolicy: schema.Spec.Desired.VerificationPolicyFrom.Name,
				// The fixture names no ServiceAccount, and the builder writes the
				// namespace default in its place.
				serviceAccountName: "default", imagePullSecrets: []corev1.LocalObjectReference{},
				requestedReference: schema.Spec.Desired.OCIRef, resolvedReference: schema.Status.Source.ResolvedReference,
			}
			if !sourceJobIsolation(jobs, inputs) {
				t.Fatal("the source isolation predicate refused the builder's Resolve and Verify Jobs")
			}
			// And it still refuses: the same Jobs read for another credential.
			inputs.registrySecret = "another-registry-auth"
			if sourceJobIsolation(jobs, inputs) {
				t.Fatal("the builder's Jobs passed for a registry credential they do not name")
			}
		})
	}
}

// The custom-CA row holds the fetch step to its exact command, arguments, variables
// and mounts, and only a cluster runs that row. The step the manager's own builder
// writes has to pass it here, or the row fails a correct operator there.
func TestFetchIsolationAcceptsTheBuildersFetch(t *testing.T) {
	t.Parallel()
	var key planseal.PublicKey
	for index := range key {
		key[index] = byte(index)
	}
	builder := workload.Builder{
		ExecutorImage: "example.invalid/ptah@" + builderDigest('d'), RunnerImage: "example.invalid/operator@" + builderDigest('e'),
		PtahVersion: "v0.3.0", ControllerImage: "example.invalid/manager@" + builderDigest('f'),
		ControllerRevision: "controller-test-revision", ControllerStateVersion: 1, PlanSealPublicKey: key,
	}
	schema := sourceContractSchema(ptahv1alpha1.RegistryAuthEnvironment)
	schema.Spec.Desired.Transport = ptahv1alpha1.OCITransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "registry-ca"}, Key: "ca.pem",
	}}
	active := ptahv1alpha1.ActiveOperationStatus{
		Type: ptahv1alpha1.OperationObserve, ID: builderDigest('8'), InputFingerprint: builderDigest('f'),
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch, CoordinationDigest: builderDigest('c'),
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)), Attempt: 1,
		Target: &ptahv1alpha1.DatabaseTargetBinding{Engine: schema.Spec.Target.Engine, URLFrom: schema.Spec.Target.URLFrom},
		Source: &ptahv1alpha1.SourceAccessBinding{
			ResolvedReference: schema.Status.Source.ResolvedReference, Digest: schema.Status.Source.Digest,
			RegistryAuthFrom: schema.Spec.Desired.RegistryAuthFrom, Transport: schema.Spec.Desired.Transport,
		},
	}
	job, err := builder.Build(schema, active, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	inputs := customCAPodIsolationInputs{
		databaseSecret: "database", registrySecret: schema.Spec.Desired.RegistryAuthFrom.Name, registryAuthority: "registry.example:5000",
		caConfigMap: "registry-ca", resolvedReference: schema.Status.Source.ResolvedReference,
	}
	for _, container := range job.Spec.Template.Spec.InitContainers {
		if container.Name != "fetch-schema" {
			continue
		}
		found++
		view := viewContainers([]corev1.Container{container})[0]
		if !inputs.fetchIsolated(&container, view) {
			t.Fatalf("the fetch isolation predicate refused the builder's fetch: %+v", container)
		}
		// And it still refuses: the same step read for another credential.
		other := inputs
		other.registrySecret = "another-registry-auth"
		if other.fetchIsolated(&container, view) {
			t.Fatal("the builder's fetch passed for a registry credential it does not name")
		}
	}
	if found != 1 {
		t.Fatalf("the Job has %d fetch steps", found)
	}
}
