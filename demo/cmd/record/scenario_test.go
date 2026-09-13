package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScenarioValidateRefusesAnUncheckedStep(t *testing.T) {
	t.Parallel()
	exit := 0
	tests := []struct {
		name  string
		steps []step
		want  string
	}{
		{
			name:  "no expectation",
			steps: []step{{Run: "kubectl get ptahschema"}},
			want:  "states no expectation",
		},
		{
			name:  "no expected exit status",
			steps: []step{{Run: "kubectl get ptahschema", Expect: &expectation{}}},
			want:  "does not say which exit status",
		},
		{
			name:  "runs nothing",
			steps: []step{{Note: "a note", Expect: &expectation{Exit: &exit}}},
			want:  "runs nothing",
		},
		{
			name: "condition without a reason",
			steps: []step{{
				Run: "kubectl get ptahschema",
				Expect: &expectation{Exit: &exit, Resource: &observation{
					Kind: "ptahschema", Name: "storefront",
					Condition: &condition{Type: "InSync", Status: "True"},
				}},
			}},
			want: "without a reason",
		},
		{
			name: "condition with a status Kubernetes does not write",
			steps: []step{{
				Run: "kubectl get ptahschema",
				Expect: &expectation{Exit: &exit, Resource: &observation{
					Kind: "ptahschema", Name: "storefront",
					Condition: &condition{Type: "InSync", Status: "true", Reason: "ScopedConverged"},
				}},
			}},
			want: "True, False or Unknown",
		},
		{
			name: "an observation that asks nothing",
			steps: []step{{
				Run:    "kubectl get ptahschema",
				Expect: &expectation{Exit: &exit, Resource: &observation{Kind: "ptahschema", Name: "storefront"}},
			}},
			want: "asks nothing of it",
		},
		{
			name: "a field with both equals and notEmpty",
			steps: []step{{
				Run: "kubectl get ptahschema",
				Expect: &expectation{Exit: &exit, Resource: &observation{
					Kind: "ptahschema", Name: "storefront",
					Fields: []field{{Path: "status.phase", Equals: "InSync", NotEmpty: true}},
				}},
			}},
			want: "cannot name the value",
		},
		{
			name: "a generation value that is not current",
			steps: []step{{
				Run: "kubectl get ptahschema",
				Expect: &expectation{Exit: &exit, Resource: &observation{
					Kind: "ptahschema", Name: "storefront", Generation: "latest",
					Condition: &condition{Type: "InSync", Status: "True", Reason: "ScopedConverged"},
				}},
			}},
			want: `the only value is "current"`,
		},
		{
			name:  "a note on two lines",
			steps: []step{{Note: "one\ntwo", Run: "kubectl get ptahschema", Expect: &expectation{Exit: &exit}}},
			want:  "note on more than one line",
		},
		{
			name: "a note longer than a terminal line",
			steps: []step{{
				Note: strings.Repeat("x", maximumNote+1), Run: "kubectl get ptahschema",
				Expect: &expectation{Exit: &exit},
			}},
			want: "one line of a terminal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			subject := scenario{
				ID: "example", Title: "Example", Tagline: "A tagline.", Learn: "A lesson.",
				Tags: []string{"Lab"}, Steps: test.steps,
			}
			err := subject.validate()
			if err == nil {
				t.Fatalf("validate accepted %s", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate said %q, which does not carry %q", err, test.want)
			}
		})
	}
}

func TestScenarioValidateAcceptsACheckedScenario(t *testing.T) {
	t.Parallel()
	exit := 0
	subject := scenario{
		ID: "first-apply", Title: "Apply a schema", Tagline: "A tagline.", Learn: "A lesson.",
		Tags: []string{"Lifecycle"},
		Steps: []step{{
			Note: "Apply it.",
			Run:  "kubectl apply -f storefront.yaml",
			Expect: &expectation{Exit: &exit, StdoutContains: []string{"created"}, Resource: &observation{
				Kind: "ptahschema", Name: "storefront", Generation: "current",
				Condition: &condition{Type: "InSync", Status: "True", Reason: "ScopedConverged"},
				Fields:    []field{{Path: "status.phase", Equals: "InSync"}},
			}},
		}},
	}
	if err := subject.validate(); err != nil {
		t.Fatalf("validate refused a checked scenario: %v", err)
	}
}

// A condition is read as fields. The rows that matter are the ones where the
// message would have said the right thing and the fields do not: a reason that
// belongs to a different outcome on the same type is the whole reason this is
// not a substring match.
func TestConditionMatches(t *testing.T) {
	t.Parallel()
	generation := int64(4)
	conditions := []map[string]any{
		{"type": "Applying", "status": "False", "reason": "JobCompleted", "observedGeneration": float64(4),
			"message": "Apply Job completed; convergence observation is pending"},
		{"type": "InSync", "status": "True", "reason": "ScopedConverged", "observedGeneration": float64(4),
			"message": "A stable scoped plan proves convergence"},
	}
	tests := []struct {
		name    string
		want    condition
		problem string
	}{
		{
			name: "the condition it names",
			want: condition{Type: "InSync", Status: "True", Reason: "ScopedConverged"},
		},
		{
			name: "and the generation it observed",
			want: condition{Type: "InSync", Status: "True", Reason: "ScopedConverged", ObservedGeneration: &generation},
		},
		{
			name:    "a finished Job is not a converged database",
			want:    condition{Type: "Applying", Status: "True", Reason: "JobCompleted"},
			problem: "condition Applying is False, expected True",
		},
		{
			name:    "the same type with another outcome's reason",
			want:    condition{Type: "InSync", Status: "True", Reason: "JobCompleted"},
			problem: "has reason ScopedConverged, expected JobCompleted",
		},
		{
			name:    "a type nothing wrote",
			want:    condition{Type: "Suspended", Status: "True", Reason: "Suspended"},
			problem: "carries no Suspended condition",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.want.matches(conditions)
			if test.problem == "" {
				if err != nil {
					t.Fatalf("matches refused the condition it names: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("matches accepted %s", test.name)
			}
			if !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("matches said %q, which does not carry %q", err, test.problem)
			}
		})
	}
}

func TestConditionMatchesRefusesAStaleObservation(t *testing.T) {
	t.Parallel()
	generation := int64(5)
	conditions := []map[string]any{
		{"type": "InSync", "status": "True", "reason": "ScopedConverged", "observedGeneration": float64(4)},
	}
	want := condition{Type: "InSync", Status: "True", Reason: "ScopedConverged", ObservedGeneration: &generation}
	err := want.matches(conditions)
	if err == nil {
		t.Fatal("matches accepted a condition that had not observed the object as it is")
	}
	if !strings.Contains(err.Error(), "observed generation 4, expected 5") {
		t.Fatalf("matches said %q", err)
	}
}

func TestConditionsOfReadsTheStatus(t *testing.T) {
	t.Parallel()
	object := []byte(`{"status":{"conditions":[{"type":"Ready","status":"True","reason":"InSync"}]}}`)
	conditions, err := conditionsOf(object)
	if err != nil {
		t.Fatalf("conditionsOf: %v", err)
	}
	if len(conditions) != 1 || conditions[0]["type"] != "Ready" {
		t.Fatalf("conditionsOf read %v", conditions)
	}
}

func TestFieldMatches(t *testing.T) {
	t.Parallel()
	var object map[string]any
	raw := `{"status":{"phase":"InSync","applied":{"artifactDigest":"sha256:ab","statementCount":3},"plan":null}}`
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	tests := []struct {
		name    string
		read    field
		problem string
	}{
		{name: "a value it names", read: field{Path: "status.phase", Equals: "InSync"}},
		{name: "a number", read: field{Path: "status.applied.statementCount", Equals: "3"}},
		{name: "set", read: field{Path: "status.applied.artifactDigest", NotEmpty: true}},
		{
			name:    "another value",
			read:    field{Path: "status.phase", Equals: "Blocked"},
			problem: `status.phase is "InSync", expected "Blocked"`,
		},
		{name: "a null", read: field{Path: "status.plan", NotEmpty: true}, problem: "status.plan is not set"},
		{name: "a path nothing wrote", read: field{Path: "status.drift", NotEmpty: true}, problem: "is not set"},
		{name: "a path through a scalar", read: field{Path: "status.phase.name", NotEmpty: true}, problem: "is not set"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.read.matches(object)
			if test.problem == "" {
				if err != nil {
					t.Fatalf("matches refused a value it names: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("matches accepted %s", test.name)
			}
			if !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("matches said %q, which does not carry %q", err, test.problem)
			}
		})
	}
}
