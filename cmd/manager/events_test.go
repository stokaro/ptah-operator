package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"

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

// The recorders main hands the controllers and the result collector come from
// newEventRecorders, so the row sends through one of them to a sink.
func TestTheManagersRecordersKeyTheSpamFilterByReason(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sink := &reasonSink{}
	recorder := newEventRecorders(sink, scheme, logr.Discard()).For("ptah-schema-controller")
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
	if requested := sink.count("ReadRequested"); requested != 1 {
		t.Fatalf("the ReadRequested Event reached the sink %d times, want once", requested)
	}
}

// controller-runtime's recorders send through a broadcaster the manager's spam
// key never reaches, and a broadcaster given to the manager reaches the API
// server twice. So no code of the manager asks controller-runtime for one: each
// recorder comes from newEventRecorders.
func TestTheManagerTakesNoEventRecorderFromControllerRuntime(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	examined := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		examined++
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if node.Sel.Name == "GetEventRecorderFor" || node.Sel.Name == "GetEventRecorder" {
					t.Errorf("%s uses %s; make the recorder with newEventRecorders", name, node.Sel.Name)
				}
			case *ast.AssignStmt:
				for _, target := range node.Lhs {
					if field, ok := target.(*ast.SelectorExpr); ok && field.Sel.Name == "EventBroadcaster" {
						t.Errorf("%s gives the manager an EventBroadcaster; make the recorders with newEventRecorders", name)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "EventBroadcaster" {
					t.Errorf("%s gives the manager an EventBroadcaster; make the recorders with newEventRecorders", name)
				}
			}
			return true
		})
	}
	if examined < 3 {
		t.Fatalf("examined %d files of the manager, want main.go, events.go and results.go at least", examined)
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
