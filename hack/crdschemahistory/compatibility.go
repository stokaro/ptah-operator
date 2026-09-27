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
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// The transitions that stop a stored object from validating, or from reading
// back as what was written.
//
// A stored object is validated only on its next write: the API server never
// re-validates it merely because it sits in etcd. What breaks it is a write
// the new schema refuses that the old one would have accepted:
//
//   - a field that was optional and is now required, or a new field that
//     arrives already required, unless the candidate gives it a default --
//     structural defaulting fills an absent field from its schema default
//     before validation runs, on every decode, so a stored object that never
//     set the field reads back with the default already in place and passes
//     the requirement it would otherwise fail. Verified against
//     k8s.io/apiextensions-apiserver v0.37.1: the decode path's defaulter
//     (pkg/apiserver/customresource_handler.go, unstructuredDefaulter.Default)
//     calls pkg/apiserver/schema/defaulting/algorithm.go, whose Default walks
//     the object before anything validates it, filling every property that
//     carries a schema default and is absent from the value being decoded.
//   - an enum that lost a value, a numeric or length bound that tightened, or
//     a pattern that changed: a stored value the old schema allowed and the
//     new one does not fails the same way, on the same next write.
//   - a new or changed x-kubernetes-validations rule on a field that already
//     existed: the rule runs against the stored value the first time anything
//     writes the object, whether or not the write touches that field.
//   - a list-type or map-type annotation that changed: pkg/registry/
//     customresource/strategy.go's Validate calls
//     structurallisttype.ValidateListSetsAndMaps on every write, which enforces
//     the *current* annotation against whatever the stored array already
//     holds -- a stored list that was fine as "atomic" can hold duplicates or
//     unordered entries a "set" or "map" list-type refuses.
//   - a default that changed, appeared, or was taken away: it changes what a
//     stored object that left the field unset reads back as, without anyone
//     having edited that object.
//
// A field the candidate no longer declares is not in this list. Structural
// pruning (pkg/apiserver/schema/pruning) drops a value for any property the
// current schema does not carry when it decodes a stored object, required or
// not, so removing a field -- and the required-ness it once had along with it
// -- reaches no object already in etcd. The schema-history verifier used to
// refuse this anyway; the refusal used a v0.34 claim that never checked
// against v0.37.1's actual pruning code, and two changes to required *status*
// fields (stokaro/ptah-operator#470, #482) that tightened validation the
// other direction went unnoticed by it because it was watching the harmless
// direction.
//
// Each is checked against the storage version of each kind, because that is
// the schema an object in etcd is read back through.

// incompatibility is one breaking transition, named where it happened.
type incompatibility struct {
	crd    string
	path   string
	detail string
}

func (i incompatibility) String() string {
	return fmt.Sprintf("%s: %s: %s", i.crd, i.path, i.detail)
}

// declaredBreak excuses the transitions one change made on purpose.
//
// Breaking a rule this check enforces means editing the check, so that the
// consequence is in the diff a reviewer reads. A declaration is that edit, and
// it is scoped twice: it names every transition it excuses, exactly as the
// check renders it, and the candidate schema version that makes them. In any
// other version it excuses nothing, so a later change that repeats one of its
// transitions is refused again. A declaration for the candidate's version that
// names a transition the candidate does not make is refused too, so it cannot
// quietly miss what it was written for.
type declaredBreak struct {
	version     uint64
	reason      string
	transitions []string
}

// declaredBreaks lists every break made on purpose, oldest first.
//
// The version-22 entry below excuses a category of transition the checker no
// longer produces at all: a required field the candidate removed. Removing a
// field -- required or not -- is pruned on the next read regardless, so it
// reaches no stored object; see the package comment above. The entry stays as
// the historical record of what that change actually did and why, and it
// cannot fire again either way, because a candidate version is required to
// strictly increase and no later commit can present version 22 again.
var declaredBreaks = []declaredBreak{
	{
		version: 22,
		reason: "Plans and approvals bind what a plan means when it runs rather than " +
			"the manager build that computed it (stokaro/ptah-operator#448). The manager " +
			"image, its revision and the runner image built from the same source leave the " +
			"execution binding and the approval; the plan still records them. No release " +
			"carried the old fields. A stored object read through the new schema loses " +
			"them to pruning, and nothing requires them any more.",
		transitions: []string{
			"ptahmigrationapprovals.operator.ptah.run: spec.controllerImage: was required and the candidate does not have it",
			"ptahmigrationapprovals.operator.ptah.run: spec.controllerRevision: was required and the candidate does not have it",
			"ptahmigrationapprovals.operator.ptah.run: spec.runnerImage: was required and the candidate does not have it",
			"ptahmigrations.operator.ptah.run: status.executionBinding.controllerImage: was required and the candidate does not have it",
			"ptahmigrations.operator.ptah.run: status.executionBinding.controllerRevision: was required and the candidate does not have it",
			"ptahmigrations.operator.ptah.run: status.executionBinding.runnerImage: was required and the candidate does not have it",
			"ptahschemaapprovals.operator.ptah.run: spec.controllerImage: was required and the candidate does not have it",
			"ptahschemaapprovals.operator.ptah.run: spec.controllerRevision: was required and the candidate does not have it",
			"ptahschemaapprovals.operator.ptah.run: spec.runnerImage: was required and the candidate does not have it",
			"ptahschemas.operator.ptah.run: status.executionBinding.controllerImage: was required and the candidate does not have it",
			"ptahschemas.operator.ptah.run: status.executionBinding.controllerRevision: was required and the candidate does not have it",
			"ptahschemas.operator.ptah.run: status.executionBinding.runnerImage: was required and the candidate does not have it",
		},
	},
}

// verifyStoredObjectCompatibility reports every transition between the
// baseline and the candidate that reaches an object already in etcd, less
// the ones a break declared for the candidate's schema version excuses.
//
// A kind the baseline does not carry is new, and a new kind has no stored
// objects to break.
func verifyStoredObjectCompatibility(
	baseline, candidate documentSet,
	candidateVersion uint64,
	declared []declaredBreak,
) error {
	var found []incompatibility
	names := make([]string, 0, len(candidate.byName))
	for name := range candidate.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		baselineDocument, carried := baseline.byName[name]
		if !carried {
			continue
		}
		before := storageSchema(baselineDocument.crd)
		after := storageSchema(candidate.byName[name].crd)
		if before == nil || after == nil {
			continue
		}
		found = append(found, compareSchemas(name, "", *before, *after)...)
	}

	// excused maps each transition declared for this version to whether the
	// candidate made it.
	excused := make(map[string]bool)
	for _, declaration := range declared {
		if declaration.version != candidateVersion {
			continue
		}
		for _, transition := range declaration.transitions {
			excused[transition] = false
		}
	}
	var rendered []string
	for _, item := range found {
		transition := item.String()
		if _, isDeclared := excused[transition]; isDeclared {
			excused[transition] = true
			continue
		}
		rendered = append(rendered, transition)
	}
	var unmatched []string
	for transition, made := range excused {
		if !made {
			unmatched = append(unmatched, transition)
		}
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		return fmt.Errorf(
			"a break declared for schema version %d names transitions the candidate does not make:\n  %s",
			candidateVersion, strings.Join(unmatched, "\n  "))
	}
	if len(rendered) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the generated CRDs change in ways that reach objects already stored:\n  %s",
		strings.Join(rendered, "\n  "))
}

// storageSchema returns the schema of the version objects are persisted as.
func storageSchema(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.JSONSchemaProps {
	if crd == nil {
		return nil
	}
	for _, version := range crd.Spec.Versions {
		if !version.Storage || version.Schema == nil {
			continue
		}
		return version.Schema.OpenAPIV3Schema
	}
	return nil
}

// compareSchemas walks two schemas of one kind in parallel: this is the
// schema of one node, be it the CRD's root, a property that is itself an
// object, or an array's item schema, and it is applied to every one of those
// by the two recursions at its own end and inside compareProperty.
func compareSchemas(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	var found []incompatibility

	found = append(found, compareBounds(crd, path, before, after)...)
	found = append(found, comparePattern(crd, path, before, after)...)
	found = append(found, compareValidations(crd, path, before, after)...)
	found = append(found, compareListAndMapType(crd, path, before, after)...)

	requiredBefore := stringSet(before.Required)
	requiredAfter := stringSet(after.Required)

	// Fields the baseline declared. One that the candidate no longer has is
	// pruned on the next read, required or not, and reaches no stored object
	// -- see the package comment. One the candidate kept is checked for
	// whether it just became required, and then walked for whatever else
	// changed underneath it.
	for _, name := range sortedKeys(before.Properties) {
		child := join(path, name)
		successor, kept := after.Properties[name]
		if !kept {
			continue
		}
		if !requiredBefore[name] && requiredAfter[name] && successor.Default == nil {
			found = append(found, incompatibility{
				crd: crd, path: child,
				detail: "was optional and the candidate requires it, with no default to fill it in",
			})
		}
		found = append(found, compareProperty(crd, child, before.Properties[name], successor)...)
	}

	// A field the baseline never had at all. If the candidate requires it
	// with no default, a stored object -- which by definition never set it --
	// fails that requirement on its next write.
	for _, name := range sortedKeys(after.Properties) {
		if _, existed := before.Properties[name]; existed {
			continue
		}
		if !requiredAfter[name] {
			continue
		}
		successor := after.Properties[name]
		if successor.Default != nil {
			continue
		}
		found = append(found, incompatibility{
			crd: crd, path: join(path, name),
			detail: "is new, required, and has no default to fill it in",
		})
	}

	if before.Items != nil && before.Items.Schema != nil &&
		after.Items != nil && after.Items.Schema != nil {
		found = append(found, compareSchemas(crd, join(path, "[]"), *before.Items.Schema, *after.Items.Schema)...)
	}
	return found
}

// compareProperty checks one field that both schemas carry, then descends.
func compareProperty(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	var found []incompatibility

	// An enum that lost a value. Every stored object holding the removed value
	// stops validating, and there is no migration for it.
	if len(before.Enum) > 0 {
		kept := make(map[string]bool, len(after.Enum))
		for _, value := range after.Enum {
			kept[string(value.Raw)] = true
		}
		var lost []string
		for _, value := range before.Enum {
			if !kept[string(value.Raw)] {
				lost = append(lost, strings.Trim(string(value.Raw), `"`))
			}
		}
		if len(lost) > 0 {
			sort.Strings(lost)
			found = append(found, incompatibility{
				crd: crd, path: path,
				detail: "the enum lost " + strings.Join(lost, ", "),
			})
		}
	}

	// A default that changed, or one added where there was none. Both rewrite
	// every stored object that left the field unset, on its next read.
	beforeDefault, afterDefault := rawDefault(before.Default), rawDefault(after.Default)
	if beforeDefault != afterDefault {
		switch {
		case beforeDefault == "":
			found = append(found, incompatibility{
				crd: crd, path: path,
				detail: "gained the default " + afterDefault,
			})
		case afterDefault == "":
			found = append(found, incompatibility{
				crd: crd, path: path,
				detail: "lost its default " + beforeDefault,
			})
		default:
			found = append(found, incompatibility{
				crd: crd, path: path,
				detail: "changed its default from " + beforeDefault + " to " + afterDefault,
			})
		}
	}

	return append(found, compareSchemas(crd, path, before, after)...)
}

// compareBounds reports a numeric or length bound that tightened: a minimum
// that rose or newly appeared, or turned exclusive at the same value; a
// maximum, length, item count or property count that fell, newly appeared, or
// turned exclusive the same way. A stored value the old bound allowed and the
// new one does not fails validation on the value's next write, unchanged.
func compareBounds(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	var found []incompatibility
	if tightenedLowerFloat(before.Minimum, after.Minimum, before.ExclusiveMinimum, after.ExclusiveMinimum) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeLowerFloat("minimum", before.Minimum, after.Minimum)})
	}
	if tightenedUpperFloat(before.Maximum, after.Maximum, before.ExclusiveMaximum, after.ExclusiveMaximum) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeUpperFloat("maximum", before.Maximum, after.Maximum)})
	}
	if tightenedLowerInt(before.MinLength, after.MinLength) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeLowerInt("minLength", before.MinLength, after.MinLength)})
	}
	if tightenedUpperInt(before.MaxLength, after.MaxLength) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeUpperInt("maxLength", before.MaxLength, after.MaxLength)})
	}
	if tightenedLowerInt(before.MinItems, after.MinItems) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeLowerInt("minItems", before.MinItems, after.MinItems)})
	}
	if tightenedUpperInt(before.MaxItems, after.MaxItems) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeUpperInt("maxItems", before.MaxItems, after.MaxItems)})
	}
	if tightenedLowerInt(before.MinProperties, after.MinProperties) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeLowerInt("minProperties", before.MinProperties, after.MinProperties)})
	}
	if tightenedUpperInt(before.MaxProperties, after.MaxProperties) {
		found = append(found, incompatibility{crd: crd, path: path,
			detail: describeUpperInt("maxProperties", before.MaxProperties, after.MaxProperties)})
	}
	return found
}

// comparePattern reports any change to a field's pattern, in either direction.
// A widened or dropped pattern is unlikely to break a stored value, but
// proving that for an arbitrary pair of regular expressions is its own
// project; refusing every change is the conservative reading, and the
// declared-break mechanism is there for the change that is not.
func comparePattern(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	if before.Pattern == after.Pattern {
		return nil
	}
	return []incompatibility{{
		crd: crd, path: path,
		detail: fmt.Sprintf("the pattern changed from %q to %q", before.Pattern, after.Pattern),
	}}
}

// compareValidations reports an x-kubernetes-validations rule the candidate
// carries and the baseline did not, matched by rule expression. A rule that
// changed and one that is brand new render the same way, because both run
// against a stored value the first time anything writes the object, whatever
// the write touches. A rule the candidate dropped is not reported: a stored
// object either already satisfied it or was never written since, so removing
// it can only pass a write the old rule would have refused.
func compareValidations(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	existed := make(map[string]bool, len(before.XValidations))
	for _, rule := range before.XValidations {
		existed[rule.Rule] = true
	}
	var changed []string
	for _, rule := range after.XValidations {
		if !existed[rule.Rule] {
			changed = append(changed, rule.Rule)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	found := make([]incompatibility, 0, len(changed))
	for _, rule := range changed {
		found = append(found, incompatibility{
			crd: crd, path: path,
			detail: fmt.Sprintf("x-kubernetes-validations gained or changed the rule %q", rule),
		})
	}
	return found
}

// compareListAndMapType reports a changed x-kubernetes-list-type or
// x-kubernetes-map-type annotation. Both are enforced on every write, not
// just when the annotated field changes: ValidateListSetsAndMaps checks the
// *current* annotation against whatever the field already holds, so a stored
// array that was fine as "atomic" can hold entries a "set" or "map" list-type
// refuses.
//
// An absent annotation is read as its documented default rather than as its
// own third state, so leaving it out where the default was already spelled
// out -- or spelling out the default where it used to be left out -- is not a
// change. Both defaults are on the field's own doc comment in v0.37.1's
// types_jsonschema.go: XListType's says "Defaults to atomic for arrays", which
// pkg/apiserver/schema/listtype/validation.go's set/map invariant check bears
// out -- it runs only `if s.XListType != nil`, so a nil annotation enforces
// nothing, exactly like an explicit "atomic" does. XMapType's says granular
// "is the default behaviour for all maps".
func compareListAndMapType(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	var found []incompatibility
	beforeListType, afterListType := listType(before.XListType), listType(after.XListType)
	if beforeListType != afterListType {
		found = append(found, incompatibility{
			crd: crd, path: path,
			detail: fmt.Sprintf("x-kubernetes-list-type changed from %q to %q", beforeListType, afterListType),
		})
	}
	beforeMapType, afterMapType := mapType(before.XMapType), mapType(after.XMapType)
	if beforeMapType != afterMapType {
		found = append(found, incompatibility{
			crd: crd, path: path,
			detail: fmt.Sprintf("x-kubernetes-map-type changed from %q to %q", beforeMapType, afterMapType),
		})
	}
	return found
}

func listType(v *string) string {
	if v == nil {
		return "atomic"
	}
	return *v
}

func mapType(v *string) string {
	if v == nil {
		return "granular"
	}
	return *v
}

func tightenedLowerFloat(before, after *float64, beforeExclusive, afterExclusive bool) bool {
	if after == nil {
		return false
	}
	if before == nil {
		return true
	}
	if *after > *before {
		return true
	}
	return *after == *before && afterExclusive && !beforeExclusive
}

func tightenedUpperFloat(before, after *float64, beforeExclusive, afterExclusive bool) bool {
	if after == nil {
		return false
	}
	if before == nil {
		return true
	}
	if *after < *before {
		return true
	}
	return *after == *before && afterExclusive && !beforeExclusive
}

func tightenedLowerInt(before, after *int64) bool {
	if after == nil {
		return false
	}
	if before == nil {
		return true
	}
	return *after > *before
}

func tightenedUpperInt(before, after *int64) bool {
	if after == nil {
		return false
	}
	if before == nil {
		return true
	}
	return *after < *before
}

func describeLowerFloat(name string, before, after *float64) string {
	switch {
	case before == nil:
		return fmt.Sprintf("gained a %s of %s", name, formatFloat(*after))
	case *after > *before:
		return fmt.Sprintf("%s rose from %s to %s", name, formatFloat(*before), formatFloat(*after))
	default:
		return fmt.Sprintf("%s %s became exclusive", name, formatFloat(*after))
	}
}

func describeUpperFloat(name string, before, after *float64) string {
	switch {
	case before == nil:
		return fmt.Sprintf("gained a %s of %s", name, formatFloat(*after))
	case *after < *before:
		return fmt.Sprintf("%s fell from %s to %s", name, formatFloat(*before), formatFloat(*after))
	default:
		return fmt.Sprintf("%s %s became exclusive", name, formatFloat(*after))
	}
}

func describeLowerInt(name string, before, after *int64) string {
	if before == nil {
		return fmt.Sprintf("gained a %s of %d", name, *after)
	}
	return fmt.Sprintf("%s rose from %d to %d", name, *before, *after)
}

func describeUpperInt(name string, before, after *int64) string {
	if before == nil {
		return fmt.Sprintf("gained a %s of %d", name, *after)
	}
	return fmt.Sprintf("%s fell from %d to %d", name, *before, *after)
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) && !math.IsInf(v, 0) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func rawDefault(value *apiextensionsv1.JSON) string {
	if value == nil {
		return ""
	}
	return string(value.Raw)
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedKeys(properties map[string]apiextensionsv1.JSONSchemaProps) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
