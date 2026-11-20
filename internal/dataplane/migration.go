package dataplane

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"
)

const (
	// SupportedMigrationStatusContract and SupportedMigrationRunContract are the
	// Ptah machine-contract versions this operator reads.
	//
	// A document announcing another version is refused rather than read: the
	// fields this operator knows may still be present and may already mean
	// something else, and a history read under the wrong contract is a history
	// nobody checked.
	//
	// Version 2 of both is stokaro/ptah#5173's. A status run that cannot read
	// the history now prints a failure document in the status contract's
	// version, which a version 1 reader would take for a database with nothing
	// applied; a run document carries error as an object with a kind, and a
	// run that stopped before it selected anything is failed with nothing
	// planned and no status.
	SupportedMigrationStatusContract = 2
	SupportedMigrationRunContract    = 2

	// MigrationStatusCommand and MigrationRunCommand are the commands the two
	// documents name. A status embedded in a run document names none.
	MigrationStatusCommand = "migrations status"
	MigrationRunCommand    = "migrations up"

	// Migration states a status document reports per migration.
	MigrationStateApplied           = "applied"
	MigrationStateModified          = "modified"
	MigrationStateDirty             = "dirty"
	MigrationStatePending           = "pending"
	MigrationStateOutOfOrder        = "out-of-order"
	MigrationStateCheckpointCovered = "checkpoint-covered"
)

// migrationStates is every state this operator knows how to act on. A document
// carrying another one is refused: an unknown state is a decision this operator
// has not been taught to make, and guessing it is how a migration runs twice.
var migrationStates = []string{
	MigrationStateApplied,
	MigrationStateModified,
	MigrationStateDirty,
	MigrationStatePending,
	MigrationStateOutOfOrder,
	MigrationStateCheckpointCovered,
}

// migrationOutcomes is every outcome this operator knows. The two failures are
// separate instructions, so an unrecognized one is never folded into either.
var migrationOutcomes = []string{
	MigrationOutcomeUpToDate,
	MigrationOutcomeApplied,
	MigrationOutcomeDryRun,
	MigrationOutcomeFailed,
	MigrationOutcomePartial,
	MigrationOutcomeUnknown,
}

// IsKnownMigrationOutcome reports whether outcome is one a run document may
// carry. The runner's termination summary repeats a run's outcome, and it is
// held to the same set the document was.
func IsKnownMigrationOutcome(outcome string) bool {
	return slices.Contains(migrationOutcomes, outcome)
}

// Outcomes a run document reports.
const (
	MigrationOutcomeUpToDate = "up-to-date"
	MigrationOutcomeApplied  = "applied"
	MigrationOutcomeDryRun   = "dry-run"
	MigrationOutcomeFailed   = "failed"
	MigrationOutcomePartial  = "partial"
	MigrationOutcomeUnknown  = "unknown"
)

// MigrationRecord is one migration of the artifact as the database accounts for
// it.
type MigrationRecord struct {
	Version         int64  `json:"version"`
	VersionKey      string `json:"version_key,omitempty"`
	Description     string `json:"description,omitempty"`
	Checksum        string `json:"checksum,omitempty"`
	AppliedChecksum string `json:"applied_checksum,omitempty"`
	Checkpoint      bool   `json:"checkpoint,omitempty"`
	TransactionMode string `json:"transaction_mode,omitempty"`
	// AppliedAt is when the migration's revision row was written, and
	// ExecutionMs how long it ran in whole milliseconds, as the row recorded
	// them. Ptah reports both only for a row that exists, and no duration for
	// a revision table too old to record one. Neither is part of the history
	// fingerprint: a plan's premise is which migrations are applied, not when.
	AppliedAt   *time.Time `json:"applied_at,omitempty"`
	ExecutionMs *int64     `json:"execution_time_ms,omitempty"`
	State       string     `json:"state"`
}

// HasRevisionRow reports whether the database holds a revision row for the
// record: an applied migration, a modified one, or the dirty one a failed run
// left behind.
func (m MigrationRecord) HasRevisionRow() bool {
	return m.State == MigrationStateApplied || m.State == MigrationStateModified || m.State == MigrationStateDirty
}

// Bounds on what a record names. A description is the rest of a file name,
// which no file system lets exceed 255 bytes, and a checksum is a digest
// spelled out; anything longer did not come from a migration file.
const (
	MaxMigrationDescriptionBytes = 255
	MaxMigrationChecksumBytes    = 128
)

// MigrationStatusReport is Ptah's `migrations status --json` document.
type MigrationStatusReport struct {
	ContractVersion   int   `json:"contract_version"`
	CurrentVersion    int64 `json:"current_version"`
	CheckpointVersion int64 `json:"checkpoint_version,omitempty"`
	TotalMigrations   int   `json:"total_migrations"`
	HasPendingChanges bool  `json:"has_pending_changes"`
	// PendingMigrations is the selection Ptah itself made: the versions its
	// next run would execute, in the order it would execute them. It is not
	// the same question the per-migration states answer, and after a
	// checkpoint bootstrap the two disagree on purpose -- a version the
	// checkpoint replaced is reported pending once the bootstrap is no longer
	// what the database is doing, and Ptah's own floor still skips it.
	//
	// Carried without omitempty, because this report is decoded from Ptah and
	// encoded again into the runner's result frame, and an empty selection is
	// the answer that matters: it says the next run would execute nothing.
	// Dropped from the frame, it reaches the controller as an absent field,
	// which [MigrationStatusReport.Pending] reads as a build that reports no
	// selection at all and answers by recounting the states. Ptah declares the
	// field the same way for the same reason.
	PendingMigrations []int64           `json:"pending_migrations"`
	Migrations        []MigrationRecord `json:"migrations,omitempty"`
	DirtyRevision     *MigrationDirty   `json:"dirty_revision,omitempty"`
	// Repeatables are the repeatable migrations Ptah lists since v0.30.0: each
	// R__<identity>.sql file of the directory, in the order a run executes
	// them, then each row of the record table no file carries any more
	// (stokaro/ptah#6156). What a record says about a failed attempt, which
	// quotes the SQL and the server's answer, is not read into it.
	Repeatables []MigrationRepeatable `json:"repeatables,omitempty"`
}

// Repeatable migration states a status document reports, as Ptah v0.33.1
// spells them.
const (
	// RepeatableStatePending is a file the database has no record of.
	RepeatableStatePending = "pending"
	// RepeatableStateChanged is a file whose content no longer hashes to the
	// digest recorded as applied.
	RepeatableStateChanged = "changed"
	// RepeatableStateApplied is a file whose content hashes to that digest.
	RepeatableStateApplied = "applied"
	// RepeatableStateRunning is an attempt that recorded its start and no
	// outcome: whether its statements took effect is not known.
	RepeatableStateRunning = "running"
	// RepeatableStateFailed is an attempt whose body failed. Inside a
	// transaction it took no effect; outside one, part of it may have.
	RepeatableStateFailed = "failed"
	// RepeatableStateOrphaned is a record no file carries any more, whose
	// last attempt applied.
	RepeatableStateOrphaned = "orphaned"
)

// repeatableStates is every repeatable state this operator knows how to act
// on. Ptah counts a state it does not know as unfinished and runs nothing;
// this operator refuses the document, for the reason it refuses an unknown
// migration state.
var repeatableStates = []string{
	RepeatableStatePending,
	RepeatableStateChanged,
	RepeatableStateApplied,
	RepeatableStateRunning,
	RepeatableStateFailed,
	RepeatableStateOrphaned,
}

// Reasons a run gives for executing a repeatable migration. The operator never
// asks Ptah to retry unfinished work, so a run it dispatched gives the first
// two only; retry is read so that a document is refused for what it says
// rather than for a word.
const (
	RepeatableReasonNew     = "new"
	RepeatableReasonChanged = "changed"
	RepeatableReasonRetry   = "retry"
)

var repeatableReasons = []string{RepeatableReasonNew, RepeatableReasonChanged, RepeatableReasonRetry}

// MigrationRepeatable is one repeatable migration a status lists: its
// identity, the file that holds it, and what the database records for it.
type MigrationRepeatable struct {
	Identity string `json:"identity"`
	// Path is the file's place in the directory, and Checksum the h1 digest
	// of its content as it is now. Both are empty for a record no file
	// carries, which Orphaned marks.
	Path     string `json:"path,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	Orphaned bool   `json:"orphaned,omitempty"`
	// AppliedChecksum is the digest of the content that last applied, empty
	// when none has.
	AppliedChecksum string `json:"applied_checksum,omitempty"`
	State           string `json:"state"`
	// NoTransaction reports that the last attempt ran outside a transaction,
	// so a failed or stopped one may have left part of its work behind.
	NoTransaction bool `json:"no_transaction,omitempty"`
}

// Unfinished reports a record whose last attempt failed or recorded no
// outcome. Ptah refuses every run while one exists.
func (r MigrationRepeatable) Unfinished() bool {
	return r.State == RepeatableStateFailed || r.State == RepeatableStateRunning
}

// Pending reports a file the next run would execute: new, or changed since it
// last applied.
func (r MigrationRepeatable) Pending() bool {
	return !r.Orphaned && (r.State == RepeatableStatePending || r.State == RepeatableStateChanged)
}

// Bounds on what a repeatable migration names: an identity is part of a file
// name, and a path is a file's place in the directory.
const (
	MaxMigrationRepeatableIdentityBytes = 255
	MaxMigrationRepeatablePathBytes     = 1024
)

// repeatableIdentityPattern is the identity a repeatable migration file may
// carry, as Ptah parses R__<identity>.sql: ASCII letters, digits, underscores
// and hyphens. A record no file carries was read from the database, which may
// hold any text, so only a file's identity is held to it.
var repeatableIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// IsRepeatableIdentity reports whether identity is one a repeatable migration
// file can carry.
func IsRepeatableIdentity(identity string) bool {
	return len(identity) <= MaxMigrationRepeatableIdentityBytes && repeatableIdentityPattern.MatchString(identity)
}

// repeatableChecksumPattern is the h1 digest Ptah records for a repeatable
// migration: base64 of a SHA-256, as ptah.sum writes it.
var repeatableChecksumPattern = regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`)

// IsRepeatableChecksum reports whether checksum is an h1 digest.
func IsRepeatableChecksum(checksum string) bool {
	return repeatableChecksumPattern.MatchString(checksum)
}

// MigrationRepeatableWork is one repeatable migration a run selected, in the
// order it executes them.
type MigrationRepeatableWork struct {
	Identity        string `json:"identity"`
	Path            string `json:"path"`
	Checksum        string `json:"checksum"`
	AppliedChecksum string `json:"applied_checksum,omitempty"`
	Reason          string `json:"reason"`
}

// MigrationDirty is the revision row a failed or interrupted run left behind.
type MigrationDirty struct {
	Version int64  `json:"version"`
	Applied int    `json:"applied"`
	Total   int    `json:"total"`
	Error   string `json:"error,omitempty"`
}

// MigrationRunReport is Ptah's `migrations up --json` document.
type MigrationRunReport struct {
	ContractVersion int     `json:"contract_version"`
	Direction       string  `json:"direction"`
	Outcome         string  `json:"outcome"`
	Planned         []int64 `json:"planned,omitempty"`
	Applied         []int64 `json:"applied,omitempty"`
	// PlannedRepeatables are the repeatable migrations the run selected under
	// the migration lock, to run after every planned version, and
	// AppliedRepeatables the identities of those whose record says the
	// selected content applied, in the same order. Ptah reports the selection
	// even for a run its expected sequence refused, so the two lists are what
	// tell a refused run from a failed one.
	PlannedRepeatables []MigrationRepeatableWork `json:"planned_repeatables,omitempty"`
	AppliedRepeatables []string                  `json:"applied_repeatables,omitempty"`
	// Error is why the run failed. Its message can quote a row, so the runner
	// reads its kind and drops the object before the report reaches a frame.
	Error   *CommandError        `json:"error,omitempty"`
	Failure *MigrationRunFailure `json:"failure,omitempty"`
	// MigrationLog is what the run's writes to the migration log came to. A
	// run that keeps no log, or a Ptah before v0.23.0, reports none, which
	// says nothing either way about the log.
	MigrationLog *MigrationLogReport    `json:"migration_log,omitempty"`
	Status       *MigrationStatusReport `json:"status,omitempty"`
	// DriftCheck is the check --check-drift ran, as Ptah wrote it. A run
	// that was not asked for one, or that stopped before it, carries none.
	// The runner reads it into DriftSummary and drops it before the report
	// reaches a frame.
	DriftCheck *MigrationDriftCheck `json:"drift_check,omitempty"`
	// DriftSummary is what a frame keeps of DriftCheck. Ptah writes no such
	// field.
	DriftSummary *MigrationDriftSummary `json:"driftSummary,omitempty"`
}

// StoppedBeforeSelecting reports a run that failed before Ptah selected any
// migration: it read no history, planned and applied nothing, and sent no
// migration statement. A target that could not be opened ends a run this way.
func (r MigrationRunReport) StoppedBeforeSelecting() bool {
	return r.Outcome == MigrationOutcomeFailed && r.Status == nil &&
		len(r.Planned) == 0 && len(r.Applied) == 0 && r.Failure == nil &&
		len(r.PlannedRepeatables) == 0 && len(r.AppliedRepeatables) == 0
}

// PlannedRepeatableIdentities names the repeatable migrations the run
// selected, in order.
func (r MigrationRunReport) PlannedRepeatableIdentities() []string {
	identities := make([]string, 0, len(r.PlannedRepeatables))
	for _, work := range r.PlannedRepeatables {
		identities = append(identities, work.Identity)
	}
	return identities
}

// MigrationRunFailure is the statement a run stopped at and the code the
// database answered with. Ptah writes it when a statement reached the database
// and failed, and since v0.23.0 also when the statements ran and the commit
// that ends them failed. Unlike Error's message it carries no text, so it names
// no row and may reach status and Events (#69, #678).
type MigrationRunFailure struct {
	Version    int64 `json:"version"`
	Statement  int   `json:"statement"`
	Statements int   `json:"statements"`
	// Commit is true when every statement ran and the commit that ends them
	// failed, as a deferred constraint does. No statement is to blame then,
	// and Statement is 0: that is Ptah's documented contract (docs/
	// exit_codes.md, "Where a Run Stopped", introduced in stokaro/ptah#5562).
	// Under --tx-mode all the one commit ends every selected migration, and
	// Version names the last of them.
	Commit bool `json:"commit,omitempty"`
	// SQLState is empty when the server gave no answer, and ErrorNumber is
	// zero on every protocol but MySQL's.
	SQLState    string `json:"sqlstate,omitempty"`
	ErrorNumber int    `json:"error_number,omitempty"`
}

// maxMigrationStatements bounds the statement counts a failure may report.
// The run message quotes both, and the bound keeps it inside the 256 bytes an
// Event keeps; no migration file comes near it.
const maxMigrationStatements = math.MaxInt32

// sqlStatePattern is the SQLSTATE form the SQL standard defines: a class and a
// subclass of digits and capital letters, five characters in all.
var sqlStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// validate holds a failure to the run that reported it. A failure on a run
// that applied everything, or in a migration it did not select or did apply,
// contradicts the document it came in, and that document is refused whole.
// Nothing here quotes the SQLSTATE back, because a value that failed its form
// could be anything.
func (f MigrationRunFailure) validate(report MigrationRunReport) error {
	if report.Outcome == MigrationOutcomeApplied || report.Outcome == MigrationOutcomeUpToDate {
		return fmt.Errorf("migration run reports outcome %q and a failed statement", report.Outcome)
	}
	if !slices.Contains(report.Planned, f.Version) || slices.Contains(report.Applied, f.Version) {
		return fmt.Errorf("migration run reports a statement of version %d failed, "+
			"and that version is not one it selected and left unapplied", f.Version)
	}
	switch {
	case f.Statements < 1 || f.Statements > maxMigrationStatements:
		return fmt.Errorf("migration run reports statement %d of %d failed", f.Statement, f.Statements)
	case f.Commit && f.Statement != 0:
		// The commit runs after every statement, so a document that blames
		// one of them as well says two things about the same failure.
		return fmt.Errorf("migration run reports statement %d of %d and the commit after them failed",
			f.Statement, f.Statements)
	case !f.Commit && (f.Statement < 1 || f.Statement > f.Statements):
		return fmt.Errorf("migration run reports statement %d of %d failed", f.Statement, f.Statements)
	}
	if f.SQLState != "" && !sqlStatePattern.MatchString(f.SQLState) {
		return fmt.Errorf("migration run reports a SQLSTATE that is not five digits or capital letters")
	}
	if f.ErrorNumber < 0 || f.ErrorNumber > math.MaxUint16 {
		return fmt.Errorf("migration run reports error number %d, which the two bytes MySQL sends cannot hold",
			f.ErrorNumber)
	}
	return nil
}

// Pending names the versions the next run would execute, in the order Ptah
// would execute them.
//
// The list is Ptah's, because deriving a second one from the per-migration
// states gets a database that bootstrapped from a checkpoint wrong: the
// versions the checkpoint replaced report themselves pending afterwards, while
// Ptah's own floor skips them and its selection is empty. A controller that
// recounted would publish a plan for migrations that cannot run, and publish it
// again after every run that correctly did nothing.
//
// A document that carries no selection at all is read the old way, so a build
// that reports only states still resolves to a list rather than to silence.
func (r MigrationStatusReport) Pending() []int64 {
	if r.PendingMigrations != nil {
		return append([]int64(nil), r.PendingMigrations...)
	}
	versions := make([]int64, 0, len(r.Migrations))
	for _, record := range r.Migrations {
		if record.State == MigrationStatePending || record.State == MigrationStateOutOfOrder {
			versions = append(versions, record.Version)
		}
	}
	return versions
}

// PendingRepeatables are the repeatable migrations the next run would
// execute, in the order it would execute them: the files that are new and the
// files that changed since they last applied.
func (r MigrationStatusReport) PendingRepeatables() []MigrationRepeatable {
	var pending []MigrationRepeatable
	for _, repeatable := range r.Repeatables {
		if repeatable.Pending() {
			pending = append(pending, repeatable)
		}
	}
	return pending
}

// UnfinishedRepeatables are the records whose last attempt failed or
// recorded no outcome, with a file or without one.
func (r MigrationStatusReport) UnfinishedRepeatables() []MigrationRepeatable {
	var unfinished []MigrationRepeatable
	for _, repeatable := range r.Repeatables {
		if repeatable.Unfinished() {
			unfinished = append(unfinished, repeatable)
		}
	}
	return unfinished
}

// OrphanedRepeatables are the records that applied and that no file carries
// any more. Ptah leaves them alone; a record without a file whose attempt did
// not finish is unfinished instead.
func (r MigrationStatusReport) OrphanedRepeatables() []MigrationRepeatable {
	var orphaned []MigrationRepeatable
	for _, repeatable := range r.Repeatables {
		if repeatable.State == RepeatableStateOrphaned {
			orphaned = append(orphaned, repeatable)
		}
	}
	return orphaned
}

// LastVersion is the highest version the artifact carries, and zero for an
// artifact that carries none. It is the artifact's own extent: the document
// lists the artifact's migrations with what the database did about each, so a
// revision beyond this number is one the artifact cannot account for at all.
func (r MigrationStatusReport) LastVersion() int64 {
	var last int64
	for _, record := range r.Migrations {
		if record.Version > last {
			last = record.Version
		}
	}
	return last
}

// Modified names the applied migrations whose files no longer account for them.
// Nothing may execute while one exists, and the controller never resolves it.
func (r MigrationStatusReport) Modified() []int64 {
	versions := make([]int64, 0)
	for _, record := range r.Migrations {
		if record.State == MigrationStateModified {
			versions = append(versions, record.Version)
		}
	}
	return versions
}

// OutOfOrder names the pending migrations that sort below a version the
// database has already applied. Ptah's linear execution order refuses a run
// while one exists, so a plan that carried them would be a decision nobody
// could execute.
func (r MigrationStatusReport) OutOfOrder() []int64 {
	versions := make([]int64, 0)
	for _, record := range r.Migrations {
		if record.State == MigrationStateOutOfOrder {
			versions = append(versions, record.Version)
		}
	}
	return versions
}

// migrationEnvelope is what a top-level migration document says about
// itself: the command that printed it, and the error a failure carries.
type migrationEnvelope struct {
	Command string        `json:"command"`
	Error   *CommandError `json:"error"`
}

// DecodeMigrationStatus reads a status document and refuses everything it
// cannot fully account for.
//
// The failure document `migrations status` prints when it cannot read the
// history is refused here, before anything reads it as a status: it carries
// the status contract version and none of the status fields, so it would
// otherwise read as a database with nothing applied.
// DecodeMigrationStatusFailure reads it.
func DecodeMigrationStatus(data []byte) (MigrationStatusReport, error) {
	var envelope migrationEnvelope
	if err := decodeJSON(data, &envelope, false); err != nil {
		return MigrationStatusReport{}, fmt.Errorf("decode migration status: %w", err)
	}
	if envelope.Command != MigrationStatusCommand {
		return MigrationStatusReport{}, fmt.Errorf("migration status names another command than %s", MigrationStatusCommand)
	}
	if envelope.Error != nil {
		return MigrationStatusReport{}, fmt.Errorf("migration status is a failure document")
	}
	var report MigrationStatusReport
	if err := decodeJSON(data, &report, false); err != nil {
		return MigrationStatusReport{}, fmt.Errorf("decode migration status: %w", err)
	}
	if err := report.validate(); err != nil {
		return MigrationStatusReport{}, err
	}
	return report, nil
}

func (r MigrationStatusReport) validate() error {
	if r.ContractVersion != SupportedMigrationStatusContract {
		return fmt.Errorf(
			"migration status contract version %d is not the %d this operator reads",
			r.ContractVersion, SupportedMigrationStatusContract,
		)
	}
	if r.CurrentVersion < 0 || r.CheckpointVersion < 0 || r.TotalMigrations < 0 {
		return fmt.Errorf("migration status reports a negative count or version")
	}
	if len(r.Migrations) > r.TotalMigrations {
		return fmt.Errorf(
			"migration status lists %d migrations and counts %d",
			len(r.Migrations), r.TotalMigrations,
		)
	}
	seen := make(map[int64]struct{}, len(r.Migrations))
	for _, record := range r.Migrations {
		if record.Version <= 0 {
			return fmt.Errorf("migration status lists a migration with no version")
		}
		if _, repeated := seen[record.Version]; repeated {
			return fmt.Errorf("migration status lists version %d twice", record.Version)
		}
		seen[record.Version] = struct{}{}
		if !slices.Contains(migrationStates, record.State) {
			return fmt.Errorf(
				"migration status reports state %q for version %d, which this operator does not know",
				record.State, record.Version,
			)
		}
		if err := record.validateRow(); err != nil {
			return err
		}
	}
	if r.DirtyRevision != nil && r.DirtyRevision.Version <= 0 {
		return fmt.Errorf("migration status reports a dirty revision with no version")
	}
	identities := make(map[string]struct{}, len(r.Repeatables))
	for _, repeatable := range r.Repeatables {
		if err := repeatable.validate(); err != nil {
			return err
		}
		if _, repeated := identities[repeatable.Identity]; repeated {
			return fmt.Errorf("migration status lists one repeatable migration twice")
		}
		identities[repeatable.Identity] = struct{}{}
	}
	return nil
}

// validate holds one repeatable migration to what its state says. A file
// carries an identity a file name can carry and the digest of its content; a
// record no file carries has neither a path nor a digest, and its last attempt
// either applied, which Ptah reports as orphaned, or did not finish. Nothing
// here quotes an identity back, because a record's may be any text.
func (r MigrationRepeatable) validate() error {
	if r.Identity == "" || len(r.Identity) > MaxMigrationRepeatableIdentityBytes ||
		len(r.Path) > MaxMigrationRepeatablePathBytes {
		return fmt.Errorf("migration status lists a repeatable migration with no identity, or with a name longer than a file name holds")
	}
	if !slices.Contains(repeatableStates, r.State) {
		return fmt.Errorf("migration status reports repeatable state %q, which this operator does not know", r.State)
	}
	if r.AppliedChecksum != "" && !IsRepeatableChecksum(r.AppliedChecksum) {
		return fmt.Errorf("migration status reports a repeatable migration whose applied digest is not an h1 digest")
	}
	if r.Orphaned {
		switch {
		case r.Path != "" || r.Checksum != "":
			return fmt.Errorf("migration status reports a repeatable record no file carries, with a file")
		case r.State != RepeatableStateOrphaned && !r.Unfinished():
			return fmt.Errorf("migration status reports a repeatable record no file carries in state %q", r.State)
		}
		return nil
	}
	switch {
	case r.State == RepeatableStateOrphaned:
		return fmt.Errorf("migration status reports an orphaned repeatable migration that a file carries")
	case r.Path == "" || !IsRepeatableIdentity(r.Identity) || !IsRepeatableChecksum(r.Checksum):
		return fmt.Errorf("migration status lists a repeatable migration file without a path, " +
			"an identity a file name can carry, or an h1 digest")
	case r.State == RepeatableStatePending && r.AppliedChecksum != "":
		return fmt.Errorf("migration status reports a pending repeatable migration with applied content")
	case r.State == RepeatableStateChanged && (r.AppliedChecksum == "" || r.AppliedChecksum == r.Checksum):
		return fmt.Errorf("migration status reports a changed repeatable migration whose content did not change")
	case r.State == RepeatableStateApplied && r.AppliedChecksum != r.Checksum:
		return fmt.Errorf("migration status reports an applied repeatable migration whose content is not the applied one")
	}
	return nil
}

// ValidateRepeatables holds a run's repeatable selection to the run: each
// selected file once, with an identity a file name carries, an h1 digest and
// a reason Ptah gives, and each applied one selected, in the order selected.
func (r MigrationRunReport) ValidateRepeatables() error {
	selected := make(map[string]struct{}, len(r.PlannedRepeatables))
	for _, work := range r.PlannedRepeatables {
		switch {
		case !IsRepeatableIdentity(work.Identity) || work.Path == "" || len(work.Path) > MaxMigrationRepeatablePathBytes:
			return fmt.Errorf("migration run selected a repeatable migration without an identity a file name " +
				"can carry, or without a path")
		case !IsRepeatableChecksum(work.Checksum) || work.AppliedChecksum != "" && !IsRepeatableChecksum(work.AppliedChecksum):
			return fmt.Errorf("migration run selected repeatable migration %s without an h1 digest", work.Identity)
		case !slices.Contains(repeatableReasons, work.Reason):
			return fmt.Errorf("migration run selected repeatable migration %s for reason %q, which this operator does not know",
				work.Identity, work.Reason)
		case work.Reason == RepeatableReasonNew && work.AppliedChecksum != "":
			return fmt.Errorf("migration run selected repeatable migration %s as new, with applied content", work.Identity)
		case work.Reason == RepeatableReasonChanged && (work.AppliedChecksum == "" || work.AppliedChecksum == work.Checksum):
			return fmt.Errorf("migration run selected repeatable migration %s as changed, and its content did not change",
				work.Identity)
		}
		if _, repeated := selected[work.Identity]; repeated {
			return fmt.Errorf("migration run selected repeatable migration %s twice", work.Identity)
		}
		selected[work.Identity] = struct{}{}
	}
	next := 0
	for _, identity := range r.AppliedRepeatables {
		index := slices.IndexFunc(r.PlannedRepeatables[next:], func(work MigrationRepeatableWork) bool {
			return work.Identity == identity
		})
		if index < 0 {
			return fmt.Errorf("migration run reports a repeatable migration applied that it did not select, " +
				"or out of the order it selected them")
		}
		next += index + 1
	}
	return nil
}

// validateRow holds what a record says of its revision row to the state it
// reports. A time on a migration that has no row, or a duration with no time,
// contradicts the document it came in.
func (m MigrationRecord) validateRow() error {
	if len(m.Description) > MaxMigrationDescriptionBytes {
		return fmt.Errorf("migration status reports a description of %d bytes for version %d, "+
			"longer than a file name holds", len(m.Description), m.Version)
	}
	if len(m.Checksum) > MaxMigrationChecksumBytes || len(m.AppliedChecksum) > MaxMigrationChecksumBytes {
		return fmt.Errorf("migration status reports a checksum longer than %d bytes for version %d",
			MaxMigrationChecksumBytes, m.Version)
	}
	if (m.AppliedAt != nil || m.ExecutionMs != nil) && !m.HasRevisionRow() {
		return fmt.Errorf("migration status reports when version %d ran, and its state %q has no revision row",
			m.Version, m.State)
	}
	if m.ExecutionMs != nil && m.AppliedAt == nil {
		return fmt.Errorf("migration status reports how long version %d ran and not when", m.Version)
	}
	if m.ExecutionMs != nil && *m.ExecutionMs < 0 {
		return fmt.Errorf("migration status reports version %d ran for %d milliseconds", m.Version, *m.ExecutionMs)
	}
	return nil
}

// DecodeMigrationRun reads a run document, including the status it carries.
func DecodeMigrationRun(data []byte) (MigrationRunReport, error) {
	var envelope migrationEnvelope
	if err := decodeJSON(data, &envelope, false); err != nil {
		return MigrationRunReport{}, fmt.Errorf("decode migration run: %w", err)
	}
	if envelope.Command != MigrationRunCommand {
		return MigrationRunReport{}, fmt.Errorf("migration run names another command than %s", MigrationRunCommand)
	}
	var report MigrationRunReport
	if err := decodeJSON(data, &report, false); err != nil {
		return MigrationRunReport{}, fmt.Errorf("decode migration run: %w", err)
	}
	if report.ContractVersion != SupportedMigrationRunContract {
		return MigrationRunReport{}, fmt.Errorf(
			"migration run contract version %d is not the %d this operator reads",
			report.ContractVersion, SupportedMigrationRunContract,
		)
	}
	if report.Direction != "up" {
		return MigrationRunReport{}, fmt.Errorf(
			"migration run reports direction %q; this operator dispatches up only",
			report.Direction,
		)
	}
	if !slices.Contains(migrationOutcomes, report.Outcome) {
		return MigrationRunReport{}, fmt.Errorf(
			"migration run reports outcome %q, which this operator does not know",
			report.Outcome,
		)
	}
	if report.Status != nil {
		if err := report.Status.validate(); err != nil {
			return MigrationRunReport{}, err
		}
	}
	for _, version := range report.Applied {
		if !slices.Contains(report.Planned, version) {
			return MigrationRunReport{}, fmt.Errorf(
				"migration run reports version %d applied and never planned",
				version,
			)
		}
	}
	if err := report.ValidateRepeatables(); err != nil {
		return MigrationRunReport{}, err
	}
	if report.Failure != nil {
		if err := report.Failure.validate(report); err != nil {
			return MigrationRunReport{}, err
		}
	}
	if report.MigrationLog != nil {
		if err := report.MigrationLog.Validate(report.Planned, report.PlannedRepeatableIdentities()); err != nil {
			return MigrationRunReport{}, err
		}
	}
	if report.DriftSummary != nil {
		return MigrationRunReport{}, errors.New("migration run carries a drift summary, which Ptah does not write")
	}
	if err := report.validateDriftCheck(); err != nil {
		return MigrationRunReport{}, err
	}
	// A failed run that read no history afterwards stopped before it
	// selected anything. One that names a selection, an applied version or a
	// failed statement contradicts that, and nothing could say what the
	// database holds after it.
	if report.Outcome == MigrationOutcomeFailed && report.Status == nil && !report.StoppedBeforeSelecting() {
		return MigrationRunReport{}, fmt.Errorf(
			"migration run reports failed with no history and names a selection, an applied version or a failed statement")
	}
	return report, nil
}
