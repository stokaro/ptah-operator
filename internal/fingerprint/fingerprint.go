// Package fingerprint creates canonical content identities for reconciliation
// inputs. Every value is credential-free before it reaches this package.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const prefix = "sha256:"

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
	TargetIdentityDigest     string `json:"target_identity_digest"`
	ActualStateFingerprint   string `json:"actual_state_fingerprint"`
	DesiredStateFingerprint  string `json:"desired_state_fingerprint"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	VerificationPolicyDigest string `json:"verification_policy_digest"`
	PtahVersion              string `json:"ptah_version"`
	ExecutorImage            string `json:"executor_image"`
	RunnerImage              string `json:"runner_image"`
	RunnerProtocolVersion    int32  `json:"runner_protocol_version"`
}

// Fingerprint validates and hashes the complete plan binding.
func (b PlanBinding) Fingerprint() (string, error) {
	if b.ContractVersion < 1 {
		return "", fmt.Errorf("plan binding contract version must be positive")
	}
	required := map[string]string{
		"schema UID":                 b.SchemaUID,
		"plan content digest":        b.PlanContentDigest,
		"artifact digest":            b.ArtifactDigest,
		"target identity digest":     b.TargetIdentityDigest,
		"actual state fingerprint":   b.ActualStateFingerprint,
		"desired state fingerprint":  b.DesiredStateFingerprint,
		"policy fingerprint":         b.PolicyFingerprint,
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
	if b.RunnerProtocolVersion < 1 {
		return "", fmt.Errorf("runner protocol version must be positive")
	}
	return DigestCanonicalJSON(b)
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
