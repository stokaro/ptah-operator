package gitrepo_test

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

func TestParseSpecRefusesRefsToNothing(t *testing.T) {
	t.Parallel()
	for name, spec := range map[string]string{
		"no commit":        `{"commits": [], "branches": {"main": 0}}`,
		"no main":          `{"commits": [{"message": "m", "files": {"a": "b"}}], "branches": {"dev": 0}}`,
		"a branch too far": `{"commits": [{"message": "m", "files": {"a": "b"}}], "branches": {"main": 1}}`,
		"a negative tag":   `{"commits": [{"message": "m", "files": {"a": "b"}}], "branches": {"main": 0}, "tags": {"v": -1}}`,
		"not JSON":         `commits`,
	} {
		if _, err := gitrepo.ParseSpec([]byte(spec)); err == nil {
			t.Errorf("ParseSpec accepted %s", name)
		}
	}
	spec, err := gitrepo.ParseSpec([]byte(`{"commits": [{"message": "m", "files": {"../a": "b"}}], "branches": {"main": 0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitrepo.Build(spec); err == nil {
		t.Error("Build wrote a file outside the tree")
	}
}

// A submodule is a link in the tree to a commit of another repository, which
// is what Ptah must find at a selected path to refuse it.
func TestBuildWritesASubmoduleAsALink(t *testing.T) {
	t.Parallel()
	linked := "0123456789abcdef0123456789abcdef01234567"
	spec, err := gitrepo.ParseSpec([]byte(`{"commits": [{"message": "m", "files": {"db/schema.sql": "x"}, ` +
		`"submodules": {"db/vendor": "` + linked + `"}}], "branches": {"main": 0}}`))
	if err != nil {
		t.Fatal(err)
	}
	built, err := gitrepo.Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := object.GetCommit(built.Storage(), built.Commit(0))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := tree.FindEntry("db/vendor")
	if err != nil || entry.Mode != filemode.Submodule || entry.Hash.String() != linked {
		t.Fatalf("db/vendor = %+v, %v; want a submodule linking to %s", entry, err, linked)
	}
	for name, document := range map[string]string{
		"a link to no commit id":   `{"commits": [{"message": "m", "files": {"a": "b"}, "submodules": {"v": "main"}}], "branches": {"main": 0}}`,
		"a file and a link at one": `{"commits": [{"message": "m", "files": {"v": "b"}, "submodules": {"v": "` + linked + `"}}], "branches": {"main": 0}}`,
	} {
		spec, err := gitrepo.ParseSpec([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gitrepo.Build(spec); err == nil {
			t.Errorf("Build accepted %s", name)
		}
	}
}
