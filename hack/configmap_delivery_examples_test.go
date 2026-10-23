package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The guide to delivering from a ConfigMap shows two ways to put the files in
// one, with Kustomize and with Helm. The mistake a reader is likeliest to make
// is one neither tool reports: a resource that names a ConfigMap the render did
// not produce. Kustomize renames a generated ConfigMap after its content and
// rewrites only the fields it is told name one; a Helm template that hashes the
// content has to give the resource the name it gave the ConfigMap. Either
// mistake renders cleanly and shows up in the cluster as a source that never
// resolves.
//
// So each example is rendered with its own tool, and every Ptah resource in
// the output is held to the CRD and to the ConfigMap it names: that ConfigMap
// has to be in the same render and hold every key the resource selects.
func TestConfigMapDeliveryExamplesRenderResourcesThatResolve(t *testing.T) {
	t.Parallel()

	for _, example := range []struct {
		name   string
		render func(t *testing.T, dir string) []byte
	}{
		{name: "examples/configmap-kustomize", render: renderKustomization},
		{name: "examples/configmap-helm", render: renderHelmChart},
	} {
		t.Run(example.name, func(t *testing.T) {
			t.Parallel()

			documents := renderedDocuments(t, example.render(t, repositoryFile(t, example.name)))
			examined, problems := configMapSourceProblems(t, documents)
			for _, problem := range problems {
				t.Error(problem)
			}
			// Both families, once each: a render that produced neither would
			// otherwise have nothing to report.
			if examined["PtahSchema"] != 1 || examined["PtahMigration"] != 1 {
				t.Errorf("examined %v, want one PtahSchema and one PtahMigration", examined)
			}
		})
	}
}

// The check has to refuse the mistakes it was written for. Each example is
// copied and broken the way a reader would break it, and the copy has to fail.
func TestTheConfigMapDeliveryCheckRefusesAResourceThatNamesNothingRendered(t *testing.T) {
	t.Parallel()

	for _, mistake := range []struct {
		name    string
		example string
		file    string
		from    string
		to      string
		render  func(t *testing.T, dir string) []byte
		want    string
	}{
		{
			// Without the name references Kustomize leaves the resources
			// naming the generator's bare name.
			name:    "a kustomization without the name references",
			example: "examples/configmap-kustomize",
			file:    "kustomization.yaml",
			from:    "configurations:\n  - ptah-references.yaml\n",
			to:      "",
			render:  renderKustomization,
			want:    "names ConfigMap inventory-schema, which the render does not hold",
		},
		{
			name:    "a chart whose resource selects a key its ConfigMap lacks",
			example: "examples/configmap-helm",
			file:    "templates/schema.yaml",
			from:    "        - key: schema.sql\n",
			to:      "        - key: schema.hcl\n",
			render:  renderHelmChart,
			want:    "selects key schema.hcl",
		},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()

			dir := copyExample(t, repositoryFile(t, mistake.example))
			path := filepath.Join(dir, mistake.file)
			original, err := os.ReadFile(path) //nolint:gosec // A file this test copied.
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(original), mistake.from) != 1 {
				t.Fatalf("%s holds %q %d times, want once", mistake.file, mistake.from, strings.Count(string(original), mistake.from))
			}
			broken := strings.Replace(string(original), mistake.from, mistake.to, 1)
			if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
				t.Fatal(err)
			}

			_, problems := configMapSourceProblems(t, renderedDocuments(t, mistake.render(t, dir)))
			if !strings.Contains(strings.Join(problems, "\n"), mistake.want) {
				t.Fatalf("the broken example passed: problems %q, want one containing %q", problems, mistake.want)
			}
		})
	}
}

// configMapSourceProblems validates each PtahSchema and PtahMigration against
// its CRD and against the ConfigMaps of the same render, and counts them.
func configMapSourceProblems(t *testing.T, documents []map[string]any) (map[string]int, []string) {
	t.Helper()

	configMaps := map[string]map[string]bool{}
	for _, document := range documents {
		if document["kind"] != "ConfigMap" {
			continue
		}
		keys := map[string]bool{}
		for _, field := range []string{"data", "binaryData"} {
			values, _ := document[field].(map[string]any)
			for key := range values {
				keys[key] = true
			}
		}
		configMaps[stringAt(document, "metadata", "name")] = keys
	}

	sources := map[string]struct{ crd, field string }{
		"PtahSchema":    {crd: "operator.ptah.run_ptahschemas.yaml", field: "desired"},
		"PtahMigration": {crd: "operator.ptah.run_ptahmigrations.yaml", field: "artifact"},
	}
	examined := map[string]int{}
	var problems []string
	for _, document := range documents {
		kind, _ := document["kind"].(string)
		source, ok := sources[kind]
		if !ok {
			continue
		}
		examined[kind]++
		name := kind + " " + stringAt(document, "metadata", "name")

		schema, props, whole := referenceSpecSchema(t, source.crd)
		spec, _ := document["spec"].(map[string]any)
		for _, unknown := range unknownFields(schema, spec) {
			problems = append(problems, fmt.Sprintf("%s names %s, which the API does not have", name, unknown))
		}
		for _, failure := range schemaFailures(t, props, spec, true) {
			problems = append(problems, fmt.Sprintf("%s: %s", name, failure))
		}
		for _, failure := range celFailures(t, whole, document) {
			problems = append(problems, fmt.Sprintf("%s: %s", name, failure))
		}

		selected, _ := spec[source.field].(map[string]any)
		configMap, _ := selected["configMap"].(map[string]any)
		configMapName, _ := configMap["name"].(string)
		keys, rendered := configMaps[configMapName]
		if !rendered {
			problems = append(problems, fmt.Sprintf("%s names ConfigMap %s, which the render does not hold", name, configMapName))
			continue
		}
		items, _ := configMap["items"].([]any)
		if len(items) == 0 {
			problems = append(problems, fmt.Sprintf("%s selects no key of ConfigMap %s", name, configMapName))
		}
		for _, item := range items {
			key, _ := item.(map[string]any)["key"].(string)
			if !keys[key] {
				problems = append(problems, fmt.Sprintf("%s selects key %s, which ConfigMap %s does not hold", name, key, configMapName))
			}
		}
	}
	return examined, problems
}

func stringAt(document map[string]any, path ...string) string {
	var current any = document
	for _, element := range path {
		object, _ := current.(map[string]any)
		current = object[element]
	}
	value, _ := current.(string)
	return value
}

func renderedDocuments(t *testing.T, rendered []byte) []map[string]any {
	t.Helper()
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	var documents []map[string]any
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents
		}
		if err != nil {
			t.Fatalf("decode the render: %v", err)
		}
		if document != nil {
			documents = append(documents, document)
		}
	}
}

// renderKustomization builds with kustomize where it is installed and with
// kubectl's copy of it otherwise.
func renderKustomization(t *testing.T, dir string) []byte {
	t.Helper()
	if kustomize, err := exec.LookPath("kustomize"); err == nil {
		return renderWith(t, kustomize, "build", dir)
	}
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kustomize or kubectl is required to render the documented kustomization")
	}
	return renderWith(t, kubectl, "kustomize", dir)
}

func renderHelmChart(t *testing.T, dir string) []byte {
	t.Helper()
	return renderWith(t, helmOrSkip(t), "template", "inventory", dir, "--namespace", "application")
}

func renderWith(t *testing.T, program string, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), program, args...) //nolint:gosec // A renderer found on PATH, run on this repository's examples.
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(program), strings.Join(args, " "), err, stderr.String())
	}
	return output
}

func copyExample(t *testing.T, source string) string {
	t.Helper()
	target := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(target, relative), 0o750)
		}
		contents, err := os.ReadFile(path) //nolint:gosec // A file under this repository's examples.
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, relative), contents, 0o600)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", source, err)
	}
	return target
}
