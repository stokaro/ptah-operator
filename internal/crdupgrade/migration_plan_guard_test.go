package crdupgrade

import (
	"encoding/json"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
)

// TestMigrationPlanWriteGuardBoundsWhatTheControllerMayPublish evaluates the
// sealed contract against manifests the controller could send, so the guard is
// judged by what it admits rather than by what its expressions say.
func TestMigrationPlanWriteGuardBoundsWhatTheControllerMayPublish(t *testing.T) {
	t.Parallel()

	controllerImage := "example.test/controller@sha256:" + strings.Repeat("1", 64)
	sha := func(character byte) string { return "sha256:" + strings.Repeat(string(character), 64) }
	plan := func() map[string]any {
		return map[string]any{
			"metadata": map[string]any{
				"name":   "ptah-mplan-0123456789abcdef01234567",
				"labels": map[string]any{"operator.ptah.run/migration": "orders"},
				"ownerReferences": []any{map[string]any{
					"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahMigration",
					"name": "orders", "uid": "migration-uid",
					"controller": true, "blockOwnerDeletion": true,
				}},
			},
			"spec": map[string]any{
				"contractVersion":          int64(1),
				"migrationRef":             map[string]any{"name": "orders", "uid": "migration-uid"},
				"fingerprint":              sha('a'),
				"historyFingerprint":       sha('b'),
				"artifactDigest":           sha('c'),
				"coordinationDigest":       sha('d'),
				"targetIdentityDigest":     sha('e'),
				"policyFingerprint":        sha('f'),
				"verificationPolicyUID":    "policy-uid",
				"verificationPolicyDigest": sha('9'),
				"currentVersion":           int64(2),
				"executionBindingID":       "v1-" + strings.Repeat("1", 32),
				"controllerImage":          controllerImage,
				"controllerRevision":       "test-revision",
				"controllerStateVersion":   int64(1),
				"ptahVersion":              "v0.3.0",
				"executorImage":            "example.test/executor@" + sha('2'),
				"runnerImage":              "example.test/runner@" + sha('3'),
				"runnerProtocolVersion":    int64(1),
				"migrations": []any{map[string]any{
					"version": int64(3), "checksum": "checksum-3", "transactionMode": "file",
				}},
			},
		}
	}

	if !admitsMigrationPlan(t, plan(), controllerImage) {
		t.Fatal("the guard refused a plan the controller publishes")
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "a name that does not follow from the fingerprint shape",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["name"] = "orders-plan"
			},
		},
		{
			name: "an owner that is not the migration the label names",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["labels"].(map[string]any)["operator.ptah.run/migration"] = "invoices"
			},
		},
		{
			name: "a schema plan's owner kind",
			mutate: func(object map[string]any) {
				owners := object["metadata"].(map[string]any)["ownerReferences"].([]any)
				owners[0].(map[string]any)["kind"] = "PtahSchema"
			},
		},
		{
			name: "an unpinned controller image",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["controllerImage"] = "example.test/controller:latest"
			},
		},
		{
			name: "another release's controller image",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["controllerImage"] = "example.test/controller@sha256:" + strings.Repeat("7", 64)
			},
		},
		{
			name: "a migration with no checksum",
			mutate: func(object map[string]any) {
				migrations := object["spec"].(map[string]any)["migrations"].([]any)
				migrations[0].(map[string]any)["checksum"] = ""
			},
		},
		{
			name: "a transaction mode nothing declares",
			mutate: func(object map[string]any) {
				migrations := object["spec"].(map[string]any)["migrations"].([]any)
				migrations[0].(map[string]any)["transactionMode"] = "batch"
			},
		},
		{
			name: "an empty sequence",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["migrations"] = []any{}
			},
		},
		{
			name: "injected status",
			mutate: func(object map[string]any) {
				object["status"] = map[string]any{"observedGeneration": int64(1)}
			},
		},
		{
			name: "an annotation the contract does not carry",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["annotations"] = map[string]any{"note": "hello"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			object := plan()
			test.mutate(object)
			if admitsMigrationPlan(t, object, controllerImage) {
				t.Fatal("the guard admitted a manifest outside the contract")
			}
		})
	}
}

func admitsMigrationPlan(t *testing.T, object map[string]any, controllerImage string) bool {
	t.Helper()

	// Round-trip through JSON so the CEL sees the same shapes the API server
	// would hand it.
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("params", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, validation := range controllerMigrationPlanWriteValidations("refused") {
		ast, issues := environment.Compile(validation.Expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile migration plan contract: %v", issues.Err())
		}
		program, err := environment.Program(ast)
		if err != nil {
			t.Fatalf("build migration plan contract: %v", err)
		}
		result, _, err := program.Eval(map[string]any{
			"object":    decoded,
			"oldObject": nil,
			"request":   map[string]any{"operation": "CREATE"},
			"params":    map[string]any{},
			"variables": map[string]any{
				"activeControllerImage":          controllerImage,
				"activeControllerState":          int64(1),
				"activeControllerStateString":    "1",
				"activeRelease":                  int64(2),
				"previousRelease":                int64(1),
				"isAnyAdmissionConvergenceProbe": false,
			},
		})
		if err != nil {
			// An expression that cannot be evaluated against this object is a
			// refusal the API server would apply as the policy's failure policy.
			return false
		}
		admitted, ok := result.Value().(bool)
		if !ok || !admitted {
			return false
		}
	}
	return true
}
