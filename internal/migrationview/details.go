package migrationview

import (
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Details is what --details adds: the status, and the read request the
// resource holds now, which the status answers.
//
// Status is decoded into the API types this plugin was built with, so it
// holds every field this plugin's API version knows; a field a newer operator
// added to the stored object is not in it. The text prints every record of
// Status the view does not already print in full; the JSON carries Status as
// decoded, so a script reads the fields the API reference of this version
// documents.
type Details struct {
	// ReadRequested is the operator.ptah.run/read-requested-at annotation as
	// the migration holds it now. status.lastReadRequest is the last value
	// the controller answered.
	ReadRequested string                               `json:"readRequested,omitempty"`
	Status        operatorv1alpha1.PtahMigrationStatus `json:"status"`
}

func detailsOf(migration *operatorv1alpha1.PtahMigration) *Details {
	return &Details{
		ReadRequested: migration.Annotations[operatorv1alpha1.ReadRequestAnnotation],
		Status:        *migration.Status.DeepCopy(),
	}
}

// renderDetails prints the records the view summarizes or leaves out, each
// whole. The conditions are already printed whole, and the journal is what
// kubectl ptah history prints.
func renderDetails(text *strings.Builder, view View) {
	details := view.Details
	status := details.Status
	text.WriteString(statusview.DetailsHeading)

	statusview.WriteStatusScalars(text, statusview.StatusFor(status.ObservedGeneration, view.Generation), status,
		statusview.ReadRequested(details.ReadRequested, status.LastReadRequest))

	if operation := status.ActiveOperation; operation != nil {
		lead := statusview.JobLead(operation.JobName, operation.JobUID, operation.DispatchStarted,
			mutationlifecycle.MigrationOperation(operation.Type).HoldsLock(false))
		// retryNotBefore stays on the claim once it has passed, so it is said
		// only of an attempt that has not dispatched, and in words that hold
		// before the time and after it.
		if retry := statusview.After(operation.RetryNotBefore); retry != "" &&
			operation.JobUID == "" && !operation.DispatchStarted {
			lead += " This attempt is not dispatched before " + retry + ", the delay the earlier one asked for."
		}
		statusview.WriteRecord(text, "Active operation",
			statusview.Claimed(string(operation.Type), operation.Attempt, operation.StartedAt),
			lead, operation)
	} else {
		statusview.WriteRecord(text, "Active operation", "none", "", nil)
	}
	if release := status.PendingLockRelease; release != nil {
		statusview.WriteRecord(text, "Lock release", "owed",
			"The operation ended and the database Lease it held is still to be handed back. This record keeps "+
				"the Lease's owner and epoch until a release succeeds.", release)
	}
	if binding := status.ExecutionBinding; binding != nil {
		statusview.WriteRecord(text, "Execution binding", statusview.Clean(binding.Epoch), "", binding)
	}
	if artifact := status.Artifact; artifact != nil {
		statusview.WriteRecord(text, "Artifact", "", "", artifact)
	}
	if configMap := status.ConfigMap; configMap != nil {
		statusview.WriteRecord(text, "ConfigMap", "", "", configMap)
	}
	if policy := status.SourcePolicy; policy != nil {
		statusview.WriteRecord(text, "Source policy", "", "", policy)
	}
	if history := status.History; history != nil {
		statusview.WriteRecord(text, "History read", statusview.Time(history.ObservedAt),
			"The database's revision table as the operator last read it, with its newest rows. It is the "+
				"database's record of what ran, apart from the operator's journal.", history)
	}
	if plan := status.Plan; plan != nil {
		statusview.WriteRecord(text, "Plan", statusview.Clean(plan.Name), "", plan)
	}
	if run := status.LastRun; run != nil {
		statusview.WriteRecord(text, "Last run", statusview.Clean(string(run.Outcome)),
			"The most recent run. A run nobody has accounted for is the unresolved run, a record of its own.", run)
	}
	if run := status.UnresolvedRun; run != nil {
		statusview.WriteRecord(text, "Unresolved run", statusview.Clean(string(run.Outcome)),
			"It holds the migration whatever the last run or the conditions say, until a reading of the "+
				"database or a person's acknowledgment settles it.", run)
	}
	if run := status.ResolvedRun; run != nil {
		statusview.WriteRecord(text, "Settled run", statusview.Clean(string(run.Resolution)), resolvedLead(*run), run)
	}
}

// resolvedLead says what settled a run, and what an acknowledgment is not.
func resolvedLead(run operatorv1alpha1.ResolvedMigrationRunStatus) string {
	switch run.Resolution {
	case operatorv1alpha1.MigrationRunResolvedByAcknowledgment:
		return "A person acknowledged the run: a statement that it is accounted for, not proof that the data is correct."
	case operatorv1alpha1.MigrationRunResolvedByHistoryRead:
		return "A read-only reading of the database found nothing of the artifact left to apply."
	default:
		return ""
	}
}
