// Package telemetry defines bounded-cardinality Prometheus telemetry for the
// reconciliation state machines of both resource families.
package telemetry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// ResourceFamily says which family of resources an observation came from.
// Both families run the same lifecycle shape against the same database, and
// an alert on an uncertain outcome wants both; a reader who needs one asks for
// it by label.
type ResourceFamily string

const (
	FamilySchema    ResourceFamily = "schema"
	FamilyMigration ResourceFamily = "migration"
)

// Operation is the logical operation an observation belongs to, across both
// families. A migration reads its recorded history where a schema observes the
// live database, so the set is the union rather than either family's enum:
// mapping one family's operation onto the other's leaves a real operation
// labeled "unknown".
type Operation string

const (
	OperationResolve Operation = "resolve"
	OperationVerify  Operation = "verify"
	OperationObserve Operation = "observe"
	OperationPlan    Operation = "plan"
	OperationApply   Operation = "apply"
	OperationHistory Operation = "history"
)

// PlanImpact says whether applying a plan can lose data.
//
// It is three-valued because only a schema plan is examined for it. Nothing
// computes it for a migration, whose statements are written by hand, so a
// migration plan reports "unknown" rather than claiming a check that never
// ran. The two definite values keep the spelling a boolean label had.
type PlanImpact string

const (
	PlanDestructive    PlanImpact = "true"
	PlanNonDestructive PlanImpact = "false"
	PlanImpactUnknown  PlanImpact = "unknown"
)

// DestructivePlan maps a computed destructive flag onto the label.
func DestructivePlan(destructive bool) PlanImpact {
	if destructive {
		return PlanDestructive
	}
	return PlanNonDestructive
}

// ReconciliationResult is a closed set of reconciliation outcomes.
type ReconciliationResult string

const (
	ReconciliationSucceeded ReconciliationResult = "success"
	ReconciliationFailed    ReconciliationResult = "error"
)

// DriftOutcome is a closed set of database observation outcomes.
type DriftOutcome string

const (
	DriftDetected DriftOutcome = "detected"
	DriftInSync   DriftOutcome = "in_sync"
)

// ApprovalOutcome is a closed set of approval lifecycle observations.
type ApprovalOutcome string

const (
	ApprovalRequired ApprovalOutcome = "required"
	ApprovalAccepted ApprovalOutcome = "accepted"
	ApprovalStale    ApprovalOutcome = "stale"
)

// ApplyOutcome is a closed set of apply lifecycle observations.
type ApplyOutcome string

const (
	ApplyStarted   ApplyOutcome = "started"
	ApplyCompleted ApplyOutcome = "completed"
	ApplyUncertain ApplyOutcome = "uncertain"
	ApplyStale     ApplyOutcome = "stale"
)

// OperationOutcome is a closed set of logical operation outcomes.
type OperationOutcome string

const (
	OperationSucceeded OperationOutcome = "success"
	OperationUncertain OperationOutcome = "uncertain"
	OperationStale     OperationOutcome = "stale"
	OperationCanceled  OperationOutcome = "canceled"
)

// FailureStage identifies the bounded state-machine stage that observed a
// failure. It deliberately excludes object identity and arbitrary error text.
type FailureStage string

const (
	FailureStageController FailureStage = "controller"
	FailureStageResolve    FailureStage = "resolve"
	FailureStageVerify     FailureStage = "verify"
	FailureStageObserve    FailureStage = "observe"
	FailureStagePlan       FailureStage = "plan"
	FailureStageApply      FailureStage = "apply"
	FailureStageHistory    FailureStage = "history"
)

// FailureCategory identifies a bounded, operationally useful failure class.
type FailureCategory string

const (
	FailureInfrastructure FailureCategory = "infrastructure"
	FailureConfiguration  FailureCategory = "configuration"
	FailureOperation      FailureCategory = "operation"
	FailurePolicyChanged  FailureCategory = "policy_changed"
	FailureStaleInput     FailureCategory = "stale_input"
	FailureUncertain      FailureCategory = "uncertain_outcome"
)

// Observer is the controller-facing telemetry contract. Implementations must
// not derive labels from Kubernetes object identity, content digests, Secret
// data, or error strings.
type Observer interface {
	ObserveReconciliation(ResourceFamily, ReconciliationResult)
	ObserveDrift(operatorv1alpha1.DatabaseEngine, DriftOutcome)
	ObservePlan(ResourceFamily, operatorv1alpha1.DatabaseEngine, PlanImpact)
	ObserveApproval(ResourceFamily, ApprovalOutcome)
	ObserveApply(ResourceFamily, ApplyOutcome)
	ObserveOperation(ResourceFamily, Operation, OperationOutcome, time.Duration)
	ObserveFailure(ResourceFamily, FailureStage, FailureCategory)
}

// Metrics implements Observer with Prometheus collectors.
type Metrics struct {
	reconciliations   *prometheus.CounterVec
	driftObservations *prometheus.CounterVec
	plans             *prometheus.CounterVec
	approvals         *prometheus.CounterVec
	applies           *prometheus.CounterVec
	operationDuration *prometheus.HistogramVec
	failures          *prometheus.CounterVec
}

// New constructs and registers one independent collector set. Call it once
// per registry; tests should pass their own prometheus.Registry.
func New(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		reconciliations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "reconciliations_total",
			Help:      "Total reconciliations by resource family and bounded outcome.",
		}, []string{"family", "result"}),
		driftObservations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "drift_observations_total",
			Help:      "Total successful database drift observations.",
		}, []string{"engine", "outcome"}),
		plans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "plans_total",
			Help:      "Total immutable plans published by the controller.",
		}, []string{"family", "engine", "destructive"}),
		approvals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "approvals_total",
			Help:      "Total approval lifecycle transitions.",
		}, []string{"family", "outcome"}),
		applies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "applies_total",
			Help:      "Total apply lifecycle transitions.",
		}, []string{"family", "outcome"}),
		operationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ptah_operator",
			Name:      "operation_duration_seconds",
			Help:      "Duration of completed logical operations.",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 16),
		}, []string{"family", "operation", "outcome"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "failures_total",
			Help:      "Total failures by bounded state-machine stage and category.",
		}, []string{"family", "stage", "category"}),
	}
	registerer.MustRegister(
		metrics.reconciliations,
		metrics.driftObservations,
		metrics.plans,
		metrics.approvals,
		metrics.applies,
		metrics.operationDuration,
		metrics.failures,
	)
	return metrics
}

func (m *Metrics) ObserveReconciliation(family ResourceFamily, result ReconciliationResult) {
	m.reconciliations.WithLabelValues(resourceFamily(family), reconciliationResult(result)).Inc()
}

func (m *Metrics) ObserveDrift(engine operatorv1alpha1.DatabaseEngine, outcome DriftOutcome) {
	m.driftObservations.WithLabelValues(databaseEngine(engine), driftOutcome(outcome)).Inc()
}

func (m *Metrics) ObservePlan(family ResourceFamily, engine operatorv1alpha1.DatabaseEngine, impact PlanImpact) {
	m.plans.WithLabelValues(resourceFamily(family), databaseEngine(engine), planImpact(impact)).Inc()
}

func (m *Metrics) ObserveApproval(family ResourceFamily, outcome ApprovalOutcome) {
	m.approvals.WithLabelValues(resourceFamily(family), approvalOutcome(outcome)).Inc()
}

func (m *Metrics) ObserveApply(family ResourceFamily, outcome ApplyOutcome) {
	m.applies.WithLabelValues(resourceFamily(family), applyOutcome(outcome)).Inc()
}

func (m *Metrics) ObserveOperation(family ResourceFamily, operation Operation, outcome OperationOutcome, duration time.Duration) {
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.operationDuration.
		WithLabelValues(resourceFamily(family), operationLabel(operation), operationOutcome(outcome)).
		Observe(seconds)
}

func (m *Metrics) ObserveFailure(family ResourceFamily, stage FailureStage, category FailureCategory) {
	m.failures.WithLabelValues(resourceFamily(family), failureStage(stage), failureCategory(category)).Inc()
}

// OperationForSchema maps the schema operation enum to the shared label.
func OperationForSchema(operation operatorv1alpha1.OperationType) Operation {
	switch operation {
	case operatorv1alpha1.OperationResolve:
		return OperationResolve
	case operatorv1alpha1.OperationVerify:
		return OperationVerify
	case operatorv1alpha1.OperationObserve:
		return OperationObserve
	case operatorv1alpha1.OperationPlan:
		return OperationPlan
	case operatorv1alpha1.OperationApply:
		return OperationApply
	default:
		return ""
	}
}

// OperationForMigration maps the migration operation enum to the shared label.
func OperationForMigration(operation operatorv1alpha1.MigrationOperationType) Operation {
	switch operation {
	case operatorv1alpha1.MigrationOperationResolve:
		return OperationResolve
	case operatorv1alpha1.MigrationOperationVerify:
		return OperationVerify
	case operatorv1alpha1.MigrationOperationHistory:
		return OperationHistory
	case operatorv1alpha1.MigrationOperationApply:
		return OperationApply
	default:
		return ""
	}
}

// StageForMigrationOperation maps the migration operation enum to a closed
// failure label. History is its own stage: a migration reads back what the
// revision table records, which no schema operation does.
func StageForMigrationOperation(operation operatorv1alpha1.MigrationOperationType) FailureStage {
	switch operation {
	case operatorv1alpha1.MigrationOperationResolve:
		return FailureStageResolve
	case operatorv1alpha1.MigrationOperationVerify:
		return FailureStageVerify
	case operatorv1alpha1.MigrationOperationHistory:
		return FailureStageHistory
	case operatorv1alpha1.MigrationOperationApply:
		return FailureStageApply
	default:
		return FailureStageController
	}
}

// StageForOperation maps the public operation enum to a closed failure label.
func StageForOperation(operation operatorv1alpha1.OperationType) FailureStage {
	switch operation {
	case operatorv1alpha1.OperationResolve:
		return FailureStageResolve
	case operatorv1alpha1.OperationVerify:
		return FailureStageVerify
	case operatorv1alpha1.OperationObserve:
		return FailureStageObserve
	case operatorv1alpha1.OperationPlan:
		return FailureStagePlan
	case operatorv1alpha1.OperationApply:
		return FailureStageApply
	default:
		return FailureStageController
	}
}

func resourceFamily(value ResourceFamily) string {
	switch value {
	case FamilySchema, FamilyMigration:
		return string(value)
	default:
		return "unknown"
	}
}

func planImpact(value PlanImpact) string {
	switch value {
	case PlanDestructive, PlanNonDestructive, PlanImpactUnknown:
		return string(value)
	default:
		return "unknown"
	}
}

func reconciliationResult(value ReconciliationResult) string {
	switch value {
	case ReconciliationSucceeded, ReconciliationFailed:
		return string(value)
	default:
		return "unknown"
	}
}

func databaseEngine(value operatorv1alpha1.DatabaseEngine) string {
	switch value {
	case operatorv1alpha1.DatabaseEnginePostgreSQL:
		return "postgresql"
	case operatorv1alpha1.DatabaseEngineMySQL:
		return "mysql"
	default:
		return "unknown"
	}
}

func driftOutcome(value DriftOutcome) string {
	switch value {
	case DriftDetected, DriftInSync:
		return string(value)
	default:
		return "unknown"
	}
}

func approvalOutcome(value ApprovalOutcome) string {
	switch value {
	case ApprovalRequired, ApprovalAccepted, ApprovalStale:
		return string(value)
	default:
		return "unknown"
	}
}

func applyOutcome(value ApplyOutcome) string {
	switch value {
	case ApplyStarted, ApplyCompleted, ApplyUncertain, ApplyStale:
		return string(value)
	default:
		return "unknown"
	}
}

func operationLabel(value Operation) string {
	switch value {
	case OperationResolve, OperationVerify, OperationObserve, OperationPlan, OperationApply, OperationHistory:
		return string(value)
	default:
		return "unknown"
	}
}

func operationOutcome(value OperationOutcome) string {
	switch value {
	case OperationSucceeded, OperationUncertain, OperationStale, OperationCanceled:
		return string(value)
	default:
		return "unknown"
	}
}

func failureStage(value FailureStage) string {
	switch value {
	case FailureStageController, FailureStageResolve, FailureStageVerify,
		FailureStageObserve, FailureStagePlan, FailureStageApply, FailureStageHistory:
		return string(value)
	default:
		return "unknown"
	}
}

func failureCategory(value FailureCategory) string {
	switch value {
	case FailureInfrastructure, FailureConfiguration, FailureOperation, FailurePolicyChanged, FailureStaleInput, FailureUncertain:
		return string(value)
	default:
		return "unknown"
	}
}
