package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

func resultRotationArgs() []string {
	return []string{"--result-secret-name=result-trust", "--result-journal-secret-name=result-journal", "--result-enrollment-policy=result-enrollment", "--result-service-name=results", "--result-lease-name=result-rotation"}
}

func TestResultRotationFlagsRequireDedicatedObjects(t *testing.T) {
	for i := range resultRotationArgs() {
		args := resultRotationArgs()
		args = append(args[:i], args[i+1:]...)
		if _, _, _, err := parseFlags(append(testRotatorArgs(), args...)); err == nil {
			t.Fatalf("missing flag %d accepted", i)
		}
	}
	for _, bad := range []string{"--lease-name=result-rotation", "--service-name=results", "--secret-name=result-trust", "--staging-secret-name=result-journal", "--result-journal-secret-name=result-trust", "--run-interval=240h"} {
		args := append(testRotatorArgs(), resultRotationArgs()...)
		if _, _, _, err := parseFlags(append(args, bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	base, supervisor, _, err := parseFlags(append(testRotatorArgs(), resultRotationArgs()...))
	if err != nil {
		t.Fatal(err)
	}
	config := supervisor.ResultRotation.config(base)
	if config.LeaseName != "result-rotation" || config.ServiceName != "results" || config.PolicyName != "result-enrollment" || config.SecretName != "result-trust" || config.StagingSecretName != "result-journal" || config.EndpointPortName != "https" || config.RecreateMissingSecret {
		t.Fatal("result rotation options did not reach dedicated config")
	}
}

func TestResultRotationRunsWhileWebhookWaitsAndBothStop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	webhookStarted, resultStarted := make(chan struct{}), make(chan struct{})
	webhookStopped, resultStopped := make(chan struct{}), make(chan struct{})
	webhook := rotationRunnerFunc(func(ctx context.Context) (certrotation.Result, error) {
		close(webhookStarted)
		<-ctx.Done()
		close(webhookStopped)
		return certrotation.Result{}, ctx.Err()
	})
	result := rotationRunnerFunc(func(ctx context.Context) (certrotation.Result, error) {
		close(resultStarted)
		<-ctx.Done()
		close(resultStopped)
		return certrotation.Result{}, ctx.Err()
	})
	config := testServiceRuntimeConfig(webhook)
	config.ResultSupervisor = testServiceRuntimeConfig(result).Supervisor
	config.HealthHandler = config.Supervisor.probes.handler(config.ResultSupervisor.probes)
	listener := newTestConnectionListener()
	done := make(chan error, 1)
	go func() { done <- runServiceOnListener(ctx, config, listener) }()
	for _, started := range []chan struct{}{webhookStarted, resultStarted} {
		select {
		case <-started:
		case <-time.After(serviceTestWait):
			t.Fatal("one certificate loop blocked the other")
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		config.HealthHandler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		want := http.StatusOK
		if path == "/readyz" {
			want = http.StatusServiceUnavailable
		}
		if w.Code != want {
			t.Fatalf("%s = %d, want %d", path, w.Code, want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(serviceTestWait):
		t.Fatal("certificate loops did not stop")
	}
	for _, stopped := range []chan struct{}{webhookStopped, resultStopped} {
		select {
		case <-stopped:
		default:
			t.Fatal("certificate loop still running")
		}
	}
	if !listener.IsClosed() {
		t.Fatal("health listener still running")
	}
}

func TestResultRotationReadinessRequiresEndpointVerdict(t *testing.T) {
	runner := rotationRunnerFunc(func(context.Context) (certrotation.Result, error) {
		return certrotation.Result{Pending: true, RequeueAfter: time.Second}, nil
	})
	config := testServiceRuntimeConfig(runner)
	config.Supervisor.wait = func(_ context.Context, delay time.Duration) bool {
		if delay != time.Second {
			t.Fatalf("pending transition retry: %s", delay)
		}
		if config.Supervisor.probes.ready.Load() {
			t.Fatal("pending transition claimed readiness before endpoint probe")
		}
		return false
	}
	if err := config.Supervisor.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	webhook, result := &probeState{}, &probeState{}
	webhook.setLive(true)
	webhook.setReady(true)
	result.setLive(true)
	handler := webhook.handler(result)
	for _, ready := range []bool{false, true, false} {
		result.setReady(ready)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if (w.Code == http.StatusOK) != ready {
			t.Fatal("aggregate readiness ignored result rotation")
		}
	}
}
