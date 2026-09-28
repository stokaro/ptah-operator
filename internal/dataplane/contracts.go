// Package dataplane defines the narrow machine-readable contracts consumed by
// the controller. It deliberately does not import the full Ptah module.
package dataplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/stokaro/ptah-operator/internal/ocireference"
)

const (
	PlanFormatVersion     = 1
	SchemaArtifactType    = "application/vnd.stokaro.ptah.schema.v1"
	MigrationArtifactType = "application/vnd.stokaro.ptah.migrations.v1"

	// DriftFindingVocabularyVersion identifies the closed category vocabulary
	// accepted from native drift reports. Extending this vocabulary is a runner
	// protocol change: an older controller must reject, rather than publish, a
	// category whose disclosure contract it does not understand.
	//
	// Nothing reads this number. The refusal it describes is delivered by the
	// vocabulary itself, at both ends: a controller decoding a report and a
	// controller parsing a result frame each ask IsKnownDriftFindingCategory,
	// and an unknown category is refused there rather than compared here.
	DriftFindingVocabularyVersion = 1
)

var (
	mediaTypePattern               = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$`)
	verificationRequirementPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	driftFindingCategories         = []string{
		"columns_added",
		"columns_modified",
		"columns_removed",
		"constraints_added",
		"constraints_removed",
		"data_rows_deleted",
		"data_rows_inserted",
		"data_rows_updated",
		"enum_values_added",
		"enum_values_removed",
		"enums_added",
		"enums_removed",
		"extensions_added",
		"extensions_modified",
		"extensions_removed",
		"functions_added",
		"functions_modified",
		"functions_removed",
		"indexes_added",
		"indexes_removed",
		"rls_enabled_tables_added",
		"rls_enabled_tables_removed",
		"rls_force_added",
		"rls_force_removed",
		"rls_policies_added",
		"rls_policies_modified",
		"rls_policies_removed",
		"roles_added",
		"roles_modified",
		"roles_removed",
		"table_constraints_added",
		"table_constraints_removed",
		"tables_added",
		"tables_removed",
		"unique_protections_removed",
		"vector_dimension_changed",
	}
	driftFindingCategorySet = func() map[string]struct{} {
		categories := make(map[string]struct{}, len(driftFindingCategories))
		for _, category := range driftFindingCategories {
			categories[category] = struct{}{}
		}
		return categories
	}()
)

// DriftFindingCategories returns a copy of the closed machine vocabulary.
//
// It is the list the pinned Ptah can emit: support/ptah-drift-categories.json
// records that list, and hack/ptahdriftcategories holds the record to the
// pinned source. An unknown category is refused rather than published or
// dropped. The status enum could not store it, and leaving it out would
// understate the report's count and its highest severity.
//
// Extending the list moves DriftFindingVocabularyVersion, the PtahSchema
// status enum and the CRD schema version together. The frame version stays:
// a controller that predates a category refuses a frame carrying it, which is
// the refusal a version mismatch would give.
func DriftFindingCategories() []string {
	return append([]string(nil), driftFindingCategories...)
}

// IsKnownDriftFindingCategory reports whether category belongs to the closed
// v1 machine vocabulary.
func IsKnownDriftFindingCategory(category string) bool {
	_, known := driftFindingCategorySet[category]
	return known
}

type ResolveReport struct {
	Reference       string `json:"reference"`
	PinnedReference string `json:"pinned_reference"`
	Digest          string `json:"digest"`
	MediaType       string `json:"media_type"`
	Size            int64  `json:"size"`
}

type VerificationFinding struct {
	Requirement string `json:"requirement"`
	Detail      string `json:"detail"`
}

type VerifyReport struct {
	Reference string                `json:"reference"`
	Digest    string                `json:"digest"`
	Satisfied []string              `json:"satisfied"`
	Findings  []VerificationFinding `json:"findings"`
}

type InspectReport struct {
	Reference       string `json:"reference"`
	PinnedReference string `json:"pinned_reference"`
	Digest          string `json:"digest"`
	MediaType       string `json:"media_type"`
	Size            int64  `json:"size"`
	ArtifactType    string `json:"artifact_type"`
}

type DriftFinding struct {
	Category string `json:"category"`
	Count    int32  `json:"count"`
	Severity string `json:"severity"`
}

type DriftReport struct {
	Drift            bool            `json:"drift"`
	Failed           bool            `json:"failed"`
	FailureThreshold string          `json:"failure_threshold"`
	HighestSeverity  string          `json:"highest_severity"`
	Dialect          string          `json:"dialect,omitempty"`
	Sources          string          `json:"sources,omitempty"`
	DatabaseURL      string          `json:"database_url,omitempty"`
	IgnoredTables    []string        `json:"ignored_tables,omitempty"`
	Findings         []DriftFinding  `json:"findings,omitempty"`
	Diff             json.RawMessage `json:"diff,omitempty"`
	Error            string          `json:"error,omitempty"`
}

type PlanStatement struct {
	SQL      string `json:"sql"`
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
}

type PlanFile struct {
	FormatVersion   int      `json:"format_version"`
	Name            string   `json:"name"`
	Dialect         string   `json:"dialect"`
	FromFingerprint string   `json:"from_fingerprint"`
	ToFingerprint   string   `json:"to_fingerprint"`
	Exclude         []string `json:"exclude,omitempty"`
	// SchemasBeyondURL names the schemas the source fingerprint covers that the
	// connection URL's own scope does not: a desired state that names a schema
	// other than the one a schema-scoped URL connects to. Ptah reads the same
	// set again when it verifies the plan before applying it. The operator
	// carries the plan bytes as they are and reads nothing from this field; it
	// is declared so the strict decoder accepts a plan that has it.
	SchemasBeyondURL []string `json:"schemas_beyond_url,omitempty"`
	// ManagedRows and RowsFingerprint describe the declared row sets this plan
	// read, and the state it read them in. A plan that changes declared rows
	// carries both, and a plan that changes only structure carries neither.
	//
	// They are names and a digest: a table, its key columns and the columns the
	// declaration owns, plus one fingerprint over the rows as they were. No row
	// value appears here, which is what lets the operator read them, publish
	// them and report on them while no declared value reaches status, an Event
	// or an ordinary log.
	ManagedRows     []PlanRowSet    `json:"managed_rows,omitempty"`
	RowsFingerprint string          `json:"rows_fingerprint,omitempty"`
	Destructive     bool            `json:"destructive"`
	Statements      []PlanStatement `json:"statements"`

	// PrivilegeChanges are the kinds of authority the statements change, read
	// by the operator from the SQL: grants, role membership, ownership,
	// row-security policies and definer rights. Ptah's document has no such
	// field and cannot supply one, since a plan is decoded strictly; the list
	// is the operator's own, in the order PrivilegeChangeKinds gives.
	PrivilegeChanges []string `json:"-"`
}

// PlanRowSet is one declared row set a plan read: the table it owns, the
// columns that identify a row in it, and the columns the declaration writes.
type PlanRowSet struct {
	Schema  string   `json:"schema,omitempty"`
	Table   string   `json:"table"`
	Keys    []string `json:"keys"`
	Columns []string `json:"columns"`
}

func DecodeResolve(data []byte) (ResolveReport, error) {
	var report ResolveReport
	if err := decodeJSON(data, &report, false); err != nil {
		return ResolveReport{}, fmt.Errorf("decode resolve report: %w", err)
	}
	if !validDigest(report.Digest) {
		return ResolveReport{}, fmt.Errorf("resolve report does not contain a matching immutable SHA-256 reference")
	}
	if _, err := ocireference.Parse(report.Reference); err != nil {
		return ResolveReport{}, fmt.Errorf("resolve report contains an invalid requested reference: %w", err)
	}
	if err := ocireference.ValidateResolution(report.Reference, report.PinnedReference, report.Digest); err != nil {
		return ResolveReport{}, fmt.Errorf("resolve report does not bind immutable content: %w", err)
	}
	if report.Size < 0 || !mediaTypePattern.MatchString(report.MediaType) {
		return ResolveReport{}, fmt.Errorf("resolve report contains invalid descriptor metadata")
	}
	return report, nil
}

func DecodeVerify(data []byte) (VerifyReport, error) {
	var report VerifyReport
	if err := decodeJSON(data, &report, false); err != nil {
		return VerifyReport{}, fmt.Errorf("decode verification report: %w", err)
	}
	if !validDigest(report.Digest) || ocireference.ValidatePinned(report.Reference, report.Digest) != nil {
		return VerifyReport{}, fmt.Errorf("verification report is missing reference or digest")
	}
	if report.Satisfied == nil || report.Findings == nil {
		return VerifyReport{}, fmt.Errorf("verification report lists must be arrays")
	}
	if len(report.Satisfied) > 64 || len(report.Findings) > 64 {
		return VerifyReport{}, fmt.Errorf("verification report exceeds the supported requirement count")
	}
	seen := make(map[string]struct{}, len(report.Satisfied)+len(report.Findings))
	for _, requirement := range report.Satisfied {
		if !verificationRequirementPattern.MatchString(requirement) {
			return VerifyReport{}, fmt.Errorf("verification report contains an invalid satisfied requirement")
		}
		if _, duplicate := seen[requirement]; duplicate {
			return VerifyReport{}, fmt.Errorf("verification report contains a duplicate requirement")
		}
		seen[requirement] = struct{}{}
	}
	for _, finding := range report.Findings {
		if !verificationRequirementPattern.MatchString(finding.Requirement) ||
			strings.TrimSpace(finding.Detail) == "" || len(finding.Detail) > 4096 {
			return VerifyReport{}, fmt.Errorf("verification report contains an incomplete finding")
		}
		if _, duplicate := seen[finding.Requirement]; duplicate {
			return VerifyReport{}, fmt.Errorf("verification report contains a duplicate requirement")
		}
		seen[finding.Requirement] = struct{}{}
	}
	return report, nil
}

func DecodeInspect(data []byte) (InspectReport, error) {
	var report InspectReport
	// The native inspection report also carries annotations, layers, and
	// subjects. They are intentionally discarded at this boundary; only the
	// immutable descriptor and artifact type are allowed into the runner frame.
	if err := decodeJSON(data, &report, false); err != nil {
		return InspectReport{}, fmt.Errorf("decode inspect report: %w", err)
	}
	if !validDigest(report.Digest) {
		return InspectReport{}, fmt.Errorf("inspect report does not identify immutable content")
	}
	if err := ocireference.ValidateResolution(report.Reference, report.PinnedReference, report.Digest); err != nil {
		return InspectReport{}, fmt.Errorf("inspect report does not bind immutable content: %w", err)
	}
	if strings.TrimSpace(report.ArtifactType) == "" {
		return InspectReport{}, fmt.Errorf("inspect report is missing artifact_type")
	}
	if report.Size < 0 || !mediaTypePattern.MatchString(report.MediaType) {
		return InspectReport{}, fmt.Errorf("inspect report contains invalid descriptor metadata")
	}
	return report, nil
}

// DecodeDrift maps the CLI's configured failure threshold into a domain
// result. Drift may be reported without failing the command.
func DecodeDrift(data []byte, exitCode int) (DriftReport, error) {
	var report DriftReport
	if err := decodeJSON(data, &report, false); err != nil {
		return DriftReport{}, fmt.Errorf("decode drift report: %w", err)
	}
	if report.Error != "" {
		return DriftReport{}, fmt.Errorf("drift observation failed: %s", bounded(report.Error, 1024))
	}
	if report.Failed && !report.Drift {
		return DriftReport{}, fmt.Errorf("drift report says failed=true without drift")
	}
	if report.Failed && exitCode != 1 {
		return DriftReport{}, fmt.Errorf("drift report says failed=true but process exited %d instead of 1", exitCode)
	}
	if !report.Failed && exitCode != 0 {
		return DriftReport{}, fmt.Errorf("drift report says failed=false but process exited %d instead of 0", exitCode)
	}
	if report.Drift && strings.TrimSpace(report.HighestSeverity) == "" {
		return DriftReport{}, fmt.Errorf("drift report is missing highest_severity")
	}
	seenFindings := make(map[string]struct{}, len(report.Findings))
	for _, finding := range report.Findings {
		if finding.Count <= 0 || !IsKnownDriftFindingCategory(finding.Category) || !knownSeverity(finding.Severity) {
			return DriftReport{}, fmt.Errorf("drift report contains an invalid finding")
		}
		if _, duplicate := seenFindings[finding.Category]; duplicate {
			return DriftReport{}, fmt.Errorf("drift report contains a duplicate finding category")
		}
		seenFindings[finding.Category] = struct{}{}
	}
	return report, nil
}

// DriftReportDigest identifies the exact comparison result emitted by one
// drift read. It is deliberately not a database-schema fingerprint: only a
// native plan's from_fingerprint may authorize an Apply. The digest is useful
// for audit and change detection without inventing a second schema identity.
func DriftReportDigest(report DriftReport) (string, error) {
	if len(report.Diff) == 0 || bytes.Equal(bytes.TrimSpace(report.Diff), []byte("null")) {
		return "", fmt.Errorf("drift report is missing its comparison diff")
	}
	var diff any
	if err := decodeJSON(report.Diff, &diff, false); err != nil {
		return "", fmt.Errorf("decode drift comparison diff: %w", err)
	}
	if _, ok := diff.(map[string]any); !ok {
		return "", fmt.Errorf("drift comparison diff must be a JSON object")
	}
	canonical, err := json.Marshal(struct {
		Dialect       string   `json:"dialect"`
		IgnoredTables []string `json:"ignored_tables"`
		Diff          any      `json:"diff"`
	}{
		Dialect:       strings.ToLower(strings.TrimSpace(report.Dialect)),
		IgnoredTables: append([]string(nil), report.IgnoredTables...),
		Diff:          diff,
	})
	if err != nil {
		return "", fmt.Errorf("encode drift report identity: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// DialectMatches reports whether a Ptah report dialect belongs to the
// explicitly configured database engine.
func DialectMatches(engine, dialect string) bool { return dialectMatches(engine, dialect) }

// StatementsSQL renders a plan's statements as they were stored.
//
// One statement per entry, in the order the plan holds them, each terminated
// once. Nothing is re-split on a semicolon: a statement may carry one inside a
// string, a comment or a function body, and splitting there would hand a reader
// SQL the plan never held.
func StatementsSQL(plan PlanFile) string {
	var sql strings.Builder
	for _, statement := range plan.Statements {
		text := strings.TrimSpace(statement.SQL)
		if text == "" {
			continue
		}
		sql.WriteString(strings.TrimSuffix(text, ";"))
		sql.WriteString(";\n")
	}
	return sql.String()
}

func DecodePlan(data []byte, expectedDialect string) (PlanFile, error) {
	var plan PlanFile
	if err := decodeJSON(data, &plan, true); err != nil {
		return PlanFile{}, fmt.Errorf("decode plan document: %w", err)
	}
	if plan.FormatVersion != PlanFormatVersion {
		return PlanFile{}, fmt.Errorf("unsupported plan format_version %d", plan.FormatVersion)
	}
	if strings.TrimSpace(plan.Name) == "" || strings.TrimSpace(plan.FromFingerprint) == "" ||
		strings.TrimSpace(plan.ToFingerprint) == "" {
		return PlanFile{}, fmt.Errorf("plan is missing its name or state fingerprints")
	}
	if !dialectMatches(expectedDialect, plan.Dialect) {
		return PlanFile{}, fmt.Errorf("plan dialect %q does not match target engine %q", plan.Dialect, expectedDialect)
	}
	if len(plan.Statements) == 0 {
		return PlanFile{}, fmt.Errorf("plan contains no statements")
	}
	if err := validateManagedRows(plan); err != nil {
		return PlanFile{}, err
	}
	hasDestructive := false
	privileges := make(map[string]bool)
	for i, statement := range plan.Statements {
		if strings.TrimSpace(statement.SQL) == "" || !knownSeverity(statement.Severity) {
			return PlanFile{}, fmt.Errorf("plan statement %d is empty or has unknown severity %q", i, statement.Severity)
		}
		if credentialBearingPrincipalDDL(statement.SQL, plan.Dialect) {
			return PlanFile{}, fmt.Errorf("plan statement %d contains credential-bearing principal DDL", i)
		}
		if conservativelyDestructiveDDL(statement.SQL, plan.Dialect) {
			plan.Statements[i].Severity = "destructive"
		}
		hasDestructive = hasDestructive || strings.EqualFold(plan.Statements[i].Severity, "destructive")
		for _, kind := range privilegeChanges(statement.SQL, plan.Dialect) {
			privileges[kind] = true
		}
	}
	if hasDestructive {
		// Safety metadata emitted by an executor may under-classify rendered SQL.
		// The operator may only raise the classification; it never lowers an
		// executor's destructive marker.
		plan.Destructive = true
	}
	// "Destructive" means data can be lost, and a plan that grants, delegates
	// or rebinds authority loses nothing. The privilege class is read
	// separately and is independent of the severity Ptah gave each statement,
	// so a statement Ptah called safe is still raised, and nothing lowers it.
	plan.PrivilegeChanges = nil
	for _, kind := range privilegeChangeKinds {
		if privileges[kind] {
			plan.PrivilegeChanges = append(plan.PrivilegeChanges, kind)
		}
	}
	return plan, nil
}

// validateManagedRows refuses a declared-row description the operator cannot
// carry: a fingerprint that is not one, a row set that names no table, and a
// row set that names no key or no column, which could not describe a row.
//
// The two fields arrive together or not at all. A plan that read declared rows
// records what it read them as, and a fingerprint without the row sets it was
// computed over is a staleness check nothing can reproduce.
func validateManagedRows(plan PlanFile) error {
	if plan.RowsFingerprint != "" && !validDigest(plan.RowsFingerprint) {
		return fmt.Errorf("plan rows_fingerprint is not an exact SHA-256 digest")
	}
	if len(plan.ManagedRows) == 0 {
		if plan.RowsFingerprint != "" {
			return fmt.Errorf("plan carries a rows fingerprint and no declared row set")
		}
		return nil
	}
	if plan.RowsFingerprint == "" {
		return fmt.Errorf("plan carries declared row sets and no rows fingerprint")
	}
	seen := make(map[string]bool, len(plan.ManagedRows))
	for index, rowSet := range plan.ManagedRows {
		if !planIdentifier(rowSet.Table) {
			return fmt.Errorf("plan declared row set %d names no table", index)
		}
		if rowSet.Schema != "" && !planIdentifier(rowSet.Schema) {
			return fmt.Errorf("plan declared row set %d names an invalid schema", index)
		}
		if len(rowSet.Keys) == 0 || len(rowSet.Columns) == 0 {
			return fmt.Errorf("plan declared row set %d names no key or no column", index)
		}
		for _, name := range append(append([]string(nil), rowSet.Keys...), rowSet.Columns...) {
			if !planIdentifier(name) {
				return fmt.Errorf("plan declared row set %d names an invalid column", index)
			}
		}
		qualified := rowSet.Schema + "." + rowSet.Table
		if seen[qualified] {
			return fmt.Errorf("plan describes the declared rows of %s twice", qualified)
		}
		seen[qualified] = true
	}
	return nil
}

// planIdentifier is the shape a table or column name may take here. It is
// deliberately narrow: these names are read back into messages and metrics, and
// a plan is a document another process wrote.
func planIdentifier(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	return planIdentifierPattern.MatchString(name)
}

var planIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

func conservativelyDestructiveDDL(statement, dialect string) bool {
	tokens := sqlKeywordTokens(statement, dialect)
	for _, token := range tokens {
		if token == "DROP" || token == "TRUNCATE" {
			return true
		}
	}
	return hasSQLTokenSequence(tokens, "DISABLE", "ROW", "LEVEL", "SECURITY") ||
		hasSQLTokenSequence(tokens, "DELETE", "FROM", "PG_ENUM") ||
		hasSQLTokenSequence(tokens, "ALTER", "COLUMN") && hasSQLToken(tokens, "TYPE") ||
		hasSQLTokenSequence(tokens, "MODIFY", "COLUMN") ||
		hasSQLTokenSequence(tokens, "CHANGE", "COLUMN")
}

func hasSQLToken(tokens []string, wanted string) bool {
	for _, token := range tokens {
		if token == wanted {
			return true
		}
	}
	return false
}

func hasSQLTokenSequence(tokens []string, sequence ...string) bool {
	if len(sequence) == 0 || len(tokens) < len(sequence) {
		return false
	}
	for start := 0; start <= len(tokens)-len(sequence); start++ {
		matched := true
		for offset, wanted := range sequence {
			if tokens[start+offset] != wanted {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func credentialBearingPrincipalDDL(statement, dialect string) bool {
	tokens := sqlKeywordTokens(statement, dialect)
	for index, token := range tokens {
		if token == "SET" && index+1 < len(tokens) && tokens[index+1] == "PASSWORD" {
			return true
		}
		if token != "GRANT" {
			continue
		}
		for identified := index + 1; identified < len(tokens); identified++ {
			if tokens[identified] == "IDENTIFIED" && identified+1 < len(tokens) &&
				(tokens[identified+1] == "BY" || tokens[identified+1] == "WITH") {
				return true
			}
		}
	}
	principalDDL := false
	for index, token := range tokens {
		if token != "CREATE" && token != "ALTER" {
			continue
		}
		limit := index + 5
		if limit > len(tokens) {
			limit = len(tokens)
		}
		for _, candidate := range tokens[index+1 : limit] {
			if candidate == "ROLE" || candidate == "USER" {
				principalDDL = true
				break
			}
		}
	}
	if !principalDDL {
		return false
	}
	for _, token := range tokens {
		if token == "PASSWORD" || token == "IDENTIFIED" {
			return true
		}
	}
	return false
}

// statementBoundary is the token the lexer emits where one statement ends and
// the next begins, around a dollar-quoted body, and between two readings of the
// same statement. No keyword is spelled that way, so no sequence a classifier
// looks for can match across one.
const statementBoundary = ";"

// sqlKeywordTokens reads the keywords of one plan statement under every
// interpretation the server might apply to it.
//
// Both engines can read the same bytes two ways, depending on a setting the plan
// document does not carry. MySQL and MariaDB honor a backslash escape in a quoted
// string unless SQL_MODE has NO_BACKSLASH_ESCAPES; PostgreSQL honors one in an
// ordinary string only while standard_conforming_strings is off. A safety
// classifier must see the keywords either reading exposes. Otherwise a quote the
// server reads as closed hides the rest of the statement from the operator: a
// quoted principal hides IDENTIFIED, and a quoted default hides DROP or SECURITY
// DEFINER.
func sqlKeywordTokens(statement, dialect string) []string {
	tokens := sqlKeywordTokensWithEscapes(statement, dialect, false)
	tokens = append(tokens, statementBoundary)
	return append(tokens, sqlKeywordTokensWithEscapes(statement, dialect, true)...)
}

func sqlKeywordTokensWithEscapes(statement, dialect string, backslashEscapes bool) []string {
	mysql := mysqlDialect(dialect)
	var tokens []string
	for index := 0; index < len(statement); {
		switch {
		case statement[index] == '\'':
			// PostgreSQL reads backslash escapes in an E'' string whatever
			// standard_conforming_strings says.
			escapes := backslashEscapes || !mysql && escapeStringPrefix(statement, index)
			index = skipSQLQuoted(statement, index, '\'', escapes)
		case statement[index] == '"':
			// A PostgreSQL identifier never takes a backslash escape.
			index = skipSQLQuoted(statement, index, '"', backslashEscapes && mysql)
		case statement[index] == '`' && mysql:
			// A backtick quotes an identifier in MySQL. In PostgreSQL it is an
			// operator character, and reading it as a quote would hide what
			// follows it.
			index = skipSQLQuoted(statement, index, '`', backslashEscapes)
		case statement[index] == ';':
			tokens = append(tokens, statementBoundary)
			index++
		case mysql && statement[index] == '#':
			index = skipSQLLineComment(statement, index+1)
		case startsDashComment(statement, index, dialect):
			index = skipSQLLineComment(statement, index+2)
		case !mysql && dollarQuoteTagEnd(statement, index) > index:
			var body []string
			body, index = dollarQuotedTokens(statement, index, dialect, backslashEscapes)
			tokens = append(tokens, statementBoundary)
			tokens = append(tokens, body...)
			tokens = append(tokens, statementBoundary)
		case !mysql && index+1 < len(statement) && statement[index:index+2] == "/*":
			end, closed := skipNestedBlockComment(statement, index)
			if !closed {
				return tokens
			}
			index = end
		case index+1 < len(statement) && statement[index:index+2] == "/*":
			if end := strings.Index(statement[index+2:], "*/"); end >= 0 {
				comment := statement[index+2 : index+2+end]
				if body, executable := mysqlExecutableComment(comment, dialect); executable {
					tokens = append(tokens, sqlKeywordTokensWithEscapes(body, dialect, backslashEscapes)...)
				}
				index += 2 + end + 2
			} else {
				return tokens
			}
		case asciiKeywordCharacter(statement[index]):
			start := index
			// A word continues through the digits, dollar signs and non-ASCII
			// bytes both engines accept inside an unquoted identifier, so
			// grant2 or owner$id is read as the name it is rather than as the
			// keyword it starts with.
			for index < len(statement) && identifierCharacter(statement[index]) {
				index++
			}
			tokens = append(tokens, strings.ToUpper(statement[start:index]))
		default:
			index++
		}
	}
	return tokens
}

// escapeStringPrefix reports whether the quote at index opens a PostgreSQL
// escape string: E'...' or e'...', where the E is a word of its own.
func escapeStringPrefix(statement string, index int) bool {
	if index < 1 || statement[index-1] != 'E' && statement[index-1] != 'e' {
		return false
	}
	return index < 2 || !identifierCharacter(statement[index-2])
}

// dollarQuoteTagEnd returns the offset just past the PostgreSQL dollar-quote
// delimiter that starts at index -- $$ or $tag$ -- and index itself where none
// does. A dollar sign inside a word belongs to the word, and $1 is a parameter.
func dollarQuoteTagEnd(statement string, index int) int {
	if statement[index] != '$' || index > 0 && identifierCharacter(statement[index-1]) {
		return index
	}
	for end := index + 1; end < len(statement); end++ {
		switch character := statement[end]; {
		case character == '$':
			return end + 1
		case asciiKeywordCharacter(character) || character >= 0x80:
		case character >= '0' && character <= '9' && end > index+1:
		default:
			return index
		}
	}
	return index
}

// dollarQuotedTokens reads a dollar-quoted body as SQL of its own.
//
// A body is a function or a DO block, and a DO block runs as soon as it is
// applied, so its keywords stay visible. Reading it separately is what keeps a
// quote or a comment inside it from swallowing the clauses after it: in
//
//	AS $$ SELECT 1 -- ' $$ SECURITY DEFINER
//
// the server reads the body as one string, and a lexer that did not would lose
// SECURITY DEFINER inside a comment that ends with the line.
func dollarQuotedTokens(statement string, index int, dialect string, backslashEscapes bool) ([]string, int) {
	bodyStart := dollarQuoteTagEnd(statement, index)
	tag := statement[index:bodyStart]
	length := strings.Index(statement[bodyStart:], tag)
	if length < 0 {
		// Unterminated, the body runs to the end of the statement, and its
		// words are read rather than dropped.
		return sqlKeywordTokensWithEscapes(statement[bodyStart:], dialect, backslashEscapes), len(statement)
	}
	body := statement[bodyStart : bodyStart+length]
	return sqlKeywordTokensWithEscapes(body, dialect, backslashEscapes), bodyStart + length + len(tag)
}

// skipNestedBlockComment skips a PostgreSQL block comment, which nests. Ending
// it at the first */ would read the rest of an outer comment as SQL, where a
// quote in it could hide the statement's real clauses.
func skipNestedBlockComment(statement string, start int) (int, bool) {
	depth := 0
	for index := start; index+1 < len(statement); {
		switch statement[index : index+2] {
		case "/*":
			depth++
			index += 2
		case "*/":
			depth--
			index += 2
			if depth == 0 {
				return index, true
			}
		default:
			index++
		}
	}
	return len(statement), false
}

func mysqlExecutableComment(comment, dialect string) (string, bool) {
	if !mysqlDialect(dialect) {
		return "", false
	}
	body := ""
	switch {
	case strings.HasPrefix(comment, "!"):
		body = comment[1:]
	case len(comment) >= 2 && (comment[0] == 'M' || comment[0] == 'm') && comment[1] == '!':
		body = comment[2:]
	default:
		return "", false
	}
	body = strings.TrimLeft(body, " \t\r\n")
	for len(body) > 0 && body[0] >= '0' && body[0] <= '9' {
		body = body[1:]
	}
	return strings.TrimLeft(body, " \t\r\n"), true
}

func skipSQLQuoted(statement string, start int, quote byte, backslashEscapes bool) int {
	for index := start + 1; index < len(statement); index++ {
		if backslashEscapes && statement[index] == '\\' && index+1 < len(statement) {
			index++
			continue
		}
		if statement[index] != quote {
			continue
		}
		if index+1 < len(statement) && statement[index+1] == quote {
			index++
			continue
		}
		return index + 1
	}
	return len(statement)
}

func startsDashComment(statement string, index int, dialect string) bool {
	if index+1 >= len(statement) || statement[index:index+2] != "--" {
		return false
	}
	if !mysqlDialect(dialect) {
		return true
	}
	// MySQL and MariaDB require a whitespace or control byte after the
	// second dash. An absent follower is not a comment introducer.
	if index+2 >= len(statement) {
		return false
	}
	next := statement[index+2]
	return next <= ' ' || next == 0x7f
}

// skipSQLLineComment returns the offset after the line ending that closes a
// comment whose body starts at from.
func skipSQLLineComment(statement string, from int) int {
	for index := from; index < len(statement); index++ {
		if statement[index] != '\r' && statement[index] != '\n' {
			continue
		}
		if statement[index] == '\r' && index+1 < len(statement) && statement[index+1] == '\n' {
			return index + 2
		}
		return index + 1
	}
	return len(statement)
}

func mysqlDialect(dialect string) bool {
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	return dialect == "mysql" || dialect == "mariadb"
}

func asciiKeywordCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character == '_'
}

func identifierCharacter(character byte) bool {
	return asciiKeywordCharacter(character) || character >= '0' && character <= '9' || character == '$' ||
		character >= 0x80
}

func decodeJSON(data []byte, target any, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("multiple JSON values")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("multiple JSON values")
	} else if err != io.EOF {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func knownSeverity(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "safe", "info", "warning", "error", "destructive":
		return true
	default:
		return false
	}
}

func dialectMatches(engine, dialect string) bool {
	engine = strings.ToLower(strings.TrimSpace(engine))
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	switch engine {
	case "postgresql", "postgres":
		return dialect == "postgres" || dialect == "postgresql"
	case "mysql":
		return dialect == "mysql" || dialect == "mariadb"
	default:
		return false
	}
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
