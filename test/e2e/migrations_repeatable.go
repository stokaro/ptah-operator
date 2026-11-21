package e2e

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// repeatableIdentity is the repeatable migration the row's fixtures carry, as
// R__e2e_item_names.sql.
const repeatableIdentity = "e2e_item_names"

// repeatableChecksum is the form of the content digest Ptah gives a
// repeatable migration and a plan binds.
var repeatableChecksum = regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`)

// repeatableRunApplied holds a run record to a run of a Job other than the
// previous one that applied exactly the versions and the repeatable
// migrations given. A run that applied the versions and left the repeatable
// migration unfinished reads as applied work too, and names none.
func repeatableRunApplied(run *ptahv1alpha1.MigrationRunStatus, previousJobUID string, versions []int64, identities []string) error {
	switch {
	case run == nil:
		return errors.New("the migration has recorded no run")
	case run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied:
		return fmt.Errorf("the last run reads %s, want Applied (%q)", run.Outcome, run.Message)
	case run.JobUID == "" || string(run.JobUID) == previousJobUID:
		return fmt.Errorf("the last run is Job %q, want a run after Job %q", run.JobUID, previousJobUID)
	case !slices.Equal(run.AppliedVersions, versions):
		return fmt.Errorf("the run applied versions %v, want %v", run.AppliedVersions, versions)
	case len(identities) == 0:
		return errors.New("the row names no repeatable migration to look for")
	case !slices.Equal(run.AppliedRepeatables, identities):
		return fmt.Errorf("the run applied repeatable migrations %v, want %v", run.AppliedRepeatables, identities)
	}
	return nil
}

// settledOnRepeatableRun is the migration in sync for its current generation,
// with nothing in flight and no run unaccounted for, after a run
// repeatableRunApplied accepts.
func settledOnRepeatableRun(resource *ptahv1alpha1.PtahMigration, previousJobUID string, versions []int64, identities []string) error {
	status := resource.Status
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseInSync:
		return fmt.Errorf("the migration is %s, want InSync", status.Phase)
	case resource.Generation < 1 || status.ObservedGeneration != resource.Generation:
		return fmt.Errorf("the status was written for generation %d, and the migration is at %d", status.ObservedGeneration, resource.Generation)
	case status.ActiveOperation != nil || status.UnresolvedRun != nil:
		return errors.New("the migration still has an operation or an unresolved run")
	case status.History == nil || status.History.PendingRepeatableCount != 0 ||
		len(status.History.UnfinishedRepeatables) != 0 || len(status.History.OrphanedRepeatables) != 0:
		return fmt.Errorf("the history still lists repeatable work: %+v", status.History)
	}
	return repeatableRunApplied(status.LastRun, previousJobUID, versions, identities)
}

// repeatablePlanned holds a plan to the versions given and to exactly one
// repeatable migration, with a content digest, run after them. applied is the
// digest the database recorded for it before, and empty for one it has no
// record of.
func repeatablePlanned(plan *ptahv1alpha1.PtahMigrationPlan, versions []int64, identity, applied string) error {
	planned := make([]int64, 0, len(plan.Spec.Migrations))
	for _, migration := range plan.Spec.Migrations {
		planned = append(planned, migration.Version)
	}
	switch {
	case !slices.Equal(planned, versions) && (len(planned) > 0 || len(versions) > 0):
		return fmt.Errorf("the plan %s approves versions %v, want %v", plan.Name, planned, versions)
	case len(plan.Spec.Repeatables) != 1:
		return fmt.Errorf("the plan %s selects %d repeatable migrations, want 1", plan.Name, len(plan.Spec.Repeatables))
	}
	repeatable := plan.Spec.Repeatables[0]
	switch {
	case repeatable.Identity != identity:
		return fmt.Errorf("the plan %s selects repeatable migration %q, want %q", plan.Name, repeatable.Identity, identity)
	case !repeatableChecksum.MatchString(repeatable.Checksum):
		return fmt.Errorf("the plan %s binds content %q, which is not a content digest", plan.Name, repeatable.Checksum)
	case repeatable.AppliedChecksum != applied:
		return fmt.Errorf("the plan %s says the database recorded %q, want %q", plan.Name, repeatable.AppliedChecksum, applied)
	case repeatable.Checksum == applied:
		return fmt.Errorf("the plan %s selects content the database already applied", plan.Name)
	}
	return nil
}

// repeatableRepair is the command the operator names for deleting the record
// of a repeatable migration. Ptah requires --force while a file of the
// directory carries the identity, so only the record no file carries is
// deleted without it.
func repeatableRepair(identity string, force bool) string {
	command := "ptah migrations repair --repeatable " + identity
	if force {
		command += " --force"
	}
	return command
}

// repeatableStop is the migration stopped on an unfinished record of one
// repeatable migration, with the history listing it, and the repair command
// the condition's message names. The command has to be the one Ptah accepts
// in that state, --force while a file carries the identity, since the row
// runs what the message says.
func repeatableStop(status ptahv1alpha1.PtahMigrationStatus, identity string) (string, error) {
	var listed []string
	if status.History != nil {
		listed = status.History.UnfinishedRepeatables
	}
	reason := ptahv1alpha1.ReasonRepeatableMigrationUnfinished
	opening := "Repeatable migration: " + identity + " "
	blocked := meta.FindStatusCondition(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked)
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseBlocked:
		return "", fmt.Errorf("the migration is %s, want Blocked", status.Phase)
	case blocked == nil || blocked.Status != metav1.ConditionTrue || blocked.Reason != string(reason):
		return "", fmt.Errorf("the Blocked condition is %+v, want True for %s", blocked, reason)
	case !slices.Equal(listed, []string{identity}):
		return "", fmt.Errorf("the history lists %v unfinished, want [%s]", listed, identity)
	case !strings.HasPrefix(blocked.Message, opening):
		return "", fmt.Errorf("the condition says %q, want it to open %q", blocked.Message, opening)
	}
	_, rest, found := strings.Cut(blocked.Message, "(repair: `")
	command, _, closed := strings.Cut(rest, "`")
	if !found || !closed {
		return "", fmt.Errorf("the condition %q names no repair command", blocked.Message)
	}
	if want := repeatableRepair(identity, true); command != want {
		return "", fmt.Errorf("the condition names %q, and Ptah repairs this record with %q", command, want)
	}
	return command, nil
}

// waitStoppedOnUnfinishedRepeatable holds what kubectl ptah wrote to stderr when a
// wait for target met a migration the record of identity's unfinished attempt
// stops: exit 8; for a wait for a read, the headline that says the read ended
// Blocked for that reason, and for any other target the Blocked stop of that
// target; and the reason with the condition's words about identity. Before
// stokaro/ptah-operator#1162, refresh --wait exited 0 on the read that ended
// there, and kubectl ptah wait --for in-sync ran to its timeout and exited 7,
// though every read gave the same answer.
func waitStoppedOnUnfinishedRepeatable(code int, stderr, target, identity string) error {
	if code != approveWaitWillNotReach {
		return fmt.Errorf("exit status %d, want %d", code, approveWaitWillNotReach)
	}
	reason := string(ptahv1alpha1.ReasonRepeatableMigrationUnfinished)
	headline := " will not reach " + target + ": Blocked.\n"
	if target == "read" {
		headline = ": the read ended Blocked: " + reason + ".\n"
	}
	if !strings.Contains(stderr, headline) {
		return fmt.Errorf("stderr does not say the wait for %s stopped on a Blocked (%q)", target, strings.TrimSpace(headline))
	}
	why := "\n  Why:         " + reason + ": Repeatable migration: " + identity + " "
	if !strings.Contains(stderr, why) {
		return fmt.Errorf("stderr does not name %s with the condition's words about %s", reason, identity)
	}
	return nil
}

// orphanedRepeatableSettled is the migration converged beside an applied
// record of one repeatable migration that no file carries: in sync and Ready
// for its current generation and not Blocked, with nothing in flight, no run
// unaccounted for and nothing pending or unfinished, the history naming that
// record alone, and no run after Job previousJobUID. Ptah runs nothing for
// such a record, so a later run would be work the artifact does not describe.
func orphanedRepeatableSettled(resource *ptahv1alpha1.PtahMigration, previousJobUID, identity string) error {
	status := resource.Status
	ready := meta.FindStatusCondition(status.Conditions, ptahv1alpha1.ConditionMigrationReady)
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseInSync:
		return fmt.Errorf("the migration is %s, want InSync", status.Phase)
	case resource.Generation < 1 || status.ObservedGeneration != resource.Generation:
		return fmt.Errorf("the status was written for generation %d, and the migration is at %d", status.ObservedGeneration, resource.Generation)
	case ready == nil || ready.Status != metav1.ConditionTrue:
		return fmt.Errorf("the Ready condition is %+v, want True", ready)
	case meta.IsStatusConditionTrue(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked):
		return errors.New("the migration is Blocked")
	case status.ActiveOperation != nil || status.UnresolvedRun != nil:
		return errors.New("the migration still has an operation or an unresolved run")
	case status.History == nil:
		return errors.New("the migration has recorded no history")
	case status.History.PendingCount != 0 || status.History.PendingRepeatableCount != 0 || len(status.History.UnfinishedRepeatables) != 0:
		return fmt.Errorf("the history lists work: %+v", status.History)
	case !slices.Equal(status.History.OrphanedRepeatables, []string{identity}):
		return fmt.Errorf("the history lists %v orphaned, want [%s]", status.History.OrphanedRepeatables, identity)
	case previousJobUID == "" || status.LastRun == nil || string(status.LastRun.JobUID) != previousJobUID:
		return fmt.Errorf("the last run is %+v, want the run of Job %q and nothing after it", status.LastRun, previousJobUID)
	}
	return nil
}

// repeatableOrphanedEventReason is the reason of the Warning Event the
// operator emits when a history read first lists a record no file carries.
const repeatableOrphanedEventReason = "RepeatableMigrationOrphaned"

// orphanedRepeatableEvent holds the Events of a namespace to exactly one
// Warning Event about the object with UID uid that names identity as a record
// no file carries and ends with the command that deletes it, without --force,
// emitted once. The event recorder folds a repeated Event into the first and
// counts it, and a recorder that restarted creates another, so a count above
// one or a second Event is a second emission.
func orphanedRepeatableEvent(events []corev1.Event, uid types.UID, identity string) error {
	if uid == "" {
		return errors.New("the row names no object to look for")
	}
	var matched []corev1.Event
	for _, event := range events {
		if event.InvolvedObject.UID == uid && event.Reason == repeatableOrphanedEventReason {
			matched = append(matched, event)
		}
	}
	if len(matched) != 1 {
		return fmt.Errorf("%d of %d Events are %s about %s, want 1", len(matched), len(events), repeatableOrphanedEventReason, uid)
	}
	event, command := matched[0], "`"+repeatableRepair(identity, false)+"`"
	switch {
	case event.Type != corev1.EventTypeWarning:
		return fmt.Errorf("the Event is %s, want Warning", event.Type)
	case event.Count != 1 || event.Series != nil:
		return fmt.Errorf("the Event was emitted %d times (series %+v), want once", event.Count, event.Series)
	case !strings.Contains(event.Message, "repeatable migration "+identity+" as applied"):
		return fmt.Errorf("the Event says %q, which does not name %s", event.Message, identity)
	case !strings.HasSuffix(event.Message, command):
		return fmt.Errorf("the Event says %q, want it to end with %s", event.Message, command)
	}
	return nil
}

// repeatableAttemptRecorded is the run of a Job other than the previous one
// that ran the repeatable migration and failed at it, recorded the way its
// engine ran it. Inside a transaction the failure took no effect, so the run
// is Failed and settled. Outside one, as on MySQL, it may have left part of
// its work behind, so the run is Unknown and stays unaccounted for until a
// person says what it did.
func repeatableAttemptRecorded(status ptahv1alpha1.PtahMigrationStatus, previousJobUID, identity string, transactional bool) error {
	run := status.LastRun
	outcome := ptahv1alpha1.MigrationRunOutcomeUnknown
	opening := "Repeatable migration " + identity + " failed outside a transaction and may have left part of its work behind"
	if transactional {
		outcome = ptahv1alpha1.MigrationRunOutcomeFailed
		opening = "Repeatable migration " + identity + " failed inside a transaction, which took no effect"
	}
	switch {
	case run == nil:
		return errors.New("the migration has recorded no run")
	case run.JobUID == "" || string(run.JobUID) == previousJobUID:
		return fmt.Errorf("the last run is Job %q, want a run after Job %q", run.JobUID, previousJobUID)
	case run.Outcome != outcome:
		return fmt.Errorf("the last run reads %s, want %s (%q)", run.Outcome, outcome, run.Message)
	case !strings.HasPrefix(run.Message, opening):
		return fmt.Errorf("the last run says %q, want it to open %q", run.Message, opening)
	case len(run.AppliedVersions) != 0 || len(run.AppliedRepeatables) != 0:
		return fmt.Errorf("the run records versions %v and repeatable migrations %v applied", run.AppliedVersions, run.AppliedRepeatables)
	case transactional && status.UnresolvedRun != nil:
		return errors.New("a failure that took no effect left a run unaccounted for")
	case !transactional && (status.UnresolvedRun == nil || status.UnresolvedRun.OperationID != run.OperationID):
		return fmt.Errorf("the run that may have left work behind is not the unresolved run: %+v", status.UnresolvedRun)
	}
	return nil
}

// repeatableLogQuery prints the database's log entries about one repeatable
// migration, one per line, as the state and the hex of the run reference: the
// reference crosses as hex for the reason migrationLogQuery gives.
func (e migrationEngine) repeatableLogQuery(identity string) string {
	return "SELECT concat(state, ':', " + hexText(e.name, "run_reference") +
		") FROM schema_migrations_log WHERE repeatable = '" + identity + "' ORDER BY seq"
}

// repeatableRunLogged holds what repeatableLogQuery printed to the entries of
// the run given: exactly the states given, in order. Entries of other runs are
// left out, and a run that wrote none is refused, since a log that holds
// nothing names nobody.
func repeatableRunLogged(output, reference string, states ...string) error {
	if reference == "" || len(states) == 0 {
		return errors.New("the row names no run or no entry to look for")
	}
	var logged []string
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		state, encoded, found := strings.Cut(line, ":")
		if !found || state == "" {
			return fmt.Errorf("the log row %q is not state:reference", line)
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("the log row %q carries a reference that is not hex: %w", line, err)
		}
		if string(decoded) == reference {
			logged = append(logged, state)
		}
	}
	if !slices.Equal(logged, states) {
		return fmt.Errorf("the log holds %v for run %s, want %v", logged, reference, states)
	}
	return nil
}
