package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func pinSource(kind string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "operator.ptah.run/v1alpha1", "kind": kind, "metadata": map[string]any{"namespace": "one", "name": "source", "uid": "source-uid", "resourceVersion": "1"}}}
	return o
}

func TestRetentionPinsMatchEveryDocumentedField(t *testing.T) {
	raw, err := os.ReadFile("../../docs/site/src/content/docs/use/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(raw), "### Which plans are pinned")
	end := strings.Index(string(raw), "### Prune plans that nothing pins")
	if start < 0 || end <= start {
		t.Fatal("missing pin contract")
	}
	rows := regexp.MustCompile("\\| `([^`]+)` \\|").FindAllStringSubmatch(string(raw)[start:end], -1)
	if len(rows) != 9 || len(retentionPinFields) != 9 {
		t.Fatal("pin denominator changed", len(rows), len(retentionPinFields))
	}
	seen := map[string]bool{}
	for _, f := range retentionPinFields {
		seen[strings.ToLower(f.kind)+"."+f.path] = true
	}
	for _, row := range rows {
		if !seen[row[1]] {
			t.Fatal("uncovered runbook pin", row[1])
		}
	}
	for _, f := range retentionPinFields {
		t.Run(f.kind+"/"+f.path, func(t *testing.T) {
			source := pinSource(f.kind)
			value := map[string]any{"name": "same-name", "uid": "original-plan"}
			if err := unstructured.SetNestedMap(source.Object, value, strings.Split(f.path, ".")...); err != nil {
				t.Fatal(err)
			}
			pins, err := readRetentionPins(source)
			if err != nil || len(pins) != 1 || pins[0].Plan != (retainedPlanID{f.family, "one", "same-name", "original-plan"}) {
				t.Fatal(pins, err)
			}
			for _, bad := range []any{map[string]any{}, map[string]any{"name": "same-name"}, map[string]any{"uid": "original-plan"}, "not a reference"} {
				broken := source.DeepCopy()
				_ = unstructured.SetNestedField(broken.Object, bad, strings.Split(f.path, ".")...)
				if _, err := readRetentionPins(broken); err == nil {
					t.Fatalf("accepted malformed %s", f.path)
				}
			}
		})
	}
}

func TestRetentionKeepsUnresolvedMetadataCopyAndRejectsMalformedEvidence(t *testing.T) {
	object := pinSource("PtahMigration")
	object.SetAnnotations(map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: `{"planRef":{"name":"original","uid":"unresolved-uid"}}`})
	pins, err := readRetentionPins(object)
	if err != nil || len(pins) != 1 || pins[0].Plan.UID != "unresolved-uid" {
		t.Fatal(pins, err)
	}
	for _, value := range []string{"", "null", "[]", "{}", `{"planRef":{"name":"original"}}`, `{"planRef":null}`, `{} {}`} {
		object.SetAnnotations(map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: value})
		if _, err := readRetentionPins(object); err == nil {
			t.Fatal("unresolved copy disappeared", value)
		}
	}
	for _, field := range []pinField{{"PtahSchema", "schema", "status.applied.planRef", true}, {"PtahMigration", "migration", "status.unresolvedRun.planRef", true}} {
		object := pinSource(field.kind)
		path := strings.Split(field.path, ".")
		_ = unstructured.SetNestedMap(object.Object, map[string]any{}, path[:len(path)-1]...)
		if _, err := readRetentionPins(object); err == nil {
			t.Fatal("required nested pin disappeared")
		}
	}
	for _, kind := range []string{"PtahSchemaApproval", "PtahMigrationApproval"} {
		if _, err := readRetentionPins(pinSource(kind)); err == nil {
			t.Fatal("approval has no plan pin")
		}
	}
}

func TestRetentionPinsBindFamilyNamespaceNameAndUID(t *testing.T) {
	id := retainedPlanID{"schema", "one", "same-name", "original-plan"}
	for _, mode := range []string{"valid", "family", "namespace", "name", "UID", "deleting", "absent"} {
		t.Run(mode, func(t *testing.T) {
			plan := pinSource("PtahSchemaPlan")
			plan.SetName(id.Name)
			plan.SetUID(types.UID(id.UID))
			resource := schemaPlanResource
			switch mode {
			case "family":
				resource = migrationPlanResource
			case "namespace":
				plan.SetNamespace("two")
			case "name":
				plan.SetName("other")
			case "UID":
				plan.SetUID("replacement")
			case "deleting":
				plan.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-01T00:00:00Z"
			}
			inventory := retentionInventory{Pins: []retentionPin{{Plan: id}}, Objects: []retainedObject{{resource, plan}}}
			if mode == "absent" {
				inventory.Objects = nil
			}
			err := inventory.validatePins()
			if (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestRetentionValidatesCapturedNativePayloads(t *testing.T) {
	raw, err := os.ReadFile("testdata/churn-plan-export.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "chunk UID", "chunk data", "projection data", "missing chunk"} {
		t.Run(mode, func(t *testing.T) {
			var f struct {
				Plan        map[string]any   `json:"plan"`
				Chunks      []map[string]any `json:"chunks"`
				Projections []map[string]any `json:"projections"`
			}
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			// JSON unmarshalling keeps API integers as float64; unstructured needs int64.
			convert := func(value map[string]any) *unstructured.Unstructured {
				b, _ := json.Marshal(value)
				o := &unstructured.Unstructured{}
				if err := o.UnmarshalJSON(b); err != nil {
					t.Fatal(err)
				}
				return o
			}
			plan, chunk, projection := convert(f.Plan), convert(f.Chunks[0]), convert(f.Projections[0])
			projection.SetAPIVersion("v1")
			projection.SetKind("ConfigMap")
			switch mode {
			case "chunk UID":
				chunk.SetUID("wrong")
			case "chunk data":
				_ = unstructured.SetNestedField(chunk.Object, "d3Jvbmc=", "spec", "data")
			case "projection data":
				_ = unstructured.SetNestedField(projection.Object, "d3Jvbmc=", "binaryData", "chunk")
			}
			inventory := retentionInventory{Objects: []retainedObject{{schemaPlanResource, plan}, {planChunkResource, chunk}, {configMapResource, projection}}}
			if mode == "missing chunk" {
				inventory.Objects = append(inventory.Objects[:1], inventory.Objects[2:]...)
			}
			err := verifyRetentionPayloads(t.Context(), inventory)
			if (err == nil) != (mode == "valid") {
				t.Fatal(fmt.Sprintf("%s: %v", mode, err))
			}
		})
	}
}
