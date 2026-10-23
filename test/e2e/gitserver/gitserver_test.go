package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh"
)

const testSpec = `{
  "commits": [
    {"message": "create", "files": {"db/schema.sql": "CREATE TABLE a (id int);\n", "README": "one\n"}},
    {"message": "readme", "files": {"db/schema.sql": "CREATE TABLE a (id int);\n", "README": "two\n"}},
    {"message": "column", "files": {"db/schema.sql": "CREATE TABLE a (id int, b int);\n", "README": "two\n", "db/migrations/0001.up.sql": "x\n"}}
  ],
  "branches": {"main": 2, "release": 1},
  "tags": {"v1": 0}
}`

func testServer(t *testing.T, byID bool) (*server, *served) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(file, []byte(testSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{repositoryPath: "/acme/app.git", byID: byID}
	if err := s.reload(file); err != nil {
		t.Fatal(err)
	}
	return s, s.current()
}

func serveHTTPS(t *testing.T, s *server) (string, []byte) {
	t.Helper()
	https := httptest.NewTLSServer(s.handler([]byte("e2e-token")))
	t.Cleanup(https.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: https.Certificate().Raw})
	return https.URL + "/acme/app.git", certificate
}

// fetch clones one ref the way Ptah's pull fetches, shallowly, and returns
// the commit and one file of its tree.
func fetch(t *testing.T, url string, auth transport.AuthMethod, ca []byte, ref plumbing.ReferenceName, depth int, file string) (plumbing.Hash, string, error) {
	t.Helper()
	repository, err := git.CloneContext(context.Background(), memory.NewStorage(), nil, &git.CloneOptions{
		URL: url, Auth: auth, CABundle: ca, ReferenceName: ref, SingleBranch: true, Depth: depth, NoCheckout: true,
		Tags: git.NoTags,
	})
	if err != nil {
		return plumbing.ZeroHash, "", err
	}
	head, err := repository.Reference(ref, true)
	if err != nil {
		return plumbing.ZeroHash, "", err
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		return plumbing.ZeroHash, "", err
	}
	tree, err := commit.Tree()
	if err != nil {
		return plumbing.ZeroHash, "", err
	}
	found, err := tree.File(file)
	if err != nil {
		return plumbing.ZeroHash, "", err
	}
	content, err := found.Contents()
	return head.Hash(), content, err
}

func TestTheServerServesEachRefShallowlyOverHTTPS(t *testing.T) {
	t.Parallel()
	s, repository := testServer(t, false)
	url, ca := serveHTTPS(t, s)
	auth := &githttp.BasicAuth{Username: "e2e", Password: "e2e-token"}
	for _, row := range []struct {
		ref     plumbing.ReferenceName
		depth   int
		commit  int
		content string
	}{
		{plumbing.NewBranchReferenceName("main"), 1, 2, "CREATE TABLE a (id int, b int);\n"},
		{plumbing.NewBranchReferenceName("release"), 1, 1, "CREATE TABLE a (id int);\n"},
		{plumbing.NewTagReferenceName("v1"), 1, 0, "CREATE TABLE a (id int);\n"},
		{plumbing.NewBranchReferenceName("main"), 16, 2, "CREATE TABLE a (id int, b int);\n"},
		{plumbing.NewBranchReferenceName("main"), 0, 2, "CREATE TABLE a (id int, b int);\n"},
	} {
		commit, content, err := fetch(t, url, auth, ca, row.ref, row.depth, "db/schema.sql")
		if err != nil {
			t.Fatalf("fetch %s depth %d: %v", row.ref, row.depth, err)
		}
		if commit != repository.Commit(row.commit) || content != row.content {
			t.Fatalf("%s at depth %d gave %s %q, want %s %q", row.ref, row.depth, commit, content, repository.Commit(row.commit), row.content)
		}
	}
	for name, wrong := range map[string]transport.AuthMethod{
		"no credential":    nil,
		"another password": &githttp.BasicAuth{Username: "e2e", Password: "guess"},
	} {
		if _, _, err := fetch(t, url, wrong, ca, plumbing.NewBranchReferenceName("main"), 1, "db/schema.sql"); err == nil {
			t.Errorf("a client with %s fetched", name)
		}
	}
}

// The same Spec gives the same commit ids, so the phase can name a commit
// before the server holds it.
func TestTheSameSpecGivesTheSameCommits(t *testing.T) {
	t.Parallel()
	_, first := testServer(t, false)
	_, second := testServer(t, false)
	for index := range 3 {
		if first.Commit(index) != second.Commit(index) {
			t.Fatalf("commit %d is %s and %s", index, first.Commit(index), second.Commit(index))
		}
	}
	if first.Commit(0) == first.Commit(1) {
		t.Fatal("two commits share an id")
	}
}

// A commit no ref names is served only with fetching by id on.
func TestACommitNoRefNamesNeedsFetchingByID(t *testing.T) {
	t.Parallel()
	for _, byID := range []bool{false, true} {
		s, repository := testServer(t, byID)
		// release named commit 1; with it gone, no ref does.
		s.mu.Lock()
		delete(s.repository.refs, plumbing.NewBranchReferenceName("release"))
		s.mu.Unlock()
		url, ca := serveHTTPS(t, s)
		want := repository.Commit(1)
		remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{url}})
		err := remote.FetchContext(context.Background(), &git.FetchOptions{
			Auth: &githttp.BasicAuth{Username: "e2e", Password: "e2e-token"}, CABundle: ca, Depth: 1,
			RefSpecs: []config.RefSpec{config.RefSpec(want.String() + ":refs/heads/pinned")}, Tags: git.NoTags,
		})
		if byID && err != nil {
			t.Fatalf("fetching %s by id: %v", want, err)
		}
		if !byID && err == nil {
			t.Fatalf("fetched %s, which no ref names, with fetching by id off", want)
		}
	}
}

func TestTheServerServesOverSSHToItsOneKey(t *testing.T) {
	t.Parallel()
	s, repository := testServer(t, false)
	directory := t.TempDir()
	hostKey := writeKey(t, filepath.Join(directory, "host"))
	clientKey := writeKey(t, filepath.Join(directory, "client"))
	clientSigner, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	authorized := filepath.Join(directory, "authorized")
	if err := os.WriteFile(authorized, ssh.MarshalAuthorizedKey(clientSigner.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := s.sshConfig(filepath.Join(directory, "host"), authorized)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = listener.Close() })
	go s.serveSSH(ctx, listener, config)

	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	url := "ssh://git@" + listener.Addr().String() + "/acme/app.git"
	auth := &gitssh.PublicKeys{User: "git", Signer: clientSigner}
	auth.HostKeyCallback = ssh.FixedHostKey(hostSigner.PublicKey())
	commit, content, err := fetch(t, url, auth, nil, plumbing.NewBranchReferenceName("main"), 1, "db/schema.sql")
	if err != nil || commit != repository.Commit(2) || !strings.Contains(content, "b int") {
		t.Fatalf("ssh fetch gave %s %q: %v", commit, content, err)
	}
	stranger := writeKey(t, filepath.Join(directory, "stranger"))
	strangerSigner, err := ssh.NewSignerFromKey(stranger)
	if err != nil {
		t.Fatal(err)
	}
	refused := &gitssh.PublicKeys{User: "git", Signer: strangerSigner}
	refused.HostKeyCallback = ssh.FixedHostKey(hostSigner.PublicKey())
	if _, _, err := fetch(t, url, refused, nil, plumbing.NewBranchReferenceName("main"), 1, "db/schema.sql"); err == nil {
		t.Fatal("a key the server does not authorize fetched")
	}
}

func writeKey(t *testing.T, file string) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// A tree with a submodule is served with the link and without what it links
// to, which the repository does not hold; a client reads the link.
func TestTheServerSendsASubmoduleAsALink(t *testing.T) {
	t.Parallel()
	linked := "0123456789abcdef0123456789abcdef01234567"
	file := filepath.Join(t.TempDir(), "spec.json")
	spec := `{"commits": [{"message": "m", "files": {"db/schema.sql": "CREATE TABLE a (id int);\n"}, ` +
		`"submodules": {"db/vendor": "` + linked + `"}}], "branches": {"main": 0}}`
	if err := os.WriteFile(file, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{repositoryPath: "/acme/app.git"}
	if err := s.reload(file); err != nil {
		t.Fatal(err)
	}
	url, ca := serveHTTPS(t, s)
	repository, err := git.CloneContext(context.Background(), memory.NewStorage(), nil, &git.CloneOptions{
		URL: url, Auth: &githttp.BasicAuth{Username: "e2e", Password: "e2e-token"}, CABundle: ca,
		ReferenceName: plumbing.NewBranchReferenceName("main"), SingleBranch: true, Depth: 1, NoCheckout: true, Tags: git.NoTags,
	})
	if err != nil {
		t.Fatalf("fetch a tree with a submodule: %v", err)
	}
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := tree.FindEntry("db/vendor")
	if err != nil || entry.Mode != filemode.Submodule || entry.Hash.String() != linked {
		t.Fatalf("db/vendor = %+v, %v; want the link to %s", entry, err, linked)
	}
}
