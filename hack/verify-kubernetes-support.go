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
	reviewedJobAPISurfaceSHA256      = "cf7bfb98b59dce0740581a34ea11d1a4813239be28241dd11c91124b7435fa48"
	// These digests make workflow policy changes explicit. Semantic checks keep
	// failures actionable; the whole-file digests also cover setup steps that
	// could otherwise alter GITHUB_ENV, GITHUB_PATH, or later shell behavior.
	ciWorkflowSHA256       = "02860adce242a5852bd3640050c39d57fa24350c54d4c6928d2c4ce6a6b99b81"
	updateWorkflowSHA256   = "6c26ffcdfccc60a28f16e600ec6f29b22d139f3637979d880c4623833b4b6580"
	controllerSchemaSHA256 = "b73a7b8718abd34b4a8f45a1342c31c50690bf82358b378621dfbbe6e30892e5"

	ciSupportMatrixTimeoutMinutes     = 10
	ciVerifyTimeoutMinutes            = 20
	ciKubernetesE2ETimeoutMinutes     = 90
	ciKubernetesSupportTimeoutMinutes = 5
	releaseQueueAPIMarginMinutes      = 5
	releaseSupportPollTimeoutMinutes  = max(ciSupportMatrixTimeoutMinutes, ciVerifyTimeoutMinutes) + ciKubernetesE2ETimeoutMinutes + ciKubernetesSupportTimeoutMinutes + releaseQueueAPIMarginMinutes
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
			"Kubernetes dependency/support profile %d/%d differs from reviewed Job API boundary %d/%d; review the reachable Job/Pod fields and update the structural guard",
			compiledMinor,
			supportedMaximum,
			reviewedKubernetesAPIMinor,
			reviewedKubernetesSupportMaximum,
		)
	}
	if actualDigest != reviewedJobAPISurfaceSHA256 {
		return fmt.Errorf(
			"compiled reachable Job API surface digest is %s, want reviewed digest %s; review the Job/Pod JSON field graph before accepting dependency drift",
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

	visit(reflect.TypeOf(batchv1.JobSpec{}))
	sort.Slice(entries, func(left, right int) bool { return entries[left].Type < entries[right].Type })
	canonical, err := json.Marshal(entries)
	if err != nil {
		panic(fmt.Sprintf("marshal reachable Job API surface: %v", err))
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
		"DOCKER_CONTEXT: ${{ steps.docker-context.outputs.name }}",
		"KIND_NODE_IMAGE: ${{ matrix.node_image }}",
		"K8S_VERSION: ${{ matrix.kubernetes_version }}",
		"run: make e2e",
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
	for _, jobName := range []string{"support-matrix", "verify", "kubernetes-e2e", "kubernetes-support-gate"} {
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
		verifySteps[4].If != "" || verifySteps[4].Uses != "" || verifySteps[4].Run != "make verify" ||
		verifySteps[4].Shell != "bash" || verifySteps[4].WorkingDirectory != "" ||
		len(verifySteps[4].With) != 0 || !equalStringMap(verifySteps[4].Env, map[string]string{
		"CRD_SCHEMA_BASELINE_REF":              "${{ steps.crd-baseline.outputs.baseline }}",
		"CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE": "true",
	}) {
		return fmt.Errorf("%s: project verification must consume only the explicit audited CRD baseline", path)
	}

	e2e := workflow.Jobs["kubernetes-e2e"]
	if e2e.If != "" || e2e.TimeoutMinutes != ciKubernetesE2ETimeoutMinutes {
		return fmt.Errorf("%s: kubernetes-e2e must run unconditionally with a %d-minute timeout", path, ciKubernetesE2ETimeoutMinutes)
	}
	if !equalStringSet(e2e.Needs, []string{"support-matrix", "verify"}) {
		return fmt.Errorf("%s: kubernetes-e2e dependencies are %v", path, e2e.Needs)
	}
	if e2e.Strategy.FailFast == nil || *e2e.Strategy.FailFast ||
		!equalStringMap(e2e.Strategy.Matrix, map[string]string{
			"include": "${{ fromJSON(needs.support-matrix.outputs.matrix) }}",
		}) {
		return fmt.Errorf("%s: kubernetes-e2e strategy must consume only the verified dynamic matrix with fail-fast disabled", path)
	}
	lifecycleSteps := make([]workflowStep, 0, 1)
	for _, candidate := range e2e.Steps {
		if candidate.Run == "make e2e" {
			lifecycleSteps = append(lifecycleSteps, candidate)
		}
	}
	if len(lifecycleSteps) != 1 {
		return fmt.Errorf("%s: kubernetes-e2e must contain exactly one run: make e2e step", path)
	}
	lifecycle := lifecycleSteps[0]
	if lifecycle.If != "" || lifecycle.Shell != "bash" || lifecycle.WorkingDirectory != "" {
		return fmt.Errorf("%s: run: make e2e must be unconditional, run from the checkout root, and use explicit bash", path)
	}
	wantMatrixEnv := map[string]string{
		"DOCKER_CONTEXT":         "${{ steps.docker-context.outputs.name }}",
		"E2E_DIRECT_HOST_ACCESS": "1",
		"E2E_PTAH_REVISION":      "5451155ed00de348abbb6dbabc5370401dc23772",
		"E2E_PTAH_SOURCE_DIR":    "${{ runner.temp }}/ptah",
		"E2E_RUN_ID":             "ci-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.minor_slug }}",
		"KIND_NODE_IMAGE":        "${{ matrix.node_image }}",
		"K8S_VERSION":            "${{ matrix.kubernetes_version }}",
	}
	if !equalStringMap(lifecycle.Env, wantMatrixEnv) {
		return fmt.Errorf("%s: run: make e2e must use exactly the audited lifecycle environment bindings", path)
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
	if !equalStringSet(gate.Needs, []string{"support-matrix", "verify", "kubernetes-e2e"}) {
		return fmt.Errorf("%s: Kubernetes support gate dependencies are %v", path, gate.Needs)
	}
	step, err := requireWorkflowStep(path, "kubernetes-support-gate", gate, "require-results")
	if err != nil {
		return err
	}
	wantEnv := map[string]string{
		"SUPPORT_MATRIX_RESULT": "${{ needs.support-matrix.result }}",
		"VERIFY_RESULT":         "${{ needs.verify.result }}",
		"KUBERNETES_E2E_RESULT": "${{ needs.kubernetes-e2e.result }}",
	}
	if !equalStringMap(step.Env, wantEnv) {
		return fmt.Errorf("%s: Kubernetes support gate result bindings do not match its dependencies", path)
	}
	const wantGateRun = `set -euo pipefail
for result in \
  "$SUPPORT_MATRIX_RESULT" \
  "$VERIFY_RESULT" \
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
	workflow, _, err := readWorkflow(path)
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
	if !equalStringMap(preflight.Outputs, map[string]string{"source-sha": "${{ steps.support-evidence.outputs.source-sha }}"}) {
		return fmt.Errorf("%s: support preflight must expose its verified source SHA", path)
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
		"actions/runs/$run_id/jobs",
		".name == \"Kubernetes support gate\"",
		"poll_deadline_epoch=$(( $(date -u +%s) + SUPPORT_POLL_TIMEOUT_MINUTES * 60 ))",
		"remaining_seconds=$((poll_deadline_epoch - $(date -u +%s)))",
		"printf 'source-sha=%s\\n' \"$GITHUB_SHA\" >> \"$GITHUB_OUTPUT\"",
	}
	for _, marker := range requiredEvidence {
		if !strings.Contains(evidence.Run, marker) {
			return fmt.Errorf("%s: support preflight is missing exact-CI evidence marker %q", path, marker)
		}
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
	return nil
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
	jq -e --arg expected "$expected_api_server_feature_gates" '
      def component_commands($component):
        [
          .items[]
          | select(.metadata.labels.component == $component)
          | .spec.containers[]
          | .command[]
        ];

      (component_commands("kube-apiserver")) as $api_server |
      (component_commands("kube-controller-manager")) as $controller_manager |
      (component_commands("kube-scheduler")) as $scheduler |
      ([.items[] | select(.metadata.labels.component == "kube-apiserver")] | length) == 1 and
      ([.items[] | select(.metadata.labels.component == "kube-controller-manager")] | length) == 1 and
      ([.items[] | select(.metadata.labels.component == "kube-scheduler")] | length) == 1 and
      ($api_server | map(select(startswith("--feature-gates=")))) ==
        (if $expected == "" then [] else ["--feature-gates=" + $expected] end) and
      ($api_server | map(select(startswith("--runtime-config="))) | length) == 1 and
      ($controller_manager | map(select(startswith("--feature-gates=")))) == [] and
      ($scheduler | map(select(startswith("--feature-gates=")))) == []
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

func verifyE2EWiring(files e2eWiringFiles) error {
	if err := verifyMakeE2ETarget(files.makefile); err != nil {
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
		exactSourceLine("bounded CRD proof namespace", `CRD_PROOF_NAMESPACE=$(dns_name ptah-crd-proof "$identity")`),
		exactSourceLine("runtime generated-name boundary fixture", `RUNTIME_FULLNAME=$(dns_name ptah-runtime-generated-name-prefix-boundary-proof "$identity" 60)`),
		exactSourceLine("runtime generated-name boundary length", `[ "${#RUNTIME_FULLNAME}" -eq 60 ] || fail "runtime fullname boundary fixture must be exactly 60 characters"`),
		exactSourceLine("node readiness snapshot path", `NODE_READINESS_FILE=$WORK_DIR/node-readiness.json`),
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
			`if ! jq -e '.items | length > 0' "$NODE_READINESS_FILE" >/dev/null; then`,
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
			`((.items | length) > 0) and`,
			`all(.items[];`,
			`any((.status.conditions // [])[];`,
			`.type == "Ready" and .status == "True"`,
			`)`,
			`)`,
			`' "$NODE_READINESS_FILE" >/dev/null`,
			`}`,
		}),
		exactSourceLineSequence("hard node readiness requirement", []string{
			`require_ready_nodes() {`,
			`required_readiness_context=$1`,
			`if ! wait_for_ready_nodes "$required_readiness_context"; then`,
			`fail "infrastructure readiness check failed: $required_readiness_context"`,
			`fi`,
			`}`,
		}),
		exactSourceLine("API-server feature gate runtime assertion", `assert_api_server_feature_gate_scope() {`),
		exactSourceLine("API-server feature gate patch implementation", `append_api_server_feature_gate_patch() {`),
		exactSourceLine("API-server feature gate patch call", `append_api_server_feature_gate_patch "$K8S_MAJOR_MINOR" "$KIND_CONFIG"`),
		exactSourceLineSequence("kind cluster creation", []string{
			`kind create cluster \`,
			`--name "$CLUSTER_NAME" \`,
			`--image "$KIND_NODE_IMAGE" \`,
			`--config "$KIND_CONFIG" \`,
			`--kubeconfig "$KUBECONFIG_FILE" \`,
			`--wait 5m`,
			`require_ready_nodes "after kind cluster creation"`,
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
			`E2E_KUBERNETES_VERSION=$K8S_VERSION \`,
			`E2E_PHASE=uninstall \`,
			`"$ROOT_DIR/hack/e2e-crd-upgrade.sh"`,
		}),
		exactSourceLine("terminal Kubernetes lifecycle evidence", `printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"`),
	}
	if err := verifyOrderedSourceContract(harness, harnessContents, harnessContract); err != nil {
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
		"require_ready_nodes",
		"append_api_server_feature_gate_patch",
		"assert_api_server_feature_gate_scope",
	} {
		if err := verifySingleShellFunctionDefinition(harness, harnessContents, functionName); err != nil {
			return err
		}
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
		exactSourceLine("operation Pod generated-name binding", `[ "$CAPTURED_POD_GENERATE_NAME" = "${CAPTURED_JOB_NAME}-" ] ||`),
		exactSourceLine("operation Job generated-name boundary fixture", `EXTERNAL_PG_SCHEMA=e2e-postgresql-external-longpod`),
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
		exactSourceLineSequence("external lifecycle digest-selected source call", []string{
			`create_schema_resource "$EXTERNAL_PG_SCHEMA" PostgreSQL "$EXTERNAL_PG_SECRET" \`,
			`"$external_reference" "$EXTERNAL_PG_COORDINATION_KEY" \`,
			`e2e-verification-policy "$REGISTRY_AUTH_SECRET" Environment 45s "$QUIESCENT_INTERVAL"`,
		}),
		exactSourceLineSequence("operation generated-name boundary proof", []string{
			`[ "${#CAPTURED_JOB_NAME}" -eq 58 ] ||`,
			`fail "external PostgreSQL plan Job did not reach the generated-name truncation boundary"`,
			`[ "${#CAPTURED_POD_GENERATE_NAME}" -eq 59 ] ||`,
			`fail "external PostgreSQL plan Pod generateName did not cross the truncation boundary"`,
			`[ "${#CAPTURED_POD_NAME}" -eq 63 ] ||`,
			`fail "external PostgreSQL plan Pod did not preserve the bounded generated name"`,
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
	if err := rejectStaticControlFlowBypass(dataPlane, dataPlaneContents, dataPlaneContract[len(dataPlaneContract)-1].pattern); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		dataPlane,
		dataPlaneContents,
		dataPlaneContract[10].pattern,
		dataPlaneContract[16].pattern,
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		dataPlane,
		dataPlaneContents,
		dataPlaneContract[17].pattern,
		dataPlaneContract[20].pattern,
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
				exactSourceLine("late activation hook capture cleanup", `if [ -n "$LATE_ACTIVATION_HOOK_CAPTURE_PID" ]; then`),
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
				exactSourceLine("late activation hook capture arming implementation", `arm_late_activation_hook_log_capture() {`),
				exactSourceLine("late activation hook capture completion implementation", `finish_late_activation_hook_log_capture() {`),
				exactSourceLine("late activation hook diagnostic implementation", `emit_late_activation_hook_diagnostic() {`),
				exactSourceLineSequence("late activation hook captured status", []string{
					`[ "$(sed -n '1p' "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE")" = captured ] ||`,
					`fail "late activation hook diagnostic was not captured"`,
				}),
				exactSourceLineSequence("late activation hook diagnostic credential scan", []string{
					`if grep -F -f "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" "$LATE_ACTIVATION_HOOK_LOG_FILE" >/dev/null; then`,
					`fail "late activation hook diagnostic contained a protected task credential"`,
					`else`,
					`diagnostic_scan_status=$?`,
					`[ "$diagnostic_scan_status" -eq 1 ] || fail "late activation hook credential scan failed closed"`,
					`fi`,
				}),
				exactSourceLine("late activation hook diagnostic credential-shape scan", `fail "late activation hook diagnostic contained a credential-shaped value"`),
				exactSourceLineSequence("late activation hook exact blocker evidence", []string{
					`'wait for release activation guard before persistence' \`,
					`'late-activation-blocker.operator.ptah.dev' \`,
					`'service "ptah-operator-e2e-missing-blocker" not found'; do`,
				}),
				exactSourceLine("late activation hook safe diagnostic emission", `cat "$LATE_ACTIVATION_HOOK_LOG_FILE" >&2`),
				exactSourceLine("predecessor top-level Deployment recovery", `fail "candidate rollout guards blocked exact predecessor Deployment recovery for $deployment_name"`),
				exactSourceLine("late activation failure implementation", `prove_late_activation_failure_recovery() {`),
				exactSourceLine("late activation hook capture arming", `arm_late_activation_hook_log_capture`),
				exactSourceLineSequence("late activation Helm failure execution", []string{
					`if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \`,
					`--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \`,
					`--wait --timeout 2m >"$WORK_DIR/late-activation-failure.out" \`,
					`2>"$WORK_DIR/late-activation-failure.err"; then`,
				}),
				exactSourceLineSequence("late activation capture completion and evidence", []string{
					`fi`,
					`late_activation_capture_succeeded=false`,
					`if finish_late_activation_hook_log_capture; then`,
					`late_activation_capture_succeeded=true`,
					`fi`,
					`delete_late_activation_blocker`,
					`[ "$late_upgrade_succeeded" = false ] ||`,
					`fail "upgrade with a late activation blocker unexpectedly succeeded"`,
					`[ "$late_activation_capture_succeeded" = true ] ||`,
					`fail "late activation hook log capture process failed"`,
					`emit_late_activation_hook_diagnostic`,
				}),
				exactSourceLine("late activation failed status fail-closed jq", `jq -e --argjson expected_revision "$late_revision" \`),
				exactSourceLine("late activation failed hook identity argument", `--arg expected_reconcile_name "$EXPECTED_RECONCILE_HOOK_NAME" '`),
				exactSourceLineSequence("late activation failed revision evidence", []string{
					`[(.hooks // [])[] | select(.last_run.phase == "Failed")] as $failed |`,
					`.version == $expected_revision and`,
					`.info.status == "failed" and`,
					`($failed | length == 1) and`,
					`($failed[0] |`,
					`.name == $expected_reconcile_name and`,
					`.kind == "Job" and`,
					`(.weight == null or ((.weight | type) == "number" and .weight == 0)) and`,
					`((.events // []) | index("pre-upgrade") != null) and`,
				}),
				exactSourceLine("late activation marker remains uncommitted", `fail "late failure advanced the release activation marker"`),
				exactSourceLine("predecessor Deployment restore", `restore_runtime_deployment_snapshot "$CONTROLLER_DEPLOYMENT" "$controller_snapshot"`),
				exactSourceLine("predecessor late-failure recovery completion", `printf '%s\n' 'e2e crd: predecessor late-failure recovery passed'`),
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
				exactSourceLine("legacy Job bootstrap semantic boundary", `fail "legacy Job bootstrap probe did not reach the semantic controller-write boundary"`),
				exactSourceLine("legacy Job active structural denial", `fail "legacy Job post-activation probe lacked the exact structural guard denial"`),
				exactSourceLine("legacy plan activation boundary implementation", `prove_legacy_plan_activation_boundary() {`),
				exactSourceLine("legacy plan bootstrap semantic boundary", `fail "legacy plan bootstrap probe did not reach the semantic controller-write boundary"`),
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
				exactSourceLine("upgrade proof implementation", `run_upgrade_proof() {`),
				exactSourceLine("predecessor upgrade proof call", `run_predecessor_upgrade_proof`),
				exactSourceLine("runtime singleton proof call", `prove_runtime_singleton_guard`),
				exactSourceLine("controller downgrade proof call", `prove_controller_downgrade_guard`),
				exactSourceLine("uninstall proof implementation", `run_uninstall_proof() {`),
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
				exactSourceLine("Lease authorization proof", `printf '%s\n' 'e2e HA: verifying namespace-scoped Lease authorization'`),
				exactSourceLine("initial leader proof", `initial_holder=$(wait_for_leader "")`),
				exactSourceLine("leader failover proof", `second_holder=$(wait_for_leader "$initial_holder")`),
				exactSourceLine("post-failover operation proof", `operation_job=$(wait_for_admitted_operation_pod "$ha_schema_uid")`),
				exactSourceLine("terminal high-availability lifecycle evidence", `printf '%s\n' 'e2e HA: PASS one Lease, exact RBAC, Pod failover, and admitted post-failover operation'`),
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
		"arm_late_activation_hook_log_capture",
		"finish_late_activation_hook_log_capture",
		"emit_late_activation_hook_diagnostic",
	} {
		if err := verifySingleShellFunctionDefinition(files.crdUpgrade, crdUpgradeContents, functionName); err != nil {
			return err
		}
	}
	if err := verifyExactShellFunctionContract(
		files.crdUpgrade,
		crdUpgradeContents,
		"arm_late_activation_hook_log_capture",
		lateActivationHookCaptureArmContract,
		"exact resourceVersion-bound late activation hook capture arm contract",
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
		"emit_late_activation_hook_diagnostic",
		lateActivationHookDiagnosticContract,
		"exact credential-safe late activation hook diagnostic contract",
	); err != nil {
		return err
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

const lateActivationHookCaptureArmContract = `arm_late_activation_hook_log_capture() {
	[ -n "$EXPECTED_RECONCILE_HOOK_NAME" ] || fail "rendered reconcile hook name is unavailable"
	[ -z "$LATE_ACTIVATION_HOOK_CAPTURE_PID" ] || fail "late activation hook log capture is already armed"
	require_mode_0600_regular_file "$EXPECTED_CRD_UPGRADE_RENDER_FILE" expected-crd-upgrade-render
	mkdir -p "$WORK_DIR/go-cache"
	env GOCACHE="$WORK_DIR/go-cache" go -C "$ROOT_DIR" build -trimpath \
		-o "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ./hack/hooklogcapture
	[ -f "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ] &&
		[ ! -L "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ] &&
		[ -x "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ] ||
		fail "late activation hook log capture helper is not a regular executable"
	"$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" \
		--kubeconfig "$E2E_KUBECONFIG" \
		--namespace "$E2E_OPERATOR_NAMESPACE" \
		--job-name "$EXPECTED_RECONCILE_HOOK_NAME" \
		--render-file "$EXPECTED_CRD_UPGRADE_RENDER_FILE" \
		--log-file "$LATE_ACTIVATION_HOOK_LOG_FILE" \
		--status-file "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE" \
		--ready-file "$LATE_ACTIVATION_HOOK_CAPTURE_READY_FILE" \
		--error-file "$LATE_ACTIVATION_HOOK_CAPTURE_ERRORS_FILE" \
		--timeout 3m >/dev/null 2>&1 &
	LATE_ACTIVATION_HOOK_CAPTURE_PID=$!
	activation_capture_arm_grace=0
	while [ ! -s "$LATE_ACTIVATION_HOOK_CAPTURE_READY_FILE" ] &&
		kill -0 "$LATE_ACTIVATION_HOOK_CAPTURE_PID" >/dev/null 2>&1 &&
		[ "$activation_capture_arm_grace" -lt 15 ]; do
		sleep 1
		activation_capture_arm_grace=$((activation_capture_arm_grace + 1))
	done
	if [ "$(sed -n '1p' "$LATE_ACTIVATION_HOOK_CAPTURE_READY_FILE" 2>/dev/null)" != ready ] ||
		[ "$(sed -n '1p' "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE" 2>/dev/null)" != watching ] ||
		! kill -0 "$LATE_ACTIVATION_HOOK_CAPTURE_PID" >/dev/null 2>&1; then
		kill "$LATE_ACTIVATION_HOOK_CAPTURE_PID" >/dev/null 2>&1 || true
		finish_late_activation_hook_log_capture || true
		fail "late activation hook log capture did not arm before Helm"
	fi
	for activation_capture_file in \
		"$LATE_ACTIVATION_HOOK_LOG_FILE" \
		"$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE" \
		"$LATE_ACTIVATION_HOOK_CAPTURE_ERRORS_FILE" \
		"$LATE_ACTIVATION_HOOK_CAPTURE_READY_FILE"; do
		require_mode_0600_regular_file "$activation_capture_file" late-activation-hook-capture-file
	done
}`

const lateActivationHookCaptureFinishContract = `finish_late_activation_hook_log_capture() {
	[ -n "$LATE_ACTIVATION_HOOK_CAPTURE_PID" ] || fail "late activation hook log capture is not armed"
	late_activation_capture_grace=0
	while kill -0 "$LATE_ACTIVATION_HOOK_CAPTURE_PID" >/dev/null 2>&1 &&
		[ "$late_activation_capture_grace" -lt 15 ]; do
		case "$(sed -n '1p' "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE" 2>/dev/null)" in
		captured | failed | canceled) break ;;
		esac
		sleep 1
		late_activation_capture_grace=$((late_activation_capture_grace + 1))
	done
	case "$(sed -n '1p' "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE" 2>/dev/null)" in
	captured | failed | canceled) ;;
	*)
		kill "$LATE_ACTIVATION_HOOK_CAPTURE_PID" >/dev/null 2>&1 || true
		;;
	esac
	late_activation_capture_status=0
	wait "$LATE_ACTIVATION_HOOK_CAPTURE_PID" >/dev/null 2>&1 || late_activation_capture_status=$?
	LATE_ACTIVATION_HOOK_CAPTURE_PID=
	return "$late_activation_capture_status"
}`

const lateActivationHookDiagnosticContract = `emit_late_activation_hook_diagnostic() {
	require_mode_0600_regular_file "$LATE_ACTIVATION_HOOK_LOG_FILE" late-activation-hook-log
	require_mode_0600_regular_file "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE" late-activation-hook-capture-status
	require_mode_0600_regular_file "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" identity-hook-credential-patterns
	[ -s "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" ] ||
		fail "late activation hook credential scanner has no protected patterns"
	[ "$(sed -n '1p' "$LATE_ACTIVATION_HOOK_CAPTURE_STATUS_FILE")" = captured ] ||
		fail "late activation hook diagnostic was not captured"
	if grep -F -f "$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE" "$LATE_ACTIVATION_HOOK_LOG_FILE" >/dev/null; then
		fail "late activation hook diagnostic contained a protected task credential"
	else
		diagnostic_scan_status=$?
		[ "$diagnostic_scan_status" -eq 1 ] || fail "late activation hook credential scan failed closed"
	fi
	if grep -Eq '(^|[^[:alnum:]_-])eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+($|[^[:alnum:]_-])|[Aa]uthorization:[[:space:]]*|[Bb]earer[[:space:]]+|://[^[:space:]@/:]+:[^[:space:]@/]+@' \
		"$LATE_ACTIVATION_HOOK_LOG_FILE"; then
		fail "late activation hook diagnostic contained a credential-shaped value"
	fi
	diagnostic_size=$(wc -c <"$LATE_ACTIVATION_HOOK_LOG_FILE" | tr -d '[:space:]')
	diagnostic_lines=$(awk 'END { print NR + 0 }' "$LATE_ACTIVATION_HOOK_LOG_FILE")
	if [ "$diagnostic_size" -gt 8192 ] || [ "$diagnostic_lines" -ne 1 ] ||
		! LC_ALL=C grep -Eq '^ptah-crd-manager: [[:print:]]+$' "$LATE_ACTIVATION_HOOK_LOG_FILE"; then
		fail "late activation hook diagnostic has an unsafe format"
	fi
	for activation_error_marker in \
		'wait for release activation guard before persistence' \
		'late-activation-blocker.operator.ptah.dev' \
		'service "ptah-operator-e2e-missing-blocker" not found'; do
		grep -F "$activation_error_marker" "$LATE_ACTIVATION_HOOK_LOG_FILE" >/dev/null ||
			fail "late activation hook diagnostic lacks exact blocker evidence"
	done
	cat "$LATE_ACTIVATION_HOOK_LOG_FILE" >&2
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
	if regexp.MustCompile(`(?m)^[ ]*(?:-?include|sinclude)[ \t]+|\$\((?:eval|file)[ \t]+`).Match(contents) {
		return fmt.Errorf("%s: Makefile must not inject unaudited rules through include, eval, or file directives", path)
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
	phony := false
	targetLine := -1
	e2eRuleCount := 0
	conditionalDepth := 0
	makeRule := regexp.MustCompile(`^[ ]*([^#:=][^:=#]*?)[ \t]*(::?|&:)(.*)$`)
	makeConditionalStart := regexp.MustCompile(`^(?:ifeq|ifneq|ifdef|ifndef)(?:[ \t(]|$)`)
	makeIgnore := regexp.MustCompile(`^[ \t]*\.IGNORE[ \t]*:(.*)$`)
	for index, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		switch {
		case makeConditionalStart.MatchString(trimmedLine):
			conditionalDepth++
		case trimmedLine == "else" || strings.HasPrefix(trimmedLine, "else "):
			if conditionalDepth == 0 {
				return fmt.Errorf("%s:%d: unmatched Make else directive", path, index+1)
			}
		case trimmedLine == "endif" || strings.HasPrefix(trimmedLine, "endif "):
			if conditionalDepth == 0 {
				return fmt.Errorf("%s:%d: unmatched Make endif directive", path, index+1)
			}
			conditionalDepth--
		}
		if match := makeIgnore.FindStringSubmatch(line); match != nil {
			ignoredTargets := strings.Fields(match[1])
			ignoreE2E := len(ignoredTargets) == 0
			for _, target := range ignoredTargets {
				ignoreE2E = ignoreE2E || target == "e2e"
			}
			if ignoreE2E {
				return fmt.Errorf("%s: Make must not ignore e2e recipe failures", path)
			}
		}
		if strings.HasPrefix(line, ".PHONY:") {
			for _, target := range strings.Fields(strings.TrimPrefix(line, ".PHONY:")) {
				if target == "e2e" {
					phony = true
				}
			}
		}
		match := makeRule.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if strings.Contains(match[1], "$") {
			return fmt.Errorf("%s:%d: dynamically named Make targets are outside the audited e2e contract", path, index+1)
		}
		isE2ETarget := false
		for _, target := range strings.Fields(match[1]) {
			if target == "e2e" {
				isE2ETarget = true
				break
			}
		}
		if !isE2ETarget {
			continue
		}
		e2eRuleCount++
		if line != "e2e:" || match[2] != ":" || strings.TrimSpace(match[3]) != "" {
			return fmt.Errorf("%s:%d: e2e target must be the unconditional exact rule e2e:", path, index+1)
		}
		if conditionalDepth != 0 {
			return fmt.Errorf("%s:%d: e2e target must not be conditional", path, index+1)
		}
		targetLine = index
	}
	if conditionalDepth != 0 {
		return fmt.Errorf("%s: unterminated Make conditional", path)
	}
	if !phony {
		return fmt.Errorf("%s: e2e target must be phony", path)
	}
	if targetLine < 0 || e2eRuleCount != 1 {
		return fmt.Errorf("%s: e2e target must be declared exactly once", path)
	}

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
