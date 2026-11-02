package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInputPathsNamesTheFilesACommandReads(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		command string
		want    []string
	}{
		{name: "a manifest applied", command: `kubectl -n "$NAMESPACE" apply -f demo/manifests/manual-approval/storefront.yaml`,
			want: []string{"demo/manifests/manual-approval/storefront.yaml"}},
		{name: "a file under a key", command: `kubectl create configmap storefront-v1 --from-file=schema.sql=demo/schemas/v1.sql`,
			want: []string{"demo/schemas/v1.sql"}},
		{name: "a directory", command: "ls demo/migrations", want: []string{"demo/migrations"}},
		{name: "a directory with a trailing slash", command: "ls demo/migrations/", want: []string{"demo/migrations"}},
		{name: "a sibling with a longer name", command: "ls demo/migrations-checkpoint", want: []string{"demo/migrations-checkpoint"}},
		{name: "two files compared", command: "diff -u demo/schemas/v1.sql demo/schemas/v2.sql",
			want: []string{"demo/schemas/v1.sql", "demo/schemas/v2.sql"}},
		{name: "a name the shell completes", command: `kubectl create configmap x --from-file="demo/mysql/release-$release"`,
			want: []string{"demo/mysql"}},
		{name: "the lab's own command", command: "demo/bin/lab reset", want: []string{}},
		{name: "a run's scratch", command: "cat demo/.lab/digest", want: []string{}},
		{name: "a coordination key", command: "echo coordinationKey: demo/storefront/primary", want: []string{}},
		{name: "a repository inside a reference", command: `ptah schema push "oci://$PTAH_OCI_REGISTRY/schemas/demo:v1-$$"`,
			want: []string{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := inputPaths(scenario{ID: "probe", Steps: []step{{Run: row.command}}})
			if !slices.Equal(got, row.want) {
				t.Fatalf("inputPaths read %v from %q, want %v", got, row.command, row.want)
			}
		})
	}
	reset := inputPaths(scenario{ID: "probe", Reset: []string{"cat demo/schemas/v1.sql"}})
	if !slices.Equal(reset, []string{"demo/schemas/v1.sql"}) {
		t.Fatalf("inputPaths read %v from the reset, want the file it prints", reset)
	}
}

func TestInputDigestFollowsTheFilesItNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeInput(t, root, "demo/schemas/v1.sql", "CREATE TABLE a (id int);\n")
	writeInput(t, root, "demo/schemas/v2.sql", "CREATE TABLE b (id int);\n")
	writeInput(t, root, "demo/mysql/release-1/0001.up.sql", "CREATE TABLE c (id int);\n")
	probe := scenario{ID: "probe", Steps: []step{{Run: "cat demo/schemas/v1.sql"}, {Run: `ls "demo/mysql/release-$n"`}}}

	before := readInputs(t, root, probe)
	// Same length every time: the digest has to read the bytes, not only
	// the size.
	writeInput(t, root, "demo/schemas/v2.sql", "CREATE TABLE b (id INT);\n")
	if readInputs(t, root, probe) != before {
		t.Fatal("a file no command names moved the digest")
	}
	writeInput(t, root, "demo/schemas/v1.sql", "CREATE TABLE a (id INT);\n")
	changed := readInputs(t, root, probe)
	if changed == before {
		t.Fatal("a file a command prints changed and the digest did not")
	}
	writeInput(t, root, "demo/mysql/release-1/0002.up.sql", "ALTER TABLE c ADD d int;\n")
	if readInputs(t, root, probe) == changed {
		t.Fatal("a file added under a directory the shell reads did not move the digest")
	}
	if _, err := inputDigest(root, scenario{ID: "probe", Steps: []step{{Run: "cat demo/schemas/v3.sql"}}}); err == nil ||
		!strings.Contains(err.Error(), "demo/schemas/v3.sql") {
		t.Fatalf("a file that does not exist was digested: %v", err)
	}
}

// Every scenario's files exist, and every manifest under demo/manifests is one
// a scenario reads: the digest binds a recording to all of them, and a
// manifest no scenario reads is one no recording proves.
func TestEveryScenarioReadsFilesThatExist(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	loaded, err := loadScenarios(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inputDigests(root, loaded); err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	for _, one := range loaded {
		for _, mentioned := range inputPaths(one) {
			read[mentioned] = true
		}
	}
	manifests := 0
	err = filepath.WalkDir(filepath.Join(root, "demo", "manifests"), func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		manifests++
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if !read[filepath.ToSlash(relative)] {
			t.Errorf("no scenario reads %s", filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifests == 0 {
		t.Fatal("demo/manifests holds nothing, so this test held nothing")
	}
}

func TestVerifyRefusesARecordingWhoseFileChanged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scenarios := filepath.Join(root, "demo", "scenarios")
	writeInput(t, root, "demo/manifests/probe/storefront.yaml", "kind: PtahSchema\n")
	writeInput(t, root, "demo/scenarios/input-probe.yaml", `id: input-probe
title: A probe
tagline: One step.
learn: What -verify reads besides the scenario.
start: An empty namespace.
tags: [Lifecycle]
steps:
  - note: Apply it.
    run: kubectl apply -f demo/manifests/probe/storefront.yaml
    expect:
      exit: 0
`)
	recorded, err := loadScenarios(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	recordFile := filepath.Join(root, "runs.json")
	if err := writeRecord(recordFile, runRecord{Scenarios: []recording{{
		ID: "input-probe", DefinitionDigest: definitionDigest(recorded[0]), InputDigest: readInputs(t, root, recorded[0]),
	}}}); err != nil {
		t.Fatal(err)
	}
	verify := func() (string, error) {
		var diagnostics strings.Builder
		err := run([]string{"-root", root, "-scenarios", scenarios, "-verify", recordFile}, &diagnostics)
		return diagnostics.String(), err
	}
	if output, err := verify(); err != nil {
		t.Fatalf("an unchanged recording was refused: %v\n%s", err, output)
	}
	// The same length, so only the bytes differ.
	writeInput(t, root, "demo/manifests/probe/storefront.yaml", "kind: Deployment\n")
	if output, err := verify(); err == nil || !strings.Contains(output, "input-probe: a file the scenario reads changed") {
		t.Fatalf("a recording of another manifest was accepted: %v\n%s", err, output)
	}
}

func readInputs(t *testing.T, root string, s scenario) string {
	t.Helper()
	digest, err := inputDigest(root, s)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func writeInput(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
