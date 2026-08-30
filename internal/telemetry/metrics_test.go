package telemetry

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestMetricsExposeRequiredBoundedSeries(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	metrics := New(registry)

	metrics.ObserveReconciliation(ReconciliationSucceeded)
	metrics.ObserveDrift(operatorv1alpha1.DatabaseEnginePostgreSQL, DriftDetected)
	metrics.ObservePlan(operatorv1alpha1.DatabaseEngineMySQL, true)
	metrics.ObserveApproval(ApprovalAccepted)
	metrics.ObserveApply(ApplyCompleted)
	metrics.ObserveOperation(operatorv1alpha1.OperationApply, OperationSucceeded, 3*time.Second)
	metrics.ObserveFailure(FailureStageApply, FailureOperation)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"ptah_operator_reconciliations_total":      false,
		"ptah_operator_drift_observations_total":   false,
		"ptah_operator_plans_total":                false,
		"ptah_operator_approvals_total":            false,
		"ptah_operator_applies_total":              false,
		"ptah_operator_operation_duration_seconds": false,
		"ptah_operator_failures_total":             false,
	}
	for _, family := range families {
		if _, ok := want[family.GetName()]; ok {
			want[family.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric family %q was not gathered", name)
		}
	}
}

func TestMetricsNormalizeUntrustedLabelValues(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	metrics := New(registry)
	secret := "postgres://user:password@database.example/customer"

	metrics.ObserveReconciliation(ReconciliationResult(secret))
	metrics.ObserveDrift(operatorv1alpha1.DatabaseEngine(secret), DriftOutcome(secret))
	metrics.ObserveApproval(ApprovalOutcome(secret))
	metrics.ObserveApply(ApplyOutcome(secret))
	metrics.ObserveOperation(operatorv1alpha1.OperationType(secret), OperationOutcome(secret), time.Second)
	metrics.ObserveFailure(FailureStage(secret), FailureCategory(secret))

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetValue() == secret {
					t.Fatalf("metric %q exposed an untrusted label value", family.GetName())
				}
			}
		}
	}
}

func TestStageForOperationIsBounded(t *testing.T) {
	t.Parallel()
	if got := StageForOperation(operatorv1alpha1.OperationPlan); got != FailureStagePlan {
		t.Fatalf("StageForOperation(Plan) = %q, want %q", got, FailureStagePlan)
	}
	if got := StageForOperation("credential-bearing-value"); got != FailureStageController {
		t.Fatalf("StageForOperation(unknown) = %q, want %q", got, FailureStageController)
	}
}
