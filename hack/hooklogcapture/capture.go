package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const (
	hookAnnotation               = "helm.sh/hook"
	hookWeightAnnotation         = "helm.sh/hook-weight"
	hookDeleteAnnotation         = "helm.sh/hook-delete-policy"
	componentLabel               = "app.kubernetes.io/component"
	managedByLabel               = "app.kubernetes.io/managed-by"
	managerCommand               = "/ptah-crd-manager"
	apiAccessVolume              = "api-access"
	generatedNameAlphabet        = "bcdfghjklmnpqrstvwxz2456789"
	generatedNameMaxPrefixLength = 58
	priorityClassListLimit       = int64(500)
)

type hookMode string

const (
	hookModePreflight hookMode = "preflight"
	hookModeReconcile hookMode = "reconcile"
)

type hookProfile struct {
	mode          hookMode
	component     string
	containerName string
	hookWeight    string
}

func profileForHookMode(mode hookMode) (hookProfile, error) {
	switch mode {
	case hookModePreflight:
		return hookProfile{
			mode:          mode,
			component:     "crd-manager-preflight",
			containerName: "crd-manager-preflight",
			hookWeight:    "-60",
		}, nil
	case hookModeReconcile:
		return hookProfile{
			mode:          mode,
			component:     "crd-manager",
			containerName: "crd-manager",
			hookWeight:    "0",
		}, nil
	default:
		return hookProfile{}, errors.New("hook mode must be exactly preflight or reconcile")
	}
}

func (profile hookProfile) serviceAccountName(jobName string) (string, error) {
	switch profile.mode {
	case hookModePreflight:
		const suffix = "-preflight"
		if !strings.HasSuffix(jobName, suffix) || len(jobName) == len(suffix) {
			return "", errors.New("preflight hook Job name does not bind its service account identity")
		}
		return strings.TrimSuffix(jobName, suffix), nil
	case hookModeReconcile:
		return jobName, nil
	default:
		return "", errors.New("hook mode must be exactly preflight or reconcile")
	}
}

var managerArgumentPrefixes = []string{
	"",
	"--timeout=",
	"--release-name=",
	"--release-namespace=",
	"--coordination-namespace=",
	"--leader-election=",
	"--leader-election-id=",
	"--webhook-service-name=",
	"--webhook-timeout-seconds=",
	"--webhook-secret-name=",
	"--webhook-port=",
	"--certificate-health-port=",
	"--hook-service-account-name=",
	"--controller-service-account-name=",
	"--controller-deployment-name=",
	"--controller-replicas=",
	"--certificate-deployment-name=",
	"--release-sequence=",
	"--manager-image=",
	"--controller-runtime-args-b64=",
	"--certificate-runtime-args-b64=",
	"--runtime-deployment-config-expressions-b64=",
	"--runtime-pod-config-expressions-b64=",
	"--runtime-admission-contract-b64=",
}

var (
	errLogTooLarge     = errors.New("hook log exceeds the configured size limit")
	errLogStartTimeout = errors.New("hook log stream did not become available before its deadline")
)

type priorityAdmissionDefaults struct {
	candidates []priorityAdmissionCandidate
}

type priorityAdmissionCandidate struct {
	name             string
	uid              types.UID
	value            int32
	preemptionPolicy corev1.PreemptionPolicy
}

type captureConfig struct {
	namespace                 string
	jobName                   string
	hookMode                  hookMode
	expectedJob               *batchv1.Job
	preflightPriority         priorityAdmissionDefaults
	preflightPriorityResolved bool
	logStartTimeout           time.Duration
	logRetryInterval          time.Duration
	maxLogBytes               int64
}

type resourceClient interface {
	listPriorityClasses(context.Context, metav1.ListOptions) (*schedulingv1.PriorityClassList, error)
	watchPriorityClasses(context.Context, metav1.ListOptions) (watch.Interface, error)
	listJobs(context.Context, string, metav1.ListOptions) (*batchv1.JobList, error)
	watchJobs(context.Context, string, metav1.ListOptions) (watch.Interface, error)
	listPods(context.Context, string, metav1.ListOptions) (*corev1.PodList, error)
	watchPods(context.Context, string, metav1.ListOptions) (watch.Interface, error)
	streamPodLogs(context.Context, string, string, string) (io.ReadCloser, error)
}

type kubernetesResourceClient struct {
	client kubernetes.Interface
}

func (client kubernetesResourceClient) listPriorityClasses(
	ctx context.Context,
	options metav1.ListOptions,
) (*schedulingv1.PriorityClassList, error) {
	return client.client.SchedulingV1().PriorityClasses().List(ctx, options)
}

func (client kubernetesResourceClient) watchPriorityClasses(
	ctx context.Context,
	options metav1.ListOptions,
) (watch.Interface, error) {
	return client.client.SchedulingV1().PriorityClasses().Watch(ctx, options)
}

func (client kubernetesResourceClient) listJobs(
	ctx context.Context,
	namespace string,
	options metav1.ListOptions,
) (*batchv1.JobList, error) {
	return client.client.BatchV1().Jobs(namespace).List(ctx, options)
}

func (client kubernetesResourceClient) watchJobs(
	ctx context.Context,
	namespace string,
	options metav1.ListOptions,
) (watch.Interface, error) {
	return client.client.BatchV1().Jobs(namespace).Watch(ctx, options)
}

func (client kubernetesResourceClient) listPods(
	ctx context.Context,
	namespace string,
	options metav1.ListOptions,
) (*corev1.PodList, error) {
	return client.client.CoreV1().Pods(namespace).List(ctx, options)
}

func (client kubernetesResourceClient) watchPods(
	ctx context.Context,
	namespace string,
	options metav1.ListOptions,
) (watch.Interface, error) {
	return client.client.CoreV1().Pods(namespace).Watch(ctx, options)
}

func (client kubernetesResourceClient) streamPodLogs(
	ctx context.Context,
	namespace string,
	podName string,
	containerName string,
) (io.ReadCloser, error) {
	return client.client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	}).Stream(ctx)
}

type logResult struct {
	bytes int64
	err   error
}

func capture(ctx context.Context, client resourceClient, config captureConfig, output *captureOutputs) error {
	if err := validateCaptureConfig(config); err != nil {
		return err
	}
	captureContext, cancelCapture := context.WithCancel(ctx)
	var activeLogResult <-chan logResult
	defer func() {
		cancelCapture()
		if activeLogResult != nil {
			// Both the client-go request and copyBounded close their stream on
			// cancellation, so this join cannot leave a log goroutine behind.
			<-activeLogResult
		}
	}()

	var priorityClasses map[string]schedulingv1.PriorityClass
	var priorityClassWatch watch.Interface
	var priorityClassEvents <-chan watch.Event
	if config.hookMode == hookModePreflight {
		defaults, inventory, watcher, err := establishPreflightPriorityContract(captureContext, client)
		if err != nil {
			return err
		}
		config.preflightPriority = defaults
		config.preflightPriorityResolved = true
		priorityClasses = inventory
		priorityClassWatch = watcher
		priorityClassEvents = watcher.ResultChan()
		defer priorityClassWatch.Stop()
	}

	jobSelector := fields.OneTermEqualSelector("metadata.name", config.jobName).String()
	jobList, err := client.listJobs(captureContext, config.namespace, metav1.ListOptions{FieldSelector: jobSelector})
	if err != nil {
		return fmt.Errorf("list exact hook Job: %w", err)
	}
	if len(jobList.Items) != 0 {
		return errors.New("exact hook Job already exists before capture watches are established")
	}
	if jobList.ResourceVersion == "" {
		return errors.New("exact hook Job list has an empty resourceVersion")
	}

	podSelector := labels.Set{batchv1.JobNameLabel: config.jobName}.AsSelector().String()
	podList, err := client.listPods(captureContext, config.namespace, metav1.ListOptions{LabelSelector: podSelector})
	if err != nil {
		return fmt.Errorf("list hook Pods: %w", err)
	}
	if len(podList.Items) != 0 {
		return errors.New("a label-selected hook Pod already exists before capture watches are established")
	}
	if podList.ResourceVersion == "" {
		return errors.New("hook Pod list has an empty resourceVersion")
	}

	jobWatch, err := client.watchJobs(captureContext, config.namespace, metav1.ListOptions{
		FieldSelector:       jobSelector,
		ResourceVersion:     jobList.ResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		return fmt.Errorf("watch exact hook Job: %w", err)
	}
	if jobWatch == nil || jobWatch.ResultChan() == nil {
		if jobWatch != nil {
			jobWatch.Stop()
		}
		return errors.New("exact hook Job watch did not provide an event channel")
	}
	defer jobWatch.Stop()

	podWatch, err := client.watchPods(captureContext, config.namespace, metav1.ListOptions{
		LabelSelector:       podSelector,
		ResourceVersion:     podList.ResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		return fmt.Errorf("watch hook Pods: %w", err)
	}
	if podWatch == nil || podWatch.ResultChan() == nil {
		if podWatch != nil {
			podWatch.Stop()
		}
		return errors.New("hook Pod watch did not provide an event channel")
	}
	defer podWatch.Stop()

	if err := drainPriorityClassEvents(priorityClassEvents, priorityClasses, config.preflightPriority); err != nil {
		return err
	}
	if err := output.setStatus(statusWatching); err != nil {
		return fmt.Errorf("write watching status: %w", err)
	}
	if err := output.markReady(); err != nil {
		return fmt.Errorf("write readiness marker: %w", err)
	}

	jobEvents := jobWatch.ResultChan()
	podEvents := podWatch.ResultChan()
	var observedJob *batchv1.Job
	var observedPod *corev1.Pod
	var logEvents <-chan logResult
	var completedLog *logResult

	for {
		if captureContext.Err() != nil {
			return captureContext.Err()
		}
		if observedJob != nil && observedPod != nil && completedLog != nil {
			if err := drainPriorityClassEvents(priorityClassEvents, priorityClasses, config.preflightPriority); err != nil {
				return err
			}
			if completedLog.err != nil {
				return completedLog.err
			}
			if completedLog.bytes == 0 {
				return errors.New("hook log stream completed without any bytes")
			}
			if err := output.publishLog(); err != nil {
				return fmt.Errorf("publish captured hook log: %w", err)
			}
			if captureContext.Err() != nil {
				return captureContext.Err()
			}
			if err := output.setStatus(statusCaptured); err != nil {
				return fmt.Errorf("write captured status: %w", err)
			}
			return nil
		}

		select {
		case <-captureContext.Done():
			return captureContext.Err()
		case event, open := <-priorityClassEvents:
			if !open {
				return errors.New("PriorityClass watch closed before capture completed")
			}
			if err := handlePriorityClassEvent(event, priorityClasses, config.preflightPriority); err != nil {
				return err
			}
		case event, open := <-jobEvents:
			if !open {
				return errors.New("exact hook Job watch closed before capture completed")
			}
			job, eventErr := handleJobEvent(event, config, observedJob)
			if eventErr != nil {
				return eventErr
			}
			if job == nil {
				continue
			}
			if observedJob == nil {
				observedJob = job
				if observedPod != nil {
					if err := validatePodOwner(observedPod, observedJob, config); err != nil {
						return err
					}
				}
				if err := output.setStatus(statusJobObserved); err != nil {
					return fmt.Errorf("write Job-observed status: %w", err)
				}
			}
		case event, open := <-podEvents:
			if !open {
				return errors.New("hook Pod watch closed before capture completed")
			}
			pod, eventErr := handlePodEvent(event, config, observedPod)
			if eventErr != nil {
				return eventErr
			}
			if pod == nil {
				continue
			}
			if observedPod == nil {
				observedPod = pod
				if observedJob != nil {
					if err := validatePodOwner(observedPod, observedJob, config); err != nil {
						return err
					}
				}
				if err := output.setStatus(statusPodObserved); err != nil {
					return fmt.Errorf("write Pod-observed status: %w", err)
				}
				if err := output.setStatus(statusStreaming); err != nil {
					return fmt.Errorf("write streaming status: %w", err)
				}
				if err := output.validateLogDestination(); err != nil {
					return fmt.Errorf("validate hook log destination before streaming: %w", err)
				}
				results := make(chan logResult, 1)
				logEvents = results
				activeLogResult = results
				go func(podName string) {
					bytes, streamErr := capturePodLog(captureContext, client, config, podName, output.log)
					results <- logResult{bytes: bytes, err: streamErr}
				}(pod.Name)
			}
		case result := <-logEvents:
			completedLog = &result
			logEvents = nil
			activeLogResult = nil
			if result.err != nil {
				return result.err
			}
		}
	}
}

func establishPreflightPriorityContract(
	ctx context.Context,
	client resourceClient,
) (priorityAdmissionDefaults, map[string]schedulingv1.PriorityClass, watch.Interface, error) {
	options := metav1.ListOptions{Limit: priorityClassListLimit}
	list, err := client.listPriorityClasses(ctx, options)
	if err != nil {
		return priorityAdmissionDefaults{}, nil, nil, fmt.Errorf("list PriorityClasses: %w", err)
	}
	if list == nil {
		return priorityAdmissionDefaults{}, nil, nil, errors.New("PriorityClass list returned a nil result")
	}
	if list.ResourceVersion == "" {
		return priorityAdmissionDefaults{}, nil, nil, errors.New("PriorityClass list has an empty resourceVersion")
	}
	if list.Continue != "" || list.RemainingItemCount != nil {
		return priorityAdmissionDefaults{}, nil, nil, errors.New("PriorityClass inventory exceeds its single-page consistency bound")
	}
	if int64(len(list.Items)) > priorityClassListLimit {
		return priorityAdmissionDefaults{}, nil, nil, errors.New("PriorityClass list exceeds the requested item limit")
	}

	inventory := make(map[string]schedulingv1.PriorityClass, len(list.Items))
	observedUIDs := make(map[types.UID]string, len(list.Items))
	for index := range list.Items {
		priorityClass := list.Items[index].DeepCopy()
		if err := validatePriorityClassObject(priorityClass); err != nil {
			return priorityAdmissionDefaults{}, nil, nil, fmt.Errorf("validate PriorityClass inventory item: %w", err)
		}
		if _, found := inventory[priorityClass.Name]; found {
			return priorityAdmissionDefaults{}, nil, nil, fmt.Errorf("PriorityClass inventory contains duplicate name %q", priorityClass.Name)
		}
		if previous, found := observedUIDs[priorityClass.UID]; found {
			return priorityAdmissionDefaults{}, nil, nil, fmt.Errorf(
				"PriorityClass inventory reuses one UID for %q and %q",
				previous,
				priorityClass.Name,
			)
		}
		inventory[priorityClass.Name] = *priorityClass
		observedUIDs[priorityClass.UID] = priorityClass.Name
	}
	defaults, err := resolveGlobalPriorityDefaults(inventory)
	if err != nil {
		return priorityAdmissionDefaults{}, nil, nil, err
	}

	priorityClassWatch, err := client.watchPriorityClasses(ctx, metav1.ListOptions{
		ResourceVersion:     list.ResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		return priorityAdmissionDefaults{}, nil, nil, fmt.Errorf("watch PriorityClasses: %w", err)
	}
	if priorityClassWatch == nil || priorityClassWatch.ResultChan() == nil {
		if priorityClassWatch != nil {
			priorityClassWatch.Stop()
		}
		return priorityAdmissionDefaults{}, nil, nil, errors.New("PriorityClass watch did not provide an event channel")
	}
	return defaults, inventory, priorityClassWatch, nil
}

func validatePriorityClassObject(priorityClass *schedulingv1.PriorityClass) error {
	if priorityClass == nil {
		return errors.New("PriorityClass is nil")
	}
	gvk := priorityClass.GetObjectKind().GroupVersionKind()
	if !gvk.Empty() && gvk != (schema.GroupVersionKind{Group: schedulingv1.GroupName, Version: "v1", Kind: "PriorityClass"}) {
		return fmt.Errorf("PriorityClass has unexpected apiVersion/kind %q", gvk.String())
	}
	if priorityClass.Namespace != "" || priorityClass.Name == "" || priorityClass.UID == "" || priorityClass.ResourceVersion == "" {
		return errors.New("PriorityClass cluster identity is incomplete")
	}
	if problems := validation.IsDNS1123Subdomain(priorityClass.Name); len(problems) != 0 {
		return fmt.Errorf("PriorityClass name is not a DNS subdomain: %s", strings.Join(problems, "; "))
	}
	if priorityClass.PreemptionPolicy != nil &&
		*priorityClass.PreemptionPolicy != corev1.PreemptLowerPriority &&
		*priorityClass.PreemptionPolicy != corev1.PreemptNever {
		return errors.New("PriorityClass has an invalid preemptionPolicy")
	}
	return nil
}

func resolveGlobalPriorityDefaults(
	inventory map[string]schedulingv1.PriorityClass,
) (priorityAdmissionDefaults, error) {
	minimumFound := false
	var minimum int32
	resolved := make([]priorityAdmissionCandidate, 0, 1)
	for name := range inventory {
		priorityClass := inventory[name]
		if !priorityClass.GlobalDefault {
			continue
		}
		if minimumFound && priorityClass.Value > minimum {
			continue
		}
		if !minimumFound || priorityClass.Value < minimum {
			minimumFound = true
			minimum = priorityClass.Value
			resolved = resolved[:0]
		}
		policy := corev1.PreemptLowerPriority
		if priorityClass.PreemptionPolicy != nil {
			policy = *priorityClass.PreemptionPolicy
		}
		resolved = append(resolved, priorityAdmissionCandidate{
			name:             priorityClass.Name,
			uid:              priorityClass.UID,
			value:            priorityClass.Value,
			preemptionPolicy: policy,
		})
	}
	if !minimumFound {
		resolved = append(resolved, priorityAdmissionCandidate{
			preemptionPolicy: corev1.PreemptLowerPriority,
		})
	}
	sort.Slice(resolved, func(left, right int) bool {
		return resolved[left].name < resolved[right].name
	})
	return priorityAdmissionDefaults{candidates: resolved}, nil
}

func (defaults priorityAdmissionDefaults) candidateForName(name string) (priorityAdmissionCandidate, bool) {
	for _, candidate := range defaults.candidates {
		if candidate.name == name {
			return candidate, true
		}
	}
	return priorityAdmissionCandidate{}, false
}

func priorityAdmissionDefaultsEqual(left, right priorityAdmissionDefaults) bool {
	if len(left.candidates) != len(right.candidates) {
		return false
	}
	for index := range left.candidates {
		if left.candidates[index] != right.candidates[index] {
			return false
		}
	}
	return true
}

func handlePriorityClassEvent(
	event watch.Event,
	inventory map[string]schedulingv1.PriorityClass,
	expected priorityAdmissionDefaults,
) error {
	if event.Type == watch.Bookmark {
		return nil
	}
	if event.Type == watch.Error {
		return fmt.Errorf("PriorityClass watch error: %w", apierrors.FromObject(event.Object))
	}
	priorityClass, ok := event.Object.(*schedulingv1.PriorityClass)
	if !ok || priorityClass == nil {
		return fmt.Errorf("PriorityClass watch returned %T", event.Object)
	}
	if err := validatePriorityClassObject(priorityClass); err != nil {
		return fmt.Errorf("validate PriorityClass watch event: %w", err)
	}

	previous, found := inventory[priorityClass.Name]
	switch event.Type {
	case watch.Added:
		if found {
			return fmt.Errorf("PriorityClass %q produced a duplicate ADDED event", priorityClass.Name)
		}
		for name := range inventory {
			if inventory[name].UID == priorityClass.UID {
				return fmt.Errorf("PriorityClass %q reuses the UID of %q", priorityClass.Name, name)
			}
		}
		inventory[priorityClass.Name] = *priorityClass.DeepCopy()
	case watch.Modified:
		if !found || previous.UID != priorityClass.UID {
			return fmt.Errorf("PriorityClass %q MODIFIED event does not preserve a known identity", priorityClass.Name)
		}
		inventory[priorityClass.Name] = *priorityClass.DeepCopy()
	case watch.Deleted:
		if !found || previous.UID != priorityClass.UID {
			return fmt.Errorf("PriorityClass %q DELETED event does not preserve a known identity", priorityClass.Name)
		}
		delete(inventory, priorityClass.Name)
	default:
		return fmt.Errorf("PriorityClass watch returned unsupported event type %q", event.Type)
	}

	resolved, err := resolveGlobalPriorityDefaults(inventory)
	if err != nil {
		return err
	}
	if !priorityAdmissionDefaultsEqual(resolved, expected) {
		return errors.New("global default PriorityClass changed during hook capture")
	}
	return nil
}

func drainPriorityClassEvents(
	events <-chan watch.Event,
	inventory map[string]schedulingv1.PriorityClass,
	expected priorityAdmissionDefaults,
) error {
	for events != nil {
		select {
		case event, open := <-events:
			if !open {
				return errors.New("PriorityClass watch closed before capture completed")
			}
			if err := handlePriorityClassEvent(event, inventory, expected); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func validateCaptureConfig(config captureConfig) error {
	if problems := validation.IsDNS1123Label(config.namespace); len(problems) != 0 {
		return fmt.Errorf("namespace is not a DNS label: %s", strings.Join(problems, "; "))
	}
	if problems := validation.IsDNS1123Subdomain(config.jobName); len(problems) != 0 {
		return fmt.Errorf("job name is not a DNS subdomain: %s", strings.Join(problems, "; "))
	}
	if config.logStartTimeout <= 0 || config.logRetryInterval <= 0 || config.maxLogBytes <= 0 {
		return errors.New("log capture bounds must be greater than zero")
	}
	if _, err := profileForHookMode(config.hookMode); err != nil {
		return err
	}
	if config.expectedJob == nil {
		return errors.New("exact rendered hook Job is required")
	}
	if err := validateRenderedJob(config.expectedJob, config); err != nil {
		return fmt.Errorf("validate exact rendered hook Job: %w", err)
	}
	return nil
}

func handleJobEvent(event watch.Event, config captureConfig, observed *batchv1.Job) (*batchv1.Job, error) {
	if event.Type == watch.Bookmark {
		return nil, nil
	}
	if event.Type == watch.Error {
		return nil, fmt.Errorf("exact hook Job watch error: %w", apierrors.FromObject(event.Object))
	}
	job, ok := event.Object.(*batchv1.Job)
	if !ok || job == nil {
		return nil, fmt.Errorf("exact hook Job watch returned %T", event.Object)
	}
	if event.Type != watch.Added {
		if event.Type != watch.Modified && event.Type != watch.Deleted {
			return nil, fmt.Errorf("exact hook Job watch returned unsupported event type %q", event.Type)
		}
		if observed == nil {
			return nil, fmt.Errorf("exact hook Job was first observed through a %s event", event.Type)
		}
		if job.Namespace != observed.Namespace || job.Name != observed.Name || job.UID != observed.UID {
			return nil, errors.New("exact hook Job identity changed after its ADDED event")
		}
		return nil, nil
	}
	if observed != nil {
		return nil, errors.New("exact hook Job produced more than one ADDED event")
	}
	if err := validateJob(job, config); err != nil {
		return nil, err
	}
	if err := validateJobAgainstRender(job, config.expectedJob); err != nil {
		return nil, err
	}
	return job.DeepCopy(), nil
}

func handlePodEvent(event watch.Event, config captureConfig, observed *corev1.Pod) (*corev1.Pod, error) {
	if event.Type == watch.Bookmark {
		return nil, nil
	}
	if event.Type == watch.Error {
		return nil, fmt.Errorf("hook Pod watch error: %w", apierrors.FromObject(event.Object))
	}
	pod, ok := event.Object.(*corev1.Pod)
	if !ok || pod == nil {
		return nil, fmt.Errorf("hook Pod watch returned %T", event.Object)
	}
	if event.Type != watch.Added {
		if event.Type != watch.Modified && event.Type != watch.Deleted {
			return nil, fmt.Errorf("hook Pod watch returned unsupported event type %q", event.Type)
		}
		if observed == nil {
			return nil, fmt.Errorf("hook Pod was first observed through a %s event", event.Type)
		}
		if pod.Namespace != observed.Namespace || pod.Name != observed.Name || pod.UID != observed.UID {
			return nil, errors.New("hook Pod identity changed after its ADDED event")
		}
		return nil, nil
	}
	if observed != nil {
		return nil, errors.New("hook Job produced more than one Pod ADDED event")
	}
	if err := validatePod(pod, config); err != nil {
		return nil, err
	}
	return pod.DeepCopy(), nil
}

func validateJob(job *batchv1.Job, config captureConfig) error {
	if job.UID == "" {
		return errors.New("hook Job UID is empty")
	}
	return validateJobShape(job, config)
}

func validateRenderedJob(job *batchv1.Job, config captureConfig) error {
	gvk := job.GetObjectKind().GroupVersionKind()
	if gvk != (schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}) {
		return fmt.Errorf("rendered hook Job has unexpected apiVersion/kind %q", gvk.String())
	}
	if job.Spec.Selector != nil {
		return errors.New("rendered hook Job must leave its selector to the Job API")
	}
	if err := rejectReservedControllerLabels(job.Spec.Template.Labels); err != nil {
		return fmt.Errorf("rendered hook Job template: %w", err)
	}
	if err := validateJobShape(job, config); err != nil {
		return err
	}
	if _, err := runtimePodDefaultsFromTemplate(job.Spec.Template.Spec, config.hookMode); err != nil {
		return fmt.Errorf("rendered hook Job runtime admission contract: %w", err)
	}
	return nil
}

func validateJobShape(job *batchv1.Job, config captureConfig) error {
	profile, err := profileForHookMode(config.hookMode)
	if err != nil {
		return err
	}
	gvk := job.GetObjectKind().GroupVersionKind()
	// The typed BatchV1 client fixes the endpoint's GVK even when its decoder
	// leaves TypeMeta empty. Reject contradictory TypeMeta without requiring it.
	if !gvk.Empty() && gvk != (schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}) {
		return fmt.Errorf("hook Job has unexpected apiVersion/kind %q", gvk.String())
	}
	if job.Namespace != config.namespace || job.Name != config.jobName {
		return errors.New("hook Job namespace or exact name is invalid")
	}
	if err := validateJobObjectMetadata(job.ObjectMeta); err != nil {
		return fmt.Errorf("hook Job metadata: %w", err)
	}
	if job.Annotations[hookAnnotation] != "pre-install,pre-upgrade" {
		return errors.New("hook Job has an unexpected Helm hook annotation")
	}
	if job.Annotations[hookWeightAnnotation] != profile.hookWeight {
		return errors.New("hook Job does not have the exact hook weight for its mode")
	}
	if job.Annotations[hookDeleteAnnotation] != "before-hook-creation,hook-succeeded,hook-failed" {
		return errors.New("hook Job has an unexpected deletion policy")
	}
	if job.Labels[componentLabel] != profile.component {
		return errors.New("hook Job component label does not match its exact hook mode")
	}
	if job.Labels[managedByLabel] != "Helm" {
		return errors.New("hook Job is not labeled as Helm-managed")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 210 {
		return errors.New("hook Job does not have the exact active deadline")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		return errors.New("hook Job must disable Pod retries")
	}
	if effectiveBool(job.Spec.ManualSelector, false) {
		return errors.New("hook Job must use the API-generated selector")
	}
	if job.Spec.Template.Labels[componentLabel] != profile.component || job.Spec.Template.Labels[managedByLabel] != "Helm" {
		return errors.New("hook Job template labels do not match the manager identity")
	}
	if err := validateJobTemplateMetadata(job.Spec.Template.ObjectMeta); err != nil {
		return fmt.Errorf("hook Job template metadata: %w", err)
	}
	if err := validatePodSpecContract(job.Spec.Template.Spec, config); err != nil {
		return fmt.Errorf("hook Job template: %w", err)
	}
	return nil
}

func validatePod(pod *corev1.Pod, config captureConfig) error {
	profile, err := profileForHookMode(config.hookMode)
	if err != nil {
		return err
	}
	gvk := pod.GetObjectKind().GroupVersionKind()
	// As above, the typed CoreV1 endpoint is authoritative when TypeMeta is
	// absent, while a contradictory value remains a fail-closed error.
	if !gvk.Empty() && gvk != (schema.GroupVersionKind{Version: "v1", Kind: "Pod"}) {
		return fmt.Errorf("hook Pod has unexpected apiVersion/kind %q", gvk.String())
	}
	if pod.Namespace != config.namespace || pod.Name == "" || pod.UID == "" {
		return errors.New("hook Pod namespace, name, or UID is invalid")
	}
	if err := validateGeneratedPodMetadata(pod, config.jobName); err != nil {
		return err
	}
	if pod.Labels[batchv1.JobNameLabel] != config.jobName {
		return errors.New("hook Pod does not have the exact stable Job name label")
	}
	if pod.Labels[componentLabel] != profile.component {
		return errors.New("hook Pod component label does not match its exact hook mode")
	}
	if err := validatePodSpecContract(pod.Spec, config); err != nil {
		return fmt.Errorf("hook Pod: %w", err)
	}
	owner, err := controllingJobOwner(pod)
	if err != nil {
		return err
	}
	if owner.Name != config.jobName || owner.UID == "" {
		return errors.New("hook Pod controlling Job owner has an invalid name or UID")
	}
	if pod.Labels[batchv1.ControllerUidLabel] != string(owner.UID) {
		return errors.New("hook Pod does not bind its stable controller UID label to its owner")
	}
	if err := validatePodAgainstRender(pod, config.expectedJob, config); err != nil {
		return err
	}
	return nil
}

func validateJobObjectMetadata(metadata metav1.ObjectMeta) error {
	if metadata.GenerateName != "" {
		return errors.New("generateName is not empty")
	}
	if len(metadata.OwnerReferences) != 0 {
		return errors.New("ownerReferences are not empty")
	}
	if len(metadata.Finalizers) != 0 {
		return errors.New("finalizers are not empty")
	}
	if metadata.DeletionTimestamp != nil || metadata.DeletionGracePeriodSeconds != nil {
		return errors.New("deletion metadata is present")
	}
	return nil
}

func validateJobTemplateMetadata(metadata metav1.ObjectMeta) error {
	if metadata.Name != "" || metadata.GenerateName != "" || metadata.Namespace != "" ||
		metadata.SelfLink != "" || metadata.UID != "" || metadata.ResourceVersion != "" || metadata.Generation != 0 ||
		!metadata.CreationTimestamp.IsZero() || metadata.DeletionTimestamp != nil || metadata.DeletionGracePeriodSeconds != nil ||
		len(metadata.OwnerReferences) != 0 || len(metadata.Finalizers) != 0 || len(metadata.ManagedFields) != 0 {
		return errors.New("fields other than labels and annotations are not API-zero")
	}
	return nil
}

func validateGeneratedPodMetadata(pod *corev1.Pod, jobName string) error {
	generateName := jobName + "-"
	if pod.GenerateName != generateName {
		return errors.New("hook Pod generateName does not match the exact Job prefix")
	}
	prefix := generateName
	if len(prefix) > generatedNameMaxPrefixLength {
		prefix = prefix[:generatedNameMaxPrefixLength]
	}
	if !strings.HasPrefix(pod.Name, prefix) {
		return errors.New("hook Pod name does not match its API-generated prefix")
	}
	suffix := pod.Name[len(prefix):]
	if len(suffix) != 5 {
		return errors.New("hook Pod name does not have the five-character generated suffix")
	}
	for _, character := range suffix {
		if !strings.ContainsRune(generatedNameAlphabet, character) {
			return errors.New("hook Pod name has a character outside the Kubernetes generated-name alphabet")
		}
	}
	if len(pod.Finalizers) != 1 || pod.Finalizers[0] != batchv1.JobTrackingFinalizer {
		return errors.New("hook Pod does not have exactly the Job tracking finalizer")
	}
	return nil
}

func validateContainerContract(containers []corev1.Container, config captureConfig) error {
	profile, err := profileForHookMode(config.hookMode)
	if err != nil {
		return err
	}
	if len(containers) != 1 {
		return errors.New("exactly one container is required")
	}
	container := containers[0]
	if container.Name != profile.containerName {
		return errors.New("container name does not match the exact hook mode")
	}
	if len(container.Command) != 1 || container.Command[0] != managerCommand {
		return errors.New("container command is not exactly /ptah-crd-manager")
	}
	if strings.TrimSpace(container.Image) == "" {
		return errors.New("container image is empty")
	}
	if err := validateManagerArguments(container.Args, config, container.Image); err != nil {
		return err
	}
	if container.TerminationMessagePath != "/dev/termination-log" || container.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		return errors.New("container termination message contract is invalid")
	}
	if len(container.Env) != 0 || len(container.EnvFrom) != 0 || len(container.Ports) != 0 {
		return errors.New("container has an unexpected environment or port surface")
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != apiAccessVolume ||
		container.VolumeMounts[0].MountPath != "/var/run/secrets/kubernetes.io/serviceaccount" || !container.VolumeMounts[0].ReadOnly {
		return errors.New("container API access volume mount is invalid")
	}
	security := container.SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.Capabilities == nil ||
		len(security.Capabilities.Add) != 0 || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != corev1.Capability("ALL") {
		return errors.New("container security context is invalid")
	}
	return nil
}

func validatePodSpecContract(spec corev1.PodSpec, config captureConfig) error {
	profile, err := profileForHookMode(config.hookMode)
	if err != nil {
		return err
	}
	if len(spec.InitContainers) != 0 || len(spec.EphemeralContainers) != 0 {
		return errors.New("init and ephemeral containers are not permitted")
	}
	expectedServiceAccount, err := profile.serviceAccountName(config.jobName)
	if err != nil {
		return err
	}
	if spec.ServiceAccountName != expectedServiceAccount {
		return errors.New("service account name does not match the exact hook mode identity")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		return errors.New("service account token automount must be disabled")
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		return errors.New("restart policy must be Never")
	}
	security := spec.SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.RunAsUser == nil || *security.RunAsUser != 65532 ||
		security.RunAsGroup == nil || *security.RunAsGroup != 65532 ||
		security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		return errors.New("Pod security context is invalid")
	}
	if len(spec.Volumes) != 1 || validateAPIAccessVolume(spec.Volumes[0]) != nil {
		return errors.New("Pod API access projected volume is invalid")
	}
	return validateContainerContract(spec.Containers, config)
}

func validateAPIAccessVolume(volume corev1.Volume) error {
	projected := volume.Projected
	if volume.Name != apiAccessVolume || projected == nil || projected.DefaultMode == nil || *projected.DefaultMode != 0o644 || len(projected.Sources) != 3 {
		return errors.New("invalid projected volume shape")
	}
	token := projected.Sources[0].ServiceAccountToken
	if token == nil || token.Path != "token" || token.ExpirationSeconds == nil || *token.ExpirationSeconds != 3600 || token.Audience != "" {
		return errors.New("invalid service account token projection")
	}
	configMap := projected.Sources[1].ConfigMap
	if configMap == nil || configMap.Name != "kube-root-ca.crt" || len(configMap.Items) != 1 ||
		configMap.Items[0].Key != "ca.crt" || configMap.Items[0].Path != "ca.crt" {
		return errors.New("invalid root CA projection")
	}
	downward := projected.Sources[2].DownwardAPI
	if downward == nil || len(downward.Items) != 1 || downward.Items[0].Path != "namespace" ||
		downward.Items[0].FieldRef == nil || downward.Items[0].FieldRef.APIVersion != "v1" ||
		downward.Items[0].FieldRef.FieldPath != "metadata.namespace" {
		return errors.New("invalid namespace projection")
	}
	return nil
}

func validateManagerArguments(arguments []string, config captureConfig, image string) error {
	profile, err := profileForHookMode(config.hookMode)
	if err != nil {
		return err
	}
	if len(arguments) != len(managerArgumentPrefixes) {
		return errors.New("container manager argument count is invalid")
	}
	for index, prefix := range managerArgumentPrefixes {
		if index == 0 {
			if arguments[index] != string(profile.mode) {
				return errors.New("container first argument does not match the exact hook mode")
			}
			continue
		}
		if !strings.HasPrefix(arguments[index], prefix) || len(arguments[index]) == len(prefix) {
			return fmt.Errorf("container manager argument %d does not match %s", index, prefix)
		}
	}
	if arguments[1] != "--timeout=180s" {
		return errors.New("container manager timeout argument is invalid")
	}
	if arguments[3] != "--release-namespace="+config.namespace {
		return errors.New("container release namespace argument is invalid")
	}
	expectedServiceAccount, err := profile.serviceAccountName(config.jobName)
	if err != nil {
		return err
	}
	if arguments[12] != "--hook-service-account-name="+expectedServiceAccount {
		return errors.New("container hook service account argument is invalid")
	}
	if arguments[18] != "--manager-image="+image {
		return errors.New("container manager image argument does not match its image")
	}
	return nil
}

func controllingJobOwner(pod *corev1.Pod) (metav1.OwnerReference, error) {
	if len(pod.OwnerReferences) != 1 {
		return metav1.OwnerReference{}, errors.New("hook Pod must have exactly one owner reference")
	}
	owner := &pod.OwnerReferences[0]
	if owner.Controller == nil || !*owner.Controller {
		return metav1.OwnerReference{}, errors.New("hook Pod owner is not the controller")
	}
	if owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return metav1.OwnerReference{}, errors.New("hook Pod owner does not block owner deletion")
	}
	if owner.APIVersion != "batch/v1" || owner.Kind != "Job" {
		return metav1.OwnerReference{}, errors.New("hook Pod controller is not a batch/v1 Job")
	}
	return *owner, nil
}

func validatePodOwner(pod *corev1.Pod, job *batchv1.Job, config captureConfig) error {
	owner, err := controllingJobOwner(pod)
	if err != nil {
		return err
	}
	if owner.Name != job.Name || owner.UID != job.UID {
		return errors.New("hook Pod controlling owner does not match the exact observed Job name and UID")
	}
	template := job.Spec.Template.Spec
	if !podSpecMatchesTemplate(pod.Spec, template, config) {
		return errors.New("hook Pod execution contract differs from the exact observed Job template")
	}
	return nil
}

type jobExecutionContract struct {
	ActiveDeadlineSeconds *int64
	BackoffLimit          *int32
	PodFailurePolicy      *batchv1.PodFailurePolicy
	SuccessPolicy         *batchv1.SuccessPolicy
	BackoffLimitPerIndex  *int32
	MaxFailedIndexes      *int32
	TTLSecondsAfterFinish *int32
	Completions           int32
	Parallelism           int32
	ManualSelector        bool
	Suspend               bool
	CompletionMode        batchv1.CompletionMode
	ManagedBy             string
	PodReplacementPolicy  batchv1.PodReplacementPolicy
	Pod                   corev1.PodSpec
}

func validateJobAgainstRender(observed, expected *batchv1.Job) error {
	if !stringMapEqual(observed.Annotations, expected.Annotations) {
		return errors.New("observed hook Job annotations differ from candidate render")
	}
	if !stringMapEqual(observed.Labels, expected.Labels) {
		return errors.New("observed hook Job labels differ from candidate render")
	}
	if !stringMapEqual(observed.Spec.Template.Annotations, expected.Spec.Template.Annotations) {
		return errors.New("observed hook Job template annotations differ from candidate render")
	}
	if err := compareTemplateLabels(observed.Spec.Template.Labels, expected.Spec.Template.Labels, observed.Name, observed.UID); err != nil {
		return fmt.Errorf("observed hook Job metadata differs from candidate render: %w", err)
	}
	if err := validateAutoGeneratedJobSelector(observed); err != nil {
		return fmt.Errorf("observed hook Job selector differs from the API-generated contract: %w", err)
	}
	observedContract := jobContract(observed)
	expectedContract := jobContract(expected)
	if !apiequality.Semantic.DeepEqual(observedContract, expectedContract) {
		return errors.New("observed hook Job execution contract differs from candidate render")
	}
	return nil
}

func validatePodAgainstRender(observed *corev1.Pod, expected *batchv1.Job, config captureConfig) error {
	if expected == nil {
		return errors.New("candidate render is unavailable for hook Pod validation")
	}
	owner, err := controllingJobOwner(observed)
	if err != nil {
		return err
	}
	if !stringMapEqual(observed.Annotations, expected.Spec.Template.Annotations) {
		return errors.New("observed hook Pod annotations differ from candidate render")
	}
	if err := compareTemplateLabels(observed.Labels, expected.Spec.Template.Labels, owner.Name, owner.UID); err != nil {
		return fmt.Errorf("observed hook Pod labels differ from candidate render: %w", err)
	}
	if !podSpecMatchesTemplate(observed.Spec, expected.Spec.Template.Spec, config) {
		return errors.New("observed hook Pod execution contract differs from candidate render")
	}
	return nil
}

func compareTemplateLabels(observed, expected map[string]string, jobName string, jobUID types.UID) error {
	if err := rejectReservedControllerLabels(expected); err != nil {
		return err
	}
	normalized := make(map[string]string, len(observed))
	for key, value := range observed {
		normalized[key] = value
	}
	dynamic := map[string]string{
		batchv1.JobNameLabel:       jobName,
		batchv1.ControllerUidLabel: string(jobUID),
		"job-name":                 jobName,
		"controller-uid":           string(jobUID),
	}
	for key, want := range dynamic {
		if got, found := normalized[key]; found {
			if got != want {
				return fmt.Errorf("dynamic label %q has an unexpected value", key)
			}
			delete(normalized, key)
		}
	}
	if !stringMapEqual(normalized, expected) {
		return errors.New("labels other than normalized Job-controller identity labels differ")
	}
	return nil
}

func rejectReservedControllerLabels(labels map[string]string) error {
	for _, key := range []string{batchv1.JobNameLabel, batchv1.ControllerUidLabel, "job-name", "controller-uid"} {
		if _, found := labels[key]; found {
			return fmt.Errorf("candidate render contains reserved Job-controller label %q", key)
		}
	}
	return nil
}

func stringMapEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func jobContract(job *batchv1.Job) jobExecutionContract {
	return jobExecutionContract{
		ActiveDeadlineSeconds: job.Spec.ActiveDeadlineSeconds,
		BackoffLimit:          job.Spec.BackoffLimit,
		PodFailurePolicy:      job.Spec.PodFailurePolicy,
		SuccessPolicy:         job.Spec.SuccessPolicy,
		BackoffLimitPerIndex:  job.Spec.BackoffLimitPerIndex,
		MaxFailedIndexes:      job.Spec.MaxFailedIndexes,
		TTLSecondsAfterFinish: job.Spec.TTLSecondsAfterFinished,
		Completions:           effectiveInt32(job.Spec.Completions, 1),
		Parallelism:           effectiveInt32(job.Spec.Parallelism, 1),
		ManualSelector:        effectiveBool(job.Spec.ManualSelector, false),
		Suspend:               effectiveBool(job.Spec.Suspend, false),
		CompletionMode:        effectiveCompletionMode(job.Spec.CompletionMode),
		ManagedBy:             effectiveManagedBy(job.Spec.ManagedBy),
		PodReplacementPolicy:  effectivePodReplacementPolicy(job.Spec.PodReplacementPolicy, job.Spec.PodFailurePolicy),
		Pod:                   normalizePodSpecDefaults(job.Spec.Template.Spec),
	}
}

func validateAutoGeneratedJobSelector(job *batchv1.Job) error {
	selector := job.Spec.Selector
	if selector == nil {
		return errors.New("selector is absent")
	}
	if len(selector.MatchExpressions) != 0 {
		return errors.New("selector has match expressions")
	}
	if len(selector.MatchLabels) != 1 {
		return errors.New("selector must have exactly one match label")
	}
	if selector.MatchLabels[batchv1.ControllerUidLabel] != string(job.UID) {
		return errors.New("selector controller UID does not match the observed Job UID")
	}
	return nil
}

func podSpecMatchesTemplate(observed, template corev1.PodSpec, config captureConfig) bool {
	expected := normalizePodSpecDefaults(template)
	actual := normalizePodSpecDefaults(observed)
	defaults, err := runtimePodDefaultsFromTemplate(template, config.hookMode)
	if err != nil {
		return false
	}
	if config.hookMode == hookModePreflight {
		if !config.preflightPriorityResolved {
			return false
		}
		candidate, found := config.preflightPriority.candidateForName(observed.PriorityClassName)
		if !found {
			return false
		}
		defaults.priorityClassName = candidate.name
		defaults.priority = candidate.value
		defaults.preemptionPolicy = candidate.preemptionPolicy
	}
	applyRuntimePodDefaults(&expected, defaults)
	return apiequality.Semantic.DeepEqual(actual, expected)
}

func normalizePodSpecDefaults(spec corev1.PodSpec) corev1.PodSpec {
	normalized := spec.DeepCopy()
	if normalized.DNSPolicy == "" {
		normalized.DNSPolicy = corev1.DNSClusterFirst
	}
	if normalized.SchedulerName == "" {
		normalized.SchedulerName = corev1.DefaultSchedulerName
	}
	if normalized.TerminationGracePeriodSeconds == nil {
		value := int64(corev1.DefaultTerminationGracePeriodSeconds)
		normalized.TerminationGracePeriodSeconds = &value
	}
	if normalized.EnableServiceLinks == nil {
		value := corev1.DefaultEnableServiceLinks
		normalized.EnableServiceLinks = &value
	}
	// Supported API servers may populate the deprecated serviceAccount alias
	// from serviceAccountName. Only that exact alias is semantically redundant;
	// a conflicting value remains in the contract and fails comparison.
	if normalized.DeprecatedServiceAccount == "" ||
		normalized.DeprecatedServiceAccount == normalized.ServiceAccountName {
		normalized.DeprecatedServiceAccount = ""
	}
	if len(normalized.ImagePullSecrets) == 0 {
		normalized.ImagePullSecrets = nil
	}
	if len(normalized.Tolerations) == 0 {
		normalized.Tolerations = nil
	}
	return *normalized
}

type runtimePodDefaults struct {
	defaultTolerationsEnabled bool
	defaultNotReadySeconds    int64
	defaultUnreachableSeconds int64
	alwaysPullImagesEnabled   bool
	priorityClassName         string
	priority                  int32
	preemptionPolicy          corev1.PreemptionPolicy
}

type renderedRuntimeAdmissionContract struct {
	Version                       int                           `json:"version"`
	ImagePullSecrets              []corev1.LocalObjectReference `json:"imagePullSecrets"`
	PriorityClassName             string                        `json:"priorityClassName"`
	PriorityClassValue            int32                         `json:"priorityClassValue"`
	PriorityClassPreemptionPolicy string                        `json:"priorityClassPreemptionPolicy"`
}

func runtimePodDefaultsFromTemplate(template corev1.PodSpec, mode hookMode) (runtimePodDefaults, error) {
	profile, err := profileForHookMode(mode)
	if err != nil {
		return runtimePodDefaults{}, err
	}
	if len(template.Containers) != 1 || len(template.Containers[0].Args) != len(managerArgumentPrefixes) {
		return runtimePodDefaults{}, errors.New("manager arguments are unavailable")
	}
	if template.Containers[0].Name != profile.containerName {
		return runtimePodDefaults{}, errors.New("manager container does not match the exact hook mode")
	}
	arguments := template.Containers[0].Args
	if arguments[0] != string(profile.mode) {
		return runtimePodDefaults{}, errors.New("manager mode does not match the exact hook mode")
	}
	var controllerArguments []string
	if err := decodeArgumentJSON(arguments[19], managerArgumentPrefixes[19], &controllerArguments); err != nil {
		return runtimePodDefaults{}, fmt.Errorf("decode controller runtime arguments: %w", err)
	}
	enabledValue, err := exactArgumentValue(controllerArguments, "--default-tolerations-enabled=")
	if err != nil {
		return runtimePodDefaults{}, err
	}
	var defaults runtimePodDefaults
	switch enabledValue {
	case "true":
		defaults.defaultTolerationsEnabled = true
	case "false":
	default:
		return runtimePodDefaults{}, errors.New("default tolerations enabled flag is not exactly true or false")
	}
	defaults.defaultNotReadySeconds, err = exactNonnegativeInt64Argument(
		controllerArguments,
		"--default-not-ready-toleration-seconds=",
	)
	if err != nil {
		return runtimePodDefaults{}, err
	}
	defaults.defaultUnreachableSeconds, err = exactNonnegativeInt64Argument(
		controllerArguments,
		"--default-unreachable-toleration-seconds=",
	)
	if err != nil {
		return runtimePodDefaults{}, err
	}
	alwaysPullImagesValue, err := exactArgumentValue(controllerArguments, "--always-pull-images-enabled=")
	if err != nil {
		return runtimePodDefaults{}, err
	}
	switch alwaysPullImagesValue {
	case "true":
		defaults.alwaysPullImagesEnabled = true
	case "false":
	default:
		return runtimePodDefaults{}, errors.New("always-pull-images enabled flag is not exactly true or false")
	}

	var contract renderedRuntimeAdmissionContract
	if err := decodeArgumentJSON(arguments[23], managerArgumentPrefixes[23], &contract); err != nil {
		return runtimePodDefaults{}, fmt.Errorf("decode runtime admission contract: %w", err)
	}
	if contract.Version != 1 {
		return runtimePodDefaults{}, errors.New("runtime admission contract version is not 1")
	}
	if !localObjectReferencesEqual(template.ImagePullSecrets, contract.ImagePullSecrets) {
		return runtimePodDefaults{}, errors.New("runtime admission image pull secrets differ from the rendered hook template")
	}
	policy := corev1.PreemptionPolicy(contract.PriorityClassPreemptionPolicy)
	if contract.PriorityClassName == "" {
		if contract.PriorityClassValue != 0 || policy != corev1.PreemptLowerPriority {
			return runtimePodDefaults{}, errors.New("runtime admission default priority contract is invalid")
		}
	} else if policy != corev1.PreemptLowerPriority && policy != corev1.PreemptNever {
		return runtimePodDefaults{}, errors.New("runtime admission priority class preemption policy is invalid")
	}
	if template.Priority != nil || template.PreemptionPolicy != nil {
		return runtimePodDefaults{}, errors.New("rendered hook template contains Pod-admission output fields")
	}
	if profile.mode == hookModePreflight {
		if template.PriorityClassName != "" {
			return runtimePodDefaults{}, errors.New("rendered preflight hook template must remain unclassified")
		}
		// The runtime admission contract describes the managed runtime and the
		// reconcile hook. Capture binds the deliberately classless preflight Pod
		// to a separately observed cluster-wide default before accepting it.
		defaults.priority = 0
		defaults.preemptionPolicy = corev1.PreemptLowerPriority
		return defaults, nil
	}
	if template.PriorityClassName == "" {
		if contract.PriorityClassName != "" {
			return runtimePodDefaults{}, errors.New("runtime admission priority class is named while the rendered hook template is unclassified")
		}
		defaults.priority = 0
		defaults.preemptionPolicy = corev1.PreemptLowerPriority
		return defaults, nil
	}
	if contract.PriorityClassName != template.PriorityClassName {
		return runtimePodDefaults{}, errors.New("runtime admission priority class differs from the rendered hook template")
	}
	defaults.priorityClassName = contract.PriorityClassName
	defaults.priority = contract.PriorityClassValue
	defaults.preemptionPolicy = policy
	return defaults, nil
}

func decodeArgumentJSON(argument, prefix string, destination any) error {
	if !strings.HasPrefix(argument, prefix) || len(argument) == len(prefix) {
		return errors.New("manager argument has an invalid prefix or empty value")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(argument, prefix))
	if err != nil {
		return err
	}
	if len(decoded) > 1<<20 {
		return errors.New("decoded manager argument exceeds the size limit")
	}
	if err := json.Unmarshal(decoded, destination); err != nil {
		return err
	}
	return nil
}

func exactArgumentValue(arguments []string, prefix string) (string, error) {
	value := ""
	found := false
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, prefix) {
			continue
		}
		if found || len(argument) == len(prefix) {
			return "", fmt.Errorf("controller runtime argument %s is duplicated or empty", prefix)
		}
		found = true
		value = strings.TrimPrefix(argument, prefix)
	}
	if !found {
		return "", fmt.Errorf("controller runtime argument %s is absent", prefix)
	}
	return value, nil
}

func exactNonnegativeInt64Argument(arguments []string, prefix string) (int64, error) {
	value, err := exactArgumentValue(arguments, prefix)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("controller runtime argument %s is not a nonnegative integer", prefix)
	}
	return parsed, nil
}

func localObjectReferencesEqual(left, right []corev1.LocalObjectReference) bool {
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return true
	}
	return apiequality.Semantic.DeepEqual(left, right)
}

func applyRuntimePodDefaults(expected *corev1.PodSpec, defaults runtimePodDefaults) {
	if defaults.defaultTolerationsEnabled {
		if !suppressesDefaultToleration(expected.Tolerations, corev1.TaintNodeNotReady) {
			seconds := defaults.defaultNotReadySeconds
			expected.Tolerations = append(expected.Tolerations, corev1.Toleration{
				Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds,
			})
		}
		if !suppressesDefaultToleration(expected.Tolerations, corev1.TaintNodeUnreachable) {
			seconds := defaults.defaultUnreachableSeconds
			expected.Tolerations = append(expected.Tolerations, corev1.Toleration{
				Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds,
			})
		}
	}
	expected.PriorityClassName = defaults.priorityClassName
	priority := defaults.priority
	expected.Priority = &priority
	policy := defaults.preemptionPolicy
	expected.PreemptionPolicy = &policy
	if defaults.alwaysPullImagesEnabled {
		for index := range expected.InitContainers {
			expected.InitContainers[index].ImagePullPolicy = corev1.PullAlways
		}
		for index := range expected.Containers {
			expected.Containers[index].ImagePullPolicy = corev1.PullAlways
		}
		for index := range expected.EphemeralContainers {
			expected.EphemeralContainers[index].ImagePullPolicy = corev1.PullAlways
		}
	}
}

func suppressesDefaultToleration(tolerations []corev1.Toleration, key string) bool {
	for _, toleration := range tolerations {
		if (toleration.Key == "" || toleration.Key == key) &&
			(toleration.Effect == "" || toleration.Effect == corev1.TaintEffectNoExecute) {
			return true
		}
	}
	return false
}

func effectiveInt32(value *int32, fallback int32) int32 {
	if value == nil {
		return fallback
	}
	return *value
}

func effectiveBool(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func effectiveCompletionMode(value *batchv1.CompletionMode) batchv1.CompletionMode {
	if value == nil {
		return batchv1.NonIndexedCompletion
	}
	return *value
}

func effectiveManagedBy(value *string) string {
	if value == nil {
		return batchv1.JobControllerName
	}
	return *value
}

func effectivePodReplacementPolicy(
	value *batchv1.PodReplacementPolicy,
	podFailurePolicy *batchv1.PodFailurePolicy,
) batchv1.PodReplacementPolicy {
	if value != nil {
		return *value
	}
	if podFailurePolicy != nil {
		return batchv1.Failed
	}
	return batchv1.TerminatingOrFailed
}

func capturePodLog(
	ctx context.Context,
	client resourceClient,
	config captureConfig,
	podName string,
	destination io.Writer,
) (int64, error) {
	profile, err := profileForHookMode(config.hookMode)
	if err != nil {
		return 0, err
	}
	streamContext, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	startTimer := time.AfterFunc(config.logStartTimeout, cancelStream)
	defer startTimer.Stop()

	for {
		stream, err := client.streamPodLogs(streamContext, config.namespace, podName, profile.containerName)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if streamContext.Err() != nil {
				return 0, errLogStartTimeout
			}
			if !isTransientLogStartError(err, podName, profile.containerName) {
				return 0, fmt.Errorf("start hook Pod log stream: %w", err)
			}
			if err := waitForRetry(streamContext, config.logRetryInterval); err != nil {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				if streamContext.Err() != nil {
					return 0, errLogStartTimeout
				}
				return 0, fmt.Errorf("wait for hook Pod log stream: %w", err)
			}
			continue
		}
		if !startTimer.Stop() {
			_ = stream.Close()
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, errLogStartTimeout
		}

		bytes, copyErr := copyBounded(streamContext, destination, stream, config.maxLogBytes)
		closeErr := stream.Close()
		if ctx.Err() != nil {
			return bytes, ctx.Err()
		}
		var destinationErr *destinationWriteError
		if errors.As(copyErr, &destinationErr) {
			return bytes, destinationErr
		}
		if errors.Is(copyErr, errLogTooLarge) {
			return bytes, copyErr
		}
		if bytes > 0 {
			return bytes, nil
		}
		if copyErr != nil {
			return 0, fmt.Errorf("read hook Pod log stream: %w", copyErr)
		}
		if closeErr != nil {
			return 0, fmt.Errorf("close empty hook Pod log stream: %w", closeErr)
		}
		return 0, errors.New("hook Pod log stream completed without any bytes")
	}
}

func isTransientLogStartError(err error, podName, containerName string) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	if !apierrors.IsBadRequest(err) {
		return false
	}
	message := err.Error()
	if strings.Contains(message, fmt.Sprintf("pod %s does not have a host assigned", podName)) {
		return true
	}
	if strings.Contains(message, fmt.Sprintf("container %q in pod %q is not available", containerName, podName)) {
		return true
	}
	waitingPrefix := fmt.Sprintf("container %q in pod %q is waiting to start", containerName, podName)
	return strings.Contains(message, waitingPrefix+": PodInitializing") ||
		strings.Contains(message, waitingPrefix+": ContainerCreating") ||
		strings.Contains(message, waitingPrefix+" - no logs yet")
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type boundedWriter struct {
	destination io.Writer
	remaining   int64
	written     int64
}

type destinationWriteError struct {
	cause error
}

func (err *destinationWriteError) Error() string {
	return "write hook log destination: " + err.cause.Error()
}

func (err *destinationWriteError) Unwrap() error {
	return err.cause
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		if writer.remaining > 0 {
			expected := int(writer.remaining)
			written, err := writer.destination.Write(data[:expected])
			writer.written += int64(written)
			writer.remaining -= int64(written)
			if err != nil {
				return written, &destinationWriteError{cause: err}
			}
			if written != expected {
				return written, &destinationWriteError{cause: io.ErrShortWrite}
			}
			return written, errLogTooLarge
		}
		return 0, errLogTooLarge
	}
	written, err := writer.destination.Write(data)
	writer.written += int64(written)
	writer.remaining -= int64(written)
	if err != nil {
		return written, &destinationWriteError{cause: err}
	}
	if written != len(data) {
		return written, &destinationWriteError{cause: io.ErrShortWrite}
	}
	return written, nil
}

func copyBounded(ctx context.Context, destination io.Writer, source io.ReadCloser, maximum int64) (int64, error) {
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			_ = source.Close()
		case <-closed:
		}
	}()

	writer := &boundedWriter{destination: destination, remaining: maximum}
	_, err := io.Copy(writer, source)
	return writer.written, err
}
