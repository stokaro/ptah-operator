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

package crdschemahistory

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The three transitions a stored object does not survive, and the neighbouring
// ones it does.
//
// The pairs matter as much as the refusals. A check that refused every schema
// change would be useless and would be turned off within a week, so each
// breaking row has an accepted row beside it that differs only in direction.
func TestStoredObjectCompatibilityRefusesOnlyWhatBreaksAStoredObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		before   apiextensionsv1.JSONSchemaProps
		after    apiextensionsv1.JSONSchemaProps
		wantPart string
	}{
		{
			name:     "a required field is removed",
			before:   object(required("engine"), field("engine", text())),
			after:    object(),
			wantPart: "was required and the candidate does not have it",
		},
		{
			name:   "an optional field is removed",
			before: object(field("engine", text())),
			after:  object(),
		},
		{
			name:     "an enum loses a value",
			before:   object(field("apply", enum("Never", "OnApproval", "Always"))),
			after:    object(field("apply", enum("Never", "OnApproval"))),
			wantPart: "the enum lost Always",
		},
		{
			name:   "an enum gains a value",
			before: object(field("apply", enum("Never", "OnApproval"))),
			after:  object(field("apply", enum("Never", "OnApproval", "Always"))),
		},
		{
			name:     "a default changes",
			before:   object(field("interval", withDefault(text(), `"5m"`))),
			after:    object(field("interval", withDefault(text(), `"10m"`))),
			wantPart: "changed its default from",
		},
		{
			name:     "a default appears where there was none",
			before:   object(field("interval", text())),
			after:    object(field("interval", withDefault(text(), `"5m"`))),
			wantPart: "gained the default",
		},
		{
			name:     "a default is taken away",
			before:   object(field("interval", withDefault(text(), `"5m"`))),
			after:    object(field("interval", text())),
			wantPart: "lost its default",
		},
		{
			name:   "a field is added",
			before: object(field("engine", text())),
			after:  object(field("engine", text()), field("interval", text())),
		},
		{
			name:     "a required field nested under an optional one is removed",
			before:   object(field("target", object(required("engine"), field("engine", text())))),
			after:    object(field("target", object())),
			wantPart: "target.engine: was required",
		},
		{
			name:     "an enum inside a list loses a value",
			before:   object(field("migrations", list(object(field("state", enum("applied", "pending")))))),
			after:    object(field("migrations", list(object(field("state", enum("applied")))))),
			wantPart: "migrations.[].state: the enum lost pending",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := verifyStoredObjectCompatibility(
				setWith("ptahschemas.operator.ptah.run", test.before),
				setWith("ptahschemas.operator.ptah.run", test.after),
				1, nil,
			)
			if test.wantPart == "" {
				if err != nil {
					t.Fatalf("a compatible change was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a change that reaches a stored object was accepted")
			}
			if !strings.Contains(err.Error(), test.wantPart) {
				t.Fatalf("refusal = %v, want it to name %q", err, test.wantPart)
			}
		})
	}
}

// A declared break excuses exactly the transitions it names, and only in the
// schema version it names. Each accepted row has refused rows beside it that
// differ in one of those two.
func TestStoredObjectCompatibilityExcusesOnlyWhatABreakDeclares(t *testing.T) {
	t.Parallel()
	const (
		engineRemoved   = "ptahschemas.operator.ptah.run: engine: was required and the candidate does not have it"
		intervalRemoved = "ptahschemas.operator.ptah.run: interval: was required and the candidate does not have it"
	)
	before := object(required("engine", "interval"), field("engine", text()), field("interval", text()))
	tests := []struct {
		name     string
		after    apiextensionsv1.JSONSchemaProps
		version  uint64
		declared []declaredBreak
		wantPart string
	}{
		{
			name:    "the break is declared for the candidate's version",
			after:   object(field("interval", text())),
			version: 7,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRemoved}},
			},
		},
		{
			name:    "the break is declared for another version",
			after:   object(field("interval", text())),
			version: 8,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRemoved}},
			},
			wantPart: engineRemoved,
		},
		{
			name:    "a second break goes undeclared",
			after:   object(),
			version: 7,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRemoved}},
			},
			wantPart: intervalRemoved,
		},
		{
			name:    "the declaration names a break the candidate does not make",
			after:   object(field("interval", text())),
			version: 7,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRemoved, intervalRemoved}},
			},
			wantPart: "names transitions the candidate does not make:\n  " + intervalRemoved,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := verifyStoredObjectCompatibility(
				setWith("ptahschemas.operator.ptah.run", before),
				setWith("ptahschemas.operator.ptah.run", test.after),
				test.version, test.declared,
			)
			if test.wantPart == "" {
				if err != nil {
					t.Fatalf("a declared break was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a break the declarations do not cover was accepted")
			}
			if !strings.Contains(err.Error(), test.wantPart) {
				t.Fatalf("refusal = %v, want it to name %q", err, test.wantPart)
			}
		})
	}
}

// Every shipped declaration names the version it was written for and at least
// one transition, and no two share a version: a second declaration for one
// version is a second reason for one change, which belongs in the first.
func TestDeclaredBreaksAreEachScopedToOneVersion(t *testing.T) {
	t.Parallel()
	if len(declaredBreaks) == 0 {
		t.Fatal("no declared break was read")
	}
	seen := make(map[uint64]bool, len(declaredBreaks))
	var previous uint64
	for _, declaration := range declaredBreaks {
		if declaration.version == 0 || len(declaration.transitions) == 0 || strings.TrimSpace(declaration.reason) == "" {
			t.Fatalf("declared break %+v needs a version, a reason and at least one transition", declaration)
		}
		if seen[declaration.version] {
			t.Fatalf("schema version %d carries more than one declared break", declaration.version)
		}
		if declaration.version < previous {
			t.Fatalf("declared breaks are not oldest first: %d follows %d", declaration.version, previous)
		}
		seen[declaration.version] = true
		previous = declaration.version
	}
}

// A kind the baseline does not carry has no stored objects to break, so its
// schema is not compared against anything.
func TestStoredObjectCompatibilitySkipsAKindThatIsNew(t *testing.T) {
	t.Parallel()
	baseline := documentSet{byName: map[string]document{}}
	candidate := setWith("ptahmigrations.operator.ptah.run",
		object(required("artifact"), field("artifact", text())))
	if err := verifyStoredObjectCompatibility(baseline, candidate, 1, nil); err != nil {
		t.Fatalf("a kind with no baseline was compared: %v", err)
	}
}

// Only the storage version is compared: it is the schema an object in etcd is
// read back through, and a served-but-not-stored version cannot break one.
func TestStoredObjectCompatibilityReadsTheStorageVersion(t *testing.T) {
	t.Parallel()
	before := crdWithVersions(
		versionSchema("v1alpha1", false, object(required("gone"), field("gone", text()))),
		versionSchema("v1beta1", true, object(required("kept"), field("kept", text()))),
	)
	after := crdWithVersions(
		versionSchema("v1alpha1", false, object()),
		versionSchema("v1beta1", true, object(required("kept"), field("kept", text()))),
	)
	baseline := documentSet{byName: map[string]document{"x": {crd: before}}}
	candidate := documentSet{byName: map[string]document{"x": {crd: after}}}
	if err := verifyStoredObjectCompatibility(baseline, candidate, 1, nil); err != nil {
		t.Fatalf("a served version's change was read as reaching a stored object: %v", err)
	}
}

func setWith(name string, schema apiextensionsv1.JSONSchemaProps) documentSet {
	return documentSet{byName: map[string]document{
		name: {crd: crdWithVersions(versionSchema("v1alpha1", true, schema))},
	}}
}

func crdWithVersions(versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture"},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Versions: versions},
	}
}

func versionSchema(name string, storage bool, schema apiextensionsv1.JSONSchemaProps) apiextensionsv1.CustomResourceDefinitionVersion {
	return apiextensionsv1.CustomResourceDefinitionVersion{
		Name:    name,
		Storage: storage,
		Schema:  &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &schema},
	}
}

type schemaOption func(*apiextensionsv1.JSONSchemaProps)

func object(options ...schemaOption) apiextensionsv1.JSONSchemaProps {
	schema := apiextensionsv1.JSONSchemaProps{
		Type:       "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{},
	}
	for _, option := range options {
		option(&schema)
	}
	return schema
}

func field(name string, child apiextensionsv1.JSONSchemaProps) schemaOption {
	return func(schema *apiextensionsv1.JSONSchemaProps) {
		schema.Properties[name] = child
	}
}

func required(names ...string) schemaOption {
	return func(schema *apiextensionsv1.JSONSchemaProps) {
		schema.Required = append(schema.Required, names...)
	}
}

func text() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "string"}
}

func enum(values ...string) apiextensionsv1.JSONSchemaProps {
	schema := text()
	for _, value := range values {
		schema.Enum = append(schema.Enum, apiextensionsv1.JSON{Raw: []byte(`"` + value + `"`)})
	}
	return schema
}

func withDefault(schema apiextensionsv1.JSONSchemaProps, raw string) apiextensionsv1.JSONSchemaProps {
	schema.Default = &apiextensionsv1.JSON{Raw: []byte(raw)}
	return schema
}

func list(item apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type:  "array",
		Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &item},
	}
}
