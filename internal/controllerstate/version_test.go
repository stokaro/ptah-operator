package controllerstate_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

func TestValidateRevision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		revision string
		wantErr  bool
	}{
		{name: "Git SHA", revision: strings.Repeat("a", 40)},
		{name: "custom provenance", revision: "release candidate 7"},
		{name: "maximum bytes", revision: strings.Repeat("r", controllerstate.MaxRevisionBytes)},
		{name: "empty", wantErr: true},
		{name: "leading whitespace", revision: " revision", wantErr: true},
		{name: "trailing whitespace", revision: "revision ", wantErr: true},
		{name: "embedded newline", revision: "release\ncandidate", wantErr: true},
		{name: "embedded null", revision: "release\x00candidate", wantErr: true},
		{name: "invalid UTF-8", revision: string([]byte{'r', 0xff}), wantErr: true},
		{name: "oversized", revision: strings.Repeat("r", controllerstate.MaxRevisionBytes+1), wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := controllerstate.ValidateRevision(test.revision)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateRevision(%q) error = %v, wantErr %t", test.revision, err, test.wantErr)
			}
		})
	}
}
