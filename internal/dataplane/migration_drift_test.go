package dataplane_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const driftRunStatus = `"status":{"contract_version":2,"current_version":2,"total_migrations":4,"has_pending_changes":true}`

func refusedRun(check string) string {
	return `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","planned":[3,4],` +
		`"drift_check":` + check + `,` + driftRunStatus + `}`
}

// A run document carries the drift check Ptah ran, and the decoder holds the
// check to the shape each outcome has and to the run around it: a check that
// did not come back clean stopped the run before any migration.
func TestDecodeMigrationRunHoldsTheDriftCheckToItsRun(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		document string
		want     *dataplane.MigrationDriftSummary
	}{
		{name: "drifted", document: refusedRun(`{"outcome":"drifted","replayed":[1,2],"findings":[{"kind":"extra_table","object":"audit","message":"m"}]}`),
			want: &dataplane.MigrationDriftSummary{Outcome: "drifted", Findings: 1, Replayed: 2}},
		{name: "unable on the shadow", document: refusedRun(`{"outcome":"unable","stage":"realm-check","reason":"target_shadow_same_realm","message":"m"}`),
			want: &dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "realm-check", Reason: "target_shadow_same_realm"}},
		{name: "unable on coverage, with findings", document: refusedRun(`{"outcome":"unable","stage":"schema-match","reason":"incomplete_coverage","findings":[{"kind":"undecided","message":"m"}]}`),
			want: &dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "schema-match", Reason: "incomplete_coverage", Findings: 1}},
		{name: "clean, then applied", document: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[3],"applied":[3],` +
			`"drift_check":{"outcome":"clean","replayed":[1,2]},"status":{"contract_version":2,"current_version":3,"total_migrations":3,"has_pending_changes":false}}`,
			want: &dataplane.MigrationDriftSummary{Outcome: "clean", Replayed: 2}},
		{name: "clean over a repeatable migration", document: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[3],"applied":[3],` +
			`"drift_check":{"outcome":"clean","replayed":[1,2],"replayed_repeatables":["item_names","order_totals"]},"status":{"contract_version":2,"current_version":3,"total_migrations":3,"has_pending_changes":false}}`,
			want: &dataplane.MigrationDriftSummary{Outcome: "clean", Replayed: 2, ReplayedRepeatables: 2}},
		{name: "no check", document: refusedRun(`null`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeMigrationRun([]byte(row.document))
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			got := report.DriftCheck.Summary()
			if (got == nil) != (row.want == nil) || got != nil && *got != *row.want {
				t.Fatalf("summary = %+v, want %+v", got, row.want)
			}
		})
	}
	for _, row := range []struct{ name, document string }{
		{name: "a drifted check beside an applied run", document: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[3],"applied":[3],` +
			`"drift_check":{"outcome":"drifted","findings":[{"kind":"extra_table","message":"m"}]},"status":{"contract_version":2,"current_version":3,"total_migrations":3,"has_pending_changes":false}}`},
		{name: "an unable check beside a failed statement", document: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","planned":[3],` +
			`"failure":{"version":3,"statement":1,"statements":2},"drift_check":{"outcome":"unable","stage":"claim","reason":"not_clean"},` + driftRunStatus + `}`},
		{name: "an outcome nobody knows", document: refusedRun(`{"outcome":"maybe"}`)},
		{name: "a clean check with a reason", document: refusedRun(`{"outcome":"clean","stage":"claim","reason":"not_clean"}`)},
		{name: "a drifted check with no finding", document: refusedRun(`{"outcome":"drifted"}`)},
		{name: "an unable check with no reason", document: refusedRun(`{"outcome":"unable","stage":"claim"}`)},
		{name: "an unable check whose reason is text", document: refusedRun(`{"outcome":"unable","stage":"claim","reason":"shadow is not empty"}`)},
		{name: "a finding with no kind", document: refusedRun(`{"outcome":"drifted","findings":[{"message":"m"}]}`)},
		{name: "a repeatable migration replayed twice", document: refusedRun(`{"outcome":"drifted","replayed_repeatables":["a","a"],"findings":[{"kind":"extra_table","message":"m"}]}`)},
		{name: "a replayed repeatable migration no file name carries", document: refusedRun(`{"outcome":"drifted","replayed_repeatables":["a b"],"findings":[{"kind":"extra_table","message":"m"}]}`)},
		{name: "the frame's summary", document: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","planned":[3,4],` +
			`"driftSummary":{"outcome":"clean"},` + driftRunStatus + `}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if _, err := dataplane.DecodeMigrationRun([]byte(row.document)); err == nil {
				t.Fatalf("DecodeMigrationRun() accepted %s", row.name)
			}
		})
	}
}

// Refused is true for the two outcomes that stop a run, and for nothing else.
func TestADriftSummaryRefusesOnlyWhenTheCheckWasNotClean(t *testing.T) {
	t.Parallel()
	var none *dataplane.MigrationDriftSummary
	for summary, want := range map[*dataplane.MigrationDriftSummary]bool{
		none:                                     false,
		{Outcome: dataplane.MigrationDriftClean}: false,
		{Outcome: dataplane.MigrationDriftDrifted, Findings: 1}:                  true,
		{Outcome: dataplane.MigrationDriftUnable, Stage: "claim", Reason: "x_y"}: true,
	} {
		if got := summary.Refused(); got != want {
			t.Errorf("%+v Refused() = %t, want %t", summary, got, want)
		}
	}
}

// A summary counts findings from zero up to a bound no schema reaches, 1<<20,
// for every outcome that may carry one.
func TestADriftSummaryCountsFindingsWithinItsBound(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		summary dataplane.MigrationDriftSummary
		valid   bool
	}{
		{summary: dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftDrifted, Findings: 1 << 20}, valid: true},
		{summary: dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftDrifted, Findings: 1<<20 + 1}},
		{summary: dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftDrifted, Findings: -1}},
		{summary: dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftUnable, Stage: "schema-match", Reason: "incomplete_coverage", Findings: 1 << 20}, valid: true},
		{summary: dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftUnable, Stage: "schema-match", Reason: "incomplete_coverage", Findings: 1<<20 + 1}},
		{summary: dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftUnable, Stage: "claim", Reason: "not_clean", Findings: -1}},
	} {
		if err := row.summary.Validate(); (err == nil) != row.valid {
			t.Errorf("%+v Validate() = %v, want valid %t", row.summary, err, row.valid)
		}
	}
}
