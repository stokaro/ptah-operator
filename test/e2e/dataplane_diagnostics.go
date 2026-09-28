package e2e

import (
	"cmp"
	"fmt"
	"io"
	"regexp"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The diagnostics below are what a failed phase prints about the schemas it
// left. A failure may be a credential that escaped into a status message, an
// Event or a log, so nothing here carries free text: every string is a name,
// a UID, a timestamp, or a value held to a closed shape, and anything else is
// null. The projection is then scanned for the protected patterns before it
// is printed, and withheld on a match.

var (
	failureCodePrefix = regexp.MustCompile(`^([a-z][a-z0-9_]{0,63}):`)
	failureCodeShape  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
	holderHashShape   = regexp.MustCompile(`^ptah-h-[a-z2-7]{52}$`)
	knownPhases       = []ptahv1alpha1.ReconciliationPhase{
		ptahv1alpha1.PhasePending, ptahv1alpha1.PhaseResolving, ptahv1alpha1.PhaseVerifying,
		ptahv1alpha1.PhaseObserving, ptahv1alpha1.PhasePlanning, ptahv1alpha1.PhaseReadyToApply,
		ptahv1alpha1.PhaseAwaitingApproval, ptahv1alpha1.PhaseApplying, ptahv1alpha1.PhaseVerifyingConvergence,
		ptahv1alpha1.PhaseInSync, ptahv1alpha1.PhaseBlocked, ptahv1alpha1.PhaseSuspended, ptahv1alpha1.PhaseFailed,
	}
)

// safeCode is the machine code a failure message opens with, as in
// "invalid_target: ...", or nil when the message opens with anything else.
func safeCode(message string) *string {
	match := failureCodePrefix.FindStringSubmatch(message)
	if match == nil || !failureCodeShape.MatchString(match[1]) {
		return nil
	}
	return &match[1]
}

func safeEpoch(value string) *string {
	if !executionEpoch.MatchString(value) {
		return nil
	}
	return &value
}

func safeDigest(value string) *string {
	if !sha256Pattern.MatchString(value) {
		return nil
	}
	return &value
}

func safePhase(phase ptahv1alpha1.ReconciliationPhase) *string {
	if !slices.Contains(knownPhases, phase) {
		return nil
	}
	value := string(phase)
	return &value
}

func safeOperation(operation string) *string {
	if !slices.Contains(schemaOperations, operation) {
		return nil
	}
	return &operation
}

type cleanupProjection struct {
	Schemas []schemaDiagnostic `json:"schemas"`
	Leases  []leaseDiagnostic  `json:"leases"`
}

type schemaDiagnostic struct {
	Name                   string             `json:"name"`
	UID                    types.UID          `json:"uid"`
	Generation             int64              `json:"generation"`
	ObservedGeneration     int64              `json:"observedGeneration"`
	Phase                  *string            `json:"phase"`
	NextReconciliationTime *metav1.Time       `json:"nextReconciliationTime"`
	Failure                *failureDiagnostic `json:"failure"`
	ExpectedEpochs         expectedEpochs     `json:"expectedEpochs"`
	LeaseContinuityLost    bool               `json:"leaseContinuityLost"`
	Events                 []eventDiagnostic  `json:"events"`
	Jobs                   []jobDiagnostic    `json:"jobs"`
}

type failureDiagnostic struct {
	Condition string  `json:"condition"`
	Reason    string  `json:"reason"`
	Code      *string `json:"code"`
}

type expectedEpochs struct {
	ActiveOperation    *string `json:"activeOperation"`
	PendingObservation *string `json:"pendingObservation"`
	PendingLockRelease *string `json:"pendingLockRelease"`
}

type eventDiagnostic struct {
	Type      *string `json:"type"`
	Reason    string  `json:"reason"`
	Code      *string `json:"code"`
	Timestamp *string `json:"timestamp"`
}

type jobDiagnostic struct {
	Name                 string       `json:"name"`
	UID                  types.UID    `json:"uid"`
	Operation            *string      `json:"operation"`
	Created              metav1.Time  `json:"created"`
	Started              *metav1.Time `json:"started"`
	Completed            *metav1.Time `json:"completed"`
	Complete             bool         `json:"complete"`
	Failed               bool         `json:"failed"`
	OperationIDHashShape bool         `json:"operationIDHashShape"`
	InputFingerprint     *string      `json:"inputFingerprint"`
}

type leaseDiagnostic struct {
	Name                 string            `json:"name"`
	UID                  types.UID         `json:"uid"`
	ResourceVersion      string            `json:"resourceVersion"`
	Created              metav1.Time       `json:"created"`
	Epoch                *string           `json:"epoch"`
	HolderPresent        bool              `json:"holderPresent"`
	HolderHashShape      bool              `json:"holderHashShape"`
	LeaseDurationSeconds *int32            `json:"leaseDurationSeconds"`
	AcquireTime          *metav1.MicroTime `json:"acquireTime"`
	RenewTime            *metav1.MicroTime `json:"renewTime"`
	Transitions          *int32            `json:"transitions"`
}

// failureReasons are the ReconciliationFailed reasons whose message opens with
// a machine code.
var failureReasons = []string{"OperationFailed", "ConfigurationError", "ApplyOutcomeUnknown"}

// diagnosedEventReasons are the Events a failure diagnosis reads, and the
// first three of them open their message with a machine code.
var diagnosedEventReasons = []string{
	"OperationFailed", "ReconciliationFailed", "ApplyOutcomeUnknown", "PlanStale", "LeaseContinuityLost",
}

// projectCleanupDiagnostic is what a failed phase prints about the schemas it
// left: each schema's phase, the failure it reports and its code, the lease
// epochs it expects, the Events that explain it and the Jobs it owns by exact
// UID, and the coordination Leases the operator holds.
func projectCleanupDiagnostic(schemas []ptahv1alpha1.PtahSchema, events []corev1.Event, jobs []batchv1.Job,
	leases []coordinationv1.Lease,
) cleanupProjection {
	projection := cleanupProjection{Schemas: []schemaDiagnostic{}, Leases: []leaseDiagnostic{}}
	for index := range schemas {
		schema := &schemas[index]
		if schema.UID == "" {
			continue
		}
		status := schema.Status
		diagnostic := schemaDiagnostic{
			Name: schema.Name, UID: schema.UID, Generation: schema.Generation,
			ObservedGeneration: status.ObservedGeneration, Phase: safePhase(status.Phase),
			NextReconciliationTime: status.NextReconciliationTime,
			Events:                 []eventDiagnostic{}, Jobs: []jobDiagnostic{},
		}
		for _, condition := range status.Conditions {
			if condition.Type == "ReconciliationFailed" && condition.Status == metav1.ConditionTrue &&
				slices.Contains(failureReasons, condition.Reason) {
				diagnostic.Failure = &failureDiagnostic{
					Condition: "ReconciliationFailed", Reason: condition.Reason, Code: safeCode(condition.Message),
				}
			}
		}
		if status.ActiveOperation != nil {
			diagnostic.ExpectedEpochs.ActiveOperation = safeEpoch(status.ActiveOperation.LeaseEpoch)
			diagnostic.LeaseContinuityLost = status.ActiveOperation.LeaseContinuityLost
		}
		if status.PendingObservation != nil {
			diagnostic.ExpectedEpochs.PendingObservation = safeEpoch(status.PendingObservation.LeaseEpoch)
		}
		if status.PendingLockRelease != nil {
			diagnostic.ExpectedEpochs.PendingLockRelease = safeEpoch(status.PendingLockRelease.LeaseEpoch)
		}
		for eventIndex := range events {
			event := &events[eventIndex]
			involved := event.InvolvedObject
			if involved.APIVersion != ptahSchemaAPIVersion || involved.Kind != "PtahSchema" ||
				involved.Name != schema.Name || involved.UID != schema.UID ||
				!slices.Contains(diagnosedEventReasons, event.Reason) {
				continue
			}
			diagnosed := eventDiagnostic{Reason: event.Reason, Timestamp: eventTimestamp(event)}
			if event.Type == corev1.EventTypeNormal || event.Type == corev1.EventTypeWarning {
				kind := event.Type
				diagnosed.Type = &kind
			}
			if slices.Contains(diagnosedEventReasons[:3], event.Reason) {
				diagnosed.Code = safeCode(event.Message)
			}
			diagnostic.Events = append(diagnostic.Events, diagnosed)
		}
		for jobIndex := range jobs {
			job := &jobs[jobIndex]
			if !ownedBySchemaUID(job.OwnerReferences, schema.UID) {
				continue
			}
			diagnostic.Jobs = append(diagnostic.Jobs, jobDiagnostic{
				Name: job.Name, UID: job.UID, Operation: safeOperation(job.Labels[labelOperation]),
				Created: job.CreationTimestamp, Started: job.Status.StartTime, Completed: job.Status.CompletionTime,
				Complete:             conditionTrue(job.Status.Conditions, batchv1.JobComplete),
				Failed:               conditionTrue(job.Status.Conditions, batchv1.JobFailed),
				OperationIDHashShape: sha256Pattern.MatchString(job.Annotations[annotationOperationID]),
				InputFingerprint:     safeDigest(job.Annotations["operator.ptah.run/input-fingerprint"]),
			})
		}
		projection.Schemas = append(projection.Schemas, diagnostic)
	}
	for index := range leases {
		lease := &leases[index]
		if lease.Labels[labelManagedBy] != managedByOperator ||
			lease.Labels["operator.ptah.run/coordination"] != "database-target" {
			continue
		}
		holder := ""
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}
		projection.Leases = append(projection.Leases, leaseDiagnostic{
			Name: lease.Name, UID: lease.UID, ResourceVersion: lease.ResourceVersion, Created: lease.CreationTimestamp,
			Epoch:                safeEpoch(lease.Annotations["operator.ptah.run/lease-epoch"]),
			HolderPresent:        holder != "",
			HolderHashShape:      holderHashShape.MatchString(holder),
			LeaseDurationSeconds: lease.Spec.LeaseDurationSeconds,
			AcquireTime:          lease.Spec.AcquireTime,
			RenewTime:            lease.Spec.RenewTime,
			Transitions:          lease.Spec.LeaseTransitions,
		})
	}
	return projection
}

func ownedBySchemaUID(references []metav1.OwnerReference, uid types.UID) bool {
	for _, reference := range references {
		if reference.APIVersion == ptahSchemaAPIVersion && reference.Kind == "PtahSchema" &&
			reference.UID == uid && isController(reference) {
			return true
		}
	}
	return false
}

// eventTimestamp is the Event's time as the API renders it: its event time,
// or the last time it was seen, or the first.
func eventTimestamp(event *corev1.Event) *string {
	var rendered []byte
	switch {
	case !event.EventTime.IsZero():
		rendered, _ = event.EventTime.MarshalJSON()
	case !event.LastTimestamp.IsZero():
		rendered, _ = event.LastTimestamp.MarshalJSON()
	case !event.FirstTimestamp.IsZero():
		rendered, _ = event.FirstTimestamp.MarshalJSON()
	default:
		return nil
	}
	value := string(rendered[1 : len(rendered)-1])
	return &value
}

// schemaWaitReport is what a wait that gave up says about the last document
// it read.
type schemaWaitDiagnostic struct {
	Name                   string                           `json:"name"`
	Generation             int64                            `json:"generation"`
	ObservedGeneration     int64                            `json:"observedGeneration"`
	Phase                  ptahv1alpha1.ReconciliationPhase `json:"phase"`
	NextReconciliationTime *metav1.Time                     `json:"nextReconciliationTime"`
	ActiveOperation        *activeOperationDiagnostic       `json:"activeOperation"`
	Plan                   *planDiagnostic                  `json:"plan"`
	Conditions             []conditionDiagnostic            `json:"conditions"`
}

type activeOperationDiagnostic struct {
	Type    ptahv1alpha1.OperationType `json:"type"`
	JobName string                     `json:"jobName"`
}

type planDiagnostic struct {
	Name        string `json:"name"`
	Destructive bool   `json:"destructive"`
}

type conditionDiagnostic struct {
	Type    string                 `json:"type"`
	Status  metav1.ConditionStatus `json:"status"`
	Reason  string                 `json:"reason"`
	Message string                 `json:"message"`
}

// conditionMessageBound is how much of a condition message a wait report
// prints.
const conditionMessageBound = 240

func schemaWaitReport(schema *ptahv1alpha1.PtahSchema) schemaWaitDiagnostic {
	status := schema.Status
	report := schemaWaitDiagnostic{
		Name: schema.Name, Generation: schema.Generation, ObservedGeneration: status.ObservedGeneration,
		Phase: status.Phase, NextReconciliationTime: status.NextReconciliationTime,
		Conditions: []conditionDiagnostic{},
	}
	if status.ActiveOperation != nil {
		report.ActiveOperation = &activeOperationDiagnostic{Type: status.ActiveOperation.Type, JobName: status.ActiveOperation.JobName}
	}
	if status.Plan != nil {
		report.Plan = &planDiagnostic{Name: status.Plan.Name, Destructive: status.Plan.Destructive}
	}
	for _, condition := range status.Conditions {
		message := []rune(condition.Message)
		if len(message) > conditionMessageBound {
			message = message[:conditionMessageBound]
		}
		report.Conditions = append(report.Conditions, conditionDiagnostic{
			Type: condition.Type, Status: condition.Status, Reason: condition.Reason, Message: string(message),
		})
	}
	return report
}

// observedJobTimeline is the order a schema's Jobs appeared in after a
// checkpoint: what a failed blocked-refresh proof prints to separate a chain
// that started early from one that never ran.
type observedJobTimeline struct {
	Kind string        `json:"kind"`
	Jobs []timelineJob `json:"jobs"`
}

type timelineJob struct {
	Name      string  `json:"name"`
	UID       string  `json:"uid"`
	Operation *string `json:"operation"`
	Created   string  `json:"created"`
}

// operationRank is where an operation falls in one read-only chain.
func operationRank(operation string) int {
	switch operation {
	case "resolve":
		return 0
	case "verify":
		return 1
	case "observe":
		return 2
	case "plan":
		return 3
	default:
		return 4
	}
}

func timelineSince(records []observedJob) observedJobTimeline {
	timeline := observedJobTimeline{Kind: "ObservedJobTimelineDiagnostic", Jobs: []timelineJob{}}
	for _, record := range records {
		timeline.Jobs = append(timeline.Jobs, timelineJob{
			Name: record.Name, UID: record.UID, Operation: safeOperation(record.Operation), Created: record.Created,
		})
	}
	slices.SortStableFunc(timeline.Jobs, func(a, b timelineJob) int {
		return cmp.Or(cmp.Compare(a.Created, b.Created),
			cmp.Compare(operationRank(stringOr(a.Operation)), operationRank(stringOr(b.Operation))),
			cmp.Compare(a.UID, b.UID))
	})
	return timeline
}

func stringOr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// emitScanned writes a diagnostic projection on one line after its header, or
// the one fixed suppression line when the scanner has nothing to scan with,
// the projection does not encode, or it carries a protected pattern.
func emitScanned(w io.Writer, scanner credentialScanner, projection any) {
	encoded, err := canonicalJSON(projection)
	if err != nil || !scanner.ready() || len(encoded) == 0 || scanner.leaks(encoded) {
		_, _ = fmt.Fprintln(w, cleanupSuppressed)
		return
	}
	_, _ = fmt.Fprintf(w, "e2e data plane: credential-safe reconciliation diagnostic projection\n%s\n", encoded)
}

const cleanupSuppressed = "e2e data plane: credential-safe reconciliation diagnostics suppressed"
