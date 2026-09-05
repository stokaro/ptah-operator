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

// DriftFindingCategories returns a copy of the closed v1 machine vocabulary.
// Extending this list requires a runner protocol bump and synchronized CRD
// schema changes.
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
	FormatVersion   int             `json:"format_version"`
	Name            string          `json:"name"`
	Dialect         string          `json:"dialect"`
	FromFingerprint string          `json:"from_fingerprint"`
	ToFingerprint   string          `json:"to_fingerprint"`
	Exclude         []string        `json:"exclude,omitempty"`
	Destructive     bool            `json:"destructive"`
	Statements      []PlanStatement `json:"statements"`
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
	hasDestructive := false
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
	}
	if hasDestructive {
		// Safety metadata emitted by an executor may under-classify rendered SQL.
		// The operator may only raise the classification; it never lowers an
		// executor's destructive marker.
		plan.Destructive = true
	}
	return plan, nil
}

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

func sqlKeywordTokens(statement, dialect string) []string {
	if mysqlDialect(dialect) {
		// MySQL and MariaDB can parse the same statement under either
		// backslash-escape interpretation, depending on SQL_MODE. The database's
		// effective mode is not part of the plan document, so a safety classifier
		// must see keywords exposed by both interpretations. Otherwise a quoted
		// principal can hide IDENTIFIED (or a destructive keyword) from the
		// operator while NO_BACKSLASH_ESCAPES makes it executable on the server.
		tokens := sqlKeywordTokensWithEscapes(statement, dialect, false)
		return append(tokens, sqlKeywordTokensWithEscapes(statement, dialect, true)...)
	}
	return sqlKeywordTokensWithEscapes(statement, dialect, false)
}

func sqlKeywordTokensWithEscapes(statement, dialect string, backslashEscapes bool) []string {
	var tokens []string
	for index := 0; index < len(statement); {
		switch {
		case statement[index] == '\'':
			index = skipSQLQuoted(statement, index, '\'', backslashEscapes)
		case statement[index] == '"':
			index = skipSQLQuoted(statement, index, '"', backslashEscapes)
		case statement[index] == '`':
			index = skipSQLQuoted(statement, index, '`', backslashEscapes)
		case startsDashComment(statement, index, dialect):
			index = skipSQLLineComment(statement, index)
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
			for index < len(statement) && asciiKeywordCharacter(statement[index]) {
				index++
			}
			tokens = append(tokens, strings.ToUpper(statement[start:index]))
		default:
			index++
		}
	}
	return tokens
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

func skipSQLLineComment(statement string, start int) int {
	for index := start + 2; index < len(statement); index++ {
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
