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
	content  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	artifact = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func snapshot() *operatorv1alpha1.SourceSnapshotBinding {
	return &operatorv1alpha1.SourceSnapshotBinding{
		Name: "ptah-src-0123456789abcdef01234567", UID: "snapshot-uid", ContentDigest: content, SchemaFile: "schema.hcl",
	}
}

func oci() operatorv1alpha1.SourceAccessBinding {
	return operatorv1alpha1.SourceAccessBinding{ResolvedReference: "oci://registry.example/schema@" + artifact, Digest: artifact}
}

func TestValidateAccessHoldsEachShapeToItself(t *testing.T) {
	t.Parallel()
	if err := sourcebinding.ValidateAccess(oci()); err != nil {
		t.Fatalf("pinned OCI binding refused: %v", err)
	}
	if err := sourcebinding.ValidateAccess(operatorv1alpha1.SourceAccessBinding{Snapshot: snapshot()}); err != nil {
		t.Fatalf("snapshot binding refused: %v", err)
	}
	for name, binding := range map[string]operatorv1alpha1.SourceAccessBinding{
		"an unpinned reference":      {ResolvedReference: "oci://registry.example/schema:latest", Digest: artifact},
		"nothing":                    {},
		"a snapshot and a reference": {ResolvedReference: oci().ResolvedReference, Digest: artifact, Snapshot: snapshot()},
		"a snapshot and credentials": {Snapshot: snapshot(), RegistryAuthFrom: &operatorv1alpha1.RegistryAuthSource{Name: "creds"}},
		"a snapshot over plain HTTP": {Snapshot: snapshot(), Transport: operatorv1alpha1.OCITransportSpec{PlainHTTP: true}},
		"a snapshot with a CA": {Snapshot: snapshot(), Transport: operatorv1alpha1.OCITransportSpec{
			CAFrom: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ca"}, Key: "ca.pem"},
		}},
		"another ConfigMap name": {Snapshot: &operatorv1alpha1.SourceSnapshotBinding{Name: "storefront", UID: "u", ContentDigest: content}},
		"no snapshot UID":        {Snapshot: &operatorv1alpha1.SourceSnapshotBinding{Name: snapshot().Name, ContentDigest: content}},
		"a short digest":         {Snapshot: &operatorv1alpha1.SourceSnapshotBinding{Name: snapshot().Name, UID: "u", ContentDigest: "sha256:11"}},
		"an escaping schema file": {Snapshot: &operatorv1alpha1.SourceSnapshotBinding{
			Name: snapshot().Name, UID: "u", ContentDigest: content, SchemaFile: "../schema.hcl",
		}},
	} {
		if err := sourcebinding.ValidateAccess(binding); err == nil {
			t.Errorf("ValidateAccess accepted %s", name)
		}
	}
}

func TestAccessGivesOnlyTheContentThePlanBinds(t *testing.T) {
	t.Parallel()
	source := &operatorv1alpha1.PlanSourceBinding{Kind: operatorv1alpha1.SourceKindConfigMap, Revision: "configmap-uid", ContentDigest: content}
	snapshotAccess := operatorv1alpha1.SourceAccessBinding{Snapshot: snapshot()}
	if !sourcebinding.AccessGives(oci(), artifact, nil) || !sourcebinding.AccessGives(snapshotAccess, "", source) {
		t.Fatal("an access binding does not give the content its own plan binds")
	}
	other := *source
	other.ContentDigest = artifact
	for name, gives := range map[string]bool{
		"OCI access for a source plan":       sourcebinding.AccessGives(oci(), "", source),
		"snapshot access for an OCI plan":    sourcebinding.AccessGives(snapshotAccess, artifact, nil),
		"snapshot access for other files":    sourcebinding.AccessGives(snapshotAccess, "", &other),
		"OCI access for another artifact":    sourcebinding.AccessGives(oci(), content, nil),
		"snapshot access for a mixed plan":   sourcebinding.AccessGives(snapshotAccess, artifact, source),
		"OCI access for a plan with nothing": sourcebinding.AccessGives(oci(), "", nil),
	} {
		if gives {
			t.Errorf("AccessGives accepted %s", name)
		}
	}
}

func TestPlanSourcesFollowTheResolvedSource(t *testing.T) {
	t.Parallel()
	revision := operatorv1alpha1.ConfigMapSourceRevision{Name: "storefront-schema", UID: "configmap-uid", ResourceVersion: "7"}
	digest, source := sourcebinding.SchemaPlanSource(operatorv1alpha1.SchemaSourceStatus{ConfigMap: &revision, Snapshot: snapshot()})
	if digest != "" || source == nil || source.Revision != "configmap-uid" || source.ContentDigest != content ||
		sourcebinding.ContractVersion(source) != fingerprint.SourcePlanContractVersion {
		t.Fatalf("SchemaPlanSource(ConfigMap) = %q, %#v", digest, source)
	}
	digest, source = sourcebinding.SchemaPlanSource(operatorv1alpha1.SchemaSourceStatus{Digest: artifact})
	if digest != artifact || source != nil || sourcebinding.ContractVersion(source) != fingerprint.CurrentPlanContractVersion {
		t.Fatalf("SchemaPlanSource(OCI) = %q, %#v", digest, source)
	}
	// The resourceVersion is not part of what a plan binds: an edit to a key
	// the source does not select keeps the plan.
	moved := revision
	moved.ResourceVersion = "8"
	if !sourcebinding.Equal(sourcebinding.ConfigMapPlanSource(revision, *snapshot()), sourcebinding.ConfigMapPlanSource(moved, *snapshot())) {
		t.Fatal("a new resourceVersion of the same ConfigMap gave another plan source")
	}
	recreated := revision
	recreated.UID = "another-configmap-uid"
	if sourcebinding.Equal(sourcebinding.ConfigMapPlanSource(revision, *snapshot()), sourcebinding.ConfigMapPlanSource(recreated, *snapshot())) {
		t.Fatal("another ConfigMap with the same files gave the same plan source")
	}
	access := &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshot()}
	if digest, source := sourcebinding.MigrationPlanSource(access, &revision, nil); digest != "" || source == nil {
		t.Fatalf("MigrationPlanSource(ConfigMap) = %q, %#v", digest, source)
	}
	if digest, source := sourcebinding.MigrationPlanSource(access, nil, nil); digest != "" || source != nil {
		t.Fatalf("MigrationPlanSource(snapshot without its ConfigMap) = %q, %#v, want nothing", digest, source)
	}
	ociAccess := oci()
	if digest, source := sourcebinding.MigrationPlanSource(&ociAccess, nil, nil); digest != artifact || source != nil {
		t.Fatalf("MigrationPlanSource(OCI) = %q, %#v", digest, source)
	}
}

func TestSchemaSourceReadyNeedsTheSourceTheSpecNames(t *testing.T) {
	t.Parallel()
	spec := operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{Name: "storefront-schema"}}
	read := operatorv1alpha1.SchemaSourceStatus{
		ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "storefront-schema", UID: "u"}, Snapshot: snapshot(),
	}
	if !sourcebinding.SchemaSourceReady(spec, read) {
		t.Fatal("a ConfigMap source read into a snapshot is not ready")
	}
	if !sourcebinding.SchemaSourceReady(operatorv1alpha1.SourceSpec{OCIRef: "oci://registry.example/schema:1"}, operatorv1alpha1.SchemaSourceStatus{Verified: true}) {
		t.Fatal("a verified OCI source is not ready")
	}
	renamed := spec
	renamed.ConfigMap = &operatorv1alpha1.ConfigMapSourceSpec{Name: "another"}
	for name, ready := range map[string]bool{
		"an unverified OCI source":        sourcebinding.SchemaSourceReady(operatorv1alpha1.SourceSpec{OCIRef: "oci://x/y:1"}, operatorv1alpha1.SchemaSourceStatus{}),
		"a snapshot of another ConfigMap": sourcebinding.SchemaSourceReady(renamed, read),
		"a snapshot under an OCI spec":    sourcebinding.SchemaSourceReady(operatorv1alpha1.SourceSpec{OCIRef: "oci://x/y:1"}, read),
		"a ConfigMap never read":          sourcebinding.SchemaSourceReady(spec, operatorv1alpha1.SchemaSourceStatus{}),
	} {
		if ready {
			t.Errorf("SchemaSourceReady accepted %s", name)
		}
	}
}

const gitCommit = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"

func gitBinding() *operatorv1alpha1.GitSourceBinding {
	return &operatorv1alpha1.GitSourceBinding{
		URL: "https://git.example.com/acme/app.git", Ref: operatorv1alpha1.GitRef{Branch: "main"}, Commit: gitCommit,
		Path: "db/schema.hcl", ContentDigest: content,
	}
}

// The revision is fixed by the repository and the path, as the API documents
// it, and by nothing else.
func TestGitRevisionNamesTheRepositoryAndThePath(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("https://git.example.com/acme/app.git\ndb/schema.hcl"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	got := sourcebinding.GitRevision("https://git.example.com/acme/app.git", "db/schema.hcl")
	if got != want {
		t.Fatalf("GitRevision() = %q, want %q", got, want)
	}
	for name, other := range map[string]string{
		"another repository": sourcebinding.GitRevision("https://git.example.com/acme/other.git", "db/schema.hcl"),
		"another path":       sourcebinding.GitRevision("https://git.example.com/acme/app.git", "db/other.hcl"),
		"the join moved":     sourcebinding.GitRevision("https://git.example.com/acme/app.git\ndb", "schema.hcl"),
	} {
		if other == got {
			t.Errorf("%s gives the same revision", name)
		}
	}
}

func TestValidateAccessHoldsAGitCommit(t *testing.T) {
	t.Parallel()
	if err := sourcebinding.ValidateAccess(operatorv1alpha1.SourceAccessBinding{Git: gitBinding()}); err != nil {
		t.Fatalf("Git binding refused: %v", err)
	}
	known := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "hosts"}, Key: "known_hosts"}
	ssh := gitBinding()
	ssh.URL = "ssh://git@git.example.com:2222/acme/app.git"
	ssh.Transport.KnownHostsFrom = known
	if err := sourcebinding.ValidateAccess(operatorv1alpha1.SourceAccessBinding{Git: ssh}); err != nil {
		t.Fatalf("ssh Git binding refused: %v", err)
	}
	for name, edit := range map[string]func(*operatorv1alpha1.SourceAccessBinding){
		"a Git commit and a snapshot": func(b *operatorv1alpha1.SourceAccessBinding) { b.Snapshot = snapshot() },
		"a Git commit and a digest":   func(b *operatorv1alpha1.SourceAccessBinding) { b.Digest = artifact },
		"a plain-HTTP registry":       func(b *operatorv1alpha1.SourceAccessBinding) { b.Transport.PlainHTTP = true },
		"another scheme":              func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.URL = "git://git.example.com/acme/app.git" },
		"a query":                     func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.URL += "?ref=main" },
		"a password in an ssh:// URL": func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.URL = "ssh://git:x@git.example.com/a.git" },
		"two refs":                    func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.Ref.Tag = "v1" },
		"no ref":                      func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.Ref = operatorv1alpha1.GitRef{} },
		"a ref commit other than bound": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.Git.Ref = operatorv1alpha1.GitRef{Commit: content[7:47]}
		},
		"an uppercase commit": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.Git.Commit = "3F1C2B0A9D8E7F6A5B4C3D2E1F0A9B8C7D6E5F4A"
		},
		"an escaping path": func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.Path = "../schema.hcl" },
		"an unnamed credential": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.Git.AuthFrom = &operatorv1alpha1.GitAuthSource{Name: " "}
		},
		"known hosts over https://": func(b *operatorv1alpha1.SourceAccessBinding) { b.Git.Transport.KnownHostsFrom = known },
		"a CA without a key": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.Git.Transport.CAFrom = &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ca"}}
		},
	} {
		binding := operatorv1alpha1.SourceAccessBinding{Git: gitBinding()}
		edit(&binding)
		if err := sourcebinding.ValidateAccess(binding); err == nil {
			t.Errorf("ValidateAccess accepted %s", name)
		}
	}
	if err := sourcebinding.ValidateAccess(operatorv1alpha1.SourceAccessBinding{Git: &operatorv1alpha1.GitSourceBinding{
		URL: ssh.URL, Ref: ssh.Ref, Commit: gitCommit, Path: ".", ContentDigest: content,
	}}); err == nil {
		t.Error("ValidateAccess accepted an ssh:// commit with no known hosts")
	}
}

func TestAGitPlanSourceBindsTheRepositoryPathAndContent(t *testing.T) {
	t.Parallel()
	binding := operatorv1alpha1.SourceAccessBinding{Git: gitBinding()}
	source := sourcebinding.GitPlanSource(binding.Git.URL, binding.Git.Path, content)
	if source.Kind != operatorv1alpha1.SourceKindGit || source.ContentDigest != content {
		t.Fatalf("GitPlanSource() = %#v", source)
	}
	if !sourcebinding.AccessGives(binding, "", source) {
		t.Fatal("a Git binding does not give the plan computed from it")
	}
	for name, plan := range map[string]*operatorv1alpha1.PlanSourceBinding{
		"other content":        sourcebinding.GitPlanSource(binding.Git.URL, binding.Git.Path, artifact),
		"another path":         sourcebinding.GitPlanSource(binding.Git.URL, "db/other.hcl", content),
		"a ConfigMap revision": {Kind: operatorv1alpha1.SourceKindConfigMap, Revision: source.Revision, ContentDigest: content},
		"no source":            nil,
	} {
		if sourcebinding.AccessGives(binding, "", plan) {
			t.Errorf("a Git binding gives a plan with %s", name)
		}
	}
	if sourcebinding.AccessGives(binding, artifact, source) {
		t.Error("a Git binding gives a plan that also names an artifact")
	}
	revision := &operatorv1alpha1.GitSourceRevision{
		URL: binding.Git.URL, Ref: binding.Git.Ref, Path: binding.Git.Path, Commit: gitCommit, ContentDigest: content,
	}
	status := operatorv1alpha1.SchemaSourceStatus{Git: revision}
	if digest, plan := sourcebinding.SchemaPlanSource(status); digest != "" || !sourcebinding.Equal(plan, source) {
		t.Fatalf("SchemaPlanSource() = %q %#v, want the Git source", digest, plan)
	}
	spec := operatorv1alpha1.SourceSpec{Git: &operatorv1alpha1.GitSourceSpec{
		URL: revision.URL, Ref: revision.Ref, Path: revision.Path, AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "token"},
	}}
	if !sourcebinding.SchemaSourceReady(spec, status) {
		t.Fatal("a resolved Git source is not ready")
	}
	access := sourcebinding.SchemaAccess(spec, status)
	if access == nil || access.Git == nil || access.Git.Commit != gitCommit || access.Git.AuthFrom == nil || access.Git.AuthFrom.Name != "token" {
		t.Fatalf("SchemaAccess() = %#v, want the resolved commit with the spec's credential", access)
	}
	for name, edit := range map[string]func(*operatorv1alpha1.GitSourceSpec){
		"another repository": func(s *operatorv1alpha1.GitSourceSpec) { s.URL = "https://git.example.com/acme/other.git" },
		"another branch":     func(s *operatorv1alpha1.GitSourceSpec) { s.Ref = operatorv1alpha1.GitRef{Branch: "release"} },
		"another path":       func(s *operatorv1alpha1.GitSourceSpec) { s.Path = "db/other.hcl" },
	} {
		moved := operatorv1alpha1.SourceSpec{Git: spec.Git.DeepCopy()}
		edit(moved.Git)
		if sourcebinding.SchemaSourceReady(moved, status) {
			t.Errorf("a resolution is ready for a spec naming %s", name)
		}
	}
	if sourcebinding.SchemaSourceReady(operatorv1alpha1.SourceSpec{OCIRef: "oci://registry.example/schema:1"}, status) ||
		sourcebinding.SchemaAccess(operatorv1alpha1.SourceSpec{OCIRef: "oci://registry.example/schema:1"}, status) != nil {
		t.Error("a Git resolution answers a spec that names no Git source")
	}
}
