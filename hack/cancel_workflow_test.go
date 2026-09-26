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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestVerifyCancelWorkflowRejectsDangerousMutations(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", cancelWorkflowPath)
	workflow := readTestWorkflow(t, path)
	if err := verifyCancelWorkflow(path); err != nil {
		t.Fatalf("verifyCancelWorkflow(valid) error = %v", err)
	}
	tests := map[string]struct {
		old       string
		new       string
		wantError string
	}{
		// The token can cancel master's run and a release tag's. A listing of
		// push runs is the mutation that would do it.
		"push runs listed": {
			old:       "              -f event=pull_request \\\n",
			new:       "              -f event=push \\\n",
			wantError: "must list only pull_request runs",
		},
		"event filter dropped": {
			old:       "              -f event=pull_request \\\n",
			new:       "",
			wantError: "must list only pull_request runs",
		},
		"push runs selected": {
			old:       "select(.event == \"pull_request\" and",
			new:       "select(.event == \"push\" and",
			wantError: "must cancel only pull_request runs",
		},
		"every event selected": {
			old:       "select(.event == \"pull_request\" and",
			new:       "select(.event != \"\" and",
			wantError: "must cancel only pull_request runs",
		},
		"every branch listed": {
			old:       "              -f branch=\"$HEAD_BRANCH\" \\\n",
			new:       "",
			wantError: "must list only the closed pull request's head branch",
		},
		"every branch selected": {
			old:       ".head_branch == $branch and",
			new:       ".head_branch != \"\" and",
			wantError: "must cancel only runs on the closed pull request's head branch",
		},
		"the base branch instead of the head branch": {
			old:       "HEAD_BRANCH: ${{ github.event.pull_request.head.ref }}",
			new:       "HEAD_BRANCH: ${{ github.event.pull_request.base.ref }}",
			wantError: "must bind the closed pull request's head branch",
		},
		// A fork can push a branch named like one of this repository's.
		"a fork's branch of the same name": {
			old:       "                       .head_repository.full_name == $repository and\n",
			new:       "",
			wantError: "must cancel only runs from this repository",
		},
		"own run canceled": {
			old:       ".id != $self) |",
			new:       ".id > 0) |",
			wantError: "must leave its own run to finish",
		},
		"a second listing": {
			old:       "          done < <(sort -u \"$run_ids_file\")\n",
			new:       "          done < <(sort -u \"$run_ids_file\")\n          gh run list --limit 100 --json databaseId --jq '.[].databaseId' | xargs -n1 gh run cancel\n",
			wantError: "differs from the audited one",
		},
		"wider permission": {
			old:       "permissions:\n  actions: write\n",
			new:       "permissions:\n  actions: write\n  contents: write\n",
			wantError: "must hold actions: write and nothing else",
		},
		"every permission": {
			old:       "permissions:\n  actions: write\n",
			new:       "permissions: write-all\n",
			wantError: "cannot unmarshal",
		},
		"job permissions override": {
			old:       "    timeout-minutes: 5\n",
			new:       "    timeout-minutes: 5\n    permissions:\n      actions: write\n      pull-requests: write\n",
			wantError: `key "permissions" is outside the audited shape`,
		},
		// Concurrency groups are shared across workflows: one named after CI's
		// cancels CI's runs with no command at all.
		"workflow concurrency": {
			old:       "permissions:\n  actions: write\n",
			new:       "permissions:\n  actions: write\n\nconcurrency:\n  group: ci-CI-refs/heads/master\n  cancel-in-progress: true\n",
			wantError: `workflow key "concurrency" is outside the audited shape`,
		},
		"job concurrency": {
			old:       "    timeout-minutes: 5\n",
			new:       "    timeout-minutes: 5\n    concurrency:\n      group: ci-CI-refs/heads/master\n      cancel-in-progress: true\n",
			wantError: `key "concurrency" is outside the audited shape`,
		},
		"step continues on error": {
			old:       "        shell: bash\n",
			new:       "        shell: bash\n        continue-on-error: true\n",
			wantError: `step key "continue-on-error" is outside the audited shape`,
		},
		"fork pull request not skipped": {
			old:       "    if: github.event.pull_request.head.repo.full_name == github.repository\n",
			new:       "",
			wantError: "must skip a pull request from a fork",
		},
		"triggered by a push": {
			old:       "on:\n  pull_request:\n    types: [closed]\n",
			new:       "on:\n  pull_request:\n    types: [closed]\n  push:\n    branches: [master]\n",
			wantError: "must run only when a pull request closes",
		},
		"triggered by every pull request event": {
			old:       "    types: [closed]\n",
			new:       "    types: [closed, synchronize]\n",
			wantError: "must run only when a pull request closes",
		},
		"triggered by an opened pull request": {
			old:       "    types: [closed]\n",
			new:       "",
			wantError: "must run only when a pull request closes",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedWorkflow(t, workflow, test.old, test.new)
			err := verifyCancelWorkflowSemanticsAtPath(path)
			if err == nil {
				t.Fatal("verifyCancelWorkflowSemantics() accepted a dangerous mutation")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyCancelWorkflowSemantics() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyCancelWorkflowDigestRejectsSemanticNoOp(t *testing.T) {
	t.Parallel()

	workflow := readTestWorkflow(t, filepath.Join("..", cancelWorkflowPath))
	path := writeMutatedWorkflow(
		t,
		workflow,
		"      - name: Cancel the head branch's unfinished pull request runs\n",
		"      - name: Cancel the head branch's unfinished pull request runs # audited policy changed\n",
	)
	if err := verifyCancelWorkflowSemanticsAtPath(path); err != nil {
		t.Fatalf("semantic verifier unexpectedly caught a comment: %v", err)
	}
	if err := verifyCancelWorkflow(path); err == nil || !strings.Contains(err.Error(), "workflow digest") {
		t.Fatalf("verifyCancelWorkflow() error = %v, want whole-workflow digest rejection", err)
	}
}

// The verifier holds the command to its text; this runs that text. A stub gh
// answers every listing with runs the API filters would already have dropped,
// so what reaches the cancel request is decided by the command's own filter,
// and each run it must not cancel is one a real mistake would have canceled.
func TestCancelWorkflowCancelsOnlyTheClosedPullRequestsRuns(t *testing.T) {
	t.Parallel()

	workflow, _, err := readWorkflow(filepath.Join("..", cancelWorkflowPath))
	if err != nil {
		t.Fatal(err)
	}
	step, err := requireWorkflowStep(cancelWorkflowPath, "cancel", workflow.Jobs["cancel"], "cancel-runs")
	if err != nil {
		t.Fatal(err)
	}

	type run struct {
		ID         int64  `json:"id"`
		Event      string `json:"event"`
		HeadBranch string `json:"head_branch"`
		Repository string `json:"repository"`
	}
	const (
		repository = "stokaro/ptah-operator"
		branch     = "feature"
		self       = 900
	)
	listing := func(runs ...run) string {
		type headRepository struct {
			FullName string `json:"full_name"`
		}
		type workflowRun struct {
			ID             int64          `json:"id"`
			Event          string         `json:"event"`
			HeadBranch     string         `json:"head_branch"`
			HeadRepository headRepository `json:"head_repository"`
		}
		page := struct {
			TotalCount   int           `json:"total_count"`
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}{TotalCount: len(runs), WorkflowRuns: []workflowRun{}}
		for _, item := range runs {
			page.WorkflowRuns = append(page.WorkflowRuns, workflowRun{
				ID: item.ID, Event: item.Event, HeadBranch: item.HeadBranch,
				HeadRepository: headRepository{FullName: item.Repository},
			})
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	statuses := []string{"requested", "queued", "pending", "waiting", "in_progress"}
	standard := map[string]string{
		"requested": listing(),
		"queued": listing(
			run{ID: 105, Event: "pull_request", HeadBranch: branch, Repository: repository},
			run{ID: 106, Event: "schedule", HeadBranch: branch, Repository: repository},
		),
		"pending": listing(
			run{ID: 107, Event: "pull_request", HeadBranch: branch, Repository: repository},
		),
		"waiting": listing(),
		"in_progress": listing(
			run{ID: 101, Event: "pull_request", HeadBranch: branch, Repository: repository},
			run{ID: 102, Event: "push", HeadBranch: branch, Repository: repository},
			run{ID: 103, Event: "pull_request", HeadBranch: "master", Repository: repository},
			run{ID: 104, Event: "pull_request", HeadBranch: branch, Repository: "someone/ptah-operator"},
			run{ID: self, Event: "pull_request", HeadBranch: branch, Repository: repository},
			// Listed twice, as a run changing status between two listings is.
			run{ID: 101, Event: "pull_request", HeadBranch: branch, Repository: repository},
		),
	}

	type outcome struct {
		canceled []string
		listings []string
		output   string
		exitCode int
	}
	execute := func(t *testing.T, pages map[string]string, refused map[string]string) outcome {
		t.Helper()
		directory := t.TempDir()
		fixtures := filepath.Join(directory, "fixtures")
		bin := filepath.Join(directory, "bin")
		for _, dir := range []string{fixtures, bin} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for status, page := range pages {
			if err := os.WriteFile(filepath.Join(fixtures, "runs-"+status+".json"), []byte(page), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// A refused cancel is GitHub's 409; the file holds the status the
		// run reports when it is read back.
		for id, status := range refused {
			if err := os.WriteFile(filepath.Join(fixtures, "refuse-"+id), []byte(status+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		const stub = `#!/bin/sh
method=GET
path=
status=
previous=
for argument in "$@"; do
  [ "$previous" = --method ] && method=$argument
  case "$argument" in
    repos/*) path=$argument ;;
    status=*) status=${argument#status=} ;;
  esac
  previous=$argument
done
case "$method $path" in
  "GET repos/stokaro/ptah-operator/actions/runs")
    printf '%s\n' "$*" >> "$STUB_DIR/listings"
    cat "$STUB_DIR/fixtures/runs-$status.json"
    ;;
  "POST repos/stokaro/ptah-operator/actions/runs/"*/cancel)
    id=${path#repos/stokaro/ptah-operator/actions/runs/}
    id=${id%/cancel}
    if [ -e "$STUB_DIR/fixtures/refuse-$id" ]; then
      echo "HTTP 409: Cannot cancel a workflow run that is completed." >&2
      exit 1
    fi
    printf '%s\n' "$id" >> "$STUB_DIR/canceled"
    ;;
  "GET repos/stokaro/ptah-operator/actions/runs/"*)
    cat "$STUB_DIR/fixtures/refuse-${path##*/}"
    ;;
  *)
    echo "unexpected gh call: $*" >&2
    exit 2
    ;;
esac
`
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
		runnerTemp := filepath.Join(directory, "runner")
		if err := os.Mkdir(runnerTemp, 0o700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-c", step.Run)
		command.Env = []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"STUB_DIR=" + directory,
			"RUNNER_TEMP=" + runnerTemp,
			"GITHUB_REPOSITORY=" + repository,
			"GITHUB_RUN_ID=900",
			"GH_TOKEN=token",
			"HEAD_BRANCH=" + branch,
		}
		output, err := command.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("the cancellation did not finish: %v", ctx.Err())
		}
		result := outcome{output: string(output)}
		if err != nil {
			if command.ProcessState == nil {
				t.Fatalf("the cancellation did not run: %v", err)
			}
			result.exitCode = command.ProcessState.ExitCode()
		}
		read := func(name string) []string {
			contents, err := os.ReadFile(filepath.Join(directory, name))
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				t.Fatal(err)
			}
			return strings.Fields(strings.TrimSpace(string(contents)))
		}
		result.canceled = read("canceled")
		if contents, err := os.ReadFile(filepath.Join(directory, "listings")); err == nil {
			result.listings = strings.Split(strings.TrimSpace(string(contents)), "\n")
		}
		return result
	}

	t.Run("only the closed pull request's runs", func(t *testing.T) {
		t.Parallel()
		got := execute(t, standard, map[string]string{"107": "completed"})
		if got.exitCode != 0 {
			t.Fatalf("the cancellation failed with %d:\n%s", got.exitCode, got.output)
		}
		// 107 concluded before its cancel arrived; that is not a failure.
		if want := []string{"101", "105"}; !slices.Equal(got.canceled, want) {
			t.Fatalf("canceled %v, want %v\n%s", got.canceled, want, got.output)
		}
		// Each listing asks the API for the branch and the event, so the
		// filter above is the second of two rather than the only one.
		if len(got.listings) != len(statuses) {
			t.Fatalf("listed %d times, want once per unfinished status: %q", len(got.listings), got.listings)
		}
		for index, status := range statuses {
			listed := got.listings[index]
			for _, want := range []string{"-f branch=" + branch, "-f event=pull_request", "-f status=" + status} {
				if !strings.Contains(listed, want) {
					t.Fatalf("listing %q does not ask for %q", listed, want)
				}
			}
		}
	})
	t.Run("a run that could not be canceled", func(t *testing.T) {
		t.Parallel()
		got := execute(t, standard, map[string]string{"107": "in_progress"})
		if got.exitCode != 1 || !strings.Contains(got.output, "run 107 is in_progress and could not be canceled") {
			t.Fatalf("the cancellation exited %d, want 1 naming run 107:\n%s", got.exitCode, got.output)
		}
	})
	t.Run("a partial page", func(t *testing.T) {
		t.Parallel()
		pages := map[string]string{}
		for status, page := range standard {
			pages[status] = page
		}
		pages["queued"] = strings.Replace(pages["queued"], `"total_count":2`, `"total_count":101`, 1)
		got := execute(t, pages, nil)
		if got.exitCode != 1 || !strings.Contains(got.output, "the queued run listing for feature is a partial page") {
			t.Fatalf("the cancellation exited %d, want 1 on a partial page:\n%s", got.exitCode, got.output)
		}
		if len(got.canceled) != 0 {
			t.Fatalf("canceled %v from a partial listing", got.canceled)
		}
	})
}

func verifyCancelWorkflowSemanticsAtPath(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	return verifyCancelWorkflowSemantics(path, workflow, contents)
}
