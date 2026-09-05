package crdupgrade

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are intentionally white-box tests because denial attribution is an
// internal admission boundary with no exported API.
func TestHasExactValidatingAdmissionPolicyDenial(t *testing.T) {
	t.Parallel()

	const (
		policy  = "policy-v2"
		binding = "binding-v2"
		denial  = "dedicated enforcement denial"
	)
	wantCause := validatingAdmissionPolicyDenialCauseMessage(policy, binding, denial)
	exactCause := metav1.StatusCause{Message: wantCause}
	statusError := func(status string, details *metav1.StatusDetails) error {
		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status:  status,
			Message: "top-level text is deliberately unrelated",
			Reason:  metav1.StatusReasonInvalid,
			Code:    422,
			Details: details,
		}}
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "exact supported-window status",
			err:  statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}}),
			want: true,
		},
		{
			name: "wrapped status",
			err: fmt.Errorf(
				"update deployment: %w",
				statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}}),
			),
			want: true,
		},
		{name: "nil error"},
		{name: "plain text", err: errors.New(wantCause)},
		{
			name: "success status",
			err:  statusError(metav1.StatusSuccess, &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}}),
		},
		{name: "missing details", err: statusError(metav1.StatusFailure, nil)},
		{
			name: "wrong reason",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Reason:  metav1.StatusReasonConflict,
				Code:    422,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}},
			}},
		},
		{
			name: "wrong code",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Reason:  metav1.StatusReasonInvalid,
				Code:    409,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}},
			}},
		},
		{
			name: "unauthorized envelope",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Reason:  metav1.StatusReasonUnauthorized,
				Code:    401,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}},
			}},
		},
		{
			name: "forbidden envelope",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Reason:  metav1.StatusReasonForbidden,
				Code:    403,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}},
			}},
		},
		{
			name: "request too large envelope",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Reason:  metav1.StatusReasonRequestEntityTooLarge,
				Code:    413,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{exactCause}},
			}},
		},
		{
			name: "multiple causes",
			err: statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{
				exactCause,
				{Message: "unexpected additional cause"},
			}}),
		},
		{
			name: "top-level message only",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Message: wantCause,
				Details: &metav1.StatusDetails{},
			}},
		},
		{
			name: "wrong policy",
			err: statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{{
				Message: validatingAdmissionPolicyDenialCauseMessage("policy-v1", binding, denial),
			}}}),
		},
		{
			name: "wrong binding",
			err: statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{{
				Message: validatingAdmissionPolicyDenialCauseMessage(policy, "binding-v1", denial),
			}}}),
		},
		{
			name: "wrong denial",
			err: statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{{
				Message: validatingAdmissionPolicyDenialCauseMessage(policy, binding, denial+" suffix"),
			}}}),
		},
		{
			name: "nonempty cause type",
			err: statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{{
				Type: metav1.CauseTypeFieldValueInvalid, Message: wantCause,
			}}}),
		},
		{
			name: "nonempty cause field",
			err: statusError(metav1.StatusFailure, &metav1.StatusDetails{Causes: []metav1.StatusCause{{
				Field: "spec", Message: wantCause,
			}}}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := hasExactValidatingAdmissionPolicyDenial(test.err, policy, binding, denial); got != test.want {
				t.Fatalf("hasExactValidatingAdmissionPolicyDenial() = %t, want %t", got, test.want)
			}
		})
	}
}
