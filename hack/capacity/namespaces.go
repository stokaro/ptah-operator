package main

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

func parseNamespaces(raw string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if len(validation.IsDNS1123Label(name)) != 0 || seen[name] {
			return nil, fmt.Errorf("workload namespace %q is invalid or repeated", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func workloadNamespaces(primary string, declared []string) []string {
	if len(declared) != 0 {
		return declared
	}
	return []string{primary}
}

func (in inputs) namespaceFor(index int) string {
	names := workloadNamespaces(in.namespace, in.namespaces)
	return names[index%len(names)]
}

// Each family is distributed independently, so ten resources of each family
// over two namespaces produces five schemas and five migrations in each.
func (s *scenarios) resourceNamespace(resource schema.GroupVersionResource, name string) (string, error) {
	if resource == schemaResource {
		for i := range s.load.Schemas {
			if name == s.schemaName(i) {
				return s.in.namespaceFor(i), nil
			}
		}
	} else if resource == migrationResource {
		for i := range s.load.Migrations {
			if name == s.migrationName(i) {
				return s.in.namespaceFor(i), nil
			}
		}
	}
	return "", fmt.Errorf("%s/%s is not a declared workload resource", resource.Resource, name)
}
