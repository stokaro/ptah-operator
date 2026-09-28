package e2e

import (
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The read-only Job fixture, the runtime Deployments and the init guard: the
// pure half of the section of hack/e2e-crd-upgrade.sh from
// dispatch_read_only_job_fixture to wait_runtime_ready. Every predicate reads
// a stored document the way the script's jq did: a field that is absent is
// null, `// default` supplies the default, and `has` over a value that is not
// an object is an error in jq, which failed the predicate it sat in.

const (
	// lifecycleRuntimeBlockedStability is BLOCKED_STABILITY_SECONDS: how long
	// the same Pods have to keep failing their explicit verifier before the
	// runtime counts as blocked.
	lifecycleRuntimeBlockedStability = 10 * time.Second
	// lifecycleRuntimeBlockedFailureTimeout is BLOCKED_FAILURE_TIMEOUT_SECONDS.
	lifecycleRuntimeBlockedFailureTimeout = 150 * time.Second

	lifecycleRuntimeTerminalReason  = "ReadOnlyJobProof"
	lifecycleRuntimeTerminalMessage = "terminal read-only Job retained across quiescence"
	// lifecycleRuntimeCleanupTTL is the ttlSecondsAfterFinished the successor
	// gives a quiesced read-only Job.
	lifecycleRuntimeCleanupTTL = 300

	lifecycleRuntimeAdmissionConfiguration = "ptah-operator-admission"
	lifecycleRuntimePodWebhook             = "vpodintent.operator.ptah.run"
	lifecycleRuntimeVerifierContainer      = "verify-candidate-runtime"
	lifecycleRuntimeRotatorContainer       = "certificate-rotator"
	lifecycleRuntimeBlockedNodeSelectorKey = "operator.ptah.run/read-only-job-proof"
)

// lifecycleRuntimeJSON encodes a document built here, which cannot fail.
func lifecycleRuntimeJSON(document any) []byte {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

// lifecycleRuntimeValue walks nested maps, as a jq path does. A path through
// something that is not an object reads as absent.
func lifecycleRuntimeValue(object any, path ...string) (any, bool) {
	value := object
	for _, key := range path {
		current, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		if value, ok = current[key]; !ok {
			return nil, false
		}
	}
	return value, value != nil
}

// lifecycleRuntimeString is a string field, "" when absent or not a string.
func lifecycleRuntimeString(object any, path ...string) string {
	value, _ := lifecycleRuntimeValue(object, path...)
	text, _ := value.(string)
	return text
}

// lifecycleRuntimeLacks is `(<path> | has(key) | not)`. jq's has on null is
// an error, which failed the whole predicate, so a container that is absent
// fails the check rather than lacking the key.
func lifecycleRuntimeLacks(object any, key string, path ...string) bool {
	container, found := lifecycleRuntimeValue(object, path...)
	if !found {
		return false
	}
	fields, ok := container.(map[string]any)
	if !ok {
		return false
	}
	_, has := fields[key]
	return !has
}

// lifecycleRuntimeNumber is `(<path> // 0)` for a count the API stores as an
// integer.
func lifecycleRuntimeNumber(object any, path ...string) int64 {
	value, found := lifecycleRuntimeValue(object, path...)
	if !found {
		return 0
	}
	switch number := value.(type) {
	case int64:
		return number
	case float64:
		return int64(number)
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	}
	return -1
}

// lifecycleRuntimeConditions is the conditions list at a path, and whether
// there is one: `(.status.conditions // [])` takes the empty list for
// absence, while a bare `.status.conditions | any(...)` fails on it.
func lifecycleRuntimeConditions(object any, path ...string) ([]map[string]any, bool) {
	value, found := lifecycleRuntimeValue(object, path...)
	if !found {
		return nil, false
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	conditions := make([]map[string]any, 0, len(items))
	for _, item := range items {
		condition, _ := item.(map[string]any)
		conditions = append(conditions, condition)
	}
	return conditions, true
}

func lifecycleRuntimeHasCondition(conditions []map[string]any, kind, reason, message string) bool {
	return slices.ContainsFunc(conditions, func(condition map[string]any) bool {
		return lifecycleRuntimeString(condition, "type") == kind &&
			lifecycleRuntimeString(condition, "status") == "True" &&
			(reason == "" || lifecycleRuntimeString(condition, "reason") == reason) &&
			(message == "" || lifecycleRuntimeString(condition, "message") == message)
	})
}

// lifecycleRuntimeReadOnlyJobDispatched is the identity the dispatched Job
// has to carry: the claim's UID, the schema's and the Resolve operation's
// labels, and no cleanup TTL yet.
func lifecycleRuntimeReadOnlyJobDispatched(job map[string]any, schema, uid string) bool {
	return lifecycleRuntimeString(job, "metadata", "uid") == uid &&
		lifecycleRuntimeString(job, "metadata", "labels", "operator.ptah.run/schema") == schema &&
		lifecycleRuntimeString(job, "metadata", "labels", "operator.ptah.run/operation") == "resolve" &&
		lifecycleRuntimeLacks(job, "ttlSecondsAfterFinished", "spec")
}

// lifecycleRuntimeSchemaQuiesced is a schema that is suspended, reports
// Suspended, and holds no operation.
func lifecycleRuntimeSchemaQuiesced(schema map[string]any) bool {
	suspend, _ := lifecycleRuntimeValue(schema, "spec", "suspend")
	_, active := lifecycleRuntimeValue(schema, "status", "activeOperation")
	return suspend == true && lifecycleRuntimeString(schema, "status", "phase") == "Suspended" && !active
}

// lifecycleRuntimePodWebhookIndex is the index of the one Pod intent webhook
// in the admission configuration.
func lifecycleRuntimePodWebhookIndex(configuration map[string]any) (int, error) {
	value, _ := lifecycleRuntimeValue(configuration, "webhooks")
	webhooks, _ := value.([]any)
	index := -1
	for position, webhook := range webhooks {
		if lifecycleRuntimeString(webhook, "name") != lifecycleRuntimePodWebhook {
			continue
		}
		if index >= 0 {
			return 0, errors.New("the admission configuration carries more than one Pod intent webhook")
		}
		index = position
	}
	if index < 0 {
		return 0, errors.New("the admission configuration carries no Pod intent webhook")
	}
	return index, nil
}

// lifecycleRuntimeWebhookAt is the name and failurePolicy of the webhook at
// an index.
func lifecycleRuntimeWebhookAt(configuration map[string]any, index int) (name, policy string) {
	value, _ := lifecycleRuntimeValue(configuration, "webhooks")
	webhooks, _ := value.([]any)
	if index < 0 || index >= len(webhooks) {
		return "", ""
	}
	return lifecycleRuntimeString(webhooks[index], "name"), lifecycleRuntimeString(webhooks[index], "failurePolicy")
}

// lifecycleRuntimeFailurePolicyTransition refuses every transition but the
// two the proofs make.
func lifecycleRuntimeFailurePolicyTransition(expected, desired string) error {
	switch expected + ":" + desired {
	case "Fail:Ignore", "Ignore:Fail":
		return nil
	}
	return errors.New("unsupported Pod webhook failurePolicy transition " + expected + " -> " + desired)
}

// lifecycleRuntimeFailurePolicyPatch tests that the webhook at the index is
// the Pod intent webhook with the expected policy before it replaces the
// policy, so a patch against a moved or already changed entry is refused by
// the API server.
func lifecycleRuntimeFailurePolicyPatch(index int, expected, desired string) []byte {
	base := "/webhooks/" + strconv.Itoa(index)
	return lifecycleRuntimeJSON([]map[string]any{
		{"op": "test", "path": base + "/name", "value": lifecycleRuntimePodWebhook},
		{"op": "test", "path": base + "/failurePolicy", "value": expected},
		{"op": "replace", "path": base + "/failurePolicy", "value": desired},
	})
}

// lifecycleRuntimeReadOnlyJobOpen is the Job before FailureTarget is staged:
// its UID, no terminal condition, and no completionTime.
func lifecycleRuntimeReadOnlyJobOpen(job map[string]any, uid string) bool {
	conditions, _ := lifecycleRuntimeConditions(job, "status", "conditions")
	for _, condition := range conditions {
		kind := lifecycleRuntimeString(condition, "type")
		if lifecycleRuntimeString(condition, "status") == "True" &&
			(kind == "Complete" || kind == "Failed" || kind == "FailureTarget") {
			return false
		}
	}
	return lifecycleRuntimeString(job, "metadata", "uid") == uid && lifecycleRuntimeLacks(job, "completionTime", "status")
}

// lifecycleRuntimeFailureTargetPatch is the status the harness stages: a
// FailureTarget condition the Job controller turns into Failed.
func lifecycleRuntimeFailureTargetPatch(at time.Time) []byte {
	stamp := at.UTC().Format("2006-01-02T15:04:05Z")
	return lifecycleRuntimeJSON(map[string]any{"status": map[string]any{"conditions": []any{map[string]any{
		"type": "FailureTarget", "status": "True",
		"reason": lifecycleRuntimeTerminalReason, "message": lifecycleRuntimeTerminalMessage,
		"lastProbeTime": stamp, "lastTransitionTime": stamp,
	}}}})
}

// lifecycleRuntimeReadOnlyJobRetired is the Job the Job controller retired:
// started, nothing active, ready, terminating or uncounted, no
// completionTime, both the staged FailureTarget and the Failed it became, and
// still no cleanup TTL.
func lifecycleRuntimeReadOnlyJobRetired(job map[string]any, uid string) bool {
	_, started := lifecycleRuntimeValue(job, "status", "startTime")
	conditions, _ := lifecycleRuntimeConditions(job, "status", "conditions")
	uncounted := func(kind string) int {
		value, _ := lifecycleRuntimeValue(job, "status", "uncountedTerminatedPods", kind)
		items, ok := value.([]any)
		if !ok && value != nil {
			return -1
		}
		return len(items)
	}
	return lifecycleRuntimeString(job, "metadata", "uid") == uid && started &&
		lifecycleRuntimeNumber(job, "status", "active") == 0 &&
		lifecycleRuntimeNumber(job, "status", "ready") == 0 &&
		lifecycleRuntimeNumber(job, "status", "terminating") == 0 &&
		uncounted("succeeded") == 0 && uncounted("failed") == 0 &&
		lifecycleRuntimeLacks(job, "completionTime", "status") &&
		lifecycleRuntimeHasCondition(conditions, "FailureTarget", lifecycleRuntimeTerminalReason, lifecycleRuntimeTerminalMessage) &&
		lifecycleRuntimeHasCondition(conditions, "Failed", lifecycleRuntimeTerminalReason, lifecycleRuntimeTerminalMessage) &&
		lifecycleRuntimeLacks(job, "ttlSecondsAfterFinished", "spec")
}

// lifecycleRuntimeJobCleanupEvidence is what a successor's cleanup may not
// change: the Job's identity, labels, annotations, owners, finalizers and
// spec, the cleanup TTL aside. The keys are written in order, as jq -S did.
func lifecycleRuntimeJobCleanupEvidence(job map[string]any) ([]byte, error) {
	metadata, _ := job["metadata"].(map[string]any)
	finalizers := metadataValue(metadata, "finalizers")
	if finalizers == nil {
		finalizers = []any{}
	}
	var spec any
	if fields, ok := job["spec"].(map[string]any); ok {
		copied := make(map[string]any, len(fields))
		for key, value := range fields {
			if key != "ttlSecondsAfterFinished" {
				copied[key] = value
			}
		}
		spec = copied
	}
	return json.Marshal(map[string]any{
		"uid": metadataValue(metadata, "uid"), "name": metadataValue(metadata, "name"),
		"namespace": metadataValue(metadata, "namespace"), "labels": metadataValue(metadata, "labels"),
		"annotations": metadataValue(metadata, "annotations"), "ownerReferences": metadataValue(metadata, "ownerReferences"),
		"finalizers": finalizers, "spec": spec,
	})
}

// lifecycleRuntimeCleanupScheduled is `.spec.ttlSecondsAfterFinished // 0`
// at the TTL the successor gives a quiesced read-only Job.
func lifecycleRuntimeCleanupScheduled(job map[string]any) bool {
	return lifecycleRuntimeNumber(job, "spec", "ttlSecondsAfterFinished") == lifecycleRuntimeCleanupTTL
}

// lifecycleRuntimeUIDGapStaged is the schema after its committed Job UID was
// removed: the same Job name, a Resolve, and no jobUID key at all.
func lifecycleRuntimeUIDGapStaged(schema map[string]any, jobName string) bool {
	return lifecycleRuntimeString(schema, "status", "activeOperation", "jobName") == jobName &&
		lifecycleRuntimeString(schema, "status", "activeOperation", "type") == "Resolve" &&
		lifecycleRuntimeLacks(schema, "jobUID", "status", "activeOperation")
}

// lifecycleRuntimeLateJobFailed is the read-only Job still the same object,
// failed, and without a cleanup TTL. Its conditions have to exist: the script
// read them without a default, and jq fails on iterating null.
func lifecycleRuntimeLateJobFailed(job map[string]any, uid string) bool {
	conditions, found := lifecycleRuntimeConditions(job, "status", "conditions")
	return found && lifecycleRuntimeString(job, "metadata", "uid") == uid &&
		lifecycleRuntimeHasCondition(conditions, "Failed", "", "") &&
		lifecycleRuntimeLacks(job, "ttlSecondsAfterFinished", "spec")
}

// lifecycleRuntimeRotatorArgument is the one value the certificate rotator
// is given for an argument prefix, such as --secret-name=, across every
// container of that name.
func lifecycleRuntimeRotatorArgument(deployment map[string]any, prefix string) (string, error) {
	value, _ := lifecycleRuntimeValue(deployment, "spec", "template", "spec", "containers")
	containers, _ := value.([]any)
	var names []string
	for _, container := range containers {
		if lifecycleRuntimeString(container, "name") != lifecycleRuntimeRotatorContainer {
			continue
		}
		arguments, _ := lifecycleRuntimeValue(container, "args")
		items, _ := arguments.([]any)
		for _, item := range items {
			argument, _ := item.(string)
			if name, ok := strings.CutPrefix(argument, prefix); ok {
				names = append(names, name)
			}
		}
	}
	if len(names) != 1 || names[0] == "" {
		return "", errors.New("certificate rotator must carry one nonempty " + prefix + " identity")
	}
	return names[0], nil
}

// lifecycleRuntimeImpersonationPod is the running controller Pod the manager's
// identity is read from: the first by name.
func lifecycleRuntimeImpersonationPod(pods []corev1.Pod) (corev1.Pod, error) {
	var running []corev1.Pod
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod)
		}
	}
	if len(running) == 0 {
		return corev1.Pod{}, errors.New("no running controller Pod")
	}
	sort.SliceStable(running, func(i, j int) bool { return running[i].Name < running[j].Name })
	return running[0], nil
}

// lifecycleRuntimeDeployment is the controller Deployment
// capture_controller_service_account_identity recorded.
type lifecycleRuntimeDeployment struct {
	name, uid, serviceAccount string
}

// lifecycleRuntimeControllerDeployment is the deployment half of
// capture_controller_service_account_identity: exactly one controller
// Deployment of the release, with a UID, a ServiceAccount, and exactly one
// manager container running the image given.
func lifecycleRuntimeControllerDeployment(deployments []map[string]any, release, image string) (lifecycleRuntimeDeployment, error) {
	var selected []map[string]any
	for _, deployment := range deployments {
		if lifecycleRuntimeString(deployment, "metadata", "labels", "app.kubernetes.io/instance") == release &&
			lifecycleRuntimeString(deployment, "metadata", "labels", "app.kubernetes.io/component") == "controller" {
			selected = append(selected, deployment)
		}
	}
	if len(selected) != 1 {
		return lifecycleRuntimeDeployment{}, errors.New("controller Deployment cardinality differs")
	}
	deployment := selected[0]
	uid := lifecycleRuntimeString(deployment, "metadata", "uid")
	account := lifecycleRuntimeString(deployment, "spec", "template", "spec", "serviceAccountName")
	value, _ := lifecycleRuntimeValue(deployment, "spec", "template", "spec", "containers")
	containers, _ := value.([]any)
	managers := 0
	for _, container := range containers {
		if lifecycleRuntimeString(container, "name") == "manager" && lifecycleRuntimeString(container, "image") == image {
			managers++
		}
	}
	if uid == "" || account == "" || managers != 1 {
		return lifecycleRuntimeDeployment{}, errors.New("the controller Deployment does not run exactly one manager container on the image")
	}
	return lifecycleRuntimeDeployment{
		name: lifecycleRuntimeString(deployment, "metadata", "name"), uid: uid, serviceAccount: account,
	}, nil
}

// lifecycleRuntimeLiveServiceAccount is the ServiceAccount half: the exact
// object the Deployment names, labeled for the release, with a UID, and not
// being deleted.
func lifecycleRuntimeLiveServiceAccount(account map[string]any, name, namespace, release string) (string, error) {
	_, deleting := lifecycleRuntimeValue(account, "metadata", "deletionTimestamp")
	uid := lifecycleRuntimeString(account, "metadata", "uid")
	if lifecycleRuntimeString(account, "apiVersion") != "v1" || lifecycleRuntimeString(account, "kind") != "ServiceAccount" ||
		lifecycleRuntimeString(account, "metadata", "name") != name ||
		lifecycleRuntimeString(account, "metadata", "namespace") != namespace ||
		lifecycleRuntimeString(account, "metadata", "labels", "app.kubernetes.io/instance") != release ||
		uid == "" || deleting {
		return "", errors.New("the ServiceAccount is not the release's live object")
	}
	return uid, nil
}

// lifecycleRuntimeSnapshot is snapshot_runtime_deployment over the live
// Deployment read with its managed fields: the one server-side apply manager
// that owns it, and the object with everything the API server assigns
// removed, ready to apply again as that manager.
func lifecycleRuntimeSnapshot(live map[string]any) (map[string]any, string, error) {
	metadata, _ := live["metadata"].(map[string]any)
	value := metadataValue(metadata, "managedFields")
	entries, _ := value.([]any)
	managers := map[string]bool{}
	for _, entry := range entries {
		if lifecycleRuntimeString(entry, "operation") == "Apply" {
			managers[lifecycleRuntimeString(entry, "manager")] = true
		}
	}
	if len(managers) != 1 {
		names := make([]string, 0, len(managers))
		for name := range managers {
			names = append(names, name)
		}
		sort.Strings(names)
		encoded, _ := json.Marshal(names)
		return nil, "", errors.New("no single server-side apply field manager to restore: " + string(encoded))
	}
	var manager string
	for name := range managers {
		manager = name
	}
	// A deep copy keeps the integers the API server returned as integers.
	snapshot := runtime.DeepCopyJSON(live)
	if fields, ok := snapshot["metadata"].(map[string]any); ok {
		for _, key := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
			delete(fields, key)
		}
		if annotations, ok := fields["annotations"].(map[string]any); ok {
			delete(annotations, "deployment.kubernetes.io/revision")
		}
	}
	delete(snapshot, "status")
	return snapshot, manager, nil
}

// lifecycleRuntimeGuardState is what hack/e2e-crd-init-guard.jq reported
// about the runtime Pods of the release.
type lifecycleRuntimeGuardState struct {
	PodCount                   int
	PodUIDs                    string
	MainContainersNeverStarted bool
	ExplicitVerifierFailures   bool
}

// lifecycleRuntimeGuard reads the release's runtime Pods in scope: controller
// alone, or controller and certificate rotation. No main container may ever
// have started, and the runtime is blocked when exactly the expected Pods each
// have an explicit verifier failure. A Pod with no container status yet is not
// a failure: the guard is satisfied only by a verifier that ran and refused.
func lifecycleRuntimeGuard(pods []corev1.Pod, release, scope string, expected int) lifecycleRuntimeGuardState {
	var selected []corev1.Pod
	for _, pod := range pods {
		if pod.Labels["app.kubernetes.io/instance"] != release {
			continue
		}
		component := pod.Labels["app.kubernetes.io/component"]
		if component == "controller" || (scope != "controller" && component == "certificate-rotation") {
			selected = append(selected, pod)
		}
	}
	uids := make([]string, 0, len(selected))
	neverStarted, verifierFailed := true, true
	for _, pod := range selected {
		uids = append(uids, string(pod.UID))
		if !lifecycleRuntimeMainNeverStarted(pod) {
			neverStarted = false
		}
		if !lifecycleRuntimeVerifierFailed(pod) {
			verifierFailed = false
		}
	}
	sort.Strings(uids)
	return lifecycleRuntimeGuardState{
		PodCount: len(selected), PodUIDs: strings.Join(uids, ","),
		MainContainersNeverStarted: neverStarted,
		ExplicitVerifierFailures:   len(selected) == expected && verifierFailed,
	}
}

func lifecycleRuntimeMainNeverStarted(pod corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		waiting := ""
		if status.State.Waiting != nil {
			waiting = status.State.Waiting.Reason
		}
		if status.RestartCount != 0 || (status.Started != nil && *status.Started) ||
			status.State.Running != nil || status.State.Terminated != nil ||
			status.LastTerminationState.Running != nil || status.LastTerminationState.Terminated != nil ||
			waiting != "PodInitializing" {
			return false
		}
	}
	return true
}

func lifecycleRuntimeVerifierFailed(pod corev1.Pod) bool {
	return slices.ContainsFunc(pod.Status.InitContainerStatuses, func(status corev1.ContainerStatus) bool {
		return status.Name == lifecycleRuntimeVerifierContainer &&
			((status.State.Terminated != nil && status.State.Terminated.ExitCode != 0) ||
				(status.LastTerminationState.Terminated != nil && status.LastTerminationState.Terminated.ExitCode != 0))
	})
}

// lifecycleRuntimeReleaseInventory is the kind the uninstall inventory walks,
// by the resource name the script passed to kubectl get.
type lifecycleRuntimeReleaseInventory struct {
	resource, apiVersion, kind string
}

// lifecycleRuntimeInventoryNamed is the namespaced inventory entry for a
// resource. A name the inventory does not carry is a programming error.
func lifecycleRuntimeInventoryNamed(resource string) lifecycleRuntimeReleaseInventory {
	for _, entry := range lifecycleRuntimeNamespacedInventory {
		if entry.resource == resource {
			return entry
		}
	}
	panic("lifecycle runtime inventory has no " + resource)
}

// lifecycleRuntimeClusterInventory and lifecycleRuntimeNamespacedInventory
// are what an uninstall has to leave nothing labeled for the release in, in
// the script's order.
var (
	lifecycleRuntimeClusterInventory = []lifecycleRuntimeReleaseInventory{
		{"validatingadmissionpolicy", "admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyList"},
		{"validatingadmissionpolicybinding", "admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBindingList"},
		{"mutatingwebhookconfiguration", "admissionregistration.k8s.io/v1", "MutatingWebhookConfigurationList"},
		{"validatingwebhookconfiguration", "admissionregistration.k8s.io/v1", "ValidatingWebhookConfigurationList"},
		{"clusterrole", "rbac.authorization.k8s.io/v1", "ClusterRoleList"},
		{"clusterrolebinding", "rbac.authorization.k8s.io/v1", "ClusterRoleBindingList"},
	}
	lifecycleRuntimeNamespacedInventory = []lifecycleRuntimeReleaseInventory{
		{"deployment", "apps/v1", "DeploymentList"},
		{"replicaset", "apps/v1", "ReplicaSetList"},
		{"service", "v1", "ServiceList"},
		{"secret", "v1", "SecretList"},
		{"serviceaccount", "v1", "ServiceAccountList"},
		{"role", "rbac.authorization.k8s.io/v1", "RoleList"},
		{"rolebinding", "rbac.authorization.k8s.io/v1", "RoleBindingList"},
		{"job", "batch/v1", "JobList"},
		{"pod", "v1", "PodList"},
		{"configmap", "v1", "ConfigMapList"},
		{"lease", "coordination.k8s.io/v1", "LeaseList"},
		{"poddisruptionbudget", "policy/v1", "PodDisruptionBudgetList"},
	}
)

// lifecycleRuntimeInitStatusSummary is the diagnostic wait_runtime_ready
// printed for a runtime that did not become ready: the first ten Pods and the
// first eight init containers of each, with the null a missing field read as.
func lifecycleRuntimeInitStatusSummary(pods []corev1.Pod) []map[string]any {
	summaries := []map[string]any{}
	for index, pod := range pods {
		if index == 10 {
			break
		}
		initStatuses := []map[string]any{}
		for position, status := range pod.Status.InitContainerStatuses {
			if position == 8 {
				break
			}
			summary := map[string]any{"name": status.Name, "waitingReason": nil, "terminatedReason": nil, "exitCode": nil}
			if status.State.Waiting != nil {
				summary["waitingReason"] = status.State.Waiting.Reason
			}
			if status.State.Terminated != nil {
				summary["terminatedReason"] = status.State.Terminated.Reason
				summary["exitCode"] = status.State.Terminated.ExitCode
			}
			initStatuses = append(initStatuses, summary)
		}
		summaries = append(summaries, map[string]any{"pod": pod.Name, "init": initStatuses})
	}
	return summaries
}
