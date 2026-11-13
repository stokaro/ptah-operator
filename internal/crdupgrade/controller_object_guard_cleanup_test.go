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
	"fmt"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"

	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/workload"
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

// The field is added once. The value is the two validations below: the exception
// is about when the field may be added, never about what it may say.
func TestControllerJobGuardCleanupTTLIsAddedOnce(t *testing.T) {
	t.Parallel()

	replaced := cleanupProbeJob("apply", "schema-operation", false)
	replaced["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(86400)
	previous := cleanupProbeJob("apply", "schema-operation", false)
	previous["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(300)
	if evaluateCleanupTTLValidation(t, replaced, previous) {
		t.Fatal("cleanup TTL validation accepted an update to a Job that already carried a TTL")
	}
}

// TestControllerJobGuardNamesWhichCleanupTTLAJobMayCarry evaluates the two
// validations that say so, each with a message of its own. The guard sees only
// the Job, which does not say whether an Apply succeeded: the webhook reads that
// from the Pod's summary, so a terminal Apply passes here whatever its run did.
func TestControllerJobGuardNamesWhichCleanupTTLAJobMayCarry(t *testing.T) {
	t.Parallel()

	const (
		admitted = ""
		value    = controllerJobCleanupTTLMessage
		long     = controllerJobRetentionTTLMessage
	)
	for _, test := range []struct {
		name               string
		operation, subject string
		ttl                int64
		refusedBy          string
	}{
		{"the short TTL on a read-only Job", "observe", "schema-operation", 300, admitted},
		{"the short TTL on a schema Apply", "apply", "schema-operation", 300, admitted},
		{"the short TTL on a migration Apply", "apply", "migration-operation", 300, admitted},
		{"the long TTL on a schema Apply", "apply", "schema-operation", 86400, admitted},
		{"the long TTL on a migration Apply", "apply", "migration-operation", 86400, admitted},
		{"the long TTL on an Observe", "observe", "schema-operation", 86400, long},
		{"the long TTL on a Plan", "plan", "schema-operation", 86400, long},
		{"the long TTL on a Resolve", "resolve", "schema-operation", 86400, long},
		{"the long TTL on a history read", "history", "migration-operation", 86400, long},
		{"a TTL just over the short one", "apply", "schema-operation", 301, value},
		{"a TTL just under the long one", "apply", "schema-operation", 86399, value},
		{"no time at all", "apply", "schema-operation", 0, value},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job := cleanupProbeJob(test.operation, test.subject, true)
			job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = test.ttl
			got := admitted
			for _, validation := range cleanupTTLValidations(t) {
				if !evaluateControllerJobUpdate(t, cleanupEnvironment(t), validation, job, cleanupProbeJob(test.operation, test.subject, true)) {
					got = validation.Message
					break
				}
			}
			if got != test.refusedBy {
				t.Fatalf("refused by %q, want %q", got, test.refusedBy)
			}
		})
	}

	t.Run("a Job with no labels", func(t *testing.T) {
		t.Parallel()
		job := cleanupProbeJob("apply", "schema-operation", true)
		delete(job["metadata"].(map[string]any), "labels")
		job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = int64(86400)
		if evaluateControllerJobUpdate(t, cleanupEnvironment(t), cleanupTTLValidations(t)[1], job, job) {
			t.Fatal("the long TTL was admitted on a Job that does not say it is an Apply")
		}
	})
	t.Run("a Job with no TTL", func(t *testing.T) {
		t.Parallel()
		job := cleanupProbeJob("observe", "schema-operation", true)
		for _, validation := range cleanupTTLValidations(t) {
			if !evaluateControllerJobUpdate(t, cleanupEnvironment(t), validation, job, job) {
				t.Fatalf("%q refused a Job that names no TTL", validation.Message)
			}
		}
	})
}

// TestControllerJobGuardCreatesOnlyAReadOnlyJobWithItsCleanupTTL evaluates the
// validation that holds a create's TTL, and only it: the shape validations leave
// the field to it, so a refusal names the reason. A read-only Job is created with
// the short TTL, or with none as a manager of the release before this one creates
// it; an Apply Job is created with none.
func TestControllerJobGuardCreatesOnlyAReadOnlyJobWithItsCleanupTTL(t *testing.T) {
	t.Parallel()

	validation := creationTTLValidation(t)
	short, long := int64(jobclaim.CleanupTTLSeconds), int64(jobclaim.FailedApplyRetentionTTLSeconds)
	for _, test := range []struct {
		name               string
		operation, subject string
		ttl                *int64
		admitted           bool
	}{
		{"a schema Resolve with the short TTL", "resolve", "schema-operation", &short, true},
		{"a schema Verify with the short TTL", "verify", "schema-operation", &short, true},
		{"an Observe with the short TTL", "observe", "schema-operation", &short, true},
		{"a Plan with the short TTL", "plan", "schema-operation", &short, true},
		{"a migration Resolve with the short TTL", "resolve", "migration-operation", &short, true},
		{"a migration Verify with the short TTL", "verify", "migration-operation", &short, true},
		{"a history read with the short TTL", "history", "migration-operation", &short, true},
		{"an Observe with no TTL", "observe", "schema-operation", nil, true},
		{"a schema Apply with no TTL", "apply", "schema-operation", nil, true},
		{"a migration Apply with no TTL", "apply", "migration-operation", nil, true},
		{"a schema Apply with the short TTL", "apply", "schema-operation", &short, false},
		{"a migration Apply with the short TTL", "apply", "migration-operation", &short, false},
		{"a schema Apply with the long TTL", "apply", "schema-operation", &long, false},
		{"an Observe with the long TTL", "observe", "schema-operation", &long, false},
		{"a history read just over the short TTL", "history", "migration-operation", ptrTo(short + 1), false},
		{"a Resolve with no time at all", "resolve", "schema-operation", ptrTo(0), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job := cleanupProbeJob(test.operation, test.subject, false)
			if test.ttl != nil {
				job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = *test.ttl
			}
			if got := evaluateControllerJobRequest(t, cleanupEnvironment(t), "CREATE", validation, job, nil); got != test.admitted {
				t.Fatalf("admitted = %t, want %t", got, test.admitted)
			}
		})
	}

	t.Run("a Job with no labels", func(t *testing.T) {
		t.Parallel()
		job := cleanupProbeJob("observe", "schema-operation", false)
		delete(job["metadata"].(map[string]any), "labels")
		job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = short
		if evaluateControllerJobRequest(t, cleanupEnvironment(t), "CREATE", validation, job, nil) {
			t.Fatal("the short TTL was admitted at CREATE on a Job that does not say it is a read")
		}
	})
	// The validation is for a create. An update is the other validations' to
	// judge, and this one leaves it alone whatever it carries.
	t.Run("an update", func(t *testing.T) {
		t.Parallel()
		job := cleanupProbeJob("apply", "schema-operation", true)
		job["spec"].(map[string]any)["ttlSecondsAfterFinished"] = long
		if !evaluateControllerJobRequest(t, cleanupEnvironment(t), "UPDATE", validation, job, cleanupProbeJob("apply", "schema-operation", true)) {
			t.Fatal("the creation TTL validation refused an update")
		}
	})
	// The shape the create also has to satisfy says nothing about the TTL, so a
	// refusal of the field is this validation's and names its reason.
	t.Run("the shape leaves the TTL to it", func(t *testing.T) {
		t.Parallel()
		for _, expression := range validationExpressions(controllerJobWriteValidations("test")) {
			if expression != controllerJobCreationTTLValidation && strings.Contains(expression, `request.operation == "CREATE" && !has(dyn(object).spec.ttlSecondsAfterFinished)`) {
				t.Fatalf("a shape validation refuses a TTL at CREATE itself: %s", expression)
			}
		}
	})
}

func ptrTo(value int64) *int64 { return &value }

// creationTTLValidation is the one validation carrying the creation message,
// found in what the guard installs rather than taken from the constant.
func creationTTLValidation(t *testing.T) admissionregistrationv1.Validation {
	t.Helper()
	var found []admissionregistrationv1.Validation
	for _, validation := range controllerJobWriteValidations("the guard's own message") {
		if validation.Message == controllerJobCreationTTLMessage {
			found = append(found, validation)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the guard installs %d creation TTL validations, want 1", len(found))
	}
	return found[0]
}

// The guard writes the values as literals, because a chart template holds
// them. This keeps them the values the manager writes.
func TestControllerJobGuardTTLsAreTheOnesTheManagerWrites(t *testing.T) {
	t.Parallel()

	values := fmt.Sprintf("[%d, %d]", jobclaim.CleanupTTLSeconds, jobclaim.FailedApplyRetentionTTLSeconds)
	if !strings.Contains(controllerJobCleanupTTLValidation, values) {
		t.Errorf("the guard does not admit %s: %s", values, controllerJobCleanupTTLValidation)
	}
	created := fmt.Sprintf("ttlSecondsAfterFinished == %d", workload.CleanupTTLSeconds)
	if !strings.Contains(controllerJobCreationTTLValidation, created) {
		t.Errorf("the guard does not create a read-only Job with %q: %s", created, controllerJobCreationTTLValidation)
	}
	long := fmt.Sprintf("!= %d", jobclaim.FailedApplyRetentionTTLSeconds)
	if !strings.Contains(controllerJobRetentionTTLValidation, long) {
		t.Errorf("the guard does not hold the long TTL %q: %s", long, controllerJobRetentionTTLValidation)
	}
}

func cleanupEnvironment(t *testing.T) *celgo.Env {
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
	return environment
}

// cleanupTTLValidations are the validations carrying the two messages, found in
// what the guard installs rather than taken from the constants.
func cleanupTTLValidations(t *testing.T) []admissionregistrationv1.Validation {
	t.Helper()
	var found []admissionregistrationv1.Validation
	for _, validation := range controllerJobWriteValidations("the guard's own message") {
		if validation.Message == controllerJobCleanupTTLMessage || validation.Message == controllerJobRetentionTTLMessage {
			found = append(found, validation)
		}
	}
	if len(found) != 2 {
		t.Fatalf("the guard installs %d cleanup TTL validations, want 2", len(found))
	}
	return found
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
	return evaluateControllerJobRequest(t, environment, "UPDATE", validation, job, oldJob)
}

// evaluateControllerJobRequest runs one validation against a request of the
// operation given. A create has no old object, which CEL reads as null.
func evaluateControllerJobRequest(
	t *testing.T,
	environment *celgo.Env,
	operation string,
	validation admissionregistrationv1.Validation,
	job, oldJob map[string]any,
) bool {
	t.Helper()
	var old any
	if oldJob != nil {
		old = oldJob
	}
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
		"oldObject": old,
		"request":   map[string]any{"operation": operation},
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
