package e2e

import (
	"encoding/json"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	mtDatabaseSecret = "e2e-postgresql-migrations-db"
	mtRegistrySecret = "e2e-registry-auth"
)

var (
	mtExecutorImage = "example.invalid/ptah@" + builderDigest('d')
	mtRunnerImage   = "example.invalid/operator@" + builderDigest('e')
)

// mtBuilder is the manager's own Job builder, with the images the fixtures
// name.
func mtBuilder() workload.Builder {
	var key planseal.PublicKey
	for index := range key {
		key[index] = byte(index)
	}
	return workload.Builder{
		ExecutorImage: mtExecutorImage, RunnerImage: mtRunnerImage, PtahVersion: "v0.3.0",
		ControllerImage: "example.invalid/manager@" + builderDigest('f'), ControllerRevision: "controller-test-revision",
		ControllerStateVersion: 1, PlanSealPublicKey: key,
	}
}

// mtMigration is a migration declared as the phase declares its own: the
// database Secret's url key, the registry credential read as environment
// variables, and the registry reached over plain HTTP.
func mtMigration() *ptahv1alpha1.PtahMigration {
	return &ptahv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ptah-e2e", Name: "e2e-migrations-postgresql", UID: "migration-uid"},
		Spec: ptahv1alpha1.PtahMigrationSpec{
			Target: ptahv1alpha1.DatabaseTargetSpec{
				Engine: ptahv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "e2e/migrations/postgresql",
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: mtDatabaseSecret}, Key: "url"},
			},
			Artifact: ptahv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.ptah-e2e.svc.cluster.local:5000/migrations/postgresql:stable",
				RegistryAuthFrom: &ptahv1alpha1.RegistryAuthSource{
					Name: mtRegistrySecret, Mode: ptahv1alpha1.RegistryAuthEnvironment,
					UsernameKey: "username", PasswordKey: "password",
				},
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: migrationPolicy}, Key: migrationPolicyKey,
				},
				Transport: ptahv1alpha1.OCITransportSpec{PlainHTTP: true},
			},
			Policy: ptahv1alpha1.MigrationPolicy{
				Apply: ptahv1alpha1.ApplyPolicyOnApproval, LockTimeout: metav1.Duration{Duration: 30 * time.Second},
			},
			Execution: ptahv1alpha1.ExecutionSpec{
				ActiveDeadlineSeconds: 300, ConnectTimeout: metav1.Duration{Duration: 30 * time.Second},
			},
		},
		Status: ptahv1alpha1.PtahMigrationStatus{
			ExecutionBinding: &ptahv1alpha1.ExecutionBindingStatus{
				Epoch: "v1-33333333333333333333333333333333", ControllerStateVersion: 1, PtahVersion: "v0.3.0",
				ExecutorImage: mtExecutorImage, RunnerProtocolVersion: int32(runner.ProtocolVersion),
			},
			Artifact: &ptahv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: "oci://registry.ptah-e2e.svc.cluster.local:5000/migrations/postgresql@" + builderDigest('a'),
				Digest:            builderDigest('a'),
			},
		},
	}
}

// mtBuiltJobs are the four Jobs the builder writes for the migration, read
// back through JSON as the API server would return them.
func mtBuiltJobs(t *testing.T) []batchv1.Job {
	t.Helper()
	migration := mtMigration()
	coordination, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", migration.Namespace, migration.Spec.Target.CoordinationKey)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ptahv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: migration.Namespace, Name: "ptah-mplan-1", UID: types.UID("migration-plan-uid")},
		Spec: ptahv1alpha1.PtahMigrationPlanSpec{
			ContractVersion: migrationplan.ContractVersion, MigrationRef: ptahv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
			Fingerprint: builderDigest('4'), HistoryFingerprint: builderDigest('5'),
			ArtifactDigest: migration.Status.Artifact.Digest, CoordinationDigest: coordination,
			TargetIdentityDigest: builderDigest('6'),
			Migrations:           []ptahv1alpha1.PlannedMigration{{Version: 1, Checksum: "h1:0001"}},
		},
	}
	started := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	var jobs []batchv1.Job
	for _, operation := range []ptahv1alpha1.MigrationOperationType{
		ptahv1alpha1.MigrationOperationResolve, ptahv1alpha1.MigrationOperationVerify,
		ptahv1alpha1.MigrationOperationHistory, ptahv1alpha1.MigrationOperationApply,
	} {
		claim := ptahv1alpha1.MigrationOperationStatus{
			Type: operation, ID: builderDigest('1'), InputFingerprint: builderDigest('2'),
			ExecutionBindingID: migration.Status.ExecutionBinding.Epoch, StartedAt: started, Attempt: 1,
		}
		if operation != ptahv1alpha1.MigrationOperationResolve {
			claim.Source = &ptahv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: migration.Status.Artifact.ResolvedReference, Digest: migration.Status.Artifact.Digest,
				RegistryAuthFrom: migration.Spec.Artifact.RegistryAuthFrom.DeepCopy(), Transport: migration.Spec.Artifact.Transport,
			}
		}
		if operation == ptahv1alpha1.MigrationOperationHistory || operation == ptahv1alpha1.MigrationOperationApply {
			claim.CoordinationDigest = coordination
			claim.Target = &ptahv1alpha1.DatabaseTargetBinding{Engine: migration.Spec.Target.Engine, URLFrom: migration.Spec.Target.URLFrom}
		}
		var planned *ptahv1alpha1.PtahMigrationPlan
		if operation == ptahv1alpha1.MigrationOperationApply {
			claim.PlanRef = &ptahv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}
			dispatch, execution := metav1.NewTime(started.Add(120*time.Second)), metav1.NewTime(started.Add(300*time.Second))
			claim.DispatchNotAfter, claim.ExecutionNotAfter = &dispatch, &execution
			planned = plan
		}
		job, err := mtBuilder().BuildMigration(migration.DeepCopy(), claim, planned)
		if err != nil {
			t.Fatalf("BuildMigration(%s) error = %v", operation, err)
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
	return jobs
}

func mtIsolated(jobs []batchv1.Job) bool {
	return migrationJobIsolation(jobs, mtDatabaseSecret, mtRegistrySecret, mtExecutorImage, mtRunnerImage, "default")
}

// The Jobs the manager's builder writes have to pass, or the row fails a
// correct operator on the cluster; and the same Jobs read for another
// database Secret, registry Secret or image have to be refused.
func TestMigrationJobIsolationAcceptsTheBuildersJobs(t *testing.T) {
	t.Parallel()
	jobs := mtBuiltJobs(t)
	if !mtIsolated(jobs) {
		t.Fatal("the migration isolation predicate refused the builder's Jobs")
	}
	for name, isolated := range map[string]bool{
		"another database Secret": migrationJobIsolation(jobs, "another-db", mtRegistrySecret, mtExecutorImage, mtRunnerImage, "default"),
		"another registry Secret": migrationJobIsolation(jobs, mtDatabaseSecret, "another-registry", mtExecutorImage, mtRunnerImage, "default"),
		"another executor":        migrationJobIsolation(jobs, mtDatabaseSecret, mtRegistrySecret, "example.invalid/other", mtRunnerImage, "default"),
		"another runner":          migrationJobIsolation(jobs, mtDatabaseSecret, mtRegistrySecret, mtExecutorImage, "example.invalid/other", "default"),
		"another service account": migrationJobIsolation(jobs, mtDatabaseSecret, mtRegistrySecret, mtExecutorImage, mtRunnerImage, "migration-jobs"),
	} {
		if isolated {
			t.Errorf("the builder's Jobs passed for %s", name)
		}
	}
}

// mtJobMutation changes one Job of the builder's four, chosen by operation, or
// the list itself when operation is empty.
type mtJobMutation struct {
	name      string
	operation string
	mutate    func(jobs []batchv1.Job, job *batchv1.Job)
}

func mtJobOf(jobs []batchv1.Job, operation string) *batchv1.Job {
	for index := range jobs {
		if jobs[index].Labels[labelOperation] == operation {
			return &jobs[index]
		}
	}
	return nil
}

func mtContainer(job *batchv1.Job, name string) *corev1.Container {
	spec := &job.Spec.Template.Spec
	for index := range spec.Containers {
		if spec.Containers[index].Name == name {
			return &spec.Containers[index]
		}
	}
	for index := range spec.InitContainers {
		if spec.InitContainers[index].Name == name {
			return &spec.InitContainers[index]
		}
	}
	return nil
}

func mtWithoutEnv(container *corev1.Container, name string) {
	var kept []corev1.EnvVar
	for _, variable := range container.Env {
		if variable.Name != name {
			kept = append(kept, variable)
		}
	}
	container.Env = kept
}

// mtWithoutSecret drops every variable that reads the Secret.
func mtWithoutSecret(container *corev1.Container, secret string) {
	var kept []corev1.EnvVar
	for _, variable := range container.Env {
		if reference := envSecretRef(variable); reference == nil || reference.Name != secret {
			kept = append(kept, variable)
		}
	}
	container.Env = kept
}

func mtSetEnv(container *corev1.Container, variable corev1.EnvVar) {
	for index := range container.Env {
		if container.Env[index].Name == variable.Name {
			container.Env[index] = variable
			return
		}
	}
	container.Env = append(container.Env, variable)
}

func mtSecretVar(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
	}}}
}

// Every clause of the filter refuses the mistake it exists for. Each
// mutation changes one thing of the builder's Jobs, and the unchanged Jobs
// are accepted above.
func TestMigrationJobIsolationRefusesEveryBreach(t *testing.T) {
	t.Parallel()
	int32Pointer := func(value int32) *int32 { return &value }
	boolPointer := func(value bool) *bool { return &value }
	mutations := []mtJobMutation{
		{"no Job at all", "", func(jobs []batchv1.Job, _ *batchv1.Job) {}},
		{"a Job without an operation label", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			delete(job.Labels, labelOperation)
		}},
		{"a lifecycle without history", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Labels[labelOperation] = "apply"
		}},
		{"an unknown operation", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Labels[labelOperation] = "observe"
		}},
		{"a retry", "apply", func(_ []batchv1.Job, job *batchv1.Job) { job.Spec.BackoffLimit = int32Pointer(1) }},
		{"no backoff limit", "apply", func(_ []batchv1.Job, job *batchv1.Job) { job.Spec.BackoffLimit = nil }},
		{"a replacement Pod", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			policy := batchv1.TerminatingOrFailed
			job.Spec.PodReplacementPolicy = &policy
		}},
		{"a restarting Pod", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
		}},
		{"a mounted token", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.AutomountServiceAccountToken = boolPointer(true)
		}},
		{"an unset token mount", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.AutomountServiceAccountToken = nil
		}},
		{"service links", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.EnableServiceLinks = nil
		}},
		{"the host network", "history", func(_ []batchv1.Job, job *batchv1.Job) { job.Spec.Template.Spec.HostNetwork = true }},
		{"the host PID namespace", "history", func(_ []batchv1.Job, job *batchv1.Job) { job.Spec.Template.Spec.HostPID = true }},
		{"the host IPC namespace", "history", func(_ []batchv1.Job, job *batchv1.Job) { job.Spec.Template.Spec.HostIPC = true }},
		{"a shared process namespace", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.ShareProcessNamespace = boolPointer(true)
		}},
		{"another service account", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.ServiceAccountName = "migration-jobs"
		}},
		{"a weaker Pod security context", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.SecurityContext.RunAsNonRoot = boolPointer(false)
		}},
		{"an extra Pod security setting", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.SecurityContext.SupplementalGroups = []int64{0}
		}},
		{"no Pod security context", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.SecurityContext = nil
		}},
		{"an ephemeral container", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"},
			}}
		}},
		{"an envFrom on a source container", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "install-runner").EnvFrom = []corev1.EnvFromSource{{
				SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: mtDatabaseSecret}},
			}}
		}},
		{"an envFrom on the fetch container", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "fetch-migrations").EnvFrom = []corev1.EnvFromSource{{
				ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "anything"}},
			}}
		}},
		// Resolve and Verify.
		{"a second source container", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar"})
		}},
		{"no runner installer on a source Job", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.InitContainers = nil
		}},
		{"a source Job from another executor", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "ptah").Image = "example.invalid/other"
		}},
		{"a source Job with another runner", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "install-runner").Image = "example.invalid/other"
		}},
		{"a source Job without the registry Secret", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			mtWithoutSecret(mtContainer(job, "ptah"), mtRegistrySecret)
		}},
		{"a source Job over TLS", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			mtWithoutEnv(mtContainer(job, "ptah"), "PTAH_PLAIN_HTTP")
		}},
		{"a source Job told plain HTTP from a Secret", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), mtSecretVar("PTAH_PLAIN_HTTP", mtRegistrySecret, "plain"))
		}},
		{"a source Job holding the database URL", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), mtSecretVar("PTAH_DB_URL", mtDatabaseSecret, "url"))
		}},
		{"a source Job naming a dev database", "verify", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_DEV_URL", Value: "postgres://dev"})
		}},
		{"a source runner installer reading the database Secret", "resolve", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "install-runner"), mtSecretVar("ANYTHING", mtDatabaseSecret, "password"))
		}},
		// History and Apply.
		{"a database Job without the authority guard", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			spec := &job.Spec.Template.Spec
			spec.InitContainers = []corev1.Container{spec.InitContainers[0], spec.InitContainers[2]}
		}},
		{"a second database container", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar"})
		}},
		{"a database Job from another executor", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "ptah").Image = "example.invalid/other"
		}},
		{"a database Job with another runner", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "install-runner").Image = "example.invalid/other"
		}},
		{"a fetch from another image", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "fetch-migrations").Image = "example.invalid/other"
		}},
		{"a fetch running another program", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "fetch-migrations").Command = []string{"/bin/sh"}
		}},
		{"a fetch that pushes", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "fetch-migrations").Args[1] = "push"
		}},
		{"a fetch without arguments", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtContainer(job, "fetch-migrations").Args = nil
		}},
		{"no database URL", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtWithoutEnv(mtContainer(job, "ptah"), "PTAH_DB_URL")
		}},
		{"a literal database URL", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_DB_URL", Value: "postgres://literal"})
		}},
		{"the database URL from another Secret", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), mtSecretVar("PTAH_DB_URL", "another-db", "url"))
		}},
		{"the database URL from another key", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), mtSecretVar("PTAH_DB_URL", mtDatabaseSecret, "password"))
		}},
		{"the database URL from a ConfigMap too", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			variable := mtSecretVar("PTAH_DB_URL", mtDatabaseSecret, "url")
			variable.ValueFrom.ConfigMapKeyRef = &corev1.ConfigMapKeySelector{Key: "url"}
			mtSetEnv(mtContainer(job, "ptah"), variable)
		}},
		{"two database URLs", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			container.Env = append(container.Env, mtSecretVar("PTAH_DB_URL", mtDatabaseSecret, "url"))
		}},
		{"no migrations directory", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtWithoutEnv(mtContainer(job, "ptah"), "PTAH_MIGRATIONS_DIR")
		}},
		{"a relative migrations directory", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_MIGRATIONS_DIR", Value: "migrations"})
		}},
		{"a reference for a migrations directory", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_MIGRATIONS_DIR", Value: "/oci://registry/migrations"})
		}},
		{"a migrations directory from a Secret", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			variable := mtSecretVar("PTAH_MIGRATIONS_DIR", mtRegistrySecret, "dir")
			variable.Value = ""
			mtSetEnv(mtContainer(job, "ptah"), variable)
		}},
		{"the SQL process holding a registry variable", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_OCI_REGISTRY", Value: "registry"})
		}},
		{"the SQL process told plain HTTP", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_PLAIN_HTTP", Value: "true"})
		}},
		{"the SQL process holding a Docker config", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/docker"})
		}},
		{"the SQL process holding an operator grant", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), corev1.EnvVar{Name: "PTAH_OPERATOR_OCI_AUTH_MODE", Value: "Environment"})
		}},
		{"the SQL process reading the registry Secret", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "ptah"), mtSecretVar("ANYTHING", mtRegistrySecret, "password"))
		}},
		{"the SQL process mounting the Docker config", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "registry-docker-config", MountPath: "/docker"})
		}},
		{"the SQL process mounting the registry CA", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "registry-ca", MountPath: "/ca"})
		}},
		{"the SQL process mounting the CA snapshot", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "registry-ca-snapshot", MountPath: "/ca"})
		}},
		{"a fetch without the registry Secret", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtWithoutSecret(mtContainer(job, "fetch-migrations"), mtRegistrySecret)
		}},
		{"a fetch over TLS", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtWithoutEnv(mtContainer(job, "fetch-migrations"), "PTAH_PLAIN_HTTP")
		}},
		{"a fetch holding the database URL", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "fetch-migrations"), mtSecretVar("PTAH_DB_URL", mtDatabaseSecret, "url"))
		}},
		{"an authority guard reading the database Secret", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "validate-source-authority"), mtSecretVar("ANYTHING", mtDatabaseSecret, "url"))
		}},
		{"an authority guard naming a dev database", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			mtSetEnv(mtContainer(job, "validate-source-authority"), corev1.EnvVar{Name: "PTAH_DEV_URL", Value: "postgres://dev"})
		}},
		{"the artifact mounted writable", "apply", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			for index := range container.VolumeMounts {
				if container.VolumeMounts[index].Name == "schema-source" {
					container.VolumeMounts[index].ReadOnly = false
				}
			}
		}},
		{"the artifact mounted twice", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "schema-source", MountPath: "/again", ReadOnly: true})
		}},
		{"the artifact not mounted", "history", func(_ []batchv1.Job, job *batchv1.Job) {
			container := mtContainer(job, "ptah")
			var kept []corev1.VolumeMount
			for _, mount := range container.VolumeMounts {
				if mount.Name != "schema-source" {
					kept = append(kept, mount)
				}
			}
			container.VolumeMounts = kept
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			jobs := mtBuiltJobs(t)
			if mutation.name == "no Job at all" {
				jobs = nil
			} else {
				job := mtJobOf(jobs, mutation.operation)
				if job == nil {
					t.Fatalf("the fixture has no %s Job", mutation.operation)
				}
				mutation.mutate(jobs, job)
			}
			if mtIsolated(jobs) {
				t.Fatalf("a lifecycle with %s was accepted", mutation.name)
			}
		})
	}
}

// Any number of Jobs of each operation is read, as the archive of a whole
// lifecycle holds several: what the filter requires is that all four are
// among them and every one keeps the boundary.
func TestMigrationJobIsolationReadsEveryArchivedJob(t *testing.T) {
	t.Parallel()
	jobs := mtBuiltJobs(t)
	repeated := append(jobs, jobs...)
	if !mtIsolated(repeated) {
		t.Fatal("a lifecycle that ran every operation twice was refused")
	}
	breach := mtBuiltJobs(t)
	mtSetEnv(mtContainer(mtJobOf(breach, "apply"), "ptah"), corev1.EnvVar{Name: "PTAH_OCI_REGISTRY", Value: "registry"})
	if mtIsolated(append(jobs, breach...)) {
		t.Fatal("a breach in a later copy of the lifecycle was accepted")
	}
}
