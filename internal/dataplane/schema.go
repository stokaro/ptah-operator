package dataplane

import (
	"fmt"
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
	SupportedSchemaPlanContract  = 1
	SupportedSchemaApplyContract = 1
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
	Outcome         string         `json:"outcome"`
	PlanDigest      string         `json:"plan_digest,omitempty"`
	PlanPath        string         `json:"plan_path,omitempty"`
	Refusal         *SchemaRefusal `json:"refusal,omitempty"`
	// Error is the sentence Ptah printed on standard error. It may quote a
	// statement, so it is for a fence refusal's tables and nothing else.
	Error string `json:"error,omitempty"`
}

// SchemaApplyReport is Ptah's `schema apply --json` document.
type SchemaApplyReport struct {
	ContractVersion int            `json:"contract_version"`
	Outcome         string         `json:"outcome"`
	PlanName        string         `json:"plan_name,omitempty"`
	PlanDigest      string         `json:"plan_digest,omitempty"`
	Statements      []string       `json:"statements,omitempty"`
	Refusal         *SchemaRefusal `json:"refusal,omitempty"`
	Error           string         `json:"error,omitempty"`
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
	if !slices.Contains(schemaApplyOutcomes, report.Outcome) {
		return SchemaApplyReport{}, fmt.Errorf("schema apply reports outcome %q, which this operator does not know", report.Outcome)
	}
	if err := validateSchemaRefusal(report.Outcome == SchemaApplyOutcomeRefused, report.Refusal); err != nil {
		return SchemaApplyReport{}, fmt.Errorf("schema apply %w", err)
	}
	if report.PlanDigest != "" && !validDigest(report.PlanDigest) {
		return SchemaApplyReport{}, fmt.Errorf("schema apply names a plan digest that is not an exact SHA-256 digest")
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
