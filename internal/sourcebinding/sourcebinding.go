// Package sourcebinding holds the comparisons every component makes about a
// resource's source: which plan contract a source gives, what a plan binds
// in place of an artifact digest, and whether an operation's access binding
// reads the content a plan names. The controllers, the Job builder and both
// admission webhooks ask these questions; answering them in one place keeps
// the answers from drifting apart.
package sourcebinding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
	corev1 "k8s.io/api/core/v1"
)

var (
	contentDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	gitCommitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// GitRevision is what a plan computed from a Git source binds as its
// revision: "sha256:" and the SHA-256 of the repository URL and the selected
// path, joined by a newline. Another repository or path is another source;
// another commit that leaves the selected files as they were is not.
func GitRevision(repository, path string) string {
	sum := sha256.Sum256([]byte(repository + "\n" + path))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GitPlanSource is what a plan computed from a Git source binds: the
// repository and path, as GitRevision, and the content digest of the
// selected files.
func GitPlanSource(repository, path, contentDigest string) *operatorv1alpha1.PlanSourceBinding {
	return &operatorv1alpha1.PlanSourceBinding{
		Kind:          operatorv1alpha1.SourceKindGit,
		Revision:      GitRevision(repository, path),
		ContentDigest: contentDigest,
	}
}

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
// ConfigMap or Git one. A source that has not been read yet binds nothing.
func SchemaPlanSource(status operatorv1alpha1.SchemaSourceStatus) (string, *operatorv1alpha1.PlanSourceBinding) {
	if git := status.Git; git != nil {
		return "", withPolicy(GitPlanSource(git.URL, git.Path, git.ContentDigest), status.SourcePolicy)
	}
	if status.ConfigMap != nil && status.Snapshot != nil {
		return "", withPolicy(ConfigMapPlanSource(*status.ConfigMap, *status.Snapshot), status.SourcePolicy)
	}
	return status.Digest, nil
}

// withPolicy is a plan source bound to the source policy it was checked
// against, if any.
func withPolicy(source *operatorv1alpha1.PlanSourceBinding, policy *operatorv1alpha1.SourcePolicyBinding) *operatorv1alpha1.PlanSourceBinding {
	if policy != nil {
		source.Policy = policy.DeepCopy()
	}
	return source
}

// MigrationPlanSource is what a plan computed from a migration's resolved
// source binds: the artifact digest of an OCI source, or the source binding
// of a ConfigMap or Git one.
func MigrationPlanSource(
	artifact *operatorv1alpha1.SourceAccessBinding,
	revision *operatorv1alpha1.ConfigMapSourceRevision,
	policy *operatorv1alpha1.SourcePolicyBinding,
) (string, *operatorv1alpha1.PlanSourceBinding) {
	if artifact == nil {
		return "", nil
	}
	if git := artifact.Git; git != nil {
		return "", withPolicy(GitPlanSource(git.URL, git.Path, git.ContentDigest), policy)
	}
	if artifact.Snapshot != nil {
		if revision == nil {
			return "", nil
		}
		return "", withPolicy(ConfigMapPlanSource(*revision, *artifact.Snapshot), policy)
	}
	return artifact.Digest, nil
}

// Fingerprint converts a plan's source binding to the form the fingerprint
// binds.
func Fingerprint(source *operatorv1alpha1.PlanSourceBinding) *fingerprint.PlanSource {
	if source == nil {
		return nil
	}
	bound := &fingerprint.PlanSource{Kind: string(source.Kind), Revision: source.Revision, ContentDigest: source.ContentDigest}
	if policy := source.Policy; policy != nil {
		bound.PolicyUID, bound.PolicyDigest = string(policy.UID), policy.Digest
	}
	return bound
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

// Equal reports whether two plan source bindings name the same source, and
// the same source policy.
func Equal(a, b *operatorv1alpha1.PlanSourceBinding) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.Revision == b.Revision && a.ContentDigest == b.ContentDigest && SamePolicy(a.Policy, b.Policy)
}

// SamePolicy reports whether two source policy bindings name the same
// policy, or are both absent.
func SamePolicy(a, b *operatorv1alpha1.SourcePolicyBinding) bool {
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

// ValidateAccess holds an access binding to one of its three shapes: an OCI
// reference pinned to a matching digest, a well-formed snapshot, or a Git
// commit, the last two with nothing that belongs to a registry.
func ValidateAccess(binding operatorv1alpha1.SourceAccessBinding) error {
	if binding.Git != nil {
		if binding.Snapshot != nil || binding.ResolvedReference != "" || binding.Digest != "" || binding.RegistryAuthFrom != nil ||
			binding.Transport.PlainHTTP || binding.Transport.CAFrom != nil {
			return errors.New("a Git access binding names no registry and no snapshot")
		}
		return ValidateGit(*binding.Git)
	}
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
	if git := binding.Git; git != nil {
		return source != nil && artifactDigest == "" && source.Kind == operatorv1alpha1.SourceKindGit &&
			git.ContentDigest == source.ContentDigest && GitRevision(git.URL, git.Path) == source.Revision
	}
	if binding.Snapshot != nil {
		return source != nil && artifactDigest == "" && binding.Snapshot.ContentDigest == source.ContentDigest
	}
	return source == nil && artifactDigest != "" && binding.Digest == artifactDigest
}

// SchemaAccess is the access binding a schema's resolved source gives later
// operations: the snapshot of a ConfigMap source, the resolved commit of a
// Git source with the spec's credential and trust selectors, or nil for an
// OCI source, whose binding the caller builds from the registry selectors.
func SchemaAccess(spec operatorv1alpha1.SourceSpec, status operatorv1alpha1.SchemaSourceStatus) *operatorv1alpha1.SourceAccessBinding {
	if status.Git != nil {
		if spec.Git == nil {
			return nil
		}
		return &operatorv1alpha1.SourceAccessBinding{Git: GitAccess(*spec.Git, *status.Git)}
	}
	if status.Snapshot == nil {
		return nil
	}
	return &operatorv1alpha1.SourceAccessBinding{Snapshot: status.Snapshot.DeepCopy()}
}

// GitAccess is what an operation fetches a resolved Git source by: the
// commit and content a resolution found, and the credential and trust
// selectors the spec names now.
func GitAccess(spec operatorv1alpha1.GitSourceSpec, revision operatorv1alpha1.GitSourceRevision) *operatorv1alpha1.GitSourceBinding {
	binding := &operatorv1alpha1.GitSourceBinding{
		URL: revision.URL, Ref: revision.Ref, Commit: revision.Commit, Path: revision.Path,
		ContentDigest: revision.ContentDigest,
	}
	if spec.AuthFrom != nil {
		binding.AuthFrom = spec.AuthFrom.DeepCopy()
	}
	spec.Transport.DeepCopyInto(&binding.Transport)
	return binding
}

// GitResolved reports whether a resolution answered the Git source the spec
// names now: the same repository, ref and path.
func GitResolved(spec *operatorv1alpha1.GitSourceSpec, revision *operatorv1alpha1.GitSourceRevision) bool {
	return spec != nil && revision != nil && revision.URL == spec.URL && revision.Ref == spec.Ref && revision.Path == spec.Path
}

// ValidateGit holds a Git access binding to what a fetch can act on: an
// https:// or ssh:// repository with no credential in it, one ref, a full
// commit id, a clean path, a content digest, and trust that fits the scheme.
func ValidateGit(git operatorv1alpha1.GitSourceBinding) error {
	parsed, err := url.Parse(git.URL)
	switch {
	case err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "":
		return errors.New("a Git access binding names an https:// or ssh:// repository")
	case parsed.Scheme == "https" && parsed.User != nil:
		return errors.New("a Git access binding's repository carries no credential")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return errors.New("a Git access binding's repository carries no credential")
	}
	refs := 0
	for _, set := range []bool{git.Ref.Branch != "", git.Ref.Tag != "", git.Ref.Commit != ""} {
		if set {
			refs++
		}
	}
	switch {
	case refs != 1:
		return errors.New("a Git access binding names exactly one of a branch, a tag and a commit")
	case !gitCommitPattern.MatchString(git.Commit) || (git.Ref.Commit != "" && git.Ref.Commit != git.Commit):
		return errors.New("a Git access binding names the full id of the commit its ref named")
	case !contentDigestPattern.MatchString(git.ContentDigest):
		return errors.New("a Git access binding names a content digest")
	case git.Path != "." && sourcecontent.ValidatePath(git.Path) != nil:
		return errors.New("a Git access binding names a clean relative path")
	case git.AuthFrom != nil && strings.TrimSpace(git.AuthFrom.Name) == "":
		return errors.New("a Git access binding's credential selector names a Secret")
	}
	ssh := parsed.Scheme == "ssh"
	switch {
	case ssh && git.Transport.KnownHostsFrom == nil:
		return errors.New("an ssh:// Git access binding names known hosts")
	case !ssh && git.Transport.KnownHostsFrom != nil, ssh && git.Transport.CAFrom != nil:
		return errors.New("a Git access binding's trust does not fit its scheme")
	}
	for _, selector := range []*corev1.ConfigMapKeySelector{git.Transport.CAFrom, git.Transport.KnownHostsFrom} {
		if selector != nil && (selector.Name == "" || selector.Key == "" || (selector.Optional != nil && *selector.Optional)) {
			return errors.New("a Git access binding's trust names a required ConfigMap key")
		}
	}
	return nil
}

// SchemaSourceReady reports whether later operations may read a schema's
// source: a verified OCI artifact, or the snapshot of the ConfigMap the spec
// names.
func SchemaSourceReady(spec operatorv1alpha1.SourceSpec, status operatorv1alpha1.SchemaSourceStatus) bool {
	if status.Git != nil {
		return GitResolved(spec.Git, status.Git)
	}
	if status.Snapshot != nil {
		return spec.ConfigMap != nil && status.ConfigMap != nil && status.ConfigMap.Name == spec.ConfigMap.Name
	}
	return status.Verified
}
