package dataplane_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The plans Ptah saved for five changes to a partition declared in SQL, as the
// runner harness captured them from the pinned release. Since v0.26.0 Ptah
// rates a detachment that takes the partition's rows out of its parent
// destructive itself (stokaro/ptah#5741), where v0.23.0 rated it a warning and
// the operator raised it; the bound changes and the attachment stay warnings.
// Each row holds the capture to Ptah's own ratings before it reads the
// operator's, so a rating Ptah moves fails here rather than passing on the
// operator's raise.
func TestDecodePlanRatesPartitionMoves(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		file string
		// rated is the severity Ptah saved for each statement, and raised the
		// severity the operator reads it as.
		rated, raised []string
	}{
		{file: "differing/plan-partition-bound.json", rated: []string{"warning", "warning"}, raised: []string{"warning", "warning"}},
		{file: "differing/plan-partition-bound-excludes-row.json", rated: []string{"warning", "warning"}, raised: []string{"warning", "warning"}},
		{file: "differing/plan-partition-attach.json", rated: []string{"warning"}, raised: []string{"warning"}},
		{file: "differing/plan-partition-detach.json", rated: []string{"destructive"}, raised: []string{"destructive"}},
		{file: "differing/plan-partition-parent.json", rated: []string{"destructive", "warning"}, raised: []string{"destructive", "warning"}},
	} {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			data := loadCapture(t, row.file)
			var saved struct {
				Destructive bool `json:"destructive"`
				Statements  []struct {
					Severity string `json:"severity"`
				} `json:"statements"`
			}
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			var rated []string
			for _, statement := range saved.Statements {
				rated = append(rated, statement.Severity)
			}
			if !slices.Equal(rated, row.rated) || saved.Destructive != slices.Contains(row.rated, "destructive") {
				t.Fatalf("Ptah rated the statements %q, plan destructive=%t; the capture no longer measures the ratings %q",
					rated, saved.Destructive, row.rated)
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
