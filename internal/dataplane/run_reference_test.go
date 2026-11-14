package dataplane_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const (
	givenReference   = "sha256:" + "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	anotherReference = "sha256:" + "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
)

func TestCheckRunReferenceHoldsTheEntriesToTheRunsReference(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		log     *dataplane.RunLogReference
		refused bool
	}{
		{name: "no log report"},
		{name: "entries under the reference given", log: &dataplane.RunLogReference{Written: 2, RunReference: givenReference}},
		{name: "no entry written and none named", log: &dataplane.RunLogReference{}},
		{name: "entries under another reference", log: &dataplane.RunLogReference{Written: 2, RunReference: anotherReference}, refused: true},
		{name: "entries under the reference with one more byte", log: &dataplane.RunLogReference{Written: 1, RunReference: givenReference + "0"}, refused: true},
		{name: "entries under no reference", log: &dataplane.RunLogReference{Written: 1}, refused: true},
		{name: "no entry and another reference", log: &dataplane.RunLogReference{RunReference: anotherReference}, refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := row.log.CheckRunReference(givenReference)
			if row.refused != (err != nil) {
				t.Fatalf("CheckRunReference() error = %v, want refused %t", err, row.refused)
			}
			if err != nil && strings.Contains(err.Error(), anotherReference) {
				t.Fatalf("the refusal quotes the reported reference: %v", err)
			}
		})
	}
}

func TestDecodeMigrationLogReferenceReadsTheReportBesideTheRun(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name, document string
		want           *dataplane.RunLogReference
	}{
		{name: "a log under a reference", document: runWithLog(`{"written":4,"run_reference":"` + givenReference + `"}`),
			want: &dataplane.RunLogReference{Written: 4, RunReference: givenReference}},
		{name: "a log whose entries were lost", document: runWithLog(`{"written":0,"lost":[` + lostActor + `]}`),
			want: &dataplane.RunLogReference{}},
		{name: "no log report", document: `{"contract_version":2,"command":"migrations up","outcome":"applied"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := dataplane.DecodeMigrationLogReference([]byte(row.document))
			if err != nil {
				t.Fatalf("DecodeMigrationLogReference() error = %v", err)
			}
			if (got == nil) != (row.want == nil) || got != nil && *got != *row.want {
				t.Fatalf("DecodeMigrationLogReference() = %+v, want %+v", got, row.want)
			}
		})
	}
	if _, err := dataplane.DecodeMigrationLogReference([]byte(`{"migration_log":{"run_reference":7}}`)); err == nil {
		t.Fatal("DecodeMigrationLogReference() read a reference that is not a string")
	}
}

// Every document the runner harness captured from the pinned Ptah with a log
// report holds the rule CheckRunReference reads: a run that wrote an entry
// names the reference it was given, and a run that wrote none names none. The
// harness gives each row its own operation ID, so each document passes under
// the reference it names and is refused under another.
func TestCapturedRunsReportTheReferenceTheirEntriesCarry(t *testing.T) {
	t.Parallel()

	written, none := 0, 0
	root := filepath.Join("testdata", "runner-harness", "documents")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".json" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var document struct {
			ApplyLog     *dataplane.RunLogReference `json:"apply_log"`
			MigrationLog *dataplane.RunLogReference `json:"migration_log"`
		}
		if json.Unmarshal(data, &document) != nil {
			return nil
		}
		for _, log := range []*dataplane.RunLogReference{document.ApplyLog, document.MigrationLog} {
			switch {
			case log == nil:
				continue
			case log.Written > 0 && log.RunReference != "":
				written++
				if err := log.CheckRunReference(log.RunReference); err != nil {
					t.Errorf("%s: refused under the reference it names: %v", path, err)
				}
				if log.CheckRunReference(anotherReference) == nil {
					t.Errorf("%s: admitted under another reference", path)
				}
			case log.Written == 0 && log.RunReference == "":
				none++
				if err := log.CheckRunReference(anotherReference); err != nil {
					t.Errorf("%s: a run that wrote nothing is refused: %v", path, err)
				}
			default:
				t.Errorf("%s: reports %d entries written under reference %q", path, log.Written, log.RunReference)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if written == 0 || none == 0 {
		t.Fatalf("read %d logs with entries and %d without, want at least one of each", written, none)
	}
}

// The schema apply decoder reads the report Ptah writes beside the outcome.
func TestDecodeSchemaApplyReadsTheApplyLog(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "statement-errors", "pg-unique-apply.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := dataplane.DecodeSchemaApply(data)
	if err != nil {
		t.Fatalf("DecodeSchemaApply() error = %v", err)
	}
	want := dataplane.RunLogReference{Written: 2, RunReference: "sha256:c1a99cbcc0db807b9233dc9c299d5b0f0935658aa1aa3e153a11f05f1cb78f61"}
	if report.ApplyLog == nil || *report.ApplyLog != want {
		t.Fatalf("ApplyLog = %+v, want %+v", report.ApplyLog, want)
	}
}
