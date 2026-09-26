package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RealmSharing says whether more than one resource may manage a realm's
// database at the same time.
// +kubebuilder:validation:Enum=Exclusive;Shared
type RealmSharing string

const (
	// RealmSharingExclusive admits one claimant at a time. A second one, in
	// any listed namespace, refuses every claimant until one of them leaves.
	RealmSharingExclusive RealmSharing = "Exclusive"
	// RealmSharingShared admits several claimants, provided each of them sets
	// spec.target.sharedRealm.
	RealmSharingShared RealmSharing = "Shared"
)

// PtahRealmSpec is an administrator's statement about one physical database:
// the engine it speaks, the namespaces whose resources may manage it, and
// whether more than one resource may manage it at once.
//
// It is the authorization for a claim. A PtahSchema or PtahMigration names a
// realm through spec.target.realmRef, and that names nothing the realm has not
// granted: a resource in a namespace the realm does not list is refused itself
// and is not counted against the resources the realm does admit.
type PtahRealmSpec struct {
	// Engine is the database family of this realm. A resource that names the
	// realm with another engine is refused. It cannot change: a realm is one
	// physical database, and a database does not change engine.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="engine is immutable; a realm is one physical database"
	Engine DatabaseEngine `json:"engine"`

	// Namespaces are the namespaces whose resources may claim this realm,
	// written out by name.
	//
	// There is deliberately no selector. Namespace labels are often writable
	// by whoever administers the namespace, and a selector would let that
	// person admit their own namespace to a database somebody else runs.
	//
	// Removing a namespace does not stop an operation already running there,
	// the same as a conflict does not: the resource is refused at its next
	// claim, before any Job.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespaces []string `json:"namespaces"`

	// Sharing is whether more than one resource may manage the database at
	// once.
	//
	// Exclusive admits one claimant. A second one, in any listed namespace,
	// is a conflict that refuses every claimant, whatever each declared.
	//
	// Shared admits several, on the rule a namespace-local key has: every
	// claimant sets spec.target.sharedRealm, and one that has not refuses them
	// all. Sharing is a statement both the administrator and each claimant
	// make; neither can make it for the other.
	Sharing RealmSharing `json:"sharing"`
}

// PtahRealm names one physical database at cluster scope and says which
// namespaces may manage it.
//
// A coordination key in spec.target is scoped to its resource's namespace, so a
// database managed from more than one namespace needs a realm, and only an
// administrator can create one. The manager reads realms and never writes them.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ptahr
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Sharing",type=string,JSONPath=`.spec.sharing`
// +kubebuilder:printcolumn:name="Namespaces",type=string,JSONPath=`.spec.namespaces`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahRealm struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the administrator's grant.
	Spec PtahRealmSpec `json:"spec"`
}

// PtahRealmList contains a list of PtahRealm.
// +kubebuilder:object:root=true
type PtahRealmList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahRealm `json:"items"`
}

// PtahRealmReference names a cluster-scoped PtahRealm.
type PtahRealmReference struct {
	// Name is the PtahRealm's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}
