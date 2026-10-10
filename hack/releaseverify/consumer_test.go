package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release preflight consumes a CI run in three reads: the run listing for
// the release commit, that run's jobs, and that run's artifact inventory. Each
// read is a jq program in the workflow. These tests run the workflow's own
// programs over what the GitHub API returned for the master run that proved
// commit 46bc2299 (run 37921415190), and over the mutations of that reading the
// preflight must refuse.
//
// The readings in testdata/ci-run-37921415190 are the API responses projected
// to the fields the programs read:
//
//	gh api repos/stokaro/ptah-operator/actions/workflows/ci.yml/runs -f head_sha=<sha>
//	gh api repos/stokaro/ptah-operator/actions/runs/37921415190/jobs -f filter=latest
//	gh api --paginate repos/stokaro/ptah-operator/actions/runs/37921415190/artifacts
//
// The run listing is deliberately not filtered by event: it carries the native
// arm64 workflow_dispatch run of the same commit, which proved the same tree
// and must still not be read as the release's support evidence.
const (
	consumerReadings = "testdata/ci-run-37921415190"
	consumerSource   = "46bc229995d0e2fccdebc42f12eb9709b734958b"
	consumerRun      = "37921415190"
)

func readConsumerReading(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(consumerReadings, name))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// releaseJQProgram is the one program in the release workflow that carries
// marker. A copy of it would compile and prove nothing about what ships.
func releaseJQProgram(t *testing.T, marker string) string {
	t.Helper()
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	programs, err := workflowJQPrograms(workflow)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, program := range programs {
		if strings.Contains(program, marker) {
			found = append(found, program)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the release workflow has %d jq programs carrying %q, want 1", len(found), marker)
	}
	return found[0]
}

func runJQ(t *testing.T, document any, arguments ...string) (string, error) {
	t.Helper()
	input, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("jq", arguments...)
	command.Stdin = strings.NewReader(string(input))
	output, err := command.CombinedOutput()
	return string(output), err
}

// cloneReading copies a reading so a mutation cannot reach another row.
func cloneReading(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var copied map[string]any
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

// each applies change to every element of the named list whose field matches.
// It reports how many it changed, so a row whose mutation reached nothing fails
// instead of passing on an unchanged reading.
func each(document map[string]any, list, field, value string, change func(map[string]any)) int {
	items, _ := document[list].([]any)
	changed := 0
	for _, item := range items {
		object, ok := item.(map[string]any)
		if ok && object[field] == value {
			change(object)
			changed++
		}
	}
	return changed
}

func without(document map[string]any, list, field, value string) int {
	items, _ := document[list].([]any)
	kept := make([]any, 0, len(items))
	removed := 0
	for _, item := range items {
		if object, ok := item.(map[string]any); ok && object[field] == value {
			removed++
			continue
		}
		kept = append(kept, item)
	}
	document[list] = kept
	return removed
}

func TestTheReleaseSelectsOnlyTheExactCommitsPushRun(t *testing.T) {
	t.Parallel()
	program := releaseJQProgram(t, ".workflow_runs[]")
	reading := readConsumerReading(t, "runs.json")
	pushRunID := json.Number(consumerRun)

	for _, row := range []struct {
		name   string
		mutate func(map[string]any) int
		want   string
	}{
		{name: "the real listing, with the arm64 dispatch run beside it", want: consumerRun + "\n"},
		{
			name: "the push run built another commit",
			mutate: func(d map[string]any) int {
				return each(d, "workflow_runs", "event", "push", func(run map[string]any) {
					run["head_sha"] = "0286882b5e0a3f3d6f2bd0b54a1e4bb1d2b0e7c1"
				})
			},
		},
		{
			name: "the push run failed",
			mutate: func(d map[string]any) int {
				return each(d, "workflow_runs", "event", "push", func(run map[string]any) { run["conclusion"] = "failure" })
			},
		},
		{
			name: "the push run was canceled",
			mutate: func(d map[string]any) int {
				return each(d, "workflow_runs", "event", "push", func(run map[string]any) { run["conclusion"] = "cancelled" })
			},
		},
		{
			name: "the push run has not finished",
			mutate: func(d map[string]any) int {
				return each(d, "workflow_runs", "event", "push", func(run map[string]any) {
					run["status"] = "in_progress"
					run["conclusion"] = nil
				})
			},
		},
		{
			name: "the push run is from another branch",
			mutate: func(d map[string]any) int {
				return each(d, "workflow_runs", "event", "push", func(run map[string]any) { run["head_branch"] = "qualify/020-final" })
			},
		},
		{
			name: "only the dispatch run of the commit remains",
			mutate: func(d map[string]any) int {
				return without(d, "workflow_runs", "event", "push")
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			document := cloneReading(t, reading)
			if row.mutate != nil && row.mutate(document) == 0 {
				t.Fatal("the mutation reached no run in the reading")
			}
			output, err := runJQ(t, document, "-r", "--arg", "branch", "master", "--arg", "sha", consumerSource, program)
			if err != nil {
				t.Fatalf("the selection failed instead of selecting: %v\n%s", err, output)
			}
			if output != row.want {
				t.Fatalf("selected %q, want %q (the push run is %s)", output, row.want, pushRunID)
			}
		})
	}
}

func TestTheReleaseRequiresTheSupportGateOfThatRun(t *testing.T) {
	t.Parallel()
	program := releaseJQProgram(t, `"Kubernetes support gate"`)
	reading := readConsumerReading(t, "jobs.json")
	const gate = "Kubernetes support gate"

	for _, row := range []struct {
		name     string
		mutate   func(map[string]any) int
		accepted bool
	}{
		{name: "the real jobs of the run", accepted: true},
		{
			name: "the gate failed",
			mutate: func(d map[string]any) int {
				return each(d, "jobs", "name", gate, func(job map[string]any) { job["conclusion"] = "failure" })
			},
		},
		{
			name: "the gate was skipped",
			mutate: func(d map[string]any) int {
				return each(d, "jobs", "name", gate, func(job map[string]any) { job["conclusion"] = "skipped" })
			},
		},
		{
			name: "the gate ran for another commit",
			mutate: func(d map[string]any) int {
				return each(d, "jobs", "name", gate, func(job map[string]any) {
					job["head_sha"] = "0286882b5e0a3f3d6f2bd0b54a1e4bb1d2b0e7c1"
				})
			},
		},
		{
			name: "the gate is absent",
			mutate: func(d map[string]any) int {
				return without(d, "jobs", "name", gate)
			},
		},
		{
			name: "the gate appears twice",
			mutate: func(d map[string]any) int {
				jobs := d["jobs"].([]any)
				for _, item := range jobs {
					if item.(map[string]any)["name"] == gate {
						d["jobs"] = append(jobs, item)
						return 1
					}
				}
				return 0
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			document := cloneReading(t, reading)
			if row.mutate != nil && row.mutate(document) == 0 {
				t.Fatal("the mutation reached no job in the reading")
			}
			output, err := runJQ(t, document, "-e", "--arg", "sha", consumerSource, program)
			switch {
			case row.accepted && err != nil:
				t.Fatalf("the gate check refused the run it must accept: %v\n%s", err, output)
			case !row.accepted && err == nil:
				t.Fatalf("the gate check accepted a run it must refuse:\n%s", output)
			}
		})
	}
}

func TestTheReleaseReadsTheCompleteArtifactInventoryOfThatRun(t *testing.T) {
	t.Parallel()
	program := releaseJQProgram(t, inventoryFilterMarker)
	reading := readConsumerReading(t, "artifacts.json")
	names := filepath.Join(t.TempDir(), "expected")
	if err := os.WriteFile(names, []byte(strings.Join(expectedCharts, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, _ := reading["artifacts"].([]any)
	if count := len(artifacts); count != 31 {
		t.Fatalf("the reading holds %d artifacts; the run published 31", count)
	}

	for _, row := range []struct {
		name     string
		mutate   func(map[string]any) int
		accepted bool
	}{
		{name: "every artifact the run published", accepted: true},
		{
			name: "an unrelated timing bundle expired",
			mutate: func(d map[string]any) int {
				return each(d, "artifacts", "name", "lifecycle-timings-1-35-lifecycle", func(a map[string]any) { a["expired"] = true })
			},
			accepted: true,
		},
		{
			name: "a chart is missing",
			mutate: func(d map[string]any) int {
				removed := without(d, "artifacts", "name", expectedCharts[1])
				d["total_count"] = len(d["artifacts"].([]any))
				return removed
			},
		},
		{
			name: "a chart expired",
			mutate: func(d map[string]any) int {
				return each(d, "artifacts", "name", expectedCharts[2], func(a map[string]any) { a["expired"] = true })
			},
		},
		{
			name: "a chart is empty",
			mutate: func(d map[string]any) int {
				return each(d, "artifacts", "name", expectedCharts[0], func(a map[string]any) { a["size_in_bytes"] = 0 })
			},
		},
		{
			name: "a chart has no size",
			mutate: func(d map[string]any) int {
				return each(d, "artifacts", "name", expectedCharts[0], func(a map[string]any) { delete(a, "size_in_bytes") })
			},
		},
		{
			name: "a chart appears twice",
			mutate: func(d map[string]any) int {
				items := d["artifacts"].([]any)
				for _, item := range items {
					if item.(map[string]any)["name"] == expectedCharts[0] {
						d["artifacts"] = append(items, item)
						d["total_count"] = len(items) + 1
						return 1
					}
				}
				return 0
			},
		},
		{
			name: "the inventory is one page of a longer one",
			mutate: func(d map[string]any) int {
				d["total_count"] = 131
				return 1
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			document := cloneReading(t, reading)
			if row.mutate != nil && row.mutate(document) == 0 {
				t.Fatal("the mutation reached no artifact in the reading")
			}
			output, err := runJQ(t, document, "-er", "--rawfile", "expected_names", names, program)
			switch {
			case row.accepted && err != nil:
				t.Fatalf("the inventory check refused the run it must accept: %v\n%s", err, output)
			case !row.accepted && err == nil:
				t.Fatalf("the inventory check accepted a run it must refuse:\n%s", output)
			}
		})
	}
}
