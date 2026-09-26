// Package coordination names the database realm a desired-state resource
// claims.
//
// A realm is what the census counts and what the target Lease serializes on. A
// resource names it one of two ways: a coordination key, which reaches only as
// far as the resource's own namespace, or a PtahRealm, which an administrator
// creates and which says itself which namespaces it admits. Every place that
// derives a realm from a spec derives it here, so the census, the Lease, the
// plan, the approval webhook and the Job builder cannot disagree about which
// realm one resource is in.
package coordination

import (
	"errors"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// Digest returns the realm the target of a resource in namespace claims.
//
// The API admits exactly one of coordinationKey and realmRef, and this refuses
// a target carrying both rather than choosing one: a spec the API validation
// would have refused names no realm.
func Digest(namespace string, target operatorv1alpha1.DatabaseTargetSpec) (string, error) {
	switch {
	case target.RealmRef != nil && target.CoordinationKey != "":
		return "", errors.New("a database target names both a coordination key and a realm")
	case target.RealmRef != nil:
		return fingerprint.DatabaseRealmDigest(string(target.Engine), target.RealmRef.Name)
	default:
		return fingerprint.DatabaseCoordinationDigest(string(target.Engine), namespace, target.CoordinationKey)
	}
}

// RealmName is the PtahRealm a target names, or "" for a namespace key.
func RealmName(target operatorv1alpha1.DatabaseTargetSpec) string {
	if target.RealmRef == nil {
		return ""
	}
	return target.RealmRef.Name
}
