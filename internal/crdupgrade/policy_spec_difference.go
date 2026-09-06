package crdupgrade

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// policySpecDifference names the first place a live policy spec and its
// compiled contract disagree: the field, the entry and the byte. A refusal
// that only says the two differ sends the reader to diff a live object
// against a compiled struct by hand.
func policySpecDifference(got, want admissionregistrationv1.ValidatingAdmissionPolicySpec) string {
	if !reflect.DeepEqual(got.FailurePolicy, want.FailurePolicy) {
		return fmt.Sprintf("failurePolicy is %s, contract %s", policySpecJSON(got.FailurePolicy), policySpecJSON(want.FailurePolicy))
	}
	if !reflect.DeepEqual(got.ParamKind, want.ParamKind) {
		return fmt.Sprintf("paramKind is %s, contract %s", policySpecJSON(got.ParamKind), policySpecJSON(want.ParamKind))
	}
	if !reflect.DeepEqual(got.MatchConstraints, want.MatchConstraints) {
		return fmt.Sprintf("matchConstraints is %s, contract %s", policySpecJSON(got.MatchConstraints), policySpecJSON(want.MatchConstraints))
	}
	if difference := policyListDifference("matchConditions", len(got.MatchConditions), len(want.MatchConditions), func(i int) (string, string) {
		return got.MatchConditions[i].Name + ": " + got.MatchConditions[i].Expression, want.MatchConditions[i].Name + ": " + want.MatchConditions[i].Expression
	}); difference != "" {
		return difference
	}
	if difference := policyListDifference("variables", len(got.Variables), len(want.Variables), func(i int) (string, string) {
		return got.Variables[i].Name + ": " + got.Variables[i].Expression, want.Variables[i].Name + ": " + want.Variables[i].Expression
	}); difference != "" {
		return difference
	}
	if difference := policyListDifference("validations", len(got.Validations), len(want.Validations), func(i int) (string, string) {
		return policySpecJSON(got.Validations[i]), policySpecJSON(want.Validations[i])
	}); difference != "" {
		return difference
	}
	if difference := policyListDifference("auditAnnotations", len(got.AuditAnnotations), len(want.AuditAnnotations), func(i int) (string, string) {
		return policySpecJSON(got.AuditAnnotations[i]), policySpecJSON(want.AuditAnnotations[i])
	}); difference != "" {
		return difference
	}
	if reflect.DeepEqual(got, want) {
		return "specs are equal"
	}
	return "specs differ in a field this comparison does not name"
}

func policyListDifference(field string, got, want int, item func(int) (string, string)) string {
	for i := 0; i < got && i < want; i++ {
		actual, expected := item(i)
		if actual == expected {
			continue
		}
		at := 0
		for at < len(actual) && at < len(expected) && actual[at] == expected[at] {
			at++
		}
		start := max(0, at-60)
		return fmt.Sprintf("%s[%d] differs at byte %d: live ...%s..., contract ...%s...", field, i, at,
			actual[start:min(len(actual), at+100)], expected[start:min(len(expected), at+100)])
	}
	if got != want {
		return fmt.Sprintf("%s has %d entries, contract %d", field, got, want)
	}
	return ""
}

func policySpecJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return strings.ReplaceAll(err.Error(), "\n", " ")
	}
	return string(encoded)
}
