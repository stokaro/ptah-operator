package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every selector this API embeds carries the description of the Kubernetes
// type it is declared as, and for a Secret or ConfigMap key selector that
// description says the name may be empty for backward compatibility. This API
// refuses exactly that, in a validation on the enclosing object, so a reader
// following the field description could build a target the API rejects.
//
// The generated page now leads with the rule's own message. These are the
// paths that must carry one, and the page is read rather than the generator.
var narrowedSelectors = map[string][]string{
	"reference/ptahmigration.md": {
		"spec.target.urlFrom.name",
		"spec.target.urlFrom.key",
		"spec.artifact.verificationPolicyFrom.name",
		"spec.artifact.verificationPolicyFrom.key",
		"spec.artifact.transport.caFrom.name",
		"spec.artifact.transport.caFrom.key",
	},
	"reference/ptahschema.md": {
		"spec.target.urlFrom.name",
		"spec.target.urlFrom.key",
		"spec.dev.urlFrom.name",
		"spec.dev.urlFrom.key",
		"spec.desired.verificationPolicyFrom.name",
		"spec.desired.verificationPolicyFrom.key",
		"spec.desired.transport.caFrom.name",
		"spec.desired.transport.caFrom.key",
	},
}

const requirementLead = "This resource requires:"

// misleadingGenericText is the sentence the embedded type carries and this API
// contradicts. It may remain -- it describes the type -- but never first.
const misleadingGenericText = "is allowed to be empty"

func TestNarrowedSelectorsLeadWithWhatTheAPIRequires(t *testing.T) {
	t.Parallel()

	for page, paths := range narrowedSelectors {
		contents, err := os.ReadFile(filepath.Join("..", "..", "docs", "site", "src", "content", "docs", page))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			row := tableRow(t, string(contents), page, path)
			if !strings.Contains(row, requirementLead) {
				t.Fatalf("%s row %s does not say what this API requires, so the type's own description stands as the contract:\n%s",
					page, path, row)
			}
			requirement := strings.Index(row, requirementLead)
			if generic := strings.Index(row, misleadingGenericText); generic >= 0 && generic < requirement {
				t.Fatalf("%s row %s says the name may be empty before it says this API refuses that:\n%s",
					page, path, row)
			}
		}
	}
}

// And a field nothing narrows keeps its own description, so the note means
// something where it appears.
func TestUnnarrowedFieldsCarryNoRequirement(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", "docs", "site", "src", "content", "docs",
		"reference", "ptahmigration.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		// A status selector the spec's validations say nothing about.
		"status.activeOperation.target.urlFrom.name",
		// And a selector the schema itself already marks required, which
		// needs no note because its own row does not mislead.
		"spec.artifact.registryAuthFrom.name",
	} {
		row := tableRow(t, string(contents), "reference/ptahmigration.md", path)
		if strings.Contains(row, requirementLead) {
			t.Fatalf("%s carries a requirement no validation names, so the note is decoration:\n%s", path, row)
		}
	}
}

func tableRow(t *testing.T, page, name, path string) string {
	t.Helper()

	prefix := "| `" + path + "` |"
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("%s has no row for %s, so this check reads nothing", name, path)
	return ""
}
