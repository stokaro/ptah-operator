package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ApprovalIdentity is stamped from the authenticated admission request. The
// API client does not choose these fields.
type ApprovalIdentity struct {
	// Username the API server authenticated the request as.
	Username string `json:"username"`
	// UID of that user, where the authenticator provides one.
	UID string `json:"uid,omitempty"`
	// Groups the authenticated user belonged to at that moment.
	// +kubebuilder:validation:MaxItems=64
	Groups []string `json:"groups,omitempty"`
}

// PtahSchemaApprovalSpec binds one authenticated decision to one exact plan.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="an approval is immutable; create a new approval instead"
type PtahSchemaApprovalSpec struct {
	// SchemaRef is the resource the approved change belongs to.
	SchemaRef ImmutableObjectReference `json:"schemaRef"`
	// PlanRef is the exact plan being approved. A plan is immutable, so this
	// names bytes rather than an intention.
	PlanRef ImmutableObjectReference `json:"planRef"`

	// PlanFingerprint is the plan's complete approval identity. Everything
	// below is the same identity written out, so a reader can see what was
	// approved without fetching the plan, and the admission that accepts this
	// approval checks each part against the live plan.
	PlanFingerprint string `json:"planFingerprint"`
	// ArtifactDigest is the OCI artifact the approved plan was computed from.
	ArtifactDigest string `json:"artifactDigest"`
	// CoordinationDigest is the database realm the approved apply takes its
	// turn in.
	CoordinationDigest string `json:"coordinationDigest"`
	// TargetIdentityDigest is the database the approved plan was computed
	// against.
	TargetIdentityDigest string `json:"targetIdentityDigest"`
	// ActualStateFingerprint is the observed database state that was planned
	// from. A database that has moved since makes this approval stale.
	ActualStateFingerprint string `json:"actualStateFingerprint"`
	// DesiredStateFingerprint is the state the artifact declared.
	DesiredStateFingerprint string `json:"desiredStateFingerprint"`
	// PolicyFingerprint is the spec.policy the plan was computed under, so an
	// edited policy retires this decision instead of inheriting it.
	PolicyFingerprint string `json:"policyFingerprint"`
	// VerificationPolicyUID is the verification policy object that accepted the
	// artifact.
	VerificationPolicyUID types.UID `json:"verificationPolicyUID"`
	// VerificationPolicyDigest is that policy's content at the time.
	VerificationPolicyDigest string `json:"verificationPolicyDigest"`
	// ExecutionBindingID is the execution epoch the approved plan belongs to. It
	// changes whenever a component that decides what the plan means when it
	// runs changes, including a change back to byte-identical versions, so an
	// approval cannot survive such a rollout unseen. A manager upgrade that
	// changes none of them keeps the epoch and this approval.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID"`
	// ControllerStateVersion is the controller-state semantics the approved
	// apply must be dispatched under.
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32 `json:"controllerStateVersion"`
	// PtahVersion is the Ptah build the approved apply must run.
	PtahVersion string `json:"ptahVersion"`
	// ExecutorImage is the digest-pinned image it must run in.
	ExecutorImage string `json:"executorImage"`
	// RunnerProtocolVersion is the protocol the runner that supervises it must
	// speak: what the runner enforces inside the Pod and the result frame it
	// returns.
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`

	// Approver is stamped by the mutating webhook from the authenticated
	// request. Whatever an API client writes here is replaced.
	Approver ApprovalIdentity `json:"approver"`
	// ApprovedAt is when that stamp was made, by the same webhook.
	ApprovedAt metav1.Time `json:"approvedAt"`
	// MutationRequestUID records the mutating AdmissionReview that stamped the
	// authenticated identity. Kubernetes creates a distinct AdmissionReview UID
	// for the later validating webhook, so the validator checks this field is
	// present while matching identity against its own authenticated UserInfo.
	MutationRequestUID string `json:"mutationRequestUID"`
}

// PtahSchemaApprovalStatus tells an approver whether the exact binding was
// accepted, consumed, or made stale by a later observation.
type PtahSchemaApprovalStatus struct {
	// ObservedGeneration is the approval generation this status was written
	// for. An approval is immutable, so it moves only when the object is first
	// reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions say whether the binding was Accepted, has been Consumed by an
	// apply, or went Stale because the database, the artifact or the policy
	// moved before it could run.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	ConditionApprovalAccepted = "Accepted"
	ConditionApprovalConsumed = "Consumed"
	ConditionApprovalStale    = "Stale"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ptahapprove
// +kubebuilder:printcolumn:name="Schema",type=string,JSONPath=`.spec.schemaRef.name`
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.planRef.name`
// +kubebuilder:printcolumn:name="Approver",type=string,JSONPath=`.spec.approver.username`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=='Accepted')].status`
// +kubebuilder:printcolumn:name="Stale",type=string,JSONPath=`.status.conditions[?(@.type=='Stale')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahSchemaApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PtahSchemaApprovalSpec   `json:"spec"`
	Status PtahSchemaApprovalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PtahSchemaApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahSchemaApproval `json:"items"`
}
