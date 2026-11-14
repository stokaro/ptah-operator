package historyview_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/historyview"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

var day0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func at(offset time.Duration) metav1.Time { return metav1.NewTime(day0.Add(offset)) }

func ptr[T any](value T) *T { return &value }

// Every field a journal entry can carry reaches the text, each where a
// reader finds it. The entry is filled by reflection, so a field the API
// gains has to be given a value here, and then has to be printed, before
// this passes again.
func TestTextPrintsEveryFieldAnEntryRecords(t *testing.T) {
	t.Parallel()

	filled := map[string]any{
		"Type":             operatorv1alpha1.JournalRunFinished,
		"At":               at(time.Hour),
		"StartedAt":        ptr(at(time.Hour - 90*time.Second)),
		"LastAt":           ptr(at(2 * time.Hour)),
		"Count":            int32(7),
		"OperationID":      "sha256:" + strings.Repeat("9", 64),
		"Plan":             "ptah-mplan-field-plan",
		"PlanUID":          types.UID("field-plan-uid"),
		"PlanFingerprint":  "sha256:field-fingerprint",
		"Statements":       int32(41),
		"Destructive":      true,
		"ApprovalRequired": true,
		"Replaced":         "ptah-plan-field-replaced",
		"Authorization":    operatorv1alpha1.MigrationRunAuthorizedByPolicy,
		"Approval":         "field-approval",
		"ApprovalUID":      types.UID("field-approval-uid"),
		"Approver":         "field-approver@example.com",
		"Outcome":          operatorv1alpha1.JournalPartial,
		"Operation":        "History",
		"Reason":           "FieldReason",
		"JobName":          "ptah-m-apply-field-job",
		"JobUID":           types.UID("field-job-uid"),
		"AppliedVersions":  int32(13),
		"Failure":          &operatorv1alpha1.MigrationRunFailureStatus{Version: 17, Statement: 19, Statements: 23, SQLState: "42P07", ErrorNumber: 1050},
		"ApplyFailure":     &operatorv1alpha1.SchemaApplyFailureStatus{Statement: 29, Statements: 31, SQLState: "23505", ErrorNumber: 1062},
		"MigrationLog": &operatorv1alpha1.MigrationLogStatus{Written: 37, Lost: 1, Losses: []operatorv1alpha1.MigrationLogLossStatus{
			{Version: 47, State: operatorv1alpha1.MigrationLogEntryApplied, SQLState: "22001", ErrorNumber: 1406},
		}},
		"Acknowledgment": "field-acknowledgment",
		"AcknowledgedBy": "field-acknowledger@example.com",
	}
	// What the text says for each field, where it is not the value itself.
	printed := map[string]string{
		"At":               "2026-10-01T10:00:00Z  RunFinished",
		"StartedAt":        "  Started:            2026-10-01T09:58:30Z, 1m30s before it ended\n",
		"LastAt":           "the last at 2026-10-01T11:00:00Z",
		"Count":            "  Attempts:           7 in a row",
		"Statements":       "  Statements:         41, destructive, asked for an approval when it was published\n",
		"Destructive":      ", destructive",
		"ApprovalRequired": "asked for an approval when it was published",
		"Authorization":    "  Authorized:         spec.policy.apply Always, with no approval\n",
		"Outcome":          "RunFinished, Partial",
		"AppliedVersions":  "  Versions applied:   13\n",
		"Failure":          "  Stopped at:         version 17, statement 19 of 23, SQLSTATE 42P07, error 1050\n",
		"ApplyFailure":     "  Stopped at:         statement 29 of 31, SQLSTATE 23505, error 1062\n",
		"MigrationLog": "  Migration log:      1 of 38 entries refused, so the database's record of the run is incomplete: " +
			"version 47 Applied (SQLSTATE 22001, error 1406)\n",
	}

	var entry operatorv1alpha1.JournalEntry
	value := reflect.ValueOf(&entry).Elem()
	for index := range value.NumField() {
		name := value.Type().Field(index).Name
		fill, ok := filled[name]
		if !ok {
			t.Fatalf("JournalEntry has a field %s this test gives no value; add one, and make the history print it", name)
		}
		value.Field(index).Set(reflect.ValueOf(fill))
	}
	// A published schema plan prints its statements, which a run does not
	// carry; the same entry is printed once as each, and every field has to be
	// found in one of them.
	asPublished := entry
	asPublished.Type = operatorv1alpha1.JournalPlanPublished
	text := render(t, viewOf(historyview.Migration, entry)) + render(t, viewOf(historyview.Schema, asPublished))

	for name := range filled {
		want, ok := printed[name]
		if !ok {
			want = fmt.Sprint(reflect.ValueOf(entry).FieldByName(name).Interface())
		}
		if !strings.Contains(text, want) {
			t.Errorf("the history does not print %s as %q:\n%s", name, want, text)
		}
	}
}

// A journal the bounds pruned says from when it is complete, and an entry the
// bounds kept from before then says so. The journal is the one the operator's
// own code keeps: a plan published and never carried out, then failures over
// more than thirty days.
func TestTextSaysWhatThePrunedJournalCovers(t *testing.T) {
	t.Parallel()

	journal, _ := lifecyclejournal.Record(nil, []operatorv1alpha1.JournalEntry{{
		Type: operatorv1alpha1.JournalPlanPublished, At: at(0), Plan: "ptah-plan-waiting", Statements: 2, ApprovalRequired: true,
	}}, day0)
	for _, day := range []int{1, 2, 40} {
		reason := fmt.Sprintf("Reason%d", day)
		journal, _ = lifecyclejournal.Record(journal, []operatorv1alpha1.JournalEntry{{
			Type: operatorv1alpha1.JournalOperationFailed, At: at(time.Duration(day) * 24 * time.Hour), Operation: "Observe", Reason: reason,
		}}, day0.Add(time.Duration(day)*24*time.Hour))
	}
	if len(journal.Entries) != 2 || !journal.CoveredSince.After(day0.Add(24*time.Hour)) {
		t.Fatalf("the bounds kept %d entries and cover from %s; the test needs a journal they pruned", len(journal.Entries), journal.CoveredSince)
	}
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "storefront", UID: "schema-uid", CreationTimestamp: at(-time.Hour)},
		Status:     operatorv1alpha1.PtahSchemaStatus{Journal: journal},
	}
	view, err := historyview.Load(context.Background(), reader(t, nil, schema), historyview.Schema, "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	text := render(t, view)
	for _, want := range []string{
		"Journal:              2 entries, complete since 2026-10-03T09:00:00Z\n",
		"2026-10-01T09:00:00Z  PlanPublished (dated before the journal's coverage begins)\n",
		"2026-11-10T09:00:00Z  OperationFailed, Observe, Reason40\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the history does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Reason40 (dated before") || !view.Journal.Entries[0].BeforeCoverage || view.Journal.Entries[1].BeforeCoverage {
		t.Fatalf("the mark is on the wrong entry: %+v\n%s", view.Journal.Entries, text)
	}
}

// Each join is shown refusing what it must refuse: a plan of the name the
// resource does not own, a plan of the name under another UID, an approval
// under another UID, and an acknowledgment of the name for another
// operation. An entry that recorded too little to join is not joined at all.
func TestJoinsRefuseWhatTheEntryDoesNotIdentify(t *testing.T) {
	t.Parallel()

	migration := &operatorv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "orders", UID: "orders-uid", CreationTimestamp: at(0),
	}}
	operation := "sha256:" + strings.Repeat("1", 64)
	other := "sha256:" + strings.Repeat("2", 64)
	migration.Status.Journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: at(0), Entries: []operatorv1alpha1.JournalEntry{
		{Type: operatorv1alpha1.JournalPlanPublished, At: at(1), Plan: "ptah-mplan-owned"},
		{Type: operatorv1alpha1.JournalPlanPublished, At: at(2), Plan: "ptah-mplan-foreign"},
		{Type: operatorv1alpha1.JournalApprovalAccepted, At: at(3), Plan: "ptah-mplan-owned", PlanUID: "another-uid",
			Approval: "orders-approval", ApprovalUID: "approval-uid"},
		{Type: operatorv1alpha1.JournalApprovalAccepted, At: at(4), Approval: "orders-approval", ApprovalUID: "deleted-approval-uid"},
		{Type: operatorv1alpha1.JournalApprovalAccepted, At: at(5), Approval: "orders-approval"},
		{Type: operatorv1alpha1.JournalRunAcknowledged, At: at(6), OperationID: operation, Acknowledgment: "orders-accounted"},
		{Type: operatorv1alpha1.JournalRunAcknowledged, At: at(7), OperationID: other, Acknowledgment: "orders-accounted"},
		{Type: operatorv1alpha1.JournalRunAcknowledged, At: at(8), Acknowledgment: "orders-accounted"},
	}}
	owned := &operatorv1alpha1.PtahMigrationPlan{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "ptah-mplan-owned", UID: "owned-uid", OwnerReferences: []metav1.OwnerReference{
			{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration", Name: "orders", UID: "orders-uid", Controller: ptr(true)},
		},
	}}
	foreign := &operatorv1alpha1.PtahMigrationPlan{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "ptah-mplan-foreign", UID: "foreign-uid", OwnerReferences: []metav1.OwnerReference{
			{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration", Name: "orders", UID: "predecessor-uid", Controller: ptr(true)},
		},
	}}
	approval := &operatorv1alpha1.PtahMigrationApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-approval", UID: "approval-uid"},
		Spec:       operatorv1alpha1.PtahMigrationApprovalSpec{MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: "orders-uid"}},
	}
	acknowledgment := &operatorv1alpha1.PtahMigrationRunAcknowledgment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-accounted", UID: "acknowledgment-uid"},
		Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: "orders-uid"}, OperationID: operation,
		},
	}
	view, err := historyview.Load(context.Background(), reader(t, nil, migration, owned, foreign, approval, acknowledgment),
		historyview.Migration, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	type joins struct{ plan, approval, acknowledgment *bool }
	want := []joins{
		{plan: ptr(true)},
		{plan: ptr(false)},
		{plan: ptr(false), approval: ptr(true)},
		{approval: ptr(false)},
		{},
		{acknowledgment: ptr(true)},
		{acknowledgment: ptr(false)},
		{},
	}
	for index, entry := range view.Journal.Entries {
		got := joins{entry.PlanStored, entry.ApprovalStored, entry.AcknowledgmentStored}
		if !reflect.DeepEqual(got, want[index]) {
			t.Errorf("entry %d (%s at %s) joined as plan %s, approval %s, acknowledgment %s; want %s, %s, %s", index,
				entry.Type, entry.At, show(got.plan), show(got.approval), show(got.acknowledgment),
				show(want[index].plan), show(want[index].approval), show(want[index].acknowledgment))
		}
	}
}

// A list that fails for a reason other than RBAC leaves every finding of its
// kind unknown, rather than false, and says how it failed.
func TestAFailedListLeavesItsFindingsUnknown(t *testing.T) {
	t.Parallel()

	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "storefront", UID: "schema-uid", CreationTimestamp: at(0)},
		Status: operatorv1alpha1.PtahSchemaStatus{Journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(0), Entries: []operatorv1alpha1.JournalEntry{
			{Type: operatorv1alpha1.JournalApprovalAccepted, At: at(1), Plan: "ptah-plan-a", Approval: "a", ApprovalUID: "a-uid"},
		}}},
	}
	refuse := func(list client.ObjectList) error {
		switch list.(type) {
		case *metav1.PartialObjectMetadataList:
			return errors.New("connection reset by peer")
		case *operatorv1alpha1.PtahSchemaApprovalList:
			return apierrors.NewForbidden(k8sschema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemaapprovals"}, "",
				errors.New("no list"))
		}
		return nil
	}
	view, err := historyview.Load(context.Background(), reader(t, refuse, schema), historyview.Schema, "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	entry := view.Journal.Entries[0]
	if entry.PlanStored != nil || entry.ApprovalStored != nil {
		t.Fatalf("a list that failed left a finding: plan %s, approval %s", show(entry.PlanStored), show(entry.ApprovalStored))
	}
	if view.PlansUnreadable == nil || view.PlansUnreadable.Forbidden || view.ApprovalsUnreadable == nil || !view.ApprovalsUnreadable.Forbidden {
		t.Fatalf("plans %+v, approvals %+v", view.PlansUnreadable, view.ApprovalsUnreadable)
	}
	text := render(t, view)
	for _, want := range []string{
		"Whether each plan is still stored is not shown: listing ptahschemaplans failed:\nconnection reset by peer\n",
		"\nApprovals: not shown; listing ptahschemaapprovals is forbidden to you (RBAC):",
		"  Plan:               ptah-plan-a\n",
		"  Approval:           a, UID a-uid\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the history does not say %q:\n%s", want, text)
		}
	}
}

// The same holds for a migration's acknowledgments: the list failing leaves
// an acknowledged run's mark unknown, while the approvals, which were listed,
// still mark theirs.
func TestAFailedAcknowledgmentListLeavesItsMarkUnknown(t *testing.T) {
	t.Parallel()

	operation := "sha256:" + strings.Repeat("3", 64)
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "orders-uid", CreationTimestamp: at(0)},
		Status: operatorv1alpha1.PtahMigrationStatus{Journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(0), Entries: []operatorv1alpha1.JournalEntry{
			{Type: operatorv1alpha1.JournalApprovalAccepted, At: at(1), Approval: "orders-approval", ApprovalUID: "approval-uid"},
			{Type: operatorv1alpha1.JournalRunAcknowledged, At: at(2), OperationID: operation, Acknowledgment: "orders-accounted"},
		}}},
	}
	acknowledgment := &operatorv1alpha1.PtahMigrationRunAcknowledgment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-accounted", UID: "acknowledgment-uid"},
		Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: "orders-uid"}, OperationID: operation,
		},
	}
	refuse := func(list client.ObjectList) error {
		if _, ok := list.(*operatorv1alpha1.PtahMigrationRunAcknowledgmentList); ok {
			return errors.New("the server is currently unable to handle the request")
		}
		return nil
	}
	view, err := historyview.Load(context.Background(), reader(t, refuse, migration, acknowledgment),
		historyview.Migration, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	approved, acknowledged := view.Journal.Entries[0], view.Journal.Entries[1]
	if acknowledged.AcknowledgmentStored != nil || view.AcknowledgmentsUnreadable == nil || view.AcknowledgmentsUnreadable.Forbidden {
		t.Fatalf("acknowledgment mark %s, unreadable %+v", show(acknowledged.AcknowledgmentStored), view.AcknowledgmentsUnreadable)
	}
	if approved.ApprovalStored == nil || *approved.ApprovalStored {
		t.Fatalf("the approvals were listed and found none, and the mark is %s", show(approved.ApprovalStored))
	}
	text := render(t, view)
	for _, want := range []string{
		"  Acknowledgment:     orders-accounted\n",
		"  Approval:           orders-approval, UID approval-uid (no longer stored)\n",
		"\nRun acknowledgments: not shown; listing ptahmigrationrunacknowledgments failed:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the history does not say %q:\n%s", want, text)
		}
	}
}

// What an entry carries reaches the terminal from whoever wrote it, an
// identity provider's username included. A control sequence in it is printed
// as its escape.
func TestTextEscapesWhatTheEntryCarries(t *testing.T) {
	t.Parallel()

	entry := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalApprovalAccepted, At: at(0), Approval: "a\x1b[2J", ApprovalUID: "uid", Approver: "eve\u202e\x1b]0;x\x07",
	}
	text := render(t, viewOf(historyview.Schema, entry))
	if strings.ContainsAny(text, "\x1b\x07\u202e") {
		t.Fatalf("the history printed a control character:\n%q", text)
	}
	if !strings.Contains(text, `eve\u202e\x1b]0;x\x07`) {
		t.Fatalf("the history does not show the escaped approver:\n%s", text)
	}
}

func viewOf(kind historyview.Kind, entries ...operatorv1alpha1.JournalEntry) historyview.View {
	view := historyview.View{
		Kind: kind, Namespace: "team-a", Name: "subject", UID: "subject-uid", CreatedAt: at(-time.Hour),
		Journal: &historyview.Journal{CoveredSince: at(0)},
	}
	for _, entry := range entries {
		view.Journal.Entries = append(view.Journal.Entries, historyview.Entry{JournalEntry: entry})
	}
	return view
}

func render(t *testing.T, view historyview.View) string {
	t.Helper()
	var text strings.Builder
	if err := historyview.Render(&text, view, historyview.Text); err != nil {
		t.Fatal(err)
	}
	return text.String()
}

func show(value *bool) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprint(*value)
}

func reader(t *testing.T, refuse func(client.ObjectList) error, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
	if refuse != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
				if err := refuse(list); err != nil {
					return err
				}
				return inner.List(ctx, list, options...)
			},
		})
	}
	return builder.Build()
}
