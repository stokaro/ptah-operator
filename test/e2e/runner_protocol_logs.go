package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// Keep complete logs while the terminal Pod exists. Recovery may take longer
// than the Job TTL; attribution must not fetch logs from a deleted or replaced
// Pod. Both live reads bracket the log request and bind it to the retained UID.
type runnerRefusalLogs struct {
	pod  *corev1.Pod
	logs map[string][]byte
}

func captureRunnerRefusalLogs(pod *corev1.Pod, get func() (*corev1.Pod, error), read func(string) ([]byte, error), scan func([]byte, string)) (runnerRefusalLogs, error) {
	evidence := runnerRefusalLogs{}
	if pod == nil || pod.UID == "" || pod.Name == "" || pod.Namespace == "" || pod.Status.Phase != corev1.PodFailed || !noRestarts(pod) {
		return evidence, errors.New("runner log audit needs one exact failed Pod without restarts")
	}
	before, err := get()
	if err != nil || !runnerLogPodMatches(pod, before) {
		return evidence, errors.New("the refused runner Pod disappeared or changed before its log audit")
	}
	logs := map[string][]byte{}
	for _, name := range startedContainers(pod) {
		if _, duplicate := logs[name]; duplicate {
			return evidence, errors.New("the refused runner log audit has duplicate container identities")
		}
		raw, err := read(name)
		if err != nil {
			return evidence, errors.New("could not read a refused runner's complete container log")
		}
		scan(raw, "complete failed runner container log")
		logs[name] = bytes.Clone(raw)
	}
	after, err := get()
	if err != nil || !runnerLogPodMatches(pod, after) {
		return evidence, errors.New("the refused runner Pod disappeared or changed during its log audit")
	}
	evidence = runnerRefusalLogs{pod: pod.DeepCopy(), logs: logs}
	if err := evidence.matches(pod); err != nil {
		return runnerRefusalLogs{}, err
	}
	return evidence, nil
}

func (e runnerRefusalLogs) matches(pod *corev1.Pod) error {
	if !runnerLogPodMatches(e.pod, pod) || len(e.logs) != 2 ||
		!slices.Contains(startedContainers(pod), "install-runner") ||
		len(startedContainers(pod)) != len(e.logs) {
		return errors.New("the refused runner has no complete logs bound to its terminal Pod")
	}
	if _, found := e.logs["install-runner"]; !found {
		return errors.New("the refused runner installer log was not retained")
	}
	if logs, found := e.logs["ptah"]; found {
		if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers) != 1 ||
			len(pod.Spec.EphemeralContainers) != 0 || !slices.Contains(startedContainers(pod), "ptah") ||
			!terminatedContainer(pod, "install-runner", 0) {
			return errors.New("the durable refusal has no exact installer and runner log inventory")
		}
		return unsupportedDurableRunnerRefusal(pod, logs)
	}
	if !slices.Contains(startedContainers(pod), "validate-source-authority") {
		return errors.New("the refused runner has no authority guard diagnostic")
	}
	want := fmt.Sprintf("ptah-runner: runner_protocol_mismatch: the Job expects runner protocol %d; this runner speaks protocol %d\n", runner.ProtocolVersion, runner.ProtocolVersion+1)
	if string(e.logs["validate-source-authority"]) != want {
		return errors.New("the actual OCI guard did not return the exact unsupported-runner diagnostic")
	}
	return nil
}

func runnerLogPodMatches(a, b *corev1.Pod) bool {
	return a != nil && b != nil && a.UID != "" && a.UID == b.UID && a.Namespace == b.Namespace && a.Name == b.Name &&
		equality.Semantic.DeepEqual(a.OwnerReferences, b.OwnerReferences) && equality.Semantic.DeepEqual(a.Labels, b.Labels) &&
		equality.Semantic.DeepEqual(a.Annotations, b.Annotations) && equality.Semantic.DeepEqual(a.Spec, b.Spec) &&
		a.Status.Phase == b.Status.Phase && a.Status.PodIP == b.Status.PodIP && equality.Semantic.DeepEqual(a.Status.PodIPs, b.Status.PodIPs) &&
		equality.Semantic.DeepEqual(a.Status.InitContainerStatuses, b.Status.InitContainerStatuses) &&
		equality.Semantic.DeepEqual(a.Status.ContainerStatuses, b.Status.ContainerStatuses) &&
		equality.Semantic.DeepEqual(a.Status.EphemeralContainerStatuses, b.Status.EphemeralContainerStatuses)
}
