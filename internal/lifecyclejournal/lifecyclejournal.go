// Package lifecyclejournal derives a resource's lifecycle journal from the
// status writes the controllers already make.
//
// Each controller writes status in one place and holds the status before and
// after the write there. [SchemaEntries] and [MigrationEntries] turn that
// transition into the events it records, and [Record] adds them to the journal
// in the same write, so no transition site has to remember the journal and the
// journal costs no request of its own.
package lifecyclejournal

import (
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The journal's bounds. The API schema holds MaxEntries as well.
const (
	MaxEntries = 64
	MaxAge     = 30 * 24 * time.Hour
)

// Record adds entries to journal, applies the bounds, and returns the journal
// to store. A nil journal starts covering at now. A failed attempt with the
// same operation and reason as the newest entry folds into it.
//
// The age bound is measured from the newest entry rather than from now: an
// entry may be dated by the time its status carries, and the bound must not
// depend on which clock dated it. Record runs only in a status write that
// happens anyway, so it never causes a write itself.
func Record(journal *operatorv1alpha1.LifecycleJournal, entries []operatorv1alpha1.JournalEntry, now time.Time) *operatorv1alpha1.LifecycleJournal {
	if journal == nil {
		journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: metav1.NewTime(now)}
	} else {
		journal = journal.DeepCopy()
	}
	for _, entry := range entries {
		if newest := len(journal.Entries) - 1; newest >= 0 && folds(journal.Entries[newest], entry) {
			folded := &journal.Entries[newest]
			last := entry.At
			folded.LastAt = &last
			folded.Count = max(folded.Count, 1) + 1
			continue
		}
		journal.Entries = append(journal.Entries, entry)
	}
	var cutoff time.Time
	if count := len(journal.Entries); count > 0 {
		cutoff = latest(journal.Entries[count-1]).Add(-MaxAge)
	}
	drop := 0
	for drop < len(journal.Entries) && (len(journal.Entries)-drop > MaxEntries || latest(journal.Entries[drop]).Before(cutoff)) {
		drop++
	}
	if drop > 0 {
		dropped := latest(journal.Entries[drop-1])
		if dropped.After(journal.CoveredSince.Time) {
			journal.CoveredSince = metav1.NewTime(dropped)
		}
		journal.Entries = slices.Clone(journal.Entries[drop:])
	}
	return journal
}

// folds reports whether entry is another attempt of the failure newest
// records.
func folds(newest, entry operatorv1alpha1.JournalEntry) bool {
	return newest.Type == operatorv1alpha1.JournalOperationFailed && entry.Type == operatorv1alpha1.JournalOperationFailed &&
		newest.Operation == entry.Operation && newest.Reason == entry.Reason
}

// latest is the newest instant an entry stands for.
func latest(entry operatorv1alpha1.JournalEntry) time.Time {
	if entry.LastAt != nil {
		return entry.LastAt.Time
	}
	return entry.At.Time
}

// SchemaEntries is what a PtahSchema status write from before to after
// records. journal is the journal before the write, which tells a plan or an
// approval the resource already recorded from a new one: the controller clears
// and republishes the same plan while it refreshes.
//
// A plan is published by the write that first names it. The plan an Apply ran
// stays in status until the proof that follows settles, and every write in
// between names it again; none of them publishes it.
func SchemaEntries(before, after *operatorv1alpha1.PtahSchemaStatus, journal *operatorv1alpha1.LifecycleJournal, now time.Time) []operatorv1alpha1.JournalEntry {
	var entries []operatorv1alpha1.JournalEntry
	if plan := after.Plan; plan != nil && (before.Plan == nil || before.Plan.Name != plan.Name) && !planRecorded(journal, plan.Name) {
		entries = append(entries, operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalPlanPublished, At: publishedAt(plan.CreatedAt, journal, now),
			Plan: plan.Name, Statements: plan.StatementCount, Destructive: plan.Destructive,
		})
	}
	if plan := after.Plan; plan != nil && plan.Approval != nil &&
		!(journal != nil && schemaHeldApproval(before, plan.Approval.UID)) && !approvalRecorded(journal, plan.Approval.UID) {
		entries = append(entries, operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalApprovalAccepted, At: orNow(plan.Approval.ApprovedAt, now),
			Plan: plan.Name, Approval: plan.Approval.Name, ApprovalUID: plan.Approval.UID, Approver: plan.Approval.Approver.Username,
		})
	}
	// An Apply ends where its claim does, and it ran only if the observation
	// that proves it took its place: an Apply claimed and withdrawn before
	// dispatch changed nothing and is not an event.
	if operation := before.ActiveOperation; operation != nil && operation.Type == operatorv1alpha1.OperationApply &&
		(after.ActiveOperation == nil || after.ActiveOperation.ID != operation.ID) {
		if pending := after.PendingObservation; pending != nil && pending.ApplyOperationID == operation.ID {
			outcome := operatorv1alpha1.JournalSucceeded
			if pending.Outcome != operatorv1alpha1.PendingObservationApplySucceeded {
				outcome = operatorv1alpha1.JournalUnknown
			}
			started := operation.StartedAt
			entries = append(entries, operatorv1alpha1.JournalEntry{
				Type: operatorv1alpha1.JournalApplyFinished, At: metav1.NewTime(now), StartedAt: &started,
				Plan: pending.Plan.Name, Outcome: outcome,
			})
		}
	}
	if pending := before.PendingObservation; pending != nil && after.PendingObservation == nil {
		// A proof that settles into Pending or Verifying settled under a newer
		// generation, policy or execution binding, which decides what follows.
		outcome := operatorv1alpha1.JournalSuperseded
		ready := meta.FindStatusCondition(after.Conditions, operatorv1alpha1.ConditionReady)
		switch {
		case after.Phase == operatorv1alpha1.PhaseInSync:
			outcome = operatorv1alpha1.JournalInSync
		case ready != nil && ready.Reason == string(operatorv1alpha1.ReasonProofSettled):
			// A person gave the proof up, so nothing established what the Apply did.
			outcome = operatorv1alpha1.JournalUnknown
		case after.Phase == operatorv1alpha1.PhasePending || after.Phase == operatorv1alpha1.PhaseVerifying:
		case meta.IsStatusConditionTrue(after.Conditions, operatorv1alpha1.ConditionDriftDetected):
			outcome = operatorv1alpha1.JournalDriftRemains
		}
		entries = append(entries, operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(now), Plan: pending.Plan.Name, Outcome: outcome,
		})
	}
	if failure := meta.FindStatusCondition(after.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failure != nil &&
		failure.Status == metav1.ConditionTrue {
		previous := meta.FindStatusCondition(before.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
		retried := before.ActiveOperation != nil && after.ActiveOperation != nil &&
			before.ActiveOperation.ID == after.ActiveOperation.ID && after.ActiveOperation.Attempt > before.ActiveOperation.Attempt
		if previous == nil || previous.Status != metav1.ConditionTrue || previous.Reason != failure.Reason || retried {
			entries = append(entries, operatorv1alpha1.JournalEntry{
				Type: operatorv1alpha1.JournalOperationFailed, At: metav1.NewTime(now),
				Operation: schemaOperation(before, after), Reason: failure.Reason,
			})
		}
	}
	return entries
}

// MigrationEntries is what a PtahMigration status write from before to after
// records, given the journal before the write. A plan is published by the
// write that first names it, as for a schema.
func MigrationEntries(before, after *operatorv1alpha1.PtahMigrationStatus, journal *operatorv1alpha1.LifecycleJournal, now time.Time) []operatorv1alpha1.JournalEntry {
	var entries []operatorv1alpha1.JournalEntry
	if plan := after.Plan; plan != nil && (before.Plan == nil || before.Plan.Name != plan.Name) && !planRecorded(journal, plan.Name) {
		entries = append(entries, operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalPlanPublished, At: metav1.NewTime(now), Plan: plan.Name,
		})
	}
	// A migration's approval is recorded on the approval; what its status says
	// is which approval the Apply it dispatched consumed.
	if operation := after.ActiveOperation; operation != nil && operation.Type == operatorv1alpha1.MigrationOperationApply &&
		operation.ApprovalRef != nil && !(journal != nil && migrationHeldApproval(before, operation.ApprovalRef.UID)) &&
		!approvalRecorded(journal, operation.ApprovalRef.UID) {
		entry := operatorv1alpha1.JournalEntry{
			Type: operatorv1alpha1.JournalApprovalAccepted, At: metav1.NewTime(now),
			Approval: operation.ApprovalRef.Name, ApprovalUID: operation.ApprovalRef.UID,
		}
		if operation.PlanRef != nil {
			entry.Plan = operation.PlanRef.Name
		}
		entries = append(entries, entry)
	}
	if entry, finished := finishedRun(before, after, journal, now); finished {
		entries = append(entries, entry)
	}
	if resolved := after.ResolvedRun; resolved != nil && resolved.Resolution == operatorv1alpha1.MigrationRunResolvedByAcknowledgment &&
		(before.ResolvedRun == nil || before.ResolvedRun.OperationID != resolved.OperationID) {
		entry := operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalRunAcknowledged, At: orNow(resolved.ResolvedAt, now)}
		if resolved.AcknowledgmentRef != nil {
			entry.Acknowledgment = resolved.AcknowledgmentRef.Name
		}
		if resolved.AcknowledgedBy != nil {
			entry.AcknowledgedBy = resolved.AcknowledgedBy.Username
		}
		entries = append(entries, entry)
	}
	if after.Phase == operatorv1alpha1.MigrationPhaseFailed {
		reason := ""
		if ready := meta.FindStatusCondition(after.Conditions, operatorv1alpha1.ConditionMigrationReady); ready != nil {
			reason = ready.Reason
		}
		previous := ""
		if ready := meta.FindStatusCondition(before.Conditions, operatorv1alpha1.ConditionMigrationReady); ready != nil {
			previous = ready.Reason
		}
		retried := before.ActiveOperation != nil && after.ActiveOperation != nil &&
			before.ActiveOperation.ID == after.ActiveOperation.ID && after.ActiveOperation.Attempt > before.ActiveOperation.Attempt
		if before.Phase != operatorv1alpha1.MigrationPhaseFailed || previous != reason || retried {
			entry := operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalOperationFailed, At: metav1.NewTime(now), Reason: reason}
			if operation := firstMigrationOperation(before, after); operation != nil {
				entry.Operation = string(operation.Type)
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

// finishedRun is the RunFinished entry a status write owes, if any. One
// execution ends once, whichever records its end writes: status.lastRun is the
// run's account of itself, and status.unresolvedRun is the hold that account put
// on the resource, which is a safety state and not a second run.
//
// A hold reaches this function without a new account in two ways. A restore
// that dropped status brings it back from metadata, and a manager that kept no
// journal may have left one standing. Either way the journal holds no end for
// that execution, so the entry is made from what is left. A hold beside the
// account that recorded its end is already journaled, unless there is no journal
// yet.
func finishedRun(
	before, after *operatorv1alpha1.PtahMigrationStatus,
	journal *operatorv1alpha1.LifecycleJournal,
	now time.Time,
) (operatorv1alpha1.JournalEntry, bool) {
	run := after.LastRun
	if run != nil && run.FinishedAt != nil && !sameRun(before.LastRun, run) {
		entry := runFinished(run)
		// Both writers of a finished run retire an Apply claim, and an Apply
		// claim names its plan.
		if operation := before.ActiveOperation; operation != nil && operation.PlanRef != nil {
			entry.Plan = operation.PlanRef.Name
		}
		return entry, true
	}
	hold := after.UnresolvedRun
	if hold == nil {
		return operatorv1alpha1.JournalEntry{}, false
	}
	if held := before.UnresolvedRun != nil && before.UnresolvedRun.OperationID == hold.OperationID; held && journal != nil {
		return operatorv1alpha1.JournalEntry{}, false
	}
	if run != nil && run.FinishedAt != nil && accountsFor(run, hold) {
		if journal != nil {
			return operatorv1alpha1.JournalEntry{}, false
		}
		entry := runFinished(run)
		entry.Plan = hold.PlanRef.Name
		return entry, true
	}
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: orNow(hold.RecordedAt, now),
		Outcome: runOutcome(hold.Outcome), Plan: hold.PlanRef.Name,
	}, true
}

// runFinished is the entry of a finished run.
func runFinished(run *operatorv1alpha1.MigrationRunStatus) operatorv1alpha1.JournalEntry {
	started := run.StartedAt
	entry := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalRunFinished, At: *run.FinishedAt, StartedAt: &started,
		// The API holds AppliedVersions to 256 entries.
		Outcome: runOutcome(run.Outcome), AppliedVersions: int32(len(run.AppliedVersions)),
	}
	if run.Failure != nil {
		entry.Failure = run.Failure.DeepCopy()
	}
	return entry
}

// sameRun reports that previous is the finished run current records. The Job
// alone does not tell two runs apart: a run settled without its Job names none,
// and neither does the next one settled that way, so the claim's start decides
// between them.
func sameRun(previous, current *operatorv1alpha1.MigrationRunStatus) bool {
	return previous != nil && previous.FinishedAt != nil && previous.JobUID == current.JobUID &&
		previous.StartedAt.Equal(&current.StartedAt)
}

// accountsFor reports that run is the account hold was recorded from. Nothing
// runs while a hold stands, so the last run beside one is its own unless the
// Job or the outcome says otherwise.
func accountsFor(run *operatorv1alpha1.MigrationRunStatus, hold *operatorv1alpha1.UnresolvedMigrationRunStatus) bool {
	return run.JobUID == hold.JobUID && run.Outcome == hold.Outcome
}

// planRecorded reports whether the journal's newest PlanPublished names plan
// and nothing has carried it out since: the controller clears and republishes
// the plan it is waiting on while it refreshes.
func planRecorded(journal *operatorv1alpha1.LifecycleJournal, plan string) bool {
	outstanding, _ := OutstandingPlan(journal)
	return outstanding == plan
}

// OutstandingPlan names the plan the journal's newest PlanPublished records
// when nothing has carried it out since, and says whether an approval of that
// plan was accepted. The name is empty when the journal holds no such plan.
//
// A status write cannot answer this from status alone: the controller clears
// the plan it is waiting on while it refreshes, so a write that publishes a
// plan usually sees no plan before it.
func OutstandingPlan(journal *operatorv1alpha1.LifecycleJournal) (plan string, approved bool) {
	if journal == nil {
		return "", false
	}
	var approvals []string
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		switch entry := journal.Entries[index]; entry.Type {
		case operatorv1alpha1.JournalPlanPublished:
			return entry.Plan, slices.Contains(approvals, entry.Plan)
		case operatorv1alpha1.JournalApprovalAccepted:
			approvals = append(approvals, entry.Plan)
		case operatorv1alpha1.JournalApplyFinished, operatorv1alpha1.JournalRunFinished:
			return "", false
		}
	}
	return "", false
}

// approvalRecorded reports whether the journal already records the approval
// with this UID. A decision is its UID and never its name: a later approval may
// take the name of an earlier one, for another plan and from another person. An
// entry with no UID, from a release before the field, says nothing about which
// object it recorded, so it is never read as having recorded this one.
func approvalRecorded(journal *operatorv1alpha1.LifecycleJournal, approval types.UID) bool {
	if journal == nil {
		return false
	}
	return slices.ContainsFunc(journal.Entries, func(entry operatorv1alpha1.JournalEntry) bool {
		return entry.Type == operatorv1alpha1.JournalApprovalAccepted && entry.ApprovalUID == approval
	})
}

// schemaHeldApproval reports that the status before this write already named the
// approval with this UID. The write is then no news about it, whether the entry
// that recorded it carries a UID or was written by a release before the field,
// or has aged out of the journal.
func schemaHeldApproval(before *operatorv1alpha1.PtahSchemaStatus, approval types.UID) bool {
	return before.Plan != nil && before.Plan.Approval != nil && before.Plan.Approval.UID == approval
}

// migrationHeldApproval is schemaHeldApproval for an Apply claim.
func migrationHeldApproval(before *operatorv1alpha1.PtahMigrationStatus, approval types.UID) bool {
	operation := before.ActiveOperation
	return operation != nil && operation.ApprovalRef != nil && operation.ApprovalRef.UID == approval
}

func schemaOperation(before, after *operatorv1alpha1.PtahSchemaStatus) string {
	if after.ActiveOperation != nil {
		return string(after.ActiveOperation.Type)
	}
	if before.ActiveOperation != nil {
		return string(before.ActiveOperation.Type)
	}
	return ""
}

func firstMigrationOperation(before, after *operatorv1alpha1.PtahMigrationStatus) *operatorv1alpha1.MigrationOperationStatus {
	if after.ActiveOperation != nil {
		return after.ActiveOperation
	}
	return before.ActiveOperation
}

func runOutcome(outcome operatorv1alpha1.MigrationRunOutcome) operatorv1alpha1.JournalOutcome {
	switch outcome {
	case operatorv1alpha1.MigrationRunOutcomeApplied:
		return operatorv1alpha1.JournalSucceeded
	case operatorv1alpha1.MigrationRunOutcomeUpToDate:
		return operatorv1alpha1.JournalUpToDate
	case operatorv1alpha1.MigrationRunOutcomeFailed:
		return operatorv1alpha1.JournalFailed
	case operatorv1alpha1.MigrationRunOutcomePartial:
		return operatorv1alpha1.JournalPartial
	default:
		return operatorv1alpha1.JournalUnknown
	}
}

// publishedAt dates a schema plan's publication. A plan is first published as
// it is created, so its creation dates it. One that comes back to status
// after the journal recorded something later, such as the Apply of a plan
// whose outcome nobody could prove, is published again now: its creation
// would date the entry before what the journal already holds.
func publishedAt(created metav1.Time, journal *operatorv1alpha1.LifecycleJournal, now time.Time) metav1.Time {
	if journal != nil && len(journal.Entries) > 0 && latest(journal.Entries[len(journal.Entries)-1]).After(created.Time) {
		return metav1.NewTime(now)
	}
	return orNow(created, now)
}

func orNow(at metav1.Time, now time.Time) metav1.Time {
	if at.IsZero() {
		return metav1.NewTime(now)
	}
	return at
}
