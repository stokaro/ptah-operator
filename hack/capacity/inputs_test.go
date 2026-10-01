package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func testInputCatalog() *inputCatalog {
	c := &inputCatalog{SchemaVersion: 1, Engine: "PostgreSQL", PtahCommit: strings.Repeat("a", 40)}
	for i := range 20 {
		c.InitialRows = append(c.InitialRows, rowInventory{Slot: i, Rows: 10000, SHA256: fmt.Sprintf("%064x", i)})
	}
	for i := range 10 {
		var refs []string
		for r := range 10 {
			value := r
			if i >= 5 {
				value = 0
			}
			refs = append(refs, fmt.Sprintf("oci://registry/input@sha256:%064x", i*10+value))
		}
		c.Schemas = append(c.Schemas, schemaInput{Slot: i, Band: []string{"small", "medium", "large"}[i%3], References: refs})
		n := 128
		if i < 4 {
			n = 2
		} else if i < 7 {
			n = 32
		}
		mrefs := make([]string, len(refs))
		for j, ref := range refs {
			mrefs[j] = strings.Replace(ref, "/input@", "/migrations@", 1)
		}
		c.Migrations = append(c.Migrations, migrationInput{Slot: i, HistoryLength: n, References: mrefs})
	}
	return c
}

func TestInputCatalogRefusesIncompleteOrMismatchedInputs(t *testing.T) {
	load := workload{Schemas: 10, Migrations: 10, ChangeBatch: 5}
	cases := map[string]func(*inputCatalog){
		"engine":             func(c *inputCatalog) { c.Engine = "MySQL" },
		"pin":                func(c *inputCatalog) { c.PtahCommit = strings.Repeat("b", 40) },
		"missing slots":      func(c *inputCatalog) { c.Schemas = c.Schemas[:9] },
		"empty rows":         func(c *inputCatalog) { c.InitialRows = nil },
		"short inventory":    func(c *inputCatalog) { c.InitialRows[0].Rows = 9999 },
		"duplicate rows":     func(c *inputCatalog) { c.InitialRows[1] = c.InitialRows[0] },
		"copied rows":        func(c *inputCatalog) { c.InitialRows[1].SHA256 = c.InitialRows[0].SHA256 },
		"reordered":          func(c *inputCatalog) { c.Schemas[1], c.Schemas[2] = c.Schemas[2], c.Schemas[1] },
		"band":               func(c *inputCatalog) { c.Schemas[1].Band = "small" },
		"history":            func(c *inputCatalog) { c.Migrations[9].HistoryLength = 2 },
		"mutable reference":  func(c *inputCatalog) { c.Schemas[0].References[0] = "oci://registry/input:tag" },
		"missing round":      func(c *inputCatalog) { c.Migrations[0].References = c.Migrations[0].References[:9] },
		"unchanged update":   func(c *inputCatalog) { c.Schemas[0].References[1] = c.Schemas[0].References[0] },
		"changed quiet slot": func(c *inputCatalog) { c.Migrations[9].References[1] = c.Migrations[0].References[1] },
	}
	if err := testInputCatalog().validate(load, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := testInputCatalog()
			mutate(c)
			if err := c.validate(load, strings.Repeat("a", 40)); err == nil {
				t.Fatal("accepted invalid inputs")
			}
		})
	}
	raw, err := json.Marshal(testInputCatalog())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, digest, err := readInputCatalog(path, load, strings.Repeat("a", 40))
	if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatalf("digest %s: %v", digest, err)
	}
	for _, suffix := range []string{"{}", " true"} {
		if err := os.WriteFile(path, append(raw, []byte(suffix)...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readInputCatalog(path, load, strings.Repeat("a", 40)); err == nil {
			t.Fatal("accepted trailing data")
		}
	}
}

func TestInputCatalogRoutesEveryResourceAndAuxiliaryApproval(t *testing.T) {
	c := testInputCatalog()
	s := scenarios{in: inputs{catalog: c, namespace: "one", namespaces: []string{"one", "two"}}, load: workload{Schemas: 10, Migrations: 10}}
	for i := range 10 {
		schema := s.schemaObject(i)
		migration := s.migrationObject(s.migrationName(i), 10+i, "Always", true)
		for _, test := range []struct {
			object          *unstructured.Unstructured
			field, expected string
		}{
			{schema, "desired", c.Schemas[i].References[0]}, {migration, "artifact", c.Migrations[i].References[0]},
		} {
			got, _, err := unstructured.NestedString(test.object.Object, "spec", test.field, "ociRef")
			if err != nil || got != test.expected {
				t.Fatalf("slot %d: %s, %v", i, got, err)
			}
		}
	}
	auxiliary := s.migrationObject("approval", 20, "Manual", false)
	got, _, _ := unstructured.NestedString(auxiliary.Object, "spec", "artifact", "ociRef")
	if got != c.Migrations[0].References[0] {
		t.Fatal("auxiliary approval uses the wrong history")
	}
}

func TestAppliedInputRequiresExactPlanAndArtifact(t *testing.T) {
	ref := "oci://registry/input@sha256:" + strings.Repeat("a", 64)
	object := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "schema", Namespace: "ns", UID: "schema-uid"}}
	plan := &operatorv1alpha1.PtahSchemaPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan", Namespace: "ns", UID: "plan-uid"}}
	plan.Spec.SchemaRef = operatorv1alpha1.ImmutableObjectReference{Name: object.Name, UID: object.UID}
	plan.Spec.ArtifactDigest = digestOf(ref)
	plan.Spec.Fingerprint = "fingerprint"
	object.Status.Applied = &operatorv1alpha1.AppliedStatus{PlanRef: operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}, PlanFingerprint: plan.Spec.Fingerprint, ArtifactDigest: plan.Spec.ArtifactDigest}
	if err := validateAppliedInput(object, plan, ref); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*operatorv1alpha1.PtahSchemaPlan){
		"plan replacement":   func(p *operatorv1alpha1.PtahSchemaPlan) { p.UID = "another" },
		"schema replacement": func(p *operatorv1alpha1.PtahSchemaPlan) { p.Spec.SchemaRef.UID = "another" },
		"fingerprint":        func(p *operatorv1alpha1.PtahSchemaPlan) { p.Spec.Fingerprint = "another" },
		"artifact":           func(p *operatorv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = "another" },
		"namespace":          func(p *operatorv1alpha1.PtahSchemaPlan) { p.Namespace = "another" },
	} {
		t.Run(name, func(t *testing.T) {
			p := plan.DeepCopy()
			mutate(p)
			if validateAppliedInput(object, p, ref) == nil {
				t.Fatal("accepted wrong binding")
			}
		})
	}
	for _, band := range []struct {
		name   string
		lo, hi int
	}{{"small", 3072, 4096}, {"medium", 245760, 262144}, {"large", 983040, 1048576}} {
		for _, size := range []int{0, band.lo - 1, band.hi + 1} {
			if validatePlanBand(band.name, size) == nil {
				t.Fatal("accepted wrong plan size")
			}
		}
		for _, size := range []int{band.lo, band.hi} {
			if err := validatePlanBand(band.name, size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if validatePlanBand("unknown", 0) == nil {
		t.Fatal("accepted unknown band")
	}
}
