package crdupgrade

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	celgo "github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apiserver/pkg/cel/library"
)

// Kubernetes rejects a CEL expression above 100,000 code points. Keep ten percent
// of that ceiling unused so ordinary contract growth cannot turn an upgrade
// into an API-server compilation failure without first failing a test.
const admissionExpressionCodePointLimitWithHeadroom = 90_000

func assertCELExpressionHeadroom(t *testing.T, description, expression string) {
	t.Helper()
	codePoints := utf8.RuneCountInString(expression)
	if codePoints >= admissionExpressionCodePointLimitWithHeadroom {
		t.Fatalf("%s has %d CEL code points, want fewer than %d", description, codePoints, admissionExpressionCodePointLimitWithHeadroom)
	}
}

// evaluatePolicyValidations evaluates a policy's variables and then each of
// its validations with cel-go, the way the API server would for one request.
func evaluatePolicyValidations(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	object, oldObject, request map[string]any,
) []bool {
	t.Helper()
	values := map[string]any{
		"object":    object,
		"oldObject": oldObject,
		"request":   request,
	}
	variables := make(map[string]any, len(policy.Spec.Variables))
	for _, variable := range policy.Spec.Variables {
		variables[variable.Name] = evaluatePolicyCEL(t, variable.Expression, values, variables)
	}
	results := make([]bool, 0, len(policy.Spec.Validations))
	for index, validation := range policy.Spec.Validations {
		result := evaluatePolicyCEL(t, validation.Expression, values, variables)
		allowed, ok := result.(bool)
		if !ok {
			t.Fatalf("validation %d result = %T(%v), want bool", index, result, result)
		}
		results = append(results, allowed)
	}
	return results
}

// evaluatePolicyMatchConditions reports whether every match condition of a
// policy selects the request.
func evaluatePolicyMatchConditions(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	object, oldObject, request map[string]any,
) bool {
	t.Helper()
	values := map[string]any{
		"object":    object,
		"oldObject": oldObject,
		"request":   request,
	}
	for index, condition := range policy.Spec.MatchConditions {
		result := evaluatePolicyCEL(t, condition.Expression, values, map[string]any{})
		matches, ok := result.(bool)
		if !ok {
			t.Fatalf("match condition %d result = %T(%v), want bool", index, result, result)
		}
		if !matches {
			return false
		}
	}
	return true
}

func evaluatePolicyCEL(t *testing.T, expression string, values, variables map[string]any) any {
	t.Helper()
	expression = strings.ReplaceAll(expression, " int(", " testInt(")
	expression = strings.ReplaceAll(expression, " string(", " testString(")
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
		celgo.Function("testInt", celgo.Overload(
			"ptah_test_dyn_to_int",
			[]*celgo.Type{celgo.DynType},
			celgo.IntType,
			celgo.UnaryBinding(func(value ref.Val) ref.Val {
				parsed, err := strconv.ParseInt(fmt.Sprint(value.Value()), 10, 64)
				if err != nil {
					return types.NewErr("parse integer: %v", err)
				}
				return types.Int(parsed)
			}),
		)),
		celgo.Function("testString", celgo.Overload(
			"ptah_test_dyn_to_string",
			[]*celgo.Type{celgo.DynType},
			celgo.StringType,
			celgo.UnaryBinding(func(value ref.Val) ref.Val {
				return types.String(fmt.Sprint(value.Value()))
			}),
		)),
		ext.Strings(),
		library.Quantity(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile CEL %q: %v", expression, issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build CEL %q: %v", expression, err)
	}
	activation := make(map[string]any, len(values)+1)
	for name, value := range values {
		activation[name] = value
	}
	activation["variables"] = variables
	result, _, err := program.Eval(activation)
	if err != nil {
		t.Fatalf("evaluate CEL %q: %v", expression, err)
	}
	return result.Value()
}

// controllerObjectValidationIndex finds the one validation that contains
// fragment.
func controllerObjectValidationIndex(t *testing.T, validations []admissionregistrationv1.Validation, fragment string) int {
	t.Helper()
	index := -1
	for candidate, validation := range validations {
		if !strings.Contains(validation.Expression, fragment) {
			continue
		}
		if index >= 0 {
			t.Fatalf("multiple controller-object validations contain %q", fragment)
		}
		index = candidate
	}
	if index < 0 {
		t.Fatalf("no controller-object validation contains %q", fragment)
	}
	return index
}
