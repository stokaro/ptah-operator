package dataplane_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// schemaPlanChanges is the document `ptah schema plan --output <path> --json`
// prints for a plan with changes, with a field no contract version declares
// added at the top and inside the embedded plan.
const schemaPlanChanges = `{
  "contract_version": 2,
  "outcome": "changes",
  "plan_digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "plan_path": "/tmp/ptah-plan-output-1/first.plan.json",
  "plan": {
    "format_version": 1,
    "name": "plan_31a90d35a7bc",
    "dialect": "postgres",
    "statements": [{"sql": "CREATE TABLE t (id bigint)", "severity": "safe", "reason": "new table"}],
    "a_later_field": true
  },
  "a_later_envelope_field": {"nested": [1, 2]}
}
`

func TestDecodeSchemaPlanReadsEachOutcome(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		document string
		check    func(t *testing.T, report dataplane.SchemaPlanReport)
	}{
		{
			name:     "changes, around fields it does not declare",
			document: schemaPlanChanges,
			check: func(t *testing.T, report dataplane.SchemaPlanReport) {
				if report.Outcome != dataplane.SchemaPlanOutcomeChanges || report.PlanDigest != digest ||
					report.PlanPath != "/tmp/ptah-plan-output-1/first.plan.json" {
					t.Fatalf("report = %#v", report)
				}
			},
		},
		{
			name:     "no changes",
			document: `{"contract_version":2,"outcome":"no-changes"}`,
			check: func(t *testing.T, report dataplane.SchemaPlanReport) {
				if report.Outcome != dataplane.SchemaPlanOutcomeNoChanges {
					t.Fatalf("report = %#v", report)
				}
			},
		},
		{
			name: "a fence refusal",
			document: `{"contract_version":2,"outcome":"refused","refusal":{"code":"protected-table",` +
				`"tables":["countries","ref.regions"]},"error":"refusing to change protected table(s) countries, ref.regions"}`,
			check: func(t *testing.T, report dataplane.SchemaPlanReport) {
				if report.Refusal == nil || report.Refusal.Code != dataplane.SchemaRefusalProtectedTable ||
					!slices.Equal(report.Refusal.Tables, []string{"countries", "ref.regions"}) {
					t.Fatalf("refusal = %#v", report.Refusal)
				}
			},
		},
		{
			// A new refusal code does not raise the version, so a document
			// carrying one is read, and the code is left for the caller to
			// treat as a refusal of no particular kind.
			name:     "a refusal code this operator does not know",
			document: `{"contract_version":2,"outcome":"refused","refusal":{"code":"quota-exceeded"},"error":"quota"}`,
			check: func(t *testing.T, report dataplane.SchemaPlanReport) {
				if report.Refusal == nil || report.Refusal.Code != "quota-exceeded" {
					t.Fatalf("refusal = %#v", report.Refusal)
				}
			},
		},
		{
			name:     "a failure",
			document: `{"contract_version":2,"outcome":"failed","error":"connect to --db-url: refused"}`,
			check: func(t *testing.T, report dataplane.SchemaPlanReport) {
				if report.Outcome != dataplane.SchemaPlanOutcomeFailed || report.Refusal != nil {
					t.Fatalf("report = %#v", report)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeSchemaPlan([]byte(test.document))
			if err != nil {
				t.Fatalf("DecodeSchemaPlan() error = %v", err)
			}
			test.check(t, report)
		})
	}
}

// Each row is a document the decoder must refuse. The accepted outcomes above
// are the other half: a decoder that refused everything would pass this table.
func TestDecodeSchemaPlanRefusesWhatItCannotAccountFor(t *testing.T) {
	t.Parallel()

	for name, document := range map[string]string{
		"an older contract version":        strings.Replace(schemaPlanChanges, `"contract_version": 2`, `"contract_version": 0`, 1),
		"the version Ptah v0.13.0 printed": strings.Replace(schemaPlanChanges, `"contract_version": 2`, `"contract_version": 1`, 1),
		"a newer contract version":         strings.Replace(schemaPlanChanges, `"contract_version": 2`, `"contract_version": 3`, 1),
		"no contract version":              `{"outcome":"no-changes"}`,
		"an unknown outcome":               `{"contract_version":2,"outcome":"partial"}`,
		"no outcome":                       `{"contract_version":1}`,
		"changes without a digest":         strings.Replace(schemaPlanChanges, `"plan_digest": "`+digest+`",`, ``, 1),
		"changes with a digest that is not one": strings.Replace(schemaPlanChanges,
			`"plan_digest": "`+digest+`"`, `"plan_digest": "sha256:ABC"`, 1),
		"changes without a path": strings.Replace(schemaPlanChanges,
			`"plan_path": "/tmp/ptah-plan-output-1/first.plan.json",`, ``, 1),
		"no changes naming a saved plan":    `{"contract_version":2,"outcome":"no-changes","plan_digest":"` + digest + `"}`,
		"no changes naming a path":          `{"contract_version":2,"outcome":"no-changes","plan_path":"/tmp/x"}`,
		"a refusal without its reason":      `{"contract_version":2,"outcome":"refused","error":"refused"}`,
		"a refusal with no code":            `{"contract_version":2,"outcome":"refused","refusal":{}}`,
		"a refusal code that is not a word": `{"contract_version":2,"outcome":"refused","refusal":{"code":"stale plan; DROP"}}`,
		"a refusal on a failure":            `{"contract_version":2,"outcome":"failed","refusal":{"code":"stale-plan"}}`,
		"a second JSON value":               schemaPlanChanges + `{"contract_version":2,"outcome":"no-changes"}`,
		"a human line after the document":   schemaPlanChanges + "Plan saved to file:///tmp/x\n",
		"a human line before the document":  "Planned schema changes:\n" + schemaPlanChanges,
		"human text alone":                  "Schema is synced, no changes to be made.\n",
		"nothing at all":                    "",
		"a truncated document":              schemaPlanChanges[:len(schemaPlanChanges)/2],
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if report, err := dataplane.DecodeSchemaPlan([]byte(document)); err == nil {
				t.Fatalf("DecodeSchemaPlan() accepted %s as %#v", name, report)
			}
		})
	}
}

func TestDecodeSchemaApplyReadsEachOutcome(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		document string
		outcome  string
	}{
		{
			name: "applied",
			document: `{"contract_version":2,"outcome":"applied","plan_name":"p","plan_digest":"` + digest +
				`","statements":["CREATE TABLE t (id bigint)"],"later":1}`,
			outcome: dataplane.SchemaApplyOutcomeApplied,
		},
		{
			name: "a dry run",
			document: `{"contract_version":2,"outcome":"dry-run","plan_name":"p","plan_digest":"` + digest +
				`","statements":["CREATE TABLE t (id bigint)"]}`,
			outcome: dataplane.SchemaApplyOutcomeDryRun,
		},
		{
			name: "a plan whose declared rows moved",
			document: `{"contract_version":2,"outcome":"refused","plan_name":"p","plan_digest":"` + digest +
				`","refusal":{"code":"stale-plan","changed":"rows","plan_fingerprint":"` + digest +
				`","database_fingerprint":"` + digest + `"},"error":"stale"}`,
			outcome: dataplane.SchemaApplyOutcomeRefused,
		},
		{
			name:     "an outcome nobody can know",
			document: `{"contract_version":2,"outcome":"unknown","statements":["SELECT 1"],"error":"connection lost"}`,
			outcome:  dataplane.SchemaApplyOutcomeUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeSchemaApply([]byte(test.document))
			if err != nil {
				t.Fatalf("DecodeSchemaApply() error = %v", err)
			}
			if report.Outcome != test.outcome {
				t.Fatalf("outcome = %q, want %q", report.Outcome, test.outcome)
			}
		})
	}
}

func TestDecodeSchemaApplyRefusesWhatItCannotAccountFor(t *testing.T) {
	t.Parallel()

	applied := `{"contract_version":2,"outcome":"applied","plan_name":"p","plan_digest":"` + digest + `"}`
	for name, document := range map[string]string{
		"a newer contract version":     strings.Replace(applied, `"contract_version":2`, `"contract_version":3`, 1),
		"the version v0.13.0 printed":  strings.Replace(applied, `"contract_version":2`, `"contract_version":1`, 1),
		"an unknown outcome":           strings.Replace(applied, `"outcome":"applied"`, `"outcome":"partial"`, 1),
		"a refusal without its reason": `{"contract_version":2,"outcome":"refused"}`,
		"a refusal on a success":       strings.Replace(applied, `"plan_name"`, `"refusal":{"code":"stale-plan"},"plan_name"`, 1),
		"a digest that is not one":     strings.Replace(applied, digest, "sha256:"+strings.Repeat("A", 64), 1),
		"a second JSON value":          applied + "\n" + applied,
		"the human transcript":         "Planned schema changes:\nSELECT 1;\nSchema apply completed successfully.\n",
		"nothing at all":               "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if report, err := dataplane.DecodeSchemaApply([]byte(document)); err == nil {
				t.Fatalf("DecodeSchemaApply() accepted %s as %#v", name, report)
			}
		})
	}
}

// The two fixtures are the documents the Ptah v0.14.0 release image printed for
// one added column on PostgreSQL 17: `schema plan --output /w/plan.json --json`
// and `schema apply --plan /w/plan.json --dry-run --json`, stored as printed.
// v0.13.0 printed version 1 of both; a decoder held only to fixtures written
// here would not have noticed the version move.
func TestDecodeSchemaReportsPtahPrinted(t *testing.T) {
	t.Parallel()

	plan, err := os.ReadFile("testdata/schema-plan-report.json")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := dataplane.DecodeSchemaPlan(plan)
	if err != nil {
		t.Fatalf("DecodeSchemaPlan() refused what Ptah printed: %v", err)
	}
	if planned.Outcome != dataplane.SchemaPlanOutcomeChanges || planned.PlanPath != "/w/plan.json" {
		t.Fatalf("plan report = %+v", planned)
	}
	dryRun, err := os.ReadFile("testdata/schema-apply-dry-run-report.json")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := dataplane.DecodeSchemaApply(dryRun)
	if err != nil {
		t.Fatalf("DecodeSchemaApply() refused what Ptah printed: %v", err)
	}
	if applied.Outcome != dataplane.SchemaApplyOutcomeDryRun || applied.PlanDigest != planned.PlanDigest || len(applied.Statements) != 1 {
		t.Fatalf("dry-run report = %+v, want the plan %s listed", applied, planned.PlanDigest)
	}
}
