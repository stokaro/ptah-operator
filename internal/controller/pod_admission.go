package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	// eventInvolvedObjectUIDField is the field selector the core Events API
	// serves for the object an Event is about. The fake client in the unit
	// suites needs an index under the same name.
	eventInvolvedObjectUIDField = "involvedObject.uid"
	// failedCreateEventReason is what the Job controller records against a
	// Job whose Pod the API server refused to create.
	failedCreateEventReason = "FailedCreate"
	// podCreationRefusalGrace is how long a Job may stand without a Pod
	// before its Events are read. The Job controller creates the Pod within
	// a second of seeing the Job, so an earlier read is a read on the normal
	// path, and there the API server's answer is what the next pass sees.
	podCreationRefusalGrace = 10 * time.Second
	// podCreationRefusalMessageLimit bounds the refusal text the condition
	// carries; setCondition bounds the whole message after it.
	podCreationRefusalMessageLimit = 768
	// createContainerConfigError is the waiting reason the kubelet writes for a
	// container whose configuration it cannot build. It retries by itself.
	createContainerConfigError = "CreateContainerConfigError"
)

// podCreationRefusal says why a Job that has never had a Pod is not getting
// one, when the Job controller has said so. A Pod the API server refuses --
// a validating policy the namespace runs, a mutating webhook whose change the
// Pod-intent webhook then refuses, a quota, a LimitRange -- leaves the Job
// with nothing active and nothing terminal, and the only record is the
// FailedCreate Event the Job controller writes against it. The Job holds its
// own deadline whatever this reads, and a policy that stops refusing lets the
// controller's next attempt through; this only tells the reader why the
// operation stands still.
//
// The read goes to the API server rather than the cache, because Events are
// not watched: caching every Event in the cluster to answer this question for
// the rare stuck Job would cost more than the question.
func podCreationRefusal(
	ctx context.Context,
	reader client.Reader,
	job *batchv1.Job,
	now time.Time,
) (string, bool, error) {
	if job == nil || job.UID == "" || job.Status.Active > 0 || job.Status.Succeeded > 0 || job.Status.Failed > 0 {
		return "", false, nil
	}
	if !job.CreationTimestamp.IsZero() && now.Sub(job.CreationTimestamp.Time) < podCreationRefusalGrace {
		return "", false, nil
	}
	events := &corev1.EventList{}
	if err := reader.List(ctx, events, client.InNamespace(job.Namespace),
		client.MatchingFields{eventInvolvedObjectUIDField: string(job.UID)}); err != nil {
		return "", false, fmt.Errorf("read the Events recorded against Job %s: %w", job.Name, err)
	}
	var refusals []corev1.Event
	for _, event := range events.Items {
		if event.Reason != failedCreateEventReason || event.InvolvedObject.UID != job.UID ||
			event.InvolvedObject.Kind != "Job" || event.Message == "" {
			continue
		}
		refusals = append(refusals, event)
	}
	if len(refusals) == 0 {
		return "", false, nil
	}
	sort.SliceStable(refusals, func(i, j int) bool {
		return eventObservedAt(refusals[i]).After(eventObservedAt(refusals[j]))
	})
	return podCreationRefusalMessage(job, refusals[0].Message), true, nil
}

// eventObservedAt is when an Event was last seen, from whichever of the
// three timestamps the recorder filled.
func eventObservedAt(event corev1.Event) time.Time {
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return event.Series.LastObservedTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}

// podCreationRefusalMessage is the condition message for a refused Pod: the
// API server's own refusal, which is what names the policy and what it wants,
// with control characters removed and a bound on its length. The text comes
// from the cluster's admission chain and carries no credential of the
// operation's -- the Pod spec it describes references its Secrets by name.
func podCreationRefusalMessage(job *batchv1.Job, refusal string) string {
	return fmt.Sprintf("Job %s cannot create its Pod; the API server refused it: %s", job.Name, boundedClusterText(refusal))
}

// boundedClusterText is text another component of the cluster wrote, with
// control characters removed and a bound on its length.
func boundedClusterText(text string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if len(cleaned) > podCreationRefusalMessageLimit {
		cleaned = cleaned[:podCreationRefusalMessageLimit] + "..."
	}
	return cleaned
}

// podConfiguration is what the kubelet said about a Job's Pods.
type podConfiguration struct {
	// message is why a Pod cannot start, empty when no Pod says so.
	message string
	// alive is a Pod that is not on its way out, and so can answer for the
	// Job now. With none, nothing here contradicts what was reported earlier.
	alive bool
}

// podConfigurationError says why a Job's Pod exists and its container does not
// start, when the kubelet has said so. A missing Secret or a missing key leaves
// the Job with one active Pod and nothing terminal until its deadline, and the
// resource read as an operation in progress while the kubelet retried. An
// external secret store that has not synced yet looks like this.
//
// The kubelet builds this text from names and never from a value it resolved:
// `secret "x" not found` from the API, or `couldn't find key url in Secret ns/x`
// (pkg/kubelet/kubelet_pods.go, makeEnvironmentVariables, v1.36), and the image
// user check in pkg/kubelet/kuberuntime. Each name in it is one the Job already
// holds, and an acceptance row reads a Secret's value against what this reports.
//
// The read goes to the API server for the reason podCreationRefusal's does, and
// a Job with a ready Pod does not make it. The Job controller's writes do not
// report this state, so the Job's poll is what finds it.
func podConfigurationError(ctx context.Context, reader client.Reader, job *batchv1.Job) (podConfiguration, error) {
	if job == nil || job.UID == "" || job.Status.Active == 0 {
		return podConfiguration{}, nil
	}
	if job.Status.Ready != nil && *job.Status.Ready > 0 {
		return podConfiguration{alive: true}, nil
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.ControllerUidLabel: string(job.UID)}); err != nil {
		return podConfiguration{}, fmt.Errorf("read the Pods of Job %s: %w", job.Name, err)
	}
	var reading podConfiguration
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.DeletionTimestamp != nil || !exactControllerOwner(pod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job", job.Name, job.UID) {
			continue
		}
		reading.alive = true
		for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
			waiting := status.State.Waiting
			if waiting != nil && waiting.Reason == createContainerConfigError && waiting.Message != "" {
				reading.message = fmt.Sprintf("Job %s cannot start its Pod: %s", job.Name, boundedClusterText(waiting.Message))
				return reading, nil
			}
		}
	}
	return reading, nil
}

// withUnstartedPod adds what the resource last reported about a Pod that could
// not start to a failure that says only that no result arrived. The Job's
// deadline removes the Pod, so what this operation reported is all that is left
// to say why. judgePodAdmission keeps the condition standing until this reads it.
//
// The report can be out of date, so the words say whose they are. The resource
// looks at a running Pod every 30 seconds and the terminal pass does not look at
// its waiting state at all, so a Secret that arrived and a Pod that then started
// and died without a result can fall between two looks.
func withUnstartedPod(failure error, reported *metav1.Condition) error {
	if reported == nil || reported.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) ||
		!errors.Is(failure, runner.ErrFrameNotFound) {
		return failure
	}
	return fmt.Errorf("%w; last reported before that: %s", failure, reported.Message)
}

// podAdmissionConditionChange is what one pass decided about a condition that
// says why a Job's Pod is not running: whether it is set now, under which
// reason, and whether that is news.
type podAdmissionConditionChange struct {
	refused bool
	reason  operatorv1alpha1.ConditionReason
	message string
	changed bool
}

// judgePodAdmission reads the refusal for job and decides the condition
// transition against current, the condition the resource carries now.
func judgePodAdmission(
	ctx context.Context,
	reader client.Reader,
	job *batchv1.Job,
	now time.Time,
	current *metav1.Condition,
) (podAdmissionConditionChange, error) {
	reason := operatorv1alpha1.ReasonPodAdmissionRefused
	message, refused, err := podCreationRefusal(ctx, reader, job, now)
	var pods podConfiguration
	if err == nil && !refused {
		pods, err = podConfigurationError(ctx, reader, job)
		if pods.message != "" {
			reason, message, refused = operatorv1alpha1.ReasonPodConfigurationError, pods.message, true
		}
	}
	if err != nil {
		return podAdmissionConditionChange{}, err
	}
	admissionRefused := current != nil && current.Reason == string(operatorv1alpha1.ReasonPodAdmissionRefused)
	cannotStart := current != nil && current.Reason == string(operatorv1alpha1.ReasonPodConfigurationError)
	switch {
	case refused:
		return podAdmissionConditionChange{
			refused: true, reason: reason, message: message,
			changed: !(admissionRefused || cannotStart) || current.Reason != string(reason) || current.Message != message,
		}, nil
	case admissionRefused:
		// The Pod is being created after all: the policy stopped refusing, or a
		// person changed what the Pod carries. The condition goes back to what
		// an operation in flight reports.
		return podAdmissionConditionChange{changed: true}, nil
	case cannotStart && pods.alive:
		// A Pod is past what stopped it: the Secret arrived.
		return podAdmissionConditionChange{changed: true}, nil
	}
	// A condition that says a Pod cannot start stands while no Pod is alive to
	// say otherwise. The Job's deadline deletes the Pod before the Job turns
	// terminal, and clearing the condition in that gap would leave the failure
	// that follows with nothing to say about why no result arrived.
	return podAdmissionConditionChange{}, nil
}
