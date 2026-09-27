package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const renderedGuardManagerImage = "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"

// chartPath is the chart this package's render tests render.
func chartPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve the chart path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator")
}

// renderReleaseChart renders the chart as release rbac-cutover in namespace
// ptah-system with pinned images, plus extraArgs.
func renderReleaseChart(t *testing.T, extraArgs ...string) []*unstructured.Unstructured {
	t.Helper()
	args := []string{
		"template", "rbac-cutover", chartPath(t),
		"--namespace", "ptah-system",
		"--set-string", "image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"--set-string", "execution.ptahVersion=rbac-cutover",
	}
	args = append(args, extraArgs...)
	return renderChartObjects(t, args, nil)
}

// renderChartObjects runs helm with args, feeding values on stdin, and
// decodes every rendered object.
func renderChartObjects(t *testing.T, args []string, values []byte) []*unstructured.Unstructured {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, args...)
	command.Stdin = bytes.NewReader(values)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	output, err := command.Output()
	if err != nil {
		// The render contains generated private key material, so it must never be
		// included in a test failure.
		t.Fatalf("helm template failed: %v", err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	objects := []*unstructured.Unstructured{}
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode Helm object: %v", err)
		}
		if object.Object != nil && object.GetKind() != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

// findReconcileJob returns the rendered CRD reconcile Job: the one hook Job that
// runs with the reconcile timeout.
func findReconcileJob(t *testing.T, objects []*unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() != "Job" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil || !found || len(containers) != 1 {
			continue
		}
		if slices.Contains(renderStringSlice(containers[0].(map[string]any)["args"]), "--timeout=360s") {
			return object
		}
	}
	t.Fatal("rendered CRD reconcile Job was not found")
	return nil
}

func findRenderObject(t *testing.T, objects []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered %s/%s was not found", kind, name)
	return nil
}

// renderStringSlice keeps the strings of a decoded YAML list.
func renderStringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func renderedDeploymentServiceAccount(t *testing.T, rendered []byte, deploymentName string) string {
	t.Helper()
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	serviceAccountName := ""
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
		if typeMeta.Kind != "Deployment" {
			continue
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Name != deploymentName {
			continue
		}
		if serviceAccountName != "" {
			t.Fatalf("rendered Deployment/%s is duplicated", deploymentName)
		}
		serviceAccountName = deployment.Spec.Template.Spec.ServiceAccountName
	}
	if serviceAccountName == "" {
		t.Fatalf("rendered Deployment/%s has no ServiceAccount", deploymentName)
	}
	return serviceAccountName
}
