// Package sourceview reduces the source a PtahSchema or a PtahMigration last
// resolved to what `kubectl ptah` prints: which kind of source it is, where it
// was read from, and what it resolved to.
//
// Everything comes from the resource's status, which records no credential,
// so neither does a view. A Git repository URL cannot carry one: the API
// refuses a URL with user information other than an SSH user name, an HTTPS
// download URL with any user information at all, and an S3 endpoint with
// anything after its authority.
package sourceview

import (
	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Kind is the kind of source a resource resolved.
type Kind string

const (
	// KindOCI is an artifact in an OCI registry.
	KindOCI Kind = "OCI"
	// KindConfigMap is a ConfigMap in the resource's namespace.
	KindConfigMap Kind = "ConfigMap"
	// KindGit is a commit of a Git repository.
	KindGit Kind = "Git"
	// KindHTTPS is a download pinned by its digest.
	KindHTTPS Kind = "HTTPS"
	// KindS3 is an object of an S3-compatible bucket pinned by its digest.
	KindS3 Kind = "S3"
)

// View is one resolved source. The fields of the other kinds stay empty.
type View struct {
	Kind Kind `json:"kind"`

	// Reference is an OCI artifact with its tag replaced by the digest it
	// resolved to, and Digest that digest on its own.
	Reference string `json:"reference,omitempty"`
	Digest    string `json:"digest,omitempty"`
	// Verified says an OCI artifact's verification policy accepted it.
	Verified bool `json:"verified,omitempty"`

	// ConfigMap is the ConfigMap the files were read from, and Snapshot the
	// immutable copy every operation reads.
	ConfigMap string `json:"configMap,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`

	// Repository, Ref and Path are what a Git source asked for, and Commit
	// what the ref named when it resolved.
	Repository string                   `json:"repository,omitempty"`
	Ref        *operatorv1alpha1.GitRef `json:"ref,omitempty"`
	Path       string                   `json:"path,omitempty"`
	Commit     string                   `json:"commit,omitempty"`

	// URL, Format and DownloadDigest are the download an HTTPS source names
	// and the digest its bytes were held to; Path is what it selects. An S3
	// source has a Format, a Path and a DownloadDigest too, the digest of
	// the object's bytes.
	URL            string `json:"url,omitempty"`
	Format         string `json:"format,omitempty"`
	DownloadDigest string `json:"downloadDigest,omitempty"`

	// Endpoint, Region, Bucket and Key are the object an S3 source names,
	// and VersionID the version every operation reads, when the bucket keeps
	// versions.
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Key       string `json:"key,omitempty"`
	VersionID string `json:"versionId,omitempty"`

	// ContentDigest is the digest of a ConfigMap, Git, HTTPS or S3 source's
	// selected files, the content a plan computed from them is bound to.
	ContentDigest string `json:"contentDigest,omitempty"`
	// SourcePolicyDigest is the content of the source policy the resolution
	// was checked against, when the resource names one.
	SourcePolicyDigest string `json:"sourcePolicyDigest,omitempty"`
}

// Field is one line of the text a view prints.
type Field struct {
	Name, Value string
}

// FromSchema is the source a schema last resolved, or nil before the first
// resolution.
func FromSchema(status operatorv1alpha1.SchemaSourceStatus) *View {
	var view *View
	switch {
	case status.Git != nil:
		view = gitView(status.Git.URL, status.Git.Ref, status.Git.Path, status.Git.Commit, status.Git.ContentDigest)
	case status.HTTPS != nil:
		view = httpsView(status.HTTPS.URL, status.HTTPS.Format, status.HTTPS.Path, status.HTTPS.Digest, status.HTTPS.ContentDigest)
	case status.S3 != nil:
		object := status.S3
		view = s3View(object.Endpoint, object.Region, object.Bucket, object.Key, object.VersionID,
			object.Format, object.Path, object.Digest, object.ContentDigest)
	case status.Snapshot != nil || status.ConfigMap != nil:
		view = configMapView(status.ConfigMap, status.Snapshot)
	case status.ResolvedReference != "" || status.Digest != "":
		return &View{Kind: KindOCI, Reference: status.ResolvedReference, Digest: status.Digest, Verified: status.Verified}
	default:
		return nil
	}
	view.SourcePolicyDigest = policyDigest(status.SourcePolicy)
	return view
}

// FromMigration is the source a migration last resolved, or nil before the
// first resolution. A migration's verification is a condition rather than a
// status field, so an OCI view says nothing about it.
func FromMigration(
	artifact *operatorv1alpha1.SourceAccessBinding,
	configMap *operatorv1alpha1.ConfigMapSourceRevision,
	policy *operatorv1alpha1.SourcePolicyBinding,
) *View {
	if artifact == nil {
		return nil
	}
	var view *View
	switch {
	case artifact.Git != nil:
		view = gitView(artifact.Git.URL, artifact.Git.Ref, artifact.Git.Path, artifact.Git.Commit, artifact.Git.ContentDigest)
	case artifact.HTTPS != nil:
		view = httpsView(artifact.HTTPS.URL, artifact.HTTPS.Format, artifact.HTTPS.Path, artifact.HTTPS.Digest, artifact.HTTPS.ContentDigest)
	case artifact.S3 != nil:
		object := artifact.S3
		view = s3View(object.Endpoint, object.Region, object.Bucket, object.Key, object.VersionID,
			object.Format, object.Path, object.Digest, object.ContentDigest)
	case artifact.Snapshot != nil:
		view = configMapView(configMap, artifact.Snapshot)
	default:
		return &View{Kind: KindOCI, Reference: artifact.ResolvedReference, Digest: artifact.Digest}
	}
	view.SourcePolicyDigest = policyDigest(policy)
	return view
}

func gitView(url string, ref operatorv1alpha1.GitRef, path, commit, contentDigest string) *View {
	return &View{Kind: KindGit, Repository: url, Ref: &ref, Path: path, Commit: commit, ContentDigest: contentDigest}
}

func httpsView(url string, format operatorv1alpha1.HTTPSFormat, path, digest, contentDigest string) *View {
	return &View{Kind: KindHTTPS, URL: url, Format: string(format), Path: path, DownloadDigest: digest, ContentDigest: contentDigest}
}

func s3View(endpoint, region, bucket, key, versionID string, format operatorv1alpha1.HTTPSFormat, path, digest, contentDigest string) *View {
	return &View{
		Kind: KindS3, Endpoint: endpoint, Region: region, Bucket: bucket, Key: key, VersionID: versionID,
		Format: string(format), Path: path, DownloadDigest: digest, ContentDigest: contentDigest,
	}
}

func configMapView(revision *operatorv1alpha1.ConfigMapSourceRevision, snapshot *operatorv1alpha1.SourceSnapshotBinding) *View {
	view := &View{Kind: KindConfigMap}
	if revision != nil {
		view.ConfigMap = revision.Name
	}
	if snapshot != nil {
		view.Snapshot, view.ContentDigest = snapshot.Name, snapshot.ContentDigest
	}
	return view
}

func policyDigest(policy *operatorv1alpha1.SourcePolicyBinding) string {
	if policy == nil {
		return ""
	}
	return policy.Digest
}

// Fields are the lines a view prints, in order. An OCI source prints what it
// always printed: the artifact, and whether it was verified.
func (v *View) Fields() []Field {
	if v == nil {
		return nil
	}
	var fields []Field
	switch v.Kind {
	case KindOCI:
		fields = append(fields, Field{"Artifact", v.Reference})
		if v.Verified {
			fields = append(fields, Field{"Verified", "true"})
		}
		return fields
	case KindConfigMap:
		fields = append(fields,
			Field{"Source", joinNonEmpty("ConfigMap", v.ConfigMap)},
			Field{"Snapshot", v.Snapshot})
	case KindGit:
		fields = append(fields,
			Field{"Source", joinNonEmpty("Git", v.Repository)},
			Field{"Ref", refText(v.Ref)},
			Field{"Path", v.Path},
			Field{"Commit", v.Commit})
	case KindHTTPS:
		fields = append(fields,
			Field{"Source", joinNonEmpty("HTTPS", v.URL)},
			Field{"Format", v.Format},
			Field{"Path", v.Path},
			Field{"Download", v.DownloadDigest})
	case KindS3:
		fields = append(fields,
			Field{"Source", "S3 s3://" + v.Bucket + "/" + v.Key},
			Field{"Endpoint", v.Endpoint},
			Field{"Region", v.Region},
			Field{"Version", v.VersionID},
			Field{"Format", v.Format},
			Field{"Path", v.Path},
			Field{"Object", v.DownloadDigest})
	}
	return append(fields,
		Field{"Content", v.ContentDigest},
		Field{"Source policy", v.SourcePolicyDigest})
}

func refText(ref *operatorv1alpha1.GitRef) string {
	switch {
	case ref == nil:
		return ""
	case ref.Branch != "":
		return "branch " + ref.Branch
	case ref.Tag != "":
		return "tag " + ref.Tag
	case ref.Commit != "":
		return "commit " + ref.Commit
	default:
		return ""
	}
}

func joinNonEmpty(kind, name string) string {
	if name == "" {
		return kind
	}
	return kind + " " + name
}
