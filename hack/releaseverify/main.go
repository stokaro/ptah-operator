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
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	semanticVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	digestReferencePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	actionPinPattern       = regexp.MustCompile(`^[^[:space:]@]+@[0-9a-f]{40}$`)
	commitPattern          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	transactionPattern     = regexp.MustCompile(`^[1-9][0-9]*$`)
	sha256DigestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	kubernetesMinorPattern = regexp.MustCompile(`^([1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	dockerArgumentPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	dockerStagePattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
	releaseSequenceHelper  = regexp.MustCompile(`(?m)^\{\{- define "ptah-operator[.]releaseSequence" -\}\}([1-9][0-9]*)\{\{- end -\}\}$`)
	releaseRunSHA256       = map[string]string{
		"smoke/verify-release":               "c91171f73101c06d5d1fdae3f0c4bd405ba7ea6af07e0b76ba38fbb3b1258520",
		"smoke/chart-reproducibility":        "e4dd3906ecd98e9b694aced076f01d981e8dfa6da6e709af486cdb533d76fde7",
		"support-preflight/support-evidence": "e4880ca682553c9ca3f26a9265d23407f3d0ebb04665f32ad5d541550a9e4dcf",
		"publish/release":                    "7d1b4969f5c2d8a9ce63fe54113b2efe9dcea9d35be72bc5dffca2add2858752",
		"publish/transaction":                "72cce0372380ba97e39ae01a383402b50b33122d24854487835b37572fc3c5e7",
		"publish/immutability-preflight":     "08d725a97a83d3a7c16fc1fe7c0e75f8b363a9e5fc43e79482a83996d9b99025",
		"publish/draft":                      "209b2c53dd93d134a098c9d9e6e9e85ee58718ca650399accaa75787d6f475ff",
		"publish/stage-inspect":              "a9bca2e0409204157b32b98595af68f45df5f1110806e2a689fa68b43ab1ddf3",
		"publish/chart-package":              "fcb5ca9057f0307cd27824d1011b12ad1c7b4b5df6b534a505a70da607da37c8",
		"publish/artifacts":                  "d8b898954b7f77f61fd8ecde63414f2e9a423531c5982d40c805d9be8fbde64c",
		"publish/image-structure":            "2d4e40651f9a84ec9f5d394abcec2794958a422eec1e858e49937706813d8b44",
		"publish/finalize-journal":           "0c241512711f0556bd45daf9c57d0e7bfeccb850e6d3db9fecb7431b20ded763",
		"publish/asset-auth":                 "e1c7c1e7eefef128a64a883a73c56dab37d8f1dd24436daa84b7a077896ea8ee",
		"publish/asset-sync":                 "8ac2fce0ed0460b72eee90bb530e1c17da79ed62f4e54877a780bd8dbbc34e4e",
		"publish/image-signature":            "e0b994a90bc38dd8019f4b4157a72e5f6cab1873f3bc1ca39b8ca41dcb023d5e",
		"publish/final-verify":               "1ad635ea3d03dc718ecfff46a020a5bcfef28f5bb2b8932c5d3245e37286d843",
		"publish/publish-release":            "3ff6501266556d3d352e9af3af8b7f2ea79db14ff5f474417b49a9686856de8e",
	}
)

const (
	repositoryName             = "stokaro/ptah-operator"
	imageName                  = "ghcr.io/stokaro/ptah-operator"
	releaseSequenceHistoryPath = "hack/releaseverify/release-sequence-history.json"
	releaseSequenceHelperPath  = "charts/ptah-operator/templates/_helpers.tpl"
	releaseSequenceGoPath      = "internal/crdupgrade/rollout.go"
	kubernetesSupportPath      = "support/kubernetes.json"
	buildxVersion              = "v0.36.1"
	buildkitImage              = "moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8"
	sbomDigest                 = "sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9"
	sbomGenerator              = "docker.io/docker/buildkit-syft-scanner:stable-1@" + sbomDigest
	// releaseWorkflowSHA256 makes every workflow edit an explicit policy edit.
	// Semantic checks below keep the failure actionable; the digest closes gaps
	// where critical shell text could otherwise be hidden in comments or dead branches.
	releaseWorkflowSHA256 = "fb27f9d93cb0bee270e8724386b3141dd4b374664860fa7b63992f25448dcef8"
)

func main() {
	root := flag.String("root", ".", "repository root")
	tag := flag.String("tag", "", "release tag to verify")
	manifest := flag.String("manifest", "", "release manifest to verify")
	journal := flag.String("journal", "", "prepared release journal to verify")
	checksums := flag.String("checksums", "", "SHA256SUMS file to verify")
	chart := flag.String("chart", "", "packaged chart to verify")
	sourceSHA := flag.String("source-sha", "", "release source commit SHA")
	verifyTagIdentity := flag.Bool("verify-tag-identity", false, "verify that the live GitHub tag still peels to GITHUB_SHA")
	printDockerfileInputDigests := flag.Bool(
		"print-dockerfile-input-digests",
		false,
		"print the canonical digest set for every external Dockerfile image input",
	)
	registryMissingError := flag.String(
		"registry-missing-error",
		"",
		"verify that a registry inspection error explicitly reports a missing name or manifest",
	)
	registryMissingReference := flag.String(
		"registry-missing-reference",
		"",
		"exact registry reference that was inspected for a missing manifest",
	)
	provenance := flag.String("provenance", "", "Buildx provenance JSON to verify")
	provenanceSource := flag.String("provenance-source", "", "expected provenance SOURCE build argument")
	provenanceRevision := flag.String("provenance-revision", "", "expected provenance REVISION build argument")
	provenanceVersion := flag.String("provenance-version", "", "expected provenance VERSION build argument")
	flag.Parse()

	if err := verifyRepository(*root, *tag); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *registryMissingError != "" || *registryMissingReference != "" {
		if *registryMissingError == "" || *registryMissingReference == "" {
			fmt.Fprintln(os.Stderr, "registry missing verification requires -registry-missing-error and -registry-missing-reference")
			os.Exit(1)
		}
		if err := verifyRegistryMissingError(*registryMissingError, *registryMissingReference); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *printDockerfileInputDigests {
		digests, err := repositoryDockerfileInputDigests(*root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, digest := range digests {
			fmt.Println(digest)
		}
		return
	}
	if *provenance != "" || *provenanceSource != "" || *provenanceRevision != "" || *provenanceVersion != "" {
		if *provenance == "" || *provenanceSource == "" || *provenanceRevision == "" || *provenanceVersion == "" {
			fmt.Fprintln(os.Stderr, "provenance verification requires -provenance, -provenance-source, -provenance-revision, and -provenance-version")
			os.Exit(1)
		}
		document, err := os.ReadFile(*provenance)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read Buildx provenance:", err)
			os.Exit(1)
		}
		digests, err := repositoryDockerfileInputDigests(*root)
		if err == nil {
			digests = append(digests, sbomDigest)
			sort.Strings(digests)
			err = verifyBuildProvenance(document, *provenanceSource, *provenanceRevision, *provenanceVersion, digests)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
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
	if *manifest != "" || *journal != "" || *checksums != "" || *chart != "" || *sourceSHA != "" {
		if *manifest == "" && *journal == "" || *tag == "" || *sourceSHA == "" {
			fmt.Fprintln(os.Stderr, "release state verification requires -manifest or -journal, plus -tag and -source-sha")
			os.Exit(1)
		}
		if *manifest != "" && *journal != "" {
			fmt.Fprintln(os.Stderr, "-manifest and -journal are mutually exclusive")
			os.Exit(1)
		}
		if (*checksums == "") != (*chart == "") {
			fmt.Fprintln(os.Stderr, "-checksums and -chart must be supplied together")
			os.Exit(1)
		}
		var err error
		if *journal != "" {
			if *checksums != "" || *chart != "" {
				fmt.Fprintln(os.Stderr, "prepared journal verification does not accept -checksums or -chart")
				os.Exit(1)
			}
			err = verifyPreparedJournal(*journal, *tag, *sourceSHA)
		} else {
			err = verifyReleaseAssets(*root, *manifest, *checksums, *chart, *tag, *sourceSHA)
		}
		if err != nil {
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

type kubernetesSupportManifest struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Policy        string                     `json:"policy"`
	WindowSize    int                        `json:"windowSize"`
	LastVerified  string                     `json:"lastVerified"`
	KindVersion   string                     `json:"kindVersion"`
	Releases      []kubernetesSupportRelease `json:"releases"`
}

type kubernetesSupportRelease struct {
	Minor     string `json:"minor"`
	NodeImage string `json:"nodeImage"`
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
	if _, err := repositoryKubernetesSupportWindow(root); err != nil {
		return err
	}
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
	helpers, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(releaseSequenceHelperPath)))
	if err != nil {
		return fmt.Errorf("read release sequence Helm helper: %w", err)
	}
	rolloutSource, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(releaseSequenceGoPath)))
	if err != nil {
		return fmt.Errorf("read release sequence Go contract: %w", err)
	}
	history, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(releaseSequenceHistoryPath)))
	if err != nil {
		return fmt.Errorf("read release sequence history: %w", err)
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
	contract, err := currentReleaseContract(version, appVersion, values, helpers, rolloutSource)
	if err != nil {
		return err
	}
	if err := verifyReleaseSequenceHistory(root, history, contract); err != nil {
		return err
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

func repositoryKubernetesSupportWindow(root string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(kubernetesSupportPath))
	document, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Kubernetes support manifest: %w", err)
	}
	window, err := parseKubernetesSupportWindow(document)
	if err != nil {
		return "", fmt.Errorf("parse Kubernetes support manifest: %w", err)
	}
	return window, nil
}

func parseKubernetesSupportWindow(document []byte) (string, error) {
	var manifest kubernetesSupportManifest
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("trailing JSON value")
		}
		return "", fmt.Errorf("decode trailing JSON: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return "", errors.New("schemaVersion must be 1")
	}
	if manifest.Policy != "upstream-active-minors" {
		return "", errors.New("policy must be upstream-active-minors")
	}
	if manifest.WindowSize != 3 || len(manifest.Releases) != manifest.WindowSize {
		return "", errors.New("windowSize and releases must describe exactly three supported minors")
	}
	if _, err := time.Parse("2006-01-02", manifest.LastVerified); err != nil {
		return "", fmt.Errorf("lastVerified must use YYYY-MM-DD: %w", err)
	}
	if strings.TrimSpace(manifest.KindVersion) != manifest.KindVersion || manifest.KindVersion == "" {
		return "", errors.New("kindVersion must be a nonempty canonical value")
	}

	minors := make([]string, 0, len(manifest.Releases))
	previousMajor, previousMinor := -1, -1
	seenImages := make(map[string]struct{}, len(manifest.Releases))
	for index, release := range manifest.Releases {
		match := kubernetesMinorPattern.FindStringSubmatch(release.Minor)
		if match == nil {
			return "", fmt.Errorf("releases[%d].minor %q must be canonical major.minor", index, release.Minor)
		}
		major, err := strconv.Atoi(match[1])
		if err != nil {
			return "", fmt.Errorf("parse releases[%d] major: %w", index, err)
		}
		minor, err := strconv.Atoi(match[2])
		if err != nil {
			return "", fmt.Errorf("parse releases[%d] minor: %w", index, err)
		}
		if index > 0 && (major != previousMajor || minor != previousMinor+1) {
			return "", errors.New("supported Kubernetes minors must be ordered and consecutive")
		}
		if strings.TrimSpace(release.NodeImage) != release.NodeImage || release.NodeImage == "" {
			return "", fmt.Errorf("releases[%d].nodeImage must be nonempty and canonical", index)
		}
		if _, duplicate := seenImages[release.NodeImage]; duplicate {
			return "", fmt.Errorf("releases[%d].nodeImage duplicates an earlier release", index)
		}
		seenImages[release.NodeImage] = struct{}{}
		minors = append(minors, release.Minor)
		previousMajor, previousMinor = major, minor
	}
	return strings.Join(minors, ","), nil
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

type releaseSequenceHistory struct {
	FormatVersion int               `json:"formatVersion"`
	Releases      []releaseContract `json:"releases"`
}

type releaseContract struct {
	Version                string `json:"version"`
	AppVersion             string `json:"appVersion"`
	ManagerImageRepository string `json:"managerImageRepository"`
	ManagerImageTag        string `json:"managerImageTag"`
	ReleaseSequence        uint64 `json:"releaseSequence"`
}

func currentReleaseContract(
	version, appVersion string,
	values, helpers, rolloutSource []byte,
) (releaseContract, error) {
	repository, err := nestedScalar(values, "image", "repository")
	if err != nil {
		return releaseContract{}, err
	}
	tag, err := nestedScalar(values, "image", "tag")
	if err != nil {
		return releaseContract{}, err
	}
	helperSequence, err := helmReleaseSequence(helpers)
	if err != nil {
		return releaseContract{}, err
	}
	goSequence, err := goReleaseSequence(rolloutSource)
	if err != nil {
		return releaseContract{}, err
	}
	if helperSequence != goSequence {
		return releaseContract{}, fmt.Errorf(
			"Helm release sequence %d does not match Go CurrentReleaseSequence %d",
			helperSequence,
			goSequence,
		)
	}
	return releaseContract{
		Version:                version,
		AppVersion:             appVersion,
		ManagerImageRepository: repository,
		ManagerImageTag:        tag,
		ReleaseSequence:        helperSequence,
	}, nil
}

func helmReleaseSequence(document []byte) (uint64, error) {
	matches := releaseSequenceHelper.FindAllSubmatch(document, -1)
	if len(matches) != 1 {
		return 0, fmt.Errorf(
			"Helm helpers must contain exactly one canonical ptah-operator.releaseSequence definition, found %d",
			len(matches),
		)
	}
	return positiveReleaseSequence(string(matches[0][1]), "Helm release sequence")
}

func goReleaseSequence(document []byte) (uint64, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), releaseSequenceGoPath, document, 0)
	if err != nil {
		return 0, fmt.Errorf("parse Go release sequence contract: %w", err)
	}
	var expressions []ast.Expr
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, rawSpecification := range general.Specs {
			specification, ok := rawSpecification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range specification.Names {
				if name.Name != "CurrentReleaseSequence" {
					continue
				}
				identifier, exactType := specification.Type.(*ast.Ident)
				if !exactType || identifier.Name != "int32" {
					return 0, errors.New("Go CurrentReleaseSequence must have the explicit int32 type")
				}
				switch {
				case len(specification.Values) == len(specification.Names):
					expressions = append(expressions, specification.Values[index])
				case len(specification.Names) == 1 && len(specification.Values) == 1:
					expressions = append(expressions, specification.Values[0])
				default:
					return 0, errors.New("Go CurrentReleaseSequence must use one explicit constant expression")
				}
			}
		}
	}
	if len(expressions) != 1 {
		return 0, fmt.Errorf("Go source must declare CurrentReleaseSequence exactly once, found %d", len(expressions))
	}
	literal, ok := expressions[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.INT {
		return 0, errors.New("Go CurrentReleaseSequence must be a positive exact decimal integer literal")
	}
	sequence, err := positiveReleaseSequence(literal.Value, "Go CurrentReleaseSequence")
	if err != nil {
		return 0, err
	}
	if sequence > uint64(1<<31-1) {
		return 0, errors.New("Go CurrentReleaseSequence exceeds int32")
	}
	return sequence, nil
}

func positiveReleaseSequence(raw, label string) (uint64, error) {
	if raw == "" || raw[0] < '1' || raw[0] > '9' {
		return 0, fmt.Errorf("%s %q is not a positive exact decimal integer", label, raw)
	}
	for _, character := range raw[1:] {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("%s %q is not a positive exact decimal integer", label, raw)
		}
	}
	sequence, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a positive exact decimal integer: %w", label, raw, err)
	}
	return sequence, nil
}

func verifyReleaseSequenceHistory(root string, candidateDocument []byte, candidate releaseContract) error {
	candidateHistory, err := decodeReleaseSequenceHistory("candidate", candidateDocument)
	if err != nil {
		return err
	}
	if err := verifyCandidateReleaseContract(candidateHistory, candidate); err != nil {
		return err
	}

	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve repository root for release sequence history: %w", err)
	}
	baselineCommit, err := selectReleaseSequenceBaseline(absoluteRoot)
	if err != nil {
		return err
	}
	baselineDocument, found, err := releaseSequenceHistoryAt(absoluteRoot, baselineCommit)
	if err != nil {
		return err
	}
	if !found {
		if len(candidateHistory.Releases) != 1 || candidateHistory.Releases[0].ReleaseSequence != 1 {
			return errors.New("initial release sequence history adoption requires exactly one release at sequence 1")
		}
		return nil
	}
	baselineHistory, err := decodeReleaseSequenceHistory("baseline", baselineDocument)
	if err != nil {
		return err
	}
	return verifyReleaseSequenceTransition(baselineHistory, candidateHistory)
}

func verifyCandidateReleaseContract(history releaseSequenceHistory, candidate releaseContract) error {
	if len(history.Releases) == 0 {
		return errors.New("candidate release sequence history has no releases")
	}
	if history.Releases[len(history.Releases)-1] != candidate {
		return fmt.Errorf(
			"current release metadata %#v does not match the final release sequence history entry %#v",
			candidate,
			history.Releases[len(history.Releases)-1],
		)
	}
	return nil
}

func decodeReleaseSequenceHistory(label string, document []byte) (releaseSequenceHistory, error) {
	var history releaseSequenceHistory
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&history); err != nil {
		return releaseSequenceHistory{}, fmt.Errorf("decode %s release sequence history: %w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return releaseSequenceHistory{}, fmt.Errorf("decode %s release sequence history: trailing JSON data", label)
	}
	if history.FormatVersion != 1 {
		return releaseSequenceHistory{}, fmt.Errorf(
			"%s release sequence history formatVersion is %d, expected 1",
			label,
			history.FormatVersion,
		)
	}
	if len(history.Releases) == 0 {
		return releaseSequenceHistory{}, fmt.Errorf("%s release sequence history has no releases", label)
	}
	for index, release := range history.Releases {
		if _, err := parseReleaseVersion(release.Version); err != nil {
			return releaseSequenceHistory{}, fmt.Errorf("%s release sequence history entry %d: %w", label, index, err)
		}
		if release.AppVersion != release.Version {
			return releaseSequenceHistory{}, fmt.Errorf(
				"%s release sequence history entry %d appVersion %q does not match version %q",
				label,
				index,
				release.AppVersion,
				release.Version,
			)
		}
		if release.ManagerImageRepository == "" || strings.ContainsAny(release.ManagerImageRepository, "@\t\r\n ") {
			return releaseSequenceHistory{}, fmt.Errorf(
				"%s release sequence history entry %d has invalid manager image repository %q",
				label,
				index,
				release.ManagerImageRepository,
			)
		}
		if release.ManagerImageTag != release.Version {
			return releaseSequenceHistory{}, fmt.Errorf(
				"%s release sequence history entry %d manager image tag %q does not match version %q",
				label,
				index,
				release.ManagerImageTag,
				release.Version,
			)
		}
		if release.ReleaseSequence == 0 || release.ReleaseSequence > uint64(1<<31-1) {
			return releaseSequenceHistory{}, fmt.Errorf(
				"%s release sequence history entry %d has invalid releaseSequence %d",
				label,
				index,
				release.ReleaseSequence,
			)
		}
		if index == 0 {
			if release.ReleaseSequence != 1 {
				return releaseSequenceHistory{}, fmt.Errorf("%s release sequence history must begin at sequence 1", label)
			}
			continue
		}
		previous := history.Releases[index-1]
		order, err := compareReleaseVersions(previous.Version, release.Version)
		if err != nil {
			return releaseSequenceHistory{}, fmt.Errorf("%s release sequence history entry %d: %w", label, index, err)
		}
		if order >= 0 {
			return releaseSequenceHistory{}, fmt.Errorf(
				"%s release sequence history version %q must strictly follow %q",
				label,
				release.Version,
				previous.Version,
			)
		}
		if release.ReleaseSequence <= previous.ReleaseSequence {
			return releaseSequenceHistory{}, fmt.Errorf(
				"%s release %q sequence %d must strictly increase prior release sequence %d",
				label,
				release.Version,
				release.ReleaseSequence,
				previous.ReleaseSequence,
			)
		}
	}
	canonical, err := canonicalReleaseSequenceHistory(history)
	if err != nil {
		return releaseSequenceHistory{}, fmt.Errorf("encode %s release sequence history: %w", label, err)
	}
	if !bytes.Equal(document, canonical) {
		return releaseSequenceHistory{}, fmt.Errorf("%s release sequence history is not canonical JSON", label)
	}
	return history, nil
}

func canonicalReleaseSequenceHistory(history releaseSequenceHistory) ([]byte, error) {
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(history); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

func verifyReleaseSequenceTransition(baseline, candidate releaseSequenceHistory) error {
	if len(candidate.Releases) < len(baseline.Releases) {
		return errors.New("candidate release sequence history removed published entries")
	}
	for index, published := range baseline.Releases {
		if candidate.Releases[index] != published {
			return fmt.Errorf("candidate release sequence history rewrote published entry %d", index)
		}
	}
	additional := len(candidate.Releases) - len(baseline.Releases)
	if additional > 1 {
		return fmt.Errorf("candidate release sequence history appended %d releases; exactly one release may be prepared at a time", additional)
	}
	if additional == 0 {
		return nil
	}
	published := baseline.Releases[len(baseline.Releases)-1]
	next := candidate.Releases[len(candidate.Releases)-1]
	if next.ReleaseSequence <= published.ReleaseSequence {
		return fmt.Errorf(
			"new release %q sequence %d must strictly increase published release %q sequence %d",
			next.Version,
			next.ReleaseSequence,
			published.Version,
			published.ReleaseSequence,
		)
	}
	return nil
}

type parsedReleaseVersion struct {
	numbers    [3]uint64
	prerelease []string
}

func parseReleaseVersion(raw string) (parsedReleaseVersion, error) {
	if !semanticVersionPattern.MatchString(raw) {
		return parsedReleaseVersion{}, fmt.Errorf("release version %q is not a supported semantic version", raw)
	}
	core, prerelease, hasPrerelease := strings.Cut(raw, "-")
	parts := strings.Split(core, ".")
	var parsed parsedReleaseVersion
	for index, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return parsedReleaseVersion{}, fmt.Errorf("release version %q component %q is invalid: %w", raw, part, err)
		}
		parsed.numbers[index] = value
	}
	if !hasPrerelease {
		return parsed, nil
	}
	parsed.prerelease = strings.Split(prerelease, ".")
	for _, identifier := range parsed.prerelease {
		if releaseVersionNumericIdentifier(identifier) && len(identifier) > 1 && identifier[0] == '0' {
			return parsedReleaseVersion{}, fmt.Errorf("release version %q has a numeric prerelease identifier with a leading zero", raw)
		}
	}
	return parsed, nil
}

func compareReleaseVersions(left, right string) (int, error) {
	leftVersion, err := parseReleaseVersion(left)
	if err != nil {
		return 0, err
	}
	rightVersion, err := parseReleaseVersion(right)
	if err != nil {
		return 0, err
	}
	for index := range leftVersion.numbers {
		switch {
		case leftVersion.numbers[index] < rightVersion.numbers[index]:
			return -1, nil
		case leftVersion.numbers[index] > rightVersion.numbers[index]:
			return 1, nil
		}
	}
	if len(leftVersion.prerelease) == 0 && len(rightVersion.prerelease) == 0 {
		return 0, nil
	}
	if len(leftVersion.prerelease) == 0 {
		return 1, nil
	}
	if len(rightVersion.prerelease) == 0 {
		return -1, nil
	}
	limit := min(len(leftVersion.prerelease), len(rightVersion.prerelease))
	for index := 0; index < limit; index++ {
		leftIdentifier := leftVersion.prerelease[index]
		rightIdentifier := rightVersion.prerelease[index]
		if leftIdentifier == rightIdentifier {
			continue
		}
		leftNumeric := releaseVersionNumericIdentifier(leftIdentifier)
		rightNumeric := releaseVersionNumericIdentifier(rightIdentifier)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftIdentifier) < len(rightIdentifier) ||
				len(leftIdentifier) == len(rightIdentifier) && leftIdentifier < rightIdentifier {
				return -1, nil
			}
			return 1, nil
		case leftNumeric:
			return -1, nil
		case rightNumeric:
			return 1, nil
		case leftIdentifier < rightIdentifier:
			return -1, nil
		default:
			return 1, nil
		}
	}
	switch {
	case len(leftVersion.prerelease) < len(rightVersion.prerelease):
		return -1, nil
	case len(leftVersion.prerelease) > len(rightVersion.prerelease):
		return 1, nil
	default:
		return 0, nil
	}
}

func releaseVersionNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func selectReleaseSequenceBaseline(root string) (string, error) {
	releaseBaseline := strings.TrimSpace(os.Getenv("RELEASE_SEQUENCE_BASELINE_REF"))
	sharedBaseline := strings.TrimSpace(os.Getenv("CRD_SCHEMA_BASELINE_REF"))
	if releaseBaseline != "" && sharedBaseline != "" && releaseBaseline != sharedBaseline {
		return "", errors.New("release sequence and CRD schema history baselines disagree")
	}
	requested := releaseBaseline
	if requested == "" {
		requested = sharedBaseline
	}
	if requested != "" {
		if !commitPattern.MatchString(requested) {
			return "", fmt.Errorf("release sequence history baseline %q is not an exact lowercase Git commit", requested)
		}
		resolved, err := resolveReleaseSequenceCommit(root, requested)
		if err != nil {
			return "", err
		}
		if resolved != requested {
			return "", fmt.Errorf("release sequence history baseline %q resolved to %q", requested, resolved)
		}
		return resolved, nil
	}

	dirty, err := releaseSequenceInputsDirty(root)
	if err != nil {
		return "", err
	}
	if dirty {
		return resolveReleaseSequenceCommit(root, "HEAD")
	}
	commit, err := resolveReleaseSequenceCommit(root, "HEAD^")
	if err == nil {
		return commit, nil
	}
	// Release smoke jobs intentionally use a shallow read-only checkout. Their
	// candidate history is still self-consistent; exact base comparison already
	// ran in the required full-history CI support gate before publication.
	return resolveReleaseSequenceCommit(root, "HEAD")
}

func releaseSequenceInputsDirty(root string) (bool, error) {
	arguments := []string{
		"status", "--porcelain=v1", "--untracked-files=all", "-z", "--",
		"charts/ptah-operator/Chart.yaml",
		"charts/ptah-operator/values.yaml",
		releaseSequenceHelperPath,
		releaseSequenceGoPath,
		releaseSequenceHistoryPath,
	}
	output, err := releaseSequenceGitOutput(root, arguments...)
	if err != nil {
		return false, fmt.Errorf("inspect release sequence inputs: %w", err)
	}
	return len(output) != 0, nil
}

func resolveReleaseSequenceCommit(root, reference string) (string, error) {
	if reference == "" || strings.HasPrefix(reference, "-") || strings.ContainsAny(reference, "\x00\r\n") {
		return "", fmt.Errorf("release sequence Git reference %q is invalid", reference)
	}
	output, err := releaseSequenceGitOutput(root, "rev-parse", "--verify", "--end-of-options", reference+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve release sequence history baseline %q: %w", reference, err)
	}
	commit := strings.TrimSpace(string(output))
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("release sequence Git reference %q resolved to invalid commit %q", reference, commit)
	}
	return commit, nil
}

func releaseSequenceHistoryAt(root, commit string) ([]byte, bool, error) {
	output, err := releaseSequenceGitOutput(
		root,
		"ls-tree", "-z", "--name-only", commit, "--", releaseSequenceHistoryPath,
	)
	if err != nil {
		return nil, false, fmt.Errorf("locate release sequence history at %s: %w", commit, err)
	}
	if len(output) == 0 {
		return nil, false, nil
	}
	if string(output) != releaseSequenceHistoryPath+"\x00" {
		return nil, false, fmt.Errorf("Git returned an unexpected release sequence history path at %s", commit)
	}
	document, err := releaseSequenceGitOutput(root, "show", commit+":"+releaseSequenceHistoryPath)
	if err != nil {
		return nil, false, fmt.Errorf("read release sequence history at %s: %w", commit, err)
	}
	return document, true, nil
}

func releaseSequenceGitOutput(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		message := strings.TrimSpace(string(exitError.Stderr))
		if len(message) > 4096 {
			message = message[:4096] + "..."
		}
		if message != "" {
			return nil, fmt.Errorf("git %s: %s: %w", arguments[0], message, err)
		}
	}
	return nil, fmt.Errorf("git %s: %w", arguments[0], err)
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

type dockerfileInput struct {
	Kind      string
	Reference string
	Line      int
}

type dockerfileInstruction struct {
	Name string
	Args string
	Line int
}

func verifyDockerfile(document []byte, toolchain string) error {
	if _, err := dockerfileExternalInputs(document, toolchain); err != nil {
		return err
	}
	return verifyManagerRevisionBinding(document)
}

func verifyManagerRevisionBinding(document []byte) error {
	instructions, err := dockerfileInstructions(document)
	if err != nil {
		return err
	}
	const managerBuild = `CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.controllerRevision=${REVISION}" -o /out/manager ./cmd/manager`
	stage := -1
	revisionDeclared := false
	managerBuilds := 0
	for _, instruction := range instructions {
		switch instruction.Name {
		case "from":
			stage++
		case "arg":
			if stage == 0 && instruction.Args == "REVISION" {
				revisionDeclared = true
			}
		case "run":
			if !strings.Contains(instruction.Args, "-o /out/manager") {
				continue
			}
			managerBuilds++
			if stage != 0 || !revisionDeclared || instruction.Args != managerBuild {
				return fmt.Errorf("Dockerfile line %d manager build must bind the builder REVISION argument to main.controllerRevision", instruction.Line)
			}
		}
	}
	if managerBuilds != 1 {
		return fmt.Errorf("Dockerfile must contain exactly one revision-bound manager build, found %d", managerBuilds)
	}
	return nil
}

func dockerfileExternalInputs(document []byte, toolchain string) ([]dockerfileInput, error) {
	lines := strings.Split(string(document), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# syntax=") {
		return nil, errors.New("Dockerfile must start with a digest-pinned syntax frontend")
	}
	frontend := strings.TrimSpace(strings.TrimPrefix(lines[0], "# syntax="))
	if !digestReferencePattern.MatchString(frontend) || !strings.HasPrefix(frontend, "docker/dockerfile:") {
		return nil, fmt.Errorf("Dockerfile syntax frontend %q is not digest-pinned", frontend)
	}

	instructions, err := dockerfileInstructions(document)
	if err != nil {
		return nil, err
	}
	arguments := make(map[string]string)
	stageRoots := make([]string, 0, 4)
	stageNames := make(map[string]string)
	inputs := make([]dockerfileInput, 0, 5)
	inputs = append(inputs, dockerfileInput{Kind: "syntax frontend", Reference: frontend, Line: 1})
	var firstStageRoot string
	var finalStageRoot string

	for _, instruction := range instructions {
		switch instruction.Name {
		case "arg":
			name, value, hasValue, err := dockerfileArgument(instruction.Args, arguments)
			if err != nil {
				return nil, fmt.Errorf("Dockerfile line %d: %w", instruction.Line, err)
			}
			if hasValue {
				arguments[name] = value
			} else if _, inherited := arguments[name]; !inherited {
				delete(arguments, name)
			}
		case "from":
			reference, stageName, err := dockerfileFrom(instruction.Args, arguments)
			if err != nil {
				return nil, fmt.Errorf("Dockerfile line %d: %w", instruction.Line, err)
			}
			root := reference
			if inheritedRoot, internal := stageNames[strings.ToLower(reference)]; internal {
				root = inheritedRoot
			} else {
				if !digestReferencePattern.MatchString(reference) {
					return nil, fmt.Errorf("Dockerfile line %d external FROM reference %q is not digest-pinned", instruction.Line, reference)
				}
				inputs = append(inputs, dockerfileInput{Kind: "FROM", Reference: reference, Line: instruction.Line})
			}
			if firstStageRoot == "" {
				firstStageRoot = root
			}
			finalStageRoot = root
			stageRoots = append(stageRoots, root)
			if stageName != "" {
				key := strings.ToLower(stageName)
				if _, duplicate := stageNames[key]; duplicate {
					return nil, fmt.Errorf("Dockerfile line %d repeats stage name %q", instruction.Line, stageName)
				}
				stageNames[key] = root
			}
		case "copy":
			reference, found, err := dockerfileCopyFrom(instruction.Args, arguments)
			if err != nil {
				return nil, fmt.Errorf("Dockerfile line %d: %w", instruction.Line, err)
			}
			if !found || dockerfileInternalStage(reference, stageNames, len(stageRoots)) {
				continue
			}
			if !digestReferencePattern.MatchString(reference) {
				return nil, fmt.Errorf("Dockerfile line %d external COPY --from reference %q is not digest-pinned", instruction.Line, reference)
			}
			inputs = append(inputs, dockerfileInput{Kind: "COPY --from", Reference: reference, Line: instruction.Line})
		case "run":
			references, err := dockerfileRunMountFrom(instruction.Args, arguments)
			if err != nil {
				return nil, fmt.Errorf("Dockerfile line %d: %w", instruction.Line, err)
			}
			for _, reference := range references {
				if dockerfileInternalStage(reference, stageNames, len(stageRoots)) {
					continue
				}
				if !digestReferencePattern.MatchString(reference) {
					return nil, fmt.Errorf("Dockerfile line %d external RUN --mount from reference %q is not digest-pinned", instruction.Line, reference)
				}
				inputs = append(inputs, dockerfileInput{Kind: "RUN --mount from", Reference: reference, Line: instruction.Line})
			}
		}
	}
	if len(stageRoots) < 2 {
		return nil, errors.New("Dockerfile must contain pinned builder and runtime stages")
	}
	wantBuilder := "golang:" + toolchain + "-alpine@"
	if !strings.HasPrefix(firstStageRoot, wantBuilder) {
		return nil, fmt.Errorf("Dockerfile builder %q does not match Go toolchain %s", firstStageRoot, toolchain)
	}
	if !strings.HasPrefix(finalStageRoot, "gcr.io/distroless/static-debian13:nonroot@") {
		return nil, fmt.Errorf("Dockerfile runtime %q is not the expected non-root image", finalStageRoot)
	}
	for _, label := range []string{
		"org.opencontainers.image.source",
		"org.opencontainers.image.revision",
		"org.opencontainers.image.version",
	} {
		if !strings.Contains(string(document), label) {
			return nil, fmt.Errorf("Dockerfile is missing OCI label %s", label)
		}
	}
	return inputs, nil
}

func dockerfileInstructions(document []byte) ([]dockerfileInstruction, error) {
	if bytes.Contains(document, []byte("\r")) {
		return nil, errors.New("Dockerfile must use LF line endings")
	}
	lines := strings.Split(string(document), "\n")
	instructions := make([]dockerfileInstruction, 0, len(lines))
	var logical strings.Builder
	startLine := 0
	continuing := false
	directivesAllowed := true

	flush := func() error {
		text := strings.TrimSpace(logical.String())
		logical.Reset()
		if text == "" {
			return errors.New("Dockerfile contains an empty continued instruction")
		}
		separator := strings.IndexAny(text, " \t")
		name := text
		arguments := ""
		if separator >= 0 {
			name = text[:separator]
			arguments = strings.TrimSpace(text[separator+1:])
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if strings.Contains(arguments, "<<") {
			return fmt.Errorf("Dockerfile line %d uses an unsupported heredoc", startLine)
		}
		instructions = append(instructions, dockerfileInstruction{Name: name, Args: arguments, Line: startLine})
		return nil
	}

	for index, rawLine := range lines {
		lineNumber := index + 1
		leftTrimmed := strings.TrimLeft(rawLine, " \t")
		if !continuing && (leftTrimmed == "" || strings.HasPrefix(leftTrimmed, "#")) {
			comment := strings.TrimSpace(strings.TrimPrefix(leftTrimmed, "#"))
			directive, _, hasValue := strings.Cut(comment, "=")
			if directivesAllowed && hasValue && strings.EqualFold(strings.TrimSpace(directive), "escape") {
				return nil, fmt.Errorf("Dockerfile line %d uses an unsupported escape directive", lineNumber)
			}
			continue
		}
		if continuing && (leftTrimmed == "" || strings.HasPrefix(leftTrimmed, "#")) {
			continue
		}
		directivesAllowed = false
		if !continuing {
			startLine = lineNumber
		} else {
			rawLine = leftTrimmed
		}

		trimmedRight := strings.TrimRight(rawLine, " \t")
		trailingBackslashes := 0
		for cursor := len(trimmedRight) - 1; cursor >= 0 && trimmedRight[cursor] == '\\'; cursor-- {
			trailingBackslashes++
		}
		continues := trailingBackslashes%2 == 1
		if continues && len(trimmedRight) != len(rawLine) {
			return nil, fmt.Errorf("Dockerfile line %d has whitespace after a continuation", lineNumber)
		}
		if continues {
			logical.WriteString(trimmedRight[:len(trimmedRight)-1])
			continuing = true
			continue
		}
		logical.WriteString(rawLine)
		continuing = false
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if continuing {
		return nil, fmt.Errorf("Dockerfile line %d has an unterminated continuation", startLine)
	}
	return instructions, nil
}

func dockerfileArgument(text string, values map[string]string) (string, string, bool, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, " \t") {
		return "", "", false, errors.New("ARG must contain exactly one name or name=value binding")
	}
	name, value, hasValue := strings.Cut(text, "=")
	if !dockerArgumentPattern.MatchString(name) {
		return "", "", false, fmt.Errorf("ARG name %q is invalid", name)
	}
	if !hasValue {
		return name, "", false, nil
	}
	expanded, err := expandDockerArguments(value, values)
	if err != nil {
		return "", "", false, fmt.Errorf("resolve ARG %s: %w", name, err)
	}
	return name, expanded, true, nil
}

func dockerfileFrom(text string, arguments map[string]string) (string, string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", "", errors.New("FROM has no image reference")
	}
	index := 0
	seenPlatform := false
	for index < len(fields) && strings.HasPrefix(fields[index], "--") {
		if !strings.HasPrefix(strings.ToLower(fields[index]), "--platform=") || seenPlatform || len(fields[index]) == len("--platform=") {
			return "", "", fmt.Errorf("FROM option %q is unsupported", fields[index])
		}
		seenPlatform = true
		index++
	}
	if index >= len(fields) {
		return "", "", errors.New("FROM has no image reference")
	}
	reference, err := expandDockerArguments(fields[index], arguments)
	if err != nil {
		return "", "", fmt.Errorf("resolve FROM reference: %w", err)
	}
	if strings.ContainsAny(reference, "\"'`") {
		return "", "", fmt.Errorf("FROM reference %q uses unsupported quoting", reference)
	}
	index++
	stageName := ""
	if index < len(fields) {
		if index+2 != len(fields) || !strings.EqualFold(fields[index], "as") {
			return "", "", errors.New("FROM must contain only an optional AS stage name after its image")
		}
		stageName = fields[index+1]
		if !dockerStagePattern.MatchString(stageName) {
			return "", "", fmt.Errorf("FROM stage name %q is invalid", stageName)
		}
	}
	return reference, stageName, nil
}

func dockerfileCopyFrom(text string, arguments map[string]string) (string, bool, error) {
	fields := strings.Fields(text)
	var reference string
	for _, field := range fields {
		lower := strings.ToLower(field)
		if lower == "--from" {
			return "", false, errors.New("COPY --from must use the unambiguous --from=value form")
		}
		if !strings.HasPrefix(lower, "--from=") {
			continue
		}
		if reference != "" {
			return "", false, errors.New("COPY contains multiple --from references")
		}
		raw := field[len("--from="):]
		if raw == "" {
			return "", false, errors.New("COPY --from reference is empty")
		}
		expanded, err := expandDockerArguments(raw, arguments)
		if err != nil {
			return "", false, fmt.Errorf("resolve COPY --from reference: %w", err)
		}
		reference = expanded
	}
	return reference, reference != "", nil
}

func dockerfileRunMountFrom(text string, arguments map[string]string) ([]string, error) {
	fields := strings.Fields(text)
	references := make([]string, 0, 1)
	for _, field := range fields {
		if !strings.HasPrefix(field, "--") {
			break
		}
		lower := strings.ToLower(field)
		if lower == "--mount" {
			return nil, errors.New("RUN --mount must use the unambiguous --mount=value form")
		}
		if !strings.HasPrefix(lower, "--mount=") {
			continue
		}
		specification := field[len("--mount="):]
		if specification == "" || strings.ContainsAny(specification, "\"'`") {
			return nil, fmt.Errorf("RUN --mount specification %q is unsupported", specification)
		}
		var reference string
		for _, option := range strings.Split(specification, ",") {
			key, raw, found := strings.Cut(option, "=")
			if !found || !strings.EqualFold(key, "from") {
				continue
			}
			if reference != "" || raw == "" {
				return nil, errors.New("RUN --mount must contain at most one non-empty from reference")
			}
			expanded, err := expandDockerArguments(raw, arguments)
			if err != nil {
				return nil, fmt.Errorf("resolve RUN --mount from reference: %w", err)
			}
			reference = expanded
		}
		if reference != "" {
			references = append(references, reference)
		}
	}
	return references, nil
}

func dockerfileInternalStage(reference string, stageNames map[string]string, stageCount int) bool {
	if _, ok := stageNames[strings.ToLower(reference)]; ok {
		return true
	}
	index, err := strconv.Atoi(reference)
	return err == nil && index >= 0 && index < stageCount
}

func expandDockerArguments(text string, values map[string]string) (string, error) {
	var result strings.Builder
	for index := 0; index < len(text); {
		if text[index] != '$' {
			result.WriteByte(text[index])
			index++
			continue
		}
		index++
		if index >= len(text) {
			return "", errors.New("trailing $ is unresolved")
		}
		var name string
		if text[index] == '{' {
			end := strings.IndexByte(text[index+1:], '}')
			if end < 0 {
				return "", errors.New("unterminated ${...} reference")
			}
			end += index + 1
			name = text[index+1 : end]
			index = end + 1
		} else {
			start := index
			for index < len(text) && (text[index] == '_' || text[index] >= '0' && text[index] <= '9' || text[index] >= 'A' && text[index] <= 'Z' || text[index] >= 'a' && text[index] <= 'z') {
				index++
			}
			name = text[start:index]
		}
		if !dockerArgumentPattern.MatchString(name) {
			return "", fmt.Errorf("Docker argument reference %q is unsupported", name)
		}
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("Docker argument %s has no resolved default", name)
		}
		result.WriteString(value)
	}
	return result.String(), nil
}

func repositoryDockerfileInputDigests(root string) ([]string, error) {
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		return nil, fmt.Errorf("read Dockerfile: %w", err)
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}
	toolchain, err := goToolchain(module)
	if err != nil {
		return nil, err
	}
	inputs, err := dockerfileExternalInputs(dockerfile, toolchain)
	if err != nil {
		return nil, err
	}
	unique := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		_, digest, found := strings.Cut(input.Reference, "@")
		if !found || !strings.HasPrefix(digest, "sha256:") {
			return nil, fmt.Errorf("Dockerfile line %d %s input %q has no canonical digest", input.Line, input.Kind, input.Reference)
		}
		unique[digest] = struct{}{}
	}
	digests := make([]string, 0, len(unique))
	for digest := range unique {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests, nil
}

func verifyRegistryMissingError(path, reference string) error {
	document, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read registry inspection error: %w", err)
	}
	if reference == "" || strings.ContainsAny(reference, "\r\n\t ") {
		return fmt.Errorf("registry inspection reference %q is invalid", reference)
	}
	message := strings.TrimSuffix(string(document), "\n")
	if strings.ContainsAny(message, "\r\n") {
		return errors.New("registry inspection error must contain exactly one line")
	}
	prefix := "ERROR: " + reference + ": "
	if !strings.HasPrefix(message, prefix) {
		return fmt.Errorf("registry inspection error is not bound to exact reference %q", reference)
	}
	detail := message[len(prefix):]
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(detail, "_", " "), "-", " "))
	if normalized != "not found" && normalized != "manifest unknown" && normalized != "name unknown" &&
		!strings.HasPrefix(normalized, "manifest unknown: ") && !strings.HasPrefix(normalized, "name unknown: ") {
		return errors.New("registry inspection did not report an exact missing-manifest response")
	}
	return nil
}

type buildxProvenancePlatform struct {
	SLSA struct {
		BuildDefinition buildxBuildDefinition `json:"buildDefinition"`
	} `json:"SLSA"`
}

type buildxBuildDefinition struct {
	ExternalParameters struct {
		Request struct {
			Args map[string]json.RawMessage `json:"args"`
		} `json:"request"`
	} `json:"externalParameters"`
	InternalParameters struct {
		BuildConfig struct {
			LLBDefinition json.RawMessage `json:"llbDefinition"`
		} `json:"buildConfig"`
	} `json:"internalParameters"`
	ResolvedDependencies []buildxResolvedDependency `json:"resolvedDependencies"`
}

type buildxResolvedDependency struct {
	URI    string            `json:"uri"`
	Digest map[string]string `json:"digest"`
}

func verifyBuildProvenance(
	document []byte,
	source, revision, version string,
	expectedDigests []string,
) error {
	if source == "" || !commitPattern.MatchString(revision) || !semanticVersionPattern.MatchString(version) {
		return errors.New("Buildx provenance expectations are invalid")
	}
	if len(expectedDigests) == 0 {
		return errors.New("Buildx provenance requires at least one expected Dockerfile input digest")
	}
	var platforms map[string]buildxProvenancePlatform
	decoder := json.NewDecoder(bytes.NewReader(document))
	if err := decoder.Decode(&platforms); err != nil {
		return fmt.Errorf("parse Buildx provenance: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("parse Buildx provenance: trailing JSON data")
	}
	if len(platforms) != 2 {
		return fmt.Errorf("Buildx provenance has %d platforms, expected 2", len(platforms))
	}
	wantArguments := map[string]string{
		"build-arg:SOURCE":   source,
		"build-arg:REVISION": revision,
		"build-arg:VERSION":  version,
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		entry, ok := platforms[platform]
		if !ok {
			return fmt.Errorf("Buildx provenance is missing platform %s", platform)
		}
		definition := entry.SLSA.BuildDefinition
		for name, want := range wantArguments {
			got, err := rawJSONString(definition.ExternalParameters.Request.Args[name])
			if err != nil || got != want {
				return fmt.Errorf("Buildx provenance platform %s argument %s is not %q", platform, name, want)
			}
		}
		if !nonEmptyJSONCollection(definition.InternalParameters.BuildConfig.LLBDefinition) {
			return fmt.Errorf("Buildx provenance platform %s has no detailed LLB definition", platform)
		}
		materials := make(map[string]struct{})
		for _, dependency := range definition.ResolvedDependencies {
			if dependency.URI == "" {
				return fmt.Errorf("Buildx provenance platform %s contains a resolved dependency without a URI", platform)
			}
			for algorithm, value := range dependency.Digest {
				if !strings.EqualFold(algorithm, "sha256") {
					continue
				}
				digest := "sha256:" + strings.TrimPrefix(strings.ToLower(value), "sha256:")
				if sha256DigestPattern.MatchString(digest) {
					materials[digest] = struct{}{}
				}
			}
		}
		for _, digest := range expectedDigests {
			if !sha256DigestPattern.MatchString(digest) {
				return fmt.Errorf("expected Dockerfile input digest %q is invalid", digest)
			}
			if _, ok := materials[digest]; !ok {
				return fmt.Errorf("Buildx provenance platform %s does not resolve Dockerfile input %s", platform, digest)
			}
		}
	}
	return nil
}

func rawJSONString(document json.RawMessage) (string, error) {
	if len(document) == 0 {
		return "", errors.New("JSON string is missing")
	}
	var value string
	if err := json.Unmarshal(document, &value); err != nil {
		return "", err
	}
	return value, nil
}

func nonEmptyJSONCollection(document json.RawMessage) bool {
	if len(document) == 0 {
		return false
	}
	var value any
	if err := json.Unmarshal(document, &value); err != nil {
		return false
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return false
	}
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
	Name            string             `yaml:"name"`
	If              string             `yaml:"if"`
	Needs           workflowStringList `yaml:"needs"`
	Environment     string             `yaml:"environment"`
	TimeoutMinutes  int                `yaml:"timeout-minutes"`
	Permissions     map[string]string  `yaml:"permissions"`
	Outputs         map[string]string  `yaml:"outputs"`
	ContinueOnError bool               `yaml:"continue-on-error"`
	Steps           []workflowStep     `yaml:"steps"`
}

type workflowStep struct {
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	Env             map[string]string `yaml:"env"`
	With            map[string]any    `yaml:"with"`
	ContinueOnError bool              `yaml:"continue-on-error"`
}

type workflowStringList []string

func (values *workflowStringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var value string
		if err := node.Decode(&value); err != nil {
			return err
		}
		*values = workflowStringList{value}
		return nil
	case yaml.SequenceNode:
		return node.Decode((*[]string)(values))
	default:
		return errors.New("workflow needs must be a string or list")
	}
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
	if len(workflow.On) != 3 {
		return errors.New("release workflow must have only pull_request, workflow_dispatch, and tag push triggers")
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		return errors.New("release workflow must run smoke checks on pull requests")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		return errors.New("release workflow must run smoke checks on manual dispatch")
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
	if len(workflow.Jobs) != 3 {
		return errors.New("release workflow must contain exactly the smoke, support-preflight, and publish jobs")
	}
	smoke, ok := workflow.Jobs["smoke"]
	if !ok {
		return errors.New("release workflow has no smoke job")
	}
	if smoke.If != "github.event_name == 'pull_request' || github.event_name == 'workflow_dispatch'" || len(smoke.Permissions) != 0 {
		return errors.New("smoke job must be read-only and gated to pull requests or manual dispatch")
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
	smokeSteps, err := stepsByID(smoke.Steps)
	if err != nil {
		return err
	}
	if err := requireRunBindings(smokeSteps, "chart-reproducibility",
		"helm lint charts/ptah-operator",
		`--set-string execution.ptahVersion="release-smoke-explicit"`); err != nil {
		return err
	}

	preflight, ok := workflow.Jobs["support-preflight"]
	if !ok {
		return errors.New("release workflow has no support-preflight job")
	}
	if preflight.Name != "Verify release Kubernetes support evidence" {
		return errors.New("support-preflight job name does not match the release contract")
	}
	if preflight.If != "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')" {
		return errors.New("support-preflight job must be gated to v* tag push refs")
	}
	if preflight.Environment != "" || len(preflight.Needs) != 0 {
		return errors.New("support-preflight job must run before and outside the protected release environment")
	}
	if preflight.TimeoutMinutes != 150 {
		return errors.New("support-preflight timeout must bound the exact-SHA CI wait to 150 minutes")
	}
	if !equalStringMap(preflight.Permissions, map[string]string{"actions": "read", "contents": "read"}) {
		return errors.New("support-preflight permissions must be actions: read and contents: read")
	}
	if !equalStringMap(preflight.Outputs, map[string]string{
		"chart-sha256":              "${{ steps.support-evidence.outputs.chart-sha256 }}",
		"kubernetes-support-window": "${{ steps.support-evidence.outputs.kubernetes-support-window }}",
		"source-sha":                "${{ steps.support-evidence.outputs.source-sha }}",
		"support-evidence-run-id":   "${{ steps.support-evidence.outputs.support-evidence-run-id }}",
	}) {
		return errors.New("support-preflight must expose only its verified chart, source, run, and Kubernetes window evidence")
	}
	if err := verifyStepContract("support-preflight", preflight.Steps,
		[]string{"checkout", "setup-go", "support-evidence"},
		map[string]string{
			"checkout": "actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
			"setup-go": "actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
		}); err != nil {
		return err
	}
	preflightSteps, err := stepsByID(preflight.Steps)
	if err != nil {
		return err
	}
	preflightCheckout, err := requireStep(preflightSteps, "checkout")
	if err != nil {
		return err
	}
	if value(preflightCheckout.With, "persist-credentials") != "false" {
		return errors.New("support-preflight checkout must not persist credentials")
	}
	preflightGo, err := requireStep(preflightSteps, "setup-go")
	if err != nil {
		return err
	}
	if value(preflightGo.With, "go-version-file") != "go.mod" || value(preflightGo.With, "cache") != "false" ||
		value(preflightGo.With, "cache-dependency-path") != "" {
		return errors.New("support-preflight Go setup must use go.mod with action caching disabled")
	}
	supportEvidence, err := requireStep(preflightSteps, "support-evidence")
	if err != nil {
		return err
	}
	if !equalStringMap(supportEvidence.Env, map[string]string{
		"DEFAULT_BRANCH":               "${{ github.event.repository.default_branch }}",
		"GH_TOKEN":                     "${{ secrets.GITHUB_TOKEN }}",
		"SUPPORT_POLL_TIMEOUT_MINUTES": "140",
	}) {
		return errors.New("support-preflight evidence must bind the default branch, Actions token, and 140-minute poll")
	}
	if err := requireRunBindings(preflightSteps, "support-evidence",
		"go run ./hack/verify-kubernetes-support.go -now \"$today\"",
		"go run ./hack/releaseverify",
		"repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs",
		"-f branch=\"$DEFAULT_BRANCH\"", "-f event=push", "-f head_sha=\"$GITHUB_SHA\"",
		".event == \"push\"", ".head_branch == $branch", ".head_sha == $sha",
		".status == \"completed\"", ".conclusion == \"success\"",
		"repos/$GITHUB_REPOSITORY/actions/runs/$run_id/jobs",
		".name == \"Kubernetes support gate\"",
		"poll_deadline_epoch=$(( $(date -u +%s) + SUPPORT_POLL_TIMEOUT_MINUTES * 60 ))",
		"remaining_seconds=$((poll_deadline_epoch - $(date -u +%s)))",
		"if (( remaining_seconds <= 0 ))", "sleep \"$sleep_seconds\"",
		"delay=$((delay < 30 ? delay * 2 : 30))",
		"support_matrix=\"$(go run ./hack/verify-kubernetes-support.go -output=matrix)\"",
		"kubernetes_support_window=\"$(jq -er '[.[].minor] | join(\",\")' <<<\"$support_matrix\")\"",
		"(.minor_slug == (.minor | gsub(\"\\\\.\"; \"-\")))",
		"repos/$GITHUB_REPOSITORY/actions/runs/$evidence_run/artifacts",
		".total_count == $expected_count", ".expired == false", ".size_in_bytes > 0",
		"gh run download \"$evidence_run\"", "--name \"installed-release-chart-$minor_slug\"",
		"[[ -f \"$chart_path\" && ! -L \"$chart_path\" ]]",
		"cmp \"$canonical_chart\" \"$chart_path\"",
		"chart_sha256=\"$(sha256sum \"$canonical_chart\" | awk '{print $1}')\"",
		"[[ \"$chart_sha256\" =~ ^[0-9a-f]{64}$ ]]",
		"[[ \"$evidence_run\" =~ ^[1-9][0-9]*$ ]]",
		"printf 'chart-sha256=%s\\n' \"$chart_sha256\"",
		"printf 'kubernetes-support-window=%s\\n' \"$kubernetes_support_window\"",
		"printf 'source-sha=%s\\n' \"$GITHUB_SHA\"",
		"printf 'support-evidence-run-id=%s\\n' \"$evidence_run\""); err != nil {
		return err
	}

	publish, ok := workflow.Jobs["publish"]
	if !ok {
		return errors.New("release workflow has no publish job")
	}
	if !equalStringSet(publish.Needs, []string{"support-preflight"}) {
		return errors.New("publish job must depend only on support-preflight")
	}
	if publish.If != "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v') && needs.support-preflight.outputs.source-sha == github.sha" {
		return errors.New("publish job must bind successful support-preflight evidence to the tag SHA")
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
			"checkout", "setup-go", "setup-buildx", "release", "immutability-preflight", "transaction",
			"journal-attestation", "draft", "stage-inspect", "registry-login", "image", "build-checkpoint",
			"chart-package", "artifacts", "image-structure", "asset-attestation", "finalize-journal", "asset-auth", "asset-sync",
			"image-attestation", "setup-cosign", "image-signature", "final-verify", "publish-release",
		},
		map[string]string{
			"checkout":            "actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
			"setup-go":            "actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
			"setup-buildx":        "docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e",
			"registry-login":      "docker/login-action@dbcb813823bdd20940b903addbd779551569679f",
			"image":               "docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a",
			"journal-attestation": "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
			"build-checkpoint":    "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
			"asset-attestation":   "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
			"image-attestation":   "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
			"setup-cosign":        "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6",
		}); err != nil {
		return err
	}
	for jobName, job := range workflow.Jobs {
		if job.ContinueOnError {
			return fmt.Errorf("release job %s must not continue on error", jobName)
		}
		for _, step := range job.Steps {
			if step.ContinueOnError {
				return fmt.Errorf("release step %q in job %s must not continue on error", step.ID, jobName)
			}
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
	chartPackage, err := requireStep(steps, "chart-package")
	if err != nil {
		return err
	}
	if !equalStringMap(chartPackage.Env, map[string]string{
		"TESTED_CHART_SHA256": "${{ needs.support-preflight.outputs.chart-sha256 }}",
	}) {
		return errors.New("chart package must bind only the support-tested chart digest")
	}
	if err := requireRunBindings(steps, "chart-package",
		"chart_sha256=\"$(sha256sum \"$chart_path\" | awk '{print $1}')\"",
		"[[ \"$TESTED_CHART_SHA256\" =~ ^[0-9a-f]{64}$ ]]",
		"[[ \"$chart_sha256\" == \"$TESTED_CHART_SHA256\" ]]"); err != nil {
		return err
	}
	artifacts, err := requireStep(steps, "artifacts")
	if err != nil {
		return err
	}
	if !equalStringMap(artifacts.Env, map[string]string{
		"TESTED_KUBERNETES_SUPPORT_WINDOW": "${{ needs.support-preflight.outputs.kubernetes-support-window }}",
		"TESTED_SUPPORT_EVIDENCE_RUN_ID":   "${{ needs.support-preflight.outputs.support-evidence-run-id }}",
	}) {
		return errors.New("release artifacts must bind only the support evidence run and Kubernetes window")
	}
	if err := requireRunBindings(steps, "artifacts",
		"[[ \"$TESTED_SUPPORT_EVIDENCE_RUN_ID\" =~ ^[1-9][0-9]*$ ]]",
		"[[ \"$TESTED_KUBERNETES_SUPPORT_WINDOW\" =~ ^[0-9]+\\.[0-9]+(,[0-9]+\\.[0-9]+)*$ ]]",
		"printf 'support-evidence-run-id=%s\\n' \"$TESTED_SUPPORT_EVIDENCE_RUN_ID\"",
		"printf 'kubernetes-support-window=%s\\n' \"$TESTED_KUBERNETES_SUPPORT_WINDOW\""); err != nil {
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
		value(image.With, "sbom") != "generator="+sbomGenerator ||
		value(image.With, "tags") != "${{ steps.transaction.outputs.image-tag }}" ||
		value(image.With, "build-args") != "VERSION=${{ steps.release.outputs.version }}\nREVISION=${{ github.sha }}\nSOURCE=https://github.com/${{ github.repository }}\n" {
		return errors.New("publish image step must push the staged multi-architecture SBOM/provenance build")
	}
	if image.If != "(steps.transaction.outputs.mode == 'fresh' || steps.transaction.outputs.mode == 'prepared') && steps.stage-inspect.outputs.reuse != 'true'" {
		return errors.New("publish image step must build only a missing prepared transaction image")
	}
	for id, condition := range map[string]string{
		"registry-login":   "steps.transaction.outputs.mode != 'published'",
		"draft":            "steps.transaction.outputs.mode == 'fresh'",
		"asset-sync":       "steps.transaction.outputs.mode != 'published'",
		"image-signature":  "steps.transaction.outputs.mode != 'published'",
		"stage-inspect":    "steps.transaction.outputs.mode == 'fresh' || steps.transaction.outputs.mode == 'prepared'",
		"finalize-journal": "steps.transaction.outputs.mode == 'fresh' || steps.transaction.outputs.mode == 'prepared'",
	} {
		step, err := requireStep(steps, id)
		if err != nil {
			return err
		}
		if step.If != condition {
			return fmt.Errorf("release step %q must use condition %q", id, condition)
		}
	}
	if err := verifyAttestationStep(steps, "journal-attestation", map[string]string{
		"subject-path": "dist/release-journal.txt",
	}, "steps.transaction.outputs.mode == 'fresh'"); err != nil {
		return err
	}
	if err := verifyAttestationStep(steps, "build-checkpoint", map[string]string{
		"subject-name":     "${{ env.IMAGE }}",
		"subject-digest":   "${{ steps.image.outputs.digest }}",
		"push-to-registry": "true",
	}, "steps.image.outcome == 'success' && steps.image.outputs.digest != ''"); err != nil {
		return err
	}

	if err := verifyAttestationStep(steps, "asset-attestation", map[string]string{
		"subject-path": "${{ steps.chart-package.outputs.path }}\ndist/release-manifest.txt\ndist/SHA256SUMS\n",
	}, "steps.transaction.outputs.mode == 'fresh' || steps.transaction.outputs.mode == 'prepared'"); err != nil {
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
		"release_state=prepared",
		"transaction=\"$GITHUB_RUN_ID\"",
		"-journal dist/release-journal.txt",
		"-verify-tag-identity",
		".assets | length == 0",
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
		"--notes-file dist/release-journal.txt", "gh attestation verify dist/release-journal.txt",
		"-verify-tag-identity", ".assets | length == 0"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "stage-inspect",
		"imagetools inspect --raw", "steps.transaction.outputs.image-tag", "reuse=true",
		"reuse=false", "refusing to rebuild", "gh attestation verify \"oci://$IMAGE@$digest\"",
		"--source-ref \"$GITHUB_REF\"", "--source-digest \"$GITHUB_SHA\"",
		"--signer-workflow \"$GITHUB_REPOSITORY/.github/workflows/release.yml\"",
		"-registry-missing-error \"$error_file\"", "-registry-missing-reference \"$reference\""); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "image-structure",
		"imagetools inspect --raw \"$reference\"", "steps.artifacts.outputs.image-tag",
		"cmp \"$image_dir/index.json\" \"$image_dir/tag-index.json\"",
		"[\"linux/amd64\", \"linux/arm64\"]", "vnd.docker.reference.digest",
		"https://spdx.dev/Document", "https://slsa.dev/provenance/v1",
		"org.opencontainers.image.source", "org.opencontainers.image.revision",
		"org.opencontainers.image.version", "{{json .Provenance}}",
		"-provenance \"$image_dir/provenance.json\"", "-provenance-source \"$source\"",
		"-provenance-revision \"$GITHUB_SHA\"", "-provenance-version \"$version\""); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "finalize-journal",
		"cmp dist/release-journal.txt", "gh release edit", "--notes-file dist/release-manifest.txt",
		"gh attestation verify dist/release-manifest.txt", "cmp dist/release-manifest.txt",
		"-verify-tag-identity", ".assets | length == 0"); err != nil {
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
		"cmp \"$image_dir/index.json\" \"$image_dir/final-index.json\"",
		"cmp \"$image_dir/final-index.json\" \"$image_dir/final-tag-index.json\""); err != nil {
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

func equalStringSet(actual workflowStringList, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	seen := make(map[string]struct{}, len(actual))
	for _, value := range actual {
		seen[value] = struct{}{}
	}
	if len(seen) != len(actual) {
		return false
	}
	for _, value := range expected {
		if _, ok := seen[value]; !ok {
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

func verifyReleaseAssets(root, manifestPath, checksumsPath, chartPath, tag, sourceSHA string) error {
	supportWindow, err := repositoryKubernetesSupportWindow(root)
	if err != nil {
		return err
	}
	manifest, fields, err := parseReleaseManifest(manifestPath, tag, sourceSHA, supportWindow)
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

func verifyPreparedJournal(path, tag, sourceSHA string) error {
	if !commitPattern.MatchString(sourceSHA) {
		return fmt.Errorf("source SHA %q is not a full lowercase commit SHA", sourceSHA)
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read prepared release journal: %w", err)
	}
	if !bytes.HasSuffix(document, []byte("\n")) || bytes.Contains(document, []byte("\r")) {
		return errors.New("prepared release journal must use newline-terminated LF records")
	}
	wantKeys := []string{
		"state", "version", "source-repository", "source-ref", "source-sha",
		"transaction", "image-tag", "chart-asset",
	}
	fields, err := exactRecords(document, wantKeys, "prepared release journal")
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(tag, "v")
	wantExact := map[string]string{
		"state":             "prepared",
		"version":           version,
		"source-repository": repositoryName,
		"source-ref":        "refs/tags/" + tag,
		"source-sha":        sourceSHA,
		"chart-asset":       "ptah-operator-" + version + ".tgz",
	}
	for key, want := range wantExact {
		if fields[key] != want {
			return fmt.Errorf("prepared release journal %s is %q, expected %q", key, fields[key], want)
		}
	}
	if !transactionPattern.MatchString(fields["transaction"]) {
		return errors.New("prepared release journal transaction identity is invalid")
	}
	wantImageTag := imageName + ":tx-" + sourceSHA + "-" + fields["transaction"]
	if fields["image-tag"] != wantImageTag {
		return fmt.Errorf("prepared release journal image-tag is %q, expected %q", fields["image-tag"], wantImageTag)
	}
	return nil
}

func parseReleaseManifest(path, tag, sourceSHA, supportWindow string) ([]byte, map[string]string, error) {
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
		"support-evidence-run-id", "kubernetes-support-window",
	}
	fields, err := exactRecords(document, wantKeys, "release manifest")
	if err != nil {
		return nil, nil, err
	}
	version := strings.TrimPrefix(tag, "v")
	wantExact := map[string]string{
		"version":                   version,
		"source-repository":         repositoryName,
		"source-ref":                "refs/tags/" + tag,
		"source-sha":                sourceSHA,
		"chart-asset":               "ptah-operator-" + version + ".tgz",
		"kubernetes-support-window": supportWindow,
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
	if !transactionPattern.MatchString(fields["support-evidence-run-id"]) {
		return nil, nil, errors.New("release manifest support evidence run identity is invalid")
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

func exactRecords(document []byte, wantKeys []string, kind string) (map[string]string, error) {
	fields := make(map[string]string, len(wantKeys))
	lines := strings.Split(strings.TrimSuffix(string(document), "\n"), "\n")
	if len(lines) != len(wantKeys) {
		return nil, fmt.Errorf("%s has %d records, expected %d", kind, len(lines), len(wantKeys))
	}
	for index, line := range lines {
		key, value, found := strings.Cut(line, "=")
		if !found || key != wantKeys[index] || value == "" || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("%s record %d is invalid", kind, index+1)
		}
		fields[key] = value
	}
	return fields, nil
}

func exactDigest(reference, repository string) (string, error) {
	prefix := repository + "@"
	if !strings.HasPrefix(reference, prefix) || !digestReferencePattern.MatchString(reference) {
		return "", fmt.Errorf("release manifest reference %q is invalid", reference)
	}
	return strings.TrimPrefix(reference, prefix), nil
}
