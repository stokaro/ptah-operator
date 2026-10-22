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

const gitCommit = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"

func gitHTTPSSpec(path string) *operatorv1alpha1.GitSourceSpec {
	return &operatorv1alpha1.GitSourceSpec{
		URL: "https://git.example.com/acme/app.git", Ref: operatorv1alpha1.GitRef{Branch: "main"}, Path: path,
		AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "git-token"},
		Transport: operatorv1alpha1.GitTransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "git-ca"}, Key: "ca.pem",
		}},
	}
}

func gitSSHSpec(path string) *operatorv1alpha1.GitSourceSpec {
	return &operatorv1alpha1.GitSourceSpec{
		URL: "ssh://git@git.example.com:2222/acme/app.git", Ref: operatorv1alpha1.GitRef{Tag: "v1.4.0"}, Path: path,
		AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "git-deploy-key"},
		Transport: operatorv1alpha1.GitTransportSpec{KnownHostsFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "git-hosts"}, Key: "known_hosts",
		}},
	}
}

func gitBindingOf(spec *operatorv1alpha1.GitSourceSpec) *operatorv1alpha1.GitSourceBinding {
	return &operatorv1alpha1.GitSourceBinding{
		URL: spec.URL, Ref: spec.Ref, Commit: gitCommit, Path: spec.Path, ContentDigest: digest('7'),
		AuthFrom: spec.AuthFrom.DeepCopy(), Transport: *spec.Transport.DeepCopy(),
	}
}

func gitSchemaFixture(spec *operatorv1alpha1.GitSourceSpec) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{Git: spec}
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
		URL: spec.URL, Ref: spec.Ref, Path: spec.Path, Commit: gitCommit, ContentDigest: digest('7'),
	}}
	return schema
}

func containerEnv(container corev1.Container) map[string]corev1.EnvVar {
	result := make(map[string]corev1.EnvVar, len(container.Env))
	for _, variable := range container.Env {
		result[variable.Name] = variable
	}
	return result
}

// secretKeyOf reports the Secret and key a variable reads, and whether it may
// be absent.
func secretKeyOf(variable corev1.EnvVar) (string, string, bool) {
	if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
		return "", "", false
	}
	ref := variable.ValueFrom.SecretKeyRef
	return ref.Name, ref.Key, ref.Optional != nil && *ref.Optional
}

// assertNoRegistry holds a Git Job to carrying nothing of a registry.
func assertNoRegistry(t *testing.T, job *batchv1.Job) {
	t.Helper()
	for _, volume := range []string{dockerVolumeName, caVolumeName, caSnapshotVolumeName, policyVolumeName, snapshotVolumeName} {
		if volumeNamed(job, volume) != nil {
			t.Fatalf("a Git-source Job carries the volume %s", volume)
		}
	}
	spec := job.Spec.Template.Spec
	for _, container := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, variable := range container.Env {
			if strings.HasPrefix(variable.Name, "PTAH_OCI_") || variable.Name == "PTAH_PLAIN_HTTP" ||
				variable.Name == runner.EnvRequestedReference {
				t.Fatalf("%s carries the registry variable %s", container.Name, variable.Name)
			}
		}
	}
}

func TestBuildResolvesAGitSourceInTheOperationContainer(t *testing.T) {
	t.Parallel()
	t.Run("https with a token and a CA", func(t *testing.T) {
		t.Parallel()
		job, err := builderFixture().Build(gitSchemaFixture(gitHTTPSSpec("db/schema.hcl")), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		assertNoRegistry(t, job)
		if got := containerNames(job.Spec.Template.Spec.InitContainers); !slices.Equal(got, []string{initContainerName}) {
			t.Fatalf("init containers = %v, want only the runner install", got)
		}
		env := envMap(job)
		for name, want := range map[string]string{
			runner.EnvGitURL: "https://git.example.com/acme/app.git", runner.EnvGitBranch: "main",
			runner.EnvGitPath: "db/schema.hcl", runner.EnvGitSelects: gitSelectsFile, runner.EnvGitCASourceFile: gitCAPath,
		} {
			if env[name].Value != want {
				t.Errorf("%s = %q, want %q", name, env[name].Value, want)
			}
		}
		for _, name := range []string{runner.EnvGitCommit, runner.EnvExpectedContentDigest, runner.EnvDatabaseURL, runner.EnvGitSSHKeyFile} {
			if _, present := env[name]; present {
				t.Errorf("a Resolve Job carries %s", name)
			}
		}
		for name, want := range map[string]struct {
			key      string
			optional bool
		}{
			runner.EnvGitHostGrant:     {operatorv1alpha1.GitHostSecretKey, false},
			runner.EnvGitCASHA256Grant: {operatorv1alpha1.GitCASHA256SecretKey, false},
			runner.EnvGitUsername:      {operatorv1alpha1.GitUsernameSecretKey, true},
			runner.EnvGitPassword:      {operatorv1alpha1.GitPasswordSecretKey, true},
		} {
			secret, key, optional := secretKeyOf(env[name])
			if secret != "git-token" || key != want.key || optional != want.optional {
				t.Errorf("%s reads %s/%s optional=%v, want git-token/%s optional=%v", name, secret, key, optional, want.key, want.optional)
			}
		}
		ca := volumeNamed(job, gitCAVolumeName)
		if ca == nil || ca.ConfigMap == nil || ca.ConfigMap.Name != "git-ca" || ca.ConfigMap.Items[0].Key != "ca.pem" {
			t.Fatalf("CA volume = %#v, want the selected key of git-ca", ca)
		}
		assertGitMountsReadOnly(t, job.Spec.Template.Spec.Containers[0])
	})
	t.Run("a commit by its id", func(t *testing.T) {
		t.Parallel()
		spec := gitHTTPSSpec("db/schema.hcl")
		spec.Ref = operatorv1alpha1.GitRef{Commit: gitCommit}
		spec.AuthFrom, spec.Transport = nil, operatorv1alpha1.GitTransportSpec{}
		job, err := builderFixture().Build(gitSchemaFixture(spec), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		env := envMap(job)
		if env[runner.EnvGitCommit].Value != gitCommit {
			t.Fatalf("%s = %q, want the commit the ref names", runner.EnvGitCommit, env[runner.EnvGitCommit].Value)
		}
		for _, name := range []string{runner.EnvGitBranch, runner.EnvGitTag, runner.EnvGitHostGrant, runner.EnvGitPassword} {
			if _, present := env[name]; present {
				t.Errorf("an anonymous Resolve by commit carries %s", name)
			}
		}
	})
	t.Run("ssh with a deploy key and known hosts", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		spec := gitSSHSpec("db/migrations")
		migration.Spec.Artifact = operatorv1alpha1.SourceSpec{Git: spec}
		job, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil)
		if err != nil {
			t.Fatalf("BuildMigration() = %v", err)
		}
		assertNoRegistry(t, job)
		env := envMap(job)
		if env[runner.EnvGitTag].Value != "v1.4.0" || env[runner.EnvGitSelects].Value != gitSelectsDirectory ||
			env[runner.EnvGitSSHKeyFile].Value != gitSSHKeyPath || env[runner.EnvGitKnownHostsSourceFile].Value != gitKnownHostsPath {
			t.Fatalf("environment = %#v", env)
		}
		if secret, key, _ := secretKeyOf(env[runner.EnvGitKnownHostsSHA256Grant]); secret != "git-deploy-key" || key != operatorv1alpha1.GitKnownHostsSHA256SecretKey {
			t.Fatalf("the known-hosts grant reads %s/%s", secret, key)
		}
		if _, present := env[runner.EnvGitPassword]; present {
			t.Fatal("an ssh:// source reads a password")
		}
		assertGitMountsReadOnly(t, job.Spec.Template.Spec.Containers[0])
		key := volumeNamed(job, gitCredentialsVolumeName)
		if key == nil || key.Secret == nil || key.Secret.SecretName != "git-deploy-key" ||
			len(key.Secret.Items) != 1 || key.Secret.Items[0].Key != operatorv1alpha1.GitSSHPrivateKeySecretKey {
			t.Fatalf("key volume = %#v, want only the deploy key", key)
		}
	})
}

// assertGitFetch holds a Job that reads a Git source to its one shape: the
// runner, holding the credential, fetches the bound commit into the source
// volume before the container that holds the database URL starts, which
// never sees the credential.
func assertGitFetch(t *testing.T, job *batchv1.Job, destination, selects string) {
	t.Helper()
	assertNoRegistry(t, job)
	spec := job.Spec.Template.Spec
	if got := containerNames(spec.InitContainers); !slices.Equal(got, []string{initContainerName, gitFetchContainerName}) {
		t.Fatalf("init containers = %v, want the runner install and the Git fetch", got)
	}
	fetch := spec.InitContainers[1]
	wantArgs := []string{"--fetch-git-source", destination, "--ptah-binary", ptahBinaryPath}
	if fetch.Command[0] != runnerPath || !slices.Equal(fetch.Args, wantArgs) {
		t.Fatalf("the fetch runs %v %v, want the runner with %v", fetch.Command, fetch.Args, wantArgs)
	}
	env := containerEnv(fetch)
	for name, want := range map[string]string{
		runner.EnvGitCommit: gitCommit, runner.EnvExpectedContentDigest: digest('7'), runner.EnvGitSelects: selects,
		"TMPDIR": fetchWorkPath, runner.EnvRunnerProtocolVersion: "1",
	} {
		if env[name].Value != want {
			t.Errorf("fetch %s = %q, want %q", name, env[name].Value, want)
		}
	}
	if _, present := env[runner.EnvGitHostGrant]; !present {
		t.Error("the fetch carries no host grant for its credential")
	}
	assertGitMountsReadOnly(t, fetch)
	main := spec.Containers[0]
	for _, variable := range main.Env {
		if strings.HasPrefix(variable.Name, "PTAH_OPERATOR_GIT_") || strings.HasPrefix(variable.Name, "PTAH_GIT_") {
			t.Fatalf("the container holding the database URL carries %s", variable.Name)
		}
	}
	for _, mount := range main.VolumeMounts {
		switch mount.Name {
		case gitCredentialsVolumeName, gitCAVolumeName, gitKnownHostsVolumeName, fetchWorkVolumeName:
			t.Fatalf("the container holding the database URL mounts %s", mount.Name)
		case sourceVolumeName:
			if !mount.ReadOnly {
				t.Fatal("the container holding the database URL can write the fetched source")
			}
		}
	}
}

// assertGitMountsReadOnly holds every credential and trust mount of a
// container to read-only.
func assertGitMountsReadOnly(t *testing.T, container corev1.Container) {
	t.Helper()
	mounted := 0
	for _, mount := range container.VolumeMounts {
		switch mount.Name {
		case gitCredentialsVolumeName, gitCAVolumeName, gitKnownHostsVolumeName:
			mounted++
			if !mount.ReadOnly {
				t.Errorf("%s mounts %s writable", container.Name, mount.Name)
			}
		}
	}
	if mounted == 0 {
		t.Errorf("%s mounts no Git credential or trust", container.Name)
	}
}

func TestBuildFetchesAGitSourceBeforeTheOperation(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			spec := gitHTTPSSpec("db/schema.hcl")
			claim := operationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{Git: gitBindingOf(spec)}
			job, err := builderFixture().Build(gitSchemaFixture(spec), claim, nil)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			assertGitFetch(t, job, schemaSnapshotPath, gitSelectsFile)
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
			spec := gitSSHSpec("db/migrations")
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{Git: spec}
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Git: gitBindingOf(spec)}
			claim := migrationOperationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{Git: gitBindingOf(spec)}
			var plan *operatorv1alpha1.PtahMigrationPlan
			if operation == operatorv1alpha1.MigrationOperationApply {
				plan = migrationPlanFixture()
			}
			job, err := builderFixture().BuildMigration(migration, claim, plan)
			if err != nil {
				t.Fatalf("BuildMigration() = %v", err)
			}
			assertGitFetch(t, job, migrationsPath, gitSelectsDirectory)
		})
	}
}

func TestBuildRefusesGitJobsItCannotCarryOut(t *testing.T) {
	t.Parallel()
	if _, err := builderFixture().Build(gitSchemaFixture(gitHTTPSSpec("db/schema.hcl")),
		operationFixture(operatorv1alpha1.OperationVerify), nil); err == nil || !strings.Contains(err.Error(), "no Verify Job") {
		t.Errorf("Build() of a Git source's Verify Job = %v, want the refusal naming it", err)
	}
	for name, mutate := range map[string]func(*operatorv1alpha1.PtahSchema, *operatorv1alpha1.ActiveOperationStatus){
		"a user in an https:// URL": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Git.URL = "https://bot@git.example.com/acme/app.git"
		},
		"an abbreviated commit": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Git.Commit = gitCommit[:12]
		},
		"no content digest": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Git.ContentDigest = ""
		},
		"a credential in the URL": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Git.URL = "https://bot:token@git.example.com/acme/app.git"
		},
		"a Git commit and registry credentials": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{Name: "registry"}
		},
		"an optional CA": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			optional := true
			claim.Source.Git.Transport.CAFrom.Optional = &optional
		},
		"known hosts for an https:// repository": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Git.Transport.KnownHostsFrom = &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "git-hosts"}, Key: "known_hosts",
			}
		},
		"an ssh:// repository without known hosts": func(_ *operatorv1alpha1.PtahSchema, claim *operatorv1alpha1.ActiveOperationStatus) {
			claim.Source.Git.URL = "ssh://git@git.example.com/acme/app.git"
			claim.Source.Git.Transport = operatorv1alpha1.GitTransportSpec{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := gitHTTPSSpec("db/schema.hcl")
			schema := gitSchemaFixture(spec)
			claim := operationFixture(operatorv1alpha1.OperationObserve)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{Git: gitBindingOf(spec)}
			mutate(schema, &claim)
			if _, err := builderFixture().Build(schema, claim, nil); err == nil {
				t.Fatalf("Build accepted %s", name)
			}
		})
	}
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{Git: gitSSHSpec("db/migrations")}
	if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationVerify), nil); err == nil ||
		!strings.Contains(err.Error(), "no Verify Job") {
		t.Errorf("BuildMigration() of a Git source's Verify Job = %v, want the refusal naming it", err)
	}
	for name, edit := range map[string]func(*operatorv1alpha1.GitSourceSpec){
		"an optional CA": func(spec *operatorv1alpha1.GitSourceSpec) {
			optional := true
			spec.URL = "https://git.example.com/acme/app.git"
			spec.Transport = operatorv1alpha1.GitTransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "git-ca"}, Key: "ca.pem", Optional: &optional,
			}}
		},
		"an ssh:// repository without known hosts": func(spec *operatorv1alpha1.GitSourceSpec) {
			spec.Transport = operatorv1alpha1.GitTransportSpec{}
		},
	} {
		spec := gitSSHSpec("db/migrations")
		edit(spec)
		migration := migrationFixture()
		migration.Spec.Artifact = operatorv1alpha1.SourceSpec{Git: spec}
		if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil); err == nil {
			t.Errorf("BuildMigration built a Resolve Job for %s", name)
		}
	}
}
