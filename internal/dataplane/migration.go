package dataplane

import (
	"fmt"
	"slices"
)

const (
	// SupportedMigrationStatusContract and SupportedMigrationRunContract are the
	// Ptah machine-contract versions this operator reads.
	//
	// A document announcing another version is refused rather than read: the
	// fields this operator knows may still be present and may already mean
	// something else, and a history read under the wrong contract is a history
	// nobody checked.
	SupportedMigrationStatusContract = 1
	SupportedMigrationRunContract    = 1

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
	State           string `json:"state"`
}

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
	PendingMigrations []int64           `json:"pending_migrations,omitempty"`
	Migrations        []MigrationRecord `json:"migrations,omitempty"`
	DirtyRevision     *MigrationDirty   `json:"dirty_revision,omitempty"`
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
	ContractVersion int                    `json:"contract_version"`
	Direction       string                 `json:"direction"`
	Outcome         string                 `json:"outcome"`
	Planned         []int64                `json:"planned,omitempty"`
	Applied         []int64                `json:"applied,omitempty"`
	Error           string                 `json:"error,omitempty"`
	Status          *MigrationStatusReport `json:"status,omitempty"`
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

// DecodeMigrationStatus reads a status document and refuses everything it
// cannot fully account for.
func DecodeMigrationStatus(data []byte) (MigrationStatusReport, error) {
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
	}
	if r.DirtyRevision != nil && r.DirtyRevision.Version <= 0 {
		return fmt.Errorf("migration status reports a dirty revision with no version")
	}
	return nil
}

// DecodeMigrationRun reads a run document, including the status it carries.
func DecodeMigrationRun(data []byte) (MigrationRunReport, error) {
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
	return report, nil
}
