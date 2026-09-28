package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// rewriteMySQLRefusalJob turns a completed MySQL Observe or Plan Job into one
// the phase runs itself against a Secret whose URL carries a server-session
// payload: the same Pod template, with the database URL read from that
// Secret, the operation ID a literal, and labels that keep the operator's
// selectors and webhooks off it. Everything else in the template is the
// operator's, so the refusal the row reads is the runner's own.
//
// It works on the Job as the API returned it, because it has to keep an
// absent, a null and an empty initContainers apart, and an absent or null env
// on a helper container, exactly as the source had them.
func rewriteMySQLRefusalJob(source map[string]any, namespace, name, schema, operation, operationID, secret string) (map[string]any, error) {
	spec, ok := runtime.DeepCopyJSONValue(source["spec"]).(map[string]any)
	if !ok {
		return nil, errors.New("the source Job has no spec object")
	}
	delete(spec, "selector")
	delete(spec, "manualSelector")
	template, ok := spec["template"].(map[string]any)
	if !ok {
		return nil, errors.New("the source Job has no Pod template")
	}
	labels := map[string]any{
		"app.kubernetes.io/component": "e2e-invalid-dsn",
		labelSchema:                   schema,
		labelOperation:                operation,
	}
	annotations := map[string]any{annotationOperationID: operationID}
	template["metadata"] = map[string]any{
		"labels": runtime.DeepCopyJSONValue(labels), "annotations": runtime.DeepCopyJSONValue(annotations),
	}
	podSpec, ok := template["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("the source Job's template has no Pod spec")
	}
	containers, ok := podSpec["containers"].([]any)
	if !ok {
		return nil, errors.New("the source Job's template has no container list")
	}
	rewritten, err := rewriteRefusalContainers(containers, operationID, secret)
	if err != nil {
		return nil, err
	}
	podSpec["containers"] = rewritten
	if initContainers, present := podSpec["initContainers"]; present && initContainers != nil {
		list, ok := initContainers.([]any)
		if !ok {
			return nil, errors.New("the source Job's initContainers is not a list")
		}
		if podSpec["initContainers"], err = rewriteRefusalContainers(list, operationID, secret); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{
			"namespace": namespace, "name": name, "labels": labels, "annotations": annotations,
		},
		"spec": spec,
	}, nil
}

// rewriteRefusalContainers points each container's database URL at the
// Secret and fixes its operation ID. A container whose env is absent, null or
// not a list keeps it as it is.
func rewriteRefusalContainers(containers []any, operationID, secret string) ([]any, error) {
	for _, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("a source container is not an object")
		}
		env, ok := container["env"].([]any)
		if !ok {
			continue
		}
		for _, item := range env {
			if item == nil {
				continue
			}
			variable, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("a source environment variable is not an object")
			}
			switch variable["name"] {
			case "PTAH_DB_URL":
				delete(variable, "value")
				variable["valueFrom"] = map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": "url"}}
			case "PTAH_OPERATION_ID":
				variable["value"] = operationID
				delete(variable, "valueFrom")
			}
		}
	}
	return containers, nil
}

// unsafeMySQLDSN is the MySQL URL with a query that would run a second
// statement in the server session if the runner passed it on: a URL the
// runner must refuse before it dispatches anything.
func unsafeMySQLDSN(url string) string {
	return url + "?multiStatements=true&sql_mode=%27%27%3BDROP%20TABLE%20e2e_widgets"
}

// latestCompletedJob is the Job of the list created last among those that
// completed: the source a refusal row copies. Jobs created in the same second
// keep the order the list gave them, as jq's stable sort did.
func latestCompletedJob(jobs []unstructured.Unstructured) (*unstructured.Unstructured, error) {
	var completed []unstructured.Unstructured
	for _, job := range jobs {
		conditions, _, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
		if slices.ContainsFunc(conditions, func(entry any) bool {
			condition, _ := entry.(map[string]any)
			return condition["type"] == "Complete" && condition["status"] == "True"
		}) {
			completed = append(completed, job)
		}
	}
	if len(completed) == 0 {
		return nil, errors.New("no completed source Job")
	}
	slices.SortStableFunc(completed, func(a, b unstructured.Unstructured) int {
		created := func(job unstructured.Unstructured) string {
			value, _, _ := unstructured.NestedString(job.Object, "metadata", "creationTimestamp")
			return value
		}
		return strings.Compare(created(a), created(b))
	})
	return &completed[len(completed)-1], nil
}

// invalidTargetRefusal holds the result of a Job run against the unsafe URL
// to the runner refusing the target before any executor work: the operation
// and ID the Job carried, invalid_target, no output, no plan, nothing started
// and nothing uncertain, whole.
func invalidTargetRefusal(result runner.Result, protocolVersion int64, operation, operationID string) error {
	switch {
	case int64(result.ProtocolVersion) != protocolVersion || string(result.Operation) != operation ||
		result.OperationID != operationID:
		return errors.New("the result belongs to another operation")
	case result.Error == nil || result.Error.Code != "invalid_target":
		return errors.New("the runner did not refuse the target as invalid_target")
	case result.Stdout != "" || result.PlanContentDigest != "" || result.PlanOutcome != "":
		return errors.New("the result carries executor output")
	case result.MutationStarted || result.Uncertain:
		return errors.New("the result reports work that started")
	case result.Truncation != nil:
		return errors.New("the result was truncated")
	}
	return nil
}

var sessionPayload = regexp.MustCompile(`(?i)DROP[[:space:]]+TABLE|%3B|side_effecting_function`)

// disclosesSessionPayload reports a runner transport that repeats the
// server-session payload the unsafe URL carried, read line by line as grep
// read it.
func disclosesSessionPayload(transport []byte) bool {
	for line := range bytes.SplitSeq(transport, []byte("\n")) {
		if sessionPayload.Match(line) {
			return true
		}
	}
	return false
}

// preChildAccessRefusal holds a Resolve result to the runner refusing the
// registry grant before it started the child: exit -1, invalid_oci_access, no
// output, nothing resolved, nothing started and nothing uncertain, whole.
func preChildAccessRefusal(result runner.Result) error {
	switch {
	case result.ChildExitCode != -1 || result.Stdout != "":
		return errors.New("the runner started the child")
	case result.Error == nil || result.Error.Code != "invalid_oci_access":
		return errors.New("the runner did not refuse the registry grant as invalid_oci_access")
	case result.ResolvedDigest != "" || result.ResolvedReference != "":
		return errors.New("the result resolved the artifact")
	case result.MutationStarted || result.Uncertain:
		return errors.New("the result reports work that started")
	case result.Truncation != nil:
		return errors.New("the result was truncated")
	}
	return nil
}

// failedBeforeResolveChild is a schema that failed for an OCI access refusal
// and persisted when it tries again.
func failedBeforeResolveChild(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseFailed && status.NextReconciliationTime != nil &&
		slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == "ReconciliationFailed" && condition.Status == metav1.ConditionTrue &&
				condition.Reason == "OperationFailed" && strings.HasPrefix(condition.Message, "invalid_oci_access:")
		})
}

// failedForCurrentSpec is a schema whose status reports Failed for the spec
// it holds now. Right after a spec edit the status still describes the spec
// before it: a schema that had failed reads Failed until the controller
// observes the edit, and that reading is no verdict on the new spec. A wait
// that ended on it would fail on the poll that came too soon, which is what the
// suspension after a refused Resolve did once the poll no longer paid for a
// kubectl process.
func failedForCurrentSpec(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.Phase == ptahv1alpha1.PhaseFailed && schema.Status.ObservedGeneration == schema.Generation
}

var decimalCount = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// tlsProxyCounter reads the request count the proxy serves on its admin
// port. Trailing newlines are the response's framing, as a shell command
// substitution strips them; anything else that is not a decimal count is
// refused.
func tlsProxyCounter(body []byte) (int64, error) {
	text := strings.TrimRight(string(body), "\n")
	if !decimalCount.MatchString(text) {
		return 0, errors.New("TLS proxy request counter returned a non-integer response")
	}
	return strconv.ParseInt(text, 10, 64)
}

// proxyPodIdentity is the one proxy Pod whose counter a row reads, and the
// container that counts.
type proxyPodIdentity struct {
	name, uid, podIP, containerID string
}

const tlsProxyContainer = "tls-registry-proxy"

func liveProxyPods(pods []corev1.Pod) []corev1.Pod {
	var live []corev1.Pod
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil {
			live = append(live, pod)
		}
	}
	return live
}

func podReadyTrue(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Status.Conditions, func(condition corev1.PodCondition) bool {
		return condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
	})
}

// tlsProxyPodIdentity is the identity of the one live proxy Pod: running,
// ready, with an address, and its proxy container ready, running, never
// restarted and bound to a container ID. Anything else is refused, so a
// counter read later can be tied to the process that counted.
func tlsProxyPodIdentity(pods []corev1.Pod) (proxyPodIdentity, error) {
	live := liveProxyPods(pods)
	if len(live) != 1 {
		return proxyPodIdentity{}, fmt.Errorf("%d live proxy Pods rather than one", len(live))
	}
	pod := &live[0]
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || !podReadyTrue(pod) {
		return proxyPodIdentity{}, errors.New("the proxy Pod is not running and ready with an address")
	}
	var statuses []corev1.ContainerStatus
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == tlsProxyContainer && status.Ready && status.RestartCount == 0 &&
			status.ContainerID != "" && status.State.Running != nil {
			statuses = append(statuses, status)
		}
	}
	if len(statuses) != 1 {
		return proxyPodIdentity{}, errors.New("the proxy container is not one ready, running, never-restarted container")
	}
	return proxyPodIdentity{
		name: pod.Name, uid: string(pod.UID), podIP: pod.Status.PodIP, containerID: statuses[0].ContainerID,
	}, nil
}

// tlsProxyIdentityStable holds the proxy to the identity a counter window
// started with: the same one live Pod, running and ready at the same address,
// and the same container, never restarted.
func tlsProxyIdentityStable(pods []corev1.Pod, identity proxyPodIdentity) bool {
	live := liveProxyPods(pods)
	if len(live) != 1 {
		return false
	}
	pod := &live[0]
	if pod.Status.Phase != corev1.PodRunning || !podReadyTrue(pod) || pod.Name != identity.name ||
		string(pod.UID) != identity.uid || pod.Status.PodIP != identity.podIP {
		return false
	}
	count := 0
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == tlsProxyContainer && status.RestartCount == 0 && status.Ready &&
			status.ContainerID == identity.containerID && status.State.Running != nil {
			count++
		}
	}
	return count == 1
}

// referenceAtDigest is the reference with its tag replaced by the digest, as
// ${reference%:*}@digest wrote it.
func referenceAtDigest(reference, digest string) string {
	if index := strings.LastIndex(reference, ":"); index >= 0 {
		reference = reference[:index]
	}
	return reference + "@" + digest
}

// convergedOn is a schema that applied the digest and converged on it, with
// no observation and no operation pending.
func convergedOn(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseInSync && status.Source.Digest == digest &&
		status.Applied != nil && status.Applied.ArtifactDigest == digest &&
		status.PendingObservation == nil && status.ActiveOperation == nil &&
		conditionIs(status.Conditions, "InSync", metav1.ConditionTrue, "ScopedConverged")
}

// awaitingApprovalOf is a schema that verified the digest and holds a plan
// for a person.
func awaitingApprovalOf(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseAwaitingApproval && status.Plan != nil && status.Plan.Name != "" &&
		status.Source.Digest == digest
}

// unsettledContainerLines is what a release that did not settle says about
// its Pods' containers: each one waiting or with a previous run, by its
// waiting reason, its last exit code and the first 400 characters of the
// message that run left. The runtime-verify init container's refusal is in
// that message.
func unsettledContainerLines(pods []corev1.Pod) []string {
	var lines []string
	for _, pod := range pods {
		for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
			waiting, terminated := status.State.Waiting, status.LastTerminationState.Terminated
			if waiting == nil && terminated == nil {
				continue
			}
			reason, exit, message := "-", "-", "-"
			if waiting != nil {
				reason = waiting.Reason
			}
			if terminated != nil {
				exit = strconv.Itoa(int(terminated.ExitCode))
				if terminated.Message != "" {
					message = terminated.Message
				}
			}
			if runes := []rune(message); len(runes) > 400 {
				message = string(runes[:400])
			}
			lines = append(lines, fmt.Sprintf("%s/%s: waiting=%s lastExit=%s message=%s", pod.Name, status.Name, reason, exit, message))
		}
	}
	return lines
}

// cutLines bounds every line of a log to its first width bytes, as cut -c
// did.
func cutLines(log []byte, width int) []byte {
	lines := bytes.Split(log, []byte("\n"))
	for index, line := range lines {
		if len(line) > width {
			lines[index] = line[:width]
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// The Pod-metadata row stands a ValidatingAdmissionPolicy in for a service
// mesh or a policy engine: it refuses an operation Pod of the row's two
// schemas unless the Pod carries the mesh opt-out annotation.
const (
	podMetadataPolicyName = "e2e-pod-metadata-mesh-opt-out"
	podMetadataAnnotation = "sidecar.istio.io/inject"
	podMetadataLabel      = "acme.example/team"
	podMetadataRefusal    = "operation Pods in this namespace must opt out of sidecar injection"
)

// celString is a string as a CEL literal, which jq's tojson spelled.
func celString(value string) string {
	return `"` + jsonStringBody(value) + `"`
}

// podMetadataMatchCondition selects the Pods of the row's two schemas, by the
// subject label every operation Pod carries.
func podMetadataMatchCondition(schema, refusedSchema string) string {
	return `has(object.metadata.labels) && "operator.ptah.run/schema" in object.metadata.labels && ` +
		`object.metadata.labels["operator.ptah.run/schema"] in [` + celString(schema) + `, ` + celString(refusedSchema) + `]`
}

// podMetadataValidation admits a Pod that opted out of sidecar injection.
func podMetadataValidation(annotation string) string {
	return `has(object.metadata.annotations) && ` + celString(annotation) + ` in object.metadata.annotations && ` +
		`object.metadata.annotations[` + celString(annotation) + `] == "false"`
}

// podMetadataPolicyDocuments are the policy and its binding: the policy refuses
// Pod creation for the two schemas, and the binding scopes it to the test
// namespace alone.
func podMetadataPolicyDocuments(policy, namespace, schema, refusedSchema, annotation string) []map[string]any {
	return []map[string]any{
		{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy",
			"metadata": map[string]any{"name": policy},
			"spec": map[string]any{
				"failurePolicy": "Fail",
				"matchConstraints": map[string]any{"resourceRules": []any{map[string]any{
					"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE"}, "resources": []any{"pods"},
				}}},
				"matchConditions": []any{map[string]any{
					"name": "pod-metadata-row-schemas", "expression": podMetadataMatchCondition(schema, refusedSchema),
				}},
				"validations": []any{map[string]any{
					"expression": podMetadataValidation(annotation), "message": podMetadataRefusal,
				}},
			},
		},
		{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding",
			"metadata": map[string]any{"name": policy},
			"spec": map[string]any{
				"policyName": policy, "validationActions": []any{"Deny"},
				"matchResources": map[string]any{"namespaceSelector": map[string]any{
					"matchLabels": map[string]any{"kubernetes.io/metadata.name": namespace},
				}},
			},
		},
	}
}

// podMetadataProbePod is a Pod the policy must refuse: the refused schema's
// subject label and no annotation, hardened so nothing else refuses it first.
func podMetadataProbePod(namespace, refusedSchema, image string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"namespace": namespace, "name": "e2e-pod-metadata-probe",
			"labels": map[string]any{labelSchema: refusedSchema},
		},
		"spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532),
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name": "probe", "image": image, "command": []any{"/bin/true"},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": map[string]any{"drop": []any{"ALL"}},
				},
			}},
		},
	}
}

// podAdmissionRefusalReported is a schema holding an operation whose Pod the
// policy refused, and saying so: Ready False for PodAdmissionRefused, with the
// policy's message and the kind of admission that refused it.
func podAdmissionRefusalReported(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.ActiveOperation != nil &&
		slices.ContainsFunc(schema.Status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == "Ready" && condition.Status == metav1.ConditionFalse &&
				condition.Reason == "PodAdmissionRefused" &&
				strings.Contains(condition.Message, "must opt out of sidecar injection") &&
				strings.Contains(condition.Message, "ValidatingAdmissionPolicy")
		})
}

// readyConditionNamesJob is a schema with one Ready condition, which names the
// Job whose Pod was refused.
func readyConditionNamesJob(schema *ptahv1alpha1.PtahSchema, job string) bool {
	var ready []metav1.Condition
	for _, condition := range schema.Status.Conditions {
		if condition.Type == "Ready" {
			ready = append(ready, condition)
		}
	}
	return len(ready) == 1 && strings.Contains(ready[0].Message, job)
}

// refusedJobIdle is a Job with no Pod, active or done, and no verdict: what a
// Job whose every Pod admission refused looks like.
func refusedJobIdle(job *batchv1.Job) bool {
	status := job.Status
	if status.Active != 0 || status.Succeeded != 0 || status.Failed != 0 {
		return false
	}
	return !slices.ContainsFunc(status.Conditions, func(condition batchv1.JobCondition) bool {
		return condition.Status == corev1.ConditionTrue
	})
}

// failedCreateNamesPolicy reports a FailedCreate the Job controller recorded
// against the Job, naming the policy that refused its Pod.
func failedCreateNamesPolicy(events []corev1.Event, job, policy string) bool {
	return slices.ContainsFunc(events, func(event corev1.Event) bool {
		return event.InvolvedObject.Name == job && event.Reason == "FailedCreate" && strings.Contains(event.Message, policy)
	})
}

// operatorLabelCount counts the labels the operator owns on an object: its
// app.kubernetes.io and operator.ptah.run keys.
func operatorLabelCount(labels map[string]string) int {
	count := 0
	for key := range labels {
		if strings.HasPrefix(key, "app.kubernetes.io/") || strings.HasPrefix(key, "operator.ptah.run/") {
			count++
		}
	}
	return count
}

// declaredMetadataOnJobs holds every operation Job of a schema to carrying the
// declared label and annotation on itself and on its Pod template, beside
// exactly the operator's five labels, and holds the Jobs to including the one
// Apply.
func declaredMetadataOnJobs(jobs []batchv1.Job, label, annotation string) error {
	applies := 0
	for _, job := range jobs {
		template := job.Spec.Template
		if job.Labels[label] != "platform" || template.Labels[label] != "platform" ||
			job.Annotations[annotation] != "false" || template.Annotations[annotation] != "false" ||
			operatorLabelCount(job.Labels) != 5 {
			return fmt.Errorf("Job %s does not carry the declared metadata beside the operator's own", job.Name)
		}
		if job.Labels[labelOperation] == "apply" {
			applies++
		}
	}
	if applies != 1 {
		return fmt.Errorf("%d Apply Jobs rather than one", applies)
	}
	return nil
}

// declaredAnnotationOnPods holds every admitted Pod to the declared opt-out
// annotation, which is what the policy read.
func declaredAnnotationOnPods(pods []corev1.Pod, annotation string) bool {
	return !slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return pod.Annotations[annotation] != "false" })
}
