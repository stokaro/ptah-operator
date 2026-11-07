package dataplane_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The plans Ptah f47b2bfa3 saved for five changes to a partition declared in
// SQL rate every statement a warning. The operator holds the two that take the
// partition's rows out of the parent, and leaves the bound changes and the
// attachment as Ptah rated them.
func TestDecodePlanRatesPtahV023PartitionMoves(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		file string
		// raised is the severity each statement reads as.
		raised []string
	}{
		{file: "plan-partition-bound.json", raised: []string{"warning", "warning"}},
		{file: "plan-partition-bound-excludes-row.json", raised: []string{"warning", "warning"}},
		{file: "plan-partition-attach.json", raised: []string{"warning"}},
		{file: "plan-partition-detach.json", raised: []string{"destructive"}},
		{file: "plan-partition-parent.json", raised: []string{"destructive", "warning"}},
	} {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			data := loadPtahV023(t, row.file)
			var saved struct {
				Destructive bool `json:"destructive"`
				Statements  []struct {
					Severity string `json:"severity"`
				} `json:"statements"`
			}
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			for index, statement := range saved.Statements {
				if saved.Destructive || statement.Severity != "warning" {
					t.Fatalf("Ptah rated statement %d %q, plan destructive=%t; the capture no longer measures the warning rating",
						index, statement.Severity, saved.Destructive)
				}
			}
			plan, err := dataplane.DecodePlan(data, "postgres")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			var got []string
			for _, statement := range plan.Statements {
				got = append(got, statement.Severity)
			}
			if !slices.Equal(got, row.raised) {
				t.Fatalf("severities = %q, want %q", got, row.raised)
			}
			if want := slices.Contains(row.raised, "destructive"); plan.Destructive != want {
				t.Fatalf("destructive = %t, want %t", plan.Destructive, want)
			}
		})
	}
}

// A detachment is left as Ptah rated it only when the next move of the same
// table attaches it to the same parent with nothing renamed in between, in the
// shape Ptah renders. Every near miss of that is destructive.
func TestDecodePlanPairsADetachmentOnlyWithItsReattachment(t *testing.T) {
	t.Parallel()

	const (
		detach = `ALTER TABLE "events" DETACH PARTITION "e1"`
		attach = `ALTER TABLE "events" ATTACH PARTITION "e1" FOR VALUES FROM ('2026-01-01') TO ('2028-01-01')`
	)
	for _, row := range []struct {
		name       string
		dialect    string
		statements []string
		// destructive lists the statements the operator raises.
		destructive []int
	}{
		{name: "a bound change", statements: []string{detach, attach}},
		{name: "a bound change with the semicolon Ptah renders",
			statements: []string{detach + ";", attach + ";"}},
		{name: "a bound change spelled in another case and unquoted",
			statements: []string{`alter table Events detach partition E1`, `ALTER TABLE events ATTACH PARTITION e1 DEFAULT`}},
		{name: "a bound change under a schema",
			statements: []string{`ALTER TABLE "app"."events" DETACH PARTITION "app"."e1"`,
				`ALTER TABLE app.events ATTACH PARTITION "app".e1 FOR VALUES IN (1)`}},
		{name: "a bound change with another statement between",
			statements: []string{detach, `ALTER TABLE "e1" ADD COLUMN "note" text`, attach}},
		{name: "two partitions trading bounds",
			statements: []string{detach, `ALTER TABLE "events" DETACH PARTITION "e2"`, attach,
				`ALTER TABLE "events" ATTACH PARTITION "e2" FOR VALUES FROM ('2028-01-01') TO ('2029-01-01')`}},

		{name: "a detachment alone", statements: []string{detach}, destructive: []int{0}},
		{name: "a reattachment under another parent",
			statements: []string{detach, `ALTER TABLE "other" ATTACH PARTITION "e1" FOR VALUES IN (1)`}, destructive: []int{0}},
		{name: "a reattachment under a parent with the schema named",
			statements: []string{detach, `ALTER TABLE "public"."events" ATTACH PARTITION "e1" DEFAULT`}, destructive: []int{0}},
		{name: "a parent whose quoted name differs in case",
			statements: []string{detach, `ALTER TABLE "Events" ATTACH PARTITION "e1" DEFAULT`}, destructive: []int{0}},
		{name: "an attachment of another table",
			statements: []string{detach, `ALTER TABLE "events" ATTACH PARTITION "e2" DEFAULT`}, destructive: []int{0}},
		{name: "an attachment before the detachment",
			statements: []string{attach, detach}, destructive: []int{1}},
		{name: "another detachment of the table first",
			statements: []string{detach, `ALTER TABLE "other" DETACH PARTITION "e1"`, attach}, destructive: []int{0, 1}},
		{name: "a rename between",
			statements: []string{detach, `ALTER TABLE "e9" RENAME TO "e1"`, attach}, destructive: []int{0}},
		{name: "a concurrent detachment",
			statements: []string{detach + " CONCURRENTLY", attach}, destructive: []int{0}},
		{name: "a finalized detachment",
			statements: []string{detach + " FINALIZE", attach}, destructive: []int{0}},
		{name: "a detachment with a comment",
			statements: []string{`ALTER TABLE "events" /* move */ DETACH PARTITION "e1"`, attach}, destructive: []int{0}},
		{name: "a detachment that runs a second statement",
			statements: []string{detach + `; ALTER TABLE "events" DETACH PARTITION "e2"`, attach}, destructive: []int{0}},
		{name: "an attachment that hides a detachment",
			statements: []string{detach, attach + `; ALTER TABLE "events" DETACH PARTITION "e1"`}, destructive: []int{1}},
		{name: "an attachment with no bound",
			statements: []string{detach, `ALTER TABLE "events" ATTACH PARTITION "e1"`}, destructive: []int{0}},
		{name: "a quoted name with a dot inside",
			statements:  []string{`ALTER TABLE "app.events" DETACH PARTITION "e1"`, `ALTER TABLE "app"."events" ATTACH PARTITION "e1" DEFAULT`},
			destructive: []int{0}},
		{name: "a quoted name with a doubled quote inside",
			statements:  []string{`ALTER TABLE "a"".""b" DETACH PARTITION "e1"`, `ALTER TABLE "a"."b" ATTACH PARTITION "e1" DEFAULT`},
			destructive: []int{0}},
		{name: "a detachment on MySQL", dialect: "mysql",
			statements:  []string{"ALTER TABLE `events` DETACH PARTITION `e1`", "ALTER TABLE `events` ATTACH PARTITION `e1`"},
			destructive: []int{0}},
		{name: "a detachment on MySQL in ANSI quotes", dialect: "mysql",
			statements: []string{detach, attach}, destructive: []int{0}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dialect := row.dialect
			if dialect == "" {
				dialect = "postgres"
			}
			var statements []string
			for _, sql := range row.statements {
				statements = append(statements, `{"sql":`+strconv.Quote(sql)+`,"severity":"warning","reason":"rated by Ptah"}`)
			}
			document := `{"format_version":1,"name":"p","dialect":` + strconv.Quote(dialect) +
				`,"from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[` +
				strings.Join(statements, ",") + `]}`
			plan, err := dataplane.DecodePlan([]byte(document), dialect)
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			var raised []int
			for index, statement := range plan.Statements {
				if statement.Severity == "destructive" {
					raised = append(raised, index)
				}
			}
			if !slices.Equal(raised, row.destructive) {
				t.Fatalf("destructive statements = %v, want %v", raised, row.destructive)
			}
			if plan.Destructive != (len(row.destructive) > 0) {
				t.Fatalf("plan destructive = %t with statements %v raised", plan.Destructive, raised)
			}
		})
	}
}
