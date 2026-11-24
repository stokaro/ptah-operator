package sourcebinding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/s3source"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// S3Revision is what a plan computed from an S3 source binds as its
// revision: "sha256:" and the SHA-256 of the endpoint's authority, the
// bucket, the key and the selected path, joined by newlines. It binds no
// object digest, no version and no region: an object written again, or read
// from another version, that leaves the selected files as they were keeps the
// plan, once the spec names its digest. The authority is taken as the spec
// writes it, as an HTTPS source's URL is.
func S3Revision(endpoint, bucket, key, path string) string {
	authority := strings.TrimPrefix(endpoint, "https://")
	sum := sha256.Sum256([]byte(authority + "\n" + bucket + "\n" + key + "\n" + path))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// S3PlanSource is what a plan computed from an S3 source binds: the
// endpoint, bucket, key and path, as S3Revision, and the content digest of
// the selected files.
func S3PlanSource(endpoint, bucket, key, path, contentDigest string) *operatorv1alpha1.PlanSourceBinding {
	return &operatorv1alpha1.PlanSourceBinding{
		Kind:          operatorv1alpha1.SourceKindS3,
		Revision:      S3Revision(endpoint, bucket, key, path),
		ContentDigest: contentDigest,
	}
}

// S3Access is what an operation fetches a resolved S3 source by: the object
// version and the content a resolution found, and the credential and trust
// selectors the spec names now.
func S3Access(spec operatorv1alpha1.S3SourceSpec, revision operatorv1alpha1.S3SourceRevision) *operatorv1alpha1.S3SourceBinding {
	binding := &operatorv1alpha1.S3SourceBinding{
		Endpoint: revision.Endpoint, Region: revision.Region, Bucket: revision.Bucket, Key: revision.Key,
		VersionID: revision.VersionID, Digest: revision.Digest, Format: revision.Format, Path: revision.Path,
		ContentDigest: revision.ContentDigest,
	}
	if spec.AuthFrom != nil {
		binding.AuthFrom = spec.AuthFrom.DeepCopy()
	}
	spec.Transport.DeepCopyInto(&binding.Transport)
	return binding
}

// S3Resolved reports whether a resolution answered the S3 source the spec
// names now: the same endpoint, region, bucket, key, digest, format and path,
// and the version the spec names, if it names one. A resolution of a spec
// that names no version recorded the one the endpoint served, and stays the
// answer until the spec changes.
func S3Resolved(spec *operatorv1alpha1.S3SourceSpec, revision *operatorv1alpha1.S3SourceRevision) bool {
	return spec != nil && revision != nil && revision.Endpoint == spec.Endpoint && revision.Region == spec.Region &&
		revision.Bucket == spec.Bucket && revision.Key == spec.Key && revision.Digest == spec.Digest &&
		revision.Format == spec.Format && revision.Path == spec.Path &&
		(spec.VersionID == "" || spec.VersionID == revision.VersionID)
}

// ValidateS3 holds an S3 access binding to what a fetch can act on: an
// object the API would admit, an object digest, a format, a clean path that
// fits it, a content digest, and selectors that name a Secret and a required
// ConfigMap key.
func ValidateS3(binding operatorv1alpha1.S3SourceBinding) error {
	object := s3source.Object{
		Endpoint: binding.Endpoint, Region: binding.Region, Bucket: binding.Bucket, Key: binding.Key, VersionID: binding.VersionID,
	}
	switch {
	case object.Validate() != nil:
		return errors.New("an S3 access binding names an object the API admits")
	case !contentDigestPattern.MatchString(binding.Digest):
		return errors.New("an S3 access binding names the object's digest")
	case !contentDigestPattern.MatchString(binding.ContentDigest):
		return errors.New("an S3 access binding names a content digest")
	case binding.AuthFrom != nil && strings.TrimSpace(binding.AuthFrom.Name) == "":
		return errors.New("an S3 access binding's credential selector names a Secret")
	}
	switch binding.Format {
	case operatorv1alpha1.HTTPSFormatFile:
		if binding.Path == "." || strings.Contains(binding.Path, "/") || sourcecontent.ValidatePath(binding.Path) != nil {
			return errors.New("an S3 access binding of a File names the one name the object is given")
		}
	case operatorv1alpha1.HTTPSFormatTarGzip, operatorv1alpha1.HTTPSFormatZip:
		if binding.Path != "." && sourcecontent.ValidatePath(binding.Path) != nil {
			return errors.New("an S3 access binding names a clean relative path")
		}
	default:
		return errors.New("an S3 access binding names File, TarGzip or Zip")
	}
	if selector := binding.Transport.CAFrom; selector != nil &&
		(selector.Name == "" || selector.Key == "" || (selector.Optional != nil && *selector.Optional)) {
		return errors.New("an S3 access binding's trust names a required ConfigMap key")
	}
	return nil
}
