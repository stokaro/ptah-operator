package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
	operationworkload "github.com/stokaro/ptah-operator/internal/workload"
	corev1 "k8s.io/api/core/v1"
)

func TestNativeVerifyFailuresProveRegistryOutage(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/registry-verify-outage.json")
	if err != nil {
		t.Fatal(err)
	}
	var reading struct {
		FaultStartedAt time.Time    `json:"faultStartedAt"`
		Pods           []corev1.Pod `json:"pods"`
	}
	if err := json.Unmarshal(data, &reading); err != nil {
		t.Fatal(err)
	}
	if len(reading.Pods) != 2 || reading.FaultStartedAt.IsZero() {
		t.Fatal("the native reading must contain both families and its outage start")
	}
	families := map[string]bool{}
	for _, pod := range reading.Pods {
		if pod.Labels[operationworkload.LabelOperation] != "verify" {
			t.Fatal("the native regression must exercise Verify")
		}
		s := &scenarios{in: inputs{namespace: pod.Namespace}, load: workload{Schemas: 10, Migrations: 10}}
		family, digest := s.failedRegistryRead(pod, reading.FaultStartedAt)
		if family == "" || digest == "" {
			t.Fatalf("ignored the native Verify failure in Pod %s", pod.Name)
		}
		families[family] = true
	}
	if !families["schema"] || !families["migration"] {
		t.Fatal("the native reading did not prove both loaded families")
	}
}

func TestRegistryOutageAcceptsFreshVerifyFailure(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 6, 12, 11, 45, 0, time.UTC)
	s := &scenarios{in: inputs{namespace: "work"}, load: workload{Schemas: 1, Migrations: 1}}
	for _, family := range []string{"schema", "migration"} {
		pod := resolvePod(t, family, start.Add(time.Second), func(result *runner.Result) {
			result.Operation = runner.OperationVerify
		})
		pod.Labels[operationworkload.LabelOperation] = "verify"
		if got, digest := s.failedRegistryRead(pod, start); got != family || digest == "" {
			t.Fatalf("fresh %s Verify failure during the registry outage was ignored: family=%q digest=%q", family, got, digest)
		}
	}
}

func TestRegistryOutageRejectsFreshDatabaseFailure(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 6, 12, 11, 45, 0, time.UTC)
	s := &scenarios{in: inputs{namespace: "work"}, load: workload{Schemas: 1, Migrations: 1}}
	for family, operation := range map[string]runner.Operation{"schema": runner.OperationObserve, "migration": runner.OperationMigrationHistory} {
		pod := resolvePod(t, family, start.Add(time.Second), func(result *runner.Result) {
			result.Operation = operation
		})
		pod.Labels[operationworkload.LabelOperation] = string(operation)
		if got, _ := s.failedRegistryRead(pod, start); got != "" {
			t.Fatalf("a %s database failure cannot prove the registry outage", operation)
		}
	}
}

func TestRegistryOutageRejectsUnrelatedVerifyRefusals(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 6, 12, 11, 45, 0, time.UTC)
	s := &scenarios{in: inputs{namespace: "work"}, load: workload{Schemas: 1}}
	for _, code := range []string{"verification_refused", "stale_source", "invalid_input", "execution_error", "dispatch_deadline_expired", "verification_policy"} {
		pod := resolvePod(t, "schema", start.Add(time.Second), func(result *runner.Result) {
			result.Operation = runner.OperationVerify
			result.Error.Code = code
			if code == "verification_refused" {
				result.ChildExitCode = 2
				result.VerificationRequirements = []string{"require_signature"}
			}
		})
		pod.Labels[operationworkload.LabelOperation] = "verify"
		if got, _ := s.failedRegistryRead(pod, start); got != "" {
			t.Fatalf("Verify refusal %q cannot prove the registry outage", code)
		}
	}
}
