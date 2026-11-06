// Package gitfixture is the Git repository and the keys a test serves through
// test/e2e/gitserver: the git-sources acceptance phase serves them on kind, and
// the runner harness (test/runnerharness) from a container. One definition, so
// the documents the harness captures answer the repository the phase reads.
package gitfixture

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

const (
	// Repository is the repository's path on the server.
	Repository = "acme/app.git"
	// SchemaPath and MigrationPath are what a source selects.
	SchemaPath    = "db/schema.sql"
	MigrationPath = "db/migrations"
	// SubmodulePath and LFSPath are what only the refused sources select: a
	// submodule that links to a commit this repository does not hold, as a
	// real one would not, and a Git LFS pointer file as Git LFS writes one.
	SubmodulePath   = "db/vendor"
	SubmoduleCommit = "0123456789abcdef0123456789abcdef01234567"
	LFSPath         = "db/lfs.sql"
	LFSPointer      = "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"
)

// SchemaRevisions are the desired schema, in order: a table, then a column on
// it, then, after the history is rewritten, a second column.
var SchemaRevisions = []string{
	"CREATE TABLE e2e_git_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL\n);\n",
	"CREATE TABLE e2e_git_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL,\n  sku text\n);\n",
	"CREATE TABLE e2e_git_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL,\n  sku text,\n  code text\n);\n",
}

// MigrationFiles is the migration directory, which no commit changes.
var MigrationFiles = map[string]string{
	"0000000001_create_parts.up.sql":   "CREATE TABLE e2e_git_parts (\n    id INTEGER PRIMARY KEY,\n    name TEXT NOT NULL\n);\n",
	"0000000001_create_parts.down.sql": "DROP TABLE e2e_git_parts;\n",
}

// RepositorySpec is the repository the server holds, with main at the commit at
// index main. Commit 0 creates the table, commit 1 changes only a file no
// resource selects, and commit 2 adds the column; the tag v1 names commit 0,
// whose migration directory the migration reads. Every commit also holds a
// submodule and an LFS pointer, which only the refused sources select.
func RepositorySpec(main int) gitrepo.Spec {
	return gitrepo.Spec{Commits: commits(false), Branches: map[string]int{"main": main}, Tags: map[string]int{"v1": 0}}
}

// RewrittenSpec is the repository after a force-push: commit 2 is replaced by
// one that adds a second column, main names the replacement, and the replaced
// commit is no longer on the server.
func RewrittenSpec() gitrepo.Spec {
	return gitrepo.Spec{Commits: commits(true), Branches: map[string]int{"main": 2}, Tags: map[string]int{"v1": 0}}
}

func commits(rewritten bool) []gitrepo.CommitSpec {
	files := func(schema, readme string) map[string]string {
		selected := map[string]string{SchemaPath: schema, "README": readme, LFSPath: LFSPointer}
		for name, content := range MigrationFiles {
			selected[MigrationPath+"/"+name] = content
		}
		return selected
	}
	submodules := map[string]string{SubmodulePath: SubmoduleCommit}
	last := gitrepo.CommitSpec{Message: "Add the sku column", Files: files(SchemaRevisions[1], "second\n"), Submodules: submodules}
	if rewritten {
		last = gitrepo.CommitSpec{Message: "Add the sku and code columns", Files: files(SchemaRevisions[2], "second\n"), Submodules: submodules}
	}
	return []gitrepo.CommitSpec{
		{Message: "Create the items table", Files: files(SchemaRevisions[0], "first\n"), Submodules: submodules},
		{Message: "Say more in the README", Files: files(SchemaRevisions[0], "second\n"), Submodules: submodules},
		last,
	}
}

// SpecDocument is a spec in the form the server reads.
func SpecDocument(spec gitrepo.Spec) (string, error) {
	data, err := json.Marshal(spec)
	return string(data), err
}

// SchemaContent is the content digest of a schema revision as the operator
// binds it: one file, by its own name.
func SchemaContent(revision int) (string, error) {
	return sourcecontent.Digest([]sourcecontent.File{{Path: "schema.sql", Data: []byte(SchemaRevisions[revision])}})
}

// MigrationContent is the content digest of the migration directory.
func MigrationContent() (string, error) {
	files := make([]sourcecontent.File, 0, len(MigrationFiles))
	for name, content := range MigrationFiles {
		files = append(files, sourcecontent.File{Path: name, Data: []byte(content)})
	}
	return sourcecontent.Digest(files)
}

// Keys is everything a test generates for its Git server: a CA and a
// certificate for the server's names, the token, the server's SSH host key and
// the one client key it authorizes, and the known-hosts line that pins the host
// key. None of it outlives the test.
type Keys struct {
	CAPEM, CertificatePEM, KeyPEM []byte
	Token                         string
	HostKeyPEM, ClientKeyPEM      []byte
	AuthorizedKey, KnownHosts     []byte
	// OtherKnownHosts pins a host key the server does not hold.
	OtherKnownHosts []byte
}

// NewKeys generates keys for a server at host, with a certificate for names.
func NewKeys(host string, names ...string) (Keys, error) {
	var keys Keys
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
	keys.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keys.CertificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keys.KeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return keys, err
	}
	keys.Token = hex.EncodeToString(token)
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
	keys.HostKeyPEM, keys.ClientKeyPEM = hostKeyPEM, clientKeyPEM
	keys.AuthorizedKey = ssh.MarshalAuthorizedKey(clientPublic)
	keys.KnownHosts = []byte(knownhosts.Line([]string{host}, hostPublic) + "\n")
	_, otherPublic, err := sshKey()
	if err != nil {
		return keys, err
	}
	keys.OtherKnownHosts = []byte(knownhosts.Line([]string{host}, otherPublic) + "\n")
	return keys, nil
}
