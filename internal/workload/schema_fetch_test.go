package workload

import (
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestASchemaFetchHoldsWhatAMigrationFetchHolds builds the two fetches over one
// registry with every way a Job can reach it. A migration's fetch is Ptah, as a
// schema's was, so it is the record of what a fetch holds. The runner around the
// schema's pull adds its own command and arguments, the runner's volume read-only
// and the protocol, and not one credential, Secret or mount more.
func TestASchemaFetchHoldsWhatAMigrationFetchHolds(t *testing.T) {
	t.Parallel()

	registryCA := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "registry-ca"}, Key: "ca.pem"}
	for _, row := range []struct {
		name string
		mode operatorv1alpha1.RegistryAuthMode
		ca   *corev1.ConfigMapKeySelector
	}{
		{name: "a password or token in the environment", mode: operatorv1alpha1.RegistryAuthEnvironment},
		{name: "a Docker config", mode: operatorv1alpha1.RegistryAuthDockerConfigJSON},
		{name: "a private CA", mode: operatorv1alpha1.RegistryAuthEnvironment, ca: registryCA},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			auth := &operatorv1alpha1.RegistryAuthSource{Name: "registry", Mode: row.mode, UsernameKey: "user", PasswordKey: "pass", TokenKey: "identity-token"}

			schema := schemaFixture()
			schema.Spec.Desired.RegistryAuthFrom = auth
			schema.Spec.Desired.Transport.CAFrom = row.ca
			read := operationFixture(operatorv1alpha1.OperationObserve)
			read.Source.RegistryAuthFrom = auth
			read.Source.Transport.CAFrom = row.ca
			schemaJob, err := builderFixture().Build(schema, read, nil)
			if err != nil {
				t.Fatal(err)
			}
			fetch := requireContainer(t, schemaJob.Spec.Template.Spec.InitContainers, SchemaFetchContainerName)

			migration := migrationFixture()
			migration.Spec.Artifact.RegistryAuthFrom = auth
			migration.Spec.Artifact.Transport.CAFrom = row.ca
			history := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
			history.Source.RegistryAuthFrom = auth
			history.Source.Transport.CAFrom = row.ca
			migrationJob, err := builderFixture().BuildMigration(migration, history, nil)
			if err != nil {
				t.Fatal(err)
			}
			ptah := requireContainer(t, migrationJob.Spec.Template.Spec.InitContainers, migrationFetchContainerName)

			environment := slices.DeleteFunc(slices.Clone(fetch.Env), func(variable corev1.EnvVar) bool {
				return variable.Name == runner.EnvRunnerProtocolVersion
			})
			if len(environment) != len(fetch.Env)-1 {
				t.Fatalf("the runner around the pull is not told the protocol: %#v", fetch.Env)
			}
			if !reflect.DeepEqual(environment, ptah.Env) {
				t.Errorf("the schema fetch holds %#v\nand a fetch that is Ptah holds %#v", environment, ptah.Env)
			}
			mounts := slices.DeleteFunc(slices.Clone(fetch.VolumeMounts), func(mount corev1.VolumeMount) bool {
				return mount.Name == runnerVolumeName
			})
			if !reflect.DeepEqual(mounts, ptah.VolumeMounts) {
				t.Errorf("the schema fetch mounts %#v\nand a fetch that is Ptah mounts %#v", mounts, ptah.VolumeMounts)
			}
			runnerMount := requireMount(t, fetch, runnerVolumeName)
			if !runnerMount.ReadOnly || runnerMount.MountPath != "/runner" {
				t.Errorf("the runner's volume = %#v, want it read-only at /runner", runnerMount)
			}
			if fetch.TerminationMessagePolicy != corev1.TerminationMessageReadFile || fetch.TerminationMessagePath != runner.TerminationMessagePath {
				t.Errorf("the termination message = %q at %q: anything but the file the runner writes puts the log in status",
					fetch.TerminationMessagePolicy, fetch.TerminationMessagePath)
			}
		})
	}
}
