package telemetry_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

var collectorNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// fixedView is a view whose answers a test controls.
type fixedView struct {
	synced     bool
	schemas    []time.Time
	migrations []time.Time
	err        error
}

func (v fixedView) Synced() bool { return v.synced }

func (v fixedView) UnresolvedSchemas(context.Context) ([]time.Time, error) {
	return v.schemas, v.err
}

func (v fixedView) UnresolvedMigrations(context.Context) ([]time.Time, error) {
	return v.migrations, v.err
}

func gather(t *testing.T, view telemetry.UnresolvedView) string {
	t.Helper()
	registry := prometheus.NewRegistry()
	telemetry.NewUnresolvedCollector(registry, view, func() time.Time { return collectorNow })
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out strings.Builder
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			out.WriteString(family.GetName())
			for _, label := range metric.GetLabel() {
				out.WriteString("{" + label.GetName() + "=" + label.GetValue() + "}")
			}
			if metric.GetGauge() != nil {
				out.WriteString(" " + formatValue(metric.GetGauge().GetValue()))
			}
			if metric.GetCounter() != nil {
				out.WriteString(" " + formatValue(metric.GetCounter().GetValue()))
			}
			out.WriteString("\n")
		}
	}
	return out.String()
}

func formatValue(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// An unsynchronized view publishes no counts. Zero would read as "nothing is
// unresolved", which is the false negative this signal exists to prevent.
func TestAnUnsyncedViewReportsNoCountAtAll(t *testing.T) {
	t.Parallel()
	out := gather(t, fixedView{synced: false})
	if !strings.Contains(out, "ptah_operator_unresolved_view_synced 0") {
		t.Fatalf("the scrape does not say the view is unsynchronized:\n%s", out)
	}
	if strings.Contains(out, "ptah_operator_unresolved_attempts") {
		t.Fatalf("an unsynchronized view published a count:\n%s", out)
	}
}

// A read that failed is not a population of zero either, and it is counted so
// a collector failing every scrape is visible.
func TestAFailedReadIsNotAPopulationOfZero(t *testing.T) {
	t.Parallel()
	out := gather(t, fixedView{synced: true, err: errors.New("the API server refused the list")})
	if strings.Contains(out, "ptah_operator_unresolved_attempts") {
		t.Fatalf("a failed read published a count:\n%s", out)
	}
	if !strings.Contains(out, "ptah_operator_unresolved_view_synced 0") {
		t.Fatalf("a failed read did not mark the view unusable:\n%s", out)
	}
	if !strings.Contains(out, "ptah_operator_unresolved_view_read_failures_total 1") {
		t.Fatalf("a failed read was not counted:\n%s", out)
	}
}

// Both families are reported, and the age is measured from the instant the
// operator could first have settled the record.
func TestBothFamiliesReportTheirOwnPopulationAndAge(t *testing.T) {
	t.Parallel()
	out := gather(t, fixedView{
		synced: true,
		schemas: []time.Time{
			collectorNow.Add(-90 * time.Minute),
			collectorNow.Add(-10 * time.Minute),
		},
		migrations: []time.Time{collectorNow.Add(-2 * time.Hour)},
	})
	for _, want := range []string{
		"ptah_operator_unresolved_attempts{family=schema} 2",
		"ptah_operator_unresolved_attempts{family=migration} 1",
		"ptah_operator_unresolved_owed_seconds{family=schema} 5400",
		"ptah_operator_unresolved_owed_seconds{family=migration} 7200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the scrape does not carry %q:\n%s", want, out)
		}
	}
}

// A family with nothing unresolved reports a count of zero and no age. An age
// of zero would read as a record made this instant, which is the opposite.
func TestAFamilyWithNothingUnresolvedPublishesNoAge(t *testing.T) {
	t.Parallel()
	out := gather(t, fixedView{synced: true, migrations: []time.Time{collectorNow.Add(-time.Minute)}})
	if !strings.Contains(out, "ptah_operator_unresolved_attempts{family=schema} 0") {
		t.Errorf("the empty family was not reported as zero:\n%s", out)
	}
	if strings.Contains(out, "ptah_operator_unresolved_owed_seconds{family=schema}") {
		t.Errorf("the empty family published an age:\n%s", out)
	}
}

// The records are read from the same durable fields the controllers refuse on,
// and a schema whose Apply is accounted for is not one of them.
func TestOnlyUnaccountedRecordsAreCounted(t *testing.T) {
	t.Parallel()
	horizon := metav1.NewTime(collectorNow.Add(-time.Hour))
	schemas := []operatorv1alpha1.PtahSchema{
		{Status: operatorv1alpha1.PtahSchemaStatus{PendingObservation: &operatorv1alpha1.PendingObservationStatus{
			Outcome: operatorv1alpha1.PendingObservationOutcomeUnknown, ObserveAfter: &horizon}}},
		{Status: operatorv1alpha1.PtahSchemaStatus{PendingObservation: &operatorv1alpha1.PendingObservationStatus{
			Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ObserveAfter: &horizon}}},
		{},
	}
	if records := telemetry.UnresolvedSchemaRecords(schemas); len(records) != 1 {
		t.Fatalf("counted %d schemas; only the one whose Apply is unaccounted for is unresolved", len(records))
	}

	recordedAt := metav1.NewTime(collectorNow.Add(-time.Hour))
	migrations := []operatorv1alpha1.PtahMigration{
		{Status: operatorv1alpha1.PtahMigrationStatus{UnresolvedRun: &operatorv1alpha1.UnresolvedMigrationRunStatus{
			RecordedAt: recordedAt}}},
		{},
	}
	if records := telemetry.UnresolvedMigrationRecords(migrations); len(records) != 1 {
		t.Fatalf("counted %d migrations; only the one carrying a record is unresolved", len(records))
	}
}

// A record with no horizon is still a record. Counting it and inventing an age
// would report a number nothing measured.
func TestARecordWithNoHorizonIsCountedWithoutAnAge(t *testing.T) {
	t.Parallel()
	schemas := []operatorv1alpha1.PtahSchema{
		{Status: operatorv1alpha1.PtahSchemaStatus{PendingObservation: &operatorv1alpha1.PendingObservationStatus{
			Outcome: operatorv1alpha1.PendingObservationOutcomeUnknown}}},
	}
	records := telemetry.UnresolvedSchemaRecords(schemas)
	if len(records) != 1 || !records[0].IsZero() {
		t.Fatalf("records = %#v, want one zero time", records)
	}
	out := gather(t, fixedView{synced: true, schemas: records})
	if !strings.Contains(out, "ptah_operator_unresolved_attempts{family=schema} 1") {
		t.Errorf("the record was not counted:\n%s", out)
	}
	if strings.Contains(out, "ptah_operator_unresolved_owed_seconds{family=schema}") {
		t.Errorf("a record with no horizon published an age:\n%s", out)
	}
}
