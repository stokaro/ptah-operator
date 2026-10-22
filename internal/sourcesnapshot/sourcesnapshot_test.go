package sourcesnapshot_test

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

var schemaOwner = sourcesnapshot.Owner{Kind: "PtahSchema", Name: "storefront", UID: "schema-uid"}

func source() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "storefront-schema"},
		Data:       map[string]string{"schema.hcl": "table \"orders\" {}\n", "notes": "unselected\n"},
		BinaryData: map[string][]byte{"seed.bin": {0xff, 0x00}},
	}
}

func TestSelectReadsDataAndBinaryDataExactly(t *testing.T) {
	t.Parallel()
	files, err := sourcesnapshot.Select(source(), []operatorv1alpha1.ConfigMapSourceItem{
		{Key: "schema.hcl", Path: "db/schema.hcl"},
		{Key: "seed.bin", Path: "seed.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "db/schema.hcl" || string(files[0].Data) != "table \"orders\" {}\n" ||
		files[1].Path != "seed.bin" || string(files[1].Data) != "\xff\x00" {
		t.Fatalf("Select() = %#v", files)
	}
	if _, err := sourcesnapshot.Select(source(), []operatorv1alpha1.ConfigMapSourceItem{{Key: "missing", Path: "a.sql"}}); !errors.Is(err, sourcesnapshot.ErrMissingKey) {
		t.Fatalf("Select(missing key) = %v, want ErrMissingKey", err)
	}
}

func built(t *testing.T) (*corev1.ConfigMap, string) {
	t.Helper()
	files, err := sourcesnapshot.Select(source(), []operatorv1alpha1.ConfigMapSourceItem{
		{Key: "schema.hcl", Path: "schema.hcl"}, {Key: "seed.bin", Path: "data/seed.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, digest, err := sourcesnapshot.Build("application", schemaOwner, files)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, digest
}

func TestBuildWritesTheLayoutTheRunnerReads(t *testing.T) {
	t.Parallel()
	snapshot, digest := built(t)
	if snapshot.Name != sourcesnapshot.Name(schemaOwner.UID, digest) || !sourcesnapshot.IsName(snapshot.Name) {
		t.Fatalf("snapshot name %q does not derive from its owner and digest", snapshot.Name)
	}
	if snapshot.Immutable == nil || !*snapshot.Immutable || len(snapshot.Data) != 0 || len(snapshot.BinaryData) != 3 {
		t.Fatalf("snapshot is not immutable binaryData with a manifest and two files: %#v", snapshot)
	}
	if sourcecontent.DigestOf(snapshot.BinaryData[sourcecontent.ManifestKey]) != digest {
		t.Fatal("the stored manifest does not give the digest")
	}
	// Files are stored in manifest order: data/seed.bin sorts before schema.hcl.
	if string(snapshot.BinaryData[sourcecontent.SnapshotFileKey(0)]) != "\xff\x00" ||
		string(snapshot.BinaryData[sourcecontent.SnapshotFileKey(1)]) != "table \"orders\" {}\n" {
		t.Fatal("files are not stored under the key of their manifest line")
	}
	if snapshot.Labels[sourcesnapshot.LabelSchema] != "storefront" || len(snapshot.OwnerReferences) != 1 ||
		snapshot.OwnerReferences[0].UID != schemaOwner.UID || !*snapshot.OwnerReferences[0].Controller {
		t.Fatalf("snapshot does not belong to its schema: %#v", snapshot.ObjectMeta)
	}
	// Another resource with the same files keeps a snapshot of its own.
	if sourcesnapshot.Name("other-uid", digest) == snapshot.Name {
		t.Fatal("two resources share one snapshot name")
	}
}

func TestBuildRefusesWhatOneConfigMapCannotHold(t *testing.T) {
	t.Parallel()
	large := strings.Repeat("x", sourcecontent.MaxSnapshotBytes/2)
	files := []sourcecontent.File{{Path: "a.sql", Data: []byte(large)}, {Path: "b.sql", Data: []byte(large)}}
	if _, _, err := sourcesnapshot.Build("application", schemaOwner, files); !errors.Is(err, sourcesnapshot.ErrTooLarge) {
		t.Fatalf("Build(two halves of the limit and a manifest) = %v, want ErrTooLarge", err)
	}
	if _, _, err := sourcesnapshot.Build("application", schemaOwner, files[:1]); err != nil {
		t.Fatalf("Build(half the limit) = %v", err)
	}
	if _, _, err := sourcesnapshot.Build("application", sourcesnapshot.Owner{Kind: "Pod", Name: "x", UID: "y"}, files[:1]); err == nil {
		t.Fatal("Build gave a snapshot to an owner that is not a Ptah resource")
	}
}

func TestVerifyAcceptsOnlyWhatBuildWrites(t *testing.T) {
	t.Parallel()
	snapshot, digest := built(t)
	owner, got, err := sourcesnapshot.Verify(snapshot)
	if err != nil || owner != schemaOwner || got != digest {
		t.Fatalf("Verify(built) = %#v, %q, %v", owner, got, err)
	}
	if !sourcesnapshot.Same(snapshot, snapshot.DeepCopy()) {
		t.Fatal("Same refused a snapshot and its copy")
	}
	for name, mutate := range map[string]func(*corev1.ConfigMap){
		"another name":       func(c *corev1.ConfigMap) { c.Name = sourcesnapshot.Name("other-uid", digest) },
		"not a snapshot":     func(c *corev1.ConfigMap) { c.Name = "storefront-schema" },
		"a changed file":     func(c *corev1.ConfigMap) { c.BinaryData[sourcecontent.SnapshotFileKey(0)] = []byte{0xff, 0x01} },
		"an extra key":       func(c *corev1.ConfigMap) { c.BinaryData[sourcecontent.SnapshotFileKey(2)] = []byte("x") },
		"a data key":         func(c *corev1.ConfigMap) { c.Data = map[string]string{"x": "y"} },
		"mutable":            func(c *corev1.ConfigMap) { c.Immutable = nil },
		"another label":      func(c *corev1.ConfigMap) { c.Labels = map[string]string{sourcesnapshot.LabelMigration: "storefront"} },
		"an annotation":      func(c *corev1.ConfigMap) { c.Annotations = map[string]string{"a": "b"} },
		"a second owner":     func(c *corev1.ConfigMap) { c.OwnerReferences = append(c.OwnerReferences, c.OwnerReferences[0]) },
		"an owner not ruled": func(c *corev1.ConfigMap) { c.OwnerReferences[0].Controller = nil },
		"a foreign owner":    func(c *corev1.ConfigMap) { c.OwnerReferences[0].Kind = "Deployment" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutated, _ := built(t)
			mutate(mutated)
			if _, _, err := sourcesnapshot.Verify(mutated); err == nil {
				t.Fatalf("Verify accepted a snapshot with %s", name)
			}
			if sourcesnapshot.Same(mutated, snapshot) {
				t.Fatalf("Same accepted a snapshot with %s", name)
			}
		})
	}
}
