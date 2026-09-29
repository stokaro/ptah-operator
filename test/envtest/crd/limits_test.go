package crd_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
)

// Refusals one past a limit cannot show that the boundary itself is usable.
// Store these values and read them back so pruning, not only rejection, fails.
func TestWorkloadSizeAndNameLimitsAreStored(t *testing.T) {
	plane.Require(t)
	t.Parallel()
	for _, family := range []struct {
		name string
		base func(string) func() *unstructured.Unstructured
	}{{"PtahSchema", schemaBase}, {"PtahMigration", migrationBase}} {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			for _, row := range []struct {
				name  string
				path  []string
				value any
			}{
				{"name", []string{"metadata", "name"}, strings.Repeat("n", 63)},
				{"coordination-key", []string{"spec", "target", "coordinationKey"}, strings.Repeat("k", 253)},
				{"label-count", []string{"spec", "execution", "podMetadata", "labels"}, numberedMap("accepted.example/key-", 16)},
				{"label-key-and-value", []string{"spec", "execution", "podMetadata", "labels"}, map[string]any{"accepted.example/" + strings.Repeat("k", 63): strings.Repeat("v", 63)}},
				{"annotation-count", []string{"spec", "execution", "podMetadata", "annotations"}, numberedMap("accepted.example/key-", 16)},
				{"annotation-value", []string{"spec", "execution", "podMetadata", "annotations"}, map[string]any{"accepted.example/note": strings.Repeat("v", 1024)}},
			} {
				t.Run(row.name, func(t *testing.T) {
					t.Parallel()
					object := family.base(newNamespace(t, "accepted-limit"))()
					set(t, object, row.value, row.path...)
					assertStoredBoundary(t, object, row.path)
				})
			}
		})
	}
}

func TestRecordSizeLimitsAreStored(t *testing.T) {
	plane.Require(t)
	t.Parallel()
	for _, row := range []struct {
		name, kind string
		spec       func() map[string]any
		path       []string
		value      any
	}{
		{"schema-revision", "PtahSchemaPlan", schemaPlanSpec, []string{"spec", "controllerRevision"}, strings.Repeat("r", 128)},
		{"migration-revision", "PtahMigrationPlan", migrationPlanSpec, []string{"spec", "controllerRevision"}, strings.Repeat("r", 128)},
		{"migration-count", "PtahMigrationPlan", migrationPlanSpec, []string{"spec", "migrations"}, maximumMigrationEntries()},
		{"schema-approver-groups", "PtahSchemaApproval", schemaApprovalSpec, []string{"spec", "approver", "groups"}, numbered("group-", 64)},
		{"migration-approver-groups", "PtahMigrationApproval", migrationApprovalSpec, []string{"spec", "approver", "groups"}, numbered("group-", 64)},
		{"realm-namespaces", "PtahRealm", realmSpec, []string{"spec", "namespaces"}, numbered("namespace-", 256)},
		{"realm-namespace-name", "PtahRealm", realmSpec, []string{"spec", "namespaces"}, []any{strings.Repeat("n", 63)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			namespace := ""
			if row.kind != "PtahRealm" {
				namespace = newNamespace(t, "record-limit")
			}
			object := resource(row.kind, namespace, "accepted-"+row.name, row.spec())
			set(t, object, row.value, row.path...)
			assertStoredBoundary(t, object, row.path)
		})
	}
	for _, field := range []string{"exclude", "protectedTables"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			object := schemaBase(newNamespace(t, "policy-limit"))()
			entries := numbered("table_", 128)
			entries[0] = strings.Repeat("t", 256)
			path := []string{"spec", "policy", field}
			set(t, object, entries, path...)
			assertStoredBoundary(t, object, path)
		})
	}
}

func maximumMigrationEntries() []any {
	entries := make([]any, 256)
	for i := range entries {
		entries[i] = map[string]any{
			"version": int64(i + 1), "versionKey": fmt.Sprintf("%04d", i+1), "description": "accepted migration",
			"checksum": strings.Repeat("a", 128), "transactionMode": "file",
		}
	}
	return entries
}

func assertStoredBoundary(t *testing.T, object *unstructured.Unstructured, path []string) {
	t.Helper()
	want, found, err := unstructured.NestedFieldCopy(object.Object, path...)
	if err != nil || !found {
		t.Fatalf("the boundary fixture has no %v: %v", path, err)
	}
	if err := api.Create(context.Background(), object); err != nil {
		t.Fatalf("store %s at %v's boundary: %v", object.GetKind(), path, err)
	}
	got, found, err := unstructured.NestedFieldCopy(reread(t, object).Object, path...)
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s did not preserve %v at its declared boundary: present=%t error=%v", object.GetKind(), path, found, err)
	}
}

// The manifest stays small while its maximum-size document is stored in
// sixteen separate API objects. Exercise every chunk through the API server
// and etcd, then reconstruct the bytes their declared sizes and digests bind.
func TestMaximumPlanBytesSurviveAPIStorage(t *testing.T) {
	plane.Require(t)
	t.Parallel()
	namespace := newNamespace(t, "maximum-plan")
	content, err := json.Marshal(dataplane.PlanFile{
		FormatVersion: dataplane.PlanFormatVersion, Name: "maximum-plan", Dialect: "postgres",
		FromFingerprint: actualStateFingerprint, ToFingerprint: desiredStateFingerprint,
		Statements: []dataplane.PlanStatement{{SQL: "SELECT 1;", Severity: "safe", Reason: "storage boundary"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// JSON whitespace counts toward the byte limit just as SQL does, while
	// leaving a valid native document for the production decoder to read.
	content = append(content, bytes.Repeat([]byte(" "), int(plancontract.MaxExecutableBytes)-len(content))...)
	refs := make([]any, plancontract.MaxChunks)
	for i := range refs {
		data := content[i*plancontract.ChunkBytes : (i+1)*plancontract.ChunkBytes]
		refs[i] = map[string]any{"index": int64(i), "name": fmt.Sprintf("maximum-plan-%03d", i),
			"size": int64(len(data)), "digest": fingerprint.DigestBytes(data)}
	}
	plan := resource("PtahSchemaPlan", namespace, "maximum-plan", schemaPlanSpec())
	set(t, plan, plancontract.MaxExecutableBytes, "spec", "size")
	set(t, plan, int64(1), "spec", "statementCount")
	set(t, plan, fingerprint.DigestBytes(content), "spec", "contentDigest")
	set(t, plan, refs, "spec", "chunks")
	assertStoredBoundary(t, plan, []string{"spec", "chunks"})
	stored := reread(t, plan)
	storedRefs, found, err := unstructured.NestedSlice(stored.Object, "spec", "chunks")
	if err != nil || !found || len(storedRefs) != plancontract.MaxChunks {
		t.Fatalf("the maximum-size plan lost its chunk references: %v", err)
	}
	var reconstructed []byte
	for i, entry := range storedRefs {
		ref := entry.(map[string]any)
		data := content[i*plancontract.ChunkBytes : (i+1)*plancontract.ChunkBytes]
		chunk := resource("PtahSchemaPlanChunk", namespace, ref["name"].(string), map[string]any{"data": base64.StdEncoding.EncodeToString(data)})
		assertStoredBoundary(t, chunk, []string{"spec", "data"})
		encoded, _, _ := unstructured.NestedString(reread(t, chunk).Object, "spec", "data")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || int64(len(decoded)) != ref["size"] || fingerprint.DigestBytes(decoded) != ref["digest"] {
			t.Fatalf("chunk %d no longer matches its size or digest: %v", i, err)
		}
		reconstructed = append(reconstructed, decoded...)
	}
	size, _, _ := unstructured.NestedInt64(stored.Object, "spec", "size")
	digest, _, _ := unstructured.NestedString(stored.Object, "spec", "contentDigest")
	if int64(len(reconstructed)) != size || fingerprint.DigestBytes(reconstructed) != digest || !bytes.Equal(reconstructed, content) {
		t.Fatal("the full plan did not survive storage at the executable byte limit")
	}
	decoded, err := dataplane.DecodePlan(reconstructed, "postgres")
	if err != nil || len(decoded.Statements) != 1 || decoded.Statements[0].SQL != "SELECT 1;" {
		t.Fatalf("the maximum-size stored document no longer decodes as its native plan: %v", err)
	}
}
