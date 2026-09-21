package examples_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The examples run together against one database, and each verifies a
// different artifact type. Pointing both at one policy is not a preference: a
// policy naming only the schema type refuses the migration's artifact, so a
// reader who follows the setup ends up with a migration that cannot verify.
//
// The guide said to give a migration its own policy while the shipped example
// reused the schema's, which is the shape of defect neither file shows on its
// own. This reads the ConfigMap the documentation creates and the name the
// example asks for, and checks they describe the same thing.
type policyDocument struct {
	// utilyaml converts to JSON first, so these are the tags it reads.
	Version       int      `json:"version"`
	ArtifactTypes []string `json:"artifact_types"`
}

// createdPolicies maps the ConfigMap name a guide creates to the file it
// creates it from, read out of the guides rather than restated here.
var configMapCreate = regexp.MustCompile(
	`create configmap ([a-z0-9-]+) \\\n\s*--from-file=policy\.yaml=examples/([a-z0-9-]+\.yaml)`)

func TestEachExampleVerifiesAgainstAPolicyThatAdmitsItsArtifact(t *testing.T) {
	t.Parallel()

	created := createdPolicies(t)

	for _, row := range []struct {
		example string
		admits  string
		refuses string
	}{
		{
			example: "ptahschema.yaml",
			admits:  dataplane.SchemaArtifactType,
			refuses: dataplane.MigrationArtifactType,
		},
		{
			example: "ptahmigration.yaml",
			admits:  dataplane.MigrationArtifactType,
			refuses: dataplane.SchemaArtifactType,
		},
	} {
		t.Run(row.example, func(t *testing.T) {
			t.Parallel()

			name := policyReferenceOf(t, row.example)
			file, documented := created[name]
			if !documented {
				t.Fatalf("%s verifies against ConfigMap %q, which no guide creates from an examples file: %v",
					row.example, name, created)
			}
			policy := readPolicy(t, file)
			if !slices.Contains(policy.ArtifactTypes, row.admits) {
				t.Fatalf("%s needs %s and %s admits %v, so following the setup leaves it unable to verify",
					row.example, row.admits, file, policy.ArtifactTypes)
			}
			if slices.Contains(policy.ArtifactTypes, row.refuses) {
				t.Fatalf("%s admits %s as well, so the other kind's artifact could stand in at the same reference",
					file, row.refuses)
			}
		})
	}
}

// policyReferenceOf is the policy name an example asks for, whichever kind it
// declares.
func policyReferenceOf(t *testing.T, example string) string {
	t.Helper()

	contents, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	var schema operatorv1alpha1.PtahSchema
	if err := utilyaml.Unmarshal(contents, &schema); err == nil && schema.Kind == "PtahSchema" {
		return schema.Spec.Desired.VerificationPolicyFrom.Name
	}
	var migration operatorv1alpha1.PtahMigration
	if err := utilyaml.Unmarshal(contents, &migration); err != nil {
		t.Fatalf("parse %s: %v", example, err)
	}
	if migration.Kind != "PtahMigration" {
		t.Fatalf("%s declares kind %q, which this check does not know", example, migration.Kind)
	}
	return migration.Spec.Artifact.VerificationPolicyFrom.Name
}

// createdPolicies reads every documented create command so the mapping is the
// reader's, not this test's.
func createdPolicies(t *testing.T) map[string]string {
	t.Helper()

	created := map[string]string{}
	guides, err := filepath.Glob(filepath.Join("..", "docs", "site", "src", "content", "docs", "*", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, guide := range guides {
		contents, readErr := os.ReadFile(guide)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, match := range configMapCreate.FindAllStringSubmatch(string(contents), -1) {
			created[match[1]] = match[2]
		}
	}
	if len(created) == 0 {
		t.Fatal("no guide creates a verification policy ConfigMap from an examples file, so this check reads nothing")
	}
	return created
}

func readPolicy(t *testing.T, file string) policyDocument {
	t.Helper()

	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var policy policyDocument
	if err := utilyaml.Unmarshal(contents, &policy); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	if policy.Version == 0 || len(policy.ArtifactTypes) == 0 {
		t.Fatalf("%s names no version or no artifact types: %#v", file, policy)
	}
	return policy
}
