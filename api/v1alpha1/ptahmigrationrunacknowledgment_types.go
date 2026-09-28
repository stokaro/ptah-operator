package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PtahMigrationRunAcknowledgmentSpec is a person's statement that the run a
// PtahMigration recorded in status.unresolvedRun has been accounted for.
//
// The decision is two identifiers: the migration, by name and UID, and the
// Apply attempt the record names. An attempt's ID is never reused, so an
// acknowledgment cannot carry over to a later run, and a migration rebuilt
// under the same name is another object with another UID.
//
// It is a separate object rather than a status write so that the decision
// carries the identity that made it. Status is written by the manager alone;
// an acknowledgment is written by a person, and admission stamps who.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="an acknowledgment is immutable; create a new acknowledgment instead"
type PtahMigrationRunAcknowledgmentSpec struct {
	// MigrationRef is the resource whose unresolved run this settles.
	MigrationRef ImmutableObjectReference `json:"migrationRef"`
	// OperationID is the status.unresolvedRun.operationID this acknowledges:
	// one Apply attempt, and no other.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	OperationID string `json:"operationID"`

	// AcknowledgedBy is stamped by the mutating webhook from the authenticated
	// request. Whatever an API client writes here is replaced.
	AcknowledgedBy ApprovalIdentity `json:"acknowledgedBy"`
	// AcknowledgedAt is when that stamp was made, by the same webhook.
	AcknowledgedAt metav1.Time `json:"acknowledgedAt"`
	// MutationRequestUID records the mutating AdmissionReview that stamped the
	// identity, exactly as an approval does.
	MutationRequestUID string `json:"mutationRequestUID"`
}

// PtahMigrationRunAcknowledgmentStatus says what the controller did with the
// acknowledgment.
type PtahMigrationRunAcknowledgmentStatus struct {
	// ObservedGeneration is the generation this status was written for. An
	// acknowledgment is immutable, so it moves once.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions say whether the acknowledgment settled the run it names
	// (Consumed), or named a run the migration was not waiting on (Stale).
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types a PtahMigrationRunAcknowledgment publishes.
const (
	// ConditionAcknowledgmentConsumed reports that the acknowledgment settled
	// the unresolved run it names.
	ConditionAcknowledgmentConsumed = "Consumed"
	// ConditionAcknowledgmentStale reports an acknowledgment that names a run
	// the migration is not waiting on, so it settled nothing.
	ConditionAcknowledgmentStale = "Stale"
)

// Condition reasons a PtahMigrationRunAcknowledgment publishes.
const (
	// ReasonRunAcknowledged means the acknowledgment settled the run it names.
	ReasonRunAcknowledged ConditionReason = "RunAcknowledged"
	// ReasonRunNotUnresolved means the run the acknowledgment names is not the
	// one the migration records as unresolved: another acknowledgment or a
	// reading of the database settled it first, or it was never this
	// migration's.
	ReasonRunNotUnresolved ConditionReason = "RunNotUnresolved"
)

// PtahMigrationRunAcknowledgment settles one run a PtahMigration could not
// account for, in the name of the person who created it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ptahmack
// +kubebuilder:printcolumn:name="Migration",type=string,JSONPath=`.spec.migrationRef.name`
// +kubebuilder:printcolumn:name="Acknowledged By",type=string,JSONPath=`.spec.acknowledgedBy.username`
// +kubebuilder:printcolumn:name="Consumed",type=string,JSONPath=`.status.conditions[?(@.type=='Consumed')].status`
// +kubebuilder:printcolumn:name="Stale",type=string,JSONPath=`.status.conditions[?(@.type=='Stale')].status`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.spec.operationID`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahMigrationRunAcknowledgment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PtahMigrationRunAcknowledgmentSpec   `json:"spec"`
	Status PtahMigrationRunAcknowledgmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PtahMigrationRunAcknowledgmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahMigrationRunAcknowledgment `json:"items"`
}
