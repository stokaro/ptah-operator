package main

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

const (
	decidedPlan        = "shipments-p-14"
	decidedPlanUID     = types.UID("uid-p-14")
	decidedFingerprint = "sha256:14"
	storedApprovalUID  = types.UID("uid-approval-14")
)

// decidingMigration is the API server with a migration whose plan waits for a
// person, and the operator acting once the approval exists: each read of the
// migration after the create first applies the next step to its status.
type decidingMigration struct {
	*admitting
	steps []func(*operatorv1alpha1.PtahMigrationStatus)
}

func (d *decidingMigration) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	// The API server names the object it stores, and the in-memory client does not.
	object.SetUID(storedApprovalUID)
	return d.admitting.Create(ctx, object, options...)
}

func (d *decidingMigration) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, migration := object.(*operatorv1alpha1.PtahMigration); migration && len(d.created) > 0 && len(d.steps) > 0 {
		step := d.steps[0]
		d.steps = d.steps[1:]
		stored := &operatorv1alpha1.PtahMigration{}
		if err := d.admitting.Get(ctx, key, stored); err != nil {
			return err
		}
		step(&stored.Status)
		// This client serves no status subresource for a migration.
		if err := d.Update(ctx, stored); err != nil {
			return err
		}
	}
	return d.admitting.Get(ctx, key, object, options...)
}

func newDecidingMigration(t *testing.T, steps ...func(*operatorv1alpha1.PtahMigrationStatus)) *decidingMigration {
	t.Helper()
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments", UID: "uid-shipments"},
		Status: operatorv1alpha1.PtahMigrationStatus{
			Phase:   operatorv1alpha1.MigrationPhaseAwaitingApproval,
			History: &operatorv1alpha1.MigrationHistoryStatus{CurrentVersion: 13, ObservedAt: metav1.NewTime(observedAt)},
			Plan:    &operatorv1alpha1.ImmutableObjectReference{Name: decidedPlan, UID: decidedPlanUID},
			Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionTrue, Reason: "AwaitingApproval",
			}},
		},
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: decidedPlan, UID: decidedPlanUID},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{
			ContractVersion: 1, Fingerprint: decidedFingerprint,
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "shipments", UID: "uid-shipments"},
		},
	}
	return &decidingMigration{admitting: &admitting{Client: planreadtest.NewClient(t, migration, plan)}, steps: steps}
}

// runOfPlan is the status a finished, verified run leaves.
func runOfPlan(plan string, planUID, approvalUID types.UID) func(*operatorv1alpha1.PtahMigrationStatus) {
	return func(status *operatorv1alpha1.PtahMigrationStatus) {
		finished, read := metav1.NewTime(readAgainAt), metav1.NewTime(readAgainAt.Add(30_000_000_000))
		status.Phase, status.Plan = operatorv1alpha1.MigrationPhaseInSync, nil
		status.History.ObservedAt = read
		status.LastRun = &operatorv1alpha1.MigrationRunStatus{
			Outcome: operatorv1alpha1.MigrationRunOutcomeApplied, JobName: "run-1", JobUID: "job-uid-1",
			StartedAt: metav1.NewTime(readAgainAt.Add(-60_000_000_000)), FinishedAt: &finished,
			OperationID: "sha256:op", PlanRef: &operatorv1alpha1.ImmutableObjectReference{Name: plan, UID: planUID},
			Authorization: &operatorv1alpha1.MigrationRunAuthorizationStatus{
				Kind:        operatorv1alpha1.MigrationRunAuthorizedByApproval,
				ApprovalRef: &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: approvalUID},
			},
			Verification: &operatorv1alpha1.MigrationRunVerificationStatus{ObservedAt: read, Result: operatorv1alpha1.MigrationRunVerifiedInSync},
		}
	}
}

// approve --wait waits for the run of the plan a person reviewed under the approval
// admission stored. It reads both from what the command itself held, so another
// plan's run, however good, is not an answer.
func TestApproveWaitsForTheRunOfTheDecisionItStored(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps []func(*operatorv1alpha1.PtahMigrationStatus)
		code  int
		says  string
	}{
		{
			name:  "the run of this plan under this approval",
			steps: []func(*operatorv1alpha1.PtahMigrationStatus){runOfPlan(decidedPlan, decidedPlanUID, storedApprovalUID)},
			code:  exitOK, says: "reached applied.",
		},
		{
			// The regression: B, approved by somebody else, verified, before this
			// wait read again. It used to return zero.
			name:  "another plan's run, applied and verified",
			steps: []func(*operatorv1alpha1.PtahMigrationStatus){runOfPlan("shipments-p-15", "uid-p-15", "uid-approval-15")},
			code:  exitWillNotReach, says: "OtherPlanApplied",
		},
		{
			name:  "this plan under another approval",
			steps: []func(*operatorv1alpha1.PtahMigrationStatus){runOfPlan(decidedPlan, decidedPlanUID, "uid-approval-other")},
			code:  exitWillNotReach, says: "OtherApprovalConsumed",
		},
		{
			name: "another plan published in place of this one",
			steps: []func(*operatorv1alpha1.PtahMigrationStatus){func(status *operatorv1alpha1.PtahMigrationStatus) {
				status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: "shipments-p-15", UID: "uid-p-15"}
			}},
			code: exitWillNotReach, says: "PlanSuperseded",
		},
		{
			name: "a run that names no plan, from an operator older than the record",
			steps: []func(*operatorv1alpha1.PtahMigrationStatus){func(status *operatorv1alpha1.PtahMigrationStatus) {
				runOfPlan(decidedPlan, decidedPlanUID, storedApprovalUID)(status)
				status.LastRun.OperationID, status.LastRun.PlanRef, status.LastRun.Authorization, status.LastRun.Verification = "", nil, nil, nil
			}},
			code: exitWillNotReach, says: "CompletionNotProved",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newDecidingMigration(t, test.steps...)

				code, stdout, stderr := runApproveWith(t, api, "application", strings.NewReader(""), false,
					"migration/shipments", "--plan-fingerprint", decidedFingerprint, "--wait")

				if code != test.code || len(api.created) != 1 {
					t.Fatalf("approve --wait = %d after %d creates; stdout %q, stderr %q", code, len(api.created), stdout, stderr)
				}
				requireSays(t, "approve --wait's output", stdout+stderr, "Approved:", test.says)
			})
		})
	}
}

// -o json prints the approval first, and binds the wait to the same one.
func TestApproveWaitBindsTheApprovalItPrinted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newDecidingMigration(t, runOfPlan(decidedPlan, decidedPlanUID, "uid-approval-other"))

		code, stdout, stderr := runApproveWith(t, api, "application", strings.NewReader(""), false,
			"migration/shipments", "--plan-fingerprint", decidedFingerprint, "--wait", "-o", "json")

		if code != exitWillNotReach || len(api.created) != 1 || !strings.Contains(stderr, "OtherApprovalConsumed") {
			t.Fatalf("approve --wait -o json = %d after %d creates; stdout %q, stderr %q", code, len(api.created), stdout, stderr)
		}
	})
}
