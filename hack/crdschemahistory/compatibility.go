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
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// The three transitions that stop a stored object from validating.
//
// AGENTS.md states them as a rule for whoever reviews an API change: "A field
// that was required and is now absent, an enum that lost a value, a default
// that changed -- every one of them is a stored object that stops validating."
// Nothing enforced it. The schema-history verifier required the stamped
// version to move when the specs changed, which records that something
// changed and says nothing about what.
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

// verifyStoredObjectCompatibility reports every transition between the
// baseline and the candidate that reaches an object already in etcd.
//
// Two of the three stop it validating. The third -- a default that moved --
// changes what it reads back as, which is the same class of surprise and the
// one AGENTS.md names beside them.
//
// A kind the baseline does not carry is new, and a new kind has no stored
// objects to break.
func verifyStoredObjectCompatibility(baseline, candidate documentSet) error {
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
	if len(found) == 0 {
		return nil
	}
	rendered := make([]string, 0, len(found))
	for _, item := range found {
		rendered = append(rendered, item.String())
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

// compareSchemas walks two schemas of one kind in parallel.
func compareSchemas(crd, path string, before, after apiextensionsv1.JSONSchemaProps) []incompatibility {
	var found []incompatibility

	// A field that was required and is now absent. A stored object carries a
	// value the candidate schema prunes on the next write, and the write then
	// fails the requirement it no longer satisfies.
	required := make(map[string]bool, len(before.Required))
	for _, name := range before.Required {
		required[name] = true
	}
	names := make([]string, 0, len(before.Properties))
	for name := range before.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := join(path, name)
		successor, kept := after.Properties[name]
		if !kept {
			if required[name] {
				found = append(found, incompatibility{
					crd: crd, path: child,
					detail: "was required and the candidate does not have it",
				})
			}
			continue
		}
		found = append(found, compareProperty(crd, child, before.Properties[name], successor)...)
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

func rawDefault(value *apiextensionsv1.JSON) string {
	if value == nil {
		return ""
	}
	return string(value.Raw)
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
