package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The policy binding wraps the reader's error, so each answer is checked
// bare and as the binding returns it.
func TestOnlyAnAnswerTheServerMayChangeIsTransient(t *testing.T) {
	t.Parallel()
	configMaps := corev1.Resource("configmaps")
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, row := range []struct {
		name string
		err  error
		want bool
	}{
		{"unavailable", apierrors.NewServiceUnavailable("etcd leader changed"), true},
		{"timed out", apierrors.NewTimeoutError("the request timed out", 1), true},
		{"server timeout", apierrors.NewServerTimeout(configMaps, "get", 1), true},
		{"throttled", apierrors.NewTooManyRequests("slow down", 1), true},
		{"internal error", apierrors.NewInternalError(errors.New("boom")), true},
		{"unexpected server answer", apierrors.NewGenericServerResponse(502, "get", configMaps, "policy", "bad gateway", 0, true), true},
		{"connection refused", &url.Error{Op: "Get", URL: "https://apiserver/api", Err: refused}, true},
		{"connection reset", &url.Error{Op: "Get", URL: "https://apiserver/api", Err: syscall.ECONNRESET}, true},
		{"cut off mid answer", &url.Error{Op: "Get", URL: "https://apiserver/api", Err: io.ErrUnexpectedEOF}, true},
		{"out of time", context.DeadlineExceeded, true},

		{"no error", nil, false},
		{"gone", apierrors.NewNotFound(configMaps, "policy"), false},
		{"forbidden", apierrors.NewForbidden(configMaps, "policy", errors.New("no")), false},
		{"conflict", apierrors.NewConflict(configMaps, "policy", errors.New("moved")), false},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "policy", nil), false},
		{"canceled", context.Canceled, false},
		// The binding's own refusals are plain errors, not API statuses, and
		// still not transport errors.
		{"mutable policy", errors.New("verification policy ConfigMap must set immutable: true"), false},
		{"missing key", errors.New("verification policy ConfigMap key is missing"), false},
		{"incomplete reference", errors.New("verification policy reference is incomplete"), false},
	} {
		for _, err := range []error{row.err, wrapped(row.err)} {
			if got := transientAPIError(err); got != row.want {
				t.Errorf("%s (%v): transient = %t, want %t", row.name, err, got, row.want)
			}
		}
	}
}

func wrapped(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("read verification policy ConfigMap: %w", err)
}
