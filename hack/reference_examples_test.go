// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// The reference pages carry worked examples, and an example is the part of a
// document a reader copies rather than reads. A field renamed in Go moves the
// generated table by itself; it does not move the YAML beside it, and a manifest
// that names a field the API dropped is worse than no manifest at all -- it
// fails in the reader's cluster, against their database.
//
// So the examples are checked against the schema the API server enforces: every
// field has to exist, every value has to be the type and one of the values the
// CRD allows. The two kinds a person writes in full are also checked for
// completeness; the rest are partial on purpose, because the operator or the
// admission webhook fills the remainder in.
const referenceExamplesDir = "docs/reference-examples"

var referenceExampleKinds = []struct {
	Kind string
	// The CRD the API server serves this kind from.
	CRD string
	// Whether a person authors the whole object. An approval carries what the
	// webhook stamps and a plan is written by the operator, so those examples
	// are deliberately not complete objects.
	AuthoredInFull bool
}{
	{Kind: "PtahSchema", CRD: "operator.ptah.run_ptahschemas.yaml", AuthoredInFull: true},
	{Kind: "PtahSchemaPlan", CRD: "operator.ptah.run_ptahschemaplans.yaml"},
	{Kind: "PtahSchemaApproval", CRD: "operator.ptah.run_ptahschemaapprovals.yaml"},
	{Kind: "PtahMigration", CRD: "operator.ptah.run_ptahmigrations.yaml", AuthoredInFull: true},
	{Kind: "PtahMigrationPlan", CRD: "operator.ptah.run_ptahmigrationplans.yaml"},
	{Kind: "PtahMigrationApproval", CRD: "operator.ptah.run_ptahmigrationapprovals.yaml"},
}

func TestEveryReferenceExampleValidatesAgainstTheAPI(t *testing.T) {
	t.Parallel()

	for _, resource := range referenceExampleKinds {
		resource := resource
		t.Run(resource.Kind, func(t *testing.T) {
			t.Parallel()

			fragment := filepath.Join(referenceExamplesDir, strings.ToLower(resource.Kind)+".md")
			documents, err := referenceExampleDocuments(repositoryFile(t, fragment))
			if err != nil {
				t.Fatalf("read %s: %v", fragment, err)
			}
			// "A couple or three" was the ask, and one example is a special
			// case a reader cannot generalize from.
			if len(documents) < 2 {
				t.Fatalf("%s carries %d example(s), want at least 2", fragment, len(documents))
			}

			schema, props, whole := referenceSpecSchema(t, resource.CRD)
			for index, document := range documents {
				assertExampleKind(t, fragment, index, document, resource.Kind)
				spec, ok := document["spec"].(map[string]any)
				if !ok {
					t.Errorf("%s example %d carries no spec", fragment, index+1)
					continue
				}
				for _, unknown := range unknownFields(schema, spec) {
					t.Errorf("%s example %d names %s, which the API does not have",
						fragment, index+1, unknown)
				}
				for _, failure := range schemaFailures(t, props, spec, resource.AuthoredInFull) {
					t.Errorf("%s example %d: %s", fragment, index+1, failure)
				}
				for _, failure := range celFailures(t, whole, document) {
					t.Errorf("%s example %d: %s", fragment, index+1, failure)
				}
			}
		})
	}
}

// unknownFields is the check that catches a renamed or deleted field. Pruning
// drops everything the schema does not declare, so an example that survives it
// unchanged names only fields the API still has.
func unknownFields(schema *structuralschema.Structural, spec map[string]any) []string {
	pruned := deepCopyJSON(spec)
	structuralpruning.Prune(pruned, schema, true)
	if reflect.DeepEqual(pruned, spec) {
		return nil
	}
	return prunedPaths("spec", spec, pruned)
}

// schemaFailures runs the validation an API server would run on the way in.
//
// A partial example is expected to omit required fields -- the webhook stamps
// the approver, the operator writes the plan -- so those errors are dropped for
// every kind but the two a person writes in full. Nothing else is dropped: a
// wrong type, a value outside an enum and a string that fails its pattern all
// fail here, for every example.
func schemaFailures(
	t *testing.T,
	props *apiextensions.JSONSchemaProps,
	spec map[string]any,
	authoredInFull bool,
) []string {
	t.Helper()
	validator, _, err := apiservervalidation.NewSchemaValidator(props)
	if err != nil {
		t.Fatalf("build the validator: %v", err)
	}
	var failures []string
	for _, failure := range apiservervalidation.ValidateCustomResource(field.NewPath("spec"), spec, validator) {
		if failure.Type == field.ErrorTypeRequired && !authoredInFull {
			continue
		}
		failures = append(failures, failure.Error())
	}
	return failures
}

// celFailures runs the CRD's own x-kubernetes-validations over an example.
//
// The OpenAPI pass above reads types, enums and patterns, and a CEL rule is
// none of those: a minimum expressed as a rule -- an interval of at least ten
// seconds, say -- is invisible to it. An example that the API server would
// refuse is worse than no example, because a reader copies it.
//
// Defaults are applied first. The API server evaluates the rules against the
// defaulted object, and a rule that reads a field the manifest omits would
// otherwise see nothing where a cluster sees the default.
func celFailures(t *testing.T, whole *structuralschema.Structural, document map[string]any) []string {
	t.Helper()
	validator := structuralcel.NewValidator(whole, true, celconfig.PerCallLimit)
	if validator == nil {
		return nil // this CRD declares no rules.
	}
	object := deepCopyJSON(document)
	structuraldefaulting.Default(object, whole)
	errs, _ := validator.Validate(
		context.Background(), field.NewPath(""), whole, object, nil, celconfig.RuntimeCELCostBudget)
	failures := make([]string, 0, len(errs))
	for _, failure := range errs {
		failures = append(failures, failure.Error())
	}
	return failures
}

func assertExampleKind(t *testing.T, fragment string, index int, document map[string]any, kind string) {
	t.Helper()
	if got, _ := document["kind"].(string); got != kind {
		t.Errorf("%s example %d has kind %q, want %q", fragment, index+1, got, kind)
	}
	const group = "operator.ptah.run/v1alpha1"
	if got, _ := document["apiVersion"].(string); got != group {
		t.Errorf("%s example %d has apiVersion %q, want %q", fragment, index+1, got, group)
	}
}

// referenceSpecSchema builds the structural schema for one kind's spec, out of
// the CRDs the chart ships -- the same documents a cluster installs.
func referenceSpecSchema(t *testing.T, name string) (*structuralschema.Structural, *apiextensions.JSONSchemaProps, *structuralschema.Structural) {
	t.Helper()
	document, err := os.ReadFile(repositoryFile(t, filepath.Join("config", "crd", "bases", name)))
	if err != nil {
		t.Fatalf("read the CRD: %v", err)
	}
	var versioned apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(document, &versioned); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(
		&versioned, &internal, nil); err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	for _, version := range internal.Spec.Versions {
		if !version.Storage {
			continue
		}
		// The conversion hoists a schema every version shares onto the CRD
		// itself, so the storage version can legitimately carry none.
		root := internal.Spec.Validation
		if version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			root = version.Schema
		}
		if root == nil || root.OpenAPIV3Schema == nil {
			t.Fatalf("%s storage version %s has no schema", name, version.Name)
		}
		spec, ok := root.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			t.Fatalf("%s declares no spec", name)
		}
		structural, err := structuralschema.NewStructural(&spec)
		if err != nil {
			t.Fatalf("build the structural schema for %s: %v", name, err)
		}
		// The CEL rules are written against the whole object -- a rule on spec
		// can read status, and the API server compiles them from the root -- so
		// the root schema is returned beside the spec one rather than derived
		// from it.
		whole, err := structuralschema.NewStructural(root.OpenAPIV3Schema)
		if err != nil {
			t.Fatalf("build the root structural schema for %s: %v", name, err)
		}
		return structural, &spec, whole
	}
	t.Fatalf("%s names no storage version", name)
	return nil, nil, nil
}

// referenceExampleDocuments returns the YAML documents fenced in one fragment.
// A fence in another language is prose about the examples -- the kubectl lines
// that read the identifiers out of a cluster -- and is not one of them.
func referenceExampleDocuments(path string) ([]map[string]any, error) {
	fragment, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var documents []map[string]any
	var block strings.Builder
	inYAML := false
	for _, line := range strings.Split(string(fragment), "\n") {
		if strings.HasPrefix(line, "```") {
			if !inYAML {
				inYAML = strings.TrimSpace(strings.TrimPrefix(line, "```")) == "yaml"
				block.Reset()
				continue
			}
			inYAML = false
			// Through JSON and the API machinery's decoder rather than
			// straight into a map: a plain YAML decode makes every number a
			// float64, and the CEL runtime reads an integer field as int64 and
			// refuses the object before any rule runs.
			encoded, err := yaml.YAMLToJSONStrict([]byte(block.String()))
			if err != nil {
				return nil, fmt.Errorf("example %d: %w", len(documents)+1, err)
			}
			document := map[string]any{}
			if err := utiljson.Unmarshal(encoded, &document); err != nil {
				return nil, fmt.Errorf("example %d: %w", len(documents)+1, err)
			}
			documents = append(documents, document)
			continue
		}
		if inYAML {
			block.WriteString(line)
			block.WriteString("\n")
		}
	}
	if inYAML {
		return nil, fmt.Errorf("%s ends inside a fenced block", path)
	}
	return documents, nil
}

// prunedPaths names what pruning removed, so the failure says which field to
// look at rather than that something, somewhere, is unknown.
func prunedPaths(prefix string, before, after map[string]any) []string {
	var removed []string
	for key, value := range before {
		next, present := after[key]
		if !present {
			removed = append(removed, prefix+"."+key)
			continue
		}
		nestedBefore, okBefore := value.(map[string]any)
		nestedAfter, okAfter := next.(map[string]any)
		if okBefore && okAfter {
			removed = append(removed, prunedPaths(prefix+"."+key, nestedBefore, nestedAfter)...)
		}
	}
	return removed
}

func deepCopyJSON(source map[string]any) map[string]any {
	copied := make(map[string]any, len(source))
	for key, value := range source {
		switch typed := value.(type) {
		case map[string]any:
			copied[key] = deepCopyJSON(typed)
		case []any:
			list := make([]any, len(typed))
			for index, element := range typed {
				if nested, ok := element.(map[string]any); ok {
					list[index] = deepCopyJSON(nested)
					continue
				}
				list[index] = element
			}
			copied[key] = list
		default:
			copied[key] = value
		}
	}
	return copied
}

// A check that has never refused anything is a check nobody has measured. Each
// mutation below is a way an example goes wrong in practice: a field the API
// dropped, a value outside its enum, a value of the wrong type, and a required
// field left out of a manifest a person is supposed to write in full.
func TestTheReferenceExampleGateRefusesABrokenExample(t *testing.T) {
	t.Parallel()

	schema, props, _ := referenceSpecSchema(t, "operator.ptah.run_ptahschemas.yaml")

	sound := func() map[string]any {
		return map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "production/application-primary",
				"urlFrom":         map[string]any{"name": "application-database", "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://ghcr.io/example/application-schema:1.4.0",
				"verificationPolicyFrom": map[string]any{"name": "ptah-verification-policy", "key": "policy.yaml"},
			},
		}
	}

	if unknown := unknownFields(schema, sound()); len(unknown) > 0 {
		t.Fatalf("the sound example was refused as unknown: %v", unknown)
	}
	if failures := schemaFailures(t, props, sound(), true); len(failures) > 0 {
		t.Fatalf("the sound example was refused: %v", failures)
	}

	t.Run("a field the API does not have", func(t *testing.T) {
		t.Parallel()
		mutated := sound()
		mutated["desiredState"] = "oci://ghcr.io/example/application-schema:1.4.0"
		if unknown := unknownFields(schema, mutated); len(unknown) == 0 {
			t.Fatal("the gate accepted an example naming a field the API does not have")
		}
	})

	t.Run("a nested field the API does not have", func(t *testing.T) {
		t.Parallel()
		mutated := sound()
		mutated["target"].(map[string]any)["coordination"] = "production/application-primary"
		unknown := unknownFields(schema, mutated)
		if len(unknown) == 0 {
			t.Fatal("the gate accepted a nested field the API does not have")
		}
		if unknown[0] != "spec.target.coordination" {
			t.Fatalf("the gate named %q, want spec.target.coordination", unknown[0])
		}
	})

	t.Run("a value outside its enum", func(t *testing.T) {
		t.Parallel()
		mutated := sound()
		mutated["policy"] = map[string]any{"apply": "Sometimes"}
		if failures := schemaFailures(t, props, mutated, true); len(failures) == 0 {
			t.Fatal("the gate accepted an apply policy the API does not offer")
		}
	})

	t.Run("a value of the wrong type", func(t *testing.T) {
		t.Parallel()
		mutated := sound()
		mutated["interval"] = 600
		if failures := schemaFailures(t, props, mutated, true); len(failures) == 0 {
			t.Fatal("the gate accepted an interval that is not a string")
		}
	})

	t.Run("a required field left out", func(t *testing.T) {
		t.Parallel()
		mutated := sound()
		delete(mutated, "target")
		if failures := schemaFailures(t, props, mutated, true); len(failures) == 0 {
			t.Fatal("the gate accepted a manifest with no target")
		}
		// The same omission is expected of a partial example, and must not fail.
		if failures := schemaFailures(t, props, mutated, false); len(failures) > 0 {
			t.Fatalf("the gate refused a partial example for an omission: %v", failures)
		}
	})
}
func TestExamplesReviewGateRejectsCELViolation(t *testing.T) {
	documents, err := referenceExampleDocuments(
		repositoryFile(t, filepath.Join(referenceExamplesDir, "ptahmigration.md")),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Adapted from the issue: it asserted against schemaFailures, which is the
	// OpenAPI pass. A CEL minimum is not an OpenAPI constraint and never
	// reaches that pass, which is the defect -- so the assertion moves to the
	// pass that now runs the rules, and the claim is unchanged.
	document := documents[0]
	document["spec"].(map[string]any)["interval"] = "1s"
	_, _, whole := referenceSpecSchema(t, "operator.ptah.run_ptahmigrations.yaml")
	if failures := celFailures(t, whole, document); len(failures) == 0 {
		t.Fatal("example gate accepted interval=1s despite the shipped CRD's 10s CEL minimum")
	}
}
