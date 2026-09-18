package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PtahMigrationApprovalSpec binds one authenticated decision to one exact
// migration plan.
//
// The binding names the history as well as the plan. A migration plan's
// premise is the history it was computed against, so an approval that named
// only the plan would still be valid after somebody else's run moved the
// database underneath it.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="an approval is immutable; create a new approval instead"
type PtahMigrationApprovalSpec struct {
	// MigrationRef is the resource the approved sequence belongs to.
	MigrationRef ImmutableObjectReference `json:"migrationRef"`
	// PlanRef is the exact plan being approved.
	PlanRef ImmutableObjectReference `json:"planRef"`

	// PlanFingerprint is the plan's complete identity. The fields below are
	// that identity written out, so the decision can be read without fetching
	// the plan and checked against the live one before anything runs.
	PlanFingerprint string `json:"planFingerprint"`
	// HistoryFingerprint is the recorded history the plan was computed against.
	// Somebody else's run moves it, and this is what notices.
	HistoryFingerprint string `json:"historyFingerprint"`
	// ArtifactDigest is the OCI migration artifact the sequence comes from.
	ArtifactDigest string `json:"artifactDigest"`
	// CoordinationDigest is the database realm the approved run takes its turn
	// in.
	CoordinationDigest string `json:"coordinationDigest"`
	// TargetIdentityDigest is the database it was computed against.
	TargetIdentityDigest string `json:"targetIdentityDigest"`
	// PolicyFingerprint is the spec.policy it was computed under.
	PolicyFingerprint string `json:"policyFingerprint"`

	// VerificationPolicyUID is the policy object that accepted the artifact.
	VerificationPolicyUID types.UID `json:"verificationPolicyUID"`
	// VerificationPolicyDigest is that policy's content at the time.
	VerificationPolicyDigest string `json:"verificationPolicyDigest"`

	// ExecutionBindingID is the execution epoch the approved plan belongs to,
	// which changes on every operator transition.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID"`
	// ControllerImage is the digest-pinned manager that must dispatch the run.
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage"`
	// ControllerRevision is that manager's revision.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`
	ControllerRevision string `json:"controllerRevision"`
	// ControllerStateVersion is the state semantics it writes.
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32 `json:"controllerStateVersion"`
	// PtahVersion is the Ptah build the approved run must use.
	PtahVersion string `json:"ptahVersion"`
	// ExecutorImage is the digest-pinned image it runs in.
	ExecutorImage string `json:"executorImage"`
	// RunnerImage is the digest-pinned image that supervises it.
	RunnerImage string `json:"runnerImage"`
	// RunnerProtocolVersion is the result-frame protocol that runner speaks.
	// +kubebuilder:validation:Minimum=1
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`

	// Approver is stamped by the mutating webhook from the authenticated
	// request; whatever a client writes here is replaced.
	Approver ApprovalIdentity `json:"approver"`
	// ApprovedAt is when that stamp was made.
	ApprovedAt metav1.Time `json:"approvedAt"`
	// MutationRequestUID records the mutating AdmissionReview that stamped the
	// authenticated identity, exactly as a schema approval does.
	MutationRequestUID string `json:"mutationRequestUID"`
}

// PtahMigrationApprovalStatus tells an approver whether the exact binding was
// accepted, consumed, or made stale by a later reading of the history.
type PtahMigrationApprovalStatus struct {
	// ObservedGeneration is the approval generation this status was written
	// for. An approval is immutable, so it moves once.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions say whether the binding was Accepted, has been Consumed by a
	// run, or went Stale because the history, the artifact or the policy moved
	// first.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ptahmapprove
// +kubebuilder:printcolumn:name="Migration",type=string,JSONPath=`.spec.migrationRef.name`
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.planRef.name`
// +kubebuilder:printcolumn:name="Approver",type=string,JSONPath=`.spec.approver.username`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=='Accepted')].status`
// +kubebuilder:printcolumn:name="Stale",type=string,JSONPath=`.status.conditions[?(@.type=='Stale')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahMigrationApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PtahMigrationApprovalSpec   `json:"spec"`
	Status PtahMigrationApprovalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PtahMigrationApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahMigrationApproval `json:"items"`
}
