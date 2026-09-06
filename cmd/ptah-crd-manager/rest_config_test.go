package main

import (
	"testing"

	"k8s.io/client-go/rest"
)

func TestBoundedSweepRESTConfigAppliesTheHookRequestRate(t *testing.T) {
	t.Parallel()
	config := &rest.Config{Host: "https://10.0.0.1:443", BearerToken: "token"}
	got := boundedSweepRESTConfig(config)
	if got.QPS != 100 || got.Burst != 200 {
		t.Fatalf("hook client rate = %v/%d, want 100/200", got.QPS, got.Burst)
	}
	if got.Host != "https://10.0.0.1:443" || got.BearerToken != "token" {
		t.Fatal("applying the request rate changed unrelated configuration")
	}
}
