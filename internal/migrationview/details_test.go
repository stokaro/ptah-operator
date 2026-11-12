package migrationview_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
	"github.com/stokaro/ptah-operator/internal/statusview"
	"github.com/stokaro/ptah-operator/internal/statusview/statusviewtest"
)

// Every field the status can hold is printed by --details, each as a line a
// reader finds by its API name: the revision rows, each run whole and the
// claimed operation included. The status is filled by reflection, so a field
// the API gains fails this until the details print it. The conditions are the
// view's own table and the journal is kubectl ptah history's.
func TestDetailsPrintEveryFieldTheStatusHolds(t *testing.T) {
	t.Parallel()

	migration := detailedMigration()
	lines := statusviewtest.Fill(&migration.Status, "Conditions", "Journal")
	if len(lines) < 150 {
		t.Fatalf("filled %d fields of the status; it holds more than that", len(lines))
	}
	view, err := migrationview.LoadWithDetails(context.Background(), readerWith(t, migration), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	details := detailsOf(t, render(t, view, migrationview.Text))
	for _, line := range lines {
		if !strings.Contains(details, line.Text+"\n") {
			t.Errorf("--details does not print %s as %q", line.Path, line.Text)
		}
	}
	if t.Failed() {
		t.Log(details)
	}
}

// -o json with --details carries the status as it is stored, every field of
// it, and without --details the view's JSON has no details at all.
func TestDetailsJSONCarriesTheWholeStatus(t *testing.T) {
	t.Parallel()

	migration := detailedMigration()
	statusviewtest.Fill(&migration.Status)
	reader := readerWith(t, migration)

	view, err := migrationview.LoadWithDetails(context.Background(), reader, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	var decoded migrationview.View
	decoder := json.NewDecoder(strings.NewReader(render(t, view, migrationview.JSON)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Details == nil || !equality.Semantic.DeepEqual(decoded.Details.Status, migration.Status) {
		t.Fatalf("details.status differs from the stored status:\ncarried %+v\nstored  %+v", decoded.Details, migration.Status)
	}
	if decoded.Details.ReadRequested != "2026-03-02T00:00:00Z" {
		t.Fatalf("readRequested = %q, the annotation holds 2026-03-02T00:00:00Z", decoded.Details.ReadRequested)
	}

	plain, err := migrationview.Load(context.Background(), reader, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(render(t, plain, migrationview.JSON)), &document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document["details"]; ok {
		t.Fatal("the view's JSON carries details nobody asked for")
	}
	if strings.Contains(render(t, plain, migrationview.Text), statusview.DetailsHeading) {
		t.Fatal("the view's text carries details nobody asked for")
	}
}

// Each record says what it is where its fields alone would not: an attempt
// that waits out the delay the one before it asked for, the latest run apart
// from the hold nobody has accounted for, and an acknowledgment that is a
// statement and not proof.
func TestDetailsSayWhatEachRecordMeans(t *testing.T) {
	t.Parallel()

	retryAt := metav1.NewTime(time.Date(2026, 3, 1, 9, 5, 0, 0, time.UTC))
	tests := []struct {
		name   string
		change func(*operatorv1alpha1.PtahMigration)
		says   []string
		denies []string
	}{
		{
			name: "a retry that waits",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
					Type: operatorv1alpha1.MigrationOperationApply, ID: "op-3", Attempt: 3,
					JobName: "ptah-m-apply-orders-3", RetryNotBefore: &retryAt,
				}
			},
			says: []string{
				"Active operation:     Apply, attempt 3\n",
				"Its Job ptah-m-apply-orders-3 is reserved by name and not created yet",
				"This attempt waits until 2026-03-01T09:05:00Z before it starts",
			},
			denies: []string{"claimed", "is confirmed"},
		},
		{
			name: "a run held unresolved",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.LastRun = &operatorv1alpha1.MigrationRunStatus{Outcome: operatorv1alpha1.MigrationRunOutcomePartial}
				migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{Outcome: operatorv1alpha1.MigrationRunOutcomePartial}
			},
			says: []string{
				"Last run:             Partial\n",
				"A run nobody has accounted for is the unresolved run, a record of its own.",
				"Unresolved run:       Partial\n",
				"It holds the migration whatever the last run or the conditions say",
				"\nActive operation:     none\n",
			},
			denies: []string{"Settled run"},
		},
		{
			name: "a run a person acknowledged",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
					Outcome: operatorv1alpha1.MigrationRunOutcomePartial, Resolution: operatorv1alpha1.MigrationRunResolvedByAcknowledgment,
				}
			},
			says: []string{
				"Settled run:          Acknowledged\n",
				"a statement that it is accounted for, not proof that the data is correct",
			},
			denies: []string{"found nothing of the artifact left"},
		},
		{
			name: "a run a reading settled",
			change: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
					Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, Resolution: operatorv1alpha1.MigrationRunResolvedByHistoryRead,
				}
			},
			says:   []string{"A read-only reading of the database found nothing of the artifact left to apply."},
			denies: []string{"not proof"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := detailedMigration()
			test.change(migration)
			view, err := migrationview.LoadWithDetails(context.Background(), readerWith(t, migration), "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			details := detailsOf(t, render(t, view, migrationview.Text))
			// A lead is wrapped, so the claims are compared as words.
			flat := flatten(details)
			for _, want := range test.says {
				if !strings.Contains(flat, flatten(want)) {
					t.Errorf("--details does not say %q", want)
				}
			}
			for _, refused := range test.denies {
				if strings.Contains(flat, flatten(refused)) {
					t.Errorf("--details says %q", refused)
				}
			}
			if t.Failed() {
				t.Log(details)
			}
		})
	}
}

func detailedMigration() *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "orders", UID: "migration-uid", Generation: 4,
		Annotations: map[string]string{operatorv1alpha1.ReadRequestAnnotation: "2026-03-02T00:00:00Z"},
	}}
}

func flatten(text string) string { return strings.Join(strings.Fields(text), " ") }

func detailsOf(t *testing.T, text string) string {
	t.Helper()
	start := strings.Index(text, statusview.DetailsHeading)
	if start < 0 {
		t.Fatalf("the view printed no details:\n%s", text)
	}
	return text[start:]
}
