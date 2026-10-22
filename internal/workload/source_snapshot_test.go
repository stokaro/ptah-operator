package workload

import (
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const snapshotName = "ptah-src-0123456789abcdef01234567"

func snapshotBinding(schemaFile string) *operatorv1alpha1.SourceSnapshotBinding {
	return &operatorv1alpha1.SourceSnapshotBinding{
		Name: snapshotName, UID: "snapshot-uid", ContentDigest: digest('7'), SchemaFile: schemaFile,
	}
}

// configMapSchemaFixture is schemaFixture with its source moved to a
// ConfigMap that was read into a snapshot.
func configMapSchemaFixture() *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
		Name:  "orders-schema",
		Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "db/schema.sql"}},
	}}
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "orders-schema", UID: "configmap-uid", ResourceVersion: "4"},
		Snapshot:  snapshotBinding("db/schema.sql"),
	}
	return schema
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for index, container := range containers {
		names[index] = container.Name
	}
	return names
}

func volumeNamed(job *batchv1.Job, name string) *corev1.Volume {
	for index := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[index].Name == name {
			return &job.Spec.Template.Spec.Volumes[index]
		}
	}
	return nil
}

// assertSnapshotCopy holds a Job that reads a ConfigMap source to the one
// shape it may have: the runner checks the snapshot into the source volume
// before the container that holds the database URL starts, and nothing of a
// registry is anywhere in the Pod.
func assertSnapshotCopy(t *testing.T, job *batchv1.Job, destination string) {
	t.Helper()
	spec := job.Spec.Template.Spec
	if got := containerNames(spec.InitContainers); !slices.Equal(got, []string{initContainerName, snapshotContainerName}) {
		t.Fatalf("init containers = %v, want the runner install and the snapshot copy", got)
	}
	copyContainer := spec.InitContainers[1]
	wantArgs := []string{"--materialize-source-snapshot", snapshotPath, "--materialize-to", destination, "--content-digest", digest('7')}
	if copyContainer.Command[0] != runnerPath || !slices.Equal(copyContainer.Args, wantArgs) {
		t.Fatalf("snapshot copy runs %v %v, want the runner with %v", copyContainer.Command, copyContainer.Args, wantArgs)
	}
	if len(copyContainer.Env) != 1 || copyContainer.Env[0].Name != runner.EnvRunnerProtocolVersion {
		t.Fatalf("snapshot copy environment = %#v, want only the runner protocol", copyContainer.Env)
	}
	snapshot := volumeNamed(job, snapshotVolumeName)
	if snapshot == nil || snapshot.ConfigMap == nil || snapshot.ConfigMap.Name != snapshotName || len(snapshot.ConfigMap.Items) != 0 {
		t.Fatalf("snapshot volume = %#v, want the whole snapshot ConfigMap", snapshot)
	}
	for _, mount := range copyContainer.VolumeMounts {
		if mount.Name == snapshotVolumeName && !mount.ReadOnly {
			t.Fatal("the snapshot is mounted writable")
		}
	}
	main := spec.Containers[0]
	for _, mount := range main.VolumeMounts {
		if mount.Name == snapshotVolumeName {
			t.Fatal("the container holding the database URL mounts the unchecked snapshot")
		}
		if mount.Name == sourceVolumeName && !mount.ReadOnly {
			t.Fatal("the container holding the database URL can write the checked source")
		}
	}
	for _, volume := range []string{fetchWorkVolumeName, dockerVolumeName, caVolumeName, caSnapshotVolumeName, policyVolumeName} {
		if volumeNamed(job, volume) != nil {
			t.Fatalf("a ConfigMap-source Job carries the registry volume %s", volume)
		}
	}
	for _, container := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, variable := range container.Env {
			if strings.HasPrefix(variable.Name, "PTAH_OCI_") || variable.Name == "PTAH_PLAIN_HTTP" {
				t.Fatalf("%s carries the registry variable %s", container.Name, variable.Name)
			}
		}
	}
}

func TestBuildReadsAConfigMapSourceThroughItsCheckedSnapshot(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema := configMapSchemaFixture()
			claim := operationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshotBinding("db/schema.sql")}
			job, err := builderFixture().Build(schema, claim, nil)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			assertSnapshotCopy(t, job, schemaSnapshotPath)
			if got := envMap(job)[runner.EnvSchemaFile].Value; got != "/source/schema/db/schema.sql" {
				t.Fatalf("%s = %q, want the file the snapshot holds", runner.EnvSchemaFile, got)
			}
		})
	}
}

func TestBuildRefusesSnapshotJobsItCannotCarryOut(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*operatorv1alpha1.PtahSchema, *operatorv1alpha1.ActiveOperationStatus){
		"a Resolve Job": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			*claim = operationFixture(operatorv1alpha1.OperationResolve)
		},
		"a Verify Job": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			*claim = operationFixture(operatorv1alpha1.OperationVerify)
		},
		"a snapshot naming no schema file": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Snapshot.SchemaFile = ""
		},
		"a snapshot that is not a snapshot": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Snapshot.Name = "orders-schema"
		},
		"a snapshot and registry credentials": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{Name: "registry"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := configMapSchemaFixture()
			claim := operationFixture(operatorv1alpha1.OperationObserve)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshotBinding("db/schema.sql")}
			mutate(schema, &claim)
			if _, err := builderFixture().Build(schema, claim, nil); err == nil {
				t.Fatalf("Build accepted %s", name)
			}
		})
	}
}

func TestBuildMigrationReadsAConfigMapSourceThroughItsCheckedSnapshot(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationApply,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
				Name:  "orders-migrations",
				Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "0001.up.sql", Path: "0001_init.up.sql"}},
			}}
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshotBinding("")}
			claim := migrationOperationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshotBinding("")}
			var plan *operatorv1alpha1.PtahMigrationPlan
			if operation == operatorv1alpha1.MigrationOperationApply {
				plan = migrationPlanFixture()
			}
			job, err := builderFixture().BuildMigration(migration, claim, plan)
			if err != nil {
				t.Fatalf("BuildMigration() = %v", err)
			}
			assertSnapshotCopy(t, job, migrationsPath)
			if got := envMap(job)[runner.EnvMigrationsDir].Value; got != migrationsPath {
				t.Fatalf("%s = %q, want %q", runner.EnvMigrationsDir, got, migrationsPath)
			}
		})
	}
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{Name: "orders-migrations"}}
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationVerify,
	} {
		if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operation), nil); err == nil {
			t.Errorf("BuildMigration built a %s Job for a ConfigMap source", operation)
		}
	}
	claim := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
	claim.Source = &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshotBinding("schema.hcl")}
	if _, err := builderFixture().BuildMigration(migration, claim, nil); err == nil {
		t.Error("BuildMigration accepted a migration snapshot that names a schema file")
	}
}
