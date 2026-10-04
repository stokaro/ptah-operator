package examples_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
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

// clusterScopedResources are the served kinds that live outside every
// namespace, read from each generated CRD's scope. A namespaced Role cannot
// grant them, and the ones this operator serves are an administrator's: the
// PtahRealm that decides which namespaces may manage a database.
func clusterScopedResources(t *testing.T) map[string]bool {
	t.Helper()

	scoped := map[string]bool{}
	for _, resource := range servedResources(t) {
		contents, err := os.ReadFile(filepath.Join("..", "config", "crd", "bases", "operator.ptah.run_"+resource+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		crd := apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(contents, &crd); err != nil {
			t.Fatalf("parse the %s CRD: %v", resource, err)
		}
		switch crd.Spec.Scope {
		case apiextensionsv1.ClusterScoped:
			scoped[resource] = true
		case apiextensionsv1.NamespaceScoped:
		default:
			t.Fatalf("the %s CRD has scope %q", resource, crd.Spec.Scope)
		}
	}
	return scoped
}

// desiredStateResources are the kinds a person writes to ask for work. They are
// the ones an author Role has to grant; plans and their chunks are not
// authored, approvals and run acknowledgments are decisions about work rather
// than requests for it, and a realm is not work but the authorization for it.
func desiredStateResources(t *testing.T) []string {
	t.Helper()

	clusterScoped := clusterScopedResources(t)
	var authored []string
	for _, resource := range servedResources(t) {
		if isDecisionOrPlan(resource) || resource == "ptahresultrecords" || clusterScoped[resource] {
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
	clusterScoped := clusterScopedResources(t)
	for _, resource := range servedResources(t) {
		// A realm lists every namespace that may manage its database, and a
		// namespace's on-call is not entitled to that list. A namespaced Role
		// could not grant it anyway. A plan chunk is a plan's SQL, which is a
		// reviewer's to read rather than on-call's; the plan manifest says what
		// diagnosis needs about it. Result records also contain confidential SQL
		// and private delivery keys, not ordinary diagnostic state.
		if clusterScoped[resource] || strings.HasSuffix(resource, "planchunks") || resource == "ptahresultrecords" {
			if granted[resource] {
				t.Fatalf("the diagnostic reader names %s, which a namespace's reader must not see", resource)
			}
			continue
		}
		if !granted[resource] {
			t.Fatalf("the diagnostic reader cannot read %s, so it diagnoses part of this operator", resource)
		}
	}
}

// Realm membership is an authorization, so the only example that can write a
// realm is the administrator's, and it can write nothing else. A starting
// point that let an author list their own namespace in a realm would hand
// every tenant the cross-namespace claim a realm exists to withhold.
func TestOnlyTheRealmAdministratorWritesRealms(t *testing.T) {
	clusterScoped := clusterScopedResources(t)
	if len(clusterScoped) == 0 {
		t.Fatal("no served kind is cluster-scoped, so this checks nothing")
	}

	role, binding := readClusterRoleExample(t, "realm-administrator-role.yaml")
	granted := map[string]bool{}
	for _, rule := range role.Rules {
		if !grantsGroup(rule.APIGroups, "operator.ptah.run") {
			t.Fatalf("the realm administrator reaches API groups %v", rule.APIGroups)
		}
		for _, resource := range rule.Resources {
			if !clusterScoped[resource] {
				t.Fatalf("the realm administrator reaches %s, which is desired state rather than a grant", resource)
			}
			granted[resource] = true
		}
	}
	for resource := range clusterScoped {
		if !granted[resource] {
			t.Fatalf("the realm administrator cannot write %s", resource)
		}
	}
	assertClusterGroupBinding(t, role, binding, "<realm-administrator-group>")

	for _, example := range []string{"desired-state-author-role.yaml", "diagnostic-reader-role.yaml", "approver-plan-reader-role.yaml"} {
		for _, rule := range readRoleRules(t, example) {
			for _, resource := range rule.Resources {
				if clusterScoped[resource] || resource == "*" {
					t.Fatalf("%s reaches %s, which only a realm administrator may", example, resource)
				}
			}
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
	// And it grants nothing else: an author Role that reached a plan, its
	// chunks, an approval or a run acknowledgment would be the separation this
	// example exists to start.
	for resource := range granted {
		if isDecisionOrPlan(resource) || resource == "ptahresultrecords" {
			t.Fatalf("the desired-state author reaches %s, which is not desired state", resource)
		}
	}
}

// isDecisionOrPlan reports a kind the operator publishes or an approver
// writes: a plan, an approval of one, or the acknowledgment that settles a run
// nobody accounted for.
func isDecisionOrPlan(resource string) bool {
	return strings.HasSuffix(resource, "plans") || strings.HasSuffix(resource, "planchunks") || strings.HasSuffix(resource, "approvals") ||
		strings.HasSuffix(resource, "acknowledgments")
}

// The operations guide counts the CRDs a reader has to preserve across an
// uninstall. A count written as a word goes stale silently.
func TestTheOperationsGuideCountsEveryCRD(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "docs", "site", "src", "content",
		"docs", "use", "operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	spelled := map[int]string{3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight", 9: "nine", 10: "ten"}
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
