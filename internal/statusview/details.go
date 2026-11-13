package statusview

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// DetailsHeading opens the part of a view --details adds, after everything
// the view prints without it. The status is decoded into the API types this
// plugin was built with, so a field a newer operator added is not among them.
const DetailsHeading = "\nDetails, every status field this plugin's API version knows:\n"

// WriteRecord writes one status record a details view prints: a "Label:"
// line with what the record is in a few words, a sentence that says what it
// means where the record alone does not, and then every field it holds. A
// record with no summary and no field set is not written at all.
func WriteRecord(text *strings.Builder, label, summary, lead string, record any) {
	var fields strings.Builder
	WriteFields(&fields, "  ", record)
	writeRecordText(text, label, summary, lead, fields.String())
}

// WriteStatusScalars writes the status's own one-line fields as the first
// record of a details view: its generation, phase and times, with what the
// status was written for as the summary and extra lines after them.
func WriteStatusScalars(text *strings.Builder, summary string, status any, extra ...string) {
	var fields strings.Builder
	WriteScalars(&fields, "  ", status)
	for _, line := range extra {
		if line != "" {
			fields.WriteString("  " + line + "\n")
		}
	}
	writeRecordText(text, "Status", summary, "", fields.String())
}

func writeRecordText(text *strings.Builder, label, summary, lead, fields string) {
	if summary == "" && fields == "" {
		return
	}
	text.WriteString("\n")
	if summary == "" {
		text.WriteString(label + ":\n")
	} else {
		FieldWriter(text, "", LabelWidth)(label, summary)
	}
	if lead != "" {
		WriteWrapped(text, "  ", 80, lead)
	}
	text.WriteString(fields)
}

// Claimed is the summary of an operation claim: its type, its attempt, and
// when it was claimed, where the claim records that.
func Claimed(operationType string, attempt int32, startedAt metav1.Time) string {
	summary := fmt.Sprintf("%s, attempt %d", Clean(operationType), attempt)
	if !startedAt.IsZero() {
		summary += ", claimed " + Time(startedAt)
	}
	return summary
}

// StatusFor says which spec generation the status was written for.
func StatusFor(observed, generation int64) string {
	if observed == 0 {
		return fmt.Sprintf("no generation yet; the spec is at generation %d", generation)
	}
	return fmt.Sprintf("generation %d; the spec is at generation %d", observed, generation)
}

// ReadRequested is the line that says whether the read a person asked for
// has been answered, or nothing where none was ever asked for.
func ReadRequested(requested, answered string) string {
	if line := ReadRequest(requested, answered); line != "" {
		return "Read requested: " + line
	}
	return ""
}

// ReadRequest says whether the read a person asked for with the
// read-request annotation has been answered: requested is the annotation as
// the resource holds it now, answered is status.lastReadRequest. The status
// records an answer as operatorv1alpha1.ReadRequestToken of the value, which
// is a digest for a value longer than MaxReadRequestLength, so the annotation
// is compared through the same token.
func ReadRequest(requested, answered string) string {
	switch {
	case requested == "" && answered == "":
		return ""
	case requested == "":
		return "none now; the last one answered was " + Clean(answered)
	case operatorv1alpha1.ReadRequestToken(requested) == answered:
		return Clean(requested) + ", answered"
	case answered == "":
		return Clean(requested) + ", not answered yet"
	default:
		return Clean(requested) + ", not answered yet; the last one answered was " + Clean(answered)
	}
}

// JobLead says what an operation claim knows about its Job. The claim names
// the Job before it creates it, so a name alone is a reservation; the UID,
// recorded once the Job exists, is what confirms which Job it is.
//
// marksDispatch says the claim records dispatchStarted before its one create,
// as every claim that holds the database realm does. Only such a claim is
// known not to have created a Job it has no mark for. A claim that does not
// mark can have created its Job and then failed the read that confirms it, or
// stopped between the create and the write that records the UID; the next
// pass adopts that Job by name.
func JobLead(name string, uid types.UID, dispatchStarted, marksDispatch bool) string {
	switch {
	case uid != "":
		return "Its Job " + Clean(name) + " is confirmed: the claim records the Job's UID " + Clean(string(uid)) +
			", and a Job of that name with another UID is not this one."
	case dispatchStarted:
		return "Its Job " + Clean(name) + " is reserved and its one create was attempted, but no UID is recorded: " +
			"whether the Job exists is not yet confirmed, and it is never created a second time."
	case marksDispatch:
		return "Its Job " + Clean(name) + " is reserved by name and not created yet: no UID is recorded."
	default:
		return "Its Job " + Clean(name) + " is named, and no UID is recorded: whether it was created is not " +
			"confirmed yet."
	}
}

// After is a time a record waits for, or nothing.
func After(at *metav1.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return Time(*at)
}
