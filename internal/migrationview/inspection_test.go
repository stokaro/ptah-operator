package migrationview_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		"Phase:            Blocked\n" +
			"Ready:            False, ApplyOutcomeUnknown: What the dispatched run did is unknown until the database is read\n" +
			"Blocked:          True, ApplyOutcomeUnknown: the dispatched Apply Job is missing and will not be recreated\n" +
			"Policy:           apply OnApproval\n",
		"\nLast run:         Unknown, started 2026-09-30T06:30:36Z, finished 2026-09-30T06:30:43Z\n" +
			"  Job:            ptah-m-apply-e2e-uncertain-mysql-04d370d4ccc65a4c, UID 2cde010b-1e46-46af-a3fb-35a75edacc7f\n" +
			"  Message:        the dispatched Apply Job is missing and will not be recreated\n",
		"\nUnresolved run:   Unknown, recorded 2026-09-30T06:30:43Z\n" +
			"  Operation:      sha256:9d8a95ebef8319572f9c4f38853a5a4815316e07f83f56865f9f7cab742dcded\n" +
			"  Job:            ptah-m-apply-e2e-uncertain-mysql-04d370d4ccc65a4c, UID 2cde010b-1e46-46af-a3fb-35a75edacc7f\n" +
			"  Plan:           ptah-mplan-8f517ba4f0200f2a4e095b65, UID 04800e23-c41a-40ff-9745-c77de45237b1\n" +
			"  Database:       sha256:dd3598d94799d326af49fb6f455ea24c3959af067ca4cff5219f1987480e4abd (target identity digest)\n" +
			"  While this record stands nothing is planned and nothing runs.\n",
		"  migration UID migration-uid and the operation above.\n",
		`A migration run nobody`,
		"kubectl events -n team-a --for ptahmigration/e2e-uncertain-mysql prints its Events.\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
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
	if want := "  Job:            none recorded: the API server never confirmed its create\n"; !strings.Contains(render(t, view, migrationview.Text), want) {
		t.Fatalf("text view is missing %q", want)
	}
}

// A Partial last run that nothing holds any more is an outcome, not a hold:
// the reading below kept its last run after the record was settled. The
// acknowledgment that settled it is still there, consumed, and names a run
// that is not unresolved any more.
func TestRenderTextShowsNoHoldWithoutAnUnresolvedRun(t *testing.T) {
	t.Parallel()

	operation := "sha256:" + strings.Repeat("1", 64)
	settled := &operatorv1alpha1.PtahMigration{}
	readReading(t, "partial-run-left-a-dirty-revision.json", settled)
	settled.Namespace, settled.Name, settled.UID = "team-a", "orders", "migration-uid"
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
		ResolvedAt:     metav1.NewTime(time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)),
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
		"\nLast run:         Partial, started 2026-01-01T00:00:00Z, finished 2026-01-01T00:00:00Z\n",
		"\nSettled run:      Partial run sha256:" + strings.Repeat("1", 64) +
			", by acknowledgment orders-run-accounted-for from alice at 2026-01-01T00:05:00Z\n",
		"Blocked:          True, HistoryDirty: Revision 4 is recorded dirty; a person has to decide what the interrupted run did\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Unresolved run:") || strings.Contains(text, "nothing is planned") ||
		strings.Contains(text, "the unresolved run") {
		t.Fatalf("a settled run was shown as a hold:\n%s", text)
	}

	settled.Status.ResolvedRun.Resolution = operatorv1alpha1.MigrationRunResolvedByHistoryRead
	settled.Status.ResolvedRun.AcknowledgmentRef, settled.Status.ResolvedRun.AcknowledgedBy = nil, nil
	view, err = migrationview.Load(context.Background(), readerWith(t, settled), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if want := ", by a history read at 2026-01-01T00:05:00Z\n"; !strings.Contains(render(t, view, migrationview.Text), want) {
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
		"  Job:            ptah-m-apply-orders-0123456789abcdef, UID job-uid\n",
		"  Applied:        3\n",
		"  Stopped at:     version 4, statement 2 of 3, SQLSTATE 42P01\n",
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
