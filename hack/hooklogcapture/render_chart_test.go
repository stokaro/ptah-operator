package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// TestRenderedChartHookJobsValidate renders the chart the way the e2e harness
// produces its candidate render, a client-side helm template of
// templates/crd-upgrade.yaml, and requires the capture to accept the hook Job
// of each mode it can follow. The unit fixtures describe a Job the helper
// expects; this is the Job the chart emits.
func TestRenderedChartHookJobsValidate(t *testing.T) {
	t.Parallel()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(helm, "template", "origin-upgrade", filepath.Join(repository, "charts", "ptah-operator"),
		"--namespace", testNamespace, "--show-only", "templates/crd-upgrade.yaml",
		"--set-string", "image.digest=sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:"+strings.Repeat("b", 64),
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:"+strings.Repeat("c", 64),
		"--set-string", "execution.ptahVersion=render-fixture")
	home := t.TempDir()
	command.Env = append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"), "HELM_DATA_HOME="+filepath.Join(home, "data"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v\n%s", err, output)
	}
	render := writePrivateRender(t, string(output))
	jobs := map[hookMode]string{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var job batchv1.Job
		if err := decoder.Decode(&job); err != nil {
			break
		}
		if job.Kind != "Job" || len(job.Spec.Template.Spec.Containers) != 1 || len(job.Spec.Template.Spec.Containers[0].Args) == 0 {
			continue
		}
		jobs[hookMode(job.Spec.Template.Spec.Containers[0].Args[0])] = job.Name
	}
	for _, mode := range []hookMode{hookModePreflight, hookModeReconcile} {
		t.Run(string(mode), func(t *testing.T) {
			name, found := jobs[mode]
			if !found {
				t.Fatalf("the chart renders no %s hook Job", mode)
			}
			job, err := loadRenderedJob(render, testNamespace, name)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRenderedJob(job, captureConfig{namespace: testNamespace, jobName: name, hookMode: mode}); err != nil {
				t.Fatalf("the capture would refuse to arm on the chart's own %s hook Job: %v", mode, err)
			}
		})
	}
}
