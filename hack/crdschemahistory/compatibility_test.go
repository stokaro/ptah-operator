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

// The transitions a stored object does not survive, and the neighbouring ones
// it does.
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
			// A field the candidate no longer declares is pruned on the next
			// read, required or not: apiextensions-apiserver drops whatever a
			// stored object holds for a property the current schema does not
			// carry. Nothing about that write fails, so this is not refused --
			// see the package comment on why the check used to and does not
			// any more.
			name:   "a required field is removed",
			before: object(required("engine"), field("engine", text())),
			after:  object(),
		},
		{
			name:   "an optional field is removed",
			before: object(field("engine", text())),
			after:  object(),
		},
		{
			name:     "a field becomes required with no default",
			before:   object(field("engine", text())),
			after:    object(required("engine"), field("engine", text())),
			wantPart: "engine: was optional and the candidate requires it, with no default to fill it in",
		},
		{
			// The default, already there before the field became required, is
			// what a stored object that never set the field reads back as
			// before validation ever runs, so the requirement it gained is
			// already satisfied. The default itself does not change, so this
			// is the required-ness transition in isolation.
			name:   "a field becomes required with a default it already had",
			before: object(field("engine", withDefault(text(), `"postgres"`))),
			after:  object(required("engine"), field("engine", withDefault(text(), `"postgres"`))),
		},
		{
			name:     "a new field arrives already required with no default",
			before:   object(),
			after:    object(required("target"), field("target", text())),
			wantPart: "target: is new, required, and has no default to fill it in",
		},
		{
			name:   "a new field arrives already required with a default",
			before: object(),
			after:  object(required("target"), field("target", withDefault(text(), `"primary"`))),
		},
		{
			name:   "a new field is added, optional",
			before: object(field("engine", text())),
			after:  object(field("engine", text()), field("interval", text())),
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
			name:     "a numeric bound tightens",
			before:   object(field("replicas", withMinimum(integer(), 1))),
			after:    object(field("replicas", withMinimum(integer(), 5))),
			wantPart: "replicas: minimum rose from 1 to 5",
		},
		{
			name:   "a numeric bound widens",
			before: object(field("replicas", withMinimum(integer(), 5))),
			after:  object(field("replicas", withMinimum(integer(), 1))),
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
			name:     "a pattern changes",
			before:   object(field("digest", withPattern(text(), `^sha256:[0-9a-f]{64}$`))),
			after:    object(field("digest", withPattern(text(), `^sha256:[0-9a-f]{32}$`))),
			wantPart: "the pattern changed from",
		},
		{
			name:   "a pattern is unchanged",
			before: object(field("digest", withPattern(text(), `^sha256:[0-9a-f]{64}$`))),
			after:  object(field("digest", withPattern(text(), `^sha256:[0-9a-f]{64}$`))),
		},
		{
			name:     "an x-kubernetes-validations rule is added to an existing field",
			before:   object(field("target", object())),
			after:    object(field("target", withValidations(object(), "has(self.engine)"))),
			wantPart: `x-kubernetes-validations gained or changed the rule "has(self.engine)"`,
		},
		{
			// A rule the candidate no longer carries can only pass a write the
			// old rule would have refused, so removing one is not refused.
			name:   "an x-kubernetes-validations rule is removed",
			before: object(field("target", withValidations(object(), "has(self.engine)"))),
			after:  object(field("target", object())),
		},
		{
			name:     "x-kubernetes-list-type changes",
			before:   object(field("migrations", withListType(list(text()), "set"))),
			after:    object(field("migrations", withListType(list(text()), "map"))),
			wantPart: `x-kubernetes-list-type changed from "set" to "map"`,
		},
		{
			name:   "x-kubernetes-list-type is unchanged",
			before: object(field("migrations", withListType(list(text()), "set"))),
			after:  object(field("migrations", withListType(list(text()), "set"))),
		},
		{
			name:     "x-kubernetes-map-type changes",
			before:   object(field("labels", withMapType(object(), "granular"))),
			after:    object(field("labels", withMapType(object(), "atomic"))),
			wantPart: `x-kubernetes-map-type changed from "granular" to "atomic"`,
		},
		{
			// The default for an absent x-kubernetes-list-type is "atomic": the
			// set/map invariant apiextensions-apiserver enforces only runs when
			// the annotation is non-nil, so leaving it unset enforces nothing,
			// same as spelling "atomic" out.
			name:   "x-kubernetes-list-type moves between absent and its own default",
			before: object(field("migrations", list(text()))),
			after:  object(field("migrations", withListType(list(text()), "atomic"))),
		},
		{
			name:   "a required field nested under an optional one is removed",
			before: object(field("target", object(required("engine"), field("engine", text())))),
			after:  object(field("target", object())),
		},
		{
			name:     "a field nested under an optional one becomes required",
			before:   object(field("target", object(field("engine", text())))),
			after:    object(field("target", object(required("engine"), field("engine", text())))),
			wantPart: "target.engine: was optional and the candidate requires it",
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

// Every bound compareBounds knows about, tightened and widened, plus a bound
// that newly appears and one whose exclusivity flips at an unchanged value.
// Each refused row has a widening row beside it so that deleting the branch
// that refuses it leaves some row silently passing.
func TestCompareBoundsRefusesOnlyTightening(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		before   apiextensionsv1.JSONSchemaProps
		after    apiextensionsv1.JSONSchemaProps
		wantPart string
	}{
		{
			name:     "minimum rises",
			before:   withMinimum(integer(), 1),
			after:    withMinimum(integer(), 5),
			wantPart: "minimum rose from 1 to 5",
		},
		{name: "minimum falls", before: withMinimum(integer(), 5), after: withMinimum(integer(), 1)},
		{
			name:     "minimum newly appears",
			before:   integer(),
			after:    withMinimum(integer(), 1),
			wantPart: "gained a minimum of 1",
		},
		{
			name:     "minimum becomes exclusive at the same value",
			before:   withMinimum(integer(), 1),
			after:    withExclusiveMinimum(withMinimum(integer(), 1)),
			wantPart: "minimum 1 became exclusive",
		},
		{
			name:     "maximum falls",
			before:   withMaximum(integer(), 10),
			after:    withMaximum(integer(), 5),
			wantPart: "maximum fell from 10 to 5",
		},
		{name: "maximum rises", before: withMaximum(integer(), 5), after: withMaximum(integer(), 10)},
		{
			name:     "maximum newly appears",
			before:   integer(),
			after:    withMaximum(integer(), 10),
			wantPart: "gained a maximum of 10",
		},
		{
			name:     "maximum becomes exclusive at the same value",
			before:   withMaximum(integer(), 10),
			after:    withExclusiveMaximum(withMaximum(integer(), 10)),
			wantPart: "maximum 10 became exclusive",
		},
		{
			name:     "minLength rises",
			before:   withMinLength(text(), 1),
			after:    withMinLength(text(), 3),
			wantPart: "minLength rose from 1 to 3",
		},
		{name: "minLength falls", before: withMinLength(text(), 3), after: withMinLength(text(), 1)},
		{
			name:     "maxLength falls",
			before:   withMaxLength(text(), 100),
			after:    withMaxLength(text(), 10),
			wantPart: "maxLength fell from 100 to 10",
		},
		{name: "maxLength rises", before: withMaxLength(text(), 10), after: withMaxLength(text(), 100)},
		{
			name:     "minItems rises",
			before:   withMinItems(list(text()), 0),
			after:    withMinItems(list(text()), 1),
			wantPart: "minItems rose from 0 to 1",
		},
		{name: "minItems falls", before: withMinItems(list(text()), 1), after: withMinItems(list(text()), 0)},
		{
			name:     "maxItems falls",
			before:   withMaxItems(list(text()), 50),
			after:    withMaxItems(list(text()), 10),
			wantPart: "maxItems fell from 50 to 10",
		},
		{name: "maxItems rises", before: withMaxItems(list(text()), 10), after: withMaxItems(list(text()), 50)},
		{
			name:     "minProperties rises",
			before:   withMinProperties(object(), 0),
			after:    withMinProperties(object(), 1),
			wantPart: "minProperties rose from 0 to 1",
		},
		{
			name:   "minProperties falls",
			before: withMinProperties(object(), 1),
			after:  withMinProperties(object(), 0),
		},
		{
			name:     "maxProperties falls",
			before:   withMaxProperties(object(), 10),
			after:    withMaxProperties(object(), 5),
			wantPart: "maxProperties fell from 10 to 5",
		},
		{
			name:   "maxProperties rises",
			before: withMaxProperties(object(), 5),
			after:  withMaxProperties(object(), 10),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := verifyStoredObjectCompatibility(
				setWith("ptahschemas.operator.ptah.run", object(field("bound", test.before))),
				setWith("ptahschemas.operator.ptah.run", object(field("bound", test.after))),
				1, nil,
			)
			if test.wantPart == "" {
				if err != nil {
					t.Fatalf("a widened bound was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a tightened bound was accepted")
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
		engineRequired   = "ptahschemas.operator.ptah.run: engine: was optional and the candidate requires it, with no default to fill it in"
		intervalRequired = "ptahschemas.operator.ptah.run: interval: was optional and the candidate requires it, with no default to fill it in"
	)
	before := object(field("engine", text()), field("interval", text()))
	tests := []struct {
		name     string
		after    apiextensionsv1.JSONSchemaProps
		version  uint64
		declared []declaredBreak
		wantPart string
	}{
		{
			name:    "the break is declared for the candidate's version",
			after:   object(required("engine"), field("engine", text()), field("interval", text())),
			version: 7,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRequired}},
			},
		},
		{
			name:    "the break is declared for another version",
			after:   object(required("engine"), field("engine", text()), field("interval", text())),
			version: 8,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRequired}},
			},
			wantPart: engineRequired,
		},
		{
			name:    "a second break goes undeclared",
			after:   object(required("engine", "interval"), field("engine", text()), field("interval", text())),
			version: 7,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRequired}},
			},
			wantPart: intervalRequired,
		},
		{
			name:    "the declaration names a break the candidate does not make",
			after:   object(required("engine"), field("engine", text()), field("interval", text())),
			version: 7,
			declared: []declaredBreak{
				{version: 7, transitions: []string{engineRequired, intervalRequired}},
			},
			wantPart: "names transitions the candidate does not make:\n  " + intervalRequired,
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
// version is a second reason for one change, which belongs in the first. The
// list is empty since the history restarted, so the check is first shown the
// lists it has to refuse; otherwise it would pass over nothing.
func TestDeclaredBreaksAreEachScopedToOneVersion(t *testing.T) {
	t.Parallel()
	transition := []string{"ptahschemas.operator.ptah.run: engine: was optional and the candidate requires it, with no default to fill it in"}
	for name, declared := range map[string][]declaredBreak{
		"no version":          {{reason: "why", transitions: transition}},
		"no reason":           {{version: 2, reason: " ", transitions: transition}},
		"no transition":       {{version: 2, reason: "why"}},
		"two for one version": {{version: 2, reason: "why", transitions: transition}, {version: 2, reason: "again", transitions: transition}},
		"newest first":        {{version: 3, reason: "why", transitions: transition}, {version: 2, reason: "why", transitions: transition}},
	} {
		if err := validateDeclaredBreaks(declared); err == nil {
			t.Errorf("%s: the declarations were accepted", name)
		}
	}
	if err := validateDeclaredBreaks([]declaredBreak{
		{version: 2, reason: "why", transitions: transition},
		{version: 3, reason: "why", transitions: transition},
	}); err != nil {
		t.Fatalf("well-formed declarations were refused: %v", err)
	}
	if err := validateDeclaredBreaks(declaredBreaks); err != nil {
		t.Fatal(err)
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

func integer() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "integer"}
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

func withPattern(schema apiextensionsv1.JSONSchemaProps, pattern string) apiextensionsv1.JSONSchemaProps {
	schema.Pattern = pattern
	return schema
}

func withMinimum(schema apiextensionsv1.JSONSchemaProps, value float64) apiextensionsv1.JSONSchemaProps {
	schema.Minimum = &value
	return schema
}

func withMaximum(schema apiextensionsv1.JSONSchemaProps, value float64) apiextensionsv1.JSONSchemaProps {
	schema.Maximum = &value
	return schema
}

func withExclusiveMinimum(schema apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	schema.ExclusiveMinimum = true
	return schema
}

func withExclusiveMaximum(schema apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	schema.ExclusiveMaximum = true
	return schema
}

func withMinLength(schema apiextensionsv1.JSONSchemaProps, value int64) apiextensionsv1.JSONSchemaProps {
	schema.MinLength = &value
	return schema
}

func withMaxLength(schema apiextensionsv1.JSONSchemaProps, value int64) apiextensionsv1.JSONSchemaProps {
	schema.MaxLength = &value
	return schema
}

func withMinItems(schema apiextensionsv1.JSONSchemaProps, value int64) apiextensionsv1.JSONSchemaProps {
	schema.MinItems = &value
	return schema
}

func withMaxItems(schema apiextensionsv1.JSONSchemaProps, value int64) apiextensionsv1.JSONSchemaProps {
	schema.MaxItems = &value
	return schema
}

func withMinProperties(schema apiextensionsv1.JSONSchemaProps, value int64) apiextensionsv1.JSONSchemaProps {
	schema.MinProperties = &value
	return schema
}

func withMaxProperties(schema apiextensionsv1.JSONSchemaProps, value int64) apiextensionsv1.JSONSchemaProps {
	schema.MaxProperties = &value
	return schema
}

func withValidations(schema apiextensionsv1.JSONSchemaProps, rules ...string) apiextensionsv1.JSONSchemaProps {
	for _, rule := range rules {
		schema.XValidations = append(schema.XValidations, apiextensionsv1.ValidationRule{Rule: rule})
	}
	return schema
}

func withListType(schema apiextensionsv1.JSONSchemaProps, value string) apiextensionsv1.JSONSchemaProps {
	schema.XListType = &value
	return schema
}

func withMapType(schema apiextensionsv1.JSONSchemaProps, value string) apiextensionsv1.JSONSchemaProps {
	schema.XMapType = &value
	return schema
}

func list(item apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type:  "array",
		Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &item},
	}
}
