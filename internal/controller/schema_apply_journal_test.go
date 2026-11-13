package controller

import (
	"context"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// journaled is every entry of type the schema's journal holds.
func journaled(schema *operatorv1alpha1.PtahSchema, entryType operatorv1alpha1.JournalEntryType) []operatorv1alpha1.JournalEntry {
	if schema.Status.Journal == nil {
		return nil
	}
	var found []operatorv1alpha1.JournalEntry
	for _, entry := range schema.Status.Journal.Entries {
		if entry.Type == entryType {
			found = append(found, entry)
		}
	}
	return found
}

// oneJournaled fails unless the journal holds exactly one entry of type, and
// returns it.
func oneJournaled(t *testing.T, schema *operatorv1alpha1.PtahSchema, entryType operatorv1alpha1.JournalEntryType) operatorv1alpha1.JournalEntry {
	t.Helper()
	found := journaled(schema, entryType)
	if len(found) != 1 {
		t.Fatalf("the journal holds %d %s entries for one Apply, want one: %#v", len(found), entryType, found)
	}
	return found[0]
}

// An approved Apply's ApplyFinished entry names what ran as the claim, the plan,
// the approval and the Job recorded it, as a migration's RunFinished does, and
// an Apply the database refused names the statement and the code the run
// reported. The proof that follows writes a Verified entry under the same
// operation ID, which is how a reader joins the two. Every value is compared
// with the object it came from, not with a copy the test made of it.
func TestAnApprovedApplyJournalsWhatRanAndItsProof(t *testing.T) {
	t.Parallel()

	refused := &dataplane.SchemaApplyFailure{Statement: 1, Statements: 1, SQLState: "23505"}
	for _, row := range []struct {
		name    string
		frame   func(*operatorv1alpha1.PtahSchema) runner.Result
		outcome operatorv1alpha1.JournalOutcome
		stopped *operatorv1alpha1.SchemaApplyFailureStatus
	}{
		{
			name: "an Apply that succeeded",
			frame: func(schema *operatorv1alpha1.PtahSchema) runner.Result {
				return runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
					MutationStarted: true, CoordinationDigest: schema.Status.Plan.CoordinationDigest,
					TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
				}
			},
			outcome: operatorv1alpha1.JournalSucceeded,
		},
		{
			// What the runner frames for testdata/statement-errors/pg-unique-apply.json,
			// which TestARefusedApplySaysWhereItStopped holds it to.
			name: "an Apply the database refused at a statement",
			frame: func(schema *operatorv1alpha1.PtahSchema) runner.Result {
				return runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
					ChildExitCode: 2, MutationStarted: true, Uncertain: true,
					CoordinationDigest: schema.Status.Plan.CoordinationDigest, TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
					Error: &runner.ResultError{
						Code: "apply_outcome_unknown", Message: "ptah sent the statements and returned an error, so how far they got is unknown",
					},
					ApplyFailure: refused,
				}
			},
			outcome: operatorv1alpha1.JournalUnknown,
			stopped: &operatorv1alpha1.SchemaApplyFailureStatus{Statement: 1, Statements: 1, SQLState: "23505"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			schema, plan, policyConfig := dispatchedApplyWithPlan(t)
			_, _, approval, _ := safetyApprovalFixture(t)
			schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
				Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
			}
			claim := schema.Status.ActiveOperation.DeepCopy()
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			reconciler, api := fakeReconciler(t, staticLogs{content: safetyRunnerFrame(t, row.frame(schema))},
				schema, plan, policyConfig, approval, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			harvested := safetyGetSchema(t, api, schema)
			pending := harvested.Status.PendingObservation
			if pending == nil || pending.ApplyOperationID != claim.ID {
				t.Fatalf("the Apply left no proof of its own claim: %#v", pending)
			}
			if !reflect.DeepEqual(pending.ApplyFailure, row.stopped) {
				t.Fatalf("status.pendingObservation.applyFailure = %#v, want %#v", pending.ApplyFailure, row.stopped)
			}
			ended := oneJournaled(t, harvested, operatorv1alpha1.JournalApplyFinished)
			for _, field := range []struct {
				name      string
				got, want any
			}{
				{"outcome", ended.Outcome, row.outcome},
				{"operationID", ended.OperationID, claim.ID},
				{"plan", ended.Plan, plan.Name},
				{"planUID", ended.PlanUID, plan.UID},
				{"planFingerprint", ended.PlanFingerprint, plan.Spec.Fingerprint},
				{"approval", ended.Approval, approval.Name},
				{"approvalUID", ended.ApprovalUID, approval.UID},
				{"approver", ended.Approver, approval.Spec.Approver.Username},
				{"jobName", ended.JobName, job.Name},
				{"jobUID", ended.JobUID, job.UID},
				{"applyFailure", ended.ApplyFailure, row.stopped},
			} {
				if !reflect.DeepEqual(field.got, field.want) {
					t.Errorf("ApplyFinished %s = %#v, want %#v", field.name, field.got, field.want)
				}
			}
			if t.Failed() {
				t.FailNow()
			}
			if found := journaled(harvested, operatorv1alpha1.JournalVerified); len(found) != 0 {
				t.Fatalf("the journal holds a Verified entry before the proof ran: %#v", found)
			}

			// The proof: garbage collection takes the Apply's Job and Pod, the
			// read finds raw drift, and the scoped plan that follows finds
			// nothing to change.
			for _, object := range []client.Object{pod, job} {
				if err := api.Delete(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			observing := reconcileSchema(t, reconciler, api, harvested)
			if operation := observing.Status.ActiveOperation; operation == nil || operation.Type != operatorv1alpha1.OperationObserve {
				t.Fatalf("the pass claimed %#v, want the proof's Observe", operation)
			}
			observed := nextProofRead(t, reconciler, api, observing, "observe", pending.Plan.TargetIdentityDigest, nil)
			planning := reconcileSchema(t, reconciler, api, observed)
			if operation := planning.Status.ActiveOperation; operation == nil || operation.Type != operatorv1alpha1.OperationPlan {
				t.Fatalf("the pass claimed %#v, want the proof's scoped Plan", operation)
			}
			settled := nextProofRead(t, reconciler, api, planning, "plan", pending.Plan.TargetIdentityDigest, nil)
			if settled.Status.PendingObservation != nil || settled.Status.Phase != operatorv1alpha1.PhaseInSync {
				t.Fatalf("the proof did not settle: phase %s, pending %#v", settled.Status.Phase, settled.Status.PendingObservation)
			}
			proved := oneJournaled(t, settled, operatorv1alpha1.JournalVerified)
			if proved.OperationID != ended.OperationID || proved.Plan != ended.Plan || proved.Outcome != operatorv1alpha1.JournalInSync {
				t.Fatalf("Verified = %#v, want InSync under operation %q and plan %q", proved, ended.OperationID, ended.Plan)
			}
			if again := oneJournaled(t, settled, operatorv1alpha1.JournalApplyFinished); !reflect.DeepEqual(again, ended) {
				t.Fatalf("the proof rewrote ApplyFinished: %#v, want %#v", again, ended)
			}
		})
	}
}

// A run that names no statement the database refused -- its commit failed, or
// it confirmed every statement and still did not succeed -- records none: the
// API admits a position from one, and a status write it refused would leave the
// Apply's claim where it stood. The outcome is unknown all the same.
func TestAnApplyThatNamesNoStatementRecordsNone(t *testing.T) {
	t.Parallel()

	for name, stopped := range map[string]*dataplane.SchemaApplyFailure{
		"the commit after every statement":               {Statements: 2, Commit: true, SQLState: "23503"},
		"every statement confirmed and still no success": {Statements: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			schema, plan, policyConfig := dispatchedApplyWithPlan(t)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			frame := safetyRunnerFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
				ChildExitCode: 2, MutationStarted: true, Uncertain: true,
				CoordinationDigest: schema.Status.Plan.CoordinationDigest, TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
				Error:        &runner.ResultError{Code: "apply_outcome_unknown", Message: "ptah sent the statements and returned an error"},
				ApplyFailure: stopped,
			})
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policyConfig, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			harvested := safetyGetSchema(t, api, schema)
			assertUnaccountedApply(t, harvested)
			if failure := harvested.Status.PendingObservation.ApplyFailure; failure != nil {
				t.Fatalf("status.pendingObservation.applyFailure = %#v, want none", *failure)
			}
			if ended := oneJournaled(t, harvested, operatorv1alpha1.JournalApplyFinished); ended.ApplyFailure != nil ||
				ended.Outcome != operatorv1alpha1.JournalUnknown {
				t.Fatalf("ApplyFinished = %#v, want Unknown with no applyFailure", ended)
			}
		})
	}
}
