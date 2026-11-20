package migrationview_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
)

// The view says what a reader needs about repeatable migrations: how many are
// pending, which ones stop the resource and which ones a record keeps with no
// file, which ones the plan runs after its versions and whether each is new or
// changed, and which ones the last run applied. The control is the fixture
// without them, whose text names none.
func TestRenderSaysWhatTheRepeatableMigrationsAre(t *testing.T) {
	t.Parallel()

	const (
		content = "h1:AAkShW1bQ18rh5+kF6nNdfoHF+XZ/7T1YU+poQYCPN4="
		before  = "h1:BBkShW1bQ18rh5+kF6nNdfoHF+XZ/7T1YU+poQYCPN4="
	)
	migration, plan := fixture()
	control, err := migrationview.Load(context.Background(), readerWith(t, migration.DeepCopy(), plan.DeepCopy()), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if text := render(t, control, migrationview.Text); strings.Contains(text, "repeatable") || strings.Contains(text, "Repeatable") {
		t.Fatalf("a migration with no repeatable migration names one:\n%s", text)
	}

	migration.Status.History.PendingRepeatableCount = 2
	migration.Status.History.UnfinishedRepeatables = []string{"order_totals"}
	migration.Status.History.OrphanedRepeatables = []string{"old_names", "old_totals"}
	migration.Status.LastRun.AppliedVersions = []int64{2}
	migration.Status.LastRun.AppliedRepeatables = []string{"item_names"}
	migration.Status.LastRun.MigrationLog = &operatorv1alpha1.MigrationLogStatus{
		Written: 3, Lost: 1, Losses: []operatorv1alpha1.MigrationLogLossStatus{{
			Repeatable: "item_names", State: operatorv1alpha1.MigrationLogEntryApplied, SQLState: "22001",
		}},
	}
	plan.Spec.Repeatables = []operatorv1alpha1.PlannedRepeatableMigration{
		{Identity: "item_names", Checksum: content},
		{Identity: "order_totals", Checksum: content, AppliedChecksum: before},
	}
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	text := render(t, view, migrationview.Text)
	for _, want := range []string{
		"Pending repeatable:   2\n",
		"Unfinished:           order_totals\n",
		"Orphaned:             old_names, old_totals (recorded applied, no file; nothing runs)\n",
		"  Versions:           2\n",
		"  Repeatables:        item_names\n",
		"repeatable migration item_names Applied (SQLSTATE 22001)",
		"Plan ptah-mplan-0123456789abcdef01234567, 1 migration and 2 repeatable migrations from version 2:\n" +
			"  3                    add the orders index\n" +
			"  repeatable           item_names [new]\n" +
			"  repeatable           order_totals [changed]\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text does not say %q", want)
		}
	}
	if t.Failed() {
		t.Log(text)
	}

	var decoded struct {
		History struct {
			PendingRepeatableCount int32    `json:"pendingRepeatableCount"`
			UnfinishedRepeatables  []string `json:"unfinishedRepeatables"`
			OrphanedRepeatables    []string `json:"orphanedRepeatables"`
		} `json:"history"`
		Plan struct {
			Repeatables []migrationview.PlannedRepeatableView `json:"repeatables"`
		} `json:"plan"`
		LastRun struct {
			AppliedRepeatables []string `json:"appliedRepeatables"`
		} `json:"lastRun"`
	}
	if err := json.Unmarshal([]byte(render(t, view, migrationview.JSON)), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.History.PendingRepeatableCount != 2 || len(decoded.History.UnfinishedRepeatables) != 1 ||
		len(decoded.History.OrphanedRepeatables) != 2 || len(decoded.LastRun.AppliedRepeatables) != 1 ||
		len(decoded.Plan.Repeatables) != 2 || decoded.Plan.Repeatables[1].AppliedChecksum != before {
		t.Fatalf("JSON view = %+v", decoded)
	}
}

// A plan of repeatable migrations alone counts them, and no versions, and its
// JSON lists no version rather than a null list.
func TestRenderCountsAPlanOfRepeatableMigrationsAlone(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	plan.Spec.Migrations = nil
	plan.Spec.Repeatables = []operatorv1alpha1.PlannedRepeatableMigration{
		{Identity: "item_names", Checksum: "h1:AAkShW1bQ18rh5+kF6nNdfoHF+XZ/7T1YU+poQYCPN4="},
	}
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if want := ", 1 repeatable migration from version 2:\n  repeatable           item_names [new]\n"; !strings.Contains(render(t, view, migrationview.Text), want) {
		t.Fatalf("text does not say %q:\n%s", want, render(t, view, migrationview.Text))
	}
	var decoded struct {
		Plan map[string]json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal([]byte(render(t, view, migrationview.JSON)), &decoded); err != nil {
		t.Fatal(err)
	}
	if migrations := string(decoded.Plan["migrations"]); migrations != "[]" {
		t.Fatalf("the JSON plan lists migrations as %q, want []", migrations)
	}
}
