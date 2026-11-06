package e2e

import (
	"os"
	"strings"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// readMigrationLog parses a reading Ptah wrote. See
// testdata/e2e/readings/migration-log.md for how each was taken.
func readMigrationLog(t *testing.T, engine, kind string) []migrationLogRow {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/migration-log-" + engine + "-" + kind + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseMigrationLog(string(body))
	if err != nil {
		t.Fatalf("parse the %s %s reading: %v", engine, kind, err)
	}
	if len(rows) != 6 {
		t.Fatalf("the %s %s reading has %d rows, and the fixture's three versions write 6", engine, kind, len(rows))
	}
	return rows
}

// The two engines print the query differently, one in lowercase hex and one in
// upper, and the row has to read the same name out of both.
func TestRunLogNamesAcceptsTheRunPtahWrote(t *testing.T) {
	t.Parallel()

	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()
			if err := runLogNames(readMigrationLog(t, engine, "approved"), loggedPersonInTheLog, 1, 2, 3); err != nil {
				t.Fatalf("the row refused the run Ptah wrote: %v", err)
			}
		})
	}
}

func TestRunLogNamesRefusesALogThatDoesNotNameTheApprover(t *testing.T) {
	t.Parallel()

	for _, engine := range []string{"postgresql", "mysql"} {
		approved := func(t *testing.T) []migrationLogRow { return readMigrationLog(t, engine, "approved") }
		for _, row := range []struct {
			name     string
			rows     func(*testing.T) []migrationLogRow
			actor    string
			versions []int64
			says     string
		}{
			{
				// What every operator run wrote before this row existed.
				name: "a run given no name", rows: func(t *testing.T) []migrationLogRow { return readMigrationLog(t, engine, "no-actor") },
				actor: loggedPersonInTheLog, versions: []int64{1, 2, 3}, says: `names nobody, with source "unknown"`,
			},
			{
				// A name, and not an approver's: the user the process ran as.
				name: "a run that fell back to the process user", rows: func(t *testing.T) []migrationLogRow { return readMigrationLog(t, engine, "process-user") },
				actor: "ptah", versions: []int64{1, 2, 3}, says: `has source "process-user"`,
			},
			{
				// A name that was not encoded, which a database in a narrower
				// character set would have refused, losing every row.
				name: "a name outside printable ASCII", rows: func(t *testing.T) []migrationLogRow { return readMigrationLog(t, engine, "unencoded") },
				actor: loggedPerson, versions: []int64{1, 2, 3}, says: "not printable ASCII",
			},
			{
				name: "someone else's name", rows: approved,
				actor: "someone-else", versions: []int64{1, 2, 3}, says: `want "someone-else"`,
			},
			{
				// The approver, but not as the log writes one.
				name: "the name before it was encoded", rows: approved,
				actor: loggedPerson, versions: []int64{1, 2, 3}, says: `want "oidc:José García $(HOME)"`,
			},
			{
				name: "a policy's name where a person approved", rows: approved,
				actor: alwaysPolicyActor("team-a", "orders"), versions: []int64{1, 2, 3}, says: "system:ptah-operator:apply-always",
			},
			{
				name: "an empty log", rows: func(*testing.T) []migrationLogRow { return nil },
				actor: loggedPersonInTheLog, versions: []int64{1, 2, 3}, says: "holds 0 rows",
			},
			{
				// Ptah drops a row the database refuses and carries on.
				name: "a run that lost a row", rows: func(t *testing.T) []migrationLogRow { return approved(t)[:5] },
				actor: loggedPersonInTheLog, versions: []int64{1, 2, 3}, says: "holds 5 rows",
			},
			{
				name: "a run of other versions", rows: approved,
				actor: loggedPersonInTheLog, versions: []int64{1, 2, 4}, says: "want version 4",
			},
			{
				name: "no name to look for", rows: approved,
				actor: "", versions: []int64{1, 2, 3}, says: "no name to look for",
			},
			{
				name: "no version to look for", rows: approved,
				actor: loggedPersonInTheLog, says: "no version to look for",
			},
		} {
			t.Run(engine+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				err := runLogNames(row.rows(t), row.actor, row.versions...)
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

// Each field on its own, on a row that is otherwise the approved one, so a
// refusal cannot come from something else being wrong with it.
func TestRunLogNamesRefusesEachFieldOfARow(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		change func(*migrationLogRow)
		says   string
	}{
		{"an empty actor", func(r *migrationLogRow) { r.Actor = "" }, "names nobody"},
		{"an unknown source", func(r *migrationLogRow) { r.ActorSource = "unknown" }, `has source "unknown"`},
		{"a process-user source", func(r *migrationLogRow) { r.ActorSource = "process-user" }, `has source "process-user"`},
		{"a failed migration", func(r *migrationLogRow) { r.State = "failed" }, "want version 1 applied"},
		{"a row of another run under the same name", func(r *migrationLogRow) { r.RunID = "9-another" }, `belongs to run "9-another"`},
		{"a row of no run", func(r *migrationLogRow) { r.RunID = "" }, `belongs to run ""`},
		{"another version", func(r *migrationLogRow) { r.Version = 9 }, "row 1 is version 9"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rows := readMigrationLog(t, "postgresql", "approved")
			row.change(&rows[1])
			err := runLogNames(rows, loggedPersonInTheLog, 1, 2, 3)
			if err == nil {
				t.Fatal("the row accepted it")
			}
			if !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
			}
		})
	}
}

// Rows that agree on no run at all agree with each other, so the row has to refuse
// the empty run as well as a second one.
func TestRunLogNamesRefusesALogThatNamesNoRun(t *testing.T) {
	t.Parallel()

	rows := readMigrationLog(t, "postgresql", "approved")
	for index := range rows {
		rows[index].RunID = ""
	}
	err := runLogNames(rows, loggedPersonInTheLog, 1, 2, 3)
	if err == nil {
		t.Fatal("the row accepted a log with no run on any row")
	}
	if !strings.Contains(err.Error(), `belongs to run ""`) {
		t.Fatalf("the refusal says %q, want it to name the empty run", err)
	}
}

func TestParseMigrationLogRefusesWhatTheQueryDoesNotPrint(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		output string
		says   string
	}{
		{"a line with no fields", "ERROR: relation \"schema_migrations_log\" does not exist\n", "is not run:version:state:source:name"},
		{"a name that is not hex", "7-abc:1:started:provided:oidc\n", "carries no hex name"},
		{"a version that is not a number", "7-abc:one:started:provided:6f\n", "names no version"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseMigrationLog(row.output)
			if err == nil {
				t.Fatal("the parser accepted it")
			}
			if !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
			}
		})
	}
	if rows, err := parseMigrationLog(""); err != nil || len(rows) != 0 {
		t.Fatalf("an empty table reads as %v, %v; want no rows, so the row's own count refuses it", rows, err)
	}
}

// The query is engine SQL the row cannot run here, so what it asks each engine
// is pinned to the query the readings were taken with.
func TestMigrationLogQueryIsTheOneTheReadingsWereTakenWith(t *testing.T) {
	t.Parallel()

	for engine, want := range map[string]string{
		"postgresql": "encode(convert_to(coalesce(actor, ''), 'UTF8'), 'hex')",
		"mysql":      "hex(coalesce(actor, ''))",
	} {
		selected, err := migrationEngineFor(engine)
		if err != nil {
			t.Fatal(err)
		}
		query := selected.migrationLogQuery()
		if !strings.Contains(query, want) || !strings.Contains(query, "FROM schema_migrations_log ORDER BY seq") {
			t.Errorf("%s query = %q, want it to read the name as %s", engine, query, want)
		}
	}
	body, err := os.ReadFile("../../testdata/e2e/readings/migration-log.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"postgresql", "mysql"} {
		selected, err := migrationEngineFor(engine)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), selected.migrationLogQuery()) {
			t.Errorf("the reading's provenance does not record the %s query the row runs", engine)
		}
	}
}

// The row approves as one string and reads another out of the database, and a
// typo in either would cost a phase to find. The operator's own encoder holds the
// two to one value here. A $ surviving it is what the kubelet would expand.
func TestTheLoggedPersonIsTheOperatorsEncodingOfTheApprover(t *testing.T) {
	t.Parallel()

	approval := &ptahv1alpha1.PtahMigrationApproval{}
	approval.Spec.Approver.Username = loggedPerson
	if got := workload.MigrationActor(&ptahv1alpha1.PtahMigration{}, approval); got != loggedPersonInTheLog {
		t.Fatalf("the operator encodes %q as %q, and the row expects %q", loggedPerson, got, loggedPersonInTheLog)
	}
	if !strings.Contains(loggedPerson, "$(HOME)") || strings.Contains(loggedPersonInTheLog, "$") {
		t.Fatalf("the row is meant to send a variable reference through the kubelet: %q, %q", loggedPerson, loggedPersonInTheLog)
	}
	if err := loggedNameDecodesTo(loggedPersonInTheLog, loggedPerson); err != nil {
		t.Fatal(err)
	}
}

// The range is the one the operator leaves unencoded, and its edges are the
// mistakes an off-by-one would make.
func TestPrintableASCIIIsTheRangeTheOperatorLeavesUnencoded(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		value string
		want  bool
	}{
		{"a space, the first printable byte", " ", true},
		{"a tilde, the last printable byte", "~", true},
		{"an encoded name", loggedPersonInTheLog, true},
		{"the byte before the range", "\x1f", false},
		{"DEL, the byte after the range", "\x7f", false},
		{"the first byte over ASCII", "\x80", false},
		{"a NUL", "\x00", false},
		{"a name that was not encoded", loggedPerson, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := printableASCII(row.value); got != row.want {
				t.Fatalf("printableASCII(%q) = %t, want %t", row.value, got, row.want)
			}
		})
	}
}

// What the log holds has to decode back to the stamp, or the row proves a name
// nobody can read back. Every refusal is a mistake a decoder or an encoder makes.
func TestLoggedNameDecodesToTheStamp(t *testing.T) {
	t.Parallel()

	if err := loggedNameDecodesTo(loggedPersonInTheLog, loggedPerson); err != nil {
		t.Fatalf("the approver's name was refused: %v", err)
	}
	if err := loggedNameDecodesTo("denis+ptah@example.test", "denis+ptah@example.test"); err != nil {
		t.Fatalf("a plus has to stay a plus: %v", err)
	}
	for _, row := range []struct {
		name            string
		logged, stamped string
		says            string
	}{
		{"another person", loggedPersonInTheLog, "someone-else", "decodes to"},
		{"an escape cut in half", "oidc:Jos%C3%A", loggedPerson, "does not decode"},
		{"a name encoded twice", "oidc:Jos%25C3%25A9 Garc%25C3%25ADa", loggedPerson, "decodes to"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := loggedNameDecodesTo(row.logged, row.stamped)
			if err == nil {
				t.Fatal("the row accepted it")
			}
			if !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the refusal says %q, want it to say %q", err, row.says)
			}
		})
	}
}

func TestAlwaysPolicyActorSaysNoPersonApproved(t *testing.T) {
	t.Parallel()

	if got, want := alwaysPolicyActor("team-a", "orders"), "system:ptah-operator:apply-always:team-a/orders"; got != want {
		t.Fatalf("alwaysPolicyActor() = %q, want %q", got, want)
	}
}
