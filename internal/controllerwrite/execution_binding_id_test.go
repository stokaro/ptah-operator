package controllerwrite

import (
	"strings"
	"testing"
)

// An execution binding ID names one epoch of the controller's own state, and
// the format is the whole of its identity: there is nothing else to compare a
// malformed one against.
func TestAnExecutionBindingIDIsExactlyItsFormat(t *testing.T) {
	t.Parallel()

	valid := "v1-" + strings.Repeat("1", 32)
	if !isExecutionBindingID(valid) {
		t.Fatalf("a valid execution binding ID was refused, so nothing below proves anything: %q", valid)
	}

	for _, row := range []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "too short", value: "v1-" + strings.Repeat("1", 31)},
		{name: "too long", value: "v1-" + strings.Repeat("1", 33)},
		{name: "another version prefix", value: "v2-" + strings.Repeat("1", 32)},
		{name: "no prefix at all", value: strings.Repeat("1", 35)},
		{name: "uppercase hexadecimal", value: "v1-" + strings.Repeat("A", 32)},
		{name: "not hexadecimal", value: "v1-" + strings.Repeat("g", 32)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if isExecutionBindingID(row.value) {
				t.Fatalf("%q was accepted as an execution binding ID", row.value)
			}
		})
	}
}
