package dataplane_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The rows in this file decode documents Ptah v0.23.0 printed, as the runner
// harness captured them from the pinned release image.

func loadPtahV023(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A deferred foreign key that fails at commit, with the failing migration
// first and second. Ptah v0.23.0 reports statement 0 beside commit true, and
// the decoder reads it. The operator asks for no --tx-mode all, so a run never
// ends the commit of two migrations at once.
func TestDecodeMigrationRun_ReadsPtahV023CommitFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		file    string
		applied []int64
		want    dataplane.MigrationRunFailure
	}{
		{
			file: "migrations-up-commit-failed.json",
			want: dataplane.MigrationRunFailure{Version: 1, Statement: 0, Statements: 3, Commit: true, SQLState: "23503"},
		},
		{
			file:    "migrations-up-commit-failed-second.json",
			applied: []int64{1},
			want:    dataplane.MigrationRunFailure{Version: 2, Statement: 0, Statements: 1, Commit: true, SQLState: "23503"},
		},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeMigrationRun(loadPtahV023(t, test.file))
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			if report.Outcome != dataplane.MigrationOutcomeFailed || report.Failure == nil || *report.Failure != test.want {
				t.Fatalf("outcome %q failure %+v, want %q %+v", report.Outcome, report.Failure, dataplane.MigrationOutcomeFailed, test.want)
			}
			if !slices.Equal(report.Applied, test.applied) {
				t.Fatalf("applied = %v, want %v", report.Applied, test.applied)
			}
		})
	}
}

// A MySQL CREATE UNIQUE INDEX that fails with a duplicate key: v0.23.0 reports
// the run unknown, because MySQL commits DDL on its own, and leaves the
// in-flight marker in the revision row.
func TestDecodeMigrationRun_ReadsPtahV023UnknownMySQLRun(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationRun(loadPtahV023(t, "statement-errors/my-unique-up.json"))
	if err != nil {
		t.Fatalf("DecodeMigrationRun() error = %v", err)
	}
	want := dataplane.MigrationRunFailure{Version: 1, Statement: 1, Statements: 1, SQLState: "23000", ErrorNumber: 1062}
	if report.Outcome != dataplane.MigrationOutcomeUnknown || report.Failure == nil || *report.Failure != want {
		t.Fatalf("outcome %q failure %+v, want %q %+v", report.Outcome, report.Failure, dataplane.MigrationOutcomeUnknown, want)
	}
	dirty := report.Status.DirtyRevision
	if dirty == nil || dirty.Version != 1 || dirty.Applied != 0 || dirty.Total != 1 {
		t.Fatalf("dirty revision = %+v, want version 1 with 0 of 1 statements recorded", dirty)
	}
}

// A MySQL account that cannot see every object: Ptah v0.23.0 reports
// permission_denied with missing_privileges. The kinds of object are read, in
// vocabulary order, and the sentence built from them names none of what Ptah
// wrote about the database.
func TestUnseenObjectsFromPtahV023Failures(t *testing.T) {
	t.Parallel()

	plan := func(t *testing.T, name string) *dataplane.CommandError {
		report, err := dataplane.DecodeSchemaPlan(loadPtahV023(t, name))
		if err != nil {
			t.Fatalf("DecodeSchemaPlan() error = %v", err)
		}
		return report.Error
	}
	drift := func(t *testing.T, name string) *dataplane.CommandError {
		failure, err := dataplane.DecodeDriftFailure(loadPtahV023(t, name))
		if err != nil {
			t.Fatalf("DecodeDriftFailure() error = %v", err)
		}
		return failure
	}
	for _, row := range []struct {
		file   string
		decode func(*testing.T, string) *dataplane.CommandError
		want   []string
		says   string
	}{
		{file: "differing/drift-permission-denied-triggers-routines.json", decode: drift,
			want: []string{dataplane.UnseenTriggers, dataplane.UnseenRoutines}, says: "every trigger and routine"},
		{file: "differing/plan-permission-denied-triggers-routines.json", decode: plan,
			want: []string{dataplane.UnseenTriggers, dataplane.UnseenRoutines}, says: "every trigger and routine"},
		{file: "differing/drift-permission-denied-named-routines.json", decode: drift,
			want: []string{dataplane.UnseenRoutines}, says: "every routine"},
		{file: "differing/drift-permission-denied-named-views.json", decode: drift,
			want: []string{dataplane.UnseenViews}, says: "every view definition"},
	} {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			failure := row.decode(t, row.file)
			objects, named := failure.UnseenObjects()
			if !named || !slices.Equal(objects, row.want) {
				t.Fatalf("UnseenObjects() = %q, %t; want %q", objects, named, row.want)
			}
			sentence := dataplane.UnseenObjectsDescription(objects)
			if !strings.Contains(sentence, row.says) {
				t.Fatalf("UnseenObjectsDescription() = %q, want it to say %q", sentence, row.says)
			}
			for _, written := range []string{"harness_differs", "harness_login", "differs_hand_fn", "differs_notes_view", "SHOW", "TRIGGER", "*.*"} {
				if strings.Contains(sentence, written) {
					t.Fatalf("UnseenObjectsDescription() = %q repeats %q from Ptah's document", sentence, written)
				}
			}
		})
	}
}

// Only a permission_denied that names known kinds of object is read; a list
// with a kind outside the vocabulary is not read short.
func TestUnseenObjectsRefuses(t *testing.T) {
	t.Parallel()

	privileges := func(objects ...string) []dataplane.MissingPrivilege {
		entries := make([]dataplane.MissingPrivilege, len(objects))
		for index, kind := range objects {
			entries[index] = dataplane.MissingPrivilege{Objects: kind}
		}
		return entries
	}
	for _, row := range []struct {
		name    string
		failure *dataplane.CommandError
	}{
		{name: "no error"},
		{name: "another kind", failure: &dataplane.CommandError{Kind: "authentication_failed", MissingPrivileges: privileges("triggers")}},
		{name: "no kind", failure: &dataplane.CommandError{MissingPrivileges: privileges("triggers")}},
		{name: "no missing privileges", failure: &dataplane.CommandError{Kind: "permission_denied"}},
		{name: "a kind of object outside the vocabulary", failure: &dataplane.CommandError{
			Kind: "permission_denied", MissingPrivileges: privileges("triggers", "events")}},
		{name: "a kind of object in another case", failure: &dataplane.CommandError{
			Kind: "permission_denied", MissingPrivileges: privileges("Triggers")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if objects, named := row.failure.UnseenObjects(); named || objects != nil {
				t.Fatalf("UnseenObjects() = %q, %t; want nothing named", objects, named)
			}
		})
	}

	repeated, named := (&dataplane.CommandError{Kind: "permission_denied",
		MissingPrivileges: privileges("routines", "tables", "routines", "views", "constraints")}).UnseenObjects()
	if want := []string{"tables", "views", "constraints", "routines"}; !named || !slices.Equal(repeated, want) {
		t.Fatalf("UnseenObjects() = %q, %t; want %q", repeated, named, want)
	}
	if got, want := dataplane.UnseenObjectsDescription(repeated),
		"the target database server accepted the credentials, and the user cannot see every table, view definition, "+
			"constraint and routine in the database"; got != want {
		t.Fatalf("UnseenObjectsDescription() = %q, want %q", got, want)
	}
	for _, list := range [][]string{nil, {}, {"events"}, {"routines", "tables"}, {"tables", "tables"}} {
		if err := dataplane.ValidateUnseenObjects(list); err == nil {
			t.Errorf("ValidateUnseenObjects(%q) accepted it", list)
		}
		if sentence := dataplane.UnseenObjectsDescription(list); sentence != "" {
			t.Errorf("UnseenObjectsDescription(%q) = %q, want none", list, sentence)
		}
	}
}
