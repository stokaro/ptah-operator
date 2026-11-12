package schemaview_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/schemaview"
	"github.com/stokaro/ptah-operator/internal/statusview"
	"github.com/stokaro/ptah-operator/internal/statusview/statusviewtest"
)

// Every field the status can hold is printed by --details, each as a line a
// reader finds by its API name. The status is filled by reflection, so a
// field the API gains fails this until the details print it. The conditions
// are the view's own table and the journal is kubectl ptah history's.
func TestDetailsPrintEveryFieldTheStatusHolds(t *testing.T) {
	t.Parallel()

	schema := detailedSchema()
	lines := statusviewtest.Fill(&schema.Status, "Conditions", "Journal")
	if len(lines) < 200 {
		t.Fatalf("filled %d fields of the status; it holds more than that", len(lines))
	}
	view, err := schemaview.LoadWithDetails(context.Background(), readerWith(t, schema), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	details := detailsOf(t, renderText(t, view))
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

	schema := detailedSchema()
	statusviewtest.Fill(&schema.Status)
	reader := readerWith(t, schema)

	view, err := schemaview.LoadWithDetails(context.Background(), reader, "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var decoded schemaview.View
	decoder := json.NewDecoder(strings.NewReader(renderJSON(t, view)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Details == nil || !equality.Semantic.DeepEqual(decoded.Details.Status, schema.Status) {
		t.Fatalf("details.status differs from the stored status:\ncarried %+v\nstored  %+v", decoded.Details, schema.Status)
	}
	if decoded.Details.ReadRequested != "2026-03-02T00:00:00Z" {
		t.Fatalf("readRequested = %q, the annotation holds 2026-03-02T00:00:00Z", decoded.Details.ReadRequested)
	}

	plain, err := schemaview.Load(context.Background(), reader, "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(renderJSON(t, plain)), &document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document["details"]; ok {
		t.Fatal("the view's JSON carries details nobody asked for")
	}
	if strings.Contains(renderText(t, plain), statusview.DetailsHeading) {
		t.Fatal("the view's text carries details nobody asked for")
	}
}

// Each record says what it is where its fields alone would not: a Job named
// by a claim and not yet confirmed by UID, a Job confirmed, an Apply whose
// own result is not the verdict, and a proof a suspension does not end.
func TestDetailsSayWhatEachClaimMeans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*operatorv1alpha1.PtahSchema)
		says   []string
		denies []string
	}{
		{
			name: "a reserved Job",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationApply, ID: "op-1", Attempt: 2, JobName: "ptah-apply-storefront-1",
					StartedAt: metav1.NewTime(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)),
				}
			},
			says: []string{
				"Active operation:     Apply, attempt 2, claimed 2026-03-01T09:00:00Z\n",
				"Its Job ptah-apply-storefront-1 is reserved by name and not created yet: no UID is recorded.",
				"\nPending proof:        none\n",
			},
			denies: []string{"is confirmed"},
		},
		{
			name: "a create attempted and not confirmed",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationApply, ID: "op-1", Attempt: 1, JobName: "ptah-apply-storefront-1",
					DispatchStarted: true,
				}
			},
			says:   []string{"is reserved and its one create was attempted, but no UID is recorded"},
			denies: []string{"is confirmed", "not created yet"},
		},
		{
			name: "a confirmed Job",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationObserve, ID: "op-2", Attempt: 1, JobName: "ptah-observe-storefront-2",
					JobUID: "job-uid", DispatchStarted: true,
				}
			},
			says:   []string{"Its Job ptah-observe-storefront-2 is confirmed: the claim records the Job's UID job-uid"},
			denies: []string{"reserved"},
		},
		{
			name: "an Apply that reported success and owes its proof",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				after := metav1.NewTime(time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC))
				schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
					Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: "op-1", ObserveAfter: &after,
				}
			},
			says: []string{
				"Pending proof:        the reading that proves an Apply is owed\n",
				"The Apply reported success. That is the Apply's own result, not the verdict",
				"It waits until 2026-03-01T09:30:00Z",
				"\nActive operation:     none\n",
			},
			denies: []string{"Suspension does not end it"},
		},
		{
			name: "an Apply nobody can account for, on a suspended schema",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Suspend = true
				schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
					Outcome: operatorv1alpha1.PendingObservationOutcomeUnknown, ApplyOperationID: "op-1",
				}
			},
			says: []string{
				"Nobody can say how the Apply ended.",
				"Suspension does not end it: nothing else changes the database until it settles.",
			},
			denies: []string{"reported success", "It waits until"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := detailedSchema()
			test.change(schema)
			view, err := schemaview.LoadWithDetails(context.Background(), readerWith(t, schema), "team-a", "storefront")
			if err != nil {
				t.Fatal(err)
			}
			details := detailsOf(t, renderText(t, view))
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

func detailedSchema() *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "storefront", UID: "schema-uid", Generation: 4,
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

func renderText(t *testing.T, view schemaview.View) string {
	t.Helper()
	var text strings.Builder
	if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	return text.String()
}

func renderJSON(t *testing.T, view schemaview.View) string {
	t.Helper()
	var text strings.Builder
	if err := schemaview.Render(&text, view, schemaview.JSON); err != nil {
		t.Fatal(err)
	}
	return text.String()
}
