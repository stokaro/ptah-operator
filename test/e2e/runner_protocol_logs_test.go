package e2e

import (
	"bytes"
	"errors"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"k8s.io/utils/ptr"
	"os"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunnerRefusalLogsSurviveCollectionWithoutAnotherRead(t *testing.T) {
	t.Parallel()
	_, pod, _, _, _, _ := runnerApplyFixture("PtahMigration")
	diagnostic, err := os.ReadFile("../../testdata/e2e/readings/unsupported-runner-native-guard.txt")
	if err != nil {
		t.Fatal(err)
	}
	live := pod.DeepCopy()
	gets, reads := 0, 0
	scanned := map[string]bool{}
	evidence, err := captureRunnerRefusalLogs(pod, func() (*corev1.Pod, error) {
		gets++
		if live == nil {
			t.Fatal("read the live Pod after TTL collection")
		}
		return live.DeepCopy(), nil
	}, func(name string) ([]byte, error) {
		reads++
		if name == "validate-source-authority" {
			return diagnostic, nil
		}
		return []byte("installer output\n"), nil
	}, func(raw []byte, _ string) { scanned[string(raw)] = true })
	if err != nil || gets != 2 || reads != 2 || !scanned[string(diagnostic)] || !scanned["installer output\n"] {
		t.Fatalf("complete UID-bracketed logs were not audited: gets=%d reads=%d error=%v", gets, reads, err)
	}
	live = nil // The 300-second Job TTL deletes the Pod before recovery completes.
	if err := evidence.matches(pod); err != nil {
		t.Fatal("collection invalidated already audited terminal logs", err)
	}
	// Input ownership must not let a later poll or reused response buffer
	// rewrite what the earlier audit actually read.
	diagnostic[0] = 'X'
	changed := pod.DeepCopy()
	changed.ResourceVersion = "later"
	changed.DeletionTimestamp = &metav1.Time{}
	if err := evidence.matches(changed); err != nil {
		t.Fatal("metadata-only collection changes invalidated the audit", err)
	}
	pod.UID = "replacement"
	if evidence.matches(pod) == nil {
		t.Fatal("same-name replacement reused the original Pod's audited logs")
	}
}

func TestRunnerRefusalLogsRejectIncompleteOrChangedEvidence(t *testing.T) {
	t.Parallel()
	_, pod, _, _, _, _ := runnerApplyFixture("PtahMigration")
	diagnostic, err := os.ReadFile("../../testdata/e2e/readings/unsupported-runner-native-guard.txt")
	if err != nil {
		t.Fatal(err)
	}
	capture := func(get func() (*corev1.Pod, error), read func(string) ([]byte, error)) (runnerRefusalLogs, error) {
		return captureRunnerRefusalLogs(pod, get, read, func([]byte, string) {})
	}
	get := func() (*corev1.Pod, error) { return pod.DeepCopy(), nil }
	read := func(name string) ([]byte, error) {
		if name == "validate-source-authority" {
			return bytes.Clone(diagnostic), nil
		}
		return nil, nil // An empty installer log is still a successfully read log.
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"replacement":    func(p *corev1.Pod) { p.UID = "other" },
		"owner":          func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"operation":      func(p *corev1.Pod) { p.Annotations[annotationOperationID] = "other" },
		"container":      func(p *corev1.Pod) { p.Spec.InitContainers[0].Image = "other" },
		"restart":        func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].RestartCount++ },
		"client address": func(p *corev1.Pod) { p.Status.PodIP = "10.0.0.3" },
		"missing status": func(p *corev1.Pod) { p.Status.InitContainerStatuses = p.Status.InitContainerStatuses[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			for _, afterRead := range []bool{false, true} {
				calls := 0
				if _, err := capture(func() (*corev1.Pod, error) {
					calls++
					if !afterRead || calls == 2 {
						return changed, nil
					}
					return pod.DeepCopy(), nil
				}, read); err == nil {
					t.Fatal("changed Pod was accepted during log capture")
				}
			}
			evidence, err := capture(get, read)
			if err != nil || evidence.matches(changed) == nil {
				t.Fatal("changed Pod reused retained logs", err)
			}
		})
	}
	for _, afterRead := range []bool{false, true} {
		calls := 0
		if _, err := capture(func() (*corev1.Pod, error) {
			calls++
			if !afterRead || calls == 2 {
				return nil, errors.New("Pod unavailable")
			}
			return pod.DeepCopy(), nil
		}, read); err == nil {
			t.Fatal("unavailable identity became an audited log")
		}
	}
	for _, name := range []string{"install-runner", "validate-source-authority"} {
		if _, err := capture(get, func(container string) ([]byte, error) {
			if container == name {
				return nil, errors.New("log unavailable")
			}
			return read(container)
		}); err == nil {
			t.Fatal("missing complete container log passed")
		}
		evidence, err := capture(get, read)
		if err != nil {
			t.Fatal(err)
		}
		delete(evidence.logs, name)
		if evidence.matches(pod) == nil {
			t.Fatal("an incomplete retained archive passed")
		}
	}
	if _, err := capture(get, func(string) ([]byte, error) { return []byte("a different refusal\n"), nil }); err == nil {
		t.Fatal("another diagnostic passed as runner protocol mismatch")
	}
	if (runnerRefusalLogs{}).matches(pod) == nil {
		t.Fatal("an absent archive counted as a successful audit")
	}
}

func TestRunnerSQLAttributionRequiresTheRetainedRefusalLogs(t *testing.T) {
	t.Parallel()
	job, pod, resource, binding, controller, image := runnerApplyFixture("PtahMigration")
	diagnostic, err := os.ReadFile("../../testdata/e2e/readings/unsupported-runner-native-guard.txt")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := captureRunnerRefusalLogs(pod, func() (*corev1.Pod, error) { return pod.DeepCopy(), nil }, func(name string) ([]byte, error) {
		if name == "validate-source-authority" {
			return diagnostic, nil
		}
		return nil, nil
	}, func([]byte, string) {})
	if err != nil {
		t.Fatal(err)
	}
	history, control := job.DeepCopy(), pod.DeepCopy()
	history.UID, history.Name, history.Labels[labelOperation] = "history-job", "history", "history"
	control.UID, control.Name, control.Labels[labelOperation] = "history-pod", "history-pod", "history"
	control.OwnerReferences[0].UID, control.OwnerReferences[0].Name = history.UID, history.Name
	control.Status.Phase, control.Status.PodIP = corev1.PodSucceeded, "10.0.0.1"
	jobs := map[types.UID]batchv1.Job{job.UID: *job, history.UID: *history}
	pods := map[types.UID]corev1.Pod{pod.UID: *pod, control.UID: *control}
	logs := map[types.UID]runnerRefusalLogs{pod.UID: evidence}
	// Attribution has no live API dependency. Both the original Job and Pod
	// may have been collected after capture; their retained identities remain.
	clients, refused, err := runnerRefusalSQLClients(resource, "PtahMigration", binding, controller, image, jobs, pods, logs)
	if err != nil || len(clients) != 2 || len(refused) != 1 || !refused[job.UID] ||
		clients[pod.Status.PodIP].podUID != string(pod.UID) || clients[pod.Status.PodIP].operation != "apply" ||
		clients[control.Status.PodIP].operation != "history" {
		t.Fatal("the retained refusal or positive History control lost SQL attribution", err)
	}
	for name, mutate := range map[string]func(map[types.UID]runnerRefusalLogs){
		"missing archive": func(l map[types.UID]runnerRefusalLogs) { delete(l, pod.UID) },
		"wrong Pod": func(l map[types.UID]runnerRefusalLogs) {
			e := l[pod.UID]
			e.pod = e.pod.DeepCopy()
			e.pod.UID = "replacement"
			l[pod.UID] = e
		},
		"wrong diagnostic": func(l map[types.UID]runnerRefusalLogs) {
			e := l[pod.UID]
			e.logs = map[string][]byte{"install-runner": nil, "validate-source-authority": []byte("other failure")}
			l[pod.UID] = e
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := map[types.UID]runnerRefusalLogs{pod.UID: evidence}
			mutate(changed)
			if _, _, err := runnerRefusalSQLClients(resource, "PtahMigration", binding, controller, image, jobs, pods, changed); err == nil {
				t.Fatal("unproven refusal was silently admitted into SQL attribution")
			}
		})
	}
}

func TestDurableRunnerRefusalKeepsCompleteLogsAndSQLAttribution(t *testing.T) {
	job, pod, resource, binding, controller, image := runnerApplyFixture("PtahSchema")
	job.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(false)
	if err := jobconfig.Attach(job, resource.UID, 1, job.Annotations[annotationOperationID], "https://receiver.test:9444"); err != nil {
		t.Fatal(err)
	}
	pod.Spec = *job.Spec.Template.Spec.DeepCopy()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{pod.Status.InitContainerStatuses[1]}
	pod.Status.ContainerStatuses[0].Name = "ptah"
	pod.Status.InitContainerStatuses = pod.Status.InitContainerStatuses[:1]
	diagnostic, err := os.ReadFile("../../testdata/e2e/readings/unsupported-runner-native-guard.txt")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := captureRunnerRefusalLogs(pod, func() (*corev1.Pod, error) { return pod.DeepCopy(), nil }, func(name string) ([]byte, error) {
		if name == "ptah" {
			return diagnostic, nil
		}
		return nil, nil
	}, func([]byte, string) {})
	if err != nil {
		t.Fatal(err)
	}
	observe, control := job.DeepCopy(), pod.DeepCopy()
	observe.UID, observe.Name, observe.Labels[labelOperation] = "observe-job", "observe", "observe"
	control.UID, control.Name, control.Labels[labelOperation] = "observe-pod", "observe-pod", "observe"
	control.OwnerReferences[0].UID, control.OwnerReferences[0].Name = observe.UID, observe.Name
	control.Status.Phase, control.Status.PodIP = corev1.PodSucceeded, "10.0.0.1"
	control.Status.ContainerStatuses[0].State.Terminated.ExitCode = 0
	jobs := map[types.UID]batchv1.Job{job.UID: *job, observe.UID: *observe}
	pods := map[types.UID]corev1.Pod{pod.UID: *pod, control.UID: *control}
	logs := map[types.UID]runnerRefusalLogs{pod.UID: evidence}
	clients, refused, err := runnerRefusalSQLClients(resource, "PtahSchema", binding, controller, image, jobs, pods, logs)
	if err != nil || len(clients) != 2 || len(refused) != 1 || !refused[job.UID] || clients[pod.Status.PodIP].operation != "apply" || clients[control.Status.PodIP].operation != "observe" {
		t.Fatal("durable refusal lost complete log or SQL attribution", err)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"replaced Job":       func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"different exit":     func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1 },
		"restarted runner":   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"changed executable": func(p *corev1.Pod) { p.Spec.Containers[0].Args[1] = "/other" },
		"foreign credential": func(p *corev1.Pod) { p.Spec.Volumes[0].Secret.SecretName = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if durableRunnerApplyRefused(job, changed) {
				t.Fatal("accepted unrelated failed workload")
			}
			changedPods := map[types.UID]corev1.Pod{changed.UID: *changed, control.UID: *control}
			if _, _, err := runnerRefusalSQLClients(resource, "PtahSchema", binding, controller, image, jobs, changedPods, logs); err == nil {
				t.Fatal("accepted unrelated SQL client")
			}
		})
	}
	for _, retained := range []map[types.UID]runnerRefusalLogs{
		{},
		{pod.UID: {pod: pod, logs: map[string][]byte{"ptah": diagnostic}}},
		{pod.UID: {pod: pod, logs: map[string][]byte{"install-runner": nil, "ptah": []byte("another failure\n")}}},
	} {
		if _, _, err := runnerRefusalSQLClients(resource, "PtahSchema", binding, controller, image, jobs, pods, retained); err == nil {
			t.Fatal("accepted absent or incomplete refusal audit")
		}
	}
}
