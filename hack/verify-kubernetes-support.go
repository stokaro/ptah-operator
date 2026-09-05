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
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	manifestPath                   = "support/kubernetes.json"
	goModPath                      = "go.mod"
	chartPath                      = "charts/ptah-operator/Chart.yaml"
	workflowPath                   = ".github/workflows/ci.yml"
	updateWorkflowPath             = ".github/workflows/update-kubernetes-support.yml"
	releaseWorkflowPath            = ".github/workflows/release.yml"
	docsPath                       = "docs/kubernetes-support.md"
	makefilePath                   = "Makefile"
	e2eHarnessPath                 = "hack/e2e-kind.sh"
	e2eKindConfigPath              = "testdata/e2e/kind.yaml.tmpl"
	apiServerEndpointFilterPath    = "hack/api-server-endpoint-inventory.jq"
	e2eStaticPath                  = "hack/e2e-static.sh"
	e2eDataPlanePath               = "hack/e2e-dataplane.sh"
	e2eAssertPath                  = "hack/e2e-assert.sh"
	e2eCRDUpgradePath              = "hack/e2e-crd-upgrade.sh"
	e2eFaultsPath                  = "hack/e2e-faults.sh"
	e2eHAPath                      = "hack/e2e-ha.sh"
	e2eCertRotationPath            = "hack/e2e-cert-rotation.sh"
	failedHookEvidencePath         = "hack/failed-hook-evidence.jq"
	failedHookEvidenceSelftestPath = "hack/failed-hook-evidence-selftest.sh"
	admissionSchemaContractPath    = "hack/admission-schema-contract.jq"
	admissionSchemaSelftestPath    = "hack/admission-schema-contract-selftest.sh"
	controllerSchemaContractPath   = "hack/controller-object-schema-contract.jq"
	controllerSchemaSelftestPath   = "hack/controller-object-schema-contract-selftest.sh"

	verificationMaxAgeDays = 35

	reviewedKubernetesAPIMinor       = 36
	reviewedKubernetesSupportMaximum = 37
	reviewedJobAPISurfaceSHA256      = "f3e0bedd7235834b17dc52727eddb1268c1c62d7f9ec7cc5bdb4fcd494f4922f"
	// These digests make workflow policy changes explicit. Semantic checks keep
	// failures actionable; the whole-file digests also cover setup steps that
	// could otherwise alter GITHUB_ENV, GITHUB_PATH, or later shell behavior.
	ciWorkflowSHA256                = "a30ca2c550af04a0b4a6abf8a6cec7ea226121a4af6417da10403cb1a973a9c3"
	updateWorkflowSHA256            = "6c26ffcdfccc60a28f16e600ec6f29b22d139f3637979d880c4623833b4b6580"
	releaseWorkflowSHA256           = "fb27f9d93cb0bee270e8724386b3141dd4b374664860fa7b63992f25448dcef8"
	releaseSupportEvidenceRunSHA256 = "e4880ca682553c9ca3f26a9265d23407f3d0ebb04665f32ad5d541550a9e4dcf"
	releaseChartPackageRunSHA256    = "fcb5ca9057f0307cd27824d1011b12ad1c7b4b5df6b534a505a70da607da37c8"
	releaseChartExportRunSHA256     = "a34800805204a2caa071d03939f9337f3472028ecb8b9c11ed26723294eb8082"
	controllerSchemaSHA256          = "b73a7b8718abd34b4a8f45a1342c31c50690bf82358b378621dfbbe6e30892e5"
	raceValidationRuleSHA256        = "41883b775532ad9be0035521d4363052137a8debb4f4a4185ec6a0f3c4a97ae9"
	raceBaseRuleSHA256              = "53a29b937246901f0b2f285964ea3a2b7580e016ab447ff2b9cb10189df83b49"
	raceMutationRuleSHA256          = "1b9d7a915e91728ce653c78534382d5d59935bdf09ceec8cba3470900c3fc2dc"
	raceAggregateSHA256             = "c4ebaf33f633432020b25919b04b70b8715ab64ef63118fbe47dee45553915f1"

	ciSupportMatrixTimeoutMinutes     = 10
	ciVerifyTimeoutMinutes            = 20
	ciRaceTimeoutMinutes              = 40
	ciKubernetesE2ETimeoutMinutes     = 90
	ciKubernetesSupportTimeoutMinutes = 5
	releaseQueueAPIMarginMinutes      = 5
	releaseSupportPollTimeoutMinutes  = max(ciSupportMatrixTimeoutMinutes, ciVerifyTimeoutMinutes, ciRaceTimeoutMinutes) + ciKubernetesE2ETimeoutMinutes + ciKubernetesSupportTimeoutMinutes + releaseQueueAPIMarginMinutes
	releasePreflightOverheadMinutes   = 10
	releasePreflightJobTimeoutMinutes = releaseSupportPollTimeoutMinutes + releasePreflightOverheadMinutes
)

var updateRunSHA256 = map[string]string{
	"prepare/discover":           "0777e441d8638d72eb040d78366facf3dc3e0674e3ab82fdcb7c8548c7a775b0",
	"prepare/verify":             "038bedf5ed59eb6eaecfe2473d0c6ac854b18a226be67a8d8c8b49c95b134337",
	"prepare/bundle":             "0a2282370fad04a75a56821a662f64ce2955f36abff8e4d7d1cafddb3b60a8b9",
	"propose/apply-bundle":       "07427747ba0f70786046ecd7ade587f8fed37fdb4e39add4c786f11f41416b7a",
	"propose/support-window-pr":  "52a1ca884f27872b285a23448748eac3c79de29847434bf94f74c8dd7a5eafb1",
	"dispatch/dispatch-evidence": "c1bda73672b5f67bf582cb77d628acd30f201828d40c631deb7902ab435769ae",
}

var (
	minorPattern     = regexp.MustCompile(`^(\d+)\.(\d+)$`)
	kindImagePattern = regexp.MustCompile(`^kindest/node:v(\d+)\.(\d+)\.(\d+)@sha256:([0-9a-f]{64})$`)
	kindVersion      = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	chartRange       = regexp.MustCompile(`(?m)^kubeVersion:\s*"([^"]+)"\s*$`)
	kubernetesModule = regexp.MustCompile(`(?m)^[\t ]*(k8s\.io/(?:api|apiextensions-apiserver|apimachinery|client-go))[\t ]+v0\.([0-9]+)\.([0-9]+)(?:[\t ]|$)`)
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
	output := flag.String("output", "verify", "output mode: verify, proposal, matrix, or helm-range")
	nowValue := flag.String("now", "", "UTC date used for freshness validation (YYYY-MM-DD; defaults to today)")
	flag.Parse()
	proposal := *output == "proposal"

	now, err := validationDate(*nowValue)
	if err != nil {
		fatal(err)
	}
	manifest, parsed, err := loadAndValidateManifest(manifestPath, now)
	if err != nil {
		fatal(err)
	}
	compiledMinor, err := verifyKubernetesDependencyWindowForMode(goModPath, parsed, proposal)
	if err != nil {
		fatal(err)
	}
	if err := verifyJobAPIBoundaryForMode(
		compiledMinor,
		parsed[len(parsed)-1].minor,
		controllerJobAPISurfaceDigest(),
		proposal,
	); err != nil {
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
	if err := verifyReleaseWorkflow(releaseWorkflowPath); err != nil {
		fatal(err)
	}
	if err := verifyDocumentation(docsPath, parsed); err != nil {
		fatal(err)
	}
	if err := verifyE2EWiring(e2eWiringFiles{
		makefile:                   makefilePath,
		harness:                    e2eHarnessPath,
		kindConfig:                 e2eKindConfigPath,
		apiServerEndpointFilter:    apiServerEndpointFilterPath,
		staticChecks:               e2eStaticPath,
		dataPlane:                  e2eDataPlanePath,
		assertions:                 e2eAssertPath,
		crdUpgrade:                 e2eCRDUpgradePath,
		faults:                     e2eFaultsPath,
		highAvailability:           e2eHAPath,
		certRotation:               e2eCertRotationPath,
		failedHookEvidence:         failedHookEvidencePath,
		failedHookEvidenceSelftest: failedHookEvidenceSelftestPath,
		admissionSchemaContract:    admissionSchemaContractPath,
		admissionSchemaSelftest:    admissionSchemaSelftestPath,
		controllerSchemaContract:   controllerSchemaContractPath,
		controllerSchemaSelftest:   controllerSchemaSelftestPath,
	}); err != nil {
		fatal(err)
	}

	switch *output {
	case "verify":
		fmt.Printf("Kubernetes support window verified: %s-%s (%d minors)\n", parsed[0].Minor, parsed[len(parsed)-1].Minor, len(parsed))
	case "proposal":
		fmt.Printf("Kubernetes support proposal validated: %s-%s (%d minors); ordinary verification still enforces the frozen API boundary\n", parsed[0].Minor, parsed[len(parsed)-1].Minor, len(parsed))
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

func verifyKubernetesDependencyWindow(path string, releases []parsedRelease) (int, error) {
	return verifyKubernetesDependencyWindowForMode(path, releases, false)
}

func verifyKubernetesDependencyWindowForMode(path string, releases []parsedRelease, proposal bool) (int, error) {
	if len(releases) == 0 {
		return 0, errors.New("Kubernetes dependency verification requires a non-empty support window")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	required := []string{
		"k8s.io/api",
		"k8s.io/apiextensions-apiserver",
		"k8s.io/apimachinery",
		"k8s.io/client-go",
	}
	versions := make(map[string]int, len(required))
	for _, match := range kubernetesModule.FindAllStringSubmatch(string(contents), -1) {
		if _, duplicate := versions[match[1]]; duplicate {
			return 0, fmt.Errorf("%s: Kubernetes module %s is required exactly once", path, match[1])
		}
		minor, conversionErr := strconv.Atoi(match[2])
		if conversionErr != nil {
			return 0, fmt.Errorf("%s: parse Kubernetes module %s minor: %w", path, match[1], conversionErr)
		}
		versions[match[1]] = minor
	}
	for _, module := range required {
		if _, exists := versions[module]; !exists {
			return 0, fmt.Errorf("%s: %s must have one stable v0.MINOR.PATCH requirement", path, module)
		}
	}

	compiledMinor := versions[required[0]]
	for _, module := range required[1:] {
		if versions[module] != compiledMinor {
			return 0, fmt.Errorf(
				"%s: Kubernetes modules must share one API minor; %s uses 0.%d while %s uses 0.%d",
				path,
				required[0],
				compiledMinor,
				module,
				versions[module],
			)
		}
	}

	newest := releases[len(releases)-1]
	if newest.major != 1 {
		return 0, fmt.Errorf("%s: Kubernetes Go module mapping only supports major 1, got %s", path, newest.Minor)
	}
	forwardSkew := newest.minor - compiledMinor
	if forwardSkew < 0 {
		return 0, fmt.Errorf(
			"%s: Kubernetes Go API 0.%d is newer than the advertised support maximum %s",
			path,
			compiledMinor,
			newest.Minor,
		)
	}
	maximumForwardSkew := 1
	if proposal {
		// Proposal validation may expose exactly the next maintained minor in a
		// pull request before its compiled API surface has been reviewed. Normal
		// verification below remains the authority for support and release.
		maximumForwardSkew = 2
	}
	if forwardSkew > maximumForwardSkew {
		return 0, fmt.Errorf(
			"%s: newest proposed Kubernetes %s is %d minors ahead of the compiled Go API 0.%d; update and review the Job/Pod API boundary before advancing the window",
			path,
			newest.Minor,
			forwardSkew,
			compiledMinor,
		)
	}
	return compiledMinor, nil
}

func verifyJobAPIBoundaryForMode(compiledMinor, supportedMaximum int, actualDigest string, proposal bool) error {
	strictErr := verifyReviewedJobAPIBoundary(compiledMinor, supportedMaximum, actualDigest)
	if strictErr == nil || !proposal {
		return strictErr
	}
	// A proposal is discovery evidence, not a support claim. Permit only the
	// immediate next supported maximum while the compiled dependency and every
	// reachable Job/Pod field remain byte-for-byte at the reviewed boundary.
	// The ordinary matrix and release modes still call the strict branch above
	// and therefore keep the proposed pull request red until review is explicit.
	if compiledMinor != reviewedKubernetesAPIMinor ||
		supportedMaximum != reviewedKubernetesSupportMaximum+1 ||
		actualDigest != reviewedJobAPISurfaceSHA256 {
		return strictErr
	}
	return nil
}

func verifyReviewedJobAPIBoundary(compiledMinor, supportedMaximum int, actualDigest string) error {
	if compiledMinor != reviewedKubernetesAPIMinor || supportedMaximum != reviewedKubernetesSupportMaximum {
		return fmt.Errorf(
			"Kubernetes dependency/support profile %d/%d differs from reviewed Job/Pod API boundary %d/%d; review the reachable Job/Pod spec and status fields and update the structural guard",
			compiledMinor,
			supportedMaximum,
			reviewedKubernetesAPIMinor,
			reviewedKubernetesSupportMaximum,
		)
	}
	if actualDigest != reviewedJobAPISurfaceSHA256 {
		return fmt.Errorf(
			"compiled reachable Job/Pod API surface digest is %s, want reviewed digest %s; review the Job/Pod spec and status JSON field graph before accepting dependency drift",
			actualDigest,
			reviewedJobAPISurfaceSHA256,
		)
	}
	return nil
}

type jobAPISurfaceEntry struct {
	Type   string   `json:"type"`
	Fields []string `json:"fields"`
}

func controllerJobAPISurfaceDigest() string {
	visited := make(map[reflect.Type]struct{})
	entries := make([]jobAPISurfaceEntry, 0)
	var visit func(reflect.Type)
	visit = func(value reflect.Type) {
		for value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
			value = value.Elem()
		}
		if value.Kind() == reflect.Map {
			visit(value.Elem())
			return
		}
		if value.Kind() != reflect.Struct || !strings.HasPrefix(value.PkgPath(), "k8s.io/api/") {
			return
		}
		if _, exists := visited[value]; exists {
			return
		}
		visited[value] = struct{}{}

		fields := make([]string, 0, value.NumField())
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if !field.IsExported() {
				continue
			}
			jsonTag := field.Tag.Get("json")
			jsonName := strings.Split(jsonTag, ",")[0]
			if jsonName == "-" {
				continue
			}
			if jsonName == "" && !field.Anonymous {
				jsonName = field.Name
			}
			if jsonName != "" {
				fields = append(fields, jsonName)
			}
			visit(field.Type)
		}
		sort.Strings(fields)
		entries = append(entries, jobAPISurfaceEntry{
			Type:   value.PkgPath() + "." + value.Name(),
			Fields: fields,
		})
	}

	// JobSpec reaches PodSpec through the template. Status is a separate API
	// graph, but the admission boundary relies on both JobStatus and PodStatus
	// when authenticating terminal progress and scheduler/node updates.
	for _, root := range []reflect.Type{
		reflect.TypeOf(batchv1.JobSpec{}),
		reflect.TypeOf(batchv1.JobStatus{}),
		reflect.TypeOf(corev1.PodStatus{}),
	} {
		visit(root)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Type < entries[right].Type })
	canonical, err := json.Marshal(entries)
	if err != nil {
		panic(fmt.Sprintf("marshal reachable Job/Pod API surface: %v", err))
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
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
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	if err := verifyCIWorkflowSemantics(path, workflow, contents); err != nil {
		return err
	}
	return verifyAuditedWorkflowDigest(path, contents, ciWorkflowSHA256)
}

func verifyCIWorkflowSemantics(path string, workflow workflowDocument, contents []byte) error {
	required := []string{
		"go run ./hack/verify-kubernetes-support.go -output=matrix",
		"fromJSON(needs.support-matrix.outputs.matrix)",
		"PULL_REQUEST_BASE_SHA: ${{ github.event.pull_request.base.sha }}",
		"EVENT_BEFORE_SHA: ${{ github.event.before }}",
		"CRD_SCHEMA_BASELINE_REF: ${{ steps.crd-baseline.outputs.baseline }}",
		"CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE: \"true\"",
		"run: make verify-source",
		"run: make test-race",
		"DOCKER_CONTEXT: ${{ steps.docker-context.outputs.name }}",
		"E2E_RELEASE_CHART_OUTPUT: ${{ runner.temp }}/ptah-operator-${{ matrix.minor_slug }}.tgz",
		"KIND_NODE_IMAGE: ${{ matrix.node_image }}",
		"K8S_VERSION: ${{ matrix.kubernetes_version }}",
		"run: make e2e",
		"uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
	}
	for _, marker := range required {
		if !bytes.Contains(contents, []byte(marker)) {
			return fmt.Errorf("%s: missing dynamic support-window marker %q", path, marker)
		}
	}
	if len(workflow.Defaults) != 0 {
		return fmt.Errorf("%s: workflow-level defaults are forbidden for the audited lifecycle", path)
	}
	if !equalStringMap(workflow.Env, map[string]string{"GOFLAGS": "-mod=readonly"}) {
		return fmt.Errorf("%s: workflow environment must contain only the audited GOFLAGS value", path)
	}
	for _, jobName := range []string{"support-matrix", "verify", "race", "kubernetes-e2e", "kubernetes-support-gate"} {
		job, err := requireWorkflowJob(path, workflow, jobName)
		if err != nil {
			return err
		}
		if err := verifyNoContinueOnError(path, jobName, job); err != nil {
			return err
		}
		if len(job.Defaults) != 0 {
			return fmt.Errorf("%s: job %q must not override run defaults", path, jobName)
		}
		if len(job.Env) != 0 {
			return fmt.Errorf("%s: job %q must not inject lifecycle environment variables", path, jobName)
		}
	}
	supportMatrix := workflow.Jobs["support-matrix"]
	if supportMatrix.If != "" || supportMatrix.TimeoutMinutes != ciSupportMatrixTimeoutMinutes {
		return fmt.Errorf("%s: support-matrix must run unconditionally with a %d-minute timeout", path, ciSupportMatrixTimeoutMinutes)
	}
	if !equalStringMap(supportMatrix.Outputs, map[string]string{
		"matrix": "${{ steps.matrix.outputs.matrix }}",
	}) {
		return fmt.Errorf("%s: support-matrix output must bind exactly to the matrix step output", path)
	}
	matrixStep, err := requireWorkflowStep(path, "support-matrix", supportMatrix, "matrix")
	if err != nil {
		return err
	}
	const wantMatrixRun = `set -euo pipefail
matrix="$(go run ./hack/verify-kubernetes-support.go -output=matrix)"
echo "matrix=$matrix" >> "$GITHUB_OUTPUT"
`
	if matrixStep.If != "" || matrixStep.Shell != "bash" || matrixStep.Run != wantMatrixRun {
		return fmt.Errorf("%s: support-matrix step must unconditionally export the verified dynamic matrix", path)
	}

	verifyJob := workflow.Jobs["verify"]
	if verifyJob.If != "" || verifyJob.TimeoutMinutes != ciVerifyTimeoutMinutes {
		return fmt.Errorf("%s: verify must run unconditionally with a %d-minute timeout", path, ciVerifyTimeoutMinutes)
	}
	verifySteps, err := requireWorkflowStepOrder(path, "verify", verifyJob, []string{
		"checkout", "setup-go", "verify-support", "crd-baseline", "project-verify",
	})
	if err != nil {
		return err
	}
	if verifySteps[0].Name != "Check out repository" {
		return fmt.Errorf("%s: verify checkout step has unexpected name %q", path, verifySteps[0].Name)
	}
	if err := verifyUpdaterActionStep(
		path,
		"verify",
		verifySteps[0],
		"actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
		map[string]string{"fetch-depth": "0", "persist-credentials": "false"},
	); err != nil {
		return err
	}
	if verifySteps[1].Name != "Set up Go" {
		return fmt.Errorf("%s: verify Go setup step has unexpected name %q", path, verifySteps[1].Name)
	}
	if err := verifyUpdaterActionStep(
		path,
		"verify",
		verifySteps[1],
		"actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
		map[string]string{"go-version-file": "go.mod", "cache-dependency-path": "go.sum"},
	); err != nil {
		return err
	}
	if verifySteps[2].Name != "Verify Kubernetes support window" ||
		verifySteps[2].If != "" || verifySteps[2].Uses != "" ||
		verifySteps[2].Run != "go run ./hack/verify-kubernetes-support.go" ||
		verifySteps[2].Shell != "bash" || verifySteps[2].WorkingDirectory != "" ||
		len(verifySteps[2].With) != 0 || len(verifySteps[2].Env) != 0 {
		return fmt.Errorf("%s: verify-support must be the unconditional audited support verifier invocation", path)
	}
	const wantCRDBaselineRun = `set -euo pipefail
zero_sha=0000000000000000000000000000000000000000
commit_pattern='^[0-9a-f]{40}$'

[[ "$CURRENT_SHA" =~ $commit_pattern ]]
checked_out_sha="$(git rev-parse --verify HEAD)"
[[ "$checked_out_sha" == "$CURRENT_SHA" ]]

resolve_exact_commit() {
  local reference=$1
  local resolved
  resolved="$(git rev-parse --verify --end-of-options "${reference}^{commit}")"
  [[ "$resolved" =~ $commit_pattern ]]
  printf '%s\n' "$resolved"
}

case "$EVENT_NAME" in
  pull_request)
    [[ "$PULL_REQUEST_BASE_SHA" =~ $commit_pattern ]]
    baseline="$(resolve_exact_commit "$PULL_REQUEST_BASE_SHA")"
    [[ "$baseline" == "$PULL_REQUEST_BASE_SHA" ]]
    ;;
  push)
    if [[ "$EVENT_BEFORE_SHA" == "$zero_sha" ]]; then
      baseline="$(resolve_exact_commit "${CURRENT_SHA}^")"
    else
      [[ "$EVENT_BEFORE_SHA" =~ $commit_pattern ]]
      baseline="$(resolve_exact_commit "$EVENT_BEFORE_SHA")"
      [[ "$baseline" == "$EVENT_BEFORE_SHA" ]]
    fi
    ;;
  schedule|workflow_dispatch)
    baseline="$(resolve_exact_commit "${CURRENT_SHA}^")"
    ;;
  *)
    echo "unsupported event for CRD schema history baseline: $EVENT_NAME" >&2
    exit 1
    ;;
esac
printf 'baseline=%s\n' "$baseline" >> "$GITHUB_OUTPUT"
`
	if verifySteps[3].Name != "Select exact CRD schema history baseline" ||
		verifySteps[3].If != "" || verifySteps[3].Uses != "" ||
		verifySteps[3].Run != wantCRDBaselineRun || verifySteps[3].Shell != "bash" ||
		verifySteps[3].WorkingDirectory != "" || len(verifySteps[3].With) != 0 ||
		!equalStringMap(verifySteps[3].Env, map[string]string{
			"CURRENT_SHA":           "${{ github.sha }}",
			"EVENT_BEFORE_SHA":      "${{ github.event.before }}",
			"EVENT_NAME":            "${{ github.event_name }}",
			"PULL_REQUEST_BASE_SHA": "${{ github.event.pull_request.base.sha }}",
		}) {
		return fmt.Errorf("%s: crd-baseline must select the exact audited event-specific Git commit", path)
	}
	if verifySteps[4].Name != "Run project verification" ||
		verifySteps[4].If != "" || verifySteps[4].Uses != "" || verifySteps[4].Run != "make verify-source" ||
		verifySteps[4].Shell != "bash" || verifySteps[4].WorkingDirectory != "" ||
		len(verifySteps[4].With) != 0 || !equalStringMap(verifySteps[4].Env, map[string]string{
		"CRD_SCHEMA_BASELINE_REF":              "${{ steps.crd-baseline.outputs.baseline }}",
		"CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE": "true",
	}) {
		return fmt.Errorf("%s: project verification must consume only the explicit audited CRD baseline", path)
	}

	race := workflow.Jobs["race"]
	if race.Name != "Race detector" || race.If != "" || len(race.Needs) != 0 ||
		race.RunsOn != "ubuntu-latest" || race.TimeoutMinutes != ciRaceTimeoutMinutes ||
		len(race.Permissions) != 0 || race.Environment != "" || race.Strategy.FailFast != nil ||
		len(race.Strategy.Matrix) != 0 {
		return fmt.Errorf("%s: race must be an unconditional isolated ubuntu-latest job with a %d-minute timeout", path, ciRaceTimeoutMinutes)
	}
	raceSteps, err := requireWorkflowStepOrder(path, "race", race, []string{
		"race-checkout", "race-setup-go", "project-race",
	})
	if err != nil {
		return err
	}
	if raceSteps[0].Name != "Check out repository" {
		return fmt.Errorf("%s: race checkout step has unexpected name %q", path, raceSteps[0].Name)
	}
	if err := verifyUpdaterActionStep(
		path,
		"race",
		raceSteps[0],
		"actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
		map[string]string{"fetch-depth": "0", "persist-credentials": "false"},
	); err != nil {
		return err
	}
	if raceSteps[1].Name != "Set up Go" {
		return fmt.Errorf("%s: race Go setup step has unexpected name %q", path, raceSteps[1].Name)
	}
	if err := verifyUpdaterActionStep(
		path,
		"race",
		raceSteps[1],
		"actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
		map[string]string{"go-version-file": "go.mod", "cache-dependency-path": "go.sum"},
	); err != nil {
		return err
	}
	if raceSteps[2].Name != "Run complete race coverage" || raceSteps[2].If != "" ||
		raceSteps[2].Uses != "" || raceSteps[2].Run != "make test-race" ||
		raceSteps[2].Shell != "bash" || raceSteps[2].WorkingDirectory != "" ||
		len(raceSteps[2].With) != 0 || len(raceSteps[2].Env) != 0 {
		return fmt.Errorf("%s: race coverage must be the unconditional audited make test-race invocation", path)
	}

	e2e := workflow.Jobs["kubernetes-e2e"]
	if e2e.If != "" || e2e.TimeoutMinutes != ciKubernetesE2ETimeoutMinutes {
		return fmt.Errorf("%s: kubernetes-e2e must run unconditionally with a %d-minute timeout", path, ciKubernetesE2ETimeoutMinutes)
	}
	if !equalStringSet(e2e.Needs, []string{"support-matrix", "verify", "race"}) {
		return fmt.Errorf("%s: kubernetes-e2e dependencies are %v", path, e2e.Needs)
	}
	if e2e.Strategy.FailFast == nil || *e2e.Strategy.FailFast ||
		!equalStringMap(e2e.Strategy.Matrix, map[string]string{
			"include": "${{ fromJSON(needs.support-matrix.outputs.matrix) }}",
		}) {
		return fmt.Errorf("%s: kubernetes-e2e strategy must consume only the verified dynamic matrix with fail-fast disabled", path)
	}
	lifecycleSteps := make([]workflowStep, 0, 1)
	lifecycleIndex := -1
	for index, candidate := range e2e.Steps {
		if candidate.Run == "make e2e" {
			lifecycleSteps = append(lifecycleSteps, candidate)
			lifecycleIndex = index
		}
	}
	if len(lifecycleSteps) != 1 {
		return fmt.Errorf("%s: kubernetes-e2e must contain exactly one run: make e2e step", path)
	}
	lifecycle := lifecycleSteps[0]
	if lifecycle.ID != "lifecycle" || lifecycle.If != "" || lifecycle.Shell != "bash" || lifecycle.WorkingDirectory != "" {
		return fmt.Errorf("%s: run: make e2e must be unconditional, run from the checkout root, and use explicit bash", path)
	}
	wantMatrixEnv := map[string]string{
		"DOCKER_CONTEXT":           "${{ steps.docker-context.outputs.name }}",
		"E2E_DIRECT_HOST_ACCESS":   "1",
		"E2E_PTAH_REVISION":        "00fc362c943bfb9d0363d5890bf449a2a9b5e7cf",
		"E2E_PTAH_SOURCE_DIR":      "${{ runner.temp }}/ptah",
		"E2E_RELEASE_CHART_OUTPUT": "${{ runner.temp }}/ptah-operator-${{ matrix.minor_slug }}.tgz",
		"E2E_RUN_ID":               "ci-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.minor_slug }}",
		"KIND_NODE_IMAGE":          "${{ matrix.node_image }}",
		"K8S_VERSION":              "${{ matrix.kubernetes_version }}",
	}
	if !equalStringMap(lifecycle.Env, wantMatrixEnv) {
		return fmt.Errorf("%s: run: make e2e must use exactly the audited lifecycle environment bindings", path)
	}
	upload, err := requireWorkflowStep(path, "kubernetes-e2e", e2e, "release-chart-evidence")
	if err != nil {
		return err
	}
	if upload.Name != "Preserve exact installed release chart" || lifecycleIndex < 0 ||
		lifecycleIndex+1 >= len(e2e.Steps) || e2e.Steps[lifecycleIndex+1].ID != upload.ID {
		return fmt.Errorf("%s: installed chart evidence must immediately follow the complete lifecycle", path)
	}
	if err := verifyUpdaterActionStep(
		path,
		"kubernetes-e2e",
		upload,
		"actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		map[string]string{
			"name":              "installed-release-chart-${{ matrix.minor_slug }}",
			"path":              "${{ runner.temp }}/ptah-operator-${{ matrix.minor_slug }}.tgz",
			"if-no-files-found": "error",
			"retention-days":    "90",
			"compression-level": "0",
			"overwrite":         "true",
		},
	); err != nil {
		return err
	}

	gate := workflow.Jobs["kubernetes-support-gate"]
	if gate.Name != "Kubernetes support gate" {
		return fmt.Errorf("%s: stable support gate name must be %q", path, "Kubernetes support gate")
	}
	if gate.If != "${{ always() }}" {
		return fmt.Errorf("%s: Kubernetes support gate must run with if: always()", path)
	}
	if gate.TimeoutMinutes != ciKubernetesSupportTimeoutMinutes {
		return fmt.Errorf("%s: Kubernetes support gate timeout must be %d minutes", path, ciKubernetesSupportTimeoutMinutes)
	}
	if !equalStringSet(gate.Needs, []string{"support-matrix", "verify", "race", "kubernetes-e2e"}) {
		return fmt.Errorf("%s: Kubernetes support gate dependencies are %v", path, gate.Needs)
	}
	step, err := requireWorkflowStep(path, "kubernetes-support-gate", gate, "require-results")
	if err != nil {
		return err
	}
	wantEnv := map[string]string{
		"SUPPORT_MATRIX_RESULT": "${{ needs.support-matrix.result }}",
		"VERIFY_RESULT":         "${{ needs.verify.result }}",
		"RACE_RESULT":           "${{ needs.race.result }}",
		"KUBERNETES_E2E_RESULT": "${{ needs.kubernetes-e2e.result }}",
	}
	if !equalStringMap(step.Env, wantEnv) {
		return fmt.Errorf("%s: Kubernetes support gate result bindings do not match its dependencies", path)
	}
	const wantGateRun = `set -euo pipefail
for result in \
  "$SUPPORT_MATRIX_RESULT" \
  "$VERIFY_RESULT" \
  "$RACE_RESULT" \
  "$KUBERNETES_E2E_RESULT"
do
  if [[ "$result" != success ]]; then
    echo "required Kubernetes support job concluded: $result" >&2
    exit 1
  fi
done
`
	if step.Shell != "bash" || step.Run != wantGateRun {
		return fmt.Errorf("%s: Kubernetes support gate must fail explicitly unless every dependency succeeded", path)
	}
	return nil
}

func verifyUpdateWorkflow(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	if err := verifyUpdateWorkflowSemantics(path, workflow, contents); err != nil {
		return err
	}
	return verifyAuditedWorkflowDigest(path, contents, updateWorkflowSHA256)
}

func verifyUpdateWorkflowSemantics(path string, workflow workflowDocument, contents []byte) error {
	required := []string{
		"permissions:\n  contents: read",
		"needs: [prepare]",
		"needs: [prepare, propose]",
		"actions: write",
		"contents: write",
		"pull-requests: write",
		"go run ./hack/updatekubernetessupport",
		"go test ./hack ./hack/updatekubernetessupport",
		"go run ./hack/verify-kubernetes-support.go -output=proposal -now \"$today\"",
		"git status --porcelain=v1 --untracked-files=all",
		"git status --porcelain=v1 --untracked-files=all > \"$status_file\"",
		"patch-base64",
		"patch-sha256",
		"git apply --check",
		"remote_base_sha",
		"if ! git merge-base --is-ancestor \"$remote_oid\" \"$BASE_SHA\"; then",
		"git merge-base --is-ancestor \"$remote_parent\" \"$BASE_SHA\"",
		"git rev-list --count \"$BASE_SHA..$remote_oid\"",
		"support branch contains review commits; refusing to overwrite",
		"git show -s --format=%ae \"$remote_oid\"",
		"git show -s --format=%ce \"$remote_oid\"",
		"git diff-tree --no-commit-id --name-status -r \"$remote_oid\"",
		"mapfile -t prior_status_lines < \"$prior_status_file\"",
		"repos/$GITHUB_REPOSITORY/pulls",
		"-f head=\"$repository_owner:$support_branch\"",
		".head.repo.full_name == $repo",
		"mapfile -t same_repo_pr_numbers",
		"case \"${#same_repo_pr_numbers[@]}\" in",
		"gh pr create",
		".headRefOid == $sha",
		".isCrossRepository == false",
		".headRepository.nameWithOwner == $repo",
		"actions/workflows/$workflow_file/runs",
		"-f branch=\"$SUPPORT_BRANCH\"",
		"-f event=workflow_dispatch",
		"-f head_sha=\"$EXPECTED_SHA\"",
		".event == \"workflow_dispatch\"",
		".head_branch == $branch",
		".head_sha == $sha",
		"$before_ids | index($id)",
		"require_dispatched_run ci.yml",
		"require_dispatched_run release.yml",
	}
	for _, marker := range required {
		if !bytes.Contains(contents, []byte(marker)) {
			return fmt.Errorf("%s: missing scheduled support-window marker %q", path, marker)
		}
	}
	for marker, expected := range map[string]int{
		"--json state,baseRefName,headRefName,headRefOid,isCrossRepository,headRepository": 2,
		".isCrossRepository == false":                                        2,
		".headRepository.nameWithOwner == $repo":                             2,
		"git status --porcelain=v1 --untracked-files=all > \"$status_file\"": 2,
		"41898282+github-actions[bot]@users.noreply.github.com":              3,
	} {
		if count := bytes.Count(contents, []byte(marker)); count != expected {
			return fmt.Errorf("%s: expected %d same-repository pull-request markers %q, found %d", path, expected, marker, count)
		}
	}
	if bytes.Contains(contents, []byte("< <(git status --porcelain=v1 --untracked-files=all)")) {
		return fmt.Errorf("%s: support updater must check git status before consuming its output", path)
	}
	if len(workflow.On) != 2 {
		return fmt.Errorf("%s: support updater must have only schedule and workflow_dispatch triggers", path)
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		return fmt.Errorf("%s: support updater must support manual dispatch", path)
	}
	schedule, ok := workflow.On["schedule"]
	if !ok {
		return fmt.Errorf("%s: support updater must retain its weekly schedule", path)
	}
	var schedules []struct {
		Cron string `yaml:"cron"`
	}
	if err := schedule.Decode(&schedules); err != nil || len(schedules) != 1 || schedules[0].Cron != "43 3 * * 2" {
		return fmt.Errorf("%s: support updater must retain the audited weekly schedule", path)
	}
	if len(workflow.Defaults) != 0 || len(workflow.Env) != 0 {
		return fmt.Errorf("%s: support updater must not define workflow defaults or environment overrides", path)
	}
	if !equalStringMap(workflow.Permissions, map[string]string{"contents": "read"}) {
		return fmt.Errorf("%s: support updater top-level permissions must be contents: read only", path)
	}
	if workflow.Concurrency.Group != "update-kubernetes-support" || workflow.Concurrency.CancelInProgress {
		return fmt.Errorf("%s: support updater must serialize deliveries without canceling an active run", path)
	}
	if len(workflow.Jobs) != 3 {
		return fmt.Errorf("%s: support updater must contain only prepare, propose, and dispatch jobs", path)
	}

	prepare, err := requireWorkflowJob(path, workflow, "prepare")
	if err != nil {
		return err
	}
	if prepare.Name != "Discover and validate maintained Kubernetes minors" ||
		prepare.If != "" || len(prepare.Needs) != 0 || prepare.RunsOn != "ubuntu-latest" ||
		prepare.TimeoutMinutes != 15 || prepare.Environment != "" ||
		len(prepare.Env) != 0 || len(prepare.Defaults) != 0 {
		return fmt.Errorf("%s: prepare must be an unconditional, isolated 15-minute validation job", path)
	}
	if !equalStringMap(prepare.Permissions, map[string]string{"contents": "read"}) {
		return fmt.Errorf("%s: prepare permissions must be contents: read only", path)
	}
	if !equalStringMap(prepare.Outputs, map[string]string{
		"changed":      "${{ steps.bundle.outputs.changed }}",
		"base-sha":     "${{ steps.bundle.outputs.base-sha }}",
		"patch-base64": "${{ steps.bundle.outputs.patch-base64 }}",
		"patch-sha256": "${{ steps.bundle.outputs.patch-sha256 }}",
	}) {
		return fmt.Errorf("%s: prepare outputs must bind exactly to the validated patch bundle", path)
	}
	if err := verifyNoContinueOnError(path, "prepare", prepare); err != nil {
		return err
	}
	prepareSteps, err := requireWorkflowStepOrder(path, "prepare", prepare, []string{
		"checkout", "setup-go", "discover", "verify", "bundle",
	})
	if err != nil {
		return err
	}
	if err := verifyUpdaterActionStep(path, "prepare", prepareSteps[0],
		"actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
		map[string]string{
			"ref":                 "${{ github.event.repository.default_branch }}",
			"fetch-depth":         "0",
			"persist-credentials": "false",
		}); err != nil {
		return err
	}
	if err := verifyUpdaterActionStep(path, "prepare", prepareSteps[1],
		"actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16",
		map[string]string{
			"go-version-file":       "go.mod",
			"cache-dependency-path": "go.sum",
		}); err != nil {
		return err
	}
	if err := verifyUpdaterRunStep(path, "prepare", prepareSteps[2], map[string]string{
		"GITHUB_TOKEN": "${{ secrets.GITHUB_TOKEN }}",
	}); err != nil {
		return err
	}
	for _, step := range prepareSteps[3:] {
		if err := verifyUpdaterRunStep(path, "prepare", step, nil); err != nil {
			return err
		}
	}

	propose, err := requireWorkflowJob(path, workflow, "propose")
	if err != nil {
		return err
	}
	if propose.Name != "Publish the verified support-window pull request" ||
		propose.If != "needs.prepare.outputs.changed == 'true'" ||
		!equalStringSet(propose.Needs, []string{"prepare"}) || propose.RunsOn != "ubuntu-latest" ||
		propose.TimeoutMinutes != 10 || propose.Environment != "" ||
		len(propose.Env) != 0 || len(propose.Defaults) != 0 {
		return fmt.Errorf("%s: propose must consume only a changed, successful prepare result in a 10-minute job", path)
	}
	if !equalStringMap(propose.Permissions, map[string]string{
		"contents":      "write",
		"pull-requests": "write",
	}) {
		return fmt.Errorf("%s: propose permissions must contain only contents and pull-requests write", path)
	}
	if !equalStringMap(propose.Outputs, map[string]string{
		"pr-number":      "${{ steps.support-window-pr.outputs.pr-number }}",
		"pushed-sha":     "${{ steps.support-window-pr.outputs.pushed-sha }}",
		"support-branch": "${{ steps.support-window-pr.outputs.support-branch }}",
	}) {
		return fmt.Errorf("%s: propose outputs must bind exactly to the revalidated pull request delivery", path)
	}
	if err := verifyNoContinueOnError(path, "propose", propose); err != nil {
		return err
	}
	proposeSteps, err := requireWorkflowStepOrder(path, "propose", propose, []string{
		"checkout", "apply-bundle", "support-window-pr",
	})
	if err != nil {
		return err
	}
	if err := verifyUpdaterActionStep(path, "propose", proposeSteps[0],
		"actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
		map[string]string{
			"ref":                 "${{ needs.prepare.outputs.base-sha }}",
			"fetch-depth":         "0",
			"persist-credentials": "false",
		}); err != nil {
		return err
	}
	if err := verifyUpdaterRunStep(path, "propose", proposeSteps[1], map[string]string{
		"EXPECTED_BASE_SHA": "${{ needs.prepare.outputs.base-sha }}",
		"PATCH_BASE64":      "${{ needs.prepare.outputs.patch-base64 }}",
		"PATCH_SHA256":      "${{ needs.prepare.outputs.patch-sha256 }}",
	}); err != nil {
		return err
	}
	if err := verifyUpdaterRunStep(path, "propose", proposeSteps[2], map[string]string{
		"BASE_BRANCH": "${{ github.event.repository.default_branch }}",
		"BASE_SHA":    "${{ needs.prepare.outputs.base-sha }}",
		"GH_TOKEN":    "${{ secrets.GITHUB_TOKEN }}",
	}); err != nil {
		return err
	}

	dispatch, err := requireWorkflowJob(path, workflow, "dispatch")
	if err != nil {
		return err
	}
	if dispatch.Name != "Dispatch and verify exact-SHA checks" ||
		dispatch.If != "needs.prepare.outputs.changed == 'true' && needs.propose.result == 'success'" ||
		!equalStringSet(dispatch.Needs, []string{"prepare", "propose"}) || dispatch.RunsOn != "ubuntu-latest" ||
		dispatch.TimeoutMinutes != 10 || dispatch.Environment != "" ||
		len(dispatch.Env) != 0 || len(dispatch.Defaults) != 0 || len(dispatch.Outputs) != 0 {
		return fmt.Errorf("%s: dispatch must consume only a successful exact proposal in a 10-minute job", path)
	}
	if !equalStringMap(dispatch.Permissions, map[string]string{
		"actions":       "write",
		"contents":      "read",
		"pull-requests": "read",
	}) {
		return fmt.Errorf("%s: dispatch permissions must contain only actions write plus contents and pull-requests read", path)
	}
	if err := verifyNoContinueOnError(path, "dispatch", dispatch); err != nil {
		return err
	}
	dispatchSteps, err := requireWorkflowStepOrder(path, "dispatch", dispatch, []string{"dispatch-evidence"})
	if err != nil {
		return err
	}
	if err := verifyUpdaterRunStep(path, "dispatch", dispatchSteps[0], map[string]string{
		"BASE_BRANCH":        "${{ github.event.repository.default_branch }}",
		"EXPECTED_BASE_SHA":  "${{ needs.prepare.outputs.base-sha }}",
		"EXPECTED_PR_NUMBER": "${{ needs.propose.outputs.pr-number }}",
		"EXPECTED_SHA":       "${{ needs.propose.outputs.pushed-sha }}",
		"GH_TOKEN":           "${{ secrets.GITHUB_TOKEN }}",
		"SUPPORT_BRANCH":     "${{ needs.propose.outputs.support-branch }}",
	}); err != nil {
		return err
	}
	if err := verifyUpdaterRunDigests(path, prepareSteps, proposeSteps, dispatchSteps); err != nil {
		return err
	}
	return nil
}

func verifyReleaseWorkflow(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		return fmt.Errorf("%s: release smoke must support manual dispatch", path)
	}
	for _, jobName := range []string{"smoke", "support-preflight", "publish"} {
		job, err := requireWorkflowJob(path, workflow, jobName)
		if err != nil {
			return err
		}
		if err := verifyNoContinueOnError(path, jobName, job); err != nil {
			return err
		}
	}

	smoke := workflow.Jobs["smoke"]
	if smoke.If != "github.event_name == 'pull_request' || github.event_name == 'workflow_dispatch'" {
		return fmt.Errorf("%s: release smoke must run for pull requests and manual dispatches", path)
	}

	preflight := workflow.Jobs["support-preflight"]
	if preflight.If != "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')" {
		return fmt.Errorf("%s: support preflight must run only for release tags", path)
	}
	if !equalStringMap(preflight.Permissions, map[string]string{"actions": "read", "contents": "read"}) {
		return fmt.Errorf("%s: support preflight permissions must be actions: read and contents: read", path)
	}
	if len(preflight.Needs) != 0 || preflight.Environment != "" {
		return fmt.Errorf("%s: support preflight must run before and outside the protected release environment", path)
	}
	if preflight.TimeoutMinutes != releasePreflightJobTimeoutMinutes {
		return fmt.Errorf("%s: support preflight timeout must be %d minutes", path, releasePreflightJobTimeoutMinutes)
	}
	if !equalStringMap(preflight.Outputs, map[string]string{
		"chart-sha256":              "${{ steps.support-evidence.outputs.chart-sha256 }}",
		"kubernetes-support-window": "${{ steps.support-evidence.outputs.kubernetes-support-window }}",
		"source-sha":                "${{ steps.support-evidence.outputs.source-sha }}",
		"support-evidence-run-id":   "${{ steps.support-evidence.outputs.support-evidence-run-id }}",
	}) {
		return fmt.Errorf("%s: support preflight must expose its verified source SHA, CI run, support window, and installed chart digest", path)
	}
	evidence, err := requireWorkflowStep(path, "support-preflight", preflight, "support-evidence")
	if err != nil {
		return err
	}
	if !equalStringMap(evidence.Env, map[string]string{
		"DEFAULT_BRANCH":               "${{ github.event.repository.default_branch }}",
		"GH_TOKEN":                     "${{ secrets.GITHUB_TOKEN }}",
		"SUPPORT_POLL_TIMEOUT_MINUTES": strconv.Itoa(releaseSupportPollTimeoutMinutes),
	}) {
		return fmt.Errorf("%s: support preflight must bind the default branch and read-only Actions token", path)
	}
	requiredEvidence := []string{
		"go run ./hack/verify-kubernetes-support.go -now \"$today\"",
		"go run ./hack/releaseverify",
		"actions/workflows/ci.yml/runs",
		"-f branch=\"$DEFAULT_BRANCH\"",
		"-f event=push",
		"-f head_sha=\"$GITHUB_SHA\"",
		".event == \"push\"",
		".head_branch == $branch",
		".head_sha == $sha",
		".conclusion == \"success\"",
		"<<<\"$runs\" > \"$run_ids_file\"",
		"mapfile -t run_ids < \"$run_ids_file\"",
		`[[ "$run_id" =~ ^[1-9][0-9]*$ ]]`,
		"actions/runs/$run_id/jobs",
		".name == \"Kubernetes support gate\"",
		"actions/runs/$evidence_run/artifacts",
		"installed-release-chart-%s\\n",
		`(.minor_slug == (.minor | gsub("\\."; "-")))`,
		"gh run download \"$evidence_run\"",
		"cmp \"$canonical_chart\" \"$chart_path\"",
		`[[ "$evidence_run" =~ ^[1-9][0-9]*$ ]]`,
		"printf 'chart-sha256=%s\\n' \"$chart_sha256\"",
		`kubernetes_support_window="$(jq -er '[.[].minor] | join(",")' <<<"$support_matrix")"`,
		"printf 'kubernetes-support-window=%s\\n' \"$kubernetes_support_window\"",
		"poll_deadline_epoch=$(( $(date -u +%s) + SUPPORT_POLL_TIMEOUT_MINUTES * 60 ))",
		"remaining_seconds=$((poll_deadline_epoch - $(date -u +%s)))",
		"printf 'source-sha=%s\\n' \"$GITHUB_SHA\"",
		"printf 'support-evidence-run-id=%s\\n' \"$evidence_run\"",
		`} >> "$GITHUB_OUTPUT"`,
	}
	for _, marker := range requiredEvidence {
		if !strings.Contains(evidence.Run, marker) {
			return fmt.Errorf("%s: support preflight is missing exact-CI evidence marker %q", path, marker)
		}
	}
	evidenceDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(evidence.Run)))
	if evidenceDigest != releaseSupportEvidenceRunSHA256 {
		return fmt.Errorf("%s: support preflight shell digest %s differs from the audited installed-chart evidence contract", path, evidenceDigest)
	}
	if strings.Contains(evidence.Run, "mapfile -t run_ids < <(jq") {
		return fmt.Errorf("%s: support preflight must check CI-run JSON decoding before polling", path)
	}

	publish := workflow.Jobs["publish"]
	if !equalStringSet(publish.Needs, []string{"support-preflight"}) {
		return fmt.Errorf("%s: publish must depend on support-preflight", path)
	}
	if publish.If != "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v') && needs.support-preflight.outputs.source-sha == github.sha" {
		return fmt.Errorf("%s: publish must bind the preflight source SHA to the tag commit", path)
	}
	chartPackage, err := requireWorkflowStep(path, "publish", publish, "chart-package")
	if err != nil {
		return err
	}
	if chartPackage.If != "" || chartPackage.Uses != "" || chartPackage.Shell != "bash" ||
		chartPackage.WorkingDirectory != "" || len(chartPackage.With) != 0 ||
		!equalStringMap(chartPackage.Env, map[string]string{
			"TESTED_CHART_SHA256": "${{ needs.support-preflight.outputs.chart-sha256 }}",
		}) {
		return fmt.Errorf("%s: release chart package must consume only the exact installed-chart digest", path)
	}
	for _, marker := range []string{
		`chart_sha256="$(sha256sum "$chart_path" | awk '{print $1}')"`,
		`[[ "$TESTED_CHART_SHA256" =~ ^[0-9a-f]{64}$ ]]`,
		`[[ "$chart_sha256" == "$TESTED_CHART_SHA256" ]]`,
	} {
		if !strings.Contains(chartPackage.Run, marker) {
			return fmt.Errorf("%s: release chart package is missing installed-artifact binding %q", path, marker)
		}
	}
	chartPackageDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(chartPackage.Run)))
	if chartPackageDigest != releaseChartPackageRunSHA256 {
		return fmt.Errorf("%s: release chart package shell digest %s differs from the audited installed-artifact binding", path, chartPackageDigest)
	}
	artifacts, err := requireWorkflowStep(path, "publish", publish, "artifacts")
	if err != nil {
		return err
	}
	if artifacts.If != "" || artifacts.Uses != "" || artifacts.Shell != "bash" ||
		artifacts.WorkingDirectory != "" || len(artifacts.With) != 0 ||
		!equalStringMap(artifacts.Env, map[string]string{
			"TESTED_KUBERNETES_SUPPORT_WINDOW": "${{ needs.support-preflight.outputs.kubernetes-support-window }}",
			"TESTED_SUPPORT_EVIDENCE_RUN_ID":   "${{ needs.support-preflight.outputs.support-evidence-run-id }}",
		}) {
		return fmt.Errorf("%s: immutable release manifest must consume only the verified CI run and Kubernetes support window", path)
	}
	for _, marker := range []string{
		`[[ "$TESTED_SUPPORT_EVIDENCE_RUN_ID" =~ ^[1-9][0-9]*$ ]]`,
		`[[ "$TESTED_KUBERNETES_SUPPORT_WINDOW" =~ ^[0-9]+\.[0-9]+(,[0-9]+\.[0-9]+)*$ ]]`,
		`printf 'support-evidence-run-id=%s\n' "$TESTED_SUPPORT_EVIDENCE_RUN_ID"`,
		`printf 'kubernetes-support-window=%s\n' "$TESTED_KUBERNETES_SUPPORT_WINDOW"`,
	} {
		if !strings.Contains(artifacts.Run, marker) {
			return fmt.Errorf("%s: immutable release manifest is missing support evidence binding %q", path, marker)
		}
	}
	return verifyAuditedWorkflowDigest(path, contents, releaseWorkflowSHA256)
}

type workflowDocument struct {
	On          map[string]yaml.Node   `yaml:"on"`
	Concurrency workflowConcurrency    `yaml:"concurrency"`
	Permissions map[string]string      `yaml:"permissions"`
	Env         map[string]string      `yaml:"env"`
	Defaults    map[string]yaml.Node   `yaml:"defaults"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type workflowJob struct {
	Name            string               `yaml:"name"`
	If              string               `yaml:"if"`
	Needs           workflowStringList   `yaml:"needs"`
	RunsOn          string               `yaml:"runs-on"`
	Environment     string               `yaml:"environment"`
	Permissions     map[string]string    `yaml:"permissions"`
	Outputs         map[string]string    `yaml:"outputs"`
	Env             map[string]string    `yaml:"env"`
	Defaults        map[string]yaml.Node `yaml:"defaults"`
	TimeoutMinutes  int                  `yaml:"timeout-minutes"`
	Strategy        workflowStrategy     `yaml:"strategy"`
	ContinueOnError bool                 `yaml:"continue-on-error"`
	Steps           []workflowStep       `yaml:"steps"`
}

type workflowStep struct {
	Name             string            `yaml:"name"`
	ID               string            `yaml:"id"`
	If               string            `yaml:"if"`
	Uses             string            `yaml:"uses"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	Run              string            `yaml:"run"`
	ContinueOnError  bool              `yaml:"continue-on-error"`
}

type workflowStrategy struct {
	FailFast *bool             `yaml:"fail-fast"`
	Matrix   map[string]string `yaml:"matrix"`
}

type workflowStringList []string

func (values *workflowStringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case 0:
		return nil
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
		return fmt.Errorf("workflow needs must be a string or list")
	}
}

func readWorkflow(path string) (workflowDocument, []byte, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return workflowDocument{}, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var workflow workflowDocument
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		return workflowDocument{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return workflow, contents, nil
}

func requireWorkflowJob(path string, workflow workflowDocument, name string) (workflowJob, error) {
	job, ok := workflow.Jobs[name]
	if !ok {
		return workflowJob{}, fmt.Errorf("%s: missing workflow job %q", path, name)
	}
	return job, nil
}

func requireWorkflowStep(path, jobName string, job workflowJob, id string) (workflowStep, error) {
	var match workflowStep
	matches := 0
	for _, step := range job.Steps {
		if step.ID == id {
			match = step
			matches++
		}
	}
	if matches != 1 {
		return workflowStep{}, fmt.Errorf("%s: job %q must contain exactly one step with id %q", path, jobName, id)
	}
	return match, nil
}

func requireWorkflowStepOrder(
	path, jobName string,
	job workflowJob,
	expectedIDs []string,
) ([]workflowStep, error) {
	if len(job.Steps) != len(expectedIDs) {
		return nil, fmt.Errorf("%s: job %q has %d steps, want the audited %d-step sequence", path, jobName, len(job.Steps), len(expectedIDs))
	}
	for index, expectedID := range expectedIDs {
		if job.Steps[index].ID != expectedID {
			return nil, fmt.Errorf("%s: job %q step %d has id %q, want %q", path, jobName, index+1, job.Steps[index].ID, expectedID)
		}
	}
	return job.Steps, nil
}

func verifyUpdaterActionStep(
	path, jobName string,
	step workflowStep,
	expectedUses string,
	expectedWith map[string]string,
) error {
	if step.If != "" || step.Uses != expectedUses || step.Run != "" || step.Shell != "" ||
		step.WorkingDirectory != "" || len(step.Env) != 0 || !equalStringMap(step.With, expectedWith) {
		return fmt.Errorf("%s: job %q step %q must be the unconditional audited action invocation", path, jobName, step.ID)
	}
	return nil
}

func verifyUpdaterRunStep(path, jobName string, step workflowStep, expectedEnv map[string]string) error {
	if step.If != "" || step.Uses != "" || step.Run == "" || step.Shell != "bash" ||
		step.WorkingDirectory != "" || len(step.With) != 0 || !equalStringMap(step.Env, expectedEnv) {
		return fmt.Errorf("%s: job %q step %q must be an unconditional audited bash invocation", path, jobName, step.ID)
	}
	return nil
}

func verifyUpdaterRunDigests(path string, prepareSteps, proposeSteps, dispatchSteps []workflowStep) error {
	steps := []struct {
		key  string
		step workflowStep
	}{
		{key: "prepare/discover", step: prepareSteps[2]},
		{key: "prepare/verify", step: prepareSteps[3]},
		{key: "prepare/bundle", step: prepareSteps[4]},
		{key: "propose/apply-bundle", step: proposeSteps[1]},
		{key: "propose/support-window-pr", step: proposeSteps[2]},
		{key: "dispatch/dispatch-evidence", step: dispatchSteps[0]},
	}
	var mismatches []error
	for _, item := range steps {
		actual := fmt.Sprintf("%x", sha256.Sum256([]byte(item.step.Run)))
		if actual != updateRunSHA256[item.key] {
			mismatches = append(mismatches, fmt.Errorf(
				"%s: updater step %s shell digest %s differs from the audited contract",
				path,
				item.key,
				actual,
			))
		}
	}
	return errors.Join(mismatches...)
}

func verifyAuditedWorkflowDigest(path string, contents []byte, expected string) error {
	actual := fmt.Sprintf("%x", sha256.Sum256(contents))
	if actual != expected {
		return fmt.Errorf("%s: workflow digest %s differs from the audited contract", path, actual)
	}
	return nil
}

func verifyNoContinueOnError(path, jobName string, job workflowJob) error {
	if job.ContinueOnError {
		return fmt.Errorf("%s: job %q must not continue on error", path, jobName)
	}
	for _, step := range job.Steps {
		if step.ContinueOnError {
			return fmt.Errorf("%s: job %q step %q must not continue on error", path, jobName, step.ID)
		}
	}
	return nil
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

func equalStringMap(actual, expected map[string]string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for key, expectedValue := range expected {
		if actual[key] != expectedValue {
			return false
		}
	}
	return true
}

func containsStringMap(actual, expected map[string]string) bool {
	for key, expectedValue := range expected {
		if actual[key] != expectedValue {
			return false
		}
	}
	return true
}

type sourceContractStep struct {
	name    string
	pattern *regexp.Regexp
}

type e2eWiringFiles struct {
	makefile                   string
	harness                    string
	kindConfig                 string
	apiServerEndpointFilter    string
	staticChecks               string
	dataPlane                  string
	assertions                 string
	crdUpgrade                 string
	faults                     string
	highAvailability           string
	certRotation               string
	failedHookEvidence         string
	failedHookEvidenceSelftest string
	admissionSchemaContract    string
	admissionSchemaSelftest    string
	controllerSchemaContract   string
	controllerSchemaSelftest   string
}

type lifecycleSourceContract struct {
	path              string
	exitTrap          string
	steps             []sourceContractStep
	successfulReturns []successfulReturnContract
}

type successfulReturnContract struct {
	start      *regexp.Regexp
	completion *regexp.Regexp
}

const apiServerFeatureGatePatchContract = `append_api_server_feature_gate_patch() {
	feature_gate_minor=$1
	feature_gate_config=$2
	case "$feature_gate_minor" in
	1.35)
		EXPECTED_API_SERVER_FEATURE_GATES=GenericWorkload=true
		{
			printf '%s\n' 'kubeadmConfigPatchesJSON6902:'
			printf '%s\n' '- group: kubeadm.k8s.io'
			printf '%s\n' '  version: v1beta3'
			printf '%s\n' '  kind: ClusterConfiguration'
			printf '%s\n' '  patch: |'
			printf '%s\n' '    - op: add'
			printf '%s\n' '      path: /apiServer/extraArgs/feature-gates'
			printf '%s\n' '      value: GenericWorkload=true'
		} >>"$feature_gate_config"
		;;
	1.36) ;;
	1.37)
		EXPECTED_API_SERVER_FEATURE_GATES=EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true
		{
			printf '%s\n' 'kubeadmConfigPatchesJSON6902:'
			printf '%s\n' '- group: kubeadm.k8s.io'
			printf '%s\n' '  version: v1beta4'
			printf '%s\n' '  kind: ClusterConfiguration'
			printf '%s\n' '  patch: |'
			printf '%s\n' '    - op: add'
			printf '%s\n' '      path: /apiServer/extraArgs/-'
			printf '%s\n' '      value:'
			printf '%s\n' '        name: feature-gates'
			printf '%s\n' '        value: EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true'
		} >>"$feature_gate_config"
		;;
	esac
}`

const apiServerFeatureGateScopeContract = `assert_api_server_feature_gate_scope() {
	expected_api_server_feature_gates=$1
	control_plane_pods_file=$WORK_DIR/control-plane-pods.json
	component_configs_file=$WORK_DIR/component-configs.json
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		-n kube-system get pods -o json >"$control_plane_pods_file"
	jq -e --arg expected "$expected_api_server_feature_gates" --arg cluster "$CLUSTER_NAME" '
      def component_pods($component):
        [.items[] | select(.metadata.labels.component == $component)];
      def command_options($pod; $prefix):
        [$pod.spec.containers[].command[] | select(startswith($prefix))];
      def exact_control_plane_nodes($pods):
        ([$pods[].spec.nodeName] | sort) ==
          ([$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3"] | sort);
      def component_is_ready($pod; $container_name):
        ($pod.metadata.deletionTimestamp == null) and
        (($pod.metadata.annotations["kubernetes.io/config.mirror"] // "") | length) > 0 and
        ($pod.metadata.name == ($container_name + "-" + $pod.spec.nodeName)) and
        ($pod.status.phase == "Running") and
        ([($pod.status.conditions // [])[] | select(.type == "Ready" and .status == "True")] | length) == 1 and
        (($pod.spec.containers // []) | length) == 1 and
        ($pod.spec.containers[0].name == $container_name) and
        (($pod.status.containerStatuses // []) | length) == 1 and
        ($pod.status.containerStatuses[0].name == $container_name) and
        ($pod.status.containerStatuses[0].ready == true) and
        (($pod.status.containerStatuses[0].state.running | type) == "object");

      (component_pods("kube-apiserver")) as $api_servers |
      (component_pods("kube-controller-manager")) as $controller_managers |
      (component_pods("kube-scheduler")) as $schedulers |
      ($api_servers | length) == 3 and
      ($controller_managers | length) == 3 and
      ($schedulers | length) == 3 and
      exact_control_plane_nodes($api_servers) and
      exact_control_plane_nodes($controller_managers) and
      exact_control_plane_nodes($schedulers) and
      all($api_servers[];
        component_is_ready(.; "kube-apiserver") and
        command_options(.; "--feature-gates=") ==
          (if $expected == "" then [] else ["--feature-gates=" + $expected] end) and
        (command_options(.; "--runtime-config=") | length) == 1
      ) and
      all($controller_managers[];
        component_is_ready(.; "kube-controller-manager") and
        command_options(.; "--feature-gates=") == []
      ) and
      all($schedulers[];
        component_is_ready(.; "kube-scheduler") and
        command_options(.; "--feature-gates=") == []
      )
	' "$control_plane_pods_file" >/dev/null ||
		fail "control-plane feature gates are not confined to the API server or kind runtime-config was replaced"
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		-n kube-system get configmaps kubelet-config kube-proxy -o json >"$component_configs_file"
	jq -e '
      (.items | map(select(.metadata.name == "kubelet-config"))) as $kubelet_configs |
      ([.items[].data | to_entries[].value] | join("\n")) as $configs |
      (.items | length) == 2 and
      ($kubelet_configs | length) == 1 and
      (($kubelet_configs[0].data.kubelet // "") | contains("KubeletInUserNamespace: true")) and
      ([
        "EmptyDirVolumeMode",
        "EvictionRequestAPI",
        "GenericWorkload",
        "VolumeBindMountOptions",
        "WorkloadWithJob"
      ] |
      all(.[]; . as $gate | ($configs | contains($gate) | not)))
    ' "$component_configs_file" >/dev/null ||
		fail "API-server-only feature gates leaked into kubelet or kube-proxy configuration"
}`

const kindHATopologyContract = `assert_kind_ha_topology() {
	kind get nodes --name "$CLUSTER_NAME" | LC_ALL=C sort >"$KIND_NODE_INVENTORY_FILE"
	# kind counts the load balancer among a cluster's nodes and Kubernetes
	# does not, which is why this list carries one more name than the node
	# inventory asserted below. Measured on kind v0.31: "kind get nodes" on
	# a two-control-plane cluster returns the balancer as a third line.
	if ! {
		printf '%s\n' \
			"${CLUSTER_NAME}-control-plane" \
			"${CLUSTER_NAME}-control-plane2" \
			"${CLUSTER_NAME}-control-plane3" \
			"${CLUSTER_NAME}-worker" \
			"${CLUSTER_NAME}-external-load-balancer"
	} | LC_ALL=C sort | cmp -s - "$KIND_NODE_INVENTORY_FILE"; then
		fail "kind cluster does not have the exact three-control-plane, one-worker, one-load-balancer topology"
	fi
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		get nodes -o json >"$NODE_READINESS_FILE"
	jq -e --arg cluster "$CLUSTER_NAME" '
      ([.items[].metadata.name] | sort) == ([$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3", $cluster + "-worker"] | sort) and
      ([.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)] | length) == 3 and
      ([.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null)] | length) == 1 and
      all(.items[];
        any((.status.conditions // [])[];
          .type == "Ready" and .status == "True"
        )
      )
    ' "$NODE_READINESS_FILE" >/dev/null ||
		fail "Kubernetes node inventory does not match the ready HA kind topology"
}`

const apiServerEndpointInventoryContract = `assert_api_server_endpoint_inventory() {
	api_endpoint_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$api_endpoint_deadline" ]; do
		if kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
			get nodes -o json >"$NODE_READINESS_FILE" &&
			kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
			-n default get endpointslices \
			-l kubernetes.io/service-name=kubernetes -o json >"$API_SERVER_ENDPOINT_INVENTORY_FILE" &&
			jq -e --arg cluster "$CLUSTER_NAME" --slurpfile nodes "$NODE_READINESS_FILE" \
				-f "$ROOT_DIR/hack/api-server-endpoint-inventory.jq" \
				"$API_SERVER_ENDPOINT_INVENTORY_FILE" >/dev/null &&
			probe_api_server_endpoints; then
			return 0
		fi
		sleep 1
	done
	fail "default Kubernetes Service did not advertise and serve exactly the three control-plane API server endpoints"
}`

const apiServerEndpointProbeContract = `probe_api_server_endpoints() {
	jq -er '
      [.items[]
        | select(.metadata.labels["kubernetes.io/service-name"] == "kubernetes")
        | .endpoints[].addresses[]]
      | sort[]
    ' "$API_SERVER_ENDPOINT_INVENTORY_FILE" >"$API_SERVER_ENDPOINT_ADDRESS_FILE" || return 1
	api_server_endpoint_probe_count=0
	while IFS= read -r api_server_endpoint; do
		api_server_endpoint_probe_count=$((api_server_endpoint_probe_count + 1))
		if ! api_server_readyz=$(docker --context "$DOCKER_CONTEXT" exec \
			"${CLUSTER_NAME}-control-plane" \
			kubectl --kubeconfig /etc/kubernetes/admin.conf \
			--server "https://${api_server_endpoint}:6443" \
			--tls-server-name kubernetes \
			--request-timeout=10s get --raw=/readyz 2>/dev/null); then
			return 1
		fi
		[ "$api_server_readyz" = ok ] || return 1
	done <"$API_SERVER_ENDPOINT_ADDRESS_FILE"
	[ "$api_server_endpoint_probe_count" -eq 3 ]
}`

const apiServerEndpointInventoryFilterContract = `if ($nodes | length) != 1 then false
else
  [$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3"] as $control_plane_names |
  [$nodes[0].items[]
    | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)
    | select(.metadata.name as $name | any($control_plane_names[]; . == $name))
  ] as $control_plane_nodes |
  [$control_plane_nodes[] |
    [(.status.addresses // [])[] | select(.type == "InternalIP") | .address] as $internal_ips |
    select(($internal_ips | length) == 1) |
    $internal_ips[0]
  ] as $control_plane_addresses |
  [.items[] | select(.metadata.labels["kubernetes.io/service-name"] == "kubernetes")] as $slices |
  [$slices[].endpoints[]] as $endpoints |
  [$endpoints[].addresses[]] as $addresses |
  ($control_plane_nodes | length) == 3 and
  ([$control_plane_nodes[].metadata.name] | sort) == ($control_plane_names | sort) and
  ($control_plane_addresses | length) == 3 and
  ($control_plane_addresses | unique | length) == 3 and
  all($control_plane_addresses[]; test("^[0-9]+(\\.[0-9]+){3}$")) and
  ($slices | length) > 0 and
  all($slices[];
    .addressType == "IPv4" and
    (.ports | length) == 1 and
    .ports[0].name == "https" and
    (.ports[0].protocol == null or .ports[0].protocol == "TCP") and
    .ports[0].port == 6443
  ) and
  ($endpoints | length) == 3 and
  all($endpoints[];
    .conditions.ready != false and
    .conditions.serving != false and
    .conditions.terminating != true and
    (.addresses | length) == 1
  ) and
  ($addresses | length) == 3 and
  ($addresses | unique | length) == 3 and
  all($addresses[]; test("^[0-9]+(\\.[0-9]+){3}$")) and
  ($addresses | sort) == ($control_plane_addresses | sort)
end
`

const registryHostsOnKindNodesContract = `configure_registry_hosts_on_kind_nodes() {
	for kind_node_container in \
		"${CLUSTER_NAME}-control-plane" \
		"${CLUSTER_NAME}-control-plane2" \
		"${CLUSTER_NAME}-control-plane3" \
		"${CLUSTER_NAME}-worker"; do
		registry_dns_deadline=$(($(date +%s) + 30))
		registry_dns_ready=0
		while [ "$(date +%s)" -lt "$registry_dns_deadline" ]; do
			if docker --context "$DOCKER_CONTEXT" exec "$kind_node_container" \
				getent ahostsv4 "$REGISTRY_DNS_NAME" 2>/dev/null |
				awk -v expected="$REGISTRY_IP" '$1 == expected {found = 1} END {exit !found}'; then
				registry_dns_ready=1
				break
			fi
			sleep 1
		done
		[ "$registry_dns_ready" -eq 1 ] ||
			fail "registry network alias did not resolve on kind node $kind_node_container"
		docker --context "$DOCKER_CONTEXT" exec "$kind_node_container" \
			mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
		docker --context "$DOCKER_CONTEXT" cp "$REGISTRY_HOSTS_FILE" \
			"${kind_node_container}:/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml"
		if ! docker --context "$DOCKER_CONTEXT" exec "$kind_node_container" \
			cat "/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml" |
			cmp -s "$REGISTRY_HOSTS_FILE" -; then
			fail "registry hosts configuration differs on kind node $kind_node_container"
		fi
	done
}`

func verifyE2EWiring(files e2eWiringFiles) error {
	if err := verifyMakeE2ETarget(files.makefile); err != nil {
		return err
	}
	if err := verifyMakeRaceTargets(files.makefile); err != nil {
		return err
	}
	if err := verifyFailedHookEvidenceAssets(files); err != nil {
		return err
	}
	if err := verifyAdmissionSchemaAssets(files); err != nil {
		return err
	}
	if err := verifyControllerObjectSchemaAssets(files); err != nil {
		return err
	}
	if err := verifyAPIServerEndpointInventoryFilter(files.apiServerEndpointFilter); err != nil {
		return err
	}
	if err := verifyKindHAConfig(files.kindConfig); err != nil {
		return err
	}

	harness := files.harness
	harnessContents, err := os.ReadFile(harness)
	if err != nil {
		return fmt.Errorf("read %s: %w", harness, err)
	}
	if err := verifyShellScriptEntrypoint(harness, harnessContents); err != nil {
		return err
	}
	if err := verifyFailurePreservingExitTrap(harness, harnessContents, "cleanup"); err != nil {
		return err
	}
	harnessContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("required Kubernetes version", `[ -n "$K8S_VERSION" ] || fail "K8S_VERSION is required (for example, 1.37.0)"`),
		exactSourceLine("exact Kubernetes version syntax", `printf '%s\n' "$K8S_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||`),
		exactSourceLineSequence("supported Kubernetes minor binding", []string{
			`K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)`,
			`case "$K8S_MAJOR_MINOR" in`,
			`1.35 | 1.36 | 1.37) ;;`,
			`*) fail "Kubernetes $K8S_MAJOR_MINOR is outside the supported 1.35-1.37 window" ;;`,
			`esac`,
		}),
		exactSourceLine("support-manifest image selection", `if [ -z "$KIND_NODE_IMAGE" ]; then`),
		exactSourceLine("digest-pinned node image", `is_pinned_image "$KIND_NODE_IMAGE" ||`),
		exactSourceLineSequence("node image version binding", []string{
			`case "$KIND_NODE_IMAGE" in`,
			`kindest/node:v"$K8S_VERSION"@sha256:*) ;;`,
			`*) fail "KIND_NODE_IMAGE version does not match K8S_VERSION $K8S_VERSION" ;;`,
			`esac`,
		}),
		exactSourceLineSequence("kind version binding", []string{
			`EXPECTED_KIND_VERSION=$(jq -r '.kindVersion // empty' "$ROOT_DIR/support/kubernetes.json")`,
			`[ -n "$EXPECTED_KIND_VERSION" ] || fail "support manifest does not declare kindVersion"`,
			`ACTUAL_KIND_VERSION=$(kind version | awk '{print $2}')`,
			`[ "$ACTUAL_KIND_VERSION" = "$EXPECTED_KIND_VERSION" ] ||`,
			`fail "kind $EXPECTED_KIND_VERSION is required, got $ACTUAL_KIND_VERSION"`,
		}),
		// 63 is the DNS label limit and 23 is the length of the
		// "-external-load-balancer" container kind gives an HA cluster, a name
		// the harness never spells and so cannot bound by inspection. Docker
		// accepts one byte more than this, which is why the bound is not its
		// hostname limit: the daemon creates the container and CNI cannot
		// resolve it.
		exactSourceLine("bounded HA cluster name", `CLUSTER_NAME=$(dns_name ptah-e2e "$identity" 40)`),
		exactSourceLine("bounded CRD proof namespace", `CRD_PROOF_NAMESPACE=$(dns_name ptah-crd-proof "$identity")`),
		exactSourceLine("runtime generated-name boundary fixture", `RUNTIME_FULLNAME=$(dns_name ptah-runtime-generated-name-prefix-boundary-proof "$identity" 60)`),
		exactSourceLine("runtime generated-name boundary length", `[ "${#RUNTIME_FULLNAME}" -eq 60 ] || fail "runtime fullname boundary fixture must be exactly 60 characters"`),
		exactSourceLine("node readiness snapshot path", `NODE_READINESS_FILE=$WORK_DIR/node-readiness.json`),
		exactSourceLine("kind node inventory path", `KIND_NODE_INVENTORY_FILE=$WORK_DIR/kind-node-inventory.txt`),
		exactSourceLine("API server endpoint inventory path", `API_SERVER_ENDPOINT_INVENTORY_FILE=$WORK_DIR/api-server-endpoints.json`),
		exactSourceLine("API server endpoint address path", `API_SERVER_ENDPOINT_ADDRESS_FILE=$WORK_DIR/api-server-endpoint-addresses.txt`),
		exactSourceLine("daemon-side task claim name", `TASK_CLAIM_VOLUME=$(dns_name ptah-e2e-claim "$identity" 63)`),
		exactSourceLine("daemon-side task claim nonce", `TASK_CLAIM_TOKEN=$(openssl rand -hex 16)`),
		exactSourceLine("daemon-side task claim create latch", `TASK_CLAIM_CREATE_STARTED=0`),
		exactSourceLine("task claim ownership verifier implementation", `task_claim_matches_owner() {`),
		exactSourceLineSequence("task claim exact immutable labels", []string{
			`.["operator.ptah.dev/e2e-owner"] == $owner and`,
			`.["operator.ptah.dev/e2e-component"] == "task-claim" and`,
			`.["operator.ptah.dev/e2e-claim-token"] == $token`,
		}),
		exactSourceLine("task claim acquisition implementation", `acquire_task_claim() {`),
		exactSourceLineSequence("task claim cleanup armed before create", []string{
			`[ "$TASK_CLAIM_CREATE_STARTED" -eq 0 ] || fail "task identity claim acquisition was attempted more than once"`,
			`TASK_CLAIM_CREATE_STARTED=1`,
		}),
		exactSourceLineSequence("atomic daemon-side task claim creation", []string{
			`if ! created_claim=$(docker --context "$DOCKER_CONTEXT" volume create \`,
			`--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \`,
			`--label 'operator.ptah.dev/e2e-component=task-claim' \`,
			`--label "operator.ptah.dev/e2e-claim-token=${TASK_CLAIM_TOKEN}" \`,
			`"$TASK_CLAIM_VOLUME"); then`,
		}),
		exactSourceLineSequence("task claim post-create ownership latch", []string{
			`if ! task_claim_matches_owner; then`,
			`fail "E2E identity $identity is already claimed on Docker context $SELECTED_DOCKER_CONTEXT; choose another E2E_RUN_ID"`,
			`fi`,
			`TASK_CLAIM_ACQUIRED=1`,
		}),
		exactSourceLine("image-audit ownership verifier implementation", `image_audit_container_matches_task() {`),
		exactSourceLineSequence("image-audit exact full-ID labels", []string{
			`.[0].Id == $id and`,
			`.[0].Name == $name and`,
			`.[0].Config.Labels["operator.ptah.dev/e2e-owner"] == $owner and`,
			`.[0].Config.Labels["operator.ptah.dev/e2e-component"] == "image-audit" and`,
			`.[0].Config.Labels["operator.ptah.dev/e2e-claim-token"] == $token`,
		}),
		exactSourceLine("image-audit creation implementation", `create_image_audit_container() {`),
		exactSourceLine("image-audit cleanup armed before create", `IMAGE_AUDIT_CONTAINER_CREATED=1`),
		exactSourceLineSequence("image-audit labeled creation", []string{
			`if ! image_audit_id=$(docker --context "$DOCKER_CONTEXT" create \`,
			`--name "$IMAGE_AUDIT_CONTAINER" \`,
			`--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \`,
			`--label 'operator.ptah.dev/e2e-component=image-audit' \`,
			`--label "operator.ptah.dev/e2e-claim-token=${TASK_CLAIM_TOKEN}" \`,
			`"$image_audit_source"); then`,
		}),
		exactSourceLineSequence("image-audit captured full-ID latch", []string{
			`IMAGE_AUDIT_CONTAINER_ID=$image_audit_id`,
			`image_audit_container_matches_task "$IMAGE_AUDIT_CONTAINER_ID" ||`,
		}),
		exactSourceLine("image-audit removal implementation", `remove_image_audit_container() {`),
		exactSourceLine("image-audit removal by captured ID", `docker --context "$DOCKER_CONTEXT" container rm "$IMAGE_AUDIT_CONTAINER_ID" >/dev/null ||`),
		exactSourceLineSequence("credential-safe node readiness diagnostics", []string{
			`collect_node_readiness_diagnostics() {`,
			`node_diagnostics_context=$1`,
			`printf 'e2e: Kubernetes node readiness diagnostics (%s)\n' \`,
			`"$node_diagnostics_context" >&2`,
			`printf '%s\n' 'e2e: node conditions: name type status reason last-transition' >&2`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s get nodes -o json |`,
			`jq -r '`,
			`.items[] as $node`,
			`| ($node.status.conditions // [])[]`,
			`| [`,
			`$node.metadata.name,`,
			`.type,`,
			`.status,`,
			`(.reason // "-"),`,
			`(.lastTransitionTime // "-")`,
			`]`,
			`| @tsv`,
			`' >&2 || true`,
			`printf '%s\n' 'e2e: recent node warnings: namespace node reason count time' >&2`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s get events -A \`,
			`--field-selector type=Warning -o json |`,
			`jq -r '`,
			`[.items[] | select(.involvedObject.kind == "Node")]`,
			`| sort_by(.eventTime // .lastTimestamp // .metadata.creationTimestamp // "")`,
			`| .[-20:][]`,
			`| [`,
			`(.metadata.namespace // "-"),`,
			`.involvedObject.name,`,
			`(.reason // "-"),`,
			`((.count // 1) | tostring),`,
			`(.eventTime // .lastTimestamp // .metadata.creationTimestamp // "-")`,
			`]`,
			`| @tsv`,
			`' >&2 || true`,
			`}`,
		}),
		exactSourceLineSequence("bounded hard node readiness wait", []string{
			`wait_for_ready_nodes() {`,
			`node_readiness_context=$1`,
			`if ! kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \`,
			`get nodes -o json >"$NODE_READINESS_FILE"; then`,
			`collect_node_readiness_diagnostics "$node_readiness_context"`,
			`return 1`,
			`fi`,
			`if ! jq -e '.items | length == 4' "$NODE_READINESS_FILE" >/dev/null; then`,
			`collect_node_readiness_diagnostics "$node_readiness_context"`,
			`return 1`,
			`fi`,
			`if ! kubectl --kubeconfig "$KUBECONFIG_FILE" wait \`,
			`--for=condition=Ready nodes --all --timeout=2m; then`,
			`collect_node_readiness_diagnostics "$node_readiness_context"`,
			`return 1`,
			`fi`,
			`}`,
		}),
		exactSourceLineSequence("immediate all-node readiness predicate", []string{
			`nodes_ready_now() {`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \`,
			`get nodes -o json >"$NODE_READINESS_FILE" &&`,
			`jq -e '`,
			`((.items | length) == 4) and`,
			`all(.items[];`,
			`any((.status.conditions // [])[];`,
			`.type == "Ready" and .status == "True"`,
			`)`,
			`)`,
			`' "$NODE_READINESS_FILE" >/dev/null`,
			`}`,
		}),
		exactSourceLine("kind HA topology implementation", `assert_kind_ha_topology() {`),
		exactSourceLine("API server endpoint inventory implementation", `assert_api_server_endpoint_inventory() {`),
		exactSourceLine("API server direct endpoint probe implementation", `probe_api_server_endpoints() {`),
		exactSourceLine("all-node registry configuration implementation", `configure_registry_hosts_on_kind_nodes() {`),
		exactSourceLineSequence("hard node readiness requirement", []string{
			`require_ready_nodes() {`,
			`required_readiness_context=$1`,
			`if ! wait_for_ready_nodes "$required_readiness_context"; then`,
			`fail "infrastructure readiness check failed: $required_readiness_context"`,
			`fi`,
			`}`,
		}),
		exactSourceLine("API-server feature gate runtime assertion", `assert_api_server_feature_gate_scope() {`),
		exactSourceLine("task-owned image-audit cleanup latch", `if [ "$IMAGE_AUDIT_CONTAINER_CREATED" -eq 1 ]; then`),
		exactSourceLine("task-owned image-audit cleanup full ID", `image_audit_cleanup_id=$IMAGE_AUDIT_CONTAINER_ID`),
		exactSourceLine("task-owned image-audit cleanup verification", `if ! image_audit_container_matches_task "$image_audit_cleanup_id"; then`),
		exactSourceLine("task-owned image-audit cleanup removal", `elif ! docker --context "$DOCKER_CONTEXT" container rm -f "$image_audit_cleanup_id" >/dev/null 2>&1; then`),
		exactSourceLineSequence("task claim cleanup presence check", []string{
			`if [ "$TASK_CLAIM_CREATE_STARTED" -eq 1 ] &&`,
			`docker --context "$DOCKER_CONTEXT" volume inspect "$TASK_CLAIM_VOLUME" >/dev/null 2>&1; then`,
		}),
		exactSourceLine("task claim cleanup ownership verification", `if task_claim_matches_owner; then`),
		exactSourceLine("task claim cleanup removal", `if ! docker --context "$DOCKER_CONTEXT" volume rm "$TASK_CLAIM_VOLUME" >/dev/null 2>&1; then`),
		exactSourceLine("task claim changed-owner refusal", `elif [ "$TASK_CLAIM_ACQUIRED" -eq 1 ]; then`),
		exactSourceLine("task claim before collision checks", `acquire_task_claim`),
		exactSourceLine("post-claim cluster inventory", `if ! existing_clusters=$(kind get clusters); then`),
		exactSourceLine("post-claim cluster collision refusal", `if printf '%s\n' "$existing_clusters" | grep -Fx "$CLUSTER_NAME" >/dev/null; then`),
		exactSourceLine("API-server feature gate patch implementation", `append_api_server_feature_gate_patch() {`),
		exactSourceLine("API-server feature gate patch call", `append_api_server_feature_gate_patch "$K8S_MAJOR_MINOR" "$KIND_CONFIG"`),
		exactSourceLineSequence("operator image audit by captured ID", []string{
			`create_image_audit_container "$OPERATOR_IMAGE"`,
			`docker --context "$DOCKER_CONTEXT" export "$IMAGE_AUDIT_CONTAINER_ID" >"$IMAGE_AUDIT_ARCHIVE"`,
		}),
		exactSourceLineSequence("fixture image audit by captured ID", []string{
			`create_image_audit_container "$FIXTURE_BUILD_IMAGE"`,
			`docker --context "$DOCKER_CONTEXT" export "$IMAGE_AUDIT_CONTAINER_ID" >"$IMAGE_AUDIT_ARCHIVE"`,
		}),
		exactSourceLineSequence("kind cluster creation", []string{
			`kind create cluster \`,
			`--name "$CLUSTER_NAME" \`,
			`--image "$KIND_NODE_IMAGE" \`,
			`--config "$KIND_CONFIG" \`,
			`--kubeconfig "$KUBECONFIG_FILE" \`,
			`--wait 5m`,
			`require_ready_nodes "after kind cluster creation"`,
			`assert_kind_ha_topology`,
			`assert_api_server_endpoint_inventory`,
		}),
		exactSourceLine("live API-server-only feature gate contract", `assert_api_server_feature_gate_scope "$EXPECTED_API_SERVER_FEATURE_GATES"`),
		exactSourceLineSequence("API server version binding", []string{
			`server_version=$(kubectl --kubeconfig "$KUBECONFIG_FILE" version -o json |`,
			`jq -r '.serverVersion.gitVersion')`,
			`case "$server_version" in`,
			`v"$K8S_VERSION"*) ;;`,
			`*) fail "cluster reports $server_version, expected v$K8S_VERSION" ;;`,
			`esac`,
		}),
		exactSourceLineSequence("live admission OpenAPI boundary", []string{
			`ADMISSION_OPENAPI_FILE=$WORK_DIR/admissionregistration-openapi-v3.json`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw \`,
			`/openapi/v3/apis/admissionregistration.k8s.io/v1 >"$ADMISSION_OPENAPI_FILE"`,
			`jq -e -f "$ROOT_DIR/hack/admission-schema-contract.jq" \`,
			`"$ADMISSION_OPENAPI_FILE" >/dev/null ||`,
			`fail "Kubernetes $K8S_VERSION admission schema exceeds the frozen certificate write boundary"`,
		}),
		exactSourceLineSequence("live controller Job OpenAPI boundary", []string{
			`CONTROLLER_BATCH_OPENAPI_FILE=$WORK_DIR/controller-batch-openapi-v3.json`,
			`CONTROLLER_CORE_OPENAPI_FILE=$WORK_DIR/controller-core-openapi-v3.json`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw \`,
			`/openapi/v3/apis/batch/v1 >"$CONTROLLER_BATCH_OPENAPI_FILE"`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw \`,
			`/openapi/v3/api/v1 >"$CONTROLLER_CORE_OPENAPI_FILE"`,
			`jq -e \`,
			`--arg minor "${server_major}.${server_minor}" \`,
			`--slurpfile core "$CONTROLLER_CORE_OPENAPI_FILE" \`,
			`-f "$ROOT_DIR/hack/controller-object-schema-contract.jq" \`,
			`"$CONTROLLER_BATCH_OPENAPI_FILE" >/dev/null ||`,
			`fail "Kubernetes $K8S_VERSION Job/Pod API exceeds the reviewed controller write boundary"`,
		}),
		exactSourceLine("all-node registry configuration call", `configure_registry_hosts_on_kind_nodes`),
		exactSourceLine("runtime fullname release-values argument", `--arg fullnameOverride "$RUNTIME_FULLNAME" \`),
		exactSourceLine("runtime fullname release-values binding", `fullnameOverride: $fullnameOverride,`),
		exactSourceLineSequence("immediate predecessor install readiness gate", []string{
			`require_ready_nodes "immediately before predecessor Helm install"`,
			`if command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \`,
			`"$PREDECESSOR_BUILD_CONTEXT/$PREDECESSOR_CHART" \`,
			`--namespace "$OPERATOR_NAMESPACE" \`,
			`--create-namespace \`,
			`--wait \`,
			`--timeout 5m \`,
			`--values "$PREDECESSOR_VALUES_FILE"; then`,
			`:`,
			`else`,
			`predecessor_install_status=$?`,
			`if nodes_ready_now; then`,
		}),
		exactSourceLineSequence("post-install-failure readiness classification", []string{
			`fail "predecessor release installation failed while Kubernetes nodes were Ready at the immediate post-failure check (Helm exit $predecessor_install_status)"`,
			`fi`,
			`collect_node_readiness_diagnostics "immediately after predecessor Helm install failed"`,
			`fail "infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)"`,
			`fi`,
		}),
		exactSourceLineSequence("candidate upgrade bounded proof namespace", []string{
			`E2E_PROOF_NAMESPACE=$CRD_PROOF_NAMESPACE \`,
			`E2E_HELM_RELEASE=$HELM_RELEASE \`,
			`E2E_CHART_PACKAGE=$CHART_PACKAGE \`,
			`E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \`,
			`E2E_PREDECESSOR_IDENTITY_FILE=$PREDECESSOR_IDENTITY_FILE \`,
		}),
		exactSourceLineSequence("candidate guarded API version propagation", []string{
			`E2E_KUBERNETES_VERSION=$K8S_VERSION \`,
			`E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \`,
		}),
		exactSourceLineSequence("candidate upgrade lifecycle", []string{
			`E2E_PHASE=upgrade \`,
			`"$ROOT_DIR/hack/e2e-crd-upgrade.sh"`,
		}),
		exactSourceLine("high-availability lifecycle", `"$ROOT_DIR/hack/e2e-ha.sh"`),
		exactSourceLine("control-plane lifecycle", `"$ROOT_DIR/hack/e2e-assert.sh"`),
		exactSourceLine("certificate lifecycle", `"$ROOT_DIR/hack/e2e-cert-rotation.sh"`),
		exactSourceLine("data-plane and OCI lifecycle", `"$ROOT_DIR/hack/e2e-dataplane.sh"`),
		exactSourceLineSequence("uninstall lifecycle", []string{
			`E2E_PROOF_NAMESPACE=$CRD_PROOF_NAMESPACE \`,
			`E2E_HELM_RELEASE=$HELM_RELEASE \`,
			`E2E_CHART_PACKAGE=$CHART_PACKAGE \`,
			`E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \`,
			`E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE \`,
			`E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \`,
			`E2E_NEXT_VALUES_FILE=$NEXT_VALUES_FILE \`,
			`E2E_NEXT_CONTROLLER_IMAGE=$NEXT_CONTROLLER_IMAGE \`,
			`E2E_CURRENT_RELEASE_SEQUENCE=$CURRENT_RELEASE_SEQUENCE \`,
			`E2E_NEXT_RELEASE_SEQUENCE=$NEXT_RELEASE_SEQUENCE \`,
			`E2E_KUBERNETES_VERSION=$K8S_VERSION \`,
			`E2E_PHASE=uninstall \`,
			`"$ROOT_DIR/hack/e2e-crd-upgrade.sh"`,
		}),
		exactSourceLine("post-lifecycle installed chart export", `export_release_chart`),
		exactSourceLine("terminal Kubernetes lifecycle evidence", `printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"`),
	}
	if err := verifyOrderedSourceContract(harness, harnessContents, harnessContract); err != nil {
		return err
	}
	if err := verifyAuditedShellFunctionDigest(
		harness,
		harnessContents,
		"export_release_chart",
		releaseChartExportRunSHA256,
		"exact post-lifecycle installed chart export",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunction(
		harness,
		harnessContents,
		"append_api_server_feature_gate_patch",
		apiServerFeatureGatePatchContract,
	); err != nil {
		return err
	}
	if err := verifyExactShellFunction(
		harness,
		harnessContents,
		"assert_api_server_feature_gate_scope",
		apiServerFeatureGateScopeContract,
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		harness,
		harnessContents,
		"assert_kind_ha_topology",
		kindHATopologyContract,
		"kind HA topology contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		harness,
		harnessContents,
		"assert_api_server_endpoint_inventory",
		apiServerEndpointInventoryContract,
		"API server endpoint inventory contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		harness,
		harnessContents,
		"probe_api_server_endpoints",
		apiServerEndpointProbeContract,
		"API server direct endpoint probe contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		harness,
		harnessContents,
		"configure_registry_hosts_on_kind_nodes",
		registryHostsOnKindNodesContract,
		"all-node registry hosts contract",
	); err != nil {
		return err
	}
	if bytes.Contains(harnessContents, []byte("featureGates:")) {
		return fmt.Errorf("%s: global kind featureGates are forbidden; guarded fields must be enabled only on the API server", harness)
	}
	if count := bytes.Count(harnessContents, []byte("kubeadmConfigPatchesJSON6902:")); count != 2 {
		return fmt.Errorf("%s: expected exactly two versioned API-server feature gate patches, found %d", harness, count)
	}
	for _, functionName := range []string{
		"collect_node_readiness_diagnostics",
		"wait_for_ready_nodes",
		"nodes_ready_now",
		"assert_kind_ha_topology",
		"assert_api_server_endpoint_inventory",
		"probe_api_server_endpoints",
		"configure_registry_hosts_on_kind_nodes",
		"require_ready_nodes",
		"append_api_server_feature_gate_patch",
		"assert_api_server_feature_gate_scope",
		"task_claim_matches_owner",
		"acquire_task_claim",
		"image_audit_container_matches_task",
		"create_image_audit_container",
		"remove_image_audit_container",
	} {
		if err := verifySingleShellFunctionDefinition(harness, harnessContents, functionName); err != nil {
			return err
		}
	}
	if count := bytes.Count(harnessContents, []byte("\nremove_image_audit_container\n")); count != 2 {
		return fmt.Errorf("%s: expected exactly two task-owned image-audit removals, found %d", harness, count)
	}
	if err := verifySingleDirectHelmInstallAttempt(harness, harnessContents); err != nil {
		return err
	}
	if err := rejectStaticControlFlowBypass(harness, harnessContents, harnessContract[len(harnessContract)-1].pattern); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(harness, harnessContents, harnessContract[len(harnessContract)-1].pattern); err != nil {
		return err
	}

	dataPlane := files.dataPlane
	dataPlaneContents, err := os.ReadFile(dataPlane)
	if err != nil {
		return fmt.Errorf("read %s: %w", dataPlane, err)
	}
	if err := verifyShellScriptEntrypoint(dataPlane, dataPlaneContents); err != nil {
		return err
	}
	if err := verifyFailurePreservingExitTrap(dataPlane, dataPlaneContents, "cleanup"); err != nil {
		return err
	}
	dataPlaneContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("private durable Job evidence root", `JOB_EVIDENCE_DIR=$WORK_DIR/job-evidence`),
		exactSourceLineSequence("private durable Job evidence root initialization", []string{
			`: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"`,
			`mkdir "$JOB_EVIDENCE_DIR"`,
			`chmod 700 "$JOB_EVIDENCE_DIR"`,
		}),
		exactSourceLineSequence("credential scanner fail-closed implementation", []string{
			`[ -s "$CREDENTIAL_PATTERNS_FILE" ] ||`,
			`fail "credential scanner has no non-empty protected patterns"`,
			`if grep -F -f "$CREDENTIAL_PATTERNS_FILE" "$scan_file" >/dev/null; then`,
			`fail "a task credential escaped into $scan_context"`,
		}),
		exactSourceLine("operation Pod admission proof implementation", `assert_active_pod_ephemeral_container_rejected() {`),
		exactSourceLineSequence("label-less operation Pod clone", []string{
			`.metadata.labels["app.kubernetes.io/managed-by"],`,
			`.metadata.labels["app.kubernetes.io/component"],`,
		}),
		exactSourceLineSequence("operation Pod create-origin refusal", []string{
			`if k -n "$TEST_NAMESPACE" create --dry-run=server -f "$RESOURCE_FILE" >"$ADMISSION_ERROR_FILE" 2>&1; then`,
			`fail "Pod intent admission allowed a namespace actor to clone active Job Pod $active_pod_name"`,
			`fi`,
		}),
		exactSourceLine("operation Pod create-origin evidence", `printf '%s\n' 'e2e data plane: PASS operation Pod create-origin enforcement'`),
		exactSourceLine("durable Job archive validation implementation", `validate_job_evidence_directory() {`),
		exactSourceLine("durable Job archive private directory validation", `require_mode_0700_directory "$validated_archive" "Job evidence archive"`),
		exactSourceLine("durable Job archive exact entry count", `[ "$archive_entry_count" -eq 5 ] ||`),
		exactSourceLineSequence("durable Job archive exact five-file inventory", []string{
			`"$validated_job_file:Job JSON" \`,
			`"$validated_pod_file:Pod JSON" \`,
			`"$validated_log_file:raw ptah log" \`,
			`"$validated_result_file:normalized result" \`,
			`"$validated_manifest_file:manifest"; do`,
		}),
		exactSourceLine("durable Job archive private file validation", `require_mode_0600_regular_file "$validated_file" \`),
		exactSourceLine("durable Job archive SHA-256 path binding", `.archiveVersion == 1 and .pathKey == $key and`),
		exactSourceLine("durable Job archive schema-operation binding", `.schema == $schema and .operation == $operation and`),
		exactSourceLine("durable Job archive Job identity binding", `.job.uid == $uid and (.job.name | type) == "string" and (.job.name | length) > 0 and`),
		exactSourceLineSequence("durable Job archive schema-owner manifest binding", []string{
			`.job.owner.apiVersion == "operator.ptah.dev/v1alpha1" and`,
			`.job.owner.kind == "PtahSchema" and .job.owner.name == $schema and`,
			`(.job.owner.uid | type) == "string" and (.job.owner.uid | length) > 0 and`,
			`($expectedSchemaUID == "" or .job.owner.uid == $expectedSchemaUID) and`,
			`.job.owner.controller == true and`,
		}),
		exactSourceLine("durable Job archive Pod owner binding", `.pod.owner.name == .job.name and .pod.owner.uid == .job.uid and`),
		exactSourceLine("durable Job archive object digest binding", `.digests.jobSHA256 == $jobDigest and .digests.podSHA256 == $podDigest and`),
		exactSourceLine("durable Job archive transport digest binding", `.digests.rawLogSHA256 == $logDigest and .digests.resultSHA256 == $resultDigest`),
		exactSourceLine("durable Job archive Job label operation-ID binding", `.spec.template.metadata.labels["operator.ptah.dev/operation-id"] == $operationLabel and`),
		exactSourceLine("durable Job archive Job annotation operation-ID binding", `.spec.template.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and`),
		exactSourceLineSequence("durable Job archive exact schema ownerReference", []string{
			`([.metadata.ownerReferences[]? | select(`,
			`.apiVersion == "operator.ptah.dev/v1alpha1" and .kind == "PtahSchema" and`,
			`.name == $schema and .uid == $schemaUID and .controller == true)] | length) == 1 and`,
		}),
		exactSourceLine("durable Job archive Job completion contract", `.spec.podReplacementPolicy == "Failed" and .spec.backoffLimit == 0 and`),
		exactSourceLineSequence("durable Job archive Pod identity binding", []string{
			`.metadata.uid == $podUID and .metadata.name == $podName and`,
			`.metadata.generateName == ($jobName + "-") and`,
			`.metadata.labels["operator.ptah.dev/schema"] == $schema and`,
			`.metadata.labels["operator.ptah.dev/operation"] == $operation and`,
			`.metadata.labels["operator.ptah.dev/operation-id"] == $operationLabel and`,
			`.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and`,
		}),
		exactSourceLineSequence("durable Job archive normalized result binding", []string{
			`.protocolVersion == 5 and .operation == $operation and`,
			`.operationId == $operationID and .truncation == null`,
			`' "$validated_result_file" >/dev/null ||`,
		}),
		exactSourceLineSequence("durable Job archive complete credential scan", []string{
			`"$validated_job_file:exact archived Job JSON" \`,
			`"$validated_pod_file:exact archived Pod JSON" \`,
			`"$validated_log_file:archived raw ptah log" \`,
			`"$validated_result_file:archived normalized result" \`,
			`"$validated_manifest_file:archived evidence manifest"; do`,
			`scan_file_for_credentials "${validated_material%%:*}" "${validated_material#*:}"`,
		}),
		exactSourceLine("supplied Job evidence identity validation implementation", `validate_supplied_job_evidence_identity() {`),
		exactSourceLineSequence("supplied Job evidence exact Job identity binding", []string{
			`$job.metadata.uid == $jobUID and $job.metadata.name == $jobName and`,
			`$job.metadata.labels["operator.ptah.dev/schema"] == $schema and`,
			`$job.metadata.labels["operator.ptah.dev/operation"] == $operation and`,
			`$job.metadata.labels["operator.ptah.dev/operation-id"] == $operationLabel and`,
			`$job.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and`,
		}),
		exactSourceLineSequence("supplied Job evidence exact schema ownerReference", []string{
			`([$job.metadata.ownerReferences[]? | select(`,
			`.apiVersion == "operator.ptah.dev/v1alpha1" and .kind == "PtahSchema" and`,
			`.name == $schema and .uid == $schemaUID and .controller == true)] | length) == 1 and`,
		}),
		exactSourceLineSequence("supplied Job evidence exact Pod identity binding", []string{
			`$pod.metadata.uid == $podUID and $pod.metadata.name == $podName and`,
			`$pod.metadata.generateName == ($jobName + "-") and`,
			`$pod.metadata.labels["operator.ptah.dev/schema"] == $schema and`,
			`$pod.metadata.labels["operator.ptah.dev/operation"] == $operation and`,
			`$pod.metadata.labels["operator.ptah.dev/operation-id"] == $operationLabel and`,
			`$pod.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and`,
		}),
		exactSourceLineSequence("supplied Job evidence exact Pod owner binding", []string{
			`([$pod.metadata.ownerReferences[]? | select(`,
			`.apiVersion == "batch/v1" and .kind == "Job" and`,
			`.uid == $jobUID and .name == $jobName and .controller == true)] | length) == 1`,
		}),
		exactSourceLine("existing Job evidence collision validation implementation", `assert_existing_job_evidence_matches_supplied() {`),
		exactSourceLineSequence("existing Job evidence schema-operation-UID validation", []string{
			`validate_job_evidence_directory "$existing_archive" \`,
			`"$existing_schema" "$existing_operation" "$existing_job_uid" \`,
			`"$existing_schema_uid"`,
		}),
		exactSourceLineSequence("existing Job evidence supplied identity comparison", []string{
			`if [ "$VALIDATED_JOB_EVIDENCE_SCHEMA_UID" != "$existing_schema_uid" ] ||`,
			`[ "$VALIDATED_JOB_EVIDENCE_OPERATION_ID" != "$existing_operation_id" ] ||`,
			`[ "$VALIDATED_JOB_EVIDENCE_JOB_NAME" != "$existing_job_name" ] ||`,
			`[ "$VALIDATED_JOB_EVIDENCE_POD_UID" != "$existing_pod_uid" ] ||`,
			`[ "$VALIDATED_JOB_EVIDENCE_POD_NAME" != "$existing_pod_name" ]; then`,
		}),
		exactSourceLine("durable Job archive publication implementation", `publish_completed_job_evidence() {`),
		exactSourceLine("durable Job archive supplied UID-bounded log", `publish_log_file=$3`),
		exactSourceLine("durable Job archive supplied log private-file validation", `require_mode_0600_regular_file "$publish_log_file" "supplied UID-bounded ptah log"`),
		exactSourceLineSequence("durable Job archive supplied schema-owner UID extraction", []string{
			`if ! publish_schema_uid=$(jq -er \`,
			`--arg schema "$publish_schema" '`,
			`[.metadata.ownerReferences[]? | select(`,
			`.apiVersion == "operator.ptah.dev/v1alpha1" and .kind == "PtahSchema" and`,
			`.name == $schema and .controller == true and`,
			`(.uid | type) == "string" and (.uid | length) > 0)] |`,
		}),
		exactSourceLineSequence("durable Job archive supplied identity validation", []string{
			`validate_supplied_job_evidence_identity \`,
			`"$publish_job_file" "$publish_pod_file" \`,
			`"$publish_schema" "$publish_schema_uid" \`,
			`"$publish_operation" "$publish_operation_id" \`,
			`"$publish_job_uid" "$publish_job_name" "$publish_pod_uid" "$publish_pod_name"`,
		}),
		exactSourceLineSequence("existing durable Job archive exact identity acceptance", []string{
			`assert_existing_job_evidence_matches_supplied \`,
			`"$publish_archive" "$publish_schema" "$publish_schema_uid" \`,
			`"$publish_operation" \`,
			`"$publish_operation_id" "$publish_job_uid" "$publish_job_name" \`,
			`"$publish_pod_uid" "$publish_pod_name"`,
			`return 0`,
		}),
		exactSourceLine("durable Job archive private staging", `publish_stage=$(mktemp -d "$JOB_EVIDENCE_DIR/.${publish_key}.XXXXXX") ||`),
		exactSourceLine("durable Job archive private staging mode", `chmod 700 "$publish_stage"`),
		exactSourceLine("durable Job archive UID-bounded log copy", `cp "$publish_log_file" "$publish_stage/ptah.log" ||`),
		exactSourceLineSequence("durable Job archive private staged file modes", []string{
			`chmod 600 "$publish_stage/job.json" "$publish_stage/pod.json" \`,
			`"$publish_stage/ptah.log" "$publish_stage/result.json"`,
		}),
		exactSourceLineSequence("durable Job archive persisted schema-owner binding", []string{
			`owner: {`,
			`apiVersion: "operator.ptah.dev/v1alpha1",`,
			`kind: "PtahSchema",`,
			`uid: $schemaUID,`,
			`name: $schema,`,
			`controller: true`,
		}),
		exactSourceLine("durable Job archive manifest-last staging", `' >"$publish_stage/manifest.json"`),
		exactSourceLine("durable Job archive private manifest mode", `chmod 600 "$publish_stage/manifest.json"`),
		exactSourceLine("durable Job archive staged validation", `validate_job_evidence_directory "$publish_stage" \`),
		exactSourceLineSequence("atomic durable Job archive rename and validation", []string{
			`mv "$publish_stage" "$publish_archive" ||`,
			`fail "could not atomically publish Job evidence archive $publish_key"`,
			`validate_job_evidence_directory "$publish_archive" \`,
		}),
		exactSourceLine("durable Job evidence live consistency implementation", `assert_live_job_evidence_consistent() {`),
		exactSourceLineSequence("durable Job evidence exact live Job read", []string{
			`if live_evidence_job=$(k -n "$TEST_NAMESPACE" get job "$live_evidence_job_name" \`,
			`-o json --ignore-not-found 2>"$LIVE_JOB_EVIDENCE_ERROR_FILE"); then`,
		}),
		exactSourceLineSequence("durable Job evidence fail-closed live Job API error", []string{
			`: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"`,
			`fail "live Job consistency read failed before exact GC absence could be established"`,
			`fi`,
			`: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"`,
			`if [ -n "$live_evidence_job" ]; then`,
		}),
		exactSourceLine("durable Job evidence exact live Job identity", `.metadata.name == $name and .metadata.uid == $uid and`),
		exactSourceLineSequence("durable Job evidence exact live Pod read", []string{
			`if live_evidence_pod=$(k -n "$TEST_NAMESPACE" get pod "$live_evidence_pod_name" \`,
			`-o json --ignore-not-found 2>"$LIVE_JOB_EVIDENCE_ERROR_FILE"); then`,
		}),
		exactSourceLineSequence("durable Job evidence fail-closed live Pod API error", []string{
			`: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"`,
			`fail "live Pod consistency read failed before exact GC absence could be established"`,
			`fi`,
			`: >"$LIVE_JOB_EVIDENCE_ERROR_FILE"`,
			`if [ -n "$live_evidence_pod" ]; then`,
		}),
		exactSourceLine("durable Job evidence exact live Pod identity", `.metadata.name == $podName and .metadata.uid == $podUID and`),
		exactSourceLineSequence("durable Job evidence exact live Pod owner", []string{
			`([.metadata.ownerReferences[]? | select(`,
			`.apiVersion == "batch/v1" and .kind == "Job" and`,
			`.uid == $jobUID and .name == $jobName and .controller == true)] | length) == 1`,
		}),
		exactSourceLineSequence("durable Job archive UID-bounded audited log capture", []string{
			`if [ "$audit_managed_complete" -eq 1 ] && [ "$audit_container" = ptah ]; then`,
			`cp "$LOG_FILE" "$audit_evidence_log_file" ||`,
			`fail "could not retain UID-bounded ptah logs for exact Pod $audit_pod_name UID $audit_pod_uid"`,
			`chmod 600 "$audit_evidence_log_file"`,
		}),
		exactSourceLineSequence("durable Job archive post-log exact Pod UID check", []string{
			`audit_pod_after=$(k -n "$TEST_NAMESPACE" get pod "$audit_pod_name" -o json 2>/dev/null) ||`,
			`fail "exact Pod $audit_pod_name UID $audit_pod_uid disappeared during its log audit"`,
			`printf '%s\n' "$audit_pod_after" | jq -e \`,
			`--arg podUID "$audit_pod_uid" \`,
			`--arg jobUID "$audit_uid" '`,
			`.metadata.uid == $podUID and`,
		}),
		exactSourceLineSequence("durable Job evidence publication before full-audit ledger", []string{
			`publish_completed_job_evidence \`,
			`"$audit_job_evidence_file" "$audit_pod_evidence_file" \`,
			`"$audit_evidence_log_file"`,
		}),
		exactSourceLine("full-audit ledger commit after durable Job evidence", `printf '%s\n' "$audit_uid" >>"$FULLY_AUDITED_JOBS_FILE"`),
		exactSourceLine("automatic external PostgreSQL post-capture Job-boundary implementation", `assert_schema_job_boundary_unchanged() {`),
		exactSourceLineSequence("automatic external PostgreSQL post-capture exact Job-boundary equality", []string{
			`($expected[0] | length) == $expectedCount and`,
			`($actual | length) == $expectedCount and`,
			`$actual == $expected[0]`,
		}),
		exactSourceLine("operation Pod generated-name binding", `[ "$CAPTURED_POD_GENERATE_NAME" = "${CAPTURED_JOB_NAME}-" ] ||`),
		exactSourceLineSequence("selected Job archived result consumption", []string{
			`validate_completed_job_evidence \`,
			`"$selected_schema" "$selected_operation" "$selected_uid"`,
		}),
		exactSourceLine("selected Job archived result copy", `cp "$VALIDATED_JOB_EVIDENCE_DIR/result.json" "$selected_output" ||`),
		exactSourceLine("selected Job archive path retention", `CAPTURED_JOB_EVIDENCE_DIR=$VALIDATED_JOB_EVIDENCE_DIR`),
		exactSourceLine("selected Job optional live consistency check", `assert_live_job_evidence_consistent \`),
		exactSourceLine("operation Job generated-name boundary fixture", `EXTERNAL_PG_SCHEMA=e2e-postgresql-external-longpod`),
		exactSourceLine("explicit optional apply-policy input", `resource_apply=${11:-}`),
		exactSourceLine("explicit apply-policy serialization", `} + if $apply == "" then {} else {apply: $apply} end),`),
		exactSourceLineSequence("safe-default persistence proof", []string{
			`k -n "$TEST_NAMESPACE" get ptahschema "$resource_schema" -o json |`,
			`jq -e '`,
			`.spec.policy.apply == "OnApproval" and`,
			`.spec.policy.allowDestructive == false`,
			`' >/dev/null || fail "$resource_schema did not persist the safe apply-policy defaults"`,
		}),
		exactSourceLine("immutable plan-storage proof implementation", `assert_plan_storage_immutable() {`),
		exactSourceLine("immutable plan-storage proof call", `assert_plan_storage_immutable "$plan_schema" "$CURRENT_PLAN" "$CURRENT_PLAN_UID"`),
		exactSourceLine("external PostgreSQL lifecycle implementation", `run_external_postgresql_lifecycle() {`),
		exactSourceLine("external OCI publication reference", `external_publish_reference="oci://${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000/schemas/postgresql-external:stable"`),
		exactSourceLineSequence("external OCI publication", []string{
			`external_digest=$(publish_schema postgresql-external v1 postgres "$external_publish_reference" \`,
			`"$ROOT_DIR/testdata/e2e/postgresql-v1.sql")`,
		}),
		exactSourceLine("external digest-selected OCI source", `external_reference="${external_publish_reference%:stable}@${external_digest}"`),
		exactSourceLineSequence("external lifecycle explicit Always source call", []string{
			`create_schema_resource "$EXTERNAL_PG_SCHEMA" PostgreSQL "$EXTERNAL_PG_SECRET" \`,
			`"$external_reference" "$EXTERNAL_PG_COORDINATION_KEY" \`,
			`e2e-verification-policy "$REGISTRY_AUTH_SECRET" Environment 45s "$QUIESCENT_INTERVAL" Always`,
		}),
		exactSourceLineSequence("external automatic lifecycle assertion call", []string{
			`assert_automatic_external_postgresql_lifecycle \`,
			`"$EXTERNAL_PG_SCHEMA" "$EXTERNAL_PG_SECRET" "$external_reference" \`,
			`"$external_digest" "$EXTERNAL_PG_COORDINATION_KEY" \`,
			`"$external_coordination_digest" "$external_before"`,
		}),
		exactSourceLineSequence("operation generated-name boundary proof", []string{
			`[ "${#CAPTURED_JOB_NAME}" -eq 58 ] ||`,
			`fail "external PostgreSQL plan Job did not reach the generated-name truncation boundary"`,
			`[ "${#CAPTURED_POD_GENERATE_NAME}" -eq 59 ] ||`,
			`fail "external PostgreSQL plan Pod generateName did not cross the truncation boundary"`,
			`[ "${#CAPTURED_POD_NAME}" -eq 63 ] ||`,
			`fail "external PostgreSQL plan Pod did not preserve the bounded generated name"`,
		}),
		exactSourceLineSequence("external PostgreSQL post-suspension durable Job-boundary proof", []string{
			`external_suspended_observed_uids_file="$WORK_DIR/${EXTERNAL_PG_SCHEMA}-automatic-suspended-observed-uids.json"`,
			`record_observed_jobs`,
			`assert_schema_job_boundary_unchanged \`,
			`"$EXTERNAL_PG_SCHEMA" "$external_before" \`,
			`"$automatic_observed_uids_file" 7 \`,
			`"$external_suspended_observed_uids_file"`,
		}),
		exactSourceLine("external per-lifecycle evidence", `printf '%s\n' 'e2e data plane: PASS external PostgreSQL bridge lifecycle'`),
		exactSourceLine("OCI lifecycle implementation", `run_engine_lifecycle() {`),
		exactSourceLine("OCI reference construction", `lifecycle_reference="oci://${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000/schemas/${lifecycle_slug}:stable"`),
		exactSourceLine("OCI publication", `digest_v1=$(publish_schema "$lifecycle_slug" v1 "$lifecycle_dialect" "$lifecycle_reference")`),
		exactSourceLine("per-engine lifecycle evidence", `printf 'e2e data plane: PASS %s lifecycle\n' "$lifecycle_engine"`),
		exactSourceLine("data-plane lifecycle entry", `printf '%s\n' 'e2e data plane: creating registry endpoint and isolated databases'`),
		exactSourceLine("registry fixture", `create_registry_service`),
		exactSourceLine("authenticated OCI fixture", `create_authenticated_tls_proxy`),
		exactSourceLine("PostgreSQL lifecycle", `run_engine_lifecycle postgresql PostgreSQL postgres "$PG_SECRET"`),
		exactSourceLine("external PostgreSQL lifecycle", `run_external_postgresql_lifecycle`),
		exactSourceLine("MySQL lifecycle", `run_engine_lifecycle mysql MySQL mysql "$MYSQL_SECRET"`),
		exactSourceLine("fault lifecycle", `"$ROOT_DIR/hack/e2e-faults.sh"`),
		exactSourceLine("audited operation evidence", `assert_observed_jobs_audited`),
		exactSourceLine("terminal data-plane lifecycle evidence", `printf '%s\n' 'e2e data plane: PASS PostgreSQL, external PostgreSQL, MySQL, OCI, restart, and fault lifecycle'`),
	}
	if err := verifyOrderedSourceContract(dataPlane, dataPlaneContents, dataPlaneContract); err != nil {
		return err
	}
	automaticFunctionPattern := regexp.MustCompile(
		`(?ms)^assert_automatic_external_postgresql_lifecycle\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`,
	)
	automaticFunctionMatches := automaticFunctionPattern.FindAll(dataPlaneContents, -1)
	if len(automaticFunctionMatches) != 1 {
		return fmt.Errorf(
			"%s: automatic external PostgreSQL lifecycle must have exactly one auditable function body, found %d",
			dataPlane,
			len(automaticFunctionMatches),
		)
	}
	automaticContract := []sourceContractStep{
		exactSourceLine("automatic external PostgreSQL lifecycle implementation", `assert_automatic_external_postgresql_lifecycle() {`),
		exactSourceLine("automatic external PostgreSQL convergence wait", `wait_for_schema "$automatic_schema" \`),
		exactSourceLine("automatic external PostgreSQL explicit policy evidence", `.spec.policy.apply == "Always" and`),
		exactSourceLine("automatic external PostgreSQL approval-free status evidence", `.type == "ApprovalRequired" and .status == "False" and .reason == "Satisfied")) and`),
		exactSourceLine("automatic external PostgreSQL durable Job-ledger refresh", `record_observed_jobs`),
		exactSourceLineSequence("automatic external PostgreSQL archived Job materialization", []string{
			`materialize_archived_schema_jobs "$automatic_schema" "$automatic_before" 7 \`,
			`"$automatic_observed_uids_file" "$automatic_jobs_file"`,
		}),
		exactSourceLine("automatic external PostgreSQL durable Job-ledger binding", `--slurpfile observed "$automatic_observed_uids_file" \`),
		exactSourceLineSequence("automatic external PostgreSQL exact Job history", []string{
			`($observed[0] | type) == "array" and`,
			`($observed[0] | length) == 7 and`,
			`($jobs | length) == 7 and`,
			`([$jobs[].metadata.uid] | unique | length) == 7 and`,
			`([$jobs[].metadata.uid] | unique | sort) == $observed[0] and`,
			`($resolve | length) == 1 and ($verify | length) == 1 and`,
			`($observe | length) == 2 and ($plan | length) == 2 and`,
			`($apply | length) == 1 and`,
		}),
		exactSourceLineSequence("automatic external PostgreSQL serialized Job order", []string{
			`$resolve[0].status.completionTime <= $verify[0].status.startTime and`,
			`$verify[0].status.completionTime <= $observe[0].status.startTime and`,
			`$observe[0].status.completionTime <= $plan[0].status.startTime and`,
			`$plan[0].status.completionTime <= $apply[0].status.startTime and`,
			`$apply[0].status.completionTime <= $observe[1].status.startTime and`,
			`$observe[1].status.completionTime <= $plan[1].status.startTime`,
		}),
		exactSourceLine("automatic external PostgreSQL Resolve result capture", `capture_selected_job_result "$automatic_schema" resolve "$automatic_resolve_uid" \`),
		exactSourceLine("automatic external PostgreSQL Verify result capture", `capture_selected_job_result "$automatic_schema" verify "$automatic_verify_uid" \`),
		exactSourceLine("automatic external PostgreSQL initial Observe result capture", `capture_selected_job_result "$automatic_schema" observe "$automatic_initial_observe_uid" \`),
		exactSourceLine("automatic external PostgreSQL initial Plan result capture", `capture_selected_job_result "$automatic_schema" plan "$automatic_initial_plan_uid" \`),
		exactSourceLine("automatic external PostgreSQL changed Plan evidence", `.planOutcome == "Changes" and (.stdout | length) > 0 and`),
		exactSourceLine("automatic external PostgreSQL additive DDL evidence", `any(.statements[]; .sql | test("\\bCREATE[[:space:]]+TABLE\\b"; "i")) and`),
		exactSourceLine("automatic external PostgreSQL immutable Plan evidence", `assert_plan_storage_immutable "$automatic_schema" "$automatic_plan_name" "$automatic_plan_uid"`),
		exactSourceLine("automatic external PostgreSQL Apply result capture", `capture_selected_job_result "$automatic_schema" apply "$automatic_apply_uid" \`),
		exactSourceLine("automatic external PostgreSQL captured Apply Job UID binding", `[ "$CAPTURED_JOB_UID" = "$automatic_apply_uid" ] ||`),
		exactSourceLine("automatic external PostgreSQL archived Apply workload evidence", `cp "$CAPTURED_JOB_EVIDENCE_DIR/job.json" "$automatic_apply_job_file" ||`),
		exactSourceLine("automatic external PostgreSQL archived Apply Pod evidence", `cp "$CAPTURED_JOB_EVIDENCE_DIR/pod.json" "$automatic_apply_pod_file" ||`),
		exactSourceLineSequence("automatic external PostgreSQL Apply annotation bindings", []string{
			`.["operator.ptah.dev/plan-fingerprint"] == $planFingerprint and`,
			`.["operator.ptah.dev/plan-content-digest"] == $contentDigest and`,
			`.["operator.ptah.dev/execution-binding-id"] == $executionBinding;`,
		}),
		exactSourceLine("automatic external PostgreSQL Apply runner image binding", `select(.name == "install-runner" and .image == $runnerImage)] | length) == 1 and`),
		exactSourceLine("automatic external PostgreSQL Apply executor image binding", `select(.name == "ptah" and .image == $executorImage)] | length) == 1 and`),
		exactSourceLineSequence("automatic external PostgreSQL Apply database-engine binding", []string{
			`select(.name == "PTAH_EXPECTED_DATABASE_ENGINE" and`,
			`.value == "PostgreSQL" and (.valueFrom // null) == null)] | length) == 1;`,
		}),
		exactSourceLine("automatic external PostgreSQL Apply Job object UID binding", `$job.metadata.name == $jobName and $job.metadata.uid == $jobUID and`),
		exactSourceLineSequence("automatic external PostgreSQL Apply Job and Pod-template identity", []string{
			`($job.metadata.annotations | exact_annotations) and`,
			`($job.spec.template.metadata.annotations | exact_annotations) and`,
			`($job.spec.template.spec | exact_runtime_spec) and`,
		}),
		exactSourceLine("automatic external PostgreSQL Apply Pod UID identity", `$pod.metadata.name == $podName and $pod.metadata.uid == $podUID and`),
		exactSourceLine("automatic external PostgreSQL Apply Pod annotation identity", `($pod.metadata.annotations | exact_annotations) and`),
		exactSourceLine("automatic external PostgreSQL Apply Pod runtime identity", `($pod.spec | exact_runtime_spec)`),
		exactSourceLine("automatic external PostgreSQL mutation evidence", `(.mutationStarted // false) == true and`),
		exactSourceLine("automatic external PostgreSQL final Observe result capture", `capture_selected_job_result "$automatic_schema" observe "$automatic_final_observe_uid" \`),
		exactSourceLine("automatic external PostgreSQL convergence evidence", `.observedDialect == "postgres" and (.observedDrift // false) == false and`),
		exactSourceLine("automatic external PostgreSQL final Plan result capture", `capture_selected_job_result "$automatic_schema" plan "$automatic_final_plan_uid" \`),
		exactSourceLine("automatic external PostgreSQL no-change Plan evidence", `.planOutcome == "NoChanges" and (.planContentDigest // "") == "" and`),
		exactSourceLine("automatic external PostgreSQL approval-object absence", `k -n "$TEST_NAMESPACE" get ptahschemaapprovals -o json |`),
		exactSourceLine("automatic external PostgreSQL approval-event absence", `k -n "$TEST_NAMESPACE" get events -o json |`),
		exactSourceLineSequence("automatic external PostgreSQL archived isolation", []string{
			`assert_job_isolation "$automatic_schema" "$automatic_secret" true \`,
			`"$automatic_jobs_file"`,
		}),
		exactSourceLine("automatic external PostgreSQL terminal evidence", `printf '%s\n' 'e2e data plane: PASS automatic safe-plan PostgreSQL lifecycle'`),
	}
	if err := verifyOrderedSourceContract(
		dataPlane+" automatic external PostgreSQL lifecycle",
		automaticFunctionMatches[0],
		automaticContract,
	); err != nil {
		return err
	}
	if err := rejectStaticControlFlowBypass(dataPlane, dataPlaneContents, dataPlaneContract[len(dataPlaneContract)-1].pattern); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		dataPlane,
		dataPlaneContents,
		sourceLinePattern(`run_external_postgresql_lifecycle() {`),
		sourceLinePattern(`printf '%s\n' 'e2e data plane: PASS external PostgreSQL bridge lifecycle'`),
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		dataPlane,
		dataPlaneContents,
		sourceLinePattern(`assert_automatic_external_postgresql_lifecycle() {`),
		sourceLinePattern(`printf '%s\n' 'e2e data plane: PASS automatic safe-plan PostgreSQL lifecycle'`),
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		dataPlane,
		dataPlaneContents,
		sourceLinePattern(`run_engine_lifecycle() {`),
		sourceLinePattern(`printf 'e2e data plane: PASS %s lifecycle\n' "$lifecycle_engine"`),
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(dataPlane, dataPlaneContents, dataPlaneContract[len(dataPlaneContract)-1].pattern); err != nil {
		return err
	}
	if err := verifyFailedUpgradeEvidenceSource(files.crdUpgrade); err != nil {
		return err
	}

	childContracts := []lifecycleSourceContract{
		{
			path:     files.assertions,
			exitTrap: "cleanup_files",
			steps: []sourceContractStep{
				exactSourceLine("fail-fast shell mode", "set -eu"),
				exactSourceLine("cleanup implementation", `cleanup_files() {`),
				exactSourceLine("cleanup status capture", `status=$?`),
				exactSourceLine("cleanup status preservation", `exit "$status"`),
				exactSourceLine("Pod admission outage proof", `printf '%s\n' 'e2e assertions: checking Pod webhook outage scope and foreign-label refusal'`),
				exactSourceLineSequence("safe-default persistence proof", []string{
					`printf '%s\n' "$schema_object" | jq -e '`,
					`.spec.interval == "10m" and`,
					`.spec.policy.apply == "OnApproval" and`,
					`.spec.policy.allowDestructive == false and`,
					`.spec.policy.driftSeverity == "all" and`,
					`.spec.policy.lockTimeout == "30s" and`,
					`.spec.policy.transactionMode == "file" and`,
					`.spec.execution.activeDeadlineSeconds == 900 and`,
					`.spec.execution.failureRetryInterval == "30s" and`,
					`.spec.execution.connectTimeout == "10s"`,
					`' >/dev/null || fail "PtahSchema API defaults were not persisted for omitted safe policy and execution fields"`,
				}),
				exactSourceLine("approval binding proof", `printf '%s\n' 'e2e assertions: checking approval stamping and exact binding'`),
				exactSourceLine("cross-namespace refusal proof", `printf '%s\n' 'e2e assertions: checking cross-namespace approval refusal'`),
				exactSourceLine("terminal control-plane lifecycle evidence", `printf '%s\n' 'e2e assertions: PASS control-plane contract'`),
			},
		},
		{
			path:     files.crdUpgrade,
			exitTrap: "cleanup",
			steps: []sourceContractStep{
				exactSourceLine("fail-fast shell mode", "set -eu"),
				exactSourceLine("required bounded proof namespace", `E2E_PROOF_NAMESPACE=${E2E_PROOF_NAMESPACE:?E2E_PROOF_NAMESPACE is required}`),
				exactSourceLine("required live Kubernetes version", `E2E_KUBERNETES_VERSION=${E2E_KUBERNETES_VERSION:?E2E_KUBERNETES_VERSION is required}`),
				exactSourceLine("private work directory mode", `chmod 700 "$WORK_DIR"`),
				exactSourceLine("private work file creation mask", `umask 077`),
				exactSourceLine("late activation helper destination", `LATE_ACTIVATION_HOOK_CAPTURE_BINARY=$WORK_DIR/hooklogcapture`),
				exactSourceLine("cleanup implementation", `cleanup() {`),
				exactSourceLine("cleanup status capture", `status=$?`),
				exactSourceLineSequence("late activation preflight capture cleanup", []string{
					`if [ -n "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" ]; then`,
					`kill "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" >/dev/null 2>&1 || true`,
					`wait "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" >/dev/null 2>&1 || true`,
					`LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID=`,
					`fi`,
				}),
				exactSourceLineSequence("late activation reconcile capture cleanup", []string{
					`if [ -n "$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" ]; then`,
					`kill "$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" >/dev/null 2>&1 || true`,
					`wait "$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" >/dev/null 2>&1 || true`,
					`LATE_ACTIVATION_RECONCILE_CAPTURE_PID=`,
					`fi`,
				}),
				exactSourceLine("late activation blocker cleanup", `if [ -n "$LATE_ACTIVATION_BLOCKER_WEBHOOK" ]; then`),
				exactSourceLine("cleanup status preservation", `exit "$status"`),
				exactSourceLine("live server version verification", `verify_supported_server_version`),
				exactSourceLine("exact rendered reconcile hook identity", `reconcile_matches=$(rendered_hook_job_name crd-manager 0)`),
				exactSourceLineSequence("unique rendered reconcile hook identity", []string{
					`[ "$(printf '%s\n' "$reconcile_matches" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||`,
					`fail "candidate render does not contain exactly one weight-0 reconcile hook Job"`,
				}),
				exactSourceLine("rendered reconcile hook identity assignment", `EXPECTED_RECONCILE_HOOK_NAME=$reconcile_matches`),
				exactSourceLineSequence("late activation exact update blocker", []string{
					`- name: exact-release-activation-update`,
					`expression: 'request.namespace == "$E2E_OPERATOR_NAMESPACE" && request.name == "ptah-operator-release-activation"'`,
					`- name: active-release-sequence-change`,
					`expression: 'oldObject != null && has(oldObject.data) && has(object.data) && "active-release-sequence" in oldObject.data && "active-release-sequence" in object.data && object.data["active-release-sequence"] != oldObject.data["active-release-sequence"]'`,
				}),
				exactSourceLine("late activation readiness implementation", `wait_for_late_activation_hook_log_capture_ready() {`),
				exactSourceLine("late activation readiness requires watching state", `[ "$(sed -n '1p' "$capture_status_file" 2>/dev/null)" != watching ] ||`),
				exactSourceLine("late activation dual capture arming implementation", `arm_late_activation_hook_log_captures() {`),
				exactSourceLine("late activation single capture completion implementation", `finish_late_activation_hook_log_capture() {`),
				exactSourceLine("late activation dual capture completion implementation", `finish_late_activation_hook_log_captures() {`),
				exactSourceLine("late activation failure class summary implementation", `late_activation_failure_class_summary() {`),
				exactSourceLine("late activation failure class size bound", `if [ "$failure_class_size" -gt 32 ]; then`),
				exactSourceLine("late activation diagnostic scanner implementation", `hook_diagnostic_is_safe() {`),
				exactSourceLineSequence("late activation hook diagnostic credential scan", []string{
					`if grep -F -f "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" "$diagnostic_file" >/dev/null; then`,
					`return 1`,
					`else`,
					`diagnostic_scan_status=$?`,
					`[ "$diagnostic_scan_status" -eq 1 ] || return 1`,
					`fi`,
				}),
				exactSourceLine("late activation hook diagnostic bounded format", `[ "$diagnostic_size" -gt 0 ] && [ "$diagnostic_size" -le 8192 ] &&`),
				exactSourceLine("late activation preflight diagnostic implementation", `emit_late_activation_preflight_diagnostic_if_available() {`),
				exactSourceLine("late activation preflight success contract", `grep -Fx 'candidate release preflight verified without persistent mutation' \`),
				exactSourceLine("late activation reconcile diagnostic implementation", `emit_late_activation_reconcile_diagnostic() {`),
				exactSourceLine("late activation reconcile safe diagnostic emission", `cat "$LATE_ACTIVATION_RECONCILE_LOG_FILE" >&2`),
				exactSourceLineSequence("late activation reconcile exact blocker evidence", []string{
					`grep -F 'wait for release activation guard before persistence' \`,
					`"$LATE_ACTIVATION_RECONCILE_LOG_FILE" >/dev/null ||`,
					`missing_blocker_evidence="$missing_blocker_evidence activation-phase"`,
					`grep -F 'late-activation-blocker.operator.ptah.dev' \`,
					`"$LATE_ACTIVATION_RECONCILE_LOG_FILE" >/dev/null ||`,
					`missing_blocker_evidence="$missing_blocker_evidence blocker-webhook"`,
					`grep -F 'service "ptah-operator-e2e-missing-blocker" not found' \`,
					`"$LATE_ACTIVATION_RECONCILE_LOG_FILE" >/dev/null ||`,
					`missing_blocker_evidence="$missing_blocker_evidence missing-service"`,
					`[ -z "$missing_blocker_evidence" ] ||`,
					`fail "late activation reconcile log lacks exact blocker evidence:$missing_blocker_evidence"`,
				}),
				exactSourceLine("late activation bounded summary implementation", `emit_late_activation_failure_summary() {`),
				exactSourceLineSequence("late activation failure class synthesis", []string{
					`preflight_failure_class=$(late_activation_failure_class_summary "$LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE")`,
					`reconcile_failure_class=$(late_activation_failure_class_summary "$LATE_ACTIVATION_RECONCILE_FAILURE_CLASS_FILE")`,
				}),
				exactSourceLineSequence("canceled reconcile is diagnostic-only target-not-reached evidence", []string{
					`expectedReconcileFailed: any($reconcile[]; (.weight == null or ((.weight | type) == "number" and .weight == 0)) and .last_run.phase == "Failed"),`,
					`preflightCapture: $preflight_capture,`,
					`preflightCaptureExit: $preflight_exit,`,
					`preflightCapturePhase: $preflight_phase,`,
					`preflightFailureClass: $preflight_failure_class,`,
					`reconcileCapture: $reconcile_capture,`,
					`reconcileCaptureExit: $reconcile_exit,`,
					`reconcileCapturePhase: $reconcile_phase,`,
					`reconcileFailureClass: $reconcile_failure_class,`,
					`reconcileTarget: (`,
					`if any($reconcile[]; ((.last_run.started_at // "") | type) == "string" and ((.last_run.started_at // "") | length > 0)) then "reached"`,
					`elif $reconcile_capture == "canceled" then "not-reached"`,
					`else "indeterminate"`,
					`end`,
					`)`,
				}),
				exactSourceLine("predecessor top-level Deployment recovery", `fail "candidate rollout guards blocked exact predecessor Deployment recovery for $deployment_name"`),
				exactSourceLine("late activation failure implementation", `prove_late_activation_failure_recovery() {`),
				exactSourceLine("late activation dual capture arming", `arm_late_activation_hook_log_captures`),
				exactSourceLineSequence("late activation Helm failure execution", []string{
					`if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \`,
					`--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \`,
					`--wait --timeout 2m >"$WORK_DIR/late-activation-failure.out" \`,
					`2>"$WORK_DIR/late-activation-failure.err"; then`,
				}),
				exactSourceLineSequence("late activation dual capture completion", []string{
					`fi`,
					`late_activation_captures_succeeded=false`,
					`if finish_late_activation_hook_log_captures; then`,
					`late_activation_captures_succeeded=true`,
					`fi`,
				}),
				exactSourceLineSequence("late activation structured revision retrieval before capture evidence", []string{
					`if ! helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \`,
					`--revision "$late_revision" -o json >"$late_status_file" 2>/dev/null; then`,
				}),
				exactSourceLineSequence("late activation failed status fail-closed exact hook identity jq", []string{
					`if jq -e --argjson expected_revision "$late_revision" \`,
					`--arg expected_preflight_name "$EXPECTED_PREFLIGHT_HOOK_NAME" \`,
					`--arg expected_reconcile_name "$EXPECTED_RECONCILE_HOOK_NAME" '`,
				}),
				exactSourceLineSequence("late activation failed revision evidence", []string{
					`((.hooks // []) | if type == "array" then . else [] end) as $hooks |`,
					`[$hooks[] | select(.last_run.phase == "Failed")] as $failed |`,
					`[$hooks[] | select(`,
					`.name == $expected_preflight_name and`,
					`.kind == "Job"`,
					`)] as $preflight |`,
				}),
				exactSourceLineSequence("late activation reconcile identity evidence", []string{
					`[$hooks[] | select(`,
					`.name == $expected_reconcile_name and`,
					`.kind == "Job"`,
					`)] as $reconcile |`,
				}),
				exactSourceLineSequence("late activation exact preflight success evidence", []string{
					`.version == $expected_revision and`,
					`.info.status == "failed" and`,
					`($preflight | length == 1) and`,
					`($preflight[0] |`,
					`((.weight | type) == "number" and .weight == -60) and`,
					`((.events // []) | type) == "array" and`,
					`((.events // []) | index("pre-upgrade") != null) and`,
					`.last_run.phase == "Succeeded" and`,
					`((.last_run.started_at // "") | type) == "string" and`,
					`((.last_run.started_at // "") | length > 0) and`,
					`((.last_run.completed_at // "") | type) == "string" and`,
					`((.last_run.completed_at // "") | length > 0)) and`,
				}),
				exactSourceLineSequence("late activation exact failed reconcile evidence", []string{
					`($reconcile | length == 1) and`,
					`($failed | length == 1) and`,
					`($failed[0].name == $expected_reconcile_name) and`,
					`($reconcile[0] |`,
					`(.weight == null or ((.weight | type) == "number" and .weight == 0)) and`,
					`((.events // []) | type) == "array" and`,
					`((.events // []) | index("pre-upgrade") != null) and`,
					`.last_run.phase == "Failed" and`,
					`((.last_run.started_at // "") | type) == "string" and`,
					`((.last_run.started_at // "") | length > 0) and`,
					`((.last_run.completed_at // "") | type) == "string" and`,
					`((.last_run.completed_at // "") | length > 0))`,
				}),
				exactSourceLineSequence("late activation capture evidence only after revision classification", []string{
					`delete_late_activation_blocker`,
					`if [ "$late_activation_captures_succeeded" != true ]; then`,
					`emit_late_activation_preflight_diagnostic_if_available`,
					`emit_late_activation_failure_summary "$late_status_file"`,
					`fail "late activation hook log captures did not both complete successfully"`,
					`fi`,
					`verify_late_activation_preflight_capture`,
					`emit_late_activation_reconcile_diagnostic`,
				}),
				exactSourceLine("late activation marker remains uncommitted", `fail "late failure advanced the release activation marker"`),
				exactSourceLine("predecessor Deployment restore", `restore_runtime_deployment_snapshot "$CONTROLLER_DEPLOYMENT" "$controller_snapshot"`),
				exactSourceLine("predecessor late-failure recovery completion", `printf '%s\n' 'e2e crd: predecessor late-failure recovery passed'`),
				exactSourceLineSequence("predecessor metric source quiesce implementation", []string{
					`quiesce_predecessor_metric_sources() {`,
					`for schema_name in "$PREDECESSOR_JOB_SCHEMA" "$PREDECESSOR_APPLY_SCHEMA"; do`,
				}),
				exactSourceLine("predecessor read-only Job fixture", `wait_for_predecessor_read_only_job() {`),
				exactSourceLine("predecessor Pod webhook bounded failure-policy helper", `set_predecessor_pod_webhook_failure_policy() {`),
				exactSourceLineSequence("predecessor Pod webhook bounded failure-policy transitions", []string{
					`case "$expected_policy:$desired_policy" in`,
					`Fail:Ignore | Ignore:Fail) ;;`,
					`*) fail "unsupported predecessor Pod webhook failurePolicy transition $expected_policy -> $desired_policy" ;;`,
					`esac`,
				}),
				exactSourceLine("predecessor Pod webhook exact identity lookup", `[.webhooks | to_entries[] | select(.value.name == "vpodintent.operator.ptah.dev")] |`),
				exactSourceLineSequence("predecessor Pod webhook compare-and-swap", []string{
					`{op: "test", path: ("/webhooks/" + ($index | tostring) + "/name"), value: "vpodintent.operator.ptah.dev"},`,
					`{op: "test", path: ("/webhooks/" + ($index | tostring) + "/failurePolicy"), value: $expected},`,
					`{op: "replace", path: ("/webhooks/" + ($index | tostring) + "/failurePolicy"), value: $desired}`,
				}),
				exactSourceLine("predecessor Pod webhook transition persistence", `' >/dev/null || fail "predecessor Pod webhook failurePolicy transition was not persisted"`),
				exactSourceLineSequence("predecessor read-only Job controller-owned failure staging", []string{
					`failure_target_patch=$(jq -nc \`,
					`--arg failure_target_at "$failure_target_at" \`,
					`--arg reason "$terminal_reason" \`,
					`--arg message "$terminal_message" '{`,
					`status: {`,
					`conditions: [{`,
					`type: "FailureTarget", status: "True",`,
					`reason: $reason, message: $message,`,
					`lastProbeTime: $failure_target_at,`,
					`lastTransitionTime: $failure_target_at`,
					`}]`,
					`}`,
					`}')`,
					`kube -n "$PROOF_NAMESPACE" patch job "$PREDECESSOR_JOB_NAME" --subresource=status \`,
					`--type=merge -p "$failure_target_patch" >/dev/null`,
				}),
				exactSourceLineSequence("predecessor read-only Job complete native terminal predicate", []string{
					`.metadata.uid == $uid and`,
					`(.status.startTime != null) and`,
					`((.status.active // 0) == 0) and`,
					`((.status.ready // 0) == 0) and`,
					`((.status.terminating // 0) == 0) and`,
					`(((.status.uncountedTerminatedPods.succeeded // []) | length) == 0) and`,
					`(((.status.uncountedTerminatedPods.failed // []) | length) == 0) and`,
					`(.status | has("completionTime") | not) and`,
					`((.status.conditions // []) | any(`,
					`.type == "FailureTarget" and .status == "True" and`,
					`.reason == $reason and .message == $message`,
					`)) and`,
					`((.status.conditions // []) | any(`,
					`.type == "Failed" and .status == "True" and`,
					`.reason == $reason and .message == $message`,
					`)) and`,
					`(.spec | has("ttlSecondsAfterFinished") | not)`,
				}),
				exactSourceLineSequence("predecessor read-only Job full terminal invariant latch", []string{
					`predecessor_job_terminal=1`,
					`break`,
				}),
				exactSourceLine("predecessor read-only Job native terminal wait", `fail "Job controller did not retire the predecessor read-only Job after FailureTarget staging"`),
				exactSourceLine("predecessor fixture Job nil-safe completion polling", `if jq -e '(.status.conditions // []) | any(.type == "Complete" and .status == "True")' \`),
				exactSourceLine("predecessor fixture Job nil-safe failure polling", `if jq -e '(.status.conditions // []) | any(.type == "Failed" and .status == "True")' \`),
				exactSourceLine("predecessor running Apply fixture", `prepare_predecessor_apply_fixture() {`),
				exactSourceLine("predecessor Apply schema fixture", `cp "$ROOT_DIR/testdata/e2e/postgresql-v1.sql" "$predecessor_plan_source"`),
				exactSourceLine("legacy plan activation probe manifest", `PREDECESSOR_PLAN_GUARD_PROBE_FILE=$WORK_DIR/predecessor-plan-guard-probe.json`),
				exactSourceLine("credential-safe predecessor Apply diagnostic", `emit_predecessor_apply_diagnostic() {`),
				exactSourceLineSequence("predecessor Apply outcome-unknown diagnostic fields", []string{
					`pendingObservation: (if .status.pendingObservation == null then null else {`,
					`outcome: .status.pendingObservation.outcome,`,
					`applyOperationID: .status.pendingObservation.applyOperationID,`,
					`applyJobName: (.status.pendingObservation.applyJobName // ""),`,
					`applyJobUID: (.status.pendingObservation.applyJobUID // ""),`,
					`applyPodCount: (.status.pendingObservation.applyPodCount // 0),`,
					`applyPodUIDs: (.status.pendingObservation.applyPodUIDs // []),`,
					`applyGeneration: (.status.pendingObservation.applyGeneration // 0),`,
					`observeAfter: (.status.pendingObservation.observeAfter // ""),`,
					`planRequired: (.status.pendingObservation.planRequired // false),`,
					`leaseEpoch: (.status.pendingObservation.leaseEpoch // "")`,
					`} end),`,
				}),
				exactSourceLineSequence("predecessor Apply diagnostic exact guard ownership", []string{
					`objects: [.items[] | select(`,
					`.metadata.annotations["operator.ptah.dev/release-name"] == $release and`,
					`.metadata.annotations["operator.ptah.dev/release-namespace"] == $namespace`,
					`) | {`,
				}),
				exactSourceLineSequence("predecessor Apply diagnostic credential scan", []string{
					`if grep -F -f "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" "$diagnostic_file" >/dev/null; then`,
					`fail "predecessor Apply diagnostic contained a protected task credential"`,
					`else`,
					`diagnostic_scan_status=$?`,
					`[ "$diagnostic_scan_status" -eq 1 ] || fail "predecessor Apply credential scan failed closed"`,
					`fi`,
				}),
				exactSourceLineSequence("predecessor Apply terminal failure fast path", []string{
					`.status.pendingObservation.outcome == "OutcomeUnknown" or`,
					`((.status.conditions // []) | any(`,
					`.type == "ReconciliationFailed" and .status == "True"`,
					`))`,
					`' "$WORK_DIR/predecessor-apply-running-schema.json" >/dev/null; then`,
					`emit_predecessor_apply_diagnostic`,
					`fail "predecessor Apply entered a terminal failure before its running Pod was observed"`,
					`fi`,
				}),
				exactSourceLine("certificate Secret identity capture implementation", `capture_certificate_secret_names() {`),
				exactSourceLineSequence("unlabeled certificate Secrets exact uninstall absence", []string{
					`remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get \`,
					`"secret/$CERTIFICATE_SECRET_NAME" --ignore-not-found=true -o name)`,
					`[ -z "$remaining" ] ||`,
					`fail "unlabeled generated certificate Secret/$CERTIFICATE_SECRET_NAME survived uninstall"`,
					`remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get \`,
					`"secret/$CERTIFICATE_STAGING_SECRET_NAME" --ignore-not-found=true -o name)`,
					`[ -z "$remaining" ] ||`,
					`fail "unlabeled certificate staging Secret/$CERTIFICATE_STAGING_SECRET_NAME survived uninstall"`,
					`CERTIFICATE_SECRET_NAME=`,
					`CERTIFICATE_STAGING_SECRET_NAME=`,
				}),
				exactSourceLineSequence("bounded runtime Deployment deletion", []string{
					`kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment \`,
					`"$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT" \`,
					`--cascade=foreground --wait=true --timeout=2m >/dev/null`,
				}),
				exactSourceLineSequence("bounded controller Deployment deletion", []string{
					`kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment "$CONTROLLER_DEPLOYMENT" \`,
					`--cascade=foreground --wait=true --timeout=2m >/dev/null`,
				}),
				exactSourceLine("legacy Job activation boundary implementation", `prove_legacy_job_activation_boundary() {`),
				exactSourceLine("legacy Job activation probe source", `legacy_job_source=$WORK_DIR/predecessor-read-only-job-terminal.json`),
				exactSourceLineSequence("legacy Job bootstrap admits before activation", []string{
					`if ! controller_kube create --dry-run=server -o json -f "$legacy_job_probe" \`,
					`>"$stdout" 2>"$stderr"; then`,
					`cat "$stderr" >&2`,
					`fail "legacy Job bootstrap probe was refused before candidate activation"`,
					`fi`,
				}),
				exactSourceLine("legacy Job active structural denial", `fail "legacy Job post-activation probe lacked the exact structural guard denial"`),
				exactSourceLine("legacy plan activation boundary implementation", `prove_legacy_plan_activation_boundary() {`),
				exactSourceLineSequence("legacy plan bootstrap admits before activation", []string{
					`if ! controller_kube create --dry-run=server -o json \`,
					`-f "$PREDECESSOR_PLAN_GUARD_PROBE_FILE" >"$stdout" 2>"$stderr"; then`,
					`cat "$stderr" >&2`,
					`fail "legacy plan bootstrap probe was refused before candidate activation"`,
					`fi`,
				}),
				exactSourceLine("legacy plan active structural denial", `fail "legacy plan post-activation probe lacked the exact structural guard denial"`),
				exactSourceLine("controller guarded-field proof implementation", `prove_controller_object_supported_window_guard() {`),
				exactSourceLine("controller guarded-field proof call", `prove_controller_object_supported_window_guard`),
				exactSourceLine("legacy Job active boundary call", `prove_legacy_job_activation_boundary active`),
				exactSourceLine("legacy plan active boundary call", `prove_legacy_plan_activation_boundary active`),
				exactSourceLineSequence("predecessor read-only Job bounded webhook outage bridge", []string{
					`set_predecessor_pod_webhook_failure_policy Fail Ignore`,
					`stage_predecessor_read_only_job_completion`,
					`set_predecessor_pod_webhook_failure_policy Ignore Fail`,
				}),
				exactSourceLine("predecessor read-only Job late-create UID gap", `stage_predecessor_read_only_job_uid_gap`),
				exactSourceLine("predecessor late activation recovery call", `prove_late_activation_failure_recovery`),
				exactSourceLine("legacy Job bootstrap boundary call", `prove_legacy_job_activation_boundary bootstrap`),
				exactSourceLine("legacy plan bootstrap boundary call", `prove_legacy_plan_activation_boundary bootstrap`),
				exactSourceLine("predecessor Apply database barrier start", `start_predecessor_apply_barrier`),
				exactSourceLine("predecessor running Apply start", `start_predecessor_apply_fixture`),
				exactSourceLine("predecessor Apply database barrier contention", `wait_for_predecessor_apply_barrier_contention`),
				exactSourceLine("predecessor Apply running late-create UID gap", `stage_predecessor_apply_job_uid_gap_while_running`),
				exactSourceLine("predecessor Apply upgrade overlap proof", `assert_predecessor_apply_remains_exclusive_while_running`),
				exactSourceLine("predecessor Apply database barrier recheck", `assert_predecessor_apply_barrier_contended`),
				exactSourceLine("predecessor Apply database barrier release", `release_predecessor_apply_barrier`),
				exactSourceLine("predecessor Apply terminal wait", `wait_for_predecessor_apply_job_terminal`),
				exactSourceLine("predecessor read-only Job cleanup proof", `wait_for_predecessor_read_only_job_cleanup`),
				exactSourceLine("predecessor Apply cleanup proof", `wait_for_predecessor_apply_job_cleanup`),
				exactSourceLine("predecessor metric source quiesce call", `quiesce_predecessor_metric_sources`),
				exactSourceLine("upgrade proof implementation", `run_upgrade_proof() {`),
				exactSourceLine("predecessor upgrade proof call", `run_predecessor_upgrade_proof`),
				exactSourceLine("runtime singleton proof call", `prove_runtime_singleton_guard`),
				exactSourceLine("controller downgrade proof call", `prove_controller_downgrade_guard`),
				exactSourceLine("uninstall proof implementation", `run_uninstall_proof() {`),
				exactSourceLineSequence("released chart fresh-install inputs", []string{
					`if [ ! -f "$E2E_CHART_PACKAGE" ] || [ -L "$E2E_CHART_PACKAGE" ]; then`,
					`fail "E2E_CHART_PACKAGE must name the regular non-symlink current-release chart package"`,
					`fi`,
					`if [ ! -f "$E2E_CANDIDATE_VALUES_FILE" ] || [ -L "$E2E_CANDIDATE_VALUES_FILE" ]; then`,
					`fail "E2E_CANDIDATE_VALUES_FILE must name the regular non-symlink current-release values file"`,
					`fi`,
				}),
				exactSourceLineSequence("upgraded release exact uninstall absence", []string{
					`assert_inventory_resources_absent \`,
					`"$next_sequence_inventory" "$next_sequence_marker_name"`,
				}),
				exactSourceLineSequence("reinstalled successor inventory capture", []string{
					`reinstalled_next_marker=$WORK_DIR/reinstalled-sequence-${E2E_NEXT_RELEASE_SEQUENCE}-admission-convergence.json`,
					`reinstalled_next_inventory=$WORK_DIR/reinstalled-sequence-${E2E_NEXT_RELEASE_SEQUENCE}-admission-inventory.json`,
				}),
				exactSourceLineSequence("reinstalled successor exact uninstall absence", []string{
					`assert_inventory_resources_absent \`,
					`"$reinstalled_next_inventory" "$reinstalled_next_marker_name"`,
				}),
				exactSourceLineSequence("exact released chart fresh install", []string{
					`helm_e2e install "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \`,
					`--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \`,
					`--wait --timeout 5m >/dev/null`,
				}),
				exactSourceLineSequence("exact released chart controller identity", []string{
					`capture_controller_service_account_identity \`,
					`"$E2E_CURRENT_RELEASE_SEQUENCE" "$E2E_CANDIDATE_IMAGE" \`,
					`"$WORK_DIR/fresh-current-sequence-${E2E_CURRENT_RELEASE_SEQUENCE}-controller-identity.json"`,
				}),
				exactSourceLineSequence("exact released chart activation", []string{
					`assert_release_activation_sequence \`,
					`"$E2E_CURRENT_RELEASE_SEQUENCE" "$E2E_CANDIDATE_IMAGE"`,
				}),
				exactSourceLineSequence("exact released chart sealed inventory", []string{
					`assert_sealed_release_inventory \`,
					`"$E2E_CURRENT_RELEASE_SEQUENCE" "$E2E_CANDIDATE_IMAGE" \`,
					`"$fresh_current_marker" "$fresh_current_inventory"`,
				}),
				exactSourceLine("exact released chart zero-residue assertion", `assert_release_sequence_candidate_residue_absent "$E2E_CURRENT_RELEASE_SEQUENCE"`),
				exactSourceLineSequence("exact released chart inventory absence", []string{
					`assert_inventory_resources_absent \`,
					`"$fresh_current_inventory" "$fresh_current_marker_name"`,
				}),
				exactSourceLine("exact released chart installability evidence", `printf '%s\n' 'e2e crd: exact exported current-release chart passed fresh install and zero-residue uninstall'`),
				exactSourceLineSequence("phase dispatch", []string{
					`case "$E2E_PHASE" in`,
					`upgrade) run_upgrade_proof ;;`,
					`uninstall) run_uninstall_proof ;;`,
					`*) fail "unsupported E2E_PHASE $E2E_PHASE" ;;`,
					`esac`,
				}),
				exactSourceLine("terminal CRD lifecycle evidence", `printf 'e2e crd: PASS phase=%s\n' "$E2E_PHASE"`),
			},
			successfulReturns: []successfulReturnContract{
				{
					start:      sourceLinePattern(`run_upgrade_proof() {`),
					completion: sourceLinePattern(`printf '%s\n' 'e2e crd: upgrade and singleton proofs passed'`),
				},
				{
					start:      sourceLinePattern(`run_uninstall_proof() {`),
					completion: sourceLinePattern(`printf '%s\n' 'e2e crd: uninstall retained CRDs and live objects'`),
				},
			},
		},
		{
			path:     files.faults,
			exitTrap: "cleanup",
			steps: []sourceContractStep{
				exactSourceLine("fail-fast shell mode", "set -eu"),
				exactSourceLine("cleanup implementation", `cleanup() {`),
				exactSourceLine("cleanup status capture", `status=$?`),
				exactSourceLine("cleanup status preservation", `exit "$status"`),
				exactSourceLine("credential-bearing principal refusal proof implementation", `run_credential_principal_refusal() {`),
				exactSourceLine("resourceVersion watch proof call", `start_watches`),
				exactSourceLine("credential-bearing principal refusal proof call", `run_credential_principal_refusal`),
				exactSourceLine("deadline fault proof", `printf '%s\n' 'e2e faults: forcing one real Kubernetes Apply Job deadline'`),
				exactSourceLine("read-chain ordering proof call", `assert_initial_read_chain_watch_order "$PG_RESTART_SCHEMA"`),
				exactSourceLine("operation Pod serialization proof call", `assert_no_overlapping_operation_pods`),
				exactSourceLine("operation Job serialization proof call", `assert_no_overlapping_operation_jobs`),
				exactSourceLine("fault audit proof call", `assert_fault_audit_complete`),
				exactSourceLine("parent audit handoff", `record_fault_jobs_for_parent`),
				exactSourceLine("terminal fault lifecycle evidence", `printf '%s\n' 'e2e faults: PASS watches, Kubernetes deadline recovery, stale-plan preflight, native lock barriers, restart identity, uncertain recovery, deletion, Pod serialization, credential audit, and coordination realms'`),
			},
			successfulReturns: []successfulReturnContract{
				{
					start:      sourceLinePattern(`run_credential_principal_refusal() {`),
					completion: sourceLinePattern(`audit_fault_runtime`),
				},
			},
		},
		{
			path:     files.highAvailability,
			exitTrap: "cleanup",
			steps: []sourceContractStep{
				exactSourceLine("fail-fast shell mode", "set -eu"),
				exactSourceLine("cleanup implementation", `cleanup() {`),
				exactSourceLine("custom metrics validator implementation", `validate_custom_operator_metrics() {`),
				exactSourceLineSequence("custom metrics exact labeled samples", []string{
					`if ($1 == "ptah_operator_reconciliations_total{result=\"success\"}") {`,
					`reconciliation_sample++`,
					`} else if ($1 == "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"}") {`,
					`failure_sample++`,
				}),
				exactSourceLineSequence("custom metrics duplicate and unexpected-family refusal", []string{
					`if (malformed || reconciliation_help > 1 || failure_help > 1 ||`,
					`reconciliation_type > 1 || failure_type > 1 ||`,
					`reconciliation_sample > 1 || failure_sample > 1) {`,
				}),
				exactSourceLineSequence("custom metrics exact two-family acceptance", []string{
					`if (reconciliation_help == 1 && failure_help == 1 &&`,
					`reconciliation_type == 1 && failure_type == 1 &&`,
					`reconciliation_sample == 1 && failure_sample == 1) {`,
				}),
				exactSourceLine("Resolve failure counter parser implementation", `resolve_operation_failure_counter_from_metrics() {`),
				exactSourceLine("prior Resolve metric source exclusion implementation", `assert_prior_resolve_metric_sources_quiesced() {`),
				exactSourceLine("Resolve failure counter increase proof", `'BEGIN { exit ! ((current + 0) > (baseline + 0)) }'; then`),
				exactSourceLine("Lease authorization proof", `printf '%s\n' 'e2e HA: verifying namespace-scoped Lease authorization'`),
				exactSourceLine("initial leader proof", `initial_holder=$(wait_for_leader "")`),
				exactSourceLine("leader failover proof", `second_holder=$(wait_for_leader "$initial_holder")`),
				exactSourceLine("prior Resolve metric source exclusion call", `assert_prior_resolve_metric_sources_quiesced`),
				exactSourceLine("pre-operation Resolve failure counter baseline", `resolve_failure_counter_before=$(read_resolve_operation_failure_counter "$second_holder")`),
				exactSourceLine("post-failover operation proof", `operation_job=$(wait_for_admitted_operation_pod "$ha_schema_uid")`),
				exactSourceLine("post-failover failed Resolve lifecycle proof", `wait_for_failed_resolve_lifecycle "$ha_schema_uid"`),
				exactSourceLine("post-failure custom metrics proof", `assert_custom_operator_metrics "$second_holder" "$resolve_failure_counter_before"`),
				exactSourceLine("terminal high-availability lifecycle evidence", `printf '%s\n' 'e2e HA: PASS one Lease, exact RBAC, Pod failover, admitted operation, and custom metrics'`),
			},
		},
		{
			path:     files.certRotation,
			exitTrap: "cleanup_upgrade_files",
			steps: []sourceContractStep{
				exactSourceLine("fail-fast shell mode", "set -eu"),
				exactSourceLine("cleanup implementation", `cleanup_upgrade_files() {`),
				exactSourceLine("cleanup status capture", `status=$?`),
				exactSourceLine("cleanup status preservation", `exit "$status"`),
				exactSourceLine("pre-upgrade admission proof call", `assert_approval_admission_callable "before the Helm upgrade"`),
				exactSourceLine("post-upgrade admission proof call", `assert_approval_admission_callable "after the Helm upgrade"`),
				exactSourceLine("corrupt-CA recovery identity proof", `[ "$NEW_ROTATOR_UID" != "$OLD_ROTATOR_UID" ] || fail "certificate rotator Pod was not replaced"`),
				exactSourceLine("missing-Secret recovery identity proof", `[ "$ROTATOR_UID_AFTER_RECREATE" != "$ROTATOR_UID_BEFORE_RECREATE" ] ||`),
				exactSourceLine("terminal certificate lifecycle evidence", `printf '%s\n' 'e2e certificate rotation: PASS live Helm lookup, corrupt-CA recovery, and exact guarded recreation'`),
			},
		},
	}
	for _, contract := range childContracts {
		if err := verifyLifecycleSource(contract); err != nil {
			return err
		}
	}
	crdUpgradeContents, err := os.ReadFile(files.crdUpgrade)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.crdUpgrade, err)
	}
	for _, functionName := range []string{
		"wait_for_late_activation_hook_log_capture_ready",
		"arm_late_activation_hook_log_captures",
		"finish_late_activation_hook_log_capture",
		"finish_late_activation_hook_log_captures",
		"late_activation_capture_status_summary",
		"late_activation_capture_exit_summary",
		"late_activation_failure_class_summary",
		"hook_diagnostic_is_safe",
		"emit_late_activation_preflight_diagnostic_if_available",
		"verify_late_activation_preflight_capture",
		"emit_late_activation_reconcile_diagnostic",
		"emit_late_activation_failure_summary",
	} {
		if err := verifySingleShellFunctionDefinition(files.crdUpgrade, crdUpgradeContents, functionName); err != nil {
			return err
		}
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"arm_late_activation_hook_log_captures",
		lateActivationHookCaptureArmContract,
		"exact dual resourceVersion-bound late activation hook capture arm contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"finish_late_activation_hook_log_capture",
		lateActivationHookCaptureFinishContract,
		"exact bounded late activation hook capture completion contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"finish_late_activation_hook_log_captures",
		lateActivationHookCapturesFinishContract,
		"exact bounded dual late activation hook capture completion contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"late_activation_failure_class_summary",
		lateActivationFailureClassSummaryContract,
		"exact bounded allowlisted late activation failure class summary contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"hook_diagnostic_is_safe",
		lateActivationHookDiagnosticContract,
		"exact credential-safe bounded hook diagnostic scanner contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"emit_late_activation_preflight_diagnostic_if_available",
		lateActivationPreflightDiagnosticContract,
		"exact optional preflight diagnostic emission contract",
	); err != nil {
		return err
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"emit_late_activation_reconcile_diagnostic",
		lateActivationReconcileDiagnosticContract,
		"exact reconcile blocker diagnostic emission contract",
	); err != nil {
		return err
	}
	lateActivationFailurePattern := regexp.MustCompile(`(?ms)^prove_late_activation_failure_recovery\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`)
	lateActivationFailureMatches := lateActivationFailurePattern.FindAll(crdUpgradeContents, -1)
	if len(lateActivationFailureMatches) != 1 {
		return fmt.Errorf("%s: late activation failure proof must have exactly one auditable function body", files.crdUpgrade)
	}
	lateActivationFailureBody := lateActivationFailureMatches[0]
	orderedEvidenceMarkers := []struct {
		description string
		marker      string
	}{
		{"structured revision query", `if ! helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \`},
		{"exact revision classification", `if jq -e --argjson expected_revision "$late_revision" \`},
		{"capture-success enforcement", `if [ "$late_activation_captures_succeeded" != true ]; then`},
	}
	previousEvidenceOffset := -1
	for _, evidenceMarker := range orderedEvidenceMarkers {
		offset := bytes.Index(lateActivationFailureBody, []byte(evidenceMarker.marker))
		if offset < 0 || offset <= previousEvidenceOffset {
			return fmt.Errorf(
				"%s: revision classification must precede capture-success enforcement at %s",
				files.crdUpgrade,
				evidenceMarker.description,
			)
		}
		previousEvidenceOffset = offset
	}
	for _, privateEvidence := range []string{
		"late-activation-failure.out",
		"late-activation-failure.err",
	} {
		if bytes.Count(crdUpgradeContents, []byte(privateEvidence)) != 1 {
			return fmt.Errorf("%s: late activation raw Helm evidence must remain write-only", files.crdUpgrade)
		}
	}
	for _, helperErrorFile := range []string{
		"LATE_ACTIVATION_PREFLIGHT_CAPTURE_ERRORS_FILE",
		"LATE_ACTIVATION_RECONCILE_CAPTURE_ERRORS_FILE",
	} {
		if bytes.Contains(lateActivationFailureBody, []byte(helperErrorFile)) {
			return fmt.Errorf("%s: late activation helper errors must not be emitted as evidence", files.crdUpgrade)
		}
		if bytes.Count(crdUpgradeContents, []byte(helperErrorFile)) != 3 {
			return fmt.Errorf("%s: late activation helper error files must remain private non-emitted capture outputs", files.crdUpgrade)
		}
	}
	for _, failureClassFile := range []string{
		"LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE",
		"LATE_ACTIVATION_RECONCILE_FAILURE_CLASS_FILE",
	} {
		if bytes.Count(crdUpgradeContents, []byte(failureClassFile)) != 4 {
			return fmt.Errorf("%s: late activation failure classes must flow only through private helper output and bounded synthesis", files.crdUpgrade)
		}
	}
	return nil
}

type kindClusterTemplate struct {
	Kind       string `yaml:"kind"`
	APIVersion string `yaml:"apiVersion"`
	Networking struct {
		IPFamily         string `yaml:"ipFamily"`
		APIServerAddress string `yaml:"apiServerAddress"`
		APIServerPort    string `yaml:"apiServerPort"`
	} `yaml:"networking"`
	Nodes []kindNodeTemplate `yaml:"nodes"`
}

type kindNodeTemplate struct {
	Role                 string   `yaml:"role"`
	KubeadmConfigPatches []string `yaml:"kubeadmConfigPatches"`
}

func verifyAPIServerEndpointInventoryFilter(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !bytes.Equal(contents, []byte(apiServerEndpointInventoryFilterContract)) {
		return fmt.Errorf("%s: API server endpoint inventory filter differs from the audited per-slice contract", path)
	}
	return nil
}

func verifyKindHAConfig(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var config kindClusterTemplate
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s: multiple YAML documents are forbidden", path)
		}
		return fmt.Errorf("decode trailing %s document: %w", path, err)
	}
	if config.Kind != "Cluster" || config.APIVersion != "kind.x-k8s.io/v1alpha4" {
		return fmt.Errorf("%s: kind cluster apiVersion/kind is invalid", path)
	}
	if config.Networking.IPFamily != "ipv4" || config.Networking.APIServerAddress != "127.0.0.1" ||
		config.Networking.APIServerPort != "__API_SERVER_PORT__" {
		return fmt.Errorf("%s: kind networking contract is invalid", path)
	}
	wantRoles := []string{"control-plane", "control-plane", "control-plane", "worker"}
	if len(config.Nodes) != len(wantRoles) {
		return fmt.Errorf("%s: kind topology has %d nodes, want exactly four", path, len(config.Nodes))
	}
	const kubeletPatch = `kind: KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
featureGates:
  KubeletInUserNamespace: true`
	for index, node := range config.Nodes {
		if node.Role != wantRoles[index] {
			return fmt.Errorf("%s: kind node %d role is %q, want %q", path, index, node.Role, wantRoles[index])
		}
		if len(node.KubeadmConfigPatches) != 1 || strings.TrimSpace(node.KubeadmConfigPatches[0]) != kubeletPatch {
			return fmt.Errorf("%s: kind node %d does not have the exact kubelet feature-gate patch", path, index)
		}
	}
	return nil
}

func verifyFailedUpgradeEvidenceSource(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	contract := []sourceContractStep{
		exactSourceLine("failed-upgrade evidence implementation", `expect_upgrade_failure_without_deployment_change() {`),
		exactSourceLine("failed-upgrade structured status destination", `status_file=$WORK_DIR/failed-upgrade-status.json`),
		exactSourceLineSequence("current and next failed revision binding", []string{
			`before_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \`,
			`--namespace "$E2E_OPERATOR_NAMESPACE" -o json | jq -er '.version | select(type == "number" and . >= 1)')`,
			`failed_revision=$((before_revision + 1))`,
		}),
		exactSourceLineSequence("rendered hook identity binding", []string{
			`[ -n "$EXPECTED_IDENTITY_HOOK_NAME" ] || fail "rendered identity hook name is unavailable"`,
			`[ -n "$EXPECTED_PREFLIGHT_HOOK_NAME" ] || fail "rendered preflight hook name is unavailable"`,
			`deployment_evidence >"$before"`,
			`arm_identity_hook_log_capture`,
		}),
		exactSourceLineSequence("failed upgrade execution and explicit revision retrieval", []string{
			`if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \`,
			`--namespace "$E2E_OPERATOR_NAMESPACE" --values "$UPGRADE_VALUES_FILE" \`,
			`--wait --timeout 2m "$@" >"$WORK_DIR/failed-upgrade.out" 2>"$WORK_DIR/failed-upgrade.err"; then`,
			`finish_identity_hook_log_capture`,
			`fail "$description unexpectedly succeeded"`,
			`fi`,
			`finish_identity_hook_log_capture`,
			`if ! helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \`,
			`--revision "$failed_revision" -o json >"$status_file"; then`,
			`fail "$description did not retain structured Helm evidence for failed revision $failed_revision"`,
			`fi`,
		}),
		exactSourceLineSequence("exact failed preflight evidence evaluation", []string{
			`if ! jq -e \`,
			`--argjson expected_revision "$failed_revision" \`,
			`--arg expected_name "$EXPECTED_PREFLIGHT_HOOK_NAME" \`,
			`--argjson expected_weight -60 \`,
			`--arg expected_identity_name "$EXPECTED_IDENTITY_HOOK_NAME" \`,
			`--argjson expected_identity_weight -105 \`,
			`-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null; then`,
			`emit_identity_hook_diagnostic >&2 ||`,
			`fail "$description identity-hook diagnostic failed closed"`,
		}),
		exactSourceLine("failed preflight evidence refusal", `fail "$description lacks exact revision-bound failed preflight evidence"`),
	}
	if err := verifyOrderedSourceContract(path, contents, contract); err != nil {
		return err
	}

	start := contract[0].pattern.FindIndex(contents)
	end := sourceLinePattern(`expect_upgrade_render_failure_without_deployment_change() {`).FindIndex(contents)
	if start == nil || end == nil || end[0] <= start[1] {
		return fmt.Errorf("%s: failed-upgrade evidence function boundaries are invalid", path)
	}
	functionBody := contents[start[0]:end[0]]
	if bytes.Count(functionBody, []byte("failed-upgrade.err")) != 1 {
		return fmt.Errorf("%s: failed-upgrade stderr may only be captured once and must not be parsed as hook evidence", path)
	}
	if bytes.Count(functionBody, []byte("failed-upgrade-status.json")) != 1 || bytes.Count(functionBody, []byte("$status_file")) != 3 {
		return fmt.Errorf("%s: failed-upgrade evidence must flow only from the explicitly retrieved structured revision status", path)
	}
	return nil
}

const lateActivationHookCaptureArmContract = `arm_late_activation_hook_log_captures() {
	[ -n "$EXPECTED_PREFLIGHT_HOOK_NAME" ] || fail "rendered preflight hook name is unavailable"
	[ -n "$EXPECTED_RECONCILE_HOOK_NAME" ] || fail "rendered reconcile hook name is unavailable"
	[ -z "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" ] || fail "late activation preflight capture is already armed"
	[ -z "$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" ] || fail "late activation reconcile capture is already armed"
	require_mode_0600_regular_file "$EXPECTED_CRD_UPGRADE_RENDER_FILE" expected-crd-upgrade-render
	mkdir -p "$WORK_DIR/go-cache"
	env GOCACHE="$WORK_DIR/go-cache" go -C "$ROOT_DIR" build -trimpath \
		-o "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ./hack/hooklogcapture
	if [ ! -f "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ] ||
		[ -L "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ] ||
		[ ! -x "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ]; then
		fail "late activation hook log capture helper is not a regular executable"
	fi
	"$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" \
		--kubeconfig "$E2E_KUBECONFIG" \
		--namespace "$E2E_OPERATOR_NAMESPACE" \
		--job-name "$EXPECTED_PREFLIGHT_HOOK_NAME" \
		--hook-mode preflight \
		--render-file "$EXPECTED_CRD_UPGRADE_RENDER_FILE" \
		--log-file "$LATE_ACTIVATION_PREFLIGHT_LOG_FILE" \
		--status-file "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_STATUS_FILE" \
		--ready-file "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_READY_FILE" \
		--error-file "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_ERRORS_FILE" \
		--failure-class-file "$LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE" \
		--timeout 3m >/dev/null 2>&1 &
	LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID=$!
	"$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" \
		--kubeconfig "$E2E_KUBECONFIG" \
		--namespace "$E2E_OPERATOR_NAMESPACE" \
		--job-name "$EXPECTED_RECONCILE_HOOK_NAME" \
		--hook-mode reconcile \
		--render-file "$EXPECTED_CRD_UPGRADE_RENDER_FILE" \
		--log-file "$LATE_ACTIVATION_RECONCILE_LOG_FILE" \
		--status-file "$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE" \
		--ready-file "$LATE_ACTIVATION_RECONCILE_CAPTURE_READY_FILE" \
		--error-file "$LATE_ACTIVATION_RECONCILE_CAPTURE_ERRORS_FILE" \
		--failure-class-file "$LATE_ACTIVATION_RECONCILE_FAILURE_CLASS_FILE" \
		--timeout 3m >/dev/null 2>&1 &
	LATE_ACTIVATION_RECONCILE_CAPTURE_PID=$!
	wait_for_late_activation_hook_log_capture_ready \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_STATUS_FILE" \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_READY_FILE" \
		"late activation preflight capture"
	wait_for_late_activation_hook_log_capture_ready \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE" \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_READY_FILE" \
		"late activation reconcile capture"
	for activation_capture_file in \
		"$LATE_ACTIVATION_PREFLIGHT_LOG_FILE" \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_STATUS_FILE" \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_ERRORS_FILE" \
		"$LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE" \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_READY_FILE" \
		"$LATE_ACTIVATION_RECONCILE_LOG_FILE" \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE" \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_ERRORS_FILE" \
		"$LATE_ACTIVATION_RECONCILE_FAILURE_CLASS_FILE" \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_READY_FILE"; do
		require_mode_0600_regular_file "$activation_capture_file" late-activation-hook-capture-file
	done
}`

const lateActivationHookCaptureFinishContract = `finish_late_activation_hook_log_capture() {
	capture_pid=$1
	capture_status_file=$2
	capture_grace=0
	while kill -0 "$capture_pid" >/dev/null 2>&1 && [ "$capture_grace" -lt 15 ]; do
		case "$(sed -n '1p' "$capture_status_file" 2>/dev/null)" in
		captured | failed | canceled) break ;;
		esac
		sleep 1
		capture_grace=$((capture_grace + 1))
	done
	case "$(sed -n '1p' "$capture_status_file" 2>/dev/null)" in
	captured | failed | canceled) ;;
	*) kill "$capture_pid" >/dev/null 2>&1 || true ;;
	esac
	capture_exit_status=0
	wait "$capture_pid" >/dev/null 2>&1 || capture_exit_status=$?
	return "$capture_exit_status"
}`

const lateActivationHookCapturesFinishContract = `finish_late_activation_hook_log_captures() {
	[ -n "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" ] || fail "late activation preflight capture is not armed"
	[ -n "$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" ] || fail "late activation reconcile capture is not armed"
	LATE_ACTIVATION_PREFLIGHT_CAPTURE_EXIT_STATUS=0
	finish_late_activation_hook_log_capture \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" \
		"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_STATUS_FILE" ||
		LATE_ACTIVATION_PREFLIGHT_CAPTURE_EXIT_STATUS=$?
	LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID=
	LATE_ACTIVATION_RECONCILE_CAPTURE_EXIT_STATUS=0
	finish_late_activation_hook_log_capture \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" \
		"$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE" ||
		LATE_ACTIVATION_RECONCILE_CAPTURE_EXIT_STATUS=$?
	LATE_ACTIVATION_RECONCILE_CAPTURE_PID=
	[ "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_EXIT_STATUS" -eq 0 ] &&
		[ "$LATE_ACTIVATION_RECONCILE_CAPTURE_EXIT_STATUS" -eq 0 ]
}`

const lateActivationFailureClassSummaryContract = `late_activation_failure_class_summary() {
	failure_class_file=$1
	if [ -L "$failure_class_file" ]; then
		printf '%s\n' invalid
		return
	fi
	if [ ! -e "$failure_class_file" ]; then
		printf '%s\n' unavailable
		return
	fi
	if [ ! -f "$failure_class_file" ]; then
		printf '%s\n' invalid
		return
	fi
	if failure_class_mode=$(stat -c '%a' "$failure_class_file" 2>/dev/null); then
		:
	else
		failure_class_mode=$(stat -f '%Lp' "$failure_class_file" 2>/dev/null) || {
			printf '%s\n' invalid
			return
		}
	fi
	if [ "$failure_class_mode" != 600 ]; then
		printf '%s\n' invalid
		return
	fi
	failure_class_size=$(wc -c <"$failure_class_file" 2>/dev/null | tr -d '[:space:]') || {
		printf '%s\n' invalid
		return
	}
	case "$failure_class_size" in
	'' | *[!0-9]*)
		printf '%s\n' invalid
		return
		;;
	0)
		printf '%s\n' unavailable
		return
		;;
	esac
	if [ "$failure_class_size" -gt 32 ]; then
		printf '%s\n' invalid
		return
	fi
	failure_class_lines=$(awk 'END { print NR + 0 }' "$failure_class_file" 2>/dev/null) || {
		printf '%s\n' invalid
		return
	}
	if [ "$failure_class_lines" -ne 1 ]; then
		printf '%s\n' invalid
		return
	fi
	failure_class=$(sed -n '1p' "$failure_class_file" 2>/dev/null) || {
		printf '%s\n' invalid
		return
	}
	case "$failure_class" in
	configuration | output | render | kubernetes-client | priority-inventory | priority-watch | job-inventory | job-watch | job-contract | pod-inventory | pod-watch | pod-contract | pod-owner | log-start | log-start-timeout | log-read | log-empty | log-too-large | deadline | canceled | internal)
		printf '%s\n' "$failure_class"
		;;
	*) printf '%s\n' invalid ;;
	esac
}`

const lateActivationHookDiagnosticContract = `hook_diagnostic_is_safe() {
	diagnostic_file=$1
	[ -f "$diagnostic_file" ] && [ ! -L "$diagnostic_file" ] || return 1
	if diagnostic_mode=$(stat -c '%a' "$diagnostic_file" 2>/dev/null); then
		:
	else
		diagnostic_mode=$(stat -f '%Lp' "$diagnostic_file" 2>/dev/null) || return 1
	fi
	[ "$diagnostic_mode" = 600 ] || return 1
	require_mode_0600_regular_file "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" identity-hook-credential-patterns
	[ -s "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" ] || return 1
	if grep -F -f "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" "$diagnostic_file" >/dev/null; then
		return 1
	else
		diagnostic_scan_status=$?
		[ "$diagnostic_scan_status" -eq 1 ] || return 1
	fi
	if LC_ALL=C grep -Eq '(^|[^[:alnum:]_-])eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+($|[^[:alnum:]_-])|[Aa]uthorization:[[:space:]]*|[Bb]earer[[:space:]]+|://[^[:space:]@/:]+:[^[:space:]@/]+@' \
		"$diagnostic_file"; then
		return 1
	else
		diagnostic_scan_status=$?
		[ "$diagnostic_scan_status" -eq 1 ] || return 1
	fi
	diagnostic_size=$(wc -c <"$diagnostic_file" | tr -d '[:space:]')
	diagnostic_lines=$(awk 'END { print NR + 0 }' "$diagnostic_file")
	case "$diagnostic_size:$diagnostic_lines" in
	*[!0-9:]* | :* | *:) return 1 ;;
	esac
	[ "$diagnostic_size" -gt 0 ] && [ "$diagnostic_size" -le 8192 ] &&
		[ "$diagnostic_lines" -eq 1 ] &&
		LC_ALL=C grep -Eq '^ptah-crd-manager: [[:print:]]+$|^candidate release preflight verified without persistent mutation$' \
			"$diagnostic_file"
}`

const lateActivationPreflightDiagnosticContract = `emit_late_activation_preflight_diagnostic_if_available() {
	[ "$(late_activation_capture_status_summary "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_STATUS_FILE")" = captured ] || return 0
	[ -s "$LATE_ACTIVATION_PREFLIGHT_LOG_FILE" ] || return 0
	if hook_diagnostic_is_safe "$LATE_ACTIVATION_PREFLIGHT_LOG_FILE"; then
		cat "$LATE_ACTIVATION_PREFLIGHT_LOG_FILE" >&2
	else
		printf '%s\n' 'e2e crd: preflight diagnostic withheld by credential and format scanner' >&2
	fi
}`

const lateActivationReconcileDiagnosticContract = `emit_late_activation_reconcile_diagnostic() {
	require_mode_0600_regular_file "$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE" late-activation-reconcile-capture-status
	[ "$(sed -n '1p' "$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE")" = captured ] ||
		fail "late activation reconcile log was not captured"
	hook_diagnostic_is_safe "$LATE_ACTIVATION_RECONCILE_LOG_FILE" ||
		fail "late activation reconcile log failed credential and format validation"
	cat "$LATE_ACTIVATION_RECONCILE_LOG_FILE" >&2
	missing_blocker_evidence=
	grep -F 'wait for release activation guard before persistence' \
		"$LATE_ACTIVATION_RECONCILE_LOG_FILE" >/dev/null ||
		missing_blocker_evidence="$missing_blocker_evidence activation-phase"
	grep -F 'late-activation-blocker.operator.ptah.dev' \
		"$LATE_ACTIVATION_RECONCILE_LOG_FILE" >/dev/null ||
		missing_blocker_evidence="$missing_blocker_evidence blocker-webhook"
	grep -F 'service "ptah-operator-e2e-missing-blocker" not found' \
		"$LATE_ACTIVATION_RECONCILE_LOG_FILE" >/dev/null ||
		missing_blocker_evidence="$missing_blocker_evidence missing-service"
	[ -z "$missing_blocker_evidence" ] ||
		fail "late activation reconcile log lacks exact blocker evidence:$missing_blocker_evidence"
}`

const failedHookEvidenceContract = `def hook_phase:
  .last_run.phase // "";

def hook_weight:
  if .weight == null then 0 else (.weight | tonumber) end;

(.hooks // []) as $hooks |
($hooks | map(select(hook_phase == "Failed"))) as $failed |
($hooks | map(select(
  .name == $expected_identity_name and
  .kind == "Job" and
  hook_weight == $expected_identity_weight and
  ((.events // []) | index("pre-upgrade") != null)))) as $identity |
(.version == $expected_revision) and
(.info.status == "failed") and
($identity | length == 1) and
($identity[0] |
  hook_phase == "Succeeded" and
  ((.last_run.started_at // "") | length > 0) and
  ((.last_run.completed_at // "") | length > 0)) and
($failed | length == 1) and
($failed[0] |
  .name == $expected_name and
  .kind == "Job" and
  hook_weight == $expected_weight and
  ((.events // []) | index("pre-upgrade") != null) and
  ((.last_run.started_at // "") | length > 0) and
  ((.last_run.completed_at // "") | length > 0)) and
($hooks | all(.[];
  if
    (((.events // []) | index("pre-upgrade")) != null) and
    (hook_weight > $expected_weight)
  then
    hook_phase == ""
  else
    true
  end))`

func verifyFailedHookEvidenceAssets(files e2eWiringFiles) error {
	filterContents, err := os.ReadFile(files.failedHookEvidence)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.failedHookEvidence, err)
	}
	if actual, expected := normalizedNonemptyLines(string(filterContents)), normalizedNonemptyLines(failedHookEvidenceContract); !equalStrings(actual, expected) {
		return fmt.Errorf("%s: failed Helm hook evidence filter must preserve the exact revision, status, hook identity, timestamp, and later-hook exclusion contract", files.failedHookEvidence)
	}

	selftestContents, err := os.ReadFile(files.failedHookEvidenceSelftest)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.failedHookEvidenceSelftest, err)
	}
	if err := verifyShellScriptEntrypoint(files.failedHookEvidenceSelftest, selftestContents); err != nil {
		return err
	}
	if err := verifyFailurePreservingExitTrap(files.failedHookEvidenceSelftest, selftestContents, "cleanup"); err != nil {
		return err
	}
	selftestContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("failed-hook evaluator implementation", `evaluate() {`),
		exactSourceLineSequence("revision-bound failed-hook evaluator", []string{
			`jq -e \`,
			`--argjson expected_revision 7 \`,
			`--arg expected_name ptah-crd-preflight \`,
			`--argjson expected_weight -60 \`,
			`--arg expected_identity_name ptah-hook-identity \`,
			`--argjson expected_identity_weight -105 \`,
			`-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$1" >/dev/null`,
		}),
		exactSourceLine("negative-fixture implementation", `expect_rejected() {`),
		exactSourceLine("negative-fixture mutation", `jq "$filter" "$WORK_DIR/valid.json" >"$fixture"`),
		exactSourceLineSequence("negative-fixture refusal", []string{
			`if evaluate "$fixture"; then`,
			`printf 'failed hook evidence self-test: accepted %s\n' "$name" >&2`,
			`exit 1`,
			`fi`,
		}),
		exactSourceLine("valid fixture evaluation", `evaluate "$WORK_DIR/valid.json"`),
		exactSourceLine("wrong revision refusal", `expect_rejected wrong-revision '.version = 8'`),
		exactSourceLine("wrong hook name refusal", `expect_rejected wrong-name '.hooks[1].name = "other-preflight"'`),
		exactSourceLine("wrong hook weight refusal", `expect_rejected wrong-weight '.hooks[1].weight = -59'`),
		exactSourceLine("wrong hook event refusal", `expect_rejected wrong-event '.hooks[1].events = ["post-upgrade"]'`),
		exactSourceLine("missing identity hook refusal", `expect_rejected missing-identity '.hooks[0].name = "other-identity"'`),
		exactSourceLine("failed identity hook refusal", `expect_rejected failed-identity '.hooks[0].last_run.phase = "Failed"'`),
		exactSourceLine("wrong identity hook weight refusal", `expect_rejected wrong-identity-weight '.hooks[0].weight = -104'`),
		exactSourceLine("multiple failed hooks refusal", `expect_rejected two-failures '.hooks[2].last_run = .hooks[1].last_run'`),
		exactSourceLine("later hook execution refusal", `expect_rejected later-hook-ran '.hooks[2].last_run = .hooks[0].last_run'`),
		exactSourceLine("malformed later hook weight refusal", `expect_rejected malformed-later-weight '.hooks[2].weight = "not-a-weight"'`),
		exactSourceLine("terminal failed-hook self-test evidence", `printf '%s\n' 'failed hook evidence self-test: PASS'`),
	}
	if err := verifyOrderedSourceContract(files.failedHookEvidenceSelftest, selftestContents, selftestContract); err != nil {
		return err
	}
	if bytes.Count(selftestContents, []byte("hack/failed-hook-evidence.jq")) != 1 {
		return fmt.Errorf("%s: self-test must invoke the audited failed-hook filter exactly once", files.failedHookEvidenceSelftest)
	}
	if err := rejectStaticControlFlowBypass(files.failedHookEvidenceSelftest, selftestContents, selftestContract[len(selftestContract)-1].pattern); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		files.failedHookEvidenceSelftest,
		selftestContents,
		selftestContract[3].pattern,
		selftestContract[5].pattern,
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(files.failedHookEvidenceSelftest, selftestContents, selftestContract[len(selftestContract)-1].pattern); err != nil {
		return err
	}

	staticContents, err := os.ReadFile(files.staticChecks)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.staticChecks, err)
	}
	if err := verifyShellScriptEntrypoint(files.staticChecks, staticContents); err != nil {
		return err
	}
	staticContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("failed-hook evidence self-test wiring", `"$(dirname -- "$0")/failed-hook-evidence-selftest.sh"`),
		exactSourceLine("static-check repository root setup", `unset CDPATH`),
	}
	if err := verifyOrderedSourceContract(files.staticChecks, staticContents, staticContract); err != nil {
		return err
	}
	if bytes.Count(staticContents, []byte("failed-hook-evidence-selftest.sh")) != 1 {
		return fmt.Errorf("%s: failed-hook evidence self-test must be wired exactly once", files.staticChecks)
	}
	if err := rejectStaticControlFlowBypass(files.staticChecks, staticContents, staticContract[1].pattern); err != nil {
		return err
	}
	return rejectEarlySuccessfulExit(files.staticChecks, staticContents, staticContract[1].pattern)
}

const admissionSchemaContract = `. as $document |

def require_exact_properties($schema_name; $expected):
  ($document.components.schemas[$schema_name] //
    error("OpenAPI schema is missing: " + $schema_name)) as $schema |
  ($schema.properties //
    error("OpenAPI schema has no properties: " + $schema_name)) as $properties |
  ($properties | keys) as $actual |
  if $actual == $expected then true
  else error("OpenAPI properties changed for " + $schema_name +
    ": actual=" + ($actual | tojson) + ", expected=" + ($expected | tojson))
  end;

require_exact_properties(
  "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta";
  [
    "annotations",
    "creationTimestamp",
    "deletionGracePeriodSeconds",
    "deletionTimestamp",
    "finalizers",
    "generateName",
    "generation",
    "labels",
    "managedFields",
    "name",
    "namespace",
    "ownerReferences",
    "resourceVersion",
    "selfLink",
    "uid"
  ]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.WebhookClientConfig";
  ["caBundle", "service", "url"]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.MutatingWebhook";
  [
    "admissionReviewVersions",
    "clientConfig",
    "failurePolicy",
    "matchConditions",
    "matchPolicy",
    "name",
    "namespaceSelector",
    "objectSelector",
    "reinvocationPolicy",
    "rules",
    "sideEffects",
    "timeoutSeconds"
  ]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.ValidatingWebhook";
  [
    "admissionReviewVersions",
    "clientConfig",
    "failurePolicy",
    "matchConditions",
    "matchPolicy",
    "name",
    "namespaceSelector",
    "objectSelector",
    "rules",
    "sideEffects",
    "timeoutSeconds"
  ]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.MutatingWebhookConfiguration";
  ["apiVersion", "kind", "metadata", "webhooks"]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.ValidatingWebhookConfiguration";
  ["apiVersion", "kind", "metadata", "webhooks"]
)`

func verifyAdmissionSchemaAssets(files e2eWiringFiles) error {
	filterContents, err := os.ReadFile(files.admissionSchemaContract)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.admissionSchemaContract, err)
	}
	if actual, expected := normalizedNonemptyLines(string(filterContents)), normalizedNonemptyLines(admissionSchemaContract); !equalStrings(actual, expected) {
		return fmt.Errorf("%s: admission OpenAPI filter must preserve the exact configuration, metadata, client, and webhook field inventories", files.admissionSchemaContract)
	}

	selftestContents, err := os.ReadFile(files.admissionSchemaSelftest)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.admissionSchemaSelftest, err)
	}
	if err := verifyShellScriptEntrypoint(files.admissionSchemaSelftest, selftestContents); err != nil {
		return err
	}
	if err := verifyFailurePreservingExitTrap(files.admissionSchemaSelftest, selftestContents, "cleanup"); err != nil {
		return err
	}
	selftestContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("cleanup implementation", `cleanup() {`),
		exactSourceLine("cleanup status capture", `status=$?`),
		exactSourceLine("cleanup status preservation", `exit "$status"`),
		exactSourceLine("exact fixture evaluation", `jq -e -f "$FILTER" "$fixture" >/dev/null`),
		exactSourceLine("added webhook field refusal", `if jq -e -f "$FILTER" "$extra" >/dev/null 2>&1; then`),
		exactSourceLine("missing metadata field refusal", `if jq -e -f "$FILTER" "$missing" >/dev/null 2>&1; then`),
		exactSourceLine("added configuration field refusal", `if jq -e -f "$FILTER" "$top_level" >/dev/null 2>&1; then`),
		exactSourceLine("terminal admission schema self-test evidence", `printf '%s\n' 'admission schema self-test: PASS'`),
	}
	if err := verifyOrderedSourceContract(files.admissionSchemaSelftest, selftestContents, selftestContract); err != nil {
		return err
	}
	if bytes.Count(selftestContents, []byte("hack/admission-schema-contract.jq")) != 1 {
		return fmt.Errorf("%s: self-test must bind the audited admission schema filter exactly once", files.admissionSchemaSelftest)
	}
	if err := rejectStaticControlFlowBypass(files.admissionSchemaSelftest, selftestContents, selftestContract[len(selftestContract)-1].pattern); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(files.admissionSchemaSelftest, selftestContents, selftestContract[len(selftestContract)-1].pattern); err != nil {
		return err
	}

	staticContents, err := os.ReadFile(files.staticChecks)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.staticChecks, err)
	}
	staticContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("admission schema self-test wiring", `"$(dirname -- "$0")/admission-schema-contract-selftest.sh"`),
		exactSourceLine("static-check repository root setup", `unset CDPATH`),
	}
	if err := verifyOrderedSourceContract(files.staticChecks, staticContents, staticContract); err != nil {
		return err
	}
	if bytes.Count(staticContents, []byte("admission-schema-contract-selftest.sh")) != 1 {
		return fmt.Errorf("%s: admission schema self-test must be wired exactly once", files.staticChecks)
	}
	if err := rejectStaticControlFlowBypass(files.staticChecks, staticContents, staticContract[1].pattern); err != nil {
		return err
	}
	return rejectEarlySuccessfulExit(files.staticChecks, staticContents, staticContract[1].pattern)
}

func verifyControllerObjectSchemaAssets(files e2eWiringFiles) error {
	filterContents, err := os.ReadFile(files.controllerSchemaContract)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.controllerSchemaContract, err)
	}
	filterDigest := fmt.Sprintf("%x", sha256.Sum256(filterContents))
	if filterDigest != controllerSchemaSHA256 {
		return fmt.Errorf(
			"%s: controller Job OpenAPI field inventory digest is %s, want reviewed digest %s",
			files.controllerSchemaContract,
			filterDigest,
			controllerSchemaSHA256,
		)
	}

	selftestContents, err := os.ReadFile(files.controllerSchemaSelftest)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.controllerSchemaSelftest, err)
	}
	if err := verifyShellScriptEntrypoint(files.controllerSchemaSelftest, selftestContents); err != nil {
		return err
	}
	if err := verifyFailurePreservingExitTrap(files.controllerSchemaSelftest, selftestContents, "cleanup"); err != nil {
		return err
	}
	selftestContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("cleanup implementation", `cleanup() {`),
		exactSourceLine("cleanup status capture", `status=$?`),
		exactSourceLine("cleanup status preservation", `exit "$status"`),
		exactSourceLine("reviewed-minor fixture evaluation", `evaluate 1.37 "$batch_fixture" "$core_fixture"`),
		exactSourceLine("added JobSpec field refusal", `if evaluate 1.37 "$job_extra" "$core_fixture" 2>/dev/null; then`),
		exactSourceLine("added PodSpec field refusal", `if evaluate 1.37 "$batch_fixture" "$pod_extra" 2>/dev/null; then`),
		exactSourceLine("added nested volume field refusal", `if evaluate 1.37 "$batch_fixture" "$volume_extra" 2>/dev/null; then`),
		exactSourceLine("added projection field refusal", `if evaluate 1.37 "$batch_fixture" "$projection_extra" 2>/dev/null; then`),
		exactSourceLine("missing reviewed schema refusal", `if evaluate 1.37 "$batch_fixture" "$missing_schema" 2>/dev/null; then`),
		exactSourceLine("unreviewed minor refusal", `if evaluate 1.38 "$batch_fixture" "$core_fixture" 2>/dev/null; then`),
		exactSourceLine("terminal controller object schema evidence", `printf '%s\n' 'controller object schema self-test: PASS'`),
	}
	if err := verifyOrderedSourceContract(files.controllerSchemaSelftest, selftestContents, selftestContract); err != nil {
		return err
	}
	if bytes.Count(selftestContents, []byte("hack/controller-object-schema-contract.jq")) != 1 {
		return fmt.Errorf("%s: self-test must bind the reviewed controller Job schema filter exactly once", files.controllerSchemaSelftest)
	}
	if err := rejectStaticControlFlowBypass(files.controllerSchemaSelftest, selftestContents, selftestContract[len(selftestContract)-1].pattern); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(files.controllerSchemaSelftest, selftestContents, selftestContract[len(selftestContract)-1].pattern); err != nil {
		return err
	}

	staticContents, err := os.ReadFile(files.staticChecks)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.staticChecks, err)
	}
	staticContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("controller object schema self-test wiring", `"$(dirname -- "$0")/controller-object-schema-contract-selftest.sh"`),
		exactSourceLine("static-check repository root setup", `unset CDPATH`),
	}
	if err := verifyOrderedSourceContract(files.staticChecks, staticContents, staticContract); err != nil {
		return err
	}
	if bytes.Count(staticContents, []byte("controller-object-schema-contract-selftest.sh")) != 1 {
		return fmt.Errorf("%s: controller object schema self-test must be wired exactly once", files.staticChecks)
	}
	if err := rejectStaticControlFlowBypass(files.staticChecks, staticContents, staticContract[1].pattern); err != nil {
		return err
	}
	return rejectEarlySuccessfulExit(files.staticChecks, staticContents, staticContract[1].pattern)
}

func normalizedNonemptyLines(source string) []string {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	lines := strings.Split(source, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func equalStrings(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

type auditedMakeRule struct {
	line             int
	raw              string
	operator         string
	conditionalDepth int
}

type auditedMakefile struct {
	lines []string
	rules map[string][]auditedMakeRule
	phony map[string]int
}

func parseAuditedMakefile(path string, contents []byte) (auditedMakefile, error) {
	if regexp.MustCompile(`(?m)^[ ]*(?:-?include|sinclude)[ \t]+|\$(?:\(|\{)(?:eval|file)[ \t]+`).Match(contents) {
		return auditedMakefile{}, fmt.Errorf("%s: Makefile must not inject unaudited rules through include, eval, or file directives", path)
	}
	parsed := auditedMakefile{
		lines: strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n"),
		rules: make(map[string][]auditedMakeRule),
		phony: make(map[string]int),
	}
	conditionalDepth := 0
	makeRule := regexp.MustCompile(`^[ ]*([^#:=][^:=#]*?)[ \t]*(::?|&:)(.*)$`)
	makeConditionalStart := regexp.MustCompile(`^(?:ifeq|ifneq|ifdef|ifndef)(?:[ \t(]|$)`)
	for index, line := range parsed.lines {
		trimmedLine := strings.TrimSpace(line)
		switch {
		case makeConditionalStart.MatchString(trimmedLine):
			conditionalDepth++
		case trimmedLine == "else" || strings.HasPrefix(trimmedLine, "else "):
			if conditionalDepth == 0 {
				return auditedMakefile{}, fmt.Errorf("%s:%d: unmatched Make else directive", path, index+1)
			}
		case trimmedLine == "endif" || strings.HasPrefix(trimmedLine, "endif "):
			if conditionalDepth == 0 {
				return auditedMakefile{}, fmt.Errorf("%s:%d: unmatched Make endif directive", path, index+1)
			}
			conditionalDepth--
		}

		match := makeRule.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if strings.Contains(match[1], "$") {
			return auditedMakefile{}, fmt.Errorf("%s:%d: dynamically named Make targets are outside the audited contract", path, index+1)
		}
		targets := strings.Fields(match[1])
		for _, target := range targets {
			if target == ".IGNORE" {
				return auditedMakefile{}, fmt.Errorf("%s:%d: .IGNORE rules are forbidden because they can suppress audited target failures", path, index+1)
			}
			parsed.rules[target] = append(parsed.rules[target], auditedMakeRule{
				line:             index,
				raw:              line,
				operator:         match[2],
				conditionalDepth: conditionalDepth,
			})
		}
		if len(targets) == 1 && targets[0] == ".PHONY" && match[2] == ":" && conditionalDepth == 0 {
			prerequisites := match[3]
			if comment := strings.IndexByte(prerequisites, '#'); comment >= 0 {
				prerequisites = prerequisites[:comment]
			}
			for _, target := range strings.Fields(prerequisites) {
				parsed.phony[target]++
			}
		}
	}
	if conditionalDepth != 0 {
		return auditedMakefile{}, fmt.Errorf("%s: unterminated Make conditional", path)
	}
	return parsed, nil
}

func (parsed auditedMakefile) requireTarget(path, target, header string) (auditedMakeRule, error) {
	rules := parsed.rules[target]
	if len(rules) != 1 {
		return auditedMakeRule{}, fmt.Errorf("%s: %s target must be declared exactly once", path, target)
	}
	rule := rules[0]
	if rule.raw != header || rule.operator != ":" {
		return auditedMakeRule{}, fmt.Errorf("%s:%d: %s target has unexpected prerequisites, whitespace, or rule syntax", path, rule.line+1, target)
	}
	if rule.conditionalDepth != 0 {
		return auditedMakeRule{}, fmt.Errorf("%s:%d: %s target must not be conditional", path, rule.line+1, target)
	}
	if parsed.phony[target] != 1 {
		return auditedMakeRule{}, fmt.Errorf("%s: %s target must have exactly one unconditional .PHONY declaration", path, target)
	}
	return rule, nil
}

func verifyMakeE2ETarget(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n")
	unsafeMakeControl := regexp.MustCompile(`(?m)^[ ]*(?:(?:export|override|private|unexport)[ \t]+)*(?:MAKEFLAGS|MFLAGS|MAKEFILES)(?:[ \t]*[:+?!]?=|[ \t]*(?:#.*)?$)`)
	if unsafeMakeControl.Match(contents) {
		return fmt.Errorf("%s: Makefile must not set or export MAKEFLAGS, MFLAGS, or MAKEFILES because they can suppress or replace the e2e recipe", path)
	}
	if regexp.MustCompile(`(?m)^[ ]*\.RECIPEPREFIX[ \t]*[:+?!]?=`).Match(contents) {
		return fmt.Errorf("%s: .RECIPEPREFIX must not alter audited recipe parsing", path)
	}
	shellAssignments := regexp.MustCompile(`(?m)^[ \t]*(?:(?:export|override|private)[ \t]+)*SHELL[ \t]*[:+?]?=[^\r\n]*$`).FindAll(contents, -1)
	if len(shellAssignments) != 1 || string(shellAssignments[0]) != "SHELL := /bin/sh" {
		return fmt.Errorf("%s: Make recipes must use exactly SHELL := /bin/sh", path)
	}
	if regexp.MustCompile(`(?m)^[ \t]*(?:(?:export|override|private)[ \t]+)*\.SHELLFLAGS[ \t]*[:+?]?=`).Match(contents) {
		return fmt.Errorf("%s: .SHELLFLAGS must not override Make recipe execution", path)
	}
	parsed, err := parseAuditedMakefile(path, contents)
	if err != nil {
		return err
	}
	rule, err := parsed.requireTarget(path, "e2e", "e2e:")
	if err != nil {
		return err
	}
	targetLine := rule.line

	var recipe []string
	for _, line := range lines[targetLine+1:] {
		if strings.HasPrefix(line, "\t") {
			command := strings.TrimSpace(strings.TrimPrefix(line, "\t"))
			if command != "" && !strings.HasPrefix(command, "#") {
				recipe = append(recipe, command)
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		break
	}
	const expected = `DOCKER_CONTEXT="$(DOCKER_CONTEXT)" ./hack/e2e-kind.sh`
	if len(recipe) != 1 || recipe[0] != expected {
		return fmt.Errorf("%s: e2e target must contain only %q", path, expected)
	}
	return nil
}

func verifyMakeRaceTargets(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for assignment, expected := range map[string]string{
		"RACE_MUTATION_SHARDS": "RACE_MUTATION_SHARDS ?= 8",
		"RACE_MUTATION_SHARD":  "RACE_MUTATION_SHARD ?=",
		"RACE_MUTATION_TESTS": "override RACE_MUTATION_TESTS := " +
			"TestVerifyE2EHarnessRejectsCriticalMutations|" +
			"TestVerifyE2EDataPlaneRejectsCriticalMutations|" +
			"TestVerifyFailedUpgradeEvidenceRejectsCriticalMutations|" +
			"TestVerifyE2EChildScriptsRejectCriticalMutations",
	} {
		pattern := regexp.MustCompile(`(?m)^(?:override[ \t]+)?` + regexp.QuoteMeta(assignment) + `[ \t]*[:+?!]?=[^\r\n]*$`)
		matches := pattern.FindAll(contents, -1)
		if len(matches) != 1 || string(matches[0]) != expected {
			return fmt.Errorf("%s: %s must have the exact audited race-shard assignment", path, assignment)
		}
	}
	parsed, err := parseAuditedMakefile(path, contents)
	if err != nil {
		return err
	}
	for target, contract := range map[string]struct {
		header string
		digest string
	}{
		"validate-race-shards": {header: "validate-race-shards:", digest: raceValidationRuleSHA256},
		"test-race-base":       {header: "test-race-base: validate-race-shards", digest: raceBaseRuleSHA256},
		"test-race-mutation":   {header: "test-race-mutation: validate-race-shards", digest: raceMutationRuleSHA256},
		"test-race":            {header: "test-race: validate-race-shards test-race-base", digest: raceAggregateSHA256},
	} {
		rule, ruleErr := parsed.requireTarget(path, target, contract.header)
		if ruleErr != nil {
			return ruleErr
		}
		ruleSource := exactMakeRule(parsed.lines, rule.line)
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(ruleSource)))
		if digest != contract.digest {
			return fmt.Errorf("%s: %s differs from the audited complete race-shard contract", path, target)
		}
	}
	return nil
}

func exactMakeRule(lines []string, start int) string {
	end := start + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "\t") {
		end++
	}
	return strings.Join(lines[start:end], "\n")
}

func verifyShellScriptEntrypoint(path string, contents []byte) error {
	if !bytes.HasPrefix(contents, []byte("#!/bin/sh\n\nset -eu\n")) {
		return fmt.Errorf("%s: lifecycle script must execute with #!/bin/sh and enable set -eu before commands", path)
	}
	return nil
}

func verifyFailurePreservingExitTrap(path string, contents []byte, cleanup string) error {
	expected := "trap " + cleanup + " EXIT"
	exitTraps := regexp.MustCompile(`(?m)^[ \t]*trap[ \t]+[^\r\n]*(?:^|[ \t])(?:EXIT|0)(?:[ \t]|$)[^\r\n]*\r?$`).FindAll(contents, -1)
	expectedCount := 0
	for _, raw := range exitTraps {
		line := strings.TrimSpace(string(raw))
		switch {
		case line == expected:
			expectedCount++
		case strings.HasPrefix(line, "trap - "):
			// A cleanup routine may disable its own trap before preserving the
			// captured status. This is not an alternate EXIT handler.
		default:
			return fmt.Errorf("%s: lifecycle script has an unaudited failure-preserving trap replacement %q", path, line)
		}
	}
	if expectedCount != 1 {
		return fmt.Errorf("%s: lifecycle script must have exactly one failure-preserving %s", path, expected)
	}
	return nil
}

func verifyLifecycleSource(contract lifecycleSourceContract) error {
	contents, err := os.ReadFile(contract.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", contract.path, err)
	}
	if len(contract.steps) == 0 {
		return fmt.Errorf("%s: lifecycle source contract is empty", contract.path)
	}
	if err := verifyShellScriptEntrypoint(contract.path, contents); err != nil {
		return err
	}
	if err := verifyFailurePreservingExitTrap(contract.path, contents, contract.exitTrap); err != nil {
		return err
	}
	if err := verifyOrderedSourceContract(contract.path, contents, contract.steps); err != nil {
		return err
	}
	completion := contract.steps[len(contract.steps)-1].pattern
	if err := rejectStaticControlFlowBypass(contract.path, contents, completion); err != nil {
		return err
	}
	for _, boundaries := range contract.successfulReturns {
		if err := rejectEarlySuccessfulReturn(contract.path, contents, boundaries.start, boundaries.completion); err != nil {
			return err
		}
	}
	return rejectEarlySuccessfulExit(contract.path, contents, completion)
}

func verifySingleDirectHelmInstallAttempt(path string, contents []byte) error {
	shellCode := maskShellHeredocBodies(contents)
	logicalShell := normalizeShellContinuations(shellCode)
	if bytes.Contains(shellCode, []byte{'`'}) {
		return fmt.Errorf("%s: legacy backtick command substitution is not allowed around the audited install", path)
	}
	if bytes.Contains(contents, []byte("<<")) {
		return fmt.Errorf("%s: shell here-document syntax is not allowed around the audited install", path)
	}
	const shellAssignment = `[A-Za-z_][A-Za-z0-9_]*=(?:"[^"\r\n]*"|'[^'\r\n]*'|[^ \t;&|"'\r\n]*)`
	const shellCommandBoundary = `(?:(?:^|;;&|;;|;&|&&|\|\||[;|&(){}])[ \t]*)`
	const shellControlPrefix = `(?:(?:if|elif|while|until|then|else|do)[ \t]+)?`
	indirectionChecks := []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{
			name:    "Helm function override",
			pattern: shellFunctionDeclaratorPattern("helm"),
		},
		{
			name:    "Helm alias override",
			pattern: shellAliasOverridePattern("helm"),
		},
		{
			name:    "command function override",
			pattern: shellFunctionDeclaratorPattern("command"),
		},
		{
			name:    "command alias override",
			pattern: shellAliasOverridePattern("command"),
		},
		{
			name: "env-launched Helm command",
			pattern: regexp.MustCompile(
				`(?m)` + shellCommandBoundary + shellControlPrefix + `(?:![ \t]+)*(?:` + shellAssignment + `[ \t]+)*` +
					`(?:command[ \t]+)*(?:[^ \t;&|]*/)?env[ \t]+(?:[^;&|\r\n]*[ \t])?(?:command[ \t]+)*(?:[^ \t;&|]*/)?helm(?:[ \t;&|]|$)`,
			),
		},
		{
			name: "Helm command-string launch",
			pattern: regexp.MustCompile(
				`(?m)` + shellCommandBoundary + shellControlPrefix + `(?:![ \t]+)*(?:` + shellAssignment + `[ \t]+)*` +
					`(?:(?:command[ \t]+)*(?:[^ \t;&|(){}]*/)?(?:sh|bash|dash|ksh|zsh)[ \t]+-[A-Za-z]*c[A-Za-z]*|(?:command[ \t]+)*eval)[ \t]+` +
					`(?:"[^"\r\n]*helm[^"\r\n]*[ \t]+install[^"\r\n]*"|'[^'\r\n]*helm[^'\r\n]*[ \t]+install[^'\r\n]*')`,
			),
		},
		{
			name: "Helm variable indirection",
			pattern: regexp.MustCompile(
				`(?m)^[ \t]*(?:(?:export|readonly)[ \t]+)?[A-Za-z_][A-Za-z0-9_]*=[ \t]*(?:helm|'helm'|"helm")(?:[ \t;#]|$)`,
			),
		},
		{
			name: "Helm argument-forwarding wrapper",
			pattern: regexp.MustCompile(
				`(?m)^[ \t]*(?:command[ \t]+)?(?:[^ \t;&|]*/)?helm[ \t]+(?:"\$(?:@|\*)"|'\$(?:@|\*)'|\$(?:@|\*))(?:[ \t;&|]|$)`,
			),
		},
	}
	for _, check := range indirectionChecks {
		if match := firstUnquotedShellMatch(shellCode, check.pattern); match != nil {
			line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
			return fmt.Errorf("%s:%d: %s is not allowed around the audited install", path, line, check.name)
		}
		if firstUnquotedShellMatch(logicalShell, check.pattern) != nil {
			return fmt.Errorf("%s: %s is not allowed around the audited install", path, check.name)
		}
	}
	hostShellLaunch := regexp.MustCompile(
		`(?m)` + shellCommandBoundary + shellControlPrefix + `(?:![ \t]+)*(?:` + shellAssignment + `[ \t]+)*` +
			`(?:(?:command|exec|time)[ \t]+)*(?:(?:[^ \t;&|(){}#\r\n]*/)?env[ \t]+(?:[^;&|\r\n]*[ \t])?)?` +
			`(?:[^ \t;&|(){}#\r\n]*/)?(?:sh|bash|dash|ksh|zsh)(?:[ \t]|$)`,
	)
	if firstUnquotedShellMatch(logicalShell, hostShellLaunch) != nil {
		return fmt.Errorf("%s: host shell command-string launch is not allowed around the audited install", path)
	}
	hostEvalLaunch := regexp.MustCompile(
		`(?m)` + shellCommandBoundary + shellControlPrefix + `(?:![ \t]+)*(?:` + shellAssignment + `[ \t]+)*(?:command[ \t]+)*eval(?:[ \t]|$)`,
	)
	if firstUnquotedShellMatch(logicalShell, hostEvalLaunch) != nil {
		return fmt.Errorf("%s: host shell command-string launch is not allowed around the audited install", path)
	}

	helmInstall := regexp.MustCompile(
		`(?m)` + shellCommandBoundary + shellControlPrefix + `(?:![ \t]+)*` +
			`(?:` + shellAssignment + `[ \t]+)*` +
			`(?:(?:command|exec|time)[ \t]+)*(?:[^ \t;&|]*/)?helm` +
			`(?:[ \t]+[^;&|\r\n]*)?[ \t]+install(?:[ \t;&|]|$)`,
	)
	attempts := helmInstall.FindAllIndex(logicalShell, -1)
	if len(attempts) != 1 {
		return fmt.Errorf("%s: predecessor Helm installation must have exactly one semantic install attempt, found %d", path, len(attempts))
	}
	return nil
}

func verifySingleShellFunctionDefinition(path string, contents []byte, name string) error {
	shellCode := normalizeShellContinuations(maskShellHeredocBodies(contents))
	definition := shellFunctionDeclaratorPattern(name)
	count := 0
	for _, match := range definition.FindAllIndex(shellCode, -1) {
		if !insideShellQuote(shellCode, match[0]) {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("%s: %s must have exactly one function definition, found %d", path, name, count)
	}
	return nil
}

func verifyExactShellFunction(path string, contents []byte, name, expected string) error {
	return verifyExactShellFunctionContract(
		path,
		contents,
		name,
		expected,
		"exact API-server feature gate contract",
	)
}

func verifyExactShellFunctionContract(path string, contents []byte, name, expected, description string) error {
	functionPattern := regexp.MustCompile(
		`(?ms)^` + regexp.QuoteMeta(name) + `\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`,
	)
	matches := functionPattern.FindAll(contents, -1)
	if len(matches) != 1 {
		return fmt.Errorf("%s: %s must have exactly one auditable function body, found %d", path, name, len(matches))
	}
	if !equalStrings(
		normalizedNonemptyLines(string(matches[0])),
		normalizedNonemptyLines(expected),
	) {
		return fmt.Errorf("%s: %s differs from the %s", path, name, description)
	}
	return nil
}

func verifyAuditedShellFunctionDigest(path string, contents []byte, name, expected, description string) error {
	functionPattern := regexp.MustCompile(
		`(?ms)^` + regexp.QuoteMeta(name) + `\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`,
	)
	matches := functionPattern.FindAll(contents, -1)
	if len(matches) != 1 {
		return fmt.Errorf("%s: %s must have exactly one auditable function body, found %d", path, name, len(matches))
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(matches[0]))
	if digest != expected {
		return fmt.Errorf("%s: %s digest %s differs from the %s", path, name, digest, description)
	}
	return nil
}

func normalizeShellContinuations(contents []byte) []byte {
	return regexp.MustCompile(`\\\r?\n`).ReplaceAll(contents, nil)
}

func shellFunctionDeclaratorPattern(name string) *regexp.Regexp {
	escapedName := regexp.QuoteMeta(name)
	return regexp.MustCompile(
		`(?m)^[ \t]*(?:function[ \t]+` + escapedName + `(?:[ \t]*\([ \t]*\)|[ \t]+|\r?$)|` +
			escapedName + `[ \t]*\([ \t]*\))`,
	)
}

func shellAliasOverridePattern(name string) *regexp.Regexp {
	escapedName := regexp.QuoteMeta(name)
	return regexp.MustCompile(
		`(?m)^[ \t]*alias[ \t]+(?:` + escapedName + `(?:[ \t]*=|[ \t]+)|` +
			`'` + escapedName + `=[^'\r\n]*'|"` + escapedName + `=[^"\r\n]*")`,
	)
}

func exactSourceLine(name, line string) sourceContractStep {
	return sourceContractStep{
		name:    name,
		pattern: sourceLinePattern(line),
	}
}

func sourceLinePattern(line string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(line) + `[ \t]*\r?$`)
}

func exactSourceLineSequence(name string, lines []string) sourceContractStep {
	var pattern strings.Builder
	pattern.WriteString(`(?m)^[ \t]*`)
	for index, line := range lines {
		if index > 0 {
			pattern.WriteString(`\r?\n[ \t]*`)
		}
		pattern.WriteString(regexp.QuoteMeta(line))
		pattern.WriteString(`[ \t]*`)
	}
	pattern.WriteString(`\r?$`)
	return sourceContractStep{name: name, pattern: regexp.MustCompile(pattern.String())}
}

func verifyOrderedSourceContract(path string, contents []byte, steps []sourceContractStep) error {
	previousEnd := 0
	for _, step := range steps {
		matches := step.pattern.FindAllIndex(contents, -1)
		if len(matches) != 1 {
			return fmt.Errorf("%s: expected exactly one %s contract step, found %d", path, step.name, len(matches))
		}
		if matches[0][0] < previousEnd {
			return fmt.Errorf("%s: %s contract step is out of order", path, step.name)
		}
		previousEnd = matches[0][1]
	}
	return nil
}

func rejectStaticControlFlowBypass(path string, contents []byte, completion *regexp.Regexp) error {
	shellCode := maskShellHeredocBodies(contents)
	completionMatch := firstUnquotedShellMatch(shellCode, completion)
	if completionMatch == nil {
		return fmt.Errorf("%s: terminal lifecycle evidence is missing", path)
	}
	prefix := shellCode[:completionMatch[1]]
	checks := []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{
			name: "always-false wrapper",
			pattern: regexp.MustCompile(
				`(?m)^[ \t]*(?:if[ \t]+(?:false|![ \t]+true)(?:[ \t]*;[ \t]*then)?|while[ \t]+false(?:[ \t]*;[ \t]*do)?|until[ \t]+true(?:[ \t]*;[ \t]*do)?|false[ \t]*&&|true[ \t]*\|\|)[^\r\n]*\r?$`,
			),
		},
		{
			name: "statically unconditional wrapper",
			pattern: regexp.MustCompile(
				`(?m)^[ \t]*(?:if[ \t]+(?:true|![ \t]+false)(?:[ \t]*;[ \t]*then)?|while[ \t]+true(?:[ \t]*;[ \t]*do)?|until[ \t]+false(?:[ \t]*;[ \t]*do)?|true[ \t]*&&|false[ \t]*\|\|)[^\r\n]*\r?$`,
			),
		},
	}
	for _, check := range checks {
		if match := firstUnquotedShellMatch(prefix, check.pattern); match != nil {
			line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
			return fmt.Errorf("%s:%d: %s can bypass audited lifecycle work", path, line, check.name)
		}
	}
	return nil
}

func rejectEarlySuccessfulExit(path string, contents []byte, completion *regexp.Regexp) error {
	shellCode := maskShellHeredocBodies(contents)
	completionMatch := firstUnquotedShellMatch(shellCode, completion)
	if completionMatch == nil {
		return fmt.Errorf("%s: terminal lifecycle evidence is missing", path)
	}
	earlySuccess := regexp.MustCompile(`(?m)^[ \t]*(?:(?:(?:builtin|command)[ \t]+)?exit(?:[ \t]+0+)?|exec[ \t]+(?:(?:/usr)?/bin/)?true)[ \t]*(?:;[ \t]*)?(?:#[^\r\n]*)?\r?$`)
	if match := firstUnquotedShellMatch(shellCode[:completionMatch[0]], earlySuccess); match != nil {
		line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
		return fmt.Errorf("%s:%d: unconditional successful exit precedes terminal lifecycle evidence", path, line)
	}
	topLevelFailFastDisable := regexp.MustCompile(`(?m)^set[ \t]+(?:\+[^ \t;#\r\n]*[eu][^ \t;#\r\n]*|\+o[ \t]+(?:errexit|nounset))[ \t]*(?:;[^\r\n]*)?(?:#[^\r\n]*)?\r?$`)
	if match := firstUnquotedShellMatch(shellCode[:completionMatch[0]], topLevelFailFastDisable); match != nil {
		line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
		return fmt.Errorf("%s:%d: top-level fail-fast mode is disabled before terminal lifecycle evidence", path, line)
	}
	return nil
}

type shellHeredoc struct {
	delimiter        []byte
	stripLeadingTabs bool
}

// maskShellHeredocBodies replaces here-document payloads and terminators with
// spaces while preserving byte offsets and line breaks. Shell payload text is
// data, so quotes or apparent commands in it must not affect control-flow
// checks on the surrounding script.
func maskShellHeredocBodies(contents []byte) []byte {
	masked := bytes.Clone(contents)
	pending := make([]shellHeredoc, 0, 1)
	readingHeredocs := false

	const (
		unquoted = iota
		singleQuoted
		doubleQuoted
		comment
	)
	state := unquoted

	for index := 0; index < len(contents); {
		if readingHeredocs {
			lineEnd := bytes.IndexByte(contents[index:], '\n')
			if lineEnd < 0 {
				lineEnd = len(contents)
			} else {
				lineEnd += index
			}
			line := contents[index:lineEnd]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			candidate := line
			if pending[0].stripLeadingTabs {
				candidate = bytes.TrimLeft(candidate, "\t")
			}
			for bodyIndex := index; bodyIndex < lineEnd; bodyIndex++ {
				masked[bodyIndex] = ' '
			}
			if bytes.Equal(candidate, pending[0].delimiter) {
				pending = pending[1:]
				readingHeredocs = len(pending) > 0
			}
			if lineEnd == len(contents) {
				break
			}
			index = lineEnd + 1
			continue
		}

		current := contents[index]
		switch state {
		case comment:
			if current == '\n' {
				state = unquoted
				readingHeredocs = len(pending) > 0
			}
			index++
		case singleQuoted:
			if current == '\'' {
				state = unquoted
			}
			index++
		case doubleQuoted:
			switch current {
			case '\\':
				if index+1 < len(contents) {
					index += 2
					continue
				}
			case '"':
				state = unquoted
			}
			index++
		default:
			switch current {
			case '\\':
				if index+1 < len(contents) {
					index += 2
					continue
				}
			case '\'':
				state = singleQuoted
			case '"':
				state = doubleQuoted
			case '#':
				if index == 0 || contents[index-1] == '\n' || contents[index-1] == ' ' || contents[index-1] == '\t' {
					state = comment
				}
			case '<':
				if heredoc, end, ok := parseShellHeredoc(contents, index); ok {
					pending = append(pending, heredoc)
					index = end
					continue
				}
			case '\n':
				readingHeredocs = len(pending) > 0
			}
			index++
		}
	}
	return masked
}

func parseShellHeredoc(contents []byte, offset int) (shellHeredoc, int, bool) {
	if offset+1 >= len(contents) || contents[offset+1] != '<' ||
		(offset > 0 && contents[offset-1] == '<') ||
		(offset+2 < len(contents) && contents[offset+2] == '<') {
		return shellHeredoc{}, offset, false
	}

	index := offset + 2
	stripLeadingTabs := false
	if index < len(contents) && contents[index] == '-' {
		stripLeadingTabs = true
		index++
	}
	for index < len(contents) && (contents[index] == ' ' || contents[index] == '\t') {
		index++
	}

	delimiter := make([]byte, 0, 16)
	started := false
	quote := byte(0)
	for index < len(contents) {
		current := contents[index]
		if quote == 0 {
			switch current {
			case ' ', '\t', '\r', '\n', ';', '|', '&', '(', ')', '<', '>':
				if !started {
					return shellHeredoc{}, offset, false
				}
				return shellHeredoc{delimiter: delimiter, stripLeadingTabs: stripLeadingTabs}, index, true
			case '\'', '"':
				started = true
				quote = current
				index++
				continue
			case '\\':
				started = true
				if index+1 >= len(contents) || contents[index+1] == '\n' {
					return shellHeredoc{}, offset, false
				}
				delimiter = append(delimiter, contents[index+1])
				index += 2
				continue
			}
		} else if current == quote {
			quote = 0
			index++
			continue
		} else if quote == '"' && current == '\\' {
			if index+1 >= len(contents) || contents[index+1] == '\n' {
				return shellHeredoc{}, offset, false
			}
			delimiter = append(delimiter, contents[index+1])
			index += 2
			continue
		}
		started = true
		delimiter = append(delimiter, current)
		index++
	}
	if !started || quote != 0 {
		return shellHeredoc{}, offset, false
	}
	return shellHeredoc{delimiter: delimiter, stripLeadingTabs: stripLeadingTabs}, index, true
}

func firstUnquotedShellMatch(contents []byte, pattern *regexp.Regexp) []int {
	for _, match := range pattern.FindAllIndex(contents, -1) {
		if !insideShellQuote(contents, match[0]) {
			return match
		}
	}
	return nil
}

func insideShellQuote(contents []byte, offset int) bool {
	const (
		unquoted = iota
		singleQuoted
		doubleQuoted
		comment
	)
	state := unquoted
	for index := 0; index < offset; index++ {
		current := contents[index]
		switch state {
		case comment:
			if current == '\n' {
				state = unquoted
			}
		case singleQuoted:
			if current == '\'' {
				state = unquoted
			}
		case doubleQuoted:
			switch current {
			case '\\':
				if index+1 < offset {
					index++
				}
			case '"':
				state = unquoted
			}
		default:
			switch current {
			case '\\':
				if index+1 < offset {
					index++
				}
			case '\'':
				state = singleQuoted
			case '"':
				state = doubleQuoted
			case '#':
				if index == 0 || contents[index-1] == '\n' || contents[index-1] == ' ' || contents[index-1] == '\t' {
					state = comment
				}
			}
		}
	}
	return state == singleQuoted || state == doubleQuoted
}

func rejectEarlySuccessfulReturn(path string, contents []byte, start, completion *regexp.Regexp) error {
	shellCode := maskShellHeredocBodies(contents)
	startMatch := firstUnquotedShellMatch(shellCode, start)
	if startMatch == nil {
		return fmt.Errorf("%s: lifecycle function boundaries are invalid", path)
	}
	functionBody := shellCode[startMatch[1]:]
	completionMatch := firstUnquotedShellMatch(functionBody, completion)
	if completionMatch == nil {
		return fmt.Errorf("%s: lifecycle function boundaries are invalid", path)
	}
	completionStart := startMatch[1] + completionMatch[0]
	earlyReturn := regexp.MustCompile(`(?m)^[ \t]*(?:(?:builtin|command)[ \t]+)?return(?:[ \t]+0+)?[ \t]*(?:;[ \t]*)?(?:#[^\r\n]*)?\r?$`)
	if match := firstUnquotedShellMatch(shellCode[startMatch[1]:completionStart], earlyReturn); match != nil {
		absoluteOffset := startMatch[1] + match[0]
		line := 1 + bytes.Count(contents[:absoluteOffset], []byte{'\n'})
		return fmt.Errorf("%s:%d: unconditional successful return precedes per-engine lifecycle evidence", path, line)
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
