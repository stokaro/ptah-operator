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
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// refusedReplans are the two Plans that end with nothing to publish, with the
// reason each one's conditions carry.
func refusedReplans() []struct {
	code   string
	reason operatorv1alpha1.ConditionReason
} {
	return []struct {
		code   string
		reason operatorv1alpha1.ConditionReason
	}{
		{code: "protected_table", reason: operatorv1alpha1.ReasonProtectedTable},
		{code: runner.CodeDifferenceNotPlannable, reason: operatorv1alpha1.ReasonDifferenceNotPlannable},
	}
}

// A Plan outside a proof that follows a plan waiting for an approval, and ends
// with nothing to publish, leaves no plan anybody can approve, so it stops
// asking for one. The refusal stays on the conditions a reader looks at: the
// conditions false with its reason are exactly InSync, PlanReady and Ready,
// which is the reading the acceptance row waits for.
//
// The control is the same refusal with nothing waiting, which leaves
// ApprovalRequired as it was: without it, a fixture that never reached the
// reset could not tell a reset from a condition nothing wrote.
func TestARefusedReplanEndsTheWaitForApproval(t *testing.T) {
	t.Parallel()
	for _, end := range refusedReplans() {
		for _, row := range []struct {
			name    string
			before  metav1.Condition
			wantFor func(metav1.Condition) metav1.Condition
		}{
			{
				name: "a plan waiting for approval",
				before: metav1.Condition{
					Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue,
					Reason: string(operatorv1alpha1.ReasonPlanReady), Message: "An exact immutable plan is ready for approval",
				},
				wantFor: func(metav1.Condition) metav1.Condition {
					return metav1.Condition{
						Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse,
						Reason: string(operatorv1alpha1.ReasonPlanNoLongerCurrent),
					}
				},
			},
			{
				name: "control: nothing waiting",
				before: metav1.Condition{
					Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse,
					Reason: string(operatorv1alpha1.ReasonSatisfied), Message: "Current plan approval requirements are satisfied",
				},
				wantFor: func(before metav1.Condition) metav1.Condition { return before },
			},
		} {
			t.Run(end.code+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				schema := planningSchema(t)
				meta.SetStatusCondition(&schema.Status.Conditions, row.before)
				actual, _, _ := reconcilePlanResult(t, schema, runner.Result{
					ProtocolVersion: runner.ProtocolVersion,
					Operation:       runner.OperationPlan,
					OperationID:     schema.Status.ActiveOperation.ID,
					Error:           &runner.ResultError{Code: end.code, Message: unplannableNote},
				})
				if actual.Status.Phase != operatorv1alpha1.PhaseBlocked || actual.Status.Plan != nil {
					t.Fatalf("phase %s, plan %v; want Blocked and no plan", actual.Status.Phase, actual.Status.Plan)
				}
				want := row.wantFor(row.before)
				approval := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
				if approval == nil || approval.Status != want.Status || approval.Reason != want.Reason {
					t.Fatalf("ApprovalRequired = %#v, want %s/%s", approval, want.Status, want.Reason)
				}
				var refused []string
				for _, condition := range actual.Status.Conditions {
					if condition.Status == metav1.ConditionFalse && condition.Reason == string(end.reason) {
						refused = append(refused, condition.Type)
					}
				}
				slices.Sort(refused)
				if want := []string{operatorv1alpha1.ConditionInSync, operatorv1alpha1.ConditionPlanReady, operatorv1alpha1.ConditionReady}; !slices.Equal(refused, want) {
					t.Fatalf("conditions false with %s = %v, want exactly %v", end.reason, refused, want)
				}
			})
		}
	}
}

// What the journal keeps of a plan a refused re-plan took away. It writes no
// entry, as for a plan the database converged past: OutstandingPlan still names
// the plan until something carries it out or another is published. What the
// refusal decides is whether the next plan replaced it.
//
// A plan nobody approved went away with the refusal, so the next plan replaces
// nothing and no PlanStale Warning names it. A plan someone approved and that
// never ran is still the change the database lacks, so the next plan replaces
// it, and the Warning tells the approver their approval will not run. A
// condition left asking for an approval would make the first plan read as
// replaced too; NoChanges in its place would make the second read as not.
func TestWhatTheJournalKeepsOfAPlanARefusedReplanTookAway(t *testing.T) {
	t.Parallel()
	for _, end := range refusedReplans() {
		for _, row := range []struct {
			name     string
			approved bool
		}{
			{name: "a plan nobody approved"},
			{name: "an approved plan that never ran", approved: true},
		} {
			t.Run(end.code+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				walk := newPlanWalk(t, operatorv1alpha1.ApplyPolicyOnApproval)
				first := walk.waitingPlan("plan-1", "observed-state")
				planStaleEvents(walk.recorder)
				if row.approved {
					// What an Apply claim leaves when it is withdrawn before
					// dispatch.
					withdrawn := walk.stored()
					withdrawn.Status.Journal.Entries = append(withdrawn.Status.Journal.Entries, operatorv1alpha1.JournalEntry{
						Type: operatorv1alpha1.JournalApprovalAccepted, At: metav1.Now(), Plan: first,
						Approval: "approve-first", ApprovalUID: "approval-uid",
					})
					setCondition(withdrawn, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonSatisfied, "fixture")
					if err := walk.api.Status().Update(context.Background(), withdrawn); err != nil {
						t.Fatal(err)
					}
				}

				walk.read("observe-2")
				before := walk.stored().Status.Journal.DeepCopy()
				claimed := walk.claim(operatorv1alpha1.OperationPlan, "plan-2")
				result := runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: "plan-2",
					CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
					Error: &runner.ResultError{Code: end.code, Message: unplannableNote},
				}
				planEnd, ok := planEndFor(operatorv1alpha1.OperationPlan, result.Error)
				if !ok {
					t.Fatalf("%s is not a Plan that ends with nothing to publish", end.code)
				}
				if _, err := walk.reconciler.endRefusedPlan(context.Background(), claimed, nil, result, planEnd, nil, 0, false); err != nil {
					t.Fatalf("harvest the refused Plan: %v", err)
				}
				refused := walk.stored()
				if refused.Status.Phase != operatorv1alpha1.PhaseBlocked || refused.Status.Plan != nil {
					t.Fatalf("the refused Plan left phase %s and plan %v", refused.Status.Phase, refused.Status.Plan)
				}
				if !conditionMatches(refused.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPlanNoLongerCurrent) {
					t.Fatalf("ApprovalRequired after the refusal = %#v, want False/PlanNoLongerCurrent",
						findCondition(refused.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired))
				}
				if !equality.Semantic.DeepEqual(before, refused.Status.Journal) {
					t.Fatalf("the refusal wrote to the journal:\nbefore %+v\nafter  %+v", before, refused.Status.Journal)
				}

				walk.read("observe-3")
				second := walk.waitingPlan("plan-3", "moved-state")
				if second == first {
					t.Fatalf("a plan from another observed state kept the name %s; this row needs another plan", first)
				}
				replaced := ""
				if row.approved {
					replaced = first
				}
				walk.assertReplacement(walk.stored(), replaced)
			})
		}
	}
}
