package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/sourcepolicy"
)

// sourcePolicyFor reads the source policy a source names, with its binding,
// or nothing when it names none. A policy that is missing, mutable or not a
// policy document refuses the source; a failure to reach the API server is
// returned as it is, to be retried.
func sourcePolicyFor(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	spec operatorv1alpha1.SourceSpec,
) (*sourcepolicy.Policy, *operatorv1alpha1.SourcePolicyBinding, error) {
	if spec.SourcePolicyFrom == nil {
		return nil, nil, nil
	}
	policy, binding, err := sourcepolicy.Read(ctx, reader, namespace, spec.SourcePolicyFrom)
	if err != nil {
		var status apierrors.APIStatus
		if errors.As(err, &status) && !apierrors.IsNotFound(err) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	return &policy, &operatorv1alpha1.SourcePolicyBinding{UID: binding.UID, Digest: binding.Digest}, nil
}

// sourceAuthority is the host[:port] a Git or HTTPS source's URL names, as
// url.Parse reads it, or "" for a URL it refuses. It is not folded: the
// source policy compares it as printable ASCII and refuses any other host by
// name, where strings.ToLower would read "g\u0130thub.com" as github.com.
func sourceAuthority(source string) string {
	parsed, err := url.Parse(source)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// checkGitSource refuses a Git source its policy does not allow: its host
// and, once it is known, how much the selected files hold.
func checkGitSource(policy *sourcepolicy.Policy, spec *operatorv1alpha1.GitSourceSpec, contentBytes int64) error {
	if policy == nil {
		return nil
	}
	if err := policy.CheckGitHost(sourceAuthority(spec.URL)); err != nil {
		return fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	if err := policy.CheckContentBytes(contentBytes); err != nil {
		return fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	return nil
}

// sourcePolicyMoved reports whether the source policy a resolved source was
// checked against is no longer the one the spec names: another object, none
// where one was named, or one that cannot be read. Such a source is resolved
// again, under the policy that holds now.
func sourcePolicyMoved(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	spec operatorv1alpha1.SourceSpec,
	recorded *operatorv1alpha1.SourcePolicyBinding,
	resolved bool,
) (bool, error) {
	if !resolved || (spec.ConfigMap == nil && spec.Git == nil && spec.HTTPS == nil) {
		return false, nil
	}
	_, live, err := sourcePolicyFor(ctx, reader, namespace, spec)
	if err != nil {
		if errors.Is(err, errSourceRefused) {
			return true, nil
		}
		return false, err
	}
	return !sourcebinding.SamePolicy(live, recorded), nil
}
