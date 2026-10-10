package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The release list GitHub serves can trail a write by a moment: prepare run
// 38033190293 created its draft and failed a second later on a read that did
// not show it. Each step that reads the list after writing to it polls until
// the list shows the write. These rows run the polling blocks the workflow
// carries, against a release list that answers from a script.

// readAfterWrite is one polling block: the step it is in, what the step has
// already set up when the block runs, a release the list shows once it has
// caught up, one it shows before that, and what the block says when it never
// catches up.
type readAfterWrite struct {
	step    string
	prelude string
	current string
	stale   string
	refusal string
}

var readsAfterWrites = []readAfterWrite{
	{
		step:    "draft",
		current: `{"draft":true,"assets":[],"body":"journal\n"}`,
		stale:   `null`,
		refusal: "the release list does not show the draft just created",
	},
	{
		step:    "finalize-journal",
		prelude: `printf 'manifest\n' > dist/release-manifest.txt`,
		current: `{"draft":true,"assets":[],"body":"manifest\n"}`,
		stale:   `{"draft":true,"assets":[],"body":"journal\n"}`,
		refusal: "the release list does not show the committed manifest",
	},
	{
		step:    "asset-sync",
		prelude: `expected_names="$(printf '%s\n' chart.tgz release-manifest.txt)"`,
		current: `{"draft":true,"assets":[{"name":"chart.tgz","state":"uploaded"},{"name":"release-manifest.txt","state":"uploaded"}]}`,
		stale:   `{"draft":true,"assets":[{"name":"chart.tgz","state":"uploaded"},{"name":"release-manifest.txt","state":"starter"}]}`,
		refusal: "the release list does not show every uploaded asset",
	},
	{
		step:    "publish-release",
		current: `{"draft":false,"immutable":true}`,
		stale:   `{"draft":true,"immutable":false}`,
		refusal: "the release list does not show the release published and immutable",
	},
}

// pollingBlock cuts the block out of a step: from the comment that names the
// lag through the check that the loop converged. A step that lost its block
// fails here.
func pollingBlock(t *testing.T, step string) string {
	t.Helper()
	run := releaseStepsForTest(t)[step].Run
	start := strings.Index(run, "# The release list can trail")
	if start < 0 {
		t.Fatalf("release step %q no longer polls the release list after its write", step)
	}
	const check = "if [[ \"$converged\" != true ]]; then\n"
	loop := strings.Index(run[start:], "\ndone\n"+check)
	if loop < 0 {
		t.Fatalf("release step %q does not check that its polling loop converged", step)
	}
	end := strings.Index(run[start+loop:], "\nfi\n")
	if end < 0 {
		t.Fatalf("release step %q leaves its convergence check open", step)
	}
	return run[start : start+loop+end+len("\nfi\n")]
}

// poll runs a block against a release list that answers with the given
// documents in order, repeating the last, and reports what the block printed,
// whether it failed, and how many times it read the list.
func poll(t *testing.T, row readAfterWrite, block string, answers ...string) (string, bool, int) {
	t.Helper()
	root := t.TempDir()
	stubs := filepath.Join(root, "bin")
	for _, directory := range []string{stubs, filepath.Join(root, "dist"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "answers"), []byte(strings.Join(answers, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The only go command a polling block runs is the release read.
	reader := `#!/bin/sh
set -eu
[ "$*" = "run ./hack/releaseverify -tag v0.2.0 -read-release" ] || { echo "unexpected go $*" >&2; exit 2; }
reads=$(( $(cat "$POLL_ROOT/reads" 2>/dev/null || echo 0) + 1 ))
echo "$reads" > "$POLL_ROOT/reads"
total=$(wc -l < "$POLL_ROOT/answers")
[ "$reads" -le "$total" ] || reads=$total
sed -n "${reads}p" "$POLL_ROOT/answers"
`
	for name, body := range map[string]string{"go": reader, "sleep": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", "-c", "set -euo pipefail\n"+row.prelude+"\n"+block+"printf 'converged\\n'\n")
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"POLL_ROOT="+root, "RELEASE_TAG=v0.2.0", "RUNNER_TEMP="+filepath.Join(root, "tmp"))
	output, err := command.CombinedOutput()
	raw, readErr := os.ReadFile(filepath.Join(root, "reads"))
	if readErr != nil {
		t.Fatalf("the block never read the release list: %v\n%s", readErr, output)
	}
	reads, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatal(convErr)
	}
	return string(output), err != nil, reads
}

func TestReleaseReadsAfterWritesWaitForTheListToCatchUp(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required to run the release workflow polling blocks")
	}
	for _, row := range readsAfterWrites {
		t.Run(row.step, func(t *testing.T) {
			t.Parallel()
			block := pollingBlock(t, row.step)

			output, failed, reads := poll(t, row, block, row.current)
			if failed || reads != 1 || !strings.Contains(output, "converged") {
				t.Fatalf("a list that already shows the write took %d reads, failed=%v:\n%s", reads, failed, output)
			}
			output, failed, reads = poll(t, row, block, row.stale, row.stale, row.current)
			if failed || reads != 3 {
				t.Fatalf("a list that caught up on the third read took %d reads, failed=%v:\n%s", reads, failed, output)
			}
			output, failed, reads = poll(t, row, block, row.stale)
			if !failed || reads != 8 || !strings.Contains(output, row.refusal) {
				t.Fatalf("a list that never catches up read %d times, failed=%v, said:\n%s", reads, failed, output)
			}

			// Without the retry budget the block reads once, and a list one
			// read behind fails the step, as it did in the run above.
			if strings.Count(block, "{1..8}") != 1 {
				t.Fatalf("release step %q no longer allows eight reads", row.step)
			}
			once := strings.Replace(block, "{1..8}", "{1..1}", 1)
			output, failed, _ = poll(t, row, once, row.stale, row.current)
			if !failed {
				t.Fatalf("a single read passed against a list one read behind, so this row cannot catch a lost retry:\n%s", output)
			}
		})
	}
}
