// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The lab builds a cluster of its own and keeps its kubeconfig in a file of its
// own. A scenario hands that file to the processes it starts, and no child can
// export it into the reader's shell -- so a manual command on this page runs
// against whatever cluster the reader already had, or against none.
//
// Running the recorder does not exercise this. The recorder passes the
// kubeconfig itself; the page is the only thing that selects it for a person,
// and nothing but this reads the page.
const tryItPagePath = "../docs/site/src/content/docs/start/try-it.md"

// What the reader is told to run and what a shell would do with it. Blocks that
// build the lab or record a scenario are the long-running half of the page and
// are not what this measures.
var (
	shellBlockPattern = regexp.MustCompile("(?s)```sh\n(.*?)```")
	buildsTheLab      = []string{"git clone", "make ", "go run "}
)

func TestTheTryItPageRunsAgainstTheLabCluster(t *testing.T) {
	t.Parallel()

	page, err := os.ReadFile(tryItPagePath)
	if err != nil {
		t.Fatalf("read %s: %v", tryItPagePath, err)
	}
	session := readerSession(t, string(page))

	// The lab, as the bootstrap leaves it, and a shell pointed somewhere else.
	root := t.TempDir()
	labKubeconfig := filepath.Join(root, "lab", "kubeconfig")
	readerKubeconfig := filepath.Join(root, "reader", "kubeconfig")
	for _, path := range []string{labKubeconfig, readerKubeconfig} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	observed := filepath.Join(root, "kubeconfig-per-call")
	writeExecutable(t, filepath.Join(root, "demo", "bin", "lab"), `#!/bin/sh
case "$1" in
kubeconfig) printf '%s\n' "$LAB_KUBECONFIG" ;;
namespace) printf 'demo\n' ;;
tools) printf '%s\n' "$LAB_TOOLS" ;;
*) printf 'lab: this session does not run %s\n' "$1" >&2; exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(root, "stubs", "kubectl"), `#!/bin/sh
printf '%s\n' "${KUBECONFIG-<unset>}" >>"$OBSERVED_KUBECONFIG"
`)

	command := exec.Command("sh", "-eu", "-c", session)
	command.Dir = root
	command.Env = append(os.Environ(),
		"PATH="+filepath.Join(root, "stubs")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG="+readerKubeconfig,
		"LAB_KUBECONFIG="+labKubeconfig,
		"LAB_TOOLS="+filepath.Join(root, "stubs"),
		"OBSERVED_KUBECONFIG="+observed,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the documented session failed: %v\n%s\nsession:\n%s", err, output, session)
	}

	seen, err := os.ReadFile(observed)
	if err != nil {
		t.Fatalf("the documented session ran no kubectl at all, so this measures nothing: %v", err)
	}
	calls := strings.Fields(string(seen))
	if len(calls) < 2 {
		t.Fatalf("the documented session ran %d kubectl commands, want the page's inspection commands", len(calls))
	}
	for _, kubeconfig := range calls {
		if kubeconfig != labKubeconfig {
			t.Fatalf("a documented command ran against %q rather than the lab's %q, so it reaches the reader's own cluster",
				kubeconfig, labKubeconfig)
		}
	}
}

// An exported variable outlives the commands it was exported for, so the page
// has to say how to put the reader's own context back.
func TestTheTryItPageSaysHowToLeaveTheLabContext(t *testing.T) {
	t.Parallel()

	page, err := os.ReadFile(tryItPagePath)
	if err != nil {
		t.Fatalf("read %s: %v", tryItPagePath, err)
	}
	if !strings.Contains(string(page), "unset KUBECONFIG") {
		t.Fatal("the page exports KUBECONFIG and never says how to stop using it")
	}
}

// readerSession is the page's inspection commands as one shell would run them,
// in order, because a variable one block sets is what the next block uses.
func readerSession(t *testing.T, page string) string {
	t.Helper()

	var blocks []string
	for _, match := range shellBlockPattern.FindAllStringSubmatch(page, -1) {
		block := match[1]
		if containsAny(block, buildsTheLab) {
			continue
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		t.Fatalf("%s documents no commands a reader runs against the lab", tryItPagePath)
	}
	return strings.Join(blocks, "\n")
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
