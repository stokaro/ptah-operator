package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestYAMLScalars(t *testing.T) {
	t.Parallel()
	document := []byte("version: 0.2.0-rc.1\nimage:\n  repository: example.invalid/operator\n  tag: \"0.2.0-rc.1\"\nnext:\n  tag: ignored\n")

	version, err := topLevelScalar(document, "version")
	if err != nil || version != "0.2.0-rc.1" {
		t.Fatalf("topLevelScalar() = %q, %v", version, err)
	}
	tag, err := nestedScalar(document, "image", "tag")
	if err != nil || tag != "0.2.0-rc.1" {
		t.Fatalf("nestedScalar() = %q, %v", tag, err)
	}
}

func TestCurrentReleaseSequenceContractMatchesHistory(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	read := func(path string) []byte {
		t.Helper()
		document, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		return document
	}
	chart := read("charts/ptah-operator/Chart.yaml")
	values := read("charts/ptah-operator/values.yaml")
	contract, err := currentReleaseContract(
		mustTopLevelScalar(t, chart, "version"),
		mustTopLevelScalar(t, chart, "appVersion"),
		values,
		read(releaseSequenceHelperPath),
		read(releaseSequenceGoPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	history, err := decodeReleaseSequenceHistory("candidate", read(releaseSequenceHistoryPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCandidateReleaseContract(history, contract); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSequenceParsersRequireExactParity(t *testing.T) {
	t.Parallel()

	helper := []byte("{{- define \"ptah-operator.releaseSequence\" -}}12{{- end -}}\n")
	goSource := []byte("package crdupgrade\nconst CurrentReleaseSequence int32 = 12\n")
	if sequence, err := helmReleaseSequence(helper); err != nil || sequence != 12 {
		t.Fatalf("helmReleaseSequence() = %d, %v", sequence, err)
	}
	if sequence, err := goReleaseSequence(goSource); err != nil || sequence != 12 {
		t.Fatalf("goReleaseSequence() = %d, %v", sequence, err)
	}
	if _, err := currentReleaseContract(
		"1.2.0", "1.2.0",
		[]byte("image:\n  repository: example.invalid/operator\n  tag: 1.2.0\n"),
		helper,
		[]byte("package crdupgrade\nconst CurrentReleaseSequence int32 = 11\n"),
	); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("currentReleaseContract() error = %v, want parity rejection", err)
	}

	for name, document := range map[string][]byte{
		"Helm expression":  []byte("{{- define \"ptah-operator.releaseSequence\" -}}{{ add 1 1 }}{{- end -}}\n"),
		"Go inferred type": []byte("package crdupgrade\nconst CurrentReleaseSequence = 12\n"),
		"Go expression":    []byte("package crdupgrade\nconst CurrentReleaseSequence int32 = 6 + 6\n"),
		"Go zero":          []byte("package crdupgrade\nconst CurrentReleaseSequence int32 = 0\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var err error
			if strings.HasPrefix(name, "Helm") {
				_, err = helmReleaseSequence(document)
			} else {
				_, err = goReleaseSequence(document)
			}
			if err == nil {
				t.Fatal("release sequence parser accepted a non-canonical contract")
			}
		})
	}
}

func TestReleaseSequenceHistoryTransitions(t *testing.T) {
	t.Parallel()

	baselineRelease := releaseContract{
		Version:                "0.1.0",
		AppVersion:             "0.1.0",
		ManagerImageRepository: "example.invalid/operator",
		ManagerImageTag:        "0.1.0",
		ReleaseSequence:        4,
	}
	nextRelease := releaseContract{
		Version:                "0.2.0",
		AppVersion:             "0.2.0",
		ManagerImageRepository: "example.invalid/operator-v2",
		ManagerImageTag:        "0.2.0",
		ReleaseSequence:        5,
	}
	baseline := releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{baselineRelease}}

	for name, test := range map[string]struct {
		candidate releaseSequenceHistory
		wantError string
	}{
		"unchanged metadata keeps sequence": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{baselineRelease}},
		},
		"new version and image contract increases sequence": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{baselineRelease, nextRelease}},
		},
		"same sequence for new contract": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{
				baselineRelease,
				withReleaseSequence(nextRelease, baselineRelease.ReleaseSequence),
			}},
			wantError: "must strictly increase",
		},
		"decreased sequence for new contract": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{
				baselineRelease,
				withReleaseSequence(nextRelease, baselineRelease.ReleaseSequence-1),
			}},
			wantError: "must strictly increase",
		},
		"published entry rewrite": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{
				withReleaseSequence(baselineRelease, baselineRelease.ReleaseSequence+1),
			}},
			wantError: "rewrote published entry",
		},
		"published entry removal": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: nil},
			wantError: "removed published entries",
		},
		"multiple releases in one transition": {
			candidate: releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{
				baselineRelease,
				nextRelease,
				{
					Version:                "0.3.0",
					AppVersion:             "0.3.0",
					ManagerImageRepository: "example.invalid/operator-v3",
					ManagerImageTag:        "0.3.0",
					ReleaseSequence:        6,
				},
			}},
			wantError: "exactly one release",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyReleaseSequenceTransition(baseline, test.candidate)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyReleaseSequenceTransition() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestReleaseSequenceHistoryRejectsInvalidOrNonCanonicalRecords(t *testing.T) {
	t.Parallel()

	release := releaseContract{
		Version:                "1.0.0",
		AppVersion:             "1.0.0",
		ManagerImageRepository: "example.invalid/operator",
		ManagerImageTag:        "1.0.0",
		ReleaseSequence:        1,
	}
	valid := releaseSequenceHistory{FormatVersion: 1, Releases: []releaseContract{release}}
	validDocument, err := canonicalReleaseSequenceHistory(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeReleaseSequenceHistory("candidate", validDocument); err != nil {
		t.Fatal(err)
	}

	wrongTag := valid
	wrongTag.Releases = append([]releaseContract(nil), valid.Releases...)
	wrongTag.Releases[0].ManagerImageTag = "latest"
	wrongTagDocument, err := canonicalReleaseSequenceHistory(wrongTag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeReleaseSequenceHistory("candidate", wrongTagDocument); err == nil || !strings.Contains(err.Error(), "manager image tag") {
		t.Fatalf("decodeReleaseSequenceHistory() error = %v, want image-tag rejection", err)
	}

	nonCanonical := append([]byte(" \n"), validDocument...)
	if _, err := decodeReleaseSequenceHistory("candidate", nonCanonical); err == nil || !strings.Contains(err.Error(), "canonical JSON") {
		t.Fatalf("decodeReleaseSequenceHistory() error = %v, want canonical rejection", err)
	}

	unknownField := bytes.Replace(validDocument, []byte("\"formatVersion\": 1"), []byte("\"formatVersion\": 1,\n  \"unexpected\": true"), 1)
	if _, err := decodeReleaseSequenceHistory("candidate", unknownField); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("decodeReleaseSequenceHistory() error = %v, want unknown-field rejection", err)
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"0.1.0-rc.1", "0.1.0"},
		{"0.1.0-rc.1", "0.1.0-rc.2"},
		{"0.1.9", "0.2.0"},
		{"1.9.9", "2.0.0"},
	} {
		order, err := compareReleaseVersions(pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if order >= 0 {
			t.Fatalf("compareReleaseVersions(%q, %q) = %d, want less than zero", pair[0], pair[1], order)
		}
	}
}

func mustTopLevelScalar(t *testing.T, document []byte, key string) string {
	t.Helper()
	value, err := topLevelScalar(document, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func withReleaseSequence(release releaseContract, sequence uint64) releaseContract {
	release.ReleaseSequence = sequence
	return release
}

func TestGoToolchain(t *testing.T) {
	t.Parallel()

	version, err := goToolchain([]byte("module example.invalid/operator\n\ngo 1.26.0\ntoolchain go1.27.0\n"))
	if err != nil || version != "1.27.0" {
		t.Fatalf("goToolchain() = %q, %v", version, err)
	}
}

func TestVerifyGitHubTagIdentityPeelsAnnotatedTag(t *testing.T) {
	t.Parallel()

	const (
		tagObject = "2222222222222222222222222222222222222222"
		commit    = "1111111111111111111111111111111111111111"
	)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Body:       io.NopCloser(strings.NewReader("unauthorized")),
			}, nil
		}
		var body string
		switch request.URL.Path {
		case "/repos/stokaro/ptah-operator/git/ref/tags/v0.1.0":
			body = fmt.Sprintf(`{"object":{"type":"tag","sha":%q}}`, tagObject)
		case "/repos/stokaro/ptah-operator/git/tags/" + tagObject:
			body = fmt.Sprintf(`{"object":{"type":"commit","sha":%q}}`, commit)
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	if err := verifyGitHubTagIdentity(
		t.Context(), client, "https://api.github.test", repositoryName, "v0.1.0", commit, "test-token",
	); err != nil {
		t.Fatal(err)
	}
	if err := verifyGitHubTagIdentity(
		t.Context(), client, "https://api.github.test", repositoryName, "v0.1.0",
		"3333333333333333333333333333333333333333", "test-token",
	); err == nil {
		t.Fatal("verifyGitHubTagIdentity() accepted a moved tag")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSemanticVersionContract(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{"0.1.0", "1.2.3", "2.0.0-rc.1"} {
		if !semanticVersionPattern.MatchString(valid) {
			t.Errorf("semantic version pattern rejected %q", valid)
		}
	}
	for _, invalid := range []string{"v1.2.3", "01.2.3", "1.2", "1.2.3+build"} {
		if semanticVersionPattern.MatchString(invalid) {
			t.Errorf("semantic version pattern accepted %q", invalid)
		}
	}
}

func TestVerifyDockerfileRequiresPinnedFrontendAndBases(t *testing.T) {
	t.Parallel()

	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	valid := []byte("# syntax=docker/dockerfile:1.7@" + digest + "\n" +
		"FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine@" + digest + " AS builder\n" +
		"ARG REVISION\n" +
		"RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=\"-s -w -X main.controllerRevision=${REVISION}\" -o /out/manager ./cmd/manager\n" +
		"FROM gcr.io/distroless/static-debian13:nonroot@" + digest + "\n" +
		"LABEL org.opencontainers.image.source=x org.opencontainers.image.revision=y org.opencontainers.image.version=z\n")
	if err := verifyDockerfile(valid, "1.27.0"); err != nil {
		t.Fatalf("verifyDockerfile() error = %v", err)
	}
	mutableFrontend := []byte("# syntax=docker/dockerfile:1.7\n" + string(valid[strings.Index(string(valid), "FROM"):]))
	if err := verifyDockerfile(mutableFrontend, "1.27.0"); err == nil {
		t.Fatal("verifyDockerfile() accepted a mutable syntax frontend")
	}
	for name, mutation := range map[string]struct {
		old string
		new string
	}{
		"missing builder revision argument": {
			old: "ARG REVISION\n",
			new: "",
		},
		"revision used only as an OCI label": {
			old: `-ldflags="-s -w -X main.controllerRevision=${REVISION}"`,
			new: `-ldflags="-s -w"`,
		},
		"constant controller revision": {
			old: `main.controllerRevision=${REVISION}`,
			new: `main.controllerRevision=unknown`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := bytes.Replace(valid, []byte(mutation.old), []byte(mutation.new), 1)
			if bytes.Equal(mutated, valid) {
				t.Fatalf("fixture does not contain %q", mutation.old)
			}
			if err := verifyDockerfile(mutated, "1.27.0"); err == nil {
				t.Fatal("verifyDockerfile() accepted a manager build without exact revision binding")
			}
		})
	}
}

func TestDockerfileInputEnumeratorRejectsParserBlindSpots(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("1", 64)
	base := "# syntax=docker/dockerfile:1.7@" + digest + "\n" +
		"FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine@" + digest + " AS builder\n"
	runtime := "FROM gcr.io/distroless/static-debian13:nonroot@" + digest + "\n" +
		"LABEL org.opencontainers.image.source=x org.opencontainers.image.revision=y org.opencontainers.image.version=z\n"

	tests := map[string]string{
		"lowercase FROM": base + "from alpine:latest AS hidden\n" + runtime,
		"continued FROM": base + "FrOm \\\n  alpine:latest AS hidden\n" + runtime,
		"mutable ARG in FROM": "# syntax=docker/dockerfile:1.7@" + digest + "\n" +
			"ARG HIDDEN=alpine:latest\n" + base[strings.Index(base, "FROM"):] +
			"from ${HIDDEN} AS hidden\n" + runtime,
		"external COPY":      base + runtime + "cOpY --from=alpine:latest /bin/tool /bin/tool\n",
		"external RUN mount": base + "RUN --mount=type=bind,from=alpine:latest,target=/mnt true\n" + runtime,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := dockerfileExternalInputs([]byte(document), "1.27.0"); err == nil {
				t.Fatal("dockerfileExternalInputs() accepted a mutable external image input")
			}
		})
	}
}

func TestDockerfileInputEnumeratorResolvesArgumentsAndInternalStages(t *testing.T) {
	t.Parallel()

	frontendDigest := "sha256:" + strings.Repeat("5", 64)
	builderDigest := "sha256:" + strings.Repeat("1", 64)
	runtimeDigest := "sha256:" + strings.Repeat("2", 64)
	copyDigest := "sha256:" + strings.Repeat("3", 64)
	mountDigest := "sha256:" + strings.Repeat("4", 64)
	document := []byte("# syntax=docker/dockerfile:1.7@" + frontendDigest + "\n" +
		"ARG BUILDER=golang:1.27.0-alpine@" + builderDigest + "\n" +
		"ARG RUNTIME=gcr.io/distroless/static-debian13:nonroot@" + runtimeDigest + "\n" +
		"from --platform=$BUILDPLATFORM ${BUILDER} as builder\n" +
		"COPY --from=builder /src /src\n" +
		"FROM ${RUNTIME}\n" +
		"COPY --from=0 /out/manager /manager\n" +
		"COPY --from=example.invalid/tool@" + copyDigest + " /tool /tool\n" +
		"RUN --mount=type=bind,from=example.invalid/data@" + mountDigest + ",target=/mnt true\n" +
		"LABEL org.opencontainers.image.source=x org.opencontainers.image.revision=y org.opencontainers.image.version=z\n")

	inputs, err := dockerfileExternalInputs(document, "1.27.0")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(inputs))
	for _, input := range inputs {
		got = append(got, input.Kind+"="+input.Reference)
	}
	want := []string{
		"syntax frontend=docker/dockerfile:1.7@" + frontendDigest,
		"FROM=golang:1.27.0-alpine@" + builderDigest,
		"FROM=gcr.io/distroless/static-debian13:nonroot@" + runtimeDigest,
		"COPY --from=example.invalid/tool@" + copyDigest,
		"RUN --mount from=example.invalid/data@" + mountDigest,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("external inputs = %#v, want %#v", got, want)
	}
}

func TestDockerfileInputEnumeratorRejectsUnresolvedArgumentsAndHeredocs(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("1", 64)
	labels := "LABEL org.opencontainers.image.source=x org.opencontainers.image.revision=y org.opencontainers.image.version=z\n"
	for name, document := range map[string]string{
		"unresolved argument": "# syntax=docker/dockerfile:1.7@" + digest + "\n" +
			"ARG BUILDER\nFROM ${BUILDER} AS builder\n" +
			"FROM gcr.io/distroless/static-debian13:nonroot@" + digest + "\n" + labels,
		"heredoc": "# syntax=docker/dockerfile:1.7@" + digest + "\n" +
			"FROM golang:1.27.0-alpine@" + digest + " AS builder\n" +
			"RUN <<EOF\nFROM alpine:latest\nEOF\n" +
			"FROM gcr.io/distroless/static-debian13:nonroot@" + digest + "\n" + labels,
		"alternate escape directive": "# syntax=docker/dockerfile:1.7@" + digest + "\n" +
			"#escape=`\n" +
			"FROM golang:1.27.0-alpine@" + digest + " AS builder\n" +
			"FROM gcr.io/distroless/static-debian13:nonroot@" + digest + "\n" + labels,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := dockerfileExternalInputs([]byte(document), "1.27.0"); err == nil {
				t.Fatal("dockerfileExternalInputs() accepted unsupported Dockerfile syntax")
			}
		})
	}
}

func TestVerifyRegistryMissingErrorRequiresExactReferenceBoundResponse(t *testing.T) {
	t.Parallel()

	reference := "ghcr.io/stokaro/ptah-operator:tx-" + strings.Repeat("1", 40) + "-123"
	for name, message := range map[string]string{
		"Buildx GHCR response": "ERROR: " + reference + ": not found\n",
		"manifest code":        "ERROR: " + reference + ": MANIFEST_UNKNOWN: manifest unknown\n",
		"name code":            "ERROR: " + reference + ": name unknown\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "error.txt")
			if err := os.WriteFile(path, []byte(message), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyRegistryMissingError(path, reference); err != nil {
				t.Fatalf("verifyRegistryMissingError() error = %v", err)
			}
		})
	}

	for name, message := range map[string]string{
		"generic local error":     "ERROR: credential helper executable not found\n",
		"wrong reference":         "ERROR: ghcr.io/example/other:tag: not found\n",
		"TLS trust store":         "ERROR: " + reference + ": trust store not found\n",
		"registry outage":         "ERROR: " + reference + ": unexpected status from HEAD request: 503 Service Unavailable\n",
		"missing plus outage":     "ERROR: " + reference + ": not found\nconnection reset by peer\n",
		"imprecise missing token": "ERROR: " + reference + ": manifest unknownish\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "error.txt")
			if err := os.WriteFile(path, []byte(message), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyRegistryMissingError(path, reference); err == nil {
				t.Fatal("verifyRegistryMissingError() accepted an ambiguous registry failure")
			}
		})
	}
}

func TestVerifyBuildProvenanceUsesExactResolvedDependencies(t *testing.T) {
	t.Parallel()

	const (
		source   = "https://github.com/stokaro/ptah-operator"
		revision = "1111111111111111111111111111111111111111"
		version  = "0.1.0"
	)
	digests := []string{
		"sha256:" + strings.Repeat("1", 64), // builder
		"sha256:" + strings.Repeat("2", 64), // runtime
		"sha256:" + strings.Repeat("3", 64), // Dockerfile syntax frontend
		"sha256:" + strings.Repeat("4", 64), // SBOM generator
	}
	fixture := func(missingMaterial string) []byte {
		dependencies := make([]any, 0, len(digests))
		for _, digest := range digests {
			if digest == missingMaterial {
				continue
			}
			dependencies = append(dependencies, map[string]any{
				"uri": "pkg:docker/example/input@pinned?digest=" + digest,
				"digest": map[string]string{
					"sha256": strings.TrimPrefix(digest, "sha256:"),
				},
			})
		}
		platform := func() map[string]any {
			return map[string]any{
				"SLSA": map[string]any{
					"buildDefinition": map[string]any{
						"externalParameters": map[string]any{
							"request": map[string]any{
								"args": map[string]string{
									"build-arg:SOURCE":   source,
									"build-arg:REVISION": revision,
									"build-arg:VERSION":  version,
								},
							},
						},
						"internalParameters": map[string]any{
							"buildConfig": map[string]any{
								"llbDefinition": []any{map[string]any{"id": "step0"}},
							},
						},
						"resolvedDependencies": dependencies,
						"irrelevant":           "expected digests are only text here: " + strings.Join(digests, ","),
					},
				},
			}
		}
		document, err := json.Marshal(map[string]any{
			"linux/amd64": platform(),
			"linux/arm64": platform(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return document
	}

	if err := verifyBuildProvenance(fixture(""), source, revision, version, digests); err != nil {
		t.Fatalf("verifyBuildProvenance(valid) error = %v", err)
	}
	for name, missing := range map[string]string{
		"Dockerfile frontend": digests[2],
		"SBOM generator":      digests[3],
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyBuildProvenance(fixture(missing), source, revision, version, digests); err == nil {
				t.Fatal("verifyBuildProvenance() accepted a digest only present in an irrelevant string")
			}
		})
	}
}

func TestReleaseWorkflowJQProgramsCompile(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required to compile the release workflow filters")
	}
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	programs, err := workflowJQPrograms(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if len(programs) == 0 {
		t.Fatal("release workflow contains no jq programs")
	}
	for index, program := range programs {
		arguments := []string{"-n"}
		for _, variable := range []string{"digest", "os", "architecture", "source", "revision", "version", "name", "branch", "sha"} {
			arguments = append(arguments, "--arg", variable, "")
		}
		arguments = append(arguments, "def __release_filter: ("+program+"); null")
		if output, err := exec.Command("jq", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("jq program %d does not compile: %v\n%s\n%s", index+1, err, output, program)
		}
	}
}

func workflowJQPrograms(document []byte) ([]string, error) {
	var workflow workflowDocument
	if err := yaml.Unmarshal(document, &workflow); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	var programs []string
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			for offset := 0; ; {
				index := strings.Index(step.Run[offset:], "jq ")
				if index < 0 {
					break
				}
				index += offset
				start := strings.IndexByte(step.Run[index:], '\'')
				if start < 0 {
					return nil, fmt.Errorf("release step %s has a jq command without a single-quoted program", step.ID)
				}
				start += index + 1
				end := strings.IndexByte(step.Run[start:], '\'')
				if end < 0 {
					return nil, fmt.Errorf("release step %s has an unterminated jq program", step.ID)
				}
				end += start
				programs = append(programs, step.Run[start:end])
				offset = end + 1
			}
		}
	}
	return programs, nil
}

func TestVerifyWorkflowRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyWorkflow(workflow); err != nil {
		t.Fatalf("verifyWorkflow(valid) error = %v", err)
	}

	tests := map[string]struct {
		old string
		new string
		all bool
	}{
		"manual smoke trigger":      {`  workflow_dispatch:`, `  # workflow_dispatch removed`, false},
		"manual smoke guard":        {`github.event_name == 'pull_request' || github.event_name == 'workflow_dispatch'`, `github.event_name == 'pull_request'`, false},
		"tag trigger":               {`      - "v*"`, `      - main`, false},
		"tag job guard":             {`startsWith(github.ref, 'refs/tags/v')`, `startsWith(github.ref, 'refs/heads/')`, false},
		"preflight permission":      {`      actions: read`, `      actions: write`, false},
		"preflight timeout":         {`    timeout-minutes: 130`, `    timeout-minutes: 1`, false},
		"support poll timeout":      {`SUPPORT_POLL_TIMEOUT_MINUTES: "120"`, `SUPPORT_POLL_TIMEOUT_MINUTES: "100"`, false},
		"fresh support policy":      {`go run ./hack/verify-kubernetes-support.go -now "$today"`, `true # support freshness omitted`, false},
		"preflight policy verifier": {`go run ./hack/releaseverify`, `true # release policy omitted`, false},
		"default branch binding":    {`DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}`, `DEFAULT_BRANCH: master`, false},
		"exact CI workflow":         {`actions/workflows/ci.yml/runs`, `actions/runs`, false},
		"exact CI head SHA":         {`-f head_sha="$GITHUB_SHA"`, `-f head_sha=unknown`, false},
		"CI push event":             {`.event == "push"`, `.event == "pull_request"`, false},
		"CI default branch":         {`.head_branch == $branch`, `.head_branch != $branch`, false},
		"stable support gate":       {`.name == "Kubernetes support gate"`, `.name == "Verify source and generated files"`, false},
		"bounded evidence polling":  {`poll_deadline_epoch=$(( $(date -u +%s) + SUPPORT_POLL_TIMEOUT_MINUTES * 60 ))`, `poll_deadline_epoch=0`, false},
		"preflight output":          {`source-sha: ${{ steps.support-evidence.outputs.source-sha }}`, `source-sha: ${{ github.sha }}`, false},
		"tested chart output":       {`chart-sha256: ${{ steps.support-evidence.outputs.chart-sha256 }}`, `chart-sha256: unverified`, false},
		"support window output":     {`kubernetes-support-window: ${{ steps.support-evidence.outputs.kubernetes-support-window }}`, `kubernetes-support-window: unverified`, false},
		"support run output":        {`support-evidence-run-id: ${{ steps.support-evidence.outputs.support-evidence-run-id }}`, `support-evidence-run-id: 1`, false},
		"support matrix derivation": {`support_matrix="$(go run ./hack/verify-kubernetes-support.go -output=matrix)"`, `support_matrix='[{"minor":"1.99","minor_slug":"1-99"}]'`, false},
		"minor slug binding":        {`(.minor_slug == (.minor | gsub("\\."; "-")))`, `(.minor_slug | length > 0)`, false},
		"artifact inventory":        {`repos/$GITHUB_REPOSITORY/actions/runs/$evidence_run/artifacts`, `repos/$GITHUB_REPOSITORY/actions/runs/1/artifacts`, false},
		"artifact count":            {`.total_count == $expected_count`, `.total_count >= 0`, false},
		"artifact freshness":        {`.expired == false`, `true`, false},
		"installed chart download":  {`gh run download "$evidence_run"`, `true # chart download omitted`, false},
		"installed chart equality":  {`cmp "$canonical_chart" "$chart_path"`, `true # byte equality omitted`, false},
		"support run validation":    {`[[ "$evidence_run" =~ ^[1-9][0-9]*$ ]]`, `test -n "$evidence_run"`, false},
		"chart digest evidence":     {`printf 'chart-sha256=%s\n' "$chart_sha256"`, `printf 'chart-sha256=%s\n' unknown`, false},
		"window evidence":           {`printf 'kubernetes-support-window=%s\n' "$kubernetes_support_window"`, `printf 'kubernetes-support-window=%s\n' 1.99`, false},
		"run evidence":              {`printf 'support-evidence-run-id=%s\n' "$evidence_run"`, `printf 'support-evidence-run-id=%s\n' 1`, false},
		"publish dependency":        {`    needs: [support-preflight]`, `    needs: []`, false},
		"publish evidence binding":  {`needs.support-preflight.outputs.source-sha == github.sha`, `github.sha == github.sha`, false},
		"preflight error bypass":    {`        id: support-evidence`, "        id: support-evidence\n        continue-on-error: true", false},
		"protected environment":     {`    environment: release`, `    environment: unprotected`, false},
		"source ancestry":           {`git merge-base --is-ancestor "$GITHUB_SHA"`, `git merge-base "$GITHUB_SHA"`, false},
		"write permission":          {`      id-token: write`, `      id-token: read`, false},
		"attestation metadata":      {`      artifact-metadata: write`, `      artifact-metadata: read`, false},
		"action pin":                {`actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`, `actions/checkout@v6`, true},
		"publish Go cache":          {`          cache: false`, `          cache: true`, false},
		"Buildx version":            {`          version: v0.36.1`, `          version: latest`, true},
		"BuildKit digest":           {`image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8`, `image=moby/buildkit:v0.32.2`, true},
		"executor version":          {`            --set-string execution.ptahVersion="release-smoke-explicit"`, `            true`, false},
		"image platform":            {`platforms: linux/amd64,linux/arm64`, `platforms: linux/amd64`, true},
		"image push":                {`          push: true`, `          push: false`, false},
		"image provenance":          {`          provenance: mode=max`, `          provenance: false`, false},
		"image SBOM":                {`          sbom: generator=docker.io/docker/buildkit-syft-scanner:stable-1@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9`, `          sbom: true`, false},
		"tested chart binding":      {`TESTED_CHART_SHA256: ${{ needs.support-preflight.outputs.chart-sha256 }}`, `TESTED_CHART_SHA256: unknown`, false},
		"tested chart equality":     {`[[ "$chart_sha256" == "$TESTED_CHART_SHA256" ]]`, `test -n "$chart_sha256"`, false},
		"artifacts run binding":     {`TESTED_SUPPORT_EVIDENCE_RUN_ID: ${{ needs.support-preflight.outputs.support-evidence-run-id }}`, `TESTED_SUPPORT_EVIDENCE_RUN_ID: 1`, false},
		"artifacts window binding":  {`TESTED_KUBERNETES_SUPPORT_WINDOW: ${{ needs.support-preflight.outputs.kubernetes-support-window }}`, `TESTED_KUBERNETES_SUPPORT_WINDOW: 1.99`, false},
		"manifest run evidence":     {`printf 'support-evidence-run-id=%s\n' "$TESTED_SUPPORT_EVIDENCE_RUN_ID"`, `printf 'support-evidence-run-id=%s\n' 1`, false},
		"manifest window evidence":  {`printf 'kubernetes-support-window=%s\n' "$TESTED_KUBERNETES_SUPPORT_WINDOW"`, `printf 'kubernetes-support-window=%s\n' 1.99`, false},
		"asset manifest binding":    {`            dist/release-manifest.txt`, `            dist/not-the-manifest.txt`, true},
		"image attestation digest":  {`subject-digest: ${{ steps.artifacts.outputs.image-digest }}`, `subject-digest: ${{ steps.artifacts.outputs.chart-digest }}`, false},
		"image signature digest":    {`${{ steps.artifacts.outputs.image-repository }}@${{ steps.artifacts.outputs.image-digest }}`, `${{ steps.artifacts.outputs.image-repository }}@sha256:bad`, true},
		"published guard":           {`published but not immutable; refusing recovery`, `published release may be reused`, false},
		"published recovery":        {`              release_state=published`, `              release_state=recover`, false},
		"immutability preflight":    {`"repos/$GITHUB_REPOSITORY/immutable-releases"`, `"repos/$GITHUB_REPOSITORY/releases"`, false},
		"prepared journal":          {`            --notes-file dist/release-journal.txt`, `            --notes 'mutable'`, false},
		"stable transaction":        {`            transaction="$GITHUB_RUN_ID"`, `            transaction="$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"`, false},
		"prepared image reuse":      {`docker buildx imagetools inspect --raw "$reference"`, `false`, false},
		"staging checkpoint":        {`gh attestation verify "oci://$IMAGE@$digest"`, `test -n "$digest"`, false},
		"checkpoint digest":         {`          subject-digest: ${{ steps.image.outputs.digest }}`, `          subject-digest: sha256:bad`, false},
		"registry error binding":    {`            -registry-missing-reference "$reference"`, `            -registry-missing-reference ghcr.io/example/other:tag`, false},
		"Docker material verifier":  {`            -provenance "$image_dir/provenance.json"`, `            -provenance /dev/null`, false},
		"live tag binding":          {`            -verify-tag-identity`, `            -verify-tag-identity=false`, true},
		"asset source ref":          {`              --source-ref "$GITHUB_REF"`, `              --source-ref refs/tags/v-any`, true},
		"asset comparison":          {`          cmp dist/release-manifest.txt`, `          test -f dist/release-manifest.txt`, false},
		"publish gate attestation":  {`gh attestation verify "$gate_dir/$name"`, `test -f "$gate_dir/$name"`, false},
		"starter cleanup":           {`gh api --method DELETE`, `gh api --method GET`, false},
		"signature identity":        {`--certificate-identity "$identity"`, `--certificate-identity-regexp '.*'`, false},
		"retention tag binding":     {`          cmp "$image_dir/index.json" "$image_dir/tag-index.json"`, `          true`, false},
		"platform contract":         {`              ["linux/amd64", "linux/arm64"] and`, `              ["linux/amd64"] and`, false},
		"max provenance":            {`            -provenance-revision "$GITHUB_SHA"`, `            -provenance-revision unknown`, false},
		"final publication":         {`gh release edit "$GITHUB_REF_NAME" --draft=false`, `gh release edit "$GITHUB_REF_NAME" --draft=true`, false},
		"immutable verification":    {`          [[ "$(jq -r '.immutable' <<<"$release_json")" == true ]]`, `          true`, false},
		"asset replacement":         {`gh release upload "$GITHUB_REF_NAME" "$source"`, `gh release upload "$GITHUB_REF_NAME" "$source" --clobber`, false},
		"extra privileged step":     {`      - name: Publish completed release transaction`, "      - name: Injected\n        id: injected\n        run: true\n      - name: Publish completed release transaction", false},
		"dead shell branch":         {`          gh release edit "$GITHUB_REF_NAME" --draft=false --latest=false`, "          if false; then\n            echo bypass\n          fi\n          gh release edit \"$GITHUB_REF_NAME\" --draft=false --latest=false", false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			mutated := string(workflow)
			if !strings.Contains(mutated, test.old) {
				t.Fatalf("fixture does not contain %q", test.old)
			}
			if test.all {
				mutated = strings.ReplaceAll(mutated, test.old, test.new)
			} else {
				mutated = strings.Replace(mutated, test.old, test.new, 1)
			}
			if err := verifyWorkflowSemantics([]byte(mutated)); err == nil {
				t.Fatal("verifyWorkflowSemantics() accepted a critical mutation")
			}
		})
	}
}

func TestVerifyWorkflowDigestIsAnIndependentTripwire(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyWorkflowDigest(workflow); err != nil {
		t.Fatalf("verifyWorkflowDigest(valid) error = %v", err)
	}
	mutated := append(append([]byte(nil), workflow...), []byte("\n# Semantically inert audit-tripwire mutation.\n")...)
	if err := verifyWorkflowSemantics(mutated); err != nil {
		t.Fatalf("verifyWorkflowSemantics(commented) error = %v", err)
	}
	if err := verifyWorkflowDigest(mutated); err == nil {
		t.Fatal("verifyWorkflowDigest() accepted changed workflow bytes")
	}
}

func TestParseKubernetesSupportWindow(t *testing.T) {
	t.Parallel()

	valid := []byte(`{
  "schemaVersion": 1,
  "policy": "upstream-active-minors",
  "windowSize": 3,
  "lastVerified": "2026-08-31",
  "kindVersion": "v0.33.0",
  "releases": [
    {"minor": "1.35", "nodeImage": "image-35"},
    {"minor": "1.36", "nodeImage": "image-36"},
    {"minor": "1.37", "nodeImage": "image-37"}
  ]
}
`)
	window, err := parseKubernetesSupportWindow(valid)
	if err != nil {
		t.Fatal(err)
	}
	if window != "1.35,1.36,1.37" {
		t.Fatalf("parseKubernetesSupportWindow() = %q, want %q", window, "1.35,1.36,1.37")
	}

	for name, document := range map[string][]byte{
		"unknown field":     bytes.Replace(valid, []byte(`"schemaVersion": 1`), []byte(`"schemaVersion": 1, "extra": true`), 1),
		"wrong window size": bytes.Replace(valid, []byte(`"windowSize": 3`), []byte(`"windowSize": 2`), 1),
		"invalid date":      bytes.Replace(valid, []byte(`"2026-08-31"`), []byte(`"2026/08/31"`), 1),
		"leading zero":      bytes.Replace(valid, []byte(`"1.35"`), []byte(`"01.35"`), 1),
		"nonconsecutive":    bytes.Replace(valid, []byte(`"1.36"`), []byte(`"1.38"`), 1),
		"duplicate image":   bytes.Replace(valid, []byte(`"image-36"`), []byte(`"image-35"`), 1),
		"trailing value":    append(append([]byte(nil), valid...), []byte("{}\n")...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseKubernetesSupportWindow(document); err == nil {
				t.Fatal("parseKubernetesSupportWindow() accepted an invalid support manifest")
			}
		})
	}
}

func TestVerifyReleaseAssets(t *testing.T) {
	t.Parallel()

	const (
		tag       = "v0.1.0"
		sourceSHA = "1111111111111111111111111111111111111111"
	)
	root := filepath.Join("..", "..")
	supportWindow, err := repositoryKubernetesSupportWindow(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	chartName := "ptah-operator-0.1.0.tgz"
	chartPath := filepath.Join(directory, chartName)
	chart := []byte("deterministic chart bytes")
	if err := os.WriteFile(chartPath, chart, 0o600); err != nil {
		t.Fatal(err)
	}
	chartSum := fmt.Sprintf("%x", sha256.Sum256(chart))
	digest := "sha256:" + strings.Repeat("2", 64)
	manifest := fmt.Sprintf("version=0.1.0\n"+
		"source-repository=%s\n"+
		"source-ref=refs/tags/%s\n"+
		"source-sha=%s\n"+
		"transaction=123\n"+
		"image=%s@%s\n"+
		"image-tag=%s:tx-%s-123\n"+
		"chart-asset=%s\n"+
		"chart-asset-sha256=%s\n"+
		"support-evidence-run-id=456\n"+
		"kubernetes-support-window=%s\n",
		repositoryName, tag, sourceSHA, imageName, digest, imageName, sourceSHA, chartName, chartSum, supportWindow)
	manifestPath := filepath.Join(directory, "release-manifest.txt")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	checksums := fmt.Sprintf("%s  %s\n%x  release-manifest.txt\n",
		chartSum, chartName, sha256.Sum256([]byte(manifest)))
	checksumsPath := filepath.Join(directory, "SHA256SUMS")
	if err := os.WriteFile(checksumsPath, []byte(checksums), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseAssets(root, manifestPath, checksumsPath, chartPath, tag, sourceSHA); err != nil {
		t.Fatalf("verifyReleaseAssets(valid) error = %v", err)
	}
	for name, mutation := range map[string]string{
		"zero support run":     strings.Replace(manifest, "support-evidence-run-id=456", "support-evidence-run-id=0", 1),
		"different window":     strings.Replace(manifest, "kubernetes-support-window="+supportWindow, "kubernetes-support-window=9.98,9.99,9.100", 1),
		"missing support run":  strings.Replace(manifest, "support-evidence-run-id=456\n", "", 1),
		"extra manifest field": manifest + "unexpected=value\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(manifestPath, []byte(mutation), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyReleaseAssets(root, manifestPath, checksumsPath, chartPath, tag, sourceSHA); err == nil {
				t.Fatal("verifyReleaseAssets() accepted a manifest evidence mutation")
			}
		})
	}
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chartPath, append(chart, '!'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseAssets(root, manifestPath, checksumsPath, chartPath, tag, sourceSHA); err == nil {
		t.Fatal("verifyReleaseAssets() accepted a changed chart")
	}
}

func TestVerifyPreparedJournal(t *testing.T) {
	t.Parallel()

	const (
		tag       = "v0.1.0"
		sourceSHA = "1111111111111111111111111111111111111111"
	)
	journal := fmt.Sprintf("state=prepared\n"+
		"version=0.1.0\n"+
		"source-repository=%s\n"+
		"source-ref=refs/tags/%s\n"+
		"source-sha=%s\n"+
		"transaction=123\n"+
		"image-tag=%s:tx-%s-123\n"+
		"chart-asset=ptah-operator-0.1.0.tgz\n",
		repositoryName, tag, sourceSHA, imageName, sourceSHA)
	path := filepath.Join(t.TempDir(), "release-journal.txt")
	if err := os.WriteFile(path, []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparedJournal(path, tag, sourceSHA); err != nil {
		t.Fatalf("verifyPreparedJournal(valid) error = %v", err)
	}
	mutated := strings.Replace(journal, "transaction=123", "transaction=123-1", 1)
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparedJournal(path, tag, sourceSHA); err == nil {
		t.Fatal("verifyPreparedJournal() accepted an unstable run-attempt transaction")
	}
	withEvidence := strings.Replace(journal,
		"chart-asset=ptah-operator-0.1.0.tgz\n",
		"chart-asset=ptah-operator-0.1.0.tgz\nsupport-evidence-run-id=456\n",
		1,
	)
	if err := os.WriteFile(path, []byte(withEvidence), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparedJournal(path, tag, sourceSHA); err == nil {
		t.Fatal("verifyPreparedJournal() accepted final evidence in the intent-only journal")
	}
}
