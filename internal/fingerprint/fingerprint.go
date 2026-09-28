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
)

const (
	prefix                      = "sha256:"
	coordinationContractVersion = 1

	// CurrentPlanContractVersion is the only plan fingerprint format. It binds
	// what decides the plan's meaning when it runs: the durable execution
	// epoch, the controller-state version, the Ptah version, the executor
	// image and the runner protocol. It also binds what the manager reads out
	// of the plan bytes -- whether they are destructive, which privileges they
	// change, how many statements they hold -- because another build of the
	// manager may read the same bytes differently, and the approval and the
	// apply policy were decided on this reading. The manager's image and
	// revision and the runner image are recorded on the plan but left out of
	// the fingerprint, so a manager release that changes only them, and reads
	// the plan the same way, keeps every plan and approval.
	CurrentPlanContractVersion int32 = 1
)

var (
	coordinationKeyPattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._:/-]{0,251}[a-z0-9])?$`)
	namespacePattern          = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	realmNamePattern          = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	executionBindingIDPattern = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
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
// serialize mutations for one physical database realm that a coordination key
// names inside one namespace. The key is deliberately absent from the returned
// value and must never be copied to status. It is a stable non-secret
// identifier, not an authentication secret.
//
// The namespace is part of the identity. A key is a string anybody who can
// create a resource may write, so a realm it names reaches no further than the
// namespace that wrote it: the same key elsewhere is a different census and a
// different Lease, and cannot refuse or delay what runs here. A database
// managed from several namespaces is named by a PtahRealm instead, through
// DatabaseRealmDigest.
func DatabaseCoordinationDigest(engine, namespace, coordinationKey string) (string, error) {
	canonicalEngine, err := canonicalDatabaseEngine(engine)
	if err != nil {
		return "", err
	}
	if !namespacePattern.MatchString(namespace) {
		return "", fmt.Errorf("coordination namespace must be a DNS-1123 label of at most 63 characters")
	}
	if !coordinationKeyPattern.MatchString(coordinationKey) {
		return "", fmt.Errorf("coordination key must be 1-253 lowercase ASCII characters using letters, digits, '.', '_', ':', '/', or '-'")
	}

	return DigestCanonicalJSON(struct {
		ContractVersion int    `json:"contract_version"`
		Engine          string `json:"engine"`
		Namespace       string `json:"namespace"`
		CoordinationKey string `json:"coordination_key"`
	}{
		ContractVersion: coordinationContractVersion,
		Engine:          canonicalEngine,
		Namespace:       namespace,
		CoordinationKey: coordinationKey,
	})
}

// DatabaseRealmDigest returns the identity of the realm a cluster-scoped
// PtahRealm names. Every resource an administrator admits to that realm, in
// whichever namespace, derives the same value, so they share one census and
// one Lease.
//
// The document has a different set of fields from a namespace key's, so no
// realm name and no key, in any namespace, can derive the other's digest.
func DatabaseRealmDigest(engine, realm string) (string, error) {
	canonicalEngine, err := canonicalDatabaseEngine(engine)
	if err != nil {
		return "", err
	}
	if len(realm) > 253 || !realmNamePattern.MatchString(realm) {
		return "", fmt.Errorf("realm name must be a DNS-1123 subdomain of at most 253 characters")
	}

	return DigestCanonicalJSON(struct {
		ContractVersion int    `json:"contract_version"`
		Engine          string `json:"engine"`
		Realm           string `json:"realm"`
	}{
		ContractVersion: coordinationContractVersion,
		Engine:          canonicalEngine,
		Realm:           realm,
	})
}

// CanonicalDatabaseEngine returns the engine family both digests use, so two
// spellings the API accepts compare as one engine.
func CanonicalDatabaseEngine(engine string) (string, error) {
	return canonicalDatabaseEngine(engine)
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

// PlanBinding is the complete approval identity of an immutable plan. It holds
// nothing that names the manager that published the plan: a manager that
// changes only its own build must be able to apply what the previous one
// planned and a person approved.
//
// Destructive, PrivilegeChanges and StatementCount are what the manager read
// out of the plan bytes. The bytes alone do not fix them: a build whose
// classifier reads a statement differently derives different values from the
// same content, and must derive a different plan rather than inherit one
// whose approval and apply policy were decided on the older reading.
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
	ControllerStateVersion   int32  `json:"controller_state_version"`
	PtahVersion              string `json:"ptah_version"`
	ExecutorImage            string `json:"executor_image"`
	RunnerProtocolVersion    int32  `json:"runner_protocol_version"`

	Destructive      bool     `json:"destructive"`
	PrivilegeChanges []string `json:"privilege_changes"`
	StatementCount   int32    `json:"statement_count"`
}

// Fingerprint validates and hashes the complete plan binding. The privilege
// kinds are a set, so their order and repetition do not change the result.
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
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
	}
	if !executionBindingIDPattern.MatchString(b.ExecutionBindingID) {
		return "", fmt.Errorf("a valid execution binding ID is required")
	}
	if b.ControllerStateVersion < 1 {
		return "", fmt.Errorf("controller state version must be positive")
	}
	if b.RunnerProtocolVersion < 1 {
		return "", fmt.Errorf("runner protocol version must be positive")
	}
	if b.StatementCount < 1 {
		return "", fmt.Errorf("statement count must be positive")
	}
	b.PrivilegeChanges = NormalizeSet(b.PrivilegeChanges)
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
