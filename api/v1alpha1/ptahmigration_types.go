package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// MigrationPhase is the coarse state of one PtahMigration, for a person
// reading `kubectl get`. Conditions carry the machine-readable account.
// +kubebuilder:validation:Enum=Pending;Resolving;Verifying;Reading;Planning;AwaitingApproval;Blocked;Applying;VerifyingHistory;InSync;Suspended;Failed
type MigrationPhase string

const (
	// MigrationPhasePending is a migration the controller has not yet acted on.
	MigrationPhasePending MigrationPhase = "Pending"
	// MigrationPhaseResolving is resolving the artifact reference to a digest.
	MigrationPhaseResolving MigrationPhase = "Resolving"
	// MigrationPhaseVerifying is verifying the artifact against its policy.
	MigrationPhaseVerifying MigrationPhase = "Verifying"
	// MigrationPhaseReading is reading the database's own migration history.
	MigrationPhaseReading MigrationPhase = "Reading"
	// MigrationPhasePlanning is selecting the pending sequence.
	MigrationPhasePlanning MigrationPhase = "Planning"
	// MigrationPhaseAwaitingApproval is a published plan waiting for the exact
	// approval its policy requires.
	MigrationPhaseAwaitingApproval MigrationPhase = "AwaitingApproval"
	// MigrationPhaseBlocked is a history this artifact cannot continue: a
	// modified applied migration, an incompatible branch, or a dirty row. It is
	// never resolved by the controller.
	MigrationPhaseBlocked MigrationPhase = "Blocked"
	// MigrationPhaseApplying is an execution Job in flight.
	MigrationPhaseApplying MigrationPhase = "Applying"
	// MigrationPhaseVerifyingHistory is reading the history back to confirm
	// what the run did.
	MigrationPhaseVerifyingHistory MigrationPhase = "VerifyingHistory"
	// MigrationPhaseInSync is a history that matches the artifact with nothing
	// pending.
	MigrationPhaseInSync MigrationPhase = "InSync"
	// MigrationPhaseSuspended is a resource whose spec asked for no new work.
	MigrationPhaseSuspended MigrationPhase = "Suspended"
	// MigrationPhaseFailed is a run that failed and may be retried once its
	// cause is fixed.
	MigrationPhaseFailed MigrationPhase = "Failed"
)

// MigrationRunOutcome is what a finished execution's own evidence said, read
// from the database rather than from the Job's exit status.
//
// The values are Ptah's, because the controller reports the verdict of the
// engine that was present for the run rather than deriving a second one.
// +kubebuilder:validation:Enum=UpToDate;Applied;Failed;Partial;Unknown
type MigrationRunOutcome string

const (
	// MigrationRunOutcomeUpToDate means the run selected nothing.
	MigrationRunOutcomeUpToDate MigrationRunOutcome = "UpToDate"
	// MigrationRunOutcomeApplied means every selected migration is recorded
	// applied and no revision row is dirty.
	MigrationRunOutcomeApplied MigrationRunOutcome = "Applied"
	// MigrationRunOutcomeFailed means the run stopped and the migration that
	// failed committed nothing. It may be retried once its cause is fixed.
	MigrationRunOutcomeFailed MigrationRunOutcome = "Failed"
	// MigrationRunOutcomePartial means the migration that failed committed some
	// of its statements and not the rest. Retrying the file would run them
	// twice, so recovery is Ptah's own resume path.
	MigrationRunOutcomePartial MigrationRunOutcome = "Partial"
	// MigrationRunOutcomeUnknown means the evidence could not be read, or a
	// dirty row does not say how far it got. Nothing may conclude from this
	// that the migration did or did not run.
	MigrationRunOutcomeUnknown MigrationRunOutcome = "Unknown"
)

// MigrationOperationType is one step of a migration lifecycle. Each one is a
// Job, and each Job belongs to exactly one claim.
// +kubebuilder:validation:Enum=Resolve;Verify;History;Apply
type MigrationOperationType string

const (
	// MigrationOperationResolve turns the artifact reference into a digest.
	MigrationOperationResolve MigrationOperationType = "Resolve"
	// MigrationOperationVerify checks the resolved artifact against its policy.
	MigrationOperationVerify MigrationOperationType = "Verify"
	// MigrationOperationHistory reads the database's own revision table against
	// the artifact and changes nothing.
	MigrationOperationHistory MigrationOperationType = "History"
	// MigrationOperationApply runs the planned sequence.
	MigrationOperationApply MigrationOperationType = "Apply"
)

// MigrationOperationStatus is one durable claim: the work the controller
// decided on, before the Job that carries it exists.
//
// It is durable because the Job is not the record. A claim written first, with
// the Job's deterministic name in it, is what lets a controller that restarts
// mid-dispatch tell the Job it created from one it has not created yet -- and
// what lets admission refuse a Job that no claim asked for.
type MigrationOperationStatus struct {
	// Type is the operation this claim authorizes.
	Type MigrationOperationType `json:"type"`

	// ID is this attempt's identity, distinct from every other attempt of the
	// same operation.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ID string `json:"id"`

	// InputFingerprint is what the operation was decided from. An input that
	// changed while the Job ran is what makes its result stale rather than
	// wrong.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	InputFingerprint string `json:"inputFingerprint"`

	// JobName is the deterministic name this claim's Job takes. It is written
	// before the Job is created.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	JobName string `json:"jobName"`

	// JobUID is the exact Job the claim is bound to, once one exists. A Job
	// with the right name and another UID is a different Job.
	JobUID types.UID `json:"jobUID,omitempty"`

	// StartedAt is when the claim was written, which is before the Job exists.
	StartedAt metav1.Time `json:"startedAt"`

	// Attempt counts this claim among the retries of the same operation.
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt"`

	// ExecutionBindingID is the epoch this claim was authorized under. A
	// rollout that changes any execution component retires the claim rather
	// than letting its Job finish under new bytes.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID,omitempty"`

	// Source is the credential-free artifact binding this operation uses:
	// the resolved digest and the selectors needed to fetch it. Every operation
	// after Resolve carries one, so a newer generation cannot send newly
	// selected credentials to the old artifact's registry.
	Source *OCIArtifactAccessBinding `json:"source,omitempty"`

	// Target is the key-free database binding, and CoordinationDigest the realm
	// the operation serializes against.
	Target *DatabaseTargetBinding `json:"target,omitempty"`
	// CoordinationDigest is that realm, hashed.
	CoordinationDigest string `json:"coordinationDigest,omitempty"`

	// PlanRef is the immutable plan an Apply carries out.
	PlanRef *ImmutableObjectReference `json:"planRef,omitempty"`

	// DispatchStarted records that the one permitted Job create attempt was
	// made. An Apply that crossed this boundary is never recreated, because
	// whether it ran is a question for the database rather than for a retry.
	DispatchStarted bool `json:"dispatchStarted,omitempty"`

	// DispatchNotAfter and ExecutionNotAfter bound the claim in time.
	DispatchNotAfter *metav1.Time `json:"dispatchNotAfter,omitempty"`
	// ExecutionNotAfter is when the authorized run itself expires.
	ExecutionNotAfter *metav1.Time `json:"executionNotAfter,omitempty"`

	// LeaseEpoch is the database lock acquisition this claim was authorized
	// under, and LeaseDurationSeconds how long that acquisition was taken for.
	// A result produced across an epoch change is discarded rather than read:
	// the lock it held was somebody else's by then.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	LeaseEpoch string `json:"leaseEpoch,omitempty"`
	// LeaseDurationSeconds is how long that acquisition was taken for.
	// +kubebuilder:validation:Minimum=1
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds,omitempty"`
	// LeaseContinuityLost records that the epoch changed under this claim.
	LeaseContinuityLost bool `json:"leaseContinuityLost,omitempty"`

	// ApprovalRef is the approval that authorized this Apply, recorded before
	// dispatch so the run is attributable to the decision that permitted it.
	ApprovalRef *ImmutableObjectReference `json:"approvalRef,omitempty"`

	// AdmissionSnapshot is the Pod envelope resolved before dispatch and bound
	// into the Job and its Pod template. It is what lets Pod admission permit
	// the built-in mutations that are modeled and safe while refusing any other
	// change to what the Pod executes. A migration Pod is judged by the same
	// envelope as a schema Pod, because it is the same kind of Pod.
	AdmissionSnapshot *PodAdmissionSnapshot `json:"admissionSnapshot,omitempty"`
}

// MigrationPolicy decides when a planned sequence may execute.
type MigrationPolicy struct {
	// Apply defaults to OnApproval. A migration artifact carries arbitrary SQL,
	// and no analyzer classifies arbitrary SQL as safe, so the conservative
	// setting is the default one rather than the one an operator opts into.
	// +kubebuilder:default=OnApproval
	Apply ApplyPolicy `json:"apply,omitempty"`

	// LockTimeout bounds the wait for the database's own migration lock. It is
	// the lock the engine takes, not the Kubernetes Lease: two controllers that
	// never run at the same time still need the database to serialize them.
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('1h')",message="policy.lockTimeout must be between 1s and 1h"
	LockTimeout metav1.Duration `json:"lockTimeout,omitempty"`

	// TransactionMode is how Ptah is asked to wrap the run. The spellings are
	// Ptah's own, because a second vocabulary for the same idea is a second
	// place to hold in agreement with something this repository does not own.
	//
	// Unset means the operator passes no mode and Ptah chooses, which is what
	// every migration does today. That is deliberate and not an oversight: a
	// default here would change how every stored resource already runs, and it
	// would pin this API to a default Ptah is free to move.
	//
	// It is on the policy rather than on the execution block because the two
	// kinds share that block, and a PtahSchema runs no migrations and has no
	// mode to choose. What the policy already says -- apply, lockTimeout -- is
	// the same kind of statement: how a run is permitted to be made.
	//
	// A MySQL-family database refuses "file" whenever an interceptor is
	// installed, which is why this is not a preference. See #132.
	// +kubebuilder:validation:Enum=file;none
	TransactionMode string `json:"transactionMode,omitempty"`
}

// PtahMigrationSpec declares a database, a migration artifact, and the terms
// under which its pending migrations may run.
//
// It is deliberately not a mode of PtahSchema. A schema resource reconciles a
// desired structure; this one executes a prepared sequence and matches a
// history. The two have different state models, and a single resource carrying
// both would have fields that mean nothing in one of them.
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.connectTimeout) || !has(self.execution.activeDeadlineSeconds) || duration(self.execution.connectTimeout).getMilliseconds() <= self.execution.activeDeadlineSeconds * 1000",message="execution.connectTimeout must not exceed execution.activeDeadlineSeconds"
// +kubebuilder:validation:XValidation:rule="!has(self.policy) || !has(self.policy.lockTimeout) || !has(self.execution) || !has(self.execution.activeDeadlineSeconds) || duration(self.policy.lockTimeout).getMilliseconds() <= self.execution.activeDeadlineSeconds * 1000",message="policy.lockTimeout must not exceed execution.activeDeadlineSeconds"
// +kubebuilder:validation:XValidation:rule="self.target.urlFrom.name.size() > 0 && self.target.urlFrom.key.size() > 0 && (!has(self.target.urlFrom.optional) || !self.target.urlFrom.optional)",message="target.urlFrom must name a required Secret key"
// +kubebuilder:validation:XValidation:rule="self.artifact.verificationPolicyFrom.name.size() > 0 && self.artifact.verificationPolicyFrom.key.size() > 0 && (!has(self.artifact.verificationPolicyFrom.optional) || !self.artifact.verificationPolicyFrom.optional)",message="artifact.verificationPolicyFrom must name a required ConfigMap key"
// +kubebuilder:validation:XValidation:rule="!has(self.artifact.transport) || !has(self.artifact.transport.caFrom) || (self.artifact.transport.caFrom.name.size() > 0 && self.artifact.transport.caFrom.key.size() > 0 && (!has(self.artifact.transport.caFrom.optional) || !self.artifact.transport.caFrom.optional))",message="artifact.transport.caFrom must name a required ConfigMap key"
type PtahMigrationSpec struct {
	// Target is the database this sequence runs against, named through a Secret
	// the manager never reads.
	Target DatabaseTargetSpec `json:"target"`

	// Artifact is the OCI migration directory this history is matched against.
	// It reuses the schema path's credential-isolated source contract: fetching
	// an artifact never hands registry credentials to the process that runs SQL.
	Artifact OCIArtifactSourceSpec `json:"artifact"`

	// Policy decides what may run without a person: the apply mode, the
	// approval requirement, and the locks a run takes.
	// +kubebuilder:default={}
	Policy MigrationPolicy `json:"policy,omitempty"`

	// Interval is the cadence for resolving a mutable tag and re-reading the
	// history.
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s') && duration(self) <= duration('24h')",message="interval must be between 10s and 24h"
	Interval metav1.Duration `json:"interval,omitempty"`

	// Execution shapes the Job a run happens in: its deadlines, its resources
	// and the scheduling it inherits.
	// +kubebuilder:default={}
	Execution ExecutionSpec `json:"execution,omitempty"`

	// Suspend prevents new Jobs. A Job already applying is observed to a
	// terminal result: a migration that is running is never abandoned, because
	// the database would be left in a state nothing recorded.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// MigrationHistoryStatus is what the database's own revision table said when
// the controller last read it.
//
// It is a summary of evidence, not a decision: the counts and flags let a
// reader see why the controller is planning, blocked, or idle without reading
// the database itself.
type MigrationHistoryStatus struct {
	// ObservedAt is when the history was read.
	ObservedAt metav1.Time `json:"observedAt"`

	// ContractVersion is the version of Ptah's status document this summary was
	// read from. A version the controller does not know is refused before the
	// database is touched rather than read as if it meant the same thing.
	// +kubebuilder:validation:Minimum=1
	ContractVersion int32 `json:"contractVersion"`

	// CurrentVersion is the highest version the revision table records.
	// +kubebuilder:validation:Minimum=0
	CurrentVersion int64 `json:"currentVersion"`

	// CheckpointVersion is the checkpoint covering the versions below it, and
	// zero where none applies. It stays set after the bootstrap has run: the
	// coverage is what keeps those versions applied rather than pending.
	// +kubebuilder:validation:Minimum=0
	CheckpointVersion int64 `json:"checkpointVersion,omitempty"`

	// AppliedCount and PendingCount describe the artifact against this history.
	// +kubebuilder:validation:Minimum=0
	AppliedCount int32 `json:"appliedCount"`
	// PendingCount is how many the artifact still has to apply, as Ptah selects
	// them rather than as a subtraction of the two counts.
	// +kubebuilder:validation:Minimum=0
	PendingCount int32 `json:"pendingCount"`

	// Fingerprint is this exact reading of the revision table. A plan names it
	// as its premise, and a history that moved between planning and execution
	// invalidates the plan rather than being applied to.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Fingerprint string `json:"fingerprint"`

	// TargetIdentityDigest is the credential-free identity of the database this
	// history was read from, as the executor derived it. A plan that was made
	// against one database is never executed against another.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	TargetIdentityDigest string `json:"targetIdentityDigest"`

	// Dirty reports a revision row a failed or interrupted run left behind.
	// Nothing applies while one exists.
	Dirty bool `json:"dirty,omitempty"`

	// ModifiedVersions names applied migrations whose files no longer account
	// for them. It is the refusal a versioned workflow exists to make, so the
	// versions are published rather than summarized.
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	ModifiedVersions []int64 `json:"modifiedVersions,omitempty"`

	// OutOfOrderVersions names migrations the artifact carries below a version
	// the database has already applied. Linear execution refuses them, so the
	// versions are published rather than counted: what a person decides here
	// depends on which migration arrived late.
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	OutOfOrderVersions []int64 `json:"outOfOrderVersions,omitempty"`
}

// MigrationRunStatus is the evidence of the last execution, as the database
// accounted for it.
type MigrationRunStatus struct {
	// Outcome is the verdict Ptah read from the revision table.
	Outcome MigrationRunOutcome `json:"outcome"`

	// JobName and JobUID identify the execution this evidence came from. The
	// UID is what makes a replacement Job with the same name a different run.
	JobName string `json:"jobName,omitempty"`
	// JobUID is that Job's UID.
	JobUID types.UID `json:"jobUID,omitempty"`

	// StartedAt is when the run began.
	StartedAt metav1.Time `json:"startedAt"`
	// FinishedAt is when its result was read. An unfinished run has none.
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// AppliedVersions names the selected migrations the history recorded
	// afterwards, so a migration the history already held is not reported as
	// this run's work.
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	AppliedVersions []int64 `json:"appliedVersions,omitempty"`

	// Message is a safe explanation. It never carries database rows, and never
	// carries the SQL a migration ran.
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// UnresolvedMigrationRunStatus is the execution whose effect on the database
// nobody has established, and the evidence a person needs to go and establish
// it.
//
// It exists because a condition cannot hold this. A condition says why the
// resource is refused right now, and every later refusal -- a realm a second
// resource claimed, a dirty revision, an applied migration whose file moved --
// legitimately writes its own reason over it. Whether a mutation that may have
// run was ever accounted for is a different question, and no refusal answers
// it, so it is recorded rather than inferred.
type UnresolvedMigrationRunStatus struct {
	// An Enum marker here would not narrow anything: the one on
	// MigrationRunOutcome wins, and the generated schema keeps all five
	// values. The two this field carries are enforced by the controller that
	// writes it.

	// Outcome is what the run's own evidence said, and is always Partial or
	// Unknown: Partial committed some of a migration's statements and not the
	// rest, and Unknown could not be read at all. No other outcome leaves the
	// database in a state nobody can name, so no other outcome is recorded
	// here.
	Outcome MigrationRunOutcome `json:"outcome"`

	// OperationID is the Apply claim that ran, so this record names one attempt
	// rather than the resource in general.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	OperationID string `json:"operationID,omitempty"`

	// JobName and JobUID identify the execution. The UID is what makes a
	// replacement Job with the same name a different run.
	// +kubebuilder:validation:MaxLength=253
	JobName string `json:"jobName,omitempty"`
	// JobUID is that Job's UID.
	JobUID types.UID `json:"jobUID,omitempty"`

	// PlanRef names the plan the run was carrying out, which is the work that
	// may have reached the database.
	PlanRef *ImmutableObjectReference `json:"planRef,omitempty"`

	// TargetIdentityDigest is the credential-free identity of the database the
	// run was dispatched against, taken from the claim rather than from the
	// run: an unresolved run is one whose own account was never read, so what
	// it reached is exactly what is unknown. The reading that settles this
	// record has to be of that
	// database: a history read somewhere else says nothing about what this run
	// did.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	TargetIdentityDigest string `json:"targetIdentityDigest,omitempty"`

	// RecordedAt is when the controller wrote this record.
	RecordedAt metav1.Time `json:"recordedAt"`
}

// PtahMigrationStatus is the controller's account of one migration resource.
type PtahMigrationStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is where the resource stands. It is a summary for a reader: the
	// conditions below are what a decision reads.
	Phase MigrationPhase `json:"phase,omitempty"`

	// Conditions are the readable verdicts: whether the artifact resolved and
	// verified, whether the history could be read, whether a plan is ready,
	// whether an approval is required, and whether the sequence is in sync.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Artifact is the resolved, credential-free artifact binding every later
	// operation of this cycle uses. A tag resolves once; the digest is what
	// travels.
	Artifact *OCIArtifactAccessBinding `json:"artifact,omitempty"`

	// ExecutionBinding is the component identity this resource's work is bound
	// to: the manager that authorized it and the executor and runner that will
	// carry it out. It is the same contract the schema path publishes, because
	// the question it answers is the same one: a rollout that changed any of
	// them has to invalidate a plan rather than execute it under new bytes.
	ExecutionBinding *ExecutionBindingStatus `json:"executionBinding,omitempty"`

	// ActiveOperation is the claim the controller is currently carrying out,
	// and nil when nothing is in flight.
	ActiveOperation *MigrationOperationStatus `json:"activeOperation,omitempty"`

	// History is the last reading of the database's own revision table.
	History *MigrationHistoryStatus `json:"history,omitempty"`

	// Plan names the immutable plan object the controller published for the
	// current pending sequence, and is cleared once that sequence is gone.
	Plan *ImmutableObjectReference `json:"plan,omitempty"`

	// LastRun is the evidence of the most recent execution, kept across later
	// reconciliations so an operator can see what happened without the Job.
	LastRun *MigrationRunStatus `json:"lastRun,omitempty"`

	// UnresolvedRun is the execution nobody could account for, and is absent
	// while there is none. It is written when a run ends Partial or Unknown,
	// and removed only when a read-only reading of the same database finds
	// nothing of this artifact left to apply. While it is here nothing is
	// planned and nothing runs, whatever the conditions happen to say.
	UnresolvedRun *UnresolvedMigrationRunStatus `json:"unresolvedRun,omitempty"`

	// NextReconciliationTime is when the controller intends to look again.
	NextReconciliationTime *metav1.Time `json:"nextReconciliationTime,omitempty"`
}

// Condition types a PtahMigration publishes.
const (
	// ConditionMigrationReady reports that the history matches the artifact and
	// nothing is pending.
	ConditionMigrationReady = "Ready"
	// ConditionMigrationProgressing reports work in flight.
	ConditionMigrationProgressing = "Progressing"
	// ConditionMigrationBlocked reports a history this artifact cannot
	// continue, which the controller never resolves on its own.
	ConditionMigrationBlocked = "Blocked"
	// ConditionMigrationApprovalRequired reports a plan waiting for the exact
	// approval its policy requires.
	ConditionMigrationApprovalRequired = "ApprovalRequired"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ptahm
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.history.currentVersion`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.history.pendingCount`
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.status.lastRun.outcome`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.artifact.digest`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PtahMigrationSpec   `json:"spec"`
	Status PtahMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PtahMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahMigration `json:"items"`
}

// Condition reasons a PtahMigration publishes in addition to the shared ones.
const (
	// ReasonHistoryMatched means the artifact and the revision table agree and
	// nothing is pending.
	ReasonHistoryMatched ConditionReason = "HistoryMatched"
	// ReasonMigrationsPending means the artifact has migrations the database
	// does not.
	ReasonMigrationsPending ConditionReason = "MigrationsPending"
	// ReasonHistoryDirty means a failed or interrupted run left a revision row
	// behind. Nothing applies while one exists, and the controller never
	// removes it: what a half-applied migration did is a question for a person
	// with the database in front of them.
	ReasonHistoryDirty ConditionReason = "HistoryDirty"
	// ReasonHistoryModified means an applied migration's file no longer
	// accounts for it. This is the refusal a versioned workflow exists to make.
	ReasonHistoryModified ConditionReason = "HistoryModified"
	// ReasonHistoryOutOfOrder means the artifact carries a migration below a
	// version the database has already applied. Linear execution refuses the
	// whole run, so the operator refuses before it publishes a plan nobody
	// could execute.
	ReasonHistoryOutOfOrder ConditionReason = "HistoryOutOfOrder"
	// ReasonHistoryAhead means the database records a migration this artifact
	// does not carry. Nothing about it is repairable from here: the operator
	// never rolls a database back to match an older artifact, and which of the
	// two is wrong is not a question a controller can answer.
	ReasonHistoryAhead ConditionReason = "HistoryAhead"
)

// ConditionMigrationArtifactVerified reports that the resolved artifact
// satisfied its verification policy.
const ConditionMigrationArtifactVerified = "ArtifactVerified"
