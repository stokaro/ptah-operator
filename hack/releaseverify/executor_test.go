package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The executor recipe is the file the harness builds and the release ships, so
// the rows start from the file itself rather than a fixture that resembles it.
func TestExecutorDockerfileRefusesWhatTheReleaseCannotVerify(t *testing.T) {
	t.Parallel()

	document, err := os.ReadFile(filepath.Join("..", "..", executorDockerfilePath))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := executorDockerfileExternalInputs(document)
	if err != nil {
		t.Fatalf("executorDockerfileExternalInputs(%s) error = %v", executorDockerfilePath, err)
	}
	if len(inputs) != 2 {
		t.Fatalf("the executor recipe reads %d external images, want the builder and the runtime: %#v", len(inputs), inputs)
	}

	replace := func(old, new string) func(string) string {
		return func(recipe string) string { return strings.Replace(recipe, old, new, 1) }
	}
	for _, row := range []struct {
		name    string
		mutate  func(string) string
		problem string
	}{
		{
			name:    "a runtime image named by a tag",
			mutate:  replace("FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b", "FROM alpine:3.24"),
			problem: "is not digest-pinned",
		},
		{
			name:    "a frontend named by a tag",
			mutate:  replace("# The Ptah executor:", "# syntax=docker/dockerfile:1\n# The Ptah executor:"),
			problem: `syntax frontend "docker/dockerfile:1" is not digest-pinned`,
		},
		{
			// The builder runs on the build platform. Without the target, the
			// arm64 image carries an amd64 binary and nothing later notices,
			// because the image config names the platform it was built for.
			name:    "a build that compiles for the build platform",
			mutate:  replace(`CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build`, `CGO_ENABLED=0 go build`),
			problem: `ptah build is missing "GOOS=\"$TARGETOS\" GOARCH=\"$TARGETARCH\" go build"`,
		},
		{
			name:    "a binary that does not carry the commit",
			mutate:  replace("internal/buildinfo.Commit=${PTAH_BUILD_COMMIT}", "internal/buildinfo.Commit=unknown"),
			problem: `ptah build is missing "/internal/buildinfo.Commit=${PTAH_BUILD_COMMIT}"`,
		},
		{
			name:    "a revision label that is not the commit",
			mutate:  replace(`org.opencontainers.image.revision="$PTAH_BUILD_COMMIT"`, `org.opencontainers.image.revision="unknown"`),
			problem: `missing the label org.opencontainers.image.revision="$PTAH_BUILD_COMMIT"`,
		},
		{
			name:    "a source label that names this repository",
			mutate:  replace(`org.opencontainers.image.source="https://github.com/stokaro/ptah"`, `org.opencontainers.image.source="https://github.com/stokaro/ptah-operator"`),
			problem: `missing the label org.opencontainers.image.source="https://github.com/stokaro/ptah"`,
		},
		{
			// A LABEL expands only the arguments its own stage declares, so
			// this one would render the label empty.
			name: "a runtime stage that does not declare the commit",
			mutate: func(recipe string) string {
				runtime := strings.LastIndex(recipe, "\nFROM ")
				return recipe[:runtime] + strings.Replace(recipe[runtime:], "\nARG PTAH_BUILD_COMMIT\n", "\n", 1)
			},
			problem: "runtime stage does not declare ARG PTAH_BUILD_COMMIT",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			mutated := row.mutate(string(document))
			if mutated == string(document) {
				t.Fatal("the mutation changed nothing, so this row measures nothing")
			}
			_, err := executorDockerfileExternalInputs([]byte(mutated))
			if err == nil {
				t.Fatal("executorDockerfileExternalInputs() accepted the recipe")
			}
			if !strings.Contains(err.Error(), row.problem) {
				t.Fatalf("executorDockerfileExternalInputs() said %q, which does not carry %q", err, row.problem)
			}
		})
	}
}

// A frontend the executor recipe names is an input like any image, so a pinned
// one is enumerated for the provenance check rather than passed over.
func TestExecutorDockerfileEnumeratesAPinnedFrontend(t *testing.T) {
	t.Parallel()

	document, err := os.ReadFile(filepath.Join("..", "..", executorDockerfilePath))
	if err != nil {
		t.Fatal(err)
	}
	frontend := "docker/dockerfile:1.27@sha256:" + strings.Repeat("7", 64)
	inputs, err := executorDockerfileExternalInputs(append([]byte("# syntax="+frontend+"\n"), document...))
	if err != nil {
		t.Fatal(err)
	}
	if inputs[0].Kind != "syntax frontend" || inputs[0].Reference != frontend || inputs[0].Line != 1 {
		t.Fatalf("the first input is %#v, want the pinned frontend on line 1", inputs[0])
	}
	// A directive below an ordinary comment is a comment to BuildKit too.
	late := strings.Replace(string(document), "# The build context is not", "# syntax=docker/dockerfile:1\n# The build context is not", 1)
	if late == string(document) {
		t.Fatal("the recipe's header changed, so this row measures nothing")
	}
	if _, err := executorDockerfileExternalInputs([]byte(late)); err != nil {
		t.Fatalf("a syntax line after the header comment is not a directive, got %v", err)
	}
}

// The release builds the executor the acceptance suite built, and the suite
// takes its commit from hack/verifyptahsupport. Both read the catalog; this
// holds them to the same answer, so the rule cannot drift in one of them.
func TestThePtahPinIsTheCommitTheSuiteBuilds(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	pin, err := repositoryPtahPin(root)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "run", "./hack/verifyptahsupport", "-output=commit")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("hack/verifyptahsupport -output=commit: %v", err)
	}
	if suite := strings.TrimSpace(string(output)); suite != pin.Commit {
		t.Fatalf("the release would build Ptah %s and the suite builds %s", pin.Commit, suite)
	}
}

func TestParsePtahPinRefusesWhatCannotBeBuiltOrInstalled(t *testing.T) {
	t.Parallel()

	const commit = "abcdef0123456789abcdef0123456789abcdef01"
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
	build := func(commit, describe string) map[string]any {
		return map[string]any{"ptahRelease": nil, "ptahCommit": commit, "ptahDescribe": describe}
	}

	pin, err := parsePtahPin(catalog(
		map[string]any{"operator": "v0.1.0", "verified": []any{build(strings.Repeat("1", 40), "v0.7.0")}},
		edge(build(commit, "v0.8.1-54-gabcdef012"), build(strings.Repeat("2", 40), "v0.8.0")),
	))
	if err != nil {
		t.Fatal(err)
	}
	if pin != (ptahPin{Commit: commit, Version: "v0.8.1-54-gabcdef012"}) {
		t.Fatalf("parsePtahPin() = %#v, want the first verified build of the edge row", pin)
	}

	for _, row := range []struct {
		name     string
		document []byte
		problem  string
	}{
		{"no edge row", catalog(map[string]any{"operator": "v0.1.0", "verified": []any{build(commit, "v0.8.0")}}), "names the edge row 0 times"},
		{"two edge rows", catalog(edge(build(commit, "v0.8.0")), edge(build(commit, "v0.8.0"))), "names the edge row 2 times"},
		{"an edge row with no build", catalog(edge()), "records no verified Ptah build"},
		{"an abbreviated commit", catalog(edge(build(commit[:12], "v0.8.0"))), "not an exact lowercase commit"},
		{"an uppercase commit", catalog(edge(build(strings.ToUpper(commit), "v0.8.0"))), "not an exact lowercase commit"},
		{"no version", catalog(edge(build(commit, ""))), "which the release cannot stamp or install"},
		{"a version with a space", catalog(edge(build(commit, "v0.8.0 dirty"))), "which the release cannot stamp or install"},
		{"a version over the chart's limit", catalog(edge(build(commit, "v"+strings.Repeat("1", 128)))), "which the release cannot stamp or install"},
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

func TestVerifyExecutorBuildProvenanceBindsThePtahBuild(t *testing.T) {
	t.Parallel()

	pin := ptahPin{Commit: "abcdef0123456789abcdef0123456789abcdef01", Version: "v0.8.1-54-gabcdef012"}
	const date = "2026-09-20T10:11:12+02:00"
	digests := []string{
		"sha256:" + strings.Repeat("1", 64), // builder
		"sha256:" + strings.Repeat("2", 64), // runtime
		"sha256:" + strings.Repeat("4", 64), // SBOM generator
	}
	fixture := func(arguments map[string]string, missing string) []byte {
		dependencies := make([]any, 0, len(digests))
		for _, digest := range digests {
			if digest == missing {
				continue
			}
			dependencies = append(dependencies, map[string]any{
				"uri":    "pkg:docker/example/input@pinned?digest=" + digest,
				"digest": map[string]string{"sha256": strings.TrimPrefix(digest, "sha256:")},
			})
		}
		platform := map[string]any{
			"SLSA": map[string]any{
				"buildDefinition": map[string]any{
					"externalParameters": map[string]any{"request": map[string]any{"args": arguments}},
					"internalParameters": map[string]any{
						"buildConfig": map[string]any{"llbDefinition": []any{map[string]any{"id": "step0"}}},
					},
					"resolvedDependencies": dependencies,
				},
			},
		}
		document, err := json.Marshal(map[string]any{"linux/amd64": platform, "linux/arm64": platform})
		if err != nil {
			t.Fatal(err)
		}
		return document
	}
	arguments := func(commit, version, date string) map[string]string {
		return map[string]string{
			"build-arg:PTAH_BUILD_COMMIT":  commit,
			"build-arg:PTAH_BUILD_VERSION": version,
			"build-arg:PTAH_BUILD_DATE":    date,
		}
	}

	if err := verifyExecutorBuildProvenance(fixture(arguments(pin.Commit, pin.Version, date), ""), pin, date, digests); err != nil {
		t.Fatalf("verifyExecutorBuildProvenance(valid) error = %v", err)
	}
	for _, row := range []struct {
		name     string
		document []byte
		date     string
		problem  string
	}{
		{
			// The harness passes an abbreviation; the release has to pass the
			// commit its label and manifest name.
			name:     "an abbreviated commit",
			document: fixture(arguments(pin.Commit[:12], pin.Version, date), ""),
			date:     date,
			problem:  "argument build-arg:PTAH_BUILD_COMMIT",
		},
		{
			name:     "another Ptah build",
			document: fixture(arguments(strings.Repeat("9", 40), pin.Version, date), ""),
			date:     date,
			problem:  "argument build-arg:PTAH_BUILD_COMMIT",
		},
		{
			name:     "another version",
			document: fixture(arguments(pin.Commit, "e2e", date), ""),
			date:     date,
			problem:  "argument build-arg:PTAH_BUILD_VERSION",
		},
		{
			name:     "another date",
			document: fixture(arguments(pin.Commit, pin.Version, "unknown"), ""),
			date:     date,
			problem:  "argument build-arg:PTAH_BUILD_DATE",
		},
		{
			name:     "the SBOM generator unresolved",
			document: fixture(arguments(pin.Commit, pin.Version, date), digests[2]),
			date:     date,
			problem:  "does not resolve Dockerfile input " + digests[2],
		},
		{
			name:     "a date that is not a timestamp",
			document: fixture(arguments(pin.Commit, pin.Version, "unknown"), ""),
			date:     "unknown",
			problem:  "expectations are invalid",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := verifyExecutorBuildProvenance(row.document, pin, row.date, digests)
			if err == nil {
				t.Fatal("verifyExecutorBuildProvenance() accepted the provenance")
			}
			if !strings.Contains(err.Error(), row.problem) {
				t.Fatalf("verifyExecutorBuildProvenance() said %q, which does not carry %q", err, row.problem)
			}
		})
	}
}

// The label filter is the check that stands between a staged executor and its
// signature, so it runs here against the image records it must accept and the
// ones it must refuse -- read from the step itself, since a copy would prove
// nothing about what ships.
func TestTheExecutorLabelFilterRefusesAnotherBuild(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required to run the release workflow filters")
	}
	filter := executorLabelFilter(t)
	const (
		digest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		commit  = "abcdef0123456789abcdef0123456789abcdef01"
		version = "v0.8.1-54-gabcdef012"
	)
	image := func(labels map[string]string) map[string]any {
		return map[string]any{
			"manifest": map[string]any{"digest": digest},
			"image": map[string]any{
				"os":           "linux",
				"architecture": "arm64",
				"config":       map[string]any{"Labels": labels},
			},
		}
	}
	labels := func(source, revision, version string) map[string]string {
		return map[string]string{
			"org.opencontainers.image.source":   source,
			"org.opencontainers.image.revision": revision,
			"org.opencontainers.image.version":  version,
		}
	}
	for _, row := range []struct {
		name     string
		record   map[string]any
		accepted bool
	}{
		{"the pinned build", image(labels(executorSourceRepository, commit, version)), true},
		{"the harness's abbreviated revision", image(labels(executorSourceRepository, commit[:12], version)), false},
		{"another commit", image(labels(executorSourceRepository, strings.Repeat("9", 40), version)), false},
		{"the operator's source", image(labels("https://github.com/stokaro/ptah-operator", commit, version)), false},
		{"the harness's default version", image(labels(executorSourceRepository, commit, "e2e")), false},
		{"no labels", image(nil), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			record, err := json.Marshal(row.record)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("jq", "-e",
				"--arg", "digest", digest,
				"--arg", "os", "linux",
				"--arg", "architecture", "arm64",
				"--arg", "source", executorSourceRepository,
				"--arg", "revision", commit,
				"--arg", "version", version,
				filter)
			command.Stdin = strings.NewReader(string(record))
			output, runErr := command.CombinedOutput()
			switch {
			case row.accepted && runErr != nil:
				t.Fatalf("the filter refused a record it must accept: %v\n%s", runErr, output)
			case !row.accepted && runErr == nil:
				t.Fatalf("the filter accepted a record it must refuse:\n%s", output)
			}
		})
	}
}

func executorLabelFilter(t *testing.T) string {
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
	structure, err := requireStep(steps, "executor-structure")
	if err != nil {
		t.Fatal(err)
	}
	programs, err := stepJQPrograms(structure)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, program := range programs {
		if strings.Contains(program, `.image.config.Labels["org.opencontainers.image.revision"] == $revision`) {
			found = append(found, program)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the executor structure step has %d revision label filters, want 1", len(found))
	}
	return found[0]
}
