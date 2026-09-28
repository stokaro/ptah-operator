package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PtahMigrationApprovalSpec binds one authenticated decision to one exact
// migration plan.
//
// The decision is three identifiers: the migration, the plan, and the plan's
// fingerprint. A migration plan's fingerprint binds the history it was
// computed against as well as the sequence, the artifact, the policy, the
// target and the execution binding, so an approval that names the plan by UID
// and fingerprint is retired by somebody else's run moving the database
// underneath it, without carrying a copy of the history it was given under.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="an approval is immutable; create a new approval instead"
type PtahMigrationApprovalSpec struct {
	// MigrationRef is the resource the approved sequence belongs to.
	MigrationRef ImmutableObjectReference `json:"migrationRef"`
	// PlanRef is the exact plan being approved.
	PlanRef ImmutableObjectReference `json:"planRef"`
	// PlanFingerprint is the plan's spec.fingerprint: its complete identity,
	// checked against the live plan before anything runs.
	PlanFingerprint string `json:"planFingerprint"`

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
