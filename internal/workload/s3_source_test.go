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
	"github.com/stokaro/ptah-operator/internal/s3source"
	"github.com/stokaro/ptah-operator/internal/sourcearchive"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// s3FileSpec is a schema file in a private bucket behind a CA.
func s3FileSpec() *operatorv1alpha1.S3SourceSpec {
	return &operatorv1alpha1.S3SourceSpec{
		Endpoint: "https://objects.example.com:9000", Region: "us-east-1", Bucket: "app-schemas",
		Key: "app/1.4.0/schema.hcl", Digest: digest('d'), Format: operatorv1alpha1.HTTPSFormatFile, Path: "schema.hcl",
		AuthFrom: &operatorv1alpha1.S3AuthSource{Name: "schemas-keys"},
		Transport: operatorv1alpha1.S3TransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "objects-ca"}, Key: "ca.pem",
		}},
	}
}

// s3ArchiveSpec is a migration directory in one version of a public tar.gz.
func s3ArchiveSpec() *operatorv1alpha1.S3SourceSpec {
	return &operatorv1alpha1.S3SourceSpec{
		Endpoint: "https://objects.example.com", Region: "auto", Bucket: "app-releases",
		Key: "app/1.4.0/app.tar.gz", VersionID: "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY",
		Digest: digest('e'), Format: operatorv1alpha1.HTTPSFormatTarGzip, Path: "app-1.4.0/db/migrations",
	}
}

// s3BindingOf is the binding a resolution of spec gives, in which the
// served version is the one named, or "served-version" where none was.
func s3BindingOf(spec *operatorv1alpha1.S3SourceSpec) *operatorv1alpha1.S3SourceBinding {
	return sourcebinding.S3Access(*spec, s3RevisionOf(spec))
}

func s3RevisionOf(spec *operatorv1alpha1.S3SourceSpec) operatorv1alpha1.S3SourceRevision {
	version := spec.VersionID
	if version == "" {
		version = "served-version"
	}
	return operatorv1alpha1.S3SourceRevision{
		Endpoint: spec.Endpoint, Region: spec.Region, Bucket: spec.Bucket, Key: spec.Key, VersionID: version,
		Format: spec.Format, Path: spec.Path, Digest: spec.Digest, ContentDigest: digest('7'),
	}
}

func s3SchemaFixture(spec *operatorv1alpha1.S3SourceSpec) *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: spec}
	revision := s3RevisionOf(spec)
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{S3: &revision}
	return schema
}

// assertNoOtherRemote holds an S3 Job to carrying nothing of a Git or an
// HTTPS source.
func assertNoOtherRemote(t *testing.T, job *batchv1.Job) {
	t.Helper()
	assertNoGit(t, job)
	for _, volume := range []string{httpsCredentialsVolumeName, httpsCAVolumeName} {
		if volumeNamed(job, volume) != nil {
			t.Fatalf("an S3-source Job carries the volume %s", volume)
		}
	}
	for _, variable := range allEnvironment(job) {
		if strings.HasPrefix(variable.Name, "PTAH_OPERATOR_HTTPS_") || strings.HasPrefix(variable.Name, "PTAH_HTTPS_") {
			t.Fatalf("an S3-source Job carries the HTTPS variable %s", variable.Name)
		}
	}
}

func allEnvironment(job *batchv1.Job) []corev1.EnvVar {
	spec := job.Spec.Template.Spec
	var environment []corev1.EnvVar
	for _, container := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		environment = append(environment, container.Env...)
	}
	return environment
}

// isS3Variable is a variable that carries an S3 source's coordinates, grants
// or credential files.
func isS3Variable(name string) bool {
	return strings.HasPrefix(name, "PTAH_OPERATOR_S3_") || strings.HasPrefix(name, "PTAH_S3_")
}

// assertS3MountsReadOnly holds every credential and trust mount of a
// container to read-only, and requires at least one.
func assertS3MountsReadOnly(t *testing.T, container corev1.Container) {
	t.Helper()
	mounted := 0
	for _, mount := range container.VolumeMounts {
		switch mount.Name {
		case s3CredentialsVolumeName, s3CAVolumeName:
			mounted++
			if !mount.ReadOnly {
				t.Errorf("%s mounts %s writable", container.Name, mount.Name)
			}
		}
	}
	if mounted == 0 {
		t.Errorf("%s mounts no S3 credential or trust", container.Name)
	}
}

func TestBuildResolvesAnS3SourceInTheOperationContainer(t *testing.T) {
	t.Parallel()
	t.Run("a File behind a key pair under a CA", func(t *testing.T) {
		t.Parallel()
		job, err := builderFixture().Build(s3SchemaFixture(s3FileSpec()), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		assertNoRegistry(t, job)
		assertNoOtherRemote(t, job)
		if got := containerNames(job.Spec.Template.Spec.InitContainers); !slices.Equal(got, []string{initContainerName}) {
			t.Fatalf("init containers = %v, want only the runner install", got)
		}
		env := envMap(job)
		for name, want := range map[string]string{
			runner.EnvS3Endpoint: "https://objects.example.com:9000", runner.EnvS3Region: "us-east-1",
			runner.EnvS3Bucket: "app-schemas", runner.EnvS3Key: "app/1.4.0/schema.hcl", runner.EnvS3Digest: digest('d'),
			runner.EnvS3Format: "File", runner.EnvS3Path: "schema.hcl", runner.EnvS3Selects: gitSelectsFile,
			runner.EnvS3CASourceFile: s3CAPath,
		} {
			if env[name].Value != want {
				t.Errorf("%s = %q, want %q", name, env[name].Value, want)
			}
		}
		// A spec that names no version reads the current one.
		for _, name := range []string{
			runner.EnvS3VersionID, runner.EnvExpectedContentDigest, runner.EnvDatabaseURL, runner.EnvDatabaseURLFile,
			runner.EnvRequestedReference,
		} {
			if _, present := env[name]; present {
				t.Errorf("a Resolve Job carries %s", name)
			}
		}
		for name, want := range map[string]struct {
			key      string
			optional bool
		}{
			runner.EnvS3HostGrant:     {operatorv1alpha1.S3HostSecretKey, false},
			runner.EnvS3BucketGrant:   {operatorv1alpha1.S3BucketSecretKey, true},
			runner.EnvS3CASHA256Grant: {operatorv1alpha1.S3CASHA256SecretKey, false},
		} {
			secret, key, optional := secretKeyOf(env[name])
			if secret != "schemas-keys" || key != want.key || optional != want.optional {
				t.Errorf("%s reads %s/%s optional=%v, want schemas-keys/%s optional=%v", name, secret, key, optional, want.key, want.optional)
			}
		}
		// The runner refuses half a pair and a token without one, so each
		// key may be missing from the Secret.
		main := job.Spec.Template.Spec.Containers[0]
		assertCredentialFile(t, job, main, runner.EnvS3AccessKeyIDFile, "schemas-keys", operatorv1alpha1.S3AccessKeyIDSecretKey, true)
		assertCredentialFile(t, job, main, runner.EnvS3SecretAccessKeyFile, "schemas-keys", operatorv1alpha1.S3SecretAccessKeySecretKey, true)
		assertCredentialFile(t, job, main, runner.EnvS3SessionTokenFile, "schemas-keys", operatorv1alpha1.S3SessionTokenSecretKey, true)
		ca := volumeNamed(job, s3CAVolumeName)
		if ca == nil || ca.ConfigMap == nil || ca.ConfigMap.Name != "objects-ca" || ca.ConfigMap.Items[0].Key != "ca.pem" {
			t.Fatalf("CA volume = %#v, want the selected key of objects-ca", ca)
		}
		assertS3MountsReadOnly(t, main)
	})
	// The owner's CA pin reaches the runner even when the resource selects
	// no CA, which refuses the pairing; the kubelet leaves the variable out
	// when the Secret pins nothing.
	t.Run("a File behind a key pair on the system roots", func(t *testing.T) {
		t.Parallel()
		spec := s3FileSpec()
		spec.Transport = operatorv1alpha1.S3TransportSpec{}
		job, err := builderFixture().Build(s3SchemaFixture(spec), operationFixture(operatorv1alpha1.OperationResolve), nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		env := envMap(job)
		if secret, key, optional := secretKeyOf(env[runner.EnvS3CASHA256Grant]); secret != "schemas-keys" ||
			key != operatorv1alpha1.S3CASHA256SecretKey || !optional {
			t.Errorf("%s reads %s/%s optional=%v, want the optional pin", runner.EnvS3CASHA256Grant, secret, key, optional)
		}
		if _, present := env[runner.EnvS3CASourceFile]; present {
			t.Errorf("a source with no CA selected carries %s", runner.EnvS3CASourceFile)
		}
		if volumeNamed(job, s3CAVolumeName) != nil {
			t.Fatal("a source with no CA selected mounts one")
		}
	})
	t.Run("a named version of a public archive of migrations", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: s3ArchiveSpec()}
		job, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil)
		if err != nil {
			t.Fatalf("BuildMigration() = %v", err)
		}
		assertNoRegistry(t, job)
		assertNoOtherRemote(t, job)
		env := envMap(job)
		for name, want := range map[string]string{
			runner.EnvS3Format: "TarGzip", runner.EnvS3Selects: gitSelectsDirectory, runner.EnvS3Path: "app-1.4.0/db/migrations",
			runner.EnvS3VersionID: "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY", runner.EnvS3Region: "auto",
		} {
			if env[name].Value != want {
				t.Errorf("%s = %q, want %q", name, env[name].Value, want)
			}
		}
		for _, name := range []string{
			runner.EnvS3HostGrant, runner.EnvS3BucketGrant, runner.EnvS3CASHA256Grant, runner.EnvS3AccessKeyIDFile,
			runner.EnvS3SecretAccessKeyFile, runner.EnvS3SessionTokenFile, runner.EnvS3CASourceFile,
		} {
			if _, present := env[name]; present {
				t.Errorf("an unsigned Resolve carries %s", name)
			}
		}
		if volumeNamed(job, s3CredentialsVolumeName) != nil || volumeNamed(job, s3CAVolumeName) != nil {
			t.Fatal("an unsigned Resolve mounts a credential or a CA")
		}
	})
}

// assertS3Fetch holds a Job that reads an S3 source to its one shape: the
// runner, holding the key pair, reads the bound version into the source
// volume before the container that holds the database URL starts, which
// never sees the key pair.
func assertS3Fetch(t *testing.T, job *batchv1.Job, destination, selects, version string, credentialed bool) {
	t.Helper()
	assertNoRegistry(t, job)
	assertNoOtherRemote(t, job)
	spec := job.Spec.Template.Spec
	if got := containerNames(spec.InitContainers); !slices.Equal(got, []string{initContainerName, s3FetchContainerName}) {
		t.Fatalf("init containers = %v, want the runner install and the S3 fetch", got)
	}
	fetch := spec.InitContainers[1]
	wantArgs := []string{"--fetch-s3-source", destination, "--ptah-binary", ptahBinaryPath}
	if fetch.Command[0] != runnerPath || !slices.Equal(fetch.Args, wantArgs) {
		t.Fatalf("the fetch runs %v %v, want the runner with %v", fetch.Command, fetch.Args, wantArgs)
	}
	env := containerEnv(fetch)
	for name, want := range map[string]string{
		runner.EnvExpectedContentDigest: digest('7'), runner.EnvS3Selects: selects, runner.EnvS3VersionID: version,
		"TMPDIR": fetchWorkPath, runner.EnvRunnerProtocolVersion: "1",
	} {
		if env[name].Value != want {
			t.Errorf("fetch %s = %q, want %q", name, env[name].Value, want)
		}
	}
	if !sha256Pattern.MatchString(env[runner.EnvS3Digest].Value) {
		t.Errorf("the fetch carries no object digest: %q", env[runner.EnvS3Digest].Value)
	}
	if _, present := env[runner.EnvS3HostGrant]; present != credentialed {
		t.Errorf("the fetch carries a host grant = %v, want %v", present, credentialed)
	}
	if credentialed {
		assertS3MountsReadOnly(t, fetch)
	}
	main := spec.Containers[0]
	for _, variable := range main.Env {
		if isS3Variable(variable.Name) {
			t.Fatalf("the container holding the database URL carries %s", variable.Name)
		}
	}
	for _, mount := range main.VolumeMounts {
		switch mount.Name {
		case s3CredentialsVolumeName, s3CAVolumeName, fetchWorkVolumeName:
			t.Fatalf("the container holding the database URL mounts %s", mount.Name)
		case sourceVolumeName:
			if !mount.ReadOnly {
				t.Fatal("the container holding the database URL can write the fetched source")
			}
		}
	}
}

func TestBuildFetchesAnS3SourceBeforeTheOperation(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			spec := s3FileSpec()
			claim := operationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(spec)}
			job, err := builderFixture().Build(s3SchemaFixture(spec), claim, nil)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			// The spec names no version, so the fetch reads the one the
			// resolution was served.
			assertS3Fetch(t, job, schemaSnapshotPath, gitSelectsFile, "served-version", true)
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
			spec := s3ArchiveSpec()
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: spec}
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(spec)}
			claim := migrationOperationFixture(operation)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(spec)}
			var plan *operatorv1alpha1.PtahMigrationPlan
			if operation == operatorv1alpha1.MigrationOperationApply {
				plan = migrationPlanFixture()
			}
			job, err := builderFixture().BuildMigration(migration, claim, plan)
			if err != nil {
				t.Fatalf("BuildMigration() = %v", err)
			}
			assertS3Fetch(t, job, migrationsPath, gitSelectsDirectory, "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY", false)
		})
	}
	// A bucket that keeps no versions leaves the binding without one, and
	// the fetch reads whatever the key holds, held to the same digests.
	t.Run("no version", func(t *testing.T) {
		t.Parallel()
		spec := s3FileSpec()
		claim := operationFixture(operatorv1alpha1.OperationObserve)
		claim.Source = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(spec)}
		claim.Source.S3.VersionID = ""
		job, err := builderFixture().Build(s3SchemaFixture(spec), claim, nil)
		if err != nil {
			t.Fatalf("Build() = %v", err)
		}
		if _, present := containerEnv(job.Spec.Template.Spec.InitContainers[1])[runner.EnvS3VersionID]; present {
			t.Fatalf("a fetch of an unversioned object carries %s", runner.EnvS3VersionID)
		}
	})
}

// s3FetchRoom is what an S3 fetch's volumes have to hold for the runner's
// bounds to refuse an object before the volume runs out of space, on a node
// whose pages are as large as Linux makes them: the CA copy and the object
// in the work directory, the selected files in the source volume, every file
// rounded up to a page.
func s3FetchRoom() (work, source int64) {
	const page = 64 << 10
	return s3source.MaxDownloadBytes + runner.MaxTrustFileBytes + 2*page,
		sourcearchive.MaxSelectedBytes + sourcecontent.MaxFiles*page
}

// holdsAtLeast reports whether volume is a memory-backed emptyDir that holds
// at least need bytes.
func holdsAtLeast(volume corev1.Volume, need int64) bool {
	dir := volume.EmptyDir
	return dir != nil && dir.Medium == corev1.StorageMediumMemory && dir.SizeLimit != nil && dir.SizeLimit.Value() >= need
}

// TestAnS3FetchHasRoomForWhatTheRunnerAdmits holds the fetch's volumes to the
// runner's bounds. With 64 MiB volumes, an object of exactly the 64 MiB the
// Resolve admitted failed the fetch for lack of space beside the CA copy, and
// so could a selection of 64 MiB in many small files.
func TestAnS3FetchHasRoomForWhatTheRunnerAdmits(t *testing.T) {
	t.Parallel()
	work, source := s3FetchRoom()
	// The check refuses the 64 MiB the volumes had.
	tooSmall := corev1.Volume{VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(64 << 20)}}
	if holdsAtLeast(tooSmall, work) || holdsAtLeast(tooSmall, source) {
		t.Fatalf("a 64 MiB volume passes the room check for %d and %d bytes", work, source)
	}
	spec := s3FileSpec()
	claim := operationFixture(operatorv1alpha1.OperationObserve)
	claim.Source = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(spec)}
	schemaJob, err := builderFixture().Build(s3SchemaFixture(spec), claim, nil)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	archive := s3ArchiveSpec()
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: archive}
	migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(archive)}
	history := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
	history.Source = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(archive)}
	migrationJob, err := builderFixture().BuildMigration(migration, history, nil)
	if err != nil {
		t.Fatalf("BuildMigration() = %v", err)
	}
	assertFetchRoom(t, map[string]*batchv1.Job{"schema observe": schemaJob, "migration history": migrationJob}, work, source)
}

// assertFetchRoom holds the two volumes of each Job's fetch to the room
// given, and fails a Job in which it did not find both.
func assertFetchRoom(t *testing.T, jobs map[string]*batchv1.Job, work, source int64) {
	t.Helper()
	for name, job := range jobs {
		checked := 0
		for _, volume := range job.Spec.Template.Spec.Volumes {
			switch volume.Name {
			case fetchWorkVolumeName:
				checked++
				if !holdsAtLeast(volume, work) {
					t.Errorf("%s: the %s volume is %+v, want a memory-backed one of at least %d bytes", name, volume.Name, volume.EmptyDir, work)
				}
			case sourceVolumeName:
				checked++
				if !holdsAtLeast(volume, source) {
					t.Errorf("%s: the %s volume is %+v, want a memory-backed one of at least %d bytes", name, volume.Name, volume.EmptyDir, source)
				}
			}
		}
		if checked != 2 {
			t.Errorf("%s: found %d of the fetch's two volumes", name, checked)
		}
	}
}

// TestAnHTTPSFetchHasRoomForWhatThePullAdmits holds an HTTPS fetch's volumes
// to the bounds of Ptah's pull. The pull writes the download, up to one byte
// past its 64 MiB bound, into the work directory beside the runner's copy of
// the CA, removes it, and writes the selected files there, which the runner
// copies to the source volume. With 64 MiB volumes a download of exactly the
// bound failed for lack of space, and so could a selection of 64 MiB in many
// small files.
func TestAnHTTPSFetchHasRoomForWhatThePullAdmits(t *testing.T) {
	t.Parallel()
	const (
		page = 64 << 10
		// Ptah's internal/httpsource.MaxDownloadBytes.
		download = 64 << 20
	)
	selected := int64(sourcearchive.MaxSelectedBytes + sourcecontent.MaxFiles*page)
	work := runner.MaxTrustFileBytes + page + max(download+page, selected)
	tooSmall := corev1.Volume{VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(64 << 20)}}
	if holdsAtLeast(tooSmall, work) || holdsAtLeast(tooSmall, selected) {
		t.Fatalf("a 64 MiB volume passes the room check for %d and %d bytes", work, selected)
	}
	spec := httpsFileSpec()
	claim := operationFixture(operatorv1alpha1.OperationObserve)
	claim.Source = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(spec)}
	schemaJob, err := builderFixture().Build(httpsSchemaFixture(spec), claim, nil)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	archive := httpsArchiveSpec()
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: archive}
	migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(archive)}
	history := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
	history.Source = &operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBindingOf(archive)}
	migrationJob, err := builderFixture().BuildMigration(migration, history, nil)
	if err != nil {
		t.Fatalf("BuildMigration() = %v", err)
	}
	assertFetchRoom(t, map[string]*batchv1.Job{"schema observe": schemaJob, "migration history": migrationJob}, work, selected)
}

// s3PlanFixture is planFixture for a schema whose source is S3: contract
// version 2, with the source bound in place of the artifact digest and the
// verification policy.
func s3PlanFixture(schema *operatorv1alpha1.PtahSchema, builder Builder) *operatorv1alpha1.PtahSchemaPlan {
	plan := planFixture(schema, builder)
	s3 := schema.Status.Source.S3
	source := sourcebinding.S3PlanSource(s3.Endpoint, s3.Bucket, s3.Key, s3.Path, s3.ContentDigest)
	plan.Spec.ContractVersion = fingerprint.SourcePlanContractVersion
	plan.Spec.ArtifactDigest, plan.Spec.Source = "", source
	plan.Spec.VerificationPolicyUID, plan.Spec.VerificationPolicyDigest = "", ""
	current := schema.Status.Plan
	current.ArtifactDigest, current.Source = "", source.DeepCopy()
	current.VerificationPolicyUID, current.VerificationPolicyDigest = "", ""
	return plan
}

// The Apply that follows a plan computed from an S3 source reads the plan's
// bytes alone, so it reads no object and carries nothing of the source. A
// resolution for an object the spec no longer names is not the source the
// plan may be applied from; one for another version, where the spec names
// none, still is.
func TestBuildAppliesAPlanComputedFromAnS3Source(t *testing.T) {
	t.Parallel()
	builder := builderFixture()
	schema := s3SchemaFixture(s3FileSpec())
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), s3PlanFixture(schema, builder))
	if err != nil {
		t.Fatalf("Build() of an S3 source's Apply = %v", err)
	}
	assertNoRegistry(t, job)
	for _, variable := range allEnvironment(job) {
		if isS3Variable(variable.Name) {
			t.Errorf("the Apply carries %s", variable.Name)
		}
	}
	for name, edit := range map[string]func(*operatorv1alpha1.S3SourceSpec){
		"another digest":   func(spec *operatorv1alpha1.S3SourceSpec) { spec.Digest = digest('f') },
		"another endpoint": func(spec *operatorv1alpha1.S3SourceSpec) { spec.Endpoint = "https://objects.example.net" },
		"another region":   func(spec *operatorv1alpha1.S3SourceSpec) { spec.Region = "eu-west-1" },
		"another bucket":   func(spec *operatorv1alpha1.S3SourceSpec) { spec.Bucket = "app-schemas-next" },
		"another key":      func(spec *operatorv1alpha1.S3SourceSpec) { spec.Key = "app/1.5.0/schema.hcl" },
		"another path":     func(spec *operatorv1alpha1.S3SourceSpec) { spec.Path = "other.hcl" },
		"another version":  func(spec *operatorv1alpha1.S3SourceSpec) { spec.VersionID = "another-version" },
	} {
		moved := s3SchemaFixture(s3FileSpec())
		plan := s3PlanFixture(moved, builder)
		edit(moved.Spec.Desired.S3)
		if _, err := builder.Build(moved, operationFixture(operatorv1alpha1.OperationApply), plan); err == nil ||
			!strings.Contains(err.Error(), "resolved for another source") {
			t.Errorf("Build() of an Apply whose S3 resolution answers %s = %v, want the refusal naming it", name, err)
		}
	}
	named := s3SchemaFixture(s3FileSpec())
	plan := s3PlanFixture(named, builder)
	named.Spec.Desired.S3.VersionID = named.Status.Source.S3.VersionID
	if _, err := builder.Build(named, operationFixture(operatorv1alpha1.OperationApply), plan); err != nil {
		t.Errorf("Build() of an Apply whose spec names the version the resolution bound = %v", err)
	}
}

func TestBuildRefusesS3JobsItCannotCarryOut(t *testing.T) {
	t.Parallel()
	if _, err := builderFixture().Build(s3SchemaFixture(s3FileSpec()),
		operationFixture(operatorv1alpha1.OperationVerify), nil); err == nil || !strings.Contains(err.Error(), "no Verify Job") {
		t.Errorf("Build() of an S3 source's Verify Job = %v, want the refusal naming it", err)
	}
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: s3ArchiveSpec()}
	if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationVerify), nil); err == nil ||
		!strings.Contains(err.Error(), "no Verify Job") {
		t.Errorf("BuildMigration() of an S3 source's Verify Job = %v, want the refusal naming it", err)
	}
	for name, mutate := range map[string]func(*operatorv1alpha1.SourceAccessBinding){
		"http://":                 func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Endpoint = "http://objects.example.com" },
		"a path in the endpoint":  func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Endpoint += "/app-schemas" },
		"a user in the endpoint":  func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Endpoint = "https://bot@objects.example.com" },
		"no region":               func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Region = "" },
		"a bucket the API lacks":  func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Bucket = "App_Schemas" },
		"a key that climbs":       func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Key = "app/../schema.hcl" },
		"a key from the root":     func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Key = "/schema.hcl" },
		"a version with a space":  func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.VersionID = "a version" },
		"no content digest":       func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.ContentDigest = "" },
		"no object digest":        func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Digest = "" },
		"a File at a nested path": func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Path = "db/schema.hcl" },
		"registry credentials": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{Name: "registry"}
		},
		"an HTTPS download beside it": func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS = httpsBindingOf(httpsFileSpec()) },
		"an optional CA":              func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Transport.CAFrom.Optional = new(true) },
		"an unnamed credential":       func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.AuthFrom.Name = "" },
		"a format the API lacks":      func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Format = "Tar" },
		"an archive's escape path": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Format, b.S3.Path = operatorv1alpha1.HTTPSFormatZip, "../db"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := s3FileSpec()
			claim := operationFixture(operatorv1alpha1.OperationObserve)
			claim.Source = &operatorv1alpha1.SourceAccessBinding{S3: s3BindingOf(spec)}
			mutate(claim.Source)
			if _, err := builderFixture().Build(s3SchemaFixture(spec), claim, nil); err == nil {
				t.Fatalf("Build accepted %s", name)
			}
		})
	}
	optional := s3ArchiveSpec()
	optional.Transport.CAFrom = &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "objects-ca"}, Key: "ca.pem", Optional: new(true),
	}
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: optional}
	if _, err := builderFixture().BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil); err == nil {
		t.Error("BuildMigration built a Resolve Job for an optional CA")
	}
}
