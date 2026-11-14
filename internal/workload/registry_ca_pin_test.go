package workload

import (
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A registry Secret's caSHA256 binds whatever the resource selects. With a CA
// selected the grant is required, so the Pod does not start without it. With
// none it is optional: the kubelet leaves it out when the Secret pins nothing,
// and hands it to the runner when the Secret does, which refuses the system
// roots before Ptah runs. It reaches only the container that checks the
// access, never the fetch that holds the credential, and plain HTTP, which
// makes no TLS connection, carries no pin.
func TestBuildCarriesTheRegistryCAPinWhateverTheSourceSelects(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	systemRoots := func(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) operatorv1alpha1.ActiveOperationStatus {
		schema.Spec.Desired.Transport = operatorv1alpha1.OCITransportSpec{}
		if operation.Source != nil {
			operation.Source.Transport = operatorv1alpha1.OCITransportSpec{}
		}
		return operation
	}
	dockerConfig := func(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) operatorv1alpha1.ActiveOperationStatus {
		schema.Spec.Desired.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
			Name: "registry-docker", Mode: operatorv1alpha1.RegistryAuthDockerConfigJSON,
		}
		if operation.Source != nil {
			operation.Source.RegistryAuthFrom = schema.Spec.Desired.RegistryAuthFrom.DeepCopy()
		}
		return operation
	}
	schemaJob := func(operationType operatorv1alpha1.OperationType, edits ...func(*operatorv1alpha1.PtahSchema, operatorv1alpha1.ActiveOperationStatus) operatorv1alpha1.ActiveOperationStatus) func() (*batchv1.Job, error) {
		return func() (*batchv1.Job, error) {
			schema := schemaFixture()
			operation := operationFixture(operationType)
			for _, edit := range edits {
				operation = edit(schema, operation)
			}
			return builder.Build(schema, operation, nil)
		}
	}
	migrationJob := func(operationType operatorv1alpha1.MigrationOperationType, caSelected bool) func() (*batchv1.Job, error) {
		return func() (*batchv1.Job, error) {
			migration := migrationFixture()
			operation := migrationOperationFixture(operationType)
			if caSelected {
				migration.Spec.Artifact.Transport.CAFrom = goldenRegistryCA()
				if operation.Source != nil {
					operation.Source.Transport.CAFrom = goldenRegistryCA()
				}
			}
			var plan *operatorv1alpha1.PtahMigrationPlan
			if operationType == operatorv1alpha1.MigrationOperationApply {
				plan = migrationPlanFixture()
			}
			return builder.BuildMigration(migration, operation, plan)
		}
	}

	for _, test := range []struct {
		name  string
		build func() (*batchv1.Job, error)
		// holder is the one container the grant reaches, empty for none.
		holder     string
		secret     string
		optional   bool
		systemRoot bool
		// credentialed is a row with no holder that still sends a
		// credential, so the grant's absence is the transport's doing.
		credentialed bool
	}{
		{name: "schema Resolve on the system roots", build: schemaJob(operatorv1alpha1.OperationResolve, systemRoots),
			holder: mainContainerName, secret: "registry", optional: true, systemRoot: true},
		{name: "schema Verify with a Docker config on the system roots", build: schemaJob(operatorv1alpha1.OperationVerify, systemRoots, dockerConfig),
			holder: mainContainerName, secret: "registry-docker", optional: true, systemRoot: true},
		{name: "schema Observe on the system roots", build: schemaJob(operatorv1alpha1.OperationObserve, systemRoots),
			holder: guardContainerName, secret: "registry", optional: true, systemRoot: true},
		{name: "schema Plan with a Docker config on the system roots", build: schemaJob(operatorv1alpha1.OperationPlan, systemRoots, dockerConfig),
			holder: guardContainerName, secret: "registry-docker", optional: true, systemRoot: true},
		{name: "migration Resolve on the system roots", build: migrationJob(operatorv1alpha1.MigrationOperationResolve, false),
			holder: mainContainerName, secret: "registry", optional: true, systemRoot: true},
		{name: "migration History on the system roots", build: migrationJob(operatorv1alpha1.MigrationOperationHistory, false),
			holder: guardContainerName, secret: "registry", optional: true, systemRoot: true},
		{name: "migration Apply on the system roots", build: migrationJob(operatorv1alpha1.MigrationOperationApply, false),
			holder: guardContainerName, secret: "registry", optional: true, systemRoot: true},
		{name: "schema Resolve with a CA", build: schemaJob(operatorv1alpha1.OperationResolve),
			holder: mainContainerName, secret: "registry"},
		{name: "schema Observe with a CA", build: schemaJob(operatorv1alpha1.OperationObserve),
			holder: guardContainerName, secret: "registry"},
		{name: "migration History with a CA", build: migrationJob(operatorv1alpha1.MigrationOperationHistory, true),
			holder: guardContainerName, secret: "registry"},
		{name: "schema Resolve over plain HTTP", build: func() (*batchv1.Job, error) {
			return builder.Build(sourceContractSchemaFixture(operatorv1alpha1.RegistryAuthEnvironment), operationFixture(operatorv1alpha1.OperationResolve), nil)
		}, credentialed: true},
		{name: "schema Plan over plain HTTP", build: func() (*batchv1.Job, error) {
			schema := sourceContractSchemaFixture(operatorv1alpha1.RegistryAuthDockerConfigJSON)
			operation := operationFixture(operatorv1alpha1.OperationPlan)
			operation.Source = &operatorv1alpha1.SourceAccessBinding{
				ResolvedReference: schema.Status.Source.ResolvedReference,
				Digest:            schema.Status.Source.Digest,
				RegistryAuthFrom:  schema.Spec.Desired.RegistryAuthFrom.DeepCopy(),
				Transport:         *schema.Spec.Desired.Transport.DeepCopy(),
			}
			return builder.Build(schema, operation, nil)
		}, credentialed: true},
		{name: "anonymous schema Resolve on the system roots", build: func() (*batchv1.Job, error) {
			schema := schemaFixture()
			schema.Spec.Desired.RegistryAuthFrom = nil
			schema.Spec.Desired.Transport = operatorv1alpha1.OCITransportSpec{}
			return builder.Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job, err := test.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			holders := map[string]corev1.EnvVar{}
			var containers []string
			credentialed := false
			template := job.Spec.Template.Spec
			for _, container := range slices.Concat(template.InitContainers, template.Containers) {
				containers = append(containers, container.Name)
				environment := containerEnvMap(container)
				if variable, present := environment[runner.EnvOCICASHA256Grant]; present {
					holders[container.Name] = variable
				}
				if _, present := environment[runner.EnvOCIAuthRegistryGrant]; present {
					credentialed = true
				}
			}
			if test.holder == "" {
				if credentialed != test.credentialed {
					t.Fatalf("the Job sends a registry credential: %v, want %v", credentialed, test.credentialed)
				}
				if len(holders) != 0 {
					t.Fatalf("%s reaches %v, want no container", runner.EnvOCICASHA256Grant, holders)
				}
				return
			}
			if !slices.Contains(containers, test.holder) {
				t.Fatalf("the Job has no %s container among %q", test.holder, containers)
			}
			if len(holders) != 1 {
				t.Fatalf("%s reaches %d containers (%v), want %s alone", runner.EnvOCICASHA256Grant, len(holders), holders, test.holder)
			}
			variable, present := holders[test.holder]
			if !present {
				t.Fatalf("%s reaches %v, want %s", runner.EnvOCICASHA256Grant, holders, test.holder)
			}
			secret, key, optional := secretKeyOf(variable)
			if variable.Value != "" || secret != test.secret || key != operatorv1alpha1.RegistryCASHA256SecretKey || optional != test.optional {
				t.Fatalf("%s reads %s/%s optional=%v value=%q, want %s/%s optional=%v",
					runner.EnvOCICASHA256Grant, secret, key, optional, variable.Value, test.secret, operatorv1alpha1.RegistryCASHA256SecretKey, test.optional)
			}
			if !test.systemRoot {
				return
			}
			// What the runner is handed is the pairing it refuses: a pin and
			// no CA to hold it to.
			holder := requireContainer(t, slices.Concat(template.InitContainers, template.Containers), test.holder)
			environment := containerEnvMap(holder)
			for _, name := range []string{runner.EnvOCIHasCA, runner.EnvOCICASourceFile} {
				if _, present := environment[name]; present {
					t.Errorf("a source on the system roots carries %s", name)
				}
			}
			if slices.Contains(holder.Args, "--snapshot-oci-ca-to") || volumeNamed(job, caVolumeName) != nil {
				t.Errorf("a source on the system roots snapshots a CA: args %q", holder.Args)
			}
		})
	}
}
