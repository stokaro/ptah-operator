package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedServiceAccountObjectGuardRejectsMutatedLiveSpecBeforeHookServiceAccount(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for live lookup render tests")
	}
	chart := serviceAccountObjectGuardChartPath(t)
	clientOutput, clientErr := runServiceAccountObjectGuardHelm(t, helm, chart, "client", "")
	if clientErr != nil {
		t.Fatalf("render expected service account object guard: %v\n%s", clientErr, clientOutput)
	}
	policy, binding := renderedServiceAccountObjectGuardPair(t, clientOutput)
	policy = persistedServiceAccountObjectPolicy(policy)
	binding = persistedServiceAccountObjectBinding(binding)
	ignore := admissionregistrationv1.Ignore
	policy.Spec.FailurePolicy = &ignore

	var policyRead atomic.Bool
	server := httptest.NewServer(serviceAccountObjectGuardAPIServer(t, policy, binding, &policyRead))
	t.Cleanup(server.Close)
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
`, server.URL)
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	output, renderErr := runServiceAccountObjectGuardHelm(t, helm, chart, "server", kubeconfig)
	if renderErr == nil {
		t.Fatalf("server-side Helm render accepted a mutated retained policy:\n%s", output)
	}
	if !policyRead.Load() {
		t.Fatalf("server-side Helm render did not read the retained policy: %v\n%s", renderErr, output)
	}
	if !bytes.Contains(output, []byte("differs from the exact service-account-object contract")) {
		t.Fatalf("server-side Helm render failed for the wrong reason: %v\n%s", renderErr, output)
	}
	if bytes.Contains(output, []byte("kind: ServiceAccount")) {
		t.Fatalf("mutated retained policy allowed a hook ServiceAccount to render:\n%s", output)
	}
}

func TestRenderedServiceAccountObjectGuardFirstInstallOrdering(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for service account object guard ordering tests")
	}
	output, renderErr := runServiceAccountObjectGuardHelm(t, helm, serviceAccountObjectGuardChartPath(t), "client", "")
	if renderErr != nil {
		t.Fatalf("render service account object guard ordering: %v\n%s", renderErr, output)
	}

	want := map[string]string{
		"ConfigMap/" + AdmissionConvergenceMarkerName("ptah-e2e", "ptah-e2e", 1):                                                              admissionConvergenceMarkerHookWeight,
		"ValidatingAdmissionPolicy/" + ServiceAccountObjectGuardPolicyName("ptah-e2e", "ptah-e2e"):                                            serviceAccountObjectPolicyWeight,
		"ValidatingAdmissionPolicyBinding/" + ServiceAccountObjectGuardBindingName("ptah-e2e", "ptah-e2e"):                                    serviceAccountObjectBindingWeight,
		"Job/ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest("ptah-e2e", "ptah-e2e", 1, renderedGuardManagerImage)[:12] + "-image-check": "-130",
		"ServiceAccount/ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest("ptah-e2e", "ptah-e2e", 1, renderedGuardManagerImage)[:12]:       "-110",
	}
	seen := map[string]string{}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(output))
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
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		key := object.Kind + "/" + object.Metadata.Name
		if _, found := want[key]; found {
			seen[key] = object.Metadata.Annotations["helm.sh/hook-weight"]
		}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("first-install guard ordering = %#v, want %#v", seen, want)
	}
}

func serviceAccountObjectGuardChartPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve service account object guard test path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator")
}

func runServiceAccountObjectGuardHelm(t *testing.T, helm, chart, dryRun, kubeconfig string) ([]byte, error) {
	t.Helper()
	args := []string{
		"template", "ptah-e2e", chart,
		"--namespace", "ptah-e2e",
		"--dry-run=" + dryRun,
		"--show-only", "templates/service-account-object-guard.yaml",
		"--show-only", "templates/admission-convergence.yaml",
		"--show-only", "templates/crd-upgrade.yaml",
		"--disable-openapi-validation",
		"--set-string", "image.digest=sha256:" + strings.Repeat("2", 64),
		"--set-string", "execution.executorImage=e2e.invalid/executor@sha256:" + strings.Repeat("0", 64),
		"--set-string", "execution.runnerImage=e2e.invalid/runner@sha256:" + strings.Repeat("1", 64),
		"--set-string", "execution.ptahVersion=e2e-explicit-version",
	}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
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

func renderedServiceAccountObjectGuardPair(t *testing.T, rendered []byte) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode service account object guard render: %v", err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var candidate admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &candidate); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(candidate.Name, serviceAccountObjectGuardNamePrefix) {
				policy = &candidate
			}
		case "ValidatingAdmissionPolicyBinding":
			var candidate admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &candidate); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(candidate.Name, serviceAccountObjectGuardNamePrefix) {
				binding = &candidate
			}
		}
	}
	if policy == nil || binding == nil {
		t.Fatal("rendered service account object guard pair is incomplete")
	}
	return policy, binding
}

func serviceAccountObjectGuardAPIServer(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	policyRead *atomic.Bool,
) http.Handler {
	t.Helper()
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/" + policy.Name
	bindingPath := "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/" + binding.Name

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/version":
			_, _ = response.Write([]byte(`{"major":"1","minor":"35","gitVersion":"v1.35.0","gitCommit":"test","gitTreeState":"clean","buildDate":"2026-01-01T00:00:00Z","goVersion":"go1.26.0","compiler":"gc","platform":"darwin/arm64"}`))
		case "/api":
			_, _ = response.Write([]byte(`{"kind":"APIVersions","apiVersion":"v1","versions":["v1"],"serverAddressByClientCIDRs":[]}`))
		case "/apis":
			_, _ = response.Write([]byte(serviceAccountObjectGuardAPIGroupList))
		case "/api/v1":
			_, _ = response.Write([]byte(serviceAccountObjectGuardCoreResources))
		case "/apis/admissionregistration.k8s.io/v1":
			_, _ = response.Write([]byte(serviceAccountObjectGuardAdmissionResources))
		case "/apis/apps/v1":
			_, _ = response.Write([]byte(serviceAccountObjectGuardAppsResources))
		case "/apis/rbac.authorization.k8s.io/v1":
			_, _ = response.Write([]byte(serviceAccountObjectGuardRBACResources))
		case "/apis/batch/v1":
			_, _ = response.Write([]byte(serviceAccountObjectGuardBatchResources))
		case policyPath:
			policyRead.Store(true)
			_, _ = response.Write(policyJSON)
		case bindingPath:
			_, _ = response.Write(bindingJSON)
		default:
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","code":404}`))
		}
	})
}

const serviceAccountObjectGuardAPIGroupList = `{
  "kind":"APIGroupList","apiVersion":"v1","groups":[
    {"name":"admissionregistration.k8s.io","versions":[{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}},
    {"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
    {"name":"batch","versions":[{"groupVersion":"batch/v1","version":"v1"}],"preferredVersion":{"groupVersion":"batch/v1","version":"v1"}},
    {"name":"rbac.authorization.k8s.io","versions":[{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}}
  ]
}`

const serviceAccountObjectGuardCoreResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"serviceaccounts","singularName":"serviceaccount","namespaced":true,"kind":"ServiceAccount","verbs":["get"]},{"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get"]}]}`
const serviceAccountObjectGuardAdmissionResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"admissionregistration.k8s.io/v1","resources":[{"name":"validatingadmissionpolicies","singularName":"validatingadmissionpolicy","namespaced":false,"kind":"ValidatingAdmissionPolicy","verbs":["get"]},{"name":"validatingadmissionpolicybindings","singularName":"validatingadmissionpolicybinding","namespaced":false,"kind":"ValidatingAdmissionPolicyBinding","verbs":["get"]}]}`
const serviceAccountObjectGuardAppsResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"apps/v1","resources":[{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get"]}]}`
const serviceAccountObjectGuardRBACResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"rbac.authorization.k8s.io/v1","resources":[{"name":"clusterrolebindings","singularName":"clusterrolebinding","namespaced":false,"kind":"ClusterRoleBinding","verbs":["get"]},{"name":"rolebindings","singularName":"rolebinding","namespaced":true,"kind":"RoleBinding","verbs":["get"]}]}`
const serviceAccountObjectGuardBatchResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"batch/v1","resources":[{"name":"jobs","singularName":"job","namespaced":true,"kind":"Job","verbs":["get"]}]}`
