package examples_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guides and examples describe an operator that serves two families, and
// nothing made them stay that way. Each inconsistency in stokaro/ptah-operator#194
// was a page written when there was one family and left behind when the second
// arrived, so this derives the families from what the generator produced rather
// than from a list someone has to remember to extend.
//
// A third family would add CRDs here and fail these, which is the point: a
// reader following a starting point that covers half the operator finds out
// when it matters, and that is the worst time.
func servedResources(t *testing.T) []string {
	t.Helper()

	entries, err := filepath.Glob(filepath.Join("..", "config", "crd", "bases", "operator.ptah.run_*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no generated CRDs were found, so this check derives nothing")
	}
	resources := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSuffix(filepath.Base(entry), ".yaml")
		_, plural, found := strings.Cut(name, "operator.ptah.run_")
		if !found || plural == "" {
			t.Fatalf("%s is not a generated CRD filename", entry)
		}
		resources = append(resources, plural)
	}
	return resources
}

// desiredStateResources are the kinds a person writes to ask for work. They are
// the ones an author Role has to grant; plans and approvals are not authored.
func desiredStateResources(t *testing.T) []string {
	t.Helper()

	var authored []string
	for _, resource := range servedResources(t) {
		if strings.HasSuffix(resource, "plans") || strings.HasSuffix(resource, "approvals") {
			continue
		}
		authored = append(authored, resource)
	}
	if len(authored) < 2 {
		t.Fatalf("derived %v as desired-state kinds, and this operator serves two families", authored)
	}
	return authored
}

func grantsGroup(groups []string, want string) bool {
	for _, group := range groups {
		if group == want {
			return true
		}
	}
	return false
}

func TestTheDiagnosticReaderCoversEveryKindTheOperatorServes(t *testing.T) {
	role, _ := readRoleExample(t, "diagnostic-reader-role.yaml")

	granted := map[string]bool{}
	for _, rule := range role.Rules {
		if !grantsGroup(rule.APIGroups, "operator.ptah.run") {
			continue
		}
		for _, resource := range rule.Resources {
			granted[resource] = true
		}
	}
	for _, resource := range servedResources(t) {
		if !granted[resource] {
			t.Fatalf("the diagnostic reader cannot read %s, so it diagnoses part of this operator", resource)
		}
	}
}

func TestTheDesiredStateAuthorCoversEveryFamilyAPersonWrites(t *testing.T) {
	role, _ := readRoleExample(t, "desired-state-author-role.yaml")

	granted := map[string]bool{}
	for _, rule := range role.Rules {
		if !grantsGroup(rule.APIGroups, "operator.ptah.run") {
			continue
		}
		for _, resource := range rule.Resources {
			granted[resource] = true
		}
	}
	for _, resource := range desiredStateResources(t) {
		if !granted[resource] {
			t.Fatalf("the desired-state author cannot write %s, which is a family this operator serves", resource)
		}
	}
	// And it grants nothing else: an author Role that reached a plan or an
	// approval would be the separation this example exists to start.
	for resource := range granted {
		if strings.HasSuffix(resource, "plans") || strings.HasSuffix(resource, "approvals") {
			t.Fatalf("the desired-state author reaches %s, which belongs to the approver", resource)
		}
	}
}

// The operations guide counts the CRDs a reader has to preserve across an
// uninstall. A count written as a word goes stale silently.
func TestTheOperationsGuideCountsEveryCRD(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "docs", "site", "src", "content",
		"docs", "use", "operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	spelled := map[int]string{3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight"}
	served := len(servedResources(t))
	want, ok := spelled[served]
	if !ok {
		t.Fatalf("this operator serves %d kinds and the check cannot spell that", served)
	}
	page := string(contents)
	if !strings.Contains(page, want+" CRDs") {
		t.Fatalf("the operations guide never says %q, so its uninstall procedure counts something else", want+" CRDs")
	}
	for count, word := range spelled {
		if count == served {
			continue
		}
		if strings.Contains(page, word+" CRDs") {
			t.Fatalf("the operations guide says %q while this operator serves %d kinds", word+" CRDs", served)
		}
	}
}
