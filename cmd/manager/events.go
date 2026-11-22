package main

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
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

// withEventBroadcaster gives the manager a broadcaster that filters every
// recorder's Events through eventCorrelatorOptions, the controllers' and the
// result collector's alike.
//
// controller-runtime does not stop a broadcaster it was given when the manager
// stops, which is what its deprecation warns about. This one lives as long as
// the process, which ends when the manager stops.
func withEventBroadcaster(options ctrl.Options) ctrl.Options {
	//nolint:staticcheck // The manager takes a spam key only through a broadcaster; see above for why it cannot leak.
	options.EventBroadcaster = record.NewBroadcaster(record.WithCorrelatorOptions(eventCorrelatorOptions()))
	return options
}
