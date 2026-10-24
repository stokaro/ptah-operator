package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LifecycleJournal is the bounded record of a resource's significant lifecycle
// events: a plan published, an approval accepted, an Apply or a run and how it
// ended, the proof that followed, and failed attempts. A read-only cycle that changes nothing writes nothing
// here. The manager derives every entry from the status write that records the
// transition, so the journal costs no request of its own.
type LifecycleJournal struct {
	// CoveredSince is the instant from which the journal is complete: when a
	// manager that keeps a journal first wrote this resource's status, moved
	// forward past the newest entry a bound has since dropped. A reader states
	// the period the journal covers from it and assumes nothing before.
	CoveredSince metav1.Time `json:"coveredSince"`

	// Entries are the events, oldest first. There are at most 64, and none is
	// more than 30 days older than the newest.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Entries []JournalEntry `json:"entries,omitempty"`
}

// JournalEntryType is the kind of event a journal entry records.
// +kubebuilder:validation:Enum=PlanPublished;ApprovalAccepted;ApplyFinished;Verified;RunFinished;RunAcknowledged;OperationFailed
type JournalEntryType string

const (
	// JournalPlanPublished is a new plan in status.plan.
	JournalPlanPublished JournalEntryType = "PlanPublished"
	// JournalApprovalAccepted is the approval an Apply consumed: the one a
	// PtahSchema's plan recorded, or the one a PtahMigration's Apply claim
	// names. A PtahMigration's status does not carry the approver.
	JournalApprovalAccepted JournalEntryType = "ApprovalAccepted"
	// JournalApplyFinished is a PtahSchema Apply that ended.
	JournalApplyFinished JournalEntryType = "ApplyFinished"
	// JournalVerified is the post-Apply observation of a PtahSchema settling.
	JournalVerified JournalEntryType = "Verified"
	// JournalRunFinished is a PtahMigration run that ended, or one recorded as
	// unresolved because nobody established its outcome.
	JournalRunFinished JournalEntryType = "RunFinished"
	// JournalRunAcknowledged is an unresolved run a person acknowledged.
	JournalRunAcknowledged JournalEntryType = "RunAcknowledged"
	// JournalOperationFailed is a failed attempt. Attempts that fail with the
	// same operation and reason in a row are one entry with a count.
	JournalOperationFailed JournalEntryType = "OperationFailed"
)

// JournalOutcome is how an Apply, a proof or a run ended.
// +kubebuilder:validation:Enum=Succeeded;Failed;Partial;Unknown;UpToDate;InSync;DriftRemains;Superseded
type JournalOutcome string

const (
	// JournalSucceeded is an Apply that completed, or a run that applied what
	// it selected.
	JournalSucceeded JournalOutcome = "Succeeded"
	// JournalFailed is an Apply or a run that failed and committed nothing.
	JournalFailed JournalOutcome = "Failed"
	// JournalPartial is a run that committed part of a migration.
	JournalPartial JournalOutcome = "Partial"
	// JournalUnknown is an Apply or a run whose effect on the database nobody
	// established.
	JournalUnknown JournalOutcome = "Unknown"
	// JournalUpToDate is a run that found nothing to apply.
	JournalUpToDate JournalOutcome = "UpToDate"
	// JournalInSync is a proof that found the database matching the plan.
	JournalInSync JournalOutcome = "InSync"
	// JournalDriftRemains is a proof that still found changes to make.
	JournalDriftRemains JournalOutcome = "DriftRemains"
	// JournalSuperseded is a proof that settled under a newer generation or
	// policy, which decides what follows.
	JournalSuperseded JournalOutcome = "Superseded"
)

// JournalEntry is one event. Which fields it carries depends on its type, and
// every name it carries is an object in the resource's namespace.
type JournalEntry struct {
	// Type is the kind of event.
	Type JournalEntryType `json:"type"`

	// At is when the event happened. For an Apply, a run or a failed attempt
	// it is when that ended.
	At metav1.Time `json:"at"`

	// StartedAt is when an Apply or a run began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// LastAt is the latest attempt an OperationFailed entry folds. A single
	// failure has none.
	// +optional
	LastAt *metav1.Time `json:"lastAt,omitempty"`
	// Count is how many attempts an OperationFailed entry stands for. A single
	// failure has none.
	// +kubebuilder:validation:Minimum=2
	// +optional
	Count int32 `json:"count,omitempty"`

	// Plan names the plan the event concerns.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Plan string `json:"plan,omitempty"`

	// Statements is how many statements a published PtahSchema plan holds.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Statements int32 `json:"statements,omitempty"`
	// Destructive reports a published PtahSchema plan that destroys data.
	// +optional
	Destructive bool `json:"destructive,omitempty"`

	// Approval names the approval an Apply consumed.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Approval string `json:"approval,omitempty"`
	// Approver is the user admission stamped on that approval.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Approver string `json:"approver,omitempty"`

	// Outcome is how an Apply, a proof or a run ended.
	// +optional
	Outcome JournalOutcome `json:"outcome,omitempty"`

	// Operation is the operation that failed, when one was claimed.
	// +kubebuilder:validation:Enum=Resolve;Verify;Observe;Plan;History;Apply
	// +optional
	Operation string `json:"operation,omitempty"`
	// Reason is the condition reason the failure set. It is a code, never the
	// message: a message can carry what status must not.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	// +optional
	Reason string `json:"reason,omitempty"`

	// AppliedVersions is how many migrations a run recorded applied.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AppliedVersions int32 `json:"appliedVersions,omitempty"`
	// Failure is where a run stopped, as status.lastRun.failure said.
	// +optional
	Failure *MigrationRunFailureStatus `json:"failure,omitempty"`

	// Acknowledgment names the acknowledgment that settled an unresolved run.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Acknowledgment string `json:"acknowledgment,omitempty"`
	// AcknowledgedBy is the user admission stamped on that acknowledgment.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	AcknowledgedBy string `json:"acknowledgedBy,omitempty"`
}
