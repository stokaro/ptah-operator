package crdupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AuthorizationReviewClient is the exact API surface required from one
// independently addressed API server. SubjectAccessReview probes canonical
// retired identities; SelfSubjectAccessReview probes the cleanup Job's current
// authenticated credential, including identity attributes supplied by the
// authenticator. Callers should not put a load-balanced client behind more than
// one endpoint entry: every entry is intended to provide distinct
// authorizer-cache evidence.
type AuthorizationReviewClient interface {
	CreateSubjectAccessReview(context.Context, *authorizationv1.SubjectAccessReview, metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error)
	CreateSelfSubjectAccessReview(context.Context, *authorizationv1.SelfSubjectAccessReview, metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error)
}

// NamedAuthorizationReviewClient identifies one independently addressed API
// server participating in authorization convergence.
type NamedAuthorizationReviewClient struct {
	Name   string
	Client AuthorizationReviewClient
}

// AuthorizationEndpointProvider returns the complete current set of directly
// addressable API endpoints. A convergence barrier refreshes this set before
// every authorization sweep so topology churn cannot leave a newly advertised
// endpoint outside the stability window.
type AuthorizationEndpointProvider func(context.Context) ([]NamedAuthorizationReviewClient, error)

// AuthorizationSubject is the exact authenticated identity presented to
// every SubjectAccessReview. Name is a diagnostic identifier and is not sent
// to the API server.
type AuthorizationSubject struct {
	Name   string
	User   string
	UID    string
	Groups []string
	Extra  map[string]authorizationv1.ExtraValue
}

// AuthorizationCheck is one formerly privileged operation that must be
// explicitly denied. Exactly one attribute pointer must be non-nil. Name is a
// diagnostic identifier and is not sent to the API server.
type AuthorizationCheck struct {
	Name                  string
	ResourceAttributes    *authorizationv1.ResourceAttributes
	NonResourceAttributes *authorizationv1.NonResourceAttributes
}

// AuthorizationProbe binds one retired canonical identity only to operations
// that its rendered RBAC grants previously authorized. Keeping this mapping
// explicit avoids treating a grant held by a different retired identity as
// evidence that this subject's removed binding has not converged.
type AuthorizationProbe struct {
	Subject AuthorizationSubject
	Checks  []AuthorizationCheck
}

// RBACConvergenceBarrier waits for all configured API servers to report that
// no subject/check pair is allowed for one uninterrupted stability window. This
// is deliberately stronger than re-reading deleted RBAC objects from storage:
// authorizer caches on different API servers can converge at different times.
type RBACConvergenceBarrier struct {
	Endpoints         []NamedAuthorizationReviewClient
	EndpointProvider  AuthorizationEndpointProvider
	Probes            []AuthorizationProbe
	SelfChecks        []AuthorizationCheck
	PollEvery         time.Duration
	StabilityDuration time.Duration

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewRBACConvergenceBarrier constructs an authorization revocation barrier.
// Callers that mutate privileges should invoke Validate during their read-only
// preflight; Wait validates again before making any API request.
func NewRBACConvergenceBarrier(
	endpoints []NamedAuthorizationReviewClient,
	probes []AuthorizationProbe,
	selfChecks []AuthorizationCheck,
	pollEvery time.Duration,
	stabilityDuration time.Duration,
) *RBACConvergenceBarrier {
	return &RBACConvergenceBarrier{
		Endpoints:         endpoints,
		Probes:            probes,
		SelfChecks:        selfChecks,
		PollEvery:         pollEvery,
		StabilityDuration: stabilityDuration,
	}
}

// Validate checks the complete endpoint, subject, operation, and timing
// configuration without making an API request.
func (b *RBACConvergenceBarrier) Validate() error {
	return b.validate()
}

// Wait polls every endpoint. Allowed and transport-error results reset the
// global stability window and are retried. A normal no-opinion result is the
// expected Kubernetes RBAC outcome once no rule authorizes the request; the
// SubjectAccessReview API represents it as allowed=false, denied=false. An
// evaluationError is ambiguous and fails immediately. Context cancellation and
// deadlines are returned to the caller.
func (b *RBACConvergenceBarrier) Wait(ctx context.Context) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return errors.New("authorization convergence context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	now := b.now
	if now == nil {
		now = time.Now
	}
	sleep := b.sleep
	if sleep == nil {
		sleep = sleepForAuthorizationConvergence
	}

	var stableSince time.Time
	endpointSet := authorizationEndpointSetKey(b.Endpoints)
	for {
		endpointSetChanged := false
		if b.EndpointProvider != nil {
			endpoints, refreshErr := b.EndpointProvider(ctx)
			if refreshErr != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				stableSince = time.Time{}
				if sleepErr := sleep(ctx, b.PollEvery); sleepErr != nil {
					return fmt.Errorf(
						"authorization endpoint discovery did not recover; last observation: %v: %w",
						refreshErr,
						sleepErr,
					)
				}
				continue
			}
			if err := validateAuthorizationEndpoints(endpoints); err != nil {
				return fmt.Errorf("validate refreshed authorization endpoints: %w", err)
			}
			refreshedSet := authorizationEndpointSetKey(endpoints)
			endpointSetChanged = refreshedSet != endpointSet
			endpointSet = refreshedSet
			b.Endpoints = endpoints
		}

		allDenied, err := b.pollOnce(ctx)
		if err != nil {
			return err
		}

		observedAt := now()
		if endpointSetChanged {
			stableSince = time.Time{}
		}
		if allDenied {
			if stableSince.IsZero() || observedAt.Before(stableSince) {
				stableSince = observedAt
			} else if observedAt.Sub(stableSince) >= b.StabilityDuration {
				if err := ctx.Err(); err != nil {
					return err
				}
				return nil
			}
		} else {
			stableSince = time.Time{}
		}

		if err := sleep(ctx, b.PollEvery); err != nil {
			return err
		}
	}
}

func (b *RBACConvergenceBarrier) pollOnce(ctx context.Context) (bool, error) {
	allDenied := true
	for _, endpoint := range b.Endpoints {
		for _, probe := range b.Probes {
			for _, check := range probe.Checks {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				review := subjectAccessReview(probe.Subject, check)
				result, err := endpoint.Client.CreateSubjectAccessReview(ctx, review, metav1.CreateOptions{})
				if err != nil {
					if contextErr := ctx.Err(); contextErr != nil {
						return false, contextErr
					}
					// A temporary endpoint or transport failure cannot prove
					// revocation. Retry it after resetting the stable window.
					allDenied = false
					continue
				}
				if result == nil {
					return false, fmt.Errorf(
						"authorization endpoint %q returned a nil SubjectAccessReview for subject %q check %q",
						endpoint.Name,
						probe.Subject.Name,
						check.Name,
					)
				}
				if result.Status.EvaluationError != "" {
					return false, fmt.Errorf(
						"authorization endpoint %q could not evaluate subject %q check %q: %s",
						endpoint.Name,
						probe.Subject.Name,
						check.Name,
						result.Status.EvaluationError,
					)
				}
				if result.Status.Allowed {
					allDenied = false
				}
			}
		}
		for _, check := range b.SelfChecks {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			review := selfSubjectAccessReview(check)
			result, err := endpoint.Client.CreateSelfSubjectAccessReview(ctx, review, metav1.CreateOptions{})
			if err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return false, contextErr
				}
				// As with canonical probes, a transport failure is
				// inconclusive and resets the stability window.
				allDenied = false
				continue
			}
			if result == nil {
				return false, fmt.Errorf(
					"authorization endpoint %q returned a nil SelfSubjectAccessReview for current cleanup credential check %q",
					endpoint.Name,
					check.Name,
				)
			}
			if result.Status.EvaluationError != "" {
				return false, fmt.Errorf(
					"authorization endpoint %q could not evaluate current cleanup credential check %q: %s",
					endpoint.Name,
					check.Name,
					result.Status.EvaluationError,
				)
			}
			if result.Status.Allowed {
				allDenied = false
			}
		}
	}
	return allDenied, nil
}

func (b *RBACConvergenceBarrier) validate() error {
	if b == nil {
		return errors.New("authorization convergence barrier is nil")
	}
	if b.PollEvery <= 0 {
		return errors.New("authorization convergence poll interval must be positive")
	}
	if b.StabilityDuration <= 0 {
		return errors.New("authorization convergence stability duration must be positive")
	}
	if len(b.Endpoints) == 0 {
		return errors.New("authorization convergence endpoints are empty")
	}
	if len(b.Probes) == 0 {
		return errors.New("authorization convergence probes are empty")
	}
	if len(b.SelfChecks) == 0 {
		return errors.New("authorization convergence current-subject checks are empty")
	}

	if err := validateAuthorizationEndpoints(b.Endpoints); err != nil {
		return err
	}

	subjectNames := make(map[string]struct{}, len(b.Probes))
	subjectKeys := make(map[string]string, len(b.Probes))
	for index, probe := range b.Probes {
		subject := probe.Subject
		if err := validateDiagnosticName(subject.Name, "subject", index); err != nil {
			return err
		}
		if _, duplicate := subjectNames[subject.Name]; duplicate {
			return fmt.Errorf("authorization convergence subject name %q is duplicated", subject.Name)
		}
		subjectNames[subject.Name] = struct{}{}
		if subject.User != strings.TrimSpace(subject.User) {
			return fmt.Errorf("authorization convergence subject %q has a padded user", subject.Name)
		}
		if err := validateSubjectSets(subject); err != nil {
			return err
		}
		if subject.User == "" && len(subject.Groups) == 0 {
			return fmt.Errorf("authorization convergence subject %q has no user or groups", subject.Name)
		}
		key, err := authorizationSubjectKey(subject)
		if err != nil {
			return fmt.Errorf("encode authorization convergence subject %q: %w", subject.Name, err)
		}
		if previous, duplicate := subjectKeys[key]; duplicate {
			return fmt.Errorf("authorization convergence subjects %q and %q are duplicates", previous, subject.Name)
		}
		subjectKeys[key] = subject.Name
		if err := validateAuthorizationChecks(probe.Checks, "probe "+fmt.Sprintf("%q", subject.Name)); err != nil {
			return err
		}
	}
	return validateAuthorizationChecks(b.SelfChecks, "current cleanup credential")
}

func validateAuthorizationChecks(checks []AuthorizationCheck, owner string) error {
	if len(checks) == 0 {
		return fmt.Errorf("authorization convergence %s checks are empty", owner)
	}
	checkNames := make(map[string]struct{}, len(checks))
	checkKeys := make(map[string]string, len(checks))
	for index, check := range checks {
		if err := validateDiagnosticName(check.Name, "check", index); err != nil {
			return fmt.Errorf("authorization convergence %s: %w", owner, err)
		}
		if _, duplicate := checkNames[check.Name]; duplicate {
			return fmt.Errorf("authorization convergence %s check name %q is duplicated", owner, check.Name)
		}
		checkNames[check.Name] = struct{}{}
		if err := validateAuthorizationCheck(check); err != nil {
			return fmt.Errorf("authorization convergence %s: %w", owner, err)
		}
		key, err := authorizationCheckKey(check)
		if err != nil {
			return fmt.Errorf("encode authorization convergence %s check %q: %w", owner, check.Name, err)
		}
		if previous, duplicate := checkKeys[key]; duplicate {
			return fmt.Errorf("authorization convergence %s checks %q and %q are duplicates", owner, previous, check.Name)
		}
		checkKeys[key] = check.Name
	}
	return nil
}

func validateAuthorizationEndpoints(endpoints []NamedAuthorizationReviewClient) error {
	if len(endpoints) == 0 {
		return errors.New("authorization convergence endpoints are empty")
	}
	endpointNames := make(map[string]struct{}, len(endpoints))
	for index, endpoint := range endpoints {
		if err := validateDiagnosticName(endpoint.Name, "endpoint", index); err != nil {
			return err
		}
		if isNilAuthorizationReviewClient(endpoint.Client) {
			return fmt.Errorf("authorization convergence endpoint %q has a nil client", endpoint.Name)
		}
		if _, duplicate := endpointNames[endpoint.Name]; duplicate {
			return fmt.Errorf("authorization convergence endpoint %q is duplicated", endpoint.Name)
		}
		endpointNames[endpoint.Name] = struct{}{}
	}
	return nil
}

func authorizationEndpointSetKey(endpoints []NamedAuthorizationReviewClient) string {
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.Name)
	}
	sort.Strings(names)
	return strings.Join(names, "\x00")
}

func isNilAuthorizationReviewClient(client AuthorizationReviewClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validateDiagnosticName(name, kind string, index int) error {
	if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
		return fmt.Errorf("authorization convergence %s at index %d has an empty or padded name", kind, index)
	}
	return nil
}

func validateSubjectSets(subject AuthorizationSubject) error {
	groups := make(map[string]struct{}, len(subject.Groups))
	for _, group := range subject.Groups {
		if strings.TrimSpace(group) == "" || group != strings.TrimSpace(group) {
			return fmt.Errorf("authorization convergence subject %q has an empty or padded group", subject.Name)
		}
		if _, duplicate := groups[group]; duplicate {
			return fmt.Errorf("authorization convergence subject %q has duplicate group %q", subject.Name, group)
		}
		groups[group] = struct{}{}
	}
	for key, values := range subject.Extra {
		if strings.TrimSpace(key) == "" || key != strings.TrimSpace(key) {
			return fmt.Errorf("authorization convergence subject %q has an empty or padded extra key", subject.Name)
		}
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("authorization convergence subject %q has duplicate value for extra key %q", subject.Name, key)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func validateAuthorizationCheck(check AuthorizationCheck) error {
	if (check.ResourceAttributes == nil) == (check.NonResourceAttributes == nil) {
		return fmt.Errorf("authorization convergence check %q must set exactly one attribute type", check.Name)
	}
	if attributes := check.ResourceAttributes; attributes != nil {
		if strings.TrimSpace(attributes.Verb) == "" || attributes.Verb != strings.TrimSpace(attributes.Verb) {
			return fmt.Errorf("authorization convergence resource check %q has an empty or padded verb", check.Name)
		}
		if strings.TrimSpace(attributes.Resource) == "" || attributes.Resource != strings.TrimSpace(attributes.Resource) {
			return fmt.Errorf("authorization convergence resource check %q has an empty or padded resource", check.Name)
		}
		if err := validateSelectorPair(check.Name, "field", attributes.FieldSelector); err != nil {
			return err
		}
		if err := validateSelectorPair(check.Name, "label", attributes.LabelSelector); err != nil {
			return err
		}
		return nil
	}
	attributes := check.NonResourceAttributes
	if strings.TrimSpace(attributes.Verb) == "" || attributes.Verb != strings.TrimSpace(attributes.Verb) {
		return fmt.Errorf("authorization convergence non-resource check %q has an empty or padded verb", check.Name)
	}
	if !strings.HasPrefix(attributes.Path, "/") || attributes.Path != strings.TrimSpace(attributes.Path) {
		return fmt.Errorf("authorization convergence non-resource check %q must have an absolute, unpadded path", check.Name)
	}
	return nil
}

type authorizationSelector interface {
	selectorParts() (string, int)
}

type fieldSelectorAdapter struct {
	selector *authorizationv1.FieldSelectorAttributes
}

func (a fieldSelectorAdapter) selectorParts() (string, int) {
	return a.selector.RawSelector, len(a.selector.Requirements)
}

type labelSelectorAdapter struct {
	selector *authorizationv1.LabelSelectorAttributes
}

func (a labelSelectorAdapter) selectorParts() (string, int) {
	return a.selector.RawSelector, len(a.selector.Requirements)
}

func validateSelectorPair(checkName, kind string, selector any) error {
	var adapted authorizationSelector
	switch value := selector.(type) {
	case *authorizationv1.FieldSelectorAttributes:
		if value == nil {
			return nil
		}
		adapted = fieldSelectorAdapter{selector: value}
	case *authorizationv1.LabelSelectorAttributes:
		if value == nil {
			return nil
		}
		adapted = labelSelectorAdapter{selector: value}
	default:
		return fmt.Errorf("authorization convergence resource check %q has an unsupported %s selector", checkName, kind)
	}
	raw, requirements := adapted.selectorParts()
	if raw != "" && requirements != 0 {
		return fmt.Errorf(
			"authorization convergence resource check %q sets both raw and structured %s selectors",
			checkName,
			kind,
		)
	}
	return nil
}

func authorizationSubjectKey(subject AuthorizationSubject) (string, error) {
	groups := append([]string(nil), subject.Groups...)
	sort.Strings(groups)
	extra := make(map[string]authorizationv1.ExtraValue, len(subject.Extra))
	for key, values := range subject.Extra {
		copied := append(authorizationv1.ExtraValue(nil), values...)
		sort.Strings(copied)
		extra[key] = copied
	}
	encoded, err := json.Marshal(authorizationv1.SubjectAccessReviewSpec{
		User:   subject.User,
		UID:    subject.UID,
		Groups: groups,
		Extra:  extra,
	})
	return string(encoded), err
}

func authorizationCheckKey(check AuthorizationCheck) (string, error) {
	encoded, err := json.Marshal(authorizationv1.SubjectAccessReviewSpec{
		ResourceAttributes:    check.ResourceAttributes,
		NonResourceAttributes: check.NonResourceAttributes,
	})
	return string(encoded), err
}

func subjectAccessReview(subject AuthorizationSubject, check AuthorizationCheck) *authorizationv1.SubjectAccessReview {
	extra := make(map[string]authorizationv1.ExtraValue, len(subject.Extra))
	for key, values := range subject.Extra {
		extra[key] = append(authorizationv1.ExtraValue(nil), values...)
	}
	spec := authorizationv1.SubjectAccessReviewSpec{
		User:   subject.User,
		UID:    subject.UID,
		Groups: append([]string(nil), subject.Groups...),
		Extra:  extra,
	}
	if check.ResourceAttributes != nil {
		spec.ResourceAttributes = check.ResourceAttributes.DeepCopy()
	} else {
		spec.NonResourceAttributes = check.NonResourceAttributes.DeepCopy()
	}
	return &authorizationv1.SubjectAccessReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: authorizationv1.SchemeGroupVersion.String(),
			Kind:       "SubjectAccessReview",
		},
		Spec: spec,
	}
}

func selfSubjectAccessReview(check AuthorizationCheck) *authorizationv1.SelfSubjectAccessReview {
	spec := authorizationv1.SelfSubjectAccessReviewSpec{}
	if check.ResourceAttributes != nil {
		spec.ResourceAttributes = check.ResourceAttributes.DeepCopy()
	} else {
		spec.NonResourceAttributes = check.NonResourceAttributes.DeepCopy()
	}
	return &authorizationv1.SelfSubjectAccessReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: authorizationv1.SchemeGroupVersion.String(),
			Kind:       "SelfSubjectAccessReview",
		},
		Spec: spec,
	}
}

func sleepForAuthorizationConvergence(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
