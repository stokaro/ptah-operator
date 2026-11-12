package historyview

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Format is how a reader asked to see a history.
type Format string

const (
	// Text is the default: what the journal covers, its entries in order, and
	// the decisions that name the resource.
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

// family is how kubectl ptah spells the resource's kind on a command line.
func (view View) family() string {
	if view.Kind == Migration {
		return "migration"
	}
	return "schema"
}

// renderText says which resource this is and what period the journal covers,
// then every entry in the order the operator recorded it, then every decision
// that names the resource.
func renderText(out io.Writer, view View) error {
	var text strings.Builder
	field := statusview.FieldWriter(&text, "", statusview.LabelWidth)
	field("History of", fmt.Sprintf("%s/%s in namespace %s", strings.ToLower(string(view.Kind)), view.Name, view.Namespace))
	field("UID", view.UID)
	field("Created", statusview.Time(view.CreatedAt))
	renderCoverage(&text, field, view)
	if journal := view.Journal; journal != nil && len(journal.Entries) > 0 {
		text.WriteString("\n")
		for _, entry := range journal.Entries {
			renderEntry(&text, view, entry)
		}
	}
	renderDecisions(&text, view)
	text.WriteString("\n")
	fmt.Fprintf(&text, "kubectl ptah %s %s -n %s prints where it stands now.\n", view.family(), view.Name, view.Namespace)
	fmt.Fprintf(&text, "kubectl events -n %s --for %s/%s prints its Events, which Kubernetes keeps for about an hour.\n",
		view.Namespace, strings.ToLower(string(view.Kind)), view.Name)
	_, err := io.WriteString(out, text.String())
	return err
}

// renderCoverage says what the journal is and which period it covers, and
// tells a missing journal from an empty one: the first is a status no manager
// that keeps a journal has written, the second a period in which nothing the
// journal records happened.
func renderCoverage(text *strings.Builder, field func(name, value string), view View) {
	journal := view.Journal
	if journal == nil {
		field("Journal", "none")
		text.WriteString("\n")
		statusview.WriteWrapped(text, "", 80, "The status holds no journal. The manager that last wrote it keeps none, "+
			"as a release before 0.2.7 did, or no manager has written this resource's status yet. "+
			"What happened to it is not retained here; its decisions follow, if any are stored.")
		return
	}
	entries := "no entries"
	switch count := len(journal.Entries); count {
	case 0:
	case 1:
		entries = "1 entry"
	default:
		entries = fmt.Sprintf("%d entries", count)
	}
	field("Journal", entries+", complete since "+statusview.Time(journal.CoveredSince))
	text.WriteString("\n")
	paragraph := "The journal is the operator's own bounded record in status.journal, in the order it recorded " +
		"the entries, oldest first. It keeps at most 64 entries, none more than 30 days older than the newest, " +
		"and is not an audit archive. Nothing before " + statusview.Time(journal.CoveredSince) + " is recorded " +
		"here, apart from what the bounds keep of a plan nothing has carried out yet. Each entry is dated by the " +
		"status it was derived from, so a time can be earlier than the entry above it."
	if view.Kind == Migration {
		paragraph += " It is not the database's revision table, which kubectl ptah migration " + view.Name +
			" summarizes as its history read."
	}
	statusview.WriteWrapped(text, "", 80, paragraph)
	if view.PlansUnreadable != nil {
		resource := "ptahschemaplans"
		if view.Kind == Migration {
			resource = "ptahmigrationplans"
		}
		statusview.WriteWrapped(text, "", 80, "Whether each plan is still stored is not shown: "+unreadableText(resource, view.PlansUnreadable))
	}
}

// renderEntry writes one entry: when, what, and how it ended on one line,
// then every field it recorded, each where a reader expects it.
func renderEntry(text *strings.Builder, view View, entry Entry) {
	heading := statusview.Time(entry.At) + "  " + statusview.Clean(string(entry.Type))
	if summary := summaryOf(entry); summary != "" {
		heading += ", " + summary
	}
	if entry.BeforeCoverage {
		heading += " (dated before the journal's coverage begins)"
	}
	text.WriteString(heading + "\n")
	field := statusview.FieldWriter(text, "  ", statusview.LabelWidth-len("  "))
	if entry.Count > 0 {
		attempts := fmt.Sprintf("%d in a row", entry.Count)
		if entry.LastAt != nil {
			attempts += ", the last at " + statusview.Time(*entry.LastAt)
		}
		field("Attempts", attempts)
	}
	if entry.StartedAt != nil {
		started := statusview.Time(*entry.StartedAt)
		if took := entry.At.Sub(entry.StartedAt.Time); took >= 0 {
			started += ", " + statusview.Duration(took) + " before it ended"
		}
		field("Started", started)
	}
	if view.Kind == Migration && concernsARun(entry.Type) {
		operation := entry.OperationID
		if operation == "" {
			operation = "not recorded; an entry a release before 0.2.10 wrote names no Apply claim, " +
				"so it is not joined to a run"
		}
		field("Operation", statusview.Clean(operation))
	}
	field("Plan", planText(entry))
	if entry.Type == operatorv1alpha1.JournalPlanPublished && view.Kind == Schema {
		field("Statements", statementsText(entry))
	}
	if entry.Replaced != "" {
		field("Replaced", statusview.Clean(entry.Replaced)+", a plan a person could approve or had approved, which never ran")
	}
	switch entry.Authorization {
	case "":
	case operatorv1alpha1.MigrationRunAuthorizedByPolicy:
		field("Authorized", "spec.policy.apply Always, with no approval")
	case operatorv1alpha1.MigrationRunAuthorizedByApproval:
		field("Authorized", "an approval")
	default:
		field("Authorized", statusview.Clean(string(entry.Authorization)))
	}
	field("Approval", approvalText(entry))
	field("Approver", statusview.Clean(entry.Approver))
	field("Job", jobText(entry))
	if entry.AppliedVersions > 0 {
		field("Versions applied", fmt.Sprintf("%d", entry.AppliedVersions))
	}
	if failure := entry.Failure; failure != nil {
		field("Stopped at", failureText(*failure))
	}
	field("Acknowledgment", acknowledgmentText(entry))
	field("Acknowledged by", statusview.Clean(entry.AcknowledgedBy))
	if entry.Type == operatorv1alpha1.JournalRunAcknowledged {
		field("Settled", "by a person's statement that the run is accounted for, not by a reading of the database")
	}
}

// concernsARun reports the entry types a migration joins to one execution by
// the Apply claim's operation ID.
func concernsARun(entryType operatorv1alpha1.JournalEntryType) bool {
	switch entryType {
	case operatorv1alpha1.JournalRunFinished, operatorv1alpha1.JournalVerified, operatorv1alpha1.JournalRunAcknowledged:
		return true
	default:
		return false
	}
}

// summaryOf is what follows the type on an entry's first line: how it ended,
// or which operation failed and why. outcome means something different on
// each type, and the type stays beside it. The operator writes an operation
// and a reason only on OperationFailed, but whatever the entry holds is
// printed, so a value is never in the status and missing from the text.
func summaryOf(entry Entry) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{string(entry.Outcome), entry.Operation, entry.Reason} {
		if part != "" {
			parts = append(parts, statusview.Clean(part))
		}
	}
	return strings.Join(parts, ", ")
}

func planText(entry Entry) string {
	if entry.Plan == "" {
		return ""
	}
	text := statusview.Clean(entry.Plan)
	if entry.PlanUID != "" {
		text += ", UID " + statusview.Clean(string(entry.PlanUID))
	}
	if entry.PlanFingerprint != "" {
		text += ", fingerprint " + statusview.Clean(entry.PlanFingerprint)
	}
	return text + presence(entry.PlanStored)
}

func statementsText(entry Entry) string {
	text := fmt.Sprintf("%d", entry.Statements)
	if entry.Destructive {
		text += ", destructive"
	}
	if entry.ApprovalRequired {
		text += ", asked for an approval when it was published"
	}
	return text
}

func approvalText(entry Entry) string {
	if entry.Approval == "" && entry.ApprovalUID == "" {
		return ""
	}
	text := statusview.Clean(entry.Approval)
	if entry.ApprovalUID == "" {
		return text + ", UID not recorded: a release before 0.2.10 wrote this entry, " +
			"so it is not joined to an approval that holds the name today"
	}
	return text + ", UID " + statusview.Clean(string(entry.ApprovalUID)) + presence(entry.ApprovalStored)
}

// jobText names the Job a migration entry recorded. A run entry an operator of
// 0.2.10 or later wrote that names none ran as no Job the API server
// confirmed; an older one names none in any case, and says nothing either way.
func jobText(entry Entry) string {
	switch {
	case entry.JobName != "" && entry.JobUID != "":
		return statusview.Clean(entry.JobName) + ", UID " + statusview.Clean(string(entry.JobUID))
	case entry.JobName != "":
		return statusview.Clean(entry.JobName)
	case entry.JobUID != "":
		return "UID " + statusview.Clean(string(entry.JobUID))
	case entry.Type == operatorv1alpha1.JournalRunFinished && entry.OperationID != "":
		return "none confirmed: the API server never confirmed one was created"
	default:
		return ""
	}
}

func failureText(failure operatorv1alpha1.MigrationRunFailureStatus) string {
	stopped := fmt.Sprintf("version %d, statement %d of %d", failure.Version, failure.Statement, failure.Statements)
	if failure.SQLState != "" {
		stopped += ", SQLSTATE " + statusview.Clean(failure.SQLState)
	}
	if failure.ErrorNumber != 0 {
		stopped += fmt.Sprintf(", error %d", failure.ErrorNumber)
	}
	return stopped
}

func acknowledgmentText(entry Entry) string {
	if entry.Acknowledgment == "" {
		return ""
	}
	return statusview.Clean(entry.Acknowledgment) + presence(entry.AcknowledgmentStored)
}

// presence says whether an object an entry names is still stored, and says
// nothing where that is not known.
func presence(stored *bool) string {
	switch {
	case stored == nil:
		return ""
	case *stored:
		return " (stored)"
	default:
		return " (no longer stored)"
	}
}

// renderDecisions writes every approval, and for a migration every run
// acknowledgment, that names this resource, oldest first.
func renderDecisions(text *strings.Builder, view View) {
	approvals := "ptahschemaapprovals"
	if view.Kind == Migration {
		approvals = "ptahmigrationapprovals"
	}
	if statusview.WriteEveryHeading(text, "Approvals", approvals, view.Approvals, view.ApprovalsUnreadable) {
		for _, approval := range view.Approvals.Items {
			fmt.Fprintf(text, "  %s, UID %s\n", statusview.Clean(approval.Name), statusview.Clean(approval.UID))
			fmt.Fprintf(text, "    approved by %s at %s\n", identity(approval.Approver.Username), statusview.Time(approval.ApprovedAt))
			fmt.Fprintf(text, "    plan %s, UID %s\n", statusview.Clean(approval.PlanRef.Name), statusview.Clean(string(approval.PlanRef.UID)))
			statusview.WriteDecisionConditions(text, approval.Conditions, "no condition written")
		}
	}
	if view.Kind != Migration {
		return
	}
	if statusview.WriteEveryHeading(text, "Run acknowledgments", "ptahmigrationrunacknowledgments",
		view.Acknowledgments, view.AcknowledgmentsUnreadable) {
		for _, acknowledgment := range view.Acknowledgments.Items {
			fmt.Fprintf(text, "  %s, UID %s\n", statusview.Clean(acknowledgment.Name), statusview.Clean(acknowledgment.UID))
			fmt.Fprintf(text, "    acknowledged by %s at %s\n", identity(acknowledgment.AcknowledgedBy.Username),
				statusview.Time(acknowledgment.AcknowledgedAt))
			fmt.Fprintf(text, "    operation %s\n", statusview.Clean(acknowledgment.OperationID))
			text.WriteString("    It states that the run is accounted for. It is not proof that the data is correct.\n")
			statusview.WriteDecisionConditions(text, acknowledgment.Conditions, "no condition written")
		}
	}
}

func identity(username string) string {
	if username == "" {
		return "(no identity stamped)"
	}
	return statusview.Clean(username)
}

func unreadableText(resource string, unreadable *statusview.Unreadable) string {
	if unreadable.Forbidden {
		return "listing " + resource + " is forbidden to you (RBAC): " + statusview.Clean(unreadable.Message)
	}
	return "listing " + resource + " failed: " + statusview.Clean(unreadable.Message)
}
