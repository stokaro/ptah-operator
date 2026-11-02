package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// The demonstration's manifests are applied in front of a reader, who copies
// them. A recording proves each one was admitted by the cluster it ran on, an
// hour and a lab after the change that broke it. This holds every manifest
// under demo/manifests to the API before any lab exists: a Ptah resource to the
// CRD the API server enforces, OpenAPI and CEL both, and a Job to the type the
// API server decodes it into.
const demoManifestsDir = "demo/manifests"

func TestEveryDemoManifestValidatesAgainstTheAPI(t *testing.T) {
	t.Parallel()

	var found []string
	root := repositoryFile(t, demoManifestsDir)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", demoManifestsDir, err)
	}
	if len(found) == 0 {
		t.Fatalf("%s holds no manifest, so this test held nothing", demoManifestsDir)
	}

	examined := 0
	for _, path := range found {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := filepath.Rel(root, path)
		documents, problems := demoManifestProblems(t, raw)
		for _, problem := range problems {
			t.Errorf("%s: %s", filepath.ToSlash(name), problem)
		}
		if documents == 0 {
			t.Errorf("%s holds no document", filepath.ToSlash(name))
			continue
		}
		examined++
	}
	if examined != len(found) {
		t.Errorf("examined %d of the %d manifests under %s", examined, len(found), demoManifestsDir)
	}
}

// demoManifestProblems says what the API would refuse in one manifest file,
// and how many documents it read.
func demoManifestProblems(t *testing.T, raw []byte) (int, []string) {
	t.Helper()
	crds := map[string]string{}
	for _, resource := range referenceExampleKinds {
		crds[resource.Kind] = resource.CRD
	}

	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	documents := 0
	var problems []string
	for {
		chunk, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, problems
		}
		if err != nil {
			return documents, append(problems, err.Error())
		}
		if len(bytes.TrimSpace(chunk)) == 0 {
			continue
		}
		documents++
		where := fmt.Sprintf("document %d", documents)
		// Through JSON and the API machinery's decoder, as the reference
		// examples are: CEL reads an integer field as int64.
		encoded, err := yaml.YAMLToJSONStrict(chunk)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", where, err))
			continue
		}
		document := map[string]any{}
		if err := utiljson.Unmarshal(encoded, &document); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", where, err))
			continue
		}
		apiVersion, _ := document["apiVersion"].(string)
		kind, _ := document["kind"].(string)
		if stringAt(document, "metadata", "name") == "" {
			problems = append(problems, fmt.Sprintf("%s: %s has no metadata.name", where, kind))
		}
		switch {
		case apiVersion == "operator.ptah.run/v1alpha1" && crds[kind] != "":
			schema, props, whole := referenceSpecSchema(t, crds[kind])
			spec, ok := document["spec"].(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: %s carries no spec", where, kind))
				continue
			}
			for _, unknown := range unknownFields(schema, spec) {
				problems = append(problems, fmt.Sprintf("%s: %s names %s, which the API does not have", where, kind, unknown))
			}
			// A demonstration manifest is applied as it is, so it is complete.
			for _, failure := range schemaFailures(t, props, spec, true) {
				problems = append(problems, fmt.Sprintf("%s: %s: %s", where, kind, failure))
			}
			for _, failure := range celFailures(t, whole, document) {
				problems = append(problems, fmt.Sprintf("%s: %s: %s", where, kind, failure))
			}
		case apiVersion == "batch/v1" && kind == "Job":
			// A strict decode refuses a field the type does not have, which is
			// what a misspelled field in a Pod template is.
			var job batchv1.Job
			if err := yaml.UnmarshalStrict(chunk, &job); err != nil {
				problems = append(problems, fmt.Sprintf("%s: Job: %v", where, err))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s: %s %s is a kind this test cannot check", where, apiVersion, kind))
		}
	}
}

// The check has to refuse what it was written for: a field the API does not
// have, in a Ptah resource and in a Job, a value the CRD's own rules refuse,
// and a kind it would otherwise pass over unread.
func TestTheDemoManifestCheckRefusesABrokenManifest(t *testing.T) {
	t.Parallel()

	sound, err := os.ReadFile(repositoryFile(t, filepath.Join(demoManifestsDir, "manual-approval", "storefront.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	job, err := os.ReadFile(repositoryFile(t, filepath.Join(demoManifestsDir, "adopt-existing-database", "baseline-v1.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	for _, accepted := range [][]byte{sound, job} {
		if documents, problems := demoManifestProblems(t, accepted); documents != 1 || len(problems) > 0 {
			t.Fatalf("a sound manifest read as %d documents with %v", documents, problems)
		}
	}

	for _, row := range []struct {
		name     string
		manifest []byte
		refusal  string
	}{
		{
			name:     "a field the CRD does not have",
			manifest: replaceOnce(t, sound, "  interval: 1m\n", "  interval: 1m\n  intervall: 1m\n"),
			refusal:  "spec.intervall",
		},
		{
			name:     "a value outside its enum",
			manifest: replaceOnce(t, sound, "apply: OnApproval", "apply: Sometimes"),
			refusal:  "spec.policy.apply",
		},
		{
			name:     "a value a CEL rule refuses",
			manifest: replaceOnce(t, sound, "  interval: 1m\n", "  interval: 1s\n"),
			refusal:  "interval",
		},
		{
			name:     "a field a Job does not have",
			manifest: replaceOnce(t, job, "  backoffLimit: 0\n", "  backofLimit: 0\n"),
			refusal:  "backofLimit",
		},
		{
			name:     "a kind the check does not read",
			manifest: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: storefront-v1\n"),
			refusal:  "cannot check",
		},
		{
			name:     "a second document that is broken",
			manifest: append(append(append([]byte{}, sound...), "---\n"...), replaceOnce(t, sound, "apply: OnApproval", "apply: Sometimes")...),
			refusal:  "document 2",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, problems := demoManifestProblems(t, row.manifest)
			if !strings.Contains(strings.Join(problems, "\n"), row.refusal) {
				t.Fatalf("the check said %v, which does not name %q", problems, row.refusal)
			}
		})
	}
}

func replaceOnce(t *testing.T, text []byte, old, replacement string) []byte {
	t.Helper()
	if count := bytes.Count(text, []byte(old)); count != 1 {
		t.Fatalf("the fixture holds %q %d times, want once", old, count)
	}
	return bytes.Replace(text, []byte(old), []byte(replacement), 1)
}
