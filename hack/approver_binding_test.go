package main

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// approverClusterRole.subjects is who may approve, which is the one thing the
// approval controls are about, so the chart renders exactly the subjects it
// was given and refuses an approver that is everyone, with or without the
// schema. kubectl ptah install --approver is the value's caller.

const approverSubjectsKey = "approverClusterRole.subjects"

func TestTheApproverBindingNamesExactlyTheSubjectsGiven(t *testing.T) {
	t.Parallel()
	if bindings := renderedApproverBindings(t, ""); len(bindings) != 0 {
		t.Fatalf("with no subjects the chart binds the approver role: %+v", bindings)
	}
	if bindings := renderedApproverBindings(t, "[]"); len(bindings) != 0 {
		t.Fatalf("with an empty list the chart binds the approver role: %+v", bindings)
	}

	bindings := renderedApproverBindings(t,
		`[{"kind":"User","name":"alice@example.com"},{"kind":"Group","name":"dba"},{"kind":"ServiceAccount","name":"deployer","namespace":"ci"}]`)
	if len(bindings) != 1 {
		t.Fatalf("the chart rendered %d approver ClusterRoleBindings, want one", len(bindings))
	}
	binding := bindings[0]
	if want := (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: approverRoleName}); binding.RoleRef != want {
		t.Errorf("the binding refers to %+v, want %+v", binding.RoleRef, want)
	}
	want := []rbacv1.Subject{
		{Kind: "User", APIGroup: rbacv1.GroupName, Name: "alice@example.com"},
		{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "dba"},
		{Kind: "ServiceAccount", Name: "deployer", Namespace: "ci"},
	}
	if !reflect.DeepEqual(binding.Subjects, want) {
		t.Errorf("the binding names %+v, want %+v", binding.Subjects, want)
	}
	if binding.Labels["app.kubernetes.io/instance"] != approverRelease {
		t.Errorf("the binding carries labels %v, not the release's", binding.Labels)
	}
}

// Each refusal holds with the schema skipped, because the template checks
// again: --skip-schema-validation is a flag anyone can pass.
func TestTheChartRefusesAnApproverItCannotBind(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, subjects, extra, refusal string
	}{
		{name: "every authenticated user", subjects: `[{"kind":"Group","name":"system:authenticated"}]`,
			refusal: "approverClusterRole.subjects[0] names system:authenticated, and an approval anyone can make is no approval"},
		{name: "every anonymous request", subjects: `[{"kind":"Group","name":"system:unauthenticated"}]`,
			refusal: "names system:unauthenticated"},
		{name: "the anonymous user, second in the list", subjects: `[{"kind":"User","name":"alice"},{"kind":"User","name":"system:anonymous"}]`,
			refusal: "approverClusterRole.subjects[1] names system:anonymous"},
		{name: "a kind RBAC does not bind", subjects: `[{"kind":"Role","name":"approvers"}]`,
			refusal: `approverClusterRole.subjects[0].kind is "Role"; it is User, Group or ServiceAccount`},
		{name: "a ServiceAccount without its namespace", subjects: `[{"kind":"ServiceAccount","name":"deployer"}]`,
			refusal: "approverClusterRole.subjects[0] is a ServiceAccount and needs its namespace"},
		{name: "a user with a namespace", subjects: `[{"kind":"User","name":"alice","namespace":"ci"}]`,
			refusal: "approverClusterRole.subjects[0] is a User, which has no namespace"},
		{name: "a name padded with space", subjects: `[{"kind":"User","name":"alice "}]`,
			refusal: "approverClusterRole.subjects[0].name is required and has no leading or trailing space"},
		{name: "no name", subjects: `[{"kind":"User"}]`,
			refusal: "approverClusterRole.subjects[0].name is required"},
		{name: "subjects for a role the chart does not create", subjects: `[{"kind":"User","name":"alice"}]`, extra: "approverClusterRole.create=false",
			refusal: "approverClusterRole.subjects binds the approver ClusterRole, which approverClusterRole.create: false does not create"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			for _, skip := range []bool{false, true} {
				output, err := renderApproverRBAC(t, row.subjects, skip, row.extra)
				if err == nil {
					t.Fatalf("the chart rendered %s (schema skipped: %t)", row.subjects, skip)
				}
				// The schema may refuse first; with it skipped, the template must.
				if skip && !strings.Contains(output, row.refusal) {
					t.Fatalf("with the schema skipped the chart refused %s with:\n%s\nwant %q", row.subjects, output, row.refusal)
				}
			}
		})
	}
}

// The notes say who may approve, or that nobody is bound yet.
func TestTheNotesNameTheApprovers(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		subjects string
		want     []string
		absent   string
	}{
		{subjects: "", want: []string{"ClusterRole " + approverRoleName + " is not\nbound automatically"}, absent: "is bound in every namespace"},
		{
			subjects: `[{"kind":"User","name":"alice@example.com"},{"kind":"ServiceAccount","name":"deployer","namespace":"ci"}]`,
			want: []string{
				"The ClusterRole " + approverRoleName + " is bound in every namespace to:\n  - User alice@example.com\n  - ServiceAccount ci/deployer\n",
				"Each of them may approve any plan in any namespace.",
			},
			absent: "not\nbound automatically",
		},
	} {
		notes := renderedNotes(t, row.subjects)
		for _, want := range row.want {
			if !strings.Contains(notes, want) {
				t.Errorf("with subjects %q the notes say:\n%s\nwant %q", row.subjects, notes, want)
			}
		}
		if strings.Contains(notes, row.absent) {
			t.Errorf("with subjects %q the notes also say %q:\n%s", row.subjects, row.absent, notes)
		}
	}
}

func approverChartArguments(t *testing.T, subjects string, extra ...string) []string {
	t.Helper()
	arguments := []string{repositoryFile(t, "charts/ptah-operator"), "--namespace", "ptah-system"}
	for _, value := range requiredInstallValues {
		arguments = append(arguments, "--set-string", value)
	}
	for _, value := range extra {
		if value != "" {
			arguments = append(arguments, "--set", value)
		}
	}
	if subjects != "" {
		arguments = append(arguments, "--set-json", approverSubjectsKey+"="+subjects)
	}
	return arguments
}

func renderApproverRBAC(t *testing.T, subjects string, skipSchema bool, extra ...string) (string, error) {
	t.Helper()
	arguments := append([]string{"template", approverRelease}, approverChartArguments(t, subjects, extra...)...)
	arguments = append(arguments, "--show-only", "templates/rbac.yaml")
	if skipSchema {
		arguments = append(arguments, "--skip-schema-validation")
	}
	output, err := exec.Command(helmRequired(t), arguments...).CombinedOutput() //nolint:gosec // Arguments are the test's own.
	return string(output), err
}

func renderedApproverBindings(t *testing.T, subjects string) []rbacv1.ClusterRoleBinding {
	t.Helper()
	output, err := renderApproverRBAC(t, subjects, false)
	if err != nil {
		t.Fatalf("helm template with subjects %q: %v\n%s", subjects, err, output)
	}
	var bindings []rbacv1.ClusterRoleBinding
	documents := 0
	for _, document := range strings.Split(output, "\n---\n") {
		var binding rbacv1.ClusterRoleBinding
		if err := yaml.Unmarshal([]byte(document), &binding); err != nil {
			t.Fatalf("the RBAC render does not parse: %v", err)
		}
		if binding.Kind != "" {
			documents++
		}
		if binding.Kind == "ClusterRoleBinding" && strings.HasSuffix(binding.Name, "-approver") {
			bindings = append(bindings, binding)
		}
	}
	// The controller's own ClusterRole and binding are always there, so a
	// render that holds less is not one this check read.
	if documents < 3 {
		t.Fatalf("the RBAC render holds %d objects:\n%s", documents, output)
	}
	return bindings
}

// renderedNotes renders the notes the way an install prints them, with no
// cluster to reach.
func renderedNotes(t *testing.T, subjects string) string {
	t.Helper()
	arguments := append([]string{"install", approverRelease}, approverChartArguments(t, subjects)...)
	arguments = append(arguments, "--dry-run=client")
	command := exec.Command(helmRequired(t), arguments...) //nolint:gosec // Arguments are the test's own.
	command.Env = append(os.Environ(), "KUBECONFIG="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helm install --dry-run=client: %v\n%s", err, output)
	}
	_, notes, found := strings.Cut(string(output), "\nNOTES:\n")
	if !found {
		t.Fatalf("helm printed no notes:\n%s", output)
	}
	return notes
}

// helmRequired is helm, which CI has: a skip there would read as these rows
// passing.
func helmRequired(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("Helm is required in CI: these rows are what prove the chart binds only the approvers it is given")
		}
		t.Skip("Helm is required to render the chart")
	}
	return helm
}
