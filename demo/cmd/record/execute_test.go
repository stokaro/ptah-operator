package main

import (
	"strings"
	"testing"
)

func TestExpectationEvaluateReportsEveryFailure(t *testing.T) {
	t.Parallel()
	exit := 0
	expected := expectation{
		Exit:           &exit,
		StdoutContains: []string{"created", "storefront"},
		StdoutAbsent:   []string{"password"},
		StderrContains: []string{"warning"},
	}
	err := expected.evaluate(outcome{stdout: "password: hunter2\n", stderr: "", exit: 1})
	if err == nil {
		t.Fatal("evaluate accepted an outcome that failed every clause")
	}
	for _, want := range []string{
		"exited 1, expected 0",
		`stdout does not contain "created"`,
		`stdout does not contain "storefront"`,
		`stderr does not contain "warning"`,
		`stdout contains "password"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("evaluate did not report %q; it said %q", want, err)
		}
	}
}

func TestExpectationEvaluateAcceptsWhatItNames(t *testing.T) {
	t.Parallel()
	exit := 1
	expected := expectation{Exit: &exit, StderrContains: []string{"refused"}}
	if err := expected.evaluate(outcome{stderr: "error: refused\n", exit: 1}); err != nil {
		t.Fatalf("evaluate refused the outcome it names: %v", err)
	}
}

func TestExpectationDescribeSaysWhatItClaims(t *testing.T) {
	t.Parallel()
	exit := 0
	expected := expectation{Exit: &exit, StdoutContains: []string{"InSync"}, StdoutAbsent: []string{"password"}}
	got := expected.describe()
	for _, want := range []string{"exits 0", `prints "InSync"`, `never prints "password"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("describe said %q, which does not carry %q", got, want)
		}
	}
}

func TestObservationDescribeNamesTheObjectAndTheClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		subject observation
		want    string
	}{
		{
			name: "a condition",
			subject: observation{
				Kind: "ptahschema", Name: "storefront",
				Condition: &condition{Type: "InSync", Status: "True", Reason: "ScopedConverged"},
			},
			want: "ptahschema/storefront: InSync=True (ScopedConverged)",
		},
		{
			name: "a field that is set",
			subject: observation{
				Kind: "ptahschemaapproval", Name: "storefront-v1",
				Fields: []field{{Path: "spec.approver.username", NotEmpty: true}},
			},
			want: "ptahschemaapproval/storefront-v1: spec.approver.username is set",
		},
		{
			name:    "an object that must not be there",
			subject: observation{Kind: "job", Name: "ptah-apply-storefront", Absent: true},
			want:    "job/ptah-apply-storefront does not exist",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.subject.describe(); got != test.want {
				t.Fatalf("describe said %q, expected %q", got, test.want)
			}
		})
	}
}

func TestGenerationOfReadsTheMetadata(t *testing.T) {
	t.Parallel()
	generation, found, err := generationOf([]byte(`{"metadata":{"generation":7}}`))
	if err != nil || !found || generation != 7 {
		t.Fatalf("generationOf read %d, %v, %v", generation, found, err)
	}
	_, found, err = generationOf([]byte(`{"metadata":{}}`))
	if err != nil || found {
		t.Fatalf("generationOf invented a generation: %v, %v", found, err)
	}
}

// A namespace arrives as a shell variable by default and as a literal when a
// scenario names one. Both have to reach kubectl as one argument.
func TestQuoteMakesOneWord(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{in: "$NAMESPACE", want: `"$NAMESPACE"`},
		{in: "ptah-system", want: `'ptah-system'`},
		{in: "it's", want: `'it'\''s'`},
	}
	for _, test := range tests {
		t.Run(test.in, func(t *testing.T) {
			t.Parallel()
			if got := quote(test.in); got != test.want {
				t.Fatalf("quote wrote %s, expected %s", got, test.want)
			}
		})
	}
}
