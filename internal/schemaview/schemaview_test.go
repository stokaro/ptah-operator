package schemaview_test

import (
	"bytes"
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
	"github.com/stokaro/ptah-operator/internal/schemaview"
)

func TestLoadReadsWhereTheSchemaStands(t *testing.T) {
	t.Parallel()

	view, err := schemaview.Load(context.Background(), readerWith(t, fixture()), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != "AwaitingApproval" {
		t.Fatalf("phase = %q", view.Phase)
	}
	if view.Source == nil || !view.Source.Verified {
		t.Fatalf("source = %#v", view.Source)
	}
	if view.Observation == nil || !view.Observation.Drift || view.Observation.FindingCount != 6 {
		t.Fatalf("observation = %#v", view.Observation)
	}
	if view.Plan == nil || view.Plan.StatementCount != 4 || view.Plan.Approved {
		t.Fatalf("plan = %#v", view.Plan)
	}
	if view.Applied == nil || view.Applied.PlanName != "ptah-plan-earlier" {
		t.Fatalf("applied = %#v", view.Applied)
	}
}

// The reference-data row is the reason this view exists. It restates the three
// row categories as one line, and it carries counts alone.
func TestLoadSummarizesTheDeclaredRowDrift(t *testing.T) {
	t.Parallel()

	view, err := schemaview.Load(context.Background(), readerWith(t, fixture()), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	rows := view.Observation.ReferenceData
	if rows == nil {
		t.Fatal("an observation carrying row categories summarized no reference data")
	}
	if rows.Inserts != 2 || rows.Updates != 1 || rows.Deletes != 3 {
		t.Fatalf("reference data = %#v, want 2 inserts, 1 update, 3 deletes", rows)
	}
}

// A schema that declares no reference rows says nothing about them. Three
// zeroes would read as a measurement of something that was never compared.
func TestLoadOmitsReferenceDataWhenNoRowCategoryWasObserved(t *testing.T) {
	t.Parallel()

	schema := fixture()
	schema.Status.Target.DriftFindings = []operatorv1alpha1.DriftFindingStatus{
		{Category: "tables_added", Count: 1, Severity: "safe"},
	}
	schema.Status.Target.DriftFindingCount = 1
	schema.Status.Target.HighestDriftSeverity = "safe"
	view, err := schemaview.Load(context.Background(), readerWith(t, schema), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if view.Observation.ReferenceData != nil {
		t.Fatalf("reference data = %#v, want nothing", view.Observation.ReferenceData)
	}
}

// A schema nobody has observed is not a converged schema, and the view has to
// keep them apart.
func TestLoadSeparatesAnUnobservedSchemaFromAConvergedOne(t *testing.T) {
	t.Parallel()

	schema := fixture()
	schema.Status.Target = operatorv1alpha1.TargetStatus{}
	view, err := schemaview.Load(context.Background(), readerWith(t, schema), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if view.Observation != nil {
		t.Fatalf("observation = %#v, want nothing for a schema nobody observed", view.Observation)
	}

	converged := fixture()
	converged.Status.Target.DriftFindings = nil
	converged.Status.Target.DriftFindingCount = 0
	converged.Status.Target.HighestDriftSeverity = ""
	view, err = schemaview.Load(context.Background(), readerWith(t, converged), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if view.Observation == nil || view.Observation.Drift {
		t.Fatalf("observation = %#v, want a converged reading", view.Observation)
	}
}

func TestLoadFailurePath(t *testing.T) {
	t.Parallel()

	if _, err := schemaview.Load(context.Background(), readerWith(t), "team-a", "storefront"); !errors.Is(err, schemaview.ErrSchemaNotFound) {
		t.Fatalf("err = %v, want ErrSchemaNotFound", err)
	}
	if _, err := schemaview.Load(context.Background(), nil, "team-a", "storefront"); err == nil {
		t.Fatal("a nil reader was accepted")
	}
	if _, err := schemaview.Load(context.Background(), readerWith(t), "", "storefront"); err == nil {
		t.Fatal("an empty namespace was accepted")
	}
}

// The text view prints counts and never a value. The fixture's declared rows
// are named in the test rather than in the object, because the object is where
// they must not be: the assertion is that nothing row-shaped can reach this
// output at all.
func TestRenderTextCarriesCountsAndNoSQL(t *testing.T) {
	t.Parallel()

	view, err := schemaview.Load(context.Background(), readerWith(t, fixture()), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := schemaview.Render(&out, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Schema:           team-a/storefront",
		"Phase:            AwaitingApproval",
		"Reference data:   2 to insert, 1 to update, 3 to delete",
		"data_rows_deleted",
		"not approved",
		"kubectl ptah plan prints the SQL this plan holds.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"INSERT", "UPDATE", "DELETE", "SELECT", "VALUES"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("text view printed %q:\n%s", unwanted, text)
		}
	}
}

func TestRenderJSONIsTheSameView(t *testing.T) {
	t.Parallel()

	view, err := schemaview.Load(context.Background(), readerWith(t, fixture()), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := schemaview.Render(&out, view, schemaview.JSON); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"referenceData"`, `"inserts": 2`, `"updates": 1`, `"deletes": 3`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("json view is missing %s:\n%s", want, out.String())
		}
	}
}

func TestRenderFailurePath(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := schemaview.Render(&out, schemaview.View{}, schemaview.Format("yaml")); err == nil {
		t.Fatal("an unknown format was accepted")
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

// fixture is a schema whose declared reference rows have drifted three ways at
// once, waiting for a person to approve the plan that would close the
// difference.
func fixture() *operatorv1alpha1.PtahSchema {
	digest := "sha256:" + strings.Repeat("a", 64)
	observed := metav1.NewTime(time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC))
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "storefront", UID: types.UID("schema-uid")},
		Status: operatorv1alpha1.PtahSchemaStatus{
			Phase: operatorv1alpha1.ReconciliationPhase("AwaitingApproval"),
			Source: operatorv1alpha1.SchemaSourceStatus{
				ResolvedReference: "oci://registry.example/acme/storefront@" + digest,
				Digest:            digest,
				Verified:          true,
			},
			Target: operatorv1alpha1.TargetStatus{
				LastObservedAt:       &observed,
				HighestDriftSeverity: "destructive",
				DriftFindingCount:    6,
				DriftFindings: []operatorv1alpha1.DriftFindingStatus{
					{Category: "data_rows_deleted", Count: 3, Severity: "destructive"},
					{Category: "data_rows_updated", Count: 1, Severity: "destructive"},
					{Category: "data_rows_inserted", Count: 2, Severity: "safe"},
				},
			},
			Plan: &operatorv1alpha1.CurrentPlanStatus{
				Name:           "ptah-plan-4b44084123f0958600629632",
				UID:            types.UID("plan-uid"),
				Fingerprint:    digest,
				StatementCount: 4,
				CreatedAt:      observed,
			},
			Applied: &operatorv1alpha1.AppliedStatus{
				PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: "ptah-plan-earlier"},
				PlanFingerprint: digest,
				PtahVersion:     "v0.5.0-131-g939c2fb46",
				CompletedAt:     observed,
			},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "AwaitingApproval"},
				{Type: "ApprovalRequired", Status: metav1.ConditionTrue, Reason: "AwaitingApproval"},
			},
		},
	}
}
