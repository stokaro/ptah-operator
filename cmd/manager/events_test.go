package main

import (
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// routineEvents is more than client-go's burst of 25, as the reads of a
// resource's first half hour are.
const routineEvents = 30

// A resource's routine Events must leave the bucket of an Event it records
// once alone. The row sends the same Events through client-go's key, which
// drops the request as stokaro/ptah-console#201's acceptance run did, and
// through the manager's, which keeps it and still holds the repeating reason to
// its burst.
func TestARoutineEventLeavesAnotherReasonsBucketAlone(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name        string
		options     record.CorrelatorOptions
		wantRequest bool
	}{
		{name: "client-go's key", options: record.CorrelatorOptions{}, wantRequest: false},
		{name: "the manager's key", options: eventCorrelatorOptions(), wantRequest: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			options := row.options
			// No token comes back while the clock stands still.
			options.Clock = clocktesting.NewFakePassiveClock(time.Date(2026, 11, 22, 3, 22, 24, 0, time.UTC))
			correlator := record.NewEventCorrelatorWithOptions(options)
			started := 0
			for range routineEvents {
				if kept(t, correlator, inventoryEvent("OperationStarted", "Resolve started")) {
					started++
				}
			}
			if started != 25 {
				t.Fatalf("%d of %d OperationStarted Events were kept, want client-go's burst of 25", started, routineEvents)
			}
			if got := kept(t, correlator, inventoryEvent("ReadRequested", "A read was requested; it starts now")); got != row.wantRequest {
				t.Fatalf("after %d OperationStarted Events, the ReadRequested Event was kept = %t, want %t", routineEvents, got, row.wantRequest)
			}
		})
	}
}

// The broadcaster main gives the manager is what every recorder it makes sends
// through, so the row sends through a recorder of that broadcaster to a sink.
func TestTheManagersRecordersKeyTheSpamFilterByReason(t *testing.T) {
	t.Parallel()

	broadcaster := withEventBroadcaster(ctrl.Options{}).EventBroadcaster //nolint:staticcheck // Reads what main sets.
	if broadcaster == nil {
		t.Fatal("the manager is given no broadcaster, so controller-runtime's own keys the spam filter without the reason")
	}
	sink := &reasonSink{}
	watcher := broadcaster.StartRecordingToSink(sink)
	t.Cleanup(watcher.Stop)

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	recorder := broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "ptah-schema-controller"})
	inventory := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "inventory", UID: "inventory-uid"}}
	for range routineEvents {
		recorder.Event(inventory, corev1.EventTypeNormal, "OperationStarted", "Resolve started")
	}
	recorder.Event(inventory, corev1.EventTypeNormal, "ReadRequested", "A read was requested; it starts now")

	// The sink is sent Events in order, so once the last one arrives every
	// Event before it has been kept or dropped.
	deadline := time.Now().Add(10 * time.Second)
	for sink.count("ReadRequested") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the ReadRequested Event never reached the sink after %d OperationStarted Events; it reached %v",
				routineEvents, sink.seen())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if started := sink.count("OperationStarted"); started != 25 {
		t.Fatalf("%d of %d OperationStarted Events reached the sink, want client-go's burst of 25", started, routineEvents)
	}
}

func inventoryEvent(reason, message string) *corev1.Event {
	at := metav1.NewTime(time.Date(2026, 11, 22, 3, 22, 24, 0, time.UTC))
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "inventory." + reason},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "PtahSchema",
			Namespace:  "shop",
			Name:       "inventory",
			UID:        "inventory-uid",
		},
		Source:         corev1.EventSource{Component: "ptah-schema-controller"},
		Type:           corev1.EventTypeNormal,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: at,
		LastTimestamp:  at,
		Count:          1,
	}
}

func kept(t *testing.T, correlator *record.EventCorrelator, event *corev1.Event) bool {
	t.Helper()
	result, err := correlator.EventCorrelate(event)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Skip {
		correlator.UpdateState(result.Event)
	}
	return !result.Skip
}

// reasonSink is an EventSink that keeps the reason of every Event it is sent,
// created or patched.
type reasonSink struct {
	mu      sync.Mutex
	reasons []string
}

func (s *reasonSink) Create(event *corev1.Event) (*corev1.Event, error) { return s.keep(event) }

func (s *reasonSink) Update(event *corev1.Event) (*corev1.Event, error) { return s.keep(event) }

func (s *reasonSink) Patch(event *corev1.Event, _ []byte) (*corev1.Event, error) {
	return s.keep(event)
}

func (s *reasonSink) keep(event *corev1.Event) (*corev1.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, event.Reason)
	return event.DeepCopy(), nil
}

func (s *reasonSink) count(reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, seen := range s.reasons {
		if seen == reason {
			count++
		}
	}
	return count
}

func (s *reasonSink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Compact(slices.Clone(s.reasons))
}
