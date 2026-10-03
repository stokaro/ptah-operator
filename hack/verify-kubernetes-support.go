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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

const (
	manifestPath                 = "support/kubernetes.json"
	e2eSuitesPath                = "support/e2e-suites.json"
	goModPath                    = "go.mod"
	chartPath                    = "charts/ptah-operator/Chart.yaml"
	workflowPath                 = ".github/workflows/ci.yml"
	updateWorkflowPath           = ".github/workflows/update-kubernetes-support.yml"
	releaseWorkflowPath          = ".github/workflows/release.yml"
	docsPath                     = "docs/site/src/content/docs/support/kubernetes.md"
	makefilePath                 = "Makefile"
	e2eHarnessPath               = "hack/e2e-kind.sh"
	e2eSupportImageResolverPath  = "hack/e2e-kubernetes-support-image.sh"
	e2eKindConfigPath            = "testdata/e2e/kind.yaml.tmpl"
	e2eKindIsolationWorkerPath   = "testdata/e2e/kind-isolation-worker.yaml.tmpl"
	apiServerEndpointFilterPath  = "hack/api-server-endpoint-inventory.jq"
	e2eStaticPath                = "hack/e2e-static.sh"
	admissionSchemaContractPath  = "hack/admission-schema-contract.jq"
	admissionSchemaSelftestPath  = "hack/admission-schema-contract-selftest.sh"
	controllerSchemaContractPath = "hack/controller-object-schema-contract.jq"
	controllerSchemaSelftestPath = "hack/controller-object-schema-contract-selftest.sh"

	verificationMaxAgeDays = 35

	reviewedKubernetesAPIMinor       = 37
	reviewedKubernetesSupportMaximum = 37
	reviewedJobAPISurfaceSHA256      = "8d6f538effe84aeb02de351456b9f387bb8b66b2f20df11d391c252ffd49189c"

	// The Helm the chart-rendering jobs install. One pin, so what CI renders
	// and what a release renders are the same program.
	helmSetupAction = "azure/setup-helm@1a275c3b69536ee54be43f2070a358922e12c8d4"
	helmVersion     = "v4.3.0"

	ciSupportMatrixTimeoutMinutes = 10
	// The verify job outlasts both go test timeouts plus what runs before and
	// between them, which verifyJobOutlastsTests holds.
	ciVerifyTimeoutMinutes = 55
	// makeTestTimeoutMinutes is the -timeout the Makefile's test target gives
	// go test, which verifyMakeRaceTargets pins.
	makeTestTimeoutMinutes = 30
	// ciVerifyBeforeTestMinutes is what runs in the verify job before go test
	// starts: the job's setup steps and the checks verify-source runs ahead of
	// test. It took four minutes on acd17c4 and 93b209b with the rolling build
	// cache, and this is twice that, for a cold one.
	ciVerifyBeforeTestMinutes = 8
	// makeEnvtestTimeoutMinutes is the -timeout the Makefile's test-envtest
	// target gives go test, which verifyEnvtestPins pins. verify-source runs
	// it after test, in the same job.
	makeEnvtestTimeoutMinutes = 10
	// ciEnvtestFetchMinutes is what test-envtest spends before its go test
	// starts: building setup-envtest and, on a cache miss, downloading the
	// control plane. Both took under a minute; this is twice that.
	ciEnvtestFetchMinutes             = 2
	ciRaceTimeoutMinutes              = 20
	ciKubernetesE2ETimeoutMinutes     = 180
	ciPrepareImagesTimeoutMinutes     = 45
	ciKubernetesSupportTimeoutMinutes = 5
	ciLifecycleTimingsTimeoutMinutes  = 10
	releaseQueueAPIMarginMinutes      = 5
	releasePreflightOverheadMinutes   = 10
	// The latest a CI run can end with every job at its limit, read off the
	// needs in ci.yml. A lifecycle starts once the verification and the shared
	// images are done, and the images wait for the support matrix. The gate
	// needs the lifecycles and the race detector, which starts with the run;
	// the published timings need only the lifecycles. The release preflight
	// waits for the whole run to complete, so it waits for whichever of those
	// two ends last. verifyCIRunBound holds this to the graph itself.
	ciLifecycleEndMinutes             = max(ciSupportMatrixTimeoutMinutes+ciPrepareImagesTimeoutMinutes, ciVerifyTimeoutMinutes) + ciKubernetesE2ETimeoutMinutes
	ciRunEndMinutes                   = max(max(ciRaceTimeoutMinutes, ciLifecycleEndMinutes)+ciKubernetesSupportTimeoutMinutes, ciLifecycleEndMinutes+ciLifecycleTimingsTimeoutMinutes)
	releaseSupportPollTimeoutMinutes  = ciRunEndMinutes + releaseQueueAPIMarginMinutes
	releasePreflightJobTimeoutMinutes = releaseSupportPollTimeoutMinutes + releasePreflightOverheadMinutes
)

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
	// The acceptance suite this job runs, in the acceptance matrix only. A job
	// is one minor and one suite, so the four suites of a minor run at once
	// against clusters of their own. The plain matrix keeps its old shape,
	// because the release workflow reads it to enumerate supported minors.
	Suite        string `json:"suite,omitempty"`
	SuiteSlug    string `json:"suite_slug,omitempty"`
	SuiteSummary string `json:"suite_summary,omitempty"`
}

type parsedRelease struct {
	release
	major int
	minor int
	patch int
}

func main() {
	output := flag.String("output", "verify",
		"output mode: verify, proposal, matrix, acceptance, or helm-range")
	nowValue := flag.String("now", "", "UTC date used for freshness validation (YYYY-MM-DD; defaults to today)")
	flag.Parse()
	proposal := *output == "proposal"

	now, err := validationDate(*nowValue)
	if err != nil {
		fatal(err)
	}
	suites, err := loadE2ESuites(e2eSuitesPath)
	if err != nil {
		fatal(err)
	}
	if err := verifyE2ESuiteCoverage(suites, e2eHarnessPath); err != nil {
		fatal(err)
	}
	if err := verifyE2ESuiteIsolationWorker(suites); err != nil {
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
	if err := verifyEnvtestPins(makefilePath, parsed, proposal); err != nil {
		fatal(err)
	}
	if err := verifyUpdateWorkflow(updateWorkflowPath); err != nil {
		fatal(err)
	}
	if err := verifyReleaseWorkflow(releaseWorkflowPath); err != nil {
		fatal(err)
	}
	if err := verifyCancelWorkflow(cancelWorkflowPath); err != nil {
		fatal(err)
	}
	if err := verifyDocumentation(docsPath, parsed); err != nil {
		fatal(err)
	}
	if err := verifyE2EWiring(e2eWiringFiles{
		makefile:                 makefilePath,
		harness:                  e2eHarnessPath,
		supportImageResolver:     e2eSupportImageResolverPath,
		kindConfig:               e2eKindConfigPath,
		kindIsolationWorker:      e2eKindIsolationWorkerPath,
		apiServerEndpointFilter:  apiServerEndpointFilterPath,
		staticChecks:             e2eStaticPath,
		admissionSchemaContract:  admissionSchemaContractPath,
		admissionSchemaSelftest:  admissionSchemaSelftestPath,
		controllerSchemaContract: controllerSchemaContractPath,
		controllerSchemaSelftest: controllerSchemaSelftestPath,
	}); err != nil {
		fatal(err)
	}

	switch *output {
	case "verify":
		fmt.Printf("Kubernetes support window verified: %s-%s (%d minors), %d acceptance suites covering %d phases\n",
			parsed[0].Minor, parsed[len(parsed)-1].Minor, len(parsed), len(suites.Suites), e2eSuiteCoveredPhases(suites))
	case "proposal":
		fmt.Printf("Kubernetes support proposal validated: %s-%s (%d minors); ordinary verification still enforces the frozen API boundary\n", parsed[0].Minor, parsed[len(parsed)-1].Minor, len(parsed))
	case "matrix", "acceptance":
		minors := make([]matrixEntry, 0, len(parsed))
		for _, item := range parsed {
			minors = append(minors, matrixEntry{
				Minor:             item.Minor,
				MinorSlug:         strings.ReplaceAll(item.Minor, ".", "-"),
				KubernetesVersion: fmt.Sprintf("%d.%d.%d", item.major, item.minor, item.patch),
				NodeImage:         item.NodeImage,
				KindVersion:       manifest.KindVersion,
			})
		}
		entries := minors
		if *output == "acceptance" {
			entries = e2eSuiteMatrix(minors, suites)
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

var (
	envtestKubernetesVersion = regexp.MustCompile(`(?m)^ENVTEST_KUBERNETES_VERSION[ \t]*[:?]?=[ \t]*(\S*)[ \t]*$`)
	setupEnvtestVersion      = regexp.MustCompile(`(?m)^SETUP_ENVTEST_VERSION[ \t]*[:?]?=[ \t]*(\S*)[ \t]*$`)
	envtestIndex             = regexp.MustCompile(`(?m)^ENVTEST_INDEX[ \t]*[:?]?=[ \t]*(\S*)[ \t]*$`)
	exactSemver              = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
	commitPinnedEnvtestIndex = regexp.MustCompile(`^https://raw\.githubusercontent\.com/kubernetes-sigs/controller-tools/[0-9a-f]{40}/envtest-releases\.yaml$`)
)

// verifyEnvtestPins holds the envtest control plane to the support window and
// to pins that are pins. The suites under test/envtest decide what the API
// server does with the chart's policies and the CRDs; an API server from a
// release the chart does not support measures a server nobody runs, and a
// setup-envtest named by branch or an index read from HEAD would let two runs
// of one commit start different binaries.
//
// A proposal moves the window before anyone has reviewed the envtest version,
// so it is not held to the new window here; the ordinary verification of the
// pull request that carries the proposal is, and names the pin to move.
func verifyEnvtestPins(path string, releases []parsedRelease, proposal bool) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	value := func(pattern *regexp.Regexp, name string) (string, error) {
		matches := pattern.FindAllStringSubmatch(string(contents), -1)
		if len(matches) != 1 || matches[0][1] == "" {
			return "", fmt.Errorf("%s: %s must be assigned exactly once, to a value", path, name)
		}
		return matches[0][1], nil
	}
	setupVersion, err := value(setupEnvtestVersion, "SETUP_ENVTEST_VERSION")
	if err != nil {
		return err
	}
	if !exactSemver.MatchString(setupVersion) || !strings.HasPrefix(setupVersion, "v") {
		return fmt.Errorf("%s: SETUP_ENVTEST_VERSION %q is not an exact vX.Y.Z module version", path, setupVersion)
	}
	index, err := value(envtestIndex, "ENVTEST_INDEX")
	if err != nil {
		return err
	}
	if !commitPinnedEnvtestIndex.MatchString(index) {
		return fmt.Errorf("%s: ENVTEST_INDEX %q is not the controller-tools envtest index at an exact commit", path, index)
	}
	version, err := value(envtestKubernetesVersion, "ENVTEST_KUBERNETES_VERSION")
	if err != nil {
		return err
	}
	parts := exactSemver.FindStringSubmatch(version)
	if parts == nil || strings.HasPrefix(version, "v") {
		return fmt.Errorf("%s: ENVTEST_KUBERNETES_VERSION %q is not an exact X.Y.Z release", path, version)
	}
	// The suites run inside make verify-source, after make test, under the
	// timeout the verify job's limit is budgeted against.
	suites := fmt.Sprintf("PTAH_REQUIRE_ENVTEST=1 $(GO) test -count=1 -timeout=%dm ./test/envtest/...", makeEnvtestTimeoutMinutes)
	if strings.Count(string(contents), suites) != 1 {
		return fmt.Errorf("%s: make test-envtest must run the suites exactly once as %q", path, suites)
	}
	if !regexp.MustCompile(`(?m)^verify-source:[^\n#]* test test-envtest(?:[ \t]|$)`).Match(contents) {
		return fmt.Errorf("%s: verify-source must run test-envtest right after test", path)
	}
	if proposal {
		return nil
	}
	minor := parts[1] + "." + parts[2]
	supported := make([]string, 0, len(releases))
	for _, release := range releases {
		if release.Minor == minor {
			return nil
		}
		supported = append(supported, release.Minor)
	}
	return fmt.Errorf("%s: ENVTEST_KUBERNETES_VERSION %s is outside the supported window %s; move it, and ENVTEST_INDEX, to a supported release",
		path, version, strings.Join(supported, ", "))
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

	// JobSpec reaches PodSpec through the template. JobStatus is a separate API
	// graph the admission boundary relies on to authenticate terminal progress.
	// PodStatus is not: nothing in the operator reads it, so it carried no
	// admission-relevant surface and only widened what a dependency bump forced
	// a re-review of.
	for _, root := range []reflect.Type{
		reflect.TypeOf(batchv1.JobSpec{}),
		reflect.TypeOf(batchv1.JobStatus{}),
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
	return verifyCIWorkflowSemantics(path, workflow, contents)
}

// ciCancelsEverySupersededRun accepts one value: the literal true, which cancels
// the older run on every ref, master included.
//
// That is the policy, and this is where it is held. A newer commit's run is the
// only one whose verdict anyone builds on, so an older run still going only
// holds the queue. `false` lets a stale run race the new one, and an expression
// that spares master -- the rule this repository used to have -- queues every
// merge behind a run that proves a tree nobody will build on again. Any other
// expression decides, without review, which runs get to finish.
func ciCancelsEverySupersededRun(node yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!bool" && node.Value == "true"
}

func verifyCIWorkflowSemantics(path string, workflow workflowDocument, contents []byte) error {
	if workflow.Concurrency.Group != "ci-${{ github.workflow }}-${{ github.ref }}" ||
		!ciCancelsEverySupersededRun(workflow.Concurrency.CancelInProgress) {
		return fmt.Errorf(
			"%s: CI must cancel a superseded run on every ref, master included: cancel-in-progress is true and nothing else",
			path,
		)
	}
	required := []string{
		"go run ./hack/verify-kubernetes-support.go -output=matrix",
		"fromJSON(needs.support-matrix.outputs.acceptance)",
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
	for _, jobName := range []string{
		"support-matrix", "verify", "race", "kubernetes-e2e", "kubernetes-support-gate",
	} {
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
		// Every supported minor against every suite. A job is one pair, so the
		// suites of a minor run at once against clusters of their own.
		"acceptance":  "${{ steps.matrix.outputs.acceptance }}",
		"ptah_commit": "${{ steps.ptah.outputs.commit }}",
		// The image build needs a Kubernetes version, a node image and a kind
		// version like any harness run, and builds nothing that depends on
		// them. They come out of the same validated matrix rather than a
		// literal, so a minor leaving the window cannot leave a pin behind.
		"prepare_kubernetes_version": "${{ steps.matrix.outputs.prepare_kubernetes_version }}",
		"prepare_node_image":         "${{ steps.matrix.outputs.prepare_node_image }}",
		"prepare_kind_version":       "${{ steps.matrix.outputs.prepare_kind_version }}",
	}) {
		return fmt.Errorf("%s: support-matrix outputs must bind exactly to the matrix and Ptah pin step outputs", path)
	}
	matrixStep, err := requireWorkflowStep(path, "support-matrix", supportMatrix, "matrix")
	if err != nil {
		return err
	}
	// The three values the image build is handed are read out of the matrix
	// this step just validated, with jq -e, and assigned before anything is
	// printed: set -e acts on a failed substitution in an assignment and
	// ignores one in printf's arguments, which exported "null" for a missing
	// field.
	const wantMatrixRun = `set -euo pipefail
matrix="$(go run ./hack/verify-kubernetes-support.go -output=matrix)"
acceptance="$(go run ./hack/verify-kubernetes-support.go -output=acceptance)"
kubernetes_version="$(jq -er '.[-1].kubernetes_version' <<<"$matrix")"
node_image="$(jq -er '.[-1].node_image' <<<"$matrix")"
kind_version="$(jq -er '.[-1].kind_version' <<<"$matrix")"
{
  echo "matrix=$matrix"
  echo "acceptance=$acceptance"
  printf 'prepare_kubernetes_version=%s\n' "$kubernetes_version"
  printf 'prepare_node_image=%s\n' "$node_image"
  printf 'prepare_kind_version=%s\n' "$kind_version"
} >> "$GITHUB_OUTPUT"
`
	if matrixStep.If != "" || matrixStep.Shell != "bash" || matrixStep.Run != wantMatrixRun {
		return fmt.Errorf("%s: support-matrix step must unconditionally export the verified dynamic matrix", path)
	}
	// The Ptah pin travels the same way the Kubernetes matrix does, and for the
	// same reason: the lifecycle job needs the tested commit before it can
	// check anything out, and a literal in the job would be a second
	// declaration of a claim support/ptah.json publishes.
	ptahStep, err := requireWorkflowStep(path, "support-matrix", supportMatrix, "ptah")
	if err != nil {
		return err
	}
	const wantPtahRun = `set -euo pipefail
commit="$(go run ./hack/verifyptahsupport -output=commit)"
echo "commit=$commit" >> "$GITHUB_OUTPUT"
`
	if ptahStep.If != "" || ptahStep.Shell != "bash" || ptahStep.Run != wantPtahRun {
		return fmt.Errorf("%s: support-matrix step must unconditionally export the verified Ptah commit", path)
	}

	verifyJob := workflow.Jobs["verify"]
	if err := verifyJobOutlastsTests(path, verifyJob.TimeoutMinutes); err != nil {
		return err
	}
	if verifyJob.If != "" || verifyJob.TimeoutMinutes != ciVerifyTimeoutMinutes {
		return fmt.Errorf("%s: verify must run unconditionally with a %d-minute timeout", path, ciVerifyTimeoutMinutes)
	}
	verifySteps, err := requireWorkflowStepOrder(path, "verify", verifyJob, []string{
		"checkout", "setup-go", "verify-build-cache", "verify-support", "crd-baseline", "verify-helm",
		"shellcheck", "promtool", "envtest-assets", "client-build-config", "project-verify",
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
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
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
		"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		map[string]string{"go-version-file": "go.mod", "cache-dependency-path": "go.sum"},
	); err != nil {
		return err
	}
	if err := verifyGoBuildCacheStep(path, "verify", verifySteps[2], "verify"); err != nil {
		return err
	}
	if verifySteps[3].Name != "Verify Kubernetes support window" ||
		verifySteps[3].If != "" || verifySteps[3].Uses != "" ||
		verifySteps[3].Run != "go run ./hack/verify-kubernetes-support.go" ||
		verifySteps[3].Shell != "bash" || verifySteps[3].WorkingDirectory != "" ||
		len(verifySteps[3].With) != 0 || len(verifySteps[3].Env) != 0 {
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
	if verifySteps[4].Name != "Select exact CRD schema history baseline" ||
		verifySteps[4].If != "" || verifySteps[4].Uses != "" ||
		verifySteps[4].Run != wantCRDBaselineRun || verifySteps[4].Shell != "bash" ||
		verifySteps[4].WorkingDirectory != "" || len(verifySteps[4].With) != 0 ||
		!equalStringMap(verifySteps[4].Env, map[string]string{
			"CURRENT_SHA":           "${{ github.sha }}",
			"EVENT_BEFORE_SHA":      "${{ github.event.before }}",
			"EVENT_NAME":            "${{ github.event_name }}",
			"PULL_REQUEST_BASE_SHA": "${{ github.event.pull_request.base.sha }}",
		}) {
		return fmt.Errorf("%s: crd-baseline must select the exact audited event-specific Git commit", path)
	}
	// ShellCheck findings are version-dependent, and the runner image ships
	// The chart-render tests shell out to Helm, and a job without it fails on
	// tests nothing changed. Pinning the version here keeps what CI renders the
	// same across runs; the lifecycle job pins the same one.
	if err := verifyHelmSetupStep(path, "verify", verifySteps[5]); err != nil {
		return err
	}
	// whatever it ships. The static gate refuses a version other than the one
	// support/tools.json declares, so this step has to install that one before
	// verification runs, and it has to read the version, the URL and the digest
	// from that same file rather than repeating them here.
	if verifySteps[6].Name != "Install the pinned ShellCheck" ||
		verifySteps[6].If != "" || verifySteps[6].Uses != "" ||
		verifySteps[6].Shell != "bash" || verifySteps[6].WorkingDirectory != "" ||
		len(verifySteps[6].With) != 0 || len(verifySteps[6].Env) != 0 {
		return fmt.Errorf("%s: the pinned ShellCheck install must be an unconditional bash step with no inputs", path)
	}
	for _, required := range []string{
		"support/tools.json",
		"sha256sum --check",
		".shellcheck.version",
		".shellcheck.linuxAmd64Url",
		".shellcheck.linuxAmd64Sha256",
	} {
		if !strings.Contains(verifySteps[6].Run, required) {
			return fmt.Errorf("%s: the pinned ShellCheck install does not read %q", path, required)
		}
	}
	// promtool runs the alerting rules the chart renders against their
	// scenarios. The same rule as ShellCheck: the exact version from
	// support/tools.json, checked against its digest, and verification below
	// requires the binary, so a job without it fails rather than skipping the
	// rule tests.
	if verifySteps[7].Name != "Install the pinned promtool" ||
		verifySteps[7].If != "" || verifySteps[7].Uses != "" ||
		verifySteps[7].Shell != "bash" || verifySteps[7].WorkingDirectory != "" ||
		len(verifySteps[7].With) != 0 || len(verifySteps[7].Env) != 0 {
		return fmt.Errorf("%s: the pinned promtool install must be an unconditional bash step with no inputs", path)
	}
	for _, required := range []string{
		"support/tools.json",
		"sha256sum --check",
		".promtool.version",
		".promtool.linuxAmd64Url",
		".promtool.linuxAmd64Sha256",
	} {
		if !strings.Contains(verifySteps[7].Run, required) {
			return fmt.Errorf("%s: the pinned promtool install does not read %q", path, required)
		}
	}
	// The envtest suites start the kube-apiserver and etcd the Makefile pins,
	// which setup-envtest fetches and checks against the digests in a
	// commit-pinned index whether or not this cache hits. The cache only saves
	// the download: its key follows the Makefile, where the pins live, and a
	// miss restores the newest earlier store, which already holds the pinned
	// release unless the pin moved. make verify-source reads the store from
	// ENVTEST_BIN_DIR below, so the two paths are one value.
	if verifySteps[8].Name != "Cache the envtest control plane" {
		return fmt.Errorf("%s: verify envtest cache step has unexpected name %q", path, verifySteps[8].Name)
	}
	if err := verifyUpdaterActionStep(path, "verify", verifySteps[8],
		"actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9", envtestCacheInputs()); err != nil {
		return err
	}
	// The client build configuration is checked where a pull request sees it:
	// a release reads its platforms out of that file, so one goreleaser refuses
	// is a release that cannot be cut.
	if verifySteps[9].Name != "Check the client build configuration" ||
		!strings.HasPrefix(verifySteps[9].Uses, "goreleaser/goreleaser-action@") ||
		verifySteps[9].Run != "" || verifySteps[9].With["args"] != "check" {
		return fmt.Errorf("%s: the client build configuration is not checked with goreleaser", path)
	}
	if verifySteps[10].Name != "Run project verification" ||
		verifySteps[10].If != "" || verifySteps[10].Uses != "" || verifySteps[10].Run != "make verify-source" ||
		verifySteps[10].Shell != "bash" || verifySteps[10].WorkingDirectory != "" ||
		len(verifySteps[10].With) != 0 || !equalStringMap(verifySteps[10].Env, map[string]string{
		"CRD_SCHEMA_BASELINE_REF":              "${{ steps.crd-baseline.outputs.baseline }}",
		"CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE": "true",
		"ENVTEST_BIN_DIR":                      envtestCacheInputs()["path"],
		"PTAH_REQUIRE_PROMTOOL":                "1",
	}) {
		return fmt.Errorf("%s: project verification must consume only the explicit audited CRD baseline, "+
			"require promtool, and read the cached envtest store", path)
	}

	if err := verifyRaceJob(path, workflow); err != nil {
		return err
	}

	prepare := workflow.Jobs["prepare-images"]
	if prepare.Name != "Build the shared task images" {
		return fmt.Errorf("%s: the shared-image job must be named %q", path, "Build the shared task images")
	}
	if prepare.If != "" || prepare.TimeoutMinutes != ciPrepareImagesTimeoutMinutes {
		return fmt.Errorf("%s: prepare-images must run unconditionally with a %d-minute timeout",
			path, ciPrepareImagesTimeoutMinutes)
	}
	if !equalStringSet(prepare.Needs, []string{"support-matrix"}) {
		return fmt.Errorf("%s: prepare-images dependencies are %v", path, prepare.Needs)
	}
	prepareStep, err := requireWorkflowStep(path, "prepare-images", prepare, "images")
	if err != nil {
		return err
	}
	if prepareStep.Run != "make e2e" || prepareStep.If != "" || prepareStep.Shell != "bash" ||
		prepareStep.WorkingDirectory != "" {
		return fmt.Errorf("%s: the shared images must be built by an unconditional run: make e2e", path)
	}
	// The same driver as a lifecycle, stopped where the images exist and no
	// cluster does, reading the same commit and the same catalog pin.
	if !equalStringMap(prepareStep.Env, map[string]string{
		"DOCKER_CONTEXT":         "${{ steps.docker-context.outputs.name }}",
		"E2E_DIRECT_HOST_ACCESS": "1",
		"E2E_IMAGE_EXPORT_DIR":   "${{ runner.temp }}/task-images",
		"E2E_PTAH_REVISION":      "${{ needs.support-matrix.outputs.ptah_commit }}",
		"E2E_PTAH_SOURCE_DIR":    "${{ runner.temp }}/ptah",
		"E2E_RUN_ID":             "ci-${{ github.run_id }}-${{ github.run_attempt }}-images",
		"E2E_STOP_AFTER":         "images",
		"E2E_TIMING_CONTEXT":     "${{ runner.temp }}/timing-context-images.json",
		"E2E_TIMING_LEDGER":      "${{ runner.temp }}/timings-images.jsonl",
		"KIND_NODE_IMAGE":        "${{ needs.support-matrix.outputs.prepare_node_image }}",
		"K8S_VERSION":            "${{ needs.support-matrix.outputs.prepare_kubernetes_version }}",
	}) {
		return fmt.Errorf("%s: run: make e2e in prepare-images must use exactly the audited image-build bindings", path)
	}
	if err := verifySharedImageHandover(path, prepare, e2eImagesArtifactName); err != nil {
		return err
	}

	e2e := workflow.Jobs["kubernetes-e2e"]
	if e2e.If != "" || e2e.TimeoutMinutes != ciKubernetesE2ETimeoutMinutes {
		return fmt.Errorf("%s: kubernetes-e2e must run unconditionally with a %d-minute timeout", path, ciKubernetesE2ETimeoutMinutes)
	}
	if !equalStringSet(e2e.Needs, []string{"support-matrix", "verify", "prepare-images"}) {
		return fmt.Errorf("%s: kubernetes-e2e dependencies are %v", path, e2e.Needs)
	}
	if e2e.Strategy.FailFast == nil || *e2e.Strategy.FailFast ||
		!equalStringMap(scalarMatrix(e2e.Strategy.Matrix), map[string]string{
			"include": "${{ fromJSON(needs.support-matrix.outputs.acceptance) }}",
		}) {
		return fmt.Errorf("%s: kubernetes-e2e strategy must consume only the verified dynamic matrix with fail-fast disabled", path)
	}
	if err := verifySharedImageCollection(path, e2e, e2eImagesArtifactName); err != nil {
		return err
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
		"DOCKER_CONTEXT":         "${{ steps.docker-context.outputs.name }}",
		"E2E_DIRECT_HOST_ACCESS": "1",
		// No Ptah source reaches a lifecycle job: the executor arrives built,
		// and the harness checks it against this pin before loading it.
		"E2E_PREBUILT_IMAGE_DIR":   "${{ runner.temp }}/task-images",
		"E2E_PTAH_REVISION":        "${{ needs.support-matrix.outputs.ptah_commit }}",
		"E2E_RELEASE_CHART_OUTPUT": "${{ runner.temp }}/ptah-operator-${{ matrix.minor_slug }}.tgz",
		"E2E_RUN_ID":               "ci-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.minor_slug }}-${{ matrix.suite_slug }}",
		// The suite this job runs. The driver reads the phases it names out of
		// support/e2e-suites.json, and runs no other.
		"E2E_SUITE": "${{ matrix.suite }}",
		// The stage ledger and the run's identity are named outside the work
		// directory the harness removes when it succeeds: a passing run's
		// timings are the baseline the next change is measured against.
		"E2E_TIMING_LEDGER":  "${{ runner.temp }}/timings-${{ matrix.minor_slug }}-${{ matrix.suite_slug }}.jsonl",
		"E2E_TIMING_CONTEXT": "${{ runner.temp }}/timing-context-${{ matrix.minor_slug }}-${{ matrix.suite_slug }}.json",
		"KIND_NODE_IMAGE":    "${{ matrix.node_image }}",
		"K8S_VERSION":        "${{ matrix.kubernetes_version }}",
	}
	if !equalStringMap(lifecycle.Env, wantMatrixEnv) {
		return fmt.Errorf("%s: run: make e2e must use exactly the audited lifecycle environment bindings", path)
	}
	// The three lifecycle jobs run at once, so their cache is scoped by minor:
	// one shared key would have them race to save the same entry and only the
	// last writer's work would survive.
	e2eCache, err := requireWorkflowStep(path, "kubernetes-e2e", e2e, "e2e-build-cache")
	if err != nil {
		return err
	}
	if err := verifyGoBuildCacheStep(
		path, "kubernetes-e2e", e2eCache, "e2e-${{ matrix.minor_slug }}-${{ matrix.suite_slug }}",
	); err != nil {
		return err
	}
	upload, err := requireWorkflowStep(path, "kubernetes-e2e", e2e, "release-chart-evidence")
	if err != nil {
		return err
	}
	if upload.Name != "Preserve exact installed release chart" || lifecycleIndex < 0 ||
		lifecycleIndex+1 >= len(e2e.Steps) || e2e.Steps[lifecycleIndex+1].ID != upload.ID {
		return fmt.Errorf("%s: installed chart evidence must immediately follow the complete lifecycle", path)
	}
	// One chart per minor, exported by the suite whose phases are the install,
	// the upgrade and the uninstall. The release workflow reads these by minor,
	// so a second suite uploading the same name would be two answers to one
	// question -- and a condition naming any other suite would leave the minor
	// with no chart at all.
	if upload.If != "${{ matrix.suite == 'lifecycle' }}" {
		return fmt.Errorf(
			"%s: the installed chart must be exported by the lifecycle suite, and its condition is %q",
			path, upload.If)
	}
	unconditionalUpload := upload
	unconditionalUpload.If = ""
	if err := verifyUpdaterActionStep(
		path,
		"kubernetes-e2e",
		unconditionalUpload,
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

	// The published timings are outside the gate, and the run is not complete
	// until they are, so their limit is part of what the release waits for.
	timings, err := requireWorkflowJob(path, workflow, "lifecycle-timings")
	if err != nil {
		return err
	}
	if timings.TimeoutMinutes != ciLifecycleTimingsTimeoutMinutes || timings.If != "${{ !cancelled() }}" ||
		!equalStringSet(timings.Needs, []string{"kubernetes-e2e"}) {
		return fmt.Errorf("%s: lifecycle-timings must follow the lifecycles with if: !cancelled() and a %d-minute timeout",
			path, ciLifecycleTimingsTimeoutMinutes)
	}
	if err := verifyCIRunBound(path, workflow, ciRunEndMinutes); err != nil {
		return err
	}

	gate := workflow.Jobs["kubernetes-support-gate"]
	if gate.Name != "Kubernetes support gate" {
		return fmt.Errorf("%s: stable support gate name must be %q", path, "Kubernetes support gate")
	}
	if gate.If != "${{ !cancelled() }}" {
		return fmt.Errorf("%s: Kubernetes support gate must run after unsuccessful dependencies and stop on cancellation with if: !cancelled()", path)
	}
	if gate.TimeoutMinutes != ciKubernetesSupportTimeoutMinutes {
		return fmt.Errorf("%s: Kubernetes support gate timeout must be %d minutes", path, ciKubernetesSupportTimeoutMinutes)
	}
	if !equalStringSet(gate.Needs, []string{
		"support-matrix", "verify", "race", "prepare-images", "kubernetes-e2e",
	}) {
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
		// The images every lifecycle loaded are part of the verdict: a matrix
		// that ran against images nobody built proved nothing about this commit.
		"PREPARE_IMAGES_RESULT": "${{ needs.prepare-images.result }}",
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
  "$PREPARE_IMAGES_RESULT" \
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

// verifyJobOutlastsTests holds the verify job's limit above both go test
// timeouts verify-source runs and what runs before and between them: make
// test, then the envtest control plane's fetch, then make test-envtest. At or
// under that sum, the job limit ends a hung test first, and the run shows a
// canceled job instead of the goroutine dump and the name of the running test
// that Go's timeout prints.
func verifyJobOutlastsTests(path string, limit int) error {
	budget := ciVerifyBeforeTestMinutes + makeTestTimeoutMinutes + ciEnvtestFetchMinutes + makeEnvtestTimeoutMinutes
	if limit <= budget {
		return fmt.Errorf("%s: verify's %d-minute limit must exceed make test's %d-minute go test timeout plus the %d minutes before it, "+
			"and make test-envtest's %d-minute timeout plus the %d minutes its control plane takes to fetch: %d minutes",
			path, limit, makeTestTimeoutMinutes, ciVerifyBeforeTestMinutes,
			makeEnvtestTimeoutMinutes, ciEnvtestFetchMinutes, budget)
	}
	return nil
}

// verifyRaceJob holds the race detector to one unconditional job under the name
// the support gate reads and a release requires. It runs make test-race, which
// verifyMakeRaceTargets holds to every package but the shell mutation suites:
// the verify job runs those without the detector.
func verifyRaceJob(path string, workflow workflowDocument) error {
	race := workflow.Jobs["race"]
	if race.Name != "Race detector" {
		return fmt.Errorf("%s: the job the support gate needs and a release requires must be named %q", path, "Race detector")
	}
	if race.If != "" || len(race.Needs) != 0 ||
		race.RunsOn != "ubuntu-latest" || race.TimeoutMinutes != ciRaceTimeoutMinutes ||
		len(race.Permissions) != 0 || race.Environment != "" || race.Strategy.FailFast != nil ||
		len(race.Strategy.Matrix) != 0 {
		return fmt.Errorf("%s: race must be an unconditional isolated ubuntu-latest job with a %d-minute timeout", path, ciRaceTimeoutMinutes)
	}
	raceSteps, err := requireWorkflowStepOrder(path, "race", race, []string{
		"race-checkout", "race-setup-go", "race-helm", "race-build-cache", "project-race",
	})
	if err != nil {
		return err
	}
	if err := verifyRaceSetupSteps(path, "race", raceSteps[0], raceSteps[1]); err != nil {
		return err
	}
	if err := verifyHelmSetupStep(path, "race", raceSteps[2]); err != nil {
		return err
	}
	if err := verifyGoBuildCacheStep(path, "race", raceSteps[3], "race"); err != nil {
		return err
	}
	if raceSteps[4].Name != "Run race coverage without the shell mutation suites" ||
		raceSteps[4].If != "" || raceSteps[4].Uses != "" || raceSteps[4].Run != "make test-race" ||
		raceSteps[4].Shell != "bash" || raceSteps[4].WorkingDirectory != "" ||
		len(raceSteps[4].With) != 0 || len(raceSteps[4].Env) != 0 {
		return fmt.Errorf("%s: race coverage must be the unconditional audited make test-race invocation", path)
	}
	return nil
}

func verifyRaceSetupSteps(path, jobName string, checkout, setupGo workflowStep) error {
	if checkout.Name != "Check out repository" {
		return fmt.Errorf("%s: %s checkout step has unexpected name %q", path, jobName, checkout.Name)
	}
	if err := verifyUpdaterActionStep(
		path,
		jobName,
		checkout,
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		map[string]string{"fetch-depth": "0", "persist-credentials": "false"},
	); err != nil {
		return err
	}
	if setupGo.Name != "Set up Go" {
		return fmt.Errorf("%s: %s Go setup step has unexpected name %q", path, jobName, setupGo.Name)
	}
	return verifyUpdaterActionStep(
		path,
		jobName,
		setupGo,
		"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		map[string]string{"go-version-file": "go.mod", "cache-dependency-path": "go.sum"},
	)
}

// githubJobTimeoutMinutes is what GitHub allows a job that sets no
// timeout-minutes.
const githubJobTimeoutMinutes = 360

// verifyCIRunBound holds the release preflight's wait to the run it waits for.
// It walks the needs in the workflow, starts each job when the last of its
// needs ends and lets it run to its limit, and requires the latest end to be
// the bound the preflight derives from its constants. A job the formula does
// not know about, an edge it does not model, or a limit that moves the end
// makes the two disagree, and the preflight would either give up on a healthy
// run or wait longer than any run can take.
func verifyCIRunBound(path string, workflow workflowDocument, bound int) error {
	ends := make(map[string]int, len(workflow.Jobs))
	visiting := make(map[string]bool, len(workflow.Jobs))
	var end func(name string) (int, error)
	end = func(name string) (int, error) {
		if minutes, done := ends[name]; done {
			return minutes, nil
		}
		job, ok := workflow.Jobs[name]
		if !ok {
			return 0, fmt.Errorf("%s: a job needs %q, which the workflow does not define", path, name)
		}
		if visiting[name] {
			return 0, fmt.Errorf("%s: job %q needs itself through its dependencies", path, name)
		}
		visiting[name] = true
		start := 0
		for _, need := range job.Needs {
			needEnd, err := end(need)
			if err != nil {
				return 0, err
			}
			start = max(start, needEnd)
		}
		visiting[name] = false
		limit := job.TimeoutMinutes
		if limit == 0 {
			limit = githubJobTimeoutMinutes
		}
		ends[name] = start + limit
		return ends[name], nil
	}
	latest, last := 0, ""
	names := make([]string, 0, len(workflow.Jobs))
	for name := range workflow.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		minutes, err := end(name)
		if err != nil {
			return err
		}
		if minutes > latest {
			latest, last = minutes, name
		}
	}
	if latest != bound {
		return fmt.Errorf("%s: with every job at its limit the run ends at %d minutes, after %s, and the release preflight waits for a run that ends at %d",
			path, latest, last, bound)
	}
	return nil
}

// scalarMatrix is a matrix whose every entry is a scalar, such as the include
// expression the lifecycle reads, and nil for a matrix with any other entry.
func scalarMatrix(matrix map[string]yaml.Node) map[string]string {
	values := make(map[string]string, len(matrix))
	for key, node := range matrix {
		if node.Kind != yaml.ScalarNode {
			return nil
		}
		values[key] = node.Value
	}
	return values
}

func verifyUpdateWorkflow(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	return verifyUpdateWorkflowSemantics(path, workflow, contents)
}

func verifyUpdateWorkflowSemantics(path string, workflow workflowDocument, contents []byte) error {
	required := []string{
		"' M " + docsPath + "'",
		"'M  " + docsPath + "'",
		"$'M\\t" + docsPath + "'",
		docsPath + " > \"$patch_file\"",
		"git add \\\n            " + manifestPath + " \\\n            " + chartPath + " \\\n            " + docsPath + "\n",
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
	// A binding the workflow states more than once has to hold at every place
	// it appears: the proposal reads the pull request twice and the dispatch
	// lists runs twice, and weakening one copy would leave a presence check
	// satisfied by the other. The discover step reports a change exactly once.
	repeatedBindings := []struct {
		marker string
		count  int
	}{
		{".headRefOid == $sha", 2},
		{".head_sha == $sha", 2},
		{"-f head_sha=\"$EXPECTED_SHA\"", 2},
		{"'changed=true'", 1},
	}
	for _, binding := range repeatedBindings {
		if got := bytes.Count(contents, []byte(binding.marker)); got != binding.count {
			return fmt.Errorf("%s: %q must appear %d times, found %d", path, binding.marker, binding.count, got)
		}
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
	cancelInProgress := workflow.Concurrency.CancelInProgress
	if workflow.Concurrency.Group != "update-kubernetes-support" ||
		cancelInProgress.Kind != yaml.ScalarNode || cancelInProgress.Tag != "!!bool" || cancelInProgress.Value != "false" {
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
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		map[string]string{
			"ref":                 "${{ github.event.repository.default_branch }}",
			"fetch-depth":         "0",
			"persist-credentials": "false",
		}); err != nil {
		return err
	}
	if err := verifyUpdaterActionStep(path, "prepare", prepareSteps[1],
		"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
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
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
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
	return nil
}

func verifyReleaseWorkflow(path string) error {
	workflow, _, err := readWorkflow(path)
	if err != nil {
		return err
	}
	cancelInProgress := workflow.Concurrency.CancelInProgress
	if workflow.Concurrency.Group != "release-${{ github.ref }}" ||
		cancelInProgress.Kind != yaml.ScalarNode || cancelInProgress.Tag != "!!str" ||
		cancelInProgress.Value != "${{ github.event_name == 'pull_request' }}" {
		return fmt.Errorf("%s: release concurrency must cancel only superseded pull request validation and serialize each tag", path)
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
		"acceptance-evidence-sha256": "${{ steps.acceptance-evidence.outputs.sha256 }}",
		"chart-sha256":               "${{ steps.support-evidence.outputs.chart-sha256 }}",
		"kubernetes-support-window":  "${{ steps.support-evidence.outputs.kubernetes-support-window }}",
		"source-sha":                 "${{ steps.support-evidence.outputs.source-sha }}",
		"support-evidence-run-id":    "${{ steps.support-evidence.outputs.support-evidence-run-id }}",
	}) {
		return fmt.Errorf("%s: support preflight must expose its verified source SHA, CI run, support window, installed chart digest, and acceptance evidence digest", path)
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
		// The inventory is read as a subset and as a whole: each chart this
		// release needs exactly once, and a page that ended early refused
		// rather than taken for the rest. Requiring the run to hold nothing
		// else is what refused every successful matrix.
		"--paginate",
		"so it is a partial page",
		"select(.name == $name)",
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
	return nil
}

// The workflow that cancels a closed pull request's runs.
const (
	cancelWorkflowPath           = ".github/workflows/cancel-closed-pull-request.yml"
	cancelWorkflowTimeoutMinutes = 5
)

// wantCancelRun is the whole cancellation command. It lists every page of the
// unfinished runs of the closed pull request's head branch by pull_request
// event, keeps the ones from this repository other than its own, and cancels
// them. A run that concludes between the listing and the request is not a
// failure; one that is still running afterwards is.
const wantCancelRun = `set -euo pipefail
[[ -n "$HEAD_BRANCH" ]]
run_ids_file="$RUNNER_TEMP/closed-pull-request-run-ids"
: > "$run_ids_file"
# Every status a run can hold before it concludes. This run is itself
# an unfinished pull_request run on the branch, and is left out.
for status in requested queued pending waiting in_progress; do
  # --paginate follows the Link header to the last page and prints
  # each page as a JSON object of its own, which jq reads in turn.
  # Nothing reads total_count: on a branch with a handful of runs, a
  # status-filtered listing has returned one its page disagreed with.
  runs="$(gh api \
    --method GET \
    --paginate \
    -H 'X-GitHub-Api-Version: 2026-03-10' \
    "repos/$GITHUB_REPOSITORY/actions/runs" \
    -f branch="$HEAD_BRANCH" \
    -f event=pull_request \
    -f status="$status" \
    -f per_page=100)"
  jq -r \
    --arg branch "$HEAD_BRANCH" \
    --arg repository "$GITHUB_REPOSITORY" \
    --argjson self "$GITHUB_RUN_ID" '
      .workflow_runs[] |
      select(.event == "pull_request" and
             .head_branch == $branch and
             .head_repository.full_name == $repository and
             .id != $self) |
      .id
    ' <<<"$runs" >> "$run_ids_file"
done
while read -r run_id; do
  [[ "$run_id" =~ ^[1-9][0-9]*$ ]]
  if gh api \
    --method POST \
    -H 'X-GitHub-Api-Version: 2026-03-10' \
    "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/cancel" >/dev/null
  then
    echo "canceled run $run_id"
    continue
  fi
  # A run can conclude between the listing and the request, and
  # GitHub refuses to cancel a run that has concluded.
  status="$(gh api \
    --method GET \
    -H 'X-GitHub-Api-Version: 2026-03-10' \
    "repos/$GITHUB_REPOSITORY/actions/runs/$run_id" \
    --jq .status)"
  if [[ "$status" != completed ]]; then
    echo "run $run_id is $status and could not be canceled" >&2
    exit 1
  fi
done < <(sort -u "$run_ids_file")
`

// verifyCancelWorkflow holds the workflow that cancels a closed pull request's
// runs to that and nothing more. It holds actions: write, which can cancel any
// run in the repository, master's and a release tag's included, so what it
// cancels has to be decided by the audited command alone.
func verifyCancelWorkflow(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	return verifyCancelWorkflowSemantics(path, workflow, contents)
}

func verifyCancelWorkflowSemantics(path string, workflow workflowDocument, contents []byte) error {
	// The keys are an allow-list rather than a set of checks on the fields
	// this verifier happens to decode. A concurrency group is the plainest
	// case: groups are shared across workflows, so one named after CI's would
	// cancel CI's runs with no command at all.
	var shape struct {
		Top  map[string]yaml.Node `yaml:",inline"`
		Jobs map[string]struct {
			Keys  map[string]yaml.Node   `yaml:",inline"`
			Steps []map[string]yaml.Node `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &shape); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for key := range shape.Top {
		if key != "name" && key != "on" && key != "permissions" {
			return fmt.Errorf("%s: workflow key %q is outside the audited shape of the cancellation", path, key)
		}
	}
	if len(shape.Jobs) != 1 {
		return fmt.Errorf("%s: the cancellation must be exactly one job, the audited cancel", path)
	}
	for jobName, job := range shape.Jobs {
		for key := range job.Keys {
			switch key {
			case "name", "if", "runs-on", "timeout-minutes":
			default:
				return fmt.Errorf("%s: job %q key %q is outside the audited shape of the cancellation", path, jobName, key)
			}
		}
		for _, step := range job.Steps {
			for key := range step {
				switch key {
				case "name", "id", "env", "shell", "run":
				default:
					return fmt.Errorf("%s: job %q step key %q is outside the audited shape of the cancellation", path, jobName, key)
				}
			}
		}
	}

	if !triggersOnlyOnPullRequestClose(workflow.On) {
		return fmt.Errorf("%s: the cancellation must run only when a pull request closes: on pull_request, types [closed], and nothing else", path)
	}
	if !equalStringMap(workflow.Permissions, map[string]string{"actions": "write"}) {
		return fmt.Errorf("%s: the cancellation must hold actions: write and nothing else", path)
	}

	job, err := requireWorkflowJob(path, workflow, "cancel")
	if err != nil {
		return err
	}
	if job.Name != "Cancel the closed pull request's runs" || job.RunsOn != "ubuntu-latest" ||
		job.TimeoutMinutes != cancelWorkflowTimeoutMinutes {
		return fmt.Errorf("%s: the cancel job must be the audited ubuntu-latest job with a %d-minute timeout", path, cancelWorkflowTimeoutMinutes)
	}
	// A fork's pull request gets a read-only token whatever the workflow asks
	// for, so the job would only fail. It is skipped instead.
	if job.If != "github.event.pull_request.head.repo.full_name == github.repository" {
		return fmt.Errorf("%s: the cancel job must skip a pull request from a fork, whose token cannot cancel anything", path)
	}
	steps, err := requireWorkflowStepOrder(path, "cancel", job, []string{"cancel-runs"})
	if err != nil {
		return err
	}
	step := steps[0]
	if step.Name != "Cancel the head branch's unfinished pull request runs" || step.Shell != "bash" {
		return fmt.Errorf("%s: the cancellation must be the audited bash step", path)
	}
	if !equalStringMap(step.Env, map[string]string{
		"GH_TOKEN":    "${{ secrets.GITHUB_TOKEN }}",
		"HEAD_BRANCH": "${{ github.event.pull_request.head.ref }}",
	}) {
		return fmt.Errorf("%s: the cancellation must bind the closed pull request's head branch and the Actions token, and nothing else", path)
	}
	for _, rule := range []struct {
		marker  string
		problem string
	}{
		{`-f event=pull_request \`, "must list only pull_request runs"},
		{`select(.event == "pull_request" and`, "must cancel only pull_request runs"},
		{`-f branch="$HEAD_BRANCH" \`, "must list only the closed pull request's head branch"},
		{`.head_branch == $branch and`, "must cancel only runs on the closed pull request's head branch"},
		{`.head_repository.full_name == $repository and`, "must cancel only runs from this repository"},
		{`.id != $self) |`, "must leave its own run to finish"},
	} {
		if !strings.Contains(step.Run, rule.marker) {
			return fmt.Errorf("%s: the cancellation %s", path, rule.problem)
		}
	}
	if step.Run != wantCancelRun {
		return fmt.Errorf("%s: the cancellation command differs from the audited one", path)
	}
	return nil
}

func triggersOnlyOnPullRequestClose(on map[string]yaml.Node) bool {
	trigger, ok := on["pull_request"]
	if !ok || len(on) != 1 {
		return false
	}
	var pullRequest struct {
		Types []string             `yaml:"types"`
		Other map[string]yaml.Node `yaml:",inline"`
	}
	if err := trigger.Decode(&pullRequest); err != nil || len(pullRequest.Other) != 0 {
		return false
	}
	return len(pullRequest.Types) == 1 && pullRequest.Types[0] == "closed"
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
	Group            string    `yaml:"group"`
	CancelInProgress yaml.Node `yaml:"cancel-in-progress"`
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
	FailFast *bool                `yaml:"fail-fast"`
	Matrix   map[string]yaml.Node `yaml:"matrix"`
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

// verifyGoBuildCacheStep holds the rolling build cache to its shape.
//
// setup-go caches the module downloads and, on a key miss, whatever build
// output existed when that job ended; its key is the Go version and go.sum, so
// after the first run every job restores one frozen snapshot and saves nothing.
// This step is what makes the compilation a job does available to the next run,
// so the key has to end in the commit and fall back to the nearest earlier one.
//
// The scope is per job, because -race objects and plain ones are different
// caches: one shared key would have each run evict the other's entries.
// verifyHelmSetupStep holds a job's Helm installation to the pinned action and
// version. The unit suite renders the chart, so a job that runs it needs Helm;
// the runner image carried one until it stopped, and an unpinned install would
// let the rendering CI checks drift from the rendering a release does.
func verifyHelmSetupStep(path, jobName string, step workflowStep) error {
	if step.Name != "Set up Helm" || step.If != "" || step.Run != "" ||
		step.Shell != "" || step.WorkingDirectory != "" || len(step.Env) != 0 {
		return fmt.Errorf("%s: job %q Helm setup must be an unconditional action step", path, jobName)
	}
	if step.Uses != helmSetupAction {
		return fmt.Errorf("%s: job %q Helm setup must use the pinned %s", path, jobName, helmSetupAction)
	}
	if len(step.With) != 1 || step.With["version"] != helmVersion {
		return fmt.Errorf("%s: job %q Helm setup must pin version %s and nothing else", path, jobName, helmVersion)
	}
	return nil
}

func verifyGoBuildCacheStep(path, jobName string, step workflowStep, scope string) error {
	key := fmt.Sprintf(
		"go-build-${{ runner.os }}-%s-${{ hashFiles('go.sum') }}-${{ github.sha }}",
		scope,
	)
	restore := fmt.Sprintf(
		"go-build-${{ runner.os }}-%[1]s-${{ hashFiles('go.sum') }}-\ngo-build-${{ runner.os }}-%[1]s-\n",
		scope,
	)
	if step.Name != "Cache the Go build output" {
		return fmt.Errorf("%s: job %q build cache step has unexpected name %q", path, jobName, step.Name)
	}
	return verifyUpdaterActionStep(
		path,
		jobName,
		step,
		"actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
		map[string]string{
			"path":         "~/.cache/go-build",
			"key":          key,
			"restore-keys": restore,
		},
	)
}

// envtestCacheInputs are the envtest store cache's inputs: the store the
// Makefile's ENVTEST_BIN_DIR names, keyed by the Makefile that pins it.
func envtestCacheInputs() map[string]string {
	return map[string]string{
		"path":         "${{ runner.temp }}/envtest",
		"key":          "envtest-${{ runner.os }}-${{ hashFiles('Makefile') }}",
		"restore-keys": "envtest-${{ runner.os }}-\n",
	}
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
	makefile                 string
	harness                  string
	supportImageResolver     string
	kindConfig               string
	kindIsolationWorker      string
	apiServerEndpointFilter  string
	staticChecks             string
	admissionSchemaContract  string
	admissionSchemaSelftest  string
	controllerSchemaContract string
	controllerSchemaSelftest string
}

// goPhaseRunnerContract is how the driver runs a Go phase: the binary the
// bootstrap built from the snapshot, in the package directory, asked for the
// phase by name, and passed only when it wrote the phase's name to a record
// the runner cleared first. The binary owns the rest -- which test is the
// phase, its bound, and the refusal of a run that reached no phase.
const goPhaseRunnerContract = `run_go_phase() {
	go_phase_record=$WORK_DIR/go-phase-$1.completed
	rm -f -- "$go_phase_record"
	(cd "$ROOT_DIR/test/e2e" &&
		"$GO_PHASE_BINARY" -test.v -e2e.phase="$1" -e2e.completed="$go_phase_record") || return 1
	[ "$(cat -- "$go_phase_record" 2>/dev/null)" = "$1" ] || {
		printf 'e2e: the Go phase %s exited 0 and recorded no completion, so nothing it ran counts\n' "$1" >&2
		return 1
	}
}`

// goPhaseBinaryAssignment is the one place the driver names the binary the Go
// phases run from. The build writes it and the runner runs it; a second
// assignment, or a command that writes something else there, would run
// another program under every Go phase's name.
const goPhaseBinaryAssignment = `GO_PHASE_BINARY=$WORK_DIR/ptah-e2e.test`

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

const controlPlaneComponentShapeContract = `wait_for_control_plane_component_shape() {
	expected_api_server_feature_gates=$1
	control_plane_pods_file=$WORK_DIR/control-plane-pods.json
	control_plane_shape_deadline=$(($(date +%s) + CONTROL_PLANE_SHAPE_DEADLINE_SECONDS))
	control_plane_shape_counts="no kube-system snapshot was read"
	while :; do
		if kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
			-n kube-system get pods -o json >"$control_plane_pods_file" &&
			control_plane_shape_reading=$(jq -r --arg expected "$expected_api_server_feature_gates" --arg cluster "$CLUSTER_NAME" '
      def component_pods($component):
        [.items[] | select(.metadata.labels.component == $component)];
      def command_options($pod; $prefix):
        [$pod.spec.containers[] | (.command // [])[] | select(startswith($prefix))];
      def control_plane_nodes:
        [$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3"] | sort;
      def expected_options($component):
        if $component == "kube-apiserver" and $expected != "" then ["--feature-gates=" + $expected] else [] end;
      def spoken($options):
        if ($options | length) == 0 then "no feature gates" else ($options | join(" ")) end;
      def refusal($pod; $component):
        if (control_plane_nodes | index($pod.spec.nodeName)) == null then
          "\($component) pod \($pod.metadata.name) runs on \($pod.spec.nodeName // "no node"), which is not a control plane"
        elif (($pod.metadata.annotations["kubernetes.io/config.mirror"] // "") | length) == 0 then
          "\($component) pod \($pod.metadata.name) is not a static-pod mirror"
        elif $pod.metadata.name != ($component + "-" + $pod.spec.nodeName) then
          "\($component) pod \($pod.metadata.name) is not the static pod of \($pod.spec.nodeName)"
        elif (($pod.spec.containers // []) | length) != 1 or ($pod.spec.containers[0].name != $component) then
          "\($component) pod \($pod.metadata.name) does not run exactly one \($component) container"
        elif command_options($pod; "--feature-gates=") != expected_options($component) then
          "\($component) pod \($pod.metadata.name) carries \(spoken(command_options($pod; "--feature-gates="))), expected \(spoken(expected_options($component)))"
        elif $component == "kube-apiserver" and (command_options($pod; "--runtime-config=") | length) != 1 then
          "kube-apiserver pod \($pod.metadata.name) carries \(command_options($pod; "--runtime-config=") | length) --runtime-config options, and kind sets exactly one"
        else null end;
      def settled($pod; $component):
        ($pod.metadata.deletionTimestamp == null) and
        ($pod.status.phase == "Running") and
        ([($pod.status.conditions // [])[] | select(.type == "Ready" and .status == "True")] | length) == 1 and
        (($pod.status.containerStatuses // []) | length) == 1 and
        ($pod.status.containerStatuses[0].name == $component) and
        ($pod.status.containerStatuses[0].ready == true) and
        (($pod.status.containerStatuses[0].state.running | type) == "object");

      . as $snapshot |
      ["kube-apiserver", "kube-controller-manager", "kube-scheduler"] |
      map(. as $component |
        ($snapshot | component_pods($component)) as $pods |
        {
          component: $component,
          seen: ($pods | length),
          ready: ([$pods[] | select(refusal(.; $component) == null and settled(.; $component))] | length),
          nodes: ([$pods[] | select(refusal(.; $component) == null and settled(.; $component)) | .spec.nodeName] | sort),
          refusal: ([$pods[] | refusal(.; $component) | select(. != null)] | first)
        }
      ) as $components |
      ([$components[].refusal | select(. != null)] | first) as $wrong |
      ([$components[] | "\(.component) \(.seen) seen \(.ready) ready"] | join(", ")) as $counts |
      if $wrong != null then "wrong " + $wrong
      elif all($components[]; .seen == 3 and .ready == 3 and .nodes == control_plane_nodes) then "ready " + $counts
      else "incomplete " + $counts
      end
	' "$control_plane_pods_file"); then
			case "$control_plane_shape_reading" in
				"ready "*) return 0 ;;
				"wrong "*)
					fail "the control plane is not the one this cluster was created with: ${control_plane_shape_reading#wrong }"
				;;
				"incomplete "*)
					control_plane_shape_counts=${control_plane_shape_reading#incomplete }
				;;
			esac
		fi
		[ "$(date +%s)" -lt "$control_plane_shape_deadline" ] || break
		sleep 2
	done
	fail "the control plane did not reach three ready kube-apiserver, kube-controller-manager and kube-scheduler pods in ${CONTROL_PLANE_SHAPE_DEADLINE_SECONDS}s; the last snapshot held $control_plane_shape_counts"
}
`

const apiServerFeatureGateScopeContract = `assert_api_server_feature_gate_scope() {
	expected_api_server_feature_gates=$1
	component_configs_file=$WORK_DIR/component-configs.json
	wait_for_control_plane_component_shape "$expected_api_server_feature_gates"
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
}
`

const kindHATopologyContract = `assert_kind_ha_topology() {
	kind get nodes --name "$CLUSTER_NAME" | LC_ALL=C sort >"$KIND_NODE_INVENTORY_FILE"
	# kind counts the load balancer among a cluster's nodes and Kubernetes
	# does not, which is why this list carries one more name than the node
	# inventory asserted below. Measured on kind v0.31: "kind get nodes" on
	# a two-control-plane cluster returns the balancer as a third line.
	#
	# The isolation worker is in both inventories exactly when this run
	# declared it. A cluster without it cannot run the row that isolates it,
	# and one that has it where nothing asked is not the cluster the other
	# suites are measured on.
	if ! {
		printf '%s\n' \
			"${CLUSTER_NAME}-control-plane" \
			"${CLUSTER_NAME}-control-plane2" \
			"${CLUSTER_NAME}-control-plane3" \
			"${CLUSTER_NAME}-worker" \
			"${CLUSTER_NAME}-external-load-balancer"
		if [ "$ISOLATION_WORKER" = true ]; then
			printf '%s\n' "${CLUSTER_NAME}-worker2"
		fi
	} | LC_ALL=C sort | cmp -s - "$KIND_NODE_INVENTORY_FILE"; then
		fail "kind cluster does not have the exact three-control-plane, one-worker, one-load-balancer topology $KIND_ISOLATION_TOPOLOGY"
	fi
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		get nodes -o json >"$NODE_READINESS_FILE"
	# Only the isolation worker carries the isolation key, as a label and as
	# the one taint that keeps everything else off it; every other node
	# carries neither, or a Pod that selects the label could land beside the
	# manager and a Pod that tolerates the taint could land anywhere.
	jq -e --arg cluster "$CLUSTER_NAME" --argjson isolation "$ISOLATION_WORKER" \
		--arg key "$ISOLATION_NODE_KEY" '
      (if $isolation then [$cluster + "-worker2"] else [] end) as $isolated |
      ([.items[].metadata.name] | sort) == ([$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3", $cluster + "-worker"] + $isolated | sort) and
      ([.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)] | length) == 3 and
      ([.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null)] | length) == 1 + ($isolated | length) and
      all(.items[];
        any((.status.conditions // [])[];
          .type == "Ready" and .status == "True"
        )
      ) and
      all(.items[];
        .metadata.name as $name |
        (.metadata.labels // {})[$key] as $label |
        [(.spec.taints // [])[] | select(.key == $key)] as $taints |
        if any($isolated[]; . == $name) then
          $label == "true" and $taints == [{key: $key, value: "true", effect: "NoSchedule"}]
        else
          $label == null and $taints == []
        end
      )
    ' "$NODE_READINESS_FILE" >/dev/null ||
		fail "Kubernetes node inventory does not match the ready HA kind topology $KIND_ISOLATION_TOPOLOGY"
}`

const kubeletLogBudgetContract = `assert_kubelet_log_budget() {
	kubelet_budget_expected=4
	if [ "$ISOLATION_WORKER" = true ]; then kubelet_budget_expected=5; fi
	jq -er '.items[].metadata.name' "$NODE_READINESS_FILE" >"$WORK_DIR/kubelet-log-nodes.txt" ||
		fail "could not enumerate Kubernetes nodes for the kubelet log budget"
	kubelet_budget_count=0
	while IFS= read -r kubelet_budget_node; do
		[ -n "$kubelet_budget_node" ] || continue
		kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s get \
			--raw "/api/v1/nodes/$kubelet_budget_node/proxy/configz" \
			>"$WORK_DIR/kubelet-log-config-$kubelet_budget_node.json" ||
			fail "could not read the effective kubelet log budget on $kubelet_budget_node"
		jq -e '.kubeletconfig.containerLogMaxSize == "10Mi"' \
			"$WORK_DIR/kubelet-log-config-$kubelet_budget_node.json" >/dev/null ||
			fail "kubelet $kubelet_budget_node must use the standard 10Mi container log size for durable-result acceptance"
		kubelet_budget_count=$((kubelet_budget_count + 1))
	done <"$WORK_DIR/kubelet-log-nodes.txt"
	[ "$kubelet_budget_count" -eq "$kubelet_budget_expected" ] ||
		fail "kubelet log budget was not verified on every declared node"
	printf 'e2e: verified default 10Mi container log files on %s kubelets\n' "$kubelet_budget_count"
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
		"${CLUSTER_NAME}-worker" \
		"${CLUSTER_NAME}-worker2"; do
		# The isolation worker pulls the executor for the Pods placed on it,
		# and exists only where this run declared it.
		if [ "$kind_node_container" = "${CLUSTER_NAME}-worker2" ] && [ "$ISOLATION_WORKER" != true ]; then
			continue
		fi
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

type kubernetesMinorCaseBlock struct {
	source string
	minors map[string]struct{}
}

func parseKubernetesMinorCaseBlocks(contents []byte) ([]kubernetesMinorCaseBlock, error) {
	logicalShell := normalizeShellContinuations(maskShellHeredocBodies(contents))
	caseStart := regexp.MustCompile(`^[ \t]*case[ \t]+.+[ \t]+in[ \t]*(?:#[^\r\n]*)?\r?$`)
	caseEnd := regexp.MustCompile(`^[ \t]*esac[ \t]*(?:#[^\r\n]*)?\r?$`)
	minorLabel := regexp.MustCompile(`(?m)^[ \t]*([0-9]+\.[0-9]+(?:[ \t]*\|[ \t]*[0-9]+\.[0-9]+)*)[ \t]*\)`)
	minorLiteral := regexp.MustCompile(`[0-9]+\.[0-9]+`)

	type caseFrame struct {
		lines []string
	}
	frames := make([]caseFrame, 0, 2)
	blocks := make([]kubernetesMinorCaseBlock, 0, 2)
	for _, line := range strings.Split(string(logicalShell), "\n") {
		lineBytes := []byte(line)
		startsCase := firstUnquotedShellMatch(lineBytes, caseStart) != nil
		endsCase := firstUnquotedShellMatch(lineBytes, caseEnd) != nil
		for index := range frames {
			frames[index].lines = append(frames[index].lines, line)
		}
		if startsCase {
			frames = append(frames, caseFrame{lines: []string{line}})
		}
		if !endsCase {
			continue
		}
		if len(frames) == 0 {
			return nil, errors.New("shell source has an unmatched esac")
		}
		frame := frames[len(frames)-1]
		frames = frames[:len(frames)-1]
		source := strings.Join(frame.lines, "\n")
		minors := make(map[string]struct{})
		for _, label := range minorLabel.FindAllStringSubmatch(source, -1) {
			for _, minor := range minorLiteral.FindAllString(label[1], -1) {
				minors[minor] = struct{}{}
			}
		}
		if len(minors) >= 3 {
			blocks = append(blocks, kubernetesMinorCaseBlock{source: source, minors: minors})
		}
	}
	if len(frames) != 0 {
		return nil, errors.New("shell source has an unterminated case statement")
	}
	return blocks, nil
}

func containsKubernetesWindowGrep(contents []byte) bool {
	logicalShell := normalizeShellContinuations(maskShellHeredocBodies(contents))
	grepExtendedRegexp := regexp.MustCompile(
		`(?:^|[|;&()][ \t]*)(?:command[ \t]+)?(?:[^ \t;&|()]+/)?grep[ \t]+(?:-[A-Za-z]*E[A-Za-z]*|--extended-regexp)(?:[ \t]|$)`,
	)
	minorLiteral := regexp.MustCompile(`([0-9]+)(?:\.|\\\.)([0-9]+)`)
	for _, line := range strings.Split(string(logicalShell), "\n") {
		lineBytes := []byte(line)
		if firstUnquotedShellMatch(lineBytes, grepExtendedRegexp) == nil {
			continue
		}
		minors := make(map[string]struct{})
		for _, match := range minorLiteral.FindAllStringSubmatch(line, -1) {
			minors[match[1]+"."+match[2]] = struct{}{}
		}
		if len(minors) >= 3 {
			return true
		}
	}
	return false
}

func verifyAuditedKubernetesMinorSelection(path string, contents []byte, allowedCaseSources ...string) error {
	actual, err := parseKubernetesMinorCaseBlocks(contents)
	if err != nil {
		return fmt.Errorf("%s: parse Kubernetes minor case statements: %w", path, err)
	}
	expected := make([]kubernetesMinorCaseBlock, 0, len(allowedCaseSources))
	for _, source := range allowedCaseSources {
		blocks, parseErr := parseKubernetesMinorCaseBlocks([]byte(source))
		if parseErr != nil || len(blocks) != 1 {
			return fmt.Errorf("internal Kubernetes minor case contract is invalid")
		}
		expected = append(expected, blocks[0])
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("%s: found %d Kubernetes minor case blocks, want exactly %d audited version-specific behavior blocks", path, len(actual), len(expected))
	}
	matched := make([]bool, len(expected))
	for _, block := range actual {
		found := false
		for index, contract := range expected {
			if matched[index] || !equalStrings(
				normalizedNonemptyLines(block.source),
				normalizedNonemptyLines(contract.source),
			) {
				continue
			}
			matched[index] = true
			found = true
			break
		}
		if !found {
			return fmt.Errorf("%s: Kubernetes minor case block differs from the audited version-specific behavior contract", path)
		}
	}
	if containsKubernetesWindowGrep(contents) {
		return fmt.Errorf("%s: extended-regexp grep must not encode a private Kubernetes support window", path)
	}
	return nil
}

func verifyKubernetesSupportWindowWiring(files e2eWiringFiles) error {
	resolverContents, err := os.ReadFile(files.supportImageResolver)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.supportImageResolver, err)
	}
	if err := verifyShellScriptEntrypoint(files.supportImageResolver, resolverContents); err != nil {
		return err
	}
	resolverContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("exact resolver argument count", `[ "$#" -eq 2 ] || fail "usage: $0 SUPPORT_MANIFEST KUBERNETES_VERSION"`),
		exactSourceLine("support manifest argument", `support_manifest=$1`),
		exactSourceLine("exact Kubernetes version argument", `kubernetes_version=$2`),
		exactSourceLine("exact Kubernetes version syntax", `printf '%s\n' "$kubernetes_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||`),
		exactSourceLine("manifest lookup minor", `kubernetes_minor=${kubernetes_version%.*}`),
		exactSourceLine("manifest lookup", `resolved_image=$(jq -er \`),
		exactSourceLine("manifest release iteration", `.releases[]`),
		exactSourceLine("manifest minor membership", `| select((.minor | type) == "string" and .minor == $minor)`),
		exactSourceLine("exact node version binding", `| select(startswith("kindest/node:v" + $version + "@sha256:"))`),
		exactSourceLine("unique membership", `| if length == 1`),
		exactSourceLine("resolved node image output", `printf '%s\n' "$resolved_image"`),
	}
	if err := verifyOrderedSourceContract(files.supportImageResolver, resolverContents, resolverContract); err != nil {
		return err
	}
	if err := verifyAuditedKubernetesMinorSelection(files.supportImageResolver, resolverContents); err != nil {
		return err
	}

	harnessContents, err := os.ReadFile(files.harness)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.harness, err)
	}
	if err := verifyAuditedKubernetesMinorSelection(files.harness, harnessContents, apiServerFeatureGatePatchContract); err != nil {
		return fmt.Errorf("API-server feature gate contract: %w", err)
	}

	return verifyOrderedSourceContract(files.harness, harnessContents, []sourceContractStep{
		exactSourceLine("Kubernetes minor behavior selector", `K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)`),
		exactSourceLineSequence("manifest-backed Kubernetes support membership", []string{
			`SUPPORTED_KIND_NODE_IMAGE=$("$ROOT_DIR/hack/e2e-kubernetes-support-image.sh" \`,
			`"$ROOT_DIR/support/kubernetes.json" "$K8S_VERSION") ||`,
			`fail "Kubernetes $K8S_VERSION is not an exact member of support/kubernetes.json"`,
		}),
		exactSourceLineSequence("support-manifest image selection", []string{
			`if [ -z "$KIND_NODE_IMAGE" ]; then`,
			`KIND_NODE_IMAGE=$SUPPORTED_KIND_NODE_IMAGE`,
			`fi`,
		}),
		exactSourceLineSequence("support-manifest image equality", []string{
			`[ "$KIND_NODE_IMAGE" = "$SUPPORTED_KIND_NODE_IMAGE" ] ||`,
			`fail "KIND_NODE_IMAGE must match the digest-pinned support manifest entry for Kubernetes $K8S_VERSION"`,
		}),
	})
}

func verifyE2ESourceSnapshot(path string, contents []byte) error {
	snapshotContract := []sourceContractStep{
		exactSourceLine("bootstrap checkout root", `BOOTSTRAP_ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)`),
		exactSourceLine("outer snapshot branch", `if [ "${1:-}" != --source-snapshot ]; then`),
		exactSourceLineSequence("exact HEAD capture", []string{
			`SNAPSHOT_REVISION=$(git -C "$BOOTSTRAP_ROOT_DIR" rev-parse --verify 'HEAD^{commit}') ||`,
			`snapshot_fail "could not resolve the operator source HEAD"`,
		}),
		exactSourceLineSequence("clean checkout preflight", []string{
			`E2E_SOURCE_STATUS=$(git -C "$BOOTSTRAP_ROOT_DIR" status --porcelain=v1 --untracked-files=all) ||`,
			`snapshot_fail "could not inspect the operator source tree before snapshot creation"`,
			`[ -z "$E2E_SOURCE_STATUS" ] ||`,
			`snapshot_fail "operator source tree must exactly match HEAD before snapshot creation"`,
		}),
		exactSourceLine("isolated snapshot directory", `SOURCE_SNAPSHOT_WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-e2e-source.XXXXXX")`),
		exactSourceLineSequence("snapshot cleanup status capture", []string{
			`snapshot_cleanup() {`,
			`status=$?`,
		}),
		exactSourceLine("snapshot cleanup path bound", `"${TMPDIR:-/tmp}"/ptah-operator-e2e-source.*)`),
		exactSourceLine("snapshot cleanup removal", `rm -rf -- "$SOURCE_SNAPSHOT_WORK_DIR"`),
		exactSourceLineSequence("snapshot failure-preserving trap", []string{
			`exit "$status"`,
			`}`,
			`trap snapshot_cleanup EXIT`,
		}),
		exactSourceLineSequence("exact source archive", []string{
			`git -C "$BOOTSTRAP_ROOT_DIR" archive --format=tar \`,
			`--output="$SOURCE_SNAPSHOT_ARCHIVE" "$SNAPSHOT_REVISION"`,
			`tar -xf "$SOURCE_SNAPSHOT_ARCHIVE" -C "$SOURCE_SNAPSHOT_ROOT"`,
		}),
		exactSourceLineSequence("snapshot execution identity", []string{
			`E2E_SOURCE_REPOSITORY_ROOT=$BOOTSTRAP_ROOT_DIR`,
			`E2E_CONTROLLER_REVISION=$SNAPSHOT_REVISION`,
		}),
		exactSourceLine("exact snapshot child execution", `"$SOURCE_SNAPSHOT_ROOT/hack/e2e-kind.sh" --source-snapshot "$@"`),
		exactSourceLine("snapshot root activation", `ROOT_DIR=$BOOTSTRAP_ROOT_DIR`),
		exactSourceLine("immutable object repository activation", `SOURCE_REPOSITORY_ROOT=${E2E_SOURCE_REPOSITORY_ROOT:?E2E_SOURCE_REPOSITORY_ROOT is required inside the source snapshot}`),
		exactSourceLine("controller revision activation", `CONTROLLER_REVISION=${E2E_CONTROLLER_REVISION:?E2E_CONTROLLER_REVISION is required inside the source snapshot}`),
		exactSourceLine("snapshot verification implementation", `verify_snapshot_source() (`),
		exactSourceLineSequence("snapshot comparison with exact commit", []string{
			`git -C "$SOURCE_REPOSITORY_ROOT" archive --format=tar \`,
			`--output="$verification_dir/source.tar" "$CONTROLLER_REVISION"`,
			`tar -xf "$verification_dir/source.tar" -C "$verification_dir/source"`,
			`git -c core.filemode=true diff --no-index --quiet --no-ext-diff --no-textconv -- \`,
			`"$verification_dir/source" "$ROOT_DIR" ||`,
			`fail "E2E source snapshot differs from the exact operator commit"`,
		}),
		exactSourceLineSequence("isolated snapshot validation", []string{
			`[ "$ROOT_DIR" != "$SOURCE_REPOSITORY_ROOT" ] ||`,
			`fail "E2E source snapshot must be isolated from the operator checkout"`,
			`[ ! -e "$ROOT_DIR/.git" ] ||`,
			`fail "E2E source snapshot must not contain Git worktree metadata"`,
		}),
		exactSourceLineSequence("exact controller object validation", []string{
			`printf '%s\n' "$CONTROLLER_REVISION" | grep -Eq '^[0-9a-f]{40}$' ||`,
			`fail "operator source revision must be an exact 40-character lowercase Git commit"`,
			`resolved_controller=$(git -C "$SOURCE_REPOSITORY_ROOT" rev-parse --verify "${CONTROLLER_REVISION}^{commit}") ||`,
			`fail "exact operator source commit $CONTROLLER_REVISION is unavailable"`,
			`[ "$resolved_controller" = "$CONTROLLER_REVISION" ] ||`,
			`fail "operator source revision resolved to $resolved_controller, expected $CONTROLLER_REVISION"`,
		}),
		exactSourceLine("snapshot content verification", `verify_snapshot_source`),
	}
	if err := verifyOrderedSourceContract(path, contents, snapshotContract); err != nil {
		return err
	}

	innerStart := exactSourceLine("snapshot root activation", `ROOT_DIR=$BOOTSTRAP_ROOT_DIR`).pattern.FindIndex(contents)
	if innerStart == nil {
		return fmt.Errorf("%s: snapshot root activation is missing", path)
	}
	innerContents := contents[innerStart[0]:]
	if regexp.MustCompile(`\$(?:\{SOURCE_REPOSITORY_ROOT\}|SOURCE_REPOSITORY_ROOT)/`).Match(innerContents) {
		return fmt.Errorf("%s: live checkout path escapes the exact source snapshot", path)
	}
	immutableObjectReads := []struct {
		line  string
		count int
	}{
		{line: `resolved_controller=$(git -C "$SOURCE_REPOSITORY_ROOT" rev-parse --verify "${CONTROLLER_REVISION}^{commit}") ||`, count: 1},
		{line: `git -C "$SOURCE_REPOSITORY_ROOT" archive --format=tar \`, count: 2},
		{line: `chart_source_epoch=$(git -C "$SOURCE_REPOSITORY_ROOT" show -s --format=%ct "$CONTROLLER_REVISION")`, count: 1},
	}
	for _, read := range immutableObjectReads {
		if count := bytes.Count(innerContents, []byte(read.line)); count != read.count {
			return fmt.Errorf("%s: immutable Git object read %q occurs %d times, want %d", path, read.line, count, read.count)
		}
	}
	if count := bytes.Count(innerContents, []byte(`$SOURCE_REPOSITORY_ROOT`)); count != 6 {
		return fmt.Errorf("%s: original checkout must have only two isolation checks and four audited immutable Git object reads, found %d references", path, count)
	}

	snapshotPaths := []struct {
		marker string
		count  int
	}{
		{marker: `go -C "$ROOT_DIR" run ./test/e2e/handcraftoci verify-certificate \`, count: 1},
		{marker: `chart_version=$(sed -n 's/^version: //p' "$ROOT_DIR/charts/ptah-operator/Chart.yaml")`, count: 1},
		{marker: `go -C "$ROOT_DIR" run ./hack/chartpackage \`, count: 1},
		{marker: `"$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" >"$KIND_CONFIG"`, count: 1},
		{marker: `cp "$ROOT_DIR/Dockerfile.executor" "$PTAH_BUILD_CONTEXT/Dockerfile.e2e"`, count: 1},
		{marker: `--file "$ROOT_DIR/test/e2e/Dockerfile.operator" \`, count: 2},
		{marker: `--tag "$OPERATOR_IMAGE" "$ROOT_DIR"`, count: 1},
		{marker: `--tag "$FIXTURE_BUILD_IMAGE" "$ROOT_DIR"`, count: 1},
		{marker: `jq -e -f "$ROOT_DIR/hack/admission-schema-contract.jq" \`, count: 1},
		{marker: `-f "$ROOT_DIR/hack/controller-object-schema-contract.jq" \`, count: 1},
		// The Go phases run from a binary built out of the snapshot, so the
		// build has to read the snapshot too. The runner that starts it is
		// pinned whole below, as goPhaseRunnerContract.
		{marker: `go -C "$ROOT_DIR" test -tags e2e -c -o "$GO_PHASE_BINARY" ./test/e2e ||`, count: 1},
	}
	for _, pathContract := range snapshotPaths {
		if count := bytes.Count(innerContents, []byte(pathContract.marker)); count != pathContract.count {
			return fmt.Errorf("%s: exact snapshot path %q occurs %d times, want %d", path, pathContract.marker, count, pathContract.count)
		}
	}

	firstDockerAccess := bytes.Index(contents, []byte(`docker --context "$DOCKER_CONTEXT"`))
	if firstDockerAccess < 0 {
		return fmt.Errorf("%s: Docker access is missing", path)
	}
	verificationCall := exactSourceLine("snapshot content verification", `verify_snapshot_source`).pattern.FindIndex(contents)
	if firstDockerAccess < verificationCall[1] {
		return fmt.Errorf("%s: exact source snapshot must be active before first Docker access", path)
	}
	return nil
}

func verifyE2EWiring(files e2eWiringFiles) error {
	if err := verifyKubernetesSupportWindowWiring(files); err != nil {
		return err
	}
	if err := verifyMakeE2ETarget(files.makefile); err != nil {
		return err
	}
	if err := verifyMakeRaceTargets(files.makefile); err != nil {
		return err
	}
	if err := verifyStaticChecksWiring(files); err != nil {
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
	if err := verifyKindHAConfig(files.kindConfig, files.kindIsolationWorker); err != nil {
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
	if err := verifyFailurePreservingExitTrap(harness, harnessContents, "snapshot_cleanup", "snapshot_verification_cleanup", "cleanup"); err != nil {
		return err
	}
	harnessContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("isolation worker key", isolationNodeKeyDeclaration),
		exactSourceLineSequence("isolation worker declared by the suite catalog", []string{
			`suite_isolation_worker() {`,
			`if [ "$E2E_STOP_AFTER" = bootstrap ]; then`,
			`printf '%s\n' false`,
			`elif [ "$E2E_SUITE" = all ]; then`,
			`jq -r 'any(.suites[]; .isolationWorker == true)' "$SUITE_CATALOG"`,
			`else`,
			`jq -r --arg suite "$E2E_SUITE" \`,
			`'any(.suites[]; .name == $suite and .isolationWorker == true)' "$SUITE_CATALOG"`,
			`fi`,
			`}`,
			`ISOLATION_WORKER=$(suite_isolation_worker) ||`,
			`fail "the acceptance suite catalog could not say whether $E2E_SUITE needs the isolation worker"`,
			`case "$ISOLATION_WORKER" in`,
			`true)`,
			`KIND_NODE_COUNT=5`,
			`KIND_ISOLATION_TOPOLOGY='and the isolation worker'`,
			`;;`,
			`false)`,
			`KIND_NODE_COUNT=4`,
			`KIND_ISOLATION_TOPOLOGY='and no isolation worker'`,
			`;;`,
			`*) fail "the acceptance suite catalog answered $ISOLATION_WORKER for whether $E2E_SUITE needs the isolation worker" ;;`,
			`esac`,
		}),
		exactSourceLine("required Kubernetes version", `[ -n "$K8S_VERSION" ] || fail "K8S_VERSION is required (for example, 1.37.0)"`),
		exactSourceLine("exact Kubernetes version syntax", `printf '%s\n' "$K8S_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||`),
		exactSourceLineSequence("manifest-backed Kubernetes support membership", []string{
			`K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)`,
			`SUPPORTED_KIND_NODE_IMAGE=$("$ROOT_DIR/hack/e2e-kubernetes-support-image.sh" \`,
			`"$ROOT_DIR/support/kubernetes.json" "$K8S_VERSION") ||`,
			`fail "Kubernetes $K8S_VERSION is not an exact member of support/kubernetes.json"`,
		}),
		exactSourceLineSequence("support-manifest image selection", []string{
			`if [ -z "$KIND_NODE_IMAGE" ]; then`,
			`KIND_NODE_IMAGE=$SUPPORTED_KIND_NODE_IMAGE`,
			`fi`,
		}),
		exactSourceLineSequence("support-manifest image equality", []string{
			`[ "$KIND_NODE_IMAGE" = "$SUPPORTED_KIND_NODE_IMAGE" ] ||`,
			`fail "KIND_NODE_IMAGE must match the digest-pinned support manifest entry for Kubernetes $K8S_VERSION"`,
		}),
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
			`.["operator.ptah.run/e2e-owner"] == $owner and`,
			`.["operator.ptah.run/e2e-component"] == "task-claim" and`,
			`.["operator.ptah.run/e2e-claim-token"] == $token`,
		}),
		exactSourceLine("task claim acquisition implementation", `acquire_task_claim() {`),
		exactSourceLineSequence("task claim cleanup armed before create", []string{
			`[ "$TASK_CLAIM_CREATE_STARTED" -eq 0 ] || fail "task identity claim acquisition was attempted more than once"`,
			`TASK_CLAIM_CREATE_STARTED=1`,
		}),
		exactSourceLineSequence("atomic daemon-side task claim creation", []string{
			`if ! created_claim=$(docker --context "$DOCKER_CONTEXT" volume create \`,
			`--label "operator.ptah.run/e2e-owner=${CLUSTER_NAME}" \`,
			`--label 'operator.ptah.run/e2e-component=task-claim' \`,
			`--label "operator.ptah.run/e2e-claim-token=${TASK_CLAIM_TOKEN}" \`,
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
			`.[0].Config.Labels["operator.ptah.run/e2e-owner"] == $owner and`,
			`.[0].Config.Labels["operator.ptah.run/e2e-component"] == "image-audit" and`,
			`.[0].Config.Labels["operator.ptah.run/e2e-claim-token"] == $token`,
		}),
		exactSourceLine("image-audit creation implementation", `create_image_audit_container() {`),
		exactSourceLine("image-audit cleanup armed before create", `IMAGE_AUDIT_CONTAINER_CREATED=1`),
		exactSourceLineSequence("image-audit labeled creation", []string{
			`if ! image_audit_id=$(docker --context "$DOCKER_CONTEXT" create \`,
			`--name "$IMAGE_AUDIT_CONTAINER" \`,
			`--label "operator.ptah.run/e2e-owner=${CLUSTER_NAME}" \`,
			`--label 'operator.ptah.run/e2e-component=image-audit' \`,
			`--label "operator.ptah.run/e2e-claim-token=${TASK_CLAIM_TOKEN}" \`,
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
			`if ! jq -e --argjson count "$KIND_NODE_COUNT" '.items | length == $count' "$NODE_READINESS_FILE" >/dev/null; then`,
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
			`jq -e --argjson count "$KIND_NODE_COUNT" '`,
			`((.items | length) == $count) and`,
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
		exactSourceLineSequence("kind template rendered", []string{
			`sed "s/__API_SERVER_PORT__/${E2E_API_SERVER_PORT}/g" \`,
			`"$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" >"$KIND_CONFIG"`,
		}),
		// Appended while the node list is still the last thing in the file, and
		// only where the suite declared it: the topology proof after creation
		// refuses a cluster that has it anywhere else.
		exactSourceLineSequence("isolation worker appended to the node list", []string{
			`if [ "$ISOLATION_WORKER" = true ]; then`,
			`cat "$ROOT_DIR/testdata/e2e/kind-isolation-worker.yaml.tmpl" >>"$KIND_CONFIG"`,
			`fi`,
			`EXPECTED_API_SERVER_FEATURE_GATES=`,
		}),
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
			`assert_kubelet_log_budget`,
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
		// The apply-policy guard judges the harness identity like anyone else,
		// so the release values exempt the groups it carries, and those come
		// from the API server's own answer about the identity, not from a name
		// the harness assumed.
		exactSourceLineSequence("apply-policy guard exempt groups read from the harness identity", []string{
			`APPLY_POLICY_EXEMPT_GROUPS=$(kubectl --kubeconfig "$KUBECONFIG_FILE" auth whoami -o json |`,
			`jq -ce '[.status.userInfo.groups[] | select(. != "system:authenticated")] | select(length > 0)') ||`,
			`fail "the harness identity carries no group the apply-policy guard could exempt"`,
		}),
		exactSourceLineSequence("digest-pinned current-release Helm values", []string{
			`render_release_values \`,
			`"$CANDIDATE_VALUES_FILE" "$CANDIDATE_OPERATOR_REPOSITORY" "$IMAGE_TAG" \`,
			`"$CANDIDATE_OPERATOR_DIGEST" "$MANAGER_PULL_SECRET" "$APPLY_POLICY_EXEMPT_GROUPS"`,
		}),
		exactSourceLineSequence("release namespace and image-pull bootstrap", []string{
			`kubectl --kubeconfig "$KUBECONFIG_FILE" create namespace "$OPERATOR_NAMESPACE" >/dev/null`,
			// The phases' namespaces come from the bootstrap too: every suite
			// needs them, and creating one proves nothing.
			`kubectl --kubeconfig "$KUBECONFIG_FILE" create namespace "$TEST_NAMESPACE" >/dev/null`,
			`kubectl --kubeconfig "$KUBECONFIG_FILE" create namespace "$FOREIGN_NAMESPACE" >/dev/null`,
			`jq -n \`,
			`--arg name "$MANAGER_PULL_SECRET" \`,
			`--arg namespace "$OPERATOR_NAMESPACE" \`,
			`--arg registry "$REGISTRY_HOST" \`,
			`--slurpfile credentials "$REGISTRY_CREDENTIALS_FILE" '`,
		}),
		exactSourceLine("image-pull Secret creation before Helm", `' | kubectl --kubeconfig "$KUBECONFIG_FILE" create -f - >/dev/null`),
		exactSourceLineSequence("immediate current-release install readiness gate", []string{
			`require_ready_nodes "immediately before current-release Helm install"`,
			`if command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \`,
			`"$CHART_PACKAGE" \`,
			`--namespace "$OPERATOR_NAMESPACE" \`,
			`--wait \`,
			`--timeout 5m \`,
			`--values "$CANDIDATE_VALUES_FILE"; then`,
			`:`,
			`else`,
			`current_install_status=$?`,
			`if nodes_ready_now; then`,
		}),
		exactSourceLineSequence("post-install-failure readiness classification", []string{
			`fail "current-release installation failed while Kubernetes nodes were Ready at the immediate post-failure check (Helm exit $current_install_status)"`,
			`fi`,
			`collect_node_readiness_diagnostics "immediately after current-release Helm install failed"`,
			`fail "infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)"`,
			`fi`,
		}),
		exactSourceLine("candidate upgrade lifecycle", `run_recorded_phase upgrade run_go_phase upgrade`),
		exactSourceLine("high-availability lifecycle", `run_recorded_phase ha run_go_phase ha`),
		exactSourceLine("control-plane lifecycle", `run_recorded_phase assert run_go_phase assert`),
		exactSourceLine("certificate lifecycle", `run_recorded_phase cert-rotation run_go_phase cert-rotation`),
		exactSourceLine("data-plane and OCI lifecycle", `run_recorded_phase dataplane run_go_phase dataplane`),
		exactSourceLine("PostgreSQL migration lifecycle", `run_recorded_phase migrations-postgresql run_go_phase migrations-postgresql`),
		exactSourceLine("MySQL migration lifecycle", `run_recorded_phase migrations-mysql run_go_phase migrations-mysql`),
		exactSourceLine("PostgreSQL reference-data lifecycle", `run_recorded_phase reference-data-postgresql run_go_phase reference-data-postgresql`),
		exactSourceLine("MySQL reference-data lifecycle", `run_recorded_phase reference-data-mysql run_go_phase reference-data-mysql`),
		exactSourceLine("uninstall lifecycle", `run_recorded_phase uninstall run_go_phase uninstall`),
		exactSourceLine("post-lifecycle installed chart export", `export_release_chart`),
		// The pass line below is reachable only for a run that left no phase out.
		// A diagnosis run says so in its own words and stops before it.
		exactSourceLineSequence("diagnosis-only terminal evidence", []string{
			`if [ -n "$SKIPPED_PHASES" ]; then`,
			`printf 'e2e: DIAGNOSIS ONLY Kubernetes=%s cluster=%s: phases left out:%s; this is not a lifecycle result\n' \`,
			`"$server_version" "$CLUSTER_NAME" "$SKIPPED_PHASES"`,
			`else`,
		}),
		exactSourceLine("terminal Kubernetes lifecycle evidence", `printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"`),
	}
	if err := verifyOrderedSourceContract(harness, harnessContents, harnessContract); err != nil {
		return err
	}
	// Leaving a phase out is allowed for a diagnosis run and is what makes the
	// pass line unreachable. The two must stay tied together: a branch that skips
	// a phase without recording it would let a run print a pass it did not earn.
	if !exactSourceLineSequence("diagnosis phase record", []string{
		`case " $E2E_DIAGNOSIS_SKIP_PHASES " in`,
		`*" $recorded_phase "*)`,
		`SKIPPED_PHASES="$SKIPPED_PHASES $recorded_phase"`,
	}).pattern.Match(harnessContents) {
		return fmt.Errorf("%s: the diagnosis phase record must name every phase the run leaves out", harness)
	}
	if err := verifyE2ESourceSnapshot(harness, harnessContents); err != nil {
		return err
	}
	// The export publishes the chart this lifecycle installed, and a release
	// ships that file. It has to copy the tested package, compare it, and
	// refuse to replace a file that appeared at the target meanwhile, which
	// ln does and mv does not.
	if err := verifyShellFunctionLines(harness, harnessContents, "export_release_chart", []sourceContractStep{
		exactSourceLine("installed chart export source", `if ! cp "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"; then`),
		exactSourceLine("installed chart export comparison", `if ! cmp -s "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"; then`),
		exactSourceLine("installed chart export without replacement", `if ! ln "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"; then`),
	}); err != nil {
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
	// The control planes start their static pods after their nodes report
	// Ready, so this waits for the shape rather than sampling it. The wait is
	// audited with the assertion it serves: a snapshot that is wrong has to
	// stay a refusal instead of becoming a timeout.
	if err := verifyExactShellFunctionContract(
		harness,
		harnessContents,
		"wait_for_control_plane_component_shape",
		controlPlaneComponentShapeContract,
		"control-plane component shape contract",
	); err != nil {
		return err
	}
	if !bytes.Contains(harnessContents, []byte("\nCONTROL_PLANE_SHAPE_DEADLINE_SECONDS=180\n")) {
		return fmt.Errorf("%s: the control-plane shape wait must carry a bounded, stated deadline", harness)
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
	if err := verifyExactShellFunctionContract(harness, harnessContents,
		"assert_kubelet_log_budget", kubeletLogBudgetContract, "kubelet log retention contract"); err != nil {
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
	// Every Go phase passes through this one line, and the binary refuses a
	// run that reached no phase only if it is asked for one: a runner that
	// returned without running it would pass every Go phase at once.
	if err := verifyExactShellFunctionContract(
		harness,
		harnessContents,
		"run_go_phase",
		goPhaseRunnerContract,
		"Go phase runner contract",
	); err != nil {
		return err
	}
	// The runner is pinned whole, and so is what it runs: one assignment of
	// the binary's path, which the build writes and the runner reads, and no
	// other mention of the name that could point it somewhere else.
	if count := len(sourceLinePattern(goPhaseBinaryAssignment).FindAll(harnessContents, -1)); count != 1 {
		return fmt.Errorf("%s: %s must be assigned exactly once, found %d", harness, goPhaseBinaryAssignment, count)
	}
	if count := len(regexp.MustCompile(`GO_PHASE_BINARY\b`).FindAll(harnessContents, -1)); count != 3 {
		return fmt.Errorf("%s: GO_PHASE_BINARY must appear exactly three times -- assigned, built and run -- and appears %d times",
			harness, count)
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
		"assert_kubelet_log_budget",
		"assert_api_server_endpoint_inventory",
		"probe_api_server_endpoints",
		"configure_registry_hosts_on_kind_nodes",
		"require_ready_nodes",
		"suite_isolation_worker",
		"append_api_server_feature_gate_patch",
		"assert_api_server_feature_gate_scope",
		"wait_for_control_plane_component_shape",
		"task_claim_matches_owner",
		"acquire_task_claim",
		"image_audit_container_matches_task",
		"create_image_audit_container",
		"remove_image_audit_container",
		"run_go_phase",
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
	// The harness has one early successful exit: the mode that brings the
	// environment up for the demonstration and keeps it. It is audited in full
	// below and then hidden from the scan, so every other early exit is still
	// refused -- which is the whole of what that scan is for.
	handoff, err := auditBootstrapHandoff(harness, harnessContents)
	if err != nil {
		return err
	}
	// And a second: the mode that builds the shared task images and stops
	// before the cluster. Audited the same way, and hidden after it is.
	handoff, err = auditImageHandoff(harness, handoff)
	if err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(harness, handoff, harnessContract[len(harnessContract)-1].pattern); err != nil {
		return err
	}

	// Last, so that a phase hidden behind an always-false branch or dropped
	// from the recorded set is reported as the control-flow defect it is. This
	// audit reads the call the shell would build and would otherwise answer a
	// missing phase with a missing binding.
	return verifyPhaseEnvironmentContracts(files)
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
	Role                 string            `yaml:"role"`
	Labels               map[string]string `yaml:"labels"`
	KubeadmConfigPatches []string          `yaml:"kubeadmConfigPatches"`
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

// isolationNodeKeyDeclaration names the key the isolation worker is labelled
// and tainted with. The driver provisions the worker under it; the phases that
// isolate the worker are the ones test/e2e/phases declares IsolatesNode for.
const isolationNodeKeyDeclaration = "ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolation"

const kindKubeletPatch = `kind: KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
featureGates:
  KubeletInUserNamespace: true`

// kindIsolationJoinPatch is the one taint the isolation worker registers with.
// NoSchedule keeps everything off it that does not tolerate the key, and
// nothing that is already running is evicted by it.
const kindIsolationJoinPatch = `kind: JoinConfiguration
nodeRegistration:
  taints:
    - key: operator.ptah.run/e2e-isolation
      value: "true"
      effect: NoSchedule`

func decodeKindClusterTemplate(path string, contents []byte) (kindClusterTemplate, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var config kindClusterTemplate
	if err := decoder.Decode(&config); err != nil {
		return kindClusterTemplate{}, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return kindClusterTemplate{}, fmt.Errorf("%s: multiple YAML documents are forbidden", path)
		}
		return kindClusterTemplate{}, fmt.Errorf("decode trailing %s document: %w", path, err)
	}
	return config, nil
}

// verifyKindHAConfig audits the cluster twice: as the template renders it, and
// with the isolation worker appended the way hack/e2e-kind.sh appends it for a
// suite that declares one. Both are the same three control planes and one
// worker; the second has one more worker, carrying the isolation label and the
// isolation taint and nothing else, and no other node carries either.
func verifyKindHAConfig(path, isolationPath string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	config, err := decodeKindClusterTemplate(path, contents)
	if err != nil {
		return err
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
	if err := verifyKindHANodes(path, config.Nodes, wantRoles); err != nil {
		return err
	}

	isolation, err := os.ReadFile(isolationPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", isolationPath, err)
	}
	combined := append(append([]byte(nil), contents...), isolation...)
	withWorker, err := decodeKindClusterTemplate(isolationPath+" appended to "+path, combined)
	if err != nil {
		return err
	}
	if len(withWorker.Nodes) != len(wantRoles)+1 {
		return fmt.Errorf("%s: appended to %s it makes %d nodes, want exactly five", isolationPath, path, len(withWorker.Nodes))
	}
	if err := verifyKindHANodes(path, withWorker.Nodes[:len(wantRoles)], wantRoles); err != nil {
		return err
	}
	worker := withWorker.Nodes[len(wantRoles)]
	if worker.Role != "worker" {
		return fmt.Errorf("%s: the isolation worker's role is %q, want \"worker\"", isolationPath, worker.Role)
	}
	key := strings.TrimPrefix(isolationNodeKeyDeclaration, "ISOLATION_NODE_KEY=")
	if len(worker.Labels) != 1 || worker.Labels[key] != "true" {
		return fmt.Errorf("%s: the isolation worker must carry exactly the label %s=true", isolationPath, key)
	}
	if len(worker.KubeadmConfigPatches) != 2 ||
		strings.TrimSpace(worker.KubeadmConfigPatches[0]) != kindKubeletPatch ||
		strings.TrimSpace(worker.KubeadmConfigPatches[1]) != kindIsolationJoinPatch {
		return fmt.Errorf("%s: the isolation worker must carry the kubelet feature-gate patch and exactly the isolation taint", isolationPath)
	}
	return nil
}

func verifyKindHANodes(path string, nodes []kindNodeTemplate, wantRoles []string) error {
	for index, node := range nodes {
		if node.Role != wantRoles[index] {
			return fmt.Errorf("%s: kind node %d role is %q, want %q", path, index, node.Role, wantRoles[index])
		}
		if len(node.KubeadmConfigPatches) != 1 || strings.TrimSpace(node.KubeadmConfigPatches[0]) != kindKubeletPatch {
			return fmt.Errorf("%s: kind node %d does not have the exact kubelet feature-gate patch", path, index)
		}
		if len(node.Labels) != 0 {
			return fmt.Errorf("%s: kind node %d carries labels, and only the isolation worker is labelled", path, index)
		}
	}
	return nil
}

// verifyStaticChecksWiring holds hack/e2e-static.sh to the self-tests it
// runs: each wired once, in order, and none behind a branch that skips it or
// an exit that ends the script before it.
func verifyStaticChecksWiring(files e2eWiringFiles) error {
	staticContents, err := os.ReadFile(files.staticChecks)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.staticChecks, err)
	}
	if err := verifyShellScriptEntrypoint(files.staticChecks, staticContents); err != nil {
		return err
	}
	staticContract := []sourceContractStep{
		exactSourceLine("fail-fast shell mode", "set -eu"),
		exactSourceLine("static-check repository root setup", `unset CDPATH`),
		// The stopwatch wraps the code that decides whether the operator works.
		// A measurement that swallowed a failure would read as a pass, so its
		// self-test is wired here on the same terms as the others.
		exactSourceLine("timing self-test wiring", `"$ROOT_DIR/hack/e2e-timing-selftest.sh"`),
		// The refusals that keep a shared image from being another commit's are
		// shell, and a shell refusal nothing exercises is a comment.
		exactSourceLine("shared-image self-test wiring", `"$ROOT_DIR/hack/e2e-shared-images-selftest.sh"`),
		// A suite that stopped running a phase is a green job that proves less,
		// so the shell that selects the phases is measured too.
		exactSourceLine("acceptance suite self-test wiring", `"$ROOT_DIR/hack/e2e-suites-selftest.sh"`),
		// The bootstrap waits for a control plane that is still joining and
		// refuses one that is wrong, and those are two behaviors of the same
		// loop. A loop that stopped refusing would still look like it waited.
		exactSourceLine("control-plane shape self-test wiring", `"$ROOT_DIR/hack/e2e-control-plane-shape-selftest.sh"`),
	}
	if err := verifyOrderedSourceContract(files.staticChecks, staticContents, staticContract); err != nil {
		return err
	}
	if bytes.Count(staticContents, []byte("e2e-timing-selftest.sh")) != 1 {
		return fmt.Errorf("%s: the timing self-test must be wired exactly once", files.staticChecks)
	}
	if bytes.Count(staticContents, []byte("e2e-shared-images-selftest.sh")) != 1 {
		return fmt.Errorf("%s: the shared-image self-test must be wired exactly once", files.staticChecks)
	}
	if bytes.Count(staticContents, []byte("e2e-suites-selftest.sh")) != 1 {
		return fmt.Errorf("%s: the acceptance suite self-test must be wired exactly once", files.staticChecks)
	}
	if bytes.Count(staticContents, []byte("e2e-control-plane-shape-selftest.sh")) != 1 {
		return fmt.Errorf("%s: the control-plane shape self-test must be wired exactly once", files.staticChecks)
	}
	for _, step := range []sourceContractStep{
		staticContract[2], staticContract[3], staticContract[4], staticContract[5],
	} {
		if err := rejectStaticControlFlowBypass(files.staticChecks, staticContents, step.pattern); err != nil {
			return err
		}
		if err := rejectEarlySuccessfulExit(files.staticChecks, staticContents, step.pattern); err != nil {
			return err
		}
	}
	return nil
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
	if _, err := os.Stat(files.controllerSchemaContract); err != nil {
		return fmt.Errorf("read %s: %w", files.controllerSchemaContract, err)
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

// verifyMakeRaceTargets holds the race pass to its audited rule, and the tests
// it skips to the shell mutation suites and nothing else. Those suites run only
// in the test target, so that target has to stay every package with nothing
// skipped and a timeout ./hack fits in, and verify-source, which the verify job
// runs, has to run it.
func verifyMakeRaceTargets(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	const skipped = "override RACE_MUTATION_TESTS := " +
		"TestVerifyE2EHarnessRejectsCriticalMutations|" +
		"TestVerifyFailedUpgradeEvidenceRejectsCriticalMutations|" +
		"TestVerifyE2EChildScriptsRejectCriticalMutations"
	assignments := regexp.MustCompile(`(?m)^(?:override[ \t]+)?RACE_MUTATION_TESTS[ \t]*[:+?!]?=[^\r\n]*$`).FindAll(contents, -1)
	if len(assignments) != 1 || string(assignments[0]) != skipped {
		return fmt.Errorf("%s: RACE_MUTATION_TESTS must be the exact audited list of suites the race pass skips", path)
	}
	parsed, err := parseAuditedMakefile(path, contents)
	if err != nil {
		return err
	}
	race, err := parsed.requireTarget(path, "test-race", "test-race:")
	if err != nil {
		return err
	}
	raceRule := exactMakeRule(parsed.lines, race.line)
	if !strings.Contains(raceRule, "\n\t$(GO) test -race -count=1 -timeout=10m -skip '^($(RACE_MUTATION_TESTS))$$' ./...") {
		return fmt.Errorf("%s: test-race must run every package under the race detector, skipping only RACE_MUTATION_TESTS", path)
	}
	test, err := parsed.requireTarget(path, "test", "test:")
	if err != nil {
		return err
	}
	if exactMakeRule(parsed.lines, test.line) != fmt.Sprintf("test:\n\t$(GO) test -timeout=%dm ./...", makeTestTimeoutMinutes) {
		return fmt.Errorf("%s: test must run every package with nothing skipped and a %d-minute timeout, because it is the only run of the shell mutation suites",
			path, makeTestTimeoutMinutes)
	}
	sources := parsed.rules["verify-source"]
	if len(sources) != 1 || sources[0].operator != ":" || sources[0].conditionalDepth != 0 {
		return fmt.Errorf("%s: verify-source target must be declared exactly once, unconditionally", path)
	}
	_, prerequisites, _ := strings.Cut(sources[0].raw, ":")
	if comment := strings.IndexByte(prerequisites, '#'); comment >= 0 {
		prerequisites = prerequisites[:comment]
	}
	if !slices.Contains(strings.Fields(prerequisites), "test") {
		return fmt.Errorf("%s: verify-source must run the test target, the only run of the shell mutation suites", path)
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

func verifyFailurePreservingExitTrap(path string, contents []byte, cleanups ...string) error {
	expected := make(map[string]int, len(cleanups))
	for _, cleanup := range cleanups {
		expected["trap "+cleanup+" EXIT"] = 0
	}
	exitTraps := regexp.MustCompile(`(?m)^[ \t]*trap[ \t]+[^\r\n]*(?:^|[ \t])(?:EXIT|0)(?:[ \t]|$)[^\r\n]*\r?$`).FindAll(contents, -1)
	for _, raw := range exitTraps {
		line := strings.TrimSpace(string(raw))
		if _, ok := expected[line]; ok {
			expected[line]++
		} else if strings.HasPrefix(line, "trap - ") {
			// A cleanup routine may disable its own trap before preserving the
			// captured status. This is not an alternate EXIT handler.
		} else {
			return fmt.Errorf("%s: lifecycle script has an unaudited failure-preserving trap replacement %q", path, line)
		}
	}
	for trap, count := range expected {
		if count != 1 {
			return fmt.Errorf("%s: lifecycle script must have exactly one failure-preserving %s", path, trap)
		}
	}
	return nil
}

// The images travel between jobs under one name, and both ends name it here so
// a rename cannot leave the matrix silently building its own.
const (
	e2eImagesArtifactName = "shared-task-images"
	e2eImagesArtifactPath = "${{ runner.temp }}/task-images"
	uploadArtifactPin     = "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a"
	downloadArtifactPin   = "actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c"
)

// verifySharedImageHandover requires the built images to leave the preparation
// job as an artifact the acceptance matrix can read, under a pinned action and
// with a missing file treated as a failure. An upload that found nothing and
// passed would hand every lifecycle an empty directory.
func verifySharedImageHandover(path string, job workflowJob, name string) error {
	for _, step := range job.Steps {
		if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			continue
		}
		if step.With["name"] != name {
			continue
		}
		if step.Uses != uploadArtifactPin {
			return fmt.Errorf("%s: the shared images are uploaded by %q rather than the pinned action", path, step.Uses)
		}
		if step.If != "" {
			return fmt.Errorf("%s: the shared-image upload must be unconditional", path)
		}
		if step.With["path"] != e2eImagesArtifactPath {
			return fmt.Errorf("%s: the shared images are uploaded from %q", path, step.With["path"])
		}
		if step.With["if-no-files-found"] != "error" {
			return fmt.Errorf("%s: a shared-image upload that finds no images must fail", path)
		}
		return nil
	}
	return fmt.Errorf("%s: prepare-images does not hand the %s artifact to the matrix", path, name)
}

// verifySharedImageCollection requires every lifecycle to take its images from
// that artifact, before it runs, under the pinned action. The images themselves
// are checked by the harness against this run's commit and Ptah pin.
func verifySharedImageCollection(path string, job workflowJob, name string) error {
	collected := -1
	lifecycle := -1
	for index, step := range job.Steps {
		if step.Run == "make e2e" && lifecycle < 0 {
			lifecycle = index
		}
		if !strings.HasPrefix(step.Uses, "actions/download-artifact@") || step.With["name"] != name {
			continue
		}
		if step.Uses != downloadArtifactPin {
			return fmt.Errorf("%s: the shared images are collected by %q rather than the pinned action", path, step.Uses)
		}
		if step.If != "" {
			return fmt.Errorf("%s: the shared-image collection must be unconditional", path)
		}
		if step.With["path"] != e2eImagesArtifactPath {
			return fmt.Errorf("%s: the shared images are collected into %q", path, step.With["path"])
		}
		collected = index
	}
	if collected < 0 {
		return fmt.Errorf("%s: the lifecycle does not collect the %s artifact", path, name)
	}
	if lifecycle < 0 || collected > lifecycle {
		return fmt.Errorf("%s: the shared images are collected after the lifecycle that needs them", path)
	}
	return nil
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
		return fmt.Errorf("%s: current-release Helm installation must have exactly one semantic install attempt, found %d", path, len(attempts))
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

func verifyShellFunctionLines(path string, contents []byte, name string, lines []sourceContractStep) error {
	functionPattern := regexp.MustCompile(
		`(?ms)^` + regexp.QuoteMeta(name) + `\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`,
	)
	matches := functionPattern.FindAll(contents, -1)
	if len(matches) != 1 {
		return fmt.Errorf("%s: %s must have exactly one auditable function body, found %d", path, name, len(matches))
	}
	for _, line := range lines {
		if !line.pattern.Match(matches[0]) {
			return fmt.Errorf("%s: %s is missing its %s", path, name, line.name)
		}
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

// Every lifecycle phase is a Go phase, and what it proves depends on what the
// driver handed it: a kubeconfig that is not the cluster the suite built, a
// controller image that is not the candidate, or a state version nothing
// pinned would each leave the phase running and its verdict meaningless. The
// phase declares its inputs in test/e2e/phases, and the audit below holds the
// driver's call to exactly those, each bound to the variable goPhaseBindings
// names.
//
// It names one property per binding rather than pinning the block of source
// the call happens to occupy. A block match answers a question nobody asked --
// whether the text moved -- and answers it with `found 0`, which does not say
// which of the guarantees above stopped being checked. It also fails on an
// addition that takes nothing away, which is how #113 removed every lifecycle
// verdict from master by handing the migrations phase two variables it
// genuinely needed.
type phaseEnvironmentBinding struct {
	name  string
	value string
}

// goPhaseBindings is what the driver binds each input a Go phase reads to.
//
// A Go phase declares its inputs in test/e2e/phases, as a struct the compiler
// holds the phase to. What a declaration cannot say is which of the driver's
// variables feeds an input. That is one
// line per variable rather than one block per phase, because an input means
// the same thing in every phase that reads it.
var goPhaseBindings = map[string]string{
	"E2E_KUBECONFIG":               `$KUBECONFIG_FILE`,
	"E2E_OPERATOR_NAMESPACE":       `$OPERATOR_NAMESPACE`,
	"E2E_TEST_NAMESPACE":           `$TEST_NAMESPACE`,
	"E2E_FOREIGN_NAMESPACE":        `$FOREIGN_NAMESPACE`,
	"E2E_HELM_RELEASE":             `$HELM_RELEASE`,
	"E2E_CHART_PACKAGE":            `$CHART_PACKAGE`,
	"E2E_EXECUTOR_IMAGE":           `$E2E_EXECUTOR_IMAGE`,
	"E2E_RUNNER_IMAGE":             `$E2E_RUNNER_IMAGE`,
	"E2E_PTAH_VERSION":             `$E2E_PTAH_VERSION`,
	"E2E_CONTROLLER_IMAGE":         `$CANDIDATE_OPERATOR_IMAGE`,
	"E2E_CONTROLLER_REVISION":      `$CONTROLLER_REVISION`,
	"E2E_CONTROLLER_STATE_VERSION": `$CONTROLLER_STATE_VERSION`,
	"E2E_FIXTURE_IMAGE":            `$E2E_FIXTURE_IMAGE`,
	// The lifecycle phases: the namespace the upgrade phase keeps its proof
	// objects in, the release it installed from, the synthetic next release,
	// the exact Kubernetes version, and the switch that prints a refused Helm
	// operation's stderr off CI.
	"E2E_DEBUG_LOGS":            `$E2E_DEBUG_LOGS`,
	"E2E_PROOF_NAMESPACE":       `$CRD_PROOF_NAMESPACE`,
	"E2E_CANDIDATE_VALUES_FILE": `$CANDIDATE_VALUES_FILE`,
	"E2E_KUBERNETES_VERSION":    `$K8S_VERSION`,
	"E2E_NEXT_CHART_PACKAGE":    `$NEXT_CHART_PACKAGE`,
	"E2E_NEXT_VALUES_FILE":      `$NEXT_VALUES_FILE`,
	"E2E_NEXT_CONTROLLER_IMAGE": `$NEXT_CONTROLLER_IMAGE`,
	"E2E_HA_TEST_NAMESPACE":     `$HA_TEST_NAMESPACE`,
	// The alerting phase's monitoring path, mirrored into the registry only
	// where a suite runs that phase.
	"E2E_PROMETHEUS_IMAGE":          `$E2E_PROMETHEUS_IMAGE`,
	"E2E_ALERTMANAGER_IMAGE":        `$E2E_ALERTMANAGER_IMAGE`,
	"E2E_POSTGRES_IMAGE":            `$E2E_POSTGRES_IMAGE`,
	"E2E_MYSQL_IMAGE":               `$E2E_MYSQL_IMAGE`,
	"E2E_REGISTRY_IP":               `$REGISTRY_IP`,
	"E2E_REGISTRY_SERVICE":          `$REGISTRY_SERVICE`,
	"E2E_REGISTRY_PORT":             `$E2E_REGISTRY_PORT`,
	"E2E_REGISTRY_CREDENTIALS_FILE": `$REGISTRY_CREDENTIALS_FILE`,
	// The registry as the host reaches it, for the one migration artifact no
	// product command can produce.
	"E2E_REGISTRY_HOST_ADDRESS": `$REMOTE_REGISTRY`,
	"E2E_DOCKER_CONTEXT":        `$DOCKER_CONTEXT`,
	"E2E_REGISTRY_CONTAINER_ID": `$REGISTRY_CONTAINER_ID`,
	// The kind cluster's name, which the isolation worker's node container is
	// named from.
	"E2E_KIND_CLUSTER_NAME": `$CLUSTER_NAME`,
	// The external PostgreSQL runs from the digest the source image resolved
	// to, and the kind cluster's name is the owner its container is labeled
	// with.
	"E2E_EXTERNAL_POSTGRES_CONTAINER_ID":     `$EXTERNAL_PG_CONTAINER_ID`,
	"E2E_EXTERNAL_POSTGRES_IP":               `$EXTERNAL_PG_IP`,
	"E2E_EXTERNAL_POSTGRES_SERVICE":          `$EXTERNAL_PG_SERVICE`,
	"E2E_EXTERNAL_POSTGRES_IMAGE":            `$E2E_POSTGRES_SOURCE_IMAGE`,
	"E2E_EXTERNAL_POSTGRES_OWNER":            `$CLUSTER_NAME`,
	"E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE": `$EXTERNAL_PG_CREDENTIALS_FILE`,
	"E2E_TLS_PROXY_SERVICE":                  `$TLS_PROXY_SERVICE`,
	"E2E_TLS_PROXY_CA_FILE":                  `$TLS_PROXY_CA_FILE`,
	"E2E_TLS_PROXY_CERT_FILE":                `$TLS_PROXY_CERT_FILE`,
	"E2E_TLS_PROXY_KEY_FILE":                 `$TLS_PROXY_CERT_KEY_FILE`,
	// full or prepare: the migration suites run the data plane for the
	// namespace it stands up and none of its own acceptance.
	"E2E_DATAPLANE_MODE": `$DATAPLANE_MODE`,
}

// phaseInvocationPattern matches one `run_recorded_phase <name> run_go_phase
// <phase>` call, and one `run_recorded_phase <name> "$ROOT_DIR/<script>"` call
// so that a phase run as a script is found and refused. The environment is read
// backwards from it rather than listed here, so what the audit compares against
// is the command the shell actually builds.
var phaseInvocationPattern = regexp.MustCompile(
	`(?m)^[ \t]*run_recorded_phase ([a-z][a-z0-9-]*) ` +
		`(?:"\$ROOT_DIR/(hack/[a-z0-9-]+\.sh)"|run_go_phase ([a-z][a-z0-9-]*))[ \t]*\r?$`)

var phaseEnvironmentAssignmentPattern = regexp.MustCompile(
	`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)=(\S*) \\[ \t]*\r?$`)

type phaseInvocation struct {
	phase  string
	script string
	// goPhase is the phase run_go_phase is asked for, empty for a script.
	goPhase  string
	bindings []phaseEnvironmentBinding
}

// findPhaseInvocations reads each phase call and the assignments that prefix
// it. The prefix ends at the first line that is not a backslash-continued
// assignment, which is where the shell stops treating it as this command's
// environment.
func findPhaseInvocations(contents []byte) []phaseInvocation {
	lines := strings.Split(string(contents), "\n")
	invocations := make([]phaseInvocation, 0, len(phases.All()))
	for index, line := range lines {
		match := phaseInvocationPattern.FindStringSubmatch(line + "\n")
		if match == nil {
			continue
		}
		invocation := phaseInvocation{phase: match[1], script: match[2], goPhase: match[3]}
		for previous := index - 1; previous >= 0; previous-- {
			assignment := phaseEnvironmentAssignmentPattern.FindStringSubmatch(lines[previous] + "\n")
			if assignment == nil {
				break
			}
			invocation.bindings = append([]phaseEnvironmentBinding{
				{name: assignment[1], value: assignment[2]},
			}, invocation.bindings...)
		}
		invocations = append(invocations, invocation)
	}
	return invocations
}

func verifyPhaseEnvironmentContracts(files e2eWiringFiles) error {
	harness, err := os.ReadFile(files.harness)
	if err != nil {
		return fmt.Errorf("read %s: %w", files.harness, err)
	}
	invocations := findPhaseInvocations(harness)
	seen := map[string]phaseInvocation{}
	for _, invocation := range invocations {
		if _, duplicate := seen[invocation.phase]; duplicate {
			return fmt.Errorf("%s: lifecycle phase %q is invoked more than once", files.harness, invocation.phase)
		}
		seen[invocation.phase] = invocation
	}
	if len(invocations) == 0 {
		return fmt.Errorf("%s: no lifecycle phase is invoked, so no environment was audited", files.harness)
	}
	for _, phase := range phases.All() {
		invocation, present := seen[phase.Name]
		if !present {
			return fmt.Errorf("%s: Go phase %q is never invoked", files.harness, phase.Name)
		}
		delete(seen, phase.Name)
		if err := verifyGoPhaseInvocation(files.harness, phase, invocation); err != nil {
			return err
		}
	}
	for phase, invocation := range seen {
		if invocation.script != "" {
			return fmt.Errorf("%s: lifecycle phase %q runs %s; every phase is a Go phase test/e2e/phases declares",
				files.harness, phase, invocation.script)
		}
		return fmt.Errorf("%s: lifecycle phase %q is invoked but test/e2e/phases does not declare it",
			files.harness, phase)
	}
	return nil
}

// goPhaseBinding is what the driver binds one input of a Go phase to.
// E2E_ENGINE is the one input whose value differs between the phases that read
// it: each migration and reference-data phase runs the engine its name ends
// in, so a phase bound to the other engine would cover one engine twice and
// leave the other unproven, with both jobs green.
func goPhaseBinding(phase, input string) (string, bool) {
	if input == engineInput {
		for _, family := range []string{"migrations-", "reference-data-"} {
			if engine, ok := strings.CutPrefix(phase, family); ok && (engine == "postgresql" || engine == "mysql") {
				return engine, true
			}
		}
		return "", false
	}
	value, known := goPhaseBindings[input]
	return value, known
}

// engineInput is the input goPhaseBinding derives from the phase's name.
const engineInput = "E2E_ENGINE"

// verifyGoPhaseInvocation holds the driver's call of a Go phase to the inputs
// test/e2e/phases declares for it: every one bound, to the driver variable
// goPhaseBindings names, and nothing else. The phase cannot read an input it
// did not declare, since it only ever sees its own struct, so a binding beyond
// the declaration is one nothing reads.
func verifyGoPhaseInvocation(path string, phase phases.Phase, invocation phaseInvocation) error {
	if invocation.goPhase == "" {
		return fmt.Errorf("%s: %s is a Go phase and must run through run_go_phase %s, not %s",
			path, phase.Name, phase.Name, invocation.script)
	}
	if invocation.goPhase != phase.Name {
		return fmt.Errorf("%s: run_recorded_phase %s runs the Go phase %s; the ledger would name one phase and the binary run another",
			path, phase.Name, invocation.goPhase)
	}
	declared := map[string]bool{}
	for _, name := range phase.Inputs() {
		declared[name] = true
	}
	bound := map[string]string{}
	for _, binding := range invocation.bindings {
		if _, duplicate := bound[binding.name]; duplicate {
			return fmt.Errorf("%s: %s phase binds %s twice", path, phase.Name, binding.name)
		}
		if !declared[binding.name] {
			return fmt.Errorf("%s: %s phase binds %s, which test/e2e/phases does not declare it reads",
				path, phase.Name, binding.name)
		}
		bound[binding.name] = binding.value
	}
	for _, name := range phase.Inputs() {
		want, known := goPhaseBinding(phase.Name, name)
		if !known {
			return fmt.Errorf("%s: %s phase reads %s, and goPhaseBindings does not say which driver variable feeds it",
				path, phase.Name, name)
		}
		value, present := bound[name]
		if !present {
			return fmt.Errorf("%s: %s phase must bind %s to %q, and binds nothing", path, phase.Name, name, want)
		}
		if value != want {
			return fmt.Errorf("%s: %s phase must bind %s to %q, and binds %q", path, phase.Name, name, want, value)
		}
	}
	return nil
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

// bootstrapHandoffOpener is the one block in the harness that may end in a
// successful exit before the lifecycle has run.
const bootstrapHandoffOpener = `if [ "$E2E_STOP_AFTER" = bootstrap ]; then`

const imageHandoffOpener = `if [ "$E2E_STOP_AFTER" = images ]; then`

// auditImageHandoff audits the harness's second early exit: the mode that builds
// the four task images, writes them for the matrix to load, and stops before a
// cluster exists.
//
// It is audited on the same terms as the demonstration hand-off and then hidden
// from the early-exit scan, so every other early exit is still refused. What
// this requires is that the mode does what its name says and nothing else: it
// writes the images, latches the run as complete so the exit trap reports a
// pass rather than a silent failure, and ends at its own exit.
func auditImageHandoff(path string, contents []byte) ([]byte, error) {
	opener := []byte("\n" + imageHandoffOpener + "\n")
	start := bytes.Index(contents, opener)
	if start < 0 {
		return nil, fmt.Errorf("%s: the shared-image hand-off is missing its audited opener", path)
	}
	if bytes.Count(contents, opener) != 1 {
		return nil, fmt.Errorf("%s: the shared-image hand-off opener appears more than once", path)
	}
	closer := []byte("\nfi\n")
	end := bytes.Index(contents[start+len(opener):], closer)
	if end < 0 {
		return nil, fmt.Errorf("%s: the shared-image hand-off is not closed at column zero", path)
	}
	block := contents[start+len(opener) : start+len(opener)+end]

	for _, required := range []string{"export_task_images", "PHASE_COMPLETED=1"} {
		if !bytes.Contains(block, []byte(required)) {
			return nil, fmt.Errorf(
				"%s: the shared-image hand-off does not %s, so writing the images is not what it does",
				path, required)
		}
	}
	lines := bytes.Split(bytes.TrimRight(block, "\n"), []byte("\n"))
	if last := bytes.TrimSpace(lines[len(lines)-1]); !bytes.Equal(last, []byte("exit 0")) {
		return nil, fmt.Errorf(
			"%s: the shared-image hand-off ends with %q rather than its exit", path, last)
	}
	if count := bytes.Count(block, []byte("exit")); count != 1 {
		return nil, fmt.Errorf(
			"%s: the shared-image hand-off holds %d exits; it may hold the one it ends with", path, count)
	}

	masked := append([]byte(nil), contents...)
	for index := start + len(opener); index < start+len(opener)+end; index++ {
		if masked[index] != '\n' {
			masked[index] = ' '
		}
	}
	return masked, nil
}

// auditBootstrapHandoff holds the demonstration lab's hand-off to its contract
// and returns the harness with that block masked.
//
// The contract is what makes the early exit safe to permit: the block runs only
// under the mode that asks for it, it releases the cleanup trap it is leaving
// the environment behind for, it writes the environment where the caller named,
// and its last statement is that exit. Masking is by spaces rather than by
// deletion so every line number the scan reports is still the file's.
func auditBootstrapHandoff(path string, contents []byte) ([]byte, error) {
	opener := []byte("\n" + bootstrapHandoffOpener + "\n")
	start := bytes.Index(contents, opener)
	if start < 0 {
		return nil, fmt.Errorf("%s: the demonstration lab hand-off is missing its audited opener", path)
	}
	if bytes.Count(contents, opener) != 1 {
		return nil, fmt.Errorf("%s: the demonstration lab hand-off opener appears more than once", path)
	}
	closer := []byte("\nfi\n")
	end := bytes.Index(contents[start+len(opener):], closer)
	if end < 0 {
		return nil, fmt.Errorf("%s: the demonstration lab hand-off is not closed at column zero", path)
	}
	block := contents[start+len(opener) : start+len(opener)+end]

	for _, required := range []string{
		"trap - EXIT HUP INT TERM",
		`>"$E2E_ENVIRONMENT_FILE"`,
	} {
		if !bytes.Contains(block, []byte(required)) {
			return nil, fmt.Errorf(
				"%s: the demonstration lab hand-off does not %s, so leaving the environment behind is not what it does",
				path, required)
		}
	}
	lines := bytes.Split(bytes.TrimRight(block, "\n"), []byte("\n"))
	if last := bytes.TrimSpace(lines[len(lines)-1]); !bytes.Equal(last, []byte("exit 0")) {
		return nil, fmt.Errorf(
			"%s: the demonstration lab hand-off ends with %q rather than its exit", path, last)
	}
	if count := bytes.Count(block, []byte("exit")); count != 1 {
		return nil, fmt.Errorf(
			"%s: the demonstration lab hand-off holds %d exits; it may hold the one it ends with", path, count)
	}

	if err := auditHandoffNames(path, block, contents); err != nil {
		return nil, err
	}

	masked := append([]byte(nil), contents...)
	for index := start + len(opener); index < start+len(opener)+end; index++ {
		if masked[index] != '\n' {
			masked[index] = ' '
		}
	}
	return masked, nil
}

// handoffExpansion matches a name the hand-off expands, and handoffDefaulted the
// subset that carries its own default at the use site.
var (
	handoffExpansion = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
	handoffDefaulted = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):`)
)

// auditHandoffNames refuses a hand-off that prints a variable the bootstrap
// assigns only on one of its paths.
//
// The harness runs under set -u, and the hand-off is its last act. A name
// assigned inside a conditional -- the Ptah build context, which exists only
// when the executor was built from source -- therefore kills the bootstrap at
// the moment it should be writing the lab's environment, and only for the
// caller who took the other path. A top-level assignment is what makes the
// value's absence an empty string that the reader of the file can act on.
func auditHandoffNames(path string, block, contents []byte) error {
	defaulted := map[string]bool{}
	for _, match := range handoffDefaulted.FindAllSubmatch(block, -1) {
		defaulted[string(match[1])] = true
	}
	reported := map[string]bool{}
	for _, match := range handoffExpansion.FindAllSubmatch(block, -1) {
		name := string(match[1])
		if defaulted[name] || reported[name] {
			continue
		}
		reported[name] = true
		assigned := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=`)
		if assigned.Match(contents) {
			continue
		}
		return fmt.Errorf(
			"%s: the demonstration lab hand-off prints $%s, which nothing assigns at the top level; "+
				"under set -u the bootstrap dies here for whichever caller took the path that skips it, "+
				"so give the name a top-level default", path, name)
	}
	return nil
}

// The acceptance suites: one source of truth for what CI runs and for the check
// that CI runs all of it.

type e2eSuite struct {
	Name    string   `json:"name"`
	Slug    string   `json:"slug"`
	Summary string   `json:"summary"`
	Phases  []string `json:"phases"`
	Prepare []string `json:"prepare"`
	// IsolationWorker gives the suite's cluster the second, tainted worker a
	// phase cuts off from the API server. verifyE2ESuiteIsolationWorker holds
	// it to the phases the suite runs.
	IsolationWorker bool `json:"isolationWorker"`
}

type e2eSuiteCatalog struct {
	Comment []string   `json:"comment"`
	Suites  []e2eSuite `json:"suites"`
}

var (
	e2eSuiteNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	// The phases the driver runs, read out of the driver rather than repeated
	// here: a second list is how a phase ends up covered on paper only.
	e2eDriverPhase = regexp.MustCompile(`(?m)^[ \t]*run_recorded_phase ([a-z][a-z0-9-]*) `)
)

// loadE2ESuites reads the suite catalog and refuses a shape that cannot be
// executed: a suite with no phases, a duplicated name, a phase claimed by two
// suites, or a preparation phase that no suite covers.
func loadE2ESuites(path string) (e2eSuiteCatalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return e2eSuiteCatalog{}, fmt.Errorf("read the acceptance suite catalog: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var catalog e2eSuiteCatalog
	if err := decoder.Decode(&catalog); err != nil {
		return e2eSuiteCatalog{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(catalog.Suites) == 0 {
		return e2eSuiteCatalog{}, fmt.Errorf("%s: names no acceptance suite", path)
	}
	names := map[string]bool{}
	owner := map[string]string{}
	for _, suite := range catalog.Suites {
		if !e2eSuiteNamePattern.MatchString(suite.Name) {
			return e2eSuiteCatalog{}, fmt.Errorf("%s: %q is not a usable suite name", path, suite.Name)
		}
		if suite.Slug != suite.Name {
			return e2eSuiteCatalog{}, fmt.Errorf(
				"%s: suite %q has slug %q; a job, a run id and an artifact are named after the slug, so it is the name",
				path, suite.Name, suite.Slug)
		}
		if strings.TrimSpace(suite.Summary) == "" {
			return e2eSuiteCatalog{}, fmt.Errorf("%s: suite %q says nothing about what it runs", path, suite.Name)
		}
		if names[suite.Name] {
			return e2eSuiteCatalog{}, fmt.Errorf("%s: suite %q appears twice", path, suite.Name)
		}
		names[suite.Name] = true
		if len(suite.Phases) == 0 {
			return e2eSuiteCatalog{}, fmt.Errorf("%s: suite %q runs no phase", path, suite.Name)
		}
		for _, phase := range suite.Phases {
			if !e2eSuiteNamePattern.MatchString(phase) {
				return e2eSuiteCatalog{}, fmt.Errorf("%s: %q is not a usable phase name", path, phase)
			}
			if previous, claimed := owner[phase]; claimed {
				return e2eSuiteCatalog{}, fmt.Errorf(
					"%s: phase %q is claimed by both %q and %q; a phase belongs to one suite so the matrix runs it once",
					path, phase, previous, suite.Name)
			}
			owner[phase] = suite.Name
		}
	}
	for _, suite := range catalog.Suites {
		for _, phase := range suite.Prepare {
			if !e2eSuiteNamePattern.MatchString(phase) {
				return e2eSuiteCatalog{}, fmt.Errorf("%s: %q is not a usable phase name", path, phase)
			}
			if _, covered := owner[phase]; !covered {
				return e2eSuiteCatalog{}, fmt.Errorf(
					"%s: suite %q prepares with phase %q, which no suite runs for its acceptance; preparation is not coverage",
					path, suite.Name, phase)
			}
			if owner[phase] == suite.Name {
				return e2eSuiteCatalog{}, fmt.Errorf(
					"%s: suite %q both runs and prepares with phase %q", path, suite.Name, phase)
			}
		}
	}
	return catalog, nil
}

func e2eSuiteCoveredPhases(catalog e2eSuiteCatalog) int {
	phases := 0
	for _, suite := range catalog.Suites {
		phases += len(suite.Phases)
	}
	return phases
}

// verifyE2ESuiteCoverage refuses a partition that lost a phase.
//
// The driver is the authority on which phases exist: every one it runs has to
// be some suite's acceptance, and every phase the catalog claims has to be one
// the driver runs. Without this, sharding a suite is one edit away from a green
// matrix that stopped running something.
func verifyE2ESuiteCoverage(catalog e2eSuiteCatalog, driverPath string) error {
	contents, err := os.ReadFile(driverPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", driverPath, err)
	}
	driverPhases := map[string]bool{}
	driverOrder := map[string]int{}
	for index, match := range e2eDriverPhase.FindAllSubmatch(contents, -1) {
		driverPhases[string(match[1])] = true
		driverOrder[string(match[1])] = index
	}
	if len(driverPhases) == 0 {
		return fmt.Errorf("%s: no lifecycle phase invocation was found, so coverage cannot be checked", driverPath)
	}
	covered := map[string]string{}
	for _, suite := range catalog.Suites {
		for _, phase := range suite.Phases {
			covered[phase] = suite.Name
		}
	}
	var missing []string
	for phase := range driverPhases {
		if _, ok := covered[phase]; !ok {
			missing = append(missing, phase)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf(
			"%s: the driver runs %s, which no suite in %s covers; a phase outside every suite is one the matrix stopped proving",
			driverPath, strings.Join(missing, ", "), e2eSuitesPath)
	}
	var unknown []string
	for phase, suite := range covered {
		if !driverPhases[phase] {
			unknown = append(unknown, fmt.Sprintf("%s (in %s)", phase, suite))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: claims phases the driver does not run: %s", e2eSuitesPath, strings.Join(unknown, ", "))
	}
	return verifyE2ESuitePrerequisites(catalog, phases.All(), driverOrder)
}

// A phase can be selected while its fixtures are absent. Require each full
// phase's declared prerequisites to run in full and earlier in the driver,
// including phases borrowed for preparation that have no shorter mode.
func verifyE2ESuitePrerequisites(catalog e2eSuiteCatalog, declared []phases.Phase, order map[string]int) error {
	definitions := map[string]phases.Phase{}
	for _, phase := range declared {
		definitions[phase.Name] = phase
	}
	for _, suite := range catalog.Suites {
		full := map[string]bool{}
		for _, phase := range suite.Phases {
			full[phase] = true
		}
		for _, name := range suite.Prepare {
			phase, found := definitions[name]
			if found && phase.Preparation == 0 {
				full[name] = true
			}
		}
		for name := range full {
			phase, found := definitions[name]
			if !found {
				return fmt.Errorf("suite %q runs undeclared phase %q", suite.Name, name)
			}
			for _, required := range phase.RequiresFull {
				before, predecessorFound := order[required]
				after, consumerFound := order[name]
				if !full[required] || !predecessorFound || !consumerFound || before >= after {
					return fmt.Errorf("suite %q phase %q requires full phase %q earlier in the driver", suite.Name, name, required)
				}
			}
		}
	}
	return nil
}

// verifyE2ESuiteIsolationWorker refuses a suite whose cluster disagrees with
// what its phases need. A phase that isolates a node, run in a suite with no
// isolation worker, fails on a node that does not exist. A suite that declares
// the worker and runs no such phase pays for a node nothing uses, on a cluster
// that is no longer the one its phases were measured on. Which phases isolate a
// node is for their declarations in test/e2e/phases to say.
func verifyE2ESuiteIsolationWorker(catalog e2eSuiteCatalog) error {
	isolating := map[string]bool{}
	preparesWithoutFaults := map[string]bool{}
	for _, phase := range phases.All() {
		preparesWithoutFaults[phase.Name] = phase.Preparation > 0
		if phase.IsolatesNode {
			isolating[phase.Name] = true
		}
	}
	if len(isolating) == 0 {
		return errors.New("no phase in test/e2e/phases isolates a node, so the isolation worker cannot be checked against anything")
	}
	for _, suite := range catalog.Suites {
		var needs []string
		for _, phase := range suite.Phases {
			if isolating[phase] {
				needs = append(needs, phase)
			}
		}
		for _, phase := range suite.Prepare {
			if isolating[phase] && !preparesWithoutFaults[phase] {
				needs = append(needs, phase)
			}
		}
		switch {
		case len(needs) > 0 && !suite.IsolationWorker:
			return fmt.Errorf(
				"%s: suite %q runs %s, which isolates a node, and does not declare isolationWorker, so its cluster has no node to isolate",
				e2eSuitesPath, suite.Name, strings.Join(needs, ", "))
		case len(needs) == 0 && suite.IsolationWorker:
			return fmt.Errorf(
				"%s: suite %q declares isolationWorker and runs no phase that isolates a node",
				e2eSuitesPath, suite.Name)
		}
	}
	return nil
}

// e2eSuiteMatrix is the acceptance matrix: every supported minor against every
// suite. The suites are the inner dimension so the newest minor stays last,
// which is where the image build reads the pair it validates.
func e2eSuiteMatrix(minors []matrixEntry, catalog e2eSuiteCatalog) []matrixEntry {
	entries := make([]matrixEntry, 0, len(minors)*len(catalog.Suites))
	for _, minor := range minors {
		for _, suite := range catalog.Suites {
			entry := minor
			entry.Suite = suite.Name
			entry.SuiteSlug = suite.Slug
			entry.SuiteSummary = suite.Summary
			entries = append(entries, entry)
		}
	}
	return entries
}
