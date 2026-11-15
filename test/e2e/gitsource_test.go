package e2e

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/e2e/gitrefusal"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

const (
	testGitURL    = "https://e2e-git.ptah-test.svc/acme/app.git"
	testGitCommit = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
	// testGitCaptures holds what the pinned Ptah image printed for this phase's
	// repository; test/runnerharness captures it.
	testGitCaptures = "../../internal/dataplane/testdata/git-pull"
)

// The rows that read Ptah's refusals in a condition message match what the
// pinned image printed for these sources. refusedSources and forcePushedBranch
// send the same sources and match the same words, against a cluster.
func TestTheGitRefusalRowsMatchWhatPtahPrinted(t *testing.T) {
	t.Parallel()
	built, err := gitrepo.Build(gitRepositorySpec(2))
	if err != nil {
		t.Fatal(err)
	}
	for capture, want := range map[string]string{
		"refused-submodule":     gitSubmoduleRefusal,
		"refused-lfs-pointer":   gitLFSRefusal,
		"refused-host-key":      gitrefusal.HostKeyMismatch,
		"refused-commit-pinned": gitrefusal.CommitNotNamed + built.Commit(2).String(),
	} {
		t.Run(capture, func(t *testing.T) {
			t.Parallel()
			stderr, err := os.ReadFile(filepath.Join(testGitCaptures, capture, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(stderr), want) {
				t.Fatalf("the capture printed %q, and the row matches %q", stderr, want)
			}
		})
	}
}

// The captured reports answer the repository this phase serves: the commit ids
// it computes from the Spec, and the content digests it binds. A change to the
// repository that leaves the captures behind fails here, and `make
// runner-harness-write` captures them again.
func TestTheGitCapturesAnswerThePhasesRepository(t *testing.T) {
	t.Parallel()
	built, err := gitrepo.Build(gitRepositorySpec(2))
	if err != nil {
		t.Fatal(err)
	}
	first, tip := built.Commit(0).String(), built.Commit(2).String()
	schemaAtFirst, err := gitSchemaContent(0)
	if err != nil {
		t.Fatal(err)
	}
	schemaAtTip, err := gitSchemaContent(1)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := gitMigrationContent()
	if err != nil {
		t.Fatal(err)
	}
	const overHTTPS, overSSH = "git+" + testGitURL, "git+ssh://git@e2e-git.ptah-test.svc/" + gitRepository
	for _, row := range []struct{ capture, reference, path, commit, digest string }{
		{"schema-https-branch", overHTTPS, gitSchemaPath, tip, schemaAtTip},
		{"schema-https-tag", overHTTPS, gitSchemaPath, first, schemaAtFirst},
		{"schema-ssh-branch", overSSH, gitSchemaPath, tip, schemaAtTip},
		{"migrations-https-branch", overHTTPS, gitMigrationPath, tip, migrations},
		{"migrations-ssh-tag", overSSH, gitMigrationPath, first, migrations},
	} {
		t.Run(row.capture, func(t *testing.T) {
			t.Parallel()
			stdout, err := os.ReadFile(filepath.Join(testGitCaptures, row.capture, "stdout"))
			if err != nil {
				t.Fatal(err)
			}
			report, err := dataplane.DecodeSourcePull(stdout, dataplane.PullRequest{Source: dataplane.PullSourceGit, Reference: row.reference, Path: row.path})
			if err != nil {
				t.Fatal(err)
			}
			if report.Commit != row.commit || report.ContentDigest != row.digest {
				t.Fatalf("the capture read %s at %s, and the phase's repository gives %s at %s: run `make runner-harness-write`",
					report.ContentDigest, report.Commit, row.digest, row.commit)
			}
		})
	}
}

// The repository the phase serves gives commits whose selected files are the
// ones the phase expects: commit 1 changes only what nothing selects.
func TestTheGitRepositoryMovesWhatThePhaseExpects(t *testing.T) {
	t.Parallel()
	built, err := gitrepo.Build(gitRepositorySpec(2))
	if err != nil {
		t.Fatal(err)
	}
	if built.Commit(0) == built.Commit(1) || built.Commit(1) == built.Commit(2) {
		t.Fatal("two commits of the phase's repository share an id")
	}
	again, err := gitrepo.Build(gitRepositorySpec(0))
	if err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		if built.Commit(index) != again.Commit(index) {
			t.Fatalf("moving main changed commit %d's id", index)
		}
	}
	first, _ := gitSchemaContent(0)
	second, _ := gitSchemaContent(1)
	migrations, err := gitMigrationContent()
	if err != nil || first == second || first == "" || migrations == "" {
		t.Fatalf("content digests %q %q %q: %v", first, second, migrations, err)
	}
	if document, err := gitSpecDocument(gitRepositorySpec(1)); err != nil || document == "" {
		t.Fatalf("the spec document: %v", err)
	}
	// The force-push keeps the first two commits and replaces the third, so
	// the commit both schemas read is no longer on the server.
	rewritten, err := gitrepo.Build(gitRewrittenSpec())
	if err != nil {
		t.Fatal(err)
	}
	if rewritten.Commit(0) != built.Commit(0) || rewritten.Commit(1) != built.Commit(1) || rewritten.Commit(2) == built.Commit(2) {
		t.Fatal("the rewritten history does not replace commit 2 alone")
	}
	if main := rewritten.Refs()["refs/heads/main"]; main != rewritten.Commit(2) {
		t.Fatalf("main names %s after the force-push, not the replacement", main)
	}
	third, _ := gitSchemaContent(2)
	if third == second || third == "" {
		t.Fatalf("the replacement's schema digest %q is not new", third)
	}
}

func TestGitResolvedToNeedsTheCommitTheContentAndTheSource(t *testing.T) {
	t.Parallel()
	ref := ptahv1alpha1.GitRef{Branch: "main"}
	status := &ptahv1alpha1.GitSourceRevision{URL: testGitURL, Ref: ref, Path: gitSchemaPath, Commit: testGitCommit, ContentDigest: testContentDigest}
	if err := gitResolvedTo(status, testGitURL, gitSchemaPath, ref, testGitCommit, testContentDigest); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.GitSourceRevision){
		"another commit":  func(s *ptahv1alpha1.GitSourceRevision) { s.Commit = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d" },
		"other content":   func(s *ptahv1alpha1.GitSourceRevision) { s.ContentDigest = "sha256:" + strings.Repeat("2", 64) },
		"another ref":     func(s *ptahv1alpha1.GitSourceRevision) { s.Ref = ptahv1alpha1.GitRef{Tag: "v1"} },
		"another path":    func(s *ptahv1alpha1.GitSourceRevision) { s.Path = gitMigrationPath },
		"another address": func(s *ptahv1alpha1.GitSourceRevision) { s.URL = "https://git.example.com/acme/app.git" },
	} {
		moved := *status
		edit(&moved)
		if gitResolvedTo(&moved, testGitURL, gitSchemaPath, ref, testGitCommit, testContentDigest) == nil {
			t.Errorf("gitResolvedTo accepted %s", name)
		}
	}
	if gitResolvedTo(nil, testGitURL, gitSchemaPath, ref, testGitCommit, testContentDigest) == nil {
		t.Error("gitResolvedTo accepted no resolution")
	}
}

func TestGitPlanBoundNeedsTheRepositoryPathAndContent(t *testing.T) {
	t.Parallel()
	source := &ptahv1alpha1.PlanSourceBinding{
		Kind: ptahv1alpha1.SourceKindGit, Revision: sourcebinding.GitRevision(testGitURL, gitSchemaPath), ContentDigest: testContentDigest,
	}
	if err := gitPlanBound(2, "", "", source, testGitURL, gitSchemaPath, testContentDigest); err != nil {
		t.Fatal(err)
	}
	other := *source
	other.Revision = sourcebinding.GitRevision(testGitURL, gitMigrationPath)
	configMap := *source
	configMap.Kind = ptahv1alpha1.SourceKindConfigMap
	for name, row := range map[string]struct {
		contract      int32
		artifact, uid string
		source        *ptahv1alpha1.PlanSourceBinding
	}{
		"contract version 1":    {1, "", "", source},
		"an artifact digest":    {2, testContentDigest, "", source},
		"a verification policy": {2, "", "policy-uid", source},
		"another path":          {2, "", "", &other},
		"a ConfigMap source":    {2, "", "", &configMap},
		"no source":             {2, "", "", nil},
	} {
		if gitPlanBound(row.contract, row.artifact, row.uid, row.source, testGitURL, gitSchemaPath, testContentDigest) == nil {
			t.Errorf("gitPlanBound accepted %s", name)
		}
	}
}

func gitJobFixture(initContainers []string, mainEnv []corev1.EnvVar, mounts []corev1.VolumeMount) *batchv1.Job {
	containers := make([]corev1.Container, 0, len(initContainers))
	for _, name := range initContainers {
		container := corev1.Container{Name: name}
		if name == gitFetchInit {
			container.Env = []corev1.EnvVar{
				{Name: "PTAH_OPERATOR_GIT_COMMIT", Value: testGitCommit},
				{Name: "PTAH_OPERATOR_GIT_HOST_GRANT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: gitTokenCredential}, Key: "host",
				}}},
			}
		}
		containers = append(containers, container)
	}
	return &batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		InitContainers: containers,
		Containers:     []corev1.Container{{Name: "ptah", Env: mainEnv, VolumeMounts: mounts}},
	}}}}
}

func TestGitFetchJobNeedsTheFetchToHoldTheCredential(t *testing.T) {
	t.Parallel()
	readOnly := []corev1.VolumeMount{{Name: "schema-source", MountPath: "/source", ReadOnly: true}}
	database := []corev1.EnvVar{{Name: "PTAH_OPERATOR_DB_URL_FILE", Value: "/credentials/database/url"}}
	if err := gitFetchJob(gitJobFixture([]string{"install-runner", gitFetchInit}, database, readOnly), testGitCommit); err != nil {
		t.Fatal(err)
	}
	for name, job := range map[string]*batchv1.Job{
		"no fetch":                 gitJobFixture([]string{"install-runner"}, database, readOnly),
		"a snapshot copy":          gitJobFixture([]string{"install-runner", "copy-source-snapshot"}, database, readOnly),
		"a credential in the main": gitJobFixture([]string{"install-runner", gitFetchInit}, append(database, corev1.EnvVar{Name: "PTAH_GIT_PASSWORD"}), readOnly),
		"a writable source":        gitJobFixture([]string{"install-runner", gitFetchInit}, database, []corev1.VolumeMount{{Name: "schema-source", MountPath: "/source"}}),
		"the trust in the main":    gitJobFixture([]string{"install-runner", gitFetchInit}, database, append(readOnly, corev1.VolumeMount{Name: "git-ca", ReadOnly: true})),
		"the database URL in the main's environment": gitJobFixture([]string{"install-runner", gitFetchInit},
			[]corev1.EnvVar{{Name: "PTAH_DB_URL", Value: "postgres://from-a-secret"}}, readOnly),
		"the Git password in the fetch's environment": func() *batchv1.Job {
			job := gitJobFixture([]string{"install-runner", gitFetchInit}, database, readOnly)
			fetch := &job.Spec.Template.Spec.InitContainers[1]
			fetch.Env = append(fetch.Env, corev1.EnvVar{Name: "PTAH_GIT_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: gitTokenCredential}, Key: "password"},
			}})
			return job
		}(),
	} {
		if gitFetchJob(job, testGitCommit) == nil {
			t.Errorf("gitFetchJob accepted %s", name)
		}
	}
	if gitFetchJob(gitJobFixture([]string{"install-runner", gitFetchInit}, database, readOnly), "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d") == nil {
		t.Error("gitFetchJob accepted a fetch of another commit")
	}
}

func TestGitResolveJobNeedsTheRepositoryAndNoDatabase(t *testing.T) {
	t.Parallel()
	repository := []corev1.EnvVar{{Name: "PTAH_OPERATOR_GIT_URL", Value: testGitURL}}
	if err := gitResolveJob(gitJobFixture([]string{"install-runner"}, repository, nil)); err != nil {
		t.Fatal(err)
	}
	for name, job := range map[string]*batchv1.Job{
		"a fetch first":  gitJobFixture([]string{"install-runner", gitFetchInit}, repository, nil),
		"no repository":  gitJobFixture([]string{"install-runner"}, nil, nil),
		"a database URL": gitJobFixture([]string{"install-runner"}, append(repository, corev1.EnvVar{Name: "PTAH_DB_URL"}), nil),
		"a database URL file": gitJobFixture([]string{"install-runner"},
			append(repository, corev1.EnvVar{Name: "PTAH_OPERATOR_DB_URL_FILE", Value: "/credentials/database/url"}), nil),
		"a database URL mount": gitJobFixture([]string{"install-runner"}, repository,
			[]corev1.VolumeMount{{Name: "database-url", MountPath: "/credentials/database", ReadOnly: true}}),
		"a shadow database URL": gitJobFixture([]string{"install-runner"}, append(repository, corev1.EnvVar{Name: "PTAH_SHADOW_DB"}), nil),
		"a shadow database URL file": gitJobFixture([]string{"install-runner"},
			append(repository, corev1.EnvVar{Name: "PTAH_OPERATOR_SHADOW_DB_URL_FILE", Value: "/credentials/shadow-database/url"}), nil),
		"a shadow database URL mount": gitJobFixture([]string{"install-runner"}, repository,
			[]corev1.VolumeMount{{Name: "shadow-database-url", MountPath: "/credentials/shadow-database", ReadOnly: true}}),
	} {
		if gitResolveJob(job) == nil {
			t.Errorf("gitResolveJob accepted %s", name)
		}
	}
}

// A Job deleted after its TTL is no Job made, and a Job made while another
// was deleted is one, which comparing counts missed.
func TestJobsOutsideNamesOnlyJobsMadeSinceTheFirstReading(t *testing.T) {
	t.Parallel()
	job := func(name string, uid types.UID) batchv1.Job {
		return batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	}
	before := map[types.UID]bool{"uid-1": true, "uid-2": true}
	for name, test := range map[string]struct {
		jobs []batchv1.Job
		want []string
	}{
		"both still there":      {jobs: []batchv1.Job{job("resolve-1", "uid-1"), job("resolve-2", "uid-2")}},
		"one deleted":           {jobs: []batchv1.Job{job("resolve-1", "uid-1")}},
		"both deleted":          {},
		"one made":              {jobs: []batchv1.Job{job("resolve-1", "uid-1"), job("resolve-2", "uid-2"), job("resolve-3", "uid-3")}, want: []string{"resolve-3"}},
		"one made, one deleted": {jobs: []batchv1.Job{job("resolve-1", "uid-1"), job("resolve-3", "uid-3")}, want: []string{"resolve-3"}},
		"a name made again":     {jobs: []batchv1.Job{job("resolve-1", "uid-1"), job("resolve-2", "uid-4")}, want: []string{"resolve-2"}},
		"one made from nothing": {jobs: []batchv1.Job{job("resolve-3", "uid-3")}, want: []string{"resolve-3"}},
	} {
		if made := jobsOutside(before, test.jobs); !slices.Equal(made, test.want) {
			t.Errorf("%s: jobsOutside named %v, want %v", name, made, test.want)
		}
	}
}

// The certificate verifies for the Service's name against the CA, the
// known-hosts line pins the host key for the host, and the client key is the
// authorized one.
func TestGitServerKeysFitTogether(t *testing.T) {
	t.Parallel()
	host := "e2e-git.ptah-test.svc"
	keys, err := newGitServerKeys(host, host, host+".cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(keys.CAPEM) {
		t.Fatal("the CA does not parse")
	}
	block, _ := pem.Decode(keys.CertificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// The CA pin row reads the server under its cluster domain name.
	for _, name := range []string{host, host + ".cluster.local"} {
		if _, err := certificate.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			t.Fatalf("the certificate does not verify for %s: %v", name, err)
		}
	}
	if _, err := certificate.Verify(x509.VerifyOptions{DNSName: "git.example.com", Roots: roots}); err == nil {
		t.Fatal("the certificate verifies for another host")
	}
	hostSigner, err := ssh.ParsePrivateKey(keys.HostKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(file, keys.KnownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	callback, err := knownhosts.New(file)
	if err != nil {
		t.Fatal(err)
	}
	address := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22}
	if err := callback(host+":22", address, hostSigner.PublicKey()); err != nil {
		t.Fatalf("the known hosts refuse the host key: %v", err)
	}
	clientSigner, err := ssh.ParsePrivateKey(keys.ClientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	authorized, _, _, _, err := ssh.ParseAuthorizedKey(keys.AuthorizedKey)
	if err != nil || string(authorized.Marshal()) != string(clientSigner.PublicKey().Marshal()) {
		t.Fatalf("the authorized key is not the client's: %v", err)
	}
	if callback(host+":22", address, clientSigner.PublicKey()) == nil {
		t.Fatal("the known hosts accept another key")
	}
	// The other known hosts pin a key the server does not hold.
	other := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(other, keys.OtherKnownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	otherCallback, err := knownhosts.New(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherCallback(host+":22", address, hostSigner.PublicKey()) == nil {
		t.Fatal("the other known hosts accept the server's host key")
	}
	if keys.Token == "" || grantDigest(keys.CAPEM) == grantDigest(keys.KnownHosts) {
		t.Fatal("the token or the grants are empty")
	}
}

// countingExecutor counts the commands the runner asked it to run and runs
// none of them.
type countingExecutor struct{ calls int }

func (e *countingExecutor) Execute(context.Context, runner.CommandSpec, io.Writer, io.Writer) (int, error) {
	e.calls++
	return 1, nil
}

// The words the CA pin row waits for are the runner's: its refusal, at a
// Resolve, of the access that row's Resolve Job carries -- the token by file,
// both grants and no CA. The runner refuses it under the code the row reads
// before it starts anything, in a result the protocol accepts and would refuse
// with a child's exit code, and the same access without the CA grant goes on
// to start Ptah, so the refusal is the pin's.
func TestGitCAPinRefusalIsTheRunnersRefusalOfThePin(t *testing.T) {
	t.Parallel()
	const qualified = "e2e-git.ptah-test.svc.cluster.local"
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	const operationID = "resolve-git-ca-pin"
	environment := []string{
		runner.EnvOperationID + "=" + operationID,
		runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion),
		runner.EnvGitURL + "=https://" + qualified + "/" + gitRepository,
		runner.EnvGitBranch + "=main",
		runner.EnvGitPath + "=" + gitSchemaPath,
		runner.EnvGitSelects + "=file",
		runner.EnvGitPasswordFile + "=" + password,
		runner.EnvGitHostGrant + "=" + qualified,
	}
	resolve := func(environment []string) (runner.Result, int) {
		executor := &countingExecutor{}
		result := runner.Run(t.Context(), runner.Config{
			Operation: runner.OperationResolve, Environment: environment, Executor: executor, TempDir: t.TempDir(),
		})
		return result, executor.calls
	}
	pinned, calls := resolve(append(slices.Clone(environment), runner.EnvGitCASHA256Grant+"="+testContentDigest))
	if pinned.Error == nil || pinned.Error.Code+": " != gitAccessRefused || !strings.Contains(pinned.Error.Message, gitCAPinRefusal) ||
		pinned.ChildExitCode != -1 || calls != 0 {
		t.Fatalf("Run() = %#v after %d commands, and the row waits for %q after %q", pinned, calls, gitCAPinRefusal, gitAccessRefused)
	}
	frame, err := runner.MarshalFrame(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ParseResultFor(frame, runner.OperationResolve, operationID); err != nil {
		t.Fatalf("the protocol refused the runner's refusal: %v", err)
	}
	ran := pinned
	ran.ChildExitCode = 1
	if frame, err := runner.MarshalFrame(ran); err == nil {
		if _, err := runner.ParseResultFor(frame, runner.OperationResolve, operationID); err == nil {
			t.Fatal("the protocol accepted the access code on a result whose child ran")
		}
	}
	if result, calls := resolve(environment); calls == 0 {
		t.Fatalf("the same access without the pin started nothing: %#v", result)
	}
}

// The statuses the Git CA pin row's two waits matched on kind: the refusal at
// generation 1, and the plan once the source selected the CA, at generation 2.
const (
	gitCAPinRefusedReading   = "git-ca-pin-refused-schema.json"
	gitCAPinRecoveredReading = "git-ca-pin-recovered-schema.json"
)

// gitCAPinMutantMessage is what ReconciliationFailed said on a kind run of
// the same row with the runner's CA pin check removed: Ptah started with the
// credential and failed its handshake under the system roots, before the
// server saw a request.
const gitCAPinMutantMessage = `child_exit: ptah exited with code 2: list the refs of git+https://e2e-git.ptah-test-a-1-36-4-local-e269ddc069-85945-1464fa9372.svc.cluster.local/acme/app.git: Get "https://e2e-git.ptah-test-a-1-36-4-local-e269ddc069-85945-1464fa9372.svc.cluster.local/acme/app.git/info/refs?service=git-upload-pack": tls: failed to verify certificate: x509: certificate signed by unknown authority`

func TestGitCAPinRefusedNeedsTheRunnersRefusalAtTheGeneration(t *testing.T) {
	t.Parallel()
	refused, generation := readReachabilityReading(t, gitCAPinRefusedReading)
	if err := gitCAPinRefused(refused, generation); err != nil {
		t.Fatal(err)
	}
	if recovered, recoveredGeneration := readReachabilityReading(t, gitCAPinRecoveredReading); gitCAPinRefused(recovered, recoveredGeneration) == nil {
		t.Error("gitCAPinRefused accepted the schema once its source selected the CA")
	}
	failed := ptahv1alpha1.ConditionReconciliationFailed
	for name, change := range map[string]func(*metav1.Condition){
		"not failed": func(condition *metav1.Condition) {
			condition.Status = metav1.ConditionFalse
		},
		"a refusal of an older generation": func(condition *metav1.Condition) {
			condition.ObservedGeneration--
		},
		// What the manager writes for a source it refuses before any Job.
		"the reason of a source the manager refused": func(condition *metav1.Condition) {
			condition.Reason = string(ptahv1alpha1.ReasonConfigurationError)
		},
		// The same words from a fetch, which reports under its own code.
		"the pin refused by a fetch": func(condition *metav1.Condition) {
			condition.Message = strings.Replace(condition.Message, gitAccessRefused, runner.CodeGitSourceRefused+": ", 1)
		},
		"another access the runner refused": func(condition *metav1.Condition) {
			condition.Message = gitAccessRefused + "the Git credential is not granted for the source's host"
		},
		"the known hosts pin": func(condition *metav1.Condition) {
			condition.Message = gitAccessRefused + "a Git known hosts digest grant was supplied without the known hosts: " +
				"the credential's Secret pins knownHostsSHA256, so the source has to select it in transport.knownHostsFrom"
		},
		"a CA other than the one granted": func(condition *metav1.Condition) {
			condition.Message = gitAccessRefused + "the Git certificate authority is not the one the credential's owner granted"
		},
		// What the schema said on kind with the runner's check removed: Ptah
		// ran with the credential and failed its handshake under the system
		// roots.
		"the handshake Ptah failed without the CA": func(condition *metav1.Condition) {
			condition.Message = gitCAPinMutantMessage
		},
	} {
		if gitCAPinRefused(changedCondition(t, refused, failed, change), generation) == nil {
			t.Errorf("gitCAPinRefused accepted %s", name)
		}
	}
	if gitCAPinRefused(nil, generation) == nil {
		t.Error("gitCAPinRefused accepted no condition")
	}
}

func TestGitCAPinClearedNeedsTheFailureClearedAndTheCommitAtTheGeneration(t *testing.T) {
	t.Parallel()
	recovered, generation := readReachabilityReading(t, gitCAPinRecoveredReading)
	if err := gitCAPinCleared(recovered, generation); err != nil {
		t.Fatal(err)
	}
	refused, _ := readReachabilityReading(t, gitCAPinRefusedReading)
	failed, resolved := ptahv1alpha1.ConditionReconciliationFailed, ptahv1alpha1.ConditionArtifactResolved
	for name, conditions := range map[string][]metav1.Condition{
		"the refused schema": refused,
		"no condition":       nil,
		"still failed": changedCondition(t, recovered, failed, func(condition *metav1.Condition) {
			condition.Status = metav1.ConditionTrue
		}),
		"cleared at an older generation": changedCondition(t, recovered, failed, func(condition *metav1.Condition) {
			condition.ObservedGeneration--
		}),
		"cleared by a settled proof": changedCondition(t, recovered, failed, func(condition *metav1.Condition) {
			condition.Reason = string(ptahv1alpha1.ReasonProofSettled)
		}),
		"no commit resolved": changedCondition(t, recovered, resolved, func(condition *metav1.Condition) {
			condition.Status = metav1.ConditionFalse
		}),
		"an HTTPS download resolved": changedCondition(t, recovered, resolved, func(condition *metav1.Condition) {
			condition.Reason = string(ptahv1alpha1.ReasonDownloadVerified)
		}),
		"a commit resolved at an older generation": changedCondition(t, recovered, resolved, func(condition *metav1.Condition) {
			condition.ObservedGeneration--
		}),
		"the refusal left on another condition": changedCondition(t, recovered, ptahv1alpha1.ConditionPlanReady,
			func(condition *metav1.Condition) {
				condition.Message = gitAccessRefused + gitCAPinRefusal
			}),
	} {
		if gitCAPinCleared(conditions, generation) == nil {
			t.Errorf("gitCAPinCleared accepted %s", name)
		}
	}
}
