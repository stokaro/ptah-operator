package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadScenariosReadsTheDirectory(t *testing.T) {
	t.Parallel()
	loaded, err := loadScenarios(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatalf("loadScenarios: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("loadScenarios read no scenario, so every check over them would pass by reading nothing")
	}
	seen := map[string]bool{}
	for _, one := range loaded {
		if seen[one.ID] {
			t.Fatalf("two scenarios answer to %s", one.ID)
		}
		seen[one.ID] = true
	}
}

func TestLoadScenariosRefusesAFileThatDoesNotMatchItsID(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	write(t, filepath.Join(directory, "wrong-name.yaml"), `
id: first-apply
title: Apply a schema
tagline: A tagline.
learn: A lesson.
start: An empty namespace.
tags: [Lifecycle]
steps:
  - run: kubectl get ptahschema
    expect:
      exit: 0
`)
	_, err := loadScenarios(directory)
	if err == nil {
		t.Fatal("loadScenarios accepted a file whose name is not its id")
	}
	if !strings.Contains(err.Error(), "first-apply.yaml") {
		t.Fatalf("loadScenarios said %q", err)
	}
}

// A key the format does not have is a step nobody runs. It is refused rather
// than ignored, because a scenario with a mistyped `expect` would otherwise
// record as a scenario with no expectation at all.
func TestLoadScenariosRefusesAnUnknownKey(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	write(t, filepath.Join(directory, "first-apply.yaml"), `
id: first-apply
title: Apply a schema
tagline: A tagline.
learn: A lesson.
start: An empty namespace.
tags: [Lifecycle]
steps:
  - run: kubectl get ptahschema
    expects:
      exit: 0
`)
	_, err := loadScenarios(directory)
	if err == nil {
		t.Fatal("loadScenarios accepted a key the format does not have")
	}
	if !strings.Contains(err.Error(), "expects") {
		t.Fatalf("loadScenarios said %q", err)
	}
}

func TestMergeReplacesOneRunAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	existing := []recording{{ID: "first-apply", Title: "old"}, {ID: "drift", Title: "old"}}
	merged := merge(existing, []recording{{ID: "drift", Title: "new"}})
	if len(merged) != 2 {
		t.Fatalf("merge returned %d runs", len(merged))
	}
	if merged[0].Title != "old" || merged[1].Title != "new" {
		t.Fatalf("merge returned %v", merged)
	}
}

func TestMergeAppendsARunTheFileDidNotHave(t *testing.T) {
	t.Parallel()
	merged := merge([]recording{{ID: "first-apply"}}, []recording{{ID: "drift"}})
	if len(merged) != 2 || merged[1].ID != "drift" {
		t.Fatalf("merge returned %v", merged)
	}
}

func TestSameLabSeparatesTwoLabs(t *testing.T) {
	t.Parallel()
	first := map[string]string{"KUBERNETES_VERSION": "1.37.0", "PTAH_VERSION": "v0.3.0"}
	if !sameLab(first, map[string]string{"KUBERNETES_VERSION": "1.37.0", "PTAH_VERSION": "v0.3.0"}) {
		t.Fatal("sameLab separated one lab from itself")
	}
	if sameLab(first, map[string]string{"KUBERNETES_VERSION": "1.36.0", "PTAH_VERSION": "v0.3.0"}) {
		t.Fatal("sameLab joined two labs on different Kubernetes releases")
	}
}

func TestLoadLabRefusesAValueTheBootstrapDidNotWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "environment")
	write(t, path, "E2E_KUBECONFIG=/tmp/kubeconfig\nE2E_TEST_NAMESPACE=\n")
	live, err := loadLab(path)
	if err != nil {
		t.Fatalf("loadLab: %v", err)
	}
	if _, err := live.required("E2E_TEST_NAMESPACE"); err == nil {
		t.Fatal("required accepted an empty value, so a scenario would have run in another namespace")
	}
	if _, err := live.environment("/repository"); err == nil {
		t.Fatal("environment accepted a lab with no namespace")
	}
}

func TestLabEnvironmentCarriesWhatAStepReads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "environment")
	write(t, path, strings.Join([]string{
		"E2E_KUBECONFIG=/tmp/kubeconfig",
		"E2E_TEST_NAMESPACE=demo",
		"E2E_OPERATOR_NAMESPACE=ptah-system",
		"E2E_CONTROLLER_NAME=ptah-operator",
		"E2E_EXECUTOR_IMAGE=registry.example/ptah@sha256:0123",
		"E2E_UNDECLARED=surprise",
		"",
	}, "\n"))
	live, err := loadLab(path)
	if err != nil {
		t.Fatalf("loadLab: %v", err)
	}
	variables, err := live.environment("/repository")
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	joined := strings.Join(variables, "\n")
	for _, want := range []string{"NAMESPACE=demo", "OPERATOR_NAMESPACE=ptah-system", "CONTROLLER=ptah-operator",
		"EXECUTOR_IMAGE=registry.example/ptah@sha256:0123", "LAB_ROOT=/repository"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment does not carry %s", want)
		}
	}
	// A step's shell is published, so what it may read is declared rather than
	// whatever the recorder happened to be run with.
	if strings.Contains(joined, "E2E_UNDECLARED") {
		t.Fatal("environment forwarded a variable the format does not declare")
	}
}

// write puts one file on disk, creating the directories above it. Every caller
// wants the parent, and a helper that refuses to make it just moves the mkdir
// into each test.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A failed scenario used to end the whole run, so a set that took most of an
// hour to reach reported one failure and hid the rest. The rows hold the two
// halves of the replacement: every scenario runs and every failure is named,
// and a run with any failure returns nothing to write.
func TestRecordEachRunsEveryScenarioAndWritesNothingOnAFailure(t *testing.T) {
	t.Parallel()
	loaded := []scenario{{ID: "first"}, {ID: "broken"}, {ID: "last"}, {ID: "also-broken"}}
	var ran []string
	run := func(_ context.Context, one scenario) (recording, error) {
		ran = append(ran, one.ID)
		if strings.Contains(one.ID, "broken") {
			return recording{ID: one.ID, Checks: []check{{Scenario: one.ID, Step: 1, Claim: "exits 0"}}},
				fmt.Errorf("%s step 1: exited 1, expected 0", one.ID)
		}
		return recording{ID: one.ID}, nil
	}
	var diagnostics strings.Builder
	recorded, err := recordEach(context.Background(), loaded, run, &diagnostics, func(string, ...any) {})
	if got := strings.Join(ran, ","); got != "first,broken,last,also-broken" {
		t.Fatalf("ran %s; every scenario has to run past a failure", got)
	}
	if err == nil || recorded != nil {
		t.Fatalf("a run with failures returned %d recordings and error %v", len(recorded), err)
	}
	for _, want := range []string{"2 of 4 scenarios failed", "broken step 1", "also-broken step 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not say %q: %v", want, err)
		}
	}
	if !strings.Contains(diagnostics.String(), "FAIL broken step 1") {
		t.Fatalf("the failed scenario's checks were not printed:\n%s", diagnostics.String())
	}

	recorded, err = recordEach(context.Background(), loaded[:1], run, &diagnostics, func(string, ...any) {})
	if err != nil || len(recorded) != 1 {
		t.Fatalf("a clean run returned %d recordings and error %v", len(recorded), err)
	}
}

// The registry credential reaches a step the way it reaches a CI job, as the
// environment ptah reads, and it comes from the lab's own command. A lab that
// printed one of the three names short would leave every push in the run
// without it, so a missing one stops the recording before anything runs.
//
// Not parallel: it executes a file it has just written, and a parallel test
// forking meanwhile can hold that file open and fail the exec with "text file
// busy". Sequential tests run before the parallel ones are released.
func TestRegistryEnvironmentReadsWhatTheLabPrints(t *testing.T) {
	lab := func(t *testing.T, printed string) string {
		t.Helper()
		root := t.TempDir()
		write(t, filepath.Join(root, "demo", "bin", "lab"), "#!/bin/sh\nprintf '%s' '"+printed+"'\n")
		if err := os.Chmod(filepath.Join(root, "demo", "bin", "lab"), 0o755); err != nil {
			t.Fatal(err)
		}
		return root
	}
	root := lab(t, "PTAH_OCI_USERNAME=pusher\nPTAH_OCI_PASSWORD=secret\nPTAH_OCI_REGISTRY=127.0.0.1:5001\n")
	entries, err := registryEnvironment(root, filepath.Join(root, "environment"))
	if err != nil {
		t.Fatalf("registryEnvironment: %v", err)
	}
	if got := strings.Join(entries, ","); got != "PTAH_OCI_REGISTRY=127.0.0.1:5001,PTAH_OCI_USERNAME=pusher,PTAH_OCI_PASSWORD=secret" {
		t.Fatalf("read %s", got)
	}
	root = lab(t, "PTAH_OCI_USERNAME=pusher\nPTAH_OCI_REGISTRY=127.0.0.1:5001\n")
	if _, err := registryEnvironment(root, filepath.Join(root, "environment")); err == nil ||
		!strings.Contains(err.Error(), "PTAH_OCI_PASSWORD") {
		t.Fatalf("a lab that printed no password was accepted: %v", err)
	}
}
