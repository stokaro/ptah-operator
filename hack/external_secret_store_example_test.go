package main

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const credentialsPage = "docs/site/src/content/docs/reference/credentials-and-admission.md"

// The credentials page carries the manifests of a path somebody ran, and a
// reader copies them. The PtahSchema is held to the CRD the way a reference
// example is. The tool's own objects are not ours to validate, but the one thing
// that joins them to the operator is: the Secret the ExternalSecret writes has to
// be the Secret the PtahSchema reads, under the key it reads. A rename on one side
// only leaves a Pod waiting for a Secret that never comes, and the resource says
// nothing about it.
func TestTheExternalSecretStoreExampleValidatesAgainstTheAPI(t *testing.T) {
	t.Parallel()

	documents, err := referenceExampleDocuments(repositoryFile(t, credentialsPage))
	if err != nil {
		t.Fatalf("read %s: %v", credentialsPage, err)
	}
	for _, failure := range externalSecretExampleFailures(documents) {
		t.Errorf("%s: %s", credentialsPage, failure)
	}

	schema, props, whole := referenceSpecSchema(t, "operator.ptah.run_ptahschemas.yaml")
	examined := 0
	for _, document := range documents {
		if document["kind"] != "PtahSchema" {
			continue
		}
		examined++
		assertExampleKind(t, credentialsPage, examined-1, document, "PtahSchema")
		spec, ok := document["spec"].(map[string]any)
		if !ok {
			t.Errorf("%s: PtahSchema %d carries no spec", credentialsPage, examined)
			continue
		}
		for _, unknown := range unknownFields(schema, spec) {
			t.Errorf("%s: PtahSchema %d names %s, which the API does not have", credentialsPage, examined, unknown)
		}
		for _, failure := range schemaFailures(t, props, spec, true) {
			t.Errorf("%s: PtahSchema %d: %s", credentialsPage, examined, failure)
		}
		for _, failure := range celFailures(t, whole, document) {
			t.Errorf("%s: PtahSchema %d: %s", credentialsPage, examined, failure)
		}
	}
	if examined != 1 {
		t.Errorf("%s carries %d PtahSchema manifests, want the one the run applied", credentialsPage, examined)
	}
}

// externalSecretExampleFailures says how the page's ExternalSecret and PtahSchema
// fail to name one Secret and one key.
func externalSecretExampleFailures(documents []map[string]any) []string {
	secret, key := "", ""
	for _, document := range documents {
		if document["kind"] != "PtahSchema" {
			continue
		}
		secret, _, _ = unstructured.NestedString(document, "spec", "target", "urlFrom", "name")
		key, _, _ = unstructured.NestedString(document, "spec", "target", "urlFrom", "key")
	}
	if secret == "" || key == "" {
		return []string{"no PtahSchema names a Secret and a key in spec.target.urlFrom"}
	}

	var failures []string
	examined := 0
	for _, document := range documents {
		if document["kind"] != "ExternalSecret" {
			continue
		}
		examined++
		target, _, _ := unstructured.NestedString(document, "spec", "target", "name")
		if target != secret {
			failures = append(failures, fmt.Sprintf("the ExternalSecret writes Secret %q and the PtahSchema reads %q", target, secret))
		}
		if !externalSecretWritesKey(document, key) {
			failures = append(failures, fmt.Sprintf("the ExternalSecret writes no key %q, which the PtahSchema reads", key))
		}
	}
	if examined == 0 {
		failures = append(failures, "no ExternalSecret writes the Secret the PtahSchema reads")
	}
	return failures
}

func externalSecretWritesKey(document map[string]any, key string) bool {
	entries, _, _ := unstructured.NestedSlice(document, "spec", "data")
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if ok && fields["secretKey"] == key {
			return true
		}
	}
	return false
}

func TestTheExternalSecretExampleCheckRefusesAPairThatNamesTwoSecrets(t *testing.T) {
	t.Parallel()

	schema := func() map[string]any {
		return map[string]any{
			"kind": "PtahSchema",
			"spec": map[string]any{"target": map[string]any{
				"urlFrom": map[string]any{"name": "application-database", "key": "url"},
			}},
		}
	}
	external := func(name, key string) map[string]any {
		return map[string]any{
			"kind": "ExternalSecret",
			"spec": map[string]any{
				"target": map[string]any{"name": name},
				"data":   []any{map[string]any{"secretKey": key}},
			},
		}
	}

	if failures := externalSecretExampleFailures([]map[string]any{schema(), external("application-database", "url")}); len(failures) != 0 {
		t.Fatalf("the check refused a pair that names one Secret and one key: %v", failures)
	}

	cases := map[string][]map[string]any{
		"no ExternalSecret":          {schema()},
		"no PtahSchema":              {external("application-database", "url")},
		"another Secret":             {schema(), external("application-db", "url")},
		"another key":                {schema(), external("application-database", "uri")},
		"another Secret and the key": {schema(), external("orders-database", "uri")},
	}
	for name, documents := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if failures := externalSecretExampleFailures(documents); len(failures) == 0 {
				t.Fatal("the check accepted it")
			}
		})
	}
}
