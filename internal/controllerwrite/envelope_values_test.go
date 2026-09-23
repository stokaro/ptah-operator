package controllerwrite

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/workload"
)

func envelopeDigest(b byte) string {
	return "sha256:" + strings.Repeat(string(b), 64)
}

func validEnvelope() map[string]string {
	return map[string]string{
		workload.AnnotationControllerImage:        "example.invalid/manager@" + envelopeDigest('a'),
		workload.AnnotationControllerRevision:     "controller-revision",
		workload.AnnotationControllerStateVersion: "1",
		workload.AnnotationPtahVersion:            "v0.3.0",
	}
}

// The controller envelope is how a Job says which controller build produced
// it. Where the envelope is compared against the binding in force, a value
// that is merely well-formed still has to be the right one -- but on the
// retired paths it is compared against a plan instead, and there these rules
// are the whole of what makes the value unambiguous.
//
// Every one of them could be removed with the package green. The canonical
// integer rule is the sharpest: "01" and "1" parse to the same number and are
// different strings, so without it two Jobs with different envelopes would
// both be accepted against one plan.
func TestAControllerEnvelopeValueIsUnambiguousOrRefused(t *testing.T) {
	t.Parallel()

	if err := validateControllerEnvelopeValues(validEnvelope()); err != nil {
		t.Fatalf("a valid envelope was refused, so nothing below proves anything: %v", err)
	}

	for _, row := range []struct {
		name  string
		key   string
		value string
	}{
		{
			name: "the image carries no digest at all",
			key:  workload.AnnotationControllerImage, value: "example.invalid/manager:v1",
		},
		{
			name: "the image names nothing before its digest",
			key:  workload.AnnotationControllerImage, value: "@" + envelopeDigest('a'),
		},
		{
			// A name carrying whitespace or a second separator is two names.
			name: "the image name carries whitespace",
			key:  workload.AnnotationControllerImage, value: "example.invalid/man ager@" + envelopeDigest('a'),
		},
		{
			name: "the image name carries a second separator",
			key:  workload.AnnotationControllerImage, value: "a@b@" + envelopeDigest('a'),
		},
		{
			name: "the digest is not a SHA-256 one",
			key:  workload.AnnotationControllerImage, value: "example.invalid/manager@sha256:beef",
		},
		{
			name: "the revision is empty",
			key:  workload.AnnotationControllerRevision, value: "",
		},
		{
			name: "the revision carries edge whitespace",
			key:  workload.AnnotationControllerRevision, value: " controller-revision",
		},
		{
			name: "the state version is not a number",
			key:  workload.AnnotationControllerStateVersion, value: "one",
		},
		{
			name: "the state version is not positive",
			key:  workload.AnnotationControllerStateVersion, value: "0",
		},
		{
			// Parses to 1, and is not the string a comparison against the
			// binding would ever produce.
			name: "the state version is not canonical",
			key:  workload.AnnotationControllerStateVersion, value: "01",
		},
		{
			name: "the state version carries a sign",
			key:  workload.AnnotationControllerStateVersion, value: "+1",
		},
		{
			name: "the data-plane version is empty",
			key:  workload.AnnotationPtahVersion, value: "",
		},
		{
			name: "the data-plane version carries edge whitespace",
			key:  workload.AnnotationPtahVersion, value: "v0.3.0 ",
		},
		{
			name: "the data-plane version is longer than the bound",
			key:  workload.AnnotationPtahVersion, value: strings.Repeat("v", 129),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			annotations := validEnvelope()
			annotations[row.key] = row.value
			if err := validateControllerEnvelopeValues(annotations); err == nil {
				t.Fatalf("%s = %q was accepted as an unambiguous envelope value", row.key, row.value)
			}
		})
	}
}

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
