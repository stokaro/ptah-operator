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
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	restartPlanCRD = "ptahschemaplans.operator.ptah.run"
	// lostContract is what the compatibility check renders for the reset of
	// the plan contract enum, which the fixture restart declares.
	lostContract = restartPlanCRD + ": spec.contractVersion: the enum lost 3"
)

// restartSchema describes the plan CRD's spec in a fixture set: the contract
// enum, and whether a second field has become required.
type restartSchema struct {
	contracts       []string
	requireDialect  bool
	otherPlanFields bool
}

// restartFixtureSet is the generated CRD set at one schema version, with the
// plan CRD's spec shaped by schema and every other CRD left alike.
func restartFixtureSet(t *testing.T, version uint64, schema restartSchema) documentSet {
	t.Helper()
	documents := make(map[string][]byte, len(restartCRDNames()))
	for index, name := range restartCRDNames() {
		crd := fixtureCRD(name, "same")
		if name == restartPlanCRD {
			spec := object(field("contractVersion", enum(schema.contracts...)), field("dialect", text()))
			if schema.requireDialect {
				spec.Required = []string{"dialect"}
			}
			if schema.otherPlanFields {
				spec.Properties["note"] = text()
			}
			root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			root.Properties = map[string]apiextensionsv1.JSONSchemaProps{"spec": spec}
		}
		stampFixtureIdentity(t, crd, version)
		documents[fmt.Sprintf("crd-%d.yaml", index)] = marshalFixtureCRD(t, crd)
	}
	set, err := decodeSetAllowingSubset("historical fixture", documents)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// restartLeaving records a restart from exactly the tree baseline holds.
func restartLeaving(baseline documentSet, version uint64, transitions ...string) historyRestart {
	digests := make(map[string]string, len(baseline.byName))
	for name, document := range baseline.byName {
		digests[name] = digestSpec(document.normalizedSpec)
	}
	return historyRestart{fromVersion: version, fromDigests: digests, reason: "fixture", transitions: transitions}
}

// The restart admits the one transition it records, and each refused row
// beside it differs from that transition in one thing: the candidate version,
// the baseline version, the baseline tree, a break the restart does not name,
// or a break it names and the candidate does not make. The rows after those
// are the history that follows the restart, where the gate is what it always
// was.
func TestARestartAdmitsOnlyTheTransitionItRecords(t *testing.T) {
	t.Parallel()

	current := restartSchema{contracts: []string{"3"}}
	reset := restartSchema{contracts: []string{"1"}}
	baseline := restartFixtureSet(t, 32, current)
	recorded := restartLeaving(baseline, 32, lostContract)
	otherTree := restartLeaving(restartFixtureSet(t, 32, restartSchema{contracts: []string{"3"}, otherPlanFields: true}), 32, lostContract)

	for _, test := range []struct {
		name          string
		baseline      documentSet
		candidate     documentSet
		restarts      []historyRestart
		wantError     string
		wantRestarted bool
	}{
		{
			name:     "the recorded restart",
			baseline: baseline, candidate: restartFixtureSet(t, 1, reset),
			restarts: []historyRestart{recorded}, wantRestarted: true,
		},
		{
			name:     "no restart recorded",
			baseline: baseline, candidate: restartFixtureSet(t, 1, reset),
			wantError: "must strictly increase baseline version 32",
		},
		{
			name:     "a candidate past version 1",
			baseline: baseline, candidate: restartFixtureSet(t, 2, reset),
			restarts: []historyRestart{recorded}, wantError: "only at version 1, and the candidate is version 2",
		},
		{
			name:     "a baseline at another version",
			baseline: restartFixtureSet(t, 31, current), candidate: restartFixtureSet(t, 1, reset),
			restarts: []historyRestart{recorded}, wantError: "must strictly increase baseline version 31",
		},
		{
			name:     "another tree at the recorded version",
			baseline: baseline, candidate: restartFixtureSet(t, 1, reset),
			restarts: []historyRestart{otherTree}, wantError: "only from the tree it records",
		},
		{
			name:     "a break the restart does not name",
			baseline: baseline, candidate: restartFixtureSet(t, 1, restartSchema{contracts: []string{"1"}, requireDialect: true}),
			restarts:  []historyRestart{recorded},
			wantError: restartPlanCRD + ": spec.dialect: was optional and the candidate requires it",
		},
		{
			name:     "a break the restart names and the candidate does not make",
			baseline: baseline, candidate: restartFixtureSet(t, 1, restartSchema{contracts: []string{"3", "1"}}),
			restarts:  []historyRestart{recorded},
			wantError: "names transitions the candidate does not make:\n  " + lostContract,
		},
		{
			name:     "after the restart, a change that keeps version 1",
			baseline: restartFixtureSet(t, 1, reset), candidate: restartFixtureSet(t, 1, restartSchema{contracts: []string{"1", "2"}}),
			restarts: []historyRestart{recorded}, wantError: "must strictly increase baseline version 1",
		},
		{
			name:     "after the restart, a tightening at version 2",
			baseline: restartFixtureSet(t, 1, reset), candidate: restartFixtureSet(t, 2, restartSchema{contracts: []string{"2"}}),
			restarts: []historyRestart{recorded}, wantError: restartPlanCRD + ": spec.contractVersion: the enum lost 1",
		},
		{
			name:     "after the restart, version 2 back to 1",
			baseline: restartFixtureSet(t, 2, reset), candidate: restartFixtureSet(t, 1, reset),
			restarts: []historyRestart{recorded}, wantError: "must equal baseline version 2",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := evaluateTransitionWith(test.baseline, test.candidate, nil, test.restarts)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("evaluateTransitionWith() error = %v, want it to name %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Restarted != test.wantRestarted || result.InitialAdoption {
				t.Fatalf("result = %+v, want Restarted %t", result, test.wantRestarted)
			}
		})
	}
}

// The recorded restarts are well formed, and the check that says so is first
// shown the records it has to refuse.
func TestHistoryRestartsAreWellFormed(t *testing.T) {
	t.Parallel()

	complete := func() historyRestart {
		digests := make(map[string]string, len(restartCRDNames()))
		for _, name := range restartCRDNames() {
			digests[name] = "sha256:" + strings.Repeat("a", 64)
		}
		return historyRestart{fromVersion: 32, fromDigests: digests, reason: "why", transitions: []string{lostContract}}
	}
	for name, mutate := range map[string]func(*historyRestart){
		"leaving version 1":   func(r *historyRestart) { r.fromVersion = 1 },
		"no reason":           func(r *historyRestart) { r.reason = "" },
		"a CRD it leaves out": func(r *historyRestart) { delete(r.fromDigests, restartPlanCRD) },
		"a CRD not generated": func(r *historyRestart) {
			r.fromDigests["other.operator.ptah.run"] = "sha256:" + strings.Repeat("a", 64)
		},
		"a digest that is not": func(r *historyRestart) { r.fromDigests[restartPlanCRD] = "sha256:abc" },
		"a repeated transition": func(r *historyRestart) {
			r.transitions = append(r.transitions, lostContract)
		},
	} {
		restart := complete()
		mutate(&restart)
		if err := validateRestarts([]historyRestart{restart}); err == nil {
			t.Errorf("%s: the restart was accepted", name)
		}
	}
	if err := validateRestarts([]historyRestart{complete(), complete()}); err == nil {
		t.Error("two restarts from one version were accepted")
	}
	if err := validateRestarts([]historyRestart{complete()}); err != nil {
		t.Fatalf("a well-formed restart was refused: %v", err)
	}

	// The history restarted once, before the first tag.
	if len(historyRestarts) != 1 {
		t.Fatalf("%d restarts are recorded, want the one before v0.1.0", len(historyRestarts))
	}
	if err := validateRestarts(historyRestarts); err != nil {
		t.Fatal(err)
	}
}
