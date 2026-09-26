package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The chart ships one ClusterRole for people rather than for a process: the
// approver's. It served one family for as long as there was one, and when the
// second arrived nothing failed, because a missing grant is a refusal in a
// cluster and nowhere else (stokaro/ptah-operator#450). So the families it has
// to cover are read from the CRDs the generator produced, and the role is read
// from the chart as Helm renders it.

const (
	approverRelease   = "ptah"
	approverRoleName  = approverRelease + "-ptah-operator-approver"
	operatorAPIGroup  = "operator.ptah.run"
	approvalsSuffix   = "approvals"
	generatedCRDsPath = "config/crd/bases"
)

var approverReadVerbs = []string{"get", "list", "watch"}

func TestTheApproverClusterRoleCoversEveryFamilyAndNothingElse(t *testing.T) {
	t.Parallel()
	served := servedOperatorResources(t)
	role := renderedApproverRole(t)
	if problems := approverRoleProblems(role.Rules, served); len(problems) != 0 {
		t.Fatalf("ClusterRole/%s is not the approver's role:\n%s", role.Name, strings.Join(problems, "\n"))
	}
}

// The check above passes on a role that is right and on a check that stopped
// looking. These are the shapes it exists to refuse, the first of them the
// role this chart shipped before #450.
func TestTheApproverRoleCheckRefusesWhatItExistsToRefuse(t *testing.T) {
	t.Parallel()
	served := servedOperatorResources(t)
	operatorRule := func(verbs []string, resources ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{operatorAPIGroup}, Resources: resources, Verbs: verbs}
	}
	complete := []rbacv1.PolicyRule{
		operatorRule(approverReadVerbs, served...),
		operatorRule([]string{"create"}, approvalResources(served)...),
	}
	if problems := approverRoleProblems(complete, served); len(problems) != 0 {
		t.Fatalf("the check refuses the role it describes: %v", problems)
	}
	for name, rules := range map[string][]rbacv1.PolicyRule{
		"one family only": {
			operatorRule(approverReadVerbs, "ptahschemas", "ptahschemaplans", "ptahschemaapprovals"),
			operatorRule([]string{"create"}, "ptahschemaapprovals"),
		},
		"no migration approval": {
			operatorRule(approverReadVerbs, served...),
			operatorRule([]string{"create"}, "ptahschemaapprovals"),
		},
		"no list on plans": {
			operatorRule([]string{"get", "watch"}, served...),
			operatorRule([]string{"create"}, approvalResources(served)...),
		},
		"plan chunks": append(slices.Clone(complete),
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}),
		"Pod logs": append(slices.Clone(complete),
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}}),
		"an edit of an approval": append(slices.Clone(complete),
			operatorRule([]string{"update"}, "ptahmigrationapprovals")),
		"an edit of desired state": append(slices.Clone(complete),
			operatorRule([]string{"patch"}, "ptahschemas")),
		"a status write": append(slices.Clone(complete),
			operatorRule([]string{"update"}, "ptahschemaapprovals/status")),
		"a wildcard": append(slices.Clone(complete),
			operatorRule([]string{"*"}, "*")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if problems := approverRoleProblems(rules, served); len(problems) == 0 {
				t.Fatalf("the check admits %#v", rules)
			}
		})
	}
}

// The default render is the first test's, which requires exactly one.
func TestTheApproverClusterRoleCanBeLeftOut(t *testing.T) {
	t.Parallel()
	roles := renderedClusterRoles(t, "approverClusterRole.create=false")
	if len(roles) == 0 {
		t.Fatal("the RBAC template renders no ClusterRole, so this reads nothing")
	}
	if slices.ContainsFunc(roles, func(role rbacv1.ClusterRole) bool { return role.Name == approverRoleName }) {
		t.Fatalf("approverClusterRole.create=false still renders ClusterRole/%s", approverRoleName)
	}
}

// approverRoleProblems reports every way rules differ from the approver's
// role: read on every served resource, create on every approval kind, and no
// other grant at all.
func approverRoleProblems(rules []rbacv1.PolicyRule, served []string) []string {
	allowed := map[string][]string{}
	for _, resource := range served {
		allowed[resource] = approverReadVerbs
	}
	for _, resource := range approvalResources(served) {
		allowed[resource] = append(slices.Clone(approverReadVerbs), "create")
	}

	var problems []string
	granted := map[string]map[string]bool{}
	for _, rule := range rules {
		if len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
			problems = append(problems, fmt.Sprintf("a rule the approver's role never carries: %#v", rule))
			continue
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					if group != operatorAPIGroup || !slices.Contains(allowed[resource], verb) {
						problems = append(problems, fmt.Sprintf("grants %s on %s in %q", verb, resource, group))
						continue
					}
					if granted[resource] == nil {
						granted[resource] = map[string]bool{}
					}
					granted[resource][verb] = true
				}
			}
		}
	}
	for _, resource := range served {
		for _, verb := range allowed[resource] {
			if !granted[resource][verb] {
				problems = append(problems, fmt.Sprintf("does not grant %s on %s", verb, resource))
			}
		}
	}
	return problems
}

func approvalResources(served []string) []string {
	var approvals []string
	for _, resource := range served {
		if strings.HasSuffix(resource, approvalsSuffix) {
			approvals = append(approvals, resource)
		}
	}
	return approvals
}

// servedOperatorResources reads the plural of every CRD the generator wrote.
// It refuses fewer than two families, and fewer approval kinds than families,
// because a short list turns every check above into one that passes.
func servedOperatorResources(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repositoryRoot(t), generatedCRDsPath, operatorAPIGroup+"_*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var served []string
	for _, path := range paths {
		content, err := os.ReadFile(path) //nolint:gosec // A path globbed under the repository root.
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Spec struct {
				Group string `json:"group"`
				Names struct {
					Plural string `json:"plural"`
				} `json:"names"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(content, &crd); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if crd.Spec.Group != operatorAPIGroup || crd.Spec.Names.Plural == "" {
			t.Fatalf("%s is not a generated %s CRD", path, operatorAPIGroup)
		}
		served = append(served, crd.Spec.Names.Plural)
	}
	sort.Strings(served)
	if len(served) < 6 || len(approvalResources(served)) < 2 {
		t.Fatalf("read %v from %s, and this operator serves two families of three kinds", served, generatedCRDsPath)
	}
	return served
}

func renderedApproverRole(t *testing.T) rbacv1.ClusterRole {
	t.Helper()
	var found []rbacv1.ClusterRole
	for _, role := range renderedClusterRoles(t) {
		if role.Name == approverRoleName {
			found = append(found, role)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the default render holds %d ClusterRoles named %s, want one", len(found), approverRoleName)
	}
	return found[0]
}

func renderedClusterRoles(t *testing.T, values ...string) []rbacv1.ClusterRole {
	t.Helper()
	helm := helmOrSkip(t)
	arguments := []string{
		"template", approverRelease, repositoryFile(t, "charts/ptah-operator"),
		"--namespace", "ptah-system",
		"--show-only", "templates/rbac.yaml",
	}
	for _, value := range requiredInstallValues {
		arguments = append(arguments, "--set-string", value)
	}
	// --set rather than --set-string: the schema types create as a boolean.
	for _, value := range values {
		arguments = append(arguments, "--set", value)
	}
	output, err := exec.Command(helm, arguments...).CombinedOutput() //nolint:gosec // Arguments are read from the repository.
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	var roles []rbacv1.ClusterRole
	for _, document := range strings.Split(string(output), "\n---\n") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(document), &role); err != nil {
			t.Fatalf("the RBAC render does not parse: %v", err)
		}
		if role.Kind == "ClusterRole" {
			roles = append(roles, role)
		}
	}
	return roles
}
