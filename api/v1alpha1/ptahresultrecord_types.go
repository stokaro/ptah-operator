package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PtahResultRecordSpec is an immutable record in the runner result transport.
// The transport validates the record's protocol, owner, binding, and digest.
// This schema bounds the stored bytes and prevents changing a published record.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a result record is immutable; create a new operation instead"
type PtahResultRecordSpec struct {
	// Type identifies the record's role in credential or result publication.
	// Credential records contain delivery key material; intent, chunk, and
	// completion records contain authenticated operation evidence.
	// +kubebuilder:validation:Enum=credential;intent;chunk;complete
	Type string `json:"type"`

	// Data is the record's exact bytes, base64-encoded on the wire. Readers must
	// validate its protocol and digest before using it. Treat every role as
	// confidential: payloads may contain SQL and credentials contain private keys.
	// MaxLength is the base64 length of the 512 KiB record ceiling.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=699052
	Data []byte `json:"data"`
}

// PtahResultRecord stores one part of a durable runner result or a delivery
// credential. It has its own RBAC resource so the control plane can read these
// records without permission to read database Secrets. It has no status or
// mutable payload; a separate completion record commits a complete publication.
// Records belong to their exact operation resource or publication intent.
// They are internal transport objects, not desired database configuration.
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ptahresult
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
type PtahResultRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PtahResultRecordSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type PtahResultRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahResultRecord `json:"items"`
}
