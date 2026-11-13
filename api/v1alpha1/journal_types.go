package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// LifecycleJournal is the bounded record of a resource's significant lifecycle
// events: a plan published, an approval accepted, an Apply or a run and how it
// ended, the proof that followed, and failed attempts. A read-only cycle that changes nothing writes nothing
// here. The manager derives every entry from the status write that records the
// transition, so the journal costs no request of its own.
type LifecycleJournal struct {
	// CoveredSince is the instant from which the journal is complete: when a
	// manager that keeps a journal first wrote this resource's status, moved
	// forward past the newest entry a bound has since dropped. A reader states
	// the period the journal covers from it and assumes nothing before, though
	// the entries of a plan nothing has carried out yet can be older.
	CoveredSince metav1.Time `json:"coveredSince"`

	// Entries are the events, oldest first. There are at most 64, and none is
	// more than 30 days older than the newest, apart from the plan nothing has
	// carried out yet: the entry that published it and the newest approval of it
	// stay until something carries it out or another plan is published.
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
	// names. One entry stands for one decision, told apart by the approval's UID:
	// a later decision may take the same name.
	JournalApprovalAccepted JournalEntryType = "ApprovalAccepted"
	// JournalApplyFinished is a PtahSchema Apply that ended: one entry per
	// execution.
	JournalApplyFinished JournalEntryType = "ApplyFinished"
	// JournalVerified is the post-Apply observation of a PtahSchema settling, or
	// the first reading of a PtahMigration's history after a run.
	JournalVerified JournalEntryType = "Verified"
	// JournalRunFinished is a PtahMigration run that ended: one entry per
	// execution. A run that ends Partial or Unknown also holds the resource in
	// status.unresolvedRun, and that hold writes no entry of its own. A hold
	// restored without the status that held its run is that run's entry, with
	// the outcome the hold recorded.
	JournalRunFinished JournalEntryType = "RunFinished"
	// JournalRunAcknowledged is an unresolved run a person acknowledged.
	JournalRunAcknowledged JournalEntryType = "RunAcknowledged"
	// JournalOperationFailed is a failed attempt. Attempts that fail with the
	// same operation and reason in a row are one entry with a count.
	JournalOperationFailed JournalEntryType = "OperationFailed"
)

// JournalOutcome is how an Apply, a proof or a run ended.
// +kubebuilder:validation:Enum=Succeeded;Failed;Partial;Unknown;UpToDate;InSync;DriftRemains;Superseded;Pending;Blocked
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
	// JournalPending is a reading after a migration run that found the artifact
	// has more to apply.
	JournalPending JournalOutcome = "Pending"
	// JournalBlocked is a reading after a migration run of a history nothing may
	// run against.
	JournalBlocked JournalOutcome = "Blocked"
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

	// OperationID is the Apply claim an entry concerns: a PtahSchema's
	// ApplyFinished and the Verified entry of the proof after it, or a
	// PtahMigration's RunFinished, Verified or RunAcknowledged. It is what joins
	// them: entries of one execution carry the same one, and neither their order
	// nor their times are the join. ApprovalUID is what joins an
	// ApprovalAccepted entry to the Apply or the run that consumed it, since a
	// claim withdrawn before dispatch leaves the approval to the next one. An
	// entry a release before this field wrote has none.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +optional
	OperationID string `json:"operationID,omitempty"`

	// Plan names the plan the event concerns.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Plan string `json:"plan,omitempty"`
	// PlanUID is the UID of the plan an Apply or a run carried out, or of the
	// plan a PtahMigration's approval was accepted for.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	// +optional
	PlanUID types.UID `json:"planUID,omitempty"`
	// PlanFingerprint is the spec.fingerprint of the plan an Apply or a run
	// carried out, when the claim recorded it.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	PlanFingerprint string `json:"planFingerprint,omitempty"`

	// Statements is how many statements a published PtahSchema plan holds.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Statements int32 `json:"statements,omitempty"`
	// Destructive reports a published PtahSchema plan that destroys data.
	// +optional
	Destructive bool `json:"destructive,omitempty"`
	// ApprovalRequired reports a published PtahSchema plan that asked for a
	// person's approval when it was published.
	// +optional
	ApprovalRequired bool `json:"approvalRequired,omitempty"`
	// Replaced names the plan a published PtahSchema plan took the place of: one
	// a person could approve, or had approved, and that never ran. A PlanStale
	// Warning reports the same replacement, and an Event can be lost.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Replaced string `json:"replaced,omitempty"`

	// Authorization is what allowed a PtahMigration's run.
	// +optional
	Authorization MigrationRunAuthorizationKind `json:"authorization,omitempty"`
	// Approval names the approval an Apply consumed. A name can be used again by
	// a later decision, so it does not say which object it was: ApprovalUID does.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Approval string `json:"approval,omitempty"`
	// ApprovalUID is the UID of that approval. An entry a release before this
	// field wrote has none: it is incomplete evidence, and is not to be joined
	// to an object that holds the name today.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	// +optional
	ApprovalUID types.UID `json:"approvalUID,omitempty"`
	// Approver is the user admission stamped on that approval, cut to the first
	// 512 characters where the name is longer.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Approver string `json:"approver,omitempty"`

	// Outcome is how an Apply, a proof or a run ended. On a PtahMigration's
	// Verified entry it is what the reading after the run found, and on a
	// RunAcknowledged entry how the run that person settled ended.
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

	// JobName is the Job an Apply or a run ran as. It is absent when no Job
	// was confirmed.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	JobName string `json:"jobName,omitempty"`
	// JobUID is that Job's UID, which outlives the Job and tells it from a later
	// Job that takes the name.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	// +optional
	JobUID types.UID `json:"jobUID,omitempty"`

	// AppliedVersions is how many migrations a run recorded applied.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AppliedVersions int32 `json:"appliedVersions,omitempty"`
	// Failure is where a PtahMigration's run stopped, as
	// status.lastRun.failure said.
	// +optional
	Failure *MigrationRunFailureStatus `json:"failure,omitempty"`
	// ApplyFailure is where a PtahSchema's Apply stopped, as
	// status.pendingObservation.applyFailure said. An Apply that ends Unknown
	// carries it when the run named the statement the database refused.
	// +optional
	ApplyFailure *SchemaApplyFailureStatus `json:"applyFailure,omitempty"`

	// Acknowledgment names the acknowledgment that settled an unresolved run.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Acknowledgment string `json:"acknowledgment,omitempty"`
	// AcknowledgedBy is the user admission stamped on that acknowledgment, cut to
	// the first 512 characters where the name is longer.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	AcknowledgedBy string `json:"acknowledgedBy,omitempty"`
}
