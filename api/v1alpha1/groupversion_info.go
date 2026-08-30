// Package v1alpha1 contains the first public API for the Ptah operator.
// +kubebuilder:object:generate=true
// +groupName=operator.ptah.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion identifies this API group and version.
	GroupVersion = schema.GroupVersion{Group: "operator.ptah.dev", Version: "v1alpha1"}

	// SchemeBuilder registers this package's API types.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds this package's API types to a runtime Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&PtahSchema{},
		&PtahSchemaList{},
		&PtahSchemaPlan{},
		&PtahSchemaPlanList{},
		&PtahSchemaApproval{},
		&PtahSchemaApprovalList{},
	)
}
