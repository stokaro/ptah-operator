package jobclaim

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

// The manager record is how a Job says which controller build produced it.
// Where the envelope is compared against the binding in force, a value that
// is merely well-formed still has to be the right one -- but on the retired
// paths it is compared against a plan or nothing, and there these rules are
// the whole of what makes the value unambiguous.
//
// Every one of them could be removed with the package green. The canonical
// integer rule is the sharpest: "01" and "1" parse to the same number and are
// different strings, so without it two Jobs with different envelopes would
// both be accepted against one plan.
func TestAControllerEnvelopeValueIsUnambiguousOrRefused(t *testing.T) {
	t.Parallel()

	if err := validateManagerRecord(validEnvelope()); err != nil {
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
			if err := validateManagerRecord(annotations); err == nil {
				t.Fatalf("%s = %q was accepted as an unambiguous envelope value", row.key, row.value)
			}
		})
	}
}
