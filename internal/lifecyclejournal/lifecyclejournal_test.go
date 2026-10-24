package lifecyclejournal_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

// now is the wall-clock instant a write stamps; the fixtures date what their
// status carries distinctly from it, so a test can tell which one an entry
// took.
var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func at(offset time.Duration) metav1.Time { return metav1.NewTime(now.Add(offset)) }

func failure(operation, reason string, offset time.Duration) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalOperationFailed, At: at(offset), Operation: operation, Reason: reason,
	}
}

func published(plan string, offset time.Duration) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalPlanPublished, At: at(offset), Plan: plan}
}

func TestRecordStartsCoveringAtTheFirstWrite(t *testing.T) {
	t.Parallel()
	journal := lifecyclejournal.Record(nil, nil, now)
	if journal == nil || !journal.CoveredSince.Time.Equal(now) || len(journal.Entries) != 0 {
		t.Fatalf("journal = %#v, want an empty one covering since %s", journal, now)
	}
}

func TestRecordFoldsAFailureRepeatedInARow(t *testing.T) {
	t.Parallel()
	start := &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour)}
	journal := lifecyclejournal.Record(start, []operatorv1alpha1.JournalEntry{
		failure("Observe", "ObservationFailed", -3*time.Minute),
		failure("Observe", "ObservationFailed", -2*time.Minute),
		failure("Observe", "ObservationFailed", -time.Minute),
	}, now)
	if len(journal.Entries) != 1 {
		t.Fatalf("entries = %#v, want the three attempts folded into one", journal.Entries)
	}
	folded := journal.Entries[0]
	if folded.Count != 3 || folded.LastAt == nil || !folded.LastAt.Time.Equal(now.Add(-time.Minute)) ||
		!folded.At.Time.Equal(now.Add(-3*time.Minute)) {
		t.Fatalf("folded = %#v, want count 3 from the first attempt to the last", folded)
	}
	if len(start.Entries) != 0 {
		t.Fatalf("Record changed the journal it was given: %#v", start.Entries)
	}
}

// A failure folds only into the newest entry, and only with its operation and
// reason: anything between two failures, or a different cause, is history of
// its own.
func TestRecordKeepsDistinctFailuresApart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		entries []operatorv1alpha1.JournalEntry
	}{
		{"another reason", []operatorv1alpha1.JournalEntry{
			failure("Observe", "ObservationFailed", -2*time.Minute), failure("Observe", "TargetUnreachable", -time.Minute),
		}},
		{"another operation", []operatorv1alpha1.JournalEntry{
			failure("Observe", "ObservationFailed", -2*time.Minute), failure("Plan", "ObservationFailed", -time.Minute),
		}},
		{"an event in between", []operatorv1alpha1.JournalEntry{
			failure("Observe", "ObservationFailed", -3*time.Minute), published("plan-a", -2*time.Minute),
			failure("Observe", "ObservationFailed", -time.Minute),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			journal := lifecyclejournal.Record(nil, test.entries, now)
			if len(journal.Entries) != len(test.entries) {
				t.Fatalf("entries = %#v, want each kept apart", journal.Entries)
			}
		})
	}
}

func TestRecordDropsTheOldestPastTheCountAndMovesCoverage(t *testing.T) {
	t.Parallel()
	var entries []operatorv1alpha1.JournalEntry
	for index := range lifecyclejournal.MaxEntries + 2 {
		entries = append(entries, published(fmt.Sprintf("plan-%02d", index), time.Duration(index)*time.Minute))
	}
	journal := lifecyclejournal.Record(&operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour)}, entries, now)
	if len(journal.Entries) != lifecyclejournal.MaxEntries || journal.Entries[0].Plan != "plan-02" {
		t.Fatalf("kept %d entries from %q, want %d from plan-02", len(journal.Entries), journal.Entries[0].Plan, lifecyclejournal.MaxEntries)
	}
	if !journal.CoveredSince.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("covered since %s, want the newest dropped entry %s", journal.CoveredSince.Time, now.Add(time.Minute))
	}
}

// The age bound counts from the newest entry, so it holds whichever clock
// dated the entries, and a folded failure is as old as its last attempt.
func TestRecordDropsWhatIsAMonthOlderThanTheNewest(t *testing.T) {
	t.Parallel()
	old := failure("Observe", "ObservationFailed", -40*24*time.Hour)
	old.LastAt, old.Count = ptr(at(-29*24*time.Hour)), 2
	journal := lifecyclejournal.Record(&operatorv1alpha1.LifecycleJournal{CoveredSince: at(-50 * 24 * time.Hour)}, []operatorv1alpha1.JournalEntry{
		published("plan-gone", -31*24*time.Hour),
		old,
		published("plan-new", 0),
	}, now)
	if names := planNames(journal.Entries); !slices.Equal(names, []string{"", "plan-new"}) {
		t.Fatalf("entries = %#v, want the folded failure and the new plan", journal.Entries)
	}
	if !journal.CoveredSince.Time.Equal(now.Add(-31 * 24 * time.Hour)) {
		t.Fatalf("covered since %s, want the dropped plan's time", journal.CoveredSince.Time)
	}
}

// Entries dated by the time their status carries may run on another clock
// than the write's: two months behind it, a day apart, both stay.
func TestRecordKeepsEntriesDatedByAnotherClock(t *testing.T) {
	t.Parallel()
	journal := lifecyclejournal.Record(&operatorv1alpha1.LifecycleJournal{CoveredSince: at(-90 * 24 * time.Hour)}, []operatorv1alpha1.JournalEntry{
		published("plan-a", -61*24*time.Hour), published("plan-b", -60*24*time.Hour),
	}, now)
	if names := planNames(journal.Entries); !slices.Equal(names, []string{"plan-a", "plan-b"}) {
		t.Fatalf("entries = %v, want both kept", names)
	}
}

func TestRecordNeverMovesCoverageBack(t *testing.T) {
	t.Parallel()
	journal := lifecyclejournal.Record(&operatorv1alpha1.LifecycleJournal{CoveredSince: at(0)}, []operatorv1alpha1.JournalEntry{
		published("plan-before", -40*24*time.Hour), published("plan-after", time.Hour),
	}, now)
	if !journal.CoveredSince.Time.Equal(now) {
		t.Fatalf("covered since %s, want it left at %s", journal.CoveredSince.Time, now)
	}
}

func planNames(entries []operatorv1alpha1.JournalEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Plan)
	}
	return names
}

func ptr[T any](value T) *T { return &value }

// schemaPlan is a published plan, dated distinctly from now.
func schemaPlan(name string) *operatorv1alpha1.CurrentPlanStatus {
	return &operatorv1alpha1.CurrentPlanStatus{Name: name, StatementCount: 3, Destructive: true, CreatedAt: at(-10 * time.Minute)}
}

func approved(plan *operatorv1alpha1.CurrentPlanStatus, name string) *operatorv1alpha1.CurrentPlanStatus {
	plan = plan.DeepCopy()
	plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
		Name: name, UID: "approval-uid", ApprovedAt: at(-5 * time.Minute),
		Approver: operatorv1alpha1.ApprovalIdentity{Username: "alice"},
	}
	return plan
}

func applying(id string) *operatorv1alpha1.ActiveOperationStatus {
	return &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationApply, ID: id, StartedAt: at(-2 * time.Minute)}
}

func observing(applyID string, outcome operatorv1alpha1.PendingObservationOutcome) *operatorv1alpha1.PendingObservationStatus {
	return &operatorv1alpha1.PendingObservationStatus{ApplyOperationID: applyID, Outcome: outcome, Plan: *schemaPlan("plan-a")}
}

func failed(reason string) []metav1.Condition {
	return []metav1.Condition{{Type: operatorv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue, Reason: reason}}
}

func TestSchemaEntriesRecordEachTransitionOnce(t *testing.T) {
	t.Parallel()
	plan := schemaPlan("plan-a")
	journalWith := func(entries ...operatorv1alpha1.JournalEntry) *operatorv1alpha1.LifecycleJournal {
		return &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: entries}
	}
	resolve := &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationObserve, ID: "observe-1", Attempt: 1}
	retried := resolve.DeepCopy()
	retried.Attempt = 2
	for _, test := range []struct {
		name          string
		before, after operatorv1alpha1.PtahSchemaStatus
		journal       *operatorv1alpha1.LifecycleJournal
		want          []operatorv1alpha1.JournalEntry
	}{
		{
			name:  "a new plan",
			after: operatorv1alpha1.PtahSchemaStatus{Plan: plan},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalPlanPublished, At: plan.CreatedAt, Plan: "plan-a", Statements: 3, Destructive: true,
			}},
		},
		{
			name:    "the same plan republished after a refresh",
			before:  operatorv1alpha1.PtahSchemaStatus{},
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: plan},
			journal: journalWith(published("plan-a", -10*time.Minute)),
		},
		{
			name:    "the same plan after it was applied",
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: plan},
			journal: journalWith(published("plan-a", -time.Hour), operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApplyFinished, At: at(-50 * time.Minute)}),
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalPlanPublished, At: plan.CreatedAt, Plan: "plan-a", Statements: 3, Destructive: true,
			}},
		},
		{
			name:    "an approval the plan recorded",
			before:  operatorv1alpha1.PtahSchemaStatus{Plan: plan},
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: approved(plan, "approve-a")},
			journal: journalWith(published("plan-a", -10*time.Minute)),
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalApprovalAccepted, At: at(-5 * time.Minute), Plan: "plan-a",
				Approval: "approve-a", Approver: "alice",
			}},
		},
		{
			name:    "an approval already recorded",
			before:  operatorv1alpha1.PtahSchemaStatus{},
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: approved(plan, "approve-a")},
			journal: journalWith(published("plan-a", -10*time.Minute), operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApprovalAccepted, Approval: "approve-a"}),
		},
		{
			name:   "an Apply that succeeded",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: applying("apply-1")},
			after:  operatorv1alpha1.PtahSchemaStatus{PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded)},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalApplyFinished, At: at(0), StartedAt: ptr(at(-2 * time.Minute)),
				Plan: "plan-a", Outcome: operatorv1alpha1.JournalSucceeded,
			}},
		},
		{
			name:   "an Apply whose outcome is unknown",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: applying("apply-1")},
			after:  operatorv1alpha1.PtahSchemaStatus{PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationOutcomeUnknown)},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalApplyFinished, At: at(0), StartedAt: ptr(at(-2 * time.Minute)),
				Plan: "plan-a", Outcome: operatorv1alpha1.JournalUnknown,
			}},
		},
		{
			name:   "an Apply withdrawn before it was dispatched",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: applying("apply-1")},
			after:  operatorv1alpha1.PtahSchemaStatus{},
		},
		{
			name:   "a proof that found the database in sync",
			before: operatorv1alpha1.PtahSchemaStatus{PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded)},
			after:  operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhaseInSync},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalVerified, At: at(0), Plan: "plan-a", Outcome: operatorv1alpha1.JournalInSync,
			}},
		},
		{
			name:   "a proof that still found changes",
			before: operatorv1alpha1.PtahSchemaStatus{PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded)},
			after: operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhaseAwaitingApproval, Conditions: []metav1.Condition{
				{Type: operatorv1alpha1.ConditionDriftDetected, Status: metav1.ConditionTrue, Reason: "ScopedChanges"},
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalVerified, At: at(0), Plan: "plan-a", Outcome: operatorv1alpha1.JournalDriftRemains,
			}},
		},
		{
			name:   "a proof a newer generation took over",
			before: operatorv1alpha1.PtahSchemaStatus{PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded)},
			after: operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhasePending, Conditions: []metav1.Condition{
				{Type: operatorv1alpha1.ConditionDriftDetected, Status: metav1.ConditionTrue, Reason: "ScopedChanges"},
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalVerified, At: at(0), Plan: "plan-a", Outcome: operatorv1alpha1.JournalSuperseded,
			}},
		},
		{
			name:   "a failure",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: resolve},
			after:  operatorv1alpha1.PtahSchemaStatus{ActiveOperation: retried, Conditions: failed("ObservationFailed")},
			want:   []operatorv1alpha1.JournalEntry{failure("Observe", "ObservationFailed", 0)},
		},
		{
			name:   "a retry that failed the same way",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: resolve, Conditions: failed("ObservationFailed")},
			after:  operatorv1alpha1.PtahSchemaStatus{ActiveOperation: retried, Conditions: failed("ObservationFailed")},
			want:   []operatorv1alpha1.JournalEntry{failure("Observe", "ObservationFailed", 0)},
		},
		{
			name:   "a failure that changed its reason",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: resolve, Conditions: failed("ObservationFailed")},
			after:  operatorv1alpha1.PtahSchemaStatus{ActiveOperation: resolve, Conditions: failed("TargetUnreachable")},
			want:   []operatorv1alpha1.JournalEntry{failure("Observe", "TargetUnreachable", 0)},
		},
		{
			name:   "a failure still standing, written again",
			before: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: resolve, Conditions: failed("ObservationFailed")},
			after:  operatorv1alpha1.PtahSchemaStatus{ActiveOperation: resolve, Conditions: failed("ObservationFailed"), NextReconciliationTime: ptr(at(time.Minute))},
		},
		{
			name:   "a read-only cycle that changed nothing it records",
			before: operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhaseInSync},
			after:  operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhaseInSync, LastSuccessfulReconciliation: ptr(at(0))},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := lifecyclejournal.SchemaEntries(&test.before, &test.after, test.journal, now)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("entries = %#v\nwant %#v", got, test.want)
			}
		})
	}
}

func TestMigrationEntriesRecordEachTransitionOnce(t *testing.T) {
	t.Parallel()
	planRef := &operatorv1alpha1.ImmutableObjectReference{Name: "billing-plan", UID: "plan-uid"}
	run := &operatorv1alpha1.MigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomeFailed, JobUID: "job-2", StartedAt: at(-3 * time.Minute),
		FinishedAt: ptr(at(-time.Minute)), AppliedVersions: []int64{14, 15},
		Failure: &operatorv1alpha1.MigrationRunFailureStatus{Version: 16, Statement: 2, Statements: 3, SQLState: "42P01"},
	}
	apply := &operatorv1alpha1.MigrationOperationStatus{
		Type: operatorv1alpha1.MigrationOperationApply, ID: "apply-1", PlanRef: planRef,
		ApprovalRef: &operatorv1alpha1.ImmutableObjectReference{Name: "billing-approval", UID: "approval-uid"},
	}
	history := &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationHistory, ID: "history-1", Attempt: 1}
	retried := history.DeepCopy()
	retried.Attempt = 2
	ready := func(reason string) []metav1.Condition {
		return []metav1.Condition{{Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionFalse, Reason: reason}}
	}
	for _, test := range []struct {
		name          string
		before, after operatorv1alpha1.PtahMigrationStatus
		journal       *operatorv1alpha1.LifecycleJournal
		want          []operatorv1alpha1.JournalEntry
	}{
		{
			name:  "a new plan",
			after: operatorv1alpha1.PtahMigrationStatus{Plan: planRef},
			want:  []operatorv1alpha1.JournalEntry{{Type: operatorv1alpha1.JournalPlanPublished, At: at(0), Plan: "billing-plan"}},
		},
		{
			name:    "the plan it already recorded",
			after:   operatorv1alpha1.PtahMigrationStatus{Plan: planRef},
			journal: &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{published("billing-plan", -time.Hour)}},
		},
		{
			name:    "an Apply that consumed an approval",
			before:  operatorv1alpha1.PtahMigrationStatus{Plan: planRef},
			after:   operatorv1alpha1.PtahMigrationStatus{Plan: planRef, ActiveOperation: apply},
			journal: &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{published("billing-plan", -time.Hour)}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalApprovalAccepted, At: at(0), Plan: "billing-plan", Approval: "billing-approval",
			}},
		},
		{
			name:   "a run that failed",
			before: operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply},
			after:  operatorv1alpha1.PtahMigrationStatus{LastRun: run},
			journal: &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{
				{Type: operatorv1alpha1.JournalApprovalAccepted, Approval: "billing-approval"},
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), StartedAt: ptr(at(-3 * time.Minute)),
				Outcome: operatorv1alpha1.JournalFailed, AppliedVersions: 2, Failure: run.Failure,
			}},
		},
		{
			name:   "the same run written again",
			before: operatorv1alpha1.PtahMigrationStatus{LastRun: run},
			after:  operatorv1alpha1.PtahMigrationStatus{LastRun: run, NextReconciliationTime: ptr(at(time.Minute))},
		},
		{
			name: "a run recorded as unresolved",
			after: operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: &operatorv1alpha1.UnresolvedMigrationRunStatus{
				OperationID: "apply-1", PlanRef: *planRef, RecordedAt: at(-time.Minute),
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), Outcome: operatorv1alpha1.JournalUnknown, Plan: "billing-plan",
			}},
		},
		{
			name: "a run a person acknowledged",
			after: operatorv1alpha1.PtahMigrationStatus{ResolvedRun: &operatorv1alpha1.ResolvedMigrationRunStatus{
				OperationID: "apply-1", Resolution: operatorv1alpha1.MigrationRunResolvedByAcknowledgment, ResolvedAt: at(-time.Minute),
				AcknowledgmentRef: &operatorv1alpha1.ImmutableObjectReference{Name: "billing-ack"},
				AcknowledgedBy:    &operatorv1alpha1.ApprovalIdentity{Username: "bob"},
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalRunAcknowledged, At: at(-time.Minute), Acknowledgment: "billing-ack", AcknowledgedBy: "bob",
			}},
		},
		{
			name: "a run the history read settled",
			after: operatorv1alpha1.PtahMigrationStatus{ResolvedRun: &operatorv1alpha1.ResolvedMigrationRunStatus{
				OperationID: "apply-1", Resolution: operatorv1alpha1.MigrationRunResolvedByHistoryRead, ResolvedAt: at(-time.Minute),
			}},
		},
		{
			name:   "a failure",
			before: operatorv1alpha1.PtahMigrationStatus{ActiveOperation: history},
			after:  operatorv1alpha1.PtahMigrationStatus{Phase: operatorv1alpha1.MigrationPhaseFailed, ActiveOperation: history, Conditions: ready("OperationFailed")},
			want:   []operatorv1alpha1.JournalEntry{failure("History", "OperationFailed", 0)},
		},
		{
			name:   "a retry that failed the same way",
			before: operatorv1alpha1.PtahMigrationStatus{Phase: operatorv1alpha1.MigrationPhaseFailed, ActiveOperation: history, Conditions: ready("OperationFailed")},
			after:  operatorv1alpha1.PtahMigrationStatus{Phase: operatorv1alpha1.MigrationPhaseFailed, ActiveOperation: retried, Conditions: ready("OperationFailed")},
			want:   []operatorv1alpha1.JournalEntry{failure("History", "OperationFailed", 0)},
		},
		{
			name:   "a failure still standing, written again",
			before: operatorv1alpha1.PtahMigrationStatus{Phase: operatorv1alpha1.MigrationPhaseFailed, ActiveOperation: history, Conditions: ready("OperationFailed")},
			after: operatorv1alpha1.PtahMigrationStatus{Phase: operatorv1alpha1.MigrationPhaseFailed, ActiveOperation: history, Conditions: ready("OperationFailed"),
				NextReconciliationTime: ptr(at(time.Minute))},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := lifecyclejournal.MigrationEntries(&test.before, &test.after, test.journal, now)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("entries = %#v\nwant %#v", got, test.want)
			}
		})
	}
}
