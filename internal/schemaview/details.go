package schemaview

import (
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Details is what --details adds: the whole status as stored, and the read
// request the spec holds now, which the status answers.
//
// The text prints every record of Status the view does not already print in
// full; the JSON carries Status as it is, so a script reads the same fields
// the API reference documents.
type Details struct {
	// ReadRequested is the operator.ptah.run/read-requested-at annotation as
	// the schema holds it now. status.lastReadRequest is the last value the
	// controller answered.
	ReadRequested string                            `json:"readRequested,omitempty"`
	Status        operatorv1alpha1.PtahSchemaStatus `json:"status"`
}

func detailsOf(schema *operatorv1alpha1.PtahSchema) *Details {
	return &Details{
		ReadRequested: schema.Annotations[operatorv1alpha1.ReadRequestAnnotation],
		Status:        *schema.Status.DeepCopy(),
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
		statusview.WriteRecord(text, "Active operation",
			statusview.Claimed(string(operation.Type), operation.Attempt, operation.StartedAt),
			statusview.JobLead(operation.JobName, operation.JobUID, operation.DispatchStarted,
				mutationlifecycle.SchemaOperation(operation.Type).HoldsLock(status.PendingObservation != nil)),
			operation)
	} else {
		statusview.WriteRecord(text, "Active operation", "none", "", nil)
	}

	if pending := status.PendingObservation; pending != nil {
		statusview.WriteRecord(text, "Pending proof", "the reading that proves an Apply is owed",
			pendingProofLead(*pending, view.Suspended), pending)
	} else {
		statusview.WriteRecord(text, "Pending proof", "none", "", nil)
	}
	if release := status.PendingLockRelease; release != nil {
		statusview.WriteRecord(text, "Lock release", "owed",
			"The operation ended and the database Lease it held is still to be handed back. This record keeps "+
				"the Lease's owner and epoch until a release succeeds.", release)
	}
	if retirement := status.PendingBindingRetirement; retirement != nil {
		statusview.WriteRecord(text, "Binding retirement", "owed",
			"An execution-binding change replaced this epoch, and the cleanup it owes that epoch is not finished. "+
				"Another change waits until it is.", retirement)
	}
	if binding := status.ExecutionBinding; binding != nil {
		statusview.WriteRecord(text, "Execution binding", statusview.Clean(binding.Epoch), "", binding)
	}
	statusview.WriteRecord(text, "Source", "", "", status.Source)
	statusview.WriteRecord(text, "Target", "", "", status.Target)
	if plan := status.Plan; plan != nil {
		statusview.WriteRecord(text, "Plan", statusview.Clean(plan.Name), "", plan)
	}
	if applied := status.Applied; applied != nil {
		statusview.WriteRecord(text, "Applied", statusview.Clean(applied.PlanRef.Name),
			"The last Apply a reading of the database showed converged, which is a different claim from a Job "+
				"that exited zero.", applied)
	}
}

// pendingProofLead keeps an Apply's own result apart from the reading that
// proves it: an Apply that reported success has still not been verified.
func pendingProofLead(pending operatorv1alpha1.PendingObservationStatus, suspended bool) string {
	var lead string
	switch pending.Outcome {
	case operatorv1alpha1.PendingObservationApplySucceeded:
		lead = "The Apply reported success. That is the Apply's own result, not the verdict: " +
			"the verdict is the reading of the database after it, which has not settled yet."
	case operatorv1alpha1.PendingObservationOutcomeUnknown:
		lead = "Nobody can say how the Apply ended. The reading of the database after it is what settles it, " +
			"and it has not settled yet."
	default:
		lead = "The reading of the database after the Apply has not settled yet."
	}
	if after := statusview.After(pending.ObserveAfter); after != "" {
		lead += " The database is not read for it before " + after +
			", while the Apply's Pod could still be within its deadline."
	}
	if suspended {
		lead += " Suspension does not end it: nothing else changes the database until it settles."
	}
	return lead
}
