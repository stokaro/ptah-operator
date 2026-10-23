package e2e

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	batchv1 "k8s.io/api/batch/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

// The Git source phase's fixtures. It runs on the data plane's PostgreSQL
// server, on databases no other phase touches, and serves its repository from
// the fixture image inside the test namespace.
const (
	gitSchemaDatabase    = "ptah_e2e_git_schema"
	gitMigrationDatabase = "ptah_e2e_git_migrations"
	gitSchema            = "e2e-git-schema"
	gitRefusedSchema     = "e2e-git-schema-refused"
	gitMigration         = "e2e-git-migration"
	gitSchemaSecret      = "e2e-git-schema-database"
	gitMigrationSecret   = "e2e-git-migration-database"

	// gitPinnedSchema reads one commit by its id, into a database of its own.
	gitPinnedSchema   = "e2e-git-pinned"
	gitPinnedDatabase = "ptah_e2e_git_pinned"
	gitPinnedSecret   = "e2e-git-pinned-database"
	// The sources Ptah refuses to read: a submodule, an LFS pointer, and a
	// server whose host key is not the one the known hosts name.
	gitSubmoduleSchema = "e2e-git-submodule"
	gitLFSSchema       = "e2e-git-lfs"
	gitHostKeySchema   = "e2e-git-host-key"

	// gitServer is the fixture Deployment and Service; gitServerSecret holds
	// its TLS key, token and SSH keys, and gitSpecConfigMap the repository.
	gitServer        = "e2e-git"
	gitServerSecret  = "e2e-git-server"
	gitSpecConfigMap = "e2e-git-spec"
	gitRepository    = "acme/app.git"

	// The credential Secrets and trust ConfigMaps the resources select.
	gitTokenCredential     = "e2e-git-token"
	gitElsewhereCredential = "e2e-git-token-elsewhere"
	gitDeployKeyCredential = "e2e-git-deploy-key"
	gitCAConfigMap         = "e2e-git-ca"
	gitKnownHostsConfigMap = "e2e-git-known-hosts"
	// gitOtherHostsConfigMap pins a host key the server does not hold, and
	// gitOtherHostsCredential grants the deploy key under it.
	gitOtherHostsConfigMap  = "e2e-git-known-hosts-other"
	gitOtherHostsCredential = "e2e-git-deploy-key-other-hosts"

	gitSchemaPath    = "db/schema.sql"
	gitMigrationPath = "db/migrations"
	gitFetchInit     = "fetch-git-source"
	// gitInterval is long enough that no periodic refresh lands inside the
	// phase: every Resolve the phase sees is one it caused.
	gitInterval = "1h"
)

// What only the refused sources select: a submodule that links to a commit
// this repository does not hold, as a real one would not, and a Git LFS
// pointer file as Git LFS writes one.
const (
	gitSubmodulePath   = "db/vendor"
	gitSubmoduleCommit = "0123456789abcdef0123456789abcdef01234567"
	gitLFSPath         = "db/lfs.sql"
	gitLFSPointer      = "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"
)

// gitSchemaRevisions are the desired schema the phase declares, in order: a
// table, then a column on it, then, after the history is rewritten, a second
// column.
var gitSchemaRevisions = []string{
	"CREATE TABLE e2e_git_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL\n);\n",
	"CREATE TABLE e2e_git_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL,\n  sku text\n);\n",
	"CREATE TABLE e2e_git_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL,\n  sku text,\n  code text\n);\n",
}

var gitMigrationFiles = map[string]string{
	"0000000001_create_parts.up.sql":   "CREATE TABLE e2e_git_parts (\n    id INTEGER PRIMARY KEY,\n    name TEXT NOT NULL\n);\n",
	"0000000001_create_parts.down.sql": "DROP TABLE e2e_git_parts;\n",
}

// gitRepositorySpec is the repository the server holds, with main at the
// commit at index main. Commit 0 creates the table, commit 1 changes only a
// file no resource selects, and commit 2 adds the column; the tag v1 names
// commit 0, whose migration directory the migration reads. Every commit also
// holds a submodule and an LFS pointer, which only the refused sources select.
func gitRepositorySpec(main int) gitrepo.Spec {
	return gitrepo.Spec{Commits: gitCommits(false), Branches: map[string]int{"main": main}, Tags: map[string]int{"v1": 0}}
}

// gitRewrittenSpec is the repository after a force-push: commit 2 is replaced
// by one that adds a second column, main names the replacement, and the
// replaced commit is no longer on the server.
func gitRewrittenSpec() gitrepo.Spec {
	return gitrepo.Spec{Commits: gitCommits(true), Branches: map[string]int{"main": 2}, Tags: map[string]int{"v1": 0}}
}

func gitCommits(rewritten bool) []gitrepo.CommitSpec {
	files := func(schema, readme string) map[string]string {
		selected := map[string]string{gitSchemaPath: schema, "README": readme, gitLFSPath: gitLFSPointer}
		for name, content := range gitMigrationFiles {
			selected[gitMigrationPath+"/"+name] = content
		}
		return selected
	}
	submodules := map[string]string{gitSubmodulePath: gitSubmoduleCommit}
	last := gitrepo.CommitSpec{Message: "Add the sku column", Files: files(gitSchemaRevisions[1], "second\n"), Submodules: submodules}
	if rewritten {
		last = gitrepo.CommitSpec{Message: "Add the sku and code columns", Files: files(gitSchemaRevisions[2], "second\n"), Submodules: submodules}
	}
	return []gitrepo.CommitSpec{
		{Message: "Create the items table", Files: files(gitSchemaRevisions[0], "first\n"), Submodules: submodules},
		{Message: "Say more in the README", Files: files(gitSchemaRevisions[0], "second\n"), Submodules: submodules},
		last,
	}
}

func gitSpecDocument(spec gitrepo.Spec) (string, error) {
	data, err := json.Marshal(spec)
	return string(data), err
}

// gitSchemaContent is the content digest of a schema revision as the
// operator binds it: one file, by its own name.
func gitSchemaContent(revision int) (string, error) {
	return sourcecontent.Digest([]sourcecontent.File{{Path: "schema.sql", Data: []byte(gitSchemaRevisions[revision])}})
}

func gitMigrationContent() (string, error) {
	files := make([]sourcecontent.File, 0, len(gitMigrationFiles))
	for name, content := range gitMigrationFiles {
		files = append(files, sourcecontent.File{Path: name, Data: []byte(content)})
	}
	return sourcecontent.Digest(files)
}

// gitResolvedTo holds a schema's Git status to the commit and content the
// phase expects, and to the repository, ref and path it asked for.
func gitResolvedTo(status *ptahv1alpha1.GitSourceRevision, url, path string, ref ptahv1alpha1.GitRef, commit, content string) error {
	switch {
	case status == nil:
		return errors.New("no Git resolution is recorded")
	case status.URL != url || status.Path != path || status.Ref != ref:
		return fmt.Errorf("the resolution answered %s %s %+v, not %s %s %+v", status.URL, status.Path, status.Ref, url, path, ref)
	case status.Commit != commit:
		return fmt.Errorf("the resolution read commit %s, not %s", status.Commit, commit)
	case status.ContentDigest != content:
		return fmt.Errorf("the resolution read content %s, not %s", status.ContentDigest, content)
	}
	return nil
}

// gitPlanBound holds a plan to the Git source it was computed from: contract
// version 2, the repository and path as the revision, and the content, and
// nothing of an OCI artifact or a verification policy.
func gitPlanBound(contract int32, artifactDigest, policyUID string, source *ptahv1alpha1.PlanSourceBinding, url, path, content string) error {
	switch {
	case contract != 2:
		return fmt.Errorf("contract version %d, want 2", contract)
	case artifactDigest != "" || policyUID != "":
		return errors.New("the plan binds an artifact or a verification policy")
	case source == nil || source.Kind != ptahv1alpha1.SourceKindGit:
		return fmt.Errorf("the plan binds the source %+v, not a Git one", source)
	case source.Revision != sourcebinding.GitRevision(url, path):
		return errors.New("the plan's revision is not its repository and path")
	case source.ContentDigest != content:
		return fmt.Errorf("the plan binds content %s, not %s", source.ContentDigest, content)
	}
	return nil
}

// gitFetchJob holds an operation Job that reads a Git source to its shape:
// the runner install and the Git fetch before the operation container, the
// fetch holding the credential and one of the commits the resource resolved
// to, and the operation container holding neither and mounting the fetched
// files read-only.
func gitFetchJob(job *batchv1.Job, commits ...string) error {
	spec := job.Spec.Template.Spec
	names := make([]string, 0, len(spec.InitContainers))
	for _, container := range spec.InitContainers {
		names = append(names, container.Name)
	}
	if !slices.Equal(names, []string{"install-runner", gitFetchInit}) {
		return fmt.Errorf("init containers %v, want the runner install and the Git fetch", names)
	}
	fetch := spec.InitContainers[1]
	committed, credential := false, false
	for _, variable := range fetch.Env {
		switch {
		case variable.Name == "PTAH_OPERATOR_GIT_COMMIT" && slices.Contains(commits, variable.Value):
			committed = true
		case variable.Name == "PTAH_OPERATOR_GIT_HOST_GRANT" && variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil:
			credential = true
		}
	}
	if !committed || !credential {
		return fmt.Errorf("the fetch carries commit=%v and a credential grant=%v", committed, credential)
	}
	if len(spec.Containers) != 1 {
		return fmt.Errorf("%d operation containers", len(spec.Containers))
	}
	for _, variable := range spec.Containers[0].Env {
		if strings.HasPrefix(variable.Name, "PTAH_GIT_") || strings.HasPrefix(variable.Name, "PTAH_OPERATOR_GIT_") {
			return fmt.Errorf("the operation container carries %s", variable.Name)
		}
	}
	for _, mount := range spec.Containers[0].VolumeMounts {
		switch mount.Name {
		case "git-credentials", "git-ca", "git-known-hosts", "fetch-work":
			return fmt.Errorf("the operation container mounts %s", mount.Name)
		case "schema-source":
			if !mount.ReadOnly {
				return errors.New("the operation container can write the fetched files")
			}
		}
	}
	for _, volume := range spec.Volumes {
		if volume.Name == "source-snapshot" || volume.Name == "registry-docker-config" {
			return fmt.Errorf("the Job carries the volume %s", volume.Name)
		}
	}
	return nil
}

// gitResolveJob holds a Resolve Job of a Git source to its shape: no init
// container but the runner install, and the repository in its operation
// container, which holds no database credential.
func gitResolveJob(job *batchv1.Job) error {
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "install-runner" {
		return errors.New("a Resolve Job fetches nothing before its operation")
	}
	repository, database := false, false
	for _, variable := range spec.Containers[0].Env {
		repository = repository || variable.Name == "PTAH_OPERATOR_GIT_URL"
		database = database || variable.Name == "PTAH_DB_URL"
	}
	if !repository || database {
		return fmt.Errorf("the Resolve Job names the repository=%v and a database=%v", repository, database)
	}
	return nil
}

// gitServerKeys is everything the phase generates for its Git server: a CA
// and a certificate for the Service's names, the token, the server's SSH host
// key and the one client key it authorizes, and the known-hosts line that
// pins the host key.
type gitServerKeys struct {
	caPEM, certificatePEM, keyPEM []byte
	token                         string
	hostKeyPEM, clientKeyPEM      []byte
	authorizedKey, knownHosts     []byte
	// otherKnownHosts pins a host key the server does not hold.
	otherKnownHosts []byte
}

func newGitServerKeys(host string, names ...string) (gitServerKeys, error) {
	var keys gitServerKeys
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return keys, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Ptah e2e Git CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		return keys, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return keys, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return keys, err
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, serverKey.Public(), caKey)
	if err != nil {
		return keys, err
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return keys, err
	}
	keys.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keys.certificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keys.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return keys, err
	}
	keys.token = hex.EncodeToString(token)
	sshKey := func() ([]byte, ssh.PublicKey, error) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		block, err := ssh.MarshalPrivateKey(private, "")
		if err != nil {
			return nil, nil, err
		}
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			return nil, nil, err
		}
		return pem.EncodeToMemory(block), signer.PublicKey(), nil
	}
	hostKeyPEM, hostPublic, err := sshKey()
	if err != nil {
		return keys, err
	}
	clientKeyPEM, clientPublic, err := sshKey()
	if err != nil {
		return keys, err
	}
	keys.hostKeyPEM, keys.clientKeyPEM = hostKeyPEM, clientKeyPEM
	keys.authorizedKey = ssh.MarshalAuthorizedKey(clientPublic)
	keys.knownHosts = []byte(knownhosts.Line([]string{host}, hostPublic) + "\n")
	_, otherPublic, err := sshKey()
	if err != nil {
		return keys, err
	}
	keys.otherKnownHosts = []byte(knownhosts.Line([]string{host}, otherPublic) + "\n")
	return keys, nil
}

// grantDigest is the "sha256:" digest a credential Secret grants trust by.
func grantDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
