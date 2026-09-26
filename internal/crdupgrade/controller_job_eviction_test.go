package crdupgrade

import (
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
)

// A mutating operation Pod carries the cluster autoscaler's safe-to-evict
// annotation, and the Job carries its template's annotations exactly, so the
// Job write guard holds that one key to the builder's shape: on every Apply,
// with the one value that keeps the Pod where it is, and on nothing else. It
// still refuses every key it does not know.
func TestControllerJobAnnotationContractRequiresEveryApplyToRefuseEviction(t *testing.T) {
	t.Parallel()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(controllerJobAnnotationContractExpression())
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile the Job annotation contract: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	const image = "registry.example/ptah@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	variables := map[string]any{
		"activeRelease": int64(1), "candidateRelease": int64(1), "previousRelease": int64(0),
		"activeControllerImage": image, "activeControllerStateString": "1", "activeControllerState": int64(1),
	}
	job := func(component, operation string, extra map[string]any) map[string]any {
		annotations := map[string]any{
			"operator.ptah.run/operation-id":              "operation-id",
			"operator.ptah.run/input-fingerprint":         "sha256:" + strings.Repeat("1", 64),
			"operator.ptah.run/ptah-version":              "v1",
			"operator.ptah.run/execution-binding-id":      "v1-" + strings.Repeat("2", 32),
			"operator.ptah.run/controller-image":          image,
			"operator.ptah.run/controller-revision":       "revision",
			"operator.ptah.run/controller-state-version":  "1",
			"operator.ptah.run/admission-snapshot-digest": "sha256:" + strings.Repeat("3", 64),
		}
		if component == "schema-operation" && operation == "apply" {
			annotations["operator.ptah.run/plan-fingerprint"] = "sha256:" + strings.Repeat("4", 64)
			annotations["operator.ptah.run/plan-content-digest"] = "sha256:" + strings.Repeat("5", 64)
		}
		for key, value := range extra {
			annotations[key] = value
		}
		return map[string]any{"metadata": map[string]any{
			"labels": map[string]any{
				"operator.ptah.run/operation": operation,
				"app.kubernetes.io/component": component,
			},
			"annotations": annotations,
		}}
	}
	const evict = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	refuses := map[string]any{evict: "false"}
	for _, row := range []struct {
		name   string
		object map[string]any
		want   bool
	}{
		{name: "a schema Apply that refuses eviction", object: job("schema-operation", "apply", refuses), want: true},
		{name: "a migration Apply that refuses eviction", object: job("migration-operation", "apply", refuses), want: true},
		{name: "a schema Apply that says nothing", object: job("schema-operation", "apply", nil), want: false},
		{name: "a migration Apply that says nothing", object: job("migration-operation", "apply", nil), want: false},
		// The control for the two rows above: without the key a read-only
		// Job is what the builder writes.
		{name: "a read-only schema Plan that says nothing", object: job("schema-operation", "plan", nil), want: true},
		{name: "a schema Apply that allows eviction", object: job("schema-operation", "apply", map[string]any{evict: "true"}), want: false},
		{name: "a read-only schema Plan that refuses eviction", object: job("schema-operation", "plan", refuses), want: false},
		{name: "a migration history read that refuses eviction", object: job("migration-operation", "history", refuses), want: false},
		// The allow-list still refuses a key it does not name.
		{name: "a schema Apply with a key nothing names", object: job("schema-operation", "apply",
			map[string]any{evict: "false", "example.com/other": "x"}), want: false},
	} {
		for _, operation := range []string{"CREATE", "UPDATE"} {
			result, _, err := program.Eval(map[string]any{
				"object":    row.object,
				"request":   map[string]any{"operation": operation},
				"variables": variables,
			})
			if err != nil {
				t.Fatalf("%s on %s: evaluate the Job annotation contract: %v", row.name, operation, err)
			}
			if allowed, ok := result.Value().(bool); !ok || allowed != row.want {
				t.Errorf("%s on %s: Job annotation contract = %v, want %t", row.name, operation, result.Value(), row.want)
			}
		}
	}
}
