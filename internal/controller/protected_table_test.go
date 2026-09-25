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

package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A plan Ptah refused because it would change a fenced table is a refusal, not
// a fault: the conditions name the fence, no plan is published, and the
// resource keeps reconciling, because the refusal stands until the fence goes
// or the artifact stops asking for the change.
func TestAPlanThatWouldChangeAProtectedTableIsRefusedByName(t *testing.T) {
	t.Parallel()

	// A planning run: a verified source, an observed target, and the fence in
	// the policy the plan is computed under.
	schema := safetyApplySchema(t)
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Status.Plan = nil
	schema.Status.Source.Verified = true
	schema.Status.Source.Digest = testDigest
	schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
	schema.Status.Target.DriftReportDigest = testDigest
	schema.Spec.Policy.ProtectedTables = []string{"countries", "ref.regions"}
	schema.Status.ActiveOperation.Type = operatorv1alpha1.OperationPlan
	schema.Status.ActiveOperation.ID = "plan-operation"
	schema.Status.ActiveOperation.JobName = "plan-job"
	schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
	schema.Status.ActiveOperation.ObservationProtectedTables = []string{"countries", "ref.regions"}
	bindActiveInput(t, schema)
	// The runner writes its frame and exits zero: a complete frame is the Job's
	// transport success, and the refusal lives in the payload.
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion,
		Operation:       runner.OperationPlan,
		OperationID:     schema.Status.ActiveOperation.ID,
		ChildExitCode:   1,
		Error: &runner.ResultError{
			Code: "protected_table",
			Message: "refusing to change protected table(s) countries: a protected table is fenced off from " +
				"the declarative path, which has no override",
		},
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(schema),
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := safetyGetSchema(t, api, schema)
	if actual.Status.Plan != nil {
		t.Fatalf("a refused plan was published: %#v", actual.Status.Plan)
	}
	for _, expected := range []struct {
		condition string
		reason    operatorv1alpha1.ConditionReason
	}{
		{operatorv1alpha1.ConditionPlanReady, operatorv1alpha1.ReasonProtectedTable},
		{operatorv1alpha1.ConditionInSync, operatorv1alpha1.ReasonProtectedTable},
		{operatorv1alpha1.ConditionReady, operatorv1alpha1.ReasonProtectedTable},
	} {
		if !conditionMatches(actual.Status.Conditions, expected.condition, metav1.ConditionFalse, expected.reason) {
			t.Fatalf("%s does not name the fence: %#v", expected.condition, actual.Status.Conditions)
		}
	}
	// A refusal is not a fault: nothing went wrong, and the answer will not
	// change until the policy or the artifact does. The resource reads as
	// blocked, and no failure is reported for it.
	if actual.Status.Phase != operatorv1alpha1.PhaseBlocked {
		t.Fatalf("a fenced plan left the resource in phase %s", actual.Status.Phase)
	}
	failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed != nil && failed.Status == metav1.ConditionTrue {
		t.Fatalf("a refusal was reported as a failure: %#v", failed)
	}
	// The operation ends here rather than retrying: a re-plan would be refused
	// again for as long as the fence and the artifact disagree. The blocked
	// interval is what picks the answer up again, so removing the entry or
	// publishing an artifact that agrees with the rows converges without a
	// person touching the resource.
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the refusal left an operation to retry: %#v", actual.Status.ActiveOperation)
	}
	if actual.Status.NextReconciliationTime == nil {
		t.Fatalf("the refusal named no next reconciliation: %#v", actual.Status)
	}
}

// The fence is part of the policy a plan is computed under, so editing it makes
// a published plan stale. Without that, a plan computed with no fence could be
// approved and applied after one was added.
func TestEditingTheFenceChangesThePolicyFingerprint(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	before, err := policyFingerprint(schema)
	if err != nil {
		t.Fatalf("policyFingerprint() error = %v", err)
	}
	schema.Spec.Policy.ProtectedTables = []string{"countries"}
	fenced, err := policyFingerprint(schema)
	if err != nil {
		t.Fatalf("policyFingerprint() error = %v", err)
	}
	if fenced == before {
		t.Fatal("adding a protected table left the policy fingerprint unchanged")
	}
	// Order and repetition are not the policy: the same fence written twice, or
	// in another order, is the same fence.
	schema.Spec.Policy.ProtectedTables = []string{"countries", "countries"}
	repeated, err := policyFingerprint(schema)
	if err != nil {
		t.Fatalf("policyFingerprint() error = %v", err)
	}
	if repeated != fenced {
		t.Fatal("a repeated entry changed the policy fingerprint")
	}
	schema.Spec.Policy.ProtectedTables = []string{"ref.regions", "countries"}
	reordered, err := policyFingerprint(schema)
	if err != nil {
		t.Fatalf("policyFingerprint() error = %v", err)
	}
	schema.Spec.Policy.ProtectedTables = []string{"countries", "ref.regions"}
	ordered, err := policyFingerprint(schema)
	if err != nil {
		t.Fatalf("policyFingerprint() error = %v", err)
	}
	if reordered != ordered {
		t.Fatal("the order of the entries changed the policy fingerprint")
	}
	if !strings.HasPrefix(fingerprint.NormalizeSet([]string{"countries"})[0], "countries") {
		t.Fatal("the fence is not normalized as a set")
	}
}

// A fence added while an Apply's proof is outstanding makes the pending
// observation stale: the plan that proof belongs to was computed without it.
func TestAFenceAddedDuringProofMakesThePendingObservationStale(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	pending := schema.Status.PendingObservation
	if pending == nil {
		t.Fatal("the fixture carries no pending observation")
	}
	// The whole predicate is about more than the fence, and this fixture is an
	// Apply's proof rather than a converged resource, so what is measured is
	// the difference the fence makes: the same record, read against a schema
	// that agrees with it and against one that does not.
	pending.ProtectedTables = []string{"countries"}
	schema.Spec.Policy.ProtectedTables = []string{"countries"}
	// The record has to agree with the schema in every other respect, or the
	// rows below pass against a record that matches nothing and the fence goes
	// unmeasured. The fixture carries a deliberately different policy
	// fingerprint, so it is recomputed here from the schema the comparison
	// reads.
	fingerprintOfPolicy, err := policyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	pending.Plan.PolicyFingerprint = fingerprintOfPolicy
	agreeing := pendingMatchesCurrentSchema(schema, pending)
	if !agreeing {
		t.Fatal("the record does not match its schema before the fence is touched, so nothing below is measured")
	}
	schema.Spec.Policy.ProtectedTables = []string{"countries", "ref.regions"}
	widened := pendingMatchesCurrentSchema(schema, pending)
	if widened {
		t.Fatal("a fence widened during proof left the pending observation current")
	}
	schema.Spec.Policy.ProtectedTables = nil
	removed := pendingMatchesCurrentSchema(schema, pending)
	if removed {
		t.Fatal("a fence removed during proof left the pending observation current")
	}
	// And the comparison is not simply refusing everything: the record that
	// agrees has to be accepted on the same inputs, or this measures nothing.
	schema.Spec.Policy.ProtectedTables = []string{"countries"}
	if pendingMatchesCurrentSchema(schema, pending) != agreeing {
		t.Fatal("the same fence was read two ways")
	}
	// Moving the schema's fence moves the policy fingerprint with it, so the
	// rows above are refused by the digest and say nothing about the fence
	// field itself. This one moves the record instead: the schema, and every
	// digest computed from it, is left exactly as it was, so the only thing
	// that can refuse is the comparison of the two fences.
	pending.ProtectedTables = []string{"countries", "ref.regions"}
	if pendingMatchesCurrentSchema(schema, pending) {
		t.Fatal("a record fencing tables the schema does not read as current")
	}
	pending.ProtectedTables = nil
	if pendingMatchesCurrentSchema(schema, pending) {
		t.Fatal("a record fencing nothing read as current against a schema that fences a table")
	}
	pending.ProtectedTables = []string{"countries"}
	if !pendingMatchesCurrentSchema(schema, pending) {
		t.Fatal("restoring the record's fence did not restore the match")
	}
}
