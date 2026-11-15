package dataplane

import (
	"errors"
	"fmt"
)

// RunLogReference is what a run's document says about the entries it wrote to
// the database's own log, as far as the run reference goes: how many the
// database accepted, and the reference they carry. Ptah v0.28.0 reports it as
// apply_log in the schema apply document and as migration_log in the
// migrations up document (stokaro/ptah#6014), and leaves the reference out
// when the run was given none or wrote no entry.
//
// It is read beside the report the frame carries rather than as part of it, so
// a frame holds what it held before the reference existed.
type RunLogReference struct {
	// Written counts the entries the database accepted.
	Written int `json:"written"`
	// RunReference is the caller's reference the written entries carry.
	RunReference string `json:"run_reference,omitempty"`
}

// CheckRunReference holds the reference a run reports to the one it was given.
//
// A document that names another reference is not this run's account: it was
// written for another run, or by a Ptah that stored a value other than the one
// it was handed, and either way the entries it counts cannot be joined to this
// operation. A document that counts written entries and names no reference
// says the entries carry none. On the SQL engines every entry Ptah writes
// carries the reference it was given, and only a provider's own log store
// keeps none, which neither engine this operator runs is. A run that wrote
// nothing names nothing, and that is the only reading of an absent reference
// that holds. An absent report is a run that kept no log, and holds.
//
// The reported value is not quoted, because a value that failed the check
// could be anything.
func (r *RunLogReference) CheckRunReference(expected string) error {
	switch {
	case r == nil:
		return nil
	case r.RunReference != "" && r.RunReference != expected:
		return errors.New("the run reports log entries under another run reference than the one it was given")
	case r.Written > 0 && r.RunReference == "":
		return fmt.Errorf("the run reports %d log entries written and names no run reference for them", r.Written)
	}
	return nil
}

// migrationRunLog is the part of a migrations up document that says what its
// log entries carry.
type migrationRunLog struct {
	MigrationLog *RunLogReference `json:"migration_log,omitempty"`
}

// DecodeMigrationLogReference reads the run reference a migrations up document
// reports for its log entries. DecodeMigrationRun validates the rest of the
// document; this reads one more field of the same bytes.
func DecodeMigrationLogReference(data []byte) (*RunLogReference, error) {
	var document migrationRunLog
	if err := decodeJSON(data, &document, false); err != nil {
		return nil, fmt.Errorf("decode migration run log: %w", err)
	}
	return document.MigrationLog, nil
}
