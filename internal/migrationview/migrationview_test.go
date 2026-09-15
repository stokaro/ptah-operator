package migrationview_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
)

func TestLoadReadsTheMigrationAndThePlanItNames(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != string(operatorv1alpha1.MigrationPhaseAwaitingApproval) {
		t.Fatalf("phase = %q", view.Phase)
	}
	if view.History == nil || view.History.PendingCount != 1 || view.History.CurrentVersion != 2 {
		t.Fatalf("history = %#v", view.History)
	}
	if view.Plan == nil || len(view.Plan.Migrations) != 1 || view.Plan.Migrations[0].Version != 3 {
		t.Fatalf("plan = %#v", view.Plan)
	}
	if view.LastRun == nil || view.LastRun.Outcome != string(operatorv1alpha1.MigrationRunOutcomeFailed) {
		t.Fatalf("last run = %#v", view.LastRun)
	}
	if view.Artifact == nil || view.Artifact.Digest == "" {
		t.Fatalf("artifact = %#v", view.Artifact)
	}
}

func TestLoadOmitsAPlanTheMigrationNoLongerNames(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	plan.UID = types.UID("a-replaced-plan")
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if view.Plan != nil {
		t.Fatal("a plan the status no longer names was shown as current")
	}

	migration, _ = fixture()
	view, err = migrationview.Load(context.Background(), readerWith(t, migration), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if view.Plan != nil {
		t.Fatal("a reference that outlived its object was shown as a plan")
	}
	if view.History == nil {
		t.Fatal("a missing plan hid the history that is still true")
	}
}

func TestLoadSaysWhenThereIsNoSuchMigration(t *testing.T) {
	t.Parallel()

	_, err := migrationview.Load(context.Background(), readerWith(t), "team-a", "orders")
	if !errors.Is(err, migrationview.ErrMigrationNotFound) {
		t.Fatalf("error = %v, want a not-found answer", err)
	}
}

func TestRenderTextCarriesNoSQLAndNoRow(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := migrationview.Render(&out, view, migrationview.Text); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"team-a/orders", "AwaitingApproval", "Current version:", "Pending:", "add the orders index",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q does not mention %q", text, want)
		}
	}
	for _, unwanted := range []string{"INSERT", "UPDATE", "DELETE", "CREATE ", "postgres://"} {
		if strings.Contains(strings.ToUpper(text), unwanted) {
			t.Fatalf("text carries %q, which a migration view never prints:\n%s", unwanted, text)
		}
	}
}

// TestRenderNamesTheOutOfOrderVersions proves the view carries the versions a
// blocked history was blocked by. A phase alone says the history stopped; which
// migration arrived late is what the person reading this has to decide about.
func TestRenderNamesTheOutOfOrderVersions(t *testing.T) {
	t.Parallel()

	migration, _ := fixture()
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.History.OutOfOrderVersions = []int64{2, 4}
	view, err := migrationview.Load(context.Background(), readerWith(t, migration), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []struct {
		format migrationview.Format
		want   string
	}{
		{format: migrationview.Text, want: "Out of order:   2, 4"},
		{format: migrationview.JSON, want: `"outOfOrderVersions": [`},
	} {
		var out strings.Builder
		if err := migrationview.Render(&out, view, format.format); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), format.want) {
			t.Fatalf("%v output does not name the out-of-order versions:\n%s", format.format, out.String())
		}
	}
}

func TestRenderSaysWhenNothingIsPlanned(t *testing.T) {
	t.Parallel()

	migration, _ := fixture()
	migration.Status.Plan = nil
	view, err := migrationview.Load(context.Background(), readerWith(t, migration), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := migrationview.Render(&out, view, migrationview.Text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No plan is published.") {
		t.Fatalf("text does not say that nothing is planned:\n%s", out.String())
	}
}

func TestRenderJSONIsAStructuredView(t *testing.T) {
	t.Parallel()

	migration, plan := fixture()
	view, err := migrationview.Load(context.Background(), readerWith(t, migration, plan), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := migrationview.Render(&out, view, migrationview.JSON); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"migration": "orders"`, `"pendingCount": 1`, `"version": 3`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("JSON %s does not contain %s", out.String(), want)
		}
	}
}

func TestRenderRefusesAnUnknownFormat(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	if err := migrationview.Render(&out, migrationview.View{}, migrationview.Format("yaml")); err == nil {
		t.Fatal("an unknown format was rendered")
	}
}

func readerWith(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func fixture() (*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan) {
	digest := "sha256:" + strings.Repeat("a", 64)
	observed := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	plan := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "ptah-mplan-0123456789abcdef01234567", UID: types.UID("plan-uid"),
		},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{
			ContractVersion: 1,
			MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: types.UID("migration-uid")},
			Fingerprint:     digest,
			CurrentVersion:  2,
			CreatedAt:       observed,
			Migrations: []operatorv1alpha1.PlannedMigration{{
				Version: 3, Description: "add the orders index", Checksum: "checksum-3",
			}},
		},
	}
	finished := metav1.NewTime(observed.Add(time.Minute))
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: types.UID("migration-uid")},
		Status: operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhaseAwaitingApproval,
			Artifact: &operatorv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: "oci://registry.example/acme/orders-migrations@" + digest, Digest: digest,
			},
			History: &operatorv1alpha1.MigrationHistoryStatus{
				ObservedAt: observed, ContractVersion: 1, CurrentVersion: 2,
				Fingerprint: digest, TargetIdentityDigest: digest,
				AppliedCount: 2, PendingCount: 1,
			},
			Plan: &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			LastRun: &operatorv1alpha1.MigrationRunStatus{
				Outcome:   operatorv1alpha1.MigrationRunOutcomeFailed,
				StartedAt: observed, FinishedAt: &finished,
				Message: "A migration failed and committed nothing",
			},
			Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionTrue,
				Reason: "AwaitingApproval", Message: "1 planned migration needs an approval",
				LastTransitionTime: observed,
			}},
		},
	}
	return migration, plan
}
