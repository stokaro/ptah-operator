package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const recoveryRunbook = "docs/site/src/content/docs/use/recovery.md"

var (
	// inlineFieldPath matches an inline-code span that names a field path on
	// one of these resources. Anything with a bracket is a jsonpath expression
	// rather than a field path, and is left to the command it appears in.
	inlineFieldPath = regexp.MustCompile("`((?:spec|status)(?:\\.[A-Za-z][A-Za-z0-9]*)*)`")
	// kubectlResource matches the resources a command in the runbook reads.
	kubectlResource = regexp.MustCompile(`kubectl get ([a-z,]+)`)
)

// The runbook tells an operator what to preserve after losing a cluster, and
// the list it gives has to be the list the operator actually keeps.
//
// A kind missing from the page is state a reader does not back up, which turns
// a consistent restore into a rebuild without anybody deciding to. This reads
// the CRDs the chart ships rather than a list beside them, so a seventh kind
// fails here instead of being discovered during a recovery.
func TestTheRecoveryRunbookNamesEveryKindThatStoresState(t *testing.T) {
	t.Parallel()
	runbook := string(readRecoveryRunbook(t))
	crds := shippedCRDs(t)
	if len(crds) != 6 {
		t.Fatalf("the chart ships %d CRDs; the runbook was written against six", len(crds))
	}
	for _, crd := range crds {
		kind := crd.Spec.Names.Kind
		if !strings.Contains(runbook, "`"+kind+"`") {
			t.Errorf("%s does not name %s among the kinds that carry durable state", recoveryRunbook, kind)
		}
	}
}

// Every location the durable contract is stored at has to be named, for the
// same reason: a status subtree a backup drops is a binding that comes back
// unbound, and the operator refuses rather than guessing.
func TestTheRecoveryRunbookNamesEveryDurableStateLocation(t *testing.T) {
	t.Parallel()
	runbook := string(readRecoveryRunbook(t))
	found := 0
	for _, crd := range shippedCRDs(t) {
		for _, location := range controllerStateLocations(t, crd) {
			found++
			quoted := "`" + location + "`"
			if !strings.Contains(runbook, quoted) {
				t.Errorf("%s does not name %s on %s, which stores a controller-state version",
					recoveryRunbook, location, crd.Spec.Names.Kind)
			}
		}
	}
	if found == 0 {
		t.Fatal("no durable state location was derived, so this check would pass over nothing")
	}
}

// A field path the runbook quotes has to exist. A procedure that reads a field
// the API dropped sends an operator looking for state that is not there, in the
// one situation where they have no time to find that out.
func TestTheRecoveryRunbookQuotesFieldsTheAPIStillHas(t *testing.T) {
	t.Parallel()
	runbook := string(readRecoveryRunbook(t))
	crds := shippedCRDs(t)
	quoted := inlineFieldPath.FindAllStringSubmatch(runbook, -1)
	if len(quoted) == 0 {
		t.Fatalf("%s quotes no field path, so this check would pass over nothing", recoveryRunbook)
	}
	for _, match := range quoted {
		path := match[1]
		resolved := false
		for _, crd := range crds {
			if resolvesInCRD(crd, path) {
				resolved = true
				break
			}
		}
		if !resolved {
			t.Errorf("%s quotes %q, which no shipped CRD has", recoveryRunbook, path)
		}
	}
}

// And a resource a command reads has to be one the cluster serves.
func TestTheRecoveryRunbookCommandsNameServedResources(t *testing.T) {
	t.Parallel()
	runbook := string(readRecoveryRunbook(t))
	served := map[string]bool{"leases": true}
	for _, crd := range shippedCRDs(t) {
		served[crd.Spec.Names.Plural] = true
	}
	commands := kubectlResource.FindAllStringSubmatch(runbook, -1)
	if len(commands) == 0 {
		t.Fatalf("%s runs no command, so this check would pass over nothing", recoveryRunbook)
	}
	for _, match := range commands {
		for _, resource := range strings.Split(match[1], ",") {
			if !served[resource] {
				t.Errorf("%s reads %q, which is neither a shipped CRD nor a core resource it declares",
					recoveryRunbook, resource)
			}
		}
	}
}

func readRecoveryRunbook(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, recoveryRunbook)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", recoveryRunbook, err)
	}
	return content
}

// shippedCRDs reads the CRDs the chart installs.
func shippedCRDs(t *testing.T) []apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	paths, err := filepath.Glob(repositoryFile(t, filepath.Join("config", "crd", "bases", "*.yaml")))
	if err != nil {
		t.Fatalf("list the CRDs: %v", err)
	}
	sort.Strings(paths)
	crds := make([]apiextensionsv1.CustomResourceDefinition, 0, len(paths))
	for _, path := range paths {
		document, readErr := os.ReadFile(path) //nolint:gosec // A path this test globbed under the repository.
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(document, &crd); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		crds = append(crds, crd)
	}
	if len(crds) == 0 {
		t.Fatal("no CRD was read, so every check over them would pass over nothing")
	}
	return crds
}

// controllerStateLocations names every subtree of one CRD that carries a
// controllerStateVersion, spelled the way the runbook spells it.
func controllerStateLocations(t *testing.T, crd apiextensionsv1.CustomResourceDefinition) []string {
	t.Helper()
	var found []string
	var walk func(node apiextensionsv1.JSONSchemaProps, trail []string)
	walk = func(node apiextensionsv1.JSONSchemaProps, trail []string) {
		names := make([]string, 0, len(node.Properties))
		for name := range node.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "controllerStateVersion" && len(trail) > 0 {
				found = append(found, strings.Join(trail, "."))
			}
			child := node.Properties[name]
			walk(child, append(append([]string{}, trail...), name))
		}
		if node.Items != nil && node.Items.Schema != nil {
			walk(*node.Items.Schema, trail)
		}
	}
	for _, version := range crd.Spec.Versions {
		if version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			walk(*version.Schema.OpenAPIV3Schema, nil)
		}
	}
	sort.Strings(found)
	return unique(found)
}

// resolvesInCRD reports a dotted field path the CRD's schema has.
func resolvesInCRD(crd apiextensionsv1.CustomResourceDefinition, path string) bool {
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		node := *version.Schema.OpenAPIV3Schema
		matched := true
		for _, segment := range strings.Split(path, ".") {
			child, found := node.Properties[segment]
			if !found {
				matched = false
				break
			}
			node = child
		}
		if matched {
			return true
		}
	}
	return false
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
