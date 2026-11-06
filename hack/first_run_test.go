package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	firstSchemaPage = "docs/site/src/content/docs/start/first-schema.md"
	// The heading the registry path starts at, by the anchor other pages link.
	firstSchemaRegistryHeading = "{#from-a-registry}"
)

var (
	firstSchemaManifest = regexp.MustCompile(`(?s)kubectl apply -f - <<'YAML'\n(.*?)\nYAML\n`)
	firstSchemaSecret   = regexp.MustCompile(`kubectl -n application create secret generic ([a-z0-9-]+) \\\n  --from-file=([a-z]+)=`)
	firstSchemaConfig   = regexp.MustCompile(`kubectl -n application create configmap ([a-z0-9-]+) \\\n  --from-file=([a-z.]+)=`)
)

// A reader who wants to see the operator work should meet no registry on the
// way to a first plan. The page opens with a Secret, a ConfigMap and a resource
// and moves the registry to a step of its own, and everything that needs one
// has to stay behind that heading: a command ahead of it is a condition of
// starting again, whatever the prose says.
func TestTheFirstRunReachesAPlanWithoutARegistry(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, firstSchemaPage)
	first, _, found := strings.Cut(page, firstSchemaRegistryHeading)
	if !found {
		t.Fatalf("%s has no heading %s, so the registry is not a marked next step", firstSchemaPage, firstSchemaRegistryHeading)
	}
	for _, needsARegistry := range registryNeeds(first) {
		t.Errorf("%s asks for %q before the registry step", firstSchemaPage, needsARegistry)
	}
	// The move itself has to be there, or the page lost what a production
	// installation reads.
	_, rest, _ := strings.Cut(page, firstSchemaRegistryHeading)
	if needs := registryNeeds(rest); len(needs) != len(firstRunRegistryNeeds) {
		t.Errorf("the registry step names %v, want every one of %v", needs, firstRunRegistryNeeds)
	}
}

// What only a reader with a registry can do.
var firstRunRegistryNeeds = []string{
	"ptah schema push",
	"ptah-verification-policy",
	"examples/ptahschema.yaml",
	"kubectl ptah set-source",
}

func registryNeeds(text string) []string {
	var found []string
	for _, need := range firstRunRegistryNeeds {
		if strings.Contains(text, need) {
			found = append(found, need)
		}
	}
	return found
}

func TestTheRegistryCheckRefusesACommandAheadOfItsStep(t *testing.T) {
	t.Parallel()
	if needs := registryNeeds("kubectl apply -f -\n"); len(needs) != 0 {
		t.Fatalf("a first run that needs no registry was refused for %v", needs)
	}
	for _, need := range firstRunRegistryNeeds {
		if needs := registryNeeds("something\n" + need + " oci://x\n"); len(needs) != 1 {
			t.Errorf("a first run that says %q was accepted", need)
		}
	}
}

// The first thing a reader copies is this manifest, and they copy it into their
// own cluster. It is held to what the API server enforces and to the two objects
// the commands above it create, since a resource naming a Secret or a key
// nothing made stops at its first Job, in a way the page never mentions.
func TestTheFirstRunManifestIsOneTheAPIAndItsCommandsAgreeWith(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, firstSchemaPage)
	first, _, _ := strings.Cut(page, firstSchemaRegistryHeading)
	matches := firstSchemaManifest.FindAllStringSubmatch(first, -1)
	if len(matches) != 1 {
		t.Fatalf("%s carries %d manifests ahead of the registry step, want 1", firstSchemaPage, len(matches))
	}
	for _, problem := range firstRunManifestProblems(t, matches[0][1], first) {
		t.Error(problem)
	}
}

func TestTheFirstRunManifestCheckRefusesABrokenManifest(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, firstSchemaPage)
	first, _, _ := strings.Cut(page, firstSchemaRegistryHeading)
	matches := firstSchemaManifest.FindAllStringSubmatch(first, -1)
	if len(matches) != 1 {
		t.Fatalf("%s carries %d manifests ahead of the registry step, want 1", firstSchemaPage, len(matches))
	}
	sound := matches[0][1]
	for _, mistake := range []struct{ name, from, to, want string }{
		{"a field the API does not have", "coordinationKey:", "coordination:", "spec.target.coordination"},
		{"an apply policy the API does not offer", "apply: OnApproval", "apply: Sometimes", "apply"},
		{"neither key of a target", "    coordinationKey: production/application-primary\n", "", "coordinationKey"},
		{"a Secret nothing created", "name: application-database", "name: application-db", "Secret application-db"},
		{"a Secret key nothing wrote", "key: url", "key: dsn", "Secret key dsn"},
		{"a ConfigMap nothing created", "name: application-schema", "name: application-sql", "ConfigMap application-sql"},
		{"a ConfigMap key nothing wrote", "key: schema.sql", "key: db.sql", "ConfigMap key db.sql"},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(sound, mistake.from) {
				t.Fatalf("the manifest no longer contains %q, so this row mutates nothing", mistake.from)
			}
			problems := firstRunManifestProblems(t, strings.Replace(sound, mistake.from, mistake.to, 1), first)
			if !strings.Contains(strings.Join(problems, "\n"), mistake.want) {
				t.Fatalf("the check reported %v, want a problem naming %q", problems, mistake.want)
			}
		})
	}
}

func firstRunManifestProblems(t *testing.T, manifest, commands string) []string {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &document); err != nil {
		return []string{fmt.Sprintf("parse the manifest: %v", err)}
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return []string{"the manifest carries no spec"}
	}
	schema, props, whole := referenceSpecSchema(t, "operator.ptah.run_ptahschemas.yaml")
	problems := unknownFields(schema, spec)
	problems = append(problems, schemaFailures(t, props, spec, true)...)
	problems = append(problems, celFailures(t, whole, document)...)

	secret := firstSchemaSecret.FindStringSubmatch(commands)
	config := firstSchemaConfig.FindStringSubmatch(commands)
	if secret == nil || config == nil {
		return append(problems, "the page no longer creates the Secret and the ConfigMap the manifest names")
	}
	target, _ := spec["target"].(map[string]any)
	urlFrom, _ := target["urlFrom"].(map[string]any)
	if urlFrom["name"] != secret[1] {
		problems = append(problems, fmt.Sprintf("the manifest names Secret %v, and the page creates %s", urlFrom["name"], secret[1]))
	}
	if urlFrom["key"] != secret[2] {
		problems = append(problems, fmt.Sprintf("the manifest names Secret key %v, and the page writes %s", urlFrom["key"], secret[2]))
	}
	desired, _ := spec["desired"].(map[string]any)
	source, _ := desired["configMap"].(map[string]any)
	if source["name"] != config[1] {
		problems = append(problems, fmt.Sprintf("the manifest names ConfigMap %v, and the page creates %s", source["name"], config[1]))
	}
	items, _ := source["items"].([]any)
	if len(items) != 1 {
		return append(problems, fmt.Sprintf("the manifest selects %d files, and a schema selects exactly one", len(items)))
	}
	item, _ := items[0].(map[string]any)
	if item["key"] != config[2] {
		problems = append(problems, fmt.Sprintf("the manifest names ConfigMap key %v, and the page writes %s", item["key"], config[2]))
	}
	return problems
}
