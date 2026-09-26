// Package fingerprint creates canonical content identities for reconciliation
// inputs. Every value is credential-free before it reaches this package.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

const (
	prefix                      = "sha256:"
	coordinationContractVersion = 1

	// CurrentPlanContractVersion is the only plan fingerprint format. It binds
	// the durable execution epoch, the digest-pinned manager image, the manager
	// revision, and the controller-state semantics into every plan.
	CurrentPlanContractVersion int32 = 3
)

var (
	coordinationKeyPattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._:/-]{0,251}[a-z0-9])?$`)
	executionBindingIDPattern = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
	imageDigestPattern        = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
)

// DigestBytes returns an OCI-style SHA-256 digest for exact bytes.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + hex.EncodeToString(sum[:])
}

// DigestCanonicalJSON returns a deterministic digest of a JSON-compatible
// value. encoding/json sorts map keys; callers must normalize unordered slices.
func DigestCanonicalJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal fingerprint input: %w", err)
	}
	return DigestBytes(data), nil
}

// DatabaseCoordinationDigest returns the credential-free identity used to
// serialize mutations for one user-declared physical database realm. The key
// is deliberately absent from the returned value and must never be copied to
// status. It is a stable non-secret identifier, not an authentication secret.
func DatabaseCoordinationDigest(engine, coordinationKey string) (string, error) {
	canonicalEngine, err := canonicalDatabaseEngine(engine)
	if err != nil {
		return "", err
	}
	if !coordinationKeyPattern.MatchString(coordinationKey) {
		return "", fmt.Errorf("coordination key must be 1-253 lowercase ASCII characters using letters, digits, '.', '_', ':', '/', or '-'")
	}

	return DigestCanonicalJSON(struct {
		ContractVersion int    `json:"contract_version"`
		Engine          string `json:"engine"`
		CoordinationKey string `json:"coordination_key"`
	}{
		ContractVersion: coordinationContractVersion,
		Engine:          canonicalEngine,
		CoordinationKey: coordinationKey,
	})
}

func canonicalDatabaseEngine(engine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "postgres", "postgresql", "pgx":
		return "postgresql", nil
	case "mariadb", "mysql":
		return "mysql", nil
	default:
		return "", fmt.Errorf("unsupported database engine")
	}
}

// NormalizeSet trims, de-duplicates, and sorts an order-independent string set.
func NormalizeSet(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	slices.Sort(normalized)
	return normalized
}

// PlanBinding is the complete approval identity of an immutable plan.
type PlanBinding struct {
	ContractVersion          int32  `json:"contract_version"`
	SchemaUID                string `json:"schema_uid"`
	PlanContentDigest        string `json:"plan_content_digest"`
	ArtifactDigest           string `json:"artifact_digest"`
	CoordinationDigest       string `json:"coordination_digest"`
	TargetIdentityDigest     string `json:"target_identity_digest"`
	ActualStateFingerprint   string `json:"actual_state_fingerprint"`
	DesiredStateFingerprint  string `json:"desired_state_fingerprint"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	VerificationPolicyUID    string `json:"verification_policy_uid"`
	VerificationPolicyDigest string `json:"verification_policy_digest"`
	ExecutionBindingID       string `json:"execution_binding_id"`
	ControllerImage          string `json:"controller_image"`
	ControllerRevision       string `json:"controller_revision"`
	ControllerStateVersion   int32  `json:"controller_state_version"`
	PtahVersion              string `json:"ptah_version"`
	ExecutorImage            string `json:"executor_image"`
	RunnerImage              string `json:"runner_image"`
	RunnerProtocolVersion    int32  `json:"runner_protocol_version"`
}

// Fingerprint validates and hashes the complete plan binding.
func (b PlanBinding) Fingerprint() (string, error) {
	if err := ValidatePlanContractVersion(b.ContractVersion); err != nil {
		return "", err
	}
	required := map[string]string{
		"schema UID":                 b.SchemaUID,
		"plan content digest":        b.PlanContentDigest,
		"artifact digest":            b.ArtifactDigest,
		"coordination digest":        b.CoordinationDigest,
		"target identity digest":     b.TargetIdentityDigest,
		"actual state fingerprint":   b.ActualStateFingerprint,
		"desired state fingerprint":  b.DesiredStateFingerprint,
		"policy fingerprint":         b.PolicyFingerprint,
		"verification policy UID":    b.VerificationPolicyUID,
		"verification policy digest": b.VerificationPolicyDigest,
		"Ptah version":               b.PtahVersion,
		"executor image":             b.ExecutorImage,
		"runner image":               b.RunnerImage,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
	}
	if !executionBindingIDPattern.MatchString(b.ExecutionBindingID) {
		return "", fmt.Errorf("a valid execution binding ID is required")
	}
	if !imageDigestPattern.MatchString(b.ControllerImage) {
		return "", fmt.Errorf("controller image must be pinned by a lowercase SHA-256 digest")
	}
	if err := controllerstate.ValidateRevision(b.ControllerRevision); err != nil {
		return "", fmt.Errorf("invalid controller revision: %w", err)
	}
	if b.ControllerStateVersion < 1 {
		return "", fmt.Errorf("controller state version must be positive")
	}
	if b.RunnerProtocolVersion < 1 {
		return "", fmt.Errorf("runner protocol version must be positive")
	}
	return DigestCanonicalJSON(b)
}

// ValidatePlanContractVersion accepts only the current plan contract. A plan
// under any other version must not be interpreted using today's approval or
// Apply semantics.
func ValidatePlanContractVersion(version int32) error {
	if version != CurrentPlanContractVersion {
		return fmt.Errorf(
			"unsupported plan contract version %d; the only supported version is %d",
			version,
			CurrentPlanContractVersion,
		)
	}
	return nil
}

// OperationInput identifies one deterministic, crash-recoverable execution.
type OperationInput struct {
	ContractVersion int32          `json:"contract_version"`
	SchemaUID       string         `json:"schema_uid"`
	Operation       string         `json:"operation"`
	Inputs          map[string]any `json:"inputs"`
}

// ID returns the digest used as the operation claim and deterministic Job key.
func (i OperationInput) ID() (string, error) {
	if i.ContractVersion < 1 {
		return "", fmt.Errorf("operation contract version must be positive")
	}
	if strings.TrimSpace(i.SchemaUID) == "" {
		return "", fmt.Errorf("schema UID is required")
	}
	if strings.TrimSpace(i.Operation) == "" {
		return "", fmt.Errorf("operation is required")
	}
	if i.Inputs == nil {
		i.Inputs = map[string]any{}
	}
	return DigestCanonicalJSON(i)
}

// SequenceEntry is one migration of an approved sequence, carrying exactly
// what MigrationSequenceDigest binds. The runner receives the sequence in this
// shape and digests it again, so the list it hands Ptah is the one the plan's
// digest names rather than one that merely travelled beside it.
type SequenceEntry struct {
	Version         int64  `json:"version"`
	VersionKey      string `json:"version_key"`
	Checksum        string `json:"checksum"`
	Checkpoint      bool   `json:"checkpoint"`
	TransactionMode string `json:"transaction_mode"`
}

// MigrationSequenceDigest binds a plan to the exact sequence it carries, in the
// order it carries it: a plan that applies the same migrations in another order
// is a different plan.
func MigrationSequenceDigest(entries []SequenceEntry) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("a plan carries at least one migration")
	}
	document := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		document = append(document, map[string]any{
			"version":          entry.Version,
			"version_key":      entry.VersionKey,
			"checksum":         entry.Checksum,
			"checkpoint":       entry.Checkpoint,
			"transaction_mode": entry.TransactionMode,
		})
	}
	return DigestCanonicalJSON(document)
}
