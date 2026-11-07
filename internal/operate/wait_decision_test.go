package operate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// The decision an approve --wait names: approval A of plan A.
var decisionA = operate.Decision{
	PlanName: "plan-a", PlanUID: "plan-a-uid", PlanFingerprint: "sha256:a",
	ApprovalName: "approve-a", ApprovalUID: "approve-a-uid",
}

// run is a finished run's record, carrying its identity. Seconds are the fixture's
// clock: the claim starts at start and the run is read a minute later.
func run(outcome operatorv1alpha1.MigrationRunOutcome, operation, plan, approval string, start int) *runDoc {
	record := &runDoc{
		outcome: outcome, job: "job-" + operation, jobUID: types.UID("job-" + operation + "-uid"),
		startedAt: at(start), finishedAt: at(start + 60),
		operationID: operation, plan: plan, planUID: types.UID(plan + "-uid"), fingerprint: "sha256:" + strings.TrimPrefix(plan, "plan-"),
	}
	if approval != "" {
		record.approval, record.approvalUID = approval, types.UID(approval+"-uid")
	}
	return record
}

func verified(record *runDoc, result operatorv1alpha1.MigrationRunVerificationResult, reading int) *runDoc {
	record.verification = result
	record.verifiedAt = at(reading)
	return record
}

// ended is the RunFinished entry the manager writes for a run.
func ended(outcome operatorv1alpha1.JournalOutcome, operation, plan, approval string, finished int) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: metav1.NewTime(at(finished)), Outcome: outcome, OperationID: operation,
		Plan: plan, PlanUID: types.UID(plan + "-uid"), PlanFingerprint: "sha256:" + strings.TrimPrefix(plan, "plan-"),
		Approval: approval, ApprovalUID: types.UID(approval + "-uid"),
	}
}

func readBack(outcome operatorv1alpha1.JournalOutcome, operation, plan string, reading int) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(at(reading)), Outcome: outcome, OperationID: operation,
		Plan: plan, PlanUID: types.UID(plan + "-uid"),
	}
}

func planDocOf(plan string) *planDoc {
	return &planDoc{name: plan, uid: types.UID(plan + "-uid")}
}

// The resource at the gate when the approval is stored: the wait's baseline.
func atTheGate(earlier *runDoc, journal ...operatorv1alpha1.JournalEntry) *doc {
	return &doc{
		phase: "AwaitingApproval", observedAt: at(0), plan: planDocOf("plan-a"), lastRun: earlier, journal: journal,
		conditions: approvalConditions(metav1.ConditionTrue),
	}
}

func TestWaitForADecisionIsTheRunThatCarriedItOut(t *testing.T) {
	t.Parallel()

	const (
		applied = operatorv1alpha1.MigrationRunOutcomeApplied
		failed  = operatorv1alpha1.MigrationRunOutcomeFailed
		partial = operatorv1alpha1.MigrationRunOutcomePartial
		unknown = operatorv1alpha1.MigrationRunOutcomeUnknown
		upToDat = operatorv1alpha1.MigrationRunOutcomeUpToDate
	)
	rows := []struct {
		name string
		// plan names the wait by name or fingerprint rather than by decision.
		plan     string
		decision *operate.Decision
		since    *doc
		docs     []doc
		// wantReads is the read whose document meets the target.
		wantReads   int
		wantStop    operate.StopReason
		wantTimeout bool
		wantMessage string
	}{
		{
			name: "the run of this decision, once its own reading is recorded", since: atTheGate(nil),
			docs: []doc{
				*atTheGate(nil),
				{phase: "Applying", observedAt: at(0), operation: "Apply", operationPlan: planDocOf("plan-a"), plan: planDocOf("plan-a")},
				{phase: "VerifyingHistory", observedAt: at(0), lastRun: run(applied, "op-a", "plan-a", "approve-a", 20)},
				{phase: "VerifyingHistory", observedAt: at(0), operation: "History", lastRun: run(applied, "op-a", "plan-a", "approve-a", 20)},
				{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "InSync", 90)},
			},
			wantReads: 5,
		},
		{
			name: "the run's own account is not the reading", since: atTheGate(nil),
			docs: []doc{
				{phase: "VerifyingHistory", observedAt: at(0), lastRun: run(applied, "op-a", "plan-a", "approve-a", 20)},
			},
			wantTimeout: true, wantMessage: "the history read back after run job-op-a",
		},
		{
			// Mid-cycle: a reading of the next interval is under way by the
			// time this document is read. The run's record says what it says
			// whatever phase the resource passes through.
			name: "the reading stands whatever the resource does next", since: atTheGate(nil),
			docs: []doc{
				{phase: "Resolving", observedAt: at(90), operation: "Resolve", lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "InSync", 90)},
			},
			wantReads: 1,
		},
		{
			// The source moved on while the run ran: the reading found more
			// to apply, which is about the source and not about this run.
			name: "a reading that finds more to apply confirms the run", since: atTheGate(nil),
			docs: []doc{
				{phase: "Planning", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "Pending", 90)},
			},
			wantReads: 1,
		},
		{
			name: "a reading blocked stops the wait", since: atTheGate(nil),
			docs: []doc{{
				phase: "Blocked", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "Blocked", 90),
				conditions: []metav1.Condition{condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryDirty", "Revision 7 is recorded dirty", 3)},
			}},
			wantStop: operate.StopNotConverged, wantMessage: "Revision 7 is recorded dirty",
		},

		// The regression #804 reports: approval A admitted, A goes stale, B is
		// approved on its own and verified. Nothing of it may satisfy the wait.
		{
			name: "plan B published in place of A, with no generation change, as a moved tag does", since: atTheGate(nil),
			docs: []doc{
				*atTheGate(nil),
				{phase: "AwaitingApproval", observedAt: at(30), plan: planDocOf("plan-b"), conditions: approvalConditions(metav1.ConditionTrue)},
			},
			wantStop: operate.StopPlanSuperseded, wantMessage: "plan plan-b replaced plan plan-a before it ran, so approval approve-a authorizes nothing",
		},
		{
			name: "plan B published in place of A, after a generation change", since: atTheGate(nil),
			docs: []doc{
				*atTheGate(nil),
				{generation: 4, observedGeneration: 4, phase: "AwaitingApproval", observedAt: at(30), plan: planDocOf("plan-b"), conditions: approvalConditions(metav1.ConditionTrue)},
			},
			wantStop: operate.StopPlanSuperseded, wantMessage: "plan plan-b replaced plan plan-a",
		},
		{
			name: "plan B running in place of A", since: atTheGate(nil),
			docs:     []doc{{phase: "Applying", observedAt: at(30), operation: "Apply", operationPlan: planDocOf("plan-b"), plan: planDocOf("plan-b")}},
			wantStop: operate.StopPlanSuperseded, wantMessage: "plan plan-b is running in place of plan plan-a",
		},
		{
			// The whole of B between two reads of this wait, with nothing
			// generation-bound to give it away: what used to return zero.
			name: "plan B applied and verified before the wait read again", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 40), "InSync", 90),
				conditions: inSyncConditions(operate.FamilyMigration, 3),
				journal:    []operatorv1alpha1.JournalEntry{ended("Succeeded", "op-b", "plan-b", "approve-b", 100), readBack("InSync", "op-b", "plan-b", 90)},
			}},
			wantStop: operate.StopOtherPlanApplied, wantMessage: "plan plan-b ran, not plan-a",
		},
		{
			name: "plan B ended some other way", since: atTheGate(nil),
			docs:     []doc{{phase: "Failed", observedAt: at(0), lastRun: run(failed, "op-b", "plan-b", "approve-b", 40)}},
			wantStop: operate.StopOtherPlanApplied, wantMessage: "the run (Job job-op-b) ended Failed",
		},
		{
			name: "a reading that leaves nothing to apply", since: atTheGate(nil),
			docs:     []doc{{phase: "InSync", observedAt: at(30), conditions: inSyncConditions(operate.FamilyMigration, 3)}},
			wantStop: operate.StopPlanSuperseded, wantMessage: "the database has nothing left to apply and plan plan-a did not run",
		},
		{
			// A record with no end says nothing yet, whichever plan it names.
			name: "a run of another plan that has not finished", since: atTheGate(nil),
			docs: func() []doc {
				unfinished := run(applied, "op-b", "plan-b", "approve-b", 40)
				unfinished.finishedAt = time.Time{}
				return []doc{{phase: "Applying", observedAt: at(0), plan: planDocOf("plan-a"), lastRun: unfinished}}
			}(),
			wantTimeout: true, wantMessage: "the run of plan plan-a",
		},
		{
			// A read under way may publish a plan yet, and a phase says nothing
			// about what it will find.
			name: "a read in flight with no plan at InSync is not an answer", since: atTheGate(nil),
			docs:        []doc{{phase: "InSync", observedAt: at(30), operation: "History", conditions: inSyncConditions(operate.FamilyMigration, 3)}},
			wantTimeout: true, wantMessage: "the run of plan plan-a",
		},
		{
			// A refusal somebody else lifts may bring the same plan back, so it
			// is not an answer. The timeout names it.
			name: "a resource blocked with its plan cleared is not an answer", since: atTheGate(nil),
			docs: []doc{{
				phase: "Blocked", observedAt: at(30),
				conditions: []metav1.Condition{condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "RealmConflict", "This database realm is claimed by 2 resources", 3)},
			}},
			wantTimeout: true, wantMessage: "RealmConflict",
		},
		{
			name: "its own claim in flight is waited for", since: atTheGate(nil),
			docs:        []doc{{phase: "Applying", observedAt: at(0), operation: "Apply", operationPlan: planDocOf("plan-a"), plan: planDocOf("plan-a")}},
			wantTimeout: true, wantMessage: "the run of plan plan-a; now the Apply operation",
		},
		{
			name: "a read in flight over its own plan is waited for", since: atTheGate(nil),
			docs:        []doc{{phase: "Reading", observedAt: at(0), operation: "History", plan: planDocOf("plan-a")}},
			wantTimeout: true, wantMessage: "the run of plan plan-a; now the History operation",
		},

		// A later run replacing lastRun.
		{
			name: "plan A verified, then replaced in lastRun by plan B before the wait read again", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Succeeded", "op-a", "plan-a", "approve-a", 80), readBack("InSync", "op-a", "plan-a", 90),
					ended("Succeeded", "op-b", "plan-b", "approve-b", 260), readBack("InSync", "op-b", "plan-b", 300),
				},
			}},
			wantReads: 1,
		},
		{
			name: "plan A replaced in lastRun with no reading after it in the journal", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Succeeded", "op-a", "plan-a", "approve-a", 80),
					ended("Succeeded", "op-b", "plan-b", "approve-b", 260), readBack("InSync", "op-b", "plan-b", 300),
				},
			}},
			wantStop: operate.StopNotProved, wantMessage: "the journal holds no reading of the history after it",
		},
		{
			name: "plan A's reading blocked, then replaced in lastRun", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Succeeded", "op-a", "plan-a", "approve-a", 80), readBack("Blocked", "op-a", "plan-a", 90),
					ended("Succeeded", "op-b", "plan-b", "approve-b", 260),
				},
			}},
			wantStop: operate.StopNotConverged, wantMessage: "blocked",
		},
		{
			name: "plan A failed, then replaced in lastRun", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Failed", "op-a", "plan-a", "approve-a", 80),
					ended("Succeeded", "op-b", "plan-b", "approve-b", 260),
				},
			}},
			wantStop: operate.StopApplyFailed, wantMessage: "the run of plan plan-a ended Failed",
		},
		{
			name: "plan A partial, then replaced in lastRun", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{ended("Partial", "op-a", "plan-a", "approve-a", 80)},
			}},
			wantStop: operate.StopUnresolved, wantMessage: "the run of plan plan-a ended Partial",
		},
		{
			name: "plan A found nothing to do, then replaced in lastRun", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{ended("UpToDate", "op-a", "plan-a", "approve-a", 80)},
			}},
			wantStop: operate.StopNothingApplied, wantMessage: "the run of plan plan-a found nothing to apply",
		},

		// One artifact used again, and two executions of a byte-identical plan.
		// The plan store reuses the plan, so its identity says nothing about
		// which execution this is: the baseline and the approval do.
		{
			name: "an earlier execution of the same plan is not this one",
			since: atTheGate(verified(run(applied, "op-old", "plan-a", "approve-old", -600), "InSync", -500),
				ended("Succeeded", "op-old", "plan-a", "approve-old", -540), readBack("InSync", "op-old", "plan-a", -500)),
			docs: []doc{*atTheGate(verified(run(applied, "op-old", "plan-a", "approve-old", -600), "InSync", -500),
				ended("Succeeded", "op-old", "plan-a", "approve-old", -540), readBack("InSync", "op-old", "plan-a", -500))},
			wantTimeout: true, wantMessage: "the run of plan plan-a",
		},
		{
			name: "this execution of a plan that ran before",
			since: atTheGate(verified(run(applied, "op-old", "plan-a", "approve-old", -600), "InSync", -500),
				ended("Succeeded", "op-old", "plan-a", "approve-old", -540), readBack("InSync", "op-old", "plan-a", -500)),
			docs: []doc{{
				phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "InSync", 90),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Succeeded", "op-old", "plan-a", "approve-old", -540), readBack("InSync", "op-old", "plan-a", -500),
					ended("Succeeded", "op-a", "plan-a", "approve-a", 80), readBack("InSync", "op-a", "plan-a", 90),
				},
			}},
			wantReads: 1,
		},
		{
			// A journaled end of the same execution has its reading under its
			// own operation ID, not the earlier execution's.
			name: "an earlier execution's reading is not this execution's",
			since: atTheGate(nil,
				ended("Succeeded", "op-old", "plan-a", "approve-old", -540), readBack("InSync", "op-old", "plan-a", -500)),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Succeeded", "op-old", "plan-a", "approve-old", -540), readBack("InSync", "op-old", "plan-a", -500),
					ended("Succeeded", "op-a", "plan-a", "approve-a", 80),
					ended("Succeeded", "op-b", "plan-b", "approve-b", 260),
				},
			}},
			wantStop: operate.StopNotProved, wantMessage: "the journal holds no reading",
		},
		{
			// The baseline's newest entry and this run's end differ only in the
			// operation. Reading them as one event would lose this run.
			name:  "two ends in one second, under the same plan and approval",
			since: atTheGate(nil, ended("Succeeded", "op-old", "plan-a", "approve-a", 80)),
			docs: []doc{{
				phase: "InSync", observedAt: at(300), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 200), "InSync", 300),
				journal: []operatorv1alpha1.JournalEntry{
					ended("Succeeded", "op-old", "plan-a", "approve-a", 80),
					ended("Succeeded", "op-a", "plan-a", "approve-a", 80), readBack("InSync", "op-a", "plan-a", 90),
					ended("Succeeded", "op-b", "plan-b", "approve-b", 260),
				},
			}},
			wantReads: 1,
		},
		{
			// Two people approved one plan: the operator runs the oldest, and
			// the later approval's wait is not told its decision ran.
			name: "the same plan under another approval", since: atTheGate(nil),
			docs:     []doc{{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-other", 20), "InSync", 90)}},
			wantStop: operate.StopOtherApproval, wantMessage: "plan plan-a ran under approval approve-other, not under approval approve-a",
		},
		{
			name: "the same plan under no approval", since: atTheGate(nil),
			docs:     []doc{{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "", 20), "InSync", 90)}},
			wantStop: operate.StopOtherApproval, wantMessage: "plan plan-a ran under no approval",
		},
		{
			name: "a plan with this UID and another fingerprint is another plan", since: atTheGate(nil),
			docs: func() []doc {
				other := verified(run(applied, "op-a", "plan-a", "approve-a", 20), "InSync", 90)
				other.fingerprint = "sha256:recomputed"
				return []doc{{phase: "InSync", observedAt: at(90), lastRun: other}}
			}(),
			wantStop: operate.StopOtherPlanApplied, wantMessage: "plan plan-a ran, not plan-a",
		},

		// Unknown identity stays unknown.
		{
			name: "a run that names no plan proves nothing", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(90), conditions: inSyncConditions(operate.FamilyMigration, 3),
				lastRun: &runDoc{outcome: applied, job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)},
			}},
			wantStop: operate.StopNotProved, wantMessage: "its record names no plan",
		},
		{
			name: "a journal entry that names no plan proves nothing", since: atTheGate(nil),
			docs: []doc{{
				phase: "InSync", observedAt: at(90), conditions: inSyncConditions(operate.FamilyMigration, 3),
				journal: []operatorv1alpha1.JournalEntry{{Type: operatorv1alpha1.JournalRunFinished, At: metav1.NewTime(at(80)), Outcome: operatorv1alpha1.JournalSucceeded}},
			}},
			wantStop: operate.StopNotProved, wantMessage: "its record names no plan",
		},

		// The other endings of this decision's run, named by the record and
		// with no Job to read: each of them holds whether or not one exists.
		{
			name: "this run ended Failed", since: atTheGate(nil),
			docs:     []doc{{phase: "VerifyingHistory", observedAt: at(0), lastRun: run(failed, "op-a", "plan-a", "approve-a", 20)}},
			wantStop: operate.StopApplyFailed, wantMessage: "ended Failed",
		},
		{
			name: "this run ended Partial", since: atTheGate(nil),
			docs:     []doc{{phase: "Blocked", observedAt: at(0), lastRun: run(partial, "op-a", "plan-a", "approve-a", 20)}},
			wantStop: operate.StopUnresolved, wantMessage: "ended Partial",
		},
		{
			name: "this run ended Unknown, settled without its Job", since: atTheGate(nil),
			docs: func() []doc {
				record := run(unknown, "op-a", "plan-a", "approve-a", 20)
				record.job, record.jobUID = "", ""
				return []doc{{phase: "Blocked", observedAt: at(0), lastRun: record}}
			}(),
			wantStop: operate.StopUnresolved, wantMessage: "ended Unknown",
		},
		{
			name: "this run found nothing to apply", since: atTheGate(nil),
			docs:     []doc{{phase: "VerifyingHistory", observedAt: at(0), lastRun: run(upToDat, "op-a", "plan-a", "approve-a", 20)}},
			wantStop: operate.StopNothingApplied,
		},

		// A hold with no run beside it, as a restore brings back.
		{
			name: "a hold on this plan", since: atTheGate(nil),
			docs:     []doc{{phase: "Blocked", observedAt: at(0), unresolved: &unresolvedDoc{operationID: "op-a", outcome: partial, job: "job-a", plan: "plan-a", planUID: "plan-a-uid"}}},
			wantStop: operate.StopUnresolved, wantMessage: "the run of plan plan-a (Job job-a) ended Partial",
		},
		{
			name: "a hold on another plan", since: atTheGate(nil),
			docs:     []doc{{phase: "Blocked", observedAt: at(0), unresolved: &unresolvedDoc{operationID: "op-b", outcome: partial, job: "job-b", plan: "plan-b", planUID: "plan-b-uid"}}},
			wantStop: operate.StopPlanSuperseded, wantMessage: "plan plan-b ran and ended Partial, and plan plan-a did not run",
		},

		{
			name: "an Apply claim given up names its failure", since: atTheGate(nil),
			docs: []doc{{
				phase: "Failed", observedAt: at(0), plan: planDocOf("plan-a"),
				conditions: failedConditions(operate.FamilyMigration, "OperationFailed", "build Apply Job: the runner image is not configured"),
				journal:    []operatorv1alpha1.JournalEntry{failure(20, "Apply", "OperationFailed")},
			}},
			wantStop: operate.StopApplyFailed, wantMessage: "the runner image is not configured",
		},

		// A wait bound by name or fingerprint, as --plan gives it.
		{
			name: "--plan by name", plan: "plan-a", since: atTheGate(nil),
			docs:      []doc{{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "InSync", 90)}},
			wantReads: 1,
		},
		{
			name: "--plan by fingerprint", plan: "sha256:a", since: atTheGate(nil),
			docs:      []doc{{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-a", 20), "InSync", 90)}},
			wantReads: 1,
		},
		{
			name: "--plan with another plan run", plan: "plan-a", since: atTheGate(nil),
			docs:     []doc{{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-b", "plan-b", "approve-b", 20), "InSync", 90)}},
			wantStop: operate.StopOtherPlanApplied, wantMessage: "plan plan-b ran, not plan-a",
		},
		{
			name: "--plan under any approval", plan: "plan-a", since: atTheGate(nil),
			docs:      []doc{{phase: "InSync", observedAt: at(90), lastRun: verified(run(applied, "op-a", "plan-a", "approve-other", 20), "InSync", 90)}},
			wantReads: 1,
		},
		{
			name: "--plan with a run that names no plan", plan: "plan-a", since: atTheGate(nil),
			docs:     []doc{{phase: "InSync", observedAt: at(90), lastRun: &runDoc{outcome: applied, job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}}},
			wantStop: operate.StopNotProved,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilyMigration, docs: row.docs}
			decision := row.decision
			if decision == nil && row.plan == "" {
				named := decisionA
				decision = &named
			}
			options := operate.WaitOptions{
				Target: operate.TargetApplied, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				Plan: row.plan, Decision: decision,
			}
			if row.since != nil {
				options.Since = row.since.object(operate.FamilyMigration)
			}
			result, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), options)
			switch {
			case row.wantReads > 0:
				if err != nil {
					t.Fatalf("wait: %v", err)
				}
				if result.Reads != row.wantReads {
					t.Fatalf("met the target at read %d, want read %d", result.Reads, row.wantReads)
				}
			case row.wantStop != "":
				var stop *operate.StopError
				if !errors.As(err, &stop) {
					t.Fatalf("err = %v, result = %+v, want a StopError", err, result)
				}
				if stop.Reason != row.wantStop || !strings.Contains(stop.Error(), row.wantMessage) {
					t.Fatalf("stop = %v, want reason %s naming %q", stop, row.wantStop, row.wantMessage)
				}
			case row.wantTimeout:
				var timedOut *operate.TimeoutError
				if !errors.As(err, &timedOut) {
					t.Fatalf("err = %v, result = %+v, want a TimeoutError", err, result)
				}
				if !strings.Contains(timedOut.Error(), row.wantMessage) {
					t.Fatalf("timeout = %q, want it to name %q", timedOut, row.wantMessage)
				}
			default:
				t.Fatal("row expects nothing")
			}
		})
	}
}

// A decision with no approval UID, which an API server that stamped none would
// leave, still binds the plan.
func TestWaitForADecisionWithNoApprovalUIDBindsThePlan(t *testing.T) {
	t.Parallel()
	tl := &timeline{family: operate.FamilyMigration, docs: []doc{{
		phase: "InSync", observedAt: at(90), lastRun: verified(run(operatorv1alpha1.MigrationRunOutcomeApplied, "op-a", "plan-a", "approve-a", 20), "InSync", 90),
	}}}
	result, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), operate.WaitOptions{
		Target: operate.TargetApplied, Timeout: timeout, Interval: interval, Clock: newStepClock(),
		Since:    atTheGate(nil).object(operate.FamilyMigration),
		Decision: &operate.Decision{PlanName: "plan-a", PlanUID: "plan-a-uid", PlanFingerprint: "sha256:a"},
	})
	if err != nil || result.Reads != 1 {
		t.Fatalf("wait = %+v, %v", result, err)
	}
}

func TestWaitRefusesADecisionItCannotHold(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		family  operate.Family
		options operate.WaitOptions
		want    string
	}{
		{
			name: "a decision on a schema", family: operate.FamilySchema,
			options: operate.WaitOptions{Target: operate.TargetApplied, Decision: &decisionA}, want: "only a wait for a PtahMigration",
		},
		{
			name: "a decision with another target", family: operate.FamilyMigration,
			options: operate.WaitOptions{Target: operate.TargetInSync, Decision: &decisionA}, want: "only a wait for a PtahMigration",
		},
		{
			name: "a decision and a plan", family: operate.FamilyMigration,
			options: operate.WaitOptions{Target: operate.TargetApplied, Decision: &decisionA, Plan: "plan-a"}, want: "already names its plan",
		},
		{
			name: "a decision naming no plan UID", family: operate.FamilyMigration,
			options: operate.WaitOptions{Target: operate.TargetApplied, Decision: &operate.Decision{PlanName: "plan-a"}}, want: "by UID",
		},
		{
			name: "a plan with another target", family: operate.FamilyMigration,
			options: operate.WaitOptions{Target: operate.TargetInSync, Plan: "plan-a"}, want: "a plan narrows only",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.options.Timeout = time.Minute
			tl := &timeline{family: row.family, docs: []doc{{phase: "InSync", observedAt: at(0)}}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(row.family), row.options)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("err = %v, want one naming %q", err, row.want)
			}
		})
	}
}
