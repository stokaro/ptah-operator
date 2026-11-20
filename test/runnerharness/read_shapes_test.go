package runnerharness_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

const (
	// relativeTimeDomain is the schema row whose file gives a domain a
	// relative time default, which Ptah v0.34.0 refuses (stokaro/ptah#6386),
	// and relativeTimeDomainColumn the row whose file gives a column a domain
	// types a relative time default of its own, which v0.33.1 refuses.
	relativeTimeDomain       = "desired-source-schema-relative-time-domain"
	relativeTimeDomainColumn = "desired-source-schema-relative-time-domain-column"
	// domainFault is how validate names that domain, and placedDomainFault
	// how drift and plan name it, in the schema they placed it in.
	domainFault       = `domain "moment": DEFAULT 'now' on a timestamp domain is read once`
	placedDomainFault = `domain "public.moment": DEFAULT 'now' on a timestamp domain is read once`
	// columnFault is how validate names that column, its table and its type,
	// and placedColumnFault how drift and plan name them: the table in the
	// schema they placed it in, and the type by the domain's schema.
	columnFault       = `column "created_at" of table "events": DEFAULT 'now' on a moment column is read once`
	placedColumnFault = `column "created_at" of table "public.events": DEFAULT 'now' on a public.moment column is read once`
)

// checkRelativeTimeDomain says what the committed captures miss of the domain
// rows' readings. Validate reports each fault as one of the schema, not of the
// file, so the runner can only read it as desired_schema_invalid, and only
// where the failed command's sentence ends with validate's words once the
// names drift and plan placed are allowed for: the domain's, the table's, and
// the schema the column's type is written with. The reason is validate's, so
// it names what the file names. A release, or an operator change, that moves
// either code or either reason moves this check.
func checkRelativeTimeDomain(captures runnerharness.Captures) error {
	var problems []error
	for _, shape := range []struct{ row, fault, placed string }{
		{relativeTimeDomain, domainFault, placedDomainFault},
		{relativeTimeDomainColumn, columnFault, placedColumnFault},
	} {
		for _, step := range []struct{ name, command string }{{"observe", "schema drift"}, {"plan", "schema plan"}} {
			name := shape.row + "/" + step.name
			record, err := capturedRow(captures, name)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if code := frameCode(record); code != runner.CodeDesiredSchemaInvalid {
				problems = append(problems, fmt.Errorf("%s: the frame says %q, want %s", name, code, runner.CodeDesiredSchemaInvalid))
			}
			if message := frameMessage(record); !strings.HasPrefix(message, "schema.sql: "+shape.fault) {
				problems = append(problems, fmt.Errorf("%s: the frame says %q, want the reason %q, in validate's words", name, message, "schema.sql: "+shape.fault))
			}
			failed, err := capturedChild(captures, name, step.command)
			switch {
			case err != nil:
				problems = append(problems, err)
			case failed.Exit == 0 || !strings.Contains(failed.Stderr, shape.placed):
				problems = append(problems, fmt.Errorf("%s: `ptah %s` exited %d and said %q, want it to stop on %q", name, step.command, failed.Exit, failed.Stderr, shape.placed))
			}
			validate, err := capturedChild(captures, name, "schema validate")
			switch {
			case err != nil:
				problems = append(problems, err)
			case validate.Exit != 1 || !strings.Contains(string(captures.Documents[validate.Stdout]), "postgres: schema: "+shape.fault):
				problems = append(problems, fmt.Errorf("%s: `ptah schema validate` exited %d and printed %q, want the structural line %q",
					name, validate.Exit, captures.Documents[validate.Stdout], "postgres: schema: "+shape.fault))
			}
		}
	}
	return errors.Join(problems...)
}

// frameMessage is the message of the error a captured frame carries.
func frameMessage(row runnerharness.Row) string {
	frame, _ := row.Result.(map[string]any)
	failure, _ := frame["error"].(map[string]any)
	message, _ := failure["message"].(string)
	return message
}

// mismatchedFormatHistory is what checkMismatchedFormat reads of a History
// frame.
type mismatchedFormatHistory struct {
	MigrationHistory *struct {
		Migrations []struct {
			VersionKey string `json:"version_key"`
			State      string `json:"state"`
		} `json:"migrations"`
	} `json:"migrationHistory"`
}

// checkMismatchedFormat says what the committed captures miss of an OCI
// artifact whose migration-format annotation says ptah over a file named for
// Atlas. The fetch writes it unread, and the History reads the directory by
// format detection, with no --dir-format, as an Apply of the same digest
// does: as Atlas migrations, with the one file pending under version key 1.
func checkMismatchedFormat(captures runnerharness.Captures) error {
	name := formatFamily + "/history"
	var problems []error
	switch pull, err := capturedChild(captures, name, "migrations pull"); {
	case err != nil:
		problems = append(problems, err)
	case pull.Exit != 0 || !slices.Contains(pull.Args, "--skip-validation"):
		problems = append(problems, fmt.Errorf("%s: the fetch ran %q and exited %d, want it to write the artifact unread and pass", name, pull.Args, pull.Exit))
	case !strings.Contains(string(captures.Documents[pull.Stdout]), "\nFormat: ptah\n"):
		problems = append(problems, fmt.Errorf("%s: the fetch printed %q, want the artifact annotated Format: ptah", name, captures.Documents[pull.Stdout]))
	}
	switch status, err := capturedChild(captures, name, "migrations status"); {
	case err != nil:
		problems = append(problems, err)
	case slices.ContainsFunc(status.Args, func(arg string) bool { return strings.HasPrefix(arg, "--dir-format") }):
		problems = append(problems, fmt.Errorf("%s: the History ran %q, want it to read the directory by detection", name, status.Args))
	}
	record, err := capturedRow(captures, name)
	if err != nil {
		return errors.Join(append(problems, err)...)
	}
	if code := frameCode(record); code != "" {
		problems = append(problems, fmt.Errorf("%s: the frame says %s, want a history", name, code))
	}
	encoded, err := json.Marshal(record.Result)
	if err != nil {
		return errors.Join(append(problems, err)...)
	}
	var frame mismatchedFormatHistory
	if err := json.Unmarshal(encoded, &frame); err != nil {
		return errors.Join(append(problems, fmt.Errorf("%s: %w", name, err))...)
	}
	var read []string
	if frame.MigrationHistory != nil {
		for _, migration := range frame.MigrationHistory.Migrations {
			read = append(read, migration.VersionKey+" "+migration.State)
		}
	}
	if want := []string{"1 pending"}; !slices.Equal(read, want) {
		problems = append(problems, fmt.Errorf("%s: the History reads %q, want %q", name, read, want))
	}
	return errors.Join(problems...)
}

// The committed captures hold both readings, so a repin or a runner change
// that moves one asks for a decision in `go test`.
func TestARelativeTimeDomainReadsAsPtahWordsIt(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	if err := checkRelativeTimeDomain(captures); err != nil {
		t.Error(err)
	}
}

func TestAMismatchedFormatReadsByDetection(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	if err := checkMismatchedFormat(captures); err != nil {
		t.Error(err)
	}
}

// Each check refuses a capture moved where it does not belong, or edited the
// way a release or a runner change could have written it.
func TestReadShapeChecksRefuseAMovedReading(t *testing.T) {
	t.Parallel()
	_, committed := committedCaptures(t)
	edit := func(captures *runnerharness.Captures, name string, change func(*runnerharness.Row)) {
		t.Helper()
		index := slices.IndexFunc(captures.Rows, func(row runnerharness.Row) bool { return row.Name == name })
		if index < 0 {
			t.Fatalf("no row %s", name)
		}
		captures.Rows[index].Children = slices.Clone(captures.Rows[index].Children)
		for child := range captures.Rows[index].Children {
			captures.Rows[index].Children[child].Args = slices.Clone(captures.Rows[index].Children[child].Args)
		}
		change(&captures.Rows[index])
	}
	document := func(captures *runnerharness.Captures, name, command string, change func(string) string) {
		t.Helper()
		child, err := capturedChild(*captures, name, command)
		if err != nil {
			t.Fatal(err)
		}
		captures.Documents[child.Stdout] = []byte(change(string(captures.Documents[child.Stdout])))
	}
	stray := func(captures *runnerharness.Captures, name string) {
		t.Helper()
		source, err := capturedRow(*captures, strayFileFamily+"/history")
		if err != nil {
			t.Fatal(err)
		}
		edit(captures, name, func(row *runnerharness.Row) { row.Result = source.Result })
	}
	for _, mistake := range []struct {
		name, want string
		check      func(runnerharness.Captures) error
		change     func(*runnerharness.Captures)
	}{
		{
			// What the Observe read as before the runner placed a domain.
			name: "a domain fault the runner could not tie to the file", want: "want " + runner.CodeDesiredSchemaInvalid, check: checkRelativeTimeDomain,
			change: func(captures *runnerharness.Captures) {
				edit(captures, relativeTimeDomain+"/observe", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": "child_exit", "message": "ptah exited with code 2"}}
				})
			},
		},
		{
			name: "a domain fault read as one of the file", want: "want " + runner.CodeDesiredSchemaInvalid, check: checkRelativeTimeDomain,
			change: func(captures *runnerharness.Captures) {
				edit(captures, relativeTimeDomain+"/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": runner.CodeDesiredSourceUnreadable, "message": "schema.sql: " + domainFault}}
				})
			},
		},
		{
			name: "a reason in the failed command's words", want: "in validate's words", check: checkRelativeTimeDomain,
			change: func(captures *runnerharness.Captures) {
				edit(captures, relativeTimeDomainColumn+"/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": runner.CodeDesiredSchemaInvalid, "message": "schema.sql: " + placedColumnFault}}
				})
			},
		},
		{
			name: "a drift that names the domain as validate does", want: "want it to stop on", check: checkRelativeTimeDomain,
			change: func(captures *runnerharness.Captures) {
				edit(captures, relativeTimeDomain+"/observe", func(row *runnerharness.Row) {
					row.Children[0].Stderr = strings.Replace(row.Children[0].Stderr, `"public.moment"`, `"moment"`, 1)
				})
			},
		},
		{
			name: "a plan that names the column's type as validate does", want: "want it to stop on", check: checkRelativeTimeDomain,
			change: func(captures *runnerharness.Captures) {
				edit(captures, relativeTimeDomainColumn+"/plan", func(row *runnerharness.Row) {
					row.Children[0].Stderr = strings.Replace(row.Children[0].Stderr, "on a public.moment column", "on a moment column", 1)
				})
			},
		},
		{
			name: "a validate that blames the file", want: "want the structural line", check: checkRelativeTimeDomain,
			change: func(captures *runnerharness.Captures) {
				document(captures, relativeTimeDomain+"/plan", "schema validate", func(text string) string {
					return strings.Replace(text, "postgres: schema: ", "postgres: source: ", 1)
				})
			},
		},
		{
			name: "a fetch that read the files in the annotation's format", want: "want it to write the artifact unread", check: checkMismatchedFormat,
			change: func(captures *runnerharness.Captures) {
				edit(captures, formatFamily+"/history", func(row *runnerharness.Row) {
					row.Children[0].Args = slices.DeleteFunc(row.Children[0].Args, func(arg string) bool { return arg == "--skip-validation" })
				})
			},
		},
		{
			name: "an artifact annotated as what it holds", want: "want the artifact annotated", check: checkMismatchedFormat,
			change: func(captures *runnerharness.Captures) {
				document(captures, formatFamily+"/history", "migrations pull", func(text string) string {
					return strings.Replace(text, "Format: ptah", "Format: atlas", 1)
				})
			},
		},
		{
			name: "a History told the annotation's format", want: "want it to read the directory by detection", check: checkMismatchedFormat,
			change: func(captures *runnerharness.Captures) {
				edit(captures, formatFamily+"/history", func(row *runnerharness.Row) {
					row.Children[1].Args = append(row.Children[1].Args, "--dir-format", "ptah")
				})
			},
		},
		{
			name: "a History that refused the file", want: "want a history", check: checkMismatchedFormat,
			change: func(captures *runnerharness.Captures) { stray(captures, formatFamily+"/history") },
		},
		{
			name: "a version read under Ptah's grammar", want: `want ["1 pending"]`, check: checkMismatchedFormat,
			change: func(captures *runnerharness.Captures) {
				edit(captures, formatFamily+"/history", func(row *runnerharness.Row) {
					encoded, err := json.Marshal(row.Result)
					if err != nil {
						t.Fatal(err)
					}
					var frame map[string]any
					if err := json.Unmarshal([]byte(strings.Replace(string(encoded), `"version_key":"1"`, `"version_key":"0000000001"`, 1)), &frame); err != nil {
						t.Fatal(err)
					}
					row.Result = frame
				})
			},
		},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()
			captures := runnerharness.Captures{Rows: slices.Clone(committed.Rows), Documents: maps.Clone(committed.Documents)}
			mistake.change(&captures)
			if err := mistake.check(captures); err == nil || !strings.Contains(err.Error(), mistake.want) {
				t.Fatalf("check() = %v, want a refusal holding %q", err, mistake.want)
			}
		})
	}
}
