package schemaselector_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/schemaselector"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		valid  bool
	}{
		{name: "empty list", valid: true},
		{name: "selectors", values: []string{"audit.*", "legacy,archive"}, valid: true},
		{name: "internal unicode whitespace", values: []string{"audit\u00a0archive"}, valid: true},
		{name: "maximum length", values: []string{strings.Repeat("é", schemaselector.MaxRunes)}, valid: true},
		{name: "empty selector", values: []string{""}},
		{name: "leading whitespace", values: []string{" audit.*"}},
		{name: "trailing unicode whitespace", values: []string{"audit.*\u3000"}},
		{name: "control character", values: []string{"audit\n.*"}},
		{name: "invalid UTF-8", values: []string{string([]byte{0xff})}},
		{name: "overlong", values: []string{strings.Repeat("a", schemaselector.MaxRunes+1)}},
		{name: "duplicate", values: []string{"audit.*", "audit.*"}},
		{name: "too many", values: make([]string, schemaselector.MaxItems+1)},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := schemaselector.Validate(test.values)
			if test.valid && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() accepted an invalid selector list")
			}
		})
	}
}
