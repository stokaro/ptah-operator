package workload

import (
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

// httpsFileSpec is a schema served as itself, behind a token, under a
// private CA.
func httpsFileSpec() *operatorv1alpha1.HTTPSSourceSpec {
	return &operatorv1alpha1.HTTPSSourceSpec{
		URL: "https://files.example.com/app/1.4.0/schema", Digest: digest('d'), Format: operatorv1alpha1.HTTPSFormatFile,
		Path:     "schema.hcl",
		AuthFrom: &operatorv1alpha1.HTTPSAuthSource{Name: "files-token"},
		Transport: operatorv1alpha1.HTTPSTransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "files-ca"}, Key: "ca.pem",
		}},
	}
}

// httpsArchiveSpec is a migration directory in a public tar.gz.
func httpsArchiveSpec() *operatorv1alpha1.HTTPSSourceSpec {
	return &operatorv1alpha1.HTTPSSourceSpec{
		URL: "https://files.example.com/app/1.4.0/app.tar.gz", Digest: digest('e'), Format: operatorv1alpha1.HTTPSFormatTarGzip,
		Path: "app-1.4.0/db/migrations",
	}
}

func httpsBindingOf(spec *operatorv1alpha1.HTTPSSourceSpec) *operatorv1alpha1.HTTPSSourceBinding {
	return &operatorv1alpha1.HTTPSSourceBinding{
		URL: spec.URL, Digest: spec.Digest, Format: spec.Format, Path: spec.Path, ContentDigest: digest('7'),
		AuthFrom: spec.AuthFrom.DeepCopy(), Transport: *spec.Transport.DeepCopy(),
	}
}

func httpsSchemaFixture(spec *operatorv1alpha1.HTTPSSourceSpec) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{HTTPS: spec}
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{HTTPS: &operatorv1alpha1.HTTPSSourceRevision{
		URL: spec.URL, Format: spec.Format, Path: spec.Path, Digest: spec.Digest, ContentDigest: digest('7'),
	}}
	return schema
}

// assertNoGit holds an HTTPS Job to carrying nothing of a Git source.
func assertNoGit(t *testing.T, job *batchv1.Job) {
	t.Helper()
	for _, volume := range []string{gitCredentialsVolumeName, gitCAVolumeName, gitKnownHostsVolumeName} {
		if volumeNamed(job, volume) != nil {
			t.Fatalf("an HTTPS-source Job carries the volume %s", volume)
		}
	}
	spec := job.Spec.Template.Spec
	for _, container := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, variable := range container.Env {
			if strings.HasPrefix(variable.Name, "PTAH_OPERATOR_GIT_") || strings.HasPrefix(variable.Name, "PTAH_GIT_") {
				t.Fatalf("%s carries the Git variable %s", container.Name, variable.Name)
			}
		}
	}
}

// assertHTTPSMountsReadOnly holds every credential and trust mount of a
// container to read-only, and requires at least one.
func assertHTTPSMountsReadOnly(t *testing.T, container corev1.Container) {
	t.Helper()
	mounted := 0
	for _, mount := range container.VolumeMounts {
		switch mount.Name {
		case httpsCredentialsVolumeName, httpsCAVolumeName:
			mounted++
			if !mount.ReadOnly {
				t.Errorf("%s mounts %s writable", container.Name, mount.Name)
			}
		}
	}
	if mounted == 0 {
		t.Errorf("%s mounts no HTTPS credential or trust", container.Name)
	}
}

func TestBuildResolvesAnHTTPSSourceInTheOperationContainer(t *testing.T) {
	t.Parallel()
	t.Run("a File behind a token under a CA", func(t *testing.T) {
		t.Parallel()
		job, err := builderFixture().Build(httpsSchemaFixture(httpsFileSpec()), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		assertNoRegistry(t, job)
		assertNoGit(t, job)
		if got := containerNames(job.Spec.Template.Spec.InitContainers); !slices.Equal(got, []string{initContainerName}) {
			t.Fatalf("init containers = %v, want only the runner install", got)
		}
		env := envMap(job)
		for name, want := range map[string]string{
			runner.EnvHTTPSURL: "https://files.example.com/app/1.4.0/schema", runner.EnvHTTPSDigest: digest('d'),
			runner.EnvHTTPSFormat: "File", runner.EnvHTTPSPath: "schema.hcl", runner.EnvHTTPSSelects: gitSelectsFile,
			runner.EnvHTTPSCASourceFile: httpsCAPath,
		} {
			if env[name].Value != want {
				t.Errorf("%s = %q, want %q", name, env[name].Value, want)
			}
		}
		for _, name := range []string{runner.EnvExpectedContentDigest, runner.EnvDatabaseURL, runner.EnvDatabaseURLFile, runner.EnvRequestedReference} {
			if _, present := env[name]; present {
				t.Errorf("a Resolve Job carries %s", name)
			}
		}
		for name, key := range map[string]string{
			runner.EnvHTTPSHostGrant:     operatorv1alpha1.HTTPSHostSecretKey,
			runner.EnvHTTPSCASHA256Grant: operatorv1alpha1.HTTPSCASHA256SecretKey,
		} {
			if secret, got, optional := secretKeyOf(env[name]); secret != "files-token" || got != key || optional {
				t.Errorf("%s reads %s/%s optional=%v, want files-token/%s required", name, secret, got, optional, key)
			}
		}
		// Basic or Bearer: any of the three keys may be missing, and the
		// runner refuses a combination that is neither.
		main := job.Spec.Template.Spec.Containers[0]
		assertCredentialFile(t, job, main, runner.EnvHTTPSUsernameFile, "files-token", operatorv1alpha1.HTTPSUsernameSecretKey, true)
		assertCredentialFile(t, job, main, runner.EnvHTTPSPasswordFile, "files-token", operatorv1alpha1.HTTPSPasswordSecretKey, true)
		assertCredentialFile(t, job, main, runner.EnvHTTPSTokenFile, "files-token", operatorv1alpha1.HTTPSTokenSecretKey, true)
		ca := volumeNamed(job, httpsCAVolumeName)
		if ca == nil || ca.ConfigMap == nil || ca.ConfigMap.Name != "files-ca" || ca.ConfigMap.Items[0].Key != "ca.pem" {
			t.Fatalf("CA volume = %#v, want the selected key of files-ca", ca)
		}
		assertHTTPSMountsReadOnly(t, main)
	})
	// The owner's CA pin reaches the runner even when the resource selects
	// no CA, which refuses the pairing; the kubelet leaves the variable out
	// when the Secret pins nothing.
	t.Run("a File behind a token on the system roots", func(t *testing.T) {
		t.Parallel()
		spec := httpsFileSpec()
		spec.Transport = operatorv1alpha1.HTTPSTransportSpec{}
		job, err := builderFixture().Build(httpsSchemaFixture(spec), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		env := envMap(job)
		for name, want := range map[string]struct {
			key      string
			optional bool
		}{
			runner.EnvHTTPSHostGrant:     {operatorv1alpha1.HTTPSHostSecretKey, false},
			runner.EnvHTTPSCASHA256Grant: {operatorv1alpha1.HTTPSCASHA256SecretKey, true},
		} {
			secret, key, optional := secretKeyOf(env[name])
			if secret != "files-token" || key != want.key || optional != want.optional {
				t.Errorf("%s reads %s/%s optional=%v, want files-token/%s optional=%v", name, secret, key, optional, want.key, want.optional)
			}
		}
		if _, present := env[runner.EnvHTTPSCASourceFile]; present {
			t.Errorf("a source with no CA selected carries %s", runner.EnvHTTPSCASourceFile)
		}
		if volumeNamed(job, httpsCAVolumeName) != nil {
			t.Fatal("a source with no CA selected mounts one")
		}
	})
	t.Run("a public archive of migrations", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: httpsArchiveSpec()}
		job, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil)
		if err != nil {
			t.Fatalf("BuildMigration() = %v", err)
		}
		assertNoRegistry(t, job)
		assertNoGit(t, job)
		env := envMap(job)
		if env[runner.EnvHTTPSFormat].Value != "TarGzip" || env[runner.EnvHTTPSSelects].Value != gitSelectsDirectory ||
			env[runner.EnvHTTPSPath].Value != "app-1.4.0/db/migrations" {
			t.Fatalf("environment = %#v", env)
		}
		for _, name := range []string{runner.EnvHTTPSHostGrant, runner.EnvHTTPSCASHA256Grant, runner.EnvHTTPSTokenFile, runner.EnvHTTPSCASourceFile} {
			if _, present := env[name]; present {
				t.Errorf("an anonymous Resolve carries %s", name)
			}
		}
		if volumeNamed(job, httpsCredentialsVolumeName) != nil || volumeNamed(job, httpsCAVolumeName) != nil {
			t.Fatal("an anonymous Resolve mounts a credential or a CA")
		}
	})
}

// assertHTTPSFetch holds a Job that reads an HTTPS source to its one shape:
// the runner, holding the credential, downloads the bound bytes into the
// source volume before the container that holds the database URL starts,
// which never sees the credential.
func assertHTTPSFetch(t *testing.T, job *batchv1.Job, destination, selects string, credentialed bool) {
	t.Helper()
	assertNoRegistry(t, job)
	assertNoGit(t, job)
	spec := job.Spec.Template.Spec
	if got := containerNames(spec.InitContainers); !slices.Equal(got, []string{initContainerName, httpsFetchContainerName}) {
		t.Fatalf("init containers = %v, want the runner install and the HTTPS fetch", got)
	}
	fetch := spec.InitContainers[1]
	wantArgs := []string{"--fetch-https-source", destination, "--ptah-binary", ptahBinaryPath}
	if fetch.Command[0] != runnerPath || !slices.Equal(fetch.Args, wantArgs) {
		t.Fatalf("the fetch runs %v %v, want the runner with %v", fetch.Command, fetch.Args, wantArgs)
	}
	env := containerEnv(fetch)
	for name, want := range map[string]string{
		runner.EnvExpectedContentDigest: digest('7'), runner.EnvHTTPSSelects: selects,
		"TMPDIR": fetchWorkPath, runner.EnvRunnerProtocolVersion: "1",
	} {
		if env[name].Value != want {
			t.Errorf("fetch %s = %q, want %q", name, env[name].Value, want)
		}
	}
	if !sha256Pattern.MatchString(env[runner.EnvHTTPSDigest].Value) {
		t.Errorf("the fetch carries no download digest: %q", env[runner.EnvHTTPSDigest].Value)
	}
	if _, present := env[runner.EnvHTTPSHostGrant]; present != credentialed {
		t.Errorf("the fetch carries a host grant = %v, want %v", present, credentialed)
	}
	if credentialed {
		assertHTTPSMountsReadOnly(t, fetch)
	}
	main := spec.Containers[0]
	for _, variable := range main.Env {
		if strings.HasPrefix(variable.Name, "PTAH_OPERATOR_HTTPS_") || strings.HasPrefix(variable.Name, "PTAH_HTTPS_") {
			t.Fatalf("the container holding the database URL carries %s", variable.Name)
		}
	}
	for _, mount := range main.VolumeMounts {
		switch mount.Name {
		case httpsCredentialsVolumeName, httpsCAVolumeName, fetchWorkVolumeName:
			t.Fatalf("the container holding the database URL mounts %s", mount.Name)
		case sourceVolumeName:
			if !mount.ReadOnly {
				t.Fatal("the container holding the database URL can write the fetched source")
			}
		}
	}
}

func TestBuildFetchesAnHTTPSSourceBeforeTheOperation(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			spec := httpsFileSpec()
			claim := operationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(spec)}
			job, err := builderFixture().Build(httpsSchemaFixture(spec), claim, nil)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			assertHTTPSFetch(t, job, schemaSnapshotPath, gitSelectsFile, true)
			if got := envMap(job)[runner.EnvSchemaFile].Value; got != "/source/schema/schema.hcl" {
				t.Fatalf("%s = %q, want the fetched file by its own name", runner.EnvSchemaFile, got)
			}
		})
	}
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationApply,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			spec := httpsArchiveSpec()
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: spec}
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(spec)}
			claim := migrationOperationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(spec)}
			var plan *operatorv1alpha1.PtahMigrationPlan
			if operation == operatorv1alpha1.MigrationOperationApply {
				plan = migrationPlanFixture()
			}
			job, err := builderFixture().BuildMigration(migration, claim, plan)
			if err != nil {
				t.Fatalf("BuildMigration() = %v", err)
			}
			assertHTTPSFetch(t, job, migrationsPath, gitSelectsDirectory, false)
		})
	}
}

// httpsPlanFixture is planFixture for a schema whose source is HTTPS:
// contract version 2, with the source bound in place of the artifact digest
// and the verification policy.
func httpsPlanFixture(schema *operatorv1alpha1.PtahSchema, builder Builder) *operatorv1alpha1.PtahSchemaPlan {
	plan := planFixture(schema, builder)
	https := schema.Status.Source.HTTPS
	source := sourcebinding.HTTPSPlanSource(https.URL, https.Path, https.ContentDigest)
	plan.Spec.ContractVersion = fingerprint.SourcePlanContractVersion
	plan.Spec.ArtifactDigest, plan.Spec.Source = "", source
	plan.Spec.VerificationPolicyUID, plan.Spec.VerificationPolicyDigest = "", ""
	current := schema.Status.Plan
	current.ArtifactDigest, current.Source = "", source.DeepCopy()
	current.VerificationPolicyUID, current.VerificationPolicyDigest = "", ""
	return plan
}

// The Apply that follows a plan computed from an HTTPS source reads the
// plan's bytes alone, so it downloads nothing and carries nothing of the
// source. A resolution for a digest the spec no longer names is not the
// source the plan may be applied from.
func TestBuildAppliesAPlanComputedFromAnHTTPSSource(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	schema := httpsSchemaFixture(httpsFileSpec())
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), httpsPlanFixture(schema, builder))
	if err != nil {
		t.Fatalf("Build() of an HTTPS source's Apply = %v", err)
	}
	assertNoRegistry(t, job)
	podSpec := job.Spec.Template.Spec
	for _, container := range append(slices.Clone(podSpec.InitContainers), podSpec.Containers...) {
		for _, variable := range container.Env {
			if strings.HasPrefix(variable.Name, "PTAH_OPERATOR_HTTPS_") || strings.HasPrefix(variable.Name, "PTAH_HTTPS_") {
				t.Errorf("the Apply container %s carries %s", container.Name, variable.Name)
			}
		}
	}
	for name, edit := range map[string]func(*operatorv1alpha1.HTTPSSourceSpec){
		"another digest": func(spec *operatorv1alpha1.HTTPSSourceSpec) { spec.Digest = digest('f') },
		"another URL":    func(spec *operatorv1alpha1.HTTPSSourceSpec) { spec.URL = "https://files.example.com/app/1.5.0/schema" },
		"another path":   func(spec *operatorv1alpha1.HTTPSSourceSpec) { spec.Path = "other.hcl" },
	} {
		moved := httpsSchemaFixture(httpsFileSpec())
		plan := httpsPlanFixture(moved, builder)
		edit(moved.Spec.Desired.HTTPS)
		if _, err := builder.Build(moved, operationFixture(operatorv1alpha1.OperationApply), plan); err == nil ||
			!strings.Contains(err.Error(), "resolved for another source") {
			t.Errorf("Build() of an Apply whose HTTPS resolution answers %s = %v, want the refusal naming it", name, err)
		}
	}
}

func TestBuildRefusesHTTPSJobsItCannotCarryOut(t *testing.T) {
	t.Parallel()
	if _, err := builderFixture().Build(httpsSchemaFixture(httpsFileSpec()),
		operationFixture(operatorv1alpha1.OperationVerify), nil); err == nil || !strings.Contains(err.Error(), "no Verify Job") {
		t.Errorf("Build() of an HTTPS source's Verify Job = %v, want the refusal naming it", err)
	}
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: httpsArchiveSpec()}
	if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationVerify), nil); err == nil ||
		!strings.Contains(err.Error(), "no Verify Job") {
		t.Errorf("BuildMigration() of an HTTPS source's Verify Job = %v, want the refusal naming it", err)
	}
	for name, mutate := range map[string]func(*operatorv1alpha1.SourceAccessBinding){
		"http://": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.URL = "http://files.example.com/app/1.4.0/schema"
		},
		"a user in the URL":       func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL = "https://bot@files.example.com/schema" },
		"no content digest":       func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.ContentDigest = "" },
		"no download digest":      func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Digest = "" },
		"a File at a nested path": func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Path = "db/schema.hcl" },
		"registry credentials": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{Name: "registry"}
		},
		"a Git commit beside it":  func(b *operatorv1alpha1.SourceAccessBinding) { b.Git = gitBindingOf(gitHTTPSSpec("db/schema.hcl")) },
		"an optional CA":          func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Transport.CAFrom.Optional = new(true) },
		"an unnamed credential":   func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.AuthFrom.Name = "" },
		"a format the API lacks":  func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Format = "Tar" },
		"a query in the download": func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL += "?token=1" },
		"a download with no path": func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL = "https://files.example.com/" },
		"an archive's escape path": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.Format, b.HTTPS.Path = operatorv1alpha1.HTTPSFormatZip, "../db"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := httpsFileSpec()
			claim := operationFixture(operatorv1alpha1.OperationObserve)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(spec)}
			mutate(claim.Source)
			if _, err := builderFixture().Build(httpsSchemaFixture(spec), claim, nil); err == nil {
				t.Fatalf("Build accepted %s", name)
			}
		})
	}
	optional := httpsArchiveSpec()
	optional.Transport.CAFrom = &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "files-ca"}, Key: "ca.pem", Optional: new(true),
	}
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: optional}
	if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil); err == nil {
		t.Error("BuildMigration built a Resolve Job for an optional CA")
	}
}
