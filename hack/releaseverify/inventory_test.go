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

// The preflight reads the artifact inventory of the CI run that proved the
// support matrix, and it used to require that run to contain the three
// installed charts and nothing else. CI publishes a shared image bundle and a
// timing bundle per matrix entry, so a successful run has nineteen artifacts,
// and the guard refused the evidence it was written to read.
//
// The tests around it checked workflow structure, script identity and filter
// syntax. None of them ran the filter over what the producer actually
// publishes, which is why a filter that compiled, matched its audited digest
// and refused every real run went unnoticed.
const inventoryFilterMarker = "so it is a partial page"

// expectedCharts is one installed chart per supported minor, as the step
// derives them from the support matrix.
var expectedCharts = []string{
	"installed-release-chart-1-35",
	"installed-release-chart-1-36",
	"installed-release-chart-1-37",
}

type inventoryArtifact struct {
	Name        string `json:"name"`
	Expired     bool   `json:"expired"`
	SizeInBytes int    `json:"size_in_bytes"`
}

func TestTheArtifactInventoryFilterReadsARealCIRun(t *testing.T) {
	t.Parallel()

	filter := inventoryFilter(t)
	names := filepath.Join(t.TempDir(), "expected")
	if err := os.WriteFile(names, []byte(strings.Join(expectedCharts, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		name      string
		artifacts []inventoryArtifact
		// total is what the API reports across every page. Zero means the
		// inventory is whole, which is the ordinary case.
		total    int
		accepted bool
	}{
		{
			// What a successful run publishes today: the three charts, the
			// shared image bundle, and one timing bundle per matrix entry.
			name:      "a successful CI run, with everything else it publishes",
			artifacts: successfulCIRun(),
			accepted:  true,
		},
		{
			name:      "the three charts and nothing else",
			artifacts: usableCharts(expectedCharts...),
			accepted:  true,
		},
		{
			// The regression the old filter would have called a failure: an
			// artifact this release does not read has expired.
			name: "an unrelated artifact that expired",
			artifacts: append(usableCharts(expectedCharts...),
				inventoryArtifact{Name: "lifecycle-timings-1-35-lifecycle", Expired: true, SizeInBytes: 2048}),
			accepted: true,
		},
		{
			name:      "a chart this release needs is missing",
			artifacts: usableCharts(expectedCharts[0], expectedCharts[1]),
			accepted:  false,
		},
		{
			name:      "a chart published twice",
			artifacts: usableCharts(append(expectedCharts, expectedCharts[2])...),
			accepted:  false,
		},
		{
			name: "a chart that expired",
			artifacts: append(usableCharts(expectedCharts[0], expectedCharts[1]),
				inventoryArtifact{Name: expectedCharts[2], Expired: true, SizeInBytes: 4096}),
			accepted: false,
		},
		{
			name: "a chart with nothing in it",
			artifacts: append(usableCharts(expectedCharts[0], expectedCharts[1]),
				inventoryArtifact{Name: expectedCharts[2], SizeInBytes: 0}),
			accepted: false,
		},
		{
			// A page that ended before the inventory did is not evidence that
			// the rest is absent, and it is not evidence that it is present.
			name:      "one page of a longer inventory",
			artifacts: successfulCIRun(),
			total:     40,
			accepted:  false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			total := row.total
			if total == 0 {
				total = len(row.artifacts)
			}
			inventory, err := json.Marshal(map[string]any{"total_count": total, "artifacts": row.artifacts})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("jq", "-er", "--rawfile", "expected_names", names, filter)
			command.Stdin = strings.NewReader(string(inventory))
			output, runErr := command.CombinedOutput()
			switch {
			case row.accepted && runErr != nil:
				t.Fatalf("the filter refused evidence it must accept: %v\n%s", runErr, output)
			case !row.accepted && runErr == nil:
				t.Fatalf("the filter accepted evidence it must refuse:\n%s", output)
			}
		})
	}
}

// inventoryFilter is the step's own program, read from the workflow that runs
// it. A copy would compile and prove nothing about what ships.
func inventoryFilter(t *testing.T) string {
	t.Helper()

	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	programs, err := workflowJQPrograms(workflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, program := range programs {
		if strings.Contains(program, inventoryFilterMarker) {
			return program
		}
	}
	t.Fatalf("the release workflow has no artifact inventory filter carrying %q", inventoryFilterMarker)
	return ""
}

func usableCharts(names ...string) []inventoryArtifact {
	artifacts := make([]inventoryArtifact, 0, len(names))
	for _, name := range names {
		artifacts = append(artifacts, inventoryArtifact{Name: name, SizeInBytes: 65536})
	}
	return artifacts
}

// successfulCIRun is the inventory of a run that proved the matrix: the charts,
// the image bundle the lifecycles share, and a timing bundle per matrix entry.
func successfulCIRun() []inventoryArtifact {
	artifacts := usableCharts(expectedCharts...)
	artifacts = append(artifacts, inventoryArtifact{Name: "shared-task-images", SizeInBytes: 1 << 28})
	for _, minor := range []string{"1-35", "1-36", "1-37"} {
		for _, suite := range []string{"lifecycle", "data-plane", "migrations-postgresql", "migrations-mysql", "crd-upgrade"} {
			artifacts = append(artifacts, inventoryArtifact{
				Name:        fmt.Sprintf("lifecycle-timings-%s-%s", minor, suite),
				SizeInBytes: 4096,
			})
		}
	}
	return artifacts
}
