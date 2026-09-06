package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ApprovalIdentity is stamped from the authenticated admission request. The
// API client does not choose these fields.
type ApprovalIdentity struct {
	Username string `json:"username"`
	UID      string `json:"uid,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	Groups []string `json:"groups,omitempty"`
}

// PtahSchemaApprovalSpec binds one authenticated decision to one exact plan.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="an approval is immutable; create a new approval instead"
type PtahSchemaApprovalSpec struct {
	SchemaRef ImmutableObjectReference `json:"schemaRef"`
	PlanRef   ImmutableObjectReference `json:"planRef"`

	PlanFingerprint          string    `json:"planFingerprint"`
	ArtifactDigest           string    `json:"artifactDigest"`
	CoordinationDigest       string    `json:"coordinationDigest"`
	TargetIdentityDigest     string    `json:"targetIdentityDigest"`
	ActualStateFingerprint   string    `json:"actualStateFingerprint"`
	DesiredStateFingerprint  string    `json:"desiredStateFingerprint"`
	PolicyFingerprint        string    `json:"policyFingerprint"`
	VerificationPolicyUID    types.UID `json:"verificationPolicyUID"`
	VerificationPolicyDigest string    `json:"verificationPolicyDigest"`
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID,omitempty"`
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`
	ControllerRevision string `json:"controllerRevision,omitempty"`
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32  `json:"controllerStateVersion,omitempty"`
	PtahVersion            string `json:"ptahVersion"`
	ExecutorImage          string `json:"executorImage"`
	RunnerImage            string `json:"runnerImage"`
	RunnerProtocolVersion  int32  `json:"runnerProtocolVersion"`

	Approver   ApprovalIdentity `json:"approver"`
	ApprovedAt metav1.Time      `json:"approvedAt"`
	// MutationRequestUID records the mutating AdmissionReview that stamped the
	// authenticated identity. Kubernetes creates a distinct AdmissionReview UID
	// for the later validating webhook, so the validator checks this field is
	// present while matching identity against its own authenticated UserInfo.
	MutationRequestUID string `json:"mutationRequestUID"`
}

// PtahSchemaApprovalStatus tells an approver whether the exact binding was
// accepted, consumed, or made stale by a later observation.
type PtahSchemaApprovalStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
