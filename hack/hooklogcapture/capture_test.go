package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	testNamespace = "ptah-system"
	testJobName   = "ptah-crd-v2-deadbeef"
	testImage     = "ghcr.io/stokaro/ptah-operator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type logAttempt struct {
	stream io.ReadCloser
	err    error
}

type fakeResourceClient struct {
	mu sync.Mutex

	jobList *batchv1.JobList
	podList *corev1.PodList

	jobWatcher *watch.RaceFreeFakeWatcher
	podWatcher *watch.RaceFreeFakeWatcher

	jobListOptions  metav1.ListOptions
	podListOptions  metav1.ListOptions
	jobWatchOptions metav1.ListOptions
	podWatchOptions metav1.ListOptions

	logAttempts    []logAttempt
	logStarted     chan struct{}
	repeatLogError error

	jobWatchHook    func()
	podWatchGate    <-chan struct{}
	podWatchEntered chan struct{}
}

func (client *fakeResourceClient) listJobs(
	_ context.Context,
	_ string,
	options metav1.ListOptions,
) (*batchv1.JobList, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.jobListOptions = options
	return client.jobList.DeepCopy(), nil
}

func (client *fakeResourceClient) watchJobs(
	_ context.Context,
	_ string,
	options metav1.ListOptions,
) (watch.Interface, error) {
	client.mu.Lock()
	client.jobWatchOptions = options
	hook := client.jobWatchHook
	watcher := client.jobWatcher
	client.mu.Unlock()
	if hook != nil {
		hook()
	}
	return watcher, nil
}

func (client *fakeResourceClient) listPods(
	_ context.Context,
	_ string,
	options metav1.ListOptions,
) (*corev1.PodList, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.podListOptions = options
	return client.podList.DeepCopy(), nil
}

func (client *fakeResourceClient) watchPods(
	ctx context.Context,
	_ string,
	options metav1.ListOptions,
) (watch.Interface, error) {
	client.mu.Lock()
	client.podWatchOptions = options
	gate := client.podWatchGate
	entered := client.podWatchEntered
	watcher := client.podWatcher
	client.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate:
		}
	}
	return watcher, nil
}

func (client *fakeResourceClient) streamPodLogs(
	_ context.Context,
	_, _, _ string,
) (io.ReadCloser, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.logStarted != nil {
		select {
		case client.logStarted <- struct{}{}:
		default:
		}
	}
	if len(client.logAttempts) == 0 {
		if client.repeatLogError != nil {
			return nil, client.repeatLogError
		}
		return nil, errors.New("unexpected log stream attempt")
	}
	attempt := client.logAttempts[0]
	client.logAttempts = client.logAttempts[1:]
	return attempt.stream, attempt.err
}

func TestCaptureUsesSnapshotResourceVersionsAndCapturesPodFirst(t *testing.T) {
	t.Parallel()

	jobWatcher := watch.NewRaceFreeFake()
	podWatcher := watch.NewRaceFreeFake()
	client := &fakeResourceClient{
		jobList:     &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "job-rv-10"}},
		podList:     &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv-20"}},
		jobWatcher:  jobWatcher,
		podWatcher:  podWatcher,
		logAttempts: []logAttempt{{stream: io.NopCloser(strings.NewReader("late activation rejected\n"))}},
		logStarted:  make(chan struct{}, 1),
	}
	output := newTestOutputs(t)
	logPath := output.logPath

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- capture(ctx, client, testCaptureConfig(), output)
	}()

	waitForFileContents(t, output.ready.path, "ready\n")
	client.mu.Lock()
	if client.jobWatchOptions.ResourceVersion != "job-rv-10" {
		t.Fatalf("Job watch resourceVersion = %q", client.jobWatchOptions.ResourceVersion)
	}
	if client.podWatchOptions.ResourceVersion != "pod-rv-20" {
		t.Fatalf("Pod watch resourceVersion = %q", client.podWatchOptions.ResourceVersion)
	}
	if client.jobWatchOptions.FieldSelector != client.jobListOptions.FieldSelector || client.jobWatchOptions.FieldSelector == "" {
		t.Fatalf("Job selectors differ: list=%q watch=%q", client.jobListOptions.FieldSelector, client.jobWatchOptions.FieldSelector)
	}
	if client.podWatchOptions.LabelSelector != client.podListOptions.LabelSelector || client.podWatchOptions.LabelSelector == "" {
		t.Fatalf("Pod selectors differ: list=%q watch=%q", client.podListOptions.LabelSelector, client.podWatchOptions.LabelSelector)
	}
	client.mu.Unlock()

	job := validJob()
	pod := validPod(job.UID)
	podWatcher.Add(pod)
	select {
	case <-client.logStarted:
	case <-time.After(time.Second):
		t.Fatal("log stream did not start from the Pod ADDED event")
	}
	jobWatcher.Add(job)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("capture returned an error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("capture did not complete: %v", ctx.Err())
	}
	if err := output.close(); err != nil {
		t.Fatalf("close outputs: %v", err)
	}
	assertFileContents(t, logPath, "late activation rejected\n")
	assertFileContents(t, output.status.path, "captured\n")
}

func TestCaptureMarksReadyOnlyAfterBothWatchesAreEstablished(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	client := &fakeResourceClient{
		jobList:         &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}},
		podList:         &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher:      watch.NewRaceFreeFake(),
		podWatcher:      watch.NewRaceFreeFake(),
		podWatchGate:    gate,
		podWatchEntered: entered,
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- capture(ctx, client, testCaptureConfig(), output) }()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Pod Watch was not attempted")
	}
	assertFileContents(t, output.ready.path, "")
	close(gate)
	waitForFileContents(t, output.ready.path, "ready\n")
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("capture error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not stop after cancellation")
	}
}

func TestCaptureRetainsEventBetweenListAndWatch(t *testing.T) {
	t.Parallel()

	jobWatcher := watch.NewRaceFreeFake()
	podWatcher := watch.NewRaceFreeFake()
	job := validJob()
	client := &fakeResourceClient{
		jobList:      &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}},
		podList:      &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher:   jobWatcher,
		podWatcher:   podWatcher,
		jobWatchHook: func() { jobWatcher.Add(job) },
		logAttempts:  []logAttempt{{stream: io.NopCloser(strings.NewReader("captured\n"))}},
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- capture(ctx, client, testCaptureConfig(), output) }()

	waitForFileContents(t, output.ready.path, "ready\n")
	podWatcher.Add(validPod(job.UID))
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("capture returned an error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("capture did not retain the between-List-and-Watch event: %v", ctx.Err())
	}
}

func TestCaptureRejectsPreexistingObjectsBeforeWatching(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{
		jobList: &batchv1.JobList{
			ListMeta: metav1.ListMeta{ResourceVersion: "10"},
			Items:    []batchv1.Job{*validJob()},
		},
		podList:    &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher: watch.NewRaceFreeFake(),
		podWatcher: watch.NewRaceFreeFake(),
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()

	err := capture(context.Background(), client, testCaptureConfig(), output)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("capture error = %v, want preexisting Job rejection", err)
	}
}

func TestCaptureDoesNotPublishPodLogBeforeBindingItsWatchedJob(t *testing.T) {
	t.Parallel()

	jobWatcher := watch.NewRaceFreeFake()
	podWatcher := watch.NewRaceFreeFake()
	client := &fakeResourceClient{
		jobList:     &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}},
		podList:     &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher:  jobWatcher,
		podWatcher:  podWatcher,
		logAttempts: []logAttempt{{stream: io.NopCloser(strings.NewReader("must not be read"))}},
		logStarted:  make(chan struct{}, 1),
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- capture(ctx, client, testCaptureConfig(), output) }()

	waitForFileContents(t, output.ready.path, "ready\n")
	podWatcher.Add(validPod(types.UID("claimed-job-uid")))
	select {
	case <-client.logStarted:
	case <-time.After(time.Second):
		t.Fatal("capture did not start the quarantined Pod log stream promptly")
	}
	job := validJob()
	jobWatcher.Add(job)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "controlling owner does not match") {
			t.Fatalf("capture error = %v, want watched Job binding failure", err)
		}
	case <-ctx.Done():
		t.Fatalf("capture did not reject unbound Pod: %v", ctx.Err())
	}
	assertFileContents(t, output.logPath, "")
}

func TestCaptureDoesNotPublishQuarantinedLogForCandidateRenderMismatch(t *testing.T) {
	t.Parallel()

	jobWatcher := watch.NewRaceFreeFake()
	podWatcher := watch.NewRaceFreeFake()
	client := &fakeResourceClient{
		jobList:     &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}},
		podList:     &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher:  jobWatcher,
		podWatcher:  podWatcher,
		logAttempts: []logAttempt{{stream: io.NopCloser(strings.NewReader("untrusted hook output\n"))}},
		logStarted:  make(chan struct{}, 1),
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- capture(ctx, client, testCaptureConfig(), output) }()

	waitForFileContents(t, output.ready.path, "ready\n")
	job := validJob()
	podWatcher.Add(validPod(job.UID))
	select {
	case <-client.logStarted:
	case <-time.After(time.Second):
		t.Fatal("capture did not start the quarantined Pod log stream promptly")
	}
	waitForFileContents(t, output.quarantinePath, "untrusted hook output\n")
	job.Spec.Template.Spec.Containers[0].Args[15] = "--controller-replicas=3"
	jobWatcher.Add(job)

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "candidate render") {
			t.Fatalf("capture error = %v, want candidate render mismatch", err)
		}
		output.reportFailure(err)
	case <-ctx.Done():
		t.Fatalf("capture did not reject the drifted Job: %v", ctx.Err())
	}
	assertFileContents(t, output.logPath, "")
	assertFileContents(t, output.status.path, "failed\n")
}

func TestValidateJobAgainstRenderRejectsExecutionAndMetadataDrift(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*batchv1.Job){
		"image": func(job *batchv1.Job) {
			image := "ghcr.io/stokaro/other@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			job.Spec.Template.Spec.Containers[0].Image = image
			job.Spec.Template.Spec.Containers[0].Args[18] = "--manager-image=" + image
		},
		"trailing argument": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Args[15] = "--controller-replicas=3"
		},
		"delete policy": func(job *batchv1.Job) {
			job.Annotations[hookDeleteAnnotation] = "hook-succeeded"
		},
		"service account": func(job *batchv1.Job) {
			job.Spec.Template.Spec.ServiceAccountName = "other"
		},
		"security context": func(job *batchv1.Job) {
			value := int64(1000)
			job.Spec.Template.Spec.SecurityContext.RunAsUser = &value
		},
		"projected volume": func(job *batchv1.Job) {
			value := int64(7200)
			job.Spec.Template.Spec.Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = &value
		},
		"host network": func(job *batchv1.Job) {
			job.Spec.Template.Spec.HostNetwork = true
		},
		"host PID": func(job *batchv1.Job) {
			job.Spec.Template.Spec.HostPID = true
		},
		"unknown scheduling field": func(job *batchv1.Job) {
			job.Spec.Template.Spec.NodeSelector = map[string]string{"untrusted.example/node": "true"}
		},
		"Pod failure policy": func(job *batchv1.Job) {
			job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{}
		},
		"success policy": func(job *batchv1.Job) {
			job.Spec.SuccessPolicy = &batchv1.SuccessPolicy{}
		},
		"backoff limit per index": func(job *batchv1.Job) {
			value := int32(0)
			job.Spec.BackoffLimitPerIndex = &value
		},
		"maximum failed indexes": func(job *batchv1.Job) {
			value := int32(0)
			job.Spec.MaxFailedIndexes = &value
		},
		"TTL after finish": func(job *batchv1.Job) {
			value := int32(0)
			job.Spec.TTLSecondsAfterFinished = &value
		},
		"custom manager": func(job *batchv1.Job) {
			value := "untrusted.example/controller"
			job.Spec.ManagedBy = &value
		},
		"Pod replacement policy": func(job *batchv1.Job) {
			value := batchv1.Failed
			job.Spec.PodReplacementPolicy = &value
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			observed := validJob()
			mutate(observed)
			if err := validateJobAgainstRender(observed, validRenderedJob()); err == nil {
				t.Fatal("validateJobAgainstRender accepted candidate drift")
			}
		})
	}
}

func TestValidateJobAgainstRenderNormalizesAPIDefaultsAndDynamicMetadata(t *testing.T) {
	t.Parallel()

	observed := validJob()
	one := int32(1)
	falseValue := false
	completionMode := batchv1.NonIndexedCompletion
	observed.Spec.Completions = &one
	observed.Spec.Parallelism = &one
	observed.Spec.ManualSelector = &falseValue
	observed.Spec.Suspend = &falseValue
	observed.Spec.CompletionMode = &completionMode
	observed.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	observed.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName
	terminationGracePeriod := int64(corev1.DefaultTerminationGracePeriodSeconds)
	observed.Spec.Template.Spec.TerminationGracePeriodSeconds = &terminationGracePeriod
	enableServiceLinks := corev1.DefaultEnableServiceLinks
	observed.Spec.Template.Spec.EnableServiceLinks = &enableServiceLinks
	observed.Spec.Template.Spec.DeprecatedServiceAccount = observed.Spec.Template.Spec.ServiceAccountName
	observed.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(observed.UID)
	observed.Spec.Template.Labels[batchv1.JobNameLabel] = observed.Name
	observed.Spec.Template.Labels["controller-uid"] = string(observed.UID)
	observed.Spec.Template.Labels["job-name"] = observed.Name
	observed.ResourceVersion = "42"
	managedBy := batchv1.JobControllerName
	observed.Spec.ManagedBy = &managedBy
	replacementPolicy := batchv1.TerminatingOrFailed
	observed.Spec.PodReplacementPolicy = &replacementPolicy

	if err := validateJobAgainstRender(observed, validRenderedJob()); err != nil {
		t.Fatalf("validateJobAgainstRender rejected normalized API defaults: %v", err)
	}
}

func TestValidateJobAgainstRenderRejectsSelectorDrift(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*batchv1.Job){
		"missing": func(job *batchv1.Job) { job.Spec.Selector = nil },
		"wrong UID": func(job *batchv1.Job) {
			job.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = "different-uid"
		},
		"extra label": func(job *batchv1.Job) {
			job.Spec.Selector.MatchLabels[batchv1.JobNameLabel] = job.Name
		},
		"match expression": func(job *batchv1.Job) {
			job.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{
				Key: batchv1.ControllerUidLabel, Operator: metav1.LabelSelectorOpExists,
			}}
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			observed := validJob()
			mutate(observed)
			if err := validateJobAgainstRender(observed, validRenderedJob()); err == nil {
				t.Fatal("validateJobAgainstRender accepted malformed API-generated selector")
			}
		})
	}
}

func TestValidateRenderedJobRequiresAPIGeneratedSelector(t *testing.T) {
	t.Parallel()

	t.Run("rendered selector", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"candidate": "selector"}}
		if err := validateRenderedJob(rendered, testCaptureConfig()); err == nil {
			t.Fatal("validateRenderedJob accepted a rendered selector")
		}
	})
	t.Run("manual selector", func(t *testing.T) {
		rendered := validRenderedJob()
		value := true
		rendered.Spec.ManualSelector = &value
		if err := validateRenderedJob(rendered, testCaptureConfig()); err == nil {
			t.Fatal("validateRenderedJob accepted manualSelector=true")
		}
	})
	t.Run("template finalizer", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Template.Finalizers = []string{"untrusted.example/finalizer"}
		if err := validateRenderedJob(rendered, testCaptureConfig()); err == nil {
			t.Fatal("validateRenderedJob accepted a template finalizer")
		}
	})
}

func TestValidateJobAgainstRenderRejectsConflictingDeprecatedServiceAccount(t *testing.T) {
	t.Parallel()

	observed := validJob()
	observed.Spec.Template.Spec.DeprecatedServiceAccount = "different-account"
	if err := validateJobAgainstRender(observed, validRenderedJob()); err == nil {
		t.Fatal("validateJobAgainstRender accepted a conflicting deprecated service account alias")
	}
}

func TestValidatePodAgainstRenderRejectsFullPodSpecDrift(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*corev1.Pod){
		"host network": func(pod *corev1.Pod) { pod.Spec.HostNetwork = true },
		"host PID":     func(pod *corev1.Pod) { pod.Spec.HostPID = true },
		"assigned node": func(pod *corev1.Pod) {
			pod.Spec.NodeName = "worker-1"
		},
		"extra image pull secret": func(pod *corev1.Pod) {
			pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "injected"}}
		},
		"zero not-ready toleration": func(pod *corev1.Pod) {
			value := int64(0)
			pod.Spec.Tolerations[0].TolerationSeconds = &value
		},
		"301-second unreachable toleration": func(pod *corev1.Pod) {
			value := int64(301)
			pod.Spec.Tolerations[1].TolerationSeconds = &value
		},
		"wrong priority": func(pod *corev1.Pod) {
			value := int32(1)
			pod.Spec.Priority = &value
		},
		"wrong preemption policy": func(pod *corev1.Pod) {
			value := corev1.PreemptNever
			pod.Spec.PreemptionPolicy = &value
		},
		"wrong generateName": func(pod *corev1.Pod) {
			pod.GenerateName = "different-"
		},
		"wrong generated name prefix": func(pod *corev1.Pod) {
			pod.Name = "different-bcdgh"
		},
		"wrong generated name suffix": func(pod *corev1.Pod) {
			pod.Name = testJobName + "-aaaaa"
		},
		"missing finalizer": func(pod *corev1.Pod) {
			pod.Finalizers = nil
		},
		"extra finalizer": func(pod *corev1.Pod) {
			pod.Finalizers = append(pod.Finalizers, "untrusted.example/finalizer")
		},
		"runtime class": func(pod *corev1.Pod) {
			value := "untrusted"
			pod.Spec.RuntimeClassName = &value
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pod := validPod(types.UID("job-uid"))
			mutate(pod)
			if err := validatePod(pod, testCaptureConfig()); err == nil {
				t.Fatal("validatePod accepted PodSpec or generated-metadata drift")
			}
		})
	}
}

func TestValidatePodAgainstRenderNormalizesRuntimeAssignedDefaults(t *testing.T) {
	t.Parallel()

	pod := validPod(types.UID("job-uid"))
	pod.Spec.DNSPolicy = corev1.DNSClusterFirst
	pod.Spec.SchedulerName = corev1.DefaultSchedulerName
	terminationGracePeriod := int64(corev1.DefaultTerminationGracePeriodSeconds)
	pod.Spec.TerminationGracePeriodSeconds = &terminationGracePeriod
	enableServiceLinks := corev1.DefaultEnableServiceLinks
	pod.Spec.EnableServiceLinks = &enableServiceLinks
	pod.Spec.DeprecatedServiceAccount = pod.Spec.ServiceAccountName

	if err := validatePodAgainstRender(pod, validRenderedJob()); err != nil {
		t.Fatalf("validatePodAgainstRender rejected documented runtime defaults: %v", err)
	}
	if err := validatePodOwner(pod, validJob()); err != nil {
		t.Fatalf("validatePodOwner rejected the redundant service account alias: %v", err)
	}
}

func TestValidatePodAgainstRenderBindsConfiguredAdmissionDefaults(t *testing.T) {
	t.Parallel()

	t.Run("toleration seconds", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Template.Spec.Containers[0].Args[19] = managerArgumentPrefixes[19] +
			encodedTestControllerRuntimeArguments(true, 17, 23, false)
		pod := validPod(types.UID("job-uid"))
		pod.Spec.Containers[0].Args[19] = rendered.Spec.Template.Spec.Containers[0].Args[19]
		notReady := int64(17)
		unreachable := int64(23)
		pod.Spec.Tolerations[0].TolerationSeconds = &notReady
		pod.Spec.Tolerations[1].TolerationSeconds = &unreachable
		if err := validatePodAgainstRender(pod, rendered); err != nil {
			t.Fatalf("validatePodAgainstRender rejected exact configured tolerations: %v", err)
		}
	})

	t.Run("priority class", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Template.Spec.PriorityClassName = "hook-critical"
		rendered.Spec.Template.Spec.Containers[0].Args[23] = managerArgumentPrefixes[23] +
			encodedTestRuntimeAdmissionContract("hook-critical", 1000, corev1.PreemptNever, nil)
		pod := validPod(types.UID("job-uid"))
		pod.Spec.Containers[0].Args[23] = rendered.Spec.Template.Spec.Containers[0].Args[23]
		pod.Spec.PriorityClassName = "hook-critical"
		priority := int32(1000)
		pod.Spec.Priority = &priority
		policy := corev1.PreemptNever
		pod.Spec.PreemptionPolicy = &policy
		if err := validatePodAgainstRender(pod, rendered); err != nil {
			t.Fatalf("validatePodAgainstRender rejected exact configured priority: %v", err)
		}
	})

	t.Run("always pull images", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Template.Spec.Containers[0].Args[19] = managerArgumentPrefixes[19] +
			encodedTestControllerRuntimeArguments(true, 300, 300, true)
		pod := validPod(types.UID("job-uid"))
		pod.Spec.Containers[0].Args[19] = rendered.Spec.Template.Spec.Containers[0].Args[19]
		if err := validatePodAgainstRender(pod, rendered); err == nil {
			t.Fatal("validatePodAgainstRender accepted an unmodified pull policy with AlwaysPullImages enabled")
		}
		pod.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
		if err := validatePodAgainstRender(pod, rendered); err != nil {
			t.Fatalf("validatePodAgainstRender rejected the exact AlwaysPullImages mutation: %v", err)
		}
	})
}

func TestValidateRenderedJobRejectsMismatchedAdmissionContract(t *testing.T) {
	t.Parallel()

	t.Run("different named class", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Template.Spec.PriorityClassName = "hook-critical"
		rendered.Spec.Template.Spec.Containers[0].Args[23] = managerArgumentPrefixes[23] +
			encodedTestRuntimeAdmissionContract("different-class", 1000, corev1.PreemptNever, nil)
		if err := validateRenderedJob(rendered, testCaptureConfig()); err == nil {
			t.Fatal("validateRenderedJob accepted a mismatched priority admission contract")
		}
	})

	t.Run("named contract for unclassified hook", func(t *testing.T) {
		rendered := validRenderedJob()
		rendered.Spec.Template.Spec.Containers[0].Args[23] = managerArgumentPrefixes[23] +
			encodedTestRuntimeAdmissionContract("runtime-only", 1000, corev1.PreemptNever, nil)
		if err := validateRenderedJob(rendered, testCaptureConfig()); err == nil {
			t.Fatal("validateRenderedJob accepted a named contract for an unclassified hook template")
		}
	})
}

func TestPodComparisonsRejectConflictingDeprecatedServiceAccount(t *testing.T) {
	t.Parallel()

	job := validJob()
	pod := validPod(job.UID)
	pod.Spec.DeprecatedServiceAccount = "different-account"
	if err := validatePodAgainstRender(pod, validRenderedJob()); err == nil {
		t.Fatal("validatePodAgainstRender accepted a conflicting deprecated service account alias")
	}
	if err := validatePodOwner(pod, job); err == nil {
		t.Fatal("validatePodOwner accepted a conflicting deprecated service account alias")
	}
}

func TestValidateJobRejectsContractDrift(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*batchv1.Job){
		"missing UID":       func(job *batchv1.Job) { job.UID = "" },
		"wrong hook":        func(job *batchv1.Job) { job.Annotations[hookAnnotation] = "pre-upgrade" },
		"wrong weight":      func(job *batchv1.Job) { job.Annotations[hookWeightAnnotation] = "00" },
		"wrong deletion":    func(job *batchv1.Job) { job.Annotations[hookDeleteAnnotation] = "hook-succeeded" },
		"wrong component":   func(job *batchv1.Job) { job.Labels[componentLabel] = "controller" },
		"wrong deadline":    func(job *batchv1.Job) { *job.Spec.ActiveDeadlineSeconds = 209 },
		"retrying Job":      func(job *batchv1.Job) { *job.Spec.BackoffLimit = 1 },
		"wrong account":     func(job *batchv1.Job) { job.Spec.Template.Spec.ServiceAccountName = "other" },
		"unexpected volume": func(job *batchv1.Job) { job.Spec.Template.Spec.Volumes = nil },
		"extra container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{Name: "extra"})
		},
		"wrong command":   func(job *batchv1.Job) { job.Spec.Template.Spec.Containers[0].Command[0] = "/bin/sh" },
		"wrong first arg": func(job *batchv1.Job) { job.Spec.Template.Spec.Containers[0].Args[0] = "preflight" },
		"init container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "injected"}}
		},
		"wrong api version": func(job *batchv1.Job) { job.APIVersion = "batch/v1beta1" },
		"generated name":    func(job *batchv1.Job) { job.GenerateName = "untrusted-" },
		"owner reference": func(job *batchv1.Job) {
			job.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"}}
		},
		"Job finalizer": func(job *batchv1.Job) {
			job.Finalizers = []string{"untrusted.example/finalizer"}
		},
		"deletion timestamp": func(job *batchv1.Job) {
			value := metav1.NewTime(time.Now())
			job.DeletionTimestamp = &value
		},
		"deletion grace period": func(job *batchv1.Job) {
			value := int64(0)
			job.DeletionGracePeriodSeconds = &value
		},
		"template finalizer": func(job *batchv1.Job) {
			job.Spec.Template.Finalizers = []string{"untrusted.example/finalizer"}
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job := validJob()
			mutate(job)
			if err := validateJob(job, testCaptureConfig()); err == nil {
				t.Fatal("validateJob accepted a drifted Job")
			}
		})
	}
}

func TestValidatePodRequiresExactControllingJobUID(t *testing.T) {
	t.Parallel()

	job := validJob()
	pod := validPod(types.UID("different-job-uid"))
	if err := validatePod(pod, testCaptureConfig()); err != nil {
		t.Fatalf("validatePod rejected structurally valid Pod: %v", err)
	}
	if err := validatePodOwner(pod, job); err == nil {
		t.Fatal("validatePodOwner accepted a different Job UID")
	}
}

func TestValidatePodRequiresControllerUIDLabel(t *testing.T) {
	t.Parallel()

	pod := validPod(types.UID("job-uid"))
	pod.Labels[batchv1.ControllerUidLabel] = "different-job-uid"
	if err := validatePod(pod, testCaptureConfig()); err == nil {
		t.Fatal("validatePod accepted a controller UID label that differs from its owner")
	}
}

func TestValidatePodOwnerRequiresObservedTemplateExecutionContract(t *testing.T) {
	t.Parallel()

	job := validJob()
	pod := validPod(job.UID)
	pod.Spec.Containers[0].Image = "ghcr.io/stokaro/other@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pod.Spec.Containers[0].Args[18] = "--manager-image=" + pod.Spec.Containers[0].Image
	config := testCaptureConfig()
	config.expectedJob.Spec.Template.Spec.Containers[0].Image = pod.Spec.Containers[0].Image
	config.expectedJob.Spec.Template.Spec.Containers[0].Args[18] = pod.Spec.Containers[0].Args[18]
	if err := validatePod(pod, config); err != nil {
		t.Fatalf("validatePod rejected the independently self-consistent Pod: %v", err)
	}
	if err := validatePodOwner(pod, job); err == nil || !strings.Contains(err.Error(), "execution contract differs") {
		t.Fatalf("validatePodOwner error = %v, want template execution mismatch", err)
	}
}

func TestValidatePodRequiresOneBlockingControllerOwner(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*corev1.Pod){
		"additional owner": func(pod *corev1.Pod) {
			pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "extra", UID: "extra"})
		},
		"controller nil": func(pod *corev1.Pod) { pod.OwnerReferences[0].Controller = nil },
		"controller false": func(pod *corev1.Pod) {
			value := false
			pod.OwnerReferences[0].Controller = &value
		},
		"block deletion nil": func(pod *corev1.Pod) { pod.OwnerReferences[0].BlockOwnerDeletion = nil },
		"block deletion false": func(pod *corev1.Pod) {
			value := false
			pod.OwnerReferences[0].BlockOwnerDeletion = &value
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pod := validPod(types.UID("job-uid"))
			mutate(pod)
			if err := validatePod(pod, testCaptureConfig()); err == nil {
				t.Fatal("validatePod accepted an invalid owner chain")
			}
		})
	}
}

func TestTypedObjectsMayHaveEmptyTypeMeta(t *testing.T) {
	t.Parallel()

	job := validJob()
	pod := validPod(job.UID)
	if !job.GetObjectKind().GroupVersionKind().Empty() || !pod.GetObjectKind().GroupVersionKind().Empty() {
		t.Fatal("test objects unexpectedly have TypeMeta")
	}
	if err := validateJob(job, testCaptureConfig()); err != nil {
		t.Fatalf("validateJob rejected a typed batch/v1 object with empty TypeMeta: %v", err)
	}
	if err := validatePod(pod, testCaptureConfig()); err != nil {
		t.Fatalf("validatePod rejected a typed core/v1 object with empty TypeMeta: %v", err)
	}
}

func TestWatchEventHandlingIsFailClosed(t *testing.T) {
	t.Parallel()

	job := validJob()
	pod := validPod(job.UID)
	status := &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReasonExpired,
		Code:     410,
	}

	if _, err := handleJobEvent(watch.Event{Type: watch.Modified, Object: job}, testCaptureConfig(), nil); err == nil {
		t.Fatal("Job Modified event was accepted before ADDED")
	}
	if _, err := handleJobEvent(watch.Event{Type: watch.Deleted, Object: job}, testCaptureConfig(), nil); err == nil {
		t.Fatal("Job Deleted event was accepted before ADDED")
	}
	if _, err := handleJobEvent(watch.Event{Type: watch.Error, Object: status}, testCaptureConfig(), nil); err == nil {
		t.Fatal("Job Error event was accepted")
	}
	if next, err := handleJobEvent(watch.Event{Type: watch.Modified, Object: job.DeepCopy()}, testCaptureConfig(), job); err != nil || next != nil {
		t.Fatalf("Job Modified event after ADDED = (%v, %v)", next, err)
	}
	if next, err := handleJobEvent(watch.Event{Type: watch.Deleted, Object: job.DeepCopy()}, testCaptureConfig(), job); err != nil || next != nil {
		t.Fatalf("Job Deleted event after ADDED = (%v, %v)", next, err)
	}

	if _, err := handlePodEvent(watch.Event{Type: watch.Modified, Object: pod}, testCaptureConfig(), nil); err == nil {
		t.Fatal("Pod Modified event was accepted before ADDED")
	}
	if _, err := handlePodEvent(watch.Event{Type: watch.Deleted, Object: pod}, testCaptureConfig(), nil); err == nil {
		t.Fatal("Pod Deleted event was accepted before ADDED")
	}
	if _, err := handlePodEvent(watch.Event{Type: watch.Error, Object: status}, testCaptureConfig(), nil); err == nil {
		t.Fatal("Pod Error event was accepted")
	}
	if next, err := handlePodEvent(watch.Event{Type: watch.Modified, Object: pod.DeepCopy()}, testCaptureConfig(), pod); err != nil || next != nil {
		t.Fatalf("Pod Modified event after ADDED = (%v, %v)", next, err)
	}
	if next, err := handlePodEvent(watch.Event{Type: watch.Deleted, Object: pod.DeepCopy()}, testCaptureConfig(), pod); err != nil || next != nil {
		t.Fatalf("Pod Deleted event after ADDED = (%v, %v)", next, err)
	}
}

func TestCaptureStopsBothWatchesAtDeadline(t *testing.T) {
	t.Parallel()

	jobWatcher := watch.NewRaceFreeFake()
	podWatcher := watch.NewRaceFreeFake()
	client := &fakeResourceClient{
		jobList:    &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}},
		podList:    &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher: jobWatcher,
		podWatcher: podWatcher,
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := capture(ctx, client, testCaptureConfig(), output)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture error = %v, want deadline exceeded", err)
	}
	if !jobWatcher.IsStopped() || !podWatcher.IsStopped() {
		t.Fatal("capture did not stop both watches at its deadline")
	}
}

func TestCaptureJoinsActiveLogStreamOnSignalCancellation(t *testing.T) {
	t.Parallel()

	jobWatcher := watch.NewRaceFreeFake()
	podWatcher := watch.NewRaceFreeFake()
	logReader, logWriter := io.Pipe()
	defer func() { _ = logWriter.Close() }()
	job := validJob()
	client := &fakeResourceClient{
		jobList:     &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}},
		podList:     &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}},
		jobWatcher:  jobWatcher,
		podWatcher:  podWatcher,
		logAttempts: []logAttempt{{stream: logReader}},
		logStarted:  make(chan struct{}, 1),
	}
	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- capture(ctx, client, testCaptureConfig(), output) }()

	waitForFileContents(t, output.ready.path, "ready\n")
	jobWatcher.Add(job)
	podWatcher.Add(validPod(job.UID))
	select {
	case <-client.logStarted:
	case <-time.After(time.Second):
		t.Fatal("log stream did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("capture error = %v, want signal-style cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not join the active log stream promptly")
	}
	if !jobWatcher.IsStopped() || !podWatcher.IsStopped() {
		t.Fatal("capture did not stop both watches after cancellation")
	}
}

func TestCapturePodLogRetriesOnlyPermittedStartupErrors(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{logAttempts: []logAttempt{
		{err: apierrors.NewNotFound(schema.GroupResource{Resource: "pods/log"}, "hook-pod")},
		{err: apierrors.NewBadRequest(`pod hook-pod does not have a host assigned`)},
		{err: apierrors.NewBadRequest(`container "crd-manager" in pod "hook-pod" is not available`)},
		{err: apierrors.NewBadRequest(`container "crd-manager" in pod "hook-pod" is waiting to start - no logs yet`)},
		{err: apierrors.NewBadRequest(`container "crd-manager" in pod "hook-pod" is waiting to start: PodInitializing`)},
		{err: apierrors.NewBadRequest(`container "crd-manager" in pod "hook-pod" is waiting to start: ContainerCreating`)},
		{stream: io.NopCloser(strings.NewReader("captured\n"))},
	}}
	config := testCaptureConfig()
	config.logRetryInterval = time.Millisecond
	var destination strings.Builder

	written, err := capturePodLog(context.Background(), client, config, "hook-pod", &destination)
	if err != nil {
		t.Fatalf("capturePodLog returned an error: %v", err)
	}
	if written != int64(len("captured\n")) || destination.String() != "captured\n" {
		t.Fatalf("captured %d bytes %q", written, destination.String())
	}
}

func TestCapturePodLogAcceptsBytesBeforeDeletedStreamError(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{logAttempts: []logAttempt{{
		stream: &dataThenErrorReader{data: []byte("failure details\n"), err: errors.New("stream ended after Pod deletion")},
	}}}
	var destination strings.Builder
	written, err := capturePodLog(context.Background(), client, testCaptureConfig(), "hook-pod", &destination)
	if err != nil {
		t.Fatalf("capturePodLog returned an error after nonempty bytes: %v", err)
	}
	if written == 0 || destination.String() != "failure details\n" {
		t.Fatalf("captured %d bytes %q", written, destination.String())
	}
}

func TestCapturePodLogRejectsDestinationErrorAfterPartialWrite(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{logAttempts: []logAttempt{{
		stream: io.NopCloser(strings.NewReader("failure details\n")),
	}}}
	destination := &partialFailureWriter{err: errors.New("disk full")}
	written, err := capturePodLog(context.Background(), client, testCaptureConfig(), "hook-pod", destination)
	if err == nil || !strings.Contains(err.Error(), "write hook log destination") {
		t.Fatalf("capturePodLog = (%d, %v), want destination failure", written, err)
	}
}

func TestCapturePodLogRejectsOtherStartupErrors(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{logAttempts: []logAttempt{{
		err: apierrors.NewBadRequest("container configuration is invalid"),
	}}}
	var destination strings.Builder
	_, err := capturePodLog(context.Background(), client, testCaptureConfig(), "hook-pod", &destination)
	if err == nil || !strings.Contains(err.Error(), "start hook Pod log stream") {
		t.Fatalf("capturePodLog error = %v", err)
	}
}

func TestCapturePodLogRejectsStartupErrorForAnotherPod(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{logAttempts: []logAttempt{{
		err: apierrors.NewBadRequest(`container "crd-manager" in pod "other-pod" is waiting to start - no logs yet`),
	}}}
	var destination strings.Builder
	_, err := capturePodLog(context.Background(), client, testCaptureConfig(), "hook-pod", &destination)
	if err == nil || !strings.Contains(err.Error(), "start hook Pod log stream") {
		t.Fatalf("capturePodLog error = %v, want fail-closed pod scoping", err)
	}
}

func TestCapturePodLogBoundsTransientStartupRetries(t *testing.T) {
	t.Parallel()

	client := &fakeResourceClient{
		repeatLogError: apierrors.NewNotFound(schema.GroupResource{Resource: "pods/log"}, "hook-pod"),
	}
	config := testCaptureConfig()
	config.logStartTimeout = 20 * time.Millisecond
	config.logRetryInterval = time.Millisecond
	var destination strings.Builder

	_, err := capturePodLog(context.Background(), client, config, "hook-pod", &destination)
	if !errors.Is(err, errLogStartTimeout) {
		t.Fatalf("capturePodLog error = %v, want bounded startup timeout", err)
	}
}

func TestCopyBoundedClosesStreamOnCancellation(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := copyBounded(ctx, io.Discard, reader, 1024)
		result <- err
	}()
	cancel()
	defer func() { _ = writer.Close() }()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("copyBounded did not stop promptly after cancellation")
	}
}

func TestCopyBoundedRejectsOversizedLog(t *testing.T) {
	t.Parallel()

	reader := io.NopCloser(strings.NewReader("12345"))
	var destination strings.Builder
	written, err := copyBounded(context.Background(), &destination, reader, 4)
	if !errors.Is(err, errLogTooLarge) {
		t.Fatalf("copyBounded error = %v, want errLogTooLarge", err)
	}
	if written != 4 || destination.String() != "1234" {
		t.Fatalf("copyBounded wrote %d bytes %q", written, destination.String())
	}
}

type dataThenErrorReader struct {
	data []byte
	err  error
}

func (reader *dataThenErrorReader) Read(destination []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, reader.err
	}
	written := copy(destination, reader.data)
	reader.data = reader.data[written:]
	if len(reader.data) == 0 {
		return written, reader.err
	}
	return written, nil
}

func (*dataThenErrorReader) Close() error { return nil }

type partialFailureWriter struct {
	err error
}

func (writer *partialFailureWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, writer.err
	}
	return 1, writer.err
}

func testCaptureConfig() captureConfig {
	return captureConfig{
		namespace:        testNamespace,
		jobName:          testJobName,
		expectedJob:      validRenderedJob(),
		logStartTimeout:  time.Second,
		logRetryInterval: 5 * time.Millisecond,
		maxLogBytes:      1024,
	}
}

func validRenderedJob() *batchv1.Job {
	job := validJob()
	job.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
	job.UID = ""
	job.Spec.Selector = nil
	return job
}

func validJob() *batchv1.Job {
	backoffLimit := int32(0)
	activeDeadline := int64(210)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      testJobName,
			UID:       types.UID("job-uid"),
			Annotations: map[string]string{
				hookAnnotation:       "pre-install,pre-upgrade",
				hookWeightAnnotation: "0",
				hookDeleteAnnotation: "before-hook-creation,hook-succeeded,hook-failed",
			},
			Labels: map[string]string{componentLabel: managerComponent, managedByLabel: "Helm"},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &activeDeadline,
			BackoffLimit:          &backoffLimit,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				batchv1.ControllerUidLabel: "job-uid",
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{componentLabel: managerComponent, managedByLabel: "Helm"}},
				Spec:       validPodSpec(),
			},
		},
	}
}

func validPod(jobUID types.UID) *corev1.Pod {
	controller := true
	blockOwnerDeletion := true
	spec := validPodSpec()
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	tolerationSeconds := int64(300)
	spec.Priority = &priority
	spec.PreemptionPolicy = &preemption
	spec.Tolerations = []corev1.Toleration{
		{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &tolerationSeconds},
		{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &tolerationSeconds},
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    testNamespace,
			Name:         testJobName + "-bcdgh",
			GenerateName: testJobName + "-",
			UID:          types.UID("pod-uid"),
			Finalizers:   []string{batchv1.JobTrackingFinalizer},
			Labels: map[string]string{
				batchv1.JobNameLabel:       testJobName,
				batchv1.ControllerUidLabel: string(jobUID),
				componentLabel:             managerComponent,
				managedByLabel:             "Helm",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               testJobName,
				UID:                jobUID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: spec,
	}
}

func validPodSpec() corev1.PodSpec {
	runAsNonRoot := true
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	automount := false
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	defaultMode := int32(0o644)
	expirationSeconds := int64(3600)
	return corev1.PodSpec{
		ServiceAccountName:           testJobName,
		AutomountServiceAccountToken: &automount,
		RestartPolicy:                corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &runAsNonRoot,
			RunAsUser:    &runAsUser,
			RunAsGroup:   &runAsGroup,
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{{
			Name:                     managerComponent,
			Image:                    testImage,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			Command:                  []string{managerCommand},
			Args:                     validManagerArguments(),
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &allowPrivilegeEscalation,
				ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      apiAccessVolume,
				MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
				ReadOnly:  true,
			}},
		}},
		Volumes: []corev1.Volume{{
			Name: apiAccessVolume,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: &defaultMode,
					Sources: []corev1.VolumeProjection{
						{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &expirationSeconds}},
						{ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
							Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
						}},
						{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
							Path: "namespace",
							FieldRef: &corev1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.namespace",
							},
						}}}},
					},
				},
			},
		}},
	}
}

func validManagerArguments() []string {
	return []string{
		managerMode,
		"--timeout=180s",
		"--release-name=ptah",
		"--release-namespace=" + testNamespace,
		"--coordination-namespace=" + testNamespace,
		"--leader-election=true",
		"--leader-election-id=ptah-operator.operator.ptah.dev",
		"--webhook-service-name=ptah-webhook",
		"--webhook-timeout-seconds=10",
		"--webhook-secret-name=ptah-webhook-cert",
		"--webhook-port=9443",
		"--certificate-health-port=8081",
		"--hook-service-account-name=" + testJobName,
		"--controller-service-account-name=ptah",
		"--controller-deployment-name=ptah",
		"--controller-replicas=2",
		"--certificate-deployment-name=ptah-cert-rotator",
		"--release-sequence=2",
		"--manager-image=" + testImage,
		managerArgumentPrefixes[19] + encodedTestControllerRuntimeArguments(true, 300, 300, false),
		"--certificate-runtime-args-b64=W10=",
		"--runtime-deployment-config-expressions-b64=W10=",
		"--runtime-pod-config-expressions-b64=W10=",
		managerArgumentPrefixes[23] + encodedTestRuntimeAdmissionContract("", 0, corev1.PreemptLowerPriority, nil),
	}
}

func encodedTestControllerRuntimeArguments(enabled bool, notReady, unreachable int64, alwaysPull bool) string {
	return encodedTestJSON([]string{
		fmt.Sprintf("--default-tolerations-enabled=%t", enabled),
		fmt.Sprintf("--default-not-ready-toleration-seconds=%d", notReady),
		fmt.Sprintf("--default-unreachable-toleration-seconds=%d", unreachable),
		fmt.Sprintf("--always-pull-images-enabled=%t", alwaysPull),
	})
}

func encodedTestRuntimeAdmissionContract(
	priorityClassName string,
	priorityClassValue int32,
	preemptionPolicy corev1.PreemptionPolicy,
	imagePullSecrets []corev1.LocalObjectReference,
) string {
	return encodedTestJSON(renderedRuntimeAdmissionContract{
		Version:                       1,
		ImagePullSecrets:              imagePullSecrets,
		PriorityClassName:             priorityClassName,
		PriorityClassValue:            priorityClassValue,
		PriorityClassPreemptionPolicy: string(preemptionPolicy),
	})
}

func encodedTestJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal test runtime contract: %v", err))
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func newTestOutputs(t *testing.T) *captureOutputs {
	t.Helper()
	requirePrivateModeSemantics(t)
	directory := t.TempDir()
	output, err := prepareOutputs(outputPaths{
		log:    filepath.Join(directory, "capture.log"),
		status: filepath.Join(directory, "capture.status"),
		ready:  filepath.Join(directory, "capture.ready"),
		error:  filepath.Join(directory, "capture.error"),
	})
	if err != nil {
		t.Fatalf("prepareOutputs: %v", err)
	}
	return output
}

func waitForFileContents(t *testing.T, path, expected string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil && string(contents) == expected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertFileContents(t, path, expected)
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(contents) != expected {
		t.Fatalf("%s contents = %q, want %q", path, string(contents), expected)
	}
}
