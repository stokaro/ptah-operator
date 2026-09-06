package crdupgrade_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	parentOriginLegacyRevision  = "210c9673e6ad8e339278d99cc4735557332df7bd"
	parentOriginManagedRevision = "3405d26c1003329fe44f019e1eb030a982fc25e5"
	parentOriginRelease         = "origin-upgrade"
	parentOriginNamespace       = "ptah-system"
)

// The chart's live lookup path is exercised through its exact template helpers.
// A client-only Helm render cannot supply retained objects; real historical
// chart renders provide those objects instead of recreating their contracts in Go.
func TestParentOriginLifecycleRecognizesLegacyBootstrapAndExactRetries(t *testing.T) {
	t.Parallel()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for parent-origin lifecycle render tests")
	}
	fixture := newParentOriginRenderFixture(t, helm)
	tests := []struct {
		name      string
		change    func(map[string]any)
		wantError string
	}{
		{name: "actual annotation-free predecessor"},
		{
			name: "bootstrap activation before boundary publication",
			change: func(state map[string]any) {
				fixture.activate(state, "0", false, false)
			},
		},
		{
			name: "exact zero-sequence retry",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "0", false, false)
			},
		},
		{
			name: "exact credential-drain retry",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "0", true, false)
			},
		},
		{
			name: "partial CRD and singleton adoption after activation",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "1", false, true)
				preV1 := state["preV1"].(map[string]any)
				preV1["crds"].([]any)[0].(map[string]any)["object"] = parentOriginCopy(fixture.candidateCRD)
				preV1["singletons"].([]any)[0].(map[string]any)["object"] = parentOriginCopy(fixture.candidateSingleton)
			},
		},
		{
			name: "credential drain after candidate activation",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "1", true, true)
			},
		},
		{
			name: "missing CRD",
			change: func(state map[string]any) {
				state["preV1"].(map[string]any)["crds"].([]any)[0].(map[string]any)["object"] = map[string]any{}
			},
			wantError: "requires annotation-free live CRD",
		},
		{
			name: "managed CRD without boundary",
			change: func(state map[string]any) {
				object := state["preV1"].(map[string]any)["crds"].([]any)[0].(map[string]any)["object"].(map[string]any)
				object["metadata"].(map[string]any)["annotations"].(map[string]any)[crdupgrade.ControllerStateVersionAnnotation] = "1"
			},
			wantError: "requires annotation-free live CRD",
		},
		{
			name: "missing admission singleton",
			change: func(state map[string]any) {
				state["preV1"].(map[string]any)["singletons"].([]any)[0].(map[string]any)["object"] = map[string]any{}
			},
			wantError: "requires both legacy admission singletons",
		},
		{
			name: "foreign admission singleton",
			change: func(state map[string]any) {
				object := state["preV1"].(map[string]any)["singletons"].([]any)[0].(map[string]any)["object"].(map[string]any)
				object["metadata"].(map[string]any)["annotations"].(map[string]any)["meta.helm.sh/release-name"] = "foreign"
			},
			wantError: "is not owned by Helm release",
		},
		{
			name: "managed singleton without boundary",
			change: func(state map[string]any) {
				object := state["preV1"].(map[string]any)["singletons"].([]any)[0].(map[string]any)["object"].(map[string]any)
				object["metadata"].(map[string]any)["annotations"].(map[string]any)["operator.ptah.dev/release-sequence"] = "1"
			},
			wantError: "cannot adopt a managed admission singleton without its boundary",
		},
		{
			name: "missing predecessor ServiceAccount",
			change: func(state map[string]any) {
				state["provenance"].(map[string]any)["serviceAccount"] = map[string]any{}
			},
			wantError: "legacy controller ServiceAccount",
		},
		{
			name: "incomplete predecessor bindings",
			change: func(state map[string]any) {
				state["provenance"].(map[string]any)["coordinationRoleBinding"] = map[string]any{}
			},
			wantError: "legacy controller provenance is incomplete",
		},
		{
			name: "foreign predecessor Deployment",
			change: func(state map[string]any) {
				object := state["provenance"].(map[string]any)["deployment"].(map[string]any)
				object["metadata"].(map[string]any)["annotations"].(map[string]any)["meta.helm.sh/release-name"] = "foreign"
			},
			wantError: "is not owned by Helm release",
		},
		{
			name: "sparse v2 boundary",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "0", false, false)
				state["current"].([]any)[0].(map[string]any)["object"] = map[string]any{}
			},
			wantError: "retained v2 parent-origin boundary is sparse",
		},
		{
			name: "corrupt complete v2 boundary",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "0", false, false)
				object := state["current"].([]any)[0].(map[string]any)["object"].(map[string]any)
				object["spec"].(map[string]any)["failurePolicy"] = "Ignore"
			},
			wantError: "differs from the exact parent-origin contract",
		},
		{
			name:      "retained boundary without activation",
			change:    fixture.retainCurrent,
			wantError: "retry requires exact activation and convergence evidence",
		},
		{
			name: "active candidate without sealed convergence",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "1", false, false)
			},
			wantError: "has foreign, malformed, or different attempt identity",
		},
		{
			name: "wrong activation image",
			change: func(state map[string]any) {
				fixture.retainCurrent(state)
				fixture.activate(state, "0", false, false)
				object := state["preV1"].(map[string]any)["activation"].(map[string]any)
				object["metadata"].(map[string]any)["annotations"].(map[string]any)["operator.ptah.dev/manager-image"] = "foreign"
			},
			wantError: "different or malformed activation ratchet",
		},
		{
			name: "sealed activation without boundary",
			change: func(state map[string]any) {
				fixture.activate(state, "1", false, true)
			},
			wantError: "cannot recreate a missing managed boundary",
		},
		{
			name:   "complete actual v1 predecessor",
			change: fixture.retainLegacy,
		},
		{
			name: "sparse v1 predecessor",
			change: func(state map[string]any) {
				fixture.retainLegacy(state)
				state["legacy"].([]any)[0].(map[string]any)["object"] = map[string]any{}
			},
			wantError: "first v2 upgrade requires the complete exact v1 predecessor",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			state := parentOriginClone(t, fixture.state)
			if test.change != nil {
				test.change(state)
			}
			output, err := fixture.render(t, state)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("recognized lifecycle refused: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), test.wantError) {
				t.Fatalf("render error = %v\n%s\nwant %q", err, output, test.wantError)
			}
		})
	}
}

type parentOriginRenderFixture struct {
	helm, helpers, values            string
	state                            map[string]any
	current, legacy                  []map[string]any
	activation, marker               map[string]any
	candidateCRD, candidateSingleton map[string]any
}

func newParentOriginRenderFixture(t *testing.T, helm string) parentOriginRenderFixture {
	t.Helper()
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	chart := filepath.Join(repository, "charts", "ptah-operator")
	candidate := parentOriginRenderObjects(t, helm, chart)
	legacy := parentOriginRenderObjects(t, helm, parentOriginHistoricalChart(t, repository, parentOriginLegacyRevision))
	managed := parentOriginRenderObjects(t, helm, parentOriginHistoricalChart(t, repository, parentOriginManagedRevision))
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(chart, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	prefix := func(source, boundary string) string {
		t.Helper()
		end := strings.Index(source, boundary)
		if end < 0 {
			t.Fatalf("template helper boundary %q is missing", boundary)
		}
		return source[:end]
	}
	fixture := parentOriginRenderFixture{
		helm: helm,
		helpers: read("templates/_helpers.tpl") + "\n" +
			prefix(read("templates/parent-workload-guard.yaml"), `{{- include "ptah-operator.validateAdmissionSingleton" . -}}`) + "\n" +
			prefix(read("templates/admission-convergence.yaml"), `{{- $policyName :=`),
		values: read("values.yaml"),
	}
	fullName := parentOriginRelease + "-ptah-operator"
	object := func(objects map[string]map[string]any, kind, namespace, name string) map[string]any {
		t.Helper()
		key := kind + "/" + namespace + "/" + name
		if objects[key] == nil {
			t.Fatalf("rendered fixture is missing %s", key)
		}
		return parentOriginClone(t, objects[key])
	}
	deployment := object(legacy, "Deployment", parentOriginNamespace, fullName)
	serviceAccountName, _, err := unstructured.NestedString(deployment, "spec", "template", "spec", "serviceAccountName")
	if err != nil {
		t.Fatal(err)
	}
	currentNames := []string{
		crdupgrade.ParentHookJobOriginGuardPolicyName(parentOriginNamespace, parentOriginRelease),
		crdupgrade.ParentHookPodOriginGuardPolicyName(parentOriginNamespace, parentOriginRelease),
	}
	current, old := []any{}, []any{}
	for index, name := range currentNames {
		oldName := strings.Replace(name, "-v2-", "-v1-", 1)
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			newObject := object(candidate, kind, "", name)
			oldObject := object(managed, kind, "", oldName)
			fixture.current = append(fixture.current, newObject)
			fixture.legacy = append(fixture.legacy, oldObject)
			weight := fmt.Sprintf("%d", -137+len(current))
			current = append(current, map[string]any{"kind": kind, "name": name, "weight": weight, "spec": newObject["spec"], "object": map[string]any{}})
			old = append(old, map[string]any{
				"kind": kind, "name": oldName, "weight": weight, "spec": oldObject["spec"], "object": map[string]any{},
				"retiredWeight": fmt.Sprintf("%d", 9+index*2), "retiredSpec": map[string]any{},
			})
		}
	}
	crds := []any{}
	for _, name := range crdupgrade.Names() {
		crds = append(crds, map[string]any{"name": name, "object": object(legacy, "CustomResourceDefinition", "", name)})
	}
	singletons := []any{}
	for _, kind := range []string{"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		singletons = append(singletons, map[string]any{"kind": kind, "object": object(legacy, kind, "", "ptah-operator-admission")})
	}
	ready := object(candidate, "ConfigMap", parentOriginNamespace, crdupgrade.ParentOriginReadyMarkerName(parentOriginNamespace, parentOriginRelease))
	fixture.activation = object(candidate, "ConfigMap", parentOriginNamespace, "ptah-operator-release-activation")
	fixture.marker = object(candidate, "ConfigMap", parentOriginNamespace, crdupgrade.AdmissionConvergenceMarkerName(parentOriginNamespace, parentOriginRelease, 1))
	fixture.candidateCRD = object(candidate, "CustomResourceDefinition", "", crdupgrade.Names()[0])
	fixture.candidateSingleton = object(candidate, "MutatingWebhookConfiguration", "", "ptah-operator-admission")
	fixture.state = map[string]any{
		"live": true, "activationBootstrap": false, "current": current, "legacy": old,
		"marker": map[string]any{}, "markerName": ready["metadata"].(map[string]any)["name"], "markerData": ready["data"],
		"provenance": map[string]any{
			"deployment":              deployment,
			"serviceAccount":          object(legacy, "ServiceAccount", parentOriginNamespace, serviceAccountName),
			"clusterRoleBinding":      object(legacy, "ClusterRoleBinding", "", fullName),
			"coordinationRoleBinding": object(legacy, "RoleBinding", parentOriginNamespace, fullName),
		},
		"preV1": map[string]any{"crds": crds, "singletons": singletons, "activation": map[string]any{}, "convergenceMarker": map[string]any{}},
	}
	return fixture
}

func (f parentOriginRenderFixture) retainCurrent(state map[string]any) {
	for index, object := range f.current {
		state["current"].([]any)[index].(map[string]any)["object"] = parentOriginCopy(object)
	}
}

func (f parentOriginRenderFixture) retainLegacy(state map[string]any) {
	for index, object := range f.legacy {
		state["legacy"].([]any)[index].(map[string]any)["object"] = parentOriginCopy(object)
	}
}

func (f parentOriginRenderFixture) activate(state map[string]any, sequence string, draining, sealed bool) {
	preV1 := state["preV1"].(map[string]any)
	activation := parentOriginCopy(f.activation)
	marker := parentOriginCopy(f.marker)
	data := activation["data"].(map[string]any)
	data["active-release-sequence"] = sequence
	if draining {
		data["controller-credentials"] = "draining"
		data["controller-credentials-target-release-sequence"] = "1"
		data["controller-credentials-attempt"] = marker["data"].(map[string]any)["release-attempt"]
	}
	if sealed {
		marker["immutable"] = true
		marker["data"].(map[string]any)["predecessor-retirement-inventory"] = `{"version":"1","entries":[{"name":"fixture"}]}`
	}
	preV1["activation"] = activation
	preV1["convergenceMarker"] = marker
}

func (f parentOriginRenderFixture) render(t *testing.T, state map[string]any) ([]byte, error) {
	t.Helper()
	chart := t.TempDir()
	if err := os.Mkdir(filepath.Join(chart, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	probe := fmt.Sprintf(`{{- $state := %q | fromJson -}}
{{- $_ := set $state "root" $ -}}
{{- $principal := include "ptah-operator.previousControllerPrincipalFromObjectsJSON" (merge (dict "root" $) $state.provenance) | fromJson -}}
{{- $_ := set $state.preV1 "previousPrincipal" $principal -}}
{{- include "ptah-operator.validateParentOriginLifecycle" $state -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: lifecycle-recognized
`, string(encoded))
	for name, content := range map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: ptah-operator\nversion: 0.1.0\n",
		"values.yaml": f.values, "templates/_helpers.tpl": f.helpers, "templates/probe.yaml": probe,
	} {
		if err := os.WriteFile(filepath.Join(chart, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return parentOriginHelm(t, f.helm, chart, "--is-upgrade", "--show-only", "templates/probe.yaml")
}

func parentOriginRenderObjects(t *testing.T, helm, chart string) map[string]map[string]any {
	t.Helper()
	output, err := parentOriginHelm(t, helm, chart, "--include-crds")
	if err != nil {
		t.Fatalf("render fixture chart: %v\n%s", err, output)
	}
	objects := map[string]map[string]any{}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(output))
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(object) == 0 {
			continue
		}
		metadata := object["metadata"].(map[string]any)
		name := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)
		if namespace == "" && (object["kind"] == "Deployment" || object["kind"] == "ServiceAccount" || object["kind"] == "RoleBinding" || object["kind"] == "ConfigMap") {
			namespace = parentOriginNamespace
			metadata["namespace"] = namespace
		}
		metadata["uid"] = "fixture-" + name
		metadata["resourceVersion"] = "1"
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			metadata["annotations"] = annotations
		}
		if hook, _ := annotations["helm.sh/hook"].(string); strings.Contains(hook, "pre-delete") {
			// Retirement hooks intentionally reuse stable object names. They do
			// not describe the retained state after installation or upgrade.
			continue
		}
		if _, hook := annotations["helm.sh/hook"]; !hook && object["kind"] != "CustomResourceDefinition" {
			annotations["meta.helm.sh/release-name"] = parentOriginRelease
			annotations["meta.helm.sh/release-namespace"] = parentOriginNamespace
		}
		objects[object["kind"].(string)+"/"+namespace+"/"+name] = object
	}
	return objects
}

func parentOriginHelm(t *testing.T, helm, chart string, extra ...string) ([]byte, error) {
	t.Helper()
	args := []string{"template", parentOriginRelease, chart, "--namespace", parentOriginNamespace,
		"--set-string", "image.digest=sha256:" + strings.Repeat("a", 64),
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:" + strings.Repeat("b", 64),
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:" + strings.Repeat("c", 64),
		"--set-string", "execution.ptahVersion=parent-origin-fixture",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, append(args, extra...)...)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"), "HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"))
	return command.CombinedOutput()
}

func parentOriginHistoricalChart(t *testing.T, repository, revision string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	archive, err := exec.CommandContext(ctx, "git", "-C", repository, "archive", revision, "charts/ptah-operator").Output()
	if err != nil {
		t.Fatalf("archive exact historical chart %s: %v", revision, err)
	}
	directory := t.TempDir()
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsLocal(header.Name) {
			t.Fatalf("historical chart archive contains nonlocal path %q", header.Name)
		}
		path := filepath.Join(directory, header.Name)
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			// git archive includes the immutable commit ID in a global PAX header.
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("historical chart archive contains unsupported entry %s", header.Name)
		}
	}
	return filepath.Join(directory, "charts", "ptah-operator")
}

func parentOriginClone(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func parentOriginCopy(object map[string]any) map[string]any {
	return (&unstructured.Unstructured{Object: object}).DeepCopy().Object
}
