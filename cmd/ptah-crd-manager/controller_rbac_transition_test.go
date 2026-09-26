package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCompleteControllerRBACCutoverOrdersQuiescenceGrantAndActivation(t *testing.T) {
	t.Parallel()
	events := []string{}
	transition := &recordingControllerRBACTransition{events: eventsRecorder(&events)}
	err := completeControllerRBACCutover(
		context.Background(),
		func(context.Context) error {
			events = append(events, "zero-pods")
			return nil
		},
		transition,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Activation is deliberately a caller action. Appending it only after the
	// helper returns proves that no caller can activate before the final role,
	// binding, and ServiceAccount identity recheck succeeds.
	events = append(events, "activation")
	want := []string{"zero-pods", "transition", "verify-complete", "activation"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestCompleteControllerRBACCutoverWaitsForQuiescenceAgainOnRetry(t *testing.T) {
	t.Parallel()
	podChecks := 0
	transition := &recordingControllerRBACTransition{
		transitionErrors: []error{errors.New("conflict"), nil},
	}
	invoke := func() error {
		return completeControllerRBACCutover(
			context.Background(),
			func(context.Context) error {
				podChecks++
				return nil
			},
			transition,
		)
	}
	if err := invoke(); err == nil || !strings.Contains(err.Error(), "move exact controller RBAC bindings") {
		t.Fatalf("first cutover error = %v, want transition failure", err)
	}
	if err := invoke(); err != nil {
		t.Fatalf("retry cutover error = %v", err)
	}
	if podChecks != 2 {
		t.Fatalf("runtime Pod quiescence checks = %d, want one per attempt", podChecks)
	}
}

func TestCompleteControllerRBACCutoverStopsAtEveryFailedStage(t *testing.T) {
	t.Parallel()
	for _, failedStage := range []string{"zero-pods", "transition", "verify-complete"} {
		t.Run(failedStage, func(t *testing.T) {
			t.Parallel()
			events := []string{}
			failure := errors.New("injected " + failedStage)
			transition := &recordingControllerRBACTransition{events: eventsRecorder(&events)}
			switch failedStage {
			case "transition":
				transition.transitionErrors = []error{failure}
			case "verify-complete":
				transition.verifyError = failure
			}
			err := completeControllerRBACCutover(
				context.Background(),
				func(context.Context) error {
					events = append(events, "zero-pods")
					if failedStage == "zero-pods" {
						return failure
					}
					return nil
				},
				transition,
			)
			if err == nil || !strings.Contains(err.Error(), "injected "+failedStage) {
				t.Fatalf("cutover error = %v, want injected failure", err)
			}
			if events[len(events)-1] != failedStage {
				t.Fatalf("events after failure = %#v, want final event %q", events, failedStage)
			}
		})
	}
}

func TestCompleteControllerRBACCutoverRefusesMissingDependencies(t *testing.T) {
	t.Parallel()
	transition := &recordingControllerRBACTransition{}
	if err := completeControllerRBACCutover(context.Background(), nil, transition); err == nil {
		t.Fatal("cutover without a quiescence wait succeeded")
	}
	if err := completeControllerRBACCutover(context.Background(), func(context.Context) error { return nil }, nil); err == nil {
		t.Fatal("cutover without a transition succeeded")
	}
	if transition.transitionCalls != 0 {
		t.Fatalf("transition calls = %d, want none after a refused cutover", transition.transitionCalls)
	}
}

type recordingControllerRBACTransition struct {
	events           func(string)
	transitionErrors []error
	verifyError      error
	transitionCalls  int
}

func (t *recordingControllerRBACTransition) Transition(context.Context) error {
	t.record("transition")
	index := t.transitionCalls
	t.transitionCalls++
	if index < len(t.transitionErrors) {
		return t.transitionErrors[index]
	}
	return nil
}

func (t *recordingControllerRBACTransition) VerifyComplete(context.Context) error {
	t.record("verify-complete")
	return t.verifyError
}

func (t *recordingControllerRBACTransition) record(event string) {
	if t.events != nil {
		t.events(event)
	}
}

func eventsRecorder(events *[]string) func(string) {
	return func(event string) {
		*events = append(*events, event)
	}
}
