package operate

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// snapshot is what a wait reads from one document of either family. The
// fields are the ones a target is decided by; nothing here is derived from
// the clock of the process reading it.
type snapshot struct {
	family             Family
	uid                types.UID
	generation         int64
	observedGeneration int64
	suspend            bool
	// deleting is a resource with metadata.deletionTimestamp set.
	deleting bool
	phase    string
	// observedAt is when the database was last read: status.target.lastObservedAt
	// for a schema, status.history.observedAt for a migration. Zero when
	// never.
	observedAt time.Time
	// operation is the type of status.activeOperation, empty when nothing is
	// in flight.
	operation string
	// operationPlan is the plan a migration's Apply claim in flight carries out.
	operationPlan planRecord
	// dispatched reports that the Job of the operation in flight may exist,
	// by the rule the controller decides it with
	// (mutationlifecycle.MayHaveDispatched).
	dispatched bool
	// proof is a schema's post-Apply proof still owed
	// (status.pendingObservation), and proofPlan the plan whose Apply owes
	// it.
	proof           bool
	proofPlan       planRecord
	readRequest     string
	lastReadRequest string
	conditions      []metav1.Condition
	plan            *planRecord
	history         bool
	applied         *appliedRecord
	lastRun         *runRecord
	unresolved      *unresolvedRecord
	journal         *operatorv1alpha1.LifecycleJournal
	object          client.Object
}

type planRecord struct {
	name        string
	uid         types.UID
	fingerprint string
	approved    bool
}

// appliedRecord is a schema's status.applied.
type appliedRecord struct {
	plan        string
	planUID     types.UID
	fingerprint string
	completedAt time.Time
}

// runRecord is a migration's status.lastRun.
type runRecord struct {
	outcome    operatorv1alpha1.MigrationRunOutcome
	jobName    string
	jobUID     types.UID
	startedAt  time.Time
	finished   bool
	finishedAt time.Time
	message    string
	identity   runIdentity
	// verified is the first reading of the database after the run, and
	// verification what it found.
	verified     bool
	verification operatorv1alpha1.MigrationRunVerificationResult
}

// runIdentity is what a record says a run carried out and who allowed it. All of
// it is empty on a record a release before these fields wrote, and what allowed the
// run is empty on the entry a hold brought back by a restore writes, since a hold
// does not hold it.
type runIdentity struct {
	plan          string
	planUID       types.UID
	fingerprint   string
	authorization operatorv1alpha1.MigrationRunAuthorizationKind
	approval      string
	approvalUID   types.UID
}

// unresolvedRecord is a migration's status.unresolvedRun.
type unresolvedRecord struct {
	operationID string
	outcome     operatorv1alpha1.MigrationRunOutcome
	jobName     string
	plan        string
	planUID     types.UID
}

func snapshotOf(object client.Object) (snapshot, error) {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		return schemaSnapshot(typed), nil
	case *operatorv1alpha1.PtahMigration:
		return migrationSnapshot(typed), nil
	default:
		return snapshot{}, fmt.Errorf("%T is neither a PtahSchema nor a PtahMigration", object)
	}
}

func schemaSnapshot(schema *operatorv1alpha1.PtahSchema) snapshot {
	status := &schema.Status
	s := snapshot{
		family:             FamilySchema,
		uid:                schema.UID,
		generation:         schema.Generation,
		observedGeneration: status.ObservedGeneration,
		suspend:            schema.Spec.Suspend,
		deleting:           schema.DeletionTimestamp != nil,
		phase:              string(status.Phase),
		observedAt:         timeOf(status.Target.LastObservedAt),
		proof:              status.PendingObservation != nil,
		readRequest:        schema.Annotations[operatorv1alpha1.ReadRequestAnnotation],
		lastReadRequest:    status.LastReadRequest,
		conditions:         status.Conditions,
		journal:            status.Journal,
		object:             schema,
	}
	if operation := status.ActiveOperation; operation != nil {
		s.operation = string(operation.Type)
		s.dispatched = mutationlifecycle.MayHaveDispatched(mutationlifecycle.DispatchState{
			DispatchStarted: operation.DispatchStarted, JobUID: string(operation.JobUID),
		})
	}
	if pending := status.PendingObservation; pending != nil {
		s.proofPlan = planRecord{name: pending.Plan.Name, uid: pending.Plan.UID, fingerprint: pending.Plan.Fingerprint}
	}
	if plan := status.Plan; plan != nil {
		s.plan = &planRecord{name: plan.Name, uid: plan.UID, fingerprint: plan.Fingerprint, approved: plan.Approval != nil}
	}
	if applied := status.Applied; applied != nil {
		s.applied = &appliedRecord{
			plan: applied.PlanRef.Name, planUID: applied.PlanRef.UID,
			fingerprint: applied.PlanFingerprint, completedAt: applied.CompletedAt.Time,
		}
	}
	return s
}

func migrationSnapshot(migration *operatorv1alpha1.PtahMigration) snapshot {
	status := &migration.Status
	s := snapshot{
		family:             FamilyMigration,
		uid:                migration.UID,
		generation:         migration.Generation,
		observedGeneration: status.ObservedGeneration,
		suspend:            migration.Spec.Suspend,
		deleting:           migration.DeletionTimestamp != nil,
		phase:              string(status.Phase),
		readRequest:        migration.Annotations[operatorv1alpha1.ReadRequestAnnotation],
		lastReadRequest:    status.LastReadRequest,
		conditions:         status.Conditions,
		history:            status.History != nil,
		journal:            status.Journal,
		object:             migration,
	}
	if history := status.History; history != nil {
		s.observedAt = history.ObservedAt.Time
	}
	if operation := status.ActiveOperation; operation != nil {
		s.operation = string(operation.Type)
		s.dispatched = mutationlifecycle.MayHaveDispatched(mutationlifecycle.DispatchState{
			DispatchStarted: operation.DispatchStarted, JobUID: string(operation.JobUID),
		})
		if plan := operation.PlanRef; plan != nil {
			s.operationPlan = planRecord{name: plan.Name, uid: plan.UID}
		}
	}
	if plan := status.Plan; plan != nil {
		s.plan = &planRecord{name: plan.Name, uid: plan.UID}
	}
	if run := status.LastRun; run != nil {
		s.lastRun = &runRecord{
			outcome: run.Outcome, jobName: run.JobName, jobUID: run.JobUID,
			startedAt: run.StartedAt.Time, finished: run.FinishedAt != nil,
			finishedAt: timeOf(run.FinishedAt), message: run.Message, identity: identityOfRun(run),
			verified: run.Verification != nil,
		}
		if run.Verification != nil {
			s.lastRun.verification = run.Verification.Result
		}
	}
	if run := status.UnresolvedRun; run != nil {
		s.unresolved = &unresolvedRecord{
			operationID: run.OperationID, outcome: run.Outcome, jobName: run.JobName,
			plan: run.PlanRef.Name, planUID: run.PlanRef.UID,
		}
	}
	return s
}

func identityOfRun(run *operatorv1alpha1.MigrationRunStatus) runIdentity {
	identity := runIdentity{fingerprint: run.PlanFingerprint}
	if plan := run.PlanRef; plan != nil {
		identity.plan, identity.planUID = plan.Name, plan.UID
	}
	if authorization := run.Authorization; authorization != nil {
		identity.authorization = authorization.Kind
		if approval := authorization.ApprovalRef; approval != nil {
			identity.approval, identity.approvalUID = approval.Name, approval.UID
		}
	}
	return identity
}

func identityOfEntry(entry operatorv1alpha1.JournalEntry) runIdentity {
	return runIdentity{
		plan: entry.Plan, planUID: entry.PlanUID, fingerprint: entry.PlanFingerprint,
		authorization: entry.Authorization, approval: entry.Approval, approvalUID: entry.ApprovalUID,
	}
}

func timeOf(value *metav1.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.Time
}

// ReadEndsIn reports whether a read cycle of family can end in phase: the
// phases in which the controller answers a pending read request and brings
// the next read forward, rather than leaving the request for a read that has
// not finished (internal/controller/read_request.go), plus Failed, where a
// failed cycle stops, and Suspended.
//
// For a schema that is InSync, AwaitingApproval, ReadyToApply, Blocked,
// Failed and Suspended. Verifying, Observing, Planning and
// VerifyingConvergence are a cycle part way through; Pending, Resolving and
// Applying are work about to start or in flight.
//
// For a migration it is InSync, AwaitingApproval, Planning, Blocked, Failed
// and Suspended. Planning ends a read only once its plan is published, which
// a document has to show as well (see snapshot.resting).
func ReadEndsIn(family Family, phase string) bool {
	switch family {
	case FamilySchema:
		switch operatorv1alpha1.ReconciliationPhase(phase) {
		case operatorv1alpha1.PhaseInSync, operatorv1alpha1.PhaseAwaitingApproval,
			operatorv1alpha1.PhaseReadyToApply, operatorv1alpha1.PhaseBlocked,
			operatorv1alpha1.PhaseFailed, operatorv1alpha1.PhaseSuspended:
			return true
		}
	case FamilyMigration:
		switch operatorv1alpha1.MigrationPhase(phase) {
		case operatorv1alpha1.MigrationPhaseInSync, operatorv1alpha1.MigrationPhaseAwaitingApproval,
			operatorv1alpha1.MigrationPhasePlanning, operatorv1alpha1.MigrationPhaseBlocked,
			operatorv1alpha1.MigrationPhaseFailed, operatorv1alpha1.MigrationPhaseSuspended:
			return true
		}
	}
	return false
}

// resting reports a resource with nothing in flight in a phase a read ends
// in (ReadEndsIn).
//
// A migration in Planning rests only once the plan it read is published. The
// history harvest is two status writes: the first records the history and
// Planning with Progressing reason MigrationsPending, and leaves the previous
// cycle's status.plan in place; the second publishes the plan and replaces
// that reason. A plan alone therefore does not show the second write.
func (s snapshot) resting() bool {
	if s.operation != "" || s.proof || !ReadEndsIn(s.family, s.phase) {
		return false
	}
	if s.family == FamilyMigration && s.phase == string(operatorv1alpha1.MigrationPhasePlanning) {
		progressing := s.condition(operatorv1alpha1.ConditionMigrationProgressing)
		return s.plan != nil &&
			(progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonMigrationsPending))
	}
	return true
}

// readingDatabase reports a claim whose Job reads the database and writes the
// reading it took: a schema's Observe, a migration's History. A wait that
// begins while one is in flight cannot tell whether that Job read the
// database before the change the wait is about.
func (s snapshot) readingDatabase() bool {
	switch s.family {
	case FamilySchema:
		return s.operation == string(operatorv1alpha1.OperationObserve)
	case FamilyMigration:
		return s.operation == string(operatorv1alpha1.MigrationOperationHistory)
	}
	return false
}

func (s snapshot) condition(conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(s.conditions, conditionType)
}

func (s snapshot) conditionTrue(conditionType string) bool {
	return meta.IsStatusConditionTrue(s.conditions, conditionType)
}

// inSyncCondition is the condition that says a resource converged: InSync
// for a schema, Ready for a migration, which publishes no InSync.
func (s snapshot) inSyncCondition() string {
	if s.family == FamilyMigration {
		return operatorv1alpha1.ConditionMigrationReady
	}
	return operatorv1alpha1.ConditionInSync
}

// blocker is the condition that best says why the resource is not moving: a
// recorded failure or refusal first, then a Ready that is not True.
//
// A schema records a failed attempt on ReconciliationFailed, and a Pod
// admission refusal or a Pod the kubelet cannot configure on Ready. A migration
// records all of them on Progressing, which a retried attempt leaves True with
// the failure as its message, and a claim waiting for the database lock says so
// there too.
func (s snapshot) blocker() *metav1.Condition {
	if s.family == FamilyMigration {
		if blocked := s.condition(operatorv1alpha1.ConditionMigrationBlocked); blocked != nil && blocked.Status == metav1.ConditionTrue {
			return blocked.DeepCopy()
		}
		if progressing := s.condition(operatorv1alpha1.ConditionMigrationProgressing); progressing != nil &&
			(progressing.Reason == string(operatorv1alpha1.ReasonOperationFailed) ||
				progressing.Reason == string(operatorv1alpha1.ReasonPodAdmissionRefused) ||
				progressing.Reason == string(operatorv1alpha1.ReasonPodConfigurationError) ||
				progressing.Reason == string(operatorv1alpha1.ReasonWaitingForDatabaseLock)) {
			return progressing.DeepCopy()
		}
	} else {
		for _, conditionType := range []string{operatorv1alpha1.ConditionReconciliationFailed, operatorv1alpha1.ConditionSuspended} {
			if condition := s.condition(conditionType); condition != nil && condition.Status == metav1.ConditionTrue {
				return condition.DeepCopy()
			}
		}
	}
	if ready := s.condition(operatorv1alpha1.ConditionReady); ready != nil && ready.Status != metav1.ConditionTrue {
		return ready.DeepCopy()
	}
	return nil
}

// failureMark identifies the newest failed attempt the journal records. It
// changes when a new failure is recorded, including one the journal folds
// into the entry before it, which raises the count rather than adding an
// entry.
type failureMark struct {
	present   bool
	at        time.Time
	last      time.Time
	count     int32
	operation string
	reason    string
}

func (s snapshot) failureMark() failureMark {
	if s.journal == nil {
		return failureMark{}
	}
	for index := len(s.journal.Entries) - 1; index >= 0; index-- {
		entry := s.journal.Entries[index]
		if entry.Type != operatorv1alpha1.JournalOperationFailed {
			continue
		}
		return failureMark{
			present: true, at: entry.At.Time, last: timeOf(entry.LastAt), count: entry.Count,
			operation: entry.Operation, reason: entry.Reason,
		}
	}
	return failureMark{}
}

func (m failureMark) equal(other failureMark) bool {
	return m.present == other.present && m.at.Equal(other.at) && m.last.Equal(other.last) &&
		m.count == other.count && m.operation == other.operation && m.reason == other.reason
}

// journalSince returns the entries current records that baseline did not:
// the ones after baseline's newest entry, and that entry again when the
// journal has since folded another failed attempt into it.
func journalSince(baseline, current *operatorv1alpha1.LifecycleJournal) []operatorv1alpha1.JournalEntry {
	if current == nil {
		return nil
	}
	if baseline == nil || len(baseline.Entries) == 0 {
		return current.Entries
	}
	newest := baseline.Entries[len(baseline.Entries)-1]
	for index := len(current.Entries) - 1; index >= 0; index-- {
		entry := current.Entries[index]
		if !sameEvent(entry, newest) {
			continue
		}
		since := current.Entries[index+1:]
		if entry.Count != newest.Count || !timeOf(entry.LastAt).Equal(timeOf(newest.LastAt)) {
			since = append([]operatorv1alpha1.JournalEntry{entry}, since...)
		}
		return since
	}
	// The baseline's newest entry is gone, which takes more new entries than
	// the journal keeps. Everything later than it is new.
	latest := latestOf(newest)
	var since []operatorv1alpha1.JournalEntry
	for _, entry := range current.Entries {
		if latestOf(entry).After(latest) {
			since = append(since, entry)
		}
	}
	return since
}

// journalThrough is journal up to and including entry: a baseline from
// which journalSince reports only what came after it.
func journalThrough(journal *operatorv1alpha1.LifecycleJournal, entry operatorv1alpha1.JournalEntry) *operatorv1alpha1.LifecycleJournal {
	through := &operatorv1alpha1.LifecycleJournal{CoveredSince: journal.CoveredSince}
	for _, candidate := range journal.Entries {
		through.Entries = append(through.Entries, candidate)
		if sameEvent(candidate, entry) {
			break
		}
	}
	return through
}

// sameEvent compares two entries without the fields a later failed attempt
// folds into an OperationFailed entry.
func sameEvent(a, b operatorv1alpha1.JournalEntry) bool {
	return a.Type == b.Type && a.At.Equal(&b.At) && timeOf(a.StartedAt).Equal(timeOf(b.StartedAt)) &&
		a.OperationID == b.OperationID && a.Plan == b.Plan && a.Approval == b.Approval && a.ApprovalUID == b.ApprovalUID && a.Outcome == b.Outcome &&
		a.Operation == b.Operation && a.Reason == b.Reason && a.Acknowledgment == b.Acknowledgment
}

func latestOf(entry operatorv1alpha1.JournalEntry) time.Time {
	if entry.LastAt != nil {
		return entry.LastAt.Time
	}
	return entry.At.Time
}
