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

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	manifestPath       = "support/kubernetes.json"
	chartPath          = "charts/ptah-operator/Chart.yaml"
	workflowPath       = ".github/workflows/ci.yml"
	updateWorkflowPath = ".github/workflows/update-kubernetes-support.yml"
	docsPath           = "docs/kubernetes-support.md"

	verificationMaxAgeDays = 35
)

var (
	minorPattern     = regexp.MustCompile(`^(\d+)\.(\d+)$`)
	kindImagePattern = regexp.MustCompile(`^kindest/node:v(\d+)\.(\d+)\.(\d+)@sha256:([0-9a-f]{64})$`)
	kindVersion      = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	chartRange       = regexp.MustCompile(`(?m)^kubeVersion:\s*"([^"]+)"\s*$`)
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

type matrixEntry struct {
	Minor             string `json:"minor"`
	MinorSlug         string `json:"minor_slug"`
	KubernetesVersion string `json:"kubernetes_version"`
	NodeImage         string `json:"node_image"`
	KindVersion       string `json:"kind_version"`
}

type parsedRelease struct {
	release
	major int
	minor int
	patch int
}

func main() {
	output := flag.String("output", "verify", "output mode: verify, matrix, or helm-range")
	nowValue := flag.String("now", "", "UTC date used for freshness validation (YYYY-MM-DD; defaults to today)")
	flag.Parse()

	now, err := validationDate(*nowValue)
	if err != nil {
		fatal(err)
	}
	manifest, parsed, err := loadAndValidateManifest(manifestPath, now)
	if err != nil {
		fatal(err)
	}

	expectedRange := helmRange(parsed)
	if err := verifyChart(chartPath, expectedRange); err != nil {
		fatal(err)
	}
	if err := verifyWorkflow(workflowPath); err != nil {
		fatal(err)
	}
	if err := verifyUpdateWorkflow(updateWorkflowPath); err != nil {
		fatal(err)
	}
	if err := verifyDocumentation(docsPath, parsed); err != nil {
		fatal(err)
	}

	switch *output {
	case "verify":
		fmt.Printf("Kubernetes support window verified: %s-%s (%d minors)\n", parsed[0].Minor, parsed[len(parsed)-1].Minor, len(parsed))
	case "matrix":
		entries := make([]matrixEntry, 0, len(parsed))
		for _, item := range parsed {
			entries = append(entries, matrixEntry{
				Minor:             item.Minor,
				MinorSlug:         strings.ReplaceAll(item.Minor, ".", "-"),
				KubernetesVersion: fmt.Sprintf("%d.%d.%d", item.major, item.minor, item.patch),
				NodeImage:         item.NodeImage,
				KindVersion:       manifest.KindVersion,
			})
		}
		encoded, err := json.Marshal(entries)
		if err != nil {
			fatal(fmt.Errorf("encode CI matrix: %w", err))
		}
		fmt.Println(string(encoded))
	case "helm-range":
		fmt.Println(expectedRange)
	default:
		fatal(fmt.Errorf("unsupported -output value %q", *output))
	}
}

func validationDate(value string) (time.Time, error) {
	if value == "" {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("-now must use YYYY-MM-DD: %w", err)
	}
	return parsed, nil
}

func loadAndValidateManifest(path string, now time.Time) (supportManifest, []parsedRelease, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return supportManifest{}, nil, fmt.Errorf("read %s: %w", path, err)
	}

	var manifest supportManifest
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return supportManifest{}, nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errorsIsEOF(err) {
		if err == nil {
			return supportManifest{}, nil, fmt.Errorf("decode %s: trailing JSON value", path)
		}
		return supportManifest{}, nil, fmt.Errorf("decode %s after first JSON value: %w", path, err)
	}
	if manifest.SchemaVersion != 1 {
		return supportManifest{}, nil, fmt.Errorf("%s: schemaVersion must be 1", path)
	}
	if manifest.Policy != "upstream-active-minors" {
		return supportManifest{}, nil, fmt.Errorf("%s: policy must be upstream-active-minors", path)
	}
	if manifest.WindowSize != 3 {
		return supportManifest{}, nil, fmt.Errorf("%s: windowSize must track the three upstream-maintained minors", path)
	}
	if len(manifest.Releases) != manifest.WindowSize {
		return supportManifest{}, nil, fmt.Errorf("%s: releases has %d entries, want windowSize %d", path, len(manifest.Releases), manifest.WindowSize)
	}
	lastVerified, err := time.Parse("2006-01-02", manifest.LastVerified)
	if err != nil {
		return supportManifest{}, nil, fmt.Errorf("%s: lastVerified must use YYYY-MM-DD: %w", path, err)
	}
	verificationAge := now.Sub(lastVerified)
	if verificationAge < 0 {
		return supportManifest{}, nil, fmt.Errorf("%s: lastVerified %s is after validation date %s", path, manifest.LastVerified, now.Format("2006-01-02"))
	}
	if verificationAge > verificationMaxAgeDays*24*time.Hour {
		return supportManifest{}, nil, fmt.Errorf(
			"%s: lastVerified %s is stale on %s (maximum age is %d days); run the scheduled support-window updater",
			path,
			manifest.LastVerified,
			now.Format("2006-01-02"),
			verificationMaxAgeDays,
		)
	}
	if !kindVersion.MatchString(manifest.KindVersion) {
		return supportManifest{}, nil, fmt.Errorf("%s: kindVersion %q is not a stable semantic version", path, manifest.KindVersion)
	}

	parsed := make([]parsedRelease, 0, len(manifest.Releases))
	seenImages := make(map[string]struct{}, len(manifest.Releases))
	for index, item := range manifest.Releases {
		minorMatch := minorPattern.FindStringSubmatch(item.Minor)
		if minorMatch == nil {
			return supportManifest{}, nil, fmt.Errorf("%s: releases[%d].minor %q must be major.minor", path, index, item.Minor)
		}
		major, _ := strconv.Atoi(minorMatch[1])
		minor, _ := strconv.Atoi(minorMatch[2])

		imageMatch := kindImagePattern.FindStringSubmatch(item.NodeImage)
		if imageMatch == nil {
			return supportManifest{}, nil, fmt.Errorf("%s: releases[%d].nodeImage must be a digest-pinned kindest/node image", path, index)
		}
		imageMajor, _ := strconv.Atoi(imageMatch[1])
		imageMinor, _ := strconv.Atoi(imageMatch[2])
		patch, _ := strconv.Atoi(imageMatch[3])
		if imageMajor != major || imageMinor != minor {
			return supportManifest{}, nil, fmt.Errorf("%s: releases[%d] minor %s does not match node image version %d.%d", path, index, item.Minor, imageMajor, imageMinor)
		}
		if _, duplicate := seenImages[item.NodeImage]; duplicate {
			return supportManifest{}, nil, fmt.Errorf("%s: duplicate node image %q", path, item.NodeImage)
		}
		seenImages[item.NodeImage] = struct{}{}
		parsed = append(parsed, parsedRelease{release: item, major: major, minor: minor, patch: patch})
	}

	if !sort.SliceIsSorted(parsed, func(i, j int) bool {
		if parsed[i].major != parsed[j].major {
			return parsed[i].major < parsed[j].major
		}
		return parsed[i].minor < parsed[j].minor
	}) {
		return supportManifest{}, nil, fmt.Errorf("%s: releases must be sorted from oldest to newest", path)
	}
	for index := 1; index < len(parsed); index++ {
		previous := parsed[index-1]
		current := parsed[index]
		if current.major != previous.major || current.minor != previous.minor+1 {
			return supportManifest{}, nil, fmt.Errorf("%s: releases must contain consecutive minors; %s is followed by %s", path, previous.Minor, current.Minor)
		}
	}

	return manifest, parsed, nil
}

func helmRange(releases []parsedRelease) string {
	oldest := releases[0]
	newest := releases[len(releases)-1]
	return fmt.Sprintf(">=%d.%d.0-0 <%d.%d.0-0", oldest.major, oldest.minor, newest.major, newest.minor+1)
}

func verifyChart(path, expected string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	matches := chartRange.FindAllStringSubmatch(string(contents), -1)
	if len(matches) != 1 {
		return fmt.Errorf("%s: expected exactly one quoted kubeVersion field", path)
	}
	if matches[0][1] != expected {
		return fmt.Errorf("%s: kubeVersion is %q, want %q derived from %s", path, matches[0][1], expected, manifestPath)
	}
	return nil
}

func verifyWorkflow(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	workflow := string(contents)
	required := []string{
		"go run ./hack/verify-kubernetes-support.go -output=matrix",
		"fromJSON(needs.support-matrix.outputs.matrix)",
		"DOCKER_CONTEXT: ${{ steps.docker-context.outputs.name }}",
		"KIND_NODE_IMAGE: ${{ matrix.node_image }}",
		"K8S_VERSION: ${{ matrix.kubernetes_version }}",
		"run: make e2e",
	}
	for _, marker := range required {
		if !strings.Contains(workflow, marker) {
			return fmt.Errorf("%s: missing dynamic support-window marker %q", path, marker)
		}
	}
	return nil
}

func verifyUpdateWorkflow(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	workflow := string(contents)
	required := []string{
		"schedule:",
		"actions: write",
		"contents: write",
		"pull-requests: write",
		"go run ./hack/updatekubernetessupport",
		"go run ./hack/verify-kubernetes-support.go",
		"gh pr create",
		"gh workflow run ci.yml --ref \"$support_branch\"",
	}
	for _, marker := range required {
		if !strings.Contains(workflow, marker) {
			return fmt.Errorf("%s: missing scheduled support-window marker %q", path, marker)
		}
	}
	return nil
}

func verifyDocumentation(path string, releases []parsedRelease) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var expected strings.Builder
	expected.WriteString("<!-- BEGIN GENERATED KUBERNETES SUPPORT -->\n")
	expected.WriteString("| Kubernetes minor | CI node image |\n")
	expected.WriteString("| --- | --- |\n")
	for _, item := range releases {
		fmt.Fprintf(&expected, "| %s | `%s` |\n", item.Minor, item.NodeImage)
	}
	expected.WriteString("<!-- END GENERATED KUBERNETES SUPPORT -->")
	if !strings.Contains(string(contents), expected.String()) {
		return fmt.Errorf("%s: generated support table does not match %s", path, manifestPath)
	}
	return nil
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
