package sourcebinding_test

import (
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
	if digest, source := sourcebinding.MigrationPlanSource(access, &revision); digest != "" || source == nil {
		t.Fatalf("MigrationPlanSource(ConfigMap) = %q, %#v", digest, source)
	}
	if digest, source := sourcebinding.MigrationPlanSource(access, nil); digest != "" || source != nil {
		t.Fatalf("MigrationPlanSource(snapshot without its ConfigMap) = %q, %#v, want nothing", digest, source)
	}
	ociAccess := oci()
	if digest, source := sourcebinding.MigrationPlanSource(&ociAccess, nil); digest != artifact || source != nil {
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
