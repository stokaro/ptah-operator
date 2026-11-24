package workload

import (
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// assertClassFile holds fileVariable in container to a file of a read-only
// CSI volume of the Secrets Store CSI driver that names class and nothing
// else, at the object name, and requires that no other container mounts it.
func assertClassFile(t *testing.T, job *batchv1.Job, container corev1.Container, fileVariable, volumeName, class, object string) {
	t.Helper()
	mount := requireMount(t, container, volumeName)
	if !mount.ReadOnly {
		t.Fatalf("container %s mounts %s writable", container.Name, volumeName)
	}
	if got, want := containerEnvMap(container)[fileVariable].Value, mount.MountPath+"/"+object; got != want {
		t.Fatalf("%s = %q, want %q", fileVariable, got, want)
	}
	volume := volumeNamed(job, volumeName)
	want := corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver: SecretsStoreCSIDriver, ReadOnly: new(true),
		VolumeAttributes: map[string]string{SecretProviderClassVolumeAttribute: class},
	}}
	if volume == nil || !reflect.DeepEqual(volume.VolumeSource, want) {
		t.Fatalf("volume %s = %#v, want only the class %s, read-only, through %s", volumeName, volume, class, SecretsStoreCSIDriver)
	}
	pod := job.Spec.Template.Spec
	for _, other := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if other.Name != container.Name && hasMount(other, volumeName) {
			t.Fatalf("container %s also mounts %s", other.Name, volumeName)
		}
	}
}

func classSelector(name, object string) *operatorv1alpha1.SecretProviderClassObjectSelector {
	return &operatorv1alpha1.SecretProviderClassObjectSelector{Name: name, ObjectName: object}
}

// TestBuildMountsAURLFromASecretProviderClass builds every schema operation
// that reads a database with a target and a development database whose URLs
// come from SecretProviderClass objects, and holds each Job to the volumes
// that deliver them.
func TestBuildMountsAURLFromASecretProviderClass(t *testing.T) {
	t.Parallel()
	builder := builderFixture()

	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Target.URLFrom = nil
			schema.Spec.Target.URLFromSecretProviderClass = classSelector("orders-store", "url")
			schema.Spec.Dev = &operatorv1alpha1.DatabaseTargetRef{URLFromSecretProviderClass: classSelector("orders-development-store", ".dev-url")}
			claim := operationFixture(operation)
			claim.Target = &operatorv1alpha1.DatabaseTargetBinding{
				Engine: schema.Spec.Target.Engine, URLFromSecretProviderClass: schema.Spec.Target.URLFromSecretProviderClass.DeepCopy(),
			}
			claim.ObservationDev = schema.Spec.Dev.DeepCopy()
			job, err := builder.Build(schema, claim, nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			main := job.Spec.Template.Spec.Containers[0]
			assertClassFile(t, job, main, runner.EnvDatabaseURLFile, databaseURLVolumeName, "orders-store", "url")
			if operation == operatorv1alpha1.OperationPlan {
				assertClassFile(t, job, main, runner.EnvDevelopmentDatabaseURLFile, developmentDatabaseURLVolumeName,
					"orders-development-store", ".dev-url")
			} else if volumeNamed(job, developmentDatabaseURLVolumeName) != nil {
				t.Fatal("an Observe mounts the development database")
			}
			for _, volume := range job.Spec.Template.Spec.Volumes {
				if volume.Secret != nil && strings.Contains(volume.Name, "database-url") {
					t.Fatalf("volume %s still reads a Secret", volume.Name)
				}
			}
		})
	}

	t.Run("Apply", func(t *testing.T) {
		t.Parallel()
		schema := schemaFixture()
		claim := operationFixture(operatorv1alpha1.OperationApply)
		claim.Target.URLFrom = nil
		claim.Target.URLFromSecretProviderClass = classSelector("orders-store", "url")
		job, err := builder.Build(schema, claim, planFixture(schema, builder))
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		// The claim's binding, not the spec's Secret, decides the mount.
		assertClassFile(t, job, job.Spec.Template.Spec.Containers[0], runner.EnvDatabaseURLFile, databaseURLVolumeName, "orders-store", "url")
	})

	// A development database keeps its Secret when only the target moved.
	t.Run("a Secret development database beside a class target", func(t *testing.T) {
		t.Parallel()
		schema := schemaFixture()
		claim := operationFixture(operatorv1alpha1.OperationPlan)
		claim.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine: schema.Spec.Target.Engine, URLFromSecretProviderClass: classSelector("orders-store", "url"),
		}
		claim.ObservationDev = &operatorv1alpha1.DatabaseTargetRef{URLFrom: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "development"}, Key: "dsn",
		}}
		job, err := builder.Build(schema, claim, nil)
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		main := job.Spec.Template.Spec.Containers[0]
		assertClassFile(t, job, main, runner.EnvDatabaseURLFile, databaseURLVolumeName, "orders-store", "url")
		assertCredentialFile(t, job, main, runner.EnvDevelopmentDatabaseURLFile, "development", "dsn", false)
	})
}

// TestBuildRefusesATargetBindingThatNamesNoOnePlace holds the builder to the
// binding: one that names both places, or neither, builds no Job.
func TestBuildRefusesATargetBindingThatNamesNoOnePlace(t *testing.T) {
	t.Parallel()
	builder := builderFixture()

	for name, binding := range map[string]*operatorv1alpha1.DatabaseTargetBinding{
		"both": {
			Engine:                     operatorv1alpha1.DatabaseEnginePostgreSQL,
			URLFrom:                    &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			URLFromSecretProviderClass: classSelector("orders-store", "url"),
		},
		"neither":           {Engine: operatorv1alpha1.DatabaseEnginePostgreSQL},
		"an unnamed object": {Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFromSecretProviderClass: classSelector("orders-store", "")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			claim := operationFixture(operatorv1alpha1.OperationPlan)
			claim.Target = binding
			job, err := builder.Build(schemaFixture(), claim, nil)
			if err == nil || !strings.Contains(err.Error(), "requires an immutable target URL binding") {
				t.Fatalf("Build() = %v, %v, want a refusal of the binding", job, err)
			}
		})
	}
}

// TestBuildMigrationMountsTargetAndShadowFromSecretProviderClasses builds the
// history read and the Apply of a drift-checked migration whose target and
// shadow URLs come from SecretProviderClass objects.
func TestBuildMigrationMountsTargetAndShadowFromSecretProviderClasses(t *testing.T) {
	t.Parallel()
	builder := builderFixture()

	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationApply,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Target.URLFrom = nil
			migration.Spec.Target.URLFromSecretProviderClass = classSelector("ledger-store", "url")
			migration.Spec.Policy.DriftCheck = &operatorv1alpha1.MigrationDriftCheck{Shadow: operatorv1alpha1.DatabaseTargetRef{
				URLFromSecretProviderClass: classSelector("ledger-store", "shadow-url"),
			}}
			claim := migrationOperationFixture(operation)
			claim.Target = &operatorv1alpha1.DatabaseTargetBinding{
				Engine: migration.Spec.Target.Engine, URLFromSecretProviderClass: migration.Spec.Target.URLFromSecretProviderClass.DeepCopy(),
			}
			plan := migrationPlanFixture()
			plan.Spec.ShadowIdentityDigest = digest('5')
			job, err := builder.BuildMigration(migration, claim, plan)
			if err != nil {
				t.Fatalf("BuildMigration() error = %v", err)
			}
			main := job.Spec.Template.Spec.Containers[0]
			assertClassFile(t, job, main, runner.EnvDatabaseURLFile, databaseURLVolumeName, "ledger-store", "url")
			assertClassFile(t, job, main, runner.EnvShadowDatabaseURLFile, shadowDatabaseURLVolumeName, "ledger-store", "shadow-url")
		})
	}
}
