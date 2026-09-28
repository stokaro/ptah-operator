package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//
// The decision is three identifiers: the schema, the plan, and the plan's
// fingerprint. The fingerprint is the plan's complete identity -- the
// artifact, the observed and desired state, the policy, the verification
// policy, the target, the execution binding and what the manager read out of
// the plan bytes -- and a plan is immutable, so an approval that names a plan
// by UID and fingerprint names every one of those without carrying a copy.
// Admission checks the three against the live plan; the controller checks
// them again before Apply.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="an approval is immutable; create a new approval instead"
type PtahSchemaApprovalSpec struct {
	// SchemaRef is the resource the approved change belongs to.
	SchemaRef ImmutableObjectReference `json:"schemaRef"`
	// PlanRef is the exact plan being approved. A plan is immutable, so this
	// names bytes rather than an intention.
	PlanRef ImmutableObjectReference `json:"planRef"`
	// PlanFingerprint is the plan's spec.fingerprint: its complete approval
	// identity. A plan under a different binding has a different fingerprint,
	// so an approval that names this one cannot carry over to it.
	PlanFingerprint string `json:"planFingerprint"`

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
