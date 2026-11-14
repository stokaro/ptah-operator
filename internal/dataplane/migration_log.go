package dataplane

import (
	"fmt"
	"math"
	"slices"
)

// States a lost entry of an up run would have recorded in Ptah's migration
// log: one before a migration runs, and one after it applied or failed. Ptah
// also writes rolled_back, which only a down run writes.
const (
	MigrationLogStarted = "started"
	MigrationLogApplied = "applied"
	MigrationLogFailed  = "failed"
)

// migrationLogStates is every state an up run's entry can carry.
var migrationLogStates = []string{MigrationLogStarted, MigrationLogApplied, MigrationLogFailed}

// MaxMigrationVersionKeyBytes bounds a lost entry's version key. The key is
// the version a migration file's name spells, and no file system lets a name
// exceed 255 bytes.
const MaxMigrationVersionKeyBytes = 255

// MigrationLogReport is what a run's writes to Ptah's migration log came to:
// the schema_migrations_log rows Ptah writes beside the revision table, one
// before each migration it runs and one after. A write the database refuses
// does not stop the migration, so the run is the only witness to a lost entry,
// and the table cannot say what it was never given. Ptah reports it in the run
// document since v0.23.0 (stokaro/ptah#5541), and leaves it out when the run
// keeps no log or stopped before it selected anything.
//
// A lost entry changes nothing about the outcome, which Ptah reads from the
// revision table and not from the log.
type MigrationLogReport struct {
	// Written counts the entries the database accepted.
	Written int `json:"written"`
	// Lost names each entry the database refused, in the order the run tried
	// them.
	Lost []MigrationLogLoss `json:"lost,omitempty"`
}

// MigrationLogLoss is one entry the migration log should hold and does not.
type MigrationLogLoss struct {
	// Operation is the verb the entry belongs to, up for every run this
	// operator dispatches.
	Operation string `json:"operation"`
	// Version and VersionKey name the migration the entry is about. Ptah
	// always writes the key, because an empty string is itself an identity.
	Version    int64  `json:"version"`
	VersionKey string `json:"version_key"`
	// State is what the entry would have recorded.
	State string `json:"state,omitempty"`
	// SQLState is empty when the server gave no answer, and ErrorNumber is
	// zero on every protocol but MySQL's.
	SQLState    string `json:"sqlstate,omitempty"`
	ErrorNumber int    `json:"error_number,omitempty"`
	// Message is Ptah's sentence about the refusal, which can quote part of
	// the value the database refused. The runner clears it before the report
	// reaches a frame, and a frame that carries one is refused, so SQLState
	// and ErrorNumber are what say why.
	Message string `json:"message,omitempty"`
}

// LostCount is how many entries the log lacks, and zero for a report that is
// absent.
func (r *MigrationLogReport) LostCount() int {
	if r == nil {
		return 0
	}
	return len(r.Lost)
}

// ClearMessages drops every lost entry's message.
func (r *MigrationLogReport) ClearMessages() {
	if r == nil {
		return
	}
	for index := range r.Lost {
		r.Lost[index].Message = ""
	}
}

// Validate holds a log report to the up run that wrote it, whose selection is
// planned. A run writes at most two entries for each migration it selected,
// and each lost entry names one of them with a state an up run records. The
// code the database answered with is held to the forms a run's failure is:
// a value that failed its form could be anything, so nothing here quotes it.
func (r MigrationLogReport) Validate(planned []int64) error {
	if r.Written < 0 {
		return fmt.Errorf("migration run reports %d migration log entries written", r.Written)
	}
	if entries := int64(r.Written) + int64(len(r.Lost)); entries > 2*int64(len(planned)) {
		return fmt.Errorf("migration run reports %d migration log entries for %d selected migrations, "+
			"and a run writes two for each", entries, len(planned))
	}
	for _, loss := range r.Lost {
		if err := loss.validate(planned); err != nil {
			return err
		}
	}
	return nil
}

func (l MigrationLogLoss) validate(planned []int64) error {
	switch {
	case l.Operation != "up":
		return fmt.Errorf("migration run reports a lost migration log entry of operation %q; "+
			"this operator dispatches up only", l.Operation)
	case !slices.Contains(migrationLogStates, l.State):
		return fmt.Errorf("migration run reports a lost migration log entry in state %q, "+
			"which an up run does not write", l.State)
	case l.Version <= 0 || !slices.Contains(planned, l.Version):
		return fmt.Errorf("migration run reports a lost migration log entry of version %d, "+
			"which is not one it selected", l.Version)
	case len(l.VersionKey) > MaxMigrationVersionKeyBytes:
		return fmt.Errorf("migration run reports a lost migration log entry whose version key is %d bytes, "+
			"longer than a file name holds", len(l.VersionKey))
	case l.SQLState != "" && !sqlStatePattern.MatchString(l.SQLState):
		return fmt.Errorf("migration run reports a lost migration log entry with a SQLSTATE " +
			"that is not five digits or capital letters")
	case l.ErrorNumber < 0 || l.ErrorNumber > math.MaxUint16:
		return fmt.Errorf("migration run reports a lost migration log entry with error number %d, "+
			"which the two bytes MySQL sends cannot hold", l.ErrorNumber)
	}
	return nil
}
