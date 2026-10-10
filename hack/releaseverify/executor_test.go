package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release names the executor the acceptance suite ran, and the suite takes
// its image from hack/verifyptahsupport. Both read the catalog; this holds them
// to the same answer, so the rule cannot drift in one of them.
func TestThePtahPinIsTheImageTheSuiteRuns(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	pin, err := repositoryPtahPin(root)
	if err != nil {
		t.Fatal(err)
	}
	for output, want := range map[string]string{"image": pin.Image, "commit": pin.Commit, "version": pin.Version} {
		command := exec.Command("go", "run", "./hack/verifyptahsupport", "-output="+output)
		command.Dir = root
		printed, err := command.Output()
		if err != nil {
			t.Fatalf("hack/verifyptahsupport -output=%s: %v", output, err)
		}
		if suite := strings.TrimSpace(string(printed)); suite != want {
			t.Fatalf("the release would name Ptah %s %s and the suite runs %s", output, want, suite)
		}
	}
}

func TestParsePtahPinRefusesWhatCannotBeNamedOrInstalled(t *testing.T) {
	t.Parallel()

	const (
		commit = "abcdef0123456789abcdef0123456789abcdef01"
		image  = "ghcr.io/stokaro/ptah@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)
	catalog := func(releases ...map[string]any) []byte {
		document, err := json.Marshal(map[string]any{"schemaVersion": 1, "releases": releases})
		if err != nil {
			t.Fatal(err)
		}
		return document
	}
	edge := func(verified ...map[string]any) map[string]any {
		return map[string]any{"operator": "edge", "verified": verified}
	}
	build := func(image, commit, describe string) map[string]any {
		return map[string]any{"ptahRelease": nil, "ptahCommit": commit, "ptahDescribe": describe, "ptahImage": image}
	}

	pin, err := parsePtahPin(catalog(
		map[string]any{"operator": "v0.1.0", "verified": []any{build(image, strings.Repeat("1", 40), "v0.7.0")}},
		edge(build(image, commit, "v0.13.0"), build(image, strings.Repeat("2", 40), "v0.12.0")),
	))
	if err != nil {
		t.Fatal(err)
	}
	if pin != (ptahPin{Image: image, Commit: commit, Version: "v0.13.0"}) {
		t.Fatalf("parsePtahPin() = %#v, want the first verified build of the edge row", pin)
	}

	for _, row := range []struct {
		name     string
		document []byte
		problem  string
	}{
		{"no edge row", catalog(map[string]any{"operator": "v0.1.0", "verified": []any{build(image, commit, "v0.8.0")}}), "names the edge row 0 times"},
		{"two edge rows", catalog(edge(build(image, commit, "v0.8.0")), edge(build(image, commit, "v0.8.0"))), "names the edge row 2 times"},
		{"an edge row with no build", catalog(edge()), "records no verified Ptah build"},
		{"no image", catalog(edge(build("", commit, "v0.8.0"))), "not ghcr.io/stokaro/ptah pinned by digest"},
		{"an image by tag", catalog(edge(build("ghcr.io/stokaro/ptah:0.13.0", commit, "v0.8.0"))), "not ghcr.io/stokaro/ptah pinned by digest"},
		{"another repository's image", catalog(edge(build("ghcr.io/stokaro/ptah-operator-executor@sha256:"+strings.Repeat("1", 64), commit, "v0.8.0"))), "not ghcr.io/stokaro/ptah pinned by digest"},
		{"an abbreviated commit", catalog(edge(build(image, commit[:12], "v0.8.0"))), "not an exact lowercase commit"},
		{"an uppercase commit", catalog(edge(build(image, strings.ToUpper(commit), "v0.8.0"))), "not an exact lowercase commit"},
		{"no version", catalog(edge(build(image, commit, ""))), "which the release cannot stamp or install"},
		{"a version with a space", catalog(edge(build(image, commit, "v0.8.0 dirty"))), "which the release cannot stamp or install"},
		{"a version over the chart's limit", catalog(edge(build(image, commit, "v"+strings.Repeat("1", 128)))), "which the release cannot stamp or install"},
		{"not JSON", []byte("releases: []"), "parse support/ptah.json"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePtahPin(row.document)
			if err == nil {
				t.Fatal("parsePtahPin() accepted the catalog")
			}
			if !strings.Contains(err.Error(), row.problem) {
				t.Fatalf("parsePtahPin() said %q, which does not carry %q", err, row.problem)
			}
		})
	}
}
