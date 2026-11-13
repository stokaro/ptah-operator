package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
)

const (
	// SupportedSchemaPlanContract and SupportedSchemaApplyContract are the
	// versions of the documents `ptah schema plan --json` and `ptah schema
	// apply --json` print that this operator reads.
	//
	// Ptah raises a version when a field changes meaning or leaves, and when an
	// outcome gains a value. A document announcing another version is refused
	// rather than read for the fields this operator recognizes, because those
	// fields may already mean something else.
	//
	// Plan version 3 and apply version 4 are stokaro/ptah#5173's. Each carries
	// error as an object, the kind of a failure to open the target and Ptah's
	// message, where the versions before carried the message alone, and names
	// its command. Every outcome, the plan digest, the units an apply lists and
	// the refusal mean what they meant in plan version 2 and apply version 3.
	SupportedSchemaPlanContract  = 3
	SupportedSchemaApplyContract = 4

	// SchemaPlanCommand and SchemaApplyCommand are the commands the two
	// documents name. A document naming another is refused.
	SchemaPlanCommand  = "schema plan"
	SchemaApplyCommand = "schema apply"
)

// Outcomes a plan document reports.
const (
	SchemaPlanOutcomeChanges   = "changes"
	SchemaPlanOutcomeNoChanges = "no-changes"
	SchemaPlanOutcomeRefused   = "refused"
	SchemaPlanOutcomeFailed    = "failed"
)

// Outcomes an apply document reports. Refused and failed say that nothing
// reached the target; unknown says the statements were sent and the run
// returned an error.
const (
	SchemaApplyOutcomeApplied   = "applied"
	SchemaApplyOutcomeNoChanges = "no-changes"
	SchemaApplyOutcomeDryRun    = "dry-run"
	SchemaApplyOutcomeCanceled  = "canceled"
	SchemaApplyOutcomeRefused   = "refused"
	SchemaApplyOutcomeFailed    = "failed"
	SchemaApplyOutcomeUnknown   = "unknown"
)

// Refusal codes this operator acts on. Ptah may add codes without raising the
// contract version, since every refusal means nothing reached the target, so a
// code outside this list is read as a refusal of no particular kind rather than
// refused as a document.
const (
	SchemaRefusalStalePlan      = "stale-plan"
	SchemaRefusalProtectedTable = "protected-table"
	// SchemaRefusalDeclarationRefused is Ptah v0.23.0's refusal of a desired
	// schema whose declarations contradict each other, such as a foreign key
	// that references an unknown table. Ptah v0.22.0 reported the same fault
	// as a failed plan, with the same error message.
	SchemaRefusalDeclarationRefused = "declaration-refused"
)

var (
	schemaPlanOutcomes = []string{
		SchemaPlanOutcomeChanges,
		SchemaPlanOutcomeNoChanges,
		SchemaPlanOutcomeRefused,
		SchemaPlanOutcomeFailed,
	}
	schemaApplyOutcomes = []string{
		SchemaApplyOutcomeApplied,
		SchemaApplyOutcomeNoChanges,
		SchemaApplyOutcomeDryRun,
		SchemaApplyOutcomeCanceled,
		SchemaApplyOutcomeRefused,
		SchemaApplyOutcomeFailed,
		SchemaApplyOutcomeUnknown,
	}
	// schemaRefusalCodePattern is the shape a refusal code may take. A code is
	// an identifier the runner may name in a message, so a value that is not
	// one is a malformed document rather than a new code.
	schemaRefusalCodePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// SchemaRefusal is why a plan or an apply refused. Only the fields its code
// describes are set.
type SchemaRefusal struct {
	Code string `json:"code"`
	// Tables are the fenced tables a protected-table refusal names.
	Tables []string `json:"tables,omitempty"`
	// Changed is what moved under a stale-plan refusal: "schema" or "rows".
	Changed             string `json:"changed,omitempty"`
	PlanFingerprint     string `json:"plan_fingerprint,omitempty"`
	DatabaseFingerprint string `json:"database_fingerprint,omitempty"`
}

// SchemaPlanReport is Ptah's `schema plan --json` document.
//
// The plan document it embeds is not declared, so it is not read: the file
// --output wrote is the plan, and PlanDigest is what binds the report to it.
type SchemaPlanReport struct {
	ContractVersion int            `json:"contract_version"`
	Command         string         `json:"command"`
	Outcome         string         `json:"outcome"`
	PlanDigest      string         `json:"plan_digest,omitempty"`
	PlanPath        string         `json:"plan_path,omitempty"`
	Refusal         *SchemaRefusal `json:"refusal,omitempty"`
	// Error is why the run failed or refused. Its message is the sentence
	// Ptah printed on standard error and may quote a statement, so it is for a
	// fence refusal's tables and nothing else; its kind names a target that
	// could not be opened.
	Error *CommandError `json:"error,omitempty"`
}

// SchemaApplyReport is Ptah's `schema apply --json` document.
type SchemaApplyReport struct {
	ContractVersion int               `json:"contract_version"`
	Command         string            `json:"command"`
	Outcome         string            `json:"outcome"`
	PlanName        string            `json:"plan_name,omitempty"`
	PlanDigest      string            `json:"plan_digest,omitempty"`
	Units           []SchemaApplyUnit `json:"units,omitempty"`
	Refusal         *SchemaRefusal    `json:"refusal,omitempty"`
	// Error is why the run failed, refused or ended unknown. Only its kind is
	// read.
	Error *CommandError `json:"error,omitempty"`
	// Failure is where a run that sent a statement stopped, and the code the
	// database answered with. It carries no text, so it is what may leave the
	// Pod. A build before stokaro/ptah#5562 writes none.
	Failure *SchemaApplyFailure `json:"failure,omitempty"`
}

// SchemaApplyFailure is the unit or the commit a schema apply stopped at, as
// Ptah's apply document names it (docs/exit_codes.md, "Where a Run Stopped").
type SchemaApplyFailure struct {
	// Statement is the position of the failed unit among the units the
	// document lists, counting from one. It is zero when no unit is to blame:
	// the commit after them failed, or the run confirmed every unit and still
	// did not succeed.
	Statement int `json:"statement"`
	// Statements is how many units the document lists.
	Statements int `json:"statements"`
	// Commit is true when every unit ran and the commit that ends them failed.
	Commit bool `json:"commit,omitempty"`
	// SQLState is empty when the server gave no answer, and ErrorNumber is
	// zero on every protocol but MySQL's.
	SQLState    string `json:"sqlstate,omitempty"`
	ErrorNumber int    `json:"error_number,omitempty"`
}

// Validate holds a failure to the forms Ptah's contract gives it. Nothing here
// quotes the SQLSTATE back, because a value that failed its form could be
// anything.
func (f SchemaApplyFailure) Validate() error {
	switch {
	case f.Statements < 1 || f.Statements > maxSchemaApplyStatements:
		return fmt.Errorf("schema apply reports statement %d of %d failed", f.Statement, f.Statements)
	case f.Commit && f.Statement != 0:
		// The commit runs after every statement, so a document that blames
		// one of them as well says two things about the same failure.
		return fmt.Errorf("schema apply reports statement %d of %d and the commit after them failed", f.Statement, f.Statements)
	case f.Statement < 0 || f.Statement > f.Statements:
		return fmt.Errorf("schema apply reports statement %d of %d failed", f.Statement, f.Statements)
	case f.SQLState != "" && !sqlStatePattern.MatchString(f.SQLState):
		return fmt.Errorf("schema apply reports a SQLSTATE that is not five digits or capital letters")
	case f.ErrorNumber < 0 || f.ErrorNumber > math.MaxUint16:
		return fmt.Errorf("schema apply reports error number %d, which the two bytes MySQL sends cannot hold", f.ErrorNumber)
	}
	return nil
}

// Failed reports whether the failure names a statement the database refused.
// A failed commit, and a run that stopped with every unit confirmed, name none.
func (f SchemaApplyFailure) Failed() bool {
	return !f.Commit && f.Statement >= 1
}

// maxSchemaApplyStatements bounds the unit count a failure may report, so the
// position and the count fit the int32 the status keeps them in.
const maxSchemaApplyStatements = math.MaxInt32

// SchemaApplyUnit is one planned operation an apply document lists: its SQL,
// or a summary of a native provider operation that has none.
type SchemaApplyUnit struct {
	SQL    string          `json:"sql,omitempty"`
	Native json.RawMessage `json:"native,omitempty"`
}

// ErrNativeApplyUnit is a listed operation that is not SQL. This operator
// plans and reviews SQL statements, so it cannot account for one.
var ErrNativeApplyUnit = errors.New("schema apply lists an operation that is not SQL")

// SQLStatements is the SQL of each listed unit, in order. A unit that is a
// native operation, or that has no SQL, is refused rather than skipped.
func (r SchemaApplyReport) SQLStatements() ([]string, error) {
	statements := make([]string, 0, len(r.Units))
	for _, unit := range r.Units {
		if len(unit.Native) != 0 || unit.SQL == "" {
			return nil, ErrNativeApplyUnit
		}
		statements = append(statements, unit.SQL)
	}
	return statements, nil
}

// DecodeSchemaPlan reads one plan document and refuses anything it cannot
// account for: another contract version, an outcome it does not know, a
// second JSON value, and an outcome whose fields contradict it.
//
// Fields it does not declare are ignored, because Ptah adds fields without
// raising the version.
func DecodeSchemaPlan(data []byte) (SchemaPlanReport, error) {
	var report SchemaPlanReport
	if err := decodeJSON(data, &report, false); err != nil {
		return SchemaPlanReport{}, fmt.Errorf("decode schema plan report: %w", err)
	}
	if report.ContractVersion != SupportedSchemaPlanContract {
		return SchemaPlanReport{}, fmt.Errorf(
			"schema plan contract version %d is not the %d this operator reads",
			report.ContractVersion, SupportedSchemaPlanContract,
		)
	}
	if report.Command != SchemaPlanCommand {
		return SchemaPlanReport{}, fmt.Errorf("schema plan report names another command than %s", SchemaPlanCommand)
	}
	if !slices.Contains(schemaPlanOutcomes, report.Outcome) {
		return SchemaPlanReport{}, fmt.Errorf("schema plan reports outcome %q, which this operator does not know", report.Outcome)
	}
	if err := validateSchemaRefusal(report.Outcome == SchemaPlanOutcomeRefused, report.Refusal); err != nil {
		return SchemaPlanReport{}, fmt.Errorf("schema plan %w", err)
	}
	switch report.Outcome {
	case SchemaPlanOutcomeChanges:
		if !validDigest(report.PlanDigest) || report.PlanPath == "" {
			return SchemaPlanReport{}, fmt.Errorf("schema plan reports changes without the digest and path of the plan it saved")
		}
	default:
		if report.PlanDigest != "" || report.PlanPath != "" {
			return SchemaPlanReport{}, fmt.Errorf("schema plan reports %s and names a saved plan", report.Outcome)
		}
	}
	return report, nil
}

// DecodeSchemaApply reads one apply document under the rules DecodeSchemaPlan
// states.
func DecodeSchemaApply(data []byte) (SchemaApplyReport, error) {
	var report SchemaApplyReport
	if err := decodeJSON(data, &report, false); err != nil {
		return SchemaApplyReport{}, fmt.Errorf("decode schema apply report: %w", err)
	}
	if report.ContractVersion != SupportedSchemaApplyContract {
		return SchemaApplyReport{}, fmt.Errorf(
			"schema apply contract version %d is not the %d this operator reads",
			report.ContractVersion, SupportedSchemaApplyContract,
		)
	}
	if report.Command != SchemaApplyCommand {
		return SchemaApplyReport{}, fmt.Errorf("schema apply report names another command than %s", SchemaApplyCommand)
	}
	if !slices.Contains(schemaApplyOutcomes, report.Outcome) {
		return SchemaApplyReport{}, fmt.Errorf("schema apply reports outcome %q, which this operator does not know", report.Outcome)
	}
	if err := validateSchemaRefusal(report.Outcome == SchemaApplyOutcomeRefused, report.Refusal); err != nil {
		return SchemaApplyReport{}, fmt.Errorf("schema apply %w", err)
	}
	if report.PlanDigest != "" && !validDigest(report.PlanDigest) {
		return SchemaApplyReport{}, fmt.Errorf("schema apply names a plan digest that is not an exact SHA-256 digest")
	}
	if failure := report.Failure; failure != nil {
		// Only a run that sent a statement and returned an error says where it
		// stopped: unknown, or failed after a rollback the database confirmed.
		if report.Outcome != SchemaApplyOutcomeUnknown && report.Outcome != SchemaApplyOutcomeFailed {
			return SchemaApplyReport{}, fmt.Errorf("schema apply reports outcome %q and a failed statement", report.Outcome)
		}
		if err := failure.Validate(); err != nil {
			return SchemaApplyReport{}, err
		}
	}
	return report, nil
}

// validateSchemaRefusal holds a refusal to its outcome: present exactly when
// the outcome is refused, and carrying a code.
func validateSchemaRefusal(refused bool, refusal *SchemaRefusal) error {
	switch {
	case refused && refusal == nil:
		return fmt.Errorf("reports a refusal without its reason")
	case !refused && refusal != nil:
		return fmt.Errorf("carries a refusal on an outcome that is not refused")
	case refused && !schemaRefusalCodePattern.MatchString(refusal.Code):
		return fmt.Errorf("reports a refusal whose code is not an identifier")
	default:
		return nil
	}
}
