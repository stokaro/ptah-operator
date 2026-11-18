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

package runner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// undecidedDriftSummary frames a drift report that found no difference and
// could not decide some declared objects (stokaro/ptah#3850): the read of the
// database did not describe their kind, as a read that covers some of a
// PostgreSQL database's schemas does not describe its publications. Ptah
// fails such a check at the threshold all and passes it at destructive, and
// its drift is false either way.
//
// The frame says no drift, so it carries no highest severity, and keeps the
// one finding that counts the objects. Plan reads the managed scope and names
// them; an Observe that found only this does not read as converged.
func undecidedDriftSummary(report dataplane.DriftReport, severity string) (string, int32, []DriftFindingSummary, error) {
	// DecodeDrift refused a repeated category, so this is the one finding.
	finding := report.Findings[0]
	findingSeverity := strings.ToLower(strings.TrimSpace(finding.Severity))
	if severity != findingSeverity {
		return "", 0, nil, errors.New("drift report highest severity does not match its findings")
	}
	return "", finding.Count, []DriftFindingSummary{{
		Category: finding.Category, Count: finding.Count, Severity: findingSeverity,
	}}, nil
}

// undecidedLead opens the sentence a plan of undecided objects is reported
// with.
const undecidedLead = "ptah could not decide "

// undecidedSentence names what a plan's comparison could not decide: how many
// declared objects, then each one with Ptah's reason, as in
//
//	ptah could not decide 1 declared object: PostgreSQL publication notes_pub
//	(whether the publication exists was not established: ...)
//
// Each object reads as its kind, in the owner's words where it gives them,
// its name as Ptah spells an identity, and the reason in parentheses: Ptah's
// sentence for a feature object, and the coverage token, such as
// not-inspected, for a common one. Names and reasons come from a schema and a
// database, so each goes through freeText. The entries that fit
// MaxUndecidedBytes are named and the rest counted; a first entry too long to
// fit is cut, and the cut ends in "...".
func undecidedSentence(undecided dataplane.Undecided, redactor Redactor) string {
	entries := undecidedEntries(undecided, redactor)
	noun := "objects"
	if len(entries) == 1 {
		noun = "object"
	}
	lead := fmt.Sprintf("%s%d declared %s", undecidedLead, len(entries), noun)
	room := MaxUndecidedBytes - len(lead) - len(": ")
	shown := 0
	for shown < len(entries) &&
		len(strings.Join(entries[:shown+1], "; ")+andMore(shown+1, len(entries))) <= room {
		shown++
	}
	if shown == 0 {
		const cut = "..."
		more := andMore(1, len(entries))
		return lead + ": " + cutLine(entries[0], room-len(more)-len(cut)) + cut + more
	}
	return lead + ": " + strings.Join(entries[:shown], "; ") + andMore(shown, len(entries))
}

// undecidedEntries spells each undecided object, common objects first, in the
// order the document lists them.
func undecidedEntries(undecided dataplane.Undecided, redactor Redactor) []string {
	entries := make([]string, 0, undecided.Count())
	for _, object := range undecided.Common {
		entry := freeText(object.Kind+" "+displayComponent(object.Name), redactor, MaxUndecidedBytes)
		if reason := freeText(object.Reason, redactor, MaxUndecidedBytes); reason != "" {
			entry += " (" + reason + ")"
		}
		entries = append(entries, entry)
	}
	for _, feature := range undecided.Features {
		kind := feature.Label
		if strings.TrimSpace(kind) == "" {
			kind = feature.Kind
		}
		entry := freeText(kind+" "+undecidedSubjectPath(feature.Subject), redactor, MaxUndecidedBytes)
		entries = append(entries, entry+" ("+freeText(feature.Reason, redactor, MaxUndecidedBytes)+")")
	}
	return entries
}

// undecidedSubjectPath spells a subject as Ptah's objectidentity.ID.Path does
// for the components a subject carries here: catalog, schema, parent and
// name, joined by dots, each in double quotes where it holds a dot or a
// quote. A subject with none of them is a whole kind, which Ptah names
// "namespace".
func undecidedSubjectPath(subject dataplane.UndecidedSubject) string {
	parts := make([]string, 0, 4)
	for _, part := range []dataplane.IdentityPart{subject.Catalog, subject.Schema, subject.Parent} {
		if part.Source != "" {
			parts = append(parts, displayComponent(part.Source))
		}
	}
	if subject.Name.Source == "" && len(parts) == 0 {
		return "namespace"
	}
	return strings.Join(append(parts, displayComponent(subject.Name.Source)), ".")
}

// displayComponent writes one component of a name: as it is, or in double
// quotes, with each quote inside doubled, where it holds the dot that
// separates components or the quote that would open one.
func displayComponent(source string) string {
	if !strings.ContainsAny(source, `."`) {
		return source
	}
	return `"` + strings.ReplaceAll(source, `"`, `""`) + `"`
}
