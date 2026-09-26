package certrotation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The chart refuses two installed objects on upgrade that no release leaves
// behind: an admission singleton without the annotations the release that
// owns it writes, and a generated webhook Secret without its CA key. Both
// refusals read an object from the cluster, which a plain render never has,
// so each helper takes the object as an argument and is rendered here against
// a fixture of it.

const admissionSingletonProbeTemplate = `{{- $fixture := default (dict) .Values.fixture -}}
{{- include "ptah-operator.validateAdmissionSingletonObjects" (dict
      "root" .
      "name" (include "ptah-operator.approvalWebhookConfigurationName" .)
      "mutating" (default (dict) (get $fixture "mutating"))
      "validating" (default (dict) (get $fixture "validating"))) -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: probe-result
`

func TestAdmissionSingletonRenderRefusesAnObjectWithoutItsOwnedAnnotations(t *testing.T) {
	t.Parallel()

	// What this release writes, with the ownership Helm adds when it installs
	// it: the singleton exactly as the next upgrade reads it back. Each row
	// gets its own copy.
	objects := renderChart(t)
	const configurationName = "ptah-operator-admission"
	renderedMutating := mustObject(t, objects, "MutatingWebhookConfiguration", configurationName)
	renderedValidating := mustObject(t, objects, "ValidatingWebhookConfiguration", configurationName)
	installed := func() (map[string]any, map[string]any) {
		return helmInstalled(renderedMutating), helmInstalled(renderedValidating)
	}

	t.Run("the singleton this release installed", func(t *testing.T) {
		t.Parallel()
		mutating, validating := installed()
		if owned := ownedAnnotationCount(mutating); owned == 0 {
			t.Fatal("the rendered singleton carries no owned annotations, so removing them below proves nothing")
		}
		if _, err := renderHelperProbe(t, admissionSingletonProbeTemplate, map[string]any{
			"mutating": mutating, "validating": validating,
		}); err != nil {
			t.Fatalf("the chart refused the singleton it installed itself: %v", err)
		}
	})

	t.Run("a fresh install with no singleton", func(t *testing.T) {
		t.Parallel()
		if _, err := renderHelperProbe(t, admissionSingletonProbeTemplate, map[string]any{}); err != nil {
			t.Fatalf("the chart refused a namespace with no singleton at all: %v", err)
		}
	})

	for _, row := range []struct {
		name  string
		strip func(annotations map[string]any)
	}{
		{
			// Helm still owns it, and no release wrote it. No upgrade starts
			// from this.
			name: "none of the owned annotations",
			strip: func(annotations map[string]any) {
				for key := range annotations {
					if strings.HasPrefix(key, "operator.ptah.run/") {
						delete(annotations, key)
					}
				}
			},
		},
		{
			name: "one owned annotation missing",
			strip: func(annotations map[string]any) {
				delete(annotations, "operator.ptah.run/admission-contract-version")
			},
		},
	} {
		t.Run("a singleton with "+row.name, func(t *testing.T) {
			t.Parallel()
			mutating, validating := installed()
			for _, object := range []map[string]any{mutating, validating} {
				row.strip(object["metadata"].(map[string]any)["annotations"].(map[string]any))
			}
			_, err := renderHelperProbe(t, admissionSingletonProbeTemplate, map[string]any{
				"mutating": mutating, "validating": validating,
			})
			const want = "has an incomplete owned annotation tuple"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("render error = %v, want a refusal naming %q", err, want)
			}
		})
	}
}

const webhookCertificateMaterialProbeTemplate = `{{- $fixture := default (dict) .Values.fixture -}}
{{- $material := include "ptah-operator.webhookCertificateMaterialJSON" (dict
      "root" .
      "existing" (default (dict) (get $fixture "secret"))) | fromJson -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: probe-result
data:
  caBundle: {{ $material.caBundle | quote }}
  caKey: {{ $material.caKey | quote }}
  tlsCrt: {{ $material.tlsCrt | quote }}
  tlsKey: {{ $material.tlsKey | quote }}
`

func TestWebhookCertificateRenderRefusesAGeneratedSecretMissingMaterial(t *testing.T) {
	t.Parallel()

	// Placeholders, not keys: the helper copies what the Secret holds and
	// never parses it.
	data := map[string]string{
		"ca.crt":  "Q0EtQ0VSVElGSUNBVEU=",
		"ca.key":  "Q0EtS0VZ",
		"tls.crt": "VExTLUNFUlRJRklDQVRF",
		"tls.key": "VExTLUtFWQ==",
	}
	generatedSecret := func(keys map[string]string) map[string]any {
		secretData := map[string]any{}
		for key, value := range keys {
			secretData[key] = value
		}
		return map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      releaseName + "-ptah-operator-webhook-cert",
				"namespace": releaseNamespace,
				"labels": map[string]any{
					"app.kubernetes.io/managed-by":                    "Helm",
					"operator.ptah.run/generated-webhook-certificate": "true",
				},
				"annotations": map[string]any{
					"meta.helm.sh/release-name":      releaseName,
					"meta.helm.sh/release-namespace": releaseNamespace,
				},
			},
			"type": "kubernetes.io/tls",
			"data": secretData,
		}
	}

	t.Run("a complete generated Secret is carried forward", func(t *testing.T) {
		t.Parallel()
		result, err := renderHelperProbe(t, webhookCertificateMaterialProbeTemplate, map[string]any{
			"secret": generatedSecret(data),
		})
		if err != nil {
			t.Fatalf("the chart refused a complete generated Secret: %v", err)
		}
		for field, key := range map[string]string{
			"caBundle": "ca.crt", "caKey": "ca.key", "tlsCrt": "tls.crt", "tlsKey": "tls.key",
		} {
			got, _, err := unstructured.NestedString(result.Object, "data", field)
			if err != nil {
				t.Fatal(err)
			}
			if got != data[key] {
				t.Errorf("%s = %q, want the Secret's %s %q rather than new material", field, got, key, data[key])
			}
		}
	})

	t.Run("no Secret yet generates all four", func(t *testing.T) {
		t.Parallel()
		result, err := renderHelperProbe(t, webhookCertificateMaterialProbeTemplate, map[string]any{})
		if err != nil {
			t.Fatalf("the chart refused to generate bootstrap material: %v", err)
		}
		for _, field := range []string{"caBundle", "caKey", "tlsCrt", "tlsKey"} {
			if got, _, _ := unstructured.NestedString(result.Object, "data", field); got == "" {
				t.Errorf("generated %s is empty", field)
			}
		}
	})

	for _, missing := range []string{"ca.key", "ca.crt", "tls.crt", "tls.key"} {
		t.Run("a generated Secret without "+missing, func(t *testing.T) {
			t.Parallel()
			partial := map[string]string{}
			for key, value := range data {
				if key != missing {
					partial[key] = value
				}
			}
			_, err := renderHelperProbe(t, webhookCertificateMaterialProbeTemplate, map[string]any{
				"secret": generatedSecret(partial),
			})
			want := "generated webhook Secret must contain " + missing
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("render error = %v, want a refusal naming %q", err, want)
			}
		})
	}

	t.Run("a Secret another owner generated", func(t *testing.T) {
		t.Parallel()
		secret := generatedSecret(data)
		secret["metadata"].(map[string]any)["annotations"].(map[string]any)["meta.helm.sh/release-name"] = "another-release"
		_, err := renderHelperProbe(t, webhookCertificateMaterialProbeTemplate, map[string]any{"secret": secret})
		const want = "foreign or incomplete Helm ownership metadata"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("render error = %v, want a refusal naming %q", err, want)
		}
	})
}

// helmInstalled returns the object as the API server holds it after Helm
// installed it, which adds its release ownership annotations.
func helmInstalled(object *unstructured.Unstructured) map[string]any {
	installed := object.DeepCopy().Object
	metadata := installed["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations["meta.helm.sh/release-name"] = releaseName
	annotations["meta.helm.sh/release-namespace"] = releaseNamespace
	return installed
}

func ownedAnnotationCount(object map[string]any) int {
	annotations, _ := object["metadata"].(map[string]any)["annotations"].(map[string]any)
	count := 0
	for key := range annotations {
		if strings.HasPrefix(key, "operator.ptah.run/") {
			count++
		}
	}
	return count
}

// renderHelperProbe renders probe against the chart's own helpers and default
// values, set the way renderChart sets them, with fixture under
// .Values.fixture. It returns the ConfigMap named probe-result, or the
// render's refusal.
func renderHelperProbe(t *testing.T, probe string, fixture map[string]any) (*unstructured.Unstructured, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	_, filename, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator")
	chartDirectory := t.TempDir()
	templatesDirectory := filepath.Join(chartDirectory, "templates")
	if err := os.Mkdir(templatesDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for source, target := range map[string]string{
		filepath.Join(chart, "Chart.yaml"):                filepath.Join(chartDirectory, "Chart.yaml"),
		filepath.Join(chart, "values.yaml"):               filepath.Join(chartDirectory, "values.yaml"),
		filepath.Join(chart, "templates", "_helpers.tpl"): filepath.Join(templatesDirectory, "_helpers.tpl"),
	} {
		contents, err := os.ReadFile(source) //nolint:gosec // A path inside the repository.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(templatesDirectory, "probe.yaml"), []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureValues, err := json.Marshal(map[string]any{"fixture": fixture})
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(chartDirectory, "fixture.json")
	if err := os.WriteFile(fixturePath, fixtureValues, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helm,
		"template", releaseName, chartDirectory,
		"--namespace", releaseNamespace,
		"--set-string", "image.digest=sha256:"+managerDigest,
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:"+executorDigest,
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:"+runnerDigest,
		"--set-string", "execution.ptahVersion="+ptahVersion,
		"--values", fixturePath,
		"--show-only", "templates/probe.yaml",
	)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if object.GetKind() == "ConfigMap" && object.GetName() == "probe-result" {
			return object, nil
		}
	}
	return nil, fmt.Errorf("the probe rendered no probe-result ConfigMap")
}
