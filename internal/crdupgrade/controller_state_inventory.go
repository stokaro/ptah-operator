package crdupgrade

import (
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// controllerStateVersionProperty is the schema property that makes an object
// carry controller-written state a later manager has to be able to read.
const controllerStateVersionProperty = "controllerStateVersion"

// ControllerStateResource is one embedded CRD whose stored objects can carry a
// controller-state version, together with the paths at which they carry it.
type ControllerStateResource struct {
	Kind     string
	Group    string
	Version  string
	Resource string
	// Paths are dotted object paths ending in controllerStateVersion, sorted.
	// A path segment inside a list or a map is written "[]", which no nested
	// field read can express, so a schema that buries the property there fails
	// the gates below instead of being quietly skipped.
	Paths []string
}

// ControllerStateBearingResources derives, from the embedded CRDs alone, every
// kind the downgrade preflight has to scan. It is the answer the preflight's
// own kind table and the CRD manager's dynamic clients are measured against:
// both are written by hand, and a kind that starts storing controller state
// has to fail a gate rather than wait for a version bump to read it wrong.
func ControllerStateBearingResources() ([]ControllerStateResource, error) {
	candidates, err := Candidates()
	if err != nil {
		return nil, err
	}
	resources := make([]ControllerStateResource, 0, len(candidates))
	for _, candidate := range candidates {
		paths := map[string]struct{}{}
		storageVersion := ""
		for _, version := range candidate.Spec.Versions {
			if version.Storage {
				storageVersion = version.Name
			}
			if version.Schema == nil {
				continue
			}
			collectControllerStatePaths(version.Schema.OpenAPIV3Schema, "", paths)
		}
		if len(paths) == 0 {
			continue
		}
		if storageVersion == "" {
			return nil, fmt.Errorf("embedded CRD %s stores controller state and has no storage version", candidate.Name)
		}
		resources = append(resources, ControllerStateResource{
			Kind:     candidate.Spec.Names.Kind,
			Group:    candidate.Spec.Group,
			Version:  storageVersion,
			Resource: candidate.Spec.Names.Plural,
			Paths:    sortedKeys(paths),
		})
	}
	return resources, nil
}

func collectControllerStatePaths(props *apiextensionsv1.JSONSchemaProps, path string, found map[string]struct{}) {
	if props == nil {
		return
	}
	child := func(segment string) string {
		if path == "" {
			return segment
		}
		return path + "." + segment
	}
	for name := range props.Properties {
		property := props.Properties[name]
		if name == controllerStateVersionProperty {
			found[child(name)] = struct{}{}
			continue
		}
		collectControllerStatePaths(&property, child(name), found)
	}
	if props.Items != nil {
		collectControllerStatePaths(props.Items.Schema, child("[]"), found)
		for index := range props.Items.JSONSchemas {
			collectControllerStatePaths(&props.Items.JSONSchemas[index], child("[]"), found)
		}
	}
	if props.AdditionalProperties != nil {
		collectControllerStatePaths(props.AdditionalProperties.Schema, child("[]"), found)
	}
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
