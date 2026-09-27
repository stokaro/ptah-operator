package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const apiCompatibilityPage = "docs/site/src/content/docs/support/api-compatibility.md"

// The compatibility page promises only what something refuses, so every rule
// it states has to have its refusal still in the tree.
//
// A page that describes a gate nobody runs is worse than no page: a reader
// deploying against it believes a stored object is protected by a check that
// was deleted. Each row here names the refusal in the code, and the page is
// held to the code rather than to its own earlier draft.
func TestTheCompatibilityPagePromisesOnlyWhatIsEnforced(t *testing.T) {
	t.Parallel()
	page := readCompatibilityPage(t)
	for _, rule := range []struct {
		stated  string
		source  string
		refusal string
	}{
		{
			stated:  "or a new field that arrives already required, without a default",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "was optional and the candidate requires it, with no default to fill it in",
		},
		{
			stated:  "An enum that lost a value",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "the enum lost ",
		},
		{
			stated:  "A numeric or length bound that tightened",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "rose from ",
		},
		{
			stated:  "A pattern that changed, in either direction",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "the pattern changed from ",
		},
		{
			stated:  "A new or changed `x-kubernetes-validations` rule",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "x-kubernetes-validations gained or changed the rule ",
		},
		{
			stated:  "A changed `x-kubernetes-list-type` or `x-kubernetes-map-type`",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "x-kubernetes-list-type changed from ",
		},
		{
			stated:  "A default that appeared, changed, or was taken away",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "changed its default from ",
		},
		{
			stated:  "removing a field",
			source:  filepath.Join("hack", "crdschemahistory", "compatibility.go"),
			refusal: "A field the candidate no longer declares is not in this list",
		},
		{
			stated:  "strictly increase",
			source:  filepath.Join("hack", "crdschemahistory", "verifier.go"),
			refusal: "must strictly increase baseline version",
		},
		{
			stated:  "exactly equal",
			source:  filepath.Join("hack", "crdschemahistory", "verifier.go"),
			refusal: "must equal baseline version",
		},
		{
			stated:  "release sequence one above",
			source:  filepath.Join("charts", "ptah-operator", "templates", "_helpers.tpl"),
			refusal: "is newer than candidate",
		},
		{
			stated:  "refuses stored state newer than that release reads",
			source:  filepath.Join("internal", "crdupgrade", "runtime.go"),
			refusal: "controller downgrade refused: ",
		},
		{
			stated:  "refuses a set that added or removed a kind",
			source:  filepath.Join("hack", "crdschemahistory", "verifier.go"),
			refusal: "added or removed CRDs require a separately reviewed migration",
		},
	} {
		if !strings.Contains(page, rule.stated) {
			t.Errorf("%s no longer states %q", apiCompatibilityPage, rule.stated)
			continue
		}
		source := readRepositoryTextFile(t, rule.source)
		if !strings.Contains(source, rule.refusal) {
			t.Errorf("%s states %q, and %s no longer refuses it (%q)",
				apiCompatibilityPage, rule.stated, rule.source, rule.refusal)
		}
	}

	// The annotation the page names is the one the operator reads.
	if !strings.Contains(page, crdupgrade.ControllerStateVersionAnnotation) {
		t.Errorf("%s does not name %s, which is the annotation the fence reads",
			apiCompatibilityPage, crdupgrade.ControllerStateVersionAnnotation)
	}
	if !strings.Contains(page, crdupgrade.SchemaVersionAnnotation) {
		t.Errorf("%s does not name %s, which is the annotation the verifier stamps",
			apiCompatibilityPage, crdupgrade.SchemaVersionAnnotation)
	}
}

func readCompatibilityPage(t *testing.T) string {
	t.Helper()
	page := readRepositoryTextFile(t, filepath.FromSlash(apiCompatibilityPage))
	return strings.Join(strings.Fields(page), " ")
}

func readRepositoryTextFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, path)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
