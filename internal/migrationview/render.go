package migrationview

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Format is how a reader asked to see a migration.
type Format string

const (
	// Text is the default: the state, the history, the sequence, the last run.
	Text Format = "text"
	// JSON is the same view as a document, for a script.
	JSON Format = "json"
)

// Formats are the accepted values, in the order the help lists them.
var Formats = []Format{Text, JSON}

// Render writes one view in the requested format.
func Render(out io.Writer, view View, format Format) error {
	switch format {
	case Text:
		return renderText(out, view)
	case JSON:
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(view)
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

// renderText says where the migration stands and what holds it before it
// says what is pending, then what the last run did and which run nobody has
// accounted for, then who decided what, and last every condition the status
// carries.
//
// No SQL appears here, and no row of any table: a migration plan records
// versions and checksums, and the statements live in the artifact.
func renderText(out io.Writer, view View) error {
	var text strings.Builder
	field := statusview.FieldWriter(&text, "", statusview.LabelWidth)
	field("Migration", view.Namespace+"/"+view.Migration)
	field("Phase", view.Phase)
	if view.ObservedGeneration != 0 && view.ObservedGeneration != view.Generation {
		field("Status for", fmt.Sprintf("generation %d; the spec is at generation %d",
			view.ObservedGeneration, view.Generation))
	}
	for _, condition := range statusview.Leading(view.Conditions) {
		field(statusview.Clean(condition.Type), statusview.Summary(condition))
	}
	if view.Suspended {
		field("Suspend", "requested")
	}
	if view.Policy != nil && view.Policy.Apply != "" {
		field("Policy", "apply "+view.Policy.Apply)
	}
	for _, line := range view.Artifact.Fields() {
		field(line.Name, line.Value)
	}
	if history := view.History; history != nil {
		field("History read", statusview.Time(history.ObservedAt))
		field("Current version", fmt.Sprintf("%d", history.CurrentVersion))
		if history.CheckpointVersion > 0 {
			field("Checkpoint", fmt.Sprintf("%d", history.CheckpointVersion))
		}
		field("Applied", fmt.Sprintf("%d", history.AppliedCount))
		field("Pending", fmt.Sprintf("%d", history.PendingCount))
		if history.Dirty {
			field("Dirty", "true")
		}
		if len(history.ModifiedVersions) > 0 {
			field("Modified", joinVersions(history.ModifiedVersions))
		}
		if len(history.OutOfOrderVersions) > 0 {
			field("Out of order", joinVersions(history.OutOfOrderVersions))
		}
	}
	renderLastRun(&text, view.LastRun)
	renderUnresolvedRun(&text, view.UID, readingGate(view), view.UnresolvedRun, view.LastRun)
	renderResolvedRun(&text, view.ResolvedRun)
	renderPlan(&text, view.Plan, statusview.Find(view.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired))
	if statusview.WriteListHeading(&text, "Approvals", "ptahmigrationapprovals", view.Namespace,
		view.Approvals, view.ApprovalsUnreadable) {
		for _, approval := range view.Approvals.Items {
			statusview.WriteApproval(&text, approval, "no condition written")
		}
	}
	if statusview.WriteListHeading(&text, "Run acknowledgments", "ptahmigrationrunacknowledgments", view.Namespace,
		view.Acknowledgments, view.AcknowledgmentsUnreadable) {
		for _, acknowledgment := range view.Acknowledgments.Items {
			renderAcknowledgment(&text, acknowledgment)
		}
	}
	if len(view.Conditions) > 0 {
		text.WriteString("\nConditions:\n")
		statusview.WriteTable(&text, "  ", view.Conditions, view.Generation)
	}
	fmt.Fprintf(&text, "\nkubectl events -n %s --for ptahmigration/%s prints its Events.\n", view.Namespace, view.Migration)
	_, err := io.WriteString(out, text.String())
	return err
}

// subField writes the indented "name: value" lines of one block, with the
// values in the same column as the lines above the block.
func subField(text *strings.Builder) func(name, value string) {
	return statusview.FieldWriter(text, "  ", statusview.LabelWidth-len("  "))
}

// renderLastRun writes the most recent execution: what the database accounted
// for, and which Job it was, so a later run under the same Job name is not
// mistaken for this one.
func renderLastRun(text *strings.Builder, run *RunView) {
	if run == nil {
		return
	}
	text.WriteString("\n")
	when := "started " + statusview.Time(run.StartedAt)
	if run.FinishedAt != nil {
		when += ", finished " + statusview.Time(*run.FinishedAt)
	}
	statusview.FieldWriter(text, "", statusview.LabelWidth)("Last run", run.Outcome+", "+when)
	field := subField(text)
	field("Job", jobText(run.JobName, run.JobUID))
	if run.OperationID != "" {
		field("Operation", run.OperationID)
	}
	if plan := run.PlanRef; plan != nil {
		named := plan.Name
		if run.PlanFingerprint != "" {
			named += ", fingerprint " + statusview.Clean(run.PlanFingerprint)
		}
		field("Plan", named)
	}
	if authorized := authorizationText(run.Authorization); authorized != "" {
		field("Authorized", authorized)
	}
	if verification := run.Verification; verification != nil {
		field("Read back", fmt.Sprintf("%s at version %d, %s", verification.Result, verification.CurrentVersion,
			statusview.Time(verification.ObservedAt)))
	}
	// Not "Applied": the history above counts applied migrations under that
	// label, and these are the versions this run applied.
	if len(run.AppliedVersions) > 0 {
		field("Versions", joinVersions(run.AppliedVersions))
	}
	if failure := run.Failure; failure != nil {
		stopped := fmt.Sprintf("version %d, statement %d of %d", failure.Version, failure.Statement, failure.Statements)
		if failure.SQLState != "" {
			stopped += ", SQLSTATE " + failure.SQLState
		}
		if failure.ErrorNumber != 0 {
			stopped += fmt.Sprintf(", error %d", failure.ErrorNumber)
		}
		field("Stopped at", stopped)
	}
	field("Message", statusview.Clean(run.Message))
}

// authorizationText says what allowed a run, and leaves out what its record does
// not hold rather than reading it from an approval that holds the name today.
func authorizationText(authorization *operatorv1alpha1.MigrationRunAuthorizationStatus) string {
	switch {
	case authorization == nil:
		return ""
	case authorization.Kind == operatorv1alpha1.MigrationRunAuthorizedByPolicy:
		return "spec.policy.apply Always, with no approval"
	case authorization.ApprovalRef == nil:
		return ""
	}
	text := "approval " + authorization.ApprovalRef.Name + " (UID " + string(authorization.ApprovalRef.UID) + ")"
	if authorization.Approver != nil {
		text += " by " + statusview.Clean(authorization.Approver.Username)
	}
	if authorization.ApprovedAt != nil {
		text += " at " + statusview.Time(*authorization.ApprovedAt)
	}
	return text
}

// renderUnresolvedRun writes the run nobody has accounted for, and what the
// operations guide says about it. It is a separate record from the last run on
// purpose: the last run is a history of outcomes, and this is a hold that
// stands until a reading or a person settles it.
//
// gate is what stops the resource before the history read that would clear
// the record, or empty when nothing the status shows does. last is the last run,
// which says whether the hold's database was named or is its plan's.
func renderUnresolvedRun(text *strings.Builder, migrationUID, gate string, run *UnresolvedRunView, last *RunView) {
	if run == nil {
		return
	}
	text.WriteString("\n")
	statusview.FieldWriter(text, "", statusview.LabelWidth)("Unresolved run",
		run.Outcome+", recorded "+statusview.Time(run.RecordedAt))
	field := subField(text)
	field("Operation", run.OperationID)
	job := jobText(run.JobName, run.JobUID)
	if job == "" {
		job = "none recorded: the API server never confirmed its create"
	}
	field("Job", job)
	field("Plan", jobText(run.PlanRef.Name, string(run.PlanRef.UID)))
	if run.TargetIdentityDigest != "" {
		field("Database", holdDatabaseText(run, last))
	}
	uid := migrationUID
	if uid == "" {
		uid = "<metadata.uid>"
	}
	guidance := []string{
		"While this record stands no plan is published and no Apply is dispatched; " +
			"resolve, verify and the history read go on.",
		"The database is the one the run named or, where it named none, " +
			"the one its plan was computed against. The runner starts nothing against a database other than " +
			"its plan's, so the run changed this one or none. A reader who inspects another database, such as " +
			"a replica or a restored copy, finds nothing wrong and settles a record that was telling the truth.",
		"Establish what the run did there first. A history read of that database that finds nothing " +
			"of the artifact left to apply clears the record by itself. If the database is accounted " +
			"for and still has work pending, settle it with a PtahMigrationRunAcknowledgment naming " +
			"migration UID " + uid + " and the operation above.",
	}
	if gate != "" {
		guidance = append(guidance, "This resource is held before any reading: "+gate+
			". No history read runs, so the record cannot clear by reading; an acknowledgment still settles it.")
	}
	guidance = append(guidance, `The operations guide has the procedure, under "A migration run nobody accounted for".`)
	for _, paragraph := range guidance {
		writeWrapped(text, "  ", 78, paragraph)
	}
}

// holdDatabaseText says which database a hold names, and that it is its plan's when
// the record of that same run names none. A hold beside no record of its run, or one
// a release before that field recorded, does not say which it is, so neither does this.
func holdDatabaseText(run *UnresolvedRunView, last *RunView) string {
	if last != nil && last.OperationID == run.OperationID && last.TargetIdentityDigest == "" {
		return run.TargetIdentityDigest + " (target identity digest of its plan: the run named none, and the runner starts nothing against another)"
	}
	return run.TargetIdentityDigest + " (target identity digest)"
}

// readingGate names what holds the resource before the history read that
// would clear an unresolved run, as the operations guide lists the gates and
// as the status shows them: the suspension the spec asks for, and an engine, a
// realm or an artifact the controller refused, which it reports as Blocked with
// the refusal's reason. An artifact is refused after Resolve and Verify have run,
// and the history read is what never starts. The gate of stored state a newer
// manager wrote is refused before any status is written, so nothing here can
// show it.
func readingGate(view View) string {
	if view.Suspended {
		return "spec.suspend is set"
	}
	blocked := statusview.Find(view.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || blocked.Status != string(metav1.ConditionTrue) {
		return ""
	}
	switch operatorv1alpha1.ConditionReason(blocked.Reason) {
	case operatorv1alpha1.ReasonUnsupportedEngine, operatorv1alpha1.ReasonRealmConflict,
		operatorv1alpha1.ReasonRealmNotAuthorized, operatorv1alpha1.ReasonPolicyRefused:
		return "Blocked is True, " + blocked.Reason
	default:
		return ""
	}
}

// writeWrapped writes one paragraph in lines of at most width columns, each
// after indent. A word longer than a line, such as a digest, takes a line of
// its own.
func writeWrapped(text *strings.Builder, indent string, width int, paragraph string) {
	line := indent
	for _, word := range strings.Fields(paragraph) {
		if line != indent && len(line)+1+len(word) > width {
			text.WriteString(line + "\n")
			line = indent
		}
		if line != indent {
			line += " "
		}
		line += word
	}
	if line != indent {
		text.WriteString(line + "\n")
	}
}

// renderResolvedRun writes how the most recent unresolved run was settled.
func renderResolvedRun(text *strings.Builder, run *ResolvedRunView) {
	if run == nil {
		return
	}
	how := "by a history read"
	if run.Resolution == string(operatorv1alpha1.MigrationRunResolvedByAcknowledgment) {
		how = "by acknowledgment " + run.AcknowledgmentName
		if run.AcknowledgedBy != "" {
			how += " from " + statusview.Clean(run.AcknowledgedBy)
		}
	}
	text.WriteString("\n")
	statusview.FieldWriter(text, "", statusview.LabelWidth)("Settled run",
		run.Outcome+" run "+run.OperationID+", "+how+" at "+statusview.Time(run.ResolvedAt))
}

// renderPlan writes the sequence the operator would run next and the
// controller's verdict on it: ApprovalRequired says whether it waits for an
// approval and what to do about it.
func renderPlan(text *strings.Builder, plan *PlanView, approvalRequired *ConditionView) {
	if plan == nil {
		text.WriteString("\nNo plan is published.\n")
		return
	}
	fmt.Fprintf(text, "\nPlan %s, %s from version %d:\n",
		plan.Name, countedMigrations(len(plan.Migrations)), plan.CurrentVersion)
	for _, planned := range plan.Migrations {
		description := planned.Description
		if description == "" {
			description = "(no description)"
		}
		marks := ""
		if planned.Checkpoint {
			marks += " [checkpoint]"
		}
		if planned.TransactionMode != "" {
			marks += " [tx " + planned.TransactionMode + "]"
		}
		fmt.Fprintf(text, "  %-20d %s%s\n", planned.Version, statusview.Clean(description), marks)
	}
	if approvalRequired != nil {
		fmt.Fprintf(text, "  ApprovalRequired: %s\n", statusview.Summary(*approvalRequired))
	}
}

// renderAcknowledgment writes one acknowledgment: who said which run was
// accounted for, and whether the controller took it.
func renderAcknowledgment(text *strings.Builder, acknowledgment AcknowledgmentView) {
	fmt.Fprintf(text, "  %s\n", statusview.Clean(acknowledgment.Name))
	who := statusview.Clean(acknowledgment.AcknowledgedBy.Username)
	if who == "" {
		who = "(no identity stamped)"
	}
	fmt.Fprintf(text, "    acknowledged by %s at %s\n", who, statusview.Time(acknowledgment.AcknowledgedAt))
	operation := "operation " + acknowledgment.OperationID
	if acknowledgment.NamesUnresolvedRun {
		operation += ", the unresolved run"
	}
	fmt.Fprintf(text, "    %s\n", operation)
	// The controller answers an acknowledgment on its next pass, Consumed or
	// Stale, so one with no condition has not been read yet.
	statusview.WriteDecisionConditions(text, acknowledgment.Conditions, "no condition written yet")
}

// jobText names an object and its UID, or returns nothing for neither.
func jobText(name, uid string) string {
	switch {
	case name == "" && uid == "":
		return ""
	case uid == "":
		return name
	case name == "":
		return "UID " + uid
	default:
		return name + ", UID " + uid
	}
}

func countedMigrations(count int) string {
	if count == 1 {
		return "1 migration"
	}
	return fmt.Sprintf("%d migrations", count)
}

func joinVersions(versions []int64) string {
	parts := make([]string, 0, len(versions))
	for _, version := range versions {
		parts = append(parts, fmt.Sprintf("%d", version))
	}
	return strings.Join(parts, ", ")
}
