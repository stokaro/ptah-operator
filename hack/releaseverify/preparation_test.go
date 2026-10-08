package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func releaseStepsForTest(t *testing.T) map[string]workflowStep {
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

func TestPublicationExecutesOnlyForTheQualifiedManifest(t *testing.T) {
	t.Parallel()
	step := releaseStepsForTest(t)["publish-release"]
	for _, test := range []struct {
		name, action                                        string
		wrongDigest, changedAsset, wantFailure, wantPublish bool
	}{
		{name: "prepare keeps the signed draft", action: "prepare"},
		{name: "publish the selected manifest", action: "publish", wantPublish: true},
		{name: "refuse another manifest", action: "publish", wrongDigest: true, wantFailure: true},
		{name: "refuse another downloaded asset", action: "publish", changedAsset: true, wantFailure: true},
		{name: "refuse an unknown action", action: "unexpected", wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			for _, name := range []string{"bin", "dist", "remote", "tmp"} {
				if err := os.Mkdir(filepath.Join(directory, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			names := []string{"ptah-operator-0.2.0.tgz", "release-manifest.txt", "SHA256SUMS", "acceptance-evidence.tar.gz",
				"kubectl-ptah-darwin-amd64", "kubectl-ptah-darwin-arm64", "kubectl-ptah-linux-amd64", "kubectl-ptah-linux-arm64"}
			var assets []map[string]any
			for _, name := range names {
				for _, location := range []string{"dist", "remote"} {
					if err := os.WriteFile(filepath.Join(directory, location, name), []byte(name+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				assets = append(assets, map[string]any{"name": name, "state": "uploaded"})
			}
			if test.changedAsset {
				if err := os.WriteFile(filepath.Join(directory, "remote", names[0]), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			release, _ := json.Marshal(map[string]any{"draft": true, "immutable": false, "body": "release-manifest.txt\n", "assets": assets})
			if err := os.WriteFile(filepath.Join(directory, "release.json"), release, 0o600); err != nil {
				t.Fatal(err)
			}
			// Only the remote service and the already-tested asset verifier are
			// replaced. The workflow's complete final shell step runs unchanged.
			stubs := map[string]string{
				"go": "#!/bin/sh\nexit 0\n",
				"gh": `#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
root = pathlib.Path(os.environ['PTAH_RELEASE_TEST_ROOT'])
args = sys.argv[1:]
if args[0] == 'api':
    value = json.loads((root / 'release.json').read_text())
    if (root / 'published').exists(): value.update(draft=False, immutable=True)
    print(json.dumps(value))
elif args[:2] == ['release', 'download']:
    name = args[args.index('--pattern') + 1]
    destination = pathlib.Path(args[args.index('--dir') + 1])
    shutil.copyfile(root / 'remote' / name, destination / name)
elif args[:2] == ['release', 'edit']:
    assert '--draft=false' in args and '--latest=false' in args
    (root / 'published').write_text('published')
elif args[:2] not in (['attestation', 'verify'], ['release', 'verify'], ['release', 'verify-asset']):
    raise SystemExit('unexpected gh command: ' + str(args))
`,
			}
			for name, body := range stubs {
				if err := os.WriteFile(filepath.Join(directory, "bin", name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte("release-manifest.txt\n")))
			if test.wrongDigest {
				digest = strings.Repeat("0", 64)
			}
			program := strings.NewReplacer(
				"${{ steps.chart-package.outputs.path }}", "dist/ptah-operator-0.2.0.tgz",
				"${{ steps.transaction.outputs.mode }}", "recover",
				"${{ steps.release.outputs.version }}", "0.2.0",
			).Replace(step.Run)
			if strings.Contains(program, "${{") {
				t.Fatal("unbound workflow expression")
			}
			command := exec.Command("bash", "-c", program)
			command.Dir = directory
			command.Env = append(os.Environ(), "PATH="+filepath.Join(directory, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
				"PTAH_RELEASE_TEST_ROOT="+directory, "RUNNER_TEMP="+filepath.Join(directory, "tmp"),
				"GITHUB_STEP_SUMMARY="+filepath.Join(directory, "summary"), "GITHUB_REF_NAME=v0.2.0",
				"GITHUB_REF=refs/tags/v0.2.0", "GITHUB_SHA="+strings.Repeat("a", 40),
				"GITHUB_REPOSITORY=stokaro/ptah-operator", "RELEASE_ACTION="+test.action,
				"QUALIFIED_MANIFEST_SHA256="+digest)
			output, err := command.CombinedOutput()
			if (err != nil) != test.wantFailure {
				t.Fatalf("exit = %v, output: %s", err, output)
			}
			_, statErr := os.Stat(filepath.Join(directory, "published"))
			if (statErr == nil) != test.wantPublish {
				t.Fatalf("publication = %v, want %v; %s", statErr == nil, test.wantPublish, output)
			}
		})
	}
}

func TestPublicationRefusesAnIncompleteTransactionBeforeBuilds(t *testing.T) {
	t.Parallel()
	program := releaseStepsForTest(t)["transaction"].Run
	start := strings.Index(program, `if [[ "$RELEASE_ACTION" == publish ]]; then`)
	if start < 0 {
		t.Fatal("missing publication transaction guard")
	}
	end := strings.Index(program[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("unterminated transaction guard")
	}
	guard := program[start : start+end+4]
	for _, state := range []string{"fresh", "prepared", "recover", "published"} {
		for _, wrongDigest := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrongDigest=%t", state, wrongDigest), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "manifest")
				content := []byte("qualified bytes")
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
				digest := fmt.Sprintf("%x", sha256.Sum256(content))
				if wrongDigest {
					digest = strings.Repeat("0", 64)
				}
				command := exec.Command("bash", "-c", "set -euo pipefail\n"+guard)
				command.Env = append(os.Environ(), "RELEASE_ACTION=publish", "release_state="+state, "state_path="+path, "QUALIFIED_MANIFEST_SHA256="+digest)
				output, err := command.CombinedOutput()
				wantFailure := wrongDigest || state == "fresh" || state == "prepared"
				if (err != nil) != wantFailure {
					t.Fatalf("exit = %v, output: %s", err, output)
				}
			})
		}
	}
}

func TestReleaseActionRequiresAnExplicitPublicationDigest(t *testing.T) {
	t.Parallel()
	program := releaseStepsForTest(t)["release"].Run
	end := strings.Index(program, "go run ./hack/releaseverify -tag")
	if end < 0 {
		t.Fatal("missing release metadata validation boundary")
	}
	for _, test := range []struct {
		action, digest string
		wantFailure    bool
	}{
		{"prepare", "", false},
		{"prepare", strings.Repeat("a", 64), true},
		{"publish", strings.Repeat("a", 64), false},
		{"publish", "", true},
		{"publish", "a-short-digest", true},
		{"unexpected", "", true},
	} {
		t.Run(test.action+"/"+test.digest, func(t *testing.T) {
			command := exec.Command("bash", "-c", program[:end])
			command.Env = append(os.Environ(), "RELEASE_ACTION="+test.action, "QUALIFIED_MANIFEST_SHA256="+test.digest)
			output, err := command.CombinedOutput()
			if (err != nil) != test.wantFailure {
				t.Fatalf("exit = %v, output: %s", err, output)
			}
		})
	}
}
