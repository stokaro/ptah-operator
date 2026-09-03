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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	manifestPath        = "support/kubernetes.json"
	chartPath           = "charts/ptah-operator/Chart.yaml"
	workflowPath        = ".github/workflows/ci.yml"
	updateWorkflowPath  = ".github/workflows/update-kubernetes-support.yml"
	releaseWorkflowPath = ".github/workflows/release.yml"
	docsPath            = "docs/kubernetes-support.md"
	makefilePath        = "Makefile"
	e2eHarnessPath      = "hack/e2e-kind.sh"
	e2eDataPlanePath    = "hack/e2e-dataplane.sh"
	e2eAssertPath       = "hack/e2e-assert.sh"
	e2eCRDUpgradePath   = "hack/e2e-crd-upgrade.sh"
	e2eFaultsPath       = "hack/e2e-faults.sh"
	e2eHAPath           = "hack/e2e-ha.sh"
	e2eCertRotationPath = "hack/e2e-cert-rotation.sh"

	verificationMaxAgeDays = 35
	// These digests make workflow policy changes explicit. Semantic checks keep
	// failures actionable; the whole-file digests also cover setup steps that
	// could otherwise alter GITHUB_ENV, GITHUB_PATH, or later shell behavior.
	ciWorkflowSHA256     = "b2464cb40e2b3bbd6c4283f86a3993be6bf64822ef4b4055b5521745d3147f33"
	updateWorkflowSHA256 = "a012609ecffb861006cfbfdf09f769dfc1f5b0d7810f78a7fdbe33d4d3266a80"

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
	"prepare/verify":             "ef49b2a3563779c072dfa5368724c3ab5da29e4e922af48fd32c14cb85d92b18",
	"prepare/bundle":             "2cdde53d6c0539ecc80276d50f8652fb6978021734aed4e50b8078ca0826e896",
	"propose/apply-bundle":       "c1ced6a6bf89a62b4a96b2ffb5a171d1ae7e493667b36b0bf6b0a97434ac9b34",
	"propose/support-window-pr":  "8222c9bdd8dece19debcf10efd43706aa8fa778df92a41712616253755c59c9f",
	"dispatch/dispatch-evidence": "c1bda73672b5f67bf582cb77d628acd30f201828d40c631deb7902ab435769ae",
}

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
	if err := verifyReleaseWorkflow(releaseWorkflowPath); err != nil {
		fatal(err)
	}
	if err := verifyDocumentation(docsPath, parsed); err != nil {
		fatal(err)
	}
	if err := verifyE2EWiring(e2eWiringFiles{
		makefile:         makefilePath,
		harness:          e2eHarnessPath,
		dataPlane:        e2eDataPlanePath,
		assertions:       e2eAssertPath,
		crdUpgrade:       e2eCRDUpgradePath,
		faults:           e2eFaultsPath,
		highAvailability: e2eHAPath,
		certRotation:     e2eCertRotationPath,
	}); err != nil {
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
		"E2E_PTAH_REVISION":      "fe26eb5af616b3b48aa75bf5cdb59ac9306b7836",
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
		"go run ./hack/verify-kubernetes-support.go -now \"$today\"",
		"git status --porcelain=v1 --untracked-files=all",
		"patch-base64",
		"patch-sha256",
		"git apply --check",
		"remote_base_sha",
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
		".isCrossRepository == false":            2,
		".headRepository.nameWithOwner == $repo": 2,
	} {
		if count := bytes.Count(contents, []byte(marker)); count != expected {
			return fmt.Errorf("%s: expected %d same-repository pull-request markers %q, found %d", path, expected, marker, count)
		}
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
	makefile         string
	harness          string
	dataPlane        string
	assertions       string
	crdUpgrade       string
	faults           string
	highAvailability string
	certRotation     string
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

func verifyE2EWiring(files e2eWiringFiles) error {
	if err := verifyMakeE2ETarget(files.makefile); err != nil {
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
		exactSourceLineSequence("kind cluster creation", []string{
			`kind create cluster \`,
			`--name "$CLUSTER_NAME" \`,
			`--image "$KIND_NODE_IMAGE" \`,
			`--config "$KIND_CONFIG" \`,
			`--kubeconfig "$KUBECONFIG_FILE" \`,
			`--wait 5m`,
		}),
		exactSourceLineSequence("API server version binding", []string{
			`server_version=$(kubectl --kubeconfig "$KUBECONFIG_FILE" version -o json |`,
			`jq -r '.serverVersion.gitVersion')`,
			`case "$server_version" in`,
			`v"$K8S_VERSION"*) ;;`,
			`*) fail "cluster reports $server_version, expected v$K8S_VERSION" ;;`,
			`esac`,
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
			`E2E_PHASE=uninstall \`,
			`"$ROOT_DIR/hack/e2e-crd-upgrade.sh"`,
		}),
		exactSourceLine("terminal Kubernetes lifecycle evidence", `printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"`),
	}
	if err := verifyOrderedSourceContract(harness, harnessContents, harnessContract); err != nil {
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
		dataPlaneContract[4].pattern,
		dataPlaneContract[9].pattern,
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulReturn(
		dataPlane,
		dataPlaneContents,
		dataPlaneContract[10].pattern,
		dataPlaneContract[13].pattern,
	); err != nil {
		return err
	}
	if err := rejectEarlySuccessfulExit(dataPlane, dataPlaneContents, dataPlaneContract[len(dataPlaneContract)-1].pattern); err != nil {
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
				exactSourceLine("cleanup implementation", `cleanup() {`),
				exactSourceLine("cleanup status capture", `status=$?`),
				exactSourceLine("cleanup status preservation", `exit "$status"`),
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
	return nil
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
	completionMatch := completion.FindIndex(contents)
	if completionMatch == nil {
		return fmt.Errorf("%s: terminal lifecycle evidence is missing", path)
	}
	prefix := contents[:completionMatch[1]]
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
		if match := check.pattern.FindIndex(prefix); match != nil {
			line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
			return fmt.Errorf("%s:%d: %s can bypass audited lifecycle work", path, line, check.name)
		}
	}
	return nil
}

func rejectEarlySuccessfulExit(path string, contents []byte, completion *regexp.Regexp) error {
	completionMatch := completion.FindIndex(contents)
	if completionMatch == nil {
		return fmt.Errorf("%s: terminal lifecycle evidence is missing", path)
	}
	earlySuccess := regexp.MustCompile(`(?m)^[ \t]*(?:exit(?:[ \t]+0)?|exec[ \t]+(?:(?:/usr)?/bin/)?true)[ \t]*(?:;[ \t]*)?(?:#[^\r\n]*)?\r?$`)
	if match := earlySuccess.FindIndex(contents[:completionMatch[0]]); match != nil {
		line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
		return fmt.Errorf("%s:%d: unconditional successful exit precedes terminal lifecycle evidence", path, line)
	}
	topLevelFailFastDisable := regexp.MustCompile(`(?m)^set[ \t]+(?:\+[^ \t;#\r\n]*[eu][^ \t;#\r\n]*|\+o[ \t]+(?:errexit|nounset))[ \t]*(?:;[^\r\n]*)?(?:#[^\r\n]*)?\r?$`)
	if match := topLevelFailFastDisable.FindIndex(contents[:completionMatch[0]]); match != nil {
		line := 1 + bytes.Count(contents[:match[0]], []byte{'\n'})
		return fmt.Errorf("%s:%d: top-level fail-fast mode is disabled before terminal lifecycle evidence", path, line)
	}
	return nil
}

func rejectEarlySuccessfulReturn(path string, contents []byte, start, completion *regexp.Regexp) error {
	startMatch := start.FindIndex(contents)
	if startMatch == nil {
		return fmt.Errorf("%s: lifecycle function boundaries are invalid", path)
	}
	completionMatch := completion.FindIndex(contents[startMatch[1]:])
	if completionMatch == nil {
		return fmt.Errorf("%s: lifecycle function boundaries are invalid", path)
	}
	completionStart := startMatch[1] + completionMatch[0]
	earlyReturn := regexp.MustCompile(`(?m)^[ \t]*return(?:[ \t]+0)?[ \t]*(?:;[ \t]*)?(?:#[^\r\n]*)?\r?$`)
	if match := earlyReturn.FindIndex(contents[startMatch[1]:completionStart]); match != nil {
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
