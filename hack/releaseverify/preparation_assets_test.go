package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual materialization step without client output or a newly
// generated evidence bundle. Only the already-qualified remote files exist.
func TestPublicationReusesQualifiedAssets(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"recover", "published"} {
		for _, defect := range []string{"none", "manifest", "chart", "missing client"} {
			t.Run(mode+"/"+defect, func(t *testing.T) {
				root := t.TempDir()
				for _, dir := range []string{"bin", "dist", "remote", "tmp"} {
					if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
						t.Fatal(err)
					}
				}
				files := map[string]string{
					"release-manifest.txt":       "image=ghcr.io/stokaro/ptah-operator@sha256:fixture\nexecutor=ghcr.io/stokaro/ptah-operator-executor@sha256:fixture\n",
					"ptah-operator-0.2.0.tgz":    "qualified chart",
					"SHA256SUMS":                 "qualified checksums",
					"acceptance-evidence.tar.gz": "original evidence, different from any newly selected run",
					"kubectl-ptah-darwin-amd64":  "qualified darwin amd64",
					"kubectl-ptah-darwin-arm64":  "qualified darwin arm64",
					"kubectl-ptah-linux-amd64":   "qualified linux amd64",
					"kubectl-ptah-linux-arm64":   "qualified linux arm64",
				}
				for name, body := range files {
					if err := os.WriteFile(filepath.Join(root, "remote", name), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range []string{"release-manifest.txt", "ptah-operator-0.2.0.tgz"} {
					if err := os.WriteFile(filepath.Join(root, "dist", name), []byte(files[name]), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if defect == "manifest" || defect == "chart" {
					name := "release-manifest.txt"
					if defect == "chart" {
						name = "ptah-operator-0.2.0.tgz"
					}
					if err := os.WriteFile(filepath.Join(root, "remote", name), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if defect == "missing client" {
					if err := os.Remove(filepath.Join(root, "remote", "kubectl-ptah-linux-arm64")); err != nil {
						t.Fatal(err)
					}
				}
				scripts := map[string]string{
					"gh": `#!/bin/sh
set -eu
[ "$1 $2 $3 $4" = 'release download v0.2.0 --dir' ]
cp "$PTAH_RELEASE_TEST_ROOT/remote/"* "$5/"
`,
					"go": `#!/bin/sh
set -eu
# The full parser has its own tests; this fixture keeps the inventory refusal.
for name in release-manifest.txt ptah-operator-0.2.0.tgz SHA256SUMS acceptance-evidence.tar.gz kubectl-ptah-darwin-amd64 kubectl-ptah-darwin-arm64 kubectl-ptah-linux-amd64 kubectl-ptah-linux-arm64; do
 test -f "dist/$name"
done
`,
				}
				for name, body := range scripts {
					if err := os.WriteFile(filepath.Join(root, "bin", name), []byte(body), 0700); err != nil {
						t.Fatal(err)
					}
				}
				step := releaseStepsForTest(t)["artifacts"].Run
				replacements := []string{
					"${{ steps.chart-package.outputs.path }}", "dist/ptah-operator-0.2.0.tgz",
					"${{ steps.transaction.outputs.mode }}", mode,
					"${{ steps.release.outputs.version }}", "0.2.0",
				}
				// These bindings are inside the fresh/prepared branch and must not run.
				for _, name := range []string{"image.outputs.digest", "stage-inspect.outputs.reuse", "stage-inspect.outputs.digest", "transaction.outputs.transaction", "transaction.outputs.image-tag", "executor-source.outputs.image", "executor-source.outputs.commit", "executor-source.outputs.version"} {
					replacements = append(replacements, "${{ steps."+name+" }}", "unused-build-output")
				}
				program := strings.NewReplacer(replacements...).Replace(step)
				if strings.Contains(program, "${{") {
					t.Fatal("unbound workflow expression")
				}
				action := "publish"
				if mode == "published" {
					action = "prepare"
				}
				command := exec.Command("bash", "-c", program)
				command.Dir = root
				command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
					"PTAH_RELEASE_TEST_ROOT="+root, "RUNNER_TEMP="+filepath.Join(root, "tmp"), "GITHUB_OUTPUT="+filepath.Join(root, "output"),
					"RELEASE_TAG=v0.2.0", "RELEASE_ACTION="+action, "SOURCE_REF=refs/heads/master", "GITHUB_SHA="+strings.Repeat("a", 40))
				output, err := command.CombinedOutput()
				if (err != nil) != (defect != "none") {
					t.Fatalf("error=%v output=%s", err, output)
				}
				if err == nil {
					for name, want := range files {
						got, err := os.ReadFile(filepath.Join(root, "dist", name))
						if err != nil || string(got) != want {
							t.Fatalf("%s changed: %s (%v)", name, got, err)
						}
					}
				}
			})
		}
	}
}
