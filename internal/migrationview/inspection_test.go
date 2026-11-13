package migrationview_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
)

// The native reading of a MySQL migration whose dispatched Apply Job went
// missing: its last run and the run nobody has accounted for are the same
// execution, and the view carries both records and the Job's identity.
func TestLoadReadsTheUnresolvedRunAndTheLastRunsJob(t *testing.T) {
	t.Parallel()

	held := uncertainMigration(t)
	view, err := migrationview.Load(context.Background(), readerWith(t, held), "team-a", held.Name)
	if err != nil {
		t.Fatal(err)
	}
	recorded := held.Status.UnresolvedRun
	want := &migrationview.UnresolvedRunView{
		Outcome:              "Unknown",
		OperationID:          recorded.OperationID,
		JobName:              "ptah-m-apply-e2e-uncertain-mysql-04d370d4ccc65a4c",
		JobUID:               "2cde010b-1e46-46af-a3fb-35a75edacc7f",
		PlanRef:              recorded.PlanRef,
		TargetIdentityDigest: recorded.TargetIdentityDigest,
		RecordedAt:           recorded.RecordedAt,
	}
	if got := view.UnresolvedRun; got == nil || *got != *want {
		t.Fatalf("unresolved run = %+v, want %+v", got, want)
	}
	if run := view.LastRun; run == nil || run.JobName != want.JobName || run.JobUID != want.JobUID || run.Outcome != "Unknown" {
		t.Fatalf("last run = %+v", run)
	}
}

// A hold always names a database, and the record of its run says whether anybody
// reported it. A hold beside no such record does not say which it is.
func TestRenderTextSaysWhenAHoldsDatabaseComesFromItsPlan(t *testing.T) {
	t.Parallel()

	const fromPlan = "of its plan: the run named none"
	for _, row := range []struct {
		name     string
		mutate   func(*operatorv1alpha1.PtahMigrationStatus)
		fromPlan bool
	}{
		{"its run named none", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.LastRun.OperationID = status.UnresolvedRun.OperationID
		}, true},
		{"its run named this one", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.LastRun.OperationID = status.UnresolvedRun.OperationID
			status.LastRun.TargetIdentityDigest = status.UnresolvedRun.TargetIdentityDigest
		}, false},
		{"the last run is another's", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.LastRun.OperationID = "sha256:" + strings.Repeat("f", 64)
		}, false},
		{"a release before the field recorded the run", func(*operatorv1alpha1.PtahMigrationStatus) {}, false},
		{"a restore left no record of the run", func(status *operatorv1alpha1.PtahMigrationStatus) { status.LastRun = nil }, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			held := uncertainMigration(t)
			row.mutate(&held.Status)
			view, err := migrationview.Load(context.Background(), readerWith(t, held), "team-a", held.Name)
			if err != nil {
				t.Fatal(err)
			}
			text := render(t, view, migrationview.Text)
			if !strings.Contains(text, held.Status.UnresolvedRun.TargetIdentityDigest+" (target identity digest") {
				t.Fatalf("the block does not name the database:\n%s", text)
			}
			if strings.Contains(text, fromPlan) != row.fromPlan {
				t.Fatalf("from its plan = %t, want %t:\n%s", strings.Contains(text, fromPlan), row.fromPlan, text)
			}
		})
	}
}

// The last run is a history of outcomes and the unresolved run is a hold, and
// the text keeps them in two blocks: the reader sees which Job ran last, and
// separately that nothing runs until this one is accounted for, and how the
// operations guide says to account for it.
func TestRenderTextKeepsTheLastRunApartFromTheUnresolvedHold(t *testing.T) {
	t.Parallel()

	held := uncertainMigration(t)
	view, err := migrationview.Load(context.Background(), readerWith(t, held), "team-a", held.Name)
	if err != nil {
		t.Fatal(err)
	}
	text := render(t, view, migrationview.Text)
	for _, want := range []string{
		"Phase:                Blocked\n" +
			"Ready:                False, ApplyOutcomeUnknown: What the dispatched run did is unknown until the database is read\n" +
			"Blocked:              True, ApplyOutcomeUnknown: the dispatched Apply Job is missing and will not be recreated\n" +
			"Policy:               apply OnApproval\n",
		"\nLast run:             Unknown, started 2026-09-30T06:30:36Z, finished 2026-09-30T06:30:43Z\n" +
			"  Job:                ptah-m-apply-e2e-uncertain-mysql-04d370d4ccc65a4c, UID 2cde010b-1e46-46af-a3fb-35a75edacc7f\n" +
			"  Message:            the dispatched Apply Job is missing and will not be recreated\n",
		"\nUnresolved run:       Unknown, recorded 2026-09-30T06:30:43Z\n" +
			"  Operation:          sha256:9d8a95ebef8319572f9c4f38853a5a4815316e07f83f56865f9f7cab742dcded\n" +
			"  Job:                ptah-m-apply-e2e-uncertain-mysql-04d370d4ccc65a4c, UID 2cde010b-1e46-46af-a3fb-35a75edacc7f\n" +
			"  Plan:               ptah-mplan-8f517ba4f0200f2a4e095b65, UID 04800e23-c41a-40ff-9745-c77de45237b1\n" +
			"  Database:           sha256:dd3598d94799d326af49fb6f455ea24c3959af067ca4cff5219f1987480e4abd (target identity digest)\n",
		"\nkubectl ptah history migration/e2e-uncertain-mysql -n team-a prints its lifecycle history and every decision that names it.\n",
		"kubectl events -n team-a --for ptahmigration/e2e-uncertain-mysql prints its Events.\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	// The guidance is prose wrapped to the terminal, so it is read as words.
	// Reads go on while the record stands; the database is the only one the
	// run can have changed; and the acknowledgment takes the UID and operation
	// printed here.
	prose := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{
		"While this record stands no plan is published and no Apply is dispatched; resolve, verify and the history read go on.",
		"The runner starts nothing against a database other than its plan's, so the run changed this one or none.",
		"A reader who inspects another database, such as a replica or a restored copy, finds nothing wrong and settles a record that was telling the truth.",
		"settle it with a PtahMigrationRunAcknowledgment naming migration UID migration-uid and the operation above.",
		`The operations guide has the procedure, under "A migration run nobody accounted for".`,
	} {
		if !strings.Contains(prose, want) {
			t.Fatalf("the guidance does not say %q:\n%s", want, text)
		}
	}
	// Blocked=True ApplyOutcomeUnknown is the hold itself, and it stops no
	// reading.
	if strings.Contains(prose, "held before any reading") {
		t.Fatalf("the hold was read as a gate in front of the reading:\n%s", text)
	}
	// The guidance is wrapped for a terminal: every line of it that is not a
	// field fits 78 columns.
	_, block, _ := strings.Cut(text, "\nUnresolved run:")
	block, _, _ = strings.Cut(block, "\n\n")
	wrapped := 0
	for _, line := range strings.Split(block, "\n")[1:] {
		if guidanceField.MatchString(line) {
			continue
		}
		wrapped++
		if len(line) > 78 {
			t.Errorf("a guidance line runs past 78 columns: %q", line)
		}
	}
	if wrapped < 5 {
		t.Fatalf("read %d guidance lines, so the width check held almost nothing:\n%s", wrapped, block)
	}
	// The run nobody accounted for has no retry to offer.
	for _, unwanted := range []string{"retry", "Retry", "rerun", "delete"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("text view offers %q for a run nobody accounted for:\n%s", unwanted, text)
		}
	}
}

// A run whose create the API server never confirmed records no Job, and the
// view says so rather than printing nothing a reader could mistake for a
// field it forgot.
func TestRenderTextSaysAnUnresolvedRunRecordsNoJob(t *testing.T) {
	t.Parallel()

	held := uncertainMigration(t)
	held.Status.UnresolvedRun.JobName = ""
	held.Status.UnresolvedRun.JobUID = ""
	view, err := migrationview.Load(context.Background(), readerWith(t, held), "team-a", held.Name)
	if err != nil {
		t.Fatal(err)
	}
	if want := "  Job:                none recorded: the API server never confirmed its create\n"; !strings.Contains(render(t, view, migrationview.Text), want) {
		t.Fatalf("text view is missing %q", want)
	}
}

// A Partial last run that nothing holds any more is an outcome, not a hold:
// the reading below holds a partial run unresolved, and settling it moves the
// record to resolvedRun and keeps the last run. The acknowledgment that settled
// it is still there, consumed, and names a run that is not unresolved any more.
func TestRenderTextShowsNoHoldWithoutAnUnresolvedRun(t *testing.T) {
	t.Parallel()

	settled := &operatorv1alpha1.PtahMigration{}
	readReading(t, "partial-run-left-a-dirty-revision.json", settled)
	settled.Namespace, settled.Name, settled.UID = "team-a", "orders", "migration-uid"
	if settled.Status.UnresolvedRun == nil || settled.Status.LastRun == nil || settled.Status.LastRun.FinishedAt == nil {
		t.Fatal("the reading holds no finished partial run left unresolved")
	}
	operation := settled.Status.UnresolvedRun.OperationID
	settled.Status.UnresolvedRun = nil
	resolvedAt := metav1.NewTime(settled.Status.LastRun.FinishedAt.Add(5 * time.Minute).UTC())
	consumed := &operatorv1alpha1.PtahMigrationRunAcknowledgment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-run-accounted-for", UID: "acknowledgment-uid"},
		Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
			MigrationRef:   operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: settled.UID},
			OperationID:    operation,
			AcknowledgedBy: operatorv1alpha1.ApprovalIdentity{Username: "alice"},
		},
		Status: operatorv1alpha1.PtahMigrationRunAcknowledgmentStatus{Conditions: []metav1.Condition{{
			Type: operatorv1alpha1.ConditionAcknowledgmentConsumed, Status: metav1.ConditionTrue,
			Reason: string(operatorv1alpha1.ReasonRunAcknowledged), Message: "The acknowledgment settled the run it names",
		}}},
	}
	settled.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
		OperationID: operation,
		Outcome:     operatorv1alpha1.MigrationRunOutcomePartial,
		Resolution:  operatorv1alpha1.MigrationRunResolvedByAcknowledgment,
		AcknowledgmentRef: &operatorv1alpha1.ImmutableObjectReference{
			Name: "orders-run-accounted-for", UID: "acknowledgment-uid",
		},
		AcknowledgedBy: &operatorv1alpha1.ApprovalIdentity{Username: "alice"},
		ResolvedAt:     resolvedAt,
	}
	view, err := migrationview.Load(context.Background(), readerWith(t, settled, consumed), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if items := view.Acknowledgments.Items; len(items) != 1 || items[0].NamesUnresolvedRun {
		t.Fatalf("acknowledgments = %+v, want one that names no unresolved run", items)
	}
	text := render(t, view, migrationview.Text)
	for _, want := range []string{
		"    operation " + operation + "\n",
		"    Consumed  True    RunAcknowledged  -      The acknowledgment settled the run it names\n",
		"\nLast run:             Partial, started " + settled.Status.LastRun.StartedAt.UTC().Format(time.RFC3339) +
			", finished " + settled.Status.LastRun.FinishedAt.UTC().Format(time.RFC3339) + "\n",
		"\nSettled run:          Partial run " + operation +
			", by acknowledgment orders-run-accounted-for from alice at " + resolvedAt.Format(time.RFC3339) + "\n",
		"Blocked:              True, HistoryDirty: Revision 4 is recorded dirty; a person has to decide what the interrupted run did\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Unresolved run:") || strings.Contains(text, "no plan is published") ||
		strings.Contains(text, "the unresolved run") {
		t.Fatalf("a settled run was shown as a hold:\n%s", text)
	}

	settled.Status.ResolvedRun.Resolution = operatorv1alpha1.MigrationRunResolvedByHistoryRead
	settled.Status.ResolvedRun.AcknowledgmentRef, settled.Status.ResolvedRun.AcknowledgedBy = nil, nil
	view, err = migrationview.Load(context.Background(), readerWith(t, settled), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if want := ", by a history read at " + resolvedAt.Format(time.RFC3339) + "\n"; !strings.Contains(render(t, view, migrationview.Text), want) {
		t.Fatalf("text view is missing %q", want)
	}
}

// Where a statement reached the database and failed there, the last run says
// which one, in numbers rather than in the database's words.
func TestRenderTextSaysWhereTheLastRunStopped(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	migration.Status.LastRun.Outcome = operatorv1alpha1.MigrationRunOutcomePartial
	migration.Status.LastRun.JobName = "ptah-m-apply-orders-0123456789abcdef"
	migration.Status.LastRun.JobUID = types.UID("job-uid")
	migration.Status.LastRun.AppliedVersions = []int64{3}
	migration.Status.LastRun.Failure = &operatorv1alpha1.MigrationRunFailureStatus{
		Version: 4, Statement: 2, Statements: 3, SQLState: "42P01",
	}
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	text := render(t, view, migrationview.Text)
	for _, want := range []string{
		"  Job:                ptah-m-apply-orders-0123456789abcdef, UID job-uid\n",
		"  Versions:           3\n",
		"  Stopped at:         version 4, statement 2 of 3, SQLSTATE 42P01\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	document := render(t, view, migrationview.JSON)
	for _, want := range []string{`"jobName": "ptah-m-apply-orders-0123456789abcdef"`, `"jobUID": "job-uid"`, `"sqlState": "42P01"`} {
		if !strings.Contains(document, want) {
			t.Fatalf("json view is missing %s:\n%s", want, document)
		}
	}
}

// The record of a run says what it carried out and who allowed it, and the view
// leaves out what the record does not hold rather than making it up.
func TestRenderTextSaysWhatTheLastRunCarriedOut(t *testing.T) {
	t.Parallel()

	stamped := metav1.NewTime(time.Date(2026, 8, 30, 11, 50, 0, 0, time.UTC))
	read := metav1.NewTime(time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC))
	const (
		operation   = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		fingerprint = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	provenance := func(run *operatorv1alpha1.MigrationRunStatus) {
		run.OperationID = operation
		run.PlanRef = &operatorv1alpha1.ImmutableObjectReference{Name: "orders-plan", UID: "plan-uid"}
		run.PlanFingerprint = fingerprint
		run.Authorization = &operatorv1alpha1.MigrationRunAuthorizationStatus{
			Kind:        operatorv1alpha1.MigrationRunAuthorizedByApproval,
			ApprovalRef: &operatorv1alpha1.ImmutableObjectReference{Name: "orders-approval", UID: "approval-uid"},
			Approver:    &operatorv1alpha1.ApprovalIdentity{Username: "alice@example.test"},
			ApprovedAt:  &stamped,
		}
		run.Verification = &operatorv1alpha1.MigrationRunVerificationStatus{
			ObservedAt: read, HistoryFingerprint: fingerprint, CurrentVersion: 3, Result: operatorv1alpha1.MigrationRunVerifiedInSync,
		}
	}
	for _, row := range []struct {
		name     string
		change   func(*operatorv1alpha1.MigrationRunStatus)
		want     []string
		wantJSON []string
		refuse   []string
		// refuseJSON is what the JSON view leaves out of everything it carries.
		refuseJSON []string
	}{
		{
			name:   "an approved run read back",
			change: provenance,
			want: []string{
				"  Operation:          " + operation + "\n",
				"  Plan:               orders-plan, fingerprint " + fingerprint + "\n",
				"  Authorized:         approval orders-approval (UID approval-uid) by alice@example.test at 2026-08-30T11:50:00Z\n",
				"  Read back:          InSync at version 3, 2026-08-30T12:05:00Z\n",
			},
			wantJSON: []string{
				`"operationID": "` + operation + `"`, `"planFingerprint": "` + fingerprint + `"`, `"kind": "Approval"`,
				`"username": "alice@example.test"`, `"historyFingerprint": "` + fingerprint + `"`,
			},
		},
		{
			name: "a run under the policy",
			change: func(run *operatorv1alpha1.MigrationRunStatus) {
				provenance(run)
				run.Authorization = &operatorv1alpha1.MigrationRunAuthorizationStatus{Kind: operatorv1alpha1.MigrationRunAuthorizedByPolicy}
			},
			want:     []string{"  Authorized:         spec.policy.apply Always, with no approval\n"},
			wantJSON: []string{`"kind": "Policy"`},
		},
		{
			// An approval's stamp nobody copied stays unknown.
			name: "a claim that copied no stamp",
			change: func(run *operatorv1alpha1.MigrationRunStatus) {
				provenance(run)
				run.PlanFingerprint = ""
				run.Authorization.Approver, run.Authorization.ApprovedAt = nil, nil
				run.Verification = nil
			},
			want:   []string{"  Plan:               orders-plan\n", "  Authorized:         approval orders-approval (UID approval-uid)\n"},
			refuse: []string{"fingerprint", "alice", "Read back"},
		},
		{
			name:       "a run from before the record",
			change:     func(*operatorv1alpha1.MigrationRunStatus) {},
			refuse:     []string{"Operation:", "Plan:", "Authorized:", "Read back:"},
			refuseJSON: []string{"operationID", "authorization", "verification"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := fixture()
			row.change(migration.Status.LastRun)
			view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			text := render(t, view, migrationview.Text)
			for _, want := range row.want {
				if !strings.Contains(text, want) {
					t.Fatalf("text view is missing %q:\n%s", want, text)
				}
			}
			document := render(t, view, migrationview.JSON)
			for _, want := range row.wantJSON {
				if !strings.Contains(document, want) {
					t.Fatalf("json view is missing %s:\n%s", want, document)
				}
			}
			block, _, _ := strings.Cut(text[strings.Index(text, "Last run"):], "\n\n")
			for _, refuse := range row.refuse {
				if strings.Contains(block, refuse) {
					t.Fatalf("the view says %q, which the record does not hold:\n%s", refuse, block)
				}
			}
			for _, refuse := range row.refuseJSON {
				if strings.Contains(document, refuse) {
					t.Fatalf("the json view says %q, which the record does not hold:\n%s", refuse, document)
				}
			}
		})
	}
}

// A resource held before any reading -- suspended, or refused for its engine or
// its realm -- never reaches the history read that would clear the record, and
// the view says so, and that an acknowledgment still settles it. The near
// misses are the hold itself, and the gates' reasons on a condition that does
// not hold anything.
func TestRenderTextSaysWhenAGateStopsTheReadingThatWouldClearTheRecord(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		suspend bool
		blocked metav1.ConditionStatus
		reason  operatorv1alpha1.ConditionReason
		gate    string
	}{
		{name: "suspended", suspend: true, blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonApplyOutcomeUnknown, gate: "spec.suspend is set"},
		{name: "realm conflict", blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonRealmConflict, gate: "Blocked is True, RealmConflict"},
		{name: "realm not authorized", blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonRealmNotAuthorized, gate: "Blocked is True, RealmNotAuthorized"},
		{name: "unsupported engine", blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonUnsupportedEngine, gate: "Blocked is True, UnsupportedEngine"},
		{name: "a refused artifact", blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonPolicyRefused, gate: "Blocked is True, PolicyRefused"},
		{name: "a refusal that ended", blocked: metav1.ConditionFalse, reason: operatorv1alpha1.ReasonPolicySatisfied},
		{name: "the hold itself", blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonApplyOutcomeUnknown},
		{name: "a dirty history", blocked: metav1.ConditionTrue, reason: operatorv1alpha1.ReasonHistoryDirty},
		{name: "a realm reason not holding", blocked: metav1.ConditionFalse, reason: operatorv1alpha1.ReasonRealmConflict},
		{name: "a realm reason in doubt", blocked: metav1.ConditionUnknown, reason: operatorv1alpha1.ReasonRealmConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			held := uncertainMigration(t)
			held.Spec.Suspend = test.suspend
			for index := range held.Status.Conditions {
				if held.Status.Conditions[index].Type == operatorv1alpha1.ConditionMigrationBlocked {
					held.Status.Conditions[index].Status = test.blocked
					held.Status.Conditions[index].Reason = string(test.reason)
				}
			}
			view, err := migrationview.Load(context.Background(), readerWith(t, held), "team-a", held.Name)
			if err != nil {
				t.Fatal(err)
			}
			prose := strings.Join(strings.Fields(render(t, view, migrationview.Text)), " ")
			said := strings.Contains(prose, "This resource is held before any reading: ")
			if test.gate == "" {
				if said {
					t.Fatalf("a resource no gate holds was called held before any reading:\n%s", prose)
				}
				return
			}
			want := "This resource is held before any reading: " + test.gate +
				". No history read runs, so the record cannot clear by reading; an acknowledgment still settles it."
			if !strings.Contains(prose, want) {
				t.Fatalf("the view does not say %q:\n%s", want, prose)
			}
		})
	}
}

// The controller writes nothing on a migration approval until its plan is
// dispatched or a read withdraws it, so an approval with no condition says so
// without promising one.
func TestRenderTextSaysAMigrationApprovalHasNoCondition(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	approval := &operatorv1alpha1.PtahMigrationApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-approval"},
		Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
			MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: migration.UID},
			PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint: plan.Spec.Fingerprint,
		},
	}
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan, approval), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	text := render(t, view, migrationview.Text)
	if !strings.Contains(text, "    plan "+plan.Name+", the current plan\n    no condition written\n") ||
		strings.Contains(text, "no condition written yet") {
		t.Fatalf("text view does not say the approval has no condition:\n%s", text)
	}
}

// The decisions listed are the ones that name this migration by name and UID.
// The near misses are the ones a filter on the name alone would let through,
// and the ones that name this migration and another plan or another run.
func TestLoadListsTheDecisionsThatNameThisMigration(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomePartial, OperationID: "sha256:" + strings.Repeat("2", 64),
		PlanRef: operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-earlier", UID: "earlier-plan-uid"},
	}
	this := operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: migration.UID}
	predecessor := operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: "predecessor-uid"}
	created := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	approval := func(name string, minutes int, reference operatorv1alpha1.ImmutableObjectReference,
		planUID types.UID, fingerprint string,
	) *operatorv1alpha1.PtahMigrationApproval {
		return &operatorv1alpha1.PtahMigrationApproval{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: name,
				CreationTimestamp: metav1.NewTime(created.Add(time.Duration(minutes) * time.Minute)),
			},
			Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
				MigrationRef:    reference,
				PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: planUID},
				PlanFingerprint: fingerprint,
				Approver:        operatorv1alpha1.ApprovalIdentity{Username: "bob"},
			},
			Status: operatorv1alpha1.PtahMigrationApprovalStatus{Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionFalse,
				Reason: "Waiting", Message: "Not dispatched yet",
			}}},
		}
	}
	acknowledgment := func(name string, minutes int, reference operatorv1alpha1.ImmutableObjectReference, operation string,
	) *operatorv1alpha1.PtahMigrationRunAcknowledgment {
		return &operatorv1alpha1.PtahMigrationRunAcknowledgment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: name,
				CreationTimestamp: metav1.NewTime(created.Add(time.Duration(minutes) * time.Minute)),
			},
			Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
				MigrationRef: reference, OperationID: operation,
				AcknowledgedBy: operatorv1alpha1.ApprovalIdentity{Username: "carol"},
			},
		}
	}
	unresolved := migration.Status.UnresolvedRun.OperationID
	objects := []client.Object{
		migration, plan,
		approval("current", 1, this, plan.UID, plan.Spec.Fingerprint),
		approval("replaced-plan", 2, this, "replaced-plan-uid", plan.Spec.Fingerprint),
		approval("other-fingerprint", 3, this, plan.UID, "sha256:"+strings.Repeat("f", 64)),
		approval("predecessor", 4, predecessor, plan.UID, plan.Spec.Fingerprint),
		approval("other-migration", 5, operatorv1alpha1.ImmutableObjectReference{Name: "invoices", UID: migration.UID},
			plan.UID, plan.Spec.Fingerprint),
		acknowledgment("names-the-hold", 1, this, unresolved),
		acknowledgment("names-another-run", 2, this, "sha256:"+strings.Repeat("3", 64)),
		acknowledgment("predecessor-acknowledgment", 3, predecessor, unresolved),
	}
	view, err := migrationview.Load(context.Background(), readerWith(t, objects...), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}

	current := map[string]bool{}
	var approvals []string
	for _, item := range view.Approvals.Items {
		approvals = append(approvals, item.Name)
		current[item.Name] = item.CurrentPlan
	}
	if want := []string{"other-fingerprint", "replaced-plan", "current"}; !slices.Equal(approvals, want) {
		t.Fatalf("approvals = %v, want %v", approvals, want)
	}
	if !current["current"] || current["replaced-plan"] || current["other-fingerprint"] {
		t.Fatalf("current plan = %v", current)
	}
	names := map[string]bool{}
	var acknowledgments []string
	for _, item := range view.Acknowledgments.Items {
		acknowledgments = append(acknowledgments, item.Name)
		names[item.Name] = item.NamesUnresolvedRun
	}
	if want := []string{"names-another-run", "names-the-hold"}; !slices.Equal(acknowledgments, want) {
		t.Fatalf("acknowledgments = %v, want %v", acknowledgments, want)
	}
	if !names["names-the-hold"] || names["names-another-run"] {
		t.Fatalf("names the unresolved run = %v", names)
	}

	text := render(t, view, migrationview.Text)
	for _, want := range []string{
		"\nApprovals, newest first:\n",
		"  current\n    approved by bob at 0001-01-01T00:00:00Z\n    plan " + plan.Name + ", the current plan\n",
		"  replaced-plan\n    approved by bob at 0001-01-01T00:00:00Z\n    plan " + plan.Name + ", not the current plan\n",
		"\nRun acknowledgments, newest first:\n",
		"  names-the-hold\n    acknowledged by carol at 0001-01-01T00:00:00Z\n    operation " + unresolved + ", the unresolved run\n",
		"  names-another-run\n    acknowledged by carol at 0001-01-01T00:00:00Z\n    operation sha256:" + strings.Repeat("3", 64) + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"predecessor", "other-migration"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("text view lists %s, which names another migration:\n%s", unwanted, text)
		}
	}
}

// A reader who may get the migration and its plan, and nothing else, still
// gets the migration, with a line for each list it could not read. One list
// refused does not hide the other.
func TestLoadKeepsTheMigrationWhenTheDecisionsCannotBeListed(t *testing.T) {
	t.Parallel()

	forbidden := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "operator.ptah.run", Resource: resource}, "",
			errors.New(`User "reader" cannot list resource "`+resource+`"`))
	}
	for _, test := range []struct {
		name                       string
		refuse                     func(client.ObjectList) error
		approvalsShown, acksShown  bool
		approvalLine, ackLine      string
		approvalsForbidden, ackFbd bool
	}{
		{
			name: "both forbidden",
			refuse: func(list client.ObjectList) error {
				switch list.(type) {
				case *operatorv1alpha1.PtahMigrationApprovalList:
					return forbidden("ptahmigrationapprovals")
				case *operatorv1alpha1.PtahMigrationRunAcknowledgmentList:
					return forbidden("ptahmigrationrunacknowledgments")
				}
				return nil
			},
			approvalLine:       "\nApprovals: not shown; listing ptahmigrationapprovals is forbidden to you (RBAC): ",
			ackLine:            "\nRun acknowledgments: not shown; listing ptahmigrationrunacknowledgments is forbidden to you (RBAC): ",
			approvalsForbidden: true, ackFbd: true,
		},
		{
			name: "acknowledgments forbidden",
			refuse: func(list client.ObjectList) error {
				if _, ok := list.(*operatorv1alpha1.PtahMigrationRunAcknowledgmentList); ok {
					return forbidden("ptahmigrationrunacknowledgments")
				}
				return nil
			},
			approvalsShown: true,
			approvalLine:   "\nApprovals: none\n",
			ackLine:        "\nRun acknowledgments: not shown; listing ptahmigrationrunacknowledgments is forbidden to you (RBAC): ",
			ackFbd:         true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := fixture()
			reader := readerRefusingLists(t, test.refuse, migration, plan)
			view, err := migrationview.Load(context.Background(), reader, "team-a", "orders")
			if err != nil {
				t.Fatalf("Load() = %v; a list that failed must not fail the view", err)
			}
			if view.Plan == nil || view.History == nil {
				t.Fatalf("a refused list hid the migration: %+v", view)
			}
			if (view.Approvals != nil) != test.approvalsShown || (view.ApprovalsUnreadable != nil) == test.approvalsShown {
				t.Fatalf("approvals = %+v, unreadable = %+v", view.Approvals, view.ApprovalsUnreadable)
			}
			if view.Acknowledgments != nil || view.AcknowledgmentsUnreadable == nil ||
				view.AcknowledgmentsUnreadable.Forbidden != test.ackFbd {
				t.Fatalf("acknowledgments = %+v, unreadable = %+v", view.Acknowledgments, view.AcknowledgmentsUnreadable)
			}
			if test.approvalsForbidden && !view.ApprovalsUnreadable.Forbidden {
				t.Fatalf("approvals unreadable = %+v, want forbidden", view.ApprovalsUnreadable)
			}
			text := render(t, view, migrationview.Text)
			for _, want := range []string{test.approvalLine, test.ackLine, "add the orders index"} {
				if !strings.Contains(text, want) {
					t.Fatalf("text view is missing %q:\n%s", want, text)
				}
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(render(t, view, migrationview.JSON)), &decoded); err != nil {
				t.Fatal(err)
			}
			if _, listed := decoded["acknowledgments"]; listed {
				t.Fatal("json view carries an acknowledgment list it could not read")
			}
			if unreadable, ok := decoded["acknowledgmentsUnreadable"].(map[string]any); !ok || unreadable["forbidden"] != true {
				t.Fatalf("json view does not say why the acknowledgments are missing: %v", decoded["acknowledgmentsUnreadable"])
			}
			if _, listed := decoded["approvals"]; listed != test.approvalsShown {
				t.Fatalf("json view approvals listed = %t, want %t", listed, test.approvalsShown)
			}
		})
	}
}

// The JSON view keeps every field it carried before the runs, the policy and
// the decisions were added, under the same names.
func TestRenderJSONKeepsItsEarlierFields(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	migration.Spec.Suspend = true
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(render(t, view, migrationview.JSON)), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"namespace"}, {"migration"}, {"phase"}, {"suspended"},
		{"artifact", "kind"}, {"artifact", "reference"}, {"artifact", "digest"},
		{"history", "observedAt"}, {"history", "currentVersion"}, {"history", "appliedCount"},
		{"history", "pendingCount"}, {"history", "fingerprint"},
		{"plan", "name"}, {"plan", "fingerprint"}, {"plan", "currentVersion"}, {"plan", "createdAt"}, {"plan", "migrations"},
		{"lastRun", "outcome"}, {"lastRun", "startedAt"}, {"lastRun", "finishedAt"}, {"lastRun", "message"},
	} {
		current := any(decoded)
		for _, key := range path {
			object, ok := current.(map[string]any)
			if !ok {
				t.Fatalf("json view lost %s", strings.Join(path, "."))
			}
			if current, ok = object[key]; !ok {
				t.Fatalf("json view lost %s", strings.Join(path, "."))
			}
		}
	}
	conditions, ok := decoded["conditions"].([]any)
	if !ok || len(conditions) == 0 {
		t.Fatal("json view lost its conditions")
	}
	for _, key := range []string{"type", "status", "reason", "message"} {
		if _, ok := conditions[0].(map[string]any)[key]; !ok {
			t.Fatalf("a condition lost %s", key)
		}
	}
}

// guidanceField is a "  Label:  value" line of a run block.
var guidanceField = regexp.MustCompile(`^  [A-Z][A-Za-z ]*: +[^ ]`)

func render(t *testing.T, view migrationview.View, format migrationview.Format) string {
	t.Helper()
	var out strings.Builder
	if err := migrationview.Render(&out, view, format); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func readerRefusingLists(t *testing.T, refuse func(client.ObjectList) error, objects ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
				if err := refuse(list); err != nil {
					return err
				}
				return inner.List(ctx, list, options...)
			},
		}).Build()
}

// uncertainMigration is the native reading of a MySQL migration whose
// dispatched Apply Job went missing, from
// testdata/e2e/readings/uncertain-migration-retained-claim.json. The reading
// keeps the name and status. The namespace and UID a Get needs are set here,
// and so is the apply policy the API server defaults an unset one to, which a
// stored object always carries.
func uncertainMigration(t *testing.T) *operatorv1alpha1.PtahMigration {
	t.Helper()
	var engines map[string]*operatorv1alpha1.PtahMigration
	readReading(t, "uncertain-migration-retained-claim.json", &engines)
	held := engines["mysql"]
	if held == nil || held.Status.UnresolvedRun == nil {
		t.Fatal("the reading holds no MySQL migration with an unresolved run")
	}
	held.Namespace, held.UID = "team-a", "migration-uid"
	held.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	return held
}

func readReading(t *testing.T, name string, into any) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}
