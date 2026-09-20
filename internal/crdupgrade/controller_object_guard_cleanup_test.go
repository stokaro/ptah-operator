// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package crdupgrade

import (
	"testing"

	celgo "github.com/google/cel-go/cel"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// The cleanup TTL is the one field the manager may add to a Job it already
// created. Normally the Job must be terminal first; a schema Apply is the
// exception, because losing database lock continuity retires an Apply that is
// still running and the Job it leaves behind has nothing else to schedule its
// collection.
//
// The rule is evaluated here rather than matched as text, because a text
// marker cannot tell an exception that fires for the right shape from one that
// fires for every shape.
func TestControllerJobGuardCleanupTTLRequiresTerminalExceptForSchemaApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation string
		component string
		terminal  bool
		allowed   bool
	}{
		{name: "terminal read-only Job", operation: "observe", component: "schema-operation", terminal: true, allowed: true},
		{name: "running read-only Job", operation: "observe", component: "schema-operation", terminal: false, allowed: false},
		{name: "terminal schema Apply", operation: "apply", component: "schema-operation", terminal: true, allowed: true},
		{name: "running schema Apply", operation: "apply", component: "schema-operation", terminal: false, allowed: true},
		{name: "running migration Apply", operation: "apply", component: "migration-operation", terminal: false, allowed: false},
		{name: "running migration History", operation: "history", component: "migration-operation", terminal: false, allowed: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldJob := cleanupProbeJob(test.operation, test.component, test.terminal)
			job := cleanupProbeJob(test.operation, test.component, test.terminal)
			job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(300)

			allowed := evaluateCleanupTTLValidation(t, job, oldJob)
			if allowed != test.allowed {
				t.Fatalf("cleanup TTL validation allowed = %t, want %t", allowed, test.allowed)
			}
		})
	}
}

// A TTL that is not the exact nil-to-300 transition stays refused for every
// shape, exception included: the exception is about when the field may be
// added, never about what it may say.
func TestControllerJobGuardCleanupTTLValueIsExactForSchemaApply(t *testing.T) {
	t.Parallel()

	oldJob := cleanupProbeJob("apply", "schema-operation", false)
	job := cleanupProbeJob("apply", "schema-operation", false)
	job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(301)
	if evaluateCleanupTTLValidation(t, job, oldJob) {
		t.Fatal("cleanup TTL validation accepted a TTL other than 300 on a running Apply")
	}

	replaced := cleanupProbeJob("apply", "schema-operation", false)
	replaced["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(300)
	previous := cleanupProbeJob("apply", "schema-operation", false)
	previous["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(300)
	if evaluateCleanupTTLValidation(t, replaced, previous) {
		t.Fatal("cleanup TTL validation accepted an update to a Job that already carried a TTL")
	}
}

// The exception reads the Job's labels, and a Job without them must not make
// the expression error out into an evaluation failure.
func TestControllerJobGuardCleanupTTLRefusesALabellessRunningJob(t *testing.T) {
	t.Parallel()

	oldJob := cleanupProbeJob("apply", "schema-operation", false)
	delete(oldJob["metadata"].(map[string]any), "labels")
	job := cleanupProbeJob("apply", "schema-operation", false)
	delete(job["metadata"].(map[string]any), "labels")
	job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(300)

	if evaluateCleanupTTLValidation(t, job, oldJob) {
		t.Fatal("cleanup TTL validation accepted a running Job with no labels")
	}
}

// cleanupProbeJob builds the smallest object the cleanup validation reads: the
// identity it compares between the two revisions, the labels the exception
// looks at, and a status that either carries a terminal condition or does not.
func cleanupProbeJob(operation, component string, terminal bool) map[string]any {
	status := map[string]any{}
	if terminal {
		status["conditions"] = []any{
			map[string]any{"type": "Complete", "status": "True"},
		}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":      "ptah-" + operation + "-orders-0123456789abcdef",
			"namespace": "team-a",
			"uid":       "job-uid",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by":   "ptah-operator",
				"app.kubernetes.io/component":    component,
				"operator.ptah.run/schema":       "orders",
				"operator.ptah.run/operation":    operation,
				"operator.ptah.run/operation-id": "0123456789abcdef",
			},
			"annotations":     map[string]any{"operator.ptah.run/operation-id": "0123456789abcdef"},
			"ownerReferences": []any{map[string]any{"kind": "PtahSchema", "name": "orders"}},
		},
		"spec": map[string]any{
			"parallelism":           int64(1),
			"completions":           int64(1),
			"activeDeadlineSeconds": int64(900),
			"backoffLimit":          int64(0),
			"manualSelector":        false,
			"completionMode":        "NonIndexed",
			"suspend":               false,
			"podReplacementPolicy":  "Failed",
			"template":              map[string]any{"spec": map[string]any{"restartPolicy": "Never"}},
		},
		"status": status,
	}
}

// evaluateCleanupTTLValidation runs the one validation that authorizes the
// cleanup TTL, against an UPDATE request.
func evaluateCleanupTTLValidation(t *testing.T, job, oldJob map[string]any) bool {
	t.Helper()
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	validations := controllerJobWriteValidations("test")
	index := controllerObjectValidationIndex(t, validations,
		`!has(dyn(oldObject).spec.ttlSecondsAfterFinished)`)
	return evaluateControllerJobUpdate(t, environment, validations[index], job, oldJob)
}

func evaluateControllerJobUpdate(
	t *testing.T,
	environment *celgo.Env,
	validation admissionregistrationv1.Validation,
	job, oldJob map[string]any,
) bool {
	t.Helper()
	ast, issues := environment.Compile(validation.Expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile the cleanup validation: %v", issues.Err())
	}
	program, programErr := environment.Program(ast)
	if programErr != nil {
		t.Fatalf("build the cleanup validation: %v", programErr)
	}
	result, _, evaluationErr := program.Eval(map[string]any{
		"object":    job,
		"oldObject": oldJob,
		"request":   map[string]any{"operation": "UPDATE"},
		"variables": map[string]any{},
	})
	if evaluationErr != nil {
		// An expression that errors denies the request in Kubernetes, so the
		// test reports the same answer rather than failing the run.
		return false
	}
	allowed, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("cleanup validation result = %T(%v), want bool", result.Value(), result.Value())
	}
	return allowed
}
