package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// statementErrorDocument is what the pinned Ptah image printed when the database
// refused a statement, as internal/dataplane/testdata/statement-errors holds it.
func statementErrorDocument(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "statement-errors", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runRefusedStatement runs one Apply on a captured document and returns the
// result, the Pod log it wrote, and what crosses to the manager.
func runRefusedStatement(t *testing.T, run targetFailureRun, stdout string) (Result, string, EncodedResult) {
	t.Helper()
	diagnostics := &bytes.Buffer{}
	result := Run(context.Background(), Config{
		Operation:   run.operation,
		Environment: withRunnerProtocol(run.environment(t)),
		Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: stdout, exitCode: 2}}},
		Diagnostics: diagnostics,
		TempDir:     t.TempDir(),
	})
	encoded, err := EncodeResult(result)
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	if encoded.SummaryErr != nil {
		t.Fatalf("the result has no termination summary: %v", encoded.SummaryErr)
	}
	return result, diagnostics.String(), encoded
}

// TestARefusedStatementLeavesTheDatabasesAnswerInThePodLog runs each captured
// refusal through the runner. The reason is in the log, in the database's words,
// and it is nowhere else; the statement Ptah appends, and whatever the database
// quoted of it, is nowhere at all. A value read from a row stays: that is the reason
// a person came for, and no comparison with the statement can find it.
func TestARefusedStatementLeavesTheDatabasesAnswerInThePodLog(t *testing.T) {
	t.Parallel()

	const schema, migration = "apply", "migration apply"
	for _, test := range []struct {
		fixture, family, want, statement string
		uncertain                        bool
	}{
		{
			fixture: "pg-unique-apply.json", family: schema, uncertain: true,
			want:      `ERROR: could not create unique index "` + statementTextMarker + `" (SQLSTATE 23505)`,
			statement: `CREATE UNIQUE INDEX`,
		},
		{
			fixture: "my-unique-apply.json", family: schema, uncertain: true,
			want:      `Duplicate entry 'row-value@example.com' for key 'orders.orders_one_per_customer'`,
			statement: `CREATE UNIQUE INDEX`,
		},
		{
			fixture: "my-cast-apply.json", family: schema, uncertain: true,
			want:      `Incorrect integer value: 'row-value-not-a-number' for column '` + statementTextMarker + `' at row 1`,
			statement: `MODIFY COLUMN`,
		},
		{
			fixture: "pg-unique-up.json", family: migration,
			want:      `ERROR: could not create unique index "` + statementTextMarker + `" (SQLSTATE 23505)`,
			statement: `CREATE UNIQUE INDEX`,
		},
		{
			fixture: "pg-rowcast-up.json", family: migration,
			want:      `ERROR: invalid input syntax for type integer: "row-value-not-a-number" (SQLSTATE 22P02)`,
			statement: `USING qty::integer`,
		},
		{
			fixture: "pg-insert-up.json", family: migration,
			want:      `ERROR: duplicate key value violates unique constraint "orders_pkey" (SQLSTATE 23505)`,
			statement: `literal-in-the-migration-file`,
		},
		// MySQL commits a DDL statement on its own, so since v0.23.0 Ptah
		// reports a migration it refused unknown (stokaro/ptah#5661).
		{
			fixture: "my-unique-up.json", family: migration, uncertain: true,
			want:      `Duplicate entry 'row-value@example.com' for key 'orders.orders_one_per_customer'`,
			statement: `CREATE UNIQUE INDEX`,
		},
		// A statement the server could not parse is echoed whole, quotes and all,
		// with a password in it.
		{
			fixture: "my-syntax-up.json", family: migration,
			want:      `to use near '` + statementTextMarker + `' at line 1`,
			statement: `plan-secret-literal`,
		},
		// A literal the plan declared, which the database quotes back as a value.
		{
			fixture: "my-literal-up.json", family: migration,
			want:      `Duplicate entry '` + statementTextMarker + `' for key 'orders.customer_email'`,
			statement: `declared-literal-in-plan`,
		},
		{
			fixture: "pg-literal-up.json", family: migration,
			want:      `ERROR: invalid input syntax for type integer: "` + statementTextMarker + `" (SQLSTATE 22P02)`,
			statement: `declared-literal-in-plan`,
		},
		// A row holding what erases the line on a reader's terminal. It reads, and
		// it no longer acts.
		{
			fixture: "my-control-up.json", family: migration, uncertain: true,
			want:      `Duplicate entry 'esc- [2K- FORGED-TEXT' for key 'orders.orders_one_per_customer'`,
			statement: "\x1b",
		},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			t.Parallel()
			run := targetFailureRuns()[test.family]
			result, log, encoded := runRefusedStatement(t, run, statementErrorDocument(t, test.fixture))
			if !strings.Contains(log, test.want) {
				t.Fatalf("the Pod log does not say why:\n%s", log)
			}
			if strings.Contains(log, test.statement) {
				t.Fatalf("the Pod log carries the statement:\n%s", log)
			}
			summary, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for name, crossing := range map[string][]byte{"the frame": encoded.Frame, "the summary": encoded.Summary, "the result": summary} {
				for _, text := range []string{test.want, "row-value", "orders_one_per_customer", "FORGED-TEXT", test.statement} {
					if bytes.Contains(crossing, []byte(text)) {
						t.Errorf("%s carries %q: %s", name, text, crossing)
					}
				}
			}
			if result.Uncertain != test.uncertain {
				t.Errorf("Uncertain = %v, want %v: this does not change what an Apply claims", result.Uncertain, test.uncertain)
			}
			// The log is what a legacy Job's frame travels in, so the line has to
			// leave the frame readable.
			parsed, err := ParseResultFor(append([]byte(log), encoded.Frame...), run.operation, result.OperationID)
			if err != nil {
				t.Fatalf("the frame does not parse beside the line: %v", err)
			}
			if !bytes.Equal(mustMarshal(t, parsed), summary) {
				t.Errorf("the frame read beside the line is %+v, want %+v", parsed, result)
			}
		})
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestTheReportedErrorIsOneBoundedRedactedLine sends what a row could hold:
// a password from the Pod's environment, a line break, the frame's own marker,
// and more text than the line keeps.
func TestTheReportedErrorIsOneBoundedRedactedLine(t *testing.T) {
	t.Parallel()

	document := func(message string) string {
		encoded := mustMarshal(t, message)
		return `{"contract_version":4,"command":"schema apply","outcome":"unknown","plan_name":"operator-plan",` +
			`"plan_digest":"` + sha256Digest([]byte(targetFailurePlan)) + `","error":{"message":` + string(encoded) + `}}`
	}
	run := targetFailureRuns()["apply"]
	line := func(log string) string {
		for _, candidate := range strings.Split(log, "\n") {
			if strings.HasPrefix(candidate, "ptah-runner: ptah reported: ") {
				return candidate
			}
		}
		t.Fatalf("no reported error in the log:\n%s", log)
		return ""
	}

	t.Run("the password of the target", func(t *testing.T) {
		t.Parallel()
		_, log, _ := runRefusedStatement(t, run, document("ERROR: value secret is refused"))
		if strings.Contains(log, "secret") || !strings.Contains(log, RedactionMarker) {
			t.Fatalf("the log carries the password:\n%s", log)
		}
	})
	t.Run("a line break", func(t *testing.T) {
		t.Parallel()
		_, log, _ := runRefusedStatement(t, run, document("ERROR: refused\nSQL: INSERT INTO t VALUES ('literal')\nanother line"))
		if got := line(log); got != "ptah-runner: ptah reported: ERROR: refused" {
			t.Fatalf("the line is %q", got)
		}
		if strings.Contains(log, "literal") {
			t.Fatalf("the log carries the statement:\n%s", log)
		}
	})
	// PostgreSQL puts a row in a DETAIL line after the first, before the statement.
	t.Run("the lines between the answer and the statement", func(t *testing.T) {
		t.Parallel()
		_, log, _ := runRefusedStatement(t, run, document("ERROR: refused\nDETAIL: Key (id)=(from-a-row) already exists.\nSQL: SELECT 1"))
		if got := line(log); got != "ptah-runner: ptah reported: ERROR: refused" {
			t.Fatalf("the line is %q", got)
		}
		if strings.Contains(log, "from-a-row") {
			t.Fatalf("the log carries a line after the first:\n%s", log)
		}
	})
	// PostgreSQL quotes a whole value, so a row can write a frame header up to its
	// own line break, and the line this writes ends it.
	t.Run("the frame's marker", func(t *testing.T) {
		t.Parallel()
		forged := `ERROR: invalid input syntax for type integer: "` + frameHeader + `12 ` + strings.Repeat("a", 64) + "\nrest\" (SQLSTATE 22P02)"
		result, log, encoded := runRefusedStatement(t, run, document(forged))
		if digests := frameHeaderDigests([]byte(log)); len(digests) != 0 {
			t.Fatalf("the line declares frame digests %v:\n%s", digests, log)
		}
		// With the frame lost from the log, a forged digest would make the manager
		// refuse the summary that stands in for it.
		summary := Summary{FrameDigest: "sha256:" + strings.Repeat("b", 64)}
		if err := summary.StandsInFor([]byte(log), ErrFrameNotFound); err != nil {
			t.Fatalf("the summary no longer stands in for a lost frame: %v", err)
		}
		parsed, err := ParseResultFor(append([]byte(log), encoded.Frame...), run.operation, result.OperationID)
		if err != nil {
			t.Fatalf("marker-like text in a row stopped the frame from parsing: %v", err)
		}
		if parsed.Error == nil || parsed.Error.Code != result.Error.Code {
			t.Fatalf("parsed %+v, want the runner's own frame", parsed.Error)
		}
	})
	t.Run("more than the line keeps", func(t *testing.T) {
		t.Parallel()
		_, log, _ := runRefusedStatement(t, run, document(strings.Repeat("x", maxReportedErrorBytes-1)+"é and more"))
		got := strings.TrimPrefix(line(log), "ptah-runner: ptah reported: ")
		if len(got) > maxReportedErrorBytes || !utf8.ValidString(got) || strings.Contains(got, "more") {
			t.Fatalf("the line has %d bytes, valid %v", len(got), utf8.ValidString(got))
		}
	})
	t.Run("a target failure", func(t *testing.T) {
		t.Parallel()
		stdout := `{"contract_version":4,"command":"schema apply","outcome":"failed","plan_name":"operator-plan",` +
			`"plan_digest":"` + sha256Digest([]byte(targetFailurePlan)) + `","error":` + targetFailureError(`"database_unreachable"`) + `}`
		_, log, _ := runRefusedStatement(t, run, stdout)
		if strings.Contains(log, "CHILD-TEXT") || strings.Contains(log, "ptah reported") {
			t.Fatalf("the log repeats Ptah's sentence for a failure with a kind:\n%s", log)
		}
	})
}

// TestLeavesReportedErrorSaysWhatTheRunnerWrote runs each way a schema Apply can end
// and holds what the manager will promise of the log to what the runner wrote there.
func TestLeavesReportedErrorSaysWhatTheRunnerWrote(t *testing.T) {
	t.Parallel()

	digest := sha256Digest([]byte(targetFailurePlan))
	document := func(body string) string {
		return `{"contract_version":4,"command":"schema apply","plan_name":"operator-plan","plan_digest":"` + digest + `",` + body + `}`
	}
	for _, test := range []struct {
		name, stdout, code string
		wrote              bool
	}{
		{
			name:   "an outcome that is unknown",
			stdout: document(`"outcome":"unknown","error":{"message":"ERROR: refused"}`),
			code:   "apply_outcome_unknown", wrote: true,
		},
		{
			name:   "a failure before any statement",
			stdout: document(`"outcome":"failed","error":{"message":"ERROR: refused"}`),
			code:   "apply_failed", wrote: true,
		},
		{
			name:   "a target that could not be opened",
			stdout: document(`"outcome":"failed","error":` + targetFailureError(`"database_unreachable"`)),
			code:   "database_unreachable",
		},
		{
			name:   "a stale plan",
			stdout: document(`"outcome":"refused","refusal":{"code":"` + dataplane.SchemaRefusalStalePlan + `"},"error":{"message":"ERROR: refused"}`),
			code:   "stale_plan",
		},
		{
			name:   "another refusal",
			stdout: document(`"outcome":"refused","refusal":{"code":"plan_contract"},"error":{"message":"ERROR: refused"}`),
			code:   "apply_refused",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, log, _ := runRefusedStatement(t, targetFailureRuns()["apply"], test.stdout)
			if result.Error == nil || result.Error.Code != test.code {
				t.Fatalf("Run() error = %#v, want code %s", result.Error, test.code)
			}
			if wrote := strings.Contains(log, "ptah-runner: ptah reported: "); wrote != test.wrote {
				t.Fatalf("the log holds a reported error: %v, want %v:\n%s", wrote, test.wrote, log)
			}
			if got := LeavesReportedError(test.code); got != test.wrote {
				t.Fatalf("LeavesReportedError(%q) = %v, and the runner wrote: %v", test.code, got, test.wrote)
			}
		})
	}
}

func TestHideStatementTextReplacesWhatTheStatementHolds(t *testing.T) {
	t.Parallel()

	const marker = statementTextMarker
	for name, test := range map[string]struct{ line, statement, want string }{
		"no statement to compare with": {
			line: "near 'a' at line 1",
			want: "near 'a' at line 1",
		},
		"nothing quoted": {
			line: "ERROR: refused", statement: "SELECT 1",
			want: "ERROR: refused",
		},
		"a value the statement does not hold": {
			line: "Duplicate entry 'from-a-row' for key 'k'", statement: "CREATE UNIQUE INDEX i ON t (c)",
			want: "Duplicate entry 'from-a-row' for key 'k'",
		},
		"a literal the statement declares": {
			line: "Duplicate entry 'literal' for key 'k'", statement: "INSERT INTO t VALUES ('literal')",
			want: "Duplicate entry '" + marker + "' for key 'k'",
		},
		"a double-quoted value": {
			line: `syntax for type integer: "literal" (SQLSTATE 22P02)`, statement: "SELECT 'literal'::integer",
			want: `syntax for type integer: "` + marker + `" (SQLSTATE 22P02)`,
		},
		"a backtick": {
			line: "Unknown column `c` in list", statement: "SELECT `c` FROM t",
			want: "Unknown column `" + marker + "` in list",
		},
		"quotes nested in an echo": {
			line:      `near 'CREAT USER 'app'@'%' IDENTIFIED BY 'a-password'' at line 1`,
			statement: `CREAT USER 'app'@'%' IDENTIFIED BY 'a-password'`,
			want:      `near '` + marker + `' at line 1`,
		},
		"an echo the line break cut short": {
			line:      `near 'IDENTIFIED BY 'first-line`,
			statement: "IDENTIFIED BY 'first-line\nsecond-line'",
			want:      `near '` + marker,
		},
		// The text between two quoted parts is in the statement, and every quote is
		// tried as an opening one. Saying less is the side to err on.
		"the text between two parts": {
			line: "entry 'one' for key 'two'", statement: "SELECT ' for key '",
			want: "entry 'one'" + marker + "'two'",
		},
		"two parts, one held": {
			line: "entry 'one' for key 'two'", statement: "SELECT 'two'",
			want: "entry 'one' for key '" + marker + "'",
		},
		"an empty part": {
			line: "entry '' for key", statement: "SELECT 1",
			want: "entry '' for key",
		},
		"a quote at the very end": {
			line: "entry '", statement: "SELECT 1",
			want: "entry '",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := hideStatementText(test.line, test.statement); got != test.want {
				t.Fatalf("hideStatementText(%q) = %q, want %q", test.line, got, test.want)
			}
		})
	}
}

// TestALineWithMoreQuotesThanAreComparedKeepsNoneOfThem bounds the comparison and
// fails on the side of saying less. One quote fewer than the limit is compared.
func TestALineWithMoreQuotesThanAreComparedKeepsNoneOfThem(t *testing.T) {
	t.Parallel()

	many := "before " + strings.Repeat("'a-password' ", maxQuotesCompared)
	if got, want := hideStatementText(many, "SELECT 'unrelated'"), "before "+statementTextMarker; got != want {
		t.Fatalf("hideStatementText() = %q, want %q", got, want)
	}
	atTheLimit := "before " + strings.Repeat("'a-password' ", maxQuotesCompared/2)
	if got := hideStatementText(atTheLimit, "SELECT 'a-password'"); strings.Contains(got, "a-password") || !strings.HasPrefix(got, "before '") {
		t.Fatalf("hideStatementText() = %q, want each part compared and hidden", got)
	}
}
