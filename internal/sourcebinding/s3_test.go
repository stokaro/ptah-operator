package sourcebinding_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

const (
	s3Endpoint = "https://objects.example.com:9000"
	s3Bucket   = "app-releases"
	s3Key      = "app/1.4.0/app.tar.gz"
	s3Path     = "app/db/migrations"
	s3Object   = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	s3Version  = "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"
)

func s3Binding() *operatorv1alpha1.S3SourceBinding {
	return &operatorv1alpha1.S3SourceBinding{
		Endpoint: s3Endpoint, Region: "us-east-1", Bucket: s3Bucket, Key: s3Key, VersionID: s3Version,
		Digest: s3Object, Format: operatorv1alpha1.HTTPSFormatTarGzip, Path: s3Path, ContentDigest: content,
	}
}

// The revision is fixed by the endpoint's authority, the bucket, the key and
// the path, and never by the object digest, the version or the region, as the
// API documents it.
func TestS3RevisionNamesTheObjectAndThePath(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("objects.example.com:9000\n" + s3Bucket + "\n" + s3Key + "\n" + s3Path))
	want := "sha256:" + hex.EncodeToString(sum[:])
	got := sourcebinding.S3Revision(s3Endpoint, s3Bucket, s3Key, s3Path)
	if got != want {
		t.Fatalf("S3Revision() = %q, want %q", got, want)
	}
	for name, other := range map[string]string{
		"another port":      sourcebinding.S3Revision("https://objects.example.com", s3Bucket, s3Key, s3Path),
		"another bucket":    sourcebinding.S3Revision(s3Endpoint, "app-schemas", s3Key, s3Path),
		"another key":       sourcebinding.S3Revision(s3Endpoint, s3Bucket, "app/1.5.0/app.tar.gz", s3Path),
		"another path":      sourcebinding.S3Revision(s3Endpoint, s3Bucket, s3Key, "app/db"),
		"the join moved":    sourcebinding.S3Revision(s3Endpoint, s3Bucket+"\napp", "1.4.0/app.tar.gz", s3Path),
		"an HTTPS revision": sourcebinding.HTTPSRevision("https://objects.example.com:9000/"+s3Bucket+"/"+s3Key, s3Path),
	} {
		if other == got {
			t.Errorf("%s gives the same revision", name)
		}
	}
}

func TestValidateAccessHoldsAnS3Object(t *testing.T) {
	t.Parallel()
	ca := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ca"}, Key: "ca.pem"}
	for name, edit := range map[string]func(*operatorv1alpha1.S3SourceBinding){
		"an archive": func(*operatorv1alpha1.S3SourceBinding) {},
		"no version": func(b *operatorv1alpha1.S3SourceBinding) { b.VersionID = "" },
		// A spec may name the null version itself, and the binding keeps it.
		"the null version": func(b *operatorv1alpha1.S3SourceBinding) { b.VersionID = "null" },
		"a zip's root":     func(b *operatorv1alpha1.S3SourceBinding) { b.Format, b.Path = operatorv1alpha1.HTTPSFormatZip, "." },
		"a File": func(b *operatorv1alpha1.S3SourceBinding) {
			b.Format, b.Path = operatorv1alpha1.HTTPSFormatFile, "schema.hcl"
		},
		"a default port":    func(b *operatorv1alpha1.S3SourceBinding) { b.Endpoint = "https://objects.example.com" },
		"an IPv4 endpoint":  func(b *operatorv1alpha1.S3SourceBinding) { b.Endpoint = "https://10.0.0.7:9000" },
		"a dotted bucket":   func(b *operatorv1alpha1.S3SourceBinding) { b.Bucket = "app.releases" },
		"a key with spaces": func(b *operatorv1alpha1.S3SourceBinding) { b.Key = "app/1.4.0/app release.tar.gz" },
		"a credential and a CA": func(b *operatorv1alpha1.S3SourceBinding) {
			b.AuthFrom, b.Transport.CAFrom = &operatorv1alpha1.S3AuthSource{Name: "keys"}, ca
		},
	} {
		binding := s3Binding()
		edit(binding)
		if err := sourcebinding.ValidateAccess(operatorv1alpha1.SourceAccessBinding{S3: binding}); err != nil {
			t.Errorf("ValidateAccess refused %s: %v", name, err)
		}
	}
	for name, edit := range map[string]func(*operatorv1alpha1.SourceAccessBinding){
		"an object and a snapshot":  func(b *operatorv1alpha1.SourceAccessBinding) { b.Snapshot = snapshot() },
		"an object and a commit":    func(b *operatorv1alpha1.SourceAccessBinding) { b.Git = gitBinding() },
		"an object and a download":  func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS = httpsBinding() },
		"an object and a digest":    func(b *operatorv1alpha1.SourceAccessBinding) { b.Digest = artifact },
		"an object and registry CA": func(b *operatorv1alpha1.SourceAccessBinding) { b.Transport.CAFrom = ca },
		"http://":                   func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Endpoint = "http://objects.example.com" },
		"a path in the endpoint": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Endpoint = "https://objects.example.com/" + s3Bucket
		},
		"a user in the endpoint":   func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Endpoint = "https://u@objects.example.com" },
		"a region in capitals":     func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Region = "US-EAST-1" },
		"no region":                func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Region = "" },
		"a bucket in capitals":     func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Bucket = "App" },
		"a short bucket":           func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Bucket = "ab" },
		"no key":                   func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Key = "" },
		"a key from the root":      func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Key = "/app.tar.gz" },
		"a key that climbs":        func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Key = "app/../app.tar.gz" },
		"a key with an empty part": func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Key = "app//app.tar.gz" },
		"a version with a space":   func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.VersionID = "a version" },
		"no object digest":         func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Digest = "" },
		"no content digest":        func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.ContentDigest = "sha256:11" },
		"a format the API lacks":   func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Format = "Tar" },
		"a File with a nested path": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Format = operatorv1alpha1.HTTPSFormatFile
		},
		"a File naming the root": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Format, b.S3.Path = operatorv1alpha1.HTTPSFormatFile, "."
		},
		"an escaping path": func(b *operatorv1alpha1.SourceAccessBinding) { b.S3.Path = "../db" },
		"an unnamed credential": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.AuthFrom = &operatorv1alpha1.S3AuthSource{Name: " "}
		},
		"a CA without a key": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Transport.CAFrom = &corev1.ConfigMapKeySelector{LocalObjectReference: ca.LocalObjectReference}
		},
		"an optional CA": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Transport.CAFrom = &corev1.ConfigMapKeySelector{LocalObjectReference: ca.LocalObjectReference, Key: "ca.pem", Optional: new(true)}
		},
		"an uppercase object digest": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.S3.Digest = "sha256:5555555555555555555555555555555555555555555555555555555555555AAA"
		},
	} {
		binding := operatorv1alpha1.SourceAccessBinding{S3: s3Binding()}
		edit(&binding)
		if err := sourcebinding.ValidateAccess(binding); err == nil {
			t.Errorf("ValidateAccess accepted %s", name)
		}
	}
}

func TestAnS3PlanSourceBindsTheObjectPathAndContent(t *testing.T) {
	t.Parallel()
	binding := operatorv1alpha1.SourceAccessBinding{S3: s3Binding()}
	source := sourcebinding.S3PlanSource(s3Endpoint, s3Bucket, s3Key, s3Path, content)
	if source.Kind != operatorv1alpha1.SourceKindS3 || source.ContentDigest != content ||
		sourcebinding.ContractVersion(source) != fingerprint.SourcePlanContractVersion {
		t.Fatalf("S3PlanSource() = %#v", source)
	}
	if !sourcebinding.AccessGives(binding, "", source) {
		t.Fatal("an S3 binding does not give the plan computed from it")
	}
	// An object written again, read from another version or another region,
	// that leaves the selected files as they were keeps the plan.
	for name, edit := range map[string]func(*operatorv1alpha1.S3SourceBinding){
		"another object digest": func(b *operatorv1alpha1.S3SourceBinding) {
			b.Digest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
		},
		"another version":    func(b *operatorv1alpha1.S3SourceBinding) { b.VersionID = "v2" },
		"no version":         func(b *operatorv1alpha1.S3SourceBinding) { b.VersionID = "" },
		"another region":     func(b *operatorv1alpha1.S3SourceBinding) { b.Region = "eu-west-1" },
		"another credential": func(b *operatorv1alpha1.S3SourceBinding) { b.AuthFrom = &operatorv1alpha1.S3AuthSource{Name: "other"} },
	} {
		moved := operatorv1alpha1.SourceAccessBinding{S3: s3Binding()}
		edit(moved.S3)
		if !sourcebinding.AccessGives(moved, "", source) {
			t.Errorf("a binding with %s does not give the plan", name)
		}
	}
	for name, plan := range map[string]*operatorv1alpha1.PlanSourceBinding{
		"other content":  sourcebinding.S3PlanSource(s3Endpoint, s3Bucket, s3Key, s3Path, artifact),
		"another path":   sourcebinding.S3PlanSource(s3Endpoint, s3Bucket, s3Key, "app/db", content),
		"another key":    sourcebinding.S3PlanSource(s3Endpoint, s3Bucket, "app/1.5.0/app.tar.gz", s3Path, content),
		"another bucket": sourcebinding.S3PlanSource(s3Endpoint, "app-schemas", s3Key, s3Path, content),
		"another port":   sourcebinding.S3PlanSource("https://objects.example.com", s3Bucket, s3Key, s3Path, content),
		"an HTTPS plan":  {Kind: operatorv1alpha1.SourceKindHTTPS, Revision: source.Revision, ContentDigest: content},
		"no source":      nil,
	} {
		if sourcebinding.AccessGives(binding, "", plan) {
			t.Errorf("an S3 binding gives a plan with %s", name)
		}
	}
	if sourcebinding.AccessGives(binding, artifact, source) {
		t.Error("an S3 binding gives a plan that also names an artifact")
	}
	if sourcebinding.AccessGives(operatorv1alpha1.SourceAccessBinding{HTTPS: &operatorv1alpha1.HTTPSSourceBinding{
		URL: s3Endpoint + "/" + s3Bucket + "/" + s3Key, Path: s3Path, ContentDigest: content,
	}}, "", source) {
		t.Error("an HTTPS binding gives an S3 plan of the same object, path and content")
	}

	revision := &operatorv1alpha1.S3SourceRevision{
		Endpoint: s3Endpoint, Region: "us-east-1", Bucket: s3Bucket, Key: s3Key, VersionID: s3Version,
		Format: binding.S3.Format, Path: s3Path, Digest: s3Object, ContentDigest: content,
	}
	status := operatorv1alpha1.SchemaSourceStatus{S3: revision}
	if digest, plan := sourcebinding.SchemaPlanSource(status); digest != "" || !sourcebinding.Equal(plan, source) {
		t.Fatalf("SchemaPlanSource() = %q %#v, want the S3 source", digest, plan)
	}
	if digest, plan := sourcebinding.MigrationPlanSource(&binding, nil, nil); digest != "" || !sourcebinding.Equal(plan, source) {
		t.Fatalf("MigrationPlanSource() = %q %#v, want the S3 source", digest, plan)
	}
	ca := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ca"}, Key: "ca.pem"}
	spec := operatorv1alpha1.SourceSpec{S3: &operatorv1alpha1.S3SourceSpec{
		Endpoint: revision.Endpoint, Region: revision.Region, Bucket: revision.Bucket, Key: revision.Key,
		Digest: revision.Digest, Format: revision.Format, Path: revision.Path,
		AuthFrom: &operatorv1alpha1.S3AuthSource{Name: "keys"}, Transport: operatorv1alpha1.S3TransportSpec{CAFrom: ca},
	}}
	if !sourcebinding.SchemaSourceReady(spec, status) {
		t.Fatal("a resolved S3 source is not ready")
	}
	named := operatorv1alpha1.SourceSpec{S3: spec.S3.DeepCopy()}
	named.S3.VersionID = s3Version
	if !sourcebinding.SchemaSourceReady(named, status) {
		t.Fatal("a resolution of the version the spec names is not ready")
	}
	access := sourcebinding.SchemaAccess(spec, status)
	if access == nil || access.S3 == nil || access.S3.Digest != s3Object || access.S3.ContentDigest != content ||
		access.S3.VersionID != s3Version || access.S3.AuthFrom == nil || access.S3.AuthFrom.Name != "keys" ||
		access.S3.Transport.CAFrom == nil || access.S3.Transport.CAFrom.Name != "ca" || access.HTTPS != nil || access.Git != nil {
		t.Fatalf("SchemaAccess() = %#v, want the resolved version with the spec's credential and CA", access)
	}
	if err := sourcebinding.ValidateAccess(*access); err != nil {
		t.Fatalf("the access SchemaAccess gives is refused: %v", err)
	}
	// The binding carries the spec's selectors, not shared pointers to them.
	spec.S3.AuthFrom.Name = "edited"
	if access.S3.AuthFrom.Name != "keys" {
		t.Fatal("SchemaAccess() shares the spec's credential selector")
	}
	spec.S3.AuthFrom.Name = "keys"
	for name, edit := range map[string]func(*operatorv1alpha1.S3SourceSpec){
		"another endpoint": func(s *operatorv1alpha1.S3SourceSpec) { s.Endpoint = "https://objects.example.com" },
		"another region":   func(s *operatorv1alpha1.S3SourceSpec) { s.Region = "eu-west-1" },
		"another bucket":   func(s *operatorv1alpha1.S3SourceSpec) { s.Bucket = "app-schemas" },
		"another key":      func(s *operatorv1alpha1.S3SourceSpec) { s.Key = "app/1.5.0/app.tar.gz" },
		"another version":  func(s *operatorv1alpha1.S3SourceSpec) { s.VersionID = "v2" },
		"another digest":   func(s *operatorv1alpha1.S3SourceSpec) { s.Digest = artifact },
		"another format":   func(s *operatorv1alpha1.S3SourceSpec) { s.Format = operatorv1alpha1.HTTPSFormatZip },
		"another path":     func(s *operatorv1alpha1.S3SourceSpec) { s.Path = "app/db" },
	} {
		moved := operatorv1alpha1.SourceSpec{S3: spec.S3.DeepCopy()}
		edit(moved.S3)
		if sourcebinding.SchemaSourceReady(moved, status) {
			t.Errorf("a resolution is ready for a spec naming %s", name)
		}
	}
	for name, other := range map[string]operatorv1alpha1.SourceSpec{
		"an OCI source":   {OCIRef: "oci://registry.example/schema:1"},
		"an HTTPS source": {HTTPS: &operatorv1alpha1.HTTPSSourceSpec{URL: s3Endpoint + "/" + s3Bucket + "/" + s3Key, Digest: s3Object, Path: s3Path}},
	} {
		if sourcebinding.SchemaSourceReady(other, status) || sourcebinding.SchemaAccess(other, status) != nil {
			t.Errorf("an S3 resolution answers a spec that names %s", name)
		}
	}
}
