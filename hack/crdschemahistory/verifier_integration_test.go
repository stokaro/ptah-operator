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

package crdschemahistory_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/stokaro/ptah-operator/hack/crdschemahistory"
)

const (
	fixtureVersionAnnotation = "operator.ptah.dev/crd-schema-version"
	fixtureDigestAnnotation  = "operator.ptah.dev/crd-schema-digest"
)

var fixtureNames = []string{
	"ptahschemaapprovals.operator.ptah.dev",
	"ptahschemaplans.operator.ptah.dev",
	"ptahschemas.operator.ptah.dev",
}

func TestVerifyUsesExplicitAndLocalAutomaticBaselines(t *testing.T) {
	t.Parallel()

	repository := newFixtureRepository(t)
	writeFixtureSet(t, repository, false, 0, "baseline")
	commitFixture(t, repository, "baseline")
	baselineCommit := gitFixtureOutput(t, repository, "rev-parse", "HEAD")

	writeFixtureSet(t, repository, true, 1, "candidate")
	explicit, err := crdschemahistory.Verify(t.Context(), crdschemahistory.Config{
		Root:        repository,
		BaselineRef: baselineCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.BaselineCommit != baselineCommit || explicit.BaselineSource != "explicit" || !explicit.InitialAdoption {
		t.Fatalf("explicit result = %+v", explicit)
	}

	dirty, err := crdschemahistory.Verify(t.Context(), crdschemahistory.Config{Root: repository})
	if err != nil {
		t.Fatal(err)
	}
	if dirty.BaselineCommit != baselineCommit || dirty.BaselineSource != "working-tree-head" {
		t.Fatalf("dirty-tree result = %+v", dirty)
	}

	commitFixture(t, repository, "candidate")
	clean, err := crdschemahistory.Verify(t.Context(), crdschemahistory.Config{Root: repository})
	if err != nil {
		t.Fatal(err)
	}
	if clean.BaselineCommit != baselineCommit || clean.BaselineSource != "clean-parent" {
		t.Fatalf("clean-tree result = %+v", clean)
	}
}

func TestVerifyAllowsOnlyVersionOneBootstrapFromEmptyBaseline(t *testing.T) {
	t.Parallel()

	repository := newFixtureRepository(t)
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("bootstrap baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	execGitFixture(t, repository, "add", "README.md")
	execGitFixture(t, repository, "commit", "--quiet", "-m", "empty CRD baseline")
	baselineCommit := gitFixtureOutput(t, repository, "rev-parse", "HEAD")

	writeFixtureSet(t, repository, true, 1, "candidate")
	result, err := crdschemahistory.Verify(t.Context(), crdschemahistory.Config{
		Root:        repository,
		BaselineRef: baselineCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BaselineVersion != 0 || result.CandidateVersion != 1 ||
		!result.InitialAdoption || !result.SchemaChanged {
		t.Fatalf("empty-baseline verification result = %+v", result)
	}

	writeFixtureSet(t, repository, true, 2, "later candidate")
	_, err = crdschemahistory.Verify(t.Context(), crdschemahistory.Config{
		Root:        repository,
		BaselineRef: baselineCommit,
	})
	if err == nil || !strings.Contains(err.Error(), "initial bootstrap from an empty CRD baseline requires candidate") {
		t.Fatalf("version-two empty-baseline verification error = %v", err)
	}
}

func TestVerifyFailsClosedForMissingOrInvalidExplicitBaseline(t *testing.T) {
	t.Parallel()

	repository := newFixtureRepository(t)
	writeFixtureSet(t, repository, false, 0, "baseline")
	commitFixture(t, repository, "baseline")
	writeFixtureSet(t, repository, true, 1, "candidate")

	_, err := crdschemahistory.Verify(t.Context(), crdschemahistory.Config{
		Root:                    repository,
		RequireExplicitBaseline: true,
	})
	if err == nil || !strings.Contains(err.Error(), "an explicit baseline is required") {
		t.Fatalf("missing explicit baseline error = %v", err)
	}

	_, err = crdschemahistory.Verify(t.Context(), crdschemahistory.Config{
		Root:                    repository,
		BaselineRef:             strings.Repeat("f", 40),
		RequireExplicitBaseline: true,
	})
	if err == nil || !strings.Contains(err.Error(), "resolve explicit baseline") {
		t.Fatalf("invalid explicit baseline error = %v", err)
	}
}

func newFixtureRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	execGitFixture(t, repository, "init", "--quiet")
	execGitFixture(t, repository, "config", "user.name", "CRD history test")
	execGitFixture(t, repository, "config", "user.email", "crd-history@example.invalid")
	execGitFixture(t, repository, "config", "commit.gpgsign", "false")
	return repository
}

func writeFixtureSet(t *testing.T, repository string, managed bool, version uint64, description string) {
	t.Helper()
	directory := filepath.Join(repository, "config", "crd", "bases")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for index, name := range fixtureNames {
		plural := strings.Split(name, ".")[0]
		kind := map[string]string{
			"ptahschemas.operator.ptah.dev":         "PtahSchema",
			"ptahschemaapprovals.operator.ptah.dev": "PtahSchemaApproval",
			"ptahschemaplans.operator.ptah.dev":     "PtahSchemaPlan",
		}[name]
		crd := &apiextensionsv1.CustomResourceDefinition{
			TypeMeta: metav1.TypeMeta{
				APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
				Kind:       "CustomResourceDefinition",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "operator.ptah.dev",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:   plural,
					Singular: strings.TrimSuffix(plural, "s"),
					Kind:     kind,
					ListKind: kind + "List",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:        "object",
							Description: description,
						},
					},
				}},
			},
		}
		if managed {
			digest, err := fixtureDigest(crd)
			if err != nil {
				t.Fatal(err)
			}
			crd.Annotations = map[string]string{
				fixtureVersionAnnotation: fmt.Sprintf("%d", version),
				fixtureDigestAnnotation:  digest,
			}
		}
		contents, err := yaml.Marshal(crd)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, fmt.Sprintf("crd-%d.yaml", index))
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureDigest(crd *apiextensionsv1.CustomResourceDefinition) (string, error) {
	normalized := &apiextensionsv1.CustomResourceDefinition{Spec: *crd.Spec.DeepCopy()}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(normalized)
	encoded, err := json.Marshal(normalized.Spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum), nil
}

func commitFixture(t *testing.T, repository, message string) {
	t.Helper()
	execGitFixture(t, repository, "add", "config/crd/bases")
	execGitFixture(t, repository, "commit", "--quiet", "-m", message)
}

func gitFixtureOutput(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = repository
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", arguments[0], err, output)
	}
	return strings.TrimSpace(string(output))
}

func execGitFixture(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	_ = gitFixtureOutput(t, repository, arguments...)
}
