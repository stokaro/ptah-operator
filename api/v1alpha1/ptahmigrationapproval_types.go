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
	MigrationRef ImmutableObjectReference `json:"migrationRef"`
	PlanRef      ImmutableObjectReference `json:"planRef"`

	PlanFingerprint      string `json:"planFingerprint"`
	HistoryFingerprint   string `json:"historyFingerprint"`
	ArtifactDigest       string `json:"artifactDigest"`
	CoordinationDigest   string `json:"coordinationDigest"`
	TargetIdentityDigest string `json:"targetIdentityDigest"`
	PolicyFingerprint    string `json:"policyFingerprint"`

	VerificationPolicyUID    types.UID `json:"verificationPolicyUID"`
	VerificationPolicyDigest string    `json:"verificationPolicyDigest"`

	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID"`
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`
	ControllerRevision string `json:"controllerRevision"`
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32  `json:"controllerStateVersion"`
	PtahVersion            string `json:"ptahVersion"`
	ExecutorImage          string `json:"executorImage"`
	RunnerImage            string `json:"runnerImage"`
	// +kubebuilder:validation:Minimum=1
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`

	Approver   ApprovalIdentity `json:"approver"`
	ApprovedAt metav1.Time      `json:"approvedAt"`
	// MutationRequestUID records the mutating AdmissionReview that stamped the
	// authenticated identity, exactly as a schema approval does.
	MutationRequestUID string `json:"mutationRequestUID"`
}

// PtahMigrationApprovalStatus tells an approver whether the exact binding was
// accepted, consumed, or made stale by a later reading of the history.
type PtahMigrationApprovalStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
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
