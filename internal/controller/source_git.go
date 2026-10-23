package controller

import (
	"errors"
	"regexp"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

var (
	gitCommitID       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	gitContentDigest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	errGitResolution  = errors.New("the Git resolution does not answer the requested source")
	errGitRefIsCommit = errors.New("the Git resolution read another commit than the ref names")
)

// gitResolution is what a Resolve Job for a Git source found, held to the
// source the spec names now: a full commit and a content digest, and, for a
// ref that is a commit, that commit. The frame parser already refuses a
// result that carries both Git and OCI evidence, so an OCI descriptor reaches
// here only without a commit. The Job ran for the spec its inputs name, and a
// spec edited since then changed those inputs, so the harvest discarded the
// result before it reached here.
func gitResolution(spec *operatorv1alpha1.GitSourceSpec, result runner.Result) (operatorv1alpha1.GitSourceRevision, error) {
	if !gitCommitID.MatchString(result.ResolvedCommit) || !gitContentDigest.MatchString(result.ResolvedContentDigest) {
		return operatorv1alpha1.GitSourceRevision{}, errGitResolution
	}
	if spec.Ref.Commit != "" && spec.Ref.Commit != result.ResolvedCommit {
		return operatorv1alpha1.GitSourceRevision{}, errGitRefIsCommit
	}
	return operatorv1alpha1.GitSourceRevision{
		URL: spec.URL, Ref: spec.Ref, Path: spec.Path,
		Commit: result.ResolvedCommit, ContentDigest: result.ResolvedContentDigest,
	}, nil
}

// gitResolutionChanged reports whether a resolution names other content than
// the one before it: another repository, another path or other files. A ref
// that moved to a commit holding the same selected files keeps the plan, as an
// edit to an unselected key keeps a ConfigMap source's.
func gitResolutionChanged(previous *operatorv1alpha1.GitSourceRevision, next operatorv1alpha1.GitSourceRevision) bool {
	return previous == nil || previous.URL != next.URL || previous.Path != next.Path || previous.ContentDigest != next.ContentDigest
}

// proofReadsTheCurrentSource reports whether the proof after an Apply reads
// the source the resource resolves to now: the same snapshot of a ConfigMap
// source, or the same repository, path and content of a Git source.
func proofReadsTheCurrentSource(status operatorv1alpha1.SchemaSourceStatus, pending operatorv1alpha1.SourceAccessBinding) bool {
	switch {
	case pending.Snapshot != nil:
		return status.Snapshot != nil && status.Snapshot.Name == pending.Snapshot.Name
	case pending.Git != nil:
		return status.Git != nil && status.Git.URL == pending.Git.URL && status.Git.Path == pending.Git.Path &&
			status.Git.ContentDigest == pending.Git.ContentDigest
	default:
		return false
	}
}
