package dataplane_test

import (
	"os"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The fixture is a `ptah schema drift --format json` document, captured from a
// run against a SQLite database holding the reference-data end-to-end
// fixtures: one declared row missing, one edited by hand, and one row in the
// table the declaration no longer holds. Two fields were removed after the
// capture, and nothing else: `sources` and `database_url` name the machine that
// produced it, and the second carries a connection string.
//
// It is captured rather than written because the question is what Ptah emits,
// and a document written here could only prove that the reader agrees with its
// own author.
func loadManagedRowDrift(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/drift-managed-rows.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A declared row that drifted is drift the operator has to be able to observe.
// The three categories carry volume and no row: a count of inserts, updates and
// deletes per managed table, with no key, column or value anywhere in the
// document the controller publishes.
func TestDecodeDriftAcceptsManagedRowFindings(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeDrift(loadManagedRowDrift(t), 1)
	if err != nil {
		t.Fatalf("DecodeDrift() rejected a drift report about declared rows: %v", err)
	}
	if !report.Drift || report.HighestSeverity != "destructive" {
		t.Fatalf("Drift = %t, HighestSeverity = %q, want true and destructive", report.Drift, report.HighestSeverity)
	}
	want := map[string]struct {
		count    int32
		severity string
	}{
		"data_rows_inserted": {count: 1, severity: "safe"},
		"data_rows_updated":  {count: 1, severity: "destructive"},
		"data_rows_deleted":  {count: 1, severity: "destructive"},
	}
	if len(report.Findings) != len(want) {
		t.Fatalf("Findings = %d, want %d", len(report.Findings), len(want))
	}
	for _, finding := range report.Findings {
		expected, known := want[finding.Category]
		if !known {
			t.Fatalf("drift report carried an unexpected category %q", finding.Category)
		}
		if finding.Count != expected.count || finding.Severity != expected.severity {
			t.Fatalf("%s = {count: %d, severity: %q}, want {count: %d, severity: %q}",
				finding.Category, finding.Count, finding.Severity, expected.count, expected.severity)
		}
	}
	if _, err := dataplane.DriftReportDigest(report); err != nil {
		t.Fatalf("DriftReportDigest() on a row-drift report: %v", err)
	}
}

// A row drift is a change to what the database holds, and the digest identifies
// an observation. Two observations that differ only in the declared rows are
// two observations, and the digest cannot say so: it is computed from the
// structural diff, which a data-only change leaves alone.
//
// Nothing reads it to detect a change -- the controller requires it to be
// present before planning and compares nothing -- so this records the property
// rather than asserting a defect. A reader who reaches for the digest to answer
// "did the observation change" finds this test first.
func TestDriftReportDigestIgnoresRowDrift(t *testing.T) {
	t.Parallel()

	drifted, err := dataplane.DecodeDrift(loadManagedRowDrift(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	converged := drifted
	converged.Drift = false
	converged.Failed = false
	converged.HighestSeverity = "safe"
	converged.Findings = nil

	driftedDigest, err := dataplane.DriftReportDigest(drifted)
	if err != nil {
		t.Fatal(err)
	}
	convergedDigest, err := dataplane.DriftReportDigest(converged)
	if err != nil {
		t.Fatal(err)
	}
	if driftedDigest != convergedDigest {
		t.Fatalf("the digest separated two observations with the same structural diff: %q and %q",
			driftedDigest, convergedDigest)
	}
}
