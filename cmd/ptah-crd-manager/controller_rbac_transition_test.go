package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func TestRetiredCredentialRevocationDelayCoversBoundTokenCache(t *testing.T) {
	t.Parallel()
	if retiredCredentialRevocationDelay != 65*time.Second {
		t.Fatalf("retired credential revocation delay = %s, want 65s", retiredCredentialRevocationDelay)
	}
}

func TestCompleteControllerRBACCutoverOrdersGraceGrantDenialAndActivation(t *testing.T) {
	t.Parallel()
	events := []string{}
	transition := &recordingControllerRBACTransition{
		events:        eventsRecorder(&events),
		predecessor:   true,
		requiresGrace: true,
	}
	barrier := &recordingAuthorizationConvergenceBarrier{events: eventsRecorder(&events)}
	err := completeControllerRBACCutover(
		context.Background(),
		func(context.Context) error {
			events = append(events, "zero-pods")
			return nil
		},
		transition,
		true,
		func(context.Context) error {
			events = append(events, "continuous-credential-fence-65s")
			return nil
		},
		func(context.Context) (authorizationConvergenceWaiter, error) {
			events = append(events, "build-endpoint-barrier")
			return barrier, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Activation is deliberately a caller action. Appending it only after the
	// helper returns proves that no caller can activate before the final role,
	// binding, and ServiceAccount identity recheck succeeds.
	events = append(events, "activation")
	want := []string{
		"continuous-credential-fence-65s",
		"transition",
		"build-endpoint-barrier",
		"barrier-validate",
		"barrier-wait",
		"verify-complete",
		"activation",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestCompleteControllerRBACCutoverWaitsGraceAgainOnRetry(t *testing.T) {
	t.Parallel()
	graceCalls := 0
	transition := &recordingControllerRBACTransition{
		predecessor:      true,
		requiresGrace:    true,
		transitionErrors: []error{errors.New("conflict"), nil},
	}
	invoke := func() error {
		return completeControllerRBACCutover(
			context.Background(),
			func(context.Context) error { return nil },
			transition,
			true,
			func(context.Context) error {
				graceCalls++
				return nil
			},
			func(context.Context) (authorizationConvergenceWaiter, error) {
				return &recordingAuthorizationConvergenceBarrier{}, nil
			},
		)
	}
	if err := invoke(); err == nil || !strings.Contains(err.Error(), "move exact controller RBAC bindings") {
		t.Fatalf("first cutover error = %v, want transition failure", err)
	}
	if err := invoke(); err != nil {
		t.Fatalf("retry cutover error = %v", err)
	}
	if graceCalls != 2 {
		t.Fatalf("credential grace calls = %d, want one full wait per attempt", graceCalls)
	}
}

func TestCompleteControllerRBACCutoverLeavesGraceBoundaryToContinuousFence(t *testing.T) {
	t.Parallel()
	podChecks := 0
	continuousFenceCalls := 0
	transition := &recordingControllerRBACTransition{requiresGrace: true}
	err := completeControllerRBACCutover(
		context.Background(),
		func(context.Context) error {
			podChecks++
			return nil
		},
		transition,
		true,
		func(context.Context) error {
			continuousFenceCalls++
			return nil
		},
		func(context.Context) (authorizationConvergenceWaiter, error) {
			return &recordingAuthorizationConvergenceBarrier{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if continuousFenceCalls != 1 || podChecks != 0 {
		t.Fatalf("continuous fence calls = %d, standalone Pod checks = %d, want 1/0", continuousFenceCalls, podChecks)
	}
}

func TestCompleteControllerRBACCutoverGraceConditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		predecessor   bool
		requiresGrace bool
		wantGrace     int
		wantPodChecks int
		wantBarrier   int
	}{
		{name: "managed fresh install", wantPodChecks: 1},
		{name: "external fresh candidate", requiresGrace: true, wantGrace: 1},
		{name: "upgrade", predecessor: true, requiresGrace: true, wantGrace: 1, wantBarrier: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			podChecks := 0
			graceCalls := 0
			barrierCalls := 0
			transition := &recordingControllerRBACTransition{
				predecessor: test.predecessor, requiresGrace: test.requiresGrace,
			}
			err := completeControllerRBACCutover(
				context.Background(),
				func(context.Context) error { podChecks++; return nil },
				transition,
				test.requiresGrace,
				func(context.Context) error { graceCalls++; return nil },
				func(context.Context) (authorizationConvergenceWaiter, error) {
					barrierCalls++
					return &recordingAuthorizationConvergenceBarrier{}, nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if graceCalls != test.wantGrace || podChecks != test.wantPodChecks || barrierCalls != test.wantBarrier {
				t.Fatalf(
					"calls grace=%d pods=%d barrier=%d, want grace=%d pods=%d barrier=%d",
					graceCalls,
					podChecks,
					barrierCalls,
					test.wantGrace,
					test.wantPodChecks,
					test.wantBarrier,
				)
			}
		})
	}
}

func TestCompleteControllerRBACCutoverStopsAtEveryFailedStage(t *testing.T) {
	t.Parallel()
	stages := []string{
		"continuous-fence",
		"transition",
		"build-barrier",
		"barrier-validate",
		"barrier-wait",
		"verify-complete",
	}
	for _, failedStage := range stages {
		t.Run(failedStage, func(t *testing.T) {
			t.Parallel()
			events := []string{}
			failure := errors.New("injected " + failedStage)
			transition := &recordingControllerRBACTransition{
				events: eventsRecorder(&events), predecessor: true, requiresGrace: true,
			}
			barrier := &recordingAuthorizationConvergenceBarrier{events: eventsRecorder(&events)}
			switch failedStage {
			case "transition":
				transition.transitionErrors = []error{failure}
			case "verify-complete":
				transition.verifyError = failure
			case "barrier-validate":
				barrier.validateError = failure
			case "barrier-wait":
				barrier.waitError = failure
			}
			err := completeControllerRBACCutover(
				context.Background(),
				func(context.Context) error {
					events = append(events, "unexpected-standalone-pod-wait")
					return nil
				},
				transition,
				true,
				func(context.Context) error {
					events = append(events, "continuous-fence")
					if failedStage == "continuous-fence" {
						return failure
					}
					return nil
				},
				func(context.Context) (authorizationConvergenceWaiter, error) {
					events = append(events, "build-barrier")
					if failedStage == "build-barrier" {
						return nil, failure
					}
					return barrier, nil
				},
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

type recordingControllerRBACTransition struct {
	events           func(string)
	predecessor      bool
	requiresGrace    bool
	preflightError   error
	transitionErrors []error
	verifyError      error
	transitionCalls  int
}

func (t *recordingControllerRBACTransition) Preflight(context.Context) error {
	t.record("preflight")
	return t.preflightError
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

func (t *recordingControllerRBACTransition) HasPredecessor() bool {
	return t.predecessor
}

func (t *recordingControllerRBACTransition) RequiresCredentialGrace(crdupgrade.ReleaseActivationState, bool) (bool, error) {
	return t.requiresGrace, nil
}

func (t *recordingControllerRBACTransition) record(event string) {
	if t.events != nil {
		t.events(event)
	}
}

type recordingAuthorizationConvergenceBarrier struct {
	events        func(string)
	validateError error
	waitError     error
}

func (b *recordingAuthorizationConvergenceBarrier) Validate() error {
	if b.events != nil {
		b.events("barrier-validate")
	}
	return b.validateError
}

func (b *recordingAuthorizationConvergenceBarrier) Wait(context.Context) error {
	if b.events != nil {
		b.events("barrier-wait")
	}
	return b.waitError
}

func eventsRecorder(events *[]string) func(string) {
	return func(event string) {
		*events = append(*events, event)
	}
}
