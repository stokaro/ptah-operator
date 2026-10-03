package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKindUsesDefaultLogRotation(t *testing.T) {
	if strings.Contains(kindKubeletPatch, "containerLogMaxSize") {
		t.Fatal("kind overrides the standard kubelet log size")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().kindConfig)
	path := filepath.Join(t.TempDir(), "kind.yaml.tmpl")
	modified := strings.Replace(source, "kind: KubeletConfiguration", "kind: KubeletConfiguration\n        containerLogMaxSize: 64Mi", 1)
	if modified == source {
		t.Fatal("mutation found no kubelet patch")
	}
	if err := os.WriteFile(path, []byte(modified), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyKindHAConfig(path, repositoryE2EWiringFiles().kindIsolationWorker); err == nil {
		t.Fatal("operator-specific log-size override accepted")
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
		{"four nodes", 4, "false", "10Mi", "", true},
		{"isolation worker", 5, "true", "10Mi", "", true},
		{"one overridden kubelet", 4, "false", "64Mi", "", false},
		{"missing value", 4, "false", "", "", false},
		{"config read failed", 4, "false", "10Mi", "yes", false},
		{"empty inventory", 0, "false", "10Mi", "", false},
		{"missing isolation worker", 4, "true", "10Mi", "", false},
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
  *) printf '{"kubeletconfig":{"containerLogMaxSize":"10Mi"}}\n' ;;
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
