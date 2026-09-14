package crdupgrade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A release that adds a kind is the only reason the chart renders a CRD from
// templates: Helm applies crds/ on install and leaves it alone on upgrade, so
// an added kind would never exist and the CRD manager would stop the upgrade
// in its pre-upgrade hook.
//
// A CRD the cluster already holds belongs to the manager, which compares its
// schema and refuses an incompatible change, so the chart must not write it.
func TestCRDBootstrapRendersOnlyWhatTheClusterDoesNotHold(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for live lookup render tests")
	}
	chart := serviceAccountObjectGuardChartPath(t)

	installed := func(names ...string) map[string]bool {
		present := make(map[string]bool, len(names))
		for _, name := range names {
			present[name] = true
		}
		return present
	}

	// Derived from the generated set rather than named, so adding a kind does
	// not quietly turn a case into a different one.
	added := expectedNames[0]
	// What a cluster from before this release carries, and what this release
	// stamps: the gap between them is what says an absent CRD is an addition.
	older := strconv.FormatUint(CurrentCRDSchemaVersion-1, 10)
	current := strconv.FormatUint(CurrentCRDSchemaVersion, 10)
	tests := []struct {
		name      string
		present   map[string]bool
		installed string
		want      []string
	}{
		{name: "nothing installed", present: installed(), installed: older, want: expectedNames},
		{
			name: "one kind added", present: installed(expectedNames[1:]...),
			installed: older, want: []string{added},
		},
		{name: "every kind installed", present: installed(expectedNames...), installed: older, want: nil},
		{
			// A CRD absent while the cluster already carries what this release
			// stamps was deleted, and the manager's refusal owns that case.
			name: "one kind deleted", present: installed(expectedNames[1:]...),
			installed: current, want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(crdBootstrapAPIServer(t, test.present, test.installed))
			t.Cleanup(server.Close)
			output, renderErr := runCRDBootstrapHelm(t, helm, chart, crdBootstrapKubeconfig(t, server.URL))
			if renderErr != nil {
				t.Fatalf("render error = %v\n%s", renderErr, output)
			}
			bootstrapped := crdBootstrapDocuments(string(output))
			for _, name := range expectedNames {
				wanted := false
				for _, want := range test.want {
					wanted = wanted || want == name
				}
				document, rendered := bootstrapped[name]
				if rendered != wanted {
					t.Fatalf("%s rendered = %t, want %t\n%s", name, rendered, wanted, output)
				}
				if !rendered {
					continue
				}
				// A CRD Helm applies from a template is a hook, kept on
				// uninstall, and ordered before the manager that reads it.
				for _, marker := range []string{
					"helm.sh/hook: pre-install,pre-upgrade",
					"helm.sh/hook-weight: \"-420\"",
					"helm.sh/resource-policy: keep",
				} {
					if !strings.Contains(document, marker) {
						t.Fatalf("%s is missing %q:\n%s", name, marker, document)
					}
				}
			}
		})
	}
}

func crdBootstrapKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    namespace: ptah-e2e
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`, serverURL)
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return kubeconfig
}

// crdBootstrapAPIServer answers the discovery every template in the chart
// performs -- --show-only filters the output, not the rendering -- plus the CRD
// reads this template makes: a present name returns the object, an absent one
// 404s.
func crdBootstrapAPIServer(t *testing.T, present map[string]bool, installedVersion string) http.Handler {
	t.Helper()
	groups := `{"kind":"APIGroupList","apiVersion":"v1","groups":[` +
		crdBootstrapGroup("apiextensions.k8s.io") + "," +
		crdBootstrapGroup("admissionregistration.k8s.io") + "," +
		crdBootstrapGroup("apps") + "," +
		crdBootstrapGroup("batch") + "," +
		crdBootstrapGroup("policy") + "," +
		crdBootstrapGroup("coordination.k8s.io") + "," +
		crdBootstrapGroup("rbac.authorization.k8s.io") + `]}`
	const crdPrefix = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
	notFound := func(response http.ResponseWriter) {
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","code":404}`))
	}

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/version":
			_, _ = response.Write([]byte(`{"major":"1","minor":"35","gitVersion":"v1.35.0","gitCommit":"test","gitTreeState":"clean","buildDate":"2026-01-01T00:00:00Z","goVersion":"go1.26.0","compiler":"gc","platform":"linux/amd64"}`))
		case request.URL.Path == "/api":
			_, _ = response.Write([]byte(`{"kind":"APIVersions","apiVersion":"v1","versions":["v1"],"serverAddressByClientCIDRs":[]}`))
		case request.URL.Path == "/apis":
			_, _ = response.Write([]byte(groups))
		case request.URL.Path == "/api/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("v1",
				"configmaps/ConfigMap/true", "secrets/Secret/true", "services/Service/true",
				"serviceaccounts/ServiceAccount/true", "pods/Pod/true", "namespaces/Namespace/false")))
		case request.URL.Path == "/apis/admissionregistration.k8s.io/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("admissionregistration.k8s.io/v1",
				"validatingadmissionpolicies/ValidatingAdmissionPolicy/false",
				"validatingadmissionpolicybindings/ValidatingAdmissionPolicyBinding/false",
				"mutatingwebhookconfigurations/MutatingWebhookConfiguration/false",
				"validatingwebhookconfigurations/ValidatingWebhookConfiguration/false")))
		case request.URL.Path == "/apis/apps/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("apps/v1", "deployments/Deployment/true")))
		case request.URL.Path == "/apis/rbac.authorization.k8s.io/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("rbac.authorization.k8s.io/v1",
				"clusterroles/ClusterRole/false", "clusterrolebindings/ClusterRoleBinding/false",
				"roles/Role/true", "rolebindings/RoleBinding/true")))
		case request.URL.Path == "/apis/batch/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("batch/v1", "jobs/Job/true")))
		case request.URL.Path == "/apis/coordination.k8s.io/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("coordination.k8s.io/v1", "leases/Lease/true")))
		case request.URL.Path == "/apis/policy/v1":
			_, _ = response.Write([]byte(crdBootstrapResources("policy/v1", "poddisruptionbudgets/PodDisruptionBudget/true")))
		case request.URL.Path == "/apis/apiextensions.k8s.io/v1":
			_, _ = response.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"apiextensions.k8s.io/v1","resources":[{"name":"customresourcedefinitions","singularName":"customresourcedefinition","namespaced":false,"kind":"CustomResourceDefinition","verbs":["get","list"]}]}`))
		case strings.HasPrefix(request.URL.Path, crdPrefix):
			name := strings.TrimPrefix(request.URL.Path, crdPrefix)
			if !present[name] {
				notFound(response)
				return
			}
			_, _ = response.Write([]byte(fmt.Sprintf(
				`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition",`+
					`"metadata":{"name":%q,"annotations":{"operator.ptah.run/crd-schema-version":%q}}}`,
				name, installedVersion)))
		default:
			notFound(response)
		}
	})
}

func runCRDBootstrapHelm(t *testing.T, helm, chart, kubeconfig string) ([]byte, error) {
	t.Helper()
	args := []string{
		"template", "ptah-e2e", chart,
		"--namespace", "ptah-e2e",
		"--dry-run=server",
		"--disable-openapi-validation",
		"--kubeconfig", kubeconfig,
		"--set-string", "image.digest=sha256:" + strings.Repeat("2", 64),
		"--set-string", "execution.executorImage=e2e.invalid/executor@sha256:" + strings.Repeat("0", 64),
		"--set-string", "execution.runnerImage=e2e.invalid/runner@sha256:" + strings.Repeat("1", 64),
		"--set-string", "execution.ptahVersion=e2e-explicit-version",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, args...)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	return command.CombinedOutput()
}

// crdBootstrapGroup and crdBootstrapResources write the discovery documents the
// render needs, so the fixture says which kinds exist rather than restating
// their JSON five times.
func crdBootstrapGroup(name string) string {
	return fmt.Sprintf(
		`{"name":%[1]q,"versions":[{"groupVersion":"%[1]s/v1","version":"v1"}],"preferredVersion":{"groupVersion":"%[1]s/v1","version":"v1"}}`,
		name,
	)
}

func crdBootstrapResources(groupVersion string, resources ...string) string {
	entries := make([]string, 0, len(resources))
	for _, resource := range resources {
		parts := strings.Split(resource, "/")
		entries = append(entries, fmt.Sprintf(
			`{"name":%q,"singularName":%q,"namespaced":%s,"kind":%q,"verbs":["get","list"]}`,
			parts[0], strings.TrimSuffix(parts[0], "s"), parts[2], parts[1],
		))
	}
	return fmt.Sprintf(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":%q,"resources":[%s]}`,
		groupVersion, strings.Join(entries, ","))
}

// crdBootstrapDocuments reads the CRDs the bootstrap template rendered, keyed
// by name. Helm labels every document with the template it came from, which is
// what separates these from the CRD names RBAC rules also carry.
func crdBootstrapDocuments(rendered string) map[string]string {
	documents := map[string]string{}
	for _, section := range strings.Split(rendered, "# Source: ") {
		if !strings.HasPrefix(section, "ptah-operator/templates/crd-bootstrap.yaml") {
			continue
		}
		for _, line := range strings.Split(section, "\n") {
			if name, found := strings.CutPrefix(strings.TrimSpace(line), "name: "); found {
				documents[name] = section
				break
			}
		}
	}
	return documents
}
