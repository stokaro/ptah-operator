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
	"sort"
)

// historyRestart is the one place the schema version may go down.
//
// Until the first tagged release the version counted commits: nobody could
// install a release holding any of those versions, so the history restarts at
// 1 in the change that precedes the v0.1.0 tag, and version 1 is the first
// entry of the history a release is upgraded from.
//
// A restart is scoped more tightly than a declared break. It names the
// baseline it leaves by its version and by the schema digest of every CRD in
// it, so no other tree can present it: a later baseline carries other
// digests, and a history that climbs back to the same number would have to
// reproduce these schemas byte for byte. It admits one candidate version, 1.
// And it names every transition the reset makes that reaches a stored object,
// exactly as the compatibility check renders it, so a change that rides along
// with the reset is refused as it would be anywhere else, and so is a
// declaration the candidate does not bear out.
//
// Restarting also ends every declared break: a declaration is scoped to a
// version number, and the numbers it named belong to the history that ended.
type historyRestart struct {
	// fromVersion is the baseline version the restart leaves.
	fromVersion uint64
	// fromDigests is the normalized schema digest of every CRD at that
	// baseline, by CRD name.
	fromDigests map[string]string
	// reason says why the history restarted.
	reason string
	// transitions are the stored-object transitions the reset makes.
	transitions []string
}

// historyRestarts lists every restart of the schema history. There is one,
// and a second is not expected: after the first tag a version moves only
// when a schema a tagged release could have stored changes.
var historyRestarts = []historyRestart{
	{
		fromVersion: 32,
		fromDigests: map[string]string{
			"ptahmigrationapprovals.operator.ptah.run":          "sha256:9391303b4f722d892389f53e9f8f4f7853e77547d9bdf4c02202c2801382c897",
			"ptahmigrationplans.operator.ptah.run":              "sha256:bd3dd4a8c79182fec93e420242d2246aabf1d5e5fb78898c369a944ad489a245",
			"ptahmigrationrunacknowledgments.operator.ptah.run": "sha256:158a67e8ea4e64b1cbe7678abb49bafd0eb807f4672a0ec4e540c99332e6c49f",
			"ptahmigrations.operator.ptah.run":                  "sha256:681554fea2f34a5565c9801608b4148614f110083d5b5ca02aeafe3bfe2596a0",
			"ptahrealms.operator.ptah.run":                      "sha256:8c001146bf02b378911dae351760f511fc465d7dafcda94e849b14d5b329d2fa",
			"ptahschemaapprovals.operator.ptah.run":             "sha256:269e21ffd731b1eb2d6f0964122f7eead2f1d29ecc6e550df5740e65ae1ef663",
			"ptahschemaplanchunks.operator.ptah.run":            "sha256:f762f55f2bdd3d7766ce5962a1a35f074ada6992016f454e26e4b06dbb746d0f",
			"ptahschemaplans.operator.ptah.run":                 "sha256:2f397e8afd58dbcb23c7023262696c3b091ac76581c88f5909f4940412f79774",
			"ptahschemas.operator.ptah.run":                     "sha256:b748c135a91ab9f47252aba72d8b556d301e2cec6d505c3caa4ed6f2eb8736fe",
		},
		reason: "Every contract counter goes back to 1 just before v0.1.0. Versions 1 to 32 counted " +
			"commits; no release carried any of them. The plan contract restarts with the schema, " +
			"so PtahSchemaPlan's contractVersion enum moves from 3 to 1: no stored plan can hold 3, " +
			"because nothing that stored one was ever released.",
		transitions: []string{
			"ptahschemaplans.operator.ptah.run: spec.contractVersion: the enum lost 3",
		},
	},
}

// restartFrom returns the restart that leaves the given baseline version.
func restartFrom(restarts []historyRestart, baselineVersion uint64) (historyRestart, bool) {
	for _, restart := range restarts {
		if restart.fromVersion == baselineVersion {
			return restart, true
		}
	}
	return historyRestart{}, false
}

// admit holds a transition that moves the version below its baseline to the
// restart that leaves that baseline.
func (r historyRestart) admit(baseline, candidate documentSet, candidateVersion uint64) error {
	if candidateVersion != 1 {
		return fmt.Errorf(
			"the schema history restarts from version %d only at version 1, and the candidate is version %d",
			r.fromVersion, candidateVersion,
		)
	}
	names := make([]string, 0, len(baseline.byName))
	for name := range baseline.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) != len(r.fromDigests) {
		return fmt.Errorf(
			"the schema history restarts from a baseline of %d CRDs, and this baseline carries %d",
			len(r.fromDigests), len(names),
		)
	}
	for _, name := range names {
		want, recorded := r.fromDigests[name]
		got := digestSpec(baseline.byName[name].normalizedSpec)
		if !recorded || got != want {
			return fmt.Errorf(
				"the schema history restarts from version %d only from the tree it records: baseline CRD %s has digest %s, and the restart records %q",
				r.fromVersion, name, got, want,
			)
		}
	}
	// The reset is held to the stored-object check like any other change,
	// with the restart's own transitions standing in for a declared break.
	declared := []declaredBreak{{version: candidateVersion, reason: r.reason, transitions: r.transitions}}
	if err := verifyStoredObjectCompatibility(baseline, candidate, candidateVersion, declared); err != nil {
		return fmt.Errorf("restart from schema version %d: %w", r.fromVersion, err)
	}
	return nil
}

// validateRestarts refuses a restart list that could admit more than it says:
// a restart with no baseline to leave, or one that does not name the complete
// generated set, a transition, or a reason.
func validateRestarts(restarts []historyRestart) error {
	seen := make(map[uint64]bool, len(restarts))
	for _, restart := range restarts {
		if restart.fromVersion < 2 {
			return fmt.Errorf("a restart leaves schema version %d, and only a version above 1 can restart at 1", restart.fromVersion)
		}
		if seen[restart.fromVersion] {
			return fmt.Errorf("schema version %d is restarted more than once", restart.fromVersion)
		}
		seen[restart.fromVersion] = true
		if restart.reason == "" {
			return fmt.Errorf("the restart from schema version %d gives no reason", restart.fromVersion)
		}
		names := requiredCRDNames()
		if len(restart.fromDigests) != len(names) {
			return fmt.Errorf("the restart from schema version %d records %d CRD digests, want the %d generated CRDs",
				restart.fromVersion, len(restart.fromDigests), len(names))
		}
		for _, name := range names {
			digest, recorded := restart.fromDigests[name]
			if !recorded {
				return fmt.Errorf("the restart from schema version %d records no digest for %s", restart.fromVersion, name)
			}
			if err := validateDigest(digest); err != nil {
				return fmt.Errorf("the restart from schema version %d records %s: %w", restart.fromVersion, name, err)
			}
		}
		seenTransitions := make(map[string]bool, len(restart.transitions))
		for _, transition := range restart.transitions {
			if transition == "" || seenTransitions[transition] {
				return fmt.Errorf("the restart from schema version %d names an empty or repeated transition %q",
					restart.fromVersion, transition)
			}
			seenTransitions[transition] = true
		}
	}
	return nil
}
