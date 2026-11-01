// Package statusview holds what `kubectl ptah schema` and `kubectl ptah
// migration` print the same way: the conditions the operator published, the
// ones a reader reads first, the decisions people made about a resource, and
// a list the reader was not allowed to read.
//
// Nothing here decides anything about a resource. Every value comes from a
// field the operator or admission wrote, and the only judgments made are which
// of those fields a reader sees first.
package statusview

import (
	"fmt"
	"strings"
	"text/tabwriter"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition is one published condition, with the two fields that say when
// and for which spec it was written.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the spec generation the condition was written for.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastTransitionTime is when the status last changed, not when the
	// condition was last written: a message rewritten under the same status
	// keeps the older time.
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// FromConditions copies the conditions in the order the status holds them.
func FromConditions(conditions []metav1.Condition) []Condition {
	if len(conditions) == 0 {
		return nil
	}
	out := make([]Condition, 0, len(conditions))
	for _, condition := range conditions {
		view := Condition{
			Type:               condition.Type,
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			ObservedGeneration: condition.ObservedGeneration,
		}
		if !condition.LastTransitionTime.IsZero() {
			changed := condition.LastTransitionTime
			view.LastTransitionTime = &changed
		}
		out = append(out, view)
	}
	return out
}

// Find returns the condition of a type, or nil when the status has none.
func Find(conditions []Condition, conditionType string) *Condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

// holdTypes are the condition types whose True status the API documents as
// something holding the resource: a history it cannot continue, a suspension
// the spec asked for, and an operation that failed. Their False status says
// nothing holds, so it is not one of them.
var holdTypes = map[string]bool{
	"Blocked":              true,
	"Suspended":            true,
	"ReconciliationFailed": true,
}

// LabelWidth is the column the schema and migration views pad every
// "Label:" to, so that every value starts in the same column. A condition read
// first is labeled by its type, and the longest of those types is longer than
// any label the views print of their own.
var LabelWidth = leadingLabelWidth()

func leadingLabelWidth() int {
	width := len("Protected tables:")
	for conditionType := range holdTypes {
		width = max(width, len(conditionType)+len(":"))
	}
	return width
}

// FieldWriter returns a function that writes "Label: value" lines, the label
// padded to width after indent, and skips a field with no value.
func FieldWriter(text *strings.Builder, indent string, width int) func(name, value string) {
	return func(name, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(text, "%s%-*s %s\n", indent, width, name+":", value)
	}
}

// Leading is what a reader reads before the full table: Ready, whatever its
// status, because the API documents its reason as the one to read first, and
// then every condition that is True and holds the resource.
//
// It is a selection and not a verdict. Nothing is derived from a combination
// of conditions, and a condition not chosen here is still in the table.
func Leading(conditions []Condition) []Condition {
	var leading []Condition
	if ready := Find(conditions, "Ready"); ready != nil {
		leading = append(leading, *ready)
	}
	for _, condition := range conditions {
		if holdTypes[condition.Type] && condition.Status == string(metav1.ConditionTrue) {
			leading = append(leading, condition)
		}
	}
	return leading
}

// Summary is one condition on one line: its status, its reason and its
// message, cleaned for a terminal.
func Summary(condition Condition) string {
	parts := []string{Clean(condition.Status)}
	if condition.Reason != "" {
		parts[0] += ", " + Clean(condition.Reason)
	}
	if condition.Message != "" {
		parts = append(parts, Clean(condition.Message))
	}
	return strings.Join(parts, ": ")
}

// WriteTable writes conditions as an aligned table under a heading, one row
// each, the message last so a long one runs off the end of its own row rather
// than pushing the others apart.
//
// generation is the resource's metadata.generation. A condition written for
// another generation says so after its message, because it may describe a
// spec that has since changed. Zero turns the note off.
func WriteTable(text *strings.Builder, indent string, conditions []Condition, generation int64) {
	var rows strings.Builder
	table := tabwriter.NewWriter(&rows, 0, 0, 2, ' ', 0)
	fmt.Fprintf(table, "%sTYPE\tSTATUS\tREASON\tSINCE\tMESSAGE\n", indent)
	for _, condition := range conditions {
		since := "-"
		if condition.LastTransitionTime != nil {
			since = Time(*condition.LastTransitionTime)
		}
		message := Clean(condition.Message)
		if generation != 0 && condition.ObservedGeneration != 0 && condition.ObservedGeneration != generation {
			message = strings.TrimSpace(message + fmt.Sprintf(" (written for generation %d)", condition.ObservedGeneration))
		}
		fmt.Fprintf(table, "%s%s\t%s\t%s\t%s\t%s\n", indent,
			Clean(condition.Type), Clean(condition.Status), orDash(Clean(condition.Reason)), since, message)
	}
	// A tabwriter writing into a strings.Builder cannot fail.
	_ = table.Flush()
	// A row with no message ends in the padding of the column before it.
	for _, row := range strings.SplitAfter(rows.String(), "\n") {
		if row == "" {
			continue
		}
		text.WriteString(strings.TrimRight(row, " \n"))
		text.WriteString("\n")
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
