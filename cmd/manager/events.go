package main

import (
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

// eventCorrelatorOptions is how the manager filters its Events before they
// reach the API server: client-go's defaults, except that each reason on an
// object has a spam-filter bucket of its own (eventSpamKey).
func eventCorrelatorOptions() record.CorrelatorOptions {
	return record.CorrelatorOptions{SpamKeyFunc: eventSpamKey}
}

// eventSpamKey is client-go's spam-filter key with the reason added.
//
// client-go keys the filter on the source, the object and the type, with a
// burst of 25 and one more every five minutes. Every read records eight Normal
// Events, OperationStarted and OperationCompleted for Resolve, Verify, Observe
// and Plan, so a resource spent the burst in its first few reads. After that,
// an Event that it records once, such as ReadRequested, ApprovalAccepted, or a
// one-shot Warning beside a read that keeps failing, was kept only when it
// arrived while a token was free (#1176). With the reason in the key, a reason
// that repeats drains only its own bucket.
func eventSpamKey(event *corev1.Event) string {
	return strings.Join([]string{
		event.Source.Component,
		event.Source.Host,
		event.InvolvedObject.Kind,
		event.InvolvedObject.Namespace,
		event.InvolvedObject.Name,
		string(event.InvolvedObject.UID),
		event.InvolvedObject.APIVersion,
		event.Type,
		event.Reason,
	}, "\x00")
}

// eventSources makes the Event recorder of a named source.
type eventSources interface {
	For(component string) record.EventRecorder
}

// eventRecorders makes the recorders of the controllers and the result
// collector. They all send through one broadcaster, which filters with
// eventCorrelatorOptions and writes core/v1 Events. The chart grants the
// manager those; the events.k8s.io recorder would need a grant it does not
// hold.
//
// The broadcaster is the manager's own. controller-runtime takes one only
// through a deprecated option, and then starts it once for each of its two
// recorder providers, the cluster's and leader election's, so a leader would
// send every Event twice and store every folded Event twice. Leader election
// keeps controller-runtime's broadcaster. This one runs until the process
// ends, which it does once the manager stops.
type eventRecorders struct {
	broadcaster record.EventBroadcaster
	scheme      *runtime.Scheme
}

// newEventRecorders starts the broadcaster, sending to sink and logging each
// Event at V(1), as controller-runtime's broadcaster does.
func newEventRecorders(sink record.EventSink, scheme *runtime.Scheme, logger logr.Logger) eventRecorders {
	broadcaster := record.NewBroadcaster(record.WithCorrelatorOptions(eventCorrelatorOptions()))
	broadcaster.StartRecordingToSink(sink)
	broadcaster.StartEventWatcher(func(event *corev1.Event) {
		logger.V(1).Info(event.Message, "type", event.Type, "object", event.InvolvedObject, "reason", event.Reason)
	})
	return eventRecorders{broadcaster: broadcaster, scheme: scheme}
}

// For makes the recorder whose Events name component as their source.
func (r eventRecorders) For(component string) record.EventRecorder {
	return r.broadcaster.NewRecorder(r.scheme, corev1.EventSource{Component: component})
}
