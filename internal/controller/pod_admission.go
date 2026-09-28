package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
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
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, refusal)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if len(cleaned) > podCreationRefusalMessageLimit {
		cleaned = cleaned[:podCreationRefusalMessageLimit] + "..."
	}
	return fmt.Sprintf("Job %s cannot create its Pod; the API server refused it: %s", job.Name, cleaned)
}

// podAdmissionConditionChange is what one pass decided about a refusal
// condition: whether it is set now, and whether that is news.
type podAdmissionConditionChange struct {
	refused bool
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
	message, refused, err := podCreationRefusal(ctx, reader, job, now)
	if err != nil {
		return podAdmissionConditionChange{}, err
	}
	reporting := current != nil && current.Reason == string(operatorv1alpha1.ReasonPodAdmissionRefused)
	switch {
	case refused:
		return podAdmissionConditionChange{
			refused: true, message: message,
			changed: !reporting || current.Message != message,
		}, nil
	case reporting:
		// The Pod is being created after all: the policy stopped refusing,
		// or a person changed what the Pod carries. The condition goes back
		// to what an operation in flight reports.
		return podAdmissionConditionChange{changed: true}, nil
	}
	return podAdmissionConditionChange{}, nil
}
