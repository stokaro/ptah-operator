// Package dataplane defines the narrow machine-readable contracts consumed by
// the controller. It deliberately does not import the full Ptah module.
package dataplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	PlanFormatVersion     = 1
	SchemaArtifactType    = "application/vnd.stokaro.ptah.schema.v1"
	MigrationArtifactType = "application/vnd.stokaro.ptah.migrations.v1"
)

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
	if !validDigest(report.Digest) || !strings.Contains(report.PinnedReference, "@"+report.Digest) {
		return ResolveReport{}, fmt.Errorf("resolve report does not contain a matching immutable SHA-256 reference")
	}
	if report.Size < 0 || strings.TrimSpace(report.MediaType) == "" {
		return ResolveReport{}, fmt.Errorf("resolve report contains invalid descriptor metadata")
	}
	return report, nil
}

func DecodeVerify(data []byte) (VerifyReport, error) {
	var report VerifyReport
	if err := decodeJSON(data, &report, false); err != nil {
		return VerifyReport{}, fmt.Errorf("decode verification report: %w", err)
	}
	if !validDigest(report.Digest) || strings.TrimSpace(report.Reference) == "" {
		return VerifyReport{}, fmt.Errorf("verification report is missing reference or digest")
	}
	if report.Satisfied == nil || report.Findings == nil {
		return VerifyReport{}, fmt.Errorf("verification report lists must be arrays")
	}
	for _, finding := range report.Findings {
		if strings.TrimSpace(finding.Requirement) == "" || strings.TrimSpace(finding.Detail) == "" {
			return VerifyReport{}, fmt.Errorf("verification report contains an incomplete finding")
		}
	}
	return report, nil
}

func DecodeInspect(data []byte) (InspectReport, error) {
	var report InspectReport
	if err := decodeJSON(data, &report, false); err != nil {
		return InspectReport{}, fmt.Errorf("decode inspect report: %w", err)
	}
	if !validDigest(report.Digest) || !strings.Contains(report.PinnedReference, "@"+report.Digest) {
		return InspectReport{}, fmt.Errorf("inspect report does not identify immutable content")
	}
	if strings.TrimSpace(report.ArtifactType) == "" {
		return InspectReport{}, fmt.Errorf("inspect report is missing artifact_type")
	}
	return report, nil
}

// DecodeDrift maps the CLI's expected negative exit into a domain result.
func DecodeDrift(data []byte, exitCode int) (DriftReport, error) {
	var report DriftReport
	if err := decodeJSON(data, &report, false); err != nil {
		return DriftReport{}, fmt.Errorf("decode drift report: %w", err)
	}
	if report.Error != "" {
		return DriftReport{}, fmt.Errorf("drift observation failed: %s", bounded(report.Error, 1024))
	}
	if report.Drift && exitCode != 1 {
		return DriftReport{}, fmt.Errorf("drift report says drift=true but process exited %d instead of 1", exitCode)
	}
	if !report.Drift && exitCode != 0 {
		return DriftReport{}, fmt.Errorf("drift report says drift=false but process exited %d instead of 0", exitCode)
	}
	if report.Drift && strings.TrimSpace(report.HighestSeverity) == "" {
		return DriftReport{}, fmt.Errorf("drift report is missing highest_severity")
	}
	for _, finding := range report.Findings {
		if finding.Count < 0 || strings.TrimSpace(finding.Category) == "" || !knownSeverity(finding.Severity) {
			return DriftReport{}, fmt.Errorf("drift report contains an invalid finding")
		}
	}
	return report, nil
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
	hasDestructive := false
	for i, statement := range plan.Statements {
		if strings.TrimSpace(statement.SQL) == "" || !knownSeverity(statement.Severity) {
			return PlanFile{}, fmt.Errorf("plan statement %d is empty or has unknown severity %q", i, statement.Severity)
		}
		hasDestructive = hasDestructive || strings.EqualFold(statement.Severity, "destructive")
	}
	if hasDestructive && !plan.Destructive {
		return PlanFile{}, fmt.Errorf("plan contains a destructive statement but destructive is false")
	}
	return plan, nil
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
