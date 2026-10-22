package gitrepo_test

import (
	"testing"

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
