package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// The refused upgrades, the late failure and the rollbacks read Helm's own
// status documents, which the scripts held with jq. The documents are decoded
// untyped, numbers kept as written, and read with jq's rules: an absent field
// is null, `a // b` takes b for null, false or an error in a, and indexing
// anything but an object or null is an error that fails the whole check, as
// it made `jq -e` exit non-zero.

const (
	lifecycleFailureBlockerWebhook = "ptah-operator-e2e-late-failure-blocker"
	lifecycleFailureBlockerName    = "late-failure-blocker.operator.ptah.run"
	lifecycleFailureSharedProbe    = "ptah-e2e-shared-namespace-probe"
)

// lifecycleFailureDecode reads a Helm JSON document with its numbers as
// written.
func lifecycleFailureDecode(document []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// lifecycleFailureIndex is jq's `.key`: null for null, the field of an object
// (null when absent), and an error for anything else.
func lifecycleFailureIndex(value any, key string) (any, error) {
	switch object := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return object[key], nil
	}
	return nil, fmt.Errorf("cannot index %T with %q", value, key)
}

// lifecycleFailurePath indexes a chain of keys.
func lifecycleFailurePath(value any, keys ...string) (any, error) {
	for _, key := range keys {
		var err error
		if value, err = lifecycleFailureIndex(value, key); err != nil {
			return nil, err
		}
	}
	return value, nil
}

// lifecycleFailureOr is jq's `a // fallback` over a path: the fallback when
// the path is null, false, or could not be read.
func lifecycleFailureOr(value any, keys []string, fallback any) any {
	found, err := lifecycleFailurePath(value, keys...)
	if err != nil || found == nil || found == false {
		return fallback
	}
	return found
}

// lifecycleFailureNumber is a JSON number as a float, and whether the value
// was one: jq compares numbers by value and nothing else to a number.
func lifecycleFailureNumber(value any) (float64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	return parsed, err == nil
}

// lifecycleFailureEqualsNumber is jq's `value == n`.
func lifecycleFailureEqualsNumber(value any, n int) bool {
	number, ok := lifecycleFailureNumber(value)
	return ok && number == float64(n)
}

// lifecycleFailureElements is jq's `.[]`: the elements of an array, the values
// of an object, nothing for null, and an error for anything else.
func lifecycleFailureElements(value any) ([]any, error) {
	switch collection := value.(type) {
	case []any:
		return collection, nil
	case map[string]any:
		values := make([]any, 0, len(collection))
		for _, key := range sortedFailureKeys(collection) {
			values = append(values, collection[key])
		}
		return values, nil
	}
	return nil, fmt.Errorf("cannot iterate over %T", value)
}

func sortedFailureKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	// jq iterates an object's values in key order.
	sort.Strings(keys)
	return keys
}

// lifecycleFailureHasEvent is `(.events // []) | index("pre-upgrade") != null`:
// an element equal to the event for an array, a substring for a string, and an
// error for anything else.
func lifecycleFailureHasEvent(hook any, event string) (bool, error) {
	switch events := lifecycleFailureOr(hook, []string{"events"}, []any{}).(type) {
	case []any:
		for _, element := range events {
			if name, ok := element.(string); ok && name == event {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(events, event), nil
	default:
		return false, fmt.Errorf("cannot index %T with %q", events, event)
	}
}

// lifecycleFailureHookPhase is `.last_run.phase // ""`.
func lifecycleFailureHookPhase(hook any) any {
	return lifecycleFailureOr(hook, []string{"last_run", "phase"}, "")
}

// lifecycleFailureHookWeight is `if .weight == null then 0 else (.weight |
// tonumber) end`: null counts as zero, a number is itself, a string is parsed
// as one, and anything else, or a string that is not a number, is an error.
func lifecycleFailureHookWeight(hook any) (float64, error) {
	weight, err := lifecycleFailureIndex(hook, "weight")
	if err != nil {
		return 0, err
	}
	switch value := weight.(type) {
	case nil:
		return 0, nil
	case json.Number:
		number, ok := lifecycleFailureNumber(value)
		if !ok {
			return 0, fmt.Errorf("weight %s is not a number", value)
		}
		return number, nil
	case string:
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, fmt.Errorf("weight %q cannot be parsed as a number", value)
		}
		return number, nil
	}
	return 0, fmt.Errorf("weight %T cannot be parsed as a number", weight)
}

// lifecycleFailureLength is jq's `length` over the value `// ""` left: the
// characters of a string, the magnitude of a number, the size of an array or
// an object, and an error for a boolean.
func lifecycleFailureLength(value any) (float64, error) {
	switch typed := value.(type) {
	case string:
		return float64(len([]rune(typed))), nil
	case json.Number:
		number, _ := lifecycleFailureNumber(typed)
		return math.Abs(number), nil
	case []any:
		return float64(len(typed)), nil
	case map[string]any:
		return float64(len(typed)), nil
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("%T has no length", value)
}

// lifecycleFailedHookEvidence is hack/failed-hook-evidence.jq: the Helm status
// of the revision a refused upgrade left proves the refusal was the reconcile
// hook of that revision and nothing after it. The revision is the one given
// and it failed; exactly one hook failed, and it is the named Job at weight 0,
// a pre-upgrade hook that started and completed; and no pre-upgrade hook
// weighted after it ran. The error says which clause failed.
func lifecycleFailedHookEvidence(status []byte, expectedRevision int, expectedName string) error {
	document, err := lifecycleFailureDecode(status)
	if err != nil {
		return fmt.Errorf("the status is not JSON: %w", err)
	}
	hooks, err := lifecycleFailureElements(lifecycleFailureOr(document, []string{"hooks"}, []any{}))
	if err != nil {
		return err
	}
	var failed []any
	for _, hook := range hooks {
		if phase, ok := lifecycleFailureHookPhase(hook).(string); ok && phase == "Failed" {
			failed = append(failed, hook)
		}
	}
	version, err := lifecycleFailureIndex(document, "version")
	if err != nil {
		return err
	}
	if !lifecycleFailureEqualsNumber(version, expectedRevision) {
		return fmt.Errorf("the status is of revision %v, not %d", version, expectedRevision)
	}
	releaseStatus, err := lifecycleFailurePath(document, "info", "status")
	if err != nil {
		return err
	}
	if releaseStatus != "failed" {
		return fmt.Errorf("the release is %v, not failed", releaseStatus)
	}
	if len(failed) != 1 {
		return fmt.Errorf("%d hooks failed, not exactly one", len(failed))
	}
	if err := lifecycleFailureRefusingHook(failed[0], expectedName); err != nil {
		return err
	}
	for _, hook := range hooks {
		preUpgrade, err := lifecycleFailureHasEvent(hook, "pre-upgrade")
		if err != nil {
			return err
		}
		if !preUpgrade {
			continue
		}
		weight, err := lifecycleFailureHookWeight(hook)
		if err != nil {
			return err
		}
		if weight > 0 && lifecycleFailureHookPhase(hook) != "" {
			return fmt.Errorf("a pre-upgrade hook weighted %v ran after the refusal", weight)
		}
	}
	return nil
}

// lifecycleFailureRefusingHook is the clause the one failed hook is held to.
func lifecycleFailureRefusingHook(hook any, expectedName string) error {
	name, err := lifecycleFailureIndex(hook, "name")
	if err != nil {
		return err
	}
	if name != expectedName {
		return fmt.Errorf("the failed hook is %v, not %s", name, expectedName)
	}
	kind, err := lifecycleFailureIndex(hook, "kind")
	if err != nil {
		return err
	}
	if kind != "Job" {
		return fmt.Errorf("the failed hook is a %v, not a Job", kind)
	}
	weight, err := lifecycleFailureHookWeight(hook)
	if err != nil {
		return err
	}
	if weight != 0 {
		return fmt.Errorf("the failed hook has weight %v, not 0", weight)
	}
	preUpgrade, err := lifecycleFailureHasEvent(hook, "pre-upgrade")
	if err != nil {
		return err
	}
	if !preUpgrade {
		return errors.New("the failed hook is not a pre-upgrade hook")
	}
	for _, instant := range []string{"started_at", "completed_at"} {
		length, err := lifecycleFailureLength(lifecycleFailureOr(hook, []string{"last_run", instant}, ""))
		if err != nil {
			return err
		}
		if length <= 0 {
			return fmt.Errorf("the failed hook has no %s", instant)
		}
	}
	return nil
}

// lifecycleLateFailureEvidence is the classification the late failure is
// held to: the release of the revision given failed, no hook failed, and the
// one reconcile hook Job of that name ran as a pre-upgrade hook and succeeded.
// A failure after the hook, not the hook's own refusal, is what failed the
// release.
func lifecycleLateFailureEvidence(status []byte, expectedRevision int, expectedReconcileName string) error {
	document, err := lifecycleFailureDecode(status)
	if err != nil {
		return fmt.Errorf("the status is not JSON: %w", err)
	}
	hooks, ok := lifecycleFailureOr(document, []string{"hooks"}, []any{}).([]any)
	if !ok {
		hooks = []any{}
	}
	var reconcile []any
	for _, hook := range hooks {
		name, err := lifecycleFailureIndex(hook, "name")
		if err != nil {
			return err
		}
		if name != expectedReconcileName {
			continue
		}
		kind, err := lifecycleFailureIndex(hook, "kind")
		if err != nil {
			return err
		}
		if kind == "Job" {
			reconcile = append(reconcile, hook)
		}
	}
	version, err := lifecycleFailureIndex(document, "version")
	if err != nil {
		return err
	}
	if !lifecycleFailureEqualsNumber(version, expectedRevision) {
		return fmt.Errorf("the status is of revision %v, not %d", version, expectedRevision)
	}
	releaseStatus, err := lifecycleFailurePath(document, "info", "status")
	if err != nil {
		return err
	}
	if releaseStatus != "failed" {
		return fmt.Errorf("the release is %v, not failed", releaseStatus)
	}
	for _, hook := range hooks {
		phase, err := lifecycleFailurePath(hook, "last_run", "phase")
		if err != nil {
			return err
		}
		if phase == "Failed" {
			name, _ := lifecycleFailureIndex(hook, "name")
			return fmt.Errorf("hook %v failed", name)
		}
	}
	if len(reconcile) != 1 {
		return fmt.Errorf("%d reconcile hook Jobs named %s, not exactly one", len(reconcile), expectedReconcileName)
	}
	phase, err := lifecycleFailurePath(reconcile[0], "last_run", "phase")
	if err != nil {
		return err
	}
	if phase != "Succeeded" {
		return fmt.Errorf("the reconcile hook is %v, not Succeeded", phase)
	}
	preUpgrade, err := lifecycleFailureHasEvent(reconcile[0], "pre-upgrade")
	if err != nil {
		return err
	}
	if !preUpgrade {
		return errors.New("the reconcile hook is not a pre-upgrade hook")
	}
	return nil
}

// lifecycleFailureStatusSummary is what the scripts printed when a status did
// not hold: `{version, status: .info.status, description: .info.description,
// hooks: [(.hooks // [])[] | {name, kind, weight, events, last_run}]}`. It
// is diagnostics, so a document it cannot read is shown as it came.
func lifecycleFailureStatusSummary(status []byte) string {
	document, err := lifecycleFailureDecode(status)
	if err != nil {
		return strings.TrimSpace(string(status))
	}
	field := func(value any, keys ...string) any {
		found, _ := lifecycleFailurePath(value, keys...)
		return found
	}
	hooks, _ := lifecycleFailureElements(lifecycleFailureOr(document, []string{"hooks"}, []any{}))
	summaries := make([]any, 0, len(hooks))
	for _, hook := range hooks {
		summary := map[string]any{}
		for _, key := range []string{"name", "kind", "weight", "events", "last_run"} {
			summary[key] = field(hook, key)
		}
		summaries = append(summaries, summary)
	}
	encoded, err := json.Marshal(map[string]any{
		"version": field(document, "version"), "status": field(document, "info", "status"),
		"description": field(document, "info", "description"), "hooks": summaries,
	})
	if err != nil {
		return strings.TrimSpace(string(status))
	}
	return string(encoded)
}

// lifecycleFailureHistoryRevision is `.[0].revision | select(type ==
// "number" and . >= 1)` over `helm history --max 1 -o json`.
func lifecycleFailureHistoryRevision(history []byte) (int, error) {
	first, _, err := lifecycleFailureHistoryFirst(history)
	if err != nil {
		return 0, err
	}
	number, ok := lifecycleFailureNumber(first)
	if !ok || number < 1 || number != math.Trunc(number) {
		return 0, fmt.Errorf("the newest history entry has revision %v, not a number of at least 1", first)
	}
	return int(number), nil
}

// lifecycleFailureRefusedRollbackHistory is what a rollback refused in its
// pre-rollback hook leaves: Helm wrote the rollback revision, one past the
// newest before it, and never deployed or superseded it. A refusal before the
// hook writes no revision at all.
func lifecycleFailureRefusedRollbackHistory(history []byte, before int) error {
	revision, status, err := lifecycleFailureHistoryFirst(history)
	if err != nil {
		return err
	}
	if !lifecycleFailureEqualsNumber(revision, before+1) {
		return fmt.Errorf("the newest revision is %v, not %d", revision, before+1)
	}
	if status == "deployed" || status == "superseded" {
		return fmt.Errorf("the rollback revision is %v", status)
	}
	return nil
}

func lifecycleFailureHistoryFirst(history []byte) (revision, status any, err error) {
	document, err := lifecycleFailureDecode(history)
	if err != nil {
		return nil, nil, fmt.Errorf("the history is not JSON: %w", err)
	}
	entries, ok := document.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("the history is a %T, not a list", document)
	}
	var first any
	if len(entries) > 0 {
		first = entries[0]
	}
	if revision, err = lifecycleFailureIndex(first, "revision"); err != nil {
		return nil, nil, err
	}
	if status, err = lifecycleFailureIndex(first, "status"); err != nil {
		return nil, nil, err
	}
	return revision, status, nil
}

// lifecycleFailureDeploymentOn is the check on a runtime Deployment after the
// late failure (stopped: `.spec.replicas == 0`) or after the rollback
// (running: `.spec.replicas >= 1`), each with every container on the image
// given. An absent replica count is jq's null, which is neither.
func lifecycleFailureDeploymentOn(deployment *appsv1.Deployment, image string, stopped bool) bool {
	replicas := deployment.Spec.Replicas
	if replicas == nil {
		return false
	}
	if stopped && *replicas != 0 || !stopped && *replicas < 1 {
		return false
	}
	// all() over the containers of a Deployment that lists none was an error
	// in jq, which failed the check.
	if deployment.Spec.Template.Spec.Containers == nil {
		return false
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Image != image {
			return false
		}
	}
	return true
}

// lifecycleFailureRuntimePodsGone is `all(.items[]; .spec.serviceAccountName
// != $controller and .spec.serviceAccountName != $certificate)`: no Pod in the
// release namespace runs as either runtime account.
func lifecycleFailureRuntimePodsGone(pods []corev1.Pod, controllerAccount, certificateAccount string) bool {
	for _, pod := range pods {
		if pod.Spec.ServiceAccountName == controllerAccount || pod.Spec.ServiceAccountName == certificateAccount {
			return false
		}
	}
	return true
}

// lifecycleFailureCandidateUnchanged is assert_late_failure_candidate_unchanged
// over the checksums and image recorded before the late failure and read
// again before the retry.
func lifecycleFailureCandidateUnchanged(before, after lifecycleFailureCandidate) bool {
	return before == after
}

// lifecycleFailureCandidate is what the retry has to be the same as.
type lifecycleFailureCandidate struct {
	chartSHA256, valuesSHA256, image string
}

// lifecycleFailureReadCandidate checksums the candidate's chart and values and
// records its image. A file that cannot be read is a refusal, as a failed
// checksum was: a retry nobody could compare is not a retry of the same thing.
func lifecycleFailureReadCandidate(chartPath, valuesPath, image string) (lifecycleFailureCandidate, error) {
	chart, err := os.ReadFile(chartPath) //nolint:gosec // A path the driver named.
	if err != nil {
		return lifecycleFailureCandidate{}, errors.New("could not checksum the late-failure candidate chart")
	}
	values, err := os.ReadFile(valuesPath) //nolint:gosec // A path the driver named.
	if err != nil {
		return lifecycleFailureCandidate{}, errors.New("could not checksum the late-failure candidate values")
	}
	return lifecycleFailureCandidate{
		chartSHA256:  strings.TrimPrefix(sha256Digest(chart), "sha256:"),
		valuesSHA256: strings.TrimPrefix(sha256Digest(values), "sha256:"),
		image:        image,
	}, nil
}

// lifecycleFailureBlockerConditions are the blocker's two match conditions: a
// write of one of this release's two runtime Deployments, carrying the
// candidate image. The hook's scale-down of the predecessor passes both, and
// Helm's apply of the candidate is refused.
func lifecycleFailureBlockerConditions(namespace, controller, rotator, candidateImage string) []map[string]any {
	return []map[string]any{
		{
			"name": "exact-runtime-deployment",
			"expression": `request.namespace == "` + namespace + `" && (request.name == "` + controller +
				`" || request.name == "` + rotator + `")`,
		},
		{
			"name":       "candidate-image",
			"expression": `object != null && object.spec.template.spec.containers.exists(container, container.image == "` + candidateImage + `")`,
		},
	}
}

// lifecycleFailureBlocker is the late-failure blocker: a webhook with no
// backend, so a write it matches is refused, that matches only a write of this
// release's two Deployments carrying the candidate image.
func lifecycleFailureBlocker(namespace, controller, rotator, candidateImage string) map[string]any {
	conditions := lifecycleFailureBlockerConditions(namespace, controller, rotator, candidateImage)
	matchConditions := make([]any, 0, len(conditions))
	for _, condition := range conditions {
		matchConditions = append(matchConditions, condition)
	}
	return map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
		"metadata": map[string]any{"name": lifecycleFailureBlockerWebhook},
		"webhooks": []any{map[string]any{
			"name":                    lifecycleFailureBlockerName,
			"admissionReviewVersions": []any{"v1"},
			"clientConfig": map[string]any{"service": map[string]any{
				"name": "ptah-operator-e2e-missing-blocker", "namespace": namespace, "path": "/deny", "port": 443,
			}},
			"failurePolicy":   "Fail",
			"matchPolicy":     "Exact",
			"timeoutSeconds":  2,
			"sideEffects":     "None",
			"matchConditions": matchConditions,
			"rules": []any{map[string]any{
				"apiGroups": []any{"apps"}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE", "UPDATE"},
				"resources": []any{"deployments"}, "scope": "Namespaced",
			}},
		}},
	}
}

// lifecycleFailureSharedNamespaceProbe is the foreign workload the shared
// release namespace proof puts in the release namespace: a suspended CronJob,
// a workload controller that starts no Pod and that no admission policy of the
// chart matches, so the refusal can only be the chart's.
func lifecycleFailureSharedNamespaceProbe() map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "CronJob",
		"metadata": map[string]any{"name": lifecycleFailureSharedProbe},
		"spec": map[string]any{
			"suspend":  true,
			"schedule": "0 0 1 1 *",
			"jobTemplate": map[string]any{"spec": map[string]any{
				"backoffLimit": 0,
				"template": map[string]any{"spec": map[string]any{
					"restartPolicy":                "Never",
					"automountServiceAccountToken": false,
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": 65532,
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name": "probe", "image": "registry.k8s.io/pause:3.10",
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false,
							"capabilities":             map[string]any{"drop": []any{"ALL"}},
						},
					}},
				}},
			}},
		},
	}
}

// lifecycleFailureSharedNamespaceRefusal is the refusal the chart has to name
// the foreign workload in.
func lifecycleFailureSharedNamespaceRefusal(namespace, release, probe string) string {
	return "release namespace " + namespace + " runs workloads without app.kubernetes.io/instance=" +
		release + " (CronJob/" + probe + ")"
}
