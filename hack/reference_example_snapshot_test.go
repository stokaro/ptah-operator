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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The reference shows a plan and then the approval admitted against it. Those
// two documents describe one execution, and admission refuses an approval
// whose bindings disagree with the plan it names -- so an example labeled
// "after admission" that carries a different artifact digest is not a
// substituted identifier, it is a state the cluster would have refused.
//
// Each check below reads an authority rather than restating one: the plan the
// approval names, the exported name derivation, the runner's own protocol
// constant, the manager's own state version, and the release catalog.
const ptahCatalogPath = "support/ptah.json"

// bindingsAnApprovalRepeats are the fields an approval carries from its plan.
// A field either side omits is not compared; a field both carry must agree.
var bindingsAnApprovalRepeats = []string{
	"artifactDigest",
	"verificationPolicyDigest",
	"desiredStateFingerprint",
	"actualStateFingerprint",
	"historyFingerprint",
	"coordinationDigest",
	"targetIdentityDigest",
	"policyFingerprint",
	"ptahVersion",
	"executorImage",
	"runnerImage",
	"runnerProtocolVersion",
	"controllerImage",
	"controllerStateVersion",
}

func TestEveryApprovalExampleAgreesWithThePlanItNames(t *testing.T) {
	t.Parallel()

	plans := planExamplesByName(t)
	if len(plans) == 0 {
		t.Fatal("no plan examples were read, so this check compares nothing")
	}
	compared := 0
	for _, kind := range referenceExampleKinds {
		documents, err := referenceExampleDocuments(repositoryFile(t, filepath.Join(referenceExamplesDir, strings.ToLower(kind.Kind)+".md")))
		if err != nil {
			t.Fatal(err)
		}
		for index, document := range documents {
			spec, _ := document["spec"].(map[string]any)
			reference, _ := spec["planRef"].(map[string]any)
			named, _ := reference["name"].(string)
			if named == "" {
				continue
			}
			plan, known := plans[named]
			if !known {
				t.Fatalf("%s example %d approves plan %q, which no plan example describes",
					kind.Kind, index+1, named)
			}
			for _, binding := range bindingsAnApprovalRepeats {
				approved, carried := spec[binding]
				planned, published := plan[binding]
				if !carried || !published {
					continue
				}
				if fmt.Sprint(approved) != fmt.Sprint(planned) {
					t.Fatalf("%s example %d carries %s %v while plan %s publishes %v, which admission refuses",
						kind.Kind, index+1, binding, approved, named, planned)
				}
				compared++
			}
		}
	}
	if compared == 0 {
		t.Fatal("no approval example repeated a binding from its plan, so this check measures nothing")
	}
}

// The runtime an example shows has to be the one this operator supports, or
// the snapshot describes a cluster nobody can build.
func TestEveryExampleShowsTheSupportedRuntime(t *testing.T) {
	t.Parallel()

	supported := map[string]string{
		"runnerProtocolVersion":  fmt.Sprint(runner.ProtocolVersion),
		"controllerStateVersion": fmt.Sprint(controllerstate.CurrentVersion),
		"ptahVersion":            verifiedPtahRelease(t),
	}
	seen := map[string]int{}
	for _, kind := range referenceExampleKinds {
		documents, err := referenceExampleDocuments(repositoryFile(t, filepath.Join(referenceExamplesDir, strings.ToLower(kind.Kind)+".md")))
		if err != nil {
			t.Fatal(err)
		}
		for index, document := range documents {
			spec, _ := document["spec"].(map[string]any)
			for field, want := range supported {
				shown, carried := spec[field]
				if !carried {
					continue
				}
				if fmt.Sprint(shown) != want {
					t.Fatalf("%s example %d shows %s %v, and this operator supports %s",
						kind.Kind, index+1, field, shown, want)
				}
				seen[field]++
			}
		}
	}
	for field := range supported {
		if seen[field] == 0 {
			t.Fatalf("no example shows %s, so this check reads nothing about it", field)
		}
	}
}

// A plan example has to be named the way the publisher names one, and carry
// its chunks the way the publisher writes them.
func TestEveryPlanExampleIsNamedAndChunkedLikeThePublisher(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, kind := range referenceExampleKinds {
		documents, err := referenceExampleDocuments(repositoryFile(t, filepath.Join(referenceExamplesDir, strings.ToLower(kind.Kind)+".md")))
		if err != nil {
			t.Fatal(err)
		}
		for index, document := range documents {
			spec, _ := document["spec"].(map[string]any)
			fingerprint, _ := spec["fingerprint"].(string)
			metadata, _ := document["metadata"].(map[string]any)
			name, _ := metadata["name"].(string)
			if fingerprint == "" || name == "" || document["kind"] == "PtahSchema" {
				continue
			}
			if document["kind"] == "PtahMigrationPlan" {
				want, nameErr := migrationplan.Name(fingerprint)
				if nameErr != nil {
					t.Fatalf("%s example %d: %v", kind.Kind, index+1, nameErr)
				}
				if name != want {
					t.Fatalf("%s example %d is named %q; the publisher derives %q from its fingerprint",
						kind.Kind, index+1, name, want)
				}
				checked++
			}
			chunks, _ := spec["chunks"].([]any)
			for chunkIndex, entry := range chunks {
				chunk, _ := entry.(map[string]any)
				wantName := fmt.Sprintf("%s-%03d", name, chunkIndex)
				if got, _ := chunk["name"].(string); got != wantName {
					t.Fatalf("%s example %d chunk %d is named %q, and the publisher writes %q",
						kind.Kind, index+1, chunkIndex, got, wantName)
				}
				if got, _ := chunk["key"].(string); got != planstore.ChunkDataKey {
					t.Fatalf("%s example %d chunk %d uses key %q, and the publisher writes %q",
						kind.Kind, index+1, chunkIndex, got, planstore.ChunkDataKey)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no plan example was named or chunked, so this check measures nothing")
	}
}

// planExamplesByName is every plan document the reference shows, keyed by the
// name an approval would reference.
func planExamplesByName(t *testing.T) map[string]map[string]any {
	t.Helper()

	plans := map[string]map[string]any{}
	for _, kind := range referenceExampleKinds {
		documents, err := referenceExampleDocuments(repositoryFile(t, filepath.Join(referenceExamplesDir, strings.ToLower(kind.Kind)+".md")))
		if err != nil {
			t.Fatal(err)
		}
		for _, document := range documents {
			declared, _ := document["kind"].(string)
			if declared != "PtahSchemaPlan" && declared != "PtahMigrationPlan" {
				continue
			}
			metadata, _ := document["metadata"].(map[string]any)
			name, _ := metadata["name"].(string)
			spec, _ := document["spec"].(map[string]any)
			if name != "" && spec != nil {
				plans[name] = spec
			}
		}
	}
	return plans
}

// verifiedPtahRelease is the release the catalog says the matrix exercised.
func verifiedPtahRelease(t *testing.T) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join("..", ptahCatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	// A tagged row names the release; an edge row advanced to an untagged
	// commit carries a null there and identifies the build by what git
	// describe reported, which is the same string the lifecycle binds.
	var catalog struct {
		Releases []struct {
			Verified []struct {
				PtahRelease  string `json:"ptahRelease"`
				PtahDescribe string `json:"ptahDescribe"`
			} `json:"verified"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(contents, &catalog); err != nil {
		t.Fatalf("parse %s: %v", ptahCatalogPath, err)
	}
	var verified []string
	for _, release := range catalog.Releases {
		for _, row := range release.Verified {
			identity := row.PtahRelease
			if identity == "" {
				identity = row.PtahDescribe
			}
			if identity == "" {
				t.Fatalf("%s verifies a build that names neither a release nor a describe", ptahCatalogPath)
			}
			verified = append(verified, identity)
		}
	}
	if len(verified) != 1 {
		t.Fatalf("%s verifies %d Ptah releases; this check needs the one the examples show: %v",
			ptahCatalogPath, len(verified), verified)
	}
	return verified[0]
}
