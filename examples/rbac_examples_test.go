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
		}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
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
// grant it is there for, including the pods/log rule the example carried until
// stokaro/ptah-operator#449.
func TestTheDiagnosticReaderCheckRefusesPlanAndCredentialAccess(t *testing.T) {
	read := []string{"get", "list", "watch"}
	for name, rule := range map[string]rbacv1.PolicyRule{
		"Pod logs":            {APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		"Pod attach":          {APIGroups: []string{""}, Resources: []string{"pods/attach"}, Verbs: []string{"get"}},
		"Pod exec":            {APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"get"}},
		"every Pod resource":  {APIGroups: []string{""}, Resources: []string{"pods/*"}, Verbs: read},
		"plan chunks":         {APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: read},
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
// to what it would read. A Plan Pod hands the whole plan to the controller
// through its log, so every stream out of an operation Pod is plan access, and
// exec reaches an Apply Pod's database credentials as well.
var diagnosticReaderRefuses = map[string]string{
	"secrets":     "database and registry credentials",
	"configmaps":  "plan chunks",
	"pods/log":    "the Plan frame, which carries the plan",
	"pods/attach": "the Plan frame, which carries the plan",
	"pods/exec":   "a process beside the plan and the credentials",
	"pods/*":      "every Pod stream",
	"*":           "every resource",
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
