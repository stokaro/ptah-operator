package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

// viewInvokerReading reads a plan the security_invoker row's filters are held
// to. The three view-invoker-*-plan.json readings are what Ptah v0.26.0 planned
// for the row's own artifacts, in order, against one PostgreSQL 17 database;
// the differing/plan-view-invoker-*.json captures are the runner harness's.
func viewInvokerReading(t *testing.T, path ...string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func readingPath(name string) []string {
	return []string{"..", "..", "testdata", "e2e", "readings", name}
}

func harnessViewPlan(name string) []string {
	return []string{"..", "..", "internal", "dataplane", "testdata", "differing", name}
}

// editedViewPlan is the reading with one edit made to its decoded document, for
// the mistakes a plan could make that no capture holds.
func editedViewPlan(t *testing.T, body []byte, edit func(statements []any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	statements, _ := document["statements"].([]any)
	edit(statements)
	edited, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

func firstEffect(statements []any) map[string]any {
	statement, _ := statements[0].(map[string]any)
	effects, _ := statement["effects"].([]any)
	effect, _ := effects[0].(map[string]any)
	return effect
}

func invokerProperty(statements []any) map[string]any {
	properties, _ := firstEffect(statements)["properties"].([]any)
	for _, raw := range properties {
		if property, _ := raw.(map[string]any); property["name"] == "security_invoker" {
			return property
		}
	}
	return nil
}

func TestViewInvokerPlannedReadsEachStepOfTheRow(t *testing.T) {
	t.Parallel()

	created := viewInvokerReading(t, readingPath("view-invoker-created-plan.json")...)
	turnedOn := viewInvokerReading(t, readingPath("view-invoker-turned-on-plan.json")...)
	kept := viewInvokerReading(t, readingPath("view-invoker-body-kept-plan.json")...)
	steps := []struct {
		name     string
		document []byte
		want     viewInvokerChange
	}{
		{"created", created, viewCreatedWithOwnerRights},
		{"turned on", turnedOn, viewInvokerTurnedOn},
		{"body kept", kept, viewBodyKeptInvoker},
	}
	for _, step := range steps {
		if err := viewInvokerPlanned(step.document, step.want); err != nil {
			t.Errorf("%s: the row's own plan was refused: %v", step.name, err)
		}
		// Each step's plan is no other step's.
		for _, other := range steps {
			if other.name != step.name {
				if err := viewInvokerPlanned(step.document, other.want); err == nil {
					t.Errorf("the %s plan passed as %s", step.name, other.want.describeChange)
				}
			}
		}
	}

	for _, test := range []struct {
		name     string
		document []byte
		want     viewInvokerChange
		says     string
	}{
		// The harness's plans are the same shapes over another view, and a view
		// the option moved on in the same statement as its body.
		{"another view", viewInvokerReading(t, harnessViewPlan("plan-view-invoker-kept.json")...), viewBodyKeptInvoker, "opens"},
		{"a body change that turns the option on", editedViewPlan(t, kept, func(statements []any) {
			invokerProperty(statements)["before"] = false
			invokerProperty(statements)["changed"] = true
		}), viewBodyKeptInvoker, "security_invoker false to true"},
		{"a new view with its readers' rights", editedViewPlan(t, created, func(statements []any) {
			invokerProperty(statements)["after"] = true
		}), viewCreatedWithOwnerRights, "security_invoker absent to true"},
		{"a view created that names the option", editedViewPlan(t, created, func(statements []any) {
			statement, _ := statements[0].(map[string]any)
			statement["sql"] = `CREATE VIEW "e2e_widget_names" AS SELECT 1 -- security_invoker=false`
		}), viewCreatedWithOwnerRights, "names security_invoker"},
		{"a replace that does not restate the option", editedViewPlan(t, kept, func(statements []any) {
			statement, _ := statements[0].(map[string]any)
			statement["sql"] = strings.Replace(statement["sql"].(string), "WITH (security_invoker=true) ", "", 1)
		}), viewBodyKeptInvoker, "opens"},
		{"an effect on another view", editedViewPlan(t, kept, func(statements []any) {
			object, _ := firstEffect(statements)["object"].(map[string]any)
			object["name"] = "e2e_widget_names_old"
		}), viewBodyKeptInvoker, "want version 1, view public.e2e_widget_names"},
		{"turned off", viewInvokerReading(t, harnessViewPlan("plan-view-invoker-off.json")...), viewInvokerTurnedOn, "opens"},
		{"no effect", editedViewPlan(t, kept, func(statements []any) {
			statement, _ := statements[0].(map[string]any)
			delete(statement, "effects")
		}), viewBodyKeptInvoker, "0 effects"},
		{"an effect at another version", editedViewPlan(t, kept, func(statements []any) {
			firstEffect(statements)["version"] = 2
		}), viewBodyKeptInvoker, "version 2"},
		{"an effect on another action", editedViewPlan(t, kept, func(statements []any) {
			firstEffect(statements)["action"] = "alter"
		}), viewBodyKeptInvoker, `action "alter"`},
		{"changed reported on a value that stayed", editedViewPlan(t, kept, func(statements []any) {
			invokerProperty(statements)["changed"] = true
		}), viewBodyKeptInvoker, "changed true"},
		{"the option recorded twice", editedViewPlan(t, kept, func(statements []any) {
			effect := firstEffect(statements)
			properties, _ := effect["properties"].([]any)
			effect["properties"] = append(properties, invokerProperty(statements))
		}), viewBodyKeptInvoker, "2 times"},
		{"the option not recorded", editedViewPlan(t, kept, func(statements []any) {
			invokerProperty(statements)["name"] = "security_barrier"
		}), viewBodyKeptInvoker, "0 times"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := viewInvokerPlanned(test.document, test.want)
			if err == nil || !strings.Contains(err.Error(), test.says) {
				t.Fatalf("viewInvokerPlanned() = %v, want an error naming %q", err, test.says)
			}
		})
	}

	var two map[string]any
	if err := json.Unmarshal(kept, &two); err != nil {
		t.Fatal(err)
	}
	statements, _ := two["statements"].([]any)
	two["statements"] = append(statements, statements[0])
	body, err := json.Marshal(two)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewInvokerPlanned(body, viewBodyKeptInvoker); err == nil || !strings.Contains(err.Error(), "2 statements") {
		t.Fatalf("viewInvokerPlanned(two statements) = %v, want the second refused", err)
	}
}

// operatorJournalOfAPlanApplied is the journal the operator's own code keeps
// across the status writes of one plan's life: its publication, with the
// ApprovalRequired condition the same write set, the Apply claim, the proof
// that Apply owes, and the proof settling in sync. approval is the approval
// the Apply consumed, nil for none.
func operatorJournalOfAPlanApplied(required bool, approval *ptahv1alpha1.ConsumedApprovalStatus) *ptahv1alpha1.LifecycleJournal {
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	claim := "sha256:" + strings.Repeat("e", 64)
	plan := ptahv1alpha1.CurrentPlanStatus{
		Name: "ptah-plan-view", UID: "plan-uid", Fingerprint: "sha256:" + strings.Repeat("f", 64),
		CreatedAt: metav1.NewTime(start), Approval: approval,
	}
	planned := &ptahv1alpha1.PtahSchemaStatus{Plan: plan.DeepCopy(), Phase: ptahv1alpha1.PhaseReadyToApply}
	condition := metav1.Condition{Type: ptahv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse, Reason: "NotRequired"}
	if required {
		condition = metav1.Condition{Type: ptahv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue, Reason: "PrivilegeChanges"}
		planned.Phase = ptahv1alpha1.PhaseAwaitingApproval
	}
	meta.SetStatusCondition(&planned.Conditions, condition)
	applying := planned.DeepCopy()
	applying.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{
		Type: ptahv1alpha1.OperationApply, ID: claim, JobName: "ptah-apply-view-1", JobUID: "job-uid",
		StartedAt: metav1.NewTime(start.Add(time.Minute)),
	}
	owed := &ptahv1alpha1.PtahSchemaStatus{PendingObservation: &ptahv1alpha1.PendingObservationStatus{
		Outcome: ptahv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: claim,
		ApplyJobName: "ptah-apply-view-1", ApplyJobUID: "job-uid", Plan: plan,
	}}
	settled := &ptahv1alpha1.PtahSchemaStatus{Phase: ptahv1alpha1.PhaseInSync}
	var journal *ptahv1alpha1.LifecycleJournal
	for index, write := range [][2]*ptahv1alpha1.PtahSchemaStatus{
		{{}, planned}, {planned, applying}, {applying, owed}, {owed, settled},
	} {
		now := start.Add(time.Duration(index+1) * time.Minute)
		journal, _ = lifecyclejournal.Record(journal, lifecyclejournal.SchemaEntries(write[0], write[1], journal, now), now)
	}
	return journal
}

func TestUnattendedApplyJournaledReadsAPlanNobodyApproved(t *testing.T) {
	t.Parallel()

	const plan = "ptah-plan-view"
	unattended := operatorJournalOfAPlanApplied(false, nil)
	if count := len(journaled(unattended, ptahv1alpha1.JournalPlanPublished)); count != 1 {
		t.Fatalf("the operator's journal holds %d PlanPublished entries, want the one this reads", count)
	}
	if err := unattendedApplyJournaled(unattended, plan); err != nil {
		t.Fatalf("the life of a plan applied with nobody's approval was refused: %v", err)
	}

	approved := operatorJournalOfAPlanApplied(true, &ptahv1alpha1.ConsumedApprovalStatus{
		Name: "e2e-postgresql-external-view-invoker", UID: "approval-uid",
		Approver: ptahv1alpha1.ApprovalIdentity{Username: "kubernetes-admin"},
	})
	edited := func(edit func([]ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry) *ptahv1alpha1.LifecycleJournal {
		journal := unattended.DeepCopy()
		journal.Entries = edit(journal.Entries)
		return journal
	}
	at := func(entries []ptahv1alpha1.JournalEntry, kind ptahv1alpha1.JournalEntryType) int {
		for index, entry := range entries {
			if entry.Type == kind {
				return index
			}
		}
		t.Fatalf("the operator's journal holds no %s entry", kind)
		return -1
	}
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		plan    string
		want    string
	}{
		{"no journal", nil, plan, "keeps no journal"},
		{"a plan held for a person and approved", approved, plan, "requiring an approval"},
		{"another plan's life", unattended, "ptah-plan-other", "never publishes"},
		{"an approval of the plan", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			return append(entries[:1:1], append([]ptahv1alpha1.JournalEntry{{
				Type: ptahv1alpha1.JournalApprovalAccepted, Plan: plan, Approval: "late", At: entries[0].At,
			}}, entries[1:]...)...)
		}), plan, `accepts approval "late"`},
		{"an Apply under an approval", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			entries[at(entries, ptahv1alpha1.JournalApplyFinished)].Approval = "kept"
			return entries
		}), plan, `under approval "kept"`},
		{"no Apply", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			index := at(entries, ptahv1alpha1.JournalApplyFinished)
			return append(entries[:index:index], entries[index+1:]...)
		}), plan, "0 ApplyFinished"},
		{"two Applies", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			return append(entries, entries[at(entries, ptahv1alpha1.JournalApplyFinished)])
		}), plan, "2 ApplyFinished"},
		{"an Apply of unknown outcome", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			entries[at(entries, ptahv1alpha1.JournalApplyFinished)].Outcome = ptahv1alpha1.JournalUnknown
			return entries
		}), plan, `as "Unknown"`},
		{"an Apply before the publication", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			published := entries[at(entries, ptahv1alpha1.JournalPlanPublished)]
			return append(entries[1:], published)
		}), plan, "before it publishes"},
		{"no proof", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			index := at(entries, ptahv1alpha1.JournalVerified)
			return entries[:index:index]
		}), plan, "stops before the Verified"},
		{"a proof that found drift", edited(func(entries []ptahv1alpha1.JournalEntry) []ptahv1alpha1.JournalEntry {
			entries[at(entries, ptahv1alpha1.JournalVerified)].Outcome = ptahv1alpha1.JournalDriftRemains
			return entries
		}), plan, `as "DriftRemains"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := unattendedApplyJournaled(test.journal, test.plan)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unattendedApplyJournaled() = %v, want an error naming %q", err, test.want)
			}
		})
	}
}

func TestApprovalsOfPlanCountsOnlyThatPlan(t *testing.T) {
	t.Parallel()

	approval := func(uid string) ptahv1alpha1.PtahSchemaApproval {
		return ptahv1alpha1.PtahSchemaApproval{Spec: ptahv1alpha1.PtahSchemaApprovalSpec{
			PlanRef: ptahv1alpha1.ImmutableObjectReference{Name: "ptah-plan-" + uid, UID: types.UID(uid)},
		}}
	}
	approvals := []ptahv1alpha1.PtahSchemaApproval{approval("grant"), approval("view"), approval("definer")}
	if got := approvalsOfPlan(approvals, "view"); got != 1 {
		t.Fatalf("approvalsOfPlan(view) = %d, want 1", got)
	}
	if got := approvalsOfPlan(approvals, "body"); got != 0 {
		t.Fatalf("approvalsOfPlan(body) = %d, want 0: the other plans' approvals are not this one's", got)
	}
}
