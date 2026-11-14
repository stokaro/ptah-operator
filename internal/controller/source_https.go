package controller

import (
	"errors"
	"fmt"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcepolicy"
)

var errHTTPSResolution = errors.New("the HTTPS resolution does not answer the requested source")

// httpsResolution is what a Resolve Job for an HTTPS source found, held to the
// source the spec names now: the download the spec pins and a content
// digest. The frame parser already refuses a download beside a commit or an
// OCI descriptor, so a Git or OCI answer reaches here without a download and
// is refused for that; a result for a spec that changed since the Job ran was
// discarded by the harvest before it reached here.
func httpsResolution(spec *operatorv1alpha1.HTTPSSourceSpec, result runner.Result) (operatorv1alpha1.HTTPSSourceRevision, error) {
	if result.ResolvedDownloadDigest != spec.Digest || !gitContentDigest.MatchString(result.ResolvedContentDigest) {
		return operatorv1alpha1.HTTPSSourceRevision{}, errHTTPSResolution
	}
	return operatorv1alpha1.HTTPSSourceRevision{
		URL: spec.URL, Format: spec.Format, Path: spec.Path,
		Digest: result.ResolvedDownloadDigest, ContentDigest: result.ResolvedContentDigest,
	}, nil
}

// httpsResolutionChanged reports whether a resolution names other content
// than the one before it: another URL, another path or other files. A
// download published again as other bytes that hold the same selected files
// keeps the plan, as a Git commit that leaves them alone does.
func httpsResolutionChanged(previous *operatorv1alpha1.HTTPSSourceRevision, next operatorv1alpha1.HTTPSSourceRevision) bool {
	return previous == nil || previous.URL != next.URL || previous.Path != next.Path || previous.ContentDigest != next.ContentDigest
}

// httpsResolvedMessage is the ArtifactResolved message of a resolved HTTPS
// source: both digests, since a person who replaced the archive reads here
// whether its selected files changed.
func httpsResolvedMessage(revision operatorv1alpha1.HTTPSSourceRevision) string {
	return fmt.Sprintf("The download %s matched its digest; the selected files give %s", revision.Digest, revision.ContentDigest)
}

// checkHTTPSSource refuses an HTTPS source its policy does not allow: its
// host and, once it is known, how much the selected files hold.
func checkHTTPSSource(policy *sourcepolicy.Policy, spec *operatorv1alpha1.HTTPSSourceSpec, contentBytes int64) error {
	if policy == nil {
		return nil
	}
	if err := policy.CheckHTTPSHost(sourceAuthority(spec.URL)); err != nil {
		return fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	if err := policy.CheckContentBytes(contentBytes); err != nil {
		return fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	return nil
}
