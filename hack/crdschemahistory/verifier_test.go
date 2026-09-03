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

// These tests intentionally use the implementation package. The version
// transition is a closed verifier state machine; exporting it solely for unit
// tests would enlarge the hack-only API without giving production callers a
// useful contract.
package crdschemahistory

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestEvaluateTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		baseline          fixtureOptions
		candidate         fixtureOptions
		wantError         string
		wantInitial       bool
		wantSchemaChanged bool
	}{
		{
			name:      "same version schema change fails",
			baseline:  fixtureOptions{managed: true, version: 2, description: "baseline"},
			candidate: fixtureOptions{managed: true, version: 2, description: "candidate"},
			wantError: "must strictly increase baseline version 2",
		},
		{
			name:              "changed schema with increased version passes",
			baseline:          fixtureOptions{managed: true, version: 2, description: "same"},
			candidate:         fixtureOptions{managed: true, version: 3, description: "same", firstDescription: "changed"},
			wantSchemaChanged: true,
		},
		{
			name:      "unchanged schema version bump fails",
			baseline:  fixtureOptions{managed: true, version: 2, description: "same"},
			candidate: fixtureOptions{managed: true, version: 3, description: "same"},
			wantError: "specs are unchanged",
		},
		{
			name:      "metadata-only controller state change is ignored",
			baseline:  fixtureOptions{managed: true, version: 2, description: "same"},
			candidate: fixtureOptions{managed: true, version: 2, description: "same", controllerStateVersion: "1"},
		},
		{
			name:      "schema rollback fails",
			baseline:  fixtureOptions{managed: true, version: 3, description: "baseline"},
			candidate: fixtureOptions{managed: true, version: 2, description: "candidate"},
			wantError: "must strictly increase baseline version 3",
		},
		{
			name:      "baseline digest mismatch fails",
			baseline:  fixtureOptions{managed: true, version: 2, description: "same", corruptDigest: true},
			candidate: fixtureOptions{managed: true, version: 2, description: "same"},
			wantError: "does not match normalized spec digest",
		},
		{
			name:      "incomplete baseline identity fails",
			baseline:  fixtureOptions{managed: true, version: 2, description: "same", incompleteIdentity: true},
			candidate: fixtureOptions{managed: true, version: 2, description: "same"},
			wantError: "incomplete managed identity",
		},
		{
			name:              "initial unowned baseline adopts version one",
			baseline:          fixtureOptions{description: "baseline"},
			candidate:         fixtureOptions{managed: true, version: 1, description: "candidate"},
			wantInitial:       true,
			wantSchemaChanged: true,
		},
		{
			name:      "initial unowned baseline rejects later version",
			baseline:  fixtureOptions{description: "baseline"},
			candidate: fixtureOptions{managed: true, version: 2, description: "candidate"},
			wantError: "requires candidate operator.ptah.dev/crd-schema-version=1",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			baseline := mustDecodeFixture(t, "baseline", test.baseline)
			candidate := mustDecodeFixture(t, "candidate", test.candidate)
			result, err := evaluateTransition(baseline, candidate)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("evaluateTransition() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.InitialAdoption != test.wantInitial {
				t.Fatalf("InitialAdoption = %t, want %t", result.InitialAdoption, test.wantInitial)
			}
			if result.SchemaChanged != test.wantSchemaChanged {
				t.Fatalf("SchemaChanged = %t, want %t", result.SchemaChanged, test.wantSchemaChanged)
			}
		})
	}
}

func TestDecodeSetRejectsCRDSetChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options fixtureOptions
	}{
		{
			name:    "removed CRD",
			options: fixtureOptions{managed: true, version: 2, description: "same", omitLast: true},
		},
		{
			name:    "added CRD",
			options: fixtureOptions{managed: true, version: 2, description: "same", addUnexpected: true},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeSet("baseline", fixtureDocuments(t, test.options))
			if err == nil || !strings.Contains(err.Error(), "complete generated set") {
				t.Fatalf("decodeSet() error = %v, want CRD set rejection", err)
			}
		})
	}
}

func TestValidateIdentityRequiresOneSharedVersion(t *testing.T) {
	t.Parallel()

	documents := fixtureDocuments(t, fixtureOptions{managed: true, version: 2, description: "same"})
	first := sortedFixtureFileNames(documents)[0]
	crd := decodeFixtureCRD(t, documents[first])
	crd.Annotations[schemaVersionAnnotation] = "3"
	documents[first] = marshalFixtureCRD(t, crd)
	set, err := decodeSet("candidate", documents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateIdentity("candidate", set, false); err == nil || !strings.Contains(err.Error(), "do not share one") {
		t.Fatalf("validateIdentity() error = %v, want shared-version rejection", err)
	}
}

func TestParseVersionRejectsNonCanonicalValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "0", "01", "+1", "-1", "1.0", " 1", "18446744073709551616"} {
		value := value
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			t.Parallel()
			if _, err := parseVersion(value); err == nil {
				t.Fatalf("parseVersion(%q) succeeded", value)
			}
		})
	}
}

type fixtureOptions struct {
	managed                bool
	version                uint64
	description            string
	firstDescription       string
	controllerStateVersion string
	corruptDigest          bool
	incompleteIdentity     bool
	omitLast               bool
	addUnexpected          bool
}

func mustDecodeFixture(t *testing.T, label string, options fixtureOptions) documentSet {
	t.Helper()
	set, err := decodeSet(label, fixtureDocuments(t, options))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func fixtureDocuments(t *testing.T, options fixtureOptions) map[string][]byte {
	t.Helper()
	names := requiredCRDNames()
	if options.omitLast {
		names = names[:len(names)-1]
	}
	if options.addUnexpected {
		names = append(names, "unexpected.operator.ptah.dev")
	}
	documents := make(map[string][]byte, len(names))
	for index, name := range names {
		description := options.description
		if index == 0 && options.firstDescription != "" {
			description = options.firstDescription
		}
		crd := fixtureCRD(name, description)
		if options.managed {
			stampFixtureIdentity(t, crd, options.version)
		}
		if options.controllerStateVersion != "" {
			if crd.Annotations == nil {
				crd.Annotations = make(map[string]string)
			}
			crd.Annotations["operator.ptah.dev/controller-state-version"] = options.controllerStateVersion
		}
		if options.corruptDigest && index == 0 {
			crd.Annotations[schemaDigestAnnotation] = "sha256:" + strings.Repeat("f", 64)
		}
		if options.incompleteIdentity && index == 0 {
			delete(crd.Annotations, schemaDigestAnnotation)
		}
		documents[fmt.Sprintf("crd-%d.yaml", index)] = marshalFixtureCRD(t, crd)
	}
	return documents
}

func fixtureCRD(name, description string) *apiextensionsv1.CustomResourceDefinition {
	plural := strings.Split(name, ".")[0]
	kind := "Unexpected"
	singular := strings.TrimSuffix(plural, "s")
	switch name {
	case "ptahschemas.operator.ptah.dev":
		kind = "PtahSchema"
	case "ptahschemaapprovals.operator.ptah.dev":
		kind = "PtahSchemaApproval"
	case "ptahschemaplans.operator.ptah.dev":
		kind = "PtahSchemaPlan"
	}
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "operator.ptah.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: singular,
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:        "object",
						Description: description,
					},
				},
			}},
		},
	}
}

func stampFixtureIdentity(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, version uint64) {
	t.Helper()
	normalized, err := normalizeSpec(crd)
	if err != nil {
		t.Fatal(err)
	}
	crd.Annotations = map[string]string{
		schemaVersionAnnotation: fmt.Sprintf("%d", version),
		schemaDigestAnnotation:  digestSpec(normalized),
	}
}

func marshalFixtureCRD(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) []byte {
	t.Helper()
	contents, err := yaml.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func decodeFixtureCRD(t *testing.T, contents []byte) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(contents, crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

func sortedFixtureFileNames(documents map[string][]byte) []string {
	names := make([]string, 0, len(documents))
	for name := range documents {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
