package e2e

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

const (
	testGitURL    = "https://e2e-git.ptah-test.svc/acme/app.git"
	testGitCommit = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
)

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
	database := []corev1.EnvVar{{Name: "PTAH_DB_URL", Value: "from a Secret"}}
	if err := gitFetchJob(gitJobFixture([]string{"install-runner", gitFetchInit}, database, readOnly), testGitCommit); err != nil {
		t.Fatal(err)
	}
	for name, job := range map[string]*batchv1.Job{
		"no fetch":                 gitJobFixture([]string{"install-runner"}, database, readOnly),
		"a snapshot copy":          gitJobFixture([]string{"install-runner", "copy-source-snapshot"}, database, readOnly),
		"a credential in the main": gitJobFixture([]string{"install-runner", gitFetchInit}, append(database, corev1.EnvVar{Name: "PTAH_GIT_PASSWORD"}), readOnly),
		"a writable source":        gitJobFixture([]string{"install-runner", gitFetchInit}, database, []corev1.VolumeMount{{Name: "schema-source", MountPath: "/source"}}),
		"the trust in the main":    gitJobFixture([]string{"install-runner", gitFetchInit}, database, append(readOnly, corev1.VolumeMount{Name: "git-ca", ReadOnly: true})),
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
	if !roots.AppendCertsFromPEM(keys.caPEM) {
		t.Fatal("the CA does not parse")
	}
	block, _ := pem.Decode(keys.certificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := certificate.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
		t.Fatalf("the certificate does not verify for %s: %v", host, err)
	}
	if _, err := certificate.Verify(x509.VerifyOptions{DNSName: "git.example.com", Roots: roots}); err == nil {
		t.Fatal("the certificate verifies for another host")
	}
	hostSigner, err := ssh.ParsePrivateKey(keys.hostKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(file, keys.knownHosts, 0o600); err != nil {
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
	clientSigner, err := ssh.ParsePrivateKey(keys.clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	authorized, _, _, _, err := ssh.ParseAuthorizedKey(keys.authorizedKey)
	if err != nil || string(authorized.Marshal()) != string(clientSigner.PublicKey().Marshal()) {
		t.Fatalf("the authorized key is not the client's: %v", err)
	}
	if callback(host+":22", address, clientSigner.PublicKey()) == nil {
		t.Fatal("the known hosts accept another key")
	}
	// The other known hosts pin a key the server does not hold.
	other := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(other, keys.otherKnownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	otherCallback, err := knownhosts.New(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherCallback(host+":22", address, hostSigner.PublicKey()) == nil {
		t.Fatal("the other known hosts accept the server's host key")
	}
	if keys.token == "" || grantDigest(keys.caPEM) == grantDigest(keys.knownHosts) {
		t.Fatal("the token or the grants are empty")
	}
}
