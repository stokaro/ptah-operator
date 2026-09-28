package examples_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestDesiredStateAuthorRoleIsSeparateAndNamespaceScoped(t *testing.T) {
	role, binding := readRoleExample(t, "desired-state-author-role.yaml")
	if role.Namespace != "application" || binding.Namespace != role.Namespace {
		t.Fatalf("desired-state author namespace = %q/%q, want application", role.Namespace, binding.Namespace)
	}
	wantRules := []rbacv1.PolicyRule{{
		APIGroups: []string{"operator.ptah.run"},
		Resources: []string{"ptahschemas", "ptahmigrations"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	}}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("desired-state author rules = %#v, want %#v", role.Rules, wantRules)
	}
	assertGroupBinding(t, role, binding, "<desired-state-author-group>")
}

func TestDiagnosticReaderRoleCannotChangeStateOrReadPlansOrCredentials(t *testing.T) {
	role, binding := readRoleExample(t, "diagnostic-reader-role.yaml")
	if role.Namespace != "application" || binding.Namespace != role.Namespace {
		t.Fatalf("diagnostic reader namespace = %q/%q, want application", role.Namespace, binding.Namespace)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"operator.ptah.run"}, Resources: []string{
			"ptahschemas", "ptahschemaplans", "ptahschemaapprovals",
			"ptahmigrations", "ptahmigrationplans", "ptahmigrationapprovals",
			"ptahmigrationrunacknowledgments",
		}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods", "pods/log"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"get", "list", "watch"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("diagnostic reader rules = %#v, want %#v", role.Rules, wantRules)
	}
	if violations := diagnosticReaderViolations(role.Rules); len(violations) != 0 {
		t.Fatalf("the diagnostic reader reaches what it exists not to reach: %v", violations)
	}
	assertGroupBinding(t, role, binding, "<diagnostic-reader-group>")
}

// The exact-rules comparison above fails on any edit, and says nothing about
// why a rule was left out. This check is the reason, so it has to refuse each
// grant it is there for.
//
// pods/log left this table with stokaro/ptah-operator#449: the Plan Job's
// runner now seals its plan payload to the manager's own key before writing
// its frame, so a Plan Pod's log holds ciphertext rather than the plan, and
// granting a diagnostic reader that log is no longer granting it the plan.
// pods/attach and pods/exec stay refused for a reason sealing does not touch:
// a live process inside an Observe, Plan or Apply Pod still holds the target
// database credential in its environment.
func TestTheDiagnosticReaderCheckRefusesPlanAndCredentialAccess(t *testing.T) {
	read := []string{"get", "list", "watch"}
	for name, rule := range map[string]rbacv1.PolicyRule{
		"Pod attach":          {APIGroups: []string{""}, Resources: []string{"pods/attach"}, Verbs: []string{"get"}},
		"Pod exec":            {APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"get"}},
		"every Pod resource":  {APIGroups: []string{""}, Resources: []string{"pods/*"}, Verbs: read},
		"plan chunks":         {APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahschemaplanchunks"}, Verbs: read},
		"plan projections":    {APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: read},
		"credentials":         {APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
		"every resource":      {APIGroups: []string{""}, Resources: []string{"*"}, Verbs: read},
		"an approval write":   {APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahschemaapprovals"}, Verbs: []string{"create"}},
		"a desired-state set": {APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahmigrations"}, Verbs: []string{"patch"}},
		"every verb":          {APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"*"}},
	} {
		t.Run(name, func(t *testing.T) {
			if violations := diagnosticReaderViolations([]rbacv1.PolicyRule{rule}); len(violations) == 0 {
				t.Fatalf("the check admits %#v", rule)
			}
		})
	}
}

// diagnosticReaderRefuses maps each resource a diagnostic reader must not reach
// to what it would read. pods/log is deliberately absent: the sealed plan
// payload made it safe to grant, and the exact-rules comparison in
// TestDiagnosticReaderRoleCannotChangeStateOrReadPlansOrCredentials is what
// holds the example role to granting exactly that and nothing more. Attach and
// exec remain refused because either reaches a live Pod's database credential,
// which sealing the plan payload does not change.
var diagnosticReaderRefuses = map[string]string{
	"secrets":              "database and registry credentials",
	"ptahschemaplanchunks": "a plan's SQL",
	"configmaps":           "the plan an Apply projects into its Pod",
	"pods/attach":          "a live Pod's database credential",
	"pods/exec":            "a live Pod's database credential",
	"pods/*":               "every Pod stream",
	"*":                    "every resource",
}

func diagnosticReaderViolations(rules []rbacv1.PolicyRule) []string {
	var violations []string
	for _, rule := range rules {
		for _, resource := range rule.Resources {
			if what, refused := diagnosticReaderRefuses[resource]; refused {
				violations = append(violations, fmt.Sprintf("%s reads %s", resource, what))
			}
		}
		for _, verb := range rule.Verbs {
			switch verb {
			case "create", "update", "patch", "delete", "deletecollection", "*":
				violations = append(violations, fmt.Sprintf("%s on %v changes state", verb, rule.Resources))
			}
		}
	}
	return violations
}

// Reading a plan takes one rule, on the operator's own kinds, in the namespace
// the plans are in. It used to take a Role naming every chunk ConfigMap of the
// one plan under review, rewritten for the next plan; a rule that names a
// ConfigMap now reaches no plan chunk, only application configuration and the
// plans an Apply ran.
func TestThePlanReaderRoleReadsEveryPlanThroughOneRule(t *testing.T) {
	role, binding := readRoleExample(t, "approver-plan-reader-role.yaml")
	if role.Namespace != "application" || binding.Namespace != role.Namespace {
		t.Fatalf("plan reader namespace = %q/%q, want application", role.Namespace, binding.Namespace)
	}
	wantRules := []rbacv1.PolicyRule{{
		APIGroups: []string{"operator.ptah.run"},
		Resources: []string{"ptahschemas", "ptahschemaplans", "ptahschemaplanchunks"},
		Verbs:     []string{"get"},
	}}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("plan reader rules = %#v, want %#v", role.Rules, wantRules)
	}
	for _, rule := range role.Rules {
		if len(rule.ResourceNames) != 0 {
			t.Fatalf("plan reader rule %#v names objects, so it goes stale with the next plan", rule)
		}
	}
	assertGroupBinding(t, role, binding, "<plan-reviewer-group>")
}

func readRoleExample(t *testing.T, path string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(content))
	var role *rbacv1.Role
	var binding *rbacv1.RoleBinding
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "Role":
			if role != nil {
				t.Fatal("example contains more than one Role")
			}
			role = &rbacv1.Role{}
			if err := json.Unmarshal(raw, role); err != nil {
				t.Fatal(err)
			}
		case "RoleBinding":
			if binding != nil {
				t.Fatal("example contains more than one RoleBinding")
			}
			binding = &rbacv1.RoleBinding{}
			if err := json.Unmarshal(raw, binding); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("example contains unexpected kind %q", typeMeta.Kind)
		}
	}
	if role == nil || binding == nil {
		t.Fatalf("example must contain one Role and one RoleBinding: role=%v binding=%v", role != nil, binding != nil)
	}
	return role, binding
}

// readClusterRoleExample reads an example holding one ClusterRole and the one
// ClusterRoleBinding that grants it.
func readClusterRoleExample(t *testing.T, path string) (*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding) {
	t.Helper()
	var role *rbacv1.ClusterRole
	var binding *rbacv1.ClusterRoleBinding
	for _, raw := range exampleDocuments(t, path) {
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "ClusterRole":
			if role != nil {
				t.Fatal("example contains more than one ClusterRole")
			}
			role = &rbacv1.ClusterRole{}
			if err := json.Unmarshal(raw, role); err != nil {
				t.Fatal(err)
			}
		case "ClusterRoleBinding":
			if binding != nil {
				t.Fatal("example contains more than one ClusterRoleBinding")
			}
			binding = &rbacv1.ClusterRoleBinding{}
			if err := json.Unmarshal(raw, binding); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("example contains unexpected kind %q", typeMeta.Kind)
		}
	}
	if role == nil || binding == nil {
		t.Fatalf("example must contain one ClusterRole and one ClusterRoleBinding: role=%v binding=%v", role != nil, binding != nil)
	}
	return role, binding
}

// readRoleRules returns the rules of every Role and ClusterRole in an example,
// whatever else it holds.
func readRoleRules(t *testing.T, path string) []rbacv1.PolicyRule {
	t.Helper()
	var rules []rbacv1.PolicyRule
	for _, raw := range exampleDocuments(t, path) {
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		if typeMeta.Kind != "Role" && typeMeta.Kind != "ClusterRole" {
			continue
		}
		var role struct {
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := json.Unmarshal(raw, &role); err != nil {
			t.Fatal(err)
		}
		rules = append(rules, role.Rules...)
	}
	if len(rules) == 0 {
		t.Fatalf("%s holds no role rules, so there is nothing to check", path)
	}
	return rules
}

func exampleDocuments(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(content))
	var documents []json.RawMessage
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		documents = append(documents, raw)
	}
	return documents
}

func assertClusterGroupBinding(t *testing.T, role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding, group string) {
	t.Helper()
	wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
	if !reflect.DeepEqual(binding.RoleRef, wantRef) {
		t.Fatalf("ClusterRoleBinding roleRef = %#v, want %#v", binding.RoleRef, wantRef)
	}
	wantSubjects := []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: group}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("ClusterRoleBinding subjects = %#v, want %#v", binding.Subjects, wantSubjects)
	}
}

func assertGroupBinding(t *testing.T, role *rbacv1.Role, binding *rbacv1.RoleBinding, group string) {
	t.Helper()
	wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}
	if !reflect.DeepEqual(binding.RoleRef, wantRef) {
		t.Fatalf("RoleBinding roleRef = %#v, want %#v", binding.RoleRef, wantRef)
	}
	wantSubjects := []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: group}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("RoleBinding subjects = %#v, want %#v", binding.Subjects, wantSubjects)
	}
}
