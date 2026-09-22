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

	metrics.ObserveReconciliation(FamilySchema, ReconciliationSucceeded)
	metrics.ObserveDrift(operatorv1alpha1.DatabaseEnginePostgreSQL, DriftDetected)
	metrics.ObservePlan(FamilySchema, operatorv1alpha1.DatabaseEngineMySQL, PlanDestructive)
	metrics.ObservePlan(FamilyMigration, operatorv1alpha1.DatabaseEngineMySQL, PlanImpactUnknown)
	metrics.ObserveApproval(FamilySchema, ApprovalAccepted)
	metrics.ObserveApply(FamilyMigration, ApplyCompleted)
	metrics.ObserveOperation(FamilySchema, OperationApply, OperationSucceeded, 3*time.Second)
	metrics.ObserveOperation(FamilyMigration, OperationHistory, OperationSucceeded, time.Second)
	metrics.ObserveFailure(FamilyMigration, FailureStageHistory, FailureOperation)

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

	metrics.ObserveReconciliation(ResourceFamily(secret), ReconciliationResult(secret))
	metrics.ObserveDrift(operatorv1alpha1.DatabaseEngine(secret), DriftOutcome(secret))
	metrics.ObservePlan(ResourceFamily(secret), operatorv1alpha1.DatabaseEngine(secret), PlanImpact(secret))
	metrics.ObserveApproval(ResourceFamily(secret), ApprovalOutcome(secret))
	metrics.ObserveApply(ResourceFamily(secret), ApplyOutcome(secret))
	metrics.ObserveOperation(ResourceFamily(secret), Operation(secret), OperationOutcome(secret), time.Second)
	metrics.ObserveFailure(ResourceFamily(secret), FailureStage(secret), FailureCategory(secret))

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

func TestStageForMigrationOperationIsBounded(t *testing.T) {
	t.Parallel()
	// History is the one a schema never performs, and mapping it through the
	// schema enum is what produced an "unknown" stage for a real operation.
	if got := StageForMigrationOperation(operatorv1alpha1.MigrationOperationHistory); got != FailureStageHistory {
		t.Fatalf("StageForMigrationOperation(History) = %q, want %q", got, FailureStageHistory)
	}
	if got := StageForMigrationOperation("credential-bearing-value"); got != FailureStageController {
		t.Fatalf("StageForMigrationOperation(unknown) = %q, want %q", got, FailureStageController)
	}
}

func TestOperationLabelsCoverBothFamilies(t *testing.T) {
	t.Parallel()
	if got := OperationForMigration(operatorv1alpha1.MigrationOperationHistory); got != OperationHistory {
		t.Fatalf("OperationForMigration(History) = %q, want %q", got, OperationHistory)
	}
	if got := OperationForSchema(operatorv1alpha1.OperationObserve); got != OperationObserve {
		t.Fatalf("OperationForSchema(Observe) = %q, want %q", got, OperationObserve)
	}
	// An operation neither mapper recognizes must reach the metric as
	// "unknown" rather than as a label the caller chose.
	if got := operationLabel(Operation("postgres://user:password@database.example")); got != "unknown" {
		t.Fatalf("operationLabel(untrusted) = %q, want unknown", got)
	}
	if got := OperationForSchema("Reticulate"); got != "" {
		t.Fatalf("OperationForSchema(unrecognized) = %q, want the empty label", got)
	}
}

func TestDestructivePlanKeepsTheSpellingABooleanLabelHad(t *testing.T) {
	t.Parallel()
	if got := DestructivePlan(true); got != PlanDestructive || string(got) != "true" {
		t.Fatalf("DestructivePlan(true) = %q, want the label %q", got, "true")
	}
	if got := DestructivePlan(false); got != PlanNonDestructive || string(got) != "false" {
		t.Fatalf("DestructivePlan(false) = %q, want the label %q", got, "false")
	}
}
