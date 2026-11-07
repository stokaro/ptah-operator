package lifecyclejournal_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

// OutstandingPlan is what tells a plan replaced before it ran from one an Apply
// carried out, across the writes that clear status.plan in between.
func TestOutstandingPlanIsTheNewestPlanNothingCarriedOut(t *testing.T) {
	t.Parallel()
	approval := func(plan string) operatorv1alpha1.JournalEntry {
		return operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApprovalAccepted, At: at(0), Plan: plan, Approval: "approve-" + plan}
	}
	for _, test := range []struct {
		name         string
		entries      []operatorv1alpha1.JournalEntry
		noJournal    bool
		wantPlan     string
		wantApproved bool
	}{
		{name: "no journal", noJournal: true},
		{name: "a journal that names no plan", entries: []operatorv1alpha1.JournalEntry{failure("Observe", "OperationFailed", 0)}},
		{name: "a plan", entries: []operatorv1alpha1.JournalEntry{published("plan-a", 0)}, wantPlan: "plan-a"},
		{name: "the newest of two plans", entries: []operatorv1alpha1.JournalEntry{published("plan-a", -time.Minute), published("plan-b", 0)}, wantPlan: "plan-b"},
		{
			name:     "entries after the plan that carry nothing out",
			entries:  []operatorv1alpha1.JournalEntry{published("plan-a", -time.Minute), failure("Observe", "OperationFailed", 0)},
			wantPlan: "plan-a",
		},
		{
			name:     "an approved plan",
			entries:  []operatorv1alpha1.JournalEntry{published("plan-a", -time.Minute), approval("plan-a")},
			wantPlan: "plan-a", wantApproved: true,
		},
		{
			name:     "a plan after an approved one",
			entries:  []operatorv1alpha1.JournalEntry{published("plan-a", -time.Minute), approval("plan-a"), published("plan-b", 0)},
			wantPlan: "plan-b",
		},
		{
			name: "a plan an Apply carried out",
			entries: []operatorv1alpha1.JournalEntry{
				published("plan-a", -time.Minute), approval("plan-a"),
				{Type: operatorv1alpha1.JournalApplyFinished, At: at(0), Plan: "plan-a"},
			},
		},
		{
			name: "a plan a migration run carried out",
			entries: []operatorv1alpha1.JournalEntry{
				published("plan-a", -time.Minute),
				{Type: operatorv1alpha1.JournalRunFinished, At: at(0)},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var journal *operatorv1alpha1.LifecycleJournal
			if !test.noJournal {
				journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: test.entries}
			}
			plan, approved := lifecyclejournal.OutstandingPlan(journal)
			if plan != test.wantPlan || approved != test.wantApproved {
				t.Fatalf("OutstandingPlan() = %q, %t, want %q, %t", plan, approved, test.wantPlan, test.wantApproved)
			}
		})
	}
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
			// An Apply whose outcome nobody could prove spends the approval;
			// the operator reads the database again and the same plan comes
			// back for a fresh one. It was created before the Apply, so the
			// entry is dated when it came back.
			name:    "the plan back after an Apply of unknown outcome",
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: plan},
			journal: journalWith(published("plan-a", -10*time.Minute), operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApplyFinished, At: at(-5 * time.Minute), Outcome: operatorv1alpha1.JournalUnknown}),
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalPlanPublished, At: at(0), Plan: "plan-a", Statements: 3, Destructive: true,
			}},
		},
		{
			// The plan an Apply ran stays in status until its proof settles,
			// and the writes in between name it again.
			name:    "the applied plan, written again before its proof settles",
			before:  operatorv1alpha1.PtahSchemaStatus{Plan: plan, PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded)},
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: plan, PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded), Phase: operatorv1alpha1.PhaseVerifyingConvergence},
			journal: journalWith(published("plan-a", -time.Hour), operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApplyFinished, At: at(-50 * time.Minute)}),
		},
		{
			name:    "an approval the plan recorded",
			before:  operatorv1alpha1.PtahSchemaStatus{Plan: plan},
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: approved(plan, "approve-a")},
			journal: journalWith(published("plan-a", -10*time.Minute)),
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalApprovalAccepted, At: at(-5 * time.Minute), Plan: "plan-a",
				Approval: "approve-a", ApprovalUID: "approval-uid", Approver: "alice",
			}},
		},
		{
			name:    "an approval already recorded",
			before:  operatorv1alpha1.PtahSchemaStatus{},
			after:   operatorv1alpha1.PtahSchemaStatus{Plan: approved(plan, "approve-a")},
			journal: journalWith(published("plan-a", -10*time.Minute), operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalApprovalAccepted, Approval: "approve-a", ApprovalUID: "approval-uid"}),
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
			// Pending as well, so the row holds the reason ahead of the phase.
			name:   "a proof a person gave up",
			before: operatorv1alpha1.PtahSchemaStatus{PendingObservation: observing("apply-1", operatorv1alpha1.PendingObservationApplySucceeded)},
			after: operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhasePending, Conditions: []metav1.Condition{
				{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: string(operatorv1alpha1.ReasonProofSettled)},
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalVerified, At: at(0), Plan: "plan-a", Outcome: operatorv1alpha1.JournalUnknown,
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
			name:   "the plan a run carried out, written again",
			before: operatorv1alpha1.PtahMigrationStatus{Plan: planRef},
			after:  operatorv1alpha1.PtahMigrationStatus{Plan: planRef, Phase: operatorv1alpha1.MigrationPhaseVerifyingHistory},
			journal: &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{
				published("billing-plan", -time.Hour), {Type: operatorv1alpha1.JournalRunFinished, At: at(-50 * time.Minute)},
			}},
		},
		{
			name:    "an Apply that consumed an approval",
			before:  operatorv1alpha1.PtahMigrationStatus{Plan: planRef},
			after:   operatorv1alpha1.PtahMigrationStatus{Plan: planRef, ActiveOperation: apply},
			journal: &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{published("billing-plan", -time.Hour)}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalApprovalAccepted, At: at(0), Plan: "billing-plan",
				Approval: "billing-approval", ApprovalUID: "approval-uid",
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
				Plan: "billing-plan", Outcome: operatorv1alpha1.JournalFailed, AppliedVersions: 2, Failure: run.Failure,
			}},
		},
		{
			name:   "a run that started the second its predecessor did, under another Job",
			before: operatorv1alpha1.PtahMigrationStatus{LastRun: run, ActiveOperation: apply},
			after: operatorv1alpha1.PtahMigrationStatus{LastRun: &operatorv1alpha1.MigrationRunStatus{
				Outcome: run.Outcome, JobUID: "job-3", StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
				AppliedVersions: run.AppliedVersions, Failure: run.Failure,
			}},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), StartedAt: ptr(at(-3 * time.Minute)),
				Plan: "billing-plan", Outcome: operatorv1alpha1.JournalFailed, AppliedVersions: 2, Failure: run.Failure,
			}},
		},
		{
			name: "a run the status held unfinished, finished",
			before: operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply, LastRun: &operatorv1alpha1.MigrationRunStatus{
				Outcome: run.Outcome, JobUID: run.JobUID, StartedAt: run.StartedAt,
			}},
			after: operatorv1alpha1.PtahMigrationStatus{LastRun: run},
			want: []operatorv1alpha1.JournalEntry{{
				Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), StartedAt: ptr(at(-3 * time.Minute)),
				Plan: "billing-plan", Outcome: operatorv1alpha1.JournalFailed, AppliedVersions: 2, Failure: run.Failure,
			}},
		},
		{
			name:   "the same run written again",
			before: operatorv1alpha1.PtahMigrationStatus{LastRun: run},
			after:  operatorv1alpha1.PtahMigrationStatus{LastRun: run, NextReconciliationTime: ptr(at(time.Minute))},
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

// accepted is the entry an earlier decision left: under the name the rows reuse,
// on an earlier plan, with its own approver. uid is empty for one a release before
// the field wrote.
func accepted(uid types.UID) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalApprovalAccepted, At: at(-time.Hour), Plan: "plan-a",
		Approval: "release-approval", ApprovalUID: uid, Approver: "alice",
	}
}

// A decision is its UID. Each row asks one thing of the journal about one more
// write, from a resource that already carried a decision under the same name, whose
// approval object is gone by then: nothing here can read it.
func TestSchemaEntriesTellDecisionsApartByUID(t *testing.T) {
	t.Parallel()
	plan := func(name string, uid types.UID, approver string) *operatorv1alpha1.CurrentPlanStatus {
		current := schemaPlan(name)
		current.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
			Name: "release-approval", UID: uid, ApprovedAt: at(-5 * time.Minute),
			Approver: operatorv1alpha1.ApprovalIdentity{Username: approver},
		}
		return current
	}
	entryFor := func(name string, uid types.UID, approver string) []operatorv1alpha1.JournalEntry {
		return []operatorv1alpha1.JournalEntry{{
			Type: operatorv1alpha1.JournalApprovalAccepted, At: at(-5 * time.Minute), Plan: name,
			Approval: "release-approval", ApprovalUID: uid, Approver: approver,
		}}
	}
	// Published for the rows that name another plan, so only the decision is new.
	published := func(entries ...operatorv1alpha1.JournalEntry) *operatorv1alpha1.LifecycleJournal {
		return &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-2 * time.Hour), Entries: append(entries,
			operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalPlanPublished, At: at(-10 * time.Minute), Plan: "plan-b"},
			operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalPlanPublished, At: at(-9 * time.Minute), Plan: "plan-a"})}
	}
	bare := func(name string) *operatorv1alpha1.CurrentPlanStatus { return schemaPlan(name) }
	for _, test := range []struct {
		name          string
		before, after *operatorv1alpha1.CurrentPlanStatus
		journal       *operatorv1alpha1.LifecycleJournal
		want          []operatorv1alpha1.JournalEntry
	}{
		{
			name:    "the name reused, for another plan, by another person",
			before:  bare("plan-b"),
			after:   plan("plan-b", "uid-b", "bob"),
			journal: &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{accepted("uid-a"), {Type: operatorv1alpha1.JournalPlanPublished, Plan: "plan-b"}}},
			want:    entryFor("plan-b", "uid-b", "bob"),
		},
		{
			// An Apply of unknown outcome spends its approval, and the same plan
			// comes back for a fresh one, under whatever name that one takes.
			name:    "the name reused, for the same plan",
			before:  bare("plan-a"),
			after:   plan("plan-a", "uid-b", "bob"),
			journal: published(accepted("uid-a")),
			want:    entryFor("plan-a", "uid-b", "bob"),
		},
		{
			name:    "the same decision, written again",
			before:  plan("plan-a", "uid-a", "alice"),
			after:   plan("plan-a", "uid-a", "alice"),
			journal: published(accepted("uid-a")),
		},
		{
			// The controller clears and republishes a plan while it refreshes.
			name:    "the same decision, back after a refresh",
			before:  nil,
			after:   plan("plan-a", "uid-a", "alice"),
			journal: published(accepted("uid-a")),
		},
		{
			// Status already named this decision, so this write is no news about
			// it, whatever the entry that recorded it says or whether a bound has
			// dropped it.
			name:    "the same decision, its entry dropped by the bound",
			before:  plan("plan-a", "uid-a", "alice"),
			after:   plan("plan-a", "uid-a", "alice"),
			journal: published(),
		},
		{
			name:    "a decision current across the upgrade, its entry written before the field",
			before:  plan("plan-a", "uid-a", "alice"),
			after:   plan("plan-a", "uid-a", "alice"),
			journal: published(accepted("")),
		},
		{
			// The entry says what the approval was called and not which object it
			// was, so it cannot be read as having recorded this one.
			name:    "a later decision under the name of an entry written before the field",
			before:  bare("plan-a"),
			after:   plan("plan-a", "uid-b", "bob"),
			journal: published(accepted("")),
			want:    entryFor("plan-a", "uid-b", "bob"),
		},
		{
			name:    "another decision taking its place in status",
			before:  plan("plan-a", "uid-a", "alice"),
			after:   plan("plan-a", "uid-b", "bob"),
			journal: published(accepted("uid-a")),
			want:    entryFor("plan-a", "uid-b", "bob"),
		},
		{
			name:   "a decision status already holds, on the first write of a manager that keeps a journal",
			before: plan("plan-a", "uid-a", "alice"),
			after:  plan("plan-a", "uid-a", "alice"),
			want:   append([]operatorv1alpha1.JournalEntry{}, entryFor("plan-a", "uid-a", "alice")...),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := lifecyclejournal.SchemaEntries(
				&operatorv1alpha1.PtahSchemaStatus{Plan: test.before}, &operatorv1alpha1.PtahSchemaStatus{Plan: test.after}, test.journal, now)
			var decisions []operatorv1alpha1.JournalEntry
			for _, entry := range got {
				if entry.Type == operatorv1alpha1.JournalApprovalAccepted {
					decisions = append(decisions, entry)
				}
			}
			if !reflect.DeepEqual(decisions, test.want) {
				t.Fatalf("decisions = %#v\nwant %#v", decisions, test.want)
			}
		})
	}
}

func TestMigrationEntriesTellDecisionsApartByUID(t *testing.T) {
	t.Parallel()
	apply := func(id, plan string, uid types.UID) *operatorv1alpha1.MigrationOperationStatus {
		return &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationApply, ID: id,
			PlanRef:     &operatorv1alpha1.ImmutableObjectReference{Name: plan, UID: types.UID(plan + "-uid")},
			ApprovalRef: &operatorv1alpha1.ImmutableObjectReference{Name: "release-approval", UID: uid},
		}
	}
	entryFor := func(plan string, uid types.UID) []operatorv1alpha1.JournalEntry {
		return []operatorv1alpha1.JournalEntry{{
			Type: operatorv1alpha1.JournalApprovalAccepted, At: at(0), Plan: plan, Approval: "release-approval", ApprovalUID: uid,
		}}
	}
	journalOf := func(entries ...operatorv1alpha1.JournalEntry) *operatorv1alpha1.LifecycleJournal {
		return &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-2 * time.Hour), Entries: entries}
	}
	for _, test := range []struct {
		name          string
		before, after *operatorv1alpha1.MigrationOperationStatus
		journal       *operatorv1alpha1.LifecycleJournal
		want          []operatorv1alpha1.JournalEntry
	}{
		{
			name:    "the name reused, for another plan",
			after:   apply("apply-2", "plan-b", "uid-b"),
			journal: journalOf(accepted("uid-a")),
			want:    entryFor("plan-b", "uid-b"),
		},
		{
			name:    "the name reused, for the same plan",
			after:   apply("apply-2", "plan-a", "uid-b"),
			journal: journalOf(accepted("uid-a")),
			want:    entryFor("plan-a", "uid-b"),
		},
		{
			name:    "the same decision, its claim written again",
			before:  apply("apply-1", "plan-a", "uid-a"),
			after:   apply("apply-1", "plan-a", "uid-a"),
			journal: journalOf(accepted("uid-a")),
		},
		{
			// A claim withdrawn before dispatch leaves the approval to be claimed
			// again, with no claim in the status between the two.
			name:    "the same decision under a new claim",
			after:   apply("apply-2", "plan-a", "uid-a"),
			journal: journalOf(accepted("uid-a")),
		},
		{
			name:    "the same decision, its entry dropped by the bound",
			before:  apply("apply-1", "plan-a", "uid-a"),
			after:   apply("apply-1", "plan-a", "uid-a"),
			journal: journalOf(),
		},
		{
			name:    "a claim current across the upgrade, its entry written before the field",
			before:  apply("apply-1", "plan-a", "uid-a"),
			after:   apply("apply-1", "plan-a", "uid-a"),
			journal: journalOf(accepted("")),
		},
		{
			name:    "a later decision under the name of an entry written before the field",
			after:   apply("apply-2", "plan-b", "uid-b"),
			journal: journalOf(accepted("")),
			want:    entryFor("plan-b", "uid-b"),
		},
		{
			name:    "another decision under the claim that replaced this one",
			before:  apply("apply-1", "plan-a", "uid-a"),
			after:   apply("apply-2", "plan-a", "uid-b"),
			journal: journalOf(accepted("uid-a")),
			want:    entryFor("plan-a", "uid-b"),
		},
		{
			// Only an acceptance records a decision: an entry of another kind may
			// name the approval it concerns.
			name:  "a decision another kind of entry mentions",
			after: apply("apply-2", "plan-a", "uid-b"),
			journal: journalOf(operatorv1alpha1.JournalEntry{
				Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Hour), Approval: "release-approval", ApprovalUID: "uid-b",
			}),
			want: entryFor("plan-a", "uid-b"),
		},
		{
			name:   "a claim status already holds, on the first write of a manager that keeps a journal",
			before: apply("apply-1", "plan-a", "uid-a"),
			after:  apply("apply-1", "plan-a", "uid-a"),
			want:   entryFor("plan-a", "uid-a"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := lifecyclejournal.MigrationEntries(
				&operatorv1alpha1.PtahMigrationStatus{ActiveOperation: test.before},
				&operatorv1alpha1.PtahMigrationStatus{ActiveOperation: test.after}, test.journal, now)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("entries = %#v\nwant %#v", got, test.want)
			}
		})
	}
}

// The whole of it through Record: two decisions under one name, each written
// repeatedly, leave one entry each.
func TestRecordKeepsTwoDecisionsUnderOneName(t *testing.T) {
	t.Parallel()
	apply := func(id, plan string, uid types.UID) operatorv1alpha1.PtahMigrationStatus {
		return operatorv1alpha1.PtahMigrationStatus{ActiveOperation: &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationApply, ID: id,
			PlanRef:     &operatorv1alpha1.ImmutableObjectReference{Name: plan, UID: types.UID(plan + "-uid")},
			ApprovalRef: &operatorv1alpha1.ImmutableObjectReference{Name: "release-approval", UID: uid},
		}}
	}
	writes := []operatorv1alpha1.PtahMigrationStatus{
		apply("apply-1", "plan-a", "uid-a"),
		apply("apply-1", "plan-a", "uid-a"),
		{},
		apply("apply-2", "plan-b", "uid-b"),
		apply("apply-2", "plan-b", "uid-b"),
		{},
		{},
	}
	var journal *operatorv1alpha1.LifecycleJournal
	before := operatorv1alpha1.PtahMigrationStatus{}
	for index := range writes {
		after := writes[index]
		journal = lifecyclejournal.Record(journal, lifecyclejournal.MigrationEntries(&before, &after, journal, now), now)
		before = after
	}
	var decisions []string
	for _, entry := range journal.Entries {
		if entry.Type == operatorv1alpha1.JournalApprovalAccepted {
			decisions = append(decisions, entry.Plan+"/"+string(entry.ApprovalUID))
		}
	}
	if want := []string{"plan-a/uid-a", "plan-b/uid-b"}; !slices.Equal(decisions, want) {
		t.Fatalf("decisions = %v, want %v", decisions, want)
	}
}

// A status write that records a run nobody accounted for writes two records of
// one execution: lastRun, the run's account, and unresolvedRun, the hold it puts
// on the resource. These rows hold the journal to one end for the two together,
// which neither record shows alone.
func TestMigrationEntriesRecordOneEndPerExecution(t *testing.T) {
	t.Parallel()
	planRef := operatorv1alpha1.ImmutableObjectReference{Name: "billing-plan", UID: "plan-uid"}
	stop := &operatorv1alpha1.MigrationRunFailureStatus{Version: 16, Statement: 2, Statements: 3, SQLState: "42P01"}
	apply := func(id string) *operatorv1alpha1.MigrationOperationStatus {
		return &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationApply, ID: id, PlanRef: &planRef}
	}
	partial := &operatorv1alpha1.MigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomePartial, JobName: "job-a", JobUID: "job-a-uid", StartedAt: at(-3 * time.Minute),
		FinishedAt: ptr(at(-time.Minute)), AppliedVersions: []int64{14, 15}, Failure: stop,
	}
	partialHold := &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomePartial, OperationID: "apply-1", JobName: "job-a", JobUID: "job-a-uid",
		PlanRef: planRef, RecordedAt: at(-time.Minute),
	}
	// An uncertain Apply whose create was never confirmed names no Job.
	unknown := &operatorv1alpha1.MigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, StartedAt: at(-3 * time.Minute), FinishedAt: ptr(at(-time.Minute)),
	}
	unknownHold := &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: "apply-1", PlanRef: planRef, RecordedAt: at(-time.Minute),
	}
	partialEnd := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), StartedAt: ptr(at(-3 * time.Minute)), Plan: "billing-plan",
		Outcome: operatorv1alpha1.JournalPartial, AppliedVersions: 2, Failure: stop,
	}
	unknownEnd := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), StartedAt: ptr(at(-3 * time.Minute)), Plan: "billing-plan",
		Outcome: operatorv1alpha1.JournalUnknown,
	}
	holdEnd := func(outcome operatorv1alpha1.JournalOutcome) []operatorv1alpha1.JournalEntry {
		return []operatorv1alpha1.JournalEntry{{
			Type: operatorv1alpha1.JournalRunFinished, At: at(-time.Minute), Plan: "billing-plan", Outcome: outcome,
		}}
	}
	kept := &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour)}
	for _, test := range []struct {
		name          string
		before, after operatorv1alpha1.PtahMigrationStatus
		journal       *operatorv1alpha1.LifecycleJournal
		want          []operatorv1alpha1.JournalEntry
	}{
		{
			name:    "a partial run and its hold, written together",
			before:  operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply("apply-1")},
			after:   operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: partialHold},
			journal: kept,
			want:    []operatorv1alpha1.JournalEntry{partialEnd},
		},
		{
			name:    "an unknown run and its hold, written together",
			before:  operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply("apply-1")},
			after:   operatorv1alpha1.PtahMigrationStatus{LastRun: unknown, UnresolvedRun: unknownHold},
			journal: kept,
			want:    []operatorv1alpha1.JournalEntry{unknownEnd},
		},
		{
			// The claim stays while its workload may still write, so the run
			// and its hold are written first and the claim retires later.
			name:    "an uncertain retirement that keeps its claim",
			before:  operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply("apply-1")},
			after:   operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply("apply-1"), LastRun: unknown, UnresolvedRun: unknownHold},
			journal: kept,
			want:    []operatorv1alpha1.JournalEntry{unknownEnd},
		},
		{
			name:    "the claim retired once its workload stopped",
			before:  operatorv1alpha1.PtahMigrationStatus{ActiveOperation: apply("apply-1"), LastRun: unknown, UnresolvedRun: unknownHold},
			after:   operatorv1alpha1.PtahMigrationStatus{LastRun: unknown, UnresolvedRun: unknownHold},
			journal: kept,
		},
		{
			name:    "the run and its hold, written again",
			before:  operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: partialHold},
			after:   operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: partialHold, NextReconciliationTime: ptr(at(time.Minute))},
			journal: kept,
		},
		{
			// A restore that drops status drops the journal with it. The hold
			// keeps the outcome the run reported, and a known partial run does
			// not become an unknown one.
			name:  "a partial hold restored without status",
			after: operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: partialHold},
			want:  holdEnd(operatorv1alpha1.JournalPartial),
		},
		{
			name:  "an unknown hold restored without status",
			after: operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: unknownHold},
			want:  holdEnd(operatorv1alpha1.JournalUnknown),
		},
		{
			name:    "a hold restored beside a journal that holds no end for it",
			after:   operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: partialHold},
			journal: kept,
			want:    holdEnd(operatorv1alpha1.JournalPartial),
		},
		{
			name:    "a hold restored beside the run that recorded its end",
			before:  operatorv1alpha1.PtahMigrationStatus{LastRun: partial},
			after:   operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: partialHold},
			journal: kept,
		},
		{
			name:    "a hold restored beside the run of another execution",
			before:  operatorv1alpha1.PtahMigrationStatus{LastRun: partial},
			after:   operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: unknownHold},
			journal: kept,
			want:    holdEnd(operatorv1alpha1.JournalUnknown),
		},
		{
			name:   "a hold restored beside a run of another Job, with the same outcome",
			before: operatorv1alpha1.PtahMigrationStatus{LastRun: partial},
			after: operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: &operatorv1alpha1.UnresolvedMigrationRunStatus{
				Outcome: operatorv1alpha1.MigrationRunOutcomePartial, OperationID: "apply-2", JobUID: "job-b-uid", PlanRef: planRef, RecordedAt: at(-time.Minute),
			}},
			journal: kept,
			want:    holdEnd(operatorv1alpha1.JournalPartial),
		},
		{
			name:   "a hold restored beside a run of its Job, with another outcome",
			before: operatorv1alpha1.PtahMigrationStatus{LastRun: partial},
			after: operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: &operatorv1alpha1.UnresolvedMigrationRunStatus{
				Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: "apply-1", JobUID: "job-a-uid", PlanRef: planRef, RecordedAt: at(-time.Minute),
			}},
			journal: kept,
			want:    holdEnd(operatorv1alpha1.JournalUnknown),
		},
		{
			// A manager that kept no journal left the hold standing: the first
			// write of one that does records it from the run beside it.
			name:   "a hold and its run from before the journal",
			before: operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: partialHold},
			after:  operatorv1alpha1.PtahMigrationStatus{LastRun: partial, UnresolvedRun: partialHold},
			want:   []operatorv1alpha1.JournalEntry{partialEnd},
		},
		{
			name:   "a hold from before the journal, with no run beside it",
			before: operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: partialHold},
			after:  operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: partialHold},
			want:   holdEnd(operatorv1alpha1.JournalPartial),
		},
		{
			name:    "a hold the journal has seen, with the run it came from gone",
			before:  operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: partialHold},
			after:   operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: partialHold},
			journal: kept,
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

// Two executions stay two runs, whatever they have in common. Each row walks a
// resource through both, settling the first in between, and the journal has to
// end with one RunFinished for each.
func TestMigrationEntriesKeepTwoExecutionsApart(t *testing.T) {
	t.Parallel()
	planRef := operatorv1alpha1.ImmutableObjectReference{Name: "billing-plan", UID: "plan-uid"}
	type job struct {
		name string
		uid  types.UID
	}
	execution := func(id string, offset time.Duration, ran job, outcome operatorv1alpha1.MigrationRunOutcome) (
		*operatorv1alpha1.MigrationOperationStatus, *operatorv1alpha1.MigrationRunStatus, *operatorv1alpha1.UnresolvedMigrationRunStatus,
	) {
		apply := &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationApply, ID: id, PlanRef: &planRef, StartedAt: at(offset)}
		run := &operatorv1alpha1.MigrationRunStatus{
			Outcome: outcome, JobName: ran.name, JobUID: ran.uid, StartedAt: at(offset), FinishedAt: ptr(at(offset + time.Minute)),
		}
		hold := &operatorv1alpha1.UnresolvedMigrationRunStatus{
			Outcome: outcome, OperationID: id, JobName: ran.name, JobUID: ran.uid, PlanRef: planRef, RecordedAt: at(offset + time.Minute),
		}
		return apply, run, hold
	}
	for _, test := range []struct {
		name          string
		first, second job
		outcome       operatorv1alpha1.MigrationRunOutcome
		want          operatorv1alpha1.JournalOutcome
	}{
		{
			name: "same plan and outcome, another Job", first: job{"job-a", "job-a-uid"}, second: job{"job-b", "job-b-uid"},
			outcome: operatorv1alpha1.MigrationRunOutcomePartial, want: operatorv1alpha1.JournalPartial,
		},
		{
			name: "same Job name, another UID", first: job{"job-a", "job-a-uid"}, second: job{"job-a", "job-a-other-uid"},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, want: operatorv1alpha1.JournalUnknown,
		},
		{
			name: "neither names a Job", first: job{}, second: job{},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, want: operatorv1alpha1.JournalUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			firstApply, firstRun, firstHold := execution("apply-1", -2*time.Hour, test.first, test.outcome)
			secondApply, secondRun, secondHold := execution("apply-2", -time.Hour, test.second, test.outcome)
			writes := []operatorv1alpha1.PtahMigrationStatus{
				{ActiveOperation: firstApply},
				{LastRun: firstRun, UnresolvedRun: firstHold},
				{LastRun: firstRun, ResolvedRun: &operatorv1alpha1.ResolvedMigrationRunStatus{
					OperationID: "apply-1", Resolution: operatorv1alpha1.MigrationRunResolvedByHistoryRead,
				}},
				{LastRun: firstRun, ActiveOperation: secondApply},
				{LastRun: secondRun, UnresolvedRun: secondHold},
				{LastRun: secondRun, UnresolvedRun: secondHold, NextReconciliationTime: ptr(at(time.Minute))},
			}
			var journal *operatorv1alpha1.LifecycleJournal
			before := operatorv1alpha1.PtahMigrationStatus{}
			for index := range writes {
				after := writes[index]
				journal = lifecyclejournal.Record(journal, lifecyclejournal.MigrationEntries(&before, &after, journal, now), now)
				before = after
			}
			var outcomes []operatorv1alpha1.JournalOutcome
			for _, entry := range journal.Entries {
				if entry.Type == operatorv1alpha1.JournalRunFinished {
					outcomes = append(outcomes, entry.Outcome)
				}
			}
			if want := []operatorv1alpha1.JournalOutcome{test.want, test.want}; !slices.Equal(outcomes, want) {
				t.Fatalf("runs = %v, want %v, from %#v", outcomes, want, journal.Entries)
			}
		})
	}
}
