// Package sourcebinding holds the comparisons every component makes about a
// resource's source: which plan contract a source gives, what a plan binds
// in place of an artifact digest, and whether an operation's access binding
// reads the content a plan names. The controllers, the Job builder and both
// admission webhooks ask these questions; answering them in one place keeps
// the answers from drifting apart.
package sourcebinding

import (
	"errors"
	"regexp"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

var contentDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ConfigMapPlanSource is what a plan computed from a ConfigMap source binds:
// the ConfigMap's UID and the content digest of the selected files.
func ConfigMapPlanSource(revision operatorv1alpha1.ConfigMapSourceRevision, snapshot operatorv1alpha1.SourceSnapshotBinding) *operatorv1alpha1.PlanSourceBinding {
	return &operatorv1alpha1.PlanSourceBinding{
		Kind:          operatorv1alpha1.SourceKindConfigMap,
		Revision:      string(revision.UID),
		ContentDigest: snapshot.ContentDigest,
	}
}

// SchemaPlanSource is what a plan computed from a schema's resolved source
// binds: the artifact digest of an OCI source, or the source binding of a
// ConfigMap one. A ConfigMap source that has not been read yet binds nothing.
func SchemaPlanSource(status operatorv1alpha1.SchemaSourceStatus) (string, *operatorv1alpha1.PlanSourceBinding) {
	if status.ConfigMap != nil && status.Snapshot != nil {
		return "", ConfigMapPlanSource(*status.ConfigMap, *status.Snapshot)
	}
	return status.Digest, nil
}

// MigrationPlanSource is what a plan computed from a migration's resolved
// source binds: the artifact digest of an OCI source, or the source binding
// of a ConfigMap one.
func MigrationPlanSource(artifact *operatorv1alpha1.SourceAccessBinding, revision *operatorv1alpha1.ConfigMapSourceRevision) (string, *operatorv1alpha1.PlanSourceBinding) {
	if artifact == nil {
		return "", nil
	}
	if artifact.Snapshot != nil {
		if revision == nil {
			return "", nil
		}
		return "", ConfigMapPlanSource(*revision, *artifact.Snapshot)
	}
	return artifact.Digest, nil
}

// Fingerprint converts a plan's source binding to the form the fingerprint
// binds.
func Fingerprint(source *operatorv1alpha1.PlanSourceBinding) *fingerprint.PlanSource {
	if source == nil {
		return nil
	}
	return &fingerprint.PlanSource{Kind: string(source.Kind), Revision: source.Revision, ContentDigest: source.ContentDigest}
}

// ContractVersion is the plan contract a plan bound to source carries.
func ContractVersion(source *operatorv1alpha1.PlanSourceBinding) int32 {
	return fingerprint.PlanContractVersionFor(Fingerprint(source))
}

// WrittenContract reports whether a plan carries the contract version this
// manager writes for its source binding: version 1 with no source, version 2
// with one.
func WrittenContract(version int32, source *operatorv1alpha1.PlanSourceBinding) bool {
	return fingerprint.ValidatePlanContractVersion(version) == nil && version == ContractVersion(source)
}

// Equal reports whether two plan source bindings name the same source.
func Equal(a, b *operatorv1alpha1.PlanSourceBinding) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SamePlanSource reports whether two plans, or a plan and a status, bind the
// same source: the same artifact digest and the same source binding.
func SamePlanSource(artifactA string, sourceA *operatorv1alpha1.PlanSourceBinding, artifactB string, sourceB *operatorv1alpha1.PlanSourceBinding) bool {
	return artifactA == artifactB && Equal(sourceA, sourceB)
}

// ValidateAccess holds an access binding to one of its two shapes: an OCI
// reference pinned to a matching digest, or a well-formed snapshot and
// nothing that belongs to a registry.
func ValidateAccess(binding operatorv1alpha1.SourceAccessBinding) error {
	if binding.Snapshot == nil {
		if err := ocireference.ValidatePinned(binding.ResolvedReference, binding.Digest); err != nil {
			return errors.New("artifact access binding must contain a matching immutable SHA-256 reference")
		}
		return nil
	}
	snapshot := binding.Snapshot
	if binding.ResolvedReference != "" || binding.Digest != "" || binding.RegistryAuthFrom != nil ||
		binding.Transport.PlainHTTP || binding.Transport.CAFrom != nil {
		return errors.New("a snapshot access binding names no registry")
	}
	if !sourcesnapshot.IsName(snapshot.Name) || snapshot.UID == "" || !contentDigestPattern.MatchString(snapshot.ContentDigest) {
		return errors.New("a snapshot access binding names a snapshot, its UID and a content digest")
	}
	if snapshot.SchemaFile != "" {
		if err := sourcecontent.ValidatePath(snapshot.SchemaFile); err != nil {
			return errors.New("a snapshot access binding names an invalid schema file")
		}
	}
	return nil
}

// AccessGives reports whether an operation reading through binding reads the
// content a plan bound: the artifact digest of an OCI plan, or the content
// digest of a source plan.
func AccessGives(binding operatorv1alpha1.SourceAccessBinding, artifactDigest string, source *operatorv1alpha1.PlanSourceBinding) bool {
	if binding.Snapshot != nil {
		return source != nil && artifactDigest == "" && binding.Snapshot.ContentDigest == source.ContentDigest
	}
	return source == nil && artifactDigest != "" && binding.Digest == artifactDigest
}

// SchemaAccess is the access binding a schema's resolved source gives later
// operations: the snapshot of a ConfigMap source, or nil for an OCI source,
// whose binding the caller builds from the registry selectors.
func SchemaAccess(status operatorv1alpha1.SchemaSourceStatus) *operatorv1alpha1.SourceAccessBinding {
	if status.Snapshot == nil {
		return nil
	}
	return &operatorv1alpha1.SourceAccessBinding{Snapshot: status.Snapshot.DeepCopy()}
}

// SchemaSourceReady reports whether later operations may read a schema's
// source: a verified OCI artifact, or the snapshot of the ConfigMap the spec
// names.
func SchemaSourceReady(spec operatorv1alpha1.SourceSpec, status operatorv1alpha1.SchemaSourceStatus) bool {
	if status.Snapshot != nil {
		return spec.ConfigMap != nil && status.ConfigMap != nil && status.ConfigMap.Name == spec.ConfigMap.Name
	}
	return status.Verified
}
