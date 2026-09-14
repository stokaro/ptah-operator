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
	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestBuildMigrationCredentialAndInputIsolationByOperation(t *testing.T) {
	t.Parallel()
	builder := builderFixture()

	tests := []struct {
		operation operatorv1alpha1.MigrationOperationType
		want      []string
		absent    []string
	}{
		{
			operation: operatorv1alpha1.MigrationOperationResolve,
			want:      []string{"PTAH_REQUESTED_REFERENCE", "PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN"},
			absent:    []string{"PTAH_DB_URL", "PTAH_MIGRATIONS_DIR", "PTAH_VERIFICATION_POLICY", "PTAH_COORDINATION_DIGEST"},
		},
		{
			operation: operatorv1alpha1.MigrationOperationVerify,
			want: []string{
				"PTAH_REQUESTED_REFERENCE", "PTAH_RESOLVED_REFERENCE",
				"PTAH_VERIFICATION_POLICY", "PTAH_EXPECTED_ARTIFACT_TYPE", "PTAH_OCI_TOKEN",
			},
			absent: []string{"PTAH_DB_URL", "PTAH_MIGRATIONS_DIR", "PTAH_COORDINATION_DIGEST"},
		},
		{
			operation: operatorv1alpha1.MigrationOperationHistory,
			want: []string{
				"PTAH_DB_URL", "PTAH_EXPECTED_DATABASE_ENGINE", "PTAH_COORDINATION_DIGEST",
				"PTAH_MIGRATIONS_DIR", "PTAH_CONNECT_TIMEOUT", "PTAH_MIGRATION_LOCK_TIMEOUT",
			},
			absent: []string{
				"PTAH_REQUESTED_REFERENCE", "PTAH_RESOLVED_REFERENCE", "PTAH_VERIFICATION_POLICY",
				"PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_REGISTRY",
				"DOCKER_CONFIG", "PTAH_PLAIN_HTTP", "PTAH_DISPATCH_NOT_AFTER", "PTAH_EXECUTION_NOT_AFTER",
			},
		},
		{
			operation: operatorv1alpha1.MigrationOperationApply,
			want: []string{
				"PTAH_DB_URL", "PTAH_EXPECTED_DATABASE_ENGINE", "PTAH_COORDINATION_DIGEST",
				"PTAH_MIGRATIONS_DIR", "PTAH_DISPATCH_NOT_AFTER", "PTAH_EXECUTION_NOT_AFTER",
			},
			absent: []string{
				"PTAH_REQUESTED_REFERENCE", "PTAH_RESOLVED_REFERENCE", "PTAH_VERIFICATION_POLICY",
				"PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_REGISTRY",
				"DOCKER_CONFIG", "PTAH_PLAIN_HTTP",
			},
		},
	}

	for _, test := range tests {
		t.Run(string(test.operation), func(t *testing.T) {
			t.Parallel()
			job, err := builder.BuildMigration(migrationFixture(), migrationOperationFixture(test.operation))
			if err != nil {
				t.Fatalf("BuildMigration() error = %v", err)
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

// TestBuildMigrationKeepsRegistryCredentialsOutOfTheSQLProcess is the reason
// the fetch pair exists: Ptah can fetch `--migrations-dir oci://...` itself,
// and doing so would open the registry and the database in one process.
func TestBuildMigrationKeepsRegistryCredentialsOutOfTheSQLProcess(t *testing.T) {
	t.Parallel()
	builder := builderFixture()

	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory,
		operatorv1alpha1.MigrationOperationApply,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			claim := migrationOperationFixture(operation)
			job, err := builder.BuildMigration(migration, claim)
			if err != nil {
				t.Fatalf("BuildMigration() error = %v", err)
			}

			pod := job.Spec.Template.Spec
			fetch := requireContainer(t, pod.InitContainers, migrationFetchContainerName)
			want := []string{"migrations", "pull", claim.Source.ResolvedReference, "--out", migrationsPath}
			if !reflect.DeepEqual(fetch.Args, want) {
				t.Fatalf("fetch args = %q, want %q", fetch.Args, want)
			}
			if !reflect.DeepEqual(fetch.Command, []string{ptahBinaryPath}) {
				t.Fatalf("fetch command = %q, want the Ptah binary", fetch.Command)
			}
			fetchEnvironment := containerEnvMap(fetch)
			if _, ok := fetchEnvironment[runner.EnvDatabaseURL]; ok {
				t.Error("the fetch container must never receive the database URL")
			}
			if _, ok := fetchEnvironment[runner.EnvOCIPassword]; !ok {
				t.Error("the fetch container is the one that holds the registry credentials")
			}
			requireContainer(t, pod.InitContainers, guardContainerName)

			main := pod.Containers[0]
			source := requireMount(t, main, sourceVolumeName)
			if source.MountPath != sourcePath || !source.ReadOnly {
				t.Fatalf("main source mount = %#v, want %s read-only", source, sourcePath)
			}
			if hasMount(main, dockerVolumeName) || hasMount(main, caVolumeName) {
				t.Error("the SQL-running container must not mount registry credentials")
			}
			directory := requireEnv(t, job, runner.EnvMigrationsDir).Value
			if directory != migrationsPath || !strings.HasPrefix(directory, sourcePath+"/") {
				t.Fatalf("migrations directory = %q, want a path inside the fetched volume", directory)
			}
		})
	}
}

func TestBuildMigrationHardensEveryContainerAndPod(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	builder := builderFixture()
	job, err := builder.BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory))
	if err != nil {
		t.Fatal(err)
	}

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed {
		t.Fatalf("podReplacementPolicy = %v, want Failed", job.Spec.PodReplacementPolicy)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("TTL must remain unset until the controller harvests logs")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("activeDeadlineSeconds = %v, want the resource's 600", job.Spec.ActiveDeadlineSeconds)
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
	if pod.ServiceAccountName != "migration-jobs" {
		t.Fatalf("service account = %q, want the declared execution identity", pod.ServiceAccountName)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context is not hardened: %#v", pod.SecurityContext)
	}
	if len(pod.InitContainers) != 3 || len(pod.Containers) != 1 {
		t.Fatalf("containers = %d init, %d main; want installer, guard, fetch, and one main", len(pod.InitContainers), len(pod.Containers))
	}
	main := pod.Containers[0]
	if main.Image != builder.ExecutorImage || !reflect.DeepEqual(main.Command, []string{runnerPath}) {
		t.Fatalf("main image/command = %q %q", main.Image, main.Command)
	}
	if !reflect.DeepEqual(main.Args, []string{
		"--ptah-binary", ptahBinaryPath,
		"--max-result-bytes", "8388608",
		"--max-plan-bytes", "8388608",
		"--operation", "migration-history",
	}) {
		t.Fatalf("main args = %q", main.Args)
	}
	for _, container := range append(append([]corev1.Container(nil), pod.InitContainers...), main) {
		security := container.SecurityContext
		if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
			security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
			security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
			security.Capabilities == nil || !reflect.DeepEqual(security.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
			security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("container %q security context is not hardened: %#v", container.Name, security)
		}
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "PtahMigration" ||
		job.OwnerReferences[0].UID != migration.UID ||
		job.OwnerReferences[0].Controller == nil || !*job.OwnerReferences[0].Controller ||
		job.OwnerReferences[0].BlockOwnerDeletion == nil || !*job.OwnerReferences[0].BlockOwnerDeletion {
		t.Fatalf("migration owner reference = %#v", job.OwnerReferences)
	}
	if job.Labels[LabelMigration] != migration.Name || job.Labels[LabelComponent] != ComponentMigrationOperation ||
		job.Labels[LabelOperation] != "history" {
		t.Fatalf("job labels = %#v", job.Labels)
	}
	if job.Annotations[AnnotationOperationID] == "" || job.Annotations[AnnotationExecutionBindingID] == "" ||
		job.Annotations[AnnotationControllerRevision] != builder.ControllerRevision {
		t.Fatalf("job annotations = %#v", job.Annotations)
	}
}

func TestBuildMigrationApplyCarriesItsPlanAndBounds(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
	job, err := builder.BuildMigration(migrationFixture(), operation)
	if err != nil {
		t.Fatal(err)
	}

	wantNotAfter := operation.StartedAt.Add(300 * time.Second).UTC().Format(time.RFC3339Nano)
	if got := requireEnv(t, job, runner.EnvExecutionNotAfter).Value; got != wantNotAfter {
		t.Fatalf("execution deadline = %q, want %q", got, wantNotAfter)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Fatalf("activeDeadlineSeconds = %v, want what remains of the execution window", job.Spec.ActiveDeadlineSeconds)
	}
	// The Job carries no plan annotation: the claim names the plan, admission
	// reads the claim, and the sealed Job contract has no key for a migration
	// plan until the plan kind itself exists.
	for _, annotation := range job.Annotations {
		if strings.Contains(annotation, string(operation.PlanRef.UID)) {
			t.Fatal("the Job carries a plan binding the sealed contract does not know")
		}
	}
}

func TestBuildMigrationRefusesClaimsItCannotCarryOut(t *testing.T) {
	t.Parallel()
	builder := builderFixture()

	tests := []struct {
		name    string
		mutate  func(*operatorv1alpha1.PtahMigration, *operatorv1alpha1.MigrationOperationStatus)
		message string
	}{
		{
			name: "unknown operation",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.Type = "Rollback"
			},
			message: "unsupported migration operation",
		},
		{
			name: "no execution binding",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.MigrationOperationStatus) {
				migration.Status.ExecutionBinding = nil
			},
			message: "durable execution binding",
		},
		{
			name: "executor rolled out under the claim",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.MigrationOperationStatus) {
				migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + digest('9')
			},
			message: "execution binding is stale",
		},
		{
			name: "claim authorized under another epoch",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.ExecutionBindingID = "v1-44444444444444444444444444444444"
			},
			message: "execution binding is stale",
		},
		{
			name: "claim names a different Job",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.JobName = "ptah-m-apply-someone-elses-job"
			},
			message: "does not match deterministic name",
		},
		{
			name: "apply without a plan",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.PlanRef = nil
			},
			message: "carries no plan reference",
		},
		{
			name: "apply without a target",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.Target = nil
			},
			message: "carries no target binding",
		},
		{
			name: "apply without a resolved artifact",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.Source = nil
			},
			message: "carries no source binding",
		},
		{
			name: "apply whose source is not digest-pinned",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.Source.ResolvedReference = "oci://registry.example/acme/orders:stable"
			},
			message: "immutable SHA-256 reference",
		},
		{
			name: "execution window already closed",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				closed := metav1.NewTime(operation.StartedAt.Add(-time.Second))
				operation.ExecutionNotAfter = &closed
			},
			message: "execution window closed before dispatch",
		},
		{
			name: "attempt is not counted",
			mutate: func(_ *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) {
				operation.Attempt = 0
			},
			message: "attempt must be positive",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
			test.mutate(migration, &operation)
			_, err := builder.BuildMigration(migration, operation)
			if err == nil {
				t.Fatal("BuildMigration() accepted a claim it cannot carry out")
			}
			if !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want one mentioning %q", err, test.message)
			}
		})
	}
}

func TestNameForMigrationIsDeterministicBoundedAndAttemptSpecific(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	migration.Name = strings.Repeat("orders-service", 8)
	operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)

	first, err := NameForMigration(migration, operation)
	if err != nil {
		t.Fatal(err)
	}
	again, err := NameForMigration(migration, operation)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("name is not deterministic: %q then %q", first, again)
	}
	if problems := validation.IsDNS1123Subdomain(first); len(problems) != 0 {
		t.Fatalf("name %q is not a valid object name: %v", first, problems)
	}
	if len(first) > 63 {
		t.Fatalf("name %q is %d characters, over the 63 a Job selector allows", first, len(first))
	}

	operation.Attempt = 2
	retry, err := NameForMigration(migration, operation)
	if err != nil {
		t.Fatal(err)
	}
	if retry == first {
		t.Fatal("a retry must not reuse the attempt's Job name")
	}

	history := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
	readName, err := NameForMigration(migration, history)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(readName, "ptah-m-history-") {
		t.Fatalf("name %q does not say which operation it carries", readName)
	}
}

func migrationFixture() *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: types.UID("migration-uid")},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "prod/team-a/orders-primary",
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"},
					Key:                  "url",
				},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.example/acme/orders-migrations:stable",
				RegistryAuthFrom: &operatorv1alpha1.RegistryAuthSource{
					Name:        "registry",
					Mode:        operatorv1alpha1.RegistryAuthEnvironment,
					UsernameKey: "user",
					PasswordKey: "pass",
					TokenKey:    "identity-token",
					RegistryKey: operatorv1alpha1.RegistryAuthoritySecretKey,
				},
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"},
					Key:                  "policy.yaml",
				},
			},
			Policy: operatorv1alpha1.MigrationPolicy{
				Apply:       operatorv1alpha1.ApplyPolicyOnApproval,
				LockTimeout: metav1.Duration{Duration: 12 * time.Second},
			},
			Execution: operatorv1alpha1.ExecutionSpec{
				ActiveDeadlineSeconds: 600,
				ConnectTimeout:        metav1.Duration{Duration: 7 * time.Second},
				ServiceAccountName:    "migration-jobs",
				ImagePullSecrets:      []corev1.LocalObjectReference{{Name: "image-pull"}},
				NodeSelector:          map[string]string{"workload": "database"},
			},
		},
		Status: operatorv1alpha1.PtahMigrationStatus{
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch:                  "v1-33333333333333333333333333333333",
				ControllerImage:        "example.invalid/manager@" + digest('f'),
				ControllerRevision:     "controller-test-revision",
				ControllerStateVersion: 1,
				PtahVersion:            "v0.3.0",
				ExecutorImage:          "example.invalid/ptah@" + digest('d'),
				RunnerImage:            "example.invalid/operator@" + digest('e'),
				RunnerProtocolVersion:  int32(runner.ProtocolVersion),
			},
			Artifact: &operatorv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: "oci://registry.example/acme/orders-migrations@" + digest('a'),
				Digest:            digest('a'),
			},
		},
	}
}

func migrationOperationFixture(operation operatorv1alpha1.MigrationOperationType) operatorv1alpha1.MigrationOperationStatus {
	migration := migrationFixture()
	claim := operatorv1alpha1.MigrationOperationStatus{
		Type:               operation,
		ID:                 digest('1'),
		InputFingerprint:   digest('2'),
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
		StartedAt:          metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
		Attempt:            1,
	}
	if operation != operatorv1alpha1.MigrationOperationResolve {
		claim.Source = &operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: migration.Status.Artifact.ResolvedReference,
			Digest:            migration.Status.Artifact.Digest,
			RegistryAuthFrom:  migration.Spec.Artifact.RegistryAuthFrom.DeepCopy(),
		}
		migration.Spec.Artifact.Transport.DeepCopyInto(&claim.Source.Transport)
	}
	if migrationReadsArtifactBytes(operation) {
		claim.CoordinationDigest = testCoordinationDigest()
		claim.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		}
	}
	if operation == operatorv1alpha1.MigrationOperationApply {
		claim.PlanRef = &operatorv1alpha1.ImmutableObjectReference{
			Name: "orders-migration-plan-1", UID: types.UID("migration-plan-uid"),
		}
		dispatchNotAfter := metav1.NewTime(claim.StartedAt.Add(120 * time.Second))
		executionNotAfter := metav1.NewTime(claim.StartedAt.Add(300 * time.Second))
		claim.DispatchNotAfter = &dispatchNotAfter
		claim.ExecutionNotAfter = &executionNotAfter
	}
	return claim
}
