package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	semanticVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	digestReferencePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	actionPinPattern       = regexp.MustCompile(`^[^[:space:]@]+@[0-9a-f]{40}$`)
	commitPattern          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	transactionPattern     = regexp.MustCompile(`^[1-9][0-9]*-[1-9][0-9]*$`)
	fromPattern            = regexp.MustCompile(`^FROM(?:[[:space:]]+--platform=[^[:space:]]+)?[[:space:]]+([^[:space:]]+)`)
	releaseRunSHA256       = map[string]string{
		"smoke/verify-release":           "c91171f73101c06d5d1fdae3f0c4bd405ba7ea6af07e0b76ba38fbb3b1258520",
		"smoke/chart-reproducibility":    "5db0f6150b5129d7be4c68067a3a3d47226a54bc31c38b0d5c72c9e062d290d4",
		"publish/release":                "a056fb01f58446cfa404a8aaa769061362082ea41a0e7dbef63c00e361cc7f0c",
		"publish/transaction":            "9e6bf973fe64871333c89135aa50b43e7c027e2565aff55ce4f1591421453833",
		"publish/immutability-preflight": "08d725a97a83d3a7c16fc1fe7c0e75f8b363a9e5fc43e79482a83996d9b99025",
		"publish/chart-package":          "d5b44cddda0c6f79697af28047accd9cf48c7bde7e2e72826078ea9937b316a2",
		"publish/artifacts":              "7ea27fb39b449c8f909f3ab35d974fb548efcb15d8e30279a825b99c614a48e2",
		"publish/draft":                  "8986affda71bc677155e4e1f29e0bf110a3818b21d376690bbe984ed824e8c50",
		"publish/asset-auth":             "e1c7c1e7eefef128a64a883a73c56dab37d8f1dd24436daa84b7a077896ea8ee",
		"publish/asset-sync":             "8ac2fce0ed0460b72eee90bb530e1c17da79ed62f4e54877a780bd8dbbc34e4e",
		"publish/image-signature":        "e0b994a90bc38dd8019f4b4157a72e5f6cab1873f3bc1ca39b8ca41dcb023d5e",
		"publish/final-verify":           "92ea4fd3c086415bac299fc75f3cee536aa04b6064958dd61faf9d26b15fff11",
		"publish/publish-release":        "3ff6501266556d3d352e9af3af8b7f2ea79db14ff5f474417b49a9686856de8e",
	}
)

const (
	repositoryName = "stokaro/ptah-operator"
	imageName      = "ghcr.io/stokaro/ptah-operator"
	buildxVersion  = "v0.36.1"
	buildkitImage  = "moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8"
	// releaseWorkflowSHA256 makes every workflow edit an explicit policy edit.
	// Semantic checks below keep the failure actionable; the digest closes gaps
	// where critical shell text could otherwise be hidden in comments or dead branches.
	releaseWorkflowSHA256 = "4a4e5e1a7d6167accb64700a35b351bf51f4a121128f757dd22091cf61c81a5d"
)

func main() {
	root := flag.String("root", ".", "repository root")
	tag := flag.String("tag", "", "release tag to verify")
	manifest := flag.String("manifest", "", "release manifest to verify")
	checksums := flag.String("checksums", "", "SHA256SUMS file to verify")
	chart := flag.String("chart", "", "packaged chart to verify")
	sourceSHA := flag.String("source-sha", "", "release source commit SHA")
	verifyTagIdentity := flag.Bool("verify-tag-identity", false, "verify that the live GitHub tag still peels to GITHUB_SHA")
	flag.Parse()

	if err := verifyRepository(*root, *tag); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *verifyTagIdentity {
		apiURL := os.Getenv("GITHUB_API_URL")
		if apiURL == "" {
			apiURL = "https://api.github.com"
		}
		if err := verifyGitHubTagIdentity(
			context.Background(),
			&http.Client{Timeout: 15 * time.Second},
			apiURL,
			os.Getenv("GITHUB_REPOSITORY"),
			os.Getenv("GITHUB_REF_NAME"),
			os.Getenv("GITHUB_SHA"),
			os.Getenv("GH_TOKEN"),
		); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *manifest != "" || *checksums != "" || *chart != "" || *sourceSHA != "" {
		if *manifest == "" || *tag == "" || *sourceSHA == "" {
			fmt.Fprintln(os.Stderr, "manifest verification requires -manifest, -tag, and -source-sha")
			os.Exit(1)
		}
		if (*checksums == "") != (*chart == "") {
			fmt.Fprintln(os.Stderr, "-checksums and -chart must be supplied together")
			os.Exit(1)
		}
		if err := verifyReleaseAssets(*manifest, *checksums, *chart, *tag, *sourceSHA); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Println("release inputs are internally consistent")
}

type gitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type gitReferenceResponse struct {
	Object gitObject `json:"object"`
}

func verifyGitHubTagIdentity(
	ctx context.Context,
	client *http.Client,
	apiURL, repository, tag, sourceSHA, token string,
) error {
	if client == nil {
		return errors.New("verify release tag identity: HTTP client is required")
	}
	if repository != repositoryName {
		return fmt.Errorf("verify release tag identity: repository %q is not %q", repository, repositoryName)
	}
	if tag == "" || sourceSHA == "" || token == "" {
		return errors.New("verify release tag identity: tag, source SHA, and token are required")
	}
	if !commitPattern.MatchString(sourceSHA) {
		return fmt.Errorf("verify release tag identity: source SHA %q is invalid", sourceSHA)
	}
	base, err := url.Parse(apiURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("verify release tag identity: GitHub API URL %q is invalid", apiURL)
	}

	object, err := fetchGitObject(ctx, client, base, token,
		"repos/stokaro/ptah-operator/git/ref/tags/"+url.PathEscape(tag))
	if err != nil {
		return err
	}
	seen := make(map[string]struct{})
	for depth := 0; object.Type == "tag"; depth++ {
		if depth >= 16 {
			return errors.New("verify release tag identity: annotated tag chain is too deep")
		}
		if !commitPattern.MatchString(object.SHA) {
			return fmt.Errorf("verify release tag identity: tag object SHA %q is invalid", object.SHA)
		}
		if _, duplicate := seen[object.SHA]; duplicate {
			return errors.New("verify release tag identity: annotated tag chain contains a cycle")
		}
		seen[object.SHA] = struct{}{}
		object, err = fetchGitObject(ctx, client, base, token,
			"repos/stokaro/ptah-operator/git/tags/"+object.SHA)
		if err != nil {
			return err
		}
	}
	if object.Type != "commit" || !commitPattern.MatchString(object.SHA) {
		return fmt.Errorf("verify release tag identity: tag resolves to invalid %q object %q", object.Type, object.SHA)
	}
	if object.SHA != sourceSHA {
		return fmt.Errorf("verify release tag identity: live tag resolves to %s, workflow source is %s", object.SHA, sourceSHA)
	}
	return nil
}

func fetchGitObject(
	ctx context.Context,
	client *http.Client,
	base *url.URL,
	token, relativePath string,
) (gitObject, error) {
	endpoint := *base
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/" + relativePath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return gitObject{}, fmt.Errorf("verify release tag identity: create GitHub API request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := client.Do(request)
	if err != nil {
		return gitObject{}, fmt.Errorf("verify release tag identity: query GitHub API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return gitObject{}, fmt.Errorf("verify release tag identity: GitHub API returned %s", response.Status)
	}
	var payload gitReferenceResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return gitObject{}, fmt.Errorf("verify release tag identity: decode GitHub API response: %w", err)
	}
	return payload.Object, nil
}

func verifyRepository(root, tag string) error {
	chart, err := os.ReadFile(filepath.Join(root, "charts", "ptah-operator", "Chart.yaml"))
	if err != nil {
		return fmt.Errorf("read Chart.yaml: %w", err)
	}
	values, err := os.ReadFile(filepath.Join(root, "charts", "ptah-operator", "values.yaml"))
	if err != nil {
		return fmt.Errorf("read values.yaml: %w", err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		return fmt.Errorf("read Dockerfile: %w", err)
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		return fmt.Errorf("read release workflow: %w", err)
	}

	version, err := topLevelScalar(chart, "version")
	if err != nil {
		return err
	}
	if !semanticVersionPattern.MatchString(version) {
		return fmt.Errorf("chart version %q is not a supported semantic version", version)
	}
	appVersion, err := topLevelScalar(chart, "appVersion")
	if err != nil {
		return err
	}
	if appVersion != version {
		return fmt.Errorf("chart version %q and appVersion %q differ", version, appVersion)
	}
	imageTag, err := nestedScalar(values, "image", "tag")
	if err != nil {
		return err
	}
	if imageTag != version {
		return fmt.Errorf("default manager image tag %q does not match chart version %q", imageTag, version)
	}
	if tag != "" && tag != "v"+version {
		return fmt.Errorf("release tag %q must equal v%s", tag, version)
	}

	toolchain, err := goToolchain(module)
	if err != nil {
		return err
	}
	if err := verifyDockerfile(dockerfile, toolchain); err != nil {
		return err
	}
	if err := verifyWorkflow(workflow); err != nil {
		return err
	}
	return nil
}

func topLevelScalar(document []byte, key string) (string, error) {
	return yamlScalar(document, "", key)
}

func nestedScalar(document []byte, section, key string) (string, error) {
	return yamlScalar(document, section, key)
}

func yamlScalar(document []byte, section, key string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(document)))
	active := section == ""
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if section != "" && indent == 0 {
			active = trimmed == section+":"
			continue
		}
		if !active || section == "" && indent != 0 || section != "" && indent != 2 {
			continue
		}
		field, value, found := strings.Cut(trimmed, ":")
		if found && field == key {
			return strings.Trim(strings.TrimSpace(value), `"'`), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if section == "" {
		return "", fmt.Errorf("top-level YAML field %q is missing", key)
	}
	return "", fmt.Errorf("YAML field %s.%s is missing", section, key)
}

func goToolchain(module []byte) (string, error) {
	var languageVersion string
	scanner := bufio.NewScanner(strings.NewReader(string(module)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "toolchain":
			if !strings.HasPrefix(fields[1], "go") {
				return "", fmt.Errorf("invalid Go toolchain %q", fields[1])
			}
			return strings.TrimPrefix(fields[1], "go"), nil
		case "go":
			languageVersion = fields[1]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if languageVersion == "" {
		return "", errors.New("go.mod has neither a toolchain nor a go version")
	}
	return languageVersion, nil
}

func verifyDockerfile(document []byte, toolchain string) error {
	lines := strings.Split(string(document), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# syntax=") {
		return errors.New("Dockerfile must start with a digest-pinned syntax frontend")
	}
	frontend := strings.TrimSpace(strings.TrimPrefix(lines[0], "# syntax="))
	if !digestReferencePattern.MatchString(frontend) || !strings.HasPrefix(frontend, "docker/dockerfile:") {
		return fmt.Errorf("Dockerfile syntax frontend %q is not digest-pinned", frontend)
	}

	var references []string
	scanner := bufio.NewScanner(strings.NewReader(string(document)))
	for scanner.Scan() {
		matches := fromPattern.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if len(matches) == 2 {
			references = append(references, matches[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(references) < 2 {
		return errors.New("Dockerfile must contain pinned builder and runtime stages")
	}
	for _, reference := range references {
		if !digestReferencePattern.MatchString(reference) {
			return fmt.Errorf("Dockerfile base image %q is not digest-pinned", reference)
		}
	}
	wantBuilder := "golang:" + toolchain + "-alpine@"
	if !strings.HasPrefix(references[0], wantBuilder) {
		return fmt.Errorf("Dockerfile builder %q does not match Go toolchain %s", references[0], toolchain)
	}
	if !strings.HasPrefix(references[len(references)-1], "gcr.io/distroless/static-debian13:nonroot@") {
		return fmt.Errorf("Dockerfile runtime %q is not the expected non-root image", references[len(references)-1])
	}
	for _, label := range []string{
		"org.opencontainers.image.source",
		"org.opencontainers.image.revision",
		"org.opencontainers.image.version",
	} {
		if !strings.Contains(string(document), label) {
			return fmt.Errorf("Dockerfile is missing OCI label %s", label)
		}
	}
	return nil
}

type workflowDocument struct {
	On          map[string]yaml.Node   `yaml:"on"`
	Concurrency workflowConcurrency    `yaml:"concurrency"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type workflowJob struct {
	If          string            `yaml:"if"`
	Environment string            `yaml:"environment"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	ID   string         `yaml:"id"`
	If   string         `yaml:"if"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

func verifyWorkflow(document []byte) error {
	if err := verifyWorkflowSemantics(document); err != nil {
		return err
	}
	return verifyWorkflowDigest(document)
}

func verifyWorkflowSemantics(document []byte) error {
	var workflow workflowDocument
	decoder := yaml.NewDecoder(bytes.NewReader(document))
	if err := decoder.Decode(&workflow); err != nil {
		return fmt.Errorf("parse release workflow: %w", err)
	}
	if len(workflow.On) != 2 {
		return errors.New("release workflow must have only pull_request and tag push triggers")
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		return errors.New("release workflow must run smoke checks on pull requests")
	}
	push, ok := workflow.On["push"]
	if !ok {
		return errors.New("release workflow must publish from tag pushes")
	}
	var pushConfig struct {
		Tags []string `yaml:"tags"`
	}
	if err := push.Decode(&pushConfig); err != nil || len(pushConfig.Tags) != 1 || pushConfig.Tags[0] != "v*" {
		return errors.New("release workflow push trigger must contain only v* tags")
	}
	if workflow.Concurrency.Group != "release-${{ github.ref }}" || workflow.Concurrency.CancelInProgress {
		return errors.New("release workflow must serialize each tag without canceling an active transaction")
	}
	if !equalStringMap(workflow.Permissions, map[string]string{"contents": "read"}) {
		return errors.New("release workflow top-level permissions must be contents: read only")
	}
	if len(workflow.Jobs) != 2 {
		return errors.New("release workflow must contain exactly the smoke and publish jobs")
	}
	smoke, ok := workflow.Jobs["smoke"]
	if !ok {
		return errors.New("release workflow has no smoke job")
	}
	if smoke.If != "github.event_name == 'pull_request'" || len(smoke.Permissions) != 0 {
		return errors.New("smoke job must be read-only and gated to pull requests")
	}
	if err := verifyStepContract("smoke", smoke.Steps,
		[]string{"checkout", "setup-go", "setup-helm", "verify-release", "chart-reproducibility", "setup-buildx", "build"},
		map[string]string{
			"checkout":     "actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
			"setup-go":     "actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
			"setup-helm":   "azure/setup-helm@1a275c3b69536ee54be43f2070a358922e12c8d4",
			"setup-buildx": "docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e",
			"build":        "docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a",
		}); err != nil {
		return err
	}

	publish, ok := workflow.Jobs["publish"]
	if !ok {
		return errors.New("release workflow has no publish job")
	}
	if publish.If != "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')" {
		return errors.New("publish job must be gated to v* tag push refs")
	}
	if publish.Environment != "release" {
		return errors.New("publish job must use the protected release environment")
	}
	wantPermissions := map[string]string{
		"contents":          "write",
		"packages":          "write",
		"attestations":      "write",
		"artifact-metadata": "write",
		"id-token":          "write",
	}
	if !equalStringMap(publish.Permissions, wantPermissions) {
		return errors.New("publish job permissions do not match the release contract")
	}
	if err := verifyStepContract("publish", publish.Steps,
		[]string{
			"checkout", "setup-go", "setup-buildx", "release", "transaction",
			"immutability-preflight", "registry-login", "image", "chart-package", "artifacts",
			"asset-attestation", "draft", "asset-auth", "asset-sync", "image-attestation",
			"setup-cosign", "image-signature", "final-verify", "publish-release",
		},
		map[string]string{
			"checkout":          "actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
			"setup-go":          "actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
			"setup-buildx":      "docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e",
			"registry-login":    "docker/login-action@dbcb813823bdd20940b903addbd779551569679f",
			"image":             "docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a",
			"asset-attestation": "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
			"image-attestation": "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
			"setup-cosign":      "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6",
		}); err != nil {
		return err
	}
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if step.Uses != "" && !actionPinPattern.MatchString(step.Uses) {
				return fmt.Errorf("release action %q in job %s is not pinned to a full commit", step.Uses, jobName)
			}
			if strings.Contains(step.Run, "--clobber") {
				return fmt.Errorf("release step %q can replace an existing asset", step.ID)
			}
		}
	}

	steps, err := stepsByID(publish.Steps)
	if err != nil {
		return err
	}
	checkout, err := requireStep(steps, "checkout")
	if err != nil {
		return err
	}
	if value(checkout.With, "persist-credentials") != "false" || value(checkout.With, "fetch-depth") != "0" {
		return errors.New("publish checkout must fetch full history without persisting credentials")
	}
	setupGo, err := requireStep(steps, "setup-go")
	if err != nil {
		return err
	}
	if value(setupGo.With, "go-version-file") != "go.mod" || value(setupGo.With, "cache") != "false" ||
		value(setupGo.With, "cache-dependency-path") != "" {
		return errors.New("publish Go setup must use go.mod with all action caches disabled")
	}
	for _, job := range []struct {
		name  string
		steps []workflowStep
	}{{name: "smoke", steps: smoke.Steps}, {name: "publish", steps: publish.Steps}} {
		jobSteps, err := stepsByID(job.steps)
		if err != nil {
			return err
		}
		setupBuildx, err := requireStep(jobSteps, "setup-buildx")
		if err != nil {
			return err
		}
		if value(setupBuildx.With, "version") != buildxVersion ||
			value(setupBuildx.With, "cache-binary") != "false" ||
			value(setupBuildx.With, "driver-opts") != "image="+buildkitImage+"\n" {
			return fmt.Errorf("release job %s must use the audited Buildx and BuildKit inputs with caching disabled", job.name)
		}
	}
	if err := requireRunBindings(steps, "release",
		"github.event.repository.default_branch", "git fetch --no-tags origin",
		"git merge-base --is-ancestor \"$GITHUB_SHA\""); err != nil {
		return err
	}
	image, err := requireStep(steps, "image")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(image.Uses, "docker/build-push-action@") ||
		value(image.With, "platforms") != "linux/amd64,linux/arm64" ||
		value(image.With, "push") != "true" ||
		value(image.With, "provenance") != "mode=max" ||
		value(image.With, "sbom") != "true" ||
		value(image.With, "tags") != "${{ env.IMAGE }}:${{ steps.release.outputs.image-tag }}" {
		return errors.New("publish image step must push the staged multi-architecture SBOM/provenance build")
	}
	if image.If != "steps.transaction.outputs.mode == 'fresh'" {
		return errors.New("publish image step must run only for a fresh release transaction")
	}
	for id, condition := range map[string]string{
		"registry-login":  "steps.transaction.outputs.mode != 'published'",
		"draft":           "steps.transaction.outputs.mode == 'fresh'",
		"asset-sync":      "steps.transaction.outputs.mode != 'published'",
		"image-signature": "steps.transaction.outputs.mode != 'published'",
	} {
		step, err := requireStep(steps, id)
		if err != nil {
			return err
		}
		if step.If != condition {
			return fmt.Errorf("release step %q must use condition %q", id, condition)
		}
	}

	if err := verifyAttestationStep(steps, "asset-attestation", map[string]string{
		"subject-path": "${{ steps.chart-package.outputs.path }}\ndist/release-manifest.txt\ndist/SHA256SUMS\n",
	}, "steps.transaction.outputs.mode == 'fresh'"); err != nil {
		return err
	}
	if err := verifyAttestationStep(steps, "image-attestation", map[string]string{
		"subject-name":     "${{ steps.artifacts.outputs.image-repository }}",
		"subject-digest":   "${{ steps.artifacts.outputs.image-digest }}",
		"push-to-registry": "true",
	}, "steps.transaction.outputs.mode != 'published'"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "image-signature",
		"cosign sign --yes", "${{ steps.artifacts.outputs.image-repository }}@${{ steps.artifacts.outputs.image-digest }}"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "transaction",
		"published but not immutable; refusing recovery",
		"release_state=published",
		"-verify-tag-identity",
		"--source-ref \"$GITHUB_REF\"",
		"--source-digest \"$GITHUB_SHA\"",
		"--signer-workflow \"$GITHUB_REPOSITORY/.github/workflows/release.yml\""); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "immutability-preflight",
		"IMMUTABLE_RELEASES_READ_TOKEN is required",
		"repos/$GITHUB_REPOSITORY/immutable-releases", ".enabled"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "draft",
		"gh release create", "--draft", "--latest=false",
		"--notes-file dist/release-manifest.txt", "-verify-tag-identity"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "asset-auth",
		"dist/release-manifest.txt", "dist/SHA256SUMS",
		"--source-ref \"$GITHUB_REF\"", "--source-digest \"$GITHUB_SHA\""); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "asset-sync",
		"cmp dist/release-manifest.txt", "gh release upload", "gh release download",
		"state\" == starter", "--method DELETE"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "final-verify",
		"--certificate-identity \"$identity\"", "--source-ref \"$GITHUB_REF\"",
		"--source-digest \"$GITHUB_SHA\"", "docker logout ghcr.io",
		"imagetools inspect --raw \"$reference\"", "steps.artifacts.outputs.image-tag",
		"linux/amd64", "linux/arm64", "vnd.docker.reference.digest",
		"https://spdx.dev/Document", "https://slsa.dev/provenance/v1",
		"org.opencontainers.image.revision", "build-arg:REVISION", "llbDefinition"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "publish-release",
		"if [[ \"$mode\" != published ]]", "gh release edit", "--draft=false", "--latest=false", "-verify-tag-identity",
		"cmp dist/release-manifest.txt", "gh release download", "gh attestation verify",
		"-checksums \"$gate_dir/SHA256SUMS\"", ".immutable", "gh release verify",
		"gh release verify-asset", "delay=$((delay < 30 ? delay * 2 : 30))"); err != nil {
		return err
	}
	if publish.Steps[len(publish.Steps)-1].ID != "publish-release" {
		return errors.New("publishing the draft must be the final release step")
	}
	return nil
}

func verifyWorkflowDigest(document []byte) error {
	actualDigest := fmt.Sprintf("%x", sha256.Sum256(document))
	if actualDigest != releaseWorkflowSHA256 {
		return fmt.Errorf("release workflow digest %s differs from the audited contract", actualDigest)
	}
	return nil
}

func verifyStepContract(jobName string, steps []workflowStep, expectedIDs []string, expectedUses map[string]string) error {
	if len(steps) != len(expectedIDs) {
		return fmt.Errorf("release job %s has %d steps, expected %d", jobName, len(steps), len(expectedIDs))
	}
	seenRunSteps := 0
	for index, step := range steps {
		wantID := expectedIDs[index]
		if step.ID != wantID {
			return fmt.Errorf("release job %s step %d has id %q, expected %q", jobName, index+1, step.ID, wantID)
		}
		wantUses := expectedUses[wantID]
		if step.Uses != wantUses {
			return fmt.Errorf("release job %s step %q uses %q, expected %q", jobName, wantID, step.Uses, wantUses)
		}
		if (step.Run == "") == (step.Uses == "") {
			return fmt.Errorf("release job %s step %q must contain exactly one of run or uses", jobName, wantID)
		}
		runKey := jobName + "/" + wantID
		wantRunDigest, runExpected := releaseRunSHA256[runKey]
		if step.Run != "" {
			seenRunSteps++
			actualRunDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(step.Run)))
			if !runExpected || actualRunDigest != wantRunDigest {
				return fmt.Errorf("release job %s step %q shell digest %s differs from the audited contract", jobName, wantID, actualRunDigest)
			}
		} else if runExpected {
			return fmt.Errorf("release job %s action step %q unexpectedly has a shell contract", jobName, wantID)
		}
	}
	expectedRunSteps := 0
	for key := range releaseRunSHA256 {
		if strings.HasPrefix(key, jobName+"/") {
			expectedRunSteps++
		}
	}
	if seenRunSteps != expectedRunSteps {
		return fmt.Errorf("release job %s has %d shell steps, expected %d audited steps", jobName, seenRunSteps, expectedRunSteps)
	}
	return nil
}

func equalStringMap(actual, expected map[string]string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for key, want := range expected {
		if actual[key] != want {
			return false
		}
	}
	return true
}

func stepsByID(steps []workflowStep) (map[string]workflowStep, error) {
	result := make(map[string]workflowStep)
	for _, step := range steps {
		if step.ID == "" {
			continue
		}
		if _, exists := result[step.ID]; exists {
			return nil, fmt.Errorf("release workflow contains duplicate step id %q", step.ID)
		}
		result[step.ID] = step
	}
	return result, nil
}

func requireStep(steps map[string]workflowStep, id string) (workflowStep, error) {
	step, ok := steps[id]
	if !ok {
		return workflowStep{}, fmt.Errorf("release workflow is missing step id %q", id)
	}
	return step, nil
}

func value(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	raw, ok := values[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(raw)
}

func verifyAttestationStep(steps map[string]workflowStep, id string, bindings map[string]string, condition string) error {
	step, err := requireStep(steps, id)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(step.Uses, "actions/attest@") || step.If != condition {
		return fmt.Errorf("release attestation step %q has the wrong action or condition", id)
	}
	for key, want := range bindings {
		if value(step.With, key) != want {
			return fmt.Errorf("release attestation step %q has invalid %s binding", id, key)
		}
	}
	return nil
}

func requireRunBindings(steps map[string]workflowStep, id string, bindings ...string) error {
	step, err := requireStep(steps, id)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if !strings.Contains(step.Run, binding) {
			return fmt.Errorf("release step %q is missing binding %q", id, binding)
		}
	}
	return nil
}

func verifyReleaseAssets(manifestPath, checksumsPath, chartPath, tag, sourceSHA string) error {
	manifest, fields, err := parseReleaseManifest(manifestPath, tag, sourceSHA)
	if err != nil {
		return err
	}
	if checksumsPath == "" {
		return nil
	}
	chartInfo, err := os.Lstat(chartPath)
	if err != nil {
		return fmt.Errorf("stat packaged chart: %w", err)
	}
	if !chartInfo.Mode().IsRegular() {
		return errors.New("packaged chart must be a regular file")
	}
	chart, err := os.ReadFile(chartPath)
	if err != nil {
		return fmt.Errorf("read packaged chart: %w", err)
	}
	chartDigest := fmt.Sprintf("%x", sha256.Sum256(chart))
	if filepath.Base(chartPath) != fields["chart-asset"] || chartDigest != fields["chart-asset-sha256"] {
		return errors.New("packaged chart does not match the release manifest")
	}
	checksums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	wantChecksums := fmt.Sprintf("%s  %s\n%x  release-manifest.txt\n",
		chartDigest, fields["chart-asset"], sha256.Sum256(manifest))
	if string(checksums) != wantChecksums {
		return errors.New("SHA256SUMS is not the exact checksum set for the chart and manifest")
	}
	return nil
}

func parseReleaseManifest(path, tag, sourceSHA string) ([]byte, map[string]string, error) {
	if !commitPattern.MatchString(sourceSHA) {
		return nil, nil, fmt.Errorf("source SHA %q is not a full lowercase commit SHA", sourceSHA)
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read release manifest: %w", err)
	}
	if !bytes.HasSuffix(document, []byte("\n")) || bytes.Contains(document, []byte("\r")) {
		return nil, nil, errors.New("release manifest must use newline-terminated LF records")
	}
	wantKeys := []string{
		"version", "source-repository", "source-ref", "source-sha", "transaction",
		"image", "image-tag", "chart-asset", "chart-asset-sha256",
	}
	fields := make(map[string]string, len(wantKeys))
	lines := strings.Split(strings.TrimSuffix(string(document), "\n"), "\n")
	if len(lines) != len(wantKeys) {
		return nil, nil, fmt.Errorf("release manifest has %d records, expected %d", len(lines), len(wantKeys))
	}
	for index, line := range lines {
		key, value, found := strings.Cut(line, "=")
		if !found || key != wantKeys[index] || value == "" || strings.TrimSpace(value) != value {
			return nil, nil, fmt.Errorf("release manifest record %d is invalid", index+1)
		}
		fields[key] = value
	}
	version := strings.TrimPrefix(tag, "v")
	wantExact := map[string]string{
		"version":           version,
		"source-repository": repositoryName,
		"source-ref":        "refs/tags/" + tag,
		"source-sha":        sourceSHA,
		"chart-asset":       "ptah-operator-" + version + ".tgz",
	}
	for key, want := range wantExact {
		if fields[key] != want {
			return nil, nil, fmt.Errorf("release manifest %s is %q, expected %q", key, fields[key], want)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields["chart-asset-sha256"]) {
		return nil, nil, errors.New("release manifest chart asset digest is invalid")
	}
	if !transactionPattern.MatchString(fields["transaction"]) {
		return nil, nil, errors.New("release manifest transaction identity is invalid")
	}
	_, err = exactDigest(fields["image"], imageName)
	if err != nil {
		return nil, nil, err
	}
	wantImageTag := imageName + ":tx-" + sourceSHA + "-" + fields["transaction"]
	if fields["image-tag"] != wantImageTag {
		return nil, nil, fmt.Errorf("release manifest image-tag is %q, expected %q", fields["image-tag"], wantImageTag)
	}
	return document, fields, nil
}

func exactDigest(reference, repository string) (string, error) {
	prefix := repository + "@"
	if !strings.HasPrefix(reference, prefix) || !digestReferencePattern.MatchString(reference) {
		return "", fmt.Errorf("release manifest reference %q is invalid", reference)
	}
	return strings.TrimPrefix(reference, prefix), nil
}
