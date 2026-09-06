package examples_test

import (
	"bytes"
	"encoding/json"
	"errors"
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
		APIGroups: []string{"operator.ptah.dev"},
		Resources: []string{"ptahschemas"},
		Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
	}}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("desired-state author rules = %#v, want %#v", role.Rules, wantRules)
	}
	assertGroupBinding(t, role, binding, "<desired-state-author-group>")
}

func TestDiagnosticReaderRoleCannotChangeStateOrReadCredentials(t *testing.T) {
	role, binding := readRoleExample(t, "diagnostic-reader-role.yaml")
	if role.Namespace != "application" || binding.Namespace != role.Namespace {
		t.Fatalf("diagnostic reader namespace = %q/%q, want application", role.Namespace, binding.Namespace)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"operator.ptah.dev"}, Resources: []string{"ptahschemas", "ptahschemaplans", "ptahschemaapprovals"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"get", "list", "watch"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("diagnostic reader rules = %#v, want %#v", role.Rules, wantRules)
	}
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if resource == "secrets" || resource == "configmaps" {
				t.Fatalf("diagnostic reader can access sensitive resource %q", resource)
			}
		}
		for _, verb := range rule.Verbs {
			switch verb {
			case "create", "update", "patch", "delete", "deletecollection":
				t.Fatalf("diagnostic reader has mutating verb %q", verb)
			}
		}
	}
	assertGroupBinding(t, role, binding, "<diagnostic-reader-group>")
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
