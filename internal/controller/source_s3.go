package controller

import (
	"errors"
	"fmt"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcepolicy"
)

var errS3Resolution = errors.New("the S3 resolution does not answer the requested source")

// s3Resolution is what a Resolve Job for an S3 source found, held to the
// source the spec names now: the object digest the spec pins, a content
// digest and, where the spec names a version, that version. The version is
// the one the spec named or the one the endpoint served, and every later
// operation reads that version. The frame parser already refuses a download
// beside a commit or an OCI descriptor, and a version without a download; a
// result for a spec that changed since the Job ran was discarded by the
// harvest before it reached here.
func s3Resolution(spec *operatorv1alpha1.S3SourceSpec, result runner.Result) (operatorv1alpha1.S3SourceRevision, error) {
	if result.ResolvedDownloadDigest != spec.Digest || !gitContentDigest.MatchString(result.ResolvedContentDigest) ||
		(spec.VersionID != "" && result.ResolvedVersionID != spec.VersionID) {
		return operatorv1alpha1.S3SourceRevision{}, errS3Resolution
	}
	return operatorv1alpha1.S3SourceRevision{
		Endpoint: spec.Endpoint, Region: spec.Region, Bucket: spec.Bucket, Key: spec.Key,
		VersionID: result.ResolvedVersionID, Format: spec.Format, Path: spec.Path,
		Digest: result.ResolvedDownloadDigest, ContentDigest: result.ResolvedContentDigest,
	}, nil
}

// s3ResolutionChanged reports whether a resolution names other content than
// the one before it: another endpoint, bucket, key or path, or other files.
// An object written again as other bytes, or read from another version, that
// holds the same selected files keeps the plan, as an HTTPS download
// published again does.
func s3ResolutionChanged(previous *operatorv1alpha1.S3SourceRevision, next operatorv1alpha1.S3SourceRevision) bool {
	return previous == nil || previous.Endpoint != next.Endpoint || previous.Bucket != next.Bucket ||
		previous.Key != next.Key || previous.Path != next.Path || previous.ContentDigest != next.ContentDigest
}

// s3ResolvedMessage is the ArtifactResolved message of a resolved S3
// source: both digests and the version every later operation reads, since a
// person who wrote the object again reads here whether its selected files
// changed, and which version the operator holds on to.
func s3ResolvedMessage(revision operatorv1alpha1.S3SourceRevision) string {
	version := "the bucket keeps no version of it, so a later write replaces what every operation reads"
	if revision.VersionID != "" {
		version = "every operation reads version " + revision.VersionID
	}
	return fmt.Sprintf("The object %s matched its digest; the selected files give %s; %s",
		revision.Digest, revision.ContentDigest, version)
}

// s3Authority is the host[:port] an S3 source's endpoint names, as the spec
// writes it: the endpoint is https:// and an authority with nothing after
// it, which the schema holds.
func s3Authority(endpoint string) string {
	return strings.TrimPrefix(endpoint, "https://")
}

// checkS3Source refuses an S3 source its policy does not allow: its
// endpoint and bucket and, once it is known, how much the selected files
// hold.
func checkS3Source(policy *sourcepolicy.Policy, spec *operatorv1alpha1.S3SourceSpec, contentBytes int64) error {
	if policy == nil {
		return nil
	}
	if err := policy.CheckS3Bucket(s3Authority(spec.Endpoint), spec.Bucket); err != nil {
		return fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	if err := policy.CheckContentBytes(contentBytes); err != nil {
		return fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	return nil
}
