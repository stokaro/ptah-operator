package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"FROM gcr.io/distroless/static-debian13:nonroot@" + digest + "\n" +
		"LABEL org.opencontainers.image.source=x org.opencontainers.image.revision=y org.opencontainers.image.version=z\n")
	if err := verifyDockerfile(valid, "1.27.0"); err != nil {
		t.Fatalf("verifyDockerfile() error = %v", err)
	}
	mutableFrontend := []byte("# syntax=docker/dockerfile:1.7\n" + string(valid[strings.Index(string(valid), "FROM"):]))
	if err := verifyDockerfile(mutableFrontend, "1.27.0"); err == nil {
		t.Fatal("verifyDockerfile() accepted a mutable syntax frontend")
	}
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
		"tag trigger":              {`      - "v*"`, `      - main`, false},
		"tag job guard":            {`startsWith(github.ref, 'refs/tags/v')`, `startsWith(github.ref, 'refs/heads/')`, false},
		"protected environment":    {`    environment: release`, `    environment: unprotected`, false},
		"source ancestry":          {`git merge-base --is-ancestor "$GITHUB_SHA"`, `git merge-base "$GITHUB_SHA"`, false},
		"write permission":         {`      id-token: write`, `      id-token: read`, false},
		"attestation metadata":     {`      artifact-metadata: write`, `      artifact-metadata: read`, false},
		"action pin":               {`actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`, `actions/checkout@v6`, true},
		"publish Go cache":         {`          cache: false`, `          cache: true`, false},
		"Buildx version":           {`          version: v0.36.1`, `          version: latest`, true},
		"BuildKit digest":          {`image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8`, `image=moby/buildkit:v0.32.2`, true},
		"image platform":           {`platforms: linux/amd64,linux/arm64`, `platforms: linux/amd64`, true},
		"image push":               {`          push: true`, `          push: false`, false},
		"image provenance":         {`          provenance: mode=max`, `          provenance: false`, false},
		"image SBOM":               {`          sbom: true`, `          sbom: false`, false},
		"asset manifest binding":   {`            dist/release-manifest.txt`, `            dist/not-the-manifest.txt`, true},
		"image attestation digest": {`subject-digest: ${{ steps.artifacts.outputs.image-digest }}`, `subject-digest: ${{ steps.artifacts.outputs.chart-digest }}`, false},
		"image signature digest":   {`${{ steps.artifacts.outputs.image-repository }}@${{ steps.artifacts.outputs.image-digest }}`, `${{ steps.artifacts.outputs.image-repository }}@sha256:bad`, true},
		"published guard":          {`published but not immutable; refusing recovery`, `published release may be reused`, false},
		"published recovery":       {`              release_state=published`, `              release_state=recover`, false},
		"immutability preflight":   {`"repos/$GITHUB_REPOSITORY/immutable-releases"`, `"repos/$GITHUB_REPOSITORY/releases"`, false},
		"draft journal":            {`            --notes-file dist/release-manifest.txt`, `            --notes 'mutable'`, false},
		"live tag binding":         {`            -verify-tag-identity`, `            -verify-tag-identity=false`, true},
		"asset source ref":         {`              --source-ref "$GITHUB_REF"`, `              --source-ref refs/tags/v-any`, true},
		"asset comparison":         {`          cmp dist/release-manifest.txt`, `          test -f dist/release-manifest.txt`, false},
		"publish gate attestation": {`gh attestation verify "$gate_dir/$name"`, `test -f "$gate_dir/$name"`, false},
		"starter cleanup":          {`gh api --method DELETE`, `gh api --method GET`, false},
		"signature identity":       {`--certificate-identity "$identity"`, `--certificate-identity-regexp '.*'`, false},
		"retention tag binding":    {`          cmp "$image_dir/index.json" "$image_dir/tag-index.json"`, `          true`, false},
		"platform contract":        {`              ["linux/amd64", "linux/arm64"] and`, `              ["linux/amd64"] and`, false},
		"max provenance":           {`llbDefinition | length`, `resolvedDependencies | length`, false},
		"final publication":        {`gh release edit "$GITHUB_REF_NAME" --draft=false`, `gh release edit "$GITHUB_REF_NAME" --draft=true`, false},
		"immutable verification":   {`          [[ "$(jq -r '.immutable' <<<"$release_json")" == true ]]`, `          true`, false},
		"asset replacement":        {`gh release upload "$GITHUB_REF_NAME" "$source"`, `gh release upload "$GITHUB_REF_NAME" "$source" --clobber`, false},
		"extra privileged step":    {`      - name: Publish completed release transaction`, "      - name: Injected\n        id: injected\n        run: true\n      - name: Publish completed release transaction", false},
		"dead shell branch":        {`          gh release edit "$GITHUB_REF_NAME" --draft=false --latest=false`, "          if false; then\n            echo bypass\n          fi\n          gh release edit \"$GITHUB_REF_NAME\" --draft=false --latest=false", false},
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

func TestVerifyReleaseAssets(t *testing.T) {
	t.Parallel()

	const (
		tag       = "v0.1.0"
		sourceSHA = "1111111111111111111111111111111111111111"
	)
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
		"transaction=123-1\n"+
		"image=%s@%s\n"+
		"image-tag=%s:tx-%s-123-1\n"+
		"chart-asset=%s\n"+
		"chart-asset-sha256=%s\n",
		repositoryName, tag, sourceSHA, imageName, digest, imageName, sourceSHA, chartName, chartSum)
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
	if err := verifyReleaseAssets(manifestPath, checksumsPath, chartPath, tag, sourceSHA); err != nil {
		t.Fatalf("verifyReleaseAssets(valid) error = %v", err)
	}
	if err := os.WriteFile(chartPath, append(chart, '!'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseAssets(manifestPath, checksumsPath, chartPath, tag, sourceSHA); err == nil {
		t.Fatal("verifyReleaseAssets() accepted a changed chart")
	}
}
