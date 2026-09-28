package crd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"

	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// The reference pages are the valid set: a reader copies an example, so each
// one has to be an object the API server stores.
//
// Three kinds are written by a person in full, and their examples go in
// verbatim, as do the plan chunk's, whose one field every example shows. The
// other five are written by the operator or finished by the admission webhook,
// which envtest does not run, so their pages show only the fields worth
// reading. For those the suite adds the required fields an example leaves out
// -- the webhook's stamp and the build binding the operator copies from the
// plan -- and nothing else; what it added is logged, and an example it did not
// need to complete is created as written.
var referenceKinds = []struct {
	kind       string
	namespaced bool
	// completion holds the value the suite supplies for each required spec
	// field an example may omit. Nil means the kind is written in full and an
	// omission is the example's defect.
	completion func() map[string]any
}{
	{kind: "PtahSchema", namespaced: true},
	{kind: "PtahMigration", namespaced: true},
	{kind: "PtahRealm"},
	{kind: "PtahSchemaPlan", namespaced: true, completion: schemaPlanSpec},
	{kind: "PtahSchemaPlanChunk", namespaced: true},
	{kind: "PtahMigrationPlan", namespaced: true, completion: migrationPlanSpec},
	{kind: "PtahSchemaApproval", namespaced: true, completion: schemaApprovalSpec},
	{kind: "PtahMigrationApproval", namespaced: true, completion: migrationApprovalSpec},
	{kind: "PtahMigrationRunAcknowledgment", namespaced: true, completion: runAcknowledgmentSpec},
}

// minimumExamples is what hack/reference_examples_test.go demands of every
// page. Fewer here means the extraction lost some, not that the page is short.
const minimumExamples = 2

func TestEveryReferenceExampleIsStored(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	for _, reference := range referenceKinds {
		t.Run(reference.kind, func(t *testing.T) {
			t.Parallel()
			page := filepath.Join(harness.RepositoryRoot(), "docs", "reference-examples", strings.ToLower(reference.kind)+".md")
			examples, err := referenceExamples(page)
			if err != nil {
				t.Fatalf("read %s: %v", page, err)
			}
			if len(examples) < minimumExamples {
				t.Fatalf("%s yields %d example(s); the page carries at least %d", page, len(examples), minimumExamples)
			}

			stored := 0
			for index, document := range examples {
				object := &unstructured.Unstructured{Object: document}
				if object.GetKind() != reference.kind || object.GetAPIVersion() != apiVersion {
					t.Errorf("example %d of %s is %s %s, want %s %s",
						index+1, page, object.GetAPIVersion(), object.GetKind(), apiVersion, reference.kind)
					continue
				}
				// A namespace per example: two examples of one kind often
				// share a name, which is how the pages read.
				if reference.namespaced {
					object.SetNamespace(newNamespace(t, "example-"+strings.ToLower(reference.kind)))
				}
				if reference.completion != nil {
					if added := complete(object, reference.completion()); len(added) > 0 {
						t.Logf("example %d of %s: supplied %s, which the %s fills in",
							index+1, page, strings.Join(added, ", "), filler(reference.kind))
					}
				}
				if err := api.Create(context.Background(), object); err != nil {
					t.Errorf("the API server refused example %d of %s (%s): %v",
						index+1, page, object.GetName(), err)
					continue
				}
				stored++
			}
			if stored != len(examples) {
				t.Fatalf("stored %d of the %d examples on %s", stored, len(examples), page)
			}
		})
	}
}

// complete adds every field of completion that object's spec lacks, and names
// what it added in a stable order.
func complete(object *unstructured.Unstructured, completion map[string]any) []string {
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}
	var added []string
	for field, value := range completion {
		if _, present := spec[field]; !present {
			spec[field] = value
			added = append(added, field)
		}
	}
	slices.Sort(added)
	object.Object["spec"] = spec
	return added
}

func filler(kind string) string {
	if strings.HasSuffix(kind, "Approval") || strings.HasSuffix(kind, "Acknowledgment") {
		return "admission webhook"
	}
	return "operator"
}

// referenceExamples returns the YAML documents fenced in one reference page. A
// fence in another language is prose about the examples -- a kubectl line --
// and is not one of them. Documents go through JSON and the API machinery's
// decoder so an integer stays an int64, as it does on the wire.
func referenceExamples(path string) ([]map[string]any, error) {
	page, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var documents []map[string]any
	var block strings.Builder
	inYAML := false
	for _, line := range strings.Split(string(page), "\n") {
		if strings.HasPrefix(line, "```") {
			if !inYAML {
				inYAML = strings.TrimSpace(strings.TrimPrefix(line, "```")) == "yaml"
				block.Reset()
				continue
			}
			inYAML = false
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
