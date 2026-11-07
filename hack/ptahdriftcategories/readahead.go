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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// ReadAheadDeclaration names fixed categories the operator accepts before the
// Ptah pinned in support/ptah.json emits them.
//
// The vendored file stays the pinned build's list, because CI holds it to the
// pinned source. A release the operator prepares for before the pin moves can
// add categories, and an operator that refused them would fail Observe for
// every change they describe on the day the pin moves. The declaration names
// the Ptah commit the categories come from and the digest of that commit's
// docs/drift-findings.json; the unit tests hold the operator's vocabulary to
// the vendored list plus these categories, and hold the declaration to a copy
// of that file under testdata.
//
// The repin empties it: once the vendored file is written from a commit that
// emits a declared category, the tests refuse the declaration until the
// category leaves it.
type ReadAheadDeclaration struct {
	// PtahCommit is the Ptah commit whose vocabulary lists the categories.
	PtahCommit string
	// VocabularySHA256 is the digest of docs/drift-findings.json at that
	// commit.
	VocabularySHA256 string
	// Vocabulary is the repository path of a byte-for-byte copy of that file.
	Vocabulary string
	// Categories are the fixed categories that commit adds to the pinned list,
	// sorted.
	Categories []string
}

// ReadAhead is Ptah v0.23.0's vocabulary, measured at the commit the release
// is to be tagged on. It adds a category for a column whose type narrows and
// one for a column that loses its identity.
var ReadAhead = ReadAheadDeclaration{
	PtahCommit:       "f47b2bfa3b696c93730e7d25397987ed0a4a2562",
	VocabularySHA256: "2e304874143613792ed92cf9f0a5d4dd29999e2aa2ed709c8d92a7d9be4ca868",
	Vocabulary:       "testdata/ptah-f47b2bfa3/drift-findings.json",
	Categories:       []string{"column_identities_removed", "column_types_narrowed"},
}

// CheckReadAhead refuses a declaration that does not describe content, the
// read-ahead commit's docs/drift-findings.json, against the vendored pinned
// list: a digest that differs, categories that are unsorted or repeated, a
// category the pinned build already emits, and a vocabulary that differs from
// the pinned one by anything other than the declared fixed categories. The last
// one keeps the declaration from hiding a category the newer build renames,
// drops or qualifies.
func CheckReadAhead(vendored Vendored, declaration ReadAheadDeclaration, content []byte) error {
	if len(declaration.Categories) == 0 {
		if declaration.PtahCommit != "" || declaration.VocabularySHA256 != "" || declaration.Vocabulary != "" {
			return errors.New("the read-ahead declaration names a commit and no category")
		}
		return nil
	}
	if !commitPattern.MatchString(declaration.PtahCommit) {
		return fmt.Errorf("read-ahead commit %q is not an exact commit", declaration.PtahCommit)
	}
	if !slices.IsSorted(declaration.Categories) || len(slices.Compact(slices.Clone(declaration.Categories))) != len(declaration.Categories) {
		return fmt.Errorf("read-ahead categories %q are not sorted and unique", declaration.Categories)
	}
	for _, category := range declaration.Categories {
		if slices.Contains(vendored.Categories, category) || slices.Contains(vendored.Qualified, category) {
			return fmt.Errorf("the pinned Ptah already emits %s; remove it from the read-ahead declaration", category)
		}
	}
	digest := sha256.Sum256(content)
	if got := hex.EncodeToString(digest[:]); got != declaration.VocabularySHA256 {
		return fmt.Errorf("%s has digest %s, the declaration records %s", declaration.Vocabulary, got, declaration.VocabularySHA256)
	}
	computed, err := Compute(func(string) ([]byte, error) { return content, nil })
	if err != nil {
		return fmt.Errorf("Ptah %s: %w", declaration.PtahCommit, err)
	}
	want := Vendored{
		Sources:    computed.Sources,
		Categories: slices.Sorted(slices.Values(append(slices.Clone(vendored.Categories), declaration.Categories...))),
		Qualified:  vendored.Qualified,
	}
	if err := Compare(want, computed); err != nil {
		return fmt.Errorf("Ptah %s differs from the pinned list plus the read-ahead categories: %w", declaration.PtahCommit, err)
	}
	return nil
}
