package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRBACConvergenceBarrierWaitsForEveryEndpointStableWindow(t *testing.T) {
	first := &scriptedSubjectAccessReviewClient{statuses: []authorizationv1.SubjectAccessReviewStatus{
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
		{Allowed: true, Reason: "stale authorizer cache"},
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
	}}
	second := &scriptedSubjectAccessReviewClient{statuses: []authorizationv1.SubjectAccessReviewStatus{
		{Allowed: true, Reason: "lagging API server"},
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
		deniedAuthorizationStatus(),
	}}
	barrier := validRBACConvergenceBarrier(first)
	barrier.Endpoints = append(barrier.Endpoints, NamedAuthorizationReviewClient{Name: "api-b", Client: second})
	barrier.StabilityDuration = 2 * time.Second
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if first.calls != 6 || second.calls != 6 {
		t.Fatalf("endpoint calls = (%d, %d), want (6, 6)", first.calls, second.calls)
	}
}

func TestRBACConvergenceBarrierResetsWindowWhenAdvertisedEndpointsChange(t *testing.T) {
	first := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	second := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	barrier := validRBACConvergenceBarrier(first)
	barrier.StabilityDuration = 2 * time.Second
	providerCalls := 0
	barrier.EndpointProvider = func(context.Context) ([]NamedAuthorizationReviewClient, error) {
		providerCalls++
		if providerCalls == 1 {
			return []NamedAuthorizationReviewClient{{Name: "api-a", Client: first}}, nil
		}
		return []NamedAuthorizationReviewClient{
			{Name: "api-b", Client: second},
			{Name: "api-a", Client: first},
		}, nil
	}
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 4 || first.calls != 4 || second.calls != 3 {
		t.Fatalf(
			"provider/endpoint calls = %d/%d/%d, want 4/4/3 after topology reset",
			providerCalls,
			first.calls,
			second.calls,
		)
	}
}

func TestRBACConvergenceBarrierDoesNotResetWindowForEndpointReordering(t *testing.T) {
	first := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	second := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	barrier := validRBACConvergenceBarrier(first)
	barrier.Endpoints = append(barrier.Endpoints, NamedAuthorizationReviewClient{Name: "api-b", Client: second})
	barrier.StabilityDuration = 2 * time.Second
	providerCalls := 0
	barrier.EndpointProvider = func(context.Context) ([]NamedAuthorizationReviewClient, error) {
		providerCalls++
		return []NamedAuthorizationReviewClient{
			{Name: "api-b", Client: second},
			{Name: "api-a", Client: first},
		}, nil
	}
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 3 || first.calls != 3 || second.calls != 3 {
		t.Fatalf("provider/endpoint calls = %d/%d/%d, want 3/3/3", providerCalls, first.calls, second.calls)
	}
}

func TestRBACConvergenceBarrierResetsWindowWhenAdvertisedEndpointDisappears(t *testing.T) {
	first := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	second := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	barrier := validRBACConvergenceBarrier(first)
	barrier.Endpoints = append(barrier.Endpoints, NamedAuthorizationReviewClient{Name: "api-b", Client: second})
	barrier.StabilityDuration = 2 * time.Second
	providerCalls := 0
	barrier.EndpointProvider = func(context.Context) ([]NamedAuthorizationReviewClient, error) {
		providerCalls++
		if providerCalls == 1 {
			return []NamedAuthorizationReviewClient{
				{Name: "api-a", Client: first},
				{Name: "api-b", Client: second},
			}, nil
		}
		return []NamedAuthorizationReviewClient{{Name: "api-b", Client: second}}, nil
	}
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 4 || first.calls != 1 || second.calls != 4 {
		t.Fatalf(
			"provider/endpoint calls = %d/%d/%d, want 4/1/4 after topology reset",
			providerCalls,
			first.calls,
			second.calls,
		)
	}
}

func TestRBACConvergenceBarrierResetsWindowAfterDiscoveryError(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{}}
	barrier := validRBACConvergenceBarrier(client)
	barrier.StabilityDuration = 2 * time.Second
	providerCalls := 0
	barrier.EndpointProvider = func(context.Context) ([]NamedAuthorizationReviewClient, error) {
		providerCalls++
		if providerCalls == 2 {
			return nil, errors.New("temporary discovery failure")
		}
		return []NamedAuthorizationReviewClient{{Name: "api-a", Client: client}}, nil
	}
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 5 || client.calls != 4 {
		t.Fatalf("provider/endpoint calls = %d/%d, want 5/4 after discovery reset", providerCalls, client.calls)
	}
}

func TestRBACConvergenceBarrierRetriesTransportErrorsAndResetsWindow(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{outcomes: []subjectAccessReviewOutcome{
		{status: deniedAuthorizationStatus()},
		{err: errors.New("temporary connection failure")},
		{status: deniedAuthorizationStatus()},
		{status: deniedAuthorizationStatus()},
	}}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if client.calls != 4 {
		t.Fatalf("Create() calls = %d, want 4", client.calls)
	}
}

func TestRBACConvergenceBarrierRejectsEvaluationError(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{statuses: []authorizationv1.SubjectAccessReviewStatus{{
		Denied:          true,
		EvaluationError: "webhook authorizer unavailable",
	}}}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	err := barrier.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not evaluate") || !strings.Contains(err.Error(), "webhook authorizer unavailable") {
		t.Fatalf("Wait() error = %v, want fail-closed evaluation error", err)
	}
	if client.calls != 1 {
		t.Fatalf("Create() calls = %d, want 1", client.calls)
	}
}

func TestRBACConvergenceBarrierAcceptsNoOpinionAndStillRejectsAllowed(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{statuses: []authorizationv1.SubjectAccessReviewStatus{
		{Allowed: true, Denied: true},
		{},
		{},
	}}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if client.calls != 3 {
		t.Fatalf("Create() calls = %d, want 3", client.calls)
	}
}

func TestRBACConvergenceBarrierSelfReviewAllowedResetsWindow(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{
		defaultStatus: deniedAuthorizationStatus(),
		selfStatuses: []authorizationv1.SubjectAccessReviewStatus{
			{Allowed: true, Reason: "cleanup binding is still effective"},
			{},
			{},
		},
	}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if client.selfCalls != 3 {
		t.Fatalf("SelfSubjectAccessReview calls = %d, want 3 after allowed result reset the window", client.selfCalls)
	}
}

func TestRBACConvergenceBarrierRejectsSelfReviewEvaluationError(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{
		defaultStatus: deniedAuthorizationStatus(),
		selfStatuses: []authorizationv1.SubjectAccessReviewStatus{{
			EvaluationError: "current credential authorizer unavailable",
		}},
	}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	err := barrier.Wait(context.Background())
	for _, want := range []string{`endpoint "api-a"`, "current cleanup credential", `check "delete-policy"`, "authorizer unavailable"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Wait() error = %v, want containing %q", err, want)
		}
	}
}

func TestRBACConvergenceBarrierRetriesSelfReviewTransportErrors(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{
		defaultStatus: deniedAuthorizationStatus(),
		selfOutcomes: []subjectAccessReviewOutcome{
			{status: deniedAuthorizationStatus()},
			{err: errors.New("temporary self-review connection failure")},
			{status: deniedAuthorizationStatus()},
			{status: deniedAuthorizationStatus()},
		},
	}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if client.selfCalls != 4 {
		t.Fatalf("SelfSubjectAccessReview calls = %d, want 4 after transport error reset the window", client.selfCalls)
	}
}

func TestRBACConvergenceBarrierRejectsNilSelfReview(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{
		defaultStatus:    deniedAuthorizationStatus(),
		selfNilResponses: 1,
	}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	err := barrier.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nil SelfSubjectAccessReview") {
		t.Fatalf("Wait() error = %v, want invalid self-review response error", err)
	}
}

func TestRBACConvergenceBarrierRequiresMoreThanOneDeniedObservation(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{defaultStatus: deniedAuthorizationStatus()}
	barrier := validRBACConvergenceBarrier(client)
	barrier.StabilityDuration = 3 * time.Second
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if client.calls != 4 {
		t.Fatalf("Create() calls = %d, want 4 observations across the stable window", client.calls)
	}
}

func TestRBACConvergenceBarrierSendsExactSubjectAndChecksToEveryEndpoint(t *testing.T) {
	first := &scriptedSubjectAccessReviewClient{defaultStatus: deniedAuthorizationStatus(), mutateRequests: true}
	second := &scriptedSubjectAccessReviewClient{defaultStatus: deniedAuthorizationStatus()}
	barrier := &RBACConvergenceBarrier{
		Endpoints: []NamedAuthorizationReviewClient{
			{Name: "api-a", Client: first},
			{Name: "api-b", Client: second},
		},
		Probes: []AuthorizationProbe{
			{Subject: AuthorizationSubject{
				Name:   "controller",
				User:   "system:serviceaccount:operator:controller",
				UID:    "controller-uid",
				Groups: []string{"system:serviceaccounts", "system:authenticated"},
				Extra: map[string]authorizationv1.ExtraValue{
					"authentication.kubernetes.io/pod-name": {"controller-0"},
				},
			}, Checks: []AuthorizationCheck{{
				Name: "delete-policy",
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb:      "delete",
					Group:     "admissionregistration.k8s.io",
					Version:   "v1",
					Resource:  "validatingadmissionpolicies",
					Name:      "fixed-policy",
					Namespace: "",
				},
			}}},
			{Subject: AuthorizationSubject{
				Name:   "certificate",
				User:   "system:serviceaccount:operator:certificate",
				Groups: []string{"system:serviceaccounts"},
			}, Checks: []AuthorizationCheck{{
				Name:                  "health-endpoint",
				NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/healthz"},
			}}},
		},
		SelfChecks: []AuthorizationCheck{
			{
				Name: "delete-policy",
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb:      "delete",
					Group:     "admissionregistration.k8s.io",
					Version:   "v1",
					Resource:  "validatingadmissionpolicies",
					Name:      "fixed-policy",
					Namespace: "",
				},
			},
			{
				Name:                  "health-endpoint",
				NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/healthz"},
			},
		},
		PollEvery:         time.Second,
		StabilityDuration: time.Second,
	}
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	for name, client := range map[string]*scriptedSubjectAccessReviewClient{"api-a": first, "api-b": second} {
		if len(client.requests) != 4 {
			t.Fatalf("%s request count = %d, want 4", name, len(client.requests))
		}
		assertAuthorizationProbeMatrix(t, name, client.requests[:2], barrier.Probes)
		assertAuthorizationProbeMatrix(t, name, client.requests[2:], barrier.Probes)
		if len(client.selfRequests) != 4 {
			t.Fatalf("%s SelfSubjectAccessReview request count = %d, want 4", name, len(client.selfRequests))
		}
		assertSelfAuthorizationRequestMatrix(t, name, client.selfRequests[:2], barrier.SelfChecks)
		assertSelfAuthorizationRequestMatrix(t, name, client.selfRequests[2:], barrier.SelfChecks)
	}
}

func TestRBACConvergenceBarrierHonorsCancellation(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{defaultStatus: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}
	barrier := validRBACConvergenceBarrier(client)
	ctx, cancel := context.WithCancel(context.Background())
	barrier.sleep = func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		<-waitCtx.Done()
		return waitCtx.Err()
	}

	err := barrier.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if client.calls != 1 {
		t.Fatalf("Create() calls = %d, want 1", client.calls)
	}
}

func TestRBACConvergenceBarrierHonorsPreCanceledContext(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{defaultStatus: deniedAuthorizationStatus()}
	barrier := validRBACConvergenceBarrier(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := barrier.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if client.calls != 0 {
		t.Fatalf("Create() calls = %d, want 0", client.calls)
	}
}

func TestRBACConvergenceBarrierAcceptsExactGroupSubject(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{defaultStatus: deniedAuthorizationStatus()}
	barrier := validRBACConvergenceBarrier(client)
	barrier.Probes[0].Subject = AuthorizationSubject{Name: "release-service-accounts", Groups: []string{"system:serviceaccounts:operator"}}
	installDeterministicConvergenceClock(barrier)

	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got := client.requests[0].Spec; got.User != "" || !reflect.DeepEqual(got.Groups, []string{"system:serviceaccounts:operator"}) {
		t.Fatalf("SubjectAccessReview subject = user %q groups %#v", got.User, got.Groups)
	}
}

func TestRBACConvergenceBarrierRejectsNilResponse(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{nilResponses: 1}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	err := barrier.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nil SubjectAccessReview") {
		t.Fatalf("Wait() error = %v, want invalid response error", err)
	}
}

func TestRBACConvergenceBarrierRejectsInvalidAndDuplicateConfiguration(t *testing.T) {
	validClient := &scriptedSubjectAccessReviewClient{defaultStatus: deniedAuthorizationStatus()}
	tests := []struct {
		name   string
		mutate func(*RBACConvergenceBarrier)
		want   string
	}{
		{name: "zero poll", mutate: func(b *RBACConvergenceBarrier) { b.PollEvery = 0 }, want: "poll interval"},
		{name: "zero stability", mutate: func(b *RBACConvergenceBarrier) { b.StabilityDuration = 0 }, want: "stability duration"},
		{name: "empty endpoints", mutate: func(b *RBACConvergenceBarrier) { b.Endpoints = nil }, want: "endpoints are empty"},
		{name: "blank endpoint", mutate: func(b *RBACConvergenceBarrier) { b.Endpoints[0].Name = "" }, want: "endpoint at index"},
		{name: "padded endpoint", mutate: func(b *RBACConvergenceBarrier) { b.Endpoints[0].Name = " api-a" }, want: "padded name"},
		{name: "nil client", mutate: func(b *RBACConvergenceBarrier) { b.Endpoints[0].Client = nil }, want: "nil client"},
		{
			name: "typed nil client",
			mutate: func(b *RBACConvergenceBarrier) {
				var client *scriptedSubjectAccessReviewClient
				b.Endpoints[0].Client = client
			},
			want: "nil client",
		},
		{
			name: "duplicate endpoint",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Endpoints = append(b.Endpoints, NamedAuthorizationReviewClient{Name: "api-a", Client: validClient})
			},
			want: "endpoint \"api-a\" is duplicated",
		},
		{name: "empty probes", mutate: func(b *RBACConvergenceBarrier) { b.Probes = nil }, want: "probes are empty"},
		{name: "blank subject name", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Subject.Name = "" }, want: "subject at index"},
		{
			name: "duplicate subject name",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes = append(b.Probes, b.Probes[0])
			},
			want: "subject name \"controller\" is duplicated",
		},
		{
			name: "empty identity",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Subject.User = ""
				b.Probes[0].Subject.Groups = nil
			},
			want: "no user or groups",
		},
		{name: "padded user", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Subject.User += " " }, want: "padded user"},
		{name: "empty group", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Subject.Groups = []string{""} }, want: "empty or padded group"},
		{name: "padded group", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Subject.Groups = []string{" system:authenticated"} }, want: "empty or padded group"},
		{name: "duplicate group", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Subject.Groups = []string{"g", "g"} }, want: "duplicate group"},
		{
			name: "empty extra key",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Subject.Extra = map[string]authorizationv1.ExtraValue{"": {"value"}}
			},
			want: "empty or padded extra key",
		},
		{
			name: "duplicate extra value",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Subject.Extra = map[string]authorizationv1.ExtraValue{"key": {"value", "value"}}
			},
			want: "duplicate value",
		},
		{
			name: "duplicate semantic subject",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Subject.Groups = []string{"group-b", "group-a"}
				duplicate := b.Probes[0]
				duplicate.Subject.Name = "same-controller"
				duplicate.Subject.Groups = []string{"group-a", "group-b"}
				b.Probes = append(b.Probes, duplicate)
			},
			want: "are duplicates",
		},
		{name: "empty probe checks", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Checks = nil }, want: "checks are empty"},
		{name: "empty current checks", mutate: func(b *RBACConvergenceBarrier) { b.SelfChecks = nil }, want: "current-subject checks are empty"},
		{name: "blank check name", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Checks[0].Name = "" }, want: "check at index"},
		{
			name: "duplicate check name",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Checks = append(b.Probes[0].Checks, b.Probes[0].Checks[0])
			},
			want: "check name \"delete-policy\" is duplicated",
		},
		{
			name:   "no attribute type",
			mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Checks[0].ResourceAttributes = nil },
			want:   "exactly one attribute type",
		},
		{
			name: "both attribute types",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Checks[0].NonResourceAttributes = &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/healthz"}
			},
			want: "exactly one attribute type",
		},
		{name: "empty resource verb", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Checks[0].ResourceAttributes.Verb = "" }, want: "empty or padded verb"},
		{name: "empty resource", mutate: func(b *RBACConvergenceBarrier) { b.Probes[0].Checks[0].ResourceAttributes.Resource = "" }, want: "empty or padded resource"},
		{
			name: "ambiguous field selector",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Checks[0].ResourceAttributes.FieldSelector = &authorizationv1.FieldSelectorAttributes{
					RawSelector:  "metadata.name=fixed-policy",
					Requirements: []metav1.FieldSelectorRequirement{{Key: "metadata.name"}},
				}
			},
			want: "both raw and structured field selectors",
		},
		{
			name: "ambiguous label selector",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Checks[0].ResourceAttributes.LabelSelector = &authorizationv1.LabelSelectorAttributes{
					RawSelector:  "app=operator",
					Requirements: []metav1.LabelSelectorRequirement{{Key: "app"}},
				}
			},
			want: "both raw and structured label selectors",
		},
		{
			name: "invalid non-resource path",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Checks[0] = AuthorizationCheck{Name: "health", NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "healthz"}}
			},
			want: "absolute, unpadded path",
		},
		{
			name: "empty non-resource verb",
			mutate: func(b *RBACConvergenceBarrier) {
				b.Probes[0].Checks[0] = AuthorizationCheck{Name: "health", NonResourceAttributes: &authorizationv1.NonResourceAttributes{Path: "/healthz"}}
			},
			want: "empty or padded verb",
		},
		{
			name: "duplicate semantic check",
			mutate: func(b *RBACConvergenceBarrier) {
				duplicate := b.Probes[0].Checks[0]
				duplicate.Name = "same-delete"
				b.Probes[0].Checks = append(b.Probes[0].Checks, duplicate)
			},
			want: "are duplicates",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			barrier := validRBACConvergenceBarrier(validClient)
			test.mutate(barrier)
			err := barrier.Wait(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Wait() error = %v, want substring %q", err, test.want)
			}
		})
	}

	var nilBarrier *RBACConvergenceBarrier
	if err := nilBarrier.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "barrier is nil") {
		t.Fatalf("nil Wait() error = %v", err)
	}
}

func validRBACConvergenceBarrier(client AuthorizationReviewClient) *RBACConvergenceBarrier {
	check := AuthorizationCheck{
		Name: "delete-policy",
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb:     "delete",
			Group:    "admissionregistration.k8s.io",
			Version:  "v1",
			Resource: "validatingadmissionpolicies",
			Name:     "fixed-policy",
		},
	}
	return NewRBACConvergenceBarrier(
		[]NamedAuthorizationReviewClient{{Name: "api-a", Client: client}},
		[]AuthorizationProbe{{
			Subject: AuthorizationSubject{
				Name:   "controller",
				User:   "system:serviceaccount:operator:controller",
				Groups: []string{"system:serviceaccounts"},
			},
			Checks: []AuthorizationCheck{check},
		}},
		[]AuthorizationCheck{check},
		time.Second,
		time.Second,
	)
}

func installDeterministicConvergenceClock(barrier *RBACConvergenceBarrier) {
	current := time.Unix(1_700_000_000, 0)
	barrier.now = func() time.Time { return current }
	barrier.sleep = func(ctx context.Context, duration time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current = current.Add(duration)
		return nil
	}
}

func deniedAuthorizationStatus() authorizationv1.SubjectAccessReviewStatus {
	return authorizationv1.SubjectAccessReviewStatus{Denied: true, Reason: "no matching authorization rule"}
}

type subjectAccessReviewOutcome struct {
	status authorizationv1.SubjectAccessReviewStatus
	err    error
}

type scriptedSubjectAccessReviewClient struct {
	statuses          []authorizationv1.SubjectAccessReviewStatus
	outcomes          []subjectAccessReviewOutcome
	defaultStatus     authorizationv1.SubjectAccessReviewStatus
	nilResponses      int
	mutateRequests    bool
	requests          []*authorizationv1.SubjectAccessReview
	calls             int
	selfStatuses      []authorizationv1.SubjectAccessReviewStatus
	selfOutcomes      []subjectAccessReviewOutcome
	selfDefaultStatus authorizationv1.SubjectAccessReviewStatus
	selfNilResponses  int
	selfRequests      []*authorizationv1.SelfSubjectAccessReview
	selfCalls         int
}

func (c *scriptedSubjectAccessReviewClient) CreateSubjectAccessReview(
	_ context.Context,
	review *authorizationv1.SubjectAccessReview,
	_ metav1.CreateOptions,
) (*authorizationv1.SubjectAccessReview, error) {
	c.requests = append(c.requests, review.DeepCopy())
	index := c.calls
	c.calls++
	if c.mutateRequests {
		review.Spec.User = "mutated-by-client"
		if review.Spec.Groups != nil {
			review.Spec.Groups[0] = "mutated-by-client"
		}
	}
	if c.nilResponses > 0 {
		c.nilResponses--
		return nil, nil
	}
	if index < len(c.outcomes) {
		outcome := c.outcomes[index]
		if outcome.err != nil {
			return nil, outcome.err
		}
		return &authorizationv1.SubjectAccessReview{Status: outcome.status}, nil
	}
	if index < len(c.statuses) {
		return &authorizationv1.SubjectAccessReview{Status: c.statuses[index]}, nil
	}
	return &authorizationv1.SubjectAccessReview{Status: c.defaultStatus}, nil
}

func (c *scriptedSubjectAccessReviewClient) CreateSelfSubjectAccessReview(
	_ context.Context,
	review *authorizationv1.SelfSubjectAccessReview,
	_ metav1.CreateOptions,
) (*authorizationv1.SelfSubjectAccessReview, error) {
	c.selfRequests = append(c.selfRequests, review.DeepCopy())
	index := c.selfCalls
	c.selfCalls++
	if c.selfNilResponses > 0 {
		c.selfNilResponses--
		return nil, nil
	}
	if index < len(c.selfOutcomes) {
		outcome := c.selfOutcomes[index]
		if outcome.err != nil {
			return nil, outcome.err
		}
		return &authorizationv1.SelfSubjectAccessReview{Status: outcome.status}, nil
	}
	if index < len(c.selfStatuses) {
		return &authorizationv1.SelfSubjectAccessReview{Status: c.selfStatuses[index]}, nil
	}
	return &authorizationv1.SelfSubjectAccessReview{Status: c.selfDefaultStatus}, nil
}

func assertAuthorizationProbeMatrix(
	t *testing.T,
	endpoint string,
	requests []*authorizationv1.SubjectAccessReview,
	probes []AuthorizationProbe,
) {
	t.Helper()
	index := 0
	for _, probe := range probes {
		for _, check := range probe.Checks {
			request := requests[index]
			index++
			if request.APIVersion != authorizationv1.SchemeGroupVersion.String() || request.Kind != "SubjectAccessReview" {
				t.Fatalf("%s request TypeMeta = %s/%s", endpoint, request.APIVersion, request.Kind)
			}
			want := subjectAccessReview(probe.Subject, check).Spec
			if !reflect.DeepEqual(request.Spec, want) {
				t.Fatalf("%s request %d spec = %#v, want %#v", endpoint, index, request.Spec, want)
			}
		}
	}
	if index != len(requests) {
		t.Fatalf("%s checked %d requests, received %d", endpoint, index, len(requests))
	}
}

func assertSelfAuthorizationRequestMatrix(
	t *testing.T,
	endpoint string,
	requests []*authorizationv1.SelfSubjectAccessReview,
	checks []AuthorizationCheck,
) {
	t.Helper()
	for index, check := range checks {
		request := requests[index]
		if request.APIVersion != authorizationv1.SchemeGroupVersion.String() || request.Kind != "SelfSubjectAccessReview" {
			t.Fatalf("%s self request TypeMeta = %s/%s", endpoint, request.APIVersion, request.Kind)
		}
		want := selfSubjectAccessReview(check).Spec
		if !reflect.DeepEqual(request.Spec, want) {
			t.Fatalf("%s self request %d spec = %#v, want %#v", endpoint, index+1, request.Spec, want)
		}
	}
	if len(checks) != len(requests) {
		t.Fatalf("%s checked %d self requests, received %d", endpoint, len(checks), len(requests))
	}
}

func TestRBACConvergenceBarrierReportsEndpointSubjectAndCheck(t *testing.T) {
	client := &scriptedSubjectAccessReviewClient{statuses: []authorizationv1.SubjectAccessReviewStatus{{
		EvaluationError: "indeterminate",
	}}}
	barrier := validRBACConvergenceBarrier(client)
	installDeterministicConvergenceClock(barrier)

	err := barrier.Wait(context.Background())
	wantParts := []string{`endpoint "api-a"`, `subject "controller"`, `check "delete-policy"`}
	for _, want := range wantParts {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Wait() error = %v, want %s", err, fmt.Sprintf("%q", want))
		}
	}
}
