package workload

import (
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestBuildCredentialAndInputIsolationByOperation(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	builder := builderFixture()
	plan := planFixture(schema, builder)

	tests := []struct {
		operation operatorv1alpha1.OperationType
		plan      *operatorv1alpha1.PtahSchemaPlan
		want      []string
		absent    []string
	}{
		{
			operation: operatorv1alpha1.OperationResolve,
			want:      []string{"PTAH_REQUESTED_REFERENCE", "PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN"},
			absent:    []string{"PTAH_DB_URL", "PTAH_DEV_URL", "PTAH_SCHEMA_FILE", "PTAH_VERIFICATION_POLICY", "PTAH_PLAN_DIR"},
		},
		{
			operation: operatorv1alpha1.OperationVerify,
			want:      []string{"PTAH_REQUESTED_REFERENCE", "PTAH_RESOLVED_REFERENCE", "PTAH_VERIFICATION_POLICY", "PTAH_EXPECTED_ARTIFACT_TYPE", "PTAH_OCI_TOKEN"},
			absent:    []string{"PTAH_DB_URL", "PTAH_DEV_URL", "PTAH_SCHEMA_FILE", "PTAH_PLAN_DIR"},
		},
		{
			operation: operatorv1alpha1.OperationObserve,
			want:      []string{"PTAH_DB_URL", "PTAH_EXPECTED_DATABASE_ENGINE", "PTAH_COORDINATION_DIGEST", "PTAH_SCHEMA_FILE", "PTAH_LOCK_TIMEOUT", "PTAH_SEVERITY"},
			absent: []string{
				"PTAH_DEV_URL", "PTAH_REQUESTED_REFERENCE", "PTAH_VERIFICATION_POLICY", "PTAH_PLAN_DIR",
				"PTAH_IGNORE", "PTAH_EXCLUDE", "PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_REGISTRY", "PTAH_PLAIN_HTTP",
			},
		},
		{
			operation: operatorv1alpha1.OperationPlan,
			want:      []string{"PTAH_DB_URL", "PTAH_COORDINATION_DIGEST", "PTAH_DEV_URL", "PTAH_SCHEMA_FILE", "PTAH_EXCLUDE"},
			absent: []string{
				"PTAH_REQUESTED_REFERENCE", "PTAH_VERIFICATION_POLICY", "PTAH_PLAN_DIR",
				"PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_REGISTRY", "PTAH_PLAIN_HTTP",
			},
		},
		{
			operation: operatorv1alpha1.OperationApply,
			plan:      plan,
			want:      []string{"PTAH_DB_URL", "PTAH_EXPECTED_DATABASE_ENGINE", "PTAH_COORDINATION_DIGEST", "PTAH_PLAN_DIR", "PTAH_EXPECTED_PLAN_CONTENT_DIGEST", "PTAH_EXPECTED_COORDINATION_DIGEST", "PTAH_EXPECTED_TARGET_IDENTITY_DIGEST", "PTAH_DISPATCH_NOT_AFTER", "PTAH_LOCK_TIMEOUT", "PTAH_TX_MODE"},
			absent: []string{
				"PTAH_DEV_URL", "PTAH_SCHEMA_FILE", "PTAH_REQUESTED_REFERENCE", "PTAH_RESOLVED_REFERENCE",
				"PTAH_VERIFICATION_POLICY", "PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN",
				"PTAH_OCI_REGISTRY", "DOCKER_CONFIG", "PTAH_PLAIN_HTTP",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(string(test.operation), func(t *testing.T) {
			t.Parallel()
			job, err := builder.Build(schema.DeepCopy(), operationFixture(test.operation), test.plan)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			environment := envMap(job)
			for _, name := range test.want {
				if _, ok := environment[name]; !ok {
					t.Errorf("environment is missing %s", name)
				}
			}
			for _, name := range test.absent {
				if _, ok := environment[name]; ok {
					t.Errorf("environment unexpectedly contains %s", name)
				}
			}
			assertSecretReferencesOnly(t, job)
		})
	}
}

func TestBuildPinsRunnerSizeContractForEveryOperation(t *testing.T) {
	t.Parallel()

	builder := builderFixture()
	schema := schemaFixture()
	plan := planFixture(schema, builder)
	wantPrefix := []string{
		"--ptah-binary", ptahBinaryPath,
		"--max-result-bytes", "8388608",
		"--max-plan-bytes", "8388608",
		"--operation",
	}
	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
		operatorv1alpha1.OperationApply,
	} {
		operation := operation
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			var operationPlan *operatorv1alpha1.PtahSchemaPlan
			if operation == operatorv1alpha1.OperationApply {
				operationPlan = plan.DeepCopy()
			}
			job, err := builder.Build(schema.DeepCopy(), operationFixture(operation), operationPlan)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			args := job.Spec.Template.Spec.Containers[0].Args
			want := append(append([]string(nil), wantPrefix...), strings.ToLower(string(operation)))
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("runner args = %q, want %q", args, want)
			}
		})
	}
}

func TestBuildBindsPersistedAdmissionSnapshotDigest(t *testing.T) {
	t.Parallel()

	operation := operationFixture(operatorv1alpha1.OperationResolve)
	operation.AdmissionSnapshot = &operatorv1alpha1.PodAdmissionSnapshot{
		Digest: digest('a'), TemplateDigest: digest('b'),
	}
	job, err := builderFixture().Build(schemaFixture(), operation, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := job.Annotations[AnnotationAdmissionSnapshotDigest]; got != digest('a') {
		t.Fatalf("Job admission snapshot digest = %q", got)
	}
	if got := job.Spec.Template.Annotations[AnnotationAdmissionSnapshotDigest]; got != digest('a') {
		t.Fatalf("Pod template admission snapshot digest = %q", got)
	}

	operation.AdmissionSnapshot.Digest = "sha256:INVALID"
	if _, err := builderFixture().Build(schemaFixture(), operation, nil); err == nil {
		t.Fatal("Build() accepted a non-canonical admission snapshot digest")
	}
}

func TestBuildUsesOnlyBoundedMemoryBackedEmptyDirs(t *testing.T) {
	t.Parallel()

	builder := builderFixture()
	schema := schemaFixture()
	plan := planFixture(schema, builder)
	for _, test := range []struct {
		operation operatorv1alpha1.OperationType
		plan      *operatorv1alpha1.PtahSchemaPlan
	}{
		{operation: operatorv1alpha1.OperationResolve},
		{operation: operatorv1alpha1.OperationObserve},
		{operation: operatorv1alpha1.OperationPlan},
		{operation: operatorv1alpha1.OperationApply, plan: plan},
	} {
		job, err := builder.Build(schema.DeepCopy(), operationFixture(test.operation), test.plan)
		if err != nil {
			t.Fatalf("Build(%s) error = %v", test.operation, err)
		}
		found := 0
		for _, volume := range job.Spec.Template.Spec.Volumes {
			if volume.EmptyDir == nil {
				continue
			}
			found++
			if volume.EmptyDir.Medium != corev1.StorageMediumMemory || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Sign() <= 0 {
				t.Errorf("%s volume %s EmptyDir = %#v, want bounded memory", test.operation, volume.Name, volume.EmptyDir)
			}
		}
		if found < 2 {
			t.Errorf("%s Job has %d work EmptyDirs, want at least runner and work", test.operation, found)
		}
	}
}

func TestBuildDatabaseOperationsUseOnlyVerifiedPinnedSource(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	schema := schemaFixture()

	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationObserve), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	schemaFile := requireEnv(t, job, "PTAH_SCHEMA_FILE")
	if schemaFile.Value != sourceFilePath {
		t.Fatalf("PTAH_SCHEMA_FILE = %q, want isolated local source", schemaFile.Value)
	}
	database := requireEnv(t, job, "PTAH_DB_URL")
	if database.Value != "" || database.ValueFrom == nil || database.ValueFrom.SecretKeyRef == nil {
		t.Fatal("PTAH_DB_URL must be a SecretKeyRef, not a controller-read value")
	}
	if got := database.ValueFrom.SecretKeyRef.Name; got != "database" {
		t.Fatalf("target Secret name = %q, want database", got)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("init containers = %d, want installer and source fetch", len(job.Spec.Template.Spec.InitContainers))
	}
	fetch := job.Spec.Template.Spec.InitContainers[1]
	if fetch.Name != fetchContainerName || !reflect.DeepEqual(fetch.Command, []string{ptahBinaryPath}) ||
		!reflect.DeepEqual(fetch.Args, []string{"schema", "pull", schema.Status.Source.ResolvedReference, "--out", sourceFilePath}) {
		t.Fatalf("source fetch command = %q %q", fetch.Command, fetch.Args)
	}
	if _, ok := containerEnvMap(fetch)["PTAH_DB_URL"]; ok {
		t.Fatal("registry fetch init received the target database reference")
	}
	if _, ok := containerEnvMap(fetch)["PTAH_OCI_TOKEN"]; !ok {
		t.Fatal("registry fetch init did not receive registry credential references")
	}
	mainMount := requireMount(t, job.Spec.Template.Spec.Containers[0], sourceVolumeName)
	if !mainMount.ReadOnly || mainMount.MountPath != sourcePath {
		t.Fatalf("main source mount = %#v, want read-only %s", mainMount, sourcePath)
	}

	for name, mutate := range map[string]func(*operatorv1alpha1.ActiveOperationStatus){
		"mutable reference": func(operation *operatorv1alpha1.ActiveOperationStatus) {
			operation.Source.ResolvedReference = "oci://registry.example/acme/orders:mutable"
		},
		"digest mismatch": func(operation *operatorv1alpha1.ActiveOperationStatus) {
			operation.Source.Digest = digest('9')
		},
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			invalid := operationFixture(operatorv1alpha1.OperationObserve)
			mutate(&invalid)
			if _, err := builder.Build(schemaFixture(), invalid, nil); err == nil {
				t.Fatal("Build() accepted an unsafe database source")
			}
		})
	}
}

func TestBuildRejectsInvalidCoordinationKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", "Production/orders", "production orders"} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Target.CoordinationKey = key
			if _, err := builderFixture().Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil); err == nil ||
				!strings.Contains(err.Error(), "coordination") {
				t.Fatalf("Build() error = %v, want fail-closed coordination-key rejection", err)
			}
		})
	}
}

func TestBuildPostApplyObservationUsesPersistedTargetAndPolicy(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	originalSource := operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: schema.Status.Source.ResolvedReference,
		Digest:            schema.Status.Source.Digest,
		RegistryAuthFrom:  schema.Spec.Desired.RegistryAuthFrom.DeepCopy(),
	}
	schema.Spec.Desired.Transport.DeepCopyInto(&originalSource.Transport)
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome:            operatorv1alpha1.PendingObservationApplySucceeded,
		Plan:               operatorv1alpha1.CurrentPlanStatus{ArtifactDigest: schema.Status.Source.Digest, CoordinationDigest: testCoordinationDigest()},
		CoordinationDigest: testCoordinationDigest(),
		Source:             originalSource,
		Target: operatorv1alpha1.DatabaseTargetBinding{
			Engine: operatorv1alpha1.DatabaseEnginePostgreSQL,
			URLFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "applied-target"}, Key: "dsn",
			},
		},
		Exclude: []string{"archive.*"}, DriftSeverity: "all",
	}
	schema.Spec.Target.URLFrom.Name = "new-generation-target"
	schema.Spec.Desired.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
		Name: "new-generation-registry", Mode: operatorv1alpha1.RegistryAuthEnvironment,
	}
	schema.Spec.Desired.Transport = operatorv1alpha1.OCITransportSpec{
		PlainHTTP: false,
		CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "new-generation-ca"}, Key: "ca.pem",
		},
		ClientCertificateFrom: &operatorv1alpha1.TLSSecretReference{Name: "new-generation-client"},
	}
	schema.Spec.Policy.Exclude = []string{"new-generation-exclude"}
	schema.Spec.Policy.DriftSeverity = "destructive"

	active := operationFixture(operatorv1alpha1.OperationObserve)
	active.Target = &schema.Status.PendingObservation.Target
	active.Source = schema.Status.PendingObservation.Source.DeepCopy()
	active.CoordinationDigest = schema.Status.PendingObservation.CoordinationDigest
	active.ObservationExclude = append([]string(nil), schema.Status.PendingObservation.Exclude...)
	active.ObservationSeverity = schema.Status.PendingObservation.DriftSeverity
	job, err := builderFixture().Build(schema, active, nil)
	if err != nil {
		t.Fatal(err)
	}
	database := requireEnv(t, job, "PTAH_DB_URL")
	if database.ValueFrom == nil || database.ValueFrom.SecretKeyRef == nil ||
		database.ValueFrom.SecretKeyRef.Name != "applied-target" || database.ValueFrom.SecretKeyRef.Key != "dsn" {
		t.Fatalf("post-apply database binding = %#v", database.ValueFrom)
	}
	if _, ok := envMap(job)["PTAH_EXCLUDE"]; ok {
		t.Fatal("raw post-Apply observation unexpectedly received managed-scope exclusions")
	}
	if got := requireEnv(t, job, "PTAH_SEVERITY").Value; got != "all" {
		t.Fatalf("PTAH_SEVERITY = %q, want persisted policy", got)
	}
	if got := requireEnv(t, job, runner.EnvCoordinationDigest).Value; got != testCoordinationDigest() {
		t.Fatalf("coordination digest = %q, want persisted proof binding", got)
	}
	fetch := requireContainer(t, job.Spec.Template.Spec.InitContainers, fetchContainerName)
	if !reflect.DeepEqual(fetch.Args, []string{"schema", "pull", originalSource.ResolvedReference, "--out", sourceFilePath}) {
		t.Fatalf("post-apply source fetch args = %q, want persisted reference", fetch.Args)
	}
	fetchEnvironment := containerEnvMap(fetch)
	for _, name := range []string{"PTAH_OCI_USERNAME", runner.EnvOCIPassword, runner.EnvOCIToken, "PTAH_OCI_REGISTRY"} {
		environment := fetchEnvironment[name]
		if environment.ValueFrom == nil || environment.ValueFrom.SecretKeyRef == nil ||
			environment.ValueFrom.SecretKeyRef.Name != originalSource.RegistryAuthFrom.Name {
			t.Fatalf("post-apply %s source = %#v, want persisted registry Secret", name, environment.ValueFrom)
		}
	}
	if got := requireVolume(t, job, caVolumeName).ConfigMap.Name; got != originalSource.Transport.CAFrom.Name {
		t.Fatalf("post-apply CA source = %q, want persisted ConfigMap", got)
	}
	if got := requireVolume(t, job, tlsVolumeName).Secret.SecretName; got != originalSource.Transport.ClientCertificateFrom.Name {
		t.Fatalf("post-apply client certificate source = %q, want persisted Secret", got)
	}
}

func TestBuildObserveAndPlanSeparateRegistryAndDatabaseCredentials(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
	} {
		operation := operation
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			if operation == operatorv1alpha1.OperationPlan {
				schema.Spec.Desired.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
					Name:                "registry-docker",
					Mode:                operatorv1alpha1.RegistryAuthDockerConfigJSON,
					DockerConfigJSONKey: corev1.DockerConfigJsonKey,
				}
			}
			active := operationFixture(operation)
			active.Source.RegistryAuthFrom = schema.Spec.Desired.RegistryAuthFrom.DeepCopy()
			job, err := builderFixture().Build(schema, active, nil)
			if err != nil {
				t.Fatal(err)
			}
			pod := job.Spec.Template.Spec
			main := pod.Containers[0]
			fetch := requireContainer(t, pod.InitContainers, fetchContainerName)
			mainEnvironment := containerEnvMap(main)
			fetchEnvironment := containerEnvMap(fetch)

			for _, name := range []string{"PTAH_DB_URL", "PTAH_DEV_URL"} {
				if _, ok := fetchEnvironment[name]; ok {
					t.Errorf("source fetch unexpectedly received %s", name)
				}
			}
			for _, name := range []string{"PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_REGISTRY", "DOCKER_CONFIG", "PTAH_OCI_CA_FILE", "PTAH_OCI_CLIENT_CERT", "PTAH_OCI_CLIENT_KEY"} {
				if _, ok := mainEnvironment[name]; ok {
					t.Errorf("database container unexpectedly received %s", name)
				}
			}
			for _, name := range []string{dockerVolumeName, caVolumeName, tlsVolumeName} {
				if hasMount(main, name) {
					t.Errorf("database container unexpectedly mounts %s", name)
				}
			}
			if _, ok := mainEnvironment["PTAH_DB_URL"]; !ok {
				t.Fatal("database container is missing its target Secret reference")
			}
			if operation == operatorv1alpha1.OperationObserve {
				if _, ok := fetchEnvironment["PTAH_OCI_PASSWORD"]; !ok {
					t.Fatal("source fetch is missing registry Secret references")
				}
			} else {
				if got := fetchEnvironment["DOCKER_CONFIG"].Value; got != dockerConfigPath || !hasMount(fetch, dockerVolumeName) {
					t.Fatal("source fetch is missing the Docker config Secret mount")
				}
			}
			if got := mainEnvironment["PTAH_SCHEMA_FILE"].Value; got != sourceFilePath {
				t.Fatalf("database schema source = %q, want %q", got, sourceFilePath)
			}
		})
	}
}

func TestBuildVerifyPreservesOriginalDigestReferenceForPolicy(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Spec.Desired.OCIRef = schema.Status.Source.ResolvedReference
	job, err := builderFixture().Build(schema, operationFixture(operatorv1alpha1.OperationVerify), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := requireEnv(t, job, "PTAH_REQUESTED_REFERENCE").Value; got != schema.Spec.Desired.OCIRef {
		t.Fatalf("requested reference = %q, want %q", got, schema.Spec.Desired.OCIRef)
	}
	if got := requireEnv(t, job, "PTAH_RESOLVED_REFERENCE").Value; got != schema.Status.Source.ResolvedReference {
		t.Fatalf("resolved reference = %q, want %q", got, schema.Status.Source.ResolvedReference)
	}
}

func TestBuildApplyProjectsExactCommittedPlan(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	builder := builderFixture()
	plan := planFixture(schema, builder)
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), plan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	volume := requireVolume(t, job, planVolumeName)
	if volume.Projected == nil {
		t.Fatal("plan volume is not a projected volume")
	}
	wantSources, err := planstore.VolumeSources(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(volume.Projected.Sources, wantSources) {
		t.Fatalf("plan projections = %#v, want %#v", volume.Projected.Sources, wantSources)
	}
	if got := requireEnv(t, job, "PTAH_EXPECTED_PLAN_CONTENT_DIGEST").Value; got != plan.Spec.ContentDigest {
		t.Fatalf("expected plan digest = %q, want %q", got, plan.Spec.ContentDigest)
	}
	if got := requireEnv(t, job, "PTAH_EXPECTED_TARGET_IDENTITY_DIGEST").Value; got != plan.Spec.TargetIdentityDigest {
		t.Fatalf("expected target digest = %q, want %q", got, plan.Spec.TargetIdentityDigest)
	}
	if got := requireEnv(t, job, runner.EnvExpectedCoordinationDigest).Value; got != plan.Spec.CoordinationDigest {
		t.Fatalf("expected coordination digest = %q, want %q", got, plan.Spec.CoordinationDigest)
	}
	if got := job.Annotations[AnnotationPlanFingerprint]; got != plan.Spec.Fingerprint {
		t.Fatalf("plan fingerprint annotation = %q, want %q", got, plan.Spec.Fingerprint)
	}
	main := job.Spec.Template.Spec.Containers[0]
	mount := requireMount(t, main, planVolumeName)
	if !mount.ReadOnly || mount.MountPath != planPath {
		t.Fatalf("plan mount = %#v, want read-only %s", mount, planPath)
	}
}

func TestBuildApplyRejectsStaleBindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, *Builder)
	}{
		{
			name: "not ready",
			mutate: func(_ *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan, _ *Builder) {
				plan.Status.Conditions[0].Status = metav1.ConditionFalse
			},
		},
		{
			name: "replaced plan UID",
			mutate: func(_ *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan, _ *Builder) {
				plan.UID = "replacement"
			},
		},
		{
			name: "coordination key changed",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan, _ *Builder) {
				schema.Spec.Target.CoordinationKey = "prod/team-a/orders-through-another-route"
			},
		},
		{
			name: "executor changed",
			mutate: func(_ *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan, builder *Builder) {
				builder.ExecutorImage = "example.invalid/ptah@sha256:" + strings.Repeat("8", 64)
			},
		},
		{
			name: "apply disabled",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan, _ *Builder) {
				schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyNever
			},
		},
		{
			name: "required approval missing",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan, _ *Builder) {
				schema.Status.Plan.Approval = nil
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			builder := builderFixture()
			plan := planFixture(schema, builder)
			test.mutate(schema, plan, &builder)
			if _, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), plan); err == nil {
				t.Fatal("Build() accepted a stale apply binding")
			}
		})
	}
}

func TestBuildHardensEveryContainerAndPod(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	builder := builderFixture()
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationPlan), nil)
	if err != nil {
		t.Fatal(err)
	}

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("activeDeadlineSeconds = %v, want 600", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed {
		t.Fatalf("podReplacementPolicy = %v, want Failed", job.Spec.PodReplacementPolicy)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("TTL must remain unset until the controller harvests logs")
	}
	pod := job.Spec.Template.Spec
	if pod.ActiveDeadlineSeconds == nil || *pod.ActiveDeadlineSeconds != 600 {
		t.Fatalf("Pod activeDeadlineSeconds = %v, want 600", pod.ActiveDeadlineSeconds)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("service account token must not be mounted")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Fatal("service-link environment injection must be disabled")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart policy = %q, want Never", pod.RestartPolicy)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context is not hardened: %#v", pod.SecurityContext)
	}
	if len(pod.InitContainers) != 2 || len(pod.Containers) != 1 {
		t.Fatalf("containers = %d init, %d main; want installer, fetch, and one main", len(pod.InitContainers), len(pod.Containers))
	}
	init := pod.InitContainers[0]
	main := pod.Containers[0]
	if init.Image != builder.RunnerImage || !reflect.DeepEqual(init.Command, []string{"/ptah-runner"}) ||
		!reflect.DeepEqual(init.Args, []string{"--install-to", runnerPath}) {
		t.Fatalf("runner installer = image %q command %q args %q", init.Image, init.Command, init.Args)
	}
	if len(init.Env) != 0 {
		t.Fatal("runner installer must receive no credentials or operation environment")
	}
	if main.Image != builder.ExecutorImage || !reflect.DeepEqual(main.Command, []string{runnerPath}) {
		t.Fatalf("main image/command = %q %q", main.Image, main.Command)
	}
	if !reflect.DeepEqual(main.Args, []string{
		"--ptah-binary", ptahBinaryPath,
		"--max-result-bytes", "8388608",
		"--max-plan-bytes", "8388608",
		"--operation", "plan",
	}) {
		t.Fatalf("main args = %q", main.Args)
	}
	for _, container := range []corev1.Container{init, pod.InitContainers[1], main} {
		security := container.SecurityContext
		if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
			security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
			security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
			security.Capabilities == nil || !reflect.DeepEqual(security.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
			security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("container %q security context is not hardened: %#v", container.Name, security)
		}
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != schema.UID ||
		job.OwnerReferences[0].Controller == nil || !*job.OwnerReferences[0].Controller {
		t.Fatalf("schema owner reference = %#v", job.OwnerReferences)
	}
}

func TestApplyPodCarriesIndependentAbsoluteAndRuntimeDeadlines(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	builder := builderFixture()
	plan := planFixture(schema, builder)
	operation := operationFixture(operatorv1alpha1.OperationApply)
	job, err := builder.Build(schema, operation, plan)
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("Job activeDeadlineSeconds = %#v, want 600", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.Template.Spec.ActiveDeadlineSeconds == nil || *job.Spec.Template.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("orphan-safe Pod activeDeadlineSeconds = %#v, want 600", job.Spec.Template.Spec.ActiveDeadlineSeconds)
	}
	wantNotAfter := operation.StartedAt.Add(600 * time.Second).UTC().Format(time.RFC3339Nano)
	if got := requireEnv(t, job, runner.EnvDispatchNotAfter).Value; got != wantNotAfter {
		t.Fatalf("dispatch deadline = %q, want %q", got, wantNotAfter)
	}
	if operation.LeaseDurationSeconds <= int32(*job.Spec.Template.Spec.ActiveDeadlineSeconds) {
		t.Fatalf("Lease duration %d does not cover Pod deadline %d plus grace", operation.LeaseDurationSeconds, *job.Spec.Template.Spec.ActiveDeadlineSeconds)
	}
}

func TestBuildRegistryDockerConfigAndTransportAreFileReferences(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Spec.Desired.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
		Name:                "registry-docker",
		Mode:                operatorv1alpha1.RegistryAuthDockerConfigJSON,
		DockerConfigJSONKey: "auth.json",
	}
	job, err := builderFixture().Build(schema, operationFixture(operatorv1alpha1.OperationVerify), nil)
	if err != nil {
		t.Fatal(err)
	}

	docker := requireVolume(t, job, dockerVolumeName)
	if docker.Secret == nil || docker.Secret.SecretName != "registry-docker" ||
		len(docker.Secret.Items) != 1 || docker.Secret.Items[0].Key != "auth.json" || docker.Secret.Items[0].Path != "config.json" {
		t.Fatalf("Docker config volume = %#v", docker)
	}
	if got := requireEnv(t, job, "DOCKER_CONFIG").Value; got != dockerConfigPath {
		t.Fatalf("DOCKER_CONFIG = %q, want %q", got, dockerConfigPath)
	}
	if got := requireEnv(t, job, "PTAH_OCI_CA_FILE").Value; got != caFilePath {
		t.Fatalf("PTAH_OCI_CA_FILE = %q", got)
	}
	if got := requireEnv(t, job, "PTAH_OCI_CLIENT_CERT").Value; got != clientCertificatePath {
		t.Fatalf("PTAH_OCI_CLIENT_CERT = %q", got)
	}
	if got := requireEnv(t, job, "PTAH_OCI_CLIENT_KEY").Value; got != clientKeyPath {
		t.Fatalf("PTAH_OCI_CLIENT_KEY = %q", got)
	}
	assertSecretReferencesOnly(t, job)
}

func TestBuildRejectsTaggedExecutionImages(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	operation := operationFixture(operatorv1alpha1.OperationResolve)
	for name, mutate := range map[string]func(*Builder){
		"executor": func(builder *Builder) { builder.ExecutorImage = "example.invalid/ptah:v0.3.0" },
		"runner":   func(builder *Builder) { builder.RunnerImage = "example.invalid/operator:latest" },
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			builder := builderFixture()
			mutate(&builder)
			if _, err := builder.Build(schema, operation, nil); err == nil || !strings.Contains(err.Error(), "pinned") {
				t.Fatalf("Build() error = %v, want pinned-image rejection", err)
			}
		})
	}
}

func TestNameForIsDeterministicBoundedAndAttemptSpecific(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Name = strings.Repeat("a", 63)
	operation := operationFixture(operatorv1alpha1.OperationObserve)
	first, err := NameFor(schema, operation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NameFor(schema, operation)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("NameFor() is not deterministic: %q != %q", first, second)
	}
	if problems := validation.IsDNS1123Subdomain(first); len(problems) > 0 {
		t.Fatalf("NameFor() = %q is invalid: %v", first, problems)
	}
	operation.Attempt++
	retry, err := NameFor(schema, operation)
	if err != nil {
		t.Fatal(err)
	}
	if retry == first {
		t.Fatal("a new attempt reused the prior deterministic Job name")
	}
}

func TestBuilderExposesValidatedExecutionBinding(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	if err := builder.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	ptahVersion, executorImage, runnerImage, protocolVersion := builder.ExecutionBinding()
	if ptahVersion != builder.PtahVersion || executorImage != builder.ExecutorImage || runnerImage != builder.RunnerImage ||
		protocolVersion != int32(runner.ProtocolVersion) {
		t.Fatalf("ExecutionBinding() = %q, %q, %q, %d", ptahVersion, executorImage, runnerImage, protocolVersion)
	}
	operation := operationFixture(operatorv1alpha1.OperationResolve)
	methodName, err := builder.NameFor(schemaFixture(), operation)
	if err != nil {
		t.Fatal(err)
	}
	packageName, err := NameFor(schemaFixture(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if methodName != packageName {
		t.Fatalf("Builder.NameFor() = %q, package NameFor() = %q", methodName, packageName)
	}
}

func schemaFixture() *operatorv1alpha1.PtahSchema {
	artifactDigest := digest('a')
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: types.UID("schema-uid")},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "prod/team-a/orders-primary",
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"},
					Key:                  "url",
				},
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.example/acme/orders:stable",
				RegistryAuthFrom: &operatorv1alpha1.RegistryAuthSource{
					Name:        "registry",
					Mode:        operatorv1alpha1.RegistryAuthEnvironment,
					UsernameKey: "user",
					PasswordKey: "pass",
					TokenKey:    "identity-token",
					RegistryKey: "host",
				},
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"},
					Key:                  "policy.yaml",
				},
				Transport: operatorv1alpha1.OCITransportSpec{
					PlainHTTP: true,
					CAFrom: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "registry-ca"},
						Key:                  "ca.pem",
					},
					ClientCertificateFrom: &operatorv1alpha1.TLSSecretReference{
						Name:           "registry-client",
						CertificateKey: "cert.pem",
						PrivateKeyKey:  "key.pem",
					},
				},
			},
			Dev: &operatorv1alpha1.DatabaseTargetRef{URLFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "development-database"},
				Key:                  "url",
			}},
			Policy: operatorv1alpha1.ReconciliationPolicy{
				DriftSeverity:   "destructive",
				Exclude:         []string{"archive.*", "column,with-comma"},
				LockTimeout:     metav1.Duration{Duration: 12 * time.Second},
				TransactionMode: "all",
			},
			Execution: operatorv1alpha1.ExecutionSpec{
				ActiveDeadlineSeconds: 600,
				ConnectTimeout:        metav1.Duration{Duration: 7 * time.Second},
				ServiceAccountName:    "schema-jobs",
				ImagePullSecrets:      []corev1.LocalObjectReference{{Name: "image-pull"}},
				NodeSelector:          map[string]string{"workload": "database"},
			},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{
			Source: operatorv1alpha1.SchemaSourceStatus{
				RequestedReference:       "oci://registry.example/acme/orders:stable",
				ResolvedReference:        "oci://registry.example/acme/orders@" + artifactDigest,
				Digest:                   artifactDigest,
				ArtifactType:             dataplane.SchemaArtifactType,
				Verified:                 true,
				VerificationPolicyUID:    "verification-policy-uid",
				VerificationPolicyDigest: digest('5'),
			},
			Target: operatorv1alpha1.TargetStatus{
				CoordinationDigest: testCoordinationDigest(),
				IdentityDigest:     digest('b'),
				DriftReportDigest:  digest('c'),
			},
		},
	}
}

func builderFixture() Builder {
	return Builder{
		ExecutorImage: "example.invalid/ptah@" + digest('d'),
		RunnerImage:   "example.invalid/operator@" + digest('e'),
		PtahVersion:   "v0.3.0",
	}
}

func operationFixture(operation operatorv1alpha1.OperationType) operatorv1alpha1.ActiveOperationStatus {
	schema := schemaFixture()
	active := operatorv1alpha1.ActiveOperationStatus{
		Type:             operation,
		ID:               "operation-01",
		InputFingerprint: digest('f'),
		StartedAt:        metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
		Attempt:          1,
	}
	if operation == operatorv1alpha1.OperationVerify {
		active.VerificationPolicyUID = schema.Status.Source.VerificationPolicyUID
		active.VerificationPolicyDigest = schema.Status.Source.VerificationPolicyDigest
	}
	if operation == operatorv1alpha1.OperationObserve || operation == operatorv1alpha1.OperationPlan {
		active.CoordinationDigest = schema.Status.Target.CoordinationDigest
		active.TargetIdentityDigest = schema.Status.Target.IdentityDigest
		active.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  schema.Spec.Target.Engine,
			URLFrom: *schema.Spec.Target.URLFrom.DeepCopy(),
		}
		active.Source = &operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: schema.Status.Source.ResolvedReference,
			Digest:            schema.Status.Source.Digest,
			RegistryAuthFrom:  schema.Spec.Desired.RegistryAuthFrom.DeepCopy(),
		}
		schema.Spec.Desired.Transport.DeepCopyInto(&active.Source.Transport)
		active.ObservationExclude = append([]string(nil), schema.Spec.Policy.Exclude...)
		active.ObservationSeverity = schema.Spec.Policy.DriftSeverity
		active.ObservationDev = schema.Spec.Dev.DeepCopy()
		active.ObservationConnectTimeout = schema.Spec.Execution.ConnectTimeout
		active.ObservationLockTimeout = schema.Spec.Policy.LockTimeout
		if operation == operatorv1alpha1.OperationPlan {
			active.LeaseDurationSeconds = 660
		}
	}
	if operation == operatorv1alpha1.OperationApply {
		active.CoordinationDigest = testCoordinationDigest()
		active.TargetIdentityDigest = digest('b')
		active.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine: operatorv1alpha1.DatabaseEnginePostgreSQL,
			URLFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
			},
		}
		active.LeaseDurationSeconds = 660
		dispatchNotAfter := metav1.NewTime(active.StartedAt.Add(600 * time.Second))
		active.DispatchNotAfter = &dispatchNotAfter
		active.ExecutionNotAfter = dispatchNotAfter.DeepCopy()
		active.TerminationGracePeriodSeconds = 30
	}
	return active
}

func planFixture(schema *operatorv1alpha1.PtahSchema, builder Builder) *operatorv1alpha1.PtahSchemaPlan {
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  schema.Namespace,
			Name:       "ptah-plan-0123456789abcdef",
			UID:        types.UID("plan-uid"),
			Generation: 1,
		},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          1,
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              digest('1'),
			ContentDigest:            digest('2'),
			Size:                     9,
			ArtifactDigest:           schema.Status.Source.Digest,
			CoordinationDigest:       schema.Status.Target.CoordinationDigest,
			TargetIdentityDigest:     schema.Status.Target.IdentityDigest,
			ActualStateFingerprint:   digest('c'),
			DesiredStateFingerprint:  digest('3'),
			PolicyFingerprint:        digest('4'),
			VerificationPolicyUID:    schema.Status.Source.VerificationPolicyUID,
			VerificationPolicyDigest: digest('5'),
			PtahVersion:              builder.PtahVersion,
			ExecutorImage:            builder.ExecutorImage,
			RunnerImage:              builder.RunnerImage,
			RunnerProtocolVersion:    int32(runner.ProtocolVersion),
			Dialect:                  "postgresql",
			StatementCount:           1,
			Chunks: []operatorv1alpha1.PlanChunkReference{
				{Name: "ptah-plan-0123456789abcdef-000", Key: planstore.ChunkDataKey, Index: 0, Digest: digest('6'), Size: 4},
				{Name: "ptah-plan-0123456789abcdef-001", Key: planstore.ChunkDataKey, Index: 1, Digest: digest('7'), Size: 5},
			},
		},
		Status: operatorv1alpha1.PtahSchemaPlanStatus{
			ObservedGeneration: 1,
			PublishedChunks: []operatorv1alpha1.PublishedPlanChunkStatus{
				{Name: "ptah-plan-0123456789abcdef-000", UID: types.UID("chunk-0"), Index: 0},
				{Name: "ptah-plan-0123456789abcdef-001", UID: types.UID("chunk-1"), Index: 1},
			},
			Conditions: []metav1.Condition{{
				Type:               operatorv1alpha1.ConditionPlanStorageReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Published",
			}},
		},
	}
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name:                     plan.Name,
		UID:                      plan.UID,
		Fingerprint:              plan.Spec.Fingerprint,
		ContentDigest:            plan.Spec.ContentDigest,
		ArtifactDigest:           plan.Spec.ArtifactDigest,
		CoordinationDigest:       plan.Spec.CoordinationDigest,
		TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
		DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint:        plan.Spec.PolicyFingerprint,
		VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
		VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		PtahVersion:              plan.Spec.PtahVersion,
		ExecutorImage:            plan.Spec.ExecutorImage,
		RunnerImage:              plan.Spec.RunnerImage,
		RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
		Destructive:              plan.Spec.Destructive,
		StatementCount:           plan.Spec.StatementCount,
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{
			Name: "approve-plan",
			UID:  types.UID("approval-uid"),
		},
	}
	return plan
}

func digest(character byte) string { return "sha256:" + strings.Repeat(string(character), 64) }

func testCoordinationDigest() string {
	digest, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/team-a/orders-primary")
	if err != nil {
		panic(err)
	}
	return digest
}

func envMap(job *batchv1.Job) map[string]corev1.EnvVar {
	result := make(map[string]corev1.EnvVar)
	for _, environment := range job.Spec.Template.Spec.Containers[0].Env {
		result[environment.Name] = environment
	}
	return result
}

func requireEnv(t *testing.T, job *batchv1.Job, name string) corev1.EnvVar {
	t.Helper()
	environment, ok := envMap(job)[name]
	if !ok {
		t.Fatalf("environment is missing %s", name)
	}
	return environment
}

func requireVolume(t *testing.T, job *batchv1.Job, name string) corev1.Volume {
	t.Helper()
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return volume
		}
	}
	t.Fatalf("volume %q not found", name)
	return corev1.Volume{}
}

func requireMount(t *testing.T, container corev1.Container, name string) corev1.VolumeMount {
	t.Helper()
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return mount
		}
	}
	t.Fatalf("volume mount %q not found", name)
	return corev1.VolumeMount{}
}

func requireContainer(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, container := range containers {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("container %q not found", name)
	return corev1.Container{}
}

func hasMount(container corev1.Container, name string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func assertSecretReferencesOnly(t *testing.T, job *batchv1.Job) {
	t.Helper()
	if len(job.Spec.Template.Spec.InitContainers[0].Env) != 0 {
		t.Fatal("init container unexpectedly received environment variables")
	}
	containers := append([]corev1.Container(nil), job.Spec.Template.Spec.InitContainers...)
	containers = append(containers, job.Spec.Template.Spec.Containers...)
	for _, container := range containers {
		for _, environment := range container.Env {
			if environment.ValueFrom == nil {
				continue
			}
			if environment.Value != "" {
				t.Errorf("container %s environment %s has both a value and a reference", container.Name, environment.Name)
			}
			if environment.ValueFrom.SecretKeyRef == nil {
				t.Errorf("container %s environment %s has a non-Secret value source", container.Name, environment.Name)
			}
		}
	}
}

func containerEnvMap(container corev1.Container) map[string]corev1.EnvVar {
	result := make(map[string]corev1.EnvVar)
	for _, environment := range container.Env {
		result[environment.Name] = environment
	}
	return result
}
