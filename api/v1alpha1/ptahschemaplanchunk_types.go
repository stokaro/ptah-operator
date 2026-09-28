package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PtahSchemaPlanChunkSpec is one piece of a schema plan's bytes, written by the
// controller with the plan and never changed afterwards.
//
// It carries the bytes and nothing else. The plan that owns the chunk records
// its index, size and digest in spec.chunks, and every reader checks the bytes
// against that record, so a copy here would be a second answer to the same
// question and would have to be held to the first.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a plan chunk is immutable; generate a new plan instead"
type PtahSchemaPlanChunkSpec struct {
	// Data is this chunk's bytes, base64-encoded on the wire. A plan is the
	// concatenation of its chunks' data in the order its spec.chunks gives,
	// and a chunk boundary falls wherever the byte count does, which may be
	// inside a statement or a character. Read a whole plan with kubectl ptah
	// plan rather than by decoding chunks.
	//
	// MaxLength is the base64 length of the 512 KiB chunk ceiling.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=699052
	Data []byte `json:"data"`
}

// PtahSchemaPlanChunk holds one ordered piece of a PtahSchemaPlan's bytes. The
// controller writes the chunks a plan names before it marks the plan Ready,
// and they are deleted with it.
//
// A chunk is its own kind so that reading a plan takes one RBAC rule on this
// resource, granted per namespace, and nothing that also reads the
// namespace's ConfigMaps. There is no status: a chunk reports nothing, and
// the plan's status records which chunk objects were verified.
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ptahchunk
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.metadata.ownerReferences[?(@.kind=='PtahSchemaPlan')].name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahSchemaPlanChunk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PtahSchemaPlanChunkSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type PtahSchemaPlanChunkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahSchemaPlanChunk `json:"items"`
}
