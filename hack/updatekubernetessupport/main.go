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
// documentation and Helm range.
package main

import (
	"context"
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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultStableURL       = "https://dl.k8s.io/release/stable.txt"
	defaultKindReleasesURL = "https://api.github.com/repos/kubernetes-sigs/kind/releases?per_page=100"
	manifestRelativePath   = "support/kubernetes.json"
	chartRelativePath      = "charts/ptah-operator/Chart.yaml"
	docsRelativePath       = "docs/kubernetes-support.md"
	windowSize             = 3
	maximumResponseBytes   = 16 << 20
	requestTimeout         = 30 * time.Second
	docsBeginMarker        = "<!-- BEGIN GENERATED KUBERNETES SUPPORT -->"
	docsEndMarker          = "<!-- END GENERATED KUBERNETES SUPPORT -->"
)

var (
	stableVersionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	kindImagePattern     = regexp.MustCompile(`kindest/node:v(\d+)\.(\d+)\.(\d+)@sha256:([0-9a-f]{64})`)
	chartRangePattern    = regexp.MustCompile(`(?m)^kubeVersion:\s*"[^"]+"\s*$`)
)

type supportManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Policy        string    `json:"policy"`
	WindowSize    int       `json:"windowSize"`
	LastVerified  string    `json:"lastVerified"`
	KindVersion   string    `json:"kindVersion"`
	Releases      []release `json:"releases"`
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

func main() {
	repositoryRoot := flag.String("repository-root", ".", "repository root to update")
	stableURL := flag.String("stable-url", defaultStableURL, "official Kubernetes stable release URL")
	kindReleasesURL := flag.String("kind-releases-url", defaultKindReleasesURL, "official kind GitHub releases API URL")
	dateValue := flag.String("date", "", "verification date in UTC (YYYY-MM-DD; defaults to today)")
	flag.Parse()

	verificationDate, err := parseDate(*dateValue)
	if err != nil {
		fatal(err)
	}

	manifestPath := filepath.Join(*repositoryRoot, manifestRelativePath)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		fatal(err)
	}

	client := &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: secureRedirectPolicy,
	}
	stableContents, err := fetch(context.Background(), client, *stableURL, "")
	if err != nil {
		fatal(fmt.Errorf("read official Kubernetes stable release: %w", err))
	}
	kindContents, err := fetch(context.Background(), client, *kindReleasesURL, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		fatal(fmt.Errorf("read official kind releases: %w", err))
	}

	plan, err := buildUpdatePlan(manifest, stableContents, kindContents, verificationDate)
	if err != nil {
		fatal(err)
	}
	changed, err := applyUpdate(*repositoryRoot, plan)
	if err != nil {
		fatal(err)
	}
	if changed {
		fmt.Printf("Updated Kubernetes support window to %s-%s with kind %s\n", plan.oldest, plan.newest, plan.manifest.KindVersion)
		return
	}
	if len(plan.manifest.Releases) == 0 {
		fatal(errors.New("support update unexpectedly produced no releases"))
	}
	fmt.Printf("Kubernetes support window is current at %s-%s with kind %s\n", plan.oldest, plan.newest, plan.manifest.KindVersion)
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
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return nil, fmt.Errorf("URL must be an absolute HTTPS URL")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json, text/plain")
	request.Header.Set("User-Agent", "ptah-operator-kubernetes-support-updater")
	if token != "" && strings.EqualFold(parsedURL.Hostname(), "api.github.com") {
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", parsedURL.Redacted(), err)
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maximumResponseBytes+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", parsedURL.Redacted(), err)
	}
	if len(contents) > maximumResponseBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", parsedURL.Redacted(), maximumResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected HTTP status %s", parsedURL.Redacted(), response.Status)
	}
	return contents, nil
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
	next.LastVerified = verificationDate.Format("2006-01-02")
	next.KindVersion = kindVersion
	next.Releases = make([]release, 0, len(targets))
	for _, minor := range targets {
		next.Releases = append(next.Releases, release{Minor: minor, NodeImage: images[minor].image})
	}
	return updatePlan{manifest: next, oldest: targets[0], newest: targets[len(targets)-1]}, nil
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
