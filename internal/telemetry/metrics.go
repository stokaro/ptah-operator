// Package telemetry defines bounded-cardinality Prometheus telemetry for the
// schema reconciliation state machine.
package telemetry

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

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
	ObserveReconciliation(ReconciliationResult)
	ObserveDrift(operatorv1alpha1.DatabaseEngine, DriftOutcome)
	ObservePlan(operatorv1alpha1.DatabaseEngine, bool)
	ObserveApproval(ApprovalOutcome)
	ObserveApply(ApplyOutcome)
	ObserveOperation(operatorv1alpha1.OperationType, OperationOutcome, time.Duration)
	ObserveFailure(FailureStage, FailureCategory)
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
			Help:      "Total PtahSchema reconciliations by bounded outcome.",
		}, []string{"result"}),
		driftObservations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "drift_observations_total",
			Help:      "Total successful database drift observations.",
		}, []string{"engine", "outcome"}),
		plans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "plans_total",
			Help:      "Total immutable plans published by the controller.",
		}, []string{"engine", "destructive"}),
		approvals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "approvals_total",
			Help:      "Total approval lifecycle transitions.",
		}, []string{"outcome"}),
		applies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "applies_total",
			Help:      "Total apply lifecycle transitions.",
		}, []string{"outcome"}),
		operationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ptah_operator",
			Name:      "operation_duration_seconds",
			Help:      "Duration of completed logical schema operations.",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 16),
		}, []string{"operation", "outcome"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "failures_total",
			Help:      "Total failures by bounded state-machine stage and category.",
		}, []string{"stage", "category"}),
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

func (m *Metrics) ObserveReconciliation(result ReconciliationResult) {
	m.reconciliations.WithLabelValues(reconciliationResult(result)).Inc()
}

func (m *Metrics) ObserveDrift(engine operatorv1alpha1.DatabaseEngine, outcome DriftOutcome) {
	m.driftObservations.WithLabelValues(databaseEngine(engine), driftOutcome(outcome)).Inc()
}

func (m *Metrics) ObservePlan(engine operatorv1alpha1.DatabaseEngine, destructive bool) {
	m.plans.WithLabelValues(databaseEngine(engine), strconv.FormatBool(destructive)).Inc()
}

func (m *Metrics) ObserveApproval(outcome ApprovalOutcome) {
	m.approvals.WithLabelValues(approvalOutcome(outcome)).Inc()
}

func (m *Metrics) ObserveApply(outcome ApplyOutcome) {
	m.applies.WithLabelValues(applyOutcome(outcome)).Inc()
}

func (m *Metrics) ObserveOperation(operation operatorv1alpha1.OperationType, outcome OperationOutcome, duration time.Duration) {
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.operationDuration.WithLabelValues(operationType(operation), operationOutcome(outcome)).Observe(seconds)
}

func (m *Metrics) ObserveFailure(stage FailureStage, category FailureCategory) {
	m.failures.WithLabelValues(failureStage(stage), failureCategory(category)).Inc()
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

func operationType(value operatorv1alpha1.OperationType) string {
	switch value {
	case operatorv1alpha1.OperationResolve:
		return "resolve"
	case operatorv1alpha1.OperationVerify:
		return "verify"
	case operatorv1alpha1.OperationObserve:
		return "observe"
	case operatorv1alpha1.OperationPlan:
		return "plan"
	case operatorv1alpha1.OperationApply:
		return "apply"
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
	case FailureStageController, FailureStageResolve, FailureStageVerify, FailureStageObserve, FailureStagePlan, FailureStageApply:
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
