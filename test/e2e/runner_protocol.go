package e2e

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func runnerVariantReference(original, output string) (string, error) {
	repository, oldDigest, ok := strings.Cut(original, "@")
	updated, marker := strings.CutPrefix(strings.TrimSpace(output), "Runner: ")
	nextRepository, nextDigest, next := strings.Cut(updated, "@")
	if !ok || !marker || !next || repository == "" || repository != nextRepository ||
		!sha256Pattern.MatchString(oldDigest) || !sha256Pattern.MatchString(nextDigest) || nextDigest == oldDigest {
		return "", errors.New("unsupported runner publication did not return a different digest in the original repository")
	}
	return updated, nil
}

// Changing the recorded image must leave the supported protocol in both the
// approved plan and every container that invokes the installed runner.
func runnerProtocolApplyInputs(job *batchv1.Job, kind string, resource metav1.Object, binding *ptahv1alpha1.ExecutionBindingStatus,
	image, fingerprint string, controller controllerIdentity,
) error {
	label := labelSchema
	if kind == "PtahMigration" {
		label = labelMigration
	} else if kind != "PtahSchema" {
		return errors.New("unsupported runner proof has no declared resource family")
	}
	if job == nil || resource == nil || resource.GetUID() == "" || binding == nil || binding.RunnerProtocolVersion != int32(runner.ProtocolVersion) ||
		job.UID == "" || job.Namespace != resource.GetNamespace() || job.Labels[label] != resource.GetName() || job.Labels[labelOperation] != "apply" ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, kind, resource.GetName(), resource.GetUID()) ||
		!executionIdentityOnJob(job, controller) || job.Annotations[annotationBindingID] != binding.Epoch ||
		fingerprint == "" || (kind == "PtahSchema" && job.Annotations[workload.AnnotationPlanFingerprint] != fingerprint) || !digestSuffix.MatchString(image) ||
		job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed ||
		job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.Completions == nil || *job.Spec.Completions != 1 ||
		job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever || !jobUsesExecutor(job, binding.ExecutorImage) {
		return errors.New("unsupported runner Apply lost its exact approved owner, execution binding or single-attempt workload")
	}
	if durableResultJob(job) {
		var err error
		job, err = resultJobWithoutProjection(job)
		if err != nil {
			return err
		}
	}
	spec := job.Spec.Template.Spec
	main := spec.Containers[0]
	operation := runner.OperationApply
	if kind == "PtahMigration" {
		operation = runner.OperationMigrationApply
	}
	if main.Name != "ptah" || !slices.Equal(main.Command, []string{"/runner/ptah-runner"}) || !slices.Equal(main.Args, []string{
		"--ptah-binary", "/usr/local/bin/ptah", "--max-result-bytes", strconv.FormatInt(runner.DefaultMaxResultBytes, 10),
		"--max-plan-bytes", strconv.FormatInt(runner.DefaultMaxPlanBytes, 10), "--operation", string(operation),
	}) ||
		!exactLiteralEnv(viewContainers([]corev1.Container{main})[0], runner.EnvRunnerProtocolVersion, strconv.Itoa(runner.ProtocolVersion)) ||
		!exactLiteralEnv(viewContainers([]corev1.Container{main})[0], runner.EnvOperationID, job.Annotations[annotationOperationID]) || job.Annotations[annotationOperationID] == "" {
		return errors.New("unsupported runner Apply omitted or changed its supported protocol or exact operation")
	}
	wantInits := 1
	if kind == "PtahMigration" {
		wantInits = 3
	}
	if len(spec.InitContainers) != wantInits || spec.InitContainers[0].Name != "install-runner" || spec.InitContainers[0].Image != image ||
		!slices.Equal(spec.InitContainers[0].Command, []string{"/ptah-runner"}) || !slices.Equal(spec.InitContainers[0].Args, []string{"--install-to", "/runner/ptah-runner"}) {
		return errors.New("unsupported runner Apply did not install the exact replacement image")
	}
	if kind == "PtahMigration" {
		guard, fetch := spec.InitContainers[1], spec.InitContainers[2]
		if guard.Name != "validate-source-authority" || guard.Image != binding.ExecutorImage || !slices.Equal(guard.Command, []string{"/runner/ptah-runner"}) ||
			len(guard.Args) < 2 || guard.Args[0] != "--validate-oci-source" || !strings.HasPrefix(guard.Args[1], "oci://") ||
			!exactLiteralEnv(viewContainers([]corev1.Container{guard})[0], runner.EnvRunnerProtocolVersion, strconv.Itoa(runner.ProtocolVersion)) ||
			fetch.Name != "fetch-migrations" || fetch.Image != binding.ExecutorImage {
			return errors.New("unsupported migration runner lost its supported OCI guard or pinned fetch image")
		}
	}
	return nil
}

func unsupportedRunnerFrame(logs []byte, operation runner.Operation, id string) error {
	result, err := runner.ParseResultFor(logs, operation, id)
	var mismatch *runner.ProtocolMismatchError
	if !errors.As(err, &mismatch) || mismatch.RunnerVersion != runner.ProtocolVersion+1 || !reflect.DeepEqual(result, runner.Result{}) ||
		mismatch.Message != fmt.Sprintf("the Job expects runner protocol %d; this runner speaks protocol %d", runner.ProtocolVersion, runner.ProtocolVersion+1) {
		return errors.New("the exact Apply Pod did not return the complete bound unsupported-runner refusal")
	}
	return nil
}

// This diagnostic proves the fixture refused before dispatch. The controller
// must still recover an unknown outcome from the database, never from logs.
func unsupportedDurableRunnerRefusal(pod *corev1.Pod, logs []byte) error {
	want := fmt.Sprintf("ptah-runner: runner_protocol_mismatch: the Job expects runner protocol %d; this runner speaks protocol %d\n", runner.ProtocolVersion, runner.ProtocolVersion+1)
	if pod == nil || pod.Status.Phase != corev1.PodFailed || !noRestarts(pod) ||
		!terminatedContainer(pod, "ptah", 2) || string(logs) != want {
		return errors.New("the durable Apply did not return the exact pre-dispatch protocol refusal")
	}
	return nil
}

func neverStartedContainer(pod *corev1.Pod, name string) bool {
	matches := 0
	for _, status := range allStatuses(pod) {
		if status.Name != name {
			continue
		}
		matches++
		if status.RestartCount != 0 || status.State.Running != nil || status.State.Terminated != nil ||
			status.LastTerminationState.Running != nil || status.LastTerminationState.Terminated != nil || (status.Started != nil && *status.Started) {
			return false
		}
	}
	return matches <= 1
}

func terminatedContainer(pod *corev1.Pod, name string, exit int32) bool {
	matches := 0
	for _, status := range allStatuses(pod) {
		if status.Name != name {
			continue
		}
		matches++
		end := status.State.Terminated
		if status.RestartCount != 0 || status.LastTerminationState.Terminated != nil || end == nil || end.ExitCode != exit || end.StartedAt.IsZero() ||
			end.FinishedAt.IsZero() || end.FinishedAt.Before(&end.StartedAt) {
			return false
		}
	}
	return matches == 1
}

// A terminal Pod whose init failed has containers that Kubernetes never
// started. Their absence is complete log coverage only when the ordered init
// statuses prove where startup stopped and every later container is untouched.
func terminalPodLogsComplete(pod *corev1.Pod) bool {
	if pod == nil || (pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) || !noRestarts(pod) {
		return false
	}
	declared := declaredContainers(pod)
	if len(declared) == 0 {
		return false
	}
	terminated := terminatedInStatusOrder(pod)
	slices.Sort(terminated)
	if slices.Equal(declared, terminated) {
		return true
	}
	if pod.Status.Phase != corev1.PodFailed || len(pod.Spec.EphemeralContainers) != 0 {
		return false
	}
	known := map[string]bool{}
	for _, name := range declared {
		if known[name] {
			return false
		}
		known[name] = true
	}
	seen := map[string]bool{}
	for _, status := range allStatuses(pod) {
		if !known[status.Name] || seen[status.Name] {
			return false
		}
		seen[status.Name] = true
	}
	failed := false
	for _, container := range pod.Spec.InitContainers {
		if failed {
			if !neverStartedContainer(pod, container.Name) {
				return false
			}
			continue
		}
		var end *corev1.ContainerStateTerminated
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name == container.Name {
				end = status.State.Terminated
			}
		}
		if end == nil {
			return false
		}
		failed = end.ExitCode != 0
	}
	if !failed {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if !neverStartedContainer(pod, container.Name) {
			return false
		}
	}
	return true
}

// A failed init has no main result. Require the copied runner to install,
// its authority guard to refuse, and both fetch and Ptah never to start.
func migrationRunnerGuardRefused(job *batchv1.Job, pod *corev1.Pod) bool {
	return runnerGuardRefused(job, pod, "fetch-migrations")
}

func runnerGuardRefused(job *batchv1.Job, pod *corev1.Pod, fetchName string) bool {
	if job == nil || pod == nil || job.UID == "" || pod.UID == "" || pod.Namespace != job.Namespace || pod.Status.Phase != corev1.PodFailed ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) || !noRestarts(pod) ||
		pod.Annotations[annotationOperationID] == "" || pod.Annotations[annotationOperationID] != job.Annotations[annotationOperationID] ||
		len(pod.Spec.InitContainers) != 3 || len(pod.Spec.Containers) != 1 || len(pod.Spec.EphemeralContainers) != 0 ||
		len(job.Spec.Template.Spec.InitContainers) != 3 || len(job.Spec.Template.Spec.Containers) != 1 {
		return false
	}
	actual := append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...)
	expected := append(slices.Clone(job.Spec.Template.Spec.InitContainers), job.Spec.Template.Spec.Containers...)
	for i := range expected {
		if actual[i].Name != expected[i].Name || actual[i].Image != expected[i].Image || !slices.Equal(actual[i].Command, expected[i].Command) ||
			!slices.Equal(actual[i].Args, expected[i].Args) || !equality.Semantic.DeepEqual(actual[i].Env, expected[i].Env) {
			return false
		}
	}
	return actual[0].Name == "install-runner" && actual[1].Name == "validate-source-authority" && actual[2].Name == fetchName && actual[3].Name == "ptah" &&
		terminatedContainer(pod, "install-runner", 0) && terminatedContainer(pod, "validate-source-authority", 2) &&
		neverStartedContainer(pod, fetchName) && neverStartedContainer(pod, "ptah")
}

// Keep the shared SQL contract's successful-Pod requirement. This one exact
// failed guard keeps its actual operation and client identity. Its caller
// refuses all SQL from that exact Job instead of dropping the failed Pod.
func addRefusedRunnerSQLClient(clients map[string]operationSQLClient, resourceUID types.UID, job *batchv1.Job, pod *corev1.Pod, fetchName string) (map[string]operationSQLClient, error) {
	if resourceUID == "" || len(clients) == 0 || !runnerGuardRefused(job, pod, fetchName) || job.Labels[labelOperation] == "" {
		return nil, errors.New("failed runner SQL attribution has no positive History control and exact refused guard")
	}
	address, err := netip.ParseAddr(pod.Status.PodIP)
	if err != nil {
		return nil, errors.New("the refused runner Pod has no numeric SQL client address")
	}
	host := address.Unmap().String()
	if _, exists := clients[host]; exists {
		return nil, errors.New("the refused runner shared a SQL client address with another workload")
	}
	copy := maps.Clone(clients)
	copy[host] = operationSQLClient{resourceUID: string(resourceUID), jobUID: string(job.UID), podUID: string(pod.UID), operation: job.Labels[labelOperation]}
	return copy, nil
}

type runnerProtocolApply struct {
	job *batchv1.Job
	pod *corev1.Pod
}

func runnerHistoryReadOnly(spec corev1.PodSpec, operation, kind string) bool {
	allowed := map[string]string{"resolve": "resolve", "verify": "verify"}
	if kind == "PtahMigration" {
		allowed["history"] = "migration-history"
	} else {
		allowed["observe"], allowed["plan"] = "observe", "plan"
	}
	want := allowed[operation]
	if want == "" || len(spec.Containers) != 1 || !slices.Equal(spec.Containers[0].Command, []string{"/runner/ptah-runner"}) {
		return false
	}
	args, matches := spec.Containers[0].Args, 0
	for i, arg := range args {
		if strings.HasPrefix(arg, "--operation=") {
			return false
		}
		if arg == "--operation" {
			if i+1 == len(args) || args[i+1] != want {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

// The complete closed watch history must contain each expected workload and
// no replacement, replay or overlapping second Apply. Repeated events are
// checked too, so a later correct object cannot hide an earlier changed one.
func runnerProtocolNoReplay(jobs []batchv1.Job, pods []corev1.Pod, kind, name, namespace string, resourceUID types.UID, expected []runnerProtocolApply) error {
	label := labelSchema
	if kind == "PtahMigration" {
		label = labelMigration
	} else if kind != "PtahSchema" {
		return errors.New("runner history has no declared resource family")
	}
	allowed := map[types.UID]runnerProtocolApply{}
	for _, pair := range expected {
		if pair.job == nil || pair.pod == nil || pair.job.UID == "" || pair.pod.UID == "" || allowed[pair.job.UID].job != nil ||
			!ownedExactlyOnce(pair.job.OwnerReferences, ptahSchemaAPIVersion, kind, name, resourceUID) ||
			!ownedExactlyOnce(pair.pod.OwnerReferences, "batch/v1", "Job", pair.job.Name, pair.job.UID) {
			return errors.New("runner history has missing or ambiguous expected workload identities")
		}
		allowed[pair.job.UID] = pair
	}
	if resourceUID == "" || len(expected) == 0 || len(expected) > 2 {
		return errors.New("runner history has no bounded nonempty Apply set")
	}
	seenJobs, seenPods := map[types.UID]bool{}, map[types.UID]bool{}
	ownedJobs := map[types.UID]bool{}
	for i := range jobs {
		job := &jobs[i]
		owned := job.Namespace == namespace && ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, kind, name, resourceUID)
		if owned {
			ownedJobs[job.UID] = true
			if job.Labels[label] != name || (job.Labels[labelOperation] != "apply" && !runnerHistoryReadOnly(job.Spec.Template.Spec, job.Labels[labelOperation], kind)) {
				return errors.New("runner history lost an owned Job's declared operation")
			}
		}
		if allowed[job.UID].job == nil && (job.Labels[label] != name || job.Labels[labelOperation] != "apply") {
			continue
		}
		pair := allowed[job.UID]
		if pair.job == nil || job.Labels[label] != name || job.Labels[labelOperation] != "apply" || job.Namespace != namespace || !ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, kind, name, resourceUID) ||
			!equality.Semantic.DeepEqual(job.Spec.Template, pair.job.Spec.Template) || job.Annotations[annotationOperationID] != pair.job.Annotations[annotationOperationID] {
			return errors.New("runner history recorded an extra, replaced or changed Apply Job")
		}
		seenJobs[job.UID] = true
	}
	for i := range pods {
		pod := &pods[i]
		owner := metav1.GetControllerOf(pod)
		ownedApply := owner != nil && allowed[owner.UID].job != nil
		if owner != nil && ownedJobs[owner.UID] &&
			(pod.Labels[label] != name || (pod.Labels[labelOperation] != "apply" && !runnerHistoryReadOnly(pod.Spec, pod.Labels[labelOperation], kind))) {
			return errors.New("runner history lost an owned Pod's declared operation")
		}
		if !ownedApply && (pod.Labels[label] != name || pod.Labels[labelOperation] != "apply") {
			continue
		}
		matched := false
		for _, pair := range expected {
			if pod.UID == pair.pod.UID && pod.Namespace == namespace && pod.Labels[label] == name && pod.Labels[labelOperation] == "apply" &&
				ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", pair.job.Name, pair.job.UID) &&
				pod.Annotations[annotationOperationID] == pair.pod.Annotations[annotationOperationID] &&
				equality.Semantic.DeepEqual(pod.Spec.Containers, pair.pod.Spec.Containers) &&
				equality.Semantic.DeepEqual(pod.Spec.InitContainers, pair.pod.Spec.InitContainers) && len(pod.Spec.EphemeralContainers) == 0 {
				matched = true
			}
		}
		if !matched {
			return errors.New("runner history recorded a replacement or unaccounted Apply Pod")
		}
		seenPods[pod.UID] = true
	}
	if len(seenJobs) != len(expected) || len(seenPods) != len(expected) {
		return errors.New("runner history did not observe every expected Apply Job and Pod")
	}
	if len(expected) == 2 {
		if expected[0].job.Annotations[annotationOperationID] == "" || expected[1].job.Annotations[annotationOperationID] == "" ||
			expected[0].job.Annotations[annotationOperationID] == expected[1].job.Annotations[annotationOperationID] {
			return errors.New("the fresh authorization reused the refused Apply's operation identity")
		}
		var finished time.Time
		for _, status := range allStatuses(expected[0].pod) {
			if end := status.State.Terminated; end != nil && end.FinishedAt.After(finished) {
				finished = end.FinishedAt.Time
			}
		}
		if finished.IsZero() || expected[1].job.CreationTimestamp.IsZero() || expected[1].job.CreationTimestamp.Time.Before(finished) {
			return errors.New("the fresh authorized Apply preceded the refused workload's termination")
		}
	}
	return nil
}

// Successful diagnostics still use the common SQL attribution contract.
// An exact failed init is a separate, fully inspected client, and its actual
// operation is retained. Callers refuse all SQL from the returned Job set.
func runnerRefusalSQLClients(resource metav1.Object, kind string,
	binding *ptahv1alpha1.ExecutionBindingStatus, controller controllerIdentity, image string,
	jobs map[types.UID]batchv1.Job, pods map[types.UID]corev1.Pod, logs map[types.UID]runnerRefusalLogs,
) (map[string]operationSQLClient, map[types.UID]bool, error) {
	label, fetch := labelSchema, "fetch-schema"
	if kind == "PtahMigration" {
		label, fetch = labelMigration, "fetch-migrations"
	} else if kind != "PtahSchema" {
		return nil, nil, fmt.Errorf("unsupported runner SQL audit has no resource family")
	}
	if resource == nil || binding == nil || binding.RunnerProtocolVersion != int32(runner.ProtocolVersion) || !digestSuffix.MatchString(image) {
		return nil, nil, fmt.Errorf("unsupported runner SQL audit has no supported binding or exact refused image")
	}
	var successful []corev1.Pod
	var refused []runnerProtocolApply
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodFailed {
			successful = append(successful, pod)
			continue
		}
		owner := metav1.GetControllerOf(&pod)
		if owner == nil {
			return nil, nil, fmt.Errorf("the refused runner SQL client has no controller owner")
		}
		job, exists := jobs[owner.UID]
		if !exists || !runnerGuardRefused(&job, &pod, fetch) || !executionIdentityOnJob(&job, controller) ||
			!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, kind, resource.GetName(), resource.GetUID()) ||
			job.Annotations[annotationBindingID] != binding.Epoch || job.Spec.Template.Spec.InitContainers[0].Image != image ||
			!jobUsesExecutor(&job, binding.ExecutorImage) ||
			!exactLiteralEnv(viewContainers(job.Spec.Template.Spec.Containers)[0], runner.EnvRunnerProtocolVersion, strconv.Itoa(runner.ProtocolVersion)) ||
			!exactLiteralEnv(viewContainers(job.Spec.Template.Spec.InitContainers)[1], runner.EnvRunnerProtocolVersion, strconv.Itoa(runner.ProtocolVersion)) {
			return nil, nil, fmt.Errorf("a failed Pod was not the exact unsupported runner's pre-fetch refusal")
		}
		if err := logs[pod.UID].matches(&pod); err != nil {
			return nil, nil, err
		}
		refused = append(refused, runnerProtocolApply{job.DeepCopy(), pod.DeepCopy()})
	}
	var jobList []batchv1.Job
	for _, job := range jobs {
		jobList = append(jobList, job)
	}
	clients, err := operationSQLClientsForIdentities(resource.GetNamespace(), resource.GetName(), kind, label,
		map[string]bool{string(resource.GetUID()): true}, jobList, successful)
	if err != nil {
		return nil, nil, err
	}
	refusedJobs := map[types.UID]bool{}
	for _, pair := range refused {
		clients, err = addRefusedRunnerSQLClient(clients, resource.GetUID(), pair.job, pair.pod, fetch)
		if err != nil {
			return nil, nil, err
		}
		refusedJobs[pair.job.UID] = true
	}
	return clients, refusedJobs, nil
}
