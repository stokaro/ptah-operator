package e2e

import (
	"errors"
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/api/equality"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

var operationIDForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// runIdentityRecorded is nil when status.lastRun names the plan its run carried
// out by every identity the plan has, under the claim that ran it, with the source,
// database and execution binding the plan was made for. None of this is read from
// the plan after the run: the row compares what the run recorded with what the plan
// says, so a value that lost its way through the API server's schema shows here.
func runIdentityRecorded(status ptahv1alpha1.PtahMigrationStatus, plan *ptahv1alpha1.PtahMigrationPlan) error {
	run := status.LastRun
	switch {
	case run == nil || run.FinishedAt == nil:
		return fmt.Errorf("no finished run: %+v", run)
	case !operationIDForm.MatchString(run.OperationID):
		return fmt.Errorf("the run names operation %q", run.OperationID)
	case run.PlanRef == nil || run.PlanRef.Name != plan.Name || run.PlanRef.UID != plan.UID:
		return fmt.Errorf("the run names plan %+v, not %s (UID %s)", run.PlanRef, plan.Name, plan.UID)
	case run.PlanFingerprint != plan.Spec.Fingerprint:
		return fmt.Errorf("the run names fingerprint %q, not %q", run.PlanFingerprint, plan.Spec.Fingerprint)
	case run.ArtifactDigest != plan.Spec.ArtifactDigest:
		return fmt.Errorf("the run names artifact %q, not %q", run.ArtifactDigest, plan.Spec.ArtifactDigest)
	case !equality.Semantic.DeepEqual(run.Source, plan.Spec.Source):
		return fmt.Errorf("the run names source %+v, not %+v", run.Source, plan.Spec.Source)
	case run.TargetIdentityDigest != plan.Spec.TargetIdentityDigest:
		return fmt.Errorf("the run names database %q, not %q", run.TargetIdentityDigest, plan.Spec.TargetIdentityDigest)
	case run.ExecutionBindingID != plan.Spec.ExecutionBindingID:
		return fmt.Errorf("the run names execution binding %q, not %q", run.ExecutionBindingID, plan.Spec.ExecutionBindingID)
	}
	return nil
}

// runApprovedBy is nil when status.lastRun names the approval that allowed its run,
// with the identity and time admission stamped on it.
func runApprovedBy(status ptahv1alpha1.PtahMigrationStatus, approval *ptahv1alpha1.PtahMigrationApproval) error {
	run := status.LastRun
	if run == nil || run.Authorization == nil {
		return fmt.Errorf("the run records nothing about what allowed it: %+v", run)
	}
	authorization := run.Authorization
	switch {
	case authorization.Kind != ptahv1alpha1.MigrationRunAuthorizedByApproval:
		return fmt.Errorf("the run was authorized by %q", authorization.Kind)
	case authorization.ApprovalRef == nil || authorization.ApprovalRef.Name != approval.Name || authorization.ApprovalRef.UID != approval.UID:
		return fmt.Errorf("the run names approval %+v, not %s (UID %s)", authorization.ApprovalRef, approval.Name, approval.UID)
	case authorization.Approver == nil || authorization.Approver.Username != approval.Spec.Approver.Username:
		return fmt.Errorf("the run names approver %+v, not %q", authorization.Approver, approval.Spec.Approver.Username)
	case authorization.ApprovedAt == nil || !authorization.ApprovedAt.Equal(&approval.Spec.ApprovedAt):
		return fmt.Errorf("the run dates the approval %v, not %v", authorization.ApprovedAt, approval.Spec.ApprovedAt)
	}
	return nil
}

// runAllowedByPolicy is nil when status.lastRun says nobody decided its run.
func runAllowedByPolicy(status ptahv1alpha1.PtahMigrationStatus) error {
	run := status.LastRun
	if run == nil || run.Authorization == nil {
		return fmt.Errorf("the run records nothing about what allowed it: %+v", run)
	}
	authorization := run.Authorization
	if authorization.Kind != ptahv1alpha1.MigrationRunAuthorizedByPolicy || authorization.ApprovalRef != nil ||
		authorization.Approver != nil || authorization.ApprovedAt != nil {
		return fmt.Errorf("the run under Always records %+v", authorization)
	}
	return nil
}

// runVerifiedInSync is nil when the first reading after the run is recorded on it:
// in sync, and of a history that holds the plan the run carried out. Its time could
// not say so: the operator stamps a reading when it harvests it, so one that read the
// database before the run and was harvested after it dates after the run too. A
// history read before the run stops short of the plan's last version. The run's own
// list of versions could not say so either: it is empty when the Pod's log is gone
// and the termination summary could not hold it.
func runVerifiedInSync(status ptahv1alpha1.PtahMigrationStatus, plan *ptahv1alpha1.PtahMigrationPlan) error {
	run := status.LastRun
	if run == nil || run.FinishedAt == nil || run.Verification == nil {
		return fmt.Errorf("no reading is recorded on the run: %+v", run)
	}
	verification := run.Verification
	switch {
	case verification.Result != ptahv1alpha1.MigrationRunVerifiedInSync:
		return fmt.Errorf("the reading after the run found %q", verification.Result)
	case !operationIDForm.MatchString(verification.HistoryFingerprint):
		return fmt.Errorf("the reading names history %q", verification.HistoryFingerprint)
	case len(plan.Spec.Migrations) == 0:
		return fmt.Errorf("plan %s names no migration, so nothing shows the reading followed its run", plan.Name)
	case verification.CurrentVersion < plan.Spec.Migrations[len(plan.Spec.Migrations)-1].Version:
		return fmt.Errorf("the reading stops at version %d, before version %d the plan ends at",
			verification.CurrentVersion, plan.Spec.Migrations[len(plan.Spec.Migrations)-1].Version)
	}
	return nil
}

// runNamesNoDatabase is nil when status.lastRun leaves its run's database absent and
// no reading has been taken as a statement about the run. Call it once a reading of
// the planned database has landed, or a record with no reading on it says nothing.
func runNamesNoDatabase(status ptahv1alpha1.PtahMigrationStatus) error {
	run := status.LastRun
	switch {
	case run == nil:
		return errors.New("there is no run")
	case run.TargetIdentityDigest != "":
		return fmt.Errorf("the record names database %q, and nothing the run wrote was read", run.TargetIdentityDigest)
	case run.Verification != nil:
		return fmt.Errorf("a reading verified a run that never said which database it reached: %+v", run.Verification)
	}
	return nil
}

// runJournaled is nil when the journal joins one end and one reading to the run by
// its operation ID, and one acceptance by its approval's UID, each naming the plan.
// A journal with none of them is refused: the join is what the row exists for.
func runJournaled(status ptahv1alpha1.PtahMigrationStatus) error {
	run := status.LastRun
	if run == nil || run.OperationID == "" || run.PlanRef == nil || status.Journal == nil ||
		run.Authorization == nil || run.Authorization.ApprovalRef == nil {
		return fmt.Errorf("nothing to join: run %+v, journal %+v", run, status.Journal)
	}
	counts := map[ptahv1alpha1.JournalEntryType]int{}
	for _, entry := range status.Journal.Entries {
		if entry.OperationID != run.OperationID &&
			(entry.Type != ptahv1alpha1.JournalApprovalAccepted || entry.ApprovalUID != run.Authorization.ApprovalRef.UID) {
			continue
		}
		counts[entry.Type]++
		if entry.Plan != run.PlanRef.Name || entry.PlanUID != run.PlanRef.UID {
			return fmt.Errorf("entry %s of the run names plan %q (UID %q), not %s", entry.Type, entry.Plan, entry.PlanUID, run.PlanRef.Name)
		}
		if entry.Type == ptahv1alpha1.JournalRunFinished && (entry.JobUID != run.JobUID || entry.JobUID == "") {
			return fmt.Errorf("the run's end names Job UID %q, not %q", entry.JobUID, run.JobUID)
		}
	}
	for _, kind := range []ptahv1alpha1.JournalEntryType{
		ptahv1alpha1.JournalApprovalAccepted, ptahv1alpha1.JournalRunFinished, ptahv1alpha1.JournalVerified,
	} {
		if counts[kind] != 1 {
			return fmt.Errorf("the journal holds %d %s entries for the run, want one", counts[kind], kind)
		}
	}
	return nil
}
