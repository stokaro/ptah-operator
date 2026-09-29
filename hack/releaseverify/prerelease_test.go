package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Execute the workflow's metadata step and draft command with local stand-ins
// for GitHub, Git, and the source verifier. No tag or release is created.
func TestReleaseWorkflowSetsPrereleaseFromTag(t *testing.T) {
	t.Parallel()
	steps := releaseWorkflowSteps(t)
	for _, test := range []struct {
		tag, prerelease string
	}{
		{"v0.1.0", "false"},
		{"v0.1.0-rc.1", "true"},
		{"v0.1.0-rc.10", "true"},
		{"v1.2.3-beta", "true"},
	} {
		t.Run(test.tag, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{
				"git": "#!/bin/sh\nexit 0\n",
				"go":  "#!/bin/sh\nexit 0\n",
				"gh": `#!/bin/bash
set -euo pipefail
case "$1 $2" in
  'attestation verify') ;;
  'release create')
    flag=false
    for arg in "$@"; do
      case "$arg" in --prerelease=*) flag="${arg#--prerelease=}";; esac
    done
    jq -n --argjson flag "$flag" --rawfile body dist/release-journal.txt \
      '{draft: true, prerelease: $flag, body: $body, assets: []}' > release.json
    ;;
  'api '*) cat release.json ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
`,
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			outputPath := filepath.Join(dir, "outputs")
			env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REF_NAME="+test.tag, "GITHUB_SHA="+strings.Repeat("a", 40),
				"GITHUB_REF=refs/tags/"+test.tag, "GITHUB_REPOSITORY="+repositoryName,
				"GITHUB_OUTPUT="+outputPath, "RUNNER_TEMP="+dir)
			run := func(script string, extraEnv ...string) {
				t.Helper()
				command := exec.Command("bash", "-c", script)
				command.Dir = dir
				command.Env = append(append([]string{}, env...), extraEnv...)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("workflow step: %v\n%s", err, output)
				}
			}
			run(strings.ReplaceAll(steps["release"].Run, "${{ github.event.repository.default_branch }}", "master"))
			outputs, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("version=%s\nprerelease=%s\n", strings.TrimPrefix(test.tag, "v"), test.prerelease)
			if string(outputs) != want {
				t.Fatalf("metadata = %q, want %q", outputs, want)
			}
			if err := os.Mkdir(filepath.Join(dir, "dist"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "dist", "release-journal.txt"), []byte("prepared journal\n"), 0600); err != nil {
				t.Fatal(err)
			}
			run(strings.ReplaceAll(steps["draft"].Run, "${{ steps.release.outputs.version }}", strings.TrimPrefix(test.tag, "v")),
				"RELEASE_PRERELEASE="+test.prerelease)
		})
	}
}

func TestReleaseWorkflowRejectsWrongPrereleaseState(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"transaction", "draft", "publish-release"} {
		programs, err := stepJQPrograms(releaseWorkflowSteps(t)[id])
		if err != nil {
			t.Fatal(err)
		}
		checks := 0
		for _, program := range programs {
			if !strings.Contains(program, ".prerelease") {
				continue
			}
			checks++
			for _, expected := range []string{"true", "false"} {
				for _, actual := range []string{"true", "false", "null", `"true"`, `"false"`} {
					command := exec.Command("jq", "-e", "--argjson", "expected", expected, program)
					command.Stdin = strings.NewReader(`{"prerelease":` + actual + `}`)
					output, err := command.CombinedOutput()
					if (err == nil) != (actual == expected) {
						t.Fatalf("%s accepted=%t for prerelease=%s, expected=%s: %s", id, err == nil, actual, expected, output)
					}
				}
			}
		}
		if checks == 0 {
			t.Fatalf("%s does not check the prerelease state", id)
		}
	}
}

func releaseWorkflowSteps(t *testing.T) map[string]workflowStep {
	t.Helper()
	document, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowDocument
	if err := yaml.Unmarshal(document, &workflow); err != nil {
		t.Fatal(err)
	}
	steps, err := stepsByID(workflow.Jobs["publish"].Steps)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}
