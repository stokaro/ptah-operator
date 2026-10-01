package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestKindLogFileHoldsSupportedRunnerFrame(t *testing.T) {
	var patch struct {
		Size string `yaml:"containerLogMaxSize"`
	}
	if err := yaml.Unmarshal([]byte(kindKubeletPatch), &patch); err != nil {
		t.Fatal(err)
	}
	size, err := resource.ParseQuantity(patch.Size)
	if err != nil {
		t.Fatal(err)
	}
	// Reserve a quarter of the parser's maximum tail for CRI record prefixes
	// and runner diagnostics, independently of the ordinary plan's smaller size.
	minimum := runner.MaxResultLogBytes + runner.MaxResultLogBytes/4
	if size.Value() < minimum {
		t.Fatalf("log file %s cannot retain supported result plus transport headroom (%d bytes)", patch.Size, minimum)
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().kindConfig)
	path := filepath.Join(t.TempDir(), "kind.yaml.tmpl")
	if err := os.WriteFile(path, []byte(strings.Replace(source, "containerLogMaxSize: 64Mi", "containerLogMaxSize: 10Mi", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyKindHAConfig(path, repositoryE2EWiringFiles().kindIsolationWorker); err == nil {
		t.Fatal("default rotation threshold accepted")
	}
}

func TestLiveKubeletLogBudgetRequiresEveryNode(t *testing.T) {
	source := readE2ESource(t, repositoryE2EWiringFiles().harness)
	for _, tc := range []struct {
		name                      string
		nodes                     int
		isolation, value, failure string
		pass                      bool
	}{
		{"four nodes", 4, "false", "64Mi", "", true},
		{"isolation worker", 5, "true", "64Mi", "", true},
		{"one default kubelet", 4, "false", "10Mi", "", false},
		{"missing value", 4, "false", "", "", false},
		{"config read failed", 4, "false", "64Mi", "yes", false},
		{"empty inventory", 0, "false", "64Mi", "", false},
		{"missing isolation worker", 4, "true", "64Mi", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			items := []any{}
			for i := range tc.nodes {
				items = append(items, map[string]any{"metadata": map[string]string{"name": fmt.Sprintf("node-%d", i)}})
			}
			body, err := json.Marshal(map[string]any{"items": items})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "nodes.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			script := `set -eu
WORK_DIR=$1
NODE_READINESS_FILE=$1/nodes.json
KUBECONFIG_FILE=unused
ISOLATION_WORKER=$2
MOCK_VALUE=$3
MOCK_FAILURE=$4
kubectl() {
 for arg; do query=$arg; done
 case "$query" in
  /api/v1/nodes/node-*/proxy/configz) ;;
  *) return 19 ;;
 esac
 printf '%s\n' "$query" >>"$WORK_DIR/requests"
 case "$query" in
  */node-2/*)
   [ -z "$MOCK_FAILURE" ] || return 1
   printf '{"kubeletconfig":{"containerLogMaxSize":"%s"}}\n' "$MOCK_VALUE" ;;
  *) printf '{"kubeletconfig":{"containerLogMaxSize":"64Mi"}}\n' ;;
 esac
}
` + extractE2EShellFunction(t, source, "fail") + "\n" + extractE2EShellFunction(t, source, "assert_kubelet_log_budget") + "\nassert_kubelet_log_budget\n"
			command := exec.Command("sh", "-c", script, "probe", dir, tc.isolation, tc.value, tc.failure)
			out, err := command.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("pass=%v want %v: %s", err == nil, tc.pass, out)
			}
			if tc.pass {
				calls, err := os.ReadFile(filepath.Join(dir, "requests"))
				if err != nil {
					t.Fatal(err)
				}
				if len(strings.Fields(string(calls))) != tc.nodes {
					t.Fatal("not every kubelet was read")
				}
				for i := range tc.nodes {
					if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("kubelet-log-config-node-%d.json", i))); err != nil {
						t.Fatal("effective configuration was not retained", err)
					}
				}
			}
		})
	}
}
