package crdupgrade

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This is intentionally a white-box Helm render test: a client-only chart
// render cannot populate lookup(), so it invokes the exact inventory helper
// with synthetic retained objects to exercise the live-lookup validation path.
func TestAdmissionConvergenceInventoryRenderRejectsForeignAttemptIdentity(t *testing.T) {
	t.Parallel()

	templatePath := filepath.Join("..", "..", "charts", "ptah-operator", "templates", "admission-convergence.yaml")
	source, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	end := strings.Index(string(source), `{{- $policyName :=`)
	if end < 0 {
		t.Fatal("admission convergence helper boundary is missing")
	}
	helperSource := string(source[:end])

	const (
		releaseName      = "ptah-e2e"
		releaseNamespace = "ptah-e2e"
		fullName         = "ptah-e2e-ptah-operator"
		sequence         = int32(1)
	)
	currentImage := "registry.example/ptah@sha256:" + strings.Repeat("b", 64)

	tests := []struct {
		name            string
		markerImage     string
		currentSequence string
		wantError       string
	}{
		{name: "exact current attempt", markerImage: currentImage, currentSequence: "1"},
		{
			name:            "self-consistent current marker for another digest",
			markerImage:     "registry.example/ptah@sha256:" + strings.Repeat("a", 64),
			currentSequence: "1",
			wantError:       "belongs to a different current release attempt",
		},
		{
			name:            "predecessor image contains another at sign",
			markerImage:     "registry.example/ptah@shadow@sha256:" + strings.Repeat("a", 64),
			currentSequence: "2",
			wantError:       "has an invalid manager identity",
		},
		{
			name:            "predecessor image contains whitespace",
			markerImage:     "registry.example/ptah image@sha256:" + strings.Repeat("a", 64),
			currentSequence: "2",
			wantError:       "has an invalid manager identity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chartDir := t.TempDir()
			templatesDir := filepath.Join(chartDir, "templates")
			if err := os.MkdirAll(templatesDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte("apiVersion: v2\nname: sentinel-test\nversion: 0.1.0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			helpers := `{{- define "ptah-operator.fullname" -}}` + fullName + `{{- end -}}` + "\n" + helperSource
			if err := os.WriteFile(filepath.Join(templatesDir, "_helpers.tpl"), []byte(helpers), 0o600); err != nil {
				t.Fatal(err)
			}

			markerName := AdmissionConvergenceMarkerName(releaseNamespace, releaseName, sequence)
			marker := admissionConvergenceRenderMarker(releaseNamespace, releaseName, fullName, markerName, sequence, test.markerImage)
			encoded, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			probe := fmt.Sprintf(
				`{{- $marker := %q | fromJson -}}{{ include "ptah-operator.validateAdmissionConvergenceInventoryMarker" (dict "root" $ "marker" $marker "name" %q "itemSequence" "1" "currentSequence" %q "currentManagerImage" %q) }}apiVersion: v1
kind: ConfigMap
metadata:
  name: result
`,
				string(encoded), markerName, test.currentSequence, currentImage,
			)
			if err := os.WriteFile(filepath.Join(templatesDir, "probe.yaml"), []byte(probe), 0o600); err != nil {
				t.Fatal(err)
			}

			command := exec.Command("helm", "template", releaseName, chartDir, "--namespace", releaseNamespace, "--show-only", "templates/probe.yaml")
			output, err := command.CombinedOutput()
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("exact marker render failed: %v: %s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), test.wantError) {
				t.Fatalf("render error = %v: %s, want containing %q", err, output, test.wantError)
			}
		})
	}
}

func admissionConvergenceRenderMarker(
	releaseNamespace, releaseName, fullName, markerName string,
	sequence int32,
	managerImage string,
) map[string]any {
	sequenceString := fmt.Sprintf("%d", sequence)
	attempt := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join([]string{
		releaseNamespace,
		releaseName,
		sequenceString,
		managerImage,
	}, "\n"))))
	cleanupBase := strings.TrimSuffix(fullName[:min(len(fullName), 24)], "-")
	cleanupServiceAccount := fmt.Sprintf("%s-cleanup-v%s-%s", cleanupBase, sequenceString, attempt[:12])
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            markerName,
			"namespace":       releaseNamespace,
			"uid":             "marker-uid",
			"resourceVersion": "101",
			"annotations": map[string]any{
				"helm.sh/hook":                                  "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                           "-165",
				"helm.sh/resource-policy":                       "keep",
				admissionConvergenceCleanupAnnotation:           cleanupServiceAccount,
				admissionConvergenceVersionAnnotation:           admissionConvergenceContractVersion,
				PredecessorRetirementInventoryVersionAnnotation: PredecessorRetirementInventoryVersion,
				ManagerImageAnnotation:                          managerImage,
				ReleaseNameAnnotation:                           releaseName,
				ReleaseNamespaceAnnotation:                      releaseNamespace,
				ReleaseSequenceAnnotation:                       sequenceString,
			},
			"labels": map[string]any{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 releaseName,
				"app.kubernetes.io/component": admissionConvergenceComponent,
			},
		},
		"data": map[string]any{
			admissionConvergenceExpectedDataKey: sequenceString,
			admissionConvergenceAttemptDataKey:  attempt,
		},
	}
}
