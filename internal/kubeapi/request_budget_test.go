package kubeapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestClientRateLimitBudgetErrorRecognizesARealRequestBeforeDeadline(t *testing.T) {
	t.Parallel()
	limiter := flowcontrol.NewTokenBucketRateLimiter(0.001, 1)
	if !limiter.TryAccept() {
		t.Fatal("new limiter did not supply its initial token")
	}
	requests := 0
	client, err := coreclientv1.NewForConfig(&rest.Config{
		Host:        "https://budget.invalid",
		RateLimiter: limiter,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return nil, errors.New("a throttled request must not reach the transport")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = client.ConfigMaps("test").Get(ctx, "marker", metav1.GetOptions{})
	if err == nil || ctx.Err() != nil || requests != 0 {
		t.Fatalf("expected an early local refusal: err=%v, context=%v, requests=%d", err, ctx.Err(), requests)
	}
	if !IsClientRateLimitBudgetError(fmt.Errorf("verify stored contract: %w", err)) {
		t.Fatalf("real client-go budget error was not recognized: %v", err)
	}
}

func TestClientRateLimitBudgetErrorRejectsOtherFailures(t *testing.T) {
	t.Parallel()
	const budget = "rate: Wait(n=1) would exceed context deadline"
	const diagnostic = "client rate limiter Wait returned an error: " + budget
	for name, err := range map[string]error{
		"nil":                            nil,
		"bare limiter error":             errors.New(budget),
		"unstructured diagnostic":        errors.New(diagnostic),
		"unrelated wrapper":              fmt.Errorf("other subsystem: %w", errors.New(budget)),
		"different limiter failure":      fmt.Errorf("client rate limiter Wait returned an error: %w", errors.New("rate: Wait(n=1) exceeds limiter's burst 0")),
		"context deadline":               context.DeadlineExceeded,
		"context canceled":               context.Canceled,
		"contract drift":                 errors.New("stored policy differs from its contract"),
		"API refusal quoting diagnostic": apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "marker", fmt.Errorf("client rate limiter Wait returned an error: %w", errors.New(budget))),
	} {
		t.Run(name, func(t *testing.T) {
			if IsClientRateLimitBudgetError(err) {
				t.Fatalf("unrelated failure was classified as local throttling: %v", err)
			}
		})
	}
}
