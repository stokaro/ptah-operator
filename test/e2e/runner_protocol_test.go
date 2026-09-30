package e2e

import (
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func runnerApplyFixture(kind string) (*batchv1.Job, *corev1.Pod, *ptahv1alpha1.PtahSchema, *ptahv1alpha1.ExecutionBindingStatus, controllerIdentity, string) {
	image := "registry.test/runner@sha256:" + strings.Repeat("b", 64)
	controller := controllerIdentity{"registry.test/manager@sha256:" + strings.Repeat("a", 64), "unchanged", "1"}
	resource := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "approved", Namespace: "test", UID: "resource"}}
	binding := &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32), RunnerProtocolVersion: 1,
		ControllerStateVersion: 1, ExecutorImage: "registry.test/executor@sha256:" + strings.Repeat("e", 64), PtahVersion: "v0.9.0"}
	label, operation := labelSchema, runner.OperationApply
	if kind == "PtahMigration" {
		label, operation = labelMigration, runner.OperationMigrationApply
	}
	marks := map[string]string{annotationOperationID: "exact-operation", annotationBindingID: binding.Epoch,
		annotationControllerImage: controller.image, annotationControllerRev: controller.revision, annotationControllerState: controller.stateVersion,
		workload.AnnotationPlanFingerprint: proofDigest("c")}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "refused", Namespace: "test", UID: "refused-job",
		Labels: map[string]string{label: resource.Name, labelOperation: "apply"}, Annotations: maps.Clone(marks),
		OwnerReferences:   []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: kind, Name: resource.Name, UID: resource.UID, Controller: ptr.To(true)}},
		CreationTimestamp: metav1.NewTime(time.Unix(100, 0))},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To(int32(0)), Parallelism: ptr.To(int32(1)), Completions: ptr.To(int32(1)), PodReplacementPolicy: ptr.To(batchv1.Failed),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{label: resource.Name, labelOperation: "apply"}, Annotations: maps.Clone(marks)},
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{{Name: "install-runner", Image: image, Command: []string{"/ptah-runner"}, Args: []string{"--install-to", "/runner/ptah-runner"}}},
					Containers: []corev1.Container{{Name: "ptah", Image: binding.ExecutorImage, Command: []string{"/runner/ptah-runner"},
						Args: []string{"--ptah-binary", "/usr/local/bin/ptah", "--max-result-bytes", strconv.FormatInt(runner.DefaultMaxResultBytes, 10),
							"--max-plan-bytes", strconv.FormatInt(runner.DefaultMaxPlanBytes, 10), "--operation", string(operation)},
						Env: []corev1.EnvVar{{Name: runner.EnvOperationID, Value: "exact-operation"}, {Name: runner.EnvRunnerProtocolVersion, Value: "1"}}}},
				}},
		}}
	if kind == "PtahMigration" {
		job.Spec.Template.Spec.InitContainers = append(job.Spec.Template.Spec.InitContainers,
			corev1.Container{Name: "validate-source-authority", Image: binding.ExecutorImage, Command: []string{"/runner/ptah-runner"},
				Args: []string{"--validate-oci-source", "oci://registry.test/migrations@" + proofDigest("a")}, Env: []corev1.EnvVar{{Name: runner.EnvRunnerProtocolVersion, Value: "1"}}},
			corev1.Container{Name: "fetch-migrations", Image: binding.ExecutorImage, Command: []string{"/usr/local/bin/ptah"}})
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "refused-pod", Namespace: "test", UID: "refused-pod",
		Labels: maps.Clone(job.Labels), Annotations: maps.Clone(marks),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}},
		Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodFailed, PodIP: "10.0.0.2",
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, StartedAt: metav1.NewTime(time.Unix(101, 0)), FinishedAt: metav1.NewTime(time.Unix(102, 0))}}},
				{Name: "validate-source-authority", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, StartedAt: metav1.NewTime(time.Unix(103, 0)), FinishedAt: metav1.NewTime(time.Unix(104, 0))}}},
			}}}
	return job, pod, resource, binding, controller, image
}

func TestUnsupportedRunnerApplyKeepsTheActualSupportedContracts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"PtahSchema", "PtahMigration"} {
		t.Run(kind, func(t *testing.T) {
			job, _, resource, binding, controller, image := runnerApplyFixture(kind)
			if err := runnerProtocolApplyInputs(job, kind, resource, binding, image, proofDigest("c"), controller); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func(*batchv1.Job){
				"another owner":        func(j *batchv1.Job) { j.OwnerReferences[0].UID = "foreign" },
				"another epoch":        func(j *batchv1.Job) { j.Annotations[annotationBindingID] = "v1-" + strings.Repeat("b", 32) },
				"different manager":    func(j *batchv1.Job) { j.Spec.Template.Annotations[annotationControllerImage] = "other" },
				"retries":              func(j *batchv1.Job) { j.Spec.BackoffLimit = ptr.To(int32(1)) },
				"multiple completions": func(j *batchv1.Job) { j.Spec.Completions = ptr.To(int32(2)) },
				"supported image":      func(j *batchv1.Job) { j.Spec.Template.Spec.InitContainers[0].Image = controller.image },
				"changed executor":     func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "other" },
				"future Job protocol":  func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Env[1].Value = "2" },
				"unbound operation":    func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Env[0].Value = "other" },
				"duplicate protocol":   func(j *batchv1.Job) { c := &j.Spec.Template.Spec.Containers[0]; c.Env = append(c.Env, c.Env[1]) },
				"wrong entry point":    func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Command = []string{"/usr/local/bin/ptah"} },
			} {
				t.Run(name, func(t *testing.T) {
					changed := job.DeepCopy()
					mutate(changed)
					if runnerProtocolApplyInputs(changed, kind, resource, binding, image, proofDigest("c"), controller) == nil {
						t.Fatal("the unsupported runner proof accepted an unrelated or unsupported workload contract")
					}
				})
			}
		})
	}
}

func TestUnsupportedMigrationGuardRequiresFetchAndPtahNeverToStart(t *testing.T) {
	t.Parallel()
	job, pod, _, _, _, _ := runnerApplyFixture("PtahMigration")
	if !migrationRunnerGuardRefused(job, pod) {
		t.Fatal("the exact pre-fetch protocol refusal was rejected")
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"missing guard":     func(p *corev1.Pod) { p.Status.InitContainerStatuses = p.Status.InitContainerStatuses[:1] },
		"different error":   func(p *corev1.Pod) { p.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 1 },
		"missing timing":    func(p *corev1.Pod) { p.Status.InitContainerStatuses[1].State.Terminated.StartedAt = metav1.Time{} },
		"wrong executable":  func(p *corev1.Pod) { p.Spec.InitContainers[1].Command = []string{"/other"} },
		"another Job":       func(p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" },
		"another operation": func(p *corev1.Pod) { p.Annotations[annotationOperationID] = "foreign" },
		"fetch started": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, corev1.ContainerStatus{Name: "fetch-migrations", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
		},
		"main exited": func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		},
		"main previously exited": func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		},
		"duplicate guard": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, p.Status.InitContainerStatuses[1])
		},
		"restarted installer": func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].RestartCount = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if migrationRunnerGuardRefused(job, changed) {
				t.Fatal("a different failure or started database process passed as the exact pre-fetch refusal")
			}
		})
	}
	clients := map[string]operationSQLClient{"10.0.0.1": {resourceUID: "resource", jobUID: "history", podUID: "history-pod", operation: "history"}}
	attributed, err := addRefusedRunnerSQLClient(clients, "resource", job, pod, "fetch-migrations")
	if err != nil || len(clients) != 1 || attributed[pod.Status.PodIP].jobUID != string(job.UID) || attributed[pod.Status.PodIP].operation != "apply" {
		t.Fatalf("the exact refused guard lost its native SQL attribution: %v", err)
	}
	if _, err := addRefusedRunnerSQLClient(nil, "resource", job, pod, "fetch-migrations"); err == nil {
		t.Fatal("an empty diagnostic audit became a refusal proof")
	}
	pod.Status.PodIP = "10.0.0.1"
	if _, err := addRefusedRunnerSQLClient(clients, "resource", job, pod, "fetch-migrations"); err == nil {
		t.Fatal("a reused client address hid a refused runner's SQL")
	}
}

func TestRunnerWatchHistoryRefusesReplayReplacementMissingEvidenceAndOverlap(t *testing.T) {
	t.Parallel()
	job, pod, resource, _, _, _ := runnerApplyFixture("PtahMigration")
	freshJob, freshPod := job.DeepCopy(), pod.DeepCopy()
	freshJob.UID, freshJob.Name, freshJob.CreationTimestamp = "fresh-job", "fresh", metav1.NewTime(time.Unix(105, 0))
	freshPod.UID, freshPod.Name, freshPod.OwnerReferences[0].UID, freshPod.OwnerReferences[0].Name = "fresh-pod", "fresh-pod", freshJob.UID, freshJob.Name
	freshJob.Annotations[annotationOperationID], freshJob.Spec.Template.Annotations[annotationOperationID], freshPod.Annotations[annotationOperationID] = "fresh-operation", "fresh-operation", "fresh-operation"
	freshJob.Spec.Template.Spec.Containers[0].Env[0].Value, freshPod.Spec.Containers[0].Env[0].Value = "fresh-operation", "fresh-operation"
	expected := []runnerProtocolApply{{job, pod}, {freshJob, freshPod}}
	jobs, pods := []batchv1.Job{*job, *freshJob}, []corev1.Pod{*pod, *freshPod}
	check := func(j []batchv1.Job, p []corev1.Pod, e []runnerProtocolApply) error {
		return runnerProtocolNoReplay(j, p, "PtahMigration", resource.Name, resource.Namespace, resource.UID, e)
	}
	if err := check(jobs, pods, expected); err != nil {
		t.Fatal(err)
	}
	extra := job.DeepCopy()
	extra.UID = "replayed-job"
	changedPod := pod.DeepCopy()
	changedPod.Spec.InitContainers[0].Image = "changed"
	replacementPod := pod.DeepCopy()
	replacementPod.UID = types.UID("replacement-pod")
	for name, verify := range map[string]func() error{
		"empty history":        func() error { return check(nil, nil, expected) },
		"missing Job":          func() error { return check(jobs[:1], pods, expected) },
		"missing Pod":          func() error { return check(jobs, pods[:1], expected) },
		"replay":               func() error { return check(append(jobs, *extra), pods, expected) },
		"Pod replacement":      func() error { return check(jobs, append(pods, *replacementPod), expected) },
		"earlier image change": func() error { return check(jobs, append([]corev1.Pod{*changedPod}, pods...), expected) },
		"empty expected set":   func() error { return check(jobs, pods, nil) },
		"hidden replacement label": func() error {
			copy := replacementPod.DeepCopy()
			delete(copy.Labels, labelOperation)
			return check(jobs, append(pods, *copy), expected)
		},
		"same operation replayed": func() error {
			copy := freshJob.DeepCopy()
			copy.Annotations[annotationOperationID] = job.Annotations[annotationOperationID]
			return check([]batchv1.Job{*job, *copy}, pods, []runnerProtocolApply{{job, pod}, {copy, freshPod}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if verify() == nil {
				t.Fatal("a replay, replacement or incomplete runner history passed")
			}
		})
	}
	freshJob.CreationTimestamp = metav1.NewTime(time.Unix(103, 0))
	jobs[1] = *freshJob
	if check(jobs, pods, expected) == nil {
		t.Fatal("the fresh Apply overlapped the refused guard")
	}
}

func TestRunnerPublicationRequiresANewDigestInItsOriginalRepository(t *testing.T) {
	t.Parallel()
	source := "registry.test/operator@" + proofDigest("a")
	updated := "registry.test/operator@" + proofDigest("b")
	if reference, err := runnerVariantReference(source, "Runner: "+updated+"\n"); err != nil || reference != updated {
		t.Fatal("the exact runner publication was rejected")
	}
	for _, output := range []string{updated, "Runner: " + source, "Runner: registry.test/other@" + proofDigest("b"), "Runner: registry.test/operator:tag",
		"Executor: " + updated, "warning\nRunner: " + updated, "Runner: " + updated + "\nRunner: " + updated} {
		if _, err := runnerVariantReference(source, output); err == nil {
			t.Fatal("an unbound, mutable or mixed runner publication passed")
		}
	}
}

func TestTerminalLogCoverageAccountsForAnOrderedFailedInit(t *testing.T) {
	t.Parallel()
	_, pod, _, _, _, _ := runnerApplyFixture("PtahMigration")
	if !terminalPodLogsComplete(pod) {
		t.Fatal("a proved init refusal left nonexistent fetch and main logs unaudited")
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"not terminal":                func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"succeeded with missing logs": func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
		"installer unfinished":        func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Terminated = nil },
		"no failed init":              func(p *corev1.Pod) { p.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 0 },
		"later init ran": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, corev1.ContainerStatus{Name: "fetch-migrations", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}})
		},
		"main ran": func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		},
		"unknown status": func(p *corev1.Pod) { p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "unaccounted"}} },
		"duplicate status": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, p.Status.InitContainerStatuses[1])
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if terminalPodLogsComplete(changed) {
				t.Fatal("an incomplete or contradictory container history passed its credential log audit")
			}
		})
	}
}
