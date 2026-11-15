package e2e

import (
	"os"
	"slices"
	"strings"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// What the schema apply log readings were given. See
// testdata/e2e/readings/schema-apply-log.md for how each was taken.
const (
	schemaReadingReferenceV1 = "sha256:9736d8870b28be7ed18bca37f8c9dc8986137ab1f85d4b3f8ec9f8c29f1b7916"
	schemaReadingReferenceV2 = "sha256:6207f4eb99a137c035a873d29b55b91bc1088fd25ae94954883b2664d778409f"
	schemaReadingApprover    = "kubernetes-admin"
)

// The digests of the plans Ptah wrote from testdata/e2e/<engine>-v1.sql and
// -v2.sql, as the apply documents and the log report them.
var (
	schemaReadingPlanV1 = map[string]string{
		"postgresql": "sha256:7fdab1f6673273a2b923f3a3a9bbeacdb78855aca6e46fcdf8cfbb6a025954ca",
		"mysql":      "sha256:af6d45fe3995e355fdb8f54e9601f521652f417ed0d529fe2278709eba6889d6",
	}
	schemaReadingPlanV2 = map[string]string{
		"postgresql": "sha256:a62b3791fa07e943ab8b6fd4d84a53d46d0c2c5892e9ddbe753bfc41bd2ee47f",
		"mysql":      "sha256:7730dcc98589fb2aada26d0fb9b68d8fcd6f71376c03bdea114a727a3ad83a69",
	}
)

// approvedSchemaReadingRun is what the first apply of each reading was given,
// and what the acceptance row asks of an Apply a person approved.
var approvedSchemaReadingRun = loggedRun{actor: schemaReadingApprover, origin: actorOriginAdmission, reference: schemaReadingReferenceV1}

// readSchemaApplyLog parses a reading and holds it to the number of entries
// the applies that wrote it write.
func readSchemaApplyLog(t *testing.T, engine, kind string) []schemaApplyLogRow {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/schema-apply-log-" + engine + "-" + kind + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseSchemaApplyLog(string(body))
	if err != nil {
		t.Fatalf("parse the %s %s reading: %v", engine, kind, err)
	}
	want := 2
	if kind == "two-runs" {
		want = 4
	}
	if len(rows) != want {
		t.Fatalf("the %s %s reading has %d entries, and its applies write %d", engine, kind, len(rows), want)
	}
	return rows
}

func TestApplyLoggedAsAcceptsTheApplyPtahWrote(t *testing.T) {
	t.Parallel()

	second := loggedRun{actor: schemaReadingApprover, origin: actorOriginAdmission, reference: schemaReadingReferenceV2}
	for _, engine := range []string{"postgresql", "mysql"} {
		for _, row := range []struct {
			name, kind string
			run        loggedRun
			plan       string
		}{
			{"the only apply", "approved", approvedSchemaReadingRun, schemaReadingPlanV1[engine]},
			// A database keeps the log of every apply it was sent; the row
			// passes over the entries of another.
			{"the first of two applies", "two-runs", approvedSchemaReadingRun, schemaReadingPlanV1[engine]},
			{"the second of two applies", "two-runs", second, schemaReadingPlanV2[engine]},
		} {
			t.Run(engine+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				if err := applyLoggedAs(readSchemaApplyLog(t, engine, row.kind), row.run, row.plan); err != nil {
					t.Fatalf("the row refused the apply Ptah wrote: %v", err)
				}
			})
		}
	}
}

func TestApplyLoggedAsRefusesALogThatDoesNotNameTheApply(t *testing.T) {
	t.Parallel()

	for _, engine := range []string{"postgresql", "mysql"} {
		approved := func(t *testing.T) []schemaApplyLogRow { return readSchemaApplyLog(t, engine, "approved") }
		with := func(change func(*loggedRun)) loggedRun {
			run := approvedSchemaReadingRun
			change(&run)
			return run
		}
		planV1 := schemaReadingPlanV1[engine]
		for _, row := range []struct {
			name string
			rows func(*testing.T) []schemaApplyLogRow
			run  loggedRun
			plan string
			says string
		}{
			{
				// What every operator Apply wrote before this row existed:
				// neither a name nor a reference, so nothing joins it to the
				// journal.
				name: "an apply given nothing", rows: func(t *testing.T) []schemaApplyLogRow { return readSchemaApplyLog(t, engine, "no-attribution") },
				run: approvedSchemaReadingRun, plan: planV1, says: "holds 0 entries under run reference",
			},
			{
				// The reference of one apply and the plan of another.
				name: "the second apply's reference and the first plan", rows: func(t *testing.T) []schemaApplyLogRow { return readSchemaApplyLog(t, engine, "two-runs") },
				run: with(func(r *loggedRun) { r.reference = schemaReadingReferenceV2 }), plan: planV1, says: "names plan digest",
			},
			{
				name: "another operation's reference", rows: approved,
				run: with(func(r *loggedRun) { r.reference = migrationReadingReference }), plan: planV1, says: "holds 0 entries under run reference",
			},
			{
				name: "someone else's name", rows: approved,
				run: with(func(r *loggedRun) { r.actor = "someone-else" }), plan: planV1, says: `want "someone-else"`,
			},
			{
				name: "a policy's name where a person approved", rows: approved,
				run: with(func(r *loggedRun) { r.actor = alwaysPolicyActor("team-a", "orders") }), plan: planV1, says: "system:ptah-operator:apply-always",
			},
			{
				name: "the policy's origin where a person approved", rows: approved,
				run: with(func(r *loggedRun) { r.origin = actorOriginPolicy }), plan: planV1, says: `records origin "admission", want "policy"`,
			},
			{
				name: "another plan", rows: approved,
				run: approvedSchemaReadingRun, plan: schemaReadingPlanV2[engine], says: "names plan digest",
			},
			{
				name: "an empty log", rows: func(*testing.T) []schemaApplyLogRow { return nil },
				run: approvedSchemaReadingRun, plan: planV1, says: "holds no entry",
			},
			{
				// Ptah drops an entry the database refuses and carries on.
				name: "an apply that lost its outcome", rows: func(t *testing.T) []schemaApplyLogRow { return approved(t)[:1] },
				run: approvedSchemaReadingRun, plan: planV1, says: "holds 1 entries under run reference",
			},
			{
				name: "an entry written twice", rows: func(t *testing.T) []schemaApplyLogRow { rows := approved(t); return append(rows, rows[1]) },
				run: approvedSchemaReadingRun, plan: planV1, says: "holds 3 entries under run reference",
			},
			{
				name: "no name to look for", rows: approved,
				run: with(func(r *loggedRun) { r.actor = "" }), plan: planV1, says: "no name to look for",
			},
			{
				name: "no origin to look for", rows: approved,
				run: with(func(r *loggedRun) { r.origin = "" }), plan: planV1, says: "no origin to look for",
			},
			{
				// The no-attribution entries carry no reference either, so a
				// row that looked for none would find them.
				name: "no run reference to look for", rows: func(t *testing.T) []schemaApplyLogRow { return readSchemaApplyLog(t, engine, "no-attribution") },
				run: with(func(r *loggedRun) { r.reference = "" }), plan: planV1, says: "no run reference to look for",
			},
			{
				name: "no plan to look for", rows: approved,
				run: approvedSchemaReadingRun, says: "no plan digest to look for",
			},
		} {
			t.Run(engine+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				err := applyLoggedAs(row.rows(t), row.run, row.plan)
				if err == nil {
					t.Fatal("the row accepted it")
				}
				if !strings.Contains(err.Error(), row.says) {
					t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
				}
			})
		}
	}
}

// Each field on its own, on an entry that is otherwise the approved one, so a
// refusal cannot come from something else being wrong with it.
func TestApplyLoggedAsRefusesEachFieldOfAnEntry(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		change func(*schemaApplyLogRow)
		says   string
	}{
		{"a failed apply", func(r *schemaApplyLogRow) { r.State = "failed" }, "entry 1 is 2 failed, want 2 applied"},
		{"an apply of unknown outcome", func(r *schemaApplyLogRow) { r.State = "unknown" }, "entry 1 is 2 unknown, want 2 applied"},
		{"an entry past the outcome", func(r *schemaApplyLogRow) { r.Seq = 3 }, "entry 1 is 3 applied"},
		{"an entry of another run under the same reference", func(r *schemaApplyLogRow) { r.RunID = "9-another" }, `belongs to run "9-another"`},
		{"an unknown source", func(r *schemaApplyLogRow) { r.ActorSource = "unknown" }, `has source "unknown"`},
		{"a process-user source", func(r *schemaApplyLogRow) { r.ActorSource = "process-user" }, `has source "process-user"`},
		{"a name that was not encoded", func(r *schemaApplyLogRow) { r.Actor = "José" }, "not printable ASCII"},
		{"no origin", func(r *schemaApplyLogRow) { r.ActorOrigin = "" }, `entry 1 (applied) records origin ""`},
		{"another plan", func(r *schemaApplyLogRow) { r.PlanDigest = "sha256:another" }, `names plan digest "sha256:another"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rows := readSchemaApplyLog(t, "postgresql", "approved")
			row.change(&rows[1])
			err := applyLoggedAs(rows, approvedSchemaReadingRun, schemaReadingPlanV1["postgresql"])
			if err == nil {
				t.Fatal("the row accepted it")
			}
			if !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
			}
		})
	}
}

// Entries that agree on no run agree with each other, so the row refuses the
// empty run as well as a second one.
func TestApplyLoggedAsRefusesEntriesOfNoRun(t *testing.T) {
	t.Parallel()

	rows := readSchemaApplyLog(t, "postgresql", "approved")
	for index := range rows {
		rows[index].RunID = ""
	}
	err := applyLoggedAs(rows, approvedSchemaReadingRun, schemaReadingPlanV1["postgresql"])
	if err == nil || !strings.Contains(err.Error(), `belongs to run ""`) {
		t.Fatalf("applyLoggedAs() = %v, want a refusal naming the empty run", err)
	}
}

// The run's sequence numbers order its entries, so two entries the clock
// stamped in the other order still read as one apply.
func TestApplyLoggedAsOrdersAnApplyBySequence(t *testing.T) {
	t.Parallel()

	rows := readSchemaApplyLog(t, "mysql", "approved")
	slices.Reverse(rows)
	if err := applyLoggedAs(rows, approvedSchemaReadingRun, schemaReadingPlanV1["mysql"]); err != nil {
		t.Fatalf("the row refused the apply read in reverse: %v", err)
	}
}

func TestParseSchemaApplyLogRefusesWhatTheQueryDoesNotPrint(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		output string
		says   string
	}{
		{"a line with no fields", "ERROR: relation \"ptah_schema_apply_log\" does not exist\n", "is not run:seq:state:source:name:origin:reference:plan"},
		// A line of the migration log's query, which prints one field fewer.
		{"a line of the migration log", "7-abc:1:started:provided:6f:61:62\n", "is not run:seq:state:source:name:origin:reference:plan"},
		{"a sequence that is not a number", "7-abc:one:started:provided:6f:61:62:63\n", "names no sequence number"},
		{"a name that is not hex", "7-abc:1:started:provided:oidc:61:62:63\n", "carries a field that is not hex"},
		{"a plan digest that is not hex", "7-abc:1:started:provided:6f:61:62:sha256\n", "carries a field that is not hex"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSchemaApplyLog(row.output)
			if err == nil {
				t.Fatal("the parser accepted it")
			}
			if !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
			}
		})
	}
	if rows, err := parseSchemaApplyLog(""); err != nil || len(rows) != 0 {
		t.Fatalf("an empty table reads as %v, %v; want no rows, so the row refuses it as empty", rows, err)
	}
}

// The query is engine SQL the row cannot run here, so what it asks each engine
// is pinned to the query the readings were taken with.
func TestSchemaApplyLogQueryIsTheOneTheReadingsWereTakenWith(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../testdata/e2e/readings/schema-apply-log.md")
	if err != nil {
		t.Fatal(err)
	}
	for engine, hexOf := range map[string]func(string) string{
		"postgresql": func(column string) string { return "encode(convert_to(coalesce(" + column + ", ''), 'UTF8'), 'hex')" },
		"mysql":      func(column string) string { return "hex(coalesce(" + column + ", ''))" },
	} {
		query := schemaApplyLogQuery(engine)
		for _, want := range []string{
			hexOf("actor"), hexOf("actor_origin"), hexOf("run_reference"), hexOf("plan_digest"),
			"FROM ptah_schema_apply_log ORDER BY logged_at, run_id, seq",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("%s query = %q, want it to hold %s", engine, query, want)
			}
		}
		if !strings.Contains(string(body), query) {
			t.Errorf("the reading's provenance does not record the %s query the row runs", engine)
		}
	}
}

// unencodedInTheLog is the row's promise that the log holds the stamp as it
// is, and the operator's encoder has to keep it: every name the row accepts
// reaches the log unchanged, and every name it refuses would not.
func TestUnencodedInTheLogIsWhatTheOperatorLeavesAlone(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		username string
		want     bool
	}{
		{schemaReadingApprover, true},
		{"system:serviceaccount:team-a:deployer", true},
		{"denis+ptah@example.test", true},
		{"~", true},
		{"a b", true},
		{strings.Repeat("a", 256), true},
		{"", false},
		{" ", false},
		{" leading", false},
		{"trailing ", false},
		{strings.Repeat("a", 257), false},
		{loggedPerson, false},
		{"José", false},
		{"a%41", false},
		{"$(HOME)", false},
		{"a\x7fb", false},
		{"a\tb", false},
	} {
		t.Run(row.username, func(t *testing.T) {
			t.Parallel()
			if got := unencodedInTheLog(row.username); got != row.want {
				t.Fatalf("unencodedInTheLog(%q) = %t, want %t", row.username, got, row.want)
			}
			if strings.TrimSpace(row.username) == "" {
				return
			}
			schema := &ptahv1alpha1.PtahSchema{}
			schema.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{Approval: &ptahv1alpha1.ConsumedApprovalStatus{
				Approver: ptahv1alpha1.ApprovalIdentity{Username: row.username},
			}}
			actor, origin := workload.SchemaApplyActor(schema)
			if origin != actorOriginAdmission {
				t.Fatalf("the operator records origin %q for an approval, the row expects %q", origin, actorOriginAdmission)
			}
			if (actor == row.username) != row.want {
				t.Fatalf("the operator writes %q as %q, and the row says it is left alone: %t", row.username, actor, row.want)
			}
		})
	}
}
