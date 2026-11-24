// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command updatekubernetessupport performs the live half of Kubernetes support
// maintenance. It reads the official stable release and kind release metadata,
// then updates the repository's deterministic support manifest and its derived
// documentation and Helm range. When it moves kind, it also reads the load
// balancer image the new kind version names and pins it by the index digest its
// registry serves.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultStableURL       = "https://dl.k8s.io/release/stable.txt"
	defaultKindReleasesURL = "https://api.github.com/repos/kubernetes-sigs/kind/releases?per_page=100"
	defaultKindSourceURL   = "https://raw.githubusercontent.com/kubernetes-sigs/kind"
	// kindLoadBalancerSource is where kind declares the image it runs as the
	// load balancer of a cluster with more than one control plane.
	kindLoadBalancerSource = "pkg/cluster/internal/loadbalancer/const.go"
	dockerHubRegistry      = "docker.io"
	dockerHubEndpoint      = "https://registry-1.docker.io"
	manifestRelativePath   = "support/kubernetes.json"
	chartRelativePath      = "charts/ptah-operator/Chart.yaml"
	docsRelativePath       = "docs/site/src/content/docs/support/kubernetes.md"
	windowSize             = 3
	verificationRefreshAge = 21 * 24 * time.Hour
	maximumResponseBytes   = 16 << 20
	requestTimeout         = 30 * time.Second
	docsBeginMarker        = "<!-- BEGIN GENERATED KUBERNETES SUPPORT -->"
	docsEndMarker          = "<!-- END GENERATED KUBERNETES SUPPORT -->"
)

var (
	stableVersionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	kindImagePattern     = regexp.MustCompile(`kindest/node:v(\d+)\.(\d+)\.(\d+)@sha256:([0-9a-f]{64})`)
	chartRangePattern    = regexp.MustCompile(`(?m)^kubeVersion:\s*"[^"]+"\s*$`)
	// imageReferencePattern is the registry, repository and tag the manifest's
	// kindLoadBalancer pin carries ahead of its digest, in the grammar
	// hack/verify-kubernetes-support.go holds the pin to.
	imageReferencePattern = regexp.MustCompile(`^([a-z0-9-]+(?:\.[a-z0-9-]+)+(?::[0-9]+)?)/([a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*):([A-Za-z0-9_][A-Za-z0-9_.-]{0,127})$`)
	challengePattern      = regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`)
)

// indexMediaTypes are the media types a registry serves a multi-platform image
// as.
var indexMediaTypes = map[string]bool{
	"application/vnd.oci.image.index.v1+json":                   true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

// manifestAccept asks for an index and admits a single-platform manifest, so a
// tag that names one is refused for what it is rather than as missing.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// requiredPlatforms are the platforms the acceptance harness runs kind on: the
// CI runners and the arm64 lab hosts.
var requiredPlatforms = []string{"linux/amd64", "linux/arm64"}

type supportManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Policy        string `json:"policy"`
	WindowSize    int    `json:"windowSize"`
	LastVerified  string `json:"lastVerified"`
	KindVersion   string `json:"kindVersion"`
	// KindLoadBalancer is re-pinned by refreshKindLoadBalancer whenever the
	// plan's kindVersion is not the version the pin was read from, and is
	// carried unchanged otherwise.
	KindLoadBalancer kindLoadBalancerPin `json:"kindLoadBalancer"`
	Releases         []release           `json:"releases"`
}

type kindLoadBalancerPin struct {
	KindVersion string `json:"kindVersion"`
	Image       string `json:"image"`
}

type release struct {
	Minor     string `json:"minor"`
	NodeImage string `json:"nodeImage"`
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Body       string `json:"body"`
}

type semanticVersion struct {
	major int
	minor int
	patch int
}

type imageCandidate struct {
	version semanticVersion
	image   string
}

type updatePlan struct {
	manifest supportManifest
	oldest   string
	newest   string
}

// options is what one update reads from and writes to.
type options struct {
	repositoryRoot  string
	stableURL       string
	kindReleasesURL string
	githubToken     string
	date            time.Time
	client          *http.Client
	loadBalancer    loadBalancerResolver
}

func main() {
	repositoryRoot := flag.String("repository-root", ".", "repository root to update")
	stableURL := flag.String("stable-url", defaultStableURL, "official Kubernetes stable release URL")
	kindReleasesURL := flag.String("kind-releases-url", defaultKindReleasesURL, "official kind GitHub releases API URL")
	kindSourceURL := flag.String("kind-source-url", defaultKindSourceURL,
		"raw file root of the kind repository; the load balancer image is read from <url>/<kind tag>/"+kindLoadBalancerSource)
	dateValue := flag.String("date", "", "verification date in UTC (YYYY-MM-DD; defaults to today)")
	flag.Parse()

	verificationDate, err := parseDate(*dateValue)
	if err != nil {
		fatal(err)
	}
	client := &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: secureRedirectPolicy,
	}
	message, err := run(context.Background(), options{
		repositoryRoot:  *repositoryRoot,
		stableURL:       *stableURL,
		kindReleasesURL: *kindReleasesURL,
		githubToken:     os.Getenv("GITHUB_TOKEN"),
		date:            verificationDate,
		client:          client,
		loadBalancer: loadBalancerResolver{
			client:           client,
			sourceURL:        *kindSourceURL,
			registryEndpoint: registryEndpoint,
		},
	})
	if err != nil {
		fatal(err)
	}
	fmt.Println(message)
}

// run reads everything an update needs before it writes anything, so a read
// that fails leaves the repository as it was rather than half updated.
func run(ctx context.Context, options options) (string, error) {
	manifestPath := filepath.Join(options.repositoryRoot, manifestRelativePath)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return "", err
	}

	stableContents, err := fetch(ctx, options.client, options.stableURL, "")
	if err != nil {
		return "", fmt.Errorf("read official Kubernetes stable release: %w", err)
	}
	kindContents, err := fetch(ctx, options.client, options.kindReleasesURL, options.githubToken)
	if err != nil {
		return "", fmt.Errorf("read official kind releases: %w", err)
	}

	plan, err := buildUpdatePlan(manifest, stableContents, kindContents, options.date)
	if err != nil {
		return "", err
	}
	plan, err = refreshKindLoadBalancer(ctx, plan, options.loadBalancer)
	if err != nil {
		return "", err
	}
	changed, err := applyUpdate(options.repositoryRoot, plan)
	if err != nil {
		return "", err
	}
	if changed {
		return fmt.Sprintf("Updated Kubernetes support window to %s-%s with kind %s", plan.oldest, plan.newest, plan.manifest.KindVersion), nil
	}
	if len(plan.manifest.Releases) == 0 {
		return "", errors.New("support update unexpectedly produced no releases")
	}
	return fmt.Sprintf("Kubernetes support window is current at %s-%s with kind %s", plan.oldest, plan.newest, plan.manifest.KindVersion), nil
}

func parseDate(value string) (time.Time, error) {
	if value == "" {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("-date must use YYYY-MM-DD: %w", err)
	}
	return parsed, nil
}

func fetch(ctx context.Context, client *http.Client, rawURL, token string) ([]byte, error) {
	header := http.Header{}
	header.Set("Accept", "application/vnd.github+json, text/plain")
	answer, err := get(ctx, client, rawURL, header, func(parsedURL *url.URL, request *http.Request) {
		if strings.EqualFold(parsedURL.Hostname(), "api.github.com") {
			request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if answer.status != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected HTTP status %s", answer.url, answer.statusText)
	}
	return answer.body, nil
}

// reply is a bounded response, read in full.
type reply struct {
	url        string
	status     int
	statusText string
	header     http.Header
	body       []byte
}

// get issues one HTTPS GET and reads at most maximumResponseBytes of the
// answer, whatever its status. prepare, when set, adds the headers that depend
// on the parsed URL.
func get(ctx context.Context, client *http.Client, rawURL string, header http.Header, prepare func(*url.URL, *http.Request)) (reply, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return reply{}, fmt.Errorf("parse URL: %w", err)
	}
	if parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return reply{}, fmt.Errorf("URL must be an absolute HTTPS URL")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return reply{}, fmt.Errorf("build request: %w", err)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	request.Header.Set("User-Agent", "ptah-operator-kubernetes-support-updater")
	if prepare != nil {
		prepare(parsedURL, request)
	}

	response, err := client.Do(request)
	if err != nil {
		return reply{}, fmt.Errorf("GET %s: %w", parsedURL.Redacted(), err)
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maximumResponseBytes+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return reply{}, fmt.Errorf("read %s: %w", parsedURL.Redacted(), err)
	}
	if len(contents) > maximumResponseBytes {
		return reply{}, fmt.Errorf("GET %s: response exceeds %d bytes", parsedURL.Redacted(), maximumResponseBytes)
	}
	return reply{
		url:        parsedURL.Redacted(),
		status:     response.StatusCode,
		statusText: response.Status,
		header:     response.Header,
		body:       contents,
	}, nil
}

func secureRedirectPolicy(request *http.Request, via []*http.Request) error {
	if request.URL.Scheme != "https" {
		return fmt.Errorf("refuse redirect to non-HTTPS URL %s", request.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

func readManifest(path string) (supportManifest, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return supportManifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	var manifest supportManifest
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return supportManifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return supportManifest{}, fmt.Errorf("decode %s: trailing JSON value", path)
		}
		return supportManifest{}, fmt.Errorf("decode %s after first JSON value: %w", path, err)
	}
	if manifest.SchemaVersion != 1 || manifest.Policy != "upstream-active-minors" || manifest.WindowSize != windowSize {
		return supportManifest{}, fmt.Errorf("%s: unsupported support manifest policy", path)
	}
	return manifest, nil
}

func buildUpdatePlan(current supportManifest, stableContents, kindContents []byte, verificationDate time.Time) (updatePlan, error) {
	stableVersion, err := parseVersion(strings.TrimSpace(string(stableContents)))
	if err != nil {
		return updatePlan{}, fmt.Errorf("parse official Kubernetes stable release: %w", err)
	}
	if stableVersion.minor < windowSize-1 {
		return updatePlan{}, fmt.Errorf("stable Kubernetes version has no complete %d-minor window", windowSize)
	}

	targets := make([]string, 0, windowSize)
	for offset := windowSize - 1; offset >= 0; offset-- {
		targets = append(targets, fmt.Sprintf("%d.%d", stableVersion.major, stableVersion.minor-offset))
	}

	var published []githubRelease
	decoder := json.NewDecoder(strings.NewReader(string(kindContents)))
	if err := decoder.Decode(&published); err != nil {
		return updatePlan{}, fmt.Errorf("decode official kind releases: %w", err)
	}
	if len(published) == 0 {
		return updatePlan{}, errors.New("official kind releases response is empty")
	}

	kindVersion, images, err := selectKindRelease(published, targets)
	if err != nil {
		return updatePlan{}, err
	}

	next := current
	next.KindVersion = kindVersion
	next.Releases = make([]release, 0, len(targets))
	for _, minor := range targets {
		next.Releases = append(next.Releases, release{Minor: minor, NodeImage: images[minor].image})
	}
	lastVerified, err := time.Parse("2006-01-02", current.LastVerified)
	if err != nil {
		return updatePlan{}, fmt.Errorf("parse current lastVerified: %w", err)
	}
	verificationAge := verificationDate.Sub(lastVerified)
	if verificationAge < 0 {
		return updatePlan{}, fmt.Errorf("current lastVerified %s is after update date %s", current.LastVerified, verificationDate.Format("2006-01-02"))
	}
	if !sameSupportBundle(current, next) || verificationAge >= verificationRefreshAge {
		next.LastVerified = verificationDate.Format("2006-01-02")
	}
	return updatePlan{manifest: next, oldest: targets[0], newest: targets[len(targets)-1]}, nil
}

// refreshKindLoadBalancer re-pins the load balancer image when the plan's kind
// version is not the one the pin was read from, and reads nothing when it is.
// A read that fails refuses the plan: a proposal that moved kind and kept the
// previous kind's image would fail its own verification.
func refreshKindLoadBalancer(ctx context.Context, plan updatePlan, resolver loadBalancerResolver) (updatePlan, error) {
	kindVersion := plan.manifest.KindVersion
	if plan.manifest.KindLoadBalancer.KindVersion == kindVersion {
		return plan, nil
	}
	pin, err := resolver.resolve(ctx, kindVersion)
	if err != nil {
		return updatePlan{}, fmt.Errorf("%s kindLoadBalancer: cannot pin the load balancer image of kind %s: %w",
			manifestRelativePath, kindVersion, err)
	}
	plan.manifest.KindLoadBalancer = pin
	return plan, nil
}

// loadBalancerResolver reads the load balancer image a kind version names and
// the index digest its registry serves for it.
type loadBalancerResolver struct {
	client *http.Client
	// sourceURL is the raw file root of the kind repository.
	sourceURL string
	// registryEndpoint maps a registry host to the root of its HTTP API.
	registryEndpoint func(registry string) string
}

func (r loadBalancerResolver) resolve(ctx context.Context, kindTag string) (kindLoadBalancerPin, error) {
	sourceURL, err := url.JoinPath(r.sourceURL, kindTag, kindLoadBalancerSource)
	if err != nil {
		return kindLoadBalancerPin{}, fmt.Errorf("build the %s URL: %w", kindLoadBalancerSource, err)
	}
	source, err := fetch(ctx, r.client, sourceURL, "")
	if err != nil {
		return kindLoadBalancerPin{}, fmt.Errorf("read %s at %s: %w", kindLoadBalancerSource, kindTag, err)
	}
	reference, err := loadBalancerImageFromSource(source)
	if err != nil {
		return kindLoadBalancerPin{}, fmt.Errorf("%s at %s: %w", kindLoadBalancerSource, kindTag, err)
	}
	digest, err := r.indexDigest(ctx, reference)
	if err != nil {
		return kindLoadBalancerPin{}, fmt.Errorf("resolve %s, which %s names at %s: %w", reference, kindLoadBalancerSource, kindTag, err)
	}
	return kindLoadBalancerPin{KindVersion: kindTag, Image: reference.String() + "@" + digest}, nil
}

// imageReference is a fully qualified registry/repository:tag.
type imageReference struct {
	registry   string
	repository string
	tag        string
}

func (r imageReference) String() string {
	return r.registry + "/" + r.repository + ":" + r.tag
}

// loadBalancerImageFromSource returns the value of the one string constant
// named Image in kind's loadbalancer package.
func loadBalancerImageFromSource(source []byte) (imageReference, error) {
	file, err := parser.ParseFile(token.NewFileSet(), kindLoadBalancerSource, source, parser.SkipObjectResolution)
	if err != nil {
		return imageReference{}, fmt.Errorf("parse: %w", err)
	}
	if file.Name.Name != "loadbalancer" {
		return imageReference{}, fmt.Errorf("declares package %s, not loadbalancer", file.Name.Name)
	}
	var values []string
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range valueSpec.Names {
				if name.Name != "Image" {
					continue
				}
				if index >= len(valueSpec.Values) {
					return imageReference{}, errors.New("constant Image has no value of its own")
				}
				literal, ok := valueSpec.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return imageReference{}, errors.New("constant Image is not a string literal")
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					return imageReference{}, fmt.Errorf("constant Image: %w", err)
				}
				values = append(values, value)
			}
		}
	}
	if len(values) != 1 {
		return imageReference{}, fmt.Errorf("declares %d constants named Image, want 1", len(values))
	}
	match := imageReferencePattern.FindStringSubmatch(values[0])
	if match == nil {
		return imageReference{}, fmt.Errorf("constant Image is %q, not the registry, repository and tag of one image", values[0])
	}
	return imageReference{registry: match[1], repository: match[2], tag: match[3]}, nil
}

// registryEndpoint is the API root of a registry host. Docker Hub answers on
// another name than the one its images are spelled with.
func registryEndpoint(registry string) string {
	if registry == dockerHubRegistry {
		return dockerHubEndpoint
	}
	return "https://" + registry
}

// indexDigest reads the manifest the reference's tag names, anonymously, and
// returns the digest of the bytes served. It refuses anything but an index
// that lists every required platform.
func (r loadBalancerResolver) indexDigest(ctx context.Context, reference imageReference) (string, error) {
	manifestURL, err := url.JoinPath(r.registryEndpoint(reference.registry), "v2", reference.repository, "manifests", reference.tag)
	if err != nil {
		return "", fmt.Errorf("build the manifest URL: %w", err)
	}
	header := http.Header{}
	header.Set("Accept", manifestAccept)
	answer, err := get(ctx, r.client, manifestURL, header, nil)
	if err != nil {
		return "", err
	}
	if answer.status == http.StatusUnauthorized {
		bearer, err := r.anonymousToken(ctx, answer.header.Get("WWW-Authenticate"), reference.repository)
		if err != nil {
			return "", err
		}
		header.Set("Authorization", "Bearer "+bearer)
		answer, err = get(ctx, r.client, manifestURL, header, nil)
		if err != nil {
			return "", err
		}
	}
	if answer.status != http.StatusOK {
		return "", fmt.Errorf("GET %s: unexpected HTTP status %s", answer.url, answer.statusText)
	}

	contentType := answer.header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("GET %s: unreadable Content-Type %q: %w", answer.url, contentType, err)
	}
	if !indexMediaTypes[mediaType] {
		return "", fmt.Errorf("GET %s: the tag names a %s, not a multi-platform index", answer.url, mediaType)
	}
	var index struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(answer.body, &index); err != nil {
		return "", fmt.Errorf("GET %s: decode the index: %w", answer.url, err)
	}
	if index.MediaType != "" && index.MediaType != mediaType {
		return "", fmt.Errorf("GET %s: the index says it is a %s, served as a %s", answer.url, index.MediaType, mediaType)
	}
	listed := make(map[string]bool, len(index.Manifests))
	for _, manifest := range index.Manifests {
		listed[manifest.Platform.OS+"/"+manifest.Platform.Architecture] = true
	}
	for _, platform := range requiredPlatforms {
		if !listed[platform] {
			return "", fmt.Errorf("GET %s: the index lists no %s image", answer.url, platform)
		}
	}

	sum := sha256.Sum256(answer.body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if served := answer.header.Get("Docker-Content-Digest"); served != "" && served != digest {
		return "", fmt.Errorf("GET %s: the registry says the index is %s, its bytes are %s", answer.url, served, digest)
	}
	return digest, nil
}

// anonymousToken answers a registry's Bearer challenge without credentials,
// which is how Docker Hub grants a pull of a public repository.
func (r loadBalancerResolver) anonymousToken(ctx context.Context, challenge, repository string) (string, error) {
	scheme, parameters, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("the registry refused an anonymous read with challenge %q, not a Bearer token challenge", challenge)
	}
	values := map[string]string{}
	for _, match := range challengePattern.FindAllStringSubmatch(parameters, -1) {
		values[strings.ToLower(match[1])] = match[2]
	}
	realm := values["realm"]
	if realm == "" {
		return "", fmt.Errorf("the registry's Bearer challenge %q names no realm", challenge)
	}
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse the token realm %q: %w", realm, err)
	}
	query := tokenURL.Query()
	if service := values["service"]; service != "" {
		query.Set("service", service)
	}
	scope := values["scope"]
	if scope == "" {
		scope = "repository:" + repository + ":pull"
	}
	query.Set("scope", scope)
	tokenURL.RawQuery = query.Encode()

	header := http.Header{}
	header.Set("Accept", "application/json")
	answer, err := get(ctx, r.client, tokenURL.String(), header, nil)
	if err != nil {
		return "", fmt.Errorf("request an anonymous pull token: %w", err)
	}
	if answer.status != http.StatusOK {
		return "", fmt.Errorf("request an anonymous pull token: GET %s: unexpected HTTP status %s", answer.url, answer.statusText)
	}
	var grant struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(answer.body, &grant); err != nil {
		return "", fmt.Errorf("decode the anonymous pull token: %w", err)
	}
	if grant.Token != "" {
		return grant.Token, nil
	}
	if grant.AccessToken != "" {
		return grant.AccessToken, nil
	}
	return "", errors.New("the token service granted no anonymous pull token")
}

func sameSupportBundle(left, right supportManifest) bool {
	if left.KindVersion != right.KindVersion || len(left.Releases) != len(right.Releases) {
		return false
	}
	for index := range left.Releases {
		if left.Releases[index] != right.Releases[index] {
			return false
		}
	}
	return true
}

func selectKindRelease(published []githubRelease, targets []string) (string, map[string]imageCandidate, error) {
	targetSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetSet[target] = struct{}{}
	}

	type candidate struct {
		kindVersion semanticVersion
		kindTag     string
		images      map[string]imageCandidate
	}
	var candidates []candidate
	for _, item := range published {
		if item.Draft || item.Prerelease {
			continue
		}
		version, err := parseVersion(item.TagName)
		if err != nil {
			continue
		}
		images, err := imagesFromRelease(item.Body, targetSet)
		if err != nil {
			return "", nil, fmt.Errorf("kind release %s: %w", item.TagName, err)
		}
		if len(images) != len(targets) {
			continue
		}
		candidates = append(candidates, candidate{kindVersion: version, kindTag: item.TagName, images: images})
	}
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("no stable kind release contains digest-pinned node images for the complete %s window", strings.Join(targets, ", "))
	}
	sort.Slice(candidates, func(i, j int) bool {
		return compareVersions(candidates[i].kindVersion, candidates[j].kindVersion) > 0
	})
	return candidates[0].kindTag, candidates[0].images, nil
}

func imagesFromRelease(body string, targets map[string]struct{}) (map[string]imageCandidate, error) {
	images := make(map[string]imageCandidate, len(targets))
	for _, match := range kindImagePattern.FindAllStringSubmatch(body, -1) {
		major, _ := strconv.Atoi(match[1])
		minor, _ := strconv.Atoi(match[2])
		patch, _ := strconv.Atoi(match[3])
		minorName := fmt.Sprintf("%d.%d", major, minor)
		if _, wanted := targets[minorName]; !wanted {
			continue
		}
		candidate := imageCandidate{
			version: semanticVersion{major: major, minor: minor, patch: patch},
			image:   fmt.Sprintf("kindest/node:v%d.%d.%d@sha256:%s", major, minor, patch, match[4]),
		}
		current, exists := images[minorName]
		if exists && compareVersions(current.version, candidate.version) == 0 && current.image != candidate.image {
			return nil, fmt.Errorf("ambiguous digest for Kubernetes %d.%d.%d", major, minor, patch)
		}
		if !exists || compareVersions(candidate.version, current.version) > 0 {
			images[minorName] = candidate
		}
	}
	return images, nil
}

func parseVersion(value string) (semanticVersion, error) {
	match := stableVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return semanticVersion{}, fmt.Errorf("%q is not a stable vMAJOR.MINOR.PATCH version", value)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return semanticVersion{major: major, minor: minor, patch: patch}, nil
}

func compareVersions(left, right semanticVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

func applyUpdate(repositoryRoot string, plan updatePlan) (bool, error) {
	manifestContents, err := json.MarshalIndent(plan.manifest, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", manifestRelativePath, err)
	}
	manifestContents = append(manifestContents, '\n')

	chartPath := filepath.Join(repositoryRoot, chartRelativePath)
	chartContents, err := os.ReadFile(chartPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", chartPath, err)
	}
	updatedChart, err := renderChart(string(chartContents), plan.manifest.Releases)
	if err != nil {
		return false, fmt.Errorf("update %s: %w", chartPath, err)
	}

	docsPath := filepath.Join(repositoryRoot, docsRelativePath)
	docsContents, err := os.ReadFile(docsPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", docsPath, err)
	}
	updatedDocs, err := renderDocumentation(string(docsContents), plan.manifest.Releases)
	if err != nil {
		return false, fmt.Errorf("update %s: %w", docsPath, err)
	}

	updates := []struct {
		path     string
		contents []byte
	}{
		{path: filepath.Join(repositoryRoot, manifestRelativePath), contents: manifestContents},
		{path: chartPath, contents: []byte(updatedChart)},
		{path: docsPath, contents: []byte(updatedDocs)},
	}
	changed := false
	for _, update := range updates {
		wrote, err := writeIfChanged(update.path, update.contents)
		if err != nil {
			return false, err
		}
		changed = changed || wrote
	}
	return changed, nil
}

func renderChart(contents string, releases []release) (string, error) {
	if len(releases) != windowSize {
		return "", fmt.Errorf("support manifest must contain %d releases", windowSize)
	}
	matches := chartRangePattern.FindAllStringIndex(contents, -1)
	if len(matches) != 1 {
		return "", errors.New("expected exactly one quoted kubeVersion field")
	}
	oldest, err := parseMinor(releases[0].Minor)
	if err != nil {
		return "", err
	}
	newest, err := parseMinor(releases[len(releases)-1].Minor)
	if err != nil {
		return "", err
	}
	replacement := fmt.Sprintf("kubeVersion: \">=%d.%d.0-0 <%d.%d.0-0\"", oldest.major, oldest.minor, newest.major, newest.minor+1)
	return chartRangePattern.ReplaceAllString(contents, replacement), nil
}

func renderDocumentation(contents string, releases []release) (string, error) {
	begin := strings.Index(contents, docsBeginMarker)
	end := strings.Index(contents, docsEndMarker)
	if begin < 0 || end < 0 || end < begin || strings.Count(contents, docsBeginMarker) != 1 || strings.Count(contents, docsEndMarker) != 1 {
		return "", errors.New("expected exactly one ordered generated-support marker pair")
	}
	end += len(docsEndMarker)
	var table strings.Builder
	table.WriteString(docsBeginMarker)
	table.WriteString("\n| Kubernetes minor | CI node image |\n")
	table.WriteString("| --- | --- |\n")
	for _, item := range releases {
		fmt.Fprintf(&table, "| %s | `%s` |\n", item.Minor, item.NodeImage)
	}
	table.WriteString(docsEndMarker)
	return contents[:begin] + table.String() + contents[end:], nil
}

func parseMinor(value string) (semanticVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return semanticVersion{}, fmt.Errorf("minor %q must use MAJOR.MINOR", value)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("minor %q has invalid major: %w", value, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("minor %q has invalid minor: %w", value, err)
	}
	return semanticVersion{major: major, minor: minor}, nil
}

func writeIfChanged(path string, contents []byte) (bool, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s before update: %w", path, err)
	}
	if string(current) == string(contents) {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, contents, info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
